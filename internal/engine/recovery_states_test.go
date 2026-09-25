package engine

import (
	"context"
	"github.com/ximo888ok-netizen/ximo-agent/internal/agent"
	"testing"

	"github.com/ximo888ok-netizen/ximo-agent/internal/ports/mem"
	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// This file is evidence for the finding recorded in
// `types迁移清单-致08.md` §3.9: task 08's authoritative
// `types.RunState.IsRecoverable()` returns false for `created`, `queued` and —
// most importantly — `waiting_user`, whereas task 02's recovery must handle all
// three.
//
// `waiting_user` is the state task 02 parks a run in when crash recovery finds a
// non-idempotent tool call whose outcome is unknown. If the recovery candidate
// filter excludes it, a run parked by one crash is silently abandoned by the
// next restart: no error, no event, the run simply never continues. That is the
// same class of silent failure as the `waiting_worker`/`waiting_user` mismatch
// the audit flagged (P0-6 / ruling A3).
//
// The tests below show task 02's own recovery produces a correct plan for each
// of those three states, i.e. the gap is purely in the *candidate filter* the
// Supervisor applies, not in the engine.

// TestRecoveryFailsRunWithNoEvents covers the one case where a `created` run
// genuinely cannot be resumed: the log holds nothing at all, so recovery has no
// prompt, no round and no state to continue from. Marking it failed is the
// honest outcome; the alternative would be to invent a task for the user.
func TestRecoveryFailsRunWithNoEvents(t *testing.T) {
	// An empty log for a run ID that something has nevertheless heard of. This
	// models a crash between allocating the run ID and writing the first event.
	r, err := NewRecovery(RecoveryConfig{
		Events:      mem.NewEventStore(),
		Checkpoints: mem.NewCheckpointStore(),
		Idempotency: mem.NewIdempotencyStore(),
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := r.Inspect(context.Background(), "run-no-events")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if plan.Decision != MarkFailed {
		t.Errorf("decision = %s, want %s for a run with no events", plan.Decision, MarkFailed)
	}
	if len(plan.Notes) == 0 {
		t.Error("a failed recovery must explain itself in Notes")
	}
}

// TestRecoveryHandlesAllNonTerminalStates checks that every non-terminal state
// yields a usable recovery plan rather than being silently skipped.
func TestRecoveryHandlesAllNonTerminalStates(t *testing.T) {
	// One case per non-terminal chapter-4 state, each seeded so the run has a
	// durable log ending in that state.
	cases := []struct {
		state types.RunState
		// wantDecision is what recovery should conclude.
		wantDecision RecoveryDecision
	}{
		// `created` with a creation event in the log is resumable: it was
		// accepted but never queued, so resuming simply re-queues it. (A run
		// with *no* events at all is a different case — recovery cannot resume
		// what it knows nothing about, and marks that failed. See
		// TestRecoveryFailsRunWithNoEvents.)
		{types.StateCreated, ResumeAuto},
		// `queued` is resumable: it was accepted but never started.
		{types.StateQueued, ResumeAuto},
		{types.StatePlanning, ResumeAuto},
		{types.StateThinking, ResumeAuto},
		// `executing` with a started non-idempotent call must ask the user.
		{types.StateExecuting, ResumeAfterConfirm},
		{types.StateCompacting, ResumeAuto},
		// The state this finding is about: a run parked for user confirmation
		// must still be recognised as needing attention.
		{types.StateWaitingUser, ResumeAuto},
		{types.StateRecovering, ResumeAuto},
	}

	for _, tc := range cases {
		t.Run(string(tc.state), func(t *testing.T) {
			events := mem.NewEventStore()
			ctx := context.Background()
			runID := "run-state-" + string(tc.state)

			// Every run starts with a creation event so the log is well-formed.
			mustAppend(t, events, runID, types.Event{
				Type: types.EventRunCreated, State: types.StateCreated, SessionID: "ses-1",
			})
			if tc.state != types.StateCreated {
				mustAppend(t, events, runID, types.Event{
					Type: types.EventRunQueued, State: types.StateQueued, SessionID: "ses-1",
				})
			}
			if tc.state != types.StateCreated && tc.state != types.StateQueued {
				ev := types.Event{State: tc.state, SessionID: "ses-1"}
				switch tc.state {
				case types.StateWaitingUser:
					ev.Type = types.EventUserInputRequired
				default:
					ev.Type = types.EventRunStateChanged
				}
				mustAppend(t, events, runID, ev)
			}
			// An interrupted non-idempotent call makes `executing` need
			// confirmation; without it the state is simply resumable.
			if tc.state == types.StateExecuting {
				mustAppend(t, events, runID, types.Event{
					Type: types.EventToolStarted, ToolCallID: "call-term", ToolName: "terminal",
					State: types.StateExecuting,
				})
			}

			r, err := NewRecovery(RecoveryConfig{
				Events:      events,
				Checkpoints: mem.NewCheckpointStore(),
				Idempotency: mem.NewIdempotencyStore(),
			})
			if err != nil {
				t.Fatal(err)
			}
			plan, err := r.Inspect(ctx, runID)
			if err != nil {
				t.Fatalf("Inspect: %v", err)
			}
			if plan.Decision != tc.wantDecision {
				t.Errorf("state %s: decision = %s, want %s (notes: %v)",
					tc.state, plan.Decision, tc.wantDecision, plan.Notes)
			}
			// Whatever the decision, it must be a valid one: "silently skip" is
			// the failure mode being guarded against, so an unrecognised
			// decision is a bug.
			if !plan.Decision.Valid() {
				t.Errorf("state %s: invalid decision %q", tc.state, plan.Decision)
			}
		})
	}
}

// TestResumableIsBroaderThanIsRecoverable documents the exact divergence between
// task 02's predicate and task 08's authoritative one, so the ruling in §3.9 can
// be checked against code rather than prose.
//
// It asserts 02's predicate includes the three states 08's excludes. If task 08
// later widens `IsRecoverable`, this test still passes; it fails only if task 02
// narrows its own predicate and would therefore start dropping parked runs.
func TestResumableIsBroaderThanIsRecoverable(t *testing.T) {
	// The states task 08's IsRecoverable() excludes but task 02 must recover.
	mustBeResumable := []types.RunState{
		types.StateWaitingUser, // a run parked by a previous crash
		types.StateQueued,      // accepted but never started
		types.StateCreated,     // created, no work recorded yet
	}
	for _, s := range mustBeResumable {
		if !s.Resumable() {
			t.Errorf("%s must be resumable: excluding it would silently abandon the run", s)
		}
		// Terminal states must never be resumable, or a finished run could be
		// put back to work.
		if s.Terminal() {
			t.Errorf("%s is both resumable and terminal, which is contradictory", s)
		}
	}
	// The complement: terminal states are not resumable.
	for _, s := range []types.RunState{
		types.StateCompleted, types.StateCancelled, types.StateFailed,
	} {
		if s.Resumable() {
			t.Errorf("terminal state %s must not be resumable", s)
		}
	}
	// And the property that ties it together: every non-terminal state must be
	// resumable, so no non-terminal run can be stranded.
	for _, s := range types.AllRunStates {
		if s.Terminal() {
			continue
		}
		if !s.Resumable() {
			t.Errorf("non-terminal state %s is not resumable, so a run could be stranded there", s)
		}
	}
}

// TestParkedRunSurvivesASecondRestart is the end-to-end form of the §3.9
// concern: a run parked by one crash recovery must still be recognised and
// actionable after a *second* simulated restart.
//
// This is exactly the scenario that breaks if the recovery candidate filter
// excludes `waiting_user`: the first restart parks the run, and the second
// restart would never look at it again.
func TestParkedRunSurvivesASecondRestart(t *testing.T) {
	events := mem.NewEventStore()
	ctx := context.Background()
	const runID = "run-second-restart"

	// A crash left a non-idempotent call in flight.
	seedParkedRun(t, events, runID, "ses-twice")

	// First restart: recovery parks the run for confirmation.
	h := &harness{
		events: events, outbox: mem.NewOutboxStore(),
		ckpts: mem.NewCheckpointStore(), idem: mem.NewIdempotencyStore(),
		tools: mem.NewToolRuntime(nil), provider: mem.NewProvider(mem.FinalRound("ok")),
		guard: agent.NewPanicGuard(agent.GuardConfig{DumpDir: t.TempDir()}),
	}
	engine, err := New(engineConfig(), Dependencies{
		Events: h.events, Outbox: h.outbox, Checkpoints: h.ckpts,
		Idempotency: h.idem, Tools: h.tools, Provider: h.provider,
	}, h.guard)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Recover(ctx); err != nil {
		t.Fatalf("first Recover: %v", err)
	}
	parked, err := engine.GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if parked.State != types.StateWaitingUser {
		t.Fatalf("after the first recovery the run is %s, want waiting_user", parked.State)
	}
	engine.Close()

	// Second restart over the same durable log: the parked run must still be
	// visible as a candidate and must still be actionable.
	engine2, err := New(engineConfig(), Dependencies{
		Events: events, Outbox: mem.NewOutboxStore(),
		Checkpoints: mem.NewCheckpointStore(), Idempotency: mem.NewIdempotencyStore(),
		Tools: mem.NewToolRuntime(nil), Provider: mem.NewProvider(mem.FinalRound("ok")),
	}, agent.NewPanicGuard(agent.GuardConfig{DumpDir: t.TempDir()}))
	if err != nil {
		t.Fatal(err)
	}
	defer engine2.Close()

	// The predicate task 01 uses to pick candidates must include this state,
	// otherwise the Supervisor never hands the run back for recovery.
	if !parked.State.Resumable() {
		t.Errorf("state %s is not resumable, so the second restart would silently ignore it", parked.State)
	}

	// And recovery over the same log must still reach a coherent verdict.
	plans, err := engine2.Recover(ctx)
	if err != nil {
		t.Fatalf("second Recover: %v", err)
	}
	found := false
	for _, p := range plans {
		if p.RunID == runID {
			found = true
			if !p.Decision.Valid() {
				t.Errorf("second recovery produced an invalid decision %q", p.Decision)
			}
		}
	}
	if !found {
		t.Errorf("the second recovery produced no plan for the parked run; plans: %+v", plans)
	}
	// The run must remain resumable by the user rather than being stranded.
	after, err := engine2.GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State.Terminal() {
		t.Errorf("the parked run became terminal (%s) instead of staying resumable", after.State)
	}
}
