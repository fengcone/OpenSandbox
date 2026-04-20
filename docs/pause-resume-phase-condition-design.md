# BatchSandbox Phase + Condition 设计方案

本文档描述 Pause/Resume 功能中 BatchSandbox Status 的 Phase + Condition 设计方案，解决当前 Phase=Failed 语义混淆问题。

---

## 一、问题背景

当前 Phase=Failed 承载了多种截然不同的语义：

| 场景 | Pod 状态 | 可 Pause？ | 可 Resume？ | 真实语义 |
|---|---|---|---|---|
| commit/push 失败 | Pod 仍在 Running | ✅ 可重试 | ✗ | Pause 操作失败，sandbox 仍可用 |
| Pool template 固化失败 | Pool Pod 仍在 Running | ✅ 可重试 | ✗ | Pause 前置步骤失败，sandbox 仍可用 |
| Pod 不存在（pause 时） | 无 Pod | ✗ | ✗ | sandbox 本身已不可用 |
| Resume snapshot 问题 | 无 Pod（仍 Paused） | ✗ | ✅ 可重试 | Resume 操作失败，sandbox 仍 Paused |
| Resume 后 Pod 启动失败 | Pod 异常 | ✗ | ✗ | sandbox 不可用 |

Server 无法仅凭 Phase=Failed 判断 sandbox 是否可用、是否允许重试。

---

## 二、方案概述

**核心原则：Phase 回归"沙盒当前是否可用"的语义，Condition 承载"操作为什么失败"的结构化上下文。**

- **Phase**：回答"沙盒现在是什么状态"
- **Condition**：回答"为什么失败"（机器可读）

关键变化：
- commit/push 失败 → Phase=**Running** + PauseFailed=True（Pod 仍在）
- Resume snapshot 问题 → Phase=**Paused** + ResumeFailed=True（可重试）
- Phase=Failed 只在 Pod 真没了时设置

---

## 三、CRD 字段变更

### BatchSandboxPhase 枚举

```go
// +kubebuilder:validation:Enum=Pending;Running;Pausing;Paused;Resuming;Failed
type BatchSandboxPhase string

const (
    // Pod 创建中，尚未 Ready
    BatchSandboxPhasePending  BatchSandboxPhase = "Pending"
    // 沙盒可用（Pod Ready），包括 pause failed 后回退到此状态
    BatchSandboxPhaseRunning  BatchSandboxPhase = "Running"
    // pause 进行中（commit/push），沙盒仍可用
    BatchSandboxPhasePausing  BatchSandboxPhase = "Pausing"
    // 已暂停，Pod 已缩为 0，包括 resume failed 后回退到此状态
    BatchSandboxPhasePaused   BatchSandboxPhase = "Paused"
    // resume 进行中（Pod 启动中），沙盒暂不可用
    BatchSandboxPhaseResuming BatchSandboxPhase = "Resuming"
    // 沙盒不可用且无法自恢复（Pod 彻底没了）
    BatchSandboxPhaseFailed   BatchSandboxPhase = "Failed"
)
```

### BatchSandboxCondition 结构

```go
type BatchSandboxCondition struct {
    Type               BatchSandboxConditionType `json:"type"`
    Status             ConditionStatus           `json:"status"`
    Reason             string                    `json:"reason,omitempty"`
    Message            string                    `json:"message,omitempty"`
    LastTransitionTime *metav1.Time              `json:"lastTransitionTime,omitempty"`
}

type BatchSandboxConditionType string

const (
    // PauseFailed 为 True 表示最近一次 pause 操作失败
    BatchSandboxConditionPauseFailed  BatchSandboxConditionType = "PauseFailed"
    // ResumeFailed 为 True 表示最近一次 resume 操作失败
    BatchSandboxConditionResumeFailed BatchSandboxConditionType = "ResumeFailed"
)

type ConditionStatus string

const (
    ConditionTrue  ConditionStatus = "True"
    ConditionFalse ConditionStatus = "False"
)
```

### Condition Reason 枚举

**PauseFailed Reason：**

| Reason | 含义 | Pod 状态 |
|---|---|---|
| `CommitPushFailed` | nerdctl commit/push 失败 | 仍在 Running |
| `PodNotFound` | pause 时找不到 Pod | 无 Pod |
| `PoolTemplateMissing` | Pool CR 不存在或 template 为空 | Pool Pod 仍在 |

**ResumeFailed Reason：**

| Reason | 含义 | Pod 状态 |
|---|---|---|
| `SnapshotNotReady` | SandboxSnapshot phase != Ready | 无 Pod（仍 Paused） |
| `SnapshotNotFound` | SandboxSnapshot 不存在 | 无 Pod（仍 Paused） |
| `PodStartFailed` | Pod CrashLoopBackOff / ImagePullBackOff | Pod 异常 |

### BatchSandboxStatus 变更

```go
type BatchSandboxStatus struct {
    // ... 既有字段 ...

    // Phase 表示沙盒的可用状态
    // +optional
    Phase BatchSandboxPhase `json:"phase,omitempty"`

    // PauseObservedGeneration 是 Controller 最近一次进入 pause/resume
    // 分发逻辑时的 generation，立即 ACK 写入以防重入
    // +optional
    PauseObservedGeneration int64 `json:"pauseObservedGeneration,omitempty"`

    // Conditions 记录操作失败的结构化上下文
    // +optional
    // +listType=map
    // +listMapKey=type
    Conditions []BatchSandboxCondition `json:"conditions,omitempty"`

    // Message 字段删除 → 故障详情由 Condition.Message 承载
}
```

---

## 四、Phase 设置规则（全场景）

### 正常链路

```
创建 → Pending
Pod Ready → Running
Server PATCH spec.pause=true → Pausing
SandboxSnapshot Ready → Paused
Server PATCH spec.pause=false → Resuming
Pod Ready → Running
```

### Pause 失败

| 子场景 | Phase | Condition | 理由 |
|---|---|---|---|
| commit/push 失败 | **Running** | PauseFailed=True, Reason=CommitPushFailed | Pod 仍在，sandbox 可用 |
| Pool template 缺失 | **Running** | PauseFailed=True, Reason=PoolTemplateMissing | Pool Pod 仍在，sandbox 可用 |
| Pod 不存在 | **Failed** | PauseFailed=True, Reason=PodNotFound | sandbox 不可用 |

### Resume 失败

| 子场景 | Phase | Condition | 理由 |
|---|---|---|---|
| Snapshot not ready | **Paused** | ResumeFailed=True, Reason=SnapshotNotReady | sandbox 仍在 Paused，可重试 |
| Snapshot not found | **Paused** | ResumeFailed=True, Reason=SnapshotNotFound | sandbox 仍在 Paused，可重试 |
| Pod 启动失败 | **Failed** | ResumeFailed=True, Reason=PodStartFailed | sandbox 不可用 |

### Condition 生命周期

- `PauseFailed`：pause 失败时设为 True；**下一次 handlePause 进入时清除为 False**
- `ResumeFailed`：resume 失败时设为 True；**下一次 handleResume 进入时清除为 False**

---

## 五、普通链路详细流程

```
[创建]
  BatchSandbox:
    spec.replicas=1, spec.template={...}, spec.poolRef=""
    spec.pause=nil
         │
         ▼  BatchSandbox Controller: scaleBatchSandbox()
[Running]
  Pod: {name: "sandbox-abc-0", node: "node-1"} Running
  status.phase                   = "Running"
  status.pauseObservedGeneration = 0
  status.conditions              = []

━━━━━━━━━━━━━━━━━━━━━━━ PAUSE（第一次）━━━━━━━━━━━━━━━━━━━━━━━

         │  Server 前置校验（读 status）：
         │    status.phase == "Running" ✓
         │    status.conditions 无 PauseFailed=True ✓
         │
         │  Server PATCH spec.pause = true
         ▼
  K8s: generation N → N+1

  ─────────────────────────────────────────────────────────────────
  BatchSandbox Controller Reconcile：
    generation(N+1) > pauseObservedGeneration(0) → 触发分发
    spec.pause = true（非 nil）→ handlePause()

    handlePause 开头清除上次 PauseFailed：
         setCondition(PauseFailed, False, "", "")

    ① 立即 ACK（防止 requeue 重入）：
         PATCH status.pauseObservedGeneration = N+1
                status.phase                  = "Pausing"

    ② 创建 SandboxSnapshot 子资源（name="sandbox-abc"）：
         spec.sandboxName = "sandbox-abc"

    ③ requeue 等待子资源状态变化
  ─────────────────────────────────────────────────────────────────

[Pausing]
  status.phase                   = "Pausing"
  status.pauseObservedGeneration = N+1
  status.conditions              = []

  ─────────────────────────────────────────────────────────────────
  SandboxSnapshot Controller 接管：
    1. 读 BatchSandbox "sandbox-abc"
       → 找到 Pod "sandbox-abc-0"，node="node-1"
    2. status.sourcePodName  = "sandbox-abc-0"
       status.sourceNodeName = "node-1"
    3. 从 Controller 启动参数读取 registry / pushSecret
    4. 生成 imageUri：reg/abc-main:snap-gen{N+1}
    5. 创建 commit Job 调度到 node-1
    6. Job 运行：nerdctl commit + push
    7. status.phase: Pending → Committing → Ready
       status.containers[0] = {containerName:"main",
                                imageUri:"reg/abc-main:snap-gen{N+1}",
                                imageDigest:"sha256:..."}
       status.readyAt = now()
  ─────────────────────────────────────────────────────────────────

  BatchSandbox Controller syncPauseOrClear（子资源 phase=Ready）：
    ① PATCH status.phase = "Paused"
       不设置任何 Condition（成功无需 Condition）
    ② PATCH spec: replicas=0, pause=nil
       → generation: N+1 → N+2

[Paused]
  spec.replicas = 0（无 Pod），spec.pause = nil
  status.phase                   = "Paused"
  status.pauseObservedGeneration = N+2
  status.conditions              = []
  SandboxSnapshot "sandbox-abc" 子资源存在

━━━━━━━━━━━━━━━━━━━━━━━ RESUME（第一次）━━━━━━━━━━━━━━━━━━━━━━

         │  Server 前置校验（读 status）：
         │    status.phase == "Paused" ✓
         │    status.conditions 无 ResumeFailed=True ✓
         │
         │  Server PATCH spec.pause = false
         ▼
  K8s: generation N+2 → N+3

  ─────────────────────────────────────────────────────────────────
  BatchSandbox Controller Reconcile：
    generation(N+3) > pauseObservedGeneration(N+2) → 触发分发
    spec.pause = false（非 nil）→ handleResume()

    handleResume 开头清除上次 ResumeFailed：
         setCondition(ResumeFailed, False, "", "")

    ① 立即 ACK：
         PATCH status.pauseObservedGeneration = N+3
                status.phase                  = "Resuming"

    ② requeue（等待 ACK 传播）
  ─────────────────────────────────────────────────────────────────

[Resuming]
  status.phase                   = "Resuming"
  status.pauseObservedGeneration = N+3
  status.conditions              = []

  ─────────────────────────────────────────────────────────────────
  BatchSandbox Controller continueResume（phase=Resuming 分支）：

    ① Get SandboxSnapshot "sandbox-abc"：
         校验 status.phase == "Ready" ✓
         读 status.containers[0].imageUri

    ② 替换 spec.template 对应容器 image

    ③ spec.replicas = 1（扩容）

    ④ 【Pool 专属】spec.poolRef = ""（脱 Pool）

    ⑤ PATCH spec: pause=nil
       → generation: N+3 → N+4

    ⑥ 删除 SandboxSnapshot "sandbox-abc"

    ⑦ requeue（等待 Pod 启动）
  ─────────────────────────────────────────────────────────────────

  Pod "sandbox-abc-0" 进入 Running →
    status.phase = "Running"

[Running]
  status.phase                   = "Running"
  status.pauseObservedGeneration = N+4
  status.conditions              = []
```

---

## 六、失败链路详细流程

### 场景 A：commit/push 失败

```
━━━ commit/push 失败 ━━━

  SandboxSnapshot Controller：Job 失败（registry 不可达）
    status.phase   = "Failed"
    status.message = "push image failed: connection refused"

         │
         ▼
  BatchSandbox Controller syncPauseOrClear（子资源 phase=Failed）：

    判断 Pod 是否还在：
    
    pod, err := r.findPodForSandbox(ctx, bs)
    if err != nil {
        // Pod 不存在
        phase = BatchSandboxPhaseFailed
        reason = "PodNotFound"
    } else {
        // Pod 仍在
        phase = BatchSandboxPhaseRunning
        reason = "CommitPushFailed"
    }

    PATCH status:
      pauseObservedGeneration = N+1
      phase                   = phase  // Running 或 Failed
      conditions = [
        {
          type:               "PauseFailed",
          status:             "True",
          reason:             reason,
          message:            "push image failed: connection refused",
          lastTransitionTime: now()
        }
      ]

    PATCH spec: pause=nil

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

[Running + PauseFailed]（Pod 仍在）
  status.phase                   = "Running"
  status.conditions              = [
    {type: "PauseFailed", status: "True", reason: "CommitPushFailed", ...}
  ]
  Pod 仍在 Running，沙盒可用 ✓
  用户可重试 Pause

[Failed + PauseFailed]（Pod 不存在）
  status.phase                   = "Failed"
  status.conditions              = [
    {type: "PauseFailed", status: "True", reason: "PodNotFound", ...}
  ]
  无 Pod，沙盒不可用 ✗
  不允许重试 Pause
```

### 场景 B：Pool template 固化失败

```
━━━ Pool template 固化失败 ━━━

  BatchSandbox Controller handlePause() 前置步骤：
    已 ACK：pauseObservedGeneration = N+1, phase = "Pausing"
    
    读 Pool CR "my-pool" → NotFound 或 pool.spec.template==nil

    判断 Pod 是否还在：
         Pool Pod 仍在 Running → phase = Running
         Pool Pod 不存在 → phase = Failed（极端情况）

    PATCH status:
      phase = Running  // 或 Failed
      conditions = [
        {
          type:    "PauseFailed",
          status:  "True",
          reason:  "PoolTemplateMissing",
          message: "pool CR not found or template empty",
          lastTransitionTime: now()
        }
      ]

    PATCH spec: pause=nil

[Running + PauseFailed]
  Pool Pod 仍在 Running，沙盒可用 ✓
  用户可重试 Pause（修复 Pool CR 后）
```

### 场景 C：Resume snapshot 问题

```
━━━ Resume snapshot 问题 ━━━

  BatchSandbox Controller continueResume()：

    Get SandboxSnapshot "sandbox-abc"：
    
    ┌─────────────────────────────────────────────────────────────┐
    │ 情况 1：NotFound                                            │
    ├─────────────────────────────────────────────────────────────┤
    │   PATCH status:                                             │
    │     phase = "Paused"  // 回退，沙盒仍在 Paused 状态         │
    │     conditions = [                                          │
    │       {                                                     │
    │         type:    "ResumeFailed",                            │
    │         status:  "True",                                    │
    │         reason:  "SnapshotNotFound",                        │
    │         message: "SandboxSnapshot not found"                │
    │       }                                                     │
    │     ]                                                       │
    │   PATCH spec: pause=nil                                     │
    │                                                             │
    │   [Paused + ResumeFailed]                                   │
    │     可重试 Resume ✓                                         │
    └─────────────────────────────────────────────────────────────┘
    
    ┌─────────────────────────────────────────────────────────────┐
    │ 情况 2：phase != Ready                                      │
    ├─────────────────────────────────────────────────────────────┤
    │   PATCH status:                                             │
    │     phase = "Paused"                                        │
    │     conditions = [                                          │
    │       {                                                     │
    │         type:    "ResumeFailed",                            │
    │         status:  "True",                                    │
    │         reason:  "SnapshotNotReady",                        │
    │         message: "snapshot not ready: phase=Failed"         │
    │       }                                                     │
    │     ]                                                       │
    │   PATCH spec: pause=nil                                     │
    │                                                             │
    │   [Paused + ResumeFailed]                                   │
    │     可重试 Resume ✓                                         │
    └─────────────────────────────────────────────────────────────┘
```

### 场景 D：Resume 后 Pod 启动失败

```
━━━ Resume 后 Pod 启动失败 ━━━

  continueResume 成功执行：
    spec.replicas = 1
    SandboxSnapshot 已删除
    pause 已清空

  Pod 创建中，但持续 ImagePullBackOff / CrashLoopBackOff：

  ─────────────────────────────────────────────────────────────────
  BatchSandbox Controller Reconcile（Pod 状态更新）：

    case BatchSandboxPhaseResuming:
      // Pod 启动失败检测
      if isPodFailed(pod) {  // CrashLoopBackOff, ImagePullBackOff
        
        PATCH status:
          phase = "Failed"
          conditions = [
            {
              type:               "ResumeFailed",
              status:             "True",
              reason:             "PodStartFailed",
              message:            "Pod ImagePullBackOff: failed to pull image",
              lastTransitionTime: now()
            }
          ]
        
        return  // 不继续等待
      }
      
      // Pod Ready → 正常完成
      if newStatus.Ready > 0 {
        newStatus.Phase = BatchSandboxPhaseRunning
      }
  ─────────────────────────────────────────────────────────────────

[Failed + ResumeFailed]
  status.phase                   = "Failed"
  status.conditions              = [
    {type: "ResumeFailed", status: "True", reason: "PodStartFailed", ...}
  ]
  SandboxSnapshot 已删除 → 无法重试 Resume
  需修复 image 问题后重新 Pause 获取新快照
```

---

## 七、Server 前置校验矩阵

### 基础校验（Phase 单字段）

| `status.phase` | 允许 Pause？ | 允许 Resume？ | 返回消息 |
|---|---|---|---|
| `Pending` | ✗ 409 | ✗ 409 | "Sandbox is being created" |
| `Running` | ✓ | ✗ 409 | — |
| `Pausing` | ✗ 409 | ✗ 409 | "Pause in progress" |
| `Paused` | ✗ 409 | ✓ | — |
| `Resuming` | ✗ 409 | ✗ 409 | "Resume in progress" |
| `Failed` | ✗ 409 | ✗ 409 | "Sandbox is not available" |

### 精细化校验（Phase + Condition）

| `status.phase` | `conditions[].type` | 允许 Pause？ | 允许 Resume？ | 用户感知 |
|---|---|---|---|---|
| `Running` | `PauseFailed=True` | ✓ 重试 | ✗ | "Running (pause failed, retry available)" |
| `Paused` | `ResumeFailed=True` | ✗ | ✓ 重试 | "Paused (resume failed, retry available)" |
| `Failed` | `PauseFailed=True` | ✗ | ✗ | "Failed (pause caused pod loss)" |
| `Failed` | `ResumeFailed=True` | ✗ | ✗ | "Failed (resume caused pod start failure)" |
| `Failed` | 无 Condition | ✗ | ✗ | "Failed (pod failure)" |

---

## 八、Controller 实现要点

### 1. setCondition 辅助函数

```go
func (r *BatchSandboxReconciler) setCondition(
    ctx context.Context,
    bs *sandboxv1alpha1.BatchSandbox,
    conditionType BatchSandboxConditionType,
    status ConditionStatus,
    reason string,
    message string,
) error {
    return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
        latest := &sandboxv1alpha1.BatchSandbox{}
        if err := r.Get(ctx, types.NamespacedName{Namespace: bs.Namespace, Name: bs.Name}, latest); err != nil {
            return err
        }
        
        // 查找现有 condition
        var conditions []sandboxv1alpha1.BatchSandboxCondition
        found := false
        for _, c := range latest.Status.Conditions {
            if c.Type == conditionType {
                if status == ConditionFalse {
                    // 移除 condition（清除失败标记）
                    continue
                }
                // 更新现有 condition
                c.Status = status
                c.Reason = reason
                c.Message = message
                c.LastTransitionTime = ptrToTime(metav1.Now())
                found = true
            }
            conditions = append(conditions, c)
        }
        
        // 新增 condition
        if !found && status == ConditionTrue {
            conditions = append(conditions, sandboxv1alpha1.BatchSandboxCondition{
                Type:               conditionType,
                Status:             status,
                Reason:             reason,
                Message:            message,
                LastTransitionTime: ptrToTime(metav1.Now()),
            })
        }
        
        latest.Status.Conditions = conditions
        return r.Status().Update(ctx, latest)
    })
}
```

### 2. handlePause 开头清除 PauseFailed

```go
func (r *BatchSandboxReconciler) handlePause(ctx context.Context, bs *sandboxv1alpha1.BatchSandbox) (ctrl.Result, error) {
    // 清除上次的 PauseFailed condition（开始新操作）
    _ = r.setCondition(ctx, bs, BatchSandboxConditionPauseFailed, ConditionFalse, "", "")
    
    // ... 原有逻辑 ...
}
```

### 3. handleResume 开头清除 ResumeFailed

```go
func (r *BatchSandboxReconciler) handleResume(ctx context.Context, bs *sandboxv1alpha1.BatchSandbox) (ctrl.Result, error) {
    // 清除上次的 ResumeFailed condition（开始新操作）
    _ = r.setCondition(ctx, bs, BatchSandboxConditionResumeFailed, ConditionFalse, "", "")
    
    // ... 原有逻辑 ...
}
```

### 4. syncPauseOrClear 中 snapshot Failed 分支

```go
case sandboxv1alpha1.SandboxSnapshotPhaseFailed:
    msg := snapshot.Status.Message
    
    // 判断 Pod 是否还在
    phase := sandboxv1alpha1.BatchSandboxPhaseRunning
    reason := "CommitPushFailed"
    
    if _, err := r.findPodForSandbox(ctx, bs); err != nil {
        phase = sandboxv1alpha1.BatchSandboxPhaseFailed
        reason = "PodNotFound"
    }
    
    _ = r.ackPauseWithPhase(ctx, bs, phase, "")
    _ = r.setCondition(ctx, bs, BatchSandboxConditionPauseFailed, ConditionTrue, reason, msg)
    _ = r.clearPause(ctx, bs)
```

### 5. continueResume 中 snapshot 问题处理

```go
// Snapshot 不存在
if errors.IsNotFound(err) {
    _ = r.ackPauseWithPhase(ctx, bs, sandboxv1alpha1.BatchSandboxPhasePaused, "")
    _ = r.setCondition(ctx, bs, BatchSandboxConditionResumeFailed, ConditionTrue, "SnapshotNotFound", "SandboxSnapshot not found")
    _ = r.clearPause(ctx, bs)
    return ctrl.Result{}, nil
}

// Snapshot 未 Ready
if snapshot.Status.Phase != sandboxv1alpha1.SandboxSnapshotPhaseReady {
    msg := fmt.Sprintf("snapshot not ready: phase=%s", snapshot.Status.Phase)
    _ = r.ackPauseWithPhase(ctx, bs, sandboxv1alpha1.BatchSandboxPhasePaused, "")
    _ = r.setCondition(ctx, bs, BatchSandboxConditionResumeFailed, ConditionTrue, "SnapshotNotReady", msg)
    _ = r.clearPause(ctx, bs)
    return ctrl.Result{}, nil
}
```

### 6. Pod 启动失败检测

```go
// 在正常 reconcile 的 pod 状态更新逻辑中
case sandboxv1alpha1.BatchSandboxPhaseResuming:
    if isPodFailed(pod) {  // CrashLoopBackOff, ImagePullBackOff
        msg := getPodFailureMessage(pod)
        _ = r.setCondition(ctx, bs, BatchSandboxConditionResumeFailed, ConditionTrue, "PodStartFailed", msg)
        newStatus.Phase = sandboxv1alpha1.BatchSandboxPhaseFailed
    } else if newStatus.Ready > 0 {
        newStatus.Phase = sandboxv1alpha1.BatchSandboxPhaseRunning
    }

func isPodFailed(pod *corev1.Pod) bool {
    // 检查 Pod 状态
    for _, cs := range pod.Status.ContainerStatuses {
        if cs.State.Waiting != nil {
            switch cs.State.Waiting.Reason {
            case "CrashLoopBackOff", "ImagePullBackOff", "ErrImagePull", "CreateContainerConfigError":
                return true
            }
        }
    }
    return false
}
```

---

## 九、对比总结

| 场景 | 当前代码 | 方案 C |
|---|---|---|
| **commit/push 失败** | Phase=Failed | Phase=**Running** + PauseFailed=True |
| **Pool template 缺失** | Phase=Failed | Phase=**Running** + PauseFailed=True |
| **Pause 时 Pod 不存在** | Phase=Failed | Phase=Failed + PauseFailed=True |
| **Resume snapshot 问题** | Phase=Failed | Phase=**Paused** + ResumeFailed=True |
| **Resume 后 Pod 启动失败** | Phase 不变 | Phase=Failed + ResumeFailed=True |
| **重试 Pause** | 需解析 message | Phase=Running + PauseFailed=True → 直接允许 |
| **重试 Resume** | 不允许 | Phase=Paused + ResumeFailed=True → 直接允许 |
