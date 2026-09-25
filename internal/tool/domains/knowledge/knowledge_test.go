package knowledge

import (
	"context"
	"strings"
	"testing"

	"github.com/ximo888ok-netizen/ximo-agent/internal/tool"
)

func exec(t *testing.T, tl tool.Tool, args map[string]any, mode tool.Mode) tool.ToolResponse {
	t.Helper()
	return tl.Execute(context.Background(), tool.ToolRequest{
		RunID: "r", ToolCallID: "c", Name: tl.Definition().Name, Arguments: args, Mode: mode,
	})
}

func TestKnowledgeAddSearchListUpdateDelete(t *testing.T) {
	repo := NewMemoryRepository()
	tl := New(repo)

	// 添加
	resp := exec(t, tl, map[string]any{"action": "add", "title": "Go 并发模式", "content": "Go 的 goroutine 与 channel 是并发基础", "tags": []any{"go", "并发"}}, tool.ModeCoding)
	if !resp.Success {
		t.Fatalf("添加失败: %+v", resp)
	}
	resp = exec(t, tl, map[string]any{"action": "add", "title": "Python 虚拟环境", "content": "使用 venv 管理 Python 依赖", "tags": []any{"python"}}, tool.ModeCoding)
	if !resp.Success {
		t.Fatalf("添加失败: %+v", resp)
	}

	// 搜索（英文关键词）
	resp = exec(t, tl, map[string]any{"action": "search", "query": "goroutine"}, tool.ModeCoding)
	if !resp.Success || !strings.Contains(resp.Content, "Go 并发模式") {
		t.Fatalf("搜索 goroutine = %s", resp.Content)
	}
	if strings.Contains(resp.Content, "Python 虚拟环境") {
		t.Fatalf("搜索结果不应包含无关条目: %s", resp.Content)
	}

	// 搜索（中文 bigram）
	resp = exec(t, tl, map[string]any{"action": "search", "query": "并发"}, tool.ModeCoding)
	if !resp.Success || !strings.Contains(resp.Content, "Go 并发模式") {
		t.Fatalf("中文搜索 = %s", resp.Content)
	}

	// 浏览
	resp = exec(t, tl, map[string]any{"action": "list"}, tool.ModeCoding)
	if !resp.Success || !strings.Contains(resp.Content, "共 2 条") {
		t.Fatalf("浏览 = %s", resp.Content)
	}

	// 模式隔离：office 模式看不到 coding 的条目
	resp = exec(t, tl, map[string]any{"action": "list"}, tool.ModeOffice)
	if !resp.Success || !strings.Contains(resp.Content, "知识库为空") {
		t.Fatalf("office 模式应为空: %s", resp.Content)
	}

	// 更新
	resp = exec(t, tl, map[string]any{"action": "update", "id": "k-1", "title": "Go 并发模式（修订）"}, tool.ModeCoding)
	if !resp.Success {
		t.Fatalf("更新失败: %+v", resp)
	}
	resp = exec(t, tl, map[string]any{"action": "search", "query": "修订"}, tool.ModeCoding)
	if !resp.Success || !strings.Contains(resp.Content, "Go 并发模式（修订）") {
		t.Fatalf("更新后搜索 = %s", resp.Content)
	}

	// 空更新被拒绝
	resp = exec(t, tl, map[string]any{"action": "update", "id": "k-1"}, tool.ModeCoding)
	if resp.Success {
		t.Fatalf("空更新应当被拒绝")
	}

	// 删除
	resp = exec(t, tl, map[string]any{"action": "delete", "id": "k-2"}, tool.ModeCoding)
	if !resp.Success {
		t.Fatalf("删除失败: %+v", resp)
	}
	resp = exec(t, tl, map[string]any{"action": "list"}, tool.ModeCoding)
	if !resp.Success || !strings.Contains(resp.Content, "共 1 条") {
		t.Fatalf("删除后浏览 = %s", resp.Content)
	}

	// 删除不存在条目
	resp = exec(t, tl, map[string]any{"action": "delete", "id": "k-999"}, tool.ModeCoding)
	if resp.Success {
		t.Fatalf("删除不存在条目应当失败")
	}
}

func TestKnowledgeValidation(t *testing.T) {
	tl := New(NewMemoryRepository())
	cases := []map[string]any{
		{"action": "add", "title": "", "content": "x"}, // 空标题
		{"action": "add", "title": "t", "content": ""}, // 空内容
		{"action": "search"},                           // 空 query
		{"action": "bogus"},                            // 未知动作
		{"action": "update", "title": "x"},             // 缺 id
		{"action": "delete"},                           // 缺 id
	}
	for i, args := range cases {
		resp := exec(t, tl, args, tool.ModeCoding)
		if resp.Success {
			t.Fatalf("用例 %d (%v) 应当失败", i, args)
		}
	}
}

func TestKnowledgeSearchPagination(t *testing.T) {
	repo := NewMemoryRepository()
	tl := New(repo)
	for i := 0; i < 25; i++ {
		resp := exec(t, tl, map[string]any{"action": "add", "title": "条目", "content": "共享关键词 内容"}, tool.ModeCoding)
		if !resp.Success {
			t.Fatalf("添加失败: %+v", resp)
		}
	}
	resp := exec(t, tl, map[string]any{"action": "search", "query": "共享关键词", "page": 1, "page_size": 10}, tool.ModeCoding)
	if !resp.Success || !strings.Contains(resp.Content, "共 25 条结果") {
		t.Fatalf("第一页 = %s", resp.Content)
	}
	resp = exec(t, tl, map[string]any{"action": "search", "query": "共享关键词", "page": 3, "page_size": 10}, tool.ModeCoding)
	if !resp.Success || !strings.Contains(resp.Content, "共 25 条结果") {
		t.Fatalf("第三页 = %s", resp.Content)
	}
	// 超出范围页码
	resp = exec(t, tl, map[string]any{"action": "search", "query": "共享关键词", "page": 99, "page_size": 10}, tool.ModeCoding)
	if !resp.Success || !strings.Contains(resp.Content, "未找到") {
		t.Fatalf("超范围页码 = %s", resp.Content)
	}
}

func TestKnowledgeToolDefinition(t *testing.T) {
	tl := New(NewMemoryRepository())
	def := tl.Definition()
	if def.Name != "knowledge" {
		t.Fatalf("名称 = %q", def.Name)
	}
	if def.Idempotency != tool.ClassIdempotent || def.Domain != tool.DomainInProcess {
		t.Fatalf("定义 = %+v", def)
	}
}

func TestTokenizer(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"hello world", 2},
		{"Hello, World!", 2},
		{"goroutine 并发 channel", 3}, // 英文 x2 + 2 字中文 1 个 bigram
		{"", 0},
		{"a-b_c", 3},
	}
	for _, c := range cases {
		got := tokenize(c.in)
		if c.want > 0 && len(got) != c.want {
			t.Errorf("tokenize(%q) = %v（%d），期望 %d 个", c.in, got, len(got), c.want)
		}
	}
	// 中文二元组：并发编程 -> 并发/发编/编程
	got := tokenize("并发编程")
	if len(got) != 3 || got[0] != "并发" || got[1] != "发编" || got[2] != "编程" {
		t.Fatalf("中文分词 = %v", got)
	}
	// 单字中文
	if got := tokenize("并"); len(got) != 1 || got[0] != "并" {
		t.Fatalf("单字中文 = %v", got)
	}
}

func TestBM25Ranking(t *testing.T) {
	repo := NewMemoryRepository()
	ctx := context.Background()
	// 一个文档多次出现关键词应当排更前
	repo.Add(ctx, "coding", Entry{ID: "1", Title: "Go", Content: "goroutine goroutine goroutine"})
	repo.Add(ctx, "coding", Entry{ID: "2", Title: "Other", Content: "goroutine once"})
	results, err := repo.Search(ctx, "coding", "goroutine", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("结果数 = %d", len(results))
	}
	if results[0].Entry.ID != "1" {
		t.Fatalf("BM25 排序错误：首位 = %s", results[0].Entry.ID)
	}
	if results[0].Score <= results[1].Score {
		t.Fatalf("得分应递减: %v / %v", results[0].Score, results[1].Score)
	}
}
