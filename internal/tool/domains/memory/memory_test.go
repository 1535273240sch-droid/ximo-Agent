package memory

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ximo888ok-netizen/ximo-agent/internal/memory"
	"github.com/ximo888ok-netizen/ximo-agent/internal/tool"
)

// fakeBackend 记录工具动作落到后端的调用，并可以注入错误。
type fakeBackend struct {
	searchQuery string
	searchTopK  int
	added       []memory.Message
	deleted     string
	records     []memory.Record
	searchErr   error
	addErr      error
	listErr     error
	deleteErr   error
}

func (f *fakeBackend) Search(_ context.Context, query string, topK int) ([]memory.Record, error) {
	f.searchQuery, f.searchTopK = query, topK
	return f.records, f.searchErr
}

func (f *fakeBackend) Add(_ context.Context, msgs []memory.Message, _ memory.AddOptions) ([]memory.Record, error) {
	f.added = append(f.added, msgs...)
	return f.records, f.addErr
}

func (f *fakeBackend) GetAll(_ context.Context, _ int) ([]memory.Record, error) {
	return f.records, f.listErr
}

func (f *fakeBackend) Delete(_ context.Context, id string) error {
	f.deleted = id
	return f.deleteErr
}

func req(args map[string]any) tool.ToolRequest {
	return tool.ToolRequest{ToolCallID: "call-1", Name: "memory", Arguments: args}
}

func TestDefinitionIsStable(t *testing.T) {
	def := New(&fakeBackend{}).Definition()
	if def.Name != "memory" {
		t.Fatalf("工具名必须是 memory（权限规则与幂等策略都按它匹配）: %q", def.Name)
	}
	// 能改外部状态的工具不能自称低风险。
	if def.Risk != tool.RiskMedium {
		t.Fatalf("风险等级应为 medium，实际 %v", def.Risk)
	}
	if len(def.Parameters.Properties["action"].Enum) != 4 {
		t.Fatalf("action 的枚举应与实现分支一一对应: %+v", def.Parameters.Properties["action"].Enum)
	}
}

func TestExecuteSearch(t *testing.T) {
	backend := &fakeBackend{records: []memory.Record{{ID: "m1", Memory: "用户偏好暗色主题", Score: 0.9}}}
	resp := New(backend).Execute(context.Background(), req(map[string]any{"action": "search", "query": "主题", "top_k": float64(3)}))
	if !resp.Success {
		t.Fatalf("search 应成功: %+v", resp)
	}
	if backend.searchQuery != "主题" || backend.searchTopK != 3 {
		t.Fatalf("参数未透传: %q %d", backend.searchQuery, backend.searchTopK)
	}
	if !strings.Contains(resp.Content, "用户偏好暗色主题") {
		t.Fatalf("应返回记忆内容: %q", resp.Content)
	}
}

func TestExecuteSearchEmptyResultIsNotAnError(t *testing.T) {
	resp := New(&fakeBackend{}).Execute(context.Background(), req(map[string]any{"action": "search", "query": "没见过的词"}))
	if !resp.Success {
		t.Fatalf("没有命中不是错误: %+v", resp)
	}
	if !strings.Contains(resp.Content, "没有检索到") {
		t.Fatalf("应明确说明没有命中: %q", resp.Content)
	}
}

func TestExecuteAddReportsWhatWasRemembered(t *testing.T) {
	backend := &fakeBackend{records: []memory.Record{{ID: "m1", Memory: "用户偏好暗色主题"}}}
	resp := New(backend).Execute(context.Background(), req(map[string]any{"action": "add", "content": "以后都用暗色主题"}))
	if !resp.Success {
		t.Fatalf("add 应成功: %+v", resp)
	}
	if len(backend.added) != 1 || backend.added[0].Content != "以后都用暗色主题" {
		t.Fatalf("写入内容未透传: %+v", backend.added)
	}
	if strings.Contains(resp.Content, "m1") {
		t.Fatalf("add 的返回值不该带 id（无用的上下文开销）: %q", resp.Content)
	}
}

func TestExecuteAddWhenNothingWasExtracted(t *testing.T) {
	// mem0 判断这段内容没有需要长期保留的事实（例如寒暄）：必须如实告知，
	// 否则模型会以为已经记住了。
	resp := New(&fakeBackend{}).Execute(context.Background(), req(map[string]any{"action": "add", "content": "你好"}))
	if !resp.Success {
		t.Fatalf("这不应算失败: %+v", resp)
	}
	if !strings.Contains(resp.Content, "没有需要长期保存") {
		t.Fatalf("应如实说明没有抽取到事实: %q", resp.Content)
	}
}

func TestExecuteListAndForget(t *testing.T) {
	backend := &fakeBackend{records: []memory.Record{{ID: "m1", Memory: "旧偏好"}}}
	listResp := New(backend).Execute(context.Background(), req(map[string]any{"action": "list"}))
	if !listResp.Success || !strings.Contains(listResp.Content, "[m1]") {
		t.Fatalf("list 应带上 id 供 forget 使用: %+v", listResp)
	}

	forgetResp := New(backend).Execute(context.Background(), req(map[string]any{"action": "forget", "id": "m1"}))
	if !forgetResp.Success || backend.deleted != "m1" {
		t.Fatalf("forget 应删除指定 id: %+v / %q", forgetResp, backend.deleted)
	}
}

func TestExecuteArgumentValidation(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"缺少 action", map[string]any{}, "action"},
		{"未知 action", map[string]any{"action": "purge"}, "未知的 action"},
		{"search 缺 query", map[string]any{"action": "search"}, "query"},
		{"add 缺 content", map[string]any{"action": "add"}, "content"},
		{"forget 缺 id", map[string]any{"action": "forget"}, "id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := New(&fakeBackend{}).Execute(context.Background(), req(tc.args))
			if resp.Success {
				t.Fatalf("应当失败: %+v", resp)
			}
			if !strings.Contains(resp.Content+resp.Error, tc.want) {
				t.Fatalf("错误信息应提到 %q，实际 %q / %q", tc.want, resp.Content, resp.Error)
			}
		})
	}
}

func TestExecuteBackendErrorsAreReported(t *testing.T) {
	backend := &fakeBackend{searchErr: errors.New("服务不可达")}
	resp := New(backend).Execute(context.Background(), req(map[string]any{"action": "search", "query": "q"}))
	if resp.Success || !strings.Contains(resp.Content+resp.Error, "服务不可达") {
		t.Fatalf("后端错误应如实上报: %+v", resp)
	}
}

func TestExecuteWithoutBackendExplainsWhy(t *testing.T) {
	resp := New(nil).Execute(context.Background(), req(map[string]any{"action": "search", "query": "q"}))
	if resp.Success {
		t.Fatalf("未启用时应失败: %+v", resp)
	}
	// 错误信息必须告诉用户「为什么不能用、去哪开」，而不是一句 "unsupported"。
	if !strings.Contains(resp.Content+resp.Error, "memory.endpoint") {
		t.Fatalf("应指出配置位置: %q / %q", resp.Content, resp.Error)
	}
}

func TestExecuteRespectsCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	resp := New(&fakeBackend{}).Execute(ctx, req(map[string]any{"action": "list"}))
	if resp.Success {
		t.Fatalf("已取消的调用不应继续: %+v", resp)
	}
}
