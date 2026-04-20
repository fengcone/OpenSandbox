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
	gerrors "errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/controller/strategy"
	taskscheduler "github.com/alibaba/OpenSandbox/sandbox-k8s/internal/scheduler"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils"
	controllerutils "github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils/controller"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils/expectations"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils/fieldindex"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils/requeueduration"
)

var (
	BatchSandboxScaleExpectations = expectations.NewScaleExpectations()
	DurationStore                 = requeueduration.DurationStore{}
)

// BatchSandboxReconciler reconciles a BatchSandbox object
type BatchSandboxReconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	Recorder       record.EventRecorder
	taskSchedulers sync.Map
	// ResumePullSecret is the K8s Secret name for pulling snapshot images during resume.
	ResumePullSecret string
}

// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=events,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=sandbox.opensandbox.io,resources=batchsandboxes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=sandbox.opensandbox.io,resources=batchsandboxes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=sandbox.opensandbox.io,resources=batchsandboxes/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the BatchSandbox object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.21.0/pkg/reconcile
func (r *BatchSandboxReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	var aggErrors []error
	defer func() {
		_ = DurationStore.Pop(req.String())
	}()
	batchSbx := &sandboxv1alpha1.BatchSandbox{}
	if err := r.Get(ctx, client.ObjectKey{
		Namespace: req.Namespace,
		Name:      req.Name,
	}, batchSbx); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	// handle expire
	if expireAt := batchSbx.Spec.ExpireTime; expireAt != nil {
		now := time.Now()
		if expireAt.Time.Before(now) {
			if batchSbx.DeletionTimestamp == nil {
				log.Info("batch sandbox expired, delete", "expireAt", expireAt)
				if err := r.Delete(ctx, batchSbx); err != nil {
					if errors.IsNotFound(err) {
						return ctrl.Result{}, nil
					}
					return ctrl.Result{}, err
				}
			}
		} else {
			DurationStore.Push(types.NamespacedName{Namespace: batchSbx.Namespace, Name: batchSbx.Name}.String(), expireAt.Time.Sub(now))
		}
	}

	// task schedule
	taskStrategy := strategy.NewTaskSchedulingStrategy(batchSbx)

	// pool strategy
	poolStrategy := strategy.NewPoolStrategy(batchSbx)

	// handle finalizers
	if batchSbx.DeletionTimestamp == nil {
		if taskStrategy.NeedTaskScheduling() {
			if !controllerutil.ContainsFinalizer(batchSbx, FinalizerTaskCleanup) {
				err := utils.UpdateFinalizer(r.Client, batchSbx, utils.AddFinalizerOpType, FinalizerTaskCleanup)
				if err != nil {
					log.Error(err, "failed to add finalizer", "finalizer", FinalizerTaskCleanup)
				} else {
					log.Info("added finalizer", "finalizer", FinalizerTaskCleanup)
				}
				return ctrl.Result{}, err
			}
		}
	} else {
		if !taskStrategy.NeedTaskScheduling() {
			return ctrl.Result{}, nil
		}
	}

	// Pause/Resume dispatch: handles pause/resume intent before normal scaling.
	if result, handled, err := r.dispatchPauseResume(ctx, batchSbx); handled {
		return result, err
	}

	pods, err := r.listPods(ctx, poolStrategy, batchSbx)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to list pods %w", err)
	}
	podIndex, err := calPodIndex(poolStrategy, batchSbx, pods)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to cal pod index %w", err)
	}
	slices.SortStableFunc(pods, utils.MultiPodSorter([]func(a, b *corev1.Pod) int{
		utils.WithPodIndexSorter(podIndex),
		utils.PodNameSorter,
	}).Sort)
	// Normal Mode need scale Pods
	if !poolStrategy.IsPooledMode() {
		err := r.scaleBatchSandbox(ctx, batchSbx, batchSbx.Spec.Template, pods)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to scale batch sandbox %w", err)
		}
	}

	// TODO merge task status update
	newStatus := batchSbx.Status.DeepCopy()
	newStatus.ObservedGeneration = batchSbx.Generation
	newStatus.Replicas = 0
	newStatus.Allocated = 0
	newStatus.Ready = 0
	ipList := make([]string, len(pods))
	for i, pod := range pods {
		newStatus.Replicas++
		if utils.IsAssigned(pod) {
			newStatus.Allocated++
			ipList[i] = pod.Status.PodIP
		}
		if pod.Status.Phase == corev1.PodRunning && utils.IsPodReady(pod) {
			newStatus.Ready++
		}
	}
	// Update phase based on pod state
	switch batchSbx.Status.Phase {
	case sandboxv1alpha1.BatchSandboxPhasePausing, sandboxv1alpha1.BatchSandboxPhasePaused:
		// Don't override Pausing/Paused phases
	case sandboxv1alpha1.BatchSandboxPhaseResuming:
		// Check for Pod startup failures first
		if len(pods) > 0 {
			for _, pod := range pods {
				if isPodFailed(pod) {
					msg := getPodFailureMessage(pod)
					_ = r.setCondition(ctx, batchSbx, sandboxv1alpha1.BatchSandboxConditionResumeFailed, sandboxv1alpha1.ConditionTrue, "PodStartFailed", msg)
					newStatus.Phase = sandboxv1alpha1.BatchSandboxPhaseFailed
					break
				}
			}
		}
		// Only check for successful resume if not already failed
		if newStatus.Phase != sandboxv1alpha1.BatchSandboxPhaseFailed {
			if newStatus.Ready > 0 {
				// Resume complete once pods are ready
				newStatus.Phase = sandboxv1alpha1.BatchSandboxPhaseRunning
				// Delete SandboxSnapshot after successful resume
				snapshot := &sandboxv1alpha1.SandboxSnapshot{}
				if err := r.Get(ctx, types.NamespacedName{Namespace: batchSbx.Namespace, Name: batchSbx.Name}, snapshot); err == nil {
					if err := r.Delete(ctx, snapshot); err != nil && !errors.IsNotFound(err) {
						log.Error(err, "Failed to delete SandboxSnapshot after successful resume")
					} else {
						log.Info("Deleted SandboxSnapshot after successful resume")
					}
				}
			}
			// If Ready == 0, keep Resuming phase (don't fall through to default)
		}
		// Don't fall through to default case - Resuming phase should be preserved
		// unless explicitly transitioned to Running or Failed above
		if newStatus.Phase == batchSbx.Status.Phase {
			// Phase not changed above, need to set appropriate phase
			if newStatus.Phase != sandboxv1alpha1.BatchSandboxPhaseFailed {
				if newStatus.Ready > 0 {
					newStatus.Phase = sandboxv1alpha1.BatchSandboxPhaseRunning
				}
				// If Ready == 0, keep current phase (Resuming)
			}
		}
	default:
		// Handle initial state (empty phase) and other phases
		// Also check for Pod failures in non-Resuming phases
		if len(pods) > 0 {
			for _, pod := range pods {
				if isPodFailed(pod) {
					// Pod failed - set to Failed if not already
					if batchSbx.Status.Phase != sandboxv1alpha1.BatchSandboxPhaseFailed {
						msg := getPodFailureMessage(pod)
						_ = r.setCondition(ctx, batchSbx, sandboxv1alpha1.BatchSandboxConditionResumeFailed, sandboxv1alpha1.ConditionTrue, "PodStartFailed", msg)
						newStatus.Phase = sandboxv1alpha1.BatchSandboxPhaseFailed
					}
					break
				}
			}
		}
		// Set phase based on Ready count (only if not already set to Failed above)
		if newStatus.Phase != sandboxv1alpha1.BatchSandboxPhaseFailed {
			if newStatus.Ready > 0 {
				newStatus.Phase = sandboxv1alpha1.BatchSandboxPhaseRunning
			} else {
				newStatus.Phase = sandboxv1alpha1.BatchSandboxPhasePending
			}
		}
	}
	raw, _ := json.Marshal(ipList)
	if batchSbx.Annotations[AnnotationSandboxEndpoints] != string(raw) {
		patchData, _ := json.Marshal(map[string]any{
			"metadata": map[string]any{
				"annotations": map[string]string{
					AnnotationSandboxEndpoints: string(raw),
				},
			},
		})
		obj := &sandboxv1alpha1.BatchSandbox{ObjectMeta: metav1.ObjectMeta{Namespace: batchSbx.Namespace, Name: batchSbx.Name}}
		if err := r.Patch(ctx, obj, client.RawPatch(types.MergePatchType, patchData)); err != nil {
			log.Error(err, "failed to patch annotation", "annotation", AnnotationSandboxEndpoints, "body", string(patchData))
			aggErrors = append(aggErrors, err)
		}
	}
	if !reflect.DeepEqual(newStatus, batchSbx.Status) {
		log.Info("To update BatchSandbox status", "replicas", newStatus.Replicas, "allocated", newStatus.Allocated, "ready", newStatus.Ready)
		if err := r.updateStatus(batchSbx, newStatus); err != nil {
			aggErrors = append(aggErrors, err)
		}
	}

	if taskStrategy.NeedTaskScheduling() {
		// Because tasks are in-memory and there is no event mechanism, periodic reconciliation is required.
		DurationStore.Push(types.NamespacedName{Namespace: batchSbx.Namespace, Name: batchSbx.Name}.String(), 3*time.Second)
		sch, err := r.getTaskScheduler(ctx, batchSbx, pods)
		if err != nil {
			return ctrl.Result{}, err
		}
		if batchSbx.DeletionTimestamp != nil {
			stoppingTasks := sch.StopTask()
			if len(stoppingTasks) > 0 {
				log.Info("stopping tasks", "count", len(stoppingTasks))
			}
		}
		now := time.Now()
		if err = r.scheduleTasks(ctx, sch, batchSbx); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to schedule tasks, err %w", err)
		} else {
			log.Info("schedule tasks completed", "costMs", time.Since(now).Milliseconds())
		}
		// check task cleanup is finished
		if batchSbx.DeletionTimestamp != nil {
			unfinishedTasks := r.getTasksCleanupUnfinished(batchSbx, sch)
			if len(unfinishedTasks) > 0 {
				log.Info("tasks cleanup is unfinished", "unfinishedCount", len(unfinishedTasks))
			} else {
				var err error
				if controllerutil.ContainsFinalizer(batchSbx, FinalizerTaskCleanup) {
					err = utils.UpdateFinalizer(r.Client, batchSbx, utils.RemoveFinalizerOpType, FinalizerTaskCleanup)
					if err != nil {
						if errors.IsNotFound(err) {
							err = nil
						} else {
							log.Error(err, "failed to remove finalizer", "finalizer", FinalizerTaskCleanup)
						}
					}
				}
				if err == nil {
					r.deleteTaskScheduler(ctx, batchSbx)
					log.Info("task cleanup is finished, removed finalizer", "finalizer", FinalizerTaskCleanup)
				}
				return ctrl.Result{}, err
			}
		}
	}

	return reconcile.Result{RequeueAfter: DurationStore.Pop(req.String())}, gerrors.Join(aggErrors...)
}

func calPodIndex(poolStrategy strategy.PoolStrategy, batchSbx *sandboxv1alpha1.BatchSandbox, pods []*corev1.Pod) (map[string]int, error) {
	podIndex := map[string]int{}
	if poolStrategy.IsPooledMode() {
		// cal index from pool alloc result while using pooling
		alloc, err := parseSandboxAllocation(batchSbx)
		if err != nil {
			return nil, err
		}
		for i := range alloc.Pods {
			podIndex[alloc.Pods[i]] = i
		}
	} else {
		for i := range pods {
			po := pods[i]
			idx, err := parseIndex(po)
			if err != nil {
				return nil, fmt.Errorf("batchsandbox: failed to parse %s/%s index %w", po.Namespace, po.Name, err)
			}
			podIndex[po.Name] = idx
		}
	}
	return podIndex, nil
}

func (r *BatchSandboxReconciler) listPods(ctx context.Context, poolStrategy strategy.PoolStrategy, batchSbx *sandboxv1alpha1.BatchSandbox) ([]*corev1.Pod, error) {
	var ret []*corev1.Pod
	if poolStrategy.IsPooledMode() {
		var (
			allocSet    = make(sets.Set[string])
			releasedSet = make(sets.Set[string])
		)
		alloc, err := parseSandboxAllocation(batchSbx)
		if err != nil {
			return nil, err
		}
		allocSet.Insert(alloc.Pods...)

		released, err := parseSandboxReleased(batchSbx)
		if err != nil {
			return nil, err
		}
		releasedSet.Insert(released.Pods...)

		activePods := allocSet.Difference(releasedSet)
		for name := range activePods {
			pod := &corev1.Pod{}
			// TODO maybe performance is problem
			if err := r.Client.Get(ctx, types.NamespacedName{Namespace: batchSbx.Namespace, Name: name}, pod); err != nil {
				if errors.IsNotFound(err) {
					continue
				}
				return nil, err
			}
			ret = append(ret, pod)
		}
	} else {
		podList := &corev1.PodList{}
		if err := r.Client.List(ctx, podList, &client.ListOptions{
			Namespace:     batchSbx.Namespace,
			FieldSelector: fields.SelectorFromSet(fields.Set{fieldindex.IndexNameForOwnerRefUID: string(batchSbx.UID)}),
		}); err != nil {
			return nil, err
		}
		for i := range podList.Items {
			ret = append(ret, &podList.Items[i])
		}
	}
	return ret, nil
}

func (r *BatchSandboxReconciler) getTaskScheduler(ctx context.Context, batchSbx *sandboxv1alpha1.BatchSandbox, pods []*corev1.Pod) (taskscheduler.TaskScheduler, error) {
	log := logf.FromContext(ctx)
	var tSch taskscheduler.TaskScheduler
	key := types.NamespacedName{Namespace: batchSbx.Namespace, Name: batchSbx.Name}.String()
	val, ok := r.taskSchedulers.Load(key)
	// The reconciler guarantees that it will not concurrently reconcile the same BatchSandbox.
	if !ok {
		policy := sandboxv1alpha1.TaskResourcePolicyRetain
		if batchSbx.Spec.TaskResourcePolicyWhenCompleted != nil {
			policy = *batchSbx.Spec.TaskResourcePolicyWhenCompleted
		}
		taskStrategy := strategy.NewTaskSchedulingStrategy(batchSbx)
		taskSpecs, err := taskStrategy.GenerateTaskSpecs()
		if err != nil {
			return nil, err
		}
		sc, err := taskscheduler.NewTaskScheduler(key, taskSpecs, pods, policy, log)
		if err != nil {
			return nil, fmt.Errorf("new task scheduler err %w", err)
		}
		log.Info("successfully created task scheduler")
		tSch = sc
		r.taskSchedulers.Store(key, sc)
	} else {
		tSch, ok = (val.(taskscheduler.TaskScheduler))
		if !ok {
			return nil, gerrors.New("invalid scheduler type stored")
		}
		// Update the pods list for this scheduler
		tSch.UpdatePods(pods)
	}
	return tSch, nil
}

func (r *BatchSandboxReconciler) deleteTaskScheduler(ctx context.Context, batchSbx *sandboxv1alpha1.BatchSandbox) {
	log := logf.FromContext(ctx)
	log.Info("delete task scheduler")
	key := types.NamespacedName{Namespace: batchSbx.Namespace, Name: batchSbx.Name}.String()
	r.taskSchedulers.Delete(key)
}

func (r *BatchSandboxReconciler) scheduleTasks(ctx context.Context, tSch taskscheduler.TaskScheduler, batchSbx *sandboxv1alpha1.BatchSandbox) error {
	log := logf.FromContext(ctx)
	if err := tSch.Schedule(); err != nil {
		return err
	}
	tasks := tSch.ListTask()
	toReleasedPods := []string{}
	var (
		running, failed, succeed, unknown int32
		pending                           int32
	)
	for i := range len(tasks) {
		task := tasks[i]
		if task.GetPodName() == "" {
			pending++
		} else {
			state := task.GetState()
			if task.IsResourceReleased() {
				toReleasedPods = append(toReleasedPods, task.GetPodName())
			}
			switch state {
			case taskscheduler.RunningTaskState:
				running++
			case taskscheduler.SucceedTaskState:
				succeed++
			case taskscheduler.FailedTaskState:
				failed++
			case taskscheduler.UnknownTaskState:
				unknown++
			}
		}
	}
	if len(toReleasedPods) > 0 {
		log.Info("try to release Pods", "count", len(toReleasedPods))
		if err := r.releasePods(ctx, batchSbx, toReleasedPods); err != nil {
			return err
		}
		log.Info("successfully released Pods", "count", len(toReleasedPods))
	}
	oldStatus := batchSbx.Status
	newStatus := oldStatus.DeepCopy()
	newStatus.ObservedGeneration = batchSbx.Generation
	newStatus.TaskRunning = running
	newStatus.TaskFailed = failed
	newStatus.TaskSucceed = succeed
	newStatus.TaskUnknown = unknown
	newStatus.TaskPending = pending
	if !reflect.DeepEqual(newStatus, oldStatus) {
		log.Info("To update BatchSandbox status", "replicas", newStatus.Replicas, "task_running", newStatus.TaskRunning, "task_succeed", newStatus.TaskSucceed, "task_failed", newStatus.TaskFailed, "task_unknown", newStatus.TaskUnknown, "task_pending", newStatus.TaskPending)
		if err := r.updateStatus(batchSbx, newStatus); err != nil {
			return err
		}
	}
	return nil
}

func (r *BatchSandboxReconciler) getTasksCleanupUnfinished(batchSbx *sandboxv1alpha1.BatchSandbox, tSch taskscheduler.TaskScheduler) []taskscheduler.Task {
	var notReleased []taskscheduler.Task
	for _, task := range tSch.ListTask() {
		if !task.IsResourceReleased() {
			notReleased = append(notReleased, task)
		}
	}
	return notReleased
}

func (r *BatchSandboxReconciler) releasePods(ctx context.Context, batchSbx *sandboxv1alpha1.BatchSandbox, toReleasePods []string) error {
	releasedSet := make(sets.Set[string])
	released, err := parseSandboxReleased(batchSbx)
	if err != nil {
		return err
	}
	releasedSet.Insert(released.Pods...)
	releasedSet.Insert(toReleasePods...)
	newRelease := AllocationRelease{
		Pods: sets.List(releasedSet),
	}
	raw, err := json.Marshal(newRelease)
	if err != nil {
		return fmt.Errorf("Failed to marshal released pod names: %v", err)
	}
	body := utils.DumpJSON(struct {
		MetaData metav1.ObjectMeta `json:"metadata"`
	}{
		MetaData: metav1.ObjectMeta{
			Annotations: map[string]string{
				AnnoAllocReleaseKey: string(raw),
			},
		},
	})
	b := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: batchSbx.Namespace,
			Name:      batchSbx.Name,
		},
	}
	return r.Client.Patch(ctx, b, client.RawPatch(types.MergePatchType, []byte(body)))
}

// Normal Mode
func (r *BatchSandboxReconciler) scaleBatchSandbox(ctx context.Context, batchSandbox *sandboxv1alpha1.BatchSandbox, podTemplateSpec *corev1.PodTemplateSpec, pods []*corev1.Pod) error {
	log := logf.FromContext(ctx)
	indexedPodMap := map[int]*corev1.Pod{}
	for i := range pods {
		pod := pods[i]
		BatchSandboxScaleExpectations.ObserveScale(controllerutils.GetControllerKey(batchSandbox), expectations.Create, pod.Name)
		idx, err := parseIndex(pod)
		if err != nil {
			return fmt.Errorf("failed to parse idx Pod %s, err %w", pod.Name, err)
		}
		indexedPodMap[idx] = pod
	}
	if satisfied, unsatisfiedDuration, dirtyPods := BatchSandboxScaleExpectations.SatisfiedExpectations(controllerutils.GetControllerKey(batchSandbox)); !satisfied {
		log.Info("scale expectation is not satisfied", "unsatisfiedDuration", unsatisfiedDuration, "dirtyPods", dirtyPods)
		DurationStore.Push(types.NamespacedName{Namespace: batchSandbox.Namespace, Name: batchSandbox.Name}.String(), expectations.ExpectationTimeout-unsatisfiedDuration)
		return nil
	}
	// TODO consider supply Pods if Pods is deleted unexpectedly
	var needCreateIndex []int
	// TODO var needDeleteIndex []int
	for i := 0; i < int(*batchSandbox.Spec.Replicas); i++ {
		_, ok := indexedPodMap[i]
		if !ok {
			needCreateIndex = append(needCreateIndex, i)
		}
	}
	// scale
	if len(needCreateIndex) > 0 {
		log.Info("try to create Pods", "count", len(needCreateIndex), "indexes", needCreateIndex)
	}
	for _, idx := range needCreateIndex {
		pod, err := utils.GetPodFromTemplate(podTemplateSpec, batchSandbox, metav1.NewControllerRef(batchSandbox, sandboxv1alpha1.SchemeBuilder.GroupVersion.WithKind("BatchSandbox")))
		if err != nil {
			return err
		}
		// Apply shard patch if available for this index
		if len(batchSandbox.Spec.ShardPatches) > 0 && idx < len(batchSandbox.Spec.ShardPatches) {
			podBytes, err := json.Marshal(pod)
			if err != nil {
				return fmt.Errorf("failed to marshal pod: %w", err)
			}
			patch := batchSandbox.Spec.ShardPatches[idx]
			modifiedPodBytes, err := strategicpatch.StrategicMergePatch(podBytes, patch.Raw, &corev1.Pod{})
			if err != nil {
				return fmt.Errorf("failed to apply shard patch for index %d: %w", idx, err)
			}
			if err := json.Unmarshal(modifiedPodBytes, pod); err != nil {
				return fmt.Errorf("failed to unmarshal patched pod for index %d: %w", idx, err)
			}
		}
		if err := ctrl.SetControllerReference(pod, batchSandbox, r.Scheme); err != nil {
			return err
		}
		pod.Labels[LabelBatchSandboxPodIndexKey] = strconv.Itoa(idx)
		pod.Labels[LabelBatchSandboxNameKey] = batchSandbox.Name
		pod.Namespace = batchSandbox.Namespace
		pod.Name = fmt.Sprintf("%s-%d", batchSandbox.Name, idx)
		BatchSandboxScaleExpectations.ExpectScale(controllerutils.GetControllerKey(batchSandbox), expectations.Create, pod.Name)
		if err := r.Create(ctx, pod); err != nil {
			BatchSandboxScaleExpectations.ObserveScale(controllerutils.GetControllerKey(batchSandbox), expectations.Create, pod.Name)
			r.Recorder.Eventf(batchSandbox, corev1.EventTypeWarning, "FailedCreate", "failed to create pod: %v, pod: %v", err, utils.DumpJSON(pod))
			return err
		}
		r.Recorder.Eventf(batchSandbox, corev1.EventTypeNormal, "SuccessfulCreate", "succeed to create pod %s", pod.Name)
	}
	return nil
}

func parseIndex(pod *corev1.Pod) (int, error) {
	if v := pod.Labels[LabelBatchSandboxPodIndexKey]; v != "" {
		return strconv.Atoi(v)
	}
	idx := strings.LastIndex(pod.Name, "-")
	if idx == -1 {
		return -1, gerrors.New("batchsandbox: Invalid pod Name")
	}
	return strconv.Atoi(pod.Name[idx+1:])
}

func (r *BatchSandboxReconciler) updateStatus(batchSandbox *sandboxv1alpha1.BatchSandbox, newStatus *sandboxv1alpha1.BatchSandboxStatus) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		clone := &sandboxv1alpha1.BatchSandbox{}
		if err := r.Get(context.TODO(), types.NamespacedName{Namespace: batchSandbox.Namespace, Name: batchSandbox.Name}, clone); err != nil {
			return err
		}
		clone.Status = *newStatus
		return r.Status().Update(context.TODO(), clone)
	})
}

// dispatchPauseResume implements the 5-case dispatch table from the design doc.
// Returns (result, handled, error). If handled=true, the caller should return immediately.
func (r *BatchSandboxReconciler) dispatchPauseResume(ctx context.Context, bs *sandboxv1alpha1.BatchSandbox) (ctrl.Result, bool, error) {
	log := logf.FromContext(ctx)
	generation := bs.Generation
	pauseObservedGen := bs.Status.PauseObservedGeneration
	pause := bs.Spec.Pause

	// If phase is Resuming, continue resume flow (handles ACK propagation delay)
	// After continueResume, let normal flow handle phase transition to Running
	log.Info("Dispatch: checking phase", "currentPhase", bs.Status.Phase, "generation", generation, "pauseObservedGen", pauseObservedGen, "pause", pause)
	if bs.Status.Phase == sandboxv1alpha1.BatchSandboxPhaseResuming {
		log.Info("Dispatch: phase is Resuming, continuing resume")
		result, err := r.continueResume(ctx, bs)
		if err != nil {
			return result, true, err
		}
		// Return handled=false to let normal flow update phase from Resuming to Running
		return result, false, nil
	}

	if generation > pauseObservedGen {
		if pause != nil {
			if *pause {
				log.Info("Dispatch: handlePause", "generation", generation, "pauseObservedGeneration", pauseObservedGen)
				result, err := r.handlePause(ctx, bs)
				return result, true, err
			}
			log.Info("Dispatch: handleResume", "generation", generation, "pauseObservedGeneration", pauseObservedGen, "currentPhase", bs.Status.Phase)
			result, err := r.handleResume(ctx, bs)
			return result, true, err
		}
		// pause == nil: spec change not related to pause, ACK only
		log.Info("Dispatch: ACK only", "generation", generation, "pauseObservedGeneration", pauseObservedGen)
		if err := r.ackPauseGeneration(ctx, bs); err != nil {
			return ctrl.Result{}, true, err
		}
		return ctrl.Result{}, false, nil
	}

	// generation == pauseObservedGeneration
	if pause != nil {
		log.Info("Dispatch: syncPauseOrClear", "generation", generation, "pause", *pause)
		result, err := r.syncPauseOrClear(ctx, bs)
		return result, true, err
	}
	// pause == nil: normal flow
	return ctrl.Result{}, false, nil
}

// handlePause implements the pause flow:
// 1. Pool mode: solidify template from Pool CR
// 2. ACK (pauseObservedGeneration + phase=Pausing)
// 3. Create SandboxSnapshot child resource
func (r *BatchSandboxReconciler) handlePause(ctx context.Context, bs *sandboxv1alpha1.BatchSandbox) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Clear any existing PauseFailed condition to mark start of new pause operation
	_ = r.setCondition(ctx, bs, sandboxv1alpha1.BatchSandboxConditionPauseFailed, sandboxv1alpha1.ConditionFalse, "", "")

	// Pool mode: solidify template before ACK
	if bs.Spec.Template == nil && bs.Spec.PoolRef != "" {
		pool := &sandboxv1alpha1.Pool{}
		if err := r.Get(ctx, types.NamespacedName{Name: bs.Spec.PoolRef, Namespace: bs.Namespace}, pool); err != nil {
			msg := fmt.Sprintf("pool CR %s not found: %v", bs.Spec.PoolRef, err)
			log.Error(err, msg)
			// Check if Pod still exists to determine phase
			phase := sandboxv1alpha1.BatchSandboxPhaseRunning
			reason := "PoolTemplateMissing"
			if _, podErr := r.findPodForSandbox(ctx, bs); podErr != nil {
				phase = sandboxv1alpha1.BatchSandboxPhaseFailed
				reason = "PodNotFound"
			}
			_ = r.ackPauseWithPhase(ctx, bs, phase, "")
			_ = r.setCondition(ctx, bs, sandboxv1alpha1.BatchSandboxConditionPauseFailed, sandboxv1alpha1.ConditionTrue, reason, msg)
			_ = r.clearPause(ctx, bs)
			return ctrl.Result{}, nil
		}
		if pool.Spec.Template == nil {
			msg := fmt.Sprintf("pool CR %s has nil template", bs.Spec.PoolRef)
			log.Error(nil, msg)
			phase := sandboxv1alpha1.BatchSandboxPhaseRunning
			reason := "PoolTemplateMissing"
			if _, podErr := r.findPodForSandbox(ctx, bs); podErr != nil {
				phase = sandboxv1alpha1.BatchSandboxPhaseFailed
				reason = "PodNotFound"
			}
			_ = r.ackPauseWithPhase(ctx, bs, phase, "")
			_ = r.setCondition(ctx, bs, sandboxv1alpha1.BatchSandboxConditionPauseFailed, sandboxv1alpha1.ConditionTrue, reason, msg)
			_ = r.clearPause(ctx, bs)
			return ctrl.Result{}, nil
		}
		// Patch spec.template to solidify pool template
		if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
			latest := &sandboxv1alpha1.BatchSandbox{}
			if err := r.Get(ctx, types.NamespacedName{Namespace: bs.Namespace, Name: bs.Name}, latest); err != nil {
				return err
			}
			patch := client.MergeFrom(latest.DeepCopy())
			latest.Spec.Template = pool.Spec.Template.DeepCopy()
			return r.Patch(ctx, latest, patch)
		}); err != nil {
			return ctrl.Result{}, err
		}
		// Re-fetch after patch (generation changed)
		if err := r.Get(ctx, types.NamespacedName{Namespace: bs.Namespace, Name: bs.Name}, bs); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("Solidified Pool template", "pool", bs.Spec.PoolRef)
	}

	// ACK: set pauseObservedGeneration and phase=Pausing
	if err := r.ackPauseWithPhase(ctx, bs, sandboxv1alpha1.BatchSandboxPhasePausing, ""); err != nil {
		return ctrl.Result{}, err
	}

	// Create SandboxSnapshot if not exists (idempotent)
	// If snapshot exists with Failed phase, delete it first for retry.
	snapshot := &sandboxv1alpha1.SandboxSnapshot{}
	err := r.Get(ctx, types.NamespacedName{Namespace: bs.Namespace, Name: bs.Name}, snapshot)
	if errors.IsNotFound(err) {
		// Create new snapshot
		snapshot = &sandboxv1alpha1.SandboxSnapshot{
			ObjectMeta: metav1.ObjectMeta{
				Name:      bs.Name,
				Namespace: bs.Namespace,
			},
			Spec: sandboxv1alpha1.SandboxSnapshotSpec{
				SandboxName: bs.Name,
			},
		}
		if err := controllerutil.SetControllerReference(bs, snapshot, r.Scheme); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, snapshot); err != nil && !errors.IsAlreadyExists(err) {
			return ctrl.Result{}, err
		}
		log.Info("Created SandboxSnapshot", "snapshot", bs.Name)
	} else if err != nil {
		return ctrl.Result{}, err
	} else if snapshot.Status.Phase == sandboxv1alpha1.SandboxSnapshotPhaseFailed {
		// Old Failed snapshot: delete so we can recreate
		log.Info("Deleting Failed SandboxSnapshot for retry", "snapshot", bs.Name)
		if err := r.Delete(ctx, snapshot); err != nil && !errors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}

	return ctrl.Result{RequeueAfter: time.Second}, nil
}

// handleResume implements the resume flow:
// 1. ACK (pauseObservedGeneration + phase=Resuming)
// 2. Read SandboxSnapshot status for image URIs
// 3. Replace template container images
// 4. Pool mode: clear poolRef
// 5. Scale up replicas, clear pause, delete snapshot
func (r *BatchSandboxReconciler) handleResume(ctx context.Context, bs *sandboxv1alpha1.BatchSandbox) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Clear any existing ResumeFailed condition to mark start of new resume operation
	_ = r.setCondition(ctx, bs, sandboxv1alpha1.BatchSandboxConditionResumeFailed, sandboxv1alpha1.ConditionFalse, "", "")

	// ACK Resuming phase (idempotent)
	log.Info("ACK Resuming phase")
	if err := r.ackPauseWithPhase(ctx, bs, sandboxv1alpha1.BatchSandboxPhaseResuming, ""); err != nil {
		return ctrl.Result{}, err
	}

	// Requeue to allow status update to propagate before patch
	return ctrl.Result{RequeueAfter: time.Second}, nil
}

// syncPauseOrClear handles the case where generation == pauseObservedGeneration && pause != nil.
// This occurs when:
// - The controller has ACKed and is waiting for SandboxSnapshot to complete (sync pause status)
// - The spec.pause clear failed and needs recovery (clear pause)
func (r *BatchSandboxReconciler) syncPauseOrClear(ctx context.Context, bs *sandboxv1alpha1.BatchSandbox) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Check if SandboxSnapshot exists
	snapshot := &sandboxv1alpha1.SandboxSnapshot{}
	err := r.Get(ctx, types.NamespacedName{Namespace: bs.Namespace, Name: bs.Name}, snapshot)
	if errors.IsNotFound(err) {
		// No snapshot yet: wait for it to be created (requeue)
		log.Info("SandboxSnapshot not found yet, waiting")
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	// Snapshot exists: sync its status
	switch snapshot.Status.Phase {
	case sandboxv1alpha1.SandboxSnapshotPhaseReady:
		// Pause complete: scale to 0, set Paused, clear pause
		log.Info("SandboxSnapshot Ready, completing pause")
		if err := r.completePause(ctx, bs); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Second}, nil

	case sandboxv1alpha1.SandboxSnapshotPhaseFailed:
		// Snapshot failed - determine phase based on Pod existence
		msg := snapshot.Status.Message
		if msg == "" {
			msg = "snapshot failed"
		}
		log.Info("SandboxSnapshot Failed", "message", msg)

		// Check if Pod still exists to determine phase
		phase := sandboxv1alpha1.BatchSandboxPhaseRunning
		reason := "CommitPushFailed"
		if _, podErr := r.findPodForSandbox(ctx, bs); podErr != nil {
			phase = sandboxv1alpha1.BatchSandboxPhaseFailed
			reason = "PodNotFound"
		}

		_ = r.ackPauseWithPhase(ctx, bs, phase, "")
		_ = r.setCondition(ctx, bs, sandboxv1alpha1.BatchSandboxConditionPauseFailed, sandboxv1alpha1.ConditionTrue, reason, msg)
		_ = r.clearPause(ctx, bs)
		return ctrl.Result{}, nil

	case sandboxv1alpha1.SandboxSnapshotPhasePending, sandboxv1alpha1.SandboxSnapshotPhaseCommitting:
		// Still in progress, requeue
		log.Info("SandboxSnapshot in progress", "phase", snapshot.Status.Phase)
		return ctrl.Result{RequeueAfter: time.Second}, nil

	default:
		// Unknown phase, requeue
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
}

// completePause finalizes the pause operation:
// 1. Set phase=Paused
// 2. Normal mode: delete all Pods (cascade via OwnerRef)
// 3. Pool mode: set alloc-release annotation (Pool Controller will release Pods)
// 4. Clear spec.pause
func (r *BatchSandboxReconciler) completePause(ctx context.Context, bs *sandboxv1alpha1.BatchSandbox) error {
	log := logf.FromContext(ctx)

	// Step 1: Write status.phase=Paused FIRST
	if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &sandboxv1alpha1.BatchSandbox{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: bs.Namespace, Name: bs.Name}, latest); err != nil {
			return err
		}
		latest.Status.Phase = sandboxv1alpha1.BatchSandboxPhasePaused
		latest.Status.Message = ""
		return r.Status().Update(ctx, latest)
	}); err != nil {
		return err
	}

	// Get all Pods owned by this BatchSandbox (using label selector instead of field index)
	podList := &corev1.PodList{}
	if err := r.Client.List(ctx, podList, client.InNamespace(bs.Namespace)); err != nil {
		return err
	}
	var pods []*corev1.Pod
	for i := range podList.Items {
		pod := &podList.Items[i]
		// Check if pod is owned by this BatchSandbox
		for _, ownerRef := range pod.OwnerReferences {
			if ownerRef.Kind == "BatchSandbox" && ownerRef.Name == bs.Name {
				pods = append(pods, pod)
				break
			}
		}
	}

	// Clear expectations before deleting pods to avoid blocking future resume
	controllerKey := controllerutils.GetControllerKey(bs)
	BatchSandboxScaleExpectations.DeleteExpectations(controllerKey)
	log.Info("Cleared scale expectations before pod deletion", "controllerKey", controllerKey)

	// Step 2: Handle Pod cleanup based on mode
	if bs.Spec.PoolRef != "" {
		// Pool mode: set alloc-release annotation
		if len(pods) > 0 {
			podNames := make([]string, 0, len(pods))
			for _, pod := range pods {
				podNames = append(podNames, pod.Name)
			}
			release := AllocationRelease{Pods: podNames}
			raw, err := json.Marshal(release)
			if err != nil {
				return fmt.Errorf("failed to marshal alloc-release: %v", err)
			}
			if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
				latest := &sandboxv1alpha1.BatchSandbox{}
				if err := r.Get(ctx, types.NamespacedName{Namespace: bs.Namespace, Name: bs.Name}, latest); err != nil {
					return err
				}
				patch := client.MergeFrom(latest.DeepCopy())
				if latest.Annotations == nil {
					latest.Annotations = make(map[string]string)
				}
				latest.Annotations[AnnoAllocReleaseKey] = string(raw)
				latest.Spec.Pause = nil
				return r.Patch(ctx, latest, patch)
			}); err != nil {
				return err
			}
			log.Info("Set alloc-release annotation for Pool mode pause", "pods", podNames)
		} else {
			// No pods to release, just clear pause
			if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
				latest := &sandboxv1alpha1.BatchSandbox{}
				if err := r.Get(ctx, types.NamespacedName{Namespace: bs.Namespace, Name: bs.Name}, latest); err != nil {
					return err
				}
				patch := client.MergeFrom(latest.DeepCopy())
				latest.Spec.Pause = nil
				return r.Patch(ctx, latest, patch)
			}); err != nil {
				return err
			}
		}
	} else {
		// Normal mode: delete all Pods directly
		for _, pod := range pods {
			if err := r.Delete(ctx, pod); err != nil && !errors.IsNotFound(err) {
				log.Error(err, "Failed to delete pod during pause", "pod", pod.Name)
				return err
			}
			log.Info("Deleted pod during pause", "pod", pod.Name)
		}
		// Clear spec.pause after deleting pods
		if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
			latest := &sandboxv1alpha1.BatchSandbox{}
			if err := r.Get(ctx, types.NamespacedName{Namespace: bs.Namespace, Name: bs.Name}, latest); err != nil {
				return err
			}
			patch := client.MergeFrom(latest.DeepCopy())
			latest.Spec.Pause = nil
			return r.Patch(ctx, latest, patch)
		}); err != nil {
			return err
		}
	}

	return nil
}

// continueResume continues the resume flow:
// 1. Read SandboxSnapshot status for image URIs
// 2. Replace template container images
// 3. Pool mode: clear poolRef
// 4. Scale up replicas, delete snapshot, clear pause
func (r *BatchSandboxReconciler) continueResume(ctx context.Context, bs *sandboxv1alpha1.BatchSandbox) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Get SandboxSnapshot
	snapshot := &sandboxv1alpha1.SandboxSnapshot{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: bs.Namespace, Name: bs.Name}, snapshot); err != nil {
		if errors.IsNotFound(err) {
			// Snapshot not found: rollback to Paused with ResumeFailed condition
			log.Info("SandboxSnapshot not found for resume, rolling back to Paused")
			_ = r.ackPauseWithPhase(ctx, bs, sandboxv1alpha1.BatchSandboxPhasePaused, "")
			_ = r.setCondition(ctx, bs, sandboxv1alpha1.BatchSandboxConditionResumeFailed, sandboxv1alpha1.ConditionTrue, "SnapshotNotFound", "SandboxSnapshot not found")
			_ = r.clearPause(ctx, bs)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if snapshot.Status.Phase != sandboxv1alpha1.SandboxSnapshotPhaseReady {
		msg := fmt.Sprintf("snapshot not ready: phase=%s", snapshot.Status.Phase)
		log.Error(nil, msg)
		// Rollback to Paused with ResumeFailed condition (retryable)
		_ = r.ackPauseWithPhase(ctx, bs, sandboxv1alpha1.BatchSandboxPhasePaused, "")
		_ = r.setCondition(ctx, bs, sandboxv1alpha1.BatchSandboxConditionResumeFailed, sandboxv1alpha1.ConditionTrue, "SnapshotNotReady", msg)
		_ = r.clearPause(ctx, bs)
		return ctrl.Result{}, nil
	}

	// Build image map from snapshot containers
	imageMap := make(map[string]string)
	for _, c := range snapshot.Status.Containers {
		imageMap[c.ContainerName] = c.ImageURI
	}

	// Patch: replace images, scale up, clear poolRef
	if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &sandboxv1alpha1.BatchSandbox{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: bs.Namespace, Name: bs.Name}, latest); err != nil {
			return err
		}
		patch := client.MergeFrom(latest.DeepCopy())

		// Replace container images in template
		if latest.Spec.Template != nil {
			for i := range latest.Spec.Template.Spec.Containers {
				if img, ok := imageMap[latest.Spec.Template.Spec.Containers[i].Name]; ok {
					latest.Spec.Template.Spec.Containers[i].Image = img
				}
			}
			// Add imagePullSecrets if configured
			if r.ResumePullSecret != "" {
				latest.Spec.Template.Spec.ImagePullSecrets = append(
					latest.Spec.Template.Spec.ImagePullSecrets,
					corev1.LocalObjectReference{Name: r.ResumePullSecret},
				)
			}
		}

		// Scale up
		replicas := int32(1)
		latest.Spec.Replicas = &replicas

		// Pool mode: detach from pool
		if latest.Spec.PoolRef != "" {
			latest.Spec.PoolRef = ""
		}

		return r.Patch(ctx, latest, patch)
	}); err != nil {
		return ctrl.Result{}, err
	}

	// Re-fetch to get updated spec with new replicas
	if err := r.Get(ctx, types.NamespacedName{Namespace: bs.Namespace, Name: bs.Name}, bs); err != nil {
		return ctrl.Result{}, err
	}

	// Delete SandboxSnapshot after successful resume (Pod is ready)
	// Note: Snapshot is deleted only when Pod is successfully running
	// If Pod fails, snapshot remains for retry

	// Clear pause to enter normal flow
	if err := r.clearPause(ctx, bs); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: time.Second}, nil
}

// ackPauseGeneration ACKs the current generation without changing phase.
func (r *BatchSandboxReconciler) ackPauseGeneration(ctx context.Context, bs *sandboxv1alpha1.BatchSandbox) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &sandboxv1alpha1.BatchSandbox{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: bs.Namespace, Name: bs.Name}, latest); err != nil {
			return err
		}
		latest.Status.PauseObservedGeneration = latest.Generation
		return r.Status().Update(ctx, latest)
	})
}

// ackPauseWithPhase ACKs the current generation and sets the phase.
func (r *BatchSandboxReconciler) ackPauseWithPhase(ctx context.Context, bs *sandboxv1alpha1.BatchSandbox, phase sandboxv1alpha1.BatchSandboxPhase, message string) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &sandboxv1alpha1.BatchSandbox{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: bs.Namespace, Name: bs.Name}, latest); err != nil {
			return err
		}
		latest.Status.PauseObservedGeneration = latest.Generation
		latest.Status.Phase = phase
		latest.Status.Message = message
		return r.Status().Update(ctx, latest)
	})
}

// setPauseFailed sets phase=Failed with a message.
func (r *BatchSandboxReconciler) setPauseFailed(ctx context.Context, bs *sandboxv1alpha1.BatchSandbox, message string) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &sandboxv1alpha1.BatchSandbox{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: bs.Namespace, Name: bs.Name}, latest); err != nil {
			return err
		}
		latest.Status.Phase = sandboxv1alpha1.BatchSandboxPhaseFailed
		latest.Status.Message = message
		return r.Status().Update(ctx, latest)
	})
}

// clearPause clears spec.pause to nil (idempotent recovery).
func (r *BatchSandboxReconciler) clearPause(ctx context.Context, bs *sandboxv1alpha1.BatchSandbox) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &sandboxv1alpha1.BatchSandbox{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: bs.Namespace, Name: bs.Name}, latest); err != nil {
			return err
		}
		if latest.Spec.Pause == nil {
			return nil
		}
		patch := client.MergeFrom(latest.DeepCopy())
		latest.Spec.Pause = nil
		return r.Patch(ctx, latest, patch)
	})
}

// findPodForSandbox finds the Pod associated with a BatchSandbox
func (r *BatchSandboxReconciler) findPodForSandbox(ctx context.Context, bs *sandboxv1alpha1.BatchSandbox) (*corev1.Pod, error) {
	// Use pool strategy to determine how to find pods
	poolStrategy := strategy.NewPoolStrategy(bs)
	pods, err := r.listPods(ctx, poolStrategy, bs)
	if err != nil {
		return nil, err
	}
	if len(pods) == 0 {
		return nil, fmt.Errorf("no pods found for BatchSandbox %s/%s", bs.Namespace, bs.Name)
	}
	// Return the first pod (for single-replica sandboxes)
	return pods[0], nil
}

// setCondition sets or clears a condition on the BatchSandbox status
func (r *BatchSandboxReconciler) setCondition(
	ctx context.Context,
	bs *sandboxv1alpha1.BatchSandbox,
	conditionType sandboxv1alpha1.BatchSandboxConditionType,
	status string,
	reason string,
	message string,
) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &sandboxv1alpha1.BatchSandbox{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: bs.Namespace, Name: bs.Name}, latest); err != nil {
			return err
		}

		var conditions []sandboxv1alpha1.BatchSandboxCondition
		found := false
		for _, c := range latest.Status.Conditions {
			if c.Type == conditionType {
				if status == sandboxv1alpha1.ConditionFalse {
					// Remove condition (clear failure mark)
					continue
				}
				// Update existing condition
				c.Status = status
				c.Reason = reason
				c.Message = message
				c.LastTransitionTime = ptr.To(metav1.Now())
				found = true
			}
			conditions = append(conditions, c)
		}

		// Add new condition
		if !found && status == sandboxv1alpha1.ConditionTrue {
			conditions = append(conditions, sandboxv1alpha1.BatchSandboxCondition{
				Type:               conditionType,
				Status:             status,
				Reason:             reason,
				Message:            message,
				LastTransitionTime: ptr.To(metav1.Now()),
			})
		}

		latest.Status.Conditions = conditions
		return r.Status().Update(ctx, latest)
	})
}

// isPodFailed checks if Pod is in a failed state (CrashLoopBackOff, ImagePullBackOff, etc.)
func isPodFailed(pod *corev1.Pod) bool {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Waiting != nil {
			switch cs.State.Waiting.Reason {
			case "CrashLoopBackOff", "ImagePullBackOff", "ErrImagePull", "CreateContainerConfigError":
				return true
			}
		}
	}
	return false
}

// getPodFailureMessage returns a human-readable message for Pod failure
func getPodFailureMessage(pod *corev1.Pod) string {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Waiting != nil {
			switch cs.State.Waiting.Reason {
			case "CrashLoopBackOff", "ImagePullBackOff", "ErrImagePull", "CreateContainerConfigError":
				return fmt.Sprintf("Pod %s: %s - %s", pod.Name, cs.State.Waiting.Reason, cs.State.Waiting.Message)
			}
		}
	}
	return fmt.Sprintf("Pod %s failed", pod.Name)
}

// SetupWithManager sets up the controller with the Manager.
func (r *BatchSandboxReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&sandboxv1alpha1.BatchSandbox{}).
		Named("batchsandbox").
		Owns(&corev1.Pod{}).
		Owns(&sandboxv1alpha1.SandboxSnapshot{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 32}).
		Complete(r)
}
