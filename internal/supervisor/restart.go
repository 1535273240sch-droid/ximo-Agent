package supervisor

import (
	"sync"
	"time"
)

// BackoffStrategy 退避算法接口
type BackoffStrategy interface {
	Next(consecutiveFailures int) time.Duration
	Reset()
}

// ExponentialBackoff 严格对齐第17章与第42章的 1s→2s→4s→8s→16s→30s 封顶退避策略
type ExponentialBackoff struct {
	steps []time.Duration
	cap   time.Duration
}

// NewDefaultBackoff 创建默认封顶退避策略 (1s, 2s, 4s, 8s, 16s, 30s 封顶)
func NewDefaultBackoff() *ExponentialBackoff {
	return &ExponentialBackoff{
		steps: []time.Duration{
			1 * time.Second,
			2 * time.Second,
			4 * time.Second,
			8 * time.Second,
			16 * time.Second,
			30 * time.Second,
		},
		cap: 30 * time.Second,
	}
}

// NewBackoffFromSteps 按配置的退避阶梯（单位：秒）构造退避策略。
//
// 之所以让配置驱动而不是写死阶梯：阶梯本身是可调项 —— 测试需要亚秒级阶梯才能跑得快，
// 运维可能需要更长的阶梯。在此之前 `config.SupervisorConfig.RestartBackoffSteps`
// 被声明却从未接入（退避恒为 1s 起步），配置项静默失效。
// 空阶梯回退到第 17 章默认值，避免零值配置造成零延迟重启风暴。
func NewBackoffFromSteps(stepsSeconds []int) *ExponentialBackoff {
	if len(stepsSeconds) == 0 {
		return NewDefaultBackoff()
	}
	steps := make([]time.Duration, 0, len(stepsSeconds))
	for _, s := range stepsSeconds {
		if s <= 0 {
			continue
		}
		steps = append(steps, time.Duration(s)*time.Second)
	}
	if len(steps) == 0 {
		return NewDefaultBackoff()
	}
	return &ExponentialBackoff{steps: steps, cap: steps[len(steps)-1]}
}

// Next 计算第 N 次连续失败对应的退避等待时间
func (b *ExponentialBackoff) Next(consecutiveFailures int) time.Duration {
	if consecutiveFailures <= 0 {
		return b.steps[0]
	}
	idx := consecutiveFailures - 1
	if idx >= len(b.steps) {
		return b.cap
	}
	return b.steps[idx]
}

func (b *ExponentialBackoff) Reset() {
	// 无状态纯计算实现，支持并发
}

// CircuitState 熔断器状态
type CircuitState string

const (
	CircuitClosed   CircuitState = "closed"    // 正常服务中
	CircuitOpen     CircuitState = "open"      // 熔断开启，禁止重启
	CircuitHalfOpen CircuitState = "half_open" // 试验性重试恢复
)

// RestartTracker 追踪单个托管实体的重启历史、退避计算与熔断
type RestartTracker struct {
	mu                  sync.Mutex
	consecutiveFailures int
	maxConsecutiveFails int
	lastFailureTime     time.Time
	lastSuccessTime     time.Time
	stableWindow        time.Duration // 连续稳定运行多长时间后重置失败计数 (默认 60s)
	backoff             BackoffStrategy
	state               CircuitState
	openUntil           time.Time
	cooldownDuration    time.Duration
}

// NewRestartTracker 创建实体重启追踪器
func NewRestartTracker(maxFails int, stableWindow time.Duration) *RestartTracker {
	return NewRestartTrackerWithBackoff(maxFails, stableWindow, NewDefaultBackoff())
}

// NewRestartTrackerWithBackoff 与 NewRestartTracker 相同，但注入指定的退避策略，
// 使调用方（Supervisor）能把 config 里的退避阶梯传进来。
func NewRestartTrackerWithBackoff(maxFails int, stableWindow time.Duration, backoff BackoffStrategy) *RestartTracker {
	if maxFails <= 0 {
		maxFails = 10
	}
	if stableWindow <= 0 {
		stableWindow = 60 * time.Second
	}
	if backoff == nil {
		backoff = NewDefaultBackoff()
	}
	return &RestartTracker{
		maxConsecutiveFails: maxFails,
		stableWindow:        stableWindow,
		backoff:             backoff,
		state:               CircuitClosed,
		cooldownDuration:    60 * time.Second,
	}
}

// RecordFailure 记录一次失败并返回下次应等待的退避时长以及是否允许重试
func (t *RestartTracker) RecordFailure() (delay time.Duration, allowRetry bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := time.Now()
	// 如果距上次成功运行已超过 stableWindow，重置失败计数
	if !t.lastSuccessTime.IsZero() && now.Sub(t.lastSuccessTime) >= t.stableWindow {
		t.consecutiveFailures = 0
		t.state = CircuitClosed
	}

	t.consecutiveFailures++
	t.lastFailureTime = now

	// 达到最大失败次数，熔断打开
	if t.consecutiveFailures >= t.maxConsecutiveFails {
		t.state = CircuitOpen
		t.openUntil = now.Add(t.cooldownDuration)
		return t.backoff.Next(t.consecutiveFailures), false
	}

	delay = t.backoff.Next(t.consecutiveFailures)
	return delay, true
}

// RecordSuccess 记录一次成功启动并稳定运行
func (t *RestartTracker) RecordSuccess() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lastSuccessTime = time.Now()
	t.state = CircuitClosed
	t.consecutiveFailures = 0
}

// CanAttempt 判断当前是否可以尝试重新拉起
func (t *RestartTracker) CanAttempt() bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := time.Now()
	if t.state == CircuitClosed {
		return true
	}
	if t.state == CircuitOpen {
		if now.After(t.openUntil) {
			t.state = CircuitHalfOpen
			return true
		}
		return false
	}
	// Half-open 允许单次探测
	return true
}

// Failures 返回连续失败次数
func (t *RestartTracker) Failures() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.consecutiveFailures
}

// State 返回当前熔断状态
func (t *RestartTracker) State() CircuitState {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.state
}
