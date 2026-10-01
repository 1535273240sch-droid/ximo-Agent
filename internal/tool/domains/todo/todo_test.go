package tododomain

import (
	"context"
	"testing"

	"github.com/ximo888ok-netizen/ximo-agent/internal/tool"
)

func call(t *testing.T, tt *Tool, runID string, args map[string]any) tool.ToolResponse {
	t.Helper()
	return tt.Execute(context.Background(), tool.ToolRequest{
		RunID:      runID,
		ToolCallID: "call-1",
		Name:       ToolName,
		Arguments:  args,
	})
}

// TestWriteReportsTotalAndDone 锁住 loop/closure 依赖的契约：
// Metadata 必须带 {total, done}，且 total > 0 —— 缺了它 F1 闭环与 todos_done
// 检查永远不会触发（agent/loop.go 的 todoSnapshot 只认这两个键）。
func TestWriteReportsTotalAndDone(t *testing.T) {
	tt := New()
	resp := call(t, tt, "run-1", map[string]any{
		"action": "write",
		"todos": []any{
			map[string]any{"id": "a", "content": "读代码", "status": StatusCompleted},
			map[string]any{"id": "b", "content": "改代码"},
			map[string]any{"id": "c", "content": "跑测试", "status": StatusInProgress},
		},
	})
	if !resp.Success {
		t.Fatalf("write 失败：%s", resp.Error)
	}
	if got := resp.Metadata["total"]; got != 3 {
		t.Errorf("total = %v，期望 3", got)
	}
	if got := resp.Metadata["done"]; got != 1 {
		t.Errorf("done = %v，期望 1", got)
	}
	if resp.ToolName != ToolName {
		t.Errorf("tool name = %q，期望 %q", resp.ToolName, ToolName)
	}
}

// TestUpdateMovesDoneCount 确认 update 改单条状态后计数同步变化（F1 的判据来源）。
func TestUpdateMovesDoneCount(t *testing.T) {
	tt := New()
	call(t, tt, "run-1", map[string]any{
		"action": "write",
		"todos": []any{
			map[string]any{"id": "a", "content": "一"},
			map[string]any{"id": "b", "content": "二"},
		},
	})

	resp := call(t, tt, "run-1", map[string]any{"action": "update", "id": "a", "status": StatusCompleted})
	if !resp.Success {
		t.Fatalf("update 失败：%s", resp.Error)
	}
	if got := resp.Metadata["done"]; got != 1 {
		t.Errorf("done = %v，期望 1", got)
	}
	if got := resp.Metadata["total"]; got != 2 {
		t.Errorf("total = %v，期望 2", got)
	}

	resp = call(t, tt, "run-1", map[string]any{"action": "update", "id": "b", "status": StatusCompleted})
	if got := resp.Metadata["done"]; got != 2 {
		t.Errorf("done = %v，期望 2（全部完成）", got)
	}
}

// TestListWithoutWriteIsNotAnError 确认 list 在没有清单时是正常结果而不是错误：
// 模型经常先看一眼再决定要不要分解，报错只会诱发一次无意义的自纠回合。
func TestListWithoutWriteIsNotAnError(t *testing.T) {
	tt := New()
	resp := call(t, tt, "run-1", map[string]any{"action": "list"})
	if !resp.Success {
		t.Fatalf("空清单的 list 不应失败：%s", resp.Error)
	}
	if got := resp.Metadata["total"]; got != 0 {
		t.Errorf("total = %v，期望 0", got)
	}
}

// TestTodosAreIsolatedPerRun 确认待办按 run 隔离：共享一份清单会让一个 run 的
// 进度出现在另一个 run 里，F1 会据此提前收尾。
func TestTodosAreIsolatedPerRun(t *testing.T) {
	tt := New()
	call(t, tt, "run-a", map[string]any{
		"action": "write",
		"todos":  []any{map[string]any{"id": "a", "content": "甲任务的活"}},
	})

	resp := call(t, tt, "run-b", map[string]any{"action": "list"})
	if got := resp.Metadata["total"]; got != 0 {
		t.Fatalf("另一个 run 看到了 %v 条待办，期望 0", got)
	}

	// 用另一个 run 的 id 去 update 必须失败，而不是悄悄改到别人的清单。
	resp = call(t, tt, "run-b", map[string]any{"action": "update", "id": "a", "status": StatusCompleted})
	if resp.Success {
		t.Fatal("run-b 不应能更新 run-a 的待办")
	}
}

func TestWriteValidation(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
	}{
		{"缺少 todos", map[string]any{"action": "write"}},
		{"todos 不是数组", map[string]any{"action": "write", "todos": "x"}},
		{"空数组", map[string]any{"action": "write", "todos": []any{}}},
		{"条目不是对象", map[string]any{"action": "write", "todos": []any{"x"}}},
		{"content 为空", map[string]any{"action": "write", "todos": []any{map[string]any{"content": "  "}}}},
		{"id 重复", map[string]any{"action": "write", "todos": []any{
			map[string]any{"id": "a", "content": "一"},
			map[string]any{"id": "a", "content": "二"},
		}}},
		{"status 非法", map[string]any{"action": "write", "todos": []any{
			map[string]any{"content": "一", "status": "done"},
		}}},
		{"未知 action", map[string]any{"action": "nope"}},
		{"update 缺 id", map[string]any{"action": "update", "status": StatusCompleted}},
		{"update 状态非法", map[string]any{"action": "update", "id": "a", "status": "x"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tt := New()
			resp := call(t, tt, "run-1", tc.args)
			if resp.Success {
				t.Fatalf("期望失败，实际成功：%+v", resp.Metadata)
			}
			if resp.Error == "" {
				t.Error("失败响应必须带可读的 Error")
			}
		})
	}
}

// TestMissingRunIDIsRejected 确认没有 run 上下文时拒绝写入：按空 runID 存待办会让
// 所有无上下文的调用共享一份清单。
func TestMissingRunIDIsRejected(t *testing.T) {
	tt := New()
	resp := tt.Execute(context.Background(), tool.ToolRequest{
		ToolCallID: "call-1",
		Name:       ToolName,
		Arguments:  map[string]any{"action": "list"},
	})
	if resp.Success {
		t.Fatal("缺少 run_id 时必须失败")
	}
}

// TestUpdateUnknownIDReportsCurrentIDs 确认报错里带上当前 ID 列表，模型才能自纠
// （只说「不存在」会让它再猜一次）。
func TestUpdateUnknownIDReportsCurrentIDs(t *testing.T) {
	tt := New()
	call(t, tt, "run-1", map[string]any{
		"action": "write",
		"todos":  []any{map[string]any{"id": "alpha", "content": "一"}},
	})
	resp := call(t, tt, "run-1", map[string]any{"action": "update", "id": "beta", "status": StatusCompleted})
	if resp.Success {
		t.Fatal("不存在的 id 必须失败")
	}
	if want := "alpha"; !contains(resp.Error, want) {
		t.Errorf("错误信息 %q 应包含现有 ID %q", resp.Error, want)
	}
}

// TestTooManyTodosRejected 确认超限时明确报错而不是静默截断：
// 静默截断会让 {total, done} 与用户看到的清单不一致。
func TestTooManyTodosRejected(t *testing.T) {
	todos := make([]any, 0, maxItems+1)
	for i := 0; i <= maxItems; i++ {
		todos = append(todos, map[string]any{"id": string(rune('a'+i%26)) + string(rune('0'+i/26)), "content": "x"})
	}
	tt := New()
	resp := call(t, tt, "run-1", map[string]any{"action": "write", "todos": todos})
	if resp.Success {
		t.Fatalf("超过 %d 条时应失败", maxItems)
	}
}

// TestDefinitionDeclaresContract 确认 schema 里声明了 loop/closure 依赖的字段。
func TestDefinitionDeclaresContract(t *testing.T) {
	def := New().Definition()
	if def.Name != ToolName {
		t.Errorf("name = %q，期望 %q", def.Name, ToolName)
	}
	for _, key := range []string{"action", "todos", "id", "status"} {
		if _, ok := def.Parameters.Properties[key]; !ok {
			t.Errorf("schema 缺少参数 %q", key)
		}
	}
	if def.Idempotency != tool.ClassDetectable {
		t.Errorf("idempotency = %v，期望 ClassDetectable（与幂等分类表一致）", def.Idempotency)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
