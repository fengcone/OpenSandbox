// Copyright 2025 Alibaba Group Holding Ltd.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils"
)

const (
	// SandboxSnapshotFinalizer is the finalizer for SandboxSnapshot cleanup
	SandboxSnapshotFinalizer = "sandboxsnapshot.sandbox.opensandbox.io/cleanup"

	// DefaultCommitJobTimeout is the default timeout for commit jobs
	DefaultCommitJobTimeout = 10 * time.Minute

	// CommitJobContainerName is the container name in commit job
	CommitJobContainerName = "commit"

	// ContainerdSocketPath is the default containerd socket path
	ContainerdSocketPath = "/var/run/containerd/containerd.sock"

	// CrictlSocketPath is the CRI socket path for crictl
	CrictlSocketPath = "/run/k8s/containerd/containerd.sock"

	// LabelSandboxSnapshotName is the label key for sandbox snapshot name
	LabelSandboxSnapshotName = "sandbox.opensandbox.io/sandbox-snapshot-name"

	// AnnotationResumedFromSnapshot marks a BatchSandbox as resumed from a snapshot
	AnnotationResumedFromSnapshot = "sandbox.opensandbox.io/resumed-from-snapshot"
)

// SandboxSnapshotReconciler reconciles a SandboxSnapshot object
type SandboxSnapshotReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// CommitExecutorImage is the image for commit-executor (contains ctr/crictl)
	CommitExecutorImage string
}

// +kubebuilder:rbac:groups=sandbox.opensandbox.io,resources=sandboxsnapshots,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=sandbox.opensandbox.io,resources=sandboxsnapshots/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=sandbox.opensandbox.io,resources=sandboxsnapshots/finalizers,verbs=update
// +kubebuilder:rbac:groups=sandbox.opensandbox.io,resources=batchsandboxes,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=sandbox.opensandbox.io,resources=pools,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=events,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop
func (r *SandboxSnapshotReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Fetch the SandboxSnapshot instance
	snapshot := &sandboxv1alpha1.SandboxSnapshot{}
	if err := r.Get(ctx, req.NamespacedName, snapshot); err != nil {
		if errors.IsNotFound(err) {
			log.Info("SandboxSnapshot resource not found")
			return ctrl.Result{}, nil
		}
		log.Error(err, "Failed to get SandboxSnapshot")
		return ctrl.Result{}, err
	}

	// Handle deletion
	if !snapshot.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, snapshot)
	}

	// Add finalizer if not present
	if !controllerutil.ContainsFinalizer(snapshot, SandboxSnapshotFinalizer) {
		if err := utils.UpdateFinalizer(r.Client, snapshot, utils.AddFinalizerOpType, SandboxSnapshotFinalizer); err != nil {
			log.Error(err, "Failed to add finalizer", "finalizer", SandboxSnapshotFinalizer)
			return ctrl.Result{}, err
		}
		log.Info("Added finalizer", "finalizer", SandboxSnapshotFinalizer)
		return ctrl.Result{RequeueAfter: time.Millisecond * 100}, nil
	}

	// Delegate to phase-specific handlers
	phase := snapshot.Status.Phase
	if phase == "" {
		phase = sandboxv1alpha1.SandboxSnapshotPhasePending
	}

	log.Info("Reconciling SandboxSnapshot", "phase", phase, "snapshot", snapshot.Name)

	switch phase {
	case sandboxv1alpha1.SandboxSnapshotPhasePending:
		return r.handlePending(ctx, snapshot)
	case sandboxv1alpha1.SandboxSnapshotPhaseCommitting:
		return r.handleCommitting(ctx, snapshot)
	case sandboxv1alpha1.SandboxSnapshotPhasePushing:
		return r.handlePushing(ctx, snapshot)
	case sandboxv1alpha1.SandboxSnapshotPhaseReady:
		return r.handleReady(ctx, snapshot)
	case sandboxv1alpha1.SandboxSnapshotPhaseFailed:
		return r.handleFailed(ctx, snapshot)
	default:
		log.Info("Unknown phase, treating as Pending", "phase", phase)
		return r.handlePending(ctx, snapshot)
	}
}

// ensureResolved resolves the template and fills spec.ContainerSnapshots with per-container
// image URIs along with pause policy info. It looks up the source BatchSandbox and
// fills in missing spec fields from the BatchSandbox, including pausePolicy, template
// for container snapshots, and ResumeTemplate for resuming after pause.
func (r *SandboxSnapshotReconciler) ensureResolved(ctx context.Context, snapshot *sandboxv1alpha1.SandboxSnapshot) error {
	log := logf.FromContext(ctx)

	// If ContainerSnapshots already have all values populated, skip resolution
	// This handles backward compatibility for pre-existing snapshots with already-filled fields
	if len(snapshot.Spec.ContainerSnapshots) > 0 {
		allResolved := true
		for _, cs := range snapshot.Spec.ContainerSnapshots {
			if cs.ContainerName != "" && cs.ImageURI != "" {
				continue
			}
			allResolved = false
			break
		}

		// Check also if essential pause policy fields are populated
		if allResolved && snapshot.Spec.SnapshotType != "" && snapshot.Spec.SnapshotRegistry != "" {
			log.Info("Snapshot already resolved, skipping resolution")
			return nil
		}
	}

	// Look up the source BatchSandbox
	bs := &sandboxv1alpha1.BatchSandbox{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      snapshot.Spec.SourceBatchSandboxName,
		Namespace: snapshot.Namespace,
	}, bs); err != nil {
		return fmt.Errorf("failed to get source BatchSandbox %s: %w", snapshot.Spec.SourceBatchSandboxName, err)
	}

	// If SourcePodName is empty, find the running pod for this sandbox
	if snapshot.Spec.SourcePodName == "" {
		pod, err := r.findPodForSandbox(ctx, bs, snapshot.Namespace)
		if err != nil {
			return fmt.Errorf("failed to find running pod for sandbox: %w", err)
		}
		snapshot.Spec.SourcePodName = pod.Name
		snapshot.Spec.SourceNodeName = pod.Spec.NodeName
		log.Info("Resolved pod info", "pod", pod.Name, "node", pod.Spec.NodeName)
	}

	// Fill in pause policy fields from BatchSandbox
	if bs.Spec.PausePolicy != nil {
		// Extract pause policy fields
		snapshot.Spec.SnapshotType = bs.Spec.PausePolicy.SnapshotType
		snapshot.Spec.SnapshotRegistry = bs.Spec.PausePolicy.SnapshotRegistry
		snapshot.Spec.SnapshotPushSecretName = bs.Spec.PausePolicy.SnapshotPushSecretName
		snapshot.Spec.ResumeImagePullSecretName = bs.Spec.PausePolicy.ResumeImagePullSecretName
	} else {
		return fmt.Errorf("BatchSandbox %s has no pausePolicy configured", bs.Name)
	}

	// Resolve the template: prefer spec.Template, otherwise look up Pool CR
	var template *corev1.PodTemplateSpec
	if bs.Spec.Template != nil {
		template = bs.Spec.Template
		log.Info("Resolved template directly from BatchSandbox spec")
	} else if bs.Spec.PoolRef != "" {
		// PoolRef mode: look up the Pool CR to get template
		pool := &sandboxv1alpha1.Pool{}
		if err := r.Get(ctx, types.NamespacedName{
			Name:      bs.Spec.PoolRef,
			Namespace: snapshot.Namespace,
		}, pool); err != nil {
			return fmt.Errorf("failed to look up Pool CR %s to get template: %w", bs.Spec.PoolRef, err)
		}
		if pool.Spec.Template == nil {
			return fmt.Errorf("Pool %s has no template defined", bs.Spec.PoolRef)
		}
		template = pool.Spec.Template
		log.Info("Resolved template via Pool CR", "pool", bs.Spec.PoolRef)
	} else {
		return fmt.Errorf("BatchSandbox %s has neither template nor poolRef, cannot resolve", bs.Name)
	}

	// Build ResumeTemplate from the template with resolved fields
	resumeTemplateData := map[string]interface{}{
		"template": convertPodTemplateSpecToMap(template), // Convert the template to map[string]interface{}
	}

	// Add or update BatchSandbox-level fields to ResumeTemplate if they exist
	if bs.Spec.ExpireTime != nil {
		resumeTemplateData["expireTime"] = bs.Spec.ExpireTime // Copy the expireTime
	}
	if bs.Spec.PausePolicy != nil {
		// We add the original pause policy back to the ResumeTemplate
		// So that resumed sandboxes retain the same pause capability
		resumeTemplateData["pausePolicy"] = map[string]interface{}{
			"snapshotType":              bs.Spec.PausePolicy.SnapshotType,
			"snapshotRegistry":          bs.Spec.PausePolicy.SnapshotRegistry,
			"snapshotPushSecretName":    bs.Spec.PausePolicy.SnapshotPushSecretName,
			"resumeImagePullSecretName": bs.Spec.PausePolicy.ResumeImagePullSecretName,
		}
	}

	// Convert the entire resume template to RawExtension
	resumeTemplateRaw, err := convertToRawExtension(resumeTemplateData)
	if err != nil {
		return fmt.Errorf("failed to convert resume template to raw extension: %w", err)
	}
	snapshot.Spec.ResumeTemplate = &resumeTemplateRaw

	// Resolve snapshot registry
	registry := snapshot.Spec.SnapshotRegistry
	if registry == "" {
		return fmt.Errorf("snapshotRegistry not resolved in pausePolicy")
	}

	// Build ContainerSnapshots from the template containers
	containerSnapshots := make([]sandboxv1alpha1.ContainerSnapshot, 0, len(template.Spec.Containers))
	for _, c := range template.Spec.Containers {
		imageURI := fmt.Sprintf("%s/%s-%s:snapshot", registry, snapshot.Spec.SandboxID, c.Name)
		containerSnapshots = append(containerSnapshots, sandboxv1alpha1.ContainerSnapshot{
			ContainerName: c.Name,
			ImageURI:      imageURI,
		})
	}

	if len(containerSnapshots) == 0 {
		return fmt.Errorf("no containers found in template for BatchSandbox %s", bs.Name)
	}

	// Update the snapshot spec with resolved fields
	snapshot.Spec.ContainerSnapshots = containerSnapshots

	if err := r.Update(ctx, snapshot); err != nil {
		return fmt.Errorf("failed to update snapshot with resolved fields: %w", err)
	}

	log.Info("Resolved and updated snapshot fields", "count", len(containerSnapshots), "snapshot", snapshot.Name)
	return nil
}

// findPodForSandbox finds the running pod belonging to a BatchSandbox.
// It first tries to parse the alloc-status annotation, then falls back to label selector.
func (r *SandboxSnapshotReconciler) findPodForSandbox(ctx context.Context, bs *sandboxv1alpha1.BatchSandbox, namespace string) (*corev1.Pod, error) {
	log := logf.FromContext(ctx)

	// Try alloc-status annotation first (pool-based allocation)
	alloc, err := parseSandboxAllocation(bs)
	if err == nil && len(alloc.Pods) > 0 {
		// Get the first allocated pod
		pod := &corev1.Pod{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: alloc.Pods[0]}, pod); err == nil {
			if pod.Status.Phase == corev1.PodRunning {
				return pod, nil
			}
			log.Info("Allocated pod not running, trying others", "pod", pod.Name, "phase", pod.Status.Phase)
		}
		// Try other pods in the allocation
		for _, podName := range alloc.Pods[1:] {
			p := &corev1.Pod{}
			if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: podName}, p); err == nil {
				if p.Status.Phase == corev1.PodRunning {
					return p, nil
				}
			}
		}
	}

	// Fallback: list pods owned by this BatchSandbox
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList,
		client.InNamespace(namespace),
		client.MatchingLabels{LabelBatchSandboxPodIndexKey: "0"},
	); err != nil {
		return nil, fmt.Errorf("failed to list pods: %w", err)
	}

	// Filter pods owned by this BatchSandbox
	for i := range podList.Items {
		pod := &podList.Items[i]
		for _, owner := range pod.OwnerReferences {
			if owner.Kind == "BatchSandbox" && owner.Name == bs.Name && pod.Status.Phase == corev1.PodRunning {
				return pod, nil
			}
		}
	}

	// Last resort: find by naming convention {batchSandboxName}-0
	podName := fmt.Sprintf("%s-0", bs.Name)
	pod := &corev1.Pod{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: podName}, pod); err == nil {
		return pod, nil
	}

	return nil, fmt.Errorf("no running pod found for BatchSandbox %s", bs.Name)
}

// handlePending creates the commit Job after ensuring resolution of container snapshots
func (r *SandboxSnapshotReconciler) handlePending(ctx context.Context, snapshot *sandboxv1alpha1.SandboxSnapshot) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Ensure container snapshots are resolved before creating the commit job
	if err := r.ensureResolved(ctx, snapshot); err != nil {
		log.Error(err, "Failed to resolve container snapshots")
		if updateErr := r.updateSnapshotStatus(ctx, snapshot, sandboxv1alpha1.SandboxSnapshotPhaseFailed, err.Error()); updateErr != nil {
			return ctrl.Result{}, updateErr
		}
		return ctrl.Result{}, nil
	}

	// Build and create the commit Job
	job, err := r.buildCommitJob(snapshot)
	if err != nil {
		log.Error(err, "Failed to build commit job")
		if updateErr := r.updateSnapshotStatus(ctx, snapshot, sandboxv1alpha1.SandboxSnapshotPhaseFailed, err.Error()); updateErr != nil {
			return ctrl.Result{}, updateErr
		}
		return ctrl.Result{}, nil
	}

	// Check if job already exists
	existingJob := &batchv1.Job{}
	err = r.Get(ctx, types.NamespacedName{Namespace: job.Namespace, Name: job.Name}, existingJob)
	if err == nil {
		// Job already exists, update phase to Committing
		log.Info("Commit job already exists", "job", job.Name)
		if updateErr := r.updateSnapshotStatus(ctx, snapshot, sandboxv1alpha1.SandboxSnapshotPhaseCommitting, "Commit job created"); updateErr != nil {
			return ctrl.Result{}, updateErr
		}
		return ctrl.Result{}, nil
	}

	if !errors.IsNotFound(err) {
		log.Error(err, "Failed to check existing job")
		return ctrl.Result{}, err
	}

	// Create the job
	if err := r.Create(ctx, job); err != nil {
		log.Error(err, "Failed to create commit job")
		r.Recorder.Eventf(snapshot, corev1.EventTypeWarning, "FailedCreateJob", "Failed to create commit job: %v", err)
		return ctrl.Result{}, err
	}

	log.Info("Created commit job", "job", job.Name)
	r.Recorder.Eventf(snapshot, corev1.EventTypeNormal, "CreatedJob", "Created commit job: %s", job.Name)

	// Update phase to Committing
	if err := r.updateSnapshotStatus(ctx, snapshot, sandboxv1alpha1.SandboxSnapshotPhaseCommitting, "Commit job created"); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// handleCommitting checks the commit Job status
func (r *SandboxSnapshotReconciler) handleCommitting(ctx context.Context, snapshot *sandboxv1alpha1.SandboxSnapshot) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	jobName := r.getJobName(snapshot)
	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Namespace: snapshot.Namespace, Name: jobName}, job)
	if err != nil {
		if errors.IsNotFound(err) {
			log.Info("Commit job not found, re-creating", "job", jobName)
			return r.handlePending(ctx, snapshot)
		}
		log.Error(err, "Failed to get commit job")
		return ctrl.Result{}, err
	}

	// Check job status
	if job.Status.Succeeded > 0 {
		log.Info("Commit job succeeded", "job", jobName)
		r.Recorder.Eventf(snapshot, corev1.EventTypeNormal, "JobSucceeded", "Commit job succeeded")

		// Update phase to Pushing
		if err := r.updateSnapshotStatus(ctx, snapshot, sandboxv1alpha1.SandboxSnapshotPhasePushing, "Commit completed, pushing image"); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	if job.Status.Failed > 0 {
		log.Info("Commit job failed", "job", jobName)
		r.Recorder.Eventf(snapshot, corev1.EventTypeWarning, "JobFailed", "Commit job failed")

		// Get failure message from job conditions
		message := "Commit job failed"
		for _, condition := range job.Status.Conditions {
			if condition.Type == batchv1.JobFailed {
				message = condition.Message
				break
			}
		}

		if err := r.updateSnapshotStatus(ctx, snapshot, sandboxv1alpha1.SandboxSnapshotPhaseFailed, message); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Job still running, requeue
	log.Info("Commit job still running", "job", jobName)
	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

// handlePushing waits for image push completion
// Note: The commit job handles both commit and push, so we transition to Ready when the job succeeds.
func (r *SandboxSnapshotReconciler) handlePushing(ctx context.Context, snapshot *sandboxv1alpha1.SandboxSnapshot) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Check if the job is still running (the commit job handles both commit and push)
	jobName := r.getJobName(snapshot)
	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Namespace: snapshot.Namespace, Name: jobName}, job)
	if err != nil && !errors.IsNotFound(err) {
		log.Error(err, "Failed to get commit job")
		return ctrl.Result{}, err
	}

	// If job exists and is still running, requeue
	if err == nil && job.Status.Succeeded == 0 && job.Status.Failed == 0 {
		log.Info("Push still in progress", "job", jobName)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// Populate status.ContainerSnapshots from spec.ContainerSnapshots (snapshot images are now pushed)
	statusSnapshots := make([]sandboxv1alpha1.ContainerSnapshot, len(snapshot.Spec.ContainerSnapshots))
	copy(statusSnapshots, snapshot.Spec.ContainerSnapshots)

	// Transition to Ready
	now := metav1.Now()
	if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latestSnapshot := &sandboxv1alpha1.SandboxSnapshot{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: snapshot.Namespace, Name: snapshot.Name}, latestSnapshot); err != nil {
			return err
		}
		latestSnapshot.Status.Phase = sandboxv1alpha1.SandboxSnapshotPhaseReady
		latestSnapshot.Status.Message = "Snapshot is ready"
		latestSnapshot.Status.ReadyAt = &now
		latestSnapshot.Status.ContainerSnapshots = statusSnapshots

		return r.Status().Update(ctx, latestSnapshot)
	}); err != nil {
		log.Error(err, "Failed to update snapshot status to Ready")
		return ctrl.Result{}, err
	}

	log.Info("Snapshot is ready", "snapshot", snapshot.Name)
	r.Recorder.Eventf(snapshot, corev1.EventTypeNormal, "SnapshotReady", "Snapshot %s is ready", snapshot.Name)

	return ctrl.Result{}, nil
}

// handleReady handles a ready snapshot.
// It deletes the original (paused) BatchSandbox after the snapshot is Ready,
// If the BatchSandbox has already been resumed (marked with annotation
// sandbox.opensandbox.io/resumed-from-snapshot), deletion is skipped.
func (r *SandboxSnapshotReconciler) handleReady(ctx context.Context, snapshot *sandboxv1alpha1.SandboxSnapshot) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	bsName := snapshot.Spec.SourceBatchSandboxName
	if bsName == "" {
		log.Info("No source BatchSandbox specified, nothing to clean up")
		return ctrl.Result{}, nil
	}

	// Check if the source BatchSandbox still exists
	bs := &sandboxv1alpha1.BatchSandbox{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      bsName,
		Namespace: snapshot.Namespace,
	}, bs)
	if err != nil {
		if errors.IsNotFound(err) {
			log.Info("Source BatchSandbox already deleted", "batchSandbox", bsName)
			return ctrl.Result{}, nil
		}
		log.Error(err, "Failed to get source BatchSandbox")
		return ctrl.Result{}, err
	}

	// Check if the BatchSandbox was already resumed from snapshot
	if resumed := bs.Annotations[AnnotationResumedFromSnapshot]; resumed == "true" {
		log.Info("BatchSandbox already resumed from snapshot, skipping cleanup",
			"batchSandbox", bsName)
		return ctrl.Result{}, nil
	}

	// Delete the original (paused) BatchSandbox
	if err := r.Delete(ctx, bs, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil {
		if errors.IsNotFound(err) {
			log.Info("BatchSandbox already gone", "batchSandbox", bsName)
			return ctrl.Result{}, nil
		}
		log.Error(err, "Failed to delete source BatchSandbox", "batchSandbox", bsName)
		return ctrl.Result{}, err
	}

	log.Info("Deleted original (paused) BatchSandbox", "batchSandbox", bsName)
	r.Recorder.Eventf(snapshot, corev1.EventTypeNormal, "CleanedUpBatchSandbox",
		"Deleted paused BatchSandbox %s after snapshot Ready", bsName)

	return ctrl.Result{}, nil
}

// handleFailed handles a failed snapshot
func (r *SandboxSnapshotReconciler) handleFailed(ctx context.Context, snapshot *sandboxv1alpha1.SandboxSnapshot) (ctrl.Result, error) {
	// Snapshot failed, nothing to do
	return ctrl.Result{}, nil
}

// handleDeletion handles the deletion of a SandboxSnapshot
func (r *SandboxSnapshotReconciler) handleDeletion(ctx context.Context, snapshot *sandboxv1alpha1.SandboxSnapshot) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Clean up the commit job if it exists
	jobName := r.getJobName(snapshot)
	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Namespace: snapshot.Namespace, Name: jobName}, job)
	if err == nil {
		// Delete the job
		if deleteErr := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); deleteErr != nil && !errors.IsNotFound(deleteErr) {
			log.Error(deleteErr, "Failed to delete commit job")
			return ctrl.Result{}, deleteErr
		}
		log.Info("Deleted commit job", "job", jobName)
	}

	// Remove finalizer
	if controllerutil.ContainsFinalizer(snapshot, SandboxSnapshotFinalizer) {
		if err := utils.UpdateFinalizer(r.Client, snapshot, utils.RemoveFinalizerOpType, SandboxSnapshotFinalizer); err != nil {
			log.Error(err, "Failed to remove finalizer")
			return ctrl.Result{}, err
		}
		log.Info("Removed finalizer", "finalizer", SandboxSnapshotFinalizer)
	}

	return ctrl.Result{}, nil
}

// buildCommitJob builds a Job for committing container snapshots.
// It supports multi-container sandboxes by creating init containers for each
// container snapshot that needs to be committed, followed by a main verification container.
func (r *SandboxSnapshotReconciler) buildCommitJob(snapshot *sandboxv1alpha1.SandboxSnapshot) (*batchv1.Job, error) {
	jobName := r.getJobName(snapshot)

	// Use commit-executor image (contains ctr and crictl tools)
	commitExecutorImage := r.CommitExecutorImage
	if commitExecutorImage == "" {
		commitExecutorImage = "commit-executor:dev" // Default fallback
	}

	// Build volume mounts for containerd and CRI sockets
	volumeMounts := []corev1.VolumeMount{
		{
			Name:      "containerd-sock",
			MountPath: ContainerdSocketPath,
		},
		{
			Name:      "cri-sock",
			MountPath: CrictlSocketPath,
		},
	}

	// Build volumes for host paths
	volumes := []corev1.Volume{
		{
			Name: "containerd-sock",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: ContainerdSocketPath,
				},
			},
		},
		{
			Name: "cri-sock",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: CrictlSocketPath,
				},
			},
		},
	}

	// Add registry credentials from secret if specified
	if snapshot.Spec.SnapshotPushSecretName != "" {
		volumes = append(volumes, corev1.Volume{
			Name: "registry-creds",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: snapshot.Spec.SnapshotPushSecretName,
					Items: []corev1.KeyToPath{
						{
							Key:  ".dockerconfigjson",
							Path: "config.json",
						},
					},
				},
			},
		})
		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name:      "registry-creds",
			MountPath: "/var/run/opensandbox/registry",
			ReadOnly:  true,
		})
	}

	// Build commit command using new multi-container format:
	// commit-snapshot.sh <pod_name> <namespace> <container1:uri1> [<container2:uri2> ...]
	containerSnapshots := snapshot.Spec.ContainerSnapshots

	if len(containerSnapshots) == 0 {
		return nil, fmt.Errorf("no container snapshots specified in snapshot spec")
	}

	var containerSpecs []string
	for _, cs := range containerSnapshots {
		spec := fmt.Sprintf("%s:%s", cs.ContainerName, cs.ImageURI)
		containerSpecs = append(containerSpecs, spec)
	}
	fullCommand := fmt.Sprintf("/scripts/commit-snapshot.sh %s %s %s",
		snapshot.Spec.SourcePodName,
		snapshot.Namespace,
		strings.Join(containerSpecs, " "),
	)

	// Build the job
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: snapshot.Namespace,
			Labels: map[string]string{
				LabelSandboxSnapshotName: snapshot.Name,
			},
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: func() *int32 { v := int32(300); return &v }(),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:            CommitJobContainerName,
							Image:           commitExecutorImage,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Command:         []string{"/bin/sh", "-c"},
							Args:            []string{fullCommand},
							VolumeMounts:    volumeMounts,
							Env: []corev1.EnvVar{
								{
									Name:  "CONTAINERD_SOCKET",
									Value: ContainerdSocketPath,
								},
								{
									Name:  "CRI_RUNTIME_ENDPOINT",
									Value: CrictlSocketPath,
								},
							},
							SecurityContext: &corev1.SecurityContext{
								RunAsUser: ptrToInt64(0), // Run as root to access containerd
							},
						},
					},
					Volumes:               volumes,
					NodeName:              snapshot.Spec.SourceNodeName,
					ActiveDeadlineSeconds: ptrToInt64(int64(DefaultCommitJobTimeout.Seconds())),
				},
			},
		},
	}

	// Set owner reference
	if err := ctrl.SetControllerReference(snapshot, job, r.Scheme); err != nil {
		return nil, fmt.Errorf("failed to set controller reference: %w", err)
	}

	return job, nil
}

// getJobName returns the job name for a snapshot
func (r *SandboxSnapshotReconciler) getJobName(snapshot *sandboxv1alpha1.SandboxSnapshot) string {
	return fmt.Sprintf("%s-commit", snapshot.Name)
}

// updateSnapshotStatus updates the snapshot status
func (r *SandboxSnapshotReconciler) updateSnapshotStatus(ctx context.Context, snapshot *sandboxv1alpha1.SandboxSnapshot, phase sandboxv1alpha1.SandboxSnapshotPhase, message string) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latestSnapshot := &sandboxv1alpha1.SandboxSnapshot{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: snapshot.Namespace, Name: snapshot.Name}, latestSnapshot); err != nil {
			return err
		}

		latestSnapshot.Status.Phase = phase
		latestSnapshot.Status.Message = message

		return r.Status().Update(ctx, latestSnapshot)
	})
}

// ptrToInt64 returns a pointer to an int64
func ptrToInt64(v int64) *int64 {
	return &v
}

// convertPodTemplateSpecToMap converts a PodTemplateSpec to a map[string]interface{}
func convertPodTemplateSpecToMap(template *corev1.PodTemplateSpec) map[string]interface{} {
	if template == nil {
		return nil
	}

	result := make(map[string]interface{})

	// Convert ObjectMeta
	if !template.ObjectMeta.CreationTimestamp.IsZero() || len(template.ObjectMeta.Labels) > 0 || len(template.ObjectMeta.Annotations) > 0 {
		meta := make(map[string]interface{})
		if len(template.ObjectMeta.Labels) > 0 {
			meta["labels"] = template.ObjectMeta.Labels
		}
		if len(template.ObjectMeta.Annotations) > 0 {
			meta["annotations"] = template.ObjectMeta.Annotations
		}
		result["metadata"] = meta
	}

	// Convert PodSpec
	podSpecBytes, _ := json.Marshal(template.Spec)
	var podSpecMap map[string]interface{}
	_ = json.Unmarshal(podSpecBytes, &podSpecMap)
	if podSpecMap != nil {
		result["spec"] = podSpecMap
	}

	return result
}

// convertToRawExtension converts a struct to RawExtension
func convertToRawExtension(data interface{}) (runtime.RawExtension, error) {
	jsonBytes, err := json.Marshal(data)
	if err != nil {
		return runtime.RawExtension{}, err
	}

	return runtime.RawExtension{
		Raw: jsonBytes,
	}, nil
}

// SetupWithManager sets up the controller with the Manager
func (r *SandboxSnapshotReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&sandboxv1alpha1.SandboxSnapshot{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&batchv1.Job{}).
		Named("sandboxsnapshot").
		Complete(r)
}

// Add the JSON import for marshaling/unmarshaling
