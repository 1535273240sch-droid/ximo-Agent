package memory

import (
	"sort"
	"strings"
	"unicode"
)

// 本文件是**词法**检索的实现，供进程内后端使用。
//
// 为什么不做向量检索：那需要额外一个 embedding 服务（DeepSeek 没有 embeddings 接口），
// 而记忆是增强能力、不是运行前提——为了它引入一个必须常驻的外部服务不成比例。
// 词法检索的代价是「同义改写命中率低」（"深色界面" 命不中 "暗色主题"），这是
// 有意的取舍，不是缺陷；以后要升级只需替换本文件的打分函数。

// tokenize 把查询切成检索词：
//   - ASCII 连续字母/数字算一个词（小写化）；
//   - 中日韩字符既取单字，也取相邻二元组（中文没有空格，二元组能显著提升召回）；
//   - 空白与标点忽略；结果去重，避免重复词放大分数。
func tokenize(s string) []string {
	var out []string
	var word strings.Builder
	var cjk []rune

	flushWord := func() {
		if word.Len() > 0 {
			out = append(out, strings.ToLower(word.String()))
			word.Reset()
		}
	}
	flushCJK := func() {
		if len(cjk) == 0 {
			return
		}
		for _, r := range cjk {
			out = append(out, string(r))
		}
		for i := 0; i+1 < len(cjk); i++ {
			out = append(out, string(cjk[i:i+2]))
		}
		cjk = cjk[:0]
	}

	for _, r := range s {
		switch {
		case unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r):
			flushWord()
			cjk = append(cjk, r)
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			flushCJK()
			word.WriteRune(r)
		default:
			flushWord()
			flushCJK()
		}
	}
	flushWord()
	flushCJK()

	seen := make(map[string]bool, len(out))
	uniq := out[:0]
	for _, t := range out {
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		uniq = append(uniq, t)
	}
	return uniq
}

// score 给一条记忆与一次查询打分：命中越多、词越长分越高；整串包含再给一次加成；
// 最后按查询词数归一化，避免长查询天然得分高。
func score(memory, query string, qTokens []string) float64 {
	if len(qTokens) == 0 {
		return 0
	}
	lower := strings.ToLower(memory)
	var s float64
	for _, t := range qTokens {
		if n := strings.Count(lower, t); n > 0 {
			s += float64(len([]rune(t))) * float64(n)
		}
	}
	if q := strings.ToLower(strings.TrimSpace(query)); q != "" && strings.Contains(lower, q) {
		s += float64(len([]rune(q))) * 2
	}
	return s / float64(len(qTokens))
}

// rank 按分数降序（同分按时间新的在前）排序并截断到 topK。
func rank(recs []Record, query string, qTokens []string, topK int) []Record {
	type scored struct {
		rec Record
		s   float64
	}
	kept := make([]scored, 0, len(recs))
	for _, rec := range recs {
		s := score(rec.Memory, query, qTokens)
		if s <= 0 {
			continue
		}
		rec.Score = s
		kept = append(kept, scored{rec: rec, s: s})
	}
	sort.SliceStable(kept, func(i, j int) bool {
		if kept[i].s != kept[j].s {
			return kept[i].s > kept[j].s
		}
		return kept[i].rec.CreatedAt > kept[j].rec.CreatedAt
	})
	if topK > 0 && len(kept) > topK {
		kept = kept[:topK]
	}
	out := make([]Record, 0, len(kept))
	for _, k := range kept {
		out = append(out, k.rec)
	}
	return out
}
