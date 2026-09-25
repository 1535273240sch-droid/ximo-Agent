package engine

import (
	"container/list"
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/agent"
	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// This file implements the Session Actor from architecture doc ch. 5.
//
// The rule: exactly one goroutine owns a session's mutable state. Every write —
// a state transition, a message append, a tool result — is performed by the
// actor's loop, never by a caller. Multiple goroutines submit *requests*; the
// actor applies them one at a time in arrival order.
//
// Parallelism happens strictly *outside* the actor: several tools of the same
// session execute concurrently in workers, and their results are posted back to
// the mailbox, where the actor applies them in the order they completed. This
// is what makes "no two goroutines mutate one session" checkable rather than
// aspirational — the actor's state is touched only from inside its loop.

// mailboxDepth bounds how many requests may be pending for one session. The
// architecture forbids unbounded buffers, so a caller that finds the mailbox
// full is rejected rather than queued.
const mailboxDepth = 256

// requestKind enumerates the operations the actor performs.
type requestKind int

const (
	reqApplyTransition requestKind = iota
	reqAppendMessage
	reqUpdateRun
	reqSnapshot
	reqCloseRun
	reqDrain
	reqStop
)

// actorRequest is one unit of work for the actor loop. Exactly one of the
// result fields is used, chosen by kind.
type actorRequest struct {
	kind requestKind
	// ctx is the caller's context, used only to abandon the wait. The actor
	// never cancels work based on it: a request that has been enqueued must be
	// applied, or the session's state and the durable log diverge.
	ctx context.Context

	// applyTransition
	transition *agent.Transition

	// appendMessage
	message *messageAppend

	// updateRun
	runUpdate *runUpdate

	// closeRun
	terminal terminalUpdate

	// results
	done     chan struct{}
	err      error
	snapshot *sessionSnapshot
}

// messageAppend is a conversation append performed by the actor so it is
// serialised with everything else writing the session.
type messageAppend struct {
	runID string
	msg   appendedMessage
}

// appendedMessage is a role-tagged message for the actor to append.
type appendedMessage struct {
	Role    string
	Content string
}

// runUpdate is a partial update to a run's materialized state.
type runUpdate struct {
	runID  string
	mutate func(*types.Run)
}

// terminalUpdate records a run's terminal outcome.
type terminalUpdate struct {
	runID  string
	state  types.RunState
	answer string
	err    *types.Error
	// uncertainToolCalls is set when the run parks in waiting_user because
	// recovery could not classify an interrupted non-idempotent call.
	uncertainToolCalls []string
	waitingReason      string
}

// sessionSnapshot is a consistent read of a session's state.
type sessionSnapshot struct {
	sessionID string
	runs      map[string]types.Run
	order     []string
	version   uint64
}

// SessionActor owns one session's mutable state and serialises every write to
// it. It is created by the engine and closed when the session's last run
// reaches a terminal state.
type SessionActor struct {
	sessionID string
	// mailbox carries requests to the loop. It is bounded; a full mailbox
	// rejects the caller (see submit).
	mailbox chan *actorRequest
	// done closes when the loop exits.
	done      chan struct{}
	closeOnce sync.Once
	// closed reports that shutdown has begun, so new submissions fail fast
	// rather than blocking on a dead actor.
	closed atomicBool

	// Engine collaborators. These are read-only from the actor's perspective.
	deps actorDeps

	// ---- state owned exclusively by the loop goroutine ----
	//
	// Nothing outside the loop may read or write these; the loop is the only
	// goroutine that touches them, which is what removes the need for a lock
	// entirely.
	runs     map[string]*types.Run
	order    []string
	version  uint64
	pending  *list.List
	lastSeen time.Time
}

// actorDeps is what the actor needs from the engine.
type actorDeps struct {
	// appendEvent durably records a transition or lifecycle event.
	appendEvent func(ctx context.Context, ev types.Event) (uint64, error)
	// onTransition is invoked after a transition is durably recorded, so the
	// engine can fan it out to subscribers.
	onTransition func(t agent.Transition, seq uint64)
	// onTerminal is invoked when a run reaches a terminal state, so the engine
	// can release per-run bookkeeping.
	onTerminal func(sessionID, runID string, state types.RunState)
}

// NewSessionActor starts an actor for a session. The caller must Close it, or
// the actor goroutine outlives its usefulness.
func NewSessionActor(sessionID string, deps actorDeps, guard *agent.PanicGuard) *SessionActor {
	a := &SessionActor{
		sessionID: sessionID,
		mailbox:   make(chan *actorRequest, mailboxDepth),
		done:      make(chan struct{}),
		deps:      deps,
		runs:      make(map[string]*types.Run),
		order:     make([]string, 0, 8),
		pending:   list.New(),
		lastSeen:  time.Now(),
	}
	// The actor loop is a CORE goroutine: it owns session state, and a panic
	// here would leave that state inconsistent. It therefore runs under
	// RunCore, which does not recover — it installs crash output and lets the
	// panic kill the process so the Supervisor can restart it.
	if guard != nil {
		go guard.RunCore("engine.session_actor", agent.CrashMeta{SessionID: sessionID}, a.loop)
	} else {
		go a.loop()
	}
	return a
}

// loop is the actor's single goroutine. It processes one request at a time
// until asked to stop.
func (a *SessionActor) loop() {
	defer close(a.done)
	for {
		select {
		case req := <-a.mailbox:
			if req.kind == reqStop {
				// Drain anything already queued so no caller is left waiting on
				// a request that will never be applied.
				a.drainPending()
				a.resolve(req, nil)
				return
			}
			a.handle(req)
		}
	}
}

// handle applies one request. It is the only place session state is written.
func (a *SessionActor) handle(req *actorRequest) {
	a.lastSeen = time.Now()
	switch req.kind {
	case reqApplyTransition:
		req.err = a.applyTransition(req.ctx, req.transition)
	case reqAppendMessage:
		req.err = a.applyMessage(req.ctx, req.message)
	case reqUpdateRun:
		req.err = a.applyRunUpdate(req.runUpdate)
	case reqSnapshot:
		req.snapshot = a.snapshot()
	case reqCloseRun:
		req.err = a.applyTerminal(req.ctx, req.terminal)
	case reqDrain:
		a.drainPending()
	default:
		req.err = types.NewError(types.CodeInternal, "unknown actor request kind %d", int(req.kind))
	}
	// Release the caller. The close happens here, inside the loop, so the
	// caller and the next request cannot observe a half-applied change.
	a.resolve(req, req.err)
}

// resolve closes a request's done channel exactly once.
func (a *SessionActor) resolve(req *actorRequest, err error) {
	if req == nil || req.done == nil {
		return
	}
	req.err = err
	close(req.done)
}

// drainPending fails every queued request, so no caller blocks forever on
// shutdown.
func (a *SessionActor) drainPending() {
	for e := a.pending.Front(); e != nil; e = e.Next() {
		req := e.Value.(*actorRequest)
		a.resolve(req, types.NewError(types.CodeActorClosed, "session actor stopped"))
	}
	a.pending.Init()
}

// submit enqueues a request and waits for the actor to apply it.
//
// The wait honours the caller's context, but abandoning the wait does not
// cancel the request: once enqueued it is applied, because a half-applied
// session update would desynchronise the in-memory state from the event log.
// A caller that times out learns nothing about whether its write landed, so
// the error says so explicitly.
func (a *SessionActor) submit(req *actorRequest) error {
	if a.closed.get() {
		return types.NewError(types.CodeActorClosed, "session %s actor is closed", a.sessionID)
	}
	req.done = make(chan struct{})

	select {
	case a.mailbox <- req:
	case <-a.done:
		return types.NewError(types.CodeActorClosed, "session %s actor stopped", a.sessionID)
	default:
		return types.NewError(types.CodeQueueFull,
			"session %s mailbox is full (%d)", a.sessionID, mailboxDepth)
	}

	if req.ctx != nil {
		select {
		case <-req.done:
			return req.err
		case <-req.ctx.Done():
			return types.WrapError(types.CodeDeadlineExceeded, req.ctx.Err(),
				"waiting for session %s actor (the request is still applied)", a.sessionID)
		case <-a.done:
			// The actor stopped between our enqueue and our wait. Read the
			// result if it was resolved, otherwise report closure.
			select {
			case <-req.done:
				return req.err
			default:
				return types.NewError(types.CodeActorClosed, "session %s actor stopped", a.sessionID)
			}
		}
	}
	<-req.done
	return req.err
}

// Transition applies a run-state transition. This is the actor's primary job:
// a transition must be durable before it is visible, so the actor appends the
// event first and only then updates the materialized run.
func (a *SessionActor) Transition(ctx context.Context, t agent.Transition) error {
	cp := t
	return a.submit(&actorRequest{kind: reqApplyTransition, ctx: ctx, transition: &cp})
}

// AppendMessage appends a conversation message for a run.
func (a *SessionActor) AppendMessage(ctx context.Context, runID string, role, content string) error {
	return a.submit(&actorRequest{
		kind: reqAppendMessage, ctx: ctx,
		message: &messageAppend{runID: runID, msg: appendedMessage{Role: role, Content: content}},
	})
}

// UpdateRun applies a mutation to a run's materialized state.
func (a *SessionActor) UpdateRun(ctx context.Context, runID string, mutate func(*types.Run)) error {
	return a.submit(&actorRequest{
		kind: reqUpdateRun, ctx: ctx,
		runUpdate: &runUpdate{runID: runID, mutate: mutate},
	})
}

// CloseRun records a run's terminal outcome.
func (a *SessionActor) CloseRun(ctx context.Context, runID string, state types.RunState, answer string, err *types.Error) error {
	return a.submit(&actorRequest{
		kind: reqCloseRun, ctx: ctx,
		terminal: terminalUpdate{runID: runID, state: state, answer: answer, err: err},
	})
}

// ParkRun records that a run is waiting for the user, with the reason and any
// uncertain non-idempotent tool calls recovery could not classify.
func (a *SessionActor) ParkRun(ctx context.Context, runID, reason string, uncertain []string) error {
	return a.submit(&actorRequest{
		kind: reqCloseRun, ctx: ctx,
		terminal: terminalUpdate{
			runID: runID, state: types.StateWaitingUser,
			waitingReason: reason, uncertainToolCalls: uncertain,
		},
	})
}

// RegisterRun adds a run to the session's materialized state.
func (a *SessionActor) RegisterRun(ctx context.Context, run types.Run) error {
	return a.submit(&actorRequest{
		kind: reqUpdateRun, ctx: ctx,
		runUpdate: &runUpdate{runID: run.ID, mutate: func(r *types.Run) { *r = run }},
	})
}

// Snapshot returns a consistent copy of the session's runs. Because it is
// applied by the actor loop, the returned map cannot be observed mid-update.
func (a *SessionActor) Snapshot(ctx context.Context) (map[string]types.Run, error) {
	req := &actorRequest{kind: reqSnapshot, ctx: ctx}
	if err := a.submit(req); err != nil {
		return nil, err
	}
	out := make(map[string]types.Run, len(req.snapshot.runs))
	for id, r := range req.snapshot.runs {
		out[id] = r.Clone()
	}
	return out, nil
}

// RunIDs returns the session's run IDs in registration order.
func (a *SessionActor) RunIDs() []string {
	req := &actorRequest{kind: reqSnapshot}
	if err := a.submit(req); err != nil {
		return nil
	}
	return append([]string(nil), req.snapshot.order...)
}

// Close stops the actor. It is idempotent and safe to call from any goroutine.
func (a *SessionActor) Close() {
	a.closeOnce.Do(func() {
		a.closed.set(true)
		// Deliver the stop request without blocking: the loop may already be
		// unwinding, in which case done is closed and waiting is unnecessary.
		select {
		case a.mailbox <- &actorRequest{kind: reqStop}:
		case <-a.done:
			return
		}
		select {
		case <-a.done:
		case <-time.After(5 * time.Second):
		}
	})
}

// Done returns a channel closed when the actor loop exits.
func (a *SessionActor) Done() <-chan struct{} { return a.done }

// SessionID returns the actor's session.
func (a *SessionActor) SessionID() string { return a.sessionID }

// ---------------------------------------------------------------------------
// loop-private state operations
// ---------------------------------------------------------------------------

// applyTransition durably records and then applies a transition.
//
// Order matters: the event is appended first, so a crash between the two steps
// leaves a durable record of a transition the in-memory state has not yet
// absorbed. Recovery replays that event, so the run ends up in the right state
// either way. Doing it the other way round would lose the transition entirely.
func (a *SessionActor) applyTransition(ctx context.Context, t *agent.Transition) error {
	r, ok := a.runs[t.RunID]
	if !ok {
		return types.NewError(types.CodeNotFound, "run %s is not registered in session %s", t.RunID, a.sessionID)
	}
	if !types.CanTransitionTo(r.State, t.To) {
		return types.NewError(types.CodeInvalidTransition,
			"illegal transition %q → %q for run %s", string(r.State), string(t.To), t.RunID)
	}

	ev := types.Event{
		RunID:     t.RunID,
		SessionID: a.sessionID,
		Type:      eventTypeForTransition(t.To),
		State:     t.To,
		PrevState: r.State,
		Round:     t.Round,
		Timestamp: t.At,
		Message:   t.Reason,
		Data: map[string]any{
			"from":   string(r.State),
			"to":     string(t.To),
			"reason": t.Reason,
		},
	}
	if t.WaitingReason != "" {
		ev.Type = types.EventUserInputRequired
		ev.Data["waitingReason"] = t.WaitingReason
		ev.Message = t.WaitingReason
	}
	if len(t.UncertainToolCalls) > 0 {
		ev.Data["uncertainToolCalls"] = t.UncertainToolCalls
	}

	seq, err := a.deps.appendEvent(ctx, ev)
	if err != nil {
		return err
	}

	r.State = t.To
	r.Round = t.Round
	r.LastSeq = seq
	r.Version++
	r.UpdatedAt = t.At
	if t.WaitingReason != "" {
		r.WaitingReason = t.WaitingReason
	}
	if len(t.UncertainToolCalls) > 0 {
		r.UncertainToolCalls = append([]string(nil), t.UncertainToolCalls...)
	}
	a.version++

	if a.deps.onTransition != nil {
		a.deps.onTransition(*t, seq)
	}
	return nil
}

// applyMessage appends a conversation message. The conversation itself lives in
// the run, so the actor records it as a durable event and lets the engine's
// per-run conversation holder apply it.
func (a *SessionActor) applyMessage(ctx context.Context, m *messageAppend) error {
	if m == nil {
		return nil
	}
	r, ok := a.runs[m.runID]
	if !ok {
		return types.NewError(types.CodeNotFound, "run %s is not registered", m.runID)
	}
	ev := types.Event{
		RunID: m.runID, SessionID: a.sessionID,
		Type:      types.EventProgress,
		Round:     r.Round,
		Timestamp: time.Now(),
		Data: map[string]any{
			"messageRole": m.msg.Role,
			// The content is truncated here on purpose: the durable log records
			// logical boundaries, not full payloads (doc ch. 7).
			"contentBytes": len(m.msg.Content),
		},
	}
	seq, err := a.deps.appendEvent(ctx, ev)
	if err != nil {
		return err
	}
	r.LastSeq = seq
	r.Version++
	return nil
}

// applyRunUpdate applies a mutation to a run.
func (a *SessionActor) applyRunUpdate(u *runUpdate) error {
	if u == nil {
		return nil
	}
	r, ok := a.runs[u.runID]
	if !ok {
		// Registration: the mutation creates the run.
		if u.mutate == nil {
			return types.NewError(types.CodeNotFound, "run %s is not registered", u.runID)
		}
		fresh := types.Run{ID: u.runID, SessionID: a.sessionID, State: types.StateCreated,
			CreatedAt: time.Now(), UpdatedAt: time.Now()}
		u.mutate(&fresh)
		if fresh.ID == "" {
			fresh.ID = u.runID
		}
		a.runs[fresh.ID] = &fresh
		a.order = append(a.order, fresh.ID)
		a.version++
		return nil
	}
	if u.mutate != nil {
		u.mutate(r)
	}
	r.Version++
	r.UpdatedAt = time.Now()
	a.version++
	return nil
}

// applyTerminal records a terminal (or parked) outcome.
func (a *SessionActor) applyTerminal(ctx context.Context, t terminalUpdate) error {
	r, ok := a.runs[t.runID]
	if !ok {
		return types.NewError(types.CodeNotFound, "run %s is not registered", t.runID)
	}

	// Two completion paths can race (the loop finishing and an explicit
	// Cancel), so a second terminal write is tolerated rather than failed.
	// The outcome fields are still applied, because the second writer may be
	// the one carrying the final answer.
	alreadyTerminal := r.State.Terminal() && t.state.Terminal()
	// The run may already be parked: the loop registers the waiting_user
	// transition, and a caller then records the same park with the reason. That
	// is a redundant write, not an illegal one.
	redundantPark := r.State == t.state && t.state == types.StateWaitingUser
	if !r.State.Terminal() && !redundantPark && !types.CanTransitionTo(r.State, t.state) {
		return types.NewError(types.CodeInvalidTransition,
			"cannot close run %s from %q to %q", t.runID, string(r.State), string(t.state))
	}
	if alreadyTerminal || redundantPark {
		a.applyTerminalFields(r, t)
		r.Version++
		r.UpdatedAt = time.Now()
		a.version++
		return nil
	}

	ev := types.Event{
		RunID: t.runID, SessionID: a.sessionID,
		Type:      eventTypeForTerminal(t.state),
		State:     t.state,
		PrevState: r.State,
		Round:     r.Round,
		Timestamp: time.Now(),
		Message:   t.waitingReason,
	}
	if t.err != nil {
		ev.Err = t.err
		ev.Message = t.err.Msg
	}
	if len(t.uncertainToolCalls) > 0 {
		ev.Data = map[string]any{"uncertainToolCalls": t.uncertainToolCalls}
	}
	if t.state == types.StateWaitingUser && ev.Type != types.EventUserInputRequired {
		ev.Type = types.EventUserInputRequired
	}

	seq, err := a.deps.appendEvent(ctx, ev)
	if err != nil {
		return err
	}

	r.State = t.state
	r.LastSeq = seq
	r.Version++
	r.UpdatedAt = time.Now()
	a.applyTerminalFields(r, t)
	a.version++

	if a.deps.onTransition != nil {
		a.deps.onTransition(agent.Transition{
			RunID: t.runID, SessionID: a.sessionID,
			To: t.state, Round: r.Round, Reason: t.waitingReason,
			WaitingReason: t.waitingReason, UncertainToolCalls: t.uncertainToolCalls,
		}, seq)
	}
	if a.deps.onTerminal != nil {
		a.deps.onTerminal(a.sessionID, t.runID, t.state)
	}
	return nil
}

// applyTerminalFields copies a terminal update's payload onto the run record.
// It is shared by the fresh and the already-terminal paths so both carry the
// answer and error through.
func (a *SessionActor) applyTerminalFields(r *types.Run, t terminalUpdate) {
	// Only overwrite the answer when this update carries one, so a cancellation
	// arriving after a completion does not erase the answer.
	if t.answer != "" {
		r.Answer = t.answer
	}
	if t.err != nil {
		e := *t.err
		r.Err = &e
	}
	if t.waitingReason != "" {
		r.WaitingReason = t.waitingReason
	}
	if len(t.uncertainToolCalls) > 0 {
		r.UncertainToolCalls = append([]string(nil), t.uncertainToolCalls...)
	}
	if t.state.Terminal() && r.CompletedAt.IsZero() {
		r.CompletedAt = time.Now()
	}
}

// snapshot copies the session's runs. Caller must be on the loop goroutine.
func (a *SessionActor) snapshot() *sessionSnapshot {
	snap := &sessionSnapshot{
		sessionID: a.sessionID,
		runs:      make(map[string]types.Run, len(a.runs)),
		order:     append([]string(nil), a.order...),
		version:   a.version,
	}
	for id, r := range a.runs {
		snap.runs[id] = r.Clone()
	}
	return snap
}

// eventTypeForTransition maps a target state onto its lifecycle event type.
func eventTypeForTransition(to types.RunState) types.EventType {
	switch to {
	case types.StateCreated:
		return types.EventRunCreated
	case types.StateQueued:
		return types.EventRunQueued
	case types.StateRecovering:
		return types.EventRunRecovering
	case types.StateCompleted:
		return types.EventRunCompleted
	case types.StateCancelled:
		return types.EventRunCancelled
	case types.StateFailed:
		return types.EventRunFailed
	case types.StateWaitingUser:
		return types.EventUserInputRequired
	default:
		return types.EventRunStateChanged
	}
}

// eventTypeForTerminal maps a terminal state onto its event type.
func eventTypeForTerminal(state types.RunState) types.EventType {
	return eventTypeForTransition(state)
}

// atomicBool is a tiny helper so the actor's closed flag is safe to read from
// any goroutine without pulling in sync/atomic's pointer rules.
type atomicBool struct {
	mu sync.Mutex
	v  bool
}

func (b *atomicBool) set(v bool) {
	b.mu.Lock()
	b.v = v
	b.mu.Unlock()
}

func (b *atomicBool) get() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.v
}

// String renders the actor for diagnostics.
func (a *SessionActor) String() string {
	return fmt.Sprintf("SessionActor(%s)", a.sessionID)
}
