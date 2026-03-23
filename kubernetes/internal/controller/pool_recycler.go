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
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
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
	config    *rest.Config
	timeout   time.Duration
}

// NewPodRecycler creates a new PodRecycler with the given configuration.
func NewPodRecycler(clientset *kubernetes.Clientset, client client.Client, config *rest.Config, timeout time.Duration) *PodRecycler {
	if timeout == 0 {
		timeout = DefaultRestartTimeout
	}
	return &PodRecycler{
		clientset: clientset,
		client:    client,
		config:    config,
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

// Recycle recycles the given Pod based on the specified policy.
// For Restart policy: executes "kill 1" in all containers and waits for them to become ready.
//
//	If timeout, falls back to Delete.
// For Delete policy: deletes the Pod directly.
func (r *PodRecycler) Recycle(ctx context.Context, pod *corev1.Pod, policy sandboxv1alpha1.PodRecyclePolicy) RecycleResult {
	switch policy {
	case sandboxv1alpha1.PodRecyclePolicyRestart:
		return r.restartAndDelete(ctx, pod)
	case sandboxv1alpha1.PodRecyclePolicyDelete, "":
		return r.delete(ctx, pod)
	default:
		return r.delete(ctx, pod)
	}
}

// delete deletes the Pod and returns the result.
func (r *PodRecycler) delete(ctx context.Context, pod *corev1.Pod) RecycleResult {
	err := r.client.Delete(ctx, pod)
	if err != nil && !errors.IsNotFound(err) {
		return RecycleResult{Action: "deleted", Error: err}
	}
	return RecycleResult{Action: "deleted", Error: nil}
}

// restartAndDelete executes "kill 1" in all containers, waits for them to become ready,
// and falls back to delete if timeout.
func (r *PodRecycler) restartAndDelete(ctx context.Context, pod *corev1.Pod) RecycleResult {
	log := logf.FromContext(ctx)

	// Step 1: Execute "kill 1" in all containers
	for _, container := range pod.Spec.Containers {
		if err := r.execKill(ctx, pod, container.Name); err != nil {
			log.Error(err, "Failed to exec kill in container, falling back to delete",
				"pod", pod.Name, "container", container.Name)
			return r.delete(ctx, pod)
		}
	}

	// Step 2: Wait for all containers to become ready
	ctxWithTimeout, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	if err := r.waitForContainersReady(ctxWithTimeout, pod); err != nil {
		log.Error(err, "Timeout waiting for containers to become ready, deleting Pod",
			"pod", pod.Name, "timeout", r.timeout)
		return r.delete(ctx, pod)
	}

	return RecycleResult{Action: "restarted", Error: nil}
}

// execKill executes "kill 1" in the specified container.
func (r *PodRecycler) execKill(ctx context.Context, pod *corev1.Pod, container string) error {
	cmd := []string{KillCommand, KillArg}

	req := r.clientset.CoreV1().RESTClient().
		Post().
		Resource("pods").
		Name(pod.Name).
		Namespace(pod.Namespace).
		SubResource("exec").
		Param("container", container).
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   cmd,
			Stdin:     false,
			Stdout:    true,
			Stderr:    true,
			TTY:       false,
		}, metav1.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(r.config, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("failed to create executor: %w", err)
	}

	if err := exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:  nil,
		Stdout: nil,
		Stderr: nil,
		Tty:    false,
	}); err != nil {
		return fmt.Errorf("failed to execute kill command: %w", err)
	}

	return nil
}

// waitForContainersReady waits for all containers in the pod to become ready.
// TODO: This is a stub implementation for Task 2.4
func (r *PodRecycler) waitForContainersReady(ctx context.Context, pod *corev1.Pod) error {
	return nil
}
