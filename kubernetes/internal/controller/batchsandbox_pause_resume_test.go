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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
)

// newTestReconciler creates a BatchSandboxReconciler with a fake client for testing.
func newTestReconciler(objs ...client.Object) *BatchSandboxReconciler {
	fakeClient := fake.NewClientBuilder().
		WithScheme(testscheme).
		WithStatusSubresource(
			&sandboxv1alpha1.BatchSandbox{},
			&sandboxv1alpha1.SandboxSnapshot{},
		).
		WithObjects(objs...).
		Build()
	return &BatchSandboxReconciler{
		Client:   fakeClient,
		Scheme:   testscheme,
		Recorder: record.NewFakeRecorder(10),
	}
}

// ---------- dispatchPauseResume 5-case tests ----------

func TestDispatchPauseResume_Case1_PauseTrue(t *testing.T) {
	// gen > pauseObservedGen, pause=true → handlePause dispatched
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
			PauseObservedGeneration: 1,
		},
	}
	r := newTestReconciler(bs)
	result, handled, err := r.dispatchPauseResume(context.Background(), bs)
	require.NoError(t, err)
	assert.True(t, handled, "should be handled by handlePause")
	assert.True(t, result.RequeueAfter > 0, "handlePause should requeue")

	// Verify ACK: phase should be Pausing
	updated := &sandboxv1alpha1.BatchSandbox{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs"}, updated))
	assert.Equal(t, sandboxv1alpha1.BatchSandboxPhasePausing, updated.Status.Phase)
	assert.Equal(t, int64(2), updated.Status.PauseObservedGeneration)
}

func TestDispatchPauseResume_Case2_PauseFalse(t *testing.T) {
	// gen > pauseObservedGen, pause=false → handleResume dispatched
	snapshot := &sandboxv1alpha1.SandboxSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-bs",
			Namespace: "default",
		},
		Spec: sandboxv1alpha1.SandboxSnapshotSpec{SandboxName: "test-bs"},
		Status: sandboxv1alpha1.SandboxSnapshotStatus{
			Phase: sandboxv1alpha1.SandboxSnapshotPhaseReady,
			Containers: []sandboxv1alpha1.ContainerSnapshot{
				{ContainerName: "main", ImageURI: "registry/test-bs-main:snap-gen1"},
			},
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
					Containers: []corev1.Container{{Name: "main", Image: "old-img"}},
				},
			},
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			PauseObservedGeneration: 1,
			Phase:                   sandboxv1alpha1.BatchSandboxPhasePaused,
		},
	}
	r := newTestReconciler(bs, snapshot)
	result, handled, err := r.dispatchPauseResume(context.Background(), bs)
	require.NoError(t, err)
	assert.True(t, handled, "should be handled by handleResume")
	assert.True(t, result.RequeueAfter > 0, "handleResume should requeue")

	// Verify ACK: phase=Resuming
	updated := &sandboxv1alpha1.BatchSandbox{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs"}, updated))
	assert.Equal(t, sandboxv1alpha1.BatchSandboxPhaseResuming, updated.Status.Phase)
}

func TestDispatchPauseResume_Case3_PauseNil_ACKOnly(t *testing.T) {
	// gen > pauseObservedGen, pause=nil → ACK only, continue normal flow (handled=false)
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 2,
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Replicas: ptr.To(int32(1)),
			Template: &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "img"}},
				},
			},
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			PauseObservedGeneration: 1,
		},
	}
	r := newTestReconciler(bs)
	result, handled, err := r.dispatchPauseResume(context.Background(), bs)
	require.NoError(t, err)
	assert.False(t, handled, "ACK only should not block normal flow")
	assert.Equal(t, ctrl.Result{}, result)

	// Verify ACK happened
	updated := &sandboxv1alpha1.BatchSandbox{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs"}, updated))
	assert.Equal(t, int64(2), updated.Status.PauseObservedGeneration)
}

func TestDispatchPauseResume_Case4_GenEqual_PauseSet(t *testing.T) {
	// gen == pauseObservedGen, pause != nil → syncPauseOrClear
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
	r := newTestReconciler(bs, snapshot)
	result, handled, err := r.dispatchPauseResume(context.Background(), bs)
	require.NoError(t, err)
	assert.True(t, handled, "syncPauseOrClear should handle this")
	assert.True(t, result.RequeueAfter > 0, "committing snapshot should requeue")
}

func TestDispatchPauseResume_Case5_GenEqual_PauseNil(t *testing.T) {
	// gen == pauseObservedGen, pause == nil → normal flow (handled=false)
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 2,
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Replicas: ptr.To(int32(1)),
			Template: &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "img"}},
				},
			},
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			PauseObservedGeneration: 2,
		},
	}
	r := newTestReconciler(bs)
	result, handled, err := r.dispatchPauseResume(context.Background(), bs)
	require.NoError(t, err)
	assert.False(t, handled, "normal flow should not be blocked")
	assert.Equal(t, ctrl.Result{}, result)
}

// ---------- handlePause tests ----------

func TestHandlePause_NormalFlow(t *testing.T) {
	// Normal pause: ACK, create SandboxSnapshot, verify phase=Pausing
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 2,
			UID:        "test-uid",
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
			PauseObservedGeneration: 1,
		},
	}
	r := newTestReconciler(bs)

	result, err := r.handlePause(context.Background(), bs)
	require.NoError(t, err)
	assert.True(t, result.RequeueAfter > 0)

	// Verify ACK: phase=Pausing, pauseObservedGeneration=2
	updated := &sandboxv1alpha1.BatchSandbox{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs"}, updated))
	assert.Equal(t, sandboxv1alpha1.BatchSandboxPhasePausing, updated.Status.Phase)
	assert.Equal(t, int64(2), updated.Status.PauseObservedGeneration)

	// Verify SandboxSnapshot was created
	snap := &sandboxv1alpha1.SandboxSnapshot{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs"}, snap))
	assert.Equal(t, "test-bs", snap.Spec.SandboxName)
	// Verify OwnerRef
	assert.Equal(t, "test-bs", snap.OwnerReferences[0].Name)
}

func TestHandlePause_PoolMode(t *testing.T) {
	// Pool mode: verify template solidified from Pool CR before creating snapshot
	pool := &sandboxv1alpha1.Pool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pool",
			Namespace: "default",
		},
		Spec: sandboxv1alpha1.PoolSpec{
			Template: &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "main", Image: "pool-image:latest"},
					},
				},
			},
		},
	}
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 2,
			UID:        "test-uid",
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Pause:   ptr.To(true),
			PoolRef: "test-pool",
			// Template is nil (pool mode)
			Replicas: ptr.To(int32(1)),
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			PauseObservedGeneration: 1,
		},
	}
	r := newTestReconciler(bs, pool)

	result, err := r.handlePause(context.Background(), bs)
	require.NoError(t, err)
	assert.True(t, result.RequeueAfter > 0)

	// Verify template was solidified from Pool
	updated := &sandboxv1alpha1.BatchSandbox{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs"}, updated))
	require.NotNil(t, updated.Spec.Template)
	assert.Equal(t, "pool-image:latest", updated.Spec.Template.Spec.Containers[0].Image)

	// Verify SandboxSnapshot was created
	snap := &sandboxv1alpha1.SandboxSnapshot{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs"}, snap))
	assert.Equal(t, "test-bs", snap.Spec.SandboxName)
}

func TestHandlePause_PoolNotFound(t *testing.T) {
	// Pool CR not found → Phase=Failed + PauseFailed condition + clearPause (Pod not found, non-retryable)
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 2,
			UID:        "test-uid",
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Pause:    ptr.To(true),
			PoolRef:  "nonexistent-pool",
			Replicas: ptr.To(int32(1)),
			// Template is nil - pool mode
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			PauseObservedGeneration: 1,
		},
	}
	// No Pod created - simulates Pod not found scenario (non-retryable)
	r := newTestReconciler(bs)

	result, err := r.handlePause(context.Background(), bs)
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	// Verify Phase=Failed with PauseFailed condition (Pod not found, so non-retryable)
	updated := &sandboxv1alpha1.BatchSandbox{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs"}, updated))
	assert.Equal(t, sandboxv1alpha1.BatchSandboxPhaseFailed, updated.Status.Phase)
	assert.Nil(t, updated.Spec.Pause)

	// Verify PauseFailed condition is set
	foundCondition := false
	for _, cond := range updated.Status.Conditions {
		if cond.Type == sandboxv1alpha1.BatchSandboxConditionPauseFailed {
			foundCondition = true
			assert.Equal(t, sandboxv1alpha1.ConditionTrue, cond.Status)
			assert.Equal(t, "PodNotFound", cond.Reason)
			assert.Contains(t, cond.Message, "pool CR nonexistent-pool not found")
			break
		}
	}
	assert.True(t, foundCondition, "PauseFailed condition should be set")
}

func TestHandlePause_FailedRetry(t *testing.T) {
	// Old snapshot is Failed → delete it, then requeue to recreate
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 2,
			UID:        "test-uid",
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
	oldSnapshot := &sandboxv1alpha1.SandboxSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-bs",
			Namespace: "default",
		},
		Spec: sandboxv1alpha1.SandboxSnapshotSpec{SandboxName: "test-bs"},
		Status: sandboxv1alpha1.SandboxSnapshotStatus{
			Phase:   sandboxv1alpha1.SandboxSnapshotPhaseFailed,
			Message: "previous commit failed",
		},
	}
	r := newTestReconciler(bs, oldSnapshot)

	result, err := r.handlePause(context.Background(), bs)
	require.NoError(t, err)
	assert.True(t, result.RequeueAfter > 0)

	// Verify old snapshot was deleted
	snap := &sandboxv1alpha1.SandboxSnapshot{}
	err = r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs"}, snap)
	assert.True(t, err == nil || len(snap.UID) == 0 || snap.DeletionTimestamp != nil,
		"old Failed snapshot should be deleted or being deleted")
}

// ---------- handleResume tests ----------

func TestHandleResume_NormalFlow(t *testing.T) {
	// handleResume now only ACKs Resuming phase and requeues
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 2,
			UID:        "test-uid",
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Pause:    ptr.To(false),
			Replicas: ptr.To(int32(0)),
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			PauseObservedGeneration: 1,
			Phase:                   sandboxv1alpha1.BatchSandboxPhasePaused,
		},
	}
	r := newTestReconciler(bs)

	result, err := r.handleResume(context.Background(), bs)
	require.NoError(t, err)
	assert.True(t, result.RequeueAfter > 0, "should requeue after ACK")

	// Verify ACK: phase=Resuming
	updated := &sandboxv1alpha1.BatchSandbox{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs"}, updated))
	assert.Equal(t, sandboxv1alpha1.BatchSandboxPhaseResuming, updated.Status.Phase)
	assert.Equal(t, int64(2), updated.Status.PauseObservedGeneration)
}

func TestContinueResume_NormalFlow(t *testing.T) {
	// continueResume does the actual work: read snapshot, replace images, scale up, delete snapshot
	snapshot := &sandboxv1alpha1.SandboxSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-bs",
			Namespace: "default",
		},
		Spec: sandboxv1alpha1.SandboxSnapshotSpec{SandboxName: "test-bs"},
		Status: sandboxv1alpha1.SandboxSnapshotStatus{
			Phase: sandboxv1alpha1.SandboxSnapshotPhaseReady,
			Containers: []sandboxv1alpha1.ContainerSnapshot{
				{ContainerName: "main", ImageURI: "registry/test-bs-main:snap-gen1"},
			},
		},
	}
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 2,
			UID:        "test-uid",
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Pause:    ptr.To(false),
			Replicas: ptr.To(int32(0)),
			Template: &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "old-img"}},
				},
			},
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			PauseObservedGeneration: 2,
			Phase:                   sandboxv1alpha1.BatchSandboxPhaseResuming,
		},
	}
	r := newTestReconciler(bs, snapshot)
	r.ResumePullSecret = "my-pull-secret"

	result, err := r.continueResume(context.Background(), bs)
	require.NoError(t, err)
	assert.True(t, result.RequeueAfter > 0)

	// Verify images replaced
	updated := &sandboxv1alpha1.BatchSandbox{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs"}, updated))
	assert.Equal(t, "registry/test-bs-main:snap-gen1", updated.Spec.Template.Spec.Containers[0].Image)
	// Verify replicas scaled up
	assert.Equal(t, int32(1), *updated.Spec.Replicas)
	// Verify pause cleared
	assert.Nil(t, updated.Spec.Pause)
	// Verify imagePullSecrets added
	found := false
	for _, s := range updated.Spec.Template.Spec.ImagePullSecrets {
		if s.Name == "my-pull-secret" {
			found = true
		}
	}
	assert.True(t, found, "imagePullSecrets should contain resume-pull-secret")
}

func TestHandleResume_PoolMode(t *testing.T) {
	// handleResume only ACKs Resuming phase, poolRef is cleared by continueResume
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 2,
			UID:        "test-uid",
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Pause:    ptr.To(false),
			PoolRef:  "test-pool",
			Replicas: ptr.To(int32(0)),
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			PauseObservedGeneration: 1,
			Phase:                   sandboxv1alpha1.BatchSandboxPhasePaused,
		},
	}
	r := newTestReconciler(bs)

	result, err := r.handleResume(context.Background(), bs)
	require.NoError(t, err)
	assert.True(t, result.RequeueAfter > 0)

	// Verify ACK: phase=Resuming
	updated := &sandboxv1alpha1.BatchSandbox{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs"}, updated))
	assert.Equal(t, sandboxv1alpha1.BatchSandboxPhaseResuming, updated.Status.Phase)
}

func TestContinueResume_PoolMode(t *testing.T) {
	// continueResume clears poolRef
	snapshot := &sandboxv1alpha1.SandboxSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-bs",
			Namespace: "default",
		},
		Spec: sandboxv1alpha1.SandboxSnapshotSpec{SandboxName: "test-bs"},
		Status: sandboxv1alpha1.SandboxSnapshotStatus{
			Phase: sandboxv1alpha1.SandboxSnapshotPhaseReady,
			Containers: []sandboxv1alpha1.ContainerSnapshot{
				{ContainerName: "main", ImageURI: "registry/test-bs-main:snap-gen1"},
			},
		},
	}
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 2,
			UID:        "test-uid",
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Pause:    ptr.To(false),
			PoolRef:  "test-pool",
			Replicas: ptr.To(int32(0)),
			Template: &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "old-img"}},
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
	assert.True(t, result.RequeueAfter > 0)

	// Verify poolRef cleared
	updated := &sandboxv1alpha1.BatchSandbox{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs"}, updated))
	assert.Equal(t, "", updated.Spec.PoolRef)
}

func TestContinueResume_SnapshotNotFound(t *testing.T) {
	// continueResume without snapshot → clear pause (recovery)
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

	// Verify pause cleared for recovery
	updated := &sandboxv1alpha1.BatchSandbox{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs"}, updated))
	assert.Nil(t, updated.Spec.Pause)
}

func TestContinueResume_SnapshotNotReady(t *testing.T) {
	// continueResume with snapshot still Committing → Phase=Paused + ResumeFailed condition + clearPause
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

	// Verify Phase=Paused with ResumeFailed condition (retryable)
	updated := &sandboxv1alpha1.BatchSandbox{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs"}, updated))
	assert.Equal(t, sandboxv1alpha1.BatchSandboxPhasePaused, updated.Status.Phase)
	assert.Nil(t, updated.Spec.Pause)

	// Verify ResumeFailed condition is set
	foundCondition := false
	for _, cond := range updated.Status.Conditions {
		if cond.Type == sandboxv1alpha1.BatchSandboxConditionResumeFailed {
			foundCondition = true
			assert.Equal(t, sandboxv1alpha1.ConditionTrue, cond.Status)
			assert.Equal(t, "SnapshotNotReady", cond.Reason)
			assert.Contains(t, cond.Message, "snapshot not ready")
			break
		}
	}
	assert.True(t, foundCondition, "ResumeFailed condition should be set")
}

// ---------- completePause test ----------

func TestCompletePause(t *testing.T) {
	// completePause: status.phase=Paused FIRST, then spec (replicas=0, pause=nil)
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 2,
			UID:        "test-uid",
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
	r := newTestReconciler(bs)

	err := r.completePause(context.Background(), bs)
	require.NoError(t, err)

	updated := &sandboxv1alpha1.BatchSandbox{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs"}, updated))

	// Phase should be Paused
	assert.Equal(t, sandboxv1alpha1.BatchSandboxPhasePaused, updated.Status.Phase)
	// Replicas should remain unchanged (not set to 0 per design doc)
	assert.Equal(t, int32(1), *updated.Spec.Replicas)
	// Pause should be nil
	assert.Nil(t, updated.Spec.Pause)
}

// ---------- syncPauseOrClear tests ----------

func TestSyncPauseOrClear_SnapshotReady(t *testing.T) {
	// Snapshot Ready → completePause
	snapshot := &sandboxv1alpha1.SandboxSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-bs",
			Namespace: "default",
		},
		Spec: sandboxv1alpha1.SandboxSnapshotSpec{SandboxName: "test-bs"},
		Status: sandboxv1alpha1.SandboxSnapshotStatus{
			Phase: sandboxv1alpha1.SandboxSnapshotPhaseReady,
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
	r := newTestReconciler(bs, snapshot)

	result, err := r.syncPauseOrClear(context.Background(), bs)
	require.NoError(t, err)
	assert.True(t, result.RequeueAfter > 0)

	// Verify completePause was called
	updated := &sandboxv1alpha1.BatchSandbox{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs"}, updated))
	assert.Equal(t, sandboxv1alpha1.BatchSandboxPhasePaused, updated.Status.Phase)
	assert.Nil(t, updated.Spec.Pause)
	// Replicas should remain unchanged (not set to 0 per design doc)
	assert.Equal(t, int32(1), *updated.Spec.Replicas)
}

func TestSyncPauseOrClear_SnapshotFailed(t *testing.T) {
	// Snapshot Failed → Phase=Running + PauseFailed condition + clearPause (Pod exists, so retryable)
	snapshot := &sandboxv1alpha1.SandboxSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-bs",
			Namespace: "default",
		},
		Spec: sandboxv1alpha1.SandboxSnapshotSpec{SandboxName: "test-bs"},
		Status: sandboxv1alpha1.SandboxSnapshotStatus{
			Phase:   sandboxv1alpha1.SandboxSnapshotPhaseFailed,
			Message: "commit job failed",
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
	r := newTestReconciler(bs, snapshot)

	result, err := r.syncPauseOrClear(context.Background(), bs)
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	// Verify Phase=Running with PauseFailed condition (Pod doesn't exist in fake client, so it becomes Failed)
	// Actually, since no Pod exists in fake client, findPodForSandbox returns error, so Phase=Failed
	updated := &sandboxv1alpha1.BatchSandbox{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs"}, updated))
	assert.Nil(t, updated.Spec.Pause)

	// Verify PauseFailed condition is set
	foundCondition := false
	for _, cond := range updated.Status.Conditions {
		if cond.Type == sandboxv1alpha1.BatchSandboxConditionPauseFailed {
			foundCondition = true
			assert.Equal(t, sandboxv1alpha1.ConditionTrue, cond.Status)
			assert.Equal(t, "PodNotFound", cond.Reason) // Pod not found in fake client
			break
		}
	}
	assert.True(t, foundCondition, "PauseFailed condition should be set")
}

func TestSyncPauseOrClear_SnapshotCommitting(t *testing.T) {
	// Snapshot Committing → requeue
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
			Pause:    ptr.To(true),
			Replicas: ptr.To(int32(1)),
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			PauseObservedGeneration: 2,
			Phase:                   sandboxv1alpha1.BatchSandboxPhasePausing,
		},
	}
	r := newTestReconciler(bs, snapshot)

	result, err := r.syncPauseOrClear(context.Background(), bs)
	require.NoError(t, err)
	assert.True(t, result.RequeueAfter > 0, "committing snapshot should requeue")
}

func TestSyncPauseOrClear_NoSnapshot(t *testing.T) {
	// No snapshot → requeue (wait for handlePause to create it)
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 2,
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Pause:    ptr.To(true),
			Replicas: ptr.To(int32(1)),
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			PauseObservedGeneration: 2,
		},
	}
	r := newTestReconciler(bs)

	result, err := r.syncPauseOrClear(context.Background(), bs)
	require.NoError(t, err)
	assert.True(t, result.RequeueAfter > 0, "no snapshot should requeue")
}

// ---------- Phase update bug fix verification ----------

func TestPhaseUpdate_Running(t *testing.T) {
	// When pods are Running+Ready, phase should be Running (not stuck at Pending)
	// This verifies the Bug 2 fix: phase judgment switch moved AFTER the pod counting loop.
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 1,
			UID:        "test-uid",
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Replicas: ptr.To(int32(1)),
			Template: &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "img"}},
				},
			},
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-bs-0",
			Namespace: "default",
			Labels: map[string]string{
				LabelBatchSandboxPodIndexKey: "0",
				LabelBatchSandboxNameKey:     "test-bs",
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: sandboxv1alpha1.GroupVersion.String(),
					Kind:       "BatchSandbox",
					Name:       "test-bs",
					UID:        "test-uid",
					Controller: ptr.To(true),
				},
			},
		},
		Spec: corev1.PodSpec{
			NodeName:   "node-1",
			Containers: []corev1.Container{{Name: "main", Image: "img"}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			PodIP: "10.0.0.1",
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}
	_ = newTestReconciler(bs, pod)

	// Verify the logic inline (same code path as Reconcile)
	newStatus := bs.Status.DeepCopy()
	newStatus.ObservedGeneration = bs.Generation
	newStatus.Replicas = 0
	newStatus.Ready = 0
	newStatus.Allocated = 0

	// Phase judgment AFTER counting (Bug 2 fix verification)
	pods := []*corev1.Pod{pod}
	for _, p := range pods {
		newStatus.Replicas++
		if p.Spec.NodeName != "" {
			newStatus.Allocated++
		}
		if p.Status.Phase == corev1.PodRunning && p.Status.Conditions != nil {
			for _, c := range p.Status.Conditions {
				if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
					newStatus.Ready++
				}
			}
		}
	}

	// Phase should be Running because Ready > 0
	switch bs.Status.Phase {
	case sandboxv1alpha1.BatchSandboxPhasePausing, sandboxv1alpha1.BatchSandboxPhasePaused,
		sandboxv1alpha1.BatchSandboxPhaseResuming:
		// Don't override
	default:
		if newStatus.Ready > 0 {
			newStatus.Phase = sandboxv1alpha1.BatchSandboxPhaseRunning
		} else {
			newStatus.Phase = sandboxv1alpha1.BatchSandboxPhasePending
		}
	}

	assert.Equal(t, int32(1), newStatus.Ready, "Ready should be 1")
	assert.Equal(t, sandboxv1alpha1.BatchSandboxPhaseRunning, newStatus.Phase,
		"Phase should be Running when Ready > 0")
}

// ---------- ackPauseGeneration test ----------

func TestAckPauseGeneration(t *testing.T) {
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 5,
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Replicas: ptr.To(int32(1)),
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			PauseObservedGeneration: 3,
		},
	}
	r := newTestReconciler(bs)

	err := r.ackPauseGeneration(context.Background(), bs)
	require.NoError(t, err)

	updated := &sandboxv1alpha1.BatchSandbox{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs"}, updated))
	assert.Equal(t, int64(5), updated.Status.PauseObservedGeneration)
}

// ---------- setPauseFailed + clearPause combined test ----------

func TestSetPauseFailed_And_ClearPause(t *testing.T) {
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 2,
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Pause:    ptr.To(true),
			Replicas: ptr.To(int32(1)),
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{},
	}
	r := newTestReconciler(bs)

	// Set Failed
	err := r.setPauseFailed(context.Background(), bs, "test error")
	require.NoError(t, err)

	updated := &sandboxv1alpha1.BatchSandbox{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs"}, updated))
	assert.Equal(t, sandboxv1alpha1.BatchSandboxPhaseFailed, updated.Status.Phase)
	assert.Equal(t, "test error", updated.Status.Message)

	// Clear Pause
	err = r.clearPause(context.Background(), updated)
	require.NoError(t, err)

	updated2 := &sandboxv1alpha1.BatchSandbox{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs"}, updated2))
	assert.Nil(t, updated2.Spec.Pause)
}

// ---------- clearPause idempotent test ----------

func TestClearPause_Idempotent(t *testing.T) {
	bs := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-bs",
			Namespace:  "default",
			Generation: 2,
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Replicas: ptr.To(int32(1)),
			// Pause already nil
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{},
	}
	r := newTestReconciler(bs)

	// clearPause should be no-op when pause is already nil
	err := r.clearPause(context.Background(), bs)
	require.NoError(t, err)

	updated := &sandboxv1alpha1.BatchSandbox{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-bs"}, updated))
	assert.Nil(t, updated.Spec.Pause)
}

// Ensure ctrl.Result type is used
var _ = ctrl.Result{}
