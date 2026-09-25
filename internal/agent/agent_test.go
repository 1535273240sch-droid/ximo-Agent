package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/ports"
	"github.com/ximo888ok-netizen/ximo-agent/internal/ports/mem"
	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// ---------------------------------------------------------------------------
// state machine
// ---------------------------------------------------------------------------

// TestRunStateMachineMatchesDocTransitions walks the documented transition
// table and asserts both the legal and the illegal edges. This is the test that
// keeps internal/types' table and the loop's behaviour in agreement.
func TestRunStateMachineMatchesDocTransitions(t *testing.T) {
	legal := []struct{ from, to types.RunState }{
		{types.StateCreated, types.StateQueued},
		{types.StateQueued, types.StatePlanning},
		{types.StateQueued, types.StateThinking},
		{types.StatePlanning, types.StateThinking},
		{types.StateThinking, types.StateExecuting},
		{types.StateThinking, types.StateCompacting},
		{types.StateThinking, types.StateCompleted},
		{types.StateExecuting, types.StateThinking},
		{types.StateExecuting, types.StateWaitingUser},
		{types.StateCompacting, types.StateThinking},
		{types.StateWaitingUser, types.StateThinking},
		{types.StateWaitingUser, types.StateCancelled},
		{types.StateRecovering, types.StateQueued},
		{types.StateRecovering, types.StateWaitingUser},
	}
	for _, tc := range legal {
		if !types.CanTransitionTo(tc.from, tc.to) {
			t.Errorf("%s -> %s should be legal", tc.from, tc.to)
		}
	}

	illegal := []struct{ from, to types.RunState }{
		{types.StateCompleted, types.StateThinking},
		{types.StateCancelled, types.StateThinking},
		{types.StateFailed, types.StateThinking},
		{types.StateCreated, types.StateThinking},
		{types.StateCompleted, types.StateQueued},
		{"bogus", types.StateThinking},
	}
	for _, tc := range illegal {
		if types.CanTransitionTo(tc.from, tc.to) {
			t.Errorf("%s -> %s should be illegal", tc.from, tc.to)
		}
	}
}

// TestMachineRejectsIllegalTransitionAndStaysConsistent checks that a rejected
// transition does not leave the machine in a half-changed state.
func TestMachineRejectsIllegalTransitionAndStaysConsistent(t *testing.T) {
	var mu sync.Mutex
	var seen []Transition
	m := NewMachine("run-1", "ses-1", TransitionSinkFunc(func(_ context.Context, tr Transition) error {
		mu.Lock()
		seen = append(seen, tr)
		mu.Unlock()
		return nil
	}))

	ctx := context.Background()
	if err := m.Transition(ctx, types.StateQueued, "accepted"); err != nil {
		t.Fatalf("created -> queued: %v", err)
	}
	// created -> executing is not legal, and must be refused.
	if err := m.Transition(ctx, types.StateExecuting, "skip ahead"); err == nil {
		t.Fatal("expected created -> executing to be refused")
	} else if types.CodeOf(err) != types.CodeInvalidTransition {
		t.Errorf("error code = %s, want %s", types.CodeOf(err), types.CodeInvalidTransition)
	}
	if got := m.State(); got != types.StateQueued {
		t.Errorf("state = %s after a refused transition, want queued", got)
	}
	mu.Lock()
	n := len(seen)
	mu.Unlock()
	if n != 1 {
		t.Errorf("sink saw %d transitions, want 1: a refused transition must not be reported", n)
	}
}

// TestMachineSealsTerminalStates checks that a terminal state cannot be left,
// which is what stops a late callback from resurrecting a finished run.
func TestMachineSealsTerminalStates(t *testing.T) {
	m := NewMachine("run-2", "ses-1", nil)
	ctx := context.Background()
	if err := m.Transition(ctx, types.StateQueued, "accepted"); err != nil {
		t.Fatal(err)
	}
	if err := m.Transition(ctx, types.StateThinking, "round"); err != nil {
		t.Fatal(err)
	}
	if err := m.Transition(ctx, types.StateCompleted, "answer"); err != nil {
		t.Fatal(err)
	}
	if !m.Terminal() {
		t.Fatal("machine should report terminal after completed")
	}
	if err := m.Transition(ctx, types.StateThinking, "late round"); err == nil {
		t.Fatal("expected a transition out of a terminal state to be refused")
	} else if types.CodeOf(err) != types.CodeTerminalState {
		t.Errorf("error code = %s, want %s", types.CodeOf(err), types.CodeTerminalState)
	}
}

// TransitionSinkFunc adapts a function to TransitionSink.
type TransitionSinkFunc func(context.Context, Transition) error

// OnTransition implements TransitionSink.
func (f TransitionSinkFunc) OnTransition(ctx context.Context, t Transition) error { return f(ctx, t) }

// ---------------------------------------------------------------------------
// panic isolation: the red line
// ---------------------------------------------------------------------------

// TestGuardBoundaryContainsNonCorePanic checks that the four permitted
// boundaries do absorb a panic and report it.
func TestGuardBoundaryContainsNonCorePanic(t *testing.T) {
	var reports []types.CrashReport
	var mu sync.Mutex
	g := NewPanicGuard(GuardConfig{
		DumpDir: t.TempDir(),
		ReportHook: func(r types.CrashReport) {
			mu.Lock()
			reports = append(reports, r)
			mu.Unlock()
		},
	})

	for _, kind := range []types.CrashKind{
		types.CrashKindTool, types.CrashKindWorker,
		types.CrashKindBackground, types.CrashKindRPC,
	} {
		var recovered any
		recovered = g.Guard(kind, "boundary."+string(kind), CrashMeta{RunID: "r1"}, func() {
			panic("boom in " + string(kind))
		})
		if recovered == nil {
			t.Errorf("kind %s: panic was not recovered at a permitted boundary", kind)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(reports) != 4 {
		t.Fatalf("crash reports = %d, want 4 (every contained panic must be reported)", len(reports))
	}
	for _, r := range reports {
		if r.Kind == types.CrashKindCore {
			t.Errorf("a contained panic was misreported as a core crash: %+v", r)
		}
		if r.Panic == "" || r.Stack == "" {
			t.Errorf("crash report is missing panic text or stack: %+v", r)
		}
	}
}

// TestGuardRefusesCoreKind is the guard rail that keeps the two halves from
// being confused: using the recover()-based boundary for a core panic panics
// rather than silently swallowing it.
func TestGuardRefusesCoreKind(t *testing.T) {
	g := NewPanicGuard(GuardConfig{DumpDir: t.TempDir()})
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("Guard with CrashKindCore must panic rather than recover")
		}
		if !strings.Contains(fmtAny(r), "RunCore") {
			t.Errorf("panic message should point at RunCore, got %q", fmtAny(r))
		}
	}()
	g.Guard(types.CrashKindCore, "engine.actor", CrashMeta{}, func() {})
}

// TestRecoverOnlyWorksInline documents why Guard takes a body function rather
// than being used as a bare deferred call. Go's recover() is only effective
// when called directly from a deferred function, so a "helper called by a
// defer" silently fails to recover. This test pins that Go behaviour so a future
// refactor back to the indirect shape fails loudly here.
func TestRecoverOnlyWorksInline(t *testing.T) {
	g := NewPanicGuard(GuardConfig{DumpDir: t.TempDir()})

	// The supported shape: the body is passed in and recover() runs inside
	// Guard's own deferred literal.
	recovered := g.Guard(types.CrashKindTool, "tool.inline", CrashMeta{}, func() {
		panic("inline panic")
	})
	if recovered == nil {
		t.Fatal("Guard failed to recover a panic thrown by its body")
	}

	// The unsupported shape, asserted to demonstrate the hazard: a deferred
	// literal that calls a helper which calls recover() does not catch the
	// panic. The panic must therefore propagate.
	helperRecovers := func() (got any) { return recover() }
	escaped := func() (escapedPanic bool) {
		defer func() {
			if r := recover(); r != nil {
				escapedPanic = true
			}
		}()
		// This deferred literal calls a helper; recover() inside that helper
		// returns nil, so the panic keeps unwinding into the outer recover.
		func() {
			defer func() { _ = helperRecovers() }()
			panic("indirect panic")
		}()
		return false
	}()
	if !escaped {
		t.Fatal("expected an indirect recover() to fail to contain the panic; " +
			"if this now passes, Guard's design may be simplified")
	}
}

// TestRunCoreLetsPanicEscape asserts the core boundary does NOT recover: the
// panic must propagate out of RunCore so the process dies, and the context must
// be cleared when the body returns normally.
func TestRunCoreLetsPanicEscape(t *testing.T) {
	dir := t.TempDir()
	g := NewPanicGuard(GuardConfig{DumpDir: dir})

	panicked := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				panicked = true
			}
		}()
		g.RunCore("engine.run", CrashMeta{RunID: "run-abc", SessionID: "ses-xyz"}, func() {
			panic("core state is broken")
		})
	}()

	if !panicked {
		t.Fatal("RunCore recovered a core panic; the process would have continued with broken state")
	}
	// The context must be cleared on the normal path, so a later crash is not
	// attributed to this run.
	component, _ := g.CoreContext()
	if component != "" {
		t.Errorf("core context was not cleared after RunCore returned normally: %q", component)
	}
}

// TestInstallCrashOutputCapturesCorePanic verifies the mechanism the core-panic
// path depends on: that SetCrashOutput points the runtime's crash reporting at
// our dump file, that the file is writable, and that the component/run header is
// written so a following runtime stack trace can be attributed.
//
// The runtime's fatal path cannot be triggered in-process without killing the
// test binary, so this asserts the wiring rather than the dump contents.
func TestInstallCrashOutputCapturesCorePanic(t *testing.T) {
	dir := t.TempDir()
	g := NewPanicGuard(GuardConfig{DumpDir: dir})

	cleanup, err := g.InstallCrashOutput()
	if err != nil {
		t.Fatalf("InstallCrashOutput: %v", err)
	}
	defer cleanup()

	path := g.CrashDumpPath()
	if path == "" {
		t.Fatal("no crash dump path was reported after installing")
	}
	if _, err := os.Stat(filepath.Dir(path)); err != nil {
		t.Fatalf("crash dump directory missing: %v", err)
	}

	g.markCore("engine.actor", CrashMeta{RunID: "r-9", SessionID: "s-9", Round: 3})
	g.clearCore()

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read crash dump: %v", err)
	}
	text := string(body)
	if !strings.Contains(text, "engine.actor") {
		t.Errorf("crash dump does not name the component:\n%s", text)
	}
	if !strings.Contains(text, "r-9") {
		t.Errorf("crash dump does not name the run:\n%s", text)
	}
}

// TestWriteCrashDumpIsAtomic checks that the dump is written via a temp file and
// renamed, so a partially written dump cannot be mistaken for a complete one.
func TestWriteCrashDumpIsAtomic(t *testing.T) {
	dir := t.TempDir()
	report := types.CrashReport{
		ID: "crash_test", Kind: types.CrashKindCore, Component: "engine.actor",
		RunID: "run-1", Panic: "boom", Stack: "goroutine 1 [running]:\n...",
		Timestamp: time.Now(),
	}
	if err := WriteCrashDump(dir, report); err != nil {
		t.Fatalf("WriteCrashDump: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly one file (no leftover .tmp), got %d", len(entries))
	}
	if strings.HasSuffix(entries[0].Name(), ".tmp") {
		t.Errorf("leftover temporary file: %s", entries[0].Name())
	}
	body, _ := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if !strings.Contains(string(body), "boom") {
		t.Errorf("dump does not contain the panic text:\n%s", body)
	}
}

// TestCrashReportRedactsSecrets checks that a credential in a panic value never
// reaches the dump file (doc ch. 21).
func TestCrashReportRedactsSecrets(t *testing.T) {
	dir := t.TempDir()
	var got types.CrashReport
	g := NewPanicGuard(GuardConfig{
		DumpDir: dir,
		ReportHook: func(r types.CrashReport) {
			got = r
		},
	})
	func() {
		g.Guard(types.CrashKindTool, "tool.exec", CrashMeta{}, func() {
			panic("provider rejected request with api_key=sk-abcdef0123456789")
		})
	}()

	if strings.Contains(got.Panic, "sk-abcdef0123456789") {
		t.Errorf("panic text leaked a credential: %q", got.Panic)
	}
	if !strings.Contains(got.Panic, "REDACTED") {
		t.Errorf("expected a redaction marker in the panic text: %q", got.Panic)
	}
}

// ---------------------------------------------------------------------------
// loop behaviour
// ---------------------------------------------------------------------------

// loopHarness assembles a loop with scripted collaborators.
type loopHarness struct {
	loop     *Loop
	provider *mem.Provider
	sink     *recordingSink
	guard    *PanicGuard
}

// recordingSink captures loop events for assertions.
type recordingSink struct {
	mu     sync.Mutex
	events []LoopEvent
}

func (s *recordingSink) Emit(_ context.Context, ev LoopEvent) error {
	s.mu.Lock()
	s.events = append(s.events, ev)
	s.mu.Unlock()
	return nil
}

func (s *recordingSink) types() []types.EventType {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]types.EventType, 0, len(s.events))
	for _, e := range s.events {
		out = append(out, e.Type)
	}
	return out
}

func (s *recordingSink) count(t types.EventType) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, e := range s.events {
		if e.Type == t {
			n++
		}
	}
	return n
}

func (s *recordingSink) has(t types.EventType) bool { return s.count(t) > 0 }

// newLoopHarness builds a loop with a scripted provider and a dispatcher backed
// by the given tool runtime.
func newLoopHarness(t *testing.T, cfg types.AgentConfig, provider *mem.Provider, runtime ports.ToolRuntime) *loopHarness {
	t.Helper()
	sink := &recordingSink{}
	guard := NewPanicGuard(GuardConfig{DumpDir: t.TempDir()})
	dispatcher := ToolDispatcherFunc(func(ctx context.Context, call types.ToolCall) ToolOutcome {
		if runtime == nil {
			return ToolOutcome{Call: call, Result: types.ToolResult{
				ToolCallID: call.ID, ToolName: call.Name, Success: true, Content: "ok",
			}}
		}
		res, err := runtime.Execute(ctx, ports.ToolRequest{
			ToolCallID: call.ID, ToolName: call.Name, Arguments: call.Arguments,
		})
		return ToolOutcome{Call: call, Result: res, Err: err}
	})
	loop, err := NewLoop(LoopConfig{
		Config: cfg, Provider: provider, Dispatcher: dispatcher, Sink: sink, Guard: guard,
	})
	if err != nil {
		t.Fatalf("NewLoop: %v", err)
	}
	return &loopHarness{loop: loop, provider: provider, sink: sink, guard: guard}
}

// run executes the loop with a fresh machine and returns the result, the
// machine and the conversation.
func (h *loopHarness) run(t *testing.T, req types.SubmitRequest, tools []types.ToolDefinition) (LoopResult, *Machine, *Conversation) {
	t.Helper()
	conv := NewConversation(req.SystemPrompt, req.Prompt, tools)
	machine := NewMachine("run-test", "ses-test", TransitionSinkFunc(func(_ context.Context, _ Transition) error {
		return nil
	}))
	res := h.loop.Run(context.Background(), LoopInput{
		RunID: "run-test", SessionID: "ses-test",
		Request: req, Conversation: conv, Machine: machine,
	})
	return res, machine, conv
}

// TestLoopRunsThinkToolObserveCycle is the basic functional test: one tool round
// then a final answer.
func TestLoopRunsThinkToolObserveCycle(t *testing.T) {
	cfg := types.DefaultAgentConfig()
	p := mem.NewProvider(
		mem.ToolCallRound(mem.NewCall("call-1", "file_read", map[string]any{"path": "a.txt"})),
		mem.FinalRound("all done"),
	)
	h := newLoopHarness(t, cfg, p, nil)

	res, machine, _ := h.run(t, types.SubmitRequest{Prompt: "read a.txt"},
		[]types.ToolDefinition{{Name: "file_read"}})

	if res.State != types.StateCompleted {
		t.Fatalf("state = %s, want completed (err=%v)", res.State, res.Err)
	}
	if res.Answer != "all done" {
		t.Errorf("answer = %q, want %q", res.Answer, "all done")
	}
	if res.Rounds != 2 {
		t.Errorf("rounds = %d, want 2 (one tool round plus the answer)", res.Rounds)
	}
	if res.ToolCalls != 1 {
		t.Errorf("tool calls = %d, want 1", res.ToolCalls)
	}
	if got := machine.State(); got != types.StateCompleted {
		t.Errorf("machine state = %s, want completed", got)
	}
	for _, want := range []types.EventType{
		types.EventRoundStarted, types.EventToolCallRequested,
		types.EventToolCompleted, types.EventFinalAnswer,
	} {
		if !h.sink.has(want) {
			t.Errorf("loop did not emit %s; emitted %v", want, h.sink.types())
		}
	}
}

// TestLoopExecutesParallelToolCallsInOrder checks that a multi-call round runs
// the calls concurrently but writes their observations back in the model's
// original order, which is what keeps the conversation deterministic.
func TestLoopExecutesParallelToolCallsInOrder(t *testing.T) {
	cfg := types.DefaultAgentConfig()
	calls := []types.ToolCall{
		mem.NewCall("c1", "slow_tool", nil),
		mem.NewCall("c2", "slow_tool", nil),
		mem.NewCall("c3", "slow_tool", nil),
	}
	p := mem.NewProvider(mem.ToolCallRound(calls...), mem.FinalRound("done"))

	// Distinct sleep per call, so completion order differs from request order.
	var mu sync.Mutex
	seen := 0
	runtime := mem.NewToolRuntime(func(ctx context.Context, req ports.ToolRequest) (types.ToolResult, error) {
		mu.Lock()
		seen++
		idx := seen
		mu.Unlock()
		time.Sleep(time.Duration(4-idx) * 10 * time.Millisecond)
		return types.ToolResult{Success: true, Content: "result-for-" + req.ToolCallID}, nil
	})

	h := newLoopHarness(t, cfg, p, runtime)
	_, _, conv := h.run(t, types.SubmitRequest{Prompt: "run three tools"},
		[]types.ToolDefinition{{Name: "slow_tool"}})

	if runtime.CallCount() != 3 {
		t.Fatalf("runtime executed %d calls, want 3", runtime.CallCount())
	}

	// Locate the three tool-result messages and check they are in call order.
	var gotOrder []string
	for _, m := range conv.Messages {
		if m.Role == ports.RoleTool {
			gotOrder = append(gotOrder, m.ToolCallID)
		}
	}
	want := []string{"c1", "c2", "c3"}
	if len(gotOrder) != len(want) {
		t.Fatalf("tool results = %v, want %v", gotOrder, want)
	}
	for i := range want {
		if gotOrder[i] != want[i] {
			t.Errorf("tool results out of order: got %v, want %v", gotOrder, want)
			break
		}
	}
}

// TestLoopEnforcesMaxRoundsAndWrapsUp checks the round cap and the forced
// text-only wrap-up round that follows it.
func TestLoopEnforcesMaxRoundsAndWrapsUp(t *testing.T) {
	cfg := types.DefaultAgentConfig()
	cfg.MaxToolRounds = 3

	// Exactly the cap's worth of tool-asking rounds: the model keeps calling
	// tools until the loop stops giving it rounds, which is the situation the
	// wrap-up path exists for. The round after the cap then hits Exhausted and
	// supplies the final text.
	script := make([]ports.ProviderResponse, 0, cfg.MaxToolRounds)
	for i := 0; i < cfg.MaxToolRounds; i++ {
		script = append(script, mem.ToolCallRound(
			mem.NewCall("loop-"+itoa(i), "noop_tool", nil)))
	}
	p := mem.NewProvider(script...)
	p.Exhausted = mem.FinalRound("wrapped up")

	h := newLoopHarness(t, cfg, p, nil)
	res, _, conv := h.run(t, types.SubmitRequest{Prompt: "loop forever"},
		[]types.ToolDefinition{{Name: "noop_tool"}})

	if res.State != types.StateCompleted {
		t.Fatalf("state = %s, want completed (err=%v)", res.State, res.Err)
	}
	if res.Rounds > cfg.MaxToolRounds+1 {
		t.Errorf("rounds = %d, want at most the cap %d plus one wrap-up round",
			res.Rounds, cfg.MaxToolRounds)
	}
	if res.Answer != "wrapped up" {
		t.Errorf("answer = %q, want the wrap-up answer", res.Answer)
	}
	// The wrap-up prompt must have been added.
	foundWrapUp := false
	for _, m := range conv.Messages {
		if strings.Contains(m.Content, WrapUpPrompt) {
			foundWrapUp = true
		}
	}
	if !foundWrapUp {
		t.Error("the wrap-up instruction was never added to the conversation")
	}
	if !h.sink.has(types.EventFinalAnswer) {
		t.Errorf("no final answer event; emitted %v", h.sink.types())
	}
}

// TestLoopLongTaskContinuations checks the segmented long-task loop: rounds per
// segment, then a continuation batch, up to the continuation budget.
func TestLoopLongTaskContinuations(t *testing.T) {
	cfg := types.DefaultAgentConfig()
	cfg.LongTaskRoundsPerSegment = 2
	cfg.LongTaskMaxContinuations = 2

	script := make([]ports.ProviderResponse, 0, 12)
	for i := 0; i < 12; i++ {
		script = append(script, mem.ToolCallRound(
			mem.NewCall("lt-"+itoa(i), "noop_tool", nil)))
	}
	p := mem.NewProvider(script...)
	p.Exhausted = mem.FinalRound("long task wrapped up")

	h := newLoopHarness(t, cfg, p, nil)
	res, _, conv := h.run(t, types.SubmitRequest{Prompt: "long task", LongTask: true},
		[]types.ToolDefinition{{Name: "noop_tool"}})

	if res.State != types.StateCompleted {
		t.Fatalf("state = %s, want completed (err=%v)", res.State, res.Err)
	}
	if conv.Continuations != cfg.LongTaskMaxContinuations {
		t.Errorf("continuations = %d, want %d", conv.Continuations, cfg.LongTaskMaxContinuations)
	}
	if got := h.sink.count(types.EventContinuation); got != cfg.LongTaskMaxContinuations {
		t.Errorf("continuation events = %d, want %d", got, cfg.LongTaskMaxContinuations)
	}
	// Rounds per segment x (1 + continuations), plus the wrap-up round.
	maxRounds := cfg.LongTaskRoundsPerSegment * (1 + cfg.LongTaskMaxContinuations)
	if res.Rounds > maxRounds+1 {
		t.Errorf("rounds = %d, want at most %d", res.Rounds, maxRounds+1)
	}
}

// TestLoopStopsWhenAllTodosDone checks the v1 behaviour of ending the run and
// stripping tools once todo_write reports everything complete.
func TestLoopStopsWhenAllTodosDone(t *testing.T) {
	cfg := types.DefaultAgentConfig()
	p := mem.NewProvider(mem.ToolCallRound(mem.NewCall("t1", "todo_write", nil)))
	runtime := mem.NewToolRuntime(func(_ context.Context, _ ports.ToolRequest) (types.ToolResult, error) {
		return types.ToolResult{
			Success: true, Content: "todos updated",
			Metadata: map[string]any{"total": 3, "done": 3, "active": 0, "pending": 0},
		}, nil
	})
	h := newLoopHarness(t, cfg, p, runtime)

	res, _, conv := h.run(t, types.SubmitRequest{Prompt: "do the todos"},
		[]types.ToolDefinition{{Name: "todo_write"}})

	if res.State != types.StateCompleted {
		t.Fatalf("state = %s, want completed (err=%v)", res.State, res.Err)
	}
	if len(conv.Tools) != 0 {
		t.Errorf("tools = %d, want 0: the catalogue must be withheld to force a summary", len(conv.Tools))
	}
	if h.provider.RoundCount() != 1 {
		t.Errorf("provider rounds = %d, want 1: a completed todo list must not spend another round",
			h.provider.RoundCount())
	}
}

// TestLoopContainsToolPanic checks the tool-goroutine boundary: a panicking tool
// becomes a failed observation, and the run continues.
func TestLoopContainsToolPanic(t *testing.T) {
	cfg := types.DefaultAgentConfig()
	p := mem.NewProvider(
		mem.ToolCallRound(mem.NewCall("boom", "exploding_tool", nil)),
		mem.FinalRound("recovered from the tool failure"),
	)
	h := newLoopHarness(t, cfg, p, mem.PanicRuntime())

	res, _, conv := h.run(t, types.SubmitRequest{Prompt: "run the exploding tool"},
		[]types.ToolDefinition{{Name: "exploding_tool"}})

	if res.State != types.StateCompleted {
		t.Fatalf("state = %s, want completed: a tool panic must not fail the run (err=%v)", res.State, res.Err)
	}
	if !h.sink.has(types.EventToolFailed) {
		t.Errorf("no tool failure event; emitted %v", h.sink.types())
	}
	// The model must be told about the panic, so it can react.
	found := false
	for _, m := range conv.Messages {
		if m.Role == ports.RoleTool && strings.Contains(m.Content, "panicked") {
			found = true
		}
	}
	if !found {
		t.Error("the tool panic was not reported back to the model as an observation")
	}
}

// TestLoopPropagatesToolFailureToModel checks that an ordinary tool failure is
// an observation, not a run failure. This is the v1 contract: the model sees
// "Error: ..." and decides what to do.
func TestLoopPropagatesToolFailureToModel(t *testing.T) {
	cfg := types.DefaultAgentConfig()
	p := mem.NewProvider(
		mem.ToolCallRound(mem.NewCall("f1", "terminal", nil)),
		mem.FinalRound("handled the failure"),
	)
	h := newLoopHarness(t, cfg, p, mem.FailingRuntime("command not found"))

	res, _, conv := h.run(t, types.SubmitRequest{Prompt: "run a bad command"},
		[]types.ToolDefinition{{Name: "terminal"}})

	if res.State != types.StateCompleted {
		t.Fatalf("state = %s, want completed: a tool failure must not fail the run", res.State)
	}
	found := false
	for _, m := range conv.Messages {
		if m.Role == ports.RoleTool && strings.HasPrefix(m.Content, "Error: command not found") {
			found = true
		}
	}
	if !found {
		t.Error("the tool failure was not written back as an observation")
	}
}

// TestLoopCancellationStopsRun checks that a cancelled context ends the run in
// the cancelled state and produces a cancellation event.
func TestLoopCancellationStopsRun(t *testing.T) {
	cfg := types.DefaultAgentConfig()
	p := mem.NewProvider(mem.ToolCallRound(mem.NewCall("c", "noop_tool", nil)))
	h := newLoopHarness(t, cfg, p, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	conv := NewConversation("", "do something", []types.ToolDefinition{{Name: "noop_tool"}})
	machine := NewMachine("run-cancel", "ses-1", nil)
	res := h.loop.Run(ctx, LoopInput{
		RunID: "run-cancel", SessionID: "ses-1",
		Request:      types.SubmitRequest{Prompt: "do something"},
		Conversation: conv, Machine: machine,
	})

	if res.State != types.StateCancelled {
		t.Fatalf("state = %s, want cancelled (err=%v)", res.State, res.Err)
	}
	if !h.sink.has(types.EventCancellation) {
		t.Errorf("no cancellation event; emitted %v", h.sink.types())
	}
	if got := machine.State(); got != types.StateCancelled {
		t.Errorf("machine state = %s, want cancelled", got)
	}
}

// TestLoopProviderErrorFailsRun checks that an unrecoverable provider error ends
// the run in the failed state rather than looping.
func TestLoopProviderErrorFailsRun(t *testing.T) {
	cfg := types.DefaultAgentConfig()
	p := mem.NewProvider(mem.ErrorRound("upstream is down"))
	h := newLoopHarness(t, cfg, p, nil)

	res, machine, _ := h.run(t, types.SubmitRequest{Prompt: "ask the model"}, nil)

	if res.State != types.StateFailed {
		t.Fatalf("state = %s, want failed", res.State)
	}
	if res.Err == nil {
		t.Error("expected an error on the result")
	}
	if got := machine.State(); got != types.StateFailed {
		t.Errorf("machine state = %s, want failed", got)
	}
	if !h.sink.has(types.EventError) {
		t.Errorf("no error event; emitted %v", h.sink.types())
	}
}

// TestLoopRoundTripsReasoningContent checks the DeepSeek protocol requirement
// that an assistant turn's reasoning content is preserved when thinking is on,
// and dropped when it is off.
func TestLoopRoundTripsReasoningContent(t *testing.T) {
	cases := []struct {
		effort types.ReasoningEffort
		keep   bool
	}{
		{types.EffortHigh, true},
		{types.EffortUltra, true},
		{types.EffortMax, true},
		{types.EffortLow, true},
		{types.EffortOff, false},
	}
	for _, tc := range cases {
		t.Run(string(tc.effort), func(t *testing.T) {
			cfg := types.DefaultAgentConfig()
			round := mem.ToolCallRound(mem.NewCall("c1", "noop_tool", nil))
			round.ReasoningContent = "step by step reasoning"
			p := mem.NewProvider(round, mem.FinalRound("done"))
			h := newLoopHarness(t, cfg, p, nil)

			_, _, conv := h.run(t, types.SubmitRequest{
				Prompt: "do it", Effort: tc.effort,
			}, []types.ToolDefinition{{Name: "noop_tool"}})

			var assistant *ports.Message
			for i := range conv.Messages {
				if conv.Messages[i].Role == ports.RoleAssistant && len(conv.Messages[i].ToolCalls) > 0 {
					assistant = &conv.Messages[i]
					break
				}
			}
			if assistant == nil {
				t.Fatal("no assistant tool-call turn was recorded")
			}
			if tc.keep && assistant.ReasoningContent != "step by step reasoning" {
				t.Errorf("effort %s: reasoning content = %q, want it preserved verbatim",
					tc.effort, assistant.ReasoningContent)
			}
			if !tc.keep && assistant.ReasoningContent != "" {
				t.Errorf("effort %s: reasoning content = %q, want it dropped",
					tc.effort, assistant.ReasoningContent)
			}
		})
	}
}

// TestLoopTruncatesLongToolOutput checks that a huge tool result is clamped
// before it enters the conversation, so one tool cannot blow the context.
func TestLoopTruncatesLongToolOutput(t *testing.T) {
	cfg := types.DefaultAgentConfig()
	cfg.MaxToolResultChars = 100
	p := mem.NewProvider(
		mem.ToolCallRound(mem.NewCall("big", "file_read", nil)),
		mem.FinalRound("read it"),
	)
	huge := strings.Repeat("x", 5000)
	runtime := mem.NewToolRuntime(func(_ context.Context, _ ports.ToolRequest) (types.ToolResult, error) {
		return types.ToolResult{Success: true, Content: huge}, nil
	})
	h := newLoopHarness(t, cfg, p, runtime)

	_, _, conv := h.run(t, types.SubmitRequest{Prompt: "read a huge file"},
		[]types.ToolDefinition{{Name: "file_read"}})

	for _, m := range conv.Messages {
		if m.Role != ports.RoleTool {
			continue
		}
		if len(m.Content) > cfg.MaxToolResultChars {
			t.Errorf("tool observation is %d chars, want at most %d",
				len(m.Content), cfg.MaxToolResultChars)
		}
		if !strings.Contains(m.Content, "truncated") {
			t.Error("a truncated observation should say so")
		}
	}
}

// TestLoopSanitizesControlCharacters checks that control characters from scraped
// content are removed, since they break provider JSON parsing.
func TestLoopSanitizesControlCharacters(t *testing.T) {
	cfg := types.DefaultAgentConfig()
	p := mem.NewProvider(
		mem.ToolCallRound(mem.NewCall("s1", "web_fetch", nil)),
		mem.FinalRound("fetched"),
	)
	dirty := "start\x00\x01\x02\x7f middle \x1b end\nkeep newline\ttab"
	runtime := mem.NewToolRuntime(func(_ context.Context, _ ports.ToolRequest) (types.ToolResult, error) {
		return types.ToolResult{Success: true, Content: dirty}, nil
	})
	h := newLoopHarness(t, cfg, p, runtime)

	_, _, conv := h.run(t, types.SubmitRequest{Prompt: "fetch a page"},
		[]types.ToolDefinition{{Name: "web_fetch"}})

	for _, m := range conv.Messages {
		if m.Role != ports.RoleTool {
			continue
		}
		for _, bad := range []string{"\x00", "\x01", "\x02", "\x7f", "\x1b"} {
			if strings.Contains(m.Content, bad) {
				t.Errorf("control character %q survived sanitisation", bad)
			}
		}
		if !strings.Contains(m.Content, "\n") || !strings.Contains(m.Content, "\t") {
			t.Error("newline and tab must be preserved")
		}
	}
}

// TestLoopSupervisionInjectsCorrection checks the ultra-mode gate: an
// off-track verdict appends a corrective system message and emits a supervision
// event.
func TestLoopSupervisionInjectsCorrection(t *testing.T) {
	cfg := types.DefaultAgentConfig()
	p := mem.NewProvider(
		mem.ToolCallRound(mem.NewCall("c1", "noop_tool", nil)),
		mem.FinalRound("corrected"),
	)
	sink := &recordingSink{}
	guard := NewPanicGuard(GuardConfig{DumpDir: t.TempDir()})
	reviewer := ReviewerFunc(func(_ context.Context, _ RoundSnapshot) (Review, error) {
		return Review{
			Verdict: VerdictOffTrack, Severity: "high",
			Issues:     []string{"skipped the verification step"},
			Correction: "run the tests before concluding",
		}, nil
	})
	loop, err := NewLoop(LoopConfig{
		Config: cfg, Provider: p, Sink: sink, Guard: guard, Reviewer: reviewer,
		Dispatcher: ToolDispatcherFunc(func(_ context.Context, call types.ToolCall) ToolOutcome {
			return ToolOutcome{Call: call, Result: types.ToolResult{
				ToolCallID: call.ID, ToolName: call.Name, Success: true, Content: "ok"}}
		}),
	})
	if err != nil {
		t.Fatal(err)
	}

	conv := NewConversation("", "fix the bug", []types.ToolDefinition{{Name: "noop_tool"}})
	machine := NewMachine("run-sup", "ses-1", nil)
	res := loop.Run(context.Background(), LoopInput{
		RunID: "run-sup", SessionID: "ses-1",
		Request:      types.SubmitRequest{Prompt: "fix the bug", Effort: types.EffortUltra},
		Conversation: conv, Machine: machine,
	})
	if res.State != types.StateCompleted {
		t.Fatalf("state = %s, want completed (err=%v)", res.State, res.Err)
	}

	if !sink.has(types.EventSupervision) {
		t.Errorf("no supervision event; emitted %v", sink.types())
	}
	found := false
	for _, m := range conv.Messages {
		if m.Role == ports.RoleSystem && strings.Contains(m.Content, "verification step") {
			found = true
		}
	}
	if !found {
		t.Error("the corrective system message was not injected into the conversation")
	}
	if len(conv.Reviews) != 1 {
		t.Errorf("recorded reviews = %d, want 1", len(conv.Reviews))
	}
}

// TestLoopReviewerFailureIsNonFatal checks that a broken reviewer degrades to
// no review rather than failing the run, matching v1's tolerant behaviour.
func TestLoopReviewerFailureIsNonFatal(t *testing.T) {
	cfg := types.DefaultAgentConfig()
	p := mem.NewProvider(
		mem.ToolCallRound(mem.NewCall("c1", "noop_tool", nil)),
		mem.FinalRound("done"),
	)
	sink := &recordingSink{}
	reviewer := ReviewerFunc(func(_ context.Context, _ RoundSnapshot) (Review, error) {
		return Review{}, types.NewError(types.CodeProviderFailed, "reviewer unreachable")
	})
	loop, err := NewLoop(LoopConfig{
		Config: cfg, Provider: p, Sink: sink, Reviewer: reviewer,
		Dispatcher: ToolDispatcherFunc(func(_ context.Context, call types.ToolCall) ToolOutcome {
			return ToolOutcome{Call: call, Result: types.ToolResult{
				ToolCallID: call.ID, ToolName: call.Name, Success: true, Content: "ok"}}
		}),
	})
	if err != nil {
		t.Fatal(err)
	}

	conv := NewConversation("", "do it", []types.ToolDefinition{{Name: "noop_tool"}})
	machine := NewMachine("run-x", "ses-1", nil)
	res := loop.Run(context.Background(), LoopInput{
		RunID: "run-x", SessionID: "ses-1",
		Request:      types.SubmitRequest{Prompt: "do it", Effort: types.EffortUltra},
		Conversation: conv, Machine: machine,
	})
	if res.State != types.StateCompleted {
		t.Fatalf("state = %s, want completed: a reviewer failure must not fail the run", res.State)
	}
}

// TestLoopEmitsTokenDeltasAsEphemeral checks that streamed deltas reach the sink
// as token.delta events and are not treated as durable boundaries.
func TestLoopEmitsTokenDeltasAsEphemeral(t *testing.T) {
	cfg := types.DefaultAgentConfig()
	p := mem.NewProvider(mem.FinalRound("streamed answer"))
	p.Delivers = true
	p.DeltasPerRound = 25
	h := newLoopHarness(t, cfg, p, nil)

	res, _, _ := h.run(t, types.SubmitRequest{Prompt: "stream something"}, nil)
	if res.State != types.StateCompleted {
		t.Fatalf("state = %s, want completed", res.State)
	}
	if got := h.sink.count(types.EventTokenDelta); got != 25 {
		t.Errorf("token delta events = %d, want 25", got)
	}
	if types.EventTokenDelta.Durable() {
		t.Error("token deltas must not be durable: the log stores boundaries, not characters")
	}
	if !types.EventTokenDelta.Mergeable() {
		t.Error("token deltas must be mergeable")
	}
}

// TestLoopPlanningNarrowsToolSet checks the planning phase: with a large
// catalogue and a long prompt, the tool set is narrowed.
func TestLoopPlanningNarrowsToolSet(t *testing.T) {
	cfg := types.DefaultAgentConfig()
	tools := make([]types.ToolDefinition, 0, 10)
	for i := 0; i < 10; i++ {
		tools = append(tools, types.ToolDefinition{Name: "tool_" + itoa(i)})
	}
	p := mem.NewProvider(mem.FinalRound("planned and done"))
	planner := PlannerFunc(func(_ context.Context, req PlanRequest) (Plan, error) {
		return ParsePlan(
			"1. read the file\n2. [TOOLS] tool_1, tool_2 [/TOOLS]\n3. [STEPS] read; edit [/STEPS]",
			"", req.Tools)
	})
	sink := &recordingSink{}
	loop, err := NewLoop(LoopConfig{
		Config: cfg, Provider: p, Sink: sink, Planner: planner,
		Dispatcher: ToolDispatcherFunc(func(_ context.Context, call types.ToolCall) ToolOutcome {
			return ToolOutcome{Call: call, Result: types.ToolResult{Success: true}}
		}),
	})
	if err != nil {
		t.Fatal(err)
	}

	const prompt = "please refactor the parser so it handles nested structures"
	conv := NewConversation("", prompt, tools)
	machine := NewMachine("run-plan", "ses-1", nil)
	res := loop.Run(context.Background(), LoopInput{
		RunID: "run-plan", SessionID: "ses-1",
		Request:      types.SubmitRequest{Prompt: prompt},
		Conversation: conv, Machine: machine,
	})
	if res.State != types.StateCompleted {
		t.Fatalf("state = %s, want completed (err=%v)", res.State, res.Err)
	}
	if len(conv.Tools) != 2 {
		t.Errorf("tools after planning = %d, want 2", len(conv.Tools))
	}
	if conv.Plan == nil {
		t.Fatal("no plan was recorded")
	}
	if !sink.has(types.EventPlanningCompleted) {
		t.Errorf("no planning completion event; emitted %v", sink.types())
	}
}

// TestShouldPlanTriggerCriteria pins the planning trigger to the v1 criteria.
func TestShouldPlanTriggerCriteria(t *testing.T) {
	long := strings.Repeat("x", 50)
	short := "hi"

	manyTools := make([]types.ToolDefinition, 8)
	for i := range manyTools {
		manyTools[i] = types.ToolDefinition{Name: "t" + itoa(i)}
	}
	fewTools := manyTools[:3]

	disabled := types.DefaultAgentConfig()
	disabled.PlanningEnabled = false

	cases := []struct {
		name string
		cfg  types.AgentConfig
		conv *Conversation
		want bool
	}{
		{"long prompt with many tools", types.DefaultAgentConfig(),
			NewConversation("", long, manyTools), true},
		{"short prompt", types.DefaultAgentConfig(),
			NewConversation("", short, manyTools), false},
		{"few tools", types.DefaultAgentConfig(),
			NewConversation("", long, fewTools), false},
		{"planning disabled", disabled, NewConversation("", long, manyTools), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ShouldPlan(tc.cfg, tc.conv, context.Background()); got != tc.want {
				t.Errorf("ShouldPlan = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestParsePlanRejectsUnusableOutput checks the validation gate: a plan that
// names no tools, or every tool, is rejected so the full catalogue is kept.
func TestParsePlanRejectsUnusableOutput(t *testing.T) {
	tools := []types.ToolDefinition{{Name: "a"}, {Name: "b"}, {Name: "c"}}

	if _, err := ParsePlan("no markers here", "", tools); err == nil {
		t.Error("expected a plan with no [TOOLS] block to be rejected")
	}
	if _, err := ParsePlan("[TOOLS] [/TOOLS]", "", tools); err == nil {
		t.Error("expected an empty tool selection to be rejected")
	}
	if _, err := ParsePlan("[TOOLS] nonexistent [/TOOLS]", "", tools); err == nil {
		t.Error("expected a plan naming only unknown tools to be rejected")
	}
	// Selecting every tool saves nothing and must be rejected.
	if _, err := ParsePlan("[TOOLS] a, b, c [/TOOLS]", "", tools); err == nil {
		t.Error("expected a plan selecting every tool to be rejected")
	}
	plan, err := ParsePlan("[TOOLS] a [/TOOLS]", "", tools)
	if err != nil {
		t.Fatalf("valid plan rejected: %v", err)
	}
	if len(plan.Tools) != 1 || plan.Tools[0].Name != "a" {
		t.Errorf("plan tools = %+v, want just [a]", plan.Tools)
	}
}

// TestCompactionTierThresholdsMatchDoc pins the four-tier ladder. With the
// default ratio of 0.8 the thresholds must be 50/60/80/90 percent.
func TestCompactionTierThresholdsMatchDoc(t *testing.T) {
	cfg := types.DefaultAgentConfig()
	th := cfg.Thresholds()
	if diff := th.Soft - 0.5; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("soft threshold = %v, want 0.5", th.Soft)
	}
	if diff := th.Snip - 0.6; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("snip threshold = %v, want 0.6", th.Snip)
	}
	if diff := th.Compact - 0.8; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("compact threshold = %v, want 0.8", th.Compact)
	}
	if diff := th.Force - 0.9; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("force threshold = %v, want 0.9", th.Force)
	}

	cases := []struct {
		ratio float64
		want  types.CompactionTier
	}{
		{0.10, types.TierNone},
		{0.49, types.TierNone},
		{0.50, types.TierSoft},
		{0.59, types.TierSoft},
		{0.60, types.TierSnip},
		{0.79, types.TierSnip},
		{0.80, types.TierCompact},
		{0.89, types.TierCompact},
		{0.90, types.TierForce},
		{0.99, types.TierForce},
	}
	for _, tc := range cases {
		if got := cfg.TierFor(tc.ratio); got != tc.want {
			t.Errorf("TierFor(%.2f) = %s, want %s", tc.ratio, got, tc.want)
		}
	}
}

// TestCompactorSoftTierDoesNotRewrite checks that the soft tier only notifies.
// Rewriting the prefix to save a few tokens would destroy the provider's prompt
// cache, so it must not happen.
func TestCompactorSoftTierDoesNotRewrite(t *testing.T) {
	cfg := types.DefaultAgentConfig()
	manager := mem.NewContextManager()
	c := NewCompactor(cfg, manager)

	conv := &Conversation{
		Usage:    ports.Usage{PromptTokens: int(0.55 * float64(cfg.ContextWindow))},
		Messages: []ports.Message{{Role: ports.RoleUser, Content: "hi"}},
	}
	d := c.Decide(conv)
	if d.Tier != types.TierSoft {
		t.Fatalf("tier = %s, want soft", d.Tier)
	}
	if !d.NotifyOnly {
		t.Error("the soft tier must be advisory only")
	}
	if d.ShouldRun {
		t.Error("the soft tier must not run the context manager")
	}
	changed, err := c.Apply(context.Background(), conv)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("the soft tier must not change the conversation")
	}
	if manager.CompactionCount() != 0 {
		t.Errorf("context manager ran %d times at the soft tier, want 0", manager.CompactionCount())
	}
}

// TestCompactorLatchesStuckAfterTwoIneffectiveCompactions checks the stuck
// latch: two ineffective compactions in a row stop further attempts.
func TestCompactorLatchesStuckAfterTwoIneffectiveCompactions(t *testing.T) {
	cfg := types.DefaultAgentConfig()
	manager := mem.NewContextManager()
	manager.ForceStuck = true
	c := NewCompactor(cfg, manager)

	conv := &Conversation{
		Usage:    ports.Usage{PromptTokens: int(0.85 * float64(cfg.ContextWindow))},
		Messages: []ports.Message{{Role: ports.RoleSystem, Content: "s"}},
	}

	if _, err := c.Apply(context.Background(), conv); err != nil {
		t.Fatal(err)
	}
	if conv.CompactionStuck {
		t.Fatal("one ineffective compaction must not latch stuck")
	}
	if _, err := c.Apply(context.Background(), conv); err != nil {
		t.Fatal(err)
	}
	if !conv.CompactionStuck {
		t.Fatal("two ineffective compactions should latch stuck")
	}

	// Once stuck, the compactor must stop trying.
	before := manager.CompactionCount()
	if _, err := c.Apply(context.Background(), conv); err != nil {
		t.Fatal(err)
	}
	if manager.CompactionCount() != before {
		t.Error("a stuck compactor kept invoking the context manager")
	}
}

// TestLoopCompactsAtThreshold checks the loop's compaction path end to end.
func TestLoopCompactsAtThreshold(t *testing.T) {
	cfg := types.DefaultAgentConfig()
	cfg.ContextWindow = 1000
	cfg.CompactionRatio = 0.8
	manager := mem.NewContextManager()

	round := mem.ToolCallRound(mem.NewCall("c1", "noop_tool", nil))
	round.Usage = ports.Usage{PromptTokens: 900, CompletionTokens: 50, TotalTokens: 950}
	p := mem.NewProvider(round, mem.FinalRound("done"))

	sink := &recordingSink{}
	loop, err := NewLoop(LoopConfig{
		Config: cfg, Provider: p, Sink: sink,
		Compactor: NewCompactor(cfg, manager),
		Dispatcher: ToolDispatcherFunc(func(_ context.Context, call types.ToolCall) ToolOutcome {
			return ToolOutcome{Call: call, Result: types.ToolResult{
				ToolCallID: call.ID, ToolName: call.Name, Success: true, Content: "ok"}}
		}),
	})
	if err != nil {
		t.Fatal(err)
	}

	conv := NewConversation("", "do it", []types.ToolDefinition{{Name: "noop_tool"}})
	machine := NewMachine("run-compact", "ses-1", nil)
	_ = loop.Run(context.Background(), LoopInput{
		RunID: "run-compact", SessionID: "ses-1",
		Request: types.SubmitRequest{Prompt: "do it"}, Conversation: conv, Machine: machine,
	})

	if manager.CompactionCount() == 0 {
		t.Error("compaction never ran despite usage crossing the threshold")
	}
	if !sink.has(types.EventCompactionStarted) {
		t.Errorf("no compaction event; emitted %v", sink.types())
	}
}

// TestLoopRequiresDependencies checks constructor validation.
func TestLoopRequiresDependencies(t *testing.T) {
	cfg := types.DefaultAgentConfig()
	if _, err := NewLoop(LoopConfig{Config: cfg}); err == nil {
		t.Error("expected an error when no provider is supplied")
	}
	if _, err := NewLoop(LoopConfig{Config: cfg, Provider: mem.NewProvider()}); err == nil {
		t.Error("expected an error when no sink is supplied")
	}
	if _, err := NewLoop(LoopConfig{
		Config: cfg, Provider: mem.NewProvider(), Sink: &recordingSink{},
	}); err == nil {
		t.Error("expected an error when no dispatcher is supplied")
	}
	if _, err := NewLoop(LoopConfig{
		Config: cfg, Provider: mem.NewProvider(), Sink: &recordingSink{},
		Dispatcher: ToolDispatcherFunc(func(context.Context, types.ToolCall) ToolOutcome {
			return ToolOutcome{}
		}),
	}); err != nil {
		t.Errorf("a fully-specified configuration should build: %v", err)
	}
}

// TestLoopIsRaceFreeUnderParallelTools runs the loop with several concurrent
// tool calls so the race detector can inspect the parallel dispatch path.
func TestLoopIsRaceFreeUnderParallelTools(t *testing.T) {
	cfg := types.DefaultAgentConfig()
	calls := make([]types.ToolCall, 0, 8)
	for i := 0; i < 8; i++ {
		calls = append(calls, mem.NewCall("p"+itoa(i), "concurrent_tool", nil))
	}
	p := mem.NewProvider(mem.ToolCallRound(calls...), mem.FinalRound("done"))
	runtime := mem.NewToolRuntime(func(ctx context.Context, req ports.ToolRequest) (types.ToolResult, error) {
		time.Sleep(time.Millisecond)
		return types.ToolResult{Success: true, Content: "ok-" + req.ToolCallID}, nil
	})
	h := newLoopHarness(t, cfg, p, runtime)

	res, _, conv := h.run(t, types.SubmitRequest{Prompt: "run many tools"},
		[]types.ToolDefinition{{Name: "concurrent_tool"}})

	if res.State != types.StateCompleted {
		t.Fatalf("state = %s, want completed (err=%v)", res.State, res.Err)
	}
	if res.ToolCalls != 8 {
		t.Errorf("tool calls = %d, want 8", res.ToolCalls)
	}
	n := 0
	for _, m := range conv.Messages {
		if m.Role == ports.RoleTool {
			n++
		}
	}
	if n != 8 {
		t.Errorf("tool observations = %d, want 8", n)
	}
}

// TestMachineTransitionIsRaceFree exercises the machine from several goroutines
// so the race detector covers the sink-under-lock design.
func TestMachineTransitionIsRaceFree(t *testing.T) {
	var mu sync.Mutex
	count := 0
	m := NewMachine("run-race", "ses-1", TransitionSinkFunc(func(_ context.Context, _ Transition) error {
		mu.Lock()
		count++
		mu.Unlock()
		return nil
	}))
	if err := m.Transition(context.Background(), types.StateQueued, "accepted"); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// thinking -> thinking is legal, so these should succeed; the
			// assertion here is only that nothing races or panics.
			_ = m.Transition(context.Background(), types.StateThinking, "round")
			_ = m.State()
			_ = m.Version()
		}()
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if count == 0 {
		t.Error("no transitions were reported")
	}
}

// fmtAny renders a panic value for an assertion message.
func fmtAny(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return sprint(v)
}
