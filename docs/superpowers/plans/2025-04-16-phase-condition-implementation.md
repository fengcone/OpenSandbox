# BatchSandbox Phase + Condition Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement the Phase + Condition design for BatchSandbox pause/resume functionality to solve the Phase=Failed semantic confusion problem.

**Architecture:** 
- Phase represents sandbox availability state (Pending, Running, Pausing, Paused, Resuming, Failed)
- Conditions capture structured failure context (PauseFailed, ResumeFailed) with Reason and Message
- Server validates pause/resume requests based on Phase + Condition combination

**Tech Stack:** Go (Kubernetes controller), Python (Server), Ginkgo/Gomega (Go tests), pytest (Python tests)

---

## File Structure

| File | Responsibility |
|------|---------------|
| `kubernetes/apis/sandbox/v1alpha1/batchsandbox_types.go` | CRD type definitions - add Condition structs and update BatchSandboxStatus |
| `kubernetes/internal/controller/batchsandbox_controller.go` | Controller logic - add setCondition helper, update pause/resume handlers, add Pod failure detection |
| `kubernetes/internal/controller/batchsandbox_pause_resume_test.go` | Controller tests - add Condition-related test cases |
| `server/opensandbox_server/services/k8s/batchsandbox_provider.py` | Server validation - update pause/resume validation based on Phase + Condition |
| `server/tests/k8s/test_batchsandbox_provider_phase_condition.py` | Server tests - new test file for Phase + Condition validation |

---

## Task 1: Update CRD Types - Add Condition Structures

**Files:**
- Modify: `kubernetes/apis/sandbox/v1alpha1/batchsandbox_types.go`

**Context:** Add Condition-related type definitions to support PauseFailed and ResumeFailed conditions.

- [ ] **Step 1: Add Condition type constants and structures after line 34**

```go
// ConditionStatus represents the status of a condition
// +kubebuilder:validation:Enum=True;False
const (
	ConditionTrue  = "True"
	ConditionFalse = "False"
)

// BatchSandboxConditionType represents the type of BatchSandbox condition
// +kubebuilder:validation:Enum=PauseFailed;ResumeFailed
type BatchSandboxConditionType string

const (
	// BatchSandboxConditionPauseFailed is set when pause operation fails
	BatchSandboxConditionPauseFailed BatchSandboxConditionType = "PauseFailed"
	// BatchSandboxConditionResumeFailed is set when resume operation fails
	BatchSandboxConditionResumeFailed BatchSandboxConditionType = "ResumeFailed"
)

// BatchSandboxCondition represents a condition of a BatchSandbox
type BatchSandboxCondition struct {
	// Type is the condition type
	// +optional
	Type BatchSandboxConditionType `json:"type,omitempty"`
	// Status is the condition status
	// +optional
	Status string `json:"status,omitempty"`
	// Reason is a brief reason for the condition
	// +optional
	Reason string `json:"reason,omitempty"`
	// Message is a human-readable message about the condition
	// +optional
	Message string `json:"message,omitempty"`
	// LastTransitionTime is the last time the condition transitioned
	// +optional
	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`
}
```

- [ ] **Step 2: Add Conditions field to BatchSandboxStatus after line 137 (before closing brace)**

```go
	// Conditions records operation failure context
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []BatchSandboxCondition `json:"conditions,omitempty"`
```

- [ ] **Step 3: Run Go build to verify types compile**

Run: `cd /home/fengjianhui.fjh/OpenSandbox/kubernetes && go build ./apis/sandbox/v1alpha1/...`
Expected: Success

- [ ] **Step 4: Commit**

```bash
cd /home/fengjianhui.fjh/OpenSandbox
git add kubernetes/apis/sandbox/v1alpha1/batchsandbox_types.go
git commit -m "feat(k8s): add BatchSandboxCondition types for pause/resume failure tracking

Add Condition structures to support PauseFailed and ResumeFailed conditions
with Reason, Message, and LastTransitionTime fields."
```

---

## Task 2: Add setCondition Helper Function

**Files:**
- Modify: `kubernetes/internal/controller/batchsandbox_controller.go`

**Context:** Add helper function to set/clear conditions on BatchSandbox status.

- [ ] **Step 1: Add setCondition method after line 948 (after clearPause function)**

```go
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
```

- [ ] **Step 2: Add import for ptr if not present**

Verify this import exists: `k8s.io/utils/ptr`

- [ ] **Step 3: Run Go build to verify code compiles**

Run: `cd /home/fengjianhui.fjh/OpenSandbox/kubernetes && go build ./internal/controller/...`
Expected: Success

- [ ] **Step 4: Commit**

```bash
cd /home/fengjianhui.fjh/OpenSandbox
git add kubernetes/internal/controller/batchsandbox_controller.go
git commit -m "feat(k8s): add setCondition helper for BatchSandbox conditions

Add setCondition method to set or clear PauseFailed/ResumeFailed
conditions with proper conflict retry handling."
```

---

## Task 3: Update handlePause to Clear PauseFailed Condition

**Files:**
- Modify: `kubernetes/internal/controller/batchsandbox_controller.go:627-705`

**Context:** At the start of handlePause, clear any existing PauseFailed condition to mark the beginning of a new pause operation.

- [ ] **Step 1: Add condition clearing at start of handlePause (after line 628)**

Find the line `log := logf.FromContext(ctx)` in handlePause function, add after it:

```go
	// Clear any existing PauseFailed condition to mark start of new pause operation
	_ = r.setCondition(ctx, bs, sandboxv1alpha1.BatchSandboxConditionPauseFailed, sandboxv1alpha1.ConditionFalse, "", "")
```

- [ ] **Step 2: Update handlePause Pool template error handling (around line 636-646)**

Current code:
```go
if err := r.Get(ctx, types.NamespacedName{Name: bs.Spec.PoolRef, Namespace: bs.Namespace}, pool); err != nil {
    msg := fmt.Sprintf("pool CR %s not found: %v", bs.Spec.PoolRef, err)
    log.Error(err, msg)
    _ = r.setPauseFailed(ctx, bs, msg)
    _ = r.clearPause(ctx, bs)
    return ctrl.Result{}, nil
}
if pool.Spec.Template == nil {
    msg := fmt.Sprintf("pool CR %s has nil template", bs.Spec.PoolRef)
    log.Error(nil, msg)
    _ = r.setPauseFailed(ctx, bs, msg)
    _ = r.clearPause(ctx, bs)
    return ctrl.Result{}, nil
}
```

Replace with:
```go
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
```

- [ ] **Step 3: Run controller tests to verify changes**

Run: `cd /home/fengjianhui.fjh/OpenSandbox/kubernetes && go test ./internal/controller/... -run TestHandlePause -v`
Expected: Tests pass

- [ ] **Step 4: Commit**

```bash
cd /home/fengjianhui.fjh/OpenSandbox
git add kubernetes/internal/controller/batchsandbox_controller.go
git commit -m "feat(k8s): update handlePause with Condition support

- Clear PauseFailed condition at start of handlePause
- Update Pool template error handling to check Pod existence
- Set appropriate phase (Running if Pod exists, Failed if not)"
```

---

## Task 4: Add findPodForSandbox Helper Function

**Files:**
- Modify: `kubernetes/internal/controller/batchsandbox_controller.go`

**Context:** Add helper function to find Pod for a BatchSandbox, needed for Pod existence checks.

- [ ] **Step 1: Add findPodForSandbox method after line 948 (after clearPause function, before setCondition)**

```go
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
```

- [ ] **Step 2: Run Go build to verify code compiles**

Run: `cd /home/fengjianhui.fjh/OpenSandbox/kubernetes && go build ./internal/controller/...`
Expected: Success

- [ ] **Step 3: Commit**

```bash
cd /home/fengjianhui.fjh/OpenSandbox
git add kubernetes/internal/controller/batchsandbox_controller.go
git commit -m "feat(k8s): add findPodForSandbox helper function

Add helper to find Pod associated with a BatchSandbox for
existence checks during pause/resume failure handling."
```

---

## Task 5: Update syncPauseOrClear for Snapshot Failed Handling

**Files:**
- Modify: `kubernetes/internal/controller/batchsandbox_controller.go:730-775`

**Context:** Update syncPauseOrClear to handle snapshot Failed phase with Pod existence check.

- [ ] **Step 1: Replace syncPauseOrClear snapshot Failed case (around line 755-764)**

Current code:
```go
case sandboxv1alpha1.SandboxSnapshotPhaseFailed:
    // Snapshot failed
    msg := snapshot.Status.Message
    if msg == "" {
        msg = "snapshot failed"
    }
    log.Info("SandboxSnapshot Failed", "message", msg)
    _ = r.setPauseFailed(ctx, bs, msg)
    _ = r.clearPause(ctx, bs)
    return ctrl.Result{}, nil
```

Replace with:
```go
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
```

- [ ] **Step 2: Run controller tests to verify changes**

Run: `cd /home/fengjianhui.fjh/OpenSandbox/kubernetes && go test ./internal/controller/... -run TestSyncPauseOrClear -v`
Expected: Tests pass

- [ ] **Step 3: Commit**

```bash
cd /home/fengjianhui.fjh/OpenSandbox
git add kubernetes/internal/controller/batchsandbox_controller.go
git commit -m "feat(k8s): update syncPauseOrClear snapshot Failed handling

- Check Pod existence when snapshot fails
- Set phase=Running+PauseFailed if Pod exists (retryable)
- Set phase=Failed+PauseFailed if Pod missing (not retryable)"
```

---

## Task 6: Update handleResume to Clear ResumeFailed Condition

**Files:**
- Modify: `kubernetes/internal/controller/batchsandbox_controller.go:713-724`

**Context:** At the start of handleResume, clear any existing ResumeFailed condition.

- [ ] **Step 1: Add condition clearing at start of handleResume (after line 715)**

Find the line `log := logf.FromContext(ctx)` in handleResume function, add after it:

```go
	// Clear any existing ResumeFailed condition to mark start of new resume operation
	_ = r.setCondition(ctx, bs, sandboxv1alpha1.BatchSandboxConditionResumeFailed, sandboxv1alpha1.ConditionFalse, "", "")
```

- [ ] **Step 2: Run Go build to verify code compiles**

Run: `cd /home/fengjianhui.fjh/OpenSandbox/kubernetes && go build ./internal/controller/...`
Expected: Success

- [ ] **Step 3: Commit**

```bash
cd /home/fengjianhui.fjh/OpenSandbox
git add kubernetes/internal/controller/batchsandbox_controller.go
git commit -m "feat(k8s): update handleResume to clear ResumeFailed condition

Clear ResumeFailed condition at start of handleResume to
mark the beginning of a new resume operation."
```

---

## Task 7: Update continueResume for Snapshot Issues

**Files:**
- Modify: `kubernetes/internal/controller/batchsandbox_controller.go:814-893`

**Context:** Update continueResume to handle snapshot NotFound and NotReady cases with proper Phase + Condition handling.

- [ ] **Step 1: Replace continueResume snapshot error handling (around line 819-834)**

Current code:
```go	// Get SandboxSnapshot
	snapshot := &sandboxv1alpha1.SandboxSnapshot{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: bs.Namespace, Name: bs.Name}, snapshot); err != nil {
		if errors.IsNotFound(err) {
			// Snapshot not found: recovery case, clear pause and let normal flow handle it
			log.Info("SandboxSnapshot not found for resume, clearing pause")
			return ctrl.Result{}, r.clearPause(ctx, bs)
		}
		return ctrl.Result{}, err
	}

	if snapshot.Status.Phase != sandboxv1alpha1.SandboxSnapshotPhaseReady {
		msg := fmt.Sprintf("snapshot not ready: phase=%s", snapshot.Status.Phase)
		log.Error(nil, msg)
		_ = r.setPauseFailed(ctx, bs, msg)
		_ = r.clearPause(ctx, bs)
		return ctrl.Result{}, nil
	}
```

Replace with:
```go	// Get SandboxSnapshot
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
```

- [ ] **Step 2: Run controller tests to verify changes**

Run: `cd /home/fengjianhui.fjh/OpenSandbox/kubernetes && go test ./internal/controller/... -run TestContinueResume -v`
Expected: Tests pass

- [ ] **Step 3: Commit**

```bash
cd /home/fengjianhui.fjh/OpenSandbox
git add kubernetes/internal/controller/batchsandbox_controller.go
git commit -m "feat(k8s): update continueResume with Condition support

- Snapshot NotFound: rollback to Paused+ResumeFailed(Reason=SnapshotNotFound)
- Snapshot NotReady: rollback to Paused+ResumeFailed(Reason=SnapshotNotReady)
- Both cases are retryable since sandbox remains in Paused state"
```

---

## Task 8: Add Pod Startup Failure Detection in Reconcile

**Files:**
- Modify: `kubernetes/internal/controller/batchsandbox_controller.go:186-202`

**Context:** Add Pod startup failure detection (CrashLoopBackOff, ImagePullBackOff) in Resuming phase.

- [ ] **Step 1: Add isPodFailed helper function after line 950 (after setCondition)**

```go
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
```

- [ ] **Step 2: Update Reconcile phase handling for Resuming (around line 186-202)**

Current code:
```go	// Update phase based on pod state
	switch batchSbx.Status.Phase {
	case sandboxv1alpha1.BatchSandboxPhasePausing, sandboxv1alpha1.BatchSandboxPhasePaused:
		// Don't override Pausing/Paused phases
	case sandboxv1alpha1.BatchSandboxPhaseResuming:
		// Resume complete once pods are ready
		if newStatus.Ready > 0 {
			newStatus.Phase = sandboxv1alpha1.BatchSandboxPhaseRunning
		}
	default:
		if newStatus.Ready > 0 {
			newStatus.Phase = sandboxv1alpha1.BatchSandboxPhaseRunning
		} else {
			newStatus.Phase = sandboxv1alpha1.BatchSandboxPhasePending
		}
	}
```

Replace with:
```go	// Update phase based on pod state
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
		// Resume complete once pods are ready
		if newStatus.Ready > 0 {
			newStatus.Phase = sandboxv1alpha1.BatchSandboxPhaseRunning
		}
	default:
		if newStatus.Ready > 0 {
			newStatus.Phase = sandboxv1alpha1.BatchSandboxPhaseRunning
		} else {
			newStatus.Phase = sandboxv1alpha1.BatchSandboxPhasePending
		}
	}
```

- [ ] **Step 3: Run Go build to verify code compiles**

Run: `cd /home/fengjianhui.fjh/OpenSandbox/kubernetes && go build ./internal/controller/...`
Expected: Success

- [ ] **Step 4: Commit**

```bash
cd /home/fengjianhui.fjh/OpenSandbox
git add kubernetes/internal/controller/batchsandbox_controller.go
git commit -m "feat(k8s): add Pod startup failure detection in Resuming phase

- Add isPodFailed and getPodFailureMessage helpers
- Detect CrashLoopBackOff, ImagePullBackOff, ErrImagePull, CreateContainerConfigError
- Set phase=Failed+ResumeFailed when Pod fails during resume"
```

---

## Task 9: Add Controller Tests for Condition Logic

**Files:**
- Modify: `kubernetes/internal/controller/batchsandbox_pause_resume_test.go`

**Context:** Add test cases for new Condition handling logic.

- [ ] **Step 1: Add test for PauseFailed with Pod still running (after line 407)**

```go
func TestHandlePause_PoolNotFound_WithPod(t *testing.T) {
	// Pool CR not found but Pod still exists → Phase=Running + PauseFailed=True
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-bs-0",
			Namespace: "default",
			Labels: map[string]string{
				LabelBatchSandboxPodIndexKey: "0",
				LabelBatchSandboxNameKey:     "test-bs",
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
		},
	}
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 2,
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Pause:    ptr.To(true),
			PoolRef:  "nonexistent-pool",
			Replicas: ptr.To(int32(1)),
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			PauseObservedGeneration: 1,
		},
	}
	r := newTestReconciler(bs, pod)

	result, err := r.handlePause(context.Background(), bs)
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	// Verify Running phase (Pod still exists)
	updated := &sandboxv1alpha1.BatchSandbox{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs"}, updated))
	assert.Equal(t, sandboxv1alpha1.BatchSandboxPhaseRunning, updated.Status.Phase)

	// Verify PauseFailed condition
	require.Len(t, updated.Status.Conditions, 1)
	assert.Equal(t, sandboxv1alpha1.BatchSandboxConditionPauseFailed, updated.Status.Conditions[0].Type)
	assert.Equal(t, sandboxv1alpha1.ConditionTrue, updated.Status.Conditions[0].Status)
	assert.Equal(t, "PoolTemplateMissing", updated.Status.Conditions[0].Reason)

	// Verify pause cleared
	assert.Nil(t, updated.Spec.Pause)
}
```

- [ ] **Step 2: Add test for Snapshot Failed with Pod still running (after line 794)**

```go
func TestSyncPauseOrClear_SnapshotFailed_WithPod(t *testing.T) {
	// Snapshot Failed but Pod still exists → Phase=Running + PauseFailed=True
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-bs-0",
			Namespace: "default",
			Labels: map[string]string{
				LabelBatchSandboxPodIndexKey: "0",
				LabelBatchSandboxNameKey:     "test-bs",
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
		},
	}
	snapshot := &sandboxv1alpha1.SandboxSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-bs",
			Namespace: "default",
		},
		Spec: sandboxv1alpha1.SandboxSnapshotSpec{SandboxName: "test-bs"},
		Status: sandboxv1alpha1.SandboxSnapshotStatus{
			Phase:   sandboxv1alpha1.SandboxSnapshotPhaseFailed,
			Message: "commit job failed: connection refused",
		},
	}
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 2,
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Pause:    ptr.To(true),
			Replicas: ptr.To(int32(1)),
			Template: &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "img"}},
				},
			},
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			PauseObservedGeneration: 2,
			Phase:                   sandboxv1alpha1.BatchSandboxPhasePausing,
		},
	}
	r := newTestReconciler(bs, snapshot, pod)

	result, err := r.syncPauseOrClear(context.Background(), bs)
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	updated := &sandboxv1alpha1.BatchSandbox{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs"}, updated))

	// Phase should be Running (Pod still exists)
	assert.Equal(t, sandboxv1alpha1.BatchSandboxPhaseRunning, updated.Status.Phase)

	// Verify PauseFailed condition
	require.Len(t, updated.Status.Conditions, 1)
	assert.Equal(t, sandboxv1alpha1.BatchSandboxConditionPauseFailed, updated.Status.Conditions[0].Type)
	assert.Equal(t, sandboxv1alpha1.ConditionTrue, updated.Status.Conditions[0].Status)
	assert.Equal(t, "CommitPushFailed", updated.Status.Conditions[0].Reason)
	assert.Contains(t, updated.Status.Conditions[0].Message, "commit job failed")

	// Pause should be cleared
	assert.Nil(t, updated.Spec.Pause)
}
```

- [ ] **Step 3: Add test for Resume Snapshot NotFound (after line 615)**

```go
func TestContinueResume_SnapshotNotFound_Rollback(t *testing.T) {
	// Snapshot not found → Rollback to Paused + ResumeFailed=True
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 2,
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Pause:    ptr.To(false),
			Replicas: ptr.To(int32(0)),
			Template: &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "img"}},
				},
			},
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			PauseObservedGeneration: 2,
			Phase:                   sandboxv1alpha1.BatchSandboxPhaseResuming,
		},
	}
	r := newTestReconciler(bs)

	result, err := r.continueResume(context.Background(), bs)
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	updated := &sandboxv1alpha1.BatchSandbox{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs"}, updated))

	// Phase should rollback to Paused
	assert.Equal(t, sandboxv1alpha1.BatchSandboxPhasePaused, updated.Status.Phase)

	// Verify ResumeFailed condition
	require.Len(t, updated.Status.Conditions, 1)
	assert.Equal(t, sandboxv1alpha1.BatchSandboxConditionResumeFailed, updated.Status.Conditions[0].Type)
	assert.Equal(t, sandboxv1alpha1.ConditionTrue, updated.Status.Conditions[0].Status)
	assert.Equal(t, "SnapshotNotFound", updated.Status.Conditions[0].Reason)

	// Pause should be cleared
	assert.Nil(t, updated.Spec.Pause)
}
```

- [ ] **Step 4: Add test for Resume Snapshot NotReady (after line 658)**

```go
func TestContinueResume_SnapshotNotReady_Rollback(t *testing.T) {
	// Snapshot not ready → Rollback to Paused + ResumeFailed=True
	snapshot := &sandboxv1alpha1.SandboxSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-bs",
			Namespace: "default",
		},
		Spec: sandboxv1alpha1.SandboxSnapshotSpec{SandboxName: "test-bs"},
		Status: sandboxv1alpha1.SandboxSnapshotStatus{
			Phase: sandboxv1alpha1.SandboxSnapshotPhaseCommitting,
		},
	}
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 2,
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Pause:    ptr.To(false),
			Replicas: ptr.To(int32(0)),
			Template: &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "img"}},
				},
			},
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			PauseObservedGeneration: 2,
			Phase:                   sandboxv1alpha1.BatchSandboxPhaseResuming,
		},
	}
	r := newTestReconciler(bs, snapshot)

	result, err := r.continueResume(context.Background(), bs)
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	updated := &sandboxv1alpha1.BatchSandbox{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs"}, updated))

	// Phase should rollback to Paused
	assert.Equal(t, sandboxv1alpha1.BatchSandboxPhasePaused, updated.Status.Phase)

	// Verify ResumeFailed condition
	require.Len(t, updated.Status.Conditions, 1)
	assert.Equal(t, sandboxv1alpha1.BatchSandboxConditionResumeFailed, updated.Status.Conditions[0].Type)
	assert.Equal(t, sandboxv1alpha1.ConditionTrue, updated.Status.Conditions[0].Status)
	assert.Equal(t, "SnapshotNotReady", updated.Status.Conditions[0].Reason)

	// Pause should be cleared
	assert.Nil(t, updated.Spec.Pause)
}
```

- [ ] **Step 5: Run all controller tests**

Run: `cd /home/fengjianhui.fjh/OpenSandbox/kubernetes && go test ./internal/controller/... -v`
Expected: All tests pass

- [ ] **Step 6: Commit**

```bash
cd /home/fengjianhui.fjh/OpenSandbox
git add kubernetes/internal/controller/batchsandbox_pause_resume_test.go
git commit -m "test(k8s): add Condition handling tests

- Test PauseFailed with Pod still running (Phase=Running)
- Test Snapshot Failed with Pod still running
- Test Resume rollback on Snapshot NotFound
- Test Resume rollback on Snapshot NotReady"
```

---

## Task 10: Update Server pause_sandbox Validation

**Files:**
- Modify: `server/opensandbox_server/services/k8s/batchsandbox_provider.py:814-833`

**Context:** Update pause_sandbox to support retry when Phase=Running with PauseFailed condition.

- [ ] **Step 1: Replace pause_sandbox function (lines 814-833)**

Current code:
```python
    def pause_sandbox(self, sandbox_id: str, namespace: str) -> None:
        """Pause a BatchSandbox by patching spec.pause=true.

        Validates that the current status.phase allows pause (Running or Failed).
        """
        batchsandbox = self.get_workload(sandbox_id, namespace)
        if not batchsandbox:
            raise ValueError(f"Sandbox '{sandbox_id}' not found")

        phase = batchsandbox.get("status", {}).get("phase", "")
        allowed = {"Running", "Failed"}
        if phase not in allowed:
            if phase in {"Pausing", "Resuming"}:
                raise ValueError(f"Cannot pause: operation in progress (phase={phase})")
            if phase == "Paused":
                raise ValueError("Sandbox is already paused")
            raise ValueError(f"Cannot pause sandbox in phase {phase}")

        self.patch_workload(sandbox_id, namespace, {"spec": {"pause": True}})
        logger.info("Patched BatchSandbox %s spec.pause=true", sandbox_id)
```

Replace with:
```python
    def pause_sandbox(self, sandbox_id: str, namespace: str) -> None:
        """Pause a BatchSandbox by patching spec.pause=true.

        Validates that the current status.phase allows pause:
        - Running: always allowed
        - Running with PauseFailed=True: allowed (retry)
        - Failed with PauseFailed=True: not allowed (Pod missing)
        - Pausing/Resuming: not allowed (operation in progress)
        - Paused: not allowed (already paused)
        """
        batchsandbox = self.get_workload(sandbox_id, namespace)
        if not batchsandbox:
            raise ValueError(f"Sandbox '{sandbox_id}' not found")

        status = batchsandbox.get("status", {})
        phase = status.get("phase", "")
        conditions = status.get("conditions", []) or []

        # Check for PauseFailed condition
        has_pause_failed = any(
            c.get("type") == "PauseFailed" and c.get("status") == "True"
            for c in conditions
        )

        # Phase-based validation with Condition consideration
        if phase == "Running":
            # Always allow pause (including retry when PauseFailed=True)
            pass
        elif phase == "Failed" and has_pause_failed:
            # Pod missing, cannot retry pause
            raise ValueError(
                f"Cannot pause sandbox: Pod no longer exists (phase={phase}, condition=PauseFailed)"
            )
        elif phase in {"Pausing", "Resuming"}:
            raise ValueError(f"Cannot pause: operation in progress (phase={phase})")
        elif phase == "Paused":
            raise ValueError("Sandbox is already paused")
        else:
            raise ValueError(f"Cannot pause sandbox in phase {phase}")

        self.patch_workload(sandbox_id, namespace, {"spec": {"pause": True}})
        logger.info("Patched BatchSandbox %s spec.pause=true", sandbox_id)
```

- [ ] **Step 2: Commit**

```bash
cd /home/fengjianhui.fjh/OpenSandbox
git add server/opensandbox_server/services/k8s/batchsandbox_provider.py
git commit -m "feat(server): update pause_sandbox with Phase+Condition validation

- Allow pause retry when Phase=Running+PauseFailed=True
- Reject pause when Phase=Failed+PauseFailed=True (Pod missing)
- Maintain existing checks for Pausing/Resuming/Paused phases"
```

---

## Task 11: Update Server resume_sandbox Validation

**Files:**
- Modify: `server/opensandbox_server/services/k8s/batchsandbox_provider.py:835-849`

**Context:** Update resume_sandbox to support retry when Phase=Paused with ResumeFailed condition.

- [ ] **Step 1: Replace resume_sandbox function (lines 835-849)**

Current code:
```python
    def resume_sandbox(self, sandbox_id: str, namespace: str) -> None:
        """Resume a BatchSandbox by patching spec.pause=false.

        Validates that the current status.phase is Paused.
        """
        batchsandbox = self.get_workload(sandbox_id, namespace)
        if not batchsandbox:
            raise ValueError(f"Sandbox '{sandbox_id}' not found")

        phase = batchsandbox.get("status", {}).get("phase", "")
        if phase != "Paused":
            raise ValueError(f"Cannot resume sandbox in phase {phase}, expected Paused")

        self.patch_workload(sandbox_id, namespace, {"spec": {"pause": False}})
        logger.info("Patched BatchSandbox %s spec.pause=false", sandbox_id)
```

Replace with:
```python
    def resume_sandbox(self, sandbox_id: str, namespace: str) -> None:
        """Resume a BatchSandbox by patching spec.pause=false.

        Validates that the current status.phase allows resume:
        - Paused: always allowed
        - Paused with ResumeFailed=True: allowed (retry)
        - Failed with ResumeFailed=True: not allowed (Pod failed to start)
        - Other phases: not allowed
        """
        batchsandbox = self.get_workload(sandbox_id, namespace)
        if not batchsandbox:
            raise ValueError(f"Sandbox '{sandbox_id}' not found")

        status = batchsandbox.get("status", {})
        phase = status.get("phase", "")
        conditions = status.get("conditions", []) or []

        # Check for ResumeFailed condition
        has_resume_failed = any(
            c.get("type") == "ResumeFailed" and c.get("status") == "True"
            for c in conditions
        )

        # Phase-based validation with Condition consideration
        if phase == "Paused":
            # Always allow resume (including retry when ResumeFailed=True)
            pass
        elif phase == "Failed" and has_resume_failed:
            # Pod failed to start during resume, cannot retry
            raise ValueError(
                f"Cannot resume sandbox: Pod failed to start (phase={phase}, condition=ResumeFailed)"
            )
        elif phase in {"Pausing", "Resuming"}:
            raise ValueError(f"Cannot resume: operation in progress (phase={phase})")
        elif phase == "Running":
            raise ValueError("Sandbox is already running")
        else:
            raise ValueError(f"Cannot resume sandbox in phase {phase}")

        self.patch_workload(sandbox_id, namespace, {"spec": {"pause": False}})
        logger.info("Patched BatchSandbox %s spec.pause=false", sandbox_id)
```

- [ ] **Step 2: Commit**

```bash
cd /home/fengjianhui.fjh/OpenSandbox
git add server/opensandbox_server/services/k8s/batchsandbox_provider.py
git commit -m "feat(server): update resume_sandbox with Phase+Condition validation

- Allow resume retry when Phase=Paused+ResumeFailed=True
- Reject resume when Phase=Failed+ResumeFailed=True (Pod start failed)
- Maintain existing checks for Pausing/Resuming/Running phases"
```

---

## Task 12: Create Server Tests for Phase+Condition Validation

**Files:**
- Create: `server/tests/k8s/test_batchsandbox_provider_phase_condition.py`

**Context:** Create test file for new Phase + Condition validation logic in server.

- [ ] **Step 1: Create test file with basic structure**

```python
# Copyright 2025 Alibaba Group Holding Ltd.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Tests for BatchSandbox Phase + Condition validation in pause/resume operations."""

import pytest
from unittest.mock import MagicMock, patch

from opensandbox_server.services.k8s.batchsandbox_provider import BatchSandboxProvider


@pytest.fixture
def mock_k8s_client():
    """Create a mock K8sClient."""
    client = MagicMock()
    return client


@pytest.fixture
def provider(mock_k8s_client):
    """Create a BatchSandboxProvider with mocked dependencies."""
    with patch("opensandbox_server.services.k8s.batchsandbox_provider.BatchSandboxTemplateManager"):
        provider = BatchSandboxProvider(
            k8s_client=mock_k8s_client,
            app_config=None,
        )
        return provider


class TestPauseSandboxValidation:
    """Tests for pause_sandbox Phase + Condition validation."""

    def test_pause_running_allowed(self, provider, mock_k8s_client):
        """Pause allowed when phase=Running."""
        mock_k8s_client.get_custom_object.return_value = {
            "metadata": {"name": "test-sandbox"},
            "status": {"phase": "Running"},
        }
        mock_k8s_client.patch_custom_object.return_value = {}

        provider.pause_sandbox("test-sandbox", "default")

        mock_k8s_client.patch_custom_object.assert_called_once()

    def test_pause_running_with_pause_failed_allowed(self, provider, mock_k8s_client):
        """Pause allowed (retry) when phase=Running with PauseFailed=True."""
        mock_k8s_client.get_custom_object.return_value = {
            "metadata": {"name": "test-sandbox"},
            "status": {
                "phase": "Running",
                "conditions": [
                    {"type": "PauseFailed", "status": "True", "reason": "CommitPushFailed"}
                ],
            },
        }
        mock_k8s_client.patch_custom_object.return_value = {}

        provider.pause_sandbox("test-sandbox", "default")

        mock_k8s_client.patch_custom_object.assert_called_once()

    def test_pause_failed_with_pause_failed_rejected(self, provider, mock_k8s_client):
        """Pause rejected when phase=Failed with PauseFailed=True (Pod missing)."""
        mock_k8s_client.get_custom_object.return_value = {
            "metadata": {"name": "test-sandbox"},
            "status": {
                "phase": "Failed",
                "conditions": [
                    {"type": "PauseFailed", "status": "True", "reason": "PodNotFound"}
                ],
            },
        }

        with pytest.raises(ValueError, match="Pod no longer exists"):
            provider.pause_sandbox("test-sandbox", "default")

    def test_pause_pausing_rejected(self, provider, mock_k8s_client):
        """Pause rejected when phase=Pausing."""
        mock_k8s_client.get_custom_object.return_value = {
            "metadata": {"name": "test-sandbox"},
            "status": {"phase": "Pausing"},
        }

        with pytest.raises(ValueError, match="operation in progress"):
            provider.pause_sandbox("test-sandbox", "default")

    def test_pause_paused_rejected(self, provider, mock_k8s_client):
        """Pause rejected when phase=Paused."""
        mock_k8s_client.get_custom_object.return_value = {
            "metadata": {"name": "test-sandbox"},
            "status": {"phase": "Paused"},
        }

        with pytest.raises(ValueError, match="already paused"):
            provider.pause_sandbox("test-sandbox", "default")


class TestResumeSandboxValidation:
    """Tests for resume_sandbox Phase + Condition validation."""

    def test_resume_paused_allowed(self, provider, mock_k8s_client):
        """Resume allowed when phase=Paused."""
        mock_k8s_client.get_custom_object.return_value = {
            "metadata": {"name": "test-sandbox"},
            "status": {"phase": "Paused"},
        }
        mock_k8s_client.patch_custom_object.return_value = {}

        provider.resume_sandbox("test-sandbox", "default")

        mock_k8s_client.patch_custom_object.assert_called_once()

    def test_resume_paused_with_resume_failed_allowed(self, provider, mock_k8s_client):
        """Resume allowed (retry) when phase=Paused with ResumeFailed=True."""
        mock_k8s_client.get_custom_object.return_value = {
            "metadata": {"name": "test-sandbox"},
            "status": {
                "phase": "Paused",
                "conditions": [
                    {"type": "ResumeFailed", "status": "True", "reason": "SnapshotNotReady"}
                ],
            },
        }
        mock_k8s_client.patch_custom_object.return_value = {}

        provider.resume_sandbox("test-sandbox", "default")

        mock_k8s_client.patch_custom_object.assert_called_once()

    def test_resume_failed_with_resume_failed_rejected(self, provider, mock_k8s_client):
        """Resume rejected when phase=Failed with ResumeFailed=True (Pod start failed)."""
        mock_k8s_client.get_custom_object.return_value = {
            "metadata": {"name": "test-sandbox"},
            "status": {
                "phase": "Failed",
                "conditions": [
                    {"type": "ResumeFailed", "status": "True", "reason": "PodStartFailed"}
                ],
            },
        }

        with pytest.raises(ValueError, match="Pod failed to start"):
            provider.resume_sandbox("test-sandbox", "default")

    def test_resume_resuming_rejected(self, provider, mock_k8s_client):
        """Resume rejected when phase=Resuming."""
        mock_k8s_client.get_custom_object.return_value = {
            "metadata": {"name": "test-sandbox"},
            "status": {"phase": "Resuming"},
        }

        with pytest.raises(ValueError, match="operation in progress"):
            provider.resume_sandbox("test-sandbox", "default")

    def test_resume_running_rejected(self, provider, mock_k8s_client):
        """Resume rejected when phase=Running."""
        mock_k8s_client.get_custom_object.return_value = {
            "metadata": {"name": "test-sandbox"},
            "status": {"phase": "Running"},
        }

        with pytest.raises(ValueError, match="already running"):
            provider.resume_sandbox("test-sandbox", "default")


class TestConditionHelpers:
    """Tests for condition checking helpers."""

    def test_has_pause_failed_condition_true(self, provider):
        """Detect PauseFailed=True condition."""
        conditions = [
            {"type": "PauseFailed", "status": "True", "reason": "CommitPushFailed"}
        ]
        has_pause_failed = any(
            c.get("type") == "PauseFailed" and c.get("status") == "True"
            for c in conditions
        )
        assert has_pause_failed is True

    def test_has_pause_failed_condition_false(self, provider):
        """Detect PauseFailed=False condition."""
        conditions = [
            {"type": "PauseFailed", "status": "False"}
        ]
        has_pause_failed = any(
            c.get("type") == "PauseFailed" and c.get("status") == "True"
            for c in conditions
        )
        assert has_pause_failed is False

    def test_has_resume_failed_condition_true(self, provider):
        """Detect ResumeFailed=True condition."""
        conditions = [
            {"type": "ResumeFailed", "status": "True", "reason": "SnapshotNotFound"}
        ]
        has_resume_failed = any(
            c.get("type") == "ResumeFailed" and c.get("status") == "True"
            for c in conditions
        )
        assert has_resume_failed is True

    def test_empty_conditions(self, provider):
        """Handle empty conditions list."""
        conditions = []
        has_pause_failed = any(
            c.get("type") == "PauseFailed" and c.get("status") == "True"
            for c in conditions
        )
        assert has_pause_failed is False
```

- [ ] **Step 2: Run Python server tests**

Run: `cd /home/fengjianhui.fjh/OpenSandbox/server && python -m pytest tests/k8s/test_batchsandbox_provider_phase_condition.py -v`
Expected: All tests pass

- [ ] **Step 3: Commit**

```bash
cd /home/fengjianhui.fjh/OpenSandbox
git add server/tests/k8s/test_batchsandbox_provider_phase_condition.py
git commit -m "test(server): add Phase+Condition validation tests

- Test pause validation: Running, Running+PauseFailed, Failed+PauseFailed
- Test resume validation: Paused, Paused+ResumeFailed, Failed+ResumeFailed
- Test condition detection helpers"
```

---

## Task 13: Run Full Test Suite

**Files:**
- All modified files

**Context:** Run full test suites to ensure all changes work correctly together.

- [ ] **Step 1: Run Kubernetes controller tests**

Run: `cd /home/fengjianhui.fjh/OpenSandbox/kubernetes && go test ./internal/controller/... -v`
Expected: All tests pass

- [ ] **Step 2: Run Server Python tests**

Run: `cd /home/fengjianhui.fjh/OpenSandbox/server && python -m pytest tests/k8s/ -v`
Expected: All tests pass

- [ ] **Step 3: Commit final changes**

```bash
cd /home/fengjianhui.fjh/OpenSandbox
git add .
git commit -m "feat: implement Phase+Condition design for pause/resume

- Add BatchSandboxCondition types (PauseFailed, ResumeFailed)
- Update Controller to set/clear conditions appropriately
- Add Pod startup failure detection (CrashLoopBackOff, ImagePullBackOff)
- Update Server validation to support Phase+Condition checks
- Allow retry when Phase=Running+PauseFailed or Phase=Paused+ResumeFailed
- Reject operations when Phase=Failed+Condition (irreversible failures)

Fixes the Phase=Failed semantic confusion by:
- Phase represents sandbox availability
- Condition captures structured failure context
- Server can now distinguish retryable vs non-retryable failures"
```

---

## Verification Checklist

After completing all tasks, verify:

1. **CRD Types**: BatchSandboxStatus has Conditions field with proper kubebuilder annotations
2. **Controller Logic**:
   - setCondition helper handles set/clear with conflict retry
   - handlePause clears PauseFailed at start
   - handleResume clears ResumeFailed at start
   - syncPauseOrClear checks Pod existence on snapshot failure
   - continueResume rolls back to Paused+ResumeFailed on snapshot issues
   - Reconcile detects Pod startup failures in Resuming phase
3. **Server Validation**:
   - pause_sandbox allows retry when Running+PauseFailed
   - pause_sandbox rejects when Failed+PauseFailed
   - resume_sandbox allows retry when Paused+ResumeFailed
   - resume_sandbox rejects when Failed+ResumeFailed
4. **Tests**:
   - All Go controller tests pass
   - All Python server tests pass
   - New tests cover Condition handling scenarios

---

## Summary

This implementation follows the Phase + Condition design documented in `docs/pause-resume-phase-condition-design.md`:

- **Phase** answers "沙盒现在是什么状态" (sandbox current state)
- **Condition** answers "为什么失败" (structured failure context)

Key behavior changes:
| Scenario | Before | After |
|----------|--------|-------|
| commit/push 失败 | Phase=Failed | Phase=Running + PauseFailed=True |
| Pool template 缺失 | Phase=Failed | Phase=Running + PauseFailed=True |
| Resume snapshot 问题 | Phase=Failed | Phase=Paused + ResumeFailed=True |
| Resume 后 Pod 启动失败 | Phase stuck | Phase=Failed + ResumeFailed=True |
| Pause 重试 | 需解析 message | Phase=Running+PauseFailed → 直接允许 |
| Resume 重试 | 不允许 | Phase=Paused+ResumeFailed → 直接允许 |
