package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/ximo888ok-netizen/ximo-agent/internal/ports"
	"github.com/ximo888ok-netizen/ximo-agent/internal/ports/mem"
	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// This file holds the acceptance tests for the audit's closure fixes (F1-F4,
// section 3.5). They live next to the loop rather than in a separate package
// because they need the loop's own harness, and they assert on both the emitted
// events and the returned LoopResult so a regression in the ordering
// (answer -> closure -> terminal state) fails loudly.

// runWith drives the loop with a fully specified input, so a test can hand it
// permission decisions the way the engine does.
func (h *loopHarness) runWith(t *testing.T, req types.SubmitRequest, tools []types.ToolDefinition, mutate func(*LoopInput)) (LoopResult, *Machine, *Conversation) {
	t.Helper()
	conv := NewConversation(req.SystemPrompt, req.Prompt, tools)
	machine := NewMachine("run-test", "ses-test", TransitionSinkFunc(func(_ context.Context, _ Transition) error {
		return nil
	}))
	in := LoopInput{
		RunID: "run-test", SessionID: "ses-test",
		Request: req, Conversation: conv, Machine: machine,
	}
	if mutate != nil {
		mutate(&in)
	}
	return h.loop.Run(context.Background(), in), machine, conv
}

// closureOf returns the closure report carried by the run.closure event.
func (s *recordingSink) closureOf(t *testing.T) map[string]any {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.events {
		if e.Type == types.EventRunClosure {
			return e.Data
		}
	}
	emitted := make([]types.EventType, 0, len(s.events))
	for _, e := range s.events {
		emitted = append(emitted, e.Type)
	}
	t.Fatalf("no %s event was emitted; emitted %v", types.EventRunClosure, emitted)
	return nil
}

// typesLocked is types() for a caller that already holds the lock.
func (s *recordingSink) typesLocked() []types.EventType {
	out := make([]types.EventType, 0, len(s.events))
	for _, e := range s.events {
		out = append(out, e.Type)
	}
	return out
}

// TestClosureExactlyOnceOnEveryTerminalPath pins the "events precede the
// terminal transition, and run.closure happens exactly once" rule.
//
// The rule is what makes the closure badge trustworthy: a path that emitted the
// report after the terminal state would have it dropped, because the event
// stream closes there.
func TestClosureExactlyOnceOnEveryTerminalPath(t *testing.T) {
	cases := []struct {
		name    string
		script  []ports.ProviderResponse
		want    types.RunState
		closure string
	}{
		{
			name:    "ordinary final answer",
			script:  []ports.ProviderResponse{mem.FinalRound("final answer text")},
			want:    types.StateCompleted,
			closure: types.ClosureClosed,
		},
		{
			name: "budget exhausted",
			script: func() []ports.ProviderResponse {
				// The per-segment budget is MaxToolRounds (set to 2 below), so
				// two tool-call rounds exhaust it and the third entry answers
				// the wrap-up round.
				return []ports.ProviderResponse{
					mem.ToolCallRound(mem.NewCall("c1", "file_read", map[string]any{"path": "a"})),
					mem.ToolCallRound(mem.NewCall("c2", "file_read", map[string]any{"path": "b"})),
					mem.FinalRound("forced wrap-up answer"),
				}
			}(),
			want:    types.StateCompleted,
			closure: types.ClosurePartial,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := types.DefaultAgentConfig()
			cfg.MaxToolRounds = 2
			p := mem.NewProvider(tc.script...)
			h := newLoopHarness(t, cfg, p, nil)

			res, _, _ := h.run(t, types.SubmitRequest{Prompt: "task"},
				[]types.ToolDefinition{{Name: "file_read"}})

			if res.State != tc.want {
				t.Fatalf("state = %s, want %s (err=%v)", res.State, tc.want, res.Err)
			}
			if n := h.sink.count(types.EventRunClosure); n != 1 {
				t.Fatalf("run.closure emitted %d times, want exactly 1; events=%v", n, h.sink.types())
			}
			data := h.sink.closureOf(t)
			if got := data["verdict"]; got != tc.closure {
				t.Errorf("verdict = %v, want %v (data=%v)", got, tc.closure, data)
			}
			// The closure report must come after final_answer and before the
			// terminal state change.
			emitted := h.sink.types()
			closureAt, answerAt, terminalAt := -1, -1, -1
			for i, tp := range emitted {
				switch tp {
				case types.EventRunClosure:
					if closureAt < 0 {
						closureAt = i
					}
				case types.EventFinalAnswer:
					answerAt = i
				case types.EventRunCompleted:
					terminalAt = i
				}
			}
			if answerAt >= 0 && closureAt >= 0 && closureAt < answerAt {
				t.Errorf("run.closure at %d precedes final_answer at %d; the answer must come first so the "+
					"payload can carry the verdict", closureAt, answerAt)
			}
			if closureAt >= 0 && terminalAt >= 0 && closureAt > terminalAt {
				t.Errorf("run.closure at %d comes after the terminal event at %d and would never reach a "+
					"subscriber", closureAt, terminalAt)
			}
		})
	}
}

// TestTodosDoneSpendsASummaryRound is the C1 regression test: the todo-done path
// must produce a fresh, tool-less summary round, and the answer must be that
// round's text rather than the stale tool-round text.
func TestTodosDoneSpendsASummaryRound(t *testing.T) {
	cfg := types.DefaultAgentConfig()
	// Round 1 asks for todo_write (which reports everything done); round 2 is
	// the summary the fix requires.
	p := mem.NewProvider(
		mem.ToolCallRound(mem.NewCall("t1", "todo_write", nil)),
		mem.FinalRound("summary from the extra round"),
	)
	runtime := mem.NewToolRuntime(func(_ context.Context, _ ports.ToolRequest) (types.ToolResult, error) {
		return types.ToolResult{
			Success: true, Content: "todos updated",
			Metadata: map[string]any{"total": 2, "done": 2, "active": 0, "pending": 0},
		}, nil
	})
	h := newLoopHarness(t, cfg, p, runtime)

	res, _, _ := h.run(t, types.SubmitRequest{Prompt: "task"},
		[]types.ToolDefinition{{Name: "todo_write"}})

	if res.Answer != "summary from the extra round" {
		t.Fatalf("answer = %q, want the summary round's text (C1: the run used to finish on stale text)", res.Answer)
	}
	if calls := h.provider.Calls; len(calls) != 2 {
		t.Fatalf("provider calls = %d, want 2 (tool round + summary round)", len(calls))
	} else if len(calls[1].Tools) != 0 {
		t.Errorf("the summary round was offered %d tools, want 0", len(calls[1].Tools))
	}
	if res.Closure.Verdict != types.ClosureClosed {
		t.Errorf("verdict = %q, want closed (checks=%+v)", res.Closure.Verdict, res.Closure.Checks)
	}
	if !hasCheck(res.Closure, types.CheckTodosDone, true) {
		t.Errorf("todos_done check missing or failing: %+v", res.Closure.Checks)
	}
}

// TestBudgetExhaustionIsMarkedPartial is the C2 regression test: a run that ran
// out of rounds must not be indistinguishable from one that finished.
func TestBudgetExhaustionIsMarkedPartial(t *testing.T) {
	cfg := types.DefaultAgentConfig()
	cfg.MaxToolRounds = 2
	p := mem.NewProvider(
		mem.ToolCallRound(mem.NewCall("c1", "file_read", map[string]any{"path": "a"})),
		mem.ToolCallRound(mem.NewCall("c2", "file_read", map[string]any{"path": "b"})),
		mem.FinalRound("forced wrap-up answer"),
	)
	h := newLoopHarness(t, cfg, p, nil)

	res, _, _ := h.run(t, types.SubmitRequest{Prompt: "task"},
		[]types.ToolDefinition{{Name: "file_read"}})

	if res.State != types.StateCompleted {
		t.Fatalf("state = %s, want completed: the state table must not change (err=%v)", res.State, res.Err)
	}
	if res.Closure.Verdict != types.ClosurePartial {
		t.Fatalf("verdict = %q, want partial: a budget-exhausted wrap-up must not look finished (checks=%+v)",
			res.Closure.Verdict, res.Closure.Checks)
	}
	if !hasCheck(res.Closure, types.CheckBudgetOK, false) {
		t.Errorf("budget_ok check must fail on this path: %+v", res.Closure.Checks)
	}
	// The final_answer payload must say the same thing as the closure report,
	// so a client that reads only one of the two cannot be misled.
	var incomplete any
	h.sink.mu.Lock()
	for _, e := range h.sink.events {
		if e.Type == types.EventFinalAnswer {
			incomplete = e.Data["incomplete"]
		}
	}
	h.sink.mu.Unlock()
	if incomplete != true {
		t.Errorf("final_answer.Data[incomplete] = %v, want true", incomplete)
	}
}

// TestLengthTruncationContinuesThenMarksPartial is the C3 regression test: a
// truncated answer is continued up to the cap, and only then accepted as
// partial.
func TestLengthTruncationContinuesThenMarksPartial(t *testing.T) {
	cfg := types.DefaultAgentConfig()
	cfg.MaxLengthContinues = 2
	p := mem.NewProvider(
		ports.ProviderResponse{FinishReason: ports.FinishLength, Content: "half one", Emitted: true},
		ports.ProviderResponse{FinishReason: ports.FinishLength, Content: "half two", Emitted: true},
		ports.ProviderResponse{FinishReason: ports.FinishLength, Content: "half three", Emitted: true},
		ports.ProviderResponse{FinishReason: ports.FinishStop, Content: "never reached", Emitted: true},
	)
	h := newLoopHarness(t, cfg, p, nil)

	res, _, _ := h.run(t, types.SubmitRequest{Prompt: "task"}, nil)

	// Two continuations are allowed, so the third length response exhausts the
	// budget and is accepted as the final (partial) answer.
	if calls := h.provider.RoundCount(); calls != 3 {
		t.Fatalf("provider rounds = %d, want 3 (truncated + 2 continuations)", calls)
	}
	if res.Closure.Verdict != types.ClosurePartial {
		t.Errorf("verdict = %q, want partial after exhausting the continuation budget (checks=%+v)",
			res.Closure.Verdict, res.Closure.Checks)
	}
	if !hasCheck(res.Closure, types.CheckAnswerNotTruncated, false) {
		t.Errorf("answer_not_truncated must fail: %+v", res.Closure.Checks)
	}
	if res.Answer != "half three" {
		t.Errorf("answer = %q, want the last truncated text", res.Answer)
	}
}

// TestLengthContinuationSucceeds checks the other half of F3: when the model
// finishes after being asked to continue, the run is clean.
func TestLengthContinuationSucceeds(t *testing.T) {
	cfg := types.DefaultAgentConfig()
	p := mem.NewProvider(
		ports.ProviderResponse{FinishReason: ports.FinishLength, Content: "half", Emitted: true},
		ports.ProviderResponse{FinishReason: ports.FinishStop, Content: "half plus more", Emitted: true},
	)
	h := newLoopHarness(t, cfg, p, nil)

	res, _, _ := h.run(t, types.SubmitRequest{Prompt: "task"}, nil)

	if res.State != types.StateCompleted {
		t.Fatalf("state = %s, err=%v", res.State, res.Err)
	}
	if res.Closure.Verdict != types.ClosureClosed {
		t.Errorf("verdict = %q, want closed: the continuation completed the answer (checks=%+v)",
			res.Closure.Verdict, res.Closure.Checks)
	}
}

// TestEmptyAnswerRetriesOnceThenFails is the C8 half of the audit: an empty
// final answer must not be reported as a completed run.
func TestEmptyAnswerRetriesOnceThenFails(t *testing.T) {
	cfg := types.DefaultAgentConfig()
	p := mem.NewProvider(
		ports.ProviderResponse{FinishReason: ports.FinishStop, Content: "", Emitted: true},
		ports.ProviderResponse{FinishReason: ports.FinishStop, Content: "   ", Emitted: true},
	)
	h := newLoopHarness(t, cfg, p, nil)

	res, _, _ := h.run(t, types.SubmitRequest{Prompt: "task"}, nil)

	if res.State != types.StateFailed {
		t.Fatalf("state = %s, want failed: an empty answer is not a finished run", res.State)
	}
	if h.provider.RoundCount() != 2 {
		t.Errorf("provider rounds = %d, want 2 (one retry, then fail)", h.provider.RoundCount())
	}
	if !h.sink.has(types.EventError) {
		t.Errorf("no error event explaining the empty answer; emitted %v", h.sink.types())
	}
	if res.Closure.Verdict != types.ClosureFailed {
		t.Errorf("verdict = %q, want failed (checks=%+v)", res.Closure.Verdict, res.Closure.Checks)
	}
	if !hasCheck(res.Closure, types.CheckAnswerNonEmpty, false) {
		t.Errorf("answer_nonempty must fail: %+v", res.Closure.Checks)
	}
}

// TestEmptyAnswerRetrySucceeds checks that the retry is not a punishment: one
// empty round followed by a real answer is a clean run.
func TestEmptyAnswerRetrySucceeds(t *testing.T) {
	cfg := types.DefaultAgentConfig()
	p := mem.NewProvider(
		ports.ProviderResponse{FinishReason: ports.FinishStop, Content: "", Emitted: true},
		mem.FinalRound("answer after the retry"),
	)
	h := newLoopHarness(t, cfg, p, nil)

	res, _, _ := h.run(t, types.SubmitRequest{Prompt: "task"}, nil)

	if res.State != types.StateCompleted {
		t.Fatalf("state = %s, want completed (err=%v)", res.State, res.Err)
	}
	if res.Answer != "answer after the retry" {
		t.Errorf("answer = %q, want the retry's text", res.Answer)
	}
	if res.Closure.Verdict != types.ClosureClosed {
		t.Errorf("verdict = %q, want closed", res.Closure.Verdict)
	}
}

// TestToolEventCarriesResultAndError is the C5 regression test: the frontend's
// "execution result" and "error detail" blocks read data.result / data.error,
// and the backend used to send neither.
func TestToolEventCarriesResultAndError(t *testing.T) {
	cfg := types.DefaultAgentConfig()
	p := mem.NewProvider(
		mem.ToolCallRound(mem.NewCall("ok1", "file_read", map[string]any{"path": "a.txt"})),
		mem.ToolCallRound(mem.NewCall("bad1", "file_read", map[string]any{"path": "b.txt"})),
		mem.FinalRound("done"),
	)
	runtime := mem.NewToolRuntime(func(_ context.Context, req ports.ToolRequest) (types.ToolResult, error) {
		if req.ToolCallID == "bad1" {
			return types.ToolResult{
				ToolCallID: req.ToolCallID, ToolName: req.ToolName,
				Success: false, Error: "no such file", Content: "partial output",
			}, nil
		}
		return types.ToolResult{
			ToolCallID: req.ToolCallID, ToolName: req.ToolName,
			Success: true, Content: "file contents here",
		}, nil
	})
	h := newLoopHarness(t, cfg, p, runtime)

	res, _, conv := h.run(t, types.SubmitRequest{Prompt: "task"},
		[]types.ToolDefinition{{Name: "file_read"}})
	if res.State != types.StateCompleted {
		t.Fatalf("state = %s, want completed (err=%v)", res.State, res.Err)
	}
	if conv == nil {
		t.Fatal("no conversation returned")
	}

	h.sink.mu.Lock()
	defer h.sink.mu.Unlock()
	var sawResult, sawError bool
	for _, e := range h.sink.events {
		switch e.Type {
		case types.EventToolCompleted:
			if s, _ := e.Data["result"].(string); s == "" {
				t.Errorf("tool_call.completed carried no result preview: %v", e.Data)
			} else {
				sawResult = true
			}
		case types.EventToolFailed:
			if s, _ := e.Data["error"].(string); s == "" {
				t.Errorf("tool_call.failed carried no error detail: %v", e.Data)
			} else {
				sawError = true
			}
			if s, _ := e.Data["result"].(string); s == "" {
				t.Errorf("tool_call.failed carried no result preview: %v", e.Data)
			}
		}
	}
	if !sawResult {
		t.Error("no tool_call.completed event with a result was emitted")
	}
	if !sawError {
		t.Error("no tool_call.failed event with an error was emitted")
	}
}

// TestToolEventPreviewIsBoundedAndRedacted checks the two hard limits on an
// event payload: it must be redacted and it must be bounded, because events are
// persisted and pushed over IPC.
func TestToolEventPreviewIsBoundedAndRedacted(t *testing.T) {
	big := strings.Repeat("x", maxEventPreviewBytes*3)
	secret := "api_key=sk-abcdef0123456789"
	got := previewForUI(big + secret)
	if len(got) > maxEventPreviewBytes+64 {
		t.Errorf("preview length = %d, want it bounded near %d", len(got), maxEventPreviewBytes)
	}
	if !strings.Contains(got, "truncated") && !strings.Contains(got, "\u5df2\u622a\u65ad") {
		t.Errorf("a truncated preview must say so: %q", got[len(got)-60:])
	}

	red := previewForUI("request failed with " + secret)
	if strings.Contains(red, "sk-abcdef0123456789") {
		t.Errorf("preview leaked a credential: %q", red)
	}
}

// TestToolCallKeyGroupsRetries checks the aggregation rule the closure check
// depends on: a retry of the same work on the same target must land on the same
// key, while a different target must not.
func TestToolCallKeyGroupsRetries(t *testing.T) {
	a := types.ToolCallKey("file_edit", map[string]any{"path": "src/a.go", "old": "x"})
	b := types.ToolCallKey("file_edit", map[string]any{"path": "src/a.go", "old": "y"})
	c := types.ToolCallKey("file_edit", map[string]any{"path": "src/b.go"})
	if a != b {
		t.Errorf("retries on the same target must share a key: %q vs %q", a, b)
	}
	if a == c {
		t.Errorf("different targets must not share a key: %q", a)
	}
	// A command keeps only its first word, so a volatile argument does not
	// create a new identity every time.
	d := types.ToolCallKey("terminal_exec", map[string]any{"command": "go test ./... -run X"})
	e := types.ToolCallKey("terminal_exec", map[string]any{"command": "go test ./... -run Y"})
	if d != e {
		t.Errorf("command retries must share a key: %q vs %q", d, e)
	}
}

// TestFailedToolThenSuccessIsResolved checks the "later success resolves an
// earlier failure" rule against the real loop.
func TestFailedToolThenSuccessIsResolved(t *testing.T) {
	cfg := types.DefaultAgentConfig()
	p := mem.NewProvider(
		mem.ToolCallRound(mem.NewCall("bad", "file_read", map[string]any{"path": "a.txt"})),
		mem.ToolCallRound(mem.NewCall("good", "file_read", map[string]any{"path": "a.txt"})),
		mem.FinalRound("done"),
	)
	first := true
	runtime := mem.NewToolRuntime(func(_ context.Context, req ports.ToolRequest) (types.ToolResult, error) {
		if first {
			first = false
			return types.ToolResult{
				ToolCallID: req.ToolCallID, ToolName: req.ToolName,
				Success: false, Error: "transient failure",
			}, nil
		}
		return types.ToolResult{
			ToolCallID: req.ToolCallID, ToolName: req.ToolName,
			Success: true, Content: "ok",
		}, nil
	})
	h := newLoopHarness(t, cfg, p, runtime)

	res, _, _ := h.run(t, types.SubmitRequest{Prompt: "task"},
		[]types.ToolDefinition{{Name: "file_read"}})

	if res.Closure.Verdict != types.ClosureClosed {
		t.Fatalf("verdict = %q, want closed: the retry succeeded, so the earlier failure is resolved (checks=%+v)",
			res.Closure.Verdict, res.Closure.Checks)
	}
	if !hasCheck(res.Closure, types.CheckToolErrorsResolved, true) {
		t.Errorf("tool_errors_resolved must pass: %+v", res.Closure.Checks)
	}
}

// TestUnresolvedToolFailureIsPartial is the other half: a failure with no later
// success must keep the run from claiming it closed cleanly.
func TestUnresolvedToolFailureIsPartial(t *testing.T) {
	cfg := types.DefaultAgentConfig()
	p := mem.NewProvider(
		mem.ToolCallRound(mem.NewCall("bad", "file_read", map[string]any{"path": "a.txt"})),
		mem.FinalRound("done anyway"),
	)
	runtime := mem.NewToolRuntime(func(_ context.Context, req ports.ToolRequest) (types.ToolResult, error) {
		return types.ToolResult{
			ToolCallID: req.ToolCallID, ToolName: req.ToolName,
			Success: false, Error: "no such file",
		}, nil
	})
	h := newLoopHarness(t, cfg, p, runtime)

	res, _, _ := h.run(t, types.SubmitRequest{Prompt: "task"},
		[]types.ToolDefinition{{Name: "file_read"}})

	if res.Closure.Verdict != types.ClosurePartial {
		t.Fatalf("verdict = %q, want partial (checks=%+v)", res.Closure.Verdict, res.Closure.Checks)
	}
	if !hasCheck(res.Closure, types.CheckToolErrorsResolved, false) {
		t.Errorf("tool_errors_resolved must fail: %+v", res.Closure.Checks)
	}
}

// TestReviewFailureMakesRunPartial checks the review_ok check: a reviewer that
// found the model off track must show up in the closure verdict.
func TestReviewFailureMakesRunPartial(t *testing.T) {
	cfg := types.DefaultAgentConfig()
	p := mem.NewProvider(
		mem.ToolCallRound(mem.NewCall("c1", "file_read", map[string]any{"path": "a"})),
		mem.FinalRound("done"),
	)
	sink := &recordingSink{}
	desc := ToolDispatcherFunc(func(_ context.Context, call types.ToolCall) ToolOutcome {
		return ToolOutcome{Call: call, Result: types.ToolResult{
			ToolCallID: call.ID, ToolName: call.Name, Success: true, Content: "ok",
		}}
	})
	reviewer := ReviewerFunc(func(context.Context, RoundSnapshot) (Review, error) {
		return Review{Verdict: VerdictLazy, Issues: []string{"not really executed"}}, nil
	})
	loop, err := NewLoop(LoopConfig{
		Config: cfg, Provider: p, Dispatcher: desc, Sink: sink,
		Guard: NewPanicGuard(GuardConfig{DumpDir: t.TempDir()}), Reviewer: reviewer,
	})
	if err != nil {
		t.Fatalf("NewLoop: %v", err)
	}
	conv := NewConversation("", "task", []types.ToolDefinition{{Name: "file_read"}})
	machine := NewMachine("run-test", "ses-test", TransitionSinkFunc(func(context.Context, Transition) error { return nil }))
	res := loop.Run(context.Background(), LoopInput{
		RunID: "run-test", SessionID: "ses-test",
		Request: types.SubmitRequest{Prompt: "task"}, Conversation: conv, Machine: machine,
	})

	if res.Closure.Verdict != types.ClosurePartial {
		t.Errorf("verdict = %q, want partial: the reviewer reported %q (checks=%+v)",
			res.Closure.Verdict, VerdictLazy, res.Closure.Checks)
	}
	if !hasCheck(res.Closure, types.CheckReviewOK, false) {
		t.Errorf("review_ok must appear and fail: %+v", res.Closure.Checks)
	}
}

// TestApprovedToolCallProceedsAndDeniedCallIsRefused covers the loop half of
// F5: a permission prompt resolved by the user must not park the run a second
// time, and a refusal must become an observation the model can react to.
func TestApprovedToolCallProceedsAndDeniedCallIsRefused(t *testing.T) {
	cfg := types.DefaultAgentConfig()

	newGated := func(t *testing.T) *loopHarness {
		t.Helper()
		sink := &recordingSink{}
		asked := false
		dispatch := ToolDispatcherFunc(func(_ context.Context, call types.ToolCall) ToolOutcome {
			if !asked {
				asked = true
				return ToolOutcome{
					Call: call, NeedsConfirmation: true,
					ConfirmationMessage: "terminal_exec needs confirmation: " + call.Name,
				}
			}
			return ToolOutcome{Call: call, Result: types.ToolResult{
				ToolCallID: call.ID, ToolName: call.Name, Success: true, Content: "executed",
			}}
		})
		loop, err := NewLoop(LoopConfig{
			Config: cfg, Provider: mem.NewProvider(
				mem.ToolCallRound(mem.NewCall("gated-1", "terminal_exec", map[string]any{"command": "ls"})),
				mem.FinalRound("finished"),
			),
			Dispatcher: dispatch, Sink: sink,
			Guard: NewPanicGuard(GuardConfig{DumpDir: t.TempDir()}),
		})
		if err != nil {
			t.Fatalf("NewLoop: %v", err)
		}
		return &loopHarness{loop: loop, sink: sink}
	}

	runWithDecisions := func(t *testing.T, h *loopHarness, decisions map[string]bool) (LoopResult, *Conversation) {
		t.Helper()
		conv := NewConversation("", "task", []types.ToolDefinition{{Name: "terminal_exec"}})
		machine := NewMachine("run-test", "ses-test", TransitionSinkFunc(func(context.Context, Transition) error { return nil }))
		res := h.loop.Run(context.Background(), LoopInput{
			RunID: "run-test", SessionID: "ses-test",
			Request: types.SubmitRequest{Prompt: "task"}, Conversation: conv, Machine: machine,
			ToolDecisions: decisions,
		})
		return res, conv
	}

	t.Run("approve once", func(t *testing.T) {
		h := newGated(t)
		res, _ := runWithDecisions(t, h, map[string]bool{"gated-1": true})
		if res.State == types.StateWaitingUser {
			t.Fatalf("run parked again despite the approval: pending=%v", res.PendingToolCallIDs)
		}
		if res.State != types.StateCompleted {
			t.Fatalf("state = %s, want completed (err=%v)", res.State, res.Err)
		}
		if data := h.sink.closureOf(t); data["verdict"] == nil {
			t.Error("no closure verdict")
		}
	})

	t.Run("no decision parks", func(t *testing.T) {
		h := newGated(t)
		res, _ := runWithDecisions(t, h, nil)
		if res.State != types.StateWaitingUser {
			t.Fatalf("state = %s, want waiting_user", res.State)
		}
		if len(res.PendingToolCallIDs) != 1 || res.PendingToolCallIDs[0] != "gated-1" {
			t.Errorf("pending = %v, want [gated-1]", res.PendingToolCallIDs)
		}
		if res.PendingToolCallNames["gated-1"] != "terminal_exec" {
			t.Errorf("pending names = %v, want terminal_exec", res.PendingToolCallNames)
		}
		if !h.sink.has(types.EventToolPermissionRequired) {
			t.Errorf("no permission_required event; emitted %v", h.sink.types())
		}
	})

	t.Run("session-level approval by tool name", func(t *testing.T) {
		h := newGated(t)
		res, _ := runWithDecisions(t, h, map[string]bool{ToolNameDecisionKey("terminal_exec"): true})
		if res.State != types.StateCompleted {
			t.Fatalf("state = %s, want completed: a name-level approval must cover a re-issued call (err=%v)",
				res.State, res.Err)
		}
	})

	t.Run("denied becomes an observation", func(t *testing.T) {
		h := newGated(t)
		res, conv := runWithDecisions(t, h, map[string]bool{"gated-1": false})
		if res.State != types.StateCompleted {
			t.Fatalf("state = %s, want completed after a refusal (err=%v)", res.State, res.Err)
		}
		found := false
		for _, m := range conv.Messages {
			if m.Role == ports.RoleTool && strings.Contains(m.Content, "用户拒绝执行") {
				found = true
			}
		}
		if !found {
			t.Error("a refused call must be recorded as a tool observation so the model can try another way")
		}
	})
}

// TestModelEchoTravelsInRoundEvents checks the other half of the model-selection
// fix: the provider's model echo must reach the events the UI reads, otherwise
// "the model I picked was really used" is unverifiable.
func TestModelEchoTravelsInRoundEvents(t *testing.T) {
	cfg := types.DefaultAgentConfig()
	resp := mem.FinalRound("answer")
	resp.Model = "deepseek-chat"
	p := mem.NewProvider(resp)
	h := newLoopHarness(t, cfg, p, nil)

	res, _, _ := h.run(t, types.SubmitRequest{Prompt: "task", Model: "deepseek-chat"}, nil)

	if res.State != types.StateCompleted {
		t.Fatalf("state = %s, err=%v", res.State, res.Err)
	}
	h.sink.mu.Lock()
	defer h.sink.mu.Unlock()
	var models []string
	for _, e := range h.sink.events {
		switch e.Type {
		case types.EventRoundCompleted, types.EventFinalAnswer:
			if s, ok := e.Data["model"].(string); ok && s != "" {
				models = append(models, s)
			}
		}
	}
	if len(models) == 0 {
		t.Fatalf("no event carried the model echo; emitted %v", h.sink.typesLocked())
	}
	for _, m := range models {
		if m != "deepseek-chat" {
			t.Errorf("model echo = %q, want deepseek-chat", m)
		}
	}
}

// hasCheck reports whether the report contains the given check with the wanted
// result.
func hasCheck(rep types.RunClosureReport, id string, pass bool) bool {
	for _, c := range rep.Checks {
		if c.ID == id {
			return c.Pass == pass
		}
	}
	return false
}
