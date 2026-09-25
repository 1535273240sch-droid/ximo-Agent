// Package knowledge 提供知识库工具（knowledge：search/list/add/update/delete），
// 内置纯 Go BM25 检索（对应 v1 的 Orama BM25）。
package knowledge

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/ximo888ok-netizen/ximo-agent/internal/tool"
	"github.com/ximo888ok-netizen/ximo-agent/internal/tool/domains"
)

// Entry 知识条目。
type Entry struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Content   string    `json:"content"`
	Tags      []string  `json:"tags,omitempty"`
	Source    string    `json:"source,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// SearchResult 检索结果（含 BM25 得分）。
type SearchResult struct {
	Entry Entry
	Score float64
}

// Repository 知识库存储。生产环境由任务03 的 knowledge_entries 表实现；
// 开发期使用 MemoryRepository。
type Repository interface {
	Add(ctx context.Context, mode string, e Entry) (Entry, error)
	Get(ctx context.Context, mode, id string) (Entry, bool, error)
	Update(ctx context.Context, mode, id string, updates Entry) (Entry, bool, error)
	Delete(ctx context.Context, mode, id string) (bool, error)
	List(ctx context.Context, mode string) ([]Entry, error)
	Search(ctx context.Context, mode, query string, limit int) ([]SearchResult, error)
}

// searchScanLimit 搜索时的扫描上限：一次性拉取的命中数上限，
// 分页在内存完成后返回（避免每页 limit 导致 total 失真）。
const searchScanLimit = 1000

// Tool 实现 knowledge 工具。
type Tool struct {
	repo Repository
	now  func() time.Time
	mu   sync.Mutex
	seq  int
}

// New 创建 knowledge 工具。
func New(repo Repository) *Tool {
	if repo == nil {
		repo = NewMemoryRepository()
	}
	return &Tool{repo: repo, now: time.Now}
}

// Definition 实现 tool.Tool。
func (t *Tool) Definition() tool.ToolDefinition {
	return tool.ToolDefinition{
		Name: "knowledge",
		Description: "管理当前模式的知识库。支持添加（add）、搜索（search，BM25 全文检索）、" +
			"浏览（list）、更新（update）、删除（delete）知识条目。每个模式（office/coding/design）的知识库相互独立。",
		Parameters: tool.ObjectSchema(map[string]tool.JSONSchema{
			"action":    {Type: "string", Description: "操作类型", Enum: []any{"add", "search", "list", "update", "delete"}},
			"title":     {Type: "string", Description: "add/update: 知识标题"},
			"content":   {Type: "string", Description: "add/update: 知识正文"},
			"tags":      {Type: "array", Description: "add/update: 标签数组", Items: &tool.JSONSchema{Type: "string"}},
			"source":    {Type: "string", Description: "add/update: 来源"},
			"query":     {Type: "string", Description: "search: 搜索关键词"},
			"page":      {Type: "integer", Description: "search/list: 页码（默认 1）", Default: 1},
			"page_size": {Type: "integer", Description: "每页条数（默认 search=10, list=20）"},
			"id":        {Type: "string", Description: "update/delete: 条目 ID"},
		}, "action"),
		Risk:        tool.RiskLow,
		Idempotency: tool.ClassIdempotent,
		Domain:      tool.DomainInProcess,
	}
}

// Execute 实现 tool.Tool。
func (t *Tool) Execute(ctx context.Context, req tool.ToolRequest) tool.ToolResponse {
	fail := func(format string, args ...any) tool.ToolResponse {
		return domains.Fail(req.ToolCallID, "knowledge", format, args...)
	}
	action := domains.StringArg(req.Arguments, "action")
	mode := string(req.Mode)
	if mode == "" {
		mode = "office"
	}
	if err := ctx.Err(); err != nil {
		return fail("调用已取消: %v", err)
	}

	switch action {
	case "add":
		return t.handleAdd(ctx, req, mode)
	case "search":
		return t.handleSearch(ctx, req, mode)
	case "list":
		return t.handleList(ctx, req, mode)
	case "update":
		return t.handleUpdate(ctx, req, mode)
	case "delete":
		return t.handleDelete(ctx, req, mode)
	default:
		return fail("未知 action: %q（支持 add/search/list/update/delete）", action)
	}
}

func (t *Tool) handleAdd(ctx context.Context, req tool.ToolRequest, mode string) tool.ToolResponse {
	fail := func(format string, args ...any) tool.ToolResponse {
		return domains.Fail(req.ToolCallID, "knowledge", format, args...)
	}
	title := domains.StringArg(req.Arguments, "title")
	content := domains.StringArg(req.Arguments, "content")
	if title == "" {
		return fail("title 不能为空")
	}
	if content == "" {
		return fail("content 不能为空")
	}
	t.mu.Lock()
	t.seq++
	id := fmt.Sprintf("k-%d", t.seq)
	t.mu.Unlock()
	source := domains.StringArgDefault(req.Arguments, "source", "agent")
	entry, err := t.repo.Add(ctx, mode, Entry{
		ID:        id,
		Title:     title,
		Content:   content,
		Tags:      domains.StringSliceArg(req.Arguments, "tags"),
		Source:    source,
		UpdatedAt: t.now(),
	})
	if err != nil {
		return fail("添加失败: %v", err)
	}
	resp := domains.Text(req.ToolCallID, "knowledge", fmt.Sprintf(
		"✅ 知识条目已添加。\n\nID: %s\n标题: %s\n标签: %s\n来源: %s\n\n(%s 模式知识库)",
		entry.ID, entry.Title, joinOrNone(entry.Tags), entry.Source, mode))
	resp.Metadata = map[string]any{"id": entry.ID, "action": "add"}
	return resp
}

func (t *Tool) handleSearch(ctx context.Context, req tool.ToolRequest, mode string) tool.ToolResponse {
	fail := func(format string, args ...any) tool.ToolResponse {
		return domains.Fail(req.ToolCallID, "knowledge", format, args...)
	}
	query := domains.StringArg(req.Arguments, "query")
	if query == "" {
		return fail("query 不能为空")
	}
	page := domains.IntArg(req.Arguments, "page", 1)
	pageSize := domains.IntArg(req.Arguments, "page_size", 10)
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 10
	}
	if pageSize > 50 {
		pageSize = 50
	}
	// 以扫描上限拉取结果再在内存分页：total 反映真实命中数，
	// 而不是“本页limit”。Repository 实现可在此上限内返回全部命中。
	results, err := t.repo.Search(ctx, mode, query, searchScanLimit)
	if err != nil {
		return fail("搜索失败: %v", err)
	}
	total := len(results)
	start := (page - 1) * pageSize
	if start > total {
		start = total
	}
	end := start + pageSize
	if end > total {
		end = total
	}
	pageResults := results[start:end]
	if len(pageResults) == 0 {
		resp := domains.Text(req.ToolCallID, "knowledge", fmt.Sprintf("未找到与 %q 相关的知识条目。", query))
		resp.Metadata = map[string]any{"total": 0, "action": "search"}
		return resp
	}
	var b strings.Builder
	fmt.Fprintf(&b, "🔍 搜索 %q — 共 %d 条结果（第 %d 页）\n\n", query, total, page)
	for i, r := range pageResults {
		tags := ""
		if len(r.Entry.Tags) > 0 {
			tags = " [" + strings.Join(r.Entry.Tags, ", ") + "]"
		}
		fmt.Fprintf(&b, "### %d. %s%s\n**ID**: %s | **相关性**: %.2f | **来源**: %s\n\n%s\n",
			start+i+1, r.Entry.Title, tags, r.Entry.ID, r.Score, r.Entry.Source, r.Entry.Content)
	}
	resp := domains.Text(req.ToolCallID, "knowledge", b.String())
	resp.Metadata = map[string]any{"total": total, "page": page, "action": "search"}
	return resp
}

func (t *Tool) handleList(ctx context.Context, req tool.ToolRequest, mode string) tool.ToolResponse {
	fail := func(format string, args ...any) tool.ToolResponse {
		return domains.Fail(req.ToolCallID, "knowledge", format, args...)
	}
	page := domains.IntArg(req.Arguments, "page", 1)
	pageSize := domains.IntArg(req.Arguments, "page_size", 20)
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}
	entries, err := t.repo.List(ctx, mode)
	if err != nil {
		return fail("浏览失败: %v", err)
	}
	total := len(entries)
	start := (page - 1) * pageSize
	if start > total {
		start = total
	}
	end := start + pageSize
	if end > total {
		end = total
	}
	if start == end {
		resp := domains.Text(req.ToolCallID, "knowledge", "知识库为空。使用 knowledge(action=\"add\") 添加知识条目。")
		resp.Metadata = map[string]any{"total": 0, "action": "list"}
		return resp
	}
	var b strings.Builder
	fmt.Fprintf(&b, "📚 %s 知识库 — 共 %d 条（第 %d 页）\n\n", mode, total, page)
	for i, e := range entries[start:end] {
		tags := ""
		if len(e.Tags) > 0 {
			tags = " [" + strings.Join(e.Tags, ", ") + "]"
		}
		preview := e.Content
		if len([]rune(preview)) > 80 {
			preview = string([]rune(preview)[:80]) + "..."
		}
		fmt.Fprintf(&b, "%d. **%s**%s\n   ID: %s | 来源: %s | 更新: %s\n   %s\n\n",
			start+i+1, e.Title, tags, e.ID, e.Source, e.UpdatedAt.Format("2006-01-02 15:04"), preview)
	}
	resp := domains.Text(req.ToolCallID, "knowledge", b.String())
	resp.Metadata = map[string]any{"total": total, "page": page, "action": "list"}
	return resp
}

func (t *Tool) handleUpdate(ctx context.Context, req tool.ToolRequest, mode string) tool.ToolResponse {
	fail := func(format string, args ...any) tool.ToolResponse {
		return domains.Fail(req.ToolCallID, "knowledge", format, args...)
	}
	id := domains.StringArg(req.Arguments, "id")
	if id == "" {
		return fail("id 不能为空")
	}
	existing, ok, err := t.repo.Get(ctx, mode, id)
	if err != nil {
		return fail("查询失败: %v", err)
	}
	if !ok {
		return fail("未找到 ID 为 %s 的条目", id)
	}
	updates := existing
	if v := domains.StringArg(req.Arguments, "title"); v != "" {
		updates.Title = v
	}
	if v := domains.StringArg(req.Arguments, "content"); v != "" {
		updates.Content = v
	}
	if tags := domains.StringSliceArg(req.Arguments, "tags"); len(tags) > 0 {
		updates.Tags = tags
	}
	if v := domains.StringArg(req.Arguments, "source"); v != "" {
		updates.Source = v
	}
	if !entryChanged(existing, updates) {
		return fail("至少提供一项要更新的字段（title/content/tags/source）")
	}
	updated, ok, err := t.repo.Update(ctx, mode, id, updates)
	if err != nil {
		return fail("更新失败: %v", err)
	}
	if !ok {
		return fail("未找到 ID 为 %s 的条目", id)
	}
	resp := domains.Text(req.ToolCallID, "knowledge", fmt.Sprintf("✅ 知识条目已更新。\n\nID: %s\n标题: %s", updated.ID, updated.Title))
	resp.Metadata = map[string]any{"id": updated.ID, "action": "update"}
	return resp
}

func (t *Tool) handleDelete(ctx context.Context, req tool.ToolRequest, mode string) tool.ToolResponse {
	fail := func(format string, args ...any) tool.ToolResponse {
		return domains.Fail(req.ToolCallID, "knowledge", format, args...)
	}
	id := domains.StringArg(req.Arguments, "id")
	if id == "" {
		return fail("id 不能为空")
	}
	ok, err := t.repo.Delete(ctx, mode, id)
	if err != nil {
		return fail("删除失败: %v", err)
	}
	if !ok {
		return fail("未找到 ID 为 %s 的条目", id)
	}
	resp := domains.Text(req.ToolCallID, "knowledge", fmt.Sprintf("✅ 知识条目 %s 已删除。", id))
	resp.Metadata = map[string]any{"id": id, "action": "delete"}
	return resp
}

func joinOrNone(tags []string) string {
	if len(tags) == 0 {
		return "无"
	}
	return strings.Join(tags, ", ")
}

// entryChanged 报告 updates 相对 existing 是否有实际变化。
func entryChanged(existing, updates Entry) bool {
	if existing.Title != updates.Title || existing.Content != updates.Content || existing.Source != updates.Source {
		return true
	}
	if len(existing.Tags) != len(updates.Tags) {
		return true
	}
	for i := range existing.Tags {
		if existing.Tags[i] != updates.Tags[i] {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 内存实现 + BM25
// ---------------------------------------------------------------------------

// MemoryRepository 是 Repository 的内存实现（开发期 mock）。
type MemoryRepository struct {
	mu     sync.Mutex
	byMode map[string][]Entry
	nextID int
}

// NewMemoryRepository 创建内存知识库。
func NewMemoryRepository() *MemoryRepository {
	return &MemoryRepository{byMode: make(map[string][]Entry)}
}

func (r *MemoryRepository) Add(ctx context.Context, mode string, e Entry) (Entry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	if e.ID == "" {
		e.ID = fmt.Sprintf("k-%d", r.nextID)
	}
	if e.UpdatedAt.IsZero() {
		e.UpdatedAt = time.Now()
	}
	r.byMode[mode] = append(r.byMode[mode], e)
	return e, nil
}

func (r *MemoryRepository) Get(ctx context.Context, mode, id string) (Entry, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.byMode[mode] {
		if e.ID == id {
			return e, true, nil
		}
	}
	return Entry{}, false, nil
}

func (r *MemoryRepository) Update(ctx context.Context, mode, id string, updates Entry) (Entry, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entries := r.byMode[mode]
	for i, e := range entries {
		if e.ID == id {
			updates.ID = id
			updates.UpdatedAt = time.Now()
			entries[i] = updates
			return updates, true, nil
		}
	}
	return Entry{}, false, nil
}

func (r *MemoryRepository) Delete(ctx context.Context, mode, id string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entries := r.byMode[mode]
	for i, e := range entries {
		if e.ID == id {
			r.byMode[mode] = append(entries[:i], entries[i+1:]...)
			return true, nil
		}
	}
	return false, nil
}

func (r *MemoryRepository) List(ctx context.Context, mode string) ([]Entry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entries := append([]Entry(nil), r.byMode[mode]...)
	sort.Slice(entries, func(i, j int) bool { return entries[i].UpdatedAt.After(entries[j].UpdatedAt) })
	return entries, nil
}

// Search 实现 BM25 检索（k1=1.2, b=0.75）。
func (r *MemoryRepository) Search(ctx context.Context, mode, query string, limit int) ([]SearchResult, error) {
	r.mu.Lock()
	entries := append([]Entry(nil), r.byMode[mode]...)
	r.mu.Unlock()
	if len(entries) == 0 || limit <= 0 {
		return nil, nil
	}

	docs := make([][]string, len(entries))
	df := make(map[string]int)
	for i, e := range entries {
		docs[i] = tokenize(e.Title + " " + e.Content + " " + strings.Join(e.Tags, " "))
		seen := make(map[string]bool)
		for _, tok := range docs[i] {
			if !seen[tok] {
				df[tok]++
				seen[tok] = true
			}
		}
	}

	const k1, b = 1.2, 0.75
	totalDocs := float64(len(entries))
	avgLen := 0.0
	for _, d := range docs {
		avgLen += float64(len(d))
	}
	if avgLen == 0 {
		avgLen = 1
	}
	avgLen /= totalDocs

	queryTokens := tokenize(query)
	type scored struct {
		entry Entry
		score float64
	}
	var results []scored
	for i, e := range entries {
		score := 0.0
		tf := make(map[string]int)
		for _, tok := range docs[i] {
			tf[tok]++
		}
		docLen := float64(len(docs[i]))
		for _, tok := range queryTokens {
			f, ok := tf[tok]
			if !ok {
				continue
			}
			n := df[tok]
			if n == 0 {
				continue
			}
			idf := ln(1 + (totalDocs-float64(n)+0.5)/(float64(n)+0.5))
			tfNorm := float64(f) * (k1 + 1) / (float64(f) + k1*(1-b+b*docLen/avgLen))
			score += idf * tfNorm
		}
		if score > 0 {
			results = append(results, scored{entry: e, score: score})
		}
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].score != results[j].score {
			return results[i].score > results[j].score
		}
		return results[i].entry.ID < results[j].entry.ID
	})
	if len(results) > limit {
		results = results[:limit]
	}
	out := make([]SearchResult, 0, len(results))
	for _, r := range results {
		out = append(out, SearchResult{Entry: r.entry, Score: r.score})
	}
	return out, nil
}

func ln(x float64) float64 {
	return math.Log(x)
}

// tokenize 分词：英文按非字母数字切分，CJK 按二元组（bigram）切分。
func tokenize(s string) []string {
	var tokens []string
	var current []rune
	flush := func() {
		if len(current) == 0 {
			return
		}
		tokens = append(tokens, strings.ToLower(string(current)))
		current = current[:0]
	}
	var cjk []rune
	flushCJK := func() {
		if len(cjk) == 0 {
			return
		}
		if len(cjk) == 1 {
			tokens = append(tokens, string(cjk))
		} else {
			for i := 0; i+1 < len(cjk); i++ {
				tokens = append(tokens, string(cjk[i:i+2]))
			}
		}
		cjk = cjk[:0]
	}
	for _, r := range strings.ToLower(s) {
		switch {
		case unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r):
			flush()
			cjk = append(cjk, r)
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			flushCJK()
			current = append(current, r)
		default:
			flush()
			flushCJK()
		}
	}
	flush()
	flushCJK()
	return tokens
}
