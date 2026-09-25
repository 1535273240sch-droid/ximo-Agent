package engine

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ximo888ok-netizen/ximo-agent/internal/agent"
	"github.com/ximo888ok-netizen/ximo-agent/internal/ports/mem"
	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// TestRecoveryPlanWireConversion checks that the engine's plan and the
// IPC-facing types.RecoveryPlan stay in agreement: the wire form is what task
// 01 serializes for the UI, so a dropped field would silently remove the
// information the user needs to answer a confirmation prompt.
func TestRecoveryPlanWireConversion(t *testing.T) {
	plan := RecoveryPlan{
		RunID:               "run-1",
		SessionID:           "ses-1",
		Decision:            ResumeAfterConfirm,
		ResumeState:         types.StateExecuting,
		LastSeq:             42,
		CheckpointSeq:       17,
		Round:               3,
		UncertainToolCalls:  []string{"call-a"},
		ReplayableToolCalls: []string{"call-b"},
		SequenceOK:          true,
		Notes:               []string{"note one"},
	}

	wire := plan.Wire()
	if wire.RunID != plan.RunID || wire.SessionID != plan.SessionID {
		t.Error("Wire lost the run or session identity")
	}
	if wire.Decision != string(plan.Decision) {
		t.Errorf("Wire decision = %q, want %q", wire.Decision, plan.Decision)
	}
	if wire.ResumeState != plan.ResumeState {
		t.Errorf("Wire resume state = %s, want %s", wire.ResumeState, plan.ResumeState)
	}
	if wire.LastSeq != plan.LastSeq || wire.CheckpointSeq != plan.CheckpointSeq || wire.Round != plan.Round {
		t.Errorf("Wire lost sequence or round data: %+v", wire)
	}
	if len(wire.UncertainToolCalls) != 1 || wire.UncertainToolCalls[0] != "call-a" {
		t.Errorf("Wire uncertain calls = %v, want [call-a]", wire.UncertainToolCalls)
	}
	if len(wire.ReplayableToolCalls) != 1 || wire.ReplayableToolCalls[0] != "call-b" {
		t.Errorf("Wire replayable calls = %v, want [call-b]", wire.ReplayableToolCalls)
	}
	if !wire.SequenceOK {
		t.Error("Wire lost the sequence-soundness flag")
	}
	if len(wire.Notes) != 1 {
		t.Errorf("Wire notes = %v, want one entry", wire.Notes)
	}

	// The wire form must survive JSON, since that is how it crosses IPC.
	encoded, err := json.Marshal(wire)
	if err != nil {
		t.Fatalf("marshal wire plan: %v", err)
	}
	var decoded types.RecoveryPlan
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal wire plan: %v", err)
	}
	if decoded.Decision != types.DecisionResumeAfterConfirm {
		t.Errorf("round-tripped decision = %q, want %q",
			decoded.Decision, types.DecisionResumeAfterConfirm)
	}
	if len(decoded.UncertainToolCalls) != 1 {
		t.Errorf("round-trip lost the uncertain calls: %v", decoded.UncertainToolCalls)
	}

	// The copies must be independent, so a caller mutating the wire form cannot
	// reach into the engine's plan.
	wire.UncertainToolCalls[0] = "mutated"
	if plan.UncertainToolCalls[0] != "call-a" {
		t.Error("Wire shares the uncertain-calls slice with the source plan")
	}
}

// TestWirePlansPreservesOrderAndCount checks the batch conversion used by the
// IPC entry point.
func TestWirePlansPreservesOrderAndCount(t *testing.T) {
	plans := []RecoveryPlan{
		{RunID: "run-a", Decision: ResumeAuto},
		{RunID: "run-b", Decision: ResumeAfterConfirm},
		{RunID: "run-c", Decision: MarkFailed},
	}
	wire := WirePlans(plans)
	if len(wire) != 3 {
		t.Fatalf("WirePlans returned %d entries, want 3", len(wire))
	}
	for i, w := range wire {
		if w.RunID != plans[i].RunID {
			t.Errorf("entry %d: run = %s, want %s", i, w.RunID, plans[i].RunID)
		}
		if w.Decision != string(plans[i].Decision) {
			t.Errorf("entry %d: decision = %s, want %s", i, w.Decision, plans[i].Decision)
		}
	}
	if WirePlans(nil) == nil {
		t.Error("WirePlans(nil) returned nil; an empty non-nil slice is friendlier to JSON callers")
	}
}

// TestRecoverForIPCMatchesRecover checks that the IPC entry point performs the
// same recovery as the direct one, and returns the wire shape.
func TestRecoverForIPCMatchesRecover(t *testing.T) {
	events := mem.NewEventStore()
	ctx := context.Background()
	const runID = "run-ipc"

	mustAppend(t, events, runID, types.Event{
		Type: types.EventRunQueued, State: types.StateQueued, SessionID: "ses-ipc",
	})
	mustAppend(t, events, runID, types.Event{
		Type: types.EventToolStarted, ToolCallID: "call-term", ToolName: "terminal",
		State: types.StateExecuting,
	})

	engine, err := New(engineConfig(), Dependencies{
		Events: events, Outbox: mem.NewOutboxStore(),
		Checkpoints: mem.NewCheckpointStore(), Idempotency: mem.NewIdempotencyStore(),
		Tools: mem.NewToolRuntime(nil), Provider: mem.NewProvider(mem.FinalRound("ok")),
	}, agent.NewPanicGuard(agent.GuardConfig{DumpDir: t.TempDir()}))
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()

	wirePlans, err := engine.RecoverForIPC(ctx)
	if err != nil {
		t.Fatalf("RecoverForIPC: %v", err)
	}
	if len(wirePlans) != 1 {
		t.Fatalf("wire plans = %d, want 1", len(wirePlans))
	}
	if wirePlans[0].RunID != runID {
		t.Errorf("wire plan run = %s, want %s", wirePlans[0].RunID, runID)
	}
	if wirePlans[0].Decision != types.DecisionResumeAfterConfirm {
		t.Errorf("wire decision = %s, want %s", wirePlans[0].Decision, types.DecisionResumeAfterConfirm)
	}
	// The run really must be parked, not merely reported as such.
	run, err := engine.GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.State != types.StateWaitingUser {
		t.Errorf("run state = %s, want waiting_user", run.State)
	}
}
