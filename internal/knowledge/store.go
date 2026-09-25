// store.go —— 知识库存储与检索门面（v1 src/main/KnowledgeStore.ts 的 Go 对等实现）。
//
// 与 v1 的差异（内存治理，对应验收标准「72h soak 无持续内存增长」）：
//
//	v1 把每个模式的全部条目常驻内存（`stores` map + Orama 索引），条目只增不减，
//	且 `stores` 从不清理 —— 长期运行下这是知识库侧的内存增长点。
//
// v2 的约束：
//   - 每个模式的条目数有上限（MaxEntriesPerMode），超限按 updatedAt 淘汰最旧
//   - 每个模式的索引缓存有上限（MaxCachedModes），超过则 LRU 淘汰整个模式的索引，
//     下次访问时从持久层重建 —— 换来「同时常驻内存的模式数有界」
//   - 全部条目的持久化委托给 Persistence 接口（任务 03 的存储层实现），
//     本包不直接操作文件系统
package knowledge

import (
	"container/list"
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Persistence 知识条目的持久化接口（由任务 03 的存储层实现）。
//
// 本包不自己读写文件 —— 存储布局由任务 03 统一裁决，避免两处各写一份格式。
type Persistence interface {
	// Load 读取某模式的全部条目。
	Load(ctx context.Context, mode Mode) ([]Entry, error)
	// Save 覆盖写入某模式的全部条目。
	Save(ctx context.Context, mode Mode, entries []Entry) error
}

// StoreOptions 知识库参数。
type StoreOptions struct {
	// MaxEntriesPerMode 单模式条目上限（0 用默认 5000）。
	// 超限时淘汰 updatedAt 最旧的条目 —— 知识库是「常用常新」的，
	// 最旧的条目价值最低。
	MaxEntriesPerMode int
	// MaxCachedModes 同时常驻内存的模式索引上限（0 用默认 3，即全部模式）。
	MaxCachedModes int
	// Persistence 持久化实现；nil 表示纯内存（测试用）。
	Persistence Persistence
	// now 注入时钟。
	now func() time.Time
}

// DefaultStoreOptions 默认参数。
func DefaultStoreOptions() StoreOptions {
	return StoreOptions{
		MaxEntriesPerMode: 5000,
		MaxCachedModes:    3,
	}
}

// modeCache 单个模式的内存态：条目列表 + 倒排索引 + id 排序视图。
type modeCache struct {
	mode    Mode
	entries []Entry
	byID    map[string]int // Entry.ID → entries 下标
	index   *index
	elem    *list.Element // 用于模式级 LRU
}

// Store 知识库门面，并发安全。
type Store struct {
	mu sync.RWMutex

	opts StoreOptions
	// modes 模式 → 内存态（受 MaxCachedModes 约束）。
	modes map[Mode]*modeCache
	// order 模式级 LRU（前端为最近使用）。
	order *list.List

	// stats 诊断计数。
	hits   uint64
	misses uint64
}

// NewStore 构造知识库。
func NewStore(opts StoreOptions) *Store {
	if opts.MaxEntriesPerMode <= 0 {
		opts.MaxEntriesPerMode = DefaultStoreOptions().MaxEntriesPerMode
	}
	if opts.MaxCachedModes <= 0 {
		opts.MaxCachedModes = DefaultStoreOptions().MaxCachedModes
	}
	if opts.now == nil {
		opts.now = time.Now
	}
	return &Store{
		opts:  opts,
		modes: make(map[Mode]*modeCache),
		order: list.New(),
	}
}

// Add 添加知识条目（v1 addKnowledge 对等）。
func (s *Store) Add(ctx context.Context, mode Mode, title, content string, tags []string, source string) (Entry, error) {
	mode = NormalizeMode(mode)
	entry := NewEntry(title, content, tags, source)
	if err := entry.Validate(); err != nil {
		return Entry{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	mc, err := s.loadModeLocked(ctx, mode)
	if err != nil {
		return Entry{}, err
	}

	mc.entries = append(mc.entries, entry)
	mc.byID[entry.ID] = len(mc.entries) - 1
	mc.index.add(entry.ID, indexText(entry))

	s.evictOverflowLocked(mc)

	if err := s.persistLocked(ctx, mc); err != nil {
		return Entry{}, err
	}
	return entry, nil
}

// Search 全文检索（v1 searchKnowledge 对等，BM25 + 分页）。
func (s *Store) Search(ctx context.Context, mode Mode, query string, page, pageSize int) (SearchResponse, error) {
	mode = NormalizeMode(mode)
	if strings.TrimSpace(query) == "" {
		return SearchResponse{}, fmt.Errorf("knowledge: query 不能为空")
	}
	page, pageSize = normalizePaging(page, pageSize, 10)

	s.mu.Lock()
	mc, err := s.loadModeLocked(ctx, mode)
	if err != nil {
		s.mu.Unlock()
		return SearchResponse{}, err
	}

	// 先取全部命中（BM25 已按分数降序），再分页 —— 这样 total 是真实命中数。
	all := mc.index.search(query, 0)
	total := len(all)

	offset := (page - 1) * pageSize
	if offset > total {
		offset = total
	}
	end := offset + pageSize
	if end > total {
		end = total
	}

	results := make([]SearchResult, 0, end-offset)
	for _, hit := range all[offset:end] {
		idx, ok := mc.byID[hit.id]
		if !ok {
			continue
		}
		results = append(results, SearchResult{Entry: mc.entries[idx], Score: hit.score})
	}
	s.mu.Unlock()

	return SearchResponse{
		Results:    results,
		Total:      total,
		Page:       page,
		PageSize:   pageSize,
		TotalPages: totalPages(total, pageSize),
	}, nil
}

// List 分页浏览全部条目（按 updatedAt 降序，v1 listKnowledge 对等）。
func (s *Store) List(ctx context.Context, mode Mode, page, pageSize int) (ListResponse, error) {
	mode = NormalizeMode(mode)
	page, pageSize = normalizePaging(page, pageSize, 20)

	s.mu.Lock()
	mc, err := s.loadModeLocked(ctx, mode)
	if err != nil {
		s.mu.Unlock()
		return ListResponse{}, err
	}

	sorted := make([]Entry, len(mc.entries))
	copy(sorted, mc.entries)
	s.mu.Unlock()

	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].UpdatedAt != sorted[j].UpdatedAt {
			return sorted[i].UpdatedAt > sorted[j].UpdatedAt
		}
		return sorted[i].ID < sorted[j].ID
	})

	total := len(sorted)
	offset := (page - 1) * pageSize
	if offset > total {
		offset = total
	}
	end := offset + pageSize
	if end > total {
		end = total
	}

	items := make([]Entry, end-offset)
	copy(items, sorted[offset:end])

	return ListResponse{
		Items:      items,
		Total:      total,
		Page:       page,
		PageSize:   pageSize,
		TotalPages: totalPages(total, pageSize),
	}, nil
}

// Update 更新条目（v1 updateKnowledge 对等）。返回 false 表示条目不存在。
func (s *Store) Update(ctx context.Context, mode Mode, id string, title, content string, tags []string, source string) (Entry, bool, error) {
	mode = NormalizeMode(mode)

	s.mu.Lock()
	defer s.mu.Unlock()

	mc, err := s.loadModeLocked(ctx, mode)
	if err != nil {
		return Entry{}, false, err
	}
	idx, ok := mc.byID[id]
	if !ok {
		return Entry{}, false, nil
	}

	entry := mc.entries[idx]
	if title != "" {
		entry.Title = clampRunes(strings.TrimSpace(title), maxTitleLen)
	}
	if content != "" {
		entry.Content = clampRunes(strings.TrimSpace(content), maxContentLen)
	}
	if tags != nil {
		entry.Tags = normalizeTags(tags)
	}
	if source != "" {
		entry.Source = source
	}
	entry.UpdatedAt = s.opts.now().UnixMilli()

	mc.entries[idx] = entry
	// 索引重建：先移除旧文档再按新文本加入（BM25 词频随内容变化）。
	mc.index.remove(id)
	mc.index.add(id, indexText(entry))

	if err := s.persistLocked(ctx, mc); err != nil {
		return Entry{}, false, err
	}
	return entry, true, nil
}

// Delete 删除条目（v1 deleteKnowledge 对等）。返回 false 表示不存在。
func (s *Store) Delete(ctx context.Context, mode Mode, id string) (bool, error) {
	mode = NormalizeMode(mode)

	s.mu.Lock()
	defer s.mu.Unlock()

	mc, err := s.loadModeLocked(ctx, mode)
	if err != nil {
		return false, err
	}
	idx, ok := mc.byID[id]
	if !ok {
		return false, nil
	}

	// 删除：索引 + 切片 + id 映射三处同步。
	mc.index.remove(id)
	mc.entries = append(mc.entries[:idx], mc.entries[idx+1:]...)
	delete(mc.byID, id)
	// 重建受影响的 id → 下标映射（删除后其后的元素整体前移）。
	for i := idx; i < len(mc.entries); i++ {
		mc.byID[mc.entries[i].ID] = i
	}

	if err := s.persistLocked(ctx, mc); err != nil {
		return false, err
	}
	return true, nil
}

// Count 返回某模式的条目数。
func (s *Store) Count(ctx context.Context, mode Mode) (int, error) {
	mode = NormalizeMode(mode)
	s.mu.Lock()
	defer s.mu.Unlock()
	mc, err := s.loadModeLocked(ctx, mode)
	if err != nil {
		return 0, err
	}
	return len(mc.entries), nil
}

// Stats 返回内存态诊断，供 72h soak 观测。
type Stats struct {
	CachedModes int    `json:"cached_modes"`
	MaxCached   int    `json:"max_cached_modes"`
	TotalItems  int    `json:"total_items"`
	TotalTerms  int    `json:"total_index_terms"`
	Hits        uint64 `json:"hits"`
	Misses      uint64 `json:"misses"`
}

// Stats 返回诊断快照。
func (s *Store) Stats() Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st := Stats{
		CachedModes: len(s.modes),
		MaxCached:   s.opts.MaxCachedModes,
		Hits:        s.hits,
		Misses:      s.misses,
	}
	for _, mc := range s.modes {
		st.TotalItems += len(mc.entries)
		st.TotalTerms += mc.index.Terms()
	}
	return st
}

// --- 内部 ---

// loadModeLocked 取（或从持久层加载）某模式的内存态，并维护模式级 LRU。
//
// 调用方必须已持写锁。
func (s *Store) loadModeLocked(ctx context.Context, mode Mode) (*modeCache, error) {
	if mc, ok := s.modes[mode]; ok {
		s.hits++
		s.order.MoveToFront(mc.elem)
		return mc, nil
	}
	s.misses++

	var entries []Entry
	if s.opts.Persistence != nil {
		loaded, err := s.opts.Persistence.Load(ctx, mode)
		if err != nil {
			return nil, fmt.Errorf("knowledge: 加载 %s 模式失败: %w", mode, err)
		}
		entries = loaded
	}

	mc := &modeCache{
		mode:    mode,
		entries: entries,
		byID:    make(map[string]int, len(entries)),
		index:   newIndex(),
	}
	for i, e := range entries {
		mc.byID[e.ID] = i
		mc.index.add(e.ID, indexText(e))
	}

	mc.elem = s.order.PushFront(mc)
	s.modes[mode] = mc

	// 模式数超限 → 淘汰最久未用的模式内存态（数据仍在持久层，可重建）。
	s.evictModesLocked()

	return mc, nil
}

// evictModesLocked 把常驻模式数压到 MaxCachedModes 以内。
//
// 淘汰的是整个模式的索引（数据仍在持久层，下次访问自动重建），
// 因此「同时常驻内存的模式数」有界 —— 这是知识库侧的内存治理点。
//
// 无持久层时不做淘汰：淘汰掉的内存态没有任何地方能重建它，
// 等同于静默丢数据。此时内存占用由 MaxEntriesPerMode 单独约束即可。
func (s *Store) evictModesLocked() {
	if s.opts.Persistence == nil {
		return
	}
	for len(s.modes) > s.opts.MaxCachedModes {
		back := s.order.Back()
		if back == nil {
			return
		}
		mc, ok := back.Value.(*modeCache)
		if !ok {
			s.order.Remove(back)
			continue
		}
		// 刚加载进来的模式在 order 前端，因此 back 不会选中它 ——
		// 循环必然收敛，不会出现「反复淘汰刚加载的模式」。
		delete(s.modes, mc.mode)
		s.order.Remove(back)
	}
}

// evictOverflowLocked 条目数超限时淘汰 updatedAt 最旧的条目。
func (s *Store) evictOverflowLocked(mc *modeCache) {
	max := s.opts.MaxEntriesPerMode
	if max <= 0 || len(mc.entries) <= max {
		return
	}

	// 按下标排序取最旧的若干条（避免整体排序，只淘汰超出部分）。
	type aging struct {
		idx       int
		updatedAt int64
	}
	aged := make([]aging, len(mc.entries))
	for i, e := range mc.entries {
		aged[i] = aging{idx: i, updatedAt: e.UpdatedAt}
	}
	sort.Slice(aged, func(i, j int) bool { return aged[i].updatedAt < aged[j].updatedAt })

	drop := make(map[int]struct{}, len(mc.entries)-max)
	for i := 0; i < len(mc.entries)-max; i++ {
		drop[aged[i].idx] = struct{}{}
	}

	kept := make([]Entry, 0, max)
	for i, e := range mc.entries {
		if _, removed := drop[i]; removed {
			mc.index.remove(e.ID)
			delete(mc.byID, e.ID)
			continue
		}
		kept = append(kept, e)
	}
	mc.entries = kept

	// 重建 id → 下标映射。
	mc.byID = make(map[string]int, len(kept))
	for i, e := range kept {
		mc.byID[e.ID] = i
	}
}

// persistLocked 写回持久层。调用方必须已持写锁。
func (s *Store) persistLocked(ctx context.Context, mc *modeCache) error {
	if s.opts.Persistence == nil {
		return nil
	}
	if err := s.opts.Persistence.Save(ctx, mc.mode, mc.entries); err != nil {
		return fmt.Errorf("knowledge: 保存 %s 模式失败: %w", mc.mode, err)
	}
	return nil
}

// indexText 构造参与索引的文本（标题权重更高 —— 重复标题提升 BM25 词频）。
func indexText(e Entry) string {
	return e.Title + " " + e.Title + " " + e.Content + " " + strings.Join(e.Tags, " ")
}

func normalizePaging(page, pageSize, defaultPageSize int) (int, int) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = defaultPageSize
	}
	if pageSize > 200 {
		pageSize = 200
	}
	return page, pageSize
}

func totalPages(total, pageSize int) int {
	if total == 0 {
		return 1
	}
	n := total / pageSize
	if total%pageSize != 0 {
		n++
	}
	if n < 1 {
		n = 1
	}
	return n
}
