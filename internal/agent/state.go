package agent

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/ports"
	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// Transition describes one run-state change. It is the payload the engine
// needs to fulfil the four obligations the architecture attaches to every
// transition (doc ch. 4): append an event, update the materialized run record,
// push a UI event and, for externally observable states, write the outbox.
type Transition struct {
	RunID     string
	SessionID string
	From      types.RunState
	To        types.RunState
	Round     int
	Reason    string
	// WaitingReason is set when To is waiting_user.
	WaitingReason string
	// UncertainToolCalls is set when To is waiting_user because recovery found
	// non-idempotent calls of unknown outcome.
	UncertainToolCalls []string
	At                 time.Time
}

// TransitionSink receives every accepted transition. The engine supplies an
// implementation that persists the event and fans it out to subscribers.
//
// The sink is called while the machine's lock is held, so it must not call
// back into the Machine (that would deadlock) and must not block for long.
type TransitionSink interface {
	OnTransition(ctx context.Context, t Transition) error
}

// Machine is the run state machine from doc ch. 4.
//
// It owns the current state and refuses any transition the documented table
// does not permit, which is what makes recovery trustworthy: a resumed run
// cannot be driven into a state it could never have reached organically.
type Machine struct {
	mu    sync.Mutex
	runID string
	// sessionID is carried into transitions for the UI fan-out.
	sessionID string
	state     types.RunState
	round     int
	// version increments on every accepted transition.
	version uint64
	// history records accepted transitions for debugging and for recovery
	// verification. It is bounded: only the tail is retained, because a long
	// run would otherwise grow it without limit.
	history []Transition
	// sink receives accepted transitions; may be nil.
	sink TransitionSink
	// now is injectable so tests get deterministic timestamps.
	now func() time.Time
	// closed marks a terminal state, after which no transition is accepted.
	closed bool
}

// maxHistory bounds the in-memory transition history.
const maxHistory = 256

// NewMachine creates a machine in the created state.
func NewMachine(runID, sessionID string, sink TransitionSink) *Machine {
	return &Machine{
		runID:     runID,
		sessionID: sessionID,
		state:     types.StateCreated,
		sink:      sink,
		now:       time.Now,
	}
}

// NewMachineAt creates a machine resuming from a known state. Recovery uses it
// after replaying the durable event log: the machine starts in the recorded
// state without re-emitting the transitions that led there.
func NewMachineAt(runID, sessionID string, state types.RunState, round int, version uint64, sink TransitionSink) *Machine {
	m := NewMachine(runID, sessionID, sink)
	m.state = state
	m.round = round
	m.version = version
	m.closed = state.Terminal()
	return m
}

// State returns the current state.
func (m *Machine) State() types.RunState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

// Round returns the current round counter.
func (m *Machine) Round() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.round
}

// SetRound records the model round, used to stamp events.
func (m *Machine) SetRound(r int) {
	m.mu.Lock()
	m.round = r
	m.mu.Unlock()
}

// Version returns the transition counter.
func (m *Machine) Version() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.version
}

// Terminal reports whether the machine has reached a terminal state.
func (m *Machine) Terminal() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}

// History returns a copy of the retained transition history.
func (m *Machine) History() []Transition {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Transition(nil), m.history...)
}

// SetClock replaces the time source. It exists for tests; production code
// never needs it.
func (m *Machine) SetClock(now func() time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if now != nil {
		m.now = now
	}
}

// Transition attempts to move to the target state.
//
// It fails with CodeInvalidTransition when the documented table forbids the
// move, and with CodeTerminalState once a terminal state has been reached —
// a completed run must not be resurrected by a late callback.
//
// On success the sink is invoked exactly once, while the lock is held, so the
// durable event, the materialized state and the UI notification cannot be
// reordered relative to each other.
func (m *Machine) Transition(ctx context.Context, to types.RunState, reason string) error {
	return m.TransitionTo(ctx, to, Transition{Reason: reason})
}

// TransitionTo is Transition with extra payload, used by the waiting_user path
// where the reason and the uncertain tool calls must travel with the event.
func (m *Machine) TransitionTo(ctx context.Context, to types.RunState, extra Transition) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return types.NewError(types.CodeTerminalState,
			"run %s is in terminal state %q and cannot move to %q", m.runID, string(m.state), string(to))
	}
	if !to.Valid() {
		return types.NewError(types.CodeInvalidTransition, "unknown target state %q", string(to))
	}
	if m.state == to {
		// A same-state transition is legal only where the table lists it
		// explicitly (consecutive thinking rounds). Otherwise it is a caller
		// bug and is reported rather than silently emitted, so the event log
		// does not fill with no-op transitions.
		if !types.CanTransitionTo(m.state, to) {
			return types.NewError(types.CodeInvalidTransition,
				"run %s is already in %q", m.runID, string(to))
		}
	}

	if !types.CanTransitionTo(m.state, to) {
		return types.NewError(types.CodeInvalidTransition,
			"illegal transition %q → %q for run %s", string(m.state), string(to), m.runID)
	}

	t := extra
	t.RunID = m.runID
	t.SessionID = m.sessionID
	t.From = m.state
	t.To = to
	t.Round = m.round
	if t.At.IsZero() {
		t.At = m.now()
	}

	m.state = to
	m.version++
	if to.Terminal() {
		m.closed = true
	}
	m.history = append(m.history, t)
	if len(m.history) > maxHistory {
		m.history = append([]Transition(nil), m.history[len(m.history)-maxHistory:]...)
	}

	if m.sink != nil {
		// The sink runs under the lock by design; see the doc comment.
		if err := m.sink.OnTransition(ctx, t); err != nil {
			// The state change already happened. Reporting the error without
			// rolling back is deliberate: rolling back would leave the durable
			// log and the in-memory state disagreeing, whereas a failed sink
			// surfaces as a missing UI notification that recovery can fill in
			// from the log.
			return types.WrapError(types.CodeOf(err), err,
				"state transition %q → %q recorded but sink failed", string(t.From), string(t.To))
		}
	}
	return nil
}

// MustTransition is Transition for call sites where failure is a programming
// error, such as an internal shutdown path. It panics in a core goroutine,
// which the panic boundary converts into a crash dump plus restart; that is
// the correct outcome for an impossible transition.
func (m *Machine) MustTransition(ctx context.Context, to types.RunState, reason string) {
	if err := m.Transition(ctx, to, reason); err != nil {
		panic(fmt.Sprintf("agent: impossible state transition: %v", err))
	}
}

// CanTransitionTo reports whether the machine may currently move to a state.
func (m *Machine) CanTransitionTo(to types.RunState) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return false
	}
	return types.CanTransitionTo(m.state, to)
}

// ---------------------------------------------------------------------------
// conversation state
// ---------------------------------------------------------------------------

// Conversation is the append-only message history plus the per-run counters
// the loop mutates. It is intentionally not concurrency-safe: exactly one
// goroutine (the run's loop) owns it, mirroring the SessionActor rule that one
// writer owns a session's state (doc ch. 5).
type Conversation struct {
	// Messages is append-only for the life of a run. Compaction may replace a
	// middle region with a summary, which is the single exception.
	Messages []ports.Message
	// Usage accumulates token accounting across rounds.
	Usage ports.Usage
	// Rounds counts model rounds completed.
	Rounds int
	// Continuations counts long-task segments started.
	Continuations int
	// Tools is the live tool catalogue. It shrinks when the planning phase
	// narrows it, grows when create_tool succeeds, and becomes empty when the
	// loop forces a final text-only summary.
	Tools []types.ToolDefinition
	// Plan holds the planning phase output, when one ran.
	Plan *Plan
	// LastAnswer is the most recent assistant text, used as the run's answer
	// when the loop ends without a dedicated final round.
	LastAnswer string
	// Reviews accumulates ultra-mode supervision verdicts.
	Reviews []Review
	// CompactionStuck latches when compaction can no longer free space, so the
	// loop stops trying (v1's compactStuck).
	CompactionStuck bool
	// ConsecutiveCompacts counts back-to-back compaction rounds without a
	// material reduction, feeding the stuck latch.
	ConsecutiveCompacts int
	// SoftNoticed records that the soft-tier warning was already emitted.
	SoftNoticed bool
	// toolCallCount totals the tool calls dispatched across the run, used for
	// the run summary and for metrics.
	toolCallCount int
}

// ToolCallCount reports how many tool calls the run has dispatched.
func (c *Conversation) ToolCallCount() int { return c.toolCallCount }

// NewConversation builds the initial conversation with the system prompt and
// the user's task.
func NewConversation(systemPrompt, prompt string, tools []types.ToolDefinition) *Conversation {
	c := &Conversation{Tools: append([]types.ToolDefinition(nil), tools...)}
	if systemPrompt != "" {
		c.Messages = append(c.Messages, ports.Message{Role: ports.RoleSystem, Content: systemPrompt})
	}
	c.Messages = append(c.Messages, ports.Message{Role: ports.RoleUser, Content: prompt})
	return c
}

// Append adds a message to the history.
func (c *Conversation) Append(m ports.Message) { c.Messages = append(c.Messages, m) }

// AppendSystem adds a system-role message, the mechanism the loop uses to
// steer the model (plan acknowledgement, correction prompts, wrap-up orders).
func (c *Conversation) AppendSystem(content string) {
	c.Messages = append(c.Messages, ports.Message{Role: ports.RoleSystem, Content: content})
}

// AppendToolResult adds a tool observation paired to its call ID. DeepSeek
// requires the pairing, so an orphaned result is a protocol error.
func (c *Conversation) AppendToolResult(toolCallID, content string) {
	c.Messages = append(c.Messages, ports.Message{
		Role: ports.RoleTool, Content: content, ToolCallID: toolCallID,
	})
}

// AppendAssistant records an assistant turn, round-tripping reasoning content
// exactly as v1 does. Reasoning must be preserved verbatim for every assistant
// turn while tools are present, or the provider rejects the request with 400.
func (c *Conversation) AppendAssistant(content, reasoning string, calls []types.ToolCall, keepReasoning bool) {
	m := ports.Message{Role: ports.RoleAssistant, Content: content, ToolCalls: calls}
	if keepReasoning {
		m.ReasoningContent = reasoning
	}
	c.Messages = append(c.Messages, m)
}

// LastUser returns the most recent user message content, which the planning
// trigger inspects for its length heuristic.
func (c *Conversation) LastUser() string {
	for i := len(c.Messages) - 1; i >= 0; i-- {
		if c.Messages[i].Role == ports.RoleUser {
			return c.Messages[i].Content
		}
	}
	return ""
}

// Clone returns a deep copy, used to hand a snapshot to the context manager
// without letting it mutate the live conversation.
func (c *Conversation) Clone() *Conversation {
	cp := &Conversation{
		Usage:               c.Usage,
		Rounds:              c.Rounds,
		Continuations:       c.Continuations,
		Plan:                c.Plan,
		LastAnswer:          c.LastAnswer,
		CompactionStuck:     c.CompactionStuck,
		ConsecutiveCompacts: c.ConsecutiveCompacts,
		SoftNoticed:         c.SoftNoticed,
	}
	cp.Messages = append([]ports.Message(nil), c.Messages...)
	cp.Tools = append([]types.ToolDefinition(nil), c.Tools...)
	cp.Reviews = append([]Review(nil), c.Reviews...)
	return cp
}

// UsageRatio returns prompt tokens as a fraction of the context window. It is
// the input to the compaction decision.
func (c *Conversation) UsageRatio(window int) float64 {
	if window <= 0 {
		return 0
	}
	return float64(c.Usage.PromptTokens) / float64(window)
}
