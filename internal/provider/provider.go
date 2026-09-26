// Package provider 实现 LLM 调用层：能力门控的多服务商解析、限流、熔断、分类重试与流式处理。
//
// 调用链（架构文档第 18 章）：
//
//	Provider → RateLimiter → CircuitBreaker → RetryPolicy → HTTP Client
//
// 本文件只放契约类型与接口：Provider / CompletionRequest / CompletionResponse / StreamChunk，
// 以及 v1 `resolveActiveProvider` 的能力门控解析逻辑。
//
// 对应 v1 源码：src/main/deepseek/provider.ts、src/main/deepseek/types.ts、src/main/deepseek/context.ts
package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// 契约类型（任务 02 Engine 消费；字段命名需与任务 07 埋点对齐）
// ---------------------------------------------------------------------------

// Role 消息角色。与 OpenAI / DeepSeek 的 chat 协议一致。
type Role = string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// ToolCall 一次工具调用（assistant 消息发出，tool 消息回应）。
//
// Arguments 保持原始 JSON 字符串：v1 的 tool_calls 也是字符串形式累积的，
// 且执行工具时由调用方解析，避免本层对 schema 做假设导致 invalid tool schema。
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolDefinition 工具 schema（能力门控后的请求体里用 function 包装）。
type ToolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// Message 一条对话消息。
//
// ReasoningContent 是 DeepSeek 思考模式的硬约束：所有 assistant 轮都必须原样回传，
// 漏传会 400。非 reasoning 服务商由能力门控自动剥离（见 buildRequestBody）。
type Message struct {
	Role             Role       `json:"role"`
	Content          string     `json:"content"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
}

// ReasoningEffort 推理强度。'ultra' 是应用层自定义等级，API 层等价于 'max'。
type ReasoningEffort string

const (
	EffortOff   ReasoningEffort = "off"
	EffortLow   ReasoningEffort = "low"
	EffortHigh  ReasoningEffort = "high"
	EffortMax   ReasoningEffort = "max"
	EffortUltra ReasoningEffort = "ultra"
)

// ToAPIEffort 将应用层等级映射为 API 支持的取值（对应 v1 toApiEffort）。
func ToAPIEffort(effort ReasoningEffort) ReasoningEffort {
	if effort == EffortUltra {
		return EffortMax
	}
	return effort
}

// RequestMeta 每次请求的关联标识，用于写入日志与任务 07 的 observability 埋点。
//
// 这四个字段是第 18 章的硬性要求：request_id/run_id/turn_id/attempt。
type RequestMeta struct {
	RequestID string
	RunID     string
	TurnID    string
	Attempt   int
}

// Labels 转为 observability 标签集，Append 时只暴露非敏感字段。
func (m RequestMeta) Labels() map[string]string {
	out := make(map[string]string, 4)
	if m.RequestID != "" {
		out["request_id"] = m.RequestID
	}
	if m.RunID != "" {
		out["run_id"] = m.RunID
	}
	if m.TurnID != "" {
		out["turn_id"] = m.TurnID
	}
	if m.Attempt > 0 {
		out["attempt"] = fmt.Sprintf("%d", m.Attempt)
	}
	return out
}

// CompletionRequest 一次补全/流式请求（任务 02 提供方契约）。
type CompletionRequest struct {
	// Meta 关联标识；RequestID 为空时由客户端自动生成。
	Meta RequestMeta

	// ProviderID 目标服务商；空则用解析出的默认活跃服务商。
	ProviderID string

	Model    string
	Messages []Message
	Tools    []ToolDefinition

	ThinkingMode    bool
	ReasoningEffort ReasoningEffort
	Temperature     float64
	MaxTokens       int

	// Timeout 覆盖本次请求的总超时（含重试）。0 表示用客户端默认值。
	Timeout time.Duration
}

// TokenUsage 归一化后的用量。DeepSeek 与 OpenAI 两种 cache 字段形态统一到这里。
//
// 对应 v1 shared/cache/normalize-usage.ts 的 normaliseUsage()：
// 哪边报非零就用哪边；只有 hit 时派生 miss = prompt - hit，保证 hit+miss == prompt。
type TokenUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	CacheHitTokens   int `json:"cache_hit_tokens"`
	CacheMissTokens  int `json:"cache_miss_tokens"`
	ReasoningTokens  int `json:"reasoning_tokens"`
}

// FinishReason 结束原因。与 v1 SingleCallResult 的取值保持一致，便于对照排查。
type FinishReason string

const (
	FinishStop      FinishReason = "stop"
	FinishToolCalls FinishReason = "tool_calls"
	FinishLength    FinishReason = "length"
	FinishError     FinishReason = "error"
	FinishCancelled FinishReason = "cancelled"
)

// CompletionResponse 非流式补全结果。
type CompletionResponse struct {
	FinishReason     FinishReason
	Content          string
	ReasoningContent string
	ToolCalls        []ToolCall
	Usage            *TokenUsage

	// Emitted 已有部分输出（流式场景判断可否重放）。
	Emitted bool

	// Attempts 实际消耗的尝试次数（含首次），供 observability 记录。
	Attempts int

	// Meta 本次请求的关联标识（重试后为最后一次的 attempt）。
	Meta RequestMeta
}

// StreamChunk 流式分片。
//
// 与 v1 StreamChunk 对齐：Content 与 ReasoningContent 增量、ToolCalls 累积后一次性携带、
// Usage 仅在结束时出现。Err 非空表示本次流以错误终止。
type StreamChunk struct {
	// Content 文本增量（可能为空）。
	Content string
	// ReasoningContent 思考内容增量（可能为空）。
	ReasoningContent string
	// ToolCalls 增量工具调用；调用方需按 Index 累积 ID/Name/Arguments。
	ToolCalls []ToolCallDelta
	// Usage 结束时上报（仅当服务商支持 sendStreamUsage）。
	Usage *TokenUsage
	// FinishReason 上游显式报告的结束原因；空 = 上游整条流都没报，调用方需自行推断。
	//
	// 上游报出结束原因时，本层会单独发一条**只带该字段**的分片（在最后一个内容分片
	// 之后、Done 之前），供网关决定响应里的 stop_reason —— 否则「被 max_tokens
	// 截断」这类信息只能靠输出长度去猜。
	//
	// 这里刻意不做「有工具调用即 tool_calls」的归一：归一属于非流式聚合结果的口径
	// （见 resolveFinishReason），用在分片里会覆盖上游明确报告的 stop/length。
	FinishReason FinishReason
	// Done 流结束标志；结束时必定发送且为最后一个分片。
	Done bool
	// Err 终止错误；与 Done 同时出现。
	Err error
	// Meta 本次流所属请求的关联标识。
	Meta RequestMeta
}

// ToolCallDelta 流式工具调用增量。Arguments 是分片字符串，需拼接。
type ToolCallDelta struct {
	Index     int
	ID        string
	Name      string
	Arguments string
}

// Provider 是任务 02 Engine 消费的契约接口。
//
// 实现必须：① 不把明文 API Key 写进日志/事件/返回值；② 断网/429/5xx 不产生死循环；
//
//	③ 已提交副作用的请求不自动重试。
type Provider interface {
	Complete(ctx context.Context, req CompletionRequest) (CompletionResponse, error)
	Stream(ctx context.Context, req CompletionRequest) (<-chan StreamChunk, error)

	// Name 返回服务商标识，供 observability 标签使用。
	Name() string
	// ContextWindow 返回上下文窗口 token 数，供任务 06 的 context 预算使用。
	ContextWindow() int
	// MaxOutputTokens 返回单次最大输出 token。
	MaxOutputTokens() int
}

// ---------------------------------------------------------------------------
// 能力门控与多服务商解析（v1 resolveActiveProvider 的 Go 对等实现）
// ---------------------------------------------------------------------------

// Capabilities 服务商能力开关，控制是否发送 DeepSeek 专属请求参数。
type Capabilities struct {
	// SendReasoningParams 发送 enable_thinking / reasoning_effort，
	// 并保留消息中的 reasoning_content 字段。
	SendReasoningParams bool
	// SendStreamUsage 发送 stream_options.include_usage。
	SendStreamUsage bool
}

// DefaultCapabilities DeepSeek 内置服务商的全开能力。
func DefaultCapabilities() Capabilities {
	return Capabilities{SendReasoningParams: true, SendStreamUsage: true}
}

// ProviderConfig 一个服务商的静态配置。
type ProviderConfig struct {
	ID              string
	Name            string
	BaseURL         string
	ContextWindow   int
	MaxOutputTokens int
	IsDeepSeek      bool
	Capabilities    Capabilities

	// SecretRef 指向 SecretsProvider 中的密钥引用（任务 03/04 提供）。
	// 本层绝不缓存明文 key，仅在发起单次请求时解析一次。
	SecretRef string
}

// Builtin DeepSeek 服务商标识。
const BuiltinProviderID = "deepseek"

// DeepSeek-V4 系列上下文窗口（v1 常量）。
const deepSeekContextWindow = 1_000_000

// 自定义服务商缺省值（v1 常量）。
const (
	defaultCustomContextWindow = 131_072
	defaultCustomMaxOutput     = 8192
	// apiMaxOutputCeiling 请求体 max_tokens 的硬上限（v1 的 393216 防护）。
	apiMaxOutputCeiling = 393_216
)

// ConfigSource 是解析活跃服务商所需的配置来源。
//
// 任务 01 的 config 模块与任务 03 的 settings 存储都可实现它；
// 本包不依赖具体存储，避免跨目录耦合。
type ConfigSource interface {
	// ActiveProviderID 返回当前选中的服务商 ID（可能为空）。
	ActiveProviderID() string
	// Provider 按 ID 查找配置；未找到返回 false。
	Provider(id string) (ProviderConfig, bool)
	// Builtin 返回内置服务商配置（DeepSeek 兜底）。
	Builtin() ProviderConfig
}

// ErrProviderNotFound 自定义服务商不存在且无兜底时返回。
var ErrProviderNotFound = errors.New("provider: 服务商不存在")

// ResolveActiveProvider 解析活跃服务商，语义与 v1 resolveActiveProvider 一致：
// 指定 ID → 内置 DeepSeek；自定义 ID → 查表；查不到 → 回退内置，保证链路不中断。
func ResolveActiveProvider(src ConfigSource, providerID string) (ProviderConfig, error) {
	if src == nil {
		return ProviderConfig{}, ErrProviderNotFound
	}
	id := strings.TrimSpace(providerID)
	if id == "" {
		id = strings.TrimSpace(src.ActiveProviderID())
	}
	builtin := normalizeBuiltin(src.Builtin())

	if id == "" || id == BuiltinProviderID {
		return builtin, nil
	}
	cfg, ok := src.Provider(id)
	if !ok {
		// 与 v1 一致：自定义服务商不存在时回退内置，而不是报错中断。
		return builtin, nil
	}
	return normalizeCustom(cfg), nil
}

// normalizeBuiltin 补齐内置 DeepSeek 的缺省字段。
func normalizeBuiltin(cfg ProviderConfig) ProviderConfig {
	if cfg.ID == "" {
		cfg.ID = BuiltinProviderID
	}
	if cfg.Name == "" {
		cfg.Name = "DeepSeek"
	}
	if cfg.ContextWindow <= 0 {
		cfg.ContextWindow = deepSeekContextWindow
	}
	if cfg.MaxOutputTokens <= 0 {
		cfg.MaxOutputTokens = defaultCustomMaxOutput
	}
	cfg.IsDeepSeek = true
	cfg.Capabilities = Capabilities{SendReasoningParams: true, SendStreamUsage: true}
	return cfg
}

// normalizeCustom 补齐自定义服务商的缺省字段。
func normalizeCustom(cfg ProviderConfig) ProviderConfig {
	if cfg.Name == "" {
		cfg.Name = cfg.ID
	}
	if cfg.ContextWindow <= 0 {
		cfg.ContextWindow = defaultCustomContextWindow
	}
	if cfg.MaxOutputTokens <= 0 {
		cfg.MaxOutputTokens = defaultCustomMaxOutput
	}
	if !cfg.Capabilities.SendReasoningParams && !cfg.Capabilities.SendStreamUsage {
		// 全 false 视为未配置 —— 缺省全开，与 v1 `?? true` 语义一致。
		cfg.Capabilities = DefaultCapabilities()
	}
	return cfg
}

// ---------------------------------------------------------------------------
// 消息净化（v1 context.ts sanitizeContent 的 Go 对等实现）
// ---------------------------------------------------------------------------

// SanitizeContent 移除会导致 API JSON 解析失败的不可见字符。
//
// 根因：web_search / web_fetch 抓取的网页可能含控制字符（0x00-0x1F 除 \n \r \t）、
// 孤立 Unicode 代理对（0xD800-0xDFFF）与 DEL（0x7F），
// 经 JSON 序列化后部分解析器报 "unexpected end of hex escape"。
func SanitizeContent(text string) string {
	if text == "" {
		return text
	}
	var b strings.Builder
	b.Grow(len(text))
	for _, r := range text {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteRune(r)
		case r < 0x20 || r == 0x7F:
			// 丢弃控制字符与 DEL
		case r >= 0xD800 && r <= 0xDFFF:
			// 丢弃孤立代理对 —— Go 的 range 对非法 UTF-8 产出 U+FFFD，
			// 这里的判断覆盖显式编码的代理码位。
			b.WriteRune('\uFFFD')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ClampMaxTokens 将 max_tokens 钳制到 [1, apiMaxOutputCeiling]。
func ClampMaxTokens(v int) int {
	if v < 1 {
		return 1
	}
	if v > apiMaxOutputCeiling {
		return apiMaxOutputCeiling
	}
	return v
}

// BuildRequestBody 构造 /chat/completions 请求体（对应 v1 buildRequestBody）。
//
// 能力门控：caps 缺省或全开时，输出与内置 DeepSeek 历史行为逐字段一致；
// 自定义服务商可通过开关裁剪 DeepSeek 专属参数。
func BuildRequestBody(
	model string,
	messages []Message,
	tools []ToolDefinition,
	thinkingMode bool,
	effort ReasoningEffort,
	temperature float64,
	maxTokens int,
	caps Capabilities,
) map[string]any {
	sanitized := make([]map[string]any, 0, len(messages))
	for _, m := range messages {
		entry := map[string]any{
			"role":    m.Role,
			"content": SanitizeContent(m.Content),
		}
		if len(m.ToolCalls) > 0 {
			calls := make([]map[string]any, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				calls = append(calls, map[string]any{
					"id":   tc.ID,
					"type": "function",
					"function": map[string]any{
						"name":      tc.Name,
						"arguments": tc.Arguments,
					},
				})
			}
			entry["tool_calls"] = calls
		}
		if m.ToolCallID != "" {
			entry["tool_call_id"] = m.ToolCallID
		}
		// 非 reasoning 服务商剥离 reasoning_content（防第三方 API 拒绝未知字段）。
		if caps.SendReasoningParams && m.ReasoningContent != "" {
			entry["reasoning_content"] = m.ReasoningContent
		}
		sanitized = append(sanitized, entry)
	}

	body := map[string]any{
		"model":      model,
		"messages":   sanitized,
		"stream":     true,
		"max_tokens": ClampMaxTokens(maxTokens),
	}

	if caps.SendStreamUsage {
		body["stream_options"] = map[string]any{"include_usage": true}
	}

	if len(tools) > 0 {
		defs := make([]map[string]any, 0, len(tools))
		for _, t := range tools {
			defs = append(defs, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        t.Name,
					"description": t.Description,
					"parameters":  t.Parameters,
				},
			})
		}
		body["tools"] = defs
		body["tool_choice"] = "auto"
	}

	// 思考模式与温度互斥：开了 thinking 就不发 temperature（v1 同款处理）。
	if !thinkingMode || effort == EffortOff || !caps.SendReasoningParams {
		body["temperature"] = temperature
	} else {
		body["enable_thinking"] = true
		body["reasoning_effort"] = string(ToAPIEffort(effort))
	}

	return body
}

// NormalizeUsage 把两种 cache-hit 字段形态归一为 TokenUsage。
//
// 输入是两个字段的原生 JSON 形态，由 openai.go 在解析响应时填入。
func NormalizeUsage(
	promptTokens, completionTokens, totalTokens int,
	cacheHit, cacheMiss int,
	nestedCached int,
	reasoningTokens int,
) TokenUsage {
	hit := cacheHit
	miss := cacheMiss
	if hit == 0 && nestedCached > 0 {
		// OpenAI/MiMo 形态：prompt_tokens_details.cached_tokens
		hit = nestedCached
	}
	if miss == 0 && hit > 0 && promptTokens > hit {
		// 后端只给 hit 时派生 miss，保证 hit+miss == prompt
		miss = promptTokens - hit
	}
	if totalTokens == 0 {
		totalTokens = promptTokens + completionTokens
	}
	return TokenUsage{
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		TotalTokens:      totalTokens,
		CacheHitTokens:   hit,
		CacheMissTokens:  miss,
		ReasoningTokens:  reasoningTokens,
	}
}
