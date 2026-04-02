# commit-snapshot 重构方案：Go + nerdctl

## 背景

当前 `commit-snapshot.sh` 使用 `ctr containers commit` 命令，但该命令在 containerd 中**不存在**（`ctr` 没有 commit 子命令）。需要重构为使用 `nerdctl` 作为替代方案。

## 目标

- 用 Go 程序替代 Shell 脚本
- 使用 `nerdctl` 实现 container commit/push
- 保留 `crictl` 用于 CRI Pod/Container 查找
- 保持原子性：pause all → commit all → resume all

## 架构

```
commit-snapshot.go
    ├── 参数解析
    ├── Pod/Container 查找 (crictl)
    ├── Pause 容器 (nerdctl pause)
    ├── Commit 容器 (nerdctl commit)
    ├── Push 镜像 (nerdctl push)
    ├── Resume 容器 (nerdctl unpause)
    └── 输出 digest
```

## 伪代码实现

```go
package main

import (
    "context"
    "fmt"
    "os"
    "os/exec"
    "strings"
    "time"
)

type ContainerSpec struct {
    Name string
    URI  string
}

func main() {
    // 1. 解析参数
    args := os.Args[1:]
    if len(args) < 3 {
        fmt.Fprintln(os.Stderr, "Usage: commit-snapshot <pod_name> <namespace> <container1:uri1> [container2:uri2...]")
        os.Exit(1)
    }
    
    podName := args[0]
    namespace := args[1]
    containerSpecs := parseContainerSpecs(args[2:])
    
    // 2. 找 Pod Sandbox ID
    podSandboxID, err := getPodSandboxID(podName, namespace)
    if err != nil {
        fmt.Fprintf(os.Stderr, "Failed to find pod: %v\n", err)
        os.Exit(1)
    }
    
    // 3. 暂停所有容器
    pausedContainers := []string{}
    for _, spec := range containerSpecs {
        containerID, err := getContainerID(podSandboxID, spec.Name)
        if err != nil {
            resumeAll(pausedContainers)
            fmt.Fprintf(os.Stderr, "Failed to find container %s: %v\n", spec.Name, err)
            os.Exit(1)
        }
        
        if err := pauseContainer(containerID); err != nil {
            resumeAll(pausedContainers)
            fmt.Fprintf(os.Stderr, "Failed to pause container %s: %v\n", spec.Name, err)
            os.Exit(1)
        }
        pausedContainers = append(pausedContainers, containerID)
    }
    
    // 4. Commit & Push 所有容器
    digests := map[string]string{}
    for _, spec := range containerSpecs {
        containerID, _ := getContainerID(podSandboxID, spec.Name)
        
        digest, err := commitAndPush(containerID, spec.URI)
        if err != nil {
            resumeAll(pausedContainers)
            fmt.Fprintf(os.Stderr, "Failed to commit container %s: %v\n", spec.Name, err)
            os.Exit(1)
        }
        digests[spec.Name] = digest
    }
    
    // 5. Resume 所有容器
    resumeAll(pausedContainers)
    
    // 6. 输出 digest
    for name, digest := range digests {
        fmt.Printf("SNAPSHOT_DIGEST_%s=%s\n", strings.ToUpper(name), digest)
    }
}

// 解析 container:uri 参数
func parseContainerSpecs(args []string) []ContainerSpec {
    specs := []ContainerSpec{}
    for _, arg := range args {
        parts := strings.SplitN(arg, ":", 2)
        if len(parts) == 2 {
            specs = append(specs, ContainerSpec{Name: parts[0], URI: parts[1]})
        }
    }
    return specs
}

// crictl 查找 Pod Sandbox ID
func getPodSandboxID(podName, namespace string) (string, error) {
    cmd := exec.Command("crictl", "pods", "--name", podName, "--namespace", namespace, "-q")
    output, err := cmd.Output()
    if err != nil {
        return "", err
    }
    return strings.TrimSpace(string(output)), nil
}

// crictl 查找 Container ID
func getContainerID(podSandboxID, containerName string) (string, error) {
    cmd := exec.Command("crictl", "ps", "--pod", podSandboxID, "--name", containerName, "-q")
    output, err := cmd.Output()
    if err != nil {
        return "", err
    }
    return strings.TrimSpace(string(output)), nil
}

// nerdctl pause 容器
func pauseContainer(containerID string) error {
    cmd := exec.Command("nerdctl", "pause", containerID)
    return cmd.Run()
}

// nerdctl unpause 容器
func resumeContainer(containerID string) error {
    cmd := exec.Command("nerdctl", "unpause", containerID)
    return cmd.Run()
}

// 批量 resume（错误恢复）
func resumeAll(containerIDs []string) {
    for _, id := range containerIDs {
        _ = resumeContainer(id)
    }
}

// nerdctl commit + push
func commitAndPush(containerID, targetImage string) (string, error) {
    // Commit
    cmd := exec.Command("nerdctl", "commit", containerID, targetImage)
    if err := cmd.Run(); err != nil {
        return "", fmt.Errorf("commit failed: %w", err)
    }
    
    // Push
    cmd = exec.Command("nerdctl", "push", targetImage)
    if err := cmd.Run(); err != nil {
        return "", fmt.Errorf("push failed: %w", err)
    }
    
    // 获取 digest
    digest, err := getImageDigest(targetImage)
    if err != nil {
        return "", fmt.Errorf("get digest failed: %w", err)
    }
    
    return digest, nil
}

// 获取镜像 digest
func getImageDigest(imageRef string) (string, error) {
    // 使用 nerdctl inspect
    cmd := exec.Command("nerdctl", "inspect", "--format", "{{.Id}}", imageRef)
    output, err := cmd.Output()
    if err == nil {
        return strings.TrimSpace(string(output)), nil
    }
    
    // fallback: ctr images list
    cmd = exec.Command("ctr", "--namespace", "k8s.io", "images", "list", "--format", "json")
    // ... 解析 JSON 找 digest
    
    return "", fmt.Errorf("failed to get digest for %s", imageRef)
}
```

## 工具对比

| 功能 | 原方案 (Shell + ctr) | 新方案 (Go + nerdctl) |
|------|---------------------|----------------------|
| 找 Pod/Container | crictl | crictl（保留） |
| Pause/Resume | ctr tasks pause/resume | nerdctl pause/unpause |
| Commit | `ctr containers commit` ❌不存在 | nerdctl commit ✅ |
| Push | ctr images push | nerdctl push |
| 流程控制 | Shell && 串联 | Go 代码控制 |
| 错误恢复 | trap | Go defer/resumeAll |

## Dockerfile

```dockerfile
# 构建阶段
FROM golang:1.24-alpine AS builder
WORKDIR /app
COPY commit-snapshot.go go.mod go.sum ./
RUN go mod download && go build -o commit-snapshot commit-snapshot.go

# 运行阶段
FROM alpine:3.20
RUN apk add --no-cache \
    nerdctl \
    crictl \
    ca-certificates

# nerdctl 配置
ENV NERDCTL_TINI_BINARY=tini

COPY --from=builder /app/commit-snapshot /usr/local/bin/
ENTRYPOINT ["commit-snapshot"]
```

## 依赖清单

| 工具 | 用途 | 安装方式 |
|------|------|---------|
| nerdctl | container commit/push/pause | apk add nerdctl |
| crictl | CRI Pod/Container 查找 | apk add crictl |
| ca-certificates | HTTPS 证书 | apk add ca-certificates |

## 注意事项

1. **nerdctl 配置**：可能需要配置 `/etc/nerdctl/nerdctl.toml` 指定默认 namespace
2. **权限**：Job Pod 需要 privileged 或适当权限访问 containerd socket
3. **镜像大小**：Go 静态编译 + alpine 基础镜像，预计 < 50MB
4. **向后兼容**：命令行参数保持与旧 shell 脚本一致

## 待解决问题

1. nerdctl 是否需要额外配置才能访问 containerd？
2. registry 认证如何处理？（nerdctl login 或挂载 dockerconfigjson）
3. multi-platform 镜像支持？

## 下一步

- [ ] 实现完整 Go 代码
- [ ] 测试 nerdctl commit 功能
- [ ] 构建 Docker 镜像
- [ ] 集成到 commit job
