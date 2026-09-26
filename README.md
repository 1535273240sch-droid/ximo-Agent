# XimoAgent Go v2

高并发、可自愈的本地智能体工作台。**Go 引擎 + Electron 桌面前端**，通过本地 IPC 二进制帧协议通信。

本分支（`v2-go`）是 v2 重写版本的完整交付：后端从「各模块各自对着 mock 开发」推进到「真正装配起来、能跑完整 run 的多进程系统」，并补齐了可交互的桌面前端。

---

## 架构

```
┌─────────────────────────────────────────────────────────┐
│  Electron 前端（React 18 + Tailwind）                    │
│  ├─ 5 套高定主题（Obsidian / Graphite / Silver / ...)     │
│  ├─ 对话界面（事件驱动流式渲染 + 工具执行链可视化）        │
│  ├─ 专家库（254 位专家）、知识库、设置页                  │
│  └─ 模型服务商与 API 密钥配置（写入 OS 凭据库，不回显）    │
└───────────────────────┬─────────────────────────────────┘
                        │ IPC 定长二进制帧（29 字节 BigEndian 头）
┌───────────────────────▼─────────────────────────────────┐
│  Supervisor 进程（看门狗 + 业务帧代理）                   │
│  ├─ 心跳巡检、崩溃检测、退避重启                          │
│  ├─ 未注册帧转发给 Engine                                 │
│  └─ Engine 重启后下发恢复指令                             │
└───────────────────────┬─────────────────────────────────┘
                        │ 同协议，独立端口
┌───────────────────────▼─────────────────────────────────┐
│  Engine 进程（业务真相所在）                              │
│  ├─ SQLite WAL（单写多读）+ 迁移                          │
│  ├─ Agent 循环：规划 → 思考 → 工具执行 → 校验 → 压缩      │
│  ├─ 工具运行时：file / git / knowledge / web + Worker 池  │
│  ├─ Provider：DeepSeek / OpenAI / 任意兼容接口            │
│  ├─ 崩溃恢复：事件溯源 + 幂等分类 + 检查点               │
│  └─ 密钥管理：系统凭据库加密，只写不读                    │
└─────────────────────────────────────────────────────────┘
```

---

## 快速开始

### 1. 构建后端

```cmd
build.cmd exe        REM 产出 dist\ximo-agent.exe
build.cmd package    REM 产出 dist\ximo-agent-v2.0.0.zip（含 exe + migrations + 配置）
```

需要 Go 1.27+。若依赖拉取失败，先设好代理：

```cmd
set HTTPS_PROXY=http://127.0.0.1:7897
```

### 2. 构建前端

```cmd
cd frontend
npm install
npm run build        REM 产出 out/
npm run dev          REM 开发模式（热重载）
```

### 3. 运行

```cmd
npm run dev          REM 在 frontend 目录下，会自动拉起后端
```

前端会自动定位 `dist/ximo-agent.exe` 并作为子进程启动，无需手动开后端。

### 4. 配置模型

启动后在 **设置 → 模型与 API 凭据** 中：
1. 填入 API 密钥（存入 Windows 凭据管理器，明文不落盘）
2. 点「获取模型」向服务商查询可用模型列表
3. 选择模型 → 保存配置（热重载，无需重启）

---

## 命令行用法

```cmd
ximo-agent.exe --role=supervisor                    REM 进程监管（推荐入口）
ximo-agent.exe --role=engine                        REM 引擎
ximo-agent.exe --role=ui                            REM UI 宿主
ximo-agent.exe --role=worker --kind=terminal        REM 单个 Worker
ximo-agent.exe --version
ximo-agent.exe --health
```

---

## 配置

`config.default.json` 是完整示例。关键段：

```jsonc
{
  "provider": {
    "id": "deepseek",
    "base_url": "https://api.deepseek.com",
    "model": "deepseek-chat",
    "secret_ref": "dpapi:...",   // 密钥引用，不是明文
    "context_window": 1000000,
    "max_output_tokens": 8192
  },
  "storage": { "db_path": "", "profile": "balanced" },
  "runtime": {
    "auto_mode": "safe",              // yolo | safe | coding | office | design
    "workspace_root": "",             // 工具沙箱根目录
    "worker_pools": { "terminal": 2 } // 按 kind 启用的 Worker 池
  }
}
```

配置文件默认落在 `<BaseDir>/config.json`（Windows: `%APPDATA%\ximo-agent\`）。
可用环境变量 `XIMO_HOME` 覆盖 BaseDir。

### 密钥存储

密钥由操作系统级凭据库加密，**没有「读回明文」的接口**：

| 平台 | 后端 |
|---|---|
| Windows | Credential Manager（非交互会话下自动降级 DPAPI 文件后端） |
| macOS | Keychain |
| Linux | Secret Service |

可用 `XIMO_SECRETS_BACKEND` 强制指定：`dpapi` / `cred`。绝无明文存储降级。

### 长期记忆（mem0）

引擎可接入 [mem0](https://github.com/mem0ai/mem0)（Apache-2.0）做跨会话长期记忆：
每次 run 开始前自动召回相关记忆注入上下文，run 结束后自动把这一轮问答交给 mem0 抽取。
默认**关闭**；未启用或服务不可用时整条链路静默降级，请求与行为跟没有该特性时逐字节一致。

```cmd
scripts\mem0\mem0-up.cmd          REM 起 mem0 服务（需 Docker Desktop，版本已固定）
```

```jsonc
{
  "feature_flags": { "memory.mem0": true },
  "memory": {
    "enabled": true,
    "endpoint": "http://127.0.0.1:8888",   // 8888 = API；3000 是 dashboard，不代理 /memories、/search
    "secret_ref": "",            // 指向凭据库中的 mem0 API Key，不留明文
    "user_id": "ximo-user",      // 记忆归属：跨会话共享的就是这一维度
    "top_k": 5,
    "recall_max_chars": 1200,
    "write_back": true,          // 省略即开启；不要靠零值判断
    "timeout": 700000000         // 纳秒。召回给每个 run 额外带来的等待上限
  }
}
```

记忆以**独立 system 消息**插在稳定系统提示词之后（不破坏 prompt cache 前缀，也不
影响 `PrefixShape` 诊断）；模型还可主动调用 `memory` 工具做检索/写入/浏览/遗忘。

> ⚠️ 起栈后必须在 mem0 侧单独配置**向量模型**：DeepSeek 没有 embeddings 接口，
> 需要本地 Ollama、硅基流动或 DashScope 之类。这一步没配好，表现是「记忆写进去了
> 但召回不到」。

完整说明（部署两条路径、REST 契约、排障与运行时计数、已知边界）见
[`docs/长期记忆-mem0.md`](docs/长期记忆-mem0.md)；后端安装与启停见
[`scripts/mem0/README.md`](scripts/mem0/README.md)。

---

## 验证

```cmd
build.cmd verify      REM build + vet + test-short（CI 口径）
build.cmd test        REM 全量测试（含 10000 run 压力测试，约 25s）
build.cmd vet
build.cmd fmt         REM 列出需要格式化的文件
```

测试分层：

| 目录 | 内容 |
|---|---|
| `internal/*/*_test.go` | 模块单元测试 |
| `internal/engine/crash_test.go` | 真实子进程 + SIGKILL 崩溃恢复 |
| `tests/e2e/` | 跨进程 IPC：提交/取消/恢复/重连续拉 |
| `tests/stress/` | 10000 run 真实压力测试（0 损坏 0 泄漏） |
| `tests/chaos`, `tests/recovery`, `tests/fuzz` | 混沌、恢复、模糊测试 |

---

## 目录结构

```
cmd/ximo-agent/          进程入口（supervisor / engine / worker / ui 四种角色）
internal/
  bootstrap/             装配层：把全部模块组装成可运行的 Engine
  ipcapi/                IPC 业务帧协议（提交/取消/状态/事件/恢复/密钥/配置/模型）
  agent/                 Agent 状态机、规划、循环、上下文压缩决策
  engine/                Engine 核心、SessionActor、崩溃恢复
  scheduler/             三层限流、加权公平队列、资源池
  storage/               SQLite WAL、事件流、Outbox、迁移
  checkpoint/            CAS 内容寻址存储、Manifest
  tool/                  统一工具运行时、权限引擎、沙箱、幂等
  worker/                Worker 池（browser/dynamic/mcp/terminal/office/computer-use）
  provider/              多服务商适配、重试熔断、流式响应
  context/               Context 压缩状态机、BPE 分词、LRU 缓存
  expert/                254 位专家静态资源、子 Agent 编排
  knowledge/             BM25 知识库
  secrets/               跨平台密钥管理（DPAPI / Keychain / SecretService）
  observability/         指标、链路追踪、脱敏日志
  supervisor/            进程看门狗、退避重启、恢复下发、帧转发
  types/                 跨模块契约类型、错误码、事件定义
frontend/                Electron + React 桌面应用
  src/main/              主进程：后端进程管理 + 帧协议客户端
  src/preload/           安全桥（contextIsolation）
  src/renderer/          UI（主题 / 组件 / 状态）
  src/shared/            共享类型与帧协议 TS 实现
migrations/              SQL 迁移脚本
tests/                   跨模块与端到端测试
docs/                    集成报告、接口裁决记录
release/                 升级与回滚脚本
```

---

## 关键设计

**事件溯源**：run 的一切状态由持久化事件流推导，不依赖内存。断线重连以 `lastSeq` 续拉，天然幂等。

**幂等与恢复**：工具调用按 `idempotent / detectable / non_idempotent` 分类。崩溃恢复时幂等调用自动重放，非幂等调用挂起等待人工确认，绝不静默重复副作用。

**进程级自愈**：Supervisor 守护 Engine，崩溃后按 1s→2s→4s→8s→16s→30s 退避重启，并通过 IPC 下发恢复指令，由 Engine 扫描持久化日志继续未完成的 run。

**Fail-closed 安全**：权限引擎默认拒绝；密钥无明文降级；工具沙箱限制在 `workspace_root` 内；Worker 高危域（terminal/computer-use）默认关闭。

---

## 环境要求

- Go 1.27+
- Node.js 22+（前端）
- Windows 10/11（主要目标；Linux/macOS 后端可编译运行，前端需自行验证）

---

## 许可

见 `LICENSE`。

---

## 中转站网关与通用插件

除 Agent 运行时外，本仓库还交付两个可独立使用的工具，外加一项引擎侧特性：

| 交付物 | 一句话定位 |
|---|---|
| `ximo-gateway`（`cmd/ximo-gateway`） | 本仓库自带的**服务端中转站**：把多个上游服务商收口成一套账号 / API Key / 额度账本 / 用量 / 审计，对外提供 OpenAI 兼容（`/v1/chat/completions`）与 Anthropic 兼容（`/v1/messages`）入口 |
| `ximo-plugin`（独立 module `plugin/`） | **独立、通用**的 CLI **+ 本地界面**：把任意 OpenAI / Anthropic 兼容的中转站接进本机已装好的 Agent（Claude Code、Codex CLI、Continue、Cursor、aider、XIMO Agent 等），产物是单个静态二进制。本地界面 `ximo-plugin ui`（**玻璃质感 + 羊皮卷**风格，只绑回环地址、带会话令牌） |
| mem0 长期记忆（`internal/memory/**`） | 引擎侧特性：跨会话召回与写回，与上面两者无依赖关系（见上节「长期记忆（mem0）」） |

三者关系：`ximo-plugin` 是**客户端**，指向 `ximo-gateway`（或任何兼容中转站）；`ximo-gateway` 是**服务端**，自身与 mem0 无关。`ximo-plugin` 是独立 Go module（`github.com/1535273240sch-droid/ximo-plugin`），**不 import 主仓库任何 `internal/**`**，可单独编译后拷到别的机器上用。

### 最小快速开始

```bash
# 服务端：编译 + 启动（默认只绑回环 127.0.0.1:8600；管理口令无默认值，缺失则启动失败退出 2）
go build -o ximo-gateway.exe ./cmd/ximo-gateway
export XIMO_GATEWAY_ADMIN_TOKEN='<你自己生成的强口令>'
./ximo-gateway.exe --db ./gw.db
# 只跑迁移然后退出：./ximo-gateway.exe --db ./gw.db --migrate-only

# 客户端：独立 module，单独编译
cd plugin
go build -o ximo-plugin.exe ./cmd/ximo-plugin
./ximo-plugin.exe doctor --gateway http://127.0.0.1:8600    # 逐项体检；有 FAIL 时退出码 1
./ximo-plugin.exe login  --gateway http://127.0.0.1:8600    # 设备码登录（用户码在网关侧授权）
./ximo-plugin.exe models --gateway http://127.0.0.1:8600
./ximo-plugin.exe apply  --gateway http://127.0.0.1:8600 --spec ximo-agent --dry-run
./ximo-plugin.exe apply  --gateway http://127.0.0.1:8600 --spec ximo-agent --yes

# 本地界面（玻璃质感 + 羊皮卷风格）：只绑 127.0.0.1，静态资源已嵌进二进制
./ximo-plugin.exe ui --gateway http://127.0.0.1:8600          # 默认 127.0.0.1:8787，并尝试打开浏览器
./ximo-plugin.exe ui --no-open --port 8787                    # 不自动开浏览器，只打印带会话令牌的地址
```

`ui` 的实际参数就是 `[--port 8787] [--no-open] [--gateway <url>]`（外加全局 `--home <dir>`，用于隔离家目录）。启动后会打印形如 `http://127.0.0.1:8787/?t=<本次会话令牌>` 的地址：**所有 `/api/*` 请求都必须带 `X-UI-Token`，不带就是 401**（防止本机其它程序驱动插件改配置）；令牌不写日志，界面里也不落明文密钥、不提供 `--show-secrets` 的等价开关（要看真值用 CLI）。上方五条 CLI 命令与这个界面走的是**同一套** `plugin/internal/**` 实现。

建用户、充值、配上游 provider、发 API Key 这些**管理侧**动作，以及设备码授权全流程，见 [`docs/XIMO中转站-V1.md`](docs/XIMO中转站-V1.md) §2.3 的 curl 逐条示例。

### 现状声明（照实写）

- 单机 SQLite（单进程单写者），**未接 Redis / NATS**；限流、熔断都在进程内。
- **无真实价目表**：`--price-micro-per-ktok` 是全局占位单价，计费金额**不可作为账单**。
- 监听地址**默认只绑回环**（`127.0.0.1:8600`），要对外必须显式改 `--addr`。
- 管理口令与上游密钥都不落明文：口令只有 PBKDF2 哈希，上游密钥只存 `internal/secrets` 的引用。

### 文档

- 中转站：[`docs/XIMO中转站-V1.md`](docs/XIMO中转站-V1.md)（接口清单、差异表、已知缺陷与未做项）
- 插件：[`plugin/docs/README.md`](plugin/docs/README.md)、[`plugin/docs/QUICKSTART.md`](plugin/docs/QUICKSTART.md)
- 长期记忆：[`docs/长期记忆-mem0.md`](docs/长期记忆-mem0.md)
