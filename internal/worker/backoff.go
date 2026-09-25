package worker

import (
	"fmt"
	"math"
	"math/rand"
	"sync"
	"time"
)

// ============================== 重启退避（对齐第 17 章 MCP 节奏） ==============================

// DefaultBackoffSchedule 是任务书明确要求的重启退避节奏：1s→2s→4s→8s→16s→30s 封顶。
// 任务 01 的 supervisor/restart.go 使用同一节奏，此处保持数值一致，避免两处策略漂移。
var DefaultBackoffSchedule = []time.Duration{
	1 * time.Second,
	2 * time.Second,
	4 * time.Second,
	8 * time.Second,
	16 * time.Second,
	30 * time.Second,
}

// Backoff 计算第 n 次（0 起）重启前的等待时长。
// schedule 为空时使用 DefaultBackoffSchedule；超出长度则取最后一项（封顶）。
// jitter 为 0 表示不加抖动（确定性，便于断言）；生产环境建议 0.1~0.2。
type Backoff struct {
	Schedule []time.Duration
	Jitter   float64
	// Rand 为 nil 时使用全局 rand（已自动播种）。测试可注入确定性源。
	Rand *rand.Rand
}

func (b Backoff) schedule() []time.Duration {
	if len(b.Schedule) == 0 {
		return DefaultBackoffSchedule
	}
	return b.Schedule
}

// Delay 返回第 attempt 次重启的等待时长。attempt < 0 视为 0。
func (b Backoff) Delay(attempt int) time.Duration {
	s := b.schedule()
	if attempt < 0 {
		attempt = 0
	}
	idx := attempt
	if idx >= len(s) {
		idx = len(s) - 1
	}
	d := s[idx]
	if b.Jitter <= 0 {
		return d
	}
	// 抖动区间 [d*(1-j), d*(1+j)]，避免所有 Worker 同一时刻同时重连。
	span := float64(d) * b.Jitter
	var r float64
	if b.Rand != nil {
		r = b.Rand.Float64()
	} else {
		r = rand.Float64()
	}
	delta := (r*2 - 1) * span
	out := time.Duration(float64(d) + delta)
	if out < 0 {
		out = 0
	}
	return out
}

// ============================== Circuit Breaker（第 17 章：连续失败 → open → cooldown → half-open） ==============================

// CircuitState 是熔断器状态。
type CircuitState int32

const (
	// CircuitClosed 正常工作。
	CircuitClosed CircuitState = iota
	// CircuitOpen 熔断中，所有调用立即失败（快速失败，不再打外部依赖）。
	CircuitOpen
	// CircuitHalfOpen 冷却结束，放行少量探测请求。
	CircuitHalfOpen
)

func (s CircuitState) String() string {
	switch s {
	case CircuitClosed:
		return "closed"
	case CircuitOpen:
		return "open"
	case CircuitHalfOpen:
		return "half_open"
	default:
		return "unknown"
	}
}

// CircuitConfig 配置熔断器。
type CircuitConfig struct {
	// FailureThreshold 是连续失败多少次后打开熔断。<=0 时用 5。
	FailureThreshold int
	// SuccessThreshold 是 half-open 状态下需要连续成功多少次才闭合。<=0 时用 2。
	SuccessThreshold int
	// Cooldown 是 open 状态持续时间，之后转 half-open。<=0 时用 30s。
	Cooldown time.Duration
	// MaxHalfOpenProbes 是 half-open 状态下允许同时在飞的探测请求数。<=0 时用 1。
	MaxHalfOpenProbes int
}

func (c CircuitConfig) withDefaults() CircuitConfig {
	if c.FailureThreshold <= 0 {
		c.FailureThreshold = 5
	}
	if c.SuccessThreshold <= 0 {
		c.SuccessThreshold = 2
	}
	if c.Cooldown <= 0 {
		c.Cooldown = 30 * time.Second
	}
	if c.MaxHalfOpenProbes <= 0 {
		c.MaxHalfOpenProbes = 1
	}
	return c
}

// CircuitBreaker 是每个外部依赖（MCP server / browser / officecli）独立的熔断器。
// 它是"每个 server 独立故障域"的结构性保证：一个 server 反复失败不会拖垮其他 server。
type CircuitBreaker struct {
	mu       sync.Mutex
	cfg      CircuitConfig
	state    CircuitState
	failures int
	success  int
	probes   int
	openedAt time.Time
	clock    Clock
	// onChange 在状态迁移时回调（用于上报事件/指标）。
	onChange func(from, to CircuitState, reason string)
}

// NewCircuitBreaker 创建熔断器。clock 为 nil 时使用 RealClock。
func NewCircuitBreaker(cfg CircuitConfig, clock Clock, onChange func(from, to CircuitState, reason string)) *CircuitBreaker {
	if clock == nil {
		clock = RealClock{}
	}
	return &CircuitBreaker{cfg: cfg.withDefaults(), clock: clock, onChange: onChange}
}

// State 返回当前状态（会按冷却时间自动从 open 迁移到 half-open）。
func (b *CircuitBreaker) State() CircuitState {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.maybeHalfOpenLocked()
	return b.state
}

// Allow 判断当前是否放行一次调用。返回 false 时调用方应快速失败。
func (b *CircuitBreaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.maybeHalfOpenLocked()
	switch b.state {
	case CircuitClosed:
		return true
	case CircuitOpen:
		return false
	case CircuitHalfOpen:
		if b.probes >= b.cfg.MaxHalfOpenProbes {
			return false
		}
		b.probes++
		return true
	default:
		return false
	}
}

// RecordSuccess 记录一次成功。
func (b *CircuitBreaker) RecordSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case CircuitHalfOpen:
		b.success++
		b.probes--
		if b.probes < 0 {
			b.probes = 0
		}
		if b.success >= b.cfg.SuccessThreshold {
			b.transitionLocked(CircuitClosed, "half-open 探测连续成功")
		}
	case CircuitClosed:
		b.failures = 0
		b.success = 0
	}
}

// RecordFailure 记录一次失败。
func (b *CircuitBreaker) RecordFailure(reason string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case CircuitHalfOpen:
		if b.probes > 0 {
			b.probes--
		}
		b.transitionLocked(CircuitOpen, "half-open 探测失败: "+reason)
	case CircuitClosed:
		b.failures++
		if b.failures >= b.cfg.FailureThreshold {
			b.transitionLocked(CircuitOpen, fmt.Sprintf("连续失败 %d 次", b.failures))
		}
	}
}

// Reset 强制闭合（人工干预/配置变更时使用）。
func (b *CircuitBreaker) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.transitionLocked(CircuitClosed, "人工重置")
}

// ForceOpen 强制打开熔断，无视当前计数。
//
// 用途：Manager 自己维护"时间窗内失败次数"（FailureWindow / FailuresToCircuit），
// 它的判定口径比熔断器内部的连续失败计数更贴近需求。当 Manager 判定应熔断时，
// 用本方法把状态同步给熔断器，从而复用其冷却→half-open 的状态机，
// 而不是在两个地方各维护一套计时逻辑（那样必然漂移）。
func (b *CircuitBreaker) ForceOpen(reason string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures = b.cfg.FailureThreshold
	b.transitionLocked(CircuitOpen, reason)
}

// Snapshot 返回可观测快照。
func (b *CircuitBreaker) Snapshot() (CircuitState, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.maybeHalfOpenLocked()
	return b.state, b.failures
}

// maybeHalfOpenLocked 在冷却期结束后把 open 迁移到 half-open。调用方必须持锁。
func (b *CircuitBreaker) maybeHalfOpenLocked() {
	if b.state != CircuitOpen {
		return
	}
	if b.clock.Now().Sub(b.openedAt) >= b.cfg.Cooldown {
		b.transitionLocked(CircuitHalfOpen, "冷却结束")
	}
}

// transitionLocked 执行状态迁移并回调。调用方必须持锁。
func (b *CircuitBreaker) transitionLocked(to CircuitState, reason string) {
	from := b.state
	if from == to {
		return
	}
	b.state = to
	switch to {
	case CircuitClosed:
		b.failures = 0
		b.success = 0
		b.probes = 0
	case CircuitOpen:
		b.openedAt = b.clock.Now()
		b.success = 0
		b.probes = 0
	case CircuitHalfOpen:
		b.success = 0
		b.probes = 0
	}
	cb := b.onChange
	if cb != nil {
		// 在锁内回调有死锁风险，但回调方只做事件上报（不回调本对象），故保持同步以保证
		// 状态迁移与事件顺序一致；调用方需保证回调不做阻塞 IO。
		cb(from, to, reason)
	}
}

// ============================== 指数退避 + 抖动的小工具 ==============================

// ExpBackoff 返回指数退避时长 base*2^attempt，并以 cap 封顶。jitterRatio 为 0 时不加抖动。
func ExpBackoff(base time.Duration, attempt int, cap time.Duration, jitterRatio float64) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	// 防止 2^attempt 溢出。
	if attempt > 62 {
		attempt = 62
	}
	d := float64(base) * math.Pow(2, float64(attempt))
	if d > float64(cap) {
		d = float64(cap)
	}
	if jitterRatio > 0 {
		d += (rand.Float64()*2 - 1) * d * jitterRatio
	}
	if d < 0 {
		d = 0
	}
	return time.Duration(d)
}
