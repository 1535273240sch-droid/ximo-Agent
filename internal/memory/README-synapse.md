# Synapse Memory（审核文档第 4 章）实现说明

本目录新增的 `synapse*.go` 是"神经网络式"长期记忆后端：**带权图 + 扩散激活 +
赫布学习 + 惰性衰减 + 后台整理**。它实现既有的 `Backend` 接口，与
`EmbeddedBackend`（扁平表）和 mem0 后端并列，不改变它们任何行为。

## 文件布局

| 文件 | 内容 |
| --- | --- |
| `synapse.go` | 常量（节点 / 边 / 状态字面量、默认参数）、选项、注入接口（`Extractor` / `Embedder`）、纯工具（哈希、实体规范化、脱敏丢弃） |
| `synapse_store.go` | 开库、建表（4.3 的表名 / 列名原样）、节点与边读写、FTS 查询、预编译语句、有界后台队列 |
| `synapse_extract.go` | 写入流水线：采集 → 脱敏 → 抽取 → 去重 → 实体链接 → 建边 → 矛盾检测；`Add` / `GetAll` / `Delete` |
| `synapse_recall.go` | 召回：查询分词 → FTS5 bm25 种子 → 融合 → 2 跳扩散 → 打分 → 预算裁剪 → `RenderBlock` → 异步写 `mem_recall_log` |
| `synapse_learn.go` | 惰性衰减、赫布强化、未采用惩罚、采用判定（纯函数）+ 结算落库 |
| `synapse_consolidate.go` | 睡眠整理：去重合并、主题聚合（并查集）、矛盾裁决、归档、清理、统计 |
| `synapse_migrate.go` | 旧 `memories` / `knowledge_entries` 迁移（幂等、不删旧数据） |
| `synapse_test.go` | 4.12 后端验收项 + 2 万节点召回基准 |

```go
b, err := memory.NewSynapseBackend(dbPath, memory.SynapseOptions{
    UserID:    "ximo-user",
    Extractor: myProviderExtractor, // 可选；nil 走规则抽取
    Embedder:  myEmbedder,          // 可选；nil 即纯词法（默认）
})
```

## 与审核文档不一致 / 超出文档的地方（逐条）

1. **`mem_fts` 的 `content` 列必须写成 `"content"`（引号）**。文档 4.3 的
   DDL 是 `fts5(tokens, title, content='mem_nodes', …)`，但 FTS5 会把
   `content='mem_nodes'` 解析成 **content 选项**，那一列根本不存在（实测报
   `table mem_fts has no column named content`）。加引号后列名仍是 `content`，
   external-content 选项同时生效。表名 / 列名 / 选项值均未改。
2. **追加 3 条 FTS 同步触发器**（`mem_nodes_fts_ai/ad/au`）。external-content
   表不会自动同步，没有触发器索引会与数据脱节。只维护 `mem_fts`，不改表结构。
3. **追加 2 个索引** `ix_mem_edges_src_weight(src, weight DESC)`、
   `ix_mem_edges_dst_weight(dst, weight DESC)`。读取路径要"每个方向取权重最高的
   N 条边"，没有它们 SQLite 只能全扫再排序（实测枢纽节点一次 20ms）。文档要求
   的 `ix_mem_edges_dst` 仍然存在。
4. **`mem_recall_log.used` 三态**：`0` = 待结算、`1` = 被采用、`-1` = 已结算但
   未采用。文档只定义了"是否采用"；增加 `-1` 是为了让赫布结算**幂等**（否则
   每轮写入都会把同一批召回再结算一次，权重被反复推高），同时保留"为什么想起
   它"的解释记录。列类型与名字未改。
5. **召回新增两个预算界**（文档未规定，为 4.11 的 p95 预算而加）：
   - `MaxFrontier`（默认 16）：每跳最多从多少个前沿节点继续扩散。第 1 跳的前沿
     就是种子（≤20，不受影响）；限制的是第 2 跳。
   - FTS 查询词裁剪：查询里"已被二元组覆盖的单字 CJK 词"不再进 `MATCH`
     （写入侧 `mem_nodes.tokens` 仍保留单字）。单字中文几乎命中全库，会逼
     bm25 对整个库逐条打分（实测 30-50ms）。
   这两条把 2 万节点召回 p95 从 ~170ms 压到 ~35ms。
6. **抽取器通过 `Extractor` 接口注入**，生产装配（把 provider 适配成它）留待
   后续阶段：本次不改 `internal/bootstrap`。
7. **迁移的 knowledge 部分需要外部传入 `*sql.DB`**：`knowledge_entries` 在引擎
   库里，本包不 import `internal/storage`。提供 `MigrateFromKnowledge(ctx, db)`
   与 `MigrateAll(ctx, old, knowledgeDB)`，由装配阶段接线；未传时只迁 `memories`
   并置 `KnowledgeSkipped=true`。
8. **`config.go` 未改**（写范围只允许纯追加常量）：`Config.EffectiveBackend`
   不认识 `"synapse"`（会落到 `BackendMem0`），`Validate` 也会拒绝它。要让
   `memory.backend: "synapse"` 生效，装配阶段必须在
   `EffectiveBackend`/`Validate` 的 switch 里加一个 `case memory.BackendSynapse`
   ——**这是本次交付留下的唯一功能性缺口**。
9. **召回日志的 `run_id` 是召回批次 id（`rc_…`）**：`Backend.Search` 的签名里
   没有 runID（`SearchOptions` 只有 TopK，且本次不允许改 types.go）。因此
   `Add(runID)` 结算的是"最近 24h 内所有未结算的召回批次"，并在开始写事务前
   先 `drainBackground` 把异步日志落盘（否则会出现"召回过却没结算"的竞态）。
   装配阶段若要精确绑定 run，可在 `SearchOptions` 上追加字段后收紧。
10. **可选幂等闸**：`SynapseOptions.Ledger`（`Gate`）非 nil 时按
    `ExtractionKey(runID)` 认领。经 `Service` 调用时不要注入——`Service` 已经
    认领过一次。
11. **`SynapseBackend.Path()`** 是新加的诊断方法（与 `EmbeddedBackend.Path()`
    同义），不实现 `Backend` 接口要求。

## 关键行为

- **写入**：单事务；同 hash 只 `use_count++` 并刷新 `last_used`；模型抽取最多
  8 条 fact，失败/超时回退规则抽取（一问一答一条低重要度 fact，不重试）。
- **脱敏**：材料先过 `types.RedactString`，**含疑似凭据的整行丢弃**（不入库、
  不进抽取提示），并有第二道裸令牌检测。
- **矛盾**：同实体 + 同谓词 + 互斥取值（深色/浅色…）→ 旧节点 `superseded` +
  `新—supersedes→旧`；无法判定 → `contradicts` 边，两者保留，整理时裁决。
- **召回**：`Record.Metadata` 带 `via`（种子为 `seed`，扩散为
  `entity:XimoAgent → fact:…`）、`kind`、`importance`、`entities`、`associations`、
  `recall_batch`；`RenderBlock` 输出文档 4.5 第 6 条的注入格式（含 `↳ 关联` 行
  与结尾"可能已过时"句），空块返回空串。
- **学习**：`used_with` 权重 `w += 0.15·(1-w)`（恒 <1）；被想起未采用时该批
  边 ×0.97；`use_count/last_used` 只对采用节点递增。
- **衰减**：`used_with` 半衰期 45 天、`related` 90 天，`mentions/derived_from/
  part_of` 不衰减，pinned 端点不衰减；`<0.03` 且从未 fire 的边在整理时删除。
- **整理**：`Consolidate(ctx)` 由调用方触发，限时 20s、分批、可中断，返回
  `ConsolidateResult`（合并 / 主题 / 裁决 / 归档 / 剪边 / 清日志），统计写回
  `Stats`。

## 验证

```powershell
go -C <src> build ./...
go -C <src> vet ./internal/memory/...
go -C <src> test -short ./internal/memory/...
go -C <src> test -run XXX -bench BenchmarkSynapseRecall20k ./internal/memory/
```

基准（2 万 fact 节点 / 200 entity 枢纽 / ~4 万边，本机）：
`p50 ≈ 1.2-1.5ms，p95 ≈ 35ms`（预算 80ms）。
