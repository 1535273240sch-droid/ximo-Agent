package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/observability"
)

// ---------------------------------------------------------------------------
// SSE 解码与累积
// ---------------------------------------------------------------------------

// TestSSEDecoderParsesDataLines 确认只挑 data: 行、跳过注释与空行。
func TestSSEDecoderParsesDataLines(t *testing.T) {
	body := strings.Join([]string{
		": 心跳注释",
		"",
		`data: {"choices":[{"delta":{"content":"你"}}]}`,
		"",
		"event: ping",
		`data: {"choices":[{"delta":{"content":"好"}}]}`,
		"data: [DONE]",
		"",
	}, "\n")

	dec := newSSEDecoder(strings.NewReader(body))
	var payloads []string
	for {
		data, ok, err := dec.Next()
		if err != nil {
			t.Fatalf("Next 报错: %v", err)
		}
		if !ok {
			break
		}
		payloads = append(payloads, data)
	}

	if len(payloads) != 3 {
		t.Fatalf("应解析出 3 条 data，实际 %d: %v", len(payloads), payloads)
	}
	if payloads[2] != "[DONE]" {
		t.Fatalf("最后一条应为 [DONE]，实际 %q", payloads[2])
	}
}

// TestAccumulatorCollectsContentAndToolCalls 确认增量累积与工具调用拼接。
func TestAccumulatorCollectsContentAndToolCalls(t *testing.T) {
	acc := newStreamAccumulator()

	chunks := []string{
		`{"choices":[{"delta":{"content":"Hello "}}]}`,
		`{"choices":[{"delta":{"reasoning_content":"think"}}]}`,
		`{"choices":[{"delta":{"content":"world"}}]}`,
		// 工具调用分片到达：name 与 arguments 都要拼接。
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"file_","arguments":"{\"pa"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"read","arguments":"th\":\"a.txt\"}"}}]}}]}`,
	}
	for _, c := range chunks {
		if _, err := acc.consume(c, nil); err != nil {
			t.Fatalf("consume 报错: %v", err)
		}
	}

	resp := acc.toResponse()
	if resp.Content != "Hello world" {
		t.Errorf("Content = %q, want %q", resp.Content, "Hello world")
	}
	if resp.ReasoningContent != "think" {
		t.Errorf("ReasoningContent = %q", resp.ReasoningContent)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("应有 1 个工具调用，实际 %d", len(resp.ToolCalls))
	}
	if resp.ToolCalls[0].Name != "file_read" {
		t.Errorf("工具名拼接失败: %q", resp.ToolCalls[0].Name)
	}
	if resp.ToolCalls[0].Arguments != `{"path":"a.txt"}` {
		t.Errorf("参数拼接失败: %q", resp.ToolCalls[0].Arguments)
	}
	if resp.FinishReason != FinishToolCalls {
		t.Errorf("有 tool_calls 时结束原因应为 tool_calls，实际 %v", resp.FinishReason)
	}
	if !resp.Emitted {
		t.Error("有输出后 Emitted 应为 true")
	}
}

// TestAccumulatorSkipsMalformedJSON 确认坏分片不中断整条流（v1 同款容错）。
func TestAccumulatorSkipsMalformedJSON(t *testing.T) {
	acc := newStreamAccumulator()
	if _, err := acc.consume(`{"choices":[{"delta":{"content":"ok`, nil); err != nil {
		t.Fatalf("不完整 JSON 应被跳过而非报错: %v", err)
	}
	if _, err := acc.consume(`{"choices":[{"delta":{"content":"好"}}]}`, nil); err != nil {
		t.Fatalf("consume 报错: %v", err)
	}
	if got := acc.toResponse().Content; got != "好" {
		t.Fatalf("Content = %q, want 好", got)
	}
}

// TestAccumulatorReportsUsage 确认 usage 归一化（含嵌套 cached_tokens 形态）。
func TestAccumulatorReportsUsage(t *testing.T) {
	acc := newStreamAccumulator()
	_, err := acc.consume(`{"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":10,"prompt_cache_hit_tokens":70}}`, nil)
	if err != nil {
		t.Fatalf("consume 报错: %v", err)
	}
	u := acc.toResponse().Usage
	if u == nil {
		t.Fatal("usage 不应为空")
	}
	if u.CacheHitTokens != 70 || u.CacheMissTokens != 30 {
		t.Fatalf("hit=%d miss=%d, want 70/30", u.CacheHitTokens, u.CacheMissTokens)
	}
}

// TestAccumulatorStopOnDone 确认 [DONE] 触发停止。
func TestAccumulatorStopOnDone(t *testing.T) {
	acc := newStreamAccumulator()
	stop, err := acc.consume("[DONE]", nil)
	if err != nil {
		t.Fatalf("报错: %v", err)
	}
	if !stop {
		t.Fatal("[DONE] 应返回 stop=true")
	}
}

// TestAccumulatorOnChunkCallback 确认分片回调被调用（配合任务 02 的背压事件分级）。
func TestAccumulatorOnChunkCallback(t *testing.T) {
	acc := newStreamAccumulator()
	var contents []string
	_, err := acc.consume(`{"choices":[{"delta":{"content":"abc"}}]}`, func(c StreamChunk) {
		if c.Content != "" {
			contents = append(contents, c.Content)
		}
	})
	if err != nil {
		t.Fatalf("报错: %v", err)
	}
	if len(contents) != 1 || contents[0] != "abc" {
		t.Fatalf("回调内容 = %v", contents)
	}
}

// ---------------------------------------------------------------------------
// 客户端调用链（限流 → 熔断 → 重试 → HTTP）
// ---------------------------------------------------------------------------

// fakeDoer 记录请求并返回预设响应序列。
type fakeDoer struct {
	calls    atomic.Int64
	handler  func(call int, req *http.Request) (*http.Response, error)
	lastAuth atomic.Value
}

func (f *fakeDoer) Do(req *http.Request) (*http.Response, error) {
	n := int(f.calls.Add(1))
	f.lastAuth.Store(req.Header.Get("Authorization"))
	return f.handler(n, req)
}

func httpResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}

func staticSecrets(key string) SecretResolver {
	return SecretResolverFunc(func(context.Context, string) (string, error) { return key, nil })
}

func newTestClient(t *testing.T, doer HTTPDoer, opts ...func(*ClientOptions)) *Client {
	t.Helper()
	o := ClientOptions{
		Config: ProviderConfig{
			ID:              "test",
			Name:            "test",
			BaseURL:         "https://api.example.com",
			ContextWindow:   100000,
			MaxOutputTokens: 8192,
			Capabilities:    DefaultCapabilities(),
		},
		Secrets:        staticSecrets("sk-test-key-1234567890"),
		HTTPClient:     doer,
		Retry:          RetryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond, Multiplier: 2},
		DefaultTimeout: 5 * time.Second,
		// 测试用静默 logger：避免数百行 JSON 日志淹没测试输出。
		Logger: observability.NewLogger(io.Discard, observability.LevelError),
	}
	for _, fn := range opts {
		fn(&o)
	}
	c, err := NewClient(o)
	if err != nil {
		t.Fatalf("NewClient 报错: %v", err)
	}
	return c
}

const sseOK = "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\ndata: [DONE]\n\n"

// TestClientCompleteSuccess 确认成功路径。
func TestClientCompleteSuccess(t *testing.T) {
	doer := &fakeDoer{handler: func(int, *http.Request) (*http.Response, error) {
		return httpResponse(200, sseOK), nil
	}}
	c := newTestClient(t, doer)

	resp, err := c.Complete(context.Background(), CompletionRequest{Model: "m", Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatalf("Complete 报错: %v", err)
	}
	if resp.Content != "hello" {
		t.Fatalf("Content = %q", resp.Content)
	}
	if resp.Attempts != 1 {
		t.Fatalf("Attempts = %d, want 1", resp.Attempts)
	}
	if resp.Meta.RequestID == "" {
		t.Fatal("应自动生成 request_id")
	}
	if resp.Meta.Attempt != 1 {
		t.Fatalf("Meta.Attempt = %d, want 1", resp.Meta.Attempt)
	}
}

// TestClientRetriesServerError 确认 5xx 被重试。
func TestClientRetriesServerError(t *testing.T) {
	doer := &fakeDoer{handler: func(call int, _ *http.Request) (*http.Response, error) {
		if call < 3 {
			return httpResponse(503, `{"error":{"message":"unavailable"}}`), nil
		}
		return httpResponse(200, sseOK), nil
	}}
	c := newTestClient(t, doer)

	resp, err := c.Complete(context.Background(), CompletionRequest{Model: "m"})
	if err != nil {
		t.Fatalf("应在第 3 次成功，实际报错: %v", err)
	}
	if resp.Content != "hello" {
		t.Fatalf("Content = %q", resp.Content)
	}
	if resp.Attempts != 3 {
		t.Fatalf("Attempts = %d, want 3", resp.Attempts)
	}
}

// TestClientDoesNotRetry400 确认 400 不重试（只发一次请求）。
func TestClientDoesNotRetry400(t *testing.T) {
	doer := &fakeDoer{handler: func(int, *http.Request) (*http.Response, error) {
		return httpResponse(400, `{"error":{"message":"bad request"}}`), nil
	}}
	c := newTestClient(t, doer)

	_, err := c.Complete(context.Background(), CompletionRequest{Model: "m"})
	if err == nil {
		t.Fatal("400 应返回错误")
	}
	if got := doer.calls.Load(); got != 1 {
		t.Fatalf("400 不应重试，实际请求 %d 次", got)
	}

	var ce *ClassifiedError
	if !errors.As(err, &ce) {
		t.Fatalf("错误应可解析为 ClassifiedError: %T", err)
	}
	if ce.Class != ClassBadRequest {
		t.Fatalf("分类 = %v, want bad_request", ce.Class)
	}
}

// TestClientRetries429WithRetryAfter 确认 429 遵守 Retry-After 且重试。
func TestClientRetries429WithRetryAfter(t *testing.T) {
	doer := &fakeDoer{handler: func(call int, _ *http.Request) (*http.Response, error) {
		if call == 1 {
			resp := httpResponse(429, `{"error":{"message":"rate limited"}}`)
			resp.Header.Set("Retry-After", "0.01")
			return resp, nil
		}
		return httpResponse(200, sseOK), nil
	}}
	c := newTestClient(t, doer)

	start := time.Now()
	resp, err := c.Complete(context.Background(), CompletionRequest{Model: "m"})
	if err != nil {
		t.Fatalf("应在重试后成功: %v", err)
	}
	if resp.Attempts != 2 {
		t.Fatalf("Attempts = %d, want 2", resp.Attempts)
	}
	if elapsed := time.Since(start); elapsed < 10*time.Millisecond {
		t.Fatalf("应遵守 Retry-After 等待，实际仅 %v", elapsed)
	}
}

// TestClientRetryExhaustionTerminates 确认重试次数用尽后终止，不死循环。
//
// 验收标准「Provider 断网/429/5xx 不造成死循环」的直接断言。
func TestClientRetryExhaustionTerminates(t *testing.T) {
	doer := &fakeDoer{handler: func(int, *http.Request) (*http.Response, error) {
		return httpResponse(500, `{"error":{"message":"boom"}}`), nil
	}}
	c := newTestClient(t, doer)

	done := make(chan error, 1)
	go func() {
		_, err := c.Complete(context.Background(), CompletionRequest{Model: "m"})
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("持续 500 应最终报错")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("持续 500 导致死循环（超时未返回）")
	}

	if got := doer.calls.Load(); got != 3 {
		t.Fatalf("应按 MaxAttempts=3 终止，实际请求 %d 次", got)
	}
}

// TestClientUserCancelNoRetry 确认用户取消不重试。
func TestClientUserCancelNoRetry(t *testing.T) {
	doer := &fakeDoer{handler: func(int, *http.Request) (*http.Response, error) {
		return nil, context.Canceled
	}}
	c := newTestClient(t, doer)

	_, err := c.Complete(context.Background(), CompletionRequest{Model: "m"})
	if err == nil {
		t.Fatal("应返回错误")
	}
	if got := doer.calls.Load(); got != 1 {
		t.Fatalf("取消不应重试，实际 %d 次", got)
	}
}

// TestClientBreakerShortCircuits 确认熔断打开后请求被短路（不再打到网络）。
func TestClientBreakerShortCircuits(t *testing.T) {
	breaker := NewCircuitBreaker(2, time.Minute)
	doer := &fakeDoer{handler: func(int, *http.Request) (*http.Response, error) {
		return httpResponse(500, `{"error":{"message":"boom"}}`), nil
	}}
	c := newTestClient(t, doer, func(o *ClientOptions) { o.Breaker = breaker })

	// 第一次请求：3 次尝试全失败（每次都记一次熔断失败）→ 熔断打开。
	_, _ = c.Complete(context.Background(), CompletionRequest{Model: "m"})
	if breaker.State() != BreakerOpen {
		t.Fatalf("应为 open，实际 %v", breaker.State())
	}

	callsBefore := doer.calls.Load()
	_, err := c.Complete(context.Background(), CompletionRequest{Model: "m"})
	if err == nil {
		t.Fatal("熔断后应报错")
	}
	if doer.calls.Load() != callsBefore {
		t.Fatal("熔断打开后不应再打到网络")
	}
}

// TestClientNeverLeaksAPIKeyInError 确认 API Key 不进入错误消息（第 21 章红线）。
func TestClientNeverLeaksAPIKeyInError(t *testing.T) {
	const secret = "sk-super-secret-key-do-not-leak"
	doer := &fakeDoer{handler: func(int, *http.Request) (*http.Response, error) {
		return httpResponse(500, `{"error":{"message":"server error"}}`), nil
	}}
	c := newTestClient(t, doer, func(o *ClientOptions) {
		o.Secrets = staticSecrets(secret)
	})

	_, err := c.Complete(context.Background(), CompletionRequest{Model: "m"})
	if err == nil {
		t.Fatal("应报错")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("错误消息泄露了 API Key: %v", err)
	}

	// 请求头里确实带了 key（证明注入生效，测试不是空转）。
	auth, _ := doer.lastAuth.Load().(string)
	if auth == "" {
		t.Fatal("Authorization 头应被设置（测试前提）")
	}
	if strings.Contains(err.Error(), auth) {
		t.Fatal("错误消息泄露了 Authorization 头")
	}
}

// TestClientStreamZeroOutputReplay 确认「零输出 + 连接断开」会重放。
func TestClientStreamZeroOutputReplay(t *testing.T) {
	doer := &fakeDoer{handler: func(call int, _ *http.Request) (*http.Response, error) {
		if call == 1 {
			// 首次：连接错误（无任何输出）。
			return nil, &netTimeoutErr{}
		}
		return httpResponse(200, sseOK), nil
	}}
	c := newTestClient(t, doer)

	ch, err := c.Stream(context.Background(), CompletionRequest{Model: "m"})
	if err != nil {
		t.Fatalf("Stream 报错: %v", err)
	}

	var content strings.Builder
	var finalErr error
	for chunk := range ch {
		content.WriteString(chunk.Content)
		if chunk.Done {
			finalErr = chunk.Err
		}
	}
	if finalErr != nil {
		t.Fatalf("零输出重放后应成功，实际错误: %v", finalErr)
	}
	if content.String() != "hello" {
		t.Fatalf("Content = %q, want hello", content.String())
	}
}

// TestClientStreamNoReplayAfterOutput 确认「已有输出」后断开不会重放（避免重复输出）。
//
// 这是 v1 emitted 标志的核心保护，也是验收标准「流式中断不产生重复输出」的断言。
func TestClientStreamNoReplayAfterOutput(t *testing.T) {
	doer := &fakeDoer{handler: func(call int, _ *http.Request) (*http.Response, error) {
		// 先把一个分片推出去，然后中断。
		body := "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"
		return &http.Response{
			StatusCode: 200,
			Body:       &errAfterReader{data: []byte(body)},
			Header:     make(http.Header),
		}, nil
	}}
	c := newTestClient(t, doer)

	ch, err := c.Stream(context.Background(), CompletionRequest{Model: "m"})
	if err != nil {
		t.Fatalf("Stream 报错: %v", err)
	}

	var content strings.Builder
	var finalErr error
	for chunk := range ch {
		content.WriteString(chunk.Content)
		if chunk.Done {
			finalErr = chunk.Err
		}
	}

	if content.String() != "partial" {
		t.Fatalf("Content = %q, want partial（不应重复）", content.String())
	}
	if finalErr == nil {
		t.Fatal("已有输出后中断应报错")
	}
	if !strings.Contains(finalErr.Error(), "不重放") {
		t.Fatalf("错误应说明不重放的原因，实际: %v", finalErr)
	}
	// 只应发一次 HTTP 请求。
	if got := doer.calls.Load(); got != 1 {
		t.Fatalf("已有输出不应重放，实际请求 %d 次", got)
	}
}

// TestClientStreamDoneAlwaysLast 确认 Done 分片总是最后一个（任务 02 依赖此约定）。
func TestClientStreamDoneAlwaysLast(t *testing.T) {
	doer := &fakeDoer{handler: func(int, *http.Request) (*http.Response, error) {
		return httpResponse(200, sseOK), nil
	}}
	c := newTestClient(t, doer)

	ch, err := c.Stream(context.Background(), CompletionRequest{Model: "m"})
	if err != nil {
		t.Fatalf("Stream 报错: %v", err)
	}

	var sawDone bool
	count := 0
	for chunk := range ch {
		count++
		if sawDone {
			t.Fatal("Done 之后不应再有分片")
		}
		if chunk.Done {
			sawDone = true
		}
	}
	if !sawDone {
		t.Fatal("必须发送 Done 分片")
	}
	if count < 2 {
		t.Fatalf("应至少有内容分片 + Done，实际 %d", count)
	}
}

// trailingUsageSSE 是 OpenAI include_usage 的真实分片顺序：
// 内容 → finish_reason → usage（独立分片）→ [DONE]。
const trailingUsageSSE = "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n" +
	"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
	"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":7,\"total_tokens\":18}}\n\n" +
	"data: [DONE]\n\n"

// TestClientStreamKeepsTrailingUsageAfterFinish 是「尾随 usage」的回归用例（D1）。
//
// 缺陷：主循环读到 finish_reason 就 break，而 include_usage 形态把 usage 放在它**之后**
// 的独立分片里 → usage 永远取不到 → 调用方只能按 0 token 计费（等于这次请求免计费）。
// 修复前本用例在「usage 必须被取到」处失败（负控见报告：去掉扫尾后本用例变红）。
//
// 同时钉住两件事：① 结束原因作为独立分片透出（网关据此决定 stop_reason）；
// ② 扫尾不得重复投递内容（文本不能变成 "hellohello"）。
func TestClientStreamKeepsTrailingUsageAfterFinish(t *testing.T) {
	doer := &fakeDoer{handler: func(int, *http.Request) (*http.Response, error) {
		return httpResponse(200, trailingUsageSSE), nil
	}}
	c := newTestClient(t, doer)

	ch, err := c.Stream(context.Background(), CompletionRequest{Model: "m"})
	if err != nil {
		t.Fatalf("Stream 报错: %v", err)
	}

	var (
		content    strings.Builder
		contentN   int
		usage      *TokenUsage
		usageAt    int
		finish     FinishReason
		finishAt   int
		doneAt     int
		doneChunks int
		n          int
	)
	for chunk := range ch {
		n++
		if chunk.Content != "" {
			contentN++
			content.WriteString(chunk.Content)
		}
		if chunk.Usage != nil {
			usage, usageAt = chunk.Usage, n
		}
		if chunk.FinishReason != "" {
			finish, finishAt = chunk.FinishReason, n
		}
		if chunk.Done {
			doneChunks++
			doneAt = n
		}
	}

	if usage == nil {
		t.Fatal("尾随 usage 被丢掉：finish_reason 之后的 usage 分片未被解析（D1 回归）")
	}
	if usage.PromptTokens != 11 || usage.CompletionTokens != 7 || usage.TotalTokens != 18 {
		t.Fatalf("usage = %d/%d/%d，期望 11/7/18", usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens)
	}
	if got := content.String(); got != "hello" {
		t.Fatalf("Content = %q，期望 hello（扫尾不得重复投递内容）", got)
	}
	if contentN != 1 {
		t.Fatalf("内容分片应只有 1 条，实际 %d 条", contentN)
	}
	if finish != FinishStop {
		t.Fatalf("结束原因分片 = %q，期望 stop（上游显式报告的值）", finish)
	}
	if !(finishAt < usageAt && usageAt < doneAt) {
		t.Fatalf("分片顺序应为 finish(%d) → usage(%d) → Done(%d)", finishAt, usageAt, doneAt)
	}
	if doneChunks != 1 || doneAt != n {
		t.Fatalf("Done 应恰好一条且为最后一条：doneChunks=%d doneAt=%d n=%d", doneChunks, doneAt, n)
	}
}

// TestClientCompleteKeepsTrailingUsageAfterFinish 确认非流式聚合路径同样不丢尾随 usage
// （请求体照旧带 include_usage，上游按同一分片顺序回；聚合器在 finish_reason 处收尾会丢）。
func TestClientCompleteKeepsTrailingUsageAfterFinish(t *testing.T) {
	doer := &fakeDoer{handler: func(int, *http.Request) (*http.Response, error) {
		return httpResponse(200, trailingUsageSSE), nil
	}}
	c := newTestClient(t, doer)

	resp, err := c.Complete(context.Background(), CompletionRequest{Model: "m"})
	if err != nil {
		t.Fatalf("Complete 报错: %v", err)
	}
	if resp.Content != "hello" {
		t.Fatalf("Content = %q，期望 hello", resp.Content)
	}
	if resp.FinishReason != FinishStop {
		t.Fatalf("FinishReason = %q，期望 stop", resp.FinishReason)
	}
	if resp.Usage == nil {
		t.Fatal("聚合结果丢失尾随 usage（D1 回归）")
	}
	if resp.Usage.PromptTokens != 11 || resp.Usage.CompletionTokens != 7 {
		t.Fatalf("usage = %d/%d，期望 11/7", resp.Usage.PromptTokens, resp.Usage.CompletionTokens)
	}
}

// TestClientStreamNoFinishChunkWhenUpstreamSilent 确认上游整条流都不报 finish_reason 时
// 不得伪造结束原因分片 —— 空值正是网关回退到推断的判据。
func TestClientStreamNoFinishChunkWhenUpstreamSilent(t *testing.T) {
	doer := &fakeDoer{handler: func(int, *http.Request) (*http.Response, error) {
		return httpResponse(200, sseOK), nil
	}}
	c := newTestClient(t, doer)

	ch, err := c.Stream(context.Background(), CompletionRequest{Model: "m"})
	if err != nil {
		t.Fatalf("Stream 报错: %v", err)
	}
	for chunk := range ch {
		if chunk.FinishReason != "" {
			t.Fatalf("上游没报结束原因，分片却带了 %q", chunk.FinishReason)
		}
	}
}

// TestClientStreamContextCancel 确认 ctx 取消后流终止且 goroutine 不泄漏。
//
// 真实的 http.Client 在 ctx 取消时会关闭响应体，因此这里的 fake 也用
// req.Context() 关闭阻塞 reader —— 否则测的是 fake 的行为而不是生产代码的。
func TestClientStreamContextCancel(t *testing.T) {
	doer := &fakeDoer{handler: func(_ int, req *http.Request) (*http.Response, error) {
		br := &blockingReader{ch: make(chan struct{})}
		go func() {
			<-req.Context().Done()
			_ = br.Close()
		}()
		return &http.Response{
			StatusCode: 200,
			Body:       br,
			Header:     make(http.Header),
		}, nil
	}}
	c := newTestClient(t, doer)

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := c.Stream(ctx, CompletionRequest{Model: "m"})
	if err != nil {
		t.Fatalf("Stream 报错: %v", err)
	}

	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	done := make(chan struct{})
	go func() {
		for range ch {
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ctx 取消后流应终止（可能有 goroutine 泄漏）")
	}
}

// TestNewClientRequiresSecrets 确认缺少密钥解析器时构造失败（防止明文 key 硬编码路径）。
func TestNewClientRequiresSecrets(t *testing.T) {
	_, err := NewClient(ClientOptions{Config: ProviderConfig{BaseURL: "https://x"}})
	if err == nil {
		t.Fatal("缺少 SecretResolver 应构造失败")
	}
}

// TestNewClientRequiresBaseURL 确认缺少 BaseURL 时构造失败。
func TestNewClientRequiresBaseURL(t *testing.T) {
	_, err := NewClient(ClientOptions{Secrets: staticSecrets("sk-x")})
	if err == nil {
		t.Fatal("缺少 BaseURL 应构造失败")
	}
}

// --- 测试用 reader/error ---

// netTimeoutErr 模拟可重试的连接超时。
type netTimeoutErr struct{}

func (e *netTimeoutErr) Error() string   { return "dial tcp: i/o timeout" }
func (e *netTimeoutErr) Timeout() bool   { return true }
func (e *netTimeoutErr) Temporary() bool { return true }

// errAfterReader 先返回数据，然后返回错误（模拟流中断）。
type errAfterReader struct {
	data []byte
	done bool
}

func (r *errAfterReader) Read(p []byte) (int, error) {
	if !r.done && len(r.data) > 0 {
		n := copy(p, r.data)
		r.data = r.data[n:]
		if len(r.data) == 0 {
			r.done = true
		}
		return n, nil
	}
	return 0, fmt.Errorf("connection reset by peer")
}

func (r *errAfterReader) Close() error { return nil }

// blockingReader 阻塞 Read 直到 Close 被调用（模拟服务端不发数据的连接）。
type blockingReader struct {
	ch       chan struct{}
	closeOne sync.Once
}

func (r *blockingReader) Read(p []byte) (int, error) {
	if r.ch == nil {
		r.ch = make(chan struct{})
	}
	<-r.ch
	return 0, io.EOF
}

// Close 幂等 —— 真实的 http 响应体也允许重复关闭（客户端与 ctx 都可能触发）。
func (r *blockingReader) Close() error {
	r.closeOne.Do(func() {
		if r.ch != nil {
			close(r.ch)
		}
	})
	return nil
}
