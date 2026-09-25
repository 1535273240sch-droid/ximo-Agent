package provider

import (
	"testing"

	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// TestContractAlignmentSelfCheck 确认适配层与 08 号冻结契约的字面量一致。
func TestContractAlignmentSelfCheck(t *testing.T) {
	if err := ValidateContractAlignment(); err != nil {
		t.Fatalf("适配层与冻结契约不一致: %v", err)
	}
}

// TestRequestToTypesPreservesMetaQuadruple 确认 A5 裁定的四元组对齐锚点无损传递。
//
// 08 号在 docs/接口裁决记录.md A5 里明确：RequestID/RunID/TurnID/Attempt
// 是 06/02/07 三方的对齐锚点。这个测试保证本任务往外传时四个字段都到位。
func TestRequestToTypesPreservesMetaQuadruple(t *testing.T) {
	req := CompletionRequest{
		Meta:        RequestMeta{RequestID: "req-1", RunID: "run-1", TurnID: "turn-1", Attempt: 3},
		Model:       "deepseek-v4",
		Messages:    []Message{{Role: RoleUser, Content: "hi"}},
		Temperature: 0.3,
		MaxTokens:   4096,
	}

	got := req.ToTypesRequest()

	if got.RequestID != "req-1" {
		t.Errorf("RequestID = %q", got.RequestID)
	}
	if got.RunID != "run-1" {
		t.Errorf("RunID = %q", got.RunID)
	}
	if got.TurnID != "turn-1" {
		t.Errorf("TurnID = %q", got.TurnID)
	}
	if got.Attempt != 3 {
		t.Errorf("Attempt = %d", got.Attempt)
	}
	if got.Model != "deepseek-v4" || got.Temperature != 0.3 || got.MaxTokens != 4096 {
		t.Errorf("标量字段传递有误: %+v", got)
	}
}

// TestRequestToTypesDocumentsContractGap 确认契约缺口被显式报出（而非静默丢弃）。
//
// 冻结契约缺 ThinkingMode/ReasoningEffort/Timeout —— 本任务用 extras 明确记录，
// 供 08 号判断是否需要扩展 types.CompletionRequest。
func TestRequestToTypesDocumentsContractGap(t *testing.T) {
	req := CompletionRequest{
		Meta:            RequestMeta{RequestID: "r"},
		ThinkingMode:    true,
		ReasoningEffort: EffortHigh,
		ProviderID:      "deepseek",
	}

	_, extras := req.ToTypesRequestWithExtras()

	if !extras.ThinkingMode {
		t.Error("extras 应记录 ThinkingMode")
	}
	if extras.ReasoningEffort != EffortHigh {
		t.Errorf("extras.ReasoningEffort = %q", extras.ReasoningEffort)
	}
	if extras.ProviderID != "deepseek" {
		t.Errorf("extras.ProviderID = %q", extras.ProviderID)
	}
}

// TestMessageToTypesRoundTrips 确认消息转换保留全部字段。
func TestMessageToTypesRoundTrips(t *testing.T) {
	m := Message{
		Role:             RoleAssistant,
		Content:          "回答",
		ReasoningContent: "思考",
		ToolCalls: []ToolCall{
			{ID: "c1", Name: "file_read", Arguments: `{"path":"a.txt"}`},
		},
	}

	got := m.ToTypesMessage()

	if string(got.Role) != string(RoleAssistant) {
		t.Errorf("Role = %q", got.Role)
	}
	if got.Content != "回答" || got.Reasoning != "思考" {
		t.Errorf("内容/思考丢失: %+v", got)
	}
	if len(got.ToolCalls) != 1 {
		t.Fatalf("工具调用数 = %d", len(got.ToolCalls))
	}
	if got.ToolCalls[0].Arguments["path"] != "a.txt" {
		t.Errorf("Arguments = %v（应解码为 map 并保留 path）", got.ToolCalls[0].Arguments)
	}
}

// TestToolCallIDToTypesPreservesPairing 确认 tool_call_id 配对信息不丢。
//
// 配对信息丢失会让下一轮请求因「assistant 有 tool_calls 但无对应 tool 响应」而 400。
func TestToolCallIDToTypesPreservesPairing(t *testing.T) {
	m := Message{Role: RoleTool, Content: "结果", ToolCallID: "call-abc"}

	got := m.ToTypesMessage()
	if got.ToolCallID != "call-abc" {
		t.Fatalf("ToolCallID = %q，配对信息丢失", got.ToolCallID)
	}
}

// TestToolDefinitionToTypesPreservesSchema 确认 map schema 原样传递到契约。
func TestToolDefinitionToTypesPreservesSchema(t *testing.T) {
	td := ToolDefinition{
		Name:        "file_read",
		Description: "读取文件",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{"path": map[string]any{"type": "string"}},
		},
	}

	got := td.ToTypesToolDefinition()

	if got.Name != "file_read" || got.Description != "读取文件" {
		t.Errorf("名称/描述传递有误: %+v", got)
	}
	if got.Parameters["type"] != "object" {
		t.Errorf("schema type = %v", got.Parameters["type"])
	}
}

// TestToolDefinitionNilSchemaGetsSaneDefault 确认 nil schema 有合理默认值。
func TestToolDefinitionNilSchemaGetsSaneDefault(t *testing.T) {
	td := ToolDefinition{Name: "no_params"}
	got := td.ToTypesToolDefinition()

	if got.Parameters["type"] != "object" {
		t.Fatalf("默认 schema 应为 object: %v", got.Parameters)
	}
}

// TestResponseFromTypesRoundTrips 确认契约响应转回本层形态。
func TestResponseFromTypesRoundTrips(t *testing.T) {
	src := types.CompletionResponse{
		RequestID: "req-9",
		Content:   "答案",
		Reasoning: "推理",
		ToolCalls: []types.ToolCall{
			{ID: "c1", Name: "file_read", Arguments: map[string]any{"a": 1}},
		},
		StopReason: types.StopToolCall,
		Usage: types.TokenUsage{
			PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120, CachedPromptTokens: 80,
		},
		Emitted: true,
	}

	got := FromTypesResponse(src, RequestMeta{})

	if got.FinishReason != FinishToolCalls {
		t.Errorf("FinishReason = %v", got.FinishReason)
	}
	if got.Content != "答案" || got.ReasoningContent != "推理" {
		t.Errorf("内容丢失: %+v", got)
	}
	if len(got.ToolCalls) != 1 || got.ToolCalls[0].Arguments != `{"a":1}` {
		t.Errorf("工具调用转换有误: %+v", got.ToolCalls)
	}
	if !got.Emitted {
		t.Error("Emitted 应保留（第 18 章重放决策依赖它）")
	}
	if got.Meta.RequestID != "req-9" {
		t.Errorf("RequestID 应从契约回填，实际 %q", got.Meta.RequestID)
	}
	if got.Usage == nil {
		t.Fatal("usage 不应为空")
	}
	// 未命中数应派生为 100-80=20。
	if got.Usage.CacheMissTokens != 20 {
		t.Errorf("CacheMissTokens = %d, want 20（派生规则）", got.Usage.CacheMissTokens)
	}
}

// TestResponseToTypesRoundTrips 确认本层响应可转为契约形态。
func TestResponseToTypesRoundTrips(t *testing.T) {
	r := CompletionResponse{
		FinishReason:     FinishStop,
		Content:          "ok",
		ReasoningContent: "think",
		Emitted:          true,
		Meta:             RequestMeta{RequestID: "req-1"},
		ToolCalls:        []ToolCall{{ID: "c1", Name: "t", Arguments: "{}"}},
		Usage:            &TokenUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15, CacheHitTokens: 4},
	}

	got := r.ToTypesResponse()

	if got.StopReason != types.StopEnd {
		t.Errorf("StopReason = %v", got.StopReason)
	}
	if got.RequestID != "req-1" {
		t.Errorf("RequestID = %q", got.RequestID)
	}
	if got.Usage.CachedPromptTokens != 4 {
		t.Errorf("CachedPromptTokens = %d", got.Usage.CachedPromptTokens)
	}
	if !got.Emitted {
		t.Error("Emitted 应传递")
	}
}

// TestStreamChunkToTypesSplitsByKind 确认内部分片按契约的 Kind 判别式拆开。
//
// 契约用判别式联合，内部用宽结构 —— 一个内部分片可能拆成多个契约分片，
// 这个转换必须无损且顺序确定（内容 → 思考 → 工具调用 → usage → error → done）。
func TestStreamChunkToTypesSplitsByKind(t *testing.T) {
	chunk := StreamChunk{
		Content:          "hi",
		ReasoningContent: "think",
		ToolCalls:        []ToolCallDelta{{Index: 0, ID: "c1", Name: "t", Arguments: "{}"}},
		Usage:            &TokenUsage{PromptTokens: 1},
		Done:             true,
		Meta:             RequestMeta{RequestID: "req-1"},
	}

	got := chunk.ToTypesChunk()

	kinds := make([]types.StreamChunkKind, 0, len(got))
	for _, c := range got {
		kinds = append(kinds, c.Kind)
		if c.RequestID != "req-1" {
			t.Errorf("分片缺少 RequestID: %+v", c)
		}
	}

	want := []types.StreamChunkKind{
		types.ChunkContent, types.ChunkReasoning, types.ChunkToolCall, types.ChunkUsage, types.ChunkDone,
	}
	if len(kinds) != len(want) {
		t.Fatalf("分片数 = %d, want %d: %v", len(kinds), len(want), kinds)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("第 %d 个分片 kind = %v, want %v（次序: %v）", i, kinds[i], want[i], kinds)
		}
	}

	// done 必须是最后一个 —— 02 号的背压合并依赖这个约定。
	if kinds[len(kinds)-1] != types.ChunkDone {
		t.Fatal("done 必须是最后一个分片")
	}
}

// TestStreamChunkToolCallIndexPreserved 确认并行工具调用的 index 保留。
//
// 契约注释明确：Index 用于消歧交错到达的并行工具调用。
func TestStreamChunkToolCallIndexPreserved(t *testing.T) {
	chunk := StreamChunk{
		ToolCalls: []ToolCallDelta{
			{Index: 2, ID: "c2", Name: "b"},
			{Index: 5, ID: "c5", Name: "c"},
		},
	}

	got := chunk.ToTypesChunk()
	if len(got) != 2 {
		t.Fatalf("分片数 = %d", len(got))
	}
	if got[0].ToolCallIndex != 2 || got[1].ToolCallIndex != 5 {
		t.Fatalf("index 丢失: %d, %d", got[0].ToolCallIndex, got[1].ToolCallIndex)
	}
}

// TestStreamChunkErrorAndDoneBothEmitted 确认 error 与 done 同时出现时都投递。
func TestStreamChunkErrorAndDoneBothEmitted(t *testing.T) {
	chunk := StreamChunk{Done: true, Err: errTest, Meta: RequestMeta{RequestID: "r"}}

	got := chunk.ToTypesChunk()
	if len(got) != 2 {
		t.Fatalf("应有 error + done 两个分片，实际 %d", len(got))
	}
	if got[0].Kind != types.ChunkError || got[0].ErrorText == "" {
		t.Errorf("首个应为 error 且带文本: %+v", got[0])
	}
	if got[1].Kind != types.ChunkDone {
		t.Errorf("末个应为 done: %+v", got[1])
	}
}

// TestUsageRoundTripViaTypes 确认用量在两个形态间往返一致。
func TestUsageRoundTripViaTypes(t *testing.T) {
	orig := TokenUsage{
		PromptTokens: 200, CompletionTokens: 50, TotalTokens: 250,
		CacheHitTokens: 150, CacheMissTokens: 50, ReasoningTokens: 10,
	}

	back := FromTypesUsage(orig.ToTypesUsage())
	if back == nil {
		t.Fatal("往返不应得到 nil")
	}
	if back.PromptTokens != orig.PromptTokens || back.CompletionTokens != orig.CompletionTokens {
		t.Fatalf("token 数往返不一致: %+v vs %+v", *back, orig)
	}
	// 契约只存 hit，miss 由派生规则还原。
	if back.CacheHitTokens != 150 || back.CacheMissTokens != 50 {
		t.Fatalf("cache 字段往返不一致: hit=%d miss=%d", back.CacheHitTokens, back.CacheMissTokens)
	}
	if back.ReasoningTokens != 10 {
		t.Fatalf("ReasoningTokens = %d", back.ReasoningTokens)
	}
}

// TestFromTypesUsageZeroReturnsNil 确认零值用量返回 nil（而非全零结构）。
func TestFromTypesUsageZeroReturnsNil(t *testing.T) {
	if got := FromTypesUsage(types.TokenUsage{}); got != nil {
		t.Fatalf("零值应返回 nil，实际 %+v", got)
	}
}

// TestStopReasonMappingCoversAll 确认结束原因映射覆盖全部取值。
func TestStopReasonMappingCoversAll(t *testing.T) {
	cases := map[FinishReason]types.StopReason{
		FinishStop:      types.StopEnd,
		FinishToolCalls: types.StopToolCall,
		FinishLength:    types.StopLength,
		FinishError:     types.StopError,
		FinishCancelled: types.StopCancelled,
	}
	for mine, theirs := range cases {
		if got := toTypesStopReason(mine); got != theirs {
			t.Errorf("toTypesStopReason(%v) = %v, want %v", mine, got, theirs)
		}
		if got := fromTypesStopReason(theirs); got != mine {
			t.Errorf("fromTypesStopReason(%v) = %v, want %v", theirs, got, mine)
		}
	}
}

// TestResponseErrorTextSetsErrorFinish 确认契约的 ErrorText 映射为 error 结束原因。
func TestResponseErrorTextSetsErrorFinish(t *testing.T) {
	got := FromTypesResponse(types.CompletionResponse{
		StopReason: types.StopEnd, // 契约可能标 stop 但同时带 error 文本
		ErrorText:  "upstream failed",
	}, RequestMeta{})

	if got.FinishReason != FinishError {
		t.Fatalf("有 ErrorText 时应为 error 结束，实际 %v", got.FinishReason)
	}
}

var errTest = &testErr{}

type testErr struct{}

func (e *testErr) Error() string { return "stream error" }
