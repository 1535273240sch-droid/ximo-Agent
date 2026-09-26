# 长期记忆（mem0 集成）

本文档说明 XimoAgent Go 引擎如何接入 [mem0](https://github.com/mem0ai/mem0)
（Apache-2.0，66k★）作为跨会话长期记忆，以及怎么把它跑起来、怎么验证它真的在工作。

## 1. 它做了什么

| 时机 | 行为 | 失败时的行为 |
|---|---|---|
| run 开始前 | 用本轮提问向 mem0 检索相关记忆，作为**一条独立 system 消息**注入上下文 | 未启用 / 连不上 / 超时 / 鉴权失败 → **不注入任何内容**，run 照常开始 |
| run 结束后 | 把这一轮的「用户提问 + 最终答复」投递给 mem0 抽取长期记忆（后台有界队列） | 队列满或抽取失败 → 丢弃并计数，**不影响 run 的收尾** |
| 模型主动调用 `memory` 工具 | `search` / `add` / `list` / `forget` 四条路径直接读写同一份记忆 | 返回明确的错误文本给模型 |

设计原则：**记忆是增强，不是前提**。这条链路上任何一个环节坏掉，工作台都必须和
没有这个特性时完全一样——包括消息表的字节。

## 2. 记忆消息插在哪里，以及为什么

```
[0] system — 稳定系统提示词（模式提示词 + 专家人格 + 技能，25KB+）
[1] system — 长期记忆（mem0 召回结果）        ← 本特性新增，且只在有命中时存在
[2] user   — 本轮提问
```

三条硬约束：

1. **插在系统提示词之后**。这样系统提示词仍是请求最前面的连续字节，新增记忆不参与
   它的哈希，不会让服务商的 prompt cache 整段失效（v1 prefix-shape 的教训，
   `internal/context/prefixshape.go` 把它固化成不变量）。
2. **一个 run 只插一次**，在 `Engine.Submit` 里完成；之后消息表只追加。重放、恢复、
   压缩都不会再动它。
3. **没有命中就完全不插**。空记忆块不会在请求里留下任何字节——`memory` 服务在但
   今天没有相关记忆，与没有这个特性时的请求逐字节一致。

代码位置：`internal/engine/memory.go`（`injectMemoryMessage` / `recallMemory` /
`rememberTurn`），注入点在 `Engine.Submit`，回填点在 `Engine.finishRun`。

## 3. 部署 mem0

### 3.1 官方自托管服务（推荐，功能最全）

mem0 仓库的 `server/` 是一个 FastAPI 服务，带 dashboard 与 API Key 管理：

```bash
git clone https://github.com/mem0ai/mem0
cd mem0/server
make bootstrap      # 起 docker compose（Postgres + pgvector + API + dashboard），
                    # 并在输出的 === Ready === 块里给出首个 admin API Key
# 或者手动：docker compose up -d
```

需要 Docker Desktop（Windows 上还需要 WSL2 与虚拟化，通常要重启一次）。
本仓库把上面这些脚本化了：`scripts\mem0\mem0-up.cmd`（clone + 起栈 + 生成
`server/.env`）、`scripts/mem0/preflight.sh`（起栈前预检）、`scripts/mem0/verify.sh`
（下文第 5 节的验证），端口一律按 8888 写。

### 3.1.1 两个端口，别弄混（写错会让验证全 404）

上游 `server/docker-compose.yaml` 同时发布 API 与 dashboard：

| 端口 | 是什么 | 能不能当 REST API 用 |
|---|---|---|
| **8888** | REST API（容器内是 8000） | **能**。`/memories`、`/search`、`/configure` 都在这里 |
| 3000 | dashboard（浏览器界面、初始化向导、Configure 页） | **不能**。它只代理 `/api/health` 与 `/api/auth/refresh`，`/memories`、`/search` 打到它上面会 404 |

所以 `memory.endpoint` 一律写 `http://127.0.0.1:8888`。**3000 不是 API 端口**——
把 endpoint 写成 3000 是最容易犯也最难自查的错误：服务其实是活的，浏览器也打得开，
只有记忆这条链路静默地全 404。

### 3.2 本机 Python 库（不想装 Docker 时）

mem0 的 Python 库支持本地嵌入式向量库（Qdrant local 模式），不需要 Postgres：

```bash
pip install "mem0ai[nlp]"
# 用一个薄包装暴露与本工程约定的同一组 REST 端点（/memories、/search），
# 或直接以 http 方式挂载 mem0 官方的 MCP server
```

本工程只依赖下面这几个端点的形状（取自 mem0 `server/main.py`）：

| 用途 | 请求 |
|---|---|
| 抽取（写） | `POST /memories`，body `{messages:[{role,content}], user_id, agent_id?, run_id?, metadata?}` |
| 检索（读） | `POST /search`，body `{query, top_k, filters:{user_id}}` |
| 列出 | `GET /memories?user_id=…&top_k=…` |
| 遗忘 | `DELETE /memories/{id}` |
| 健康检查 | `GET /configure` |

### 3.3 两个必须准备的依赖

mem0 的服务端自己不做推理，它把两件事外包出去，都要用 `POST /configure`
（等价于 dashboard 的 Configure 页）设置好。**`POST /configure` 需要 admin 角色的 key**，
普通 key 会 403。

#### 3.3.1 抽取用的 LLM

可以复用本工作台已配置的 OpenAI 兼容服务商（`config.json` 的 `provider.base_url` +
密钥）；DeepSeek 也可以，写成 `provider=openai` + `openai_base_url=https://api.deepseek.com/v1`。

#### 3.3.2 向量模型（embedder）：必须有独立来源

**DeepSeek 没有 embeddings 接口**，所以拿 DeepSeek 当 extractor 的同时，
仍然必须另找一家 embedding 服务。没有它，记忆只会「写进去但召回不到」。

**硬约束：`provider` 是白名单。** 上游 `server/main.py:62`：

```python
BUNDLED_LLM_PROVIDERS      = ("openai", "anthropic", "gemini")
BUNDLED_EMBEDDER_PROVIDERS = ("openai", "gemini")
```

`_validate_bundled_providers` 对名单外的 provider **直接返回 400**。embedder 的白名单
更窄：只有 `openai` / `gemini`，**没有** `anthropic`。因此
`provider="ollama"` / `"siliconflow"` / `"dashscope"` 这类写法会被拒——本地 Ollama 与
国内各家**一律写成 `provider="openai"` + `openai_base_url=<实际地址>`**
（上游 `mem0/llms/openai.py:51` 与 `mem0/embeddings/openai.py:23` 都读这个字段）。
`POST /configure` 的 body 形状如下（`scripts/mem0/verify.sh configure` 发的就是它）：

```jsonc
// (a) 本地 Ollama —— API 默认 11434，先 `ollama pull nomic-embed-text`
{"llm":      {"provider": "openai", "config": {"api_key": "<DeepSeek 或别家的 key>",
                                               "model": "deepseek-chat",
                                               "openai_base_url": "https://api.deepseek.com/v1"}},
 "embedder": {"provider": "openai", "config": {"api_key": "ollama",
                                               "model": "nomic-embed-text",
                                               "openai_base_url": "http://127.0.0.1:11434/v1"}}}

// (b) 硅基流动（OpenAI 兼容面在 /v1）
{"embedder": {"provider": "openai", "config": {"api_key": "sk-...",
                                               "model": "BAAI/bge-m3",
                                               "openai_base_url": "https://api.siliconflow.cn/v1"}}}

// (c) 阿里云 DashScope / 百炼（兼容模式）
{"embedder": {"provider": "openai", "config": {"api_key": "sk-...",
                                               "model": "text-embedding-v3",
                                               "openai_base_url": "https://dashscope.aliyuncs.com/compatible-mode/v1"}}}
```

- 这里的 `api_key` 是**服务端存着、给上游服务商用的密钥**：与下文 `memory.secret_ref`
  指向的 mem0 API Key 是两把不同的钥匙，不要混用。
- 本地 Ollama 不校验密钥，但这一项要填一个非空占位串（`scripts/mem0/verify.sh` 也要求
  `MEM0_LLM_KEY` / `MEM0_EMBED_KEY` 非空）。
- **换 embedder 模型/维度之后，pgvector 里已建的集合维度会不匹配**：需要清库重建
  （`docker compose down -v`）或换一个新的 `POSTGRES_COLLECTION_NAME`。

**这一步没配好，记忆只会「写进去但召回不到」**——这是最容易误判为「功能坏了」的
配置错误，所以放在最前面说。

## 4. 配置

`config.json`（`<BaseDir>/config.json`，Windows 上通常是
`%APPDATA%\ximo-agent\config.json`；设了 `XIMO_HOME` 则是 `%XIMO_HOME%\config.json`）
新增一段；`config.default.json` 里有完整默认值：

```json
{
  "feature_flags": { "memory.mem0": true },
  "memory": {
    "enabled": true,
    "endpoint": "http://127.0.0.1:8888",
    "secret_ref": "",
    "user_id": "ximo-user",
    "agent_id": "ximo-agent",
    "top_k": 5,
    "recall_max_chars": 1200,
    "write_back": true,
    "timeout": 700000000,
    "extract_timeout": 30000000000,
    "queue_depth": 32,
    "max_inflight": 2
  }
}
```

### 4.1 两个开关：确切键名与文件位置

两个开关都在**同一个文件**里，也就是上面那份 `config.json`（不是两个文件、也不是
环境变量）：

| 级别 | 文件里的位置（字面键名） | 作用 |
|---|---|---|
| 一级熔断 | 顶层 `feature_flags` 对象里的键 **`"memory.mem0"`**（含点号，是一个完整键名，不是嵌套对象）：`"feature_flags": { "memory.mem0": true }` | 整机/整环境熔断，用来不改用户配置就关掉整条链路 |
| 用户级开关 | 顶层 `memory` 对象里的 **`"enabled": true`** | 用户级开关 |

两者都开才生效，改完要**重启工作台**。默认两个都是 `false`
（`internal/config/config.go` 的 `DefaultFeatureFlags` 与 `config.default.json`），
保持默认是刻意的：记忆依赖外部服务，用户没显式打开之前不该改变任何行为。

**环境变量不是替代品：`XIMO_FEATURE_MEMORY_MEM0=true` 不生效。**
`internal/config` 里确实有一个 `XIMO_FEATURE_*` → flag 名的转换（`applyEnvOverrides`，
`XIMO_FEATURE_ENGINE_V2` → `engine.v2`），但它挂在 `FeatureManager` 上，而装配期读的是
`internal/bootstrap/memory.go` 里的 `cfg.FeatureFlags`——那是**从配置文件反序列化出来的
map**，与 `FeatureManager` 不是同一条路（`NewFeatureManager` 目前只在测试里被调用）。
所以开这个特性只有一条路：改配置文件里的这两个键。

### 4.2 其余字段

- **时长单位是纳秒**（与 `supervisor` / `ipc` 段一致）：`700000000` = 700ms，
  `30000000000` = 30s。
- `timeout` 是**每个 run 开始前多出的等待上限**，刻意取得很小；
  `extract_timeout` 是后台抽取的上限，大两个数量级是正常的（那是「写」）。
- `secret_ref` 指向系统凭据库中的 mem0 API Key（界面「保存密钥」写入的那套机制），
  **不写明文**。服务端 `AUTH_DISABLED=true` 的本地开发部署可以留空。
- `write_back` 为 `*bool`：不写就是默认开启。**不要**把它当成普通 bool——省略键
  却被零值静默关掉，会表现为「记忆只读不写」，是最难排查的一类故障。

## 5. 验证它真的在工作

下面 1–3 步由 `scripts/mem0/verify.sh` 自动化（默认打 8888，且**拒绝**打到 3000）；
它的第 3 步会把「库内条数」与「`POST /search` 命中数」放在一起看：**条数 > 0 而命中 = 0
就是 embedder 没配好**（见 §3.3.2）。

1. **服务活着**
   ```bash
   curl -s -H "X-API-Key: <key>" http://127.0.0.1:8888/configure
   ```
   注意：mem0 的 `verify_auth` 一旦看到 `Authorization: Bearer` 就按 JWT 解析且
   不回退，所以**用 API Key 时必须发 `X-API-Key`**。本工程的客户端只发这一个头。
   连不上先确认端口是 **8888**（API）而不是 3000（dashboard，打上去是 404）。

2. **一轮对话后记忆里有东西**
   ```bash
   curl -s -H "X-API-Key: <key>" "http://127.0.0.1:8888/memories?user_id=ximo-user&top_k=10"
   ```

3. **下一轮真的召回了**：观察日志里的告警（正常情况下没有），或者把
   `memory.top_k` 调大后对比两轮请求——第一轮命中时，请求的消息数组里会多出
   一条 `--- 长期记忆 (mem0) ---` 开头的 system 消息。

4. **降级也正常**：把 mem0 停掉再跑一轮，应当只有一条
   「长期记忆召回失败，本轮按「无记忆」继续」的告警，对话不受影响。

## 6. 运行时计数与排障

`internal/memory.Service.Stats()` 提供：召回次数/命中/失败/注入字符数，回填
入队/成功/失败/跳过/丢弃。语义：

- `BackfillSkipped` 不为零是**正常的**：同一个 run 被重复收尾（取消、崩溃恢复、
  重复 finish）时，幂等闸只让第一次真正抽取。
- `BackfillDropped` 不为零说明回填队列被打满：调大 `queue_depth` / `max_inflight`，
  或看看 mem0 的抽取是不是太慢。
- `LastError` 是最近一次失败的脱敏描述，密钥永远不会出现在里面。

## 7. 已知边界（刻意如此）

- **只投递「本轮」问答**。引擎每次 run 只拿到当前提示词，完整会话历史由前端拼在
  系统提示词里，本进程取不到原文。让 mem0 面对一对问答，足以提炼偏好与结论；
  伪造一份引擎手上没有的历史才是真正有害的。
- **不做向量迁移与记忆去重**。记忆的合并/去重交给 mem0 自己的算法（v3 是
  单遍 ADD-only 抽取 + 实体链接），本工程不重复实现，也不在自己的库里存副本。
- **不新增数据库 schema**。「这个 run 已回填」直接复用 `tool_idempotency` 表的
  原子 `Claim`（该表由 `0002_tool_idempotency.sql` 建立），不新开一张表。
- **不做跨用户共享**。`user_id` 默认 `ximo-user`，是单机单用户的语义。

---

## 附：进程内后端（推荐，零依赖）

上面几节讲的是 **mem0 自托管**路线：需要 Docker + Postgres(pgvector) + 一个抽取用的
LLM + 一个**额外的向量模型**（DeepSeek 没有 embeddings 接口）。这套东西部署面大，
对"就想让工作台记住我的偏好"这个目标来说不成比例。

引擎现在自带一个**进程内后端**：记忆存在本机一个 SQLite 文件里，随引擎一起跑，
不需要 Docker、不需要任何服务、不需要密钥。

### 怎么开

`config.json` 里两个开关打开即可，`backend` 都不用写（留空时：给了 `endpoint` 走
mem0，没给就走进程内）：

```jsonc
{
  "feature_flags": { "memory.mem0": true },   // 一级开关
  "memory": {
    "enabled": true,                          // 二级开关
    "backend": "embedded",                    // 可省略；显式写更清楚
    "user_id": "ximo-user",
    "agent_id": "ximo-agent",
    "top_k": 5,
    "recall_max_chars": 1200,
    "write_back": true,
    "timeout": 700000000,
    "extract_timeout": 30000000000,
    "queue_depth": 32,
    "max_inflight": 2
  }
}
```

记忆文件位置：`<BaseDir>/data/memory.db`。删掉它 = 清空全部记忆。

### 与 mem0 路线的差别（有意为之，不是缺陷）

| | 进程内后端 | mem0 自托管 |
| --- | --- | --- |
| 依赖 | 无（引擎自带） | Docker + Postgres + LLM + 向量模型 |
| 写入 | **原样存这一轮问答** | LLM 抽取成"事实" |
| 检索 | **词法**（中文单字+二元组，英文按词） | 向量语义检索 |
| 同义改写命中 | 弱（"深色界面" 命不中 "暗色主题"） | 强 |
| 数据位置 | 本机文件，不出机器 | 你自托管的 Postgres |

选哪条：只想"记住我说过的偏好"→ 进程内后端足够；要更强的语义召回、或者团队多人
共享一套记忆 → 用 mem0 路线。两者共用同一套配置与同一条引擎链路，切换只改
`backend` 与 `endpoint`。

### 实现位置

- `internal/memory/embedded.go` —— 进程内后端（SQLite）
- `internal/memory/lexical.go` —— 词法分词与打分（要升级成语义检索，换这里的打分函数即可）
- `internal/memory/backend.go` —— 后端接口（`*Client` 与 `*EmbeddedBackend` 都满足它）
- `internal/bootstrap/memory.go` —— 按 `backend` 选后端

测试：`go test ./internal/memory/...`（含写入幂等、词法排序、同库多用户隔离、
后端选择与校验）。
