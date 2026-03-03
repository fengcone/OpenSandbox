# OpenSandbox 安全容器运行时 - 部署与验证手册

本文档详细介绍如何部署和验证 OpenSandbox 的安全容器运行时功能（Secure Runtime Support），包括 gVisor、Kata Containers 等安全运行时的完整配置。

---

## 目录

1. [前置准备](#前置准备)
2. [Kind 集群部署](#kind-集群部署)
3. [Kubernetes Controller 部署](#kubernetes-controller-部署)
4. [gVisor 安全运行时配置](#gvisor-安全运行时配置)
5. [Kata Containers 安全运行时配置](#kata-containers-安全运行时配置)
6. [OpenSandbox Server 启动](#opensandbox-server-启动)
7. [完整验证流程](#完整验证流程)
8. [故障排查](#故障排查)

---

## 前置准备

### 系统要求

- macOS 或 Linux 系统
- Go 1.24.0+
- Docker 17.03+
- Python 3.10+
- kubectl 1.11.3+

### 安装必要工具

#### 1. 安装 Homebrew（macOS）

```bash
/bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)"
```

#### 2. 安装 Kind

```bash
brew install kind
```

验证安装：
```bash
kind version
```

#### 3. 安装 kubectl

```bash
brew install kubectl
```

验证安装：
```bash
kubectl version --client
```

#### 4. 安装 Docker Desktop for Mac

```bash
brew install --cask docker
```

启动 Docker Desktop 应用程序。

#### 5. 安装 Python 依赖

```bash
# 如果使用 pyenv
pyenv install 3.10.17
pyenv global 3.10.17

# 安装 uv（推荐的 Python 包管理器）
pip install uv
```

---

## Kind 集群部署

### 1. 创建 Kind 集群配置文件

创建文件 `kind-config.yaml`：

```yaml
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
- role: control-plane
  image: kindest/node:v1.29.2
  extraPortMappings:
  - containerPort: 8080
    hostPort: 8080
  - containerPort: 30080
    hostPort: 30080
```

### 2. 创建 Kind 集群

```bash
kind create cluster --config kind-config.yaml --name opensandbox
```

验证集群：
```bash
kubectl cluster-info --context kind-opensandbox
kubectl get nodes
```

预期输出：
```
NAME                           STATUS   ROLES           AGE   VERSION
opensandbox-control-plane   Ready    control-plane   10s   v1.29.2
```

### 3. 删除集群（如需重新开始）

```bash
kind delete cluster --name opensandbox
```

---

## Kubernetes Controller 部署

OpenSandbox Kubernetes Controller 是管理沙箱生命周期的 Operator。

### 1. 克隆项目并进入目录

```bash
cd /Users/fengjianhui/WorkSpaceGithub/OpenSandbox
```

### 2. 构建 Controller 镜像

```bash
cd kubernetes

# 构建 controller 镜像
make docker-build IMG=opensandbox/controller:v1.0.0

# 构建 task-executor 镜像
make docker-build-task-executor TASK_EXECUTOR_IMG=opensandbox/task-executor:v1.0.0
```

### 3. 加载镜像到 Kind 集群

```bash
kind load docker-image opensandbox/controller:v1.0.0 --name opensandbox
kind load docker-image opensandbox/task-executor:v1.0.0 --name opensandbox
```

验证镜像已加载：
```bash
docker exec opensandbox-control-plane crictl images | grep opensandbox
```

### 4. 安装 CRD

```bash
make install
```

预期输出：
```
customresourcedefinition.apiextensions.k8s.io/batchsandboxes.sandbox.opensandbox.io created
customresourcedefinition.apiextensions.k8s.io/pools.sandbox.opensandbox.io created
...
```

### 5. 部署 Controller

```bash
make deploy IMG=opensandbox/controller:v1.0.0 TASK_EXECUTOR_IMG=opensandbox/task-executor:v1.0.0
```

### 6. 验证 Controller 运行状态

```bash
kubectl get pods -n opensandbox-controller-system
```

预期输出：
```
NAME                                                   READY   STATUS    RESTARTS   AGE
opensandbox-controller-manager-xxx                      2/2     Running   0          30s
```

查看日志：
```bash
kubectl logs -n opensandbox-controller-system deployment/opensandbox-controller-manager -f
```

---

## gVisor 安全运行时配置

gVisor 是一个用户空间内核，提供 syscall 拦截和隔离。

### 1. 在 Kind 节点中安装 gVisor (runsc)

#### 1.1 进入 Kind 节点

```bash
docker exec -it opensandbox-control-plane bash
```

#### 1.2 下载并安装 runsc

```bash
# 在节点内执行
curl -fsSL https://gvisor.dev/archive.key | gpg --dearmor -o /usr/share/keyrings/gvisor-archive-keyring.gpg

echo "deb [signed-by=/usr/share/keyrings/gvisor-archive-keyring.gpg] https://storage.googleapis.com/gvisor/releases release main" > /etc/apt/sources.list.d/gvisor.list

apt-get update

apt-get install -y runsc
```

验证安装：
```bash
runsc --version
```

#### 1.3 配置 containerd

Kind 使用 containerd 作为容器运行时。需要配置 containerd 以支持 gVisor。

```bash
# 获取 containerd 配置
cat /etc/containerd/config.toml
```

添加 gVisor 运行时配置：

```bash
cat >> /etc/containerd/config.toml << 'EOF'

[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runsc]
  runtime_type = "io.containerd.runsc.v1"
  pod_annotations = ["runsc.google.com/oci-platform"]
  container_annotations = ["runsc.google.com/oci-platform"]
EOF
```

重启 containerd：
```bash
systemctl restart containerd
```

验证配置：
```bash
containerd config dump
```

退出节点：
```bash
exit
```

### 2. 创建 gVisor RuntimeClass

创建文件 `gvisor-runtimeclass.yaml`：

```yaml
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: gvisor
handler: runsc
```

应用配置：
```bash
kubectl apply -f gvisor-runtimeclass.yaml
```

验证：
```bash
kubectl get runtimeclass
```

预期输出：
```
NAME     HANDLER   AGE
gvisor   runsc     10s
```

### 3. 测试 gVisor 运行时

创建测试 Pod：

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: test-gvisor
spec:
  runtimeClassName: gvisor
  containers:
  - name: test
    image: nginx:alpine
    command: ["sh", "-c", "echo 'Running in gVisor!' && sleep 3600"]
```

```bash
kubectl apply -f test-gvisor.yaml
kubectl get pod test-gvisor
```

---

## Kata Containers 安全运行时配置

Kata Containers 提供基于虚拟机的隔离。

### 1. 在 Kind 节点中安装 Kata Containers

```bash
docker exec -it opensandbox-control-plane bash
```

安装 Kata Containers：

```bash
curl -fsSL https://raw.githubusercontent.com/kata-containers/kata-containers/main/docs/install/install-kata-containerd.sh | bash
```

配置 containerd 添加 Kata 运行时：

```bash
cat >> /etc/containerd/config.toml << 'EOF'

[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.kata-qemu]
  runtime_type = "io.containerd.kata-qemu.v2"
  pod_annotations = ["io.katacontainers.*"]
  container_annotations = ["io.katacontainers.*"]
EOF
```

重启 containerd：
```bash
systemctl restart containerd
```

退出节点：
```bash
exit
```

### 2. 创建 Kata RuntimeClass

创建文件 `kata-runtimeclass.yaml`：

```yaml
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: kata-qemu
handler: kata-qemu
```

应用配置：
```bash
kubectl apply -f kata-runtimeclass.yaml
```

---

## OpenSandbox Server 启动

Server 是沙箱生命周期的 API 服务。

### 1. 配置 Server

创建配置文件 `~/.sandbox.toml`：

```toml
[server]
host = "0.0.0.0"
port = 8080
log_level = "INFO"

[runtime]
# 使用 Kubernetes 运行时
type = "kubernetes"
execd_image = "opensandbox/execd:v1.0.6"

# Kubernetes 配置
[kubernetes]
# namespace = "default"
# workload_provider = "batchsandbox"

# 安全运行时配置
[secure_runtime]
# 支持: "", "gvisor", "kata", "firecracker"
type = "gvisor"
# Docker 模式使用 (当 runtime.type = "docker")
docker_runtime = "runsc"
# Kubernetes 模式使用 (当 runtime.type = "kubernetes")
k8s_runtime_class = "gvisor"

[ingress]
mode = "direct"

[docker]
network_mode = "bridge"

[storage]
allowed_host_paths = []

[egress]
# image = "opensandbox/egress:v1.0.1"
```

### 2. 安装 Python 依赖

```bash
cd /Users/fengjianhui/WorkSpaceGithub/OpenSandbox/server

# 使用 uv 安装依赖
uv sync

# 或者使用 pip
pip install -e .
```

### 3. 启动 Server

```bash
cd /Users/fengjianhui/WorkSpaceGithub/OpenSandbox/server

# 方法 1: 使用 uv 运行
uv run python -m src.main

# 方法 2: 直接运行 Python
.venv/bin/python -m src.main

# 方法 3: 使用 opensandbox-server 命令（如果已安装）
opensandbox-server
```

Server 启动后，你会看到类似输出：

```
INFO:     Started server process [xxxxx]
INFO:     Waiting for application startup.
INFO:     Secure runtime 'gvisor' validated successfully.
INFO:     Application startup complete.
INFO:     Uvicorn running on http://0.0.0.0:8080 (Press CTRL+C to quit)
```

### 4. 验证 Server 健康状态

```bash
curl http://localhost:8080/health
```

预期输出：
```json
{"status": "healthy"}
```

---

## 完整验证流程

### 验证 1: 使用 BatchSandboxProvider 创建沙箱

创建文件 `test-batchsandbox-gvisor.yaml`：

```yaml
apiVersion: sandbox.opensandbox.io/v1alpha1
kind: BatchSandbox
metadata:
  name: test-secure-sandbox
spec:
  replicas: 1
  expireTime: "2026-12-31T23:59:59Z"
  template:
    spec:
      runtimeClassName: gvisor  # 安全运行时类名
      containers:
      - name: sandbox
        image: nginx:alpine
        command: ["sh", "-c"]
        args:
        - |
          echo "Hello from gVisor secure sandbox!"
          echo "Checking secure runtime..."
          # gVisor 不会有 /proc/kcore
          if [ ! -f /proc/kcore ]; then
            echo "✓ /proc/kcore not accessible (gVisor isolation working)"
          fi
          sleep 3600
```

```bash
kubectl apply -f test-batchsandbox-gvisor.yaml
```

### 验证 2: 检查 BatchSandbox 状态

```bash
kubectl get batchsandbox test-secure-sandbox -o wide
```

预期输出：
```
NAME                   DESIRED   TOTAL   ALLOCATED   READY   EXPIRE               AGE
test-secure-sandbox    1         1       1           1       2026-12-31T23:59   1m
```

查看详细信息：
```bash
kubectl describe batchsandbox test-secure-sandbox
```

### 验证 3: 检查 Pod 运行时

```bash
kubectl get pods -l sandbox.opensandbox.io/sandbox-id=test-secure-sandbox
```

查看 Pod 的运行时类：
```bash
kubectl get pod -l sandbox.opensandbox.io/sandbox-id=test-secure-sandbox -o jsonpath='{.items[0].spec.runtimeClassName}'
```

预期输出：
```
gvisor
```

### 验证 4: 查看 Pod 日志

```bash
kubectl logs -l sandbox.opensandbox.io/sandbox-id=test-secure-sandbox
```

预期输出包含：
```
Hello from gVisor secure sandbox!
Checking secure runtime...
✓ /proc/kcore not accessible (gVisor isolation working)
```

### 验证 5: 通过 API 创建沙箱

安装 Python SDK：

```bash
pip install opensandbox
```

创建测试脚本 `test_secure_sandbox.py`：

```python
import asyncio
from datetime import timedelta
from opensandbox import Sandbox

async def main():
    print("Creating secure sandbox with gVisor...")

    sandbox = await Sandbox.create(
        image="nginx:alpine",
        entrypoint=["sh", "-c", "echo 'Hello from secure sandbox!' && sleep 60"],
        timeout=timedelta(minutes=5),
    )

    print(f"Sandbox created: {sandbox.id}")
    print(f"Status: {sandbox.status.state}")

    # 等待沙箱就绪
    await asyncio.sleep(5)

    # 获取沙箱信息
    sandbox_info = await sandbox.get()
    print(f"Final status: {sandbox_info.status.state}")

    # 清理
    await sandbox.kill()
    print("Sandbox terminated")

if __name__ == "__main__":
    asyncio.run(main())
```

运行测试：

```bash
python test_secure_sandbox.py
```

### 验证 6: 切换到 Kata 运行时

修改 `~/.sandbox.toml`：

```toml
[secure_runtime]
type = "kata"
docker_runtime = "kata-runtime"
k8s_runtime_class = "kata-qemu"
```

重启 Server：

```bash
# 停止当前 Server (Ctrl+C)
uv run python -m src.main
```

创建新的测试 Pod：

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: test-kata
spec:
  runtimeClassName: kata-qemu
  containers:
  - name: test
    image: nginx:alpine
    command: ["sh", "-c", "echo 'Running in Kata VM!' && sleep 60"]
```

```bash
kubectl apply -f test-kata.yaml
kubectl logs test-kata
```

---

## 故障排查

### 问题 1: Server 启动失败 - RuntimeClass 不存在

**错误信息**：
```
ValueError: RuntimeClass 'gvisor' does not exist.
```

**解决方案**：
```bash
# 检查 RuntimeClass 是否存在
kubectl get runtimeclass

# 如果不存在，重新创建
kubectl apply -f gvisor-runtimeclass.yaml
```

### 问题 2: Pod 启动失败 - RuntimeClass 创建失败

**错误信息**：
```
Failed to create sandbox: rpc error: code = NotFound desc = runtime handler "runsc" not found
```

**解决方案**：
```bash
# 进入 Kind 节点检查配置
docker exec -it opensandbox-control-plane bash

# 检查 containerd 配置
cat /etc/containerd/config.toml | grep -A 3 runsc

# 检查 runsc 是否安装
which runsc

# 重启 containerd
systemctl restart containerd
```

### 问题 3: 无法连接到 Kubernetes 集群

**错误信息**：
```
ValueError: Failed to load Kubernetes configuration
```

**解决方案**：
```bash
# 检查 kubeconfig
echo $KUBECONFIG
kubectl config current-context

# 如果使用 Kind，确保 context 正确
kubectl config use-context kind-opensandbox
```

### 问题 4: Controller Pod 无法启动

**检查步骤**：
```bash
# 查看 Pod 状态
kubectl get pods -n opensandbox-controller-system

# 查看日志
kubectl logs -n opensandbox-controller-system deployment/opensandbox-controller-manager

# 查看事件
kubectl describe pod -n opensandbox-controller-system <pod-name>
```

### 问题 5: execd 镜像拉取失败

**解决方案**：
```bash
# 预先拉取镜像
docker pull opensandbox/execd:v1.0.6

# 加载到 Kind 集群
kind load docker-image opensandbox/execd:v1.0.6 --name opensandbox
```

### 问题 6: 沙箱创建后立即失败

**检查步骤**：
```bash
# 查看 BatchSandbox 状态
kubectl get batchsandbox -o yaml

# 查看 Pod 状态
kubectl get pods -l sandbox.opensandbox.io/sandbox-id=<sandbox-id>

# 查看 Pod 日志
kubectl logs <pod-name>

# 查看 Pod 事件
kubectl describe pod <pod-name>
```

---

## 附录

### A. 完整的 gVisor 配置示例

`~/.sandbox.toml`:
```toml
[server]
host = "0.0.0.0"
port = 8080
log_level = "DEBUG"

[runtime]
type = "kubernetes"
execd_image = "opensandbox/execd:v1.0.6"

[kubernetes]
namespace = "default"
workload_provider = "batchsandbox"
# informer_enabled = true

[secure_runtime]
type = "gvisor"
docker_runtime = "runsc"
k8s_runtime_class = "gvisor"

[ingress]
mode = "direct"

[docker]
network_mode = "bridge"
drop_capabilities = ["AUDIT_WRITE", "MKNOD", "NET_ADMIN", "NET_RAW", "SYS_ADMIN", "SYS_MODULE", "SYS_PTRACE", "SYS_TIME", "SYS_TTY_CONFIG"]
no_new_privileges = true
pids_limit = 512

[storage]
allowed_host_paths = []
```

### B. RuntimeClass 配置清单

| Runtime Type | RuntimeClass Name | Handler | Use Case |
|--------------|------------------|---------|----------|
| gVisor | gvisor | runsc | 通用工作负载，低开销 |
| Kata QEMU | kata-qemu | kata-qemu | 最高隔离，完整特性 |
| Kata Firecracker | kata-fc | kata-fc | 最小内存占用，快速启动 |

### C. 清理环境

```bash
# 删除测试资源
kubectl delete batchsandbox --all
kubectl delete pool --all

# 删除 RuntimeClass
kubectl delete runtimeclass gvisor kata-qemu kata-fc

# 卸载 Controller
make undeploy

# 删除 Kind 集群
kind delete cluster --name opensandbox

# 删除本地配置
rm ~/.sandbox.toml
```

### D. 有用的调试命令

```bash
# 查看 Server 日志
tail -f /var/log/opensandbox.log

# 查看所有 Pod 的运行时类
kubectl get pods -A -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.runtimeClassName}{"\n"}{end}'

# 实时监控 BatchSandbox 状态
kubectl get batchsandbox -w

# 进入 Pod 检查运行时
kubectl exec -it <pod-name> -- sh

# 检查 gVisor 是否在运行（在 Pod 内）
# gVisor 会没有 /proc/kcore
ls /proc/kcore  # 应该失败或不存在
```

---

## 总结

本手册涵盖了：

1. ✅ Kind 集群的创建和配置
2. ✅ OpenSandbox Kubernetes Controller 的部署
3. ✅ gVisor 安全运行时的完整配置
4. ✅ Kata Containers 安全运行时的配置
5. ✅ OpenSandbox Server 的启动和配置
6. ✅ 通过多种方式验证安全运行时功能
7. ✅ 常见问题的排查方法

按照本手册操作后，你应该能够成功部署和验证 OpenSandbox 的安全容器运行时功能。
