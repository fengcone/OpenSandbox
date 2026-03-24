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
	"time"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestPodRecycleMetaSerDe(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-pod",
		},
	}
	meta := &PodRecycleMeta{
		State:       RecycleStateRestarting,
		TriggeredAt: 123456789,
	}

	setPodRecycleMeta(pod, meta)
	assert.Contains(t, pod.Annotations[AnnoPodRecycleMeta], "Restarting")

	parsed, err := parsePodRecycleMeta(pod)
	assert.NoError(t, err)
	assert.Equal(t, meta.State, parsed.State)
	assert.Equal(t, meta.TriggeredAt, parsed.TriggeredAt)
}

func TestRestartTracker_IsRestarting(t *testing.T) {
	cases := []struct {
		state    PodRecycleState
		expected bool
	}{
		{RecycleStateNone, false},
		{RecycleStateRestarting, true},
	}

	for _, c := range cases {
		pod := &corev1.Pod{}
		setPodRecycleMeta(pod, &PodRecycleMeta{State: c.state, TriggeredAt: 100})
		assert.Equal(t, c.expected, isRestarting(pod), "State: %s", c.state)
	}
}

func TestRestartTracker_CheckRestartStatus_Ready(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod1",
			Namespace: "default",
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name:         "c1",
					Ready:        true,
					RestartCount: 1,
				},
			},
		},
	}

	meta := &PodRecycleMeta{
		State:       RecycleStateRestarting,
		TriggeredAt: time.Now().UnixMilli() - 2000,
		InitialRestartCounts: map[string]int32{
			"c1": 0,
		},
	}
	setPodRecycleMeta(pod, meta)

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	tracker := &restartTracker{
		client: client,
	}

	err := tracker.checkRestartStatus(context.Background(), pod)
	assert.NoError(t, err)

	// Verify annotation is cleared (restart completed)
	updatedPod := &corev1.Pod{}
	err = client.Get(context.Background(), types.NamespacedName{Name: "pod1", Namespace: "default"}, updatedPod)
	assert.NoError(t, err)
	_, exists := updatedPod.Annotations[AnnoPodRecycleMeta]
	assert.False(t, exists, "annotation should be cleared after restart completed")
}

func TestRestartTracker_CheckRestartStatus_Timeout(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod1",
			Namespace: "default",
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name:         "c1",
					RestartCount: 0, // Not restarted
				},
			},
		},
	}

	meta := &PodRecycleMeta{
		State:       RecycleStateRestarting,
		TriggeredAt: time.Now().UnixMilli() - (restartTimeout.Milliseconds() + 1000),
		InitialRestartCounts: map[string]int32{
			"c1": 0,
		},
	}
	setPodRecycleMeta(pod, meta)

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	tracker := &restartTracker{
		client: client,
	}

	err := tracker.checkRestartStatus(context.Background(), pod)
	assert.NoError(t, err)

	// Verify pod is deleted
	updatedPod := &corev1.Pod{}
	err = client.Get(context.Background(), types.NamespacedName{Name: "pod1", Namespace: "default"}, updatedPod)
	assert.True(t, errors.IsNotFound(err))
}

func TestRestartTracker_CheckRestartStatus_CrashLoop(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod1",
			Namespace: "default",
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "c1",
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{
							Reason: "CrashLoopBackOff",
						},
					},
				},
			},
		},
	}

	meta := &PodRecycleMeta{
		State:       RecycleStateRestarting,
		TriggeredAt: time.Now().UnixMilli() - 1000,
	}
	setPodRecycleMeta(pod, meta)

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	tracker := &restartTracker{
		client: client,
	}

	err := tracker.checkRestartStatus(context.Background(), pod)
	assert.NoError(t, err)

	// Verify pod is deleted
	updatedPod := &corev1.Pod{}
	err = client.Get(context.Background(), types.NamespacedName{Name: "pod1", Namespace: "default"}, updatedPod)
	assert.True(t, errors.IsNotFound(err))
}

func TestRestartTracker_CheckRestartStatus_StillRestarting(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod1",
			Namespace: "default",
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name:         "c1",
					Ready:        false,
					RestartCount: 0, // Not yet restarted
				},
			},
		},
	}

	meta := &PodRecycleMeta{
		State:       RecycleStateRestarting,
		TriggeredAt: time.Now().UnixMilli() - 5000, // Only 5s ago, within timeout
		InitialRestartCounts: map[string]int32{
			"c1": 0,
		},
	}
	setPodRecycleMeta(pod, meta)

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	tracker := &restartTracker{
		client: client,
	}

	err := tracker.checkRestartStatus(context.Background(), pod)
	assert.NoError(t, err)

	// Verify pod still exists and annotation is still there
	updatedPod := &corev1.Pod{}
	err = client.Get(context.Background(), types.NamespacedName{Name: "pod1", Namespace: "default"}, updatedPod)
	assert.NoError(t, err)
	_, exists := updatedPod.Annotations[AnnoPodRecycleMeta]
	assert.True(t, exists, "annotation should still be present while restarting")
}

func TestRestartTracker_HandleRestart_Initial(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod1",
			Namespace: "default",
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name:         "c1",
					RestartCount: 5,
				},
			},
		},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	tracker := &restartTracker{
		client: client,
	}

	err := tracker.HandleRestart(context.Background(), pod)
	assert.NoError(t, err)

	// Verify meta initialized
	updatedPod := &corev1.Pod{}
	err = client.Get(context.Background(), types.NamespacedName{Name: "pod1", Namespace: "default"}, updatedPod)
	assert.NoError(t, err)
	meta, err := parsePodRecycleMeta(updatedPod)
	assert.NoError(t, err)
	assert.Equal(t, RecycleStateRestarting, meta.State)
	assert.Equal(t, int32(5), meta.InitialRestartCounts["c1"])
	assert.True(t, meta.TriggeredAt > 0)
}
