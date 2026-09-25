// limiter.go —— RateLimiter，令牌桶限流（第 18 章调用链第二环）。
//
// 目标：主动把请求速率压在服务商配额之下，减少 429。
// 与熔断器分工：限流器管「发太快」，熔断器管「服务端挂了」。
//
// 注意：限流器只做节流，不解决配额穿透（多进程场景需服务端配合）。
package provider

import (
	"context"
	"sync"
	"time"
)

// RateLimiter 令牌桶限流器，并发安全。
type RateLimiter struct {
	mu sync.Mutex

	// rate 每秒补充的令牌数（<=0 表示不限流）。
	rate float64
	// burst 桶容量（允许的瞬时突发量）。
	burst float64
	// tokens 当前令牌数。
	tokens float64
	// last 上次补充时间。
	last time.Time

	// now 注入时钟便于测试。
	now func() time.Time
	// sleep 注入等待实现便于测试；默认 time.Sleep。
	sleep func(context.Context, time.Duration) error
}

// ErrRateLimiterClosed limiter 被关闭后返回。
var ErrRateLimiterClosed = errClosed{}

type errClosed struct{}

func (errClosed) Error() string { return "provider: 限流器已关闭" }

// NewRateLimiter 构造限流器。ratePerSec<=0 时退化为「不限流」的透传实现。
func NewRateLimiter(ratePerSec float64, burst int) *RateLimiter {
	if burst <= 0 {
		burst = 1
	}
	rl := &RateLimiter{
		rate:  ratePerSec,
		burst: float64(burst),
		now:   time.Now,
		sleep: sleepCtx,
	}
	rl.last = rl.now()
	if rl.rate > 0 {
		// 初始给满一桶，避免冷启动时首个请求被无谓延迟。
		rl.tokens = rl.burst
	}
	return rl
}

// Wait 阻塞直到取得一个令牌或 ctx 结束。返回 ctx 错误表示未取得令牌。
//
// 实现要点：只「预留一次、等待一次」，不在等待后重新预留 ——
// 否则每次循环都会再扣一个令牌，低速率下会退化成无限等待。
func (rl *RateLimiter) Wait(ctx context.Context) error {
	if rl == nil || rl.rate <= 0 {
		return ctx.Err()
	}
	wait := rl.reserve()
	if wait <= 0 {
		return nil
	}
	return rl.sleep(ctx, wait)
}

// TryAcquire 非阻塞尝试取令牌，取不到返回 false（调用方可降级或直接 429 处理）。
func (rl *RateLimiter) TryAcquire() bool {
	if rl == nil || rl.rate <= 0 {
		return true
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.refillLocked()
	if rl.tokens >= 1 {
		rl.tokens--
		return true
	}
	return false
}

// SetRate 动态调整速率（服务商配置变更时调用）。
func (rl *RateLimiter) SetRate(ratePerSec float64, burst int) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.rate = ratePerSec
	if burst > 0 {
		rl.burst = float64(burst)
	}
	rl.refillLocked()
	if rl.tokens > rl.burst {
		rl.tokens = rl.burst
	}
}

// reserve 预留一个令牌，返回需要等待的时长（0 表示可立即使用）。
//
// 令牌不足时允许透支为负数：透支量由 refillLocked 按速率补齐，
// 这样长期平均速率严格等于 rate，而不是「每次都不足就等到满」造成的慢于配置速率。
func (rl *RateLimiter) reserve() time.Duration {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	rl.refillLocked()
	if rl.tokens >= 1 {
		rl.tokens--
		return 0
	}

	// 需要等待补足 1 个令牌的时间。
	missing := 1 - rl.tokens
	wait := time.Duration(missing / rl.rate * float64(time.Second))
	if wait <= 0 {
		wait = time.Millisecond
	}
	rl.tokens--
	return wait
}

func (rl *RateLimiter) refillLocked() {
	now := rl.now()
	elapsed := now.Sub(rl.last)
	if elapsed <= 0 {
		return
	}
	rl.last = now
	rl.tokens += elapsed.Seconds() * rl.rate
	if rl.tokens > rl.burst {
		rl.tokens = rl.burst
	}
}

// Tokens 返回当前可用令牌数（诊断用）。
func (rl *RateLimiter) Tokens() float64 {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.refillLocked()
	return rl.tokens
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
