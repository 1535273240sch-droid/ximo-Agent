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

// ---------------------------------------------------------------------------
// harness
// ---------------------------------------------------------------------------

// harness wires an engine over in-memory ports.
type harness struct {
	engine   *Engine
	events   *mem.EventStore
	outbox   *mem.OutboxStore
	ckpts    *mem.CheckpointStore
	idem     *mem.IdempotencyStore
	tools    *mem.ToolRuntime
	provider *mem.Provider
	guard    *agent.PanicGuard
}

// engineConfig returns a small, fast configuration for tests: the defaults with
// the caps reduced so limit behaviour is reachable without thousands of tasks.
func engineConfig() EngineConfig {
	cfg := DefaultEngineConfig()
	cfg.Scheduler.MaxRunningTasks = 8
	cfg.Scheduler.MaxConcurrentToolsPerSession = 4
	cfg.Admission.MaxRunningRuns = 8
	cfg.Admission.MaxRunningRunsPerSession = 4
	cfg.Scheduler.Limits = types.QueueLimits{
		MaxQueuedRuns:      64,
		MaxQueuedToolCalls: 64,
		MaxMemoryBytes:     64 << 20,
		MaxOutputBytes:     8 << 20,
		MaxRunDuration:     30 * time.Second,
	}
	cfg.MaxRecoveryAttempts = 2
	cfg.CrashDumpDir = "crashes"
	return cfg
}

// newHarness builds an engine with in-memory ports.
func newHarness(t *testing.T, script ...ports.ProviderResponse) *harness {
	t.Helper()
	h := &harness{
		events:   mem.NewEventStore(),
		outbox:   mem.NewOutboxStore(),
		ckpts:    mem.NewCheckpointStore(),
		idem:     mem.NewIdempotencyStore(),
		tools:    mem.NewToolRuntime(nil),
		provider: mem.NewProvider(script...),
		guard:    agent.NewPanicGuard(agent.GuardConfig{DumpDir: t.TempDir()}),
	}
	engine, err := New(engineConfig(), Dependencies{
		Events:      h.events,
		Outbox:      h.outbox,
		Checkpoints: h.ckpts,
		Idempotency: h.idem,
		Tools:       h.tools,
		Provider:    h.provider,
	}, h.guard)
	if err != nil {
		t.Fatalf("New engine: %v", err)
	}
	h.engine = engine
	t.Cleanup(engine.Close)
	return h
}

// ---------------------------------------------------------------------------
// submit / run lifecycle
// ---------------------------------------------------------------------------

// TestSubmitRunsToCompletion walks the happy path: submit, run, complete, and
// observe the durable event chain.
func TestSubmitRunsToCompletion(t *testing.T) {
	h := newHarness(t,
		mem.ToolCallRound(mem.NewCall("call-1", "file_read", nil)),
		mem.FinalRound("finished"),
	)

	ctx := context.Background()
	handle, err := h.engine.Submit(ctx, types.SubmitRequest{Prompt: "read the file"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if handle.RunID == "" {
		t.Fatal("Submit returned an empty run ID")
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
	if run.Answer != "finished" {
		t.Errorf("answer = %q, want %q", run.Answer, "finished")
	}

	// The durable log must contain the lifecycle boundaries.
	typesSeen := h.events.TypesOf(handle.RunID)
	for _, want := range []types.EventType{
		types.EventRunQueued, types.EventRunStateChanged,
		types.EventToolCallRequested, types.EventToolCompleted,
		types.EventFinalAnswer, types.EventRunCompleted,
	} {
		if !containsType(typesSeen, want) {
			t.Errorf("event log is missing %s; saw %v", want, typesSeen)
		}
	}
	// The log must contain logical boundaries only, never per-token rows.
	if containsType(typesSeen, types.EventTokenDelta) {
		t.Error("token deltas were written to the durable log; the log stores boundaries, not characters")
	}

	// The outbox must have mirrored the durable events.
	if h.outbox.Len() == 0 {
		t.Error("no events were mirrored to the outbox")
	}
}

// TestSubmitRejectsInvalidRequest checks that a request that cannot run is
// refused before it consumes any capacity.
func TestSubmitRejectsInvalidRequest(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if _, err := h.engine.Submit(ctx, types.SubmitRequest{}); err == nil {
		t.Error("expected an empty prompt to be rejected")
	}
	if _, err := h.engine.Submit(ctx, types.SubmitRequest{
		Prompt: "hi", Priority: "urgent",
	}); err == nil {
		t.Error("expected an unknown priority to be rejected")
	}
	if _, err := h.engine.Submit(ctx, types.SubmitRequest{
		Prompt: "hi", Effort: "ludicrous",
	}); err == nil {
		t.Error("expected an unknown reasoning effort to be rejected")
	}
	if st := h.engine.Stats(); st.Submitted != 0 {
		t.Errorf("submitted = %d, want 0: rejected requests must not be counted as accepted", st.Submitted)
	}
}

// TestSubmitRejectsWhenAdmissionFull checks the global admission ceiling: past
// it, Submit fails fast with a coded error rather than queueing.
func TestSubmitRejectsWhenAdmissionFull(t *testing.T) {
	// A provider that blocks until released holds every running slot.
	release := make(chan struct{})
	p := mem.NewProvider(mem.FinalRound("done"))
	p.Block = release

	cfg := engineConfig()
	cfg.Admission.MaxRunningRuns = 2
	cfg.Admission.MaxRunningRunsPerSession = 2
	cfg.Scheduler.MaxRunningTasks = 2

	h := &harness{
		events: mem.NewEventStore(), outbox: mem.NewOutboxStore(),
		ckpts: mem.NewCheckpointStore(), idem: mem.NewIdempotencyStore(),
		tools: mem.NewToolRuntime(nil), provider: p,
		guard: agent.NewPanicGuard(agent.GuardConfig{DumpDir: t.TempDir()}),
	}
	engine, err := New(cfg, Dependencies{
		Events: h.events, Outbox: h.outbox, Checkpoints: h.ckpts,
		Idempotency: h.idem, Tools: h.tools, Provider: h.provider,
	}, h.guard)
	if err != nil {
		t.Fatal(err)
	}
	h.engine = engine
	defer engine.Close()

	ctx := context.Background()
	// Two runs occupy the ceiling.
	for i := 0; i < 2; i++ {
		if _, err := engine.Submit(ctx, types.SubmitRequest{
			Prompt: "blocking task", SessionID: "session-a",
		}); err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
	}

	// Wait for both to be actually running, not merely accepted.
	deadline := time.Now().Add(3 * time.Second)
	for engine.admission.RunningRuns() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	_, err = engine.Submit(ctx, types.SubmitRequest{Prompt: "one too many", SessionID: "session-b"})
	if err == nil {
		t.Fatal("expected the third run to be rejected at the admission ceiling")
	}
	if code := types.CodeOf(err); code != types.CodeAdmissionRejected {
		t.Errorf("error code = %s, want %s", code, types.CodeAdmissionRejected)
	}
	// The rejection must be explainable to a user.
	if msg := ExplainRejection(err); msg == "" {
		t.Error("rejection has no user-facing explanation")
	}

	close(release)
	if err := engine.WaitIdle(context.Background()); err != nil {
		t.Fatalf("WaitIdle: %v", err)
	}
}

// TestCancelStopsRun checks that Cancel ends a run in the cancelled state and
// records it durably.
func TestCancelStopsRun(t *testing.T) {
	release := make(chan struct{})
	p := mem.NewProvider(mem.FinalRound("should not be reached"))
	p.Block = release
	defer close(release)

	h := newHarness(t)
	h.engine.deps.Provider = p
	// Rebuild the engine so the blocking provider is in place.
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
	handle, err := engine.Submit(ctx, types.SubmitRequest{Prompt: "long task"})
	if err != nil {
		t.Fatal(err)
	}
	// Let the run reach the provider round before cancelling.
	deadline := time.Now().Add(3 * time.Second)
	for p.RoundCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	if err := engine.Cancel(ctx, handle.RunID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if err := engine.WaitRun(ctx, handle.RunID); err != nil {
		t.Fatalf("WaitRun after cancel: %v", err)
	}

	run, err := engine.GetRun(ctx, handle.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.State != types.StateCancelled {
		t.Errorf("state = %s, want cancelled", run.State)
	}
	if !containsType(h.events.TypesOf(handle.RunID), types.EventRunCancelled) {
		t.Errorf("cancellation was not recorded durably; saw %v", h.events.TypesOf(handle.RunID))
	}

	// Cancelling an already-finished run is reported, not silently accepted.
	if err := engine.Cancel(ctx, handle.RunID); err == nil {
		t.Error("expected cancelling a terminal run to be reported")
	}
	// Cancelling an unknown run is a not-found error.
	if err := engine.Cancel(ctx, "run_does_not_exist"); types.CodeOf(err) != types.CodeNotFound {
		t.Errorf("cancel of an unknown run: code = %s, want %s", types.CodeOf(err), types.CodeNotFound)
	}
}

// TestGetRunFallsBackToEventLog checks that a run which is no longer in memory
// (because its process died, or because it completed and was evicted) can still
// be read back from the durable log.
func TestGetRunFallsBackToEventLog(t *testing.T) {
	h := newHarness(t, mem.FinalRound("persisted answer"))
	ctx := context.Background()

	handle, err := h.engine.Submit(ctx, types.SubmitRequest{Prompt: "persist me"})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.engine.WaitRun(ctx, handle.RunID); err != nil {
		t.Fatal(err)
	}
	// The completed run is dropped from memory on purpose.
	if _, inMemory := h.engine.runs[handle.RunID]; inMemory {
		t.Fatal("a terminal run should not be retained in memory")
	}

	run, err := h.engine.GetRun(ctx, handle.RunID)
	if err != nil {
		t.Fatalf("GetRun after eviction: %v", err)
	}
	if run.State != types.StateCompleted {
		t.Errorf("state = %s, want completed", run.State)
	}
	if run.Answer != "persisted answer" {
		t.Errorf("answer = %q, want it recovered from the event log", run.Answer)
	}
}

// TestToolStartedEventIsWrittenBeforeExecution is a regression test for a bug
// the crash test exposed: the loop recorded tool_call.requested but never
// tool_call.started, so recovery could not distinguish "never started" (safe to
// replay) from "started, outcome unknown" (must not be replayed). That
// distinction is what the acceptance criterion "a non-idempotent tool call is
// never repeated automatically" rests on.
func TestToolStartedEventIsWrittenBeforeExecution(t *testing.T) {
	h := newHarness(
		t,
		mem.ToolCallRound(mem.NewCall("call-1", "terminal", nil)),
		mem.FinalRound("done"),
	)

	// The tool checks the durable log from inside its own execution, which is
	// what proves the write happened *before* the call ran rather than after.
	var startedBeforeExecution bool
	h.tools.Handler = func(_ context.Context, _ ports.ToolRequest) (types.ToolResult, error) {
		for _, typ := range h.events.TypesOf(currentRunID) {
			if typ == types.EventToolStarted {
				startedBeforeExecution = true
			}
		}
		return types.ToolResult{Success: true, Content: "ran"}, nil
	}

	ctx := context.Background()
	handle, err := h.engine.Submit(ctx, types.SubmitRequest{Prompt: "run a tool"})
	if err != nil {
		t.Fatal(err)
	}
	currentRunID = handle.RunID
	if err := h.engine.WaitRun(ctx, handle.RunID); err != nil {
		t.Fatal(err)
	}

	if !startedBeforeExecution {
		t.Error("tool_call.started was not durable before the tool executed; " +
			"crash recovery would misclassify an interrupted call as never-started")
	}

	// The three tool-call events must all be present, in causal order.
	got := h.events.TypesOf(handle.RunID)
	idxRequested, idxStarted, idxCompleted := -1, -1, -1
	for i, typ := range got {
		switch typ {
		case types.EventToolCallRequested:
			if idxRequested < 0 {
				idxRequested = i
			}
		case types.EventToolStarted:
			if idxStarted < 0 {
				idxStarted = i
			}
		case types.EventToolCompleted:
			if idxCompleted < 0 {
				idxCompleted = i
			}
		}
	}
	if idxStarted < 0 {
		t.Errorf("the log has no tool_call.started marker; recovery would be blind: %v", got)
	}
	if idxCompleted < 0 {
		t.Errorf("the log has no tool_call.completed marker: %v", got)
	}
	if !(idxRequested >= 0 && idxStarted > idxRequested && idxCompleted > idxStarted) {
		t.Errorf("tool-call events are out of order: requested=%d started=%d completed=%d (%v)",
			idxRequested, idxStarted, idxCompleted, got)
	}
}

// currentRunID lets the tool handler in the regression test above find the run
// whose log it should inspect. The handler runs after Submit has assigned the
// ID, so a package-level slot is sufficient and avoids threading the ID through
// the in-memory tool runtime.
var currentRunID string

// TestAdmitToolCallEnforcesLayerTwoAndThree checks the inbound hook task 04
// calls before executing a tool. It must refuse a call that would exceed the
// per-session or per-resource ceiling, so the runtime can fail early instead of
// discovering the limit inside the scheduler.
func TestAdmitToolCallEnforcesLayerTwoAndThree(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// A call with headroom must be admitted.
	if err := h.engine.AdmitToolCall(ctx, ToolAdmissionRequest{
		SessionID: "ses-ok", RunID: "run-1", ToolCallID: "call-1", ToolName: "file_read",
	}); err != nil {
		t.Fatalf("a call with headroom was refused: %v", err)
	}

	// An unknown resource class is a caller bug and must be reported as such.
	if err := h.engine.AdmitToolCall(ctx, ToolAdmissionRequest{
		ToolName: "file_read", Resource: "teleporter",
	}); types.CodeOf(err) != types.CodeInvalidArgument {
		t.Errorf("unknown resource: code = %s, want %s", types.CodeOf(err), types.CodeInvalidArgument)
	}

	// A missing tool name is rejected rather than guessed at.
	if err := h.engine.AdmitToolCall(ctx, ToolAdmissionRequest{RunID: "run-1"}); types.CodeOf(err) != types.CodeInvalidArgument {
		t.Errorf("missing tool name: code = %s, want %s", types.CodeOf(err), types.CodeInvalidArgument)
	}

	// A cancelled context is reported as a cancellation.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := h.engine.AdmitToolCall(cancelled, ToolAdmissionRequest{ToolName: "file_read"}); !types.IsCancelled(err) {
		t.Errorf("cancelled context: err = %v, want a cancellation", err)
	}

	// Saturate the vision pool (capacity 8) and confirm the ceiling bites.
	for i := 0; i < h.engine.sched.Resources().Capacity(types.ResourceVision); i++ {
		if _, ok := h.engine.sched.Resources().TryAcquire(types.ResourceVision); !ok {
			t.Fatalf("could not acquire vision lease %d", i)
		}
	}
	err := h.engine.AdmitToolCall(ctx, ToolAdmissionRequest{
		SessionID: "ses-ok", RunID: "run-1", ToolCallID: "call-2", ToolName: "vision_describe",
	})
	if err == nil {
		t.Fatal("expected a saturated resource class to refuse the call")
	}
	if code := types.CodeOf(err); code != types.CodeResourceUnavailable {
		t.Errorf("saturated resource: code = %s, want %s", code, types.CodeResourceUnavailable)
	}
	// A different, unsaturated class is unaffected.
	if err := h.engine.AdmitToolCall(ctx, ToolAdmissionRequest{
		SessionID: "ses-ok", RunID: "run-1", ToolCallID: "call-3", ToolName: "file_read",
	}); err != nil {
		t.Errorf("an unrelated resource class should still admit: %v", err)
	}

	// Saturation must be readable, so a runtime can defer instead of being
	// refused.
	var sawSaturatedVision bool
	for _, s := range h.engine.Saturation() {
		if s.Resource == types.ResourceVision && s.Saturated {
			sawSaturatedVision = true
		}
	}
	if !sawSaturatedVision {
		t.Error("Saturation did not report the vision class as saturated")
	}
}

// TestAdmitToolCallEnforcesSessionCeiling checks the per-session limit through
// the inbound hook.
//
// The session's in-flight count is driven by real runs holding real tool calls
// rather than by manipulating the fair queue directly: the scheduler's own
// dispatcher is running, so a test that dequeued by hand would race it. Driving
// it through Submit exercises the same counter on the path production uses.
func TestAdmitToolCallEnforcesSessionCeiling(t *testing.T) {
	const session = "ses-busy"

	// One blocking tool call per run; the session cap is 4 in the test config.
	release := make(chan struct{})
	h := newHarness(t,
		mem.ToolCallRound(mem.NewCall("c1", "terminal", nil)),
		mem.ToolCallRound(mem.NewCall("c2", "terminal", nil)),
		mem.ToolCallRound(mem.NewCall("c3", "terminal", nil)),
		mem.ToolCallRound(mem.NewCall("c4", "terminal", nil)),
	)
	h.tools.Handler = func(ctx context.Context, _ ports.ToolRequest) (types.ToolResult, error) {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return types.ToolResult{Success: true, Content: "ok"}, nil
	}

	ctx := context.Background()
	limit := h.engine.cfg.Scheduler.MaxConcurrentToolsPerSession
	for i := 0; i < limit; i++ {
		if _, err := h.engine.Submit(ctx, types.SubmitRequest{
			Prompt: "hold a tool slot", SessionID: session,
		}); err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
	}

	// Wait until every slot is genuinely held.
	deadline := time.Now().Add(10 * time.Second)
	for h.engine.sched.FairQueue().InFlight(session) < limit && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := h.engine.sched.FairQueue().InFlight(session); got != limit {
		close(release)
		t.Fatalf("in-flight = %d, want the session limit %d", got, limit)
	}

	err := h.engine.AdmitToolCall(ctx, ToolAdmissionRequest{
		SessionID: session, RunID: "run-over", ToolCallID: "call-over", ToolName: "terminal",
	})
	if err == nil {
		close(release)
		t.Fatal("expected the per-session ceiling to refuse the call")
	}
	if code := types.CodeOf(err); code != types.CodeAdmissionRejected {
		t.Errorf("session ceiling: code = %s, want %s", code, types.CodeAdmissionRejected)
	}
	// A different session is unaffected.
	if err := h.engine.AdmitToolCall(ctx, ToolAdmissionRequest{
		SessionID: "ses-free", RunID: "run-2", ToolCallID: "call-ok", ToolName: "terminal",
	}); err != nil {
		t.Errorf("a different session should be admitted: %v", err)
	}

	// Releasing the tools lets the runs finish, after which the session has
	// capacity again.
	close(release)
	idleCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := h.engine.WaitIdle(idleCtx); err != nil {
		t.Fatalf("WaitIdle: %v", err)
	}
	if got := h.engine.sched.FairQueue().InFlight(session); got != 0 {
		t.Errorf("in-flight = %d after the runs finished, want 0", got)
	}
	if err := h.engine.AdmitToolCall(ctx, ToolAdmissionRequest{
		SessionID: session, RunID: "run-3", ToolCallID: "call-again", ToolName: "terminal",
	}); err != nil {
		t.Errorf("after the runs finished the session should have capacity: %v", err)
	}
}

// TestFailoverExplainRejectionCoversEveryCapacityCode checks that every capacity
// rejection the engine can produce has a user-facing explanation: an
// unexplained failure is indistinguishable from a bug to the person seeing it.
func TestExplainRejectionCoversEveryCapacityCode(t *testing.T) {
	codes := []types.ErrorCode{
		types.CodeAdmissionRejected,
		types.CodeQueueFull,
		types.CodeResourceUnavailable,
		types.CodeMemoryLimitExceeded,
		types.CodeRunDurationExceeded,
		types.CodeOutputLimitExceeded,
		types.CodeEngineClosed,
	}
	for _, code := range codes {
		msg := ExplainRejection(types.NewError(code, "details"))
		if msg == "" {
			t.Errorf("code %s has no user-facing explanation", code)
		}
		// The raw code string must not be what the user reads.
		if msg == string(code) {
			t.Errorf("code %s falls through to the raw error text", code)
		}
	}
	if got := ExplainRejection(nil); got != "" {
		t.Errorf("ExplainRejection(nil) = %q, want empty", got)
	}
}

// TestSubmitIsRaceFree hammers Submit and Cancel from many goroutines so the
// race detector covers the engine's bookkeeping.
func TestSubmitIsRaceFree(t *testing.T) {
	h := newHarness(t)
	h.engine.deps.Provider = mem.NewProvider(mem.FinalRound("ok"))
	ctx := context.Background()

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 4; i++ {
				handle, err := h.engine.Submit(ctx, types.SubmitRequest{
					Prompt: "concurrent task", SessionID: "s" + itoa(g%2),
				})
				if err != nil {
					continue
				}
				_, _ = h.engine.GetRun(ctx, handle.RunID)
				_, _ = h.engine.Events(ctx, handle.RunID, 0)
			}
		}(g)
	}
	wg.Wait()

	idleCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := h.engine.WaitIdle(idleCtx); err != nil {
		t.Fatalf("WaitIdle: %v", err)
	}
	if st := h.engine.Stats(); st.Active != 0 {
		t.Errorf("active runs = %d after everything finished, want 0", st.Active)
	}
}

// ---------------------------------------------------------------------------
// events and backpressure
// ---------------------------------------------------------------------------

// TestEventsStreamDeliversDurableHistory checks that Events replays the durable
// log and terminates once the run is terminal.
func TestEventsStreamDeliversDurableHistory(t *testing.T) {
	h := newHarness(t, mem.FinalRound("streamed answer"))
	ctx := context.Background()

	handle, err := h.engine.Submit(ctx, types.SubmitRequest{Prompt: "produce events"})
	if err != nil {
		t.Fatal(err)
	}
	ch, err := h.engine.Events(ctx, handle.RunID, 0)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}

	var got []types.Event
	deadline := time.After(10 * time.Second)
collect:
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				break collect
			}
			got = append(got, ev)
		case <-deadline:
			t.Fatal("event stream did not close after the run finished")
		}
	}

	if len(got) == 0 {
		t.Fatal("event stream delivered nothing")
	}
	if !containsType(eventTypesOf(got), types.EventFinalAnswer) {
		t.Errorf("stream is missing the final answer; saw %v", eventTypesOf(got))
	}
	// Sequence numbers must be strictly ascending: that is what lets a UI
	// reconnect with afterSeq without gaps or duplicates.
	var last uint64
	for _, ev := range got {
		if ev.Seq != 0 && ev.Seq <= last {
			t.Errorf("event sequence went backwards: %d after %d", ev.Seq, last)
		}
		if ev.Seq > last {
			last = ev.Seq
		}
	}
}

// TestEventsAfterSeqResumesFromSequence checks that afterSeq is honoured, which
// is the mechanism a reconnecting UI relies on.
func TestEventsAfterSeqResumesFromSequence(t *testing.T) {
	h := newHarness(t, mem.FinalRound("answer"))
	ctx := context.Background()

	handle, err := h.engine.Submit(ctx, types.SubmitRequest{Prompt: "produce events"})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.engine.WaitRun(ctx, handle.RunID); err != nil {
		t.Fatal(err)
	}

	all := h.events.All(handle.RunID)
	if len(all) < 3 {
		t.Fatalf("expected several durable events, got %d", len(all))
	}
	cut := all[1].Seq

	ch, err := h.engine.Events(ctx, handle.RunID, cut)
	if err != nil {
		t.Fatal(err)
	}
	var got []uint64
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				goto done
			}
			got = append(got, ev.Seq)
		case <-deadline:
			goto done
		}
	}
done:
	for _, seq := range got {
		if seq != 0 && seq <= cut {
			t.Errorf("event with seq %d was delivered although afterSeq was %d", seq, cut)
		}
	}
	if len(got) == 0 {
		t.Error("expected events after the cut")
	}
}

// TestCoalescerMergesTokenDeltas is the backpressure test: many ephemeral deltas
// must be folded into far fewer frames, and the merge count must be conserved.
func TestCoalescerMergesTokenDeltas(t *testing.T) {
	cfg := types.DefaultBackpressureConfig()
	cfg.MaxCoalesce = 32
	bus := newEventBus(cfg)
	c := newCoalescer(cfg, bus)
	defer c.Close()

	sub, err := bus.Subscribe("run-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer bus.Unsubscribe(sub)

	// 1000 deltas, as the task book's example describes.
	const deltas = 1000
	for i := 0; i < deltas; i++ {
		c.Publish(types.Event{
			RunID: "run-1", Type: types.EventTokenDelta, Timestamp: time.Now(),
			Data: map[string]any{"content": "d"},
		})
	}
	// Force the trailing partial frame out.
	c.flush()

	var frames int
	var merged int
	deadline := time.After(2 * time.Second)
drain:
	for {
		select {
		case ev := <-sub.ch:
			frames++
			merged += ev.Coalesced
		case <-deadline:
			break drain
		default:
			break drain
		}
	}

	if merged != deltas {
		t.Errorf("merged event count = %d, want %d (no delta may be lost)", merged, deltas)
	}
	if frames == 0 {
		t.Fatal("no frames were delivered")
	}
	// The task book's target is 20-50 frames for 1000 deltas; a 32-event fold
	// yields ~32 frames, so assert the documented ceiling generously.
	if frames > 60 {
		t.Errorf("frames = %d for %d deltas, want at most ~50: coalescing is not effective", frames, deltas)
	}
}

// TestCoalescerNeverMergesDurableEvents checks that a durable event is delivered
// immediately and is never folded, which is what keeps the log and the UI in
// agreement.
func TestCoalescerNeverMergesDurableEvents(t *testing.T) {
	cfg := types.DefaultBackpressureConfig()
	bus := newEventBus(cfg)
	c := newCoalescer(cfg, bus)
	defer c.Close()

	sub, err := bus.Subscribe("run-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer bus.Unsubscribe(sub)

	for _, typ := range []types.EventType{
		types.EventRunStateChanged, types.EventToolStarted, types.EventToolCompleted,
		types.EventToolFailed, types.EventFinalAnswer, types.EventError,
		types.EventCheckpointCreated, types.EventCancellation,
	} {
		c.Publish(types.Event{RunID: "run-1", Type: typ, Seq: 1, Timestamp: time.Now()})
		select {
		case ev := <-sub.ch:
			if ev.Coalesced != 0 {
				t.Errorf("%s was merged (%d events); durable events must be delivered as-is",
					typ, ev.Coalesced)
			}
			if ev.Type != typ {
				t.Errorf("delivered %s, want %s", ev.Type, typ)
			}
		case <-time.After(time.Second):
			t.Errorf("%s was not delivered immediately", typ)
		}
	}
}

// TestSubscriberSlowIsDisconnectedNotBlocking checks the core backpressure
// property: a consumer that cannot keep up is dropped rather than allowed to
// block the producer, because blocking would stall the engine.
func TestSubscriberSlowIsDisconnectedNotBlocking(t *testing.T) {
	cfg := types.DefaultBackpressureConfig()
	cfg.StreamBuffer = 4
	bus := newEventBus(cfg)
	defer bus.Close()

	// The subscriber is registered in the bus's own map, so it stays alive
	// without a local reference; its channel is deliberately never read.
	if _, err := bus.Subscribe("run-1", 0); err != nil {
		t.Fatal(err)
	}

	// Never read from the subscriber's channel, then overflow its buffer.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			bus.Publish(types.Event{RunID: "run-1", Type: types.EventRunStateChanged, Seq: uint64(i + 1)})
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Publish blocked on a slow subscriber; the producer must never block")
	}
	if st := bus.Stats(); st.Dropped == 0 {
		t.Error("expected the slow subscriber's overflow to be counted as dropped")
	}
}

// ---------------------------------------------------------------------------
// crash recovery
// ---------------------------------------------------------------------------

// TestRecoveryClassifiesIdempotentCallsAsReplayable checks the classification
// rule for a call that was declared idempotent.
func TestRecoveryClassifiesIdempotentCallsAsReplayable(t *testing.T) {
	events := mem.NewEventStore()
	ctx := context.Background()

	// A run that died mid-tool-call: the call was started but never completed.
	mustAppend(t, events, "run-1", types.Event{
		Type: types.EventRunQueued, State: types.StateQueued, SessionID: "ses-1",
	})
	mustAppend(t, events, "run-1", types.Event{
		Type: types.EventRunStateChanged, State: types.StateExecuting, SessionID: "ses-1",
		Round: 2,
	})
	mustAppend(t, events, "run-1", types.Event{
		Type: types.EventToolCallRequested, ToolCallID: "call-read", ToolName: "file_read",
		State: types.StateExecuting,
	})
	mustAppend(t, events, "run-1", types.Event{
		Type: types.EventToolStarted, ToolCallID: "call-read", ToolName: "file_read",
		State: types.StateExecuting,
	})

	r, err := NewRecovery(RecoveryConfig{
		Events:      events,
		Checkpoints: mem.NewCheckpointStore(),
		Idempotency: mem.NewIdempotencyStore(),
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := r.Inspect(ctx, "run-1")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if plan.Decision != ResumeAuto {
		t.Errorf("decision = %s, want %s (an idempotent call may be replayed): %v",
			plan.Decision, ResumeAuto, plan.Notes)
	}
	if len(plan.UncertainToolCalls) != 0 {
		t.Errorf("uncertain calls = %v, want none for an idempotent call", plan.UncertainToolCalls)
	}
	if len(plan.ReplayableToolCalls) != 1 {
		t.Errorf("replayable calls = %v, want the one idempotent call", plan.ReplayableToolCalls)
	}
	if !plan.SequenceOK {
		t.Error("sequence verification failed on a well-formed log")
	}
}

// TestRecoveryFlagsNonIdempotentCallAsUncertain is the acceptance criterion
// "a non-idempotent tool call is never repeated automatically after a crash".
func TestRecoveryFlagsNonIdempotentCallAsUncertain(t *testing.T) {
	events := mem.NewEventStore()
	ctx := context.Background()

	mustAppend(t, events, "run-2", types.Event{
		Type: types.EventRunQueued, State: types.StateQueued, SessionID: "ses-1",
	})
	mustAppend(t, events, "run-2", types.Event{
		Type: types.EventRunStateChanged, State: types.StateExecuting, SessionID: "ses-1",
	})
	// terminal is a non-idempotent tool in the default classification table.
	mustAppend(t, events, "run-2", types.Event{
		Type: types.EventToolStarted, ToolCallID: "call-term", ToolName: "terminal",
		State: types.StateExecuting,
	})

	r, _ := NewRecovery(RecoveryConfig{
		Events: events, Checkpoints: mem.NewCheckpointStore(),
		Idempotency: mem.NewIdempotencyStore(),
	})
	plan, err := r.Inspect(ctx, "run-2")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if plan.Decision != ResumeAfterConfirm {
		t.Fatalf("decision = %s, want %s: an interrupted non-idempotent call must not be auto-replayed",
			plan.Decision, ResumeAfterConfirm)
	}
	if len(plan.UncertainToolCalls) != 1 || plan.UncertainToolCalls[0] != "call-term" {
		t.Errorf("uncertain calls = %v, want [call-term]", plan.UncertainToolCalls)
	}

	// The park transition must carry the uncertain calls and a user-facing
	// reason, so the UI can ask the right question.
	tr := plan.PlanToWaiting()
	if tr.To != types.StateWaitingUser {
		t.Errorf("park target = %s, want waiting_user", tr.To)
	}
	if len(tr.UncertainToolCalls) != 1 {
		t.Errorf("park transition lost the uncertain calls: %v", tr.UncertainToolCalls)
	}
	if !strings.Contains(tr.WaitingReason, "call-term") {
		t.Errorf("park reason does not name the uncertain call: %q", tr.WaitingReason)
	}
}

// TestRecoveryTreatsNeverStartedCallAsReplayable checks the subtle case: a
// non-idempotent call that never started cannot have taken effect, so replaying
// it is safe even though its class says otherwise.
func TestRecoveryTreatsNeverStartedCallAsReplayable(t *testing.T) {
	events := mem.NewEventStore()
	ctx := context.Background()

	mustAppend(t, events, "run-3", types.Event{
		Type: types.EventRunQueued, State: types.StateQueued, SessionID: "ses-1",
	})
	// Requested but never started: the crash landed between the model round and
	// the dispatch.
	mustAppend(t, events, "run-3", types.Event{
		Type: types.EventToolCallRequested, ToolCallID: "call-term", ToolName: "terminal",
		State: types.StateThinking,
	})

	r, _ := NewRecovery(RecoveryConfig{
		Events: events, Checkpoints: mem.NewCheckpointStore(),
		Idempotency: mem.NewIdempotencyStore(),
	})
	plan, err := r.Inspect(ctx, "run-3")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if plan.Decision != ResumeAuto {
		t.Errorf("decision = %s, want %s: a call that never started cannot have taken effect",
			plan.Decision, ResumeAuto)
	}
}

// TestRecoveryIgnoresCompletedCalls checks that a call with a completion event
// is not treated as incomplete.
func TestRecoveryIgnoresCompletedCalls(t *testing.T) {
	events := mem.NewEventStore()
	ctx := context.Background()

	mustAppend(t, events, "run-4", types.Event{
		Type: types.EventRunQueued, State: types.StateQueued, SessionID: "ses-1",
	})
	mustAppend(t, events, "run-4", types.Event{
		Type: types.EventToolStarted, ToolCallID: "call-1", ToolName: "terminal",
		State: types.StateExecuting,
	})
	mustAppend(t, events, "run-4", types.Event{
		Type: types.EventToolCompleted, ToolCallID: "call-1", ToolName: "terminal",
		State: types.StateExecuting,
	})

	r, _ := NewRecovery(RecoveryConfig{
		Events: events, Checkpoints: mem.NewCheckpointStore(),
		Idempotency: mem.NewIdempotencyStore(),
	})
	plan, err := r.Inspect(ctx, "run-4")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if len(plan.Incomplete) != 0 {
		t.Errorf("incomplete calls = %+v, want none: the call has a completion event", plan.Incomplete)
	}
	if plan.Decision != ResumeAuto {
		t.Errorf("decision = %s, want %s", plan.Decision, ResumeAuto)
	}
}

// TestRecoveryRejectsGappedEventLog checks the "verify event sequence" step: a
// log with a hole, a duplicate, or a backwards jump cannot be trusted, so
// recovery refuses to resume from it. The sequence checks are exercised directly
// because a real store cannot produce a corrupt sequence on demand.
func TestRecoveryRejectsGappedEventLog(t *testing.T) {
	// A contiguous sequence is sound.
	contiguous := []types.Event{
		{Seq: 1, Type: types.EventRunQueued},
		{Seq: 2, Type: types.EventRunStateChanged},
		{Seq: 3, Type: types.EventToolStarted},
	}
	if ok, last := verifySequence(contiguous); !ok {
		t.Errorf("a contiguous log was reported as unsound (last=%d)", last)
	} else if last != 3 {
		t.Errorf("last sequence = %d, want 3", last)
	}

	// A hole means a write was lost, so the log underspecifies what happened.
	gapped := []types.Event{
		{Seq: 1, Type: types.EventRunQueued},
		{Seq: 2, Type: types.EventRunStateChanged},
		{Seq: 5, Type: types.EventToolStarted},
	}
	if ok, last := verifySequence(gapped); ok {
		t.Error("a gapped sequence was reported as sound; recovery must refuse to resume from it")
	} else if last != 5 {
		t.Errorf("last sequence = %d, want 5", last)
	}

	// A duplicate means a write was replayed, so the log overspecifies it.
	dup := []types.Event{{Seq: 1}, {Seq: 2}, {Seq: 2}}
	if ok, _ := verifySequence(dup); ok {
		t.Error("a duplicated sequence was reported as sound")
	}

	// An out-of-order sequence is equally untrustworthy.
	backwards := []types.Event{{Seq: 3}, {Seq: 1}}
	if ok, _ := verifySequence(backwards); ok {
		t.Error("a descending sequence was reported as sound")
	}

	// A zero sequence means the event was never durably assigned a position.
	unassigned := []types.Event{{Seq: 1}, {Seq: 0}}
	if ok, _ := verifySequence(unassigned); ok {
		t.Error("an event with no assigned sequence was reported as sound")
	}
}

// TestRecoveryFailsRunOnCorruptLog checks that a corrupt sequence makes
// recovery mark the run failed rather than resume it.
func TestRecoveryFailsRunOnCorruptLog(t *testing.T) {
	events := mem.NewEventStore()
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		mustAppend(t, events, "run-corrupt", types.Event{
			Type: types.EventRunStateChanged, State: types.StateThinking, SessionID: "ses-1",
		})
	}

	// Wrap the store so the sequence appears to have a hole, modelling a torn
	// write that a real store could produce after a crash.
	r, err := NewRecovery(RecoveryConfig{
		Events:      &corruptingStore{EventStore: events, breakAfter: 1},
		Checkpoints: mem.NewCheckpointStore(),
		Idempotency: mem.NewIdempotencyStore(),
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := r.Inspect(ctx, "run-corrupt")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if plan.SequenceOK {
		t.Fatal("a corrupt sequence was reported as sound")
	}
	if plan.Decision != MarkFailed {
		t.Errorf("decision = %s, want %s for a corrupt log", plan.Decision, MarkFailed)
	}
	if !strings.Contains(strings.Join(plan.Notes, " "), "gap") {
		t.Errorf("plan notes do not explain the gap: %v", plan.Notes)
	}
}

// corruptingStore wraps an EventStore and rewrites sequence numbers so a gap
// appears, which is how a torn write presents itself to recovery.
type corruptingStore struct {
	*mem.EventStore
	breakAfter int
}

// Read implements ports.EventStore, perturbing the sequence past breakAfter.
func (c *corruptingStore) Read(ctx context.Context, runID string, afterSeq uint64, limit int) ([]types.Event, error) {
	events, err := c.EventStore.Read(ctx, runID, afterSeq, limit)
	if err != nil {
		return nil, err
	}
	for i := range events {
		if events[i].Seq > uint64(c.breakAfter) {
			// Skip a number, as a lost write would.
			events[i].Seq++
		}
	}
	return events, nil
}

// TestRecoverySkipsTerminalRuns checks that a run which already finished needs
// no recovery work.
func TestRecoverySkipsTerminalRuns(t *testing.T) {
	events := mem.NewEventStore()
	ctx := context.Background()

	mustAppend(t, events, "run-6", types.Event{
		Type: types.EventRunQueued, State: types.StateQueued, SessionID: "ses-1",
	})
	mustAppend(t, events, "run-6", types.Event{
		Type: types.EventRunStateChanged, State: types.StateThinking, SessionID: "ses-1",
	})
	mustAppend(t, events, "run-6", types.Event{
		Type: types.EventRunCompleted, State: types.StateCompleted, SessionID: "ses-1",
	})

	r, _ := NewRecovery(RecoveryConfig{
		Events: events, Checkpoints: mem.NewCheckpointStore(),
		Idempotency: mem.NewIdempotencyStore(),
	})
	plans, err := r.Scan(ctx)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(plans) != 1 {
		t.Fatalf("plans = %d, want 1", len(plans))
	}
	if plans[0].Decision != Skip {
		t.Errorf("decision = %s, want %s", plans[0].Decision, Skip)
	}
}

// TestRecoveryFindsLastSafeCheckpoint checks the "locate last safe checkpoint"
// step, including that a checkpoint marked unsafe is skipped.
func TestRecoveryFindsLastSafeCheckpoint(t *testing.T) {
	events := mem.NewEventStore()
	ckpts := mem.NewCheckpointStore()
	ctx := context.Background()

	mustAppend(t, events, "run-7", types.Event{
		Type: types.EventRunQueued, State: types.StateQueued, SessionID: "ses-1",
	})
	mustAppend(t, events, "run-7", types.Event{
		Type: types.EventRunStateChanged, State: types.StateThinking, SessionID: "ses-1",
	})
	mustAppend(t, events, "run-7", types.Event{
		Type: types.EventCheckpointCreated, State: types.StateThinking, SessionID: "ses-1",
	})

	safeID, _ := ckpts.Save(ctx, ports.Checkpoint{
		RunID: "run-7", Seq: 2, Round: 1, State: types.StateThinking,
	})
	unsafeID, _ := ckpts.Save(ctx, ports.Checkpoint{
		RunID: "run-7", Seq: 3, Round: 2, State: types.StateExecuting,
	})
	// The newest checkpoint was taken mid-way through a non-idempotent call, so
	// it must not be resumed from.
	if err := ckpts.MarkUnsafe(ctx, "run-7", unsafeID, "taken during a non-idempotent call"); err != nil {
		t.Fatal(err)
	}

	r, _ := NewRecovery(RecoveryConfig{
		Events: events, Checkpoints: ckpts, Idempotency: mem.NewIdempotencyStore(),
	})
	plan, err := r.Inspect(ctx, "run-7")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if plan.CheckpointSeq != 2 {
		t.Errorf("checkpoint seq = %d, want 2 (the safe one, not the unsafe one at 3)", plan.CheckpointSeq)
	}
	_ = safeID
	if !strings.Contains(strings.Join(plan.Notes, " "), "safe checkpoint") {
		t.Errorf("recovery notes do not mention the checkpoint: %v", plan.Notes)
	}
}

// TestRecoveryHonoursAttemptBudget checks that a run which crashes repeatedly is
// eventually failed rather than retried forever across restarts.
func TestRecoveryHonoursAttemptBudget(t *testing.T) {
	events := mem.NewEventStore()
	ctx := context.Background()

	mustAppend(t, events, "run-8", types.Event{
		Type: types.EventRunQueued, State: types.StateQueued, SessionID: "ses-1",
	})
	mustAppend(t, events, "run-8", types.Event{
		Type: types.EventRunStateChanged, State: types.StateThinking, SessionID: "ses-1",
	})

	r, err := NewRecovery(RecoveryConfig{
		Events: events, Checkpoints: mem.NewCheckpointStore(),
		Idempotency:         mem.NewIdempotencyStore(),
		MaxRecoveryAttempts: 2,
		attempts:            func(string) int { return 2 },
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := r.Inspect(ctx, "run-8")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if plan.Decision != MarkFailed {
		t.Errorf("decision = %s, want %s once the attempt budget is spent", plan.Decision, MarkFailed)
	}
	if !strings.Contains(strings.Join(plan.Notes, " "), "recovery budget") {
		t.Errorf("plan notes do not explain the budget: %v", plan.Notes)
	}
}

// TestEngineRecoverParksNonIdempotentRunForConfirmation is the end-to-end crash
// recovery test: an in-flight run with an interrupted non-idempotent call is
// parked in waiting_user and requires explicit resume.
func TestEngineRecoverParksNonIdempotentRunForConfirmation(t *testing.T) {
	// Seed a durable log describing a run that died mid-tool-call.
	events := mem.NewEventStore()
	ctx := context.Background()
	const runID = "run-crashed"

	mustAppend(t, events, runID, types.Event{
		Type: types.EventRunCreated, State: types.StateCreated, SessionID: "ses-crash",
	})
	mustAppend(t, events, runID, types.Event{
		Type: types.EventRunQueued, State: types.StateQueued, SessionID: "ses-crash",
	})
	mustAppend(t, events, runID, types.Event{
		Type: types.EventRunStateChanged, State: types.StateThinking, SessionID: "ses-crash",
		Round: 3,
	})
	mustAppend(t, events, runID, types.Event{
		Type: types.EventToolStarted, ToolCallID: "call-term", ToolName: "terminal",
		State: types.StateExecuting,
	})

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

	plans, err := engine.Recover(ctx)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if len(plans) != 1 {
		t.Fatalf("plans = %d, want 1", len(plans))
	}
	if plans[0].Decision != ResumeAfterConfirm {
		t.Fatalf("decision = %s, want %s: %v", plans[0].Decision, ResumeAfterConfirm, plans[0].Notes)
	}

	run, err := engine.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun after recovery: %v", err)
	}
	if run.State != types.StateWaitingUser {
		t.Fatalf("state = %s, want waiting_user after recovery", run.State)
	}
	if len(run.UncertainToolCalls) == 0 {
		t.Error("the run did not record which tool calls are uncertain")
	}
	if run.WaitingReason == "" {
		t.Error("the run has no user-facing reason for waiting")
	}

	// The tool must not have been executed automatically.
	if h.tools.CallCount() != 0 {
		t.Errorf("tool calls executed during recovery = %d, want 0", h.tools.CallCount())
	}
	// The park must be recorded durably.
	if !containsType(h.events.TypesOf(runID), types.EventUserInputRequired) {
		t.Errorf("the waiting_user park was not recorded durably; saw %v", h.events.TypesOf(runID))
	}

	// Only an explicit Resume continues the run, and that decision is recorded.
	if err := engine.Resume(ctx, runID); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if !containsType(h.events.TypesOf(runID), types.EventUserInputReceived) {
		t.Errorf("the resume approval was not recorded durably; saw %v", h.events.TypesOf(runID))
	}
}

// TestEngineRecoverAutoParksReplayableRun checks the other recovery branch: a run
// whose incomplete calls are all replayable is parked ready to resume, without
// requiring a confirmation question.
func TestEngineRecoverAutoParksReplayableRun(t *testing.T) {
	events := mem.NewEventStore()
	ctx := context.Background()
	const runID = "run-replayable"

	mustAppend(t, events, runID, types.Event{
		Type: types.EventRunQueued, State: types.StateQueued, SessionID: "ses-r",
	})
	mustAppend(t, events, runID, types.Event{
		Type: types.EventToolStarted, ToolCallID: "call-read", ToolName: "file_read",
		State: types.StateExecuting,
	})

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
	h.engine = engine
	defer engine.Close()

	plans, err := engine.Recover(ctx)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if len(plans) != 1 || plans[0].Decision != ResumeAuto {
		t.Fatalf("plans = %+v, want one ResumeAuto plan", plans)
	}
	run, err := engine.GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.State != types.StateWaitingUser {
		t.Errorf("state = %s, want waiting_user", run.State)
	}
	if len(run.UncertainToolCalls) != 0 {
		t.Errorf("uncertain calls = %v, want none for a replayable plan", run.UncertainToolCalls)
	}
}

// TestRecoverIsIdempotent checks that running recovery twice produces the same
// outcome, because every plan is derived from the durable log rather than from
// in-memory state.
func TestRecoverIsIdempotent(t *testing.T) {
	events := mem.NewEventStore()
	ctx := context.Background()
	const runID = "run-idem"

	mustAppend(t, events, runID, types.Event{
		Type: types.EventRunQueued, State: types.StateQueued, SessionID: "ses-i",
	})
	mustAppend(t, events, runID, types.Event{
		Type: types.EventToolStarted, ToolCallID: "call-term", ToolName: "terminal",
		State: types.StateExecuting,
	})

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
	h.engine = engine
	defer engine.Close()

	first, err := engine.Recover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	second, err := engine.Recover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != len(second) {
		t.Fatalf("plan counts differ: %d then %d", len(first), len(second))
	}
	for i := range first {
		if first[i].Decision != second[i].Decision {
			t.Errorf("plan %d decision changed: %s then %s", i, first[i].Decision, second[i].Decision)
		}
	}
	if h.tools.CallCount() != 0 {
		t.Errorf("recovery executed %d tool calls, want 0", h.tools.CallCount())
	}
}

// ---------------------------------------------------------------------------
// session actor
// ---------------------------------------------------------------------------

// TestSessionActorSerialisesWrites checks that concurrent writers go through the
// actor and that its materialized state ends up consistent.
func TestSessionActorSerialisesWrites(t *testing.T) {
	var mu sync.Mutex
	var applied []agent.Transition
	deps := actorDeps{
		appendEvent: func(_ context.Context, ev types.Event) (uint64, error) {
			return 1, nil
		},
		onTransition: func(tr agent.Transition, _ uint64) {
			mu.Lock()
			applied = append(applied, tr)
			mu.Unlock()
		},
	}
	a := NewSessionActor("ses-actor", deps, agent.NewPanicGuard(agent.GuardConfig{DumpDir: t.TempDir()}))
	defer a.Close()

	ctx := context.Background()
	if err := a.RegisterRun(ctx, types.Run{ID: "run-a", State: types.StateCreated}); err != nil {
		t.Fatalf("RegisterRun: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// All these are legal from created/queued; the point is that the
			// actor serialises them rather than the state being torn.
			_ = a.Transition(ctx, agent.Transition{RunID: "run-a", To: types.StateQueued, Reason: "queued"})
			_, _ = a.Snapshot(ctx)
		}()
	}
	wg.Wait()

	snap, err := a.Snapshot(ctx)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	run, ok := snap["run-a"]
	if !ok {
		t.Fatal("run-a missing from the snapshot")
	}
	if run.State != types.StateQueued {
		t.Errorf("state = %s, want queued", run.State)
	}
	// A snapshot must not be observable mid-update: the version must be
	// non-zero and the record self-consistent.
	if run.Version == 0 {
		t.Error("run version was never advanced by the actor")
	}
}

// TestSessionActorRejectsWritesForUnknownRun checks that the actor refuses to
// invent state for a run it does not know about.
func TestSessionActorRejectsWritesForUnknownRun(t *testing.T) {
	deps := actorDeps{appendEvent: func(context.Context, types.Event) (uint64, error) { return 1, nil }}
	a := NewSessionActor("ses-unknown", deps, nil)
	defer a.Close()

	err := a.Transition(context.Background(), agent.Transition{
		RunID: "nope", To: types.StateQueued,
	})
	if err == nil {
		t.Fatal("expected a transition for an unregistered run to be refused")
	}
	if types.CodeOf(err) != types.CodeNotFound {
		t.Errorf("error code = %s, want %s", types.CodeOf(err), types.CodeNotFound)
	}
}

// TestSessionActorRejectsIllegalTransition checks that the actor enforces the
// state table on behalf of the engine.
func TestSessionActorRejectsIllegalTransition(t *testing.T) {
	deps := actorDeps{appendEvent: func(context.Context, types.Event) (uint64, error) { return 1, nil }}
	a := NewSessionActor("ses-illegal", deps, nil)
	defer a.Close()

	ctx := context.Background()
	if err := a.RegisterRun(ctx, types.Run{ID: "run-x", State: types.StateCreated}); err != nil {
		t.Fatal(err)
	}
	// created -> executing skips queued, so it is illegal.
	err := a.Transition(ctx, agent.Transition{RunID: "run-x", To: types.StateExecuting})
	if err == nil {
		t.Fatal("expected an illegal transition to be refused by the actor")
	}
	if types.CodeOf(err) != types.CodeInvalidTransition {
		t.Errorf("error code = %s, want %s", types.CodeOf(err), types.CodeInvalidTransition)
	}
}

// TestSessionActorCloseReleasesCallers checks that Close does not leave a caller
// blocked forever.
func TestSessionActorCloseReleasesCallers(t *testing.T) {
	deps := actorDeps{appendEvent: func(context.Context, types.Event) (uint64, error) { return 1, nil }}
	a := NewSessionActor("ses-close", deps, nil)

	ctx := context.Background()
	if err := a.RegisterRun(ctx, types.Run{ID: "run-c", State: types.StateCreated}); err != nil {
		t.Fatal(err)
	}
	a.Close()

	// After Close, further writes must fail fast rather than hanging.
	done := make(chan error, 1)
	go func() {
		done <- a.Transition(ctx, agent.Transition{RunID: "run-c", To: types.StateQueued})
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("expected a write after Close to be refused")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a write after Close blocked instead of failing fast")
	}
	// Close must be idempotent.
	a.Close()
}

// TestSessionActorSnapshotSurvivesConcurrentMutation reads snapshots while
// writes are in flight, so the race detector covers the actor's hand-off.
func TestSessionActorSnapshotSurvivesConcurrentMutation(t *testing.T) {
	deps := actorDeps{appendEvent: func(context.Context, types.Event) (uint64, error) { return 1, nil }}
	a := NewSessionActor("ses-race", deps, nil)
	defer a.Close()

	ctx := context.Background()
	for i := 0; i < 4; i++ {
		if err := a.RegisterRun(ctx, types.Run{
			ID: "run-" + itoa(i), State: types.StateCreated,
		}); err != nil {
			t.Fatal(err)
		}
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = a.UpdateRun(ctx, "run-"+itoa(i%4), func(r *types.Run) { r.Round++ })
			_, _ = a.Snapshot(ctx)
			_ = a.RunIDs()
		}(i)
	}
	wg.Wait()

	snap, err := a.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap) != 4 {
		t.Errorf("snapshot has %d runs, want 4", len(snap))
	}
}

// TestActorCloseIsSafeUnderConcurrentLoad closes the actor while writers are
// active, so a lost wakeup or a double close would surface under -race.
func TestActorCloseIsSafeUnderConcurrentLoad(t *testing.T) {
	deps := actorDeps{appendEvent: func(context.Context, types.Event) (uint64, error) { return 1, nil }}
	a := NewSessionActor("ses-load", deps, nil)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		_ = a.RegisterRun(ctx, types.Run{ID: "run-" + itoa(i), State: types.StateCreated})
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				_ = a.UpdateRun(ctx, "run-"+itoa(i%2), func(r *types.Run) { r.Round++ })
			}
		}(i)
	}
	time.Sleep(5 * time.Millisecond)
	a.Close()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("writers did not finish after the actor closed")
	}
}

// ---------------------------------------------------------------------------
// admission
// ---------------------------------------------------------------------------

// TestAdmissionPerSessionLimit checks that one session cannot occupy the whole
// machine.
func TestAdmissionPerSessionLimit(t *testing.T) {
	cfg := DefaultAdmissionConfig()
	cfg.MaxRunningRuns = 8
	cfg.MaxRunningRunsPerSession = 2
	a, err := NewAdmissionForTest(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	var tickets []*AdmissionTicket
	for i := 0; i < 2; i++ {
		tk, err := a.Admit(ctx, types.SubmitRequest{Prompt: "x"}, "run-"+itoa(i), "session-1")
		if err != nil {
			t.Fatalf("admit %d: %v", i, err)
		}
		tickets = append(tickets, tk)
	}
	if _, err := a.Admit(ctx, types.SubmitRequest{Prompt: "x"}, "run-extra", "session-1"); err == nil {
		t.Fatal("expected the per-session ceiling to reject a third run")
	} else if types.CodeOf(err) != types.CodeAdmissionRejected {
		t.Errorf("error code = %s, want %s", types.CodeOf(err), types.CodeAdmissionRejected)
	}
	// A different session is unaffected.
	tk, err := a.Admit(ctx, types.SubmitRequest{Prompt: "x"}, "run-other", "session-2")
	if err != nil {
		t.Fatalf("a different session should be admitted: %v", err)
	}
	tk.Release()

	// Releasing frees capacity for the blocked session.
	tickets[0].Release()
	if _, err := a.Admit(ctx, types.SubmitRequest{Prompt: "x"}, "run-again", "session-1"); err != nil {
		t.Errorf("after a release the session should have capacity: %v", err)
	}

	// Release must be idempotent.
	tickets[1].Release()
	tickets[1].Release()
	if got := a.RunningRuns(); got != 1 {
		t.Errorf("running runs = %d, want 1 after releasing two of three", got)
	}
}

// TestAdmissionTicketDeadline checks the wall-clock budget attached to a ticket.
func TestAdmissionTicketDeadline(t *testing.T) {
	cfg := DefaultAdmissionConfig()
	cfg.Limits.MaxRunDuration = time.Minute
	a, err := NewAdmissionForTest(cfg)
	if err != nil {
		t.Fatal(err)
	}
	tk, err := a.Admit(context.Background(), types.SubmitRequest{Prompt: "x"}, "run-d", "ses-d")
	if err != nil {
		t.Fatal(err)
	}
	defer tk.Release()

	if got := tk.Remaining(time.Now()); got <= 0 || got > time.Minute {
		t.Errorf("remaining = %s, want something in (0, 1m]", got)
	}
	if got := tk.Remaining(time.Now().Add(2 * time.Minute)); got != 0 {
		t.Errorf("remaining past the deadline = %s, want 0", got)
	}
}

// TestAdmissionRejectsAfterClose checks that shutdown stops new admissions.
func TestAdmissionRejectsAfterClose(t *testing.T) {
	a, err := NewAdmissionForTest(DefaultAdmissionConfig())
	if err != nil {
		t.Fatal(err)
	}
	a.Close()
	if _, err := a.Admit(context.Background(), types.SubmitRequest{Prompt: "x"}, "r", "s"); err == nil {
		t.Fatal("expected admission to be refused after Close")
	} else if types.CodeOf(err) != types.CodeEngineClosed {
		t.Errorf("error code = %s, want %s", types.CodeOf(err), types.CodeEngineClosed)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// mustAppend is a test helper that appends an event or fails.
func mustAppend(t *testing.T, store *mem.EventStore, runID string, ev types.Event) {
	t.Helper()
	if _, err := store.Append(context.Background(), runID, ev); err != nil {
		t.Fatalf("append %s for %s: %v", ev.Type, runID, err)
	}
}

// containsType reports whether the slice contains the event type.
func containsType(haystack []types.EventType, needle types.EventType) bool {
	for _, t := range haystack {
		if t == needle {
			return true
		}
	}
	return false
}

// eventTypesOf extracts the event types from a slice of events.
func eventTypesOf(events []types.Event) []types.EventType {
	out := make([]types.EventType, 0, len(events))
	for _, e := range events {
		out = append(out, e.Type)
	}
	return out
}
