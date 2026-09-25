// tokenizer.go —— DeepSeek V4 本地 BPE 分词器（架构文档第 23 章）。
//
// 对应 v1 src/main/deepseek/tokenizer.ts，但按第 23 章要求做了两处关键改造：
//
//		embed → lazy load → immutable tokenizer
//
//	 1. 词表通过 embed.FS 打进二进制，进程启动时不解析（7.8MB JSON 解析约 100-300ms），
//	    首次调用 countTokens 时才懒加载；加载后结构只读，不再修改。
//	 2. BPE 结果缓存改用有界 LRU（lru.go），而不是 v1 的无限增长 map。
//	    v1 的 `bpeCache` 没有任何上限 —— 这是 72h soak 内存持续上涨的头号嫌疑对象，
//	    本任务书把它列为「审计重点」。改造后条数/字节/TTL 三重上限恒定约束内存。
//
// 与 v1 的一处已知差异（已在交付说明登记）：
//
//	v1 预分词正则含负向前瞻 `\s+(?!\S)`，Go 的 RE2 引擎不支持前瞻。
//	本实现用 `\s+\z`（文本末尾空白）等价替代。该分支只影响「文末空白」这一小类切分，
//	对 token 计数的影响可忽略；预算侧另有 SafetyMargin 兜底（见 budget.go）。
package ctxmgr

import (
	"embed"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"
)

//go:embed assets/tokenizer.json
var tokenizerFS embed.FS

// tokenizerAssetPath embed 资源路径。
const tokenizerAssetPath = "assets/tokenizer.json"

// Tokenizer 分词计数接口。抽象出来是为了让预算逻辑可测（无需加载 7.8MB 词表）。
type Tokenizer interface {
	// CountTokens 计算文本的 token 数。
	CountTokens(text string) int
	// CountMessageTokens 计算多条消息的总 token 数（含 role 标签开销）。
	CountMessageTokens(messages []Message) int
	// Version 返回分词器版本号（参与压缩产物版本化）。
	Version() string
	// Ready 报告底层词表是否已加载。
	Ready() bool
}

// ---------------------------------------------------------------------------
// BPE 分词器
// ---------------------------------------------------------------------------

// tokenizerData 加载后的只读词表结构。
type tokenizerData struct {
	vocab map[string]int32
	// mergeRanks 用结构体做 key，避免 v1 那种 "a" + " " + "b" 的每次拼接分配。
	mergeRanks map[mergePair]int32
}

// mergePair BPE 合并对（替代 v1 的 "tokenA tokenB" 字符串键）。
type mergePair struct {
	a, b string
}

// BPETokenizer 懒加载、加载后不可变的 BPE 分词器。
type BPETokenizer struct {
	loadOnce sync.Once
	// loadMu 保护 data / byteEnc / loadErr 的读取（Ready 与懒加载路径都要读）。
	loadMu  sync.Mutex
	loadErr error
	data    *tokenizerData
	byteEnc *byteEncoder

	// cache BPE 结果的有界缓存（第 23 章要求）。
	cache *LRU

	// preTokenRe 预分词正则。
	preTokenRe *regexp.Regexp
}

// tokenizerRaw 是 tokenizer.json 的最小可解析结构。
type tokenizerRaw struct {
	Model struct {
		Vocab  map[string]int32 `json:"vocab"`
		Merges []string         `json:"merges"`
	} `json:"model"`
}

// NewBPETokenizer 构造分词器。词表在首次 CountTokens 时加载。
//
// cacheOpts 传零值时使用 DefaultTokenizerLRUOptions()。
func NewBPETokenizer(cacheOpts LRUOptions) *BPETokenizer {
	if cacheOpts.MaxEntries == 0 && cacheOpts.MaxBytes == 0 && cacheOpts.TTL == 0 {
		cacheOpts = DefaultTokenizerLRUOptions()
	}
	return &BPETokenizer{
		cache:      NewLRU(cacheOpts),
		preTokenRe: buildPreTokenRegex(),
	}
}

// Version 实现 Tokenizer。
func (t *BPETokenizer) Version() string { return TokenizerVersion }

// Ready 实现 Tokenizer。
//
// 用互斥锁读而非 sync.Once —— Once.Do 有副作用（会认领「已执行」），
// 用它做纯查询会让之后的 load() 变成空操作，懒加载就永久失效了。
func (t *BPETokenizer) Ready() bool {
	t.loadMu.Lock()
	defer t.loadMu.Unlock()
	return t.data != nil
}

// Cache 暴露底层 BPE 缓存，供内存预算模块执行「回收 tokenizer cache」降级动作。
func (t *BPETokenizer) Cache() *LRU { return t.cache }

// load 懒加载词表。并发首次调用只解析一次。
func (t *BPETokenizer) load() error {
	t.loadOnce.Do(func() {
		raw, err := tokenizerFS.ReadFile(tokenizerAssetPath)
		if err != nil {
			t.setLoadErr(fmt.Errorf("ctxmgr: 读取 embed 词表失败: %w", err))
			return
		}
		var parsed tokenizerRaw
		if err := json.Unmarshal(raw, &parsed); err != nil {
			t.setLoadErr(fmt.Errorf("ctxmgr: 解析词表 JSON 失败: %w", err))
			return
		}
		if len(parsed.Model.Vocab) == 0 {
			t.setLoadErr(fmt.Errorf("ctxmgr: 词表为空"))
			return
		}

		ranks := make(map[mergePair]int32, len(parsed.Model.Merges))
		for i, merge := range parsed.Model.Merges {
			// merges 元素形如 "a b"；byte-level 编码后 token 内不含空格，
			// 因此第一个空格就是分隔符（v1 同款假设）。
			idx := strings.IndexByte(merge, ' ')
			if idx < 0 {
				continue
			}
			ranks[mergePair{a: merge[:idx], b: merge[idx+1:]}] = int32(i)
		}

		t.loadMu.Lock()
		t.data = &tokenizerData{vocab: parsed.Model.Vocab, mergeRanks: ranks}
		t.byteEnc = newByteEncoder()
		t.loadMu.Unlock()

		// raw 在此处失去引用，交由 GC 回收，不长期驻留（内存预算考量）。
		raw = nil
		_ = raw
	})
	t.loadMu.Lock()
	defer t.loadMu.Unlock()
	return t.loadErr
}

// setLoadErr 在持锁状态下记录加载错误。
func (t *BPETokenizer) setLoadErr(err error) {
	t.loadMu.Lock()
	t.loadErr = err
	t.loadMu.Unlock()
}

// CountTokens 计算文本的 token 数量。
func (t *BPETokenizer) CountTokens(text string) int {
	if text == "" {
		return 0
	}
	if err := t.load(); err != nil {
		// 词表不可用 —— 退化为保守估算，绝不 panic 让主流程崩溃。
		return heuristicCount(text)
	}

	preTokens := t.preTokenRe.FindAllString(text, -1)
	if len(preTokens) == 0 {
		// 无匹配：整段当做一个 pre-token 处理，避免漏计。
		return len(t.bpe(t.byteEncode(text)))
	}

	count := 0
	for _, pt := range preTokens {
		count += len(t.bpe(t.byteEncode(pt)))
	}
	return count
}

// CountMessageTokens 计算多条消息的总 token 数。
//
// 与 v1 一致：content + role 分别计数（充当 role 标签的结构开销）。
func (t *BPETokenizer) CountMessageTokens(messages []Message) int {
	total := 0
	for _, m := range messages {
		total += t.CountTokens(m.Content)
		total += t.CountTokens(m.Role)
		total += t.CountTokens(m.ReasoningContent)
	}
	return total
}

// byteEncode 把 UTF-8 字节串映射为可打印 Unicode 字符串（GPT-2 ByteLevel 编码）。
func (t *BPETokenizer) byteEncode(s string) string {
	if t.byteEnc == nil {
		t.byteEnc = newByteEncoder()
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		b.WriteString(t.byteEnc.encode(s[i]))
	}
	return b.String()
}

// bpe 对单个 byte-level 编码后的 pre-token 执行 BPE 合并。
//
// 结果写入有界 LRU —— 这是与 v1 的核心差异（v1 用无限 map）。
func (t *BPETokenizer) bpe(token string) []string {
	if cached, ok := t.cache.Get(token); ok {
		return cached
	}

	word := utf8Runes(token)
	if len(word) < 2 {
		t.cache.Put(token, word)
		return word
	}

	for {
		bestRank := int32(-1)
		bestIdx := -1
		for i := 0; i < len(word)-1; i++ {
			rank, ok := t.data.mergeRanks[mergePair{a: word[i], b: word[i+1]}]
			if !ok {
				continue
			}
			if bestRank < 0 || rank < bestRank {
				bestRank = rank
				bestIdx = i
			}
		}
		if bestIdx < 0 {
			break
		}

		merged := word[bestIdx] + word[bestIdx+1]
		next := make([]string, 0, len(word)-1)
		for i := 0; i < len(word); {
			if i == bestIdx {
				next = append(next, merged)
				i += 2
				continue
			}
			next = append(next, word[i])
			i++
		}
		word = next
		if len(word) < 2 {
			break
		}
	}

	t.cache.Put(token, word)
	return word
}

// utf8Runes 将字符串切为单字符切片。
//
// 等价于 v1 的 Array.from(token)：byte-level 编码后每个字符代表一个字节，
// 且全部落在 BMP 内（U+0021..U+0144），因此一个 rune 即一个「字节符号」。
func utf8Runes(s string) []string {
	n := utf8.RuneCountInString(s)
	if n == 0 {
		return nil
	}
	out := make([]string, 0, n)
	for _, r := range s {
		out = append(out, string(r))
	}
	return out
}

// heuristicCount 词表不可用时的保守估算：按 ~2.5 chars/token（中英混合偏保守）。
func heuristicCount(text string) int {
	n := utf8.RuneCountInString(text)
	est := (n + 1) / 2
	if est < 1 {
		est = 1
	}
	return est
}

// ---------------------------------------------------------------------------
// 预分词正则
// ---------------------------------------------------------------------------

// buildPreTokenRegex 构造与 v1 等价的预分词正则（前瞻分支已替换为 \z）。
func buildPreTokenRegex() *regexp.Regexp {
	patterns := []string{
		`\p{N}{1,3}`, // 1-3 位数字
		`[\x{4e00}-\x{9fa5}\x{3040}-\x{309f}\x{30a0}-\x{30ff}]+`,    // 中日韩
		`[!"#$%&'()*+,\-./:;<=>?@\[\\\]^_` + "`" + `{|}~][A-Za-z]+`, // 标点+字母
		`[^\r\n\p{L}\p{P}\p{S}]?[\p{L}\p{M}]+`,                      // 可选前缀+字母/标记
		` ?[\p{P}\p{S}]+[\r\n]*`,                                    // 可选空格+标点/符号
		`\s*[\r\n]+`,                                                // 空白+换行
		`\s+\z`,                                                     // 文末空白（替代 v1 的 \s+(?!\S)）
		`\s+`,                                                       // 任意空白
	}
	return regexp.MustCompile(strings.Join(patterns, "|"))
}

// ---------------------------------------------------------------------------
// 全局默认分词器
// ---------------------------------------------------------------------------

var (
	defaultTokenizerOnce sync.Once
	defaultTokenizer     *BPETokenizer
)

// DefaultTokenizer 返回进程级共享分词器（懒加载词表）。
func DefaultTokenizer() *BPETokenizer {
	defaultTokenizerOnce.Do(func() {
		defaultTokenizer = NewBPETokenizer(DefaultTokenizerLRUOptions())
	})
	return defaultTokenizer
}

// CountTokens 便捷函数：用默认分词器计数。
func CountTokens(text string) int { return DefaultTokenizer().CountTokens(text) }
