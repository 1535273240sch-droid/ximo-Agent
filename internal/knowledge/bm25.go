// bm25.go —— 轻量 BM25 全文检索（v1 用 Orama 的 BM25；本实现自建，不引第三方依赖）。
//
// 任务书红线「不引入任务书未提及的第三方依赖」，而 v1 的 @orama/orama 是 JS 生态
// 的包，Go 侧没有等价且已在任务书登记的依赖，因此按 BM25 公式自实现。
//
// BM25 公式（k1=1.2, b=0.75 为业界默认）：
//
//	score(q, d) = Σ_t IDF(t) * (f(t,d) * (k1+1)) / (f(t,d) + k1 * (1 - b + b * |d|/avgdl))
//	IDF(t) = ln(1 + (N - df(t) + 0.5) / (df(t) + 0.5))
//
// 中文分词：不做词性分析，用 bigram（相邻两字）切分 —— 对中文检索足够有效，
// 且无需词表。英文/数字按空格与标点切词。
//
// 内存治理：倒排索引的 posting list 与文档长度表都随条目数线性增长，
// 因此 Store 侧对条目数设上限（见 store.go 的 MaxEntries）。
package knowledge

import (
	"math"
	"sort"
	"strings"
	"unicode"
)

// BM25 参数。
const (
	bm25K1 = 1.2
	bm25B  = 0.75
)

// tokenize 把文本切为检索词元。
//
// 规则：
//   - 拉丁字母/数字：连续段作为一个词元（小写归一）
//   - CJK 字符：相邻两字组成 bigram（单字文本退化为 unigram）
//   - 其他符号：作为分隔符丢弃
func tokenize(text string) []string {
	if text == "" {
		return nil
	}
	lower := strings.ToLower(text)

	var tokens []string
	var latin strings.Builder
	// cjk 保存当前连续的 CJK 字符序列，用于生成 bigram。
	var cjk []rune

	flushLatin := func() {
		if latin.Len() > 0 {
			tokens = append(tokens, latin.String())
			latin.Reset()
		}
	}
	flushCJK := func() {
		switch len(cjk) {
		case 0:
		case 1:
			tokens = append(tokens, string(cjk))
		default:
			for i := 0; i+1 < len(cjk); i++ {
				tokens = append(tokens, string(cjk[i:i+2]))
			}
			// 同时保留单字，提升召回（例如查单字「货」也能命中）。
			for _, r := range cjk {
				tokens = append(tokens, string(r))
			}
		}
		cjk = cjk[:0]
	}

	for _, r := range lower {
		switch {
		case isCJK(r):
			flushLatin()
			cjk = append(cjk, r)
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_':
			flushCJK()
			latin.WriteRune(r)
		default:
			flushLatin()
			flushCJK()
		}
	}
	flushLatin()
	flushCJK()

	return tokens
}

// isCJK 判断是否中日韩字符（与 context 包预分词正则的区间保持一致）。
func isCJK(r rune) bool {
	switch {
	case r >= 0x4E00 && r <= 0x9FA5: // 中日韩统一表意文字
		return true
	case r >= 0x3040 && r <= 0x309F: // 平假名
		return true
	case r >= 0x30A0 && r <= 0x30FF: // 片假名
		return true
	default:
		return false
	}
}

// index 单个模式的倒排索引。
type index struct {
	// postings 词元 → 文档 ID → 词频。
	postings map[string]map[string]int
	// docLen 文档 ID → 文档长度（词元总数）。
	docLen map[string]int
	// totalLen 全部文档长度之和（用于 avgdl）。
	totalLen int
	// docCount 文档数。
	docCount int
}

func newIndex() *index {
	return &index{
		postings: make(map[string]map[string]int),
		docLen:   make(map[string]int),
	}
}

// add 把一个文档加入索引。
func (ix *index) add(id string, text string) {
	if _, exists := ix.docLen[id]; exists {
		ix.remove(id)
	}
	tokens := tokenize(text)
	if len(tokens) == 0 {
		ix.docLen[id] = 0
		ix.docCount++
		return
	}

	ix.docLen[id] = len(tokens)
	ix.totalLen += len(tokens)
	ix.docCount++

	for _, t := range tokens {
		docs, ok := ix.postings[t]
		if !ok {
			docs = make(map[string]int)
			ix.postings[t] = docs
		}
		docs[id]++
	}
}

// remove 从索引中删除一个文档。
func (ix *index) remove(id string) {
	length, exists := ix.docLen[id]
	if !exists {
		return
	}
	delete(ix.docLen, id)
	ix.docCount--
	ix.totalLen -= length

	// 遍历该文档的全部词元并减去词频。
	// 为了不额外存「文档→词元」的反向映射（省内存），这里用 posting list 扫描：
	// 代价是 O(词表大小)，但删除是低频操作（用户主动删或容量淘汰）。
	for token, docs := range ix.postings {
		if _, ok := docs[id]; ok {
			delete(docs, id)
			if len(docs) == 0 {
				delete(ix.postings, token)
			}
		}
	}
}

// search 执行 BM25 检索，返回按分数降序的 (id, score)。
func (ix *index) search(query string, limit int) []scoredID {
	tokens := tokenize(query)
	if len(tokens) == 0 || ix.docCount == 0 {
		return nil
	}

	avgdl := float64(ix.totalLen) / float64(ix.docCount)
	if avgdl <= 0 {
		avgdl = 1
	}

	scores := make(map[string]float64)
	for _, t := range tokens {
		docs, ok := ix.postings[t]
		if !ok {
			continue
		}
		df := float64(len(docs))
		// IDF：加 1 平滑，保证 df 很大时仍为正值。
		idf := math.Log(1 + (float64(ix.docCount)-df+0.5)/(df+0.5))

		for id, tf := range docs {
			dl := float64(ix.docLen[id])
			denom := float64(tf) + bm25K1*(1-bm25B+bm25B*dl/avgdl)
			if denom == 0 {
				continue
			}
			scores[id] += idf * (float64(tf) * (bm25K1 + 1)) / denom
		}
	}

	out := make([]scoredID, 0, len(scores))
	for id, s := range scores {
		if s > 0 {
			out = append(out, scoredID{id: id, score: s})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score > out[j].score
		}
		// 分数相同时按 ID 排序，保证结果稳定（便于测试与分页一致性）。
		return out[i].id < out[j].id
	})

	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// scoredID 检索中间结果。
type scoredID struct {
	id    string
	score float64
}

// Len 返回已索引文档数。
func (ix *index) Len() int { return ix.docCount }

// Terms 返回词表大小（诊断用）。
func (ix *index) Terms() int { return len(ix.postings) }
