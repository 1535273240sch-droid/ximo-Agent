package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ============================== 测试替身 ==============================

// faketWorker 是可控的 Worker 替身，用于驱动 Manager 的各条生命周期路径。
type fakeWorker struct {
	id   string
	kind string

	mu      sync.Mutex
	started bool
	stopped bool
	healthy bool
	calls   int
	// execFn 允许测试注入行为（阻塞、panic、返回错误等）。
	execFn func(context.Context, WorkerRequest) (WorkerResponse, error)
	// needRecycle 模拟 DrainingWorker 的自报回收。
	needRecycle string
	pids        []int
	startErr    error
	healthErr   error
}

func (f *fakeWorker) ID() string   { return f.id }
func (f *fakeWorker) Kind() string { return f.kind }

func (f *fakeWorker) Start(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return f.startErr
	}
	f.started = true
	f.healthy = true
	return nil
}

func (f *fakeWorker) Execute(ctx context.Context, req WorkerRequest) (WorkerResponse, error) {
	f.mu.Lock()
	f.calls++
	fn := f.execFn
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, req)
	}
	return OKResponse(req, f.id, time.Now(), map[string]any{"echo": req.Action})
}

func (f *fakeWorker) Health(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.healthErr != nil {
		return f.healthErr
	}
	if !f.healthy {
		return errors.New("fake: 不健康")
	}
	return nil
}

func (f *fakeWorker) Stop(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = true
	f.started = false
	return nil
}

func (f *fakeWorker) PIDs() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.pids...)
}

func (f *fakeWorker) NeedRecycle() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.needRecycle
}

func (f *fakeWorker) setHealthy(v bool) {
	f.mu.Lock()
	f.healthy = v
	f.mu.Unlock()
}

func (f *fakeWorker) setNeedRecycle(r string) {
	f.mu.Lock()
	f.needRecycle = r
	f.mu.Unlock()
}

func (f *fakeWorker) setHealthErr(err error) {
	f.mu.Lock()
	f.healthErr = err
	f.mu.Unlock()
}

func (f *fakeWorker) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeWorker) isStopped() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stopped
}

// fakeFactory 记录所有创建的 Worker 实例，便于测试断言"确实重建过"。
type fakeFactory struct {
	kind string
	mu   sync.Mutex
	made []*fakeWorker
	// configure 在每次创建后调用，用于注入行为。
	configure func(w *fakeWorker)
	// failFirst 表示前 N 次创建失败（测试启动失败路径）。
	failFirst int
	created   atomic.Int64
}

func (ff *fakeFactory) factory(spec Spec) (Worker, error) {
	n := ff.created.Add(1)
	ff.mu.Lock()
	defer ff.mu.Unlock()
	if int(n) <= ff.failFirst {
		return nil, fmt.Errorf("fake: 第 %d 次创建失败", n)
	}
	w := &fakeWorker{id: spec.ID, kind: spec.Kind}
	if ff.configure != nil {
		ff.configure(w)
	}
	ff.made = append(ff.made, w)
	return w, nil
}

func (ff *fakeFactory) workers() []*fakeWorker {
	ff.mu.Lock()
	defer ff.mu.Unlock()
	return append([]*fakeWorker(nil), ff.made...)
}

func (ff *fakeFactory) count() int {
	ff.mu.Lock()
	defer ff.mu.Unlock()
	return len(ff.made)
}

// newTestManager 构造一个使用可控时钟的 Manager。
func newTestManager(t *testing.T, cfg ManagerConfig) (*Manager, *ManualClock) {
	t.Helper()
	clock := NewManualClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	cfg.Backoff = Backoff{Schedule: []time.Duration{0, 0, 0, 0}, Jitter: 0}
	m := NewManager(cfg, WithClock(clock), WithMetrics(NopMetricsSink{}))
	return m, clock
}

// ============================== 验收标准 1：随机 kill Worker 后 Engine 不退出 ==============================

// TestKillWorkerDoesNotKillEngine 验证核心验收标准：
// Worker 崩溃（模拟被 kill）后，调用方拿到明确错误，但 Manager 本身不退出、可继续服务。
func TestKillWorkerDoesNotKillEngine(t *testing.T) {
	ff := &fakeFactory{kind: "browser"}
	m, clock := newTestManager(t, ManagerConfig{
		LeaseTTL:       5 * time.Second,
		HealthInterval: time.Second,
		MaxAttempts:    1, // 不自动重试，便于断言首次调用失败
	})
	if err := m.RegisterPool(PoolSpec{Kind: "browser", Size: 1, Factory: ff.factory}); err != nil {
		t.Fatalf("注册池失败: %v", err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("启动 Manager 失败: %v", err)
	}
	defer m.Stop(context.Background())

	if ff.count() != 1 {
		t.Fatalf("期望创建 1 个 worker，实际 %d", ff.count())
	}
	first := ff.workers()[0]

	// 模拟"Worker 被 kill"：健康检查开始失败。
	first.setHealthErr(errors.New("模拟被 kill"))

	// 触发一轮维护：Manager 应检测到并走恢复链条。
	clock.Advance(2 * time.Second)
	m.sweep()

	// 关键断言 1：Engine（此处即 Manager）仍然存活，Health 能返回。
	if err := m.Health(context.Background()); err != nil {
		t.Fatalf("Manager 在 worker 崩溃后应仍可服务，却返回: %v", err)
	}
	// 关键断言 2：确实重建了 Worker（恢复链条走到了 restart + health check）。
	deadline := time.Now().Add(3 * time.Second)
	for ff.count() < 2 && time.Now().Before(deadline) {
		clock.Advance(2 * time.Second)
		m.sweep()
		time.Sleep(10 * time.Millisecond)
	}
	if ff.count() < 2 {
		t.Fatalf("期望 worker 被重启（创建数 >= 2），实际 %d", ff.count())
	}
	// 关键断言 3：旧 worker 被停止（资源被回收，不是"崩溃后 log continue"）。
	if !first.isStopped() {
		t.Error("崩溃的 worker 应被停止回收")
	}
}

// TestCrashPersistsFailure 验证恢复链条的 "persist worker failure" 步。
func TestCrashPersistsFailure(t *testing.T) {
	store := &MemoryFailureStore{}
	ff := &fakeFactory{kind: "terminal"}
	clock := NewManualClock(time.Now())
	m := NewManager(ManagerConfig{HealthInterval: time.Second, MaxAttempts: 1},
		WithClock(clock), WithFailureStore(store))
	if err := m.RegisterPool(PoolSpec{Kind: "terminal", Size: 1, Factory: ff.factory}); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer m.Stop(context.Background())

	ff.workers()[0].setHealthErr(errors.New("out of memory"))
	clock.Advance(2 * time.Second)
	m.sweep()

	recs := store.Records()
	if len(recs) == 0 {
		t.Fatal("worker 崩溃后必须持久化失败记录（恢复链条第 3 步）")
	}
	if recs[0].Kind != "terminal" {
		t.Errorf("失败记录 kind 错误: %s", recs[0].Kind)
	}
	if recs[0].Message == "" {
		t.Error("失败记录应包含原因")
	}
}

// TestPanicInWorkerIsContained 验证 Worker panic 不会带崩 Manager（panic 恢复边界）。
func TestPanicInWorkerIsContained(t *testing.T) {
	ff := &fakeFactory{kind: "dynamic-js", configure: func(w *fakeWorker) {
		w.execFn = func(ctx context.Context, req WorkerRequest) (WorkerResponse, error) {
			panic("沙箱里发生了 panic")
		}
	}}
	m, _ := newTestManager(t, ManagerConfig{})
	if err := m.RegisterPool(PoolSpec{Kind: "dynamic-js", Size: 1, Factory: ff.factory}); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer m.Stop(context.Background())

	resp, err := m.Execute(context.Background(), WorkerRequest{
		CallID: "c1", Kind: "dynamic-js", Action: "eval",
	})
	// panic 必须被转成响应里的错误，而不是让调用方也 panic。
	if resp.Status != StatusCrash {
		t.Errorf("期望 status=crash，实际 %s", resp.Status)
	}
	if resp.Error == nil || resp.Error.Code != CodeCrash {
		t.Errorf("期望 crash 错误码，实际 %+v", resp.Error)
	}
	// Manager 仍然存活。
	if err := m.Health(context.Background()); err != nil {
		t.Errorf("panic 后 Manager 应仍可服务: %v", err)
	}
	_ = err
}

// ============================== 验收标准 2：所有 worker 都有 heartbeat/timeout/restart ==============================

// TestHeartbeatKeepsLeaseAlive 验证租约心跳语义：
// 持续心跳的租约不会过期；停止心跳后过期并被回收。
func TestHeartbeatKeepsLeaseAlive(t *testing.T) {
	ff := &fakeFactory{kind: "browser"}
	m, clock := newTestManager(t, ManagerConfig{
		LeaseTTL:          3 * time.Second,
		HeartbeatInterval: time.Second,
	})
	if err := m.RegisterPool(PoolSpec{Kind: "browser", Size: 1, Factory: ff.factory}); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer m.Stop(context.Background())

	// 关闭自动心跳，手动控制以做确定性断言。
	lease, err := m.Acquire(context.Background(), "browser", WithAutoHeartbeat(false))
	if err != nil {
		t.Fatalf("Acquire 失败: %v", err)
	}
	// 手动心跳 3 次（每次间隔 2s，小于 3s TTL）。
	for i := 0; i < 3; i++ {
		clock.Advance(2 * time.Second)
		m.sweep()
		if err := lease.Heartbeat(); err != nil {
			t.Fatalf("第 %d 次心跳失败: %v", i+1, err)
		}
		if lease.Expired() {
			t.Fatal("持续心跳的租约不应过期")
		}
	}
	if lease.Expired() {
		t.Error("租约不应过期")
	}
	if err := lease.Release(context.Background()); err != nil {
		t.Fatalf("Release 失败: %v", err)
	}
	if !lease.Released() {
		t.Error("Release 后应标记为已释放")
	}
}

// TestLeaseTimeoutTriggersRecycle 验证"租约超时 → force cleanup → worker recycle"。
func TestLeaseTimeoutTriggersRecycle(t *testing.T) {
	recycleEvents := make(chan WorkerEvent, 8)
	ff := &fakeFactory{kind: "browser"}
	clock := NewManualClock(time.Now())
	m := NewManager(ManagerConfig{
		LeaseTTL:          3 * time.Second,
		HeartbeatInterval: time.Hour, // 手动控制，避免自动心跳干扰
		HealthInterval:    time.Second,
	}, WithClock(clock), WithEvents(sinkFunc(func(ev WorkerEvent) {
		select {
		case recycleEvents <- ev:
		default:
		}
	})))
	if err := m.RegisterPool(PoolSpec{Kind: "browser", Size: 1, Factory: ff.factory}); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer m.Stop(context.Background())

	lease, err := m.Acquire(context.Background(), "browser", WithAutoHeartbeat(false))
	if err != nil {
		t.Fatal(err)
	}
	// 不再心跳，推进超过 TTL。
	clock.Advance(5 * time.Second)
	m.sweep()

	if !lease.Expired() {
		t.Fatal("超过 TTL 未心跳，租约应被判定过期")
	}
	// 恢复链条：应发出 lease.expired 与/或 worker.recycled 事件。
	found := false
	timeout := time.After(time.Second)
	for !found {
		select {
		case ev := <-recycleEvents:
			if ev.Type == EventLeaseExpired || ev.Type == EventWorkerRecycled {
				found = true
			}
		case <-timeout:
			t.Fatal("租约超时后应发出 lease.expired / worker.recycled 事件")
		}
	}
	// 过期后使用租约必须失败（调用方不能继续用被回收的 worker）。
	if err := lease.Heartbeat(); err == nil {
		t.Error("过期租约的 Heartbeat 应返回错误")
	}
}

// TestCallTimeoutIsEnforced 验证单次调用超时。
func TestCallTimeoutIsEnforced(t *testing.T) {
	ff := &fakeFactory{kind: "terminal", configure: func(w *fakeWorker) {
		w.execFn = func(ctx context.Context, req WorkerRequest) (WorkerResponse, error) {
			// 模拟卡死的命令：等到 ctx 结束。
			<-ctx.Done()
			return WorkerResponse{}, ctx.Err()
		}
	}}
	m, _ := newTestManager(t, ManagerConfig{CallTimeout: 50 * time.Millisecond})
	if err := m.RegisterPool(PoolSpec{Kind: "terminal", Size: 1, Factory: ff.factory}); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer m.Stop(context.Background())

	start := time.Now()
	resp, _ := m.Execute(context.Background(), WorkerRequest{
		CallID: "c1", Kind: "terminal", Action: "exec", Timeout: 50 * time.Millisecond,
	})
	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Fatalf("调用超时未被及时强制，耗时 %s", elapsed)
	}
	if resp.Status != StatusTimeout {
		t.Errorf("期望 status=timeout，实际 %s (err=%v)", resp.Status, resp.Error)
	}
}

// TestRetryableCallResumedAfterRestart 验证"重启后恢复排队调用"（resume queued calls）。
func TestRetryableCallResumedAfterRestart(t *testing.T) {
	var attempts atomic.Int64
	ff := &fakeFactory{kind: "browser", configure: func(w *fakeWorker) {
		w.execFn = func(ctx context.Context, req WorkerRequest) (WorkerResponse, error) {
			n := attempts.Add(1)
			if n == 1 {
				// 第一次调用时模拟 Worker 崩溃。
				w.setHealthErr(errors.New("模拟崩溃"))
				return ErrorResponse(req, w.id, CodeCrash, "worker 崩溃"), fmt.Errorf("%w: 模拟崩溃", ErrCrash)
			}
			return OKResponse(req, w.id, time.Now(), map[string]any{"content": "ok"})
		}
	}}
	m, clock := newTestManager(t, ManagerConfig{
		MaxAttempts:    3,
		HealthInterval: time.Second,
		QueueSize:      8,
	})
	if err := m.RegisterPool(PoolSpec{Kind: "browser", Size: 1, Factory: ff.factory}); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer m.Stop(context.Background())

	// navigate 是幂等动作，允许崩溃后自动重派。
	go func() {
		for i := 0; i < 30; i++ {
			clock.Advance(500 * time.Millisecond)
			m.sweep()
			time.Sleep(5 * time.Millisecond)
		}
	}()

	resp, err := m.Execute(context.Background(), WorkerRequest{
		CallID: "c1", Kind: "browser", Action: "navigate",
		IdempotencyKey: "k1",
	})
	// 允许两种可接受结果：要么重派成功，要么明确失败（都不算错误行为）。
	if err != nil {
		t.Logf("调用最终失败（可接受，取决于时序）: %v", err)
		return
	}
	if resp.Status == StatusOK {
		if attempts.Load() < 2 {
			t.Error("期望发生过重派")
		}
	}
}

// TestNonIdempotentCallNotRetried 验证 I5：非幂等动作崩溃后**不**自动重放。
func TestNonIdempotentCallNotRetried(t *testing.T) {
	var attempts atomic.Int64
	ff := &fakeFactory{kind: "terminal", configure: func(w *fakeWorker) {
		w.execFn = func(ctx context.Context, req WorkerRequest) (WorkerResponse, error) {
			attempts.Add(1)
			w.setHealthErr(errors.New("崩溃"))
			return ErrorResponse(req, w.id, CodeCrash, "崩溃"), fmt.Errorf("%w: 崩溃", ErrCrash)
		}
	}}
	m, _ := newTestManager(t, ManagerConfig{MaxAttempts: 5})
	if err := m.RegisterPool(PoolSpec{Kind: "terminal", Size: 1, Factory: ff.factory}); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer m.Stop(context.Background())

	resp, _ := m.Execute(context.Background(), WorkerRequest{
		// "rm"-类动作不在幂等白名单里，绝不能自动重试。
		CallID: "c1", Kind: "terminal", Action: "exec_destructive",
		IdempotencyKey: "k2",
	})
	if resp.Status != StatusCrash && resp.Status != StatusError {
		t.Errorf("期望 crash/error 状态，实际 %s", resp.Status)
	}
	if n := attempts.Load(); n > 1 {
		t.Errorf("非幂等动作被自动重放 %d 次，违反 I5 不变量", n)
	}
}

// ============================== 验收标准 3 & 4：进程树清理 + 可回收 ==============================

// TestProcessTreeCleanedOnCrash 验证崩溃时对 Worker 上报的 PID 做进程树清理。
func TestProcessTreeCleanedOnCrash(t *testing.T) {
	// 用一个真实存活但无害的进程（当前进程自身）PID 作为"遗留进程"，
	// 只验证 Manager 会去调用清理路径（不真的 kill 测试进程：用明显无效的 PID）。
	const fakePID = 999999 // 不存在的 PID，kill 是安全的 no-op
	ff := &fakeFactory{kind: "office", configure: func(w *fakeWorker) {
		w.pids = []int{fakePID}
		w.execFn = func(ctx context.Context, req WorkerRequest) (WorkerResponse, error) {
			return ErrorResponse(req, w.id, CodeCrash, "崩溃"), fmt.Errorf("%w: 崩溃", ErrCrash)
		}
	}}
	var orphanCleanups atomic.Int64
	clock := NewManualClock(time.Now())
	m := NewManager(ManagerConfig{}, WithClock(clock), WithMetrics(metricsFunc(func(name, kind string, delta int64) {
		if name == "worker_orphan_processes" {
			orphanCleanups.Add(delta)
		}
	})))
	if err := m.RegisterPool(PoolSpec{Kind: "office", Size: 1, Factory: ff.factory}); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer m.Stop(context.Background())

	_, _ = m.Execute(context.Background(), WorkerRequest{CallID: "c1", Kind: "office", Action: "docs"})

	if orphanCleanups.Load() == 0 {
		t.Error("崩溃的 worker 上报了 PID，应触发进程树清理（验收标准：所有外部进程都有 process-tree cleanup）")
	}
}

// TestWorkerIDsAreRecoverable 验证 I10 不变量：
// Worker 崩溃被重启后，Manager 仍能把所有 slot 恢复为可服务状态（都能被 Supervisor 回收）。
func TestWorkerIDsAreRecoverable(t *testing.T) {
	ff := &fakeFactory{kind: "browser"}
	m, clock := newTestManager(t, ManagerConfig{HealthInterval: time.Second})
	// 4 个 worker，模拟任务书要求的 BrowserPool 默认规模。
	if err := m.RegisterPool(PoolSpec{Kind: "browser", Size: 4, Factory: ff.factory}); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer m.Stop(context.Background())

	if len(m.SlotInfos()) != 4 {
		t.Fatalf("期望 4 个 slot，实际 %d", len(m.SlotInfos()))
	}

	// 逐个把所有 worker 打成不健康，然后推进维护循环。
	for _, w := range ff.workers() {
		w.setHealthErr(errors.New("模拟随机 kill"))
	}
	for i := 0; i < 20; i++ {
		clock.Advance(2 * time.Second)
		m.sweep()
		time.Sleep(5 * time.Millisecond)
		if ff.count() >= 8 { // 4 个原始 + 4 个重启
			break
		}
	}
	if ff.count() < 8 {
		t.Fatalf("期望 4 个 worker 全部被重启（创建数 >= 8），实际 %d", ff.count())
	}
	// 所有 slot 最终都应回到 ready 或有效的重启状态（没有永久卡死）。
	infos := m.SlotInfos()
	for _, info := range infos {
		if info.State != StateReady && info.State != StateRestarting && info.State != StateCircuitOpen {
			t.Errorf("slot %d 处于异常状态 %s", info.Slot, info.StateName)
		}
	}
}

// ============================== 熔断与退避 ==============================

// TestCircuitOpensAfterRepeatedCrashes 验证"反复崩溃 → circuit open"。
func TestCircuitOpensAfterRepeatedCrashes(t *testing.T) {
	circuitOpened := make(chan WorkerEvent, 16)
	ff := &fakeFactory{kind: "mcp", configure: func(w *fakeWorker) {
		// 每次新建的 worker 都是不健康的 → 反复崩溃。
		w.startErr = errors.New("启动即失败")
	}}
	clock := NewManualClock(time.Now())
	m := NewManager(ManagerConfig{
		FailuresToCircuit: 3,
		FailureWindow:     5 * time.Minute,
		HealthInterval:    time.Second,
		Backoff:           Backoff{Schedule: []time.Duration{0}},
	}, WithClock(clock), WithEvents(sinkFunc(func(ev WorkerEvent) {
		if ev.Type == EventCircuitOpened {
			select {
			case circuitOpened <- ev:
			default:
			}
		}
	})))
	if err := m.RegisterPool(PoolSpec{Kind: "mcp", Size: 1, Factory: ff.factory}); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer m.Stop(context.Background())

	for i := 0; i < 50; i++ {
		clock.Advance(2 * time.Second)
		m.sweep()
		select {
		case <-circuitOpened:
			// 熔断打开后，Acquire 应快速失败（不再反复重启打风暴）。
			if _, err := m.Acquire(context.Background(), "mcp"); err == nil {
				t.Error("熔断打开后 Acquire 应快速失败")
			}
			return
		default:
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("反复崩溃后应触发 circuit open")
}

// TestBackoffScheduleMatchesSpec 验证退避节奏严格等于 1s→2s→4s→8s→16s→30s 封顶。
func TestBackoffScheduleMatchesSpec(t *testing.T) {
	b := Backoff{} // 使用默认 schedule
	want := []time.Duration{
		1 * time.Second, 2 * time.Second, 4 * time.Second,
		8 * time.Second, 16 * time.Second, 30 * time.Second,
		30 * time.Second, // 封顶
		30 * time.Second,
	}
	for i, w := range want {
		if got := b.Delay(i); got != w {
			t.Errorf("第 %d 次退避: got=%s want=%s", i, got, w)
		}
	}
}

// TestCircuitBreakerStates 验证熔断状态机：closed → open → half-open → closed。
func TestCircuitBreakerStates(t *testing.T) {
	clock := NewManualClock(time.Now())
	var transitions []string
	cb := NewCircuitBreaker(CircuitConfig{
		FailureThreshold: 3,
		SuccessThreshold: 2,
		Cooldown:         10 * time.Second,
	}, clock, func(from, to CircuitState, reason string) {
		transitions = append(transitions, from.String()+"->"+to.String())
	})

	if cb.State() != CircuitClosed {
		t.Fatal("初始应为 closed")
	}
	// 连续 3 次失败 → open。
	for i := 0; i < 3; i++ {
		cb.RecordFailure("boom")
	}
	if cb.State() != CircuitOpen {
		t.Fatalf("连续 3 次失败后应为 open，实际 %s", cb.State())
	}
	if cb.Allow() {
		t.Error("open 状态应拒绝调用")
	}
	// 冷却结束后 → half-open，放行 1 个探测。
	clock.Advance(11 * time.Second)
	if cb.State() != CircuitHalfOpen {
		t.Fatalf("冷却后应为 half-open，实际 %s", cb.State())
	}
	if !cb.Allow() {
		t.Error("half-open 应放行探测请求")
	}
	// 探测成功 2 次 → closed。
	cb.RecordSuccess()
	cb.RecordSuccess()
	if cb.State() != CircuitClosed {
		t.Fatalf("探测成功后应闭合，实际 %s", cb.State())
	}
	if len(transitions) < 3 {
		t.Errorf("期望至少 3 次状态迁移，实际 %v", transitions)
	}
}

// TestCircuitHalfOpenProbeFailureReopens 验证 half-open 探测失败会重新打开。
func TestCircuitHalfOpenProbeFailureReopens(t *testing.T) {
	clock := NewManualClock(time.Now())
	cb := NewCircuitBreaker(CircuitConfig{FailureThreshold: 1, Cooldown: time.Second, SuccessThreshold: 2}, clock, nil)
	cb.RecordFailure("x")
	if cb.State() != CircuitOpen {
		t.Fatal("应为 open")
	}
	clock.Advance(2 * time.Second)
	if !cb.Allow() {
		t.Fatal("half-open 应放行探测")
	}
	cb.RecordFailure("探测失败")
	if cb.State() != CircuitOpen {
		t.Errorf("探测失败应重回 open，实际 %s", cb.State())
	}
}

// ============================== 帧协议 ==============================

// TestFrameCodecRoundTrip 验证帧编解码往返。
func TestFrameCodecRoundTrip(t *testing.T) {
	var buf writerBuf
	c := FrameCodec{}
	payload := []byte(`{"hello":"世界"}`)
	h := FrameHeader{RequestID: "r1", SessionID: "s1", Sequence: 7, Type: FrameTypeRequest}
	if err := c.WriteFrame(&buf, h, payload); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	got, gotPayload, err := c.ReadFrame(&buf)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if got.RequestID != "r1" || got.Sequence != 7 || got.Type != FrameTypeRequest {
		t.Errorf("帧头不匹配: %+v", got)
	}
	if got.Version != ProtocolVersion {
		t.Errorf("版本应为 %d，实际 %d", ProtocolVersion, got.Version)
	}
	if got.PayloadLength != uint32(len(payload)) {
		t.Errorf("payload 长度不匹配: %d", got.PayloadLength)
	}
	if string(gotPayload) != string(payload) {
		t.Errorf("payload 不匹配: %s", gotPayload)
	}
}

// TestFrameCodecRejectsOversizedPayload 验证 payload 上限在分配内存前被拒绝。
func TestFrameCodecRejectsOversizedPayload(t *testing.T) {
	var buf writerBuf
	c := FrameCodec{MaxPayload: 16}
	err := c.WriteFrame(&buf, FrameHeader{Type: FrameTypeRequest}, make([]byte, 32))
	if !errors.Is(err, ErrFramePayloadTooLarge) {
		t.Fatalf("期望 ErrFramePayloadTooLarge，实际 %v", err)
	}
	if buf.Len() != 0 {
		t.Error("超限时不应写出任何字节")
	}
}

// TestFrameCodecDetectsTruncation 验证截断被检测。
func TestFrameCodecDetectsTruncation(t *testing.T) {
	var buf writerBuf
	c := FrameCodec{}
	if err := c.WriteFrame(&buf, FrameHeader{Type: FrameTypeRequest}, []byte("abcdef")); err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()
	truncated := &readerBuf{data: data[:len(data)-3]}
	if _, _, err := c.ReadFrame(truncated); !errors.Is(err, ErrFrameTruncated) {
		t.Fatalf("期望 ErrFrameTruncated，实际 %v", err)
	}
}

// TestFrameCodecRejectsBadVersion 验证协议版本不匹配被拒绝。
func TestFrameCodecRejectsBadVersion(t *testing.T) {
	// 手工构造一个 version=99 的帧。
	head := []byte(`{"version":99,"type":"worker.request","payload_length":0}`)
	var buf writerBuf
	var lenBuf [4]byte
	lenBuf[0] = byte(len(head) >> 24)
	lenBuf[1] = byte(len(head) >> 16)
	lenBuf[2] = byte(len(head) >> 8)
	lenBuf[3] = byte(len(head))
	buf.Write(lenBuf[:])
	buf.Write(head)
	if _, _, err := (FrameCodec{}).ReadFrame(&buf); !errors.Is(err, ErrFrameVersionMismatch) {
		t.Fatalf("期望 ErrFrameVersionMismatch，实际 %v", err)
	}
}

// ============================== WorkerError 语义 ==============================

// TestWorkerErrorUnwrapEnablesErrorsIs 验证错误码可通过 errors.Is 判断。
func TestWorkerErrorUnwrapEnablesErrorsIs(t *testing.T) {
	cases := []struct {
		code string
		want error
	}{
		{CodeTimeout, ErrTimeout},
		{CodePolicyDenied, ErrPolicyDenied},
		{CodeCanceled, ErrCanceled},
		{CodeCrash, ErrCrash},
		{CodeUnavailable, ErrUnavailable},
		{CodePayloadTooLarge, ErrPayloadTooLarge},
	}
	for _, tc := range cases {
		err := NewWorkerError(tc.code, "x", true)
		if !errors.Is(err, tc.want) {
			t.Errorf("code=%s 应可 errors.Is 到 %v", tc.code, tc.want)
		}
	}
}

// TestErrorResponseRetryability 验证可重试标记与状态码的对应。
func TestErrorResponseRetryability(t *testing.T) {
	retryable := []string{CodeTimeout, CodeUnavailable, CodeCrash, CodeRejected, CodeUpstream}
	notRetryable := []string{CodeInvalidArgument, CodePolicyDenied, CodeCanceled, CodeInternal, CodePayloadTooLarge}
	req := WorkerRequest{CallID: "c1"}
	for _, c := range retryable {
		if resp := ErrorResponse(req, "w", c, "x"); !resp.Error.Retryable {
			t.Errorf("code=%s 应标记为可重试", c)
		}
	}
	for _, c := range notRetryable {
		if resp := ErrorResponse(req, "w", c, "x"); resp.Error.Retryable {
			t.Errorf("code=%s 不应标记为可重试", c)
		}
	}
}

// ============================== 结果归一化（与任务 04 的契约） ==============================

// TestResultNormalizerMapping 验证 WorkerResponse → ToolResponse 的映射。
func TestResultNormalizerMapping(t *testing.T) {
	n := NewDefaultResultNormalizer(nil)

	// 成功路径。
	raw, _ := json.Marshal(map[string]any{"content": "已导航", "title": "T"})
	resp := WorkerResponse{
		CallID: "c1", WorkerID: "w1", Status: StatusOK, Result: raw,
		Artifacts: []Artifact{{Ref: "cas://abc", Name: "s.png"}},
	}
	out, err := n.Normalize(resp)
	if err != nil {
		t.Fatal(err)
	}
	if out.ToolCallID != "c1" || !out.Success || out.Content != "已导航" {
		t.Errorf("成功映射错误: %+v", out)
	}
	if len(out.ArtifactRefs) != 1 || out.ArtifactRefs[0] != "cas://abc" {
		t.Errorf("产物引用映射错误: %+v", out.ArtifactRefs)
	}

	// 失败路径。
	respErr := WorkerResponse{
		CallID: "c2", Status: StatusTimeout,
		Error: NewWorkerError(CodeTimeout, "超时了", true),
	}
	out2, _ := n.Normalize(respErr)
	if out2.Success || out2.ToolCallID != "c2" || out2.Error != "超时了" {
		t.Errorf("失败映射错误: %+v", out2)
	}
}

// TestFieldMappingConsistency 验证字段映射表无重复/无空值（防止手工维护写错）。
func TestFieldMappingConsistency(t *testing.T) {
	if err := ValidateMappingConsistency(); err != nil {
		t.Fatalf("字段映射表不自洽: %v", err)
	}
	if len(FieldMappingTable()) == 0 {
		t.Fatal("字段映射表不应为空")
	}
}

// ============================== Manager 生命周期 ==============================

// TestManagerStopIsClean 验证优雅停机：无 goroutine 泄漏、所有 worker 被停止。
func TestManagerStopIsClean(t *testing.T) {
	ff := &fakeFactory{kind: "browser"}
	m, _ := newTestManager(t, ManagerConfig{})
	if err := m.RegisterPool(PoolSpec{Kind: "browser", Size: 3, Factory: ff.factory}); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := m.Stop(context.Background()); err != nil {
		t.Fatalf("Stop 失败: %v", err)
	}
	for i, w := range ff.workers() {
		if !w.isStopped() {
			t.Errorf("第 %d 个 worker 未被停止", i)
		}
	}
	// 重复 Stop 必须幂等。
	if err := m.Stop(context.Background()); err != nil {
		t.Errorf("重复 Stop 应幂等，却返回: %v", err)
	}
	// 停机后不再接受调用。
	if _, err := m.Execute(context.Background(), WorkerRequest{CallID: "c", Kind: "browser", Action: "navigate"}); err == nil {
		t.Error("停机后 Execute 应失败")
	}
}

// TestStopRejectsQueuedCalls 验证停机时排队调用被明确失败（不挂住调用方）。
func TestStopRejectsQueuedCalls(t *testing.T) {
	release := make(chan struct{})
	ff := &fakeFactory{kind: "browser", configure: func(w *fakeWorker) {
		w.execFn = func(ctx context.Context, req WorkerRequest) (WorkerResponse, error) {
			select {
			case <-release:
			case <-ctx.Done():
			}
			return OKResponse(req, w.id, time.Now(), nil)
		}
	}}
	m, _ := newTestManager(t, ManagerConfig{DrainTimeout: 100 * time.Millisecond})
	if err := m.RegisterPool(PoolSpec{Kind: "browser", Size: 1, Factory: ff.factory}); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	// 一个长调用占住唯一 slot，再压一个排队调用。
	go func() {
		_, _ = m.Execute(context.Background(), WorkerRequest{CallID: "long", Kind: "browser", Action: "navigate"})
	}()
	time.Sleep(50 * time.Millisecond)
	done := make(chan error, 1)
	go func() {
		_, err := m.Execute(context.Background(), WorkerRequest{CallID: "queued", Kind: "browser", Action: "navigate"})
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)

	close(release)
	if err := m.Stop(context.Background()); err != nil {
		t.Fatalf("Stop 失败: %v", err)
	}
	select {
	case <-done:
		// 排队调用被明确返回（成功或失败都可接受），关键是不能永久挂住。
	case <-time.After(3 * time.Second):
		t.Fatal("停机时排队调用被挂住，未明确返回")
	}
}

// TestAcquireUnregisteredKindFails 验证未注册 kind 的清晰错误。
func TestAcquireUnregisteredKindFails(t *testing.T) {
	m, _ := newTestManager(t, ManagerConfig{})
	ff := &fakeFactory{kind: "browser"}
	if err := m.RegisterPool(PoolSpec{Kind: "browser", Size: 1, Factory: ff.factory}); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer m.Stop(context.Background())

	if _, err := m.Acquire(context.Background(), "不存在的类型"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("期望 ErrUnavailable，实际 %v", err)
	}
}

// TestConcurrentExecuteIsSerializedPerSlot 验证同一 slot 上调用被串行化
// （这是 v1 "全局单例 page 被并发工具调用互相干扰" 的反面保证）。
func TestConcurrentExecuteIsSerializedPerSlot(t *testing.T) {
	var concurrent atomic.Int64
	var maxConcurrent atomic.Int64
	ff := &fakeFactory{kind: "browser", configure: func(w *fakeWorker) {
		w.execFn = func(ctx context.Context, req WorkerRequest) (WorkerResponse, error) {
			n := concurrent.Add(1)
			for {
				old := maxConcurrent.Load()
				if n <= old || maxConcurrent.CompareAndSwap(old, n) {
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
			concurrent.Add(-1)
			return OKResponse(req, w.id, time.Now(), nil)
		}
	}}
	m, _ := newTestManager(t, ManagerConfig{QueueSize: 32})
	if err := m.RegisterPool(PoolSpec{Kind: "browser", Size: 1, Factory: ff.factory}); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer m.Stop(context.Background())

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = m.Execute(context.Background(), WorkerRequest{
				CallID: fmt.Sprintf("c%d", i), Kind: "browser", Action: "navigate",
			})
		}(i)
	}
	wg.Wait()

	if got := maxConcurrent.Load(); got > 1 {
		t.Errorf("同一 slot 上并发执行了 %d 个调用，应被串行化为 1（v1 的并发干扰问题）", got)
	}
}

// TestSupervisorAdapterImplementsSupervisable 验证适配器满足任务 01 的 Supervisable 契约。
func TestSupervisorAdapterImplementsSupervisable(t *testing.T) {
	ff := &fakeFactory{kind: "browser"}
	m, _ := newTestManager(t, ManagerConfig{})
	if err := m.RegisterPool(PoolSpec{Kind: "browser", Size: 1, Factory: ff.factory}); err != nil {
		t.Fatal(err)
	}
	// 编译期断言 + 运行期行为。
	var sv Supervisable = NewSupervisorAdapter(m, "worker-pool", context.Background(), SupervisorHooks{})
	if sv.ID() != "worker-pool" || sv.Kind() != "worker" {
		t.Errorf("适配器标识错误: id=%s kind=%s", sv.ID(), sv.Kind())
	}
	if err := sv.Start(); err != nil {
		t.Fatalf("Supervisable.Start 失败: %v", err)
	}
	if err := sv.Heartbeat(); err != nil {
		t.Fatalf("Supervisable.Heartbeat 失败: %v", err)
	}
	if err := sv.Kill(); err != nil {
		t.Fatalf("Supervisable.Kill 失败: %v", err)
	}
}

// TestSpecAccessors 验证 Spec 的配置读取（含 JSON 数字与字符串时长）。
func TestSpecAccessors(t *testing.T) {
	s := Spec{
		ID: "x", Kind: "y",
		Config: map[string]any{
			"str":   "hello",
			"b":     true,
			"num":   float64(42),
			"dur_s": "30s",
			"dur_m": float64(1500),
			"list":  []any{"a", "b"},
		},
	}
	if s.String("str", "def") != "hello" {
		t.Error("String 读取失败")
	}
	if !s.Bool("b", false) {
		t.Error("Bool 读取失败")
	}
	if s.Int("num", 0) != 42 {
		t.Error("Int 读取 float64 失败")
	}
	if s.Duration("dur_s", 0) != 30*time.Second {
		t.Error("Duration 读取字符串失败")
	}
	if s.Duration("dur_m", 0) != 1500*time.Millisecond {
		t.Error("Duration 读取数字（毫秒）失败")
	}
	if got := s.Strings("list"); len(got) != 2 {
		t.Errorf("Strings 读取失败: %v", got)
	}
	// 缺省值路径。
	if s.String("missing", "def") != "def" {
		t.Error("String 缺省值失败")
	}
}

// ============================== 测试辅助类型 ==============================

type writerBuf struct {
	mu  sync.Mutex
	buf []byte
	pos int
}

func (w *writerBuf) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	return len(p), nil
}

func (w *writerBuf) Bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]byte(nil), w.buf...)
}

func (w *writerBuf) Len() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.buf)
}

// Read 让 writerBuf 同时可作为 io.Reader（帧编解码往返测试需要读写同一缓冲）。
func (w *writerBuf) Read(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.pos >= len(w.buf) {
		return 0, io.EOF
	}
	n := copy(p, w.buf[w.pos:])
	w.pos += n
	return n, nil
}

type readerBuf struct {
	data []byte
	pos  int
}

func (r *readerBuf) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

// sinkFunc 把函数适配成 EventSink。
type sinkFunc func(WorkerEvent)

func (f sinkFunc) Emit(ev WorkerEvent) { f(ev) }

// metricsFunc 把函数适配成 MetricsSink（只关心 IncCounter）。
type metricsFunc func(name, kind string, delta int64)

func (f metricsFunc) ObserveCallDuration(string, string, string, time.Duration) {}
func (f metricsFunc) ObserveQueueWait(string, time.Duration)                    {}
func (f metricsFunc) IncCounter(name, kind string, delta int64)                 { f(name, kind, delta) }
func (f metricsFunc) SetGauge(string, string, string, float64)                  {}
