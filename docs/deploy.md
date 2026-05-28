# metalkit 打包部署完整手册

把 metalkit 从源代码构建成产物,部署到 PXE 控制节点,接管同二层网段里的目标服务器:
PXE 装机、硬件纳管、远程上电/重装。

适用版本:当前主线(含 Noble cloud-image 支持、IPMI `options=efiboot` 修复、fstab 驱动挂载、
agent live-image 自带完整安装工具链)。

> **三套机器**贯穿全文,先弄清角色:
> - **构建机**:跑 `make build / make agent / make live` 的开发机或 CI。可以联网。
> - **控制节点**:跑 `metalkit-controller` 服务的 Linux 主机。需要绑被装段的网卡。
> - **被装机**:接到同二层网段的目标服务器。PXE/UEFI 开启,BMC 可达。
>
> 构建机和控制节点可以是同一台。被装机一定是独立的物理机(或带 PXE 启动的 VM)。

---

## 0. 总体流程图

```
  源代码 (/opt/claude/devops)
        │
   ┌────┴────┐
   │ make    │ ── 构建机:Go 1.25+, live-build 或 docker, 联网
   │ build   │ ── 产出: bin/controller bin/agent
   │ agent   │           boot/{vmlinuz,initrd.img,filesystem.squashfs}
   │ live    │           internal/ipxebin/assets/{undionly.kpxe,snponly.efi,ipxe.efi}
   └────┬────┘                            (后者已 go:embed 进 controller)
        │
        │ rsync / scp
        ▼
  控制节点 (任意 Linux: Debian 11+ / Ubuntu 20.04+ / RHEL 7+)
        │
   ┌────┴────────────────────────┐
   │ sudo scripts/install.sh     │ ── apt/dnf 装 runtime deps
   │   (或手工 §4-§7)            │ ── 落 /opt/metalkit/{bin,boot}
   │                             │ ── 落 /etc/metalkit/config.yaml
   │                             │ ── 落 /var/lib/metalkit/{db,images,key}
   │                             │ ── 装 systemd unit
   │                             │ ── 跑 doctor preflight
   └────┬────────────────────────┘
        │
        ▼
  systemctl start metalkit-controller
        │
        │  广播 PXE OFFER (UDP 67)
        │  BSDP ACK (UDP 4011) → 发 snponly.efi 路径
        │  TFTP 服务 (UDP 69) → 发 iPXE 二进制
        │  HTTP 服务 (TCP 8080) → 发 iPXE 脚本 + kernel + initrd + squashfs + API + UI
        ▼
  被装机 PXE → iPXE → live 镜像 → agent 上报 → 装机
```

---

## 1. 构建机:工具链与依赖

### 1.1 编译 `bin/controller` 和 `bin/agent`

| 依赖 | 版本 | 来源 |
|---|---|---|
| Go 工具链 | **1.25.0+**(`go.mod` 钉死,旧版会拒绝构建) | golang.org/dl 或发行版 backports |
| git | 任意(`make build` 用 `git describe` 嵌版本号) | apt/dnf |
| 网络 | 拉 `proxy.golang.org`(或 GOPROXY 镜像)解决 modules | / |

操作:

```bash
git clone <metalkit-repo> && cd metalkit
make build           # → bin/controller   (CGO_ENABLED=0, 静态)
make agent           # → bin/agent        (CGO_ENABLED=0 GOOS=linux GOARCH=amd64, 静态)
```

- 两个二进制都是 `-trimpath -ldflags='-s -w'`,体积约 15 MB / 6.5 MB。
- 静态链接:控制节点 / live image 都不需要装 glibc 对应版本。
- 系统 Go 太旧时:`GOTOOLCHAIN=auto make build`,让 Go 1.21+ 自动下载 1.25;
  或显式指定:`GO=/path/to/go1.25 make build`。

### 1.2 拉 iPXE 二进制(只要做一次)

```bash
make ipxe           # → internal/ipxebin/assets/{undionly.kpxe,snponly.efi,ipxe.efi}
```

`scripts/fetch-ipxe.sh` 从 `https://boot.ipxe.org` 拉三个文件,放进
`internal/ipxebin/assets/`。这些资产通过 `//go:embed` 编进 `bin/controller`,
**之后不需要再放到 TFTP 目录里** — controller 内置的 TFTP 服务直接从内存返回。

iPXE 升级或第一次构建必须联网到 boot.ipxe.org。已签 SHA-256 可在脚本里写死做严格校验。

### 1.3 打 live image(squashfs 三件套)

这是最重的一步:产出 `boot/{vmlinuz, initrd.img, filesystem.squashfs}`,
被装机 PXE 后跑的就是这个 Debian Bookworm 系统,里面已经 baked 了 `metalkit-agent`。

#### 1.3.1 后端选择

`scripts/build-live.sh` 自动选,也可显式 `--native` / `--docker`:

| 后端 | 适用 | 依赖 |
|---|---|---|
| **native** | Debian/Ubuntu 构建机 | `live-build`(apt 装),**root 权限**(debootstrap/chroot 要) |
| **docker** | 任何能跑 docker 的机器(macOS / RHEL / WSL2 都行) | `docker`,容器要 `--privileged` |

```bash
sudo apt install live-build           # 仅 native 后端要
make live                              # auto: 有 lb 用 native,否则用 docker
# 或
sudo ./scripts/build-live.sh --native
./scripts/build-live.sh --docker
```

首次构建 10–25 分钟(取决于网速 + CPU),增量(改 hook 或换 agent 二进制)2–5 分钟。

#### 1.3.2 live-build 拉的网络资源

| 资源 | 用途 | 镜像源 |
|---|---|---|
| `debian-bookworm` debootstrap | 基础 rootfs | `deb.debian.org` 或自动镜像选择 |
| apt main + contrib + non-free-firmware | 内核 + 工具 + 固件 | 同上 |
| `docker.io/library/debian:bookworm` | docker 后端的容器底 | docker hub |

所有都要联网。离线构建可以预先 `lb config --mirror-bootstrap <内网镜像>` 改源,然后
把 `live-image/cache/` 整个打包带走;再次 `lb build` 会复用缓存。

#### 1.3.3 squashfs 里固化的包(`live-image/config/package-lists/`)

`live.list.chroot`(基础 live 系统 + agent 采集):

```
live-boot live-config live-config-systemd linux-image-amd64 systemd-sysv
iproute2 iputils-ping curl ca-certificates chrony pciutils dmidecode
bash less vim-tiny firmware-linux firmware-linux-nonfree openssh-server
net-tools lsof strace htop ethtool
```

`installer.list.chroot`(M2 装机阶段工具,agent 调用):

```
qemu-utils parted gdisk dosfstools e2fsprogs xfsprogs btrfs-progs
lvm2 mdadm cryptsetup-bin nvme-cli hdparm smartmontools
rsync wget xz-utils zstd pv ipmitool
cloud-guest-utils cloud-image-utils mtools xorriso
grub-efi-amd64-bin grub-efi-amd64-signed shim-signed efibootmgr
whois
```

**改包列表必须重打 live image**。改 hook(密码、SSH key、systemd unit)同理。

#### 1.3.4 关键 hook(`live-image/config/hooks/normal/`)

| Hook | 作用 |
|---|---|
| `0500-metalkit-ssh.hook.chroot` | 设 root/`metalkit`、user/`metalkit`,开 `PermitRootLogin yes` |
| `0600-metalkit-agent.hook.chroot` | `systemctl enable metalkit-agent.service` + chrony 时间同步 |
| `0700-metalkit-vt-init.hook.chroot` | 启动时 `setleds -D -scroll` 解 iDRAC Avocent 的 Scroll Lock 黑屏 |

`scripts/build-live.sh` 在 `lb build` 前把 `bin/agent` 复制到
`live-image/config/includes.chroot/usr/local/bin/metalkit-agent`,构建结束后清掉
(`trap rm`),保证不进 git。

### 1.4 构建产物清单

完成 §1.1–§1.3 后,仓库里有:

```
bin/controller                                15 MB
bin/agent                                      6.5 MB
boot/vmlinuz                                   8 MB     (Debian 6.1 内核 amd64)
boot/initrd.img                                62 MB    (live-boot initramfs)
boot/filesystem.squashfs                       580 MB   (Debian bookworm + agent + installer 工具集, xz 压缩)
internal/ipxebin/assets/undionly.kpxe          ~80 KB   (已 embed 进 controller)
internal/ipxebin/assets/snponly.efi            ~160 KB  (已 embed 进 controller)
internal/ipxebin/assets/ipxe.efi               ~1 MB    (已 embed 进 controller)
```

**部署只需要带走前 5 个文件**(`scripts/install.sh` 也只读这些)。iPXE 资产已经
在编译期进了 controller 二进制,不用再单独传。

---

## 2. 控制节点:硬件/系统要求

| 项 | 要求 | 备注 |
|---|---|---|
| OS | Debian 11+ / Ubuntu 20.04+ / RHEL 7+(CentOS Stream / Rocky / Alma / Fedora / OL) | 其他发行版需手工部署 |
| 架构 | amd64 | live image 也是 amd64;arm64 未测试 |
| CPU/RAM | 2 vCPU / 2 GB 起 | 加机器主要拼 IO,CPU 通常空 |
| 磁盘 | 系统盘外 ≥ 30 GB 可用 | OS 镜像 + 上传中转 + sqlite + 备份 |
| 网卡 | **一张接到被装段同二层的网卡,绑静态 IPv4** | 必填;不能是 0.0.0.0 / 域名 / CNAME |
| 端口 | UDP 67、UDP 4011、UDP 69、TCP 8080(默认,可改) | 都要可绑 |
| 权限 | **root**(绑特权端口 + 写 master.key) | 当前未支持 capability-only 模式 |
| 内核 | 任何主线 5.x+ | 不需要特殊模块 |
| systemd | 已是默认 init | service unit 走 systemd |
| 防火墙 | 67/69/4011/8080 在被装段方向放开 | firewalld/ufw 注意,WSL2 默认无 |

**WSL2 限制**:WSL2 默认 NAT 模式不能桥接到物理 LAN,**不能跑真实 PXE 流量**;只能
做开发自测或在 mirrored networking 模式下结合一台真实 LAN 主机做转发。生产部署一律
用真实 Linux 物理机或 VM。

---

## 3. 控制节点:运行时依赖(外部命令)

`metalkit-controller` 自己是静态二进制,但**装机流程会调一组外部命令**。`scripts/install.sh`
会按 OS family 自动装齐。手工部署照下表对应。

### 3.1 必装(没有它装机会 fail)

| 命令 | 用在哪 | Debian 包 | RHEL 包 |
|---|---|---|---|
| `ipmitool` | BMC 上电/选 PXE/重启(`internal/ipmi/`) | `ipmitool` | `ipmitool` (EPEL) |
| `qemu-img` | 镜像上传时 lint + qcow2→raw 转换 | `qemu-utils` | `qemu-img` |
| `mkpasswd` | UI 帮 op 生成 root SHA-512 hash(`/api/v1/util/crypt-sha512`) | `whois` | `whois`(RHEL 9)或缺(RHEL 7 minimal,需 EPEL 或自带) |
| `cloud-localds` | cloud-init seed ISO 生成(NoCloud) | `cloud-image-utils` | `genisoimage` + 内置(等价路径) |

> `mkpasswd` 在 RHEL 7 上偶尔缺,doctor 会 WARN。装不上不致命:用户可以在别处生成
> hash 贴进 UI。但 cloud-localds 缺失会让所有装机任务在 seed 阶段失败,务必装上。

### 3.2 install.sh 实际执行的命令

```bash
# debian / ubuntu
apt-get install -y --no-install-recommends \
    ipmitool whois qemu-utils cloud-image-utils ca-certificates curl

# rhel family
dnf install -y epel-release || true        # ipmitool 在 EPEL
dnf install -y ipmitool qemu-img genisoimage ca-certificates curl
dnf install -y whois || true               # 可能缺,继续
```

`--skip-deps` 跳过自动装依赖(离线 / 自管理仓库时用),依赖必须靠别的方式补上。

---

## 4. 部署执行

### 4.1 一键安装(99% 场景用这条)

```bash
# 4.1.1 在构建机上同步代码(整个仓库或 §1.4 列出的最小子集)
rsync -av --exclude=.git --exclude=node_modules \
    /opt/claude/devops/ root@<控制节点IP>:/root/metalkit-src/

# 4.1.2 在控制节点 ssh 进去
ssh root@<控制节点IP>
cd /root/metalkit-src

# 4.1.3 交互式部署
sudo ./scripts/install.sh

# 或非交互(CI / 批量):
sudo MK_INTERFACE=eno1 \
     MK_SERVER_IP=10.0.0.5 \
     MK_ADMIN_PASS='ChangeMe!' \
     MK_ADMIN_USER=admin \
     MK_HTTP_ADDR=':8080' \
     ./scripts/install.sh --yes
```

`scripts/install.sh` 的可选 flag:

| flag | 用途 |
|---|---|
| `-y, --yes` | 不弹 prompt,完全靠环境变量(必须先 export 全) |
| `--skip-deps` | 跳过 apt/dnf install(离线 / 自己装好依赖) |
| `--skip-doctor` | 跳过末尾的 preflight(强烈不建议,只在 hot-fix 紧急部署时用) |

### 4.2 安装脚本做的 8 步

| 步骤 | 动作 | 落点 |
|---|---|---|
| 1 | 校验 repo layout:`bin/{controller,agent}` 和 `boot/{vmlinuz,initrd.img,filesystem.squashfs}` 都在 | / |
| 2 | 探测 OS family(`/etc/os-release` 的 `ID` / `ID_LIKE`),装 §3 依赖 | / |
| 3 | 创建 `/opt/metalkit/{bin,boot}` (0755), `/etc/metalkit` (0755), `/var/lib/metalkit/{,images}` (0750) | 三个根目录 |
| 4 | 复制 controller → `/opt/metalkit/bin/metalkit-controller` (0755) | / |
|   | 复制 agent → `/opt/metalkit/bin/metalkit-agent` (0755,**只是冷备**,生产中没有进程跑它) | / |
| 5 | 复制 `vmlinuz / initrd.img / filesystem.squashfs` → `/opt/metalkit/boot/` (0644) | / |
| 6 | 生成 `/etc/metalkit/config.yaml` (0640,**已存在则保留**),空 admin pass 时 WARN | / |
| 7 | 写 `/etc/systemd/system/metalkit-controller.service`,`systemctl daemon-reload && enable` | / |
| 8 | 跑 `/opt/metalkit/bin/metalkit-controller doctor -config /etc/metalkit/config.yaml`,通过则 `restart` | / |

完成后:

```
[ok] metalkit controller installed and running.
  Config:    /etc/metalkit/config.yaml
  Data:      /var/lib/metalkit
  Logs:      journalctl -u metalkit-controller -f
  Status:    systemctl status metalkit-controller
  Doctor:    /opt/metalkit/bin/metalkit-controller doctor -config /etc/metalkit/config.yaml
  Web UI:    http://10.0.0.5:8080/ui/
```

### 4.3 手工部署(脚本搞不定的场景)

参考 §4.2 八步逐项执行。systemd unit 完整内容:

```ini
[Unit]
Description=metalkit bare-metal provisioning controller
Documentation=https://github.com/metalkit/metalkit
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStartPre=/opt/metalkit/bin/metalkit-controller doctor -config /etc/metalkit/config.yaml
ExecStart=/opt/metalkit/bin/metalkit-controller -config /etc/metalkit/config.yaml
User=root
Group=root
WorkingDirectory=/opt/metalkit
Restart=on-failure
RestartSec=5s
StandardOutput=journal
StandardError=journal
# 轻度 hardening,兼容绑 UDP 67/69 + 写 master.key
NoNewPrivileges=true
ProtectSystem=full
ProtectHome=true
PrivateTmp=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
ReadWritePaths=/var/lib/metalkit /opt/metalkit/boot

[Install]
WantedBy=multi-user.target
```

`ExecStartPre=… doctor` 让坏配置在拉起前就 fail(端口被占、网卡没绑、boot 目录缺文件、
依赖 binary 缺),`systemctl status` / journal 一眼能看到 `[FAIL]` 行。

`ProtectSystem=full` + `ReadWritePaths=/var/lib/metalkit /opt/metalkit/boot` 让二进制
即便被攻破也写不到其他系统目录。当前未启用 `User=metalkit`(non-root),因为还要绑
特权端口和读 0600 的 master.key;后续会做。

---

## 5. 配置文件 `/etc/metalkit/config.yaml`

```yaml
# ─── 必填:网络绑定 ──────────────────────────────────────────────
serverIP: 10.0.0.5         # 控制节点真实 IPv4(被装机也用这个 IP 回 HTTP / TFTP)
                           # 必须是 interface 上真实绑定的地址
                           # 不能写 0.0.0.0 / 域名 / CNAME(live image 里 busybox-wget 无 DNS)
interface: eno1            # 控制节点出口网卡名;用于 DHCP server-identifier
                           # 写错的话客户端忽略 OFFER

# ─── 监听端口 ──────────────────────────────────────────────────
httpAddr: ":8080"          # HTTP: iPXE 脚本 + kernel + initrd + squashfs + API + Web UI + agent 回传
dhcpAddr: ":67"            # ProxyDHCP OFFER(注意不发 IP)
bsdpAddr: ":4011"          # BSDP Layer 3:Dell BIOS 拿 bootfile 路径
tftpAddr: ":69"            # TFTP:发 iPXE 二进制(snponly.efi 等),从 controller embed 直出

# ─── 文件系统路径 ──────────────────────────────────────────────
bootDir: /opt/metalkit/boot                  # vmlinuz / initrd.img / filesystem.squashfs 所在
dbPath: /var/lib/metalkit/inventory.db        # 单文件 SQLite,所有 store 共用
imagesDir: /var/lib/metalkit/images           # 上传 OS 镜像最终落盘(含 .tmp/ 中转切片)
masterKeyPath: /var/lib/metalkit/master.key   # AES-256 密钥,加密 BMC 密码;首次启动自动生成 0600
logLevel: info                                 # debug / info / warn / error

# ─── 认证 ──────────────────────────────────────────────────────
adminUser: admin                              # Web UI + 读 API 的 Basic Auth 用户名
adminPass: "ChangeMe!"                        # 留空 = open mode,启动 WARN,UI 直接放行;仅限隔离环境
                                              # Agent POST 端点(/api/v1/report、heartbeat、jobs)永远 open
                                              # —— agent 没办法存凭据

# ─── 可选:profile 创建期 root 密码默认值 ──────────────────────
defaultRootPassword: metalkit                 # 留空走 "metalkit";启动时 hash 一次缓存在内存
```

**关键校验**(全部在 controller 启动期 + doctor 子命令里跑):
- `serverIP` 必须是 `interface` 上**真实绑定**的 IPv4。controller 启动时校验,错就退出。
- `bootDir` 三件套必须齐。少一个 PXE 会 `TFTP timeout` 或 `iPXE 404`。
- `masterKeyPath` 必须 0600(若已存在)。世界可读的密钥 = 加密失效。
- `adminPass` 留空启动会打 WARN,UI / 读 API 直接放行。

---

## 6. DHCP 拓扑:必选一种

metalkit 走 **ProxyDHCP**:在 67 端口同段广播极简 OFFER(`vendor-class-id` + `server-identifier`),
**不发 IP**;BIOS PXE 收到后单播到 4011 拿 bootfile(Dell BSDP)。

被装机器**必须从某处拿到 IP**。三种拓扑选一:

### 6.1 拓扑 A:段里已有 IP-only DHCP(推荐)

零额外配置。metalkit 和上游 DHCP 各回各的 OFFER,BIOS 用上游的 IP + metalkit 的 PXE 信息。

> **必须确认**:上游 DHCP 不能配 `option 66`/`option 67`/`PXE Class`,否则 BIOS 会
> 优先用上游(拉到错文件)。如果上游运维定死,把 PXE 部分删掉,留给 metalkit。

### 6.2 拓扑 B:段里没 DHCP,起一个 IP-only dnsmasq sidecar

`/etc/dnsmasq.d/iponly.conf`:

```
interface=eno1                  # 改成你的网卡
bind-dynamic                    # 关键:允许 metalkit 同时绑 :67
port=0                          # 关 DNS
dhcp-range=10.0.0.150,10.0.0.180,12h
dhcp-option=option:router,10.0.0.1
dhcp-option=option:dns-server,1.1.1.1
log-dhcp
# 关键禁忌:不发任何 PXE option(66/67/43),那是 metalkit 的活
```

启动:

```bash
systemctl restart dnsmasq
# 或临时跑:systemd-run --unit=pxe-iponly --collect /usr/sbin/dnsmasq -d -C /etc/dnsmasq.d/iponly.conf
```

**67 端口共存原理**:metalkit 用 `SO_REUSEADDR`,dnsmasq 用 `bind-dynamic`,两个进程
都收同一份 broadcast 各回各包。`ss -ulnp | grep :67` 应该看到两行。doctor 对 `:67`
"address in use" 给的是 WARN(不是 FAIL),专门为这个共存场景留的。

### 6.3 拓扑 C:让上游 ISC dhcpd 转发到 metalkit

ISC dhcpd `dhcpd.conf` 加:

```
class "metalkit-pxe" {
    match if substring(option vendor-class-identifier, 0, 9) = "PXEClient";
    next-server 10.0.0.5;       # = serverIP
    filename    "snponly.efi";  # controller 内置,不需要单独放 TFTP
}
```

注意:`snponly.efi` 是 controller `//go:embed` 出来的,controller 自带 TFTP 服务
直接发,**不需要外部 TFTP 目录**。

---

## 7. 验证(doctor + 手工)

### 7.1 doctor 完整通过

```bash
/opt/metalkit/bin/metalkit-controller doctor -config /etc/metalkit/config.yaml
```

期望(每行 `[ PASS | WARN | FAIL ]  描述`):

```
[ PASS ]  config load                — /etc/metalkit/config.yaml
[ PASS ]  network interface          — eno1 has 10.0.0.5
[ PASS ]  DHCP/BSDP port 67          — bindable on :67
[ PASS ]  BSDP port 4011             — bindable on :4011
[ PASS ]  TFTP port 69               — bindable on :69
[ PASS ]  HTTP :8080                 — bindable on :8080
[ PASS ]  boot artifacts             — /opt/metalkit/boot has vmlinuz, initrd.img, filesystem.squashfs
[ PASS ]  db path                    — /var/lib/metalkit/inventory.db writable
[ PASS ]  images dir                 — /var/lib/metalkit/images writable
[ PASS ]  master key                 — /var/lib/metalkit/master.key mode 0600
                                       (首次启动前是 WARN: will be generated on first start)
[ PASS ]  admin auth                 — user=admin, password set
[ PASS ]  tool: ipmitool             — available (BMC power / boot-device control)
[ PASS ]  tool: mkpasswd             — available (POST /api/v1/util/crypt-sha512)
[ PASS ]  tool: qemu-img             — available (image lint / format detection on upload)
```

doctor 退出码:全 PASS/WARN → 0;任何 FAIL → 1。

`WARN tool ipmitool`:在测试环境无 BMC 时可忽略,装机时会用别的方式触发(手 PXE)。
`WARN master key`:首次启动会自动生成,正常。
`FAIL port :67 address already in use`:dnsmasq 没用 `bind-dynamic`,或 controller 已在跑。

### 7.2 端口都被 controller 拿着了

```bash
ss -ulnp | grep -E ':(67|69|4011) '
ss -tlnp | grep ':8080 '
```

期望四行 PID 都是 `metalkit-controller`(端口 67 可能多一行 dnsmasq —— OK)。

### 7.3 Web UI

浏览器开 `http://<serverIP>:8080/ui/`,弹 Basic Auth 输 `adminUser` / `adminPass`。
看到 Machines / Images / Subnets / Profiles / Bindings / Jobs 六个 tab 都能正常打开。

401 但密码对:`adminPass` 含 YAML 特殊字符(`!` `&` `*` `#`)没引号包起来。

### 7.4 PXE 烟雾测试

任何同段机器开机选 PXE,看 controller 日志:

```bash
journalctl -u metalkit-controller -f --output=cat | grep -E '(stage=|bsdp|tftp|http)'
```

期望 60–90 秒内出现:

```
stage=discover hwaddr=…
stage=bsdp     hwaddr=…  → snponly.efi
stage=tftp     file=snponly.efi  ip=…
stage=ipxe     hwaddr=…  → http://<serverIP>:8080/boot/ipxe
stage=http     file=vmlinuz
stage=http     file=initrd.img
stage=http     file=filesystem.squashfs   bytes=580+ MiB
```

约 30 秒后机器进 live shell,agent 上报,`/api/v1/machines` 出现一条 `status=online`,
UI 同步刷新。

### 7.5 装机完整冒烟(走完 §8 的数据模型 + UI 操作)

UI 流程:
1. **Images** 上传 Ubuntu 24.04 cloud qcow2(或 Rocky 9)。
2. **Subnets** 录被装段:CIDR / gateway / DNS。
3. **Profiles** 建套餐:选 image + subnet 模板 + root 密码留空(走 `defaultRootPassword`)。
4. **Machines** 详情 → 录 BMC IP / user / pass(IPMI interface 默认 `lanplus`)。
5. **Machines** 详情 → 「装机」 → 选 profile + subnet → 提交。
6. **Jobs** 看状态:`pending → preparing → installing → finalizing → done`。
7. 完成后机器 power cycle 进新 OS,可 ping 可 SSH。

Ubuntu 24.04 装 16 GB 系统盘约 3–5 分钟。

---

## 8. 数据模型速查

| 表 | 内容 | 关键字段 |
|---|---|---|
| `machines` | agent 上报的机器及硬件信息 | uuid, serial, last_seen, status |
| `bmcs` | BMC 凭据(密码用 master.key 加密) | machine_uuid, ip, user, password_encrypted |
| `images` | 上传的 OS 镜像(内容寻址) | sha256, family, format, virtual_size |
| `subnets` | 装完后机器要落到哪个网段 | cidr, gateway, dns, vlan_id |
| `profiles` | 镜像 + 密码 hash + cloud-init 模板的可复用安装套餐 | image_id, root_password_hash, user_data_template |
| `bindings` | machine ↔ profile ↔ subnet 三方绑定 + 单机覆盖项 | machine_uuid, profile_id, subnet_id, ip_override, vlan_override |
| `jobs` | 装机执行任务,带状态机和日志流 | machine_uuid, kind, state, log_offset |

并发模型:`jobs` 表上的 UNIQUE partial index `idx_jobs_one_inflight_per_machine`
`ON jobs(machine_uuid) WHERE status IN ('pending','running')` 是**每台机器只能有一个 job 在飞**
的硬约束;跨机器无应用层限制,瓶颈是控制节点出口带宽 × N agents × ~GB 镜像。

详细 API 见 `docs/api.md`。

---

## 9. 升级 / 维护

### 9.1 只升 controller(不动 live image)

```bash
# 构建机
make build
scp bin/controller root@<节点>:/tmp/controller.new

# 控制节点
install -m 0755 /tmp/controller.new /opt/metalkit/bin/metalkit-controller
systemctl restart metalkit-controller
journalctl -u metalkit-controller -n 30 --no-pager
```

### 9.2 升 agent(必须重打 squashfs)

agent 是 baked 进 squashfs 的(`/usr/local/bin/metalkit-agent`):

```bash
# 构建机
make agent live
scp boot/filesystem.squashfs root@<节点>:/tmp/squashfs.new

# 控制节点
cd /opt/metalkit/boot
mv filesystem.squashfs filesystem.squashfs.bak.$(date +%Y%m%d)
mv /tmp/squashfs.new filesystem.squashfs
# 不需要重启 controller — squashfs 按需读,下次 PXE 用新的
```

### 9.3 fast repack(只换 agent,不跑 lb build)

只是 agent 改了几行,跳过 live-build:

```bash
# 控制节点
cd /tmp
unsquashfs -d sq /opt/metalkit/boot/filesystem.squashfs
cp /tmp/agent.new sq/usr/local/bin/metalkit-agent
mksquashfs sq filesystem.squashfs.new -comp xz -noappend
mv filesystem.squashfs.new /opt/metalkit/boot/filesystem.squashfs
```

约 2–3 分钟,比 `make live` 快 5–10 倍。

### 9.4 备份 / 还原

唯一需要备份的是 `/var/lib/metalkit/`:

```bash
systemctl stop metalkit-controller
tar czf metalkit-state-$(date +%F).tar.gz -C /var/lib metalkit
systemctl start metalkit-controller
# 包含 inventory.db、master.key、images/*
```

> **必须备份 `master.key`**:BMC 密码用它加密,丢了 = 库里所有 BMC 凭据作废需重录。
> 建议至少一份离机副本(异地 / 加密对象存储)。

还原到新机:解压回 `/var/lib/metalkit/`,**确认 `master.key` 0600 root:root**。

### 9.5 卸载

```bash
systemctl stop metalkit-controller
systemctl disable metalkit-controller
rm /etc/systemd/system/metalkit-controller.service
rm -rf /opt/metalkit
rm /etc/metalkit/config.yaml; rmdir /etc/metalkit
# /var/lib/metalkit 保留(含历史数据 + master.key);确认要丢再 rm -rf
systemctl daemon-reload
```

---

## 10. 故障排查速查

| 症状 | 看哪 / 怎么修 |
|---|---|
| `systemctl restart metalkit-controller` 立刻 fail | `journalctl -u metalkit-controller -n 80`;多半 doctor 报错,照 `[FAIL]` 行修 |
| `port 67/69/4011 address in use` 启动失败 | 上游 dnsmasq 没用 `bind-dynamic`;或 ISC dhcpd 已在跑;或上次 controller 没退干净 |
| 机器 PXE `PXE-E16: No offer received` | 段里没 DHCP 给 IP;起 IP-only dnsmasq sidecar(§6.2) |
| 机器拿到 IP 但 `PXE-E07` / 反复 DHCPDECLINE | 上游 DHCP 在 OFFER 里塞了 PXE bootfile;删它,留 metalkit 发 |
| Dell `chassis bootdev pxe` 后下次开机仍进本地盘 | 缺 `options=efiboot`;已修(`internal/ipmi/ipmi.go:115`),确认主线版本 |
| Ubuntu 24.04 装完进 grub rescue | `/boot` 没当独立分区挂上;已修(mount.go fstab 驱动),确认主线 |
| live shell 里 vi 方向键变 `ABCD` | hook 已修 `vimrc.tiny` 的 `compatible`;不生效就重打镜像 |
| BMC 控制台无 prompt 卡黑 | iDRAC vKVM 的 Scroll Lock 问题;`metalkit-vt-init.service` 已修;老镜像需重打 |
| Web UI 401 但密码对 | `adminUser`/`adminPass` 含 YAML 特殊字符;用引号包起来 |
| `/api/v1/machines` 永远空 | 看 agent POST `/report` 是不是 200;早期 live image 的 URL 解析 bug,重打镜像修 |
| 装机 job 卡在 `installing` 不动 | `journalctl -u metalkit-controller -f \| grep job_id=…`;多半镜像 sha 不匹配 / 目标盘识别错 |
| doctor `tool: mkpasswd` WARN | RHEL 7 minimal 缺;装 `whois` 包或在别处生成 hash 贴进 UI |
| doctor `tool: ipmitool` WARN | EPEL 仓库没启:`dnf install epel-release && dnf install ipmitool` |

抓包诊断(对照 controller 日志):

```bash
tcpdump -i eno1 -nn -vvv 'udp port 67 or udp port 4011 or udp port 69' -w pxe.pcap
```

跟 controller 日志按时间对比。详细 PXE 时序分析见 `docs/build-and-deploy.md` §6。

---

## 11. 多控制节点 / 多段

当前是**单节点单段**设计:

- 控制节点和被装机必须同二层(DHCP 是广播)。
- 一个 metalkit 实例只服务一个段(`interface` + `serverIP` 是 1 对 1)。

跨段方案:

| 方案 | 做法 | 状态 |
|---|---|---|
| **A. 每段一个控制节点** | 每段独立 sqlite + master.key,互不感知 | **已支持**,简单,但机器列表分裂 |
| **B. DHCP relay 转发** | 每段核心交换机 `ip helper-address <控制节点>`,把 67/4011 转发到中心节点 | 代码层支持 `giaddr` 非零,**真机未跑过** |

短期推荐 A,生产场景把每段的实例数据周期性 export 汇总到运营报表。B 等到真机验证再开。

---

## 12. 离线 / 内网部署

构建机能联网(拉 Go modules + apt + boot.ipxe.org + debian apt),控制节点完全离线也能跑。

操作:

1. **构建机一次性打包**:
   ```bash
   tar czf metalkit-bundle.tgz \
       bin/controller bin/agent \
       boot/vmlinuz boot/initrd.img boot/filesystem.squashfs \
       scripts/install.sh config.example.yaml docs/
   ```

2. **拷到控制节点**,`tar xf` 后:
   ```bash
   sudo ./scripts/install.sh --skip-deps
   ```

3. **运行时依赖**(`ipmitool` / `qemu-img` / `mkpasswd` / `cloud-localds`)需要从离线
   yum/apt 仓库装,否则 doctor 会 WARN,装机会在调外部命令时 fail。

   常见做法:
   - **Debian/Ubuntu**:把构建机上 `/var/cache/apt/archives/*.deb` 打包到控制节点,
     `dpkg -i *.deb` 或 `apt install ./pkg.deb`。
   - **RHEL**:`dnf install --downloadonly --downloaddir=/tmp/rpms ...` 拉包,拷过去
     `rpm -Uvh *.rpm`。

4. **OS 镜像**走 UI 上传 → 切片 `imagesDir/.tmp/` → 拼接 → 落 `imagesDir/`,
   全程不出节点。镜像源可以是离线 mirror,也可以是 op 上传的本地文件。

---

## 13. 安全收尾清单(生产前必做)

- [ ] `adminPass` 改强口令,不留 `ChangeMe!` 或空
- [ ] `defaultRootPassword` 改强口令,或保留默认但每个 profile 都显式设密码 / SSH key
- [ ] live image hook (`0500-metalkit-ssh.hook.chroot`) 里 root/user 密码改掉,或改成纯 SSH key 认证
- [ ] **重打 squashfs** 并部署上去(改 hook 不重打没用)
- [ ] BMC 默认密码全部改掉(Dell 出厂 calvin,Supermicro ADMIN,HPE password)
- [ ] 控制节点上 firewalld/ufw 把 8080 限给运维段,67/69/4011 限给被装段
- [ ] `/var/lib/metalkit/master.key` 确认 `0600 root:root`
- [ ] 备份策略上线,`master.key` 至少一份**离机**副本(加密对象存储 / U 盘冷备)
- [ ] 升级流程演练过一次:restart controller、重打 squashfs、还原备份
- [ ] systemd unit 的 `ProtectSystem=full` + `ReadWritePaths` 没被改弱
- [ ] 日志收集对接(journal → 集中日志);`logLevel: info` 在生产、`debug` 仅排障

---

## 14. 附录:文件落点全景

部署完成后,控制节点上有:

```
/opt/metalkit/
├── bin/
│   ├── metalkit-controller       # 15 MB,静态,root:root 0755
│   └── metalkit-agent            # 6.5 MB,冷备(实际跑在 live image 里)
└── boot/
    ├── vmlinuz                   # 8 MB,Debian bookworm 内核
    ├── initrd.img                # 62 MB,live-boot initramfs
    └── filesystem.squashfs       # 580 MB,Debian + agent + 装机工具集

/etc/metalkit/
└── config.yaml                   # 0640,生成后人工调整

/etc/systemd/system/
└── metalkit-controller.service   # 0644

/var/lib/metalkit/                # 0750 root:root
├── inventory.db                  # 单文件 SQLite(machines/bmcs/images/profiles/...)
├── inventory.db-wal              # WAL 模式
├── inventory.db-shm
├── images/                       # 0755
│   ├── .tmp/                     # 上传切片中转
│   ├── <sha256>.qcow2           # 内容寻址
│   └── <sha256>.raw             # 转码后副本(可选)
└── master.key                    # 0600,AES-256 密钥,加密 BMC 密码
```

---

## 15. 附录:每个端口的实际行为

| 端口 | 协议 | 用途 | 谁发起 |
|---|---|---|---|
| 67 | UDP | DHCP DISCOVER → ProxyDHCP OFFER(不带 IP) | 被装机 PXE 阶段广播 |
| 4011 | UDP | BSDP REQUEST → ACK 带 bootfile 路径 | Dell BIOS 单播 |
| 69 | UDP | TFTP GET `snponly.efi` / `undionly.kpxe` | BIOS / iPXE 单播 |
| 8080 | TCP | HTTP `/boot/ipxe`(脚本) / `/boot/vmlinuz` / `/boot/initrd.img` / `/boot/filesystem.squashfs` | iPXE + live image |
| 8080 | TCP | HTTP `/api/v1/...` 控制面 + `/ui/` Web | op + agent |
| 8080 | TCP | HTTP `/api/v1/report`(agent 上报)+ `/agent/jobs/...`(agent 拉 spec) | live image 里的 agent |

被装机方向只需要 67/4011/69/8080,运维方向只需要 8080(可加 22 给 ssh)。
