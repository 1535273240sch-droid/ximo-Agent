package worker

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// ============================== 可观测性与依赖注入钩子 ==============================
//
// 本包刻意不 import 任务 07 的 observability 包、也不 import 任务 03 的 storage 包，
// 全部通过下面的小接口注入，这样 7 路并发开发期间不会有编译期互相阻塞。
// 任务 08 集成时用真实实现替换 Nop* 即可。

// Logger 是最小日志接口，签名与 log/slog 对齐，便于直接适配任务 07 的实现。
//
// 第 21 章红线：调用方传入的 kv 中禁止出现 API Key / 环境变量全量转储。
type Logger interface {
	Debug(msg string, kv ...any)
	Info(msg string, kv ...any)
	Warn(msg string, kv ...any)
	Error(msg string, kv ...any)
}

// SlogLogger 用 *slog.Logger 适配 Logger。
type SlogLogger struct{ L *slog.Logger }

// NewSlogLogger 包装一个 slog.Logger。
func NewSlogLogger(l *slog.Logger) SlogLogger { return SlogLogger{L: l} }

func (s SlogLogger) Debug(msg string, kv ...any) { s.L.Debug(msg, kv...) }
func (s SlogLogger) Info(msg string, kv ...any)  { s.L.Info(msg, kv...) }
func (s SlogLogger) Warn(msg string, kv ...any)  { s.L.Warn(msg, kv...) }
func (s SlogLogger) Error(msg string, kv ...any) { s.L.Error(msg, kv...) }

// NopLogger 丢弃全部日志，测试与"未注入"场景使用。
type NopLogger struct{}

func (NopLogger) Debug(string, ...any) {}
func (NopLogger) Info(string, ...any)  {}
func (NopLogger) Warn(string, ...any)  {}
func (NopLogger) Error(string, ...any) {}

// EventSink 接收 Worker 生命周期事件（崩溃/重启/租约超时/熔断）。
// 最终由任务 07 写入 EventLog；本包只上报。
//
// 事件 payload 里禁止放密钥（第 21 章 / I12 不变量）。
type EventSink interface {
	Emit(ev WorkerEvent)
}

// WorkerEvent 是 Worker 池上报的结构化事件。
type WorkerEvent struct {
	Type     string         `json:"type"`
	WorkerID string         `json:"worker_id"`
	Kind     string         `json:"kind"`
	Slot     int            `json:"slot"`
	CallID   string         `json:"call_id,omitempty"`
	RunID    string         `json:"run_id,omitempty"`
	Reason   string         `json:"reason,omitempty"`
	Attempt  int            `json:"attempt,omitempty"`
	Duration int64          `json:"duration_ms,omitempty"`
	Extra    map[string]any `json:"extra,omitempty"`
	At       time.Time      `json:"at"`
}

// 事件类型常量。
const (
	EventWorkerStarted   = "worker.started"
	EventWorkerReady     = "worker.ready"
	EventWorkerCrashed   = "worker.crashed"
	EventWorkerRestarted = "worker.restarted"
	EventWorkerStopped   = "worker.stopped"
	EventWorkerRecycled  = "worker.recycled"
	EventLeaseAcquired   = "worker.lease.acquired"
	EventLeaseReleased   = "worker.lease.released"
	EventLeaseExpired    = "worker.lease.expired"
	EventCallRejected    = "worker.call.rejected"
	EventCallResumed     = "worker.call.resumed"
	EventCircuitOpened   = "worker.circuit.opened"
	EventCircuitHalfOpen = "worker.circuit.half_open"
	EventCircuitClosed   = "worker.circuit.closed"
)

// NopEventSink 丢弃全部事件。
type NopEventSink struct{}

func (NopEventSink) Emit(WorkerEvent) {}

// FanoutEventSink 把事件广播给多个 sink。
type FanoutEventSink []EventSink

func (f FanoutEventSink) Emit(ev WorkerEvent) {
	for _, s := range f {
		if s != nil {
			s.Emit(ev)
		}
	}
}

// MetricsSink 接收指标。任务 07 用 Prometheus/OTel 实现。
type MetricsSink interface {
	ObserveCallDuration(kind, workerID, status string, d time.Duration)
	ObserveQueueWait(kind string, d time.Duration)
	IncCounter(name, kind string, delta int64)
	SetGauge(name, kind string, workerID string, v float64)
}

// NopMetricsSink 丢弃全部指标。
type NopMetricsSink struct{}

func (NopMetricsSink) ObserveCallDuration(string, string, string, time.Duration) {}
func (NopMetricsSink) ObserveQueueWait(string, time.Duration)                    {}
func (NopMetricsSink) IncCounter(string, string, int64)                          {}
func (NopMetricsSink) SetGauge(string, string, string, float64)                  {}

// 指标名常量（任务 07 直接复用这些名字，避免两侧各起一套）。
const (
	MetricCallDuration = "worker_call_duration_seconds"
	MetricQueueWait    = "worker_queue_wait_seconds"
	MetricCrashes      = "worker_crashes_total"
	MetricRestarts     = "worker_restarts_total"
	MetricLeaseExpired = "worker_lease_expired_total"
	MetricInFlight     = "worker_inflight"
	MetricReady        = "worker_ready"
	MetricQueued       = "worker_queued"
	MetricCircuitOpen  = "worker_circuit_open"
)

// FailureStore 持久化 Worker 失败记录（第 13 章恢复链条的 "persist worker failure" 步）。
// 任务 03 提供落库实现；开发期用 MemoryFailureStore。
type FailureStore interface {
	PersistFailure(ctx context.Context, f WorkerFailure) error
}

// WorkerFailure 是一条 Worker 失败记录。注意只记录可审计信息，禁止写入敏感 payload。
type WorkerFailure struct {
	WorkerID  string    `json:"worker_id"`
	Kind      string    `json:"kind"`
	CallID    string    `json:"call_id,omitempty"`
	RunID     string    `json:"run_id,omitempty"`
	ErrorCode string    `json:"error_code"`
	Message   string    `json:"message"`
	Attempt   int       `json:"attempt"`
	At        time.Time `json:"at"`
}

// MemoryFailureStore 是进程内失败记录（开发期/测试用）。
type MemoryFailureStore struct {
	mu   sync.Mutex
	recs []WorkerFailure
}

// PersistFailure 实现 FailureStore。
func (m *MemoryFailureStore) PersistFailure(_ context.Context, f WorkerFailure) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recs = append(m.recs, f)
	return nil
}

// Records 返回失败记录快照（测试用）。
func (m *MemoryFailureStore) Records() []WorkerFailure {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]WorkerFailure(nil), m.recs...)
}

// ============================== 时钟抽象（保证 timer/ticker 可停、测试可确定） ==============================

// Clock 抽离时间源，使心跳/租约/退避在测试中可以确定性推进。
// 验收标准"所有 timer/ticker 都有 Stop"由 Ticker 接口直接约束。
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
	NewTicker(d time.Duration) Ticker
}

// Ticker 是可停止的定时器。所有实现必须保证 Stop 幂等。
type Ticker interface {
	C() <-chan time.Time
	Stop()
}

// RealClock 是生产用时钟。
type RealClock struct{}

// Now 实现 Clock。
func (RealClock) Now() time.Time { return time.Now() }

// After 实现 Clock。
func (RealClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// NewTicker 实现 Clock。
func (RealClock) NewTicker(d time.Duration) Ticker { return &realTicker{t: time.NewTicker(d)} }

type realTicker struct{ t *time.Ticker }

func (r *realTicker) C() <-chan time.Time { return r.t.C }
func (r *realTicker) Stop()               { r.t.Stop() }

// ManualClock 是测试用可控时钟：需要显式 Advance 才推进时间。
// 它同时满足 Clock 与 Ticker 语义，便于在单测里精确触发"租约超时/心跳丢失"。
type ManualClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []manualWaiter
	tickers []*manualTicker
}

type manualWaiter struct {
	at time.Time
	ch chan time.Time
}

type manualTicker struct {
	mu   sync.Mutex
	d    time.Duration
	next time.Time
	ch   chan time.Time
	stop bool
}

// NewManualClock 以 t0 为起点创建可控时钟。
func NewManualClock(t0 time.Time) *ManualClock { return &ManualClock{now: t0} }

// Now 实现 Clock。
func (m *ManualClock) Now() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.now
}

// After 实现 Clock。
func (m *ManualClock) After(d time.Duration) <-chan time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	ch := make(chan time.Time, 1)
	at := m.now.Add(d)
	if !at.After(m.now) {
		ch <- m.now
		return ch
	}
	m.waiters = append(m.waiters, manualWaiter{at: at, ch: ch})
	return ch
}

// NewTicker 实现 Clock。d <= 0 时按 1ms 处理，避免忙等。
func (m *ManualClock) NewTicker(d time.Duration) Ticker {
	if d <= 0 {
		d = time.Millisecond
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	t := &manualTicker{d: d, next: m.now.Add(d), ch: make(chan time.Time, 1)}
	m.tickers = append(m.tickers, t)
	return t
}

// Advance 推进时钟并唤醒到期的 waiter/ticker。
func (m *ManualClock) Advance(d time.Duration) {
	m.mu.Lock()
	m.now = m.now.Add(d)
	now := m.now
	remain := m.waiters[:0]
	for _, w := range m.waiters {
		if !w.at.After(now) {
			select {
			case w.ch <- now:
			default:
			}
		} else {
			remain = append(remain, w)
		}
	}
	m.waiters = remain
	tickers := append([]*manualTicker(nil), m.tickers...)
	m.mu.Unlock()

	for _, t := range tickers {
		t.mu.Lock()
		if t.stop {
			t.mu.Unlock()
			continue
		}
		for !t.next.After(now) {
			select {
			case t.ch <- now:
			default:
			}
			t.next = t.next.Add(t.d)
		}
		t.mu.Unlock()
	}
}

func (t *manualTicker) C() <-chan time.Time { return t.ch }

func (t *manualTicker) Stop() {
	t.mu.Lock()
	t.stop = true
	t.mu.Unlock()
}
