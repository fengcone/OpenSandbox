#!/bin/bash
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

set -e

# Script to commit running containers to snapshot images and push to registry.
# Supports multi-container atomic snapshots with backward compatibility.
#
# New format (multi-container):
#   commit-snapshot.sh <pod_name> <namespace> <container1:uri1> [<container2:uri2> ...]
#
# Legacy format (single-container, backward compatible):
#   commit-snapshot.sh <pod_name> <container_name> <namespace> <target_image>

# --- Containerd configuration ---
CONTAINERD_SOCKET="${CONTAINERD_SOCKET:-/var/run/containerd/containerd.sock}"
CRI_RUNTIME_ENDPOINT="${CRI_RUNTIME_ENDPOINT:-/run/k8s/containerd/containerd.sock}"
CONTAINERD_NAMESPACE="k8s.io"
CREDS_DIR="/var/run/opensandbox/registry"

# --- State tracking ---
# Associative arrays to track paused containers and committed images.
declare -A PAUSED_CONTAINERS=()
declare -A COMMITTED_IMAGES=()
declare -A CONTAINER_DIGESTS=()

# Ordered list of container names in processing order.
CONTAINER_ORDER=()

# --- Helper functions ---

# find_container_id discovers the container ID for a named container in a pod sandbox.
find_container_id() {
    local pod_sandbox_id="$1"
    local container_name="$2"
    local container_id

    container_id=$(crictl ps --pod "$pod_sandbox_id" --name "$container_name" -q | head -1)

    if [ -z "$container_id" ]; then
        echo "ERROR: Container $container_name not found in pod sandbox $pod_sandbox_id" >&2
        crictl ps --pod "$pod_sandbox_id" >&2
        return 1
    fi

    echo "$container_id"
}

# pause_container freezes a container task so containerd can commit the rootfs.
# Returns 0 on success, 1 on failure.
pause_container() {
    local container_id="$1"

    echo "Pausing container task $container_id..."
    if ctr --address "$CONTAINERD_SOCKET" --namespace "$CONTAINERD_NAMESPACE" tasks pause "$container_id" 2>&1; then
        echo "Task paused successfully."
        return 0
    else
        echo "WARNING: Failed to pause task $container_id (container may already be stopped or paused)."
        return 1
    fi
}

# resume_container resumes a previously paused container task.
resume_container() {
    local container_id="$1"

    echo "Resuming container task $container_id..."
    if ctr --address "$CONTAINERD_SOCKET" --namespace "$CONTAINERD_NAMESPACE" tasks resume "$container_id" 2>&1; then
        echo "Task resumed successfully."
    else
        echo "WARNING: Failed to resume task $container_id. The container may have been stopped externally."
    fi
}

# commit_container commits a container rootfs to a new image.
# Returns 0 on success, 1 on failure.
commit_container() {
    local container_id="$1"
    local target_image="$2"

    echo "Committing container $container_id to image $target_image..."
    if ctr --address "$CONTAINERD_SOCKET" --namespace "$CONTAINERD_NAMESPACE" containers commit "$container_id" "$target_image" 2>&1; then
        echo "Commit succeeded."
        return 0
    else
        echo "ERROR: Failed to commit container $container_id to $target_image."
        return 1
    fi
}

# push_image pushes a committed image to the registry and returns the digest.
push_image() {
    local target_image="$1"
    local push_opts=""

    # --- Registry credential handling ---
    if [ -f "$CREDS_DIR/config.json" ]; then
        echo "Found registry credentials at $CREDS_DIR/config.json"

        local registry_host
        registry_host=$(echo "$target_image" | cut -d'/' -f1)

        local auth
        auth=$(jq -r --arg host "$registry_host" '.auths[$host].auth // empty' "$CREDS_DIR/config.json" 2>/dev/null || echo "")

        if [ -n "$auth" ]; then
            local decoded
            decoded=$(echo "$auth" | base64 -d 2>/dev/null || echo "")
            if [ -n "$decoded" ]; then
                push_opts="--user $decoded"
            fi
        fi

        # Insecure/local registry detection
        if [[ "$registry_host" == *"local"* ]] || [[ "$registry_host" == *"localhost"* ]] || [[ "$registry_host" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+ ]]; then
            push_opts="$push_opts --plain-http"
        fi
    else
        echo "No registry credentials found, assuming insecure or pre-authenticated registry"
    fi

    echo "Pushing image $target_image..."
    if ctr --address "$CONTAINERD_SOCKET" --namespace "$CONTAINERD_NAMESPACE" images push $push_opts "$target_image" 2>&1; then
        return 0
    fi

    # Fallback: retry with plain-http
    echo "Push failed, trying with plain-http..."
    if ctr --address "$CONTAINERD_SOCKET" --namespace "$CONTAINERD_NAMESPACE" images push --plain-http "$target_image" 2>&1; then
        return 0
    fi

    echo "ERROR: Failed to push image $target_image to registry" >&2
    return 1
}

# extract_digest retrieves the digest for a committed image.
extract_digest() {
    local target_image="$1"
    local digest

    # Primary method: query image list
    digest=$(ctr --address "$CONTAINERD_SOCKET" --namespace "$CONTAINERD_NAMESPACE" images list --format json \
        | jq -r --arg img "$target_image" '.[] | select(.name==$img) | .digest' 2>/dev/null || echo "")

    if [ -z "$digest" ]; then
        # Fallback: query content list
        digest=$(ctr --address "$CONTAINERD_SOCKET" --namespace "$CONTAINERD_NAMESPACE" content list --format json \
            | jq -r --arg img "$target_image" '.[] | select(.labels.name==$img) | .digest' 2>/dev/null || echo "sha256:placeholder")
    fi

    echo "$digest"
}

# cleanup_paused resumes all tracked paused containers.
# Called on error via trap to guarantee no container is left paused.
cleanup_paused() {
    if [ ${#PAUSED_CONTAINERS[@]} -eq 0 ]; then
        return 0
    fi

    echo ""
    echo "=== Cleanup: Resuming all paused containers ==="
    for container_name in "${CONTAINER_ORDER[@]}"; do
        if [ "${PAUSED_CONTAINERS[$container_name]:-false}" = "true" ]; then
            local container_id="${CONTAINER_IDS[$container_name]:-}"
            if [ -n "$container_id" ]; then
                resume_container "$container_id"
            fi
            PAUSED_CONTAINERS["$container_name"]="false"
        fi
    done
}

# --- Argument parsing ---

POD_NAME=""
NAMESPACE=""
# CONTAINER_SPECS holds "container_name:target_image" pairs
CONTAINER_SPECS=()
# CONTAINER_IDS maps container_name -> container_id
declare -A CONTAINER_IDS=()

if [ $# -lt 3 ]; then
    echo "ERROR: Missing required parameters"
    echo "Usage (multi-container): $0 <pod_name> <namespace> <container1:uri1> [<container2:uri2> ...]"
    echo "Usage (legacy):          $0 <pod_name> <container_name> <namespace> <target_image>"
    exit 1
fi

# Detect old vs new format:
# Old: 4 args, arg2 (container_name) does NOT contain ':'
#   e.g. pod_name container_name namespace target_image
# New: 3+ args where arg3+ each contain ':'
#   e.g. pod_name namespace container1:uri1 [container2:uri2 ...]
if [ $# -eq 4 ] && [ "$2" = "${2#*:}" ]; then
    # Arg 2 has no ':' — legacy format: pod_name container_name namespace target_image
    echo "=== Detected legacy single-container format ==="
    POD_NAME="$1"
    local_container_name="$2"
    NAMESPACE="$3"
    local_target_image="$4"
    CONTAINER_SPECS=("${local_container_name}:${local_target_image}")
else
    # New format: pod_name namespace container1:uri1 [container2:uri2 ...]
    POD_NAME="$1"
    NAMESPACE="$2"
    shift 2
    CONTAINER_SPECS=("$@")
fi

# Validate
if [ -z "$POD_NAME" ]; then
    echo "ERROR: Pod name is required"
    exit 1
fi

if [ ${#CONTAINER_SPECS[@]} -eq 0 ]; then
    echo "ERROR: No container specifications provided"
    echo "Usage: $0 <pod_name> <namespace> <container1:uri1> [<container2:uri2> ...]"
    exit 1
fi

echo "=== Commit Snapshot Script (Multi-Container) ==="
echo "Pod: $POD_NAME"
echo "Namespace: $NAMESPACE"
echo "Container specs: ${CONTAINER_SPECS[*]}"
echo "Containerd Socket: $CONTAINERD_SOCKET"

# --- Set up trap to guarantee cleanup ---
trap cleanup_paused EXIT

# --- Step 1: Discover pod sandbox ---

echo ""
echo "=== Step 1: Find pod sandbox ==="

POD_SANDBOX_ID=$(crictl pods --name "$POD_NAME" --namespace "$NAMESPACE" -q | head -1)

if [ -z "$POD_SANDBOX_ID" ]; then
    echo "ERROR: Pod sandbox not found for $POD_NAME in namespace $NAMESPACE"
    crictl pods
    exit 1
fi

echo "Pod sandbox ID: $POD_SANDBOX_ID"

# --- Step 2: Parse container specs and find container IDs ---

echo ""
echo "=== Step 2: Find container IDs ==="

PARSE_ERRORS=0
for spec in "${CONTAINER_SPECS[@]}"; do
    # Split on first ':' to get container_name and target_image
    container_name="${spec%%:*}"
    target_image="${spec#*:}"

    if [ -z "$container_name" ] || [ -z "$target_image" ]; then
        echo "ERROR: Invalid container spec '$spec'. Expected format: container_name:target_image"
        PARSE_ERRORS=$((PARSE_ERRORS + 1))
        continue
    fi

    echo "Finding container '$container_name' (target: $target_image)..."
    container_id=$(find_container_id "$POD_SANDBOX_ID" "$container_name") || {
        echo "ERROR: Could not find container '$container_name'"
        PARSE_ERRORS=$((PARSE_ERRORS + 1))
        continue
    }

    # Verify container exists in containerd
    if ! ctr --address "$CONTAINERD_SOCKET" --namespace "$CONTAINERD_NAMESPACE" containers list | grep -q "$container_id"; then
        echo "ERROR: Container $container_id not found in containerd namespace $CONTAINERD_NAMESPACE"
        PARSE_ERRORS=$((PARSE_ERRORS + 1))
        continue
    fi

    echo "Container '$container_name' -> ID: $container_id"

    CONTAINER_IDS["$container_name"]="$container_id"
    CONTAINER_ORDER+=("$container_name")
    PAUSED_CONTAINERS["$container_name"]="false"
done

if [ $PARSE_ERRORS -gt 0 ]; then
    echo "ERROR: $PARSE_ERRORS container(s) could not be resolved. Aborting."
    exit 1
fi

if [ ${#CONTAINER_ORDER[@]} -eq 0 ]; then
    echo "ERROR: No valid containers to snapshot"
    exit 1
fi

# --- Step 3: Pause ALL containers ---

echo ""
echo "=== Step 3: Pause all containers ==="

PAUSE_ERRORS=0
for container_name in "${CONTAINER_ORDER[@]}"; do
    container_id="${CONTAINER_IDS[$container_name]}"

    if pause_container "$container_id"; then
        PAUSED_CONTAINERS["$container_name"]="true"
    else
        echo "WARNING: Could not pause '$container_name'. Will attempt commit anyway (container may be stopped)."
        PAUSE_ERRORS=$((PAUSE_ERRORS + 1))
    fi
done

# --- Step 4: Commit ALL containers ---

echo ""
echo "=== Step 4: Commit all containers ==="

COMMIT_ERRORS=0
for container_name in "${CONTAINER_ORDER[@]}"; do
    container_id="${CONTAINER_IDS[$container_name]}"

    # Extract target_image from the spec
    for spec in "${CONTAINER_SPECS[@]}"; do
        spec_name="${spec%%:*}"
        if [ "$spec_name" = "$container_name" ]; then
            target_image="${spec#*:}"
            break
        fi
    done

    if commit_container "$container_id" "$target_image"; then
        COMMITTED_IMAGES["$container_name"]="$target_image"
    else
        COMMIT_ERRORS=$((COMMIT_ERRORS + 1))
    fi
done

# --- Step 5: Resume ALL paused containers (ALWAYS, even if commit failed) ---

echo ""
echo "=== Step 5: Resume all paused containers ==="

for container_name in "${CONTAINER_ORDER[@]}"; do
    if [ "${PAUSED_CONTAINERS[$container_name]}" = "true" ]; then
        resume_container "${CONTAINER_IDS[$container_name]}"
        PAUSED_CONTAINERS["$container_name"]="false"
    fi
done

# Now that all containers are resumed, clear the trap so EXIT doesn't re-resume
trap - EXIT

# If any commits failed, exit with error after resuming
if [ $COMMIT_ERRORS -gt 0 ]; then
    echo "ERROR: $COMMIT_ERRORS container(s) failed to commit. All containers have been resumed."
    exit 1
fi

# --- Step 6: Push ALL committed images ---

echo ""
echo "=== Step 6: Push all images ==="

PUSH_ERRORS=0
for container_name in "${CONTAINER_ORDER[@]}"; do
    target_image="${COMMITTED_IMAGES[$container_name]}"

    if [ -z "$target_image" ]; then
        echo "ERROR: No committed image for container '$container_name'"
        PUSH_ERRORS=$((PUSH_ERRORS + 1))
        continue
    fi

    if push_image "$target_image"; then
        echo "Push succeeded for $target_image"
    else
        echo "ERROR: Failed to push image for container '$container_name'"
        PUSH_ERRORS=$((PUSH_ERRORS + 1))
    fi
done

if [ $PUSH_ERRORS -gt 0 ]; then
    echo "ERROR: $PUSH_ERRORS image(s) failed to push."
    exit 1
fi

# --- Step 7: Extract digests and output results ---

echo ""
echo "=== Step 7: Extract digests ==="

FIRST_DIGEST=""
for container_name in "${CONTAINER_ORDER[@]}"; do
    target_image="${COMMITTED_IMAGES[$container_name]}"

    digest=$(extract_digest "$target_image")
    CONTAINER_DIGESTS["$container_name"]="$digest"

    echo "Container '$container_name' digest: $digest"

    # Capture first digest for legacy output
    if [ -z "$FIRST_DIGEST" ]; then
        FIRST_DIGEST="$digest"
    fi
done

# --- Final output ---

echo ""
echo "=== Snapshot completed successfully ==="

for container_name in "${CONTAINER_ORDER[@]}"; do
    digest="${CONTAINER_DIGESTS[$container_name]}"
    target_image="${COMMITTED_IMAGES[$container_name]}"
    upper_name=$(echo "$container_name" | tr '[:lower:]-' '[:upper:]_')
    echo "SNAPSHOT_DIGEST_${upper_name}=${digest}"
    echo "  Image: $target_image"
    echo "  Digest: $digest"
done

# Legacy single-digest output for backward compatibility
echo "SNAPSHOT_DIGEST=${FIRST_DIGEST}"

exit 0
