package openai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/catalog"
	"github.com/ximo888ok-netizen/ximo-agent/internal/provider"
	"github.com/ximo888ok-netizen/ximo-agent/internal/quota"
)

// chatRequest 是 /v1/chat/completions 的入参投影。
//
// 字段取舍：
//   - 声明并**显式 400 拒绝**本网关无法转发的参数（n>1、logprobs、response_format、
//     audio、modalities、stop、legacy functions/function_call）。provider 层只能构造
//     messages/tools/temperature/max_tokens（provider.BuildRequestBody），静默丢弃这些
//     参数会让客户端以为语义生效了，属于假实现。
//   - 纯遥测/调优类字段（user、top_p、frequency_penalty、presence_penalty、seed、
//     parallel_tool_calls、store、metadata、service_tier、reasoning_effort）不声明、
//     被 JSON 解码忽略：它们不改变响应的结构契约，拒绝反而会让常见客户端完全不可用。
//   - 未声明的未知字段同样被忽略（httpx.DecodeJSON 是标准 json.Decoder）。
type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Stream   bool          `json:"stream"`

	Temperature         *float64 `json:"temperature"`
	MaxTokens           *int     `json:"max_tokens"`
	MaxCompletionTokens *int     `json:"max_completion_tokens"`

	StreamOptions *streamOptions `json:"stream_options"`

	Tools      []chatTool      `json:"tools"`
	ToolChoice json.RawMessage `json:"tool_choice"`

	N              *int            `json:"n"`
	Stop           json.RawMessage `json:"stop"`
	Logprobs       *bool           `json:"logprobs"`
	TopLogprobs    *int            `json:"top_logprobs"`
	ResponseFormat json.RawMessage `json:"response_format"`
	Audio          json.RawMessage `json:"audio"`
	Modalities     []string        `json:"modalities"`
	Functions      json.RawMessage `json:"functions"`
	FunctionCall   json.RawMessage `json:"function_call"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// chatMessage 一条对话消息。Content 用 RawMessage：OpenAI 允许字符串或内容块数组。
type chatMessage struct {
	Role             string          `json:"role"`
	Content          json.RawMessage `json:"content"`
	Name             string          `json:"name"`
	ToolCallID       string          `json:"tool_call_id"`
	ToolCalls        []chatToolCall  `json:"tool_calls"`
	ReasoningContent string          `json:"reasoning_content"`
}

type chatToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

type chatTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

// requestError 是入参校验失败，携带对外错误码。
type requestError struct {
	Code    string
	Message string
}

func (e *requestError) Error() string { return e.Message }

// invalidRequest 组包结构/取值错误。
func invalidRequest(format string, args ...any) error {
	return &requestError{Code: "invalid_request_error", Message: fmt.Sprintf(format, args...)}
}

// unsupportedParam 组「本网关无法转发」的错误。
func unsupportedParam(format string, args ...any) error {
	return &requestError{Code: "unsupported_parameter", Message: fmt.Sprintf(format, args...)}
}

// normalizedRequest 是校验并翻译后的请求：内部只认这一份形态。
type normalizedRequest struct {
	RequestID string
	// Model 是客户端请求的模型 ID。对外一律用它，绝不换成上游模型名（防泄漏上游命名）。
	Model        string
	Stream       bool
	IncludeUsage bool

	Messages    []provider.Message
	Tools       []provider.ToolDefinition
	Temperature float64
	// MaxTokens 0 表示未指定（由 provider 侧取 provider 缺省）。
	MaxTokens int

	// InputTokensEstimate 输入 token 粗估（字符数/4，§6）。只用于预占上界，不做计费依据。
	InputTokensEstimate int64
}

// normalizeChatRequest 校验入参并翻译为内部形态。返回的错误均为 *requestError（400）。
func normalizeChatRequest(req chatRequest, requestID string) (*normalizedRequest, error) {
	modelID := strings.TrimSpace(req.Model)
	if modelID == "" {
		return nil, invalidRequest("缺少 model")
	}
	if len(req.Messages) == 0 {
		return nil, invalidRequest("messages 不能为空")
	}

	if err := rejectUnsupportedParams(req); err != nil {
		return nil, err
	}

	tools := req.Tools
	if len(req.ToolChoice) > 0 {
		choice, err := normalizeToolChoice(req.ToolChoice)
		if err != nil {
			return nil, err
		}
		if choice == "none" {
			// tool_choice=none 可以保真地实现：不下发工具（provider 侧无工具即不发
			// tools/tool_choice）。
			tools = nil
		}
	}

	msgs, err := normalizeMessages(req.Messages)
	if err != nil {
		return nil, err
	}
	defs, err := normalizeTools(tools)
	if err != nil {
		return nil, err
	}

	// provider 侧每次都会下发 temperature；缺省必须补 OpenAI 的默认值 1，
	// 否则 0 值会让上游变成确定性采样，与客户端预期不符。
	temperature := 1.0
	if req.Temperature != nil {
		temperature = *req.Temperature
	}
	if temperature < 0 || temperature > 2 {
		return nil, invalidRequest("temperature 必须在 [0,2] 区间内，收到 %v", temperature)
	}

	maxTokens, err := pickMaxTokens(req)
	if err != nil {
		return nil, err
	}

	norm := &normalizedRequest{
		RequestID:   requestID,
		Model:       modelID,
		Stream:      req.Stream,
		Messages:    msgs,
		Tools:       defs,
		Temperature: temperature,
		MaxTokens:   maxTokens,
	}
	if req.StreamOptions != nil {
		norm.IncludeUsage = req.StreamOptions.IncludeUsage
	}
	norm.InputTokensEstimate = estimateInputTokens(msgs, defs)
	return norm, nil
}

// rejectUnsupportedParams 拒绝无法转发的参数（有值才算「出现」）。
func rejectUnsupportedParams(req chatRequest) error {
	if req.N != nil && *req.N != 1 {
		// 上游一次只回一个 choice（provider 的解析也只取 choices[0]），
		// 收了 n=2 却只回一个 choice 属于假实现，直接拒绝。
		return unsupportedParam("n=%d 暂不支持：本网关一次只返回 1 个 choice", *req.N)
	}
	if req.Stop != nil && !isEmptyJSON(req.Stop) {
		return unsupportedParam("stop 暂不支持（provider 层不下发该参数）")
	}
	if req.Logprobs != nil && *req.Logprobs {
		return unsupportedParam("logprobs 暂不支持")
	}
	if req.TopLogprobs != nil && *req.TopLogprobs > 0 {
		return unsupportedParam("top_logprobs 暂不支持")
	}
	if len(req.ResponseFormat) > 0 && !isEmptyJSON(req.ResponseFormat) {
		return unsupportedParam("response_format 暂不支持")
	}
	if len(req.Audio) > 0 && !isEmptyJSON(req.Audio) {
		return unsupportedParam("audio 暂不支持")
	}
	if len(req.Modalities) > 0 {
		return unsupportedParam("modalities 暂不支持（仅文本输出）")
	}
	if len(req.Functions) > 0 && !isEmptyJSON(req.Functions) {
		return unsupportedParam("legacy functions 已废弃，请改用 tools")
	}
	if len(req.FunctionCall) > 0 && !isEmptyJSON(req.FunctionCall) {
		return unsupportedParam("legacy function_call 已废弃，请改用 tool_choice")
	}
	return nil
}

// isEmptyJSON 判定「字段出现了但相当于未设置」：null、空串、空数组、空对象。
func isEmptyJSON(raw json.RawMessage) bool {
	switch s := strings.TrimSpace(string(raw)); s {
	case "", "null", `""`, "[]", "{}":
		return true
	default:
		return false
	}
}

// normalizeToolChoice 只接受 auto / none / 缺省；其余（required、指定函数）无法转发。
func normalizeToolChoice(raw json.RawMessage) (string, error) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return "", nil
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		switch asString {
		case "auto", "":
			return "auto", nil
		case "none":
			return "none", nil
		default:
			return "", unsupportedParam("tool_choice=%q 暂不支持（仅支持 auto/none/缺省）", asString)
		}
	}
	return "", unsupportedParam("tool_choice 指定具体函数暂不支持（仅支持 auto/none/缺省）")
}

// pickMaxTokens 解析输出上限：max_completion_tokens 优先（OpenAI 的新名字），
// 其次 max_tokens；都未给则返回 0，交由 provider 取 provider 缺省值。
func pickMaxTokens(req chatRequest) (int, error) {
	pick := func(v *int, name string) (int, error) {
		if v == nil {
			return 0, nil
		}
		if *v < 0 {
			return 0, invalidRequest("%s 不能为负，收到 %d", name, *v)
		}
		return provider.ClampMaxTokens(*v), nil
	}
	modern, err := pick(req.MaxCompletionTokens, "max_completion_tokens")
	if err != nil {
		return 0, err
	}
	legacy, err := pick(req.MaxTokens, "max_tokens")
	if err != nil {
		return 0, err
	}
	if modern > 0 {
		return modern, nil
	}
	return legacy, nil
}

// normalizeMessages 把 wire 消息翻译为 provider.Message。
func normalizeMessages(in []chatMessage) ([]provider.Message, error) {
	out := make([]provider.Message, 0, len(in))
	for i, m := range in {
		role, err := normalizeRole(m.Role)
		if err != nil {
			return nil, fmt.Errorf("messages[%d]: %w", i, err)
		}
		content, err := contentText(m.Content)
		if err != nil {
			return nil, fmt.Errorf("messages[%d]: %w", i, err)
		}
		msg := provider.Message{
			Role:             role,
			Content:          content,
			ToolCallID:       m.ToolCallID,
			ReasoningContent: m.ReasoningContent,
		}
		for j, tc := range m.ToolCalls {
			if tc.Type != "" && tc.Type != "function" {
				return nil, fmt.Errorf("messages[%d].tool_calls[%d]: %w", i, j,
					unsupportedParam("tool_calls[].type=%q 暂不支持（仅 function）", tc.Type))
			}
			if strings.TrimSpace(tc.Function.Name) == "" {
				return nil, fmt.Errorf("messages[%d].tool_calls[%d]: %w", i, j,
					invalidRequest("缺少 function.name"))
			}
			msg.ToolCalls = append(msg.ToolCalls, provider.ToolCall{
				ID:        tc.ID,
				Name:      tc.Function.Name,
				Arguments: argumentsText(tc.Function.Arguments),
			})
		}
		out = append(out, msg)
	}
	return out, nil
}

// normalizeRole 把 OpenAI 角色映射到 provider 的角色白名单。
func normalizeRole(role string) (provider.Role, error) {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "system", "developer": // developer 是 system 的新名字，语义一致
		return provider.RoleSystem, nil
	case "user":
		return provider.RoleUser, nil
	case "assistant":
		return provider.RoleAssistant, nil
	case "tool":
		return provider.RoleTool, nil
	case "":
		return "", invalidRequest("缺少 role")
	default:
		return "", invalidRequest("不支持的角色 %q", role)
	}
}

// contentText 把消息内容压平成纯文本：字符串直接用，内容块数组只接受 text，
// 图像/音频块显式报错（V1 不做多模态，静默丢图会让模型看到残缺输入）。
func contentText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return asString, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", invalidRequest("content 必须是字符串或内容块数组")
	}
	var b strings.Builder
	for _, p := range parts {
		switch p.Type {
		case "text", "input_text", "":
			b.WriteString(p.Text)
		case "image_url", "image", "input_audio", "audio":
			return "", unsupportedParam("content.type=%q 暂不支持（V1 仅支持文本）", p.Type)
		default:
			return "", unsupportedParam("content.type=%q 暂不支持", p.Type)
		}
	}
	return b.String(), nil
}

// argumentsText 归一工具调用参数：字符串原样，对象/数组压成 JSON 文本。
func argumentsText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return asString
	}
	compact := compactJSON(raw)
	if compact == "" {
		return ""
	}
	return compact
}

// compactJSON 去掉首尾空白；无法解析时返回空串（provider 侧按「无参数」处理，
// 不让一个坏参数把整次请求打掉——参数格式错误由上游返回更准确的 400）。
func compactJSON(raw json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return ""
	}
	return buf.String()
}

// normalizeTools 翻译工具定义。
func normalizeTools(in []chatTool) ([]provider.ToolDefinition, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]provider.ToolDefinition, 0, len(in))
	for i, t := range in {
		if t.Type != "" && t.Type != "function" {
			return nil, fmt.Errorf("tools[%d]: %w", i,
				unsupportedParam("tools[].type=%q 暂不支持（仅 function）", t.Type))
		}
		name := strings.TrimSpace(t.Function.Name)
		if name == "" {
			return nil, fmt.Errorf("tools[%d]: %w", i, invalidRequest("缺少 function.name"))
		}
		params := map[string]any{}
		if len(t.Function.Parameters) > 0 && !isEmptyJSON(t.Function.Parameters) {
			if err := json.Unmarshal(t.Function.Parameters, &params); err != nil {
				return nil, fmt.Errorf("tools[%d]: %w", i,
					invalidRequest("function.parameters 不是合法 JSON 对象"))
			}
		}
		if _, ok := params["type"]; !ok {
			// 补一个合法的空 schema：provider 会原样下发 parameters，
			// null/缺字段会让部分上游以 invalid tool schema 拒绝整个请求。
			params["type"] = "object"
			if _, ok := params["properties"]; !ok {
				params["properties"] = map[string]any{}
			}
		}
		out = append(out, provider.ToolDefinition{
			Name:        name,
			Description: t.Function.Description,
			Parameters:  params,
		})
	}
	return out, nil
}

// estimateInputTokens 用「字符数 / 4」粗估输入 token（§6，quota.EstimateInputTokens）。
//
// 这是**粗估**：中文接近 1 字符/token，英文散文约 4，代码/JSON 更高。它只决定预占
// 上界，不参与计费（结算只用上游真实 usage）。工具 schema 是 JSON，按字节近似。
func estimateInputTokens(msgs []provider.Message, tools []provider.ToolDefinition) int64 {
	chars := 0
	for _, m := range msgs {
		chars += utf8.RuneCountInString(m.Content)
		chars += utf8.RuneCountInString(m.ReasoningContent)
		for _, tc := range m.ToolCalls {
			chars += utf8.RuneCountInString(tc.Name) + len(tc.Arguments)
		}
	}
	for _, t := range tools {
		chars += utf8.RuneCountInString(t.Name) + utf8.RuneCountInString(t.Description)
		if t.Parameters != nil {
			if data, err := json.Marshal(t.Parameters); err == nil {
				chars += len(data)
			}
		}
	}
	return quota.EstimateInputTokens(chars)
}

// completionRequest 生成发往某个具体候选的上游请求。
//
// 每换一个候选都要重建：Model 要用该候选的上游模型名（ProviderModel.UpstreamModelID），
// 对外响应仍用客户端请求的模型名。
func (n *normalizedRequest) completionRequest(cand catalog.Candidate) provider.CompletionRequest {
	upstreamModel := strings.TrimSpace(cand.UpstreamModel)
	if upstreamModel == "" {
		// 目录映射没填上游模型名时按同名转发（比直接报错更可能成功；
		// 映射缺失本身应由管理后台校验挡住）。
		upstreamModel = n.Model
	}
	return provider.CompletionRequest{
		Meta:        provider.RequestMeta{RequestID: n.RequestID},
		ProviderID:  cand.Provider.ID,
		Model:       upstreamModel,
		Messages:    n.Messages,
		Tools:       n.Tools,
		Temperature: n.Temperature,
		MaxTokens:   n.MaxTokens,
	}
}
