package openai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/catalog"
	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/httpx"
	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/model"
	"github.com/ximo888ok-netizen/ximo-agent/internal/provider"
)

// sseStream 负责 SSE 输出的「首字节」语义：在写出 SSE 响应头与第一行 data 之前，
// HTTP 状态码还能改，所以上游在首字节前失败时必须回普通 JSON 错误（§11.5）。
type sseStream struct {
	w       http.ResponseWriter
	flusher http.Flusher
	id      string
	model   string
	created int64

	// started 为 true 表示响应头与 200 已写出（此后失败只能发错误事件，不能再改状态码）。
	started bool
	// broken 为 true 表示写失败（客户端已断开），继续写没有意义。
	broken bool
	// roleSent 是否已发过含 role 的首个 delta。
	roleSent bool
	// toolCalls 是否出现过工具调用增量：上游显式报告了结束原因时不用它，仅作推断回退。
	toolCalls bool
}

func newSSEStream(w http.ResponseWriter, reqID, modelID string, created int64) (*sseStream, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, errors.New("响应写入器不支持 flush，无法提供 SSE")
	}
	return &sseStream{
		w:       w,
		flusher: flusher,
		id:      completionID(reqID),
		model:   modelID,
		created: created,
	}, nil
}

// start 写出 SSE 响应头。只生效一次。
func (s *sseStream) start() {
	if s.started {
		return
	}
	h := s.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// 反向代理（nginx 等）默认缓冲响应体，会把 SSE 攒到流结束才发出去。
	h.Set("X-Accel-Buffering", "no")
	s.w.WriteHeader(http.StatusOK)
	s.started = true
}

// writeDelta 转发一条内容/思考/工具调用增量。
func (s *sseStream) writeDelta(chunk provider.StreamChunk) bool {
	delta := streamDelta{
		Content:          chunk.Content,
		ReasoningContent: chunk.ReasoningContent,
	}
	if !s.roleSent {
		delta.Role = "assistant"
		s.roleSent = true
	}
	if len(chunk.ToolCalls) > 0 {
		s.toolCalls = true
		for _, tc := range chunk.ToolCalls {
			d := wireToolCallDelta{Index: tc.Index}
			if tc.ID != "" {
				d.ID = tc.ID
				d.Type = "function"
			}
			if tc.Name != "" || tc.Arguments != "" {
				d.Function = &wireFunctionDelta{Name: tc.Name, Arguments: tc.Arguments}
			}
			delta.ToolCalls = append(delta.ToolCalls, d)
		}
	}
	return s.writeChunk(streamChoice{Index: 0, Delta: delta})
}

// writeFinish 写终止分片（finish_reason 非空）。upstream 是本次候选尝试上游显式报告的结束
// 原因，为空表示上游没报（回退到推断，见 finishReason）。
func (s *sseStream) writeFinish(upstream provider.FinishReason) bool {
	reason := s.finishReason(upstream)
	return s.writeChunk(streamChoice{Index: 0, Delta: streamDelta{}, FinishReason: &reason})
}

// writeUsage 写用法分片（choices 为空数组，与 OpenAI 的 include_usage 收尾一致）。
func (s *sseStream) writeUsage(u usageBody) bool {
	if s.broken {
		return false
	}
	payload, err := json.Marshal(streamChunkBody{
		ID:      s.id,
		Object:  "chat.completion.chunk",
		Created: s.created,
		Model:   s.model,
		Choices: []streamChoice{},
		Usage:   &u,
	})
	if err != nil {
		return true
	}
	return s.writeRaw("data: " + string(payload) + "\n\n")
}

// writeChunk 写一条 choices 分片。
func (s *sseStream) writeChunk(choice streamChoice) bool {
	if s.broken {
		return false
	}
	payload, err := json.Marshal(streamChunkBody{
		ID:      s.id,
		Object:  "chat.completion.chunk",
		Created: s.created,
		Model:   s.model,
		Choices: []streamChoice{choice},
	})
	if err != nil {
		// 自己的结构体编码失败只可能是程序错误；不因此中断整条流。
		return true
	}
	return s.writeRaw("data: " + string(payload) + "\n\n")
}

// writeErrorEvent 发送流中途失败的错误事件。
//
// 刻意**不发 [DONE]**：终止分片与正常收尾在协议层必须可区分，否则客户端会把中途
// 失败当成功完成（§11.5「发送错误事件并终止」）。
func (s *sseStream) writeErrorEvent(detail httpx.ErrorDetail) bool {
	if s.broken {
		return false
	}
	payload, err := json.Marshal(streamErrorBody{Error: detail})
	if err != nil {
		return false
	}
	return s.writeRaw("data: " + string(payload) + "\n\n")
}

// writeDone 写 data: [DONE] 收尾。
func (s *sseStream) writeDone() bool {
	return s.writeRaw("data: [DONE]\n\n")
}

// writeRaw 写原始 SSE 行并立即 flush。
func (s *sseStream) writeRaw(line string) bool {
	if s.broken {
		return false
	}
	if _, err := io.WriteString(s.w, line); err != nil {
		s.broken = true
		return false
	}
	s.flusher.Flush()
	return true
}

// finishReason 决定收尾分片的 finish_reason。
//
// 优先用上游显式报告的结束原因（参数 upstream = provider.StreamChunk.FinishReason，
// 映射见 wireFinishReason：length → length、tool_calls → tool_calls、stop → stop）；
// 只有上游没报（空值）时才回退到推断。
//
// 为什么保留推断：OpenAI 兼容上游里存在只发 delta + [DONE]、整条流都不报 finish_reason 的
// 实现，那类流没有任何上游信息可用，只能沿用老口径 —— 出现过工具调用增量 → tool_calls，
// 否则 stop。此时「被 length 截断」仍会被写成 stop，那是上游信息缺失，不是网关的取舍。
func (s *sseStream) finishReason(upstream provider.FinishReason) string {
	if upstream != "" {
		return wireFinishReason(upstream)
	}
	if s.toolCalls {
		return "tool_calls"
	}
	return "stop"
}

// streamAttempt 是一次候选尝试的流式结果。
type streamAttempt struct {
	// done 上游正常收尾。
	done bool
	// clientGone 客户端已断开（或写不进去）。
	clientGone bool
	// usage 上游在流中上报的 usage；上游没报就是 nil（如实处理，不编造）。
	usage *provider.TokenUsage
	// finish 本次尝试上游显式报告的结束原因；空 = 上游没报。
	// 按尝试存放（而不是挂在 sseStream 上）：一次候选失败后换下一个候选时，
	// 上一次的结束原因绝不能漏进新候选的收尾分片。
	finish provider.FinishReason
	err    error
}

// pumpStream 把上游分片持续转发给客户端，直到流结束、出错或客户端断开。
func (h *handler) pumpStream(ctx context.Context, sse *sseStream, ch <-chan provider.StreamChunk) streamAttempt {
	out := streamAttempt{}
	for {
		select {
		case <-ctx.Done():
			// 客户端断开（或中间件请求超时）优先于上游错误：这不是上游的问题。
			out.clientGone = true
			out.err = ctx.Err()
			return out
		case chunk, ok := <-ch:
			if !ok {
				// provider 保证 Done 是最后一条分片；通道关闭却没有 Done 说明流被截断。
				if !out.done && out.err == nil {
					out.err = errors.New("上游流未正常结束（缺少终止分片）")
				}
				return out
			}
			if chunk.Usage != nil {
				out.usage = chunk.Usage
			}
			if chunk.Err != nil {
				out.err = chunk.Err
				return out
			}
			if chunk.FinishReason != "" {
				// 上游显式报告的结束原因：收尾分片以它为准，不再推断。
				out.finish = chunk.FinishReason
			}
			if chunk.Content != "" || chunk.ReasoningContent != "" || len(chunk.ToolCalls) > 0 {
				// 真正要写数据给客户端了，此刻才写响应头：这之前失败还能回 JSON 错误。
				sse.start()
				if !sse.writeDelta(chunk) {
					out.clientGone = true
					out.err = errors.New("向客户端写入失败")
					return out
				}
			}
			if chunk.Done {
				out.done = true
				return out
			}
		}
	}
}

// streamToClient 走流式路径：逐个候选 Stream，成功则收尾 + 结算。
func (h *handler) streamToClient(w http.ResponseWriter, r *http.Request, norm *normalizedRequest,
	cands []catalog.Candidate, base model.UsageRecord, c *charge, start time.Time) {
	ctx := r.Context()

	sse, err := newSSEStream(w, norm.RequestID, norm.Model, start.Unix())
	if err != nil {
		// 不静默降级成非流式（客户端要的是流）：按首字节前失败回普通错误。
		c.releaseOnly(ctx, "sse_unsupported")
		httpx.WriteError(w, r, http.StatusInternalServerError, "server_error", "网关不支持流式响应")
		return
	}

	var (
		lastErr      error
		lastProvider string
	)
	for _, cand := range cands {
		if ctx.Err() != nil {
			base.ProviderID = lastProvider
			base.LatencyMS = time.Since(start).Milliseconds()
			c.release(ctx, base, OutcomeClientCanceled, "client_canceled")
			return
		}

		client, err := h.d.Upstream.Client(ctx, cand.Provider.ID)
		if err != nil {
			lastErr, lastProvider = err, cand.Provider.ID
			h.log().Warn(ctx, "上游客户端不可用，尝试下一个候选", map[string]any{
				"request_id":  norm.RequestID,
				"provider_id": cand.Provider.ID,
				"error":       err.Error(),
			})
			continue
		}
		lastProvider = cand.Provider.ID

		ch, err := client.Stream(ctx, norm.completionRequest(cand))
		if err != nil {
			// 连流都没建立起（首字节前）：可以换候选。
			lastErr = err
			class := provider.Classify(err)
			if isClientCanceled(ctx, class) {
				base.ProviderID = cand.Provider.ID
				base.LatencyMS = time.Since(start).Milliseconds()
				c.release(ctx, base, OutcomeClientCanceled, "client_canceled")
				return
			}
			if provider.Retryable(class.Class) {
				continue
			}
			break
		}

		attempt := h.pumpStream(ctx, sse, ch)
		rec := base
		rec.ProviderID = cand.Provider.ID
		rec.LatencyMS = time.Since(start).Milliseconds()
		if attempt.usage != nil {
			rec.InputTokens = int64(attempt.usage.PromptTokens)
			rec.OutputTokens = int64(attempt.usage.CompletionTokens)
		}

		if attempt.clientGone {
			// 客户端断开：归还预占，终态 client_canceled（§11.4）。
			c.release(ctx, rec, OutcomeClientCanceled, "client_canceled")
			return
		}

		if attempt.err != nil {
			if !sse.started {
				// 首字节前失败：换候选，或走下面的普通 JSON 错误。
				lastErr = attempt.err
				class := provider.Classify(attempt.err)
				if isClientCanceled(ctx, class) {
					c.release(ctx, rec, OutcomeClientCanceled, "client_canceled")
					return
				}
				if provider.Retryable(class.Class) {
					h.log().Warn(ctx, "上游可重试错误，切换候选（流未开始）", map[string]any{
						"request_id":  norm.RequestID,
						"provider_id": cand.Provider.ID,
						"class":       class.Class.String(),
					})
					continue
				}
				break
			}
			// 首字节后失败：发错误事件并终止，同时落 usage 行标明终态（§11.5）。
			// 不能换候选 —— 已经发出去的字节收不回来。
			outcome := outcomeFor(ctx, attempt.err)
			failure := describeUpstreamFailure(attempt.err)
			sse.writeErrorEvent(httpx.ErrorDetail{
				Message:   failure.Message,
				Type:      httpx.TypeForStatus(failure.Status),
				Code:      failure.Code,
				RequestID: norm.RequestID,
			})
			h.log().Warn(ctx, "流式传输中断（已发送首字节）", map[string]any{
				"request_id":  norm.RequestID,
				"provider_id": cand.Provider.ID,
				"class":       provider.Classify(attempt.err).Class.String(),
				"outcome":     outcome,
			})
			c.release(ctx, rec, outcome, "stream_failed")
			return
		}

		// 正常收尾：finish_reason → 可选 usage → [DONE]（§11.5）。
		// 顺序与分片数不变；finish_reason 取值优先来自上游（attempt.finish）。
		sse.start()
		sse.writeFinish(attempt.finish)
		if norm.IncludeUsage && attempt.usage != nil {
			sse.writeUsage(usageFrom(attempt.usage))
		}
		sse.writeDone()
		c.settle(ctx, rec)
		h.log().Info(ctx, "chat.completions 流式完成", map[string]any{
			"request_id":    norm.RequestID,
			"provider_id":   cand.Provider.ID,
			"model":         norm.Model,
			"stream":        true,
			"input_tokens":  rec.InputTokens,
			"output_tokens": rec.OutputTokens,
			"latency_ms":    rec.LatencyMS,
			"outcome":       OutcomeSettled,
		})
		return
	}

	// 所有候选都在首字节前失败：普通 JSON 错误 + 非 2xx（§11.5）。
	rec := base
	rec.ProviderID = lastProvider
	rec.LatencyMS = time.Since(start).Milliseconds()
	outcome := outcomeFor(ctx, lastErr)
	c.release(ctx, rec, outcome, "upstream_failed")

	status, code, msg, retryAfter := http.StatusBadGateway, "provider_unavailable", "上游服务不可用，请稍后重试", int64(0)
	if lastErr != nil && !provider.Retryable(provider.Classify(lastErr).Class) {
		f := describeUpstreamFailure(lastErr)
		status, code, msg, retryAfter = f.Status, f.Code, f.Message, f.RetryAfter
	}
	h.log().Warn(ctx, "流式请求在首字节前失败", map[string]any{
		"request_id":  norm.RequestID,
		"provider_id": lastProvider,
		"class":       provider.Classify(lastErr).Class.String(),
		"outcome":     outcome,
	})
	if retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.FormatInt(clampRetryAfter(retryAfter), 10))
	}
	httpx.WriteError(w, r, status, code, msg)
}
