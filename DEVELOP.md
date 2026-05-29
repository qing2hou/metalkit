# metalkit 开发指南

本文档说明如何在本地开发、构建和测试 metalkit。

---

## 📋 开发环境要求

- **Go**：1.21+ (推荐 1.25)
- **Docker**：用于构建 live 镜像（可选，仅在修改 live 镜像时需要）
- **操作系统**：Linux（推荐 Ubuntu 22.04+ 或 Debian 12+）
- **工具**：
  - `git`
  - `make`（可选）
  - `ipmitool`、`qemu-img`、`mkpasswd`（运行时依赖，测试用）

---

## 🏗️ 项目结构

```
/opt/claude/metalkit/              # 开发源码（git 仓库）
├── cmd/                           # 主程序入口
│   ├── controller/                # metalkit-controller（服务端）
│   └── agent/                     # metalkit-agent（live 镜像内运行）
├── internal/                      # 内部包
│   ├── dhcp/                      # DHCP 服务器
│   ├── httpd/                     # HTTP API + Web UI
│   ├── jobs/                      # 装机任务编排
│   ├── bindings/                  # 机器绑定（profile + subnet）
│   ├── installer/                 # cloud-init seed 生成
│   └── ...
├── scripts/
│   ├── build-live.sh              # 构建 Debian live 镜像
│   ├── install.sh                 # 目标服务器安装脚本
│   └── package.sh                 # 打包发布脚本
├── live-image/                    # live 镜像构建配置
│   ├── config/                    # live-build 配置
│   └── tftpboot/                  # TFTP 启动文件
├── docs/                          # 文档
├── go.mod                         # Go 模块定义
├── go.sum
├── .gitignore
├── DEPLOY.md                      # 部署文档
└── README.md

# 构建产物（不进 git）
├── bin/                           # 本地构建的二进制
│   ├── controller
│   └── agent
├── boot/                          # 本地构建的 boot 文件
│   ├── vmlinuz
│   ├── initrd.img
│   └── filesystem.squashfs
└── dist/                          # 发布包输出
    └── metalkit-<version>.tar.gz
```

---

## 🚀 快速开始

### 1. 克隆仓库

```bash
git clone <repo-url> /opt/claude/metalkit
cd /opt/claude/metalkit
```

---

### 2. 构建二进制

```bash
# 构建 controller
go build -o bin/controller ./cmd/controller

# 构建 agent
go build -o bin/agent ./cmd/agent

# 或使用推荐的 go 版本（如果系统 go 版本过低）
/usr/local/go/bin/go build -buildvcs=false -o bin/controller ./cmd/controller
/usr/local/go/bin/go build -buildvcs=false -o bin/agent      ./cmd/agent
```

---

### 3. 构建 live 镜像（首次或修改 agent 后）

```bash
# 使用 Docker（推荐，隔离环境）
./scripts/build-live.sh --docker

# 或原生构建（需要 root，会污染系统）
sudo ./scripts/build-live.sh --native
```

构建完成后，`boot/` 目录会包含：
- `vmlinuz` — Linux 内核
- `initrd.img` — initramfs
- `filesystem.squashfs` — Debian live 根文件系统（约 600MB）

**注意**：live 镜像构建耗时 10-30 分钟，仅在以下情况需要重新构建：
- 首次开发
- 修改了 `cmd/agent/` 代码
- 修改了 `live-image/` 配置

---

### 4. 运行测试

```bash
# 运行所有测试
go test ./...

# 运行特定包的测试
go test ./internal/dhcp/...
go test ./internal/bindings/...

# 带覆盖率
go test -cover ./...

# 详细输出
go test -v ./internal/dhcp/...
```

---

## 🔧 开发工作流

### 修改代码后

```bash
# 1. 重新构建
go build -buildvcs=false -o bin/controller ./cmd/controller

# 2. 运行测试
go test ./internal/...

# 3. 本地测试（可选）
# 创建测试配置
cp config.example.yaml config.local.yaml
# 编辑 config.local.yaml 填入本地网卡和 IP
nano config.local.yaml

# 以 root 运行（需要绑定特权端口 67/69）
sudo ./bin/controller -config config.local.yaml

# 4. 打包发布版本
./scripts/package.sh
```

---

### 修改 agent 后

```bash
# 1. 重新构建 agent
go build -buildvcs=false -o bin/agent ./cmd/agent

# 2. 重新构建 live 镜像（会自动打包 bin/agent）
./scripts/build-live.sh --docker

# 3. 打包发布版本
./scripts/package.sh
```

---

## 📦 打包发布

### 使用 package.sh 脚本

```bash
# 自动用时间戳作为版本号
./scripts/package.sh

# 指定版本号
./scripts/package.sh v1.2.3

# 输出
ls -lh dist/
# dist/metalkit-20260529-192300.tar.gz
```

打包内容：
- `bin/controller`
- `bin/agent`
- `boot/vmlinuz`
- `boot/initrd.img`
- `boot/filesystem.squashfs`
- `scripts/install.sh`
- `config.example.yaml`（可选）
- `README.md`（可选）

---

## 🧪 测试指南

### 单元测试

```bash
# DHCP 服务器测试
go test ./internal/dhcp/...

# Bindings 逻辑测试
go test ./internal/bindings/...

# Installer seed 生成测试
go test ./internal/installer/...
```

---

### 集成测试

```bash
# 需要 root 权限（绑定特权端口）
sudo go test ./internal/dhcp/ -tags=integration
```

---

### 手动测试

1. **启动 controller**：

```bash
# 创建测试配置
cat > config.test.yaml <<EOF
serverIP: 192.168.10.10
interface: ens32
httpAddr: ":9090"
dhcpAddr: ":67"
bsdpAddr: ":4011"
tftpAddr: ":69"
bootDir: ./boot
logLevel: debug
dbPath: /tmp/metalkit-test.db
imagesDir: /tmp/metalkit-images
masterKeyPath: /tmp/metalkit-master.key
adminUser: admin
adminPass: "test123"
EOF

# 以 root 运行
sudo ./bin/controller -config config.test.yaml
```

2. **访问 Web UI**：

打开浏览器访问 `http://192.168.10.10:9090/ui/`，用 `admin/test123` 登录。

3. **测试 PXE 启动**：

在同一子网内启动一台 PXE 机器，观察日志：

```bash
journalctl -u metalkit-controller -f
```

应该看到：
```
dhcp: received mac=... msg_type=DHCPDISCOVER
dhcp: reply stage=lease yiaddr=192.168.10.20
dhcp: received mac=... msg_type=DHCPREQUEST
dhcp: reply stage=ipxe yiaddr=192.168.10.20 bootfile=http://...
```

---

## 🐛 调试技巧

### 1. 启用 debug 日志

```yaml
# config.yaml
logLevel: debug
```

---

### 2. 查看 DHCP 交互

```bash
# 在 controller 所在机器抓包
sudo tcpdump -i ens32 -n port 67 or port 68 -vv
```

---

### 3. 查看 TFTP 请求

```bash
# 日志会显示 TFTP 文件请求
journalctl -u metalkit-controller -f | grep tftp
```

---

### 4. 检查 live 镜像内容

```bash
# 挂载 squashfs 查看
sudo mkdir -p /mnt/metalkit-live
sudo mount -o loop boot/filesystem.squashfs /mnt/metalkit-live
ls -la /mnt/metalkit-live/usr/local/bin/
# 应该看到 metalkit-agent
sudo umount /mnt/metalkit-live
```

---

## 📝 代码规范

### Go 代码风格

- 遵循 `gofmt` 格式
- 使用 `golangci-lint` 检查（可选）
- 包名小写，单数形式
- 导出的函数/类型使用 PascalCase
- 私有的函数/类型使用 camelCase

---

### 提交规范

```
<type>: <subject>

<body>

Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
```

**type**：
- `feat`: 新功能
- `fix`: Bug 修复
- `refactor`: 重构
- `test`: 测试
- `docs`: 文档
- `chore`: 构建/工具

**示例**：

```
fix: iPXE DHCP yiaddr=0.0.0.0 issue

Remove iPXE special-case in buildAck() that delegated to legacy
buildReply returning yiaddr=0.0.0.0. Now all REQUESTs in full mode
properly allocate IP and apply lease options + PXE attach.

Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
```

---

## 🔍 常见开发问题

### 1. `go build` 报错 `error obtaining VCS status`

**解决**：

```bash
go build -buildvcs=false -o bin/controller ./cmd/controller
```

---

### 2. live 镜像构建失败

**症状**：`lb build` 报错

**解决**：

```bash
# 清理缓存重试
cd live-image
sudo lb clean --purge
cd ..
./scripts/build-live.sh --docker
```

---

### 3. 测试时端口被占用

**症状**：`bind: address already in use`

**解决**：

```bash
# 查看占用者
sudo ss -ulnp | grep :67

# 停掉冲突服务
sudo systemctl stop dnsmasq
```

---

### 4. 修改代码后行为没变化

**原因**：可能运行的是旧二进制

**解决**：

```bash
# 确认二进制时间戳
ls -lh bin/controller

# 重新构建
go build -buildvcs=false -o bin/controller ./cmd/controller

# 确认时间戳更新
ls -lh bin/controller
```

---

## 📚 相关文档

- [DEPLOY.md](./DEPLOY.md) — 部署指南
- [docs/api.md](./docs/api.md) — API 文档
- [docs/features.md](./docs/features.md) — 功能说明

---

## 🤝 贡献指南

1. Fork 仓库
2. 创建特性分支：`git checkout -b feat/my-feature`
3. 提交修改：`git commit -m "feat: add my feature"`
4. 推送分支：`git push origin feat/my-feature`
5. 创建 Pull Request

---

## 📞 获取帮助

- 查看日志：`journalctl -u metalkit-controller -f`
- 运行自检：`./bin/controller doctor -config config.yaml`
- 查看测试覆盖率：`go test -cover ./...`
