package types

import "context"

// This file declares the two cross-task interfaces from architecture doc ch. 32
// verbatim.
//
// They live in internal/types rather than in the engine package because the
// consumers are other tasks, not this one: task 01's UI host and task 07's tests
// call the Engine surface, and nothing in them should have to import the
// engine's internals (or risk an import cycle) to hold a reference to it. The
// concrete types in internal/engine and internal/scheduler satisfy these
// interfaces, verified by a compile-time assertion at each definition site.
//
// The declarations are also the place task 08 reconciles any signature drift:
// changing a method here forces every implementation and every caller to agree.

// Engine is the run lifecycle surface.
//
// Contract notes for callers:
//
//   - Submit rejects fast rather than queueing without bound: a full machine
//     produces a CodeAdmissionRejected or CodeQueueFull error, never a hang.
//   - Cancel is idempotent for a run that already finished: it reports
//     CodeTerminalState rather than pretending to have stopped something.
//   - Resume is required after a crash for any run whose parked state came from
//     CodeAwaitingUser; it is never implicit.
//   - Events delivers in ascending sequence order and closes when the run
//     reaches a terminal state. A reconnecting caller passes the last sequence
//     it saw as afterSeq and receives the gap with no duplicates.
type Engine interface {
	// Submit accepts a run and returns a handle immediately.
	Submit(ctx context.Context, req SubmitRequest) (RunHandle, error)
	// Cancel stops a run, cancelling its in-flight tool calls too.
	Cancel(ctx context.Context, runID string) error
	// Resume continues a run parked in waiting_user.
	Resume(ctx context.Context, runID string) error
	// GetRun returns a run's current materialized state.
	GetRun(ctx context.Context, runID string) (Run, error)
	// Events streams a run's events, resuming after the given sequence.
	Events(ctx context.Context, runID string, afterSeq uint64) (<-chan Event, error)
}

// Scheduler is the three-layer rate limiter's control surface.
//
// Queueing is bounded at every layer by design (doc ch. 6.2): Submit returns a
// capacity error instead of growing a queue, and Stats exists so a caller can
// see which layer is saturated.
type Scheduler interface {
	// Submit queues a task, rejecting it fast when a cap is reached.
	Submit(task Task) error
	// Cancel cancels a queued or running task.
	Cancel(taskID string) error
	// Stats snapshots all three layers.
	Stats() SchedulerStats
}

// Recoverer is the optional recovery surface, expressed in wire types.
//
// It is separate from Engine so a UI host cannot accidentally trigger recovery
// by calling a method it already holds. The method is named RecoverForIPC rather
// than Recover because the concrete engine also exposes a richer Recover whose
// plan type carries engine-internal detail; giving the wire form its own name
// keeps both available without a method-name collision.
type Recoverer interface {
	// RecoverForIPC inspects runs left in flight by a dead process, acts on each
	// plan, and returns the plans in their serializable form.
	RecoverForIPC(ctx context.Context) ([]RecoveryPlan, error)
}

// RecoveryPlan is re-exported in a shape the IPC boundary can carry.
//
// It is declared here, rather than only in internal/engine, because task 01
// needs to serialize it over IPC and the engine package's own type is not
// importable from the supervisor without a dependency on the whole engine.
type RecoveryPlan struct {
	RunID     string `json:"runId"`
	SessionID string `json:"sessionId,omitempty"`
	// Decision is one of resume_auto / resume_after_confirm / mark_failed / skip.
	Decision string `json:"decision"`
	// ResumeState is the state the run was in when it died.
	ResumeState RunState `json:"resumeState"`
	LastSeq     uint64   `json:"lastSeq"`
	// CheckpointSeq is the sequence of the last safe checkpoint, or 0.
	CheckpointSeq uint64 `json:"checkpointSeq"`
	Round         int    `json:"round"`
	// UncertainToolCalls lists non-idempotent calls whose outcome is unknown and
	// which therefore require user confirmation before any replay.
	UncertainToolCalls []string `json:"uncertainToolCalls,omitempty"`
	// ReplayableToolCalls lists incomplete calls that are safe to replay.
	ReplayableToolCalls []string `json:"replayableToolCalls,omitempty"`
	// SequenceOK reports whether the event log had no gaps.
	SequenceOK bool `json:"sequenceOk"`
	// Notes explains the decision, for the UI and for the integration report.
	Notes []string `json:"notes,omitempty"`
}

// Recovery decision values, mirrored from internal/engine so the IPC layer can
// switch on them without importing the engine.
const (
	DecisionResumeAuto         = "resume_auto"
	DecisionResumeAfterConfirm = "resume_after_confirm"
	DecisionMarkFailed         = "mark_failed"
	DecisionSkip               = "skip"
)
