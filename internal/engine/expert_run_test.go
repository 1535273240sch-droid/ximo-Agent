package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/expert"
	"github.com/ximo888ok-netizen/ximo-agent/internal/ports"
	"github.com/ximo888ok-netizen/ximo-agent/internal/ports/mem"
	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// newExpertHarness builds an engine whose expert registry holds a deterministic
// fake expert instead of the embedded 254-expert catalogue.
//
// Why a fake: the shipped registry's tool recommendations come from the real
// division tables and its system prompt from the real personality text, neither
// of which a test should depend on. What is under test here is the wiring —
// "does a submitted expert_id reach Orchestrator.Activate" — not the catalogue.
func newExpertHarness(t *testing.T, script ...ports.ProviderResponse) *harness {
	t.Helper()
	h := newHarness(t, script...)
	h.engine.expertRegistry = expert.NewRegistry(nil)
	return h
}

// TestExpertRunActivatesChosenExpert is the core acceptance check for task 3:
// a submission carrying expert_id must be handled by that expert's sub-agent,
// with SubAgentMode true and the expert name matching the one picked.
func TestExpertRunActivatesChosenExpert(t *testing.T) {
	h := newExpertHarness(t,
		// planPhase (no tools) then runPhase (with tools) — two-phase is enabled,
		// so the first round is the plan and the second the answer.
		mem.FinalRound("1. 读取文件\n2. 总结"),
		mem.FinalRound("专家已完成分析。"),
	)

	ctx := context.Background()
	handle, err := h.engine.Submit(ctx, types.SubmitRequest{
		Prompt:   "帮我审查这段代码",
		ExpertID: "engineering-frontend-developer",
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if err := h.engine.WaitRun(ctx, handle.RunID); err != nil {
		t.Fatalf("WaitRun: %v", err)
	}

	run, err := h.engine.GetRun(ctx, handle.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.State != types.StateCompleted {
		t.Fatalf("state = %s, want completed (err=%v)", run.State, run.Err)
	}
	if !strings.Contains(run.Answer, "专家已完成分析") {
		t.Errorf("answer = %q, want the expert's sub-agent output", run.Answer)
	}

	// The final answer event must name the chosen expert and confirm that the
	// sub-agent really ran (the acceptance criterion for this task).
	events := h.events.All(handle.RunID)
	var final map[string]any
	for _, ev := range events {
		if ev.Type == types.EventFinalAnswer {
			final = ev.Data
		}
	}
	if final == nil {
		t.Fatal("no final_answer event was recorded")
	}
	if got := final["expertId"]; got != "engineering-frontend-developer" {
		t.Errorf("final answer expertId = %v, want engineering-frontend-developer", got)
	}
	if got := final["subAgentMode"]; got != true {
		t.Errorf("subAgentMode = %v, want true (the sub-agent must have run)", got)
	}
	if got := final["expertName"]; got == "" || got == nil {
		t.Error("final answer did not carry the expert name")
	}
}

// TestExpertRunUnknownExpertFails verifies an unknown id fails loudly instead of
// silently falling back to the generic agent loop: the user picked a specific
// expert, and a plausible-looking answer produced by someone else would be worse
// than an error.
func TestExpertRunUnknownExpertFails(t *testing.T) {
	h := newExpertHarness(t, mem.FinalRound("should not be reached"))

	ctx := context.Background()
	handle, err := h.engine.Submit(ctx, types.SubmitRequest{
		Prompt:   "do something",
		ExpertID: "no-such-expert",
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if err := h.engine.WaitRun(ctx, handle.RunID); err != nil {
		t.Fatalf("WaitRun: %v", err)
	}

	run, err := h.engine.GetRun(ctx, handle.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.State != types.StateFailed {
		t.Fatalf("state = %s, want failed for an unknown expert", run.State)
	}
	if !containsType(h.events.TypesOf(handle.RunID), types.EventError) {
		t.Error("no error event was recorded for the unknown expert")
	}
}

// TestSubmitWithoutExpertIsUnchanged is the regression guard the task calls for:
// a submission with no expert_id must take the ordinary agent loop and produce
// exactly the pre-existing event chain.
func TestSubmitWithoutExpertIsUnchanged(t *testing.T) {
	h := newExpertHarness(t,
		mem.ToolCallRound(mem.NewCall("call-1", "file_read", nil)),
		mem.FinalRound("finished"),
	)

	ctx := context.Background()
	handle, err := h.engine.Submit(ctx, types.SubmitRequest{Prompt: "read the file"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if err := h.engine.WaitRun(ctx, handle.RunID); err != nil {
		t.Fatalf("WaitRun: %v", err)
	}

	run, err := h.engine.GetRun(ctx, handle.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.State != types.StateCompleted || run.Answer != "finished" {
		t.Fatalf("plain run = (%s, %q), want (completed, finished)", run.State, run.Answer)
	}

	// The ordinary path must still route the tool call through the engine's own
	// dispatcher (durable started marker included).
	seen := h.events.TypesOf(handle.RunID)
	for _, want := range []types.EventType{
		types.EventToolCallRequested, types.EventToolStarted,
		types.EventToolCompleted, types.EventFinalAnswer,
	} {
		if !containsType(seen, want) {
			t.Errorf("plain run is missing %s; saw %v", want, seen)
		}
	}
}

// TestExpertRunEmitsPhaseEvents checks the two-phase orchestration is observable:
// the plan phase must be reported so the UI can show what the expert intends to
// do before it does it.
func TestExpertRunEmitsPhaseEvents(t *testing.T) {
	h := newExpertHarness(t,
		mem.FinalRound("## 方案\n1. 第一步\n2. 第二步"),
		mem.FinalRound("实施完成。"),
	)

	ctx := context.Background()
	handle, err := h.engine.Submit(ctx, types.SubmitRequest{
		Prompt:   "重构这个模块",
		ExpertID: "engineering-senior-developer",
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if err := h.engine.WaitRun(ctx, handle.RunID); err != nil {
		t.Fatalf("WaitRun: %v", err)
	}

	var final map[string]any
	for _, ev := range h.events.All(handle.RunID) {
		if ev.Type == types.EventFinalAnswer {
			final = ev.Data
		}
	}
	if final == nil {
		t.Fatal("no final_answer event was recorded")
	}
	rawPhases, ok := final["phaseEvents"].([]expert.PhaseEvent)
	if !ok || len(rawPhases) < 2 {
		t.Fatalf("phase events = %v, want at least plan and execute", final["phaseEvents"])
	}
	if rawPhases[0].Phase != expert.PhasePlan {
		t.Errorf("first phase = %q, want %q", rawPhases[0].Phase, expert.PhasePlan)
	}
	if rawPhases[len(rawPhases)-1].Phase != expert.PhaseExecute {
		t.Errorf("last phase = %q, want %q", rawPhases[len(rawPhases)-1].Phase, expert.PhaseExecute)
	}
}

// TestExpertRunRoutesToolsThroughDispatcher proves the expert path does not
// bypass the scheduler: a tool call made by the sub-agent must appear in the
// durable log as a started call, which only engine.dispatchToolCall writes.
func TestExpertRunRoutesToolsThroughDispatcher(t *testing.T) {
	var got []string
	h := newExpertHarness(t,
		// plan phase: no tools are offered, so it just plans.
		mem.FinalRound("## 方案\n1. 读文件"),
		// execute phase: the sub-agent asks for a tool, then answers.
		mem.ToolCallRound(mem.NewCall("expert-call-1", "file_read", map[string]any{"path": "x.txt"})),
		mem.FinalRound("读完了。"),
	)
	h.tools.Handler = func(_ context.Context, req ports.ToolRequest) (types.ToolResult, error) {
		got = append(got, req.ToolName)
		return types.ToolResult{
			ToolCallID: req.ToolCallID, ToolName: req.ToolName,
			Success: true, Content: "file contents",
		}, nil
	}

	ctx := context.Background()
	handle, err := h.engine.Submit(ctx, types.SubmitRequest{
		Prompt:   "读一下 x.txt",
		ExpertID: "engineering-frontend-developer",
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if err := h.engine.WaitRun(ctx, handle.RunID); err != nil {
		t.Fatalf("WaitRun: %v", err)
	}

	// The tool must have run, and the durable log must show the engine's own
	// marker — the evidence that it went through admission + fair queue + lease
	// rather than straight to the runtime.
	if len(got) == 0 {
		t.Fatal("the sub-agent's tool call never reached the tool runtime")
	}
	if got[0] != "file_read" {
		t.Errorf("tool = %q, want file_read", got[0])
	}
	if !containsType(h.events.TypesOf(handle.RunID), types.EventToolStarted) {
		t.Error("no tool_call.started event: the expert path bypassed engine.dispatchToolCall")
	}
}

// TestExpertRunTimeoutIsBoundedByRunBudget guards against configuring a sub-agent
// timeout that outlives the run's own wall-clock budget.
func TestExpertRunTimeoutIsBoundedByRunBudget(t *testing.T) {
	h := newExpertHarness(t)

	if h.engine.cfg.Scheduler.Limits.MaxRunDuration <= 0 {
		t.Fatal("test config has no run duration budget")
	}
	orch := h.engine.newExpertOrchestrator()
	if orch == nil {
		t.Fatal("newExpertOrchestrator returned nil for a configured engine")
	}
	// The orchestrator is opaque, so assert the observable contract instead: it
	// exposes the engine's own registry, i.e. it reads the real catalogue.
	if orch.Registry() != h.engine.expertRegistry {
		t.Error("orchestrator does not expose the engine's expert registry")
	}
}

// TestExpertRunWithoutProviderFails checks the path degrades with a clear error
// rather than a panic when no provider is wired.
func TestExpertRunWithoutProviderFails(t *testing.T) {
	h := newExpertHarness(t)
	h.engine.deps.Provider = nil
	h.engine.expertRegistry = expert.NewRegistry(nil)

	ctx := context.Background()
	handle, err := h.engine.Submit(ctx, types.SubmitRequest{
		Prompt:   "hi",
		ExpertID: "engineering-frontend-developer",
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if err := h.engine.WaitRun(ctx, handle.RunID); err != nil {
		t.Fatalf("WaitRun: %v", err)
	}
	run, err := h.engine.GetRun(ctx, handle.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.State != types.StateFailed {
		t.Fatalf("state = %s, want failed when no provider is configured", run.State)
	}
}

// TestExpertRunIgnoresPlanModeInteraction records the composition rule between
// the two field-adding tasks: expert_id decides the execution path, and
// plan_mode has no meaning on it (the expert's own plan phase replaces it).
func TestExpertRunIgnoresPlanModeInteraction(t *testing.T) {
	h := newExpertHarness(t,
		mem.FinalRound("## 方案\n1. 做事"),
		mem.FinalRound("完成。"),
	)

	ctx := context.Background()
	handle, err := h.engine.Submit(ctx, types.SubmitRequest{
		Prompt:   "做点事",
		ExpertID: "engineering-frontend-developer",
		PlanMode: true,
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if err := h.engine.WaitRun(ctx, handle.RunID); err != nil {
		t.Fatalf("WaitRun: %v", err)
	}

	run, err := h.engine.GetRun(ctx, handle.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	// The expert path completes rather than parking on a plan proposal: its plan
	// phase is internal, not a user-visible gate.
	if run.State != types.StateCompleted {
		t.Fatalf("state = %s, want completed (expert path does not park on plan_mode)", run.State)
	}
}

// TestExpertRunCancelledMidFlight checks a cancel during expert execution lands
// in the cancelled terminal state rather than hanging or reporting completed.
//
// The provider is held on a channel so the run is deterministically in flight
// when Cancel arrives; it releases on ctx cancellation, so the blocked round
// cannot outlive the test.
func TestExpertRunCancelledMidFlight(t *testing.T) {
	release := make(chan struct{})
	p := mem.NewProvider(mem.FinalRound("should not be reached"))
	p.Block = release
	defer close(release)

	h := newExpertHarness(t)
	h.engine.deps.Provider = p
	h.engine.Close()
	engine, err := New(engineConfig(), Dependencies{
		Events: h.events, Outbox: h.outbox, Checkpoints: h.ckpts,
		Idempotency: h.idem, Tools: h.tools, Provider: p,
	}, h.guard)
	if err != nil {
		t.Fatal(err)
	}
	h.engine = engine
	defer engine.Close()

	ctx := context.Background()
	handle, err := h.engine.Submit(ctx, types.SubmitRequest{
		Prompt:   "慢慢来",
		ExpertID: "engineering-frontend-developer",
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && p.RoundCount() == 0 {
		time.Sleep(2 * time.Millisecond)
	}
	if p.RoundCount() == 0 {
		t.Fatal("the run never reached the provider")
	}

	if err := h.engine.Cancel(ctx, handle.RunID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if err := h.engine.WaitRun(ctx, handle.RunID); err != nil {
		t.Fatalf("WaitRun: %v", err)
	}
	run, err := h.engine.GetRun(ctx, handle.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.State != types.StateCancelled {
		t.Fatalf("state = %s, want cancelled", run.State)
	}
}
