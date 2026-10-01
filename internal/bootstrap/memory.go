package bootstrap

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/config"
	"github.com/ximo888ok-netizen/ximo-agent/internal/engine"
	"github.com/ximo888ok-netizen/ximo-agent/internal/memory"
	"github.com/ximo888ok-netizen/ximo-agent/internal/observability"
	"github.com/ximo888ok-netizen/ximo-agent/internal/storage/sqlite"
	"github.com/ximo888ok-netizen/ximo-agent/internal/tool"
	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// memoryExtractionTTL 是「这个 run 已回填过」标记的存活时间。
//
// 取 24 小时：这个标记只需要覆盖「同一次收尾重复到达」与「崩溃后恢复重放」的
// 窗口，而不是永远保留。过期之后的重放会再抽取一次，代价是记忆里可能多一条
// 近似重复项——比永远无法重新抽取要好。
const memoryExtractionTTL = 24 * time.Hour

// buildMemoryService 装配长期记忆（mem0）。
//
// 与其他可选能力（Worker 池、专家）一致：未启用就返回 nil，配置有问题只告警并
// 返回 nil，绝不阻断启动。记忆依赖一个**外部**服务，为一个没开机的 mem0 拒绝
// 启动整个工作台是明显不成比例的——用户会看到「应用打不开」，而不是「记忆不可用」。
func buildMemoryService(cfg *config.Config, app *App, db *sqlite.DB) *memory.Service {
	if cfg == nil || db == nil {
		return nil
	}
	memCfg := memoryConfigFrom(cfg.Memory)
	if !memCfg.Enabled {
		return nil
	}
	// 两级开关：feature flag 用于整机/整环境熔断，memory.enabled 用于用户级开关。
	// 两者都开才生效，这样可以不改用户配置就把整条链路关掉。
	if cfg.FeatureFlags != nil {
		if enabled, ok := cfg.FeatureFlags[config.FlagMemory]; ok && !enabled {
			observability.LogWarn(context.Background(),
				"长期记忆已在 memory 段启用，但特性开关为关，整段按关闭处理",
				map[string]any{"flag": config.FlagMemory})
			return nil
		}
	}
	if err := memCfg.Validate(); err != nil {
		observability.LogWarn(context.Background(), "长期记忆配置无效，功能关闭",
			map[string]any{"err": err.Error()})
		return nil
	}

	backend, ok := buildMemoryBackend(app, cfg, memCfg)
	if !ok {
		return nil
	}
	svc := memory.NewService(memCfg, backend, memoryGate{store: tool.NewIdempotencyStore(
		tool.NewSQLIdempotencyDB(db.ReadDB()),
		toolCallResolver{},
		nil, // nil = 使用 DefaultClassPolicy，与工具路径保持一致
		memoryExtractionTTL,
	)})
	return svc
}

// buildMemoryBackend 按配置选后端：进程内 SQLite（默认，零依赖）、图记忆
// （Synapse，同样零依赖）或 mem0（HTTP）。
//
// 进程内后端失败（目录不可写、库打不开）时返回 false —— 与其它可选能力一致，
// 记忆装不起来只告警，绝不阻断启动。
func buildMemoryBackend(app *App, cfg *config.Config, memCfg memory.Config) (memory.Backend, bool) {
	switch memCfg.EffectiveBackend() {
	case memory.BackendSynapse:
		return buildSynapseBackend(app, cfg, memCfg)
	case memory.BackendEmbedded:
		path := filepath.Join(cfg.Paths.DataDir, "memory.db")
		eb, err := memory.NewEmbeddedBackend(path, memCfg)
		if err != nil {
			observability.LogWarn(context.Background(), "进程内记忆库打不开，长期记忆关闭",
				map[string]any{"path": path, "err": err.Error()})
			return nil, false
		}
		observability.LogInfo(context.Background(), "长期记忆使用进程内后端（SQLite）",
			map[string]any{"path": path, "user_id": memCfg.UserID})
		return eb, true
	}
	observability.LogInfo(context.Background(), "长期记忆使用 mem0 服务",
		map[string]any{"endpoint": memCfg.Endpoint, "user_id": memCfg.UserID})
	return memory.NewClient(memCfg, memory.ClientOptions{
		APIKey: memoryKeyResolver(app, cfg),
	}), true
}

// buildSynapseBackend 装配图记忆后端（审核文档第 4 章）。
//
// 库文件与 embedded 后端同一路径策略：<DataDir>/memory.db。两者表名不冲突
// （synapse 全部用 mem_* 前缀），因此共用同一个文件是安全的，也意味着用户从
// embedded 切到 synapse 时数据就在同一个文件里，迁移只是读同一张库的另一张表。
func buildSynapseBackend(app *App, cfg *config.Config, memCfg memory.Config) (memory.Backend, bool) {
	path := filepath.Join(cfg.Paths.DataDir, "memory.db")
	opts := synapseOptionsFrom(memCfg)
	if app != nil {
		// provider → Extractor：模型可用时走严格 JSON 抽取，不可用时
		// SynapseBackend 内部回退规则抽取（不会因为模型问题丢掉记忆）。
		opts.Extractor = memoryProviderExtractor{
			provider: app.providerPort,
			model:    cfg.Provider.Model,
			timeout:  memCfg.ExtractTimeout,
		}
	}
	sb, err := memory.NewSynapseBackend(path, opts)
	if err != nil {
		observability.LogWarn(context.Background(), "图记忆库打不开，长期记忆关闭",
			map[string]any{"path": path, "err": err.Error()})
		return nil, false
	}
	if app != nil {
		app.synapse = memory.NewSynapseGraph(sb)
	}
	observability.LogInfo(context.Background(), "长期记忆使用图记忆后端（Synapse）",
		map[string]any{"path": path, "user_id": memCfg.UserID})
	migrateSynapseLegacy(app, cfg, memCfg, sb)
	return sb, true
}

// synapseMigrationTimeout 是旧数据迁移的整体时限。迁移是首次启用的顺带动作，
// 不能拖住启动；超时就留到下次启动重试（迁移本身幂等）。
const synapseMigrationTimeout = 30 * time.Second

// migrateSynapseLegacy 首次启用图记忆时把旧的扁平记忆导入新图（文档 4.10）。
//
// 幂等由两道闸共同保证：
//   - 库上的 PRAGMA user_version 标记（迁移成功后才置位），因此清空记忆之后
//     重启不会把旧数据又导回来；
//   - 迁移本身按内容哈希去重，中途失败下次重跑不会产生重复节点。
func migrateSynapseLegacy(app *App, cfg *config.Config, memCfg memory.Config, sb *memory.SynapseBackend) {
	ctx, cancel := context.WithTimeout(context.Background(), synapseMigrationTimeout)
	defer cancel()

	done, err := sb.MigrationDone(ctx)
	if err != nil {
		observability.LogWarn(ctx, "读取图记忆迁移标记失败，本次跳过旧数据迁移",
			map[string]any{"err": err.Error()})
		return
	}
	if done {
		return
	}

	// 旧数据就在同一个库文件的 memories 表里（embedded 后端的既有 schema）。
	// 打开失败或表为空都只是"没有旧数据"，不是错误。
	path := filepath.Join(cfg.Paths.DataDir, "memory.db")
	old, err := memory.NewEmbeddedBackend(path, memCfg)
	if err != nil {
		observability.LogWarn(ctx, "旧进程内记忆库打不开，跳过旧数据迁移",
			map[string]any{"path": path, "err": err.Error()})
		return
	}
	defer func() { _ = old.Close() }()

	var knowledgeDB *sql.DB
	if app != nil && app.db != nil {
		// knowledge_entries 在引擎主库里，通过只读连接池读取。
		knowledgeDB = app.db.ReadDB()
	}
	res, err := sb.MigrateAll(ctx, old, knowledgeDB)
	if err != nil {
		observability.LogWarn(ctx, "旧记忆迁移失败，下次启动会重试",
			map[string]any{"err": err.Error()})
		return
	}
	if err := sb.MarkMigrationDone(ctx); err != nil {
		observability.LogWarn(ctx, "迁移完成但标记写入失败，下次启动会重跑（幂等）",
			map[string]any{"err": err.Error()})
		return
	}
	observability.LogInfo(ctx, "旧记忆已迁移到图记忆后端", map[string]any{
		"source_memories":  res.SourceMemories,
		"source_knowledge": res.SourceKnowledge,
		"nodes_created":    res.NodesCreated,
		"nodes_existing":   res.NodesExisting,
		"edges_created":    res.EdgesCreated,
		"knowledge_skipped": res.KnowledgeSkipped,
	})
}

// synapseOptionsFrom 把长期记忆配置映射成 Synapse 构造选项。
//
// 零值语义：数值与时长字段的零值表示「用 internal/memory 的默认值」，
// SynapseOptions.withDefaults 会补齐，所以这里只在配置显式给了正值时才覆盖。
func synapseOptionsFrom(memCfg memory.Config) memory.SynapseOptions {
	opts := memory.SynapseOptions{
		UserID:             memCfg.UserID,
		AgentID:            memCfg.AgentID,
		TopK:               memCfg.TopK,
		RecallMaxChars:     memCfg.RecallMaxChars,
		ExtractTimeout:     memCfg.ExtractTimeout,
		ConsolidateTimeout: 0, // 用默认 20s
		QueueDepth:         memCfg.QueueDepth,
	}
	return opts
}

// MemoryGraph 返回记忆图的读写面（types.MemoryGraphPort）。
//
// 只有启用图记忆后端（memory.backend = "synapse"）时才非 nil；其它后端返回 nil，
// ipcapi 据此让每个 memory 帧返回明确错误，而不是"成功但什么都没做"。
func (a *App) MemoryGraph() types.MemoryGraphPort {
	if a == nil || a.synapse == nil {
		return nil
	}
	return a.synapse
}

// memoryGate 把工具层的幂等存储适配成 memory.Gate。
//
// 复用 tool_idempotency 表（0002 迁移已建）而不是新增 schema：这张表的 Claim 语义
// 正是「同一个 key 不能产生两个 durable side effect」，而「同一个 run 只回填一次」
// 是它的一个直接实例。新增一张表则要动 schema 归属规则（见 docs/接口裁决记录.md）。
type memoryGate struct{ store *tool.Store }

func (g memoryGate) Claim(ctx context.Context, key string) (bool, error) {
	if g.store == nil {
		// 没有幂等存储时放行：宁可重复抽取一次，也不要因为装配缺失而永远不写记忆。
		return true, nil
	}
	return g.store.Claim(ctx, key)
}

// memoryAdapter 把 internal/memory.Service 适配成 engine 侧的 MemoryPort。
//
// 引擎不 import internal/memory——端口声明在消费方（见 internal/ports 的约定），
// 类型转换放在装配层，两边可以各自演化而不互相牵制。
type memoryAdapter struct{ svc *memory.Service }

// 编译期断言：适配器必须满足引擎的结构化召回能力，否则 memory.recalled 事件
// 永远不会出现（而这是「Work Log 显示回忆了几条」的唯一来源）。
var _ engine.MemoryRecallReporter = memoryAdapter{}

func (a memoryAdapter) Recall(ctx context.Context, query string) string {
	if a.svc == nil {
		return ""
	}
	return a.svc.Recall(ctx, query)
}

// RecallDetailed 实现 engine.MemoryRecallReporter（可选能力）：除了注入文本，
// 还给出本次召回的条目，供引擎发出 memory.recalled 事件。
//
// 它只是转发到 memory.Service：记忆块怎么渲染由后端自己决定（Synapse 用带
// ↳ 关联 路径行的 RenderBlock，mem0/embedded 用通用 Block），装配层不重复
// 实现渲染逻辑。
func (a memoryAdapter) RecallDetailed(ctx context.Context, query string) (string, []engine.MemoryRecallItem) {
	if a.svc == nil {
		return "", nil
	}
	text, items := a.svc.RecallDetailed(ctx, query)
	if len(items) == 0 {
		return text, nil
	}
	out := make([]engine.MemoryRecallItem, 0, len(items))
	for _, it := range items {
		out = append(out, engine.MemoryRecallItem{ID: it.ID, Text: it.Text, Via: it.Via})
	}
	return text, out
}

func (a memoryAdapter) Remember(turn engine.MemoryTurn) {
	if a.svc == nil {
		return
	}
	a.svc.Remember(memory.Turn{
		RunID:     turn.RunID,
		SessionID: turn.SessionID,
		Prompt:    turn.Prompt,
		Answer:    turn.Answer,
	})
}

// memoryConfigFrom 把配置段的写法映射成运行期配置。
//
// 零值语义在这里落地：数值与时长字段的零值表示「用 internal/memory 的默认值」；
// WriteBack 是三态（nil = 未写，用默认开启），所以不能直接赋值——详见
// config.MemoryConfig 的注释，那里解释了为什么它必须是指针。
func memoryConfigFrom(mc config.MemoryConfig) memory.Config {
	out := memory.DefaultConfig()
	out.Enabled = mc.Enabled
	out.Backend = mc.Backend
	out.Endpoint = memory.NormalizeEndpoint(mc.Endpoint)
	out.SecretRef = mc.SecretRef
	if mc.UserID != "" {
		out.UserID = mc.UserID
	}
	if mc.AgentID != "" {
		out.AgentID = mc.AgentID
	}
	if mc.TopK > 0 {
		out.TopK = mc.TopK
	}
	if mc.RecallMaxChars > 0 {
		out.RecallMaxChars = mc.RecallMaxChars
	}
	if mc.WriteBack != nil {
		out.WriteBack = *mc.WriteBack
	}
	if mc.Timeout > 0 {
		out.Timeout = mc.Timeout
	}
	if mc.ExtractTimeout > 0 {
		out.ExtractTimeout = mc.ExtractTimeout
	}
	if mc.QueueDepth > 0 {
		out.QueueDepth = mc.QueueDepth
	}
	if mc.MaxInflight > 0 {
		out.MaxInflight = mc.MaxInflight
	}
	return out
}

// memoryKeyResolver 返回 mem0 API Key 的解析函数；未配置密钥时返回 nil（不发鉴权头）。
//
// 与 provider 的密钥路径完全一致：配置里只存 ref，明文只存在操作系统凭据库。
// 读不到时返回**明确错误**而不是空串——带着空鉴权头请求得到的 401，无法区分
// 「密钥没配」和「密钥配错了」，那是最难排查的一类问题。
func memoryKeyResolver(app *App, cfg *config.Config) func(context.Context) (string, error) {
	if cfg.Memory.SecretRef == "" {
		return nil
	}
	ref := cfg.Memory.SecretRef
	return func(context.Context) (string, error) {
		if app != nil && app.secrets != nil {
			return app.secrets.Get(ref)
		}
		return "", fmt.Errorf("bootstrap: 没有可用的密钥后端，无法解析 %q", ref)
	}
}
