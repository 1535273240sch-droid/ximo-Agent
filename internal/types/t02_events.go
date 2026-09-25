package types

import (
	"encoding/json"
	"time"
)

// EventType enumerates everything the Engine emits. The set is split into two
// groups by the backpressure policy (architecture doc ch. 7):
//
//	durable  — logical boundaries. Written to the event log, replayed during
//	           recovery, and always delivered to the UI.
//	ephemeral— high-frequency chatter. Never written to the durable log and
//	           coalesced before it reaches the UI.
//
// Adding a durable event type requires updating ReplayEvents in the engine,
// because replay is driven by the type's payload.
type EventType string

const (
	// ---- run lifecycle (durable) ----
	EventRunCreated      EventType = "run.created"
	EventRunQueued       EventType = "run.queued"
	EventRunStarted      EventType = "run.started"
	EventRunStateChanged EventType = "run.state_changed"
	EventRunCompleted    EventType = "run.completed"
	EventRunFailed       EventType = "run.failed"
	EventRunCancelled    EventType = "run.cancelled"
	EventRunRecovering   EventType = "run.recovering"
	EventRunResumed      EventType = "run.resumed"

	// ---- planning (durable) ----
	EventPlanningStarted   EventType = "planning.started"
	EventPlanningCompleted EventType = "planning.completed"
	EventPlanningSkipped   EventType = "planning.skipped"

	// ---- model rounds (durable boundaries, ephemeral deltas) ----
	EventRoundStarted   EventType = "round.started"
	EventRoundCompleted EventType = "round.completed"
	EventFinalAnswer    EventType = "final_answer"

	// ---- tool calls (durable) ----
	EventToolCallRequested      EventType = "tool_call.requested"
	EventToolStarted            EventType = "tool_call.started"
	EventToolCompleted          EventType = "tool_call.completed"
	EventToolFailed             EventType = "tool_call.failed"
	EventToolCancelled          EventType = "tool_call.cancelled"
	EventToolPermissionRequired EventType = "tool_call.permission_required"

	// ---- error / cancellation (durable) ----
	EventError        EventType = "error"
	EventCancellation EventType = "cancellation"

	// ---- checkpointing (durable) ----
	EventCheckpointCreated EventType = "checkpoint.created"
	EventCheckpointLoaded  EventType = "checkpoint.loaded"

	// ---- compaction (durable boundary) ----
	EventCompactionStarted   EventType = "compaction.started"
	EventCompactionCompleted EventType = "compaction.completed"
	EventCompactionSkipped   EventType = "compaction.skipped"

	// ---- supervision (durable: it changes the conversation) ----
	EventSupervision EventType = "supervision"

	// ---- long-task continuations (durable) ----
	EventContinuation EventType = "continuation"

	// ---- waiting for user (durable) ----
	EventUserInputRequired EventType = "user_input.required"
	EventUserInputReceived EventType = "user_input.received"

	// ---- ephemeral (coalescible, never durable) ----
	EventTokenDelta EventType = "token.delta"
	EventHeartbeat  EventType = "heartbeat"
	EventProgress   EventType = "progress"

	// ---- plan mode (task 4, durable) ----
	//
	// Appended after every existing value on purpose: adding an event type in
	// the middle would make two people editing this list collide, and the
	// shared contract for this batch of tasks reserves the names
	// plan.proposed / plan.confirmed / plan.rejected for task 4.
	EventPlanProposed  EventType = "plan.proposed"
	EventPlanConfirmed EventType = "plan.confirmed"
	EventPlanRejected  EventType = "plan.rejected"
)

// durableEvents is the closed set of event types written to the event log.
// A type absent from this set is coalescible UI chatter.
var durableEvents = map[EventType]bool{
	EventRunCreated: true, EventRunQueued: true, EventRunStarted: true,
	EventRunStateChanged: true, EventRunCompleted: true, EventRunFailed: true,
	EventRunCancelled: true, EventRunRecovering: true, EventRunResumed: true,
	EventPlanningStarted: true, EventPlanningCompleted: true, EventPlanningSkipped: true,
	EventRoundStarted: true, EventRoundCompleted: true, EventFinalAnswer: true,
	EventToolCallRequested: true, EventToolStarted: true, EventToolCompleted: true,
	EventToolFailed: true, EventToolCancelled: true, EventToolPermissionRequired: true,
	EventError: true, EventCancellation: true,
	EventCheckpointCreated: true, EventCheckpointLoaded: true,
	EventCompactionStarted: true, EventCompactionCompleted: true, EventCompactionSkipped: true,
	EventSupervision: true, EventContinuation: true,
	EventUserInputRequired: true, EventUserInputReceived: true,
	// Task 4: the plan proposal and the user's decision are logical boundaries —
	// the proposal carries the text the UI renders, and the decision is what
	// makes the run's continuation auditable, so neither may be coalesced away.
	EventPlanProposed: true, EventPlanConfirmed: true, EventPlanRejected: true,
}

// Durable reports whether this event type must be persisted. The event log
// stores logical boundaries only — never one row per streamed character.
func (t EventType) Durable() bool { return durableEvents[t] }

// Mergeable reports whether instances of this type may be coalesced into a
// single UI frame under backpressure (e.g. 1000 token deltas → tens of
// frames). Durable events are never mergeable.
func (t EventType) Mergeable() bool {
	switch t {
	case EventTokenDelta, EventHeartbeat, EventProgress:
		return true
	default:
		return false
	}
}

// Valid reports whether t is a known event type.
func (t EventType) Valid() bool {
	if durableEvents[t] {
		return true
	}
	switch t {
	case EventTokenDelta, EventHeartbeat, EventProgress:
		return true
	default:
		return false
	}
}

// Event is one entry in a run's event stream.
//
// Seq is assigned by the EventStore and is the only ordering authority: the
// EventStore returns it from Append, and Events(afterSeq) resumes from it.
// The engine never invents a sequence number, because a gap or a repeat would
// make crash recovery ambiguous.
type Event struct {
	// Seq is the durable sequence number. Zero means "not yet appended".
	Seq uint64 `json:"seq"`
	// RunID scopes the stream.
	RunID string `json:"runId"`
	// SessionID lets the UI fan a session's events without a second lookup.
	SessionID string    `json:"sessionId,omitempty"`
	Type      EventType `json:"type"`
	// SeqInRun is the run-local monotonic counter used to verify that the
	// recovered sequence has no holes (doc ch. 11: "verify event sequence").
	SeqInRun uint64 `json:"seqInRun"`
	// State is set on run.state_changed and lifecycle events.
	State     RunState `json:"state,omitempty"`
	PrevState RunState `json:"prevState,omitempty"`
	Round     int      `json:"round,omitempty"`
	// ToolCallID / ToolName identify tool-call events.
	ToolCallID string `json:"toolCallId,omitempty"`
	ToolName   string `json:"toolName,omitempty"`
	// CheckpointID is set on checkpoint events.
	CheckpointID string `json:"checkpointId,omitempty"`
	// Data is the type-specific payload. It must never contain credentials.
	Data map[string]any `json:"data,omitempty"`
	// Message is short human-readable text for the UI.
	Message string `json:"message,omitempty"`
	// Err carries a failure detail. It is already redacted.
	Err *Error `json:"err,omitempty"`
	// Coalesced counts how many source events this frame represents. It is >1
	// only for merged ephemeral events.
	Coalesced int       `json:"coalesced,omitempty"`
	Timestamp time.Time `json:"ts"`
}

// ByteSize estimates the serialized footprint of the event, used to enforce
// MaxOutputBytes without a full marshal on the hot path.
func (e *Event) ByteSize() int {
	if e == nil {
		return 0
	}
	n := len(e.RunID) + len(e.SessionID) + len(e.Type) + len(e.Message) +
		len(e.ToolCallID) + len(e.ToolName) + len(e.CheckpointID) + 64
	for k, v := range e.Data {
		n += len(k) + 16
		if s, ok := v.(string); ok {
			n += len(s)
		}
	}
	if e.Err != nil {
		n += len(e.Err.Msg)
	}
	return n
}

// MarshalJSON is provided so that time formatting and the omission of
// zero-valued sequence fields stay consistent between the event log and the
// IPC wire.
func (e Event) MarshalJSON() ([]byte, error) {
	type alias Event
	return json.Marshal(alias(e))
}

// EventStream is the read side returned by Engine.Events.
type EventStream struct {
	// RunID identifies the stream.
	RunID string
	// Ch delivers events in ascending Seq order. It is closed when the run
	// reaches a terminal state or the caller's context ends.
	Ch <-chan Event
	// Cancel releases the stream early. Calling it more than once is safe.
	Cancel func()
}
