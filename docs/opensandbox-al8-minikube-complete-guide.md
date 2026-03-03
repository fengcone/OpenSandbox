# OpenSandbox 完整部署手册 (Alibaba Cloud Linux 8 + Minikube)

本文档适用于 Alibaba Cloud Linux 8 系统，详细介绍了部署 OpenSandbox 所有组件的完整流程，包括安全容器运行时的配置与验证。

---

## 目录

1. [系统环境](#系统环境)
2. [环境准备](#环境准备)
3. [Minikube 集群部署](#minikube-集群部署)
4. [构建组件镜像](#构建组件镜像)
5. [部署 Kubernetes Controller](#部署-kubernetes-controller)
6. [部署 Ingress Gateway](#部署-ingress-gateway)
7. [配置 gVisor 安全运行时](#配置-gvisor-安全运行时)
8. [启动 OpenSandbox Server](#启动-opensandbox-server)
9. [完整验证流程](#完整验证流程)
10. [故障排查](#故障排查)

---

## 系统环境

- **操作系统**: Alibaba Cloud Linux 8 (5.10.134-18.al8.x86_64)
- **CPU**: 4 核心以上
- **内存**: 8GB 以上 (推荐 16GB)
- **磁盘**: 50GB 以上可用空间

---

## 环境准备

### 1. 安装 Docker

```bash
# 安装 Docker
sudo yum install -y docker

# 启动 Docker
sudo systemctl start docker
sudo systemctl enable docker

# 将当前用户添加到 docker 组
sudo usermod -aG docker $USER
newgrp docker

# 验证
docker --version
```

### 2. 安装 Minikube

```bash
# 下载 Minikube
curl -LO https://storage.googleapis.com/minikube/releases/latest/minikube-linux-amd64
sudo install minikube-linux-amd64 /usr/local/bin/minikube

# 验证
minikube version
```

### 3. 安装 kubectl

```bash
# 下载 kubectl
curl -LO "https://dl.k8s.io/release/$(curl -L -s https://dl.k8s.io/release/stable.txt)/bin/linux/amd64/kubectl"
sudo chmod +x kubectl
sudo mv kubectl /usr/local/bin/

# 验证
kubectl version --client
```

### 4. 安装 Go (编译组件用)

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

### 5. 安装 Python 3.11

```bash
# 安装 EPEL 仓库
sudo yum install -y epel-release

# 安装 Python 3.11 (AL8 可用版本)
sudo yum install -y python3.11 python3.11-devel python3.11-pip gcc make openssl-devel libffi-devel

# 设置 alternatives 让 python3 指向 3.11
sudo alternatives --install /usr/bin/python3 python3 /usr/bin/python3.11 1
sudo alternatives --install /usr/bin/pip3 pip3 /usr/bin/pip3.11 1

# 验证
python3 --version
pip3 --version
```

### 6. 安装 uv (推荐)

```bash
# 使用官方脚本安装 uv
curl -LsSf https://astral.sh/uv/install.sh | sh

# 添加到 PATH
echo 'export PATH=$HOME/.local/bin:$PATH' >> ~/.bashrc
source ~/.bashrc

# 验证
uv --version
```

---

## Minikube 集群部署

### 1. 启动 Minikube 集群

```bash
# 启动集群
minikube start \
  --driver=docker \
  --cpus=4 \
  --memory=8192 \
  --disk-size=50g \
  --kubernetes-version=v1.29.2 \
  --container-runtime=containerd

# 验证
minikube status
```

### 2. 验证集群

```bash
# 配置 kubectl
eval "$(minikube docker-env)"

# 验证节点
kubectl get nodes

# 验证集群信息
kubectl cluster-info
```

---

## 构建组件镜像

**重要**: 所有组件镜像必须从**项目根目录**构建！

```bash
# 确保在项目根目录
cd ~/OpenSandbox

# 查看当前目录
pwd
# 应该输出: /home/fengjianhui.fjh/OpenSandbox
```

### 1. 构建 execd 镜像

```bash
# 从根目录构建，指定 Dockerfile 路径
docker build -t opensandbox/execd:v1.0.6 -f components/execd/Dockerfile .
```

### 2. 构建 egress 镜像

```bash
docker build -t opensandbox/egress:v1.0.1 -f components/egress/Dockerfile .
```

### 3. 构建 ingress 镜像

```bash
docker build -t opensandbox/ingress:v1.0.0 -f components/ingress/Dockerfile .
```

### 4. 构建 Kubernetes Controller 镜像

```bash
cd kubernetes
make docker-build IMG=opensandbox/controller:v1.0.0
make docker-build-task-executor TASK_EXECUTOR_IMG=opensandbox/task-executor:v1.0.0
cd ..
```

### 5. 加载镜像到 Minikube

```bash
minikube image load opensandbox/execd:v1.0.6
minikube image load opensandbox/egress:v1.0.1
minikube image load opensandbox/ingress:v1.0.0
minikube image load opensandbox/controller:v1.0.0
minikube image load opensandbox/task-executor:v1.0.0

# 验证镜像已加载
minikube image list | grep opensandbox
```

---

## 部署 Kubernetes Controller

### 1. 安装 CRD

```bash
cd ~/OpenSandbox/kubernetes
make install
```

### 2. 验证 CRD

```bash
kubectl get crd | grep sandbox
```

预期输出:
```
batchsandboxes.sandbox.opensandbox.io
pools.sandbox.opensandbox.io
```

### 3. 部署 Controller

```bash
make deploy IMG=opensandbox/controller:v1.0.0 TASK_EXECUTOR_IMG=opensandbox/task-executor:v1.0.0
```

### 4. 验证 Controller

```bash
kubectl get pods -n opensandbox-controller-system
```

预期输出:
```
NAME                                                   READY   STATUS    RESTARTS   AGE
opensandbox-controller-manager-xxx                      2/2     Running   0          1m
```

### 5. 创建工作命名空间

```bash
kubectl create namespace opensandbox
kubectl config set-context --current --namespace=opensandbox
```

---

## 部署 Ingress Gateway

### 1. 创建部署文件

```bash
cat > opensandbox-ingress.yaml << 'EOF'
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
        env:
        - name: KUBECONFIG
          value: /in-cluster-config
EOF
```

### 2. 部署

```bash
kubectl apply -f opensandbox-ingress.yaml
```

### 3. 验证

```bash
kubectl get pods -n opensandbox-ingress
```

---

## 配置 gVisor 安全运行时

### 1. 进入 Minikube 节点

```bash
minikube ssh
```

### 2. 安装 runsc (gVisor)

```bash
# 在节点内执行
curl -fsSL https://gvisor.dev/archive.key | sudo gpg --dearmor -o /usr/share/keyrings/gvisor-archive-keyring.gpg
echo "deb [signed-by=/usr/share/keyrings/gvisor-archive-keyring.gpg] https://storage.googleapis.com/gvisor/releases release main" | sudo tee /etc/apt/sources.list.d/gvisor.list
sudo apt-get update
sudo apt-get install -y runsc

# 验证
runsc --version
```

### 3. 配置 containerd

```bash
# 备份配置
sudo cp /etc/containerd/config.toml /etc/containerd/config.toml.bak

# 添加 gVisor 配置
cat | sudo tee -a /etc/containerd/config.toml << 'EOF'

[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runsc]
  runtime_type = "io.containerd.runsc.v1"
EOF

# 重启 containerd
sudo systemctl restart containerd

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

## 启动 OpenSandbox Server

### 1. 安装 Python 依赖

```bash
cd ~/OpenSandbox/server

# 使用 uv 安装依赖
uv sync
```

### 2. 创建配置文件

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
kubeconfig_path = "~/.kube/config"
namespace = "opensandbox"
workload_provider = "batchsandbox"
informer_enabled = true

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

### 3. 启动 Server

```bash
# 方式 1: 使用 uv
uv run python -m src.main

# 方式 2: 后台运行
nohup uv run python -m src.main > /var/log/opensandbox-server.log 2>&1 &
```

### 4. 验证 Server

```bash
curl http://localhost:8080/health
```

预期输出:
```json
{"status":"healthy"}
```

---

## 完整验证流程

### 验证 1: 创建测试 BatchSandbox

```bash
cat > test-sandbox.yaml << 'EOF'
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
      runtimeClassName: gvisor
      containers:
      - name: sandbox
        image: nginx:alpine
        command: ["sh", "-c", "echo 'gVisor sandbox!' && sleep 300"]
EOF

kubectl apply -f test-sandbox.yaml
```

### 验证 2: 检查状态

```bash
# 查看 BatchSandbox 状态
kubectl get batchsandbox test-gvisor-sandbox -n opensandbox -o wide

# 等待就绪
kubectl wait --for=jsonpath='{.status.ready}'=1 batchsandbox/test-gvisor-sandbox -n opensandbox --timeout=120s
```

### 验证 3: 查看 Pod

```bash
# 获取 Pod 名称
POD=$(kubectl get pods -n opensandbox -l sandbox.opensandbox.io/sandbox-id=test-gvisor-sandbox -o jsonpath='{.items[0].metadata.name}')

# 查看 Pod 运行时类
kubectl get pod $POD -n opensandbox -o jsonpath='{.spec.runtimeClassName}'

# 应该输出: gvisor
```

### 验证 4: 查看 Pod 日志

```bash
kubectl logs $POD -n opensandbox
```

预期输出包含:
```
gVisor sandbox!
```

### 验证 5: 验证 gVisor 隔离

```bash
# gVisor 隔离特性：/proc/kcore 不存在
kubectl exec $POD -n opensandbox -- sh -c "[ ! -f /proc/kcore ] && echo '✓ gVisor isolation working!'"
```

---

## 故障排查

### 问题 1: `python3` 仍然指向旧版本

```bash
# 重新设置 alternatives
sudo alternatives --install /usr/bin/python3 python3 /usr/bin/python3.11 1
sudo alternatives --config python3
# 选择 python3.11

# 验证
python3 --version
```

### 问题 2: Docker 构建失败 "not found"

**原因**: 在组件目录下构建，Dockerfile 期望从项目根目录构建

**解决**:
```bash
# 回到项目根目录
cd ~/OpenSandbox

# 从根目录构建
docker build -t opensandbox/execd:v1.0.6 -f components/execd/Dockerfile .
```

### 问题 3: `powertools` 仓库不存在

**原因**: AL8 使用 `crb` 替代 `powertools`

**解决**:
```bash
# 使用 crb
sudo yum config-manager --set-enabled crb

# 或直接安装
sudo yum install -y python3.11
```

### 问题 4: RuntimeClass 不存在

```bash
# 检查 RuntimeClass
kubectl get runtimeclass

# 重新创建
kubectl apply -f gvisor-runtimeclass.yaml
```

### 问题 5: Pod 启动失败

```bash
# 查看 Pod 事件
kubectl describe pod <pod-name> -n opensandbox

# 查看 Pod 日志
kubectl logs <pod-name> -n opensandbox

# 进入 Minikube 节点检查
minikube ssh
# 检查 containerd 配置
sudo cat /etc/containerd/config.toml | grep runsc
```

---

## 清理环境

```bash
# 删除测试资源
kubectl delete batchsandbox --all -n opensandbox
kubectl delete runtimeclass gvisor
kubectl delete -f opensandbox-ingress.yaml

# 删除 Controller
cd ~/OpenSandbox/kubernetes
make undeploy

# 停止 Minikube
minikube stop

# 删除 Minikube 集群
minikube delete

# 删除配置
rm ~/.sandbox.toml
```

---

## 快速构建脚本

```bash
cat > ~/OpenSandbox/build-all.sh << 'EOF'
#!/bin/bash
set -e

echo "=== 构建 OpenSandbox 所有组件 ==="

cd ~/OpenSandbox

echo "1/4: 构建 execd..."
docker build -t opensandbox/execd:v1.0.6 -f components/execd/Dockerfile .

echo "2/4: 构建 egress..."
docker build -t opensandbox/egress:v1.0.1 -f components/egress/Dockerfile .

echo "3/4: 构建 ingress..."
docker build -t opensandbox/ingress:v1.0.0 -f components/ingress/Dockerfile .

echo "4/4: 构建 controller..."
cd kubernetes
make docker-build IMG=opensandbox/controller:v1.0.0
cd ..

echo ""
echo "=== 构建完成 ==="
docker images | grep opensandbox
EOF

chmod +x ~/OpenSandbox/build-all.sh
```

---

## 附录: 常用命令

```bash
# === 集群管理 ===
minikube start              # 启动集群
minikube stop               # 停止集群
minikube delete             # 删除集群
minikube ssh                # 进入节点
minikube image load xxx     # 加载镜像

# === 资源查看 ===
kubectl get nodes           # 查看节点
kubectl get pods -A         # 查看所有 Pod
kubectl get runtimeclass    # 查看运行时类
kubectl get batchsandbox    # 查看 BatchSandbox

# === 日志查看 ===
kubectl logs -f deployment/xxx    # 查看 Deployment 日志
kubectl logs -f pod/xxx           # 查看 Pod 日志

# === 验证命令 ===
curl http://localhost:8080/health    # Server 健康检查
python3 --version                    # Python 版本
uv --version                         # uv 版本
```

---

## 总结

本文档涵盖了在 Alibaba Cloud Linux 8 系统上部署 OpenSandbox 的完整流程：

1. ✅ **环境准备** - Docker, Minikube, kubectl, Go, Python 3.11, uv
2. ✅ **Minikube 集群** - 启动和配置本地 K8s 集群
3. ✅ **组件构建** - 从项目根目录构建所有镜像
4. ✅ **Controller 部署** - CRD 和 Controller Manager
5. ✅ **Ingress Gateway** - 流量路由组件
6. ✅ **gVisor 配置** - 安全运行时安装和 RuntimeClass
7. ✅ **Server 启动** - Python FastAPI Server
8. ✅ **完整验证** - 端到端测试

**重要提示**:
- 所有 Docker 构建必须从项目根目录执行
- Python 使用 alternatives 切换到 3.11
- AL8 的 powertools 可能叫 crb 或不存在
