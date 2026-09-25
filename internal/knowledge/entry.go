// entry.go —— 知识条目模型（对应 v1 src/main/KnowledgeStore.ts 的 KnowledgeEntry）。
//
// 知识库让 Agent 跨会话沉淀可复用经验：解决方案、踩过的坑、关键决策。
// 每个模式（office/coding/design）的知识库相互独立。
package knowledge

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// Mode 知识库分区模式。与 v1 Mode 一致。
type Mode string

const (
	ModeOffice Mode = "office"
	ModeCoding Mode = "coding"
	ModeDesign Mode = "design"
)

// Valid 校验模式合法性。
func (m Mode) Valid() bool {
	switch m {
	case ModeOffice, ModeCoding, ModeDesign:
		return true
	default:
		return false
	}
}

// NormalizeMode 归一化模式，非法值回退 office（v1 默认值）。
func NormalizeMode(m Mode) Mode {
	if m.Valid() {
		return m
	}
	return ModeOffice
}

// Entry 知识条目（字段与 v1 KnowledgeEntry 一致）。
type Entry struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Content   string   `json:"content"`
	Tags      []string `json:"tags"`
	Source    string   `json:"source"`
	CreatedAt int64    `json:"createdAt"`
	UpdatedAt int64    `json:"updatedAt"`
}

// SearchResult 搜索结果项（带相关性分数）。
type SearchResult struct {
	Entry
	Score float64 `json:"score"`
}

// SearchResponse 带分页的搜索结果（与 v1 KnowledgeSearchResponse 对齐）。
type SearchResponse struct {
	Results    []SearchResult `json:"results"`
	Total      int            `json:"total"`
	Page       int            `json:"page"`
	PageSize   int            `json:"page_size"`
	TotalPages int            `json:"total_pages"`
}

// ListResponse 带分页的列表结果。
type ListResponse struct {
	Items      []Entry `json:"items"`
	Total      int     `json:"total"`
	Page       int     `json:"page"`
	PageSize   int     `json:"page_size"`
	TotalPages int     `json:"total_pages"`
}

// 条目内容上限（v1 knowledge-extract.ts 的截断约束）。
const (
	maxTitleLen   = 200
	maxContentLen = 4000
	maxTags       = 5
)

// NewEntry 归一化构造一个知识条目。
func NewEntry(title, content string, tags []string, source string) Entry {
	now := time.Now().UnixMilli()
	if source == "" {
		source = "agent"
	}
	return Entry{
		ID:        genID(),
		Title:     clampRunes(strings.TrimSpace(title), maxTitleLen),
		Content:   clampRunes(strings.TrimSpace(content), maxContentLen),
		Tags:      normalizeTags(tags),
		Source:    source,
		CreatedAt: now,
		UpdatedAt: now,
	}
}

// Validate 校验条目必填字段（v1 addKnowledge 的入参校验）。
func (e Entry) Validate() error {
	if strings.TrimSpace(e.Title) == "" {
		return fmt.Errorf("knowledge: title 不能为空")
	}
	if strings.TrimSpace(e.Content) == "" {
		return fmt.Errorf("knowledge: content 不能为空")
	}
	return nil
}

// normalizeTags 去空、去重、限长（v1 最多 5 个标签）。
func normalizeTags(tags []string) []string {
	out := make([]string, 0, len(tags))
	seen := make(map[string]struct{}, len(tags))
	for _, t := range tags {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
		if len(out) >= maxTags {
			break
		}
	}
	return out
}

// genID 生成条目 ID。前缀与 v1 一致（kb_），但用 crypto/rand 而非 Math.random
// —— 并发写入时随机源冲突概率更低，且不依赖全局 math/rand 状态。
func genID() string {
	var buf [5]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// 极端情况退化为时间戳，仍保证唯一性足够（配合纳秒）。
		return fmt.Sprintf("kb_%s", time.Now().Format("20060102150405.000000000"))
	}
	return fmt.Sprintf("kb_%x_%s", time.Now().UnixMilli(), hex.EncodeToString(buf[:]))
}

// clampRunes 按字符截断（避免切坏多字节 UTF-8）。
func clampRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}
