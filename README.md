# metalkit

单二进制裸机 PXE 自动装机系统。集成 DHCP、TFTP、HTTP 服务器，通过 PXE 网络启动裸机进入 Debian live 系统并自动安装操作系统。

---

## ✨ 特性

- **零依赖部署**：单个二进制 + boot 文件，无需 Docker/K8s
- **多发行版支持**：Ubuntu、Debian、Rocky Linux、AlmaLinux、CentOS
- **自动化装机**：PXE 启动 → 自动分区 → 写入镜像 → cloud-init 配置 → 重启进入系统
- **Web UI 管理**：机器清单、装机任务、网络配置、镜像管理
- **灵活网络配置**：支持 DHCP、静态 IP、bond、VLAN
- **IPMI 集成**：自动上下电、设置启动设备

---

## 📋 系统要求

### 部署服务器

- **操作系统**：Ubuntu 20.04+、Debian 11+、Rocky Linux 8+、AlmaLinux 8+
- **架构**：x86_64 (amd64)
- **网络**：至少一个物理网卡，UDP 67/69/4011 和 TCP 8080/9090 可绑定
- **权限**：root 或 sudo

### 目标机器（被装机的裸机）

- **启动方式**：支持 PXE（BIOS 或 UEFI，需禁用 Secure Boot）
- **BMC**：可选，用于 IPMI 远程上下电

---

## 🚀 快速开始

### 1. 下载发布包

从开发机获取最新的 `metalkit-<version>.tar.gz`（约 660MB）。

---

### 2. 部署到服务器

```bash
# 传输到目标服务器
scp metalkit-*.tar.gz root@<server-ip>:/root/

# SSH 登录
ssh root@<server-ip>

# 解压并安装
cd /root
tar -xzf metalkit-*.tar.gz
cd metalkit
sudo ./scripts/install.sh
```

安装脚本会提示输入：
- **Network interface**：网卡名（如 `ens32`）
- **Server IP**：服务器 IP（如 `192.168.10.10`）
- **HTTP listen addr**：HTTP 端口（如 `:9090`）
- **Admin username**：管理员用户名（默认 `admin`）
- **Admin password**：管理员密码

---

### 3. 访问 Web UI

打开浏览器访问：

```
http://<server-ip>:9090/ui/
```

用设置的管理员账号登录。

---

### 4. 上传 OS 镜像

在 Web UI 的 **Images** 页面上传操作系统镜像（支持 `.qcow2`、`.raw`、`.img`）。

---

### 5. 创建 Profile

在 **Profiles** 页面创建装机配置：
- 选择网络模式（DHCP 或静态 IP）
- 设置 root 密码
- 选择目标磁盘策略

---

### 6. 纳管机器并装机

1. 在 **Machines** 页面等待机器 PXE 启动后自动注册
2. 点击机器进入详情页
3. 点击 **Install** 按钮，选择镜像和 Profile
4. 系统自动：
   - IPMI 设置启动设备为 PXE
   - 重启机器
   - PXE 启动进入 live 系统
   - 自动分区、写入镜像、配置网络
   - 重启进入安装好的系统

---

## 📚 文档

- **[DEPLOY.md](./DEPLOY.md)** — 详细部署指南（更新、故障排查）
- **[DEVELOP.md](./DEVELOP.md)** — 开发指南（构建、测试、打包）
- **[docs/api.md](./docs/api.md)** — HTTP API 文档
- **[docs/features.md](./docs/features.md)** — 功能详解

---

## 🏗️ 架构

```
┌─────────────────────────────────────────────────────────────┐
│                    metalkit-controller                       │
│  ┌──────────┐  ┌──────────┐  ┌──────────┐  ┌──────────┐   │
│  │   DHCP   │  │   TFTP   │  │   HTTP   │  │  Web UI  │   │
│  │  :67     │  │   :69    │  │  :9090   │  │          │   │
│  └──────────┘  └──────────┘  └──────────┘  └──────────┘   │
│       │             │              │              │          │
└───────┼─────────────┼──────────────┼──────────────┼─────────┘
        │             │              │              │
        ▼             ▼              ▼              ▼
   PXE 启动      TFTP 下载      HTTP 下载       浏览器管理
   (iPXE)       (iPXE 二进制)   (内核/镜像)
        │             │              │
        └─────────────┴──────────────┘
                      │
                      ▼
            ┌──────────────────┐
            │  Debian Live 系统 │
            │  (metalkit-agent) │
            └──────────────────┘
                      │
                      ▼
            自动分区、写入镜像、配置网络
                      │
                      ▼
            重启进入安装好的操作系统
```

---

## 🔧 开发

### 构建

```bash
cd /opt/claude/metalkit

# 构建二进制
go build -buildvcs=false -o bin/controller ./cmd/controller
go build -buildvcs=false -o bin/agent      ./cmd/agent

# 构建 live 镜像（首次或修改 agent 后，耗时 10-30 分钟）
./scripts/build-live.sh --docker

# 打包发布版本
./scripts/package.sh
```

---

### 测试

```bash
# 运行所有测试
go test ./...

# 运行特定包测试
go test ./internal/dhcp/...
go test ./internal/bindings/...

# 带覆盖率
go test -cover ./...
```

详见 [DEVELOP.md](./DEVELOP.md)。

---

## 🐛 故障排查

### iPXE 获取不到 IP（yiaddr=0.0.0.0）

**症状**：PXE 机器显示 `No configuration methods succeeded`

**解决**：确保部署的是最新版本（2026-05-29 之后），包含 DHCP 修复。

---

### 端口被占用

**症状**：`doctor` 报错 `bind: address already in use`

**解决**：

```bash
# 查看占用者
sudo ss -ulnp | grep :69

# 停掉冲突服务
sudo systemctl stop dnsmasq
sudo systemctl disable dnsmasq
sudo systemctl restart metalkit-controller
```

---

### DHCP 模式下需要手动 dhclient

**症状**：装机后系统没有自动获取 IP

**原因**：旧版本 bug（已在 2026-05-29 修复）

**解决**：更新到最新版本。

详见 [DEPLOY.md](./DEPLOY.md) 的故障排查章节。

---

## 📂 项目结构

```
/opt/claude/metalkit/              # 开发源码
├── cmd/                           # 主程序入口
│   ├── controller/                # metalkit-controller（服务端）
│   └── agent/                     # metalkit-agent（live 镜像内）
├── internal/                      # 内部包
│   ├── dhcp/                      # DHCP 服务器
│   ├── httpd/                     # HTTP API + Web UI
│   ├── jobs/                      # 装机任务编排
│   ├── bindings/                  # 机器绑定
│   ├── installer/                 # cloud-init seed 生成
│   └── ...
├── scripts/
│   ├── build-live.sh              # 构建 Debian live 镜像
│   ├── install.sh                 # 目标服务器安装脚本
│   └── package.sh                 # 打包发布脚本
├── live-image/                    # live 镜像构建配置
├── bin/                           # 本地构建产物（不进 git）
├── boot/                          # 本地构建产物（不进 git）
├── dist/                          # 发布包输出（不进 git）
├── DEPLOY.md                      # 部署文档
├── DEVELOP.md                     # 开发文档
└── README.md                      # 本文件
```

---

## 🔐 安全建议

1. **修改默认密码**：首次登录后立即修改管理员密码
2. **限制访问**：使用防火墙规则只允许内网访问 Web UI
3. **定期备份**：备份 `/var/lib/metalkit/inventory.db`
4. **保护密钥**：`/var/lib/metalkit/master.key` 权限设为 0600

---

## 📝 已知限制

- **Secure Boot**：目标机器必须禁用 Secure Boot（iPXE 二进制未签名）
- **同子网 DHCP**：部署服务器所在子网不能有其他 DHCP 服务器（或使用 ProxyDHCP 模式）
- **IPv4 only**：当前仅支持 IPv4（IPv6 支持计划中）

---

## 🤝 贡献

欢迎提交 Issue 和 Pull Request！

1. Fork 仓库
2. 创建特性分支：`git checkout -b feat/my-feature`
3. 提交修改：`git commit -m "feat: add my feature"`
4. 推送分支：`git push origin feat/my-feature`
5. 创建 Pull Request

---

## 📄 许可证

[MIT License](./LICENSE)

---

## 📞 获取帮助

- **查看日志**：`journalctl -u metalkit-controller -f`
- **运行自检**：`/opt/metalkit/bin/metalkit-controller doctor -config /etc/metalkit/config.yaml`
- **文档**：[DEPLOY.md](./DEPLOY.md) | [DEVELOP.md](./DEVELOP.md)
- **live系统默认密码：root/metalkit
