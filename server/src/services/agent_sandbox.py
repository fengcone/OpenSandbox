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
Agent-sandbox runtime implementation for OpenSandbox.
"""

import logging
from typing import Any, Optional

from fastapi import HTTPException, status

from src.api.schema import Sandbox, SandboxStatus
from src.config import AppConfig, AgentSandboxRuntimeConfig, get_config
from src.services.constants import SANDBOX_ID_LABEL, SandboxErrorCodes
from src.services.k8s.client import K8sClient
from src.services.k8s.agent_sandbox_provider import AgentSandboxProvider
from src.services.k8s.kubernetes_service import KubernetesSandboxService

logger = logging.getLogger(__name__)


class AgentSandboxService(KubernetesSandboxService):
    """
    Agent-sandbox-based implementation of SandboxService.
    """

    def __init__(self, config: Optional[AppConfig] = None):
        self.app_config = config or get_config()
        runtime_config = self.app_config.runtime

        if runtime_config.type != "agent-sandbox":
            raise ValueError("AgentSandboxService requires runtime.type = 'agent-sandbox'")

        if not self.app_config.kubernetes:
            raise ValueError("Kubernetes configuration is required")

        self.namespace = self.app_config.kubernetes.namespace
        self.execd_image = runtime_config.execd_image
        self.service_account = self.app_config.kubernetes.service_account

        agent_config = self.app_config.agent_sandbox or AgentSandboxRuntimeConfig()

        try:
            self.k8s_client = K8sClient(self.app_config.kubernetes)
            logger.info("Kubernetes client initialized successfully")
        except Exception as e:
            logger.error("Failed to initialize Kubernetes client: %s", e)
            raise HTTPException(
                status_code=status.HTTP_503_SERVICE_UNAVAILABLE,
                detail={
                    "code": SandboxErrorCodes.K8S_INITIALIZATION_ERROR,
                    "message": f"Failed to initialize Kubernetes client: {str(e)}",
                },
            ) from e

        self.workload_provider = AgentSandboxProvider(
            k8s_client=self.k8s_client,
            template_file_path=agent_config.template_file,
            execd_mode=agent_config.execd_mode,
            shutdown_policy=agent_config.shutdown_policy,
            service_account=self.service_account,
        )

        logger.info(
            "AgentSandboxService initialized: namespace=%s, execd_image=%s, execd_mode=%s",
            self.namespace,
            self.execd_image,
            agent_config.execd_mode,
        )

    def _build_sandbox_from_workload(self, workload: Any) -> Sandbox:
        if isinstance(workload, dict):
            metadata = workload.get("metadata", {})
            spec = workload.get("spec", {})
            labels = metadata.get("labels", {})
            creation_timestamp = metadata.get("creationTimestamp")
        else:
            metadata = workload.metadata
            spec = workload.spec
            labels = metadata.labels or {}
            creation_timestamp = metadata.creation_timestamp

        sandbox_id = labels.get(SANDBOX_ID_LABEL, "")
        expires_at = self.workload_provider.get_expiration(workload)
        status_info = self.workload_provider.get_status(workload)

        user_metadata = {
            k: v for k, v in labels.items()
            if not k.startswith("opensandbox.io/")
        }

        image_uri = ""
        entrypoint = []

        if isinstance(workload, dict):
            template = spec.get("podTemplate", {})
            pod_spec = template.get("spec", {})
            containers = pod_spec.get("containers", [])
            if containers:
                container = containers[0]
                image_uri = container.get("image", "")
                entrypoint = container.get("command", [])
        else:
            if hasattr(spec, "podTemplate") and spec.podTemplate:
                containers = spec.podTemplate.spec.containers
                if containers:
                    container = containers[0]
                    image_uri = container.image or ""
                    entrypoint = container.command or []

        from src.api.schema import ImageSpec

        image_spec = ImageSpec(uri=image_uri) if image_uri else ImageSpec(uri="unknown")

        return Sandbox(
            id=sandbox_id,
            status=SandboxStatus(
                state=status_info["state"],
                reason=status_info["reason"],
                message=status_info["message"],
                last_transition_at=status_info["last_transition_at"],
            ),
            created_at=creation_timestamp,
            expires_at=expires_at,
            metadata=user_metadata if user_metadata else None,
            image=image_spec,
            entrypoint=entrypoint,
        )
