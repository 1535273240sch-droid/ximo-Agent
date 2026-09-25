package types

import "time"

// Limits and defaults. The documented numbers come from architecture doc
// ch. 6 (three-layer rate limiting), ch. 4 (round caps) and the v1 constants
// in src/main/deepseek/context.ts; the task book fixes the first layer at
// MaxRunningTasks=32 and MaxConcurrentToolsPerSession=8.
const (
	// ---- layer 1: global admission ----
	//
	// MaxRunningTasks caps concurrently running *runs* process-wide. It is the
	// outermost gate: past this point submission fails fast rather than
	// queueing without bound.
	DefaultMaxRunningTasks = 32

	// ---- layer 2: per-session ----
	//
	// MaxConcurrentToolsPerSession caps tool calls in flight inside one
	// session. Tools of a single session fan out to workers, but never wider
	// than this.
	DefaultMaxConcurrentToolsPerSession = 8

	// ---- hard queue caps (reject fast, never queue to OOM) ----
	DefaultMaxQueuedRuns      = 4096
	DefaultMaxQueuedToolCalls = 8192

	// ---- round caps, from v1 context.ts ----
	//
	// DefaultMaxToolRounds is the flat Think→Tool→Observe round cap.
	DefaultMaxToolRounds = 30
	// DefaultLongTaskRoundsPerSegment is the round budget of one long-task
	// segment; on exhaustion the loop continues with a fresh segment.
	DefaultLongTaskRoundsPerSegment = 50
	// DefaultLongTaskMaxContinuations bounds the segment count, giving a
	// hard ceiling of 50 × (1+10) = 550 rounds.
	DefaultLongTaskMaxContinuations = 10

	// ---- context budget, from v1 context.ts ----
	DefaultContextWindow      = 1_000_000
	DefaultCompactionRatio    = 0.8
	DefaultMaxToolResultChars = 8000
	DefaultMaxContextChars    = 300_000
	// DefaultRecentKeep is how many trailing turns compaction protects.
	DefaultRecentKeep = 5

	// ---- timeouts, from v1 ----
	//
	// DefaultToolExecTimeout is the per-tool-call safety net. Tools that hang
	// (subprocess never exits, socket never closes) are cut off here instead
	// of stalling the round forever.
	DefaultToolExecTimeout = 180 * time.Second
	// DefaultProviderTimeout bounds a single model round.
	DefaultProviderTimeout = 120 * time.Second
	// DefaultProviderIdleTimeout mirrors v1's 60s stream idle watchdog.
	DefaultProviderIdleTimeout = 60 * time.Second
	// DefaultConfirmTimeout is how long a permission prompt waits for the
	// user before defaulting to deny.
	DefaultConfirmTimeout = 60 * time.Second
	// DefaultUserInputTimeout is the long-form equivalent for plan questions.
	DefaultUserInputTimeout = 120 * time.Second
	// DefaultSupervisionTimeout is the hard cap on the ultra-mode review.
	DefaultSupervisionTimeout = 45 * time.Second
	// DefaultRunDuration caps a whole run (QueueLimits.MaxRunDuration).
	DefaultRunDuration = 2 * time.Hour
)

// QueueLimits bundles the per-queue hard caps. Every field has a hard ceiling
// by design: the architecture forbids unbounded queues (doc ch. 6.2).
type QueueLimits struct {
	// MaxQueuedRuns limits waiting run tasks.
	MaxQueuedRuns int
	// MaxQueuedToolCalls limits waiting tool-call tasks.
	MaxQueuedToolCalls int
	// MaxMemoryBytes limits the engine's accounted in-flight payload memory.
	MaxMemoryBytes int64
	// MaxOutputBytes limits cumulative event bytes per run.
	MaxOutputBytes int64
	// MaxRunDuration limits wall-clock time per run.
	MaxRunDuration time.Duration
}

// DefaultQueueLimits returns the documented limits.
func DefaultQueueLimits() QueueLimits {
	return QueueLimits{
		MaxQueuedRuns:      DefaultMaxQueuedRuns,
		MaxQueuedToolCalls: DefaultMaxQueuedToolCalls,
		MaxMemoryBytes:     512 << 20, // 512 MiB
		MaxOutputBytes:     64 << 20,  // 64 MiB
		MaxRunDuration:     DefaultRunDuration,
	}
}

// Validate rejects nonsensical limit configurations early. A zero cap would
// silently mean "reject everything", so it is treated as a configuration bug.
func (l QueueLimits) Validate() error {
	if l.MaxQueuedRuns <= 0 {
		return NewError(CodeInvalidArgument, "MaxQueuedRuns must be positive, got %d", l.MaxQueuedRuns)
	}
	if l.MaxQueuedToolCalls <= 0 {
		return NewError(CodeInvalidArgument, "MaxQueuedToolCalls must be positive, got %d", l.MaxQueuedToolCalls)
	}
	if l.MaxMemoryBytes <= 0 {
		return NewError(CodeInvalidArgument, "MaxMemoryBytes must be positive, got %d", l.MaxMemoryBytes)
	}
	if l.MaxOutputBytes <= 0 {
		return NewError(CodeInvalidArgument, "MaxOutputBytes must be positive, got %d", l.MaxOutputBytes)
	}
	if l.MaxRunDuration <= 0 {
		return NewError(CodeInvalidArgument, "MaxRunDuration must be positive, got %s", l.MaxRunDuration)
	}
	return nil
}

// SchedulerConfig configures the three-layer scheduler.
type SchedulerConfig struct {
	// MaxRunningTasks is layer 1.
	MaxRunningTasks int
	// MaxConcurrentToolsPerSession is layer 2.
	MaxConcurrentToolsPerSession int
	// Limits holds the hard queue caps.
	Limits QueueLimits
	// ResourceCapacities overrides the default resource table. Nil uses
	// types.ResourceCapacities.
	ResourceCapacities map[ResourceClass]int
}

// DefaultSchedulerConfig returns the documented configuration.
func DefaultSchedulerConfig() SchedulerConfig {
	return SchedulerConfig{
		MaxRunningTasks:              DefaultMaxRunningTasks,
		MaxConcurrentToolsPerSession: DefaultMaxConcurrentToolsPerSession,
		Limits:                       DefaultQueueLimits(),
	}
}

// Validate rejects invalid scheduler configurations.
func (c SchedulerConfig) Validate() error {
	if c.MaxRunningTasks <= 0 {
		return NewError(CodeInvalidArgument, "MaxRunningTasks must be positive, got %d", c.MaxRunningTasks)
	}
	if c.MaxConcurrentToolsPerSession <= 0 {
		return NewError(CodeInvalidArgument, "MaxConcurrentToolsPerSession must be positive, got %d", c.MaxConcurrentToolsPerSession)
	}
	if err := c.Limits.Validate(); err != nil {
		return err
	}
	for r, cap := range c.ResourceCapacities {
		if !r.Valid() {
			return NewError(CodeInvalidArgument, "unknown resource class %q", string(r))
		}
		if cap <= 0 {
			return NewError(CodeInvalidArgument, "resource %q capacity must be positive, got %d", string(r), cap)
		}
	}
	return nil
}

// Capacities returns the effective resource capacity table.
func (c SchedulerConfig) Capacities() map[ResourceClass]int {
	if c.ResourceCapacities != nil {
		return c.ResourceCapacities
	}
	out := make(map[ResourceClass]int, len(ResourceCapacities))
	for k, v := range ResourceCapacities {
		out[k] = v
	}
	return out
}

// BackpressureConfig controls how ephemeral events are coalesced. The task
// book's example is 1000 token deltas folding into 20–50 UI frames.
type BackpressureConfig struct {
	// FrameInterval is how often the coalescer flushes to the UI.
	FrameInterval time.Duration
	// MaxCoalesce is the largest number of source events one frame may carry
	// before a flush is forced.
	MaxCoalesce int
	// StreamBuffer is the per-subscriber buffer depth. A subscriber that
	// cannot keep up is dropped rather than allowed to block the actor: an
	// unbounded channel here is exactly the OOM the doc forbids.
	StreamBuffer int
	// MaxSubscribers bounds concurrent subscribers per engine.
	MaxSubscribers int
}

// DefaultBackpressureConfig returns the documented backpressure settings.
// 50ms with a 32-event fold turns 1000 deltas into roughly 20–50 frames, which
// matches the chapter 7 guidance.
func DefaultBackpressureConfig() BackpressureConfig {
	return BackpressureConfig{
		FrameInterval:  50 * time.Millisecond,
		MaxCoalesce:    32,
		StreamBuffer:   256,
		MaxSubscribers: 1024,
	}
}

// Validate rejects invalid backpressure settings.
func (c BackpressureConfig) Validate() error {
	if c.FrameInterval <= 0 {
		return NewError(CodeInvalidArgument, "FrameInterval must be positive, got %s", c.FrameInterval)
	}
	if c.MaxCoalesce <= 0 {
		return NewError(CodeInvalidArgument, "MaxCoalesce must be positive, got %d", c.MaxCoalesce)
	}
	if c.StreamBuffer <= 0 {
		return NewError(CodeInvalidArgument, "StreamBuffer must be positive, got %d", c.StreamBuffer)
	}
	if c.MaxSubscribers <= 0 {
		return NewError(CodeInvalidArgument, "MaxSubscribers must be positive, got %d", c.MaxSubscribers)
	}
	return nil
}

// AgentConfig is the agent-loop tuning surface. Field-for-field it mirrors v1
// agentConfig in src/main/deepseek/context.ts so behaviour parity can be
// checked by comparing values rather than reading two loops.
type AgentConfig struct {
	MaxToolRounds            int
	MaxToolResultChars       int
	MaxContextChars          int
	RecentKeep               int
	PlanningEnabled          bool
	ContextWindow            int
	CompactionRatio          float64
	LongTaskRoundsPerSegment int
	LongTaskMaxContinuations int
	ToolExecTimeout          time.Duration
	SupervisionTimeout       time.Duration
	// StopAfterAllTodosDone reproduces the v1 behaviour of stripping tools and
	// forcing a final summary once todo_write reports everything complete.
	StopAfterAllTodosDone bool
}

// DefaultAgentConfig returns the v1-equivalent defaults.
func DefaultAgentConfig() AgentConfig {
	return AgentConfig{
		MaxToolRounds:            DefaultMaxToolRounds,
		MaxToolResultChars:       DefaultMaxToolResultChars,
		MaxContextChars:          DefaultMaxContextChars,
		RecentKeep:               DefaultRecentKeep,
		PlanningEnabled:          true,
		ContextWindow:            DefaultContextWindow,
		CompactionRatio:          DefaultCompactionRatio,
		LongTaskRoundsPerSegment: DefaultLongTaskRoundsPerSegment,
		LongTaskMaxContinuations: DefaultLongTaskMaxContinuations,
		ToolExecTimeout:          DefaultToolExecTimeout,
		SupervisionTimeout:       DefaultSupervisionTimeout,
		StopAfterAllTodosDone:    true,
	}
}

// Validate rejects invalid agent configurations.
func (c AgentConfig) Validate() error {
	if c.MaxToolRounds <= 0 {
		return NewError(CodeInvalidArgument, "MaxToolRounds must be positive, got %d", c.MaxToolRounds)
	}
	if c.LongTaskRoundsPerSegment <= 0 {
		return NewError(CodeInvalidArgument, "LongTaskRoundsPerSegment must be positive, got %d", c.LongTaskRoundsPerSegment)
	}
	if c.LongTaskMaxContinuations < 0 {
		return NewError(CodeInvalidArgument, "LongTaskMaxContinuations must not be negative, got %d", c.LongTaskMaxContinuations)
	}
	if c.ContextWindow <= 0 {
		return NewError(CodeInvalidArgument, "ContextWindow must be positive, got %d", c.ContextWindow)
	}
	if c.CompactionRatio <= 0 || c.CompactionRatio > 1 {
		return NewError(CodeInvalidArgument, "CompactionRatio must be in (0,1], got %v", c.CompactionRatio)
	}
	if c.ToolExecTimeout <= 0 {
		return NewError(CodeInvalidArgument, "ToolExecTimeout must be positive, got %s", c.ToolExecTimeout)
	}
	return nil
}

// CompactionTier is the four-level context-compaction ladder from v1
// shared/cache/context-manager.ts. Thresholds are derived from
// CompactionRatio: soft=0.625r, snip=0.75r, compact=1.0r, force=1.125r,
// which with the default 0.8 yields 50%/60%/80%/90% of the window.
type CompactionTier string

const (
	// TierNone means usage is below the soft threshold.
	TierNone CompactionTier = "none"
	// TierSoft means context is growing: notify, do not modify the prefix
	// (rewriting it would destroy the provider prompt cache).
	TierSoft CompactionTier = "soft"
	// TierSnip means mechanically clip old tool outputs in place.
	TierSnip CompactionTier = "snip"
	// TierCompact means summarise and replace the middle region.
	TierCompact CompactionTier = "compact"
	// TierForce means the same as compact but without the economics check.
	TierForce CompactionTier = "force"
)

// CompactionThresholds holds the four trigger ratios.
type CompactionThresholds struct {
	Soft    float64
	Snip    float64
	Compact float64
	Force   float64
}

// Thresholds derives the four trigger points from the compaction ratio.
//
// The multipliers are applied in hundredths and converted once, because the
// natural float form is not exact: 0.8*0.75 evaluates to 0.6000000000000001,
// which would push a usage ratio of exactly 0.60 down into the soft tier
// instead of the snip tier the documentation specifies.
func (c AgentConfig) Thresholds() CompactionThresholds {
	r := int64(c.CompactionRatio*10000 + 0.5)
	return CompactionThresholds{
		Soft:    float64(r*625/1000) / 10000,
		Snip:    float64(r*75/100) / 10000,
		Compact: float64(r) / 10000,
		Force:   float64(r*1125/1000) / 10000,
	}
}

// TierFor classifies a window-usage ratio into a compaction tier.
func (c AgentConfig) TierFor(usageRatio float64) CompactionTier {
	t := c.Thresholds()
	switch {
	case usageRatio >= t.Force:
		return TierForce
	case usageRatio >= t.Compact:
		return TierCompact
	case usageRatio >= t.Snip:
		return TierSnip
	case usageRatio >= t.Soft:
		return TierSoft
	default:
		return TierNone
	}
}

// EngineConfig bundles everything the Engine needs to start.
type EngineConfig struct {
	Scheduler    SchedulerConfig
	Backpressure BackpressureConfig
	Agent        AgentConfig
	// MaxRecoveryAttempts bounds how many times a run may be auto-resumed
	// before it is parked in waiting_user instead. Without this a run that
	// crashes deterministically would loop forever across restarts.
	MaxRecoveryAttempts int
	// CrashDumpDir is where core-goroutine crash dumps are written before the
	// process is handed to the Supervisor for restart.
	CrashDumpDir string
}

// DefaultEngineConfig returns the documented defaults.
func DefaultEngineConfig() EngineConfig {
	return EngineConfig{
		Scheduler:           DefaultSchedulerConfig(),
		Backpressure:        DefaultBackpressureConfig(),
		Agent:               DefaultAgentConfig(),
		MaxRecoveryAttempts: 3,
		CrashDumpDir:        "crashes",
	}
}

// Validate rejects invalid engine configurations.
func (c EngineConfig) Validate() error {
	if err := c.Scheduler.Validate(); err != nil {
		return err
	}
	if err := c.Backpressure.Validate(); err != nil {
		return err
	}
	if err := c.Agent.Validate(); err != nil {
		return err
	}
	if c.MaxRecoveryAttempts < 0 {
		return NewError(CodeInvalidArgument, "MaxRecoveryAttempts must not be negative, got %d", c.MaxRecoveryAttempts)
	}
	return nil
}
