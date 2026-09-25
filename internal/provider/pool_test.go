package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// newPoolClient 构造一个指向测试 HTTP 服务端的真客户端 —— 池的选路只依赖
// 熔断器状态，不真正发请求，但 Client 的构造约束（BaseURL/Secrets）要保持真实。
func newPoolClient(t *testing.T, id string, breaker *CircuitBreaker) *Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	client, err := NewClient(ClientOptions{
		Config:  ProviderConfig{ID: id, Name: id, BaseURL: server.URL, SecretRef: "ref-" + id},
		Secrets: SecretResolverFunc(func(context.Context, string) (string, error) { return "key", nil }),
		// 单次尝试，避免单测里退避等待。
		Retry:   RetryPolicy{MaxAttempts: 1},
		Breaker: breaker,
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// tripBreaker 用真实失败把熔断器打进 open 态（走 RecordFailure 的正式路径，
// 而不是从外面摆弄内部字段）。
func tripBreaker(t *testing.T, b *CircuitBreaker) {
	t.Helper()
	if b == nil {
		return
	}
	b.FailureThreshold = 1
	b.RecordFailure(ClassRateLimit)
	if b.State() != BreakerOpen {
		t.Fatalf("熔断器应处于 open, got %s", b.State())
	}
}

func TestPoolSelectRotatesAcrossCandidates(t *testing.T) {
	a := newPoolClient(t, "a", nil)
	b := newPoolClient(t, "b", nil)
	c := newPoolClient(t, "c", nil)
	p := NewPool([]PoolCandidate{
		{ID: "a", Model: "ma", Client: a},
		{ID: "b", Model: "mb", Client: b},
		{ID: "c", Model: "mc", Client: c},
	})

	seen := map[string]int{}
	for i := 0; i < 6; i++ {
		target, ok := p.Select(nil)
		if !ok {
			t.Fatalf("第 %d 次选路失败", i)
		}
		if target.Model == "" || target.Provider == nil {
			t.Fatalf("选路结果不完整: %+v", target)
		}
		seen[target.ID]++
	}
	if len(seen) != 3 {
		t.Fatalf("轮询应覆盖全部候选, got %v", seen)
	}
	for id, n := range seen {
		if n != 2 {
			t.Fatalf("候选 %s 被选中 %d 次, want 2", id, n)
		}
	}
}

func TestPoolSelectSkipsOpenBreaker(t *testing.T) {
	a := newPoolClient(t, "a", NewCircuitBreaker(1, time.Hour))
	tripBreaker(t, a.breaker)
	b := newPoolClient(t, "b", NewCircuitBreaker(1, time.Hour))
	p := NewPool([]PoolCandidate{
		{ID: "a", Model: "ma", Client: a},
		{ID: "b", Model: "mb", Client: b},
	})

	for i := 0; i < 4; i++ {
		target, ok := p.Select(nil)
		if !ok {
			t.Fatal("选路失败")
		}
		if target.ID != "b" {
			t.Fatalf("熔断打开的候选应被跳过, got %s", target.ID)
		}
	}
	if got := p.HealthyCount(); got != 1 {
		t.Fatalf("HealthyCount = %d, want 1", got)
	}
}

// TestPoolSelectNeverConsumesHalfOpenSlot 池只读 State 做预检，绝不能替真正
// 的请求占用 half-open 探测名额 —— 否则探测请求会被池自己挤掉。
func TestPoolSelectNeverConsumesHalfOpenSlot(t *testing.T) {
	b := NewCircuitBreaker(1, time.Microsecond) // 1µs 后 open → half-open
	tripBreaker(t, b)
	time.Sleep(2 * time.Millisecond)
	client := newPoolClient(t, "a", b)
	if b.State() != BreakerHalfOpen {
		t.Fatalf("应处于 half-open, got %s", b.State())
	}
	p := NewPool([]PoolCandidate{{ID: "a", Model: "m", Client: client}})

	for i := 0; i < 5; i++ {
		if _, ok := p.Select(nil); !ok {
			t.Fatal("half-open 候选应可选")
		}
	}
	if !b.Allow() {
		t.Fatalf("池的预检泄漏了 half-open 探测名额")
	}
}

// TestPoolSelectFallsBackWhenAllOpen 全部熔断时仍给出「最不坏」的候选，
// 是否放行交给该候选自己的熔断器裁决。
func TestPoolSelectFallsBackWhenAllOpen(t *testing.T) {
	a := newPoolClient(t, "a", NewCircuitBreaker(1, time.Hour))
	tripBreaker(t, a.breaker)
	b := newPoolClient(t, "b", NewCircuitBreaker(1, time.Hour))
	tripBreaker(t, b.breaker)
	p := NewPool([]PoolCandidate{
		{ID: "a", Model: "ma", Client: a},
		{ID: "b", Model: "mb", Client: b},
	})

	target, ok := p.Select(nil)
	if !ok {
		t.Fatalf("全部熔断时也应返回一个候选而不是罢工")
	}
	if target.ID != "a" && target.ID != "b" {
		t.Fatalf("返回了未知候选 %s", target.ID)
	}
}

// TestPoolSelectHonoursOrder 失败转移的排除语义与指定顺序。
func TestPoolSelectHonoursOrder(t *testing.T) {
	a := newPoolClient(t, "a", nil)
	b := newPoolClient(t, "b", nil)
	c := newPoolClient(t, "c", nil)
	p := NewPool([]PoolCandidate{
		{ID: "a", Model: "ma", Client: a},
		{ID: "b", Model: "mb", Client: b},
		{ID: "c", Model: "mc", Client: c},
	})

	target, ok := p.Select([]string{"c", "a"})
	if !ok || target.ID != "c" {
		t.Fatalf("指定顺序应优先取 c, got %+v", target)
	}
	target, _ = p.Select([]string{"c", "a"}, "c")
	if target.ID != "a" {
		t.Fatalf("排除 c 后应取 a, got %s", target.ID)
	}
	if _, ok := p.Select([]string{"c", "a"}, "c", "a"); ok {
		t.Fatalf("候选全部被排除时应返回 false")
	}
}

// TestPoolSelectFallsBackWhenOrderUnknown 指定顺序里全是池中不存在的 ID
// （设置页的分配残留了已删除的服务商）时，必须退回池的默认顺序，而不是把
// 健康候选整体浪费掉直接罢工。
func TestPoolSelectFallsBackWhenOrderUnknown(t *testing.T) {
	a := newPoolClient(t, "a", nil)
	b := newPoolClient(t, "b", nil)
	p := NewPool([]PoolCandidate{
		{ID: "a", Model: "ma", Client: a},
		{ID: "b", Model: "mb", Client: b},
	})

	target, ok := p.Select([]string{"gone", "ghost"})
	if !ok {
		t.Fatalf("order 全是未知 ID 时应回退池默认顺序, got ok=false")
	}
	if target.ID != "a" && target.ID != "b" {
		t.Fatalf("返回了未知候选 %s", target.ID)
	}

	// order 混有未知 ID 与真实 ID 时仍按 pinned 语义优先真实 ID。
	target, ok = p.Select([]string{"gone", "b"})
	if !ok || target.ID != "b" {
		t.Fatalf("order 混合未知与真实 ID 应取真实候选, got %+v ok=%v", target, ok)
	}

	// 回退路径同样尊重排除集：排除 a 后应取 b。
	target, ok = p.Select([]string{"gone"}, "a")
	if !ok || target.ID != "b" {
		t.Fatalf("回退选路应跳过被排除的候选, got %+v ok=%v", target, ok)
	}
}

// TestPoolFailoverLimit 锁死重试上限：最多把候选轮询一遍。
func TestPoolFailoverLimit(t *testing.T) {
	p := NewPool([]PoolCandidate{
		{ID: "a", Model: "ma", Client: newPoolClient(t, "a", nil)},
		{ID: "b", Model: "mb", Client: newPoolClient(t, "b", nil)},
		{ID: "c", Model: "mc", Client: newPoolClient(t, "c", nil)},
	})
	if p.FailoverLimit() != 3 {
		t.Fatalf("FailoverLimit = %d, want 3（候选数）", p.FailoverLimit())
	}
	if got := NewPool(nil).FailoverLimit(); got != 0 {
		t.Fatalf("空池 FailoverLimit = %d, want 0", got)
	}
}

// TestPoolConcurrentSelect 选路并发安全（-race 的守门用例）。
func TestPoolConcurrentSelect(t *testing.T) {
	p := NewPool([]PoolCandidate{
		{ID: "a", Model: "ma", Client: newPoolClient(t, "a", nil)},
		{ID: "b", Model: "mb", Client: newPoolClient(t, "b", nil)},
	})
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_, _ = p.Select(nil)
				_ = p.HealthyCount()
				_ = p.Len()
			}
		}()
	}
	wg.Wait()
}
