package engine

import (
	"sync"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// This file implements the event backpressure policy from doc ch. 7.
//
// The rule: streamed events must never pile up in an unbounded channel, and the
// durable log must not record every character. Events are therefore split in
// two:
//
//	durable    run state, tool started/completed/failed, final answer, error,
//	           checkpoint, cancellation. Always persisted, always delivered,
//	           never merged.
//
//	ephemeral  token deltas, heartbeats, progress. Never persisted by default,
//	           and merged before delivery so that e.g. 1000 token deltas become
//	           a few dozen UI frames.
//
// Mergeable events are folded per (run, type) in a small buffer that flushes on
// a timer or when the buffer for that key fills, whichever comes first. The
// per-key cap is what bounds latency: a token stream flushes at the frame
// interval rather than waiting for a batch that may never fill.

// eventBus fans events out to subscribers with per-run ordering and bounded
// per-subscriber buffers.
type eventBus struct {
	cfg types.BackpressureConfig

	mu   sync.Mutex
	subs map[string]map[uint64]*subscriber
	next uint64
	// closed stops new subscriptions.
	closed bool

	// counters
	emitted   uint64
	dropped   uint64
	coalesced uint64
}

// subscriber is one Events() consumer.
type subscriber struct {
	id  uint64
	run string
	// ch is bounded: a consumer that cannot keep up is disconnected rather
	// than allowed to block the producer, because blocking the producer would
	// stall the engine (the exact failure the architecture forbids).
	ch     chan types.Event
	cancel chan struct{}
	once   sync.Once
	// dropped counts events lost because this subscriber was too slow.
	dropped uint64
}

// newEventBus builds a bus with the given backpressure configuration.
func newEventBus(cfg types.BackpressureConfig) *eventBus {
	return &eventBus{
		cfg:  cfg,
		subs: make(map[string]map[uint64]*subscriber),
	}
}

// Subscribe registers a consumer for a run's events.
func (b *eventBus) Subscribe(runID string, afterSeq uint64) (*subscriber, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, types.NewError(types.CodeEngineClosed, "engine is closed")
	}
	total := 0
	for _, m := range b.subs {
		total += len(m)
	}
	if total >= b.cfg.MaxSubscribers {
		return nil, types.NewError(types.CodeQueueFull,
			"too many event subscribers (%d/%d)", total, b.cfg.MaxSubscribers)
	}

	b.next++
	s := &subscriber{
		id:     b.next,
		run:    runID,
		ch:     make(chan types.Event, b.cfg.StreamBuffer),
		cancel: make(chan struct{}),
	}
	if b.subs[runID] == nil {
		b.subs[runID] = make(map[uint64]*subscriber)
	}
	b.subs[runID][s.id] = s
	return s, nil
}

// Unsubscribe removes a subscriber and closes its channel.
func (b *eventBus) Unsubscribe(s *subscriber) {
	if s == nil {
		return
	}
	b.mu.Lock()
	if m, ok := b.subs[s.run]; ok {
		delete(m, s.id)
		if len(m) == 0 {
			delete(b.subs, s.run)
		}
	}
	b.mu.Unlock()
	s.once.Do(func() {
		close(s.cancel)
		close(s.ch)
	})
}

// Publish delivers an event to the run's subscribers.
//
// Delivery is non-blocking: when a subscriber's buffer is full the event is
// dropped *for that subscriber* and the subscriber is disconnected. Dropping
// is the correct behaviour here because the buffer is a UI delivery queue, and
// the authoritative copy of every durable event is already in the event log,
// which the UI re-reads with Events(afterSeq) after a reconnect.
func (b *eventBus) Publish(ev types.Event) {
	b.mu.Lock()
	b.emitted++
	m := b.subs[ev.RunID]
	targets := make([]*subscriber, 0, len(m))
	for _, s := range m {
		targets = append(targets, s)
	}
	b.mu.Unlock()

	for _, s := range targets {
		select {
		case s.ch <- ev:
		case <-s.cancel:
		default:
			// Buffer full: drop and disconnect so the producer never blocks.
			b.mu.Lock()
			b.dropped++
			s.dropped++
			b.mu.Unlock()
			b.Unsubscribe(s)
		}
	}
}

// Close terminates every subscription.
func (b *eventBus) Close() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	all := make([]*subscriber, 0)
	for _, m := range b.subs {
		for _, s := range m {
			all = append(all, s)
		}
	}
	b.subs = make(map[string]map[uint64]*subscriber)
	b.mu.Unlock()
	for _, s := range all {
		s.once.Do(func() {
			close(s.cancel)
			close(s.ch)
		})
	}
}

// BusStats reports bus counters.
type BusStats struct {
	Emitted     uint64 `json:"emitted"`
	Dropped     uint64 `json:"dropped"`
	Coalesced   uint64 `json:"coalesced"`
	Subscribers int    `json:"subscribers"`
}

// Stats snapshots the bus.
func (b *eventBus) Stats() BusStats {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, m := range b.subs {
		n += len(m)
	}
	return BusStats{Emitted: b.emitted, Dropped: b.dropped, Coalesced: b.coalesced, Subscribers: n}
}

// coalescer folds mergeable events before they reach the bus.
//
// Buffering is keyed by (runID, type) so that a burst of token deltas for one
// run cannot delay another run's deltas, and so that folding never mixes event
// types whose payloads are not interchangeable.
type coalescer struct {
	cfg types.BackpressureConfig
	bus *eventBus

	mu      sync.Mutex
	pending map[string]*pendingFrame
	order   []string
	timer   *time.Timer
	closed  bool
	// wg tracks the flush goroutine so Close can wait for it.
	wg sync.WaitGroup
}

// pendingFrame accumulates one key's mergeable events.
type pendingFrame struct {
	runID string
	typ   types.EventType
	// count is how many source events this frame represents.
	count int
	// content and reasoning accumulate token text.
	content   []byte
	reasoning []byte
	// last carries the newest event, whose envelope the merged frame reuses.
	last types.Event
	// bytes is the accumulated payload size, used to force a flush.
	bytes int
}

// maxFrameBytes forces a flush when a single key accumulates too much text, so
// one chatty run cannot hold a frame open indefinitely.
const maxFrameBytes = 4096

func newCoalescer(cfg types.BackpressureConfig, bus *eventBus) *coalescer {
	return &coalescer{
		cfg:     cfg,
		bus:     bus,
		pending: make(map[string]*pendingFrame),
	}
}

// frameKey identifies a coalescing bucket.
func frameKey(runID string, t types.EventType) string {
	return runID + "\x00" + string(t)
}

// Publish routes an event: durable events go straight to the bus, mergeable
// events go through the frame buffer, and anything else is delivered directly.
func (c *coalescer) Publish(ev types.Event) {
	if ev.Type.Durable() || !ev.Type.Mergeable() {
		c.bus.Publish(ev)
		return
	}

	key := frameKey(ev.RunID, ev.Type)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		c.bus.Publish(ev)
		return
	}
	f, ok := c.pending[key]
	if !ok {
		f = &pendingFrame{runID: ev.RunID, typ: ev.Type}
		c.pending[key] = f
		c.order = append(c.order, key)
	}
	f.count++
	f.last = ev
	if s, _ := ev.Data["content"].(string); s != "" {
		f.content = append(f.content, s...)
		f.bytes += len(s)
	}
	if s, _ := ev.Data["reasoning"].(string); s != "" {
		f.reasoning = append(f.reasoning, s...)
		f.bytes += len(s)
	}
	// Force a flush when either the frame is large or the batch is long enough,
	// so latency stays bounded regardless of traffic shape.
	forceFlush := f.bytes >= maxFrameBytes || f.count >= c.cfg.MaxCoalesce
	if !forceFlush && c.timer == nil {
		c.timer = time.AfterFunc(c.cfg.FrameInterval, c.flush)
	}
	c.mu.Unlock()

	if forceFlush {
		c.flushKey(key)
	}
}

// flush emits every pending frame. It is the timer callback.
func (c *coalescer) flush() {
	c.mu.Lock()
	keys := append([]string(nil), c.order...)
	c.timer = nil
	c.mu.Unlock()
	for _, k := range keys {
		c.flushKey(k)
	}
}

// flushKey emits one frame and removes it from the buffer.
func (c *coalescer) flushKey(key string) {
	c.mu.Lock()
	f, ok := c.pending[key]
	if !ok {
		c.mu.Unlock()
		return
	}
	delete(c.pending, key)
	// Drop from order lazily; flush tolerates stale keys.
	c.mu.Unlock()

	ev := f.last
	ev.Data = map[string]any{
		"content":   string(f.content),
		"reasoning": string(f.reasoning),
	}
	ev.Coalesced = f.count
	ev.Seq = 0 // A merged frame is not a durable sequence point.
	c.bus.mu.Lock()
	c.bus.coalesced += uint64(f.count)
	c.bus.mu.Unlock()
	c.bus.Publish(ev)
}

// Close flushes remaining frames and stops the timer.
func (c *coalescer) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	keys := append([]string(nil), c.order...)
	c.mu.Unlock()
	for _, k := range keys {
		c.flushKey(k)
	}
	c.wg.Wait()
}

// streamOptions carries a subscriber's cancel function for the public API.
type streamHandle struct {
	sub    *subscriber
	bus    *eventBus
	ch     <-chan types.Event
	cancel func()
}

// subscriberChannel returns the delivery channel.
func (s *streamHandle) channel() <-chan types.Event { return s.ch }
