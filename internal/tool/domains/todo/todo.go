// Package tododomain 提供待办清单工具（todo_write）。
//
// 为什么这个工具必须存在：F1 闭环（「待办全部完成就收尾」）与 closure 报告的
// todos_done 检查都以它为前提 —— agent/loop.go 从工具结果的 Metadata 里读
// {total, done} 判断是否该进入收尾轮，ClosureTracker 也靠它记录快照。工具不存在时
// 这两条路径永远不触发（TodoSeen 恒为 false），而部门推荐表与权限/幂等分类表却
// 一直在引用它 —— 那是「代码说有、运行时不存在的假能力」。
//
// 存储位置：**进程内、按 run 隔离**。待办的真相是「模型当前这一轮工作的分解」，
// 它的判据由 run 内的闭环跟踪器持有；跨进程恢复后重放一份旧待办列表反而会让模型
// 以为任务还在中途。因此这里不做持久化，只保证同一 run 内的一致视图。
package tododomain

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/ximo888ok-netizen/ximo-agent/internal/tool"
	"github.com/ximo888ok-netizen/ximo-agent/internal/tool/domains"
)

// ToolName 工具名。闭环与前端都按这个名字识别（agent/loop.go 的 todoSnapshot）。
const ToolName = "todo_write"

// 状态取值。completed 是 F1 的判定依据，其余都算未完成。
const (
	StatusPending    = "pending"
	StatusInProgress = "in_progress"
	StatusCompleted  = "completed"
)

// maxItems 单次写入的条目上限。
//
// 上限是为了让工具结果保持在一个可读、可推理的规模：待办清单是给模型和用户看的
// 工作分解，不是数据库。超过上限时明确报错而不是静默截断 —— 静默截断会让
// {total, done} 与用户看到的清单不一致。
const maxItems = 100

// maxContentRunes 单条待办的长度上限。
const maxContentRunes = 500

// maxRuns 保留多少个 run 的待办视图。
//
// 待办只在 run 存活期间有意义，但引擎没有「run 结束」回调给工具。用 FIFO 上限
// 兜底：长时间运行的进程不会因为每个 run 一份清单而无界增长。
const maxRuns = 128

// Item 一条待办。
type Item struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	Status  string `json:"status"`
}

// Tool 实现 todo_write 工具。
type Tool struct {
	mu sync.Mutex
	// runs 是 runID → 待办列表（按写入顺序）。
	runs map[string][]Item
	// order 记录 runs 的插入顺序，用于 FIFO 淘汰。
	order []string
}

// New 创建工具。
func New() *Tool {
	return &Tool{runs: make(map[string][]Item, maxRuns)}
}

// Definition 实现 tool.Tool。
func (t *Tool) Definition() tool.ToolDefinition {
	return tool.ToolDefinition{
		Name: ToolName,
		Description: "维护本次任务的待办清单。write 用完整列表整体替换（适合首次分解与批量更新），" +
			"update 改单条状态（适合边做边推进），list 查看当前清单。" +
			"当所有待办都为 completed 时，本次任务会进入收尾并给出最终总结 —— " +
			"所以请如实推进状态，不要为了早点收尾而把没做完的标成 completed。",
		Parameters: tool.ObjectSchema(map[string]tool.JSONSchema{
			"action": {Type: "string", Description: "操作类型", Enum: []any{"write", "update", "list"}},
			"todos": {
				Type:        "array",
				Description: "write: 完整待办列表（整体替换，最多 100 条）",
				Items: &tool.JSONSchema{
					Type: "object",
					Properties: map[string]tool.JSONSchema{
						"id":      {Type: "string", Description: "可选稳定 ID；不填按序号生成"},
						"content": {Type: "string", Description: "待办内容"},
						"status":  {Type: "string", Description: "状态", Enum: []any{StatusPending, StatusInProgress, StatusCompleted}},
					},
					Required: []string{"content"},
				},
			},
			"id":     {Type: "string", Description: "update: 要更新的待办 ID"},
			"status": {Type: "string", Description: "update: 新状态", Enum: []any{StatusPending, StatusInProgress, StatusCompleted}},
		}, "action"),
		Risk:        tool.RiskLow,
		Idempotency: tool.ClassDetectable,
		Domain:      tool.DomainInProcess,
	}
}

// Execute 实现 tool.Tool。
func (t *Tool) Execute(ctx context.Context, req tool.ToolRequest) tool.ToolResponse {
	fail := func(format string, args ...any) tool.ToolResponse {
		return domains.Fail(req.ToolCallID, ToolName, format, args...)
	}
	if err := ctx.Err(); err != nil {
		return fail("调用已取消: %v", err)
	}
	// 没有 run 上下文时不写入任何视图：按空 runID 存待办会让所有无上下文的调用
	// 共享一份清单（一个 run 的待办出现在另一个 run 里），比直接报错危险得多。
	runID := strings.TrimSpace(req.RunID)
	if runID == "" {
		return fail("缺少 run_id：待办清单按 run 隔离，无法写入")
	}

	switch action := domains.StringArg(req.Arguments, "action"); action {
	case "write":
		return t.handleWrite(req, runID, fail)
	case "update":
		return t.handleUpdate(req, runID, fail)
	case "list":
		return t.handleList(req, runID)
	default:
		return fail("未知 action: %q（支持 write/update/list）", action)
	}
}

func (t *Tool) handleWrite(
	req tool.ToolRequest,
	runID string,
	fail func(string, ...any) tool.ToolResponse,
) tool.ToolResponse {
	raw, ok := req.Arguments["todos"]
	if !ok {
		return fail("write 需要 todos 数组")
	}
	arr, ok := raw.([]any)
	if !ok {
		return fail("todos 必须是数组")
	}
	if len(arr) == 0 {
		return fail("todos 不能为空：清空待办请用 action=\"write\" 提交新的分解，或直接不再调用本工具")
	}
	if len(arr) > maxItems {
		return fail("待办条目 %d 条，超过上限 %d 条；请合并成更粗粒度的分解", len(arr), maxItems)
	}

	items := make([]Item, 0, len(arr))
	seen := make(map[string]bool, len(arr))
	for i, rawItem := range arr {
		obj, ok := rawItem.(map[string]any)
		if !ok {
			return fail("todos[%d] 必须是对象", i)
		}
		content := strings.TrimSpace(domains.StringArg(obj, "content"))
		if content == "" {
			return fail("todos[%d].content 不能为空", i)
		}
		if n := len([]rune(content)); n > maxContentRunes {
			return fail("todos[%d].content 长度 %d 超过上限 %d", i, n, maxContentRunes)
		}
		id := strings.TrimSpace(domains.StringArg(obj, "id"))
		if id == "" {
			id = fmt.Sprintf("todo-%d", i+1)
		}
		if seen[id] {
			return fail("todos[%d].id = %q 重复；ID 是后续 update 的定位依据", i, id)
		}
		seen[id] = true

		status := strings.TrimSpace(domains.StringArg(obj, "status"))
		if status == "" {
			status = StatusPending
		}
		if !validStatus(status) {
			return fail("todos[%d].status = %q 非法（pending/in_progress/completed）", i, status)
		}
		items = append(items, Item{ID: id, Content: content, Status: status})
	}

	t.store(runID, items)
	return t.response(req.ToolCallID, items, fmt.Sprintf("已更新待办清单（%d 条）：%s", len(items), summary(items)))
}

func (t *Tool) handleUpdate(
	req tool.ToolRequest,
	runID string,
	fail func(string, ...any) tool.ToolResponse,
) tool.ToolResponse {
	id := strings.TrimSpace(domains.StringArg(req.Arguments, "id"))
	if id == "" {
		return fail("update 需要 id")
	}
	status := strings.TrimSpace(domains.StringArg(req.Arguments, "status"))
	if !validStatus(status) {
		return fail("update 需要合法 status（pending/in_progress/completed），收到 %q", status)
	}

	t.mu.Lock()
	items := append([]Item(nil), t.runs[runID]...)
	t.mu.Unlock()
	if len(items) == 0 {
		return fail("本次 run 还没有待办清单：请先用 action=\"write\" 提交分解")
	}

	found := false
	for i := range items {
		if items[i].ID == id {
			items[i].Status = status
			found = true
			break
		}
	}
	if !found {
		return fail("待办 %q 不存在（当前：%s）", id, ids(items))
	}

	t.store(runID, items)
	return t.response(req.ToolCallID, items,
		fmt.Sprintf("待办 %s 已置为 %s：%s", id, status, summary(items)))
}

func (t *Tool) handleList(req tool.ToolRequest, runID string) tool.ToolResponse {
	t.mu.Lock()
	items := append([]Item(nil), t.runs[runID]...)
	t.mu.Unlock()

	if len(items) == 0 {
		// 空清单不是错误：list 的用途就是看一眼「现在有什么」，没有就是没有。
		return t.response(req.ToolCallID, items, "本次 run 还没有待办清单")
	}
	return t.response(req.ToolCallID, items, fmt.Sprintf("当前待办（%d 条）：%s", len(items), summary(items)))
}

// response 统一把清单、计数与 Metadata 一起返回。
//
// Metadata 里的 {total, done} 是**契约**：agent/loop.go 的 todoSnapshot 与
// ClosureTracker 都只认这两个键，缺了 F1 闭环与 todos_done 检查就永远不会触发。
// total 必须 > 0，否则 loop 视为「没有待办快照」。
func (t *Tool) response(toolCallID string, items []Item, text string) tool.ToolResponse {
	resp := domains.Text(toolCallID, ToolName, text)
	done := 0
	for _, it := range items {
		if it.Status == StatusCompleted {
			done++
		}
	}
	// 正文同时给一份 JSON，模型与用户都能直接看到结构化清单。
	if body, err := json.Marshal(items); err == nil {
		resp.Content = text + "\n\n```json\n" + string(body) + "\n```"
	}
	resp.Metadata = map[string]any{
		"total": len(items),
		"done":  done,
		"items": items,
	}
	return resp
}

func (t *Tool) store(runID string, items []Item) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, exists := t.runs[runID]; !exists {
		t.order = append(t.order, runID)
		for len(t.order) > maxRuns {
			oldest := t.order[0]
			t.order = t.order[1:]
			delete(t.runs, oldest)
		}
	}
	t.runs[runID] = items
}

func validStatus(s string) bool {
	switch s {
	case StatusPending, StatusInProgress, StatusCompleted:
		return true
	default:
		return false
	}
}

// summary 生成一行「已完成 2/5」式的摘要与最多三条待办，避免把整份清单塞进
// 一行文本里（结构化内容在 Metadata 与 JSON 正文里）。
func summary(items []Item) string {
	done := 0
	for _, it := range items {
		if it.Status == StatusCompleted {
			done++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "已完成 %d/%d", done, len(items))
	shown := 0
	for _, it := range items {
		if it.Status == StatusCompleted || shown >= 3 {
			continue
		}
		fmt.Fprintf(&b, "；待办 %s[%s]", it.Content, it.Status)
		shown++
	}
	return b.String()
}

func ids(items []Item) string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.ID)
	}
	return strings.Join(out, ", ")
}
