# OpenSandbox Linux + Minikube 完整部署手册

本文档详细介绍了在 Linux 环境下使用 Minikube 部署 OpenSandbox 所有组件的完整流程，包括安全容器运行时（Secure Runtime）的配置与验证。

---

## 目录

1. [环境准备](#环境准备)
2. [Minikube 集群部署](#minikube-集群部署)
3. [构建组件镜像](#构建组件镜像)
4. [部署 Kubernetes Controller](#部署-kubernetes-controller)
5. [部署 Ingress Gateway](#部署-ingress-gateway)
6. [配置 gVisor 安全运行时](#配置-gvisor-安全运行时)
7. [启动 OpenSandbox Server](#启动-opensandbox-server)
8. [完整验证流程](#完整验证流程)
9. [故障排查](#故障排查)
10. [附录](#附录)

---

## 环境准备

### 系统要求

- **操作系统**: Linux (Ubuntu 20.04+ / Debian 11+ / CentOS 8+ / Alibaba Cloud Linux 8+)
- **CPU**: 4 核心以上
- **内存**: 8GB 以上 (推荐 16GB)
- **磁盘**: 50GB 以上可用空间

### 安装必要工具

#### 1. 安装 Docker

```bash
# Ubuntu/Debian
curl -fsSL https://get.docker.com -o get-docker.sh
sudo sh get-docker.sh

# 启动 Docker
sudo systemctl start docker
sudo systemctl enable docker

# 将当前用户添加到 docker 组
sudo usermod -aG docker $USER
newgrp docker

# 验证
docker --version
docker info
```

#### 2. 安装 Minikube

```bash
# 下载最新版本
curl -LO https://storage.googleapis.com/minikube/releases/latest/minikube-linux-amd64
sudo install minikube-linux-amd64 /usr/local/bin/minikube

# 验证
minikube version
```

#### 3. 安装 kubectl

```bash
# 下载最新版本
curl -LO "https://dl.k8s.io/release/$(curl -L -s https://dl.k8s.io/release/stable.txt)/bin/linux/amd64/kubectl"
sudo chmod +x kubectl
sudo mv kubectl /usr/local/bin/

# 验证
kubectl version --client
```

#### 4. 安装 Go (编译组件用)

```bash
# 下载并安装 Go 1.24
wget https://go.dev/dl/go1.24.0.linux-amd64.tar.gz
sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf go1.24.0.linux-amd64.tar.gz

# 配置环境变量
cat >> ~/.bashrc << 'EOF'
export PATH=$PATH:/usr/local/go/bin
export GOPATH=$HOME/go
EOF

source ~/.bashrc

# 验证
go version
```

#### 5. 安装 Python 3.10+

##### Alibaba Cloud Linux 8 (Al8)

```bash
# AL8 通常自带 Python 3.9+，检查版本
python3 --version

# 启用 EPEL 和 Powertools 仓库
sudo yum install -y epel-release
sudo yum config-manager --set-enabled powertools

# 安装 Python 及开发工具
sudo yum install -y python3 python3-devel python3-pip python3-venv python3-libvirt

# 安装 uv (推荐)
pip3 install uv --user
echo 'export PATH=$HOME/.local/bin:$PATH' >> ~/.bashrc
source ~/.bashrc

# 验证
python3 --version
uv --version
```

##### CentOS/RHEL 8/9, Rocky Linux, AlmaLinux

```bash
# CentOS/RHEL 8/9 通常自带 Python 3.9 或 3.11
# 如果系统自带 Python 版本 >= 3.10，可直接使用：
python3 --version

# 安装 Python 及相关工具
sudo dnf install -y python3 python3-devel python3-pip python3-venv

# 或者使用 yum
sudo yum install -y python3 python3-devel python3-pip python3-venv

# 安装 uv (推荐)
pip3 install uv --user

# 添加 uv 到 PATH
echo 'export PATH=$HOME/.local/bin:$PATH' >> ~/.bashrc
source ~/.bashrc

# 验证
python3 --version
uv --version
```

##### CentOS 7 (需要安装 Python 3.10)

```bash
# CentOS 7 默认 Python 版本较老，需要从 SCL 或源码安装

# 方法 1: 使用 Software Collections (SCL)
sudo yum install -y centos-release-scl
sudo yum install -y rh-python38 rh-python38-python-devel rh-python38-python-pip
scl enable rh-python38 bash

# 方法 2: 从源码编译 Python 3.10
sudo yum install -y gcc make openssl-devel bzip2-devel libffi-devel zlib-devel
cd /usr/src
wget https://www.python.org/ftp/python/3.10.17/Python-3.10.17.tgz
tar xzf Python-3.10.17.tgz
cd Python-3.10.17
./configure --enable-optimizations
make altinstall

# 验证
python3.10 --version

# 安装 uv
python3.10 -m pip install uv --user
export PATH=$HOME/.local/bin:$PATH
uv --version
```

##### Ubuntu/Debian

```bash
sudo apt-get update
sudo apt-get install -y python3.10 python3.10-venv python3-pip

# 安装 uv (推荐)
pip install uv

# 验证
python3.10 --version
uv --version
```

#### 6. 安装其他工具

```bash
# 安装 make 和构建工具
sudo apt-get install -y build-essential make git

# 安装 kind (可选，用于替代 minikube)
# go install sigs.k8s.io/kind@latest
```

---

## Minikube 集群部署

### 1. 启动 Minikube 集群

```bash
# 启动 Minikube (推荐配置)
minikube start \
  --driver=docker \
  --cpus=4 \
  --memory=8192 \
  --disk-size=50g \
  --kubernetes-version=v1.29.2 \
  --container-runtime=containerd \
  --cni=calico

# 等待集群就绪
minikube status
```

预期输出：
```
minikube
type: Control Plane
host: Running
kubelet: Running
apiserver: Running
kubeconfig: Configured
```

### 2. 验证集群连接

```bash
# 配置 kubectl
eval "$(minikube docker-env)"

# 验证节点
kubectl get nodes

# 验证集群信息
kubectl cluster-info
```

### 3. 开启 ingress 插件 (可选)

```bash
minikube addons enable ingress
```

### 4. 常用 Minikube 命令

```bash
# 查看日志
minikube logs

# 进入节点 shell
minikube ssh

# 停止集群
minikube stop

# 删除集群
minikube delete

# 重置集群
minikube delete
minikube start
```

---

## 构建组件镜像

OpenSandbox 包含多个组件，需要在本地构建镜像。

### 项目结构

```
OpenSandbox/
├── kubernetes/        # Kubernetes Controller 和 CRD
├── server/            # Python FastAPI Server
├── components/
│   ├── execd/        # 执行守护进程 (Go)
│   ├── egress/       # 网络策略 Sidecar (Go)
│   └── ingress/      # Ingress 网关 (Go)
```

### 1. 克隆项目

```bash
cd ~/
git clone https://github.com/alibaba/OpenSandbox.git
cd OpenSandbox
```

### 2. 构建 execd 镜像

```bash
cd components/execd

# 构建 Docker 镜像
docker build -t opensandbox/execd:v1.0.6 .

# 验证镜像
docker images | grep execd
```

### 3. 构建 egress 镜像

```bash
cd ../../egress

# 构建 Docker 镜像
docker build -t opensandbox/egress:v1.0.1 .

# 验证镜像
docker images | grep egress
```

### 4. 构建 ingress 镜像

```bash
cd ../../ingress

# 构建 Docker 镜像
docker build -t opensandbox/ingress:v1.0.0 .

# 验证镜像
docker images | grep ingress
```

### 5. 构建 Kubernetes Controller 镜像

```bash
cd ../../kubernetes

# 构建 Controller 镜像
make docker-build IMG=opensandbox/controller:v1.0.0

# 构建 Task Executor 镜像
make docker-build-task-executor TASK_EXECUTOR_IMG=opensandbox/task-executor:v1.0.0

# 验证镜像
docker images | grep opensandbox
```

### 6. 加载镜像到 Minikube

```bash
# 将所有镜像加载到 Minikube
minikube image load opensandbox/execd:v1.0.6
minikube image load opensandbox/egress:v1.0.1
minikube image load opensandbox/ingress:v1.0.0
minikube image load opensandbox/controller:v1.0.0
minikube image load opensandbox/task-executor:v1.0.0

# 验证镜像已加载
minikube image list | grep opensandbox
```

预期输出：
```
| opensandbox/controller      | v1.0.0   |
| opensandbox/egress          | v1.0.1   |
| opensandbox/execd           | v1.0.6   |
| opensandbox/ingress         | v1.0.0   |
| opensandbox/task-executor   | v1.0.0   |
```

---

## 部署 Kubernetes Controller

Controller 是 OpenSandbox 的核心组件，管理沙箱生命周期。

### 1. 安装 CRD

```bash
cd ~/OpenSandbox/kubernetes

# 安装自定义资源定义
make install
```

预期输出：
```
customresourcedefinition.apiextensions.k8s.io/batchsandboxes.sandbox.opensandbox.io created
customresourcedefinition.apiextensions.k8s.io/pools.sandbox.opensandbox.io created
serviceaccount/opensandbox-controller-manager created
...
```

### 2. 验证 CRD 安装

```bash
kubectl get crd | grep sandbox
```

预期输出：
```
batchsandboxes.sandbox.opensandbox.io          2025-03-02T00:00:00Z
pools.sandbox.opensandbox.io                   2025-03-02T00:00:00Z
```

### 3. 部署 Controller

```bash
# 部署 Controller Manager
make deploy IMG=opensandbox/controller:v1.0.0 TASK_EXECUTOR_IMG=opensandbox/task-executor:v1.0.0
```

### 4. 验证 Controller 运行状态

```bash
# 查看 Controller Pod
kubectl get pods -n opensandbox-controller-system

# 查看 Controller 日志
kubectl logs -n opensandbox-controller-system deployment/opensandbox-controller-manager -f
```

预期 Pod 状态：
```
NAME                                                   READY   STATUS    RESTARTS   AGE
opensandbox-controller-manager-xxx                      2/2     Running   0          1m
```

### 5. 部署命名空间

```bash
# 创建 OpenSandbox 工作命名空间
kubectl create namespace opensandbox

# 设置为默认命名空间
kubectl config set-context --current --namespace=opensandbox
```

---

## 部署 Ingress Gateway

Ingress Gateway 是 OpenSandbox 的流量入口组件，负责将请求路由到具体的沙箱实例。

### 1. 创建 Ingress 部署配置

创建文件 `opensandbox-ingress.yaml`:

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: opensandbox-ingress
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ingress-gateway
  namespace: opensandbox-ingress
spec:
  replicas: 1
  selector:
    matchLabels:
      app: ingress-gateway
  template:
    metadata:
      labels:
        app: ingress-gateway
    spec:
      containers:
      - name: ingress
        image: opensandbox/ingress:v1.0.0
        args:
        - --namespace=opensandbox
        - --provider-type=batchsandbox
        - --mode=header
        - --port=8080
        - --log-level=info
        ports:
        - containerPort: 8080
          name: http
        env:
        - name: KUBECONFIG
          value: /in-cluster-config
        resources:
          requests:
            cpu: 100m
            memory: 128Mi
          limits:
            cpu: 500m
            memory: 512Mi
---
apiVersion: v1
kind: Service
metadata:
  name: ingress-gateway
  namespace: opensandbox-ingress
spec:
  type: NodePort
  selector:
    app: ingress-gateway
  ports:
  - port: 8080
    targetPort: 8080
    nodePort: 30080
    name: http
---
# 可选：创建 Ingress 用于外部访问
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: ingress-gateway
  namespace: opensandbox-ingress
  annotations:
    nginx.ingress.kubernetes.io/rewrite-target: /
spec:
  ingressClassName: nginx
  rules:
  - host: opensandbox.local
    http:
      paths:
      - path: /
        pathType: Prefix
        backend:
          service:
            name: ingress-gateway
            port:
              number: 8080
```

### 2. 部署 Ingress Gateway

```bash
# 应用配置
kubectl apply -f opensandbox-ingress.yaml

# 等待 Pod 就绪
kubectl wait --for=condition=ready pod -l app=ingress-gateway -n opensandbox-ingress --timeout=120s

# 验证部署
kubectl get pods -n opensandbox-ingress
kubectl get svc -n opensandbox-ingress
```

### 3. 验证 Ingress 运行

```bash
# 查看日志
kubectl logs -l app=ingress-gateway -n opensandbox-ingress -f

# 健康检查
kubectl exec -n opensandbox-ingress deployment/ingress-gateway -- curl -s http://localhost:8080/status.ok
```

预期输出：
```
status.ok
```

### 4. 获取 Ingress 访问地址

```bash
# Minikube NodePort 方式
minikube service -n opensandbox-ingress ingress-gateway --url

# 或使用端口转发
kubectl port-forward -n opensandbox-ingress svc/ingress-gateway 8080:8080
```

---

## 配置 gVisor 安全运行时

gVisor 提供用户空间内核隔离，是推荐的安全运行时方案。

### 1. 进入 Minikube 节点

```bash
minikube ssh
```

### 2. 安装 runsc (gVisor)

```bash
# 在 Minikube 节点内执行

# 安装依赖
sudo apt-get update
sudo apt-get install -y curl

# 下载并安装 gVisor
curl -fsSL https://gvisor.dev/archive.key | sudo gpg --dearmor -o /usr/share/keyrings/gvisor-archive-keyring.gpg
echo "deb [signed-by=/usr/share/keyrings/gvisor-archive-keyring.gpg] https://storage.googleapis.com/gvisor/releases release main" | sudo tee /etc/apt/sources.list.d/gvisor.list

sudo apt-get update
sudo apt-get install -y runsc

# 验证安装
runsc --version
```

### 3. 配置 containerd

Minikube 使用 containerd 作为容器运行时，需要配置 gVisor。

```bash
# 备份配置
sudo cp /etc/containerd/config.toml /etc/containerd/config.toml.bak

# 查看 containerd 配置
sudo cat /etc/containerd/config.toml
```

添加 gVisor 运行时配置：

```bash
cat | sudo tee -a /etc/containerd/config.toml << 'EOF'

# gVisor 运行时配置
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runsc]
  runtime_type = "io.containerd.runsc.v1"
  pod_annotations = ["runsc.google.com/oci-platform"]
  container_annotations = ["runsc.google.com/oci-platform"]

# Kata Containers 运行时配置 (可选)
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.kata-qemu]
  runtime_type = "io.containerd.kata-qemu.v2"
  pod_annotations = ["io.katacontainers.*"]
  container_annotations = ["io.katacontainers.*"]
EOF
```

### 4. 重启 containerd

```bash
# 重启 containerd
sudo systemctl restart containerd

# 验证 containerd 运行状态
sudo systemctl status containerd

# 退出节点
exit
```

### 5. 创建 gVisor RuntimeClass

创建文件 `gvisor-runtimeclass.yaml`:

```yaml
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: gvisor
handler: runsc
```

```bash
# 应用 RuntimeClass
kubectl apply -f gvisor-runtimeclass.yaml

# 验证
kubectl get runtimeclass
```

---

## 启动 OpenSandbox Server

Server 是沙箱生命周期的 API 服务，可以通过本地或容器方式运行。

### 方式一：本地运行 (推荐用于开发)

#### 1. 安装 Python 依赖

```bash
cd ~/OpenSandbox/server

# 使用 uv 安装依赖
uv sync

# 或使用 pip
pip install -e .
```

#### 2. 创建配置文件

创建 `~/.sandbox.toml`:

```toml
[server]
host = "0.0.0.0"
port = 8080
log_level = "INFO"
api_key = ""

[runtime]
type = "kubernetes"
execd_image = "opensandbox/execd:v1.0.6"

[kubernetes]
kubeconfig_path = "~/.kube/config"
namespace = "opensandbox"
workload_provider = "batchsandbox"
informer_enabled = true
informer_resync_seconds = 300
informer_watch_timeout_seconds = 60

# 安全运行时配置
[secure_runtime]
# 支持: "", "gvisor", "kata", "firecracker"
type = "gvisor"
docker_runtime = "runsc"
k8s_runtime_class = "gvisor"

[ingress]
mode = "gateway"
gateway.address = "192.168.49.1:30080"
gateway.route.mode = "header"

[docker]
network_mode = "bridge"
drop_capabilities = ["AUDIT_WRITE", "MKNOD", "NET_ADMIN", "NET_RAW", "SYS_ADMIN", "SYS_MODULE", "SYS_PTRACE", "SYS_TIME", "SYS_TTY_CONFIG"]
no_new_privileges = true
pids_limit = 512

[storage]
allowed_host_paths = []

[egress]
# image = "opensandbox/egress:v1.0.1"
```

**重要配置说明**:

| 配置项 | 说明 |
|--------|------|
| `runtime.type` | 使用 "kubernetes" 时需要在集群中运行 |
| `kubernetes.namespace` | 与 Controller 创建沙箱的命名空间一致 |
| `secure_runtime.type` | 安全运行时类型：gvisor/kata/firecracker |
| `secure_runtime.k8s_runtime_class` | 对应 K8s RuntimeClass 名称 |
| `ingress.mode` | 使用 "gateway" 时需要部署 Ingress Gateway |
| `ingress.gateway.address` | Minikube NodePort 地址 |

#### 3. 启动 Server

```bash
cd ~/OpenSandbox/server

# 方式 1: 使用 uv
uv run python -m src.main

# 方式 2: 使用 Python
python3.10 -m src.main

# 方式 3: 后台运行
nohup python3.10 -m src.main > /var/log/opensandbox-server.log 2>&1 &
```

预期输出：
```
INFO:     Started server process [xxxxx]
INFO:     Waiting for application startup.
INFO:     Secure runtime 'gvisor' validated successfully.
INFO:     Application startup complete.
INFO:     Uvicorn running on http://0.0.0.0:8080 (Press CTRL+C to quit)
```

#### 4. 验证 Server 运行

```bash
# 健康检查
curl http://localhost:8080/health

# 查看 API 文档
# 在浏览器打开 http://localhost:8080/docs
```

### 方式二：容器运行 (推荐用于生产)

#### 1. 创建 Dockerfile

```dockerfile
FROM python:3.10-slim

WORKDIR /app

# 安装依赖
COPY pyproject.toml uv.lock ./
RUN pip install uv
RUN uv sync --no-dev

# 复制代码
COPY . .

# 暴露端口
EXPOSE 8080

# 启动服务
CMD ["uv", "run", "python", "-m", "src.main"]
```

#### 2. 构建和运行

```bash
# 构建镜像
docker build -t opensandbox/server:v1.0.0 .

# 运行容器
docker run -d \
  --name opensandbox-server \
  --network host \
  -v ~/.kube/config:/root/.kube/config:ro \
  -v ~/.sandbox.toml:/app/.sandbox.toml:ro \
  opensandbox/server:v1.0.0
```

---

## 完整验证流程

### 验证 1: 测试 BatchSandbox 创建

创建测试文件 `test-batchsandbox.yaml`:

```yaml
apiVersion: sandbox.opensandbox.io/v1alpha1
kind: BatchSandbox
metadata:
  name: test-gvisor-sandbox
  namespace: opensandbox
spec:
  replicas: 1
  expireTime: "2026-12-31T23:59:59Z"
  template:
    spec:
      runtimeClassName: gvisor  # 使用 gVisor 安全运行时
      containers:
      - name: sandbox
        image: nginx:alpine
        command: ["sh", "-c"]
        args:
        - |
          echo "=== OpenSandbox Secure Runtime Test ==="
          echo "Running in gVisor secure container..."
          echo ""
          echo "Test 1: Checking gVisor isolation..."
          if [ ! -f /proc/kcore ]; then
            echo "✓ PASS: /proc/kcore not accessible (gVisor isolation working)"
          else
            echo "✗ FAIL: /proc/kcore accessible (not isolated)"
          fi
          echo ""
          echo "Test 2: Checking seccomp..."
          if grep -q "seccomp" /proc/self/status 2>/dev/null; then
            echo "✓ PASS: seccomp enabled"
          fi
          echo ""
          echo "Starting web server on port 8080..."
          echo "Access this sandbox via Ingress Gateway"
          sleep 3600
```

```bash
# 创建 BatchSandbox
kubectl apply -f test-batchsandbox.yaml

# 查看状态
kubectl get batchsandbox test-gvisor-sandbox -n opensandbox -o wide

# 等待就绪 (READY = 1)
kubectl wait --for=jsonpath='{.status.ready}'=1 batchsandbox/test-gvisor-sandbox -n opensandbox --timeout=120s
```

预期输出：
```
NAME                   DESIRED   TOTAL   ALLOCATED   READY   EXPIRE               AGE
test-gvisor-sandbox    1         1       1           1       2026-12-31T23:59   1m
```

### 验证 2: 查看 Pod 详情

```bash
# 获取 Pod 名称
POD_NAME=$(kubectl get pods -n opensandbox -l sandbox.opensandbox.io/sandbox-id=test-gvisor-sandbox -o jsonpath='{.items[0].metadata.name}')

# 查看 Pod 状态
kubectl get pod $POD_NAME -n opensandbox -o wide

# 查看 Pod 运行时类
kubectl get pod $POD_NAME -n opensandbox -o jsonpath='{.spec.runtimeClassName}'

# 查看 Pod 事件
kubectl describe pod $POD_NAME -n opensandbox
```

预期输出：
```
gvisor
```

### 验证 3: 查看 Pod 日志

```bash
kubectl logs $POD_NAME -n opensandbox
```

预期日志包含：
```
=== OpenSandbox Secure Runtime Test ===
Running in gVisor secure container...
✓ PASS: /proc/kcore not accessible (gVisor isolation working)
✓ PASS: seccomp enabled
```

### 验证 4: 通过 Server API 创建沙箱

```bash
# 使用 API 创建沙箱
curl -X POST "http://localhost:8080/v1/sandboxes" \
  -H "Content-Type: application/json" \
  -d '{
    "image": {"uri": "nginx:alpine"},
    "entrypoint": ["sh", "-c"],
    "env": {"TEST": "gvisor"},
    "entrypoint": ["sh", "-c", "echo \"API Test\" && sleep 60"],
    "timeout": 3600,
    "resourceLimits": {
      "cpu": "500m",
      "memory": "512Mi"
    }
  }'
```

### 验证 5: 通过 Ingress Gateway 访问

```bash
# 获取沙箱 ID
SANDBOX_ID="test-gvisor-sandbox"

# 通过 Ingress Gateway 访问 (header 模式)
curl -H "OpenSandbox-Ingress-To: ${SANDBOX_ID}-8080" \
  http://192.168.49.1:30080/

# 或使用 URI 模式
# curl http://192.168.49.1:30080/${SANDBOX_ID}/8080/
```

### 验证 6: 使用 Python SDK

```bash
# 安装 SDK
pip install opensandbox

# 创建测试脚本
cat > test_api.py << 'EOF'
import asyncio
from opensandbox import Sandbox
from datetime import timedelta

async def main():
    print("Creating secure sandbox with gVisor...")

    sandbox = await Sandbox.create(
        image="nginx:alpine",
        entrypoint=["sh", "-c", "echo 'Hello from gVisor!' && sleep 60"],
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

### 验证 7: 检查安全运行时注入

```bash
# 检查 Pod YAML 中是否包含 runtimeClassName
kubectl get pod -n opensandbox -l sandbox.opensandbox.io/sandbox-id=test-gvisor-sandbox -o yaml | grep runtimeClassName

# 进入 Pod 验证隔离特性
kubectl exec -n opensandbox $POD_NAME -- sh -c "ls /proc/kcore 2>&1"
```

预期输出 (gVisor 隔离特性)：
```
ls: /proc/kcore: No such file or directory
```

### 验证 8: 运行时验证清单

```bash
# 创建验证脚本
cat > verify_secure_runtime.sh << 'EOF'
#!/bin/bash

echo "=== OpenSandbox Secure Runtime Verification ==="
echo ""

# 1. 检查 Minikube 集群
echo "1. Checking Minikube cluster..."
minikube status
echo ""

# 2. 检查 gVisor RuntimeClass
echo "2. Checking gVisor RuntimeClass..."
kubectl get runtimeclass gvisor
echo ""

# 3. 检查 Controller
echo "3. Checking Controller..."
kubectl get pods -n opensandbox-controller-system
echo ""

# 4. 检查 Ingress Gateway
echo "4. Checking Ingress Gateway..."
kubectl get pods -n opensandbox-ingress
echo ""

# 5. 检查 Server
echo "5. Checking Server..."
curl -s http://localhost:8080/health
echo ""

# 6. 检查 BatchSandbox
echo "6. Checking BatchSandbox..."
kubectl get batchsandbox -n opensandbox
echo ""

# 7. 检查 Pod 运行时类
echo "7. Checking Pod runtimeClassName..."
kubectl get pods -n opensandbox -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.runtimeClassName}{"\n"}{end}'
echo ""

# 8. 测试 gVisor 隔离特性
echo "8. Testing gVisor isolation..."
POD_NAME=$(kubectl get pods -n opensandbox -l sandbox.opensandbox.io/sandbox-id=test-gvisor-sandbox -o jsonpath='{.items[0].metadata.name}')
kubectl exec -n opensandbox $POD_NAME -- sh -c "if [ ! -f /proc/kcore ]; then echo '✓ gVisor isolation working'; else echo '✗ Isolation failed'; fi"
echo ""

echo "=== Verification Complete ==="
EOF

chmod +x verify_secure_runtime.sh
./verify_secure_runtime.sh
```

---

## 故障排查

### 问题 1: Server 启动失败 - RuntimeClass 不存在

**错误信息**:
```
ValueError: RuntimeClass 'gvisor' does not exist.
```

**解决方案**:
```bash
# 检查 RuntimeClass
kubectl get runtimeclass

# 如果不存在，重新创建
kubectl apply -f gvisor-runtimeclass.yaml
```

### 问题 2: Pod 启动失败 - RuntimeHandler 未找到

**错误信息**:
```
Failed to create pod: rpc error: code = NotFound desc = runtime handler "runsc" not found
```

**解决方案**:
```bash
# 进入 Minikube 节点
minikube ssh

# 检查 runsc 是否安装
which runsc

# 检查 containerd 配置
sudo cat /etc/containerd/config.toml | grep runsc

# 重启 containerd
sudo systemctl restart containerd

# 退出节点
exit

# 重启测试 Pod
kubectl delete pod <pod-name> -n opensandbox
```

### 问题 3: Controller 无法连接到 Kubernetes

**解决方案**:
```bash
# 检查 kubeconfig
echo $KUBECONFIG
kubectl config current-context

# 如果使用 Minikube，确保 context 正确
kubectl config use-context minikube

# 重启 Controller
kubectl rollout restart deployment/opensandbox-controller-manager -n opensandbox-controller-system
```

### 问题 4: Ingress Gateway 无法路由到沙箱

**解决方案**:
```bash
# 检查 Ingress Gateway 日志
kubectl logs -l app=ingress-gateway -n opensandbox-ingress -f

# 检查 BatchSandbox endpoint annotation
kubectl get batchsandbox test-gvisor-sandbox -n opensandbox -o jsonpath='{.metadata.annotations.sandbox\.opensandbox\.io/endpoints}'

# 确认沙箱已就绪
kubectl get batchsandbox test-gvisor-sandbox -n opensandbox -o yaml
```

### 问题 5: 沙箱创建后立即失败

**解决方案**:
```bash
# 查看 BatchSandbox 状态
kubectl get batchsandbox -n opensandbox -o yaml

# 查看相关 Pod
kubectl get pods -n opensandbox -l sandbox.opensandbox.io/sandbox-id=<sandbox-id>

# 查看 Pod 日志
kubectl logs <pod-name> -n opensandbox

# 查看 Pod 事件
kubectl describe pod <pod-name> -n opensandbox
```

### 问题 6: Server 无法连接到 Kubernetes 集群

**解决方案**:
```bash
# 检查 kubeconfig 文件
ls -la ~/.kube/config

# 测试连接
kubectl get pods

# 检查 Server 配置中的 kubeconfig_path
cat ~/.sandbox.toml | grep kubeconfig_path
```

### 问题 7: gVisor 隔离验证失败

**症状**: Pod 可以访问 /proc/kcore

**原因**: 没有正确使用 gVisor 运行时

**解决方案**:
```bash
# 检查 Pod 的 runtimeClassName
kubectl get pod <pod-name> -n opensandbox -o jsonpath='{.spec.runtimeClassName}'

# 确保为 "gvisor"

# 检查 containerd 配置
minikube ssh
sudo cat /etc/containerd/config.toml | grep -A 3 runsc
```

---

## 附录

### A. 完整配置示例

#### Server 配置 (`~/.sandbox.toml`)

```toml
[server]
host = "0.0.0.0"
port = 8080
log_level = "INFO"
api_key = ""

[runtime]
type = "kubernetes"
execd_image = "opensandbox/execd:v1.0.6"

[kubernetes]
kubeconfig_path = "~/.kube/config"
namespace = "opensandbox"
workload_provider = "batchsandbox"
informer_enabled = true
informer_resync_seconds = 300
informer_watch_timeout_seconds = 60

[secure_runtime]
type = "gvisor"
docker_runtime = "runsc"
k8s_runtime_class = "gvisor"

[ingress]
mode = "gateway"
gateway.address = "192.168.49.1:30080"
gateway.route.mode = "header"

[docker]
network_mode = "bridge"
drop_capabilities = ["AUDIT_WRITE", "MKNOD", "NET_ADMIN", "NET_RAW", "SYS_ADMIN", "SYS_MODULE", "SYS_PTRACE", "SYS_TIME", "SYS_TTY_CONFIG"]
no_new_privileges = true
pids_limit = 512

[storage]
allowed_host_paths = []
```

### B. RuntimeClass 配置清单

| Runtime Type | RuntimeClass Name | Handler | 隔离级别 |
|--------------|------------------|---------|----------|
| gVisor | gvisor | runsc | 用户空间内核 |
| Kata QEMU | kata-qemu | kata-qemu | 虚拟机 |
| Kata Firecracker | kata-fc | kata-fc | 微虚拟机 |

### C. Minikube 端口转发说明

| 服务 | NodePort | 本地访问 |
|------|----------|----------|
| Ingress Gateway | 30080 | 192.168.49.1:30080 |
| Server (本地运行) | 8080 | localhost:8080 |

**Minikube IP 获取**:
```bash
minikube ip
# 通常输出 192.168.49.1
```

### D. 清理环境

```bash
# 删除测试资源
kubectl delete batchsandbox --all -n opensandbox
kubectl delete runtimeclass gvisor kata-qemu kata-fc

# 删除 Ingress Gateway
kubectl delete -f opensandbox-ingress.yaml

# 删除 Controller
cd ~/OpenSandbox/kubernetes
make undeploy

# 删除命名空间
kubectl delete namespace opensandbox
kubectl delete namespace opensandbox-ingress

# 停止 Minikube
minikube stop

# 删除 Minikube 集群
minikube delete

# 删除本地配置
rm ~/.sandbox.toml
```

### E. 部署架构图

```
┌─────────────────────────────────────────────────────────────────┐
│                         客户端 (SDK/CURL)                      │
└────────────────────────────┬────────────────────────────────┘
                             │
                             ▼
┌─────────────────────────────────────────────────────────────────┐
│                     OpenSandbox Server (FastAPI)               │
│  - 端口: 8080                                                 │
│  - 安全运行时: gVisor/Kata                                    │
└────────────────────────────┬────────────────────────────────┘
                             │
                             ▼
┌─────────────────────────────────────────────────────────────────┐
│                  Kubernetes Controller (Go)                    │
│  - 管理 BatchSandbox CRD                                      │
│  - 注入 runtimeClassName                                       │
└────────────────────────────┬────────────────────────────────┘
                             │
                             ▼
┌─────────────────────────────────────────────────────────────────┐
│                      Minikube (K8s 集群)                       │
│  ┌────────────────────────────────────────────────────────────┐│
│  │  BatchSandbox (CRD)                                       ││
│  │  ┌──────────────────────────────────────────────────────┐  ││
│  │  │  Pod (gVisor 运行时)                                 │  ││
│  │  │  ┌──────────────┐  ┌──────────────────────────────┐ │  ││
│  │  │  │ execd        │  │  沙箱应用容器                  │ │  ││
│  │  │  │ (init 容器)   │  │  (用户代码)                    │ │  ││
│  │  │  └──────────────┘  └──────────────────────────────────┘ │  ││
│  │  └──────────────────────────────────────────────────────┘  ││
│  └────────────────────────────────────────────────────────────┘│
│  ┌────────────────────────────────────────────────────────────┐│
│  │  Ingress Gateway (流量路由)                               ││
│  └────────────────────────────────────────────────────────────┘│
└─────────────────────────────────────────────────────────────────┘
```

### F. 常用命令速查

```bash
# === 集群管理 ===
minikube start                    # 启动集群
minikube stop                     # 停止集群
minikube delete                   # 删除集群
minikube ssh                      # 进入节点
minikube logs                     # 查看日志

# === 资源查看 ===
kubectl get nodes                 # 查看节点
kubectl get pods -A               # 查看所有 Pod
kubectl get svc -A                # 查看所有服务
kubectl get runtimeclass          # 查看运行时类
kubectl get batchsandbox -A       # 查看所有 BatchSandbox

# === 日志查看 ===
kubectl logs -f deployment/xxx      # 查看 Deployment 日志
kubectl logs -f pod/xxx             # 查看 Pod 日志

# === 资源清理 ===
kubectl delete pod xxx             # 删除 Pod
kubectl delete -f xxx.yaml          # 删除配置文件中的资源
kubectl delete all pods -n xxx     # 删除命名空间下所有 Pod

# === Server 操作 ===
curl http://localhost:8080/health  # 健康检查
curl http://localhost:8080/docs     # API 文档
```

---

## 总结

本手册涵盖了在 Linux + Minikube 环境下部署 OpenSandbox 的完整流程：

1. ✅ **环境准备** - Docker, Minikube, kubectl, Go, Python
2. ✅ **Minikube 集群** - 启动和配置本地 K8s 集群
3. ✅ **组件构建** - execd, egress, ingress, controller 镜像
4. ✅ **Controller 部署** - CRD 和 Controller Manager
5. ✅ **Ingress Gateway** - 流量路由组件
6. ✅ **gVisor 配置** - 安全运行时安装和 RuntimeClass
7. ✅ **Server 启动** - Python FastAPI Server
8. ✅ **完整验证** - 从 Pod 创建到 API 访问的端到端测试

按照本手册操作后，你应该能够：
- 在 Minikube 中运行 OpenSandbox 的所有组件
- 使用 gVisor 安全运行时创建隔离的沙箱环境
- 通过 Server API 和 Ingress Gateway 访问沙箱
- 理解各个组件之间的交互关系

祝你部署顺利！
