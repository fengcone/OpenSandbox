# Secure Container Runtime Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Implement server-level secure container runtime configuration (gVisor, Kata) that applies transparently to all sandboxes without SDK changes.

**Architecture:**
1. Add `[secure_runtime]` TOML config block parsed by `SecureRuntimeConfig` class
2. `SecureRuntimeResolver` translates config to backend-specific parameters (Docker `--runtime`, K8s `runtimeClassName`)
3. Providers inject runtime at sandbox creation time
4. Startup validation fails fast if runtime is unavailable

**Tech Stack:** Python 3.10+, FastAPI, Pydantic, Docker SDK, Kubernetes Python client

**Prerequisites:**
- Read `docs/plans/2026-03-01-secure-container-runtime-design.md` for full design context
- Read `oseps/0004-secure-container-runtime.md` for requirements
- Current branch: `feature/public-secure-container`

---

## Stage 1: Configuration Foundation

### Task 1.1: Add SecureRuntimeConfig to config.py

**Files:**
- Modify: `server/src/config.py`

**Context:** This adds the Pydantic model for parsing the `[secure_runtime]` TOML section. Add it after the `DockerConfig` class definition (around line 237).

**Step 1: Add SecureRuntimeConfig class**

```python
# After DockerConfig class (line ~237), add:

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
            # Empty type is valid (means use default runc)
            return self

        # Firecracker is Kubernetes-only (requires kata-fc handler)
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

    class Config:
        populate_by_name = True
```

**Step 2: Update AppConfig to include secure_runtime**

Find the `AppConfig` class (around line 239) and add `secure_runtime` field:

```python
# In AppConfig class, add after the `egress` field (around line 249):

class AppConfig(BaseModel):
    """Root application configuration model."""

    server: ServerConfig = Field(default_factory=ServerConfig)
    runtime: RuntimeConfig = Field(..., description="Sandbox runtime configuration.")
    kubernetes: Optional[KubernetesRuntimeConfig] = None
    agent_sandbox: Optional["AgentSandboxRuntimeConfig"] = None
    router: Optional[RouterConfig] = None
    docker: DockerConfig = Field(default_factory=DockerConfig)
    storage: StorageConfig = Field(default_factory=StorageConfig)
    egress: Optional[EgressConfig] = None
    secure_runtime: Optional[SecureRuntimeConfig] = None  # <-- ADD THIS LINE
```

**Step 3: Update __all__ exports**

At the bottom of `config.py`, add `SecureRuntimeConfig` to the exports (around line 355):

```python
__all__ = [
    "AppConfig",
    "ServerConfig",
    "RuntimeConfig",
    "RouterConfig",
    "DockerConfig",
    "StorageConfig",
    "KubernetesRuntimeConfig",
    "EgressConfig",
    "SecureRuntimeConfig",  # <-- ADD THIS LINE
    "DEFAULT_CONFIG_PATH",
    "CONFIG_ENV_VAR",
    "get_config",
    "get_config_path",
    "load_config",
]
```

**Step 4: Verify config file syntax**

Check that the file has no syntax errors:

```bash
cd /Users/fengjianhui/WorkSpaceGithub/OpenSandbox && python -c "from server.src.config import AppConfig; print('Import successful')"
```

Expected: `Import successful`

**Step 5: Commit**

```bash
git add server/src/config.py
git commit -m "feat(config): add SecureRuntimeConfig for secure runtime configuration

Add Pydantic model for parsing [secure_runtime] TOML section with:
- type: \"gvisor\" | \"kata\" | \"firecracker\" | \"\"
- docker_runtime: OCI runtime name for Docker mode
- k8s_runtime_class: RuntimeClass name for Kubernetes mode
- Validation for firecracker K8s-only requirement
"
```

---

### Task 1.2: Create runtime_resolver.py module

**Files:**
- Create: `server/src/services/runtime_resolver.py`
- Create: `tests/unit/test_runtime_resolver.py`

**Context:** This module contains the `SecureRuntimeResolver` class that translates config to backend parameters, plus startup validation functions.

**Step 1: Create runtime_resolver.py with base imports**

```bash
cat > /Users/fengjianhui/WorkSpaceGithub/OpenSandbox/server/src/services/runtime_resolver.py << 'EOF'
# Copyright 2025 Alibaba Group Holding Ltd.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""
Secure runtime resolver for translating configuration to backend parameters.

This module provides the SecureRuntimeResolver class which converts
server-level secure runtime configuration into backend-specific
parameters (Docker --runtime or Kubernetes runtimeClassName).
"""

import logging
from typing import Optional

from kubernetes.client.exceptions import ApiException

from src.config import AppConfig

logger = logging.getLogger(__name__)


class ConfigError(Exception):
    """Configuration error for secure runtime."""
    pass


class SecureRuntimeResolver:
    """Resolves secure runtime config to backend-specific parameters.

    This class is initialized once at server startup with the validated
    configuration, then used by sandbox providers to get the appropriate
    runtime parameter for their backend.
    """

    def __init__(self, config: AppConfig):
        """Initialize resolver with server configuration.

        Args:
            config: Validated application configuration.
        """
        self.secure_runtime = getattr(config, 'secure_runtime', None)
        self.runtime_mode = config.runtime.type  # "docker" or "kubernetes"

    def get_docker_runtime(self) -> Optional[str]:
        """Return Docker --runtime value, or None for runc default.

        Returns:
            OCI runtime name (e.g., "runsc", "kata-runtime") or None.

        Raises:
            ConfigError: If runtime is configured but not available in Docker mode.
        """
        if not self.secure_runtime or not self.secure_runtime.type:
            return None

        if self.runtime_mode == "kubernetes":
            return None

        if not self.secure_runtime.docker_runtime:
            raise ConfigError(
                f"Secure runtime '{self.secure_runtime.type}' is configured "
                f"but docker_runtime is empty. This runtime may not be supported "
                f"in Docker mode (e.g., firecracker)."
            )

        return self.secure_runtime.docker_runtime

    def get_k8s_runtime_class(self) -> Optional[str]:
        """Return K8s runtimeClassName, or None for cluster default.

        Returns:
            RuntimeClass name (e.g., "gvisor", "kata-qemu") or None.
        """
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


async def _validate_docker_runtime(
    sr: "SecureRuntimeConfig",
    resolver: SecureRuntimeResolver,
    docker_client,
) -> None:
    """Validate Docker runtime availability."""
    docker_runtime = resolver.get_docker_runtime()

    if not docker_runtime:
        raise ConfigError(
            f"secure_runtime.type='{sr.type}' but docker_runtime is empty. "
            f"This runtime is not supported in Docker mode."
        )

    try:
        info = docker_client.info()
        available = info.get("Runtimes", {}).keys()

        if docker_runtime not in available:
            raise ConfigError(
                f"Docker runtime '{docker_runtime}' is not available. "
                f"Available runtimes: {list(available)}. "
                f"Please install and configure it in /etc/docker/daemon.json."
            )
    except Exception as e:
        if isinstance(e, ConfigError):
            raise
        raise ConfigError(
            f"Failed to validate Docker runtime '{docker_runtime}': {e}"
        )


async def _validate_k8s_runtime(
    sr: "SecureRuntimeConfig",
    resolver: SecureRuntimeResolver,
    k8s_client,
) -> None:
    """Validate Kubernetes RuntimeClass availability."""
    runtime_class = resolver.get_k8s_runtime_class()

    if not runtime_class:
        raise ConfigError(
            f"secure_runtime.type='{sr.type}' but k8s_runtime_class is empty."
        )

    try:
        await k8s_client.read_runtime_class(runtime_class)
    except ApiException as e:
        if e.status == 404:
            raise ConfigError(
                f"RuntimeClass '{runtime_class}' does not exist. "
                f"Please create it in the cluster."
            )
        raise ConfigError(
            f"Failed to validate RuntimeClass '{runtime_class}': {e}"
        )


__all__ = [
    "ConfigError",
    "SecureRuntimeResolver",
    "validate_secure_runtime_on_startup",
]
EOF
```

**Step 2: Verify the module imports correctly**

```bash
cd /Users/fengjianhui/WorkSpaceGithub/OpenSandbox && python -c "from server.src.services.runtime_resolver import SecureRuntimeResolver; print('Import successful')"
```

Expected: `Import successful`

**Step 3: Commit**

```bash
git add server/src/services/runtime_resolver.py
git commit -m "feat(runtime): add SecureRuntimeResolver for runtime parameter resolution

Add resolver class that translates [secure_runtime] config to backend:
- get_docker_runtime(): Returns OCI runtime name or None
- get_k8s_runtime_class(): Returns RuntimeClass name or None
- is_enabled(): Checks if any secure runtime is configured
- validate_secure_runtime_on_startup(): Validates availability at startup
"
```

---

### Task 1.3: Write unit tests for SecureRuntimeResolver

**Files:**
- Create: `tests/unit/test_runtime_resolver.py`

**Step 1: Create test file**

```bash
cat > /Users/fengjianhui/WorkSpaceGithub/OpenSandbox/tests/unit/test_runtime_resolver.py << 'EOF'
# Copyright 2025 Alibaba Group Holding Ltd.
#
# Licensed under the Apache License, Version 2.0

"""Unit tests for SecureRuntimeResolver."""

import pytest

from src.config import AppConfig
from src.services.runtime_resolver import (
    ConfigError,
    SecureRuntimeResolver,
)


class TestSecureRuntimeResolver:
    """Test SecureRuntimeResolver functionality."""

    def test_no_secure_runtime_configured_returns_none(self):
        """When no secure_runtime, returns None for both modes."""
        config = AppConfig(
            runtime={"type": "docker", "execd_image": "execd:v1"},
            secure_runtime={"type": "", "docker_runtime": None, "k8s_runtime_class": None},
        )
        resolver = SecureRuntimeResolver(config)

        assert resolver.get_docker_runtime() is None
        assert resolver.get_k8s_runtime_class() is None
        assert not resolver.is_enabled()

    def test_gvisor_docker_mode_returns_docker_runtime(self):
        """gVisor in Docker mode returns docker_runtime."""
        config = AppConfig(
            runtime={"type": "docker", "execd_image": "execd:v1"},
            secure_runtime={
                "type": "gvisor",
                "docker_runtime": "runsc",
                "k8s_runtime_class": "gvisor",
            },
        )
        resolver = SecureRuntimeResolver(config)

        assert resolver.get_docker_runtime() == "runsc"
        assert resolver.get_k8s_runtime_class() is None  # Docker mode ignores K8s
        assert resolver.is_enabled()

    def test_gvisor_kubernetes_mode_returns_k8s_runtime_class(self):
        """gVisor in K8s mode returns k8s_runtime_class."""
        config = AppConfig(
            runtime={"type": "kubernetes", "execd_image": "execd:v1"},
            secure_runtime={
                "type": "gvisor",
                "docker_runtime": "runsc",
                "k8s_runtime_class": "gvisor",
            },
        )
        resolver = SecureRuntimeResolver(config)

        assert resolver.get_docker_runtime() is None  # K8s mode ignores Docker
        assert resolver.get_k8s_runtime_class() == "gvisor"
        assert resolver.is_enabled()

    def test_kata_docker_mode(self):
        """Kata in Docker mode returns docker_runtime."""
        config = AppConfig(
            runtime={"type": "docker", "execd_image": "execd:v1"},
            secure_runtime={
                "type": "kata",
                "docker_runtime": "kata-runtime",
                "k8s_runtime_class": "kata-qemu",
            },
        )
        resolver = SecureRuntimeResolver(config)

        assert resolver.get_docker_runtime() == "kata-runtime"
        assert resolver.is_enabled()

    def test_kata_kubernetes_mode(self):
        """Kata in K8s mode returns k8s_runtime_class."""
        config = AppConfig(
            runtime={"type": "kubernetes", "execd_image": "execd:v1"},
            secure_runtime={
                "type": "kata",
                "docker_runtime": "kata-runtime",
                "k8s_runtime_class": "kata-qemu",
            },
        )
        resolver = SecureRuntimeResolver(config)

        assert resolver.get_k8s_runtime_class() == "kata-qemu"
        assert resolver.is_enabled()

    def test_firecracker_docker_mode_raises_error(self):
        """Firecracker in Docker mode should raise ConfigError."""
        config = AppConfig(
            runtime={"type": "docker", "execd_image": "execd:v1"},
            secure_runtime={
                "type": "firecracker",
                "docker_runtime": "",
                "k8s_runtime_class": "kata-fc",
            },
        )
        resolver = SecureRuntimeResolver(config)

        with pytest.raises(ConfigError) as exc:
            resolver.get_docker_runtime()

        assert "not supported in Docker mode" in str(exc.value)

    def test_firecracker_kubernetes_mode(self):
        """Firecracker in K8s mode returns k8s_runtime_class."""
        config = AppConfig(
            runtime={"type": "kubernetes", "execd_image": "execd:v1"},
            secure_runtime={
                "type": "firecracker",
                "docker_runtime": "",
                "k8s_runtime_class": "kata-fc",
            },
        )
        resolver = SecureRuntimeResolver(config)

        assert resolver.get_k8s_runtime_class() == "kata-fc"
        assert resolver.is_enabled()

    def test_empty_type_with_none_runtime_fields(self):
        """Empty type with None fields returns None for both."""
        config = AppConfig(
            runtime={"type": "docker", "execd_image": "execd:v1"},
            secure_runtime={"type": "", "docker_runtime": None, "k8s_runtime_class": None},
        )
        resolver = SecureRuntimeResolver(config)

        assert resolver.get_docker_runtime() is None
        assert resolver.get_k8s_runtime_class() is None
        assert not resolver.is_enabled()
EOF
```

**Step 2: Run the tests**

```bash
cd /Users/fengjianhui/WorkSpaceGithub/OpenSandbox && pytest tests/unit/test_runtime_resolver.py -v
```

Expected: All tests pass

**Step 3: Commit**

```bash
git add tests/unit/test_runtime_resolver.py
git commit -m "test(runtime): add unit tests for SecureRuntimeResolver

Test coverage:
- No secure runtime configured
- gVisor in Docker and Kubernetes modes
- Kata in Docker and Kubernetes modes
- Firecracker Kubernetes-only validation
- Empty type handling
"
```

---

### Task 1.4: Write unit tests for SecureRuntimeConfig validation

**Files:**
- Modify: `tests/unit/test_config.py` (create if not exists)

**Step 1: Create or extend test_config.py**

First check if file exists:

```bash
ls -la /Users/fengjianhui/WorkSpaceGithub/OpenSandbox/tests/unit/test_config.py 2>/dev/null || echo "FILE_NOT_FOUND"
```

If file doesn't exist, create it:

```bash
cat > /Users/fengjianhui/WorkSpaceGithub/OpenSandbox/tests/unit/test_config.py << 'EOF'
# Copyright 2025 Alibaba Group Holding Ltd.
#
# Licensed under the Apache License, Version 2.0

"""Unit tests for configuration models."""

import pytest

from pydantic import ValidationError

from src.config import (
    AppConfig,
    SecureRuntimeConfig,
)


class TestSecureRuntimeConfig:
    """Test SecureRuntimeConfig validation."""

    def test_empty_type_is_valid(self):
        """Empty type (default runc) should be valid."""
        config = SecureRuntimeConfig(type="")
        assert config.type == ""
        assert config.docker_runtime is None
        assert config.k8s_runtime_class is None

    def test_gvisor_with_docker_runtime_is_valid(self):
        """gVisor with docker_runtime should be valid."""
        config = SecureRuntimeConfig(
            type="gvisor",
            docker_runtime="runsc",
            k8s_runtime_class="gvisor",
        )
        assert config.type == "gvisor"
        assert config.docker_runtime == "runsc"
        assert config.k8s_runtime_class == "gvisor"

    def test_gvisor_with_k8s_runtime_class_is_valid(self):
        """gVisor with only k8s_runtime_class should be valid."""
        config = SecureRuntimeConfig(
            type="gvisor",
            docker_runtime=None,
            k8s_runtime_class="gvisor",
        )
        assert config.type == "gvisor"
        assert config.docker_runtime is None
        assert config.k8s_runtime_class == "gvisor"

    def test_kata_with_runtimes_is_valid(self):
        """Kata with both runtimes should be valid."""
        config = SecureRuntimeConfig(
            type="kata",
            docker_runtime="kata-runtime",
            k8s_runtime_class="kata-qemu",
        )
        assert config.type == "kata"
        assert config.docker_runtime == "kata-runtime"
        assert config.k8s_runtime_class == "kata-qemu"

    def test_firecracker_with_k8s_runtime_is_valid(self):
        """Firecracker with k8s_runtime_class should be valid."""
        config = SecureRuntimeConfig(
            type="firecracker",
            docker_runtime="",
            k8s_runtime_class="kata-fc",
        )
        assert config.type == "firecracker"
        assert config.docker_runtime == ""
        assert config.k8s_runtime_class == "kata-fc"

    def test_firecracker_without_k8s_runtime_raises_error(self):
        """Firecracker without k8s_runtime_class should raise error."""
        with pytest.raises(ValidationError) as exc:
            SecureRuntimeConfig(
                type="firecracker",
                docker_runtime="",
                k8s_runtime_class=None,
            )

        assert "k8s_runtime_class" in str(exc.value).lower()

    def test_gvisor_without_any_runtime_raises_error(self):
        """gVisor without any runtime configured should raise error."""
        with pytest.raises(ValidationError) as exc:
            SecureRuntimeConfig(
                type="gvisor",
                docker_runtime=None,
                k8s_runtime_class=None,
            )

        assert "docker_runtime" in str(exc.value).lower() or "k8s_runtime_class" in str(exc.value).lower()

    def test_kata_without_any_runtime_raises_error(self):
        """Kata without any runtime configured should raise error."""
        with pytest.raises(ValidationError) as exc:
            SecureRuntimeConfig(
                type="kata",
                docker_runtime=None,
                k8s_runtime_class=None,
            )

        assert "docker_runtime" in str(exc.value).lower() or "k8s_runtime_class" in str(exc.value).lower()

    def test_invalid_type_raises_error(self):
        """Invalid type should raise ValidationError."""
        with pytest.raises(ValidationError):
            SecureRuntimeConfig(type="invalid_runtime")

    def test_app_config_with_secure_runtime(self):
        """AppConfig should parse secure_runtime section."""
        config = AppConfig(
            runtime={"type": "docker", "execd_image": "execd:v1"},
            secure_runtime={
                "type": "gvisor",
                "docker_runtime": "runsc",
                "k8s_runtime_class": "gvisor",
            },
        )
        assert config.secure_runtime is not None
        assert config.secure_runtime.type == "gvisor"
        assert config.secure_runtime.docker_runtime == "runsc"

    def test_app_config_without_secure_runtime(self):
        """AppConfig without secure_runtime should have None."""
        config = AppConfig(
            runtime={"type": "docker", "execd_image": "execd:v1"},
        )
        assert config.secure_runtime is None
EOF
```

**Step 2: Run the tests**

```bash
cd /Users/fengjianhui/WorkSpaceGithub/OpenSandbox && pytest tests/unit/test_config.py::TestSecureRuntimeConfig -v
```

Expected: All tests pass

**Step 3: Commit**

```bash
git add tests/unit/test_config.py
git commit -m "test(config): add unit tests for SecureRuntimeConfig validation

Test coverage:
- Empty type (default runc)
- gVisor with docker_runtime and/or k8s_runtime_class
- Kata with runtimes
- Firecracker K8s-only requirement
- Validation errors for missing runtime configuration
- AppConfig integration
"
```

---

### Task 1.5: Add startup validation to main.py

**Files:**
- Modify: `server/src/main.py`

**Context:** We need to call `validate_secure_runtime_on_startup` during server startup. This happens in the lifespan function.

**Step 1: Read main.py to find the lifespan function**

```bash
grep -n "async def lifespan" /Users/fengjianhui/WorkSpaceGithub/OpenSandbox/server/src/main.py
```

Expected: Find the line number of the lifespan function

**Step 2: Add import for runtime_resolver**

Add this import near the top of main.py with other service imports:

```python
from src.services.runtime_resolver import validate_secure_runtime_on_startup
```

**Step 3: Add validation to lifespan function**

In the lifespan function, after the existing startup code but before `yield`, add:

```python
# Validate secure runtime configuration at startup
try:
    await validate_secure_runtime_on_startup(
        config,
        docker_client=app.state.docker_service.docker_client if hasattr(app.state, 'docker_service') else None,
        k8s_client=app.state.k8s_service.client.api if hasattr(app.state, 'k8s_service') else None,
    )
except Exception as exc:
    logger.error("Secure runtime validation failed: %s", exc)
    raise
```

**Step 4: Verify no syntax errors**

```bash
cd /Users/fengjianhui/WorkSpaceGithub/OpenSandbox && python -c "from server.src.main import app; print('Import successful')"
```

Expected: `Import successful`

**Step 5: Commit**

```bash
git add server/src/main.py
git commit -m "feat(server): add secure runtime validation at startup

Call validate_secure_runtime_on_startup during server lifespan
to fail fast if configured runtime is not available.

- Validates Docker runtime exists in daemon.json
- Validates K8s RuntimeClass exists in cluster
- Logs clear error messages for misconfiguration
"
```

---

## Stage 2: Kubernetes Injection

### Task 2.1: Add runtimeClassName injection to BatchSandboxProvider

**Files:**
- Modify: `server/src/services/k8s/batchsandbox_provider.py`

**Context:** We need to inject `runtimeClassName` into the Pod spec when a secure runtime is configured.

**Step 1: Add import and initialize resolver in __init__**

First, find the `__init__` method line number:

```bash
grep -n "def __init__" /Users/fengjianhui/WorkSpaceGithub/OpenSandbox/server/src/services/k8s/batchsandbox_provider.py | head -1
```

Now add the import at the top of the file:

```python
from src.services.runtime_resolver import SecureRuntimeResolver
```

In the `__init__` method, after `self.template_manager = ...`, add:

```python
# Initialize secure runtime resolver
self.resolver = SecureRuntimeResolver(config) if config else None
self.runtime_class = self.resolver.get_k8s_runtime_class() if self.resolver else None
```

Note: You'll need to add a `config` parameter to `__init__` if it's not already there.

**Step 2: Inject runtimeClassName in create_workload**

Find the `create_workload` method and locate where `pod_spec` is built. Add the runtimeClassName injection:

```python
# In create_workload, after building pod_spec, add:
if self.runtime_class:
    pod_spec["runtimeClassName"] = self.runtime_class
```

**Step 3: Verify syntax**

```bash
cd /Users/fengjianhui/WorkSpaceGithub/OpenSandbox && python -c "from server.src.services.k8s.batchsandbox_provider import BatchSandboxProvider; print('Import successful')"
```

Expected: `Import successful`

**Step 4: Commit**

```bash
git add server/src/services/k8s/batchsandbox_provider.py
git commit -m "feat(k8s): inject runtimeClassName in BatchSandboxProvider

When secure_runtime is configured at server level, automatically inject
runtimeClassName into Pod specs created by BatchSandboxProvider.

- Initialize SecureRuntimeResolver in __init__
- Get k8s_runtime_class from config
- Inject runtimeClassName into pod_spec during create_workload
"
```

---

### Task 2.2: Add runtimeClassName injection to AgentSandboxProvider

**Files:**
- Modify: `server/src/services/k8s/agent_sandbox_provider.py`

**Step 1: Apply same changes as BatchSandboxProvider**

Add import:

```python
from src.services.runtime_resolver import SecureRuntimeResolver
```

Update `__init__`:

```python
# Initialize secure runtime resolver
self.resolver = SecureRuntimeResolver(config) if config else None
self.runtime_class = self.resolver.get_k8s_runtime_class() if self.resolver else None
```

Inject in create_workload:

```python
if self.runtime_class:
    pod_spec["runtimeClassName"] = self.runtime_class
```

**Step 2: Verify syntax**

```bash
cd /Users/fengjianhui/WorkSpaceGithub/OpenSandbox && python -c "from server.src.services.k8s.agent_sandbox_provider import AgentSandboxProvider; print('Import successful')"
```

Expected: `Import successful`

**Step 3: Commit**

```bash
git add server/src/services/k8s/agent_sandbox_provider.py
git commit -m "feat(k8s): inject runtimeClassName in AgentSandboxProvider

Apply same runtimeClassName injection logic to AgentSandboxProvider
for consistency with BatchSandboxProvider.
"
```

---

### Task 2.3: Update provider factory to pass config to providers

**Files:**
- Modify: `server/src/services/k8s/provider_factory.py` or wherever providers are instantiated

**Step 1: Find where providers are created**

```bash
grep -rn "BatchSandboxProvider(" /Users/fengjianhui/WorkSpaceGithub/OpenSandbox/server/src/
```

**Step 2: Update instantiation to pass config**

Where `BatchSandboxProvider` is created, add `config=config` parameter.

**Step 3: Commit**

```bash
git add server/src/services/k8s/provider_factory.py
git commit -m "feat(k8s): pass config to workload providers

Pass AppConfig to BatchSandboxProvider and AgentSandboxProvider
so they can initialize SecureRuntimeResolver.
"
```

---

## Stage 3: Docker Support

### Task 3.1: Add runtime parameter to DockerSandboxService

**Files:**
- Modify: `server/src/services/docker.py`

**Context:** We need to add the `runtime` parameter when creating containers.

**Step 1: Add import**

```python
from src.services.runtime_resolver import SecureRuntimeResolver
```

**Step 2: Initialize resolver in __init__**

In `DockerSandboxService.__init__`, after existing initialization:

```python
# Initialize secure runtime resolver
self.resolver = SecureRuntimeResolver(self.app_config)
self.docker_runtime = self.resolver.get_docker_runtime()
```

**Step 3: Add runtime to container creation**

In the `_create_and_start_container` method or wherever `docker_client.containers.run` is called, add the runtime parameter:

```python
# In container creation, add runtime parameter:
container_kwargs = {
    "image": image_uri,
    "runtime": self.docker_runtime,  # Add this
    # ... other parameters ...
}
```

**Step 4: Verify syntax**

```bash
cd /Users/fengjianhui/WorkSpaceGithub/OpenSandbox && python -c "from server.src.services.docker import DockerSandboxService; print('Import successful')"
```

Expected: `Import successful`

**Step 5: Commit**

```bash
git add server/src/services/docker.py
git commit -m "feat(docker): add --runtime parameter support

When secure_runtime is configured, pass the OCI runtime name
(e.g., 'runsc', 'kata-runtime') to Docker container creation.
"
```

---

## Stage 4: Testing & Documentation

### Task 4.1: Write integration tests for K8s runtime injection

**Files:**
- Create: `tests/integration/test_k8s_secure_runtime.py`

**Step 1: Create integration test file**

```bash
cat > /Users/fengjianhui/WorkSpaceGithub/OpenSandbox/tests/integration/test_k8s_secure_runtime.py << 'EOF'
# Copyright 2025 Alibaba Group Holding Ltd.
#
# Licensed under the Apache License, Version 2.0

"""Integration tests for K8s secure runtime support."""

import pytest
from kubernetes.client import ApiException

from src.config import AppConfig
from src.services.runtime_resolver import (
    ConfigError,
    SecureRuntimeResolver,
    validate_secure_runtime_on_startup,
)


@pytest.mark.asyncio
class TestK8sSecureRuntimeIntegration:
    """Integration tests for K8s secure runtime."""

    async def test_runtime_class_exists_validation_passes(self, mock_k8s_client):
        """Server validation passes when RuntimeClass exists."""
        # Setup: Mock RuntimeClass exists
        mock_k8s_client.read_runtime_class.return_value = {"metadata": {"name": "gvisor"}}

        config = AppConfig(
            runtime={"type": "kubernetes", "execd_image": "execd:v1"},
            secure_runtime={
                "type": "gvisor",
                "k8s_runtime_class": "gvisor",
            },
        )

        # Should not raise
        await validate_secure_runtime_on_startup(config, k8s_client=mock_k8s_client)

        mock_k8s_client.read_runtime_class.assert_called_once_with("gvisor")

    async def test_runtime_class_not_found_raises_error(self, mock_k8s_client):
        """Server validation fails when RuntimeClass doesn't exist."""
        # Setup: Mock RuntimeClass not found
        mock_k8s_client.read_runtime_class.side_effect = ApiException(status=404, reason="Not Found")

        config = AppConfig(
            runtime={"type": "kubernetes", "execd_image": "execd:v1"},
            secure_runtime={
                "type": "gvisor",
                "k8s_runtime_class": "nonexistent",
            },
        )

        with pytest.raises(ConfigError) as exc:
            await validate_secure_runtime_on_startup(config, k8s_client=mock_k8s_client)

        assert "does not exist" in str(exc.value)

    async def test_no_secure_runtime_skips_validation(self, mock_k8s_client):
        """No validation when secure_runtime not configured."""
        config = AppConfig(
            runtime={"type": "kubernetes", "execd_image": "execd:v1"},
            secure_runtime=None,
        )

        # Should not raise, should not call API
        await validate_secure_runtime_on_startup(config, k8s_client=mock_k8s_client)

        mock_k8s_client.read_runtime_class.assert_not_called()
EOF
```

**Step 2: Run tests**

```bash
cd /Users/fengjianhui/WorkSpaceGithub/OpenSandbox && pytest tests/integration/test_k8s_secure_runtime.py -v
```

Expected: Tests may fail if mocks not set up - fix as needed

**Step 3: Commit**

```bash
git add tests/integration/test_k8s_secure_runtime.py
git commit -m "test(k8s): add integration tests for secure runtime validation

Test coverage:
- RuntimeClass exists validation passes
- RuntimeClass not found raises ConfigError
- No secure runtime skips validation
"
```

---

### Task 4.2: Write integration tests for Docker runtime validation

**Files:**
- Create: `tests/integration/test_docker_secure_runtime.py`

**Step 1: Create integration test file**

```bash
cat > /Users/fengjianhui/WorkSpaceGithub/OpenSandbox/tests/integration/test_docker_secure_runtime.py << 'EOF'
# Copyright 2025 Alibaba Group Holding Ltd.
#
# Licensed under the Apache License, Version 2.0

"""Integration tests for Docker secure runtime support."""

import pytest

from src.config import AppConfig
from src.services.runtime_resolver import (
    ConfigError,
    validate_secure_runtime_on_startup,
)


@pytest.mark.asyncio
class TestDockerSecureRuntimeIntegration:
    """Integration tests for Docker secure runtime."""

    async def test_docker_runtime_available_validation_passes(self, mock_docker_client):
        """Server validation passes when Docker runtime exists."""
        # Setup: Mock runtime is available
        mock_docker_client.info.return_value = {
            "Runtimes": {
                "runc": {"path": "/usr/bin/runc"},
                "runsc": {"path": "/usr/bin/runsc"},
            }
        }

        config = AppConfig(
            runtime={"type": "docker", "execd_image": "execd:v1"},
            secure_runtime={
                "type": "gvisor",
                "docker_runtime": "runsc",
                "k8s_runtime_class": "gvisor",
            },
        )

        # Should not raise
        await validate_secure_runtime_on_startup(config, docker_client=mock_docker_client)

    async def test_docker_runtime_not_found_raises_error(self, mock_docker_client):
        """Server validation fails when Docker runtime doesn't exist."""
        # Setup: Mock runtime not available
        mock_docker_client.info.return_value = {
            "Runtimes": {
                "runc": {"path": "/usr/bin/runc"},
            }
        }

        config = AppConfig(
            runtime={"type": "docker", "execd_image": "execd:v1"},
            secure_runtime={
                "type": "gvisor",
                "docker_runtime": "runsc",
                "k8s_runtime_class": "gvisor",
            },
        )

        with pytest.raises(ConfigError) as exc:
            await validate_secure_runtime_on_startup(config, docker_client=mock_docker_client)

        assert "not available" in str(exc.value)
        assert "runsc" in str(exc.value)

    async def test_no_secure_runtime_skips_validation(self, mock_docker_client):
        """No validation when secure_runtime not configured."""
        config = AppConfig(
            runtime={"type": "docker", "execd_image": "execd:v1"},
            secure_runtime=None,
        )

        # Should not raise
        await validate_secure_runtime_on_startup(config, docker_client=mock_docker_client)
EOF
```

**Step 2: Run tests**

```bash
cd /Users/fengjianhui/WorkSpaceGithub/OpenSandbox && pytest tests/integration/test_docker_secure_runtime.py -v
```

**Step 3: Commit**

```bash
git add tests/integration/test_docker_secure_runtime.py
git commit -m "test(docker): add integration tests for secure runtime validation

Test coverage:
- Docker runtime available validation passes
- Docker runtime not found raises ConfigError
- No secure runtime skips validation
"
```

---

### Task 4.3: Update secure-container.md documentation

**Files:**
- Modify: `docs/secure-container.md`

**Context:** Update the documentation to reflect server-level configuration instead of Pool-only configuration.

**Step 1: Update the document to emphasize server-level config**

Key changes to make:
1. Update the User Guide section to show server-level configuration
2. Add examples of `~/.sandbox.toml` configuration
3. Keep Pool mode as an alternative/fallback
4. Add troubleshooting for server config issues

**Step 2: Commit**

```bash
git add docs/secure-container.md
git commit -m "docs(secure-container): update for server-level configuration

Update documentation to reflect OSEP-0004 server-level configuration:
- Add ~/.sandbox.toml examples
- Emphasize server config over Pool config
- Update troubleshooting section
"
```

---

### Task 4.4: Run all tests and verify

**Step 1: Run unit tests**

```bash
cd /Users/fengjianhui/WorkSpaceGithub/OpenSandbox && pytest tests/unit/ -v
```

Expected: All tests pass

**Step 2: Run integration tests**

```bash
cd /Users/fengjianhui/WorkSpaceGithub/OpenSandbox && pytest tests/integration/ -v
```

Expected: All tests pass

**Step 3: Verify no linting errors**

```bash
cd /Users/fengjianhui/WorkSpaceGithub/OpenSandbox && ruff check server/src/tests/
```

Expected: No errors (or fix any issues)

**Step 4: Final documentation commit**

```bash
git add docs/oseps/0004-secure-container-runtime.md
git commit -m "docs(osep): update OSEP-0004 status to implemented

Mark secure container runtime support as implemented with
reference to design and implementation docs.
"
```

---

## Verification Checklist

After completing all tasks, verify:

- [ ] All unit tests pass
- [ ] All integration tests pass
- [ ] Code follows project formatting/linting standards
- [ ] Documentation is updated
- [ ] Implementation matches design document
- [ ] OSEP-0004 requirements are met

---

## Rollback Plan

If issues arise:
1. Revert commits: `git revert <commit-range>`
2. Hotfix branch: `git checkout -b hotfix/secure-runtime`
3. Test fix, then merge back

---

## Next Steps After Implementation

1. Create PR: `git push origin feature/public-secure-container`
2. Request code review using `superpowers:requesting-code-review`
3. After approval, merge to main
4. Update OSEP-0004 status to `implemented`
