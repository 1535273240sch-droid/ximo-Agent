// retry.go —— 错误分类驱动的重试策略（架构文档第 18 章）。
//
// 设计要点：
//  1. 重试与否完全由 ErrorClass 决定，不在重试循环里重复判断错误内容。
//  2. 429 必须遵守 Retry-After；无该头时退避。
//  3. ClassUnknown 保守视为不可重试 —— 未知错误可能已产生副作用。
//  4. ClassContextTooLong 不重试，由上层触发 context compact 后再发新请求。
package provider

import (
	"context"
	"math"
	"math/rand"
	"time"
)

// RetryPolicy 重试策略。
type RetryPolicy struct {
	// MaxAttempts 总尝试次数（含首次）。<=1 表示不重试。
	MaxAttempts int
	// BaseDelay 首次退避基数。
	BaseDelay time.Duration
	// MaxDelay 单次退避上限。
	MaxDelay time.Duration
	// Multiplier 指数退避倍数。
	Multiplier float64
	// MaxRetryAfter 429 的 Retry-After 上限 —— 超过则放弃重试，避免长时间挂起。
	MaxRetryAfter time.Duration

	// rand 注入随机源便于测试；nil 时用全局源。
	rand *rand.Rand
}

// DefaultRetryPolicy 默认策略：4 次尝试、500ms 起、指数 2x、封顶 8s。
//
// 4 次尝试的上限配合熔断器，保证断网/429/5xx 不产生死循环。
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxAttempts:   4,
		BaseDelay:     500 * time.Millisecond,
		MaxDelay:      8 * time.Second,
		Multiplier:    2.0,
		MaxRetryAfter: 60 * time.Second,
	}
}

// Retryable 报告某错误类别是否可重试（第 18 章分类表的可重试列）。
func Retryable(class ErrorClass) bool {
	switch class {
	case ClassDNS, ClassConnectTimeout, ClassRateLimit, ClassServerError:
		return true
	default:
		// 400 / invalid API key / invalid tool schema / context too long /
		// user cancelled / tool side effect / unknown 一律不重试。
		return false
	}
}

// retryDelay 计算第 attempt 次（1-based）失败后的等待时长。
//
// 429 且带 Retry-After 时优先遵守服务端指示；否则指数退避 + 抖动。
func (p RetryPolicy) retryDelay(attempt int, ce *ClassifiedError) time.Duration {
	if ce != nil && ce.Class == ClassRateLimit && ce.RetryAfter > 0 {
		d := ce.RetryAfter
		if p.MaxRetryAfter > 0 && d > p.MaxRetryAfter {
			d = p.MaxRetryAfter
		}
		return d
	}

	base := p.BaseDelay
	if base <= 0 {
		base = 500 * time.Millisecond
	}
	mult := p.Multiplier
	if mult < 1 {
		mult = 2.0
	}

	delay := float64(base) * math.Pow(mult, float64(attempt-1))
	max := float64(p.MaxDelay)
	if max <= 0 {
		max = float64(8 * time.Second)
	}
	if delay > max {
		delay = max
	}

	// 全抖动（full jitter）：避免大量并发请求同时重试冲垮服务端。
	delay = delay * (0.5 + p.jitter()*0.5)
	return time.Duration(delay)
}

func (p RetryPolicy) jitter() float64 {
	if p.rand != nil {
		return p.rand.Float64()
	}
	return rand.Float64()
}

// ShouldRetryAfter 判断 429 的 Retry-After 是否长到不该等（例如超过 MaxRetryAfter）。
func (p RetryPolicy) ShouldGiveUpOnRetryAfter(ce *ClassifiedError) bool {
	if ce == nil || ce.Class != ClassRateLimit {
		return false
	}
	if ce.RetryAfter <= 0 || p.MaxRetryAfter <= 0 {
		return false
	}
	return ce.RetryAfter > p.MaxRetryAfter
}

// backoff 等待指定时长，期间响应 ctx 取消。
func backoff(ctx context.Context, d time.Duration) error {
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
