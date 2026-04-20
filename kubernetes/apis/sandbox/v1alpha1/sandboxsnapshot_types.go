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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:validation:Enum=Pending;Committing;Ready;Failed
// SandboxSnapshotPhase defines the phase of a snapshot.
type SandboxSnapshotPhase string

const (
	SandboxSnapshotPhasePending    SandboxSnapshotPhase = "Pending"
	SandboxSnapshotPhaseCommitting SandboxSnapshotPhase = "Committing"
	SandboxSnapshotPhaseReady      SandboxSnapshotPhase = "Ready"
	SandboxSnapshotPhaseFailed     SandboxSnapshotPhase = "Failed"
)

// ContainerSnapshot records the snapshot result for a single container.
type ContainerSnapshot struct {
	// ContainerName is the name of the container.
	ContainerName string `json:"containerName"`
	// ImageURI is the snapshot image URI for this container.
	ImageURI string `json:"imageUri"`
	// ImageDigest is the digest of the pushed snapshot image.
	// +optional
	ImageDigest string `json:"imageDigest,omitempty"`
}

// SandboxSnapshotSpec defines the desired state of SandboxSnapshot.
// Pure atomic capability: caller fills spec, Controller only reads spec.
// Registry/pushSecret/snapshotType come from Controller Manager startup params.
type SandboxSnapshotSpec struct {
	// SandboxName is the name of the target BatchSandbox (same namespace as SandboxSnapshot).
	// Controller uses this to find BatchSandbox -> find Pod -> dispatch commit Job.
	// +kubebuilder:validation:Required
	SandboxName string `json:"sandboxName"`
}

// SandboxSnapshotStatus defines the observed state of SandboxSnapshot.
// Status is written by Controller, read-only for callers.
type SandboxSnapshotStatus struct {
	// Phase indicates the current phase of the snapshot.
	Phase SandboxSnapshotPhase `json:"phase,omitempty"`

	// Message provides human-readable status information.
	Message string `json:"message,omitempty"`

	// Containers holds per-container snapshot results, filled after Ready.
	// +optional
	Containers []ContainerSnapshot `json:"containers,omitempty"`

	// ReadyAt is the timestamp when the snapshot became Ready.
	// +optional
	ReadyAt *metav1.Time `json:"readyAt,omitempty"`

	// SourcePodName is the name of the source Pod (resolved by Controller).
	// +optional
	SourcePodName string `json:"sourcePodName,omitempty"`

	// SourceNodeName is the node where the source Pod runs (for Job scheduling).
	// +optional
	SourceNodeName string `json:"sourceNodeName,omitempty"`

	// ObservedGeneration is the most recent spec generation observed by the Controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=sbxsnap
// +kubebuilder:printcolumn:name="PHASE",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="SANDBOX",type="string",JSONPath=".spec.sandboxName"
// +kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"
type SandboxSnapshot struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SandboxSnapshotSpec   `json:"spec,omitempty"`
	Status SandboxSnapshotStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SandboxSnapshotList contains a list of SandboxSnapshot.
type SandboxSnapshotList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SandboxSnapshot `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SandboxSnapshot{}, &SandboxSnapshotList{})
}
