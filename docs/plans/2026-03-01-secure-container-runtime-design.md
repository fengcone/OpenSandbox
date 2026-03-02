# Secure Container Runtime Implementation Design

**Date:** 2026-03-01
**Author:** @hittyt
**Status:** Design Approved
**OSEP:** [OSEP-0004](../../oseps/0004-secure-container-runtime.md)

---

## Executive Summary

This document details the implementation of secure container runtime support for OpenSandbox, enabling sandboxes to run in gVisor, Kata Containers, and other secure runtimes for hardware-level isolation.

**Key Principle:** Server-level configuration — administrators configure the runtime once at the server level, and all sandboxes transparently use it. SDK users require no code changes.

---

## Table of Contents

1. [Architecture](#architecture)
2. [Components](#components)
3. [Data Flow](#data-flow)
4. [Error Handling](#error-handling)
5. [Testing Strategy](#testing-strategy)
6. [File Changes](#file-changes)
7. [Implementation Stages](#implementation-stages)

---

## Architecture

### Overall Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                        OpenSandbox Server                        │
├─────────────────────────────────────────────────────────────────┤
│                                                                   │
│  ┌───────────────────────────────────────────────────────────┐  │
│  │                   Configuration Layer                      │  │
│  │  ┌─────────────┐  ┌──────────────────────────────────┐   │  │
│  │  │ ~/.sandbox  │  │   SecureRuntimeConfig            │   │  │
│  │  │    .toml    │─│  - type: "gvisor" | "kata" | ""   │   │  │
│  │  │             │  │  - docker_runtime: "runsc"       │   │  │
│  │  │ [runtime]   │  │  - k8s_runtime_class: "gvisor"   │   │  │
│  │  │ [secure_    │  └──────────────────────────────────┘   │  │
│  │  │  runtime]   │                                          │  │
│  │  └─────────────┘                                          │  │
│  └───────────────────────────────────────────────────────────┘  │
│                              │                                     │
│                              ▼                                     │
│  ┌───────────────────────────────────────────────────────────┐  │
│  │                  Runtime Resolver Layer                     │  │
│  │  ┌─────────────────────────────────────────────────────┐  │  │
│  │  │         SecureRuntimeResolver                        │  │  │
│  │  │  - get_docker_runtime()   → "runsc" | None          │  │  │
│  │  │  - get_k8s_runtime_class() → "gvisor" | None        │  │  │
│  │  │  - validate_at_startup()  → ConfigError | None       │  │  │
│  │  └─────────────────────────────────────────────────────┘  │  │
│  └───────────────────────────────────────────────────────────┘  │
│                              │                                     │
│              ┌───────────────┴───────────────┐                     │
│              ▼                               ▼                     │
│  ┌───────────────────────┐       ┌───────────────────────┐        │
│  │   Docker Mode         │       │   Kubernetes Mode      │        │
│  │  ┌─────────────────┐  │       │  ┌─────────────────┐  │        │
│  │  │ DockerSandbox   │  │       │  │  BatchSandbox   │  │        │
│  │  │    Service      │  │       │  │    Provider     │  │        │
│  │  │                 │  │       │  │                 │  │        │
│  │  │ runtime =       │  │       │  │ runtimeClass =  │  │        │
│  │  │   resolver.     │  │       │  │   resolver.     │  │        │
│  │  │   get_docker()  │  │       │  │   get_k8s()     │  │        │
│  │  └─────────────────┘  │       │  └─────────────────┘  │        │
│  │         │             │       │         │             │        │
│  │         ▼             │       │         ▼             │        │
│  │  docker run           │       │  Pod spec with         │        │
│  │  --runtime=runsc      │       │  runtimeClassName      │        │
│  └───────────────────────┘       └───────────────────────┘        │
│                                                                   │
└─────────────────────────────────────────────────────────────────┘
```

### Configuration Structure (TOML)

```toml
# ~/.sandbox.toml

[runtime]
type = "docker"  # or "kubernetes"
execd_image = "opensandbox/execd:v1.0.5"

# Secure container runtime configuration
# When enabled, ALL sandboxes on this server use the specified runtime
[secure_runtime]
# Runtime type: "gvisor", "kata", "firecracker", or "" for default runc
type = "gvisor"

# Docker mode: OCI runtime name (e.g., "runsc", "kata-runtime")
docker_runtime = "runsc"

# Kubernetes mode: RuntimeClass name (e.g., "gvisor", "kata-qemu")
k8s_runtime_class = "gvisor"
```

---

## Components

### 1. SecureRuntimeConfig

**Location:** `server/src/config.py`

```python
class SecureRuntimeConfig(BaseModel):
    """Secure container runtime configuration.

    When configured, all sandboxes on this server use the specified
    secure runtime (gVisor, Kata, etc.) for hardware-level isolation.
    """

    type: Literal["", "gvisor", "kata", "firecracker"] = Field(
        default="",
        description=(
            "Runtime type identifier. Empty string uses standard runc. "
            "Supported: gvisor, kata, firecracker."
        ),
    )
    docker_runtime: Optional[str] = Field(
        default=None,
        description=(
            "Docker mode: OCI runtime name (e.g., 'runsc', 'kata-runtime'). "
            "Ignored when runtime.type = 'kubernetes'."
        ),
    )
    k8s_runtime_class: Optional[str] = Field(
        default=None,
        description=(
            "Kubernetes mode: RuntimeClass name (e.g., 'gvisor', 'kata-qemu'). "
            "Ignored when runtime.type = 'docker'."
        ),
    )

    @model_validator(mode="after")
    def validate_runtime_config(self) -> "SecureRuntimeConfig":
        """Validate runtime configuration consistency."""
        if not self.type:
            return self

        # Firecracker is Kubernetes-only
        if self.type == "firecracker" and not self.k8s_runtime_class:
            raise ValueError(
                "secure_runtime.type='firecracker' requires k8s_runtime_class "
                "(Firecracker is not a standalone Docker OCI runtime)."
            )

        # Non-empty type must have corresponding runtime configured
        if self.type == "gvisor":
            if not self.docker_runtime and not self.k8s_runtime_class:
                raise ValueError(
                    "secure_runtime.type='gvisor' requires either "
                    "docker_runtime or k8s_runtime_class to be set."
                )
        elif self.type == "kata":
            if not self.docker_runtime and not self.k8s_runtime_class:
                raise ValueError(
                    "secure_runtime.type='kata' requires either "
                    "docker_runtime or k8s_runtime_class to be set."
                )

        return self
```

### 2. SecureRuntimeResolver

**Location:** `server/src/services/runtime_resolver.py` (new file)

```python
class SecureRuntimeResolver:
    """Resolves secure runtime config to backend-specific parameters.

    This class is initialized once at server startup with the validated
    configuration, then used by sandbox providers to get the appropriate
    runtime parameter for their backend.
    """

    def __init__(self, config: AppConfig):
        """Initialize resolver with server configuration."""
        self.secure_runtime = getattr(config, 'secure_runtime', None)
        self.runtime_mode = config.runtime.type  # "docker" or "kubernetes"

    def get_docker_runtime(self) -> Optional[str]:
        """Return Docker --runtime value, or None for runc default."""
        if not self.secure_runtime or not self.secure_runtime.type:
            return None

        if self.runtime_mode == "kubernetes":
            return None

        if not self.secure_runtime.docker_runtime:
            raise ConfigError(
                f"Secure runtime '{self.secure_runtime.type}' is configured "
                f"but docker_runtime is empty."
            )

        return self.secure_runtime.docker_runtime

    def get_k8s_runtime_class(self) -> Optional[str]:
        """Return K8s runtimeClassName, or None for cluster default."""
        if not self.secure_runtime or not self.secure_runtime.type:
            return None

        if self.runtime_mode == "docker":
            return None

        return self.secure_runtime.k8s_runtime_class

    def is_enabled(self) -> bool:
        """Check if any secure runtime is configured."""
        return bool(
            self.secure_runtime and
            self.secure_runtime.type
        )
```

### 3. Startup Validation

**Location:** `server/src/services/runtime_resolver.py`

```python
async def validate_secure_runtime_on_startup(
    config: AppConfig,
    docker_client=None,
    k8s_client=None,
) -> None:
    """Validate secure runtime availability at server startup.

    This function is called during server initialization to fail fast
    if the configured runtime is not available.

    Args:
        config: Application configuration.
        docker_client: Docker client (for Docker mode).
        k8s_client: Kubernetes client (for K8s mode).

    Raises:
        ConfigError: If runtime is configured but not available.
    """
    sr = getattr(config, 'secure_runtime', None)
    if not sr or not sr.type:
        logger.info("No secure runtime configured; using standard runc.")
        return

    resolver = SecureRuntimeResolver(config)

    if config.runtime.type == "docker":
        await _validate_docker_runtime(sr, resolver, docker_client)
    else:  # kubernetes
        await _validate_k8s_runtime(sr, resolver, k8s_client)

    logger.info(f"Secure runtime '{sr.type}' validated successfully.")
```

---

## Data Flow

### Sandbox Creation Flow

```
1. Server Startup
   └─ load_config()
      └─ validate_secure_runtime_on_startup()
         ├─ K8s: check RuntimeClass exists
         └─ Docker: check runtime in daemon.json

2. Provider Initialization
   └─ BatchSandboxProvider.__init__()
      ├─ resolver = SecureRuntimeResolver(config)
      └─ runtime_class = resolver.get_k8s_runtime_class()

3. Sandbox Creation (SDK Request)
   └─ HTTP POST /api/v1/sandboxes
      └─ KubernetesService.create_sandbox()
         └─ BatchSandboxProvider.create_workload()
            ├─ Build pod spec
            ├─ If runtime_class: pod_spec["runtimeClassName"] = runtime_class
            └─ Create BatchSandbox CR
               └─ Kubernetes creates Pod with runtimeClassName
                  └─ containerd uses runsc handler → gVisor
```

### Pool Consistency Check

```
Server Config              Pool CRD
┌──────────────────┐      ┌──────────────────┐
│ type = "gvisor"  │ ≠?   │ runtimeClass =   │
│ k8s_runtime =    │      │   "kata"         │
│   "gvisor"       │      └──────────────────┘
└──────────────────┘

         │
         ▼
┌─────────────────────┐
│ Validation Result   │
└─────────────────────┘
         │
    ┌────┴────┐
    ▼         ▼
 Match ✓   Mismatch ✗
 Use Pool  Log WARNING
           Skip Pool
```

---

## Error Handling

### Error Types

| Scenario | Error Type | Action |
|----------|------------|--------|
| RuntimeClass not found | ConfigError | Server fails to start |
| Docker runtime not in daemon | ConfigError | Server fails to start |
| Invalid config combination | ValidationError | Server fails to start |
| Pool runtime mismatch | Warning | Skip pool, log warning |

### Error Messages

```python
ERROR_RUNTIME_CLASS_NOT_FOUND = """
RuntimeClass '{runtime_class}' does not exist in the cluster.
Please create it first.
"""

ERROR_DOCKER_RUNTIME_NOT_AVAILABLE = """
Docker runtime '{runtime_name}' is not available.
Available runtimes: {available_runtimes}
"""

WARNING_POOL_RUNTIME_MISMATCH = """
Pool '{pool_name}' has runtimeClassName='{pool_runtime}' but server is
configured for '{server_runtime}'. This pool will NOT be used.
"""
```

---

## Testing Strategy

### Unit Tests

| Test File | Coverage |
|-----------|----------|
| `tests/unit/test_config.py` | SecureRuntimeConfig parsing |
| `tests/unit/test_runtime_resolver.py` | Resolver methods |
| `tests/unit/test_startup_validation.py` | Startup validation |
| `tests/unit/test_pool_validation.py` | Pool consistency |

### Integration Tests

| Test | Description |
|------|-------------|
| Server starts with valid RuntimeClass | Verify successful startup |
| Server fails with missing RuntimeClass | Verify failure with clear error |
| Pool compatibility check | Verify warning on mismatch |

### E2E Tests

| Test | Description |
|------|-------------|
| Runtime injection | Pod has runtimeClassName |
| Docker runtime | Container uses correct runtime |
| Pool mode | Pool matching behavior |

---

## File Changes

### New Files

| File | Lines | Description |
|------|-------|-------------|
| `server/src/services/runtime_resolver.py` | ~200 | Resolver and validation |
| `tests/unit/test_runtime_resolver.py` | ~200 | Resolver tests |
| `tests/unit/test_startup_validation.py` | ~150 | Validation tests |
| `tests/integration/test_k8s_secure_runtime.py` | ~200 | K8s integration tests |
| `tests/integration/test_docker_secure_runtime.py` | ~150 | Docker integration tests |

### Modified Files

| File | Changes | Lines |
|------|---------|-------|
| `server/src/config.py` | Add SecureRuntimeConfig | ~100 |
| `server/src/services/k8s/batchsandbox_provider.py` | Inject runtimeClassName | ~50 |
| `server/src/services/k8s/agent_sandbox_provider.py` | Inject runtimeClassName | ~30 |
| `server/src/services/docker.py` | Add runtime parameter | ~20 |
| `server/src/main.py` | Startup validation call | ~10 |
| `docs/secure-container.md` | Update for server-level config | Rewrite |

---

## Implementation Stages

### Stage 1: Configuration Foundation (2-3 days)

- [ ] SecureRuntimeConfig class in config.py
- [ ] SecureRuntimeResolver in runtime_resolver.py
- [ ] validate_secure_runtime_on_startup function
- [ ] main.py calls startup validation
- [ ] Unit tests for config parsing
- [ ] Unit tests for resolver

### Stage 2: Kubernetes Injection (2-3 days)

- [ ] BatchSandboxProvider uses resolver
- [ ] AgentSandboxProvider uses resolver
- [ ] Pool consistency validation
- [ ] Integration tests for K8s
- [ ] E2E tests for runtime injection

### Stage 3: Docker Support (1-2 days)

- [ ] DockerSandboxService uses resolver
- [ ] Docker runtime validation
- [ ] Integration tests for Docker
- [ ] E2E tests for Docker runtime

### Stage 4: Testing & Documentation (1-2 days)

- [ ] All unit tests passing
- [ ] All integration tests passing
- [ ] All E2E tests passing
- [ ] docs/secure-container.md updated
- [ ] OSEP-0004 status updated

**Total Estimated Time:** 6-10 days

---

## References

- [OSEP-0004: Pluggable Secure Container Runtime Support](../../oseps/0004-secure-container-runtime.md)
- [gVisor Documentation](https://gvisor.dev/docs/)
- [Kata Containers Documentation](https://katacontainers.io/docs/)
