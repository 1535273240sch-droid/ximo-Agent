package expert

import (
	"strings"
	"sync"
	"testing"
)

// TestDesignSystemRegistryLoadsAll 确认 151 套设计系统全部可加载。
func TestDesignSystemRegistryLoadsAll(t *testing.T) {
	r := NewDesignSystemRegistry()

	count, err := r.Count()
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if count != 151 {
		t.Fatalf("设计系统数 = %d, want 151（v1 design-systems 目录数量）", count)
	}
}

// TestDesignSystemMetadataComplete 确认每套都有 ID 与展示名。
func TestDesignSystemMetadataComplete(t *testing.T) {
	r := NewDesignSystemRegistry()
	list, err := r.Load()
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}

	seen := make(map[string]bool, len(list))
	for _, ds := range list {
		if ds.ID == "" {
			t.Error("设计系统缺少 ID")
		}
		if seen[ds.ID] {
			t.Errorf("ID 重复: %s", ds.ID)
		}
		seen[ds.ID] = true

		if ds.Name == "" {
			t.Errorf("%s 缺少展示名", ds.ID)
		}
		if !ds.HasDesign {
			t.Errorf("%s 缺少 DESIGN.md", ds.ID)
		}
		if !ds.HasTokens {
			t.Errorf("%s 缺少 tokens.css", ds.ID)
		}
	}
}

// TestDesignSystemLoadIsLazy 确认首次访问才读取目录。
func TestDesignSystemLoadIsLazy(t *testing.T) {
	r := NewDesignSystemRegistry()

	r.mu.RLock()
	before := len(r.systems)
	r.mu.RUnlock()
	if before != 0 {
		t.Fatal("构造后不应已加载（懒加载）")
	}

	if _, err := r.Load(); err != nil {
		t.Fatalf("加载失败: %v", err)
	}

	r.mu.RLock()
	after := len(r.systems)
	r.mu.RUnlock()
	if after == 0 {
		t.Fatal("访问后应已加载")
	}
}

// TestDesignSystemReadableAssets 确认能取出 DESIGN.md 与 tokens.css 内容。
func TestDesignSystemReadableAssets(t *testing.T) {
	r := NewDesignSystemRegistry()
	list, err := r.Load()
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if len(list) == 0 {
		t.Fatal("无设计系统")
	}

	target := list[0]

	doc, err := r.DesignDoc(target.ID)
	if err != nil {
		t.Fatalf("读取 DESIGN.md 失败: %v", err)
	}
	if !strings.Contains(doc, "# Design System") {
		t.Errorf("DESIGN.md 内容异常: %s", truncate(doc, 120))
	}

	tokens, err := r.TokensCSS(target.ID)
	if err != nil {
		t.Fatalf("读取 tokens.css 失败: %v", err)
	}
	if !strings.Contains(tokens, "--") && !strings.Contains(tokens, "design-systems") {
		t.Errorf("tokens.css 内容异常: %s", truncate(tokens, 120))
	}
}

// TestDesignSystemGetByID 确认按 ID 查找。
func TestDesignSystemGetByID(t *testing.T) {
	r := NewDesignSystemRegistry()
	list, _ := r.Load()

	target := list[0]
	got, ok := r.Get(target.ID)
	if !ok {
		t.Fatalf("应能查到 %s", target.ID)
	}
	if got.Name != target.Name {
		t.Fatalf("Name = %q, want %q", got.Name, target.Name)
	}

	if _, ok := r.Get("no-such-design-system"); ok {
		t.Fatal("不存在的 ID 不应命中")
	}
}

// TestDesignSystemCategories 确认分类统计可用。
func TestDesignSystemCategories(t *testing.T) {
	r := NewDesignSystemRegistry()
	cats, err := r.Categories()
	if err != nil {
		t.Fatalf("报错: %v", err)
	}
	if len(cats) == 0 {
		t.Fatal("应有分类")
	}

	total := 0
	for _, n := range cats {
		total += n
	}
	count, _ := r.Count()
	if total != count {
		t.Fatalf("分类计数合计 %d != 总数 %d", total, count)
	}
}

// TestDesignSystemListByCategory 确认按分类筛选。
func TestDesignSystemListByCategory(t *testing.T) {
	r := NewDesignSystemRegistry()
	cats, _ := r.Categories()

	var someCategory string
	for cat := range cats {
		someCategory = cat
		break
	}

	list, err := r.ListByCategory(someCategory)
	if err != nil {
		t.Fatalf("报错: %v", err)
	}
	if len(list) == 0 {
		t.Fatalf("分类 %q 应有条目", someCategory)
	}
	for _, ds := range list {
		if ds.Category != someCategory {
			t.Fatalf("筛选结果含其他分类: %s (%s)", ds.ID, ds.Category)
		}
	}
}

// TestValidAssetIDRejectsTraversal 确认路径穿越被拒绝。
//
// embed.FS 本身也不允许 .. 路径，但显式校验让错误信息更清晰，
// 且不依赖底层实现细节 —— 这是安全边界，不留给「应该会拦」的假设。
func TestValidAssetIDRejectsTraversal(t *testing.T) {
	bad := []string{"", "..", "../etc", "a/b", "a\\b", "a..b/../c", "design system", "设计"}
	for _, id := range bad {
		if validAssetID(id) {
			t.Errorf("validAssetID(%q) 应为 false", id)
		}
	}

	good := []string{"linear-app", "apple", "x-ai", "openai", "a_b", "abc123"}
	for _, id := range good {
		if !validAssetID(id) {
			t.Errorf("validAssetID(%q) 应为 true", id)
		}
	}

	// 通过公开 API 也不能穿越。
	r := NewDesignSystemRegistry()
	if _, err := r.DesignDoc("../secret"); err == nil {
		t.Fatal("路径穿越应被拒绝")
	}
}

// TestDesignSystemConcurrentLoad 确认并发加载安全（配合 -race）。
func TestDesignSystemConcurrentLoad(t *testing.T) {
	r := NewDesignSystemRegistry()

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
		if n != 151 {
			t.Fatalf("goroutine %d 得到 %d，want 151", i, n)
		}
	}
}
