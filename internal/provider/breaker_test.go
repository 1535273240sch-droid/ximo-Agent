package provider

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestBreakerOpensAfterThreshold 确认连续失败达到阈值后打开。
func TestBreakerOpensAfterThreshold(t *testing.T) {
	b := NewCircuitBreaker(3, time.Minute)

	// 阈值内的失败不应打开。
	for i := 0; i < 2; i++ {
		if !b.Allow() {
			t.Fatalf("第 %d 次不应被拒绝", i+1)
		}
		b.RecordFailure(ClassServerError)
	}
	if b.State() != BreakerClosed {
		t.Fatalf("2 次失败后应为 closed，实际 %v", b.State())
	}

	// 第 3 次失败达到阈值 → open。
	if !b.Allow() {
		t.Fatal("第 3 次应放行（此时尚未打开）")
	}
	b.RecordFailure(ClassServerError)
	if b.State() != BreakerOpen {
		t.Fatalf("3 次失败后应为 open，实际 %v", b.State())
	}

	// open 后必须拒绝。
	if b.Allow() {
		t.Fatal("open 态必须拒绝请求")
	}
}

// TestBreakerIgnoresNonRetryableFailures 确认客户端错误不计入熔断。
//
// 这是本文件最关键的断言：如果把 400/invalid key 计入熔断，
// 会出现「用户改完配置也无法恢复」的假死 —— 必须只统计服务端/传输层失败。
func TestBreakerIgnoresNonRetryableFailures(t *testing.T) {
	b := NewCircuitBreaker(2, time.Minute)

	for i := 0; i < 10; i++ {
		if !b.Allow() {
			t.Fatal("客户端错误不应打开熔断")
		}
		b.RecordFailure(ClassBadRequest)
		b.RecordFailure(ClassAuthInvalidKey)
		b.RecordFailure(ClassContextTooLong)
		b.RecordFailure(ClassInvalidToolSchema)
	}
	if b.State() != BreakerClosed {
		t.Fatalf("客户端错误后应保持 closed，实际 %v", b.State())
	}
}

// TestBreakerHalfOpenRecovery 确认 open → half-open → closed 的恢复路径。
func TestBreakerHalfOpenRecovery(t *testing.T) {
	var now atomic.Int64
	base := time.Now()
	now.Store(base.UnixNano())

	b := NewCircuitBreaker(2, 100*time.Millisecond)
	b.now = func() time.Time { return time.Unix(0, now.Load()) }

	// 打开熔断。
	b.Allow()
	b.RecordFailure(ClassServerError)
	b.Allow()
	b.RecordFailure(ClassServerError)
	if b.State() != BreakerOpen {
		t.Fatalf("应为 open，实际 %v", b.State())
	}

	// 时间推进 → 应转 half-open。
	now.Store(base.Add(200 * time.Millisecond).UnixNano())
	if b.State() != BreakerHalfOpen {
		t.Fatalf("超时后应为 half-open，实际 %v", b.State())
	}

	// half-open 下连续成功 2 次 → 闭合。
	if !b.Allow() {
		t.Fatal("half-open 应放行探测请求")
	}
	b.RecordSuccess()
	if !b.Allow() {
		t.Fatal("half-open 应继续放行探测请求")
	}
	b.RecordSuccess()

	if b.State() != BreakerClosed {
		t.Fatalf("连续成功后应闭合，实际 %v", b.State())
	}
}

// TestBreakerHalfOpenFailureReopens 确认 half-open 探测失败立刻回到 open。
func TestBreakerHalfOpenFailureReopens(t *testing.T) {
	var now atomic.Int64
	base := time.Now()
	now.Store(base.UnixNano())

	b := NewCircuitBreaker(1, 50*time.Millisecond)
	b.now = func() time.Time { return time.Unix(0, now.Load()) }

	b.Allow()
	b.RecordFailure(ClassServerError)
	if b.State() != BreakerOpen {
		t.Fatal("应为 open")
	}

	now.Store(base.Add(100 * time.Millisecond).UnixNano())
	if b.State() != BreakerHalfOpen {
		t.Fatalf("应为 half-open，实际 %v", b.State())
	}

	b.Allow()
	b.RecordFailure(ClassServerError)
	if b.State() != BreakerOpen {
		t.Fatalf("探测失败应回到 open，实际 %v", b.State())
	}
}

// TestBreakerStateChangeCallback 确认状态跃迁回调被触发（observability 埋点依赖它）。
func TestBreakerStateChangeCallback(t *testing.T) {
	b := NewCircuitBreaker(1, time.Minute)
	var transitions []string
	b.OnStateChange = func(from, to BreakerState) {
		transitions = append(transitions, from.String()+"→"+to.String())
	}

	b.Allow()
	b.RecordFailure(ClassServerError)

	if len(transitions) != 1 || transitions[0] != "closed→open" {
		t.Fatalf("跃迁记录 = %v, want [closed→open]", transitions)
	}
}

// TestBreakerConcurrentAccess 确认并发下无数据竞争（配合 -race）。
func TestBreakerConcurrentAccess(t *testing.T) {
	b := NewCircuitBreaker(50, 10*time.Millisecond)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				if b.Allow() {
					if (i+j)%3 == 0 {
						b.RecordFailure(ClassServerError)
					} else {
						b.RecordSuccess()
					}
				}
				_ = b.State()
			}
		}(i)
	}
	wg.Wait()
}

// ---------------------------------------------------------------------------
// RateLimiter
// ---------------------------------------------------------------------------

// TestLimiterThrottles 确认限流器按速率节流。
func TestLimiterThrottles(t *testing.T) {
	var now atomic.Int64
	base := time.Now()
	now.Store(base.UnixNano())

	rl := NewRateLimiter(10, 1) // 10/s，桶容量 1
	rl.now = func() time.Time { return time.Unix(0, now.Load()) }
	var slept atomic.Int64
	rl.sleep = func(ctx context.Context, d time.Duration) error {
		slept.Add(int64(d))
		now.Add(int64(d))
		return nil
	}

	// 首个请求用初始令牌。
	if err := rl.Wait(context.Background()); err != nil {
		t.Fatalf("首个请求不应报错: %v", err)
	}
	// 第二个请求需要等约 100ms。
	if err := rl.Wait(context.Background()); err != nil {
		t.Fatalf("第二个请求不应报错: %v", err)
	}
	if slept.Load() == 0 {
		t.Fatal("第二个请求应产生等待（限流生效）")
	}
}

// TestLimiterUnlimitedWhenRateZero 确认 rate=0 时退化为不限流。
func TestLimiterUnlimitedWhenRateZero(t *testing.T) {
	rl := NewRateLimiter(0, 1)
	ctx := context.Background()
	start := time.Now()
	for i := 0; i < 1000; i++ {
		if err := rl.Wait(ctx); err != nil {
			t.Fatalf("不限流模式不应报错: %v", err)
		}
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("不限流模式不应阻塞，实际耗时 %v", elapsed)
	}
	if !rl.TryAcquire() {
		t.Fatal("不限流模式 TryAcquire 应恒为 true")
	}
}

// TestLimiterWaitRespectsContext 确认等待期间 ctx 取消能立即返回。
func TestLimiterWaitRespectsContext(t *testing.T) {
	rl := NewRateLimiter(0.001, 1) // 极慢：1000s 一个令牌
	ctx, cancel := context.WithCancel(context.Background())

	// 耗尽初始令牌。
	if err := rl.Wait(context.Background()); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("首个请求不应报错: %v", err)
	}

	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	err := rl.Wait(ctx)
	if err == nil {
		t.Fatal("期望 ctx 取消错误")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("ctx 取消后应立刻返回，实际 %v", elapsed)
	}
}

// TestLimiterTryAcquire 确认非阻塞取令牌。
func TestLimiterTryAcquire(t *testing.T) {
	rl := NewRateLimiter(0.001, 2)
	if !rl.TryAcquire() {
		t.Fatal("初始令牌应可取")
	}
	if !rl.TryAcquire() {
		t.Fatal("第二个初始令牌应可取")
	}
	if rl.TryAcquire() {
		t.Fatal("令牌耗尽后 TryAcquire 应为 false")
	}
}

// TestLimiterConcurrent 确认并发下无竞态且不超发。
func TestLimiterConcurrent(t *testing.T) {
	rl := NewRateLimiter(1000, 10) // 快，避免测试慢
	ctx := context.Background()

	var wg sync.WaitGroup
	var acquired atomic.Int64
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := rl.Wait(ctx); err == nil {
				acquired.Add(1)
			}
		}()
	}
	wg.Wait()

	if acquired.Load() != 50 {
		t.Fatalf("高速率下应全部取到令牌，实际 %d/50", acquired.Load())
	}
}
