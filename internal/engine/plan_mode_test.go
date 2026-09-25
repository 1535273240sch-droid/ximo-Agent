package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/ports"
	"github.com/ximo888ok-netizen/ximo-agent/internal/ports/mem"
	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// This file pins the engine half of the task-4 plan mode: a run submitted with
// PlanMode parks on its plan, ConfirmPlan accepts or rejects it, and a plan that
// is still waiting survives an Engine restart.

// waitForState polls a run until it reaches the wanted state. The engine's run
// goroutine is asynchronous, so a test must wait rather than read once.
func waitForState(t *testing.T, h *harness, runID string, want types.RunState) types.Run {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last types.Run
	for time.Now().Before(deadline) {
		run, err := h.engine.GetRun(context.Background(), runID)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		last = run
		if run.State == want {
			return run
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("run %s stayed in %s, want %s (err=%v)", runID, last.State, want, last.Err)
	return last
}

// planText is the plan the scripted provider proposes.
const planText = "## 方案\n1. 读取 a.txt\n2. 修改配置"

// TestPlanModeParksAndApprovalRunsToCompletion covers acceptance criteria 2 and
// 3 end to end: the run stops on the plan, and confirming it executes.
func TestPlanModeParksAndApprovalRunsToCompletion(t *testing.T) {
	// Round 1 is the proposal; round 2 answers the approved execution.
	h := newHarness(t,
		ports.ProviderResponse{Content: planText, FinishReason: ports.FinishStop},
		mem.FinalRound("executed the plan"),
	)
	ctx := context.Background()

	handle, err := h.engine.Submit(ctx, types.SubmitRequest{
		Prompt: "refactor the parser", PlanMode: true,
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	// It must park rather than run to completion on its own.
	run := waitForState(t, h, handle.RunID, types.StateWaitingUser)
	if run.State != types.StateWaitingUser {
		t.Fatalf("state = %s, want waiting_user", run.State)
	}
	if run.Answer != "" {
		t.Errorf("a parked run must not have an answer yet, got %q", run.Answer)
	}

	// The proposal must be on the durable log with its text, which is what lets
	// a reconnecting UI rebuild the card.
	events := h.events.All(handle.RunID)
	var proposal map[string]any
	for _, ev := range events {
		if ev.Type == types.EventPlanProposed {
			proposal = ev.Data
		}
	}
	if proposal == nil {
		t.Fatalf("no plan.proposed event on the log; saw %v", h.events.TypesOf(handle.RunID))
	}
	if got, _ := proposal["plan"].(string); got != planText {
		t.Errorf("logged plan = %q, want %q", got, planText)
	}

	// Confirm: the run must now execute and finish.
	if err := h.engine.ConfirmPlan(ctx, handle.RunID, true); err != nil {
		t.Fatalf("ConfirmPlan(approved): %v", err)
	}
	final := waitForState(t, h, handle.RunID, types.StateCompleted)
	if final.Answer != "executed the plan" {
		t.Errorf("answer = %q, want %q", final.Answer, "executed the plan")
	}

	// The approval must be recorded, so the decision is auditable.
	if !containsType(h.events.TypesOf(handle.RunID), types.EventPlanConfirmed) {
		t.Errorf("no plan.confirmed event; saw %v", h.events.TypesOf(handle.RunID))
	}
	// The proposal is spent: a second confirmation must be refused rather than
	// re-running the plan.
	if err := h.engine.ConfirmPlan(ctx, handle.RunID, true); err == nil {
		t.Error("a second confirmation should be refused")
	}
}

// TestPlanModeRejectionReproposesPlan is acceptance criterion 4 at engine level.
func TestPlanModeRejectionReproposesPlan(t *testing.T) {
	const secondPlan = "## 方案\n1. 换个思路：先备份再改"
	h := newHarness(t,
		ports.ProviderResponse{Content: planText, FinishReason: ports.FinishStop},
		ports.ProviderResponse{Content: secondPlan, FinishReason: ports.FinishStop},
		mem.FinalRound("done"),
	)
	ctx := context.Background()

	handle, err := h.engine.Submit(ctx, types.SubmitRequest{
		Prompt: "refactor the parser", PlanMode: true,
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	waitForState(t, h, handle.RunID, types.StateWaitingUser)

	// Rejecting must produce a new plan and park again, not fail or hang.
	if err := h.engine.ConfirmPlan(ctx, handle.RunID, false); err != nil {
		t.Fatalf("ConfirmPlan(rejected): %v", err)
	}
	waitForState(t, h, handle.RunID, types.StateWaitingUser)

	// The newest proposal must be the second one.
	var plans []string
	for _, ev := range h.events.All(handle.RunID) {
		if ev.Type == types.EventPlanProposed {
			if v, ok := ev.Data["plan"].(string); ok {
				plans = append(plans, v)
			}
		}
	}
	if len(plans) != 2 {
		t.Fatalf("plan.proposed events = %d, want 2 (original + re-plan)", len(plans))
	}
	if plans[1] != secondPlan {
		t.Errorf("second plan = %q, want %q", plans[1], secondPlan)
	}
	if !containsType(h.events.TypesOf(handle.RunID), types.EventPlanRejected) {
		t.Errorf("no plan.rejected event; saw %v", h.events.TypesOf(handle.RunID))
	}
	// Still parked, and not executing: a rejection is not an approval.
	if run, _ := h.engine.GetRun(ctx, handle.RunID); run.State != types.StateWaitingUser {
		t.Errorf("state = %s after re-plan, want waiting_user", run.State)
	}

	// Approving the second plan finishes the run.
	if err := h.engine.ConfirmPlan(ctx, handle.RunID, true); err != nil {
		t.Fatalf("ConfirmPlan(approved on retry): %v", err)
	}
	final := waitForState(t, h, handle.RunID, types.StateCompleted)
	if final.Answer != "done" {
		t.Errorf("answer = %q, want %q", final.Answer, "done")
	}
}

// TestConfirmPlanRejectsRunWithoutPlan checks the guard: a run parked for a
// reason other than a plan must not be resumable through this path.
func TestConfirmPlanRejectsRunWithoutPlan(t *testing.T) {
	h := newHarness(t, mem.FinalRound("done"))
	ctx := context.Background()

	// A run that finished normally has no plan pending.
	handle, err := h.engine.Submit(ctx, types.SubmitRequest{Prompt: "just answer"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if err := h.engine.WaitRun(ctx, handle.RunID); err != nil {
		t.Fatalf("WaitRun: %v", err)
	}
	if err := h.engine.ConfirmPlan(ctx, handle.RunID, true); err == nil {
		t.Error("confirming a run with no pending plan should fail")
	}

	// A run that never existed is reported as such rather than silently accepted.
	if err := h.engine.ConfirmPlan(ctx, "run-does-not-exist", true); err == nil {
		t.Error("confirming an unknown run should fail")
	}
}

// TestPlanModeOffDoesNotPark is the engine-level regression guard for acceptance
// criterion 1: without the flag the run behaves exactly as before.
func TestPlanModeOffDoesNotPark(t *testing.T) {
	h := newHarness(t,
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
	if run.State != types.StateCompleted {
		t.Fatalf("state = %s, want completed (err=%v)", run.State, run.Err)
	}

	seen := h.events.TypesOf(handle.RunID)
	for _, bad := range []types.EventType{
		types.EventPlanProposed, types.EventPlanConfirmed, types.EventPlanRejected,
	} {
		if containsType(seen, bad) {
			t.Errorf("plan_mode=false produced %s; saw %v", bad, seen)
		}
	}
}

// TestPendingPlanRebuiltFromLog is acceptance criterion 5: a run parked on a plan
// stays confirmable after the Engine process is replaced, which is what makes it
// survive a crash or a reconnect.
//
// The test simulates the restart by dropping the in-memory record while keeping
// the same durable event log, which is exactly the state a fresh process finds.
func TestPendingPlanRebuiltFromLog(t *testing.T) {
	h := newHarness(t,
		ports.ProviderResponse{Content: planText, FinishReason: ports.FinishStop},
		mem.FinalRound("executed after restart"),
	)
	ctx := context.Background()

	handle, err := h.engine.Submit(ctx, types.SubmitRequest{
		Prompt: "refactor the parser", PlanMode: true,
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	waitForState(t, h, handle.RunID, types.StateWaitingUser)

	// Forget the in-memory record, keeping the log: the run now looks like one
	// whose process died while parked.
	h.engine.mu.Lock()
	rec := h.engine.runs[handle.RunID]
	delete(h.engine.runs, handle.RunID)
	h.engine.mu.Unlock()
	if rec == nil {
		t.Fatal("the run record was not in memory")
	}

	// The proposal must still be discoverable from the log alone.
	plan, ok := h.engine.pendingPlanFromLog(ctx, handle.RunID)
	if !ok {
		t.Fatal("the pending plan was not recoverable from the durable log")
	}
	if plan != planText {
		t.Errorf("recovered plan = %q, want %q", plan, planText)
	}

	// Once a decision has been recorded, the proposal is no longer pending, so a
	// stale confirmation cannot re-apply a spent decision.
	ev := types.Event{
		RunID: handle.RunID, SessionID: handle.RunID,
		Type: types.EventPlanConfirmed, Timestamp: time.Now(), Message: "confirmed",
	}
	if _, err := h.events.Append(ctx, handle.RunID, ev); err != nil {
		t.Fatalf("append decision: %v", err)
	}
	if _, ok := h.engine.pendingPlanFromLog(ctx, handle.RunID); ok {
		t.Error("a decided plan must not still count as pending")
	}
}

// TestPlanApprovedAfterRestartStillExecutes pins the missing half of acceptance
// criterion 5: confirming a plan after the process came back must execute the
// approved plan, not silently drop the decision.
//
// The gap: a run recovered from a dead process is rehydrated with a synthetic
// request whose PlanMode is false, and the loop ignores plan decisions without
// that flag (anti-injection by design). The engine is the only legitimate source
// of decisions, so it must raise the flag itself when it holds a decision — this
// test fails without that bridge.
//
// The restart is simulated the same way TestPendingPlanRebuiltFromLog does:
// drop the in-memory record, keep the durable log, then let Recover's
// ResumeAuto path rehydrate the run — exactly what a fresh process does.
func TestPlanApprovedAfterRestartStillExecutes(t *testing.T) {
	h := newHarness(t,
		ports.ProviderResponse{Content: planText, FinishReason: ports.FinishStop},
		mem.FinalRound("executed after restart"),
	)
	ctx := context.Background()

	handle, err := h.engine.Submit(ctx, types.SubmitRequest{
		Prompt: "refactor the parser", PlanMode: true,
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	waitForState(t, h, handle.RunID, types.StateWaitingUser)

	// Simulate the restart: forget the in-memory record, keep the log.
	h.engine.mu.Lock()
	rec := h.engine.runs[handle.RunID]
	delete(h.engine.runs, handle.RunID)
	h.engine.mu.Unlock()
	if rec == nil {
		t.Fatal("the run record was not in memory")
	}

	// Recovery must rehydrate the parked run without errors: a waiting_user run
	// rehydrates at waiting_user, where the state machine has no self-edge.
	plans, err := h.engine.Recover(ctx)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	for _, plan := range plans {
		if plan.RunID == handle.RunID {
			for _, note := range plan.Notes {
				if strings.Contains(note, "apply failed") {
					t.Errorf("recovery reported an apply failure: %v", plan.Notes)
				}
			}
		}
	}
	run, err := h.engine.GetRun(ctx, handle.RunID)
	if err != nil {
		t.Fatalf("GetRun after rehydrate: %v", err)
	}
	if run.State != types.StateWaitingUser {
		t.Fatalf("rehydrated run state = %s, want waiting_user", run.State)
	}

	// The user confirms. The plan comes back from the log (the in-memory flag
	// died with the process) and the run must complete with the approved plan
	// actually executed.
	if err := h.engine.ConfirmPlan(ctx, handle.RunID, true); err != nil {
		t.Fatalf("ConfirmPlan after restart: %v", err)
	}
	final := waitForState(t, h, handle.RunID, types.StateCompleted)
	if final.Answer != "executed after restart" {
		t.Errorf("answer = %q, want %q (the approved plan must be executed, not dropped)",
			final.Answer, "executed after restart")
	}
}

// TestPlanModeProposalIsDurable checks the backpressure classification the
// reconnect path depends on.
func TestPlanModeProposalIsDurable(t *testing.T) {
	for _, ev := range []types.EventType{
		types.EventPlanProposed, types.EventPlanConfirmed, types.EventPlanRejected,
	} {
		if !ev.Durable() {
			t.Errorf("%s must be durable", ev)
		}
		if ev.Mergeable() {
			t.Errorf("%s must not be mergeable", ev)
		}
		if !ev.Valid() {
			t.Errorf("%s must be a known event type", ev)
		}
	}
	// The wire names are the shared contract and must not drift.
	if string(types.EventPlanProposed) != "plan.proposed" ||
		string(types.EventPlanConfirmed) != "plan.confirmed" ||
		string(types.EventPlanRejected) != "plan.rejected" {
		t.Errorf("plan event names drifted: %s / %s / %s",
			types.EventPlanProposed, types.EventPlanConfirmed, types.EventPlanRejected)
	}
}

// TestPlanModeWaitingReasonIsNotAToolPrompt checks that the park explains itself
// as a plan question, since the UI renders the reason verbatim.
func TestPlanModeWaitingReasonIsNotAToolPrompt(t *testing.T) {
	h := newHarness(t,
		ports.ProviderResponse{Content: planText, FinishReason: ports.FinishStop},
		mem.FinalRound("done"),
	)
	ctx := context.Background()

	handle, err := h.engine.Submit(ctx, types.SubmitRequest{
		Prompt: "refactor the parser", PlanMode: true,
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	run := waitForState(t, h, handle.RunID, types.StateWaitingUser)
	if !strings.Contains(run.WaitingReason, "计划") {
		t.Errorf("waiting reason = %q, should explain that a plan is waiting", run.WaitingReason)
	}
}
