# metalkit live-image 装机工具链（M2.3）

本文给 M2.3-7（agent 装机执行器）落地用。覆盖 live 镜像里**装机阶段会调用的命令行工具**——
镜像写盘、分区扩展、cloud-init NoCloud seed 生成、UEFI grub 安装。仅 UEFI；BIOS / chroot
fallback 在 M2.4 才上。

包名都按 Debian 12 bookworm 校对，已加进
`live-image/config/package-lists/installer.list.chroot`，下次 `scripts/build-live.sh`
重出 squashfs 时即生效。

---

## 1. Pipeline

agent 拿到 job（`{image, profile, binding}`）后按下表顺序跑。变量约定：

- `$IMG_URL` —— `http://controller:8080/api/v1/images/{id}/blob`（M2.3-4 上线的下载端点）
- `$DISK` —— 目标盘块设备，如 `/dev/sda`（按 profile.target_disk 选盘后定下）
- `$ROOT_PART_NUM` —— 镜像里 root 分区号（一般是最大编号那一个）
- `$ROOT_PART` —— `${DISK}${ROOT_PART_NUM}`（nvme/mmc 需要中间加 `p`，agent 处理）
- `$ESP_PART` —— ESP 分区（FAT32 标号 `EFI System` 或 type `ef00`）
- `/mnt/root` / `/mnt/root/boot/efi` —— 临时挂载点
- `/run/metalkit/seed` —— 工作目录（tmpfs）

### 1.1 下载镜像并 stream 写盘

写盘建议 stream，不要落临时文件——10 GB 镜像 + 落 ext4 再读一次太亏。

```sh
curl -fsSL "$IMG_URL" \
  | qemu-img convert -p -f qcow2 -O raw /dev/stdin "$DISK"
```

- `curl` (curl), `qemu-img` (qemu-utils 已有)。
- qcow2 内部 sparse 已经被 qemu-img 展开成 raw；写到块设备直接是物理 sector，**不**需要再 `dd`。
- 写完跑 `sync` + `partprobe "$DISK"`（partprobe 来自 parted 包，已有），确保内核重读分区表。

### 1.2 探测最后一个分区 + 扩到末尾

cloud 镜像 root 分区一般是最大编号那一个（Ubuntu cloudimg 是 p1，Debian genericcloud 也是
p1；但金标镜像可能有 ESP+root+other 多分区，逻辑统一按"最大编号"找）。

```sh
ROOT_PART_NUM=$(lsblk -nrpo NAME,TYPE "$DISK" \
                | awk '$2=="part"{print $1}' \
                | tail -1 \
                | sed -E "s|.*[^0-9]([0-9]+)$|\1|")
growpart "$DISK" "$ROOT_PART_NUM"
partprobe "$DISK"
```

- `lsblk` (util-linux，base 系统自带), `growpart` (**cloud-guest-utils**), `partprobe` (parted)。
- `growpart` 也会自动重写 GPT 备份表到新末尾——10 GB 镜像写到 1 TB 盘也只动 GPT，不动数据。

### 1.3 扩文件系统

按 fs 类型分支：

```sh
FSTYPE=$(blkid -s TYPE -o value "$ROOT_PART")
case "$FSTYPE" in
  ext4) e2fsck -f -y "$ROOT_PART"; resize2fs "$ROOT_PART" ;;
  xfs)  mount "$ROOT_PART" /mnt/root && xfs_growfs /mnt/root ;;  # xfs 需要先挂
  btrfs) mount "$ROOT_PART" /mnt/root && btrfs filesystem resize max /mnt/root ;;
  *) echo "unsupported fs: $FSTYPE" >&2; exit 1 ;;
esac
```

- `blkid` (util-linux), `resize2fs/e2fsck` (e2fsprogs), `xfs_growfs` (xfsprogs),
  `btrfs` (btrfs-progs)——全部已经在列表上。
- ext4 在挂载前扩；xfs/btrfs 在线扩；agent 把这条逻辑包成一个函数。

### 1.4 挂 root + ESP

```sh
mountpoint -q /mnt/root || mount "$ROOT_PART" /mnt/root
mkdir -p /mnt/root/boot/efi
ESP_PART=$(lsblk -nrpo NAME,PARTTYPE "$DISK" \
           | awk '$2=="c12a7328-f81f-11d2-ba4b-00a0c93ec93b"{print $1}')
mount "$ESP_PART" /mnt/root/boot/efi
```

- ESP 用 GPT type GUID `c12a7328-f81f-11d2-ba4b-00a0c93ec93b` 探测——比 `blkid LABEL=EFI`
  靠谱（cloud 镜像 ESP 通常没设 label 或 label 不固定）。
- 还要 bind mount：

```sh
for d in proc sys dev dev/pts run; do
  mount --bind /$d /mnt/root/$d
done
```

UEFI 变量节点（M2.3 不需要进 chroot 装 grub 也行；但 efibootmgr 必须能写）：

```sh
mount --bind /sys/firmware/efi/efivars /mnt/root/sys/firmware/efi/efivars
```

### 1.5 生成 NoCloud seed

**选 ISO 不选 FAT：**

| | FAT 镜像（mtools+mkfs.vfat） | ISO9660（xorriso/genisoimage） |
|---|---|---|
| cloud-init 兼容 | 都支持（label=CIDATA） | 都支持（label=CIDATA） |
| 构建工具 | `mkfs.vfat` + `mcopy` 两步 | `cloud-localds` 一步包好 |
| 体积下限 | FAT12 最小 ~32 KB | ISO 最小 ~256 KB |
| 写盘 | 必须先 `dd` 出空镜像再格 | xorriso 直接产文件 |
| 失败模式 | mtools 编码坑（短文件名） | 干净 |

→ **走 ISO**。`cloud-localds`（cloud-image-utils 提供）底下调 `genisoimage`/`xorriso`
任选其一，Debian 12 上默认链 `xorriso`，所以装 `xorriso`+`cloud-image-utils`。装
`mtools` 留作降级选项（也是 cloud-init seed 调试时的常用工具）。

工作目录 `/run/metalkit/seed`：

```sh
mkdir -p /run/metalkit/seed
cd /run/metalkit/seed
# 写 user-data / meta-data / network-config，见 §1.6
cloud-localds \
  --filesystem=iso9660 \
  --network-config=network-config \
  seed.iso user-data meta-data
```

把 seed ISO 落到 ESP 或者一个独立分区。**M2.3 落 ESP 上的 `/boot/efi/metalkit-seed.iso`**
（最简：不动镜像分区表），让 cloud-init datasource 通过内核 cmdline 指过去：

```sh
cp seed.iso /mnt/root/boot/efi/metalkit-seed.iso
```

然后在 §1.7 装 grub 时，往 `/etc/default/grub` 追加：

```sh
GRUB_CMDLINE_LINUX_DEFAULT="$GRUB_CMDLINE_LINUX_DEFAULT ds=nocloud;s=/dev/disk/by-label/CIDATA"
```

> ⚠ 这里有个 open question：ESP 是 FAT32 不是 ISO9660，label 也不是 CIDATA。简单点是把
> seed.iso loop-mount 到 /var/lib/cloud/seed/nocloud-net/，但镜像里 cloud-init 还没跑过、
> 路径不一定存在。更稳的做法是**单独切一个 64 MiB 分区**写 CIDATA ISO，付出多一次 partprobe
> 的代价。这条建议 M2.3-7 实现时再敲定，先记成 TODO。

### 1.6 user-data / meta-data / network-config 骨架

agent 按 profile + binding 模板渲染。下面是骨架（YAML 头第一行必须是
`#cloud-config`，meta-data/network-config 不需要）。

**user-data**：

```yaml
#cloud-config
hostname: node-ABC1234           # binding.hostname 优先；否则展开 profile.hostname_template
fqdn: node-ABC1234.metalkit.lan
manage_etc_hosts: true

# root 密码（profile.root_password_hash，sha512crypt）
chpasswd:
  expire: false
users:
  - name: root
    lock_passwd: false
    hashed_passwd: "$6$rounds=4096$<salt>$<hash>"
  - name: platops
    gecos: metalkit platform admin
    groups: [sudo]
    sudo: "ALL=(ALL) NOPASSWD:ALL"
    shell: /bin/bash
    lock_passwd: true
    ssh_authorized_keys:
      - "ssh-ed25519 AAAA... platform-admin@metalkit"   # M2.3-3 controller 全局密钥

# 一次性脚本：通知 controller job 完成（M2.3-7 实现）
runcmd:
  - [curl, -fsS, -X, POST, "http://controller:8080/api/v1/jobs/${JOB_ID}/done"]

# 关掉 cloud-init 后续再跑（避免重启又跑一遍）
power_state:
  mode: reboot
  condition: True
```

**meta-data**（agent 必填 instance-id，不然 cloud-init 会认为是同一台机器跳过运行）：

```yaml
instance-id: metalkit-<uuid>-<job_id>
local-hostname: node-ABC1234
```

**network-config** v2 格式：

```yaml
version: 2
ethernets:
  primary:
    match:
      macaddress: "aa:bb:cc:dd:ee:ff"   # profile.network.nic_selector 算出来的具体 MAC
    set-name: eno1
    addresses: [10.0.0.7/24]            # binding.static_address + profile.network.prefix_len
    routes:
      - to: default
        via: 192.168.10.1               # profile.network.gateway
    nameservers:
      addresses: [8.8.8.8]              # profile.network.dns
# dhcp 模式时 addresses/routes/nameservers 全部省略，dhcp4: true
```

### 1.7 grub-install（UEFI）

```sh
chroot /mnt/root grub-install \
  --target=x86_64-efi \
  --efi-directory=/boot/efi \
  --bootloader-id=metalkit \
  --recheck
chroot /mnt/root update-grub
```

- `grub-install` 来自镜像里的 grub-efi-amd64 包，**但 live 镜像也要装** `grub-efi-amd64-bin`
  + `grub-efi-amd64-signed` + `shim-signed`——cloud 镜像有时候 grub 文件残缺（虚机模板裁过），
  live 端这套作为 fallback 让 `grub-install` 能复制模块文件出去。
- `--bootloader-id=metalkit` 在 NVRAM 里建 `Boot0001*metalkit` 一项，避免和 Dell 内置
  `UEFI Boot Manager` / 镜像里残留的 `ubuntu` 项冲突。
- `update-grub` 重生成 `/boot/grub/grub.cfg`，会把 §1.5 追加的 `ds=nocloud;...` cmdline
  落进去。

确认 NVRAM 写入：

```sh
chroot /mnt/root efibootmgr -v | grep -i metalkit
```

如果要把 metalkit 项设为下次启动：

```sh
BOOTNUM=$(chroot /mnt/root efibootmgr | awk '/metalkit/{sub(/\*$/,"",$1); sub(/Boot/,"",$1); print $1; exit}')
chroot /mnt/root efibootmgr -n "$BOOTNUM"   # BootNext
```

> M2.3 实际不需要在 agent 里设 BootNext——controller 之后会 `ipmitool chassis bootdev disk
> + chassis power cycle`，BIOS 启动顺序里只要 metalkit 项排在前面就行。装机完写一条 boot
> 顺序到 NVRAM 顶端：`efibootmgr -o $BOOTNUM,$(其余)`。

### 1.8 unmount + 上报

```sh
sync
for d in run dev/pts dev sys/firmware/efi/efivars sys proc boot/efi; do
  umount /mnt/root/$d || true
done
umount /mnt/root
curl -fsS -X POST "http://controller:8080/api/v1/jobs/$JOB_ID/done"  # M2.3-5
```

agent 退出后 live 系统**不**自己重启——controller 收到 `done` 后才发
`ipmitool chassis bootdev disk + chassis power cycle`，理由：避免 agent 还没回报状态机器
就 reboot 走了，jobs 表里留下一条没收尾的 running 记录。

---

## 2. 工具参考表

base 系统包不再标"已有"——下面只列**装机阶段真正会调到的**二进制。

| 二进制 | 包 | 用途 | M2.3 关键? |
|---|---|---|---|
| `curl` | curl | 下镜像、回 controller | ✅ |
| `qemu-img` | qemu-utils | qcow2→raw stream 写盘 | ✅ |
| `partprobe` | parted | 写完盘重读分区表 | ✅ |
| `sgdisk` / `sfdisk` | gdisk / fdisk(util-linux) | 探 GPT 分区 type GUID；可能切 CIDATA 分区 | ✅ |
| `lsblk` | util-linux | 列盘、找最大编号分区、ESP 探测 | ✅ |
| `blkid` | util-linux | 探文件系统类型 | ✅ |
| `growpart` | **cloud-guest-utils** | 把 root 分区扩到盘末尾 | ✅ |
| `e2fsck` / `resize2fs` | e2fsprogs | ext4 离线扩 | ✅ |
| `xfs_growfs` | xfsprogs | xfs 在线扩 | ✅ |
| `btrfs` | btrfs-progs | btrfs 在线扩；M2.3 不强求 | ⬜ |
| `mkfs.vfat` | dosfstools | （仅 FAT seed 路径）格 CIDATA 镜像 | ⬜ 降级用 |
| `mcopy` / `mformat` | **mtools** | （仅 FAT seed 路径）拷文件进 FAT 镜像 | ⬜ 降级用 |
| `cloud-localds` | **cloud-image-utils** | 一行命令把 user-data/meta-data/network-config 打成 NoCloud ISO | ✅ |
| `xorriso` | **xorriso** | cloud-localds 底下用的 ISO9660 作者；也可直接调 | ✅（被 cloud-localds 依赖） |
| `genisoimage` | genisoimage（**未装**，xorriso 替代） | 同上的老牌方案 | ⬜ |
| `grub-install` | **grub-efi-amd64-bin** + **grub-efi-amd64-signed** | UEFI grub 装到 ESP，写 NVRAM 项 | ✅ |
| `shim` 二进制 | **shim-signed** | Secure Boot 链上的第一阶段；BIOS Secure Boot 开着时必须有 | ✅（开 SB 时） |
| `update-grub` | grub-common（grub-efi-amd64-* 依赖） | 重生成 grub.cfg | ✅ |
| `efibootmgr` | **efibootmgr** | 验证 NVRAM 项、设 BootNext / BootOrder | ✅ |
| `mkpasswd` | **whois** | （仅 dev/调试）生成 sha512crypt root_password_hash | ⬜ 调试用 |
| `ipmitool` | ipmitool | live 端基本不用；agent 探 BMC 状态时可能调 | ⬜ |
| `rsync` / `wget` / `pv` / `xz` / `zstd` | 同名包 | 备用传输工具 | ⬜ |

加粗 = M2.3 新增到 `installer.list.chroot` 的包。其余都在 M1/M2.1 已经进去了。

---

## 3. Open questions / TODO

记在这里给 M2.3-7 实现时一起敲定，不在 M2.3-8 范围内：

1. **CIDATA 落地位置**：ESP 上塞 ISO + 内核 cmdline `ds=nocloud;s=...`，还是切 64 MiB 独立
   `CIDATA` label 分区？前者省一次 `parted`，后者 cloud-init 探测最稳。倾向后者。
2. **镜像里 grub 残缺时**：M2.3-7 是否要在 chroot 里 `apt install -y grub-efi-amd64` 把
   缺失的 grub 包补回去？需要镜像里有可用的 apt 源；离线场景不成立。倾向"lint 阶段拒收
   grub 残缺的镜像"，问题前推到 M2.2 / G4。
3. **Secure Boot**：Dell 默认 SB on。`shim-signed` + `grub-efi-amd64-signed` 装好后默认
   流程应该过；但 `--bootloader-id=metalkit` 的项不在 shim 默认信任链里，可能需要走
   `/EFI/debian/shimx64.efi` 路径。M2.3-7 真机过一遍 SB on/off 都得验证。
4. **NoCloud datasource 锁定**：镜像里 `/etc/cloud/cloud.cfg.d/` 没限定 `datasource_list:
   [ NoCloud, None ]` 时，cloud-init 可能先去 EC2/Azure metadata IP 卡 30s。G2 已经把这
   条列成 checklist，M2.2 lint 是否要把它升成"拒收"待定。
