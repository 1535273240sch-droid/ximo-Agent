package anthropic

import "encoding/json"

// 本文件只放 Anthropic Messages 的线协议类型（入站请求、出站响应、SSE 事件）。
// 字段名与 Anthropic 官方 API 逐字对应；未列出的字段（cache_control、thinking、
// 未来新增字段）由 encoding/json 忽略，不影响解析。

// messagesRequest 是 POST /v1/messages 的入站请求。
//
// MaxTokens / Temperature 用指针以区分「未提供」与「提供了 0」：
// max_tokens 是 Anthropic 的必填字段（缺失要报 400），temperature 缺省应为 1.0
// （而不是 Go 零值 0，否则会把客户端没指定的请求变成贪心解码）。
type messagesRequest struct {
	Model         string          `json:"model"`
	MaxTokens     *int64          `json:"max_tokens"`
	System        json.RawMessage `json:"system"`
	Messages      []inMessage     `json:"messages"`
	Tools         []inTool        `json:"tools"`
	ToolChoice    json.RawMessage `json:"tool_choice"`
	Temperature   *float64        `json:"temperature"`
	TopP          *float64        `json:"top_p"`
	StopSequences []string        `json:"stop_sequences"`
	Stream        bool            `json:"stream"`
	Metadata      json.RawMessage `json:"metadata"`
}

// inMessage 一条入站消息。Content 可能是字符串或 content block 数组，故保留原始 JSON。
type inMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// inBlock 一个入站 content block。所有 block 类型共用一套字段（与 v1 provider 的
// 宽结构风格一致），按 Type 取用：
//
//	text        → Text
//	image       → Source（V1 不支持，见 doc.go 缺口 1）
//	tool_use    → ID / Name / Input（assistant 消息）
//	tool_result → ToolUseID / Content / IsError（user 消息）
type inBlock struct {
	Type string `json:"type"`

	Text string `json:"text"`

	Source *inImageSource `json:"source"`

	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`

	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// inImageSource 图片块来源。本包只解析不转发 —— 目的是在拒绝时能说清是 base64 还是 url。
type inImageSource struct {
	Type      string `json:"type"` // base64 | url
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
	URL       string `json:"url"`
}

// inTool 入站工具定义。
type inTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// inToolChoice 入站 tool_choice。disable_parallel_tool_use 无法转发（provider 无此字段），
// 一并忽略。
type inToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

// --- 出站 ---

// messageResponse 是 Anthropic Messages 响应。StopReason/StopSequence 用指针，
// 因为流式 message_start 里二者必须是 null。
type messageResponse struct {
	ID           string     `json:"id"`
	Type         string     `json:"type"`
	Role         string     `json:"role"`
	Model        string     `json:"model"`
	Content      []any      `json:"content"`
	StopReason   *string    `json:"stop_reason"`
	StopSequence *string    `json:"stop_sequence"`
	Usage        usageBlock `json:"usage"`
}

// textBlock 文本内容块。Text 不加 omitempty：Anthropic 客户端要求该字段恒存在。
type textBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// toolUseBlock 工具调用内容块。
type toolUseBlock struct {
	Type  string          `json:"type"`
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// usageBlock 用量。V1 只填 input/output（Anthropic 的 cache_* 字段本层无从得知）。
type usageBlock struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

// --- SSE 事件（契约 §11.5：event: <type> + data: {...}）---

type messageStartEvent struct {
	Type    string          `json:"type"`
	Message messageResponse `json:"message"`
}

type contentBlockStartEvent struct {
	Type         string `json:"type"`
	Index        int    `json:"index"`
	ContentBlock any    `json:"content_block"`
}

type contentBlockDeltaEvent struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
	Delta any    `json:"delta"`
}

type contentBlockStopEvent struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
}

type messageDeltaEvent struct {
	Type  string           `json:"type"`
	Delta messageDeltaBody `json:"delta"`
	Usage usageBlock       `json:"usage"`
}

type messageDeltaBody struct {
	StopReason   *string `json:"stop_reason"`
	StopSequence *string `json:"stop_sequence"`
}

type messageStopEvent struct {
	Type string `json:"type"`
}

type errorEvent struct {
	Type  string         `json:"type"`
	Error errorEventBody `json:"error"`
}

type errorEventBody struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// textDelta 文本增量。
type textDelta struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// inputJSONDelta 工具入参增量（partial_json 是 JSON 文本分片，需客户端拼接）。
type inputJSONDelta struct {
	Type        string `json:"type"`
	PartialJSON string `json:"partial_json"`
}
