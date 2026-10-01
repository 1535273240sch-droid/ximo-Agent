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

// RecallItem 是一条被召回的条目（结构化形态）。
//
// 它与 Record 的区别只在用途：Record 是后端之间的通用载荷，而 RecallItem 是给
// 事件流看的——Work Log 要展示"回忆了哪几条、经由什么路径想起的"，因此只带
// id / 正文 / via 三样，不带 embedding、score 这些渲染不上的东西。
type RecallItem struct {
	ID   string
	Text string
	Via  string
}

// blockRenderer 是"后端自带渲染器"的可选能力。
//
// SynapseBackend 的 RenderBlock 会按文档 4.5 第 6 条的格式渲染（按实体分组、
// 带 ↳ 关联 路径行与结尾的"可能已过时"句），比通用的 Block 更能解释"为什么
// 想起它"。没有该能力的后端（mem0 / embedded）继续用 Block。
type blockRenderer interface {
	RenderBlock(records []Record, maxChars int) string
}

// RecallDetailed 是 Recall 的结构化版本：除了可注入文本，还返回本次召回的具体
// 条目，供引擎发出 memory.recalled 事件。
//
// items 是**后端实际返回的召回集合**（即"被激活的记忆"），不按注入块的字符预算
// 再裁一次：预算裁剪决定的是"哪些写进提示词"，而事件要回答的是"这次想起了
// 哪些"。两者语义不同，混在一起会让 Work Log 少报它该点亮的节点。
//
// 与 Recall 一样，它永不返回错误。
func (s *Service) RecallDetailed(ctx context.Context, query string) (string, []RecallItem) {
	if s == nil || s.backend == nil || !s.cfg.Active() {
		return "", nil
	}
	if strings.TrimSpace(query) == "" {
		return "", nil
	}
	s.recallCalls.Add(1)

	records, err := s.backend.Search(ctx, query, SearchOptions{})
	if err != nil {
		s.recallErrors.Add(1)
		s.logWarn(ctx, "长期记忆召回失败，本轮按「无记忆」继续", err)
		return "", nil
	}
	render := Block
	if br, ok := s.backend.(blockRenderer); ok {
		render = br.RenderBlock
	}
	block := render(records, s.cfg.RecallMaxChars)
	if block == "" {
		return "", nil
	}
	s.recallHits.Add(1)
	s.recallChars.Add(uint64(len(block)))

	items := make([]RecallItem, 0, len(records))
	for _, r := range records {
		text := strings.Join(strings.Fields(r.Memory), " ")
		if text == "" {
			continue
		}
		via, _ := r.Metadata["via"].(string)
		items = append(items, RecallItem{ID: r.ID, Text: text, Via: via})
	}
	return block, items
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
