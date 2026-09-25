package tool

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestToolPoliciesConsistent 是审核报告 S-3 的一致性断言：
// DefaultRiskTable（工具→风险）与 DefaultClassPolicy（工具→幂等分类）
// 是两张独立维护的表，任何一方新增工具而另一方忘了改，就会出现
// “高风险但被判为 A 类幂等可自动重试”这种危险组合。
// 这里把两条不变量固化成测试，漂移会在 CI 立刻暴露。
func TestToolPoliciesConsistent(t *testing.T) {
	policy := DefaultClassPolicy()
	for toolName, class := range policy.Tool {
		if class == ClassNonIdempotent {
			// C 类非幂等：绝不自动重复，风险必须至少 Medium
			if got := DefaultRisk(toolName, ""); got < RiskMedium {
				t.Errorf("工具 %q 被判为 C 类非幂等，但风险仅 %s（应 >= medium）", toolName, got)
			}
		}
		if class == ClassIdempotent {
			// A 类可自动重试：风险不得为 Critical
			if got := DefaultRisk(toolName, ""); got >= RiskCritical {
				t.Errorf("工具 %q 被判为 A 类可自动重试，但风险为 %s", toolName, got)
			}
		}
	}
	// 动作级分类同样受约束
	for key, class := range policy.ToolAction {
		idx := strings.Index(key, ":")
		if idx <= 0 {
			continue
		}
		toolName, action := key[:idx], key[idx+1:]
		if class == ClassNonIdempotent && DefaultRisk(toolName, action) < RiskMedium {
			t.Errorf("%q 被判为 C 类非幂等，但风险仅 %s", key, DefaultRisk(toolName, action))
		}
	}
}

func TestIdempotencyKeyDerivation(t *testing.T) {
	k1 := Key("run-1", "call-1")
	k2 := Key("run-1", "call-1")
	k3 := Key("run-1", "call-2")
	k4 := Key("run-2", "call-1")
	if k1 != k2 {
		t.Fatalf("同一 (run_id, tool_call_id) 必须得到同一 key")
	}
	if k1 == k3 || k1 == k4 {
		t.Fatalf("不同输入得到相同 key：%q / %q / %q", k1, k3, k4)
	}
	if len(k1) != 64 {
		t.Fatalf("key 长度 = %d，期望 64（sha256 hex）", len(k1))
	}
}

func TestIdempotencyThreeClasses(t *testing.T) {
	policy := DefaultClassPolicy()
	cases := []struct {
		tool, action string
		want         IdempotencyClass
	}{
		{"file_read", "", ClassIdempotent},
		{"web_fetch", "", ClassIdempotent},
		{"knowledge", "search", ClassIdempotent},
		{"knowledge", "list", ClassIdempotent},
		{"git_status", "", ClassDetectable},
		{"file_write", "", ClassDetectable},
		{"knowledge", "add", ClassDetectable},
		{"file_delete", "", ClassNonIdempotent},
		{"terminal_exec", "", ClassNonIdempotent},
		{"send_message", "", ClassNonIdempotent},
		{"knowledge", "delete", ClassNonIdempotent},
		{"git_operations", "push", ClassNonIdempotent},
		{"unknown_tool", "", ClassNonIdempotent},     // fail-safe
		{"browser_navigate", "", ClassNonIdempotent}, // 前缀通配
		{"mcp_call", "", ClassNonIdempotent},         // 前缀通配
	}
	for _, c := range cases {
		if got := policy.ClassifyTool(c.tool, c.action); got != c.want {
			t.Errorf("ClassifyTool(%q, %q) = %v，期望 %v", c.tool, c.action, got, c.want)
		}
	}
}

func TestIdempotencyClassifyViaResolver(t *testing.T) {
	resolver := NewMemoryResolver()
	resolver.Add("call-a", "file_read", nil)
	resolver.Add("call-b", "file_delete", nil)
	resolver.Add("call-c", "knowledge", map[string]any{"action": "search"})

	store := NewIdempotencyStore(NewMemoryIdempotencyDB(), resolver, nil, 5*time.Minute)
	ctx := context.Background()

	if class, err := store.Classify(ctx, "call-a"); err != nil || class != ClassIdempotent {
		t.Fatalf("call-a 分类 = %v, %v", class, err)
	}
	if class, err := store.Classify(ctx, "call-b"); err != nil || class != ClassNonIdempotent {
		t.Fatalf("call-b 分类 = %v, %v", class, err)
	}
	if class, err := store.Classify(ctx, "call-c"); err != nil || class != ClassIdempotent {
		t.Fatalf("call-c 分类 = %v, %v", class, err)
	}
	// 未知 call fail-safe 到非幂等
	if class, err := store.Classify(ctx, "call-missing"); err == nil || class != ClassNonIdempotent {
		t.Fatalf("未知 call 应当 fail-safe 到非幂等并报错，得到 %v, %v", class, err)
	}
}

func TestIdempotencyClaimIsAtomic(t *testing.T) {
	db := NewMemoryIdempotencyDB()
	store := NewIdempotencyStore(db, nil, nil, 5*time.Minute)
	ctx := context.Background()
	key := Key("run", "call")

	claimed, err := store.Claim(ctx, key)
	if err != nil || !claimed {
		t.Fatalf("首次认领应当成功: %v %v", claimed, err)
	}
	// I5：同一 key 不能被并行再次认领
	claimed2, err := store.Claim(ctx, key)
	if err != nil {
		t.Fatalf("二次认领不应当报错: %v", err)
	}
	if claimed2 {
		t.Fatalf("I5 被违反：同一 idempotency key 被认领了两次")
	}

	// 完成后可以再次认领（新的调用周期）
	if err := store.Complete(ctx, key, []byte(`{"ok":true}`)); err != nil {
		t.Fatalf("Complete 失败: %v", err)
	}
	// 重复提交必须被拒绝（I5：不产生第二个 durable result）
	if err := store.Complete(ctx, key, []byte(`{"ok":false}`)); err == nil {
		t.Fatalf("重复 Complete 应当被拒绝")
	}
	// done 之后重新认领允许（重放场景由上层决定）
	claimed3, err := store.Claim(ctx, key)
	if err != nil || !claimed3 {
		t.Fatalf("done 后重新认领应当成功: %v %v", claimed3, err)
	}
}

func TestIdempotencyGetReturnsResult(t *testing.T) {
	db := NewMemoryIdempotencyDB()
	store := NewIdempotencyStore(db, nil, nil, 5*time.Minute)
	ctx := context.Background()
	key := Key("run", "call")

	if _, _, found, _ := store.Get(ctx, key); found {
		t.Fatalf("未认领前不应当有记录")
	}
	if _, err := store.Claim(ctx, key); err != nil {
		t.Fatal(err)
	}
	status, _, found, _ := store.Get(ctx, key)
	if !found || status != StatusInflight {
		t.Fatalf("认领后状态 = %q found=%v", status, found)
	}
	payload := []byte(`{"content":"done"}`)
	if err := store.Complete(ctx, key, payload); err != nil {
		t.Fatal(err)
	}
	status, result, found, _ := store.Get(ctx, key)
	if !found || status != StatusDone || string(result) != string(payload) {
		t.Fatalf("完成后状态 = %q 结果 = %q", status, result)
	}
}

func TestIdempotencyRecoverInflightNonIdempotent(t *testing.T) {
	db := NewMemoryIdempotencyDB()
	fixed := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store := NewIdempotencyStore(db, nil, nil, time.Minute)
	store.Now = func() time.Time { return fixed }
	ctx := context.Background()
	key := Key("run", "call")

	if _, err := store.Claim(ctx, key); err != nil {
		t.Fatal(err)
	}
	// 认领仍然新鲜：不允许恢复
	if resume, err := store.RecoverInflight(ctx, key, ClassNonIdempotent); err != nil || resume {
		t.Fatalf("新鲜认领不应当允许恢复: %v %v", resume, err)
	}
	// 时间前进到认领过期
	store.Now = func() time.Time { return fixed.Add(2 * time.Minute) }
	// C 类：标记 needs_confirmation，绝不自动重复
	if resume, err := store.RecoverInflight(ctx, key, ClassNonIdempotent); err != nil || resume {
		t.Fatalf("C 类陈旧认领不应当放行重试: %v %v", resume, err)
	}
	status, _, _, _ := store.Get(ctx, key)
	if status != StatusNeedsConfirmation {
		t.Fatalf("C 类恢复后状态 = %q，期望 needs_confirmation", status)
	}
}

func TestIdempotencyRecoverInflightIdempotent(t *testing.T) {
	db := NewMemoryIdempotencyDB()
	fixed := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store := NewIdempotencyStore(db, nil, nil, time.Minute)
	store.Now = func() time.Time { return fixed }
	ctx := context.Background()
	key := Key("run", "call")

	if _, err := store.Claim(ctx, key); err != nil {
		t.Fatal(err)
	}
	store.Now = func() time.Time { return fixed.Add(2 * time.Minute) }
	// A 类：允许接管并重试
	if resume, err := store.RecoverInflight(ctx, key, ClassIdempotent); err != nil || !resume {
		t.Fatalf("A 类陈旧认领应当放行重试: %v %v", resume, err)
	}
	status, _, _, _ := store.Get(ctx, key)
	if status != StatusInflight {
		t.Fatalf("接管后状态 = %q，期望 inflight", status)
	}
}

func TestIdempotencyResultRoundTrip(t *testing.T) {
	resp := ToolResponse{
		ToolCallID: "call-1",
		ToolName:   "file_read",
		Content:    "file content",
		Success:    true,
		ErrorCode:  ErrOK,
		Metadata:   map[string]any{"lines": 3},
	}
	data, err := EncodeResult(resp)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeResult(data)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Content != resp.Content || !decoded.Success || decoded.ToolName != resp.ToolName {
		t.Fatalf("解码结果 = %+v", decoded)
	}
	if _, err := DecodeResult(nil); err == nil {
		t.Fatalf("空结果应当报错")
	}
}

func TestIdempotencySchemaIsExact(t *testing.T) {
	// 任务书第19章给出的 DDL 必须原样保留
	want := `CREATE TABLE tool_idempotency (
    key TEXT PRIMARY KEY,
    status TEXT NOT NULL,
    result BLOB,
    created_at INTEGER NOT NULL
);`
	if IdempotencySchema != want {
		t.Fatalf("schema 与任务书不一致:\n%s", IdempotencySchema)
	}
}

// NewMemoryResolver 创建内存 ToolCallResolver（与 mock 包等价，测试内联避免反向依赖）。
func NewMemoryResolver() *memoryResolver { return &memoryResolver{calls: map[string]resolvedCall{}} }

type resolvedCall struct {
	tool string
	args map[string]any
}

type memoryResolver struct {
	calls map[string]resolvedCall
}

func (r *memoryResolver) Add(toolCallID, toolName string, args map[string]any) {
	r.calls[toolCallID] = resolvedCall{tool: toolName, args: args}
}

func (r *memoryResolver) ResolveToolCall(ctx context.Context, toolCallID string) (string, map[string]any, error) {
	entry, ok := r.calls[toolCallID]
	if !ok {
		return "", nil, ErrToolCallUnknown
	}
	return entry.tool, entry.args, nil
}
