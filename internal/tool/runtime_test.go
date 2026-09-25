package tool

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/secrets"
)

// ---------------------------------------------------------------------------
// 测试替身（tool 包测试内联定义，避免与 mock 包形成测试环）
// ---------------------------------------------------------------------------

type testPool struct {
	mu       sync.Mutex
	capacity map[ResourceKind]int
	used     map[ResourceKind]int
	leases   map[string]Lease
	failOn   map[ResourceKind]bool
	nextID   int
}

func newTestPool() *testPool {
	return &testPool{
		capacity: map[ResourceKind]int{ResourceToolCall: 64, ResourceTerminal: 4},
		used:     map[ResourceKind]int{},
		leases:   map[string]Lease{},
		failOn:   map[ResourceKind]bool{},
	}
}

func (p *testPool) Reserve(ctx context.Context, kind ResourceKind, n int) (Lease, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failOn[kind] {
		return Lease{}, fmt.Errorf("资源 %s 不可用", kind)
	}
	if p.used[kind]+n > p.capacity[kind] {
		return Lease{}, fmt.Errorf("资源 %s 不足", kind)
	}
	p.used[kind] += n
	p.nextID++
	lease := Lease{ID: fmt.Sprintf("lease-%d", p.nextID), Kind: kind, Amount: n, AcquiredAt: time.Now()}
	p.leases[lease.ID] = lease
	return lease, nil
}

func (p *testPool) Release(ctx context.Context, lease Lease) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	existing, ok := p.leases[lease.ID]
	if !ok {
		return nil
	}
	delete(p.leases, lease.ID)
	p.used[existing.Kind] -= existing.Amount
	return nil
}

func (p *testPool) outstanding() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.leases)
}

type sinkEvent struct {
	runID   string
	typ     string
	payload []byte
}

type testEventSink struct {
	mu     sync.Mutex
	events []sinkEvent
	byType map[string]int
}

func newTestEventSink() *testEventSink {
	return &testEventSink{byType: map[string]int{}}
}

func (s *testEventSink) Append(ctx context.Context, runID string, eventType string, payload []byte) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, sinkEvent{runID: runID, typ: eventType, payload: append([]byte(nil), payload...)})
	s.byType[eventType]++
	return uint64(len(s.events)), nil
}

func (s *testEventSink) count(eventType string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byType[eventType]
}

type testAuditLogger struct {
	mu      sync.Mutex
	records []AuditRecord
}

func (a *testAuditLogger) Log(ctx context.Context, rec AuditRecord) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.records = append(a.records, rec)
	return nil
}

func (a *testAuditLogger) count(event AuditEventType) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, r := range a.records {
		if r.Event == event {
			n++
		}
	}
	return n
}

type testConfirmer struct {
	mu       sync.Mutex
	approved bool
	calls    int
}

func (c *testConfirmer) RequestConfirmation(ctx context.Context, req ConfirmationRequest) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return c.approved, nil
}

type testCrashSink struct {
	mu      sync.Mutex
	records []CrashRecord
}

func (c *testCrashSink) RecordCrash(ctx context.Context, rec CrashRecord) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, rec)
}

func (c *testCrashSink) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.records)
}

// countingTool 统计执行次数。
type countingTool struct {
	def      ToolDefinition
	execs    atomic.Int64
	response func(req ToolRequest) ToolResponse
	panic    bool
	sleep    time.Duration
}

func (t *countingTool) Definition() ToolDefinition { return t.def }

func (t *countingTool) Execute(ctx context.Context, req ToolRequest) ToolResponse {
	t.execs.Add(1)
	if t.sleep > 0 {
		select {
		case <-time.After(t.sleep):
		case <-ctx.Done():
			return ToolResponse{ToolCallID: req.ToolCallID, ToolName: t.def.Name, Success: false, Error: ctx.Err().Error(), ErrorCode: ErrCanceled}
		}
	}
	if t.panic {
		panic("boom")
	}
	if t.response != nil {
		return t.response(req)
	}
	return ToolResponse{ToolCallID: req.ToolCallID, ToolName: t.def.Name, Content: "ok", Success: true}
}

type fixture struct {
	rt        *ToolRuntime
	registry  Registry
	pool      *testPool
	sink      *testEventSink
	audit     *testAuditLogger
	confirmer *testConfirmer
	crashes   *testCrashSink
	idem      *Store
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{
		registry:  NewRegistry(),
		pool:      newTestPool(),
		sink:      newTestEventSink(),
		audit:     &testAuditLogger{},
		confirmer: &testConfirmer{approved: true},
		crashes:   &testCrashSink{},
	}
	engine, err := NewPermissionEngine(DefaultPermissionConfigs(), nil)
	if err != nil {
		t.Fatal(err)
	}
	resolver := NewMemoryResolver()
	f.idem = NewIdempotencyStore(NewMemoryIdempotencyDB(), resolver, nil, 5*time.Minute)
	f.rt = &ToolRuntime{
		Registry:     f.registry,
		Permission:   engine,
		ResourcePool: f.pool,
		Idempotency:  f.idem,
		Audit:        f.audit,
		Events:       f.sink,
		Crashes:      f.crashes,
		Confirmer:    f.confirmer,
		Now:          time.Now,
	}
	return f
}

func (f *fixture) register(t *testing.T, tool Tool) {
	t.Helper()
	if err := f.registry.Register(tool); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) resolverAdd(t *testing.T, toolCallID, toolName string, args map[string]any) {
	t.Helper()
	r, ok := f.idem.Resolver.(*memoryResolver)
	if !ok {
		t.Fatalf("resolver 类型错误")
	}
	r.Add(toolCallID, toolName, args)
}

func readToolDef() ToolDefinition {
	return ToolDefinition{
		Name:        "file_read",
		Description: "读取文件",
		Parameters:  ObjectSchema(map[string]JSONSchema{"filePath": {Type: "string"}}, "filePath"),
		Risk:        RiskLow,
		Idempotency: ClassIdempotent,
		Domain:      DomainInProcess,
	}
}

// ---------------------------------------------------------------------------
// 调用链
// ---------------------------------------------------------------------------

func TestRuntimeHappyPath(t *testing.T) {
	f := newFixture(t)
	tool := &countingTool{def: readToolDef()}
	f.register(t, tool)
	f.resolverAdd(t, "call-1", "file_read", map[string]any{"filePath": "/tmp/a.txt"})

	resp := f.rt.Execute(context.Background(), ToolRequest{
		RunID: "run-1", ToolCallID: "call-1", Name: "file_read",
		Arguments: map[string]any{"filePath": "/tmp/a.txt"},
		Mode:      ModeCoding,
	})
	if !resp.Success || resp.Content != "ok" {
		t.Fatalf("响应 = %+v", resp)
	}
	if resp.ErrorCode != ErrOK {
		t.Fatalf("ErrorCode = %q", resp.ErrorCode)
	}
	if tool.execs.Load() != 1 {
		t.Fatalf("执行次数 = %d", tool.execs.Load())
	}
	if f.pool.outstanding() != 0 {
		t.Fatalf("租约未释放：%d", f.pool.outstanding())
	}
	if f.sink.count("tool.started") != 1 || f.sink.count("tool.completed") != 1 {
		t.Fatalf("事件 = %v", f.sink.events)
	}
	if f.audit.count(AuditToolCompleted) != 1 {
		t.Fatalf("审计记录 = %+v", f.audit.records)
	}
}

func TestRuntimeSchemaValidationFails(t *testing.T) {
	f := newFixture(t)
	tool := &countingTool{def: readToolDef()}
	f.register(t, tool)

	resp := f.rt.Execute(context.Background(), ToolRequest{
		RunID: "run-1", ToolCallID: "call-1", Name: "file_read",
		Arguments: map[string]any{}, Mode: ModeCoding,
	})
	if resp.Success || resp.ErrorCode != ErrSchemaInvalid {
		t.Fatalf("响应 = %+v", resp)
	}
	if tool.execs.Load() != 0 {
		t.Fatalf("schema 失败不应执行工具")
	}
	if f.pool.outstanding() != 0 {
		t.Fatalf("租约泄漏")
	}
}

func TestRuntimeToolNotFound(t *testing.T) {
	f := newFixture(t)
	resp := f.rt.Execute(context.Background(), ToolRequest{
		RunID: "run-1", ToolCallID: "call-1", Name: "nope", Mode: ModeCoding,
	})
	if resp.ErrorCode != ErrToolNotFound {
		t.Fatalf("ErrorCode = %q", resp.ErrorCode)
	}
}

func TestRuntimePermissionDenied(t *testing.T) {
	f := newFixture(t)
	tool := &countingTool{def: ToolDefinition{
		Name: "act_ui", Description: "UI 操作",
		Parameters: ObjectSchema(nil),
		Risk:       RiskCritical, Idempotency: ClassNonIdempotent, Domain: DomainInProcess,
	}}
	f.register(t, tool)
	f.resolverAdd(t, "call-1", "act_ui", nil)

	resp := f.rt.Execute(context.Background(), ToolRequest{
		RunID: "run-1", ToolCallID: "call-1", Name: "act_ui", Mode: ModeCoding,
	})
	if resp.Success || resp.ErrorCode != ErrPermissionDenied {
		t.Fatalf("响应 = %+v", resp)
	}
	if tool.execs.Load() != 0 {
		t.Fatalf("被拒绝的调用不应执行")
	}
	if f.audit.count(AuditPermissionDenied) != 1 {
		t.Fatalf("缺少拒绝审计")
	}
	if f.pool.outstanding() != 0 {
		t.Fatalf("租约泄漏")
	}
}

func TestRuntimePermissionAskFlows(t *testing.T) {
	def := ToolDefinition{
		Name: "file_delete", Description: "删除文件",
		Parameters: ObjectSchema(map[string]JSONSchema{"filePath": {Type: "string"}}, "filePath"),
		Risk:       RiskHigh, Idempotency: ClassNonIdempotent, Domain: DomainInProcess, SideEffect: true,
	}
	// 批准的 ask -> 执行
	f := newFixture(t)
	tool := &countingTool{def: def}
	f.register(t, tool)
	f.resolverAdd(t, "call-1", "file_delete", map[string]any{"filePath": "/tmp/x"})
	resp := f.rt.Execute(context.Background(), ToolRequest{
		RunID: "run-1", ToolCallID: "call-1", Name: "file_delete",
		Arguments: map[string]any{"filePath": "/tmp/x"}, Mode: ModeCoding,
	})
	if !resp.Success {
		t.Fatalf("批准的 ask 应当执行: %+v", resp)
	}
	if f.confirmer.calls != 1 {
		t.Fatalf("确认次数 = %d", f.confirmer.calls)
	}

	// 拒绝的 ask -> 不执行
	f2 := newFixture(t)
	f2.confirmer.approved = false
	tool2 := &countingTool{def: def}
	f2.register(t, tool2)
	f2.resolverAdd(t, "call-1", "file_delete", map[string]any{"filePath": "/tmp/x"})
	resp2 := f2.rt.Execute(context.Background(), ToolRequest{
		RunID: "run-1", ToolCallID: "call-1", Name: "file_delete",
		Arguments: map[string]any{"filePath": "/tmp/x"}, Mode: ModeCoding,
	})
	if resp2.Success || resp2.ErrorCode != ErrPermissionDenied {
		t.Fatalf("拒绝的 ask 不应执行: %+v", resp2)
	}
	if tool2.execs.Load() != 0 {
		t.Fatalf("拒绝后不应执行")
	}

	// 无确认器 -> fail-closed
	f3 := newFixture(t)
	f3.rt.Confirmer = nil
	tool3 := &countingTool{def: def}
	f3.register(t, tool3)
	f3.resolverAdd(t, "call-1", "file_delete", map[string]any{"filePath": "/tmp/x"})
	resp3 := f3.rt.Execute(context.Background(), ToolRequest{
		RunID: "run-1", ToolCallID: "call-1", Name: "file_delete",
		Arguments: map[string]any{"filePath": "/tmp/x"}, Mode: ModeCoding,
	})
	if resp3.Success || resp3.ErrorCode != ErrPermissionDenied {
		t.Fatalf("无确认器必须 fail-closed: %+v", resp3)
	}
	if tool3.execs.Load() != 0 {
		t.Fatalf("fail-closed 后不应执行")
	}
}

type testAdmission struct {
	fail bool
}

func (a *testAdmission) AdmitToolCall(ctx context.Context, req AdmissionRequest) error {
	if a.fail {
		return errors.New("队列已满")
	}
	return nil
}

func TestRuntimeAdmissionRejected(t *testing.T) {
	f := newFixture(t)
	f.rt.Admission = &testAdmission{fail: true}
	tool := &countingTool{def: readToolDef()}
	f.register(t, tool)
	f.resolverAdd(t, "call-1", "file_read", map[string]any{"filePath": "/tmp/a"})

	resp := f.rt.Execute(context.Background(), ToolRequest{
		RunID: "run-1", ToolCallID: "call-1", Name: "file_read",
		Arguments: map[string]any{"filePath": "/tmp/a"}, Mode: ModeCoding,
	})
	if resp.ErrorCode != ErrAdmissionRejected {
		t.Fatalf("ErrorCode = %q", resp.ErrorCode)
	}
	if tool.execs.Load() != 0 {
		t.Fatalf("准入拒绝不应执行")
	}
	if f.pool.outstanding() != 0 {
		t.Fatalf("租约泄漏")
	}
}

func TestRuntimeResourceExhaustedReleasesEverything(t *testing.T) {
	f := newFixture(t)
	f.pool.failOn[ResourceToolCall] = true
	tool := &countingTool{def: readToolDef()}
	f.register(t, tool)
	f.resolverAdd(t, "call-1", "file_read", map[string]any{"filePath": "/tmp/a"})

	resp := f.rt.Execute(context.Background(), ToolRequest{
		RunID: "run-1", ToolCallID: "call-1", Name: "file_read",
		Arguments: map[string]any{"filePath": "/tmp/a"}, Mode: ModeCoding,
	})
	if resp.ErrorCode != ErrResourceExhausted {
		t.Fatalf("ErrorCode = %q", resp.ErrorCode)
	}
	if f.pool.outstanding() != 0 {
		t.Fatalf("资源预留失败后仍有租约")
	}
	// 认领必须被释放为 failed（允许重试），不能留在 inflight
	status, _, found, _ := f.idem.Get(context.Background(), Key("run-1", "call-1"))
	if !found || status != StatusFailed {
		t.Fatalf("认领状态 = %q found=%v，期望 failed", status, found)
	}
}

func TestRuntimeEveryFailureStepReleasesResources(t *testing.T) {
	// 表格驱动：让调用链的每一步失败，断言租约总是 0、认领不泄漏。
	// claimState 描述失败后期望的幂等记录状态：
	//   ""        = 不产生记录（失败发生在认领之前）
	//   failed    = A/B 类失败，允许重试
	//   done      = 业务失败已作为 durable result 提交（I3）
	//   needsconf = C 类失败，标记需用户确认
	cases := []struct {
		name       string
		setup      func(f *fixture, tool *countingTool)
		want       ErrorCode
		claimState string
	}{
		{"schema", func(f *fixture, tool *countingTool) {}, ErrSchemaInvalid, ""},
		{"permission", func(f *fixture, tool *countingTool) {
			f.rt.Permission = denyAllEngine{}
		}, ErrPermissionDenied, ""},
		{"admission", func(f *fixture, tool *countingTool) {
			f.rt.Admission = &testAdmission{fail: true}
		}, ErrAdmissionRejected, ""},
		{"resource", func(f *fixture, tool *countingTool) {
			f.pool.failOn[ResourceToolCall] = true
		}, ErrResourceExhausted, "failed"},
		{"tool_failure", func(f *fixture, tool *countingTool) {
			tool.response = func(req ToolRequest) ToolResponse {
				return ToolResponse{Success: false, Error: "业务失败"}
			}
		}, ErrToolFailed, "done"},
		{"tool_panic", func(f *fixture, tool *countingTool) {
			tool.panic = true
		}, ErrToolPanicked, "failed"},
		{"timeout", func(f *fixture, tool *countingTool) {
			tool.sleep = 2 * time.Second
			tool.def.Timeout = 50 * time.Millisecond
		}, ErrCanceled, "failed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			tool := &countingTool{def: readToolDef()}
			c.setup(f, tool)
			f.register(t, tool)
			f.resolverAdd(t, "call-x", "file_read", map[string]any{"filePath": "/tmp/a"})
			req := ToolRequest{
				RunID: "run-x", ToolCallID: "call-x", Name: "file_read",
				Arguments: map[string]any{"filePath": "/tmp/a"}, Mode: ModeCoding,
			}
			if c.name == "schema" {
				req.Arguments = map[string]any{} // 缺必填参数
			}
			resp := f.rt.Execute(context.Background(), req)
			if resp.ErrorCode != c.want {
				t.Fatalf("ErrorCode = %q（%s），期望 %q", resp.ErrorCode, resp.Error, c.want)
			}
			if f.pool.outstanding() != 0 {
				t.Fatalf("步骤 %q 失败后租约未释放：%d", c.name, f.pool.outstanding())
			}
			status, _, found, _ := f.idem.Get(context.Background(), Key("run-x", "call-x"))
			switch c.claimState {
			case "":
				if found {
					t.Fatalf("步骤 %q 失败后不应有幂等记录，得到 %q", c.name, status)
				}
			case "failed":
				if !found || status != StatusFailed {
					t.Fatalf("步骤 %q 失败后认领状态 = %q found=%v，期望 failed", c.name, status, found)
				}
			case "done":
				if !found || status != StatusDone {
					t.Fatalf("步骤 %q 失败后记录状态 = %q found=%v，期望 done（业务失败也是 durable result）", c.name, status, found)
				}
			case "needsconf":
				if !found || status != StatusNeedsConfirmation {
					t.Fatalf("步骤 %q 失败后记录状态 = %q found=%v，期望 needs_confirmation", c.name, status, found)
				}
			}
		})
	}
}

// denyAllEngine 全部拒绝的权限引擎（测试用）。
type denyAllEngine struct{}

func (denyAllEngine) Evaluate(ctx context.Context, req PermissionRequest) Decision {
	return Decision{Effect: EffectDeny, Reason: "测试拒绝", Risk: RiskHigh}
}
func (denyAllEngine) Explain(ctx context.Context, req PermissionRequest) Explanation {
	return Explanation{Decision: Decision{Effect: EffectDeny, Reason: "测试拒绝"}}
}
func (denyAllEngine) AddUserRule(ctx context.Context, rule Rule) error { return nil }
func (denyAllEngine) RemoveUserRule(ctx context.Context, ruleID string) error {
	return nil
}

func TestRuntimePanicIsolated(t *testing.T) {
	f := newFixture(t)
	tool := &countingTool{def: readToolDef(), panic: true}
	f.register(t, tool)
	f.resolverAdd(t, "call-1", "file_read", map[string]any{"filePath": "/tmp/a"})

	resp := f.rt.Execute(context.Background(), ToolRequest{
		RunID: "run-1", ToolCallID: "call-1", Name: "file_read",
		Arguments: map[string]any{"filePath": "/tmp/a"}, Mode: ModeCoding,
	})
	if resp.Success || resp.ErrorCode != ErrToolPanicked {
		t.Fatalf("响应 = %+v", resp)
	}
	if f.crashes.count() != 1 {
		t.Fatalf("panic 应当记录崩溃：%d", f.crashes.count())
	}
	if f.audit.count(AuditToolPanicked) != 1 {
		t.Fatalf("缺少 panic 审计")
	}
	if f.pool.outstanding() != 0 {
		t.Fatalf("panic 后租约泄漏")
	}
}

func TestRuntimeIdempotentCacheHit(t *testing.T) {
	f := newFixture(t)
	tool := &countingTool{def: readToolDef()}
	f.register(t, tool)
	f.resolverAdd(t, "call-1", "file_read", map[string]any{"filePath": "/tmp/a"})
	req := ToolRequest{
		RunID: "run-1", ToolCallID: "call-1", Name: "file_read",
		Arguments: map[string]any{"filePath": "/tmp/a"}, Mode: ModeCoding,
	}
	first := f.rt.Execute(context.Background(), req)
	second := f.rt.Execute(context.Background(), req)
	if !first.Success || !second.Success {
		t.Fatalf("两次都应成功: %+v / %+v", first, second)
	}
	if !second.Cached {
		t.Fatalf("第二次应当命中缓存")
	}
	if tool.execs.Load() != 1 {
		t.Fatalf("执行次数 = %d，缓存命中不应重复执行", tool.execs.Load())
	}
	if f.audit.count(AuditIdempotencyReplay) != 1 {
		t.Fatalf("缺少缓存命中审计")
	}
}

func TestRuntimeI5NoParallelDoubleExecution(t *testing.T) {
	f := newFixture(t)
	var execs atomic.Int64
	tool := &countingTool{def: readToolDef(), response: func(req ToolRequest) ToolResponse {
		execs.Add(1)
		time.Sleep(20 * time.Millisecond) // 拉长执行窗口
		return ToolResponse{ToolCallID: req.ToolCallID, ToolName: "file_read", Content: "ok", Success: true}
	}}
	f.register(t, tool)
	f.resolverAdd(t, "call-1", "file_read", map[string]any{"filePath": "/tmp/a"})
	req := ToolRequest{
		RunID: "run-1", ToolCallID: "call-1", Name: "file_read",
		Arguments: map[string]any{"filePath": "/tmp/a"}, Mode: ModeCoding,
	}

	var wg sync.WaitGroup
	results := make([]ToolResponse, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = f.rt.Execute(context.Background(), req)
		}(i)
	}
	wg.Wait()

	success := 0
	for _, r := range results {
		if r.Success {
			success++
		}
	}
	if execs.Load() != 1 {
		t.Fatalf("I5 违反：同一 key 并发执行了 %d 次", execs.Load())
	}
	if success == 0 {
		t.Fatalf("并发下没有任何调用成功")
	}
	if f.pool.outstanding() != 0 {
		t.Fatalf("并发后租约泄漏：%d", f.pool.outstanding())
	}
}

type stateCheckedTool struct {
	def   ToolDefinition
	state StateStatus
	execs atomic.Int64
}

func (t *stateCheckedTool) Definition() ToolDefinition { return t.def }

func (t *stateCheckedTool) Execute(ctx context.Context, req ToolRequest) ToolResponse {
	t.execs.Add(1)
	return ToolResponse{ToolCallID: req.ToolCallID, ToolName: t.def.Name, Content: "written", Success: true}
}

func (t *stateCheckedTool) CheckState(ctx context.Context, req ToolRequest) (StateStatus, error) {
	return t.state, nil
}

func TestRuntimeBClassStateCheckSkipsExecution(t *testing.T) {
	f := newFixture(t)
	tool := &stateCheckedTool{
		def: ToolDefinition{
			Name: "file_write", Description: "写文件",
			Parameters:  ObjectSchema(map[string]JSONSchema{"filePath": {Type: "string"}, "content": {Type: "string"}}, "filePath", "content"),
			Risk:        RiskMedium,
			Idempotency: ClassDetectable,
			Domain:      DomainInProcess,
			SideEffect:  true,
		},
		state: StateAlreadyDone,
	}
	f.register(t, tool)
	f.resolverAdd(t, "call-1", "file_write", map[string]any{"filePath": "/tmp/a", "content": "x"})

	resp := f.rt.Execute(context.Background(), ToolRequest{
		RunID: "run-1", ToolCallID: "call-1", Name: "file_write",
		Arguments: map[string]any{"filePath": "/tmp/a", "content": "x"}, Mode: ModeCoding,
	})
	if !resp.Success || !resp.Cached {
		t.Fatalf("状态已达标应当跳过执行并标记 cached: %+v", resp)
	}
	if tool.execs.Load() != 0 {
		t.Fatalf("状态已达标不应执行工具")
	}
	if f.pool.outstanding() != 0 {
		t.Fatalf("租约泄漏")
	}
}

func TestRuntimeNonIdempotentMarkedNeedsConfirmation(t *testing.T) {
	f := newFixture(t)
	tool := &countingTool{def: ToolDefinition{
		Name: "file_delete", Description: "删除",
		Parameters:  ObjectSchema(map[string]JSONSchema{"filePath": {Type: "string"}}, "filePath"),
		Risk:        RiskHigh,
		Idempotency: ClassNonIdempotent,
		Domain:      DomainInProcess,
		SideEffect:  true,
	}}
	f.register(t, tool)
	f.resolverAdd(t, "call-1", "file_delete", map[string]any{"filePath": "/tmp/x"})
	key := Key("run-1", "call-1")

	// 模拟崩溃遗留：inflight 且已陈旧
	ctx := context.Background()
	if _, err := f.idem.Claim(ctx, key); err != nil {
		t.Fatal(err)
	}
	f.idem.Now = func() time.Time { return time.Now().Add(10 * time.Minute) }

	resp := f.rt.Execute(ctx, ToolRequest{
		RunID: "run-1", ToolCallID: "call-1", Name: "file_delete",
		Arguments: map[string]any{"filePath": "/tmp/x"}, Mode: ModeCoding,
	})
	if resp.Success || resp.ErrorCode != ErrNeedsConfirmation {
		t.Fatalf("C 类崩溃恢复必须标记 NEEDS_CONFIRMATION: %+v", resp)
	}
	if tool.execs.Load() != 0 {
		t.Fatalf("NEEDS_CONFIRMATION 的调用绝不自动重复执行")
	}
	status, _, _, _ := f.idem.Get(ctx, key)
	if status != StatusNeedsConfirmation {
		t.Fatalf("状态 = %q，期望 needs_confirmation", status)
	}
}

type recordingWorkerRouter struct {
	mu       sync.Mutex
	requests []WorkerRequest
	failWith error
}

func (r *recordingWorkerRouter) Route(ctx context.Context, req WorkerRequest) (WorkerResponse, error) {
	r.mu.Lock()
	r.requests = append(r.requests, req)
	fail := r.failWith
	r.mu.Unlock()
	if fail != nil {
		return WorkerResponse{}, fail
	}
	return WorkerResponse{
		ToolCallID: req.ToolCallID, ToolName: req.ToolName,
		Content: "worker-output", Success: true,
	}, nil
}

func terminalToolDef() ToolDefinition {
	return ToolDefinition{
		Name: "terminal_exec", Description: "终端",
		Parameters:  ObjectSchema(map[string]JSONSchema{"command": {Type: "string"}}, "command"),
		Risk:        RiskHigh,
		Idempotency: ClassNonIdempotent,
		Domain:      DomainWorkerTerminal,
		SideEffect:  true,
	}
}

func TestRuntimeWorkerRoutingForHighRisk(t *testing.T) {
	f := newFixture(t)
	router := &recordingWorkerRouter{}
	f.rt.Workers = router
	tool := &countingTool{def: terminalToolDef()}
	f.register(t, tool)
	f.resolverAdd(t, "call-1", "terminal_exec", map[string]any{"command": "ls"})

	resp := f.rt.Execute(context.Background(), ToolRequest{
		RunID: "run-1", ToolCallID: "call-1", Name: "terminal_exec",
		Arguments: map[string]any{"command": "ls"}, Mode: ModeCoding,
	})
	if !resp.Success {
		t.Fatalf("worker 路由失败: %+v", resp)
	}
	if resp.Domain != DomainWorkerTerminal {
		t.Fatalf("Domain = %v", resp.Domain)
	}
	if len(router.requests) != 1 {
		t.Fatalf("worker 请求次数 = %d", len(router.requests))
	}
	if tool.execs.Load() != 0 {
		t.Fatalf("高风险工具不应在进程内执行")
	}
}

func TestRuntimeWorkerCrashMarksFailure(t *testing.T) {
	f := newFixture(t)
	router := &recordingWorkerRouter{failWith: &ErrWorkerUnavailable{Domain: DomainWorkerTerminal, Reason: "worker 崩溃"}}
	f.rt.Workers = router
	tool := &countingTool{def: terminalToolDef()}
	f.register(t, tool)
	f.resolverAdd(t, "call-1", "terminal_exec", map[string]any{"command": "ls"})

	resp := f.rt.Execute(context.Background(), ToolRequest{
		RunID: "run-1", ToolCallID: "call-1", Name: "terminal_exec",
		Arguments: map[string]any{"command": "ls"}, Mode: ModeCoding,
	})
	if resp.ErrorCode != ErrWorkerCrashed {
		t.Fatalf("ErrorCode = %q（%s）", resp.ErrorCode, resp.Error)
	}
	if f.pool.outstanding() != 0 {
		t.Fatalf("worker 崩溃后租约泄漏")
	}
	if f.audit.count(AuditWorkerCrashed) != 1 {
		t.Fatalf("缺少 worker 崩溃审计")
	}
}

func TestRuntimeNoWorkerRouterRejectsHighRisk(t *testing.T) {
	f := newFixture(t) // 未配置 Workers
	tool := &countingTool{def: terminalToolDef()}
	f.register(t, tool)
	f.resolverAdd(t, "call-1", "terminal_exec", map[string]any{"command": "ls"})

	resp := f.rt.Execute(context.Background(), ToolRequest{
		RunID: "run-1", ToolCallID: "call-1", Name: "terminal_exec",
		Arguments: map[string]any{"command": "ls"}, Mode: ModeCoding,
	})
	if resp.Success || resp.ErrorCode != ErrWorkerCrashed {
		t.Fatalf("未配置 Worker 路由必须拒绝高风险工具: %+v", resp)
	}
	if tool.execs.Load() != 0 {
		t.Fatalf("高风险工具不得在进程内兜底执行")
	}
}

func TestRuntimeSecretRedactionAtSerialization(t *testing.T) {
	f := newFixture(t)
	redactor := secrets.NewRedactor()
	redactor.Track("secretref:v1:abc", "sk-super-secret-value")
	f.rt.Redactor = redactor
	tool := &countingTool{def: readToolDef()}
	f.register(t, tool)
	f.resolverAdd(t, "call-1", "file_read", map[string]any{"filePath": "/tmp/a"})

	resp := f.rt.Execute(context.Background(), ToolRequest{
		RunID: "run-1", ToolCallID: "call-1", Name: "file_read",
		Arguments: map[string]any{"filePath": "/tmp/a"}, Mode: ModeCoding,
	})
	if !resp.Success {
		t.Fatalf("执行失败: %+v", resp)
	}
	// I12：序列化处的兜底过滤
	data, err := redactor.RedactJSON([]byte(`{"note":"key is sk-super-secret-value","api_key":"sk-super-secret-value"}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "sk-super-secret-value") {
		t.Fatalf("I12 违反：秘密进入事件 payload: %s", data)
	}
}

func TestRuntimeDurableCommitRedactsSecrets(t *testing.T) {
	f := newFixture(t)
	redactor := secrets.NewRedactor()
	redactor.Track("secretref:v1:db", "sk-db-plaintext-key")
	f.rt.Redactor = redactor
	// 工具结果意外包含密钥
	tool := &countingTool{def: readToolDef(), response: func(req ToolRequest) ToolResponse {
		return ToolResponse{ToolCallID: req.ToolCallID, ToolName: "file_read", Content: "config: sk-db-plaintext-key", Success: true}
	}}
	f.register(t, tool)
	f.resolverAdd(t, "call-1", "file_read", map[string]any{"filePath": "/tmp/a"})

	resp := f.rt.Execute(context.Background(), ToolRequest{
		RunID: "run-1", ToolCallID: "call-1", Name: "file_read",
		Arguments: map[string]any{"filePath": "/tmp/a"}, Mode: ModeCoding,
	})
	if !resp.Success {
		t.Fatalf("执行失败: %+v", resp)
	}
	// 内存返回保持原样（LLM 需要真实输出）
	if !strings.Contains(resp.Content, "sk-db-plaintext-key") {
		t.Fatalf("返回给调用方的内容被修改: %q", resp.Content)
	}
	// 持久化到 tool_idempotency 的副本必须脱敏（第21章）
	_, result, found, _ := f.idem.Get(context.Background(), Key("run-1", "call-1"))
	if !found {
		t.Fatalf("缺少持久化结果")
	}
	if strings.Contains(string(result), "sk-db-plaintext-key") {
		t.Fatalf("第21章 违反：数据库中的结果包含明文 key: %s", result)
	}
}

func TestRuntimeGoroutineCleanupOnCancel(t *testing.T) {
	f := newFixture(t)
	tool := &countingTool{def: readToolDef(), sleep: 300 * time.Millisecond}
	f.register(t, tool)
	f.resolverAdd(t, "call-1", "file_read", map[string]any{"filePath": "/tmp/a"})

	before := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	resp := f.rt.Execute(ctx, ToolRequest{
		RunID: "run-1", ToolCallID: "call-1", Name: "file_read",
		Arguments: map[string]any{"filePath": "/tmp/a"}, Mode: ModeCoding,
	})
	if resp.Success {
		t.Fatalf("取消后不应成功")
	}
	// 等待工具 goroutine 自然退出后对比
	time.Sleep(500 * time.Millisecond)
	after := runtime.NumGoroutine()
	if after > before+2 {
		t.Fatalf("goroutine 泄漏：before=%d after=%d", before, after)
	}
	if f.pool.outstanding() != 0 {
		t.Fatalf("取消后租约泄漏")
	}
}
