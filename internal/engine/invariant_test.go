package engine

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/agent"
	"github.com/ximo888ok-netizen/ximo-agent/internal/ports"
	"github.com/ximo888ok-netizen/ximo-agent/internal/ports/mem"
	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// This file covers the two invariants the independent audit (task 09) marked
// "partially satisfied" for task 02:
//
//	I1  any running run must have exactly one owner
//	I8  a cancelled run must not start new side effects
//
// Both are tested through observable behaviour rather than by inspecting
// internals: how many goroutines end up driving a run, and whether a tool
// executes after a cancel.

// TestResumeIsIdempotentUnderConcurrency is the I1 test for the resume path.
//
// A parked run has exactly one legal resume. Two concurrent Resume calls must
// not both start an execution goroutine: "thinking → thinking" is a legal
// self-transition (consecutive model rounds), so a naive state check would let
// both callers through and give the run two owners.
func TestResumeIsIdempotentUnderConcurrency(t *testing.T) {
	events := mem.NewEventStore()
	ctx := context.Background()
	const runID = "run-resume-race"

	seedParkedRun(t, events, runID, "ses-resume")

	h := &harness{
		events: events, outbox: mem.NewOutboxStore(),
		ckpts: mem.NewCheckpointStore(), idem: mem.NewIdempotencyStore(),
		tools: mem.NewToolRuntime(nil), provider: mem.NewProvider(mem.FinalRound("resumed answer")),
		guard: agent.NewPanicGuard(agent.GuardConfig{DumpDir: t.TempDir()}),
	}
	engine, err := New(engineConfig(), Dependencies{
		Events: h.events, Outbox: h.outbox, Checkpoints: h.ckpts,
		Idempotency: h.idem, Tools: h.tools, Provider: h.provider,
	}, h.guard)
	if err != nil {
		t.Fatal(err)
	}
	h.engine = engine
	defer engine.Close()

	// Park it, then resume from many goroutines at once.
	if _, err := engine.Recover(ctx); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	run, err := engine.GetRun(ctx, runID)
	if err != nil || run.State != types.StateWaitingUser {
		t.Fatalf("expected a parked run, got %s (err=%v)", run.State, err)
	}

	const racers = 8
	var accepted atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := engine.Resume(ctx, runID); err == nil {
				accepted.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := accepted.Load(); got != 1 {
		t.Errorf("accepted resumes = %d, want exactly 1: a run must have one owner (I1)", got)
	}

	// The run must finish coherently rather than being driven twice.
	idleCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := engine.WaitIdle(idleCtx); err != nil {
		t.Fatalf("WaitIdle: %v", err)
	}
	final, err := engine.GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if !final.State.Terminal() {
		t.Errorf("run state = %s after resuming, want a terminal state", final.State)
	}
	// Exactly one final answer must have been recorded: two owners would
	// produce two.
	answers := 0
	for _, typ := range h.events.TypesOf(runID) {
		if typ == types.EventFinalAnswer {
			answers++
		}
	}
	if answers != 1 {
		t.Errorf("final-answer events = %d, want 1 (two owners would each emit one)", answers)
	}
}

// TestCancelDuringRunStartsNoNewToolCalls is the I8 test.
//
// After a run is cancelled, no further tool call may be dispatched. The loop
// checks the context at every round boundary and before each dispatch, so a
// cancel that lands mid-round must stop the *next* call rather than letting the
// round run to completion.
func TestCancelDuringRunStartsNoNewToolCalls(t *testing.T) {
	// Many rounds, so a cancel lands while rounds remain.
	script := make([]ports.ProviderResponse, 0, 32)
	for i := 0; i < 32; i++ {
		script = append(script, mem.ToolCallRound(
			mem.NewCall("call-"+itoa(i), "counting_tool", nil)))
	}

	var dispatchCount atomic.Int64
	h := newHarness(t, script...)
	// Each call blocks briefly so the cancel lands while one is in flight.
	h.tools.Handler = func(ctx context.Context, _ ports.ToolRequest) (types.ToolResult, error) {
		dispatchCount.Add(1)
		select {
		case <-ctx.Done():
		case <-time.After(50 * time.Millisecond):
		}
		return types.ToolResult{Success: true, Content: "ok"}, nil
	}

	ctx := context.Background()
	handle, err := h.engine.Submit(ctx, types.SubmitRequest{Prompt: "loop with tools"})
	if err != nil {
		t.Fatal(err)
	}

	// Let a couple of calls run, then cancel.
	deadline := time.Now().Add(5 * time.Second)
	for dispatchCount.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if err := h.engine.Cancel(ctx, handle.RunID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if err := h.engine.WaitRun(ctx, handle.RunID); err != nil {
		t.Fatalf("WaitRun: %v", err)
	}

	afterCancel := dispatchCount.Load()
	// Give any stray dispatch a chance to appear: a leak here would be
	// asynchronous, so an immediate check could miss it.
	time.Sleep(300 * time.Millisecond)
	if got := dispatchCount.Load(); got != afterCancel {
		t.Errorf("tool calls dispatched after the run settled: %d then %d (I8: a cancelled run must start no new side effects)",
			afterCancel, got)
	}
	run, err := h.engine.GetRun(ctx, handle.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.State != types.StateCancelled {
		t.Errorf("state = %s, want cancelled", run.State)
	}
	// And it must not have burned every round.
	if afterCancel >= 32 {
		t.Errorf("dispatched %d tool calls; the cancel did not stop the loop (I8)", afterCancel)
	}
}

// TestCancelledRunCannotBeResumed checks the companion rule: a cancelled run is
// terminal, so no code path may put it back to work.
func TestCancelledRunCannotBeResumed(t *testing.T) {
	release := make(chan struct{})
	h := newHarness(t)
	h.engine.Close()

	p := mem.NewProvider(mem.FinalRound("never reached"))
	p.Block = release
	engine, err := New(engineConfig(), Dependencies{
		Events: h.events, Outbox: h.outbox, Checkpoints: h.ckpts,
		Idempotency: h.idem, Tools: h.tools, Provider: p,
	}, h.guard)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	defer close(release)

	ctx := context.Background()
	handle, err := engine.Submit(ctx, types.SubmitRequest{
		Prompt: "cancel me", SessionID: "ses-cancel-resume",
	})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for p.RoundCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if err := engine.Cancel(ctx, handle.RunID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if err := engine.WaitRun(ctx, handle.RunID); err != nil {
		t.Fatal(err)
	}

	err = engine.Resume(ctx, handle.RunID)
	if err == nil {
		t.Fatal("a cancelled run must not be resumable (I1/I8)")
	}
	if code := types.CodeOf(err); code != types.CodeTerminalState {
		t.Errorf("resume of a cancelled run: code = %s, want %s", code, types.CodeTerminalState)
	}
}

// seedParkedRun writes the durable log of a run that crashed while a
// non-idempotent tool call was in flight, which is what makes recovery park it.
func seedParkedRun(t *testing.T, events *mem.EventStore, runID, sessionID string) {
	t.Helper()
	mustAppend(t, events, runID, types.Event{
		Type: types.EventRunCreated, State: types.StateCreated, SessionID: sessionID,
	})
	mustAppend(t, events, runID, types.Event{
		Type: types.EventRunQueued, State: types.StateQueued, SessionID: sessionID,
	})
	mustAppend(t, events, runID, types.Event{
		Type: types.EventToolStarted, ToolCallID: "call-hang", ToolName: "terminal",
		State: types.StateExecuting,
	})
}
