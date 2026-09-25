// contract.go —— 与 08 号冻结契约（internal/types）的适配层。
//
// 背景：09 号审核报告（问题 15）指出本任务的 provider 契约与 08 号
// `internal/types/provider.go` 存在重复定义，属阻断级问题。
//
// 08 号的裁决（docs/接口裁决记录.md A5）把 `CompletionRequest/Response/StreamChunk`
// 的**规范形态**冻结在 `internal/types`，并把所有权记为本任务（06）。本文件是
// 「按契约形状对外，按内部形状实现」之间的唯一转换点：
//
//	internal/types  ←→  internal/provider
//
// 为什么不直接把 provider 的类型改成 types 的别名：
//
//  1. `types.CompletionRequest` 缺少本任务实现所必需的三项 —— `ThinkingMode`、
//     `ReasoningEffort`、`Timeout`。DeepSeek 的思考模式与温度互斥、以及单次请求超时，
//     都是第 18 章调用链的实现前提，删掉它们会让重试/退避逻辑无从落地。
//  2. `types.StreamChunk` 用 `Kind` 判别式联合，而本任务内部用「字段是否为空」的
//     宽结构（v1 同款）。改成判别式要重写 SSE 累积与 200+ 测试，风险高收益低。
//  3. `types.ToolDefinition.InputSchema` 是 `json.RawMessage`，本任务是
//     `map[string]any` —— 前者更适合跨进程传输（04↔05），后者更适合本层构造请求体。
//
// 因此本层选择「适配」而非「别名」。等 08 号补齐 types 缺失字段后，可进一步收敛；
// 在此之前，本文件保证两个方向都有确定、可测试的转换路径。
//
// 注意：本文件依赖 08 号的 `internal/types`，与该包一样属于「集成后由 08 提供」的
// module 级共享包（同 07 号的 observability）。本任务目录内不重复实现该包。
package provider

import (
	"encoding/json"
	"fmt"

	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// argsToJSONString renders the frozen contract's map-shaped tool arguments as
// the raw JSON text this layer works in. The in-process contract carries
// Arguments as map[string]any (task 02 is its dominant consumer), while the SSE
// accumulator here keeps the raw string it received from the provider, so this
// is the one place the two representations meet.
func argsToJSONString(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	data, err := json.Marshal(args)
	if err != nil {
		return ""
	}
	return string(data)
}

// jsonStringToArgs is the inverse of argsToJSONString: it decodes raw JSON text
// into the contract's map form. A malformed payload yields nil rather than an
// error so a single bad tool call cannot fail an entire completion.
func jsonStringToArgs(raw string) map[string]any {
	if raw == "" {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

// ToTypesRequest 把内部请求转换为 08 号冻结的契约形态。
// Meta 被展平为 RequestID/RunID/TurnID/Attempt —— 这正是 A5 裁定的四元组对齐锚点。
// 其余字段按语义直译；ProviderID/ThinkingMode/ReasoningEffort/Timeout 不在冻结契约内，
// 由调用方另行携带（见下方 ToTypesRequestWithExtras）。
func (r CompletionRequest) ToTypesRequest() types.CompletionRequest {
	msgs := make([]types.Message, 0, len(r.Messages))
	for _, m := range r.Messages {
		msgs = append(msgs, m.ToTypesMessage())
	}

	tools := make([]types.ToolDefinition, 0, len(r.Tools))
	for _, t := range r.Tools {
		tools = append(tools, t.ToTypesToolDefinition())
	}

	return types.CompletionRequest{
		RequestID:   r.Meta.RequestID,
		RunID:       r.Meta.RunID,
		TurnID:      r.Meta.TurnID,
		Attempt:     r.Meta.Attempt,
		Model:       r.Model,
		Messages:    msgs,
		Tools:       tools,
		Temperature: r.Temperature,
		MaxTokens:   r.MaxTokens,
	}
}

// ToTypesRequestOptions 携带冻结契约未覆盖、但本层实现必需的字段。
//
// 这是给 08 号的证据：把这四项补进 `types.CompletionRequest` 之后，
// 本适配层即可退化为纯字段拷贝。
type ToTypesRequestOptions struct {
	SessionID       string
	ProviderID      string
	ThinkingMode    bool
	ReasoningEffort ReasoningEffort
	TimeoutMS       int64
}

// ToTypesRequestWithExtras 转换并记录契约缺口（用于集成期核对）。
//
// 返回的 extras 非空即表示冻结契约缺少这些字段 —— 08 号可据此判断
// 是否需要扩展 `types.CompletionRequest`。
func (r CompletionRequest) ToTypesRequestWithExtras() (types.CompletionRequest, ToTypesRequestOptions) {
	out := r.ToTypesRequest()
	extras := ToTypesRequestOptions{
		ProviderID:      r.ProviderID,
		ThinkingMode:    r.ThinkingMode,
		ReasoningEffort: r.ReasoningEffort,
	}
	if r.Timeout > 0 {
		extras.TimeoutMS = r.Timeout.Milliseconds()
	}
	return out, extras
}

// FromTypesResponse 把 08 号契约的响应转回本层形态。
func FromTypesResponse(resp types.CompletionResponse, meta RequestMeta) CompletionResponse {
	out := CompletionResponse{
		FinishReason:     fromTypesStopReason(resp.StopReason),
		Content:          resp.Content,
		ReasoningContent: resp.Reasoning,
		Emitted:          resp.Emitted,
		Meta:             meta,
	}
	if resp.RequestID != "" && out.Meta.RequestID == "" {
		out.Meta.RequestID = resp.RequestID
	}

	for _, tc := range resp.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, ToolCall{
			ID:        tc.ID,
			Name:      tc.Name,
			Arguments: argsToJSONString(tc.Arguments), // 契约用 map，本层用 string
		})
	}

	u := FromTypesUsage(resp.Usage)
	if u != nil {
		out.Usage = u
	}

	if resp.ErrorText != "" {
		out.FinishReason = FinishError
	}
	return out
}

// ToTypesResponse 把本层响应转换为冻结契约形态。
func (r CompletionResponse) ToTypesResponse() types.CompletionResponse {
	out := types.CompletionResponse{
		RequestID:  r.Meta.RequestID,
		Content:    r.Content,
		Reasoning:  r.ReasoningContent,
		StopReason: toTypesStopReason(r.FinishReason),
		Emitted:    r.Emitted,
	}
	for _, tc := range r.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, types.ToolCall{
			ID:        tc.ID,
			Name:      tc.Name,
			Arguments: jsonStringToArgs(tc.Arguments),
		})
	}
	if r.Usage != nil {
		out.Usage = r.Usage.ToTypesUsage()
	}
	return out
}

// ToTypesChunk 把本层流式分片转换为冻结契约的判别式联合。
//
// 一个内部分片可能同时带 content 与 tool_call 增量；契约用 `Kind` 判别，
// 因此这里按优先级拆成多个契约分片（内容优先，其次是工具调用增量）。
// 调用方应遍历返回的切片并依次投递。
func (c StreamChunk) ToTypesChunk() []types.StreamChunk {
	out := make([]types.StreamChunk, 0, 3)

	if c.Content != "" {
		out = append(out, types.StreamChunk{
			RequestID: c.Meta.RequestID,
			Kind:      types.ChunkContent,
			Delta:     c.Content,
		})
	}
	if c.ReasoningContent != "" {
		out = append(out, types.StreamChunk{
			RequestID: c.Meta.RequestID,
			Kind:      types.ChunkReasoning,
			Delta:     c.ReasoningContent,
		})
	}
	for _, d := range c.ToolCalls {
		out = append(out, types.StreamChunk{
			RequestID:     c.Meta.RequestID,
			Kind:          types.ChunkToolCall,
			ToolCallIndex: d.Index,
			ToolCall: &types.ToolCall{
				ID:        d.ID,
				Name:      d.Name,
				Arguments: jsonStringToArgs(d.Arguments),
			},
		})
	}
	if c.Usage != nil {
		u := c.Usage.ToTypesUsage()
		out = append(out, types.StreamChunk{
			RequestID: c.Meta.RequestID,
			Kind:      types.ChunkUsage,
			Usage:     &u,
		})
	}
	if c.Err != nil {
		out = append(out, types.StreamChunk{
			RequestID: c.Meta.RequestID,
			Kind:      types.ChunkError,
			ErrorText: c.Err.Error(),
		})
	}
	if c.Done {
		out = append(out, types.StreamChunk{
			RequestID: c.Meta.RequestID,
			Kind:      types.ChunkDone,
		})
	}

	return out
}

// ToTypesMessage 转换单条消息。
func (m Message) ToTypesMessage() types.Message {
	out := types.Message{
		Role:       types.MessageRole(m.Role),
		Content:    m.Content,
		ToolCallID: m.ToolCallID,
		Reasoning:  m.ReasoningContent,
	}
	for _, tc := range m.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, types.ToolCall{
			ID:        tc.ID,
			Name:      tc.Name,
			Arguments: jsonStringToArgs(tc.Arguments),
		})
	}
	return out
}

// ToTypesToolDefinition 转换工具定义。
//
// 两侧的 schema 表示不同：本层用 `map[string]any`，冻结契约（D-4 裁决后由
// 任务 02 的定义主导）用 `Parameters map[string]any`。二者同为 map，故这里是
// 直接赋值；空 schema 补一个合法的空对象，避免服务端以 invalid tool schema 拒绝。
func (t ToolDefinition) ToTypesToolDefinition() types.ToolDefinition {
	params := t.Parameters
	if params == nil {
		params = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	return types.ToolDefinition{
		Name:        t.Name,
		Description: t.Description,
		Parameters:  params,
	}
}

// FromTypesUsage 把契约的用量转回本层形态（nil 表示契约为零值）。
func FromTypesUsage(u types.TokenUsage) *TokenUsage {
	if u == (types.TokenUsage{}) {
		return nil
	}
	// 契约只给 CachedPromptTokens（命中数），未命中数按 prompt - hit 派生 ——
	// 与 v1 normaliseUsage 及本层 NormalizeUsage 的派生规则一致。
	miss := 0
	if u.CachedPromptTokens > 0 && u.PromptTokens > u.CachedPromptTokens {
		miss = u.PromptTokens - u.CachedPromptTokens
	}
	return &TokenUsage{
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		TotalTokens:      u.TotalTokens,
		CacheHitTokens:   u.CachedPromptTokens,
		CacheMissTokens:  miss,
		ReasoningTokens:  u.ReasoningTokens,
	}
}

// ToTypesUsage 把本层用量转为契约形态。
func (u TokenUsage) ToTypesUsage() types.TokenUsage {
	return types.TokenUsage{
		PromptTokens:       u.PromptTokens,
		CompletionTokens:   u.CompletionTokens,
		TotalTokens:        u.TotalTokens,
		CachedPromptTokens: u.CacheHitTokens,
		ReasoningTokens:    u.ReasoningTokens,
	}
}

// FromTypesStopReason 契约结束原因 → 本层结束原因。
func fromTypesStopReason(s types.StopReason) FinishReason {
	switch s {
	case types.StopEnd:
		return FinishStop
	case types.StopToolCall:
		return FinishToolCalls
	case types.StopLength:
		return FinishLength
	case types.StopError:
		return FinishError
	case types.StopCancelled:
		return FinishCancelled
	default:
		return FinishReason(s)
	}
}

// toTypesStopReason 本层结束原因 → 契约结束原因。
func toTypesStopReason(f FinishReason) types.StopReason {
	switch f {
	case FinishStop:
		return types.StopEnd
	case FinishToolCalls:
		return types.StopToolCall
	case FinishLength:
		return types.StopLength
	case FinishError:
		return types.StopError
	case FinishCancelled:
		return types.StopCancelled
	default:
		return types.StopReason(f)
	}
}

// ValidateContractAlignment 自检适配层与冻结契约的一致性。
//
// 供集成期调用：任何一侧改了字段而忘了同步，这里会报出来。
func ValidateContractAlignment() error {
	// 消息角色必须一一对应。
	pairs := []struct {
		mine   Role
		theirs types.MessageRole
	}{
		{RoleSystem, types.RoleSystem},
		{RoleUser, types.RoleUser},
		{RoleAssistant, types.RoleAssistant},
		{RoleTool, types.RoleTool},
	}
	for _, p := range pairs {
		if string(p.mine) != string(p.theirs) {
			return fmt.Errorf("provider: 角色字面量不一致 %q vs %q", p.mine, p.theirs)
		}
	}

	// 结束原因必须一一对应（用一个往返断言覆盖全部取值）。
	for _, f := range []FinishReason{FinishStop, FinishToolCalls, FinishLength, FinishError, FinishCancelled} {
		if got := fromTypesStopReason(toTypesStopReason(f)); got != f {
			return fmt.Errorf("provider: 结束原因往返失败 %q → %q", f, got)
		}
	}

	return nil
}
