# metalkit 部署指南

本文档说明如何将 metalkit 部署到生产服务器。

---

## 📋 前置要求

### 目标服务器要求

- **操作系统**：Ubuntu 20.04+、Debian 11+、Rocky Linux 8+、AlmaLinux 8+、CentOS 7+
- **架构**：x86_64 (amd64)
- **权限**：root 或 sudo 访问
- **网络**：
  - 至少一个物理网卡（用于 PXE）
  - UDP 端口 67（DHCP）、69（TFTP）、4011（BSDP）可绑定
  - TCP 端口 8080 或 9090（HTTP，可配置）可绑定
  - 同子网内**不能**有其他 DHCP 服务器（除非使用 ProxyDHCP 模式）

### 开发机要求（打包用）

- Go 1.21+（推荐 1.25）
- Docker（仅在需要重新构建 live 镜像时）

---

## 🚀 快速部署（推荐）

### 1. 在开发机打包最新版本

```bash
cd /opt/claude/metalkit

# 构建二进制（包含最新代码修复）
/usr/local/go/bin/go build -buildvcs=false -o bin/controller ./cmd/controller
/usr/local/go/bin/go build -buildvcs=false -o bin/agent      ./cmd/agent

# 打包发布版本（输出到 dist/ 目录）
./scripts/package.sh

# 查看生成的 tarball
ls -lh dist/
```

输出示例：`dist/metalkit-20260529-192300.tar.gz`（约 660MB）

---

### 2. 传输到目标服务器

```bash
# 替换 <target-ip> 为目标服务器 IP
scp dist/metalkit-*.tar.gz root@<target-ip>:/root/
```

---

### 3. 在目标服务器上安装

SSH 登录到目标服务器：

```bash
ssh root@<target-ip>
```

解压并运行安装脚本：

```bash
cd /root
tar -xzf metalkit-*.tar.gz
cd metalkit

# 交互式安装（会提示输入配置）
sudo ./scripts/install.sh

# 或非交互式安装（适合自动化部署）
sudo MK_INTERFACE=ens32 \
     MK_SERVER_IP=192.168.10.10 \
     MK_HTTP_ADDR=:9090 \
     MK_ADMIN_USER=admin \
     MK_ADMIN_PASS='your-password' \
     ./scripts/install.sh --yes
```

安装脚本会自动：
- 安装运行依赖（`ipmitool`、`qemu-utils`、`whois` 等）
- 部署二进制到 `/opt/metalkit/bin/`
- 部署 boot 文件到 `/opt/metalkit/boot/`
- 生成配置文件 `/etc/metalkit/config.yaml`
- 安装 systemd 服务 `metalkit-controller.service`
- 运行 `doctor` 自检
- 启动服务

---

### 4. 验证部署

```bash
# 查看服务状态
systemctl status metalkit-controller

# 查看实时日志
journalctl -u metalkit-controller -f

# 运行自检
/opt/metalkit/bin/metalkit-controller doctor -config /etc/metalkit/config.yaml

# 访问 Web UI
# http://<MK_SERVER_IP>:9090/ui/
```

---

## 🔄 更新已部署的服务器

### 方式 A：完整更新（推荐）

重新打包并运行 `install.sh`（脚本是幂等的，会保留配置和数据）：

```bash
# 开发机
cd /opt/claude/metalkit
/usr/local/go/bin/go build -buildvcs=false -o bin/controller ./cmd/controller
/usr/local/go/bin/go build -buildvcs=false -o bin/agent      ./cmd/agent
./scripts/package.sh
scp dist/metalkit-*.tar.gz root@<target-ip>:/root/

# 目标服务器
ssh root@<target-ip>
cd /root
tar -xzf metalkit-*.tar.gz
cd metalkit
sudo ./scripts/install.sh --yes  # 保留现有配置
```

---

### 方式 B：仅更新二进制（快速）

适合只修改了 controller 或 agent 代码的情况：

```bash
# 开发机
cd /opt/claude/metalkit
/usr/local/go/bin/go build -buildvcs=false -o bin/controller ./cmd/controller
scp bin/controller root@<target-ip>:/tmp/

# 目标服务器
ssh root@<target-ip>
systemctl stop metalkit-controller
install -m 0755 /tmp/controller /opt/metalkit/bin/metalkit-controller
systemctl start metalkit-controller
journalctl -u metalkit-controller -f
```

---

### 方式 C：更新 live 镜像

如果修改了 agent 或 live 镜像内容（不常见）：

```bash
# 开发机（需要 Docker，耗时 10-30 分钟）
cd /opt/claude/metalkit
./scripts/build-live.sh --docker

# 然后按方式 A 完整更新
```

---

## 🛠️ 常见问题

### 1. 端口被占用

**症状**：`doctor` 报错 `TFTP port 69 — bind: address already in use`

**解决**：

```bash
# 查看占用者
sudo ss -ulnp | grep :69

# 停掉冲突服务（常见：dnsmasq、tftpd-hpa）
sudo systemctl stop dnsmasq
sudo systemctl disable dnsmasq
sudo systemctl restart metalkit-controller
```

---

### 2. iPXE 获取不到 IP（yiaddr=0.0.0.0）

**症状**：PXE 机器显示 `No configuration methods succeeded`，日志显示 `yiaddr":"0.0.0.0"`

**原因**：部署的二进制版本过旧，缺少 DHCP 修复

**解决**：确保使用最新打包的版本（2026-05-29 之后），按**方式 A** 或**方式 B** 重新部署

---

### 3. 防火墙拦截

**症状**：PXE 机器无法连接，日志无 DHCP 请求

**解决**：

```bash
# Ubuntu/Debian
sudo ufw allow 67/udp
sudo ufw allow 69/udp
sudo ufw allow 4011/udp
sudo ufw allow 9090/tcp

# RHEL/CentOS
sudo firewall-cmd --permanent --add-port=67/udp
sudo firewall-cmd --permanent --add-port=69/udp
sudo firewall-cmd --permanent --add-port=4011/udp
sudo firewall-cmd --permanent --add-port=9090/tcp
sudo firewall-cmd --reload
```

---

### 4. 配置错误

**症状**：`doctor` 报错 `serverIP "8090": ParseAddr("8090"): unable to parse IP`

**解决**：手动编辑配置文件

```bash
sudo nano /etc/metalkit/config.yaml
```

确保：
- `serverIP: 192.168.10.10`（IP 地址，不是端口）
- `httpAddr: ":9090"`（带冒号的端口）
- `interface: ens32`（实际网卡名）

修改后重启：

```bash
sudo systemctl restart metalkit-controller
```

---

## 📂 部署后的目录结构

```
/opt/metalkit/
├── bin/
│   ├── metalkit-controller    # controller 二进制
│   └── metalkit-agent          # agent 二进制（打包进 live 镜像）
└── boot/
    ├── vmlinuz                 # Linux 内核
    ├── initrd.img              # initramfs
    └── filesystem.squashfs     # Debian live 根文件系统

/etc/metalkit/
└── config.yaml                 # 配置文件

/var/lib/metalkit/
├── inventory.db                # SQLite 数据库
├── images/                     # 上传的 OS 镜像
└── master.key                  # 加密密钥（自动生成）

/etc/systemd/system/
└── metalkit-controller.service # systemd 服务单元
```

---

## 🔐 安全建议

1. **修改默认管理员密码**：首次登录后立即修改
2. **限制 HTTP 访问**：使用防火墙规则只允许内网访问 Web UI
3. **定期备份数据库**：`/var/lib/metalkit/inventory.db`
4. **保护 master.key**：`/var/lib/metalkit/master.key` 用于加密敏感数据，权限 0600

---

## 📞 获取帮助

- 查看日志：`journalctl -u metalkit-controller -f`
- 运行自检：`/opt/metalkit/bin/metalkit-controller doctor -config /etc/metalkit/config.yaml`
- 查看配置：`cat /etc/metalkit/config.yaml`
