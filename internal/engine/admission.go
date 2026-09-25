package engine

import (
	"context"
	"sync"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/scheduler"
	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// This file is the global admission-control entry point: the single funnel
// every run passes through before any work is scheduled.
//
// Admission answers one question — "may this run start?" — using the outer
// edge of the three-layer limiter (doc ch. 6). It is deliberately separate from
// the scheduler's own Submit, because admission must also enforce per-session
// and per-tenant fairness that the task-level scheduler has no view of:
//
//   - the global running-run ceiling (layer 1),
//   - a per-session running-run ceiling, so one session cannot occupy the whole
//     machine,
//   - the hard queue caps from QueueLimits,
//   - the run duration budget.
//
// A rejection is always "fast": the caller gets a coded error immediately
// rather than being parked in a queue that could grow to OOM.

// AdmissionConfig configures the admission controller.
type AdmissionConfig struct {
	// MaxRunningRuns is layer 1 for runs. Zero uses
	// SchedulerConfig.MaxRunningTasks.
	MaxRunningRuns int
	// MaxRunningRunsPerSession bounds how many of one session's runs may be
	// in flight at once, so a single chatty session cannot starve the rest.
	MaxRunningRunsPerSession int
	// Limits holds the hard queue caps.
	Limits types.QueueLimits
}

// DefaultAdmissionConfig returns the documented configuration: the global
// ceiling is the task book's 32, and a single session may hold a quarter of it.
func DefaultAdmissionConfig() AdmissionConfig {
	return AdmissionConfig{
		MaxRunningRuns:           types.DefaultMaxRunningTasks,
		MaxRunningRunsPerSession: types.DefaultMaxRunningTasks / 4,
		Limits:                   types.DefaultQueueLimits(),
	}
}

// Validate rejects an unusable configuration.
func (c AdmissionConfig) Validate() error {
	if c.MaxRunningRuns <= 0 {
		return types.NewError(types.CodeInvalidArgument, "MaxRunningRuns must be positive, got %d", c.MaxRunningRuns)
	}
	if c.MaxRunningRunsPerSession <= 0 {
		return types.NewError(types.CodeInvalidArgument,
			"MaxRunningRunsPerSession must be positive, got %d", c.MaxRunningRunsPerSession)
	}
	return c.Limits.Validate()
}

// AdmissionTicket is the right to run, granted by Admit and released exactly
// once by Release (directly or through Done).
type AdmissionTicket struct {
	// RunID identifies the admitted run.
	RunID     string
	SessionID string
	// AdmittedAt is when the ticket was granted, used to enforce the duration
	// budget from a single authority.
	AdmittedAt time.Time

	ctrl     *Admission
	released bool
	mu       sync.Mutex
}

// Release returns the ticket's capacity. It is idempotent, so a deferred
// release plus an explicit early release cannot double-free a slot.
func (t *AdmissionTicket) Release() {
	if t == nil || t.ctrl == nil {
		return
	}
	t.mu.Lock()
	if t.released {
		t.mu.Unlock()
		return
	}
	t.released = true
	t.mu.Unlock()
	t.ctrl.release(t)
}

// Deadline returns the instant the run's duration budget expires.
func (t *AdmissionTicket) Deadline() time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.AdmittedAt.Add(t.ctrl.cfg.Limits.MaxRunDuration)
}

// Remaining returns how much of the duration budget is left.
func (t *AdmissionTicket) Remaining(now time.Time) time.Duration {
	d := t.Deadline().Sub(now)
	if d < 0 {
		return 0
	}
	return d
}

// Admission is the global admission controller.
type Admission struct {
	cfg    AdmissionConfig
	budget *scheduler.Budget

	mu sync.Mutex
	// running and perSession account the granted tickets.
	running    int
	perSession map[string]int
	// closed rejects new admissions during shutdown.
	closed bool

	// counters
	granted          uint64
	rejected         uint64
	durationExceeded uint64
}

// NewAdmission builds an admission controller sharing the scheduler's budget,
// so queue caps are enforced from one accountant rather than two that could
// disagree.
func NewAdmission(cfg AdmissionConfig, budget *scheduler.Budget) (*Admission, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if budget == nil {
		budget = scheduler.NewBudget(cfg.Limits)
	}
	return &Admission{
		cfg:        cfg,
		budget:     budget,
		perSession: make(map[string]int),
	}, nil
}

// Admit decides whether a run may start, returning a ticket on success.
//
// Checks are ordered cheapest-and-most-likely-to-reject first: shutdown, then
// the global ceiling, then the per-session ceiling, then the queue caps. A
// rejection never consumes capacity.
func (a *Admission) Admit(ctx context.Context, req types.SubmitRequest, runID, sessionID string) (*AdmissionTicket, error) {
	now := time.Now()

	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil, types.NewError(types.CodeEngineClosed, "engine is shutting down")
	}
	if a.running >= a.cfg.MaxRunningRuns {
		a.rejected++
		running := a.running
		a.mu.Unlock()
		return nil, types.NewError(types.CodeAdmissionRejected,
			"global running-run limit reached (%d/%d)", running, a.cfg.MaxRunningRuns)
	}
	if n := a.perSession[sessionID]; n >= a.cfg.MaxRunningRunsPerSession {
		a.rejected++
		a.mu.Unlock()
		return nil, types.NewError(types.CodeAdmissionRejected,
			"session %s running-run limit reached (%d/%d)", sessionID, n, a.cfg.MaxRunningRunsPerSession)
	}

	// The queue cap is checked through the shared budget so the scheduler's
	// tool-call queue and this run queue cannot each believe they have room.
	if err := a.budget.AdmitQueue(types.TaskKindRun); err != nil {
		a.rejected++
		a.mu.Unlock()
		return nil, err
	}

	a.running++
	a.perSession[sessionID]++
	a.granted++
	a.mu.Unlock()

	ticket := &AdmissionTicket{
		RunID: runID, SessionID: sessionID,
		AdmittedAt: now, ctrl: a,
	}
	a.budget.StartRun(runID, now)
	return ticket, nil
}

// release returns a ticket's capacity.
func (a *Admission) release(t *AdmissionTicket) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.running > 0 {
		a.running--
	}
	if n := a.perSession[t.SessionID]; n <= 1 {
		delete(a.perSession, t.SessionID)
	} else {
		a.perSession[t.SessionID] = n - 1
	}
	a.budget.ForgetRun(t.RunID)
}

// CheckDuration verifies that an admitted run has not exceeded its wall-clock
// budget. The engine calls it at round boundaries; a run past its budget is
// failed with CodeRunDurationExceeded rather than being allowed to run forever.
func (a *Admission) CheckDuration(runID string, now time.Time) error {
	if _, err := a.budget.CheckDuration(runID, now); err != nil {
		a.mu.Lock()
		a.durationExceeded++
		a.mu.Unlock()
		return err
	}
	return nil
}

// RunningRuns reports the current global running-run count.
func (a *Admission) RunningRuns() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.running
}

// RunningRunsFor returns how many runs a session has in flight.
func (a *Admission) RunningRunsFor(sessionID string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.perSession[sessionID]
}

// AdmissionStats reports admission counters.
type AdmissionStats struct {
	Running          int    `json:"running"`
	MaxRunning       int    `json:"maxRunning"`
	MaxPerSession    int    `json:"maxPerSession"`
	Sessions         int    `json:"sessions"`
	Granted          uint64 `json:"granted"`
	Rejected         uint64 `json:"rejected"`
	DurationExceeded uint64 `json:"durationExceeded"`
}

// Stats snapshots the controller.
func (a *Admission) Stats() AdmissionStats {
	a.mu.Lock()
	defer a.mu.Unlock()
	return AdmissionStats{
		Running:          a.running,
		MaxRunning:       a.cfg.MaxRunningRuns,
		MaxPerSession:    a.cfg.MaxRunningRunsPerSession,
		Sessions:         len(a.perSession),
		Granted:          a.granted,
		Rejected:         a.rejected,
		DurationExceeded: a.durationExceeded,
	}
}

// SaturationReport reports a resource class's current utilisation, so a caller
// can decide whether to defer a tool call.
type SaturationReport struct {
	Resource types.ResourceClass `json:"resource"`
	InUse    int                 `json:"inUse"`
	Capacity int                 `json:"capacity"`
	Waiting  int                 `json:"waiting"`
	// Saturated is true when every unit of the class is held.
	Saturated bool `json:"saturated"`
}

// Saturation reports the current utilisation of every resource class.
//
// It is the read-only companion to AdmitToolCall: a runtime that wants to defer
// work rather than attempt and be refused can consult this instead.
func (e *Engine) Saturation() []SaturationReport {
	stats := e.sched.Resources().Stats()
	out := make([]SaturationReport, 0, len(stats))
	for _, s := range stats {
		out = append(out, SaturationReport{
			Resource:  s.Resource,
			InUse:     s.InUse,
			Capacity:  s.Capacity,
			Waiting:   s.Waiting,
			Saturated: s.InUse >= s.Capacity,
		})
	}
	return out
}

// Close stops admitting new runs. Already-granted tickets stay valid so
// in-flight runs can finish or be cancelled deliberately.
func (a *Admission) Close() {
	a.mu.Lock()
	a.closed = true
	a.mu.Unlock()
}

// ---------------------------------------------------------------------------
// inbound contract: the tool runtime's admission hook
// ---------------------------------------------------------------------------

// ToolAdmissionRequest is the shape task 04's `tool.AdmissionController`
// interface passes for a per-tool-call decision.
//
// Why this is a local mirror rather than an import: task 04 declares that
// interface (with its own `AdmissionRequest` and `ResourceKind` types) in package
// `tool`, and task 02 must not depend on task 04's package — task 04 already
// depends on task 02 for this hook, so importing would be a cycle once the
// modules are merged. Go interface satisfaction is structural, but a method
// parameter is not: to satisfy their interface the method must accept *their*
// struct.
//
// Registered for task 08 arbitration. The provisional resolution is that this
// type be lifted into internal/types, at which point AdmitToolCall below
// satisfies task 04's interface unchanged, because the method name and the
// semantics already match.
type ToolAdmissionRequest struct {
	SessionID  string
	RunID      string
	ToolCallID string
	ToolName   string
	Resource   types.ResourceClass
}

// AdmitToolCall applies the layer-2 and layer-3 limits to a single tool call.
//
// Task 04 calls this before executing a tool so a call exceeding the per-session
// or per-resource ceiling is refused at the runtime boundary as well as in the
// scheduler. A refusal here is a fast rejection, not a wait: the scheduler owns
// queueing, and a second queue inside the runtime would be a second unbounded
// one, which the architecture forbids.
//
// The check consults current utilisation without reserving anything. A caller
// that ignores the answer still meets the scheduler's own gate, so this cannot
// be used to exceed a limit — it only lets the runtime fail earlier and more
// cheaply.
//
// This is an RPC-style boundary: a panic in the caller's code path is contained
// elsewhere, but this method itself does no work that can panic beyond map and
// slice access on validated input.
func (e *Engine) AdmitToolCall(ctx context.Context, req ToolAdmissionRequest) error {
	if err := ctx.Err(); err != nil {
		return types.WrapError(types.CodeCancelled, err, "admission cancelled for tool call %s", req.ToolCallID)
	}
	if req.ToolName == "" {
		return types.NewError(types.CodeInvalidArgument, "admission requires a tool name")
	}

	// Layer 2: the per-session tool ceiling.
	if req.SessionID != "" {
		inFlight := e.sched.FairQueue().InFlight(req.SessionID)
		if inFlight >= e.cfg.Scheduler.MaxConcurrentToolsPerSession {
			return types.NewError(types.CodeAdmissionRejected,
				"session %s already has %d tool calls in flight (limit %d)",
				req.SessionID, inFlight, e.cfg.Scheduler.MaxConcurrentToolsPerSession)
		}
	}

	// Layer 3: the resource ceiling. A saturated class is reported so the
	// runtime can defer rather than pile more work onto the pool.
	resource := req.Resource
	if resource == "" {
		resource = scheduler.DefaultToolResource(req.ToolName)
	}
	if !resource.Valid() {
		return types.NewError(types.CodeInvalidArgument, "unknown resource class %q", string(resource))
	}
	inUse := e.sched.Resources().InUse(resource)
	capacity := e.sched.Resources().Capacity(resource)
	if inUse >= capacity {
		return types.NewError(types.CodeResourceUnavailable,
			"resource %q is saturated (%d/%d)", string(resource), inUse, capacity)
	}
	return nil
}
