package expert

import (
	"context"
	"testing"

	"github.com/ximo888ok-netizen/ximo-agent/internal/provider"
)

// describingExecutor 只承认部分工具可用，并能给出真实定义。
//
// 它模拟的是生产装配（bootstrap.toolRuntimeAdapter）：能回答「这个 build 里
// 有没有这个工具」以及「它的参数长什么样」。子 Agent 的工具 schema 必须完全
// 依据这两个问题的答案，而不是依据部门推荐表里的名字。
type describingExecutor struct {
	available map[string]bool
	descs     map[string]ToolDescriptor
	executed  []string
}

func (d *describingExecutor) Available(name string) bool { return d.available[name] }

func (d *describingExecutor) Execute(_ context.Context, req ToolCallRequest) (ToolCallResult, error) {
	d.executed = append(d.executed, req.Name)
	return ToolCallResult{Content: "ok", Success: true}, nil
}

func (d *describingExecutor) Describe(name string) (ToolDescriptor, bool) {
	desc, ok := d.descs[name]
	return desc, ok
}

// plainExecutor 只有 Available（不实现 ToolDescriber），用于验证占位 schema 回退。
type plainExecutor struct{ available map[string]bool }

func (p *plainExecutor) Available(name string) bool { return p.available[name] }

func (p *plainExecutor) Execute(context.Context, ToolCallRequest) (ToolCallResult, error) {
	return ToolCallResult{Content: "ok", Success: true}, nil
}

// toolRequestFor 跑一轮子 Agent 并返回 provider 收到的最后一个请求。
//
// 脚本给的是「直接收尾」的响应，因此只会发生一次 API 调用，断言的就是那一次
// 请求里携带的 tool schema。
func toolRequestFor(t *testing.T, exec ToolExecutor, names []string) provider.CompletionRequest {
	t.Helper()

	fp := &fakeProvider{responses: []provider.CompletionResponse{
		{FinishReason: provider.FinishStop, Content: "done"},
	}}
	res, err := RunSubAgent(context.Background(), SubAgentRequest{
		SystemPrompt: BuildSystemPrompt(Expert{ID: "e1", Name: "测试专家", Emoji: "🧪"}),
		Task:         "做事",
		Options: SubAgentOptions{
			Provider:  fp,
			Executor:  exec,
			ToolNames: names,
		},
	})
	if err != nil {
		t.Fatalf("RunSubAgent: %v", err)
	}
	if res.Rounds != 1 {
		t.Fatalf("rounds = %d, want 1 (a single no-tool round)", res.Rounds)
	}
	return fp.lastRequest()
}

// TestSubAgentSchemaExcludesUnregisteredTools 是「已注册才进 schema」的验收：
// 部门推荐表里那些本 build 没有实现的工具名绝不能交给模型 —— 模型选中后必然
// 得到一次「工具未注册」的失败往返，还会误以为自己有该能力。
func TestSubAgentSchemaExcludesUnregisteredTools(t *testing.T) {
	exec := &describingExecutor{
		available: map[string]bool{"file_read": true},
		descs:     map[string]ToolDescriptor{"file_read": {Description: "读文件"}},
	}

	req := toolRequestFor(t, exec, []string{"file_read", "ui_generate", "code_execute", ""})

	if len(req.Tools) != 1 {
		names := make([]string, 0, len(req.Tools))
		for _, d := range req.Tools {
			names = append(names, d.Name)
		}
		t.Fatalf("tools = %v, want exactly [file_read]", names)
	}
	if req.Tools[0].Name != "file_read" {
		t.Fatalf("tool = %q, want file_read", req.Tools[0].Name)
	}
}

// TestSubAgentSchemaCarriesRealParameters 是「schema 用真实定义」的验收：
// 旧实现给每个工具都发 {"type":"object","properties":{}}，模型看不到任何参数名，
// 只能瞎猜，第一轮就被工具的参数校验打回。
func TestSubAgentSchemaCarriesRealParameters(t *testing.T) {
	params := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{"type": "string"},
		},
		"required": []any{"path"},
	}
	exec := &describingExecutor{
		available: map[string]bool{"file_read": true},
		descs: map[string]ToolDescriptor{
			"file_read": {Description: "读取指定路径的文件内容", Parameters: params},
		},
	}

	req := toolRequestFor(t, exec, []string{"file_read"})

	if len(req.Tools) != 1 {
		t.Fatalf("tools = %d, want 1", len(req.Tools))
	}
	got := req.Tools[0]
	if got.Description != "读取指定路径的文件内容" {
		t.Errorf("description = %q, want the tool's own description", got.Description)
	}
	props, _ := got.Parameters["properties"].(map[string]any)
	if _, ok := props["path"]; !ok {
		t.Errorf("parameters = %v, want the tool's real property list", got.Parameters)
	}
	if required, _ := got.Parameters["required"].([]any); len(required) != 1 {
		t.Errorf("required = %v, want the tool's real required list", got.Parameters["required"])
	}
}

// TestSubAgentSchemaFallsBackWhenExecutorCannotDescribe 确认没有导出能力的执行器
// 仍能工作（占位 schema），而不是让工具直接消失 —— 只是参数信息缺失。
func TestSubAgentSchemaFallsBackWhenExecutorCannotDescribe(t *testing.T) {
	exec := &plainExecutor{available: map[string]bool{"file_read": true}}

	req := toolRequestFor(t, exec, []string{"file_read"})

	if len(req.Tools) != 1 {
		t.Fatalf("tools = %d, want 1 (placeholder schema must still be offered)", len(req.Tools))
	}
	if req.Tools[0].Parameters["type"] != "object" {
		t.Errorf("parameters = %v, want the empty-object placeholder", req.Tools[0].Parameters)
	}
}

// TestSubAgentDropsEveryToolWhenNoneAvailable 确认「一个都没注册」时干脆不发
// tools 字段，而不是发一堆注定失败的名字。
func TestSubAgentDropsEveryToolWhenNoneAvailable(t *testing.T) {
	exec := &describingExecutor{available: map[string]bool{}, descs: map[string]ToolDescriptor{}}

	req := toolRequestFor(t, exec, []string{"ui_generate", "code_execute"})

	if len(req.Tools) != 0 {
		t.Fatalf("tools = %d, want 0 when nothing is registered", len(req.Tools))
	}
}
