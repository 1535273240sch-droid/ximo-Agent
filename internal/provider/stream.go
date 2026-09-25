// stream.go —— 流式响应处理（第 18 章 Provider.Stream）。
//
// 三件事：
//  1. SSE 解码（data: 行 / [DONE] / 注释行）。
//  2. 增量累积：content、reasoning_content、tool_calls（按 Index 拼接 arguments）。
//  3. 断流重放决策 —— v1 的 emitted 标志：已有输出则绝不重放（避免重复输出），
//     零输出且连接断开才重放，且重放次数有上限（防死循环）。
//
// 空闲看门狗（v1 IDLE_TIMEOUT_MS = 60s）：服务端长时间不发数据则主动断开，
// 否则一次卡死的连接会永久占住 goroutine。
package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/observability"
)

// 流式重连上限。与 v1 MAX_STREAM_RECONNECTS 一致。
const maxStreamReconnects = 3

// streamDebugEnabled 报告流式诊断日志是否开启（任务2 Part A）。
// 仅当环境变量 XIMO_STREAM_DEBUG=1 时开启，进程启动时读取一次，默认关闭——
// 诊断日志不允许常驻生产。开启后 provider 端每回调一次 onChunk 就向 stderr
// 打一条带时间戳的日志，与前端 token.delta 的同名日志对比即可定位"突发"
// 发生在哪一段链路。
var streamDebugEnabled = sync.OnceValue(func() bool {
	return os.Getenv("XIMO_STREAM_DEBUG") == "1"
})

// streamDebugChunk 输出一条 provider 端流式诊断日志（时间戳 + chunk 类型与字节数）。
func streamDebugChunk(kind string, n int) {
	if !streamDebugEnabled() {
		return
	}
	fmt.Fprintf(os.Stderr, "[stream-debug] %s provider chunk kind=%s bytes=%d\n",
		time.Now().Format("15:04:05.000"), kind, n)
}

// DefaultIdleTimeout 流式空闲超时（v1 为 60s）。
const DefaultIdleTimeout = 60 * time.Second

// streamBufSize SSE 读缓冲。
const streamBufSize = 256 << 10

// SSEEvent 一条解析后的 SSE 事件数据。
type SSEEvent struct {
	// Data data: 之后的内容（已 trim）。
	Data string
	// Comment 是否为注释行（以 ':' 开头）—— 心跳，跳过即可。
	Comment bool
}

// sseDecoder 增量 SSE 解码器。按行读取，非 data: 行忽略。
type sseDecoder struct {
	reader *bufio.Reader
}

func newSSEDecoder(r io.Reader) *sseDecoder {
	return &sseDecoder{reader: bufio.NewReaderSize(r, streamBufSize)}
}

// Next 返回下一个 data 事件。ok=false 表示流正常结束。
func (d *sseDecoder) Next() (data string, ok bool, err error) {
	for {
		line, err := d.reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				// 处理最后一行无换行符的情况。
				if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "data:") {
					return strings.TrimSpace(trimmed[len("data:"):]), true, nil
				}
				return "", false, nil
			}
			return "", false, err
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, ":") {
			continue // 空行分隔符 / 注释心跳
		}
		if !strings.HasPrefix(trimmed, "data:") {
			continue
		}
		return strings.TrimSpace(trimmed[len("data:"):]), true, nil
	}
}

// ---------------------------------------------------------------------------
// 累积器
// ---------------------------------------------------------------------------

// toolCallAcc 单个工具调用的累积状态。
type toolCallAcc struct {
	id        string
	name      string
	arguments strings.Builder
}

// streamAccumulator 累积一次流式响应的全部内容。
//
// Emitted 是重放决策的核心标志：任何 content/reasoning/tool_call 增量出现即为 true。
type streamAccumulator struct {
	content   strings.Builder
	reasoning strings.Builder

	toolCalls map[int]*toolCallAcc
	order     []int

	usage   *TokenUsage
	emitted bool

	// finishReason 服务端最后一次报告的结束原因。
	finishReason FinishReason
}

func newStreamAccumulator() *streamAccumulator {
	return &streamAccumulator{toolCalls: make(map[int]*toolCallAcc)}
}

// rawChunk 对应一个 SSE data 负载的 JSON 结构。
type rawChunk struct {
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				Index    *int   `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *rawUsage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"`
	} `json:"error"`
}

// rawUsage 覆盖 DeepSeek 与 OpenAI 两种 cache 字段形态。
type rawUsage struct {
	PromptTokens         int `json:"prompt_tokens"`
	CompletionTokens     int `json:"completion_tokens"`
	TotalTokens          int `json:"total_tokens"`
	PromptCacheHitTokens int `json:"prompt_cache_hit_tokens"`
	PromptCacheMissToken int `json:"prompt_cache_miss_tokens"`
	PromptTokensDetails  *struct {
		CachedTokens    int `json:"cached_tokens"`
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

// consume 处理一条 SSE data 负载。
//
// 返回 stop=true 表示流应当终止（收到 [DONE] 或 finish_reason）。
// onChunk 可为 nil（非流式聚合场景）。
func (a *streamAccumulator) consume(data string, onChunk func(StreamChunk)) (stop bool, err error) {
	if data == "[DONE]" {
		a.finishReason = a.resolveFinishReason(FinishStop)
		return true, nil
	}

	var chunk rawChunk
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		// 不完整的 JSON 分片直接跳过（v1 同款容错）—— 不能因为一个坏分片中断整条流。
		return false, nil
	}

	if chunk.Error != nil && chunk.Error.Message != "" {
		return true, &APIError{StatusCode: 400, Code: chunk.Error.Type, Message: chunk.Error.Message}
	}

	if chunk.Usage != nil {
		u := NormalizeUsage(
			chunk.Usage.PromptTokens,
			chunk.Usage.CompletionTokens,
			chunk.Usage.TotalTokens,
			chunk.Usage.PromptCacheHitTokens,
			chunk.Usage.PromptCacheMissToken,
			usageNestedCached(chunk.Usage),
			usageReasoning(chunk.Usage),
		)
		a.usage = &u
		if onChunk != nil {
			onChunk(StreamChunk{Usage: &u})
		}
	}

	if len(chunk.Choices) == 0 {
		return false, nil
	}
	choice := chunk.Choices[0]
	delta := choice.Delta

	if delta.Content != "" {
		a.content.WriteString(delta.Content)
		a.emitted = true
		if onChunk != nil {
			streamDebugChunk("content", len(delta.Content))
			onChunk(StreamChunk{Content: delta.Content})
		}
	}
	if delta.ReasoningContent != "" {
		a.reasoning.WriteString(delta.ReasoningContent)
		a.emitted = true
		if onChunk != nil {
			streamDebugChunk("reasoning", len(delta.ReasoningContent))
			onChunk(StreamChunk{ReasoningContent: delta.ReasoningContent})
		}
	}
	if len(delta.ToolCalls) > 0 {
		a.emitted = true
		deltas := make([]ToolCallDelta, 0, len(delta.ToolCalls))
		for _, tc := range delta.ToolCalls {
			idx := 0
			if tc.Index != nil {
				idx = *tc.Index
			}
			acc, exists := a.toolCalls[idx]
			if !exists {
				acc = &toolCallAcc{}
				a.toolCalls[idx] = acc
				a.order = append(a.order, idx)
			}
			if tc.ID != "" {
				acc.id = tc.ID
			}
			if tc.Function.Name != "" {
				acc.name += tc.Function.Name
			}
			if tc.Function.Arguments != "" {
				acc.arguments.WriteString(tc.Function.Arguments)
			}
			deltas = append(deltas, ToolCallDelta{
				Index:     idx,
				ID:        tc.ID,
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			})
		}
		if onChunk != nil {
			args := 0
			for _, d := range deltas {
				args += len(d.Name) + len(d.Arguments)
			}
			streamDebugChunk("tool_calls", args)
			onChunk(StreamChunk{ToolCalls: deltas})
		}
	}

	if choice.FinishReason != "" {
		a.finishReason = mapFinishReason(choice.FinishReason)
		return true, nil
	}
	return false, nil
}

// resolveFinishReason 有 tool_calls 时优先归为 tool_calls（v1 同款优先级）。
func (a *streamAccumulator) resolveFinishReason(fallback FinishReason) FinishReason {
	if len(a.toolCalls) > 0 {
		return FinishToolCalls
	}
	if a.finishReason != "" {
		return a.finishReason
	}
	return fallback
}

// collectedToolCalls 按出现顺序输出累积的工具调用。
func (a *streamAccumulator) collectedToolCalls() []ToolCall {
	if len(a.toolCalls) == 0 {
		return nil
	}
	out := make([]ToolCall, 0, len(a.toolCalls))
	for _, idx := range a.order {
		acc := a.toolCalls[idx]
		out = append(out, ToolCall{
			ID:        acc.id,
			Name:      acc.name,
			Arguments: acc.arguments.String(),
		})
	}
	return out
}

// toResponse 聚合为 CompletionResponse。
func (a *streamAccumulator) toResponse() CompletionResponse {
	return CompletionResponse{
		FinishReason:     a.resolveFinishReason(FinishStop),
		Content:          a.content.String(),
		ReasoningContent: a.reasoning.String(),
		ToolCalls:        a.collectedToolCalls(),
		Usage:            a.usage,
		Emitted:          a.emitted,
	}
}

func mapFinishReason(raw string) FinishReason {
	switch raw {
	case "stop":
		return FinishStop
	case "tool_calls":
		return FinishToolCalls
	case "length":
		return FinishLength
	default:
		return FinishReason(raw)
	}
}

func usageNestedCached(u *rawUsage) int {
	if u.PromptTokensDetails != nil {
		return u.PromptTokensDetails.CachedTokens
	}
	return 0
}

func usageReasoning(u *rawUsage) int {
	if u.CompletionTokensDetails != nil && u.CompletionTokensDetails.ReasoningTokens > 0 {
		return u.CompletionTokensDetails.ReasoningTokens
	}
	if u.PromptTokensDetails != nil {
		return u.PromptTokensDetails.ReasoningTokens
	}
	return 0
}

// ---------------------------------------------------------------------------
// 流式 Provider.Stream
// ---------------------------------------------------------------------------

// Stream 发起流式补全。
//
// 返回的 channel 由内部 goroutine 写入，调用方必须持续读取直到 Done 或被取消，
// 否则 goroutine 会阻塞（channel 关闭由本方法保证）。
//
// 断流重放：仅在「尚未发出任何输出」且「错误属于连接类」时重连，
// 上限 maxStreamReconnects 次。已有输出则直接以错误终止，绝不重放。
func (c *Client) Stream(ctx context.Context, req CompletionRequest) (<-chan StreamChunk, error) {
	body, err := c.buildBody(req)
	if err != nil {
		return nil, err
	}
	meta := req.Meta
	if meta.RequestID == "" {
		meta.RequestID = c.nextRequestID()
	}

	out := make(chan StreamChunk, 16)
	go c.runStream(ctx, req, meta, body, out)
	return out, nil
}

// runStream 是流式循环的主体，负责重放决策与收尾。
func (c *Client) runStream(ctx context.Context, req CompletionRequest, meta RequestMeta, body []byte, out chan<- StreamChunk) {
	defer close(out)

	observability.ProviderRequest(c.cfg.Name, req.Model)
	start := time.Now()
	defer func() {
		observability.ProviderLatency(c.cfg.Name, req.Model, float64(time.Since(start).Milliseconds()))
	}()

	var lastErr error
	for attempt := 0; attempt <= maxStreamReconnects; attempt++ {
		meta.Attempt = attempt + 1

		emitted, err := c.streamOnce(ctx, req, meta, body, out)
		if err == nil {
			sendDone(ctx, out, meta, nil)
			return
		}
		lastErr = err

		ce := Classify(err)
		if ce.Class == ClassUserCancelled {
			sendDone(ctx, out, meta, err)
			return
		}

		// 已有部分输出 → 重放会造成重复输出，直接报错终止。
		if emitted {
			sendDone(ctx, out, meta, fmt.Errorf("流式传输中断（已有部分输出，不重放）：%w", err))
			return
		}

		// 仅连接类错误值得重放（v1 isConnResetError 的对等实现）。
		if !isConnResetClass(ce.Class) {
			sendDone(ctx, out, meta, err)
			return
		}
		if attempt >= maxStreamReconnects {
			sendDone(ctx, out, meta, fmt.Errorf("流式连接断开，已重试 %d 次仍失败：%w", maxStreamReconnects, err))
			return
		}

		c.logRetry(meta, ce)
		observability.ProviderRetry(c.cfg.Name, ce.Class.String())

		if delayErr := backoff(ctx, c.retry.retryDelay(attempt+1, ce)); delayErr != nil {
			sendDone(ctx, out, meta, delayErr)
			return
		}
	}

	sendDone(ctx, out, meta, lastErr)
}

// streamOnce 执行一次流式请求（含限流、熔断），返回是否已有输出。
func (c *Client) streamOnce(ctx context.Context, req CompletionRequest, meta RequestMeta, body []byte, out chan<- StreamChunk) (bool, error) {
	if c.limiter != nil {
		if err := c.limiter.Wait(ctx); err != nil {
			return false, err
		}
	}

	halfOpen := false
	if c.breaker != nil {
		if !c.breaker.Allow() {
			return false, &ClassifiedError{Class: ClassServerError, Err: ErrCircuitOpen}
		}
		halfOpen = c.breaker.State() == BreakerHalfOpen
	}
	defer func() {
		if c.breaker != nil && halfOpen {
			c.breaker.ReleaseHalfOpenSlot()
		}
	}()

	emitted, err := c.doStreamHTTP(ctx, req, meta, body, out)

	if c.breaker != nil {
		if err != nil {
			c.breaker.RecordFailure(Classify(err).Class)
		} else {
			c.breaker.RecordSuccess()
		}
	}
	return emitted, err
}

// doStreamHTTP 发起 HTTP 并逐分片推送到 out。
func (c *Client) doStreamHTTP(ctx context.Context, req CompletionRequest, meta RequestMeta, body []byte, out chan<- StreamChunk) (bool, error) {
	timeout := c.timeout
	if req.Timeout > 0 {
		timeout = req.Timeout
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	httpReq, err := newJSONRequest(reqCtx, c.cfg.BaseURL+"/chat/completions", body)
	if err != nil {
		return false, err
	}

	apiKey, err := c.secrets.Get(ctx, c.cfg.SecretRef)
	if err != nil {
		return false, fmt.Errorf("provider: 解析 API Key 失败: %w", err)
	}
	if apiKey == "" {
		return false, &APIError{StatusCode: 401, Code: "invalid_api_key", Message: "未配置 API Key"}
	}
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)

	httpResp, err := c.http.Do(httpReq)
	if err != nil {
		return false, err
	}
	defer func() { _ = httpResp.Body.Close() }()

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return false, c.buildAPIError(httpResp)
	}

	acc := newStreamAccumulator()
	// 空闲看门狗：服务端长时间不推数据则断开，避免永久挂起。
	idle := newIdleWatchdog(reqCtx, cancel, DefaultIdleTimeout)
	defer idle.stop()

	dec := newSSEDecoder(httpResp.Body)
	for {
		data, ok, err := dec.Next()
		if err != nil {
			if reqCtx.Err() != nil {
				return acc.emitted, ctxError(reqCtx, ctx)
			}
			return acc.emitted, fmt.Errorf("流式读取中断: %w", err)
		}
		if !ok {
			break
		}
		idle.reset()

		stop, err := acc.consume(data, func(chunk StreamChunk) {
			chunk.Meta = meta
			sendChunk(reqCtx, out, chunk)
		})
		if err != nil {
			return acc.emitted, err
		}
		if stop {
			break
		}
	}

	if reqCtx.Err() != nil {
		return acc.emitted, ctxError(reqCtx, ctx)
	}
	// 结束时补一条带 usage 的收尾分片（若 usage 已单独推过则为空通知）。
	if acc.usage != nil && !acc.emitted {
		sendChunk(reqCtx, out, StreamChunk{Usage: acc.usage, Meta: meta})
	}
	return acc.emitted, nil
}

// isConnResetClass 判定该错误类别是否属于「可重放的连接中断」。
func isConnResetClass(class ErrorClass) bool {
	switch class {
	case ClassDNS, ClassConnectTimeout:
		return true
	default:
		return false
	}
}

// ctxError 区分「用户取消」与「上层超时」。
func ctxError(reqCtx, parent context.Context) error {
	if parent.Err() != nil {
		return parent.Err()
	}
	if reqCtx.Err() != nil {
		return reqCtx.Err()
	}
	return ErrContextCancelled
}

// sendChunk 向 out 发送分片；ctx 结束时立即返回，避免消费者停止读取后 goroutine 永久阻塞。
func sendChunk(ctx context.Context, out chan<- StreamChunk, chunk StreamChunk) {
	select {
	case out <- chunk:
	case <-ctx.Done():
	}
}

// sendDone 发送终止分片（含错误），保证是流的最后一个分片。
//
// 即使 ctx 已取消也要尽力送达：消费者据 Done 判定流结束。这里用一个带兜底
// 的发送 —— 若消费者已完全放弃读取，goroutine 也不会永久泄漏。
func sendDone(ctx context.Context, out chan<- StreamChunk, meta RequestMeta, err error) {
	chunk := StreamChunk{Done: true, Err: err, Meta: meta}
	select {
	case out <- chunk:
	case <-ctx.Done():
		// 消费者已取消：再尝试一次非阻塞投递，失败则放弃（channel 由 close 收尾）。
		select {
		case out <- chunk:
		default:
		}
	}
}

// newJSONRequest 构造带 JSON Content-Type 的 POST 请求。
func newJSONRequest(ctx context.Context, url string, body []byte) (*http.Request, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	// 流式响应必须关闭缓冲：部分中间层会缓冲 SSE 导致首字节迟迟不到。
	httpReq.Header.Set("Accept", "text/event-stream")
	return httpReq, nil
}

// idleWatchdog 在空闲超时后取消请求上下文。
type idleWatchdog struct {
	mu     sync.Mutex
	timer  *time.Timer
	cancel context.CancelFunc
	d      time.Duration
	armed  bool
}

func newIdleWatchdog(ctx context.Context, cancel context.CancelFunc, d time.Duration) *idleWatchdog {
	w := &idleWatchdog{cancel: cancel, d: d}
	w.timer = time.AfterFunc(d, func() {
		w.mu.Lock()
		armed := w.armed
		w.mu.Unlock()
		if armed {
			cancel()
		}
	})
	w.armed = true
	return w
}

func (w *idleWatchdog) reset() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.armed {
		return
	}
	w.timer.Reset(w.d)
}

func (w *idleWatchdog) stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.armed = false
	w.timer.Stop()
}
