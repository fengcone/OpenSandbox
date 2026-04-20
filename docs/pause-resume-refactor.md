# Pause/Resume 架构重构设计文档

本文档描述 BatchSandbox Pause/Resume 功能的重构方案，包含 CRD 字段变更、三条链路流程（普通链路、Pool 链路、失败链路）以及 Controller 分发逻辑。

---

## 目录

- [一、CRD 字段变更](#一crd-字段变更)
- [二、普通链路（非 Pool）](#二普通链路非-pool)
- [三、Pool 链路](#三pool-链路)
- [四、失败链路](#四失败链路)
- [五、Controller 分发逻辑](#五controller-分发逻辑)
- [六、Server 前置校验矩阵](#六server-前置校验矩阵)
- [七、各层职责边界](#七各层职责边界)

---

## 一、CRD 字段变更

### BatchSandbox（变更部分）

**Spec 新增：**

```go
// Pause 是暂停/恢复意图，Server 写入，Controller 执行后清空为 nil。
//   nil   = 无操作（初始状态）
//   true  = 请求暂停（Pause）
//   false = 请求恢复（Resume）
// 每次写入（nil→true / true→nil / nil→false）均触发 generation +1。
// 操作完成后由 Controller 清空为 nil，为下一次操作复位信号线。
// +optional
Pause *bool `json:"pause,omitempty"`
```

**Status 新增：**

```go
// Phase 是 BatchSandbox 的总体状态，由 Controller 聚合写入。
// Server 直接读此字段，无需组合多字段推导。
// 值域：Pending / Running / Pausing / Paused / Resuming / Failed
// +optional
Phase BatchSandboxPhase `json:"phase,omitempty"`

// PauseObservedGeneration 是 Controller 最近一次进入 pause/resume
// 分发逻辑时的 generation，立即 ACK 写入以防重入（幂等门控）。
// 与 metadata.generation 比对决定是否处理当前请求。
// +optional
PauseObservedGeneration int64 `json:"pauseObservedGeneration,omitempty"`
```

**Phase 枚举值：**

```go
// +kubebuilder:validation:Enum=Pending;Running;Pausing;Paused;Resuming;Failed
type BatchSandboxPhase string

const (
    BatchSandboxPhasePending  BatchSandboxPhase = "Pending"   // Pod 创建中，尚未 Running
    BatchSandboxPhaseRunning  BatchSandboxPhase = "Running"   // 正常运行
    BatchSandboxPhasePausing  BatchSandboxPhase = "Pausing"   // pause 操作进行中（commit/push）
    BatchSandboxPhasePaused   BatchSandboxPhase = "Paused"    // 已暂停，Pod 为 0
    BatchSandboxPhaseResuming BatchSandboxPhase = "Resuming"  // resume 操作进行中（Pod 启动中）
    BatchSandboxPhaseFailed   BatchSandboxPhase = "Failed"    // pause/resume 操作失败
)
```

**废弃：**

- `annotations["snapshot-status"]` → 完全删除，Phase 已覆盖主要场景

---

### SandboxSnapshot（全量重构）

**重构原则：** 纯原子能力，只做 Pod→Image commit+push，不知道 BatchSandbox 业务语义。

**Spec 删除字段：**

- `sandboxId`、`sourceBatchSandboxName` — 业务 ID，与原子能力无关
- `action`（Pause/Resume）— Resume 是 BatchSandbox 的业务语义
- `snapshotRegistry`、`snapshotType`、`snapshotPushSecret`、`resumeImagePullSecret` — 移至 Controller Manager 启动参数

**Spec 保留/新增（调用者填写，Controller 只读）：**

```go
type SandboxSnapshotSpec struct {
    // SandboxName 是目标 BatchSandbox 的名称（与 SandboxSnapshot 同 namespace）。
    // Controller 通过此字段找到 BatchSandbox → 找到 Pod → 下发 commit Job。
    // +kubebuilder:validation:Required
    SandboxName string `json:"sandboxName"`

    // 注：registry / pushSecret / snapshotType 由 Controller Manager 启动参数提供，
    //     不写入 spec。
}
```

**Status 删除字段：**

- `resumeTemplate`、`pauseVersion`、`resumeVersion`、`lastPauseAt`、`lastResumeAt`、`history[]` — BatchSandbox 业务记录，不属于原子能力

**Status 保留/新增（Controller 填写，调用者只读）：**

```go
type SandboxSnapshotStatus struct {
    Phase          SandboxSnapshotPhase  `json:"phase,omitempty"`
    Message        string                `json:"message,omitempty"`
    Containers     []ContainerSnapshot   `json:"containers,omitempty"` // Ready 后填入
    ReadyAt        *metav1.Time          `json:"readyAt,omitempty"`
    SourcePodName  string                `json:"sourcePodName,omitempty"`  // Controller 解析后写入
    SourceNodeName string                `json:"sourceNodeName,omitempty"` // 用于 Job 调度
    ObservedGeneration int64             `json:"observedGeneration,omitempty"`
}

// ContainerSnapshot 记录单个容器的快照结果（原 ContainerSnapshotResult 改名）。
type ContainerSnapshot struct {
    ContainerName string `json:"containerName"`
    ImageURI      string `json:"imageUri"`
    ImageDigest   string `json:"imageDigest,omitempty"`
}
```

---

## 二、普通链路（非 Pool）

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

━━━━━━━━━━━━━━━━━━━━━━━ PAUSE（第一次）━━━━━━━━━━━━━━━━━━━━━━━

         │  Server 前置校验（读 status）：
         │    status.phase == "Running" ✓
         │
         │  Server PATCH spec.pause = true
         ▼
  K8s: generation N → N+1

  ─────────────────────────────────────────────────────────────────
  BatchSandbox Controller Reconcile：
    generation(N+1) > pauseObservedGeneration(0) → 触发分发
    spec.pause = true（非 nil）→ handlePause()

    ① 立即 ACK（防止 requeue 重入）：
         PATCH status.pauseObservedGeneration = N+1
                status.phase                  = "Pausing"

    ② 创建 SandboxSnapshot 子资源（name="sandbox-abc"，OwnerRef→BatchSandbox）：
         spec.sandboxName = "sandbox-abc"
         // registry / pushSecret / snapshotType 来自 Controller 启动参数

    ③ requeue 等待子资源状态变化
  ─────────────────────────────────────────────────────────────────

[Pausing]
  status.phase = "Pausing"

  ─────────────────────────────────────────────────────────────────
  SandboxSnapshot Controller 接管：
    1. 读 BatchSandbox "sandbox-abc"（循环依赖，团队接受）
       → 找到 Pod "sandbox-abc-0"，node="node-1"
    2. status.sourcePodName  = "sandbox-abc-0"
       status.sourceNodeName = "node-1"
    3. 从 Controller 启动参数读取 registry / pushSecret / snapshotType
    4. 生成 imageUri：reg/abc-main:snap-gen{N+1}
    5. 创建 commit Job 调度到 node-1
    6. Job 运行：nerdctl commit + push
    7. status.phase: Pending → Committing → Ready
       status.containers[0] = {containerName:"main",
                                imageUri:"reg/abc-main:snap-gen{N+1}",
                                imageDigest:"sha256:..."}
       status.readyAt = now()
  ─────────────────────────────────────────────────────────────────

  BatchSandbox Controller completePause（Watch 到子资源变化，generation 已 ACK）：
    子资源 phase=Ready →
      ① PATCH status.phase = "Paused"
      ② 【普通模式】直接删除所有 Pod（OwnerRef 是 BatchSandbox）
      ③ PATCH spec: pause=nil  → generation: N+1 → N+2
         注：spec.replicas 保持不变（仍为 1）

  BatchSandbox Controller Reconcile（spec 清空触发 generation 递增）：
    generation(N+2) > pauseObservedGeneration(N+1) → 触发分发
    spec.pause = nil → 直接 ACK：pauseObservedGeneration = N+2 ✓
    status.phase = "Paused" → scaleBatchSandbox 跳过（不创建 Pod）

[Paused]
  spec.replicas = 1（保持不变），spec.pause = nil
  无 Pod（已被删除）
  status.phase                   = "Paused"
  status.pauseObservedGeneration = N+2
  SandboxSnapshot "sandbox-abc" 子资源存在（存在 ≡ Paused）

  Server 感知：status.phase == "Paused" → pause 完成 ✓

━━━━━━━━━━━━━━━━━━━━━━━ RESUME（第一次）━━━━━━━━━━━━━━━━━━━━━━

         │  Server 前置校验（读 status）：
         │    status.phase == "Paused" ✓
         │
         │  Server PATCH spec.pause = false
         ▼
  K8s: generation N+2 → N+3

  ─────────────────────────────────────────────────────────────────
  BatchSandbox Controller Reconcile：
    generation(N+3) > pauseObservedGeneration(N+2) → 触发分发
    spec.pause = false（非 nil）→ handleResume()

    ① 立即 ACK：
         PATCH status.pauseObservedGeneration = N+3
                status.phase                  = "Resuming"

    ② Get SandboxSnapshot "sandbox-abc"：
         校验 status.phase == "Ready" ✓
         读 status.containers[0].imageUri = "reg/abc-main:snap-gen{N+1}"

    ③ 替换 spec.template 对应容器 image：
         spec.template.spec.containers[0].image = "reg/abc-main:snap-gen{N+1}"

    ④ PATCH spec: pause=nil  → generation: N+3 → N+4
       注：spec.replicas 保持不变（仍为 1）

    ⑤ 删除 SandboxSnapshot "sandbox-abc"
       （Resume 触发后立即删除：SandboxSnapshot 存在 ≡ Paused，语义闭环）

    ⑥ scaleBatchSandbox()：用新 image 创建 Pod "sandbox-abc-0"
       （phase 从 Paused → Resuming，scaleBatchSandbox 正常执行）
  ─────────────────────────────────────────────────────────────────

  BatchSandbox Controller Reconcile（spec 变化触发）：
    generation(N+4) > pauseObservedGeneration(N+3)，pause=nil → ACK ✓
    pauseObservedGeneration = N+4

  Pod "sandbox-abc-0" 进入 Running →
    status.phase = "Running"

[Running]
  spec.template image 已永久替换为 "reg/abc-main:snap-gen{N+1}"
  SandboxSnapshot 子资源已删除
  Server 感知：status.phase == "Running" → resume 完成 ✓

━━━━━━━━━━━━━━━━━━━━━━━ PAUSE（第二次，验证幂等）━━━━━━━━━━━━━━━

         │  Server 前置校验：status.phase == "Running" ✓
         │
         │  Server PATCH spec.pause = true
         │  （旧值 nil → true，generation 必然递增）
         ▼
  K8s: generation N+4 → N+5

  BatchSandbox Controller Reconcile：
    generation(N+5) > pauseObservedGeneration(N+4) → 触发分发  ✓
    spec.pause = true → handlePause()
    → 走与第一次完全相同的流程
    → 新 imageUri：reg/abc-main:snap-gen{N+5}（携带新 generation，天然区分）
    → 旧 SandboxSnapshot 已不存在（上次 Resume 时已删），直接创建新的 ✓
```

---

## 三、Pool 链路

```
[创建（Pool 模式）]
  BatchSandbox:
    spec.replicas=1, spec.poolRef="my-pool", spec.template=nil
    spec.pause=nil
         │
         ▼  Pool Controller 分配预热 Pod
[Running]
  Pod: {name: "pool-pod-xyz", node: "node-2"} Running（来自 Pool 缓冲）
  spec.template = nil
  status.phase  = "Running"

━━━━━━━━━━━━━━━━━━━━━━━ PAUSE 请求 ━━━━━━━━━━━━━━━━━━━━━━━

         │  Server 前置校验：status.phase == "Running" ✓
         │  Server PATCH spec.pause = true
         ▼
  K8s: generation N → N+1

  ─────────────────────────────────────────────────────────────────
  BatchSandbox Controller handlePause()：

    【Pool 专属前置步骤】
    检测到 spec.template==nil && spec.poolRef!="" →
      读 Pool CR "my-pool" → pool.spec.template
      PATCH spec.template = pool.spec.template
      （固化模板到 BatchSandbox 自身，Resume 时不再依赖 Pool CR）
      （此 PATCH 不触发 Pod 重建：IsPooledMode=true，scaleBatchSandbox 不执行）

    ① 立即 ACK：
         status.pauseObservedGeneration = N+1
         status.phase                   = "Pausing"

    ② 创建 SandboxSnapshot（与普通链路相同）：
         spec.sandboxName = "sandbox-abc"

  SandboxSnapshot Controller：
    读 BatchSandbox → 找到 Pod "pool-pod-xyz"（通过 alloc-status annotation）
    commit + push → phase=Ready

  BatchSandbox Controller completePause（phase=Ready）：
    ① PATCH status.phase = "Paused"
    ② 【Pool 模式】设置 alloc-release annotation：
         annotations["alloc-release"] = {"pods": ["pool-pod-xyz"]}
       → Pool Controller Watch 到变化 → 从 podAllocation 移除
       → Pod 变为 idle，可被 Pool 重新分配
       → Pool Controller 自动补充预热 Pod 维持容量
    ③ PATCH spec: pause=nil → generation 递增 → ACK ✓
       注：spec.replicas 保持不变，spec.poolRef 保留（Resume 时脱 Pool）
  ─────────────────────────────────────────────────────────────────

[Paused（Pool 模式）]
  spec.poolRef   = "my-pool"（保留）
  spec.template  = {已固化的 Pool template}（新增）
  status.phase   = "Paused"
  SandboxSnapshot 存在

━━━━━━━━━━━━━━━━━━━━━━━ RESUME 请求（关键：脱 Pool）━━━━━━━━━━━━━━

         │  Server 前置校验：status.phase == "Paused" ✓
         │  Server PATCH spec.pause = false
         ▼
  K8s: generation N+2 → N+3

  ─────────────────────────────────────────────────────────────────
  BatchSandbox Controller handleResume()：

    ① 立即 ACK：pauseObservedGeneration = N+3，phase = "Resuming"
    ② Get SandboxSnapshot → imageUri = "reg/abc-main:snap-gen{N+1}"
    ③ 替换 spec.template 中对应容器 image
    ④ 【Pool 专属】spec.poolRef = ""（脱 Pool）
       → Pool Controller 感知：该 BatchSandbox 从 Pool 视图消失
    ⑤ PATCH spec: pause=nil
       注：spec.replicas 保持不变（仍为 1）
    ⑥ 删除 SandboxSnapshot
    ⑦ scaleBatchSandbox()：自主创建新 Pod（OwnerRef → BatchSandbox，不走 Pool 分配）

  Pool Controller 感知：spec.poolRef="" → 该 BatchSandbox 从 Pool 视图消失，互不影响

  Pod Running → status.phase = "Running"
  ─────────────────────────────────────────────────────────────────

[Running（已永久脱 Pool）]
  spec.poolRef  = ""
  spec.template = {Pool template，image 已替换为快照 image}
  后续 pause/resume → 走普通链路 ✓

━━━━━━━━━━━━━━━━━━━━━━━ 多次 PAUSE/RESUME（Pool 脱池后）━━━━━━━━━━━

  第二次 Pause：
    spec.poolRef="" → 走普通链路
    spec.template 已固化，无需再读 Pool CR ✓
    imageUri：reg/abc-main:snap-gen{N+5}（新 generation）

  第二次 Resume：
    与普通链路完全相同 ✓
```

---

## 四、失败链路

```
━━━ 场景 A：commit/push 失败 ━━━

  SandboxSnapshot Controller：Job 失败（registry 不可达 / secret 错误）
    status.phase   = "Failed"
    status.message = "push image failed: connection refused"

         │
         ▼
  BatchSandbox Controller completePause（子资源 phase=Failed）：
    ① status.phase = "Failed"
    ② 不删除 Pod / 不设置 alloc-release（Pod 仍 Running，sandbox 可继续使用）
    ③ PATCH spec: pause=nil → generation 递增 → ACK ✓

[Failed / Sandbox Still Running]
  Server 读 status.phase=="Failed" → 返回 pause 失败
  sandbox 仍可正常使用

       │  【重试 Pause】
       │  Server 前置校验：status.phase ∉ {Pausing} ✓
       │  Server PATCH spec.pause=true（nil→true，generation 递增）
       ▼
  Controller：删除旧 SandboxSnapshot（phase=Failed）
              → 创建新 SandboxSnapshot → 重走流程 ✓

━━━ 场景 B：Pause 时 Pod 不存在 ━━━

  SandboxSnapshot Controller：
    读 BatchSandbox "sandbox-abc" → 无 Running Pod（节点宕机 / 意外删除）
    → status.phase   = "Failed"
      status.message = "source pod not found"

  BatchSandbox Controller：
    ① status.phase = "Failed"
    ② 清空 spec.pause=nil ✓

[Failed]（sandbox 本身也不可用）

━━━ 场景 C：Pool 模式固化 template 失败 ━━━

  BatchSandbox Controller handlePause() 前置步骤：
    已 ACK：pauseObservedGeneration = N+1
    读 Pool CR "my-pool" → NotFound 或 pool.spec.template==nil
    → status.phase   = "Failed"
      status.message = "pool CR not found or template empty"
    → 清空 spec.pause=nil ✓

[Failed / Pool Pod 仍在运行]

━━━ 场景 D：Resume 时 SandboxSnapshot 不存在或未 Ready ━━━

  BatchSandbox Controller handleResume()：
    已 ACK：pauseObservedGeneration = N+3，phase = "Resuming"
    Get SandboxSnapshot "sandbox-abc" → NotFound
    OR status.phase != "Ready"
    → status.phase   = "Failed"
      status.message = "snapshot not ready: phase=Failed"
    → 清空 spec.pause=nil ✓

  通常不应发生（Server 前置校验 status.phase=="Paused"，Paused 隐含快照存在且 Ready）

━━━ 场景 E：Resume 后 Pod 启动失败 ━━━

  spec.replicas=1，SandboxSnapshot 已删除，Pod 创建中
  Pod 持续 ImagePullBackOff / CrashLoopBackOff

  BatchSandbox Controller：
    Pod 无法变为 Running
    → status.phase   = "Failed"
      status.message = "resume pod failed: ImagePullBackOff"

[Failed]
  ⚠️ SandboxSnapshot 已删除 → 无法直接重试 Resume
  需先修复 image 问题，重新 Pause 获取新快照

━━━ 场景 F：spec 清空失败（双写异常恢复）━━━

  Controller 完成操作（Pause 成功）：
    PATCH status（pauseObservedGeneration=N+1, phase="Paused"）✓
    PATCH spec（pause=nil）✗（网络抖动）

  下次 Reconcile：
    generation(N+1) == pauseObservedGeneration(N+1)  ← 已 ACK
    spec.pause = true（残留，非 nil）
    → 进入补清空分支：clearPause() 幂等重试
    → 清空成功 → generation: N+1→N+2 → pause=nil → ACK ✓
    → 不会重复执行 Pause 操作（pauseObservedGeneration 已对齐保护）✓
```

---

## 五、Controller 分发逻辑

BatchSandbox Controller Reconcile 中的 snapshot 分发逻辑（5 个 Case）：

| generation 与 pauseObservedGeneration | spec.pause 值 | 执行动作 |
|---|---|---|
| `generation > pauseObservedGeneration` | `true` | `handlePause()` |
| `generation > pauseObservedGeneration` | `false` | `handleResume()` |
| `generation > pauseObservedGeneration` | `nil` | 仅 ACK（spec 其他字段变化，无 pause 意图） |
| `generation == pauseObservedGeneration` | `非 nil` | 补清空（spec.pause 清空失败的幂等恢复） |
| `generation == pauseObservedGeneration` | `nil` | 正常 Pod 调度（`scaleBatchSandbox`） |

**关键规则：**
- `spec.pause == nil` 时永远不执行 pause/resume 操作，无论 generation 差多少
- `generation == pauseObservedGeneration && pause != nil` 在正常路径下不可能出现，唯一来源是 spec 清空失败，可安全用作补清空触发条件

---

## 六、Server 前置校验矩阵

| `status.phase` | 允许 Pause？ | 允许 Resume？ | sandbox 对外状态 |
|---|---|---|---|
| `Pending` | ✗ 409 | ✗ 409 | 创建中，尚未就绪 |
| `Running` | ✓ | ✗ 409 | 正常运行 |
| `Pausing` | ✗ 409 | ✗ 409 | 操作进行中 |
| `Paused` | ✗ 409 | ✓ | 已暂停 |
| `Resuming` | ✗ 409 | ✗ 409 | 操作进行中 |
| `Failed` | ✓（重试）| ✗ 409 | 失败，可重试 Pause |

> **注：** Failed 时只允许重试 Pause，不允许 Resume（SandboxSnapshot 可能已删，Resume 无快照可用）

---

## 七、各层职责边界

| 层 | 写 | 读 |
|---|---|---|
| **Server** | `spec.pause`（true/false/nil） | `status.phase` |
| **BatchSandbox Controller** | `status.phase`、`status.pauseObservedGeneration`、`spec.template`（resume 时替换 image）、`spec.poolRef`（resume 时清空）、`spec.pause`（清空）、`annotations[alloc-release]`（Pool 模式 pause 时释放 Pod）、Pod 删除（普通模式 pause 时） | `spec.pause`、SandboxSnapshot.status |
| **SandboxSnapshot Controller** | `status.phase`、`status.message`、`status.containers`（含 imageDigest）、`status.sourcePodName`、`status.sourceNodeName` | `spec.sandboxName`（读 BatchSandbox 找 Pod）、Controller 启动参数（registry/pushSecret/snapshotType） |

**关键约束：**
- SandboxSnapshot Controller 需读 BatchSandbox 找到 Pod（循环依赖，团队接受）
- SandboxSnapshot Controller 不执行任何 BatchSandbox 业务逻辑（不删 Pod、不设 alloc-release、不脱 Pool）
- `spec.pause` 清空由 BatchSandbox Controller 负责，Server 不主动清空
- SandboxSnapshot 随 BatchSandbox OwnerReference 级联删除；Resume 触发后由 BatchSandbox Controller 主动删除
- Pause 时不修改 `spec.replicas`，普通模式直接删除 Pod，Pool 模式设置 `alloc-release` annotation
