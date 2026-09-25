// breaker.go —— CircuitBreaker，防止对持续失败的服务端反复重试（第 18 章调用链第三环）。
//
// 三态：closed 正常放行 → open 全部拒绝 → half-open 放行少量探测。
//
// 关键约束：只有「服务端/传输层」失败才计入熔断统计。
// 400 / invalid API key / context too long 这类确定性错误是客户端问题，
// 计入熔断会导致「改配置也没法恢复」的假死。
package provider

import (
	"errors"
	"sync"
	"time"
)

// ErrCircuitOpen 熔断器处于 open 态时拒绝请求。
var ErrCircuitOpen = errors.New("provider: 熔断器打开，请求被拒绝")

// BreakerState 熔断器状态。
type BreakerState int

const (
	// BreakerClosed 正常放行。
	BreakerClosed BreakerState = iota
	// BreakerOpen 熔断中，全部拒绝。
	BreakerOpen
	// BreakerHalfOpen 半开，放行探测请求。
	BreakerHalfOpen
)

func (s BreakerState) String() string {
	switch s {
	case BreakerOpen:
		return "open"
	case BreakerHalfOpen:
		return "half-open"
	default:
		return "closed"
	}
}

// CircuitBreaker 并发安全的熔断器。
type CircuitBreaker struct {
	mu sync.Mutex

	// FailureThreshold 连续失败达到此值则打开熔断。
	FailureThreshold int
	// SuccessThreshold half-open 下连续成功达到此值则闭合。
	SuccessThreshold int
	// OpenTimeout open 态持续时长，到期转 half-open。
	OpenTimeout time.Duration
	// HalfOpenMaxRequests half-open 态最多同时放行的探测数。
	HalfOpenMaxRequests int

	state            BreakerState
	consecutiveFails int
	consecutiveOK    int
	openedAt         time.Time
	halfOpenInFlight int

	// now 注入时钟便于测试。
	now func() time.Time
	// OnStateChange 状态跃迁回调（供 observability 埋点）；可为 nil。
	OnStateChange func(from, to BreakerState)
}

// NewCircuitBreaker 构造熔断器。默认：连续 5 次失败打开，30s 后 half-open，
// half-open 下连续 2 次成功闭合。
func NewCircuitBreaker(failureThreshold int, openTimeout time.Duration) *CircuitBreaker {
	if failureThreshold <= 0 {
		failureThreshold = 5
	}
	if openTimeout <= 0 {
		openTimeout = 30 * time.Second
	}
	return &CircuitBreaker{
		FailureThreshold:    failureThreshold,
		SuccessThreshold:    2,
		OpenTimeout:         openTimeout,
		HalfOpenMaxRequests: 1,
		state:               BreakerClosed,
		now:                 time.Now,
	}
}

// State 返回当前状态（必要时先做 open → half-open 的时间跃迁）。
func (b *CircuitBreaker) State() BreakerState {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.maybeHalfOpenLocked()
	return b.state
}

// Allow 判断是否放行本次请求。返回 false 时调用方应直接返回 ErrCircuitOpen。
func (b *CircuitBreaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.maybeHalfOpenLocked()

	switch b.state {
	case BreakerOpen:
		return false
	case BreakerHalfOpen:
		if b.halfOpenInFlight >= b.maxHalfOpenLocked() {
			return false
		}
		b.halfOpenInFlight++
		return true
	default:
		return true
	}
}

// RecordSuccess 记录一次成功。
func (b *CircuitBreaker) RecordSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.consecutiveFails = 0
	switch b.state {
	case BreakerHalfOpen:
		b.halfOpenInFlight--
		if b.halfOpenInFlight < 0 {
			b.halfOpenInFlight = 0
		}
		b.consecutiveOK++
		if b.consecutiveOK >= b.successThresholdLocked() {
			b.transitionLocked(BreakerClosed)
		}
	case BreakerOpen:
		// 理论不可达（open 态不放行），保守复位 in-flight。
	default:
		b.consecutiveOK = 0
	}
}

// RecordFailure 记录一次失败。
//
// 只有可重试类别（服务端/传输层）才推进熔断计数 —— 见文件头注释的原因。
func (b *CircuitBreaker) RecordFailure(class ErrorClass) {
	if !countsTowardBreaker(class) {
		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case BreakerHalfOpen:
		// 探测失败 → 立刻回到 open，重新计时。
		b.halfOpenInFlight--
		if b.halfOpenInFlight < 0 {
			b.halfOpenInFlight = 0
		}
		b.consecutiveOK = 0
		if b.consecutiveFails++; b.consecutiveFails >= b.failureThresholdLocked() {
			b.transitionLocked(BreakerOpen)
		}
	case BreakerOpen:
		// 已在 open，重置计时窗口（服务端仍在故障，延长冷却）。
		b.openedAt = b.now()
	default:
		b.consecutiveOK = 0
		b.consecutiveFails++
		if b.consecutiveFails >= b.failureThresholdLocked() {
			b.transitionLocked(BreakerOpen)
		}
	}
}

// ReleaseHalfOpenSlot 在 half-open 探测因非错误原因结束时释放占位。
func (b *CircuitBreaker) ReleaseHalfOpenSlot() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == BreakerHalfOpen {
		b.halfOpenInFlight--
		if b.halfOpenInFlight < 0 {
			b.halfOpenInFlight = 0
		}
	}
}

// Reset 强制闭合熔断器（配置变更、切换服务商时调用）。
func (b *CircuitBreaker) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.transitionLocked(BreakerClosed)
	b.consecutiveFails = 0
	b.consecutiveOK = 0
	b.halfOpenInFlight = 0
}

// Snapshot 返回诊断快照。
func (b *CircuitBreaker) Snapshot() (BreakerState, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.maybeHalfOpenLocked()
	return b.state, b.consecutiveFails
}

// --- 内部 ---

func (b *CircuitBreaker) maybeHalfOpenLocked() {
	if b.state != BreakerOpen {
		return
	}
	if b.now().Sub(b.openedAt) >= b.openTimeoutLocked() {
		b.transitionLocked(BreakerHalfOpen)
	}
}

func (b *CircuitBreaker) transitionLocked(to BreakerState) {
	from := b.state
	if from == to {
		return
	}
	b.state = to
	switch to {
	case BreakerOpen:
		b.openedAt = b.now()
	case BreakerHalfOpen:
		b.halfOpenInFlight = 0
		b.consecutiveOK = 0
	case BreakerClosed:
		b.consecutiveFails = 0
		b.consecutiveOK = 0
		b.halfOpenInFlight = 0
	}
	if b.OnStateChange != nil {
		b.OnStateChange(from, to)
	}
}

func (b *CircuitBreaker) failureThresholdLocked() int {
	if b.FailureThreshold <= 0 {
		return 5
	}
	return b.FailureThreshold
}

func (b *CircuitBreaker) successThresholdLocked() int {
	if b.SuccessThreshold <= 0 {
		return 2
	}
	return b.SuccessThreshold
}

func (b *CircuitBreaker) openTimeoutLocked() time.Duration {
	if b.OpenTimeout <= 0 {
		return 30 * time.Second
	}
	return b.OpenTimeout
}

func (b *CircuitBreaker) maxHalfOpenLocked() int {
	if b.HalfOpenMaxRequests <= 0 {
		return 1
	}
	return b.HalfOpenMaxRequests
}

// countsTowardBreaker 判定错误类别是否计入熔断统计。
func countsTowardBreaker(class ErrorClass) bool {
	switch class {
	case ClassDNS, ClassConnectTimeout, ClassRateLimit, ClassServerError:
		return true
	default:
		return false
	}
}
