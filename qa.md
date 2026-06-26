# MetalKit 装机问题与解决方案 (QA)

记录 MetalKit 在物理服务器（Dell PowerEdge R630, 4 NIC eth0-eth3, PERC RAID）上安装各 Linux 发行版时遇到的实际问题、根因、临时修复与永久修复。

---

## 1. Rocky 8 bond 配置生成虚拟 slave-0/slave-1 网卡

**现象**：装好后 `ip a` 看到 `slave-0`、`slave-1` 这两个虚拟接口，而不是真正 enslavement 到 eth0/eth1/eth2/eth3。

**根因**：`renderNetworkConfigBond` 把逻辑名 `slave-0`/`slave-1` 当作 YAML key 写入 cloud-init network-config v2。cloud-init 据此创建虚拟接口，而不是把真实物理 NIC 加入 bond。

**修复**：`renderNetworkConfigBond` 改为通过 MAC 地址解析出真实 NIC 名（eth0/eth1/...）作为 YAML key。

**涉及文件**：`internal/installer/seed.go`

---

## 2. NM keyfile + bond 组合下 cloud-init 生成重复 keyfile

**现象**：装好后 bond 不工作，`nmcli` 看到 bond0 配置了但 slave 没绑上，并且 `interface-name=eno3` 等指向 live 系统的 NIC 名（与装好系统不一致）。

**根因**：cloud-init 的 NetworkManager 渲染器会基于 network-config v2 重新生成一份 bond keyfile，与我们手写的 NM keyfile 重复冲突。`interface-name=` 字段引用的是 live 系统的 NIC 名（eno1/eno3），但装好系统后内核 cmdline 有 `net.ifnames=0`，名字变成了 eth0/eth3 → 匹配不上。

**修复**：
- 引入 `bypassCloudInitNetwork` 标志：renderer=NM + bond 配置时跳过写 `network-config`，并写 `97-metalkit-no-network.cfg` 禁用 cloud-init 的网络渲染
- slave keyfile 去掉 `interface-name=` 行，文件名改为 `bond0-slave-N.nmconnection`，只靠 `mac-address=` 匹配

**涉及文件**：`internal/installer/seed.go`

---

## 3. Rocky 9 重装后启动到旧 Ubuntu

**现象**：Rocky 9 装完后重启，引导的还是之前盘上的旧 Ubuntu，不是新装的 Rocky 9。

**根因**：UEFI NVRAM 里有同 label 的旧 boot entry，`efibootmgr` 它指向旧 PARTUUID。我们旧代码做 idempotency 检查 `efiEntryMatches` 命中后跳过 `--create`，但写入磁盘会改变 GPT PARTUUID → NVRAM 里的 entry 失效但没被替换。

**修复**：`registerEFIBootEntryRHEL` 在 `--create` 前先扫描 efibootmgr 输出，删除所有同 label 的旧 entry（`efibootmgr -b <idx> -B`），再创建新的。

**涉及文件**：`internal/installer/grub.go` (`removeStaleEFIEntries`)

---

## 4. Rocky 10 卡在 Plymouth 企鹅动画（真实是 dracut emergency）

**现象**：Rocky 10 装完重启后，屏幕卡在多个企鹅图标的 Plymouth 启动画面，按 Esc 没反应。改 GRUB cmdline 去掉 `console=ttyS0,115200n8` 并加 `nomodeset` 后看到真实日志：dracut 报 `Warning: /dev/disk/by-uuid/01582c00-... does not exist`，进 dracut emergency shell。

**根因**：Rocky 10 GenericCloud 镜像默认只装 `kernel-core` + `kernel-modules-core`，**不装 `kernel-modules`**，导致 `megaraid_sas.ko`（Dell PERC H730 RAID 控制器驱动）不在 `/lib/modules/<KVER>/` 里。我们 `dracut --add-drivers megaraid_sas` 看似成功 exit 0，但实际驱动文件不存在 → initramfs 里是空的 → 内核找不到根分区磁盘 → emergency。

**为什么 8/9 行、10 不行**：R8/R9 GenericCloud 自带 `kernel-modules` 包，含 megaraid_sas。R10 cloud image 精简策略把老一代 RAID 驱动从 core 包挪到完整 `kernel-modules` 包里，而 GenericCloud 默认不装该包。RHEL 10 release notes 把 `megaraid_sas`、`mpt3sas`、`hpsa` 等老驱动列为 unmaintained，cloud image 进一步精简。

**临时修复（live 救援）**：
1. 进 live，挂载根分区 `/dev/sdc4` + boot `/dev/sdc3` + ESP `/dev/sdc2`
2. chroot `dnf install -y kernel-modules kernel-modules-extra`（拉新内核 `6.12.0-211.22.1.el10_2`，带 megaraid_sas.ko）
3. 手动补 `/boot/vmlinuz-<NEW>`、`System.map`、`config`、`symvers`（dnf 装时 chroot 里 /boot 没挂载，kernel-core 的 scriptlet 把文件落到了被覆盖的目录）
4. `dracut --no-hostonly --force --add-drivers "megaraid_sas mpt3sas hpsa aacraid smartpqi nvme" /boot/initramfs-<NEW>.img <NEW>` 重建新内核 initramfs
5. 手写 BLS entry `/boot/loader/entries/<UUID>-<NEW>.conf`（kernel-core scriptlet 漏生成）
6. `grub2-set-default <NEW>` 设置新内核为默认
7. `umount` + `sync` + 重启

**永久修复（已实施）**：installer 的 `regenerateInitramfsRHEL` 在重建 initramfs 前先调用 `ensureKernelModulesInstalled`，检测 `/lib/modules/<KVER>/` 下 `megaraid_sas.ko` / `vfat.ko` 缺失时自动 `chroot dnf install -y kernel-modules kernel-modules-extra`。dracut 用 `--force-drivers "megaraid_sas mpt3sas hpsa aacraid smartpqi nvme vfat fat"` 强制注入。

镜像层预制方案见 [IMAGE-PREP.md](./IMAGE-PREP.md) —— 推荐做法是预制一个本地化 qcow2（在 VM 里装好 kernel-modules + 关 SELinux + 关 kdump + 重建 initramfs），上传到 MetalKit。installer 端的兜底逻辑对已预制的镜像是幂等的。

**涉及文件**：`internal/installer/grub.go` (`regenerateInitramfsRHEL`, `ensureKernelModulesInstalled`)

---

## 5. Rocky 10 进入 emergency mode: kdump.service 失败 + root account locked

**现象**：修完 RAID 驱动问题后重启，cloud-init 起来、网络起来，但 `kdump.service` 起来失败 → 进 emergency mode。屏幕显示 `Cannot open access to console, the root account is locked. See sulogin(8) man page for more details.` 按 Enter 无法进入 shell，只能反复 reload 配置。

**根因**：
- kdump 在 cloud image 里默认 enable，但它配置的 crashkernel 内存预留/kdump initramfs 在物理机上没准备好 → 起动失败
- Rocky cloud image root 默认密码锁定（`!`），emergency mode 的 sulogin 拒绝登录 → 用户彻底卡死

**修复（live 救援）**：
1. 进 live，挂载根分区
2. `chroot /mnt systemctl mask kdump.service` —— 彻底禁用 kdump
3. `chroot /mnt passwd -u root` + `echo 'root:metalkit' | chroot /mnt chpasswd` —— 解锁 root，设置密码
4. （可选）`chroot /mnt systemctl mask systemd-emergency.service` 避免类似卡死
5. umount + sync + 重启

**永久修复（已实施）**：installer 的 `installGRUBChrootRHEL` 末尾调用 `maskKdumpService`，在目标 rootfs 创建 `/etc/systemd/system/kdump.service → /dev/null` 软链，彻底禁用 kdump。同时清理 `multi-user.target.wants/kdump.service` 等启动勾子。

**涉及文件**：`internal/installer/grub.go` (`maskKdumpService`)

---

## 6. Rocky 10 进 emergency mode: /boot/efi 挂载失败 (unknown filesystem vfat)

**现象**：修复完 #4 #5 后重启，cloud-init / 网络都起来了，但仍进 emergency mode，root locked 无法登录。

**根因**：journalctl 显示 `boot-efi.mount: Failed with result 'exit-code'` → `mount: /boot/efi: unknown filesystem type 'vfat'` → `Dependency failed for local-fs.target` → emergency。

Rocky 10 cloud image 的 initramfs 默认不挂 ESP，且没有把 `vfat.ko` 加载到内核。systemd 切到 rootfs 后试图 mount `/boot/efi` (vfat)，但内核里没有 vfat 模块 → mount 失败 → local-fs.target 失败 → emergency。同样属于 cloud image 精简导致模块缺失的一类问题。

**修复（live 救援）**：
1. 在 `/etc/modules-load.d/vfat.conf` 写入 `vfat` 和 `fat`，让 systemd-modules-load 启动时自动加载
2. `dracut --no-hostonly --force --add-drivers "megaraid_sas mpt3sas hpsa aacraid smartpqi nvme vfat fat"` 重建 initramfs（双保险）
3. 验证 `lsinitrd /boot/initramfs-*.img | grep vfat` 看到 vfat.ko.xz

**永久修复（已实施）**：installer 的 `regenerateInitramfsRHEL` 改用 `--force-drivers` 而非 `--add-drivers`，强制把 `vfat fat` 加入 initramfs 启动时的 insmod 列表（dracut 写 `etc/cmdline.d/20-force_drivers.conf`）。同时 `ensureKernelModulesInstalled` 检测 megaraid_sas/vfat 模块缺失时自动 `dnf install kernel-modules kernel-modules-extra`。

**涉及文件**：`internal/installer/grub.go` (`regenerateInitramfsRHEL`, `ensureKernelModulesInstalled`)

---

## 7. SELinux 拒绝读取我们写入的配置文件（ Permission denied / unlabeled context）

**现象**：在 Rocky 10 emergency 模式下，journal 显示 `systemd-modules-load[1302]: Failed to open /etc/modules-load.d/vfat.conf: Permission denied`，虽然文件存在且权限正常。同时 NetworkManager 不读取我们写的 `bond0-slave-*.nmconnection` —— bond 网卡起不来。

**根因**：我们写入的所有配置文件（NM keyfile、sshd_config.d、cloud.cfg.d、modules-load.d 等）在文件系统上 SELinux 上下文是 `?`（unlabeled）。SELinux=enforcing 模式下，对应服务（NetworkManager、systemd-modules-load、sshd）的进程上下文不允许读取 unlabeled 文件 → 拒绝。`ls -laZ` 看到文件第二列上下文是 `?` 就是这个问题。

**修复（live 救援）**：
1. **彻底禁用 SELinux**（最终采用）：`sed -i 's/^SELINUX=.*/SELINUX=disabled/' /etc/selinux/config`。Rocky 10 cloud image 装到物理机后，我们写入大量配置文件（NM keyfile / cloud-init cfg / sshd / fstab / BLS entries / grubenv 等），逐个维护 SELinux 上下文成本高、易遗漏。直接禁用 SELinux 是最稳妥的做法，避免 unlabeled 文件触发 Permission denied → 服务启动失败 → emergency 的连锁反应。
2. （备选）保留 SELinux=enforcing，但用 `setfiles` 修复上下文：`chroot /mnt /usr/sbin/setfiles -F -c /etc/selinux/targeted/policy/policy.35 /etc/selinux/targeted/contexts/files/file_contexts /etc /var/lib/cloud /root`
   - 注意：live 系统不一定装了 setfiles；要用 chroot 内的 `/usr/sbin/setfiles`
   - 注意：`setfiles -r /` 在 chroot 内会报 `invalid alt_rootpath`，去掉 `-r` 参数
3. （备选）`touch /.autorelabel` 触发下次启动做完整 relabel（耗时长，大磁盘可能 30+ 分钟，Plymouth 不显示进度看起来像卡死）

**永久修复（已实施）**：installer 在 `installGRUBChrootRHEL` 末尾调用 `disableSELinuxIfEnforcing`，把 `/etc/selinux/config` 里 `SELINUX=enforcing` 改为 `SELINUX=disabled`。这样 installer 写入的所有配置文件都不需要管 SELinux 上下文，避免了 #7 #8 类问题。`BuildSeed` 里的 `restorecon` + `/.autorelabel` 逻辑在 SELinux=disabled 时自动跳过（通过 `selinuxWillBeDisabled` 检测），避免触发全盘 relabel 导致 Plymouth 假死（#4）。如果用户后续想开 SELinux，可手动改 enforcing + `touch /.autorelabel` + 重启。

详见 `internal/installer/grub.go` (`disableSELinuxIfEnforcing`, `maskKdumpService`) 和 `internal/installer/seed.go` (`selinuxWillBeDisabled`)。镜像层预制方案见 [IMAGE-PREP.md](./IMAGE-PREP.md)。

**涉及文件**：`internal/installer/seed.go` 或新增 `internal/installer/selinux.go`

---

## 8. /etc/modules-load.d 加载 vfat 策略在 SELinux 下不可靠

**现象**：在 `/etc/modules-load.d/vfat.conf` 写 `vfat`+`fat` 试图让 systemd-modules-load 启动时加载 vfat（修复 #6 的 /boot/efi 挂载失败），但 SELinux 拒绝读取这个文件 → vfat 没加载 → /boot/efi 仍然挂载失败。

**根因**：systemd-modules-load 是 systemd PID 1 的子进程，SELinux 上下文受 init_t 限制，新建的 `/etc/modules-load.d/vfat.conf` unlabeled → 拒绝访问。

**修复（live 救援）**：删掉 `/etc/modules-load.d/vfat.conf`，改用 dracut `--force-drivers` 让 vfat 在 initramfs 阶段就加载（dracut 写 `etc/cmdline.d/20-force_drivers.conf`，dracut-initqueue 启动时 insmod）：

```bash
chroot /mnt dracut --no-hostonly --force \
  --force-drivers "megaraid_sas mpt3sas hpsa aacraid smartpqi nvme vfat fat" \
  --add-drivers "megaraid_sas mpt3sas hpsa aacraid smartpqi nvme vfat fat" \
  /boot/initramfs-<KVER>.img <KVER>
```

**永久修复（待实施）**：installer 在 `regenerateInitramfsRHEL` 把 `--force-drivers` 用于关键启动驱动（RAID + vfat），`--add-drivers` 仅追加。`--force-drivers` 在 initramfs 里强制 insmod，不依赖 rootfs 的 systemd-modules-load。

**涉及文件**：`internal/installer/grub.go` (`regenerateInitramfsRHEL`)

---

## 9. Rocky 10 GRUB cmdline 默认 `console=ttyS0` 导致本地屏幕只看到 Plymouth 动画

### 现象

装机完成、重启后屏幕一直停在多个企鹅图标的 Plymouth 启动动画，看不到任何 dracut / systemd 日志。即使后面 emergency mode 进了，也无法在本地屏幕定位问题。手动编辑 GRUB 把 `console=ttyS0,115200n8` 改成 `console=tty0 nomodeset` 才看到真正的启动日志。

### 根因

Rocky/RHEL GenericCloud 云镜像默认 GRUB_CMDLINE_LINUX_DEFAULT 里有 `console=ttyS0,115200n8`，把内核日志全部重定向到串口。物理服务器本地没有串口终端，屏幕只显示 Plymouth 动画。`nomodeset` 也是必要的：某些显卡 (Matrox G200e 在 Dell iDRAC 上) 加载 drm 驱动后会黑屏，nomodeset 阻止加载 drm 驱动。

### 修复（installer 代码层）

`internal/installer/grub.go` 的 `fixRHELGrubCmdline`：

1. 读 `/etc/default/grub`，把 `GRUB_CMDLINE_LINUX_DEFAULT` 里的 `console=ttyS0,115200n8` 替换为 `console=tty0 nomodeset`
2. 调 `chroot <root> grubby --update-kernel=ALL --args="console=tty0 nomodeset" --remove-args="console=ttyS0,115200n8"` 把变更同步到所有 BLS entry
3. grubby 不可用 / 失败时，fallback 到 `fixBLSEntriesCmdline`：用 `find /boot/loader/entries/*.conf` 列出 BLS 文件，直接 sed 替换 `options` 行里的 console 参数

调用链：`InstallGRUB → installGRUBChrootRHEL → fixRHELGrubCmdline`。

### 修复（镜像层）

预制基础镜像时直接改 `/etc/default/grub`，详见 `IMAGE-PREP.md`。

---

## 10. `dnf install kernel-modules` 拉新内核，老内核 initramfs 仍缺 megaraid_sas → switch-root 失败

### 现象

装机日志显示 dracut 成功为两个内核都重建了 initramfs：

```
warn  regenerate-initramfs: driver MISSING from image [driver=megaraid_sas kernel=6.12.0-211.16.1.el10_2.0.1.x86_64]
info  regenerate-initramfs: dracut succeeded [kernel=6.12.0-211.16.1.el10_2.0.1.x86_64]
info  regenerate-initramfs: driver present [driver=megaraid_sas .../6.12.0-211.22.1.el10_2.x86_64/...]
info  regenerate-initramfs: dracut succeeded [kernel=6.12.0-211.22.1.el10_2.x86_64]
```

但重启后仍然卡在：

```
[FAILED] Failed to start initrd-switch-root.service - Switch Root.
Generating "/run/initramfs/rdsosreport.txt"
Entering emergency mode.
```

### 根因

`dnf install -y kernel-modules kernel-modules-extra`（不带版本号）解析到 repo 里**最新**的 kernel-modules 包，dnf 自动把匹配的**新内核**也拉了进来（`6.12.0-211.22.1.el10_2.x86_64`），并只为新内核装了 megaraid_sas。

老内核 `6.12.0-211.16.1.el10_2.0.1.x86_64` 的 `/lib/modules/<老 kver>/` 下仍然没有 megaraid_sas.ko.xz。我们调用 `dracut --force-drivers megaraid_sas` 时，dracut 找不到对应 .ko 文件就**静默跳过**——老内核 initramfs 仍然没有 megaraid_sas。

但 **GRUB 默认仍然是老内核**：dnf 装新内核不会修改 `/etc/sysconfig/kernel` 的 `DEFAULTKERNEL`，也不会改 grubenv 的 `saved_entry`。所以系统启动老内核 → initramfs 没 megaraid_sas → 找不到 RAID 根盘 → switch-root 失败 → emergency mode。

### 修复（installer 代码层）

`internal/installer/grub.go`：

1. **`ensureKernelModulesInstalled`** —— dnf 装包前先用 `listKernelVersions` 探测已存在的内核，只有 1 个时传**带版本号的包名** `kernel-modules-<kver> kernel-modules-extra-<kver>`，避免拉新内核。失败再 fallback 到无版本号。
2. **`setDefaultBootKernelToDriverComplete`** —— dracut 跑完后扫描所有 `/lib/modules/<kver>/`，给每个内核打"关键驱动齐全度"分数（megaraid_sas/mpt3sas/hpsa/aacraid/smartpqi/vfat 共 6 个），分数最高（并列取更新版本）的那个用 `grubby --set-default=/boot/vmlinuz-<kver>` 设为默认启动项。grubby 不可用时直接改 grubenv 的 `saved_entry`。

调用链：`InstallGRUB → installGRUBChrootRHEL → SELinux disabled → setDefaultBootKernelToDriverComplete → fixRHELGrubCmdline → maskKdumpService`。

### 修复（镜像层）

跟 #8 一样，预制基础镜像时直接装完整内核驱动包，避免运行时 dnf 拉新内核。详见 `IMAGE-PREP.md`。

---



**现象**：Rocky cloud image 默认 `/etc/default/grub` 的 `GRUB_CMDLINE_LINUX_DEFAULT` 含 `console=ttyS0,115200n8`，把所有内核日志输出到串口，本地屏幕只剩 Plymouth 企鹅动画，按 Esc 无反应。调试时必须改 cmdline。

**修复（live 救援）**：所有 BLS entry (`/boot/loader/entries/*.conf`) 的 `options` 行把 `console=ttyS0,115200n8` 替换为 `console=tty0 nomodeset`，让日志显示在本地屏幕且禁用图形模式（避免显卡驱动卡 Plymouth）。

```bash
for f in /mnt/boot/loader/entries/*.conf; do
  sed -i 's|console=ttyS0,115200n8|console=tty0 nomodeset|g' "$f"
done
```

**永久修复（待实施）**：installer 安装 RHEL family 后把 `/etc/default/grub` 的 `GRUB_CMDLINE_LINUX_DEFAULT` 改为 `console=tty0 nomodeset no_timer_check crashkernel=...`（或同时保留 ttyS0 + tty0），然后重新生成 grub.cfg / BLS entries。Rocky 10 用 BLS 模式，修改 `grub.cfg` 不够，得修改 BLS entry 文件，最稳是改 `/etc/default/grub` 然后重新跑 `grub2-mkconfig -o /boot/grub2/grub.cfg`（虽然 10_linux 是空，BLS entry 的 options 字段从 `GRUB_CMDLINE_LINUX_DEFAULT` + `grubby --update-kernel` 同步）。

**涉及文件**：`internal/installer/grub.go` 或 `internal/installer/seed.go`（写入 /etc/default/grub 的逻辑）

---


### 如何看 Plymouth 遮盖下的真实启动日志

Rocky/RHEL cloud image 默认 GRUB cmdline 含 `console=ttyS0,115200n8`，把日志全部输出到串口，本地屏幕只剩 Plymouth 动画。调试时：

1. GRUB 菜单按 `e` 编辑 entry
2. 删掉 `console=ttyS0,115200n8`（让日志显示在本地屏幕）
3. 删掉 `rhgb quiet`（如果有）
4. 末尾加 `nomodeset`（避免显卡驱动卡 Plymouth）
5. `Ctrl+X` 启动

### 进入 emergency mode 但 root locked 怎么办

cloud image 默认 root 锁定，emergency mode 进不去 shell。两种解法：

- **live 救援**：PXE 进 MetalKit live，挂载根分区，`passwd -u root` + `chpasswd` 设密码
- **GRUB cmdline**：加 `systemd.unit=rescue.target` 或 `init=/bin/sh` 绕过 sulogin

### 重新生成 Rocky 10 initramfs 的正确步骤

```bash
# 在 live 系统，挂载目标根分区到 /mnt，boot 到 /mnt/boot，ESP 到 /mnt/boot/efi
mount --bind /dev /mnt/dev
mount --bind /proc /mnt/proc
mount --bind /sys /mnt/sys
cp /etc/resolv.conf /mnt/etc/resolv.conf

# 装完整内核驱动包（cloud image 缺）
chroot /mnt dnf install -y kernel-modules kernel-modules-extra

# 补 vmlinuz/System.map 到 /boot（dnf 装 kernel-core 时 chroot /boot 未挂载会漏）
KVER=$(ls /mnt/lib/modules | sort -V | tail -1)
cp /mnt/lib/modules/$KVER/vmlinuz /mnt/boot/vmlinuz-$KVER
cp /mnt/lib/modules/$KVER/System.map /mnt/boot/System.map-$KVER
cp /mnt/lib/modules/$KVER/config /mnt/boot/config-$KVER
cp /mnt/lib/modules/$KVER/symvers.xz /mnt/boot/symvers-$KVER.xz

# 重建 initramfs，强制注入 RAID 驱动
chroot /mnt dracut --no-hostonly --force \
  --add-drivers "megaraid_sas mpt3sas hpsa aacraid smartpqi nvme" \
  /boot/initramfs-$KVER.img $KVER

# 写 BLS entry（kernel-core scriptlet 漏生成）
UUID=$(ls /mnt/boot/loader/entries/ | grep -oP '^[a-f0-9]+' | head -1)
cat > /mnt/boot/loader/entries/${UUID}-${KVER}.conf <<EOF
title Rocky Linux ($KVER) 10.2 (Red Quartz)
version $KVER
linux /vmlinuz-$KVER
initrd /initramfs-$KVER.img
options console=ttyS0,115200n8 no_timer_check crashkernel=1G-4G:192M,4G-64G:256M,64G-:512M root=UUID=$(blkid -s UUID -o value /dev/sdc4)
grub_users \$grub_users
grub_arg --unrestricted
grub_class rocky
EOF

# 设置默认内核
chroot /mnt grub2-set-default $KVER
```

---

## 11. Debian 13/14 cloud kernel 缺 megaraid_sas → 启动卡在 "Run /init as init process"

### 现象

Debian 13 (trixie) / 14 genericcloud 镜像安装完成后重启，内核日志停在：

```
[   10.145237] Run /init as init process
[   10.377085] SCSI subsystem initialized
[   10.435244] ahci 0000:00:11.4: SSS flag set, parallel bus scan disabled
...
[   10.950291] ata1: SATA link down (SStatus 0 SControl 300)
[   11.205186] ata5: SATA link down (SStatus 0 SControl 300)
... (所有 10 个 ata 端口都 link down，之后无限挂起)
```

只有 ahci（板载 SATA），看不到任何 RAID 控制器（PERC H730 / megaraid_sas）。Debian 12 正常启动。

### 根因

Debian 13 起的 genericcloud 镜像默认用 `-cloud-amd64` 内核（如 `7.0.12+deb14.1-cloud-amd64`），为缩减镜像体积**剥掉了所有硬件 RAID/HBA 驱动**：`megaraid_sas`、`mpt3sas`、`hpsa`、`aacraid`、`smartpqi` 都不在 `/lib/modules/<kver>/` 下。

Dell R630 的 PERC H730 Mini 需要 `megaraid_sas`。cloud kernel 找不到根盘 → init 挂起。Debian 12 的 cloud kernel 仍带这些驱动，所以不受影响。

### 修复（installer 代码层）

`internal/installer/grub.go`：

1. **`regenerateInitramfsDebian`** —— 检测 `/lib/modules/<kver>/` 是否缺 `megaraid_sas` 等 5 个 critical 驱动；缺则写 `resolv.conf`、切 apt 镜像到 TUNA、`apt-get install -y linux-image-amd64`（拉标准内核，带完整 SCSI 驱动集）、`update-initramfs -u -k all`。
2. **`pinDebianDefaultKernel`** —— 跑 `chroot update-grub` 把新内核 menuentry 写进 grub.cfg；`/etc/default/grub` 设 `GRUB_DEFAULT=saved`；解析 grub.cfg 找到标准内核 menuentry 的 **ID**（不是 title！见 #13），用 `grub-set-default <id>` 写 grubenv。

调用链：`InstallGRUB → installGRUBHostDebian → regenerateInitramfsDebian → purgeCloudKernelDebian + pinDebianDefaultKernel`。

---

## 12. Debian cloud kernel 文件不在 dpkg 管理下，apt purge 不删文件 → GRUB 仍选 cloud kernel

### 现象

`regenerateInitramfsDebian` 装了 `linux-image-amd64`，`purgeCloudKernelDebian` 也跑了 `apt-get purge linux-image-<cloud-kver>`，日志显示 `purged driver-stripped cloud kernels: 7.0.12+deb14.1-cloud-amd64`。但重启后启动的还是 cloud kernel：

```
Kernel panic - not syncing: VFS: Unable to mount root fs on unknown-block(0,0)
...
Comm: swapper/0 Not tainted 7.0.12+deb14.1-cloud-amd64 #1
```

### 根因

Debian genericcloud 镜像把 cloud kernel 文件**直接烤进镜像**（`/boot/vmlinuz-<cloud-kver>`、`/boot/initrd.img-<cloud-kver>`、`/lib/modules/<cloud-kver>/`），**没有对应的 dpkg 包元数据**。

`apt-get purge linux-image-<cloud-kver>` 报 `Unable to locate package` 退出 100，dpkg 数据库里查不到这个包，但文件留在 /boot。`update-grub` 扫描 /boot 还能看到 vmlinuz 文件，grub.cfg 里仍然生成 cloud kernel menuentry。

GRUB 默认按 menuentry 顺序选第一个，cloud kernel 排在标准 kernel 前面（cloud image 的 `/etc/grub.d/10_linux` 把 cloud kernel 当作主内核生成 simplified entry），所以又启动了 cloud kernel。

### 修复

`purgeCloudKernelDebian`（`internal/installer/grub.go`）在 apt purge 后**手动 rm 文件**：

```go
// apt purge（清 dpkg 元数据，best-effort，容忍 "Unable to locate package"）
for _, pkg := range []string{
    "linux-image-" + kver,
    "linux-modules-" + kver,
    "linux-modules-extra-" + kver,
} {
    chroot mntRoot apt-get purge -y pkg  // 容忍 missing
}

// 手动 rm /boot/* 和 /lib/modules/<kver>/（删 dpkg 不管的文件）
for _, rel := range []string{
    "boot/vmlinuz-" + kver,
    "boot/initrd.img-" + kver,
    "boot/System.map-" + kver,
    "boot/config-" + kver,
} {
    FS.Remove(filepath.Join(mntRoot, rel))
}
Exec.Run("rm", "-rf", filepath.Join(mntRoot, "lib", "modules", kver))

// 再 update-grub，grub.cfg 只剩标准内核 menuentry
chroot mntRoot update-grub
```

**关键点**：只删包元数据没用，必须删 /boot 文件。`update-grub` 看 /boot 不看 dpkg 数据库。

---

## 13. GRUB `saved_entry` 匹配 menuentry ID，不是 title

### 现象

`pinDebianDefaultKernel` 设了 `GRUB_DEFAULT=saved`，用 `grub-set-default` 写入 `saved_entry`，日志显示：

```
GRUB saved_entry set to "Debian GNU/Linux, with Linux 6.12.90+deb13.1-amd64" (kernel 6.12.90+deb13.1-amd64, driver score 6/6)
```

但重启后 GRUB 仍然启动 cloud kernel（第一个 menuentry）。

### 根因

`grub-set-default <title>` 不会把 title 转换成 menuentry ID —— 它直接把参数写入 grubenv。但 GRUB 启动时 `load_env` 读 `saved_entry`，然后**按 menuentry ID 匹配**，不是按 title。Debian grub.cfg 里每个 menuentry 有显式 ID：

```
menuentry 'Debian GNU/Linux, with Linux 6.12.90+deb13.1-amd64' ... $menuentry_id_option 'gnulinux-6.12.90+deb13.1-amd64-advanced-<uuid>' {
```

`saved_entry="Debian GNU/Linux, with Linux 6.12.90+deb13.1-amd64"` 找不到任何 menuentry 的 ID 匹配 → GRUB fallback 到 `default=0` → cloud kernel。

### 修复

`findMenuentryIDForKernel`（`internal/installer/grub.go`）解析 grub.cfg，找到 `linux /boot/vmlinuz-<kver>` 所在 menuentry 行的 **ID**（`$menuentry_id_option '...'` 里单引号字符串），用 ID 调 `grub-set-default`：

```go
entryID := findMenuentryIDForKernel(deps, mntRoot, bestKver)
// entryID = "gnulinux-6.12.90+deb13.1-amd64-advanced-98f79480-..."
chroot mntRoot grub-set-default entryID
```

`extractMenuentryID` 函数提取 menuentry 行里 `$menuentry_id_option` 后第一个单引号字符串。

### 验证

```bash
chroot /mnt/rootfs grub-set-default 'gnulinux-6.12.90+deb13.1-amd64-advanced-<uuid>'
grep saved_entry /mnt/rootfs/boot/grub/grubenv
# saved_entry=gnulinux-6.12.90+deb13.1-amd64-advanced-<uuid>  ← 正确
```

---

## 14. Dell R630 NVIDIA Kepler GPU 持续刷 nouveau PRIVRING 错误

### 现象

装好 Debian 14 / openEuler / Rocky 等任何发行版后，控制台持续刷：

```
[  330.654082] nouveau 0000:81:00.0: bus: MMIO write of 80000067 FAULT at 10eb14 [ PRIVRING ]
[  331.059948] nouveau 0000:04:00.0: bus: MMIO write of 80000067 FAULT at 10eb14 [ PRIVRING ]
[  331.654701] nouveau 0000:04:00.0: bus: MMIO write of 80000067 FAULT at 10eb14 [ PRIVRING ]
... (每秒一条，两张卡交替)
```

### 根因

Dell R630 装了两张老 NVIDIA Kepler 架构 GPU（Tesla K 系列，0000:04:00.0 和 0000:81:00.0）。开源 `nouveau` 驱动对新 kernel 的 PRIVRING 寄存器访问实现与老 GPU 固件不兼容，每次访问失败刷一条错误。6.x / 7.x 内核都复现。

**这是控制台 spam，不是 boot 失败**。系统已经在运行（时间戳到几百秒，uptime 正常），只是日志被刷屏。

### 修复（临时 / live 救援）

```bash
# 临时禁用 nouveau modeset
echo "options nouveau modeset=0" > /etc/modprobe.d/nouveau.conf
# 或彻底 blacklist
echo "blacklist nouveau" > /etc/modprobe.d/blacklist-nouveau.conf
update-initramfs -u   # Debian/Ubuntu
dracut --force        # RHEL family
```

或 GRUB cmdline 加 `nouveau.modeset=0` / `nomodeset`。

### 修复（installer 代码层）

`fixRHELGrubCmdline`（`internal/installer/grub.go`）已经为 RHEL family 加 `nomodeset`。Debian/Ubuntu 路径暂未处理 —— 如需静默，在 `installGRUBHostDebian` 写 `/etc/default/grub` 时加 `nomodeset` 到 `GRUB_CMDLINE_LINUX_DEFAULT`。

---

## 15. 无网络环境下装 Debian 13/14 / Rocky 10 / openEuler：cloud kernel 缺驱动导致不可启动

### 现象

用户在无外网环境装 Debian 13/14（或 Rocky 10 / openEuler 剥了 kernel-modules 的镜像），`apt install linux-image-amd64`（或 `dnf install kernel-modules`）失败：

```
warn  linux-image-amd64 install failed: ... (output: ...) — initramfs may lack storage drivers
```

但安装 job 仍报 `succeeded`，重启后卡在 cloud kernel（缺 megaraid_sas）。

### 根因

`regenerateInitramfsDebian` / `ensureKernelModulesInstalled` 是 best-effort —— 失败只打 warn 不传播错误，job 继续。失败后 cloud kernel 还在，GRUB 选 cloud kernel，系统不可启动。

**但这个逻辑只对 RAID 盘有意义**：cloud kernel 缺的是 `megaraid_sas`/`mpt3sas`/`hpsa`/`aacraid`/`smartpqi` 这些 RAID/HBA 驱动。如果目标盘不在 RAID 控制器上（纯 SATA/NVMe/virtio），cloud kernel 的 `ahci`/`nvme`/`virtio_blk` 足以启动，根本不需要装标准内核。

旧代码不区分 RAID 盘和非 RAID 盘，对所有缺驱动的镜像都尝试装标准内核 —— 无网络时失败，留下不可启动的磁盘（其实非 RAID 盘本可以直接启动 cloud kernel）。

### 修复

`isRAIDControllerDisk`（`internal/installer/grub.go`）通过 sysfs 检测目标盘是否在 RAID 控制器上：

```go
// /sys/block/sda/device/driver -> /sys/bus/pci/drivers/megaraid_sas/
out, err := Exec.Run("readlink", "-f", "/sys/block/sda/device/driver")
driverName := filepath.Base(resolved)  // "megaraid_sas" / "ahci" / "nvme" / ...
```

匹配的 RAID 驱动：`megaraid_sas`, `mpt3sas`, `hpsa`, `aacraid`, `smartpqi`, `cciss`, `arcmsr`。

`regenerateInitramfsDebian` 和 `ensureKernelModulesInstalled` 在"cloud kernel 缺驱动"分支前加判断：

```go
if !isRAIDControllerDisk(ctx, deps, devPath) {
    // 非 RAID 盘：cloud kernel 的 ahci/nvme 足以启动，跳过装标准内核
    Reporter.Log("info", "skipped standard-kernel install: <dev> is not behind a RAID controller")
    return
}
// RAID 盘：必须装标准内核（否则 cloud kernel 缺 megaraid_sas 不可启动）
apt install linux-image-amd64  // 或 dnf install kernel-modules
```

### 效果

- **非 RAID 盘（SATA/NVMe）+ 无网络**：跳过装标准内核，cloud kernel 直接启动，安装成功
- **RAID 盘 + 有网络**：装标准内核，purge cloud kernel，启动标准内核（原逻辑）
- **RAID 盘 + 无网络**：装标准内核失败，warn 日志，系统不可启动（需预装镜像或网络）

### 涉及文件

`internal/installer/grub.go`：
- `isRAIDControllerDisk` 函数
- `regenerateInitramfsDebian` 签名加 `devPath`，调用前判断
- `ensureKernelModulesInstalled` 签名加 `devPath`，调用前判断
- `installGRUBHostDebian` / `installGRUBChrootRHEL` 传 `devPath`

---

## 16. apt-get purge 一次性给多个包，遇到 missing 包整个失败

### 现象

```go
chroot mntRoot apt-get purge -y linux-image-<kver> linux-modules-<kver> linux-modules-extra-<kver>
```

日志：

```
E: Unable to locate package linux-modules-6.12.90+deb13.1-cloud-amd64
E: Couldn't find any package by glob 'linux-modules-6.12.90+deb13.1-cloud-amd64'
...
exit status 100
```

结果 `linux-image-<kver>`（实际存在的包）也没被 purge。

### 根因

Debian 13/14 cloud kernel 只以 `linux-image-<cloud-kver>` 单包形式存在，**没有对应的 `linux-modules-<cloud-kver>` / `linux-modules-extra-<cloud-kver>`**（这些是标准内核才有的分包）。`apt-get purge` 一次给多个包名时，任何一个找不到就整体 exit 100，不执行任何 purge。

### 修复

`purgeCloudKernelDebian` 改为**逐个包 purge**，容忍 "Unable to locate package" / "is not installed"：

```go
for _, pkg := range []string{
    "linux-image-" + kver,
    "linux-modules-" + kver,
    "linux-modules-extra-" + kver,
} {
    out, err := chroot mntRoot apt-get purge -y pkg
    if err != nil {
        combined := out + err
        if strings.Contains(combined, "Unable to locate package") ||
           strings.Contains(combined, "is not installed") {
            continue  // 包不存在，跳过
        }
        // 真错误才告警
    }
}
```

配合 #12 的手动 rm 文件，无论包是否存在都能彻底清理 cloud kernel。

---

## 17. ESP 嵌套目录 efi/EFI/<id>/ 导致 Boot Failed

### 现象

openEuler 装完后重启，iDRAC virtual console 显示 `Boot Failed`，循环 PXE / NIC 启动，进不到磁盘。

### 根因

某些发行版的 `grub-install` 把 ESP 目录结构嵌套了一层：`esp/efi/EFI/<bootID>/grubx64.efi`（多了一层 `efi/`）。但 UEFI 固件按 NVRAM 里的路径 `\EFI\<bootID>\shimx64.efi` 查找，FAT32 虽然大小写不敏感但**路径层级必须匹配**，找不到 `\EFI\<bootID>\` 下的 loader 就 Boot Failed。

### 修复

`normalizeESPLayout`（`internal/installer/grub.go`）在 `grub-install` 后检查 `espMount/EFI/<bootID>/` 是否有 loader（`grubx64.efi` / `shimx64.efi` / `shim.efi`），没有就从 `espMount/efi/EFI/<bootID>/` 拷上来。同时处理 `BOOT/` fallback（`BOOTX64.EFI`）。

调用点：`installGRUBChrootRHEL` 的 UEFI 分支，在 `registerEFIBootEntryRHEL` 之后。

---

## 18. GPT PMBR size mismatch 导致 sfdisk 改 ESP 分区类型失败

### 现象

Debian 12 安装时 ESP 类型修复失败：

```
warn  fixing ESP partition type on /dev/sdb15: C12A7328-... -> EF00 (gpt table)
warn  fix ESP type: sfdisk --part-type failed, falling back to sgdisk -t
     [err=sfdisk --part-type /dev/sdb 15 EF00: exit status 1
     (output: sfdisk: /dev/sdb: partition 15: failed to set partition type)]
```

### 根因

把小容量 cloud image dd 到大容量物理盘后，GPT backup header 还在原 image 大小位置，不在磁盘末尾 → `GPT PMBR size mismatch`。sfdisk 检测到这个不一致就拒绝修改分区类型。

### 修复

`fixESPTypeIfWrong`（`internal/installer/esp.go`）对 GPT 盘先跑 `sgdisk -e`（relocate backup GPT + rewrite PMBR），再 partprobe，再 sfdisk。sfdisk 仍失败时 fallback 到 `sgdisk -t <n>:ef00`。

---

## 19. dd 写镜像后 lsblk 看不到分区 → "no partitions on /dev/sdX"

### 现象

Debian 12 安装 grow 阶段失败：

```
error install: no partitions on /dev/sda
```

### 根因

`qemu-img convert` 写完镜像后，内核分区表缓存还是旧的（空），`partprobe` 在某些情况下不够强制。`rootPartitionOf` 调 `lsblk` 看不到任何分区 → 报错。

### 修复

1. `WriteImage`（`internal/installer/qemuwrite.go`）在 partprobe 后加 `blockdev --rereadpt`（BLKRRPART ioctl）强制内核重读分区表。
2. `rootPartitionOf`（`internal/installer/grow.go`）首次 lsblk 看不到分区时，跑 `partprobe` + `blockdev --rereadpt` + `udevadm settle` 后重试一次。

---

## 20. NVRAM 残留旧 BootXXXX 条目，固件按 BootOrder 逐个尝试拖慢启动

### 现象

Dell R630 在多次安装不同发行版后，NVRAM 里堆积了一堆指向已不存在 PARTUUID 的 BootXXXX 条目。固件按 BootOrder 逐个尝试，每个都 Boot Failed 后才轮到正确条目，启动很慢且屏幕刷大量 Boot Failed。

### 根因

旧代码 `pruneStaleNVRAM` 用**安装前**采集的 PARTUUID 列表判断哪些条目 stale。但安装过程改写了分区表，安装前的 PARTUUID 列表本身也过期了 —— 该删的没删，该留的可能误删。

### 修复

`pruneStaleNVRAM`（`internal/installer/zap.go`）改为**白名单**策略，用**安装后**的当前分区表：

1. `collectPartUUIDs` 读当前盘所有分区的 PARTUUID
2. 遍历 NVRAM 条目：
   - MBR 条目（`HD(N,MBR,0x<sig>,...)`）—— 安装时 MBR boot sector 被 dd 覆盖，旧 sig 必失效，**总是删**
   - GPT 条目（`HD(N,GPT,<guid>,...)`）—— GUID 不在当前集合就删
   - 非 HD 条目（`VenHw` / `BBS` / `PciRoot`，NIC PXE 等）—— 无法判断指向，**保留**
3. lsblk 失败（无法采集当前集合）时，只删 MBR 条目，GPT 条目保守保留

调用点：`Run`（`internal/installer/installer.go`）的 UEFI 分支，在 `InstallGRUB` 之后、`umount` 之前。这样刚创建的新 BootXXXX 条目的 GUID 在当前集合里，不会被误删。

---

## 21. agent 上报 stage 失败：`SQLITE_BUSY (5) database is locked`

### 现象

装机 Job 跑到 `grub-install` 阶段时，agent 端日志报：

```
install: stage grub-install: agentclient: http 400: update stage: database is locked (5) (SQLITE_BUSY)
```

Web 平台 Job 详情页日志流也在此处中断，机器卡在中间状态。

### 根因

`internal/sqlitedb/sqlitedb.go` 的 `Open` 之前是这样应用 pragma 的：

```go
db, err := sql.Open("sqlite", opts.Path)
// ...
for _, p := range pragmas {
    db.ExecContext(ctx, p)  // PRAGMA busy_timeout = 5000 ...
}
```

`database/sql` 维护连接池，`db.ExecContext` 只对**当前 checkout 出来的那一个连接**执行 PRAGMA。`busy_timeout` 和 `foreign_keys` 是**每连接**设置（per-connection），不会自动传播到池里其他连接。

modernc.org/sqlite 驱动的连接池默认开多个连接。装机期间并发写：
- orchestrator 5s tick 改 binding / job 状态
- agent POST `/agent/jobs/{id}/stage` 改 job 当前 stage
- agent POST `/agent/jobs/{id}/log` 追加日志行（每秒可能多次）

任意两个写落在不同连接上，其中一个拿锁另一个立刻 `SQLITE_BUSY` —— 因为 `busy_timeout=0`（per-connection 默认值），驱动不会等待重试，直接返回错误。

### 修复

`internal/sqlitedb/sqlitedb.go` 改成把 pragma 通过 DSN query parameter 注入：

```go
func buildDSN(path string) string {
    q := url.Values{}
    q.Add("_pragma", "busy_timeout(30000)")
    q.Add("_pragma", "foreign_keys(on)")
    if path != ":memory:" {
        q.Add("_pragma", "journal_mode(WAL)")
    }
    return path + "?" + q.Encode()
}
```

modernc.org/sqlite 驱动在 checkout 每个新连接前会应用所有 `_pragma` query 参数，确保池里每个连接都带上 `busy_timeout=30000` + `foreign_keys=ON`。

`journal_mode=WAL` 是数据库级设置（持久化到文件头），单次 ExecContext 即可。但放进 DSN 也无害（驱动幂等处理），且能让从 rollback journal 模式的旧 DB 平滑切换。

`busy_timeout` 从 5000 提到 30000：装机日志写可以和 orchestrator 改 binding 的 transaction 撞上，5s 在慢盘上不够，30s 留足余量。

### 验证

- `TestOpenAppliesPragmas` 断言 `busy_timeout == 30000`
- 单测全部通过
- 部署后重新触发 openEuler 20.03 装机，stage 切换不再 400

---

## 部署 agent 到 live ISO 的流程

MetalKit 的安装逻辑跑在 agent 二进制里，agent 在 live ISO 的 squashfs 中。**修复 installer 代码后必须重建 squashfs 才能让修复生效**，仅替换 controller 不够。

```bash
# 1. 本地构建 agent
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o /tmp/metalkit-agent ./cmd/agent

# 2. 上传到 controller
sshpass -p 123 scp /tmp/metalkit-agent root@192.168.10.120:/tmp/

# 3. 在 controller 上解包 squashfs → 替换 agent → 重新打包
sshpass -p 123 ssh root@192.168.10.120 "
  cd /tmp && rm -rf sqfs-root && \
  unsquashfs -d sqfs-root /opt/metalkit/boot/filesystem.squashfs && \
  cp /tmp/metalkit-agent sqfs-root/usr/local/bin/metalkit-agent && \
  chmod +x sqfs-root/usr/local/bin/metalkit-agent && \
  rm -f /opt/metalkit/boot/filesystem.squashfs && \
  mksquashfs sqfs-root /opt/metalkit/boot/filesystem.squashfs -comp xz -noappend && \
  rm -rf sqfs-root
"
```

验证 agent md5 在 squashfs 内匹配：
```bash
unsquashfs -d /tmp/check /opt/metalkit/boot/filesystem.squashfs usr/local/bin/metalkit-agent
md5sum /tmp/check/usr/local/bin/metalkit-agent
```

---

## 测试的发行版支持矩阵

| 发行版 | 版本 | 状态 | 备注 |
|--------|------|------|------|
| Rocky Linux | 8.10 GenericCloud | OK | 需修复 #1 #2 |
| Rocky Linux | 9 GenericCloud | OK | 需修复 #3 |
| Rocky Linux | 10 GenericCloud | OK | 需修复 #4 #5 #8 #10 |
| Debian | 12 generic | OK | |
| Debian | 13 genericcloud | OK | 需修复 #11 #12 #13 #16（cloud kernel 缺 RAID 驱动）；非 RAID 盘无网络可装（#15） |
| Debian | 14 genericcloud | OK | 需修复 #11 #12 #13 #16（同上）；非 RAID 盘无网络可装（#15） |
| Ubuntu | 24.04 (noble) server cloudimg | OK | |
| Ubuntu | 26.04 (resolute) server cloudimg | OK | |
| openEuler | 20.03 LTS SP4 | OK | 需修复 #17（ESP 嵌套目录） |
| openEuler | 22.03 LTS SP4 | OK | 需修复 #17 |
| openEuler | 24.03 LTS SP3 | OK | |

### 通用问题（所有 UEFI 安装都受益）

- #18 GPT PMBR size mismatch → sgdisk -e + sfdisk fallback
- #19 dd 后 lsblk 看不到分区 → blockdev --rereadpt
- #20 NVRAM 残留旧条目 → 白名单 pruneStaleNVRAM
- #21 SQLITE_BUSY → 见对应章节
