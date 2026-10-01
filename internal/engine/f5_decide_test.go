package engine

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/agent"
	"github.com/ximo888ok-netizen/ximo-agent/internal/ports"
	"github.com/ximo888ok-netizen/ximo-agent/internal/ports/mem"
	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// This file pins the engine half of F5, the tool-authorization closed loop:
// a run parks on a tool the permission layer will not run unattended, Decide
// records the user's answer for one specific call, and Resume carries it into
// the next invocation of the loop.

// fakePermissionLayer models the permission layer's half of F5 in front of the
// tool port: a call it cannot run unattended comes back with
// Metadata["requiresConfirmation"], and it only executes once the engine has
// recorded the user's authorization on the run's durable log.
//
// Reading the log (rather than a private flag) is what makes the execution claim
// meaningful: the tool runs *because* the user's answer reached the engine, and
// the counter proves the ask itself never executed anything.
type fakePermissionLayer struct {
	events *mem.EventStore
	// alwaysAsk makes the layer refuse to run a call no matter what the log
	// says. It is used by tests that only need to observe whether the engine
	// parks, never by the execution-counting tests.
	alwaysAsk bool

	mu         sync.Mutex
	dispatches int
	executions int
}

// handler is the mem.ToolRuntime handler.
func (f *fakePermissionLayer) handler() func(context.Context, ports.ToolRequest) (types.ToolResult, error) {
	return func(_ context.Context, req ports.ToolRequest) (types.ToolResult, error) {
		f.mu.Lock()
		f.dispatches++
		f.mu.Unlock()

		if f.alwaysAsk || !f.authorized(req.RunID, req.ToolName) {
			// The permission layer refuses to run it unattended: it hands the
			// call back for a human decision rather than executing it.
			return types.ToolResult{
				ToolCallID: req.ToolCallID, ToolName: req.ToolName, Success: false,
				Error: "该工具需要用户授权",
				Metadata: map[string]any{
					metaRequiresConfirmation: true,
					metaConfirmationMessage:  "是否允许执行 " + req.ToolName + "？",
				},
			}, nil
		}

		f.mu.Lock()
		f.executions++
		f.mu.Unlock()
		return types.ToolResult{
			ToolCallID: req.ToolCallID, ToolName: req.ToolName, Success: true,
			Content: req.ToolName + " executed",
		}, nil
	}
}

// authorized reports whether the user's durable answer for this tool was an
// approval. It intentionally only understands approve=true: a recorded refusal
// must keep the tool from running.
func (f *fakePermissionLayer) authorized(runID, toolName string) bool {
	for _, ev := range f.events.All(runID) {
		if ev.Type != types.EventUserInputReceived {
			continue
		}
		name, _ := ev.Data["toolName"].(string)
		decide, _ := ev.Data["decide"].(bool)
		if name == toolName && decide {
			return true
		}
	}
	return false
}

func (f *fakePermissionLayer) counts() (dispatches, executions int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dispatches, f.executions
}

// f5RecordingProvider wraps the scripted provider and keeps its own copy of
// every request, so the test can assert what the model was actually shown
// without reaching into another package's private lock. (memory_submit_test.go
// has its own recordingProvider; this one is separate because it needs a lock
// and a content search.)
type f5RecordingProvider struct {
	inner *mem.Provider

	mu    sync.Mutex
	calls []ports.ProviderRequest
}

func (r *f5RecordingProvider) Complete(ctx context.Context, req ports.ProviderRequest) (ports.ProviderResponse, error) {
	r.mu.Lock()
	r.calls = append(r.calls, req)
	r.mu.Unlock()
	return r.inner.Complete(ctx, req)
}

// sawToolContent reports whether any request carried a tool-role message
// containing one of want. It accepts alternatives because the loop owns the
// exact refusal wording ("用户拒绝执行"/"用户取消执行"); what the engine must
// guarantee is that *a* refusal observation reaches the model.
func (r *f5RecordingProvider) sawToolContent(want ...string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, req := range r.calls {
		for _, m := range req.Messages {
			if m.Role != ports.RoleTool {
				continue
			}
			for _, w := range want {
				if strings.Contains(m.Content, w) {
					return true
				}
			}
		}
	}
	return false
}

// newPermissionHarness wires a harness whose tool runtime is the fake permission
// layer above.
func newPermissionHarness(t *testing.T, script ...ports.ProviderResponse) (*harness, *fakePermissionLayer, *f5RecordingProvider) {
	t.Helper()
	h := newHarness(t, script...)
	rec := &f5RecordingProvider{inner: h.provider}
	h.engine.deps.Provider = rec
	layer := &fakePermissionLayer{events: h.events}
	h.tools.Handler = layer.handler()
	return h, layer, rec
}

// pendingSnapshot copies the record's waiting set for assertions.
func pendingSnapshot(t *testing.T, h *harness, runID string) map[string]string {
	t.Helper()
	h.engine.mu.Lock()
	rec := h.engine.runs[runID]
	h.engine.mu.Unlock()
	if rec == nil {
		return nil
	}
	rec.mu.RLock()
	defer rec.mu.RUnlock()
	out := make(map[string]string, len(rec.pendingToolCalls))
	for k, v := range rec.pendingToolCalls {
		out[k] = v
	}
	return out
}

// waitForToolPark waits until the run parks with at least one pending call and
// returns the recorded set.
func waitForToolPark(t *testing.T, h *harness, runID string) map[string]string {
	t.Helper()
	waitForState(t, h, runID, types.StateWaitingUser)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if pending := pendingSnapshot(t, h, runID); len(pending) > 0 {
			return pending
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("run %s parked without recording a pending tool call", runID)
	return nil
}

// TestDecideApprovalExecutesTheParkedTool is acceptance criterion 1 end to end:
// the run parks instead of running the flagged tool, the approval resumes it,
// and the tool really executes afterwards — never before.
func TestDecideApprovalExecutesTheParkedTool(t *testing.T) {
	const toolX = "terminal_exec"
	// Round 1 asks; the resumed run re-issues the call (the wire format cannot
	// reply to the parked call across a park), then summarises.
	h, layer, _ := newPermissionHarness(t,
		mem.ToolCallRound(mem.NewCall("call-1", toolX, nil)),
		mem.ToolCallRound(mem.NewCall("call-2", toolX, nil)),
		mem.FinalRound("已完成"),
	)
	ctx := context.Background()

	handle, err := h.engine.Submit(ctx, types.SubmitRequest{Prompt: "run the tool"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	pending := waitForToolPark(t, h, handle.RunID)
	if got, ok := pending["call-1"]; !ok || got != toolX {
		t.Fatalf("pending tool calls = %v, want call-1 -> %s", pending, toolX)
	}

	// Nothing may have executed yet: the permission layer asked, it did not run.
	if _, execs := layer.counts(); execs != 0 {
		t.Fatalf("the flagged tool executed %d time(s) before the user approved", execs)
	}
	// The request that parked the run must be on the log, with the call ID a
	// decision quotes back.
	if !containsType(h.events.TypesOf(handle.RunID), types.EventToolPermissionRequired) {
		t.Fatalf("no tool_call.permission_required event; saw %v", h.events.TypesOf(handle.RunID))
	}

	if err := h.engine.Decide(ctx, handle.RunID, "call-1", true, ""); err != nil {
		t.Fatalf("Decide(approve): %v", err)
	}
	run := waitForState(t, h, handle.RunID, types.StateCompleted)
	if run.Answer != "已完成" {
		t.Errorf("answer = %q, want %q", run.Answer, "已完成")
	}

	dispatches, executions := layer.counts()
	if executions != 1 {
		t.Errorf("tool executions = %d, want 1 (the approved call must actually run)", executions)
	}
	if dispatches < 2 {
		t.Errorf("dispatches = %d, want >= 2 (the ask, then the authorized run)", dispatches)
	}
	// The answer is auditable: the user's approval is on the durable log.
	if !containsType(h.events.TypesOf(handle.RunID), types.EventUserInputReceived) {
		t.Errorf("no user_input.received event; saw %v", h.events.TypesOf(handle.RunID))
	}
}

// TestDecideDenialFeedsTheRefusalToTheModel is acceptance criterion 2: a refusal
// is not a failure — the loop hands the model "用户取消执行" and the run finishes.
func TestDecideDenialFeedsTheRefusalToTheModel(t *testing.T) {
	const toolX = "terminal_exec"
	h, layer, rec := newPermissionHarness(t,
		mem.ToolCallRound(mem.NewCall("call-1", toolX, nil)),
		mem.ToolCallRound(mem.NewCall("call-2", toolX, nil)),
		mem.FinalRound("换了一条路"),
	)
	ctx := context.Background()

	handle, err := h.engine.Submit(ctx, types.SubmitRequest{Prompt: "run the tool"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	waitForToolPark(t, h, handle.RunID)

	if err := h.engine.Decide(ctx, handle.RunID, "call-1", false, ""); err != nil {
		t.Fatalf("Decide(deny): %v", err)
	}
	run := waitForState(t, h, handle.RunID, types.StateCompleted)
	if run.Answer != "换了一条路" {
		t.Errorf("answer = %q, want %q", run.Answer, "换了一条路")
	}

	// The model must have been told the user refused, as a tool observation.
	if !rec.sawToolContent("用户拒绝执行", "用户取消执行") {
		t.Errorf("no tool message carrying the refusal reached the model")
	}
	// And the tool must not have run.
	if _, execs := layer.counts(); execs != 0 {
		t.Errorf("the refused tool executed %d time(s)", execs)
	}
}

// TestDecideRejectsUnknownRun checks that a decision for a run that does not
// exist is refused, not silently accepted.
func TestDecideRejectsUnknownRun(t *testing.T) {
	h, _, _ := newPermissionHarness(t)
	err := h.engine.Decide(context.Background(), "run_does_not_exist", "call-1", true, "")
	if err == nil {
		t.Fatal("a decision for an unknown run must fail")
	}
	if code := types.CodeOf(err); code != types.CodeNotFound && code != types.CodeTerminalState {
		t.Errorf("code = %s, want not_found/terminal_state", code)
	}
}

// TestDecideRejectsRunThatIsNotWaiting checks that a run which is not parked on
// an authorization cannot be steered by a decision.
func TestDecideRejectsRunThatIsNotWaiting(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	h, _, _ := newPermissionHarness(t)
	p := mem.NewProvider(mem.FinalRound("done"))
	p.Block = release
	h.engine.deps.Provider = p

	ctx := context.Background()
	handle, err := h.engine.Submit(ctx, types.SubmitRequest{Prompt: "blocking"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for p.RoundCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	err = h.engine.Decide(ctx, handle.RunID, "call-1", true, "")
	if err == nil {
		t.Fatal("a decision for a running (not parked) run must fail")
	}
	if code := types.CodeOf(err); code != types.CodeInvalidTransition {
		t.Errorf("code = %s, want %s", code, types.CodeInvalidTransition)
	}
}

// TestDecideRejectsCallThatIsNotPending checks the privilege boundary: a call ID
// the run never parked on is refused, and the park stays intact so the user can
// answer the real prompt.
func TestDecideRejectsCallThatIsNotPending(t *testing.T) {
	const toolX = "terminal_exec"
	h, _, _ := newPermissionHarness(t,
		mem.ToolCallRound(mem.NewCall("call-1", toolX, nil)),
		mem.ToolCallRound(mem.NewCall("call-2", toolX, nil)),
		mem.FinalRound("已完成"),
	)
	ctx := context.Background()

	handle, err := h.engine.Submit(ctx, types.SubmitRequest{Prompt: "run the tool"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	waitForToolPark(t, h, handle.RunID)

	err = h.engine.Decide(ctx, handle.RunID, "call-not-asked-about", true, "")
	if err == nil {
		t.Fatal("a decision for a call that is not pending must fail")
	}
	if code := types.CodeOf(err); code != types.CodeInvalidArgument {
		t.Errorf("code = %s, want %s", code, types.CodeInvalidArgument)
	}

	// The refusal must change nothing: the run is still parked and answerable.
	if run, gerr := h.engine.GetRun(ctx, handle.RunID); gerr != nil || run.State != types.StateWaitingUser {
		t.Fatalf("state after a refused decision = %s (err=%v), want waiting_user", run.State, gerr)
	}
	if pending := pendingSnapshot(t, h, handle.RunID); pending["call-1"] != toolX {
		t.Fatalf("pending set changed after a refused decision: %v", pending)
	}
	// And the real call is still decidable.
	if err := h.engine.Decide(ctx, handle.RunID, "call-1", true, ""); err != nil {
		t.Fatalf("Decide on the real pending call: %v", err)
	}
}

// TestDecideDoesNotAuthorizeAPlanPark is the cross-feature guard: waiting_user is
// also how plan mode parks a run, and a plan is not a tool authorization.
func TestDecideDoesNotAuthorizeAPlanPark(t *testing.T) {
	h := newHarness(t,
		ports.ProviderResponse{Content: planText, FinishReason: ports.FinishStop},
		mem.FinalRound("executed the plan"),
	)
	ctx := context.Background()

	handle, err := h.engine.Submit(ctx, types.SubmitRequest{Prompt: "refactor", PlanMode: true})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	waitForState(t, h, handle.RunID, types.StateWaitingUser)

	err = h.engine.Decide(ctx, handle.RunID, "call-1", true, "")
	if err == nil {
		t.Fatal("a tool decision must not resume a run parked on a plan")
	}
	if code := types.CodeOf(err); code != types.CodeInvalidArgument {
		t.Errorf("code = %s, want %s", code, types.CodeInvalidArgument)
	}

	// The plan path must still work afterwards.
	if err := h.engine.ConfirmPlan(ctx, handle.RunID, true); err != nil {
		t.Fatalf("ConfirmPlan after a refused tool decision: %v", err)
	}
	if run := waitForState(t, h, handle.RunID, types.StateCompleted); run.Answer != "executed the plan" {
		t.Errorf("answer = %q", run.Answer)
	}
}

// TestDecideIsSpentExactlyOnce covers acceptance criterion 4 at both levels: the
// atomic claim refuses a second answer for the same call, and a second Decide on
// a live run fails rather than queueing a second resume.
func TestDecideIsSpentExactlyOnce(t *testing.T) {
	t.Run("claim is atomic", func(t *testing.T) {
		rec := &runRecord{}
		rec.setPendingToolCalls([]string{"call-1"}, map[string]string{"call-1": "terminal_exec"})
		if _, ok := rec.claimToolCall("call-1"); !ok {
			t.Fatal("the first claim must win")
		}
		if _, ok := rec.claimToolCall("call-1"); ok {
			t.Fatal("the second claim must lose: the answer was already consumed")
		}
	})

	t.Run("second decide fails", func(t *testing.T) {
		const toolX = "terminal_exec"
		h, _, _ := newPermissionHarness(t,
			mem.ToolCallRound(mem.NewCall("call-1", toolX, nil)),
			mem.ToolCallRound(mem.NewCall("call-2", toolX, nil)),
			mem.FinalRound("已完成"),
		)
		ctx := context.Background()
		handle, err := h.engine.Submit(ctx, types.SubmitRequest{Prompt: "run the tool"})
		if err != nil {
			t.Fatalf("Submit: %v", err)
		}
		waitForToolPark(t, h, handle.RunID)

		if err := h.engine.Decide(ctx, handle.RunID, "call-1", true, ""); err != nil {
			t.Fatalf("first Decide: %v", err)
		}
		err = h.engine.Decide(ctx, handle.RunID, "call-1", true, "")
		if err == nil {
			t.Fatal("the second decision for the same call must fail")
		}
		if types.CodeOf(err) == "" {
			t.Errorf("the refusal carries no error code: %v", err)
		}
	})
}

// TestDecideRebuildsThePendingSetFromTheLog covers acceptance criterion 5: an
// Engine restart loses the in-memory set but not the ability to answer the
// prompt, because the request is still on the durable log.
func TestDecideRebuildsThePendingSetFromTheLog(t *testing.T) {
	const toolX = "terminal_exec"
	h, layer, _ := newPermissionHarness(t,
		mem.ToolCallRound(mem.NewCall("call-1", toolX, nil)),
		mem.ToolCallRound(mem.NewCall("call-2", toolX, nil)),
		mem.FinalRound("已完成"),
	)
	ctx := context.Background()
	handle, err := h.engine.Submit(ctx, types.SubmitRequest{Prompt: "run the tool"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	waitForToolPark(t, h, handle.RunID)

	// Simulate the restart: the durable log survives, the map does not.
	h.engine.mu.Lock()
	rec := h.engine.runs[handle.RunID]
	h.engine.mu.Unlock()
	if rec == nil {
		t.Fatal("the parked run vanished from memory")
	}
	rec.clearPendingToolCalls()
	if len(pendingSnapshot(t, h, handle.RunID)) != 0 {
		t.Fatal("the in-memory pending set was not cleared; the test would prove nothing")
	}

	if err := h.engine.Decide(ctx, handle.RunID, "call-1", true, ""); err != nil {
		t.Fatalf("Decide after losing the in-memory set: %v", err)
	}
	if run := waitForState(t, h, handle.RunID, types.StateCompleted); run.State != types.StateCompleted {
		t.Fatalf("state = %s", run.State)
	}
	if _, execs := layer.counts(); execs != 1 {
		t.Errorf("tool executions = %d, want 1", execs)
	}
}

// TestPendingToolCallsFromLog checks the reconstruction rules directly: only a
// request that no terminal tool event ever resolved is still pending.
func TestPendingToolCallsFromLog(t *testing.T) {
	h, _, _ := newPermissionHarness(t)
	ctx := context.Background()
	const runID = "run_pending_test"

	events := []types.Event{
		{Type: types.EventToolPermissionRequired, ToolCallID: "resolved", ToolName: "toolA"},
		{Type: types.EventToolCompleted, ToolCallID: "resolved", ToolName: "toolA"},
		{Type: types.EventToolPermissionRequired, ToolCallID: "waiting", ToolName: "toolB"},
		{Type: types.EventToolPermissionRequired, ToolCallID: "unnamed", ToolName: ""},
	}
	for _, ev := range events {
		if _, err := h.events.Append(ctx, runID, ev); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	got := h.engine.pendingToolCallsFromLog(ctx, runID)
	if _, ok := got["resolved"]; ok {
		t.Errorf("a call with a completed event is not pending: %v", got)
	}
	if name, ok := got["waiting"]; !ok || name != "toolB" {
		t.Errorf("pending = %v, want waiting -> toolB", got)
	}
	if _, ok := got["unnamed"]; ok {
		t.Errorf("a call with no tool name cannot be authorized: %v", got)
	}

	// A later prompt for the same call re-opens it (the loop parked again).
	if _, err := h.events.Append(ctx, runID, types.Event{
		Type: types.EventToolPermissionRequired, ToolCallID: "resolved", ToolName: "toolA",
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if name, ok := h.engine.pendingToolCallsFromLog(ctx, runID)["resolved"]; !ok || name != "toolA" {
		t.Errorf("a re-opened call must be pending again: %v", got)
	}
}

// TestDecideAfterRestartExecutesTheParkedTool is acceptance criterion 5 end to
// end: the process dies while a tool authorization is outstanding, recovery
// rehydrates the run, and the decision made afterwards still executes the tool.
//
// The restart is simulated the way TestPlanApprovedAfterRestartStillExecutes
// does it: drop the in-memory record, keep the durable log, then let Recover
// rebuild the run — exactly what a fresh process finds.
func TestDecideAfterRestartExecutesTheParkedTool(t *testing.T) {
	const toolX = "terminal_exec"
	h, layer, _ := newPermissionHarness(t,
		mem.ToolCallRound(mem.NewCall("call-1", toolX, nil)),
		mem.ToolCallRound(mem.NewCall("call-2", toolX, nil)),
		mem.FinalRound("重启后完成"),
	)
	ctx := context.Background()
	handle, err := h.engine.Submit(ctx, types.SubmitRequest{Prompt: "run the tool"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	waitForToolPark(t, h, handle.RunID)

	// The process dies while parked: the record goes, the log stays.
	h.engine.mu.Lock()
	delete(h.engine.runs, handle.RunID)
	h.engine.mu.Unlock()

	plans, err := h.engine.Recover(ctx)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	for _, plan := range plans {
		if plan.RunID != handle.RunID {
			continue
		}
		for _, note := range plan.Notes {
			if strings.Contains(note, "apply failed") {
				t.Errorf("recovery reported an apply failure: %v", plan.Notes)
			}
		}
	}
	if run, gerr := h.engine.GetRun(ctx, handle.RunID); gerr != nil || run.State != types.StateWaitingUser {
		t.Fatalf("rehydrated state = %s (err=%v), want waiting_user", run.State, gerr)
	}
	// The in-memory pending set died with the process; the decision must still
	// find the call on the log.
	if pending := pendingSnapshot(t, h, handle.RunID); len(pending) != 0 {
		t.Fatalf("rehydrated record unexpectedly carries a pending set: %v", pending)
	}

	if err := h.engine.Decide(ctx, handle.RunID, "call-1", true, ""); err != nil {
		t.Fatalf("Decide after restart: %v", err)
	}
	run := waitForState(t, h, handle.RunID, types.StateCompleted)
	if run.Answer != "重启后完成" {
		t.Errorf("answer = %q, want %q", run.Answer, "重启后完成")
	}
	if _, execs := layer.counts(); execs != 1 {
		t.Errorf("tool executions = %d, want 1 after the post-restart approval", execs)
	}
}

// TestDecideSessionScopeReleasesLaterCalls drives three separate invocations of
// one run with remember="session": a tool ask, a second ask for a different tool
// (which ends the invocation the first answer belonged to), and then a *new*
// call of the first tool. The session answer must release that new call, so the
// run finishes without a third click.
//
// Scope note on what this does NOT prove: with the current loop the folded
// decisions live on the conversation for the rest of the run, so a one-shot
// answer would also release this call. The difference between "once" and
// "session" is therefore not observable end to end yet; the engine-side half of
// the difference is pinned by TestSessionApprovalIsReapplied, and the loop-side
// retention is reported as a finding rather than silently asserted here.
func TestDecideSessionScopeReleasesLaterCalls(t *testing.T) {
	const (
		toolX = "terminal_exec"
		toolY = "file_write"
	)
	script := []ports.ProviderResponse{
		mem.ToolCallRound(mem.NewCall("call-1", toolX, nil)),
		mem.ToolCallRound(mem.NewCall("call-2", toolY, nil)),
		mem.ToolCallRound(mem.NewCall("call-3", toolX, nil)),
		mem.FinalRound("完成"),
	}

	h := newHarness(t, script...)
	h.tools.Handler = (&fakePermissionLayer{events: h.events, alwaysAsk: true}).handler()
	ctx := context.Background()

	handle, err := h.engine.Submit(ctx, types.SubmitRequest{Prompt: "work"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if pending := waitForToolPark(t, h, handle.RunID); pending["call-1"] != toolX {
		t.Fatalf("first park = %v, want call-1 -> %s", pending, toolX)
	}
	if err := h.engine.Decide(ctx, handle.RunID, "call-1", true, "session"); err != nil {
		t.Fatalf("Decide(call-1, session): %v", err)
	}

	// The next invocation hits a different tool, which ends the invocation the
	// session answer was recorded in.
	if pending := waitForToolPark(t, h, handle.RunID); pending["call-2"] != toolY {
		t.Fatalf("second park = %v, want call-2 -> %s", pending, toolY)
	}
	if err := h.engine.Decide(ctx, handle.RunID, "call-2", true, ""); err != nil {
		t.Fatalf("Decide(call-2): %v", err)
	}

	run := waitForState(t, h, handle.RunID, types.StateCompleted)
	if run.Answer != "完成" {
		t.Errorf("answer = %q, want %q", run.Answer, "完成")
	}
}

// TestToolDecisionsCarryBothKeys pins the map the engine hands the loop: the
// exact call and the tool-name key, because a resumed run re-issues the call
// with a new ID and only the name key can match it.
func TestToolDecisionsCarryBothKeys(t *testing.T) {
	rec := &runRecord{}
	rec.recordToolDecision("call-1", "terminal_exec", true)
	got := rec.takeToolDecisions()
	if v, ok := got["call-1"]; !ok || !v {
		t.Errorf("call-keyed decision missing: %v", got)
	}
	if v, ok := got[agent.ToolNameDecisionKey("terminal_exec")]; !ok || !v {
		t.Errorf("tool-name-keyed decision missing: %v", got)
	}
	// Consumed exactly once.
	if len(rec.takeToolDecisions()) != 0 {
		t.Error("the decisions were not cleared after being taken")
	}
}

// TestSessionApprovalIsReapplied checks the "remember for this session" scope:
// the answer survives the invocation it was given in and is re-applied to every
// later invocation of the same run.
func TestSessionApprovalIsReapplied(t *testing.T) {
	rec := &runRecord{}
	rec.recordToolDecision("call-1", "terminal_exec", true)
	rec.rememberSessionTool("terminal_exec")

	// First invocation consumes the explicit answer.
	first := rec.takeToolDecisions()
	if _, ok := first["call-1"]; !ok {
		t.Fatalf("explicit answer missing: %v", first)
	}
	// A later invocation has no explicit answer left, but the session rule must
	// still release the tool (keyed by name, since the ID is new).
	second := rec.takeToolDecisions()
	rec.applySessionApprovals(second)
	if v, ok := second[agent.ToolNameDecisionKey("terminal_exec")]; !ok || !v {
		t.Errorf("session approval was not re-applied: %v", second)
	}
	// A call that is still parked in the same batch is released by ID too.
	rec.setPendingToolCalls([]string{"call-still-parked"}, map[string]string{"call-still-parked": "terminal_exec"})
	third := rec.takeToolDecisions()
	rec.applySessionApprovals(third)
	if v, ok := third["call-still-parked"]; !ok || !v {
		t.Errorf("session approval did not release the parked call by ID: %v", third)
	}
	// rollback must undo both keys and remember that it did.
	rec.recordToolDecision("call-2", "terminal_exec", true)
	rec.rollbackToolDecision("call-2", "terminal_exec", true)
	fourth := rec.takeToolDecisions()
	if _, ok := fourth["call-2"]; ok {
		t.Errorf("rollback left the call key behind: %v", fourth)
	}
	if v, ok := fourth[agent.ToolNameDecisionKey("terminal_exec")]; ok && v {
		t.Errorf("rollback left the session rule behind: %v", fourth)
	}
}
