package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/catalog"
	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/model"
	"github.com/ximo888ok-netizen/ximo-agent/internal/provider"
)

// errEmptyStream 表示上游流未产出任何分片就结束（既没有数据也没有错误分片）。
var errEmptyStream = errors.New("anthropic: 上游流未产出任何分片")

// sseWriter 按 Anthropic 约定输出 SSE：`event: <type>` + `data: {...}` + 空行。
// 每个事件后立即 Flush —— 否则中间层会缓冲整条流，客户端「一个字都不来」。
type sseWriter struct {
	w http.ResponseWriter
	f http.Flusher
}

func newSSEWriter(w http.ResponseWriter) *sseWriter {
	f, _ := w.(http.Flusher)
	return &sseWriter{w: w, f: f}
}

// writeHeader 写 SSE 响应头。必须在任何 body 之前调用一次。
func (s *sseWriter) writeHeader() {
	h := s.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	s.w.WriteHeader(http.StatusOK)
	s.flush()
}

// event 写一个事件。payload 序列化失败（不该发生）时静默跳过，避免 panic 打断整条流。
func (s *sseWriter) event(name string, payload any) {
	buf, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_, _ = s.w.Write([]byte("event: " + name + "\ndata: "))
	_, _ = s.w.Write(buf)
	_, _ = s.w.Write([]byte("\n\n"))
	s.flush()
}

func (s *sseWriter) flush() {
	if s.f != nil {
		s.f.Flush()
	}
}

// toolBlock 是流式期间一个 tool_use 内容块的累积状态。
type toolBlock struct {
	index int
	id    string
	name  string
	open  bool
}

// messageStream 是一棵「OpenAI 分片 → Anthropic 事件」的状态机。
//
// 事件序列（契约 §11.5）：
//
//	message_start → (content_block_start → content_block_delta* → content_block_stop)*
//	  → message_delta → message_stop
//
// 空响应（没有任何文本与工具调用）只发 message_start/message_delta/message_stop，
// 不发空的内容块 —— 这与 Anthropic 在 max_tokens=1 之类场景下的行为一致。
type messageStream struct {
	sse   *sseWriter
	id    string
	model string

	filter *stopFilter

	started   bool
	nextIndex int
	openIndex int
	openType  string

	tools map[int]*toolBlock
}

func newMessageStream(sse *sseWriter, id, model string, stopSeqs []string) *messageStream {
	return &messageStream{
		sse:       sse,
		id:        id,
		model:     model,
		filter:    newStopFilter(stopSeqs),
		openIndex: -1,
		tools:     make(map[int]*toolBlock),
	}
}

// start 发 message_start（幂等）。必须在上游确认可产出之后调用 —— 这是
// 「首字节前失败要能回普通 JSON 错误」的唯一保障。
//
// message_start 里的 input_tokens 记 0：上游 usage 只在流末尾上报，此刻无从得知；
// 真实值在末尾的 message_delta.usage 里给出，不在这里填估算值（§6 禁止估算冒充用量）。
func (m *messageStream) start() {
	if m.started {
		return
	}
	m.started = true
	m.sse.writeHeader()
	m.sse.event("message_start", messageStartEvent{
		Type: "message_start",
		Message: messageResponse{
			ID:      m.id,
			Type:    "message",
			Role:    "assistant",
			Model:   m.model,
			Content: []any{},
			Usage:   usageBlock{},
		},
	})
}

// text 喂入一段文本增量，返回是否已命中客户端的 stop 序列（命中即应立刻收尾）。
func (m *messageStream) text(delta string) bool {
	if delta == "" {
		return m.filter.stopped()
	}
	emit, stopped := m.filter.feed(delta)
	if emit != "" {
		m.ensureTextBlock()
		m.sse.event("content_block_delta", contentBlockDeltaEvent{
			Type:  "content_block_delta",
			Index: m.openIndex,
			Delta: textDelta{Type: "text_delta", Text: emit},
		})
	}
	return stopped
}

// toolCalls 喂入工具调用增量。
//
// 映射：OpenAI 的 tool_calls[i] → Anthropic 的 tool_use 内容块，
// 第一个带 id/name 的分片触发 content_block_start，后续 arguments 分片作为
// input_json_delta 透传（客户端自行拼接 partial_json）。
//
// 已知边界：OpenAI 的分片按 index 顺序到达时一一对应；若上游把同一 index 的
// 参数分片穿插到别的 index 之后（极罕见），本状态机会为它再开一个 tool_use 块。
func (m *messageStream) toolCalls(deltas []provider.ToolCallDelta) {
	for _, d := range deltas {
		if m.filter.stopped() {
			return
		}
		blk := m.tools[d.Index]
		if blk == nil {
			blk = &toolBlock{}
			m.tools[d.Index] = blk
		}
		if d.ID != "" {
			blk.id = d.ID
		}
		if d.Name != "" {
			blk.name += d.Name
		}
		if !blk.open {
			if m.openIndex >= 0 {
				m.closeBlock()
			}
			blk.index = m.nextIndex
			m.nextIndex++
			blk.open = true
			m.openIndex = blk.index
			m.openType = blockToolUse
			m.sse.event("content_block_start", contentBlockStartEvent{
				Type:  "content_block_start",
				Index: blk.index,
				ContentBlock: toolUseBlock{
					Type:  blockToolUse,
					ID:    blk.id,
					Name:  blk.name,
					Input: json.RawMessage("{}"),
				},
			})
		}
		if d.Arguments != "" {
			m.sse.event("content_block_delta", contentBlockDeltaEvent{
				Type:  "content_block_delta",
				Index: blk.index,
				Delta: inputJSONDelta{Type: "input_json_delta", PartialJSON: d.Arguments},
			})
		}
	}
}

// ensureTextBlock 保证当前有一个打开的 text 块。
func (m *messageStream) ensureTextBlock() {
	if m.openIndex >= 0 && m.openType == blockText {
		return
	}
	if m.openIndex >= 0 {
		m.closeBlock()
	}
	m.openIndex = m.nextIndex
	m.nextIndex++
	m.openType = blockText
	m.sse.event("content_block_start", contentBlockStartEvent{
		Type:         "content_block_start",
		Index:        m.openIndex,
		ContentBlock: textBlock{Type: blockText, Text: ""},
	})
}

// closeBlock 关闭当前打开的内容块（幂等）。
func (m *messageStream) closeBlock() {
	if m.openIndex < 0 {
		return
	}
	m.sse.event("content_block_stop", contentBlockStopEvent{Type: "content_block_stop", Index: m.openIndex})
	m.openIndex = -1
	m.openType = ""
}

// hasToolUse 报告本次响应是否产出过工具调用块。
func (m *messageStream) hasToolUse() bool { return len(m.tools) > 0 }

// finish 收尾：冲刷截断器滞留的尾部、关闭内容块、发 message_delta 与 message_stop。
func (m *messageStream) finish(stopReason string, inTok, outTok int64) {
	if tail := m.filter.flush(); tail != "" {
		m.ensureTextBlock()
		m.sse.event("content_block_delta", contentBlockDeltaEvent{
			Type:  "content_block_delta",
			Index: m.openIndex,
			Delta: textDelta{Type: "text_delta", Text: tail},
		})
	}
	m.closeBlock()

	delta := messageDeltaBody{StopReason: &stopReason}
	if seq := m.filter.matchedSequence(); seq != "" {
		delta.StopSequence = &seq
	}
	m.sse.event("message_delta", messageDeltaEvent{
		Type:  "message_delta",
		Delta: delta,
		Usage: usageBlock{InputTokens: inTok, OutputTokens: outTok},
	})
	m.sse.event("message_stop", messageStopEvent{Type: "message_stop"})
}

// fail 发送错误事件终止流（契约 §11.5：首字节后失败走 error 事件）。
func (m *messageStream) fail(message string) {
	m.closeBlock()
	m.sse.event("error", errorEvent{
		Type:  "error",
		Error: errorEventBody{Type: "api_error", Message: message},
	})
}

// streamMessage 是流式路径：先等到上游确实产出（首个分片），再开始回写客户端。
//
// 这样做的原因（契约 §11.5）：首字节前的失败必须能返回普通 JSON 错误（非 2xx），
// 一旦写了 SSE 头就只能发错误事件。因此在拿到第一个非错误分片之前，本函数不会写任何
// 响应字节，候选切换也只可能发生在这个窗口内。
func (h *handler) streamMessage(w http.ResponseWriter, r *http.Request, tr *translated, requestID string, reservation model.Reservation, cands []catalog.Candidate) {
	ctx := r.Context()
	guard := &reservationGuard{h: h, rsv: reservation}
	defer guard.abandon(ctx)

	var lastErr error
	lastInfo := h.baseInfo(requestID, reservation, tr.model, cands[0])

	for _, cand := range cands {
		client, err := h.d.Upstream.ClientFor(ctx, cand.Provider)
		if err != nil {
			lastErr = err
			lastInfo = h.baseInfo(requestID, reservation, tr.model, cand)
			h.log().Warn(ctx, "anthropic: 上游客户端构建失败", map[string]any{
				"request_id":  requestID,
				"provider_id": cand.Provider.ID,
				"error":       err.Error(),
			})
			continue
		}

		info := h.baseInfo(requestID, reservation, tr.model, cand)
		start := time.Now()
		ch, err := client.Stream(ctx, tr.completionRequest(cand.UpstreamModel, requestID))
		if err != nil {
			lastErr, lastInfo = err, info
			if !retryableForCandidate(err) {
				info.latencyMS = time.Since(start).Milliseconds()
				guard.fail(ctx, info, failureStatus(err, ctx.Err()), "upstream_rejected")
				writeUpstreamError(w, r, err)
				return
			}
			continue
		}

		first, ok := awaitChunk(ctx, ch)
		if !ok {
			if cerr := ctx.Err(); cerr != nil {
				lastErr, lastInfo = cerr, info
				break
			}
			lastErr, lastInfo = errEmptyStream, info
			continue
		}
		if first.Err != nil {
			drain(ch)
			lastErr, lastInfo = first.Err, info
			if !retryableForCandidate(first.Err) {
				info.latencyMS = time.Since(start).Milliseconds()
				guard.fail(ctx, info, failureStatus(first.Err, ctx.Err()), "upstream_rejected")
				writeUpstreamError(w, r, first.Err)
				return
			}
			h.log().Warn(ctx, "anthropic: 候选流式失败，尝试下一个上游", map[string]any{
				"request_id":  requestID,
				"provider_id": cand.Provider.ID,
				"error_class": provider.Classify(first.Err).Class.String(),
			})
			continue
		}

		info.latencyMS = time.Since(start).Milliseconds()
		h.pumpStream(ctx, w, tr, guard, info, first, ch)
		return
	}

	guard.fail(ctx, lastInfo, failureStatus(lastErr, ctx.Err()), "no_candidate_succeeded")
	writeUpstreamError(w, r, lastErr)
}

// pumpStream 把上游分片翻成 Anthropic 事件并完成结算。
func (h *handler) pumpStream(ctx context.Context, w http.ResponseWriter, tr *translated, guard *reservationGuard, info callInfo, first provider.StreamChunk, ch <-chan provider.StreamChunk) {
	ms := newMessageStream(newSSEWriter(w), newID("msg"), tr.model, tr.stopSequences)
	ms.start()

	var (
		usage  *provider.TokenUsage
		runErr error
		done   bool
		// upFinish 上游显式报告的结束原因；空 = 上游没报，收尾走推断。
		upFinish provider.FinishReason
	)

	handle := func(c provider.StreamChunk) {
		if c.Usage != nil {
			usage = c.Usage
		}
		if c.Err != nil {
			runErr = c.Err
			done = true
			return
		}
		if c.FinishReason != "" {
			// provider 在最后一个内容分片之后、Done 之前补发的「仅带结束原因」分片。
			upFinish = c.FinishReason
		}
		if c.Content != "" && ms.text(c.Content) {
			// 命中客户端 stop 序列：立刻收尾（剩余上游分片不再消费，连接随 handler 返回释放）。
			done = true
			return
		}
		if len(c.ToolCalls) > 0 {
			ms.toolCalls(c.ToolCalls)
		}
		if c.Done {
			done = true
		}
	}

	handle(first)
	for !done {
		c, ok := awaitChunk(ctx, ch)
		if !ok {
			done = true
			if cerr := ctx.Err(); cerr != nil {
				runErr = cerr
			} else {
				runErr = provider.ErrContextCancelled
			}
			break
		}
		handle(c)
	}

	if usage != nil {
		info.inputTok = int64(usage.PromptTokens)
		info.outputTok = int64(usage.CompletionTokens)
	}

	if runErr != nil {
		status := failureStatus(runErr, ctx.Err())
		ms.fail(streamErrorMessage(status))
		guard.fail(ctx, info, status, "stream_failed")
		return
	}

	ms.finish(streamStopReason(upFinish, info.outputTok, int64(tr.maxTokens), ms.hasToolUse(), ms.filter.matchedSequence()), info.inputTok, info.outputTok)
	guard.settle(ctx, info)
}

// streamStopReason 决定 message_delta 的 stop_reason。
//
// 映射（与 translate.stopReasonFor 的非流式口径一致）：length → max_tokens、
// tool_calls → tool_use、stop → end_turn；上游自定义的未知取值归为 end_turn，
// 避免把 Anthropic 客户端无法识别的 stop_reason 透出去。命中客户端 stop 序列时
// 优先报 stop_sequence —— 那次截断是本网关造成的，比上游结束原因更准确。
//
// 为什么保留推断（up == "" 的回退）：OpenAI 兼容上游里存在只发 delta + [DONE]、
// 整条流都不报 finish_reason 的实现，那类流拿不到结束原因，只能沿用老口径
// （见 inferredStopReason），否则「被 max_tokens 截断」会被一律报成 end_turn。
func streamStopReason(up provider.FinishReason, outTok, maxTokens int64, hasToolUse bool, matched string) string {
	if matched != "" {
		return stopReasonStopSequence
	}
	switch up {
	case provider.FinishLength:
		return stopReasonMaxTokens
	case provider.FinishToolCalls:
		return stopReasonToolUse
	case "":
		return inferredStopReason(outTok, maxTokens, hasToolUse, matched)
	default:
		// stop 以及上游自定义取值。
		return stopReasonEndTurn
	}
}

// inferredStopReason 推断流式响应的 stop_reason。
//
// 这是**回退路径**：上游显式报了 finish_reason 时由 streamStopReason 直接映射，不走到这里。
// 只有上游整条流都没报结束原因（provider.StreamChunk.FinishReason 为空）时才用它：
// 用「输出 token 达到 max_tokens」推断截断（上游按该上限停止，正常结束几乎不可能精确撞上
// 这个数）；推断不出时按 end_turn 报，工具调用则报 tool_use。
// 已知误差：模型恰好自然结束在 max_tokens 上时会被报成 max_tokens；上游按 length 截断但
// usage 未上报（outTok 为 0）时会被报成 end_turn。
func inferredStopReason(outTok, maxTokens int64, hasToolUse bool, matched string) string {
	switch {
	case matched != "":
		return stopReasonStopSequence
	case maxTokens > 0 && outTok >= maxTokens:
		return stopReasonMaxTokens
	case hasToolUse:
		return stopReasonToolUse
	default:
		return stopReasonEndTurn
	}
}

// streamErrorMessage 是流式错误事件的客户端可见文案（固定串，不透传上游细节）。
func streamErrorMessage(status string) string {
	switch status {
	case usageStatusClientCanceled:
		return "client canceled the request"
	case usageStatusUpstreamTimeout:
		return "upstream request timed out"
	default:
		return "upstream stream failed"
	}
}

// awaitChunk 读一个分片；ctx 结束时立刻返回 ok=false（客户端断开不可无限等待）。
func awaitChunk(ctx context.Context, ch <-chan provider.StreamChunk) (provider.StreamChunk, bool) {
	select {
	case c, ok := <-ch:
		return c, ok
	case <-ctx.Done():
		return provider.StreamChunk{}, false
	}
}

// drain 后台排空被放弃的流通道，避免上游 goroutine 卡在发送上
// （handler 返回时请求 ctx 会被取消，goroutine 随之退出）。
func drain(ch <-chan provider.StreamChunk) {
	go func() {
		for range ch {
		}
	}()
}
