package provider

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"
)

// TestClassifyErrorTable 逐条验证第 18 章错误分类表。
//
// 这是本模块最重要的测试：分类错了会导致「400 被重试」或「429 不重试」这类线上事故。
func TestClassifyErrorTable(t *testing.T) {
	cases := []struct {
		name  string
		err   error
		class ErrorClass
		retry bool
	}{
		// ── 可重试 ──
		{"DNS 临时失败", &net.DNSError{Err: "server misbehaving", IsTemporary: true}, ClassDNS, true},
		{"connect timeout", &APIError{StatusCode: 0, Message: "connect timeout"}, ClassConnectTimeout, true},
		{"context deadline", context.DeadlineExceeded, ClassConnectTimeout, true},
		{"429", &APIError{StatusCode: 429, Message: "rate limit exceeded"}, ClassRateLimit, true},
		{"500", &APIError{StatusCode: 500, Message: "internal server error"}, ClassServerError, true},
		{"502", &APIError{StatusCode: 502, Message: "bad gateway"}, ClassServerError, true},
		{"503", &APIError{StatusCode: 503, Message: "service unavailable"}, ClassServerError, true},
		{"504", &APIError{StatusCode: 504, Message: "gateway timeout"}, ClassServerError, true},

		// ── 不可重试 ──
		{"400", &APIError{StatusCode: 400, Message: "bad request"}, ClassBadRequest, false},
		{"invalid API key (401)", &APIError{StatusCode: 401, Message: "invalid api key"}, ClassAuthInvalidKey, false},
		{"invalid API key (403)", &APIError{StatusCode: 403, Message: "forbidden"}, ClassAuthInvalidKey, false},
		{"invalid tool schema", &APIError{StatusCode: 400, Message: "invalid tool schema for function foo"}, ClassInvalidToolSchema, false},
		{"context too long", &APIError{StatusCode: 400, Message: "context length exceeded"}, ClassContextTooLong, false},
		{"user cancelled", context.Canceled, ClassUserCancelled, false},
		{"tool side effect", ErrToolSideEffectCommitted, ClassToolSideEffect, false},
		{"unknown", errors.New("something weird happened"), ClassUnknown, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(tc.err)
			if got.Class != tc.class {
				t.Errorf("Classify(%v) = %v, want %v", tc.err, got.Class, tc.class)
			}
			if r := Retryable(got.Class); r != tc.retry {
				t.Errorf("Retryable(%v) = %v, want %v", got.Class, r, tc.retry)
			}
		})
	}
}

// TestClassifyContextTooLongBeatsBadRequest 确认 400 的细分优先级：
// context too long 必须先于一般 400 被识别，否则上层的 compact 触发就失效了。
func TestClassifyContextTooLongBeatsBadRequest(t *testing.T) {
	err := &APIError{
		StatusCode: 400,
		Code:       "invalid_request_error",
		Message:    "This model's maximum context length is 65536 tokens, however your messages resulted in 70000",
	}
	if got := Classify(err); got.Class != ClassContextTooLong {
		t.Fatalf("expected ClassContextTooLong, got %v", got.Class)
	}
}

// TestClassifyInvalidToolSchemaBeatsBadRequest 确认 tool schema 细分。
func TestClassifyInvalidToolSchemaBeatsBadRequest(t *testing.T) {
	err := &APIError{
		StatusCode: 400,
		Message:    "Invalid schema for function 'foo': required field missing in parameters",
	}
	if got := Classify(err); got.Class != ClassInvalidToolSchema {
		t.Fatalf("expected ClassInvalidToolSchema, got %v", got.Class)
	}
}

// TestClassifyWrappedErrors 确认 errors.Is/As 的包装能被穿透。
func TestClassifyWrappedErrors(t *testing.T) {
	wrapped := fmt.Errorf("调用失败: %w", context.Canceled)
	if got := Classify(wrapped); got.Class != ClassUserCancelled {
		t.Errorf("wrapped cancel: got %v, want ClassUserCancelled", got.Class)
	}

	wrappedAPI := fmt.Errorf("请求失败: %w", &APIError{StatusCode: 429, Message: "too many requests"})
	if got := Classify(wrappedAPI); got.Class != ClassRateLimit {
		t.Errorf("wrapped 429: got %v, want ClassRateLimit", got.Class)
	}
}

// TestNoRetryOnUnknownPreventsInfiniteLoop 确认未知错误不会被无限重试。
//
// 验收标准「Provider 断网/429/5xx 不造成死循环」的一部分：
// 未知错误必须保守地不重试，否则一个反复失败且无法分类的错误会打爆配额。
func TestNoRetryOnUnknownPreventsInfiniteLoop(t *testing.T) {
	if Retryable(ClassUnknown) {
		t.Fatal("ClassUnknown 必须不可重试，否则可能死循环")
	}
	for _, c := range []ErrorClass{ClassBadRequest, ClassAuthInvalidKey, ClassInvalidToolSchema, ClassContextTooLong, ClassUserCancelled, ClassToolSideEffect} {
		if Retryable(c) {
			t.Errorf("%v 必须不可重试", c)
		}
	}
}

// TestRetryAfterParsed 确认 429 的 Retry-After 被正确保留。
func TestRetryAfterParsed(t *testing.T) {
	err := &APIError{
		StatusCode:    429,
		Message:       "rate limited",
		RetryAfter:    12 * time.Second,
		HasRetryAfter: true,
	}
	ce := Classify(err)
	if ce.Class != ClassRateLimit {
		t.Fatalf("got %v", ce.Class)
	}
	if ce.RetryAfter != 12*time.Second {
		t.Fatalf("RetryAfter = %v, want 12s", ce.RetryAfter)
	}
}

// TestRetryDelayHonorsRetryAfter 确认退避遵守 Retry-After 而非指数退避。
func TestRetryDelayHonorsRetryAfter(t *testing.T) {
	p := DefaultRetryPolicy()
	ce := &ClassifiedError{Class: ClassRateLimit, RetryAfter: 3 * time.Second}
	if d := p.retryDelay(1, ce); d != 3*time.Second {
		t.Fatalf("retryDelay = %v, want 3s (Retry-After)", d)
	}
}

// TestRetryDelayCapsHugeRetryAfter 确认过长的 Retry-After 会被封顶。
func TestRetryDelayCapsHugeRetryAfter(t *testing.T) {
	p := DefaultRetryPolicy()
	p.MaxRetryAfter = 60 * time.Second
	ce := &ClassifiedError{Class: ClassRateLimit, RetryAfter: 10 * time.Minute}
	if d := p.retryDelay(1, ce); d != 60*time.Second {
		t.Fatalf("retryDelay = %v, want 60s (capped)", d)
	}
	// 且应当放弃重试。
	if !p.ShouldGiveUpOnRetryAfter(ce) {
		t.Fatal("超过 MaxRetryAfter 时应放弃重试")
	}
}

// TestRetryDelayExponentialAndBounded 确认指数退避且不超过上限。
func TestRetryDelayExponentialAndBounded(t *testing.T) {
	p := DefaultRetryPolicy()
	ce := &ClassifiedError{Class: ClassServerError}

	for attempt := 1; attempt <= 6; attempt++ {
		d := p.retryDelay(attempt, ce)
		if d <= 0 {
			t.Fatalf("attempt %d: delay %v 必须为正", attempt, d)
		}
		if d > p.MaxDelay {
			t.Fatalf("attempt %d: delay %v 超过上限 %v", attempt, d, p.MaxDelay)
		}
	}
}

// TestBackoffRespectsContext 确认退避期间 ctx 取消能立即返回（不死等）。
func TestBackoffRespectsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	err := backoff(ctx, 10*time.Second)
	if err == nil {
		t.Fatal("期望 ctx 取消错误")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("ctx 取消后应立刻返回，实际等了 %v", elapsed)
	}
}

// TestBuildRequestBodyCapabilityGating 确认能力门控逐字段生效。
func TestBuildRequestBodyCapabilityGating(t *testing.T) {
	msgs := []Message{{
		Role:             RoleAssistant,
		Content:          "hi",
		ReasoningContent: "thinking...",
	}}

	// 全开：应包含 reasoning_content / stream_options / enable_thinking。
	full := BuildRequestBody("m", msgs, nil, true, EffortHigh, 0.5, 1000, DefaultCapabilities())
	if _, ok := full["stream_options"]; !ok {
		t.Error("sendStreamUsage=true 时应带 stream_options")
	}
	if full["enable_thinking"] != true {
		t.Error("thinking 模式应带 enable_thinking")
	}
	entry := full["messages"].([]map[string]any)[0]
	if entry["reasoning_content"] != "thinking..." {
		t.Error("sendReasoningParams=true 时应保留 reasoning_content")
	}

	// 全关：应剥离 reasoning_content，且不发 stream_options / enable_thinking。
	off := BuildRequestBody("m", msgs, nil, true, EffortHigh, 0.5, 1000, Capabilities{})
	if _, ok := off["stream_options"]; ok {
		t.Error("sendStreamUsage=false 时不应带 stream_options")
	}
	if _, ok := off["enable_thinking"]; ok {
		t.Error("sendReasoningParams=false 时不应带 enable_thinking")
	}
	entryOff := off["messages"].([]map[string]any)[0]
	if _, ok := entryOff["reasoning_content"]; ok {
		t.Error("sendReasoningParams=false 时应剥离 reasoning_content")
	}
	// 剥离 reasoning 后应退回 temperature。
	if off["temperature"] != 0.5 {
		t.Error("非 reasoning 时应发 temperature")
	}
}

// TestSanitizeContentRemovesInvisibleChars 确认净化的字符范围（v1 同款）。
func TestSanitizeContentRemovesInvisibleChars(t *testing.T) {
	input := "hello\x00\x01\x08world\x0b\x0c\x0e\x1f\x7f!"
	want := "helloworld!"
	if got := SanitizeContent(input); got != want {
		t.Fatalf("SanitizeContent = %q, want %q", got, want)
	}
	// \n \r \t 必须保留。
	keep := "a\nb\r\tc"
	if got := SanitizeContent(keep); got != keep {
		t.Fatalf("换行/制表符应保留: got %q", got)
	}
}

// TestClampMaxTokens 确认 max_tokens 防护边界。
func TestClampMaxTokens(t *testing.T) {
	if got := ClampMaxTokens(0); got != 1 {
		t.Errorf("0 → %d, want 1", got)
	}
	if got := ClampMaxTokens(-5); got != 1 {
		t.Errorf("-5 → %d, want 1", got)
	}
	if got := ClampMaxTokens(500_000); got != apiMaxOutputCeiling {
		t.Errorf("500000 → %d, want %d", got, apiMaxOutputCeiling)
	}
	if got := ClampMaxTokens(8192); got != 8192 {
		t.Errorf("8192 → %d, want 8192", got)
	}
}

// TestNormalizeUsageDerivesMiss 确认 cache 字段双形态归一（v1 normaliseUsage 对等）。
func TestNormalizeUsageDerivesMiss(t *testing.T) {
	// DeepSeek 形态：顶层 hit/miss。
	u := NormalizeUsage(100, 20, 120, 80, 20, 0, 0)
	if u.CacheHitTokens != 80 || u.CacheMissTokens != 20 {
		t.Fatalf("DeepSeek 形态: hit=%d miss=%d", u.CacheHitTokens, u.CacheMissTokens)
	}

	// OpenAI 形态：嵌套 cached_tokens，miss 需派生。
	u2 := NormalizeUsage(100, 20, 120, 0, 0, 90, 0)
	if u2.CacheHitTokens != 90 {
		t.Fatalf("OpenAI 形态 hit=%d, want 90", u2.CacheHitTokens)
	}
	if u2.CacheMissTokens != 10 {
		t.Fatalf("派生的 miss=%d, want 10 (100-90)", u2.CacheMissTokens)
	}
	if u2.CacheHitTokens+u2.CacheMissTokens != u2.PromptTokens {
		t.Fatal("hit+miss 必须等于 prompt")
	}
}

// TestToAPIEffort 确认 ultra → max 映射。
func TestToAPIEffort(t *testing.T) {
	if got := ToAPIEffort(EffortUltra); got != EffortMax {
		t.Errorf("ultra → %v, want max", got)
	}
	if got := ToAPIEffort(EffortHigh); got != EffortHigh {
		t.Errorf("high → %v, want high", got)
	}
}

// TestResolveActiveProviderFallback 确认自定义服务商缺失时回退内置（v1 同源语义）。
func TestResolveActiveProviderFallback(t *testing.T) {
	src := fakeConfigSource{
		active: "nonexistent",
		builtin: ProviderConfig{
			BaseURL:         "https://api.deepseek.com",
			ContextWindow:   1000000,
			MaxOutputTokens: 8192,
		},
	}
	got, err := ResolveActiveProvider(src, "")
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if got.ID != BuiltinProviderID {
		t.Fatalf("ID = %q, want %q", got.ID, BuiltinProviderID)
	}
	if !got.IsDeepSeek {
		t.Fatal("应标记为内置 DeepSeek")
	}
	if !got.Capabilities.SendReasoningParams || !got.Capabilities.SendStreamUsage {
		t.Fatal("内置服务商能力应全开")
	}
}

// TestResolveActiveProviderCustom 确认自定义服务商解析与缺省补齐。
func TestResolveActiveProviderCustom(t *testing.T) {
	src := fakeConfigSource{
		active:  "custom",
		builtin: ProviderConfig{BaseURL: "https://api.deepseek.com"},
		providers: map[string]ProviderConfig{
			"custom": {ID: "custom", Name: "MyProvider", BaseURL: "https://x.example.com"},
		},
	}
	got, err := ResolveActiveProvider(src, "custom")
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if got.IsDeepSeek {
		t.Fatal("自定义服务商不应标记为 DeepSeek")
	}
	if got.ContextWindow != defaultCustomContextWindow {
		t.Errorf("ContextWindow = %d, want %d", got.ContextWindow, defaultCustomContextWindow)
	}
	if got.MaxOutputTokens != defaultCustomMaxOutput {
		t.Errorf("MaxOutputTokens = %d, want %d", got.MaxOutputTokens, defaultCustomMaxOutput)
	}
}

type fakeConfigSource struct {
	active    string
	builtin   ProviderConfig
	providers map[string]ProviderConfig
}

func (f fakeConfigSource) ActiveProviderID() string { return f.active }
func (f fakeConfigSource) Provider(id string) (ProviderConfig, bool) {
	p, ok := f.providers[id]
	return p, ok
}
func (f fakeConfigSource) Builtin() ProviderConfig { return f.builtin }
