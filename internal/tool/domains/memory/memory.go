// Package memory 提供 memory 工具：让模型**主动**检索、写入、列出、遗忘长期记忆。
//
// 与引擎自动召回的分工：
//   - 自动召回（internal/memory + engine）解决「模型不知道自己该记得什么」——
//     每次 run 开头把相关记忆直接放进上下文，模型不必先问；
//   - 本工具解决「模型明确知道自己想查/想记」——例如用户说「记住这个」，
//     或者模型发现自己需要三周前定下的某个约定。
//
// 两条路径写的是同一个 mem0 库，所以工具写入的内容下一轮就会被自动召回带回来。
package memory

import (
	"context"
	"fmt"
	"strings"

	"github.com/ximo888ok-netizen/ximo-agent/internal/memory"
	"github.com/ximo888ok-netizen/ximo-agent/internal/tool"
	"github.com/ximo888ok-netizen/ximo-agent/internal/tool/domains"
)

// Backend 是工具需要的记忆读写能力。（*memory.Service 实现它。）
type Backend interface {
	Search(ctx context.Context, query string, topK int) ([]memory.Record, error)
	Add(ctx context.Context, msgs []memory.Message, opts memory.AddOptions) ([]memory.Record, error)
	GetAll(ctx context.Context, topK int) ([]memory.Record, error)
	Delete(ctx context.Context, id string) error
}

// Tool 实现 memory 工具。
type Tool struct {
	backend Backend
}

// New 创建 memory 工具。backend 为 nil 时工具仍会注册，但每次调用都返回
// 「未启用」的明确错误——这比让工具从注册表里消失更好：模型看到的工具列表
// 因此保持稳定，而「记忆没开」这件事在用户要求记东西时会被明确说出来。
func New(backend Backend) *Tool { return &Tool{backend: backend} }

// Definition 实现 tool.Tool。
func (t *Tool) Definition() tool.ToolDefinition {
	return tool.ToolDefinition{
		Name: "memory",
		Description: "管理跨会话的长期记忆（mem0）。支持检索（search）、写入（add）、" +
			"浏览（list）、遗忘（forget）。\n\n" +
			"## 何时使用\n" +
			"- 用户说「记住这个」「以后都这样」→ add，把结论写下来\n" +
			"- 需要回忆以前的约定、偏好、踩过的坑 → search\n" +
			"- 记忆里有过时或错误的内容 → list 找到 id 后 forget\n\n" +
			"## 与自动召回的关系\n" +
			"每次对话开始时会自动注入相关记忆，所以不必为了「用上记忆」而先调 search；" +
			"只在自动召回没覆盖到你需要的细节时才主动检索。",
		Parameters: tool.ObjectSchema(map[string]tool.JSONSchema{
			"action": {
				Type:        "string",
				Description: "操作类型",
				Enum:        []any{"search", "add", "list", "forget"},
			},
			"query":   {Type: "string", Description: "search: 检索语句（自然语言）"},
			"content": {Type: "string", Description: "add: 要记住的内容，一条一件事实，不要写成大段笔记"},
			"top_k":   {Type: "integer", Description: "search/list: 返回条数上限（默认沿用服务端配置）"},
			"id":      {Type: "string", Description: "forget: 要删除的记忆 id（先用 list/search 取到）"},
		}, "action"),
		// 这个工具能改外部状态（写入/删除记忆），所以不是纯只读的低风险工具；
		// 权限层按动作把 search/list 放行、add/forget 归入需要确认的档位。
		Risk:        tool.RiskMedium,
		Idempotency: tool.ClassDetectable,
		Domain:      tool.DomainInProcess,
	}
}

// Execute 实现 tool.Tool。
func (t *Tool) Execute(ctx context.Context, req tool.ToolRequest) tool.ToolResponse {
	fail := func(format string, args ...any) tool.ToolResponse {
		return domains.Fail(req.ToolCallID, "memory", format, args...)
	}
	action := domains.StringArg(req.Arguments, "action")
	if err := ctx.Err(); err != nil {
		return fail("调用已取消: %v", err)
	}
	if t.backend == nil {
		return fail("长期记忆未启用（config.json 的 memory.endpoint 为空或 enabled=false）")
	}

	switch action {
	case "search":
		return t.handleSearch(ctx, req)
	case "add":
		return t.handleAdd(ctx, req)
	case "list":
		return t.handleList(ctx, req)
	case "forget":
		return t.handleForget(ctx, req)
	case "":
		return fail("缺少参数 action（search/add/list/forget）")
	default:
		return fail("未知的 action %q（支持 search/add/list/forget）", action)
	}
}

func (t *Tool) handleSearch(ctx context.Context, req tool.ToolRequest) tool.ToolResponse {
	query := domains.StringArg(req.Arguments, "query")
	if query == "" {
		return domains.Fail(req.ToolCallID, "memory", "search 需要 query")
	}
	topK := domains.IntArg(req.Arguments, "top_k", 0)
	records, err := t.backend.Search(ctx, query, topK)
	if err != nil {
		return domains.Fail(req.ToolCallID, "memory", "检索长期记忆失败: %v", err)
	}
	if len(records) == 0 {
		return domains.Text(req.ToolCallID, "memory", "没有检索到相关记忆。")
	}
	return domains.Text(req.ToolCallID, "memory", formatRecords("检索到以下记忆：", records, true))
}

func (t *Tool) handleAdd(ctx context.Context, req tool.ToolRequest) tool.ToolResponse {
	content := strings.TrimSpace(domains.StringArg(req.Arguments, "content"))
	if content == "" {
		return domains.Fail(req.ToolCallID, "memory", "add 需要 content")
	}
	records, err := t.backend.Add(ctx, []memory.Message{{Role: "user", Content: content}}, memory.AddOptions{
		Metadata: map[string]any{"source": "memory_tool"},
	})
	if err != nil {
		return domains.Fail(req.ToolCallID, "memory", "写入长期记忆失败: %v", err)
	}
	if len(records) == 0 {
		// mem0 认为这段内容没有可长期保留的事实（例如寒暄），这不是错误，
		// 但必须如实告诉模型，否则模型会以为已经记住了。
		return domains.Text(req.ToolCallID, "memory", "已提交给记忆系统，但它判断这段内容没有需要长期保存的事实。")
	}
	return domains.Text(req.ToolCallID, "memory", formatRecords(
		fmt.Sprintf("已记住 %d 条：", len(records)), records, false))
}

func (t *Tool) handleList(ctx context.Context, req tool.ToolRequest) tool.ToolResponse {
	topK := domains.IntArg(req.Arguments, "top_k", 0)
	records, err := t.backend.GetAll(ctx, topK)
	if err != nil {
		return domains.Fail(req.ToolCallID, "memory", "列出长期记忆失败: %v", err)
	}
	if len(records) == 0 {
		return domains.Text(req.ToolCallID, "memory", "长期记忆里还没有内容。")
	}
	return domains.Text(req.ToolCallID, "memory", formatRecords("当前长期记忆：", records, true))
}

func (t *Tool) handleForget(ctx context.Context, req tool.ToolRequest) tool.ToolResponse {
	id := domains.StringArg(req.Arguments, "id")
	if id == "" {
		return domains.Fail(req.ToolCallID, "memory", "forget 需要 id（先用 list 或 search 取到）")
	}
	if err := t.backend.Delete(ctx, id); err != nil {
		return domains.Fail(req.ToolCallID, "memory", "删除记忆 %s 失败: %v", id, err)
	}
	return domains.Text(req.ToolCallID, "memory", fmt.Sprintf("已删除记忆 %s。", id))
}

// formatRecords 把记忆渲染成紧凑的列表。withID 为真时带上 id，因为 forget
// 需要它；add 的返回值里 id 对模型没有用处，带着只会浪费上下文。
func formatRecords(title string, records []memory.Record, withID bool) string {
	var b strings.Builder
	b.WriteString(title)
	for _, r := range records {
		text := strings.Join(strings.Fields(r.Memory), " ")
		if text == "" {
			continue
		}
		b.WriteString("\n- ")
		if withID && r.ID != "" {
			b.WriteString("[" + r.ID + "] ")
		}
		b.WriteString(text)
	}
	return b.String()
}
