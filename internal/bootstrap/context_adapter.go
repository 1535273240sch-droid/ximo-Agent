package bootstrap

import (
	"context"
	"encoding/json"

	ctxmgr "github.com/ximo888ok-netizen/ximo-agent/internal/context"
	"github.com/ximo888ok-netizen/ximo-agent/internal/ports"
	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// contextAdapter 让 internal/context（包名 ctxmgr）的压缩器满足端口的压缩契约。
//
// 语义落差（如实交代，不伪造数据）：
//   - 端口带 Tier/ProtectRecent 入参，但 ctxmgr 自己按用量比例决定档位、自己按
//     AgentConfig.RecentKeep 保护近期轮次，因此这两个入参被忽略。
//   - 端口要求返回 Folded（折叠了多少条消息）与 SummaryMessage，ctxmgr 不产生
//     这两个量：它只发 NeedLLMSummary 信号，由 Agent 循环决定要不要真的摘要。
//     所以 Folded 记 0、SummaryMessage 为 nil。
//
// 该退化是安全的，因为 Engine 实际只依赖返回值的 Messages 与 Stuck 两个字段
// （见 agent/compaction.go 的读取方式）。
type contextAdapter struct {
	mgr *ctxmgr.ContextManager
}

var _ ports.ContextManager = (*contextAdapter)(nil)

func newContextAdapter(mgr *ctxmgr.ContextManager) *contextAdapter {
	return &contextAdapter{mgr: mgr}
}

// Compact 实现 ports.ContextManager。
func (a *contextAdapter) Compact(ctx context.Context, session ports.ContextSession) (ports.CompactionResult, error) {
	window := session.Window
	if window <= 0 {
		window = a.mgr.ProviderWindow()
	}

	snapshot := ctxmgr.SessionSnapshot{
		SessionID: session.SessionID,
		Messages:  toCtxMessages(session.Messages),
		// ctxmgr 只接受单一 prompt 用量，这里取端口 Usage 的 prompt 侧。
		PromptTokens: session.Usage.PromptTokens,
	}

	out, err := a.mgr.CompactWithWindow(ctx, snapshot, window)
	if err != nil {
		return ports.CompactionResult{}, err
	}

	return ports.CompactionResult{
		Tier:     types.CompactionTier(out.Stats.Tier),
		Messages: fromCtxMessages(out.Messages),
		Folded:   0,
		Stuck:    out.StuckPaused,
	}, nil
}

// toCtxMessages 把端口消息转成 ctxmgr 消息。
//
// ctxmgr.Message.ToolCalls 是 json.RawMessage（保持 OpenAI 的数组形态），而端口
// 用结构化的 []types.ToolCall，因此这里需要序列化一次。
func toCtxMessages(msgs []ports.Message) []ctxmgr.Message {
	out := make([]ctxmgr.Message, 0, len(msgs))
	for _, m := range msgs {
		var raw json.RawMessage
		if len(m.ToolCalls) > 0 {
			if b, err := json.Marshal(m.ToolCalls); err == nil {
				raw = b
			}
		}
		out = append(out, ctxmgr.Message{
			Role:             string(m.Role),
			Content:          m.Content,
			ToolCalls:        raw,
			ToolCallID:       m.ToolCallID,
			ReasoningContent: m.ReasoningContent,
		})
	}
	return out
}

// fromCtxMessages 把压缩后的 ctxmgr 消息转回端口消息。
func fromCtxMessages(msgs []ctxmgr.Message) []ports.Message {
	out := make([]ports.Message, 0, len(msgs))
	for _, m := range msgs {
		var calls []types.ToolCall
		if len(m.ToolCalls) > 0 {
			// 解析失败时留空：压缩后的历史里工具调用信息丢失不影响继续对话，
			// 只是模型看不到那一步的调用细节，比整轮失败更可取。
			_ = json.Unmarshal(m.ToolCalls, &calls)
		}
		out = append(out, ports.Message{
			Role:             ports.MessageRole(m.Role),
			Content:          m.Content,
			ReasoningContent: m.ReasoningContent,
			ToolCalls:        calls,
			ToolCallID:       m.ToolCallID,
		})
	}
	return out
}
