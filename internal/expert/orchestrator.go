// orchestrator.go —— 专家调度与两阶段编排协议（方案设计 → 有序实施）。
//
// 对应 v1 src/main/tools/Skill/AgentExpertTool.ts 的 activate 分支，但把
// 「两阶段编排」显式建模 —— v1 的编排逻辑藏在 prompt 文案里，v2 把它变成代码：
//
//	阶段 1 Plan：让专家先产出方案（步骤列表），不调用任何工具
//	阶段 2 Execute：按方案有序实施，每步工具调用都归因到方案步骤
//
// 这样做的收益：
//   - 实施阶段可校验「方案没说的工具调用」并记录偏差，而不是盲目执行
//   - 前端 ExpertWorkCard 能显示「当前在第几步」，而非一串无结构的工具调用
//   - 长时间任务可在阶段之间做 checkpoint（配合任务 03）
package expert

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ximo888ok-netizen/ximo-agent/internal/scheduler"
	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// 两阶段名称。
const (
	PhasePlan    = "plan"
	PhaseExecute = "execute"
)

// PhaseEvent 阶段跃迁事件（前端据此切换卡片状态）。
type PhaseEvent struct {
	ExpertID   string `json:"expert_id"`
	ExpertName string `json:"expert_name"`
	Phase      string `json:"phase"`
	// Plan 阶段：本次产出的方案文本。Execute 阶段为空。
	Plan string `json:"plan,omitempty"`
}

// ExpertRequest 一次专家调度请求（对应 v1 agent_expert 工具的 activate 参数）。
type ExpertRequest struct {
	// ExpertID 目标专家 ID。
	ExpertID string
	// Task 交给专家处理的子任务。为空则仅返回专家信息与提示词分析（v1 同款行为）。
	Task string
}

// ExpertOutcome 专家调度结果。
type ExpertOutcome struct {
	Expert   Expert         `json:"expert"`
	System   string         `json:"system_prompt"`
	Analysis ExpertAnalysis `json:"analysis"`
	// SubAgentMode 是否真的跑了子 Agent（有 task 且成功）。
	SubAgentMode bool `json:"sub_agent_mode"`
	// Content 最终回答；无 task 时是专家信息 + 分析结果 + 手动指引。
	Content string `json:"content"`
	// Events 工作过程事件（供前端可视化与持久化）。
	Events []WorkEvent `json:"events"`
	// Plan 阶段产出的方案（两阶段编排的第一个阶段）。
	Plan string `json:"plan,omitempty"`
	// PlanPhaseEvents 阶段跃迁事件。
	PlanPhaseEvents []PhaseEvent `json:"phase_events,omitempty"`
	// Deviation 实施阶段检测到的「方案未覆盖的工具调用」数量。
	Deviation int `json:"deviation,omitempty"`
	// Error 子 Agent 失败时的错误（此时 Content 降级为手动指引，不向上抛错）。
	Error string `json:"error,omitempty"`
}

// Orchestrator 专家调度器。
type Orchestrator struct {
	registry *Registry
	// runner 子 Agent 执行配置（不含 Task 与 ExpertID —— 每次调用时补）。
	runner SubAgentOptions
	// enableTwoPhase 是否启用两阶段编排（方案设计 → 有序实施）。
	enableTwoPhase bool
	// OnPhase 阶段跃迁回调。
	OnPhase func(PhaseEvent)
	// resources 是子代理的并发闸门（任务 05 的独立资源类）。
	//
	// 为什么必须接在 Activate 外面而不是让子代理自己抢：一次专家激活 = 一个
	// 子代理槽位，Plan 与 Execute 两个阶段共用它。 nil 时不限并发，保持
	// 未接资源池时的既有行为（含全部既有单测）。
	resources *scheduler.ResourcePool
	// allocator 返回某位专家的候选服务商 ID 顺序（设置面板的分配结果）。
	// nil 时用整个模型池的默认顺序。
	allocator func(Expert) []string
}

// OrchestratorOptions 调度器构造参数。
type OrchestratorOptions struct {
	Registry       *Registry
	Runner         SubAgentOptions
	EnableTwoPhase bool
	OnPhase        func(PhaseEvent)
	// Resources 子代理资源池；非 nil 时每次专家激活先申请一个
	// types.ResourceClassExpertAgent 槽位（容量默认 8），保证「同时在跑的
	// 子代理数」有硬上限，且与普通工具调用的槽位互相隔离。
	Resources *scheduler.ResourcePool
	// Allocator 见 Orchestrator.allocator。
	Allocator func(Expert) []string
}

// NewOrchestrator 构造调度器。
func NewOrchestrator(opts OrchestratorOptions) *Orchestrator {
	reg := opts.Registry
	if reg == nil {
		reg = NewRegistry(nil)
	}
	return &Orchestrator{
		registry:       reg,
		runner:         opts.Runner,
		enableTwoPhase: opts.EnableTwoPhase,
		OnPhase:        opts.OnPhase,
		resources:      opts.Resources,
		allocator:      opts.Allocator,
	}
}

// Registry 返回内部注册表。
func (o *Orchestrator) Registry() *Registry { return o.registry }

// ErrExpertNotFound 专家不存在。
var ErrExpertNotFound = errors.New("expert: 专家不存在")

// Activate 激活专家（v1 agent_expert activate 分支的 Go 对等实现）。
//
// 有 Task 时跑子 Agent；无 Task 时返回专家信息 + 提示词分析 + 推荐工具 + 预设工作流。
func (o *Orchestrator) Activate(ctx context.Context, req ExpertRequest) (*ExpertOutcome, error) {
	e, ok := o.registry.Get(req.ExpertID)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrExpertNotFound, req.ExpertID)
	}

	analysis := AnalyzeExpert(e)
	systemPrompt := BuildSystemPrompt(e)

	out := &ExpertOutcome{
		Expert:   e,
		System:   systemPrompt,
		Analysis: analysis,
	}

	// 无 task → 仅返回分析结果（v1 同款）。
	if strings.TrimSpace(req.Task) == "" {
		out.Content = buildInfoContent(e, analysis, systemPrompt)
		return out, nil
	}

	// 有 task → 先占一个 expert_agent 槽位再真正跑子代理（任务 05）。
	//
	// 申请失败与子代理失败同等对待：降级为手动指引而不是向上抛错 —— 但
	// ctx 已取消时原样返回，不能把引擎的取消语义吞掉。
	lease, leaseErr := o.acquireExpertSlot(ctx)
	if leaseErr != nil {
		if ctx.Err() != nil {
			return nil, leaseErr
		}
		out.Error = leaseErr.Error()
		out.SubAgentMode = false
		out.Content = buildFallbackContent(e, analysis, systemPrompt, leaseErr)
		return out, nil
	}
	if lease != nil {
		defer lease.Release()
	}

	// 有 task → 两阶段编排。
	plan, err := o.planPhase(ctx, e, req.Task, out)
	if err != nil {
		// Plan 阶段失败不阻断：降级为单阶段执行（v1 本来就没有 Plan 阶段）。
		plan = ""
	}

	res, runErr := o.runPhase(ctx, e, req.Task, plan, analysis, out)
	if runErr != nil {
		out.Error = runErr.Error()
		out.SubAgentMode = false
		out.Content = buildFallbackContent(e, analysis, systemPrompt, runErr)
		return out, nil // 降级为手动指引，不向上抛错（v1 同款「失败也返回可操作信息」）
	}

	out.SubAgentMode = true
	out.Events = res.Events
	out.Plan = plan
	out.Content = fmt.Sprintf("**%s**（%s）的回复：\n\n%s", e.Name, e.Emoji, res.Content)
	return out, nil
}

// acquireExpertSlot 申请一个 expert_agent 资源（FIFO 排队，ctx 决定等多久）。
// resources 为 nil 时返回 (nil, nil)，调用方按「不限并发」处理。
func (o *Orchestrator) acquireExpertSlot(ctx context.Context) (*scheduler.Lease, error) {
	if o.resources == nil {
		return nil, nil
	}
	return o.resources.Acquire(ctx, types.ResourceClassExpertAgent)
}

// candidatesFor 返回某位专家的候选服务商 ID 顺序；未配置 allocator 时为 nil
// （= 使用整个模型池的默认顺序）。
func (o *Orchestrator) candidatesFor(e Expert) []string {
	if o.allocator == nil {
		return nil
	}
	return o.allocator(e)
}

// planPhase 阶段 1：让专家先产出方案（不调用工具）。
//
// 该阶段刻意不传 tools —— 强制模型只做规划，避免它直接开始动手。
// 规划失败本就降级为单阶段执行，因此这一步不参与失败转移，只挑第一个候选。
func (o *Orchestrator) planPhase(ctx context.Context, e Expert, task string, out *ExpertOutcome) (string, error) {
	if !o.enableTwoPhase || (o.runner.Provider == nil && o.runner.Pool == nil) {
		return "", nil
	}

	runner := o.runner
	runner.ToolNames = nil // 规划阶段不给工具
	runner.CandidateOrder = o.candidatesFor(e)
	runner.MaxFailovers = 1 // 规划是锦上添花，不值得为它轮询所有候选

	res, err := RunSubAgent(ctx, SubAgentRequest{
		SystemPrompt: e2ePlanInstruction(),
		Task:         task,
		Options:      runner,
	})
	if err != nil {
		return "", err
	}

	plan := strings.TrimSpace(res.Content)
	if plan == "" || res.Empty {
		return "", nil
	}

	ev := PhaseEvent{ExpertID: e.ID, ExpertName: e.Name, Phase: PhasePlan, Plan: plan}
	out.PlanPhaseEvents = append(out.PlanPhaseEvents, ev)
	if o.OnPhase != nil {
		o.OnPhase(ev)
	}
	return plan, nil
}

// runPhase 阶段 2：按方案有序实施。
func (o *Orchestrator) runPhase(
	ctx context.Context,
	e Expert,
	task, plan string,
	analysis ExpertAnalysis,
	out *ExpertOutcome,
) (*SubAgentResult, error) {
	runner := o.runner
	runner.ExpertID = e.ID
	runner.ExpertName = e.Name
	runner.ToolNames = analysis.Tools
	// 失败转移发生在实施阶段：同一个 task、同一个 expert，换模型重跑。
	runner.CandidateOrder = o.candidatesFor(e)

	// 事件收集：把子 Agent 事件记进 runner，由 RunSubAgent 汇总返回。
	var events []WorkEvent
	runner.EventSink = func(ev WorkEvent) { events = append(events, ev) }

	ev := PhaseEvent{ExpertID: e.ID, ExpertName: e.Name, Phase: PhaseExecute}
	out.PlanPhaseEvents = append(out.PlanPhaseEvents, ev)
	if o.OnPhase != nil {
		o.OnPhase(ev)
	}

	// 实施阶段的 system 提示词 = 专家人格 + 已确认的方案。
	systemPrompt := BuildSystemPrompt(e)
	if plan != "" {
		systemPrompt += "\n\n## 已确认的实施方案\n" + plan + "\n\n请严格按上述方案步骤有序实施。"
	}

	res, err := RunSubAgent(ctx, SubAgentRequest{
		SystemPrompt: systemPrompt,
		Task:         task,
		Options:      runner,
	})
	if err != nil {
		return res, err
	}

	// 偏差检测：统计方案里没提到的工具调用。
	if plan != "" {
		out.Deviation = countDeviations(plan, res.Events)
	}
	return res, nil
}

// buildInfoContent 构造「无 task」时的信息型返回（与 v1 文案结构一致）。
func buildInfoContent(e Expert, analysis ExpertAnalysis, systemPrompt string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## 已激活专家 %s **%s**（`%s`）\n\n", e.Emoji, e.Name, e.ID)
	fmt.Fprintf(&b, "**简介**：%s\n\n---\n\n", e.Description)
	b.WriteString("### 📋 提示词分析结果\n\n")
	fmt.Fprintf(&b, "**人格设定**：%s\n\n", e.Personality)
	fmt.Fprintf(&b, "**工作风格**：%s\n\n---\n\n", e.Vibe)
	fmt.Fprintf(&b, "### 🔧 推荐工具配置（%d 个）\n\n", len(analysis.Tools))
	for _, t := range analysis.Tools {
		fmt.Fprintf(&b, "- `%s`\n", t)
	}
	fmt.Fprintf(&b, "\n---\n\n### 🔄 %s\n\n---\n\n", analysis.Workflow)
	fmt.Fprintf(&b, "### 📝 系统提示词\n\n%s\n\n---\n\n", systemPrompt)
	fmt.Fprintf(&b, "> 主 Agent 可基于以上分析，使用 agent_expert(action=\"activate\", expert_id=\"%s\", task=\"具体任务描述\") 让该专家带工具独立处理子任务。", e.ID)
	return b.String()
}

// buildFallbackContent 子 Agent 失败时的降级返回（v1 同款：仍给出可操作指引）。
func buildFallbackContent(e Expert, analysis ExpertAnalysis, systemPrompt string, cause error) string {
	var b strings.Builder
	fmt.Fprintf(&b, "子 Agent 调用失败：%v\n\n---\n\n", cause)
	fmt.Fprintf(&b, "## 专家信息\n%s **%s**（`%s`）— %s\n\n", e.Emoji, e.Name, e.ID, e.Description)
	b.WriteString("## 推荐工具配置\n")
	for _, t := range analysis.Tools {
		fmt.Fprintf(&b, "- `%s`\n", t)
	}
	fmt.Fprintf(&b, "\n## %s\n\n## 系统提示词\n%s\n\n", analysis.Workflow, systemPrompt)
	b.WriteString("请主 Agent 自行以该专家视角，使用推荐工具处理任务。")
	return b.String()
}

// e2ePlanInstruction 规划阶段指令。
func e2ePlanInstruction() string {
	return `请先只做规划，不要调用任何工具。
根据用户任务，输出一份有序的实施方案，格式如下：

## 方案
1. <步骤一：要做什么，用哪个工具，为什么>
2. <步骤二：...>
...

要求：
- 步骤必须具体可执行，明确使用哪个工具
- 标出哪些步骤有依赖关系（后一步依赖前一步的输出）
- 不要执行，只输出方案`
}

// countDeviations 统计实施阶段中「方案未提及的工具」的调用次数。
//
// 这是两阶段协议的价值所在：方案偏离会被量化，而不是无声地发生。
func countDeviations(plan string, events []WorkEvent) int {
	planLower := strings.ToLower(plan)
	deviation := 0
	for _, ev := range events {
		if ev.Stage != StageTool {
			continue
		}
		// Detail 形如 "调用工具 file_read"。
		name := strings.TrimPrefix(ev.Detail, "调用工具 ")
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if !strings.Contains(planLower, strings.ToLower(name)) {
			deviation++
		}
	}
	return deviation
}

// List 列出专家（可选按部门筛选），返回格式化文本（v1 list 分支对等）。
func (o *Orchestrator) List(division string) (string, error) {
	names, groups, err := o.registry.GroupByDivision()
	if err != nil {
		return "", err
	}
	total, err := o.registry.Count()
	if err != nil {
		return "", err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "## AI 专家库（共 %d 位专家）\n\n", total)
	for _, div := range names {
		if division != "" && div != division {
			continue
		}
		list := groups[div]
		fmt.Fprintf(&b, "### %s（%d 位）\n", div, len(list))
		for _, a := range list {
			desc := []rune(a.Description)
			snippet := string(desc)
			if len(desc) > 80 {
				snippet = string(desc[:80]) + "..."
			}
			fmt.Fprintf(&b, "- %s **%s**（`%s`）：%s\n", a.Emoji, a.Name, a.ID, snippet)
		}
		b.WriteString("\n")
	}
	return b.String(), nil
}

// Search 搜索专家并返回格式化文本（v1 search 分支对等）。
func (o *Orchestrator) Search(query string) (string, error) {
	results, err := o.registry.Search(query)
	if err != nil {
		return "", err
	}
	if len(results) == 0 {
		return fmt.Sprintf("未找到与「%s」匹配的专家", query), nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "## 搜索「%s」— 找到 %d 位专家\n\n", query, len(results))
	for _, a := range results {
		desc := []rune(a.Description)
		snippet := string(desc)
		if len(desc) > 100 {
			snippet = string(desc[:100])
		}
		fmt.Fprintf(&b, "- %s **%s**（`%s`，%s）：%s\n", a.Emoji, a.Name, a.ID, a.Division, snippet)
	}
	return b.String(), nil
}

// ToolDefinitionName 返回 agent_expert 工具名（供任务 04 注册时引用）。
const ToolDefinitionName = "agent_expert"
