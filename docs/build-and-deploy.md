# metalkit M1 端到端制作 & 部署手册

本文记录从源码到 Dell PowerEdge 真机 PXE 启动至 Debian live shell 的**完整制作流程**，包含
所有真机调试中踩到的坑和固化进 hook 的修复。

适用场景：M1 阶段在隔离测试网段（示例：`192.168.10.0/24`，PXE 控制节点 `192.168.10.147`）
里跑端到端 PXE → live shell 验证。

---

## 1. 组件总览

| 组件 | 作用 |
|---|---|
| `bin/controller` | metalkit 主程序，单进程内置 ProxyDHCP (67) / BSDP (4011) / TFTP (69) / HTTP (8080)。 |
| `boot/vmlinuz` | live 系统内核（live-build 产出）。 |
| `boot/initrd.img` | live 系统 initramfs，带 live-boot 模块。 |
| `boot/filesystem.squashfs` | Debian bookworm 只读根文件系统镜像（~586 MB）。 |
| `boot/undionly.kpxe` / `snponly.efi` | iPXE 第一阶段，由 BIOS/UEFI 通过 TFTP 拉取。 |
| 外部 DHCP（如 dnsmasq） | 给客户端发 IP；metalkit 自己**不**派 IP（proxyDHCP 模式）。 |

启动链：

```
Dell BIOS PXE
   │ Discover (UDP 67 广播)
   ▼
┌─ dnsmasq ────────────── DHCPOFFER yiaddr=192.168.10.X  ┐
│                                                        │
└─ metalkit DHCP ──────── 极简 OFFER（仅 Class-ID）       ┘
                              ↑ 两个 OFFER 客户端都收
   │
   ▼ Dell 选 dnsmasq 的 ACK 拿到 IP
   │
   ▼ Dell 单播 → 192.168.10.147:4011 (BSDP)
   │
   ▼ metalkit 回 ACK(siaddr=192.168.10.147, file=snponly.efi)
   │
   ▼ Dell TFTP 拉 snponly.efi (288 KB)
   │
   ▼ iPXE 启动，第二次 DHCP（option 77=iPXE）
   │
   ▼ metalkit 在 67 上回 filename=http://192.168.10.147:8080/boot/ipxe
   │
   ▼ iPXE HTTP 拉 vmlinuz / initrd
   │
   ▼ kernel 启动，live-boot fetch= 拉 filesystem.squashfs 入内存
   │
   ▼ Debian login 提示符（BMC 控制台 + SSH 同时可用）
```

---

## 2. 构建主机要求

构建主机（本机示例：WSL2 Ubuntu）：

- Go ≥ 1.23（旧版本可用 `GOTOOLCHAIN=auto` 自动拉取）
- Docker（用于在 `debian:bookworm` 容器里跑 live-build）
- `curl`、`make`、`unsquashfs`（属于 `squashfs-tools` 包，用于验证镜像）
- 磁盘：构建过程下载 ~600 MB apt 包 + 中间产物，预留 3 GB

目标 PXE 服务器（本机示例：`192.168.10.147`，Ubuntu 22.04）：

- 一张接到测试 LAN 的网卡（示例：`ens32`，绑定 `192.168.10.147/24`）
- 可绑定 UDP 67/69/4011 和 TCP 8080（需 root 或 `CAP_NET_BIND_SERVICE`）
- `dnsmasq` 包已装（用于 IP-only sidecar）

---

## 3. 构建步骤

```bash
cd /opt/claude/devops

# 1) 拉取 iPXE 第一阶段二进制（undionly.kpxe / snponly.efi / ipxe.efi）
make ipxe

# 2) 构建 controller 二进制（约 8 MB，静态、纯 Go）
make build

# 3) 构建 Debian live 镜像（10–25 分钟，需要 docker 和网络）
make live          # 实际调用 scripts/build-live.sh
```

产物：

```
bin/controller                    ← 主程序
boot/vmlinuz                      ← live 内核
boot/initrd.img                   ← live initramfs
boot/filesystem.squashfs          ← live 根文件系统
internal/ipxebin/assets/*.{efi,kpxe}  ← embedded
```

### 3.1 live 镜像里固化了哪些定制

`live-image/config/hooks/normal/0500-metalkit-ssh.hook.chroot` 在 chroot 内执行，固化以下内容：

| 项 | 原因 |
|---|---|
| `chpasswd` 设 `root` 密码为 `metalkit` | 允许 SSH 和控制台 root 登录（M1 测试用，生产前替换）。 |
| 建 `user` 账户（uid 1000，密码 `metalkit`） | live-config 的 tty1 drop-in 写死 `--autologin user`，但 user-setup 组件在精简镜像里没建这个账户。不建会导致 `getty@tty1` 五次 PAM 失败后被 systemd 标 start-limit-hit，BMC 控制台永远看不到 prompt。 |
| `/etc/ssh/sshd_config.d/10-metalkit.conf` | 开 `PermitRootLogin yes` + `PasswordAuthentication yes`。 |
| `systemctl enable ssh` | 默认 sshd 是禁用的，要显式启用。 |
| `/etc/systemd/system/getty@tty1.service.d/zz-metalkit-no-autologin.conf` | 覆盖 live-config 在 `/run/systemd/generator/` 写的 autologin drop-in。文件名 `zz-` 让我们的排在最后，`ExecStart=` 清空 + 重设为不带 `--autologin` 的 agetty。这样 BMC 控制台显示正常 login prompt 而不是直接拿到 shell。 |
| 修正 `/etc/vim/vimrc.tiny` | 把 `set compatible` 改成 `set nocompatible`，并加 `set backspace=2`。否则 BMC 控制台进 vi 时方向键变 ABCD、Backspace 不工作。 |
| `metalkit-vt-init.service` + 装 `kbd` 包（见 `live-image/config/hooks/normal/0700-metalkit-vt-init.hook.chroot`） | iDRAC vKVM 的 Avocent USB HID 会发 Scroll Lock 键码。Linux VT 接到 `Scroll_Lock` keysym 会暂停 tty1 输出（看起来像控制台卡死，物理键盘敲了没回显，虚拟键盘正常）。服务在 getty 之前 `loadkeys` 把 keycode 70 重映为 `VoidSymbol`，并 `setleds -D -scroll` 把所有 VT 的 lockstate 清掉。 |

包列表：`live-image/config/package-lists/installer.list.chroot`

```
# 启动 / 核心
live-boot live-config live-config-systemd
linux-image-amd64 systemd-sysv
firmware-linux firmware-linux-nonfree

# 基础工具 / 网络
iproute2 iputils-ping curl ca-certificates
pciutils dmidecode bash less vim-tiny
openssh-server net-tools lsof strace htop

# 装机工具集（M2 准备）
qemu-utils         # qemu-img：raw/qcow2/vmdk 互转
parted gdisk       # MBR/GPT 分区
dosfstools         # mkfs.vfat（UEFI ESP）
e2fsprogs xfsprogs btrfs-progs   # 各种文件系统
lvm2 mdadm cryptsetup-bin        # LVM / RAID / LUKS
nvme-cli hdparm smartmontools    # 盘控制和健康
rsync wget xz-utils zstd pv      # 传输 / 压缩
ipmitool                         # 装完通过 IPMI 切回本盘启动
```

### 3.2 验证 squashfs 内容（不用启动）

```bash
# 看 hook 写进去的关键文件都在
unsquashfs -ll boot/filesystem.squashfs | grep -E \
  '(/etc/passwd$|/etc/shadow$|zz-metalkit|10-metalkit\.conf$|/home/user)'

# 看 vimrc.tiny 已被修
unsquashfs -cat boot/filesystem.squashfs etc/vim/vimrc.tiny | grep -E '^set'
# 期望输出：
#   set nocompatible
#   set backspace=2
```

---

## 4. 部署到 PXE 控制节点（示例 `192.168.10.147`）

### 4.1 推送二进制和镜像

```bash
DEST=root@192.168.10.147

ssh $DEST 'mkdir -p /opt/metalkit/boot'
scp bin/controller            $DEST:/opt/metalkit/controller
scp config.host147.yaml       $DEST:/opt/metalkit/config.yaml
scp boot/vmlinuz              $DEST:/opt/metalkit/boot/
scp boot/initrd.img           $DEST:/opt/metalkit/boot/
scp boot/filesystem.squashfs  $DEST:/opt/metalkit/boot/
# iPXE 第一阶段（snponly.efi/undionly.kpxe）已 embed 进 controller，不需要单独推
```

`config.host147.yaml` 示例（重点：`serverIP` 必须是真实可达 IPv4）：

```yaml
serverIP: 192.168.10.147
interface: ens32
httpAddr: ":8080"
dhcpAddr: ":67"
bsdpAddr: ":4011"
tftpAddr: ":69"
bootDir: /opt/metalkit/boot
logLevel: info
```

### 4.2 安装 systemd unit

`/etc/systemd/system/metalkit.service`：

```ini
[Unit]
Description=metalkit PXE controller
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/opt/metalkit/controller -config /opt/metalkit/config.yaml
Restart=on-failure
RestartSec=2
AmbientCapabilities=CAP_NET_BIND_SERVICE CAP_NET_RAW
CapabilityBoundingSet=CAP_NET_BIND_SERVICE CAP_NET_RAW
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
```

启动：

```bash
ssh $DEST 'systemctl daemon-reload && systemctl enable --now metalkit'
ssh $DEST 'journalctl -u metalkit -f'   # 看启动日志
```

### 4.3 配置 IP-only dnsmasq sidecar（**必需**）

metalkit 在 proxyDHCP 模式下**不发 IP**——只发 PXE 信息。客户端必须从另一台 DHCP 拿 IP。
M1 测试网段没有现成 DHCP，所以在 PXE 控制节点本机起一个**只发 IP、不掺和 PXE** 的 dnsmasq。

`/tmp/dnsmasq-iponly.conf`（关键：`port=0` 关 DNS、不发 PXE option）：

```
interface=ens32
bind-dynamic
port=0
dhcp-range=192.168.10.150,192.168.10.180,12h
dhcp-option=option:router,192.168.10.1
dhcp-option=option:dns-server,192.168.10.1
log-dhcp
```

启动（用 transient unit 方便随时停）：

```bash
ssh $DEST 'systemd-run --unit=pxe-iponly --collect \
  /usr/sbin/dnsmasq -d -C /tmp/dnsmasq-iponly.conf'
```

> **能同时绑 UDP 67 的原因**：metalkit 用 `SO_REUSEADDR`，dnsmasq 用 `bind-dynamic`，
> 两者都收同一个 DHCP Discover 广播，各自独立回包，互不影响。

### 4.4 检查端口

```bash
ssh $DEST 'ss -ulnp | grep -E ":(67|69|4011) "'
# 期望：
#   :4011  controller
#   :67    controller   (interface=ens32)
#   :67    dnsmasq      (interface=ens32)
#   :69    controller
```

---

## 5. 端到端验证

### 5.1 在 Dell 上按 F11 → 选 PXE 启动

预期时间线（共约 60–90 秒）：

| 阶段 | 看到 |
|---|---|
| 0–5s | Dell BIOS POST 完，开始广播 DHCP Discover |
| 5–10s | dnsmasq DHCPOFFER 192.168.10.X / DHCPACK；metalkit 极简 OFFER（无 bootfile） |
| 10–15s | Dell 单播 → 192.168.10.147:4011，metalkit BSDP ACK 带 `snponly.efi` |
| 15–20s | TFTP 拉 `snponly.efi`（288 KB），iPXE 启动 |
| 20–25s | iPXE 第二次 DHCP，option 77=`iPXE`，metalkit 回 HTTP URL |
| 25–30s | HTTP 拉 `/boot/ipxe`、`/boot/vmlinuz`（8 MB）、`/boot/initrd.img`（64 MB） |
| 30–60s | kernel 启动，live-boot 通过 HTTP fetch `filesystem.squashfs`（~603 MB）入内存 |
| 60–90s | systemd 拉起服务，BMC 控制台显示 `debian login:`，SSH 同步可用 |

### 5.2 BMC（iDRAC vKVM）控制台

应看到标准登录提示：

```
debian login: root
Password: metalkit
```

或 `user` / `metalkit`。

试方向键和 Backspace：

```bash
vi /tmp/test   # 方向键应正常移动光标，Backspace 应能删字符
```

### 5.3 SSH 登录（推荐运维路径）

```bash
# 从控制节点（或任何能访问 192.168.10.0/24 的机器）
# Dell 拿到的 IP 看 dnsmasq 日志：
ssh $DEST 'journalctl -u pxe-iponly | grep DHCPACK | tail -3'

# 然后：
sshpass -p 'metalkit' ssh -o StrictHostKeyChecking=no root@<Dell IP>
```

---

## 6. 抓包诊断（出问题时）

```bash
ssh $DEST 'systemd-run --unit=pxe-trace --collect \
  /usr/bin/tcpdump -i ens32 -nn -s 0 -w /tmp/pxe.pcap \
  "udp port 67 or udp port 69 or udp port 4011 or tcp port 8080"'

# Dell F11 → 等到 squashfs 拉完
ssh $DEST 'systemctl stop pxe-trace'
scp $DEST:/tmp/pxe.pcap /tmp/pxe.pcap
wireshark /tmp/pxe.pcap
```

按 stage 过滤 metalkit 日志：

```bash
ssh $DEST 'journalctl -u metalkit -f --output=cat | grep -E "(stage|bsdp|tftp)"'
```

---

## 7. 故障排查手册

| 症状 | 根因 | 解法 |
|---|---|---|
| `PXE-E16: No offer received` | 主 DHCP 没回 IP（segment 上没 DHCP，或被防火墙挡了 67 广播） | 起 dnsmasq IP-only sidecar（见 4.3）。 |
| `PXE-E07: Network device error` + dnsmasq 日志看到 `DHCPDECLINE`，"Lease confirmed isn't the same as that in the offer" | metalkit 和 dnsmasq 都 ACK 了 BIOS 的 DHCPRequest，Server-ID 相同但 yiaddr 不同（dnsmasq=真实 IP，metalkit=0.0.0.0），Dell DECLINE | 已在 `internal/dhcp/server.go` 修复：BIOS 阶段（非 iPXE）的 DHCPRequest 直接丢弃，只回 Discover。iPXE 第二阶段照常 ACK。 |
| Dell 拿到 IP 但报 `PXE-E16` | 端口 67 OFFER 里塞了 bootfile/siaddr/option 66/option 43 | 不要塞。Dell BIOS 走 PXE Layer 3 (BSDP)，端口 67 只发极简广告，bootfile 在 4011 上发。 |
| BMC 控制台看到 kernel dmesg 但没 login prompt，光标在闪 | live-config 自动登录的目标账户 `user` 不存在，`getty@tty1` PAM 失败 5 次后 start-limit-hit | 已在 hook 里 `useradd -m -s /bin/bash user`。 |
| BMC 控制台直接是 shell，跳过登录 | live-config 默认 autologin | 已在 hook 里写 `/etc/systemd/system/getty@tty1.service.d/zz-metalkit-no-autologin.conf` 覆盖，强制显示 login prompt。 |
| BMC 控制台 vi 方向键变 ABCD、Backspace 不工作 | vim-tiny 默认 `set compatible`（严格 vi 兼容模式） | 已在 hook 里 sed 改为 `set nocompatible` + `set backspace=2`。 |
| iDRAC vKVM 看得到 login prompt，但物理键盘敲了没回显（虚拟键盘能输入；按一下虚拟 Scroll Lock 之后物理键盘也突然能用） | Avocent USB HID 把 `Scroll_Lock` keysym 当成 lock 键，VT 收到后暂停 tty1 输出。kernel printk 同时打到 tty0 会放大这个现象 | 已固化两件事：`internal/httpd/ipxe.go` 的 kernel cmdline 去掉 `console=tty0`（printk 只走 ttyS0/SOL）；live image 里 `metalkit-vt-init.service` 启动期 `loadkeys` 把 keycode 70 = `VoidSymbol`、`setleds -D -scroll` 清 lockstate。 |
| iPXE 拉 squashfs 时报 DNS 解析失败 | iPXE 脚本里写了域名 | `serverIP` 必须是 IPv4 字面量。live-boot 的 busybox-wget 不解析 DNS。controller 启动时已强制校验。 |
| iPXE 启动后再次拉 snponly.efi（无限循环） | iPXE 第二阶段 DHCP 返回的 filename 又是 snponly.efi，没切到 HTTP URL | 检查 metalkit 是否识别 option 77=`iPXE`。看日志 `stage=ipxe`。 |
| kernel 启动后无网 | 内核命令行少了 `ip=dhcp` | 看 `internal/httpd/ipxe.go` 模板，确认 `ip=dhcp` 在 kernel 行。 |

---

## 8. 应急回滚

完全停 metalkit，回退到 dnsmasq 一体化 proxyDHCP（M0 风格）：

```bash
ssh $DEST 'systemctl stop metalkit pxe-iponly'
ssh $DEST 'systemd-run --unit=pxe-dnsmasq --collect \
  /usr/sbin/dnsmasq -d -C /tmp/dnsmasq-pxe.conf'
```

`/tmp/dnsmasq-pxe.conf` 是带 PXE option 66/67 + TFTP 的完整 dnsmasq 配置，应作为对照备份保留。

---

## 9. 验证清单（在每次镜像/二进制改动后跑一遍）

- [ ] `make build` 成功，`bin/controller --version` 输出版本
- [ ] `make live` 成功，三件套文件大小合理（vmlinuz ~8 MB / initrd ~64 MB / squashfs ~603 MB）
- [ ] `unsquashfs -ll boot/filesystem.squashfs | grep <关键文件>` 确认 hook 生效
- [ ] `scp` 推送到目标后 `journalctl -u metalkit -f` 启动无 error
- [ ] `ss -ulnp` 确认 67/69/4011 监听 + dnsmasq 共绑 67
- [ ] Dell F11 → 60–90 秒内出 `debian login:`
- [ ] BMC 用 `root/metalkit` 登录，`vi /tmp/x` 方向键和 Backspace 正常
- [ ] SSH `root@<Dell IP>` 可登

---

## 10. 已知遗留 / 后续

- **QEMU 验证未跑通**：QEMU 内置 iPXE 不做 BSDP（Layer 3），可能需要在端口 67 OFFER 里同时塞 bootfile（Layer 1）来兼容。M2 处理。
- **Secure Boot**：iPXE 二进制未签 Microsoft UEFI CA，目标机必须关 Secure Boot。
- **iPXE 二进制无 SHA-256 pin**：`scripts/fetch-ipxe.sh` 打印 hash 但未写死校验，构建非完全可重现。
- **arm64**：bootfile 路径已识别 arch 0x000b，但 `make ipxe` 不下载 arm64 二进制。需要时加 `https://boot.ipxe.org/arm64-efi/snponly.efi`。
- **M1 测试密码（root/metalkit、user/metalkit、root@147 的 `123`）只用于实验室**，生产前必须替换并改为 key-based。

---

## 11. M2.1 — 纳管 + 查阅（新增）

M2.1 在 M1 的引导链基础上加了三件事：
1. live 镜像里跑一个 `metalkit-agent` 采集硬件 + 30s 心跳
2. controller 多了一个 SQLite 库 + `/api/v1/*` REST API 接收上报
3. 一套 Web UI 在 `/ui/` 显示机器列表 + 详情

### 11.1 新增构建产物

```
bin/agent                                            ← live image 里跑的采集器
internal/webui/assets/*                              ← embed 进 controller 的静态资源
/var/lib/metalkit/inventory.db                       ← controller 启动时自动建
```

`make agent` 单独编译 agent（Linux/amd64、静态、~10 MB）。`make live` 已经把
依赖加上，先 `make agent` 再跑 `lb build`，agent 通过 includes.chroot 落在
`/usr/local/bin/metalkit-agent`。

### 11.2 新增配置项

`config.host147.yaml`（或对应环境的 config）需要新增：

```yaml
dbPath: /var/lib/metalkit/inventory.db
adminUser: admin
adminPass: <set me — empty = no auth, controller 启动会 WARN>
```

未设置时 `dbPath` 默认 `/var/lib/metalkit/inventory.db`，`adminUser` 默认 `admin`。

### 11.3 systemd unit 调整

`/etc/systemd/system/metalkit.service` 增加 `StateDirectory`（自动建 0755 owned by root）：

```ini
[Service]
StateDirectory=metalkit
```

如果之前是手动 `mkdir /var/lib/metalkit`，可以省了。

### 11.4 端到端时间线变化

引导链不变，但 live 起来后多两步：

| 阶段 | 看到 |
|---|---|
| 60–90s | systemd 拉起服务，BMC 显示 `debian login:`，SSH 同步可用（同 M1）|
| 90–120s | `metalkit-agent` 启动，跑完 9 个采集器（约 5–20s 视硬件）|
| 120s 起 | controller `/api/v1/machines` 列表里能看到这台机器，30s 心跳维持 online |

### 11.5 验证新增项

```bash
# 在 controller 节点（192.168.10.147）
sshpass -p 123 ssh root@192.168.10.147 \
  'curl -s -u admin:<adminPass> http://localhost:8080/api/v1/machines | jq'

# 期望：每台 PXE 进来的机器一行，包含 UUID/serial/manufacturer/product_name
#       + status="online" + last_seen 是近 30s 内的 unix timestamp

# Web UI（浏览器）：
#   http://192.168.10.147:8080/ui/   → 弹 Basic Auth，输 admin/<adminPass> → 看到机器列表
#   点 Details → 12 个折叠段（machine / firmware / cpu / memory / disks /
#                              nics / pci / accelerators / bmc / sensors /
#                              system / agent）+ 历史 report 侧栏
```

详细 API 参考见 [docs/api.md](api.md)。

### 11.6 排障要点

| 症状 | 根因 | 解法 |
|---|---|---|
| UI 401 但本机 curl 也 401 | `adminPass` 没设，但客户端用 Basic Auth 又传错 | 把 `adminPass` 设上，或留空就别传 `-u` |
| `journalctl -u metalkit-agent` 报 `no metalkit.url=` | kernel cmdline 里没 `metalkit.url=` —— 老 iPXE 模板拉来的，没更新 controller | 重新 `make build` 推 controller，重新 PXE boot |
| `/api/v1/machines` 永远空，但 agent 日志说 POST 200 | 数据库路径权限不对，写在了 root home | 检查 `dbPath` 配置 + `journalctl -u metalkit` 看启动日志的 inventory open 路径 |
| 心跳成功但 status 一直 `unknown` | 数据库刚建出来，`RunOfflineMarker` 还没跑第一轮 | 等 60s 后再看；或重启 controller |
| Web UI 进 detail 页 500 / 卡在 loading | API CORS 或路径不对（不应该出现，UI 走同源） | 浏览器 devtools 看 fetch 报错；通常是 controller 没挂 `/api/v1/*` |
| agent 启动后 SMBIOS UUID 为空、退出 | dmidecode 在虚机里跑不出来 / 缺权限 | 真机用 root 跑（live image 默认 root）；VM 测试用 `-url=` 直接跑 controller，跳过 cmdline 解析 |
| `report.agent.errors` 里有 `lspci: invalid option -- 'J'` / `pci_devices` 数组为空 | `lspci -mmJvk`（JSON 输出）要 pciutils ≥ 3.10.0 （2023-12），Debian Bookworm ship 的是 3.9.0 | 已修：`internal/inventory/collect/pci.go` 改用 `lspci -vmmnnk` 文本格式 + 自定义解析（pciutils 3.1+ 都支持）。 |

### 11.7 M2.1 验证清单（在 M1 清单之外增加）

- [ ] `make agent` 成功，`bin/agent` 是 Linux/amd64 ELF 静态
- [ ] `make test` 全绿（cmd/agent + internal/inventory + internal/httpd + internal/webui）
- [ ] `unsquashfs -ll boot/filesystem.squashfs | grep metalkit-agent` 找得到二进制
- [ ] live 起来后 `systemctl status metalkit-agent` 是 active (running)
- [ ] `/api/v1/machines` 在 PXE boot 2 分钟内能看到这台机器
- [ ] Web UI 12 个段都能展开，没有空白或 JS error
- [ ] 拔网线 90 秒后 status 自动从 `online` 翻 `offline`，恢复网络后下一次心跳又 `online`


## 12. M2.2 — 镜像管理（新增）

M2.2 在 M2.1 之上加了「镜像仓库」：UI 上传 qcow2/raw 镜像，controller 端做内容寻址 +
`qemu-img info` 元数据提取。**不涉及装机**，装机在 M2.3 才接进来。

### 12.1 新增构建产物

```
internal/images/                              ← 镜像 store + chunked-upload + qemu-img
internal/webui/assets/images.{html,js}        ← UI 镜像页（embed 进 controller）
/var/lib/metalkit/inventory.db                ← 多了 images / upload_sessions 两张表
/var/lib/metalkit/images/                     ← 内容寻址的最终镜像
/var/lib/metalkit/images/.tmp/{uid}/          ← chunked-upload 临时目录
```

数据库是同一个 SQLite 文件（`dbPath`），通过新增的 `internal/sqlitedb` 包共享连接，
为 M2.3 的 `bindings` / `jobs` 表（要 FK 到 `machines` 和 `images`）做准备。

### 12.2 新增配置项

```yaml
imagesDir: /var/lib/metalkit/images   # 默认值，可以省略
```

未设置时默认 `/var/lib/metalkit/images`。目录会在启动时 `mkdir -p`。

### 12.3 运行依赖

- **`qemu-img`**（建议）：用于解析上传镜像的 format / virtual_size。
  - Debian/Ubuntu: `apt install qemu-utils`
  - 缺这个不致命，controller 启动会 WARN，镜像元数据会回退到「按文件名后缀猜 format」。

### 12.4 新增 API 端点

全部走 `/api/v1/images*`，**整个子树要求 Basic Auth**（agent 不上传镜像）。

| 方法 路径 | 用途 |
|---|---|
| `POST /api/v1/images/uploads` | 初始化 chunked upload 会话，请求体 JSON `{name, family?, version?, notes?, expected_sha256, total_size, chunk_size?}` |
| `GET /api/v1/images/uploads/{uid}` | 查询会话状态（已上传几片） |
| `PUT /api/v1/images/uploads/{uid}/chunks/{n}` | 上传第 n 片（1-based）。原始字节做 body；`X-Chunk-Sha256` 头给本片的 sha256 |
| `POST /api/v1/images/uploads/{uid}/finalize` | 拼接 + 校验整体 sha256 + `qemu-img info` + 落正式目录 |
| `DELETE /api/v1/images/uploads/{uid}` | 弃用会话，清掉 chunks |
| `GET /api/v1/images` | 列出全部镜像 |
| `GET /api/v1/images/{id}` | 单镜像详情（含 `metadata_json`） |
| `DELETE /api/v1/images/{id}` | 删除（catalog row + 文件） |

错误码：400 = 参数错（sha 不是 64 hex / 长度对不上 / 缺片），409 = 同 sha256 已存在，
404 = 会话/镜像不存在，413 = init 请求体超 64 KiB。

### 12.5 UI

`/ui/images` 上有：
- 顶部 nav：Machines / Images
- 上传卡片：选文件 → 填 name/family/version/notes → 点 Upload
  - JS 用 SubtleCrypto 全量算 sha256；按 8 MiB 切片 PUT；每片单独算 sha256 走 `X-Chunk-Sha256`
  - 进度条按已完成片数 / 总片数显示
  - 出错或点 Abort 会调 `DELETE /uploads/{uid}` 清服务端临时目录
- 镜像表格：name / family / format / 文件大小 + 虚拟大小 / 上传时间 / sha 前 12 字符 / 删除按钮

### 12.6 验证

```bash
# 部署后 host147，无浏览器情况下用 curl 走完一次 5 KiB 假镜像：
H='http://192.168.10.147:8080'
AUTH='admin:metalkit'
NAME=fake.qcow2

dd if=/dev/urandom of=/tmp/fake.qcow2 bs=1024 count=5
SHA=$(sha256sum /tmp/fake.qcow2 | awk '{print $1}')
SIZE=$(stat -c %s /tmp/fake.qcow2)

# init
SESS=$(curl -fsSu "$AUTH" -X POST "$H/api/v1/images/uploads" \
  -H 'content-type: application/json' \
  -d "{\"name\":\"$NAME\",\"expected_sha256\":\"$SHA\",\"total_size\":$SIZE,\"chunk_size\":$SIZE}" \
  | jq -r .id)

# put single chunk
curl -fsSu "$AUTH" -X PUT "$H/api/v1/images/uploads/$SESS/chunks/1" \
  -H "X-Chunk-Sha256: $SHA" \
  --data-binary @/tmp/fake.qcow2

# finalize
curl -fsSu "$AUTH" -X POST "$H/api/v1/images/uploads/$SESS/finalize" | jq

# 列表里有它了
curl -fsSu "$AUTH" "$H/api/v1/images" | jq

# 落盘文件应该在 /var/lib/metalkit/images/{sha256}.qcow2（fallback 推断 format=qcow2）
ssh root@192.168.10.147 ls -la /var/lib/metalkit/images/
```

### 12.7 M2.2 验证清单

- [ ] `make test` 全绿（cmd/agent + internal/images + internal/httpd + internal/webui）
- [ ] controller 启动日志包含 `images-api` 组件、无 `qemu-img not on PATH` 警告（如已装 qemu-utils）
- [ ] `/ui/images` 弹 Basic Auth；登录后镜像表显示「No images uploaded yet」
- [ ] UI 上传一份 < 100 MiB 的 Ubuntu cloud qcow2 成功（进度条到 100%）
- [ ] 表格出现新镜像、format=qcow2、virtual_size 显示正确
- [ ] 删除按钮 → 文件 `/var/lib/metalkit/images/{sha}.qcow2` 同步消失
- [ ] 重复上传同 sha256 → init 返回 409
- [ ] curl 越过 Basic Auth（不带 `-u`）访问任意 `/api/v1/images*` → 401
