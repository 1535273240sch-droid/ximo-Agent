package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/catalog"
	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/model"
	"github.com/ximo888ok-netizen/ximo-agent/internal/observability"
	"github.com/ximo888ok-netizen/ximo-agent/internal/provider"
	"github.com/ximo888ok-netizen/ximo-agent/internal/quota"
)

// 本文件是端到端测试：Anthropic 入站 → 真实翻译 → 真实 provider.Client → 假上游 HTTP →
// Anthropic 出站，同时覆盖额度不足与上游失败两条失败路径的收尾（释放 + 终态 usage）。
// 上游是真的 HTTP（httptest），只有 provider 目录 / 凭据 / 额度存储是 fake。

const (
	testUserID = "u1"
	testModel  = "claude-3-5-sonnet"
	testKUpstr = "gpt-4o-mini" // 上游模型名（与对外模型名不同，验证映射）
)

// --- 假额度存储（实现 quota.accountStore 的全部方法）---

type fakeStore struct {
	mu    sync.Mutex
	acct  model.QuotaAccount
	resv  map[string]*model.Reservation
	usage map[string]model.UsageRecord
}

func newFakeStore(total int64) *fakeStore {
	return &fakeStore{
		acct: model.QuotaAccount{
			UserID: testUserID, TotalAmount: total, Status: model.QuotaStatusActive,
		},
		resv:  map[string]*model.Reservation{},
		usage: map[string]model.UsageRecord{},
	}
}

func (f *fakeStore) ReserveTx(_ context.Context, r model.Reservation, _ string, _ int64) (model.Reservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ex := range f.resv {
		if ex.UserID == r.UserID && ex.RequestID == r.RequestID && ex.Status == model.ReservationHeld {
			return *ex, nil
		}
	}
	if f.acct.Status != model.QuotaStatusActive {
		return model.Reservation{}, model.ErrDisabled
	}
	if f.acct.Available() < r.Amount {
		return model.Reservation{}, model.ErrInsufficientQuota
	}
	f.acct.ReservedAmount += r.Amount
	f.acct.Version++
	cp := r
	f.resv[r.ID] = &cp
	return cp, nil
}

func (f *fakeStore) SettleTx(_ context.Context, reservationID string, actual int64, _, _ string, _ int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.resv[reservationID]
	if !ok {
		return fmt.Errorf("reservation %s not found", reservationID)
	}
	if r.Status == model.ReservationSettled {
		return nil
	}
	f.acct.ReservedAmount -= r.Amount
	f.acct.UsedAmount += actual
	r.Status = model.ReservationSettled
	r.SettledAmount = actual
	return nil
}

func (f *fakeStore) ReleaseTx(_ context.Context, reservationID string, _ string, _ int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.resv[reservationID]
	if !ok {
		return fmt.Errorf("reservation %s not found", reservationID)
	}
	if r.Status == model.ReservationReleased || r.Status == model.ReservationExpired {
		return nil
	}
	f.acct.ReservedAmount -= r.Amount
	r.Status = model.ReservationReleased
	return nil
}

func (f *fakeStore) AdjustTx(_ context.Context, _ string, _ string, _ int64, _, _, _, _ string, _ int64) (model.LedgerEntry, error) {
	return model.LedgerEntry{}, nil
}

func (f *fakeStore) GetQuotaAccount(_ context.Context, _ string) (model.QuotaAccount, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.acct, nil
}

func (f *fakeStore) ExpireReservations(_ context.Context, _ int64, _ int) (int, error) { return 0, nil }

func (f *fakeStore) InsertUsage(_ context.Context, u model.UsageRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.usage[u.RequestID]; ok {
		return nil
	}
	f.usage[u.RequestID] = u
	return nil
}

func (f *fakeStore) account() model.QuotaAccount {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.acct
}

func (f *fakeStore) reservations() []model.Reservation {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]model.Reservation, 0, len(f.resv))
	for _, r := range f.resv {
		out = append(out, *r)
	}
	return out
}

func (f *fakeStore) usageRows() []model.UsageRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]model.UsageRecord, 0, len(f.usage))
	for _, u := range f.usage {
		out = append(out, u)
	}
	return out
}

func (f *fakeStore) onlyUsage(t *testing.T) model.UsageRecord {
	t.Helper()
	rows := f.usageRows()
	if len(rows) != 1 {
		t.Fatalf("期望恰好 1 条 usage，实际 %d: %+v", len(rows), rows)
	}
	return rows[0]
}

// --- 假认证 / 模型目录 / 候选 / 上游池 ---

type fakeAuth struct{}

func (fakeAuth) VerifyAPIKey(_ context.Context, plain string) (model.User, model.APIKey, error) {
	if plain != "ximo_sk_test" {
		return model.User{}, model.APIKey{}, model.ErrBadCredentials
	}
	return testUser(), model.APIKey{ID: "k1", UserID: testUserID, Status: model.KeyStatusActive}, nil
}

func (fakeAuth) VerifyAccess(_ context.Context, token string) (model.User, error) {
	if token != "gwa_test" {
		return model.User{}, model.ErrBadCredentials
	}
	return testUser(), nil
}

func testUser() model.User {
	return model.User{ID: testUserID, Username: "alice", Status: model.UserStatusActive}
}

type fakeModels struct{}

func (fakeModels) GetModel(_ context.Context, modelID string) (model.ModelSpec, error) {
	if modelID != testModel {
		return model.ModelSpec{}, model.ErrNotFound
	}
	return model.ModelSpec{ModelID: testModel, Enabled: true, CapabilitiesJSON: `{"stream":true,"tools":true}`}, nil
}

type fakeCatalog struct {
	cands []catalog.Candidate
	err   error
}

func (c fakeCatalog) Candidates(_ context.Context, _ string) ([]catalog.Candidate, error) {
	return c.cands, c.err
}

type fakePool struct {
	mu         sync.Mutex
	builds     int
	buildErr   error
	httpClient *http.Client
}

func (p *fakePool) ClientFor(_ context.Context, spec model.ProviderSpec) (*provider.Client, error) {
	p.mu.Lock()
	p.builds++
	buildErr := p.buildErr
	p.mu.Unlock()
	if buildErr != nil {
		return nil, buildErr
	}
	return provider.NewClient(provider.ClientOptions{
		Config: provider.ProviderConfig{
			ID: spec.ID, Name: spec.Name, BaseURL: spec.Endpoint,
			MaxOutputTokens: 8192, Capabilities: provider.DefaultCapabilities(),
		},
		Secrets:        provider.SecretResolverFunc(func(context.Context, string) (string, error) { return "upstream-key", nil }),
		HTTPClient:     p.httpClient,
		Retry:          provider.RetryPolicy{MaxAttempts: 1},
		DefaultTimeout: 10 * time.Second,
		Logger:         observability.NewLogger(io.Discard, observability.LevelError),
	})
}

func (p *fakePool) buildCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.builds
}

// --- 假上游 ---

type upstreamScript struct {
	// status != 0 时直接回该状态码（模拟首字节前失败）。
	status int
	body   string
	// deltas 文本增量，按顺序推送。
	deltas []string
	// toolCalls 每个元素是一段原始 tool_calls delta JSON。
	toolCalls []string
	// usage 非空时在结尾推送 usage 分片（含 stream_options 要求）。
	usage string
	// finish 显式指定上游报告的 finish_reason；为空时按是否有工具调用给 stop/tool_calls。
	finish string
	// omitFinish 为 true 时完全不发 finish_reason（模拟只发 delta + [DONE] 的上游）。
	omitFinish bool
	// errorAfterFirst 推送第一个文本增量后推一条 error 分片（模拟中途失败）。
	errorAfterFirst bool
	// holdAfterFirst 推送第一个文本增量后挂住，直到客户端断开。
	holdAfterFirst bool
}

type fakeUpstream struct {
	srv    *httptest.Server
	script upstreamScript

	mu       sync.Mutex
	requests []map[string]any
}

func newFakeUpstream(t *testing.T, script upstreamScript) *fakeUpstream {
	t.Helper()
	u := &fakeUpstream{script: script}
	u.srv = httptest.NewServer(http.HandlerFunc(u.handle))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *fakeUpstream) handle(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)

	u.mu.Lock()
	u.requests = append(u.requests, body)
	script := u.script
	u.mu.Unlock()

	if script.status != 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(script.status)
		_, _ = io.WriteString(w, script.body)
		return
	}

	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	write := func(payload string) {
		_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
		if flusher != nil {
			flusher.Flush()
		}
	}
	jsonString := func(s string) string {
		buf, _ := json.Marshal(s)
		return string(buf)
	}

	for i, d := range script.deltas {
		write(fmt.Sprintf(`{"choices":[{"delta":{"content":%s}}]}`, jsonString(d)))
		if script.errorAfterFirst && i == 0 {
			write(`{"error":{"message":"upstream exploded","type":"server_error"}}`)
			return
		}
		if script.holdAfterFirst && i == 0 {
			<-r.Context().Done()
			return
		}
	}
	for _, tc := range script.toolCalls {
		write(fmt.Sprintf(`{"choices":[{"delta":{"tool_calls":%s}}]}`, tc))
	}
	if script.usage != "" {
		write(fmt.Sprintf(`{"choices":[],"usage":%s}`, script.usage))
	}
	if !script.omitFinish {
		finish := script.finish
		if finish == "" {
			finish = "stop"
			if len(script.toolCalls) > 0 {
				finish = "tool_calls"
			}
		}
		write(fmt.Sprintf(`{"choices":[{"delta":{},"finish_reason":%q}]}`, finish))
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

func (u *fakeUpstream) lastRequest(t *testing.T) map[string]any {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.requests) == 0 {
		t.Fatalf("上游未收到任何请求")
	}
	return u.requests[len(u.requests)-1]
}

func (u *fakeUpstream) requestCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.requests)
}

// --- 环境装配 ---

type env struct {
	route    http.HandlerFunc
	store    *fakeStore
	pool     *fakePool
	upstream *fakeUpstream
}

func newEnv(t *testing.T, script upstreamScript, totalMicro int64) *env {
	t.Helper()
	up := newFakeUpstream(t, script)
	st := newFakeStore(totalMicro)
	pool := &fakePool{httpClient: up.srv.Client()}

	deps := Deps{
		Models: fakeModels{},
		Catalog: fakeCatalog{cands: []catalog.Candidate{{
			Provider: model.ProviderSpec{
				ID: "p1", Name: "fake", Endpoint: up.srv.URL,
				Protocol: model.ProtocolOpenAIChat, Status: model.ProviderStatusEnabled,
			},
			UpstreamModel: testKUpstr,
		}}},
		Upstream:          pool,
		Quota:             quota.New(st),
		Auth:              fakeAuth{},
		Usage:             st,
		PriceMicroPerKTok: 1000,
		Logger:            observability.NewLogger(io.Discard, observability.LevelError),
	}
	return &env{route: routeFor(t, deps), store: st, pool: pool, upstream: up}
}

func routeFor(t *testing.T, d Deps) http.HandlerFunc {
	t.Helper()
	for _, r := range Routes(d) {
		if r.Pattern == "POST /v1/messages" {
			return r.Handler
		}
	}
	t.Fatalf("Routes 未导出 POST /v1/messages")
	return nil
}

// do 发起一次请求。token 为空表示不带 Authorization。
func (e *env) do(t *testing.T, body, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	e.route(rec, req)
	return rec
}

// --- 用例 ---

func TestMessagesNonStreamEndToEnd(t *testing.T) {
	e := newEnv(t, upstreamScript{
		deltas: []string{"Hel", "lo"},
		usage:  `{"prompt_tokens":12,"completion_tokens":2,"total_tokens":14}`,
	}, 1_000_000)

	rec := e.do(t, `{"model":"claude-3-5-sonnet","max_tokens":64,"system":"sys",
		"messages":[{"role":"user","content":"hi"}]}`, "ximo_sk_test")

	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Request-Id") == "" {
		t.Fatalf("响应应带 Request-Id")
	}
	var out struct {
		ID         string  `json:"id"`
		Type       string  `json:"type"`
		Role       string  `json:"role"`
		Model      string  `json:"model"`
		Content    []any   `json:"content"`
		StopReason *string `json:"stop_reason"`
		Usage      struct {
			Input  int64 `json:"input_tokens"`
			Output int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if out.Type != "message" || out.Role != "assistant" || out.Model != testModel {
		t.Fatalf("响应外壳不符: %+v", out)
	}
	if !strings.HasPrefix(out.ID, "msg_") {
		t.Fatalf("消息 id 前缀不符: %q", out.ID)
	}
	if out.StopReason == nil || *out.StopReason != stopReasonEndTurn {
		t.Fatalf("stop_reason 期望 end_turn，实际 %v", out.StopReason)
	}
	block := out.Content[0].(map[string]any)
	if block["type"] != "text" || block["text"] != "Hello" {
		t.Fatalf("内容块不符: %v", block)
	}
	if out.Usage.Input != 12 || out.Usage.Output != 2 {
		t.Fatalf("usage 期望 12/2，实际 %d/%d", out.Usage.Input, out.Usage.Output)
	}

	// 上游收到的应是翻译后的 OpenAI 请求。
	up := e.upstream.lastRequest(t)
	if up["model"] != testKUpstr {
		t.Fatalf("上游模型应为 %q，实际 %v", testKUpstr, up["model"])
	}
	if up["max_tokens"].(float64) != 64 {
		t.Fatalf("上游 max_tokens 不符: %v", up["max_tokens"])
	}
	if up["temperature"].(float64) != 1 {
		t.Fatalf("未指定 temperature 应默认 1.0，实际 %v", up["temperature"])
	}
	msgs := up["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("期望 system + user 两条消息，实际 %d", len(msgs))
	}
	if first := msgs[0].(map[string]any); first["role"] != "system" || first["content"] != "sys" {
		t.Fatalf("首条应为 system：%v", first)
	}

	// 结算：预占归还、按真实 usage 计费、usage 行带模型与上游。
	acct := e.store.account()
	if acct.ReservedAmount != 0 {
		t.Fatalf("结算后预占应清零，实际 %d", acct.ReservedAmount)
	}
	if acct.UsedAmount != 14 {
		t.Fatalf("用量成本期望 14 micro（(12+2)*1000/1000），实际 %d", acct.UsedAmount)
	}
	row := e.store.onlyUsage(t)
	if row.Status != quota.UsageStatusSettled || row.ModelID != testModel || row.ProviderID != "p1" {
		t.Fatalf("usage 行不符: %+v", row)
	}
	if row.CostMicro != 14 || row.InputTokens != 12 || row.OutputTokens != 2 {
		t.Fatalf("usage 金额/token 不符: %+v", row)
	}
}

func TestMessagesStreamEndToEnd(t *testing.T) {
	e := newEnv(t, upstreamScript{
		deltas: []string{"Hel", "lo"},
		usage:  `{"prompt_tokens":9,"completion_tokens":3,"total_tokens":12}`,
	}, 1_000_000)

	rec := e.do(t, `{"model":"claude-3-5-sonnet","max_tokens":32,"stream":true,
		"messages":[{"role":"user","content":"hi"}]}`, "ximo_sk_test")

	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type 期望 text/event-stream，实际 %q", ct)
	}
	events := parseSSE(t, rec.Body.String())
	want := []string{
		"message_start", "content_block_start", "content_block_delta", "content_block_delta",
		"content_block_stop", "message_delta", "message_stop",
	}
	if got := strings.Join(eventNames(events), ","); got != strings.Join(want, ",") {
		t.Fatalf("事件序列不符\n期望 %v\n实际 %v", want, eventNames(events))
	}
	delta := events[len(events)-2].data["delta"].(map[string]any)
	if delta["stop_reason"] != stopReasonEndTurn {
		t.Fatalf("stop_reason 期望 end_turn，实际 %v", delta["stop_reason"])
	}
	usage := events[len(events)-2].data["usage"].(map[string]any)
	if usage["input_tokens"].(float64) != 9 || usage["output_tokens"].(float64) != 3 {
		t.Fatalf("流式 usage 不符: %v", usage)
	}

	acct := e.store.account()
	if acct.ReservedAmount != 0 || acct.UsedAmount != 12 {
		t.Fatalf("流式请求应结算 12 micro 且预占清零，实际 reserved=%d used=%d", acct.ReservedAmount, acct.UsedAmount)
	}
}

func TestMessagesStreamToolUseEndToEnd(t *testing.T) {
	e := newEnv(t, upstreamScript{
		toolCalls: []string{
			`[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]`,
			`[{"index":0,"function":{"arguments":"{\"city\":\"北京\"}"}}]`,
		},
		usage: `{"prompt_tokens":11,"completion_tokens":4,"total_tokens":15}`,
	}, 1_000_000)

	rec := e.do(t, `{"model":"claude-3-5-sonnet","max_tokens":32,"stream":true,
		"tools":[{"name":"get_weather","input_schema":{"type":"object"}}],
		"messages":[{"role":"user","content":"北京天气"}]}`, "ximo_sk_test")

	events := parseSSE(t, rec.Body.String())
	var (
		blockType string
		partial   strings.Builder
		reason    string
	)
	for _, ev := range events {
		switch ev.name {
		case "content_block_start":
			blockType = ev.data["content_block"].(map[string]any)["type"].(string)
		case "content_block_delta":
			partial.WriteString(ev.data["delta"].(map[string]any)["partial_json"].(string))
		case "message_delta":
			reason = ev.data["delta"].(map[string]any)["stop_reason"].(string)
		}
	}
	if blockType != blockToolUse {
		t.Fatalf("期望 tool_use 块，实际 %q", blockType)
	}
	if partial.String() != `{"city":"北京"}` {
		t.Fatalf("partial_json 拼接不符: %q", partial.String())
	}
	if reason != stopReasonToolUse {
		t.Fatalf("stop_reason 期望 tool_use，实际 %q", reason)
	}

	up := e.upstream.lastRequest(t)
	tools, _ := up["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("上游应收到 1 个工具定义，实际 %v", up["tools"])
	}
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Fatalf("工具名不符: %v", fn)
	}
}

func TestMessagesInsufficientQuota(t *testing.T) {
	e := newEnv(t, upstreamScript{deltas: []string{"nope"}}, 0)

	rec := e.do(t, `{"model":"claude-3-5-sonnet","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`, "ximo_sk_test")

	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("期望 402，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "insufficient_quota") {
		t.Fatalf("错误码不符: %s", rec.Body.String())
	}
	if e.upstream.requestCount() != 0 {
		t.Fatalf("额度不足不得进上游，实际上游收到 %d 次请求", e.upstream.requestCount())
	}
	if rows := e.store.usageRows(); len(rows) != 0 {
		t.Fatalf("未进上游不应产生 usage 行: %+v", rows)
	}
	if rs := e.store.reservations(); len(rs) != 0 {
		t.Fatalf("预占失败不应留下预占行: %+v", rs)
	}
}

func TestMessagesModelNotFound(t *testing.T) {
	e := newEnv(t, upstreamScript{deltas: []string{"x"}}, 1_000_000)

	rec := e.do(t, `{"model":"unknown-model","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`, "ximo_sk_test")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("期望 404，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "model_not_found") {
		t.Fatalf("错误码不符: %s", rec.Body.String())
	}
	if e.upstream.requestCount() != 0 || e.pool.buildCount() != 0 {
		t.Fatalf("模型不存在不得进上游与建客户端")
	}
	if rs := e.store.reservations(); len(rs) != 0 {
		t.Fatalf("模型校验先于预占，不应有预占行: %+v", rs)
	}
}

func TestMessagesMalformedBody(t *testing.T) {
	e := newEnv(t, upstreamScript{deltas: []string{"x"}}, 1_000_000)
	rec := e.do(t, `{"model":`, "ximo_sk_test")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("期望 400，实际 %d", rec.Code)
	}
}

func TestMessagesUnsupportedContentBlock(t *testing.T) {
	e := newEnv(t, upstreamScript{deltas: []string{"x"}}, 1_000_000)
	rec := e.do(t, `{"model":"claude-3-5-sonnet","max_tokens":8,"messages":[{"role":"user","content":[
		{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]}]}`, "ximo_sk_test")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "unsupported_content_block") {
		t.Fatalf("期望 400 unsupported_content_block，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if e.upstream.requestCount() != 0 {
		t.Fatalf("400 不应进上游")
	}
}

func TestMessagesMissingCredentials(t *testing.T) {
	e := newEnv(t, upstreamScript{deltas: []string{"x"}}, 1_000_000)
	rec := e.do(t, `{"model":"claude-3-5-sonnet","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("期望 401，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if rows := e.store.usageRows(); len(rows) != 0 {
		t.Fatalf("未认证不应产生任何副作用: %+v", rows)
	}
}

// TestMessagesUpstreamFailureBeforeFirstByte 覆盖契约 §11.5 的「首字节前失败」：
// 必须回普通 JSON 错误（非 2xx），且预占归还并落 upstream_error 终态。
func TestMessagesUpstreamFailureBeforeFirstByte(t *testing.T) {
	e := newEnv(t, upstreamScript{status: http.StatusServiceUnavailable, body: `{"error":{"message":"down"}}`}, 1_000_000)

	rec := e.do(t, `{"model":"claude-3-5-sonnet","max_tokens":16,"stream":true,
		"messages":[{"role":"user","content":"hi"}]}`, "ximo_sk_test")

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("期望 502，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("首字节前失败必须是 JSON 错误，实际 Content-Type %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "provider_unavailable") {
		t.Fatalf("错误码不符: %s", rec.Body.String())
	}
	row := e.store.onlyUsage(t)
	if row.Status != usageStatusUpstreamError || row.CostMicro != 0 {
		t.Fatalf("终态 usage 不符: %+v", row)
	}
	if row.ModelID != testModel || row.ProviderID != "p1" {
		t.Fatalf("终态 usage 应带模型与上游: %+v", row)
	}
	acct := e.store.account()
	if acct.ReservedAmount != 0 || acct.UsedAmount != 0 {
		t.Fatalf("失败必须归还预占且不计费，实际 reserved=%d used=%d", acct.ReservedAmount, acct.UsedAmount)
	}
}

// TestMessagesStreamMidStreamError 覆盖「首字节后失败」：已回 SSE 头，只能发 error 事件。
func TestMessagesStreamMidStreamError(t *testing.T) {
	e := newEnv(t, upstreamScript{deltas: []string{"Hel"}, errorAfterFirst: true}, 1_000_000)

	rec := e.do(t, `{"model":"claude-3-5-sonnet","max_tokens":16,"stream":true,
		"messages":[{"role":"user","content":"hi"}]}`, "ximo_sk_test")

	if rec.Code != http.StatusOK {
		t.Fatalf("已开始 SSE 后不应改状态码，实际 %d", rec.Code)
	}
	events := parseSSE(t, rec.Body.String())
	last := events[len(events)-1]
	if last.name != "error" {
		t.Fatalf("最后一个事件应为 error，实际 %v", eventNames(events))
	}
	row := e.store.onlyUsage(t)
	if row.Status != usageStatusUpstreamError {
		t.Fatalf("终态应为 upstream_error，实际 %q", row.Status)
	}
	acct := e.store.account()
	if acct.ReservedAmount != 0 || acct.UsedAmount != 0 {
		t.Fatalf("中途失败应归还预占且不计费，实际 reserved=%d used=%d", acct.ReservedAmount, acct.UsedAmount)
	}
}

func TestMessagesUpstreamNonRetryableError(t *testing.T) {
	e := newEnv(t, upstreamScript{status: http.StatusBadRequest, body: `{"error":{"message":"bad tool"}}`}, 1_000_000)

	rec := e.do(t, `{"model":"claude-3-5-sonnet","max_tokens":16,
		"messages":[{"role":"user","content":"hi"}]}`, "ximo_sk_test")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("上游 400 应回 400，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if e.upstream.requestCount() != 1 {
		t.Fatalf("不可重试错误不得换候选重试，实际上游收到 %d 次", e.upstream.requestCount())
	}
	if row := e.store.onlyUsage(t); row.Status != usageStatusUpstreamError {
		t.Fatalf("终态不符: %+v", row)
	}
}

func TestMessagesNoCandidate(t *testing.T) {
	e := newEnv(t, upstreamScript{deltas: []string{"x"}}, 1_000_000)
	// 候选协议不是 openai-chat → 被过滤掉，等价于「无可用上游」。
	e.route = routeFor(t, Deps{
		Models: fakeModels{},
		Catalog: fakeCatalog{cands: []catalog.Candidate{{
			Provider: model.ProviderSpec{ID: "p2", Protocol: model.ProtocolAnthropic, Status: model.ProviderStatusEnabled},
		}}},
		Upstream: e.pool,
		Quota:    quota.New(e.store),
		Auth:     fakeAuth{},
		Usage:    e.store,
		Logger:   observability.NewLogger(io.Discard, observability.LevelError),
	})

	rec := e.do(t, `{"model":"claude-3-5-sonnet","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, "ximo_sk_test")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("期望 502，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if e.pool.buildCount() != 0 {
		t.Fatalf("协议不匹配的候选不得建客户端")
	}
	acct := e.store.account()
	if acct.ReservedAmount != 0 {
		t.Fatalf("无候选必须释放预占，实际 reserved=%d", acct.ReservedAmount)
	}
}

func TestMessagesClientCancelDuringStream(t *testing.T) {
	e := newEnv(t, upstreamScript{deltas: []string{"Hel"}, holdAfterFirst: true}, 1_000_000)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"claude-3-5-sonnet","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer ximo_sk_test")
	req = req.WithContext(ctx)

	rec := &syncRecorder{rec: httptest.NewRecorder()}
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.route(rec, req)
	}()

	// 等到客户端确实收到第一个内容增量（说明已经进入 SSE 阶段）再断开。
	waitFor(t, 3*time.Second, func() bool { return strings.Contains(rec.body(), "content_block_delta") })
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("客户端取消后 handler 未退出")
	}

	row := e.store.onlyUsage(t)
	if row.Status != usageStatusClientCanceled {
		t.Fatalf("终态应为 client_canceled，实际 %q", row.Status)
	}
	acct := e.store.account()
	if acct.ReservedAmount != 0 {
		t.Fatalf("取消必须释放预占，实际 reserved=%d", acct.ReservedAmount)
	}
	if acct.UsedAmount != 0 {
		t.Fatalf("取消不应计费，实际 used=%d", acct.UsedAmount)
	}
}

// syncRecorder 是并发安全的 ResponseRecorder 包装：流式用例需要在 handler 仍在写的时候
// 读取已产出的字节（httptest.ResponseRecorder 的 Body 并发读写不是安全的）。
type syncRecorder struct {
	mu  sync.Mutex
	rec *httptest.ResponseRecorder
}

func (s *syncRecorder) Header() http.Header {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rec.Header()
}

func (s *syncRecorder) WriteHeader(code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec.WriteHeader(code)
}

func (s *syncRecorder) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rec.Write(b)
}

func (s *syncRecorder) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec.Flush()
}

func (s *syncRecorder) body() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rec.Body.String()
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待条件超时")
}
