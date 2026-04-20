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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils"
)

const (
	// SandboxSnapshotFinalizer is the finalizer for SandboxSnapshot cleanup
	SandboxSnapshotFinalizer = "sandboxsnapshot.sandbox.opensandbox.io/cleanup"

	// DefaultCommitJobTimeout is the default timeout for commit jobs
	DefaultCommitJobTimeout = 10 * time.Minute

	DefaultTTLSecondsAfterFinished = 300

	// CommitJobContainerName is the container name in commit job
	CommitJobContainerName = "commit"

	// ContainerdSocketPath is the default containerd socket path
	ContainerdSocketPath = "/var/run/containerd/containerd.sock"

	// LabelSandboxSnapshotName is the label key for sandbox snapshot name
	LabelSandboxSnapshotName = "sandbox.opensandbox.io/sandbox-snapshot-name"
)

// SandboxSnapshotReconciler reconciles a SandboxSnapshot object.
// Pure atomic capability: reads BatchSandbox via spec.sandboxName, finds Pod,
// creates commit Job to commit+push container images, reports status.
// No business logic (no scaling, no pool, no resume).
type SandboxSnapshotReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// ImageCommitterImage is the image for image-committer (uses nerdctl to commit/push container images)
	ImageCommitterImage string

	// CommitJobTimeout is the timeout for commit jobs (default: 10 minutes)
	CommitJobTimeout time.Duration

	// SnapshotRegistry is the OCI registry for snapshot images (from Controller Manager startup params)
	SnapshotRegistry string

	// SnapshotPushSecret is the K8s Secret name for pushing to registry (from Controller Manager startup params)
	SnapshotPushSecret string
}

// +kubebuilder:rbac:groups=sandbox.opensandbox.io,resources=sandboxsnapshots,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=sandbox.opensandbox.io,resources=sandboxsnapshots/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=sandbox.opensandbox.io,resources=sandboxsnapshots/finalizers,verbs=update
// +kubebuilder:rbac:groups=sandbox.opensandbox.io,resources=batchsandboxes,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=events,verbs=get;list;watch;create;update;patch;delete

func (r *SandboxSnapshotReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	snapshot := &sandboxv1alpha1.SandboxSnapshot{}
	if err := r.Get(ctx, req.NamespacedName, snapshot); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Handle deletion
	if !snapshot.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, snapshot)
	}

	// Add finalizer if not present
	if !controllerutil.ContainsFinalizer(snapshot, SandboxSnapshotFinalizer) {
		if err := utils.UpdateFinalizer(r.Client, snapshot, utils.AddFinalizerOpType, SandboxSnapshotFinalizer); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Millisecond * 100}, nil
	}

	// ACK generation immediately to prevent re-entry
	generation := snapshot.Generation
	if generation > snapshot.Status.ObservedGeneration {
		if err := r.ackGeneration(ctx, snapshot); err != nil {
			return ctrl.Result{}, err
		}
		// Re-fetch after ACK
		if err := r.Get(ctx, req.NamespacedName, snapshot); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Dispatch by phase
	switch snapshot.Status.Phase {
	case "", sandboxv1alpha1.SandboxSnapshotPhasePending:
		return r.handlePending(ctx, snapshot)
	case sandboxv1alpha1.SandboxSnapshotPhaseCommitting:
		return r.handleCommitting(ctx, snapshot)
	case sandboxv1alpha1.SandboxSnapshotPhaseReady:
		// Ready: nothing more to do, BatchSandbox Controller handles completion
		return ctrl.Result{}, nil
	case sandboxv1alpha1.SandboxSnapshotPhaseFailed:
		// Failed: wait for BatchSandbox Controller to handle recovery
		return ctrl.Result{}, nil
	default:
		log.Info("Unknown phase, treating as Pending", "phase", snapshot.Status.Phase)
		return r.handlePending(ctx, snapshot)
	}
}

// ackGeneration ACKs the current generation.
func (r *SandboxSnapshotReconciler) ackGeneration(ctx context.Context, snapshot *sandboxv1alpha1.SandboxSnapshot) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &sandboxv1alpha1.SandboxSnapshot{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: snapshot.Namespace, Name: snapshot.Name}, latest); err != nil {
			return err
		}
		latest.Status.ObservedGeneration = latest.Generation
		if latest.Status.Phase == "" {
			latest.Status.Phase = sandboxv1alpha1.SandboxSnapshotPhasePending
		}
		return r.Status().Update(ctx, latest)
	})
}

// handlePending resolves the source Pod and creates the commit Job.
func (r *SandboxSnapshotReconciler) handlePending(ctx context.Context, snapshot *sandboxv1alpha1.SandboxSnapshot) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Validate registry config
	if r.SnapshotRegistry == "" {
		msg := "snapshot-registry not configured in controller manager"
		log.Error(nil, msg)
		_ = r.updateSnapshotStatus(ctx, snapshot, sandboxv1alpha1.SandboxSnapshotPhaseFailed, msg)
		return ctrl.Result{}, nil
	}

	// Read BatchSandbox to find the Pod
	bs := &sandboxv1alpha1.BatchSandbox{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      snapshot.Spec.SandboxName,
		Namespace: snapshot.Namespace,
	}, bs); err != nil {
		msg := fmt.Sprintf("failed to get BatchSandbox %s: %v", snapshot.Spec.SandboxName, err)
		_ = r.updateSnapshotStatus(ctx, snapshot, sandboxv1alpha1.SandboxSnapshotPhaseFailed, msg)
		return ctrl.Result{}, nil
	}

	// Find running pod for this BatchSandbox
	pod, err := r.findPodForSandbox(ctx, bs, snapshot.Namespace)
	if err != nil {
		msg := fmt.Sprintf("source pod not found: %v", err)
		log.Error(err, msg)
		_ = r.updateSnapshotStatus(ctx, snapshot, sandboxv1alpha1.SandboxSnapshotPhaseFailed, msg)
		return ctrl.Result{}, nil
	}

	// Resolve source pod info and generate image URIs
	sourcePodName := pod.Name
	sourceNodeName := pod.Spec.NodeName

	// Build container snapshots with image URIs using BatchSandbox generation
	var containers []sandboxv1alpha1.ContainerSnapshot
	for _, c := range bs.Spec.Template.Spec.Containers {
		imageURI := fmt.Sprintf("%s/%s-%s:snap-gen%d", r.SnapshotRegistry, bs.Name, c.Name, bs.Generation)
		containers = append(containers, sandboxv1alpha1.ContainerSnapshot{
			ContainerName: c.Name,
			ImageURI:      imageURI,
		})
	}
	if len(containers) == 0 {
		msg := fmt.Sprintf("no containers found in BatchSandbox %s template", bs.Name)
		_ = r.updateSnapshotStatus(ctx, snapshot, sandboxv1alpha1.SandboxSnapshotPhaseFailed, msg)
		return ctrl.Result{}, nil
	}

	// Persist resolved data to status
	if err := r.persistResolvedData(ctx, snapshot, sourcePodName, sourceNodeName, containers); err != nil {
		return ctrl.Result{}, err
	}
	// Update local snapshot object for buildCommitJob
	snapshot.Status.SourcePodName = sourcePodName
	snapshot.Status.SourceNodeName = sourceNodeName
	snapshot.Status.Containers = containers

	// Build and create the commit Job
	job, err := r.buildCommitJob(snapshot)
	if err != nil {
		msg := fmt.Sprintf("failed to build commit job: %v", err)
		_ = r.updateSnapshotStatus(ctx, snapshot, sandboxv1alpha1.SandboxSnapshotPhaseFailed, msg)
		return ctrl.Result{}, nil
	}

	// Check if job already exists
	existingJob := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: job.Namespace, Name: job.Name}, existingJob); err == nil {
		log.Info("Commit job already exists", "job", job.Name)
		_ = r.updateSnapshotStatus(ctx, snapshot, sandboxv1alpha1.SandboxSnapshotPhaseCommitting, "Commit job already exists")
		return ctrl.Result{RequeueAfter: time.Second}, nil
	} else if !errors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	if err := r.Create(ctx, job); err != nil {
		log.Error(err, "Failed to create commit job")
		r.Recorder.Eventf(snapshot, corev1.EventTypeWarning, "FailedCreateJob", "Failed to create commit job: %v", err)
		return ctrl.Result{}, err
	}

	log.Info("Created commit job", "job", job.Name)
	r.Recorder.Eventf(snapshot, corev1.EventTypeNormal, "CreatedJob", "Created commit job: %s", job.Name)
	_ = r.updateSnapshotStatus(ctx, snapshot, sandboxv1alpha1.SandboxSnapshotPhaseCommitting, "Commit job created")

	return ctrl.Result{RequeueAfter: time.Second}, nil
}

// handleCommitting checks the commit Job status and transitions to Ready or Failed.
func (r *SandboxSnapshotReconciler) handleCommitting(ctx context.Context, snapshot *sandboxv1alpha1.SandboxSnapshot) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	jobName := r.getJobName(snapshot)
	job := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: snapshot.Namespace, Name: jobName}, job); err != nil {
		if errors.IsNotFound(err) {
			log.Info("Commit job not found, re-creating", "job", jobName)
			return r.handlePending(ctx, snapshot)
		}
		return ctrl.Result{}, err
	}

	// Check job succeeded
	if job.Status.Succeeded > 0 {
		log.Info("Commit job succeeded", "job", jobName)
		r.Recorder.Eventf(snapshot, corev1.EventTypeNormal, "JobSucceeded", "Commit job succeeded")

		now := metav1.Now()
		return ctrl.Result{}, retry.RetryOnConflict(retry.DefaultBackoff, func() error {
			latest := &sandboxv1alpha1.SandboxSnapshot{}
			if err := r.Get(ctx, types.NamespacedName{Namespace: snapshot.Namespace, Name: snapshot.Name}, latest); err != nil {
				return err
			}
			latest.Status.Phase = sandboxv1alpha1.SandboxSnapshotPhaseReady
			latest.Status.Message = "Snapshot is ready"
			latest.Status.ReadyAt = &now
			return r.Status().Update(ctx, latest)
		})
	}

	// Check job failed
	if job.Status.Failed > 0 {
		message := "Commit job failed"
		for _, condition := range job.Status.Conditions {
			if condition.Type == batchv1.JobFailed {
				message = condition.Message
				break
			}
		}
		log.Info("Commit job failed", "job", jobName, "message", message)
		r.Recorder.Eventf(snapshot, corev1.EventTypeWarning, "JobFailed", "Commit job failed")
		_ = r.updateSnapshotStatus(ctx, snapshot, sandboxv1alpha1.SandboxSnapshotPhaseFailed, message)
		return ctrl.Result{}, nil
	}

	// Job still running
	log.Info("Commit job still running", "job", jobName)
	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

// handleDeletion cleans up the commit job and removes the finalizer.
func (r *SandboxSnapshotReconciler) handleDeletion(ctx context.Context, snapshot *sandboxv1alpha1.SandboxSnapshot) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Clean up commit job
	jobName := r.getJobName(snapshot)
	job := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: snapshot.Namespace, Name: jobName}, job); err == nil {
		if deleteErr := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); deleteErr != nil && !errors.IsNotFound(deleteErr) {
			return ctrl.Result{}, deleteErr
		}
		log.Info("Deleted commit job", "job", jobName)
	}

	// Remove finalizer
	if controllerutil.ContainsFinalizer(snapshot, SandboxSnapshotFinalizer) {
		if err := utils.UpdateFinalizer(r.Client, snapshot, utils.RemoveFinalizerOpType, SandboxSnapshotFinalizer); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

// findPodForSandbox finds the running pod belonging to a BatchSandbox.
func (r *SandboxSnapshotReconciler) findPodForSandbox(ctx context.Context, bs *sandboxv1alpha1.BatchSandbox, namespace string) (*corev1.Pod, error) {
	// Try alloc-status annotation first (pool-based allocation)
	alloc, err := parseSandboxAllocation(bs)
	if err == nil && len(alloc.Pods) > 0 {
		for _, podName := range alloc.Pods {
			pod := &corev1.Pod{}
			if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: podName}, pod); err == nil {
				if pod.Status.Phase == corev1.PodRunning {
					return pod, nil
				}
			}
		}
	}

	// Fallback: find by batch-sandbox name label
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList,
		client.InNamespace(namespace),
		client.MatchingLabels{LabelBatchSandboxNameKey: bs.Name},
	); err != nil {
		return nil, fmt.Errorf("failed to list pods: %w", err)
	}
	for i := range podList.Items {
		if podList.Items[i].Status.Phase == corev1.PodRunning {
			return &podList.Items[i], nil
		}
	}

	// Fallback: find by naming convention {batchSandboxName}-0
	podName := fmt.Sprintf("%s-0", bs.Name)
	pod := &corev1.Pod{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: podName}, pod); err == nil {
		if pod.Status.Phase == corev1.PodRunning {
			return pod, nil
		}
	}

	return nil, fmt.Errorf("no running pod found for BatchSandbox %s", bs.Name)
}

// persistResolvedData writes resolved pod/container info to status.
func (r *SandboxSnapshotReconciler) persistResolvedData(ctx context.Context, snapshot *sandboxv1alpha1.SandboxSnapshot, sourcePodName, sourceNodeName string, containers []sandboxv1alpha1.ContainerSnapshot) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &sandboxv1alpha1.SandboxSnapshot{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: snapshot.Namespace, Name: snapshot.Name}, latest); err != nil {
			return err
		}
		latest.Status.SourcePodName = sourcePodName
		latest.Status.SourceNodeName = sourceNodeName
		latest.Status.Containers = containers
		return r.Status().Update(ctx, latest)
	})
}

// buildCommitJob builds a Job for committing container snapshots.
func (r *SandboxSnapshotReconciler) buildCommitJob(snapshot *sandboxv1alpha1.SandboxSnapshot) (*batchv1.Job, error) {
	jobName := r.getJobName(snapshot)
	imageCommitterImage := r.ImageCommitterImage
	if imageCommitterImage == "" {
		imageCommitterImage = "image-committer:dev"
	}

	volumeMounts := []corev1.VolumeMount{
		{Name: "containerd-sock", MountPath: ContainerdSocketPath},
	}
	volumes := []corev1.Volume{
		{
			Name: "containerd-sock",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: ContainerdSocketPath},
			},
		},
	}

	// Add registry credentials from startup-param configured secret
	if r.SnapshotPushSecret != "" {
		volumes = append(volumes, corev1.Volume{
			Name: "registry-creds",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: r.SnapshotPushSecret,
					Items: []corev1.KeyToPath{
						{Key: ".dockerconfigjson", Path: "config.json"},
					},
				},
			},
		})
		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name: "registry-creds", MountPath: "/var/run/opensandbox/registry", ReadOnly: true,
		})
	}

	// Build commit command: image-committer <pod> <namespace> <container:uri> [...]
	var containerSpecs []string
	for _, cs := range snapshot.Status.Containers {
		containerSpecs = append(containerSpecs, fmt.Sprintf("%s:%s", cs.ContainerName, cs.ImageURI))
	}
	fullCommand := fmt.Sprintf("/usr/local/bin/image-committer %s %s %s",
		snapshot.Status.SourcePodName,
		snapshot.Namespace,
		strings.Join(containerSpecs, " "),
	)

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: snapshot.Namespace,
			Labels:    map[string]string{LabelSandboxSnapshotName: snapshot.Name},
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: ptrToInt32(int32(DefaultTTLSecondsAfterFinished)),
			ActiveDeadlineSeconds:   ptrToInt64(int64(r.getCommitJobTimeout().Seconds())),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:            CommitJobContainerName,
							Image:           imageCommitterImage,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Command:         []string{"/bin/sh", "-c"},
							Args:            []string{fullCommand},
							VolumeMounts:    volumeMounts,
							Env: []corev1.EnvVar{
								{Name: "CONTAINERD_SOCKET", Value: ContainerdSocketPath},
							},
							SecurityContext: &corev1.SecurityContext{
								RunAsUser: ptrToInt64(0),
							},
						},
					},
					Volumes:  volumes,
					NodeName: snapshot.Status.SourceNodeName,
				},
			},
		},
	}

	if err := ctrl.SetControllerReference(snapshot, job, r.Scheme); err != nil {
		return nil, fmt.Errorf("failed to set controller reference: %w", err)
	}
	return job, nil
}

func (r *SandboxSnapshotReconciler) getJobName(snapshot *sandboxv1alpha1.SandboxSnapshot) string {
	return fmt.Sprintf("%s-commit", snapshot.Name)
}

func (r *SandboxSnapshotReconciler) updateSnapshotStatus(ctx context.Context, snapshot *sandboxv1alpha1.SandboxSnapshot, phase sandboxv1alpha1.SandboxSnapshotPhase, message string) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &sandboxv1alpha1.SandboxSnapshot{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: snapshot.Namespace, Name: snapshot.Name}, latest); err != nil {
			return err
		}
		latest.Status.Phase = phase
		latest.Status.Message = message
		return r.Status().Update(ctx, latest)
	})
}

func (r *SandboxSnapshotReconciler) getCommitJobTimeout() time.Duration {
	if r.CommitJobTimeout > 0 {
		return r.CommitJobTimeout
	}
	return DefaultCommitJobTimeout
}

func ptrToInt64(v int64) *int64 { return &v }
func ptrToInt32(v int32) *int32 { return &v }

// SetupWithManager sets up the controller with the Manager.
func (r *SandboxSnapshotReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&sandboxv1alpha1.SandboxSnapshot{}).
		Owns(&batchv1.Job{}).
		Named("sandboxsnapshot").
		Complete(r)
}
