package scheduler

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// TestFairQueueWeightsMatchDoc pins the QoS weights to the documented values.
// If one of these changes, the doc has to change with it.
func TestFairQueueWeightsMatchDoc(t *testing.T) {
	q := NewFairQueue(100, 8)
	want := map[types.Priority]int{
		types.PriorityInteractive: 8,
		types.PriorityNormal:      4,
		types.PriorityBackground:  1,
		types.PriorityKnowledge:   1,
	}
	for p, w := range want {
		if got := p.Weight(); got != w {
			t.Errorf("priority %s: weight = %d, want %d", p, got, w)
		}
		cl, ok := q.classes[p]
		if !ok {
			t.Fatalf("priority %s has no queue class", p)
		}
		if cl.weight != w {
			t.Errorf("priority %s: queue weight = %d, want %d", p, cl.weight, w)
		}
	}
}

// TestFairQueueInteractiveGetsLargerShare checks that weighted fair queueing
// actually favours the interactive class. With weights 8:4:1:1 the interactive
// class should receive roughly 8/14 of dispatches, so the assertion is a
// generous lower bound rather than an exact ratio, which would be flaky.
//
// The queues are deliberately over-filled (100 tasks per class, only 40
// dispatches) so that the measurement reflects the selection policy rather
// than some classes simply running out of work.
func TestFairQueueInteractiveGetsLargerShare(t *testing.T) {
	q := NewFairQueue(100000, 100000)
	for _, p := range types.AllPriorities {
		for i := 0; i < 100; i++ {
			if err := q.Enqueue(&Task{
				ID: string(p) + "-" + itoa(i), Priority: p, Cost: 1,
			}); err != nil {
				t.Fatalf("enqueue: %v", err)
			}
		}
	}

	counts := map[types.Priority]int{}
	for i := 0; i < 40; i++ {
		task := q.Dequeue()
		if task == nil {
			break
		}
		counts[task.Priority]++
		q.Release(task)
	}

	interactive := counts[types.PriorityInteractive]
	background := counts[types.PriorityBackground]
	if interactive <= background {
		t.Errorf("interactive (%d) should get more dispatches than background (%d): %v",
			interactive, background, counts)
	}
	// No class may be starved: the whole point of weighted fair queueing over
	// strict priority.
	for _, p := range types.AllPriorities {
		if counts[p] == 0 {
			t.Errorf("class %s was starved: %v", p, counts)
		}
	}
}

// TestFairQueueRejectsWhenFull verifies the hard ceiling: a full queue refuses
// rather than growing without bound.
func TestFairQueueRejectsWhenFull(t *testing.T) {
	q := NewFairQueue(3, 8)
	for i := 0; i < 3; i++ {
		if err := q.Enqueue(&Task{ID: itoa(i), Priority: types.PriorityNormal}); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	err := q.Enqueue(&Task{ID: "overflow", Priority: types.PriorityNormal})
	if err == nil {
		t.Fatal("expected the queue to reject the fourth task")
	}
	if types.CodeOf(err) != types.CodeQueueFull {
		t.Errorf("error code = %s, want %s", types.CodeOf(err), types.CodeQueueFull)
	}
	if got := q.Len(); got != 3 {
		t.Errorf("queue length = %d, want 3 (a rejected task must not be counted)", got)
	}
}

// TestFairQueueSessionCapIsEnforced is the layer-2 test: a single session may
// not exceed MaxConcurrentToolsPerSession, and its queued work waits rather
// than being lost.
func TestFairQueueSessionCapIsEnforced(t *testing.T) {
	const sessionCap = 2
	q := NewFairQueue(100, sessionCap)

	// Six tool calls from one session.
	for i := 0; i < 6; i++ {
		if err := q.Enqueue(&Task{
			ID: "s1-" + itoa(i), Kind: types.TaskKindToolCall, SessionID: "s1",
			Priority: types.PriorityNormal, Cost: 1,
		}); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}

	var dispatched []*Task
	for {
		task := q.Dequeue()
		if task == nil {
			break
		}
		dispatched = append(dispatched, task)
	}
	if len(dispatched) != sessionCap {
		t.Fatalf("dispatched %d tool calls for one session, want the cap %d", len(dispatched), sessionCap)
	}
	if got := q.InFlight("s1"); got != sessionCap {
		t.Errorf("in-flight = %d, want %d", got, sessionCap)
	}
	if got := q.Len(); got != 4 {
		t.Errorf("queue length = %d, want 4 still waiting", got)
	}

	// Releasing one lease lets exactly one more through.
	q.Release(dispatched[0])
	next := q.Dequeue()
	if next == nil {
		t.Fatal("expected a queued task to become dispatchable after a release")
	}
	if got := q.InFlight("s1"); got != sessionCap {
		t.Errorf("in-flight after release = %d, want %d", got, sessionCap)
	}
}

// TestFairQueueBlockedSessionDoesNotStallOthers checks that one saturated
// session cannot block a different session's work. This is the property that
// makes the per-session cap a fairness mechanism rather than a global stall.
func TestFairQueueBlockedSessionDoesNotStallOthers(t *testing.T) {
	q := NewFairQueue(100, 1)

	if err := q.Enqueue(&Task{ID: "a1", Kind: types.TaskKindToolCall, SessionID: "a",
		Priority: types.PriorityNormal, Cost: 1}); err != nil {
		t.Fatal(err)
	}
	if err := q.Enqueue(&Task{ID: "a2", Kind: types.TaskKindToolCall, SessionID: "a",
		Priority: types.PriorityNormal, Cost: 1}); err != nil {
		t.Fatal(err)
	}
	if err := q.Enqueue(&Task{ID: "b1", Kind: types.TaskKindToolCall, SessionID: "b",
		Priority: types.PriorityNormal, Cost: 1}); err != nil {
		t.Fatal(err)
	}

	first := q.Dequeue()
	if first == nil {
		t.Fatal("expected a dispatch")
	}
	// Session a is now saturated at 1. Session b's task must still be reachable.
	second := q.Dequeue()
	if second == nil {
		t.Fatal("session a's saturation must not stall session b")
	}
	if second.SessionID != "b" {
		t.Errorf("expected session b to be dispatched, got %q", second.SessionID)
	}
}

// TestResourcePoolCapacitiesMatchDoc pins the third layer's capacity table.
func TestResourcePoolCapacitiesMatchDoc(t *testing.T) {
	want := map[types.ResourceClass]int{
		types.ResourceBrowser:          4,
		types.ResourceComputerUse:      2,
		types.ResourceTerminal:         16,
		types.ResourceOffice:           4,
		types.ResourceMCP:              16,
		types.ResourceVision:           8,
		types.ResourceProvider:         32,
		types.ResourceClassExpertAgent: 8,
	}
	p := NewResourcePool(nil, 16)
	for r, cap := range want {
		if got := p.Capacity(r); got != cap {
			t.Errorf("resource %s: capacity = %d, want %d", r, got, cap)
		}
		if got := types.ResourceCapacities[r]; got != cap {
			t.Errorf("types.ResourceCapacities[%s] = %d, want %d", r, got, cap)
		}
	}
}

// TestResourcePoolEnforcesCapacity checks that the pool blocks beyond capacity
// and hands a freed slot to the oldest waiter.
func TestResourcePoolEnforcesCapacity(t *testing.T) {
	p := NewResourcePool(map[types.ResourceClass]int{types.ResourceBrowser: 2}, 16)

	l1, err := p.Acquire(context.Background(), types.ResourceBrowser)
	if err != nil {
		t.Fatalf("acquire 1: %v", err)
	}
	l2, err := p.Acquire(context.Background(), types.ResourceBrowser)
	if err != nil {
		t.Fatalf("acquire 2: %v", err)
	}
	if _, ok := p.TryAcquire(types.ResourceBrowser); ok {
		t.Fatal("expected TryAcquire to fail at capacity")
	}
	if got := p.InUse(types.ResourceBrowser); got != 2 {
		t.Errorf("in use = %d, want 2", got)
	}

	// A third acquisition must block, then succeed when one is released.
	got := make(chan *Lease, 1)
	go func() {
		l, err := p.Acquire(context.Background(), types.ResourceBrowser)
		if err != nil {
			got <- nil
			return
		}
		got <- l
	}()

	select {
	case <-got:
		t.Fatal("acquire returned while the pool was at capacity")
	case <-time.After(50 * time.Millisecond):
	}

	l1.Release()
	select {
	case l := <-got:
		if l == nil {
			t.Fatal("blocked acquire failed after a release")
		}
		l.Release()
	case <-time.After(2 * time.Second):
		t.Fatal("blocked acquire did not proceed after a release")
	}
	// Capacity must never be exceeded, and must return to zero.
	l2.Release()
	if got := p.InUse(types.ResourceBrowser); got != 0 {
		t.Errorf("in use after releasing everything = %d, want 0", got)
	}
}

// TestResourcePoolRespectsContextCancellation checks that a waiter gives up when
// its context ends and does not leak capacity.
func TestResourcePoolRespectsContextCancellation(t *testing.T) {
	p := NewResourcePool(map[types.ResourceClass]int{types.ResourceBrowser: 1}, 16)
	l, err := p.Acquire(context.Background(), types.ResourceBrowser)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err = p.Acquire(ctx, types.ResourceBrowser)
	if err == nil {
		t.Fatal("expected the acquire to fail when its context expired")
	}
	if types.CodeOf(err) != types.CodeDeadlineExceeded {
		t.Errorf("error code = %s, want %s", types.CodeOf(err), types.CodeDeadlineExceeded)
	}
	if got := p.Waiting(types.ResourceBrowser); got != 0 {
		t.Errorf("waiting = %d, want 0: a timed-out waiter must be removed", got)
	}
	if got := p.InUse(types.ResourceBrowser); got != 1 {
		t.Errorf("in use = %d, want 1: a failed acquire must not consume capacity", got)
	}
}

// TestResourcePoolRejectsTooManyWaiters verifies the hard ceiling on waiters.
func TestResourcePoolRejectsTooManyWaiters(t *testing.T) {
	p := NewResourcePool(map[types.ResourceClass]int{types.ResourceBrowser: 1}, 2)
	held, err := p.Acquire(context.Background(), types.ResourceBrowser)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()

	// Fill the two waiter slots.
	for i := 0; i < 2; i++ {
		go func() {
			_, _ = p.Acquire(context.Background(), types.ResourceBrowser)
		}()
	}
	deadline := time.Now().Add(2 * time.Second)
	for p.Waiting(types.ResourceBrowser) < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err = p.Acquire(ctx, types.ResourceBrowser)
	if err == nil {
		t.Fatal("expected rejection once the waiter list is full")
	}
	if code := types.CodeOf(err); code != types.CodeResourceUnavailable {
		t.Errorf("error code = %s, want %s", code, types.CodeResourceUnavailable)
	}
}

// TestBudgetQueueCapsRejectFast covers the hard caps in QueueLimits: every one
// of them rejects rather than queueing without bound.
func TestBudgetQueueCapsRejectFast(t *testing.T) {
	limits := types.QueueLimits{
		MaxQueuedRuns:      2,
		MaxQueuedToolCalls: 2,
		MaxMemoryBytes:     100,
		MaxOutputBytes:     10,
		MaxRunDuration:     time.Minute,
	}
	if err := limits.Validate(); err != nil {
		t.Fatal(err)
	}
	b := NewBudget(limits)

	for i := 0; i < 2; i++ {
		if err := b.ReserveQueue(types.TaskKindRun); err != nil {
			t.Fatalf("reserve run %d: %v", i, err)
		}
	}
	if err := b.ReserveQueue(types.TaskKindRun); types.CodeOf(err) != types.CodeQueueFull {
		t.Errorf("third run reservation: code = %s, want %s", types.CodeOf(err), types.CodeQueueFull)
	}
	for i := 0; i < 2; i++ {
		if err := b.ReserveQueue(types.TaskKindToolCall); err != nil {
			t.Fatalf("reserve tool %d: %v", i, err)
		}
	}
	if err := b.ReserveQueue(types.TaskKindToolCall); types.CodeOf(err) != types.CodeQueueFull {
		t.Errorf("third tool reservation: code = %s, want %s", types.CodeOf(err), types.CodeQueueFull)
	}

	if err := b.ReserveMemory(100); err != nil {
		t.Fatalf("reserve memory: %v", err)
	}
	if err := b.ReserveMemory(1); types.CodeOf(err) != types.CodeMemoryLimitExceeded {
		t.Errorf("over-budget memory: code = %s, want %s", types.CodeOf(err), types.CodeMemoryLimitExceeded)
	}
	b.ReleaseMemory(100)
	if err := b.ReserveMemory(50); err != nil {
		t.Errorf("memory after release: %v", err)
	}

	if err := b.RecordOutput("run1", 10); err != nil {
		t.Fatalf("record output: %v", err)
	}
	if err := b.RecordOutput("run1", 1); types.CodeOf(err) != types.CodeOutputLimitExceeded {
		t.Errorf("over-budget output: code = %s, want %s", types.CodeOf(err), types.CodeOutputLimitExceeded)
	}

	// Duration budget.
	b.StartRun("run2", time.Now().Add(-2*time.Minute))
	if _, err := b.CheckDuration("run2", time.Now()); types.CodeOf(err) != types.CodeRunDurationExceeded {
		t.Errorf("over-budget duration: code = %s, want %s", types.CodeOf(err), types.CodeRunDurationExceeded)
	}
}

// TestBudgetReleaseIsSafeWhenUnbalanced checks that a stray release cannot make
// a counter negative, which would silently disable the cap.
func TestBudgetReleaseIsSafeWhenUnbalanced(t *testing.T) {
	b := NewBudget(types.QueueLimits{
		MaxQueuedRuns: 1, MaxQueuedToolCalls: 1,
		MaxMemoryBytes: 10, MaxOutputBytes: 10, MaxRunDuration: time.Minute,
	})
	b.ReleaseQueue(types.TaskKindRun)
	b.ReleaseMemory(5)
	snap := b.Snapshot()
	if snap.RunQueue != 0 || snap.Memory != 0 {
		t.Errorf("counters went negative: %+v", snap)
	}
	// The cap must still be enforceable.
	if err := b.ReserveQueue(types.TaskKindRun); err != nil {
		t.Fatalf("reserve after an unbalanced release: %v", err)
	}
	if err := b.ReserveQueue(types.TaskKindRun); types.CodeOf(err) != types.CodeQueueFull {
		t.Errorf("cap not enforced after an unbalanced release: code = %s", types.CodeOf(err))
	}
}

// TestSchedulerEnforcesGlobalRunningLimit is the layer-1 test: no more than
// MaxRunningTasks tasks execute concurrently.
func TestSchedulerEnforcesGlobalRunningLimit(t *testing.T) {
	cfg := types.DefaultSchedulerConfig()
	cfg.MaxRunningTasks = 3
	cfg.Limits.MaxQueuedRuns = 100
	cfg.Limits.MaxQueuedToolCalls = 100

	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	var inFlight, maxInFlight atomic.Int64
	s.SetRunner(RunnerFunc(func(ctx context.Context, task types.Task) error {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			cur := maxInFlight.Load()
			if n <= cur || maxInFlight.CompareAndSwap(cur, n) {
				break
			}
		}
		select {
		case <-time.After(10 * time.Millisecond):
		case <-ctx.Done():
		}
		return nil
	}))

	// Run tasks are not subject to the session cap, so they exercise layer 1
	// directly.
	for i := 0; i < 12; i++ {
		if err := s.Submit(types.Task{
			ID: "run-" + itoa(i), Kind: types.TaskKindRun,
			RunID: "r" + itoa(i), SessionID: "s" + itoa(i),
			Priority: types.PriorityNormal, Cost: 1,
		}); err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := s.Wait(ctx); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if got := maxInFlight.Load(); got > 3 {
		t.Errorf("max concurrent tasks = %d, want at most 3", got)
	}
	if got := maxInFlight.Load(); got < 2 {
		t.Errorf("max concurrent tasks = %d, expected the limit to actually be used", got)
	}
}

// TestSchedulerRejectsBeyondQueueCap checks that Submit itself rejects, so the
// caller learns immediately instead of waiting on work that will not run.
func TestSchedulerRejectsBeyondQueueCap(t *testing.T) {
	cfg := types.DefaultSchedulerConfig()
	cfg.MaxRunningTasks = 2
	cfg.Limits.MaxQueuedToolCalls = 2
	cfg.Limits.MaxQueuedRuns = 2

	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// A blocking runner holds every running slot so the queue fills.
	release := make(chan struct{})
	s.SetRunner(RunnerFunc(func(ctx context.Context, task types.Task) error {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil
	}))
	defer close(release)

	var lastErr error
	accepted := 0
	for i := 0; i < 12; i++ {
		err := s.Submit(types.Task{
			ID: "task-" + itoa(i), Kind: types.TaskKindToolCall,
			RunID: "r", SessionID: "s" + itoa(i),
			ToolName: "terminal", Priority: types.PriorityNormal, Cost: 1,
		})
		if err != nil {
			lastErr = err
			break
		}
		accepted++
	}
	if lastErr == nil {
		t.Fatalf("expected a rejection once the caps were reached; accepted %d tasks", accepted)
	}
	code := types.CodeOf(lastErr)
	if code != types.CodeQueueFull && code != types.CodeResourceUnavailable && code != types.CodeAdmissionRejected {
		t.Errorf("rejection code = %s, want a capacity-related code", code)
	}
	if accepted >= 12 {
		t.Error("the scheduler accepted every task, so no cap was enforced")
	}
}

// TestSchedulerContainsRunnerPanic is the panic-boundary test for the worker
// side: a panicking runner must fail its task without taking down the
// scheduler, and must not leak capacity.
func TestSchedulerContainsRunnerPanic(t *testing.T) {
	cfg := types.DefaultSchedulerConfig()
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	var hookCalls atomic.Int64
	s.SetPanicHook(func(n Notification, r any, stack []byte) {
		hookCalls.Add(1)
		if n.Err == nil {
			t.Error("panic hook called with no error")
		}
	})

	var okCount atomic.Int64
	s.SetRunner(RunnerFunc(func(ctx context.Context, task types.Task) error {
		if task.ToolName == "boom" {
			panic("runner exploded")
		}
		okCount.Add(1)
		return nil
	}))

	ctx := context.Background()
	if err := s.SubmitWait(ctx, types.Task{ID: "panic-1", Kind: types.TaskKindToolCall,
		RunID: "r", SessionID: "s1", ToolName: "boom", Priority: types.PriorityNormal}); err == nil {
		t.Fatal("expected the panicking task to report an error")
	} else if types.CodeOf(err) != types.CodePanicIsolated {
		t.Errorf("error code = %s, want %s", types.CodeOf(err), types.CodePanicIsolated)
	}

	// The scheduler must still be usable, and the panic must not have leaked a
	// running slot or a resource lease.
	if err := s.SubmitWait(ctx, types.Task{ID: "ok-1", Kind: types.TaskKindToolCall,
		RunID: "r", SessionID: "s1", ToolName: "terminal", Priority: types.PriorityNormal}); err != nil {
		t.Fatalf("scheduler unusable after a contained panic: %v", err)
	}
	if got := hookCalls.Load(); got != 1 {
		t.Errorf("panic hook calls = %d, want 1", got)
	}

	deadline := time.Now().Add(2 * time.Second)
	for s.RunningCount() > 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := s.RunningCount(); got != 0 {
		t.Errorf("running count = %d after everything finished, want 0", got)
	}
	if st := s.Stats(); st.Resources[types.ResourceTerminal].InUse != 0 {
		t.Errorf("terminal lease leaked: in use = %d", st.Resources[types.ResourceTerminal].InUse)
	}
}

// TestSchedulerCancelRunStopsWork verifies that cancelling a run stops both its
// queued and its running tasks.
func TestSchedulerCancelRunStopsWork(t *testing.T) {
	cfg := types.DefaultSchedulerConfig()
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	started := make(chan struct{}, 1)
	s.SetRunner(RunnerFunc(func(ctx context.Context, task types.Task) error {
		select {
		case started <- struct{}{}:
		default:
		}
		// Only terminal-class work blocks; the extra calls submitted below use
		// a resource class whose leases are released promptly, so the queue
		// actually drains instead of every goroutine blocking forever.
		if task.Resource != types.ResourceTerminal {
			return nil
		}
		<-ctx.Done()
		return ctx.Err()
	}))

	for i := 0; i < 4; i++ {
		if err := s.Submit(types.Task{
			ID: "t" + itoa(i), Kind: types.TaskKindToolCall,
			RunID: "run-x", SessionID: "sess-x", ToolName: "terminal",
			Priority: types.PriorityNormal,
		}); err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
	}

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("no task started")
	}
	if n := s.CancelRun("run-x"); n == 0 {
		t.Error("CancelRun reported no affected tasks")
	}

	// A task submitted *after* the cancel must not run: it would otherwise slip
	// through the gap between the cancel snapshot and its own dispatch.
	if err := s.Submit(types.Task{
		ID: "after-cancel", Kind: types.TaskKindToolCall,
		RunID: "run-x", SessionID: "sess-x", ToolName: "terminal",
		Priority: types.PriorityNormal,
	}); err != nil {
		t.Fatalf("submit after cancel: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Wait(ctx); err != nil {
		t.Fatalf("wait for drain: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for s.RunningCount() > 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := s.RunningCount(); got != 0 {
		t.Errorf("running count = %d after cancel, want 0", got)
	}
}

// TestDefaultToolResourceRouting checks the tool鈫抮esource mapping, including
// the deliberate fallback for an unknown tool.
func TestDefaultToolResourceRouting(t *testing.T) {
	cases := map[string]types.ResourceClass{
		"browser_click":        types.ResourceBrowser,
		"playwright_run":       types.ResourceBrowser,
		"computer_use":         types.ResourceComputerUse,
		"terminal":             types.ResourceTerminal,
		"mcp_call":             types.ResourceMCP,
		"vision_describe":      types.ResourceVision,
		"xlsx_write":           types.ResourceOffice,
		"totally_unknown_tool": types.ResourceTerminal,
	}
	for tool, want := range cases {
		if got := DefaultToolResource(tool); got != want {
			t.Errorf("DefaultToolResource(%q) = %s, want %s", tool, got, want)
		}
	}
}

// TestSchedulerSubmitIsRaceFree hammers Submit from many goroutines so the race
// detector can observe the gate accounting. Correctness here is "no races and
// no leaked capacity", not a specific ordering.
func TestSchedulerSubmitIsRaceFree(t *testing.T) {
	cfg := types.DefaultSchedulerConfig()
	cfg.MaxRunningTasks = 4
	cfg.Limits.MaxQueuedToolCalls = 64
	cfg.Limits.MaxQueuedRuns = 64

	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	s.SetRunner(RunnerFunc(func(ctx context.Context, task types.Task) error {
		time.Sleep(time.Millisecond)
		return nil
	}))

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				_ = s.Submit(types.Task{
					ID: "g" + itoa(g) + "-" + itoa(i), Kind: types.TaskKindToolCall,
					RunID: "r" + itoa(g), SessionID: "s" + itoa(g%3),
					ToolName: "terminal", Priority: types.AllPriorities[i%len(types.AllPriorities)],
					Cost: 1,
				})
			}
		}(g)
	}
	wg.Wait()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := s.Wait(ctx); err != nil {
		t.Fatalf("wait: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for s.RunningCount() > 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := s.RunningCount(); got != 0 {
		t.Errorf("running count = %d, want 0", got)
	}
	// Every admitted task must have released its resource lease.
	for _, rs := range s.Stats().Resources {
		if rs.InUse != 0 {
			t.Errorf("resource %s leaked %d leases", rs.Resource, rs.InUse)
		}
	}
}
