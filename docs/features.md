# metalkit 功能清单

按"我能用 metalkit 做什么"逐项列出当前主线已经实现并跑通的功能。
每节标注实现的源码位置(`internal/<pkg>/`)和暴露给外部的入口(HTTP API / Web UI / CLI)。

> **范围**:M1(硬件纳管)+ M2(镜像/装机)+ M2.3 系列(子网、绑定、BMC、Job、auth、subnet 等)。
> 跨段(M3)和远程 console(M4)尚未实现。

---

## 1. PXE / 网络引导栈

> 一份 metalkit 实例独立承载完整的 PXE 引导链,不依赖外部 TFTP/HTTP 服务器。

| 功能 | 说明 | 模块 |
|---|---|---|
| **ProxyDHCP OFFER**(UDP 67) | 同段广播极简 OFFER(只带 `vendor-class-id` + `server-identifier`),不发 IP,避开 Dell PXE-E16 | `internal/dhcp/server.go` |
| **BSDP Layer 3**(UDP 4011) | Dell BIOS 单播 REQUEST → ACK 带 bootfile 路径 + `siaddr/sname` | `internal/dhcp/bsdp.go` |
| **架构区分** | BIOS / UEFI(amd64)按 `client-arch` 派不同 bootfile(`undionly.kpxe` / `snponly.efi`) | `internal/dhcp/` |
| **TFTP 服务**(UDP 69) | 内置 TFTP,直接发 `//go:embed` 进 binary 的 iPXE 资产,**无须外部 TFTP 目录** | `internal/tftp/` + `internal/ipxebin/` |
| **iPXE chain-load** | BIOS/UEFI → iPXE → HTTP 拉 `/boot/ipxe` 脚本 → 拉 kernel + initrd + squashfs | `internal/httpd/ipxe.go` |
| **HTTP boot 资产**(TCP 8080) | `/boot/vmlinuz` / `/boot/initrd.img` / `/boot/filesystem.squashfs` 高速发送 | `internal/httpd/server.go` |
| **kernel cmdline 注入** | iPXE 脚本里嵌 `metalkit.url=http://<serverIP>:<port>`,让 live 镜像里 agent 自动找回 | `internal/httpd/ipxe.go` |
| **dnsmasq 共存** | DHCP 67 用 `SO_REUSEADDR`,允许同一段上 IP-only dnsmasq sidecar 同时听 67 | `internal/dhcp/server.go` |
| **Dell BIOS 兼容修复** | OFFER 不带 bootfile(走 4011),避免老 iDRAC 启动失败 | 设计选择,见 [[pxe-dell-bsdp]] |

---

## 2. 硬件纳管(M1)

> 任意机器 PXE 进 live 镜像就自动上报,无需事先在控制器登记。

| 功能 | 说明 | 模块 |
|---|---|---|
| **agent 自举** | 解析 `/proc/cmdline` 的 `metalkit.url=`,无需在镜像里硬编 controller 地址 | `cmd/agent/main.go` |
| **首次报告 + 重试** | POST `/api/v1/report`,失败按 2s→60s 指数退避重试,直到成功 | `cmd/agent/main.go` |
| **心跳**(30 s) | POST `/api/v1/heartbeat/{uuid}`,只带轻量字段,用于 online/offline 判定 | 同上 |
| **离线判定** | controller 端按 `last_seen` 阈值算 `status=online/stale/offline` | `internal/inventory/store.go` |
| **MAC 查机** | `GET /api/v1/lookup?mac=...` 根据网卡 MAC 反查 machine_uuid | `internal/inventory/api.go` |
| **历史报告** | 每次启动都新增一条 `reports` 记录,可回放硬件变化(`GET /machines/{uuid}/reports`) | 同上 |

### 2.1 采集器(`internal/inventory/collect/`)

| 文件 | 采集内容 | 实现要点 |
|---|---|---|
| `system.go` | 整机:UUID / 序列号 / 厂商 / 型号 / BIOS 版本 / chassis 类型 | 优先读 `/sys/class/dmi/id/`,失败回退 dmidecode |
| `dmidecode.go` | 主板细节(原文 SMBIOS) | 解析 `dmidecode -t 0,1,2,3` |
| `cpu.go` | 物理/逻辑核心数、型号、频率、socket 数 | `/proc/cpuinfo` |
| `memory.go` | DIMM 条数、单条容量、总容量、ECC | dmidecode type 17 |
| `disks.go` | 块设备清单、容量、转速/SSD、SMART 健康、是否 boot 盘 | `lsblk -J`、`smartctl`、`/sys/block/*/queue/rotational` |
| `nics.go` | 网卡名、MAC、PCI 位置、speed/duplex、驱动 | `/sys/class/net/`、`ethtool` |
| `pci.go` | 全 PCI 设备清单(class / vendor / device id / kernel driver) | `lspci -mm -nn -vk` |
| `sensors.go` | 温度 / 风扇 / 电压传感器 | `/sys/class/hwmon/`、`sensors` |
| `bmc.go` | 在 live 系统里本地探 BMC IP / MAC / 版本 | `ipmitool lan print 1` |

每个采集器都有独立 unit test(`*_test.go`),用 `testdata/` 里的真实样本回放。

---

## 3. 镜像管理(M2.2)

> OS 镜像通过 Web UI / API 上传,内容寻址存储,支持大文件断点续传。

| 功能 | 说明 | 模块 |
|---|---|---|
| **分块上传** | `POST /api/v1/images/uploads` 开 session → 多次 `PUT /uploads/{id}/chunks/{n}` → `POST /uploads/{id}/finalize` | `internal/images/api.go` |
| **断点续传** | 客户端可查 `GET /uploads/{id}` 拿已收到的 chunk 索引,断网后只补未传部分 | 同上 |
| **内容寻址落盘** | finalize 时算 SHA-256,文件名 = sha256,重复上传秒级去重 | 同上 |
| **格式检测** | qcow2 / raw / iso / vmdk → `qemu-img info` 提元数据(virtual_size, format) | `internal/images/detect.go` |
| **family 推断** | 从文件名启发式判 Ubuntu / Debian / RHEL / Rocky / Alma / CentOS / Fedora | 同上 |
| **operator 覆盖** | UI 操作员可手工设 family/format,覆盖自动检测(`MergeDetected`) | `internal/images/api.go` |
| **catalog CRUD** | `GET/POST/PATCH/DELETE /api/v1/images` 浏览、改元数据、删 | 同上 |
| **磁盘布局** | `/var/lib/metalkit/images/.tmp/<session>/{chunks}` 中转,finalize 拼好搬到 `images/<sha>` | `internal/images/api.go` |
| **删除安全** | 引用计数:被 profile 引用的镜像禁止删 | profile 端 ref 检查 |

---

## 4. Subnet 子网管理(M2.3-12)

> 装完机后机器要落到哪个段:IP / 网关 / DNS / VLAN 的配方。

| 功能 | 说明 | 模块 |
|---|---|---|
| **CRUD** | `GET/POST/PATCH/DELETE /api/v1/subnets` | `internal/subnets/` |
| **CIDR 校验** | 解析 + 范式化(`10.0.0.0/24`),禁止 host bit | `validate.go` |
| **DNS 列表** | 多 DNS,逗号分隔或数组 | schema.go |
| **VLAN tag** | 0 = untagged,1–4094 = tagged;binding 可以覆盖 | schema.go |
| **gateway 校验** | 必须落在 CIDR 内 | validate.go |
| **引用保护** | 被 binding 引用时禁止删除 | store.go |

---

## 5. Profile 装机套餐(M2.3)

> 一个 profile = 镜像 + root 密码 hash + cloud-init 模板,可复用给多台机器。

| 功能 | 说明 | 模块 |
|---|---|---|
| **CRUD** | `GET/POST/PATCH/DELETE /api/v1/profiles` | `internal/profiles/` |
| **image 关联** | `image_id` 引用 `images` 表 | schema.go |
| **root 密码 hash** | 接收明文,服务端用 `mkpasswd -m sha-512` 转 hash 后存(不存明文);留空走 `defaultRootPassword` | validate.go |
| **cloud-init 模板** | `user_data_template` / `meta_data_template` / `network_config_template` 三个 Go template,渲染期注入 hostname / IP / hash | schema.go |
| **partial PATCH** | 只改部分字段,未传字段保留;`null` 显式清空(密码除外) | api.go |
| **删除保护** | 被 binding 引用禁止删 | store.go |

---

## 6. BMC 凭据管理(M2.3-3 / 9)

> BMC 密码服务端加密存储,UI 永远拿不回明文。

| 功能 | 说明 | 模块 |
|---|---|---|
| **CRUD** | `GET/POST/PATCH/DELETE /api/v1/bmc/{machine_uuid}` | `internal/bmc/` |
| **AES-256 加密** | 密码字段用 `master.key`(0600)加密入库,响应里永远是 `***` | `internal/crypto/` + `internal/bmc/` |
| **凭据测试** | `POST /api/v1/bmc/{uuid}/test` → 后端跑 `ipmitool chassis status` 验证可达性 + 凭据 | `internal/bmc/api.go` + `internal/ipmi/` |
| **远程电源控制** | `POST /api/v1/bmc/{uuid}/power/{on,off,cycle,reset,soft}` | 同上 |
| **onboard 接入** | `POST /api/v1/bmc/{uuid}/onboard`:一键测试 + 写凭据 + 触发首次 PXE 探测 | 同上 |
| **master.key 自管理** | 启动期自动生成(0600),已存在则加载;丢失等于库里所有 BMC 凭据作废 | `cmd/controller/main.go` |
| **审计字段** | `last_tested_at` / `last_test_result` 记录最近一次 test 时间和结果 | schema.go |

---

## 7. Binding 三方绑定(M2.3-4 / 12 phase ②)

> 把 machine + profile + subnet 关联起来,声明"这台机器要装成什么样"。

| 功能 | 说明 | 模块 |
|---|---|---|
| **CRUD** | `GET/POST/PATCH/DELETE /api/v1/bindings` | `internal/bindings/` |
| **三方关联** | `machine_uuid` + `profile_id` + `subnet_id` 都要存在(referential 检查) | validate.go |
| **每机覆盖项** | `ip_override` / `vlan_override` / `bond_override` / `nic_selector` / `target_disk` 单机微调,不污染 profile | schema.go |
| **desired_state 状态机** | `none` / `install` / `reinstall` —— orchestrator 据此决定是否触发装机 | schema.go |
| **password_override** | 单机临时密码,覆盖 profile 默认 | schema.go |
| **referential 完整** | 删 profile / subnet 时先看 binding 是否还引用 | store.go |
| **ref-count 辅助** | 内部 helper `CountByProfileID` / `CountBySubnetID` 给上游做删除前置检查 | store.go |

---

## 8. Job 编排(M2.3-5)

> 一个 binding 进入 `install` 状态后,orchestrator 自动创建 job 并执行。

| 功能 | 说明 | 模块 |
|---|---|---|
| **5 s tick orchestrator** | 单 goroutine 5s 一轮 `handleInstallRequests` + `handleSucceededJobs` | `internal/jobs/orchestrator.go` |
| **每机互斥** | UNIQUE partial index `idx_jobs_one_inflight_per_machine ON jobs(machine_uuid) WHERE status IN ('pending','running')` —— 同机只能有 1 个 job 在飞 | `internal/jobs/schema.go` |
| **状态机** | `pending → preparing → installing → finalizing → succeeded / failed / canceled` | schema.go |
| **触发 BMC 上电** | `pending` → `ipmi.BootForPXE`(`bootdev=pxe options=efiboot` + `power cycle`) | orchestrator.go + ipmi.go |
| **完成后切回硬盘** | `succeeded` 且 `finished_at >= binding.updated_at` → `ipmi.FinalizeBootDisk`(`bootdev=disk` + 重启)| orchestrator.go |
| **stale-job 保护** | 用 `finished_at >= updated_at` 防止重新 arm 的 binding 被旧 succeeded job 抢先 finalize | orchestrator.go |
| **日志流** | `jobs.log` 表存阶段事件,支持 `?since_offset=` 增量拉 | `internal/jobs/api.go` |
| **失败语义** | `failed` 带 `error_code` + `error_detail`,UI 直接渲染 | api.go |
| **手工 cancel** | `POST /api/v1/jobs/{id}/cancel`(运行中的 job 标 `canceling`,agent 收到下个 poll 退出) | api.go |
| **容量与清理** | 定期 GC 完成超过 N 天的 job 记录(不删 binding 历史) | store.go |

---

## 9. Agent ↔ Controller Job 协议(M2.3-6 / 7)

> 装机 live 镜像里的 agent 用一组无认证端点跟 controller 协商任务。

| 功能 | 说明 | 模块 |
|---|---|---|
| **拉 job** | `GET /api/v1/agent/jobs/next?machine_uuid=...` —— 拿到 pending job 的 spec | `internal/jobs/agent_api.go` |
| **InstallSpec** | 把 binding 覆盖项叠到 profile 上,组装出最终的 `InstallSpec`(disk / image_sha / cloud-init / network / 密码 hash) | agent_api.go + `getSpec` |
| **镜像 blob** | `GET /api/v1/agent/images/{sha}/blob` 流式发送 OS 镜像到 agent(支持 Range,断点续传) | `internal/images/api.go` |
| **阶段汇报** | `POST /agent/jobs/{id}/stage`,带 `stage` 名上报当前进展 | agent_api.go |
| **日志追加** | `POST /agent/jobs/{id}/log` 流式追加结构化日志行 | agent_api.go |
| **完成 / 失败** | `POST /agent/jobs/{id}/success` / `/fail` | agent_api.go |
| **一致性检查** | 每个 endpoint 校验 `machine_uuid` 跟 job 是否一致,防止 agent 错领别人的活 | agent_api.go |
| **无凭据设计** | live 镜像没法存 token,这些端点故意 open;权限边界靠 job_id + machine_uuid 双重匹配 | 设计选择 |

---

## 10. 装机执行管道(M2.3-7)

> agent 跑的实际装机流程,9 个阶段串成一条 pipeline。

`internal/installer/installer.go` 定义入口 `Run`,按顺序触发以下 stage,每个 stage 失败立刻退出并上报。

| 阶段 | 作用 | 实现 |
|---|---|---|
| `boot-detect` | 探测当前是 BIOS 还是 UEFI 启动(`/sys/firmware/efi` 存在性) | `installer.go` |
| `disk-pick` | 按 binding 的 `target_disk` 或自动策略挑选目标盘 | 同上 |
| `download` | 从 controller 拉镜像 blob 到本地 `/tmp/`(或直接流式管道) | 同上 |
| `write` | `qemu-img convert`(qcow2)或 `dd`(raw)写入目标盘 | `installer.go` |
| `grow` | 拉伸根分区填满盘(`growpart` + `resize2fs/xfs_growfs`) | `internal/installer/grow.go` |
| `mount` | 按写入后的 `fstab` 把目标盘的所有分区挂到 `/mnt/install/`(LABEL/UUID 双 fallback) | `internal/installer/mount.go` |
| `seed` | 渲染 cloud-init 模板,生成 NoCloud seed(`user-data` + `meta-data` + `network-config`),写到 `/var/lib/cloud/seed/nocloud-net/` | `internal/installer/seed.go` |
| `grub-install` | RHEL 家族:chroot 跑 `grub2-install` + `grub2-mkconfig`;Debian/Ubuntu cloud-image:host 跑 `grub-install --boot-directory` | `internal/installer/grub.go` |
| `umount` | 倒序 umount,清环回设备 | `installer.go` |

### 10.1 装机辅助能力

| 功能 | 模块 |
|---|---|
| **`ipmitool` 包装**(超时、env 传密码、cmd 隔离) | `internal/ipmi/` |
| **磁盘镜像 lint** | `internal/images/detect.go` |
| **Debian/Ubuntu 与 RHEL 双 GRUB 策略** | `internal/installer/grub.go` |
| **fstab 驱动的分区挂载**(支持 `/boot` 独立分区、LVM) | `internal/installer/mount.go` |
| **依赖注入**(`Deps` 接口) | 单元测试可注入 mock runner,不动真盘 |

---

## 11. Web UI

> 服务端渲染的 SPA-lite,所有页面在 `internal/webui/assets/`。

| 路径 | 页面 | 主要功能 |
|---|---|---|
| `/ui/` | `index.html` | machine 列表 + 在线状态 + 关键硬件摘要 |
| `/ui/m/{uuid}` | `detail.html` | 单机详情:硬件、报告历史、BMC、binding、装机控制 |
| `/ui/images` | `images.html` | 镜像目录:上传、family/format 编辑、删除 |
| `/ui/profiles` | `profiles.html` | profile 编辑、cloud-init 模板 |
| `/ui/subnets` | `subnets.html` | 子网 CRUD |
| `/ui/bmc` | `bmc.html` | BMC 凭据集中视图 + 批量电源操作 |
| `/ui/jobs` | `jobs.html` | job 列表,带过滤(状态、机器) |
| `/ui/jobs/{id}` | `job.html` | 单 job 详情:阶段、日志流(autorefresh) |
| `/ui/login` | `login.html` | 登录页(submit → `/api/v1/auth/login` → cookie) |

辅助 JS:`assets/common.js`(fetch 封装)、`assets/app.js`(全局),静态资源都通过 `//go:embed` 进 binary。

---

## 12. 认证(M2.3-11)

| 功能 | 说明 | 模块 |
|---|---|---|
| **HTTP Basic Auth** | 默认走 Authorization 头,适合 API 客户端 / 脚本 | `internal/authapi/` + `internal/sessions/` |
| **Cookie session** | `POST /api/v1/auth/login` 用 user/pass 换 cookie,UI 用 | 同上 |
| **`who-am-i`** | `GET /api/v1/auth/me` 查当前 session | 同上 |
| **`logout`** | `POST /api/v1/auth/logout` 销毁 session | 同上 |
| **open mode** | `adminPass` 留空 → 启动 WARN,UI / 读 API 直接放行(隔离环境) | config.go |
| **agent 端点豁免** | `/api/v1/report` / `heartbeat` / `agent/jobs/*` 永远 open;agent 没法存凭据 | server.go |
| **session 存储** | SQLite `sessions` 表,过期自动 GC | `internal/sessions/` |

---

## 13. Utility 端点

| 功能 | 端点 | 模块 |
|---|---|---|
| **SHA-512 crypt** | `POST /api/v1/util/crypt-sha512`:UI 调一下把明文转 `$6$...` hash,profile 表单立刻能用 | `internal/util/` |
| **healthz** | `GET /healthz`:轻量探活,无 auth | `internal/httpd/server.go` |

---

## 14. CLI 子命令

| 子命令 | 用途 | 实现 |
|---|---|---|
| `metalkit-controller` (无子命令) | 主服务:起 DHCP + BSDP + TFTP + HTTP + orchestrator | `cmd/controller/main.go` |
| `metalkit-controller doctor -config <path>` | preflight:校验 config / 端口 / boot 目录 / master.key / 外部工具(ipmitool/qemu-img/mkpasswd) | `cmd/controller/doctor.go` |
| `metalkit-controller migrate-subnets -config <path>` | 一次性迁移:从老 binding 的散字段抽出 subnet 行,反向写回 `subnet_id` | `cmd/controller/migrate_subnets.go` |
| `metalkit-agent` | live 镜像里的客户端:解析 cmdline → 采集 → POST → 心跳 + job poll | `cmd/agent/main.go` |

---

## 15. 加密 / 密钥

| 功能 | 说明 | 模块 |
|---|---|---|
| **AES-256-GCM** | 通用加密包,被 BMC 密码字段使用 | `internal/crypto/` |
| **master.key 自管理** | 首次启动生成 32 字节,落盘 `masterKeyPath` 0600;后续启动加载 | `cmd/controller/main.go` |
| **加密上下文** | 每条 BMC 记录绑 `nonce`,即使密钥泄露也能逐条审计 | `internal/bmc/store.go` |

---

## 16. 可观测 / 运维

| 功能 | 说明 | 来源 |
|---|---|---|
| **结构化日志** | `slog` 全程,带 `stage=`、`hwaddr=`、`job_id=`、`machine_uuid=` 字段 | 全局 |
| **journal 标签** | systemd unit `StandardOutput=journal`,`journalctl -u metalkit-controller` 一站式 | deploy |
| **stage 事件** | PXE: `discover` / `bsdp` / `tftp` / `ipxe` / `http`;装机: 9 个 installer stage 都打事件 | `internal/dhcp/` + `internal/installer/` |
| **`healthz`** | TCP 8080 上的探活端点 | `internal/httpd/` |
| **`doctor`** | preflight 14+ 项检查,可单独跑 | `cmd/controller/doctor.go` |

---

## 17. 持久化(SQLite 共享)

> 所有 store 共用一份 `inventory.db`,WAL 模式,跨进程读 / 单进程写。

| 表 | 内容 | store 模块 |
|---|---|---|
| `machines` | 机器主体 + 当前 status | `internal/inventory/` |
| `reports` | 每次启动一条硬件全量快照 | 同上 |
| `images` | 上传的 OS 镜像(sha 主键) | `internal/images/` |
| `subnets` | 网段配方 | `internal/subnets/` |
| `profiles` | 装机套餐 | `internal/profiles/` |
| `bindings` | 三方绑定 + 单机覆盖 | `internal/bindings/` |
| `bmcs` | BMC 凭据(加密) | `internal/bmc/` |
| `jobs` | 装机任务 + 状态 + log | `internal/jobs/` |
| `sessions` | UI cookie session | `internal/sessions/` |
| `image_uploads` | 分块上传 session 中间态 | `internal/images/` |

封装层:`internal/sqlitedb/` 提供共享 DSN / WAL pragma / 迁移注册。

---

## 18. 未实现(已规划)

| 功能 | 状态 | 备注 |
|---|---|---|
| 跨段 PXE(DHCP relay) | 代码层支持 `giaddr` 非零,**真机未跑过** | 见 deploy.md §11 |
| 多控制节点联邦 | 当前单节点单段 | 同上 |
| 远程 SOL console proxy | 未实现 | M4 规划 |
| RBAC(多用户角色) | 当前单 admin 帐号 | M3 规划 |
| 镜像签名校验 | 只算 sha256,不验签 | 待定 |
| ARM64 live image | 仅 amd64 | / |

---

## 19. 快速查阅

- **API 详细参数 / 字段**:[`docs/api.md`](api.md)
- **从源码到上线**:[`docs/deploy.md`](deploy.md)
- **PXE / 装机调试手册**:[`docs/build-and-deploy.md`](build-and-deploy.md)
- **installer 内部工具**:[`docs/installer-tooling.md`](installer-tooling.md)
