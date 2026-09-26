package openai

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/catalog"
	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/httpx"
	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/model"
	"github.com/ximo888ok-netizen/ximo-agent/internal/quota"
)

// upstreamChatSSE 是最小的 OpenAI 兼容流式响应，覆盖「内容 + 终止 + usage」三段。
// usage 与 finish_reason 放在同一分片：上游的常见形态之一。两者分开（usage 尾随在
// finish_reason 之后）的形态由 provider 的回归用例
// TestClientStreamKeepsTrailingUsageAfterFinish 覆盖，本文件不重复。
func upstreamChatSSE(content, usageJSON string) []string {
	return []string{
		sseData(`{"id":"chatcmpl-up","choices":[{"index":0,"delta":{"role":"assistant","content":"` + content + `"},"finish_reason":null}]}`),
		sseData(`{"id":"chatcmpl-up","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":` + usageJSON + `}`),
		sseData("[DONE]"),
	}
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) completionResponseBody {
	t.Helper()
	var body completionResponseBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON：%v，body=%s", err, rec.Body.String())
	}
	return body
}

func decodeError(t *testing.T, rec *httptest.ResponseRecorder) httpx.ErrorBody {
	t.Helper()
	var body httpx.ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("错误响应不是合法 JSON：%v，body=%s", err, rec.Body.String())
	}
	return body
}

func TestChatNonStreamSuccess(t *testing.T) {
	e := newEnv(t)
	srv, upstream := startUpstream(t, http.StatusOK,
		upstreamChatSSE("Hello", `{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}`))
	e.up.endpoints["p1"] = srv.URL
	e.allowModel(t, "m1", "p1")

	rec := e.post(t, `{"model":"m1","messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200，body=%s", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	if body.Object != "chat.completion" || body.Model != "m1" || body.ID != completionID(testReqID) {
		t.Fatalf("响应元信息不符：%+v", body)
	}
	if len(body.Choices) != 1 || body.Choices[0].Message.Content != "Hello" || body.Choices[0].FinishReason != "stop" {
		t.Fatalf("choices 不符：%+v", body.Choices)
	}
	if body.Usage != (usageBody{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}) {
		t.Fatalf("usage = %+v，期望 10/5/15", body.Usage)
	}
	// 上游收到的必须是上游模型名与注入的密钥。
	if upstream.count() != 1 {
		t.Fatalf("上游命中 %d 次，期望 1", upstream.count())
	}
	if got := upstream.authorization(); got != "Bearer "+testAPIKey {
		t.Fatalf("上游 Authorization = %q，期望注入测试密钥", got)
	}
	if !strings.Contains(upstream.body, `"model":"upstream-p1"`) {
		t.Fatalf("上游请求体未使用上游模型名：%s", upstream.body)
	}
	// 响应体不得泄漏上游地址与密钥。
	if raw := rec.Body.String(); strings.Contains(raw, testAPIKey) || strings.Contains(raw, srv.URL) {
		t.Fatalf("响应体泄漏了上游信息：%s", raw)
	}
	// 成功路径由 quota 结算（写 usage 行），网关不自己写行。
	if _, settled, released := e.quota.counts(); settled != 1 || released != 0 {
		t.Fatalf("预占收尾错误：settled=%d released=%d", settled, released)
	}
	got, ok := e.quota.settleRecord()
	if !ok {
		t.Fatal("没有结算记录")
	}
	want := model.UsageRecord{
		RequestID: testReqID, UserID: testUserID, ModelID: "m1", ProviderID: "p1",
		Status: quota.UsageStatusSettled, InputTokens: 10, OutputTokens: 5,
		CostMicro: quota.CostMicro(10, 5, quota.DefaultPriceMicroPerKTok),
	}
	if got.RequestID != want.RequestID || got.ProviderID != want.ProviderID || got.ModelID != want.ModelID ||
		got.InputTokens != want.InputTokens || got.OutputTokens != want.OutputTokens ||
		got.CostMicro != want.CostMicro || got.Status != want.Status {
		t.Fatalf("结算记录 = %+v，期望 %+v", got, want)
	}
	if len(e.usage.rows) != 0 {
		t.Fatalf("成功路径不该由网关写 usage 行：%+v", e.usage.rows)
	}
}

func TestChatInsufficientQuotaDoesNotReachUpstream(t *testing.T) {
	e := newEnv(t)
	srv, upstream := startUpstream(t, http.StatusOK, upstreamChatSSE("never", `{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}`))
	e.up.endpoints["p1"] = srv.URL
	e.allowModel(t, "m1", "p1")
	e.quota.reserveErr = model.ErrInsufficientQuota

	rec := e.post(t, `{"model":"m1","messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("状态码 = %d，期望 402，body=%s", rec.Code, rec.Body.String())
	}
	if code := decodeError(t, rec).Error.Code; code != "insufficient_quota" {
		t.Fatalf("错误码 = %q，期望 insufficient_quota", code)
	}
	if upstream.count() != 0 {
		t.Fatalf("额度不足时不应打上游，实际命中 %d 次", upstream.count())
	}
}

func TestChatModelNotFoundSkipsReservation(t *testing.T) {
	e := newEnv(t)
	rec := e.post(t, `{"model":"nope","messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("状态码 = %d，期望 404，body=%s", rec.Code, rec.Body.String())
	}
	if code := decodeError(t, rec).Error.Code; code != "model_not_found" {
		t.Fatalf("错误码 = %q，期望 model_not_found", code)
	}
	if held, _, _ := e.quota.counts(); held != 0 {
		t.Fatalf("模型不存在不应预占额度，实际预占 %d 次", held)
	}
}

func TestChatDisabledModelReturns404(t *testing.T) {
	e := newEnv(t)
	e.models.specs["m1"] = model.ModelSpec{ModelID: "m1", Enabled: false}
	rec := e.post(t, `{"model":"m1","messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("状态码 = %d，期望 404", rec.Code)
	}
	if held, _, _ := e.quota.counts(); held != 0 {
		t.Fatalf("模型未启用不应预占额度")
	}
}

func TestChatRetryableUpstreamErrorSwitchesCandidate(t *testing.T) {
	e := newEnv(t)
	bad, badRec := startUpstream(t, http.StatusInternalServerError, nil)
	good, _ := startUpstream(t, http.StatusOK, upstreamChatSSE("from-B", `{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}`))
	e.up.endpoints["p1"] = bad.URL
	e.up.endpoints["p2"] = good.URL
	e.allowModel(t, "m1", "p1", "p2")

	rec := e.post(t, `{"model":"m1","messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200，body=%s", rec.Code, rec.Body.String())
	}
	if got := decodeBody(t, rec).Choices[0].Message.Content; got != "from-B" {
		t.Fatalf("内容 = %q，期望来自第二个候选", got)
	}
	if badRec.count() != 1 {
		t.Fatalf("第一个候选命中 %d 次，期望 1", badRec.count())
	}
	// 换候选后必须用第二个候选结算，且不能有归还。
	got, _ := e.quota.settleRecord()
	if got.ProviderID != "p2" {
		t.Fatalf("结算的 provider = %q，期望 p2", got.ProviderID)
	}
	if _, settled, released := e.quota.counts(); settled != 1 || released != 0 {
		t.Fatalf("收尾错误：settled=%d released=%d", settled, released)
	}
}

func TestChatAllCandidatesFailReleasesReservation(t *testing.T) {
	e := newEnv(t)
	b1, _ := startUpstream(t, http.StatusInternalServerError, nil)
	b2, _ := startUpstream(t, http.StatusBadGateway, nil)
	e.up.endpoints["p1"] = b1.URL
	e.up.endpoints["p2"] = b2.URL
	e.allowModel(t, "m1", "p1", "p2")

	rec := e.post(t, `{"model":"m1","messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("状态码 = %d，期望 502，body=%s", rec.Code, rec.Body.String())
	}
	if code := decodeError(t, rec).Error.Code; code != "provider_unavailable" {
		t.Fatalf("错误码 = %q，期望 provider_unavailable", code)
	}
	held, settled, released := e.quota.counts()
	if held != 1 || settled != 0 || released != 1 {
		t.Fatalf("预占收尾错误：held=%d settled=%d released=%d，期望 1/0/1", held, settled, released)
	}
	row, ok := e.usage.last()
	if !ok {
		t.Fatal("失败路径必须落 usage 行标明终态")
	}
	if row.Status != OutcomeUpstreamError || row.ProviderID != "p2" || row.ModelID != "m1" || row.CostMicro != 0 {
		t.Fatalf("终态 usage 行不符：%+v", row)
	}
	if raw := rec.Body.String(); strings.Contains(raw, b1.URL) || strings.Contains(raw, testAPIKey) {
		t.Fatalf("错误响应泄漏上游信息：%s", raw)
	}
}

func TestChatNonRetryableUpstreamErrorDoesNotSwitchCandidate(t *testing.T) {
	e := newEnv(t)
	bad, _ := startUpstream(t, http.StatusUnauthorized, nil)
	good, goodRec := startUpstream(t, http.StatusOK, upstreamChatSSE("should-not-happen", `{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}`))
	e.up.endpoints["p1"] = bad.URL
	e.up.endpoints["p2"] = good.URL
	e.allowModel(t, "m1", "p1", "p2")

	rec := e.post(t, `{"model":"m1","messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("状态码 = %d，期望 502（上游鉴权失败不外泄成 401），body=%s", rec.Code, rec.Body.String())
	}
	if code := decodeError(t, rec).Error.Code; code != "upstream_auth_failed" {
		t.Fatalf("错误码 = %q，期望 upstream_auth_failed", code)
	}
	if goodRec.count() != 0 {
		t.Fatalf("不可重试错误不应换候选，第二个候选被打了 %d 次", goodRec.count())
	}
	if _, settled, released := e.quota.counts(); settled != 0 || released != 1 {
		t.Fatalf("收尾错误：settled=%d released=%d，期望 0/1", settled, released)
	}
}

func TestChatUsageMissingRecordsZeros(t *testing.T) {
	e := newEnv(t)
	// 上游只报内容与终止原因，不给 usage。
	srv, _ := startUpstream(t, http.StatusOK, []string{
		sseData(`{"id":"c","choices":[{"index":0,"delta":{"content":"no-usage"},"finish_reason":null}]}`),
		sseData(`{"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`),
		sseData("[DONE]"),
	})
	e.up.endpoints["p1"] = srv.URL
	e.allowModel(t, "m1", "p1")

	rec := e.post(t, `{"model":"m1","messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", rec.Code)
	}
	body := decodeBody(t, rec)
	if body.Usage != (usageBody{}) {
		t.Fatalf("上游没报 usage 时必须如实记 0，实际 %+v", body.Usage)
	}
	got, ok := e.quota.settleRecord()
	if !ok {
		t.Fatal("缺少结算记录")
	}
	if got.InputTokens != 0 || got.OutputTokens != 0 || got.CostMicro != 0 {
		t.Fatalf("缺 usage 时结算应按 0 计：%+v", got)
	}
	if _, settled, released := e.quota.counts(); settled != 1 || released != 0 {
		t.Fatalf("缺 usage 但请求成功，仍应结算（不归还）：settled=%d released=%d", settled, released)
	}
}

func TestChatNoCandidateReleasesReservation(t *testing.T) {
	e := newEnv(t)
	e.models.specs["m1"] = model.ModelSpec{ModelID: "m1", Enabled: true}
	rec := e.post(t, `{"model":"m1","messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("状态码 = %d，期望 502，body=%s", rec.Code, rec.Body.String())
	}
	held, settled, released := e.quota.counts()
	if held != 1 || settled != 0 || released != 1 {
		t.Fatalf("预占收尾错误：held=%d settled=%d released=%d", held, settled, released)
	}
	if len(e.usage.rows) != 0 {
		t.Fatalf("未打到上游不该写 usage 行：%+v", e.usage.rows)
	}
}

func TestChatProtocolMismatchIsFilteredOut(t *testing.T) {
	e := newEnv(t)
	other, otherRec := startUpstream(t, http.StatusOK, upstreamChatSSE("x", `{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}`))
	e.up.endpoints["p1"] = other.URL
	e.models.specs["m1"] = model.ModelSpec{ModelID: "m1", Enabled: true}
	e.cat.byModel["m1"] = []catalog.Candidate{{
		Provider: model.ProviderSpec{
			ID:       "p1",
			Protocol: model.ProtocolAnthropic, // V1 不支持的上游协议（§11.1.4）
			Status:   model.ProviderStatusEnabled,
		},
	}}

	rec := e.post(t, `{"model":"m1","messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("状态码 = %d，期望 502，body=%s", rec.Code, rec.Body.String())
	}
	if otherRec.count() != 0 {
		t.Fatalf("协议不符的候选不应被调用，实际命中 %d 次", otherRec.count())
	}
	if _, settled, released := e.quota.counts(); settled != 0 || released != 1 {
		t.Fatalf("收尾错误：settled=%d released=%d", settled, released)
	}
}
