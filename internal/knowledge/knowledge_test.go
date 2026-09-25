package knowledge

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 条目模型
// ---------------------------------------------------------------------------

// TestNewEntryNormalizes 确认条目构造时归一化。
func TestNewEntryNormalizes(t *testing.T) {
	e := NewEntry("  标题  ", "  正文  ", []string{" tag1 ", "", "tag2", "tag1"}, "")

	if e.Title != "标题" {
		t.Errorf("Title = %q，应 trim", e.Title)
	}
	if e.Content != "正文" {
		t.Errorf("Content = %q，应 trim", e.Content)
	}
	if len(e.Tags) != 2 {
		t.Errorf("Tags = %v，应去空去重", e.Tags)
	}
	if e.Source != "agent" {
		t.Errorf("Source = %q，应默认 agent", e.Source)
	}
	if e.ID == "" {
		t.Error("应生成 ID")
	}
	if e.CreatedAt == 0 || e.UpdatedAt == 0 {
		t.Error("应设置时间戳")
	}
}

// TestEntryValidate 确认必填校验。
func TestEntryValidate(t *testing.T) {
	if err := NewEntry("标题", "正文", nil, "").Validate(); err != nil {
		t.Errorf("合法条目不应报错: %v", err)
	}
	if err := NewEntry("", "正文", nil, "").Validate(); err == nil {
		t.Error("空标题应报错")
	}
	if err := NewEntry("标题", "  ", nil, "").Validate(); err == nil {
		t.Error("空正文应报错")
	}
}

// TestEntryTruncatesOversize 确认超长字段被截断（v1 的 200/4000 约束）。
func TestEntryTruncatesOversize(t *testing.T) {
	longTitle := strings.Repeat("标", 500)
	longContent := strings.Repeat("文", 9000)
	e := NewEntry(longTitle, longContent, nil, "")

	if n := len([]rune(e.Title)); n > maxTitleLen {
		t.Errorf("标题长度 %d 超上限 %d", n, maxTitleLen)
	}
	if n := len([]rune(e.Content)); n > maxContentLen {
		t.Errorf("正文长度 %d 超上限 %d", n, maxContentLen)
	}
}

// TestEntryCapsTags 确认标签数上限为 5（v1 约束）。
func TestEntryCapsTags(t *testing.T) {
	tags := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	e := NewEntry("t", "c", tags, "")
	if len(e.Tags) > maxTags {
		t.Fatalf("标签数 %d 超上限 %d", len(e.Tags), maxTags)
	}
}

// TestGenIDUnique 确认 ID 生成不重复。
func TestGenIDUnique(t *testing.T) {
	const n = 5000
	seen := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		id := genID()
		if seen[id] {
			t.Fatalf("ID 重复: %s", id)
		}
		seen[id] = true
	}
}

// ---------------------------------------------------------------------------
// BM25 检索
// ---------------------------------------------------------------------------

// TestTokenizeHandlesChineseAndLatin 确认中英混合分词。
func TestTokenizeHandlesChineseAndLatin(t *testing.T) {
	tokens := tokenize("修复 TypeScript 的类型错误")

	if len(tokens) == 0 {
		t.Fatal("应切出词元")
	}
	// 拉丁词应整体保留。
	if !containsStr(tokens, "typescript") {
		t.Errorf("应含 typescript 词元: %v", tokens)
	}
	// 中文应产生 bigram。
	if !containsStr(tokens, "修复") {
		t.Errorf("应含「修复」bigram: %v", tokens)
	}
}

// TestTokenizeEmptyInput 确认空输入不 panic。
func TestTokenizeEmptyInput(t *testing.T) {
	if got := tokenize(""); got != nil {
		t.Fatalf("空输入应返回 nil，实际 %v", got)
	}
	if got := tokenize("   \n\t "); len(got) != 0 {
		t.Fatalf("纯空白应无词元，实际 %v", got)
	}
}

// TestBM25RanksRelevantFirst 确认 BM25 把相关文档排在前面。
func TestBM25RanksRelevantFirst(t *testing.T) {
	ix := newIndex()
	ix.add("d1", "TypeScript 类型错误修复")
	ix.add("d2", "今天天气很好适合出门散步")
	ix.add("d3", "TypeScript 编译配置说明")

	got := ix.search("TypeScript 类型错误", 10)
	if len(got) == 0 {
		t.Fatal("应有检索结果")
	}
	if got[0].id != "d1" {
		t.Fatalf("最相关应为 d1，实际 %s（结果: %+v）", got[0].id, got)
	}
}

// TestBM25ExcludesIrrelevant 确认无关文档不出现在结果里。
func TestBM25ExcludesIrrelevant(t *testing.T) {
	ix := newIndex()
	ix.add("d1", "Go 语言并发编程")
	ix.add("d2", "完全无关的另一段内容")

	got := ix.search("Go 并发", 10)
	for _, r := range got {
		if r.id == "d2" {
			t.Fatal("无关文档不应命中")
		}
	}
}

// TestBM25RemoveStopsMatching 确认删除后不再被检索到（索引同步正确）。
func TestBM25RemoveStopsMatching(t *testing.T) {
	ix := newIndex()
	ix.add("d1", "独特的检索关键词 zzunique")
	ix.add("d2", "其他内容")

	if len(ix.search("zzunique", 10)) == 0 {
		t.Fatal("删除前应能检索到")
	}

	ix.remove("d1")

	if len(ix.search("zzunique", 10)) != 0 {
		t.Fatal("删除后不应再检索到（索引未同步）")
	}
	if ix.Len() != 1 {
		t.Fatalf("文档数 = %d, want 1", ix.Len())
	}
}

// TestBM25ReAddUpdatesIndex 确认同 ID 重新加入会更新索引（内容变更场景）。
func TestBM25ReAddUpdatesIndex(t *testing.T) {
	ix := newIndex()
	ix.add("d1", "旧内容 oldkeyword")
	ix.add("d1", "新内容 newkeyword")

	if len(ix.search("oldkeyword", 10)) != 0 {
		t.Fatal("重新加入后旧词不应命中")
	}
	if len(ix.search("newkeyword", 10)) == 0 {
		t.Fatal("新词应命中")
	}
	if ix.Len() != 1 {
		t.Fatalf("文档数 = %d, want 1（同 ID 应覆盖）", ix.Len())
	}
}

// TestBM25StableOrdering 确认同分时结果顺序稳定（分页一致性依赖它）。
func TestBM25StableOrdering(t *testing.T) {
	ix := newIndex()
	ix.add("b", "相同关键词 same keyword")
	ix.add("a", "相同关键词 same keyword")
	ix.add("c", "相同关键词 same keyword")

	first := ix.search("相同关键词", 10)
	second := ix.search("相同关键词", 10)

	if len(first) != len(second) {
		t.Fatal("两次检索结果数不一致")
	}
	for i := range first {
		if first[i].id != second[i].id {
			t.Fatalf("结果顺序不稳定: %v vs %v", first, second)
		}
	}
}

// ---------------------------------------------------------------------------
// Store 门面
// ---------------------------------------------------------------------------

func newTestStore(t *testing.T) *Store {
	t.Helper()
	return NewStore(StoreOptions{
		MaxEntriesPerMode: 1000,
		MaxCachedModes:    3,
	})
}

// TestStoreAddAndSearch 确认添加后可检索。
func TestStoreAddAndSearch(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	entry, err := s.Add(ctx, ModeCoding, "修复 ESLint 报错", "需要在 tsconfig 中调整 no-explicit-any", []string{"typescript", "lint"}, "agent")
	if err != nil {
		t.Fatalf("Add 报错: %v", err)
	}
	if entry.ID == "" {
		t.Fatal("应返回带 ID 的条目")
	}

	resp, err := s.Search(ctx, ModeCoding, "ESLint", 1, 10)
	if err != nil {
		t.Fatalf("Search 报错: %v", err)
	}
	if resp.Total != 1 {
		t.Fatalf("命中数 = %d, want 1", resp.Total)
	}
	if resp.Results[0].ID != entry.ID {
		t.Fatalf("命中的是 %s，want %s", resp.Results[0].ID, entry.ID)
	}
	if resp.Results[0].Score <= 0 {
		t.Fatal("相关性分数应为正")
	}
}

// TestStoreRejectsInvalidInput 确认入参校验。
func TestStoreRejectsInvalidInput(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.Add(ctx, ModeCoding, "", "正文", nil, ""); err == nil {
		t.Error("空标题应报错")
	}
	if _, err := s.Add(ctx, ModeCoding, "标题", "", nil, ""); err == nil {
		t.Error("空正文应报错")
	}
	if _, err := s.Search(ctx, ModeCoding, "", 1, 10); err == nil {
		t.Error("空 query 应报错")
	}
}

// TestStoreModesAreIsolated 确认各模式知识库互相独立（v1 设计）。
func TestStoreModesAreIsolated(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.Add(ctx, ModeCoding, "代码知识", "关于代码", nil, ""); err != nil {
		t.Fatalf("Add 报错: %v", err)
	}
	if _, err := s.Add(ctx, ModeDesign, "设计知识", "关于设计", nil, ""); err != nil {
		t.Fatalf("Add 报错: %v", err)
	}

	codingCount, _ := s.Count(ctx, ModeCoding)
	designCount, _ := s.Count(ctx, ModeDesign)
	officeCount, _ := s.Count(ctx, ModeOffice)

	if codingCount != 1 || designCount != 1 {
		t.Fatalf("coding=%d design=%d, want 1/1", codingCount, designCount)
	}
	if officeCount != 0 {
		t.Fatalf("office 应为空，实际 %d", officeCount)
	}

	// coding 模式不应搜到 design 的知识。
	resp, _ := s.Search(ctx, ModeCoding, "设计", 1, 10)
	if resp.Total != 0 {
		t.Fatalf("跨模式串味：coding 搜到了 %d 条", resp.Total)
	}
}

// TestStoreUpdate 确认更新生效并刷新索引。
func TestStoreUpdate(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	entry, _ := s.Add(ctx, ModeOffice, "原标题", "原内容 with oldterm", nil, "")

	updated, ok, err := s.Update(ctx, ModeOffice, entry.ID, "新标题", "新内容 with newterm", []string{"new"}, "user")
	if err != nil {
		t.Fatalf("Update 报错: %v", err)
	}
	if !ok {
		t.Fatal("应更新成功")
	}
	if updated.Title != "新标题" {
		t.Fatalf("Title = %q", updated.Title)
	}
	if updated.UpdatedAt < entry.UpdatedAt {
		t.Fatal("UpdatedAt 应更新")
	}
	if updated.CreatedAt != entry.CreatedAt {
		t.Fatal("CreatedAt 不应变")
	}

	// 索引应已刷新。
	old, _ := s.Search(ctx, ModeOffice, "oldterm", 1, 10)
	if old.Total != 0 {
		t.Fatal("旧内容不应再被检索到")
	}
	fresh, _ := s.Search(ctx, ModeOffice, "newterm", 1, 10)
	if fresh.Total != 1 {
		t.Fatal("新内容应可检索")
	}
}

// TestStoreUpdateNonexistent 确认更新不存在的条目返回 false（而非报错）。
func TestStoreUpdateNonexistent(t *testing.T) {
	s := newTestStore(t)
	_, ok, err := s.Update(context.Background(), ModeOffice, "no-such-id", "t", "c", nil, "")
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if ok {
		t.Fatal("不存在的条目应返回 false")
	}
}

// TestStoreDelete 确认删除后条目与索引同步移除。
func TestStoreDelete(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	e1, _ := s.Add(ctx, ModeOffice, "保留", "保留内容 keepterm", nil, "")
	e2, _ := s.Add(ctx, ModeOffice, "删除", "删除内容 delterm", nil, "")

	ok, err := s.Delete(ctx, ModeOffice, e2.ID)
	if err != nil {
		t.Fatalf("Delete 报错: %v", err)
	}
	if !ok {
		t.Fatal("应删除成功")
	}

	count, _ := s.Count(ctx, ModeOffice)
	if count != 1 {
		t.Fatalf("删除后应剩 1 条，实际 %d", count)
	}

	// 被删条目不应再搜到。
	gone, _ := s.Search(ctx, ModeOffice, "delterm", 1, 10)
	if gone.Total != 0 {
		t.Fatal("被删条目不应再被检索到")
	}

	// 剩余条目的索引应完好（验证删除后 id→下标映射重建正确）。
	kept, _ := s.Search(ctx, ModeOffice, "keepterm", 1, 10)
	if kept.Total != 1 {
		t.Fatal("剩余条目应仍可检索（删除后映射重建有误）")
	}
	if kept.Results[0].ID != e1.ID {
		t.Fatalf("剩余条目 ID = %s, want %s", kept.Results[0].ID, e1.ID)
	}
}

// TestStoreDeleteNonexistent 确认删除不存在的条目返回 false。
func TestStoreDeleteNonexistent(t *testing.T) {
	s := newTestStore(t)
	ok, err := s.Delete(context.Background(), ModeOffice, "nope")
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if ok {
		t.Fatal("应返回 false")
	}
}

// TestStoreListSortedByUpdatedAt 确认列表按 updatedAt 降序（v1 同款）。
func TestStoreListSortedByUpdatedAt(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	var ids []string
	for i := 0; i < 5; i++ {
		e, _ := s.Add(ctx, ModeOffice, fmt.Sprintf("条目 %d", i), "内容", nil, "")
		ids = append(ids, e.ID)
		time.Sleep(2 * time.Millisecond) // 确保时间戳有区分
	}

	// 更新最早的那条 → 它应排到最前。
	if _, _, err := s.Update(ctx, ModeOffice, ids[0], "更新后的标题", "内容", nil, ""); err != nil {
		t.Fatalf("Update 报错: %v", err)
	}

	resp, err := s.List(ctx, ModeOffice, 1, 10)
	if err != nil {
		t.Fatalf("List 报错: %v", err)
	}
	if resp.Total != 5 {
		t.Fatalf("总数 = %d, want 5", resp.Total)
	}
	if resp.Items[0].ID != ids[0] {
		t.Fatalf("最近更新的应排最前，实际 %s", resp.Items[0].ID)
	}
}

// TestStorePagination 确认分页正确。
func TestStorePagination(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	for i := 0; i < 25; i++ {
		if _, err := s.Add(ctx, ModeOffice, fmt.Sprintf("条目 %d", i), "共同内容 commonterm", nil, ""); err != nil {
			t.Fatalf("Add 报错: %v", err)
		}
	}

	// 搜索分页。
	page1, _ := s.Search(ctx, ModeOffice, "commonterm", 1, 10)
	if len(page1.Results) != 10 || page1.Total != 25 || page1.TotalPages != 3 {
		t.Fatalf("第 1 页: results=%d total=%d pages=%d", len(page1.Results), page1.Total, page1.TotalPages)
	}

	page3, _ := s.Search(ctx, ModeOffice, "commonterm", 3, 10)
	if len(page3.Results) != 5 {
		t.Fatalf("第 3 页应有 5 条，实际 %d", len(page3.Results))
	}

	// 越界页应返回空而非报错。
	page99, err := s.Search(ctx, ModeOffice, "commonterm", 99, 10)
	if err != nil {
		t.Fatalf("越界页不应报错: %v", err)
	}
	if len(page99.Results) != 0 {
		t.Fatalf("越界页应返回空，实际 %d 条", len(page99.Results))
	}

	// 列表分页。
	list1, _ := s.List(ctx, ModeOffice, 1, 10)
	if len(list1.Items) != 10 || list1.TotalPages != 3 {
		t.Fatalf("列表第 1 页: items=%d pages=%d", len(list1.Items), list1.TotalPages)
	}
}

// TestStoreEntryCapEvictsOldest 确认条目数上限触发淘汰最旧（内存治理）。
//
// 这是验收标准「72h 无持续内存增长」在知识库侧的断言。
func TestStoreEntryCapEvictsOldest(t *testing.T) {
	const cap = 20
	s := NewStore(StoreOptions{MaxEntriesPerMode: cap, MaxCachedModes: 3})
	ctx := context.Background()

	for i := 0; i < cap*5; i++ {
		if _, err := s.Add(ctx, ModeOffice, fmt.Sprintf("条目 %d", i), "内容", nil, ""); err != nil {
			t.Fatalf("Add 报错: %v", err)
		}
		time.Sleep(time.Millisecond) // 让 updatedAt 有区分度
	}

	count, _ := s.Count(ctx, ModeOffice)
	if count > cap {
		t.Fatalf("条目数 %d 超上限 %d（知识库存在无限增长）", count, cap)
	}
}

// TestStoreModeCacheEviction 确认有持久层时，常驻模式数有界且数据不丢。
//
// 淘汰的前提是有持久层可重建 —— 无持久层时淘汰等于丢数据，因此不做淘汰
// （见 store.go evictModesLocked 的说明）。
func TestStoreModeCacheEviction(t *testing.T) {
	persist := newMemPersistence()
	s := NewStore(StoreOptions{
		MaxEntriesPerMode: 100,
		MaxCachedModes:    2, // 只有 3 个模式，强制淘汰
		Persistence:       persist,
	})
	ctx := context.Background()

	// 依次访问三个模式。
	for _, m := range []Mode{ModeOffice, ModeCoding, ModeDesign} {
		if _, err := s.Add(ctx, m, "标题", "内容", nil, ""); err != nil {
			t.Fatalf("Add 报错: %v", err)
		}
	}

	st := s.Stats()
	if st.CachedModes > 2 {
		t.Fatalf("常驻模式数 %d 超上限 2", st.CachedModes)
	}

	// 被淘汰模式的数据不应丢失 —— 再次访问应从持久层重建。
	count, err := s.Count(ctx, ModeOffice)
	if err != nil {
		t.Fatalf("Count 报错: %v", err)
	}
	if count != 1 {
		t.Fatalf("淘汰后重载应仍有 1 条，实际 %d", count)
	}
}

// TestStoreNoEvictionWithoutPersistence 确认无持久层时不淘汰（避免丢数据）。
func TestStoreNoEvictionWithoutPersistence(t *testing.T) {
	s := NewStore(StoreOptions{MaxEntriesPerMode: 100, MaxCachedModes: 1})
	ctx := context.Background()

	for _, m := range []Mode{ModeOffice, ModeCoding, ModeDesign} {
		if _, err := s.Add(ctx, m, "标题", "内容", nil, ""); err != nil {
			t.Fatalf("Add 报错: %v", err)
		}
	}

	// 三个模式的数据都应还在。
	for _, m := range []Mode{ModeOffice, ModeCoding, ModeDesign} {
		count, err := s.Count(ctx, m)
		if err != nil {
			t.Fatalf("Count 报错: %v", err)
		}
		if count != 1 {
			t.Fatalf("%s 模式数据丢失（无持久层时不应淘汰）", m)
		}
	}
}

// TestStorePersistsAcrossReload 确认持久层往返正确。
func TestStorePersistsAcrossReload(t *testing.T) {
	persist := newMemPersistence()
	ctx := context.Background()

	s1 := NewStore(StoreOptions{MaxEntriesPerMode: 100, MaxCachedModes: 1, Persistence: persist})
	entry, err := s1.Add(ctx, ModeCoding, "持久化测试", "内容 persistterm", []string{"t"}, "user")
	if err != nil {
		t.Fatalf("Add 报错: %v", err)
	}

	// 新建 Store（模拟进程重启）读同一持久层。
	s2 := NewStore(StoreOptions{MaxEntriesPerMode: 100, MaxCachedModes: 1, Persistence: persist})
	resp, err := s2.Search(ctx, ModeCoding, "persistterm", 1, 10)
	if err != nil {
		t.Fatalf("Search 报错: %v", err)
	}
	if resp.Total != 1 {
		t.Fatalf("重启后应能检索到，实际 %d 条", resp.Total)
	}
	if resp.Results[0].ID != entry.ID {
		t.Fatalf("ID = %s, want %s", resp.Results[0].ID, entry.ID)
	}
	if len(resp.Results[0].Tags) != 1 || resp.Results[0].Tags[0] != "t" {
		t.Fatalf("标签未正确持久化: %v", resp.Results[0].Tags)
	}
}

// TestStorePersistenceFailureSurfaces 确认持久层故障会报错（不静默丢数据）。
func TestStorePersistenceFailureSurfaces(t *testing.T) {
	s := NewStore(StoreOptions{
		MaxEntriesPerMode: 100,
		MaxCachedModes:    1,
		Persistence:       failingPersistence{},
	})
	if _, err := s.Add(context.Background(), ModeOffice, "标题", "内容", nil, ""); err == nil {
		t.Fatal("持久层故障应向上报错")
	}
}

// TestNormalizeMode 确认非法模式回退 office（v1 默认值）。
func TestNormalizeMode(t *testing.T) {
	cases := map[Mode]Mode{
		ModeOffice: ModeOffice,
		ModeCoding: ModeCoding,
		ModeDesign: ModeDesign,
		"invalid":  ModeOffice,
		"":         ModeOffice,
	}
	for in, want := range cases {
		if got := NormalizeMode(in); got != want {
			t.Errorf("NormalizeMode(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestStoreConcurrentAccess 确认并发读写安全（配合 -race）。
func TestStoreConcurrentAccess(t *testing.T) {
	s := NewStore(StoreOptions{MaxEntriesPerMode: 500, MaxCachedModes: 3})
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			mode := []Mode{ModeOffice, ModeCoding, ModeDesign}[i%3]
			for j := 0; j < 60; j++ {
				entry, err := s.Add(ctx, mode, fmt.Sprintf("标题 %d-%d", i, j), "共同内容 shared", nil, "")
				if err != nil {
					continue
				}
				_, _ = s.Search(ctx, mode, "shared", 1, 5)
				_, _ = s.List(ctx, mode, 1, 5)
				_, _, _ = s.Update(ctx, mode, entry.ID, fmt.Sprintf("更新 %d", j), "共同内容 shared", nil, "")
				_, _ = s.Count(ctx, mode)
				_ = s.Stats()
			}
		}(i)
	}
	wg.Wait()

	st := s.Stats()
	if st.CachedModes > 3 {
		t.Fatalf("并发后常驻模式数 %d 超上限", st.CachedModes)
	}
}

// TestStoreStatsAccurate 确认统计上报。
func TestStoreStatsAccurate(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if _, err := s.Add(ctx, ModeOffice, fmt.Sprintf("t%d", i), "内容", nil, ""); err != nil {
			t.Fatalf("Add 报错: %v", err)
		}
	}

	st := s.Stats()
	if st.TotalItems != 5 {
		t.Fatalf("TotalItems = %d, want 5", st.TotalItems)
	}
	if st.TotalTerms == 0 {
		t.Fatal("应统计索引词表大小")
	}
	if st.MaxCached != 3 {
		t.Fatalf("MaxCached = %d, want 3", st.MaxCached)
	}
	if st.Misses == 0 {
		t.Fatal("首次加载应记为 miss")
	}
}

// TestStoreNoUnboundedGrowthSoak 是 72h soak 的加速替代：
// 大量增删后断言内存态指标收敛。
func TestStoreNoUnboundedGrowthSoak(t *testing.T) {
	const cap = 200
	persist := newMemPersistence()
	s := NewStore(StoreOptions{MaxEntriesPerMode: cap, MaxCachedModes: 2, Persistence: persist})
	ctx := context.Background()

	modes := []Mode{ModeOffice, ModeCoding, ModeDesign}
	for i := 0; i < 20_000; i++ {
		mode := modes[i%len(modes)]
		if _, err := s.Add(ctx, mode, fmt.Sprintf("条目 %d", i), "soak 内容 sharedterm", nil, ""); err != nil {
			t.Fatalf("Add 报错: %v", err)
		}
		if i%500 == 0 {
			_, _ = s.Search(ctx, mode, "sharedterm", 1, 5)
		}
	}

	st := s.Stats()
	if st.CachedModes > 2 {
		t.Fatalf("soak 后常驻模式 %d 超上限 2", st.CachedModes)
	}
	// 每个模式都被上限约束（不是只约束当前常驻的那个）。
	for _, m := range modes {
		count, err := s.Count(ctx, m)
		if err != nil {
			t.Fatalf("Count 报错: %v", err)
		}
		if count > cap {
			t.Fatalf("%s 模式条目数 %d 超上限 %d", m, count, cap)
		}
	}
	t.Logf("soak 完成：20000 次写入后 cached_modes=%d total_items=%d total_terms=%d hits=%d misses=%d",
		st.CachedModes, st.TotalItems, st.TotalTerms, st.Hits, st.Misses)
}

// --- 测试辅助 ---

// memPersistence 内存持久层（模拟任务 03 的存储实现）。
type memPersistence struct {
	mu   sync.Mutex
	data map[Mode][]Entry
}

func newMemPersistence() *memPersistence {
	return &memPersistence{data: make(map[Mode][]Entry)}
}

func (p *memPersistence) Load(_ context.Context, mode Mode) ([]Entry, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	src := p.data[mode]
	out := make([]Entry, len(src))
	copy(out, src)
	return out, nil
}

func (p *memPersistence) Save(_ context.Context, mode Mode, entries []Entry) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	cp := make([]Entry, len(entries))
	copy(cp, entries)
	p.data[mode] = cp
	return nil
}

// failingPersistence 模拟持久层故障。
type failingPersistence struct{}

func (failingPersistence) Load(context.Context, Mode) ([]Entry, error) {
	return nil, errPersist
}
func (failingPersistence) Save(context.Context, Mode, []Entry) error { return errPersist }

var errPersist = &persistError{}

type persistError struct{}

func (e *persistError) Error() string { return "persist failure" }

func containsStr(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
