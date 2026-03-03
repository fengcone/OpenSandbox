### 背景
在 OpenSandbox 项目中，针对 Sandbox 提供了原生的 pause 和 resume 语义，但是在 Kubernets 中，我们还没有实现这个功能，需要制定在 Kubernets 环境下 Sandbox 的 puase 和 resume 的方案。

### Puase/Resume级别
在讨论这个问题之前，我们应该去了解社区针对容器的 puase 和 resume 有什么方案，各自方案有什么优缺点，特别是在 agent 使用 Sandbox 的领域，可能是我们重点关注的领域。

如下是 deepseek 总结的几种方案和使用场景。

| <font style="color:rgb(15, 17, 21);">方案</font> | <font style="color:rgb(15, 17, 21);">进程状态</font> | <font style="color:rgb(15, 17, 21);">内存状态</font> | <font style="color:rgb(15, 17, 21);">文件状态</font> | <font style="color:rgb(15, 17, 21);">跨生命周期</font> |
| --- | --- | --- | --- | --- |
| <font style="color:rgb(15, 17, 21);">Cgroup Freezer</font> | <font style="color:rgb(15, 17, 21);">✅</font><font style="color:rgb(15, 17, 21);">（同一容器内）</font> | <font style="color:rgb(15, 17, 21);">✅</font><font style="color:rgb(15, 17, 21);">（内存驻留）</font> | <font style="color:rgb(15, 17, 21);">✅</font><font style="color:rgb(15, 17, 21);">（已刷盘）</font> | <font style="color:rgb(15, 17, 21);">❌</font> |
| <font style="color:rgb(15, 17, 21);">Volumes</font> | <font style="color:rgb(15, 17, 21);">❌</font> | <font style="color:rgb(15, 17, 21);">❌</font> | <font style="color:rgb(15, 17, 21);">✅</font><font style="color:rgb(15, 17, 21);">（仅挂载点）</font> | <font style="color:rgb(15, 17, 21);">✅</font> |
| <font style="color:rgb(15, 17, 21);">CRIU</font> | <font style="color:rgb(15, 17, 21);">✅</font> | <font style="color:rgb(15, 17, 21);">✅</font> | <font style="color:rgb(15, 17, 21);">✅</font> | <font style="color:rgb(15, 17, 21);">✅</font> |
| <font style="color:rgb(15, 17, 21);">RootFS</font> | <font style="color:rgb(15, 17, 21);">❌</font> | <font style="color:rgb(15, 17, 21);">❌</font> | <font style="color:rgb(15, 17, 21);">✅</font><font style="color:rgb(15, 17, 21);">（全文件系统）</font> | <font style="color:rgb(15, 17, 21);">✅</font> |
| <font style="color:rgb(15, 17, 21);">VM Snapshot</font> | <font style="color:rgb(15, 17, 21);">✅</font> | <font style="color:rgb(15, 17, 21);">✅</font> | <font style="color:rgb(15, 17, 21);">✅</font> | <font style="color:rgb(15, 17, 21);">✅</font> |


[AI Agent Sandbox 暂停与恢复方案分析](https://aliyuque.antfin.com/search-infra/pufssx/kz6cglu5qz1g2000)



### 演进路线
经过讨论，基本确定 volumes --> rootfs --> vm snapshot 的演进路径

![](https://intranetproxy.alipay.com/skylark/lark/__mermaid_v3/9edfd804f0f8f9ffc317e1f0c746fe6e.svg)

#### <font style="color:rgb(15, 17, 21);">阶段收益分析</font>
| <font style="color:rgb(15, 17, 21);">演进阶段</font> | <font style="color:rgb(15, 17, 21);">解决的问题</font> | <font style="color:rgb(15, 17, 21);">对 AI Agent 的价值</font> | <font style="color:rgb(15, 17, 21);">投入成本</font> |
| --- | --- | --- | --- |
| **<font style="color:rgb(15, 17, 21);">Volumes</font>** | <font style="color:rgb(15, 17, 21);">基础数据不丢失</font> | <font style="color:rgb(15, 17, 21);">保证核心产出物安全</font> | <font style="color:rgb(15, 17, 21);">低（成熟技术）</font> |
| **<font style="color:rgb(15, 17, 21);">RootFS</font>** | <font style="color:rgb(15, 17, 21);">环境配置不丢失</font> | <font style="color:rgb(15, 17, 21);">减少重复安装依赖，加快启动</font> | <font style="color:rgb(15, 17, 21);">中（镜像管理）</font> |
| **<font style="color:rgb(15, 17, 21);">VM Snapshot</font>** | <font style="color:rgb(15, 17, 21);">完整状态不丢失</font> | <font style="color:rgb(15, 17, 21);">支持复杂有状态场景，体验最好</font> | <font style="color:rgb(15, 17, 21);">高（需定制）</font> |




1. **<font style="color:rgb(15, 17, 21);">80/20 原则</font>**<font style="color:rgb(15, 17, 21);">：80% 的 AI Agent 任务可以通过文件级 checkpoint 实现恢复，只有 20% 的复杂场景需要完整快照</font>
2. **<font style="color:rgb(15, 17, 21);">渐进式投资</font>**<font style="color:rgb(15, 17, 21);">：先解决最普遍的数据持久化需求，再逐步完善环境管理，最后攻克完整状态保存</font>
3. **<font style="color:rgb(15, 17, 21);">技术成熟度匹配</font>**<font style="color:rgb(15, 17, 21);">：优先采用 Kubernetes 原生支持的技术（Volumes、RootFS），将需要定制的 VM Snapshot 作为长期目标</font>
4. **<font style="color:rgb(15, 17, 21);">用户体验权衡</font>**<font style="color:rgb(15, 17, 21);">：文件级恢复要求 Agent 代码配合（定期保存状态），但实现简单；完整快照对应用无侵入，但运维复杂</font>

### API 以及系统架构设计

#### 整体架构

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                           API Layer (FastAPI)                                │
│  POST /sandboxes/{id}/pause    POST /sandboxes/{id}/resume                   │
└─────────────────────────────────────────────────────────────────────────────┘
                                        │
                                        ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│                      KubernetesSandboxService                                │
│  - 更新 CR spec.pausePolicy                                                │
│  - 返回 202, 状态变更为 Pausing                                             │
└─────────────────────────────────────────────────────────────────────────────┘
                                        │
                    ┌───────────────────┴───────────────────┐
                    ▼                                       ▼
┌──────────────────────────────┐        ┌──────────────────────────────────┐
│  方案一: 扩展 BatchSandbox    │        │  方案二: 独立 SandboxPause CR    │
│  spec.pausePolicy             │        │  spec.targetSandboxRef           │
│  spec.pausedReplicas (可选)   │        │  spec.pausePolicy                │
└──────────────────────────────┘        └──────────────────────────────────┘
                    │                                       │
                    └───────────────────┬───────────────────┘
                                        ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│                        DaemonSet Controller                                  │
│  (或扩展现有 TaskExecutor)                                                   │
│  - 监听 CR 变更                                                             │
│  - 识别本节点上的 Pod                                                      │
│  - 执行 pause/resume 操作                                                   │
│  - 更新 CR status                                                           │
└─────────────────────────────────────────────────────────────────────────────┘
                                        │
                    ┌───────────────────┴───────────────────┐
                    ▼                                       ▼
┌──────────────────────────────┐        ┌──────────────────────────────────┐
│   RootFS 级别                 │        │   VM Snapshot 级别               │
│   文件系统持久化为镜像         │        │   Kata/Firecracker snapshot      │
│   可跨节点恢复                 │        │   完整状态持久化                 │
└──────────────────────────────┘        └──────────────────────────────────┘
```

#### 状态机

```
                    ┌─────────────────────────────────────┐
                    │                                     │
                    ▼                                     │
    ┌──────────┐   pause   ┌──────────┐   resume   ┌──────────┐
    │ Running  │ ────────▶ │ Pausing  │ ──────────▶ │ Running  │
    └──────────┘           └────┬─────┘            └──────────┘
                                  │
                    ┌─────────────┘
                    │ completed
                    ▼
                ┌──────────┐
                │  Paused  │
                └──────────┘
```

#### Server API (保持现有接口)

```
POST /sandboxes/{id}/pause
→ 202 Accepted, status.state = "Pausing"

POST /sandboxes/{id}/resume
→ 202 Accepted, status.state = "Running"
```

#### CRD 设计对比

**方案一：扩展现有 CR (BatchSandbox/AgentSandbox)**

```yaml
# BatchSandbox CR 扩展
spec:
  replicas: 3
  # ... 现有字段
  pausePolicy:              # 新增
    type: "rootfs" | "vmsnapshot" | "auto"
    pausedReplicas: 3       # 可选，选择性暂停

status:
  pauseState: "None" | "Pausing" | "Paused" | "Failed"
  pauseStrategy: "rootfs" | "vmsnapshot"
  pausedReplicas: 0
  pauseConditions:
    - type: "PauseProgressing"
      status: "True"
      reason: "CommittingFileSystem"
      message: "Committing container rootfs to image..."
  pausedAt: "2025-03-03T10:00:00Z"
  imageRef: "registry.example.com/sandbox/abc123:paused-20250303-100030"  # RootFS
  snapshotRef: "pvc-abc123-snapshot"  # VM Snapshot
```

**方案二：独立 SandboxPause CR**

```yaml
apiVersion: opensandbox.io/v1alpha1
kind: SandboxPause
metadata:
  name: sandbox-{id}-pause
spec:
  targetRef:
    kind: "BatchSandbox" | "AgentSandbox"
    name: "sandbox-abc123"
  pausePolicy:
    type: "rootfs" | "vmsnapshot" | "auto"
    pausedReplicas: 3       # 可选

status:
  state: "None" | "Pausing" | "Paused" | "Failed"
  observedGeneration: 1
  pauseStrategy: "rootfs" | "vmsnapshot"
  pausedReplicas: 0
```

| 维度 | 方案一 (扩展现有 CR) | 方案二 (独立 CR) |
|------|---------------------|------------------|
| CRD 数量 | 无新增 | +1 个 CRD |
| API 复杂度 | spec 字段增多 | 职责分离清晰 |
| Controller 逻辑 | 在现有 controller 中扩展 | 独立 controller |
| 与已有系统集成 | 紧耦合 | 松耦合 |
| 适用场景 | 快速实现 | 长期演进 |

#### 实现策略

**DaemonSet Controller 职责**

```
1. Watch CR 变更 (pausePolicy 字段变化)
2. 识别本节点上的相关 Pod
3. 根据 pausePolicy.type 选择实现策略:
   - "auto": 根据 runtimeClass 自动选择
   - "rootfs": 提交 RootFS 为镜像
   - "vmsnapshot": VM snapshot API
4. 执行操作并更新 CR status
```

**RootFS 级别实现 (文件系统持久化)**

```yaml
pausePolicy:
  type: "rootfs"
  # 或 "auto" + runtimeClass = runc/gVisor

# 实现方式:
# 1. 停止容器
# 2. 提交容器 rootfs 变更为新镜像层
# 3. 推送到镜像仓库
# 4. 删除原 Pod
```

| 特点 | 说明 |
|------|------|
| 保存内容 | 文件系统变更（已安装的包、配置文件、生成的内容） |
| 不保存 | 进程状态、内存数据、网络连接 |
| 跨节点 | ✅ 支持 |
| 适用场景 | Agent 安装了新依赖、需要跨节点迁移、环境配置需要保留 |

**VM Snapshot 级别实现**

```yaml
pausePolicy:
  type: "vmsnapshot"
  # 或 "auto" + runtimeClass = kata/firecracker

# 实现方式:
# 1. 调用 VM 运行时快照 API (内存 + 磁盘)
# 2. 保存到持久化存储 (PVC/对象存储)
# 3. 删除原 Pod
```

| 特点 | 说明 |
|------|------|
| 保存内容 | 内存 + 进程 + 文件系统 + 网络 |
| 恢复精度 | 精确恢复到暂停点 |
| 存储成本 | 高（整机快照） |
| 适用场景 | 复杂有状态的 Agent、需要保持内存中的计算状态、浏览器自动化会话 |

**两种级别对比**

| 维度 | RootFS 级别 | VM Snapshot 级别 |
|------|-------------|------------------|
| 保存内容 | 文件系统变更 | 内存 + 进程 + 文件系统 + 网络 |
| 恢复精度 | 重启后从入口点开始 | 精确恢复到暂停点 |
| 存储成本 | 低（仅镜像层差异） | 高（整机快照） |
| 跨节点 | ✅ 支持 | ✅ 支持 |
| 运行时要求 | 任意 | Kata/Firecracker |

**auto 策略选择逻辑**

```
runtimeClass          │ pausePolicy.type │ 实际使用
─────────────────────────────────────────────────────
gvisor, runsc         │ auto             │ rootfs
kata, kata-fc         │ auto             │ vmsnapshot
kata, kata-fc         │ rootfs           │ rootfs (降级)
gvisor, runsc         │ vmsnapshot       │ error (不支持)
```

#### 错误处理

| 场景 | 处理方式 |
|------|----------|
| Pod 不存在 | status.pauseState = "Failed", reason = "PodNotFound" |
| 镜像推送失败 | status.pauseState = "Failed", 保留原 Pod |
| Snapshot 存储满 | status.pauseState = "Failed", reason = "StorageInsufficient" |
| 运行时不匹配 | status.pauseState = "Failed", reason = "RuntimeNotSupported" |

#### API 交互流程

```
┌──────────┐      ┌─────────────────────┐      ┌──────────────┐
│  Client  │      │  FastAPI Server     │      │ DaemonSet    │
└─────┬────┘      └──────────┬──────────┘      └──────┬───────┘
      │                      │                        │
      │ POST /sandboxes/{id}/pause                      │
      │─────────────────────▶│                        │
      │                      │                        │
      │                      │ patch CR               │
      │                      │ pausePolicy.type=...   │
      │                      │──────────────────────▶│
      │                      │                        │
      │ 202 Accepted         │                        │ 识别本节点 Pod
      │ status.state=Pausing │                        │ 执行 pause
      │◀─────────────────────│                        │
      │                      │                        │
      │                      │                        │ 更新 status
      │                      │◀───────────────────────│
      │                      │                        │
      │ GET /sandboxes/{id}  │                        │
      │─────────────────────▶│                        │
      │                      │                        │
      │ status.state=Paused  │                        │
      │◀─────────────────────│                        │
```

#### 实现优先级

```
P0 (第一阶段):
  ├── 扩展 BatchSandbox/AgentSandbox CR (pausePolicy 字段)
  ├── DaemonSet Controller 基础框架
  ├── RootFS 级别实现 (文件系统持久化为镜像)
  └── Server API pause/resume 实现

P1 (第二阶段):
  ├── VM Snapshot 级别实现 (Kata/Firecracker)
  ├── auto 策略自动选择
  └── pausedReplicas 选择性暂停

P2 (增强):
  ├── 独立 SandboxPause CRD (可选)
  ├── 跨节点恢复
  └── 监控指标集成
```

