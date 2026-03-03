# OpenSandbox 快速部署指南 (Linux + Minikube)

5 分钟快速部署 OpenSandbox 核心组件

---

## 快速开始

### 1. 启动 Minikube (1 分钟)

```bash
# 启动集群
minikube start --cpus=4 --memory=8192 --driver=docker

# 验证
kubectl get nodes
```

### 2. 配置 gVisor (2 分钟)

```bash
# 进入节点
minikube ssh

# 安装 gVisor
curl -fsSL https://gvisor.dev/archive.key | sudo gpg --dearmor -o /usr/share/keyrings/gvisor-archive-keyring.gpg
echo "deb [signed-by=/usr/share/keyrings/gvisor-archive-keyring.gpg] https://storage.googleapis.com/gvisor/releases release main" | sudo tee /etc/apt/sources.list.d/gvisor.list
sudo apt-get update && sudo apt-get install -y runsc

# 配置 containerd
cat | sudo tee -a /etc/containerd/config.toml << 'EOF'

[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runsc]
  runtime_type = "io.containerd.runsc.v1"
EOF

# 重启 containerd
sudo systemctl restart containerd

# 退出节点
exit
```

### 3. 部署 Controller (1 分钟)

```bash
cd ~/OpenSandbox/kubernetes

# 安装 CRD
make install

# 构建和加载镜像
make docker-build IMG=opensandbox/controller:v1.0.0
minikube image load opensandbox/controller:v1.0.0

# 部署 Controller
make deploy IMG=opensandbox/controller:v1.0.0 TASK_EXECUTOR_IMG=opensandbox/task-executor:v1.0.0

# 验证
kubectl get pods -n opensandbox-controller-system
```

### 4. 创建 RuntimeClass (10 秒)

```bash
cat > gvisor-runtimeclass.yaml << 'EOF'
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: gvisor
handler: runsc
EOF

kubectl apply -f gvisor-runtimeclass.yaml
```

### 5. 创建命名空间 (10 秒)

```bash
kubectl create namespace opensandbox
kubectl config set-context --current --namespace=opensandbox
```

### 6. 启动 Server (30 秒)

```bash
cd ~/OpenSandbox/server

# 安装依赖
uv sync

# 创建配置
cat > ~/.sandbox.toml << 'EOF'
[server]
host = "0.0.0.0"
port = 8080

[runtime]
type = "kubernetes"
execd_image = "opensandbox/execd:v1.0.6"

[kubernetes]
namespace = "opensandbox"
workload_provider = "batchsandbox"

[secure_runtime]
type = "gvisor"
k8s_runtime_class = "gvisor"

[ingress]
mode = "direct"
EOF

# 启动 Server
uv run python -m src.main
```

> **注意**: 如果 `uv` 命令找不到，运行以下命令安装：
> ```bash
> # CentOS/RHEL/Rocky Linux
> pip3 install uv --user && export PATH=$HOME/.local/bin:$PATH
>
> # Ubuntu/Debian
> pip install uv
> ```

### 7. 验证 (1 分钟)

```bash
# 创建测试沙箱
cat > test-sandbox.yaml << 'EOF'
apiVersion: sandbox.opensandbox.io/v1alpha1
kind: BatchSandbox
metadata:
  name: test-secure
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

# 等待就绪
kubectl wait --for=jsonpath='{.status.ready}'=1 batchsandbox/test-secure --timeout=60s

# 查看日志
kubectl logs -l sandbox.opensandbox.io/sandbox-id=test-secure

# 验证 gVisor 隔离
POD=$(kubectl get pods -l sandbox.opensandbox.io/sandbox-id=test-secure -o name)
kubectl exec $POD -- sh -c "[ ! -f /proc/kcore ] && echo '✓ gVisor isolation working!'"
```

---

## 清理

```bash
# 删除测试资源
kubectl delete batchsandbox test-secure
kubectl delete runtimeclass gvisor

# 停止集群
minikube stop
```

---

## 故障排查

| 问题 | 解决方案 |
|------|----------|
| Server 无法连接 K8s | 检查 `kubectl config current-context` |
| Pod 启动失败 | 进入 `minikube ssh` 检查 containerd 配置 |
| RuntimeClass 不存在 | 重新运行 `kubectl apply -f gvisor-runtimeclass.yaml` |

---

## 下一步

- 阅读完整手册：`docs/opensandbox-linux-minikube-deployment-guide.md`
- 了解 Ingress Gateway 部署
- 配置 Kata Containers 运行时
