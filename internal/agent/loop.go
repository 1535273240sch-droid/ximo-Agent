// Package agent implements the recoverable Think→Tool Calls→Observe state
// machine.
//
// The loop is the direct successor of v1's agent-loop.ts, with three changes
// the architecture requires:
//
//  1. It is a state machine, not a straight-line function: every phase is a
//     named state (types.RunState) transition, so a crash can be resumed from
//     the last durable boundary rather than restarting the task.
//  2. Tool calls run through the three-layer scheduler instead of being run
//     inline, so a burst of parallel calls is bounded by the documented caps.
//  3. A panic in a tool is contained and reported; a panic in the core is not
//     recovered at all and terminates the process (see supervision.go).
package agent

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/ports"
	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// LoopSink receives the loop's events. The engine implements it: the loop
// decides *what happened*, the engine decides how it is persisted, fanned out
// and coalesced.
type LoopSink interface {
	// Emit records one loop event. The loop calls it synchronously, so an
	// implementation that blocks will slow the loop.
	Emit(ctx context.Context, ev LoopEvent) error
}

// LoopSinkFunc adapts functions to LoopSink, for tests.
type LoopSinkFunc struct {
	OnEmit func(ctx context.Context, ev LoopEvent) error
}

// Emit implements LoopSink.
func (f LoopSinkFunc) Emit(ctx context.Context, ev LoopEvent) error {
	if f.OnEmit == nil {
		return nil
	}
	return f.OnEmit(ctx, ev)
}

// LoopEvent is a loop-level occurrence. It is deliberately coarser than
// types.Event: the engine maps it onto the durable/ephemeral split, which is
// the only place that policy should live.
type LoopEvent struct {
	Type       types.EventType
	Round      int
	ToolCallID string
	ToolName   string
	State      types.RunState
	PrevState  types.RunState
	Data       map[string]any
	Message    string
	Err        error
	At         time.Time
}

// ToolOutcome is the result of one dispatched tool call.
type ToolOutcome struct {
	Call   types.ToolCall
	Result types.ToolResult
	// Err is set when the dispatch itself failed (scheduler rejection,
	// resource starvation, cancellation).
	Err error
	// Panicked holds a recovered panic value when the tool goroutine blew up.
	Panicked any
	// Denied is set when the permission layer refused the call.
	Denied bool
	// NeedsConfirmation is set when the call cannot proceed without a human
	// decision; the loop parks the run in waiting_user.
	NeedsConfirmation bool
	// ConfirmationMessage explains what the user is being asked.
	ConfirmationMessage string
	Duration            time.Duration
}

// ToolDispatcher executes one tool call. The engine supplies an implementation
// that routes the call through the three-layer scheduler; tests supply a
// direct one.
type ToolDispatcher interface {
	// Dispatch runs one tool call, honouring ctx.
	Dispatch(ctx context.Context, call types.ToolCall) ToolOutcome
}

// ToolDispatcherFunc adapts a function to ToolDispatcher.
type ToolDispatcherFunc func(ctx context.Context, call types.ToolCall) ToolOutcome

// Dispatch implements ToolDispatcher.
func (f ToolDispatcherFunc) Dispatch(ctx context.Context, c types.ToolCall) ToolOutcome {
	return f(ctx, c)
}

// Confirmer resolves a call that needs a human decision. Returning an error
// parks the run in waiting_user rather than failing it, because a missing
// answer is a pause, not a failure.
type Confirmer interface {
	// Confirm reports whether the call may proceed. A nil error with allowed
	// false means the user declined; the loop feeds the refusal back to the
	// model as a tool error.
	Confirm(ctx context.Context, call types.ToolCall, message string) (allowed bool, err error)
}

// ConfirmerFunc adapts a function to Confirmer.
type ConfirmerFunc func(ctx context.Context, call types.ToolCall, message string) (bool, error)

// Confirm implements Confirmer.
func (f ConfirmerFunc) Confirm(ctx context.Context, c types.ToolCall, m string) (bool, error) {
	return f(ctx, c, m)
}

// Reviewer is the ultra-mode quality gate.
//
// Naming note: this is the v1 "监督审查Agent" (supervisor.ts), which reviews
// the model for laziness, drift and rule violations. It is NOT the
// process-level Supervisor from task 01, which restarts a crashed Engine. The
// two are unrelated, so this type is named Reviewer to keep the distinction
// obvious at every call site.
type Reviewer interface {
	// Review inspects one round and returns a verdict.
	Review(ctx context.Context, snap RoundSnapshot) (Review, error)
}

// ReviewerFunc adapts a function to Reviewer.
type ReviewerFunc func(ctx context.Context, snap RoundSnapshot) (Review, error)

// Review implements Reviewer.
func (f ReviewerFunc) Review(ctx context.Context, s RoundSnapshot) (Review, error) { return f(ctx, s) }

// ReviewSummary is one tool outcome as the reviewer sees it.
type ReviewSummary struct {
	Name    string
	Success bool
	Summary string
}

// Review is an ultra-mode verdict.
type Review struct {
	Verdict    Verdict
	Issues     []string
	Correction string
	Severity   string
	Round      int
	// CorrectionMessage is the system message injected into the conversation
	// when the verdict warrants one.
	CorrectionMessage string
}

// Verdict enumerates the reviewer's conclusions.
type Verdict string

const (
	VerdictOnTrack   Verdict = "on_track"
	VerdictLazy      Verdict = "lazy"
	VerdictOffTrack  Verdict = "off_track"
	VerdictViolation Verdict = "violation"
)

// Valid reports whether v is a known verdict.
func (v Verdict) Valid() bool {
	switch v {
	case VerdictOnTrack, VerdictLazy, VerdictOffTrack, VerdictViolation:
		return true
	default:
		return false
	}
}

// NeedsCorrection reports whether the verdict warrants injecting a corrective
// system message.
func (v Verdict) NeedsCorrection() bool { return v != VerdictOnTrack && v != "" }

// LoopInput is everything one run of the loop needs.
type LoopInput struct {
	RunID     string
	SessionID string
	Request   types.SubmitRequest
	// Conversation carries the message history and the live tool catalogue.
	// The loop mutates it in place; nothing else may touch it concurrently.
	Conversation *Conversation
	// Machine registers every state transition. Required.
	Machine *Machine

	// PlanDecision and PendingPlan carry the user's answer to a plan proposed by
	// an earlier invocation of this run (task 4).
	//
	// They are separate from Request because they are not part of the original
	// submission: Request.PlanMode says "plan before executing", whereas these
	// say "here is the answer to the plan you showed me". Keeping them apart is
	// what lets a parked run be resumed with a decision while its original
	// request stays untouched.
	//
	// Both are ignored when Request.PlanMode is false, so a run that never
	// opted in cannot be steered by them.
	PlanDecision PlanDecision
	// PendingPlan is the plan text the decision applies to. On approval the loop
	// appends it to the conversation as the plan of record; on rejection it is
	// discarded and a fresh plan is requested.
	PendingPlan string
	// PlanRevision counts plans already proposed for this run, so the UI and the
	// event payload can tell a retry from a first draft.
	PlanRevision int

	// ToolDecisions carries the user's answers to tool-permission prompts (F5),
	// keyed by tool call ID.
	//
	// It is consumed by observe: a call that needs confirmation and has a
	// decision here proceeds (approve) or is recorded as refused (deny) instead
	// of parking the run again. A call with no entry parks the run exactly as
	// before, so a run that never hit a permission prompt is unaffected.
	//
	// The engine copies the map in and clears it afterwards: an approval is
	// spent once, which is what keeps "the user said yes five rounds ago" from
	// silently authorising a different call now. Run() folds the map into the
	// conversation (which is per-run state) and clears it here, so nothing
	// downstream can accidentally re-apply a decision.
	ToolDecisions map[string]bool
}

// LoopResult is the terminal outcome of a run.
type LoopResult struct {
	State         types.RunState
	Answer        string
	Rounds        int
	Continuations int
	Usage         ports.Usage
	ToolCalls     int
	Err           error
	// Plan and PlanRevision report the plan the run stopped on, set only when
	// the run parked in waiting_user for a plan decision. The engine stores them
	// so the next invocation (the user's answer) knows what it is answering.
	Plan         string
	PlanRevision int
	// Closure is the run's deterministic closure report (F2/F4). It is the same
	// value the run.closure event carried, so the engine can persist it on the
	// run record without re-deriving it.
	Closure types.RunClosureReport
	// PendingToolCallIDs lists the tool calls this run parked on, in request
	// order. Only the park path sets it. The engine stores the set so a
	// user decision (or a recovery pass) can be matched against it: without
	// this, a decision RPC could not tell "approve this specific call" from
	// "approve whatever is pending".
	PendingToolCallIDs []string
	// PendingToolCallNames maps the same call IDs to their tool names, for the
	// authorisation card and for the session-level "remember" rule.
	PendingToolCallNames map[string]string
}

// Loop is the agent loop. It is stateless between runs: per-run state lives in
// LoopInput.Conversation, so one Loop instance can drive many runs
// concurrently as long as each run has its own Conversation.
type Loop struct {
	cfg        types.AgentConfig
	provider   ports.Provider
	dispatcher ToolDispatcher
	compactor  *Compactor
	planner    Planner
	reviewer   Reviewer
	confirmer  Confirmer
	guard      *PanicGuard
	sink       LoopSink
	// maxParallelTools bounds the goroutines a single round may spawn. It
	// matches the per-session cap so a pathological model response cannot
	// create unbounded goroutines that merely wait in the scheduler.
	maxParallelTools int
	// lastParkedCalls records the calls that parked the current invocation, so
	// runRound can report them to the engine as the pending approval set.
	//
	// It is per-Loop rather than per-Conversation because it describes "this
	// invocation parked", not run state: the engine clears and re-reads it on
	// every park, and the loop is single-goroutine per run.
	lastParkedCalls []ToolOutcome
}

// LoopConfig assembles a Loop. Only Config, Provider, Dispatcher and Sink are
// required; the optional collaborators simply disable their features when nil
// (no planner means no planning phase, no reviewer means no ultra review).
type LoopConfig struct {
	Config     types.AgentConfig
	Provider   ports.Provider
	Dispatcher ToolDispatcher
	Sink       LoopSink
	Planner    Planner
	Reviewer   Reviewer
	Confirmer  Confirmer
	Compactor  *Compactor
	Guard      *PanicGuard
	// MaxParallelTools overrides the per-round goroutine bound. Zero uses the
	// per-session tool cap.
	MaxParallelTools int
}

// NewLoop builds a loop.
func NewLoop(cfg LoopConfig) (*Loop, error) {
	if cfg.Provider == nil {
		return nil, types.NewError(types.CodeInvalidArgument, "loop requires a provider")
	}
	if cfg.Dispatcher == nil {
		return nil, types.NewError(types.CodeInvalidArgument, "loop requires a tool dispatcher")
	}
	if cfg.Sink == nil {
		return nil, types.NewError(types.CodeInvalidArgument, "loop requires an event sink")
	}
	if err := cfg.Config.Validate(); err != nil {
		return nil, err
	}
	maxParallel := cfg.MaxParallelTools
	if maxParallel <= 0 {
		maxParallel = types.DefaultMaxConcurrentToolsPerSession
	}
	compactor := cfg.Compactor
	if compactor == nil {
		compactor = NewCompactor(cfg.Config, nil)
	}
	return &Loop{
		cfg:              cfg.Config,
		provider:         cfg.Provider,
		dispatcher:       cfg.Dispatcher,
		compactor:        compactor,
		planner:          cfg.Planner,
		reviewer:         cfg.Reviewer,
		confirmer:        cfg.Confirmer,
		guard:            cfg.Guard,
		sink:             cfg.Sink,
		maxParallelTools: maxParallel,
	}, nil
}

// Run drives one run to a terminal state.
//
// It returns only when the run is terminal: a completed answer, a cancellation,
// a failure, or a park in waiting_user (which is terminal for this invocation —
// resuming continues from the same conversation). Every state transition is
// registered with the machine, and every exit path sets a terminal state.
func (l *Loop) Run(ctx context.Context, in LoopInput) LoopResult {
	if in.Machine == nil {
		return LoopResult{
			State: types.StateFailed,
			Err:   types.NewError(types.CodeInvalidArgument, "loop requires a state machine"),
		}
	}
	conv := in.Conversation
	if conv == nil {
		conv = NewConversation("", in.Request.Prompt, in.Request.Tools)
	}

	// The per-segment round budget and the continuation budget come from the
	// request: long-task mode trades a flat cap for repeated segments.
	roundsPerSegment := l.cfg.MaxToolRounds
	maxContinuations := 0
	if in.Request.LongTask {
		roundsPerSegment = l.cfg.LongTaskRoundsPerSegment
		maxContinuations = l.cfg.LongTaskMaxContinuations
	}
	if in.Request.MaxRounds > 0 && !in.Request.LongTask {
		roundsPerSegment = in.Request.MaxRounds
	}

	res := LoopResult{State: types.StateFailed}

	// A machine handed over in "created" has not been queued yet. The engine
	// normally performs this transition in Submit, but the loop makes itself
	// self-sufficient here so it can be driven directly (by tests, or by a
	// recovery path that resumes into created) without every caller having to
	// replicate the engine's bookkeeping.
	if in.Machine.State() == types.StateCreated {
		if err := l.transition(ctx, in, types.StateQueued, "accepted"); err != nil {
			return l.finishFailed(ctx, in, conv, res, err)
		}
	}

	// F5: fold the user's permission answers into this run's conversation and
	// consume them from the input, so a retry cannot re-apply an old approval.
	conv.FoldToolDecisions(in.ToolDecisions)
	in.ToolDecisions = nil

	// Phase 0: planning. It narrows the tool catalogue before the first round,
	// which is what keeps the model from being handed a 40-tool catalogue for a
	// one-file task.
	if err := l.runPlanning(ctx, in, conv); err != nil {
		if types.IsCancelled(err) {
			return l.finishCancelled(ctx, in, conv, res)
		}
		return l.finishFailed(ctx, in, conv, res, err)
	}

	// Phase 0.25 (task 4): the user-visible plan mode. It runs strictly after
	// the internal planning phase and is gated on the request flag, so a run
	// that did not opt in behaves exactly as it did before this phase existed.
	if in.Request.PlanMode {
		parked, err := l.runPlanMode(ctx, in, conv, res)
		if err != nil {
			if types.IsCancelled(err) {
				return l.finishCancelled(ctx, in, conv, res)
			}
			return l.finishFailed(ctx, in, conv, res, err)
		}
		if parked != nil {
			// The plan is on screen and the run is parked. Returning here is what
			// keeps execution from starting before the user has decided; resuming
			// re-enters Run() with the decision set.
			return *parked
		}
	}

	// Phase 0.5: the segment loop. Each iteration is one round; when a
	// segment's rounds are exhausted the loop starts a new segment with a
	// continuation prompt until the continuation budget is spent.
	for segment := 0; ; segment++ {
		for round := 0; round < roundsPerSegment; round++ {
			if err := l.checkCancelled(ctx); err != nil {
				return l.finishCancelled(ctx, in, conv, res)
			}

			// F5 is resolved inside observe(): a call the user approved is
			// allowed to proceed, a refused one becomes a denial observation.
			// No replay of the original call is needed or attempted — see the
			// note on ToolNameDecisionKey.

			outcome, err := l.runRound(ctx, in, conv, round)
			res.Rounds = conv.Rounds
			res.Usage = conv.Usage
			if err != nil {
				return l.finishFailed(ctx, in, conv, res, err)
			}
			if outcome != nil {
				if outcome.WrapUp {
					// F1: the round ended with "stop" but the run is not
					// allowed to finish on it. Every-todo-done means the model
					// was told (by the pending AllTodosDonePrompt) to summarise,
					// and it has not answered that yet; budget exhaustion means
					// it never will without being asked. Either way the run owes
					// the user one final tool-less round.
					return l.forceWrapUp(ctx, in, conv, res, outcome.Reason)
				}
				res.State = outcome.State
				res.Answer = outcome.Answer
				res.Err = outcome.Err
				res.ToolCalls = conv.toolCallCount
				res.Closure = conv.lastClosure
				res.PendingToolCallIDs = outcome.PendingToolCallIDs
				res.PendingToolCallNames = outcome.PendingToolCallNames
				return res
			}
		}

		// The segment's rounds are spent. Continue only if the long-task budget
		// allows it; otherwise fall through to the forced wrap-up.
		if segment >= maxContinuations {
			break
		}
		if err := l.checkCancelled(ctx); err != nil {
			return l.finishCancelled(ctx, in, conv, res)
		}
		conv.Continuations++
		if err := l.emit(ctx, LoopEvent{
			Type:    types.EventContinuation,
			Round:   conv.Rounds,
			Message: "long-task continuation",
			Data: map[string]any{
				"segment":         segment + 1,
				"maxSegments":     maxContinuations,
				"completedRounds": conv.Rounds,
			},
		}); err != nil {
			return l.finishFailed(ctx, in, conv, res, err)
		}
		conv.Append(ports.Message{
			Role:    ports.RoleUser,
			Content: LongTaskContinuation(segment+1, maxContinuations, conv.Rounds),
		})
	}

	// The round budget is exhausted: force a text-only final answer. Tools are
	// withheld for this round, which is what makes the model produce a summary
	// instead of asking for another tool it will never get to run.
	//
	// F2: the reason travels with the call so the closure report can label the
	// result partial instead of letting a budget-exhausted run look finished.
	return l.forceWrapUp(ctx, in, conv, res, types.ClosureReasonBudgetExhausted)
}

// resultFromOutcome converts a failed round outcome into a terminal LoopResult,
// giving the failure the terminal bookkeeping the run needs.
func (l *Loop) resultFromOutcome(outcome *roundOutcome, conv *Conversation, res LoopResult) LoopResult {
	res.Rounds = conv.Rounds
	res.Usage = conv.Usage
	res.ToolCalls = conv.toolCallCount
	if outcome == nil {
		res.State = types.StateFailed
		res.Err = types.NewError(types.CodeInternal, "loop produced no outcome and no error")
		return res
	}
	res.State = outcome.State
	res.Answer = outcome.Answer
	res.Err = outcome.Err
	return res
}

// roundOutcome is the terminal result of a round, when the round ends the run.
type roundOutcome struct {
	State  types.RunState
	Answer string
	Err    error
	// WrapUp asks Run to spend one final, tool-less summarising round before
	// the run terminates (F1).
	//
	// Why a flag rather than the round doing the summary itself: the summary is
	// a *round* (it needs the run's round bookkeeping, the streaming deltas and
	// the terminal transition), and runRound is where rounds are executed. The
	// flag keeps that ownership in one place instead of duplicating the
	// provider call inside observe.
	WrapUp bool
	// Reason names why the wrap-up is happening, so the closure report can tell
	// "the todo list finished" from "the round budget ran out". See the
	// types.ClosureReason* constants.
	Reason string
	// PendingToolCallIDs / PendingToolCallNames describe the calls this round
	// parked on (F5), so Run can hand them back to the engine. They are empty on
	// every other outcome.
	PendingToolCallIDs   []string
	PendingToolCallNames map[string]string
}

// runRound executes one Think→Tool Calls→Observe cycle.
//
// The first return value is non-nil when the run must stop after this round —
// a final answer, a cancellation, a failure, or a park in waiting_user. A nil
// outcome with a nil error means "continue to the next round".
func (l *Loop) runRound(ctx context.Context, in LoopInput, conv *Conversation, round int) (*roundOutcome, error) {
	conv.Rounds++
	in.Machine.SetRound(round)

	// Observe the state machine move into thinking. The machine's own sink
	// writes the durable transition event; the loop's emit adds the round
	// detail the UI renders.
	if err := l.transition(ctx, in, types.StateThinking, "model round"); err != nil {
		return l.failedOutcome(err)
	}
	if err := l.emit(ctx, LoopEvent{
		Type: types.EventRoundStarted, Round: round,
		State: types.StateThinking, PrevState: types.StateThinking,
	}); err != nil {
		return l.failedOutcome(err)
	}

	// Think: one provider round.
	resp, err := l.think(ctx, in, conv, round)
	if err != nil {
		if types.IsCancelled(err) {
			return &roundOutcome{State: types.StateCancelled, Err: err}, nil
		}
		// 错误事件必须先于终态迁移发出：事件流在终态即关闭，若先迁移，
		// 携带真实原因（如 HTTP 400/401）的错误事件将永远到不了订阅方，
		// 界面上只剩一句含糊的 "provider round failed"。
		if eerr := l.emitError(ctx, round, "", "", err); eerr != nil {
			return l.failedOutcome(eerr)
		}
		if terr := l.transition(ctx, in, types.StateFailed, "provider round failed"); terr != nil {
			return l.failedOutcome(terr)
		}
		return &roundOutcome{State: types.StateFailed, Err: err}, nil
	}
	conv.Usage = mergeUsage(conv.Usage, resp.Usage)

	// Compact between rounds, before the next request is assembled, so the
	// model sees the reduced context rather than the expanded one.
	if err := l.maybeCompact(ctx, in, conv, round); err != nil {
		if types.IsCancelled(err) {
			return &roundOutcome{State: types.StateCancelled, Err: err}, nil
		}
		if terr := l.transition(ctx, in, types.StateFailed, "compaction failed"); terr != nil {
			return l.failedOutcome(terr)
		}
		return &roundOutcome{State: types.StateFailed, Err: err}, nil
	}

	switch resp.FinishReason {
	case ports.FinishCancelled:
		return &roundOutcome{State: types.StateCancelled,
			Err: types.NewError(types.CodeCancelled, "user cancelled the task")}, nil

	case ports.FinishError:
		e := types.NewError(types.CodeProviderFailed, "provider error: %s", resp.Error)
		// 与上方一致：真实原因先行，终态迁移随后。
		if eerr := l.emitError(ctx, round, "", "", e); eerr != nil {
			return l.failedOutcome(eerr)
		}
		if terr := l.transition(ctx, in, types.StateFailed, "provider error"); terr != nil {
			return l.failedOutcome(terr)
		}
		return &roundOutcome{State: types.StateFailed, Err: e}, nil

	case ports.FinishStop:
		// The model produced a final answer.
		//
		// F4: an empty answer is not an answer. It used to be reported as
		// "completed" with an empty body, which is the one outcome a user can
		// never act on. Give the model exactly one chance to produce text; if it
		// refuses again the run fails with a reason instead of pretending.
		if strings.TrimSpace(resp.Content) == "" {
			return l.handleEmptyAnswer(ctx, in, conv, round, resp)
		}
		return l.finishWithAnswer(ctx, in, conv, round, resp, types.ClosureReasonCompleted, false)

	case ports.FinishLength:
		// F3: an answer cut off by the output-length limit is not a finished
		// answer. Ask the model to continue from where it stopped, up to the
		// configured cap; only when that budget is spent is the truncated text
		// accepted — and then honestly marked partial.
		maxContinues := l.cfg.MaxLengthContinues
		if maxContinues <= 0 {
			maxContinues = types.DefaultMaxLengthContinues
		}
		if conv.Closure.LengthContinues < maxContinues {
			conv.Closure.LengthContinues++
			conv.AppendAssistant(resp.Content, resp.ReasoningContent, nil,
				effortOr(in.Request.Effort).ThinkingEnabled())
			conv.Append(ports.Message{Role: ports.RoleUser, Content: LengthContinuationPrompt})
			// A nil outcome with a nil error means "run the next round".
			return nil, nil
		}
		conv.Closure.Truncated = true
		return l.finishWithAnswer(ctx, in, conv, round, resp, types.ClosureReasonTruncated, true)

	case ports.FinishToolCalls:
		// Fall through to observation below.

	default:
		e := types.NewError(types.CodeInternal, "unknown finish reason %q", string(resp.FinishReason))
		if terr := l.transition(ctx, in, types.StateFailed, "unknown finish reason"); terr != nil {
			return l.failedOutcome(terr)
		}
		return &roundOutcome{State: types.StateFailed, Err: e}, nil
	}

	if len(resp.ToolCalls) == 0 {
		// The finish reason claims tool calls but none arrived. Treat it as a
		// final answer rather than looping forever on empty rounds.
		if strings.TrimSpace(resp.Content) == "" {
			return l.handleEmptyAnswer(ctx, in, conv, round, resp)
		}
		return l.finishWithAnswer(ctx, in, conv, round, resp, types.ClosureReasonCompleted, false)
	}

	// Observe: execute the round's tool calls.
	stop, err := l.observe(ctx, in, conv, round, resp)
	if err != nil {
		if types.IsCancelled(err) {
			return &roundOutcome{State: types.StateCancelled, Err: err}, nil
		}
		if types.CodeOf(err) == types.CodeAwaitingUser {
			// The run is parked, not dead. The conversation keeps its place so
			// a resume continues from the same round. The pending calls are
			// reported so the engine can match a later decision against them.
			out := &roundOutcome{State: types.StateWaitingUser, Err: err}
			for _, oc := range l.lastParkedCalls {
				out.PendingToolCallIDs = append(out.PendingToolCallIDs, oc.Call.ID)
				if out.PendingToolCallNames == nil {
					out.PendingToolCallNames = make(map[string]string, len(l.lastParkedCalls))
				}
				out.PendingToolCallNames[oc.Call.ID] = oc.Call.Name
			}
			return out, nil
		}
		return l.failedOutcome(err)
	}
	if stop {
		// F1: every todo is done. Do NOT finish here on the model's older text:
		// the AllTodosDonePrompt observe() appended has not been read by anyone
		// yet, so finishing now would show a stale (often empty) answer as the
		// final one. Ask for a real summary round instead.
		return &roundOutcome{WrapUp: true, Reason: types.ClosureReasonTodosDone}, nil
	}
	return nil, nil
}

// finishWithAnswer emits the final answer, the closure report and the terminal
// transition, in the only order the event stream tolerates: answer, then
// closure, then the terminal state.
//
// Centralising it is not cosmetic. The ordering constraint ("events precede the
// terminal transition because the stream closes there") applied to two separate
// hand-written paths before, and the closure report added a third thing that had
// to be exactly once and exactly here.
func (l *Loop) finishWithAnswer(ctx context.Context, in LoopInput, conv *Conversation, round int, resp ports.ProviderResponse, reason string, truncated bool) (*roundOutcome, error) {
	conv.LastAnswer = resp.Content
	// The closure report is computed first so the final_answer payload can
	// carry its verdict in the same event: a second final_answer frame would be
	// redundant traffic, and deriving the verdict twice would risk the two
	// disagreeing.
	rep := types.BuildRunClosureReport(&conv.Closure, types.ClosureInput{
		Reason: reason, Answer: resp.Content,
	})
	conv.lastClosure = rep

	data := map[string]any{
		"finishReason": string(resp.FinishReason),
		"length":       len(resp.Content),
		"reason":       reason,
	}
	if truncated || rep.Incomplete {
		data["incomplete"] = true
	}
	if rep.Verdict != "" {
		data["verdict"] = rep.Verdict
	}
	// The model that actually served this round, when the provider reported it.
	// It is the evidence chain for "the model I picked was really used": the
	// request field is empty whenever the user chose to follow the global
	// default, so only an echo can prove it.
	if resp.Model != "" {
		data["model"] = resp.Model
	}
	if eerr := l.emit(ctx, LoopEvent{
		Type: types.EventFinalAnswer, Round: round,
		Message: resp.Content, Data: data,
	}); eerr != nil {
		return l.failedOutcome(eerr)
	}
	if _, cerr := l.emitClosureReport(ctx, conv, rep, reason); cerr != nil {
		return l.failedOutcome(cerr)
	}
	if terr := l.transition(ctx, in, types.StateCompleted, "final answer"); terr != nil {
		return l.failedOutcome(terr)
	}
	return &roundOutcome{State: types.StateCompleted, Answer: resp.Content}, nil
}

// handleEmptyAnswer implements F4: one retry, then an honest failure.
func (l *Loop) handleEmptyAnswer(ctx context.Context, in LoopInput, conv *Conversation, round int, resp ports.ProviderResponse) (*roundOutcome, error) {
	maxRetries := l.cfg.EmptyAnswerRetries
	if maxRetries == 0 {
		maxRetries = types.DefaultEmptyAnswerRetries
	}
	if conv.EmptyAnswerRetries < maxRetries {
		conv.EmptyAnswerRetries++
		conv.AppendAssistant(resp.Content, resp.ReasoningContent, resp.ToolCalls,
			effortOr(in.Request.Effort).ThinkingEnabled())
		conv.Append(ports.Message{Role: ports.RoleUser, Content: EmptyAnswerPrompt})
		return nil, nil
	}
	// Out of retries: fail loudly. The error event must precede the terminal
	// transition (the stream closes there), and the closure report is emitted
	// between them so the UI can show *why* the answer is missing.
	e := types.NewError(types.CodeProviderFailed,
		"模型连续两次返回空答复，本次 run 没有可用的最终答案")
	if eerr := l.emitError(ctx, round, "", "", e); eerr != nil {
		return l.failedOutcome(eerr)
	}
	rep, cerr := l.emitClosure(ctx, conv, types.ClosureInput{
		Reason: types.ClosureReasonEmpty, Answer: "",
	})
	if cerr != nil {
		return l.failedOutcome(cerr)
	}
	conv.lastClosure = rep
	if terr := l.transition(ctx, in, types.StateFailed, "empty final answer"); terr != nil {
		return l.failedOutcome(terr)
	}
	return &roundOutcome{State: types.StateFailed, Err: e}, nil
}

// emitClosure builds and emits the run.closure event.
//
// Exactly-once is guaranteed structurally rather than by convention: this is the
// only function that emits the event, and every terminal path calls it after its
// answer event and before its terminal transition.
func (l *Loop) emitClosure(ctx context.Context, conv *Conversation, in types.ClosureInput) (types.RunClosureReport, error) {
	rep := types.BuildRunClosureReport(&conv.Closure, in)
	return l.emitClosureReport(ctx, conv, rep, in.Reason)
}

// emitClosureReport emits an already-built report. It is separate from
// emitClosure because the completion paths build the report before the answer
// event (so the answer can carry the verdict) and emit it after.
func (l *Loop) emitClosureReport(ctx context.Context, conv *Conversation, rep types.RunClosureReport, reason string) (types.RunClosureReport, error) {
	checks := make([]map[string]any, 0, len(rep.Checks))
	for _, c := range rep.Checks {
		checks = append(checks, map[string]any{
			"id": c.ID, "label": c.Label, "pass": c.Pass, "note": c.Note,
		})
	}
	if err := l.emit(ctx, LoopEvent{
		Type: types.EventRunClosure, Round: conv.Rounds,
		Message: rep.Verdict,
		Data: map[string]any{
			"verdict":    rep.Verdict,
			"checks":     checks,
			"incomplete": rep.Incomplete,
			"reason":     reason,
		},
	}); err != nil {
		return rep, err
	}
	return rep, nil
}

// think performs one provider round, streaming deltas into the sink.
func (l *Loop) think(ctx context.Context, in LoopInput, conv *Conversation, round int) (ports.ProviderResponse, error) {
	req := ports.ProviderRequest{
		// 模型名留空是常态：UI 提交不指定模型，由 Provider 适配层回退到
		// 配置里的默认模型。绝不能在这里填占位名（如 "default"）——那会把
		// 字面量发给第三方接口，被按未知模型拒绝（HTTP 400）。
		Model:     in.Request.Model,
		Messages:  conv.Messages,
		Tools:     conv.Tools,
		Effort:    effortOr(in.Request.Effort),
		MaxTokens: 0,
	}
	// The deltas are ephemeral: they go to the sink, which coalesces them. The
	// loop never writes a durable event per token.
	req.OnDelta = func(d ports.Delta) {
		if d.Content == "" && d.Reasoning == "" {
			return
		}
		_ = l.emit(ctx, LoopEvent{
			Type: types.EventTokenDelta, Round: round,
			Data: map[string]any{"content": d.Content, "reasoning": d.Reasoning},
		})
	}
	req.OnUsage = func(u ports.Usage) {
		conv.Usage = mergeUsage(conv.Usage, u)
	}

	resp, err := l.provider.Complete(ctx, req)
	if err != nil {
		return ports.ProviderResponse{}, types.WrapError(types.CodeOf(err), err, "provider round")
	}
	// Record the assistant turn. Reasoning is round-tripped whenever thinking
	// is on: DeepSeek rejects the request with a 400 if an assistant turn's
	// reasoning_content is dropped while tools are present.
	conv.AppendAssistant(resp.Content, resp.ReasoningContent, resp.ToolCalls,
		effortOr(in.Request.Effort).ThinkingEnabled())

	if err := l.emit(ctx, LoopEvent{
		Type: types.EventRoundCompleted, Round: round,
		Data: map[string]any{
			"finishReason": string(resp.FinishReason),
			"toolCalls":    len(resp.ToolCalls),
			"usage":        resp.Usage,
			"model":        firstNonEmpty(resp.Model, in.Request.Model),
		},
	}); err != nil {
		return resp, err
	}
	return resp, nil
}

// observe dispatches a round's tool calls in parallel and folds the results
// back into the conversation in request order.
//
// Parallelism is bounded and every spawned goroutine is joined before observe
// returns, so a cancelled or panicking call cannot leave a goroutine behind.
// Results are applied strictly in the model's original call order, which keeps
// the conversation deterministic even though execution raced.
func (l *Loop) observe(ctx context.Context, in LoopInput, conv *Conversation, round int, resp ports.ProviderResponse) (bool, error) {
	calls := resp.ToolCalls
	conv.toolCallCount += len(calls)

	if err := l.transition(ctx, in, types.StateExecuting, "dispatching tool calls"); err != nil {
		return false, err
	}
	for _, c := range calls {
		if err := l.emit(ctx, LoopEvent{
			Type: types.EventToolCallRequested, Round: round,
			ToolCallID: c.ID, ToolName: c.Name,
			Data: map[string]any{"arguments": redactArgs(c.Arguments)},
		}); err != nil {
			return false, err
		}
	}

	outcomes := l.dispatchAll(ctx, round, calls)

	// F5: a call that needs confirmation is resolved from the user's decision
	// when one exists, instead of parking the run again. Without this the run
	// would park forever: the user's approval had nowhere to go.
	outcomes, parked, parkedCalls := l.applyToolDecisions(conv, outcomes)
	if len(parked) > 0 {
		l.lastParkedCalls = parkedCalls
		if err := l.transitionTo(ctx, in, types.StateWaitingUser, Transition{
			Reason:        "tool call requires user confirmation",
			WaitingReason: parked[0].ConfirmationMessage,
		}); err != nil {
			return false, err
		}
		// R5: the event carries everything the approval card needs to show
		// *what* the user is being asked to allow. It is emitted before the
		// park is reported so the UI has the request by the time it renders the
		// waiting state; the call ID is what the decision RPC will quote back.
		for _, oc := range parked {
			if err := l.emit(ctx, LoopEvent{
				Type: types.EventToolPermissionRequired, Round: round,
				ToolCallID: oc.Call.ID, ToolName: oc.Call.Name,
				Message: oc.ConfirmationMessage,
				Data: map[string]any{
					"arguments": redactArgs(oc.Call.Arguments),
					"callId":    oc.Call.ID,
					"toolName":  oc.Call.Name,
				},
			}); err != nil {
				return false, err
			}
		}
		return false, types.NewError(types.CodeAwaitingUser,
			"tool %s requires user confirmation", parked[0].Call.Name)
	}
	l.lastParkedCalls = nil

	allTodosDone := false
	for _, oc := range outcomes {
		if l.observeOne(ctx, in, conv, round, oc) {
			allTodosDone = true
		}
	}

	// Ultra mode: review the round once the real results are known. v1 runs the
	// review after execution precisely so it can see the outcomes.
	if err := l.reviewRound(ctx, in, conv, round, outcomes); err != nil {
		return false, err
	}

	if allTodosDone && l.cfg.StopAfterAllTodosDone {
		conv.AppendSystem(AllTodosDonePrompt)
		// Withholding the tools is what forces the model to summarise.
		conv.Tools = nil
		return true, nil
	}
	return false, nil
}

// applyToolDecisions resolves the calls that need confirmation against the
// user's decisions, and reports the ones that still have none.
//
// Three outcomes per call:
//
//   - NeedsConfirmation with a decision in ToolDecisions: the call is allowed
//     to proceed (approve) or is turned into a refusal indistinguishable from
//     the permission layer's own denial (deny), so the model sees one uniform
//     "the user declined" observation either way.
//   - NeedsConfirmation with no decision: the call stays parked.
//   - Anything else: untouched.
//
// The returned slices are ordered like the input, so the conversation and the
// event stream stay deterministic even though the decisions arrive from another
// goroutine.
func (l *Loop) applyToolDecisions(conv *Conversation, outcomes []ToolOutcome) (resolved []ToolOutcome, parked []ToolOutcome, parkedCalls []ToolOutcome) {
	if len(outcomes) == 0 {
		return outcomes, nil, nil
	}
	decide := func(call types.ToolCall) (approve, ok bool) {
		if v, found := conv.toolDecision(call.ID); found {
			return v, true
		}
		// A session-level approval ("remember" in the design note) is keyed by
		// tool name, because the re-issued call carries a new ID.
		if v, found := conv.toolDecision(ToolNameDecisionKey(call.Name)); found {
			return v, true
		}
		return false, false
	}
	resolved = make([]ToolOutcome, 0, len(outcomes))
	for _, oc := range outcomes {
		if !oc.NeedsConfirmation {
			resolved = append(resolved, oc)
			continue
		}
		approve, ok := decide(oc.Call)
		if !ok {
			parked = append(parked, oc)
			parkedCalls = append(parkedCalls, oc)
			continue
		}
		if !approve {
			// A refusal is recorded exactly like a permission denial so the
			// model gets the same observation it already knows how to react to.
			oc.Denied = true
			oc.NeedsConfirmation = false
			oc.ConfirmationMessage = ""
			resolved = append(resolved, oc)
			continue
		}
		oc.NeedsConfirmation = false
		oc.ConfirmationMessage = ""
		resolved = append(resolved, oc)
	}
	return resolved, parked, parkedCalls
}

// ToolNameDecisionKey is the key an engine uses in ToolDecisions to approve a
// whole tool by name rather than one specific call.
//
// It exists because an approval has to survive the model re-issuing the call:
// a tool_call ID belongs to the assistant turn that produced it, and a resumed
// run cannot reuse it. Approving "the file_write the user just allowed" by name
// is what actually unblocks the run, and the prefix keeps the two kinds of key
// from ever colliding with a real call ID.
func ToolNameDecisionKey(toolName string) string { return "name:" + toolName }

// takeApprovedCalls is intentionally absent.
//
// An approval resumes a run by REMEMBERING it (per call ID, and per tool name
// for a session-level "remember"), then letting the next model round re-issue
// the call. Replaying the original call directly is not possible on this wire
// format: a tool_call ID belongs to the assistant turn that produced it and the
// protocol only accepts a result that replies to the immediately preceding
// assistant message, so after a park and a resume the old pairing is dead. See
// ToolNameDecisionKey and applyToolDecisions.


func (l *Loop) dispatchAll(ctx context.Context, round int, calls []types.ToolCall) []ToolOutcome {
	outcomes := make([]ToolOutcome, len(calls))
	sem := make(chan struct{}, l.maxParallelTools)
	var wg sync.WaitGroup

	for i, call := range calls {
		wg.Add(1)
		// Acquire the semaphore before spawning so at most maxParallelTools
		// goroutines exist per round. Waiting here still respects ctx: if the
		// run is cancelled while queued, the goroutine is never started and the
		// outcome records the cancellation.
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			outcomes[i] = ToolOutcome{Call: call, Err: ctx.Err(), Panicked: nil}
			wg.Done()
			continue
		}

		go func(idx int, c types.ToolCall) {
			defer wg.Done()
			defer func() { <-sem }()
			// A tool goroutine is a permitted recover boundary. A panic is
			// contained, reported, and turned into a failed observation so the
			// round can still complete.
			outcomes[idx] = l.dispatchContained(ctx, c)
		}(i, call)
	}
	// Joining unconditionally is what guarantees no goroutine outlives the
	// round, even when the context was cancelled mid-flight.
	wg.Wait()
	return outcomes
}

// dispatchContained runs one tool call with a panic boundary around it.
func (l *Loop) dispatchContained(ctx context.Context, call types.ToolCall) (oc ToolOutcome) {
	oc.Call = call
	start := time.Now()

	if l.guard != nil {
		// CrashKindTool is one of the four permitted recover boundaries.
		// Passing CrashKindCore here would panic by design, which is the guard
		// rail that keeps the two halves from being confused.
		var recovered any
		var res ToolOutcome
		recovered = l.guard.Guard(types.CrashKindTool, "agent.tool", CrashMeta{
			ToolCallID: call.ID,
		}, func() {
			if err := ctx.Err(); err != nil {
				res = ToolOutcome{Call: call, Err: err}
				return
			}
			res = l.dispatcher.Dispatch(ctx, call)
		})
		if recovered != nil {
			// A panicking tool must not leak a half-built result, so the
			// outcome is replaced wholesale rather than patched.
			return ToolOutcome{
				Call:     call,
				Panicked: recovered,
				Duration: time.Since(start),
				Result: types.ToolResult{
					ToolCallID: call.ID, ToolName: call.Name, Success: false,
					Error: "tool panicked: " + types.RedactString(toString(recovered)),
				},
			}
		}
		res.Duration = time.Since(start)
		return res
	}

	if err := ctx.Err(); err != nil {
		oc.Err = err
		oc.Duration = time.Since(start)
		return oc
	}
	res := l.dispatcher.Dispatch(ctx, call)
	res.Duration = time.Since(start)
	return res
}

// observeOne folds one tool outcome into the conversation and reports whether
// this outcome completed the whole todo list.
func (l *Loop) observeOne(ctx context.Context, in LoopInput, conv *Conversation, round int, oc ToolOutcome) (allTodosDone bool) {
	call := oc.Call
	// F6: every outcome is recorded in the closure tracker under the tool's
	// target key, so "a later success resolved an earlier failure" is
	// expressible without a second pass over the event log.
	key := types.ToolCallKey(call.Name, call.Arguments)

	switch {
	case oc.Panicked != nil:
		detail := types.RedactString(toString(oc.Panicked))
		msg := "Error: tool panicked: " + detail
		_ = l.emit(ctx, LoopEvent{
			Type: types.EventToolFailed, Round: round,
			ToolCallID: call.ID, ToolName: call.Name,
			Message: msg, Data: map[string]any{
				"panicked":   true,
				"error":      detail,
				"durationMs": oc.Duration.Milliseconds(),
			},
		})
		conv.Closure.RecordToolCall(key, false, detail)
		conv.AppendToolResult(call.ID, msg)
		return false

	case oc.Err != nil:
		// A dispatch failure is reported to the model as an observation: it can
		// retry, choose another tool, or explain the failure, which beats
		// aborting the run. Cancellation is the exception, handled by the
		// caller before reaching here.
		detail := redactErr(oc.Err)
		msg := "Error: " + detail
		_ = l.emit(ctx, LoopEvent{
			Type: types.EventToolFailed, Round: round,
			ToolCallID: call.ID, ToolName: call.Name,
			Message: msg, Err: oc.Err,
			Data: map[string]any{
				"error":      detail,
				"durationMs": oc.Duration.Milliseconds(),
			},
		})
		conv.Closure.RecordToolCall(key, false, detail)
		conv.AppendToolResult(call.ID, msg)
		return false

	case oc.Denied:
		msg := "Error: 用户拒绝执行"
		_ = l.emit(ctx, LoopEvent{
			Type: types.EventToolCancelled, Round: round,
			ToolCallID: call.ID, ToolName: call.Name, Message: msg,
			Data: map[string]any{"denied": true, "durationMs": oc.Duration.Milliseconds()},
		})
		conv.Closure.RecordToolCall(key, false, msg)
		conv.AppendToolResult(call.ID, msg)
		return false

	default:
		if !oc.Result.Success {
			// v1 normalises failures to "Error: …" so a rebuilt request is
			// byte-identical to the streamed one, protecting the prompt cache.
			detail := firstNonEmpty(oc.Result.Error, oc.Result.Content, "工具执行失败")
			content := "Error: " + detail
			// F6: the failure carried only a duration before, so the UI could
			// not say *why* a tool failed. The error text and a bounded preview
			// of the output travel with the event now; both are redacted and
			// clamped so the event log and the IPC frame stay bounded.
			_ = l.emit(ctx, LoopEvent{
				Type: types.EventToolFailed, Round: round,
				ToolCallID: call.ID, ToolName: call.Name,
				Message: types.RedactString(oc.Result.Error),
				Data: map[string]any{
					"durationMs": oc.Duration.Milliseconds(),
					"error":      types.RedactString(detail),
					"result":     previewForUI(oc.Result.Content),
					"bytes":      len(oc.Result.Content),
				},
			})
			conv.Closure.RecordToolCall(key, false, detail)
			conv.AppendToolResult(call.ID,
				sanitizeContent(truncatePlain(content, l.cfg.MaxToolResultChars)))
			return false
		}
		// F6: the success payload carried only success/duration/bytes, so the
		// frontend's "execution result" block could never fill in. The preview
		// is redacted and clamped to 2 KiB: the full output belongs in the
		// conversation, never in a durable event that is also an IPC frame.
		_ = l.emit(ctx, LoopEvent{
			Type: types.EventToolCompleted, Round: round,
			ToolCallID: call.ID, ToolName: call.Name,
			Data: map[string]any{
				"success":    true,
				"durationMs": oc.Duration.Milliseconds(),
				"bytes":      len(oc.Result.Content),
				"result":     previewForUI(oc.Result.Content),
			},
		})
		conv.Closure.RecordToolCall(key, true, "")
		if total, done, ok := todoSnapshot(oc); ok {
			conv.Closure.RecordTodos(total, done)
		}
		conv.AppendToolResult(call.ID,
			sanitizeContent(truncatePlain(oc.Result.Content, l.cfg.MaxToolResultChars)))
		return detectsAllTodosDone(oc)
	}
}

// maxEventPreviewBytes bounds the tool output that may travel in an event.
//
// The design note fixes it at 2 KiB. It is a hard bound rather than a
// convenience: events are persisted and pushed over IPC, so an unbounded
// preview would let one verbose tool fill the event log.
const maxEventPreviewBytes = 2048

// previewForUI renders a tool output for an event payload: redacted first, then
// clamped on a rune boundary with an explicit "truncated" marker, because a
// silently shortened preview reads as a complete one.
func previewForUI(s string) string {
	s = types.RedactString(s)
	if len(s) <= maxEventPreviewBytes {
		return s
	}
	runes := []rune(s)
	// Cut on a rune boundary; the byte budget is the trigger, the rune count is
	// the cut, so a multi-byte character is never split in half.
	if len(runes) > maxEventPreviewBytes {
		runes = runes[:maxEventPreviewBytes]
	}
	return string(runes) + "…（已截断，共 " + itoa(len(s)) + " 字节）"
}

// todoSnapshot reads the todo_write counts out of a tool result, if it has any.
func todoSnapshot(oc ToolOutcome) (total, done int, ok bool) {
	if oc.Result.ToolName != "todo_write" && oc.Call.Name != "todo_write" {
		return 0, 0, false
	}
	if oc.Result.Metadata == nil {
		return 0, 0, false
	}
	total = intFromAny(oc.Result.Metadata["total"])
	if total <= 0 {
		return 0, 0, false
	}
	return total, intFromAny(oc.Result.Metadata["done"]), true
}

// reviewRound runs the ultra-mode quality gate, when one is configured.
func (l *Loop) reviewRound(ctx context.Context, in LoopInput, conv *Conversation, round int, outcomes []ToolOutcome) error {
	if l.reviewer == nil {
		return nil
	}
	snap := RoundSnapshot{Round: round, OriginalTask: conv.LastUser()}
	for _, oc := range outcomes {
		summary := oc.Result.Content
		if !oc.Result.Success {
			summary = firstNonEmpty(oc.Result.Error, oc.Result.Content, "(no output)")
		}
		snap.Tools = append(snap.Tools, ReviewSummary{
			Name:    oc.Call.Name,
			Success: oc.Result.Success,
			Summary: truncatePlain(summary, 300),
		})
	}

	review, err := l.reviewer.Review(ctx, snap)
	if err != nil {
		// A review failure must never break the run: the gate is advisory
		// infrastructure, and v1 explicitly tolerates its absence (the 45s
		// timeout resolves to "no verdict"). It is reported, not propagated.
		return l.emit(ctx, LoopEvent{
			Type: types.EventSupervision, Round: round,
			Message: "supervision review unavailable",
			Err:     err,
		})
	}
	if !review.Verdict.Valid() || review.Verdict == "" {
		return nil
	}
	review.Round = round
	conv.Reviews = append(conv.Reviews, review)
	// F2: the closure report's review check reads the newest verdict, so it is
	// recorded here — where the verdict becomes real — rather than re-derived
	// from the conversation later.
	conv.Closure.RecordReview(string(review.Verdict))

	data := map[string]any{
		"verdict":  string(review.Verdict),
		"issues":   review.Issues,
		"severity": review.Severity,
	}
	if review.Verdict.NeedsCorrection() {
		msg := review.CorrectionMessage
		if msg == "" {
			msg = DefaultCorrectionMessage(review)
		}
		data["correction"] = review.Correction
		data["message"] = msg
		// Appended after all tool results so the prefix stays append-only and
		// the provider's prompt cache is preserved.
		conv.AppendSystem(msg)
	}
	return l.emit(ctx, LoopEvent{Type: types.EventSupervision, Round: round, Data: data})
}

// DefaultCorrectionMessage renders the corrective system prompt for a review.
func DefaultCorrectionMessage(r Review) string {
	var b strings.Builder
	b.WriteString("监督审查发现问题（")
	b.WriteString(string(r.Verdict))
	b.WriteString("）：")
	if len(r.Issues) > 0 {
		b.WriteString(strings.Join(r.Issues, "；"))
	}
	if r.Correction != "" {
		b.WriteString("。纠正要求：")
		b.WriteString(r.Correction)
	}
	b.WriteString("。请立即按上述要求修正，不要重复已完成的工作。")
	return b.String()
}

// RoundSnapshot is what the reviewer inspects.
type RoundSnapshot struct {
	Round        int
	OriginalTask string
	Reasoning    string
	Content      string
	Tools        []ReviewSummary
}

// runPlanning runs the optional planning phase. It returns an error only when
// the run must stop: a cancellation, or a failed state transition.
func (l *Loop) runPlanning(ctx context.Context, in LoopInput, conv *Conversation) error {
	if err := l.checkCancelled(ctx); err != nil {
		return err
	}
	if l.planner == nil || !ShouldPlan(l.cfg, conv, ctx) {
		if l.planner != nil && !l.cfg.PlanningEnabled {
			_ = l.emit(ctx, LoopEvent{Type: types.EventPlanningSkipped,
				Message: "planning disabled by configuration"})
		}
		return nil
	}
	if err := l.transition(ctx, in, types.StatePlanning, "planning phase"); err != nil {
		return err
	}
	_ = l.emit(ctx, LoopEvent{Type: types.EventPlanningStarted,
		Data: map[string]any{"tools": len(conv.Tools)}})

	plan, err := l.planner.Plan(ctx, PlanRequest{
		Model:        in.Request.Model,
		SystemPrompt: in.Request.SystemPrompt,
		Tools:        conv.Tools,
		Task:         conv.LastUser(),
		Effort:       effortOr(in.Request.Effort),
	})
	if err != nil {
		// A planning cancellation is fatal to the run; any other failure is
		// tolerated, because planning is an optimisation.
		if types.IsCancelled(err) || ctx.Err() != nil {
			return types.WrapError(types.CodeCancelled, err, "planning round cancelled")
		}
		_ = l.emit(ctx, LoopEvent{
			Type: types.EventPlanningSkipped, Message: "planning failed", Err: err,
		})
	}
	if err != nil || len(plan.Tools) == 0 {
		// A failed plan keeps the full catalogue: planning is an optimisation,
		// and shrinking the tools to nothing would break the run.
		if err == nil {
			_ = l.emit(ctx, LoopEvent{
				Type:    types.EventPlanningSkipped,
				Message: "planning produced no usable tool selection",
			})
		}
		// Return to thinking so the main loop continues from a legal state.
		return l.transition(ctx, in, types.StateThinking, "planning skipped")
	}
	conv.Plan = &plan
	conv.Tools = plan.Tools
	conv.AppendAssistant(plan.Content, plan.Reasoning, nil,
		effortOr(in.Request.Effort).ThinkingEnabled())
	conv.AppendSystem(PlanAck)

	_ = l.emit(ctx, LoopEvent{
		Type: types.EventPlanningCompleted,
		Data: map[string]any{
			"selectedTools": plan.SelectedTools,
			"steps":         plan.Steps,
			"toolCount":     len(plan.Tools),
		},
	})
	return nil
}

// maybeCompact applies the compaction decision between rounds.
func (l *Loop) maybeCompact(ctx context.Context, in LoopInput, conv *Conversation, round int) error {
	d := l.compactor.Decide(conv)
	if d.Tier == types.TierNone {
		return nil
	}
	if d.NotifyOnly {
		// Soft tier: tell the UI, leave the prefix alone so the provider's
		// prompt cache survives.
		_ = l.emit(ctx, LoopEvent{
			Type: types.EventCompactionSkipped, Round: round,
			Message: d.Reason,
			Data:    map[string]any{"tier": string(d.Tier), "ratio": d.Ratio},
		})
		conv.SoftNoticed = true
		return nil
	}
	if !d.ShouldRun {
		return nil
	}

	if err := l.transition(ctx, in, types.StateCompacting, "context compaction"); err != nil {
		return err
	}
	_ = l.emit(ctx, LoopEvent{
		Type: types.EventCompactionStarted, Round: round,
		Message: d.Reason,
		Data: map[string]any{"tier": string(d.Tier), "ratio": d.Ratio,
			"promptTokens": conv.Usage.PromptTokens, "window": l.cfg.ContextWindow},
	})

	changed, err := l.compactor.Apply(ctx, conv)
	if err != nil {
		_ = l.emit(ctx, LoopEvent{Type: types.EventCompactionCompleted, Round: round, Err: err})
		return err
	}
	_ = l.emit(ctx, LoopEvent{
		Type: types.EventCompactionCompleted, Round: round,
		Data: map[string]any{
			"tier": string(d.Tier), "changed": changed,
			"promptTokens": conv.Usage.PromptTokens, "stuck": conv.CompactionStuck,
		},
	})
	// Return to thinking: compaction happens *within* a round boundary, so the
	// run continues in the phase it will use next.
	return l.transition(ctx, in, types.StateThinking, "compaction finished")
}

// forceWrapUp asks for a final answer with the tool catalogue withheld.
//
// reason distinguishes the two ways a run arrives here (F1/F2): "todos_done"
// means the model already completed its todo list and only owes a summary, so
// observe() has already appended the AllTodosDonePrompt and appending the
// generic wrap-up prompt on top would ask the same thing twice; every other
// reason means the budget ran out and the model has not been told to stop.
func (l *Loop) forceWrapUp(ctx context.Context, in LoopInput, conv *Conversation, res LoopResult, reason string) LoopResult {
	if err := l.checkCancelled(ctx); err != nil {
		return l.finishCancelled(ctx, in, conv, res)
	}
	if err := l.transition(ctx, in, types.StateThinking, "forced wrap-up"); err != nil {
		return l.finishFailed(ctx, in, conv, res, err)
	}
	if reason != types.ClosureReasonTodosDone {
		conv.Append(ports.Message{Role: ports.RoleUser, Content: WrapUpPrompt})
	}

	// Tools are withheld for the wrap-up round. The catalogue is restored
	// afterwards so a resume from this point is not silently tool-less.
	saved := conv.Tools
	conv.Tools = nil
	req := ports.ProviderRequest{
		// 同 think()：留空交给适配层用配置模型，不填占位名。
		Model:    in.Request.Model,
		Messages: conv.Messages,
		Tools:    nil,
		Effort:   effortOr(in.Request.Effort),
	}
	// 收尾轮是用户可见的最终答案生成轮，输出可能很长：与 think() 一样接上
	// OnDelta，让这一轮也保持流式观感，而不是"卡很久突然全出来"。
	req.OnDelta = func(d ports.Delta) {
		if d.Content == "" && d.Reasoning == "" {
			return
		}
		_ = l.emit(ctx, LoopEvent{
			Type: types.EventTokenDelta, Round: conv.Rounds,
			Data: map[string]any{"content": d.Content, "reasoning": d.Reasoning},
		})
	}
	req.OnUsage = func(u ports.Usage) {
		conv.Usage = mergeUsage(conv.Usage, u)
	}
	resp, err := l.provider.Complete(ctx, req)
	conv.Tools = saved

	if err != nil {
		if types.IsCancelled(err) {
			return l.finishCancelled(ctx, in, conv, res)
		}
		return l.finishFailed(ctx, in, conv, res,
			types.WrapError(types.CodeOf(err), err, "wrap-up round"))
	}
	conv.Usage = mergeUsage(conv.Usage, resp.Usage)
	conv.AppendAssistant(resp.Content, resp.ReasoningContent, nil,
		effortOr(in.Request.Effort).ThinkingEnabled())

	switch resp.FinishReason {
	case ports.FinishCancelled:
		return l.finishCancelled(ctx, in, conv, res)
	case ports.FinishError:
		return l.finishFailed(ctx, in, conv, res,
			types.NewError(types.CodeProviderFailed, "wrap-up round failed: %s", resp.Error))
	}

	res.Usage = conv.Usage
	res.Rounds = conv.Rounds
	res.ToolCalls = conv.toolCallCount

	// The wrap-up round is asked for text only, but a model can still answer
	// with another tool call (or with nothing at all). Falling back to the last
	// assistant text means the run ends with the best answer available rather
	// than with an empty one.
	answer := resp.Content
	if strings.TrimSpace(answer) == "" {
		answer = conv.LastAnswer
	}
	conv.LastAnswer = answer

	// F3 can also bite the wrap-up round: a truncated final answer is still
	// truncated, whatever round produced it.
	if resp.FinishReason == ports.FinishLength && conv.Closure.LengthContinues >= l.lengthContinueCap() {
		conv.Closure.Truncated = true
	}

	// F2: the wrap-up answer must be labelled honestly. When the reason is
	// budget exhaustion the run is partial by construction, and the closure
	// report says so instead of the state machine pretending otherwise.
	rep := types.BuildRunClosureReport(&conv.Closure, types.ClosureInput{
		Reason: reason, Answer: answer,
	})
	conv.lastClosure = rep

	data := map[string]any{
		"wrappedUp":    true,
		"finishReason": string(resp.FinishReason),
		"emptyContent": strings.TrimSpace(resp.Content) == "",
		"reason":       reason,
	}
	if rep.Incomplete {
		data["incomplete"] = true
	}
	if rep.Verdict != "" {
		data["verdict"] = rep.Verdict
	}
	if resp.Model != "" {
		data["model"] = resp.Model
	}

	// Emitted before the terminal transition, as on every other completion path.
	if err := l.emit(ctx, LoopEvent{
		Type: types.EventFinalAnswer, Round: conv.Rounds,
		Message: answer, Data: data,
	}); err != nil {
		return l.finishFailed(ctx, in, conv, res, err)
	}
	if _, err := l.emitClosureReport(ctx, conv, rep, reason); err != nil {
		return l.finishFailed(ctx, in, conv, res, err)
	}
	if err := l.transition(ctx, in, types.StateCompleted, "wrap-up answer"); err != nil {
		return l.finishFailed(ctx, in, conv, res, err)
	}
	res.State = types.StateCompleted
	res.Answer = answer
	res.Closure = rep
	return res
}

// lengthContinueCap returns the effective output-length continuation cap.
func (l *Loop) lengthContinueCap() int {
	if l.cfg.MaxLengthContinues > 0 {
		return l.cfg.MaxLengthContinues
	}
	return types.DefaultMaxLengthContinues
}

// finishCancelled records a cancelled terminal state.
func (l *Loop) finishCancelled(ctx context.Context, in LoopInput, conv *Conversation, res LoopResult) LoopResult {
	res.Rounds = conv.Rounds
	res.Usage = conv.Usage
	res.ToolCalls = conv.toolCallCount
	res.Err = types.NewError(types.CodeCancelled, "run cancelled")
	res.State = types.StateCancelled
	// Use a detached context: the run's context is already cancelled, but the
	// cancellation event must still reach the durable log.
	dctx := context.WithoutCancel(ctx)
	// The closure report is emitted for every terminal path, cancellation
	// included: "cancelled" is an honest verdict the UI must be able to show,
	// and a path that skipped the event would leave the badge empty.
	rep, cerr := l.emitClosure(dctx, conv, types.ClosureInput{
		Reason: types.ClosureReasonCancelled, Answer: conv.LastAnswer,
	})
	if cerr == nil {
		res.Closure = rep
		conv.lastClosure = rep
	}
	_ = l.transition(dctx, in, types.StateCancelled, "cancelled")
	_ = l.emit(dctx, LoopEvent{
		Type: types.EventCancellation, Round: conv.Rounds, Message: "run cancelled",
	})
	return res
}

// finishFailed records a failed terminal state.
func (l *Loop) finishFailed(ctx context.Context, in LoopInput, conv *Conversation, res LoopResult, err error) LoopResult {
	res.Rounds = conv.Rounds
	res.Usage = conv.Usage
	res.ToolCalls = conv.toolCallCount
	res.Err = err
	res.State = types.StateFailed
	// As with cancellation, the failure must be durably recorded even though
	// the run's context may be dead.
	dctx := context.WithoutCancel(ctx)
	// 错误事件先行：事件流在终态事件处关闭，真实原因必须先于 run.failed
	// 发出，否则流式订阅方永远只看到含糊的失败而看不到原因。
	_ = l.emitError(dctx, conv.Rounds, "", "", err)
	rep, cerr := l.emitClosure(dctx, conv, types.ClosureInput{
		Reason: types.ClosureReasonFailed, Answer: conv.LastAnswer,
	})
	if cerr == nil {
		res.Closure = rep
		conv.lastClosure = rep
	}
	_ = l.transition(dctx, in, types.StateFailed, types.RedactString(toString(err)))
	return res
}

// failedOutcome wraps a bookkeeping error as a failed round outcome.
func (l *Loop) failedOutcome(err error) (*roundOutcome, error) {
	return &roundOutcome{State: types.StateFailed, Err: err}, nil
}

// transition moves the machine, wrapping failures with the run's context.
func (l *Loop) transition(ctx context.Context, in LoopInput, to types.RunState, reason string) error {
	if err := in.Machine.Transition(ctx, to, reason); err != nil {
		// A terminal state is not an error for the loop's purposes: it means an
		// external cancellation already closed the run.
		if types.CodeOf(err) == types.CodeTerminalState {
			return err
		}
		return err
	}
	return nil
}

// transitionTo moves the machine with extra payload.
func (l *Loop) transitionTo(ctx context.Context, in LoopInput, to types.RunState, extra Transition) error {
	return in.Machine.TransitionTo(ctx, to, extra)
}

// emit sends a loop event to the sink.
func (l *Loop) emit(ctx context.Context, ev LoopEvent) error {
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	return l.sink.Emit(ctx, ev)
}

// emitError sends an error event.
func (l *Loop) emitError(ctx context.Context, round int, toolCallID, toolName string, err error) error {
	return l.emit(ctx, LoopEvent{
		Type: types.EventError, Round: round,
		ToolCallID: toolCallID, ToolName: toolName,
		Message: types.RedactString(toString(err)),
	})
}

// checkCancelled reports a cancellation as a coded error.
func (l *Loop) checkCancelled(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return types.WrapError(types.CodeCancelled, err, "run context cancelled")
	}
	return nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// detectsAllTodosDone reports whether a todo_write result says every task is
// complete, which is the loop's cue to stop and summarise.
func detectsAllTodosDone(oc ToolOutcome) bool {
	if oc.Result.ToolName != "todo_write" && oc.Call.Name != "todo_write" {
		return false
	}
	if !oc.Result.Success || oc.Result.Metadata == nil {
		return false
	}
	total := intFromAny(oc.Result.Metadata["total"])
	done := intFromAny(oc.Result.Metadata["done"])
	active := intFromAny(oc.Result.Metadata["active"])
	pending := intFromAny(oc.Result.Metadata["pending"])
	return total > 0 && done == total && active == 0 && pending == 0
}

// intFromAny coerces a metadata value to int. Metadata crosses the tool
// boundary as map[string]any, so JSON numbers arrive as float64.
func intFromAny(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int32:
		return int(n)
	case int64:
		return int(n)
	case float64:
		return int(n)
	case float32:
		return int(n)
	default:
		return 0
	}
}

// mergeUsage accumulates usage across rounds. Token counters add; the cached /
// missed split is kept from the newest observation because it describes the
// most recent request's cache behaviour rather than a running total.
func mergeUsage(acc, add ports.Usage) ports.Usage {
	if add.TotalTokens == 0 && add.PromptTokens == 0 && add.CompletionTokens == 0 {
		return acc
	}
	acc.PromptTokens += add.PromptTokens
	acc.CompletionTokens += add.CompletionTokens
	acc.TotalTokens += add.TotalTokens
	acc.ReasoningTokens += add.ReasoningTokens
	if add.CacheHitTokens > 0 || add.CacheMissTokens > 0 {
		acc.CacheHitTokens = add.CacheHitTokens
		acc.CacheMissTokens = add.CacheMissTokens
	}
	return acc
}

// truncate clamps a tool observation to the configured character budget,
// preferring the success content and falling back to the error text.
func truncate(raw, fallback string, max int) string {
	s := raw
	if s == "" {
		s = fallback
	}
	return truncatePlain(s, max)
}

// truncatePlain cuts s to max characters, appending a marker so the model can
// tell that output was elided rather than ending naturally.
func truncatePlain(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	const marker = "\n...[truncated]"
	if max <= len(marker) {
		return s[:max]
	}
	return s[:max-len(marker)] + marker
}

// sanitizeContent strips characters that break provider JSON parsing.
//
// Web-scraped content can contain control characters, lone surrogates and DEL,
// whose JSON escapes some providers cannot parse; v1 sanitises for the same
// reason.
func sanitizeContent(s string) string {
	if s == "" {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\n' || c == '\r' || c == '\t':
			b.WriteByte(c)
		case c < 0x20 || c == 0x7f:
			// Drop control characters.
		default:
			b.WriteByte(c)
		}
	}
	return strings.ToValidUTF8(b.String(), "\uFFFD")
}

// redactArgs renders tool arguments for an event payload with credential-like
// values masked, so an API key passed as a tool argument never reaches the log
// (doc ch. 21).
func redactArgs(args map[string]any) map[string]any {
	if len(args) == 0 {
		return nil
	}
	out := make(map[string]any, len(args))
	// Sort keys so the rendered payload is stable across runs, which keeps
	// event comparison in tests and in the UI deterministic.
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := args[k]
		if s, ok := v.(string); ok {
			out[k] = types.RedactString(s)
			continue
		}
		out[k] = v
	}
	return out
}

// redactErr renders an error for a model-visible observation.
func redactErr(err error) string {
	if err == nil {
		return ""
	}
	return types.RedactString(err.Error())
}

// firstNonEmpty returns the first non-empty string.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// effortOr defaults the reasoning effort.
func effortOr(e types.ReasoningEffort) types.ReasoningEffort {
	if e == "" {
		return types.EffortHigh
	}
	return e
}

// toString renders any panic value or error as text.
func toString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case error:
		return t.Error()
	case interface{ String() string }:
		return t.String()
	default:
		return strings.TrimSpace(sprint(v))
	}
}

// sprint renders an arbitrary value. It exists so a panic value of unexpected
// type is still reported verbatim rather than as an empty string.
func sprint(v any) string {
	return fmt.Sprint(v)
}
