// Package engine implements the Engine from architecture doc ch. 32: the
// submit/cancel/resume/observe surface the UI host (task 01) calls, and the
// run orchestrator that drives the agent loop.
//
// The pieces and how they fit:
//
//	Engine
//	  ├─ Admission      global gate: may this run start? (layer 1 for runs)
//	  ├─ SessionActor   one goroutine owning one session's writable state
//	  ├─ Scheduler      three-layer limiter: gate → fair queue → resource pool
//	  ├─ agent.Loop     the Think→Tool Calls→Observe state machine
//	  ├─ Recovery       crash recovery: verify, checkpoint, classify, resume
//	  └─ eventBus + coalescer   backpressure: durable vs mergeable delivery
//
// Two rules shape almost every decision here:
//
//   - One writer per session. All session mutation goes through that session's
//     actor goroutine (doc ch. 5).
//   - Core goroutines never recover. The actor loop and the engine's run
//     goroutines run under agent.PanicGuard.RunCore, so a panic produces a
//     crash dump and a process exit, never a silently corrupted continuation
//     (doc ch. 3.2).
package engine

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/agent"
	"github.com/ximo888ok-netizen/ximo-agent/internal/expert"
	"github.com/ximo888ok-netizen/ximo-agent/internal/ports"
	"github.com/ximo888ok-netizen/ximo-agent/internal/scheduler"
	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// EngineConfig is the engine's full configuration.
type EngineConfig struct {
	types.EngineConfig
	// Admission configures the global admission controller.
	Admission AdmissionConfig
}

// DefaultEngineConfig returns the documented defaults.
func DefaultEngineConfig() EngineConfig {
	return EngineConfig{
		EngineConfig: types.DefaultEngineConfig(),
		Admission:    DefaultAdmissionConfig(),
	}
}

// Dependencies are the ports the engine needs. Only Events and Provider are
// strictly required; the rest degrade gracefully (a nil Outbox means events are
// not mirrored externally, a nil CheckpointStore means recovery resumes from the
// event log alone).
type Dependencies struct {
	Events      ports.EventStore
	Outbox      ports.OutboxStore
	Checkpoints ports.CheckpointStore
	Idempotency ports.IdempotencyStore
	Tools       ports.ToolRuntime
	Provider    ports.Provider
	Context     ports.ContextManager
	// Planner and Reviewer are optional agent collaborators.
	Planner  agent.Planner
	Reviewer agent.Reviewer
	// Confirmer resolves tool calls that need a human decision.
	Confirmer agent.Confirmer
	// Supervisor receives core-panic notifications (task 01).
	Supervisor agent.Supervisor
	// Experts is the expert registry the task-3 direct-activation path reads.
	// Nil means the engine builds the built-in registry itself, so the shipped
	// binary always has the full 254-expert catalogue; tests inject their own.
	Experts *expert.Registry
	// SubAgentPool 惰性取当前的子代理候选池（任务 05）。用函数而不是具体值：
	// 设置页改动服务商配置后池会整体失效重建，引擎每次专家 run 都要拿到最新
	// 的池。为 nil（测试 / 独立构建的引擎）时专家路径退回 Provider 单通道，
	// 行为与没有池时完全一致；取池出错或返回 nil 同样走单通道。
	SubAgentPool func() (expert.ModelPool, error)
	// SubAgentCandidates 返回某位专家（按 ID + 分类）的候选服务商 ID 优先级
	// 顺序（任务 05 的 by_expert / by_division 分配）。nil 时用整个池的默认
	// 顺序。
	SubAgentCandidates func(expertID, division string) []string
}

// Engine is the concrete implementation of the ch. 32 Engine interface.
type Engine struct {
	cfg   EngineConfig
	deps  Dependencies
	guard *agent.PanicGuard

	sched     *scheduler.Scheduler
	admission *Admission
	recovery  *Recovery
	bus       *eventBus
	coalescer *coalescer

	// expertRegistry 是专家库（任务3）。懒加载且加载后只读，因此可以安全地在
	// 多个 run 的 goroutine 之间共享。为 nil 表示本装配未启用专家能力。
	expertRegistry *expert.Registry

	mu sync.Mutex
	// sessions maps a session ID to its actor.
	sessions map[string]*SessionActor
	// runs maps a run ID to its engine-side record.
	runs map[string]*runRecord
	// recoverAttempts counts automatic recovery passes per run, so a
	// deterministically crashing run is parked rather than retried forever.
	recoverAttempts map[string]int
	// toolExecs holds the pending executor for a tool-call task, keyed by the
	// tool call ID the scheduler will dispatch.
	toolExecs map[string]*toolExec
	// closed guards shutdown.
	closed bool
	// wg tracks run goroutines.
	wg sync.WaitGroup

	// counters
	submitted uint64
	completed uint64
	failed    uint64
	cancelled uint64
	recovered uint64
}

// runRecord is the engine's per-run bookkeeping, distinct from the actor's
// materialized types.Run: it holds what only the engine needs, such as the
// conversation and the cancellation function.
//
// Locking: run is guarded by mu, because several goroutines touch it — the run's
// own goroutine updates it on completion, the event stream reads it to decide
// whether to keep waiting, and Cancel reads it to check for a terminal state.
// The immutable fields (the IDs, the priority) are written once at construction
// and are safe to read directly.
type runRecord struct {
	mu      sync.RWMutex
	run     types.Run
	request types.SubmitRequest
	// conv and machine belong to the run's own goroutine; only that goroutine
	// may use them. They are nil'd on terminal completion while holding
	// e.mu, which is why the run goroutine is the only writer.
	conv      *agent.Conversation
	machine   *agent.Machine
	actor     *SessionActor
	ticket    *AdmissionTicket
	cancel    context.CancelFunc
	done      chan struct{}
	startedAt time.Time

	// executing is the run's ownership claim (invariant I1): true while exactly
	// one goroutine is driving the run. It is claimed atomically before a run
	// goroutine is started and released when that goroutine finishes, which is
	// what stops two concurrent Resume calls from both starting an execution.
	//
	// A plain state check is not sufficient: thinking → thinking is a legal
	// self-transition (consecutive model rounds), so two resumers would both
	// pass a state-based guard. This flag is the guard that does not have that
	// hole.
	executing atomic.Bool

	// ---- task 4: user-visible plan mode ----
	//
	// planPending mirrors the machine's waiting_user park with its cause, which
	// the state alone cannot express: waiting_user is also how a tool
	// permission prompt and a crash-recovery adjudication park a run, and only
	// this run's own plan may be confirmed.
	//
	// These are guarded by mu rather than by the run's goroutine because a plan
	// is confirmed from an RPC goroutine while the run goroutine may be reading
	// them. The values cross between the two only inside executeRun and
	// ConfirmPlan, both of which take mu.
	planPending  bool
	planDecision agent.PlanDecision
	pendingPlan  string
	planRevision int
}

// claimExecution takes ownership of the run, reporting false when another
// goroutine already owns it.
func (r *runRecord) claimExecution() bool { return r.executing.CompareAndSwap(false, true) }

// releaseExecution gives up ownership so the run may be driven again (after a
// park) or is simply finished.
func (r *runRecord) releaseExecution() { r.executing.Store(false) }

// owned reports whether a goroutine currently owns the run.
func (r *runRecord) owned() bool { return r.executing.Load() }

// runID returns the run's immutable identifier.
func (r *runRecord) runID() string { return r.run.ID }

// sessionID returns the run's immutable session identifier.
func (r *runRecord) sessionID() string { return r.run.SessionID }

// priority returns the run's immutable QoS class.
func (r *runRecord) priority() types.Priority { return r.run.Priority }

// snapshot returns a copy of the materialized run.
func (r *runRecord) snapshot() types.Run {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.run.Clone()
}

// state returns the run's current state.
func (r *runRecord) state() types.RunState {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.run.State
}

// terminal reports whether the run has reached a terminal state.
func (r *runRecord) terminal() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.run.State.Terminal()
}

// currentRound returns the run's current round.
func (r *runRecord) currentRound() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.run.Round
}

// setState records the run's state.
func (r *runRecord) setState(s types.RunState) {
	r.mu.Lock()
	r.run.State = s
	r.mu.Unlock()
}

// setOutcome records a run's terminal payload.
func (r *runRecord) setOutcome(state types.RunState, answer string, err *types.Error) {
	r.mu.Lock()
	r.run.State = state
	r.run.Answer = answer
	if err != nil {
		r.run.Err = err
	}
	r.mu.Unlock()
}

// setLastSeq records the highest durable sequence for the run.
func (r *runRecord) setLastSeq(seq uint64) {
	r.mu.Lock()
	if seq > r.run.LastSeq {
		r.run.LastSeq = seq
	}
	r.mu.Unlock()
}

// releaseResources drops the heavy per-run state. Callers must hold e.mu so a
// concurrent reader of the engine's maps cannot observe a half-released record.
func (r *runRecord) releaseResources() {
	r.conv = nil
	r.machine = nil
}

// New builds an engine. It validates the configuration and installs the
// scheduler runner, so a run submitted immediately after New makes progress.
func New(cfg EngineConfig, deps Dependencies, guard *agent.PanicGuard) (*Engine, error) {
	if err := cfg.EngineConfig.Validate(); err != nil {
		return nil, err
	}
	if err := cfg.Admission.Validate(); err != nil {
		return nil, err
	}
	if deps.Events == nil {
		return nil, types.NewError(types.CodeInvalidArgument, "engine requires an event store")
	}
	if deps.Provider == nil {
		return nil, types.NewError(types.CodeInvalidArgument, "engine requires a provider")
	}
	if deps.Tools == nil {
		return nil, types.NewError(types.CodeInvalidArgument, "engine requires a tool runtime")
	}
	if guard == nil {
		guard = agent.NewPanicGuard(agent.GuardConfig{DumpDir: cfg.CrashDumpDir})
	}

	sched, err := scheduler.New(cfg.Scheduler)
	if err != nil {
		return nil, types.WrapError(types.CodeInvalidArgument, err, "invalid scheduler configuration")
	}
	admission, err := NewAdmission(cfg.Admission, sched.Budget())
	if err != nil {
		return nil, err
	}
	recovery, err := NewRecovery(RecoveryConfig{
		Events:              deps.Events,
		Checkpoints:         deps.Checkpoints,
		Idempotency:         deps.Idempotency,
		Outbox:              deps.Outbox,
		MaxRecoveryAttempts: cfg.MaxRecoveryAttempts,
		attempts:            nil, // wired below once the engine exists
	})
	if err != nil {
		return nil, err
	}

	// The expert registry is built here rather than required from the caller: the
	// catalogue is embedded in the binary and lazily parsed on first use, so a
	// plain engine always has the task-3 direct-activation path available.
	expertRegistry := deps.Experts
	if expertRegistry == nil {
		expertRegistry = expert.NewRegistry(nil)
	}

	e := &Engine{
		cfg:             cfg,
		deps:            deps,
		guard:           guard,
		sched:           sched,
		admission:       admission,
		recovery:        recovery,
		bus:             newEventBus(cfg.Backpressure),
		expertRegistry:  expertRegistry,
		sessions:        make(map[string]*SessionActor),
		runs:            make(map[string]*runRecord),
		recoverAttempts: make(map[string]int),
		toolExecs:       make(map[string]*toolExec),
	}
	e.coalescer = newCoalescer(cfg.Backpressure, e.bus)
	// The recovery attempt counter has to read engine state, so it is wired
	// after construction rather than passed in.
	e.recovery.cfg.attempts = e.recoveryAttemptsFor

	sched.SetRunner(scheduler.RunnerFunc(e.runTask))
	sched.SetNotifier(e.onSchedulerNotification)
	sched.SetPanicHook(func(n scheduler.Notification, r any, stack []byte) {
		// A panicking worker/tool runner is a contained panic, but it is still
		// worth a crash report so the failure is diagnosable after the fact.
		_ = e.guard
		ev := types.Event{
			RunID: n.RunID, SessionID: n.SessionID,
			Type: types.EventError, Timestamp: time.Now(),
			Message: types.RedactString("task runner panicked: " + toStr(r)),
			Data: map[string]any{
				"taskId": n.TaskID, "panicked": true, "stackBytes": len(stack),
			},
		}
		e.appendEventBestEffort(context.Background(), ev)
		e.coalescer.Publish(ev)
	})

	return e, nil
}

// ---------------------------------------------------------------------------
// public API (ch. 32)
// ---------------------------------------------------------------------------

// Submit accepts a run.
//
// Order: validate → admit (fast rejection if the machine is full) → register
// with the session actor → hand to the scheduler. Nothing is queued before
// admission succeeds, which is what makes rejection cheap and keeps an
// overflowing queue from consuming capacity it cannot use.
func (e *Engine) Submit(ctx context.Context, req types.SubmitRequest) (types.RunHandle, error) {
	if err := req.Validate(); err != nil {
		return types.RunHandle{}, err
	}
	req = req.Normalized()

	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return types.RunHandle{}, types.NewError(types.CodeEngineClosed, "engine is closed")
	}
	e.mu.Unlock()

	sessionID := req.SessionID
	if sessionID == "" {
		sessionID = types.NewID(types.PrefixSession)
	}
	runID := types.NewID(types.PrefixRun)

	actor, err := e.sessionActor(sessionID)
	if err != nil {
		return types.RunHandle{}, err
	}

	// Admission first: it is the cheapest gate and the one most likely to
	// reject under load.
	ticket, err := e.admission.Admit(ctx, req, runID, sessionID)
	if err != nil {
		return types.RunHandle{}, err
	}

	run := types.Run{
		ID: runID, SessionID: sessionID,
		State: types.StateCreated, Priority: req.Priority,
		Prompt: req.Prompt, Model: req.Model, Effort: req.Effort,
		LongTask:         req.LongTask,
		MaxRounds:        req.MaxRounds,
		MaxContinuations: 0,
		CreatedAt:        time.Now(), UpdatedAt: time.Now(),
	}
	if req.LongTask {
		run.MaxContinuations = e.cfg.Agent.LongTaskMaxContinuations
	}

	// Register with the actor so the run exists before any event references it:
	// the actor rejects transitions for unknown runs, which is what stops a
	// stray event from creating phantom state.
	if err := actor.RegisterRun(ctx, run); err != nil {
		ticket.Release()
		return types.RunHandle{}, err
	}

	conv := agent.NewConversation(req.SystemPrompt, req.Prompt, req.Tools)
	machine := agent.NewMachine(runID, sessionID, &machineSink{engine: e, actor: actor, sessionID: sessionID})

	runCtx, cancel := context.WithCancel(context.Background())
	rec := &runRecord{
		run: run, request: req, conv: conv, machine: machine,
		actor: actor, ticket: ticket, cancel: cancel, done: make(chan struct{}),
	}
	e.mu.Lock()
	e.runs[runID] = rec
	e.submitted++
	e.mu.Unlock()

	// Move to queued and record it durably before scheduling. If the process
	// dies right after this point, recovery finds a queued run and resumes it;
	// without the transition, the run would exist only in memory.
	if err := machine.Transition(ctx, types.StateQueued, "accepted"); err != nil {
		e.releaseRun(runID, ticket)
		return types.RunHandle{}, err
	}
	if err := actor.UpdateRun(ctx, runID, func(r *types.Run) {
		r.State = types.StateQueued
	}); err != nil {
		e.releaseRun(runID, ticket)
		return types.RunHandle{}, err
	}

	// Claim ownership before the run goroutine exists, so the invariant "a
	// running run has exactly one owner" holds from the first instant (I1).
	rec.claimExecution()

	e.wg.Add(1)
	// The run goroutine is a core goroutine: it owns this run's conversation and
	// machine. A panic here must not be recovered, so it runs under RunCore.
	go func() {
		defer e.wg.Done()
		defer close(rec.done)
		defer rec.releaseExecution()
		e.guard.RunCore("engine.run", agent.CrashMeta{
			RunID: runID, SessionID: sessionID, State: types.StateQueued,
		}, func() {
			e.executeRun(runCtx, rec)
		})
	}()

	return types.RunHandle{
		RunID: runID, SessionID: sessionID,
		State: types.StateQueued, Priority: req.Priority,
	}, nil
}

// liveRun returns the in-memory record for a run. When the run is not in memory
// it consults the durable log: a run that finished normally was evicted from
// memory on purpose, so "not in memory" does not mean "does not exist".
//
// The second return value distinguishes a run that exists but is terminal, so
// callers can report CodeTerminalState ("it already finished") rather than
// CodeNotFound ("no such run") — a distinction the UI needs in order to say
// something useful.
func (e *Engine) liveRun(ctx context.Context, runID string) (*runRecord, types.Run, error) {
	e.mu.Lock()
	rec, ok := e.runs[runID]
	e.mu.Unlock()
	if ok {
		return rec, types.Run{}, nil
	}
	// Not in memory: it may be a finished run from this process or a previous
	// one. Rebuild it from the log so the caller can tell the two cases apart.
	run, err := e.loadRunFromLog(ctx, runID)
	if err != nil {
		return nil, types.Run{}, err
	}
	return nil, run, nil
}

// Cancel stops a run. A queued or running run is cancelled; a terminal run is
// reported as such rather than silently accepted.
func (e *Engine) Cancel(ctx context.Context, runID string) error {
	rec, archived, err := e.liveRun(ctx, runID)
	if err != nil {
		return err
	}
	if rec == nil {
		return types.NewError(types.CodeTerminalState,
			"run %s is already %s", runID, string(archived.State))
	}
	if rec.terminal() {
		return types.NewError(types.CodeTerminalState,
			"run %s is already %s", runID, string(rec.state()))
	}

	// Cancel scheduler work first so no new tool call starts, then the run.
	e.sched.CancelRun(runID)
	rec.cancel()

	// The machine transition is what records the cancellation durably. It may
	// fail if the run raced to completion, which is not an error worth
	// surfacing: the run ended either way.
	if err := rec.machine.Transition(ctx, types.StateCancelled, "cancelled by request"); err != nil {
		if types.CodeOf(err) == types.CodeTerminalState {
			return nil
		}
		return err
	}
	if err := rec.actor.CloseRun(ctx, runID, types.StateCancelled, "", nil); err != nil {
		return err
	}
	e.mu.Lock()
	e.cancelled++
	e.mu.Unlock()
	return nil
}

// Resume continues a run that is parked in waiting_user.
//
// Resuming is explicit rather than automatic because the reason a run is parked
// is usually that a human decision is required — most often whether to replay a
// non-idempotent tool call whose outcome is unknown after a crash.
func (e *Engine) Resume(ctx context.Context, runID string) error {
	rec, archived, err := e.liveRun(ctx, runID)
	if err != nil {
		return err
	}
	if rec == nil {
		return types.NewError(types.CodeTerminalState,
			"run %s is %s and cannot be resumed", runID, string(archived.State))
	}

	// Read the authoritative state from the actor's materialized record rather
	// than the engine's cached copy: the actor is the single writer, so its
	// view is the one that decides whether a resume is legal.
	run, err := e.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	if run.State.Terminal() {
		return types.NewError(types.CodeTerminalState,
			"run %s is %s and cannot be resumed", runID, string(run.State))
	}
	if run.State != types.StateWaitingUser && run.State != types.StateRecovering {
		return types.NewError(types.CodeNotResumable,
			"run %s is %s, only a waiting_user or recovering run can be resumed",
			runID, string(run.State))
	}

	// The uncertain calls are resolved by the user's decision to resume, so the
	// park's reason is cleared and the calls are treated as approved for replay.
	if len(run.UncertainToolCalls) > 0 {
		uncertain := append([]string(nil), run.UncertainToolCalls...)
		if err := rec.actor.UpdateRun(ctx, runID, func(r *types.Run) {
			r.UncertainToolCalls = nil
			r.WaitingReason = ""
		}); err != nil {
			return err
		}
		if err := e.recordResumeApproval(ctx, rec, uncertain); err != nil {
			return err
		}
	}

	// Claim ownership before doing anything else that commits to resuming. A
	// concurrent Resume loses the race here and reports "already running" rather
	// than starting a second execution goroutine for the same run (I1).
	//
	// This is checked *before* the transition so a loser cannot emit a
	// misleading "resumed by user" event for a run it does not own.
	if !rec.claimExecution() {
		return types.NewError(types.CodeAlreadyExists,
			"run %s is already being executed", runID)
	}

	if err := rec.machine.Transition(ctx, types.StateThinking, "resumed by user"); err != nil {
		rec.releaseExecution()
		return err
	}

	// A run cancelled earlier must be allowed to schedule new work again.
	e.sched.ClearRunCancellation(runID)

	e.mu.Lock()
	runCtx, cancel := context.WithCancel(context.Background())
	if rec.cancel != nil {
		// Release the parked run's old cancellation function; leaving it
		// installed would leak the context it holds.
		rec.cancel()
	}
	rec.cancel = cancel
	rec.done = make(chan struct{})
	done := rec.done
	e.wg.Add(1)
	e.mu.Unlock()
	// The record's own lock is taken after releasing the engine lock, keeping
	// the only ever lock ordering e.mu → rec.mu.
	rec.setState(types.StateThinking)

	go func() {
		defer e.wg.Done()
		defer close(done)
		// Ownership is released here rather than in finishRun so that a run
		// parked *again* (a second crash-recovery pass) can be resumed once
		// more. finishRun clears it only for the paths that do not come back.
		defer rec.releaseExecution()
		e.guard.RunCore("engine.run.resume", agent.CrashMeta{
			RunID: runID, SessionID: rec.sessionID(), State: types.StateThinking,
		}, func() {
			e.executeRun(runCtx, rec)
		})
	}()
	return nil
}

// GetRun returns a run's current state.
func (e *Engine) GetRun(ctx context.Context, runID string) (types.Run, error) {
	e.mu.Lock()
	rec, ok := e.runs[runID]
	e.mu.Unlock()
	if ok {
		// The actor is the authority for the materialized record, so read from
		// it rather than from the engine's cached copy.
		snap, err := rec.actor.Snapshot(ctx)
		if err == nil {
			if r, ok := snap[runID]; ok {
				return r, nil
			}
		}
		return rec.snapshot(), nil
	}
	// Not in memory: it may be a run from a previous process. Rebuild its state
	// from the durable log so a UI reconnect after an Engine restart works.
	return e.loadRunFromLog(ctx, runID)
}

// Events returns a run's event stream.
//
// The stream is assembled from two sources, in this order: the durable log
// (everything after afterSeq) and then live delivery. Subscribing *before*
// reading the log is what makes the two halves join without a gap — a live
// event published while the log is being read is buffered in the subscription
// and discarded once the log is caught up, because it is delivered with a
// sequence number the log read has already covered.
func (e *Engine) Events(ctx context.Context, runID string, afterSeq uint64) (<-chan types.Event, error) {
	sub, err := e.bus.Subscribe(runID, afterSeq)
	if err != nil {
		return nil, err
	}

	out := make(chan types.Event, e.cfg.Backpressure.StreamBuffer)
	go e.streamRun(ctx, runID, afterSeq, sub, out)
	return out, nil
}

// streamRun joins the durable history and the live stream into one channel.
func (e *Engine) streamRun(ctx context.Context, runID string, afterSeq uint64, sub *subscriber, out chan<- types.Event) {
	defer close(out)
	defer e.bus.Unsubscribe(sub)

	var lastSeq = afterSeq
	if e.deps.Events != nil {
		history, err := e.deps.Events.Read(ctx, runID, afterSeq, 0)
		if err == nil {
			for _, ev := range history {
				if ev.Seq <= lastSeq {
					continue
				}
				select {
				case out <- ev:
					lastSeq = ev.Seq
				case <-ctx.Done():
					return
				case <-sub.cancel:
					return
				}
			}
		}
	}

	// Terminal check: if the run already finished, the durable history is the
	// complete stream and waiting for live events would block forever.
	if e.isTerminalRun(ctx, runID) {
		return
	}

	for {
		select {
		case ev, ok := <-sub.ch:
			if !ok {
				return
			}
			// Skip what history already delivered. A merged ephemeral frame has
			// Seq 0 and is always delivered, since it has no durable position.
			if ev.Seq != 0 && ev.Seq <= lastSeq {
				continue
			}
			if ev.Seq != 0 {
				lastSeq = ev.Seq
			}
			select {
			case out <- ev:
			case <-ctx.Done():
				return
			}
			if ev.State.Terminal() {
				return
			}
		case <-ctx.Done():
			return
		case <-sub.cancel:
			return
		}
	}
}

// Stats reports engine-level counters.
type Stats struct {
	Submitted uint64 `json:"submitted"`
	Completed uint64 `json:"completed"`
	Failed    uint64 `json:"failed"`
	Cancelled uint64 `json:"cancelled"`
	Recovered uint64 `json:"recovered"`
	Active    int    `json:"active"`
	Sessions  int    `json:"sessions"`
	Runs      int    `json:"runs"`

	Scheduler types.SchedulerStats `json:"scheduler"`
	Admission AdmissionStats       `json:"admission"`
	Bus       BusStats             `json:"bus"`
}

// Stats snapshots the engine.
func (e *Engine) Stats() Stats {
	e.mu.Lock()
	st := Stats{
		Submitted: e.submitted,
		Completed: e.completed,
		Failed:    e.failed,
		Cancelled: e.cancelled,
		Recovered: e.recovered,
		Sessions:  len(e.sessions),
		Runs:      len(e.runs),
	}
	for _, rec := range e.runs {
		if !rec.terminal() {
			st.Active++
		}
	}
	e.mu.Unlock()
	st.Scheduler = e.sched.Stats()
	st.Admission = e.admission.Stats()
	st.Bus = e.bus.Stats()
	return st
}

// Recover inspects runs left in flight by a dead process and acts on each plan.
//
// It is the entry point task 01's Supervisor calls after a restart. Recovery is
// idempotent: running it twice produces the same outcome, because each plan is
// derived from the durable log rather than from in-memory state.
func (e *Engine) Recover(ctx context.Context) ([]RecoveryPlan, error) {
	plans, err := e.recovery.Scan(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]RecoveryPlan, 0, len(plans))
	for _, plan := range plans {
		if err := e.applyRecoveryPlan(ctx, plan); err != nil {
			plan.Notes = append(plan.Notes, "apply failed: "+types.RedactString(err.Error()))
		}
		out = append(out, plan)
	}
	return out, nil
}

// RecoverForIPC is Recover in the IPC-facing shape declared by
// types.Recoverer, so task 01's Supervisor can call it without importing this
// package's plan type.
func (e *Engine) RecoverForIPC(ctx context.Context) ([]types.RecoveryPlan, error) {
	plans, err := e.Recover(ctx)
	if err != nil {
		return nil, err
	}
	return WirePlans(plans), nil
}

// applyRecoveryPlan acts on one recovery plan.
func (e *Engine) applyRecoveryPlan(ctx context.Context, plan RecoveryPlan) error {
	switch plan.Decision {
	case Skip:
		return nil

	case MarkFailed:
		// Record the failure so the run stops being "in flight" on the next
		// pass, and so the UI can show why.
		ev := types.Event{
			RunID: plan.RunID, SessionID: plan.SessionID,
			Type: types.EventRunFailed, State: types.StateFailed,
			Timestamp: time.Now(),
			Message:   strings.Join(plan.Notes, "; "),
			Err: types.NewError(types.CodeNotResumable,
				"run could not be resumed after a crash: %s", strings.Join(plan.Notes, "; ")),
		}
		e.coalescer.Publish(ev)
		return e.appendEventBestEffort(ctx, ev)

	case ResumeAfterConfirm:
		// Park the run: the user must decide whether the uncertain
		// non-idempotent calls may be replayed. This is the branch that makes
		// "a non-idempotent tool call is never repeated automatically after a
		// crash" true.
		t := plan.PlanToWaiting()
		rec, err := e.rehydrateRun(ctx, plan)
		if err != nil {
			return err
		}
		if rec == nil {
			return nil
		}
		// The rehydrated machine starts at the folded ResumeState. A run that
		// was ALREADY parked in waiting_user when the process died (a plan
		// proposal or a tool confirmation) rehydrates at waiting_user, and the
		// table has no waiting_user → waiting_user edge: skip the transition in
		// that case and only re-record the park, so the restart does not report
		// a bogus "apply failed" for a run it recovered just fine.
		if rec.machine.State() != types.StateWaitingUser {
			if err := rec.machine.TransitionTo(ctx, types.StateWaitingUser, t); err != nil {
				return err
			}
		}
		return rec.actor.ParkRun(ctx, plan.RunID, t.WaitingReason, plan.UncertainToolCalls)

	case ResumeAuto:
		rec, err := e.rehydrateRun(ctx, plan)
		if err != nil {
			return err
		}
		if rec == nil {
			return nil
		}
		e.mu.Lock()
		e.recoverAttempts[plan.RunID]++
		e.recovered++
		e.mu.Unlock()

		// Park in waiting_user rather than executing immediately. Automatic
		// execution after a restart would be a surprising amount of initiative
		// for a run whose side effects the user has not reviewed; the plan's
		// replayable calls are already classified as safe, so the user's Resume
		// is a one-click confirmation of an assessed-safe continuation.
		//
		// No uncertain calls are reported here: every incomplete call in this
		// branch was classified as safe to replay, so there is nothing for the
		// user to adjudicate. Listing them as uncertain would misrepresent the
		// risk and train the user to click through a warning that does not apply.
		//
		// Already-parked runs rehydrate at waiting_user; see the
		// ResumeAfterConfirm note on why the transition is then skipped.
		if rec.machine.State() != types.StateWaitingUser {
			if err := rec.machine.TransitionTo(ctx, types.StateWaitingUser, agent.Transition{
				Reason:        "engine restarted; run is ready to resume from the last safe checkpoint",
				WaitingReason: recoveryParkReason(plan),
			}); err != nil {
				return err
			}
		}
		return rec.actor.ParkRun(ctx, plan.RunID, recoveryParkReason(plan), nil)

	default:
		return types.NewError(types.CodeInternal, "unknown recovery decision %q", string(plan.Decision))
	}
}

// recoveryParkReason renders the message shown for a run parked by recovery.
func recoveryParkReason(plan RecoveryPlan) string {
	if len(plan.ReplayableToolCalls) > 0 {
		return "引擎已重启：run 可从最后一个安全 checkpoint 继续，" +
			itoa(len(plan.ReplayableToolCalls)) + " 个未完成的幂等工具调用将被重放。"
	}
	return "引擎已重启：run 可从最后一个安全 checkpoint 继续。"
}

// rehydrateRun rebuilds an in-memory run record from a recovery plan so the run
// can be resumed. It returns nil when the run's request cannot be reconstructed.
func (e *Engine) rehydrateRun(ctx context.Context, plan RecoveryPlan) (*runRecord, error) {
	e.mu.Lock()
	if rec, ok := e.runs[plan.RunID]; ok {
		e.mu.Unlock()
		// A live record for a run that recovery is also working on means a
		// duplicate pass; the in-memory record wins.
		return rec, nil
	}
	e.mu.Unlock()

	sessionID := plan.SessionID
	if sessionID == "" {
		sessionID = types.NewID(types.PrefixSession)
	}
	actor, err := e.sessionActor(sessionID)
	if err != nil {
		return nil, err
	}

	// The prompt and model are not in the event log by design (the log stores
	// boundaries, not payloads), so a run recovered from a previous process
	// cannot be re-executed blindly. Recording it as a synthetic prompt keeps
	// the state machine coherent and surfaces the gap to the user rather than
	// inventing a task.
	req := types.SubmitRequest{
		SessionID: sessionID,
		Prompt:    "(recovered run: original prompt unavailable in this process)",
		Priority:  types.PriorityNormal,
		Effort:    types.EffortHigh,
	}.Normalized()

	run := types.Run{
		ID: plan.RunID, SessionID: sessionID,
		State: plan.ResumeState, Priority: req.Priority,
		Prompt:        req.Prompt,
		Round:         plan.Round,
		LastSeq:       plan.LastSeq,
		CheckpointSeq: plan.CheckpointSeq,
		CreatedAt:     time.Now(), UpdatedAt: time.Now(),
	}
	if err := actor.RegisterRun(ctx, run); err != nil {
		return nil, err
	}

	conv := agent.NewConversation("", req.Prompt, nil)
	machine := agent.NewMachineAt(plan.RunID, sessionID, plan.ResumeState, plan.Round,
		plan.LastSeq, &machineSink{engine: e, actor: actor, sessionID: sessionID})

	rec := &runRecord{
		run: run, request: req, conv: conv, machine: machine,
		actor: actor, cancel: func() {}, done: make(chan struct{}),
	}
	e.mu.Lock()
	e.runs[plan.RunID] = rec
	e.mu.Unlock()
	return rec, nil
}

// Close shuts the engine down: stop admitting, cancel runs, drain the
// scheduler, stop the actors. It is idempotent.
func (e *Engine) Close() {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	e.closed = true
	recs := make([]*runRecord, 0, len(e.runs))
	for _, rec := range e.runs {
		recs = append(recs, rec)
	}
	e.mu.Unlock()

	e.admission.Close()
	for _, rec := range recs {
		rec.cancel()
	}
	e.sched.Close()

	// Drain run goroutines before stopping the actors, so a run that is
	// mid-transition still has a live actor to apply to.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		e.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}

	e.mu.Lock()
	actors := make([]*SessionActor, 0, len(e.sessions))
	for _, a := range e.sessions {
		actors = append(actors, a)
	}
	e.mu.Unlock()
	for _, a := range actors {
		a.Close()
	}
	e.coalescer.Close()
	e.bus.Close()
}

// ---------------------------------------------------------------------------
// run execution
// ---------------------------------------------------------------------------

// executeRun drives one run through the agent loop and records the outcome.
func (e *Engine) executeRun(ctx context.Context, rec *runRecord) {
	if rec.ticket != nil {
		defer rec.ticket.Release()
	}

	// The run's wall-clock budget. A run admitted normally carries a ticket
	// whose deadline is that budget; a rehydrated run (recovered from the log
	// after a restart, or resumed by the user) has no ticket, so the budget is
	// derived from the run's own start time instead.
	//
	// Getting this wrong is not cosmetic: a nil ticket's Deadline is the zero
	// time, so an already-expired context would make the run exit instantly with
	// no answer recorded.
	deadline := time.Now().Add(e.cfg.Scheduler.Limits.MaxRunDuration)
	if rec.ticket != nil {
		deadline = rec.ticket.Deadline()
	}
	if rec.startedAt.IsZero() {
		rec.startedAt = time.Now()
	}
	runCtx, cancelCtx := context.WithDeadline(ctx, deadline)
	defer cancelCtx()

	// Capture the run's collaborators under the engine lock and use only these
	// local references for the rest of the run. finishRun clears the record's
	// fields when the run terminates, so reading them mid-run would race with a
	// concurrent Cancel finishing the same run.
	e.mu.Lock()
	conv := rec.conv
	machine := rec.machine
	runID := rec.runID()
	sessionID := rec.sessionID()
	rec.startedAt = time.Now()
	e.mu.Unlock()

	if machine == nil || conv == nil {
		// Another goroutine already finished this run.
		return
	}

	rec.setState(types.StateThinking)
	if err := machine.Transition(runCtx, types.StateThinking, "starting execution"); err != nil {
		e.finishRun(runCtx, rec, types.StateFailed, "", err)
		return
	}

	// 任务3：请求带了用户手选的专家时，直连专家两阶段编排
	// （internal/expert 的 Activate），跳过「等主模型自己决定要不要调用
	// agent_expert 工具」这一步。
	//
	// 这是一个平行分支：没有 ExpertID 的请求根本不进这里，下面原有的 Agent
	// Loop 路径一字未改，行为与改动前完全一致。
	if rec.request.ExpertID != "" {
		e.executeExpertRun(runCtx, rec)
		return
	}

	// Task 4: pick up the user's answer to a proposed plan, if one is waiting.
	// The decision is consumed (cleared) here so that a later resume of the same
	// run cannot silently re-apply it: an approval is spent exactly once.
	e.mu.Lock()
	decision := rec.planDecision
	pendingPlan := rec.pendingPlan
	revision := rec.planRevision
	rec.planDecision = agent.PlanDecisionNone
	rec.pendingPlan = ""
	e.mu.Unlock()

	loop, err := agent.NewLoop(agent.LoopConfig{
		Config:     e.cfg.Agent,
		Provider:   e.deps.Provider,
		Dispatcher: &schedulerDispatcher{engine: e, rec: rec},
		Sink:       &engineSink{engine: e, runID: runID, sessionID: sessionID},
		Planner:    e.deps.Planner,
		Reviewer:   e.deps.Reviewer,
		Confirmer:  e.deps.Confirmer,
		Compactor:  agent.NewCompactor(e.cfg.Agent, e.deps.Context),
		Guard:      e.guard,
	})
	if err != nil {
		e.finishRun(runCtx, rec, types.StateFailed, "", err)
		return
	}

	// The engine is the only legitimate source of plan decisions (ConfirmPlan /
	// RejectPlan require planPending), so a pending decision implies this run
	// opted into plan mode. That matters for the restart path: a run rehydrated
	// from a recovery plan carries a synthetic request without PlanMode, and
	// without this flag the loop would silently drop the user's just-approved
	// plan and re-execute the placeholder prompt.
	loopReq := rec.request
	if decision != agent.PlanDecisionNone {
		loopReq.PlanMode = true
	}

	res := loop.Run(runCtx, agent.LoopInput{
		RunID:        runID,
		SessionID:    sessionID,
		Request:      loopReq,
		Conversation: conv,
		Machine:      machine,
		PlanDecision: decision,
		PendingPlan:  pendingPlan,
		PlanRevision: revision,
	})

	// A run that parked on a plan decision leaves the proposal on the record so
	// the next invocation (the user's answer) knows what it is answering.
	if res.State == types.StateWaitingUser && res.Plan != "" {
		e.mu.Lock()
		rec.pendingPlan = res.Plan
		rec.planRevision = res.PlanRevision
		rec.planPending = true
		e.mu.Unlock()
	}

	// A deadline breach is reported as its own code, so the UI can distinguish
	// "too slow" from "something broke".
	err = res.Err
	if runCtx.Err() != nil && errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		err = types.NewError(types.CodeRunDurationExceeded,
			"run exceeded the %s duration budget", e.cfg.Scheduler.Limits.MaxRunDuration)
	}
	e.finishRun(runCtx, rec, res.State, res.Answer, err)
}

// ConfirmPlan records the user's decision on a run's proposed plan and, when the
// plan is approved, resumes the run so execution starts.
//
// It is the engine half of the task-4 confirmation RPC. Two properties matter:
//
//   - A run with no plan awaiting confirmation is rejected rather than resumed.
//     waiting_user is also how tool permission prompts and crash-recovery
//     adjudications park a run, and treating a confirmation aimed at a plan as
//     approval to continue past those would be a privilege escalation.
//   - Rejection does not start execution. It clears the pending flag and resumes
//     the run so the loop proposes a fresh plan, which then parks it again.
func (e *Engine) ConfirmPlan(ctx context.Context, runID string, approved bool) error {
	rec, archived, err := e.liveRun(ctx, runID)
	if err != nil {
		return err
	}
	if rec == nil {
		return types.NewError(types.CodeTerminalState,
			"run %s is %s and cannot be confirmed", runID, string(archived.State))
	}

	e.mu.Lock()
	pending := rec.planPending
	pendingPlan := rec.pendingPlan
	e.mu.Unlock()

	if !pending {
		// Nothing is pending in memory. That does not prove there is nothing to
		// confirm: the plan and the park are both durable, so an Engine restart
		// (or a UI reconnect to a run this process no longer holds a record for)
		// loses only the in-memory flag while the proposal is still on the log.
		// Rebuilding from the log here is what keeps a plan waiting across a
		// restart from becoming unconfirmable.
		plan, ok := e.pendingPlanFromLog(ctx, runID)
		if !ok {
			return types.NewError(types.CodeNotResumable,
				"run %s has no plan awaiting confirmation", runID)
		}
		e.mu.Lock()
		rec.planPending = true
		rec.pendingPlan = plan
		e.mu.Unlock()
		pendingPlan = plan
	}

	if approved {
		e.mu.Lock()
		rec.planDecision = agent.PlanDecisionApproved
		// The pending flag is cleared before the resume so a duplicate
		// confirmation arriving concurrently fails the guard above instead of
		// queueing a second decision for the same proposal.
		rec.planPending = false
		e.mu.Unlock()

		e.recordPlanDecision(ctx, rec, types.EventPlanConfirmed,
			"user confirmed the plan", pendingPlan)
	} else {
		e.mu.Lock()
		rec.planDecision = agent.PlanDecisionRejected
		rec.planPending = false
		e.mu.Unlock()

		e.recordPlanDecision(ctx, rec, types.EventPlanRejected,
			"user rejected the plan and asked for a new one", pendingPlan)
	}

	// Reuse the existing park/resume machinery rather than starting a second
	// execution path: it already owns the single-writer claim (I1), the
	// cancellation bookkeeping and the thinking transition a parked run needs.
	if err := e.Resume(ctx, runID); err != nil {
		// The resume failed, so the decision will never be consumed. Put the
		// park back so the user can retry instead of being left with a run that
		// looks parked but can no longer be confirmed.
		e.mu.Lock()
		rec.planDecision = agent.PlanDecisionNone
		rec.planPending = true
		e.mu.Unlock()
		return err
	}
	return nil
}

// recordPlanDecision durably records the user's answer to a plan.
//
// It is best-effort by design: the decision has already been taken and the run
// is about to be resumed, so a failure to log it must not strand the run. The
// event exists for the audit trail and for the UI to close the card.
func (e *Engine) recordPlanDecision(ctx context.Context, rec *runRecord, t types.EventType, message, plan string) {
	ev := types.Event{
		RunID: rec.runID(), SessionID: rec.sessionID(),
		Type: t, Timestamp: time.Now(), Message: message,
		Data: map[string]any{"planMode": true, "plan": plan},
	}
	if err := e.appendEventBestEffort(ctx, ev); err != nil {
		return
	}
	e.coalescer.Publish(ev)
}

// pendingPlanFromLog reconstructs the plan a parked run is waiting on.
//
// It returns false unless the run's newest plan event is a proposal: once a
// decision has been recorded, the proposal is no longer awaiting an answer, so
// a stale confirmation cannot re-apply an already-spent decision.
func (e *Engine) pendingPlanFromLog(ctx context.Context, runID string) (string, bool) {
	if e.deps.Events == nil {
		return "", false
	}
	events, err := e.deps.Events.Read(ctx, runID, 0, 0)
	if err != nil {
		return "", false
	}
	plan := ""
	awaiting := false
	for _, ev := range events {
		switch ev.Type {
		case types.EventPlanProposed:
			if v, ok := ev.Data["plan"].(string); ok && v != "" {
				plan = v
				awaiting = true
			}
		case types.EventPlanConfirmed, types.EventPlanRejected:
			awaiting = false
		}
	}
	if !awaiting {
		return "", false
	}
	return plan, true
}

// finishRun records a terminal outcome and releases the run's resources.
//
// It must be safe to call when a previous finish already ran: with the ownership
// claim in place only one goroutine drives a run, but a run can still be
// finished by an explicit Cancel racing the loop's own completion. The machine
// and conversation are therefore read once, under the engine lock, and every
// use goes through those local references rather than the record's fields,
// which another finisher may clear.
func (e *Engine) finishRun(ctx context.Context, rec *runRecord, state types.RunState, answer string, err error) {
	// Use a detached context: the run's context may already be cancelled, but
	// the outcome must still reach the durable log.
	dctx := context.WithoutCancel(ctx)

	var coded *types.Error
	if err != nil {
		code := types.CodeOf(err)
		if code == "" {
			code = types.CodeInternal
		}
		coded = types.NewError(code, "%s", types.RedactString(err.Error()))
	}

	if state == "" || !state.Valid() {
		state = types.StateFailed
	}

	// Snapshot the per-run collaborators under the engine lock. After this
	// point the record's fields may be cleared by a concurrent finisher, so only
	// the local references are used.
	e.mu.Lock()
	machine := rec.machine
	actor := rec.actor
	runID := rec.runID()
	e.mu.Unlock()

	// waiting_user is not terminal, and the machine has already registered it;
	// only record a close for genuinely terminal outcomes.
	if machine != nil && state.Terminal() && !machine.Terminal() {
		_ = machine.Transition(dctx, state, "run finished")
	}
	if actor != nil && (state.Terminal() || state == types.StateWaitingUser) {
		_ = actor.CloseRun(dctx, runID, state, answer, coded)
	}

	// Record the outcome on the engine's own record. setOutcome takes the
	// record's lock, so this is safe against a concurrent reader even though the
	// caller does not hold e.mu.
	rec.setOutcome(state, answer, coded)

	e.mu.Lock()
	if state.Terminal() {
		// Release the conversation only for a terminal run: a waiting_user run
		// is parked, and Resume needs the conversation to continue from the
		// same round rather than starting the task over. Keeping every finished
		// run's history would instead grow without bound over a long-lived
		// process, which is the class of leak the architecture forbids.
		rec.releaseResources()
	}
	switch {
	case state == types.StateCompleted:
		e.completed++
	case state == types.StateCancelled:
		e.cancelled++
	case state == types.StateFailed:
		e.failed++
	}
	if state.Terminal() {
		// Drop the in-memory record; GetRun reconstructs it from the durable
		// log, which keeps the log the single authority for a finished run.
		delete(e.runs, runID)
	}
	e.mu.Unlock()

	e.sched.Budget().ForgetRun(runID)
}

// runTask is the scheduler's Runner: it dispatches by task kind.
func (e *Engine) runTask(ctx context.Context, task types.Task) error {
	switch task.Kind {
	case types.TaskKindRun:
		// Run tasks are executed by their own goroutine, which the scheduler
		// only tracks for capacity accounting. Nothing to do here.
		return nil
	case types.TaskKindToolCall:
		return e.runToolTask(ctx, task)
	default:
		return types.NewError(types.CodeInvalidArgument, "unknown task kind %q", string(task.Kind))
	}
}

// toolExec runs one tool call and retains its result.
//
// The scheduler's Runner contract is `Run(ctx, task) error`, so the result
// cannot travel back through the return value alone. The executor therefore
// records the result in a slot that the submitting goroutine reads after
// SubmitWait returns. The slot is per-call and keyed by the tool-call ID, so
// concurrent calls cannot cross their results.
type toolExec struct {
	run func(ctx context.Context) (types.ToolResult, error)

	mu     sync.Mutex
	result types.ToolResult
	err    error
	// ran records that run executed, so a caller can tell "no result" from
	// "returned an empty result".
	ran bool
}

func (t *toolExec) execute(ctx context.Context) error {
	res, err := t.run(ctx)
	t.mu.Lock()
	t.result, t.err, t.ran = res, err, true
	t.mu.Unlock()
	return err
}

func (t *toolExec) outcome() (types.ToolResult, error, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.result, t.err, t.ran
}

// runToolTask executes one tool call that the scheduler has admitted.
func (e *Engine) runToolTask(ctx context.Context, task types.Task) error {
	e.mu.Lock()
	exec, ok := e.toolExecs[task.ID]
	e.mu.Unlock()
	if !ok {
		return types.NewError(types.CodeNotFound, "no executor registered for tool call %s", task.ID)
	}
	return exec.execute(ctx)
}

// dispatchToolCall runs one tool call through the full three-layer chain.
//
// This is the only path a tool call takes, which is what makes the limits
// meaningful: there is no shortcut around admission, the fair queue or the
// resource pool.
func (e *Engine) dispatchToolCall(ctx context.Context, rec *runRecord, call types.ToolCall) (types.ToolResult, error) {
	if call.ID == "" {
		return types.ToolResult{}, types.NewError(types.CodeInvalidArgument,
			"tool call has no id; the provider response could not be paired to a result")
	}
	timeout := e.cfg.Agent.ToolExecTimeout

	task := types.Task{
		ID:        call.ID,
		Kind:      types.TaskKindToolCall,
		RunID:     rec.runID(),
		SessionID: rec.sessionID(),
		Priority:  rec.priority(),
		ToolName:  call.Name,
		Cost:      1,
	}

	exec := &toolExec{run: func(execCtx context.Context) (types.ToolResult, error) {
		// The "started" marker must be durable *before* the tool runs. It is the
		// only evidence, after a crash, that this call may already have had a
		// side effect; without it recovery would see a requested-but-never-
		// started call and classify a genuinely executed non-idempotent call as
		// safe to replay.
		if err := e.markToolStarted(execCtx, rec, call); err != nil {
			return types.ToolResult{
				ToolCallID: call.ID, ToolName: call.Name, Success: false,
				Error: "could not record the tool call as started: " + types.RedactString(err.Error()),
			}, err
		}
		execCtx, cancel := context.WithTimeout(execCtx, timeout)
		defer cancel()
		return e.executeTool(execCtx, rec, call)
	}}

	// Register before submitting: the scheduler may dispatch the task on
	// another goroutine as soon as Submit returns.
	e.mu.Lock()
	if _, dup := e.toolExecs[call.ID]; dup {
		e.mu.Unlock()
		return types.ToolResult{}, types.NewError(types.CodeAlreadyExists,
			"tool call %s is already in flight", call.ID)
	}
	e.toolExecs[call.ID] = exec
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		delete(e.toolExecs, call.ID)
		e.mu.Unlock()
	}()

	// SubmitWait drives the task through the scheduler and blocks for the
	// result, so the three layers are traversed exactly once per call.
	submitErr := e.sched.SubmitWait(ctx, task)
	res, execErr, ran := exec.outcome()

	switch {
	case submitErr != nil:
		// A gate rejected the call, or the wait was abandoned. Either way the
		// tool never produced a result, and the caller must treat it as a
		// dispatch failure rather than a successful empty result.
		return types.ToolResult{
			ToolCallID: call.ID, ToolName: call.Name, Success: false,
			Error: types.RedactString(submitErr.Error()),
		}, submitErr
	case !ran:
		return types.ToolResult{}, types.NewError(types.CodeInternal,
			"tool call %s was admitted but never executed", call.ID)
	default:
		return res, execErr
	}
}

// markToolStarted durably records that a tool call is about to run.
//
// This is the event that makes crash recovery safe. Recovery distinguishes
// three cases for an incomplete call: it has a result (complete), it never
// started (cannot have taken effect, so replay is safe), or it started but has
// no result (outcome unknown, so a non-idempotent call must not be replayed
// without asking). Without this write the third case is indistinguishable from
// the second, and recovery would replay a call that may already have had a side
// effect.
//
// The write is deliberately synchronous and happens before the tool executes:
// an asynchronous or post-hoc marker would be lost in exactly the crash it
// exists to describe.
func (e *Engine) markToolStarted(ctx context.Context, rec *runRecord, call types.ToolCall) error {
	ev := types.Event{
		RunID:      rec.runID(),
		SessionID:  rec.sessionID(),
		Type:       types.EventToolStarted,
		State:      types.StateExecuting,
		Round:      rec.currentRound(),
		ToolCallID: call.ID,
		ToolName:   call.Name,
		Timestamp:  time.Now(),
		// The arguments are summarised rather than stored: the log records
		// logical boundaries, and a tool argument may contain a credential.
		Data: map[string]any{"argumentKeys": len(call.Arguments)},
	}
	seq, err := e.deps.Events.Append(ctx, rec.runID(), ev)
	if err != nil {
		return types.WrapError(types.CodeOf(err), err, "record tool call %s as started", call.ID)
	}
	ev.Seq = seq
	e.mu.Lock()
	rec.setLastSeq(seq)
	e.mu.Unlock()
	e.enqueueOutbox(ctx, ev)
	e.coalescer.Publish(ev)
	return nil
}

// executeTool invokes the tool runtime for one call.
//
// It runs inside the executor closure registered with the scheduler, so by the
// time it is called the call has already passed admission, the fair queue and
// its resource lease.
func (e *Engine) executeTool(ctx context.Context, rec *runRecord, call types.ToolCall) (types.ToolResult, error) {
	return e.deps.Tools.Execute(ctx, ports.ToolRequest{
		RunID:      rec.runID(),
		SessionID:  rec.sessionID(),
		ToolCallID: call.ID,
		ToolName:   call.Name,
		Arguments:  call.Arguments,
		Timeout:    e.cfg.Agent.ToolExecTimeout,
		Resource:   scheduler.DefaultToolResource(call.Name),
	})
}

// ---------------------------------------------------------------------------
// sinks and helpers
// ---------------------------------------------------------------------------

// machineSink persists and publishes every state transition. It is the
// implementation of the first two obligations the architecture attaches to a
// transition (append an event, update materialized state); the UI push is the
// actor's callback.
type machineSink struct {
	engine    *Engine
	actor     *SessionActor
	sessionID string
}

// OnTransition implements agent.TransitionSink.
func (s *machineSink) OnTransition(ctx context.Context, t agent.Transition) error {
	return s.actor.Transition(ctx, t)
}

// engineSink maps agent loop events onto engine events: durable ones are
// appended to the log, and all of them are published (through the coalescer for
// mergeable types).
type engineSink struct {
	engine    *Engine
	runID     string
	sessionID string
}

// Emit implements agent.LoopSink.
func (s *engineSink) Emit(ctx context.Context, le agent.LoopEvent) error {
	ev := types.Event{
		RunID:      s.runID,
		SessionID:  s.sessionID,
		Type:       le.Type,
		State:      le.State,
		PrevState:  le.PrevState,
		Round:      le.Round,
		ToolCallID: le.ToolCallID,
		ToolName:   le.ToolName,
		Data:       le.Data,
		Message:    le.Message,
		Timestamp:  le.At,
	}
	if ev.Timestamp.IsZero() {
		ev.Timestamp = time.Now()
	}
	if le.Err != nil {
		code := types.CodeOf(le.Err)
		if code == "" {
			code = types.CodeInternal
		}
		ev.Err = types.NewError(code, "%s", types.RedactString(le.Err.Error()))
	}

	if ev.Type.Durable() {
		seq, err := s.engine.deps.Events.Append(ctx, s.runID, ev)
		if err != nil {
			return err
		}
		ev.Seq = seq
		s.engine.mu.Lock()
		if rec, ok := s.engine.runs[s.runID]; ok {
			rec.setLastSeq(seq)
		}
		s.engine.mu.Unlock()
		// Durable events also go to the outbox so an external observer that was
		// disconnected still learns about them.
		s.engine.enqueueOutbox(ctx, ev)
		s.engine.sched.Budget().RecordOutput(s.runID, int64(ev.ByteSize()))
	}
	// Everything is published, including ephemeral deltas, which the coalescer
	// folds before they reach a subscriber.
	s.engine.coalescer.Publish(ev)
	return nil
}

// schedulerDispatcher adapts the engine's tool dispatch to the agent loop's
// ToolDispatcher port. It is what routes every tool call through the three
// layers instead of letting the loop run tools directly.
type schedulerDispatcher struct {
	engine *Engine
	rec    *runRecord
}

// Dispatch implements agent.ToolDispatcher.
func (d *schedulerDispatcher) Dispatch(ctx context.Context, call types.ToolCall) agent.ToolOutcome {
	res, err := d.engine.dispatchToolCall(ctx, d.rec, call)
	return agent.ToolOutcome{Call: call, Result: res, Err: err}
}

// onSchedulerNotification mirrors scheduler lifecycle decisions into the
// durable log for run-level tasks, so recovery can see when a run started.
func (e *Engine) onSchedulerNotification(n scheduler.Notification) {
	if n.RunID == "" {
		return
	}
	switch n.Kind {
	case scheduler.NotifyRejected:
		ev := types.Event{
			RunID: n.RunID, SessionID: n.SessionID,
			Type: types.EventError, Timestamp: time.Now(),
			Message: "scheduler rejected task",
			Data:    map[string]any{"taskId": n.TaskID, "priority": string(n.Priority)},
		}
		if n.Err != nil {
			ev.Err = n.Err
			ev.Message = n.Err.Msg
		}
		e.coalescer.Publish(ev)
	}
}

// sessionActor returns the actor for a session, creating it on first use.
func (e *Engine) sessionActor(sessionID string) (*SessionActor, error) {
	e.mu.Lock()
	if a, ok := e.sessions[sessionID]; ok {
		e.mu.Unlock()
		return a, nil
	}
	if e.closed {
		e.mu.Unlock()
		return nil, types.NewError(types.CodeEngineClosed, "engine is closed")
	}
	deps := actorDeps{
		appendEvent: func(ctx context.Context, ev types.Event) (uint64, error) {
			seq, err := e.deps.Events.Append(ctx, ev.RunID, ev)
			if err != nil {
				return 0, err
			}
			ev.Seq = seq
			e.enqueueOutbox(ctx, ev)
			return seq, nil
		},
		onTransition: func(t agent.Transition, seq uint64) {
			e.coalescer.Publish(types.Event{
				RunID: t.RunID, SessionID: t.SessionID,
				Type: eventTypeForTransition(t.To), State: t.To, PrevState: t.From,
				Round: t.Round, Message: t.Reason, Seq: seq, Timestamp: t.At,
			})
		},
		onTerminal: func(sessionID, runID string, state types.RunState) {
			if state.Terminal() {
				e.sched.CancelRun(runID)
			}
		},
	}
	a := NewSessionActor(sessionID, deps, e.guard)
	e.sessions[sessionID] = a
	e.mu.Unlock()
	return a, nil
}

// enqueueOutbox mirrors an event to the outbox, when one is configured.
func (e *Engine) enqueueOutbox(ctx context.Context, ev types.Event) {
	if e.deps.Outbox == nil {
		return
	}
	// Best-effort: the durable log is the authority, and the outbox is a
	// convenience for external observers. A failure here must not fail the run.
	_ = e.deps.Outbox.Enqueue(context.WithoutCancel(ctx), ev)
}

// appendEventBestEffort records an event without failing the caller. It is used
// on paths that run during unwinding or shutdown, where returning an error
// would have nowhere useful to go.
func (e *Engine) appendEventBestEffort(ctx context.Context, ev types.Event) error {
	if e.deps.Events == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	_, err := e.deps.Events.Append(ctx, ev.RunID, ev)
	return err
}

// recordResumeApproval records that the user approved replaying the uncertain
// calls, so the decision is auditable rather than implicit in a resume click.
func (e *Engine) recordResumeApproval(ctx context.Context, rec *runRecord, uncertain []string) error {
	ev := types.Event{
		RunID: rec.runID(), SessionID: rec.sessionID(),
		Type: types.EventUserInputReceived, Timestamp: time.Now(),
		Message: "user approved replaying uncertain tool calls",
		Data:    map[string]any{"approvedToolCalls": uncertain},
	}
	if err := e.appendEventBestEffort(ctx, ev); err != nil {
		return err
	}
	e.coalescer.Publish(ev)
	return nil
}

// recoveryAttemptsFor reports how many recovery passes a run has had.
func (e *Engine) recoveryAttemptsFor(runID string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.recoverAttempts[runID]
}

// releaseRun cleans up a run that failed before it started.
func (e *Engine) releaseRun(runID string, ticket *AdmissionTicket) {
	if ticket != nil {
		ticket.Release()
	}
	e.mu.Lock()
	delete(e.runs, runID)
	e.mu.Unlock()
}

// isTerminalRun reports whether a run has reached a terminal state, consulting
// the in-memory record first and the durable log second.
func (e *Engine) isTerminalRun(ctx context.Context, runID string) bool {
	e.mu.Lock()
	rec, ok := e.runs[runID]
	e.mu.Unlock()
	if ok {
		return rec.terminal()
	}
	r, err := e.loadRunFromLog(ctx, runID)
	if err != nil {
		// If the log is unreadable, treat the run as terminal so a subscriber
		// is not left hanging on a stream that will never produce events.
		return true
	}
	return r.State.Terminal()
}

// loadRunFromLog rebuilds a run's state from its durable event log.
func (e *Engine) loadRunFromLog(ctx context.Context, runID string) (types.Run, error) {
	events, err := e.deps.Events.Read(ctx, runID, 0, 0)
	if err != nil {
		return types.Run{}, types.WrapError(types.CodeOf(err), err, "read run %s", runID)
	}
	if len(events) == 0 {
		return types.Run{}, types.NewError(types.CodeNotFound, "run %s not found", runID)
	}
	st := foldEvents(events)
	run := types.Run{
		ID: runID, SessionID: st.sessionID, State: st.state, Round: st.round,
		LastSeq: events[len(events)-1].Seq,
	}
	for _, ev := range events {
		if ev.Type == types.EventRunCreated {
			run.CreatedAt = ev.Timestamp
		}
		if ev.Type == types.EventFinalAnswer {
			run.Answer = ev.Message
		}
		if ev.Type == types.EventUserInputRequired {
			run.WaitingReason = ev.Message
			if v, ok := ev.Data["uncertainToolCalls"].([]string); ok {
				run.UncertainToolCalls = append([]string(nil), v...)
			}
		}
		if ev.Err != nil {
			run.Err = ev.Err
		}
		if ev.Type == types.EventRunCompleted || ev.Type == types.EventRunFailed ||
			ev.Type == types.EventRunCancelled {
			run.CompletedAt = ev.Timestamp
		}
	}
	return run, nil
}

// SessionIDs returns the sessions the engine knows about, sorted.
func (e *Engine) SessionIDs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, 0, len(e.sessions))
	for id := range e.sessions {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// RunIDs returns the in-memory run IDs, sorted.
func (e *Engine) RunIDs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, 0, len(e.runs))
	for id := range e.runs {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// toStr renders a panic value for a log message.
func toStr(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case error:
		return t.Error()
	default:
		return strings.TrimSpace(sprintAny(v))
	}
}

// Compile-time assertions that Engine satisfies the two published contracts.
//
// The first is the ch. 32 Engine interface, asserted against a local literal so
// this package does not depend on the alias. The second and third bind the
// concrete type to the shared declarations in internal/types that task 01 and
// task 07 program against; if a signature drifts, the build breaks here rather
// than at the integration step.
var _ interface {
	Submit(context.Context, types.SubmitRequest) (types.RunHandle, error)
	Cancel(context.Context, string) error
	Resume(context.Context, string) error
	GetRun(context.Context, string) (types.Run, error)
	Events(context.Context, string, uint64) (<-chan types.Event, error)
} = (*Engine)(nil)

var _ types.Engine = (*Engine)(nil)

var _ types.Recoverer = (*Engine)(nil)
