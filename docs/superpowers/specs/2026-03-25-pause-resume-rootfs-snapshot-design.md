# OSEP-0008 Implementation Design: Pause and Resume via Rootfs Snapshot

**Date:** 2026-03-25
**Author:** Claude
**Status:** Draft

## Overview

This document describes the implementation design for OSEP-0008: Pause and Resume via Rootfs Snapshot. The feature allows users to pause a running sandbox by committing its root filesystem to an OCI image, and later resume it from the snapshot.

## Architecture

```
┌─────────────────────────────────────────────────────────────────────────┐
│                              Client                                      │
└─────────────────────────────────────────────────────────────────────────┘
                                    │
                                    ▼
┌─────────────────────────────────────────────────────────────────────────┐
│                         OpenSandbox Server                               │
│  ┌──────────────────────────────────────────────────────────────────┐   │
│  │  KubernetesSandboxService                                         │   │
│  │  - pause_sandbox(): Create SandboxSnapshot CR                     │   │
│  │  - resume_sandbox(): Rebuild BatchSandbox from snapshot           │   │
│  │  - get_sandbox(): Aggregate BatchSandbox + SandboxSnapshot state  │   │
│  │  - delete_sandbox(): Cleanup BatchSandbox + SandboxSnapshot       │   │
│  └──────────────────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────────────────┘
                                    │
                                    ▼
┌─────────────────────────────────────────────────────────────────────────┐
│                        Kubernetes Cluster                                │
│                                                                          │
│  ┌────────────────┐     ┌────────────────┐     ┌────────────────────┐   │
│  │ BatchSandbox   │     │ SandboxSnapshot│     │ Commit Job         │   │
│  │ (live workload)│     │ (snapshot CR)  │────▶│ (rootfs commit)    │   │
│  └────────────────┘     └────────────────┘     └────────────────────┘   │
│         │                      │                        │               │
│         │                      ▼                        ▼               │
│         │            ┌────────────────────┐    ┌─────────────────┐      │
│         │            │ SnapshotController │    │ OCI Registry    │      │
│         │            │ (watches Snapshot) │    │ (snapshot image)│      │
│         │            └────────────────────┘    └─────────────────┘      │
│         │                      │                                          │
│         ▼                      │                                          │
│  ┌────────────────┐            │                                          │
│  │ Sandbox Pod    │◀───────────┘ (same node)                              │
│  │ (replicas=1)   │                                                       │
│  └────────────────┘                                                       │
└─────────────────────────────────────────────────────────────────────────┘
```

## Design Decisions

Based on discussion with the user, the following decisions were made:

| Decision | Choice |
|----------|--------|
| Snapshot registry configuration | Combine `pausePolicy.snapshotRegistry` with server default fallback |
| Commit job tool | Configurable, default to `containerd/containerd:1.7` |
| Pause behavior | Delete BatchSandbox after snapshot Ready to release resources |
| Delete cleanup | Configurable (default: don't delete registry image) |
| Testing strategy | Mock tests for Server layer, real K8s E2E for Controller |

---

## Part 1: CRD Design

### 1.1 SandboxSnapshot CRD

**File:** `kubernetes/apis/sandbox/v1alpha1/sandboxsnapshot_types.go`

```go
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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtime "k8s.io/apimachinery/pkg/runtime"
)

// SnapshotType defines the type of snapshot.
type SnapshotType string

const (
	SnapshotTypeRootfs SnapshotType = "Rootfs"
	// Reserved for future: SnapshotTypeVM
)

// SandboxSnapshotPhase defines the phase of a snapshot.
type SandboxSnapshotPhase string

const (
	SandboxSnapshotPhasePending    SandboxSnapshotPhase = "Pending"
	SandboxSnapshotPhaseCommitting SandboxSnapshotPhase = "Committing"
	SandboxSnapshotPhasePushing    SandboxSnapshotPhase = "Pushing"
	SandboxSnapshotPhaseReady      SandboxSnapshotPhase = "Ready"
	SandboxSnapshotPhaseFailed     SandboxSnapshotPhase = "Failed"
)

// SandboxSnapshotSpec defines the desired state of SandboxSnapshot.
type SandboxSnapshotSpec struct {
	// SandboxID is the stable public identifier for the sandbox.
	SandboxID string `json:"sandboxId"`

	// SnapshotType indicates the type of snapshot (Rootfs for v1).
	SnapshotType SnapshotType `json:"snapshotType"`

	// SourceBatchSandboxName is the name of the source BatchSandbox.
	SourceBatchSandboxName string `json:"sourceBatchSandboxName"`

	// SourcePodName is the name of the source Pod.
	SourcePodName string `json:"sourcePodName"`

	// SourceContainerName is the name of the source container.
	SourceContainerName string `json:"sourceContainerName"`

	// SourceNodeName is the node where the source Pod runs.
	SourceNodeName string `json:"sourceNodeName"`

	// ImageURI is the target image URI for the snapshot.
	ImageURI string `json:"imageUri"`

	// SnapshotPushSecretName is the Secret name for pushing to registry.
	// +optional
	SnapshotPushSecretName string `json:"snapshotPushSecretName,omitempty"`

	// ResumeImagePullSecretName is the Secret name for pulling snapshot during resume.
	// +optional
	ResumeImagePullSecretName string `json:"resumeImagePullSecretName,omitempty"`

	// ResumeTemplate contains enough information to reconstruct BatchSandbox.
	// +optional
	ResumeTemplate *runtime.RawExtension `json:"resumeTemplate,omitempty"`

	// PausedAt is the timestamp when pause was initiated.
	PausedAt metav1.Time `json:"pausedAt"`
}

// SandboxSnapshotStatus defines the observed state of SandboxSnapshot.
type SandboxSnapshotStatus struct {
	// Phase indicates the current phase of the snapshot.
	Phase SandboxSnapshotPhase `json:"phase,omitempty"`

	// Message provides human-readable status information.
	Message string `json:"message,omitempty"`

	// ReadyAt is the timestamp when the snapshot became Ready.
	ReadyAt *metav1.Time `json:"readyAt,omitempty"`

	// ImageDigest is the digest of the pushed snapshot image.
	ImageDigest string `json:"imageDigest,omitempty"`
}

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=sbxsnap
// +kubebuilder:printcolumn:name="PHASE",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="SANDBOX_ID",type="string",JSONPath=".spec.sandboxId"
// +kubebuilder:printcolumn:name="IMAGE",type="string",JSONPath=".spec.imageUri"
// +kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"
type SandboxSnapshot struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SandboxSnapshotSpec   `json:"spec,omitempty"`
	Status SandboxSnapshotStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type SandboxSnapshotList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SandboxSnapshot `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SandboxSnapshot{}, &SandboxSnapshotList{})
}
```

### 1.2 BatchSandbox Extension

**File:** `kubernetes/apis/sandbox/v1alpha1/batchsandbox_types.go` (add to existing)

```go
// PausePolicy defines the policy for pause/resume operations.
type PausePolicy struct {
	// SnapshotType indicates the type of snapshot (default: Rootfs).
	// +optional
	SnapshotType string `json:"snapshotType,omitempty"`

	// SnapshotRegistry is the OCI registry for snapshot images.
	SnapshotRegistry string `json:"snapshotRegistry"`

	// SnapshotPushSecretName is the Secret name for pushing snapshots.
	// +optional
	SnapshotPushSecretName string `json:"snapshotPushSecretName,omitempty"`

	// ResumeImagePullSecretName is the Secret name for pulling snapshots during resume.
	// +optional
	ResumeImagePullSecretName string `json:"resumeImagePullSecretName,omitempty"`
}

// Add to BatchSandboxSpec:
// PausePolicy defines the pause/resume policy for this sandbox.
// +optional
PausePolicy *PausePolicy `json:"pausePolicy,omitempty"`
```

---

## Part 2: Controller Design

### 2.1 SandboxSnapshotController

**File:** `kubernetes/internal/controller/sandboxsnapshot_controller.go`

```
┌─────────────────────────────────────────────────────────────────┐
│                    SandboxSnapshotController                     │
│                                                                  │
│  Reconcile(req Request) Result:                                  │
│  ┌────────────────────────────────────────────────────────────┐ │
│  │ 1. Fetch SandboxSnapshot CR                                │ │
│  │ 2. Check phase, act accordingly:                           │ │
│  │                                                            │ │
│  │    Pending -> create Commit Job, set phase=Committing      │ │
│  │    Committing -> check Job status                          │ │
│  │      - running: requeue                                    │ │
│  │      - succeeded: set phase=Pushing (or Ready)             │ │
│  │      - failed: set phase=Failed                           │ │
│  │                                                            │ │
│  │    Pushing -> (handled inside Job, just wait)              │ │
│  │      - succeeded: set phase=Ready, record imageDigest      │ │
│  │      - failed: set phase=Failed                           │ │
│  │                                                            │ │
│  │    Ready/Failed -> no action                               │ │
│  └────────────────────────────────────────────────────────────┘ │
└─────────────────────────────────────────────────────────────────┘
```

### 2.2 Reconcile Logic

```go
func (r *SandboxSnapshotReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	snap := &sandboxv1alpha1.SandboxSnapshot{}
	if err := r.Get(ctx, req.NamespacedName, snap); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Terminal states, no action
	if snap.Status.Phase == sandboxv1alpha1.SandboxSnapshotPhaseReady ||
		snap.Status.Phase == sandboxv1alpha1.SandboxSnapshotPhaseFailed {
		return ctrl.Result{}, nil
	}

	switch snap.Status.Phase {
	case "", sandboxv1alpha1.SandboxSnapshotPhasePending:
		return r.handlePending(ctx, snap)
	case sandboxv1alpha1.SandboxSnapshotPhaseCommitting:
		return r.handleCommitting(ctx, snap)
	case sandboxv1alpha1.SandboxSnapshotPhasePushing:
		return r.handlePushing(ctx, snap)
	}

	return ctrl.Result{}, nil
}
```

### 2.3 Commit Job Pod Spec

```yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: sbxsnap-commit-{sandboxId}
  namespace: {namespace}
  ownerReferences:
    - apiVersion: sandbox.opensandbox.io/v1alpha1
      kind: SandboxSnapshot
      name: {sandboxId}
spec:
  ttlSecondsAfterFinished: 300
  template:
    spec:
      nodeName: {sourceNodeName}  # Pin to source node
      restartPolicy: Never
      containers:
        - name: committer
          image: {committer_image}  # configurable, default containerd
          command: ["/bin/sh", "-c"]
          args:
            - |
              set -e
              # Resolve container ID from pod
              CONTAINER_ID=$(crictl ps --name {sourceContainerName} --pod {sourcePodName} -q)

              # Commit container rootfs
              ctr -n k8s.io images export /tmp/rootfs.tar ${CONTAINER_ID}
              ctr -n k8s.io images import /tmp/rootfs.tar
              ctr -n k8s.io images tag ${CONTAINER_ID} {imageUri}

              # Push to registry
              ctr -n k8s.io images push {imageUri} --plain-http=false
          volumeMounts:
            - name: containerd-sock
              mountPath: /run/containerd/containerd.sock
            - name: auth
              mountPath: /var/run/auth
              readOnly: true
      volumes:
        - name: containerd-sock
          hostPath:
            path: /run/containerd/containerd.sock
            type: Socket
        - name: auth
          secret:
            secretName: {snapshotPushSecretName}
```

---

## Part 3: Server Layer Design

### 3.1 Schema Extension

**File:** `server/opensandbox_server/api/schema.py`

```python
class PausePolicy(BaseModel):
    """Configuration for pause/resume with rootfs snapshot."""
    snapshot_type: Literal["Rootfs"] = Field(
        "Rootfs",
        alias="snapshotType",
        description="Snapshot type, currently only 'Rootfs' is supported",
    )
    snapshot_registry: str = Field(
        ...,
        alias="snapshotRegistry",
        description="OCI registry for snapshot images, e.g. registry.example.com/snapshots",
    )
    snapshot_push_secret_name: Optional[str] = Field(
        None,
        alias="snapshotPushSecretName",
        description="K8s Secret name for pushing snapshot to registry",
    )
    resume_image_pull_secret_name: Optional[str] = Field(
        None,
        alias="resumeImagePullSecretName",
        description="K8s Secret name for pulling snapshot image during resume",
    )

    class Config:
        populate_by_name = True


class CreateSandboxRequest(BaseModel):
    # ... existing fields ...
    pause_policy: Optional[PausePolicy] = Field(
        None,
        alias="pausePolicy",
        description="Optional pause policy for snapshot support",
    )
```

### 3.2 Config Extension

**File:** `server/opensandbox_server/config.py`

```python
class PauseConfig(BaseModel):
    """Pause/resume configuration."""
    default_snapshot_registry: str = Field(
        "",
        description="Default registry for snapshots when pausePolicy.snapshotRegistry is not set",
    )
    committer_image: str = Field(
        "containerd/containerd:1.7",
        description="Image used by commit Job Pod for rootfs snapshot",
    )
    cleanup_snapshot_image_on_delete: bool = Field(
        False,
        description="Whether to delete snapshot image from registry when sandbox is deleted",
    )
    commit_timeout_seconds: int = Field(
        600,
        description="Timeout for commit job in seconds",
    )
```

### 3.3 KubernetesSandboxService Implementation

#### pause_sandbox

```
pause_sandbox(sandbox_id):
  1. Get BatchSandbox, validate:
     - exists and Running
     - replicas == 1
     - pausePolicy configured
     - no in-flight snapshot (phase Pending/Committing/Pushing)

  2. Get Pod info:
     - podName, containerName, nodeName
     - resolve container ID

  3. Resolve snapshot registry:
     - pausePolicy.snapshotRegistry OR config.default_snapshot_registry
     - imageUri = {registry}/{sandboxId}:snapshot

  4. Build resumeTemplate from current BatchSandbox spec

  5. Create SandboxSnapshot CR:
     - metadata.name = sandboxId
     - spec fields from above
     - status.phase = Pending

  6. Return (async, controller handles commit)
```

#### resume_sandbox

```
resume_sandbox(sandbox_id):
  1. Get SandboxSnapshot by sandboxId
  2. Validate:
     - exists
     - status.phase == Ready
     - no existing BatchSandbox (or will conflict)

  3. Reconstruct BatchSandbox from snapshot.resumeTemplate:
     - metadata.name = sandboxId
     - replicas = 1
     - template.image = snapshot.spec.imageUri
     - imagePullSecrets = resumeImagePullSecretName

  4. Create BatchSandbox
  5. Return (async, BatchSandboxController handles pod creation)
```

#### get_sandbox (State Aggregation)

```python
def _derive_sandbox_state(batchsandbox, snapshot) -> tuple[str, str, str]:
    """Derive sandbox state from both resources."""

    # Snapshot failed
    if snapshot and snapshot.get("status", {}).get("phase") == "Failed":
        return ("Failed", "SNAPSHOT_FAILED", snapshot.get("status", {}).get("message", ""))

    # Pausing (both exist, snapshot in progress)
    if batchsandbox and snapshot:
        phase = snapshot.get("status", {}).get("phase")
        if phase in ("Pending", "Committing", "Pushing"):
            return ("Pausing", f"SNAPSHOT_{phase.upper()}", f"Snapshot is {phase.lower()}")
        if phase == "Ready":
            return ("Pausing", "SNAPSHOT_READY_CLEANUP", "Releasing resources")

    # Paused (no workload, snapshot ready)
    if not batchsandbox and snapshot:
        phase = snapshot.get("status", {}).get("phase")
        if phase == "Ready":
            return ("Paused", "SNAPSHOT_READY", "Sandbox paused")
        if phase in ("Pending", "Committing", "Pushing"):
            return ("Pausing", f"SNAPSHOT_{phase.upper()}", f"Snapshot is {phase.lower()}")

    # Resuming (workload from snapshot)
    if batchsandbox:
        status = self.workload_provider.get_status(batchsandbox)
        if batchsandbox.get("metadata", {}).get("annotations", {}).get("opensandbox.io/from-snapshot") == "true":
            if status["state"] != "Running":
                return ("Resuming", "RESUMING", "Restoring from snapshot")
        return (status["state"], status["reason"], status["message"])

    return ("NotFound", "SANDBOX_NOT_FOUND", "Sandbox does not exist")
```

#### list_sandboxes (Merge Logic)

```python
def list_sandboxes():
    """
    Merge results from BatchSandboxes and SandboxSnapshots.
    Each sandboxId appears exactly once, with state derived from both resources.
    """
    # 1. Collect all BatchSandboxes
    batchsandbox_map = {}
    for bsbx in list_batchsandboxes():
        sandbox_id = get_sandbox_id(bsbx)
        batchsandbox_map[sandbox_id] = bsbx

    # 2. Collect all SandboxSnapshots
    snapshot_map = {}
    for snap in list_sandboxsnapshots():
        snapshot_map[snap.spec.sandboxId] = snap

    # 3. Merge: all sandboxIds from both maps
    all_ids = set(batchsandbox_map.keys()) | set(snapshot_map.keys())

    sandboxes = []
    for sandbox_id in all_ids:
        bsbx = batchsandbox_map.get(sandbox_id)
        snap = snapshot_map.get(sandbox_id)

        # Derive state from both resources
        state, reason, message = derive_sandbox_state(bsbx, snap)
        sandbox = build_sandbox(sandbox_id, state, reason, message, bsbx, snap)
        sandboxes.append(sandbox)

    return sandboxes
```

---

## Part 4: State Model

### 4.1 State Transitions

```
┌─────────┐     POST /pause      ┌──────────┐     commit done    ┌────────┐
│ Running │ ───────────────────▶ │ Pausing  │ ─────────────────▶ │ Paused │
└─────────┘                      └──────────┘                    └────────┘
     ▲                                                              │
     │         POST /resume                                         │
     │    ┌───────────────┐                                         │
     └────│   Resuming    │◀────────────────────────────────────────┘
          └───────────────┘
                │
                │ Pod ready
                ▼
          ┌─────────┐
          │ Running │
          └─────────┘
```

### 4.2 API Error Codes

| Scenario | HTTP Status | Error Code | Description |
|----------|-------------|------------|-------------|
| Pause when sandbox not found | 404 | SANDBOX_NOT_FOUND | - |
| Pause when sandbox not Running | 409 | INVALID_STATE | Can only pause Running state |
| Pause when no pausePolicy | 400 | PAUSE_POLICY_NOT_CONFIGURED | Created without pausePolicy |
| Pause when snapshot in progress | 409 | SNAPSHOT_IN_PROGRESS | Wait for completion |
| Resume when snapshot not found | 404 | SNAPSHOT_NOT_FOUND | - |
| Resume when snapshot not Ready | 409 | SNAPSHOT_NOT_READY | Wait for Ready phase |

---

## Part 5: Configuration and RBAC

### 5.1 Server Configuration

```toml
[pause]
default_snapshot_registry = "registry.example.com/sandbox-snapshots"
committer_image = "containerd/containerd:1.7"
cleanup_snapshot_image_on_delete = false
commit_timeout_seconds = 600
```

### 5.2 RBAC Rules

```yaml
# Controller Role
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: sandbox-snapshot-controller
rules:
  - apiGroups: ["sandbox.opensandbox.io"]
    resources: ["sandboxsnapshots", "sandboxsnapshots/status"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]

  - apiGroups: ["batch"]
    resources: ["jobs"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]

  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["get", "list", "watch"]

  - apiGroups: ["sandbox.opensandbox.io"]
    resources: ["batchsandboxes"]
    verbs: ["get", "list", "watch", "delete"]

  - apiGroups: [""]
    resources: ["events"]
    verbs: ["create", "patch"]
```

---

## Part 6: Test Plan

### 6.1 Server Unit Tests (Mock)

| Test Scenario | Verification |
|---------------|--------------|
| `pause_sandbox` normal flow | Creates SandboxSnapshot CR, phase=Pending |
| `pause_sandbox` no pausePolicy | Returns 400 PAUSE_POLICY_NOT_CONFIGURED |
| `pause_sandbox` not Running state | Returns 409 INVALID_STATE |
| `pause_sandbox` snapshot in progress | Returns 409 SNAPSHOT_IN_PROGRESS |
| `resume_sandbox` normal flow | Rebuilds BatchSandbox from resumeTemplate |
| `resume_sandbox` snapshot not found | Returns 404 SNAPSHOT_NOT_FOUND |
| `resume_sandbox` snapshot not Ready | Returns 409 SNAPSHOT_NOT_READY |
| `get_sandbox` state aggregation | BatchSandbox + SandboxSnapshot combination |
| `delete_sandbox` cleanup | Deletes both BatchSandbox and SandboxSnapshot |
| `list_sandboxes` merge | Correctly merges BatchSandbox and Snapshot |

### 6.2 Server E2E Tests (Mock)

```python
# server/tests/test_sandbox_pause_resume_e2e.py

import pytest
from unittest.mock import MagicMock, patch

@pytest.fixture
def mock_k8s():
    """Mock K8s client and providers."""
    with patch("opensandbox_server.services.k8s.kubernetes_service.K8sClient") as mock_client:
        snapshots = {}

        def create_snapshot(namespace, plural, body):
            snapshots[body["metadata"]["name"]] = {
                **body,
                "status": {"phase": "Pending"},
            }

        def get_snapshot(namespace, plural, name):
            return snapshots.get(name)

        mock_client.create_custom_object.side_effect = create_snapshot
        mock_client.get_custom_object.side_effect = get_snapshot

        yield {"client": mock_client, "snapshots": snapshots}


async def test_pause_creates_snapshot(client, mock_k8s):
    """POST /sandboxes/{id}/pause creates SandboxSnapshot."""
    response = client.post("/v1/sandboxes/test-sandbox/pause")
    assert response.status_code == 202
    assert "test-sandbox" in mock_k8s["snapshots"]


async def test_resume_requires_ready_snapshot(client, mock_k8s):
    """Resume fails when snapshot is not Ready."""
    mock_k8s["snapshots"]["test-sandbox"] = {
        "metadata": {"name": "test-sandbox"},
        "spec": {"sandboxId": "test-sandbox"},
        "status": {"phase": "Pending"},
    }

    response = client.post("/v1/sandboxes/test-sandbox/resume")
    assert response.status_code == 409
    assert response.json()["code"] == "SNAPSHOT_NOT_READY"


async def test_get_sandbox_returns_paused_state(client, mock_k8s):
    """GET /sandboxes/{id} returns Paused when only snapshot exists."""
    mock_k8s["snapshots"]["test-sandbox"] = {
        "metadata": {"name": "test-sandbox"},
        "spec": {"sandboxId": "test-sandbox"},
        "status": {"phase": "Ready"},
    }
    # No BatchSandbox exists

    response = client.get("/v1/sandboxes/test-sandbox")
    assert response.status_code == 200
    assert response.json()["status"]["state"] == "Paused"
```

### 6.3 K8s Controller E2E Tests

```go
// kubernetes/test/e2e/sandbox_snapshot_test.go

Context("SandboxSnapshot", func() {
    It("should create commit job pinned to source node", func() {
        // Create SandboxSnapshot with sourceNodeName
        // Verify Job.spec.template.spec.nodeName matches
    })

    It("should transition phase Pending -> Committing -> Ready", func() {
        // Watch phase transitions
    })

    It("should set phase to Failed when commit job fails", func() {
        // Use invalid image URI to trigger failure
    })

    It("should delete BatchSandbox after snapshot Ready", func() {
        // Verify cleanup behavior
    })
})
```

---

## Implementation Checklist

### CRD Layer
- [ ] Create `sandboxsnapshot_types.go`
- [ ] Add `PausePolicy` to `batchsandbox_types.go`
- [ ] Generate deepcopy functions
- [ ] Create CRD YAML

### Controller Layer
- [ ] Create `sandboxsnapshot_controller.go`
- [ ] Implement Reconcile logic
- [ ] Create commit Job builder
- [ ] Add RBAC rules

### Server Layer
- [ ] Add `PausePolicy` schema
- [ ] Add `PauseConfig` to config
- [ ] Implement `pause_sandbox()`
- [ ] Implement `resume_sandbox()`
- [ ] Update `get_sandbox()` state aggregation
- [ ] Update `list_sandboxes()` merge logic
- [ ] Update `delete_sandbox()` cleanup

### Test Layer
- [ ] Server unit tests
- [ ] Server E2E tests (mock)
- [ ] K8s controller E2E tests

### Documentation
- [ ] Update API docs
- [ ] Update user guide