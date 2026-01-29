---
title: Volume and VolumeBinding Support
authors:
  - "yutian.taoyt"
creation-date: 2026-01-29
last-updated: 2026-01-29
status: draft
---

# OSEP-0003: Volume and VolumeBinding Support

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Requirements](#requirements)
- [Proposal](#proposal)
  - [Notes/Constraints/Caveats](#notesconstraintscaveats)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
- [Test Plan](#test-plan)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
- [Infrastructure Needed](#infrastructure-needed)
- [Upgrade & Migration Strategy](#upgrade--migration-strategy)
<!-- /toc -->

## Summary

Introduce a runtime-neutral Volume + VolumeBinding model in the Lifecycle API to enable persistent storage mounts across Docker and Kubernetes sandboxes. The proposal adds explicit volume definitions, binding semantics, and security constraints so that artifacts can persist beyond sandbox lifecycles without relying on file transfers.

```text
Time --------------------------------------------------------------->

Volume lifecycle:  [provisioned]-------------------------[retained]--->
Sandbox lifecycle:           [create]---[running]---[stop/delete]
                              |                         |
                          bind volume              unbind volume
```

## Motivation

OpenSandbox users running long-lived agents need artifacts (web pages, images, reports) to persist after a sandbox is terminated or restarted. Today, the API only supports transient filesystem operations via upload/download and provides no mount semantics; as a result, users must move large outputs out-of-band. This proposal adds first-class storage semantics while maintaining runtime portability and security boundaries.

### Goals

- Add Volume + VolumeBinding fields to the Lifecycle API without breaking existing clients.
- Support Docker bind mounts (local path) and Kubernetes PVC/NFS mounts as the initial MVP.
- Provide secure, explicit controls for read/write access and path isolation.
- Keep runtime-specific details out of the core API where possible.

### Non-Goals

- Full-featured storage orchestration (auto-provisioning, snapshots, backups).
- Object storage mounting (S3/OSS) in the initial MVP.
- Cross-sandbox sharing semantics beyond explicit volume bindings.
- Guaranteeing portability for every storage backend in every runtime.

## Requirements

- Backward compatible with existing sandbox creation requests.
- Works with both Docker and Kubernetes runtimes.
- Enforces path safety and explicit read/write permissions.
- Supports per-sandbox isolation (via subPath or equivalent).
- Clear error messages when a runtime does not support a requested backend.

## Proposal

Add two new optional fields to the Lifecycle API:
- `volumes[]`: defines reusable, runtime-neutral storage resources.
- `volumeBindings[]`: binds a volume to a sandbox mount path with access restrictions and optional subPath isolation. Use `accessMode` on bindings to define read/write behavior and avoid conflicting flags.

The core API describes what storage is required, while each runtime provider translates the model into platform-specific mounts. Provider-specific options are supplied via a generic `parameters` map on `volumes[]` when needed. `volumeBindings[]` does not define `parameters`; binding-specific behavior should be expressed with explicit fields such as `mountPath`, `accessMode`, and `subPath`.

### Notes/Constraints/Caveats

- Kubernetes template merging currently replaces lists; this proposal requires list-merge or append behavior for volumes/volumeMounts to preserve user input.
- Pool-based creation must allow volume bindings defined by the Pool template or a future binding override mechanism.

### Risks and Mitigations

- Security risk: Docker hostPath mounts can expose host data. Mitigation: enforce allowlist prefixes, forbid path traversal, and require explicit `accessMode=RW` for write access.
- Portability risk: different backends behave differently. Mitigation: keep core API minimal and require explicit backend selection.
- Operational risk: storage misconfiguration causes startup failures. Mitigation: validate mounts early and provide clear error responses.

## Design Details

### API schema changes
Add to `CreateSandboxRequest`:

```yaml
volumes:
  - name: workdir
    backendType: local
    backendRef: "/data/opensandbox/user-a"
    parameters:
      storageClass: "fast"
      size: "10Gi"

volumeBindings:
  - volumeName: workdir
    mountPath: /mnt/work
    accessMode: RW
    subPath: "task-001"
```

### Core semantics
- `volumes[]` declares storage resources. Each volume has a logical `name` and a `backendType`/`backendRef` pair describing the underlying storage, with optional `parameters` for backend-specific attributes.
- `volumeBindings[]` ties a volume to a sandbox mount path with explicit `accessMode` and optional `subPath` isolation.

### API enum specifications
Enumerations are fixed and validated by the API:
- `accessMode`: use short forms `RW` (read/write) and `RO` (read-only). Examples in this document follow that convention.
- `backendType`: `local`, `pvc`, `nfs`. `local` refers to host path bind mounts in Docker and hostPath-equivalent mounts in Kubernetes, and must be documented explicitly to avoid ambiguity.

### Backend constraints (minimum schema)
Define minimal, documented constraints per `backendType` to reduce runtime-only failures:
- `backendType=local`: `backendRef` must be an absolute host path (e.g., `/data/opensandbox/user-a`). Reject relative paths and require normalization before validation.
- `backendType=pvc`: `backendRef` is the PVC `claimName` and must be a valid DNS-1123 name.
- `backendType=nfs`: `backendRef` uses `server:/export/path` format and must include both server and absolute export path.
These constraints are enforced in request validation and surfaced as clear API errors; runtimes may apply stricter checks.

### Permissions and ownership
Volume permissions are a frequent source of runtime failures and must be explicit in the contract:
- Default behavior: OpenSandbox does not automatically fix ownership or permissions on mounted storage. Users are responsible for ensuring the `backendRef` target is writable by the sandbox process UID/GID.
- Docker: host path permissions are enforced by the host filesystem. Even with `accessMode=RW`, writes will fail if the host path is not writable by the container user.
- Kubernetes: PVC permissions vary by CSI driver. If runtime supports it, `volumes[].parameters.fsGroup` can be used to request a pod-level `fsGroup` for volume access; otherwise users must provision PVCs with compatible ownership.

### Concurrency and isolation
SubPath provides path-level isolation, not concurrency control. If multiple sandboxes bind the same volume without distinct `subPath` values and use `accessMode=RW`, they may overwrite each other. OpenSandbox does not provide file-locking or coordination; users are responsible for handling concurrent access safely.

### Docker mapping
- `backendType=local` maps to bind mounts.
- `backendRef + subPath` resolves to a concrete host directory.
- The host config uses `mounts`/`binds` with `readOnly` derived from `accessMode`.
- If the resolved host path does not exist, the request fails validation (do not auto-create host directories in MVP to avoid permission and security pitfalls).

### Kubernetes mapping
- `backendType=pvc` maps to `persistentVolumeClaim`.
- `backendType=nfs` maps to `nfs` volume fields.
- `subPath` maps to `volumeMounts.subPath`.

### Example: Docker local path
Create a sandbox with a local host path bind mount, read/write, isolated to a subdirectory:

```yaml
volumes:
  - name: workdir
    backendType: local
    backendRef: "/data/opensandbox/user-a"

volumeBindings:
  - volumeName: workdir
    mountPath: /mnt/work
    accessMode: RW
    subPath: "task-001"
```

Runtime mapping (Docker):
- host path: `/data/opensandbox/user-a/task-001`
- container path: `/mnt/work`
- accessMode: `RW`

### Example: Python SDK (lifecycle client)
Use the Python SDK lifecycle client to create a sandbox with a local path volume mount:

```python
from opensandbox.api.lifecycle.client import AuthenticatedClient
from opensandbox.api.lifecycle.api.sandboxes import post_sandboxes
from opensandbox.api.lifecycle.models.create_sandbox_request import CreateSandboxRequest
from opensandbox.api.lifecycle.models.image_spec import ImageSpec
from opensandbox.api.lifecycle.models.resource_limits import ResourceLimits

client = AuthenticatedClient(base_url="https://api.opensandbox.io", token="YOUR_API_KEY")

resource_limits = ResourceLimits.from_dict({"cpu": "500m", "memory": "512Mi"})
request = CreateSandboxRequest(
    image=ImageSpec(uri="python:3.11"),
    timeout=3600,
    resource_limits=resource_limits,
    entrypoint=["python", "-c", "print('hello')"],
)
request["volumes"] = [
    {
        "name": "workdir",
        "backendType": "local",
        "backendRef": "/data/opensandbox/user-a",
    }
]
request["volumeBindings"] = [
    {
        "volumeName": "workdir",
        "mountPath": "/mnt/work",
        "accessMode": "RW",
        "subPath": "task-001",
    }
]

post_sandboxes.sync(client=client, body=request)
```

### Example: Kubernetes PVC
Create a sandbox that mounts a pre-provisioned PVC with subPath isolation:

```yaml
volumes:
  - name: workdir
    backendType: pvc
    backendRef: "user-a-pvc"

volumeBindings:
  - volumeName: workdir
    mountPath: /mnt/work
    accessMode: RW
    subPath: "task-001"
```

Runtime mapping (Kubernetes):
```yaml
volumes:
  - name: workdir
    persistentVolumeClaim:
      claimName: user-a-pvc
containers:
  - name: sandbox
    volumeMounts:
      - name: workdir
        mountPath: /mnt/work
        readOnly: false  # derived from accessMode=RW
        subPath: task-001
```

Python SDK example (PVC):

```python
from opensandbox.api.lifecycle.client import AuthenticatedClient
from opensandbox.api.lifecycle.api.sandboxes import post_sandboxes
from opensandbox.api.lifecycle.models.create_sandbox_request import CreateSandboxRequest
from opensandbox.api.lifecycle.models.image_spec import ImageSpec
from opensandbox.api.lifecycle.models.resource_limits import ResourceLimits

client = AuthenticatedClient(base_url="https://api.opensandbox.io", token="YOUR_API_KEY")

resource_limits = ResourceLimits.from_dict({"cpu": "500m", "memory": "512Mi"})
request = CreateSandboxRequest(
    image=ImageSpec(uri="python:3.11"),
    timeout=3600,
    resource_limits=resource_limits,
    entrypoint=["python", "-c", "print('hello')"],
)
request["volumes"] = [
    {
        "name": "workdir",
        "backendType": "pvc",
        "backendRef": "user-a-pvc",
    }
]
request["volumeBindings"] = [
    {
        "volumeName": "workdir",
        "mountPath": "/mnt/work",
        "accessMode": "RW",
        "subPath": "task-001",
    }
]

post_sandboxes.sync(client=client, body=request)
```

### Provider validation
- Reject unsupported backends per runtime.
- Normalize and validate `subPath` against traversal; reject `..` and absolute path inputs.
- Enforce allowlist prefixes for Docker host paths.

### Configuration (example)
Host path allowlists are configured by the control plane (server/execd) and enforced at validation time. Example `config.toml`:

```toml
[storage]
allow_host_paths = ["/data/opensandbox", "/tmp/sandbox"]
```

## Test Plan

- Unit tests for schema validation and path normalization.
- Provider unit tests:
  - Docker: bind mount generation, read-only enforcement, allowlist rejection.
  - Kubernetes: PVC/NFS mapping, subPath propagation.
- Integration tests for sandbox creation with volumes in Docker and K8s.
- Negative tests for unsupported backends and invalid paths.

## Drawbacks

- Adds API surface area and increases runtime provider complexity.
- Docker bind mounts introduce security considerations and operational policy requirements.

## Alternatives

- Keep using file upload/download only: simpler but does not satisfy persistence requirements.
- Use runtime-specific `extensions` only: faster to ship but fractures API consistency and increases client complexity.

## Infrastructure Needed

None required for the MVP; runtime environments must already have access to the storage backends being mounted (e.g., host paths, PVCs, NFS).

## Upgrade & Migration Strategy

This change is additive and backward compatible. Existing clients continue to work without modification. If a client submits volume fields to a runtime that does not support them, the API will return a clear validation error.
