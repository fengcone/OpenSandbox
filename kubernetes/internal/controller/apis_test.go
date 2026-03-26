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
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestCanAllocate(t *testing.T) {
	tests := []struct {
		name     string
		pod      *corev1.Pod
		expected bool
	}{
		{
			name: "normal pod without labels",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "pod-normal",
				},
			},
			expected: true,
		},
		{
			name: "pod with deallocated-from but no recycle-confirmed",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "pod-deallocated",
					Labels: map[string]string{
						LabelPodDeallocatedFrom: "bsx-uid-123",
					},
				},
			},
			expected: false,
		},
		{
			name: "pod with deallocated-from and recycle-confirmed, no recycle-meta",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "pod-confirmed",
					Labels: map[string]string{
						LabelPodDeallocatedFrom:  "bsx-uid-123",
						LabelPodRecycleConfirmed: "bsx-uid-123",
					},
				},
			},
			expected: true,
		},
		{
			name: "pod with deallocated-from and recycle-confirmed and recycle-meta (still restarting)",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "pod-restarting",
					Labels: map[string]string{
						LabelPodDeallocatedFrom:  "bsx-uid-123",
						LabelPodRecycleConfirmed: "bsx-uid-123",
					},
					Annotations: map[string]string{
						AnnoPodRecycleMeta: `{"state":"Restarting","triggeredAt":1234567890}`,
					},
				},
			},
			expected: false,
		},
		{
			name: "pod with only recycle-confirmed (edge case)",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "pod-only-confirmed",
					Labels: map[string]string{
						LabelPodRecycleConfirmed: "bsx-uid-123",
					},
				},
			},
			expected: true, // No deallocated-from means normal pod
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := canAllocate(tt.pod)
			assert.Equal(t, tt.expected, result)
		})
	}
}
