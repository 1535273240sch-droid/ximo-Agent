package anthropic

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ximo888ok-netizen/ximo-agent/internal/provider"
	"github.com/ximo888ok-netizen/ximo-agent/internal/quota"
)

// defaultTemperature 是入站未指定 temperature 时发给上游的值。
// Anthropic 的缺省值是 1.0；provider.CompletionRequest.Temperature 的零值是 0（贪心解码），
// 直接透传会把「没指定」变成「要求确定性输出」，因此这里显式补 1.0。
const defaultTemperature = 1.0

// blockType 常量：Anthropic content block 类型。
const (
	blockText       = "text"
	blockImage      = "image"
	blockToolUse    = "tool_use"
	blockToolResult = "tool_result"
)

// stop reason 常量（Anthropic 出站）。
const (
	stopReasonEndTurn      = "end_turn"
	stopReasonMaxTokens    = "max_tokens"
	stopReasonToolUse      = "tool_use"
	stopReasonStopSequence = "stop_sequence"
)

// translated 是一次入站请求翻译后、与「选中哪个候选」无关的上游调用参数。
type translated struct {
	model         string
	maxTokens     int
	messages      []provider.Message
	tools         []provider.ToolDefinition
	temperature   float64
	stopSequences []string
	stream        bool

	// warnings 收集被降级/忽略的入站参数（如无法转发的 tool_choice）。
	// 只进日志，不进响应 —— 客户端拿到的东西必须符合它发来的协议。
	warnings []string
}

// completionRequest 按候选的上游模型名构造一次上游调用请求。
//
// 注意 Timeout 留 0：超时由 upstream.Pool 按 ProviderSpec.TimeoutMS 构建的客户端决定，
// handler 不再叠加一层，避免两处超时口径不一致。
func (t *translated) completionRequest(upstreamModel, requestID string) provider.CompletionRequest {
	return provider.CompletionRequest{
		Meta:        provider.RequestMeta{RequestID: requestID},
		Model:       upstreamModel,
		Messages:    t.messages,
		Tools:       t.tools,
		Temperature: t.temperature,
		MaxTokens:   t.maxTokens,
	}
}

// estimatedInputTokens 用「字符数 / 4」粗估输入 token（契约 §6：**只用于预占上界**，
// 绝不作计费依据）。中英混排下这个系数必然有偏差，代价是预占额偏小或偏大，
// 结算时一律以真实 usage 修正。
func (t *translated) estimatedInputTokens() int64 {
	chars := 0
	for _, m := range t.messages {
		chars += len(m.Content)
	}
	for _, tool := range t.tools {
		chars += len(tool.Name) + len(tool.Description)
	}
	return quota.EstimateInputTokens(chars)
}

// translateRequest 把 Anthropic 入站请求翻译成上游调用参数。
//
// 校验失败返回 *requestError（400）。翻译策略：
//   - 一条 Anthropic 消息可能拆成多条 OpenAI 消息（user 消息里的 tool_result 要变成
//     role=tool 的独立消息，OpenAI 要求它紧跟带 tool_calls 的 assistant 消息）；
//   - 同一消息里的多个 text block 以 "\n" 连接（Anthropic 服务端同样把相邻文本块视作连续文本）；
//   - 全空的文本消息会被丢弃（OpenAI 不接受 content 为空串的消息），该消息若带 tool_calls 则保留。
func translateRequest(req *messagesRequest) (*translated, error) {
	out := &translated{
		model:         strings.TrimSpace(req.Model),
		temperature:   defaultTemperature,
		stopSequences: req.StopSequences,
		stream:        req.Stream,
	}
	if out.model == "" {
		return nil, badRequest("model", "model: field required")
	}
	// max_tokens 是 Anthropic 的必填字段（契约任务项 1）。缺了它无法算出预占上界，
	// 也就无法保证「额度不足不进上游」，所以这里必须硬校验而不是默认一个值。
	if req.MaxTokens == nil {
		return nil, badRequest("max_tokens", "max_tokens: field required")
	}
	if *req.MaxTokens <= 0 {
		return nil, badRequest("max_tokens", "max_tokens: must be >= 1")
	}
	// 超上限时由 provider.ClampMaxTokens 统一钳制（上游请求体的硬上限）。
	out.maxTokens = int(*req.MaxTokens)
	if req.Temperature != nil {
		out.temperature = *req.Temperature
	}

	system, err := translateSystem(req.System)
	if err != nil {
		return nil, err
	}

	msgs, err := translateMessages(req.Messages)
	if err != nil {
		return nil, err
	}
	if system != "" {
		// Anthropic 的 system 是顶层字段，OpenAI 侧必须成为首条消息。
		msgs = append([]provider.Message{{Role: provider.RoleSystem, Content: system}}, msgs...)
	}
	out.messages = msgs

	tools, err := translateTools(req.Tools)
	if err != nil {
		return nil, err
	}
	out.tools = tools

	if warn := translateToolChoice(req.ToolChoice, len(tools) > 0); warn != "" {
		out.warnings = append(out.warnings, warn)
	}
	if req.TopP != nil {
		out.warnings = append(out.warnings, "top_p 无法转发（provider.CompletionRequest 无该字段），已忽略")
	}
	if len(req.Metadata) > 0 && string(req.Metadata) != "null" {
		out.warnings = append(out.warnings, "metadata 无法转发，已忽略")
	}

	if len(out.messages) == 0 {
		return nil, badRequest("messages", "messages: at least one non-empty message is required")
	}
	return out, nil
}

// translateSystem 处理顶层 system：既接受字符串，也接受 text block 数组。
// 数组里的非 text 块（含 image）一律拒绝 —— Anthropic 的 system 也只允许文本块，
// 放行会让我们在上游侧悄悄改变语义。
func translateSystem(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var blocks []inBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", badRequest("system", "system: expected a string or an array of text blocks")
	}
	parts := make([]string, 0, len(blocks))
	for i, b := range blocks {
		if b.Type != blockText {
			return "", badRequest("unsupported_content_block", "system[%d]: unsupported block type %q (only text is supported)", i, b.Type)
		}
		parts = append(parts, b.Text)
	}
	return strings.Join(parts, "\n"), nil
}

// translateMessages 翻译消息数组。
func translateMessages(msgs []inMessage) ([]provider.Message, error) {
	out := make([]provider.Message, 0, len(msgs))
	for i, m := range msgs {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		text, blocks, err := decodeContent(m.Content)
		if err != nil {
			return nil, fmt.Errorf("messages[%d]: %w", i, err)
		}
		switch role {
		case "user":
			converted, err := translateUserMessage(i, text, blocks)
			if err != nil {
				return nil, err
			}
			out = append(out, converted...)
		case "assistant":
			converted, err := translateAssistantMessage(i, text, blocks)
			if err != nil {
				return nil, err
			}
			out = append(out, converted...)
		default:
			return nil, badRequest("messages", "messages[%d].role: must be %q or %q", i, "user", "assistant")
		}
	}
	return out, nil
}

// translateUserMessage 翻译一条 user 消息：可能产出多条 OpenAI 消息。
//
// 顺序敏感：tool_result 之前累积的文本先落一条 user 消息，再落 tool 消息，
// 保证 tool 消息紧跟在它回应的 assistant(tool_calls) 之后。
func translateUserMessage(idx int, text string, blocks []inBlock) ([]provider.Message, error) {
	out := make([]provider.Message, 0, len(blocks)+1)
	pending := make([]string, 0, len(blocks))
	flush := func() {
		if len(pending) == 0 {
			return
		}
		out = append(out, provider.Message{Role: provider.RoleUser, Content: strings.Join(pending, "\n")})
		pending = pending[:0]
	}

	if text != "" {
		pending = append(pending, text)
	}
	for bi, b := range blocks {
		switch b.Type {
		case blockText:
			pending = append(pending, b.Text)
		case blockImage:
			return nil, imageUnsupportedError(fmt.Sprintf("messages[%d].content[%d]", idx, bi), b.Source)
		case blockToolResult:
			if strings.TrimSpace(b.ToolUseID) == "" {
				return nil, badRequest("tool_result", "messages[%d].content[%d]: tool_result.tool_use_id is required", idx, bi)
			}
			result, err := toolResultText(fmt.Sprintf("messages[%d].content[%d]", idx, bi), b.Content)
			if err != nil {
				return nil, err
			}
			flush()
			out = append(out, provider.Message{
				Role:       provider.RoleTool,
				Content:    result,
				ToolCallID: b.ToolUseID,
			})
		case blockToolUse:
			return nil, badRequest("tool_use", "messages[%d].content[%d]: tool_use blocks are only valid in assistant messages", idx, bi)
		default:
			return nil, badRequest("unsupported_content_block", "messages[%d].content[%d]: unsupported block type %q", idx, bi, b.Type)
		}
	}
	flush()
	return out, nil
}

// translateAssistantMessage 翻译一条 assistant 消息：文本 + tool_use → OpenAI assistant 消息。
func translateAssistantMessage(idx int, text string, blocks []inBlock) ([]provider.Message, error) {
	texts := make([]string, 0, len(blocks)+1)
	if text != "" {
		texts = append(texts, text)
	}
	calls := make([]provider.ToolCall, 0, len(blocks))
	for bi, b := range blocks {
		switch b.Type {
		case blockText:
			texts = append(texts, b.Text)
		case blockImage:
			return nil, imageUnsupportedError(fmt.Sprintf("messages[%d].content[%d]", idx, bi), b.Source)
		case blockToolUse:
			if strings.TrimSpace(b.Name) == "" {
				return nil, badRequest("tool_use", "messages[%d].content[%d]: tool_use.name is required", idx, bi)
			}
			if strings.TrimSpace(b.ID) == "" {
				// 没有 id 就无法与后续 tool_result 配对，OpenAI 侧也会被拒。
				return nil, badRequest("tool_use", "messages[%d].content[%d]: tool_use.id is required", idx, bi)
			}
			calls = append(calls, provider.ToolCall{
				ID:        b.ID,
				Name:      b.Name,
				Arguments: argumentsOrDefault(b.Input),
			})
		case blockToolResult:
			return nil, badRequest("tool_result", "messages[%d].content[%d]: tool_result blocks are only valid in user messages", idx, bi)
		default:
			return nil, badRequest("unsupported_content_block", "messages[%d].content[%d]: unsupported block type %q", idx, bi, b.Type)
		}
	}

	content := strings.Join(texts, "\n")
	if content == "" && len(calls) == 0 {
		// 全空消息对上游没有信息量（内容为空且无工具调用的 assistant 消息会被上游拒绝）。
		return nil, nil
	}
	return []provider.Message{{
		Role:      provider.RoleAssistant,
		Content:   content,
		ToolCalls: calls,
	}}, nil
}

// toolResultText 把 tool_result 的 content（字符串或 text block 数组）压成纯文本。
func toolResultText(path string, raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var blocks []inBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", badRequest("tool_result", "%s: tool_result.content must be a string or an array of blocks", path)
	}
	parts := make([]string, 0, len(blocks))
	for i, b := range blocks {
		switch b.Type {
		case blockText:
			parts = append(parts, b.Text)
		case blockImage:
			return "", imageUnsupportedError(fmt.Sprintf("%s.content[%d]", path, i), b.Source)
		default:
			return "", badRequest("unsupported_content_block", "%s.content[%d]: unsupported block type %q", path, i, b.Type)
		}
	}
	return strings.Join(parts, "\n"), nil
}

// imageUnsupportedError 是 V1 的显式缺口：provider.Message 只有字符串 content，
// 冻结的 provider 客户端没有任何 content-parts 入口，图片无法转发（doc.go 缺口 1）。
func imageUnsupportedError(path string, src *inImageSource) error {
	kind := ""
	if src != nil && src.Type != "" {
		kind = " (" + src.Type + ")"
	}
	return badRequest("unsupported_content_block",
		"%s: image blocks%s are not supported by this gateway build (upstream client has no multimodal content-parts path)", path, kind)
}

// translateTools 把 Anthropic 工具定义翻成 OpenAI function 形状。
//
// 已支持：name / description / input_schema。未支持（不假装）：Anthropic 的
// cache_control（缓存断点）、input_schema 之外的扩展字段，均在 JSON 解析时被忽略。
func translateTools(tools []inTool) ([]provider.ToolDefinition, error) {
	if len(tools) == 0 {
		return nil, nil
	}
	out := make([]provider.ToolDefinition, 0, len(tools))
	for i, t := range tools {
		name := strings.TrimSpace(t.Name)
		if name == "" {
			return nil, badRequest("tools", "tools[%d].name is required", i)
		}
		params := map[string]any(nil)
		if len(t.InputSchema) > 0 && string(t.InputSchema) != "null" {
			if err := json.Unmarshal(t.InputSchema, &params); err != nil {
				return nil, badRequest("input_schema", "tools[%d].input_schema: must be a JSON object", i)
			}
		}
		if params == nil {
			// 空 schema 补一个合法空对象，避免上游以 invalid tool schema 拒绝。
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, provider.ToolDefinition{
			Name:        name,
			Description: t.Description,
			Parameters:  params,
		})
	}
	return out, nil
}

// translateToolChoice 校验入站 tool_choice。
//
// provider.BuildRequestBody 在有 tools 时把 tool_choice 硬编码为 "auto"，
// 因此 {"type":"any"} / {"type":"tool"} 无法转发（doc.go 缺口 3）。客户端在明确要求
// 「必须调用某个工具」时降级为 auto 会改变行为，故这里返回告警串由 handler 记 Warn 日志：
// 选择「继续服务 + 留日志」而不是「拒绝」——拒绝会让依赖强制工具调用的客户端整体不可用，
// 而降级的后果（模型可能回文本）在响应里可见。
func translateToolChoice(raw json.RawMessage, hasTools bool) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var tc inToolChoice
	if err := json.Unmarshal(raw, &tc); err != nil {
		return "tool_choice 解析失败，已按 auto 处理"
	}
	switch tc.Type {
	case "", "auto":
		return ""
	case "any", "tool":
		return fmt.Sprintf("tool_choice.type=%q 无法转发（上游固定 auto），已降级", tc.Type)
	default:
		return fmt.Sprintf("tool_choice.type=%q 未知，已按 auto 处理", tc.Type)
	}
}

// decodeContent 解析消息 content：返回字符串形态的文本（若 content 是字符串）与 block 数组。
func decodeContent(raw json.RawMessage) (string, []inBlock, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil, nil
	}
	var blocks []inBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", nil, badRequest("content", "content must be a string or an array of content blocks")
	}
	return "", blocks, nil
}

// argumentsOrDefault 把 tool_use.input 渲染为 OpenAI 的 arguments 字符串。
// 缺失/为空时给 "{}"：OpenAI 侧要求 arguments 是合法 JSON 文本。
func argumentsOrDefault(input json.RawMessage) string {
	if len(input) == 0 || string(input) == "null" {
		return "{}"
	}
	if !json.Valid(input) {
		return "{}"
	}
	return string(input)
}

// --- 出站翻译 ---

// translateResponse 把上游非流式响应翻成 Anthropic Messages 响应。
//
// 文本块在内容为空时省略（Anthropic 在无输出时返回 content: []），
// 工具调用一律附带 input（arguments 非法 JSON 时退化为 {}，客户端仍能看到 id/name）。
func translateResponse(resp provider.CompletionResponse, model string, stopSeqs []string) *messageResponse {
	content := resp.Content
	matched := ""
	if idx, seq := findStop(content, stopSeqs); idx >= 0 {
		content = content[:idx]
		matched = seq
	}

	blocks := make([]any, 0, 1+len(resp.ToolCalls))
	if content != "" {
		blocks = append(blocks, textBlock{Type: blockText, Text: content})
	}
	for _, tc := range resp.ToolCalls {
		blocks = append(blocks, toolUseBlock{
			Type:  blockToolUse,
			ID:    tc.ID,
			Name:  tc.Name,
			Input: argumentsJSON(tc.Arguments),
		})
	}

	reason := stopReasonFor(resp.FinishReason, len(resp.ToolCalls) > 0, matched)
	out := &messageResponse{
		ID:         newID("msg"),
		Type:       "message",
		Role:       "assistant",
		Model:      model,
		Content:    blocks,
		StopReason: &reason,
		Usage:      usageFromTokens(resp.Usage),
	}
	if matched != "" {
		out.StopSequence = &matched
	}
	return out
}

// stopReasonFor 映射结束原因：stop→end_turn、length→max_tokens、tool_calls→tool_use。
// 命中客户端 stop 序列时优先报 stop_sequence（与 Anthropic 语义一致）。
func stopReasonFor(finish provider.FinishReason, hasToolUse bool, matched string) string {
	switch {
	case matched != "":
		return stopReasonStopSequence
	case finish == provider.FinishLength:
		return stopReasonMaxTokens
	case hasToolUse:
		return stopReasonToolUse
	default:
		return stopReasonEndTurn
	}
}

// argumentsJSON 把上游 arguments 文本原样作为 input 输出（合法 JSON 对象才透传）。
func argumentsJSON(args string) json.RawMessage {
	trimmed := strings.TrimSpace(args)
	if trimmed == "" || !json.Valid([]byte(trimmed)) {
		return json.RawMessage("{}")
	}
	return json.RawMessage(trimmed)
}

// usageFromTokens 把上游 usage 翻成 Anthropic usage。usage 为 nil（上游没回）时记 0：
// 契约 §6 明确禁止用估算值计费，宁可记 0 也不臆造 token 数。
func usageFromTokens(u *provider.TokenUsage) usageBlock {
	if u == nil {
		return usageBlock{}
	}
	return usageBlock{
		InputTokens:  int64(u.PromptTokens),
		OutputTokens: int64(u.CompletionTokens),
	}
}
