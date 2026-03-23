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
	"time"

	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// DefaultRestartTimeout is the default timeout for waiting containers to become ready after restart
	DefaultRestartTimeout = 60 * time.Second
	// KillCommand is the command executed to restart containers
	KillCommand = "kill"
	KillArg     = "1"
)

// PodRecycler is responsible for recycling Pods based on the specified policy.
type PodRecycler struct {
	clientset *kubernetes.Clientset
	client    client.Client
	timeout   time.Duration
}

// NewPodRecycler creates a new PodRecycler with the given configuration.
func NewPodRecycler(clientset *kubernetes.Clientset, client client.Client, timeout time.Duration) *PodRecycler {
	if timeout == 0 {
		timeout = DefaultRestartTimeout
	}
	return &PodRecycler{
		clientset: clientset,
		client:    client,
		timeout:   timeout,
	}
}

// RecycleResult contains the result of a recycle operation.
type RecycleResult struct {
	// Action is the action taken: "deleted", "restarted", or "failed_then_deleted"
	Action string
	// Error is the error encountered, if any
	Error error
}
