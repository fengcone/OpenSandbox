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

package v1alpha1

import (
	"testing"
)

func TestGetPodRecyclePolicy(t *testing.T) {
	tests := []struct {
		name     string
		pool     *Pool
		expected PodRecyclePolicy
	}{
		{
			name: "default to Delete",
			pool: &Pool{
				Spec: PoolSpec{},
			},
			expected: PodRecyclePolicyDelete,
		},
		{
			name: "explicit Restart",
			pool: &Pool{
				Spec: PoolSpec{
					PodRecyclePolicy: PodRecyclePolicyRestart,
				},
			},
			expected: PodRecyclePolicyRestart,
		},
		{
			name: "explicit Delete",
			pool: &Pool{
				Spec: PoolSpec{
					PodRecyclePolicy: PodRecyclePolicyDelete,
				},
			},
			expected: PodRecyclePolicyDelete,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.pool.GetPodRecyclePolicy()
			if result != tt.expected {
				t.Errorf("GetPodRecyclePolicy() = %v, want %v", result, tt.expected)
			}
		})
	}
}
