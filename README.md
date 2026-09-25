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
