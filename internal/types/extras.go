package types

// This file holds the symbols contributed by task 08's authoritative contract
// package that task 02's packages do not define. After the D-4 merge, the
// colliding names (Event, Run, RunState, Priority, SubmitRequest, RunHandle,
// QueueLimits, ToolCall, ToolDefinition, ToolResult) are owned by task 02's
// richer definitions in run.go / events.go / config.go, because task 02's
// engine, agent, scheduler and ports packages are their only consumers.
//
// Everything below is referenced by name from outside this package, so it must
// stay exported and keep its shape.

import (
	"encoding/json"
	"fmt"
	"time"
)

// ---------------------------------------------------------------------------
// Event classification (task 07 reads these through the durable-event boundary)
// ---------------------------------------------------------------------------

// EventClass controls backpressure (chapter 7).
type EventClass int

const (
	// EventClassDurable events are never dropped and never coalesced.
	EventClassDurable EventClass = iota
	// EventClassCoalescible events may be merged in the UI fan-out path.
	EventClassCoalescible
)

// EventClassOf classifies an event type by its wire name. Unknown types default
// to durable: silently coalescing an unrecognised event risks dropping a
// boundary event that recovery depends on.
func EventClassOf(eventType string) EventClass {
	switch eventType {
	case "token.delta", "heartbeat", "progress":
		return EventClassCoalescible
	default:
		return EventClassDurable
	}
}

// IsDurable reports whether an event type must reach the durable log verbatim.
func IsDurable(eventType string) bool {
	return EventClassOf(eventType) == EventClassDurable
}

// RunStatePayload is the body of a run state-change event.
type RunStatePayload struct {
	From   RunState `json:"from"`
	To     RunState `json:"to"`
	Reason string   `json:"reason,omitempty"`
}

// ToolEventPayload is the body of tool started/completed/failed/timeout events.
type ToolEventPayload struct {
	ToolCallID string         `json:"tool_call_id"`
	ToolName   string         `json:"tool_name"`
	Status     ToolCallStatus `json:"status,omitempty"`
	DurationMs int64          `json:"duration_ms,omitempty"`
	ErrorCode  Code           `json:"error_code,omitempty"`
	ErrorText  string         `json:"error_text,omitempty"`
}

// ---------------------------------------------------------------------------
// Tool call state machine (task 04 owns the values, task 02 consumes them)
// ---------------------------------------------------------------------------

// ToolCallStatus is the terminal state of a tool call.
type ToolCallStatus string

const (
	ToolCallPending   ToolCallStatus = "pending"
	ToolCallRunning   ToolCallStatus = "running"
	ToolCallCompleted ToolCallStatus = "completed"
	ToolCallFailed    ToolCallStatus = "failed"
	ToolCallTimeout   ToolCallStatus = "timeout"
	// ToolCallNeedsConfirmation is the terminal state for a non-idempotent call
	// whose outcome is unknown after a crash. It is never retried
	// automatically (I8, chapter 19 class C).
	ToolCallNeedsConfirmation ToolCallStatus = "needs_confirmation"
	ToolCallCancelled         ToolCallStatus = "cancelled"
)

// IsTerminal reports whether the call has reached a final status.
func (s ToolCallStatus) IsTerminal() bool {
	switch s {
	case ToolCallCompleted, ToolCallFailed, ToolCallTimeout,
		ToolCallNeedsConfirmation, ToolCallCancelled:
		return true
	default:
		return false
	}
}

// IdempotencyClass is the A/B/C classification from chapter 19.
type IdempotencyClass string

const (
	// Idempotent (class A) — safe to retry blindly.
	Idempotent IdempotencyClass = "idempotent"
	// Detectable (class B) — retry only after checking current state.
	Detectable IdempotencyClass = "detectable"
	// NonIdempotent (class C) — never auto-repeat; park for confirmation.
	NonIdempotent IdempotencyClass = "non_idempotent"
)

// ToolRequest is one tool invocation. Task 02 builds it, task 04 executes it.
type ToolRequest struct {
	ToolCallID string `json:"tool_call_id"`
	RunID      string `json:"run_id"`
	SessionID  string `json:"session_id"`
	TurnID     string `json:"turn_id,omitempty"`
	ToolName   string `json:"tool_name"`
	// Arguments is the raw JSON argument object from the model.
	Arguments json.RawMessage `json:"arguments"`
	// Deadline bounds execution; zero means the tool's own default timeout.
	Deadline time.Time `json:"deadline,omitempty"`
	// IdempotencyKey is hash(run_id + tool_call_id), computed once by task 04.
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	// Idempotency governs crash recovery.
	Idempotency IdempotencyClass `json:"idempotency,omitempty"`
}

// ToolResponse is the normalised result of a tool invocation.
type ToolResponse struct {
	ToolCallID string         `json:"tool_call_id"`
	ToolName   string         `json:"tool_name"`
	Status     ToolCallStatus `json:"status"`
	Output     json.RawMessage `json:"output,omitempty"`
	// BlobRefs are content-addressed artefacts the tool produced.
	BlobRefs   []BlobRef `json:"blob_refs,omitempty"`
	ErrorCode  Code      `json:"error_code,omitempty"`
	ErrorText  string    `json:"error_text,omitempty"`
	DurationMs int64     `json:"duration_ms,omitempty"`
	// Durable is set only by the storage commit (I3), never by a normalizer.
	Durable bool `json:"durable,omitempty"`
}

// RiskLevel orders tool capabilities for the permission engine.
type RiskLevel int

const (
	RiskLow RiskLevel = iota
	RiskMedium
	RiskHigh
	RiskCritical
)

// Effect is a permission decision outcome (deny > ask > allow > default).
type Effect string

const (
	EffectAllow Effect = "allow"
	EffectAsk   Effect = "ask"
	EffectDeny  Effect = "deny"
)

// Decision is the permission engine's verdict, carrying the rule that produced
// it so the UI can explain a block.
type Decision struct {
	Effect Effect    `json:"effect"`
	Reason string    `json:"reason"`
	RuleID string    `json:"rule_id"`
	Risk   RiskLevel `json:"risk"`
}

// Allows reports whether the decision permits unattended execution.
func (d Decision) Allows() bool { return d.Effect == EffectAllow }

// ---------------------------------------------------------------------------
// Worker domain boundary (task 05 owns workers; task 04 routes to them)
// ---------------------------------------------------------------------------

// WorkerKind identifies a worker fault domain (chapters 15-17).
type WorkerKind string

const (
	WorkerBrowser     WorkerKind = "browser"
	WorkerTerminal    WorkerKind = "terminal"
	WorkerMCP         WorkerKind = "mcp"
	WorkerOffice      WorkerKind = "office"
	WorkerDynamicJS   WorkerKind = "dynamic-js"
	WorkerComputerUse WorkerKind = "computer-use"
	WorkerVision      WorkerKind = "vision"
)

// RequiresWorker reports whether a kind must run out-of-process (chapter 13).
// Task 04 routes on this; task 05 sizes pools by it, so the two cannot drift.
func (k WorkerKind) RequiresWorker() bool {
	switch k {
	case WorkerBrowser, WorkerTerminal, WorkerMCP, WorkerOffice,
		WorkerDynamicJS, WorkerComputerUse:
		return true
	default:
		return false
	}
}

// WorkerRequest is the process-boundary request a worker receives. It is
// deliberately distinct from ToolRequest: that is the Engine-facing contract,
// this is the transport contract, so the wire format can evolve without
// touching every tool implementation.
type WorkerRequest struct {
	RequestID      string          `json:"request_id"`
	RunID          string          `json:"run_id"`
	SessionID      string          `json:"session_id"`
	ToolName       string          `json:"tool_name"`
	Kind           WorkerKind      `json:"kind"`
	Arguments      json.RawMessage `json:"arguments"`
	Deadline       time.Time       `json:"deadline,omitempty"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
	Attempt        int             `json:"attempt"`
}

// WorkerResponse is the raw worker result, before normalization.
type WorkerResponse struct {
	RequestID string `json:"request_id"`
	// OK is the worker's own success flag, independent of any status enum: a
	// worker may return OK=false with a usable partial Output.
	OK         bool            `json:"ok"`
	Output     json.RawMessage `json:"output,omitempty"`
	Artefacts  []BlobRef       `json:"artefacts,omitempty"`
	ErrorCode  Code            `json:"error_code,omitempty"`
	ErrorText  string          `json:"error_text,omitempty"`
	DurationMs int64           `json:"duration_ms,omitempty"`
	TimedOut   bool            `json:"timed_out,omitempty"`
}

// WorkerState is a worker's lifecycle state as tracked by the manager.
type WorkerState string

const (
	WorkerStarting WorkerState = "starting"
	WorkerReady    WorkerState = "ready"
	WorkerBusy     WorkerState = "busy"
	WorkerDraining WorkerState = "draining"
	WorkerFailed   WorkerState = "failed"
	WorkerStopped  WorkerState = "stopped"
)

// WorkerHealth is returned by Worker.Health over IPC.
type WorkerHealth struct {
	WorkerID string      `json:"worker_id"`
	Kind     WorkerKind  `json:"kind"`
	State    WorkerState `json:"state"`
	PID      int         `json:"pid,omitempty"`
	// ActiveLeases counts outstanding leases; a worker holding leases must be
	// drained, not killed, during graceful shutdown (I10).
	ActiveLeases int       `json:"active_leases"`
	LastBeat     time.Time `json:"last_beat"`
	Restarts     int       `json:"restarts"`
}

// KindOfTool maps a tool name to its worker fault domain by name prefix.
func KindOfTool(toolName string) (WorkerKind, bool) {
	switch {
	case hasPrefix(toolName, "browser_"):
		return WorkerBrowser, true
	case hasPrefix(toolName, "terminal_"), hasPrefix(toolName, "shell_"):
		return WorkerTerminal, true
	case hasPrefix(toolName, "mcp_"):
		return WorkerMCP, true
	case hasPrefix(toolName, "office_"):
		return WorkerOffice, true
	case hasPrefix(toolName, "dynamic_"), hasPrefix(toolName, "execute_js"):
		return WorkerDynamicJS, true
	case hasPrefix(toolName, "computer_"):
		return WorkerComputerUse, true
	case hasPrefix(toolName, "vision_"):
		return WorkerVision, true
	default:
		return "", false
	}
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

// BuildWorkerRequest converts an Engine-facing ToolRequest into the
// process-boundary WorkerRequest — the outbound half of result normalization.
//
// It refuses a tool that is not a worker-domain tool: sending a pure-Go tool
// across the process boundary would silently move it into a weaker fault domain
// than task 04's routing decided on.
func BuildWorkerRequest(req ToolRequest, kind WorkerKind, attempt int) (WorkerRequest, error) {
	if req.ToolCallID == "" {
		return WorkerRequest{}, fmt.Errorf("types: ToolRequest.ToolCallID is required")
	}
	if req.ToolName == "" {
		return WorkerRequest{}, fmt.Errorf("types: ToolRequest.ToolName is required")
	}
	if !kind.RequiresWorker() {
		return WorkerRequest{}, fmt.Errorf("types: worker kind %q does not require a worker process", kind)
	}
	if attempt < 1 {
		return WorkerRequest{}, fmt.Errorf("types: attempt must be >= 1, got %d", attempt)
	}
	args := req.Arguments
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	return WorkerRequest{
		RequestID:      req.ToolCallID,
		RunID:          req.RunID,
		SessionID:      req.SessionID,
		ToolName:       req.ToolName,
		Kind:           kind,
		Arguments:      args,
		Deadline:       req.Deadline,
		IdempotencyKey: req.IdempotencyKey,
		Attempt:        attempt,
	}, nil
}

// WorkerResponseOptions carries the normalizer's context that the raw worker
// payload cannot know: the tool's identity and its idempotency class.
type WorkerResponseOptions struct {
	ToolCallID string
	ToolName   string
	// Idempotency decides what a *failed* response means. A class-C tool that
	// failed at an unknown point may or may not have applied its side effect,
	// so it must not become a retryable failure (I8).
	Idempotency IdempotencyClass
}

// NormalizeWorkerResponse converts a raw WorkerResponse into the canonical
// ToolResponse. Branch order is semantic priority:
//
//	worker OK                -> completed (Durable still false: only the
//	                            storage commit may set it, per I3)
//	worker timed out         -> timeout
//	worker failed, class C   -> needs_confirmation (never auto-retried)
//	worker failed, class A/B -> failed (retry permitted)
func NormalizeWorkerResponse(resp WorkerResponse, opts WorkerResponseOptions) (ToolResponse, error) {
	if opts.ToolCallID == "" {
		return ToolResponse{}, fmt.Errorf("types: normalize: ToolCallID is required")
	}
	if resp.RequestID != "" && resp.RequestID != opts.ToolCallID {
		return ToolResponse{}, fmt.Errorf(
			"types: normalize: response request_id %q does not match tool call %q",
			resp.RequestID, opts.ToolCallID)
	}

	out := ToolResponse{
		ToolCallID: opts.ToolCallID,
		ToolName:   opts.ToolName,
		DurationMs: resp.DurationMs,
		Output:     resp.Output,
		BlobRefs:   resp.Artefacts,
		// Durable is deliberately left false: a worker reporting success is not
		// evidence of commitment (I3).
		Durable: false,
	}

	switch {
	case resp.OK:
		out.Status = ToolCallCompleted
		out.ErrorCode = resp.ErrorCode
		out.ErrorText = resp.ErrorText
	case resp.TimedOut:
		out.Status = ToolCallTimeout
		out.ErrorCode = resp.ErrorCode
		if out.ErrorCode == "" {
			out.ErrorCode = CodeTimeout
		}
		out.ErrorText = resp.ErrorText
	case opts.Idempotency == NonIdempotent:
		out.Status = ToolCallNeedsConfirmation
		out.ErrorCode = resp.ErrorCode
		if out.ErrorCode == "" {
			out.ErrorCode = CodeIdempotencyConflict
		}
		out.ErrorText = resp.ErrorText
	default:
		out.Status = ToolCallFailed
		out.ErrorCode = resp.ErrorCode
		if out.ErrorCode == "" {
			out.ErrorCode = CodeToolFailed
		}
		out.ErrorText = resp.ErrorText
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// IPC frame header (task 01 owns the wire, tasks 02/05 consume it)
// ---------------------------------------------------------------------------

// FrameHeader prefixes every IPC frame (chapter 8). It lives here rather than
// in internal/ipc so that engine and worker packages never have to import the
// transport layer.
type FrameHeader struct {
	Version       uint8  `json:"version"`
	RequestID     string `json:"request_id"`
	SessionID     string `json:"session_id"`
	Sequence      uint64 `json:"sequence"`
	Type          string `json:"type"`
	PayloadLength uint32 `json:"payload_length"`
}

// FrameVersion is the current wire version. A peer presenting a different
// version is refused rather than best-effort parsed.
const FrameVersion uint8 = 1

// MaxFramePayload bounds a single IPC payload; a frame declaring more is
// rejected before allocation.
const MaxFramePayload uint32 = 64 << 20

// Validate checks header self-consistency. Sequence monotonicity needs
// per-stream state and is checked by the reader, not here.
func (h FrameHeader) Validate() error {
	if h.Version != FrameVersion {
		return fmt.Errorf("types: unsupported frame version %d (want %d)", h.Version, FrameVersion)
	}
	if h.Type == "" {
		return fmt.Errorf("types: frame type is required")
	}
	if h.PayloadLength > MaxFramePayload {
		return fmt.Errorf("types: frame payload %d exceeds limit %d", h.PayloadLength, MaxFramePayload)
	}
	return nil
}

// Frame type names exchanged over the Supervisor link (chapter 8).
const (
	FrameHello       = "hello"
	FrameHeartbeat   = "heartbeat"
	FrameRunSubmit   = "run.submit"
	FrameRunCancel   = "run.cancel"
	FrameRunResume   = "run.resume"
	FrameRunGet      = "run.get"
	FrameEventsSince = "events.since"
	FrameEventPush   = "event.push"
	FrameToolCall    = "tool.call"
	FrameToolResult  = "tool.result"
	FrameWorkerExec  = "worker.exec"
	FrameWorkerDone  = "worker.done"
	FrameHealthReq   = "health.req"
	FrameHealthResp  = "health.resp"
	FrameShutdown    = "shutdown"
	FrameError       = "error"
)

// Code is the machine-readable error identifier carried across module
// boundaries.
//
// Adjudication D-8: task 02 defines the same concept as `ErrorCode`, with a
// richer in-process wrapper (`Error` with Unwrap/Is) that 174 call sites
// depend on. Task 02's definition therefore wins, and `Code` is an alias so
// both spellings refer to one type — a value produced by one is accepted by the
// other. Codes that only this package needs are added below; codes that task 02
// already defines (internal, cancelled, tool_failed, ...) are NOT redeclared
// here to avoid duplicate constants.
type Code = ErrorCode

// Additional wire codes contributed by the transport, worker and storage
// boundaries. Task 02's errors.go covers the run-lifecycle vocabulary.
const (
	CodeOK ErrorCode = ""

	// Transport / IPC.
	CodeFrameVersion   ErrorCode = "frame_version"
	CodeFrameTooLarge  ErrorCode = "frame_too_large"
	CodeFrameMalformed ErrorCode = "frame_malformed"
	CodePeerGone       ErrorCode = "peer_gone"
	CodeDeadlinePassed ErrorCode = "deadline_passed"

	// Tool / worker boundary (the parts task 02 does not define).
	CodeToolNotFound        ErrorCode = "tool_not_found"
	CodeToolSchemaInvalid   ErrorCode = "tool_schema_invalid"
	CodePermissionDenied    ErrorCode = "permission_denied"
	CodePermissionNeedsAsk  ErrorCode = "permission_needs_ask"
	CodeSandboxViolation    ErrorCode = "sandbox_violation"
	CodeIdempotencyConflict ErrorCode = "idempotency_conflict"
	CodeWorkerUnavailable   ErrorCode = "worker_unavailable"
	CodeWorkerCrashed       ErrorCode = "worker_crashed"
	CodeResourceExhausted   ErrorCode = "resource_exhausted"
	CodeProcessTreeLeak     ErrorCode = "process_tree_leak"

	// Storage / durability.
	CodeStorageError      ErrorCode = "storage_error"
	CodeSequenceConflict  ErrorCode = "sequence_conflict"
	CodeCheckpointMissing ErrorCode = "checkpoint_missing"
	CodeCasConflict       ErrorCode = "cas_conflict"
	CodeMigrationFailed   ErrorCode = "migration_failed"

	// Provider.
	CodeProviderUnavailable ErrorCode = "provider_unavailable"
	CodeProviderRateLimited ErrorCode = "provider_rate_limited"
	CodeProviderAuthFailed  ErrorCode = "provider_auth_failed"
	CodeContextTooLong      ErrorCode = "context_too_long"
	CodeCircuitOpen         ErrorCode = "circuit_open"

	// Generic.
	CodeTimeout   ErrorCode = "timeout"
	CodeInvariant ErrorCode = "invariant_violation"
)

// ---------------------------------------------------------------------------
// Checkpoint / content addressing (task 03 owns the store; these are the
// cross-module value types)
// ---------------------------------------------------------------------------

// BlobRef is a content-addressed reference to a stored artefact.
type BlobRef struct {
	Hash      [32]byte `json:"hash"`
	Size      int64    `json:"size"`
	MediaType string   `json:"media_type"`
	Mode      uint32   `json:"mode"`
}

// HashHex renders the hash as lowercase hex, the on-disk name under blobs/.
func (b BlobRef) HashHex() string { return fmt.Sprintf("%x", b.Hash[:]) }

// IsZero reports whether the ref is unset.
func (b BlobRef) IsZero() bool { return b.Hash == [32]byte{} }

// Validate rejects a ref that could not correspond to a stored blob.
func (b BlobRef) Validate() error {
	if b.IsZero() {
		return fmt.Errorf("types: BlobRef hash is zero")
	}
	if b.Size < 0 {
		return fmt.Errorf("types: BlobRef size is negative: %d", b.Size)
	}
	if b.MediaType == "" {
		return fmt.Errorf("types: BlobRef media_type is required")
	}
	return nil
}

// FileFingerprint identifies a user file for conflict detection (chapter 11).
//
// Adjudication D-6: the architecture document fixes the four fields
// (path/size/mtime/sha256) but not the mtime unit or the JSON name, so task 03
// chose nanoseconds + "modTime" while this package chose milliseconds +
// "mod_time". Millisecond is used here for consistency with Event timestamps,
// and Exists is retained to distinguish "absent" from "zero-length". Task 03's
// checkpoint package keeps its own fingerprint type; the two are reconciled at
// the boundary. See docs/接口裁决记录.md#d-6.
type FileFingerprint struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"mod_time"` // Unix milliseconds
	// SHA256 is the strongest check and survives a same-size, same-mtime rewrite.
	SHA256 [32]byte `json:"sha256"`
	// Exists distinguishes "file is absent" from "file is empty".
	Exists bool `json:"exists"`
}

// ---------------------------------------------------------------------------
// Context / memory budgets (task 06 owns the manager)
// ---------------------------------------------------------------------------

// TokenBudget is the context-window allocation (chapter 22.1), already scaled to
// the provider's window.
type TokenBudget struct {
	System        int `json:"system"`
	Tools         int `json:"tools"`
	Recent        int `json:"recent"`
	WorkingMem    int `json:"working_mem"`
	Knowledge     int `json:"knowledge"`
	Reasoning     int `json:"reasoning"`
	OutputReserve int `json:"output_reserve"`
}

// Total is the sum of every bucket.
func (b TokenBudget) Total() int {
	return b.System + b.Tools + b.Recent + b.WorkingMem + b.Knowledge + b.Reasoning + b.OutputReserve
}

// MemoryBudget is the global memory ceiling with its shedding ladder (chapter
// 24). The degradation order is normative and must not be reordered.
type MemoryBudget struct {
	SoftLimitBytes int64 `json:"soft_limit_bytes"`
	HardLimitBytes int64 `json:"hard_limit_bytes"`

	MaxMessagesBytes     int64 `json:"max_messages_bytes"`
	MaxToolResultBytes   int64 `json:"max_tool_result_bytes"`
	MaxEventBufferBytes  int64 `json:"max_event_buffer_bytes"`
	MaxCheckpointBytes   int64 `json:"max_checkpoint_bytes"`
	MaxWorkerOutputBytes int64 `json:"max_worker_output_bytes"`
}

// ShedStep is the memory-pressure degradation ladder, least to most disruptive.
type ShedStep int

const (
	// ShedUIDelta drops rebuildable streaming deltas first.
	ShedUIDelta ShedStep = iota
	// ShedTokenizerCache reclaims the bounded BPE cache.
	ShedTokenizerCache
	// ShedWebCache reclaims cached web fetches.
	ShedWebCache
	// ShedIdleBrowsers recycles inactive browser workers (notifies task 05).
	ShedIdleBrowsers
	// ShedBackgroundPriority deprioritises background workers.
	ShedBackgroundPriority
	// ShedRejectBackground refuses new background runs.
	ShedRejectBackground
	// ShedInteractive is the last resort: interactive work is shed only after
	// every step above is exhausted.
	ShedInteractive
)

// ---------------------------------------------------------------------------
// Provider wire types (task 06 owns the transport)
// ---------------------------------------------------------------------------

// MessageRole is the role of a conversation message.
type MessageRole string

const (
	RoleSystem    MessageRole = "system"
	RoleUser      MessageRole = "user"
	RoleAssistant MessageRole = "assistant"
	RoleTool      MessageRole = "tool"
)

// Message is one conversation turn element.
type Message struct {
	Role    MessageRole `json:"role"`
	Content string      `json:"content,omitempty"`
	// ToolCalls is set on an assistant message that requested tools.
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	// ToolCallID links a tool-role message to its request.
	ToolCallID string `json:"tool_call_id,omitempty"`
	// Reasoning carries the model's thinking channel separately so the UI can
	// render it distinctly and compaction can drop it first.
	Reasoning string `json:"reasoning,omitempty"`
}

// CompletionRequest is a provider call. The identifier quartet below
// (RunID/TurnID/RequestID/Attempt) is the alignment point task 07's metrics and
// tracing need to attribute a call to a run.
type CompletionRequest struct {
	// RequestID is unique per attempt: a retry gets a new RequestID with the
	// same RunID/TurnID and an incremented Attempt, so retry metrics stay
	// attributable.
	RequestID string `json:"request_id"`

	RunID     string `json:"run_id"`
	SessionID string `json:"session_id"`
	TurnID    string `json:"turn_id"`

	// Attempt is 1-based.
	Attempt int `json:"attempt"`

	Model    string           `json:"model"`
	Messages []Message        `json:"messages"`
	Tools    []ToolDefinition `json:"tools,omitempty"`

	Temperature float64 `json:"temperature,omitempty"`
	MaxTokens   int     `json:"max_tokens,omitempty"`
}

// StopReason explains why a completion ended.
type StopReason string

const (
	StopEnd       StopReason = "stop"
	StopToolCall  StopReason = "tool_calls"
	StopLength    StopReason = "length"
	StopError     StopReason = "error"
	StopCancelled StopReason = "cancelled"
)

// TokenUsage is normalised usage across providers. CachedPromptTokens is
// tracked separately because prefix-cache hit rate is a first-class metric for
// the byte-stable-prefix design (chapter 23).
type TokenUsage struct {
	PromptTokens       int `json:"prompt_tokens"`
	CompletionTokens   int `json:"completion_tokens"`
	TotalTokens        int `json:"total_tokens"`
	CachedPromptTokens int `json:"cached_prompt_tokens,omitempty"`
	ReasoningTokens    int `json:"reasoning_tokens,omitempty"`
}

// CompletionResponse is the non-streaming provider result.
type CompletionResponse struct {
	RequestID string `json:"request_id"`

	Content   string `json:"content,omitempty"`
	Reasoning string `json:"reasoning,omitempty"`

	ToolCalls []ToolCall `json:"tool_calls,omitempty"`

	StopReason StopReason `json:"stop_reason"`
	Usage      TokenUsage `json:"usage"`

	ErrorCode Code   `json:"error_code,omitempty"`
	ErrorText string `json:"error_text,omitempty"`

	// Emitted reports whether the model produced output before the call ended.
	// Chapter 18 uses it to decide whether a failed attempt is safe to replay:
	// a zero-output failure can be retried without duplicating visible output.
	Emitted bool `json:"emitted,omitempty"`
}

// StreamChunkKind discriminates the streaming union.
type StreamChunkKind string

const (
	ChunkContent   StreamChunkKind = "content"
	ChunkReasoning StreamChunkKind = "reasoning"
	ChunkToolCall  StreamChunkKind = "tool_call"
	ChunkUsage     StreamChunkKind = "usage"
	ChunkDone      StreamChunkKind = "done"
	ChunkError     StreamChunkKind = "error"
)

// StreamChunk is one element of a streaming completion; the coalescible traffic
// chapter 7 permits merging in the UI fan-out path.
type StreamChunk struct {
	RequestID string          `json:"request_id"`
	Kind      StreamChunkKind `json:"kind"`

	Delta string `json:"delta,omitempty"`

	// ToolCall is set for a tool_call chunk; ToolCallIndex disambiguates
	// parallel tool calls streamed in an interleaved fashion.
	ToolCall      *ToolCall `json:"tool_call,omitempty"`
	ToolCallIndex int       `json:"tool_call_index,omitempty"`

	Usage *TokenUsage `json:"usage,omitempty"`

	StopReason StopReason `json:"stop_reason,omitempty"`

	ErrorCode Code   `json:"error_code,omitempty"`
	ErrorText string `json:"error_text,omitempty"`
}

// ProviderCapabilities gates optional features per provider/model.
type ProviderCapabilities struct {
	Streaming     bool `json:"streaming"`
	ToolCalling   bool `json:"tool_calling"`
	Reasoning     bool `json:"reasoning"`
	Vision        bool `json:"vision"`
	PrefixCaching bool `json:"prefix_caching"`
}

// ContextVersions stamps a compaction result so historical runs stay
// interpretable after the algorithms change (chapter 22.2).
type ContextVersions struct {
	ContextVersion      int `json:"context_version"`
	CompressionVersion  int `json:"compression_version"`
	TokenizerVersion    int `json:"tokenizer_version"`
	PromptSchemaVersion int `json:"prompt_schema_version"`
}

// APIKeyRef is an opaque handle to a secret. The plaintext key never appears in
// this package, in an event payload, or in a log line (I12): code that needs a
// key resolves the ref through the SecretsProvider at the moment of use.
type APIKeyRef string
