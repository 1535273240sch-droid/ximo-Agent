package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestBuildVisionBodyStructure 确认多模态请求体结构正确（content 为块数组）。
func TestBuildVisionBodyStructure(t *testing.T) {
	c := newTestClient(t, &fakeDoer{handler: func(int, *http.Request) (*http.Response, error) {
		return httpResponse(200, `{"choices":[{"message":{"content":"ok"}}]}`), nil
	}})

	body, err := c.buildVisionBody(VisionRequest{
		Model:  "agnes-2.5-flash",
		Prompt: "描述这张界面",
		Images: []string{"https://example.com/a.png", "data:image/png;base64,AAA"},
	})
	if err != nil {
		t.Fatalf("构造请求体失败: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("解析失败: %v", err)
	}

	// 非流式。
	if parsed["stream"] != false {
		t.Errorf("视觉请求应为非流式，实际 stream=%v", parsed["stream"])
	}

	messages := parsed["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("应有 system + user 两条消息，实际 %d", len(messages))
	}

	sys := messages[0].(map[string]any)
	if sys["role"] != "system" {
		t.Errorf("首条应为 system，实际 %v", sys["role"])
	}
	if !strings.Contains(sys["content"].(string), "视觉分析助手") {
		t.Error("system 应使用视觉分析提示词")
	}

	user := messages[1].(map[string]any)
	if user["role"] != "user" {
		t.Errorf("次条应为 user，实际 %v", user["role"])
	}

	parts := user["content"].([]any)
	// 1 个文本块 + 2 个图片块。
	if len(parts) != 3 {
		t.Fatalf("应有 3 个内容块，实际 %d", len(parts))
	}
	first := parts[0].(map[string]any)
	if first["type"] != "text" {
		t.Errorf("首块应为 text，实际 %v", first["type"])
	}
	second := parts[1].(map[string]any)
	if second["type"] != "image_url" {
		t.Errorf("次块应为 image_url，实际 %v", second["type"])
	}
	imgURL := second["image_url"].(map[string]any)
	if imgURL["url"] != "https://example.com/a.png" {
		t.Errorf("图片 URL 传递有误: %v", imgURL["url"])
	}
}

// TestBuildVisionBodyThinkingMode 确认思考模式与温度互斥（与文本 provider 同规则）。
func TestBuildVisionBodyThinkingMode(t *testing.T) {
	c := newTestClient(t, &fakeDoer{handler: func(int, *http.Request) (*http.Response, error) {
		return httpResponse(200, `{}`), nil
	}})

	thinking, _ := c.buildVisionBody(VisionRequest{Model: "m", Prompt: "p", Images: []string{"u"}, EnableThinking: true})
	var tb map[string]any
	_ = json.Unmarshal(thinking, &tb)
	if tb["enable_thinking"] != true {
		t.Error("思考模式应带 enable_thinking")
	}
	if _, hasTemp := tb["temperature"]; hasTemp {
		t.Error("思考模式不应带 temperature")
	}

	normal, _ := c.buildVisionBody(VisionRequest{Model: "m", Prompt: "p", Images: []string{"u"}})
	var nb map[string]any
	_ = json.Unmarshal(normal, &nb)
	if _, hasThinking := nb["enable_thinking"]; hasThinking {
		t.Error("非思考模式不应带 enable_thinking")
	}
	if nb["temperature"] != 0.3 {
		t.Errorf("非思考模式温度应为 0.3，实际 %v", nb["temperature"])
	}
}

// TestAnalyzeImagesSuccess 确认成功路径。
func TestAnalyzeImagesSuccess(t *testing.T) {
	doer := &fakeDoer{handler: func(int, *http.Request) (*http.Response, error) {
		return httpResponse(200, `{"choices":[{"message":{"content":"这是一个登录界面，包含用户名和密码输入框。"}}],"usage":{"prompt_tokens":100,"completion_tokens":30}}`), nil
	}}
	c := newTestClient(t, doer)

	resp, err := c.AnalyzeImages(context.Background(), VisionRequest{
		Model:  "vision-model",
		Prompt: "描述界面",
		Images: []string{"https://example.com/ui.png"},
	})
	if err != nil {
		t.Fatalf("报错: %v", err)
	}
	if !strings.Contains(resp.Content, "登录界面") {
		t.Fatalf("Content = %q", resp.Content)
	}
	if resp.Usage == nil || resp.Usage.PromptTokens != 100 {
		t.Fatalf("usage 解析有误: %+v", resp.Usage)
	}
	if resp.Attempts != 1 {
		t.Fatalf("Attempts = %d, want 1", resp.Attempts)
	}
}

// TestAnalyzeImagesContentBlockArray 确认兼容块数组形态的 content 返回。
//
// 部分多模态服务商把 content 返回为 [{"type":"text","text":"..."}]，
// 而不是纯字符串 —— 必须两种都能解析。
func TestAnalyzeImagesContentBlockArray(t *testing.T) {
	doer := &fakeDoer{handler: func(int, *http.Request) (*http.Response, error) {
		return httpResponse(200, `{"choices":[{"message":{"content":[{"type":"text","text":"第一段"},{"type":"image_url","image_url":{"url":"x"}},{"type":"text","text":"第二段"}]}}]}`), nil
	}}
	c := newTestClient(t, doer)

	resp, err := c.AnalyzeImages(context.Background(), VisionRequest{
		Model: "m", Prompt: "p", Images: []string{"u"},
	})
	if err != nil {
		t.Fatalf("报错: %v", err)
	}
	if resp.Content != "第一段\n第二段" {
		t.Fatalf("Content = %q, want 第一段\\n第二段（应只取 text 块）", resp.Content)
	}
}

// TestAnalyzeImagesRequiresImage 确认无图片时报错。
func TestAnalyzeImagesRequiresImage(t *testing.T) {
	c := newTestClient(t, &fakeDoer{})
	_, err := c.AnalyzeImages(context.Background(), VisionRequest{Model: "m", Prompt: "p"})
	if err == nil {
		t.Fatal("无图片应报错")
	}
}

// TestAnalyzeImagesRetriesEmptyResponse 确认空响应被重试（思考模式的「还在思考」）。
//
// v1 的 callVisionWithWait 对这种空响应会持续等待 —— v2 保留重试语义，
// 但把次数收敛到 RetryPolicy 上限，不再无限等。
func TestAnalyzeImagesRetriesEmptyResponse(t *testing.T) {
	doer := &fakeDoer{handler: func(call int, _ *http.Request) (*http.Response, error) {
		if call == 1 {
			// 首次：空 content（思考中）。
			return httpResponse(200, `{"choices":[{"message":{"content":""}}]}`), nil
		}
		return httpResponse(200, `{"choices":[{"message":{"content":"分析完成"}}]}`), nil
	}}
	c := newTestClient(t, doer)

	resp, err := c.AnalyzeImages(context.Background(), VisionRequest{
		Model: "m", Prompt: "p", Images: []string{"u"},
	})
	if err != nil {
		t.Fatalf("空响应后应重试成功: %v", err)
	}
	if resp.Content != "分析完成" {
		t.Fatalf("Content = %q", resp.Content)
	}
	if resp.Attempts < 2 {
		t.Fatalf("Attempts = %d, want ≥2（应重试过空响应）", resp.Attempts)
	}
}

// TestAnalyzeImagesEmptyResponseTerminates 确认持续空响应最终终止（不死循环）。
func TestAnalyzeImagesEmptyResponseTerminates(t *testing.T) {
	doer := &fakeDoer{handler: func(int, *http.Request) (*http.Response, error) {
		return httpResponse(200, `{"choices":[{"message":{"content":""}}]}`), nil
	}}
	c := newTestClient(t, doer)

	_, err := c.AnalyzeImages(context.Background(), VisionRequest{
		Model: "m", Prompt: "p", Images: []string{"u"},
	})
	if err == nil {
		t.Fatal("持续空响应应最终报错（不能无限等）")
	}
	if got := doer.calls.Load(); got != 3 {
		t.Fatalf("应按 MaxAttempts=3 终止，实际请求 %d 次", got)
	}
}

// TestAnalyzeImagesDoesNotRetry400 确认 400 不重试。
func TestAnalyzeImagesDoesNotRetry400(t *testing.T) {
	doer := &fakeDoer{handler: func(int, *http.Request) (*http.Response, error) {
		return httpResponse(400, `{"error":{"message":"invalid image url"}}`), nil
	}}
	c := newTestClient(t, doer)

	_, err := c.AnalyzeImages(context.Background(), VisionRequest{
		Model: "m", Prompt: "p", Images: []string{"u"},
	})
	if err == nil {
		t.Fatal("400 应报错")
	}
	if got := doer.calls.Load(); got != 1 {
		t.Fatalf("400 不应重试，实际 %d 次", got)
	}
}

// TestAnalyzeImagesNeverLeaksAPIKey 确认视觉路径同样不泄露密钥。
func TestAnalyzeImagesNeverLeaksAPIKey(t *testing.T) {
	const secret = "sk-vision-secret-key-abcdef"
	doer := &fakeDoer{handler: func(int, *http.Request) (*http.Response, error) {
		return httpResponse(500, `{"error":{"message":"boom"}}`), nil
	}}
	c := newTestClient(t, doer, func(o *ClientOptions) {
		o.Secrets = staticSecrets(secret)
	})

	_, err := c.AnalyzeImages(context.Background(), VisionRequest{
		Model: "m", Prompt: "p", Images: []string{"u"},
	})
	if err == nil {
		t.Fatal("应报错")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("错误泄露密钥: %v", err)
	}
}

// TestExtractContentText 确认三种 content 形态的提取。
func TestExtractContentText(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"字符串", `"纯文本内容"`, "纯文本内容"},
		{"块数组", `[{"type":"text","text":"A"},{"type":"text","text":"B"}]`, "A\nB"},
		{"块数组含非文本块", `[{"type":"text","text":"A"},{"type":"image_url"}]`, "A"},
		{"null", `null`, ""},
		{"空", ``, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractContentText(json.RawMessage(tc.raw)); got != tc.want {
				t.Errorf("extractContentText(%s) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// TestVisionFallbackUsesReasoning 确认正文为空时回退到 reasoning_content。
func TestVisionFallbackUsesReasoning(t *testing.T) {
	got := VisionFallback(VisionResponse{Content: "", ReasoningContent: "思考内容"})
	if got != "思考内容" {
		t.Fatalf("应回退到 reasoning，实际 %q", got)
	}
	got2 := VisionFallback(VisionResponse{Content: "正文", ReasoningContent: "思考"})
	if got2 != "正文" {
		t.Fatalf("有正文时应用正文，实际 %q", got2)
	}
}

// TestAnalyzeImagesRespectsBreaker 确认视觉路径也受熔断保护。
func TestAnalyzeImagesRespectsBreaker(t *testing.T) {
	breaker := NewCircuitBreaker(2, 60_000_000_000) // 60s
	doer := &fakeDoer{handler: func(int, *http.Request) (*http.Response, error) {
		return httpResponse(503, `{"error":{"message":"unavailable"}}`), nil
	}}
	c := newTestClient(t, doer, func(o *ClientOptions) { o.Breaker = breaker })

	_, _ = c.AnalyzeImages(context.Background(), VisionRequest{Model: "m", Prompt: "p", Images: []string{"u"}})
	if breaker.State() != BreakerOpen {
		t.Fatalf("应打开熔断，实际 %v", breaker.State())
	}

	callsBefore := doer.calls.Load()
	_, err := c.AnalyzeImages(context.Background(), VisionRequest{Model: "m", Prompt: "p", Images: []string{"u"}})
	if err == nil {
		t.Fatal("熔断后应报错")
	}
	if doer.calls.Load() != callsBefore {
		t.Fatal("熔断打开后视觉请求也不应打到网络")
	}
}

// TestParseVisionResponsePropagatesAPIError 确认响应体里的 error 被转成 APIError。
func TestParseVisionResponsePropagatesAPIError(t *testing.T) {
	_, err := parseVisionResponse(io.NopCloser(strings.NewReader(
		`{"error":{"message":"model does not support images","type":"invalid_request_error"}}`)))
	if err == nil {
		t.Fatal("应返回错误")
	}
	if !strings.Contains(err.Error(), "does not support images") {
		t.Fatalf("错误消息应保留原文: %v", err)
	}
}

// TestVisionSystemPromptContent 确认提示词包含 v1 的五条强制规则要点。
func TestVisionSystemPromptContent(t *testing.T) {
	for _, want := range []string{"完整覆盖", "结构化输出", "原文保留", "细节优先", "异常标注"} {
		if !strings.Contains(VisionSystemPrompt, want) {
			t.Errorf("视觉提示词缺少规则 %q", want)
		}
	}
}
