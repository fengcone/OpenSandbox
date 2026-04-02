# Pause/Resume 优化方案 A

## 当前问题

数据冗余流：
```
Server pause_sandbox() ──► 读 BatchSandbox.spec.pausePolicy ──► 写入 SandboxSnapshot.spec
                                      │
                                      ▼
Controller ensureResolved() ─────────► 又读 BatchSandbox.spec.Template/PoolRef
```

BatchSandbox 被读两次，pausePolicy 数据被写两次（BS → Snapshot 字段重复填充）。

## 优化后数据流

精简流：
```
Server pause_sandbox() ──► 创建最简 SandboxSnapshot CR
                              ├── spec.sandboxId
                              ├── spec.sourceBatchSandboxName  (唯一关键引用)
                              └── spec.pausedAt

Controller ensureResolved() ──► 读 BatchSandbox (通过 sourceBatchSandboxName)
                                  ├── 读取 pausePolicy (registry, secrets, type)
                                  ├── 读取 Template/PoolRef 解析容器模板
                                  ├── 构建 ContainerSnapshots[]
                                  ├── 构建 ResumeTemplate
                                  └── 写回 Snapshot.spec (补全所有字段)
```

## 改动点

| 组件 | 当前行为 | 优化后 |
|------|---------|--------|
| **Server pause_sandbox()** | 读取 pausePolicy 并写入 Snapshot.spec | **不读**，只写最简 CR（sandboxId, sourceBatchSandboxName, pausedAt） |
| **Controller ensureResolved()** | 只读 Template/PoolRef | **新增**：读 pausePolicy，填充 snapshot.spec.{registry, secrets, resumeTemplate} |

## 向后兼容

- 旧数据：已有 Snapshot CR（含完整 pausePolicy）→ `ensureResolved()` 检测到已填充，直接跳过
- 新数据：最简 Snapshot CR → `ensureResolved()` 从 BatchSandbox 补全

## 待讨论问题

1. **pausePolicy 字段废弃？** Snapshot.spec 是否还需要 `pausePolicy` 子结构，还是扁平化为 `snapshotRegistry`, `snapshotPushSecretName` 等顶层字段？
2. **resumeTemplate 构建**：完全由 Controller 构建，Server 不参与，是否可行？
3. **多容器命名冲突**：`registry/sandboxId-containerName:snapshot` 命名是否满足需求？
