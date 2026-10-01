package engine

import (
	"context"
	"strings"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/agent"
	"github.com/ximo888ok-netizen/ximo-agent/internal/ports"
	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// MemoryTurn 是一次已完成 run 的可抽取内容。
//
// 只有「这一轮」的提问与最终答复：引擎每次 run 只拿到当前提示词，完整会话历史
// 由前端拼在系统提示词里，本进程取不到原文。编造一份引擎手上没有的历史才是
// 真正有害的。
type MemoryTurn struct {
	RunID     string
	SessionID string
	Prompt    string
	Answer    string
}

// MemoryPort 是引擎可选的长期记忆协作者。
//
// 两个方法的签名都刻意不是「错误敏感」的，因为它们的调用点分别在 run 的入口与
// 收尾路径上，而那里没有「记忆出问题该怎么办」的答案：
//
//   - Recall 只返回可注入的文本；没有相关记忆、服务不可达、超时、鉴权失败，
//     一律返回空串。一个 run 不能因为记忆服务挂了而失败或卡住。
//   - Remember 不返回任何东西，且必须不阻塞：它只把这一轮投进有界队列。
//
// 端口声明在消费方（engine）而不是实现方（internal/memory），是沿用本仓库
// ports 包的既定约定：实现方通过 bootstrap 的适配器注入，两边可以各自演化。
type MemoryPort interface {
	Recall(ctx context.Context, query string) string
	Remember(turn MemoryTurn)
}

// MemoryRecallItem 是一条被召回的长期记忆（结构化形态）。
//
// 只带 id / 正文 / via 三样，形状与 types.EventMemoryRecalled 的载荷一致
// （audit 4.5 第 7 条：{count, items:[{id,text,via}]}）。via 是"为什么想起它"
// 的路径描述，种子为 "seed"。
type MemoryRecallItem struct {
	ID   string
	Text string
	Via  string
}

// MemoryRecallReporter 是 MemoryPort 的**可选**扩展：能给出结构化召回条目的
// 实现额外满足它。
//
// 为什么是可选接口而不是给 MemoryPort 加方法：MemoryPort 的既有实现（以及测试
// 替身）不该为了一个"展示层"的需求被迫实现新方法；而且不是每个后端都能说清
// "经由哪条路径想起的"。引擎用类型断言探测它——与 Recoverer / ToolDecider 的
// 能力探测完全同一条思路：没有该能力时退化成只有文本，行为与从前逐字节一致。
type MemoryRecallReporter interface {
	RecallDetailed(ctx context.Context, query string) (text string, items []MemoryRecallItem)
}

// injectMemoryMessage 在会话开头插入一条独立的记忆 system 消息。
//
// 位置：稳定系统提示词之后、用户消息之前，即 [system, memory, user]。
// 这个位置是刻意的，理由有三条：
//
//  1. prompt cache：25KB+ 的系统提示词（模式提示词 + 专家人格 + 技能）仍是请求
//     最前面的连续字节，新增的记忆消息不参与它的哈希，所以不会让缓存整段失效。
//     v1 吃过这个亏，v2 用 internal/context 的 PrefixShape 把它固化成了不变量。
//  2. 只插一次：插入发生在 run 创建时，之后消息列表只追加。重放、恢复、压缩都
//     不会再动它，因此同一个 run 的位置在字节上是稳定的。
//  3. 空块不插：没有相关记忆时完全不产生这条消息——「今天没有记忆」不该在请求
//     里留下任何多余字节。
//
// conv 为 nil 或 block 为空时是 no-op。
func injectMemoryMessage(conv *agent.Conversation, block string) {
	if conv == nil || strings.TrimSpace(block) == "" {
		return
	}
	at := 0
	if len(conv.Messages) > 0 && conv.Messages[0].Role == ports.RoleSystem {
		at = 1
	}
	conv.Messages = append(conv.Messages, ports.Message{})
	copy(conv.Messages[at+1:], conv.Messages[at:])
	conv.Messages[at] = ports.Message{Role: ports.RoleSystem, Content: block}
}

// recallMemory 向记忆端口取一段可注入的文本；未装配或没有命中时返回空串。
//
// 记忆端口自己负责超时（实现方的单次调用超时远小于模型调用超时），这里不再
// 叠加一层，避免两层超时互相掩盖真实的失败原因。
func (e *Engine) recallMemory(ctx context.Context, prompt string) string {
	if e == nil || e.deps.Memory == nil {
		return ""
	}
	if strings.TrimSpace(prompt) == "" {
		return ""
	}
	return e.deps.Memory.Recall(ctx, prompt)
}

// recallMemoryDetailed 是 recallMemory 的结构化版本：端口实现了
// MemoryRecallReporter 时同时拿到召回条目，否则退化成只有文本、没有条目。
//
// 它永不返回错误，理由与 MemoryPort 的签名约定相同：调用点在 run 的入口，
// 那里没有"记忆出问题该怎么办"的答案。
func (e *Engine) recallMemoryDetailed(ctx context.Context, prompt string) (string, []MemoryRecallItem) {
	if e == nil || e.deps.Memory == nil {
		return "", nil
	}
	if strings.TrimSpace(prompt) == "" {
		return "", nil
	}
	if reporter, ok := e.deps.Memory.(MemoryRecallReporter); ok {
		return reporter.RecallDetailed(ctx, prompt)
	}
	return e.deps.Memory.Recall(ctx, prompt), nil
}

// emitMemoryRecalled 把本次召回的条目写成 durable 事件，供 Work Log 展示
// 「回忆 N 条记忆」并解释每条为什么被想起。
//
// 事件只是展示层的增强，不是 run 的前提：写日志失败时静默跳过（不改变 run 的
// 任何状态），这与"记忆不可用不阻塞 run"的既有约定一致。
//
// 正文再次过 types.RedactString：记忆内容在写入时已经脱敏过一次，但事件是另一
// 条出站路径（会进事件日志、outbox、IPC），出口处再脱敏一次是纵深防御，不是
// 冗余——任何一条新的出站路径都不该假设上游一定干净。
func (e *Engine) emitMemoryRecalled(ctx context.Context, runID, sessionID string, items []MemoryRecallItem) {
	if e == nil || e.deps.Events == nil || len(items) == 0 {
		return
	}
	list := make([]map[string]any, 0, len(items))
	for _, it := range items {
		list = append(list, map[string]any{
			"id":   it.ID,
			"text": types.RedactString(it.Text),
			"via":  it.Via,
		})
	}
	ev := types.Event{
		RunID:     runID,
		SessionID: sessionID,
		Type:      types.EventMemoryRecalled,
		State:     types.StateQueued,
		Timestamp: time.Now(),
		Data:      map[string]any{"count": len(list), "items": list},
	}
	seq, err := e.deps.Events.Append(ctx, runID, ev)
	if err != nil {
		return
	}
	ev.Seq = seq
	e.enqueueOutbox(ctx, ev)
	e.coalescer.Publish(ev)
}

// rememberTurn 把一轮完成的对话投递给记忆端口。
//
// 只在「有最终答复的终态」才投递：失败的中途、只有工具调用的空答复、以及用户
// 取消但什么都没产出的 run，都没有可抽取的内容，投递它们只会让记忆里多出噪声。
func (e *Engine) rememberTurn(turn MemoryTurn) {
	if e == nil || e.deps.Memory == nil {
		return
	}
	if turn.RunID == "" || strings.TrimSpace(turn.Prompt) == "" || strings.TrimSpace(turn.Answer) == "" {
		return
	}
	e.deps.Memory.Remember(turn)
}
