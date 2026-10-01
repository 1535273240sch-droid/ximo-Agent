# XimoAgent v2.4.0 更新说明

发布日期：2026-10-01 ｜ 版本跨度：v2.3.0 → v2.4.0

## 一、本次更新概述

v2.3.0 按审核报告交付了 P0 闭环与 Work Log，并实现了 Synapse 记忆后端但**没有装配**。
v2.4.0 补齐这条链路的最后一段：**记忆真正跑起来，并且有一个能看、能改、能删的界面**。

三件事：

1. **Synapse 后端接进生产链路**——`memory.backend: "synapse"` 现在真的会构造图记忆后端，
   首次启用自动迁移旧数据，`memory.recalled` 事件会把「回忆了哪几条、经由什么路径」
   送到 Work Log 与记忆页。
2. **记忆页（P1-c）**——力导向图 + 节点详情 + 列表视图 + 设置项，节点可编辑、置顶、
   归档、遗忘、手动连线，可导出/导入 JSON。
3. **10 个记忆 IPC 帧**（`system.memory.*`）——界面与引擎之间的读写面，含分页子图查询、
   邻域聚焦、搜索、统计与清空。

---

## 二、新增

### 1. Synapse 记忆后端装配

- `memory.backend` 支持 `"synapse"`（也接受别名 `"graph"`）；`Active()`/`Validate()`/
  `EffectiveBackend()` 三处都已识别，进程内后端不需要 `endpoint`。
- 库文件沿用 `<DataDir>/memory.db`：Synapse 用 `mem_*` 前缀表，与旧 embedded 后端的
  `memories` 表同库共存，**不动引擎库的迁移注册表**。
- **首次启用自动迁移**：`MigrateAll(ctx, oldEmbedded, knowledgeDB)` 把旧 `memories`
  每行导为 `fact` 节点、knowledge 条目导为 `fact`/`procedure` 并把 tags 变成
  `entity`/`topic` 节点。迁移按内容哈希幂等，重复启动不会产生重复节点，旧数据不删除。
- 抽取器接上当前生效的 provider：一次严格 JSON 抽取（最多 8 条事实），
  **provider 未配置、超时或 JSON 解析失败一律回退规则抽取**，绝不因为抽取失败
  影响 run 或写入。

### 2. `memory.recalled` 事件（Work Log 的「回忆 N 条记忆」真的会出现）

- 引擎新增可选接口 `MemoryRecallReporter`：端口除了返回可注入文本，还能给出结构化条目。
- 每次召回后发一条 `memory.recalled`，载荷 `{count, items:[{id,text,via}]}`，
  其中 `via` 是扩散激活的路径描述（如 `entity:Go → fact:并发偏好`），
  用来回答「为什么想起它」——这是审核文档 4.2 第 4 条「可解释」的落点。
- 未实现该接口的端口（embedded / mem0）行为与改动前完全一致：只注入文本，不发事件。

### 3. 记忆页（审核文档 4.9）

- **网络视图**：`d3-force` 力导向 + Canvas 渲染（新增依赖仅此一个）。节点颜色按 `kind`、
  大小按 `importance` 与度数、**边粗细按惰性衰减后的 `effective_weight`**——按原始权重
  画会把早已失效的连接画得和刚建立时一样粗，那是在骗用户。支持拖拽、缩放、点击看详情、
  搜索高亮。
- **节点详情**：完整正文、来源 run、关联节点列表（带权重条）、用过 N 次 / 最近使用；
  操作：编辑、置顶、归档、遗忘（二次确认）、手动连线（选目标 + 关系下拉）。
- **列表视图**：按 kind / 实体 / 主题筛选，与图共用同一数据源，方便批量清理。
- **设置**：总开关、允许自动抽取、允许后台整理、嵌入模型（可选，界面说明「未配置时
  同义改写命中率较低」）、导出 / 导入 JSON、清空全部记忆（要求输入确认字面量
  `DELETE_ALL`，与后端校验一致）。
- **「点亮」动画**：收到 `memory.recalled` 时被召回的节点依次脉冲、激活路径的边流光一次；
  开启系统「减少动态效果」后全部关闭。
- **与 Work Log 联动**：展开「回忆 N 条记忆」步骤，点某条记忆即跳到记忆页并聚焦该节点。

### 4. 记忆 IPC 帧（`system.memory.*`，只追加）

| 帧 | 作用 |
|---|---|
| `system.memory.graph` | 分页取子图；支持 kind 过滤、含归档、词法搜索、以某节点为中心取邻域 |
| `system.memory.node.get` | 单个节点完整详情（正文 + 相邻边 + 邻居 + 最近召回记录） |
| `system.memory.node.update` | 改标题/正文/重要度/置顶/状态（指针三态：`nil` = 不改） |
| `system.memory.node.delete` | 硬删除节点及其所有边（「遗忘」，不是归档） |
| `system.memory.link` | 显式建立一条边（rel 白名单与数据库 CHECK 一致） |
| `system.memory.consolidate` | 手动触发一次睡眠整理 |
| `system.memory.export` / `.import` | 可读 JSON 导出 / 按哈希幂等导入 |
| `system.memory.stats` | 计数快照 + 当前后端名 + 是否启用 |
| `system.memory.clear` | 清空全部记忆（需 `confirm: "DELETE_ALL"`） |

---

## 三、设计取舍（值得单独说明的几处）

1. **`update` 用指针三态而不是零值**：把「用户没动这个字段」与「用户想把它置空」
   混为一谈，会静默清掉用户的数据。只改标题的请求不会把 importance 归零。
2. **`clear` 保留库文件、只删数据**：连接已被持有，Windows 上删一个被打开的文件会失败
   并留下半开状态。语义上「清空记忆」是数据操作，不是文件管理。
3. **能力缺失一律明确报错**：没有记忆后端时十个帧全部返回错误，不做静默成功——
   静默成功会让界面显示「已遗忘」而库里那条记忆还在。
4. **图查询有界**：分页 + 邻域深度上限 + 邻域节点上限。文档 4.11 把单用户容量定在
   ≤5 万节点，一次把全图塞进 IPC 帧既超帧长上限，也让渲染层卡死。
5. **边宽按 `effective_weight`**：只画原始权重的话，一条 45 天前用过、实际关联已经很弱的
   边看起来仍然和刚建立时一样粗，那是在骗用户。
6. **迁移闸用 `PRAGMA user_version` 而不是「图是否为空」**：后者在用户清空记忆后重启会
   把旧数据重新导回来。迁移本身也做了幂等闸——否则重跑会把 `use_count` 顶高，用户会看到
   「我什么都没干，记忆使用次数涨了」。
7. **手动 `link` 覆盖权重，自动学习「取大者」**：人工编辑是明确意图，应当生效；
   自动学习不该因为一次共现就把一条已经很强的边削弱。

---

## 四、启用方式

两个开关都要开（`memory.enabled` 是用户级开关，`feature_flags.memory.mem0` 是整机熔断）：

```jsonc
{
  "feature_flags": { "memory.mem0": true },
  "memory": { "enabled": true, "backend": "synapse", "user_id": "ximo-user" }
}
```

（特性开关的名字沿用 mem0 时代的键，现在同时管三个后端；改成中性名字会破坏既有
配置文件，故保留。）

首次以 `synapse` 启动时会自动把旧 `memories` 表与 knowledge 条目迁移进图，
按内容哈希幂等，旧数据不删除。之后在侧栏「记忆」页可以看到、编辑、遗忘这些记忆。

---

## 五、已知事项与后续

1. **设置页的三项开关只落 localStorage，未写回后端配置**：`memory.enabled`、
   「允许自动抽取」、「允许后台整理」目前在界面上可切换并即时影响本次会话的显示，
   但后端仍按 `config.json` 的 `memory` 段运行。原因是 `RuntimeSettingsPayload`
   里没有记忆段字段，加它属于扩契约，留待下一版（或由用户直接改配置文件）。
2. **嵌入模型（Embedder）未装配**：`SynapseOptions.Embedder` 仍为 nil，即纯词法召回。
   这是文档 4.11 明确的有意取舍（「无嵌入时功能完整、只是同义改写命中率较低」），
   界面里也已如实说明。
3. **`update` 改正文不会重算实体链接**：只重算 tokens/hash/FTS 并同步索引，不隐式
   重写 `mentions` 边——用户改一句话不该悄悄重连整张图。需要时由「整理」处理。
4. **「来源 run」暂不跳转**：只显示 run id。跳转需要「切到对话视图 + 选中对应会话」
   的跨 store 写操作，未擅自扩范围。
5. **`-race` 未跑**：本机无 gcc（cgo 不可用），属环境限制。
6. **一个与本次改动无关的既有失败**：`internal/worker/office` 的
   `TestResolvePathOutsideRootsRejected` 在 Windows 8.3 短路径名（`ADMINI~1`）下失败，
   已在未改动的工作树上复现确认。

---

## 六、验证方式

```cmd
build.cmd verify                     REM build + vet + test-short
cd frontend && pnpm run typecheck     REM tsc（node + web 两个工程）
cd frontend && pnpm test              REM vitest（82 个用例）
cd frontend && pnpm exec electron-vite build
ximo-agent.exe --version              REM 应打印 v2.4.0
```

除了单元测试，`tests/e2e/memory_e2e_test.go` 走**真实 IPC 往返**（独立客户端 → 监听中的
服务端 → 真实 SQLite）覆盖十个记忆帧，并逐字断言响应 JSON 的键名与
`frontend/src/shared/types.ts` 一致——这条链路的失败模式是静默的（键名不一致只会让
界面永远是空的，编译期与单测都不会报错），所以它值得一个单独的端到端测试。
