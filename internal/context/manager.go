// manager.go —— ContextManager：四档压缩决策 + stuck 保护 + 版本化产物（第 22 章）。
//
// 对应 v1 shared/cache/context-manager.ts 的 maybeCompact() + compactStuck 机制，
// 并补齐第 22.2 章的版本化要求。
//
// 四档（按 prompt 占预算比例，配合 compress.go 的因子）：
//
//	50% soft    — 仅通知，不动前缀（保持缓存命中）
//	60% snip    — 机械裁剪陈旧 tool results（保留配对，前缀大部分仍命中）
//	80% compact — 调用 LLM 生成摘要替换旧消息（由 agent-loop 执行）
//	90% force   — 强制 compact，跳过经济性检查
//
// stuck 保护：连续 ≥2 次 compact 后仍未降到 trigger 以下 → 暂停自动压缩，
// 让 prefix append-only 增长，命中率自然恢复。防止「压缩→仍超限→再压缩」的抖动循环。
//
// 版本化：Compact 产出的 CompactedContext 必带四个版本号，
// 使未来改算法后历史 Run 仍可解释复现。
package ctxmgr

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// SessionSnapshot 压缩输入：一个会话当前的消息与用量快照。
type SessionSnapshot struct {
	SessionID string
	Messages  []Message

	// PromptTokens 最近一次 API 调用的 promptTokens（来自 usage）。
	// 0 表示尚无用量数据 —— 此时按消息估算。
	PromptTokens int

	// SystemPrompt / Tools 用于 PrefixShape 诊断与预算核算。
	SystemPrompt string
	Tools        []ToolSchema
}

// CompactedContext 压缩产物（第 22.2 章要求带版本号）。
type CompactedContext struct {
	Messages []Message `json:"messages"`

	// Versions 四个版本号 —— 压缩结果必须版本化。
	Versions Versions `json:"versions"`

	// Stats 本次压缩的档位与效果。
	Stats CompactionStats `json:"stats"`

	// 压缩前后度量。
	TokensBefore int `json:"tokens_before"`
	TokensAfter  int `json:"tokens_after"`

	// RewriteVersion 历史重写版本（参与 PrefixShape）。
	RewriteVersion int `json:"rewrite_version"`

	// Shape 本次压缩后的前缀形状快照。
	Shape PrefixShape `json:"prefix_shape"`

	// StuckPaused 是否因 stuck 保护而暂停了自动压缩。
	StuckPaused bool `json:"stuck_paused"`
}

// ContextManager 实现任务书规定的契约接口。
//
// 并发安全：状态（consecutiveCompacts / compactStuck / rewriteVersion）由互斥锁保护，
// 因为同一 Manager 可能被多个 turn 并发调用。
type ContextManager struct {
	mu sync.Mutex

	compressor *Compressor
	tokenizer  Tokenizer

	budgetOpts BudgetOptions

	// providerWindow 当前 Provider 的上下文窗口（多 Provider 场景由 SetProviderWindow 更新）。
	providerWindow int

	state compressState

	// config 压缩配置。
	config AgentConfig

	// 上一轮的前缀形状，用于 CompareShape 诊断。
	lastShape    PrefixShape
	hasLastShape bool

	// lastStats 最近一次压缩统计（供状态行展示）。
	lastStats CompactionStats
}

// ContextManagerOptions 构造参数。
type ContextManagerOptions struct {
	Config     AgentConfig
	Tokenizer  Tokenizer
	BudgetOpts BudgetOptions
}

// NewContextManager 构造 ContextManager。
//
// tokenizer 为 nil 时使用默认分词器（懒加载 embed 词表）。
func NewContextManager(opts ContextManagerOptions) *ContextManager {
	cfg := opts.Config
	if cfg.RecentKeep == 0 {
		cfg = DefaultAgentConfig()
	}
	tk := opts.Tokenizer
	if tk == nil {
		tk = DefaultTokenizer()
	}
	bopts := opts.BudgetOpts
	if bopts.Ratios.Sum() == 0 {
		bopts = DefaultBudgetOptions()
	}

	return &ContextManager{
		compressor:     NewCompressor(cfg),
		tokenizer:      tk,
		budgetOpts:     bopts,
		config:         cfg,
		providerWindow: defaultProviderWindow,
	}
}

// Budget 实现契约接口：按 Provider 窗口计算 Token 预算。
//
// 这是第 22.1 章「实际比例按 Provider 的 context window 动态计算」的入口。
func (m *ContextManager) Budget(ctx context.Context, providerWindow int) TokenBudget {
	if err := ctx.Err(); err != nil {
		return TokenBudget{Versions: CurrentVersions()}
	}
	return ComputeBudget(providerWindow, m.budgetOpts)
}

// Compact 实现契约接口：按需压缩会话上下文。
//
// 决策完全基于 promptTokens 占预算的比例；返回值始终带四个版本号。
//
// 使用的 Provider 窗口来自 SetProviderWindow（默认 1M，与内置 DeepSeek 一致）；
// 多 Provider 场景应在切换服务商时调用 SetProviderWindow 更新。
func (m *ContextManager) Compact(ctx context.Context, snapshot SessionSnapshot) (CompactedContext, error) {
	return m.CompactWithWindow(ctx, snapshot, m.ProviderWindow())
}

// ProviderWindow 返回当前配置的 Provider 上下文窗口。
func (m *ContextManager) ProviderWindow() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.providerWindow <= 0 {
		return defaultProviderWindow
	}
	return m.providerWindow
}

// SetProviderWindow 更新 Provider 上下文窗口（切换服务商时调用）。
func (m *ContextManager) SetProviderWindow(window int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.providerWindow = window
}

// defaultProviderWindow 内置 DeepSeek 的默认窗口。
const defaultProviderWindow = 1_000_000

// CompactWithWindow 用显式 Provider 窗口执行压缩（多 Provider 场景）。
func (m *ContextManager) CompactWithWindow(ctx context.Context, snapshot SessionSnapshot, providerWindow int) (CompactedContext, error) {
	if err := ctx.Err(); err != nil {
		return CompactedContext{}, err
	}
	if providerWindow <= 0 {
		providerWindow = m.ProviderWindow()
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// 压缩作用在副本上 —— 不污染调用方的会话状态（v1 注释：压缩不重置会话聚合计数器）。
	messages := CloneMessages(snapshot.Messages)

	budget := ComputeBudget(providerWindow, m.budgetOpts)
	measurer := BudgetOrChars{Budget: &budget, Tokenizer: m.tokenizer, MaxChars: m.config.MaxContextChars}

	promptTokens := snapshot.PromptTokens
	if promptTokens <= 0 {
		promptTokens = m.tokenizer.CountMessageTokens(messages)
	}

	tokensBefore := m.tokenizer.CountMessageTokens(messages)

	stats := m.decide(promptTokens, budget, messages, measurer)

	tokensAfter := m.tokenizer.CountMessageTokens(messages)
	shape := CaptureShape(snapshot.SystemPrompt, snapshot.Tools, m.state.rewriteVersion, CompressionVersion)

	versions := CurrentVersions()
	if err := versions.Validate(); err != nil {
		return CompactedContext{}, err
	}

	m.lastShape = shape
	m.hasLastShape = true
	m.lastStats = stats

	return CompactedContext{
		Messages:       messages,
		Versions:       versions,
		Stats:          stats,
		TokensBefore:   tokensBefore,
		TokensAfter:    tokensAfter,
		RewriteVersion: m.state.rewriteVersion,
		Shape:          shape,
		StuckPaused:    m.state.compactStuck,
	}, nil
}

// decide 执行四档决策（v1 maybeCompact 的对等实现，含 stuck 保护）。
//
// 调用方必须已持锁。
func (m *ContextManager) decide(promptTokens int, budget TokenBudget, messages []Message, measurer BudgetOrChars) CompactionStats {
	empty := CompactionStats{Tier: TierNone, StuckPaused: m.state.compactStuck}

	if budget.ContextWindow <= 0 || promptTokens <= 0 {
		return empty
	}

	ratio := m.config.CompactionRatio
	if ratio <= 0 {
		ratio = 0.8
	}
	base := float64(budget.InputBudget()) * ratio

	soft := int(base * softFactor)
	snip := int(base * snipFactor)
	high := int(base)
	force := int(base * forceFactor)

	// ── 低于 trigger：清除 stuck 状态（v1 同款） ──
	if promptTokens < high {
		if promptTokens >= soft && promptTokens < snip && !m.state.softNoticed {
			m.state.softNoticed = true
			return CompactionStats{Tier: TierSoft, StuckPaused: m.state.compactStuck}
		}
		if promptTokens >= snip {
			// snip 区间：机械裁剪陈旧 tool results。
			stats := m.compressor.SnipStaleToolResults(messages)
			if stats.SnippedResults > 0 {
				m.state.rewriteVersion++
				stats.StuckPaused = m.state.compactStuck
				return stats
			}
			return empty
		}
		m.state.consecutiveCompacts = 0
		m.state.compactStuck = false
		return empty
	}

	// ── compact / force 区间 ──
	if m.state.compactStuck {
		// stuck 暂停中：不再自动压缩，让 prefix append-only 增长。
		return CompactionStats{Tier: TierNone, StuckPaused: true}
	}

	// 先做一次机械裁剪，能解决就不必上 LLM 摘要（经济性考量）。
	trimStats := m.compressor.TrimContext(messages, measurer)
	if trimStats.SnippedResults > 0 || trimStats.PrunedResults > 0 {
		m.state.rewriteVersion++
		// 裁剪后若已降到 trigger 以下，本轮无需 LLM 摘要。
		after := measurer.measure(messages)
		if after < high {
			trimStats.StuckPaused = m.state.compactStuck
			return trimStats
		}
	}

	isForce := promptTokens >= force
	m.state.rewriteVersion++
	m.state.consecutiveCompacts++
	if m.state.consecutiveCompacts >= 2 {
		m.state.compactStuck = true
	}

	return CompactionStats{
		Tier:           mergeTier(trimStats.Tier, tierFor(isForce)),
		SnippedResults: trimStats.SnippedResults,
		PrunedResults:  trimStats.PrunedResults,
		SavedChars:     trimStats.SavedChars,
		StuckPaused:    m.state.compactStuck,
		// 机械裁剪不足 → 需要 agent-loop 调用 LLM 生成摘要。
		NeedLLMSummary: true,
	}
}

// tierFor 返回 compact / force 档位。
func tierFor(isForce bool) Tier {
	if isForce {
		return TierForce
	}
	return TierCompact
}

// mergeTier 取两者中「更重」的档位，保证 compact/force 不被 none 覆盖。
func mergeTier(mechanical, decision Tier) Tier {
	if mechanical == TierSnip && decision != TierNone {
		return decision
	}
	if decision != TierNone {
		return decision
	}
	return mechanical
}

// Reset 重置状态（切换会话时调用）。
func (m *ContextManager) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state = compressState{}
	m.lastShape = PrefixShape{}
	m.hasLastShape = false
	m.lastStats = CompactionStats{}
}

// RewriteVersion 返回当前历史重写版本号。
func (m *ContextManager) RewriteVersion() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state.rewriteVersion
}

// StuckPaused 报告是否处于 stuck 暂停。
func (m *ContextManager) StuckPaused() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state.compactStuck
}

// LastStats 返回最近一次压缩统计。
func (m *ContextManager) LastStats() CompactionStats {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastStats
}

// Diagnose 对比本轮与上一轮前缀形状，生成 cache miss 归因。
//
// usage 为最近一次 API 调用的 cache 命中/未命中 token 数。
func (m *ContextManager) Diagnose(systemPrompt string, tools []ToolSchema, cacheHit, cacheMiss int) (CacheDiagnostics, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	cur := CaptureShape(systemPrompt, tools, m.state.rewriteVersion, CompressionVersion)
	if !m.hasLastShape {
		return CompareShape(PrefixShape{}, cur, cacheHit, cacheMiss), false
	}
	return CompareShape(m.lastShape, cur, cacheHit, cacheMiss), true
}

// TruncateToolResult 暴露单条工具结果截断（供工具运行时在写入前调用）。
func (m *ContextManager) TruncateToolResult(content string) string {
	return TruncateToolResult(content, m.config)
}

// Validate 校验压缩产物：版本号齐全、消息结构未破坏（tool_calls 配对完整）。
//
// 供落库前守门 —— 「压缩把 tool_calls 配对搞坏」是最严重的压缩缺陷，
// 会直接导致下一轮 API 调用 400。
func (c CompactedContext) Validate() error {
	if err := c.Versions.Validate(); err != nil {
		return err
	}
	if err := validateToolPairing(c.Messages); err != nil {
		return err
	}
	if c.TokensAfter > c.TokensBefore && c.Stats.Tier != TierNone && c.Stats.Tier != TierSoft {
		return fmt.Errorf("ctxmgr: 压缩后 token 反而增加（%d → %d），压缩逻辑异常",
			c.TokensBefore, c.TokensAfter)
	}
	return nil
}

// ErrToolPairingBroken 压缩破坏了 tool_calls 与 tool 响应的配对。
var ErrToolPairingBroken = errors.New("ctxmgr: 压缩破坏了 tool_calls/tool 配对")

// validateToolPairing 校验每条 tool 消息都有对应的 assistant tool_calls。
func validateToolPairing(messages []Message) error {
	pending := make(map[string]bool)
	for i, m := range messages {
		switch m.Role {
		case "assistant":
			ids, err := toolCallIDs(m.ToolCalls)
			if err != nil {
				return fmt.Errorf("ctxmgr: 第 %d 条 assistant 消息 tool_calls 解析失败: %w", i, err)
			}
			for _, id := range ids {
				pending[id] = true
			}
		case "tool":
			if m.ToolCallID == "" {
				return fmt.Errorf("%w: 第 %d 条 tool 消息缺少 tool_call_id", ErrToolPairingBroken, i)
			}
			if !pending[m.ToolCallID] {
				return fmt.Errorf("%w: 第 %d 条 tool 消息的 tool_call_id=%q 无对应 assistant 调用",
					ErrToolPairingBroken, i, m.ToolCallID)
			}
			delete(pending, m.ToolCallID)
		}
	}
	return nil
}
