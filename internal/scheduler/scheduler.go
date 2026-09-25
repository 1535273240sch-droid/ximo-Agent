// Package scheduler implements the three-layer rate limiter from architecture
// doc ch. 6.
//
// The execution path is fixed and strictly ordered:
//
//	Global Admission → Session Fair Queue → Tool Resource Pool → Worker
//
//	layer 1  global gate    caps concurrently running tasks (MaxRunningTasks=32)
//	layer 2  FairQueue      per-QoS weighted fair queue + per-session tool cap
//	                        (MaxConcurrentToolsPerSession=8)
//	layer 3  ResourcePool   per-resource semaphores (browser=4, terminal=16, …)
//
// Every layer has a hard ceiling. When one is reached the task is rejected
// fast rather than queued without bound, because the architecture requires
// that no queue may grow until the process runs out of memory (doc ch. 6.2).
//
// Workers are reached through a Runner supplied by task 05; this package never
// spawns a worker itself.
package scheduler

import (
	"context"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/ports"
	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// Task is an alias for the shared task contract, so this package can be read
// and written without a types. prefix on every line.
type Task = types.Task

// Runner executes an admitted task. Task 05 supplies the worker-backed
// implementation; the engine supplies one that wraps the agent loop for run
// tasks and the tool runtime for tool-call tasks.
//
// Run is called with a context carrying the task's cancellation. It must
// respect ctx. A panic is contained by the scheduler and reported as a failed
// task, because a worker/tool goroutine is a permitted recover boundary
// (doc ch. 3.2).
type Runner interface {
	Run(ctx context.Context, task Task) error
}

// RunnerFunc adapts a function to Runner.
type RunnerFunc func(ctx context.Context, task Task) error

// Run implements Runner.
func (f RunnerFunc) Run(ctx context.Context, t Task) error { return f(ctx, t) }

// ToolResourceResolver maps a tool name onto the resource class it must lease.
// Task 04 can override the default table.
type ToolResourceResolver func(toolName string) types.ResourceClass

// defaultToolResources maps tool-name prefixes onto resource classes. Matching
// by prefix means a whole tool family ("browser_click", "browser_navigate")
// lands on one class without an exhaustive list.
var defaultToolResources = map[string]types.ResourceClass{
	"browser":      types.ResourceBrowser,
	"playwright":   types.ResourceBrowser,
	"computer_use": types.ResourceComputerUse,
	"computer":     types.ResourceComputerUse,
	"terminal":     types.ResourceTerminal,
	"shell":        types.ResourceTerminal,
	"exec":         types.ResourceTerminal,
	"office":       types.ResourceOffice,
	"docx":         types.ResourceOffice,
	"xlsx":         types.ResourceOffice,
	"pptx":         types.ResourceOffice,
	"pdf":          types.ResourceOffice,
	"mcp":          types.ResourceMCP,
	"vision":       types.ResourceVision,
	"screenshot":   types.ResourceVision,
	"image":        types.ResourceVision,
}

// DefaultToolResource resolves a tool name to its resource class. Unknown
// tools fall back to the terminal class rather than being unlimited: an
// unclassified tool must still be governed by some ceiling, and terminal is
// the most permissive executor class (16).
func DefaultToolResource(toolName string) types.ResourceClass {
	n := strings.ToLower(toolName)
	for prefix, class := range defaultToolResources {
		if strings.HasPrefix(n, prefix) {
			return class
		}
	}
	return types.ResourceTerminal
}

// dispatchPollInterval bounds the latency of a task becoming dispatchable for a
// reason other than a new submission (a session lease being released). It is a
// var so tests can shorten it.
var dispatchPollInterval = 2 * time.Millisecond

// NotificationKind enumerates scheduler lifecycle callbacks.
type NotificationKind string

const (
	NotifyAdmitted  NotificationKind = "admitted"
	NotifyCompleted NotificationKind = "completed"
	NotifyFailed    NotificationKind = "failed"
	NotifyRejected  NotificationKind = "rejected"
	NotifyCancelled NotificationKind = "cancelled"
)

// Notification is a scheduler lifecycle event handed to an observer. It exists
// so the engine can mirror scheduler decisions into the durable event log
// without this package importing the engine.
type Notification struct {
	Kind      NotificationKind
	TaskID    string
	RunID     string
	SessionID string
	Priority  types.Priority
	Err       *types.Error
	At        time.Time
}

// NotifyFunc observes scheduler notifications. It is called from the dispatch
// goroutine, so it must not block: a slow observer would stall dispatch.
type NotifyFunc func(Notification)

// Scheduler is the concrete three-layer scheduler. It satisfies the ch. 32
// `Scheduler` interface (Submit/Cancel/Stats) and additionally supports
// waiting for a task's completion.
type Scheduler struct {
	cfg       types.SchedulerConfig
	budget    *Budget
	fair      *FairQueue
	resources *ResourcePool
	resolver  ToolResourceResolver
	runner    Runner
	notify    NotifyFunc
	panicHook func(Notification, any, []byte)

	// gate is layer 1: a buffered channel used as a counting semaphore.
	gate chan struct{}

	mu      sync.Mutex
	running map[string]*runningTask
	// waits holds completion channels for callers blocked in SubmitWait.
	waits map[string]chan error
	// cancelledRuns records runs whose work must not run, so a task dispatched
	// after CancelRun is cancelled too rather than slipping through the gap
	// between the cancel snapshot and its own dispatch.
	cancelledRuns map[string]struct{}
	// closed guards shutdown.
	closed bool
	// wake signals the dispatcher that the queue gained work.
	wake chan struct{}
	// wg tracks dispatch goroutines.
	wg sync.WaitGroup
	// inflight counts accepted tasks that have not yet finished, including
	// those still queued. Wait uses it to know when the scheduler is idle.
	inflight atomic.Int64
	// dispatcherDone closes when the dispatcher loop exits.
	dispatcherDone chan struct{}
	// startOnce guards starting exactly one dispatcher.
	startOnce sync.Once

	dispatched atomic.Uint64
	completed  atomic.Uint64
	rejected   atomic.Uint64
	failedTask atomic.Uint64
	// observerPanics counts panics contained at the notification boundary, so a
	// misbehaving observer is diagnosable rather than invisible.
	observerPanics atomic.Uint64
}

// runningTask is a dispatched task plus its cancellation state.
type runningTask struct {
	task    Task
	cancel  context.CancelFunc
	done    chan struct{}
	started time.Time
}

// New builds a scheduler from a configuration. It validates the configuration
// and fails immediately rather than deferring the failure to the first Submit,
// because an invalid ceiling is a startup bug.
func New(cfg types.SchedulerConfig) (*Scheduler, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	totalCap := cfg.Limits.MaxQueuedRuns
	if cfg.Limits.MaxQueuedToolCalls > totalCap {
		totalCap = cfg.Limits.MaxQueuedToolCalls
	}
	s := &Scheduler{
		cfg:            cfg,
		budget:         NewBudget(cfg.Limits),
		fair:           NewFairQueue(totalCap, cfg.MaxConcurrentToolsPerSession),
		resources:      NewResourcePool(cfg.ResourceCapacities, 4096),
		resolver:       DefaultToolResource,
		gate:           make(chan struct{}, cfg.MaxRunningTasks),
		running:        make(map[string]*runningTask),
		waits:          make(map[string]chan error),
		cancelledRuns:  make(map[string]struct{}),
		wake:           make(chan struct{}, 1),
		dispatcherDone: make(chan struct{}),
	}
	return s, nil
}

// SetRunner installs the executor. It must be called before tasks can make
// progress.
func (s *Scheduler) SetRunner(r Runner) {
	s.mu.Lock()
	s.runner = r
	s.mu.Unlock()
	s.kick()
}

// SetNotifier installs a lifecycle observer.
func (s *Scheduler) SetNotifier(n NotifyFunc) {
	s.mu.Lock()
	s.notify = n
	s.mu.Unlock()
}

// SetToolResourceResolver overrides the tool→resource mapping.
func (s *Scheduler) SetToolResourceResolver(r ToolResourceResolver) {
	s.mu.Lock()
	if r == nil {
		r = DefaultToolResource
	}
	s.resolver = r
	s.mu.Unlock()
}

// SetPanicHook installs the handler for runner panics.
func (s *Scheduler) SetPanicHook(h func(Notification, any, []byte)) {
	s.mu.Lock()
	s.panicHook = h
	s.mu.Unlock()
}

// Budget exposes the budget accountant so the engine can charge output bytes
// and run durations against the same limits the scheduler enforces.
func (s *Scheduler) Budget() *Budget { return s.budget }

// Resources exposes the resource pool.
func (s *Scheduler) Resources() *ResourcePool { return s.resources }

// FairQueue exposes the fair queue.
func (s *Scheduler) FairQueue() *FairQueue { return s.fair }

// Submit validates, admits and queues a task, returning as soon as it is
// queued. Completion is reported through the Notify observer; use SubmitWait
// to block for the result.
//
// The gate order is the documented chain: hard queue caps → fair queue →
// (dispatcher) resource pool → global gate → runner. A task failing any gate
// is rejected without consuming a later gate's capacity.
func (s *Scheduler) Submit(task Task) error {
	prepared, err := s.prepare(task)
	if err != nil {
		s.reject(Task{ID: task.ID, RunID: task.RunID, SessionID: task.SessionID, Priority: task.Priority}, err)
		return err
	}

	// Gate: hard queue cap, checked before the fair queue so a saturated queue
	// cannot consume anything else.
	if err := s.budget.AdmitQueue(prepared.Kind); err != nil {
		s.reject(prepared, err)
		return err
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return types.NewError(types.CodeEngineClosed, "scheduler is closed")
	}
	if s.runner == nil {
		s.mu.Unlock()
		return types.NewError(types.CodeInternal, "scheduler has no runner installed")
	}
	s.mu.Unlock()

	// Reserve the queue slot, then hand the task to the fair queue.
	if err := s.budget.ReserveQueue(prepared.Kind); err != nil {
		s.reject(prepared, err)
		return err
	}
	if err := s.fair.Enqueue(&prepared); err != nil {
		s.budget.ReleaseQueue(prepared.Kind)
		s.reject(prepared, err)
		return err
	}
	// Count the task as in flight before it can be dispatched, so Wait cannot
	// observe "idle" while work is still queued.
	s.inflight.Add(1)
	s.kick()
	return nil
}

// SubmitWait submits a task and blocks until it finishes. It returns the
// runner's error. The caller must not use both SubmitWait and a notifier to
// observe the same task's completion ordering-critical side effects.
func (s *Scheduler) SubmitWait(ctx context.Context, task Task) error {
	prepared, err := s.prepare(task)
	if err != nil {
		return err
	}
	ch := make(chan error, 1)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return types.NewError(types.CodeEngineClosed, "scheduler is closed")
	}
	if _, dup := s.waits[prepared.ID]; dup {
		s.mu.Unlock()
		return types.NewError(types.CodeAlreadyExists, "task %s already has a waiter", prepared.ID)
	}
	s.waits[prepared.ID] = ch
	s.mu.Unlock()

	if err := s.Submit(prepared); err != nil {
		s.mu.Lock()
		delete(s.waits, prepared.ID)
		s.mu.Unlock()
		return err
	}

	select {
	case err := <-ch:
		return err
	case <-ctx.Done():
		// Stop the task; the completion path still cleans up its waiter entry.
		_ = s.Cancel(prepared.ID)
		return types.WrapError(types.CodeDeadlineExceeded, ctx.Err(), "waiting for task %s", prepared.ID)
	}
}

// prepare fills in defaults and validates a task, returning a normalised copy.
// It deliberately takes a value: Submit(Task) matches the documented interface
// signature, and the scheduler works on its own copy so a caller mutating its
// struct afterwards cannot corrupt scheduler state.
func (s *Scheduler) prepare(task Task) (Task, error) {
	if task.Kind == "" {
		task.Kind = types.TaskKindToolCall
	}
	if task.Kind != types.TaskKindRun && task.Kind != types.TaskKindToolCall {
		return task, types.NewError(types.CodeInvalidArgument, "unknown task kind %q", string(task.Kind))
	}
	if task.Priority == "" {
		task.Priority = types.PriorityNormal
	}
	if !task.Priority.Valid() {
		return task, types.NewError(types.CodeInvalidArgument, "unknown priority %q", string(task.Priority))
	}
	if task.Cost <= 0 {
		task.Cost = 1
	}
	if task.ID == "" {
		task.ID = types.NewID(types.PrefixTask)
	}
	if task.Kind == types.TaskKindToolCall {
		s.mu.Lock()
		resolver := s.resolver
		s.mu.Unlock()
		if task.Resource == "" {
			task.Resource = resolver(task.ToolName)
		}
		if !task.Resource.Valid() {
			return task, types.NewError(types.CodeInvalidArgument, "unknown resource class %q", string(task.Resource))
		}
	}
	return task, nil
}

// kick nudges the dispatcher, coalescing rapid submits into one wakeup.
func (s *Scheduler) kick() {
	s.startOnce.Do(func() { go s.dispatchLoop() })
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// dispatchLoop is the single goroutine that owns task dispatch. Having one
// loop (rather than one goroutine per task racing to dequeue) is what makes
// the fair queue's virtual-time selection meaningful: admission order and
// dispatch order are decided in one place.
//
// The loop retries on a ticker rather than on time.After: the polling path runs
// on every idle iteration, and time.After would allocate a fresh timer each
// time. One ticker is created here and stopped on exit, which is also what keeps
// the "every timer has a Stop" requirement checkable by inspection.
func (s *Scheduler) dispatchLoop() {
	defer close(s.dispatcherDone)

	tick := time.NewTicker(dispatchPollInterval)
	defer tick.Stop()

	for {
		task := s.fair.Dequeue()
		if task == nil {
			select {
			case <-s.wake:
				continue
			case <-tick.C:
				// Poll as a safety net: a task may become dispatchable because
				// a session lease was released, which does not kick the wake
				// channel. The interval bounds that latency and the loop is
				// otherwise idle-cheap.
				s.mu.Lock()
				closed := s.closed
				s.mu.Unlock()
				if closed && s.fair.Len() == 0 {
					return
				}
				continue
			}
		}
		s.wg.Add(1)
		go s.runTask(task)
	}
}

// runTask acquires the remaining gates and executes one task. Every
// acquisition has a matching release on every exit path, including the panic
// path, which is what keeps the limits exact over a long run.
func (s *Scheduler) runTask(task *Task) {
	defer s.wg.Done()
	defer s.inflight.Add(-1)
	// Own the fair-queue lease for the whole execution so the per-session
	// in-flight count reflects running tools, not merely dequeued ones.
	defer s.fair.Release(task)
	defer s.budget.ReleaseQueue(task.Kind)

	// A run cancelled while this task was queued must not start: the cancel may
	// have happened after CancelRun's snapshot, so the flag is re-checked here
	// rather than trusted to have been seen.
	if s.runCancelled(task.RunID) {
		s.finish(task, types.NewError(types.CodeCancelled, "run %s was cancelled", task.RunID))
		return
	}

	// Layer 3: the resource lease. Acquired before the global gate so a task
	// starving on a scarce resource does not hold a running slot.
	var lease *Lease
	if task.Kind == types.TaskKindToolCall {
		ctx, cancel := context.WithCancel(context.Background())
		l, err := s.resources.Acquire(ctx, task.Resource)
		cancel()
		if err != nil {
			s.finish(task, types.WrapError(types.CodeOf(err), err, "resource lease"))
			return
		}
		lease = l
		defer lease.Release()
	}

	// Layer 1: the global running gate. Failing fast here (rather than
	// blocking) is what keeps a burst bounded: the queue caps already limit
	// how much can be waiting behind it.
	select {
	case s.gate <- struct{}{}:
	default:
		s.finish(task, types.NewError(types.CodeAdmissionRejected,
			"global running limit reached (%d)", s.cfg.MaxRunningTasks))
		return
	}
	defer func() { <-s.gate }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.mu.Lock()
	runner := s.runner
	if _, cancelled := s.cancelledRuns[task.RunID]; cancelled {
		s.mu.Unlock()
		s.finish(task, types.NewError(types.CodeCancelled, "run %s was cancelled", task.RunID))
		return
	}
	rt := &runningTask{task: *task, cancel: cancel, done: make(chan struct{}), started: time.Now()}
	s.running[task.ID] = rt
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.running, task.ID)
		s.mu.Unlock()
		close(rt.done)
	}()

	s.dispatched.Add(1)
	s.budget.StartRun(task.RunID, rt.started)
	s.emit(Notification{Kind: NotifyAdmitted, TaskID: task.ID, RunID: task.RunID,
		SessionID: task.SessionID, Priority: task.Priority})

	err := s.runContained(ctx, task, runner)
	s.finish(task, err)
}

// runCancelled reports whether a run has been cancelled.
func (s *Scheduler) runCancelled(runID string) bool {
	if runID == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, cancelled := s.cancelledRuns[runID]
	return cancelled
}

// runContained invokes the runner and converts a panic into an error. This is
// the worker/tool boundary, where recovering is correct and required.
func (s *Scheduler) runContained(ctx context.Context, task *Task, runner Runner) (err error) {
	defer func() {
		if r := recover(); r != nil {
			stack := debug.Stack()
			s.mu.Lock()
			hook := s.panicHook
			s.mu.Unlock()
			n := Notification{Kind: NotifyFailed, TaskID: task.ID, RunID: task.RunID,
				SessionID: task.SessionID, Priority: task.Priority,
				Err: types.NewError(types.CodePanicIsolated, "task runner panicked: %v", r),
				At:  time.Now()}
			if hook != nil {
				hook(n, r, stack)
			}
			err = types.NewError(types.CodePanicIsolated, "task runner panicked: %v", r)
		}
	}()
	if runner == nil {
		return types.NewError(types.CodeInternal, "scheduler has no runner installed")
	}
	return runner.Run(ctx, *task)
}

// finish records a task's outcome, releases its waiter and notifies observers.
func (s *Scheduler) finish(task *Task, err error) {
	s.mu.Lock()
	ch := s.waits[task.ID]
	delete(s.waits, task.ID)
	notify := s.notify
	s.mu.Unlock()

	switch {
	case err == nil:
		s.completed.Add(1)
	case types.IsCancelled(err):
		// A cancelled task is neither a success nor a failure.
	case types.CodeOf(err) == types.CodeAdmissionRejected || types.CodeOf(err) == types.CodeResourceUnavailable:
		s.rejected.Add(1)
	default:
		s.failedTask.Add(1)
	}

	if notify != nil {
		var n Notification
		switch {
		case err == nil:
			n = Notification{Kind: NotifyCompleted, TaskID: task.ID, RunID: task.RunID,
				SessionID: task.SessionID, Priority: task.Priority, At: time.Now()}
		case types.IsCancelled(err):
			n = Notification{Kind: NotifyCancelled, TaskID: task.ID, RunID: task.RunID,
				SessionID: task.SessionID, Priority: task.Priority, At: time.Now()}
		default:
			n = Notification{Kind: NotifyFailed, TaskID: task.ID, RunID: task.RunID,
				SessionID: task.SessionID, Priority: task.Priority, At: time.Now(),
				Err: types.WrapError(types.CodeOf(err), err, "task failed")}
		}
		s.report(notify, n)
	}

	if ch != nil {
		// Buffered, so this never blocks even if the waiter gave up.
		ch <- err
	}
	// Release the waiter's context deadline if the dispatcher is idle.
	s.kick()
}

// reject reports a task refused by a gate.
func (s *Scheduler) reject(task Task, err error) {
	s.rejected.Add(1)
	s.emit(Notification{Kind: NotifyRejected, TaskID: task.ID, RunID: task.RunID,
		SessionID: task.SessionID, Priority: task.Priority,
		Err: types.WrapError(types.CodeOf(err), err, "rejected")})
}

func (s *Scheduler) emit(n Notification) {
	if n.At.IsZero() {
		n.At = time.Now()
	}
	s.mu.Lock()
	notify := s.notify
	s.mu.Unlock()
	s.report(notify, n)
}

func (s *Scheduler) report(notify NotifyFunc, n Notification) {
	if notify == nil {
		return
	}
	// An observer is an external boundary: a panic in callback code supplied by
	// another component must not take the scheduler down. It is contained and
	// counted rather than silently swallowed, so a broken observer is visible.
	defer func() {
		if r := recover(); r != nil {
			s.observerPanics.Add(1)
		}
	}()
	notify(n)
}

// Cancel cancels a queued or running task.
func (s *Scheduler) Cancel(taskID string) error {
	if taskID == "" {
		return types.NewError(types.CodeInvalidArgument, "task id must not be empty")
	}
	if s.fair.Cancel(taskID) {
		s.mu.Lock()
		ch := s.waits[taskID]
		delete(s.waits, taskID)
		s.mu.Unlock()
		s.budget.ReleaseQueue(types.TaskKindToolCall)
		s.inflight.Add(-1)
		s.emit(Notification{Kind: NotifyCancelled, TaskID: taskID})
		if ch != nil {
			ch <- types.NewError(types.CodeCancelled, "task %s cancelled while queued", taskID)
		}
		return nil
	}
	s.mu.Lock()
	rt, ok := s.running[taskID]
	s.mu.Unlock()
	if !ok {
		return types.NewError(types.CodeNotFound, "task %s is not queued or running", taskID)
	}
	rt.cancel()
	return nil
}

// CancelRun cancels every task belonging to a run, queued or running. It
// returns how many were affected.
//
// The run is marked cancelled *before* the snapshot of running tasks is taken,
// so a task that is dispatched concurrently is still seen as cancelled by
// runTask's own re-check. Without the mark there would be a window in which a
// task dequeued between the snapshot and the cancel survives.
func (s *Scheduler) CancelRun(runID string) int {
	if runID == "" {
		return 0
	}

	s.mu.Lock()
	if s.cancelledRuns == nil {
		s.cancelledRuns = make(map[string]struct{})
	}
	s.cancelledRuns[runID] = struct{}{}
	victims := make([]*runningTask, 0, len(s.running))
	for _, rt := range s.running {
		if rt.task.RunID == runID {
			victims = append(victims, rt)
		}
	}
	s.mu.Unlock()

	// Only tool-call tasks are tracked in the fair queue per run; the run's own
	// slot is counted separately.
	queued := s.fair.CancelRun(runID)
	// Every dropped task must release its queue reservation and its in-flight
	// count, or the depth accounting and Wait would never settle.
	for i := 0; i < queued; i++ {
		s.budget.ReleaseQueue(types.TaskKindToolCall)
		s.inflight.Add(-1)
	}

	for _, rt := range victims {
		rt.cancel()
	}
	return queued + len(victims)
}

// ClearRunCancellation forgets a run's cancellation mark, so a run that is
// subsequently resumed can schedule new work.
func (s *Scheduler) ClearRunCancellation(runID string) {
	if runID == "" {
		return
	}
	s.mu.Lock()
	delete(s.cancelledRuns, runID)
	s.mu.Unlock()
}

// Stats returns a consistent snapshot of all three layers.
func (s *Scheduler) Stats() types.SchedulerStats {
	s.mu.Lock()
	running := len(s.running)
	s.mu.Unlock()

	snap := s.budget.Snapshot()
	st := types.SchedulerStats{
		RunningTasks:       running,
		MaxRunningTasks:    s.cfg.MaxRunningTasks,
		QueuedTasks:        snap.RunQueue,
		MaxQueuedTasks:     s.cfg.Limits.MaxQueuedRuns,
		QueuedToolCalls:    snap.ToolQueue,
		MaxQueuedToolCalls: s.cfg.Limits.MaxQueuedToolCalls,
		TotalDispatched:    s.dispatched.Load(),
		TotalCompleted:     s.completed.Load(),
		TotalRejected:      s.rejected.Load(),
		ObserverPanics:     s.observerPanics.Load(),
		ByPriority:         make(map[types.Priority]types.QueueStats, len(types.AllPriorities)),
		Resources:          make(map[types.ResourceClass]types.ResourceStats, len(types.AllResourceClasses)),
	}
	for _, q := range s.fair.Stats() {
		st.ByPriority[q.Priority] = q
	}
	for _, r := range s.resources.Stats() {
		st.Resources[r.Resource] = r
	}
	return st
}

// QueueDepth reports how many tasks have passed Submit but not yet finished.
func (s *Scheduler) QueueDepth() int { return s.fair.Len() }

// RunningCount reports how many tasks are executing.
func (s *Scheduler) RunningCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.running)
}

// Wait blocks until every accepted task has finished, or ctx is done.
//
// It waits on the in-flight counter rather than on wg alone, because a task can
// be accepted and queued before any dispatch goroutine exists for it: waiting
// on wg alone would report "idle" for work that has not started.
func (s *Scheduler) Wait(ctx context.Context) error {
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		if s.inflight.Load() <= 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return types.WrapError(types.CodeDeadlineExceeded, ctx.Err(), "waiting for scheduler drain")
		case <-tick.C:
		}
	}
}

// Close stops accepting work, cancels everything in flight and waits for the
// dispatch goroutines to exit. The scheduler cannot be reused afterwards.
func (s *Scheduler) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	victims := make([]*runningTask, 0, len(s.running))
	for _, rt := range s.running {
		victims = append(victims, rt)
	}
	s.mu.Unlock()

	// Starting the dispatcher here guarantees it observes the closed flag even
	// if no task was ever submitted.
	s.kick()

	// Wake anything blocked on a resource so its goroutine can exit.
	s.resources.Close()
	for _, rt := range victims {
		rt.cancel()
	}

	// Abandon whatever is still queued. Each abandoned task must release its
	// queue reservation and in-flight count, and fail its waiter, or Close
	// would hang waiting for work that will never run.
	for _, task := range s.fair.DrainAll() {
		s.budget.ReleaseQueue(task.Kind)
		s.inflight.Add(-1)
		s.mu.Lock()
		ch := s.waits[task.ID]
		delete(s.waits, task.ID)
		s.mu.Unlock()
		if ch != nil {
			ch <- types.NewError(types.CodeEngineClosed, "scheduler closed before task %s ran", task.ID)
		}
		s.emit(Notification{Kind: NotifyCancelled, TaskID: task.ID, RunID: task.RunID,
			SessionID: task.SessionID, Priority: task.Priority})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = s.Wait(ctx)

	select {
	case <-s.dispatcherDone:
	case <-time.After(time.Second):
	}
}

// Prepare normalises a task the way Submit would, without submitting it. The
// engine uses it to learn a tool call's assigned ID and resource class before
// recording the corresponding event.
func (s *Scheduler) Prepare(task Task) (Task, error) { return s.prepare(task) }

// ToolRequestFromTask builds the ports.ToolRequest for a tool-call task,
// keeping the task→request mapping in one place.
func ToolRequestFromTask(task Task, args map[string]any, timeout time.Duration, autoMode string) ports.ToolRequest {
	return ports.ToolRequest{
		RunID:         task.RunID,
		SessionID:     task.SessionID,
		ToolCallID:    task.ID,
		ToolName:      task.ToolName,
		Arguments:     args,
		Timeout:       timeout,
		AutoModeLevel: autoMode,
		Resource:      task.Resource,
	}
}

// Compile-time assertions that Scheduler satisfies the published contracts: the
// ch. 32 Scheduler interface (as a local literal, so this package does not
// depend on the alias) and the shared declaration in internal/types that task 01
// uses. A signature drift breaks the build here rather than at integration.
var _ interface {
	Submit(Task) error
	Cancel(string) error
	Stats() types.SchedulerStats
} = (*Scheduler)(nil)

var _ types.Scheduler = (*Scheduler)(nil)
