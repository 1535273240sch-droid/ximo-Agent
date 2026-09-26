package openai

import (
	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/httpx"
	"github.com/ximo888ok-netizen/ximo-agent/internal/provider"
)

// 对外响应体。逐字段白名单：只出现 OpenAI 协议字段与客户端请求的模型名。
// 上游 base URL、API Key、内部 provider 字段（ID/Endpoint/APIKeyRef/ConfigJSON）
// 一律不出现在任何响应里。
type completionResponseBody struct {
	ID      string             `json:"id"`
	Object  string             `json:"object"`
	Created int64              `json:"created"`
	Model   string             `json:"model"`
	Choices []completionChoice `json:"choices"`
	Usage   usageBody          `json:"usage"`
}

type completionChoice struct {
	Index        int               `json:"index"`
	Message      completionMessage `json:"message"`
	FinishReason string            `json:"finish_reason"`
}

type completionMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	// ReasoningContent 是 DeepSeek 系的思考内容，OpenAI 客户端不认识该字段时忽略即可
	// （客户端传入的 reasoning_content 我们也会原样回传，思考模型的多轮要求）。
	ReasoningContent string         `json:"reasoning_content,omitempty"`
	ToolCalls        []wireToolCall `json:"tool_calls,omitempty"`
}

type wireToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function wireFunction `json:"function"`
}

type wireFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// usageBody 与 OpenAI 的 usage 字段一致。上游没报 usage 时一律如实写 0，
// 不按估算值编造 token 数（§6.1）。
type usageBody struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

// 流式分片。
type streamChunkBody struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []streamChoice `json:"choices"`
	// Usage 只在客户端显式要求 stream_options.include_usage、且上游真的报了 usage 时出现。
	Usage *usageBody `json:"usage,omitempty"`
}

type streamChoice struct {
	Index int         `json:"index"`
	Delta streamDelta `json:"delta"`
	// FinishReason 非终止分片为 null（用指针才能编码成 null 而不是省略）。
	FinishReason *string `json:"finish_reason"`
}

type streamDelta struct {
	Role             string              `json:"role,omitempty"`
	Content          string              `json:"content,omitempty"`
	ReasoningContent string              `json:"reasoning_content,omitempty"`
	ToolCalls        []wireToolCallDelta `json:"tool_calls,omitempty"`
}

type wireToolCallDelta struct {
	Index    int                `json:"index"`
	ID       string             `json:"id,omitempty"`
	Type     string             `json:"type,omitempty"`
	Function *wireFunctionDelta `json:"function,omitempty"`
}

type wireFunctionDelta struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// streamErrorBody 是 SSE 流中途失败时发出的错误事件体（沿用 httpx 的统一错误形状，
// 并带上 request_id 便于对账）。
type streamErrorBody struct {
	Error httpx.ErrorDetail `json:"error"`
}

// completionID 由请求 ID 派生，便于把一次响应对到一次请求与一次 usage 行。
func completionID(requestID string) string {
	if requestID == "" {
		return "chatcmpl-unknown"
	}
	return "chatcmpl-" + requestID
}

// usageFrom 把上游 usage 投影为对外 usage；nil（上游未报）时按 §6.1 如实返回 0。
func usageFrom(u *provider.TokenUsage) usageBody {
	if u == nil {
		return usageBody{}
	}
	total := u.TotalTokens
	if total == 0 {
		total = u.PromptTokens + u.CompletionTokens
	}
	return usageBody{
		PromptTokens:     int64(u.PromptTokens),
		CompletionTokens: int64(u.CompletionTokens),
		TotalTokens:      int64(total),
	}
}

// wireFinishReason 把 provider 的结束原因映射为 OpenAI 取值。
//
// FinishError/FinishCancelled 不会走到这里（错误在调用层就终止了）；万一出现，
// 归为 stop，避免给客户端一个它无法解析的 finish_reason。
func wireFinishReason(f provider.FinishReason) string {
	switch f {
	case provider.FinishToolCalls:
		return "tool_calls"
	case provider.FinishLength:
		return "length"
	default:
		return "stop"
	}
}

// toolCallsFrom 投影工具调用结果。
func toolCallsFrom(calls []provider.ToolCall) []wireToolCall {
	if len(calls) == 0 {
		return nil
	}
	out := make([]wireToolCall, 0, len(calls))
	for _, c := range calls {
		out = append(out, wireToolCall{
			ID:       c.ID,
			Type:     "function",
			Function: wireFunction{Name: c.Name, Arguments: c.Arguments},
		})
	}
	return out
}
