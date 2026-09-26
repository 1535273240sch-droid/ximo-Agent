package memory

import (
	"context"
	"sort"
	"strings"
)

// BlockHeader 是记忆块的首行。
//
// 记忆以**独立 system 消息**注入，位置在稳定系统提示词之后、用户消息之前：
// 这样 25KB+ 的系统提示词仍是请求最前面的那段字节，新增记忆不会让服务商的
// prompt cache 整段失效（v1 prefix-shape 的教训，v2 的 internal/context/prefixshape.go
// 把它固化成了不变量）。首行的固定文案让「这段是不是记忆」一眼可辨。
const BlockHeader = "--- 长期记忆 (mem0) ---"

// Block 把召回结果渲染成注入用的文本块；没有可用条目时返回空串。
//
// 预算语义：逐条累加，单条超预算就跳过它继续看下一条（宁可少一条，也不把半条
// 记忆截断后塞进上下文）；一条都放不下或者原本就没有内容时返回空串，调用方
// 据此完全跳过注入——「今天没有相关记忆」不该在请求里留下任何多余字节。
func Block(records []Record, maxChars int) string {
	if len(records) == 0 {
		return ""
	}
	if maxChars <= 0 {
		maxChars = DefaultRecallMaxChars
	}

	// mem0 自己按相关度排序，这里再兜一次，保证「预算不够时先丢最不相关的」。
	ordered := make([]Record, len(records))
	copy(ordered, records)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Score > ordered[j].Score })

	var b strings.Builder
	b.WriteString(BlockHeader)
	used := len(BlockHeader)
	lines := 0
	seen := make(map[string]struct{}, len(ordered))

	for _, r := range ordered {
		text := strings.Join(strings.Fields(r.Memory), " ")
		if text == "" {
			continue
		}
		if _, dup := seen[text]; dup {
			continue
		}
		line := "\n- " + text
		if used+len(line) > maxChars {
			continue
		}
		b.WriteString(line)
		used += len(line)
		lines++
		seen[text] = struct{}{}
	}
	if lines == 0 {
		return ""
	}
	return b.String()
}

// Recall 检索与 query 相关的长期记忆并渲染成可注入的文本块。
//
// 它永远不返回错误：记忆不可用（未启用、连不上、超时、鉴权失败）与「没有相关
// 记忆」对调用方是同一种结果——这一轮没有记忆，对话照常继续。失败会打一条
// 告警日志并计入统计，而不是静默吞掉。
func (s *Service) Recall(ctx context.Context, query string) string {
	if s == nil || s.backend == nil || !s.cfg.Active() {
		return ""
	}
	if strings.TrimSpace(query) == "" {
		return ""
	}
	s.recallCalls.Add(1)

	records, err := s.backend.Search(ctx, query, SearchOptions{})
	if err != nil {
		s.recallErrors.Add(1)
		s.logWarn(ctx, "长期记忆召回失败，本轮按「无记忆」继续", err)
		return ""
	}
	block := Block(records, s.cfg.RecallMaxChars)
	if block == "" {
		return ""
	}
	s.recallHits.Add(1)
	s.recallChars.Add(uint64(len(block)))
	return block
}
