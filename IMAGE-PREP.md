# MetalKit 基础镜像预制指南

MetalKit 用 cloud image (qcow2) 装机。某些发行版的 cloud image 在物理服务器上会缺关键内核驱动 / 启动配置不对，导致装完进不了系统。本指南描述如何预制"MetalKit 友好"的镜像，让装机一次成功。

## 为什么需要预制

Rocky 10 GenericCloud 是典型反面教材，物理服务器上踩了三个坑（详见 [qa.md](./qa.md) #4-#8）：

1. **cloud image 不装 `kernel-modules` 包** → `megaraid_sas.ko`（Dell PERC RAID 驱动）缺失 → initramfs 找不到磁盘 → dracut emergency
2. **initramfs 没加载 vfat** → `/boot/efi` 挂载失败 → local-fs.target 失败 → emergency
3. **SELinux enforcing + 我们写入的配置文件 unlabeled** → NetworkManager / systemd-modules-load 拒绝读 → 网络不通 / 服务起不来 → emergency

镜像层一次性修好这三个问题后，installer 端的兜底逻辑只是为了应对未预制或预制不完整的镜像。**镜像层是首选方案**，installer 兜底是双保险。

---

## 预制环境

任选一种：

### 方案 A：libvirt/KVM（推荐，需要图形/虚拟化环境）

需要一台装了 `libvirt` + `qemu-kvm` 的 Linux 宿主机（可以是 MetalKit controller 本身，也可以是另一台 VM host）。

### 方案 B：qemu-nbd 离线修改（不需启动 VM）

不需要启动虚拟机，直接挂载 qcow2 改文件。但 `dnf install` 需要在 chroot 内跑且要有网络，对离线操作要求较高。本指南主要讲方案 A，方案 B 见末尾"离线修改"小节。

---

## 预制 Rocky 10（典型示例）

### 1. 准备原始镜像

```bash
mkdir -p /var/lib/libvirt/images/prep
cd /var/lib/libvirt/images/prep

# 下载原始 GenericCloud 镜像（如果 MetalKit 已经有，直接拷贝过来）
curl -O https://download.rockylinux.org/pub/rocky/10/images/x86_64/Rocky-10-GenericCloud.latest.x86_64.qcow2

# 用 backing file 方式启动，不污染原文件
cp Rocky-10-GenericCloud.latest.x86_64.qcow2 Rocky-10-GenericCloud-metalkit.base.qcow2
```

### 2. 启动 VM

```bash
virt-install \
  --name rocky10-prep \
  --memory 4096 --vcpus 2 \
  --disk path=/var/lib/libvirt/images/prep/Rocky-10-GenericCloud-metalkit.base.qcow2,bus=virtio \
  --network network=default,model=virtio \
  --import --os-variant rockylinux-10 \
  --noautoconsole
```

### 3. 拿到 IP 并 ssh 登录

```bash
# 等 30 秒让 DHCP 分配
sleep 30
virsh domifaddr rocky10-prep
# 输出类似：192.168.122.10

# cloud image 默认用户 rocky，需要 ssh key 注入；或者用 virsh console 进去
virsh console rocky10-prep
# 进 console 后用 rocky 登录（密码无，需通过 cloud-init 注入 ssh key）
# 或者用 root 用户：默认锁，需要先在 cloud-init 注入密码
```

**简化办法**：用 `cloud-localds` 生成 seed ISO 注入 ssh key 和 root 密码：

```bash
cat > /tmp/user-data <<'EOF'
#cloud-config
ssh_pwauth: true
disable_root: false
chpasswd:
  list: |
    root:metalkit
  expire: false
runcmd:
  - sed -i 's/^SELINUX=.*/SELINUX=disabled/' /etc/selinux/config
EOF
cloud-localds /var/lib/libvirt/images/prep/seed.iso /tmp/user-data

# 重启 VM 时 attach seed ISO
virsh attach-disk rocky10-prep /var/lib/libvirt/images/prep/seed.iso vdb --type cdrom --mode readonly
virsh reboot rocky10-prep

# 之后 ssh root@<IP> 密码 metalkit
```

### 4. 在 VM 内做预制

```bash
ssh root@<VM-IP>

# 4.1 装完整内核驱动包（cloud image 默认缺）
dnf install -y kernel-modules kernel-modules-extra

# 4.2 重装 kernel-core 触发 scriptlet 重新生成 vmlinuz/initramfs/BLS entry
dnf reinstall -y kernel-core

# 4.3 验证关键驱动存在
KVER=$(ls /lib/modules | sort -V | tail -1)
ls /lib/modules/$KVER/kernel/drivers/scsi/megaraid/megaraid_sas.ko.xz
ls /lib/modules/$KVER/kernel/fs/fat/vfat.ko.xz
# 两个都应能列出来

# 4.4 重建 initramfs，强制注入关键驱动
dracut --no-hostonly --force \
  --force-drivers "megaraid_sas mpt3sas hpsa aacraid smartpqi nvme vfat fat" \
  --add-drivers  "megaraid_sas mpt3sas hpsa aacraid smartpqi nvme vfat fat" \
  /boot/initramfs-$KVER.img $KVER

# 验证 initramfs 包含这些驱动
lsinitrd /boot/initramfs-$KVER.img | grep -E 'megaraid_sas|vfat'

# 4.5 关 SELinux（避免 unlabeled 配置文件被拒绝读取）
sed -i 's/^SELINUX=.*/SELINUX=disabled/' /etc/selinux/config
grep '^SELINUX' /etc/selinux/config  # 应输出 SELINUX=disabled

# 4.6 关 kdump（cloud image 默认 enable，物理机上会失败触发 emergency）
systemctl disable --now kdump
systemctl mask kdump

# 4.7 修改 GRUB 默认 cmdline：去掉 console=ttyS0，加 console=tty0 nomodeset
sed -i 's/GRUB_CMDLINE_LINUX_DEFAULT=.*/GRUB_CMDLINE_LINUX_DEFAULT="console=tty0 nomodeset no_timer_check crashkernel=1G-4G:192M,4G-64G:256M,64G-:512M"/' /etc/default/grub
grub2-mkconfig -o /boot/grub2/grub.cfg

# 4.8 设置 root 密码（cloud image 默认锁定）
echo 'root:metalkit' | chpasswd
passwd -u root

# 4.9 改 cloud-init 配置，避免首次启动覆盖我们的设置
cat > /etc/cloud/cloud.cfg.d/99-metalkit.cfg <<'EOF'
disable_root: false
ssh_pwauth: true
disable_modules:
  - set-passwords
  - ssh-authkey-fingerprints
EOF

# 4.10 清理 cloud-init 状态，让镜像能再次跑 cloud-init
cloud-init clean --logs
rm -rf /var/lib/cloud/instances/*

# 4.11 清理 dnf 缓存，减小镜像体积
dnf clean all
rm -rf /var/cache/dnf/*

# 4.12 关机
poweroff
```

### 5. 导出预制好的镜像

```bash
# 在宿主机上
virsh destroy rocky10-prep 2>/dev/null
virsh undefine rocky10-prep

# flatten backing file，输出独立 qcow2
qemu-img convert -O qcow2 \
  /var/lib/libvirt/images/prep/Rocky-10-GenericCloud-metalkit.base.qcow2 \
  /var/lib/libvirt/images/prep/Rocky-10-GenericCloud-metalkit.x86_64.qcow2

# 验证镜像
qemu-img info /var/lib/libvirt/images/prep/Rocky-10-GenericCloud-metalkit.x86_64.qcow2

# （可选）压缩镜像
qemu-img convert -O qcow2 -c \
  /var/lib/libvirt/images/prep/Rocky-10-GenericCloud-metalkit.x86_64.qcow2 \
  /var/lib/libvirt/images/prep/Rocky-10-GenericCloud-metalkit.compressed.qcow2
```

### 6. 上传到 MetalKit

```bash
# 通过 API 上传
curl -u admin:metalkit \
  -F image=@/var/lib/libvirt/images/prep/Rocky-10-GenericCloud-metalkit.x86_64.qcow2 \
  -F name="Rocky-10-GenericCloud-metalkit" \
  -F os=rocky \
  -F version=10 \
  http://192.168.10.120:8080/api/v1/images

# 或直接 scp 到 controller，再用 controller 的命令导入
scp /var/lib/libvirt/images/prep/Rocky-10-GenericCloud-metalkit.x86_64.qcow2 \
  root@192.168.10.120:/var/lib/metalkit/images/
```

### 7. 在 MetalKit webui 里测试新镜像

装机时镜像下拉选 `Rocky-10-GenericCloud-metalkit`，应该一次装成功，不需要任何 live 救援。

---

## 各发行版预制清单

| 发行版 | 是否需要预制 | 关键操作 |
|--------|-------------|---------|
| Rocky 8 GenericCloud | 不强制 | 自带 megaraid_sas + vfat，SELinux 可保持 enforcing |
| Rocky 9 GenericCloud | 不强制 | 同上 |
| **Rocky 10 GenericCloud** | **必须** | dnf install kernel-modules + 重建 initramfs (force-drivers) + SELinux=disabled + 关 kdump |
| RHEL 8/9 GenericCloud | 不强制 | 同 Rocky 8/9 |
| RHEL 10 GenericCloud | 必须 | 同 Rocky 10 |
| Ubuntu 24.04+ cloudimg | 不强制 | 自带完整驱动 |
| Debian 12 generic | 不强制 | 自带完整驱动 |
| openEuler 24.03 | 不强制 | 自带完整驱动 |

判断一个新镜像是否需要预制：在物理机上装一次，如果进 dracut emergency 报 `unknown filesystem` 或 `/dev/disk/by-uuid/... does not exist`，就是缺驱动，需要预制。

---

## 离线修改（方案 B：qemu-nbd）

不需要启动 VM，适合批量预制。

```bash
# 1. 加载 nbd 模块
modprobe nbd max_part=8

# 2. 连接 qcow2 到 /dev/nbd0
qemu-nbd --connect=/dev/nbd0 Rocky-10-GenericCloud.latest.x86_64.qcow2

# 3. 看 partition table
fdisk -l /dev/nbd0

# 4. 挂载根分区（Rocky cloud image 是分区 1，含 LVM 或直接 xfs）
# 如果是直接 xfs：
mount /dev/nbd0p1 /mnt
# 如果是 LVM：
vgscan
vgchange -ay
mount /dev/rl/root /mnt

# 5. 挂载 boot / efi（如果有）
mount /dev/nbd0p2 /mnt/boot 2>/dev/null
mount /dev/nbd0p1 /mnt/boot/efi 2>/dev/null

# 6. bind mount + chroot
mount --bind /dev /mnt/dev
mount --bind /proc /mnt/proc
mount --bind /sys /mnt/sys
cp /etc/resolv.conf /mnt/etc/resolv.conf
chroot /mnt

# 7. 在 chroot 内执行和方案 A 第 4 步相同的预制命令
# （dnf install / dracut / sed / systemctl 等）

# 8. 退出 chroot，卸载
exit
umount /mnt/{dev,proc,sys,boot/efi,boot,}
qemu-nbd --disconnect /dev/nbd0

# 9. 镜像已就地修改，直接上传
```

注意：方案 B 不能用 `systemctl disable --now kdump` 的 `--now`（chroot 没跑 systemd），用 `systemctl disable kdump` + 手动 `rm /etc/systemd/system/multi-user.target.wants/kdump.service` 即可。

---

## 验证预制效果

预制好的镜像应该满足：

```bash
# 挂载镜像根分区到 /mnt 后
KVER=$(ls /mnt/lib/modules | sort -V | tail -1)

# 1. 关键驱动模块文件存在
ls /mnt/lib/modules/$KVER/kernel/drivers/scsi/megaraid/megaraid_sas.ko.xz
ls /mnt/lib/modules/$KVER/kernel/fs/fat/vfat.ko.xz

# 2. initramfs 内含关键驱动
lsinitrd /mnt/boot/initramfs-$KVER.img | grep -E 'megaraid_sas|vfat'

# 3. SELinux 配置
grep '^SELINUX' /mnt/etc/selinux/config  # 应为 disabled

# 4. kdump 已禁用
ls /mnt/etc/systemd/system/kdump.service  # 应为 mask 软链 → /dev/null
# 或不在 multi-user.target.wants 里
ls /mnt/etc/systemd/system/multi-user.target.wants/kdump.service 2>&1  # No such file

# 5. GRUB cmdline 不含 console=ttyS0
grep GRUB_CMDLINE_LINUX_DEFAULT /mnt/etc/default/grub  # 应含 console=tty0

# 6. BLS entry 存在
ls /mnt/boot/loader/entries/*.conf

# 7. root 密码已设
grep '^root:' /mnt/etc/shadow | cut -d: -f2  # 应为非 ! 开头的哈希
```

---

## installer 端的兜底逻辑

即使镜像已经预制，installer 仍会做以下兜底（详见 [internal/installer/grub.go](./internal/installer/grub.go) `regenerateInitramfsRHEL`）：

1. **检测关键驱动缺失**：`find /lib/modules/<KVER> -name megaraid_sas.ko*`，缺失则记录 warning
2. **dracut --force-drivers**：用 `--force-drivers` 而非 `--add-drivers`，确保 initramfs 启动时 insmod 关键驱动（vfat + RAID）
3. **SELinux=disabled**：如果镜像层未禁用，installer 兜底 `sed` 改 `/etc/selinux/config`
4. **mask kdump.service**：避免物理机上 kdump 启动失败触发 emergency

这些兜底逻辑对已预制的镜像是幂等的（不会重复做无用功），对未预制的镜像能让装机成功。
