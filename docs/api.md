# metalkit Inventory & Images & Profiles & BMC API（M2.1 / M2.2 / M2.3）

机内 agent 和 Web UI 之间共享的 HTTP API 参考。base URL 是 controller 的
`http://<serverIP>:<httpPort>`（例：`http://192.168.10.147:8080`）。

## 1. 认证

| 路径 | 认证 |
|---|---|
| `POST /api/v1/report` | 开放（agent 在 live-boot 没法持有凭据） |
| `POST /api/v1/heartbeat/{uuid}` | 开放 |
| `GET /api/v1/machines` | Basic Auth（admin / `adminPass`） |
| `GET /api/v1/machines/{uuid}` | Basic Auth |
| `GET /api/v1/machines/{uuid}/reports` | Basic Auth |
| `GET /api/v1/machines/{uuid}/reports/{id}` | Basic Auth |
| `GET /api/v1/lookup?mac=...` | Basic Auth |
| `POST /api/v1/images/uploads` (+ 整个子树) | Basic Auth（agent 不上传镜像） |
| `POST /api/v1/profiles` (+ 整个子树) | Basic Auth（profiles 只允许 admin 编辑） |
| `PUT /api/v1/bindings/{uuid}` (+ 整个子树) | Basic Auth（bindings 只允许 admin 编辑） |
| `PUT /api/v1/bmc/{uuid}` (+ 整个子树) | Basic Auth（BMC 凭据只允许 admin 编辑） |
| `/healthz`、`/boot/*` | 开放 |
| `/ui/*` | Basic Auth |

`config.yaml` 里 `adminPass` 为空时 Basic Auth 关闭（启动日志 `WARN`），仅
推荐在隔离测试网段用。

## 2. 数据模型

完整 schema 在 `internal/inventory/types.go`。两个高层别名：

- **`Report`**：agent 每次启动 POST 一份完整快照。`schema_version=1`。
  顶层有 12 大段（machine / firmware / cpu / memory / disks / nics /
  pci_devices / accelerators / bmc / sensors / system / agent）。
- **`MachineSummary`**：列表页用，6 列（uuid / serial / manufacturer /
  product_name / status / first_seen / last_seen / latest_report_id）。

## 3. 端点

### 3.1 `POST /api/v1/report`

agent 上报硬件快照。

- 请求体：完整 `Report` JSON
- 大小上限：5 MiB（超出 413）
- 校验：`schema_version == 1`、`machine.smbios_uuid` 非空
- 200 响应：`{"uuid": "<smbios uuid>", "report_id": 42}`
- 失败：400（schema 错误 / UUID 缺失 / JSON 无效）、413（超大）、500（写库失败）

写入语义：
- 第一次见的 UUID → 新增 `machines` 行（`first_seen` 设为当前时间，`status='online'`）
- 已有 UUID → 更新 `serial/manufacturer/product_name/last_seen/latest_report`，**MAC 集合替换**（旧 MAC 行删除，新 MAC 行写入，包括 BMC role）
- `reports` 表每次都追加一行

### 3.2 `POST /api/v1/heartbeat/{uuid}`

agent 30s 一次。

- URL 参数：`uuid` 必须是规范小写带短横线 SMBIOS UUID（如 `4c4c4544-0058-3210-8053-c5c04f463832`）
- 204 No Content：心跳已刷新 `machines.last_seen` 并 `status='online'`
- 404：UUID 未注册（agent 应当先 POST `/report`）
- 400：UUID 格式非法

### 3.3 `GET /api/v1/machines`

列表，按 `last_seen DESC` 排序。

```json
[
  {
    "uuid": "4c4c4544-0058-3210-8053-c5c04f463832",
    "serial": "ABC1234",
    "manufacturer": "Dell Inc.",
    "product_name": "PowerEdge R740",
    "status": "online",
    "first_seen": 1716504000,
    "last_seen": 1716504300,
    "latest_report_id": 42
  }
]
```

`status` 枚举：`online`（最近 ≤ 90s 内有报告/心跳）、`offline`（超过 90s）、`unknown`。

### 3.4 `GET /api/v1/machines/{uuid}`

返回最近一份完整 `Report`（包含全部 12 段）。

- 200：完整 Report JSON
- 404：UUID 不存在或还没收到过 report

### 3.5 `GET /api/v1/machines/{uuid}/reports`

历史 report 列表（不含 body），新到旧。

```json
[
  {"id": 42, "ts": 1716504300},
  {"id": 38, "ts": 1716500700}
]
```

- 404：UUID 不存在

### 3.6 `GET /api/v1/machines/{uuid}/reports/{id}`

取某条历史 report 的完整 body。

- 200：完整 Report JSON
- 404：UUID 或 ID 不匹配

### 3.7 `GET /api/v1/lookup?mac=AA:BB:CC:DD:EE:FF`

按 MAC 反查机器。

- 200：`{"uuid": "<smbios uuid>", "role": "nic"}` 或 `role="bmc"`
- 400：MAC 格式非法（必须 6 段冒号分隔十六进制）
- 404：MAC 没注册

## 4. 离线判定

controller 启动后跑后台 goroutine（`Store.RunOfflineMarker`），每 60s 扫一遍
`machines.last_seen`，超过 90s 没动静的标 `status='offline'`。心跳 / report
都会刷新 `last_seen` 并把 status 拉回 `online`。

阈值在 `internal/inventory/store.go` 顶部常量（`offlineAfter = 90s`、
`markOfflineTick = 60s`）。

## 5. 错误响应

所有失败响应统一格式：

```json
{"error": "human readable message"}
```

HTTP code 反映语义，body 用 `application/json; charset=utf-8`。

## 6. agent 行为契约

`cmd/agent` 启动流程：

1. 从 `/proc/cmdline` 取 `metalkit.url=...`（也支持 `-url=` 命令行覆盖，便于本地调试）
2. 跑 `collect.All(ctx)` 收集 9 个采集器（per-collector 10s timeout，单个子进程 8s）
3. POST `/api/v1/report`，2xx → 进入心跳循环；5xx → 指数退避重试（2s → 4s → … → 60s 封顶）；4xx → 失败退出
4. 每 30s POST `/api/v1/heartbeat/{uuid}`，失败仅 warn 日志、不退出

## 7. 关键运营细节

- **MAC 索引唯一**：`machine_macs.mac` 是 UNIQUE 索引。同一台机器再次上报会
  替换它的整个 MAC 集合（先 delete-where-uuid 再 insert）。换网卡 → 旧 MAC
  被驱逐，新 MAC 注册。两台机器谎称同一 MAC → 第二台的 upsert 会 fail（事务
  回滚），日志能看到 UNIQUE constraint 错误。
- **schema_version 强校验**：agent 与 controller 版本对不上时直接 400，不
  尝试兼容旧 schema。M2.x 系列内 `schema_version` 都是 1。
- **report body 存的是原始 JSON**：`reports.body` 是 TEXT 列，存的是 agent
  发来的整段 JSON。controller 查询时再 unmarshal。这样 schema 演进只动 types，
  历史 report 仍可读。
- **不做 DB 备份的话**：SQLite WAL 模式下 `.db` + `.db-wal` + `.db-shm` 三件
  套放 `/var/lib/metalkit/`。备份时停 controller 拷三件、或者用 `sqlite3
  inventory.db ".backup snapshot.db"`。

## 8. 镜像 API（M2.2 新增）

base 路径 `/api/v1/images*`，**整个子树 Basic Auth**。状态码总结：400 = 参数错 /
sha 不是 64 hex / 长度对不上；404 = 会话/镜像不存在；409 = 同 sha256 已上传；413 =
init 请求体超 64 KiB。

### 8.1 chunked upload（上传链）

```
POST /api/v1/images/uploads
  body: {"name":"ubuntu.qcow2","family":"ubuntu","version":"22.04",
         "notes":"optional","expected_sha256":"<64 hex>",
         "total_size":<bytes>,"chunk_size":<bytes, default 10 MiB>}
  201 → UploadSession {id, num_chunks, chunk_size, started_at, ...}
  409 → 同 sha256 已经在 images 表里

GET /api/v1/images/uploads/{uid}
  200 → 当前 UploadSession（含 uploaded_chunks 进度）

PUT /api/v1/images/uploads/{uid}/chunks/{n}
  header: X-Chunk-Sha256: <64 hex of this chunk>
  body: 原始字节，长度 = chunk_size（末片可以短）
  200 → {"chunk":n,"bytes_written":...}
  400 → 长度不符 / 顺序越界 / X-Chunk-Sha256 对不上

POST /api/v1/images/uploads/{uid}/finalize
  无 body
  201 → 完整 Image 行（id, name, sha256, size_bytes, virtual_size, format,
                       uploaded_at, metadata_json, ...）
  400 → 有片缺失 / 整体 sha256 对不上
  409 → 并发完成的同 sha256 抢先一步

DELETE /api/v1/images/uploads/{uid}
  204 → 会话 + 临时片清掉；幂等性：第二次返回 404
```

### 8.2 catalog（CRUD 浏览）

```
GET /api/v1/images          → [Image, ...]，按 uploaded_at DESC
GET /api/v1/images/{id}     → Image
DELETE /api/v1/images/{id}  → 200 Image（已被删的），catalog row + 磁盘文件同步删除
```

### 8.3 Image schema

```json
{
  "id": "<32 hex>",
  "name": "ubuntu-22.04-cloudimg.qcow2",
  "version": "22.04",
  "family": "ubuntu",
  "format": "qcow2",
  "size_bytes": 581238784,
  "virtual_size": 2361393152,
  "sha256": "<64 hex>",
  "uploaded_at": "2026-05-24T12:34:56Z",
  "uploaded_by": "admin",
  "last_used_at": null,
  "notes": "",
  "metadata_json": "<qemu-img info --output=json 原文>"
}
```

`metadata_json` 是 `qemu-img info --output=json` 的整段输出（含 `cluster-size`、
`backing-filename`、`encrypted` 等 qemu 自带的字段），UI 暂时不解析，但留出来给
M2.3 装机阶段判断 backing chain / 加密镜像用。

### 8.4 磁盘布局

```
{imagesDir}/                       # 默认 /var/lib/metalkit/images
{imagesDir}/.tmp/{uid}/chunk-N     # 上传中
{imagesDir}/.tmp/{uid}/assembled   # finalize 拼接中
{imagesDir}/{sha256}.{format}      # 正式镜像（内容寻址）
```

`Store.GCStaleUploads(threshold)` 会清掉超过 threshold 没更新的 upload_sessions 行 +
对应 .tmp 子目录，并清理无 DB 行的 orphan .tmp 子目录。M2.2 暂未把这条接成定时任务，
M2.3 会随作业 GC 一起接进来。

## 9. 装机 Profile API（M2.3 新增）

base 路径 `/api/v1/profiles*`，**整个子树 Basic Auth**。Profile = 一份可被多台机器
复用的「装机模板」：hostname 模式、root 密码哈希、目标盘选择策略、网络模板。

**Per-machine 状态（实际静态 IP、BMC 凭据）不在这里**——profile 是模板，binding 表
（M2.3-2）会把 profile + image + 机器 + per-machine 静态 IP 绑到一起。

### 9.1 schema

```json
{
  "id": "<32 hex>",
  "name": "lab-ubuntu",
  "description": "static ip lab profile",
  "hostname_template": "node-{serial}",
  "root_password_hash": "$6$rounds=4096$<salt>$<86 hex/dot/slash>",
  "target_disk": {
    "mode": "smallest" | "by-path" | "by-wwn" | "by-model",
    "value": "/dev/disk/by-path/..."
  },
  "network": {
    "method": "static" | "dhcp",
    "prefix_len": 24,
    "gateway": "192.168.10.1",
    "dns": ["8.8.8.8"],
    "nic_selector": "auto" | "by-mac:AA:BB:CC:DD:EE:FF" | "by-name:eno1"
  },
  "created_at": "2026-05-24T07:30:46Z",
  "updated_at": "2026-05-24T07:30:46Z",
  "created_by": "admin"
}
```

字段校验摘要（细节见 `internal/profiles/validate.go`）：

- `name`：唯一；`[A-Za-z0-9][A-Za-z0-9._-]{0,63}`
- `hostname_template`：≤253 chars；允许 `{serial}`、`{uuid8}`、`{mac}` 占位符
- `root_password_hash`：必须是 `$6$...` sha512crypt；用 `mkpasswd -m sha-512` 或同等工具
- `target_disk.mode = smallest`：value 必须空；其它 mode：value 必须有
- `network.method = static`：必须填 `prefix_len` (1-32)、`gateway` (IPv4)、`dns[]` (IPv4)
- `network.method = dhcp`：static-only 字段会被服务器侧抹平
- `nic_selector`：`auto` / `by-mac:` (规范 6 段冒号 MAC) / `by-name:` (≤15 字符 Linux ifname)

### 9.2 端点

```
GET    /api/v1/profiles           → [Profile, ...]，按 created_at DESC
POST   /api/v1/profiles           → 创建；201 Profile / 400 校验失败 / 409 duplicate name
GET    /api/v1/profiles/{id}      → Profile / 404
PUT    /api/v1/profiles/{id}      → 部分更新（JSON 里只放要改的字段）；200 Profile / 400 / 404
DELETE /api/v1/profiles/{id}      → 204 / 404
```

错误响应同 §5：`{"error": "..."}` + JSON content-type。

### 9.3 部分更新规则

PUT 接受所有 POST 字段（除 `name` 不可改），未给的字段保持不变。`hostname_template`、
`root_password_hash`、`target_disk`、`network` 会跑同样的校验；任何字段失败整个请求 400。

### 9.4 注意事项（M2.3 后续会用到）

- profile 删除目前没有 referential check——M2.3-2 上 bindings 表后，会变成 ON DELETE
  RESTRICT，被引用的 profile 删不掉。
- profile 不存 BMC 凭据或 platform-admin 私钥。前者按机器维度存（M2.3-3 加密表），后者
  是 controller 全局一份（M2.3-3 落地）。
- profile 也不存 per-machine 静态 IP——`network.method=static` 表示「这个 profile 走
  静态」，具体地址在 binding 上。bindings 表（M2.3-2）会和 profile + image 一起把这台
  机器的装机指令拼完。

## 10. 装机绑定 API（M2.3 新增）

base 路径 `/api/v1/bindings*`，**整个子树 Basic Auth**。Binding = 一台机器要装什么的「当前
赋值」：machine_uuid ↔ image_id ↔ profile_id ↔ desired_state（+ per-machine 静态 IP /
hostname 覆盖）。一台机器最多有一个 binding，主键就是 `machine_uuid`，所以**没有 POST**：
URL 已经指明了哪台机器，PUT 既是新增也是覆盖。

历史「装过哪些 (image, profile) 组合」放 jobs 表（M2.3-5）。bindings 表只反映当前赋值。

### 10.1 schema

```json
{
  "machine_uuid": "4c4c4544-0044-4d10-8035-b1c04f484a32",
  "image_id":     "<32 hex>",
  "profile_id":   "<32 hex>",
  "desired_state": "none" | "install" | "reinstall",
  "static_address": "10.0.0.7",     // 仅 profile.network.method=static 时给值，dhcp 时必须省略
  "hostname":       "node7",        // 可选；覆盖 profile.hostname_template 的展开结果
  "updated_at":     "2026-05-24T07:51:05Z",
  "updated_by":     "admin"
}
```

字段校验摘要（细节见 `internal/bindings/validate.go`）：

- `machine_uuid`：规范小写 SMBIOS UUID，从 URL 取，body 里写也没用
- `image_id` / `profile_id`：32 字符小写 hex（同 images / profiles 表的 PK 格式）
- `desired_state`：`none` / `install` / `reinstall` 之一
- `static_address`：profile.network.method=static 时必填 IPv4 字面量；method=dhcp 时必须为空
- `hostname`：可选；RFC-1123 字符集 + 各段 1–63 字符 + 总长 ≤253；空则 agent 会展开
  profile.hostname_template

### 10.2 端点

```
GET    /api/v1/bindings              → [Binding, ...]，按 machine_uuid 升序
GET    /api/v1/bindings/{uuid}       → Binding / 404
PUT    /api/v1/bindings/{uuid}       → 创建或覆盖；200 Binding
DELETE /api/v1/bindings/{uuid}       → 204 / 404
```

PUT 严格模式（`DisallowUnknownFields`）：body 出现未知字段直接 400。

### 10.3 referential 检查

PUT 时 controller 会查三张表：

| 检查 | 失败 | HTTP |
|---|---|---|
| machine_uuid 在 `machines` 表里存在（即 agent 上报过） | `bindings: machine_uuid not in inventory` | 422 |
| image_id 在 `images` 表里存在 | `bindings: image_id not in catalog` | 422 |
| profile_id 在 `profiles` 表里存在 | `bindings: profile_id not in catalog` | 422 |
| `static_address` 和 profile.network.method 兼容 | `static_address: required when ... =static` 等 | 400 |

> 「422 vs 400」：未知机器 / 镜像 / profile 是**外键缺失**，URL/body 本身格式都对，
> 只是被引用对象不存在；其余校验失败（IP 不是 IPv4、UUID 大写、hostname 字符非法）都是 400。

### 10.4 ref-count 辅助（内部）

`bindings.Store` 暴露 `RefCountByImage(imageID)` / `RefCountByProfile(profileID)`，
供 images / profiles 的 DELETE 处理在「有 binding 还引用」时拒绝删除（M2.3-3 起把这些挂上）。

### 10.5 跟 jobs 表的分工（M2.3-5 前的占位）

- **bindings 表**：当前状态。一台机器一行；PUT 覆盖；DELETE 撤销赋值。
- **jobs 表（M2.3-5）**：装机历史。每次触发装机都生成一条 job，记录 (image, profile, stage,
  started_at, finished_at, error)。binding 改了不影响历史 job。

操作流程是 controller 看到 `bindings.desired_state ∈ {install, reinstall}` 后，创建一条
pending job，再由 BMC 重启 → agent 拉 job → 走装机流程。binding 本身不携带"什么时候触发"
的信息——触发由 UI / API 显式发起（M2.3-5 落地）。

## 11. BMC 凭据 API（M2.3 新增）

存放每台机器的 IPMI / BMC 凭据，给 controller 内部 `ipmitool` 包装器使用——controller
后续会通过 `ipmitool chassis bootdev pxe + power cycle` 强制目标机重进 live PXE
完成重装（plan F1）。

### 11.1 schema

```sql
CREATE TABLE bmc_credentials (
    machine_uuid    TEXT PRIMARY KEY NOT NULL REFERENCES machines(uuid) ON DELETE CASCADE,
    ip              TEXT NOT NULL,                       -- BMC 管理网 IP (IPv4)
    port            INTEGER NOT NULL DEFAULT 623 CHECK (port BETWEEN 1 AND 65535),
    username        TEXT NOT NULL,                       -- IPMI 账户名
    password_ct     BLOB NOT NULL,                       -- AES-GCM 密文（永不返给 HTTP）
    ipmi_interface  TEXT NOT NULL DEFAULT 'lanplus'
                    CHECK (ipmi_interface IN ('lan','lanplus')),
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    updated_by      TEXT NOT NULL
);
```

- **password_ct** 是 `version || 12-byte nonce || AES-256-GCM(plaintext, tag)`。
  master key 在 `/var/lib/metalkit/master.key`（mode 0600），controller 首次启动
  自动生成。**丢这把钥匙等于丢所有 BMC 密码** — 备份一下。
- 主键就是 `machine_uuid`，一机一行；机器从 `machines` 删除时 FK 级联。
- 列名 `password_ct`（ciphertext）是故意的——在 DB 里 dump 时一目了然，避免运维误判
  "这字段不是明文吗"。

### 11.2 端点

| 方法 + 路径 | 含义 |
|---|---|
| `GET /api/v1/bmc` | 列出全部（**不返回密码字段**） |
| `POST /api/v1/bmc` | 按 BMC IP 注册：服务端派生占位 UUID（`placeholder-<ipv4-dashed>`），并自动建一行 `machines` 占位（status=unknown）。目标机首次 PXE 上报后会自动迁移到真实 SMBIOS UUID |
| `GET /api/v1/bmc/{machine_uuid}` | 单条（**不返回密码字段**） |
| `PUT /api/v1/bmc/{machine_uuid}` | upsert；首次 create 时 `password` 必填，update 时省略 = 保留旧值。`{machine_uuid}` 可以是真实 SMBIOS UUID 也可以是占位 UUID |
| `DELETE /api/v1/bmc/{machine_uuid}` | 删除（若是占位 UUID，连带把对应的占位 machines 行一起清掉） |
| `POST /api/v1/bmc/{machine_uuid}/test` | 现场探活：调用 `ipmitool chassis power status` |
| `POST /api/v1/bmc/{machine_uuid}/power/{action}` | 电源动作：action ∈ `on` / `off` / `cycle` / `soft` / `reset` |
| `POST /api/v1/bmc/{machine_uuid}/onboard` | 纳管：`bootdev=pxe + power cycle` —— 目标机进 live 上报硬件，**不装系统** |

PUT body 示例：
```json
{
  "ip": "192.168.10.247",
  "username": "ADMIN",
  "password": "hunter2",
  "ipmi_interface": "lanplus",
  "port": 623
}
```

**Update 时省略 `password`**：在 update 路径下（已有行）`password` 是 `*string`+`omitempty`——
缺省或空串都表示"保留 DB 里现有的密文"。这让 UI 改 IP / username 时不必让操作员重输密码。
新建（行不存在时）仍要求 `password` 非空。

GET / LIST 响应示例（**注意没有 password 键**）：
```json
{
  "machine_uuid": "4c4c4544-0044-4d10-8035-b1c04f484a32",
  "ip": "192.168.10.247",
  "port": 623,
  "username": "ADMIN",
  "ipmi_interface": "lanplus",
  "created_at": "2026-05-24T08:15:28Z",
  "updated_at": "2026-05-24T08:15:28Z",
  "updated_by": "admin"
}
```

### 11.2.1 POST `/api/v1/bmc/{machine_uuid}/test`（M2.3-9 新增）

调一次 `ipmitool chassis power status`，验证存档凭据是否有效。

**响应**（无论 ipmi 调用成功失败都返 200，UI 凭 `ok` 字段渲染）：

```json
// 探活成功
{"ok": true,  "power": "on"}     // 或 "off" / "unknown"

// ipmi 调用失败（凭据错 / BMC 不可达 / 超时）
{"ok": false, "error": "auth failed: ..."}
```

**非 200 状态码**：

| 状态 | 含义 |
|---|---|
| 404 | 没找到这个 machine_uuid 的 BMC 凭据 |
| 503 | 当前 controller 节点没有 `ipmitool` 可用（启动时 `ipmi.NewClient` 失败 → tester 没注入） |
| 400 | machine_uuid 格式非法 |

设计取舍：
- **ok:false 仍返 200**：UI 用 `MK.apiSend` 时 4xx/5xx 会 throw，导致 banner 红显；而 ipmi 失败
  对运维是正常的诊断结果，需要把错误内容渲染出来，不应当 throw。和 jobs orchestrator
  对 `chassis power status` 失败的处理一致。
- **10s 超时**：handler 用 `context.WithTimeout(r.Context(), 10*time.Second)` 包了 ipmi 调用，
  避免 BMC 不通时把请求线程挂死。

### 11.2.2 POST `/api/v1/bmc/{machine_uuid}/power/{action}`

主动触发 BMC 电源动作。`action` 取值固定：

| action | 含义 | 对应 ipmitool |
|---|---|---|
| `on`    | 上电（已在线则 no-op） | `chassis power on` |
| `off`   | 强制关机（无 graceful） | `chassis power off` |
| `soft`  | ACPI 软关机 | `chassis power soft` |
| `cycle` | 重启（在线时 warm reboot；离线时上电） | `chassis power cycle` |
| `reset` | 硬件复位（等价于按 reset 按钮） | `chassis power reset` |

**响应**（同 `/test`：ipmi 调用失败也返 200，UI 凭 `ok` 字段判断）：

```json
// 调用成功
{"ok": true, "action": "cycle"}

// ipmi 调用失败
{"ok": false, "error": "BMC unreachable: ..."}
```

**非 200 状态码**：

| 状态 | 含义 |
|---|---|
| 400 | action 不在 {on,off,cycle,soft,reset} 中 |
| 404 | 没找到这个 machine_uuid 的 BMC 凭据 |
| 503 | 当前 controller 节点没有 `ipmitool` 可用 |

**15s 超时**：电源动作的 BMC 响应通常比 `power status` 慢（要排队、ACK 命令），所以
比 `/test` 的 10s 更宽一些。

**审计**：handler 成功路径会在 controller 日志里记一行
`bmc power action uuid=... action=... by=...`，by 来自 Basic Auth 用户名（cookie 会话当前
不传 actor，统一为 `anonymous`——后续 RBAC 上线时再补 ctx 透传）。

### 11.2.3 POST `/api/v1/bmc/{machine_uuid}/onboard`

纳管：调 `ipmitool chassis bootdev pxe` + `chassis power cycle`，让目标机进 live 系统上报硬件。

**典型场景**：
1. **占位 BMC 首次发现**：操作员先按 IP 注册 BMC（生成 `placeholder-<ip-dashed>` UUID 和占位
   machines 行），然后点纳管 → 目标机 PXE 进 live → live agent 报 inventory → controller
   按 BMC IP 匹配把占位 UUID 迁移到真实 SMBIOS UUID。
2. **真实 UUID 重新发现 / 诊断**：机器跑过一段时间后需要刷新硬件信息（换了内存条 / 加了盘 / 改了 BIOS），
   触发纳管让它重进 live 重新上报。

**关键约束**：**不装系统**。orchestrator 只在 `binding.desired_state ∈ {install, reinstall}`
时才下发装机 job；纳管路径完全绕过 binding，agent claim 不到任何 job，只是在 live 里跑完
inventory 上报就停在 live 等指令。

**响应**（同 `/test` / `/power`：ipmi 调用失败也返 200，UI 凭 `ok` 字段判断）：

```json
// 调用成功
{"ok": true, "action": "onboard"}

// ipmi 调用失败
{"ok": false, "error": "set bootdev=pxe: ..."}
```

**非 200 状态码**：

| 状态 | 含义 |
|---|---|
| 404 | 没找到这个 machine_uuid 的 BMC 凭据 |
| 503 | 当前 controller 节点没有 `ipmitool` 可用 |
| 400 | machine_uuid 格式非法 |

**20s 超时**：底层 `ipmi.BootForPXE` 串行调用 `chassis bootdev pxe` + `chassis power cycle`
两个子命令，比单个动作慢，所以比 `/power` 的 15s 再宽一些。

**审计**：handler 成功路径记 `bmc onboard uuid=... ip=... by=...`。

### 11.3 设计取舍

- **PUT 上传 password 是可选的**：create 时必填，update 时缺省/空串 = 保留旧密文。
  少 1 个失败模式（操作员改 IP 时忘了再贴一次密码），UI 上"改 IP / 改用户名"不必让运维
  把明文密码再粘贴一遍。底层 sentinel 是 `*string` —— `nil` / `""` 都触发 keep。
- **GET 永远不返密码**：连 admin 也读不到密码（要看密码必须用 controller 内部的
  `Store.GetWithPassword` 接口，即 M2.3-4 的 ipmitool 包装器）。
- **`ipmi_interface` 只允许 `lan` / `lanplus`**：覆盖 IPMI 1.5 / 2.0 两种主流；老式
  ManageOL / sol 不在 M2 范围。
- **422 vs 400**：machine 不存在 → 422（数据库层引用失败）；JSON 字段类型 / 取值
  非法 → 400。错误响应统一 `{"error":"..."}`。

### 11.4 内部加解密接口（非 HTTP）

供 controller 进程内（如 ipmitool 包装器、装机 orchestrator）取明文密码：

```go
got, err := bmcStore.GetWithPassword(ctx, machineUUID)
// got.Credential = 同 HTTP 返回结构
// got.Password   = 明文密码
```

`PasswordedCredential.Password` 没有 `json` tag——一旦不小心把它 marshal 进 HTTP
响应就会被 `encoding/json` 默认按字段名输出（首字母大写的 `Password`），代码 review
时极易识别。**正确做法是只用 `Credential`（type-level guarantee：没有 password 字段）
作为 HTTP 返回。**


---

## 12. 装机 Job API（M2.3-5 新增）

操作员侧的只读 + 取消接口。**agent 侧**（claim、append log、succeed/fail、update stage）
将由 M2.3-6 在 `/api/v1/agent/jobs/*` 下另行挂载，鉴权模型独立（token-based）。

### 12.1 schema

```json
{
  "id": "<32-hex>",
  "machine_uuid": "4c4c4544-0058-3210-8053-c5c04f463830",
  "type": "install",
  "image_id": "<32-hex>",
  "profile_id": "<32-hex>",
  "status": "pending",
  "stage": "pxe_booting",
  "error": "",
  "created_at": "2026-05-24T10:00:00Z",
  "started_at": null,
  "finished_at": null,
  "created_by": "orchestrator",
  "retry_of_job_id": ""
}
```

- `type` ∈ `install` / `reinstall`
- `status` ∈ `pending` / `running` / `succeeded` / `failed` / `cancelled`
- 同一 machine_uuid **同时只能有一个非终态 job**（UNIQUE partial index 强制）
- 失败 job 不会自动重试。重新装请管理员发起新的 job（可以带 `retry_of_job_id`）

### 12.2 端点（全部需要 Basic Auth）

| 方法 | 路径 | 行为 |
|---|---|---|
| GET | `/api/v1/jobs` | 列出所有 jobs，最多 100 条（`?limit=` 调整），按 created_at DESC |
| GET | `/api/v1/jobs?machine_uuid=<uuid>` | 该机器的全部 job 历史 |
| GET | `/api/v1/jobs?status=<status>` | 按状态过滤 |
| GET | `/api/v1/jobs/{id}` | 单个 job |
| GET | `/api/v1/jobs/{id}/logs` | 日志，`?since_id=N` 增量，`?limit=` 调整 |
| POST | `/api/v1/jobs/{id}/cancel` | 取消（仅 pending / running 可取消，终态 → 409 Conflict） |

### 12.3 编排（orchestrator）

controller 进程内有一个 5s tick 的 reconciliation loop：

1. **install 触发**：扫描 `bindings.desired_state ∈ {install, reinstall}` 且无在飞 job 的行，
   创建 pending job，取 BMC 凭据，调用 ipmitool `chassis bootdev pxe + chassis power cycle`，
   把 job 推入 `running` 并打 `stage=pxe_booting`。
2. **finalize**：扫描 `status=succeeded` 但 `bindings.desired_state != none` 的 job：
   ipmitool `chassis bootdev disk`（防止下次启动又 PXE 回 live），然后清掉
   `desired_state` 为 `none`。

orchestrator 用本地接口（`BMCFetcher` / `IPMIClient` / `BindingUpdater`）解耦 bmc / ipmi /
bindings 包，避免循环导入。

### 12.4 日志结构

```json
{
  "id": 12,
  "job_id": "<32-hex>",
  "ts": "2026-05-24T10:00:05Z",
  "level": "info",
  "message": "PXE boot initiated; waiting for agent"
}
```

- `level` ∈ `debug` / `info` / `warn` / `error`
- 单行最长 `MaxLogMessageLen = 4096`（超长截断）
- orchestrator 把 ipmitool 错误输出写日志前会 `sanitize()` 掉 password 字串，防泄漏

### 12.5 失败语义

- BMC 凭据缺失 / 错误 → job `failed`，`error` 字段含 `"BMC credentials"`
- ipmitool 调用失败 → job `failed`，`error` 字段含 `"ipmi reboot failed"`
- agent 上报失败 → 由 agent 协议（M2.3-6）侧调用 `Fail()`

### 12.6 容量与清理

succeeded / failed / cancelled job 永久保留作为审计记录。`RefCountByImage` /
`RefCountByProfile` 只计入非终态 job，所以历史 job 不会永远阻塞 image / profile 删除。

## 13. Agent Job 协议（M2.3-6 新增）

live-boot agent 用这组端点拿任务、上报进度、收尾。所有路径挂在
`/api/v1/agent/jobs/*`，**全部豁免 Basic Auth**（agent 跑在 live 镜像里，
没有可信凭据存储；同 `/api/v1/report` / `/api/v1/heartbeat/*` 同一路）。

> ⚠️ **machine_uuid 是一致性校验，不是认证**：每个状态变更端点的请求
> body 必须包含 `machine_uuid`，controller 会拿它跟 job 行里的
> `machine_uuid` 比对，不一致返 403。这是为了防"agent A 误装 agent B 的
> job"的 foot-gun，不是安全边界。基于 token 的真正认证留给 M2.5。

### 13.1 端点速查

| 方法 | 路径 | body | 返回 |
|---|---|---|---|
| GET  | `/api/v1/agent/jobs/current?machine_uuid=<uuid>` | — | 200 `Job`（pending/running）/ 404 |
| POST | `/api/v1/agent/jobs/{id}/claim`   | `{machine_uuid}`                  | 200 `Job`（→ running） |
| POST | `/api/v1/agent/jobs/{id}/stage`   | `{machine_uuid, stage}`           | 204 |
| POST | `/api/v1/agent/jobs/{id}/logs`    | `{machine_uuid, level, message}`  | 204 |
| POST | `/api/v1/agent/jobs/{id}/succeed` | `{machine_uuid}`                  | 204 |
| POST | `/api/v1/agent/jobs/{id}/fail`    | `{machine_uuid, error}`           | 204 |

请求体上限 16KB（`http.MaxBytesReader`），未知字段一律 400
（`DisallowUnknownFields`）。

### 13.2 错误码

- 400 — JSON 解析失败 / 缺 machine_uuid / 字段非法（level、stage、id 格式）
- 403 — `body.machine_uuid` 不匹配 job 的 `machine_uuid`
- 404 — job 不存在；`/current` 无 in-flight job
- 409 — 状态机不允许此次转移（如对 `succeeded` job 调 `claim`）
- 500 — DB 错

### 13.3 典型 agent 流程

```text
loop:
    GET /api/v1/agent/jobs/current?machine_uuid=<self> → 404 → sleep 5s; continue
                                                       → 200 job → break
POST /api/v1/agent/jobs/{id}/claim    {machine_uuid}
POST /api/v1/agent/jobs/{id}/stage    {machine_uuid, stage:"download"}
POST /api/v1/agent/jobs/{id}/logs     {machine_uuid, level:"info", message:"..."}
... 写盘、cloud-init seed、umount ...
POST /api/v1/agent/jobs/{id}/stage    {machine_uuid, stage:"installed"}
POST /api/v1/agent/jobs/{id}/succeed  {machine_uuid}
    # 失败时改 POST /fail {machine_uuid, error:"..."}，orchestrator 不自动重试
```

succeed 后 orchestrator 下一 tick 触发 finalize（`ipmitool chassis bootdev disk`
+ power cycle）；fail 不自动重试，admin 需要 POST `/api/v1/bindings/<muuid>`
重置 `desired_state` 才会再生成新 job。

## 14. Agent Install Spec & Image Blob（M2.3-7 新增）

agent 拿到 job 后还需要两样东西：完整的"装机蓝图"（binding + profile + 镜像
元数据）和实际的镜像字节流。这两个端点跟 §13 一样豁免 Basic Auth，依旧用
machine_uuid 一致性校验做 foot-gun guard——不是认证。

> ⚠️ 安全姿态延续 §13：machine_uuid 是一致性校验不是安全边界；blob 端点
> 没有 machine_uuid 检查，只靠 32-hex 镜像 ID 不可猜（跟 `/boot/*` 静态
> 文件同一档：不可猜 URL 就是能力 token）。

### 14.1 端点速查

| 方法 | 路径 | 鉴权 | 返回 |
|---|---|---|---|
| GET | `/api/v1/agent/jobs/{id}/spec?machine_uuid=<uuid>` | machine_uuid 必须匹配 job 的 owner | 200 `InstallSpec` JSON |
| GET | `/api/v1/agent/images/{id}/blob` | 无（不可猜 ID） | 200 镜像字节流；支持 `Range` / `If-Modified-Since`（`http.ServeFile`） |

错误码：

- `/spec`：400（缺 machine_uuid）/ 403（machine_uuid 与 owner 不符）/
  404（job 不存在）/ 500（依赖的 binding/profile/image 行缺失，视为脏数据）/
  503（controller 未配置 fetchers，仅出现在精简构造路径上）。
- `/blob`：400（id 非 32-hex）/ 404（catalog 没有这个镜像）。

### 14.2 InstallSpec 结构（示例）

```json
{
  "job_id": "abcd1234abcd1234abcd1234abcd1234",
  "machine_uuid": "4c4c4544-0058-3210-8053-c5c04f463830",
  "image_id": "9f9f9f9f9f9f9f9f9f9f9f9f9f9f9f9f",
  "image_blob_url": "/api/v1/agent/images/9f9f9f9f9f9f9f9f9f9f9f9f9f9f9f9f/blob",
  "image_sha256": "deadbeef...",
  "image_format": "qcow2",
  "profile": { "id": "...", "name": "ubuntu-default", "hostname_template": "node-{n}",
               "root_password_hash": "$6$...", "target_disk": {...}, "network": {...} },
  "binding":  { "machine_uuid": "...", "image_id": "...", "profile_id": "...",
                "desired_state": "install", "hostname": "node-007" }
}
```

`image_blob_url` 是相对路径——agent 用自己的 controller baseURL 拼成完整
URL 后流式下载，边下边校验 sha256，写入目标盘。

### 14.3 agent 拉取顺序

```text
GET  /api/v1/agent/jobs/{id}/spec?machine_uuid=<self>    → InstallSpec
GET  /api/v1/agent/images/<image_id>/blob                → 流式写盘，并行算 sha256
... 校验 sha256 == spec.image_sha256；不符则 POST /fail
... cloud-init seed（profile.hostname_template + binding.hostname 等）
POST /api/v1/agent/jobs/{id}/succeed  {machine_uuid}
```

## 15. Util: SHA-512 crypt（M2.3-9 新增）

operator UI 在创建 / 编辑 profile 时需要 `$6$…` 形式的 sha512crypt 哈希，
直接把明文密码塞进 profile JSON 既不安全也容易写错；这个端点把哈希生成
留在 controller 进程内完成，浏览器只发送 plaintext password 一次。

> 这个端点是 admin-only 工具：跟整个 `/api/v1/util*` 子树一样要求
> Basic Auth，agent 永远不会调用它。

### 15.1 端点

| 方法 | 路径 | 鉴权 |
|---|---|---|
| POST | `/api/v1/util/crypt-sha512` | Basic Auth |

请求体：

```json
{ "password": "plain-text-password" }
```

- 长度限制：8 ≤ len ≤ 128 字符（4 KiB body cap，超出 400）
- 未知字段：拒绝（`DisallowUnknownFields`，400）

200 响应：

```json
{ "hash": "$6$<16-char salt>$<86-char hash>" }
```

错误：
- `400`：JSON 解析失败 / 含未知字段 / 密码长度越界 / `mkpasswd` 不可用或调用失败
- `401`：未提供 Basic Auth

### 15.2 实现细节

- controller 进程在每次调用时生成 16 字节随机 salt（`crypto/rand`，base64
  alphabet 映射进 mkpasswd 的 `[./A-Za-z0-9]`），shell out 到 `mkpasswd
  -m sha-512 -S <salt> --stdin`（来自 `whois` 包）。
- 调用整体 5s 超时；plaintext password 仅在内存 + 子进程 stdin 中存在，
  从不落盘，从不写日志。controller 也不会把生成的 hash 写到 INFO 日志
  （只记一条 `generated sha512crypt hash` 摘要）。
- 同一明文密码每次会产生不同 hash（salt 不同）——这是 crypt(3) 的预期
  行为。

## 16. Auth: 登录 / 登出 / who-am-I（M2.3-11 新增）

控制面 UI 用 form-based 登录页（`/ui/login`）取代浏览器原生 Basic Auth
弹框。鉴权状态保存在 server-side `sessions` 表，浏览器拿一个
`metalkit_session=<64-hex>` cookie。Basic Auth **没有**被废弃 —— curl /
agent / CI 仍可直接走 Basic Auth；任何受保护端点接受 cookie **或** Basic
（OR 关系）。

cookie 属性：`HttpOnly`、`SameSite=Strict`、`Path=/`、`Max-Age=604800`
（7 天）；M2 内网 HTTP 阶段 `Secure` 未置位，M3 上 HTTPS 时再打开。
session 滑动续期：每次命中且 `last_seen_at` 距现在 > 1 小时就把
`expires_at` 推到 now+7d。GC 每小时一次。

未鉴权 UI 路径（`/ui/*`，浏览器导航 / `Accept: text/html`）→ 302 到
`/ui/login?next=<urlencoded原路径>`；未鉴权 API 路径 → 401 JSON
`{"error":"unauthorized"}`，**不再返回 `WWW-Authenticate: Basic`**
（避免浏览器原生弹框回归）。

### 16.1 端点

| 方法 | 路径 | 鉴权 |
|---|---|---|
| POST | `/api/v1/auth/login`  | open |
| POST | `/api/v1/auth/logout` | open（idempotent，无 cookie 也 204）|
| GET  | `/api/v1/auth/me`     | cookie 或 Basic |

#### POST /api/v1/auth/login

请求体：

```json
{ "username": "admin", "password": "metalkit" }
```

- 4 KiB body cap，`DisallowUnknownFields`
- 用 `subtle.ConstantTimeCompare` 校验 username + password，两次比较都跑
  完再合并结果，避免时序泄漏哪一段错了

成功 200：

```json
{ "username": "admin" }
```

并 `Set-Cookie: metalkit_session=<64-hex>; Path=/; HttpOnly; SameSite=Strict;
Max-Age=604800`

失败 401：

```json
{ "error": "invalid credentials" }
```

（empty body / 解析失败 / 字段缺失 / 用户错 / 密码错 全部映射到这一个
响应 —— 不区分原因）

503：当 `--admin-pass` 在配置里为空（auth 全局关闭）时，登录无意义，
直接拒绝。

#### POST /api/v1/auth/logout

无 body。Always 204；如果带 cookie，则在 sessions 表中删除该 row；
不带 cookie 也 204（idempotent）。

响应里始终 `Set-Cookie: metalkit_session=; Path=/; HttpOnly; SameSite=Strict;
Max-Age=0` —— 即使原本就没 cookie，也明确发一次清空指令，让浏览器
立即丢弃。

#### GET /api/v1/auth/me

200 时回当前登录人：

```json
{ "username": "admin" }
```

未鉴权时走通用 401 JSON 响应。该端点的用途仅是 UI 顶栏 "右上显示
当前登录人" 的填充；不要把它当生死探活。

### 16.2 客户端示例

**浏览器（自动管理 cookie）**：

```javascript
// 登录
await fetch("/api/v1/auth/login", {
  method: "POST",
  headers: {"Content-Type":"application/json"},
  credentials: "same-origin",
  body: JSON.stringify({username: "admin", password: "metalkit"}),
});
// 之后所有 fetch 自动带上 metalkit_session cookie
```

**curl（继续走 Basic Auth）**：

```bash
# 普通 API 调用 — 无需登录
curl -u admin:metalkit http://192.168.10.147:8080/api/v1/machines

# 或想用 cookie 路径：先登录拿 cookie jar
curl -c /tmp/cj -X POST -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"metalkit"}' \
  http://192.168.10.147:8080/api/v1/auth/login
curl -b /tmp/cj http://192.168.10.147:8080/api/v1/auth/me
# → {"username":"admin"}
```

### 16.3 实现细节

- session ID = 32 random bytes from `crypto/rand`, hex-encoded → 64-char
  lowercase hex; 抗碰撞 + 不可猜测
- 存储：SQLite `sessions(id, username, created_at, last_seen_at,
  expires_at)`，`expires_at` 上有 index 供 GC 扫描
- 滑动续期阈值 `sessionTouchInterval = 1h`：太密会让每个 GET 都触发
  一次 UPDATE；太松会让长开标签页的用户 7 天后被踢
- 鉴权失败的日志：只记 username，绝不记 password、绝不记 cookie 值
  （session_id 也只截前 8 位作摘要）
- 安全模型：M2 内网 HTTP + 单 admin 帐号 + SameSite=Strict 已足够；
  CSRF token / rate limit / 多用户 / SSO 都推到 M3+

