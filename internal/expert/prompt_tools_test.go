package expert

import (
	"context"
	"strings"
	"testing"

	"github.com/ximo888ok-netizen/ximo-agent/internal/provider"
)

// systemMessageOf 取出一次请求里的 system 提示词。
func systemMessageOf(t *testing.T, req provider.CompletionRequest) string {
	t.Helper()
	for _, m := range req.Messages {
		if m.Role == provider.RoleSystem {
			return m.Content
		}
	}
	t.Fatal("请求里没有 system 消息")
	return ""
}

// productionEngineeringTools 是默认配置下工程类专家真正能用的工具
// （与 internal/bootstrap/expert_tools_test.go 的注册表快照一致的子集）。
var productionEngineeringTools = []string{"file_read", "file_write", "file_search", "terminal_exec"}

// TestExecutePromptOnlyAdvertisesExecutableTools 是「提示词说了什么，模型就能调什么」
// 的验收。
//
// 背景：实施阶段的系统提示词此前直接照抄部门推荐表，而推荐表里含大量本 build 未
// 实现的工具（code_execute / git_operations / todo_write…）。于是模型被告知"你已被
// 配置以下工具"，去调用时却得到「工具未注册」，或者干脆在回答里声称用过了 ——
// 用户看到的是一段自信的假执行报告。
func TestExecutePromptOnlyAdvertisesExecutableTools(t *testing.T) {
	p := &fakeProvider{
		responses: []provider.CompletionResponse{
			{FinishReason: provider.FinishStop, Content: "1. 读文件"},
			{FinishReason: provider.FinishStop, Content: "完成"},
		},
	}
	exec := &describingExecutor{
		available: map[string]bool{},
		descs:     map[string]ToolDescriptor{},
	}
	for _, n := range productionEngineeringTools {
		exec.available[n] = true
	}

	o := NewOrchestrator(OrchestratorOptions{
		Registry:       NewRegistry(nil),
		Runner:         SubAgentOptions{Provider: p, Executor: exec, Model: "m"},
		EnableTwoPhase: true,
	})

	// 用真实专家库里的工程类专家：它的推荐工具集里必然包含推荐表里的全部名字。
	e, ok := o.Registry().Get("engineering-frontend-developer")
	if !ok {
		t.Fatal("找不到 engineering-frontend-developer")
	}
	if _, err := o.Activate(context.Background(), ExpertRequest{ExpertID: e.ID, Task: "改一下登录页"}); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if len(p.calls) < 2 {
		t.Fatalf("模型调用次数 = %d，期望至少 2（计划 + 实施）", len(p.calls))
	}

	executeReq := p.calls[1]
	prompt := systemMessageOf(t, executeReq)

	// 1. 真工具必须在提示词里。
	for _, n := range productionEngineeringTools {
		if !strings.Contains(prompt, "`"+n+"`") {
			t.Errorf("系统提示词没有列出可用工具 %s", n)
		}
	}
	// 2. 推荐表里有、但本 build 没实现的工具一个都不能出现。
	for _, n := range []string{
		"code_execute", "code_lint", "code_format", "git_operations",
		"todo_write", "multi_edit", "project_index", "dependency_check",
	} {
		if strings.Contains(prompt, "`"+n+"`") {
			t.Errorf("系统提示词宣传了未注册的工具 %s（模型会去调用它并失败）", n)
		}
	}
	// 3. 提示词与 function schema 必须同源同集合。
	schema := map[string]bool{}
	for _, d := range executeReq.Tools {
		schema[d.Name] = true
	}
	for _, n := range productionEngineeringTools {
		if !schema[n] {
			t.Errorf("提示词列了 %s，但 function schema 里没有", n)
		}
	}
	if len(schema) != len(productionEngineeringTools) {
		t.Errorf("schema 工具数 = %d，期望 %d（提示词与 schema 必须一致）",
			len(schema), len(productionEngineeringTools))
	}
}

// TestExecutePromptDeclaresNoToolsWhenNoneAvailable 确认「一个工具都没有」时，
// 提示词明确说出来并禁止虚构工具调用 —— 空列表不能让模型以为自己有工具。
func TestExecutePromptDeclaresNoToolsWhenNoneAvailable(t *testing.T) {
	p := &fakeProvider{
		responses: []provider.CompletionResponse{
			{FinishReason: provider.FinishStop, Content: "1. 分析"},
			{FinishReason: provider.FinishStop, Content: "纯分析结论"},
		},
	}
	// 生产上会出现这种情况：例如所有推荐工具都没注册，或工具运行时不可用。
	exec := &describingExecutor{available: map[string]bool{}, descs: map[string]ToolDescriptor{}}

	o := NewOrchestrator(OrchestratorOptions{
		Registry:       NewRegistry(nil),
		Runner:         SubAgentOptions{Provider: p, Executor: exec, Model: "m"},
		EnableTwoPhase: true,
	})
	e, ok := o.Registry().Get("engineering-frontend-developer")
	if !ok {
		t.Fatal("找不到 engineering-frontend-developer")
	}

	out, err := o.Activate(context.Background(), ExpertRequest{ExpertID: e.ID, Task: "改一下登录页"})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if !out.SubAgentMode {
		t.Fatal("没有工具不等于不能执行：子 Agent 仍应产出分析")
	}

	executeReq := p.calls[1]
	prompt := systemMessageOf(t, executeReq)
	if !strings.Contains(prompt, "没有任何可用工具") {
		t.Errorf("提示词没有明确告知没有工具可用：\n%s", prompt)
	}
	if len(executeReq.Tools) != 0 {
		t.Errorf("tools = %d，期望 0", len(executeReq.Tools))
	}
	// 不能留下「你已被配置以下工具」这句话，否则模型会以为自己有工具。
	if strings.Contains(prompt, "你已被配置以下工具") {
		t.Error("提示词仍然声称已配置工具")
	}
	// 禁止虚构：输出要求里必须有一条约束。
	if !strings.Contains(prompt, "不要编造工具执行结果") {
		t.Error("提示词缺少「不要编造工具执行结果」的约束")
	}
}

// TestInfoPathKeepsRecommendationList 确认「只看专家档案」的信息型返回仍然展示
// 完整推荐表（那里不执行工具，展示推荐能力是它的用途）。
func TestInfoPathKeepsRecommendationList(t *testing.T) {
	o := NewOrchestrator(OrchestratorOptions{
		Registry: NewRegistry(nil),
		Runner:   SubAgentOptions{Provider: &fakeProvider{}, Executor: &describingExecutor{}},
	})
	e, ok := o.Registry().Get("engineering-frontend-developer")
	if !ok {
		t.Fatal("找不到 engineering-frontend-developer")
	}

	// 不带 Task → 信息型返回。
	out, err := o.Activate(context.Background(), ExpertRequest{ExpertID: e.ID})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if out.SubAgentMode {
		t.Fatal("无 task 不应进入子 Agent 模式")
	}
	if !strings.Contains(out.System, "`code_execute`") {
		t.Error("信息型返回应展示完整推荐工具表")
	}
}
