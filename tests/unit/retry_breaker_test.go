package unit

import (
	"errors"
	"sync"
	"testing"
	"time"
)

type ErrorCategory int

const (
	ErrCategoryRetryable ErrorCategory = iota
	ErrCategoryNonRetryable
)

func ClassifyError(statusCode int, err error) ErrorCategory {
	if statusCode == 429 || statusCode == 500 || statusCode == 502 || statusCode == 503 || statusCode == 504 {
		return ErrCategoryRetryable
	}
	if statusCode == 400 || statusCode == 401 || statusCode == 403 {
		return ErrCategoryNonRetryable
	}
	if err != nil && (err.Error() == "dns timeout" || err.Error() == "connect timeout") {
		return ErrCategoryRetryable
	}
	return ErrCategoryNonRetryable
}

type CircuitState string

const (
	StateClosed   CircuitState = "CLOSED"
	StateOpen     CircuitState = "OPEN"
	StateHalfOpen CircuitState = "HALF_OPEN"
)

type CircuitBreaker struct {
	mu           sync.Mutex
	state        CircuitState
	failureCount int
	threshold    int
	cooldown     time.Duration
	lastOpenTime time.Time
}

func NewCircuitBreaker(threshold int, cooldown time.Duration) *CircuitBreaker {
	return &CircuitBreaker{
		state:     StateClosed,
		threshold: threshold,
		cooldown:  cooldown,
	}
}

func (cb *CircuitBreaker) AllowRequest() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	now := time.Now()
	if cb.state == StateOpen {
		if now.Sub(cb.lastOpenTime) > cb.cooldown {
			cb.state = StateHalfOpen
			return true
		}
		return false
	}
	return true
}

func (cb *CircuitBreaker) RecordResult(success bool) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	if success {
		cb.failureCount = 0
		cb.state = StateClosed
		return
	}

	cb.failureCount++
	if cb.failureCount >= cb.threshold || cb.state == StateHalfOpen {
		cb.state = StateOpen
		cb.lastOpenTime = time.Now()
	}
}

func TestErrorClassificationAndCircuitBreaker(t *testing.T) {
	// 1. 错误分类测试
	if ClassifyError(429, nil) != ErrCategoryRetryable {
		t.Fatalf("expected 429 to be retryable")
	}
	if ClassifyError(500, nil) != ErrCategoryRetryable {
		t.Fatalf("expected 500 to be retryable")
	}
	if ClassifyError(400, nil) != ErrCategoryNonRetryable {
		t.Fatalf("expected 400 to be non-retryable")
	}
	if ClassifyError(0, errors.New("dns timeout")) != ErrCategoryRetryable {
		t.Fatalf("expected dns timeout to be retryable")
	}

	// 2. 熔断器测试
	cb := NewCircuitBreaker(3, 50*time.Millisecond)

	// 连续记录 3 次失败
	cb.RecordResult(false)
	cb.RecordResult(false)
	cb.RecordResult(false)

	// 此时应已 Open，拒绝请求
	if cb.AllowRequest() {
		t.Fatalf("circuit should be OPEN, rejecting requests")
	}

	// 等待冷却期过去进入 Half-Open
	time.Sleep(60 * time.Millisecond)
	if !cb.AllowRequest() {
		t.Fatalf("circuit should allow request in HALF_OPEN state")
	}

	// Half-Open 探测成功，闭合
	cb.RecordResult(true)
	if !cb.AllowRequest() || cb.state != StateClosed {
		t.Fatalf("circuit should recover to CLOSED after success")
	}
}
