package types

import "time"

// RunState is the agent run state machine (architecture doc ch. 4). Every
// transition is recorded as an event, applied to the materialized run record,
// pushed to the UI and, where the state is externally observable, written to
// the outbox.
type RunState string

const (
	// StateCreated is the initial state, before admission control has seen
	// the run. Nothing has been durably scheduled yet.
	StateCreated RunState = "created"

	// StateQueued means the run holds a slot in the session fair queue and is
	// waiting for global admission plus a worker.
	StateQueued RunState = "queued"

	// StatePlanning is the optional pre-loop phase that narrows the tool set
	// before the first model round (v1 planning-phase.ts).
	StatePlanning RunState = "planning"

	// StateThinking covers a model round in flight, including streaming of
	// reasoning and the arrival of tool calls.
	StateThinking RunState = "thinking"

	// StateExecuting covers dispatched tool calls awaiting their results.
	StateExecuting RunState = "executing"

	// StateCompacting means context compaction is running between rounds.
	StateCompacting RunState = "compacting"

	// StateWaitingUser means the run is parked on a human decision: a tool
	// permission prompt, a plan question, or an uncertain non-idempotent tool
	// call found during crash recovery.
	StateWaitingUser RunState = "waiting_user"

	// StateCompleted is terminal: the run produced a final answer.
	StateCompleted RunState = "completed"

	// StateCancelled is terminal: the run was cancelled by the user or by
	// engine shutdown.
	StateCancelled RunState = "cancelled"

	// StateFailed is terminal: the run ended in an unrecoverable error.
	StateFailed RunState = "failed"

	// StateRecovering is entered by a fresh Engine process that found an
	// in-flight run in the durable log and is rebuilding its state.
	StateRecovering RunState = "recovering"
)

// AllRunStates lists every state, in the order used by the transition table.
// It exists so tests can assert the enumeration stays in sync with the
// documented state machine.
var AllRunStates = []RunState{
	StateCreated, StateQueued, StatePlanning, StateThinking, StateExecuting,
	StateCompacting, StateWaitingUser, StateCompleted, StateCancelled,
	StateFailed, StateRecovering,
}

// transitionTable is the single source of truth for legal state changes.
// Anything not listed is rejected with CodeInvalidTransition, which keeps the
// recovery logic honest: a resumed run cannot silently jump to a state it
// could never have reached.
var transitionTable = map[RunState][]RunState{
	StateCreated: {StateQueued, StateCancelled, StateFailed},
	// A queued run may be parked for user confirmation: crash recovery finds
	// runs that died before their first round and still has to hand them to the
	// user rather than executing them unattended.
	StateQueued: {StatePlanning, StateThinking, StateWaitingUser,
		StateCancelled, StateFailed, StateRecovering},
	StatePlanning:   {StateThinking, StateWaitingUser, StateCancelled, StateFailed, StateCompacting},
	StateThinking:   {StateExecuting, StateCompacting, StateWaitingUser, StateCompleted, StateCancelled, StateFailed, StateThinking},
	StateExecuting:  {StateThinking, StateCompacting, StateWaitingUser, StateCompleted, StateCancelled, StateFailed},
	StateCompacting: {StateThinking, StateWaitingUser, StateCancelled, StateFailed},
	StateWaitingUser: {StateExecuting, StateThinking, StatePlanning, StateCompacting,
		StateCancelled, StateFailed, StateCompleted},
	StateRecovering: {StateQueued, StateThinking, StateExecuting, StateWaitingUser,
		StateCancelled, StateFailed, StateCompleted},
	// Terminal states have no outgoing edges.
	StateCompleted: {},
	StateCancelled: {},
	StateFailed:    {},
}

// CanTransitionTo reports whether from 鈫?to is a legal transition. A no-op
// self transition is allowed only where the table lists it explicitly
// (StateThinking 鈫?StateThinking, for consecutive model rounds).
func CanTransitionTo(from, to RunState) bool {
	if !from.Valid() || !to.Valid() {
		return false
	}
	for _, allowed := range transitionTable[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// Valid reports whether s is a known state.
func (s RunState) Valid() bool {
	_, ok := transitionTable[s]
	return ok
}

// Terminal reports whether no further transition is possible.
func (s RunState) Terminal() bool {
	if !s.Valid() {
		return false
	}
	return len(transitionTable[s]) == 0
}

// Resumable reports whether a run found in this state during crash recovery
// should be offered for resume rather than being declared dead.
func (s RunState) Resumable() bool {
	switch s {
	case StateCreated, StateQueued, StatePlanning, StateThinking, StateExecuting,
		StateCompacting, StateWaitingUser, StateRecovering:
		return true
	default:
		return false
	}
}

// Priority is the QoS class of a run or task. It maps onto the weighted fair
// queue weights (architecture doc ch. 6): interactive 8, normal 4,
// background 1, knowledge 1.
type Priority string

const (
	PriorityInteractive Priority = "interactive"
	PriorityNormal      Priority = "normal"
	PriorityBackground  Priority = "background"
	PriorityKnowledge   Priority = "knowledge"
)

// AllPriorities lists the QoS classes in descending weight order.
var AllPriorities = []Priority{
	PriorityInteractive, PriorityNormal, PriorityBackground, PriorityKnowledge,
}

// Weight returns the fair-queue weight for this class. The values are the
// contract from ch. 6 and must not be changed without updating the doc.
func (p Priority) Weight() int {
	switch p {
	case PriorityInteractive:
		return 8
	case PriorityNormal:
		return 4
	case PriorityBackground, PriorityKnowledge:
		return 1
	default:
		return 1
	}
}

// Valid reports whether p is a known QoS class.
func (p Priority) Valid() bool {
	switch p {
	case PriorityInteractive, PriorityNormal, PriorityBackground, PriorityKnowledge:
		return true
	default:
		return false
	}
}

// ResourceClass names a limited execution resource. The capacities are the
// third limiting layer from ch. 6: browser=4, computer_use=2, terminal=16,
// office=4, mcp=16, vision=8, provider_requests=32, expert_agent=8.
type ResourceClass string

const (
	ResourceBrowser     ResourceClass = "browser"
	ResourceComputerUse ResourceClass = "computer_use"
	ResourceTerminal    ResourceClass = "terminal"
	ResourceOffice      ResourceClass = "office"
	ResourceMCP         ResourceClass = "mcp"
	ResourceVision      ResourceClass = "vision"
	ResourceProvider    ResourceClass = "provider_requests"
	// ResourceClassExpertAgent gates expert sub-agents. Before it existed, a burst
	// of agent_expert calls competed for the same per-session tool slots as
	// ordinary tool calls; giving sub-agents their own class isolates the two
	// so a fan-out of experts cannot starve file/git/web work 鈥?and vice versa.
	// The capacity matches DefaultMaxConcurrentToolsPerSession so a model may
	// still fan out as wide as one round allows.
	ResourceClassExpertAgent ResourceClass = "expert_agent"
)

// ResourceCapacities is the fixed capacity table from ch. 6 (plus the task-05
// expert_agent class). It is exported so the scheduler, the tests and task 05's
// worker pool all read one table.
var ResourceCapacities = map[ResourceClass]int{
	ResourceBrowser:          4,
	ResourceComputerUse:      2,
	ResourceTerminal:         16,
	ResourceOffice:           4,
	ResourceMCP:              16,
	ResourceVision:           8,
	ResourceProvider:         32,
	ResourceClassExpertAgent: 8,
}

// AllResourceClasses lists every resource class.
var AllResourceClasses = []ResourceClass{
	ResourceBrowser, ResourceComputerUse, ResourceTerminal, ResourceOffice,
	ResourceMCP, ResourceVision, ResourceProvider, ResourceClassExpertAgent,
}

// Valid reports whether r is a known resource class.
func (r ResourceClass) Valid() bool {
	_, ok := ResourceCapacities[r]
	return ok
}

// TaskKind distinguishes the two queueable units of work. Run tasks occupy a
// global running slot; tool-call tasks additionally occupy a resource-class
// lease.
type TaskKind string

const (
	TaskKindRun      TaskKind = "run"
	TaskKindToolCall TaskKind = "tool_call"
)

// ReasoningEffort mirrors the v1 thinking-intensity modes (shared/types/core.ts).
// ultra additionally enables the engineering-paradigm prompt and the
// supervision agent; every non-off mode requests reasoning_content.
type ReasoningEffort string

const (
	EffortOff   ReasoningEffort = "off"
	EffortLow   ReasoningEffort = "low"
	EffortHigh  ReasoningEffort = "high"
	EffortMax   ReasoningEffort = "max"
	EffortUltra ReasoningEffort = "ultra"
)

// Valid reports whether e is a known effort level.
func (e ReasoningEffort) Valid() bool {
	switch e {
	case EffortOff, EffortLow, EffortHigh, EffortMax, EffortUltra:
		return true
	default:
		return false
	}
}

// ThinkingEnabled reports whether reasoning content should be requested and
// round-tripped. v1 omits reasoning params entirely for "off".
func (e ReasoningEffort) ThinkingEnabled() bool { return e != EffortOff }

// SupervisionEnabled reports whether the ultra-mode supervision agent runs
// after each tool round.
func (e ReasoningEffort) SupervisionEnabled() bool { return e == EffortUltra }

// APIMapped returns the effort actually sent to the provider. v1 maps ultra
// onto max (api-request-builder.ts toApiEffort).
func (e ReasoningEffort) APIMapped() ReasoningEffort {
	if e == EffortUltra {
		return EffortMax
	}
	return e
}

// Run is the materialized view of one agent run. It is deliberately a plain
// value type: the SessionActor owns the authoritative copy and hands out
// copies, so no caller can mutate engine state without going through the
// mailbox.
type Run struct {
	ID        string          `json:"id"`
	SessionID string          `json:"sessionId"`
	State     RunState        `json:"state"`
	Priority  Priority        `json:"priority"`
	Prompt    string          `json:"prompt"`
	Model     string          `json:"model"`
	Effort    ReasoningEffort `json:"effort"`
	LongTask  bool            `json:"longTask"`

	Round            int `json:"round"`
	Continuation     int `json:"continuation"`
	MaxRounds        int `json:"maxRounds"`
	MaxContinuations int `json:"maxContinuations"`

	// LastSeq is the highest durable event sequence written for this run.
	LastSeq uint64 `json:"lastSeq"`
	// CheckpointSeq is the sequence of the newest checkpoint known to be safe
	// to resume from. Zero means "no checkpoint yet".
	CheckpointSeq uint64 `json:"checkpointSeq"`

	// Answer holds the final answer once the run completes.
	Answer string `json:"answer,omitempty"`
	// Err holds the terminal error for a failed run.
	Err *Error `json:"err,omitempty"`
	// WaitingReason explains why the run is parked in waiting_user.
	WaitingReason string `json:"waitingReason,omitempty"`
	// UncertainToolCalls lists non-idempotent tool calls whose completion
	// could not be determined after a crash. They must be confirmed by the
	// user before the run continues (doc ch. 11).
	UncertainToolCalls []string `json:"uncertainToolCalls,omitempty"`

	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
	StartedAt   time.Time `json:"startedAt,omitempty"`
	CompletedAt time.Time `json:"completedAt,omitempty"`

	// Version increments on every materialized-state mutation. It is the
	// optimistic-concurrency token used by recovery to detect a run that was
	// already picked up by another process.
	Version uint64 `json:"version"`
}

// Clone returns a deep copy safe to hand to callers outside the actor.
func (r Run) Clone() Run {
	c := r
	if r.Err != nil {
		e := *r.Err
		c.Err = &e
	}
	if r.UncertainToolCalls != nil {
		c.UncertainToolCalls = append([]string(nil), r.UncertainToolCalls...)
	}
	return c
}

// RunHandle is returned by Engine.Submit so the caller can follow the run
// without a second lookup.
type RunHandle struct {
	RunID     string   `json:"runId"`
	SessionID string   `json:"sessionId"`
	State     RunState `json:"state"`
	Priority  Priority `json:"priority"`
}

// SubmitRequest is the input to Engine.Submit. SessionID selects which
// SessionActor serialises the writes; an empty SessionID makes the engine
// create a fresh session for the run.
type SubmitRequest struct {
	SessionID string `json:"sessionId,omitempty"`
	Prompt    string `json:"prompt"`

	Model  string          `json:"model,omitempty"`
	Effort ReasoningEffort `json:"effort,omitempty"`

	// SystemPrompt is the session-level instruction block. It is excluded
	// from prompt-cache-shape diagnostics only if empty.
	SystemPrompt string `json:"systemPrompt,omitempty"`

	// Tools is the tool catalogue available to the first model round. The
	// planning phase may narrow it.
	Tools []ToolDefinition `json:"tools,omitempty"`

	// LongTask enables the segmented long-task loop (50 rounds/segment,
	// 10 continuations) instead of the flat 30-round cap.
	LongTask bool `json:"longTask,omitempty"`

	// ClusterSize > 0 时以「Agent 集群」模式执行：引擎按任务内容自动挑选
	// ClusterSize 位专家，并行跑子代理，最后汇总成一份多视角报告。
	//
	// 与 ExpertID（用户手选单一专家）互斥，由前端保证：集群是"我不指定谁，
	// 让系统自己组队"，直连是"我就要这一位"。引擎侧集群分支优先判定。
	// 上限由 expert_agent 资源闸门（8）兜住，超出部分排队而非拒绝。
	ClusterSize int `json:"clusterSize,omitempty"`

	// Priority selects the fair-queue class. Empty means PriorityNormal.
	Priority Priority `json:"priority,omitempty"`

	// MaxRounds overrides the per-segment round cap. Zero uses the default.
	MaxRounds int `json:"maxRounds,omitempty"`

	// PlanMode makes the run propose a user-visible execution plan and park in
	// waiting_user until the user accepts or rejects it (task 4). It is
	// independent of the internal planning phase, which narrows the tool
	// catalogue and is controlled by AgentConfig.PlanningEnabled.
	//
	// The zero value is false, so a caller that does not set it gets exactly the
	// pre-existing behaviour.
	PlanMode bool `json:"planMode,omitempty"`

	// ExpertID is the user-picked expert (task 3). When non-empty the engine
	// activates that expert's two-phase orchestration directly instead of
	// running the generic agent loop and relying on the model to decide by
	// itself whether to call the agent_expert tool.
	//
	// Independent of PlanMode; both are zero-valued for a plain submission, so
	// the pre-existing behaviour is unchanged.
	ExpertID string `json:"expertId,omitempty"`

	// Metadata carries caller-supplied labels. It is copied into events, so
	// it must not contain credentials.
	Metadata map[string]string `json:"metadata,omitempty"`
}

// Validate rejects requests that cannot possibly be executed, before they
// consume a queue slot.
func (r SubmitRequest) Validate() error {
	if r.Prompt == "" {
		return NewError(CodeInvalidArgument, "prompt must not be empty")
	}
	if r.Priority != "" && !r.Priority.Valid() {
		return NewError(CodeInvalidArgument, "unknown priority %q", string(r.Priority))
	}
	if r.Effort != "" && !r.Effort.Valid() {
		return NewError(CodeInvalidArgument, "unknown reasoning effort %q", string(r.Effort))
	}
	return nil
}

// Normalized returns a copy with defaults filled in.
func (r SubmitRequest) Normalized() SubmitRequest {
	c := r
	if c.Priority == "" {
		c.Priority = PriorityNormal
	}
	if c.Effort == "" {
		c.Effort = EffortHigh
	}
	if c.MaxRounds <= 0 {
		c.MaxRounds = DefaultMaxToolRounds
	}
	return c
}

// ToolDefinition is the schema passed to the provider. It is declared here
// because SubmitRequest carries it across the Engine boundary; task 04 owns
// the executor side.
type ToolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// ToolCall is a single tool invocation requested by the model.
type ToolCall struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments,omitempty"`
}

// ToolResult is the outcome of one tool invocation.
type ToolResult struct {
	ToolCallID string         `json:"toolCallId"`
	ToolName   string         `json:"toolName"`
	Content    string         `json:"content,omitempty"`
	Success    bool           `json:"success"`
	Error      string         `json:"error,omitempty"`
	Metadata   map[string]any `json:"metadata,omitempty"`
	// Duration is how long the tool ran, for progress reporting.
	Duration time.Duration `json:"duration,omitempty"`
}

// Task is the unit of work the Scheduler queues. A run produces one TaskKindRun
// at admission time and one TaskKindToolCall per dispatched tool call.
type Task struct {
	ID        string        `json:"id"`
	Kind      TaskKind      `json:"kind"`
	RunID     string        `json:"runId"`
	SessionID string        `json:"sessionId"`
	Priority  Priority      `json:"priority"`
	ToolName  string        `json:"toolName,omitempty"`
	Resource  ResourceClass `json:"resource,omitempty"`

	// Cost is the task's size in abstract units used by the weighted fair
	// queue. Tool calls default to 1; a run costs its estimated round budget
	// so a runaway run cannot starve interactive traffic.
	Cost int `json:"cost"`

	EnqueuedAt time.Time `json:"enqueuedAt"`
}

// QueueStats reports one queue's depth and lifetime counters.
type QueueStats struct {
	Priority   Priority `json:"priority"`
	Weight     int      `json:"weight"`
	Queued     int      `json:"queued"`
	MaxQueued  int      `json:"maxQueued"`
	Dispatched uint64   `json:"dispatched"`
	Rejected   uint64   `json:"rejected"`
}

// ResourceStats reports one resource pool's utilisation.
type ResourceStats struct {
	Resource ResourceClass `json:"resource"`
	InUse    int           `json:"inUse"`
	Capacity int           `json:"capacity"`
	Waiting  int           `json:"waiting"`
	Acquired uint64        `json:"acquired"`
	Rejected uint64        `json:"rejected"`
}

// SchedulerStats is the snapshot returned by Scheduler.Stats. It is a value
// type so callers cannot observe a torn view of the scheduler.
type SchedulerStats struct {
	RunningTasks       int `json:"runningTasks"`
	MaxRunningTasks    int `json:"maxRunningTasks"`
	QueuedTasks        int `json:"queuedTasks"`
	MaxQueuedTasks     int `json:"maxQueuedTasks"`
	QueuedToolCalls    int `json:"queuedToolCalls"`
	MaxQueuedToolCalls int `json:"maxQueuedToolCalls"`

	TotalDispatched uint64 `json:"totalDispatched"`
	TotalCompleted  uint64 `json:"totalCompleted"`
	TotalRejected   uint64 `json:"totalRejected"`
	// ObserverPanics counts panics contained at the notification boundary.
	ObserverPanics uint64 `json:"observerPanics"`

	ByPriority map[Priority]QueueStats         `json:"byPriority"`
	Resources  map[ResourceClass]ResourceStats `json:"resources"`
}
