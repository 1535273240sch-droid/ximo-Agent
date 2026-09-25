package agent

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/ximo888ok-netizen/ximo-agent/internal/ports"
	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// This file implements the user-visible plan mode (task 4): the model states
// what it intends to do, the run parks, and only the user's approval starts the
// real work.
//
// It is deliberately separate from planning.go. That file is the *internal*
// optimisation that narrows the tool catalogue and whose output the user never
// sees; this file is the *visible* contract. The two run at different times for
// different reasons and share no state, so runPlanning keeps its behaviour
// unchanged and this phase runs strictly after it.

// PlanDecision is the user's answer to a proposed plan.
//
// The zero value means "no decision is pending", which is what a first
// execution carries: the loop proposes a plan and parks.
type PlanDecision string

const (
	// PlanDecisionNone means no decision has been made yet.
	PlanDecisionNone PlanDecision = ""
	// PlanDecisionApproved means the plan was accepted. The loop appends it to
	// the conversation as the confirmed plan of record and starts executing.
	PlanDecisionApproved PlanDecision = "approved"
	// PlanDecisionRejected means the user asked for a different plan. The loop
	// proposes a new one and parks again.
	PlanDecisionRejected PlanDecision = "rejected"
)

// Valid reports whether d is a known decision.
func (d PlanDecision) Valid() bool {
	switch d {
	case PlanDecisionNone, PlanDecisionApproved, PlanDecisionRejected:
		return true
	default:
		return false
	}
}

// planModeWaitingReason is the message the UI shows while a run is parked on a
// plan. It travels with the waiting_user transition, so a reconnecting client
// can explain the park without replaying the whole stream.
const planModeWaitingReason = "执行计划已生成，等待你确认后开始执行。"

// planModeInstruction is the planning instruction sent to the *main* model.
//
// The shape follows internal/expert/orchestrator.go's e2ePlanInstruction (plan
// only, no tools, ordered steps naming the tool per step), but it is addressed
// to the main model for the main task rather than to an expert sub-agent, and
// it carries the effective tool catalogue so each step can name a real tool.
func planModeInstruction(tools []types.ToolDefinition) string {
	var b strings.Builder
	b.WriteString("请先只做规划，不要调用任何工具。\n")
	b.WriteString("根据用户的任务，输出一份有序的实施方案，格式如下：\n\n")
	b.WriteString("## 方案\n")
	b.WriteString("1. <步骤一：要做什么，用哪个工具，为什么>\n")
	b.WriteString("2. <步骤二：...>\n\n")
	b.WriteString("要求：\n")
	b.WriteString("- 步骤必须具体可执行，明确使用哪个工具\n")
	b.WriteString("- 标出哪些步骤有依赖关系（后一步依赖前一步的输出）\n")
	b.WriteString("- 不要执行，只输出方案\n")
	if len(tools) > 0 {
		b.WriteString("\n可用工具：\n")
		b.WriteString(toolCatalog(tools))
	}
	return b.String()
}

// rePlanHint is appended to the planning instruction when the user rejected a
// previous plan, so the second proposal is asked to differ rather than repeat.
const rePlanHint = "\n\n用户否决了上一版方案，请给出一份不同的、更符合任务要求的方案。"

// ConfirmedPlanMessage renders the system message that carries an approved plan
// into the conversation. Its wording mirrors the expert orchestrator's
// two-phase hand-off ("已确认的实施方案"), which is the reference implementation
// this task follows.
func ConfirmedPlanMessage(plan string) string {
	return "## 已确认的实施方案\n" + plan +
		"\n\n用户已确认上述方案。请严格按上述方案步骤有序执行，不要重新规划。"
}

// runPlanMode executes the user-visible plan phase.
//
// It returns a non-nil LoopResult when the run must stop here (the plan has
// been proposed and the run is parked), and nil when execution should continue
// into the normal round loop (the plan was approved).
func (l *Loop) runPlanMode(ctx context.Context, in LoopInput, conv *Conversation, res LoopResult) (*LoopResult, error) {
	if err := l.checkCancelled(ctx); err != nil {
		return nil, err
	}

	switch in.PlanDecision {
	case PlanDecisionApproved:
		// The user approved: the plan becomes part of the conversation before the
		// first round, so the model executes its own approved plan instead of
		// starting over. This is the whole point of the feature — an approved
		// plan that is never shown to the model would be decoration.
		if plan := strings.TrimSpace(in.PendingPlan); plan != "" {
			conv.AppendSystem(ConfirmedPlanMessage(plan))
		}
		return nil, nil

	case PlanDecisionRejected:
		// Fall through to propose a fresh plan. The run stays parked until the
		// user answers again, which is why this does not simply continue.

	default:
		// No decision pending: first proposal for this run.
	}

	instruction := planModeInstruction(conv.Tools)
	if in.PlanDecision == PlanDecisionRejected {
		instruction += rePlanHint
	}

	plan, err := l.proposePlan(ctx, in, conv, instruction)
	if err != nil {
		return nil, err
	}

	// Emitted before the park transition, so a subscriber always sees the plan
	// text before it sees the run waiting on it.
	revision := in.PlanRevision + 1
	if err := l.emit(ctx, LoopEvent{
		Type:    types.EventPlanProposed,
		Message: "执行计划已生成，等待用户确认",
		Data: map[string]any{
			"plan":     plan,
			"steps":    splitPlanSteps(plan),
			"revision": revision,
			"planMode": true,
		},
	}); err != nil {
		return nil, err
	}

	// Park the run in waiting_user. Reusing that state (instead of adding a new
	// RunState value) is what lets the existing park/resume machinery — durable
	// event, materialized WaitingReason, Resume path — carry this feature with
	// no change to the state machine's semantics.
	if err := l.transitionTo(ctx, in, types.StateWaitingUser, Transition{
		Reason:        "plan proposed; waiting for the user's decision",
		WaitingReason: planModeWaitingReason,
	}); err != nil {
		return nil, err
	}

	res.State = types.StateWaitingUser
	res.Rounds = conv.Rounds
	res.Usage = conv.Usage
	res.ToolCalls = conv.toolCallCount
	res.Answer = conv.LastAnswer
	// The plan and its revision travel back to the engine, which stores them so
	// the user's answer can be matched to the proposal it is answering.
	res.Plan = plan
	res.PlanRevision = revision
	return &res, nil
}

// proposePlan asks the provider for a plan with the tool catalogue withheld.
//
// Withholding the tools is what makes "plan only" enforceable rather than a
// polite request: a model that cannot call a tool cannot start the work early.
// The proposal is deliberately *not* appended to the conversation. It reaches
// the model only through ConfirmedPlanMessage once the user approves it, which
// keeps the message history append-only and stops a rejected draft from
// steering the retry.
func (l *Loop) proposePlan(ctx context.Context, in LoopInput, conv *Conversation, instruction string) (string, error) {
	if l.provider == nil {
		return "", types.NewError(types.CodeInternal, "plan mode requires a provider")
	}

	msgs := make([]ports.Message, 0, len(conv.Messages)+1)
	msgs = append(msgs, conv.Messages...)
	msgs = append(msgs, ports.Message{Role: ports.RoleSystem, Content: instruction})

	resp, err := l.provider.Complete(ctx, ports.ProviderRequest{
		// 与 think() 一致：模型名留空交给适配层回退到配置模型，不填占位名。
		Model:    in.Request.Model,
		Messages: msgs,
		Tools:    nil,
		Effort:   effortOr(in.Request.Effort),
	})
	if err != nil {
		if types.IsCancelled(err) || ctx.Err() != nil {
			return "", types.WrapError(types.CodeCancelled, err, "planning round cancelled")
		}
		return "", types.WrapError(types.CodeOf(err), err, "plan proposal round failed")
	}
	switch resp.FinishReason {
	case ports.FinishCancelled:
		return "", types.NewError(types.CodeCancelled, "plan proposal round cancelled")
	case ports.FinishError:
		return "", types.NewError(types.CodeProviderFailed,
			"plan proposal round failed: %s", resp.Error)
	}

	conv.Usage = mergeUsage(conv.Usage, resp.Usage)

	plan := strings.TrimSpace(resp.Content)
	if plan == "" {
		// An empty proposal would park the run on an empty card: there would be
		// nothing for the user to approve. Failing is honest and diagnosable.
		return "", types.NewError(types.CodeProviderFailed,
			"the model returned an empty plan; cannot ask the user to confirm it")
	}
	return plan, nil
}

// splitPlanSteps extracts the step lines from a plan so the UI can render a
// list rather than a wall of text.
//
// It is intentionally lenient: models format plans as numbered lists, bullets
// or plain prose, and a strict parser would produce an empty list exactly when
// the plan was most free-form. An empty result is fine — the UI falls back to
// rendering the raw text.
func splitPlanSteps(plan string) []string {
	var steps []string
	for _, line := range strings.Split(plan, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if !hasStepMarker(trimmed) {
			continue
		}
		steps = append(steps, trimStepMarker(trimmed))
	}
	return steps
}

// hasStepMarker reports whether a line looks like a plan step.
func hasStepMarker(line string) bool {
	if line == "" {
		return false
	}
	first, _ := utf8.DecodeRuneInString(line)
	switch first {
	case '-', '*', '•':
		return true
	}
	// A leading number followed by a delimiter: "1." / "1、" / "1)" / "1）".
	digits := 0
	for _, r := range line {
		if r < '0' || r > '9' {
			break
		}
		digits++
	}
	if digits == 0 || digits > 3 {
		// No digits, or an implausibly long run of them (a year, a token count).
		return false
	}
	rest := line[digits:]
	delim, _ := utf8.DecodeRuneInString(rest)
	switch delim {
	case '.', '、', ')', '）', '．':
		return true
	}
	return false
}

// trimStepMarker strips the list marker from a step line.
func trimStepMarker(line string) string {
	if line == "" {
		return ""
	}
	if first, width := utf8.DecodeRuneInString(line); first == '-' || first == '*' || first == '•' {
		return strings.TrimSpace(line[width:])
	}
	digits := 0
	for _, r := range line {
		if r < '0' || r > '9' {
			break
		}
		digits++
	}
	rest := line[digits:]
	if rest == "" {
		return ""
	}
	_, width := utf8.DecodeRuneInString(rest)
	return strings.TrimSpace(rest[width:])
}
