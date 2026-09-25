// budget.go —— Token 预算管理（架构文档第 22.1 章）。
//
// 基准比例（七段模型）：
//
//	system 5% / tools 15% / recent conversation 25% / working memory 15%
//	/ retrieved knowledge 10% / agent reasoning 10% / output reserve 20%
//
// 实际比例按 Provider 的 context window 动态计算 —— 小窗口下百分比取整会
// 让某些段归零，因此本实现保证每段至少有 MinSegmentTokens，并在总量超限时
// 按「可压缩程度」回收到 output reserve 之外。
package ctxmgr

import (
	"fmt"
	"math"
)

// BudgetRatios 七段预算的基准比例（合计 1.0）。
type BudgetRatios struct {
	System     float64 // 0.05
	Tools      float64 // 0.15
	RecentConv float64 // 0.25
	WorkingMem float64 // 0.15
	Knowledge  float64 // 0.10
	Reasoning  float64 // 0.10
	Output     float64 // 0.20
}

// DefaultBudgetRatios 第 22.1 章的基准比例。
func DefaultBudgetRatios() BudgetRatios {
	return BudgetRatios{
		System:     0.05,
		Tools:      0.15,
		RecentConv: 0.25,
		WorkingMem: 0.15,
		Knowledge:  0.10,
		Reasoning:  0.10,
		Output:     0.20,
	}
}

// Sum 返回比例合计（用于校验配置是否归一）。
func (r BudgetRatios) Sum() float64 {
	return r.System + r.Tools + r.RecentConv + r.WorkingMem + r.Knowledge + r.Reasoning + r.Output
}

// TokenBudget 单个 Provider 窗口下的分段预算（单位：token）。
type TokenBudget struct {
	// ContextWindow 该 Provider 的原始窗口大小。
	ContextWindow int `json:"context_window"`
	// Usable 扣除安全边际后的可用窗口。
	Usable int `json:"usable"`
	// SafetyMargin 安全边际（token）—— 应对分词差异与协议开销。
	SafetyMargin int `json:"safety_margin"`

	System     int `json:"system"`
	Tools      int `json:"tools"`
	RecentConv int `json:"recent_conversation"`
	WorkingMem int `json:"working_memory"`
	Knowledge  int `json:"retrieved_knowledge"`
	Reasoning  int `json:"agent_reasoning"`
	Output     int `json:"output_reserve"`

	// Ratios 实际生效的比例（可能与基准不同：小窗口下会被下限修正）。
	Ratios BudgetRatios `json:"ratios"`
	// Versions 生成该预算的版本集合。
	Versions Versions `json:"versions"`
}

// InputBudget 返回除 output reserve 外的输入侧预算。
func (b TokenBudget) InputBudget() int {
	return b.System + b.Tools + b.RecentConv + b.WorkingMem + b.Knowledge + b.Reasoning
}

// CompactionTrigger 返回触发 compact 的 token 阈值（默认 80% 输入预算）。
func (b TokenBudget) CompactionTrigger(ratio float64) int {
	if ratio <= 0 {
		ratio = 0.8
	}
	return int(float64(b.InputBudget()) * ratio)
}

// BudgetOptions 预算计算参数。
type BudgetOptions struct {
	// SafetyMarginRatio 安全边际比例（默认 0.05）。
	SafetyMarginRatio float64
	// MinSegmentTokens 每段最小 token 数（默认 512）—— 防止小窗口下某段归零。
	MinSegmentTokens int
	// Ratios 自定义比例；零值用 DefaultBudgetRatios。
	Ratios BudgetRatios
}

// DefaultBudgetOptions 默认预算参数。
func DefaultBudgetOptions() BudgetOptions {
	return BudgetOptions{
		SafetyMarginRatio: 0.05,
		MinSegmentTokens:  512,
		Ratios:            DefaultBudgetRatios(),
	}
}

// ComputeBudget 按 Provider 窗口动态计算分段预算。
//
// contextWindow<=0 视为禁用预算（返回零值预算，调用方据此跳过压缩）。
func ComputeBudget(contextWindow int, opts BudgetOptions) TokenBudget {
	if opts.Ratios.Sum() == 0 {
		opts.Ratios = DefaultBudgetRatios()
	}
	if opts.SafetyMarginRatio <= 0 {
		opts.SafetyMarginRatio = 0.05
	}
	if opts.MinSegmentTokens <= 0 {
		opts.MinSegmentTokens = 512
	}

	if contextWindow <= 0 {
		return TokenBudget{Versions: CurrentVersions(), Ratios: opts.Ratios}
	}

	margin := int(math.Ceil(float64(contextWindow) * opts.SafetyMarginRatio))
	usable := contextWindow - margin
	if usable < 1 {
		usable = contextWindow
		margin = 0
	}

	r := opts.Ratios
	b := TokenBudget{
		ContextWindow: contextWindow,
		Usable:        usable,
		SafetyMargin:  margin,
		System:        scale(usable, r.System, opts.MinSegmentTokens),
		Tools:         scale(usable, r.Tools, opts.MinSegmentTokens),
		RecentConv:    scale(usable, r.RecentConv, opts.MinSegmentTokens),
		WorkingMem:    scale(usable, r.WorkingMem, opts.MinSegmentTokens),
		Knowledge:     scale(usable, r.Knowledge, opts.MinSegmentTokens),
		Reasoning:     scale(usable, r.Reasoning, opts.MinSegmentTokens),
		Output:        scale(usable, r.Output, opts.MinSegmentTokens),
		Ratios:        r,
		Versions:      CurrentVersions(),
	}

	// 超过上下文窗口时把 output reserve 之外的部分按比例回收。
	if total := b.InputBudget() + b.Output; total > contextWindow {
		b = b.shrinkTo(contextWindow)
	}
	return b
}

// scale 按比例换算 token 数并施加最小值。
func scale(usable int, ratio float64, minTokens int) int {
	v := int(math.Floor(float64(usable) * ratio))
	if v < minTokens {
		v = minTokens
	}
	return v
}

// shrinkTo 把各段按比例压缩到不超过 limit（不动 output reserve，它是最后防线）。
//
// 回收顺序体现优先级：先压 reasoning / working memory / knowledge（可重建或可丢弃），
// 最后才动 recent conversation（对话连续性最关键）。
func (b TokenBudget) shrinkTo(limit int) TokenBudget {
	outputKeep := b.Output
	avail := limit - outputKeep
	if avail < 1 {
		// 窗口小到连输出预留都放不下：输出预留降到 1/4，其余全给输入。
		outputKeep = limit / 4
		avail = limit - outputKeep
	}
	if avail < 1 {
		avail = 1
	}

	in := b.InputBudget()
	if in <= 0 {
		return b
	}
	ratio := float64(avail) / float64(in)

	b.Reasoning = int(float64(b.Reasoning) * ratio)
	b.WorkingMem = int(float64(b.WorkingMem) * ratio)
	b.Knowledge = int(float64(b.Knowledge) * ratio)
	b.Tools = int(float64(b.Tools) * ratio)
	b.System = int(float64(b.System) * ratio)
	b.RecentConv = int(float64(b.RecentConv) * ratio)
	b.Output = outputKeep

	// 修正取整误差，把差额补到 recent conversation。
	diff := avail - b.InputBudget()
	if diff != 0 {
		b.RecentConv += diff
		if b.RecentConv < 0 {
			b.RecentConv = 0
		}
	}
	return b
}

// Describe 生成人类可读的预算摘要（供日志与状态行）。
func (b TokenBudget) Describe() string {
	return fmt.Sprintf(
		"window=%d usable=%d margin=%d | system=%d tools=%d recent=%d workmem=%d knowledge=%d reasoning=%d output=%d",
		b.ContextWindow, b.Usable, b.SafetyMargin,
		b.System, b.Tools, b.RecentConv, b.WorkingMem, b.Knowledge, b.Reasoning, b.Output,
	)
}

// Validate 校验预算自洽性（分段合计不超过窗口）。
func (b TokenBudget) Validate() error {
	if b.ContextWindow <= 0 {
		return nil // 预算禁用
	}
	total := b.InputBudget() + b.Output
	if total > b.ContextWindow {
		return fmt.Errorf("ctxmgr: 预算超窗口: 分段合计 %d > window %d", total, b.ContextWindow)
	}
	return b.Versions.Validate()
}
