package openai

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// sseLines 从 SSE 响应体里取出所有 data 行（保持顺序）。
func sseLines(t *testing.T, body string) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "data: ") {
			out = append(out, strings.TrimPrefix(line, "data: "))
		}
	}
	return out
}

func TestStreamSuccessOrderAndDone(t *testing.T) {
	e := newEnv(t)
	srv, upstream := startUpstream(t, http.StatusOK, []string{
		sseData(`{"id":"c","choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"},"finish_reason":null}]}`),
		sseData(`{"id":"c","choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":null}]}`),
		sseData(`{"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`),
		sseData("[DONE]"),
	})
	e.up.endpoints["p1"] = srv.URL
	e.allowModel(t, "m1", "p1")

	rec := e.post(t, `{"model":"m1","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200，body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q，期望 text/event-stream", ct)
	}
	lines := sseLines(t, rec.Body.String())
	if len(lines) != 4 {
		t.Fatalf("data 行数 = %d，期望 4（两个增量 + finish + [DONE]）：%v", len(lines), lines)
	}
	if lines[3] != "[DONE]" {
		t.Fatalf("最后一行 = %q，期望 [DONE]", lines[3])
	}
	var first, second streamChunkBody
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("第一个分片不是合法 JSON：%v", err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatalf("第二个分片不是合法 JSON：%v", err)
	}
	if first.Choices[0].Delta.Content != "Hel" || first.Choices[0].Delta.Role != "assistant" ||
		first.Choices[0].FinishReason != nil {
		t.Fatalf("第一个分片不符：%+v", first.Choices[0])
	}
	if second.Choices[0].Delta.Content != "lo" || second.Choices[0].Delta.Role != "" {
		t.Fatalf("第二个分片不符：%+v", second.Choices[0])
	}
	// finish 分片：finish_reason=stop，delta 为空对象。
	var finish streamChunkBody
	if err := json.Unmarshal([]byte(lines[2]), &finish); err != nil {
		t.Fatalf("finish 分片不是合法 JSON：%v", err)
	}
	if finish.Choices[0].FinishReason == nil || *finish.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish_reason = %v，期望 stop", finish.Choices[0].FinishReason)
	}
	// 对外模型名必须是客户端请求的模型名，不能是上游模型名。
	if first.Model != "m1" || first.Object != "chat.completion.chunk" || first.ID != completionID(testReqID) {
		t.Fatalf("分片元信息不符：%+v", first)
	}
	if strings.Contains(rec.Body.String(), "upstream-p1") || strings.Contains(rec.Body.String(), testAPIKey) {
		t.Fatalf("流式响应泄漏上游信息：%s", rec.Body.String())
	}
	if upstream.count() != 1 {
		t.Fatalf("上游命中 %d 次，期望 1", upstream.count())
	}
	if _, settled, released := e.quota.counts(); settled != 1 || released != 0 {
		t.Fatalf("流式成功必须结算：settled=%d released=%d", settled, released)
	}
	// 本用例的假上游整条流都没报 usage（既非「尾随被丢」，也不是网关编造）→ 如实记 0（§6.1）。
	got, ok := e.quota.settleRecord()
	if !ok {
		t.Fatal("缺少结算记录")
	}
	if got.InputTokens != 0 || got.OutputTokens != 0 || got.CostMicro != 0 {
		t.Fatalf("上游未上报 usage 时结算必须按 0：%+v", got)
	}
	if got.ProviderID != "p1" || got.ModelID != "m1" || got.Status != "settled" {
		t.Fatalf("结算记录不符：%+v", got)
	}
}

func TestStreamFirstByteFailureReturnsJSONError(t *testing.T) {
	e := newEnv(t)
	b1, _ := startUpstream(t, http.StatusInternalServerError, nil)
	b2, _ := startUpstream(t, http.StatusBadGateway, nil)
	e.up.endpoints["p1"] = b1.URL
	e.up.endpoints["p2"] = b2.URL
	e.allowModel(t, "m1", "p1", "p2")

	rec := e.post(t, `{"model":"m1","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("状态码 = %d，期望 502（首字节前失败回普通 JSON 错误）", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q，期望 application/json（不能是 SSE）", ct)
	}
	if code := decodeError(t, rec).Error.Code; code != "provider_unavailable" {
		t.Fatalf("错误码 = %q，期望 provider_unavailable", code)
	}
	held, settled, released := e.quota.counts()
	if held != 1 || settled != 0 || released != 1 {
		t.Fatalf("预占收尾错误：held=%d settled=%d released=%d，期望 1/0/1", held, settled, released)
	}
	row, ok := e.usage.last()
	if !ok || row.Status != OutcomeUpstreamError {
		t.Fatalf("首字节前失败也要落终态 usage 行：%+v", row)
	}
}

func TestStreamRetryableFailureBeforeFirstByteSwitchesCandidate(t *testing.T) {
	e := newEnv(t)
	bad, badRec := startUpstream(t, http.StatusServiceUnavailable, nil)
	good, _ := startUpstream(t, http.StatusOK, []string{
		sseData(`{"id":"c","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":null}]}`),
		sseData(`{"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`),
		sseData("[DONE]"),
	})
	e.up.endpoints["p1"] = bad.URL
	e.up.endpoints["p2"] = good.URL
	e.allowModel(t, "m1", "p1", "p2")

	rec := e.post(t, `{"model":"m1","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ok") {
		t.Fatalf("期望换候选后成功，状态码=%d body=%s", rec.Code, rec.Body.String())
	}
	if badRec.count() != 1 {
		t.Fatalf("第一个候选命中 %d 次，期望 1", badRec.count())
	}
	if got, _ := e.quota.settleRecord(); got.ProviderID != "p2" {
		t.Fatalf("结算 provider = %q，期望 p2", got.ProviderID)
	}
}

func TestStreamFailureAfterFirstByteEmitsErrorEventAndReleases(t *testing.T) {
	e := newEnv(t)
	// 先给一个内容增量（首字节已发出），紧接一条错误事件 —— provider 会把错误事件
	// 翻成 APIError 并在已有输出后终止，正好对应「首字节后失败」。
	srv, _ := startUpstream(t, http.StatusOK, []string{
		sseData(`{"id":"c","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}`),
		sseData(`{"error":{"message":"boom","type":"server_error"}}`),
	})
	e.up.endpoints["p1"] = srv.URL
	e.allowModel(t, "m1", "p1")

	rec := e.post(t, `{"model":"m1","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（首字节已发出，无法再改状态码）", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "partial") {
		t.Fatalf("已发出的增量不该被吞掉：%s", body)
	}
	if !strings.Contains(body, `"code":"invalid_request_error"`) && !strings.Contains(body, `"error"`) {
		t.Fatalf("首字节后失败必须发错误事件：%s", body)
	}
	if strings.Contains(body, "[DONE]") {
		t.Fatalf("中途失败不得发 [DONE]（会与正常收尾混淆）：%s", body)
	}
	held, settled, released := e.quota.counts()
	if held != 1 || settled != 0 || released != 1 {
		t.Fatalf("预占收尾错误：held=%d settled=%d released=%d，期望 1/0/1", held, settled, released)
	}
	row, ok := e.usage.last()
	if !ok {
		t.Fatal("流式失败必须落 usage 行")
	}
	if row.Status != OutcomeUpstreamError || row.ProviderID != "p1" || row.CostMicro != 0 {
		t.Fatalf("终态 usage 行不符：%+v", row)
	}
}

func TestStreamClientCancelReleasesReservation(t *testing.T) {
	e := newEnv(t)
	srv, _ := startHangingUpstream(t, []string{
		sseData(`{"id":"c","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}`),
	})
	e.up.endpoints["p1"] = srv.URL
	e.allowModel(t, "m1", "p1")

	httpSrv := httptest.NewServer(e.mux)
	defer httpSrv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, httpSrv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"m1","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("构造请求失败：%v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败：%v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// 读到第一个增量分片（确认首字节已到客户端），然后断开。
	reader := bufio.NewReader(resp.Body)
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("读取首字节失败：%v", err)
	}
	if !strings.Contains(line, "partial") {
		t.Fatalf("首个分片不符：%q", line)
	}
	cancel()

	// 处理器在另一个 goroutine 里收尾：等它归还预占。
	waitFor(t, "客户端取消后归还预占", func() bool {
		_, _, released := e.quota.counts()
		return released == 1
	})
	if _, settled, _ := e.quota.counts(); settled != 0 {
		t.Fatalf("客户端取消不应结算")
	}
	waitFor(t, "写入 client_canceled 终态", func() bool {
		row, ok := e.usage.last()
		return ok && row.Status == OutcomeClientCanceled
	})
	row, _ := e.usage.last()
	if row.ProviderID != "p1" || row.ModelID != "m1" {
		t.Fatalf("终态 usage 行不符：%+v", row)
	}
}

func TestStreamReasoningContentAndToolCallsAreForwarded(t *testing.T) {
	e := newEnv(t)
	srv, _ := startUpstream(t, http.StatusOK, []string{
		sseData(`{"id":"c","choices":[{"index":0,"delta":{"reasoning_content":"think"},"finish_reason":null}]}`),
		sseData(`{"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"weather","arguments":"{}"}}]},"finish_reason":null}]}`),
		sseData(`{"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`),
		sseData("[DONE]"),
	})
	e.up.endpoints["p1"] = srv.URL
	e.allowModel(t, "m1", "p1")

	rec := e.post(t, `{"model":"m1","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", rec.Code)
	}
	lines := sseLines(t, rec.Body.String())
	if len(lines) < 4 {
		t.Fatalf("data 行数不足：%v", lines)
	}
	var reasoning, tools streamChunkBody
	if err := json.Unmarshal([]byte(lines[0]), &reasoning); err != nil {
		t.Fatalf("分片解析失败：%v", err)
	}
	if reasoning.Choices[0].Delta.ReasoningContent != "think" {
		t.Fatalf("reasoning_content 未透传：%+v", reasoning.Choices[0].Delta)
	}
	if err := json.Unmarshal([]byte(lines[1]), &tools); err != nil {
		t.Fatalf("分片解析失败：%v", err)
	}
	tc := tools.Choices[0].Delta.ToolCalls
	if len(tc) != 1 || tc[0].Index != 0 || tc[0].ID != "call_1" || tc[0].Type != "function" ||
		tc[0].Function == nil || tc[0].Function.Name != "weather" {
		t.Fatalf("工具调用增量未透传：%+v", tc)
	}
	// 流式分片不带上游 finish_reason，出现过工具调用时收尾按 tool_calls 推断。
	// 上游的 finish_reason 分片本身不会被转发（provider 的聚合器在它那里收尾），
	// 网关自己的收尾分片是倒数第二行。
	if got := lines[len(lines)-1]; got != "[DONE]" {
		t.Fatalf("最后一行 = %q，期望 [DONE]", got)
	}
	var finish streamChunkBody
	if err := json.Unmarshal([]byte(lines[len(lines)-2]), &finish); err != nil {
		t.Fatalf("finish 分片解析失败：%v", err)
	}
	if finish.Choices[0].FinishReason == nil || *finish.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish_reason = %v，期望 tool_calls", finish.Choices[0].FinishReason)
	}
}

// finishReasonOf 从 SSE 体里取出收尾分片的 finish_reason（跳过 delta 与 usage 分片），
// 并顺带校验 [DONE] 仍是最后一行。
func finishReasonOf(t *testing.T, body string) string {
	t.Helper()
	lines := sseLines(t, body)
	if len(lines) == 0 || lines[len(lines)-1] != "[DONE]" {
		t.Fatalf("SSE 必须以 [DONE] 收尾：%v", lines)
	}
	for i := len(lines) - 1; i >= 0; i-- {
		if lines[i] == "[DONE]" {
			continue
		}
		var chunk streamChunkBody
		if err := json.Unmarshal([]byte(lines[i]), &chunk); err != nil {
			t.Fatalf("分片不是合法 JSON：%v (%q)", err, lines[i])
		}
		if len(chunk.Choices) == 0 {
			continue // usage 分片
		}
		if chunk.Choices[0].FinishReason != nil {
			return *chunk.Choices[0].FinishReason
		}
	}
	t.Fatalf("收尾分片缺少 finish_reason：%s", body)
	return ""
}

// TestStreamFinishReasonPrefersUpstreamExplicitValue 断言上游显式报告的 finish_reason
// 直达响应，且不被本地推断覆盖。
func TestStreamFinishReasonPrefersUpstreamExplicitValue(t *testing.T) {
	toolDelta := sseData(`{"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"f","arguments":"{}"}}]},"finish_reason":null}]}`)

	cases := []struct {
		name   string
		events []string
		want   string
	}{
		{
			// 修复前：收尾一律写 stop，客户端会把「被 length 截断」当成正常结束。
			name: "上游报 length",
			events: []string{
				sseData(`{"id":"c","choices":[{"index":0,"delta":{"content":"cut"},"finish_reason":null}]}`),
				sseData(`{"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`),
				sseData("[DONE]"),
			},
			want: "length",
		},
		{
			name: "上游报 tool_calls",
			events: []string{
				toolDelta,
				sseData(`{"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`),
				sseData("[DONE]"),
			},
			want: "tool_calls",
		},
		{
			// 反向验证「优先用上游」：出现过工具调用增量，但上游明确说 stop —— 不得再推断成 tool_calls。
			name: "上游报 stop 且出现过工具调用",
			events: []string{
				toolDelta,
				sseData(`{"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`),
				sseData("[DONE]"),
			},
			want: "stop",
		},
		{
			// 无内容、只有 finish_reason 的流也要产出合法收尾（不能吞掉空回答）。
			name: "空回答只有 finish_reason",
			events: []string{
				sseData(`{"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"length"}],"usage":{"prompt_tokens":3,"completion_tokens":0,"total_tokens":3}}`),
				sseData("[DONE]"),
			},
			want: "length",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			srv, _ := startUpstream(t, http.StatusOK, tc.events)
			e.up.endpoints["p1"] = srv.URL
			e.allowModel(t, "m1", "p1")

			rec := e.post(t, `{"model":"m1","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
			if rec.Code != http.StatusOK {
				t.Fatalf("状态码 = %d，期望 200，body=%s", rec.Code, rec.Body.String())
			}
			if got := finishReasonOf(t, rec.Body.String()); got != tc.want {
				t.Fatalf("finish_reason = %q，期望与上游一致 %q", got, tc.want)
			}
		})
	}
}

// TestStreamFinishReasonFallsBackToInferenceWhenUpstreamSilent 是回归用例：上游只发
// delta + [DONE]、不报 finish_reason 时，仍走原有推断（出现过工具调用 → tool_calls）。
func TestStreamFinishReasonFallsBackToInferenceWhenUpstreamSilent(t *testing.T) {
	cases := []struct {
		name   string
		events []string
		want   string
	}{
		{
			name: "无工具调用",
			events: []string{
				sseData(`{"id":"c","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`),
				sseData("[DONE]"),
			},
			want: "stop",
		},
		{
			name: "出现过工具调用",
			events: []string{
				sseData(`{"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"f","arguments":"{}"}}]},"finish_reason":null}]}`),
				sseData("[DONE]"),
			},
			want: "tool_calls",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			srv, _ := startUpstream(t, http.StatusOK, tc.events)
			e.up.endpoints["p1"] = srv.URL
			e.allowModel(t, "m1", "p1")

			rec := e.post(t, `{"model":"m1","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
			if rec.Code != http.StatusOK {
				t.Fatalf("状态码 = %d，期望 200，body=%s", rec.Code, rec.Body.String())
			}
			if got := finishReasonOf(t, rec.Body.String()); got != tc.want {
				t.Fatalf("finish_reason = %q，期望按推断得 %q", got, tc.want)
			}
		})
	}
}

func TestStreamModelWithoutStreamCapabilityIsRejected(t *testing.T) {
	e := newEnv(t)
	srv, upstream := startUpstream(t, http.StatusOK, nil)
	e.up.endpoints["p1"] = srv.URL
	e.allowModel(t, "m1", "p1")
	spec := e.models.specs["m1"]
	spec.CapabilitiesJSON = `{"stream":false,"tools":true}`
	e.models.specs["m1"] = spec

	rec := e.post(t, `{"model":"m1","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400，body=%s", rec.Code, rec.Body.String())
	}
	if code := decodeError(t, rec).Error.Code; code != "unsupported_parameter" {
		t.Fatalf("错误码 = %q，期望 unsupported_parameter", code)
	}
	if upstream.count() != 0 {
		t.Fatalf("能力不符不应打上游")
	}
	if held, _, _ := e.quota.counts(); held != 0 {
		t.Fatalf("能力校验应在预占之前")
	}
}
