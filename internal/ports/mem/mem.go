// Package mem provides in-memory implementations of the ports declared in
// package ports, plus controllable fakes for the failure modes task 02 must
// survive.
//
// These exist so task 02 can be built and tested before tasks 03, 04 and 06
// deliver. They are real implementations, not stubs: the event store
// allocates gapless sequences and enforces append-only reads, because the
// engine's recovery logic is only meaningful against a store that behaves
// like the real one. Where a fake deliberately deviates (fault injection,
// crash simulation) the field name says so.
package mem

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/ports"
	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// EventStore is an in-memory ports.EventStore. It is concurrency-safe and
// allocates sequence numbers under a single lock, which is what makes the
// "no gaps, no duplicates" property testable.
type EventStore struct {
	mu     sync.RWMutex
	events map[string][]types.Event
	// nextSeq is the next run-local sequence to hand out.
	nextSeq map[string]uint64

	// Appends counts Append calls, for tests asserting durability boundaries.
	Appends atomic.Int64
	// FailAfter makes Append return an error once the run has this many
	// events, simulating a storage fault mid-run. Zero disables it.
	FailAfter int
	// DropWrites makes Append report success without storing anything, so a
	// test can simulate a lost write.
	DropWrites bool
}

// NewEventStore returns an empty in-memory event store.
func NewEventStore() *EventStore {
	return &EventStore{
		events:  make(map[string][]types.Event),
		nextSeq: make(map[string]uint64),
	}
}

// Append implements ports.EventStore.
func (s *EventStore) Append(_ context.Context, runID string, event types.Event) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.FailAfter > 0 && len(s.events[runID]) >= s.FailAfter {
		return 0, types.NewError(types.CodeInternal, "injected event store failure at %d events", s.FailAfter)
	}

	seq := s.nextSeq[runID] + 1
	s.nextSeq[runID] = seq
	event.Seq = seq
	event.RunID = runID
	if event.SeqInRun == 0 {
		event.SeqInRun = seq
	}
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}
	if !s.DropWrites {
		s.events[runID] = append(s.events[runID], event)
	}
	s.Appends.Add(1)
	return seq, nil
}

// Read implements ports.EventStore.
func (s *EventStore) Read(_ context.Context, runID string, afterSeq uint64, limit int) ([]types.Event, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	all := s.events[runID]
	out := make([]types.Event, 0, len(all))
	for _, e := range all {
		if e.Seq > afterSeq {
			out = append(out, e)
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

// LastSeq implements ports.EventStore.
func (s *EventStore) LastSeq(_ context.Context, runID string) (uint64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.nextSeq[runID], nil
}

// ListRuns implements ports.EventStore.
func (s *EventStore) ListRuns(_ context.Context) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.events))
	for id := range s.events {
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}

// All returns a copy of every stored event for a run, for assertions.
func (s *EventStore) All(runID string) []types.Event {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]types.Event(nil), s.events[runID]...)
}

// TypesOf returns just the event types for a run, for assertions.
func (s *EventStore) TypesOf(runID string) []types.EventType {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]types.EventType, 0, len(s.events[runID]))
	for _, e := range s.events[runID] {
		out = append(out, e.Type)
	}
	return out
}

// Truncate simulates a crash that lost the tail of the log: every event after
// seq is discarded, mirroring a torn write.
func (s *EventStore) Truncate(runID string, seq uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.events[runID][:0]
	for _, e := range s.events[runID] {
		if e.Seq <= seq {
			kept = append(kept, e)
		}
	}
	s.events[runID] = kept
	s.nextSeq[runID] = seq
}

// OutboxStore is an in-memory ports.OutboxStore.
type OutboxStore struct {
	mu      sync.Mutex
	pending []types.Event
	acked   map[string]bool
	// Enqueued counts every enqueue, including duplicates, so tests can assert
	// that a crash-redelivery path did not invent events.
	Enqueued int
}

// NewOutboxStore returns an empty outbox.
func NewOutboxStore() *OutboxStore {
	return &OutboxStore{acked: make(map[string]bool)}
}

// Enqueue implements ports.OutboxStore.
func (s *OutboxStore) Enqueue(_ context.Context, event types.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := ackKey(event.RunID, event.Seq)
	if s.acked[key] {
		return nil
	}
	s.pending = append(s.pending, event)
	s.Enqueued++
	return nil
}

// Pending implements ports.OutboxStore.
func (s *OutboxStore) Pending(_ context.Context, limit int) ([]types.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.pending)
	if limit > 0 && n > limit {
		n = limit
	}
	return append([]types.Event(nil), s.pending[:n]...), nil
}

// Ack implements ports.OutboxStore.
func (s *OutboxStore) Ack(_ context.Context, runID string, seq uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.acked[ackKey(runID, seq)] = true
	kept := s.pending[:0]
	for _, e := range s.pending {
		if !(e.RunID == runID && e.Seq == seq) {
			kept = append(kept, e)
		}
	}
	s.pending = kept
	return nil
}

// Len returns the number of undelivered events.
func (s *OutboxStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pending)
}

func ackKey(runID string, seq uint64) string {
	return fmt.Sprintf("%s#%d", runID, seq)
}

// CheckpointStore is an in-memory ports.CheckpointStore.
type CheckpointStore struct {
	mu     sync.Mutex
	byRun  map[string][]ports.Checkpoint
	unsafe map[string]string
	seq    int
	// Saves counts successful saves.
	Saves int
}

// NewCheckpointStore returns an empty checkpoint store.
func NewCheckpointStore() *CheckpointStore {
	return &CheckpointStore{
		byRun:  make(map[string][]ports.Checkpoint),
		unsafe: make(map[string]string),
	}
}

// Save implements ports.CheckpointStore.
func (s *CheckpointStore) Save(_ context.Context, cp ports.Checkpoint) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	if cp.ID == "" {
		cp.ID = fmt.Sprintf("ckpt_%d", s.seq)
	}
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = time.Now()
	}
	s.byRun[cp.RunID] = append(s.byRun[cp.RunID], cp)
	s.Saves++
	return cp.ID, nil
}

// Latest implements ports.CheckpointStore.
func (s *CheckpointStore) Latest(_ context.Context, runID string) (*ports.Checkpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.byRun[runID]
	if len(list) == 0 {
		return nil, nil
	}
	cp := list[len(list)-1]
	return &cp, nil
}

// Restore implements ports.CheckpointStore. It returns the newest snapshot
// whose Seq is at most seq and which has not been marked unsafe, which is the
// definition of "last safe checkpoint" from doc ch. 11.
func (s *CheckpointStore) Restore(_ context.Context, runID string, seq uint64) (*ports.Checkpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var best *ports.Checkpoint
	for i := range s.byRun[runID] {
		cp := s.byRun[runID][i]
		if cp.Seq > seq {
			continue
		}
		if _, bad := s.unsafe[cp.ID]; bad {
			continue
		}
		if best == nil || cp.Seq > best.Seq {
			c := cp
			best = &c
		}
	}
	return best, nil
}

// MarkUnsafe implements ports.CheckpointStore.
func (s *CheckpointStore) MarkUnsafe(_ context.Context, _ string, checkpointID string, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unsafe[checkpointID] = reason
	return nil
}

// Count returns how many checkpoints a run has.
func (s *CheckpointStore) Count(runID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.byRun[runID])
}

// IdempotencyStore is an in-memory ports.IdempotencyStore with a
// name→class table.
type IdempotencyStore struct {
	mu     sync.RWMutex
	byName map[string]ports.IdempotencyClass
	byCall map[string]ports.IdempotencyClass
	// Default applies to unknown tools. It defaults to NonIdempotent so an
	// unclassified tool is never silently replayed after a crash.
	Default ports.IdempotencyClass
}

// NewIdempotencyStore returns a store pre-loaded with the classifications for
// the tools the P0 scope exercises.
func NewIdempotencyStore() *IdempotencyStore {
	return &IdempotencyStore{
		byName: map[string]ports.IdempotencyClass{
			"file_read":   ports.Idempotent,
			"file_write":  ports.Detectable,
			"file_edit":   ports.Detectable,
			"multi_edit":  ports.Detectable,
			"move_file":   ports.Detectable,
			"file_delete": ports.NonIdempotent,
			"terminal":    ports.NonIdempotent,
			"web_search":  ports.Idempotent,
			"web_fetch":   ports.Idempotent,
			"todo_write":  ports.Idempotent,
			"create_tool": ports.Detectable,
		},
		byCall:  make(map[string]ports.IdempotencyClass),
		Default: ports.NonIdempotent,
	}
}

// SetClass overrides the class for a tool name.
func (s *IdempotencyStore) SetClass(toolName string, c ports.IdempotencyClass) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byName[toolName] = c
}

// SetCallClass pins the class for a specific tool-call ID.
func (s *IdempotencyStore) SetCallClass(toolCallID string, c ports.IdempotencyClass) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byCall[toolCallID] = c
}

// Classify implements ports.IdempotencyStore.
//
// An unknown call ID is reported as not-found rather than as the default class,
// so the caller falls through to ClassifyByName and gets the tool-specific
// answer. Returning the default here would misclassify every call.
func (s *IdempotencyStore) Classify(_ context.Context, toolCallID string) (ports.IdempotencyClass, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if c, ok := s.byCall[toolCallID]; ok {
		return c, nil
	}
	return "", types.NewError(types.CodeNotFound, "no classification pinned for tool call %s", toolCallID)
}

// ClassifyByName implements ports.IdempotencyStore.
func (s *IdempotencyStore) ClassifyByName(_ context.Context, toolName string) (ports.IdempotencyClass, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if c, ok := s.byName[toolName]; ok {
		return c, nil
	}
	return s.Default, nil
}

// ToolRuntime is an in-memory ports.ToolRuntime driven by a handler function.
// Tests use it to model fast tools, hanging tools, panicking tools and failing
// tools without touching a real sandbox.
type ToolRuntime struct {
	// Handler runs the call. A nil Handler returns a canned success.
	Handler func(ctx context.Context, req ports.ToolRequest) (types.ToolResult, error)

	mu    sync.Mutex
	calls []ports.ToolRequest
	// Cancelled records calls whose context was cancelled mid-flight, so tests
	// can assert the loop propagates cancellation into running tools.
	Cancelled int
}

// NewToolRuntime returns a runtime with the given handler.
func NewToolRuntime(handler func(ctx context.Context, req ports.ToolRequest) (types.ToolResult, error)) *ToolRuntime {
	return &ToolRuntime{Handler: handler}
}

// Execute implements ports.ToolRuntime.
func (r *ToolRuntime) Execute(ctx context.Context, req ports.ToolRequest) (types.ToolResult, error) {
	start := time.Now()
	r.mu.Lock()
	r.calls = append(r.calls, req)
	r.mu.Unlock()

	if r.Handler == nil {
		return types.ToolResult{
			ToolCallID: req.ToolCallID, ToolName: req.ToolName,
			Content: "ok", Success: true, Duration: time.Since(start),
		}, nil
	}
	res, err := r.Handler(ctx, req)
	res.ToolCallID = req.ToolCallID
	res.ToolName = req.ToolName
	res.Duration = time.Since(start)
	if ctx.Err() != nil {
		r.mu.Lock()
		r.Cancelled++
		r.mu.Unlock()
	}
	return res, err
}

// Calls returns a copy of every request seen, for assertions.
func (r *ToolRuntime) Calls() []ports.ToolRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ports.ToolRequest(nil), r.calls...)
}

// CallCount returns how many calls were executed.
func (r *ToolRuntime) CallCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

// Provider is an in-memory ports.Provider that replays a scripted list of
// responses, one per round. It models the v1 loop's contract: a round either
// returns tool calls or a final answer.
type Provider struct {
	mu sync.Mutex
	// Script supplies round responses in order. When exhausted, Exhausted is
	// used, which defaults to a final "stop" answer.
	Script []ports.ProviderResponse
	// Exhausted is returned once Script runs out.
	Exhausted ports.ProviderResponse
	// Delivers streams fragments through OnDelta before returning the
	// response, so backpressure and coalescing can be exercised.
	Delivers bool
	// DeltasPerRound is how many token deltas to stream per round when
	// Delivers is set.
	DeltasPerRound int
	// Err makes Complete fail, simulating a transport error.
	Err error
	// Block, when non-nil, is closed by the test to release a blocked round.
	Block chan struct{}
	// Calls records every request for assertions.
	Calls []ports.ProviderRequest
	// round counts consumed script entries.
	round int
}

// NewProvider returns a provider that scripts the given responses.
func NewProvider(script ...ports.ProviderResponse) *Provider {
	return &Provider{
		Script:    script,
		Exhausted: ports.ProviderResponse{FinishReason: ports.FinishStop, Content: "done", Emitted: true},
	}
}

// Complete implements ports.Provider.
func (p *Provider) Complete(ctx context.Context, req ports.ProviderRequest) (ports.ProviderResponse, error) {
	p.mu.Lock()
	p.Calls = append(p.Calls, req)
	idx := p.round
	p.round++
	var resp ports.ProviderResponse
	if idx < len(p.Script) {
		resp = p.Script[idx]
	} else {
		resp = p.Exhausted
	}
	block := p.Block
	delivers := p.Delivers
	deltas := p.DeltasPerRound
	p.mu.Unlock()

	if p.Err != nil {
		return ports.ProviderResponse{}, p.Err
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return ports.ProviderResponse{FinishReason: ports.FinishCancelled}, nil
		}
	}
	if delivers && req.OnDelta != nil {
		for i := 0; i < deltas; i++ {
			select {
			case <-ctx.Done():
				return ports.ProviderResponse{FinishReason: ports.FinishCancelled}, nil
			default:
			}
			req.OnDelta(ports.Delta{Content: "d", Reasoning: "r"})
		}
	}
	if resp.Usage.TotalTokens == 0 {
		resp.Usage = ports.Usage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150}
	}
	if req.OnUsage != nil {
		req.OnUsage(resp.Usage)
	}
	return resp, nil
}

// RoundCount returns how many rounds were requested.
func (p *Provider) RoundCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.Calls)
}

// ContextManager is an in-memory ports.ContextManager. It implements the
// documented tiers honestly enough to test the engine's *decision* logic:
// snip truncates old tool output, compact folds the middle region.
type ContextManager struct {
	mu sync.Mutex
	// Compactions records every requested tier.
	Compactions []types.CompactionTier
	// Err makes Compact fail.
	Err error
	// ForceStuck makes every compaction report Stuck, so the engine's
	// give-up path can be tested.
	ForceStuck bool
}

// NewContextManager returns a fresh context manager.
func NewContextManager() *ContextManager { return &ContextManager{} }

// Compact implements ports.ContextManager.
func (m *ContextManager) Compact(_ context.Context, s ports.ContextSession) (ports.CompactionResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Compactions = append(m.Compactions, s.Tier)
	if m.Err != nil {
		return ports.CompactionResult{}, m.Err
	}
	res := ports.CompactionResult{Tier: s.Tier, Messages: append([]ports.Message(nil), s.Messages...)}
	switch s.Tier {
	case types.TierSnip:
		// Clip tool output beyond the protected tail.
		cut := len(res.Messages) - s.ProtectRecent
		for i := 0; i < cut && i < len(res.Messages); i++ {
			msg := res.Messages[i]
			if msg.Role == ports.RoleTool && len(msg.Content) > 200 {
				msg.Content = msg.Content[:200] + "\n...[snipped]"
				res.Messages[i] = msg
			}
		}
	case types.TierCompact, types.TierForce:
		cut := len(res.Messages) - s.ProtectRecent
		if cut > 0 {
			summary := ports.Message{Role: ports.RoleSystem, Content: fmt.Sprintf("[summary of %d earlier messages]", cut)}
			res.Messages = append([]ports.Message{summary}, res.Messages[cut:]...)
			res.Folded = cut
			res.SummaryMessage = &summary
		}
	}
	res.Stuck = m.ForceStuck
	res.Usage = s.Usage
	return res, nil
}

// CompactionCount returns how many compactions ran.
func (m *ContextManager) CompactionCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.Compactions)
}

// Tiers returns the recorded tiers.
func (m *ContextManager) Tiers() []types.CompactionTier {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]types.CompactionTier(nil), m.Compactions...)
}

// ---------------------------------------------------------------------------
// scripted provider helpers
// ---------------------------------------------------------------------------

// ToolCallRound builds a provider response that requests the given tool calls.
func ToolCallRound(calls ...types.ToolCall) ports.ProviderResponse {
	return ports.ProviderResponse{
		FinishReason: ports.FinishToolCalls,
		Content:      "",
		ToolCalls:    calls,
		Emitted:      true,
		Usage:        ports.Usage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150},
	}
}

// FinalRound builds a provider response that ends the run with an answer.
func FinalRound(answer string) ports.ProviderResponse {
	return ports.ProviderResponse{
		FinishReason: ports.FinishStop,
		Content:      answer,
		Emitted:      true,
		Usage:        ports.Usage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150},
	}
}

// ErrorRound builds a provider response representing a provider-side failure.
func ErrorRound(msg string) ports.ProviderResponse {
	return ports.ProviderResponse{
		FinishReason: ports.FinishError,
		Error:        msg,
		Usage:        ports.Usage{PromptTokens: 100, CompletionTokens: 0, TotalTokens: 100},
	}
}

// NewCall builds a tool call with an ID.
func NewCall(id, name string, args map[string]any) types.ToolCall {
	return types.ToolCall{ID: id, Name: name, Arguments: args}
}

// PanicRuntime returns a ToolRuntime whose handler panics, to exercise the
// tool-goroutine panic boundary.
func PanicRuntime() *ToolRuntime {
	return NewToolRuntime(func(_ context.Context, req ports.ToolRequest) (types.ToolResult, error) {
		panic("tool panicked: " + req.ToolName)
	})
}

// FailingRuntime returns a ToolRuntime whose handler always fails the tool.
func FailingRuntime(msg string) *ToolRuntime {
	return NewToolRuntime(func(_ context.Context, _ ports.ToolRequest) (types.ToolResult, error) {
		return types.ToolResult{Success: false, Error: msg}, nil
	})
}

// SleepyRuntime returns a ToolRuntime that sleeps for d, honouring ctx.
func SleepyRuntime(d time.Duration) *ToolRuntime {
	return NewToolRuntime(func(ctx context.Context, req ports.ToolRequest) (types.ToolResult, error) {
		select {
		case <-time.After(d):
			return types.ToolResult{Success: true, Content: "slept"}, nil
		case <-ctx.Done():
			return types.ToolResult{Success: false, Error: ctx.Err().Error()}, ctx.Err()
		}
	})
}

// DumpJSON is a debug helper that renders a value as indented JSON.
func DumpJSON(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("<marshal error: %v>", err)
	}
	return string(b)
}

// SummarizeEventTypes renders a run's event types as a readable chain, used in
// test failure messages.
func SummarizeEventTypes(events []types.Event) string {
	parts := make([]string, 0, len(events))
	for _, e := range events {
		parts = append(parts, string(e.Type))
	}
	return strings.Join(parts, " → ")
}
