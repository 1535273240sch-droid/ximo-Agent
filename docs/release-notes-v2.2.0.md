# XimoAgent v2.2.0 更新说明

发布日期：2026-09-26 ｜ 版本跨度：v2.1.0 → v2.2.0

## 一、本次更新概述

v2.2.0 在桌面端之外补齐了「服务端中转站 + 通用接入插件」两块能力，并修掉三个真实缺陷（其中一个是流式请求按 0 token 计费）。桌面端（Electron 渲染层）本身没有改动。

## 二、新增能力

### 1. 中转站网关（`cmd/ximo-gateway`）

服务端中转站，把上游模型服务统一收口，提供账号、额度、模型目录与路由：

- **账号与凭据**：用户/密码（PBKDF2-HMAC-SHA256，21 万次迭代）、API Key（只存 sha256）、不透明令牌会话（短期 access + 可轮换 refresh）、设备码登录流程。
- **额度账本**：`topup / reserve / settle / release / freeze` 全流水，预占 + 幂等键 + 单写事务；`available = total − used − reserved` 恒不为负，账本 Σ 与账户可对账。
- **模型目录与路由**：模型 ↔ Provider 映射、优先级、健康探测与熔断隔离，复用引擎既有 `internal/provider` 的熔断/重试/限流/SSE。
- **协议入口**：`POST /v1/chat/completions`（含 SSE 流式）与 `POST /v1/messages`（Anthropic 兼容入站，内部翻译为 OpenAI 上游）。
- **管理 API**：用户、Key、额度（幂等 + 审计）、模型、Provider、用量、审计、设备码批准。
- 存储：`migrations/0003_gateway.sql`（12 张 `gw_*` 表）；默认只绑回环。

### 2. 独立通用接入插件（`plugin/`，独立 module）

一个可单独分发、与仓库主干解耦的单二进制插件，把任意 OpenAI/Anthropic 兼容中转站接进本机各种 Agent：

- **数据驱动适配器**：内置 `claude-code` / `codex-cli` / `continue` / `cursor` / `aider` / `openai-compatible-generic` / `env-openai` / `env-anthropic` / `ximo-agent`；新增一个 Agent 只需往 `~/.ximo-plugin/specs/` 放一份 JSON。
- **本地界面**（`ximo-plugin ui`，玻璃质感 + 羊皮卷风格）：检测 / 连接网关 / 模型 / 应用 / 体检 / 适配器；仅绑回环，每次启动生成会话令牌，写操作需二次确认。
- **安全默认**：凭据 0600、密钥只存引用、改配置前备份 + `--dry-run`、默认拒绝把短寿命令牌写进 Agent 配置。
- 支持 json / toml / yaml / dotenv 四种配置格式，保留未知字段。

### 3. mem0 长期记忆（默认关闭）

并入 `internal/memory` 等模块，记忆消息插在系统提示词之后、用户消息之前，未命中时不插入；`feature_flags["memory.mem0"]` 与 `memory.enabled` 默认均为 false。修正了默认端口（3000 是 dashboard，API 在 8888）与 embedder 的 provider 白名单说明。

### 4. 部署材料（`deploy/`）

systemd 单元、幂等安装脚本、端到端验证脚本与假上游、设备码批准脚本，以及在 Linux 服务器上的部署说明。当前已在 Ubuntu 26.04 上以 systemd 托管运行并通过端到端验证（18 项断言全通过）。

## 三、缺陷修复

1. **流式/聚合请求丢弃尾随 usage，导致按 0 token 计费（等于免计费）**
   `internal/provider` 在收到 `finish_reason` 时即结束读取，而多数上游把 `usage` 放在其后单独一条分片。现改为结束分片后继续扫尾（只认 usage 与 `[DONE]`，命中即停），并让 `StreamChunk` 携带上游 `finish_reason`，网关侧据此给出准确的 `finish_reason` / `stop_reason`。
2. **`--migrate-only` 从未定义**：`release/upgrade.ps1|.sh` 一直在调用它，而 `cmd/ximo-agent` 没有该 flag（实测退出码 2，升级脚本必然失败）。已补上。
3. **`migrations_test.go` 把「迁移恰好两版」写死**：新增 `0003` 会让仓库自带验收门变红。改为断言「版本从 1 连续到 N、当前版本等于最后一版」这一不变量。

## 四、Linux 可构建性（部分修复）

网关侧已可完整交叉编译并在 Linux 运行：新增可移植的 `internal/gateway/secretref`，并把网关的密钥后端按平台拆分（Windows 用凭据管理器/DPAPI，其他平台用 0600 文件后端并显式告警）。

**仍待修**：`internal/secrets`、`internal/ipc`、`internal/worker/procguard` 三个包在非 Windows 下无法编译，因此完整引擎的 Linux 构建仍不成立（README 中相关表述已不准确）。

## 五、已知事项

- 中转站网关不把 `usage` 分片转发给 SSE 客户端：流式响应里没有 `usage` 字段，token 数请查 `GET /v1/usage`（上游 usage 的解析与计费正常）。
- 计价为占位价（`--price-micro-per-ktok` 默认 1），上线前需按真实成本配置。
- 第三方 Agent 的配置文件字段名未经逐家真机验证，因此对应适配器只设置环境变量、不写它们的配置文件。
- 仓库不带 `.git` 的检出无法直接执行 `build.cmd fmt`（`gofmt -l` 会列出若干既有未格式化文件，属历史状态）。
