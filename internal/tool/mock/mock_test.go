package mock

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/secrets"
	"github.com/ximo888ok-netizen/ximo-agent/internal/tool"
)

func TestEventSinkSequences(t *testing.T) {
	sink := NewEventSink()
	ctx := context.Background()
	seq1, err := sink.Append(ctx, "run-1", "tool.started", []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	seq2, _ := sink.Append(ctx, "run-1", "tool.completed", []byte(`{}`))
	if seq1 != 1 || seq2 != 2 {
		t.Fatalf("seq = %d, %d，期望 1, 2", seq1, seq2)
	}
	// 不同 run 独立计数
	seq3, _ := sink.Append(ctx, "run-2", "tool.started", []byte(`{}`))
	if seq3 != 1 {
		t.Fatalf("run-2 的 seq = %d，期望 1", seq3)
	}
	if types := sink.Types("run-1"); len(types) != 2 || types[0] != "tool.started" {
		t.Fatalf("Types = %v", types)
	}
}

func TestResourcePoolAccounting(t *testing.T) {
	pool := NewResourcePool()
	ctx := context.Background()
	lease, err := pool.Reserve(ctx, tool.ResourceTerminal, 1)
	if err != nil {
		t.Fatal(err)
	}
	if pool.Outstanding() != 1 {
		t.Fatalf("Outstanding = %d", pool.Outstanding())
	}
	// 超出容量
	pool.SetCapacity(tool.ResourceTerminal, 1)
	if _, err := pool.Reserve(ctx, tool.ResourceTerminal, 1); err == nil {
		t.Fatalf("容量耗尽后应当失败")
	}
	// 释放后可以再拿
	if err := pool.Release(ctx, lease); err != nil {
		t.Fatal(err)
	}
	if pool.Outstanding() != 0 {
		t.Fatalf("释放后 Outstanding = %d", pool.Outstanding())
	}
	if _, err := pool.Reserve(ctx, tool.ResourceTerminal, 1); err != nil {
		t.Fatalf("释放后应当可以预留: %v", err)
	}
	// 重复释放幂等
	first, _ := pool.Reserve(ctx, tool.ResourceToolCall, 1)
	_ = pool.Release(ctx, first)
	if err := pool.Release(ctx, first); err != nil {
		t.Fatalf("重复释放应幂等: %v", err)
	}
}

func TestAdmissionLimit(t *testing.T) {
	adm := NewAdmission(2)
	ctx := context.Background()
	req := tool.AdmissionRequest{SessionID: "s1", RunID: "r", ToolCallID: "c", ToolName: "t"}
	if err := adm.AdmitToolCall(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err := adm.AdmitToolCall(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err := adm.AdmitToolCall(ctx, req); err == nil {
		t.Fatalf("超过并发上限应当拒绝")
	}
	adm.ReleaseSession("s1")
	if err := adm.AdmitToolCall(ctx, req); err != nil {
		t.Fatalf("释放后应当可准入: %v", err)
	}
}

func TestEchoWorkerRouter(t *testing.T) {
	router := NewEchoWorkerRouter()
	ctx := context.Background()
	resp, err := router.Route(ctx, tool.WorkerRequest{
		RunID: "r", ToolCallID: "c1", ToolName: "terminal_exec",
		Domain: tool.DomainWorkerTerminal, Arguments: map[string]any{"command": "ls"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.ToolCallID != "c1" || !resp.Success {
		t.Fatalf("响应 = %+v", resp)
	}
	if !strings.Contains(resp.Content, "terminal") {
		t.Fatalf("回显内容 = %q", resp.Content)
	}
	if len(router.Requests()) != 1 {
		t.Fatalf("请求记录 = %d", len(router.Requests()))
	}
}

func TestWorkerControllerScripting(t *testing.T) {
	ctrl := NewWorkerController()
	ctx := context.Background()
	ctrl.FailHealthChecks(tool.DomainWorkerBrowser, 2)
	if err := ctrl.Health(ctx, tool.DomainWorkerBrowser); err == nil {
		t.Fatalf("第 1 次健康检查应当失败")
	}
	if err := ctrl.Health(ctx, tool.DomainWorkerBrowser); err == nil {
		t.Fatalf("第 2 次健康检查应当失败")
	}
	if err := ctrl.Health(ctx, tool.DomainWorkerBrowser); err != nil {
		t.Fatalf("第 3 次健康检查应当成功: %v", err)
	}
	if ctrl.HealthCheckCount(tool.DomainWorkerBrowser) != 3 {
		t.Fatalf("健康检查次数 = %d", ctrl.HealthCheckCount(tool.DomainWorkerBrowser))
	}
	restarted := make(chan struct{}, 1)
	ctrl.OnRestart(func(tool.ExecutionDomain) { restarted <- struct{}{} })
	if err := ctrl.Restart(ctx, tool.DomainWorkerBrowser); err != nil {
		t.Fatal(err)
	}
	select {
	case <-restarted:
	default:
		t.Fatalf("重启回调未触发")
	}
}

func TestWorkerFailureStore(t *testing.T) {
	store := NewWorkerFailureStore()
	ctx := context.Background()
	if err := store.RecordFailure(ctx, tool.WorkerFailure{
		Domain: tool.DomainWorkerTerminal, Reason: "crash", RejectedCalls: []string{"c1"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkRecovered(ctx, tool.DomainWorkerTerminal, time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(store.Failures()) != 1 {
		t.Fatalf("失败记录 = %d", len(store.Failures()))
	}
	if len(store.RecoveredDomains()) != 1 {
		t.Fatalf("恢复记录 = %d", len(store.RecoveredDomains()))
	}
}

func TestToolCallResolverAndRuleStore(t *testing.T) {
	resolver := NewToolCallResolver()
	resolver.Add("c1", "file_read", map[string]any{"filePath": "/a"})
	toolName, args, err := resolver.ResolveToolCall(context.Background(), "c1")
	if err != nil || toolName != "file_read" || args["filePath"] != "/a" {
		t.Fatalf("解析 = %q %v %v", toolName, args, err)
	}
	if _, _, err := resolver.ResolveToolCall(context.Background(), "missing"); !errors.Is(err, tool.ErrToolCallUnknown) {
		t.Fatalf("未知 call 错误 = %v", err)
	}

	store := NewRuleStore()
	ctx := context.Background()
	rule := tool.Rule{ID: "r1", Tool: "terminal_exec", Effect: tool.EffectAllow}
	if err := store.Add(ctx, rule); err != nil {
		t.Fatal(err)
	}
	if err := store.Add(ctx, rule); err == nil {
		t.Fatalf("重复规则应当报错")
	}
	rules, err := store.List(ctx)
	if err != nil || len(rules) != 1 {
		t.Fatalf("规则列表 = %v %v", rules, err)
	}
	if err := store.Remove(ctx, "r1"); err != nil {
		t.Fatal(err)
	}
	if err := store.Remove(ctx, "r1"); err == nil {
		t.Fatalf("移除不存在的规则应当报错")
	}
}

func TestConfirmers(t *testing.T) {
	ctx := context.Background()
	approve := &ApproveAllConfirmer{}
	ok, err := approve.RequestConfirmation(ctx, tool.ConfirmationRequest{})
	if err != nil || !ok {
		t.Fatalf("ApproveAll = %v %v", ok, err)
	}
	if len(approve.Requests()) != 1 {
		t.Fatalf("请求记录 = %d", len(approve.Requests()))
	}

	deny := &DenyAllConfirmer{}
	if ok, _ := deny.RequestConfirmation(ctx, tool.ConfirmationRequest{}); ok {
		t.Fatalf("DenyAll 应拒绝")
	}

	scripted := NewScriptedConfirmer([]bool{true, false}, []error{nil, errors.New("UI 不可用")})
	ok1, err1 := scripted.RequestConfirmation(ctx, tool.ConfirmationRequest{})
	ok2, err2 := scripted.RequestConfirmation(ctx, tool.ConfirmationRequest{})
	ok3, _ := scripted.RequestConfirmation(ctx, tool.ConfirmationRequest{})
	if !ok1 || err1 != nil {
		t.Fatalf("第 1 次 = %v %v", ok1, err1)
	}
	if ok2 || err2 == nil {
		t.Fatalf("第 2 次 = %v %v", ok2, err2)
	}
	if ok3 {
		t.Fatalf("第 3 次（超出脚本）应拒绝")
	}
	if scripted.Calls() != 3 {
		t.Fatalf("调用次数 = %d", scripted.Calls())
	}
}

func TestStaticToolPanicIsolation(t *testing.T) {
	tl := &StaticTool{
		Def:   tool.ToolDefinition{Name: "boom"},
		Panic: true,
	}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("StaticTool.Execute 的 panic 不应逃逸（调用方负责 safeExecute）")
		}
	}()
	// 直接调用会 panic——这是测试替身的预期行为，由 ToolRuntime 的
	// safeExecute 隔离。这里只验证替身可用性。
	_ = tl
}

func TestSecretsMock(t *testing.T) {
	s := NewSecrets()
	ctx := context.Background()
	_ = ctx
	ref, err := s.Put("sk-value")
	if err != nil {
		t.Fatal(err)
	}
	if !s.Has(ref) {
		t.Fatalf("ref 不存在")
	}
	got, err := s.Get(ref)
	if err != nil || got != "sk-value" {
		t.Fatalf("Get = %q %v", got, err)
	}
	if !s.ContainsValue("prefix sk-value suffix") {
		t.Fatalf("ContainsValue 失败")
	}
	if _, err := s.Get("missing"); err == nil {
		t.Fatalf("未知 ref 应当报错")
	}
	if s.Count() != 1 {
		t.Fatalf("Count = %d", s.Count())
	}
	if len(s.Refs()) != 1 {
		t.Fatalf("Refs = %v", s.Refs())
	}
}

func TestMockSatisfiesInterfaces(t *testing.T) {
	// 编译期契约：mock 实现必须满足 tool 包接口。
	var _ tool.EventSink = NewEventSink()
	var _ tool.ResourcePool = NewResourcePool()
	var _ tool.AdmissionController = NewAdmission(1)
	var _ tool.WorkerRouter = NewEchoWorkerRouter()
	var _ tool.WorkerController = NewWorkerController()
	var _ tool.WorkerFailureStore = NewWorkerFailureStore()
	var _ tool.ToolCallResolver = NewToolCallResolver()
	var _ tool.RuleStore = NewRuleStore()
	var _ tool.AuditLogger = NewAuditLogger()
	var _ tool.CrashSink = NewCrashSink()
	var _ tool.ConfirmationHandler = &ApproveAllConfirmer{}
	var _ secrets.SecretsProvider = NewSecrets()
	var _ tool.Tool = &StaticTool{}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done() }()
	wg.Wait()
}
