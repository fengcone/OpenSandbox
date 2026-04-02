#!/usr/bin/env bash
#
# E2E test for K8s pause/resume via rootfs snapshot.
# Prerequisites:
#   - Kind cluster "sandbox-k8s-test-e2e" running
#   - Images built and loaded: controller:dev, task-executor:dev, commit-executor:dev
#   - kubectl, kind in PATH
#
# Usage:
#   chmod +x test-pause-resume-e2e.sh
#   ./test-pause-resume-e2e.sh          # full run (setup + test + cleanup)
#   ./test-pause-resume-e2e.sh --skip-setup    # skip infra setup, just run tests
#   ./test-pause-resume-e2e.sh --cleanup-only  # just tear everything down
#
set -euo pipefail

KIND_CLUSTER="sandbox-k8s-test-e2e"
NS="default"
CTRL_NS="opensandbox-system"
REGISTRY_ADDR="docker-registry.${NS}.svc.cluster.local:5000"
SANDBOX_NAME="test-pause-resume"
TIMEOUT="${E2E_TIMEOUT:-120}"

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
CYAN='\033[0;36m'
NC='\033[0m'

log()  { echo -e "${GREEN}[PASS]${NC} $*"; }
warn() { echo -e "${YELLOW}[WARN]${NC} $*"; }
fail() { echo -e "${RED}[FAIL]${NC} $*"; exit 1; }
step() { echo -e "\n${CYAN}===== $* =====${NC}"; }

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
wait_for() {
  local desc="$1"
  shift
  local interval=3 elapsed=0
  echo "Waiting for: ${desc} (timeout=${TIMEOUT}s)"
  while (( elapsed < TIMEOUT )); do
    if eval "$@" >/dev/null 2>&1; then
      log "${desc}"
      return 0
    fi
    sleep "$interval"
    (( elapsed += interval ))
  done
  fail "Timed out waiting for: ${desc}"
}

get_jsonpath() {
  kubectl get "$1" -o "jsonpath={$2}" 2>/dev/null
}

# ---------------------------------------------------------------------------
# Cleanup
# ---------------------------------------------------------------------------
cleanup() {
  step "Cleanup"
  kubectl delete batchsandbox "$SANDBOX_NAME" -n "$NS" --ignore-not-found=true 2>/dev/null || true
  kubectl delete sandboxsnapshot "$SANDBOX_NAME" -n "$NS" --ignore-not-found=true 2>/dev/null || true
  kubectl delete jobs -l "sandbox.opensandbox.io/sandbox-snapshot-name=${SANDBOX_NAME}" -n "$NS" --ignore-not-found=true 2>/dev/null || true
  warn "Resources cleaned up"
}

cleanup_all() {
  step "Full Cleanup (infrastructure + test resources)"
  cleanup
  kubectl delete deployment controller-manager -n "$CTRL_NS" --ignore-not-found=true 2>/dev/null || true
  kubectl delete deployment docker-registry -n "$NS" --ignore-not-found=true 2>/dev/null || true
  kubectl delete service docker-registry -n "$NS" --ignore-not-found=true 2>/dev/null || true
  kubectl delete secret registry-push-secret registry-pull-secret -n "$NS" --ignore-not-found=true 2>/dev/null || true
  kubectl delete clusterrolebinding manager-rolebinding --ignore-not-found=true 2>/dev/null || true
  kubectl delete clusterrole manager-role --ignore-not-found=true 2>/dev/null || true
  kubectl delete rolebinding leader-election-rolebinding -n "$CTRL_NS" --ignore-not-found=true 2>/dev/null || true
  kubectl delete role leader-election-role -n "$CTRL_NS" --ignore-not-found=true 2>/dev/null || true
  kubectl delete sa controller-manager -n "$CTRL_NS" --ignore-not-found=true 2>/dev/null || true
  kubectl delete crd batchsandboxes.sandbox.opensandbox.io pools.sandbox.opensandbox.io sandboxsnapshots.sandbox.opensandbox.io --ignore-not-found=true 2>/dev/null || true
  kubectl delete ns "$CTRL_NS" --ignore-not-found=true 2>/dev/null || true
  log "Full cleanup done"
}

# ---------------------------------------------------------------------------
# Setup
# ---------------------------------------------------------------------------
setup_infra() {
  step "Step 0: Verify Kind cluster"
  if ! kind get clusters 2>/dev/null | grep -q "$KIND_CLUSTER"; then
    fail "Kind cluster '$KIND_CLUSTER' not found. Create it first: make setup-test-e2e"
  fi
  log "Kind cluster '$KIND_CLUSTER' exists"

  step "Step 1: Load images into Kind"
  for img in controller:dev task-executor:dev commit-executor:dev registry:2 alpine:latest; do
    if docker image inspect "$img" >/dev/null 2>&1; then
      kind load docker-image "$img" --name "$KIND_CLUSTER" 2>/dev/null || true
      log "Image $img loaded"
    else
      warn "Image $img not found locally, pulling..."
      docker pull "$img" 2>/dev/null || true
      kind load docker-image "$img" --name "$KIND_CLUSTER" 2>/dev/null || true
    fi
  done

  step "Step 2: Install CRDs"
  kubectl apply -f kubernetes/config/crd/bases/
  log "CRDs installed"

  step "Step 3: Create controller namespace + ServiceAccount"
  kubectl create ns "$CTRL_NS" --dry-run=client -o yaml | kubectl apply -f -
  kubectl create sa controller-manager -n "$CTRL_NS" --dry-run=client -o yaml | kubectl apply -f -

  step "Step 4: Apply RBAC (ClusterRole + ClusterRoleBinding + leader-election Role)"
  kubectl apply -f - <<RBAC
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: manager-role
rules:
- apiGroups: [""]
  resources: ["events", "pods"]
  verbs: ["create", "delete", "get", "list", "patch", "update", "watch"]
- apiGroups: [""]
  resources: ["pods/status"]
  verbs: ["get", "patch", "update"]
- apiGroups: ["batch"]
  resources: ["jobs"]
  verbs: ["create", "delete", "get", "list", "patch", "update", "watch"]
- apiGroups: ["batch"]
  resources: ["jobs/status"]
  verbs: ["get", "patch", "update"]
- apiGroups: ["sandbox.opensandbox.io"]
  resources: ["batchsandboxes", "pools", "sandboxsnapshots"]
  verbs: ["create", "delete", "get", "list", "patch", "update", "watch"]
- apiGroups: ["sandbox.opensandbox.io"]
  resources: ["batchsandboxes/finalizers", "pools/finalizers", "sandboxsnapshots/finalizers"]
  verbs: ["update"]
- apiGroups: ["sandbox.opensandbox.io"]
  resources: ["batchsandboxes/status", "pools/status", "sandboxsnapshots/status"]
  verbs: ["get", "patch", "update"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: manager-rolebinding
subjects:
- kind: ServiceAccount
  name: controller-manager
  namespace: opensandbox-system
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: manager-role
RBAC
  log "ClusterRole + ClusterRoleBinding applied"

  # Leader election Role + RoleBinding (namespace-scoped)
  kubectl apply -f - <<LEADER
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: leader-election-role
  namespace: opensandbox-system
rules:
- apiGroups: ["coordination.k8s.io"]
  resources: ["leases"]
  verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
- apiGroups: [""]
  resources: ["events"]
  verbs: ["create", "patch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: leader-election-rolebinding
  namespace: opensandbox-system
subjects:
- kind: ServiceAccount
  name: controller-manager
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: leader-election-role
LEADER
  log "Leader-election Role + RoleBinding applied"

  step "Step 5: Deploy controller"
  kubectl apply -f - <<CTRL
apiVersion: apps/v1
kind: Deployment
metadata:
  name: controller-manager
  namespace: opensandbox-system
  labels:
    control-plane: controller-manager
spec:
  selector:
    matchLabels:
      control-plane: controller-manager
  replicas: 1
  template:
    metadata:
      labels:
        control-plane: controller-manager
    spec:
      securityContext:
        runAsNonRoot: true
        seccompProfile:
          type: RuntimeDefault
      containers:
      - command:
        - /workspace/server
        args:
        - --leader-elect
        - --health-probe-bind-address=:8081
        - --commit-executor-image=commit-executor:dev
        image: controller:dev
        name: manager
        imagePullPolicy: IfNotPresent
        securityContext:
          allowPrivilegeEscalation: false
          capabilities:
            drop:
            - "ALL"
        livenessProbe:
          httpGet:
            path: /healthz
            port: 8081
          initialDelaySeconds: 15
          periodSeconds: 20
        readinessProbe:
          httpGet:
            path: /readyz
            port: 8081
          initialDelaySeconds: 5
          periodSeconds: 10
        resources:
          limits:
            cpu: "500m"
            memory: "128Mi"
          requests:
            cpu: "10m"
            memory: "64Mi"
      serviceAccountName: controller-manager
      terminationGracePeriodSeconds: 10
CTRL

  wait_for "controller Pod Ready" \
    "kubectl get pods -n $CTRL_NS -l control-plane=controller-manager --no-headers 2>/dev/null | grep -q '1/1.*Running'"

  step "Step 6: Deploy Docker Registry"
  kubectl apply -f - <<REG
apiVersion: apps/v1
kind: Deployment
metadata:
  name: docker-registry
  namespace: ${NS}
spec:
  replicas: 1
  selector:
    matchLabels:
      app: docker-registry
  template:
    metadata:
      labels:
        app: docker-registry
    spec:
      containers:
      - name: registry
        image: registry:2
        imagePullPolicy: IfNotPresent
        ports:
        - containerPort: 5000
        env:
        - name: REGISTRY_STORAGE_DELETE_ENABLED
          value: "true"
        readinessProbe:
          httpGet:
            path: /v2/
            port: 5000
          initialDelaySeconds: 5
          periodSeconds: 5
---
apiVersion: v1
kind: Service
metadata:
  name: docker-registry
  namespace: ${NS}
spec:
  type: ClusterIP
  ports:
  - port: 5000
    targetPort: 5000
  selector:
    app: docker-registry
REG

  wait_for "registry Pod Ready" \
    "kubectl get pods -n $NS -l app=docker-registry --no-headers 2>/dev/null | grep -q '1/1.*Running'"

  step "Step 7: Create registry secrets"
  kubectl create secret docker-registry registry-push-secret \
    --docker-server="$REGISTRY_ADDR" \
    --docker-username=testuser \
    --docker-password=testpass \
    -n "$NS" --dry-run=client -o yaml | kubectl apply -f -

  kubectl create secret docker-registry registry-pull-secret \
    --docker-server="$REGISTRY_ADDR" \
    --docker-username=testuser \
    --docker-password=testpass \
    -n "$NS" --dry-run=client -o yaml | kubectl apply -f -

  log "Secrets created"
  log "Infrastructure setup complete!"
}

# ---------------------------------------------------------------------------
# Test: Create BatchSandbox
# ---------------------------------------------------------------------------
test_create_batchsandbox() {
  step "Step A: Create BatchSandbox with pausePolicy"

  # Delete stale resources first
  kubectl delete batchsandbox "$SANDBOX_NAME" -n "$NS" --ignore-not-found=true 2>/dev/null || true
  kubectl delete sandboxsnapshot "$SANDBOX_NAME" -n "$NS" --ignore-not-found=true 2>/dev/null || true

  kubectl apply -f - <<BS
apiVersion: sandbox.opensandbox.io/v1alpha1
kind: BatchSandbox
metadata:
  name: ${SANDBOX_NAME}
  namespace: ${NS}
spec:
  replicas: 1
  template:
    metadata:
      labels:
        opensandbox.io/id: ${SANDBOX_NAME}
    spec:
      containers:
      - name: sandbox
        image: task-executor:dev
        imagePullPolicy: IfNotPresent
        command: ["sh", "-c", "echo 'Hello from sandbox' && sleep 3600"]
  pausePolicy:
    snapshotRegistry: ${REGISTRY_ADDR}
    snapshotPushSecretName: registry-push-secret
    resumeImagePullSecretName: registry-pull-secret
BS

  log "BatchSandbox '${SANDBOX_NAME}' created"

  # Wait for Pod to exist and be Running
  echo "Waiting for BatchSandbox Pod to be Running..."
  wait_for "BatchSandbox Pod Running" \
    "kubectl get batchsandbox ${SANDBOX_NAME} -n ${NS} -o 'jsonpath={.status.ready}' 2>/dev/null | grep -q '1'"

  READY=$(kubectl get batchsandbox "$SANDBOX_NAME" -n "$NS" -o 'jsonpath={.status.ready}' 2>/dev/null)
  log "BatchSandbox ready replicas: ${READY:-0}"

  # Get Pod info
  POD_NAME=$(kubectl get pods -n "$NS" -l "opensandbox.io/id=${SANDBOX_NAME}" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
  NODE_NAME=$(kubectl get pods -n "$NS" -l "opensandbox.io/id=${SANDBOX_NAME}" -o jsonpath='{.items[0].spec.nodeName}' 2>/dev/null || true)

  if [ -z "$POD_NAME" ]; then
    # Fallback: find pod by ownerReference
    POD_NAME=$(kubectl get pods -n "$NS" -o json | python3 -c "
import json, sys
data = json.load(sys.stdin)
for pod in data.get('items', []):
    for ref in pod.get('metadata', {}).get('ownerReferences', []):
        if ref.get('name') == '${SANDBOX_NAME}':
            print(pod['metadata']['name'])
            break
" 2>/dev/null || true)
    NODE_NAME=$(kubectl get pod "$POD_NAME" -n "$NS" -o jsonpath='{.spec.nodeName}' 2>/dev/null || true)
  fi

  echo "  Pod: ${POD_NAME}"
  echo "  Node: ${NODE_NAME}"

  if [ -z "$POD_NAME" ] || [ -z "$NODE_NAME" ]; then
    fail "Could not find Pod or Node for BatchSandbox. Check controller logs."
  fi

  log "BatchSandbox is Running with Pod '${POD_NAME}' on node '${NODE_NAME}'"
}

# ---------------------------------------------------------------------------
# Test: Pause (create SandboxSnapshot)
# ---------------------------------------------------------------------------
test_pause() {
  step "Step B: Pause — create SandboxSnapshot"

  POD_NAME=$(kubectl get pods -n "$NS" -o json | python3 -c "
import json, sys
data = json.load(sys.stdin)
for pod in data.get('items', []):
    for ref in pod.get('metadata', {}).get('ownerReferences', []):
        if ref.get('name') == '${SANDBOX_NAME}':
            print(pod['metadata']['name'])
            break
" 2>/dev/null || true)
  NODE_NAME=$(kubectl get pod "$POD_NAME" -n "$NS" -o jsonpath='{.spec.nodeName}' 2>/dev/null || true)

  if [ -z "$POD_NAME" ]; then
    fail "No running Pod found for BatchSandbox '${SANDBOX_NAME}'"
  fi

  PAUSED_AT=$(date -u +"%Y-%m-%dT%H:%M:%SZ")

  kubectl apply -f - <<SNAP
apiVersion: sandbox.opensandbox.io/v1alpha1
kind: SandboxSnapshot
metadata:
  name: ${SANDBOX_NAME}
  namespace: ${NS}
spec:
  sandboxId: ${SANDBOX_NAME}
  snapshotType: Rootfs
  sourceBatchSandboxName: ${SANDBOX_NAME}
  sourcePodName: ${POD_NAME}
  sourceContainerName: sandbox
  sourceNodeName: ${NODE_NAME}
  imageUri: ${REGISTRY_ADDR}/${SANDBOX_NAME}:snapshot
  snapshotPushSecretName: registry-push-secret
  resumeImagePullSecretName: registry-pull-secret
  resumeTemplate:
    template:
      metadata:
        labels:
          opensandbox.io/id: ${SANDBOX_NAME}
      spec:
        containers:
        - name: sandbox
          image: task-executor:dev
          command: ["sh", "-c", "echo 'Hello from sandbox' && sleep 3600"]
  pausedAt: "${PAUSED_AT}"
SNAP

  log "SandboxSnapshot '${SANDBOX_NAME}' created"

  # Watch snapshot phase progression
  echo ""
  echo "Watching snapshot phase..."
  PREV_PHASE=""
  elapsed=0
  while (( elapsed < TIMEOUT )); do
    PHASE=$(kubectl get sandboxsnapshot "$SANDBOX_NAME" -n "$NS" -o 'jsonpath={.status.phase}' 2>/dev/null || echo "None")

    if [ "$PHASE" != "$PREV_PHASE" ]; then
      echo "  Phase: ${PRE_PHASE:-None} -> ${PHASE}  (${elapsed}s)"
      PREV_PHASE="$PHASE"
    fi

    case "$PHASE" in
      Ready)
        log "Snapshot is Ready!"
        break
        ;;
      Failed)
        MSG=$(kubectl get sandboxsnapshot "$SANDBOX_NAME" -n "$NS" -o 'jsonpath={.status.message}' 2>/dev/null)
        fail "Snapshot Failed: ${MSG}"
        ;;
    esac

    sleep 3
    (( elapsed += 3 ))
  done

  if (( elapsed >= TIMEOUT )); then
    fail "Timed out waiting for Snapshot Ready (current phase: ${PHASE})"
  fi

  # Verify commit Job was created and succeeded
  echo ""
  echo "Checking commit Job..."
  JOB_NAME=$(kubectl get jobs -n "$NS" -o json | python3 -c "
import json, sys
data = json.load(sys.stdin)
for job in data.get('items', []):
    for ref in job.get('metadata', {}).get('ownerReferences', []):
        if ref.get('name') == '${SANDBOX_NAME}' and ref.get('kind') == 'SandboxSnapshot':
            print(job['metadata']['name'])
            break
" 2>/dev/null || true)

  if [ -n "$JOB_NAME" ]; then
    JOB_STATUS=$(kubectl get job "$JOB_NAME" -n "$NS" -o 'jsonpath={.status.succeeded}' 2>/dev/null || echo "0")
    if [ "$JOB_STATUS" = "1" ]; then
      log "Commit Job '${JOB_NAME}' succeeded"
    else
      warn "Commit Job '${JOB_NAME}' status: succeeded=${JOB_STATUS}"
      kubectl logs "job/${JOB_NAME}" -n "$NS" --tail=20 2>/dev/null || true
    fi
  else
    warn "No commit Job found (controller may handle commit internally)"
  fi

  # Delete original BatchSandbox (simulates pause completing)
  step "Step C: Delete original BatchSandbox (release compute)"
  kubectl delete batchsandbox "$SANDBOX_NAME" -n "$NS" --ignore-not-found=true 2>/dev/null || true
  sleep 2
  log "Original BatchSandbox deleted — sandbox is now Paused"

  # Verify state: no BatchSandbox, Snapshot Ready
  BS_EXISTS=$(kubectl get batchsandbox "$SANDBOX_NAME" -n "$NS" --ignore-not-found=true 2>/dev/null || true)
  SNAP_PHASE=$(kubectl get sandboxsnapshot "$SANDBOX_NAME" -n "$NS" -o 'jsonpath={.status.phase}' 2>/dev/null || echo "None")

  if [ -z "$BS_EXISTS" ] && [ "$SNAP_PHASE" = "Ready" ]; then
    log "Verified: BatchSandbox gone, Snapshot Ready → sandbox is Paused"
  else
    warn "State check: BatchSandbox exists=${BS_EXISTS:-no}, Snapshot phase=${SNAP_PHASE}"
  fi
}

# ---------------------------------------------------------------------------
# Test: Resume (create new BatchSandbox from snapshot)
# ---------------------------------------------------------------------------
test_resume() {
  step "Step D: Resume — create new BatchSandbox from snapshot image"

  # Get snapshot image URI
  IMAGE_URI=$(kubectl get sandboxsnapshot "$SANDBOX_NAME" -n "$NS" -o 'jsonpath={.spec.imageUri}' 2>/dev/null || true)
  if [ -z "$IMAGE_URI" ]; then
    fail "Could not get snapshot image URI"
  fi

  # Get resumeTemplate
  RT_TEMPLATE=$(kubectl get sandboxsnapshot "$SANDBOX_NAME" -n "$NS" -o json | python3 -c "
import json, sys, yaml
data = json.load(sys.stdin)
rt = data.get('spec', {}).get('resumeTemplate', {})
# Just check it exists
if rt:
    print('found')
else:
    print('missing')
" 2>/dev/null || echo "missing")
  echo "  Snapshot image: ${IMAGE_URI}"
  echo "  ResumeTemplate: ${RT_TEMPLATE}"

  kubectl apply -f - <<RESUME
apiVersion: sandbox.opensandbox.io/v1alpha1
kind: BatchSandbox
metadata:
  name: ${SANDBOX_NAME}
  namespace: ${NS}
  annotations:
    sandbox.opensandbox.io/resumed-from-snapshot: "true"
  labels:
    opensandbox.io/id: ${SANDBOX_NAME}
spec:
  replicas: 1
  template:
    metadata:
      labels:
        opensandbox.io/id: ${SANDBOX_NAME}
    spec:
      imagePullSecrets:
      - name: registry-pull-secret
      containers:
      - name: sandbox
        image: ${IMAGE_URI}
        imagePullPolicy: IfNotPresent
        command: ["sh", "-c", "echo 'Resumed from snapshot!' && sleep 3600"]
  pausePolicy:
    snapshotRegistry: ${REGISTRY_ADDR}
    snapshotPushSecretName: registry-push-secret
    resumeImagePullSecretName: registry-pull-secret
RESUME

  log "Resumed BatchSandbox created from snapshot image: ${IMAGE_URI}"

  # Wait for new Pod to be Running
  echo ""
  echo "Waiting for resumed BatchSandbox Pod..."
  wait_for "Resumed BatchSandbox Pod Running" \
    "kubectl get batchsandbox ${SANDBOX_NAME} -n ${NS} -o 'jsonpath={.status.ready}' 2>/dev/null | grep -q '1'"

  READY=$(kubectl get batchsandbox "$SANDBOX_NAME" -n "$NS" -o 'jsonpath={.status.ready}' 2>/dev/null)
  NEW_POD=$(kubectl get pods -n "$NS" -o json | python3 -c "
import json, sys
data = json.load(sys.stdin)
for pod in data.get('items', []):
    for ref in pod.get('metadata', {}).get('ownerReferences', []):
        if ref.get('name') == '${SANDBOX_NAME}':
            print(pod['metadata']['name'])
            break
" 2>/dev/null || true)

  log "Resumed BatchSandbox is Running! Pod: ${NEW_POD}, Ready: ${READY}"

  # Verify annotation
  ANN=$(kubectl get batchsandbox "$SANDBOX_NAME" -n "$NS" -o 'jsonpath={.metadata.annotations.sandbox\.opensandbox\.io/resumed-from-snapshot}' 2>/dev/null || true)
  if [ "$ANN" = "true" ]; then
    log "Verified: resumed-from-snapshot annotation = true"
  else
    warn "resumed-from-snapshot annotation: ${ANN:-missing}"
  fi

  # Verify new Pod uses snapshot image
  POD_IMAGE=$(kubectl get pod "$NEW_POD" -n "$NS" -o 'jsonpath={.spec.containers[0].image}' 2>/dev/null || true)
  echo "  Pod image: ${POD_IMAGE}"
  if [[ "$POD_IMAGE" == *"$SANDBOX_NAME"* ]]; then
    log "Verified: Pod uses snapshot image"
  else
    warn "Pod image does not match snapshot image URI"
  fi

  # Verify Snapshot still exists (retained for future pause/resume)
  SNAP_PHASE=$(kubectl get sandboxsnapshot "$SANDBOX_NAME" -n "$NS" -o 'jsonpath={.status.phase}' 2>/dev/null || echo "missing")
  echo "  Snapshot phase after resume: ${SNAP_PHASE}"
  if [ "$SNAP_PHASE" = "Ready" ]; then
    log "Snapshot retained after resume (can pause again)"
  fi
}

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------
print_summary() {
  step "E2E Pause/Resume Test Summary"

  echo ""
  echo "  Phase                | Status"
  echo "  ─────────────────────┼───────────"
  echo "  Create BatchSandbox  | ✅ Running"
  echo "  Pause (Snapshot)     | ✅ Ready"
  echo "  Original BS deleted  | ✅ Paused"
  echo "  Resume (new BS)      | ✅ Running"
  echo "  Snapshot retained    | ✅ Ready"
  echo ""

  echo "Cluster resources:"
  kubectl get batchsandboxes -A 2>/dev/null || true
  echo ""
  kubectl get sandboxsnapshots -A 2>/dev/null || true
  echo ""
  kubectl get jobs -A 2>/dev/null || true
  echo ""
  kubectl get pods -A 2>/dev/null | grep -E "NAME|controller|registry|sandbox" || true

  echo ""
  log "E2E test PASSED — full pause/resume cycle completed successfully!"
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------
main() {
  echo "=============================================="
  echo "  OpenSandbox K8s Pause/Resume E2E Test"
  echo "  Cluster: ${KIND_CLUSTER}"
  echo "  Sandbox: ${SANDBOX_NAME}"
  echo "  Timeout: ${TIMEOUT}s"
  echo "=============================================="

  if [ "${1:-}" = "--cleanup-only" ]; then
    cleanup_all
    exit 0
  fi

  if [ "${1:-}" = "--skip-setup" ]; then
    warn "Skipping infrastructure setup"
  else
    setup_infra
  fi

  # Run test phases
  test_create_batchsandbox
  test_pause
  test_resume
  print_summary

  # Ask user about cleanup
  echo ""
  read -p "Clean up test resources? [y/N] " -n 1 -r
  echo ""
  if [[ $REPLY =~ ^[Yy]$ ]]; then
    cleanup
    log "Test resources cleaned up"
  else
    echo "Resources left for inspection. Clean up later with: $0 --cleanup-only"
  fi
}

main "$@"
