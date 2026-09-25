package observability

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

type traceContextKey struct{}

// TraceContext 包含分布式上下文跟踪标头
type TraceContext struct {
	TraceID      string `json:"trace_id"`
	SpanID       string `json:"span_id"`
	ParentSpanID string `json:"parent_span_id,omitempty"`
}

// Span 跨度生命周期接口
type Span interface {
	End()
	SetTag(key string, val any) Span
	RecordError(err error) Span
	Context() TraceContext
}

// Tracer 追踪接口契约
type Tracer interface {
	StartSpan(ctx context.Context, name string) (context.Context, Span)
}

// SpanRecord 结构化跨度数据（用于回溯、导出与树构建）
type SpanRecord struct {
	TraceID      string         `json:"trace_id"`
	SpanID       string         `json:"span_id"`
	ParentSpanID string         `json:"parent_span_id,omitempty"`
	Name         string         `json:"name"`
	StartTime    time.Time      `json:"start_time"`
	EndTime      time.Time      `json:"end_time"`
	DurationMs   float64        `json:"duration_ms"`
	Tags         map[string]any `json:"tags,omitempty"`
	Error        string         `json:"error,omitempty"`
	Children     []*SpanRecord  `json:"children,omitempty"`
}

// MemorySpan 具体 Span 实现
type MemorySpan struct {
	mu     sync.Mutex
	tracer *MemoryTracer
	record *SpanRecord
	ended  bool
}

func (s *MemorySpan) End() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return
	}
	s.ended = true
	s.record.EndTime = time.Now()
	s.record.DurationMs = float64(s.record.EndTime.Sub(s.record.StartTime).Microseconds()) / 1000.0
	s.tracer.recordSpan(s.record)
}

func (s *MemorySpan) SetTag(key string, val any) Span {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.record.Tags == nil {
		s.record.Tags = make(map[string]any)
	}
	s.record.Tags[key] = val
	return s
}

func (s *MemorySpan) RecordError(err error) Span {
	if err == nil {
		return s
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record.Error = err.Error()
	if s.record.Tags == nil {
		s.record.Tags = make(map[string]any)
	}
	s.record.Tags["error"] = true
	return s
}

func (s *MemorySpan) Context() TraceContext {
	return TraceContext{
		TraceID:      s.record.TraceID,
		SpanID:       s.record.SpanID,
		ParentSpanID: s.record.ParentSpanID,
	}
}

const (
	// DefaultMaxTraces 默认内存中保留的最大 Trace 数量（防止内存泄漏）
	DefaultMaxTraces = 500
)

// MemoryTracer 内存态 Trace 收集器（具备 LRU/容量淘汰机制，杜绝内存持续增长）
type MemoryTracer struct {
	mu         sync.RWMutex
	maxTraces  int
	traceOrder []string                  // 保序队列用于 LRU/FIFO 淘汰
	byTrace    map[string][]*SpanRecord  // traceID -> spans
	lastSeen   map[string]time.Time      // traceID -> 最后更新时间
}

// NewMemoryTracer 创建具备淘汰机制的内存 Tracer
func NewMemoryTracer() *MemoryTracer {
	return &MemoryTracer{
		maxTraces:  DefaultMaxTraces,
		traceOrder: make([]string, 0, DefaultMaxTraces),
		byTrace:    make(map[string][]*SpanRecord),
		lastSeen:   make(map[string]time.Time),
	}
}

// SetCapacity 设置最大保留 Trace 树数量（超出时淘汰最早的 Trace）
func (t *MemoryTracer) SetCapacity(maxTraces int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if maxTraces <= 0 {
		maxTraces = 10
	}
	t.maxTraces = maxTraces
	t.evictOldTracesLocked()
}

func (t *MemoryTracer) recordSpan(rec *SpanRecord) {
	t.mu.Lock()
	defer t.mu.Unlock()

	traceID := rec.TraceID
	if _, exists := t.byTrace[traceID]; !exists {
		t.traceOrder = append(t.traceOrder, traceID)
	}
	t.byTrace[traceID] = append(t.byTrace[traceID], rec)
	t.lastSeen[traceID] = rec.EndTime

	t.evictOldTracesLocked()
}

func (t *MemoryTracer) evictOldTracesLocked() {
	for len(t.traceOrder) > t.maxTraces {
		oldestID := t.traceOrder[0]
		t.traceOrder = t.traceOrder[1:]
		delete(t.byTrace, oldestID)
		delete(t.lastSeen, oldestID)
	}
}

// CleanExpired 清除超过指定生存时间 (TTL) 的旧 Trace
func (t *MemoryTracer) CleanExpired(ttl time.Duration) int {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := time.Now()
	expiredCount := 0
	var remainingOrder []string

		for _, traceID := range t.traceOrder {
			lastTime := t.lastSeen[traceID]
			if ttl <= 0 || now.Sub(lastTime) >= ttl {
				delete(t.byTrace, traceID)
				delete(t.lastSeen, traceID)
				expiredCount++
			} else {
				remainingOrder = append(remainingOrder, traceID)
			}
		}
	t.traceOrder = remainingOrder
	return expiredCount
}

// ActiveTracesCount 返回当前活跃 Trace 数
func (t *MemoryTracer) ActiveTracesCount() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.byTrace)
}

// StartSpan 开启一个新 Span
func (t *MemoryTracer) StartSpan(ctx context.Context, name string) (context.Context, Span) {
	parentCtx, hasParent := FromContext(ctx)

	traceID := parentCtx.TraceID
	parentSpanID := ""
	if !hasParent || traceID == "" {
		traceID = newRandomID(16)
	} else {
		parentSpanID = parentCtx.SpanID
	}
	spanID := newRandomID(8)

	rec := &SpanRecord{
		TraceID:      traceID,
		SpanID:       spanID,
		ParentSpanID: parentSpanID,
		Name:         name,
		StartTime:    time.Now(),
		Tags:         make(map[string]any),
	}

	span := &MemorySpan{
		tracer: t,
		record: rec,
	}

	newTraceCtx := TraceContext{
		TraceID:      traceID,
		SpanID:       spanID,
		ParentSpanID: parentSpanID,
	}

	return context.WithValue(ctx, traceContextKey{}, newTraceCtx), span
}

// GetTraceTree 将某一 traceID 的所有 span 组装成树状结构
func (t *MemoryTracer) GetTraceTree(traceID string) []*SpanRecord {
	t.mu.RLock()
	defer t.mu.RUnlock()

	records := t.byTrace[traceID]
	if len(records) == 0 {
		return nil
	}

	// 复制 records 避免并发修改
	cloneMap := make(map[string]*SpanRecord, len(records))
	for _, r := range records {
		cloneMap[r.SpanID] = &SpanRecord{
			TraceID:      r.TraceID,
			SpanID:       r.SpanID,
			ParentSpanID: r.ParentSpanID,
			Name:         r.Name,
			StartTime:    r.StartTime,
			EndTime:      r.EndTime,
			DurationMs:   r.DurationMs,
			Tags:         copyMap(r.Tags),
			Error:        r.Error,
			Children:     make([]*SpanRecord, 0),
		}
	}

	var roots []*SpanRecord
	for _, item := range cloneMap {
		if item.ParentSpanID == "" {
			roots = append(roots, item)
		} else if parent, exists := cloneMap[item.ParentSpanID]; exists {
			parent.Children = append(parent.Children, item)
		} else {
			// 孤立 span，当作 root
			roots = append(roots, item)
		}
	}

	return roots
}

// AllSpans 返回当前保留的所有 Span（扁平列表）
func (t *MemoryTracer) AllSpans() []*SpanRecord {
	t.mu.RLock()
	defer t.mu.RUnlock()

	var res []*SpanRecord
	for _, spans := range t.byTrace {
		res = append(res, spans...)
	}
	return res
}

// Reset 清空跟踪缓存
func (t *MemoryTracer) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.traceOrder = make([]string, 0, t.maxTraces)
	t.byTrace = make(map[string][]*SpanRecord)
	t.lastSeen = make(map[string]time.Time)
}

func copyMap(src map[string]any) map[string]any {
	if src == nil {
		return nil
	}
	dst := make(map[string]any, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func newRandomID(bytesLen int) string {
	b := make([]byte, bytesLen)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// FromContext 从 Context 提取 TraceContext
func FromContext(ctx context.Context) (TraceContext, bool) {
	if ctx == nil {
		return TraceContext{}, false
	}
	val := ctx.Value(traceContextKey{})
	if val == nil {
		return TraceContext{}, false
	}
	tc, ok := val.(TraceContext)
	return tc, ok
}

// WithTraceContext 将 TraceContext 注入 Context
func WithTraceContext(ctx context.Context, tc TraceContext) context.Context {
	return context.WithValue(ctx, traceContextKey{}, tc)
}

// 全局默认 Tracer 单例
var (
	defaultTracer   Tracer = NewMemoryTracer()
	defaultTracerMu sync.RWMutex
)

// DefaultTracer 获取全局默认 Tracer
func DefaultTracer() Tracer {
	defaultTracerMu.RLock()
	defer defaultTracerMu.RUnlock()
	return defaultTracer
}

// SetDefaultTracer 设置全局默认 Tracer
func SetDefaultTracer(tr Tracer) {
	defaultTracerMu.Lock()
	defer defaultTracerMu.Unlock()
	defaultTracer = tr
}

// StartSpan 全局便捷方法
func StartSpan(ctx context.Context, name string) (context.Context, Span) {
	return DefaultTracer().StartSpan(ctx, name)
}
