# MetalKit WebUI Vue 重写方案

## 一、现状与目标

**现状**：`internal/webui/assets/` 下约 7900 行零依赖手写 HTML/JS/CSS（10 页面 + 12 个 JS + 1286 行 style.css），经 `go:embed` 嵌入 controller 单二进制，`server.go`（170 行）按页面注册路由。API 为 `/api/v1` REST，cookie+BasicAuth 认证，全部轮询交互（无 WebSocket）。

**目标**：用 Vue 3 + TypeScript + Vite + Element Plus 重写全部 10 个页面，产出仍嵌入 controller 二进制，**对部署侧（tarball/install.sh/systemd）零影响**。

**技术选型**（已与用户确认）：
- Vue 3 + TypeScript + Vite（构建机已有 Node v23.11.1 + npm 10.9.2）
- Element Plus + icons，unplugin 按需引入，locale zh-cn，暗色模式沿用 `prefers-color-scheme` 自动跟随
- Vue Router 4（history 模式，base `/ui/`）、Pinia（轻量状态）
- Vitest 覆盖从 common.js 移植的纯逻辑（子网校验、CSV 解析、文件名识别、格式化）
- 一次性全量重写；构建产物提交 git

## 二、目录结构

```
frontend/                      # Vue 工程源码（新增，node_modules 忽略）
  package.json / vite.config.ts / tsconfig.json
  index.html                   # 含 <meta name="metalkit-api-base">，沿用现有 API base 机制
  src/
    main.ts / App.vue / router/index.ts
    api/                       # 类型化 client（对齐 Go 结构体：MachineSummary/Job/JobLog/Image/Profile/Binding/Subnet/Bmc/DHCPSettings…）
    lib/                       # 纯逻辑（+vitest）：format.ts、net.ts(hostInSubnet/IPv4/CIDR)、csv.ts、filename.ts、password.ts
    composables/               # usePolling（自适应+visibilitychange 暂停+倒计时）、useApi（401→login）、URL query 同步
    stores/                    # Pinia: auth
    components/                # AppShell/Topbar、CopyableText、StatusDot、InstallDialog、BmcCredentialDialog、NicSelector、BondEditor、TargetDiskPicker、LogTerminal 等
    views/                     # 10 个视图：Login/Machines/MachineDetail/Images/Profiles/Subnets/Bmc/Jobs/JobDetail/Settings
internal/webui/assets/         # Vite 构建产物直接输出（提交 git），旧的 23 个手写文件清空替换
```

## 三、必须保真的业务逻辑（重写核心风险点）

1. **装机弹窗**（common.js `openInstallModal` ~660 行）：目标盘三态（smallest/by-wwn/by-path）、NIC selector、Bond 全字段（mode/slaves/miimon/lacp_rate/xmit_hash/primary）、profile→subnet 联动、静态 IP 客户端子网校验、🎲 随机密码；提交 `PUT /bindings/{uuid}`，bond 永远显式发（null=清除）
2. **profiles 表单**（1122 行）：字段与校验对齐 Go `profiles/validate.go`；root 密码先 `POST /util/crypt-sha512` 换 hash，明文不出表单
3. **分块上传**（images）：`POST /images/uploads` → 8MiB `PUT chunks/{n}` → `finalize`；中止/失败 DELETE 清理
4. **作业日志增量 tail**：`GET /jobs/{id}/logs?since_id=N`；作业详情自适应轮询（running 1s / 终态 5s 收尾后停）
5. **BMC 页**：IPMI 测试/电源操作（危险操作需输入"确认"）、onboard、RFC4180 CSV 批量导入
6. **轮询**：列表 30s+倒计时+页面隐藏暂停；任务列表 5s；过滤条件与 URL query 双向同步
7. **认证流**：401 统一跳 `/ui/login?next=...`；`/auth/me` 启动时取用户

## 四、Go 侧改动（仅 `internal/webui/`）

`server.go` 改为 SPA 模式：
- `GET /ui/assets/` → dist `assets/` 子树的 FileServer（Vite hash 文件在此前缀下，auth.go 白名单 `/ui/assets/*` 不变）
- `GET /ui/assets/` → dist assets 子树 FileServer（Vite hash 文件在此前缀，auth.go 白名单不变）；`/ui/` 子树 → SPA fallback（存在真实文件则 serve，否则回 index.html，no-cache）；`/ui`→301 保留；per-page 路由全删
- `server_test.go` 重写为 SPA 契约测试；认证语义不变（/ui/login、/ui/assets/* 放行）

## 五、构建链改动

- Makefile 新增 `frontend` target；`build` 不依赖 npm（dist 已提交）；DEVELOP.md/DEPLOY.md/docs/features.md §11 同步
- Vite：`base:"/ui/"`、`outDir:"../internal/webui/assets"`、`emptyOutDir:true`；dev proxy `/api/v1`→`:8080`
- `.gitignore` 增加 `frontend/node_modules/`；提交 lockfile

## 六、实施步骤（每阶段可验证）

1. 从 dev-master 切 `feat/vue-webui` 分支；初始化 Vite+Vue3+TS 工程与 EP 按需引入
2. 基础设施：api client、auth store、router、AppShell、LoginView；`vue-tsc`+build 通过
3. 纯逻辑移植 + vitest（断言与 common.js 现行为一致）
4. 简单页面：Settings/Subnets/Images（含分块上传）/Bmc
5. 机器列表+详情（装机面板、上报历史、12 个硬件折叠区、原始 JSON）
6. 作业列表+详情（自适应轮询、时间线、增量日志、导出）
7. 复杂表单：ProfilesView、InstallDialog、BmcCredentialDialog
8. 切换：重写 server.go/server_test.go，npm build 清空替换 assets，更新 Makefile/.gitignore/文档
9. 验证：`make frontend && make build && make test` + `vitest run`；启动 controller 浏览器端到端冒烟；self-review 后按仓库中文 commit 风格分阶段提交

## 七、边界

不新增后端 API、不改认证中间件、不动 agent/PXE/DHCP/TFTP、不引入 i18n（文案保持中文）。