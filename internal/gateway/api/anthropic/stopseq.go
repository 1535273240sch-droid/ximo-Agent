package anthropic

import (
	"strings"
	"unicode/utf8"
)

// 本文件实现 stop_sequences 的输出侧模拟（doc.go 缺口 2）。
//
// 为什么是「模拟」而不是「转发」：provider.CompletionRequest 没有 Stop 字段，
// provider.BuildRequestBody 也没有 stop 参数，冻结的上游客户端无法把停止序列发出去。
// 直接忽略 stop_sequences 会让客户端收到它明确要求截断之后的内容（例如 Claude Code
// 用 "\n\nHuman:" 之类的序列做分隔，多出的文本会污染它的解析），因此这里在**本层**
// 按序列截断，客户端可见语义（文本 + stop_reason=stop_sequence）与 Anthropic 一致。
//
// 差别（必须对外说明）：上游仍会生成到自然结束，所以真实计费可能高于客户端所见内容。

// findStop 返回 text 中最早出现的停止序列位置与命中序列；无命中返回 (-1, "")。
// 同一位置多个序列命中时按入参顺序取第一个，保证结果稳定可测。
func findStop(text string, seqs []string) (int, string) {
	idx, matched := -1, ""
	for _, s := range seqs {
		if s == "" {
			continue
		}
		p := strings.Index(text, s)
		if p >= 0 && (idx < 0 || p < idx) {
			idx, matched = p, s
		}
	}
	return idx, matched
}

// stopFilter 是流式场景的增量截断器。
//
// 它对「停止序列可能跨越分片边界」这一情形是安全的：任何时刻最多滞留
// maxLen-1 个字节不发出，因此当命中发生时，命中点一定还在滞留区里，绝不会已经把
// 命中之后的内容发给客户端（证明见下）。
//
// 证明（归纳）：设 T 时刻已见文本长度 S。若某次命中起点为 p，则命中必然在 ≥ p+len(seq)
// 个字节到达时才被发现，故在更早的 T'（S' < p+len(seq)）时未被发现，即 p > S' - len(seq)
// ≥ S' - maxLen，于是 T' 时刻发出的前缀长度 max(0, S'-maxLen+1) ≤ p。归纳成立。
type stopFilter struct {
	seqs []string
	// keep 是必须滞留的尾部字节数（最长序列长度 - 1）。
	keep int

	held    string
	matched string
	done    bool
}

// newStopFilter 构造截断器；seqs 为空（或全为空串）时等价于直通。
func newStopFilter(seqs []string) *stopFilter {
	f := &stopFilter{seqs: seqs}
	for _, s := range seqs {
		if n := len(s) - 1; n > f.keep {
			f.keep = n
		}
	}
	return f
}

// feed 喂入一段增量，返回可立即下发的文本与是否已命中停止序列。
// 命中后 held 被丢弃（命中点之后的文本一律不下发），后续 feed 恒返回空。
func (f *stopFilter) feed(text string) (string, bool) {
	if f == nil {
		return text, false
	}
	if f.done {
		return "", true
	}
	if text == "" {
		return "", false
	}

	f.held += text
	if idx, seq := findStop(f.held, f.seqs); idx >= 0 {
		f.matched, f.done = seq, true
		f.held = ""
		return "", true
	}
	if f.keep == 0 {
		out := f.held
		f.held = ""
		return out, false
	}
	if len(f.held) <= f.keep {
		return "", false
	}
	cut := runeSafeCut(f.held, len(f.held)-f.keep)
	out := f.held[:cut]
	f.held = f.held[cut:]
	return out, false
}

// flush 返回流结束后仍滞留的尾部。命中序列时返回空（尾部属于被截断的部分）。
func (f *stopFilter) flush() string {
	if f == nil || f.done {
		return ""
	}
	out := f.held
	f.held = ""
	return out
}

// stopped 报告是否已命中停止序列。
func (f *stopFilter) stopped() bool { return f != nil && f.done }

// matchedSequence 返回命中的停止序列（未命中为空串）。
func (f *stopFilter) matchedSequence() string {
	if f == nil {
		return ""
	}
	return f.matched
}

// runeSafeCut 返回不超过 n 的最大 UTF-8 字符边界下标，避免把多字节字符切成两半。
func runeSafeCut(s string, n int) int {
	if n <= 0 {
		return 0
	}
	if n >= len(s) {
		return len(s)
	}
	for i := n; i > 0; i-- {
		if utf8.RuneStart(s[i]) {
			return i
		}
	}
	return 0
}
