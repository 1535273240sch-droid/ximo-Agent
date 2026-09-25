// compress.go —— 机械上下文压缩（第 22.2 章；v1 context-compress.ts + context-manager.ts 的合并升级）。
//
// 三级机械压缩 + 一级 LLM 压缩信号：
//
//	SNIP    （软阈值 60%）旧 tool 结果截断为摘要 + 前 N 字符 — 仅裁 LOW 优先级
//	PRUNE   （硬阈值 80%）进一步缩短为最小占位符 — 裁 LOW + MEDIUM，HIGH 用 snippedKeep
//	ASSISTANT 截断（极阈值 100%）截断旧 assistant 内容（保留 tool_calls 结构）
//	COMPACT/FORCE（≥ trigger）不做机械裁剪，返回信号由 agent-loop 调用 LLM 生成摘要
//
// 压缩直接修改传入的消息副本（调用方负责 CloneMessages）。
//
// 与 v1 的分工整合：v1 把「按字符数的预防性压缩」(context-compress.ts) 与
// 「按实际 token 数的动态压缩」(context-manager.ts) 拆成两个文件、两套阈值。
// v2 统一到一个压缩器，阈值以 token 为准，字符数仅作快速预筛。
package ctxmgr

import (
	"strings"
)

// AgentConfig 压缩相关配置（v1 agentConfig 的对应子集）。
type AgentConfig struct {
	// MaxToolResultChars 单条工具结果的字符上限（超出即截断）。
	MaxToolResultChars int
	// MaxContextChars 上下文总字符硬上限（字符级兜底）。
	MaxContextChars int
	// RecentKeep 最近 N 条消息受保护，不参与裁剪。
	RecentKeep int
	// SnippedKeep SNIP 阶段保留的前缀字符数。
	SnippedKeep int
	// PrunedKeep PRUNE 阶段保留的前缀字符数。
	PrunedKeep int
	// CompactionRatio 压缩触发比例（promptTokens / 可用预算）。
	CompactionRatio float64
}

// DefaultAgentConfig 与 v1 DEFAULT_SETTINGS 对齐的默认值。
func DefaultAgentConfig() AgentConfig {
	return AgentConfig{
		MaxToolResultChars: 8000,
		MaxContextChars:    300_000,
		RecentKeep:         5,
		SnippedKeep:        200,
		PrunedKeep:         80,
		CompactionRatio:    0.8,
	}
}

// 压缩档位比例因子（v1 context-manager.ts 的 SOFT/SNIP/FORCE_FACTOR）。
//
// 相对 compactionRatio 换算，因此用户调 ratio 时各档位等比移动：
//
//	soft  = ratio * 0.625  （默认 0.8 → 0.50）
//	snip  = ratio * 0.75   （默认 0.8 → 0.60）
//	force = ratio * 1.125  （默认 0.8 → 0.90）
const (
	softFactor  = 0.625
	snipFactor  = 0.75
	forceFactor = 1.125
)

// Tier 压缩档位。
type Tier string

const (
	TierNone    Tier = "none"
	TierSoft    Tier = "soft"
	TierSnip    Tier = "snip"
	TierCompact Tier = "compact"
	TierForce   Tier = "force"
)

// CompactionStats 一次压缩决策/执行的结果。
type CompactionStats struct {
	Tier           Tier `json:"tier"`
	SnippedResults int  `json:"snipped_results"`
	PrunedResults  int  `json:"pruned_results"`
	SavedChars     int  `json:"saved_chars"`
	StuckPaused    bool `json:"stuck_paused"`
	// NeedLLMSummary 为 true 表示 compact/force 档位需要 agent-loop 调用 LLM 摘要。
	NeedLLMSummary bool `json:"need_llm_summary"`
}

// 截断标记（与 v1 文案一致，用于避免双重截断）。
const (
	markerSnipped = "\n[...已自动截断以节省上下文空间]"
	markerPruned  = "\n[...已省略]"
	markerTrunc   = "[...结果已截断，原始长度 %d 字符。如需完整内容请重新调用工具并指定更小范围]"
)

// TruncateToolResult 截断超长工具结果（v1 truncateToolResult 对等）。
func TruncateToolResult(content string, config AgentConfig) string {
	if config.MaxToolResultChars <= 0 || len(content) <= config.MaxToolResultChars {
		return content
	}
	truncated := content[:config.MaxToolResultChars]
	var b strings.Builder
	b.WriteString(truncated)
	b.WriteString("\n\n")
	b.WriteString(sprintfMarker(len(content)))
	return b.String()
}

// compressState 压缩器的持久状态（对应 v1 ContextManager 的实例字段）。
//
// 这些状态必须跨轮保留 —— stuck 保护依赖连续压缩次数。
type compressState struct {
	consecutiveCompacts int
	compactStuck        bool
	softNoticed         bool
	rewriteVersion      int
}

// Compressor 机械压缩执行器。
type Compressor struct {
	config AgentConfig
}

// NewCompressor 构造压缩器。
func NewCompressor(config AgentConfig) *Compressor {
	if config.RecentKeep <= 0 {
		config.RecentKeep = 5
	}
	if config.SnippedKeep <= 0 {
		config.SnippedKeep = 200
	}
	if config.PrunedKeep <= 0 {
		config.PrunedKeep = 80
	}
	return &Compressor{config: config}
}

// Config 返回压缩器配置。
func (c *Compressor) Config() AgentConfig { return c.config }

// TrimContext 三级机械压缩（v1 trimContext 的 token 阈值版）。
//
// 返回统计。仅修改传入切片中的 Content 字段，不增删消息 ——
// 保证 assistant(tool_calls) 与 tool(tool_call_id) 的配对结构不被破坏。
func (c *Compressor) TrimContext(messages []Message, budget BudgetOrChars) CompactionStats {
	stats := CompactionStats{Tier: TierNone}
	if len(messages) == 0 {
		return stats
	}

	snipThreshold := budget.threshold(snipFactor)
	pruneThreshold := budget.threshold(1.0)
	hardThreshold := budget.hardLimit()

	total := budget.measure(messages)
	if total <= snipThreshold {
		return stats
	}

	protectFrom := len(messages) - c.config.RecentKeep
	if protectFrom < 1 {
		protectFrom = 1
	}

	// ── 第一级 SNIP：仅裁 LOW 优先级工具结果 ──
	if total > snipThreshold {
		for i := 1; i < protectFrom; i++ {
			m := &messages[i]
			if m.Role != "tool" || m.Content == "" {
				continue
			}
			if len(m.Content) <= c.config.SnippedKeep+100 {
				continue
			}
			if alreadyTruncated(m.Content) {
				continue
			}
			if GetToolRetention(FindToolName(messages, i)) != RetentionLow {
				continue
			}
			stats.SavedChars += len(m.Content) - (c.config.SnippedKeep + len(markerSnipped))
			m.Content = m.Content[:c.config.SnippedKeep] + markerSnipped
			stats.SnippedResults++
		}
		if stats.SnippedResults > 0 {
			stats.Tier = TierSnip
		}
	}

	// ── 第二级 PRUNE：裁 LOW + MEDIUM，HIGH 用 snippedKeep 保留更多 ──
	if budget.measure(messages) > pruneThreshold {
		for i := 1; i < protectFrom; i++ {
			m := &messages[i]
			if m.Role != "tool" || m.Content == "" {
				continue
			}
			if len(m.Content) <= c.config.PrunedKeep {
				continue
			}
			if strings.Contains(m.Content, markerPruned) {
				continue
			}
			retention := GetToolRetention(FindToolName(messages, i))
			if retention == RetentionHigh {
				// HIGH：用 snippedKeep 保留更多（跳过已被 snip 截断的）。
				if len(m.Content) > c.config.SnippedKeep && !alreadyTruncated(m.Content) {
					stats.SavedChars += len(m.Content) - (c.config.SnippedKeep + len(markerSnipped))
					m.Content = m.Content[:c.config.SnippedKeep] + markerSnipped
					stats.PrunedResults++
				}
			} else {
				stats.SavedChars += len(m.Content) - (c.config.PrunedKeep + len(markerPruned))
				m.Content = m.Content[:c.config.PrunedKeep] + markerPruned
				stats.PrunedResults++
			}
		}
		if stats.PrunedResults > 0 {
			stats.Tier = TierSnip
		}
	}

	// ── 第三级：仍超硬阈值则截断旧 assistant 内容（保留 tool_calls） ──
	if budget.measure(messages) > hardThreshold {
		for i := 1; i < protectFrom; i++ {
			m := &messages[i]
			if m.Role == "assistant" && m.Content != "" && len(m.Content) > 500 && len(m.ToolCalls) == 0 {
				stats.SavedChars += len(m.Content) - 200
				m.Content = m.Content[:200] + markerPruned
			}
			if budget.measure(messages) <= hardThreshold {
				break
			}
		}
		if stats.Tier == TierNone {
			stats.Tier = TierSnip
		}
	}

	return stats
}

// SnipStaleToolResults 仅执行 SNIP 级裁剪（v1 snipStaleToolResults 对等）。
//
// 先裁 LOW，若无可裁再裁 MEDIUM；HIGH 在 snip 阶段永久保留。
func (c *Compressor) SnipStaleToolResults(messages []Message) CompactionStats {
	stats := CompactionStats{Tier: TierNone}
	protectFrom := len(messages) - c.config.RecentKeep
	if protectFrom < 1 {
		protectFrom = 1
	}

	for _, retention := range []ToolRetention{RetentionLow, RetentionMedium} {
		for i := 1; i < protectFrom; i++ {
			m := &messages[i]
			if m.Role != "tool" || m.Content == "" {
				continue
			}
			if len(m.Content) <= c.config.SnippedKeep+100 {
				continue
			}
			if alreadyTruncated(m.Content) {
				continue
			}
			if GetToolRetention(FindToolName(messages, i)) != retention {
				continue
			}
			stats.SavedChars += len(m.Content) - (c.config.SnippedKeep + len(markerSnipped))
			m.Content = m.Content[:c.config.SnippedKeep] + markerSnipped
			stats.SnippedResults++
		}
		// LOW 裁完即可 —— snip 是软阈值，保留更多上下文有利于后续轮次（v1 同款）。
		if stats.SnippedResults > 0 {
			break
		}
	}
	stats.Tier = TierSnip
	return stats
}

// BudgetOrChars 抽象「按 token 预算」与「按字符上限」两种阈值来源。
//
// 这样同一套压缩逻辑既能服务 token 感知的 ContextManager，
// 也能服务只有字符数的静态消息构造路径（保持 v1 的双入口能力）。
type BudgetOrChars struct {
	// Budget 为 nil 时退化为纯字符模式。
	Budget *TokenBudget
	// Tokenizer 用于测量；Budget 非 nil 时必须提供。
	Tokenizer Tokenizer
	// MaxChars 字符模式下的总上限。
	MaxChars int
}

// measure 返回当前度量值：
// token 模式下返回 promptTokens（用预算的 trigger 基准），字符模式返回字符数。
//
// 说明：token 模式下这里返回的是「相对于 trigger 的等效比例分子」，
// 因此返回 raw token 数，阈值也用 raw token 数。
func (b BudgetOrChars) measure(messages []Message) int {
	if b.Budget != nil && b.Tokenizer != nil {
		return b.Tokenizer.CountMessageTokens(messages)
	}
	return TotalChars(messages)
}

// threshold 返回 factor 对应的阈值。
//
// token 模式：base = 输入预算 * CompactionRatio，阈值 = base * factor
//
//	（softFactor 0.625 / 1.0 / forceFactor 1.125）
//
// 字符模式：base = MaxChars，阈值 = base * factor
func (b BudgetOrChars) threshold(factor float64) int {
	base := b.base()
	return int(float64(base) * factor)
}

// hardLimit 返回第三级（assistant 截断）的阈值。
func (b BudgetOrChars) hardLimit() int {
	return b.base()
}

func (b BudgetOrChars) base() int {
	if b.Budget != nil {
		in := b.Budget.InputBudget()
		if in <= 0 {
			return 0
		}
		ratio := 0.8
		return int(float64(in) * ratio)
	}
	return b.MaxChars
}

// sprintfMarker 构造截断提示（避免引入 fmt 到热路径的判断成本以外的开销）。
func sprintfMarker(origLen int) string {
	return "[" + "..." + "结果已截断，原始长度 " + itoa(origLen) + " 字符。如需完整内容请重新调用工具并指定更小范围]"
}

// itoa 轻量整数转字符串。
func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
