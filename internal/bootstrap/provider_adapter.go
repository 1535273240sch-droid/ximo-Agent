package bootstrap

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/ximo888ok-netizen/ximo-agent/internal/ports"
	"github.com/ximo888ok-netizen/ximo-agent/internal/provider"
	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// 本文件集中处理「Engine 的 ports 契约」与「provider/context 包的真实类型」
// 之间的形状差异。
//
// 为什么必须转换而不能省掉：ports 包是任务02 为并行开发先行冻结的消费侧接口，
// provider 包（任务06）按自己的规格实现，两边是各自独立的具名类型
// （ports.ProviderRequest vs provider.CompletionRequest），Go 的隐式接口满足
// 在这里完全不适用——方法名相同但参数类型不同，编译器直接判定未实现。
//
// 语义上有一处此前必须显式交代的落差已在本版补齐：ports 的 OnDelta 是流式
// 回调，而早期适配器走非流式 Complete、OnDelta 保持不发——UI 只能在整轮
// 结束后一次性拿到回答，观感是"卡很久突然全出来"。现在带 OnDelta 的请求
// 直通 Client.Stream：增量实时转发，结束时聚合出与非流式等价的响应。

// providerAdapter 让 *provider.Client 满足 ports.Provider。
type providerAdapter struct {
	client *provider.Client
	model  string
}

var _ ports.Provider = (*providerAdapter)(nil)

func newProviderAdapter(client *provider.Client, model string) *providerAdapter {
	return &providerAdapter{client: client, model: model}
}

// swappableProvider 是一个可在运行时替换底层实现的 ports.Provider。
//
// 为什么需要它：用户在设置页填入 API 密钥或改模型名之后，应当立即生效而不是
// 重启后端。但 Engine 在装配时拿到的是 Provider 的**值**（接口值拷贝），
// 之后替换 App 上的字段对 Engine 不可见。所以这里给 Engine 一个稳定的中间层，
// 它自己持有可替换的指针，换实现就是换指针。
type swappableProvider struct {
	mu  sync.RWMutex
	cur ports.Provider
}

var _ ports.Provider = (*swappableProvider)(nil)

// Swap 原子替换底层 Provider。
func (s *swappableProvider) Swap(next ports.Provider) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cur = next
}

// Current 返回当前生效的 Provider。
func (s *swappableProvider) Current() ports.Provider {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cur
}

// Complete 实现 ports.Provider，转发给当前底层实现。
func (s *swappableProvider) Complete(ctx context.Context, req ports.ProviderRequest) (ports.ProviderResponse, error) {
	cur := s.Current()
	if cur == nil {
		return ports.ProviderResponse{}, fmt.Errorf("bootstrap: provider is not configured")
	}
	return cur.Complete(ctx, req)
}

// Complete 实现 ports.Provider：把端口请求翻译成 provider 请求，再把响应翻回来。
//
// 带 OnDelta 的请求走流式直通：增量实时回调给调用方（UI 的打字机效果），
// 流结束后聚合出与非流式等价的 CompletionResponse。无 OnDelta 的内部调用
// （规划、收尾前的探测等）保持非流式路径不变。
func (a *providerAdapter) Complete(ctx context.Context, req ports.ProviderRequest) (ports.ProviderResponse, error) {
	model := req.Model
	if model == "" {
		model = a.model
	}

	conv, err := toProviderRequest(req, model)
	if err != nil {
		return ports.ProviderResponse{}, err
	}

	if req.OnDelta == nil {
		resp, err := a.client.Complete(ctx, conv)
		if err != nil {
			// 端口约定：非 nil error 表示传输/服务商层面的失败。传输错误原样透出，
			// 让 Agent 循环的重试与熔断逻辑拿到原始分类（provider.ClassifiedError）。
			return ports.ProviderResponse{}, err
		}
		out := fromProviderResponse(resp)
		if req.OnUsage != nil {
			req.OnUsage(out.Usage)
		}
		return out, nil
	}

	return a.completeStreaming(ctx, conv, req)
}

// completeStreaming 消费 Client.Stream 的增量通道并聚合完整响应。
//
// 结束原因按与服务端一致的规则推导：出现过工具调用分片即为 tool_calls，
// 否则视为正常停止（max_tokens 截断在 loop 侧与 stop 同样按最终答案处理）。
// 流通道由 provider 侧保证关闭；提前退出时其 goroutine 感知 ctx 取消而收尾。
func (a *providerAdapter) completeStreaming(ctx context.Context, conv provider.CompletionRequest, req ports.ProviderRequest) (ports.ProviderResponse, error) {
	stream, err := a.client.Stream(ctx, conv)
	if err != nil {
		return ports.ProviderResponse{}, err
	}

	var (
		content   strings.Builder
		reasoning strings.Builder
		calls     = make(map[int]*provider.ToolCall)
		order     []int
		usage     *provider.TokenUsage
		emitted   bool
	)
	for chunk := range stream {
		if chunk.Err != nil {
			return ports.ProviderResponse{}, chunk.Err
		}
		if chunk.Done {
			break
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
		if chunk.Content != "" || chunk.ReasoningContent != "" {
			content.WriteString(chunk.Content)
			reasoning.WriteString(chunk.ReasoningContent)
			emitted = true
			req.OnDelta(ports.Delta{
				Content:   chunk.Content,
				Reasoning: chunk.ReasoningContent,
			})
		}
		for _, d := range chunk.ToolCalls {
			acc, ok := calls[d.Index]
			if !ok {
				acc = &provider.ToolCall{}
				calls[d.Index] = acc
				order = append(order, d.Index)
			}
			if d.ID != "" {
				acc.ID = d.ID
			}
			if d.Name != "" {
				acc.Name += d.Name
			}
			if d.Arguments != "" {
				acc.Arguments += d.Arguments
			}
			emitted = true
			frag := d.Arguments
			req.OnDelta(ports.Delta{ToolCallDelta: &ports.ToolCallDelta{
				Index:             d.Index,
				ID:                d.ID,
				Name:              d.Name,
				ArgumentsFragment: frag,
			}})
		}
	}

	outCalls := make([]provider.ToolCall, 0, len(order))
	for _, idx := range order {
		outCalls = append(outCalls, *calls[idx])
	}

	finish := provider.FinishStop
	if len(outCalls) > 0 {
		finish = provider.FinishToolCalls
	}
	out := ports.ProviderResponse{
		FinishReason:     ports.FinishReason(finish),
		Content:          content.String(),
		ReasoningContent: reasoning.String(),
		ToolCalls:        []types.ToolCall{},
		Emitted:          emitted,
	}
	for _, tc := range outCalls {
		var args map[string]any
		if tc.Arguments != "" {
			_ = json.Unmarshal([]byte(tc.Arguments), &args)
		}
		out.ToolCalls = append(out.ToolCalls, types.ToolCall{
			ID:        tc.ID,
			Name:      tc.Name,
			Arguments: args,
		})
	}
	if usage != nil {
		out.Usage = ports.Usage{
			PromptTokens:     usage.PromptTokens,
			CompletionTokens: usage.CompletionTokens,
			TotalTokens:      usage.TotalTokens,
			CacheHitTokens:   usage.CacheHitTokens,
			CacheMissTokens:  usage.CacheMissTokens,
			ReasoningTokens:  usage.ReasoningTokens,
		}
		if req.OnUsage != nil {
			req.OnUsage(out.Usage)
		}
	}
	return out, nil
}

// toProviderRequest 完成 ports.ProviderRequest -> provider.CompletionRequest。
func toProviderRequest(req ports.ProviderRequest, model string) (provider.CompletionRequest, error) {
	msgs := make([]provider.Message, 0, len(req.Messages))
	for _, m := range req.Messages {
		calls := make([]provider.ToolCall, 0, len(m.ToolCalls))
		for _, tc := range m.ToolCalls {
			args, err := json.Marshal(tc.Arguments)
			if err != nil {
				return provider.CompletionRequest{}, fmt.Errorf("bootstrap: marshal tool call args: %w", err)
			}
			calls = append(calls, provider.ToolCall{
				ID:        tc.ID,
				Name:      tc.Name,
				Arguments: string(args),
			})
		}
		msgs = append(msgs, provider.Message{
			Role:             provider.Role(m.Role),
			Content:          m.Content,
			ReasoningContent: m.ReasoningContent,
			ToolCalls:        calls,
			ToolCallID:       m.ToolCallID,
		})
	}

	tools := make([]provider.ToolDefinition, 0, len(req.Tools))
	for _, t := range req.Tools {
		tools = append(tools, provider.ToolDefinition{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.Parameters,
		})
	}

	effort := provider.ReasoningEffort(req.Effort)
	return provider.CompletionRequest{
		Model:           model,
		Messages:        msgs,
		Tools:           tools,
		ReasoningEffort: effort,
		// ThinkingMode 由 effort 推导：只要不是「关闭」就开启思考模式，
		// 与 expert/subagent.go 的做法一致（DeepSeek 靠该开关区分 reasoning）。
		ThinkingMode: req.Effort != "" && req.Effort != types.EffortOff,
		MaxTokens:    req.MaxTokens,
	}, nil
}

// fromProviderResponse 完成 provider.CompletionResponse -> ports.ProviderResponse。
func fromProviderResponse(resp provider.CompletionResponse) ports.ProviderResponse {
	calls := make([]types.ToolCall, 0, len(resp.ToolCalls))
	for _, tc := range resp.ToolCalls {
		var args map[string]any
		if tc.Arguments != "" {
			// 参数是模型给的 JSON 文本。解析失败时退化为空参数而不是报错：
			// 后续工具运行时会以「参数不合法」的明确错误返回给模型，让模型自纠，
			// 比在传输层直接失败更有用。
			_ = json.Unmarshal([]byte(tc.Arguments), &args)
		}
		calls = append(calls, types.ToolCall{
			ID:        tc.ID,
			Name:      tc.Name,
			Arguments: args,
		})
	}

	out := ports.ProviderResponse{
		FinishReason:     ports.FinishReason(resp.FinishReason),
		Content:          resp.Content,
		ReasoningContent: resp.ReasoningContent,
		ToolCalls:        calls,
		Emitted:          resp.Emitted,
	}
	if resp.Usage != nil {
		out.Usage = ports.Usage{
			PromptTokens:     resp.Usage.PromptTokens,
			CompletionTokens: resp.Usage.CompletionTokens,
			TotalTokens:      resp.Usage.TotalTokens,
			CacheHitTokens:   resp.Usage.CacheHitTokens,
			CacheMissTokens:  resp.Usage.CacheMissTokens,
			ReasoningTokens:  resp.Usage.ReasoningTokens,
		}
	}
	if resp.FinishReason == provider.FinishError {
		out.Error = "provider reported an error finish reason"
	}
	return out
}
