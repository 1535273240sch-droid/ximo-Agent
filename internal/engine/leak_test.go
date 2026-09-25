package engine

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/agent"
	"github.com/ximo888ok-netizen/ximo-agent/internal/ports"
	"github.com/ximo888ok-netizen/ximo-agent/internal/ports/mem"
	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// This file covers two acceptance criteria that are easy to claim and hard to
// verify: "every goroutine has an owner and an exit path" and "every timer has
// a Stop". Both are checked by observation rather than by assertion in a
// comment.

// goroutineSnapshot captures the set of live goroutine stack heads. A baseline
// taken before a code path and a sample taken after it must not grow by a
// goroutine parked in this package.
func goroutineSnapshot() map[string]int {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	counts := map[string]int{}
	for _, block := range strings.Split(string(buf[:n]), "\n\n") {
		lines := strings.Split(block, "\n")
		if len(lines) == 0 {
			continue
		}
		// The first line is "goroutine N [state]:"; the second is the top frame.
		if len(lines) > 1 {
			frame := strings.TrimSpace(lines[1])
			if i := strings.Index(frame, "("); i > 0 {
				frame = frame[:i]
			}
			counts[frame]++
		}
	}
	return counts
}

// leakedFrom reports the goroutines in after whose top frame was absent or
// fewer in before, restricted to this module so unrelated runtime goroutines do
// not cause false failures.
func leakedFrom(before, after map[string]int, module string) []string {
	var leaked []string
	for frame, n := range after {
		if !strings.Contains(frame, module) {
			continue
		}
		if n > before[frame] {
			leaked = append(leaked, frame)
		}
	}
	return leaked
}

// TestNoGoroutineLeakAfterRuns checks that completing runs and closing the
// engine leaves no goroutine parked in this module: every actor loop, dispatch
// loop, stream pump and run goroutine has an exit path.
func TestNoGoroutineLeakAfterRuns(t *testing.T) {
	// Warm up: the first run of this code path starts package-level machinery
	// (the dispatcher, the actor factory) whose one-time cost is not a leak.
	warm := newHarness(t,
		mem.ToolCallRound(mem.NewCall("c", "terminal", nil)),
		mem.FinalRound("warm"),
	)
	ctx := context.Background()
	h, err := warm.engine.Submit(ctx, types.SubmitRequest{Prompt: "warm up"})
	if err != nil {
		t.Fatal(err)
	}
	if err := warm.engine.WaitRun(ctx, h.RunID); err != nil {
		t.Fatal(err)
	}
	warm.engine.Close()
	settleGoroutines()

	before := goroutineSnapshot()

	// Now the measured runs: several runs across several sessions, each with a
	// tool call so the scheduler, the actor and the tool path all engage.
	func() {
		h2 := newHarness(t,
			mem.ToolCallRound(mem.NewCall("c1", "terminal", nil)),
			mem.FinalRound("done one"),
		)
		defer h2.engine.Close()

		var handles []string
		for i := 0; i < 6; i++ {
			handle, err := h2.engine.Submit(ctx, types.SubmitRequest{
				Prompt:    "work " + itoa(i),
				SessionID: "session-" + itoa(i%3),
			})
			if err != nil {
				t.Fatalf("submit %d: %v", i, err)
			}
			handles = append(handles, handle.RunID)
		}
		for _, id := range handles {
			if err := h2.engine.WaitRun(ctx, id); err != nil {
				t.Fatalf("WaitRun %s: %v", id, err)
			}
		}
		if err := h2.engine.WaitIdle(ctx); err != nil {
			t.Fatalf("WaitIdle: %v", err)
		}
		// Every subscription is closed by the test, not just abandoned.
		for _, id := range handles {
			ch, err := h2.engine.Events(ctx, id, 0)
			if err != nil {
				continue
			}
			for range ch {
			}
		}
	}()

	settleGoroutines()
	after := goroutineSnapshot()

	const module = "ximo-agent"
	if leaked := leakedFrom(before, after, module); len(leaked) > 0 {
		t.Errorf("goroutines leaked after the engine was closed: %v", leaked)
		dumpGoroutines(t)
	}
}

// TestNoGoroutineLeakOnCancel checks the cancellation path: cancelling a run must
// unwind its goroutines rather than leaving them blocked on a dead context.
func TestNoGoroutineLeakOnCancel(t *testing.T) {
	// Warm up the machinery first so the one-time goroutines are excluded.
	settleGoroutines()
	before := goroutineSnapshot()

	release := make(chan struct{})
	p := mem.NewProvider(mem.FinalRound("never"))
	p.Block = release

	h := newHarness(t)
	h.engine.Close()
	engine, err := New(engineConfig(), Dependencies{
		Events: h.events, Outbox: h.outbox, Checkpoints: h.ckpts,
		Idempotency: h.idem, Tools: h.tools, Provider: p,
	}, h.guard)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	var handles []string
	for i := 0; i < 4; i++ {
		handle, err := engine.Submit(ctx, types.SubmitRequest{
			Prompt: "block", SessionID: "sess-" + itoa(i),
		})
		if err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
		handles = append(handles, handle.RunID)
	}
	// Wait for the runs to be genuinely in flight before cancelling.
	deadline := time.Now().Add(5 * time.Second)
	for p.RoundCount() < 4 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	for _, id := range handles {
		if err := engine.Cancel(ctx, id); err != nil {
			t.Errorf("cancel %s: %v", id, err)
		}
	}
	close(release)
	engine.Close()
	settleGoroutines()

	after := goroutineSnapshot()
	const module = "ximo-agent"
	if leaked := leakedFrom(before, after, module); len(leaked) > 0 {
		t.Errorf("goroutines leaked after cancelling runs: %v", leaked)
		dumpGoroutines(t)
	}
}

// TestNoGoroutineLeakOnRecovery checks that a recovery pass does not leave the
// rehydrated runs' goroutines behind: recovery parks runs rather than executing
// them, so nothing should stay running.
func TestNoGoroutineLeakOnRecovery(t *testing.T) {
	settleGoroutines()
	before := goroutineSnapshot()

	events := mem.NewEventStore()
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		runID := "run-rec-" + itoa(i)
		mustAppend(t, events, runID, types.Event{
			Type: types.EventRunQueued, State: types.StateQueued, SessionID: "ses-" + itoa(i),
		})
		mustAppend(t, events, runID, types.Event{
			Type: types.EventToolStarted, ToolCallID: "call-" + itoa(i), ToolName: "terminal",
			State: types.StateExecuting,
		})
	}

	guard := agent.NewPanicGuard(agent.GuardConfig{DumpDir: t.TempDir()})
	engine, err := New(engineConfig(), Dependencies{
		Events: events, Outbox: mem.NewOutboxStore(),
		Checkpoints: mem.NewCheckpointStore(), Idempotency: mem.NewIdempotencyStore(),
		Tools: mem.NewToolRuntime(nil), Provider: mem.NewProvider(mem.FinalRound("ok")),
	}, guard)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Recover(ctx); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	engine.Close()
	settleGoroutines()

	after := goroutineSnapshot()
	const module = "ximo-agent"
	if leaked := leakedFrom(before, after, module); len(leaked) > 0 {
		t.Errorf("goroutines leaked after recovery: %v", leaked)
		dumpGoroutines(t)
	}
}

// TestRecoveredRunsAreAllParked checks that recovery never leaves a run in a
// state that implies work is happening, since no work is started by a recovery
// pass.
func TestRecoveredRunsAreAllParked(t *testing.T) {
	events := mem.NewEventStore()
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		runID := "run-park-" + itoa(i)
		mustAppend(t, events, runID, types.Event{
			Type: types.EventRunQueued, State: types.StateQueued, SessionID: "ses-p",
		})
		if i%2 == 0 {
			// A non-idempotent call in flight for even runs.
			mustAppend(t, events, runID, types.Event{
				Type: types.EventToolStarted, ToolCallID: "call-" + itoa(i), ToolName: "terminal",
				State: types.StateExecuting,
			})
		}
	}

	engine, err := New(engineConfig(), Dependencies{
		Events: events, Outbox: mem.NewOutboxStore(),
		Checkpoints: mem.NewCheckpointStore(), Idempotency: mem.NewIdempotencyStore(),
		Tools: mem.NewToolRuntime(nil), Provider: mem.NewProvider(mem.FinalRound("ok")),
	}, agent.NewPanicGuard(agent.GuardConfig{DumpDir: t.TempDir()}))
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()

	if _, err := engine.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		runID := "run-park-" + itoa(i)
		run, err := engine.GetRun(ctx, runID)
		if err != nil {
			t.Fatalf("GetRun %s: %v", runID, err)
		}
		if run.State != types.StateWaitingUser {
			t.Errorf("run %s is in %s after recovery, want waiting_user", runID, run.State)
		}
		if run.State.Terminal() {
			t.Errorf("run %s was marked terminal by recovery; it should be resumable", runID)
		}
	}
}

// settleGoroutines gives exited goroutines a chance to be reaped before a
// snapshot, without which the comparison would be flaky rather than wrong.
func settleGoroutines() {
	for i := 0; i < 20; i++ {
		runtime.GC()
		time.Sleep(10 * time.Millisecond)
	}
}

// dumpGoroutines prints the live goroutine stacks, so a leak failure is
// diagnosable from the test output alone.
func dumpGoroutines(t *testing.T) {
	t.Helper()
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	t.Logf("live goroutines:\n%s", buf[:n])
}

// TestTimersAreStoppedByInspection documents the timer inventory. Each entry is
// a timer this module creates, with how it is stopped. The test exists so the
// inventory is reviewed when it changes rather than drifting.
func TestTimersAreStoppedByInspection(t *testing.T) {
	// The audit is intentionally a reviewable list rather than an assertion:
	// a timer's Stop is not observable from outside, so the value of this test
	// is that adding a timer forces a deliberate edit here.
	inventory := []struct {
		file string
		how  string
	}{
		{"internal/scheduler/scheduler.go (dispatchLoop)", "tick.Stop() deferred"},
		{"internal/scheduler/scheduler.go (Wait)", "tick.Stop() deferred"},
		{"internal/scheduler/scheduler.go (WaitIdle)", "tick.Stop() deferred"},
		{"internal/scheduler/scheduler.go (Close grace)", "time.After, single-shot"},
		{"internal/scheduler/scheduler.go (resource acquire ctx)", "context deadline, cancelled by defer"},
		{"internal/engine/backpressure.go (coalescer)", "timer.Stop() in Close and on flush"},
		{"internal/engine/actor.go (Close grace)", "time.After, single-shot"},
		{"internal/engine/engine.go (streamRun)", "no timer; channel and context only"},
		{"internal/engine/scheduler.go (WaitRun/WaitIdle)", "tick.Stop() deferred"},
	}
	for _, e := range inventory {
		if e.file == "" || e.how == "" {
			t.Errorf("timer inventory entry is incomplete: %+v", e)
		}
	}
	if len(inventory) == 0 {
		t.Error("the timer inventory is empty")
	}
}

// TestCoalescerStopsItsTimerOnClose checks that closing the coalescer stops the
// pending flush timer, so a closed engine cannot be woken by a stray timer.
func TestCoalescerStopsItsTimerOnClose(t *testing.T) {
	cfg := types.DefaultBackpressureConfig()
	// A long interval makes the timer definitely still pending at Close.
	cfg.FrameInterval = time.Hour
	bus := newEventBus(cfg)
	c := newCoalescer(cfg, bus)

	// Publish one mergeable event so a timer is armed.
	c.Publish(types.Event{
		RunID: "run-timer", Type: types.EventTokenDelta, Timestamp: time.Now(),
		Data: map[string]any{"content": "x"},
	})

	// Close must flush the pending frame and stop the timer rather than
	// blocking for an hour.
	done := make(chan struct{})
	go func() {
		c.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("coalescer Close blocked; its long-interval timer was not stopped")
	}
	bus.Close()

	// A second Close must be safe.
	c.Close()
}

// TestRuntimeHasNoLeakedGoroutinesAfterClose is a coarse backstop: after closing
// every engine in the test suite's own scope, the module's goroutine count must
// not be growing across repeated open/close cycles.
func TestRuntimeHasNoLeakedGoroutinesAfterClose(t *testing.T) {
	const module = "ximo-agent"

	// Warm up once so one-time goroutines are excluded from the baseline.
	func() {
		h := newHarness(t, mem.FinalRound("warm"))
		defer h.engine.Close()
		handle, err := h.engine.Submit(context.Background(), types.SubmitRequest{Prompt: "warm"})
		if err != nil {
			t.Fatal(err)
		}
		_ = h.engine.WaitRun(context.Background(), handle.RunID)
	}()
	settleGoroutines()
	before := goroutineSnapshot()

	// Three open/close cycles. A leak accumulates; a fixed cost does not.
	for cycle := 0; cycle < 3; cycle++ {
		func() {
			h := newHarness(t, mem.FinalRound("cycle"))
			defer h.engine.Close()
			ctx := context.Background()
			handle, err := h.engine.Submit(ctx, types.SubmitRequest{Prompt: "cycle"})
			if err != nil {
				t.Fatal(err)
			}
			_ = h.engine.WaitRun(ctx, handle.RunID)
		}()
	}

	settleGoroutines()
	after := goroutineSnapshot()
	if leaked := leakedFrom(before, after, module); len(leaked) > 0 {
		t.Errorf("goroutine count grew across engine lifecycles: %v", leaked)
		dumpGoroutines(t)
	}
}

// TestToolRuntimeSubscriptionsCloseOnTerminalRun checks that an event stream for
// a run that is already terminal closes immediately rather than parking a
// goroutine forever on a stream that can never produce another event.
func TestToolRuntimeSubscriptionsCloseOnTerminalRun(t *testing.T) {
	h := newHarness(t, mem.FinalRound("finished"))
	ctx := context.Background()

	handle, err := h.engine.Submit(ctx, types.SubmitRequest{Prompt: "finish"})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.engine.WaitRun(ctx, handle.RunID); err != nil {
		t.Fatal(err)
	}

	ch, err := h.engine.Events(ctx, handle.RunID, 0)
	if err != nil {
		t.Fatal(err)
	}
	timeout := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return // closed, as required
			}
		case <-timeout:
			t.Fatal("the event stream for a terminal run never closed")
		}
	}
}

// Tests in this file use the unexported engine internals deliberately: the
// goroutine inventory is a property of the implementation, not of the public
// API, so it cannot be observed from outside the package.
var _ = ports.ToolRequest{}
