package expert

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/ximo888ok-netizen/ximo-agent/internal/provider"
)

// ---------------------------------------------------------------------------
// 注册表：embed.FS 懒加载 + 254 位专家
// ---------------------------------------------------------------------------

// TestRegistryLoadsAllExperts 确认 embed 的 254 位专家全部可加载。
//
// 这是「功能对等」的底线断言：v1 有 254 位专家，v2 不能少。
func TestRegistryLoadsAllExperts(t *testing.T) {
	r := NewRegistry(nil)

	count, err := r.Count()
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if count != 254 {
		t.Fatalf("专家数 = %d, want 254（v1 agents-raw.json 的数量）", count)
	}
}

// TestRegistryDataIntegrity 确认每位专家都有必要字段。
func TestRegistryDataIntegrity(t *testing.T) {
	r := NewRegistry(nil)
	list, err := r.Load()
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}

	seen := make(map[string]bool, len(list))
	for i, e := range list {
		if e.ID == "" {
			t.Errorf("第 %d 位专家缺少 ID", i)
		}
		if seen[e.ID] {
			t.Errorf("专家 ID 重复: %s", e.ID)
		}
		seen[e.ID] = true

		if e.Name == "" {
			t.Errorf("专家 %s 缺少 Name", e.ID)
		}
		if e.Division == "" {
			t.Errorf("专家 %s 缺少 Division", e.ID)
		}
		if e.Description == "" {
			t.Errorf("专家 %s 缺少 Description", e.ID)
		}
		if e.Personality == "" {
			t.Errorf("专家 %s 缺少 Personality", e.ID)
		}
	}
}

// TestRegistryDivisionCoverage 确认 v1 的 17 个部门都存在。
func TestRegistryDivisionCoverage(t *testing.T) {
	r := NewRegistry(nil)
	names, groups, err := r.GroupByDivision()
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}

	wantDivisions := []string{
		"academic", "design", "engineering", "finance", "game-development",
		"gis", "healthcare", "marketing", "paid-media", "product",
		"project-management", "sales", "security", "spatial-computing",
		"specialized", "support", "testing",
	}
	if len(names) != len(wantDivisions) {
		t.Fatalf("部门数 = %d, want %d: %v", len(names), len(wantDivisions), names)
	}
	for _, want := range wantDivisions {
		if _, ok := groups[want]; !ok {
			t.Errorf("缺少部门 %q", want)
		}
	}
}

// TestRegistryGetByID 确认按 ID 查找。
func TestRegistryGetByID(t *testing.T) {
	r := NewRegistry(nil)
	list, err := r.Load()
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}

	target := list[0]
	got, ok := r.Get(target.ID)
	if !ok {
		t.Fatalf("应能查到 %s", target.ID)
	}
	if got.Name != target.Name {
		t.Fatalf("Name = %q, want %q", got.Name, target.Name)
	}

	if _, ok := r.Get("definitely-not-an-expert"); ok {
		t.Fatal("不存在的 ID 不应命中")
	}
}

// TestRegistryListByDivision 确认按部门筛选。
func TestRegistryListByDivision(t *testing.T) {
	r := NewRegistry(nil)
	eng, err := r.ListByDivision("engineering")
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if len(eng) == 0 {
		t.Fatal("engineering 部门应有专家")
	}
	for _, e := range eng {
		if e.Division != "engineering" {
			t.Fatalf("筛选结果含其他部门: %s (%s)", e.ID, e.Division)
		}
	}
}

// TestRegistryLazyLoad 确认首次访问才解析 JSON，且只解析一次。
func TestRegistryLazyLoad(t *testing.T) {
	r := NewRegistry(nil)

	// 未访问前 builtin 为空。
	r.mu.RLock()
	before := len(r.builtin)
	r.mu.RUnlock()
	if before != 0 {
		t.Fatal("构造后不应已加载（懒加载）")
	}

	if _, err := r.Load(); err != nil {
		t.Fatalf("加载失败: %v", err)
	}

	r.mu.RLock()
	after := len(r.builtin)
	r.mu.RUnlock()
	if after == 0 {
		t.Fatal("访问后应已加载")
	}
}

// TestRegistryCustomOverridesBuiltin 确认自定义专家同 ID 覆盖内置（v1 合并语义）。
func TestRegistryCustomOverridesBuiltin(t *testing.T) {
	store := newFakeCustomStore()
	r := NewRegistry(store)

	list, _ := r.Load()
	target := list[0]

	// 用同 ID 保存一个自定义版本。
	store.save(Expert{
		ID:          target.ID,
		Division:    target.Division,
		Name:        "被覆盖的名字",
		Description: "自定义描述",
		Personality: "自定义人格",
		Vibe:        "自定义风格",
	})

	got, ok := r.Get(target.ID)
	if !ok {
		t.Fatal("应能查到")
	}
	if got.Name != "被覆盖的名字" {
		t.Fatalf("Name = %q，自定义应覆盖内置", got.Name)
	}
	if !got.Custom {
		t.Fatal("应标记为自定义")
	}

	// 总数不应因覆盖而增加。
	count, _ := r.Count()
	if count != 254 {
		t.Fatalf("覆盖不应改变总数，实际 %d", count)
	}
}

// TestRegistryCustomStoreFailureDegrades 确认自定义存储失败时内置仍可用。
func TestRegistryCustomStoreFailureDegrades(t *testing.T) {
	r := NewRegistry(failingCustomStore{})

	count, err := r.Count()
	if err != nil {
		t.Fatalf("自定义存储失败不应让内置不可用: %v", err)
	}
	if count != 254 {
		t.Fatalf("应仍有 254 位内置专家，实际 %d", count)
	}
}

// TestRegistryConcurrentLoad 确认并发加载安全（配合 -race）。
func TestRegistryConcurrentLoad(t *testing.T) {
	r := NewRegistry(nil)

	var wg sync.WaitGroup
	results := make([]int, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			n, err := r.Count()
			if err != nil {
				results[i] = -1
				return
			}
			results[i] = n
		}(i)
	}
	wg.Wait()

	for i, n := range results {
		if n != 254 {
			t.Fatalf("goroutine %d 得到 %d，want 254", i, n)
		}
	}
}

// ---------------------------------------------------------------------------
// 部门映射与提示词（v1 expert-config.ts 对等）
// ---------------------------------------------------------------------------

// TestDivisionToolsCoverAllDivisions 确认 v1 的 17 个部门都有工具映射。
func TestDivisionToolsCoverAllDivisions(t *testing.T) {
	r := NewRegistry(nil)
	names, _, _ := r.GroupByDivision()

	for _, div := range names {
		if _, ok := DivisionTools[div]; !ok {
			t.Errorf("部门 %q 缺少工具映射（专家将拿不到工具）", div)
		}
		if _, ok := DivisionWorkflows[div]; !ok {
			t.Errorf("部门 %q 缺少预设工作流", div)
		}
	}
}

// TestKeywordToolRulesCoverV1 确认 v1 的 12 条关键词规则都在。
func TestKeywordToolRulesCoverV1(t *testing.T) {
	if len(KeywordToolRules) != 12 {
		t.Fatalf("关键词规则数 = %d, want 12（v1 KEYWORD_TOOL_RULES）", len(KeywordToolRules))
	}
	for i, rule := range KeywordToolRules {
		if len(rule.Keywords) == 0 {
			t.Errorf("规则 %d 缺少关键词", i)
		}
		if len(rule.Tools) == 0 {
			t.Errorf("规则 %d 缺少工具", i)
		}
	}
}

// TestAnalyzeExpertUsesDivisionTools 确认以部门基础工具集为起点。
func TestAnalyzeExpertUsesDivisionTools(t *testing.T) {
	e := Expert{
		ID:          "x",
		Division:    "engineering",
		Description: "普通描述无关键词",
		Personality: "人格",
		Vibe:        "风格",
	}
	got := AnalyzeExpert(e)

	// engineering 的基础工具集里的每一项都应出现。
	for _, want := range DivisionTools["engineering"] {
		if !containsStr(got.Tools, want) {
			t.Errorf("缺少部门基础工具 %q", want)
		}
	}
	if got.Workflow == "" {
		t.Fatal("应有预设工作流")
	}
}

// TestAnalyzeExpertAddsKeywordTools 确认关键词能叠加额外工具。
func TestAnalyzeExpertAddsKeywordTools(t *testing.T) {
	base := Expert{
		ID:          "x",
		Division:    "academic", // 基础工具集不含 code_execute
		Description: "普通描述",
		Personality: "人格",
		Vibe:        "风格",
	}
	baseTools := AnalyzeExpert(base).Tools
	if containsStr(baseTools, "code_execute") {
		t.Skip("测试前提不成立：academic 基础集已含 code_execute")
	}

	// 加入「代码」关键词 → 应叠加编程相关工具。
	withKeyword := base
	withKeyword.Description = "擅长编写代码与开发"
	got := AnalyzeExpert(withKeyword)
	if !containsStr(got.Tools, "code_execute") {
		t.Error("关键词「代码」应叠加 code_execute")
	}
}

// TestAnalyzeExpertIncludesOwnTools 确认专家自带 tools 字段被纳入。
func TestAnalyzeExpertIncludesOwnTools(t *testing.T) {
	e := Expert{
		ID:          "x",
		Division:    "specialized",
		Description: "描述",
		Personality: "人格",
		Vibe:        "风格",
		Tools:       []string{"custom_tool_xyz"},
	}
	got := AnalyzeExpert(e)
	if !containsStr(got.Tools, "custom_tool_xyz") {
		t.Fatal("专家自带的 tools 应被纳入")
	}
}

// TestAnalyzeExpertToolsAreSorted 确认工具列表有序（保证 prompt cache 前缀稳定）。
func TestAnalyzeExpertToolsAreSorted(t *testing.T) {
	e := Expert{
		ID:          "x",
		Division:    "engineering",
		Description: "代码开发",
		Personality: "人格",
		Vibe:        "风格",
	}
	tools := AnalyzeExpert(e).Tools
	for i := 1; i < len(tools); i++ {
		if tools[i] < tools[i-1] {
			t.Fatalf("工具列表未排序: [%d]=%q < [%d]=%q", i, tools[i], i-1, tools[i-1])
		}
	}
}

// TestBuildSystemPromptFormat 确认提示词格式（sub-agent 反解依赖它）。
func TestBuildSystemPromptFormat(t *testing.T) {
	e := Expert{
		ID:          "engineering-frontend-developer",
		Division:    "engineering",
		Name:        "前端架构师",
		Emoji:       "🎨",
		Description: "精通前端架构",
		Personality: "严谨细致",
		Vibe:        "注重代码质量",
	}
	prompt := BuildSystemPrompt(e)

	for _, want := range []string{"你现在扮演 **前端架构师**（🎨）", "严谨细致", "精通前端架构", "注重代码质量", "## 你的核心能力", "## 输出要求"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("系统提示词缺少片段 %q", want)
		}
	}
}

// TestParseExpertIdentityRoundTrip 确认从提示词反解身份能与构建对称。
func TestParseExpertIdentityRoundTrip(t *testing.T) {
	e := Expert{
		ID:          "x",
		Division:    "design",
		Name:        "设计师",
		Emoji:       "✨",
		Description: "描述",
		Personality: "人格",
		Vibe:        "风格",
	}
	prompt := BuildSystemPrompt(e)

	name, emoji := ParseExpertIdentity(prompt)
	if name != "设计师" {
		t.Errorf("name = %q, want 设计师", name)
	}
	if emoji != "✨" {
		t.Errorf("emoji = %q, want ✨", emoji)
	}

	// 无效输入应降级而非 panic。
	n2, e2 := ParseExpertIdentity("完全无关的文本")
	if n2 != "unknown-expert" || e2 != "🧠" {
		t.Fatalf("无效输入应降级，实际 %q/%q", n2, e2)
	}
}

// ---------------------------------------------------------------------------
// 子 Agent 与两阶段编排
// ---------------------------------------------------------------------------

// fakeProvider 按预设脚本返回响应。
type fakeProvider struct {
	mu        sync.Mutex
	responses []provider.CompletionResponse
	calls     []provider.CompletionRequest
}

func (f *fakeProvider) Complete(_ context.Context, req provider.CompletionRequest) (provider.CompletionResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, req)
	if len(f.responses) == 0 {
		return provider.CompletionResponse{FinishReason: provider.FinishStop, Content: "默认回答"}, nil
	}
	resp := f.responses[0]
	if len(f.responses) > 1 {
		f.responses = f.responses[1:]
	}
	return resp, nil
}

func (f *fakeProvider) Stream(context.Context, provider.CompletionRequest) (<-chan provider.StreamChunk, error) {
	ch := make(chan provider.StreamChunk, 1)
	ch <- provider.StreamChunk{Done: true}
	close(ch)
	return ch, nil
}

func (f *fakeProvider) Name() string         { return "fake" }
func (f *fakeProvider) ContextWindow() int   { return 100000 }
func (f *fakeProvider) MaxOutputTokens() int { return 8192 }

func (f *fakeProvider) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeProvider) lastRequest() provider.CompletionRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return provider.CompletionRequest{}
	}
	return f.calls[len(f.calls)-1]
}

// fakeExecutor 记录工具调用并返回固定结果。
type fakeExecutor struct {
	mu      sync.Mutex
	calls   []ToolCallRequest
	results map[string]ToolCallResult
}

func (f *fakeExecutor) Execute(_ context.Context, req ToolCallRequest) (ToolCallResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, req)
	if r, ok := f.results[req.Name]; ok {
		return r, nil
	}
	return ToolCallResult{Content: "工具执行成功", Success: true}, nil
}

func (f *fakeExecutor) Available(name string) bool { return true }

func (f *fakeExecutor) callNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		out = append(out, c.Name)
	}
	return out
}

// TestSubAgentReturnsFinalAnswer 确认无工具调用时直接返回回答。
func TestSubAgentReturnsFinalAnswer(t *testing.T) {
	p := &fakeProvider{responses: []provider.CompletionResponse{
		{FinishReason: provider.FinishStop, Content: "最终回答"},
	}}

	res, err := RunSubAgent(context.Background(), SubAgentRequest{
		SystemPrompt: "你是专家",
		Task:         "做个任务",
		Options:      SubAgentOptions{Provider: p, Model: "m"},
	})
	if err != nil {
		t.Fatalf("报错: %v", err)
	}
	if res.Content != "最终回答" {
		t.Fatalf("Content = %q", res.Content)
	}
	if res.Rounds != 1 {
		t.Fatalf("Rounds = %d, want 1", res.Rounds)
	}
}

// TestSubAgentExecutesToolCallsThenAnswers 确认多轮工具调用后给出最终回答。
func TestSubAgentExecutesToolCallsThenAnswers(t *testing.T) {
	p := &fakeProvider{responses: []provider.CompletionResponse{
		{
			FinishReason: provider.FinishToolCalls,
			ToolCalls: []provider.ToolCall{
				{ID: "c1", Name: "file_read", Arguments: `{"path":"a.txt"}`},
			},
		},
		{FinishReason: provider.FinishStop, Content: "读完了"},
	}}
	exec := &fakeExecutor{}

	res, err := RunSubAgent(context.Background(), SubAgentRequest{
		SystemPrompt: "你是专家",
		Task:         "读文件",
		Options:      SubAgentOptions{Provider: p, Executor: exec, Model: "m", ToolNames: []string{"file_read"}},
	})
	if err != nil {
		t.Fatalf("报错: %v", err)
	}
	if res.Content != "读完了" {
		t.Fatalf("Content = %q", res.Content)
	}
	if names := exec.callNames(); len(names) != 1 || names[0] != "file_read" {
		t.Fatalf("工具调用 = %v, want [file_read]", names)
	}
	if res.Rounds != 2 {
		t.Fatalf("Rounds = %d, want 2", res.Rounds)
	}
}

// TestSubAgentPreservesToolPairing 确认 assistant(tool_calls) 后有对应 tool 响应。
//
// 配对不完整会让下一轮 API 请求 400 —— 这是子 Agent 最容易出的错。
func TestSubAgentPreservesToolPairing(t *testing.T) {
	p := &fakeProvider{responses: []provider.CompletionResponse{
		{
			FinishReason: provider.FinishToolCalls,
			ToolCalls: []provider.ToolCall{
				{ID: "c1", Name: "file_read", Arguments: "{}"},
				{ID: "c2", Name: "file_write", Arguments: "{}"},
			},
		},
		{FinishReason: provider.FinishStop, Content: "done"},
	}}

	_, err := RunSubAgent(context.Background(), SubAgentRequest{
		SystemPrompt: "你是专家",
		Task:         "任务",
		Options: SubAgentOptions{
			Provider: p, Executor: &fakeExecutor{}, Model: "m",
			ToolNames: []string{"file_read", "file_write"},
		},
	})
	if err != nil {
		t.Fatalf("报错: %v", err)
	}

	// 检查第二次请求的消息配对完整性。
	req := p.lastRequest()
	pending := map[string]bool{}
	for _, m := range req.Messages {
		if m.Role == provider.RoleAssistant {
			for _, tc := range m.ToolCalls {
				pending[tc.ID] = true
			}
		}
		if m.Role == provider.RoleTool {
			if !pending[m.ToolCallID] {
				t.Fatalf("tool 消息 %q 无对应 assistant 调用（配对破坏）", m.ToolCallID)
			}
			delete(pending, m.ToolCallID)
		}
	}
	if len(pending) != 0 {
		t.Fatalf("有 %d 个工具调用缺少响应", len(pending))
	}
}

// TestSubAgentMalformedArgumentsHandled 确认参数非法 JSON 时不执行工具、回传错误。
func TestSubAgentMalformedArgumentsHandled(t *testing.T) {
	p := &fakeProvider{responses: []provider.CompletionResponse{
		{
			FinishReason: provider.FinishToolCalls,
			ToolCalls:    []provider.ToolCall{{ID: "c1", Name: "file_read", Arguments: "{not json"}},
		},
		{FinishReason: provider.FinishStop, Content: "修正后回答"},
	}}
	exec := &fakeExecutor{}

	_, err := RunSubAgent(context.Background(), SubAgentRequest{
		SystemPrompt: "你是专家", Task: "任务",
		Options: SubAgentOptions{Provider: p, Executor: exec, Model: "m", ToolNames: []string{"file_read"}},
	})
	if err != nil {
		t.Fatalf("报错: %v", err)
	}
	if len(exec.calls) != 0 {
		t.Fatal("参数非法时不应执行工具")
	}
}

// TestSubAgentUnknownToolNotInSchema 确认未注册工具不进 schema。
func TestSubAgentUnknownToolNotInSchema(t *testing.T) {
	p := &fakeProvider{responses: []provider.CompletionResponse{
		{FinishReason: provider.FinishStop, Content: "ok"},
	}}
	exec := &restrictedExecutor{available: map[string]bool{"file_read": true}}

	_, err := RunSubAgent(context.Background(), SubAgentRequest{
		SystemPrompt: "你是专家", Task: "任务",
		Options: SubAgentOptions{
			Provider: p, Executor: exec, Model: "m",
			ToolNames: []string{"file_read", "nonexistent_tool"},
		},
	})
	if err != nil {
		t.Fatalf("报错: %v", err)
	}

	schema := p.lastRequest().Tools
	if len(schema) != 1 || schema[0].Name != "file_read" {
		names := make([]string, len(schema))
		for i, s := range schema {
			names[i] = s.Name
		}
		t.Fatalf("schema 应只含可用工具 [file_read]，实际 %v", names)
	}
}

// TestSubAgentEmitsWorkEvents 确认工作过程事件被推送（前端 ExpertWorkCard 需要）。
func TestSubAgentEmitsWorkEvents(t *testing.T) {
	p := &fakeProvider{responses: []provider.CompletionResponse{
		{
			FinishReason: provider.FinishToolCalls,
			ToolCalls:    []provider.ToolCall{{ID: "c1", Name: "file_read", Arguments: "{}"}},
		},
		{FinishReason: provider.FinishStop, Content: "done"},
	}}

	var events []WorkEvent
	_, err := RunSubAgent(context.Background(), SubAgentRequest{
		SystemPrompt: "你是专家", Task: "任务",
		Options: SubAgentOptions{
			Provider: p, Executor: &fakeExecutor{}, Model: "m",
			ToolNames: []string{"file_read"},
			ExpertID:  "test-expert", ExpertName: "测试专家",
			OnEvent: func(ev WorkEvent) { events = append(events, ev) },
		},
	})
	if err != nil {
		t.Fatalf("报错: %v", err)
	}

	stages := make(map[Stage]int)
	for _, ev := range events {
		stages[ev.Stage]++
		if ev.ExpertID != "test-expert" {
			t.Errorf("事件缺少专家 ID: %+v", ev)
		}
	}
	if stages[StageStarted] != 1 {
		t.Error("应有 1 个 started 事件")
	}
	if stages[StageTool] == 0 {
		t.Error("应有 tool 事件")
	}
	if stages[StageToolResult] == 0 {
		t.Error("应有 toolResult 事件")
	}
	if stages[StageFinished] != 1 {
		t.Error("应有 1 个 finished 事件")
	}
}

// TestSubAgentMaxRoundsRequestsSummary 确认达轮次上限后请求最终总结（v1 兜底）。
func TestSubAgentMaxRoundsRequestsSummary(t *testing.T) {
	// 始终返回 tool_calls，迫使达到轮次上限。
	toolResp := provider.CompletionResponse{
		FinishReason: provider.FinishToolCalls,
		ToolCalls:    []provider.ToolCall{{ID: "c", Name: "file_read", Arguments: "{}"}},
	}
	p := &fakeProvider{responses: []provider.CompletionResponse{
		toolResp, toolResp, // 2 轮
		{FinishReason: provider.FinishStop, Content: "总结回答"},
	}}

	res, err := RunSubAgent(context.Background(), SubAgentRequest{
		SystemPrompt: "你是专家", Task: "任务",
		Options: SubAgentOptions{
			Provider: p, Executor: &fakeExecutor{}, Model: "m",
			ToolNames: []string{"file_read"}, MaxRounds: 2,
		},
	})
	if err != nil {
		t.Fatalf("报错: %v", err)
	}
	if res.Content != "总结回答" {
		t.Fatalf("Content = %q, want 总结回答", res.Content)
	}
	// 最后一轮请求应包含「不要再调用工具」的指令。
	req := p.lastRequest()
	found := false
	for _, m := range req.Messages {
		if strings.Contains(m.Content, "不要再调用任何工具") {
			found = true
		}
	}
	if !found {
		t.Fatal("达上限后应追加「不要再调用工具」的指令")
	}
	// 总结轮不应再带 tools。
	if len(req.Tools) != 0 {
		t.Fatal("总结轮不应带工具 schema")
	}
}

// TestSubAgentRejectsNilProvider 确认缺少 Provider 时报错而非 panic。
func TestSubAgentRejectsNilProvider(t *testing.T) {
	_, err := RunSubAgent(context.Background(), SubAgentRequest{
		SystemPrompt: "s", Task: "t", Options: SubAgentOptions{},
	})
	if err == nil {
		t.Fatal("缺少 Provider 应报错")
	}
}

// TestSubAgentTruncatesHugeToolResult 确认超长工具结果被截断（防上下文溢出）。
func TestSubAgentTruncatesHugeToolResult(t *testing.T) {
	p := &fakeProvider{responses: []provider.CompletionResponse{
		{
			FinishReason: provider.FinishToolCalls,
			ToolCalls:    []provider.ToolCall{{ID: "c1", Name: "file_read", Arguments: "{}"}},
		},
		{FinishReason: provider.FinishStop, Content: "done"},
	}}
	exec := &fakeExecutor{results: map[string]ToolCallResult{
		"file_read": {Content: strings.Repeat("x", MaxSubToolResult*2), Success: true},
	}}

	_, err := RunSubAgent(context.Background(), SubAgentRequest{
		SystemPrompt: "你是专家", Task: "任务",
		Options: SubAgentOptions{Provider: p, Executor: exec, Model: "m", ToolNames: []string{"file_read"}},
	})
	if err != nil {
		t.Fatalf("报错: %v", err)
	}

	req := p.lastRequest()
	for _, m := range req.Messages {
		if m.Role == provider.RoleTool {
			if len(m.Content) > MaxSubToolResult+100 {
				t.Fatalf("工具结果长度 %d 未被截断", len(m.Content))
			}
			if !strings.Contains(m.Content, "已截断") {
				t.Fatal("应带截断标记")
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Orchestrator 两阶段编排
// ---------------------------------------------------------------------------

// TestOrchestratorActivateWithoutTaskReturnsInfo 确认无 task 时只返回专家信息。
func TestOrchestratorActivateWithoutTaskReturnsInfo(t *testing.T) {
	o := NewOrchestrator(OrchestratorOptions{Registry: NewRegistry(nil)})
	list, _ := o.Registry().Load()

	out, err := o.Activate(context.Background(), ExpertRequest{ExpertID: list[0].ID})
	if err != nil {
		t.Fatalf("报错: %v", err)
	}
	if out.SubAgentMode {
		t.Fatal("无 task 不应跑子 Agent")
	}
	if out.System == "" {
		t.Fatal("应返回系统提示词")
	}
	if len(out.Analysis.Tools) == 0 {
		t.Fatal("应返回推荐工具")
	}
	for _, want := range []string{"已激活专家", "提示词分析结果", "推荐工具配置", "预设工作流"} {
		if !strings.Contains(out.Content, want) {
			t.Errorf("信息型返回缺少片段 %q", want)
		}
	}
}

// TestOrchestratorUnknownExpert 确认未知专家返回错误。
func TestOrchestratorUnknownExpert(t *testing.T) {
	o := NewOrchestrator(OrchestratorOptions{Registry: NewRegistry(nil)})
	_, err := o.Activate(context.Background(), ExpertRequest{ExpertID: "no-such-expert"})
	if err == nil {
		t.Fatal("未知专家应报错")
	}
}

// TestTwoPhaseOrchestration 确认「方案设计 → 有序实施」两阶段协议。
func TestTwoPhaseOrchestration(t *testing.T) {
	plan := "## 方案\n1. 用 file_read 读取配置文件\n2. 用 file_write 写入结果"
	p := &fakeProvider{responses: []provider.CompletionResponse{
		{FinishReason: provider.FinishStop, Content: plan},   // Plan 阶段
		{FinishReason: provider.FinishStop, Content: "实施完成"}, // Execute 阶段
	}}

	var phases []PhaseEvent
	o := NewOrchestrator(OrchestratorOptions{
		Registry:       NewRegistry(nil),
		Runner:         SubAgentOptions{Provider: p, Executor: &fakeExecutor{}, Model: "m"},
		EnableTwoPhase: true,
		OnPhase:        func(ev PhaseEvent) { phases = append(phases, ev) },
	})

	list, _ := o.Registry().Load()
	out, err := o.Activate(context.Background(), ExpertRequest{
		ExpertID: list[0].ID, Task: "处理配置",
	})
	if err != nil {
		t.Fatalf("报错: %v", err)
	}

	if out.Plan == "" {
		t.Fatal("应产出方案")
	}
	if !strings.Contains(out.Plan, "方案") {
		t.Fatalf("方案内容异常: %q", out.Plan)
	}
	if len(phases) != 2 {
		t.Fatalf("应有 2 个阶段事件，实际 %d: %+v", len(phases), phases)
	}
	if phases[0].Phase != PhasePlan {
		t.Fatalf("第一个阶段应为 plan，实际 %s", phases[0].Phase)
	}
	if phases[1].Phase != PhaseExecute {
		t.Fatalf("第二个阶段应为 execute，实际 %s", phases[1].Phase)
	}
	if !out.SubAgentMode {
		t.Fatal("应跑子 Agent")
	}
}

// TestPlanPhaseHasNoTools 确认规划阶段不传工具（强制只做规划）。
func TestPlanPhaseHasNoTools(t *testing.T) {
	p := &fakeProvider{responses: []provider.CompletionResponse{
		{FinishReason: provider.FinishStop, Content: "## 方案\n1. 步骤"},
		{FinishReason: provider.FinishStop, Content: "done"},
	}}

	o := NewOrchestrator(OrchestratorOptions{
		Registry:       NewRegistry(nil),
		Runner:         SubAgentOptions{Provider: p, Executor: &fakeExecutor{}, Model: "m"},
		EnableTwoPhase: true,
	})

	list, _ := o.Registry().Load()
	if _, err := o.Activate(context.Background(), ExpertRequest{ExpertID: list[0].ID, Task: "任务"}); err != nil {
		t.Fatalf("报错: %v", err)
	}

	// 第一次调用（Plan）不应带工具。
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.calls) == 0 {
		t.Fatal("应有调用记录")
	}
	if len(p.calls[0].Tools) != 0 {
		t.Fatalf("规划阶段不应带工具，实际 %d 个", len(p.calls[0].Tools))
	}
}

// TestPlanPhaseFailureDegrades 确认 Plan 阶段失败时降级为单阶段（不阻断）。
func TestPlanPhaseFailureDegrades(t *testing.T) {
	p := &fakeProvider{
		responses: []provider.CompletionResponse{
			{FinishReason: provider.FinishStop, Content: ""}, // Plan 返回空
			{FinishReason: provider.FinishStop, Content: "实施完成"},
		},
	}

	o := NewOrchestrator(OrchestratorOptions{
		Registry:       NewRegistry(nil),
		Runner:         SubAgentOptions{Provider: p, Executor: &fakeExecutor{}, Model: "m"},
		EnableTwoPhase: true,
	})

	list, _ := o.Registry().Load()
	out, err := o.Activate(context.Background(), ExpertRequest{ExpertID: list[0].ID, Task: "任务"})
	if err != nil {
		t.Fatalf("Plan 失败不应向上抛错: %v", err)
	}
	if out.Plan != "" {
		t.Fatal("Plan 为空时 out.Plan 应为空")
	}
	if !out.SubAgentMode {
		t.Fatal("实施阶段应仍然执行")
	}
}

// TestDeviationDetection 确认偏差检测能识别方案外的工具调用。
func TestDeviationDetection(t *testing.T) {
	plan := "## 方案\n1. 用 file_read 读取"
	events := []WorkEvent{
		{Stage: StageTool, Detail: "调用工具 file_read"},     // 方案内
		{Stage: StageTool, Detail: "调用工具 terminal_exec"}, // 方案外 → 偏差
		{Stage: StageMessage, Detail: "无关阶段"},            // 非 tool 阶段，忽略
	}

	got := countDeviations(plan, events)
	if got != 1 {
		t.Fatalf("偏差数 = %d, want 1", got)
	}
}

// TestOrchestratorSubAgentFailureReturnsFallback 确认子 Agent 失败时降级为手动指引。
func TestOrchestratorSubAgentFailureReturnsFallback(t *testing.T) {
	p := &failingProvider{}

	o := NewOrchestrator(OrchestratorOptions{
		Registry: NewRegistry(nil),
		Runner:   SubAgentOptions{Provider: p, Executor: &fakeExecutor{}, Model: "m"},
	})

	list, _ := o.Registry().Load()
	out, err := o.Activate(context.Background(), ExpertRequest{ExpertID: list[0].ID, Task: "任务"})
	if err != nil {
		t.Fatalf("失败应降级而非抛错: %v", err)
	}
	if out.SubAgentMode {
		t.Fatal("失败时不应标记为子 Agent 模式")
	}
	if out.Error == "" {
		t.Fatal("应记录错误原因")
	}
	for _, want := range []string{"子 Agent 调用失败", "专家信息", "推荐工具配置", "系统提示词"} {
		if !strings.Contains(out.Content, want) {
			t.Errorf("降级内容缺少片段 %q", want)
		}
	}
}

// TestOrchestratorListAndSearch 确认 list / search 分支可用。
func TestOrchestratorListAndSearch(t *testing.T) {
	o := NewOrchestrator(OrchestratorOptions{Registry: NewRegistry(nil)})

	listText, err := o.List("")
	if err != nil {
		t.Fatalf("List 报错: %v", err)
	}
	if !strings.Contains(listText, "254") {
		t.Errorf("列表应包含专家总数 254: %s", truncate(listText, 200))
	}

	engText, err := o.List("engineering")
	if err != nil {
		t.Fatalf("List 报错: %v", err)
	}
	if !strings.Contains(engText, "engineering") {
		t.Error("按部门筛选结果应含部门名")
	}

	searchText, err := o.Search("前端")
	if err != nil {
		t.Fatalf("Search 报错: %v", err)
	}
	if !strings.Contains(searchText, "搜索") {
		t.Errorf("搜索结果格式异常: %s", truncate(searchText, 200))
	}
}

// TestOrchestratorSearchNoMatch 确认无结果时的提示（而非报错）。
func TestOrchestratorSearchNoMatch(t *testing.T) {
	o := NewOrchestrator(OrchestratorOptions{Registry: NewRegistry(nil)})
	text, err := o.Search("zzzz绝不可能匹配zzzz")
	if err != nil {
		t.Fatalf("报错: %v", err)
	}
	if !strings.Contains(text, "未找到") {
		t.Fatalf("应提示未找到: %s", text)
	}
}

// TestWorkEventSerializesToJSON 确认事件可序列化（前端消费格式）。
func TestWorkEventSerializesToJSON(t *testing.T) {
	ev := WorkEvent{
		ExpertID: "e1", ExpertName: "专家", Stage: StageTool,
		Detail: "调用工具 file_read", Timestamp: 1234567890,
	}
	data, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if !strings.Contains(string(data), "expert_id") {
		t.Fatalf("JSON 字段名异常: %s", data)
	}
}

// --- 测试辅助 ---

type fakeCustomStore struct {
	mu      sync.Mutex
	entries []Expert
}

func newFakeCustomStore() *fakeCustomStore { return &fakeCustomStore{} }

func (f *fakeCustomStore) Load() ([]Expert, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Expert, len(f.entries))
	copy(out, f.entries)
	return out, nil
}

func (f *fakeCustomStore) Save(e Expert) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, existing := range f.entries {
		if existing.ID == e.ID {
			f.entries[i] = e
			return nil
		}
	}
	f.entries = append(f.entries, e)
	return nil
}

func (f *fakeCustomStore) Delete(id string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, e := range f.entries {
		if e.ID == id {
			f.entries = append(f.entries[:i], f.entries[i+1:]...)
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeCustomStore) save(e Expert) { _ = f.Save(e) }

// failingCustomStore 模拟持久层故障。
type failingCustomStore struct{}

func (failingCustomStore) Load() ([]Expert, error)     { return nil, errFakeStore }
func (failingCustomStore) Save(Expert) error           { return errFakeStore }
func (failingCustomStore) Delete(string) (bool, error) { return false, errFakeStore }

var errFakeStore = &fakeStoreError{}

type fakeStoreError struct{}

func (e *fakeStoreError) Error() string { return "fake store failure" }

// restrictedExecutor 只允许指定工具。
type restrictedExecutor struct{ available map[string]bool }

func (r *restrictedExecutor) Execute(context.Context, ToolCallRequest) (ToolCallResult, error) {
	return ToolCallResult{Content: "ok", Success: true}, nil
}
func (r *restrictedExecutor) Available(name string) bool { return r.available[name] }

// failingProvider 所有调用都失败。
type failingProvider struct{}

func (f *failingProvider) Complete(context.Context, provider.CompletionRequest) (provider.CompletionResponse, error) {
	return provider.CompletionResponse{}, errProviderFailed
}
func (f *failingProvider) Stream(context.Context, provider.CompletionRequest) (<-chan provider.StreamChunk, error) {
	return nil, errProviderFailed
}
func (f *failingProvider) Name() string         { return "failing" }
func (f *failingProvider) ContextWindow() int   { return 1000 }
func (f *failingProvider) MaxOutputTokens() int { return 100 }

var errProviderFailed = &providerError{}

type providerError struct{}

func (e *providerError) Error() string { return "provider unavailable" }

func containsStr(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "..."
}
