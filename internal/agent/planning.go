package agent

import (
	"context"
	"regexp"
	"strings"

	"github.com/ximo888ok-netizen/ximo-agent/internal/ports"
	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// Plan is the output of the planning phase: a narrowed tool set plus the
// model's own statement of the task. It corresponds to v1's planning-phase.ts,
// which runs once before the main loop to cut the tool catalogue down to the
// tools the task actually needs.
type Plan struct {
	// Understanding is the model's one-or-two sentence restatement of the task.
	Understanding string
	// Steps are the execution steps the model outlined.
	Steps []string
	// SelectedTools are the tool names the model declared it needs.
	SelectedTools []string
	// Tools is the filtered catalogue the main loop should use.
	Tools []types.ToolDefinition
	// Content is the raw planning text, appended to the conversation so the
	// main loop can see its own plan.
	Content string
	// Reasoning is the planning round's reasoning content, round-tripped when
	// thinking is enabled.
	Reasoning string
}

// planningTriggerMsgLen is the minimum user-prompt length that makes planning
// worthwhile. v1 uses >30 characters: shorter prompts are too small to plan.
const planningTriggerMsgLen = 30

// planningMinTools is the minimum catalogue size that makes planning
// worthwhile. v1 uses >5 tools, since filtering a tiny catalogue saves nothing
// and risks dropping a needed tool.
const planningMinTools = 5

// toolsBlockRe extracts the [TOOLS]…[/TOOLS] block from the planning output.
var toolsBlockRe = regexp.MustCompile(`(?s)\[TOOLS\]\s*(.*?)\s*\[/TOOLS\]`)

// stepsBlockRe extracts the [STEPS]…[/STEPS] block when the model emits one.
var stepsBlockRe = regexp.MustCompile(`(?s)\[STEPS\]\s*(.*?)\s*\[/STEPS\]`)

// ShouldPlan decides whether to run the planning phase. The criteria are the
// v1 ones, kept as a predicate so the decision is testable in isolation:
// planning enabled, more than five tools available, a user prompt longer than
// 30 characters, and the loop not already cancelled.
func ShouldPlan(cfg types.AgentConfig, conv *Conversation, ctx context.Context) bool {
	if !cfg.PlanningEnabled {
		return false
	}
	if conv == nil || len(conv.Tools) <= planningMinTools {
		return false
	}
	if len(conv.LastUser()) <= planningTriggerMsgLen {
		return false
	}
	if ctx != nil && ctx.Err() != nil {
		return false
	}
	return true
}

// PlanRequest is the input to the planner. It is separated from the loop so
// the planner can be tested without a loop or a scheduler.
type PlanRequest struct {
	Model        string
	SystemPrompt string
	Tools        []types.ToolDefinition
	Task         string
	Effort       types.ReasoningEffort
	MaxTokens    int
}

// Planner runs the planning round. The loop supplies an implementation backed
// by the provider port; tests supply a scripted one.
type Planner interface {
	Plan(ctx context.Context, req PlanRequest) (Plan, error)
}

// PlannerFunc adapts a function to Planner.
type PlannerFunc func(ctx context.Context, req PlanRequest) (Plan, error)

// Plan implements Planner.
func (f PlannerFunc) Plan(ctx context.Context, r PlanRequest) (Plan, error) { return f(ctx, r) }

// ProviderPlanner is the production planner: it asks the provider to pick the
// tools it needs, then filters the catalogue accordingly.
type ProviderPlanner struct {
	Provider ports.Provider
	// OnDelta, when set, receives the planning round's streamed text so the UI
	// can show the planning as it happens.
	OnDelta func(ports.Delta)
}

// toolCatalog renders the tool set as a compact list, which is what keeps the
// planning round cheap: v1 sends name plus a truncated description rather than
// the full JSON schemas.
func toolCatalog(tools []types.ToolDefinition) string {
	var b strings.Builder
	for _, t := range tools {
		desc := t.Description
		if i := strings.IndexByte(desc, '\n'); i >= 0 {
			desc = desc[:i]
		}
		if len(desc) > 100 {
			desc = desc[:100]
		}
		b.WriteString("- ")
		b.WriteString(t.Name)
		if desc != "" {
			b.WriteString(": ")
			b.WriteString(desc)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// PlanningPrompt is the instruction sent to the model for the planning round.
// The output format is fixed because ParsePlan depends on the two block
// markers.
func PlanningPrompt(req PlanRequest) string {
	var b strings.Builder
	b.WriteString("你是任务规划器。请分析下面的任务，然后从可用工具中挑选真正需要的工具。\n\n")
	b.WriteString("可用工具：\n")
	b.WriteString(toolCatalog(req.Tools))
	b.WriteString("\n任务：\n")
	b.WriteString(req.Task)
	b.WriteString("\n\n请严格按以下格式回答：\n")
	b.WriteString("1. 任务理解（1-2 句）\n")
	b.WriteString("2. 需要的工具：[TOOLS] 工具名1, 工具名2 [/TOOLS]\n")
	b.WriteString("3. 执行步骤：[STEPS] 步骤1; 步骤2 [/STEPS]\n")
	b.WriteString("\n只输出工具名，不要输出参数 schema。")
	return b.String()
}

// Plan implements Planner by calling the provider once and parsing the
// response.
func (p *ProviderPlanner) Plan(ctx context.Context, req PlanRequest) (Plan, error) {
	if p.Provider == nil {
		return Plan{}, types.NewError(types.CodeInternal, "planner has no provider")
	}
	msgs := make([]ports.Message, 0, 2)
	if req.SystemPrompt != "" {
		msgs = append(msgs, ports.Message{Role: ports.RoleSystem, Content: req.SystemPrompt})
	}
	msgs = append(msgs, ports.Message{Role: ports.RoleUser, Content: PlanningPrompt(req)})

	presp, err := p.Provider.Complete(ctx, ports.ProviderRequest{
		Model:     req.Model,
		Messages:  msgs,
		Effort:    req.Effort,
		MaxTokens: req.MaxTokens,
		OnDelta:   p.OnDelta,
	})
	if err != nil {
		return Plan{}, types.WrapError(types.CodeOf(err), err, "planning round failed")
	}
	if presp.FinishReason == ports.FinishCancelled {
		return Plan{}, types.NewError(types.CodeCancelled, "planning round cancelled")
	}
	if presp.FinishReason == ports.FinishError {
		return Plan{}, types.NewError(types.CodeProviderFailed, "planning round failed: %s", presp.Error)
	}
	return ParsePlan(presp.Content, presp.ReasoningContent, req.Tools)
}

// ParsePlan turns planning text into a Plan, filtering the catalogue to the
// declared tools.
//
// It returns an error when the response yields no usable tool selection. The
// caller treats that as "planning produced nothing" and keeps the full
// catalogue, which is the v1 behaviour: a failed plan must not shrink the tool
// set to empty.
func ParsePlan(content, reasoning string, tools []types.ToolDefinition) (Plan, error) {
	plan := Plan{Content: content, Reasoning: reasoning}

	m := toolsBlockRe.FindStringSubmatch(content)
	if m == nil {
		return plan, types.NewError(types.CodeInvalidArgument,
			"planning output has no [TOOLS] block")
	}
	names := splitToolNames(m[1])
	if len(names) == 0 {
		return plan, types.NewError(types.CodeInvalidArgument,
			"planning output declared no tools")
	}

	// Map declared names back onto the real definitions, case-insensitively.
	// A hallucinated name is dropped rather than invented.
	byLower := make(map[string]types.ToolDefinition, len(tools))
	for _, t := range tools {
		byLower[strings.ToLower(t.Name)] = t
	}
	seen := make(map[string]bool, len(names))
	selected := make([]types.ToolDefinition, 0, len(names))
	selectedNames := make([]string, 0, len(names))
	for _, n := range names {
		key := strings.ToLower(n)
		if seen[key] {
			continue
		}
		def, ok := byLower[key]
		if !ok {
			continue
		}
		seen[key] = true
		selected = append(selected, def)
		selectedNames = append(selectedNames, def.Name)
	}
	if len(selected) == 0 {
		return plan, types.NewError(types.CodeInvalidArgument,
			"planning output named no known tool")
	}
	// If the model asked for everything there is nothing to narrow, and v1
	// treats that as a failed plan so the full catalogue (with its original
	// ordering) is kept.
	if len(selected) >= len(tools) {
		return plan, types.NewError(types.CodeInvalidArgument,
			"planning selected every tool, which saves nothing")
	}

	plan.SelectedTools = selectedNames
	plan.Tools = selected

	if sm := stepsBlockRe.FindStringSubmatch(content); sm != nil {
		for _, s := range strings.FieldsFunc(sm[1], func(r rune) bool {
			return r == ';' || r == '；' || r == '\n'
		}) {
			s = strings.TrimSpace(s)
			if s != "" {
				plan.Steps = append(plan.Steps, s)
			}
		}
	}
	plan.Understanding = firstLine(content)
	return plan, nil
}

// SplitToolNames is the exported form of splitToolNames, used by tests.
func SplitToolNames(s string) []string { return splitToolNames(s) }

// splitToolNames splits a comma/whitespace separated tool list.
func splitToolNames(s string) []string {
	parts := strings.FieldsFunc(s, func(r rune) bool {
		switch r {
		case ',', '，', ' ', '\t', '\n', '、', ';', '；':
			return true
		}
		return false
	})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(strings.Trim(p, "`*-\"'"))
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// firstLine returns the first non-empty line, trimmed.
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return ""
}

// PlanAck is the system message appended after planning, which is what makes
// the model execute its own plan instead of re-planning every round.
const PlanAck = "以上是你的任务规划。现在按计划执行，只使用你选定的工具。不要重复规划，直接开始。"

// LongTaskContinuation is the user message injected when a long-task segment
// runs out of rounds. v1 uses a message of this shape to hand control back to
// the model for the next segment.
func LongTaskContinuation(segment, maxSegments, completedRounds int) string {
	return "[" + itoa(segment) + "/" + itoa(maxSegments) + "] 你已经完成了 " +
		itoa(completedRounds) + " 轮工具调用。请检查 todo_write 中仍未完成的任务并继续执行；" +
		"如果全部完成，请直接给出最终回答。"
}

// WrapUpPrompt is the instruction that forces a final answer once the round
// budget is exhausted. Tools are withheld for that round.
const WrapUpPrompt = "你已经完成了所有工具调用。请基于已有信息直接给出最终回答，不要再调用任何工具。"

// AllTodosDonePrompt is injected when todo_write reports every task complete.
const AllTodosDonePrompt = "所有任务已标记为完成。请基于已有工作成果，直接给出最终总结回复，无需再调用任何工具。"

// itoa is a tiny local integer formatter, avoiding a strconv import in a file
// that otherwise has none.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
