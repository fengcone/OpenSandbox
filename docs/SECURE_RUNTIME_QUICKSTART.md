# OpenSandbox 安全运行时 - 快速入门指南

这是一个简化版的快速入门指南，帮助你在 15 分钟内完成安全运行时的部署和验证。

---

## 第一步：安装 Kind（5 分钟）

### macOS

```bash
# 安装 Kind
brew install kind

# 验证安装
kind version
```

### 创建测试集群

```bash
# 创建集群
kind create cluster --name opensandbox

# 验证集群
kubectl get nodes
```

预期输出：
```
NAME                           STATUS   ROLES           AGE
opensandbox-control-plane   Ready    control-plane   10s
```

---

## 第二步：部署 gVisor 运行时（5 分钟）

### 1. 进入 Kind 节点

```bash
docker exec -it opensandbox-control-plane bash
```

### 2. 安装 runsc（gVisor）

```bash
# 在节点内执行以下命令
curl -fsSL https://gvisor.dev/archive.key | gpg --dearmor -o /usr/share/keyrings/gvisor-archive-keyring.gpg
echo "deb [signed-by=/usr/share/keyrings/gvisor-archive-keyring.gpg] https://storage.googleapis.com/gvisor/releases release main" > /etc/apt/sources.list.d/gvisor.list
apt-get update
apt-get install -y runsc

# 验证
runsc --version
```

### 3. 配置 containerd

```bash
# 添加 gVisor 运行时配置
cat >> /etc/containerd/config.toml << 'EOF'

[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runsc]
  runtime_type = "io.containerd.runsc.v1"
EOF

# 重启 containerd
systemctl restart containerd

# 退出节点
exit
```

### 4. 创建 RuntimeClass

```bash
cat > gvisor-runtimeclass.yaml << 'EOF'
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: gvisor
handler: runsc
EOF

kubectl apply -f gvisor-runtimeclass.yaml

# 验证
kubectl get runtimeclass
```

---

## 第三步：测试 gVisor 隔离（2 分钟）

### 创建测试 Pod

```bash
cat > test-gvisor.yaml << 'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: test-gvisor
spec:
  runtimeClassName: gvisor
  containers:
  - name: test
    image: nginx:alpine
    command: ["sh", "-c", "echo 'Hello gVisor!' && sleep 60"]
EOF

kubectl apply -f test-gvisor.yaml
```

### 验证 gVisor 隔离特性

```bash
# 等待 Pod 启动
kubectl wait --for=condition=ready pod/test-gvisor --timeout=60s

# 进入 Pod
kubectl exec -it test-gvisor -- sh

# 在 Pod 内测试 gVisor 隔离
# gVisor 没有 /proc/kcore（这是正常的隔离行为）
ls /proc/kcore
# 输出应该显示 "No such file or directory"

# 退出 Pod
exit
```

---

## 第四步：部署 OpenSandbox Controller（3 分钟）

```bash
cd /Users/fengjianhui/WorkSpaceGithub/OpenSandbox/kubernetes

# 构建镜像
make docker-build IMG=opensandbox/controller:v1.0.0
make docker-build-task-executor TASK_EXECUTOR_IMG=opensandbox/task-executor:v1.0.0

# 加载到 Kind
kind load docker-image opensandbox/controller:v1.0.0 --name opensandbox
kind load docker-image opensandbox/task-executor:v1.0.0 --name opensandbox

# 安装 CRD
make install

# 部署 Controller
make deploy IMG=opensandbox/controller:v1.0.0 TASK_EXECUTOR_IMG=opensandbox/task-executor:v1.0.0

# 等待 Controller 就绪
kubectl wait --for=condition=available -n opensandbox-controller-system deployment/opensandbox-controller-manager --timeout=120s

# 验证
kubectl get pods -n opensandbox-controller-system
```

---

## 第五步：启动 OpenSandbox Server（2 分钟）

### 1. 创建配置文件

```bash
cat > ~/.sandbox.toml << 'EOF'
[server]
host = "0.0.0.0"
port = 8080
log_level = "INFO"

[runtime]
type = "kubernetes"
execd_image = "opensandbox/execd:v1.0.6"

[kubernetes]
namespace = "default"
workload_provider = "batchsandbox"

[secure_runtime]
type = "gvisor"
docker_runtime = "runsc"
k8s_runtime_class = "gvisor"

[ingress]
mode = "direct"

[docker]
network_mode = "bridge"

[storage]
allowed_host_paths = []
EOF
```

### 2. 拉取 execd 镜像

```bash
docker pull opensandbox/execd:v1.0.6
kind load docker-image opensandbox/execd:v1.0.6 --name opensandbox
```

### 3. 启动 Server

```bash
cd /Users/fengjianhui/WorkSpaceGithub/OpenSandbox/server

# 安装依赖
uv sync

# 启动 Server
uv run python -m src.main
```

保持这个终端窗口运行，Server 会显示：
```
INFO:     Secure runtime 'gvisor' validated successfully.
INFO:     Uvicorn running on http://0.0.0.0:8080
```

---

## 第六步：验证安全运行时（2 分钟）

### 方法 1：通过 Kubernetes API 创建

```bash
cat > test-secure-sandbox.yaml << 'EOF'
apiVersion: sandbox.opensandbox.io/v1alpha1
kind: BatchSandbox
metadata:
  name: test-secure
spec:
  replicas: 1
  expireTime: "2026-12-31T23:59:59Z"
  template:
    spec:
      runtimeClassName: gvisor
      containers:
      - name: sandbox
        image: nginx:alpine
        command: ["sh", "-c"]
        args:
        - |
          echo "=== Secure Runtime Test ==="
          echo "Testing gVisor isolation..."
          if [ ! -f /proc/kcore ]; then
            echo "✓ PASS: /proc/kcore not accessible (gVisor working)"
          else
            echo "✗ FAIL: /proc/kcore accessible (not isolated)"
          fi
          sleep 300
EOF

kubectl apply -f test-secure-sandbox.yaml

# 等待就绪
sleep 10

# 查看状态
kubectl get batchsandbox test-secure

# 查看日志
kubectl logs -l sandbox.opensandbox.io/sandbox-id=test-secure
```

预期日志输出：
```
=== Secure Runtime Test ===
Testing gVisor isolation...
✓ PASS: /proc/kcore not accessible (gVisor working)
```

### 方法 2：通过 Python SDK

```bash
# 新开一个终端窗口

cd /Users/fengjianhui/WorkSpaceGithub/OpenSandbox

# 安装 SDK
pip install opensandbox

# 创建测试脚本
cat > test_api.py << 'EOF'
import asyncio
from opensandbox import Sandbox
from datetime import timedelta

async def main():
    print("Creating secure sandbox...")
    sandbox = await Sandbox.create(
        image="nginx:alpine",
        entrypoint=["sh", "-c", "echo 'Hello from gVisor!' && sleep 30"],
        timeout=timedelta(minutes=5),
    )

    print(f"Sandbox ID: {sandbox.id}")
    print(f"Status: {sandbox.status.state}")

    await asyncio.sleep(5)

    info = await sandbox.get()
    print(f"Final Status: {info.status.state}")

    await sandbox.kill()
    print("Sandbox terminated")

asyncio.run(main())
EOF

# 运行测试
python test_api.py
```

---

## 验证成功检查清单

运行以下命令确认一切正常：

```bash
# 1. Kind 集群运行中
kubectl get nodes
# ✓ 输出: 1 个 Ready 节点

# 2. gVisor RuntimeClass 存在
kubectl get runtimeclass gvisor
# ✓ 输出: NAME=gvisor, HANDLER=runsc

# 3. Controller 运行中
kubectl get pods -n opensandbox-controller-system
# ✓ 输出: 2/2 Running

# 4. Server 健康检查
curl http://localhost:8080/health
# ✓ 输出: {"status":"healthy"}

# 5. BatchSandbox 运行中
kubectl get batchsandbox
# ✓ 输出: READY=1

# 6. Pod 使用 gVisor 运行时
kubectl get pods -o jsonpath='{.items[0].spec.runtimeClassName}'
# ✓ 输出: gvisor
```

---

## 清理环境

```bash
# 删除测试资源
kubectl delete batchsandbox test-secure
kubectl delete pod test-gvisor
kubectl delete runtimeclass gvisor

# 删除 Controller
cd /Users/fengjianhui/WorkSpaceGithub/OpenSandbox/kubernetes
make undeploy

# 删除 Kind 集群
kind delete cluster --name opensandbox

# 删除配置
rm ~/.sandbox.toml
```

---

## 常见问题快速解决

### Server 启动失败

```bash
# 检查配置文件语法
cat ~/.sandbox.toml

# 检查 Kubeconfig
kubectl config current-context
kubectl config use-context kind-opensandbox
```

### Pod 启动失败

```bash
# 查看 Pod 状态
kubectl get pods -o wide

# 查看日志
kubectl logs <pod-name>

# 查看事件
kubectl describe pod <pod-name>
```

### RuntimeClass 不工作

```bash
# 进入 Kind 节点检查
docker exec -it opensandbox-control-plane bash

# 检查 runsc
which runsc

# 检查 containerd 配置
cat /etc/containerd/config.toml | grep runsc

# 重启 containerd
systemctl restart containerd
```

---

## 下一步

- 阅读完整部署手册：`docs/SECURE_RUNTIME_DEPLOYMENT_GUIDE.md`
- 了解 Kata Containers 配置
- 查看高级配置选项
- 了解如何创建资源池（Pool）

需要帮助？查看完整手册或提交 Issue。
