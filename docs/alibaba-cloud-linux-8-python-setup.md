# Alibaba Cloud Linux 8 Python 安装指南

系统版本: `5.10.134-18.al8.x86_64` (Alibaba Cloud Linux 8)

---

## 方案一：使用系统自带 Python (推荐)

AL8 通常自带 Python 3.9+，检查系统版本：

```bash
# 检查系统自带 Python 版本
python3 --version

# 检查是否有其他版本
ls -la /usr/bin/python3*

# 检查是否有 Python 3.10+
rpm -qa | grep python3
```

如果 `python3 --version` 显示 >= 3.9，OpenSandbox Server 可以直接使用（支持 3.10+）。

### 安装 Python 开发包和工具

```bash
# 安装 Python 3 及相关工具
sudo yum install -y python3 python3-devel python3-pip python3-libvirt

# 安装虚拟环境支持
sudo yum install -y python3-venv

# 验证
python3 --version
pip3 --version
```

---

## 方案二：安装 Python 3.10/3.11 (EPEL 或 IUS)

如果需要 Python 3.10 或 3.11：

### 方法 1: 使用 EPEL + powertools

```bash
# 启用 EPEL 和 Powertools 仓库
sudo yum install -y epel-release
sudo yum config-manager --set-enabled powertools

# 安装 Python 3.11 (AL8 通常可用)
sudo yum install -y python3.11 python3.11-devel python3.11-pip

# 创建软链接（可选）
sudo alternatives --install /usr/bin/python3 python3 /usr/bin/python3.11 1

# 验证
python3.11 --version
```

### 方法 2: 使用 IUS 仓库

```bash
# 安装 IUS 仓库
sudo yum install -y https://repo.ius.io/ius-release-el8.rpm

# 安装 Python 3.10
sudo yum install -y python310 python310-devel python310-pip

# 验证
python3.10 --version
```

### 方法 3: 从源码编译 Python 3.10

```bash
# 安装编译依赖
sudo yum groupinstall -y "Development Tools"
sudo yum install -y gcc make openssl-devel bzip2-devel libffi-devel zlib-devel wget

# 下载 Python 3.10
cd /usr/src
wget https://www.python.org/ftp/python/3.10.17/Python-3.10.17.tgz
tar xzf Python-3.10.17.tgz
cd Python-3.10.17

# 配置和编译
./configure --enable-optimizations --enable-shared
make -j$(nproc)
sudo make altinstall

# 配置动态库路径
echo "/usr/local/lib" | sudo tee /etc/ld.so.conf.d/python3.10.conf
sudo ldconfig

# 验证
python3.10 --version

# 创建软链接（可选）
sudo ln -sf /usr/local/bin/python3.10 /usr/bin/python3.10
```

---

## 安装 uv (推荐的包管理器)

### 方法 1: 使用 pip 安装 (推荐)

```bash
# 使用 pip 安装 uv
pip3 install uv --user

# 添加到 PATH
echo 'export PATH=$HOME/.local/bin:$PATH' >> ~/.bashrc
source ~/.bashrc

# 验证
uv --version
```

### 方法 2: 使用官方安装脚本

```bash
# 下载并安装 uv
curl -LsSf https://astral.sh/uv/install.sh | sh

# 重新加载 shell
source ~/.bashrc

# 验证
uv --version
```

---

## 创建 Python 虚拟环境

### 使用 uv 创建 (推荐)

```bash
cd ~/OpenSandbox/server

# 创建虚拟环境并安装依赖
uv sync

# 激活环境后运行
uv run python -m src.main
```

### 使用 venv 创建

```bash
cd ~/OpenSandbox/server

# 创建虚拟环境
python3 -m venv .venv

# 激活虚拟环境
source .venv/bin/activate

# 升级 pip
pip install --upgrade pip

# 安装依赖
pip install -e .

# 运行 Server
python -m src.main

# 退出虚拟环境
deactivate
```

---

## 完整安装脚本 (Al8)

将以下内容保存为 `install-python-al8.sh`：

```bash
#!/bin/bash
set -e

echo "=== Alibaba Cloud Linux 8 Python 安装脚本 ==="

# 1. 检查系统
echo "检查系统版本..."
uname -r
cat /etc/alinux-release 2>/dev/null || cat /etc/redhat-release

# 2. 安装系统包
echo "安装 Python 和开发工具..."
sudo yum install -y epel-release
sudo yum config-manager --set-enabled powertools || true
sudo yum install -y python3 python3-devel python3-pip python3-venv
sudo yum install -y gcc make openssl-devel libffi-devel

# 3. 安装 uv
echo "安装 uv..."
pip3 install uv --user
export PATH=$HOME/.local/bin:$PATH

# 4. 验证
echo ""
echo "=== 安装完成，验证版本 ==="
python3 --version
pip3 --version
uv --version || echo "uv 需要重新加载 shell: source ~/.bashrc"

# 5. 提示
echo ""
echo "=== 下一步 ==="
echo "1. 如果 uv 命令找不到，运行: source ~/.bashrc"
echo "2. 进入 Server 目录: cd ~/OpenSandbox/server"
echo "3. 安装依赖: uv sync"
echo "4. 启动 Server: uv run python -m src.main"
```

运行脚本：

```bash
chmod +x install-python-al8.sh
./install-python-al8.sh
```

---

## 快速验证命令

```bash
# 一键检查所有环境
echo "=== Python 环境检查 ==="
echo "Python 版本: $(python3 --version)"
echo "Pip 版本: $(pip3 --version)"
echo "uv 版本: $(uv --version 2>/dev/null || echo '未安装或未在 PATH 中')"
echo ""
echo "=== 虚拟环境支持 ==="
python3 -m venv --help >/dev/null 2>&1 && echo "✓ venv 支持" || echo "✗ venv 不支持"
echo ""
echo "=== 测试导入 ==="
python3 -c "import sys; print(f'Python 路径: {sys.executable}')"
python3 -c "import ssl; print('✓ SSL 模块可用')"
```

---

## 常见问题

### Q1: `python3: command not found`

```bash
# 查找 Python
sudo find /usr -name "python3*" 2>/dev/null

# 或安装 Python
sudo yum install -y python3
```

### Q2: `pip3: command not found`

```bash
# 安装 pip
sudo yum install -y python3-pip

# 或使用 ensurepip
python3 -m ensurepip --upgrade
```

### Q3: SSL/TLS 错误

```bash
# 安装 SSL 库
sudo yum install -y openssl-devel
```

### Q4: 共享库错误

```bash
# 配置动态库路径
echo "/usr/local/lib" | sudo tee /etc/ld.so.conf.d/python-local.conf
sudo ldconfig
```

---

## 推荐配置 (Al8)

对于 Alibaba Cloud Linux 8，推荐使用：

```bash
# 1. 使用系统自带的 Python 3.9+
sudo yum install -y python3 python3-devel python3-pip python3-venv

# 2. 安装 uv
pip3 install uv --user
export PATH=$HOME/.local/bin:$PATH

# 3. 在 Server 目录使用
cd ~/OpenSandbox/server
uv sync
uv run python -m src.main
```
