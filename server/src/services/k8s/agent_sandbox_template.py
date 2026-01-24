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
Agent-sandbox template loader and merger.

Provides functionality to load Sandbox CR templates from YAML files
and merge them with runtime-generated values.
"""

import logging
from pathlib import Path
from typing import Any, Dict, Optional

import yaml

logger = logging.getLogger(__name__)


class AgentSandboxTemplateManager:
    """
    Manager for agent-sandbox Sandbox CR templates.

    Loads templates from YAML files and provides deep merge functionality
    to combine template fields with runtime-generated fields.
    """

    def __init__(self, template_file_path: Optional[str] = None):
        """
        Initialize template manager.

        Args:
            template_file_path: Optional path to Sandbox CR YAML template file

        Raises:
            FileNotFoundError: If template file path is provided but file doesn't exist
            ValueError: If template file is not a valid YAML object
            RuntimeError: If template file cannot be loaded
        """
        self.template_file_path = template_file_path
        self._template: Optional[Dict[str, Any]] = None

        if template_file_path:
            self._load_template()

    def _load_template(self) -> None:
        """
        Load Sandbox template from YAML file.

        Raises:
            FileNotFoundError: If template file doesn't exist
            ValueError: If template is not a valid YAML object (dict)
            RuntimeError: If template cannot be loaded for other reasons
        """
        if not self.template_file_path:
            return

        template_path = Path(self.template_file_path).expanduser()

        if not template_path.exists():
            raise FileNotFoundError(
                f"Agent-sandbox template file not found: {template_path}"
            )

        try:
            with template_path.open("r") as f:
                self._template = yaml.safe_load(f)

            if not isinstance(self._template, dict):
                raise ValueError(
                    f"Invalid template file {template_path}: must be a YAML object, "
                    f"got {type(self._template).__name__}"
                )

            logger.info("Loaded agent-sandbox template from %s", template_path)
        except (FileNotFoundError, ValueError):
            raise
        except Exception as e:
            raise RuntimeError(
                f"Failed to load agent-sandbox template from {template_path}: {e}"
            ) from e

    def get_base_template(self) -> Dict[str, Any]:
        """
        Get base Sandbox template.

        Returns a deep copy of the loaded template, or an empty dict if no template is loaded.

        Returns:
            Dict containing the base template structure
        """
        if self._template:
            return self._deep_copy(self._template)
        return {}

    def merge_with_runtime_values(self, runtime_manifest: Dict[str, Any]) -> Dict[str, Any]:
        """
        Merge template with runtime-generated Sandbox manifest.

        The merge strategy is:
        - Start with the template as base
        - Deep merge runtime values on top
        - Runtime values override template values at leaf level
        - Lists are replaced (not merged)

        Args:
            runtime_manifest: Runtime-generated Sandbox manifest

        Returns:
            Merged Sandbox manifest
        """
        base = self.get_base_template()

        if not base:
            return runtime_manifest

        return self._deep_merge(base, runtime_manifest)

    @staticmethod
    def _deep_copy(obj: Any) -> Any:
        if isinstance(obj, dict):
            return {k: AgentSandboxTemplateManager._deep_copy(v) for k, v in obj.items()}
        if isinstance(obj, list):
            return [AgentSandboxTemplateManager._deep_copy(item) for item in obj]
        return obj

    @staticmethod
    def _deep_merge(base: Dict[str, Any], override: Dict[str, Any]) -> Dict[str, Any]:
        result = base.copy()

        for key, override_value in override.items():
            if override_value is None:
                continue

            if key not in result:
                result[key] = AgentSandboxTemplateManager._deep_copy(override_value)
            elif isinstance(result[key], dict) and isinstance(override_value, dict):
                result[key] = AgentSandboxTemplateManager._deep_merge(result[key], override_value)
            else:
                result[key] = AgentSandboxTemplateManager._deep_copy(override_value)

        return result
