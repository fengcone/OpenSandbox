#!/usr/bin/env bash
#
# test-pause-resume-e2e-fullchain.sh — Pause/Resume E2E full-chain test
#
# Tests the complete flow: SDK (HTTP/curl) -> Server REST API -> K8s Controller -> Commit Job
#
# Prerequisites:
#   - Kubernetes cluster running with OpenSandbox controller deployed
#   - Registry available in the namespace
#   - Server running and accessible
#
# Usage:
#   ./test-pause-resume-e2e-fullchain.sh [options]
#
# Options:
#   --server-url URL          Server base URL (default: http://localhost:8080)
#   --api-key KEY             API key for authentication (default: e2e-test)
#   --namespace NS            Kubernetes namespace (default: opensandbox)
#   --registry URL            Snapshot registry URL (default: sandbox-registry:8000/snapshots)
#   --secret SECRET           Push secret name (default: registry-secret)
#   --image IMAGE             Sandbox container image (default: busybox:latest)
#   --timeout SECONDS         Operation timeout in seconds (default: 600)
#   --skip-cleanup            Skip resource cleanup on exit
#   --verbose                 Enable verbose output
#   --help                    Show this help message
#

# ============================================================================
# Configuration defaults
# ============================================================================

SERVER_URL="http://localhost:8080"
API_KEY="e2e-test"
NAMESPACE="opensandbox"
REGISTRY="sandbox-registry:8000/snapshots"
SECRET_NAME="registry-secret"
SANDBOX_IMAGE="busybox:latest"
OPERATION_TIMEOUT=600
SKIP_CLEANUP="false"
VERBOSE="false"

# ============================================================================
# State tracking
# ============================================================================

SANDBOX_NAME=""
SANDBOX_ID=""
RESUMED_SANDBOX_ID=""
SNAPSHOT_NAME=""
PASSED_COUNT=0
FAILED_COUNT=0
TOTAL_COUNT=0

# ============================================================================
# Helper functions
# ============================================================================

log() {
    local timestamp
    timestamp=$(date +"%H:%M:%S")
    echo "[${timestamp}] $*"
}

log_verbose() {
    if [[ "$VERBOSE" == "true" ]]; then
        log "[VERBOSE] $*"
    fi
}

record_pass() {
    local test_name="$1"
    PASSED_COUNT=$((PASSED_COUNT + 1))
    TOTAL_COUNT=$((TOTAL_COUNT + 1))
    echo "[PASS] ${test_name}"
}

record_fail() {
    local test_name="$1"
    local detail="$2"
    FAILED_COUNT=$((FAILED_COUNT + 1))
    TOTAL_COUNT=$((TOTAL_COUNT + 1))
    echo "[FAIL] ${test_name} — ${detail}"
}

call_api() {
    local method="$1"
    local path="$2"
    shift 2
    curl -s -w "\n%{http_code}" -X "$method" \
        "${SERVER_URL}${path}" \
        -H "OPEN-SANDBOX-API-KEY: ${API_KEY}" \
        -H "Content-Type: application/json" \
        "$@"
}

wait_for_state() {
    local sandbox_identifier="$1"
    local target_state="$2"
    local max_wait="$3"
    local start_time
    start_time=$(date +%s)

    while true; do
        local elapsed
        elapsed=$(( $(date +%s) - start_time ))
        if [[ $elapsed -ge $max_wait ]]; then
            return 1
        fi

        local response
        response=$(call_api GET "/v1/sandboxes/${sandbox_identifier}")
        local http_code
        http_code=$(echo "$response" | tail -1)
        local body
        body=$(echo "$response" | sed '$d')

        if [[ "$http_code" != "200" ]]; then
            sleep 3
            continue
        fi

        local current_state
        current_state=$(echo "$body" | jq -r '.status.state // empty')
        log_verbose "Sandbox ${sandbox_identifier} state: ${current_state} (target: ${target_state}, elapsed: ${elapsed}s)"

        if [[ "$current_state" == "$target_state" ]]; then
            return 0
        fi

        sleep 3
    done
}

# ============================================================================
# parseArgs — Parse command-line arguments
# ============================================================================

parseArgs() {
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --server-url)
                SERVER_URL="$2"
                shift 2
                ;;
            --api-key)
                API_KEY="$2"
                shift 2
                ;;
            --namespace)
                NAMESPACE="$2"
                shift 2
                ;;
            --registry)
                REGISTRY="$2"
                shift 2
                ;;
            --secret)
                SECRET_NAME="$2"
                shift 2
                ;;
            --image)
                SANDBOX_IMAGE="$2"
                shift 2
                ;;
            --timeout)
                OPERATION_TIMEOUT="$2"
                shift 2
                ;;
            --skip-cleanup)
                SKIP_CLEANUP="true"
                shift
                ;;
            --verbose)
                VERBOSE="true"
                shift
                ;;
            --help)
                echo "Usage: $0 [options]"
                echo ""
                echo "Options:"
                echo "  --server-url URL     Server URL (default: http://localhost:8080)"
                echo "  --api-key KEY        API key (default: e2e-test)"
                echo "  --namespace NS       K8s namespace (default: opensandbox)"
                echo "  --registry URL       Snapshot registry (default: sandbox-registry:8000/snapshots)"
                echo "  --secret SECRET      Push secret name (default: registry-secret)"
                echo "  --image IMAGE        Sandbox image (default: busybox:latest)"
                echo "  --timeout SECONDS    Timeout (default: 600)"
                echo "  --skip-cleanup       Skip cleanup"
                echo "  --verbose            Verbose output"
                exit 0
                ;;
            *)
                log "Unknown argument: $1"
                exit 1
                ;;
        esac
    done
}

# ============================================================================
# checkServer — Verify server is accessible
# ============================================================================

checkServer() {
    log "Checking server at ${SERVER_URL} ..."

    local response
    response=$(curl -s -w "\n%{http_code}" "${SERVER_URL}/health" 2>/dev/null)
    local http_code
    http_code=$(echo "$response" | tail -1)

    if [[ "$http_code" == "200" ]]; then
        record_pass "checkServer"
        return 0
    else
        record_fail "checkServer" "Server health check returned HTTP ${http_code}"
        return 1
    fi
}

# ============================================================================
# testCreateSandbox — Create sandbox with pausePolicy
# ============================================================================

testCreateSandbox() {
    log "Creating sandbox with pausePolicy via Server API ..."

    SANDBOX_NAME="pause-resume-e2e-$(date +%s)"

    local create_payload
    create_payload=$(cat <<EOF
{
    "image": {"uri": "${SANDBOX_IMAGE}"},
    "entrypoint": ["sleep", "3600"],
    "metadata": {"test-e2e": "true"},
    "pausePolicy": {
        "snapshotType": "Rootfs",
        "snapshotRegistry": "${REGISTRY}",
        "snapshotPushSecretName": "${SECRET_NAME}"
    },
    "timeout": 1800
}
EOF
)

    log_verbose "Create payload: ${create_payload}"

    local response
    response=$(call_api POST "/v1/sandboxes" -d "$create_payload")
    local http_code
    http_code=$(echo "$response" | tail -1)
    local body
    body=$(echo "$response" | sed '$d')

    log_verbose "Create response (HTTP ${http_code}): ${body}"

    if [[ "$http_code" != "202" ]]; then
        record_fail "testCreateSandbox" "Expected HTTP 202, got ${http_code}: ${body}"
        return 1
    fi

    SANDBOX_ID=$(echo "$body" | jq -r '.id // empty')
    if [[ -z "$SANDBOX_ID" ]]; then
        record_fail "testCreateSandbox" "Response missing sandbox id: ${body}"
        return 1
    fi

    log_verbose "Sandbox created with id: ${SANDBOX_ID}, waiting for Running state ..."

    if ! wait_for_state "$SANDBOX_ID" "Running" "$OPERATION_TIMEOUT"; then
        record_fail "testCreateSandbox" "Sandbox did not reach Running state within ${OPERATION_TIMEOUT}s"
        return 1
    fi

    record_pass "testCreateSandbox"
    return 0
}

# ============================================================================
# testWriteData — Write test data into sandbox
# ============================================================================

testWriteData() {
    log "Writing test data into sandbox ${SANDBOX_ID} ..."

    # Use server proxy to execd's /command endpoint (SSE response)
    local exec_payload
    exec_payload='{"command": "echo test-pause-resume-data > /tmp/test-pause-resume.txt", "timeout": 30000}'

    local response
    response=$(call_api POST "/v1/sandboxes/${SANDBOX_ID}/proxy/44772/command" -d "$exec_payload")
    local http_code
    http_code=$(echo "$response" | tail -1)
    local body
    body=$(echo "$response" | sed '$d')

    log_verbose "Write data response (HTTP ${http_code}): ${body}"

    if [[ "$http_code" != "200" ]]; then
        record_fail "testWriteData" "Expected HTTP 200, got ${http_code}: ${body}"
        return 1
    fi

    record_pass "testWriteData"
    return 0
}

# ============================================================================
# testPauseSandbox — Pause the sandbox
# ============================================================================

testPauseSandbox() {
    log "Pausing sandbox ${SANDBOX_ID} ..."

    local response
    response=$(call_api POST "/v1/sandboxes/${SANDBOX_ID}/pause")
    local http_code
    http_code=$(echo "$response" | tail -1)
    local body
    body=$(echo "$response" | sed '$d')

    log_verbose "Pause response (HTTP ${http_code}): ${body}"

    if [[ "$http_code" != "202" ]]; then
        record_fail "testPauseSandbox" "Expected HTTP 202, got ${http_code}: ${body}"
        return 1
    fi

    record_pass "testPauseSandbox"
    return 0
}

# ============================================================================
# testWaitForSnapshot — Wait for SandboxSnapshot CR to appear and complete
# ============================================================================

testWaitForSnapshot() {
    log "Waiting for SandboxSnapshot CR to appear in namespace ${NAMESPACE} ..."

    local start_time
    start_time=$(date +%s)
    SNAPSHOT_NAME=""

    while true; do
        local elapsed
        elapsed=$(( $(date +%s) - start_time ))
        if [[ $elapsed -ge $OPERATION_TIMEOUT ]]; then
            record_fail "testWaitForSnapshot" "Timed out after ${OPERATION_TIMEOUT}s waiting for snapshot"
            return 1
        fi

        local snapshots_json
        snapshots_json=$(kubectl get sandboxsnapshots -n "$NAMESPACE" -o json 2>/dev/null)
        if [[ -n "$snapshots_json" ]]; then
            SNAPSHOT_NAME=$(echo "$snapshots_json" | jq -r ".items[] | select(.spec.sandboxId == \"${SANDBOX_ID}\") | .metadata.name" | head -1)
            if [[ -n "$SNAPSHOT_NAME" ]]; then
                log "Found SandboxSnapshot: ${SNAPSHOT_NAME}"
                break
            fi
        fi

        sleep 3
    done

    log "Waiting for snapshot ${SNAPSHOT_NAME} to reach Ready phase ..."

    while true; do
        local elapsed
        elapsed=$(( $(date +%s) - start_time ))
        if [[ $elapsed -ge $OPERATION_TIMEOUT ]]; then
            record_fail "testWaitForSnapshot" "Timed out waiting for snapshot ${SNAPSHOT_NAME} to become Ready"
            return 1
        fi

        local phase
        phase=$(kubectl get sandboxsnapshot "$SNAPSHOT_NAME" -n "$NAMESPACE" -o jsonpath='{.status.phase}' 2>/dev/null)
        log_verbose "Snapshot ${SNAPSHOT_NAME} phase: ${phase:-not set}"

        if [[ "$phase" == "Ready" ]]; then
            log "Snapshot ${SNAPSHOT_NAME} is Ready"
            break
        fi

        if [[ "$phase" == "Failed" ]]; then
            record_fail "testWaitForSnapshot" "Snapshot ${SNAPSHOT_NAME} entered Failed phase"
            return 1
        fi

        sleep 5
    done

    record_pass "testWaitForSnapshot"
    return 0
}

# ============================================================================
# testVerifySnapshotReady — Verify snapshot is Ready via kubectl
# ============================================================================

testVerifySnapshotReady() {
    log "Verifying snapshot ${SNAPSHOT_NAME} is Ready via kubectl ..."

    if [[ -z "$SNAPSHOT_NAME" ]]; then
        record_fail "testVerifySnapshotReady" "No snapshot name available"
        return 1
    fi

    local phase
    phase=$(kubectl get sandboxsnapshot "$SNAPSHOT_NAME" -n "$NAMESPACE" -o jsonpath='{.status.phase}' 2>/dev/null)

    if [[ "$phase" != "Ready" ]]; then
        record_fail "testVerifySnapshotReady" "Expected phase Ready, got: ${phase:-not found}"
        return 1
    fi

    local image_uri
    image_uri=$(kubectl get sandboxsnapshot "$SNAPSHOT_NAME" -n "$NAMESPACE" -o jsonpath='{.spec.containerSnapshots[0].imageURI}' 2>/dev/null)
    log "Snapshot image URI: ${image_uri:-not set}"

    record_pass "testVerifySnapshotReady"
    return 0
}

# ============================================================================
# testDeleteSandbox — Delete original sandbox (release compute)
# ============================================================================

testDeleteSandbox() {
    log "Deleting sandbox ${SANDBOX_ID} to release compute ..."

    local response
    response=$(call_api DELETE "/v1/sandboxes/${SANDBOX_ID}")
    local http_code
    http_code=$(echo "$response" | tail -1)
    local body
    body=$(echo "$response" | sed '$d')

    log_verbose "Delete response (HTTP ${http_code}): ${body}"

    if [[ "$http_code" != "204" ]]; then
        record_fail "testDeleteSandbox" "Expected HTTP 204, got ${http_code}: ${body}"
        return 1
    fi

    record_pass "testDeleteSandbox"
    return 0
}

# ============================================================================
# testResumeSandbox — Resume sandbox from snapshot
# ============================================================================

testResumeSandbox() {
    log "Resuming sandbox from snapshot ${SNAPSHOT_NAME} ..."

    local snapshot_image
    snapshot_image=$(kubectl get sandboxsnapshot "$SNAPSHOT_NAME" -n "$NAMESPACE" -o jsonpath='{.spec.containerSnapshots[0].imageURI}' 2>/dev/null)

    if [[ -z "$snapshot_image" ]]; then
        record_fail "testResumeSandbox" "Could not get snapshot image URI"
        return 1
    fi

    log "Snapshot image: ${snapshot_image}"

    local resume_payload
    resume_payload=$(cat <<EOF
{
    "image": {"uri": "${snapshot_image}"},
    "entrypoint": ["sleep", "3600"],
    "metadata": {"test-e2e": "true", "resumed-from": "${SNAPSHOT_NAME}"},
    "pausePolicy": {
        "snapshotType": "Rootfs",
        "snapshotRegistry": "${REGISTRY}",
        "snapshotPushSecretName": "${SECRET_NAME}"
    },
    "timeout": 1800
}
EOF
)

    log_verbose "Resume payload: ${resume_payload}"

    local response
    response=$(call_api POST "/v1/sandboxes/${SANDBOX_ID}/resume" -d "$resume_payload")
    local http_code
    http_code=$(echo "$response" | tail -1)
    local body
    body=$(echo "$response" | sed '$d')

    log_verbose "Resume response (HTTP ${http_code}): ${body}"

    if [[ "$http_code" != "202" ]]; then
        record_fail "testResumeSandbox" "Expected HTTP 202, got ${http_code}: ${body}"
        return 1
    fi

    RESUMED_SANDBOX_ID=$(echo "$body" | jq -r '.id // empty')
    if [[ -n "$RESUMED_SANDBOX_ID" ]]; then
        log "Resumed as sandbox: ${RESUMED_SANDBOX_ID}"
    else
        RESUMED_SANDBOX_ID="$SANDBOX_ID"
        log "Resume returned same sandbox id: ${SANDBOX_ID}"
    fi

    record_pass "testResumeSandbox"
    return 0
}

# ============================================================================
# testWaitForResumed — Wait for resumed sandbox to reach Running
# ============================================================================

testWaitForResumed() {
    log "Waiting for resumed sandbox ${RESUMED_SANDBOX_ID} to reach Running ..."

    if ! wait_for_state "$RESUMED_SANDBOX_ID" "Running" "$OPERATION_TIMEOUT"; then
        record_fail "testWaitForResumed" "Resumed sandbox did not reach Running within ${OPERATION_TIMEOUT}s"
        return 1
    fi

    record_pass "testWaitForResumed"
    return 0
}

# ============================================================================
# testVerifyDataPersistence — Verify data persisted after resume
# ============================================================================

testVerifyDataPersistence() {
    log "Verifying data persistence in resumed sandbox ${RESUMED_SANDBOX_ID} ..."

    # Use server proxy to execd's /command endpoint (SSE response)
    local exec_payload
    exec_payload='{"command": "cat /tmp/test-pause-resume.txt", "timeout": 30000}'

    local response
    response=$(call_api POST "/v1/sandboxes/${RESUMED_SANDBOX_ID}/proxy/44772/command" -d "$exec_payload")
    local http_code
    http_code=$(echo "$response" | tail -1)
    local body
    body=$(echo "$response" | sed '$d')

    log_verbose "Data verify response (HTTP ${http_code}): ${body}"

    if [[ "$http_code" != "200" ]]; then
        record_fail "testVerifyDataPersistence" "Expected HTTP 200, got ${http_code}: ${body}"
        return 1
    fi

    # execd returns raw NDJSON lines: {"type":"stdout","text":"..."}
    local stdout_content
    stdout_content=$(echo "$body" | grep '^{' | jq -r 'select(.type == "stdout" and .text != null) | .text' 2>/dev/null | tr -d '\n')
    log_verbose "File content: ${stdout_content}"

    if [[ "$stdout_content" != *"test-pause-resume-data"* ]]; then
        record_fail "testVerifyDataPersistence" "Expected 'test-pause-resume-data', got: ${stdout_content:-empty}"
        return 1
    fi

    log "Data persisted correctly: ${stdout_content}"
    record_pass "testVerifyDataPersistence"
    return 0
}

# ============================================================================
# testSecondPauseCycle — Second pause/resume cycle
# ============================================================================

testSecondPauseCycle() {
    log "Starting second pause cycle on sandbox ${RESUMED_SANDBOX_ID} ..."

    local pause_response
    pause_response=$(call_api POST "/v1/sandboxes/${RESUMED_SANDBOX_ID}/pause")
    local pause_http_code
    pause_http_code=$(echo "$pause_response" | tail -1)
    local pause_body
    pause_body=$(echo "$pause_response" | sed '$d')

    log_verbose "Second pause response (HTTP ${pause_http_code}): ${pause_body}"

    if [[ "$pause_http_code" != "202" ]]; then
        record_fail "testSecondPauseCycle (pause)" "Expected HTTP 202, got ${pause_http_code}: ${pause_body}"
        return 1
    fi

    log "Second pause accepted, waiting for snapshot Ready ..."

    local start_time
    start_time=$(date +%s)
    local second_snapshot_name=""

    while true; do
        local elapsed
        elapsed=$(( $(date +%s) - start_time ))
        if [[ $elapsed -ge $OPERATION_TIMEOUT ]]; then
            record_fail "testSecondPauseCycle (snapshot)" "Timed out waiting for second snapshot"
            return 1
        fi

        local snapshots_json
        snapshots_json=$(kubectl get sandboxsnapshots -n "$NAMESPACE" -o json 2>/dev/null)
        if [[ -n "$snapshots_json" ]]; then
            local newest_snapshot
            newest_snapshot=$(echo "$snapshots_json" | jq -r "[.items[] | select(.spec.sandboxId == \"${RESUMED_SANDBOX_ID}\")] | sort_by(.metadata.creationTimestamp) | last | .metadata.name // empty" 2>/dev/null)
            if [[ -n "$newest_snapshot" && "$newest_snapshot" != "$SNAPSHOT_NAME" ]]; then
                second_snapshot_name="$newest_snapshot"
                break
            fi
            if [[ -n "$newest_snapshot" ]]; then
                local phase
                phase=$(kubectl get sandboxsnapshot "$newest_snapshot" -n "$NAMESPACE" -o jsonpath='{.status.phase}' 2>/dev/null)
                if [[ "$phase" == "Ready" ]]; then
                    second_snapshot_name="$newest_snapshot"
                    break
                fi
            fi
        fi

        sleep 5
    done

    log "Second snapshot found: ${second_snapshot_name}, waiting for Ready ..."

    while true; do
        local elapsed
        elapsed=$(( $(date +%s) - start_time ))
        if [[ $elapsed -ge $OPERATION_TIMEOUT ]]; then
            record_fail "testSecondPauseCycle (snapshot-ready)" "Timed out waiting for second snapshot Ready"
            return 1
        fi

        local phase
        phase=$(kubectl get sandboxsnapshot "$second_snapshot_name" -n "$NAMESPACE" -o jsonpath='{.status.phase}' 2>/dev/null)

        if [[ "$phase" == "Ready" ]]; then
            log "Second snapshot ${second_snapshot_name} is Ready"
            break
        fi

        if [[ "$phase" == "Failed" ]]; then
            record_fail "testSecondPauseCycle (snapshot-ready)" "Second snapshot entered Failed phase"
            return 1
        fi

        sleep 5
    done

    SNAPSHOT_NAME="$second_snapshot_name"

    local resume_payload
    resume_payload=$(cat <<EOF
{
    "image": {"uri": "${REGISTRY}/${second_snapshot_name}"},
    "entrypoint": ["sleep", "3600"],
    "metadata": {"test-e2e": "true", "resumed-from": "${second_snapshot_name}", "cycle": "second"},
    "pausePolicy": {
        "snapshotType": "Rootfs",
        "snapshotRegistry": "${REGISTRY}",
        "snapshotPushSecretName": "${SECRET_NAME}"
    },
    "timeout": 1800
}
EOF
)

    local resume_response
    resume_response=$(call_api POST "/v1/sandboxes/${RESUMED_SANDBOX_ID}/resume" -d "$resume_payload")
    local resume_http_code
    resume_http_code=$(echo "$resume_response" | tail -1)
    local resume_body
    resume_body=$(echo "$resume_response" | sed '$d')

    log_verbose "Second resume response (HTTP ${resume_http_code}): ${resume_body}"

    if [[ "$resume_http_code" != "202" ]]; then
        record_fail "testSecondPauseCycle (resume)" "Expected HTTP 202, got ${resume_http_code}: ${resume_body}"
        return 1
    fi

    if ! wait_for_state "$RESUMED_SANDBOX_ID" "Running" "$OPERATION_TIMEOUT"; then
        record_fail "testSecondPauseCycle (wait-running)" "Sandbox did not reach Running after second resume"
        return 1
    fi

    record_pass "testSecondPauseCycle"
    return 0
}

# ============================================================================
# testDeleteAndWait — Delete sandbox and wait for cleanup
# ============================================================================

testDeleteAndWait() {
    local target_id="${RESUMED_SANDBOX_ID:-$SANDBOX_ID}"
    if [[ -z "$target_id" ]]; then
        record_fail "testDeleteAndWait" "No sandbox id to delete"
        return 1
    fi

    log "Deleting sandbox ${target_id} and waiting for cleanup ..."

    local response
    response=$(call_api DELETE "/v1/sandboxes/${target_id}")
    local http_code
    http_code=$(echo "$response" | tail -1)

    if [[ "$http_code" != "204" ]]; then
        record_fail "testDeleteAndWait" "Expected HTTP 204, got ${http_code}"
        return 1
    fi

    log "Delete accepted (204), waiting for sandbox to be removed ..."

    local start_time
    start_time=$(date +%s)

    while true; do
        local elapsed
        elapsed=$(( $(date +%s) - start_time ))
        if [[ $elapsed -ge 60 ]]; then
            log_verbose "Timeout waiting for sandbox removal, continuing"
            break
        fi

        local check_response
        check_response=$(call_api GET "/v1/sandboxes/${target_id}")
        local check_code
        check_code=$(echo "$check_response" | tail -1)

        if [[ "$check_code" == "404" ]]; then
            log "Sandbox ${target_id} removed"
            break
        fi

        sleep 3
    done

    record_pass "testDeleteAndWait"
    return 0
}

# ============================================================================
# cleanup — Remove all K8s resources
# ============================================================================

cleanup() {
    if [[ "$SKIP_CLEANUP" == "true" ]]; then
        log "Skipping cleanup (--skip-cleanup)"
        return 0
    fi

    log "Cleaning up resources in namespace ${NAMESPACE} ..."

    if [[ -n "$SANDBOX_ID" ]]; then
        call_api DELETE "/v1/sandboxes/${SANDBOX_ID}" > /dev/null 2>&1
    fi

    if [[ -n "$RESUMED_SANDBOX_ID" && "$RESUMED_SANDBOX_ID" != "$SANDBOX_ID" ]]; then
        call_api DELETE "/v1/sandboxes/${RESUMED_SANDBOX_ID}" > /dev/null 2>&1
    fi

    if [[ -n "$SANDBOX_NAME" ]]; then
        kubectl delete batchsandbox "$SANDBOX_NAME" -n "$NAMESPACE" --ignore-not-found=true > /dev/null 2>&1
    fi

    kubectl delete sandboxsnapshots -n "$NAMESPACE" -l "test-e2e=true" --ignore-not-found=true > /dev/null 2>&1

    local snapshot_labels
    snapshot_labels=$(kubectl get sandboxsnapshots -n "$NAMESPACE" -o json 2>/dev/null | jq -r ".items[] | select(.spec.sandboxId | test(\"^pause-resume-e2e\")) | .metadata.name" 2>/dev/null)
    if [[ -n "$snapshot_labels" ]]; then
        echo "$snapshot_labels" | while read -r snap_name; do
            kubectl delete sandboxsnapshot "$snap_name" -n "$NAMESPACE" --ignore-not-found=true > /dev/null 2>&1
        done
    fi

    log "Cleanup completed"
    return 0
}

# ============================================================================
# printSummary — Print test results
# ============================================================================

printSummary() {
    echo ""
    echo "============================================"
    echo "  Pause/Resume E2E Full-Chain Test Summary"
    echo "============================================"
    echo ""
    echo "  Server:      ${SERVER_URL}"
    echo "  Namespace:   ${NAMESPACE}"
    echo "  Registry:    ${REGISTRY}"
    echo "  Image:       ${SANDBOX_IMAGE}"
    echo "  Sandbox:     ${SANDBOX_ID}"
    echo "  Snapshot:    ${SNAPSHOT_NAME:-N/A}"
    echo ""
    echo "  Total:  ${TOTAL_COUNT}"
    echo "  Passed: ${PASSED_COUNT}"
    echo "  Failed: ${FAILED_COUNT}"
    echo ""

    if [[ "$FAILED_COUNT" -eq 0 ]]; then
        echo "  Result: ALL PASSED"
        echo "============================================"
        return 0
    else
        echo "  Result: ${FAILED_COUNT} FAILED"
        echo "============================================"
        return 1
    fi
}

# ============================================================================
# main — Entry point
# ============================================================================

main() {
    parseArgs "$@"

    echo "============================================"
    echo "  Pause/Resume E2E Full-Chain Test"
    echo "============================================"
    echo ""

    checkServer
    if [[ $? -ne 0 ]]; then
        printSummary
        exit 1
    fi

    testCreateSandbox
    if [[ $? -ne 0 ]]; then
        cleanup
        printSummary
        exit 1
    fi

    testWriteData
    if [[ $? -ne 0 ]]; then
        cleanup
        printSummary
        exit 1
    fi

    testPauseSandbox
    if [[ $? -ne 0 ]]; then
        cleanup
        printSummary
        exit 1
    fi

    testWaitForSnapshot
    if [[ $? -ne 0 ]]; then
        cleanup
        printSummary
        exit 1
    fi

    testVerifySnapshotReady
    if [[ $? -ne 0 ]]; then
        cleanup
        printSummary
        exit 1
    fi

    testDeleteSandbox
    if [[ $? -ne 0 ]]; then
        cleanup
        printSummary
        exit 1
    fi

    testResumeSandbox
    if [[ $? -ne 0 ]]; then
        cleanup
        printSummary
        exit 1
    fi

    testWaitForResumed
    if [[ $? -ne 0 ]]; then
        cleanup
        printSummary
        exit 1
    fi

    testVerifyDataPersistence
    if [[ $? -ne 0 ]]; then
        cleanup
        printSummary
        exit 1
    fi

    testSecondPauseCycle
    if [[ $? -ne 0 ]]; then
        cleanup
        printSummary
        exit 1
    fi

    testDeleteAndWait

    cleanup

    printSummary
    exit $?
}

main "$@"
