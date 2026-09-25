package worker

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestPoolSpecConfigReachesWorkerSpec 锁定回归：PoolSpec.Config 必须透传到
// 每次 newWorker 的 Spec.Config。
//
// 背景：Worker 的权限边界（terminal 的 allowed_roots、mcp 的 servers、browser
// 的 headless）全部经由 Spec.Config 注入，而所有高危域都是 fail-closed 的。
// 一旦池不把配置交给 Worker，症状不是报错而是「静默全拒绝」—— 工具注册齐全、
// 调用却一律返回策略拒绝，排查成本极高。本测试是该链路的守门人。
func TestPoolSpecConfigReachesWorkerSpec(t *testing.T) {
	wantRoots := `C:\workspace`
	poolCfg := map[string]any{
		"allowed_roots": []string{wantRoots},
		"max_processes": 3,
	}

	var (
		mu   sync.Mutex
		seen []map[string]any
	)
	factory := func(spec Spec) (Worker, error) {
		mu.Lock()
		seen = append(seen, spec.Config)
		mu.Unlock()
		return &fakeWorker{id: spec.ID, kind: spec.Kind, healthy: true}, nil
	}

	m := NewManager(ManagerConfig{})
	if err := m.RegisterPool(PoolSpec{
		Kind: "terminal", Size: 2, Factory: factory, Config: poolCfg,
	}); err != nil {
		t.Fatalf("RegisterPool: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = m.Stop(context.Background()) }()

	// Start 会为每个 slot 拉起一次 Worker，等两个 slot 都完成创建。
	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		n := len(seen)
		mu.Unlock()
		if n >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 {
		t.Fatal("factory 从未被调用：池没有拉起任何 Worker")
	}
	for i, got := range seen {
		if got == nil {
			t.Fatalf("第 %d 个 Worker 拿到的 Spec.Config 为 nil：池未透传配置，"+
				"所有依赖 allowed_roots 的工具都会 fail-closed 全拒绝", i)
		}
		roots, ok := got["allowed_roots"].([]string)
		if !ok || len(roots) != 1 || roots[0] != wantRoots {
			t.Fatalf("第 %d 个 Worker 的 allowed_roots = %#v，期望 [%s]", i, got["allowed_roots"], wantRoots)
		}
		if got["max_processes"] != 3 {
			t.Fatalf("第 %d 个 Worker 的 max_processes = %#v，期望 3", i, got["max_processes"])
		}
	}
}

// TestPoolSpecConfigSurvivesRestart 验证 Worker 崩溃重启后仍能拿到池配置。
//
// 必要性：重启路径（startSlot → newWorker）与首次启动共用同一条代码路径，
// 但配置来自 slot 上缓存的副本。若缓存只在首次启动时赋值，重启后就会丢配置，
// 表现为「刚启动能用、崩一次之后全部工具被策略拒绝」。
func TestPoolSpecConfigSurvivesRestart(t *testing.T) {
	poolCfg := map[string]any{"allowed_roots": []string{`C:\ws`}}

	var (
		mu   sync.Mutex
		seen []map[string]any
	)
	factory := func(spec Spec) (Worker, error) {
		mu.Lock()
		seen = append(seen, spec.Config)
		mu.Unlock()
		return &fakeWorker{id: spec.ID, kind: spec.Kind, healthy: true}, nil
	}

	m := NewManager(ManagerConfig{})
	if err := m.RegisterPool(PoolSpec{Kind: "terminal", Size: 1, Factory: factory, Config: poolCfg}); err != nil {
		t.Fatalf("RegisterPool: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = m.Stop(context.Background()) }()

	waitCreated := func(n int) {
		deadline := time.Now().Add(3 * time.Second)
		for {
			mu.Lock()
			got := len(seen)
			mu.Unlock()
			if got >= n || time.Now().After(deadline) {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitCreated(1)

	// 模拟崩溃回收，触发一次重启。
	m.mu.RLock()
	p := m.pools["terminal"]
	m.mu.RUnlock()
	if p == nil {
		t.Fatal("未找到 terminal 池")
	}
	m.recycleSlot(context.Background(), p.slots[0], "测试触发回收", "")
	waitCreated(2)

	mu.Lock()
	defer mu.Unlock()
	if len(seen) < 2 {
		t.Fatalf("重启后 factory 只被调用 %d 次，期望至少 2 次", len(seen))
	}
	last := seen[len(seen)-1]
	if last == nil {
		t.Fatal("重启后的 Worker 拿到的 Spec.Config 为 nil：配置缓存在重启路径上丢失")
	}
	roots, _ := last["allowed_roots"].([]string)
	if len(roots) != 1 || roots[0] != `C:\ws` {
		t.Fatalf("重启后 allowed_roots = %#v，期望 [C:\\ws]", last["allowed_roots"])
	}
}
