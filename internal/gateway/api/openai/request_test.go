package openai

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/ximo888ok-netizen/ximo-agent/internal/provider"
)

// testTool 构造一个最小工具定义（chatTool 的匿名函数结构由本 helper 收口）。
func testTool(name string) chatTool {
	var tool chatTool
	tool.Type = "function"
	tool.Function.Name = name
	tool.Function.Description = name + " desc"
	return tool
}

// TestRequestValidation 覆盖入参校验：能转发的放行，不能转发的显式 400（不静默丢弃）。
func TestRequestValidation(t *testing.T) {
	cases := []struct {
		name string
		body string
		code string // 期望的错误码；空表示期望成功
	}{
		{
			name: "缺少 model",
			body: `{"messages":[{"role":"user","content":"hi"}]}`,
			code: "invalid_request_error",
		},
		{
			name: "messages 为空",
			body: `{"model":"m1","messages":[]}`,
			code: "invalid_request_error",
		},
		{
			name: "未知角色",
			body: `{"model":"m1","messages":[{"role":"robot","content":"hi"}]}`,
			code: "invalid_request_error",
		},
		{
			name: "temperature 越界",
			body: `{"model":"m1","messages":[{"role":"user","content":"hi"}],"temperature":3}`,
			code: "invalid_request_error",
		},
		{
			name: "max_tokens 为负",
			body: `{"model":"m1","messages":[{"role":"user","content":"hi"}],"max_tokens":-1}`,
			code: "invalid_request_error",
		},
		{
			name: "n=2",
			body: `{"model":"m1","messages":[{"role":"user","content":"hi"}],"n":2}`,
			code: "unsupported_parameter",
		},
		{
			name: "stop",
			body: `{"model":"m1","messages":[{"role":"user","content":"hi"}],"stop":["x"]}`,
			code: "unsupported_parameter",
		},
		{
			name: "logprobs",
			body: `{"model":"m1","messages":[{"role":"user","content":"hi"}],"logprobs":true}`,
			code: "unsupported_parameter",
		},
		{
			name: "response_format",
			body: `{"model":"m1","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_object"}}`,
			code: "unsupported_parameter",
		},
		{
			name: "图像内容块",
			body: `{"model":"m1","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"http://x/y.png"}}]}]}`,
			code: "unsupported_parameter",
		},
		{
			name: "tool_choice=required",
			body: `{"model":"m1","messages":[{"role":"user","content":"hi"}],"tool_choice":"required"}`,
			code: "unsupported_parameter",
		},
		{
			name: "tool 类型非 function",
			body: `{"model":"m1","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"computer_use"}]}`,
			code: "unsupported_parameter",
		},
		{
			name: "developer 角色 + 内容块 + 工具（合法，应放行）",
			body: `{"model":"m1","messages":[{"role":"developer","content":[{"type":"text","text":"be brief"}]},{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object","properties":{}}}}],"tool_choice":"auto","max_completion_tokens":64}`,
		},
		{
			name: "content 为 null 的 assistant 工具调用轮次",
			body: `{"model":"m1","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},{"role":"tool","tool_call_id":"c1","content":"42"}]}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			srv, _ := startUpstream(t, http.StatusOK, upstreamChatSSE("ok", `{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}`))
			e.up.endpoints["p1"] = srv.URL
			e.allowModel(t, "m1", "p1")

			rec := e.post(t, tc.body)
			if tc.code == "" {
				if rec.Code != http.StatusOK {
					t.Fatalf("期望 200，实际 %d，body=%s", rec.Code, rec.Body.String())
				}
				return
			}
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("期望 400，实际 %d，body=%s", rec.Code, rec.Body.String())
			}
			if got := decodeError(t, rec).Error.Code; got != tc.code {
				t.Fatalf("错误码 = %q，期望 %q", got, tc.code)
			}
			if held, _, _ := e.quota.counts(); held != 0 {
				t.Fatalf("校验失败不应预占额度")
			}
		})
	}
}

func TestNormalizeDefaultsTemperatureToOpenAIDefault(t *testing.T) {
	norm, err := normalizeChatRequest(chatRequest{
		Model:    "m1",
		Messages: []chatMessage{{Role: "user", Content: []byte(`"hi"`)}},
	}, testReqID)
	if err != nil {
		t.Fatalf("校验失败：%v", err)
	}
	if norm.Temperature != 1 {
		t.Fatalf("temperature 缺省 = %v，期望 OpenAI 默认值 1（0 会让上游变成确定性采样）", norm.Temperature)
	}
	if norm.MaxTokens != 0 {
		t.Fatalf("max_tokens 缺省 = %d，期望 0（交由 provider 取缺省）", norm.MaxTokens)
	}
	if norm.InputTokensEstimate <= 0 {
		t.Fatalf("输入 token 粗估应大于 0，实际 %d", norm.InputTokensEstimate)
	}
}

func TestNormalizeToolChoiceNoneDropsTools(t *testing.T) {
	norm, err := normalizeChatRequest(chatRequest{
		Model:      "m1",
		Messages:   []chatMessage{{Role: "user", Content: []byte(`"hi"`)}},
		Tools:      []chatTool{testTool("f")},
		ToolChoice: []byte(`"none"`),
	}, testReqID)
	if err != nil {
		t.Fatalf("校验失败：%v", err)
	}
	if len(norm.Tools) != 0 {
		t.Fatalf("tool_choice=none 应丢弃工具，实际 %d 个", len(norm.Tools))
	}
}

func TestNormalizeToolPadsEmptySchema(t *testing.T) {
	norm, err := normalizeChatRequest(chatRequest{
		Model:    "m1",
		Messages: []chatMessage{{Role: "user", Content: []byte(`"hi"`)}},
		Tools:    []chatTool{testTool("f")},
	}, testReqID)
	if err != nil {
		t.Fatalf("校验失败：%v", err)
	}
	if len(norm.Tools) != 1 {
		t.Fatalf("工具数 = %d，期望 1", len(norm.Tools))
	}
	params := norm.Tools[0].Parameters
	if params["type"] != "object" {
		t.Fatalf("空 schema 应补 type=object，实际 %+v", params)
	}
	if _, ok := params["properties"]; !ok {
		t.Fatalf("空 schema 应补 properties，实际 %+v", params)
	}
}

func TestSanitizeUpstreamMessageHidesURLsAndSecrets(t *testing.T) {
	in := "invalid request to https://api.example.com/v1 with Bearer sk-test-0123456789abcdefghij"
	got := sanitizeUpstreamMessage(in)
	for _, bad := range []string{"api.example.com", "sk-test-0123456789abcdefghij"} {
		if strings.Contains(got, bad) {
			t.Fatalf("清洗后仍含敏感片段 %q：%s", bad, got)
		}
	}
}

func TestDescribeUpstreamFailureMapping(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"上游 401 不外泄成 401", &provider.APIError{StatusCode: 401, Message: "bad key"}, 502, "upstream_auth_failed"},
		{"上游 400 透传为客户端错误", &provider.APIError{StatusCode: 400, Message: "bad payload"}, 400, "invalid_request_error"},
		{"上游 429 透传限流", &provider.APIError{StatusCode: 429, Message: "slow down"}, 429, "rate_limit_exceeded"},
		{"上游 500 归为不可用", &provider.APIError{StatusCode: 500, Message: "boom"}, 502, "provider_unavailable"},
		{"非 HTTP 错误归为不可用", errors.New("connection reset"), 502, "provider_unavailable"},
		{"上游 400 文案被清洗", &provider.APIError{StatusCode: 400, Message: "bad https://leak.example.com/x"}, 400, "invalid_request_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := describeUpstreamFailure(tc.err)
			if got.Status != tc.status || got.Code != tc.code {
				t.Fatalf("映射 = (%d,%s)，期望 (%d,%s)", got.Status, got.Code, tc.status, tc.code)
			}
			if strings.Contains(got.Message, "leak.example.com") {
				t.Fatalf("上游文案里的 URL 未清洗：%s", got.Message)
			}
		})
	}
}

func TestOutcomeForVocabulary(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if got := outcomeFor(canceled, errors.New("boom")); got != OutcomeClientCanceled {
		t.Fatalf("上下文取消 → %q，期望 %s", got, OutcomeClientCanceled)
	}
	if got := outcomeFor(t.Context(), context.DeadlineExceeded); got != OutcomeUpstreamTimeout {
		t.Fatalf("超时类错误 → %q，期望 %s", got, OutcomeUpstreamTimeout)
	}
	if got := outcomeFor(t.Context(), &provider.APIError{StatusCode: 500, Message: "boom"}); got != OutcomeUpstreamError {
		t.Fatalf("普通上游错误 → %q，期望 %s", got, OutcomeUpstreamError)
	}
}

// TestUsageFromMissingReportsZeros 守住「上游没报 usage 就如实记 0」的口径（§6.1）。
func TestUsageFromMissingReportsZeros(t *testing.T) {
	if got := usageFrom(nil); got != (usageBody{}) {
		t.Fatalf("usageFrom(nil) = %+v，期望全 0", got)
	}
	got := usageFrom(&provider.TokenUsage{PromptTokens: 3, CompletionTokens: 4})
	if got.TotalTokens != 7 {
		t.Fatalf("total_tokens 应由 prompt+completion 派生，实际 %d", got.TotalTokens)
	}
}
