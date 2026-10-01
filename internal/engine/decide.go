package engine

import (
	"context"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/agent"
	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// This file is the engine half of F5 — the tool-authorization closed loop.
//
// The shape of the loop: a run reaches a tool call the permission layer refuses
// to run unattended, the agent loop parks the run in waiting_user and reports
// the call IDs it parked on, the engine records them here, the user answers
// through engine.run.decide (types.ToolDecider), and Resume carries the answer
// into the next invocation of the loop.
//
// Why this is not ConfirmPlan: both RPCs resume a run parked in waiting_user,
// but they authorize different objects. ConfirmPlan authorizes a *plan* the user
// read; Decide authorizes *one specific tool call*. waiting_user is also how a
// crash-recovery adjudication parks a run. Treating any of those as the others
// would let one approval silently authorize something the user never saw, which
// is why both entry points validate their own cause-of-park marker (planPending
// for plans, pendingToolCalls here) instead of trusting the state alone.
//
// Crash-recovery stance for the state introduced here (acceptance item 7):
//
//   - pendingToolCalls is rebuilt from the durable log by
//     pendingToolCallsFromLog, exactly as pendingPlanFromLog rebuilds a plan for
//     plan mode. An Engine restart therefore does not make a parked run
//     unanswerable: the in-memory flag is a cache, not the authority.
//   - toolDecisions is deliberately NOT rebuilt. It is a one-shot permit; after
//     a restart the user's click is gone with the process, and re-asking costs
//     one click while resurrecting an approval the user cannot see in context
//     would be a silent privilege grant.
//   - sessionApproved is deliberately NOT rebuilt either. "Remember for this
//     session" describes a live UI session; a fresh process cannot prove the
//     same session is still the one asking, so it fails closed.

// Metadata keys the permission layer uses to hand a call back for a human
// decision. types.ToolResult has no dedicated field for this and is a frozen
// contract, so the two facts travel in Metadata — the same two facts
// internal/tool already models as ToolResponse.RequiresConfirmation /
// ConfirmationMessage.
const (
	metaRequiresConfirmation = "requiresConfirmation"
	metaConfirmationMessage  = "confirmationMessage"
)

// rememberSession is the Decide payload's opt-in to a session-scoped approval.
const rememberSession = "session"

// Decide implements types.ToolDecider: it records the user's answer for one
// parked tool call and resumes the run when the answer unblocks it.
//
// Inputs are validated before anything is mutated, so a refused decision leaves
// the run exactly as it was (and therefore retryable):
//
//   - unknown/archived run                    -> CodeTerminalState
//   - run is not parked in waiting_user       -> CodeInvalidTransition
//   - callID is not in the pending set        -> CodeInvalidArgument
func (e *Engine) Decide(ctx context.Context, runID, callID string, approve bool, remember string) error {
	rec, archived, err := e.liveRun(ctx, runID)
	if err != nil {
		return err
	}
	if rec == nil {
		return types.NewError(types.CodeTerminalState,
			"run %s is %s and cannot accept tool decisions", runID, string(archived.State))
	}
	if callID == "" {
		return types.NewError(types.CodeInvalidArgument,
			"call_id is required to decide a tool call")
	}

	// The state check is the first half of the privilege boundary: a run that is
	// not parked is not waiting for an authorization, whatever else it is doing.
	//
	// rec.state() is the fast path, but it is the engine's cached copy. One path
	// updates the state without going through the record: crash recovery
	// rehydrates a record and parks it through the actor, so the cached copy can
	// still read the pre-restart state. Asking the actor (the single writer, and
	// the same authority Resume consults) before refusing keeps a legitimately
	// parked authorization answerable after a restart.
	if s := rec.state(); s != types.StateWaitingUser {
		authoritative, gerr := e.GetRun(ctx, runID)
		if gerr != nil || authoritative.State != types.StateWaitingUser {
			return types.NewError(types.CodeInvalidTransition,
				"run %s is %s and has no tool authorization pending", runID, string(s))
		}
	}

	// An empty in-memory set does not prove there is nothing to authorize: the
	// park and its cause are both durable, so an Engine restart (or a run
	// rehydrated by recovery) loses only the map while the request is still on
	// the log. Rebuilding here is what keeps a parked authorization answerable
	// after a restart — the same reasoning as ConfirmPlan's pendingPlanFromLog.
	if rec.pendingToolCallCount() == 0 {
		if rebuilt := e.pendingToolCallsFromLog(ctx, runID); len(rebuilt) > 0 {
			rec.mergePendingToolCalls(rebuilt)
		}
	}

	// The second half of the privilege boundary: the call must be one this run
	// actually parked on, and it must have a tool name (an unnamed call cannot be
	// authorized meaningfully). A plan confirmation or a crash-recovery
	// adjudication has no entry here and is therefore refused rather than
	// resumed on this decision's authority.
	toolName, claimed := rec.claimToolCall(callID)
	if !claimed {
		return types.NewError(types.CodeInvalidArgument,
			"run %s has no tool call %s awaiting authorization", runID, callID)
	}

	rec.recordToolDecision(callID, toolName, approve)
	if approve && remember == rememberSession {
		rec.rememberSessionTool(toolName)
	}

	// The decision is recorded durably before it is acted on, so the audit trail
	// exists even if the resume fails (same ordering as ConfirmPlan).
	e.recordToolDecisionEvent(ctx, rec, callID, toolName, approve, remember)

	// Reuse the existing park/resume machinery rather than starting a second
	// execution path: it already owns the single-writer claim (I1), the
	// cancellation bookkeeping and the thinking transition a parked run needs.
	if err := e.Resume(ctx, runID); err != nil {
		// The resume failed, so the decision will never be consumed. Put the park
		// back, so the user can retry instead of being left with a run that looks
		// answerable but no longer is.
		rec.rollbackToolDecision(callID, toolName, approve && remember == rememberSession)
		return err
	}
	return nil
}

// recordToolDecisionEvent durably records the user's answer to one tool prompt.
//
// It reuses existing event types rather than adding one: the contract may only
// be appended, and both facts it carries already have a home.
//
//   - approve=false resolves the call in the log (the loop will refuse it), so
//     it is recorded as tool_call.cancelled — the same event the loop itself
//     emits when it observes the refusal. That also makes the refusal visible to
//     the log-based pending-set rebuild, so a second decision for the same call
//     is refused even after `pendingToolCalls` was lost.
//   - approve=true is recorded as user_input.received, the same event
//     recordResumeApproval already uses for "the user authorized a parked call".
//     A tool_call.started would be a lie — the call has not started, and writing
//     it would also make crash recovery believe a non-idempotent call had begun
//     and must not be replayed.
//
// It is best-effort by design: the decision has already been taken and the run
// is about to be resumed, so a failure to log must not strand it.
func (e *Engine) recordToolDecisionEvent(ctx context.Context, rec *runRecord, callID, toolName string, approve bool, remember string) {
	data := map[string]any{
		"decide":   approve,
		"callId":   callID,
		"toolName": toolName,
	}
	if remember != "" {
		data["remember"] = remember
	}

	ev := types.Event{
		RunID: rec.runID(), SessionID: rec.sessionID(),
		Type: types.EventUserInputReceived, Timestamp: time.Now(),
		Round:      rec.currentRound(),
		ToolCallID: callID, ToolName: toolName,
		Message: types.RedactString("用户" + approvedWord(approve) + "执行工具 " + toolName),
		Data:    data,
	}
	if !approve {
		// A refusal ends this call's life, which is exactly what
		// tool_call.cancelled means; the loop emits the same event when it feeds
		// the refusal back to the model.
		ev.Type = types.EventToolCancelled
	}
	if err := e.appendEventBestEffort(ctx, ev); err != nil {
		return
	}
	e.coalescer.Publish(ev)
}

// approvedWord renders the two answers for the audit message.
func approvedWord(approve bool) string {
	if approve {
		return "批准"
	}
	return "拒绝"
}

// pendingToolCallsFromLog reconstructs the set of tool calls a parked run is
// waiting on.
//
// A call is pending when the permission layer asked about it
// (tool_call.permission_required) and nothing has resolved it since: no
// tool_call.completed / failed / cancelled. A later permission prompt for the
// same call ID re-opens it (the loop parked on it again after a new round).
//
// The rebuild is what makes the in-memory pending set a cache rather than the
// authority: losing it (a restart, a rehydrated run record) must not turn a
// still-answerable authorization into an unanswerable one.
func (e *Engine) pendingToolCallsFromLog(ctx context.Context, runID string) map[string]string {
	if e.deps.Events == nil {
		return nil
	}
	events, err := e.deps.Events.Read(ctx, runID, 0, 0)
	if err != nil {
		return nil
	}

	type entry struct {
		toolName string
		resolved bool
	}
	var order []string
	seen := make(map[string]*entry)
	for _, ev := range events {
		switch ev.Type {
		case types.EventToolPermissionRequired:
			if ev.ToolCallID == "" {
				continue
			}
			en, ok := seen[ev.ToolCallID]
			if !ok {
				en = &entry{}
				seen[ev.ToolCallID] = en
				order = append(order, ev.ToolCallID)
			}
			if ev.ToolName != "" {
				en.toolName = ev.ToolName
			}
			en.resolved = false
		case types.EventToolCompleted, types.EventToolFailed, types.EventToolCancelled:
			if en, ok := seen[ev.ToolCallID]; ok {
				en.resolved = true
			}
		}
	}

	out := make(map[string]string)
	for _, id := range order {
		en := seen[id]
		if en.resolved || en.toolName == "" {
			continue
		}
		out[id] = en.toolName
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ---------------------------------------------------------------------------
// runRecord state (guarded by runRecord.mu, like the plan-mode fields)
// ---------------------------------------------------------------------------

// pendingToolCallCount reports how many calls are waiting for an answer.
func (r *runRecord) pendingToolCallCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.pendingToolCalls)
}

// setPendingToolCalls replaces the waiting set with the calls a park reported.
//
// A call with no name is stored with an empty name on purpose: Decide refuses
// unnamed calls, so an unlabelled prompt stays parked instead of being
// authorized by a decision that cannot say what it allowed.
func (r *runRecord) setPendingToolCalls(ids []string, names map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pendingToolCalls = make(map[string]string, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		r.pendingToolCalls[id] = names[id]
	}
}

// mergePendingToolCalls adds rebuilt entries without dropping what memory
// already holds (the log may lag the live call).
func (r *runRecord) mergePendingToolCalls(rebuilt map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pendingToolCalls == nil {
		r.pendingToolCalls = make(map[string]string, len(rebuilt))
	}
	for id, name := range rebuilt {
		if cur, ok := r.pendingToolCalls[id]; ok && cur != "" {
			continue
		}
		r.pendingToolCalls[id] = name
	}
}

// clearPendingToolCalls drops the waiting set. It is called once the set has
// been consumed (resume) or the run is no longer parked, so a stale entry can
// never authorize a call that already ran.
func (r *runRecord) clearPendingToolCalls() {
	r.mu.Lock()
	r.pendingToolCalls = nil
	r.mu.Unlock()
}

// claimToolCall removes one call from the waiting set and returns its tool name.
// The check-and-remove happens under a single lock so two concurrent decisions
// for the same call cannot both win and queue two resumes.
func (r *runRecord) claimToolCall(callID string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	name, ok := r.pendingToolCalls[callID]
	if !ok || name == "" {
		return "", false
	}
	delete(r.pendingToolCalls, callID)
	return name, true
}

// recordToolDecision stores the user's answer for the next executeRun to
// consume. Two keys are written for one answer:
//
//   - the call ID, so the exact call the user was shown is answered; and
//   - the tool-name key (agent.ToolNameDecisionKey), because a resumed run does
//     not replay the parked call: the loop re-asks the model, which re-issues
//     the call with a new ID. Without the name key the approval would be spent
//     on an ID that never comes back and the run would park again immediately.
//
// Both live in the same map, which executeRun consumes and clears, so the answer
// is still spent by exactly one invocation.
func (r *runRecord) recordToolDecision(callID, toolName string, approve bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.toolDecisions == nil {
		r.toolDecisions = make(map[string]bool)
	}
	r.toolDecisions[callID] = approve
	if toolName != "" {
		r.toolDecisions[agent.ToolNameDecisionKey(toolName)] = approve
	}
}

// rollbackToolDecision undoes a decision whose resume failed, so the park looks
// untouched and the user can retry.
func (r *runRecord) rollbackToolDecision(callID, toolName string, session bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.toolDecisions, callID)
	delete(r.toolDecisions, agent.ToolNameDecisionKey(toolName))
	if r.pendingToolCalls == nil {
		r.pendingToolCalls = make(map[string]string)
	}
	r.pendingToolCalls[callID] = toolName
	if session {
		delete(r.sessionApproved, toolName)
	}
}

// takeToolDecisions returns the pending answers and clears them: an approval is
// spent by exactly one execution attempt.
func (r *runRecord) takeToolDecisions() map[string]bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.toolDecisions
	r.toolDecisions = nil
	if out == nil {
		out = make(map[string]bool)
	}
	return out
}

// rememberSessionTool records that the user allowed this tool for the rest of
// the session, so the calls of the same tool that are still parked in this
// batch are released with them.
func (r *runRecord) rememberSessionTool(toolName string) {
	if toolName == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sessionApproved == nil {
		r.sessionApproved = make(map[string]bool)
	}
	r.sessionApproved[toolName] = true
}

// applySessionApprovals adds a name-keyed allow answer for every tool the user
// approved for this session.
//
// The name key is what makes "remember for this session" mean anything: a call
// the model re-issues after a resume, or issues in a later round, carries a new
// call ID, so only a tool-name answer can release it. The answers are re-applied
// to every invocation of the run while the record is alive; they are deliberately
// not rebuilt after a restart (see the file header).
//
// Known limitation, reported rather than hidden: the loop folds every answer
// into the conversation's own decision map and never removes it, so inside one
// run a one-shot answer also releases later calls of the same tool. "once" and
// "session" therefore differ only in the engine's bookkeeping today; the
// conversation-side retention is owned by internal/agent.
func (r *runRecord) applySessionApprovals(into map[string]bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.sessionApproved) == 0 {
		return
	}
	for toolName := range r.sessionApproved {
		if toolName == "" {
			continue
		}
		into[agent.ToolNameDecisionKey(toolName)] = true
	}
	// The calls already parked in this batch are released by call ID as well, so
	// the answer applies whichever key the loop looks up first.
	for callID, toolName := range r.pendingToolCalls {
		if toolName == "" || !r.sessionApproved[toolName] {
			continue
		}
		if _, decided := into[callID]; !decided {
			into[callID] = true
		}
	}
}

// batchApproval reports whether a call about to be queued carries the user's
// permission approval, and whether that approval was session-scoped (F5).
//
// It is read at dispatch time rather than threaded through the scheduler's task
// type because the answer is consumed once per execution attempt and never
// travels with a retry: an approval belongs to this attempt.
func (r *runRecord) batchApproval(call types.ToolCall) (approved, forSession bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if call.ID != "" && r.toolDecisions[call.ID] {
		// A call-ID answer authorizes exactly this call.
		return true, false
	}
	if call.Name != "" && r.sessionApproved[call.Name] {
		return true, true
	}
	return false, false
}

// clearToolState drops every F5 map. It is called when the run reaches a
// terminal state (nothing can be authorized any more) and on engine shutdown.
func (r *runRecord) clearToolState() {
	r.mu.Lock()
	r.pendingToolCalls = nil
	r.toolDecisions = nil
	r.sessionApproved = nil
	r.mu.Unlock()
}

// toolOutcomeFor maps one dispatch result onto the loop's outcome, including the
// permission hand-off.
//
// The confirmation signal travels in ToolResult.Metadata: the frozen
// types.ToolResult has no dedicated field, and the permission layer
// (internal/tool) already models the same two facts as
// ToolResponse.RequiresConfirmation / ConfirmationMessage. Reading them out
// here is what turns "the permission layer wants a human" into the loop's
// park-and-ask path; without it a flagged call would be fed to the model as a
// plain failure and the user would never be asked.
func toolOutcomeFor(call types.ToolCall, res types.ToolResult, err error) (needsConfirmation bool, message string) {
	if err != nil || res.Success {
		// A successful result is a result: parking on it would hide the tool's
		// output from the model for no reason.
		return false, ""
	}
	if !metadataBool(res.Metadata, metaRequiresConfirmation) {
		return false, ""
	}
	msg := metadataString(res.Metadata, metaConfirmationMessage)
	if msg == "" {
		msg = "工具 " + call.Name + " 需要用户授权后才能执行"
	}
	return true, types.RedactString(msg)
}

// metadataBool reads a boolean flag from a tool result's metadata.
func metadataBool(md map[string]any, key string) bool {
	if md == nil {
		return false
	}
	switch v := md[key].(type) {
	case bool:
		return v
	case string:
		return v == "true"
	default:
		return false
	}
}

// metadataString reads a string field from a tool result's metadata.
func metadataString(md map[string]any, key string) string {
	if md == nil {
		return ""
	}
	s, _ := md[key].(string)
	return s
}

// Compile-time assertion that the concrete engine satisfies the F5 contract
// declared in internal/types, so a signature drift breaks the build here rather
// than in the IPC layer.
var _ types.ToolDecider = (*Engine)(nil)
