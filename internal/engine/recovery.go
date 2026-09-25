package engine

import (
	"context"
	"sort"

	"github.com/ximo888ok-netizen/ximo-agent/internal/agent"
	"github.com/ximo888ok-netizen/ximo-agent/internal/ports"
	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// This file implements crash recovery: the procedure doc ch. 11 specifies as
//
//	load run → verify event sequence → locate last safe checkpoint →
//	identify incomplete tool calls → classify idempotent/non-idempotent →
//	resume or mark uncertain
//
// The classifier comes from task 04's IdempotencyStore; this package only
// consumes it and acts on the answer. That split matters: whether replaying a
// tool call is safe is knowledge about the tool, which belongs with the tool,
// whereas what to *do* about it is engine policy.

// RecoveryDecision is the action recovery recommends for one in-flight run.
type RecoveryDecision string

const (
	// ResumeAuto means the run can continue without asking the user: either
	// nothing was in flight, or every incomplete call is safe to replay.
	ResumeAuto RecoveryDecision = "resume_auto"
	// ResumeAfterConfirm means at least one incomplete non-idempotent call has
	// an unknown outcome, so the user must confirm before the run continues.
	ResumeAfterConfirm RecoveryDecision = "resume_after_confirm"
	// MarkFailed means the run cannot be resumed safely and should be failed.
	MarkFailed RecoveryDecision = "mark_failed"
	// Skip means the run is already terminal and needs no recovery.
	Skip RecoveryDecision = "skip"
)

// Valid reports whether d is a known decision.
func (d RecoveryDecision) Valid() bool {
	switch d {
	case ResumeAuto, ResumeAfterConfirm, MarkFailed, Skip:
		return true
	default:
		return false
	}
}

// IncompleteCall is a tool call that was requested but whose completion could
// not be confirmed from the durable log.
type IncompleteCall struct {
	ToolCallID string
	ToolName   string
	Class      ports.IdempotencyClass
	// HasResult reports whether a result event exists for the call. A call with
	// a result is complete regardless of class.
	HasResult bool
	// StartedReported reports whether a started event exists. A call that never
	// started cannot have taken effect, so it is always safe to replay, no
	// matter its class.
	StartedReported bool
}

// RecoveryPlan is the full plan for one in-flight run.
type RecoveryPlan struct {
	RunID     string
	SessionID string
	Decision  RecoveryDecision

	// ResumeState is the state the run was in when it died.
	ResumeState types.RunState
	// LastSeq is the highest durable event sequence found.
	LastSeq uint64
	// CheckpointSeq is the sequence of the last safe checkpoint, if any.
	CheckpointSeq uint64
	// Round is the round in progress at crash time.
	Round int

	// Incomplete lists the calls whose completion is unknown.
	Incomplete []IncompleteCall
	// UncertainToolCalls is the subset that must not be replayed without
	// confirmation, in a stable order.
	UncertainToolCalls []string
	// ReplayableToolCalls is the subset that may simply be re-run.
	ReplayableToolCalls []string

	// SequenceOK reports whether the event sequence had no gaps.
	SequenceOK bool
	// Notes records what recovery observed, for the event log and the report.
	Notes []string
}

// RecoveryConfig configures the recovery engine.
type RecoveryConfig struct {
	// Events is the durable event log (task 03).
	Events ports.EventStore
	// Checkpoints is the snapshot store (task 03).
	Checkpoints ports.CheckpointStore
	// Idempotency classifies tool calls (task 04).
	Idempotency ports.IdempotencyStore
	// Outbox is the delivery outbox (task 03). Optional.
	Outbox ports.OutboxStore
	// MaxRecoveryAttempts bounds automatic resumes per run, so a run that
	// crashes deterministically is parked instead of looping across restarts.
	MaxRecoveryAttempts int
	// attempts counts resumes already performed per run, supplied by the
	// engine so the count survives across recovery passes within a process.
	attempts func(runID string) int
}

// Recorder persists a recovery plan's outcome.
type Recorder interface {
	// RecordRecovery is called once per inspected run.
	RecordRecovery(ctx context.Context, plan RecoveryPlan) error
}

// Recovery inspects in-flight runs left behind by a dead process and decides
// what to do with each.
type Recovery struct {
	cfg RecoveryConfig
}

// NewRecovery builds a recovery engine.
func NewRecovery(cfg RecoveryConfig) (*Recovery, error) {
	if cfg.Events == nil {
		return nil, types.NewError(types.CodeInvalidArgument, "recovery requires an event store")
	}
	if cfg.Idempotency == nil {
		return nil, types.NewError(types.CodeInvalidArgument, "recovery requires an idempotency store")
	}
	if cfg.MaxRecoveryAttempts < 0 {
		return nil, types.NewError(types.CodeInvalidArgument,
			"MaxRecoveryAttempts must not be negative, got %d", cfg.MaxRecoveryAttempts)
	}
	return &Recovery{cfg: cfg}, nil
}

// Scan finds every run that was in flight when the previous process died and
// returns a plan for each.
//
// The scan is read-only: it inspects the log and produces plans, leaving the
// actual resumption to the caller. That keeps "decide" and "act" separately
// testable, which matters because the decision is the part with the subtle
// rules.
func (r *Recovery) Scan(ctx context.Context) ([]RecoveryPlan, error) {
	runIDs, err := r.cfg.Events.ListRuns(ctx)
	if err != nil {
		return nil, types.WrapError(types.CodeOf(err), err, "list runs for recovery")
	}
	sort.Strings(runIDs)

	plans := make([]RecoveryPlan, 0, len(runIDs))
	for _, id := range runIDs {
		plan, err := r.Inspect(ctx, id)
		if err != nil {
			// One unreadable run must not block recovery of the rest: a corrupt
			// log for run A says nothing about run B.
			plans = append(plans, RecoveryPlan{
				RunID:    id,
				Decision: MarkFailed,
				Notes:    []string{"inspection failed: " + types.RedactString(err.Error())},
			})
			continue
		}
		plans = append(plans, plan)
	}
	return plans, nil
}

// Inspect builds the recovery plan for one run.
func (r *Recovery) Inspect(ctx context.Context, runID string) (RecoveryPlan, error) {
	plan := RecoveryPlan{RunID: runID}

	events, err := r.cfg.Events.Read(ctx, runID, 0, 0)
	if err != nil {
		return plan, types.WrapError(types.CodeOf(err), err, "read events for run %s", runID)
	}
	if len(events) == 0 {
		plan.Decision = MarkFailed
		plan.Notes = append(plan.Notes, "run has no events")
		return plan, nil
	}

	// Step 1: verify the event sequence. A gap means the log is not
	// trustworthy, and resuming from an untrustworthy log could replay work
	// that already happened.
	plan.SequenceOK, plan.LastSeq = verifySequence(events)
	if !plan.SequenceOK {
		plan.Decision = MarkFailed
		plan.Notes = append(plan.Notes,
			"event sequence has a gap; refusing to resume because the log cannot be trusted")
		return plan, r.withCheckpoint(ctx, &plan)
	}

	// Step 2: fold the log into the run's last observed state.
	st := foldEvents(events)
	plan.SessionID = st.sessionID
	plan.ResumeState = st.state
	plan.Round = st.round
	plan.Notes = append(plan.Notes, "replayed "+itoa(len(events))+" events")

	// Step 3: a run that was already terminal needs nothing.
	if st.state.Terminal() {
		plan.Decision = Skip
		plan.Notes = append(plan.Notes, "run is already terminal: "+string(st.state))
		return plan, r.withCheckpoint(ctx, &plan)
	}
	if !st.state.Resumable() {
		plan.Decision = MarkFailed
		plan.Notes = append(plan.Notes, "state "+string(st.state)+" is not resumable")
		return plan, r.withCheckpoint(ctx, &plan)
	}

	// Step 4: locate the last safe checkpoint.
	if err := r.withCheckpoint(ctx, &plan); err != nil {
		return plan, err
	}

	// Step 5: identify incomplete tool calls.
	plan.Incomplete = incompleteCalls(events)

	// Step 6: classify each incomplete call and split into replayable versus
	// uncertain.
	for i := range plan.Incomplete {
		ic := &plan.Incomplete[i]
		class, err := r.classify(ctx, *ic)
		if err != nil {
			// An unclassifiable call is treated as non-idempotent: the safe
			// default is to ask rather than to replay.
			class = ports.NonIdempotent
			plan.Notes = append(plan.Notes,
				"classification failed for "+ic.ToolCallID+", assuming non-idempotent")
		}
		ic.Class = class

		switch {
		case ic.HasResult:
			// Complete: nothing to replay.
		case !ic.StartedReported:
			// The call never started, so it cannot have taken effect. Replaying
			// it is safe regardless of class.
			plan.ReplayableToolCalls = append(plan.ReplayableToolCalls, ic.ToolCallID)
		case class.SafeToReplay():
			plan.ReplayableToolCalls = append(plan.ReplayableToolCalls, ic.ToolCallID)
		default:
			// Started, no result, and not safely repeatable. This is exactly
			// the case the acceptance criterion calls out: a non-idempotent
			// tool call must not be repeated automatically after a crash.
			plan.UncertainToolCalls = append(plan.UncertainToolCalls, ic.ToolCallID)
		}
	}

	// Step 7: enforce the recovery attempt budget.
	attempts := 0
	if r.cfg.attempts != nil {
		attempts = r.cfg.attempts(runID)
	} else if st.recoveryAttempts > 0 {
		attempts = st.recoveryAttempts
	}
	if r.cfg.MaxRecoveryAttempts > 0 && attempts >= r.cfg.MaxRecoveryAttempts {
		plan.Decision = MarkFailed
		plan.Notes = append(plan.Notes,
			"exceeded the automatic recovery budget ("+itoa(attempts)+"/"+itoa(r.cfg.MaxRecoveryAttempts)+
				"); failing rather than looping across restarts")
		return plan, nil
	}

	if len(plan.UncertainToolCalls) > 0 {
		plan.Decision = ResumeAfterConfirm
		plan.Notes = append(plan.Notes,
			"non-idempotent tool calls have unknown outcomes; user confirmation required before resuming")
		return plan, nil
	}

	plan.Decision = ResumeAuto
	if len(plan.ReplayableToolCalls) > 0 {
		plan.Notes = append(plan.Notes,
			"resuming; "+itoa(len(plan.ReplayableToolCalls))+" incomplete call(s) will be replayed")
	} else {
		plan.Notes = append(plan.Notes, "resuming; no incomplete tool calls")
	}
	return plan, nil
}

// withCheckpoint records the last safe checkpoint for the plan.
func (r *Recovery) withCheckpoint(ctx context.Context, plan *RecoveryPlan) error {
	if r.cfg.Checkpoints == nil {
		return nil
	}
	cp, err := r.cfg.Checkpoints.Restore(ctx, plan.RunID, plan.LastSeq)
	if err != nil {
		// A missing checkpoint is not fatal: recovery can resume from the event
		// log alone, because the log is the authority for state.
		plan.Notes = append(plan.Notes, "checkpoint lookup failed: "+types.RedactString(err.Error()))
		return nil
	}
	if cp == nil {
		plan.Notes = append(plan.Notes, "no safe checkpoint; resuming from the event log")
		return nil
	}
	plan.CheckpointSeq = cp.Seq
	plan.Notes = append(plan.Notes,
		"last safe checkpoint "+cp.ID+" at seq "+itoa(int(cp.Seq))+
			" (round "+itoa(cp.Round)+", state "+string(cp.State)+")")
	return nil
}

// classify asks the idempotency store, falling back to the name-based lookup
// when the call ID is unknown.
func (r *Recovery) classify(ctx context.Context, ic IncompleteCall) (ports.IdempotencyClass, error) {
	if ic.ToolCallID != "" {
		if c, err := r.cfg.Idempotency.Classify(ctx, ic.ToolCallID); err == nil && c.Valid() {
			return c, nil
		}
	}
	if ic.ToolName == "" {
		return ports.NonIdempotent, types.NewError(types.CodeInvalidArgument,
			"cannot classify a call with neither id nor name")
	}
	c, err := r.cfg.Idempotency.ClassifyByName(ctx, ic.ToolName)
	if err != nil {
		return ports.NonIdempotent, err
	}
	if !c.Valid() {
		return ports.NonIdempotent, types.NewError(types.CodeInvalidArgument,
			"idempotency store returned unknown class %q for tool %q", string(c), ic.ToolName)
	}
	return c, nil
}

// recoveredState is the folded view of a run's durable log.
type recoveredState struct {
	sessionID string
	state     types.RunState
	round     int
	// recoveryAttempts counts prior recovery passes recorded in the log, which
	// is how the attempt budget survives a restart.
	recoveryAttempts int
}

// foldEvents replays a run's events into its last observed state.
//
// The state comes from the newest event that carries one, which is why every
// lifecycle event in this engine stamps State. Folding rather than trusting a
// single "last state" event means a truncated log still yields a usable state.
func foldEvents(events []types.Event) recoveredState {
	st := recoveredState{state: types.StateCreated}
	for _, e := range events {
		if e.SessionID != "" {
			st.sessionID = e.SessionID
		}
		if e.State != "" && e.State.Valid() {
			st.state = e.State
		}
		if e.Round > st.round {
			st.round = e.Round
		}
		if e.Type == types.EventRunRecovering || e.Type == types.EventRunResumed {
			st.recoveryAttempts++
		}
	}
	if st.state == types.StateRecovering {
		// A run that was itself mid-recovery when the process died is still
		// resumable, but it starts from its pre-recovery state, which recovery
		// cannot know from the log alone. Treating it as "queued" is the
		// conservative choice: it re-enters the normal path without replaying a
		// phase that may have been half-applied.
		st.state = types.StateQueued
	}
	return st
}

// verifySequence checks that event sequences are contiguous and ascending.
// It returns the highest sequence seen and whether the sequence is sound.
//
// A gap means a write was lost, so the log underspecifies what happened; a
// duplicate means a write was replayed, so the log overspecifies it. Neither is
// safe to resume from, which is why both are reported as failure.
func verifySequence(events []types.Event) (bool, uint64) {
	var last uint64
	ok := true
	for i, e := range events {
		if e.Seq == 0 {
			ok = false
			continue
		}
		if i > 0 {
			if e.Seq <= last {
				ok = false
			} else if e.Seq != last+1 {
				ok = false
			}
		}
		if e.Seq > last {
			last = e.Seq
		}
	}
	return ok, last
}

// incompleteCalls finds tool calls that were requested but not observed to
// complete.
//
// Only calls that appear in a requested/started event are considered: a call
// the model asked for in a round whose request never reached the log cannot be
// identified, and recovery does not speculate about it.
func incompleteCalls(events []types.Event) []IncompleteCall {
	type callState struct {
		ic IncompleteCall
	}
	calls := make(map[string]*callState)
	var order []string

	ensure := func(id, name string) *callState {
		if id == "" {
			id = "__unknown__:" + name
		}
		if cs, ok := calls[id]; ok {
			if cs.ic.ToolName == "" {
				cs.ic.ToolName = name
			}
			return cs
		}
		cs := &callState{ic: IncompleteCall{ToolCallID: id, ToolName: name}}
		calls[id] = cs
		order = append(order, id)
		return cs
	}

	for _, e := range events {
		switch e.Type {
		case types.EventToolCallRequested:
			cs := ensure(e.ToolCallID, e.ToolName)
			_ = cs
		case types.EventToolStarted:
			cs := ensure(e.ToolCallID, e.ToolName)
			cs.ic.StartedReported = true
		case types.EventToolCompleted, types.EventToolFailed, types.EventToolCancelled:
			cs := ensure(e.ToolCallID, e.ToolName)
			cs.ic.HasResult = true
		}
	}

	out := make([]IncompleteCall, 0, len(order))
	for _, id := range order {
		cs := calls[id]
		if !cs.ic.HasResult {
			out = append(out, cs.ic)
		}
	}
	return out
}

// PlanToWaiting builds the transition payload that parks a run for user
// confirmation, listing the calls whose outcome is unknown.
func (p RecoveryPlan) PlanToWaiting() agent.Transition {
	return agent.Transition{
		RunID:     p.RunID,
		SessionID: p.SessionID,
		To:        types.StateWaitingUser,
		Round:     p.Round,
		Reason:    "crash recovery found non-idempotent tool calls with unknown outcomes",
		WaitingReason: "崩溃恢复：以下非幂等工具调用的执行结果未知，需要你确认是否重试：" +
			joinIDs(p.UncertainToolCalls),
		UncertainToolCalls: append([]string(nil), p.UncertainToolCalls...),
	}
}

// Wire converts a plan into the IPC-facing form declared in internal/types.
//
// The two types are deliberately separate: this package's plan carries the
// engine's own naming, while types.RecoveryPlan is the wire contract task 01
// serializes. Keeping the conversion explicit means a field added here is a
// conscious decision about whether it crosses the boundary, rather than leaking
// by default.
func (p RecoveryPlan) Wire() types.RecoveryPlan {
	return types.RecoveryPlan{
		RunID:               p.RunID,
		SessionID:           p.SessionID,
		Decision:            string(p.Decision),
		ResumeState:         p.ResumeState,
		LastSeq:             p.LastSeq,
		CheckpointSeq:       p.CheckpointSeq,
		Round:               p.Round,
		UncertainToolCalls:  append([]string(nil), p.UncertainToolCalls...),
		ReplayableToolCalls: append([]string(nil), p.ReplayableToolCalls...),
		SequenceOK:          p.SequenceOK,
		Notes:               append([]string(nil), p.Notes...),
	}
}

// WirePlans converts a slice of plans for the IPC boundary.
func WirePlans(plans []RecoveryPlan) []types.RecoveryPlan {
	out := make([]types.RecoveryPlan, 0, len(plans))
	for _, p := range plans {
		out = append(out, p.Wire())
	}
	return out
}

// joinIDs renders an ID list for a user-facing message.
func joinIDs(ids []string) string {
	if len(ids) == 0 {
		return "(none)"
	}
	out := ""
	for i, id := range ids {
		if i > 0 {
			out += ", "
		}
		out += id
	}
	return out
}
