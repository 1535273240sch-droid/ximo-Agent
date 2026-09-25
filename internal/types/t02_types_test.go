package types

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// TestRedactStringMasksCredentials is a security test: the redactor is the only
// thing standing between a stray API key and the event log, the crash dump and
// the UI (doc ch. 21), so its behaviour is pinned.
func TestRedactStringMasksCredentials(t *testing.T) {
	cases := []struct {
		name  string
		input string
		leak  string
		// keep is text that must survive, so the message stays diagnosable.
		keep string
	}{
		{"sk- key", "request failed with api_key=sk-abcdef0123456789",
			"sk-abcdef0123456789", "request failed"},
		{"bearer token", "Authorization: Bearer abc123def456",
			"abc123def456", "Authorization"},
		{"bare sk prefix", "key sk-live-9f8e7d6c5b4a",
			"sk-live-9f8e7d6c5b4a", "key"},
		{"token query", "url?access_token=eyJhbGciOiJIUzI1NiJ9",
			"eyJhbGciOiJIUzI1NiJ9", "url?"},
		{"password", "connect failed password=hunter2xyz",
			"hunter2xyz", "connect failed"},
		{"token with quotes", `{"api_key":"sk-quotedsecret99"}`,
			"sk-quotedsecret99", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactString(tc.input)
			if strings.Contains(got, tc.leak) {
				t.Errorf("RedactString(%q) = %q, still contains the credential", tc.input, got)
			}
			if !strings.Contains(got, redactedMarker) {
				t.Errorf("RedactString(%q) = %q, expected a redaction marker", tc.input, got)
			}
			if tc.keep != "" && !strings.Contains(got, tc.keep) {
				t.Errorf("RedactString(%q) = %q, lost context %q", tc.input, got, tc.keep)
			}
		})
	}
}

// TestRedactStringLeavesOrdinaryTextAlone checks that the redactor does not
// mangle prose: over-redaction would make error messages useless.
func TestRedactStringLeavesOrdinaryTextAlone(t *testing.T) {
	inputs := []string{
		"the file has 3 tokens left",
		"transition created -> queued failed",
		"tool terminal timed out after 180s",
		"",
	}
	for _, in := range inputs {
		if got := RedactString(in); got != in {
			t.Errorf("RedactString(%q) = %q, want it unchanged", in, got)
		}
	}
}

// TestNewErrorRedactsAtConstruction checks that a coded error cannot carry a
// credential even if the caller formats one in.
func TestNewErrorRedactsAtConstruction(t *testing.T) {
	err := NewError(CodeProviderFailed, "call failed with api_key=sk-secret123456")
	if strings.Contains(err.Msg, "sk-secret123456") {
		t.Errorf("NewError leaked a credential into the message: %q", err.Msg)
	}
	if strings.Contains(err.Error(), "sk-secret123456") {
		t.Errorf("Error() leaked a credential: %q", err.Error())
	}
}

// TestErrorCodeChainAndIs checks that coded errors compose: errors.Is must match
// on the code, and errors.As must recover the code from a wrapped chain.
func TestErrorCodeChainAndIs(t *testing.T) {
	base := NewError(CodeQueueFull, "queue %d is full", 3)
	if !errors.Is(base, ErrQueueFull) {
		t.Error("errors.Is did not match ErrQueueFull on an equal code")
	}
	if errors.Is(base, ErrNotFound) {
		t.Error("errors.Is matched an unrelated code")
	}

	wrapped := WrapError(CodeQueueFull, base, "submitting task")
	if CodeOf(wrapped) != CodeQueueFull {
		t.Errorf("CodeOf(wrapped) = %s, want %s", CodeOf(wrapped), CodeQueueFull)
	}
	var coded *Error
	if !errors.As(wrapped, &coded) {
		t.Fatal("errors.As did not recover the coded error from the chain")
	}
	if coded.Code != CodeQueueFull {
		t.Errorf("recovered code = %s, want %s", coded.Code, CodeQueueFull)
	}
	if !errors.Unwrap(wrapped).(*Error).Is(base) {
		t.Error("the wrapped error did not unwrap to its cause")
	}

	// A foreign error has no code, and defaults to internal rather than to an
	// empty string that would silently match nothing.
	if got := CodeOf(errors.New("plain error")); got != CodeInternal {
		t.Errorf("CodeOf(foreign error) = %s, want %s", got, CodeInternal)
	}
	if got := CodeOf(nil); got != "" {
		t.Errorf("CodeOf(nil) = %q, want empty", got)
	}
}

// TestWrapErrorWithNilIsNil checks that wrapping a nil error yields nil, so a
// caller can propagate an absent error without a spurious coded error.
func TestWrapErrorWithNilIsNil(t *testing.T) {
	if err := WrapError(CodeInternal, nil, "nothing happened"); err != nil {
		t.Errorf("WrapError(nil) = %v, want nil", err)
	}
}

// TestIsCancelledDetection checks the cancellation classifier used to branch the
// engine's cancellation paths.
func TestIsCancelledDetection(t *testing.T) {
	if !IsCancelled(NewError(CodeCancelled, "stopped")) {
		t.Error("a CodeCancelled error should be reported as cancelled")
	}
	if !IsCancelled(NewError(CodeDeadlineExceeded, "too slow")) {
		t.Error("a CodeDeadlineExceeded error should be reported as cancelled")
	}
	if IsCancelled(NewError(CodeToolFailed, "tool broke")) {
		t.Error("a tool failure is not a cancellation")
	}
	if IsCancelled(nil) {
		t.Error("nil is not a cancellation")
	}
}

// TestCanTransitionToTableMatchesDoc checks the state table directly, including
// the property that terminal states have no outgoing edges.
func TestCanTransitionToTableMatchesDoc(t *testing.T) {
	// Terminal states must be sealed.
	for _, s := range []RunState{StateCompleted, StateCancelled, StateFailed} {
		if !s.Terminal() {
			t.Errorf("%s should be terminal", s)
		}
		for _, to := range AllRunStates {
			if CanTransitionTo(s, to) {
				t.Errorf("terminal state %s should not transition to %s", s, to)
			}
		}
	}
	// Every non-terminal state must have somewhere to go, or a run could stall.
	for _, s := range AllRunStates {
		if s.Terminal() {
			continue
		}
		reachable := false
		for _, to := range AllRunStates {
			if CanTransitionTo(s, to) {
				reachable = true
				break
			}
		}
		if !reachable {
			t.Errorf("non-terminal state %s has no outgoing transition", s)
		}
	}
	// Unknown states are rejected in both directions.
	if CanTransitionTo("bogus", StateQueued) || CanTransitionTo(StateQueued, "bogus") {
		t.Error("an unknown state should not participate in any transition")
	}
	// Every state must be reachable from created, or recovery could strand a run.
	seen := map[RunState]bool{StateCreated: true}
	for i := 0; i < len(AllRunStates); i++ {
		for _, from := range AllRunStates {
			if !seen[from] {
				continue
			}
			for _, to := range AllRunStates {
				if CanTransitionTo(from, to) {
					seen[to] = true
				}
			}
		}
	}
	for _, s := range AllRunStates {
		if !seen[s] {
			t.Errorf("state %s is unreachable from created", s)
		}
	}
}

// TestRunStateValidityAndHelpers checks the state predicates the engine relies
// on to decide whether a run may be resumed.
func TestRunStateValidityAndHelpers(t *testing.T) {
	if !StateThinking.Valid() || RunState("nope").Valid() {
		t.Error("Valid is not distinguishing known states")
	}
	// A waiting_user run is resumable but not terminal: that combination is what
	// lets a parked run be continued.
	if !StateWaitingUser.Resumable() {
		t.Error("waiting_user should be resumable")
	}
	if StateWaitingUser.Terminal() {
		t.Error("waiting_user must not be terminal, or it could never be resumed")
	}
	for _, s := range []RunState{StateCompleted, StateCancelled, StateFailed} {
		if s.Resumable() {
			t.Errorf("terminal state %s must not be resumable", s)
		}
	}
}

// TestPriorityWeightsMatchDoc pins the QoS weights and, with them, the fair
// queue's behaviour.
func TestPriorityWeightsMatchDoc(t *testing.T) {
	want := map[Priority]int{
		PriorityInteractive: 8,
		PriorityNormal:      4,
		PriorityBackground:  1,
		PriorityKnowledge:   1,
	}
	for p, w := range want {
		if got := p.Weight(); got != w {
			t.Errorf("%s weight = %d, want %d", p, got, w)
		}
		if !p.Valid() {
			t.Errorf("%s should be valid", p)
		}
	}
	if Priority("urgent").Valid() {
		t.Error("an unknown priority should not be valid")
	}
	// Weights must be ordered interactive > normal > background, or the queue
	// would not prioritise as documented.
	if !(PriorityInteractive.Weight() > PriorityNormal.Weight() &&
		PriorityNormal.Weight() > PriorityBackground.Weight()) {
		t.Error("priority weights are not ordered interactive > normal > background")
	}
}

// TestResourceCapacitiesMatchDoc pins the third limiting layer's table.
func TestResourceCapacitiesMatchDoc(t *testing.T) {
	want := map[ResourceClass]int{
		ResourceBrowser:          4,
		ResourceComputerUse:      2,
		ResourceTerminal:         16,
		ResourceOffice:           4,
		ResourceMCP:              16,
		ResourceVision:           8,
		ResourceProvider:         32,
		ResourceClassExpertAgent: 8,
	}
	if len(want) != len(AllResourceClasses) {
		t.Errorf("AllResourceClasses has %d entries, want %d", len(AllResourceClasses), len(want))
	}
	for r, cap := range want {
		if got := ResourceCapacities[r]; got != cap {
			t.Errorf("%s capacity = %d, want %d", r, got, cap)
		}
		if !r.Valid() {
			t.Errorf("%s should be valid", r)
		}
	}
}

// TestReasoningEffortMapping checks the effort helpers, including the v1 rule
// that ultra maps onto max at the API boundary.
func TestReasoningEffortMapping(t *testing.T) {
	if !EffortOff.Valid() || ReasoningEffort("turbo").Valid() {
		t.Error("Valid is not distinguishing known efforts")
	}
	if EffortOff.ThinkingEnabled() {
		t.Error("effort off must not request reasoning content")
	}
	for _, e := range []ReasoningEffort{EffortLow, EffortHigh, EffortMax, EffortUltra} {
		if !e.ThinkingEnabled() {
			t.Errorf("effort %s should request reasoning content", e)
		}
	}
	if !EffortUltra.SupervisionEnabled() {
		t.Error("ultra must enable the supervision review")
	}
	for _, e := range []ReasoningEffort{EffortOff, EffortLow, EffortHigh, EffortMax} {
		if e.SupervisionEnabled() {
			t.Errorf("effort %s must not enable supervision", e)
		}
	}
	if got := EffortUltra.APIMapped(); got != EffortMax {
		t.Errorf("ultra maps to %s at the API, want max", got)
	}
	if got := EffortHigh.APIMapped(); got != EffortHigh {
		t.Errorf("high should pass through unchanged, got %s", got)
	}
}

// TestEventDurabilityClassification pins the backpressure split: the durable set
// is what recovery replays, so getting it wrong loses data or bloats the log.
func TestEventDurabilityClassification(t *testing.T) {
	// The events the task book names as "must be preserved".
	mustBeDurable := []EventType{
		EventRunStateChanged, EventToolStarted, EventToolCompleted, EventToolFailed,
		EventFinalAnswer, EventError, EventCheckpointCreated, EventCancellation,
	}
	for _, e := range mustBeDurable {
		if !e.Durable() {
			t.Errorf("%s must be durable: losing it would corrupt recovery or the UI", e)
		}
		if e.Mergeable() {
			t.Errorf("%s must not be mergeable", e)
		}
	}
	// The events the task book names as "may be merged".
	mustBeMergeable := []EventType{EventTokenDelta, EventHeartbeat, EventProgress}
	for _, e := range mustBeMergeable {
		if !e.Mergeable() {
			t.Errorf("%s must be mergeable", e)
		}
		if e.Durable() {
			t.Errorf("%s must not be durable: the log stores boundaries, not every character", e)
		}
	}
	if EventType("bogus").Valid() {
		t.Error("an unknown event type should not be valid")
	}
}

// TestEventByteSizeIsMonotonic checks the accounting used to enforce
// MaxOutputBytes: a bigger payload must never report a smaller size.
func TestEventByteSizeIsMonotonic(t *testing.T) {
	small := &Event{RunID: "r", Type: EventProgress, Data: map[string]any{"content": "x"}}
	large := &Event{RunID: "r", Type: EventProgress, Data: map[string]any{"content": strings.Repeat("x", 1000)}}
	if large.ByteSize() <= small.ByteSize() {
		t.Errorf("a larger event reported a smaller size: %d vs %d", large.ByteSize(), small.ByteSize())
	}
	var nilEvent *Event
	if got := nilEvent.ByteSize(); got != 0 {
		t.Errorf("nil event size = %d, want 0", got)
	}
}

// TestRunCloneIsDeep checks that Clone isolates the mutable slices, so a caller
// cannot mutate engine state through a returned copy.
func TestRunCloneIsDeep(t *testing.T) {
	orig := Run{
		ID: "run-1", State: StateWaitingUser,
		Err:                NewError(CodeProviderFailed, "boom"),
		UncertainToolCalls: []string{"call-1", "call-2"},
	}
	cp := orig.Clone()

	cp.UncertainToolCalls[0] = "mutated"
	cp.Err.Msg = "mutated"
	if orig.UncertainToolCalls[0] != "call-1" {
		t.Error("Clone shares the uncertain-calls slice with the original")
	}
	if orig.Err.Msg != "boom" {
		t.Error("Clone shares the error pointer with the original")
	}
}

// TestSubmitRequestValidationAndDefaults checks the request contract the engine
// relies on before it consumes any capacity.
func TestSubmitRequestValidationAndDefaults(t *testing.T) {
	if err := (SubmitRequest{}).Validate(); err == nil {
		t.Error("an empty prompt must be rejected")
	}
	if err := (SubmitRequest{Prompt: "hi"}).Validate(); err != nil {
		t.Errorf("a minimal request should validate: %v", err)
	}
	if err := (SubmitRequest{Prompt: "hi", Priority: "bogus"}).Validate(); err == nil {
		t.Error("an unknown priority must be rejected")
	}
	if err := (SubmitRequest{Prompt: "hi", Effort: "bogus"}).Validate(); err == nil {
		t.Error("an unknown effort must be rejected")
	}

	got := (SubmitRequest{Prompt: "hi"}).Normalized()
	if got.Priority != PriorityNormal {
		t.Errorf("default priority = %s, want normal", got.Priority)
	}
	if got.Effort != EffortHigh {
		t.Errorf("default effort = %s, want high", got.Effort)
	}
	if got.MaxRounds != DefaultMaxToolRounds {
		t.Errorf("default max rounds = %d, want %d", got.MaxRounds, DefaultMaxToolRounds)
	}
	// An explicit value must survive normalisation.
	custom := (SubmitRequest{Prompt: "hi", Priority: PriorityInteractive, MaxRounds: 7}).Normalized()
	if custom.Priority != PriorityInteractive || custom.MaxRounds != 7 {
		t.Errorf("normalisation overwrote explicit values: %+v", custom)
	}
}

// TestConfigDefaultsMatchDocumentedCaps pins the numbers the task book fixes, so
// a drifting constant fails here rather than in production.
func TestConfigDefaultsMatchDocumentedCaps(t *testing.T) {
	cfg := DefaultEngineConfig()

	if cfg.Scheduler.MaxRunningTasks != 32 {
		t.Errorf("MaxRunningTasks = %d, want the documented 32", cfg.Scheduler.MaxRunningTasks)
	}
	if cfg.Scheduler.MaxConcurrentToolsPerSession != 8 {
		t.Errorf("MaxConcurrentToolsPerSession = %d, want the documented 8",
			cfg.Scheduler.MaxConcurrentToolsPerSession)
	}
	if cfg.Agent.MaxToolRounds != 30 {
		t.Errorf("MaxToolRounds = %d, want the v1 default 30", cfg.Agent.MaxToolRounds)
	}
	if cfg.Agent.LongTaskRoundsPerSegment != 50 {
		t.Errorf("LongTaskRoundsPerSegment = %d, want the v1 default 50", cfg.Agent.LongTaskRoundsPerSegment)
	}
	if cfg.Agent.LongTaskMaxContinuations != 10 {
		t.Errorf("LongTaskMaxContinuations = %d, want the v1 default 10", cfg.Agent.LongTaskMaxContinuations)
	}
	if cfg.Agent.ToolExecTimeout != 180*time.Second {
		t.Errorf("ToolExecTimeout = %s, want the v1 default 180s", cfg.Agent.ToolExecTimeout)
	}
	if cfg.Agent.CompactionRatio != 0.8 {
		t.Errorf("CompactionRatio = %v, want the v1 default 0.8", cfg.Agent.CompactionRatio)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("the default configuration must validate: %v", err)
	}
}

// TestConfigValidationRejectsBadValues checks that a nonsensical limit is
// reported at startup rather than silently disabling a cap.
func TestConfigValidationRejectsBadValues(t *testing.T) {
	// A zero queue cap would silently mean "reject everything", so it must be a
	// configuration error.
	if err := (QueueLimits{}).Validate(); err == nil {
		t.Error("zero limits must be rejected")
	}
	limits := DefaultQueueLimits()
	limits.MaxQueuedRuns = 0
	if err := limits.Validate(); err == nil {
		t.Error("a zero MaxQueuedRuns must be rejected")
	}
	limits = DefaultQueueLimits()
	limits.MaxRunDuration = 0
	if err := limits.Validate(); err == nil {
		t.Error("a zero MaxRunDuration must be rejected")
	}

	sc := DefaultSchedulerConfig()
	sc.MaxRunningTasks = 0
	if err := sc.Validate(); err == nil {
		t.Error("a zero MaxRunningTasks must be rejected")
	}
	sc = DefaultSchedulerConfig()
	sc.ResourceCapacities = map[ResourceClass]int{ResourceBrowser: 0}
	if err := sc.Validate(); err == nil {
		t.Error("a zero resource capacity must be rejected")
	}
	sc = DefaultSchedulerConfig()
	sc.ResourceCapacities = map[ResourceClass]int{"bogus": 4}
	if err := sc.Validate(); err == nil {
		t.Error("an unknown resource class must be rejected")
	}

	ac := DefaultAgentConfig()
	ac.CompactionRatio = 1.5
	if err := ac.Validate(); err == nil {
		t.Error("a compaction ratio above 1 must be rejected")
	}
	ac = DefaultAgentConfig()
	ac.CompactionRatio = 0
	if err := ac.Validate(); err == nil {
		t.Error("a zero compaction ratio must be rejected")
	}

	bc := DefaultBackpressureConfig()
	bc.StreamBuffer = 0
	if err := bc.Validate(); err == nil {
		t.Error("a zero stream buffer must be rejected: it would drop every event")
	}
}

// TestSchedulerConfigCapacitiesIsACopy checks that Capacities does not hand out
// the package-level table, which a caller could then mutate process-wide.
func TestSchedulerConfigCapacitiesIsACopy(t *testing.T) {
	sc := DefaultSchedulerConfig()
	caps := sc.Capacities()
	caps[ResourceBrowser] = 999
	if ResourceCapacities[ResourceBrowser] != 4 {
		t.Error("Capacities returned the shared table; mutating it changed the package default")
	}
	// An explicit override must be honoured.
	sc.ResourceCapacities = map[ResourceClass]int{ResourceBrowser: 2}
	if got := sc.Capacities()[ResourceBrowser]; got != 2 {
		t.Errorf("override not honoured: got %d, want 2", got)
	}
}

// TestCrashKindBoundaryPolicy pins which crash kinds may be contained. This is
// the red-line policy from doc ch. 3.2 expressed as executable data.
func TestCrashKindBoundaryPolicy(t *testing.T) {
	// A core panic must never be recoverable.
	if CrashKindCore.Recoverable() {
		t.Fatal("a core panic must not be recoverable: the process must restart")
	}
	if !CrashKindCore.Core() {
		t.Error("CrashKindCore should report itself as core")
	}
	// The four permitted boundaries must be recoverable.
	for _, k := range []CrashKind{CrashKindTool, CrashKindWorker, CrashKindBackground, CrashKindRPC} {
		if !k.Recoverable() {
			t.Errorf("%s must be recoverable at its boundary", k)
		}
		if k.Core() {
			t.Errorf("%s must not be treated as a core panic", k)
		}
	}
	if CrashKind("bogus").Valid() {
		t.Error("an unknown crash kind should not be valid")
	}
	if CrashKindCore.Valid() == false {
		t.Error("CrashKindCore should be valid")
	}
	// The exit code must be distinct from a clean shutdown so the Supervisor can
	// tell "restart me" from "I finished".
	if ExitCodeCorePanic == 0 {
		t.Error("the core-panic exit code must be non-zero")
	}
}

// TestRunStateEnumerationIsComplete checks that AllRunStates lists exactly the
// documented states: a missing entry would make the transition-table tests
// silently skip it.
func TestRunStateEnumerationIsComplete(t *testing.T) {
	want := []RunState{
		StateCreated, StateQueued, StatePlanning, StateThinking, StateExecuting,
		StateCompacting, StateWaitingUser, StateCompleted, StateCancelled,
		StateFailed, StateRecovering,
	}
	if len(AllRunStates) != len(want) {
		t.Fatalf("AllRunStates has %d entries, want %d", len(AllRunStates), len(want))
	}
	seen := map[RunState]bool{}
	for _, s := range AllRunStates {
		if seen[s] {
			t.Errorf("duplicate state %s in AllRunStates", s)
		}
		seen[s] = true
		if !s.Valid() {
			t.Errorf("AllRunStates lists %s but Valid rejects it", s)
		}
	}
	for _, s := range want {
		if !seen[s] {
			t.Errorf("AllRunStates is missing %s", s)
		}
	}
}

// TestRedactStringHandlesMultipleSecrets checks that every credential in a
// message is masked, not just the first.
func TestRedactStringHandlesMultipleSecrets(t *testing.T) {
	in := "first api_key=sk-aaaabbbbccccc then token=dddddeeeefffff done"
	got := RedactString(in)
	for _, leak := range []string{"sk-aaaabbbbccccc", "dddddeeeefffff"} {
		if strings.Contains(got, leak) {
			t.Errorf("RedactString left %q in %q", leak, got)
		}
	}
	if !strings.Contains(got, "first") || !strings.Contains(got, "done") {
		t.Errorf("RedactString destroyed the message context: %q", got)
	}
}
