package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/ximo888ok-netizen/ximo-agent/internal/ports"
	"github.com/ximo888ok-netizen/ximo-agent/internal/ports/mem"
	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// This file pins the task-4 plan mode contract: park on a proposal, start only
// on approval, re-propose on rejection, and — most importantly — leave a run
// that never opted in completely untouched.

// planSink records emitted events, including their Data payloads, so the
// proposal's text can be asserted rather than only its type.
type planSink struct {
	recordingSink
}

func (s *planSink) dataOf(t types.EventType) []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]any
	for _, e := range s.events {
		if e.Type == t {
			out = append(out, e.Data)
		}
	}
	return out
}

// newPlanHarness builds a loop whose provider first returns a plan (tools are
// withheld for that round) and then behaves as scripted.
func newPlanHarness(t *testing.T, cfg types.AgentConfig, provider *mem.Provider) (*Loop, *planSink, *mem.Provider) {
	t.Helper()
	sink := &planSink{}
	guard := NewPanicGuard(GuardConfig{DumpDir: t.TempDir()})
	loop, err := NewLoop(LoopConfig{
		Config: cfg, Provider: provider, Sink: sink, Guard: guard,
		Dispatcher: ToolDispatcherFunc(func(_ context.Context, call types.ToolCall) ToolOutcome {
			return ToolOutcome{Call: call, Result: types.ToolResult{
				ToolCallID: call.ID, ToolName: call.Name, Success: true, Content: "ok",
			}}
		}),
	})
	if err != nil {
		t.Fatalf("NewLoop: %v", err)
	}
	return loop, sink, provider
}

// planRound is a provider response that looks like a plan proposal.
func planRound(plan string) ports.ProviderResponse {
	return ports.ProviderResponse{
		Content:      plan,
		FinishReason: ports.FinishStop,
	}
}

const samplePlan = "## 方案\n1. 读取 a.txt 了解现状\n2. 用 file_write 修改配置"

// TestPlanModeParksBeforeExecuting is acceptance criterion 2: with plan_mode on,
// a request stops on the plan and does not start executing.
func TestPlanModeParksBeforeExecuting(t *testing.T) {
	cfg := types.DefaultAgentConfig()
	// The provider is scripted with a plan and then an answer that must NOT be
	// reached: reaching it would mean execution started before the user decided.
	p := mem.NewProvider(planRound(samplePlan), mem.FinalRound("should not run"))
	loop, sink, provider := newPlanHarness(t, cfg, p)

	conv := NewConversation("", "refactor the parser", []types.ToolDefinition{{Name: "file_read"}})
	machine := NewMachine("run-plan", "ses-1", nil)
	res := loop.Run(context.Background(), LoopInput{
		RunID: "run-plan", SessionID: "ses-1",
		Request:      types.SubmitRequest{Prompt: "refactor the parser", PlanMode: true},
		Conversation: conv, Machine: machine,
	})

	if res.State != types.StateWaitingUser {
		t.Fatalf("state = %s, want waiting_user (err=%v)", res.State, res.Err)
	}
	if machine.State() != types.StateWaitingUser {
		t.Errorf("machine state = %s, want waiting_user", machine.State())
	}
	if !sink.has(types.EventPlanProposed) {
		t.Fatalf("no plan.proposed event; emitted %v", sink.types())
	}
	// Only the proposal round ran: no execution round happened.
	if got := provider.RoundCount(); got != 1 {
		t.Errorf("provider rounds = %d, want 1 (plan only, no execution)", got)
	}
	if res.Rounds != 0 {
		t.Errorf("completed rounds = %d, want 0: no model round should have run yet", res.Rounds)
	}
	if res.Plan != samplePlan {
		t.Errorf("result plan = %q, want the proposed text", res.Plan)
	}
	if res.PlanRevision != 1 {
		t.Errorf("plan revision = %d, want 1", res.PlanRevision)
	}
	// The waiting reason must describe a plan question, not a tool prompt.
	if !strings.Contains(planModeWaitingReason, "计划") {
		t.Errorf("waiting reason %q should describe the plan question", planModeWaitingReason)
	}
}

// TestPlanModeProposalCarriesPlanAndSteps checks the event payload the UI needs:
// the full text plus the parsed step list.
func TestPlanModeProposalCarriesPlanAndSteps(t *testing.T) {
	cfg := types.DefaultAgentConfig()
	p := mem.NewProvider(planRound(samplePlan), mem.FinalRound("done"))
	loop, sink, _ := newPlanHarness(t, cfg, p)

	conv := NewConversation("", "refactor the parser", nil)
	machine := NewMachine("run-plan", "ses-1", nil)
	loop.Run(context.Background(), LoopInput{
		RunID: "run-plan", SessionID: "ses-1",
		Request:      types.SubmitRequest{Prompt: "refactor the parser", PlanMode: true},
		Conversation: conv, Machine: machine,
	})

	payloads := sink.dataOf(types.EventPlanProposed)
	if len(payloads) != 1 {
		t.Fatalf("plan.proposed payloads = %d, want 1", len(payloads))
	}
	data := payloads[0]
	if got, _ := data["plan"].(string); got != samplePlan {
		t.Errorf("payload plan = %q, want the proposed text", got)
	}
	steps, ok := data["steps"].([]string)
	if !ok {
		t.Fatalf("payload steps has type %T, want []string", data["steps"])
	}
	if len(steps) != 2 {
		t.Fatalf("steps = %d (%v), want 2", len(steps), steps)
	}
	if !strings.Contains(steps[0], "a.txt") {
		t.Errorf("step 1 = %q, want the first plan step", steps[0])
	}

	// The proposal must be durable: losing it would strand a parked run with
	// nothing for the user to answer.
	if !types.EventPlanProposed.Durable() {
		t.Error("plan.proposed must be durable so a reconnect can rebuild the card")
	}
	if !types.EventPlanConfirmed.Durable() || !types.EventPlanRejected.Durable() {
		t.Error("plan.confirmed / plan.rejected must be durable so the decision is auditable")
	}
}

// TestPlanModeApprovalInjectsPlanAndExecutes is acceptance criterion 3: after the
// user approves, the run finishes and the plan text is really used as context.
func TestPlanModeApprovalInjectsPlanAndExecutes(t *testing.T) {
	cfg := types.DefaultAgentConfig()
	p := mem.NewProvider(
		mem.ToolCallRound(mem.NewCall("c1", "file_read", map[string]any{"path": "a.txt"})),
		mem.FinalRound("finished per plan"),
	)
	loop, _, _ := newPlanHarness(t, cfg, p)

	conv := NewConversation("", "refactor the parser", []types.ToolDefinition{{Name: "file_read"}})
	machine := NewMachine("run-plan", "ses-1", nil)
	res := loop.Run(context.Background(), LoopInput{
		RunID: "run-plan", SessionID: "ses-1",
		Request:      types.SubmitRequest{Prompt: "refactor the parser", PlanMode: true},
		Conversation: conv, Machine: machine,
		// The user's answer to the proposal made on the previous invocation.
		PlanDecision: PlanDecisionApproved,
		PendingPlan:  samplePlan,
		PlanRevision: 1,
	})

	if res.State != types.StateCompleted {
		t.Fatalf("state = %s, want completed (err=%v)", res.State, res.Err)
	}
	if res.Answer != "finished per plan" {
		t.Errorf("answer = %q", res.Answer)
	}
	// The plan must be present in the conversation as a system message, or the
	// feature is decoration: the model has to see the plan it is executing.
	found := false
	for _, m := range conv.Messages {
		if m.Role == ports.RoleSystem && strings.Contains(m.Content, samplePlan) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("the approved plan was not injected into the conversation; messages=%d",
			len(conv.Messages))
	}
}

// TestPlanModeRejectionReproposesPlan is acceptance criterion 4: asking for a
// re-plan yields a new plan rather than an error or a hang.
func TestPlanModeRejectionReproposesPlan(t *testing.T) {
	cfg := types.DefaultAgentConfig()
	const secondPlan = "## 方案\n1. 先备份配置\n2. 再改端口"
	p := mem.NewProvider(planRound(secondPlan), mem.FinalRound("done"))
	loop, sink, provider := newPlanHarness(t, cfg, p)

	conv := NewConversation("", "refactor the parser", nil)
	machine := NewMachine("run-plan", "ses-1", nil)
	res := loop.Run(context.Background(), LoopInput{
		RunID: "run-plan", SessionID: "ses-1",
		Request:      types.SubmitRequest{Prompt: "refactor the parser", PlanMode: true},
		Conversation: conv, Machine: machine,
		PlanDecision: PlanDecisionRejected,
		PendingPlan:  "## 方案\n1. 旧方案",
		PlanRevision: 1,
	})

	if res.State != types.StateWaitingUser {
		t.Fatalf("state = %s, want waiting_user after a rejection (err=%v)", res.State, res.Err)
	}
	if res.Err != nil {
		t.Fatalf("a rejected plan must not surface an error: %v", res.Err)
	}
	payloads := sink.dataOf(types.EventPlanProposed)
	if len(payloads) != 1 {
		t.Fatalf("plan.proposed payloads = %d, want 1 new proposal", len(payloads))
	}
	if got, _ := payloads[0]["plan"].(string); got != secondPlan {
		t.Errorf("re-proposed plan = %q, want the new text", got)
	}
	if got, _ := payloads[0]["revision"].(int); got != 2 {
		t.Errorf("revision = %v, want 2 (a re-plan is a new revision)", payloads[0]["revision"])
	}
	if res.PlanRevision != 2 {
		t.Errorf("result revision = %d, want 2", res.PlanRevision)
	}
	// Exactly one planning round ran; rejection does not start execution.
	if got := provider.RoundCount(); got != 1 {
		t.Errorf("provider rounds = %d, want 1", got)
	}
	// The rejected draft must not be carried into the conversation, or the model
	// would execute the plan the user just refused.
	for _, m := range conv.Messages {
		if strings.Contains(m.Content, "旧方案") {
			t.Error("the rejected plan leaked into the conversation")
		}
	}
}

// TestPlanModeRejectionHintsAtDifference checks that the re-plan instruction
// tells the model the previous attempt was refused.
func TestPlanModeRejectionHintsAtDifference(t *testing.T) {
	cfg := types.DefaultAgentConfig()
	p := mem.NewProvider(planRound(samplePlan))
	loop, _, provider := newPlanHarness(t, cfg, p)

	conv := NewConversation("", "refactor the parser", nil)
	machine := NewMachine("run-plan", "ses-1", nil)
	loop.Run(context.Background(), LoopInput{
		RunID: "run-plan", SessionID: "ses-1",
		Request:      types.SubmitRequest{Prompt: "refactor the parser", PlanMode: true},
		Conversation: conv, Machine: machine,
		PlanDecision: PlanDecisionRejected,
		PlanRevision: 1,
	})

	// The provider records the request it received; the last one is the re-plan.
	reqs := provider.Calls
	if len(reqs) == 0 {
		t.Fatal("the provider received no request")
	}
	last := reqs[len(reqs)-1]
	var prompt string
	for _, m := range last.Messages {
		if m.Role == ports.RoleSystem && strings.Contains(m.Content, "规划") {
			prompt = m.Content
		}
	}
	if prompt == "" {
		t.Fatalf("no planning instruction was sent; messages=%d", len(last.Messages))
	}
	if !strings.Contains(prompt, "否决") {
		t.Errorf("the re-plan instruction should mention the rejection:\n%s", prompt)
	}
	// Tools must be withheld, which is what makes "plan only" enforceable.
	if len(last.Tools) != 0 {
		t.Errorf("the planning round received %d tools, want 0", len(last.Tools))
	}
}

// TestPlanModeOffIsUnchanged is acceptance criterion 1 and the regression test
// the task book calls out: with plan_mode false, behaviour is exactly as before.
func TestPlanModeOffIsUnchanged(t *testing.T) {
	cfg := types.DefaultAgentConfig()

	// Run the same script twice, once with the flag absent and once explicitly
	// false, and compare against the pre-task-4 baseline shape.
	scripts := func() *mem.Provider {
		return mem.NewProvider(
			mem.ToolCallRound(mem.NewCall("c1", "file_read", map[string]any{"path": "a.txt"})),
			mem.FinalRound("all done"),
		)
	}
	for _, tc := range []struct {
		name string
		req  types.SubmitRequest
	}{
		{"flag absent", types.SubmitRequest{Prompt: "read a.txt"}},
		{"flag explicitly false", types.SubmitRequest{Prompt: "read a.txt", PlanMode: false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := scripts()
			loop, sink, _ := newPlanHarness(t, cfg, p)
			conv := NewConversation("", tc.req.Prompt, []types.ToolDefinition{{Name: "file_read"}})
			machine := NewMachine("run-x", "ses-1", nil)
			res := loop.Run(context.Background(), LoopInput{
				RunID: "run-x", SessionID: "ses-1",
				Request:      tc.req,
				Conversation: conv, Machine: machine,
			})

			if res.State != types.StateCompleted {
				t.Fatalf("state = %s, want completed (err=%v)", res.State, res.Err)
			}
			if res.Answer != "all done" {
				t.Errorf("answer = %q, want %q", res.Answer, "all done")
			}
			if res.Rounds != 2 {
				t.Errorf("rounds = %d, want 2", res.Rounds)
			}
			if res.ToolCalls != 1 {
				t.Errorf("tool calls = %d, want 1", res.ToolCalls)
			}
			// No plan-mode event may appear at all.
			for _, bad := range []types.EventType{
				types.EventPlanProposed, types.EventPlanConfirmed, types.EventPlanRejected,
			} {
				if sink.has(bad) {
					t.Errorf("plan_mode=false emitted %s; emitted %v", bad, sink.types())
				}
			}
			if res.Plan != "" || res.PlanRevision != 0 {
				t.Errorf("plan_mode=false produced plan state: %q rev=%d", res.Plan, res.PlanRevision)
			}
			// No planning instruction was sent to the provider.
			for _, req := range p.Calls {
				for _, m := range req.Messages {
					if strings.Contains(m.Content, "请先只做规划") {
						t.Error("plan_mode=false still sent a plan-mode instruction")
					}
				}
			}
		})
	}
}

// TestPlanModeDecisionIgnoredWithoutFlag checks that a stray decision cannot
// steer a run that never opted into plan mode.
func TestPlanModeDecisionIgnoredWithoutFlag(t *testing.T) {
	cfg := types.DefaultAgentConfig()
	p := mem.NewProvider(mem.FinalRound("plain answer"))
	loop, _, _ := newPlanHarness(t, cfg, p)

	conv := NewConversation("", "just answer", nil)
	machine := NewMachine("run-x", "ses-1", nil)
	res := loop.Run(context.Background(), LoopInput{
		RunID: "run-x", SessionID: "ses-1",
		Request:      types.SubmitRequest{Prompt: "just answer"}, // PlanMode false
		Conversation: conv, Machine: machine,
		PlanDecision: PlanDecisionApproved,
		PendingPlan:  "a plan that should be ignored",
	})

	if res.State != types.StateCompleted {
		t.Fatalf("state = %s, want completed", res.State)
	}
	for _, m := range conv.Messages {
		if strings.Contains(m.Content, "a plan that should be ignored") {
			t.Error("a plan was injected into a run that never enabled plan mode")
		}
	}
}

// TestPlanModeEmptyPlanFails checks that a model returning nothing does not park
// the user on an empty card.
func TestPlanModeEmptyPlanFails(t *testing.T) {
	cfg := types.DefaultAgentConfig()
	p := mem.NewProvider(planRound("   \n  "))
	loop, sink, _ := newPlanHarness(t, cfg, p)

	conv := NewConversation("", "refactor the parser", nil)
	machine := NewMachine("run-plan", "ses-1", nil)
	res := loop.Run(context.Background(), LoopInput{
		RunID: "run-plan", SessionID: "ses-1",
		Request:      types.SubmitRequest{Prompt: "refactor the parser", PlanMode: true},
		Conversation: conv, Machine: machine,
	})

	if res.State != types.StateFailed {
		t.Fatalf("state = %s, want failed for an empty plan", res.State)
	}
	if sink.has(types.EventPlanProposed) {
		t.Error("an empty plan must not be proposed to the user")
	}
	if res.Err == nil {
		t.Error("an empty plan should report an error")
	}
}

// TestPlanModeCancellationIsHonoured checks that a cancelled planning round ends
// the run as cancelled rather than parking it.
func TestPlanModeCancellationIsHonoured(t *testing.T) {
	cfg := types.DefaultAgentConfig()
	p := mem.NewProvider(ports.ProviderResponse{
		FinishReason: ports.FinishCancelled,
		Error:        "user cancelled",
	})
	loop, _, _ := newPlanHarness(t, cfg, p)

	conv := NewConversation("", "refactor the parser", nil)
	machine := NewMachine("run-plan", "ses-1", nil)
	res := loop.Run(context.Background(), LoopInput{
		RunID: "run-plan", SessionID: "ses-1",
		Request:      types.SubmitRequest{Prompt: "refactor the parser", PlanMode: true},
		Conversation: conv, Machine: machine,
	})

	if res.State != types.StateCancelled {
		t.Fatalf("state = %s, want cancelled (err=%v)", res.State, res.Err)
	}
	if !types.IsCancelled(res.Err) {
		t.Errorf("error = %v, want a cancellation code", res.Err)
	}
}

// TestPlanModeRunsAfterInternalPlanning checks the ordering the task book
// requires: the internal planning phase still runs (and still narrows tools)
// before the user-visible plan is produced.
func TestPlanModeRunsAfterInternalPlanning(t *testing.T) {
	cfg := types.DefaultAgentConfig()
	tools := make([]types.ToolDefinition, 0, 10)
	for i := 0; i < 10; i++ {
		tools = append(tools, types.ToolDefinition{Name: "tool_" + itoa(i)})
	}
	p := mem.NewProvider(planRound(samplePlan), mem.FinalRound("done"))
	planner := PlannerFunc(func(_ context.Context, req PlanRequest) (Plan, error) {
		return ParsePlan(
			"1. read\n2. [TOOLS] tool_1, tool_2 [/TOOLS]\n3. [STEPS] read; edit [/STEPS]",
			"", req.Tools)
	})
	sink := &planSink{}
	loop, err := NewLoop(LoopConfig{
		Config: cfg, Provider: p, Sink: sink, Planner: planner,
		Dispatcher: ToolDispatcherFunc(func(_ context.Context, call types.ToolCall) ToolOutcome {
			return ToolOutcome{Call: call, Result: types.ToolResult{Success: true}}
		}),
	})
	if err != nil {
		t.Fatal(err)
	}

	const prompt = "please refactor the parser so it handles nested structures"
	conv := NewConversation("", prompt, tools)
	machine := NewMachine("run-plan", "ses-1", nil)
	res := loop.Run(context.Background(), LoopInput{
		RunID: "run-plan", SessionID: "ses-1",
		Request:      types.SubmitRequest{Prompt: prompt, PlanMode: true},
		Conversation: conv, Machine: machine,
	})

	if res.State != types.StateWaitingUser {
		t.Fatalf("state = %s, want waiting_user", res.State)
	}
	// The internal planning phase ran first and narrowed the catalogue.
	if !sink.has(types.EventPlanningCompleted) {
		t.Errorf("the internal planning phase did not run; emitted %v", sink.types())
	}
	if len(conv.Tools) != 2 {
		t.Errorf("tools after internal planning = %d, want 2 (it must still narrow)", len(conv.Tools))
	}
	// The plan-mode instruction is sent after that narrowing, so it can name the
	// tools that actually survived.
	reqs := p.Calls
	var planPrompt string
	for _, m := range reqs[0].Messages {
		if strings.Contains(m.Content, "请先只做规划") {
			planPrompt = m.Content
		}
	}
	if planPrompt == "" {
		t.Fatal("no plan-mode instruction was sent")
	}
	if !strings.Contains(planPrompt, "tool_1") || strings.Contains(planPrompt, "tool_9") {
		t.Errorf("the plan-mode instruction should list the narrowed catalogue:\n%s", planPrompt)
	}
}

// TestPlanModeSplitPlanSteps checks the step parser across the formats models
// actually produce, including the empty result that makes the UI fall back.
func TestPlanModeSplitPlanSteps(t *testing.T) {
	cases := []struct {
		name string
		plan string
		want []string
	}{
		{
			name: "numbered with dots",
			plan: "## 方案\n1. read a.txt\n2. edit b.go",
			want: []string{"read a.txt", "edit b.go"},
		},
		{
			name: "numbered with ideographic comma",
			plan: "1、读取文件\n2、修改配置",
			want: []string{"读取文件", "修改配置"},
		},
		{
			name: "numbered with parenthesis",
			plan: "1) first\n2) second",
			want: []string{"first", "second"},
		},
		{
			name: "bullets",
			plan: "- read a.txt\n* edit b.go\n• run tests",
			want: []string{"read a.txt", "edit b.go", "run tests"},
		},
		{
			name: "headers are not steps",
			plan: "## 方案\n1. only step",
			want: []string{"only step"},
		},
		{
			name: "prose yields nothing",
			plan: "I will read the file and then edit it.",
			want: nil,
		},
		{
			name: "empty",
			plan: "",
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := splitPlanSteps(tc.plan)
			if len(got) != len(tc.want) {
				t.Fatalf("steps = %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("step %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestPlanDecisionValidity pins the decision enum, including the zero value the
// loop branches on.
func TestPlanDecisionValidity(t *testing.T) {
	for _, d := range []PlanDecision{PlanDecisionNone, PlanDecisionApproved, PlanDecisionRejected} {
		if !d.Valid() {
			t.Errorf("%q should be valid", string(d))
		}
	}
	if PlanDecision("maybe").Valid() {
		t.Error("an unknown decision should not be valid")
	}
	// The zero value must mean "no decision": a first execution carries it and
	// relies on the loop treating it as "propose a plan".
	if PlanDecisionNone != "" {
		t.Errorf("PlanDecisionNone = %q, want the zero value", string(PlanDecisionNone))
	}
}

// TestConfirmedPlanMessageCarriesThePlan checks the system message the model
// receives, since that text is what makes the approval real rather than cosmetic.
func TestConfirmedPlanMessageCarriesThePlan(t *testing.T) {
	msg := ConfirmedPlanMessage(samplePlan)
	if !strings.Contains(msg, samplePlan) {
		t.Error("the confirmed-plan message must contain the plan verbatim")
	}
	if !strings.Contains(msg, "已确认") {
		t.Error("the confirmed-plan message should say the user approved it")
	}
}
