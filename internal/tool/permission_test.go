package tool

import (
	"context"
	"testing"
)

func newTestEngine(t *testing.T) PermissionEngine {
	t.Helper()
	engine, err := NewPermissionEngine(DefaultPermissionConfigs(), nil)
	if err != nil {
		t.Fatalf("创建权限引擎失败: %v", err)
	}
	return engine
}

func TestPermissionPriorityDenyOverAskOverAllow(t *testing.T) {
	configs := map[Mode]PermissionConfig{
		ModeCoding: {
			Allow:           []Rule{{ID: "a1", Tool: "t", Effect: EffectAllow}},
			Ask:             []Rule{{ID: "q1", Tool: "t", Effect: EffectAsk}},
			Deny:            []Rule{{ID: "d1", Tool: "t", Effect: EffectDeny}},
			DefaultDecision: EffectAllow,
		},
	}
	engine, err := NewPermissionEngine(configs, nil)
	if err != nil {
		t.Fatal(err)
	}
	decision := engine.Evaluate(context.Background(), PermissionRequest{Tool: "t", Mode: ModeCoding})
	if decision.Effect != EffectDeny {
		t.Fatalf("deny 应当优先，得到 %v（%s）", decision.Effect, decision.Reason)
	}
	if decision.RuleID != "d1" {
		t.Fatalf("RuleID = %q，期望 d1", decision.RuleID)
	}
	if decision.Reason == "" {
		t.Fatalf("Reason 不能为空（UI 需要解释）")
	}
}

func TestPermissionAskOverAllow(t *testing.T) {
	configs := map[Mode]PermissionConfig{
		ModeCoding: {
			Allow:           []Rule{{ID: "a1", Tool: "t", Effect: EffectAllow}},
			Ask:             []Rule{{ID: "q1", Tool: "t", Effect: EffectAsk}},
			DefaultDecision: EffectAllow,
		},
	}
	engine, err := NewPermissionEngine(configs, nil)
	if err != nil {
		t.Fatal(err)
	}
	decision := engine.Evaluate(context.Background(), PermissionRequest{Tool: "t", Mode: ModeCoding})
	if decision.Effect != EffectAsk || decision.RuleID != "q1" {
		t.Fatalf("ask 应当优先于 allow，得到 %v / %q", decision.Effect, decision.RuleID)
	}
}

func TestPermissionDefaultIsAsk(t *testing.T) {
	engine := newTestEngine(t)
	// 未命中任何规则的工具（coding 模式默认 ask）
	decision := engine.Evaluate(context.Background(), PermissionRequest{Tool: "unknown_tool", Mode: ModeCoding})
	if decision.Effect != EffectAsk {
		t.Fatalf("未命中规则应当默认 ask（fail-closed），得到 %v", decision.Effect)
	}
	if decision.Reason == "" {
		t.Fatalf("Reason 不能为空（UI 需要解释）")
	}
}

func TestPermissionDimensionMatching(t *testing.T) {
	configs := map[Mode]PermissionConfig{
		ModeCoding: {
			Deny: []Rule{
				{ID: "deny-ssh", Tool: "*", Path: "*/.ssh/*", Effect: EffectDeny},
				{ID: "deny-host", Tool: "web_fetch", Host: "*.evil.com", Effect: EffectDeny},
				{ID: "deny-port", Tool: "web_fetch", Port: "8080", Effect: EffectDeny},
				{ID: "deny-cmd", Tool: "terminal_exec", Command: "rm -rf *", Effect: EffectDeny},
			},
			Allow:           []Rule{{ID: "allow-all", Tool: "*", Effect: EffectAllow}},
			DefaultDecision: EffectAllow,
		},
	}
	engine, err := NewPermissionEngine(configs, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	cases := []struct {
		name string
		req  PermissionRequest
		want Effect
	}{
		{"ssh 路径拒绝", PermissionRequest{Tool: "file_read", Path: "/home/u/.ssh/id_rsa", Mode: ModeCoding}, EffectDeny},
		{"普通路径放行", PermissionRequest{Tool: "file_read", Path: "/home/u/code/main.go", Mode: ModeCoding}, EffectAllow},
		{"恶意主机拒绝", PermissionRequest{Tool: "web_fetch", Host: "api.evil.com", Mode: ModeCoding}, EffectDeny},
		{"正常主机放行", PermissionRequest{Tool: "web_fetch", Host: "api.good.com", Mode: ModeCoding}, EffectAllow},
		{"危险端口拒绝", PermissionRequest{Tool: "web_fetch", Port: 8080, Mode: ModeCoding}, EffectDeny},
		{"安全端口放行", PermissionRequest{Tool: "web_fetch", Port: 443, Mode: ModeCoding}, EffectAllow},
		{"危险命令拒绝", PermissionRequest{Tool: "terminal_exec", Command: "rm -rf /data", Mode: ModeCoding}, EffectDeny},
		{"普通命令放行", PermissionRequest{Tool: "terminal_exec", Command: "ls -la", Mode: ModeCoding}, EffectAllow},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := engine.Evaluate(ctx, c.req)
			if got.Effect != c.want {
				t.Fatalf("决策 = %v（%s），期望 %v", got.Effect, got.Reason, c.want)
			}
		})
	}
}

func TestPermissionSpecificityWins(t *testing.T) {
	configs := map[Mode]PermissionConfig{
		ModeCoding: {
			Allow: []Rule{
				{ID: "broad", Tool: "file_*", Effect: EffectAllow},
				{ID: "narrow", Tool: "file_read", Path: "/repo/*", Effect: EffectAllow, Description: "仓库内读取"},
			},
			DefaultDecision: EffectDeny,
		},
	}
	engine, err := NewPermissionEngine(configs, nil)
	if err != nil {
		t.Fatal(err)
	}
	decision := engine.Evaluate(context.Background(), PermissionRequest{
		Tool: "file_read", Path: "/repo/main.go", Mode: ModeCoding,
	})
	if decision.Effect != EffectAllow {
		t.Fatalf("决策 = %v", decision.Effect)
	}
	if decision.RuleID != "narrow" {
		t.Fatalf("RuleID = %q，期望更具体的 narrow", decision.RuleID)
	}
}

func TestPermissionRiskFloor(t *testing.T) {
	// yolo 模式默认 allow，但 critical 风险仍必须 ask（v2 硬化）。
	engine := newTestEngine(t)
	decision := engine.Evaluate(context.Background(), PermissionRequest{
		Tool: "act_ui", Risk: RiskCritical, Mode: ModeYolo,
	})
	if decision.Effect != EffectAsk {
		t.Fatalf("critical 风险在 YOLO 下也必须 ask，得到 %v（%s）", decision.Effect, decision.Reason)
	}
	// allow 规则显式 bypass 风险地板时可以放行
	configs := map[Mode]PermissionConfig{
		ModeYolo: {
			Allow:           []Rule{{ID: "yolo-act", Tool: "act_ui", Effect: EffectAllow, BypassRiskFloor: true}},
			DefaultDecision: EffectAllow,
		},
	}
	engine2, err := NewPermissionEngine(configs, nil)
	if err != nil {
		t.Fatal(err)
	}
	decision2 := engine2.Evaluate(context.Background(), PermissionRequest{Tool: "act_ui", Mode: ModeYolo})
	if decision2.Effect != EffectAllow {
		t.Fatalf("显式 bypass 后应当 allow，得到 %v（%s）", decision2.Effect, decision2.Reason)
	}
}

func TestPermissionConfirmationPromotesAskOnly(t *testing.T) {
	configs := map[Mode]PermissionConfig{
		ModeCoding: {
			Ask:             []Rule{{ID: "q1", Tool: "t", Effect: EffectAsk}},
			Deny:            []Rule{{ID: "d1", Tool: "bad", Effect: EffectDeny}},
			DefaultDecision: EffectAllow,
		},
	}
	engine, err := NewPermissionEngine(configs, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// 已确认的 ask -> allow
	got := engine.Evaluate(ctx, PermissionRequest{Tool: "t", Mode: ModeCoding, Confirmed: true})
	if got.Effect != EffectAllow {
		t.Fatalf("确认后应当 allow，得到 %v", got.Effect)
	}
	// 已确认的 deny 仍然是 deny（deny 绝对优先）
	got = engine.Evaluate(ctx, PermissionRequest{Tool: "bad", Mode: ModeCoding, Confirmed: true})
	if got.Effect != EffectDeny {
		t.Fatalf("确认不能覆盖 deny，得到 %v", got.Effect)
	}
}

func TestPermissionUserRules(t *testing.T) {
	store := &memoryRuleStore{}
	engine, err := NewPermissionEngine(DefaultPermissionConfigs(), store)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// terminal_exec 在 coding 模式默认 ask
	if got := engine.Evaluate(ctx, PermissionRequest{Tool: "terminal_exec", Command: "ls", Mode: ModeCoding}); got.Effect != EffectAsk {
		t.Fatalf("初始决策 = %v，期望 ask", got.Effect)
	}
	// 用户添加 allow 规则
	if err := engine.AddUserRule(ctx, Rule{ID: "user-ls", Tool: "terminal_exec", Command: "ls*", Effect: EffectAllow}); err != nil {
		t.Fatal(err)
	}
	if got := engine.Evaluate(ctx, PermissionRequest{Tool: "terminal_exec", Command: "ls -la", Mode: ModeCoding}); got.Effect != EffectAllow {
		t.Fatalf("用户规则生效后 = %v，期望 allow", got.Effect)
	}
	// 用户规则不能覆盖静态 deny
	if err := engine.AddUserRule(ctx, Rule{ID: "user-ssh", Tool: "file_read", Path: "*/.ssh/*", Effect: EffectAllow}); err != nil {
		t.Fatal(err)
	}
	if got := engine.Evaluate(ctx, PermissionRequest{Tool: "file_read", Path: "/h/.ssh/id_rsa", Mode: ModeCoding}); got.Effect != EffectDeny {
		t.Fatalf("用户规则不能覆盖静态 deny，得到 %v", got.Effect)
	}
	// 移除
	if err := engine.RemoveUserRule(ctx, "user-ls"); err != nil {
		t.Fatal(err)
	}
	if got := engine.Evaluate(ctx, PermissionRequest{Tool: "terminal_exec", Command: "ls -la", Mode: ModeCoding}); got.Effect != EffectAsk {
		t.Fatalf("移除后 = %v，期望回到 ask", got.Effect)
	}
}

func TestPermissionExplain(t *testing.T) {
	engine := newTestEngine(t)
	explanation := engine.Explain(context.Background(), PermissionRequest{
		Tool: "file_delete", Path: "/tmp/x.txt", Mode: ModeCoding, Risk: RiskHigh,
	})
	if explanation.Decision.Effect != EffectAsk {
		t.Fatalf("file_delete 在 coding 模式应当 ask，得到 %v", explanation.Decision.Effect)
	}
	if len(explanation.MatchedRules) == 0 {
		t.Fatalf("解释应包含命中规则")
	}
	if explanation.EffectiveRisk != RiskHigh {
		t.Fatalf("有效风险 = %v，期望 high", explanation.EffectiveRisk)
	}
}

func TestPermissionDefaultRiskTable(t *testing.T) {
	cases := map[string]RiskLevel{
		"file_read":     RiskLow,
		"file_write":    RiskMedium,
		"file_delete":   RiskHigh,
		"terminal_exec": RiskHigh,
		"act_ui":        RiskCritical,
		"unknown":       RiskMedium,
	}
	for toolName, want := range cases {
		if got := DefaultRisk(toolName, ""); got != want {
			t.Errorf("DefaultRisk(%q) = %v，期望 %v", toolName, got, want)
		}
	}
	// 动作级细化
	if got := DefaultRisk("knowledge", "search"); got != RiskLow {
		t.Errorf("knowledge:search = %v，期望 low", got)
	}
}

func TestPermissionRiskFloorAppliesToAllowRules(t *testing.T) {
	// 审核报告 S-4：风险地板必须对“命中 allow 规则”的请求也生效，
	// 否则 critical 工具只要有一条 allow 规则就永远自动放行。
	configs := map[Mode]PermissionConfig{
		ModeCoding: {
			Allow: []Rule{
				{ID: "plain-allow", Tool: "act_ui", Effect: EffectAllow},
				{ID: "low-allow", Tool: "file_read", Effect: EffectAllow},
				{ID: "bypass-allow", Tool: "computer_use", Effect: EffectAllow, BypassRiskFloor: true},
			},
			DefaultDecision: EffectAllow,
		},
	}
	engine, err := NewPermissionEngine(configs, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// 无 bypass 的 allow 规则 + critical 风险 → Ask
	got := engine.Evaluate(ctx, PermissionRequest{Tool: "act_ui", Mode: ModeCoding, Risk: RiskCritical})
	if got.Effect != EffectAsk {
		t.Fatalf("critical 风险命中 allow 规则必须升级为 Ask，得到 %v（%s）", got.Effect, got.Reason)
	}
	// 显式 bypass → Allow（用户在 UI 固定允许的高危操作）
	got = engine.Evaluate(ctx, PermissionRequest{Tool: "computer_use", Mode: ModeCoding, Risk: RiskCritical})
	if got.Effect != EffectAllow {
		t.Fatalf("BypassRiskFloor 规则应当放行，得到 %v（%s）", got.Effect, got.Reason)
	}
	// 低风险工具不受地板影响（有效风险取请求与工具默认风险的较大者）
	got = engine.Evaluate(ctx, PermissionRequest{Tool: "file_read", Mode: ModeCoding, Risk: RiskLow})
	if got.Effect != EffectAllow {
		t.Fatalf("低风险应当放行，得到 %v（%s）", got.Effect, got.Reason)
	}
}

func TestPermissionConfirmationScope(t *testing.T) {
	configs := map[Mode]PermissionConfig{
		ModeCoding: {
			Ask: []Rule{
				{ID: "ask-a", Tool: "tool_a", Effect: EffectAsk},
				{ID: "ask-b", Tool: "tool_b", Effect: EffectAsk},
			},
			DefaultDecision: EffectAllow,
		},
	}
	engine, err := NewPermissionEngine(configs, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// single 范围：确认只对本次调用生效，覆盖任意规则
	got := engine.Evaluate(ctx, PermissionRequest{
		Tool: "tool_a", Mode: ModeCoding,
		Confirmed: true, Scope: ScopeSingle,
	})
	if got.Effect != EffectAllow {
		t.Fatalf("single 确认应当放行: %v（%s）", got.Effect, got.Reason)
	}

	// session 范围：确认绑定在 ask-a，不能拿去放行 ask-b
	got = engine.Evaluate(ctx, PermissionRequest{
		Tool: "tool_b", Mode: ModeCoding,
		Confirmed: true, Scope: ScopeSession, ApprovedRuleID: "ask-a",
	})
	if got.Effect != EffectAsk {
		t.Fatalf("session 确认不能跨规则放行，得到 %v", got.Effect)
	}
	// session 范围且规则匹配 → 放行
	got = engine.Evaluate(ctx, PermissionRequest{
		Tool: "tool_a", Mode: ModeCoding,
		Confirmed: true, Scope: ScopeSession, ApprovedRuleID: "ask-a",
	})
	if got.Effect != EffectAllow {
		t.Fatalf("session 确认匹配规则应当放行: %v", got.Effect)
	}
	// session 范围但没带规则 ID → 不放行（防止空 ID 万能钥匙）
	got = engine.Evaluate(ctx, PermissionRequest{
		Tool: "tool_a", Mode: ModeCoding,
		Confirmed: true, Scope: ScopeSession,
	})
	if got.Effect != EffectAsk {
		t.Fatalf("session 确认缺少 ApprovedRuleID 应当不放行，得到 %v", got.Effect)
	}
}

func TestPermissionInvalidPortSpecRejected(t *testing.T) {
	_, err := NewPermissionEngine(map[Mode]PermissionConfig{
		ModeCoding: {Deny: []Rule{{ID: "bad", Tool: "t", Port: "not-a-port", Effect: EffectDeny}}},
	}, nil)
	if err == nil {
		t.Fatalf("非法端口规格应当在构造时被拒绝")
	}
}

// memoryRuleStore 是 RuleStore 的内存实现（mock 包的同名类型在测试里内联一份，
// 避免 tool 包测试依赖 mock 包造成反向依赖）。
type memoryRuleStore struct {
	rules []Rule
}

func (s *memoryRuleStore) List(ctx context.Context) ([]Rule, error) {
	return append([]Rule(nil), s.rules...), nil
}

func (s *memoryRuleStore) Add(ctx context.Context, rule Rule) error {
	for _, existing := range s.rules {
		if existing.ID == rule.ID {
			return errDuplicateRule
		}
	}
	s.rules = append(s.rules, rule)
	return nil
}

func (s *memoryRuleStore) Remove(ctx context.Context, ruleID string) error {
	for i, existing := range s.rules {
		if existing.ID == ruleID {
			s.rules = append(s.rules[:i], s.rules[i+1:]...)
			return nil
		}
	}
	return errRuleNotFound
}

var (
	errDuplicateRule = &ruleError{"规则已存在"}
	errRuleNotFound  = &ruleError{"规则不存在"}
)

type ruleError struct{ msg string }

func (e *ruleError) Error() string { return e.msg }
