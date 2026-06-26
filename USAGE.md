# MetalKit 平台使用文档

> 一份面向**运维操作人员**的端到端使用指南：从 0 到把一台裸机装上 Linux。
> 配套的 API 细节参见 `docs/api.md`，功能清单参见 `docs/features.md`，装机排错参见 `qa.md`。

---

## 目录

1. [平台概览](#1-平台概览)
2. [登录与界面导览](#2-登录与界面导览)
3. [首次部署确认](#3-首次部署确认)
4. [把裸机纳管进来](#4-把裸机纳管进来)
5. [准备 OS 镜像](#5-准备-os-镜像)
6. [主流云镜像下载地址](#6-主流云镜像下载地址)
7. [配置子网（Subnet）](#7-配置子网subnet)
8. [创建装机套餐（Profile）](#8-创建装机套餐profile)
9. [绑定机器到套餐（Binding）](#9-绑定机器到套餐binding)
10. [触发装机并查看进度](#10-触发装机并查看进度)
11. [BMC 远程电源控制](#11-bmc-远程电源控制)
12. [API 速查](#12-api-速查)
13. [常见问题](#13-常见问题)

---

## 1. 平台概览

MetalKit 是一个**单二进制裸机 PXE 自动装机系统**。controller 进程跑在一台 Linux 服务器上（本文档示例为 `192.168.10.120`），同时承担：

- **DHCP / ProxyDHCP**（UDP 67）—— 给 PXE 客户端派 bootfile
- **BSDP**（UDP 4011）—— 兼容 Dell BIOS 的单播引导
- **TFTP**（UDP 69）—— 发 iPXE 二进制
- **HTTP**（TCP 8080）—— 发 vmlinuz / initrd / squashfs / 镜像 blob / Web UI / API

被装机的裸机只要和 controller 在同一个二层广播域、PXE 启用，就会自动被纳管。装机流程：

```
裸机 PXE 启动
   → 从 controller 拉 iPXE → 拉 live 内核 + initramfs + squashfs
   → 启动进 Debian live 系统，里面跑 agent
   → agent 上报硬件快照（POST /api/v1/report）
   → controller 检查这台机器有没有 binding
   → 有 binding 且 desired_state=install → 下发 InstallSpec
   → agent 拉镜像、写盘、配 cloud-init、装 GRUB、重启
   → controller 把 BMC bootdev 切回 disk，机器进入新系统
```

### 关键概念

| 概念 | 含义 |
|---|---|
| **Machine** | 一台被纳管的裸机，按 SMBIOS UUID 唯一标识 |
| **Image** | 一个 OS 镜像文件（qcow2 / raw / iso），SHA-256 内容寻址存储 |
| **Subnet** | 装完机后机器落到哪个网段（CIDR / 网关 / DNS / VLAN） |
| **Profile** | 装机套餐：镜像 + root 密码 + cloud-init 模板 + 网络/disk 策略，可复用给多台机器 |
| **Binding** | 把某台 machine + profile + subnet 三方绑定，声明"这台机器要装成什么样" |
| **Job** | 一次具体装机任务的执行实例，状态机 `pending → installing → succeeded / failed` |
| **BMC 凭据** | 机器的 IPMI 用户名/密码，AES-256 加密入库，UI 拿不回明文 |

---

## 2. 登录与界面导览

打开浏览器访问：

```
http://192.168.10.120:8080/ui/
```

默认账号（`config.yaml` 的 `adminUser` / `adminPass`）：

```
用户名：admin
密码：  metalkit
```

> ⚠️ 生产环境务必改 `config.yaml` 的 `adminPass`，并在反向代理层加 TLS。

| URL | 页面 | 用途 |
|---|---|---|
| `/ui/` | 机器列表 | 所有被纳管的裸机，按 `last_seen` 倒序，绿色=online、灰色=offline |
| `/ui/m/{uuid}` | 单机详情 | 硬件配置、报告历史、BMC 状态、binding 配置、装机触发按钮 |
| `/ui/images` | 镜像目录 | 上传 / 编辑 / 删除 OS 镜像 |
| `/ui/profiles` | 装机套餐 | Profile CRUD + cloud-init 模板编辑 |
| `/ui/subnets` | 子网管理 | Subnet CRUD |
| `/ui/bmc` | BMC 集中视图 | 批量电源操作、凭据测试 |
| `/ui/jobs` | 装机任务列表 | 按状态/机器过滤 |
| `/ui/jobs/{id}` | 单任务详情 | 阶段时间线 + 实时日志流（自动刷新） |

---

## 3. 首次部署确认

登录后先确认服务正常：

1. **服务状态**：controller 进程在跑（`systemctl status metalkit-controller`）
2. **DHCP 监听**：`ss -lun | grep ':67'` 应能看到 controller 占着 UDP 67
3. **HTTP 监听**：`curl http://192.168.10.120:8080/healthz` 返回 `ok`
4. **boot 文件就位**：`ls /opt/metalkit/boot/` 应有 `vmlinuz`、`initrd.img`、`filesystem.squashfs`
5. **网络可达性**：被装机的裸机和 controller 在同一二层广播域，且 UDP 67/69、TCP 8080 不被防火墙拦截

> 💡 如果 controller 跟被装机不在同段，需要单独配 DHCP Relay（ip helper-address）把 PXE 广播转过来。

---

## 4. 把裸机纳管进来

**不需要预先在平台登记机器**。任何机器只要 PXE 启动进 MetalKit live 镜像，agent 就会自动上报硬件。

### 4.1 让机器 PXE 启动

两种方式：

**A. 通过 BMC 远程触发**（推荐，机器已配置好 BMC 凭据）：

进 `/ui/bmc` → 找到机器 → 点 "PXE Boot + Power Cycle"。后端调 `ipmitool chassis bootdev pxe options=efiboot` + `power cycle`。

**B. 手动操作**：

机器开机 → 进 BIOS/iDRAC/iLO Boot Menu → 选 "PXE Network Boot"。

### 4.2 确认纳管

机器 PXE 启动 1-2 分钟后，刷新 `/ui/`，应该看到新机器出现在列表里。点进详情页可看：

- **Machine 段**：UUID、序列号、厂商、型号、BIOS 版本
- **CPU / Memory**：型号、核数、DIMM 详情
- **Disks**：块设备清单、容量、SMART 健康
- **NICs**：网卡名、MAC、PCI 位置、speed/duplex
- **PCI Devices**：全 PCI 设备清单
- **BMC**：BMC IP / MAC / 固件版本（如果能本地探到）

> 💡 如果机器一直不出现，进机器本地 console 看是否卡在 PXE（`PXE-E16: No offer received` 等错误），多半是 DHCP 不通或 controller 没监听 67。

---

## 5. 准备 OS 镜像

### 5.1 推荐使用云镜像（cloud image）

| 镜像类型 | 文件名特征 | 优点 |
|---|---|---|
| **GenericCloud / cloudimg** | `*.qcow2`，体积小（几百 MB～1GB） | 自带 cloud-init，MetalKit 直接 seed NoCloud 数据源即可 |
| **DVD ISO** | `*.iso`，体积大（4-9GB） | 需要人工或 kickstart 配合，MetalKit 当前**不推荐** |
| **Raw** | `*.img` | 体积最大，但兼容性最好，无 qemu-img 转换开销 |

**强烈建议用 GenericCloud / cloudimg**。MetalKit 的 cloud-init seed、自动 grow、GRUB 修复链都是针对云镜像调过的。

### 5.2 上传镜像

进 `/ui/images`：

1. 点 "Upload Image" → 选本地文件 → 上传（支持大文件分块断点续传）
2. 上传完后会自动跑 `qemu-img info` 检测格式和 `virtual_size`，并启发式判断 OS family
3. 在镜像列表里点编辑，确认 / 修正 `family`（rocky / ubuntu / debian / almalinux / centos / fedora / openeuler）、`format`（qcow2 / raw / iso）

### 5.3 API 上传（脚本化）

大文件推荐用 chunked upload：

```bash
SERVER=http://192.168.10.120:8080
AUTH=admin:metalkit
FILE=rocky-10-genericcloud.x86_64.qcow2

# 1. 开 session
SESSION=$(curl -s -u "$AUTH" -X POST "$SERVER/api/v1/images/uploads" \
  -d "{\"filename\":\"$(basename $FILE)\",\"size\":$(stat -c%s $FILE)}" \
  | jq -r .id)

# 2. 分块上传（每块 64MB）
CHUNK_SIZE=$((64*1024*1024))
TOTAL=$(stat -c%s $FILE)
N=$(( (TOTAL + CHUNK_SIZE - 1) / CHUNK_SIZE ))
for ((i=0; i<N; i++)); do
  OFFSET=$((i * CHUNK_SIZE))
  dd if=$FILE bs=$CHUNK_SIZE skip=$i count=1 2>/dev/null > /tmp/chunk
  curl -s -u "$AUTH" -X PUT "$SERVER/api/v1/images/uploads/$SESSION/chunks/$i" \
       --data-binary @/tmp/chunk
done

# 3. finalize
curl -s -u "$AUTH" -X POST "$SERVER/api/v1/images/uploads/$SESSION/finalize"
```

---

## 6. 主流云镜像下载地址

> **下载前先看**：MetalKit 装机管线对**云镜像（cloud image）**做了大量适配（cloud-init seed、自动 grow、GRUB 修复、SELinux 处理等）。**不要用 Server ISO**（需要 kickstart / preseed 手动配合，MetalKit 当前不支持）。下表里的链接全部指向云镜像。

### 6.1 Rocky Linux

| 版本 | 链接 | 备注 |
|---|---|---|
| Rocky 10 (latest) | https://download.rockylinux.org/pub/rocky/10/images/x86_64/Rocky-10-GenericCloud.latest.x86_64.qcow2 | 推荐物理服务器装机 |
| Rocky 9 (latest) | https://download.rockylinux.org/pub/rocky/9/images/x86_64/Rocky-9-GenericCloud.latest.x86_64.qcow2 | |
| Rocky 8 (latest) | https://download.rockylinux.org/pub/rocky/8/images/x86_64/Rocky-8-GenericCloud.latest.x86_64.qcow2 | |

下载页面总览：https://rockylinux.org/download

### 6.2 AlmaLinux

| 版本 | 链接 |
|---|---|
| AlmaLinux 10 | https://repo.almalinux.org/almalinux/10/cloud/x86_64/images/AlmaLinux-10-GenericCloud-latest.x86_64.qcow2 |
| AlmaLinux 9 | https://repo.almalinux.org/almalinux/9/cloud/x86_64/images/AlmaLinux-9-GenericCloud-latest.x86_64.qcow2 |
| AlmaLinux 8 | https://repo.almalinux.org/almalinux/8/cloud/x86_64/images/AlmaLinux-8-GenericCloud-latest.x86_64.qcow2 |

下载页面：https://almalinux.org/cloud-images/

### 6.3 CentOS

| 版本 | 链接 |
|---|---|
| CentOS Stream 10 | https://cloud.centos.org/centos/10-stream/x86_64/images/CentOS-Stream-GenericCloud-10-latest.x86_64.qcow2 |
| CentOS Stream 9 | https://cloud.centos.org/centos/9-stream/x86_64/images/CentOS-Stream-GenericCloud-9-latest.x86_64.qcow2 |

下载索引：https://cloud.centos.org/centos/

### 6.4 Ubuntu

| 版本 | 链接 |
|---|---|
| Ubuntu 24.04 LTS (Noble) | https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img |
| Ubuntu 22.04 LTS (Jammy) | https://cloud-images.ubuntu.com/jammy/current/jammy-server-cloudimg-amd64.img |
| Ubuntu 26.04 LTS (Resolute) | https://cloud-images.ubuntu.com/releases/26.04/release/ubuntu-26.04-server-cloudimg-amd64.img |

下载索引（含 daily build）：https://cloud-images.ubuntu.com/

### 6.5 Debian

| 版本 | 链接 |
|---|---|
| Debian 12 (Bookworm) | https://cloud.debian.org/images/cloud/bookworm/latest/debian-12-genericcloud-amd64.qcow2 |
| Debian 11 (Bullseye) | https://cloud.debian.org/images/cloud/bullseye/latest/debian-11-genericcloud-amd64.qcow2 |
| Debian 13 (Trixie) | https://cloud.debian.org/images/cloud/trixie/latest/debian-13-genericcloud-amd64.qcow2 |

下载索引：https://cloud.debian.org/images/cloud/

### 6.6 openEuler

| 版本 | 链接 |
|---|---|
| openEuler 24.03 LTS SP3 | https://repo.openeuler.org/openEuler-24.03-LTS-SP3/virtual_machine_img/x86_64/openEuler-24.03-LTS-SP3-x86_64.qcow2.xz |
| openEuler 22.03 LTS SP4 | https://repo.openeuler.org/openEuler-22.03-LTS-SP4/virtual_machine_img/x86_64/openEuler-22.03-LTS-SP4-x86_64.qcow2.xz |

下载索引：https://repo.openeuler.org/

> 💡 openEuler 镜像通常是 `.qcow2.xz`，下载后先 `xz -d` 解压再上传到 MetalKit。

### 6.7 镜像选择建议

| 场景 | 推荐 |
|---|---|
| Dell PowerEdge + PERC RAID | Rocky 10 GenericCloud（已测通，qa.md 有完整修复记录） |
| 通用物理服务器 | Rocky 9 / AlmaLinux 9 GenericCloud |
| 想要 SELinux 强制 | 自己 repack 镜像预装 kernel-modules，详见 `IMAGE-PREP.md` |
| Ubuntu 生态 | Ubuntu 24.04 cloudimg |
| 国产化合规 | openEuler 24.03 LTS |

---

## 7. 配置子网（Subnet）

进 `/ui/subnets` → "New Subnet"：

| 字段 | 示例 | 说明 |
|---|---|---|
| Name | `prod-mgmt` | 易读名字 |
| CIDR | `192.168.10.0/24` | 装完机机器要落的网段 |
| Gateway | `192.168.10.1` | |
| DNS | `223.5.5.5, 114.114.114.114` | 逗号分隔，可填多个 |
| VLAN ID | `10`（可选） | 留空 = 不打 VLAN tag |
| IP Pool | `192.168.10.100-192.168.10.200` | MetalKit 从池里自动给 binding 分配静态 IP（可选） |

> 💡 一个 subnet 可以被多个 profile / binding 复用。机器装完后落到这个网段，IP 由 subnet 池分配或 binding 单机覆盖。

---

## 8. 创建装机套餐（Profile）

进 `/ui/profiles` → "New Profile"：

### 8.1 基础字段

| 字段 | 示例 | 说明 |
|---|---|---|
| Name | `rocky10-dell-r630` | 1-64 字符，`[A-Za-z0-9._-]`，必须字母数字开头 |
| Description | `Rocky 10 for Dell PowerEdge R630` | 选填 |
| Hostname Template | `node-{{.Index}}` | Go template，`{{.Index}}` = binding 序号、`{{.UUID}}` = 机器 UUID 前 8 位 |
| Root Password Hash | （留空） | 留空走 `defaultRootPassword`；填了必须 `$6$...` sha512crypt 格式（用 `mkpasswd -m sha-512` 生成） |
| OS Family | `rocky` | 必须和镜像实际 family 一致 |
| Image | （下拉选已上传的镜像） | |
| Subnet | `prod-mgmt`（可选） | 给 binding 默认 subnet |

### 8.2 磁盘选择（Target Disk）

```json
{
  "selector": "largest",
  "min_size_gb": 100
}
```

支持的选择策略：
- `largest` —— 自动选最大盘
- `smallest` —— 自动选最小盘
- `by_path` —— 按 PCI 路径匹配（如 `/dev/disk/by-path/pci-0000:00:1f.2-ata-1.0`）
- `by_wwn` —— 按 WWN 匹配
- `by_model` —— 按盘型号匹配

### 8.3 网络配置（Network）

```json
{
  "method": "static",
  "interface": "bond0",
  "bond_mode": 4,
  "bond_slaves": ["eno1", "eno2"],
  "prefix_len": 24,
  "gateway": "192.168.10.1",
  "dns": ["223.5.5.5", "114.114.114.114"],
  "vlan": 10
}
```

字段说明：
- `method`：`dhcp` 或 `static`
- `interface`：bond0 / eth0 / eno1 等
- `bond_mode`：0=balance-rr、1=active-backup、4=LACP（802.3ad）、6=balance-alb
- `bond_slaves`：bond 的成员网卡名列表
- `prefix_len` / `gateway` / `dns` / `vlan`：如果 profile 关联了 subnet，这些字段可以留空（subnet 会补齐）

### 8.4 高级字段

| 字段 | 说明 |
|---|---|
| **Network Renderer** | 网络配置渲染策略：`auto`（按 OS family 选）、`netplan`、`nm-keyfile`、`sysconfig` |
| **Bootloader** | 引导安装策略：`auto`、`grub2`、`grub-efi`、`grub-pc` |
| **Chroot DNS** | chroot 里装包用的 DNS，逗号分隔。留空 = `223.5.5.5, 114.114.114.114`。当镜像缺 kernel-modules 需要联网补包时用到 |

### 8.5 cloud-init 模板（可选）

Profile 可以自带三个 Go template（留空走内置默认）：

- `user_data_template` —— cloud-init user-data
- `meta_data_template` —— cloud-init meta-data
- `network_config_template` —— cloud-init network-config

渲染期可注入：`.Hostname`、`.IP`、`.Gateway`、`.DNS`、`.VLAN`、`.RootPasswordHash`、`.MachineUUID`、`.BindingIndex`。

> 💡 普通装机不用碰这三个模板，留空走默认即可。需要自定义 ssh key / 额外用户 / runcmd 时再写。

---

## 9. 绑定机器到套餐（Binding）

进单机详情页 `/ui/m/{uuid}` → "Binding" 区域 → "Create Binding"：

| 字段 | 说明 |
|---|---|
| Profile | 选刚才创建的 profile |
| Subnet | 选 subnet（覆盖 profile 的 subnet） |
| IP Override | 单机指定静态 IP（覆盖 subnet 池分配） |
| VLAN Override | 单机指定 VLAN（覆盖 subnet） |
| NIC Selector | 单机指定网卡（覆盖 profile 的 bond_slaves 选择逻辑） |
| Target Disk Override | 单机指定目标盘（覆盖 profile 的 target_disk） |
| Password Override | 单机临时 root 密码 hash（覆盖 profile） |
| Desired State | `none` / `install` / `reinstall` —— 选 `install` 表示创建 binding 后立即触发装机 |

### 9.1 触发装机的两种姿势

**A. 创建 binding 时直接选 `install`**：

binding 一建立，5 秒内 orchestrator 会自动创建 Job，触发 BMC 上电 + PXE。

**B. 先建 binding（desired_state=`none`），稍后手动触发**：

进单机详情页 → 点 "Install" 或 "Reinstall" 按钮。后端把 desired_state 改成 `install` / `reinstall`，orchestrator 接管。

### 9.2 reinstall 的语义

`reinstall` 跟 `install` 的区别只在于：`install` 要求机器当前没装过（binding 处于初始状态），`reinstall` 允许覆盖已有系统。两者都会触发完整的 PXE + 写盘流程。

---

## 10. 触发装机并查看进度

### 10.1 任务列表

进 `/ui/jobs`，按状态过滤：

| 状态 | 含义 |
|---|---|
| `pending` | 已创建，orchestrator 还没触发 BMC |
| `preparing` | 正在触发 BMC 上电 + PXE |
| `installing` | agent 已领取 job，正在写盘 |
| `finalizing` | 写盘完成，正在切回硬盘启动 |
| `succeeded` | 装机完成，机器应该已经进新系统 |
| `failed` | 失败，点进去看错误码和日志 |
| `canceled` | 人工取消 |

### 10.2 单任务详情

点 job 进 `/ui/jobs/{id}`：

- **阶段时间线**：boot-detect → disk-pick → download → write → grow → mount → seed → grub-install → umount
- **日志流**（自动刷新）：实时显示 installer 输出，包括 dnf / dracut / grubby 命令输出
- **错误信息**：失败时显示 `error_code` + `error_detail`

### 10.3 装机成功的判定

Job 进入 `succeeded` 状态需要满足：

1. agent 跑完 9 个 stage 全部成功
2. agent POST `/agent/jobs/{id}/success`
3. orchestrator 检测到 `finished_at >= binding.updated_at`，调 BMC 切回 `bootdev=disk` 并 power cycle

机器重启后应该进新系统。如果机器一直起不来，参见第 13 节。

### 10.4 取消任务

`POST /api/v1/jobs/{id}/cancel`，或在 Web UI 点 "Cancel"。运行中的 job 标 `canceling`，agent 下次 poll 时退出。

---

## 11. BMC 远程电源控制

### 11.1 录入 BMC 凭据

进单机详情页 → "BMC" 区域 → "Configure BMC"：

| 字段 | 示例 |
|---|---|
| Host | `192.168.10.50` |
| Username | `root` |
| Password | `calvin` |

> 💡 Dell iDRAC 默认 `root/calvin`，HP iLO 默认 `Administrator/随机密码`，华为 iBMC 默认 `Administrator/Admin@9000`。

保存前可以点 "Test"，后端跑 `ipmitool -I lanplus -H <host> -U <user> -P <pass> chassis status` 验证可达性。

### 11.2 电源操作

| 操作 | 含义 |
|---|---|
| `power on` | 上电 |
| `power off` | 硬关机（直接断电） |
| `power cycle` | 断电再上电 |
| `power reset` | 软复位（按 reset 按钮） |
| `power soft` | 软关机（OS graceful shutdown） |
| `bootdev pxe` | 下次启动从 PXE（一次性） |
| `bootdev disk` | 下次启动从硬盘（一次性） |

### 11.3 一键 PXE 装机

单机详情页 → "PXE Boot + Install" 按钮 = 一次性触发：BMC 切 `bootdev=pxe` + power cycle + 创建 job。

---

## 12. API 速查

所有 API 走 Basic Auth（`admin:metalkit`），除了 agent 端点（`/api/v1/agent/*`、`/api/v1/report`、`/api/v1/heartbeat/*`）开放。

### 12.1 机器

```bash
# 列出所有机器
curl -u admin:metalkit http://192.168.10.120:8080/api/v1/machines

# 单机详情（最近一份完整 report）
curl -u admin:metalkit http://192.168.10.120:8080/api/v1/machines/<uuid>

# 按 MAC 反查
curl -u admin:metalkit "http://192.168.10.120:8080/api/v1/lookup?mac=aa:bb:cc:dd:ee:ff"
```

### 12.2 镜像

```bash
# 列出镜像
curl -u admin:metalkit http://192.168.10.120:8080/api/v1/images

# 修改 family / format
curl -u admin:metalkit -X PATCH http://192.168.10.120:8080/api/v1/images/<sha> \
  -d '{"family":"rocky","format":"qcow2"}'
```

### 12.3 Profile

```bash
# 创建
curl -u admin:metalkit -X POST http://192.168.10.120:8080/api/v1/profiles -d '{
  "name": "rocky10-dell",
  "hostname_template": "node-{{.Index}}",
  "os_family": "rocky",
  "image_id": "<sha>",
  "subnet_id": "<subnet-id>",
  "target_disk": {"selector":"largest","min_size_gb":100},
  "network": {"method":"static","interface":"bond0","bond_mode":4,"bond_slaves":["eno1","eno2"]},
  "chroot_dns": "223.5.5.5, 114.114.114.114"
}'

# 列出
curl -u admin:metalkit http://192.168.10.120:8080/api/v1/profiles

# 部分更新（只改 chroot_dns）
curl -u admin:metalkit -X PATCH http://192.168.10.120:8080/api/v1/profiles/<id> \
  -d '{"chroot_dns":"8.8.8.8, 1.1.1.1"}'
```

### 12.4 Binding

```bash
# 创建 binding 并立即装机
curl -u admin:metalkit -X POST http://192.168.10.120:8080/api/v1/bindings -d '{
  "machine_uuid": "<uuid>",
  "profile_id": "<profile-id>",
  "subnet_id": "<subnet-id>",
  "desired_state": "install"
}'

# 改 desired_state 触发重装
curl -u admin:metalkit -X PATCH http://192.168.10.120:8080/api/v1/bindings/<uuid> \
  -d '{"desired_state":"reinstall"}'
```

### 12.5 Job

```bash
# 列出
curl -u admin:metalkit http://192.168.10.120:8080/api/v1/jobs

# 单 job 详情（含元数据）
curl -u admin:metalkit http://192.168.10.120:8080/api/v1/jobs/<id>

# 拉日志（增量）
curl -u admin:metalkit "http://192.168.10.120:8080/api/v1/jobs/<id>/logs?since_offset=0"

# 取消
curl -u admin:metalkit -X POST http://192.168.10.120:8080/api/v1/jobs/<id>/cancel
```

### 12.6 BMC

```bash
# 录入凭据
curl -u admin:metalkit -X PUT http://192.168.10.120:8080/api/v1/bmc/<uuid> -d '{
  "host":"192.168.10.50","username":"root","password":"calvin"
}'

# 测试凭据
curl -u admin:metalkit -X POST http://192.168.10.120:8080/api/v1/bmc/<uuid>/test

# 电源操作
curl -u admin:metalkit -X POST http://192.168.10.120:8080/api/v1/bmc/<uuid>/power/cycle
```

---

## 13. 常见问题

### 13.1 机器 PXE 启动后不出现

**现象**：机器进 PXE，但 `/ui/` 列表里看不到新机器。

**排查**：
1. controller 是否在监听 UDP 67？`ss -lun | grep ':67'`
2. controller 和机器是否在同一二层？跨段需要 DHCP Relay
3. 机器 console 上有没有 PXE 错误？`PXE-E16: No offer received` = DHCP 不通；`PXE-E32: TFTP open timeout` = TFTP 不通
4. controller 日志：`journalctl -u metalkit-controller -f`，看有没有 DHCP 请求进来

### 13.2 装机 Job 一直卡在 `pending`

orchestrator 5 秒一轮。如果超过 30 秒还在 `pending`：

1. 检查 binding 的 BMC 凭据是否录入且测试通过
2. controller 日志看 `ipmitool` 是否报错（凭据错、网络不通、BMC 拒绝）
3. 手动验证：`ipmitool -I lanplus -H <bmc-ip> -U <user> -P <pass> chassis status`

### 13.3 装机 Job `failed`，进 emergency mode

进 `/ui/jobs/{id}` 看日志和错误码。最常见的几类：

| 错误码 / 关键字 | 根因 | 修复 |
|---|---|---|
| `dracut-initqueue timeout` | initramfs 缺 RAID 驱动 | MetalKit 已自动跑 `dnf install kernel-modules` + `dracut --force-drivers`，看日志是否成功。仍失败多半是 chroot DNS 不通，去 profile 改 `chroot_dns` |
| `Failed to start initrd-switch-root` | 老内核 initramfs 缺 megaraid_sas，dnf 拉了新内核但默认还是老的 | MetalKit 已自动 `grubby --set-default` 选驱动完整的内核 |
| `unknown filesystem type 'vfat'` | initramfs 缺 vfat.ko | MetalKit 已自动 `dracut --force-drivers vfat fat` |
| `Permission denied` on `/etc/...` | SELinux 拒绝读 unlabeled 文件 | MetalKit 已自动 `SELINUX=disabled` |
| `kdump.service failed` | kdump 装不起来拖累启动 | MetalKit 已自动 mask kdump.service |
| Plymouth 卡企鹅图标 | console=ttyS0 把日志重定向到串口 | MetalKit 已自动改成 `console=tty0 nomodeset` |

更详细的根因和修复记录在 `qa.md`。

### 13.4 装完机器起不来，本地屏幕黑屏 / 卡 Plymouth

进单机详情页，看 BMC 系统事件日志（SEL）。或者本地 console 上按 `e` 编辑 GRUB，把 `console=ttyS0,115200n8` 删掉、加上 `console=tty0 nomodeset`，按 Ctrl+X 启动。

MetalKit 已经在装机阶段自动做这个修改了（`fixRHELGrubCmdline`）。如果还是出问题，检查日志里有没有 `fix-grub-cmdline: rewrote GRUB_CMDLINE_LINUX_DEFAULT` 这一行。

### 13.5 镜像上传失败

1. 磁盘空间够吗？`df -h /var/lib/metalkit/images`
2. 文件名带空格 / 特殊字符？换成纯 ASCII
3. 用 chunked upload 而不是一次性 POST（大文件容易超时）

### 13.6 BMC 操作返回 `ipmitool: command not found`

controller 服务器上没装 ipmitool：

```bash
apt install -y ipmitool     # Debian/Ubuntu
dnf install -y ipmitool     # RHEL/Rocky
```

### 13.7 改了 profile，已装好的机器没生效

Profile 改动只影响**之后**的装机。已经装好的机器不会自动重新配置。要让改动生效，对该机器的 binding 触发 `reinstall`。

### 13.8 想看历史硬件快照

进单机详情页 → "Report History"，每次机器 PXE 启动都会留一条 report。点不同时间戳可以回放当时的硬件状态。

---

## 附录：相关文档

| 文档 | 内容 |
|---|---|
| `README.md` | 项目概述、快速部署 |
| `CONFIG.md` | `config.yaml` 配置项详解 |
| `DEPLOY.md` | 详细部署步骤 |
| `IMAGE-PREP.md` | 如何预制 / repack 基础镜像（解决 cloud image 缺驱动等问题） |
| `docs/api.md` | 完整 HTTP API 参考 |
| `docs/features.md` | 功能清单（按模块） |
| `qa.md` | 装机问题排错记录（Rocky 8/9/10 各种坑） |
| `DEVELOP.md` | 开发者指南 |
