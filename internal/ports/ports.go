// Package ports declares the interfaces task 02 consumes from the other
// concurrent tasks, plus the small value types they exchange.
//
// Why the interfaces live here rather than in the providing packages: tasks
// 03 (storage/checkpoint), 04 (tool runtime) and 06 (provider/context) are
// being written in parallel. Declaring the port in the consumer — Go's usual
// convention — lets the Engine compile and be tested against the in-memory
// implementations in package ports/mem today, and lets the real
// implementations satisfy these interfaces later without task 02 importing
// their packages (and without an import cycle when they need engine types).
//
// Signatures here are the ones named in the task book's "本任务需要消费的接口"
// section. If a provider task needs a different shape, that is a decision for
// task 08; the port is the single place to change.
package ports

import (
	"context"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// ---------------------------------------------------------------------------
// task 03: storage layer
// ---------------------------------------------------------------------------

// EventStore is the durable append-only event log (task 03).
//
// Seq allocation belongs to the store, not the engine: it is the only
// component that can guarantee no gaps and no duplicates across a crash.
type EventStore interface {
	// Append writes one event and returns its assigned sequence number.
	Append(ctx context.Context, runID string, event types.Event) (uint64, error)
	// Read returns events with Seq greater than afterSeq, in ascending order.
	// It returns a closed slice (not a channel) so replay is deterministic.
	Read(ctx context.Context, runID string, afterSeq uint64, limit int) ([]types.Event, error)
	// LastSeq returns the highest sequence written for the run, or 0 if none.
	LastSeq(ctx context.Context, runID string) (uint64, error)
	// ListRuns returns the IDs of every run with at least one event. Recovery
	// uses it to discover in-flight runs left by a dead process.
	ListRuns(ctx context.Context) ([]string, error)
}

// OutboxStore is the transactional outbox (task 03). Events that must reach an
// external observer (UI host, audit sink) are enqueued here so delivery
// survives a crash between the state change and the notification.
type OutboxStore interface {
	// Enqueue adds an event to the outbox. Implementations must assign a
	// stable ID so a redelivery after a crash is idempotent.
	Enqueue(ctx context.Context, event types.Event) error
	// Pending returns up to limit undelivered events, oldest first.
	Pending(ctx context.Context, limit int) ([]types.Event, error)
	// Ack marks an event delivered.
	Ack(ctx context.Context, runID string, seq uint64) error
}

// Checkpoint is a durable snapshot of a run's progress.
type Checkpoint struct {
	ID    string `json:"id"`
	RunID string `json:"runId"`
	// Seq is the event sequence the snapshot corresponds to. Restoring must
	// replay events after this point only.
	Seq uint64 `json:"seq"`
	// Round is the model round in progress when the snapshot was taken.
	Round int `json:"round"`
	// State is the run state at snapshot time.
	State types.RunState `json:"state"`
	// Payload is the opaque serialized run state. Task 02 treats it as bytes
	// so the snapshot format can evolve without changing this interface.
	Payload   []byte    `json:"payload,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

// CheckpointStore persists and restores run snapshots (task 03).
type CheckpointStore interface {
	// Save stores a snapshot and returns its ID.
	Save(ctx context.Context, cp Checkpoint) (string, error)
	// Latest returns the newest snapshot for a run. It returns a nil
	// checkpoint and no error when the run has none.
	Latest(ctx context.Context, runID string) (*Checkpoint, error)
	// Restore returns the newest snapshot with Seq <= seq. Recovery uses it to
	// locate the last safe point, which is by definition at or before the last
	// durable event it has already replayed.
	Restore(ctx context.Context, runID string, seq uint64) (*Checkpoint, error)
	// MarkUnsafe records that a snapshot must not be resumed from, e.g. when
	// it was taken mid-way through a non-idempotent tool call.
	MarkUnsafe(ctx context.Context, runID, checkpointID string, reason string) error
}

// ---------------------------------------------------------------------------
// task 04: tool runtime
// ---------------------------------------------------------------------------

// ToolRequest is one tool invocation handed to the runtime.
type ToolRequest struct {
	RunID      string
	SessionID  string
	ToolCallID string
	ToolName   string
	Arguments  map[string]any
	// Timeout is the maximum time the runtime may spend. The runtime is
	// expected to enforce it; the agent loop enforces it again as a safety net.
	Timeout time.Duration
	// AutoModeLevel is the permission posture ("yolo"/"safe"/...). The runtime
	// owns the policy; the agent loop only surfaces the waiting_user state when
	// the runtime asks for confirmation.
	AutoModeLevel string
	// Resource is the resource class the call must lease, derived by the
	// scheduler from the tool name.
	Resource types.ResourceClass
}

// ToolRuntime executes tool calls (task 04). It is the port the scheduler
// wraps: the scheduler decides *when* a call may run, the runtime decides
// *whether* and *how* it runs.
type ToolRuntime interface {
	// Execute runs one tool call. It returns a result for every outcome,
	// including failure; a non-nil error means the runtime itself broke, not
	// that the tool failed.
	Execute(ctx context.Context, req ToolRequest) (types.ToolResult, error)
}

// ToolRuntimeFunc adapts a function to ToolRuntime.
type ToolRuntimeFunc func(ctx context.Context, req ToolRequest) (types.ToolResult, error)

// Execute implements ToolRuntime.
func (f ToolRuntimeFunc) Execute(ctx context.Context, req ToolRequest) (types.ToolResult, error) {
	return f(ctx, req)
}

// IdempotencyClass classifies whether a tool call may be replayed after a
// crash. This is the input to the recovery decision in doc ch. 11: idempotent
// calls are re-run, detectable ones are probed for their effect, and
// non-idempotent ones are never repeated without asking the user.
type IdempotencyClass string

const (
	// Idempotent means replaying the call is harmless (file_read, ls, grep).
	Idempotent IdempotencyClass = "idempotent"
	// Detectable means the effect can be observed before deciding (file_write
	// with a known target path, a git commit that can be inspected).
	Detectable IdempotencyClass = "detectable"
	// NonIdempotent means replay could cause a duplicate side effect
	// (send_message, payment, a non-idempotent API call).
	NonIdempotent IdempotencyClass = "non_idempotent"
)

// Valid reports whether c is a known class.
func (c IdempotencyClass) Valid() bool {
	switch c {
	case Idempotent, Detectable, NonIdempotent:
		return true
	default:
		return false
	}
}

// SafeToReplay reports whether recovery may re-dispatch the call without
// asking the user.
func (c IdempotencyClass) SafeToReplay() bool { return c == Idempotent }

// IdempotencyStore answers the classification question (task 04). It is
// separate from ToolRuntime because classification must be available during
// recovery, when no runtime invocation is possible.
type IdempotencyStore interface {
	// Classify returns the class of the tool behind toolCallID.
	//
	// It must return a CodeNotFound error when the call ID is not known,
	// rather than a default class: recovery composes this call with
	// ClassifyByName, and a silent default would preempt the more specific
	// name-based answer. Only ClassifyByName carries a fallback default.
	Classify(ctx context.Context, toolCallID string) (IdempotencyClass, error)
	// ClassifyByName is the fallback used when recovery finds a call whose
	// tool-call ID was never pinned, or was never durably recorded (the crash
	// happened between the model round and the first event). It may return a
	// default class for an unknown tool name, which is why recovery treats an
	// unclassifiable call as non-idempotent.
	ClassifyByName(ctx context.Context, toolName string) (IdempotencyClass, error)
}

// ---------------------------------------------------------------------------
// task 06: provider and context management
// ---------------------------------------------------------------------------

// MessageRole mirrors the provider's role set.
type MessageRole string

const (
	RoleSystem    MessageRole = "system"
	RoleUser      MessageRole = "user"
	RoleAssistant MessageRole = "assistant"
	RoleTool      MessageRole = "tool"
)

// Message is one entry in the conversation sent to the provider.
type Message struct {
	Role    MessageRole `json:"role"`
	Content string      `json:"content,omitempty"`
	// ReasoningContent must be round-tripped verbatim for every assistant
	// turn while tools are present: DeepSeek rejects the request with a 400
	// otherwise (v1 tool-execution.ts A2').
	ReasoningContent string `json:"reasoningContent,omitempty"`
	// ToolCalls is set on an assistant turn that requested tools.
	ToolCalls []types.ToolCall `json:"toolCalls,omitempty"`
	// ToolCallID is set on a tool-result turn and pairs it back to the call.
	ToolCallID string `json:"toolCallId,omitempty"`
}

// ProviderRequest is one model round.
type ProviderRequest struct {
	Model     string
	Messages  []Message
	Tools     []types.ToolDefinition
	Effort    types.ReasoningEffort
	MaxTokens int
	// OnDelta receives streamed content and reasoning fragments. It must be
	// cheap and non-blocking: the loop hands the deltas to the coalescer.
	OnDelta func(Delta)
	// OnUsage receives normalized token accounting when the provider reports it.
	OnUsage func(Usage)
}

// Delta is a streamed fragment.
type Delta struct {
	// Content is assistant-visible text.
	Content string
	// Reasoning is chain-of-thought text.
	Reasoning string
	// ToolCallDelta carries incremental tool-call fragments. Providers stream
	// tool calls in pieces that must be concatenated by index.
	ToolCallDelta *ToolCallDelta
}

// ToolCallDelta is one incremental tool-call fragment.
type ToolCallDelta struct {
	Index int
	ID    string
	Name  string
	// ArgumentsFragment is a partial JSON string to concatenate.
	ArgumentsFragment string
}

// FinishReason explains why a model round ended.
type FinishReason string

const (
	FinishStop      FinishReason = "stop"
	FinishToolCalls FinishReason = "tool_calls"
	FinishLength    FinishReason = "length"
	FinishError     FinishReason = "error"
	FinishCancelled FinishReason = "cancelled"
)

// Valid reports whether f is a known finish reason.
func (f FinishReason) Valid() bool {
	switch f {
	case FinishStop, FinishToolCalls, FinishLength, FinishError, FinishCancelled:
		return true
	default:
		return false
	}
}

// ProviderResponse is the outcome of one model round.
type ProviderResponse struct {
	FinishReason     FinishReason
	Content          string
	ReasoningContent string
	ToolCalls        []types.ToolCall
	Usage            Usage
	Error            string
	// Emitted reports whether any content, reasoning or tool call reached the
	// client. v1 uses it to decide whether a dropped connection may be
	// replayed: replaying after partial output would duplicate it.
	Emitted bool
}

// Usage is normalized token accounting. The two shapes DeepSeek returns
// (prompt/completion vs. cached/missed) are folded into these fields, matching
// v1's normaliseUsage.
type Usage struct {
	PromptTokens     int `json:"promptTokens"`
	CompletionTokens int `json:"completionTokens"`
	TotalTokens      int `json:"totalTokens"`
	CacheHitTokens   int `json:"cacheHitTokens"`
	CacheMissTokens  int `json:"cacheMissTokens"`
	ReasoningTokens  int `json:"reasoningTokens"`
}

// Provider performs model rounds (task 06).
type Provider interface {
	// Complete runs one round. A non-nil error means the transport or provider
	// failed; a provider-side failure is reported as FinishError with Error
	// set, so the loop can distinguish "retry the transport" from "the model
	// refused".
	Complete(ctx context.Context, req ProviderRequest) (ProviderResponse, error)
}

// ProviderFunc adapts a function to Provider.
type ProviderFunc func(ctx context.Context, req ProviderRequest) (ProviderResponse, error)

// Complete implements Provider.
func (f ProviderFunc) Complete(ctx context.Context, req ProviderRequest) (ProviderResponse, error) {
	return f(ctx, req)
}

// ContextSession is the compaction unit. The engine passes the conversation it
// wants compacted plus the usage that triggered the decision.
type ContextSession struct {
	RunID     string
	SessionID string
	Messages  []Message
	Usage     Usage
	Window    int
	// Tier is the compaction level the engine decided on. The context manager
	// executes it; the *decision* lives in the agent package (task 02).
	Tier types.CompactionTier
	// ProtectRecent is how many trailing turns must survive compaction.
	ProtectRecent int
}

// CompactionResult reports what compaction did.
type CompactionResult struct {
	Tier types.CompactionTier
	// Messages is the rewritten conversation, oldest first.
	Messages []Message
	// Folded is how many messages were replaced by the summary.
	Folded int
	// SummaryMessage is the message that replaced the folded region, if any.
	SummaryMessage *Message
	// Usage after compaction, when the manager can estimate it.
	Usage Usage
	// Stuck reports that compaction cannot free enough space. The engine then
	// stops trying and lets the prefix grow append-only, exactly as v1 does
	// after two consecutive ineffective compactions.
	Stuck bool
}

// ContextManager performs context compaction (task 06). Task 02 decides *when*
// (see internal/agent/compaction.go); task 06 decides *how*.
type ContextManager interface {
	Compact(ctx context.Context, session ContextSession) (CompactionResult, error)
}
