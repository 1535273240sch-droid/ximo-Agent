package types

import (
	"sort"
	"strings"
)

// RunClosureReport is the deterministic closure verdict of a run. It answers a
// single question the UI must never guess at: *may this run honestly be called
// finished?*
//
// Design rules (from the audit's global constraints):
//
//   - The run state machine is untouched. "Did it finish cleanly" is expressed
//     here, not by a new state, so every existing transition table, snapshot and
//     test stays valid.
//   - Every check is deterministic: no model call, no network, no clock other
//     than the one already recorded. That is what makes it cheap enough to run
//     on every terminal path and testable without a fake provider.
//   - The report is emitted as the run.closure event exactly once per terminal
//     path and always *before* the terminal transition, because the event
//     stream closes at the terminal state.
type RunClosureReport struct {
	// Verdict is one of closed / partial / needs_user / failed.
	Verdict string `json:"verdict"`
	// Checks lists every check that applied, in a stable order, so the UI can
	// render "why" next to the badge.
	Checks []ClosureCheck `json:"checks"`
	// Incomplete is a convenience mirror of Verdict != "closed" for clients
	// that only want a boolean (it is also placed in the final_answer payload).
	Incomplete bool `json:"incomplete,omitempty"`
}

// ClosureCheck is one pass/fail line of the closure report.
type ClosureCheck struct {
	// ID is a stable machine name, e.g. answer_nonempty.
	ID string `json:"id"`
	// Label is the human-readable line shown in the UI.
	Label string `json:"label"`
	Pass  bool   `json:"pass"`
	// Note explains a failure (or adds detail on success).
	Note string `json:"note,omitempty"`
}

// Closure verdicts. They are the only values Verdict may take.
const (
	// ClosureClosed means every applicable check passed.
	ClosureClosed = "closed"
	// ClosurePartial means the run ended, but something is demonstrably not
	// finished: the round budget ran out, an answer was truncated, a todo list
	// was left incomplete, or a tool is still failing.
	ClosurePartial = "partial"
	// ClosureNeedsUser means the run is parked on a human decision (tool
	// permission, plan approval, crash-recovery adjudication).
	ClosureNeedsUser = "needs_user"
	// ClosureFailed means there is no usable answer at all.
	ClosureFailed = "failed"
)

// Closure check ids. They are referenced by the loop, the tests and the UI's
// "continue" button (which quotes the labels of the checks that failed), so
// they are named constants rather than free strings.
const (
	CheckAnswerNonEmpty     = "answer_nonempty"
	CheckAnswerNotTruncated = "answer_not_truncated"
	CheckTodosDone          = "todos_done"
	CheckToolErrorsResolved = "tool_errors_resolved"
	CheckBudgetOK           = "budget_ok"
	CheckReviewOK           = "review_ok"
	// CheckExpertsExecuted applies only to the expert paths (a hand-picked expert
	// or an Agent cluster). It is the one piece of evidence the main loop's
	// tracker cannot carry: how many of the dispatched experts really ran,
	// as opposed to degrading to "manual guidance" text. The report already
	// treated a degraded expert's fallback text as an answer (it is non-empty),
	// which is exactly the "looks finished but nothing ran" case the closure
	// report exists to catch.
	CheckExpertsExecuted = "experts_executed"
)

// Terminal closure reasons. They distinguish "the loop decided the task was
// done" from "the loop ran out of room".
const (
	// ClosureReasonCompleted is the ordinary finish path.
	ClosureReasonCompleted = "completed"
	// ClosureReasonTodosDone is the F1 path: todo_write reported everything
	// complete and the loop spent one final tool-less round summarising.
	ClosureReasonTodosDone = "todos_done"
	// ClosureReasonBudgetExhausted is the F2 path: the round budget ran out and
	// the loop forced a wrap-up answer.
	ClosureReasonBudgetExhausted = "budget_exhausted"
	// ClosureReasonTruncated is the F3 path: the model kept hitting the output
	// length limit until the continuation budget was spent.
	ClosureReasonTruncated = "truncated"
	// ClosureReasonEmpty is the F4 path: the model produced an empty answer
	// twice in a row.
	ClosureReasonEmpty = "empty_answer"
	// ClosureReasonCancelled is the user-cancel path.
	ClosureReasonCancelled = "cancelled"
	// ClosureReasonFailed is the failure path.
	ClosureReasonFailed = "failed"
	// ClosureReasonWaitingUser is the park path.
	ClosureReasonWaitingUser = "waiting_user"
)

// ClosureInput is everything BuildRunClosureReport needs that does not live in
// the tracker: the terminal reason and the final answer text.
type ClosureInput struct {
	// Reason is one of the ClosureReason* values.
	Reason string
	// Answer is the final answer text (for the answer checks).
	Answer string
	// WaitingUser marks a park, which is needs_user rather than partial.
	WaitingUser bool
	// Degraded counts experts that were dispatched but did not really run (their
	// content is fallback guidance). Zero means "not applicable" for paths that
	// dispatch no experts, so the check is omitted rather than reported as
	// passing — same rule as the todo/tool checks.
	Degraded int
}

// BuildRunClosureReport turns the tracker's accumulated evidence plus the
// terminal reason into the ordered report the UI renders.
//
// Check ordering is fixed (answer, truncation, todos, tools, budget, review) so
// the badge, the tests and a log diff all read the same way. A check whose
// evidence does not exist (no todo_write in this run, no reviewer configured)
// is omitted rather than reported as passing, because "not applicable" and
// "verified good" are different claims.
func BuildRunClosureReport(tr *ClosureTracker, in ClosureInput) RunClosureReport {
	rep := RunClosureReport{}

	// 1. answer_nonempty — always applies.
	answerOK := strings.TrimSpace(in.Answer) != ""
	empty := ClosureCheck{ID: CheckAnswerNonEmpty, Label: "答案非空", Pass: answerOK}
	if !answerOK {
		empty.Note = "本次 run 没有产生任何最终答案文本"
	}
	rep.Checks = append(rep.Checks, empty)

	// 2. answer_not_truncated — always applies.
	truncated := tr != nil && tr.Truncated
	trunc := ClosureCheck{ID: CheckAnswerNotTruncated, Label: "答案未被截断", Pass: !truncated}
	if truncated {
		trunc.Note = "模型连续触发输出长度上限，续写次数已用尽"
	}
	rep.Checks = append(rep.Checks, trunc)

	// 3. todos_done — only when this run actually used todo_write.
	if tr != nil && tr.TodoSeen {
		todo := ClosureCheck{
			ID: CheckTodosDone, Label: "待办全部完成", Pass: tr.TodoDone == tr.TodoTotal,
		}
		if tr.TodoDone != tr.TodoTotal {
			todo.Note = "最后一次 todo_write 快照：" +
				itoa(tr.TodoDone) + "/" + itoa(tr.TodoTotal) + " 已完成"
		}
		rep.Checks = append(rep.Checks, todo)
	}

	// 4. tool_errors_resolved — only when the run dispatched tools.
	if tr != nil && len(tr.ToolResults) > 0 {
		failing := tr.FailingToolKeys()
		tools := ClosureCheck{
			ID: CheckToolErrorsResolved, Label: "工具错误已解决",
			Pass: len(failing) == 0,
		}
		if len(failing) > 0 {
			tools.Note = "仍失败：" + strings.Join(failing, "、")
		}
		rep.Checks = append(rep.Checks, tools)
	}

	// 5. budget_ok — always applies; it is the check that keeps a
	// budget-exhausted wrap-up from masquerading as a clean finish.
	budgetOK := in.Reason != ClosureReasonBudgetExhausted
	budget := ClosureCheck{ID: CheckBudgetOK, Label: "未耗尽轮次预算", Pass: budgetOK}
	if !budgetOK {
		budget.Note = "轮次预算用尽，本轮由收尾轮强制产出答案"
	}
	rep.Checks = append(rep.Checks, budget)

	// 6. review_ok — only when a supervision review actually ran.
	if tr != nil && tr.ReviewSeen {
		review := ClosureCheck{
			ID: CheckReviewOK, Label: "复核通过", Pass: tr.ReviewVerdict == "on_track",
		}
		if !review.Pass {
			review.Note = "最后一次复核结论：" + tr.ReviewVerdict
		}
		rep.Checks = append(rep.Checks, review)
	}

	// 7. experts_executed — only for the expert paths that dispatched experts and
	// saw at least one degrade. Appended last on purpose: the order of the six
	// checks above is relied on by the loop, its tests and the UI, and this one
	// never applies to a main-loop run.
	if in.Degraded > 0 {
		rep.Checks = append(rep.Checks, ClosureCheck{
			ID:    CheckExpertsExecuted,
			Label: "专家全部真正执行",
			Pass:  false,
			Note: itoa(in.Degraded) +
				" 位专家未真正执行，其内容为降级指引（不代表专家结论）",
		})
	}

	rep.Verdict = verdictFor(rep.Checks, in)
	rep.Incomplete = rep.Verdict != ClosureClosed
	return rep
}

// verdictFor applies the documented precedence rules.
//
// Precedence matters: a parked run is needs_user even though the answer is
// empty (nothing was asked of the model yet), and a run whose answer is empty
// for any other reason is failed, not partial — there is nothing for the user
// to continue from.
func verdictFor(checks []ClosureCheck, in ClosureInput) string {
	pass := make(map[string]bool, len(checks))
	for _, c := range checks {
		pass[c.ID] = c.Pass
	}
	if in.WaitingUser {
		return ClosureNeedsUser
	}
	if !pass[CheckAnswerNonEmpty] {
		return ClosureFailed
	}
	if !pass[CheckBudgetOK] || !pass[CheckAnswerNotTruncated] {
		return ClosurePartial
	}
	if ok, present := pass[CheckTodosDone]; present && !ok {
		return ClosurePartial
	}
	if ok, present := pass[CheckToolErrorsResolved]; present && !ok {
		return ClosurePartial
	}
	if ok, present := pass[CheckReviewOK]; present && !ok {
		return ClosurePartial
	}
	// A run that dispatched experts but had some of them degrade is "partial",
	// not "closed": part of the requested work demonstrably did not happen. The
	// failed (not partial) case is already handled by answer_nonempty above,
	// because the expert paths report a degraded expert with an empty answer.
	if ok, present := pass[CheckExpertsExecuted]; present && !ok {
		return ClosurePartial
	}
	return ClosureClosed
}

// FailedChecks returns the labels of every failed check, in report order. The
// UI's "continue" button quotes these in the follow-up prompt so the user can
// see exactly what the next attempt has to fix.
func (r RunClosureReport) FailedChecks() []string {
	var out []string
	for _, c := range r.Checks {
		if !c.Pass {
			out = append(out, c.Label)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// tracker
// ---------------------------------------------------------------------------

// ClosureTracker accumulates the deterministic evidence the closure report is
// built from. It is folded into the conversation as the loop runs, so it is the
// loop's own memory of "where did the tool calls actually land", not a second
// source of truth derived from the event stream.
//
// It is intentionally a plain value with no clock and no I/O: every field is
// either observed directly from a tool outcome or set by the loop.
type ClosureTracker struct {
	// ToolResults holds the most recent success flag per tool-call key
	// (name + salient arguments), which is what makes "a later success
	// resolves an earlier failure for the same target" expressible.
	ToolResults map[string]bool `json:"toolResults,omitempty"`
	// ToolFailNotes holds the last error text per failing key, for the UI note.
	ToolFailNotes map[string]string `json:"toolFailNotes,omitempty"`
	// TodoSeen reports whether todo_write ran at all in this run.
	TodoSeen bool `json:"todoSeen,omitempty"`
	// TodoTotal and TodoDone are the newest todo_write snapshot.
	TodoTotal int `json:"todoTotal,omitempty"`
	TodoDone  int `json:"todoDone,omitempty"`
	// ReviewSeen reports whether a supervision verdict was recorded.
	ReviewSeen    bool   `json:"reviewSeen,omitempty"`
	ReviewVerdict string `json:"reviewVerdict,omitempty"`
	// Truncated latches when the model exhausted the output-length
	// continuation budget (F3).
	Truncated bool `json:"truncated,omitempty"`
	// LengthContinues counts consecutive finish_reason=length rounds already
	// spent on this run. It is the input to the continuation cap.
	LengthContinues int `json:"lengthContinues,omitempty"`
}

// RecordToolCall folds one tool outcome into the tracker.
//
// Lazy initialisation (rather than a constructor) keeps the zero value usable,
// which matters because a Conversation may be rebuilt by recovery without ever
// having seen a tool call.
func (t *ClosureTracker) RecordToolCall(key string, success bool, errText string) {
	if key == "" {
		return
	}
	if t.ToolResults == nil {
		t.ToolResults = make(map[string]bool, 8)
	}
	t.ToolResults[key] = success
	if !success {
		if t.ToolFailNotes == nil {
			t.ToolFailNotes = make(map[string]string, 4)
		}
		if s, ok := redactAndClamp(errText, 200); ok {
			t.ToolFailNotes[key] = s
		}
		return
	}
	// A success clears the note: the failure it described is no longer the
	// last thing that happened to this target.
	delete(t.ToolFailNotes, key)
}

// RecordTodos records a todo_write snapshot.
func (t *ClosureTracker) RecordTodos(total, done int) {
	t.TodoSeen = true
	t.TodoTotal = total
	t.TodoDone = done
}

// RecordReview records the newest supervision verdict.
func (t *ClosureTracker) RecordReview(verdict string) {
	t.ReviewSeen = true
	t.ReviewVerdict = verdict
}

// FailingToolKeys returns the sorted keys whose most recent call failed.
func (t *ClosureTracker) FailingToolKeys() []string {
	if t == nil || len(t.ToolResults) == 0 {
		return nil
	}
	var out []string
	for k, ok := range t.ToolResults {
		if !ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// Clone copies the tracker so a Conversation clone does not share mutable maps.
func (t *ClosureTracker) Clone() *ClosureTracker {
	if t == nil {
		return nil
	}
	cp := &ClosureTracker{
		TodoSeen:        t.TodoSeen,
		TodoTotal:       t.TodoTotal,
		TodoDone:        t.TodoDone,
		ReviewSeen:      t.ReviewSeen,
		ReviewVerdict:   t.ReviewVerdict,
		Truncated:       t.Truncated,
		LengthContinues: t.LengthContinues,
	}
	if t.ToolResults != nil {
		cp.ToolResults = make(map[string]bool, len(t.ToolResults))
		for k, v := range t.ToolResults {
			cp.ToolResults[k] = v
		}
	}
	if t.ToolFailNotes != nil {
		cp.ToolFailNotes = make(map[string]string, len(t.ToolFailNotes))
		for k, v := range t.ToolFailNotes {
			cp.ToolFailNotes[k] = v
		}
	}
	return cp
}

// ToolCallKey derives the identity a closure check aggregates on: the tool name
// plus the arguments that make two calls "the same work on the same target".
//
// Why not the call ID: a model that hits a failing file_edit and then retries it
// produces two different call IDs, and the retry succeeding is exactly the
// "resolved" case the check must recognise. Why not every argument either: a
// timestamp or a page number in the arguments would make every retry a new key
// and the check would report a permanent failure.
//
// The salient keys are deliberately a small, explicit, ordered list. Ordering
// keeps the key stable (a map iteration order would not be), and the fallback to
// the tool name alone keeps this total for tools with none of them.
func ToolCallKey(name string, args map[string]any) string {
	if name == "" {
		return ""
	}
	keys := []string{"path", "file", "file_path", "command", "cmd", "url", "query"}
	var parts []string
	for _, k := range keys {
		raw, ok := args[k]
		if !ok {
			continue
		}
		s, ok := raw.(string)
		if !ok || strings.TrimSpace(s) == "" {
			continue
		}
		// A command's first word identifies the target far better than the
		// whole line, which usually carries volatile arguments.
		if k == "command" || k == "cmd" {
			if fields := strings.Fields(s); len(fields) > 0 {
				s = fields[0]
			}
		}
		redacted, _ := redactAndClamp(s, 120)
		parts = append(parts, k+"="+redacted)
	}
	if len(parts) == 0 {
		return name
	}
	return name + "(" + strings.Join(parts, ",") + ")"
}

// redactAndClamp applies the mandatory redaction and then a rune-safe clamp, so
// a key or a note can never carry a credential and never grows without bound.
func redactAndClamp(s string, max int) (string, bool) {
	s = RedactString(s)
	if s == "" {
		return "", false
	}
	if max <= 0 {
		return s, true
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s, true
	}
	return string(runes[:max]) + "…", true
}

// itoa is a tiny local integer formatter. It exists to keep this file free of a
// strconv import for two call sites, and matches the loop's own helper style.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
