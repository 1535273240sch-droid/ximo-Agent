package tool

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// WorkerGateway 崩溃恢复链：reject in-flight → persist failure → restart →
// health check → resume queued calls（第13章）
// ---------------------------------------------------------------------------

type gatewayTestRouter struct {
	mu       sync.Mutex
	calls    int
	crashN   int // 前 N 次调用崩溃
	failWith error
	succeed  chan struct{} // 每次成功调用时发送信号
}

func (r *gatewayTestRouter) Route(ctx context.Context, req WorkerRequest) (WorkerResponse, error) {
	r.mu.Lock()
	r.calls++
	n := r.calls
	crash := n <= r.crashN
	fail := r.failWith
	success := r.succeed
	r.mu.Unlock()
	if crash {
		return WorkerResponse{}, fail
	}
	if success != nil {
		select {
		case success <- struct{}{}:
		default:
		}
	}
	return WorkerResponse{ToolCallID: req.ToolCallID, ToolName: req.ToolName, Content: "done", Success: true}, nil
}

type gatewayTestController struct {
	mu           sync.Mutex
	restarts     int
	healthFails  int
	healthChecks int
	restartFailN int
	restartedCh  chan struct{}
}

func (c *gatewayTestController) Restart(ctx context.Context, domain ExecutionDomain) error {
	c.mu.Lock()
	c.restarts++
	fail := c.restarts <= c.restartFailN
	ch := c.restartedCh
	c.mu.Unlock()
	if fail {
		return errors.New("重启失败")
	}
	if ch != nil {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	return nil
}

func (c *gatewayTestController) Health(ctx context.Context, domain ExecutionDomain) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.healthChecks++
	if c.healthChecks <= c.healthFails {
		return errors.New("健康检查失败")
	}
	return nil
}

func (c *gatewayTestController) snapshot() (restarts, healthChecks int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.restarts, c.healthChecks
}

type gatewayTestFailures struct {
	mu        sync.Mutex
	failures  []WorkerFailure
	recovered []ExecutionDomain
}

func (s *gatewayTestFailures) RecordFailure(ctx context.Context, f WorkerFailure) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures = append(s.failures, f)
	return nil
}

func (s *gatewayTestFailures) MarkRecovered(ctx context.Context, domain ExecutionDomain, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recovered = append(s.recovered, domain)
	return nil
}

func (s *gatewayTestFailures) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.failures)
}

func newTestGateway(router WorkerRouter, controller WorkerController, failures WorkerFailureStore) *WorkerGateway {
	cfg := DefaultGatewayConfig()
	cfg.RestartBackoff = 5 * time.Millisecond
	cfg.HealthCheckInterval = 2 * time.Millisecond
	cfg.MaxRestartAttempts = 8
	return NewWorkerGateway(router, controller, failures, cfg)
}

func TestWorkerGatewayCrashRecoveryChain(t *testing.T) {
	router := &gatewayTestRouter{crashN: 1, failWith: &ErrWorkerUnavailable{Domain: DomainWorkerTerminal, Reason: "worker 进程退出"}}
	controller := &gatewayTestController{restartedCh: make(chan struct{}, 4)}
	failures := &gatewayTestFailures{}
	gateway := newTestGateway(router, controller, failures)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := gateway.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer gateway.Stop()

	req := WorkerRequest{RunID: "r", ToolCallID: "c1", ToolName: "terminal_exec", Domain: DomainWorkerTerminal}
	// 第一次调用：worker 崩溃
	if _, err := gateway.Route(ctx, req); err == nil {
		t.Fatalf("崩溃调用应当返回错误")
	}
	// 失败记录已持久化
	if failures.count() != 1 {
		t.Fatalf("失败记录 = %d，期望 1", failures.count())
	}
	// 重启已被触发（等 supervisor 处理）
	waitFor(t, 2*time.Second, func() bool {
		restarts, _ := controller.snapshot()
		return restarts >= 1
	})
	// 健康检查通过后恢复
	waitFor(t, 2*time.Second, func() bool {
		stats := gateway.Stats()
		st, ok := stats.Domains[DomainWorkerTerminal]
		return ok && st.Healthy && !st.Restarting
	})
	// 恢复后的调用成功
	resp, err := gateway.Route(ctx, req)
	if err != nil {
		t.Fatalf("恢复后调用失败: %v", err)
	}
	if resp.Content != "done" {
		t.Fatalf("恢复后响应 = %+v", resp)
	}
	if _, healthChecks := controller.snapshot(); healthChecks < 1 {
		t.Fatalf("恢复前应当有健康检查")
	}
}

func TestWorkerGatewayQueuesDuringRestart(t *testing.T) {
	router := &gatewayTestRouter{crashN: 1, failWith: &ErrWorkerUnavailable{Domain: DomainWorkerTerminal, Reason: "crash"}}
	controller := &gatewayTestController{healthFails: 2, restartedCh: make(chan struct{}, 4)}
	failures := &gatewayTestFailures{}
	gateway := newTestGateway(router, controller, failures)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := gateway.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer gateway.Stop()

	// 触发崩溃
	if _, err := gateway.Route(ctx, WorkerRequest{RunID: "r", ToolCallID: "c1", ToolName: "t", Domain: DomainWorkerTerminal}); err == nil {
		t.Fatalf("应当崩溃")
	}
	// 重启期间的新调用进入队列（不丢、不执行）
	waitFor(t, 2*time.Second, func() bool {
		stats := gateway.Stats()
		st, ok := stats.Domains[DomainWorkerTerminal]
		return ok && st.Restarting
	})
	_, err := gateway.Route(ctx, WorkerRequest{RunID: "r", ToolCallID: "c2", ToolName: "t", Domain: DomainWorkerTerminal})
	var unavailable *ErrWorkerUnavailable
	if !errors.As(err, &unavailable) || !unavailable.Restarting {
		t.Fatalf("重启期调用应当被排队并提示重启中: %v", err)
	}
	// 最终恢复且队列被重放
	waitFor(t, 3*time.Second, func() bool {
		router.mu.Lock()
		defer router.mu.Unlock()
		return router.calls >= 2 // 1 次崩溃 + 1 次排队重放
	})
	waitFor(t, 2*time.Second, func() bool {
		stats := gateway.Stats()
		st, ok := stats.Domains[DomainWorkerTerminal]
		return ok && st.Healthy && st.Queued == 0
	})
}

func TestWorkerGatewayQueueCapacityRejectsFast(t *testing.T) {
	router := &gatewayTestRouter{crashN: 1, failWith: &ErrWorkerUnavailable{Domain: DomainWorkerTerminal, Reason: "crash"}}
	controller := &gatewayTestController{healthFails: 1000, restartedCh: make(chan struct{}, 1)} // 一直不健康
	failures := &gatewayTestFailures{}
	gateway := newTestGateway(router, controller, failures)
	// 缩小队列容量
	gateway.cfg.MaxQueuedPerDomain = 2

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := gateway.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer gateway.Stop()

	if _, err := gateway.Route(ctx, WorkerRequest{RunID: "r", ToolCallID: "c1", ToolName: "t", Domain: DomainWorkerTerminal}); err == nil {
		t.Fatalf("应当崩溃")
	}
	waitFor(t, 2*time.Second, func() bool {
		stats := gateway.Stats()
		st, ok := stats.Domains[DomainWorkerTerminal]
		return ok && st.Restarting
	})
	// 队列上限 2：前两个排队，第三个 reject fast
	var lastErr error
	for i := 0; i < 4; i++ {
		_, err := gateway.Route(ctx, WorkerRequest{RunID: "r", ToolCallID: "q", ToolName: "t", Domain: DomainWorkerTerminal})
		if err != nil {
			lastErr = err
		}
	}
	var unavailable *ErrWorkerUnavailable
	if !errors.As(lastErr, &unavailable) {
		t.Fatalf("队列满后应当 reject fast，最后错误 = %v", lastErr)
	}
}

func TestWorkerGatewayDeadAfterMaxAttempts(t *testing.T) {
	router := &gatewayTestRouter{crashN: 1, failWith: &ErrWorkerUnavailable{Domain: DomainWorkerTerminal, Reason: "crash"}}
	controller := &gatewayTestController{restartFailN: 100, restartedCh: make(chan struct{}, 1)}
	failures := &gatewayTestFailures{}
	gateway := newTestGateway(router, controller, failures)
	gateway.cfg.MaxRestartAttempts = 3

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := gateway.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer gateway.Stop()

	if _, err := gateway.Route(ctx, WorkerRequest{RunID: "r", ToolCallID: "c1", ToolName: "t", Domain: DomainWorkerTerminal}); err == nil {
		t.Fatalf("应当崩溃")
	}
	waitFor(t, 3*time.Second, func() bool {
		stats := gateway.Stats()
		st, ok := stats.Domains[DomainWorkerTerminal]
		return ok && st.Dead
	})
	_, err := gateway.Route(ctx, WorkerRequest{RunID: "r", ToolCallID: "c2", ToolName: "t", Domain: DomainWorkerTerminal})
	var unavailable *ErrWorkerUnavailable
	if !errors.As(err, &unavailable) {
		t.Fatalf("dead 域的调用应当被拒绝: %v", err)
	}
}

func TestWorkerGatewayStopReleasesGoroutines(t *testing.T) {
	router := &gatewayTestRouter{}
	controller := &gatewayTestController{}
	failures := &gatewayTestFailures{}
	gateway := newTestGateway(router, controller, failures)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := gateway.Start(ctx); err != nil {
		t.Fatal(err)
	}
	// 触发一次崩溃让 supervisor 活跃起来
	router.mu.Lock()
	router.crashN = 1
	router.failWith = &ErrWorkerUnavailable{Domain: DomainWorkerTerminal, Reason: "crash"}
	router.mu.Unlock()
	_, _ = gateway.Route(ctx, WorkerRequest{RunID: "r", ToolCallID: "c1", ToolName: "t", Domain: DomainWorkerTerminal})

	time.Sleep(100 * time.Millisecond)
	before := countGatewayGoroutines()
	gateway.Stop()
	time.Sleep(100 * time.Millisecond)
	after := countGatewayGoroutines()
	if after > before {
		t.Fatalf("Stop 后 goroutine 未退出：before=%d after=%d", before, after)
	}
}

// countGatewayGoroutines 统计当前 goroutine 数量，用于 Stop 后的泄漏断言。
func countGatewayGoroutines() int {
	return runtime.NumGoroutine()
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("条件在 %v 内未满足", timeout)
}

// ---------------------------------------------------------------------------
// ResultNormalizer
// ---------------------------------------------------------------------------

func TestDefaultNormalizer(t *testing.T) {
	n := NewDefaultNormalizer(0)
	resp, err := n.Normalize(WorkerResponse{ToolCallID: "c1", ToolName: "t", Content: "hi", Success: true})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "hi" || !resp.Success {
		t.Fatalf("归一化结果 = %+v", resp)
	}
	// 失败但无 ErrorCode -> TOOL_FAILED
	resp, err = n.Normalize(WorkerResponse{ToolCallID: "c1", Success: false, Error: "boom"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.ErrorCode != ErrToolFailed {
		t.Fatalf("ErrorCode = %q", resp.ErrorCode)
	}
	// 透传 ErrorCode
	resp, _ = n.Normalize(WorkerResponse{ToolCallID: "c1", Success: false, Error: "x", ErrorCode: ErrCanceled})
	if resp.ErrorCode != ErrCanceled {
		t.Fatalf("ErrorCode 未透传: %q", resp.ErrorCode)
	}
	// 超大内容
	small := NewDefaultNormalizer(8)
	if _, err := small.Normalize(WorkerResponse{Content: "0123456789"}); err == nil {
		t.Fatalf("超大内容应当报错")
	}
}

// ---------------------------------------------------------------------------
// DynamicTool / create_tool
// ---------------------------------------------------------------------------

func TestDynamicToolExecution(t *testing.T) {
	sandbox := NewSandbox(testPolicy())
	dyn := NewDynamicTool("calc_tax", "计算税", ObjectSchema(map[string]JSONSchema{
		"amount": {Type: "number"},
	}), `return "tax=" + (args.amount * 0.1);`, sandbox)

	if dyn.Definition().Name != "calc_tax" {
		t.Fatalf("定义名 = %q", dyn.Definition().Name)
	}
	if dyn.Definition().Domain != DomainWorkerDynamicJS {
		t.Fatalf("动态工具必须路由到 dynamic-js worker 域")
	}
	resp := dyn.Execute(context.Background(), ToolRequest{
		RunID: "r", ToolCallID: "c1", Name: "calc_tax",
		Arguments: map[string]any{"amount": 100.0},
	})
	if !resp.Success || resp.Content != "tax=10" {
		t.Fatalf("执行结果 = %+v", resp)
	}
}

func TestDynamicToolSandboxViolationSurfaces(t *testing.T) {
	sandbox := NewSandbox(testPolicy())
	dyn := NewDynamicTool("evil", "恶意工具", ObjectSchema(nil), `return require("fs");`, sandbox)
	resp := dyn.Execute(context.Background(), ToolRequest{RunID: "r", ToolCallID: "c1", Name: "evil"})
	if resp.Success || resp.ErrorCode != ErrSandboxViolation {
		t.Fatalf("沙箱违规应当 surfaced: %+v", resp)
	}
}

func TestCreateToolValidation(t *testing.T) {
	reg := NewRegistry()
	sandbox := NewSandbox(testPolicy())
	meta := NewCreateToolTool(reg, sandbox)

	newReq := func(args map[string]any) ToolRequest {
		return ToolRequest{RunID: "r", ToolCallID: "c1", Name: "create_tool", Arguments: args}
	}

	// 非法名称
	resp := meta.Execute(context.Background(), newReq(map[string]any{
		"name": "Bad-Name", "description": "d", "parameters": map[string]any{}, "code": "return 1;",
	}))
	if resp.Success {
		t.Fatalf("非法工具名应当被拒绝")
	}

	// 含禁用 API 的代码在创建期即拒绝
	resp = meta.Execute(context.Background(), newReq(map[string]any{
		"name": "ok_tool", "description": "d", "parameters": map[string]any{"type": "object"},
		"code": `return process.version;`,
	}))
	if resp.Success {
		t.Fatalf("含禁用 API 的代码应当在创建期被拒绝")
	}
	if reg.Has("ok_tool") {
		t.Fatalf("被拒绝的工具不应进入注册表")
	}

	// 合法创建
	resp = meta.Execute(context.Background(), newReq(map[string]any{
		"name": "ok_tool", "description": "d", "parameters": map[string]any{"type": "object", "properties": map[string]any{}},
		"code": `return "hello";`,
	}))
	if !resp.Success {
		t.Fatalf("合法创建失败: %+v", resp)
	}
	if !reg.Has("ok_tool") {
		t.Fatalf("工具未注册")
	}

	// 重复创建被拒绝
	resp = meta.Execute(context.Background(), newReq(map[string]any{
		"name": "ok_tool", "description": "d", "parameters": map[string]any{"type": "object"},
		"code": `return "hello2";`,
	}))
	if resp.Success {
		t.Fatalf("重复创建应当被拒绝")
	}
}
