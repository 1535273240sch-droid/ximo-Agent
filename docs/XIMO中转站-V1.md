# XIMO 中转站 V1 —— 实施文档

- 落点分支：`v2`（工作树 `C:/Users/新建文件夹/repo/v2`；module `github.com/ximo888ok-netizen/ximo-agent`，`go 1.27.1`）
- **基线事实：`v2` 分支比 `v2.1.0` 标签更新。** 证据【静态】：同仓 `git ls-tree --name-only v2.1.0` 列出的顶层没有 `plugin/`，`internal/` 下也没有 `gateway/` 与 `memory/`；这三者（外加 `cmd/ximo-gateway`、`migrations/0003_gateway.sql`）只存在于 `v2`。`v2` 工作树本身不带 `.git`，无法在本工作区直接跑 `git log v2.1.0..v2`，上面的对比是在同仓另一份带 `.git` 的检出上做的。
- **插件侧现状（2026-09-26 清理后）**：`v2` 早期还带过一个主仓内的 `cmd/ximo-cli`（本产品自己的配置 CLI），**现已按「主仓只保留插件、插件独立成仓」的要求整体删除**，仓库内 `cmd/` 只剩 `ximo-agent` 与 `ximo-gateway`。该 CLI 的 `login` / `models` / `apply` / `doctor` 能力**由独立 module 的 `plugin/`（二进制 `ximo-plugin`）统一承接**，并新增本地界面 `ximo-plugin ui`。见 §1.1、§1.6、§2.4。
- 本文写作时间：2026-09-26 11:53（+0800）；**2026-09-26 复核修订**：D1 / D5 / D6 标记为已修并补上修复后的真实行号，`release/` 相关表述按 `v2` 分支现状重写（见 §4.D3），新增 §1.6（插件与 mem0 并入）与预占回收的交付物/flag 条目。**修订只改本文档与 `tests/gateway/e2e_test.go` 的注释、`README.md` 末尾追加一节，未改动仓库其它既有文件。**
- 契约依据：`recon/V1-接口冻结.md`（§0 硬约束、§10 契约修订、§11 HTTP 契约、§12 Round 3 决定）
- 交叉参考：`recon/gap-map-design-v1-vs-v2go.md`（设计文档 ↔ 仓库差距映射）

## 0. 证据等级（先看这条）

本文所有结论按来源分两类，正文里逐处标注：

| 标记 | 含义 |
| --- | --- |
| **【实跑】** | 在本工作区真实执行过命令（含真二进制 + 真 HTTP 往返），结果可复现（见 §5） |
| **【静态】** | 只读代码/迁移/脚本得出，未执行 |

两个前提：

1. **本工作区不存在**《XIMO 中转站 + Agent 插件系统 技术设计方案 V1.0》原文（全盘检索 `.md/.docx/.pdf` 只有 `recon/*.md` 与本仓库自己的文档）。因此 §3 差异表的「文档要求」一列取自**任务书列出的条目 + 契约 §11.6 / §12.4 的「本轮明确不做」清单**，不是逐字核对设计文档原文。若设计文档中某条目实际指的是别的机制，需人工复核。
2. **本文写作过程中，`cmd/ximo-gateway`（11:51）与 `tests/gateway/e2e_test.go`（11:51，11:53 定稿）落地**。§1.3、§2.2、§4、§5 已按落地后的真实代码与实跑结果写；本文初稿里「主程序不存在」的判断已作废（保留说明见 §4.D4）。

---

## 1. 这一版做了什么

### 1.1 交付物清单（文件级）

| 类别 | 路径 | 说明 |
| --- | --- | --- |
| 迁移 | `migrations/0003_gateway.sql`（214 行） | 网关域 12 张表 `gw_*`，版本号连续（3） |
| 类型 | `internal/gateway/model/model.go`（211 行） | 冻结：类型 + 哨兵错误 + 状态/协议/账本常量，无逻辑 |
| HTTP 原语 | `internal/gateway/httpx/httpx.go`（175 行） | 冻结：`Route`/`WriteJSON`/`WriteError`/`DecodeJSON`/`WriteMappedError`/`Bearer`/`ClientIP`/`RequestID`/`StatusFor` |
| 数据访问 | `internal/gateway/store/{store,account,quota,usage,catalog}.go` | `Store` 门面；一切写走 `sqlite.DB.WithTx` |
| 额度服务 | `internal/quota/{service,estimate,errors,doc}.go` | 预占/结算/归还/管理调整/回收，单位微单位 int64 |
| 账号服务 | `internal/account/{service,user,apikey,session,device,password,token,store}.go` | 口令哈希、API Key、不透明令牌会话、设备码流程 |
| 目录 | `internal/gateway/catalog/catalog.go` | 候选路由（priority 升序）+ 对外模型投影 |
| 上游池 | `internal/gateway/upstream/{pool,spec}.go` | per-provider `provider.Client` + 熔断 + 限速 + 密钥解析 |
| HTTP 骨架 | `internal/gateway/{config,server,middleware,auth,ratelimit}.go` | 配置、ServeMux 装配、中间件链、鉴权、限流 |
| API 子包 | `internal/gateway/api/{meta,openai,anthropic,admin}/**` | 各导出 `Routes(Deps) []httpx.Route`（meta 另导出 `UserRoutes`/`PublicRoutes`） |
| **主程序** | `cmd/ximo-gateway/{main,flags,wire,secrets}.go`（约 660 行）+ `main_test.go`（392 行） | 装配与启动；flag 与 §12.1 逐字一致 |
| 插件侧（主仓内 CLI **已删除**） | 原 `cmd/ximo-cli/**` —— **本仓已无此目录** | 该 CLI 面向「把这台机器的 Agent 接到本网关」，提供 `login` / `models` / `apply` / `doctor`；按用户「只保留插件」的要求整体移除。能力**由独立 module 的 `plugin/` 统一承接**（见下行与 §2.4），不是「存在但未提及」，也不是「已删除却仍当存在」 |
| **E2E** | `tests/gateway/e2e_test.go`（1634 行） | 真二进制驱动：`TestGatewayEndToEnd` + 三条已修缺陷的回归用例（D1 / D5 / 预占不泄漏） |
| 既有缺陷修复 | `cmd/ximo-agent/main.go` 的 `--migrate-only` | 已落地，见 §4.D3′（`v2` 分支当前**无调用方**） |
| **预占回收** | `cmd/ximo-gateway/reaper.go`（130 行）+ `flags.go` 的 `--reap-interval` / `--reap-limit` | 本轮修 D6：后台周期回收超时 `held` 预占，随主 ctx 优雅退出；见 §4.D6 |
| 长期记忆 | `internal/memory/{client,config,recall,service,writeback,types}.go` | mem0 集成**已并入本分支**；默认关闭，未启用或服务不可用时全链路静默降级（见 §1.6） |
| **独立插件（独立 module，插件侧唯一产物）** | `plugin/**`（module `github.com/1535273240sch-droid/ximo-plugin`） | 通用 Agent 接入 **CLI**（`detect` / `adapters` / `login` / `models` / `usage` / `apply` / `print-env` / `doctor`）+ **本地界面**（`ximo-plugin ui`，玻璃质感 + 羊皮卷风格）；**不 import 主仓库 `internal/**`**；见 §1.6 与 §2.4 |

### 1.2 包结构与关键构造签名【静态】

```
internal/gateway/
├── config.go        gateway.Config / ParseConfig(args) / DefaultConfig() / Config.Validate() / Config.LogFields()
├── server.go        gateway.New(cfg, routes []httpx.Route, logger) (*Server, error); Server.Run(ctx) / Server.Serve(ctx, ln)
├── middleware.go    chain(): Recover → RequestID → AccessLog → LimitBody → Timeout → mux
├── auth.go          gateway.NewAuthenticator(verifier CredentialVerifier, adminToken string, logger)
│                    Authenticator.RequireUser/RequireAdmin/RequireUserRoutes/RequireAdminRoutes
│                    gateway.PrincipalFrom(ctx) → Principal{Kind,User,Key,Credential}
├── ratelimit.go     gateway.NewKeyLimiter(perMin int); KeyLimiter.Middleware / LimitRoutes
├── httpx/           httpx.Route{Pattern,Handler}; httpx.StatusFor(err) → (status, code)
├── model/           冻结类型与哨兵错误（model.Err*）
├── store/           store.New(db *sqlite.DB) *Store
├── catalog/         catalog.New(st store, probe Probe) *Catalog; Candidates / PublicModels
├── upstream/        upstream.NewWithOptions(st, secrets SecretResolver, opts Options) *Pool
│                    Client / Healthy / MarkFailure / MarkSuccess / Invalidate
└── api/
    ├── meta/        meta.Routes(d) / meta.UserRoutes(d) / meta.PublicRoutes(d)
    ├── openai/      openai.Routes(d)
    ├── anthropic/   anthropic.Routes(d)
    └── admin/       admin.Routes(d)

internal/quota/      quota.New(st accountStore) *Service; Reserve / Settle / SettleWithUsage / Release / Adjust / Account / ReapExpired
internal/account/    account.New(st Store, pepper []byte) *Service; CreateUser / Authenticate / IssueSession /
                     CreateAPIKey / VerifyAPIKey / VerifyAccess / Refresh /
                     StartDeviceLogin / PollDeviceLogin / ApproveDeviceLogin / HashPassword / VerifyPassword
cmd/ximo-gateway/    parseOptions(args) / runWithContext(ctx,args,...) / buildStack(...) / mountRoutes(...)
```

关键取舍（与契约一致，代码注释里也逐条写了理由）：

- **接口由消费方定义**（契约 §10.2）：各服务包自带窄接口，`*gwstore.Store` / `*account.Service` / `*quota.Service` / `*catalog.Catalog` / `*upstream.Pool` 结构性满足，装配方不需要写适配器；唯一例外是 `internal/secrets.Manager`（`Get`）→ `upstream.SecretResolver`（`Resolve`）的一层小适配（`cmd/ximo-gateway/secrets.go:54`）。
- **`internal/gateway` 不 import 任何 api 子包**（`internal/gateway/config.go:4`）：路由由 main 汇总后交给 `gateway.New`。
- **协议过滤在调用方做**（契约 §11.1.4）：`catalog.Candidates` 不接收「本次请求所需协议」入参，由 `api/openai`、`api/anthropic` 各自按 `Candidate.Provider.Protocol == "openai-chat"` 过滤。
- **优先级升序**（数值小者优先），与 `idx_gw_provider_models_model(model_id, enabled, priority)` 索引序一致。
- **挂载顺序**：无鉴权（`/v1/health`、`/v1/capabilities`、`/v1/auth/*`，只套限流）→ 用户态（`RequireUserRoutes(LimitRoutes(...))`）→ 管理态（`RequireAdminRoutes(LimitRoutes(...))`）。先套限流再套认证，执行顺序才是「认证 → 限流 → handler」（`cmd/ximo-gateway/wire.go:68-73` 说明了理由：反过来会让未认证请求按 IP 分桶，同一 NAT 后互相顶掉配额）。

### 1.3 命令（`cmd/ximo-gateway`）——与契约 §12.1 逐字一致

**【实跑】`--help` 实际输出（原文摘录，含默认值）：**

```
ximo-gateway v1.0.0-alpha —— XIMO 中转站服务端

   -addr string            监听地址；默认只绑回环，不要默认对外 (default "127.0.0.1:8600")
   -admin-token string     管理口令（X-Admin-Token）；也可用环境变量 XIMO_GATEWAY_ADMIN_TOKEN；两者都没有则拒绝启动
   -db string              SQLite 数据库文件路径 (default "C:\Users\<you>\AppData\Roaming\ximo-agent\gateway\gateway.db")
   -max-output-tokens int  额度预占时假定的最大输出 token 数 (default 4096)
   -migrate-only           只跑迁移然后退出 0
   -migrations string      迁移脚本目录；未指定时按 exe 同级 → ./migrations → ../migrations → ../../migrations 探测
   -price-micro-per-ktok int  占位单价（微单位 / 1K token）；V1 无真实价目表 (default 1)
   -rate-limit-per-min int  每凭据每分钟请求数上限，0 表示不限流
   -reap-interval duration  超时预占（held）的回收周期，0 表示关闭回收 (default 1m0s)
   -reap-limit int          单次回收的预占条数上限，<=0 时用 quota 包默认值 (default 500)
   -request-timeout duration  单请求总时限，0 表示不限时 (default 5m0s)
   -version                 打印版本后退出 0
```

> 口径：`-reap-interval` / `-reap-limit` **不在契约 §12.1 的冻结列表里**，是本轮修 D6 时新增的**追加项**（默认值保住「启动即开始回收」的期望，未改动任何既有 flag 的语义）。

| flag | 契约 §12.1 | 实测（`cmd/ximo-gateway/flags.go:34-48` 为默认值常量、`:75-86` 为 flag 定义） |
| --- | --- | --- |
| `--addr` | 默认 `127.0.0.1:8600` | ✅ `defaultAddr = "127.0.0.1:8600"` |
| `--db` | 默认 `<XIMO_GATEWAY_HOME\|%APPDATA%\ximo-agent\gateway>\gateway.db` | ✅ `defaultDBPath()`；Windows 走 `APPDATA`（缺则 `USERPROFILE\AppData\Roaming`），macOS 走 `~/Library/Application Support/...`，Linux 走 `XDG_CONFIG_HOME` 或 `~/.config` |
| `--migrations` | exe 同级 → `./migrations` → `../migrations` → `../../migrations` | ✅ `defaultMigrationDirs()`；显式传入时**只认该目录**，找不到即报错（不静默回退） |
| `--admin-token` | 也可用 `XIMO_GATEWAY_ADMIN_TOKEN`；两者都无 → 启动即报错退出（fail closed） | ✅ 缺失时打印 `gateway: 缺少管理令牌…` 并**退出码 2**，且在打开数据库之前返回（**不留下库文件**，实测确认） |
| `--price-micro-per-ktok` | 默认 1 | ✅ |
| `--max-output-tokens` | 默认 4096 | ✅ |
| `--request-timeout` | 默认 5m | ✅ |
| `--rate-limit-per-min` | 默认 0（不限流） | ✅ |
| `--migrate-only` | 只跑迁移然后退出 0 | ✅ 退出码 0；**`v2` 分支当前无任何调用方**（旧分支的升级脚本才有，见 §4.D3′） |
| `--reap-interval` | **不在契约 §12.1 冻结列表内**（本轮修 D6 的追加项） | ✅ 默认 `1m`（`defaultReapInterval`，`:46`）；`0` = 显式关闭回收（不启协程） |
| `--reap-limit` | 同上 | ✅ 默认 `quota.DefaultReapLimit = 500`（`internal/quota/service.go:27`）；`<=0` 时归一为该默认值 |
| `--version` | 打印版本 | ✅ `ximo-gateway v1.0.0-alpha (commit: dev, built: unknown)`，退出码 0 |

**一个读代码时容易踩的口径差**：HTTP 骨架库 `internal/gateway/config.go` 的 `DefaultAddr` 是 `127.0.0.1:8080`、`DefaultRateLimitPerMin` 是 `60`，与本二进制对外承诺的 `8600` / `0` 不同。这是**刻意的**——`flags.go:30-32` 明确写了不复用库默认值，理由是复用会在改动骨架时悄悄改掉对外契约。因此引用默认值一律以 `--help` / `flags.go` 为准，不要看 `internal/gateway/config.go`。

其它环境变量（都是可选，缺省不改变行为）：`XIMO_GATEWAY_HOME`（覆盖网关数据目录）、`XIMO_GATEWAY_LOG_LEVEL`、`XIMO_GATEWAY_PEPPER`（口令 pepper；**一旦使用必须跨重启一致，否则全员登录不上**。默认不生成随机 pepper，原因写在 `flags.go:147-155`：网关库里没有 kv 表，`internal/secrets` 的 ref 由明文值派生，无法「按固定名字取回同一秘密」）。

**启动顺序（`cmd/ximo-gateway/main.go` + `wire.go`）**：解析 flag/env → 校验管理令牌（fail closed）→ 打开 SQLite + 应用迁移 → 组装 `gwstore.New` / `account.New(gwStore, pepper)` / `quota.New` / `secretsForGateway` + `secretResolver` 适配 → `upstream.NewWithOptions` → `catalog.New(gwStore, pool)`（Probe 就是池子）→ `gateway.NewAuthenticator` / `NewKeyLimiter` → `mountRoutes` → `gateway.New` → 自行 `net.Listen`（这样 `--addr :0` 时能报出真实端口）→ 收 SIGINT/SIGTERM 优雅停机。启动日志里管理令牌**只以 `admin_fingerprint`（sha256 前 8 位十六进制）+ 布尔量出现**；实测日志中搜索令牌明文命中数为 0。

### 1.4 HTTP 接口清单（照契约 §11.3，已实跑核对）

鉴权模型：用户态 `Authorization: Bearer <API Key 或 access token>`（先按 `ximo_sk_` 前缀走 API Key，再按 `gwa_` 走 access token，`internal/gateway/auth.go:134`）；管理态 `X-Admin-Token`（SHA-256 后恒定时间比较，`auth.go:163`）。

**无鉴权**（`internal/gateway/api/meta/meta.go:119-126`）

| 方法+路径 | 实测响应（2026-09-26 实跑） |
| --- | --- |
| `GET /v1/health` | `{"status":"ok","version":"v1.0.0-alpha","time_ms":1790394791282}`；**刻意不做依赖体检** |
| `GET /v1/capabilities` | `{"protocol_version":1,"server_version":"v1.0.0-alpha","features":{"device_login":true,"token_refresh":true,"api_keys":true,"openai_chat":true,"anthropic_messages":true,"streaming":true},"limits":{"max_request_bytes":33554432,"default_page_size":100,"max_page_size":200,"access_token_ttl_seconds":900,"refresh_token_ttl_seconds":2592000,"device_code_ttl_seconds":600,"device_poll_interval_seconds":5}}` |
| `POST /v1/auth/device` | `{"device_code":"gwd_<64hex>","user_code":"G3QN2RVH","expires_in":600,"interval":5}` |
| `POST /v1/auth/token` `{"device_code"}` | `{"access_token":"gwa_<64hex>","refresh_token":"gwr_<64hex>","expires_in":900,"token_type":"Bearer"}`；待授权 → 400 `authorization_pending` |
| `POST /v1/auth/refresh` `{"refresh_token"}` | 200 新令牌对（旧 refresh 立即失效） |
| `POST /v1/auth/login` `{"username","password"}` | 200 同上形状令牌对 |

**用户态**

| 方法+路径 | 实测行为 |
| --- | --- |
| `GET /v1/models` | `{"data":[{"id":"gpt-4o-mini","display_name":"GPT-4o mini","provider":"prov-local","protocols":["openai-chat"],"capabilities":{"stream":true,"vision":false,"tools":true,"reasoning":false},"enabled":true}]}` |
| `GET /v1/usage?limit=&offset=` | 每条含 `request_id/model_id/provider_id/status/input_tokens/output_tokens/latency_ms/cost_micro/created_at`；`limit` 上限 200（超出静默收紧） |
| `POST /v1/chat/completions` | OpenAI 兼容；`stream:true` 走 SSE |
| `POST /v1/messages` | Anthropic 兼容入站 → 翻译成 OpenAI 上游 |

**管理态**（`api/admin/routes.go:35`，全部经 `h.guard` 二次校验 `X-Admin-Token` 且写操作同请求落 `gw_audit`）

| 方法+路径 | 关键入参（取自 handler 结构体，实测可用） |
| --- | --- |
| `POST /admin/users` | `{"username","password","group_id"}` → `{"id":"usr_…","username","status","group_id","created_at","updated_at"}` |
| `GET /admin/users?limit=&offset=` | 列表 |
| `POST /admin/users/{id}/status` | `{"status"}` |
| `POST /admin/keys` | `{"user_id","ttl_seconds"}` → `{"api_key":"ximo_sk_<32hex>","key":{…}}`；明文**只此一次** |
| `GET /admin/keys?user_id=` | `user_id` 必填 |
| `DELETE /admin/keys/{id}` | — |
| `POST /admin/quota/adjust` | `{"user_id","kind","amount","reason","idempotency_key"}` → `{"ledger":{…},"account":{…}}`；**同 idempotency_key 重放返回同一账本行、不重复入账（实测）** |
| `GET /admin/quota/accounts/{user_id}` | 含 `available` |
| `GET /admin/quota/ledger?user_id=&limit=&offset=` | `user_id` 必填 |
| `GET /admin/models?enabled=` / `POST /admin/models` / `GET /admin/models/{id}` | 写入：`{"id"/"model_id","display_name","capabilities"/"capabilities_json","enabled"}`（局部更新语义） |
| `GET /admin/providers?enabled=` / `POST /admin/providers` / `GET /admin/providers/{id}` | 写入：`{"id","name","endpoint","protocol","status","api_key"/"api_key_ref","config"/"config_json","timeout_ms","weight"}`；`protocol` 只接受 `openai-chat`，其它值 400（实测：`protocol 不支持：V1 上游只支持 openai-chat`，且该次失败**也落 audit**，`result:"error"`） |
| `POST /admin/providers/{id}/models` | `{"model_id","upstream_model_id","enabled","priority"}`（priority 小者优先） |
| `GET /admin/usage?user_id=&limit=&offset=` | `user_id` 必填 |
| `GET /admin/audit?limit=&offset=` | 含失败留痕 |
| `POST /admin/device/approve` | `{"user_code","user_id"}` → `{"status":"approved","user_code","user_id"}` |

审计 action 词表：`user.create` / `user.status` / `key.create` / `key.revoke` / `quota.adjust` / `model.upsert` / `provider.upsert` / `provider.model.upsert` / `device.approve`；`actor` 恒为 `"admin"`（`api/admin/deps.go:161`），`result` ∈ `ok|error`。

**请求生命周期**（`api/openai/chat.go` 的 `chatCompletions`，与契约 §11.4 顺序一致）：
`request_id → 认证 → 限流(中间件) → 模型启用校验（404 model_not_found）→ 额度预占（402 insufficient_quota，不进上游）→ 候选路由（过滤协议/健康）→ 逐个候选尝试 → 结算 + 写 usage → 返回`。
候选全失败 → 502 `provider_unavailable` 且**必须归还预占**；客户端断开 → 终态 `client_canceled`；不可重试错误 → 不换候选、直接返回并归还预占。生命周期状态机收在 `api/openai/charge.go` 的 `charge` 类型里（`settle` / `release` / `releaseOnly` / `abandon` 四条出口，`done` 位防双重退款）。

**终态词表**（契约 §12.2）：成功终态统一记为 `settled`（由 `quota.SettleWithUsage` 写入，实测 usage 行 `status:"settled"`）；失败终态 `upstream_error` / `upstream_timeout` / `client_canceled`。

**SSE 约定**（契约 §11.5，实测流式响应体）：

```
data: {"id":"chatcmpl-<request_id>",...,"choices":[{"index":0,"delta":{"role":"assistant","content":"po"},"finish_reason":null}]}
data: {... "delta":{"content":"ng"},"finish_reason":null}
data: {... "delta":{},"finish_reason":"stop"}
data: {... "choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}
data: [DONE]
```

首字节前失败返回普通 JSON + 非 2xx；首字节后失败发错误事件并终止（**刻意不发 `[DONE]`**，让客户端能区分「正常收尾」与「中途失败」），同时落 usage 行标明终态。

### 1.5 SQLite 表（照 `migrations/0003_gateway.sql`）

约定与 `0001_init.sql` 一致：时间戳一律 `INTEGER` unix 毫秒（0 = 无/未用）；ID 一律 `TEXT` 由应用层生成；金额一律 `INTEGER` 微单位（1e-6 credit），**禁 float**；外键显式声明 + `PRAGMA foreign_keys=ON`；索引显式创建。表名统一 `gw_` 前缀，与 Agent 运行时表隔离。

| 表 | 用途与关键约束 |
| --- | --- |
| `gw_users` | 登录主体；`username UNIQUE`；`password_hash` 形如 `pbkdf2-sha256$<iter>$<saltB64>$<hashB64>`；索引 `(status,created_at)`、`(group_id,created_at)` |
| `gw_api_keys` | 只存 `key_hash`（sha256，`UNIQUE`）+ 展示用 `key_prefix`；`user_id` FK CASCADE |
| `gw_auth_sessions` | 不透明令牌会话，`access_hash`/`refresh_hash` 各建索引（刻意**不设 UNIQUE**，唯一性由应用层轮换逻辑保证） |
| `gw_device_codes` | 设备授权登录；`device_code_hash` 主键、`user_code UNIQUE`；`user_id` 可空 + `ON DELETE SET NULL` |
| `gw_quota_accounts` | 每用户一行快照；`reserved_amount >= 0` 有 DB CHECK；`available = total - used - reserved`。**建户即建这一行**（`store.CreateUser` 在同一事务里插入 0 额度账户行，本轮修 D5 的第一层，见 §4.D5） |
| `gw_quota_ledger` | append-only 账本；`idempotency_key UNIQUE`（非幂等来源写 `NULL`，SQLite 中多个 NULL 互不冲突）；每行必带 `balance_after`、`reserved_after` |
| `gw_quota_reservations` | 预占行；**部分唯一索引** `(user_id, request_id) WHERE status='held'` 把「同请求至多一条 held」升级为 DB 约束 |
| `gw_models` | 对外模型目录；`capabilities_json` 形如 `{"stream":true,"vision":true,"tools":true,"reasoning":true}` |
| `gw_providers` | 上游服务商；`api_key_ref` 只存 `internal/secrets` 的引用（实测形如 `secretref:v1:e9429ac80772c6ce230e4d939b973a4c`），**绝不存明文** |
| `gw_provider_models` | 模型 → 上游模型名映射；主键 `(provider_id, model_id)`；外键要求 provider 与 model 都已登记 |
| `gw_usage` | 每次上游调用的用量/成本；`request_id` 主键即幂等键（重放不重复计费） |
| `gw_audit` | 管理侧写操作审计；`actor/action/target/result/ip/detail_json` |

与 `0001` 的两点偏差（写在迁移文件头注释里）：`capabilities_json`/`config_json`/`detail_json` 用 `TEXT` 而非 `BLOB`（对应冻结结构体的 `string` 字段，避免 string↔[]byte 混用）；表名前缀 `gw_`。

**对账不变量（实测成立）**：账本 `amount` 统一记为「对 `available` 的有符号增量」，因此
`SumLedgerAmount(userID) == 账户快照的 Available()`（只要充值也经 `AdjustTx` 入账）。实测一次非流式 + 一次流式后：`available=49964000`，账本 5 行合计 `+50000000 -257000 +239000 -257000 +239000 = 49964000` ✅。

### 1.6 同分支一并并入的两块：独立插件 module 与 mem0 记忆

**一、独立插件 module `plugin/`**（module `github.com/1535273240sch-droid/ximo-plugin`，**自带 go.mod**）

- 定位：把**任意** OpenAI 兼容 / Anthropic 兼容中转站接进本机已装好的 Agent；产物是**单个静态二进制**，不依赖 ximo-Agent 运行时，可单独拷到别的机器。
- **硬约束（已遵守）【静态】**：`grep -rn "ximo888ok-netizen/ximo-agent" plugin/` **零命中** —— 该 module 不 import 主仓库任何包（更不用说 `internal/**`）；依赖只有 `gopkg.in/yaml.v3` + `github.com/BurntSushi/toml`（给「写配置文件」形态用）。
- 内置适配器 9 个（`plugin/internal/spec/specs/*.json`，**纯数据**，新增一个 Agent = 加一份 JSON，不改代码）：

  | id | 名称 | 改什么 |
  | --- | --- | --- |
  | `claude-code` | Claude Code | 环境变量 `ANTHROPIC_BASE_URL` / `ANTHROPIC_AUTH_TOKEN` |
  | `codex-cli` | Codex CLI | 环境变量 `OPENAI_BASE_URL` / `OPENAI_API_KEY` |
  | `continue` | Continue | 环境变量 `OPENAI_BASE_URL` / `OPENAI_API_KEY` |
  | `cursor` | Cursor | 环境变量 `OPENAI_BASE_URL` / `OPENAI_API_KEY` |
  | `aider` | aider | 环境变量 `OPENAI_API_BASE` / `OPENAI_API_KEY` |
  | `env-openai` / `env-anthropic` | 仅环境变量（两种协议） | **不落盘**，只打印语句 |
  | `openai-compatible-generic` | 通用 OpenAI 兼容客户端 | 写**本插件自己维护**的 dotenv（`~/.ximo-plugin/openai-compatible.env`） |
  | `ximo-agent` | XIMO Agent（本产品） | **优先走引擎 IPC 帧**，回退改配置文件 |

- 边界与已知风险（照实）：除 `openai-compatible-generic` 与 `ximo-agent` 外，**都不写第三方配置文件**（那些工具的 schema 未核实，宁可不猜）；`apply --dry-run` 的差异 `-`（current）一侧会**回显目标文件已有的明文 key**（未修）；`files[].create=true` 不会创建父目录。逐条见 `plugin/docs/README.md` §5 / §6。
- **插件侧只剩这一处**：主仓内原有的 `cmd/ximo-cli` 已删除，`plugin/` 是插件侧唯一产物；它同时提供 CLI 与本地 UI（`ximo-plugin ui`），两者共用同一套 `internal/**` 实现，见 §2.4。

**二、mem0 长期记忆（`internal/memory/**`）**：本分支已并入 —— 每次 run 开始前召回相关记忆注入上下文、run 结束后把这一轮问答交给 mem0 抽取；默认**关闭**，未启用或服务不可用时整条链路静默降级，请求与行为跟没有该特性时一致。开关与参数在 `feature_flags.memory.mem0` + `memory.*`；部署两条路径、REST 契约与排障见 `docs/长期记忆-mem0.md` 与 `scripts/mem0/README.md`。

> 与本篇的关系：这两块**不属于中转站**，也不改变中转站的任何契约；列在这里只是让读者知道「这个分支上还有什么」，避免把 `plugin/` 或 `internal/memory` 的缺陷误记到网关账上。

---

## 2. 怎么跑起来

### 2.1 构建与静态检查【实跑】

```bash
export GOROOT=/c/ximo-tools/go && export PATH=$GOROOT/bin:$PATH && export GOPROXY=https://goproxy.cn,direct
cd "C:/Users/新建文件夹/repo/v2"
go build ./...                                                              # 通过
go vet ./cmd/ximo-gateway/... ./internal/gateway/... ./internal/quota/... ./internal/account/...   # 通过
```

### 2.2 启动中转站【实跑】

```bash
# 1) 管理口令必须自己给（没有默认值；不给则启动即退出码 2，且不会创建库文件）
export XIMO_GATEWAY_ADMIN_TOKEN='<你自己生成的强口令>'

# 2) 启动（默认只绑回环 127.0.0.1:8600；要对外必须显式改 --addr）
./ximo-gateway.exe --db ./gw.db
# 或一次性指定：./ximo-gateway.exe --addr 127.0.0.1:8600 --db ./gw.db --admin-token '<强口令>'

# 3) 只想跑迁移然后退出（对齐升级脚本的既有契约；`v2` 分支本身没有 release/upgrade.*，见 §4.D3′）
./ximo-gateway.exe --db ./gw.db --migrate-only     # 退出码 0

# 4) 预占回收默认就开着（--reap-interval 默认 1m，设 0 关闭；见 §4.D6）：
#    ./ximo-gateway.exe --db ./gw.db --reap-interval 30s --reap-limit 200
```

实测启动日志（脱敏后的字段）：

```json
{"level":"INFO","message":"ximo-gateway: 迁移已应用","fields":{"dir":"<repo>\\migrations","versions":[1,2,3]}}
{"level":"INFO","message":"ximo-gateway: 启动","fields":{"addr":"127.0.0.1:8790","admin_fingerprint":"…","admin_token_configured":true,"db":"…","listen":"127.0.0.1:8790",…}}
```

**安全默认值（务必遵守）**：

- `--admin-token` **没有默认值**，也没有硬编码兜底：缺失即启动失败（fail closed，实测退出码 2、不创建库文件）。
- 监听地址**默认只绑回环**（`flags.go:34`）；**日志里只出现 `admin_fingerprint`（sha256 前 8 位）与布尔量，不含令牌明文**（实测在日志中 grep 令牌明文命中 0 次）。
- 上游密钥只经 `POST /admin/providers` 的 `api_key` 字段写入 `internal/secrets`，库里只留 `api_key_ref`；**下面的示例一律用占位符**。
- 平台安全存储不可用时（`secretsForGateway`，`cmd/ximo-gateway/secrets.go:18-35`）会降级到环境变量后端（**只能读、不能写**）并打 Warning；两者都不可用时「带明文密钥的 provider 写入」明确失败，**绝不降级为明文落库**。

### 2.3 最小可用流程（curl）—— **【实跑】全部返回实测值**

> 下列命令在本工作区对真二进制执行过（`--price-micro-per-ktok 1000000 --max-output-tokens 256` 以便金额可见；假上游是一个本地 SSE/JSON 服务）。输出为实测原文（`<…>` 处为可变量）。

```bash
GW=http://127.0.0.1:8790
ADMIN='<上面那个 XIMO_GATEWAY_ADMIN_TOKEN>'
H="-H X-Admin-Token:$ADMIN -H Content-Type:application/json"

# 1) 建用户（口令只在此处出现，库里只有 PBKDF2 哈希）
curl -sS $H -X POST $GW/admin/users -d '{"username":"alice","password":"<强口令>","group_id":"default"}'
# → {"id":"usr_740a154809580683","username":"alice","status":"active","group_id":"default",...}

# 2) 充值（**必须做，但原因不是「建账户行」**：`store.CreateUser` 建户时已同事务建好 0 额度
#    账户行（见 §4.D5，本轮已修）；不充值就是可用额 0，首次对话会被 402 `insufficient_quota` 拒掉）
curl -sS $H -X POST $GW/admin/quota/adjust \
  -d '{"user_id":"<usr_…>","kind":"topup","amount":50000000,"reason":"initial","idempotency_key":"topup-alice-1"}'
# → ledger: {"type":"topup","amount":50000000,"balance_after":50000000,"reserved_after":0} / account.available=50000000
# 重放同一 idempotency_key → 返回同一条 ledger（id 相同），available 不变（实测）

# 3) 配上游 provider（api_key 用占位符！会存入平台安全存储，响应只回 api_key_ref）
curl -sS $H -X POST $GW/admin/providers \
  -d '{"id":"prov-local","name":"local","endpoint":"http://127.0.0.1:8800/v1",
       "protocol":"openai-chat","status":"enabled","api_key":"sk-<占位符，替换成真密钥>"}'
# → {"id":"prov-local","api_key_ref":"secretref:v1:<hex>",...}   注意：protocol 只接受 openai-chat

# 4) 建模型 + 把模型映射到该 provider（priority 数值小者优先）
curl -sS $H -X POST $GW/admin/models \
  -d '{"id":"gpt-4o-mini","display_name":"GPT-4o mini","capabilities":{"stream":true,"vision":false,"tools":true,"reasoning":false}}'
curl -sS $H -X POST $GW/admin/providers/prov-local/models \
  -d '{"model_id":"gpt-4o-mini","upstream_model_id":"gpt-4o-mini","enabled":true,"priority":0}'

# 5) 给用户发一枚 API Key（明文只回这一次，请立刻保存）
curl -sS $H -X POST $GW/admin/keys -d '{"user_id":"<usr_…>","ttl_seconds":0}'
# → {"api_key":"ximo_sk_820e1ee3b6df671e37f27ceffa4e7c58","key":{"key_prefix":"ximo_sk_820e",...}}

# 6) 用户态：列模型 + 非流式对话
curl -sS -H "Authorization: Bearer ximo_sk_<32hex>" $GW/v1/models
curl -sS -H "Authorization: Bearer ximo_sk_<32hex>" -H 'Content-Type: application/json' \
  -X POST $GW/v1/chat/completions -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}'
# → {"choices":[{"message":{"content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":7}}

# 7) 流式（带 stream_options.include_usage，尾部会出现 usage 分片）
curl -sS -N -H "Authorization: Bearer ximo_sk_<32hex>" -H 'Content-Type: application/json' \
  -X POST $GW/v1/chat/completions -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}],
       "stream":true,"stream_options":{"include_usage":true}}'
# → data: …finish_reason stop → data: …"choices":[],"usage":{…} → data: [DONE]

# 8) 对账：usage / 账户 / 账本
curl -sS -H "Authorization: Bearer ximo_sk_<32hex>" "$GW/v1/usage?limit=10"
# → 两条 status:"settled"，各 input 11 / output 7 / cost_micro 18000（流式那条同样有真实 token，见 §4.D1）
curl -sS $H $GW/admin/quota/accounts/<usr_…>     # → available=49964000 = total-used-reserved
curl -sS $H "$GW/admin/quota/ledger?user_id=<usr_…>&limit=20"

# 9) 设备码登录链（插件侧交互，无鉴权）
curl -sS -X POST $GW/v1/auth/device                 # → {"device_code":"gwd_…","user_code":"G3QN2RVH","expires_in":600,"interval":5}
curl -sS $H -X POST $GW/admin/device/approve -d '{"user_code":"G3QN2RVH","user_id":"<usr_…>"}'
curl -sS -X POST $GW/v1/auth/token -d '{"device_code":"gwd_…"}'   # → 令牌对；待授权时 400 authorization_pending
```

实测的边界行为（可复现）：

| 场景 | 实测结果 |
| --- | --- |
| 不带 `X-Admin-Token` 调 `/admin/*` | 401 `invalid_admin_token` |
| 模型不存在/未启用 | 404 `model_not_found`（**上游未被调用**，假上游命中计数不变） |
| 账户有额度但不够预占 | 402 `insufficient_quota`（上游未被调用） |
| 账户已冻结 | 403 `account_disabled` |
| **用户无额度账户行**（全新建户、未做任何额度操作） | 本轮已修：建户即建 0 额度账户行，且 `ReserveTx` 把「账户行缺失」归一为 `ErrInsufficientQuota` → **402 `insufficient_quota`**（契约 §11.4，见 §4.D5）。上面这份手工链路是在修复**前**跑的，所以当时这一行记的是 404；修复后的行为由 `tests/gateway/e2e_test.go` 的 `TestGatewayUnfundedUserGets402` 钉住（本轮实跑结果见 §5.1） |
| `--rate-limit-per-min 2` 连打 3 次 | 前 2 次 200，第 3 次 429 |
| 同 `--db` 重启 | 用户/额度/账本/usage 全部保留 |
| 写 provider 时 `protocol` 非 `openai-chat` | 400，且 audit 记 `result:"error"` |

### 2.4 插件侧：独立 module 的 `plugin/`（二进制 `ximo-plugin`）

> **本节按 2026-09-26 清理后的现状重写。** 这里以前写的是「主仓内的 `cmd/ximo-cli`」——该目录**已删除**，仓库里已经没有这个二进制。插件侧现在**只有一个产物**：**独立 module** 的通用接入器 `plugin/`（二进制 `ximo-plugin`），它把**任意**本机 Agent 接到任意兼容中转站，同时提供 CLI 与本地界面。定位见 §1.6 与 `plugin/docs/README.md`。

```bash
cd plugin
go build -o ximo-plugin.exe ./cmd/ximo-plugin          # 独立 module，单独编译；产物是单文件二进制

# 旧的 ximo-cli login/models/apply/doctor 四条能力，现在全部在这里：
./ximo-plugin.exe login  --gateway http://127.0.0.1:8600                    # 设备码流程（用户码在网关侧授权）
./ximo-plugin.exe models --gateway http://127.0.0.1:8600                    # 列模型
./ximo-plugin.exe usage  --gateway http://127.0.0.1:8600 --limit 20         # 查用量
./ximo-plugin.exe apply  --gateway http://127.0.0.1:8600 --spec ximo-agent --dry-run
                                                                            # 优先走引擎 IPC 帧，回退改 <BaseDir>/config.json（先备份）
./ximo-plugin.exe doctor [--gateway http://127.0.0.1:8600]                  # 逐项体检，有 FAIL 则退出码 1
```

命令全集（以 `cmd/ximo-plugin/main.go` 的 `printUsage` 为准，这是**源码实测**的全集）：`detect` / `adapters [list|show <id>]` / `login` / `models` / `usage` / `apply` / `print-env` / `doctor`，外加 `version`。全局 flag：`--home <dir>`（覆盖用户主目录，默认 `$XIMO_PLUGIN_HOME` 或系统主目录）、`--json`、`--no-color`、`--version`、`-h/--help`。这些 flag 比旧 CLI 多，`apply` 另有 `--all` / `--model` / `--api-key` / `--yes` / `--no-ipc` / `--ipc-endpoint` / `--show-secrets` / `--allow-short-lived-token`；`--dry-run` 只生成差异、不落盘（非 tty 下不加 `--yes` 就拒绝写入并报用法错误）。**【静态】**

**本地界面（新增，`ximo-plugin ui`）**：在**回环地址**起一个本地网页界面（不是 Electron，保持单二进制、零外部运行时），风格是**玻璃质感 + 羊皮卷**；静态资源 `//go:embed` 进二进制，运行时零外部请求。**实际 flag 只有 `[--port 8787] [--no-open] [--gateway <url>]`**（`--port 0` 表示由系统分配空闲端口，越界/非数字报用法错误退出码 2），家目录隔离用全局 `--home <dir>`：

```bash
./ximo-plugin.exe ui --gateway http://127.0.0.1:8600     # 默认 127.0.0.1:8787，并尝试打开浏览器
./ximo-plugin.exe ui --no-open --port 8787               # 只打印带会话令牌的地址
```

每次启动生成随机会话令牌，地址形如 `http://127.0.0.1:8787/?t=<token>`；**所有 `/api/*` 必须带 `X-UI-Token`，不带即 401**（防本机其它程序驱动插件改配置），令牌不打进日志、页面不落明文密钥、响应里的凭据一律掩码。**【实跑·本清理轮】**：`ximo-plugin.exe --home <tmp> ui --no-open --port 8799` 启动后，`GET /` → **200**（`<title>ximo-plugin · 羊皮卷控制台</title>`，CSS 含 `backdrop-filter` 与 `prefers-reduced-motion`），`GET /api/state` 无令牌 → **401**、带令牌 → **200** 且返回 `{"ok":true,"data":{"version":"0.1.0",…,"adapters":[…]}}`。命令全集以 `ximo-plugin --help` 为准（`ui` 已列在其中）。

**凭据与路径（与旧 CLI 的区别）**：插件自己的凭据文件默认是 `<用户主目录>/.ximo-plugin/cred.json`（目录 0700 / 文件 0600；Windows 上 `chmod` 只影响只读位，真实保护靠家目录 ACL，`doctor` 据此报 WARN），与 Agent 的 `config.json` **是两个文件**。`<BaseDir>` 解析顺序（ximo-agent 适配器）：`XIMO_HOME` 优先 → Windows `%APPDATA%\ximo-agent` / macOS `~/Library/Application Support/ximo-agent` / 其他 `~/.config/ximo-agent`（`plugin/internal/agents/ximoagent.go:366-379`）。**【静态】**

---

## 3. 与《技术设计方案 V1.0》的差异表

> 「文档要求」一列的来源见 §0：设计文档原文不在本工作区，条目取自任务书列出的清单 + 契约 §11.6 / §12.4。状态：**已实现 / 部分 / 未做**。

| # | 文档要求 | 本实现 | 状态 | 原因 / 备注 |
| --- | --- | --- | --- | --- |
| 1 | PostgreSQL 存账号/额度/订单 | SQLite（`modernc.org/sqlite` + 0003 迁移 12 张 `gw_*` 表） | 已实现（等价替代） | 契约 §0.3：仓库既有存储是「单进程单写者」SQLite（写队列 + `_txlock=immediate` + `foreign_keys=ON`）；契约 §0.1 禁止新增依赖 |
| 2 | Redis 做分布式限流/缓存 | 进程内令牌桶（`gateway.KeyLimiter` 复用 `provider.RateLimiter`，每凭据一个桶 + 空闲回收/LRU 淘汰） | 已实现（以进程内替代） | 契约 §11.6 明确不做 Redis；单副本前提下进程内即可。已实跑 429 |
| 3 | NATS/Kafka 事件总线 | 无 | 未做 | 契约 §11.6 |
| 4 | 事务型 outbox 表 + 异步投递 | **网关内没有 outbox 表**（`grep outbox migrations/0003_gateway.sql` = 0 命中），网关同步写 usage | 未做 | 契约 §11.6 把 outbox 保留为升级路径。注意：`outbox` 表存在于 `migrations/0001_init.sql:254`，属 **Agent 运行时**，网关零引用 |
| 5 | outbox 异步计费 | 结算在请求内同步完成（`quota.SettleWithUsage` → 账本 + usage） | 未做 | 契约 §11.6 |
| 6 | JWT 短期令牌 | 不透明随机令牌 + sha256 落库（access `gwa_` 15m / refresh `gwr_` 30d，refresh 轮换后旧令牌立即失效） | 已实现（等价且更强：可撤销、可轮换） | 契约 §0.1 明令不引入 JWT 库；`internal/account/token.go`、`service.go:11-16` |
| 7 | Argon2id 口令哈希 | PBKDF2-HMAC-SHA256，210000 次迭代，16B 盐，32B 派生键 | 已实现（等价级别） | 契约 §0.1 认可「≥200k 次迭代为等价级别」；`internal/account/password.go:24`。另支持可选 pepper（`XIMO_GATEWAY_PEPPER`） |
| 8 | `POST /v1/responses`（Responses 风格） | 未实现 | 未做 | 契约 §11.1.9：`/v1/chat/completions` + `/v1/messages` 两个入口足够 |
| 9 | Anthropic 上游直连 | 未做；只做 **入站** Anthropic Messages → OpenAI chat 上游的协议翻译 | 部分 | 契约 §11.1.4：V1 上游只支持 `openai-chat`；admin 写其它协议明确 400（已实跑） |
| 10 | 账号/额度管理员 RBAC | 未做：只有一个**共享** `X-Admin-Token`，`gw_audit.actor` 恒为 `"admin"`，无角色/权限模型、无管理员账号表 | 未做 | 契约 §12.4；`api/admin/deps.go:161` |
| 11 | 管理员 MFA | 未做 | 未做 | 契约 §12.4 |
| 12 | 真实价目表（按模型/按 token 定价） | 未做：全局占位单价 `--price-micro-per-ktok`（默认 1 微单位/1K token），`gw_models.capabilities_json` 里也没有价格字段；输入 token 预估用「字符数/4」粗估 | 未做 | 契约 §11.6；`internal/quota/estimate.go`。**金额不可当账单** |
| 13 | 多副本 / 分布式锁 | 未做：全链路假设单进程单写者（SQLite 单写者 + 进程内限流桶 + per-provider 熔断器/令牌桶 + 进程内设备码消费兜底） | 未做 | 契约 §11.6 |
| 14 | 管理后台 Web UI | 未做：只有 `/admin/*` JSON API | 未做 | 契约 §11.6 |
| 15 | 上下文窗口门控 | 未做 | 未做 | 契约 §11.1.8：`model.ModelSpec` 无 context window 字段，无法门控 |
| 16 | 设备授权登录（文档 §5.2） | 已实现：`/v1/auth/device` → `/admin/device/approve` → `/v1/auth/token`；设备码一次性消费由 `store.ConsumeDeviceCode` 落库保证（跨进程有效） | 已实现 | `internal/account/device.go`、`internal/gateway/store/account.go`。已实跑全链 |
| 17 | 模型发现 API（文档 §5.3） | 已实现：`GET /v1/models`，能力字段白名单投影为 `{stream,vision,tools,reasoning}` | 已实现 | `api/meta/models.go`。已实跑 |
| 18 | API Gateway + Provider Router（文档 §6） | 已实现：入站 `/v1/*` + SSE 转发 + 逐候选路由（priority 升序、过滤停用/熔断/协议） | 已实现 | `api/openai`、`api/anthropic`、`catalog`、`upstream`。已实跑非流式 + 流式 |
| 19 | 后台可观测（指标/链路） | 只有结构化日志 + `X-Request-Id` 贯穿 + 访问日志 + panic 恢复；**无 `/metrics` 端点、无 trace 上报** | 部分 | `internal/gateway/middleware.go`。设计文档未在本文核对范围，此处仅记录事实 |

---

## 4. 已知缺陷与风险清单（照实写）

严重度：**P1** = 影响正确性/钱；**P2** = 影响可用性或运维；**P3** = 设计取舍，已知并接受。

### D1【P1】流式 usage 缺失导致流式按 0 计费 —— **本轮已修，且已实跑验证**

- 原始缺陷：`internal/provider/stream.go` 的 `consume()` 在 `choice.FinishReason != ""` 时返回 `stop=true`，流读取循环立即 `break`；而 OpenAI 的 `stream_options.include_usage` 形态把 usage 放在 finish_reason **之后**的独立分片里 → usage 永远取不到 → 网关按 0 token 结算。而网关侧**始终请求** `include_usage`（`internal/gateway/upstream/spec.go` 的 `capabilitiesFor` 用 `provider.DefaultCapabilities()`，其 `SendStreamUsage=true`），所以这个缺陷在实际链路上必然触发。
- 修复【静态；行号为本轮复核值】：`stream.go` 仍是「命中 finish_reason 即停主循环」（`consume` 返回 `stop=true`，`internal/provider/stream.go:268-275`），但读取循环之后补了**收尾扫描** `drainTrailingUsage`（定义 `internal/provider/stream.go:303-344`，调用点 `:600-608`）：只在「还没拿到 usage 且还没看到 `[DONE]`」时触发，读到 usage 或 `[DONE]` 即停，最多 `tailMaxEvents = 4` 条，收尾阶段把空闲窗口收紧为 `trailingIdleTimeout = 2s`，且扫尾失败**永不**把成功的流转成错误。`provider.StreamChunk` 新增 `FinishReason` 字段（`internal/provider/provider.go:187-195`）。网关侧：`pumpStream` 一直读到 `chunk.Done` 才退出、遇 `chunk.Usage != nil` 即记录（`internal/gateway/api/openai/stream.go:223-232`）。
- **修复效果【实跑】**：
  - 我的手工链路（真二进制 + 假上游，usage 分片放在 finish_reason 之后）：流式请求落库 `status:"settled"`、`input_tokens:11`、`output_tokens:7`、`cost_micro:18000`，与同参非流式完全一致。
  - 仓库 E2E：`go test ./tests/gateway/ -count=1 -v` → `TestGatewayEndToEnd PASS`，并打印 `e2e_test.go:514: 流式 usage 行: input=11 output=5 cost_micro=16`（该行号为当时版本，会随文件版本浮动）。另有专门钉这条的回归用例 `TestGatewayTrailingUsageIsSettled`（假上游把 usage 放在 finish_reason **之后**），本轮复跑结果见 §5.1。
- **残留 1【静态】**：上游**不报 usage** 时（例如非 OpenAI 兼容自建网关，或 `ConfigJSON` 里关掉了 `capabilities.stream`），网关如实记 0 token / `cost_micro=0`，即该次请求不计费。代码注释明确说明「不伪造 token 数」（`api/openai/deps.go` 的终态词表注释段）。这是取舍，不是 bug，但要知道它存在。
- **残留 2 —— 已修（本轮复核）**：`provider.StreamChunk.FinishReason` **已被两个入口消费**。OpenAI 入口：`internal/gateway/api/openai/stream.go:230-232` 收下上游显式值，收尾分片由 `finishReason()`（`internal/gateway/api/openai/stream.go:180-188`）优先采用它（映射见 `wireFinishReason`，`internal/gateway/api/openai/response.go:125`），只有上游整条流都没报时才回退到「有 tool_calls → `tool_calls`，否则 `stop`」的推断 —— 因此上游报 `length` 现在会如实写成 `length`。Anthropic 入口同理：`internal/gateway/api/anthropic/stream.go:357-359` 收下，由 `streamStopReason()`（`:415-430`）映射为 `max_tokens` / `tool_use` / `end_turn`（命中客户端 stop 序列时优先生效 `stop_sequence`），只有 `up == ""` 时才走 `inferredStopReason()`（`:440`）。契约 §12.4.3 说的「流式 stop_reason 只能推断」在网关层**已不再成立**；仅对「整条流不报 finish_reason」的上游保留推断回退。

### D2【P2 · 本轮已修】`ximo-plugin apply` 从文件回退写出的 `<BaseDir>/config.json` 此前**不会被读回**

- 缺陷原状（对应已删除的 `ximo-cli apply`）：`cmd/ximo-agent/main.go` 的 `--config` 默认为空串，`config.LoadConfig("")` 直接返回默认值 —— 只有以 `--config <BaseDir>/config.json` 显式启动的 Agent 才会读回该文件；旧 CLI 因此只能在 `apply` / `doctor` 里打印告警，没有从根上修。
- **现状【静态，本轮复核代码后改写】**：`cmd/ximo-agent/main.go:59-68` 已在 `--config` 为空时自动取 `config.DefaultConfigPath()`（`internal/config/config.go:834-841`，即 `<BaseDir>/config.json`），文件不存在才退回默认配置；加载成功会打印 `[CONFIG] Loaded configuration from <path>`（`main.go:74-76`）。也就是说**这条陷阱在当前 `v2` 树上已不复存在**（前提是配置确实落在 `<BaseDir>/config.json`）。
- 插件侧（`plugin/`）仍会在文件回退路径上照实告警：`plugin/internal/agents/ximoagent.go:297-301` 说明「该文件只在 ximo-agent 重启后才被读回」「明文密钥没有进平台安全存储」，由 `plugin/internal/agents/ximoagent_test.go:233` 断言文案。
- 影响：走 IPC 路径时配置在运行期立即生效（`apply` 默认优先 IPC）；只有引擎不可达时才落到文件回退，此时**需要重启 Agent** 才生效（不再是静默忽略）。
- **诚实标注**：本轮清理只改文档与注释，**没有实跑 Agent 做读回验证**，所以上面「已修」是读码结论（【静态】），不是【实跑】。

### D3【P2 · 在 `v2` 分支不适用】升级脚本的健康探测指向不存在的 HTTP 端点（**只报告不修**）

- **先说目录事实**：`v2` 分支**没有 `release/` 目录**（`ls release` → No such file or directory），也就没有 `upgrade.ps1` / `upgrade.sh` / `updater.go`。`v2` 用的发布路径是 [`.github/workflows/release.yml`](../.github/workflows/release.yml)（`workflow_dispatch` 填版本号或推 `v*` 标签 → 构建 Windows 安装包 + 引擎 zip + 前端更新包），其中**没有**任何迁移/健康探测步骤。
- 本条缺陷的事实仍然成立，但只对**旧分支**成立：`repo/v2-go/release/upgrade.ps1:14` 是 `$HealthProbeURL = "http://127.0.0.1:28888/readiness"`（`:98` 轮询），`upgrade.sh:29` 同一个 URL（第 5 步 `curl` 后 grep `"status":"UP"`）。【静态：在 `v2-go` 工作树上 `grep -n` 实测】
- 会 `net.Listen("tcp", …)` 的业务代码只有 `internal/gateway/server.go`（中转站，默认回环 8600）与 `internal/ipc/transport.go`（IPC，命名管道/unix socket）；`internal/observability/health.go` 有 readiness 注册表但**没有被任何 HTTP handler 暴露**。因此那份旧脚本第 5 步**必然失败并回滚**（脚本本身安全，但升级永远走不完）。契约 §12.3 明确：**只报告不修**（属发布流程设计问题，超出中转站范围）。
- **结论**：这条不再是 `v2` 的待办；保留作旧分支记录。若将来把旧分支的升级脚本搬进 `v2`，本条与下条都要重新评估。

### D3′【已修 · 实跑验证，但 `v2` 分支当前无调用方】`ximo-agent --migrate-only` 缺口

- 原缺陷（旧分支）：`release/upgrade.ps1:84` / `upgrade.sh:79` 调用 `ximo-agent --role=engine --migrate-only`，但 `cmd/ximo-agent/main.go` 从未定义该 flag → `flag provided but not defined: -migrate-only`，退出码 2，升级脚本第 4 步必然失败。
- 现状：`cmd/ximo-agent/main.go:41,50` 定义 `migrateOnly` flag，`:78-86` 在**角色分发之前**处理（`--role=engine --migrate-only` 这种形式下角色不应影响迁移），复用 `storage.Open` + `migrations.ApplyFromDir`；退出码语义见 `runMigrateOnly`（`:134`）。
- **【实跑】**：`XIMO_HOME=<tmp>/home ./ximo-agent.exe --role=engine --migrate-only` → 退出码 0，输出
  `[MIGRATE] Database <tmp>/home/data/ximo-agent.db ready: schema version 3 (applied [1 2 3] this run, dir=<repo>/migrations)`。
- 另：`ximo-gateway --migrate-only` 同样存在且实测退出码 0（`versions":[1,2,3]`）。
- **诚实补充**：`v2` 分支**没有任何调用方** —— 全仓 `grep -- "--migrate-only"` 只命中 `cmd/ximo-agent/main.go`（定义与注释）、`cmd/ximo-agent/migrate_test.go`（测试）、`cmd/ximo-gateway/*`（网关自己的同名 flag）；`.github/workflows/release.yml` 里 `grep migrate` 0 命中，仓库内也没有任何 `.cmd/.ps1/.sh` 调用它。也就是说该 flag 目前只有单元测试在保护（`cmd/ximo-agent/migrate_test.go` 的 `TestMigrateOnlyFlagIsAcceptedByTheBinary` 用真二进制跑），**升级流程本身在 `v2` 上并未接线**。

### D4【P2 · 已作废的判断 + 正确的读法】主程序落地情况

- **本文初稿（11:50 前）的判断已作废**：那时 `cmd/ximo-gateway` 与 `tests/gateway/e2e_test.go` 确实不存在；它们在 11:51–11:53 落地，现在都在，且 `--help` 的 flag 与默认值与契约 §12.1 **逐字一致**（见 §1.3 实测表）。
- 仍要提醒的读法陷阱：`internal/gateway/config.go` 的 `DefaultAddr`（`127.0.0.1:8080`）与 `DefaultRateLimitPerMin`（`60`）**不是**对外默认值 —— 二进制在 `cmd/ximo-gateway/flags.go:33-39` 自己定义了 `8600` / `0`，并**刻意不复用**库默认值（注释在 `flags.go:30-32`）。引用默认值请以 `--help` 为准。

### D5【P1 · **已修**】没有额度账户行的用户请求被拒时曾返回 **404**，契约要求 **402**

- 原缺陷：`store.CreateUser` 建户时**不创建 `gw_quota_accounts` 行**，`ReserveTx` 读账户拿到 `sql.ErrNoRows` → 经 `wrapNotFound` 变成 `model.ErrNotFound` → `httpx.StatusFor` 映射 404。而契约 §11.4 只定义了「模型不存在/未启用 → 404 `model_not_found`」与「额度不足 → 402 `insufficient_quota`」，客户端会把「没充值」误判成「资源不存在」。上轮手工链路的实测原文：新建用户后直接对话 → `HTTP 404` + `{"error":{"message":"额度预占失败","type":"not_found_error","code":"not_found"}}`，上游未被调用。
- **修复（上一版给出的两条建议现在都落地了，互为纵深防御）【静态，行号为本轮复核值】**：
  1. `store.CreateUser`（`internal/gateway/store/account.go:40`）在**同一个事务**里插入 0 额度账户行（`internal/gateway/store/account.go:56`）；username 冲突时整事务回滚，不留无主账户行。
  2. `ReserveTx`（`internal/gateway/store/quota.go:102`）读不到账户行时不再当成「资源不存在」，而归一为 `model.ErrInsufficientQuota`（`internal/gateway/store/quota.go:141-146`），HTTP 层因此是 **402**；`model.ErrNotFound` 留给「用户 / 模型不存在」。这条是纵深防御，兜住历史数据与直接写库的场景。
- **回归用例**：`tests/gateway/e2e_test.go` 的 `TestGatewayUnfundedUserGets402`（`:844`）用**全新建户、不做任何额度操作**的用户直测这条路径，断言 402。就旧代码（`wrapNotFound` → `StatusFor` → 404）而言该断言必为红；**本轮未回退基线复跑那份红**，红侧依据是上轮实跑记录 + 现存的错误映射链（静态）。绿侧本轮实跑见 §5.1。
- 历史说明：`TestGatewayEndToEnd` 里的「贫穷用户」仍先 `topup 0`，但语义已从「绕过缺陷」变成「额外覆盖『有账户、可用额为 0』这条分支」（注释已按修复后的语义改写）。

### D6【P1 · **已修**】`quota.ReapExpired`（预占回收器）曾**没有任何调用方**

- 原缺陷：`grep -rn "ReapExpired" cmd/` = 0 命中，主程序既不起 goroutine 也无 ticker。影响：上游挂住 / 进程崩溃后遗留的 `held` 预占不会被回收，用户额度被永久占住；而 `api/openai/charge.go` 的 `settle` 失败路径**刻意不回退为 Release**（避免双重退款），其正确性前提正是「预占最终会被回收」—— 那个前提当时不成立。
- **修复【静态，行号为本轮复核值】**：新增 `cmd/ximo-gateway/reaper.go`（130 行）：`startReaper(ctx, svc, interval, limit, logger) func()` 起 ticker 协程，每周期调 `ReapExpired(ctx, 0, limit)`（定义 `internal/quota/service.go:315`）。接线点 `cmd/ximo-gateway/main.go:122`（`stopReaper := startReaper(...)` + `defer stopReaper()`）；停等函数取消派生 context 并**等协程真正退出**，defer 顺序保证「协程停稳 → 才关库」。flag 见 `cmd/ximo-gateway/flags.go:42-47,83-84`：`--reap-interval` 默认 `1m`（`0` = 显式关闭回收，不启协程）、`--reap-limit` 默认取 `quota.DefaultReapLimit = 500`（`internal/quota/service.go:27`）。回收失败只记 Warn，下周期自然重试（`ReapExpired` 幂等）。
- **测试**：`cmd/ximo-gateway/reaper_test.go`（459 行）覆盖回收超时 `held`、`--reap-interval 0` 不启动、协程随主 ctx 退出等场景；本轮复跑见 §5.1。
- **仍未对接的部分**：`quota.Service.SetReservationTTL`（`internal/quota/service.go:70`，「预占存活多久后能被回收」）**没有**接到任何 flag/env —— 全仓非测试代码零调用方，回收协程传 `now=0`（取当前时钟），TTL 用包默认 `DefaultReservationTTL`。要调 TTL 目前只能改代码。

### D7【P2】既有代码「有实现无接线」清单

| 对象 | 实现位置 | 接线状态（证据） |
| --- | --- | --- |
| `internal/storage/repository.ProviderRepo`（`providers` 表） | `internal/storage/repository/catalog.go:30` | **无任何非测试调用方**（grep 计数 0）。网关用的是自己的 `gw_providers`，Agent 运行时的 `providers` 表两端都没人读 |
| `storage.Outbox`（事务型 outbox） | `internal/storage/outbox.go` | 只被 `bootstrap` 适配成 Engine 的 `ports.OutboxStore`（`internal/bootstrap/adapters.go:266`，装配点 `bootstrap.go:173`）；Engine 侧**只 Enqueue，不投递** —— `grep "\.Pending(\|\.Ack("` 在 `internal/engine`、`internal/agent` 零命中，唯一命中的是 `internal/engine/engine.go:1460` 的 `Outbox.Enqueue`。即 outbox 只写不投递。**网关侧完全不使用 outbox** |
| 其它既有 Repo：`PermissionRepo` / `McpServerRepo` / `ExpertRepo` / `SkillRepo` / `SessionRunRepo` | `internal/storage/repository/*.go` | 同样**零非测试调用方**（逐个 grep 计数 0） |

### D8【P2】结算与写 usage 不是同一事务；审计与业务写也不是

- `quota.SettleWithUsage` 内部是两段写（先账本、后 `InsertUsage`），存储接口没有合并方法（`internal/quota/service.go:156` 注释如实写明）。失败时账本可能已生效，调用方**不回退**（否则双重退款），预占留给回收器 —— 又回到 D6。
- admin 侧：业务写与 `gw_audit` 是同一请求内的两次独立写，审计失败时返回 500 且文案写明「操作已生效但未留痕」（`api/admin/routes.go` 的 `audit`/`auditOK` 注释），绝不假装成功。
- 影响：极端情况下可能出现「有账本无 usage 行」或「有业务写无审计行」。三处都有明确处理与日志，不会静默。

### D9【P2】限流桶被回收 = 该凭据重新获得整桶额度

- `internal/gateway/ratelimit.go:132` 的 sweep 是**内存保护而非硬配额**：桶超上限时按空闲 TTL 回收、仍超限则 LRU 淘汰到 7/8。代码注释已写明「不能用作防绕过的唯一手段，真正的防线是账号体系」。
- 另：默认 `burst = perMin`（分钟额度可一次性用完），`Retry-After` 回的是补满一个令牌的秒数。实测 `--rate-limit-per-min 2` 时第 3 次请求 429。

### D10【P3】Anthropic 入站翻译的已知缺口（都在代码里逐条标注 + 有明确行为，不假装支持）

来源：`internal/gateway/api/anthropic/doc.go`（逐条编号）。

1. **图片**：冻结的 `provider.Message.Content` 是 string，无 content-parts 入口 → image block **显式 400 拒绝**（`unsupported_content_block`），而不是静默丢图。
2. **stop_sequences**：上游无 Stop 字段 → 在**输出侧按 stop 序列截断**并回报 `stop_reason=stop_sequence`；流式截断时上游 usage 可能尚未上报，此时按已知 usage 结算。
3. **tool_choice**：`provider.BuildRequestBody` 在有 tools 时硬编码 `"auto"` → `{"type":"any"}` / `{"type":"tool"}` 降级为 auto 并记 Warn。
4. **top_p / metadata / thinking**：冻结的请求类型无对应字段，忽略并记 Warn（`thinking` 直接忽略、无告警）。
5. **流式 stop_reason**：优先用上游显式报告的 `finish_reason`（`streamStopReason`，`internal/gateway/api/anthropic/stream.go:415-430`）；**只有**上游整条流都不报时才回退到推断（`inferredStopReason`，`:440`）。推断路径的已知误差：模型恰好自然结束在 `max_tokens` 上会被报成 `max_tokens`；上游按 `length` 截断但 usage 未上报（`outTok=0`）时会被报成 `end_turn`。见 D1 残留 2（已修）。

### D11【P2】空值/边界行为（非缺陷，但调用方容易踩）

- `POST /admin/providers` 的 `protocol` 只接受 `openai-chat`，其它值 400；`endpoint` 必须能解析出 scheme+host 且为 http/https；`api_key_ref` 必须是 `secretref:v1:...` 形式。
- `GET /admin/usage`、`GET /admin/keys`、`GET /admin/quota/ledger` 的 `user_id` **必填**（缺失 400）；`GET /v1/usage` 的 `limit` 上限 200（超出静默收紧到上限）、非法值 400；`user_id` 只来自鉴权结果，绝不接受 query 指定。
- 用户态两条路由（`/v1/models`、`/v1/usage`）必须由装配方单独套 `RequireUserRoutes` —— `meta.Routes(d)` 会返回全部 8 条，把登录链四条也套上 `RequireUser` 会导致插件**永远无法登录**（`api/meta/meta.go:106-115` 明确警告）。落地实现在 `cmd/ximo-gateway/wire.go:151-158`：`meta.PublicRoutes` 只套限流，`meta.UserRoutes` 才套鉴权。
- 位置参数一律拒绝（`flags.go:82-86`）：静默忽略会把 `--admin-token=<token>` 这类手滑写成位置参数的情形变成「用默认配置启动了」。

### D12【P3】无真实价目表 → 计费金额不可作为账单

占位单价默认 1 微单位/1K token；输入 token 预估是「字符数/4」的粗估（中英混排/代码/JSON 会明显偏离），**只用于预占上界，绝不用于计费**（`internal/quota/estimate.go` 注释）。结算一律用上游真实 usage。实测里为了看清金额才把单价调成 `1000000` 微单位/1K token。

### D13【P2】健康检查无依赖体检、无 `/metrics`

`GET /v1/health` 只回进程存活与服务器时间，**刻意不体检 store/account**（`api/meta/health.go` 注释说明了理由：依赖瞬时故障会让健康检查抖动并被编排系统放大成重启）。代价是「进程活着但数据库不可用」时健康检查仍报 ok；依赖可用性只能在真实请求上观察（未装配 → 503，查询失败 → 5xx）。另：仓库无 Prometheus 指标端点。

---

## 5. 验证命令与本次实跑结果

### 5.1 本次实跑（全部通过）

```bash
export GOROOT=/c/ximo-tools/go && export PATH=$GOROOT/bin:$PATH && export GOPROXY=https://goproxy.cn,direct
cd "C:/Users/新建文件夹/repo/v2"
```

| 命令 | 结果【实跑】 |
| --- | --- |
| `go build ./...` | **通过**（退出码 0） |
| `go vet ./cmd/ximo-gateway/... ./internal/gateway/... ./internal/quota/... ./internal/account/...` | **通过**（无输出，退出码 0） |
| `go test ./internal/gateway/... ./internal/quota/... ./internal/account/... -count=1` | **10 个含测试的包全绿**：`internal/gateway` 0.220s、`api/admin` 0.648s、`api/anthropic` 0.461s、`api/meta` 0.618s、`api/openai` 0.222s、`catalog` 0.123s、`store` 0.536s、`upstream` 0.315s、`internal/quota` 0.140s、`internal/account` 1.386s；`httpx` 与 `model` 无测试文件（纯原语/纯类型） |
| ~~`go test ./cmd/ximo-cli/... -count=1`~~ | **已不适用**：该包已随 `cmd/ximo-cli` 一并删除（本仓已无 `cmd/ximo-cli/` 目录）。2026-09-26 清理时实跑该命令，输出为 `pattern ./cmd/ximo-cli/...: GetFileAttributesEx .\cmd\ximo-cli\: The system cannot find the file specified.` / `FAIL ... [setup failed]`，退出码 1。插件侧的测试改跑 `cd plugin && go test ./... -count=1`（见 §5.3 末行） |
| `go test ./cmd/ximo-gateway/... -count=1` | **通过**（0.320s） |
| `go test ./tests/gateway/ -count=1 -v` | **`TestGatewayEndToEnd PASS`**（1.42s），日志打印 `流式 usage 行: input=11 output=5 cost_micro=16`（原输出含 `e2e_test.go:<N>:` 行号，随文件版本浮动） |
| `XX=1 ./ximo-gateway.exe --role=…`（手工 E2E，见下） | 见 §5.2 |
| `XIMO_HOME=<tmp> ./ximo-agent.exe --role=engine --migrate-only` | **退出码 0**，`schema version 3 (applied [1 2 3] this run, dir=<repo>/migrations)` |
| `./ximo-gateway.exe --db <tmp>/gw.db --migrate-only` | **退出码 0**，`versions":[1,2,3]` |
| `./ximo-gateway.exe --version` / `--help` | 退出码 0；`--help` 默认值与 §12.1 逐字一致 |
| 不给 `--admin-token` 启动 | **退出码 2**，报「缺少管理令牌」，且**未创建库文件**（fail closed 实测） |

**本轮复核实跑（2026-09-26 修订时重跑，命令与输出原文摘录）**

| 命令 | 结果【实跑】 |
| --- | --- |
| `go build ./...` | **通过**（退出码 0） |
| `go vet ./tests/gateway/... ./cmd/ximo-gateway/... ./cmd/ximo-agent/...` | **通过**（无输出） |
| `go test ./tests/gateway/... -count=1 -v` | **4 条用例全绿，`ok ... tests/gateway 8.402s`**：`TestGatewayEndToEnd PASS`（1.45s，打印 `流式 usage 行: input=11 output=5 cost_micro=16`）、`TestGatewayTrailingUsageIsSettled PASS`（1.38s，打印 `尾随 usage 用例实跑值：非流式 input=11 output=5 cost=16；流式 input=11 output=5 cost=16（want cost=16）`）、`TestGatewayUnfundedUserGets402 PASS`（1.43s）、`TestGatewayFailedUpstreamLeavesNoHeldReservation PASS`（3.98s） |
| `go test ./cmd/ximo-gateway/... ./cmd/ximo-agent/... -count=1` | **通过**：`ok cmd/ximo-gateway 0.868s`（含 `reaper_test.go`）、`ok cmd/ximo-agent 4.381s`（含 `--migrate-only` 回归） |
| `go test ./internal/provider/... -count=1` | **通过**：`ok internal/provider 0.271s`（本轮补跑，见 §6 第 8 条） |

### 5.2 手工端到端（真二进制 + 真 HTTP + 本地假上游）【实跑】

做法：`go build -o <tmp>/ximo-gateway.exe ./cmd/ximo-gateway`；另用一段仅依赖标准库的 Go 程序做假上游（返回 OpenAI 兼容非流式响应，以及「内容分片 → finish_reason → **usage 分片** → `[DONE]`」的 SSE，并记录每次命中）；再以 `--db <tmp>/gw.db --admin-token <自定> --price-micro-per-ktok 1000000 --max-output-tokens 256 --rate-limit-per-min 0` 启动网关，用 curl 跑 §2.3 的全流程。要点结果：

1. `/v1/health`、`/v1/capabilities` 正常；缺 admin token → 401；
2. 建用户 → 充值（重放同 `idempotency_key` 返回同一账本行，`available` 不变）→ 配 provider（明文密钥只回 `secretref:v1:<hex>`）→ 建模型 → 建映射 → 发 API Key；
3. `GET /v1/models` 返回带 `provider`/`protocols`/`capabilities` 的目录；
4. 非流式对话：`content:"pong"`、`finish_reason:"stop"`、`usage 11/7`；
5. 流式对话：SSE 序列与 §11.5 一致（含 `choices:[]` 的 usage 分片与 `[DONE]`）；
6. `/v1/usage` 两条记录均 `status:"settled"`、`cost_micro:18000`（**流式那条有真实 token，说明 D1 的修复贯穿到结算**）；
7. `available=49964000`，账本 5 行合计 `= 49964000`（**对账不变量成立**）；
8. 未知模型 → 404 `model_not_found`，假上游命中计数不变；
9. 未充值用户 → 当时是 **404 `not_found`**（这就是 D5，**本轮已修**：现在建户即建 0 额度账户行，且账户缺失归一为额度不足 → **402 `insufficient_quota`**）；充值 1 微单位后 → 402；冻结 → 403 `account_disabled`；（修复后的 402 由 §5.1 的 `TestGatewayUnfundedUserGets402` 实跑钉住）
10. 设备码链 + 口令登录都拿到令牌对；`--rate-limit-per-min 2` 时第 3 次 429；同 `--db` 重启数据保留；
11. 网关日志中 grep 管理令牌明文 **0 命中**（只有 `admin_fingerprint`）。

### 5.3 按功能点的验证命令（供复跑）

| 功能点 | 验证命令 | 覆盖内容 |
| --- | --- | --- |
| 网关配置/路由装配/中间件/鉴权 | `go test ./internal/gateway/ -count=1` | 路由重复/空 pattern 装配报错、中间件顺序、`X-Request-Id`、恒定时间 admin 比较 |
| 主程序装配与 flag | `go test ./cmd/ximo-gateway/ -count=1` | flag 解析、默认值、fail-closed、`--migrate-only` / `--version` 分流 |
| 预占回收（D6） | `go test ./cmd/ximo-gateway/ -count=1` | 整包运行（本轮 `ok 0.868s`）；覆盖回收的用例：`TestReaperReclaimsExpiredHold`、`TestReaperDisabledWhenIntervalZero`、`TestReaperSurvivesFailuresAndPanics`、`TestReapFlagsDefaultsAndDisable`、`TestGatewayReapsExpiredHoldOverHTTP`（经真 HTTP 触发回收） |
| 真二进制 E2E | `go test ./tests/gateway/ -count=1` | 全链路冒烟，外加三条「曾经真的坏过」的回归：`TestGatewayTrailingUsageIsSettled`（D1：尾随 usage 计费）、`TestGatewayUnfundedUserGets402`（D5：全新用户 → 402）、`TestGatewayFailedUpstreamLeavesNoHeldReservation`（上游失败后预占不泄漏）；以及 402/404 且上游未被打、usage 行存在、账本对账 |
| HTTP 表与登录链 | `go test ./internal/gateway/api/meta/ -count=1` | `routes_test.go` 断言 §11.3 全部 8 条路由与响应形状；`integration_test.go` 用真实 `Deps` 跑登录链 |
| OpenAI 入口（非流式 + 流式 + 计费） | `go test ./internal/gateway/api/openai/ -count=1` | 请求归一化、SSE 收尾、402/404/502 路径、`charge` 四条出口与 usage 终态 |
| Anthropic 翻译 | `go test ./internal/gateway/api/anthropic/ -count=1` | 双向翻译、stop 序列截断、流式事件序列、`integration_test.go` |
| 管理 API + 审计 | `go test ./internal/gateway/api/admin/ -count=1` | 全部 `/admin/*`、写操作审计留痕、协议校验、密钥只回 ref |
| 额度账本原子性与对账 | `go test ./internal/quota/ -count=1` | 并发预占、幂等、欠账结算、`store_contract_test.go` 编译期断言 `*gwstore.Store` 可直接注入 |
| 账号/令牌/设备码 | `go test ./internal/account/ -count=1` | 口令哈希、API Key、refresh 轮换、设备码一次性消费、`integration_test.go` 走真实 store |
| 数据访问（含迁移） | `go test ./internal/gateway/store/ -count=1` | `ReserveTx`/`SettleTx`/`AdjustTx` 幂等与原子性、`SumLedgerAmount` 对账、`ConsumeDeviceCode` 0 行 → `ErrConflict` |
| 目录与候选路由 | `go test ./internal/gateway/catalog/ -count=1` | 过滤停用/熔断/映射关闭、priority 升序稳定排序 |
| 上游池 | `go test ./internal/gateway/upstream/ -count=1` | per-provider client/breaker/limiter、密钥解析与脱敏、`include_usage` 确实发出（`pool_test.go:235`） |
| 插件（独立 module `plugin/`） | `cd plugin && go build ./... && go vet ./... && go test ./... -count=1` | 设备码登录与轮询节奏（`internal/gateway/auth_test.go`）、`apply` 的 IPC/文件双路径与备份、回退告警文案（`internal/agents/ximoagent_test.go`、`cmd/ximo-plugin/ipc_test.go`）、`doctor` 自检、凭据文件 0600 与密钥明文不进配置文件（`internal/gateway/cred_test.go`、`internal/agents/ximoagent_test.go:83-96,170`）、**本地 UI**（`plugin/internal/ui/ui_test.go`、`plugin/cmd/ximo-plugin/ui_test.go`：令牌校验、参数越界、静态资源）。**主仓内原 `cmd/ximo-cli` 已删除，不在本表** |

**未跑的验证**（如实标注）：

- `go test ./...`（主仓全量）—— 契约 §0.6 明令不跑（含 10000-run 压测会超时）。本轮只跑与中转站/本次修订相关的包（见 §5.1）。
- 独立 module `plugin/` 与本轮无关的其它包（`internal/engine`、`internal/tool`、`frontend/`）**未跑**：本轮只改文档与 `tests/gateway/e2e_test.go` 的注释，不碰这些包。
- **【2026-09-26 清理轮】**：`cmd/ximo-cli` 已删除（`go build ./...` 通过）；本文档 §1.1 / §1.6 / §2.4 / §4.D2 / §5.1 / §5.3 / §6 与 `README.md` 末尾一节按现状改写，`plugin/internal/gateway` 里 4 处指向已删除二进制的注释改为指向规则本身（只改注释，未动逻辑）。本轮实跑：主仓 `go build ./...`、`go test ./internal/gateway/... -count=1`，以及 `cd plugin && go build ./... && go vet ./... && go test ./... -count=1`。
- **本地 UI 的验证范围（本轮实跑）**：真二进制 `ximo-plugin.exe --home <tmp> ui --no-open --port 8799`（`cmd/ximo-plugin/ui.go` + `plugin/internal/ui/**`，由并行任务落地）——`GET /` 200、无令牌 `GET /api/state` 401、带令牌 200。**未覆盖**：`/api/login/*`、`/api/plan`、`/api/apply` 这些需要真网关的路径（本轮没有起网关），以及浏览器里的视觉效果（只验证了返回的 HTML/CSS 标记）。`--help` 输出已把 `ui` 列进命令表。
- 【上一轮记录，本轮已补跑】`go test ./internal/provider/...`（D1 的修复代码在 provider 包）当时未跑；本轮跑到 `ok internal/provider 0.271s`。即便如此，该包的实现结论仍以包所有者（W-Provider）的验证为准。

---

## 6. 不确定项 / 未做部分（汇总）

1. **设计文档原文不在工作区**，§3 差异表的「文档要求」列取自任务书与契约，未逐字核对原文（见 §0）。
2. **D5、D6 本轮已修**（见 §4.D5 / §4.D6）；**D7 仍是未接线状态**（outbox 只写不投递、`internal/storage/repository` 全部 Repo 无调用方）。另有一处仍未对接：`quota.Service.SetReservationTTL`（回收 TTL）没有对外 flag/env。
3. **D1 的两点残留**：① 上游不报 usage 时仍按 0 计费（如实记 0，不伪造 token）；② 网关流式 `stop_reason` 的**推断回退仍保留**，但只在「上游整条流都不报 finish_reason」时才会用到 —— 残留 2（上游报 `length` 被写成 `stop`）本轮已修，见 §4.D1。
4. **升级脚本相关**：`v2` 分支**没有 `release/` 目录**（发布走 `.github/workflows/release.yml`），所以旧分支那两个升级脚本的健康探测缺陷（`http://127.0.0.1:28888/readiness`）在本分支**不适用**；`ximo-agent --migrate-only` 已补，但**本分支当前没有任何调用方**，只有单元测试在保护（见 §4.D3 / §4.D3′）。
5. `ximo-plugin apply` 的文件回退（引擎不可达时）写出的 `<BaseDir>/config.json` 需要重启 Agent 才生效，插件会照实告警；原先「Agent 根本不读回该文件」的陷阱**已在 `cmd/ximo-agent` 侧修掉**（静态结论，见 D2）。
6. 计费金额全为占位价，**不可作为账单依据**（见 D12）。
7. **本轮修订的范围**：`README.md` 末尾追加一节、本文档的状态更新、`tests/gateway/e2e_test.go` 的注释修正 —— **没有改动任何 Go 生产代码**，所以凡涉及实现正确性的结论仍以对应包的所有者为权威。
8. `internal/provider` 本轮**已补跑**：`go test ./internal/provider/... -count=1` → `ok ... 0.271s`，D1 的 provider 侧不再属于「未跑的验证」。
9. **【2026-09-26 清理轮】** 主仓内的 `cmd/ximo-cli` 已整体删除，插件侧只保留独立 module 的 `plugin/`（CLI + 本地 UI）。本文档所有原指该 CLI 的段落已按现状改写（§1.1 / §1.6 / §2.4 / §4.D2 / §5.1 / §5.3 / 本条），不再存在「已删除却仍当存在」的指引。清理轮**只改文档与注释，未改任何 Go 逻辑**；插件侧本地 UI 已落地并做了真二进制冒烟（`GET /` 200、无令牌 `/api/state` 401、带令牌 200，见 §5「未跑的验证」最后一条）。
