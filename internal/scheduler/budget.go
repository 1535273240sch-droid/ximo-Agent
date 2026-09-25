package scheduler

import (
	"sync"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// Budget enforces the hard caps in types.QueueLimits.
//
// Every cap here is a *ceiling*, never a hint: the architecture forbids
// unbounded queues (doc ch. 6.2), so breaching a cap produces a fast
// CodeQueueFull / CodeBudgetExceeded rejection rather than a longer wait.
// Admission control asks the Budget first, before anything is queued.
type Budget struct {
	limits types.QueueLimits

	mu sync.Mutex
	// runQueue and toolQueue are current depths of the two task classes.
	runQueue  int
	toolQueue int
	// memory is the accounted in-flight payload footprint.
	memory int64
	// output accumulates durable event bytes per run so one runaway run
	// cannot exhaust the machine's log volume.
	output map[string]int64
	// started tracks when each run began, for MaxRunDuration.
	started map[string]time.Time
	// ended guards against double-releasing a run's duration slot.
	ended map[string]bool
}

// NewBudget returns a Budget enforcing the given limits.
func NewBudget(limits types.QueueLimits) *Budget {
	return &Budget{
		limits:  limits,
		output:  make(map[string]int64),
		started: make(map[string]time.Time),
		ended:   make(map[string]bool),
	}
}

// Limits returns the configured limits.
func (b *Budget) Limits() types.QueueLimits { return b.limits }

// AdmitQueue checks whether a task of the given kind may join a queue. It is
// the first gate in the chain and does not reserve anything; ReserveQueue
// does that once the task is definitely being enqueued.
func (b *Budget) AdmitQueue(kind types.TaskKind) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch kind {
	case types.TaskKindRun:
		if b.runQueue >= b.limits.MaxQueuedRuns {
			return types.NewError(types.CodeQueueFull,
				"run queue is full (%d/%d)", b.runQueue, b.limits.MaxQueuedRuns)
		}
	case types.TaskKindToolCall:
		if b.toolQueue >= b.limits.MaxQueuedToolCalls {
			return types.NewError(types.CodeQueueFull,
				"tool-call queue is full (%d/%d)", b.toolQueue, b.limits.MaxQueuedToolCalls)
		}
	default:
		return types.NewError(types.CodeInvalidArgument, "unknown task kind %q", string(kind))
	}
	if b.memory >= b.limits.MaxMemoryBytes {
		return types.NewError(types.CodeMemoryLimitExceeded,
			"memory budget exhausted (%d/%d bytes)", b.memory, b.limits.MaxMemoryBytes)
	}
	return nil
}

// ReserveQueue records a task joining a queue. It re-checks the cap so that a
// racing admission cannot oversubscribe: the check and the increment happen
// under one lock.
func (b *Budget) ReserveQueue(kind types.TaskKind) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch kind {
	case types.TaskKindRun:
		if b.runQueue >= b.limits.MaxQueuedRuns {
			return types.NewError(types.CodeQueueFull, "run queue is full (%d/%d)", b.runQueue, b.limits.MaxQueuedRuns)
		}
		b.runQueue++
	case types.TaskKindToolCall:
		if b.toolQueue >= b.limits.MaxQueuedToolCalls {
			return types.NewError(types.CodeQueueFull, "tool-call queue is full (%d/%d)", b.toolQueue, b.limits.MaxQueuedToolCalls)
		}
		b.toolQueue++
	default:
		return types.NewError(types.CodeInvalidArgument, "unknown task kind %q", string(kind))
	}
	return nil
}

// ReleaseQueue records a task leaving a queue, whether it was dispatched or
// cancelled. It is safe to call for an unbalanced release; the counter floors
// at zero rather than going negative, because a negative depth would silently
// disable the cap.
func (b *Budget) ReleaseQueue(kind types.TaskKind) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch kind {
	case types.TaskKindRun:
		if b.runQueue > 0 {
			b.runQueue--
		}
	case types.TaskKindToolCall:
		if b.toolQueue > 0 {
			b.toolQueue--
		}
	}
}

// ReserveMemory accounts n bytes of in-flight payload. Callers must pair a
// successful reservation with ReleaseMemory.
func (b *Budget) ReserveMemory(n int64) error {
	if n < 0 {
		return types.NewError(types.CodeInvalidArgument, "cannot reserve negative memory (%d)", n)
	}
	if n == 0 {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.memory+n > b.limits.MaxMemoryBytes {
		return types.NewError(types.CodeMemoryLimitExceeded,
			"memory budget exceeded: %d + %d > %d bytes", b.memory, n, b.limits.MaxMemoryBytes)
	}
	b.memory += n
	return nil
}

// ReleaseMemory returns n bytes to the memory budget.
func (b *Budget) ReleaseMemory(n int64) {
	if n <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.memory -= n
	if b.memory < 0 {
		b.memory = 0
	}
}

// RecordOutput accounts durable event bytes for a run. It returns an error
// once the run exceeds MaxOutputBytes, which the engine treats as a terminal
// failure: continuing would write an unbounded log.
func (b *Budget) RecordOutput(runID string, n int64) error {
	if n <= 0 {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	total := b.output[runID] + n
	b.output[runID] = total
	if total > b.limits.MaxOutputBytes {
		return types.NewError(types.CodeOutputLimitExceeded,
			"run %s exceeded output budget: %d > %d bytes", runID, total, b.limits.MaxOutputBytes)
	}
	return nil
}

// OutputBytes reports a run's accounted output volume.
func (b *Budget) OutputBytes(runID string) int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.output[runID]
}

// StartRun marks a run as started so its wall-clock budget can be enforced.
// Calling it twice for the same run keeps the first timestamp.
func (b *Budget) StartRun(runID string, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.started[runID]; ok {
		return
	}
	b.started[runID] = now
	delete(b.ended, runID)
}

// CheckDuration reports whether a run has exceeded MaxRunDuration. It returns
// the remaining time on success so callers can size a context deadline.
func (b *Budget) CheckDuration(runID string, now time.Time) (time.Duration, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	start, ok := b.started[runID]
	if !ok {
		return b.limits.MaxRunDuration, nil
	}
	elapsed := now.Sub(start)
	if elapsed >= b.limits.MaxRunDuration {
		return 0, types.NewError(types.CodeRunDurationExceeded,
			"run %s exceeded max duration %s (elapsed %s)", runID, b.limits.MaxRunDuration, elapsed)
	}
	return b.limits.MaxRunDuration - elapsed, nil
}

// Deadline returns the wall-clock instant at which a run's duration budget
// expires, for use as a context deadline.
func (b *Budget) Deadline(runID string) time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	start, ok := b.started[runID]
	if !ok {
		start = time.Now()
	}
	return start.Add(b.limits.MaxRunDuration)
}

// ForgetRun releases every per-run reservation. The engine calls it when a run
// reaches a terminal state; without it the output and started maps would grow
// without bound over the process lifetime.
func (b *Budget) ForgetRun(runID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.output, runID)
	delete(b.started, runID)
	delete(b.ended, runID)
}

// Snapshot returns the current accounting, for Stats and for tests.
type BudgetSnapshot struct {
	RunQueue  int
	ToolQueue int
	Memory    int64
	Runs      int
}

// Snapshot reports current usage.
func (b *Budget) Snapshot() BudgetSnapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	return BudgetSnapshot{
		RunQueue:  b.runQueue,
		ToolQueue: b.toolQueue,
		Memory:    b.memory,
		Runs:      len(b.started),
	}
}
