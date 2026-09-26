package bootstrap

import (
	"context"
	"fmt"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/config"
	"github.com/ximo888ok-netizen/ximo-agent/internal/engine"
	"github.com/ximo888ok-netizen/ximo-agent/internal/memory"
	"github.com/ximo888ok-netizen/ximo-agent/internal/observability"
	"github.com/ximo888ok-netizen/ximo-agent/internal/storage/sqlite"
	"github.com/ximo888ok-netizen/ximo-agent/internal/tool"
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

	client := memory.NewClient(memCfg, memory.ClientOptions{
		APIKey: memoryKeyResolver(app, cfg),
	})
	svc := memory.NewService(memCfg, client, memoryGate{store: tool.NewIdempotencyStore(
		tool.NewSQLIdempotencyDB(db.ReadDB()),
		toolCallResolver{},
		nil, // nil = 使用 DefaultClassPolicy，与工具路径保持一致
		memoryExtractionTTL,
	)})
	return svc
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

func (a memoryAdapter) Recall(ctx context.Context, query string) string {
	if a.svc == nil {
		return ""
	}
	return a.svc.Recall(ctx, query)
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
