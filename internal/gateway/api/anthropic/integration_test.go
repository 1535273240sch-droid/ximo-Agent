package anthropic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/account"
	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/catalog"
	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/model"
	gwstore "github.com/ximo888ok-netizen/ximo-agent/internal/gateway/store"
	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/upstream"
	"github.com/ximo888ok-netizen/ximo-agent/internal/observability"
	"github.com/ximo888ok-netizen/ximo-agent/internal/provider"
	"github.com/ximo888ok-netizen/ximo-agent/internal/quota"
	"github.com/ximo888ok-netizen/ximo-agent/internal/storage/migrations"
	"github.com/ximo888ok-netizen/ximo-agent/internal/storage/sqlite"
)

// 本文件是**真实链路**集成测试：真实 SQLite + 真实迁移 + 真实 gwstore / account /
// quota / catalog / upstream.Pool，只有上游 LLM 是假的（httptest）。
//
// 价值：验证 Deps 的窄接口确实能被共享组件直接满足（装配假设），以及额度链路端到端
// （预占 → 结算 → 账本 → usage 行）在真实存储上的行为，而不只是 fake 上的行为。

// migrationsDir 指向仓库根 migrations/（本包位于 internal/gateway/api/anthropic，向上四级）。
var migrationsDir = filepath.Join("..", "..", "..", "..", "migrations")

func newRealStack(t *testing.T, script upstreamScript) (Deps, *gwstore.Store, *account.Service, *quota.Service, string, string) {
	t.Helper()
	ctx := context.Background()

	dbPath := filepath.Join(t.TempDir(), "gateway.db")
	db, err := sqlite.Open(sqlite.DefaultConfig(dbPath))
	if err != nil {
		t.Fatalf("打开 sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := migrations.ApplyFromDir(ctx, db, migrationsDir); err != nil {
		t.Fatalf("应用迁移: %v", err)
	}

	st := gwstore.New(db)
	acct := account.New(st, nil)
	quotaSvc := quota.New(st)

	user, err := acct.CreateUser(ctx, "alice", "correct horse battery staple", "default")
	if err != nil {
		t.Fatalf("建用户: %v", err)
	}
	key, _, err := acct.CreateAPIKey(ctx, user.ID, 0)
	if err != nil {
		t.Fatalf("签发 API Key: %v", err)
	}
	if _, err := quotaSvc.Adjust(ctx, user.ID, model.LedgerTopup, 5_000_000, "test topup", "test-op", "idem-topup-1"); err != nil {
		t.Fatalf("充值: %v", err)
	}

	up := newFakeUpstream(t, script)
	now := time.Now().UnixMilli()
	if err := st.UpsertModel(ctx, model.ModelSpec{
		ModelID: testModel, DisplayName: "Claude 3.5 Sonnet",
		CapabilitiesJSON: `{"stream":true,"tools":true}`, Enabled: true,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("写模型目录: %v", err)
	}
	if err := st.UpsertProvider(ctx, model.ProviderSpec{
		ID: "p1", Name: "fake-upstream", Endpoint: up.srv.URL,
		Protocol: model.ProtocolOpenAIChat, Status: model.ProviderStatusEnabled,
		APIKeyRef: "secretref:v1:test", TimeoutMS: 10_000,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("写 provider: %v", err)
	}
	if err := st.UpsertProviderModel(ctx, model.ProviderModel{
		ProviderID: "p1", ModelID: testModel, UpstreamModelID: testKUpstr,
		Enabled: true, Priority: 0,
	}); err != nil {
		t.Fatalf("写 provider_model: %v", err)
	}

	quiet := observability.NewLogger(io.Discard, observability.LevelError)
	pool := upstream.NewWithOptions(st,
		upstream.SecretResolverFunc(func(context.Context, string) (string, error) { return "upstream-key", nil }),
		upstream.Options{
			HTTPClient: up.srv.Client(),
			Retry:      provider.RetryPolicy{MaxAttempts: 1},
			Logger:     quiet,
		},
	)
	cat := catalog.New(st, pool)

	deps := Deps{
		Models:            st,
		Catalog:           cat,
		Upstream:          pool,
		Quota:             quotaSvc,
		Auth:              acct,
		Usage:             st,
		PriceMicroPerKTok: 1000,
		Logger:            quiet,
	}
	return deps, st, acct, quotaSvc, user.ID, key
}

func TestIntegrationNonStreamChargesRealLedger(t *testing.T) {
	ctx := context.Background()
	deps, st, _, quotaSvc, userID, key := newRealStack(t, upstreamScript{
		deltas: []string{"Hel", "lo"},
		usage:  `{"prompt_tokens":12,"completion_tokens":2,"total_tokens":14}`,
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"claude-3-5-sonnet","max_tokens":64,"system":"sys","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	routeFor(t, deps)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Content    []map[string]any `json:"content"`
		StopReason string           `json:"stop_reason"`
		Usage      struct {
			Input  int64 `json:"input_tokens"`
			Output int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if out.Content[0]["text"] != "Hello" || out.StopReason != stopReasonEndTurn {
		t.Fatalf("响应内容不符: %s", rec.Body.String())
	}
	if out.Usage.Input != 12 || out.Usage.Output != 2 {
		t.Fatalf("usage 不符: %+v", out.Usage)
	}

	// 真实额度账户：预占清零、按真实 usage 计费（(12+2) * 1000/1000 = 14）。
	qacct, err := quotaSvc.Account(ctx, userID)
	if err != nil {
		t.Fatalf("读账户: %v", err)
	}
	if qacct.ReservedAmount != 0 || qacct.UsedAmount != 14 {
		t.Fatalf("账户不符: reserved=%d used=%d", qacct.ReservedAmount, qacct.UsedAmount)
	}

	// 真实 usage 行。
	rows, err := st.ListUsage(ctx, userID, 10, 0)
	if err != nil {
		t.Fatalf("读 usage: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("期望 1 条 usage，实际 %d", len(rows))
	}
	row := rows[0]
	if row.Status != quota.UsageStatusSettled || row.ModelID != testModel || row.ProviderID != "p1" {
		t.Fatalf("usage 行不符: %+v", row)
	}
	if row.InputTokens != 12 || row.OutputTokens != 2 || row.CostMicro != 14 {
		t.Fatalf("usage 金额/token 不符: %+v", row)
	}

	// 真实账本：充值 + 预占 + 结算，且余额快照与账本一致。
	ledger, err := st.ListLedger(ctx, userID, 10, 0)
	if err != nil {
		t.Fatalf("读账本: %v", err)
	}
	types := make(map[string]int)
	for _, e := range ledger {
		types[e.Type]++
	}
	if types[model.LedgerTopup] != 1 || types[model.LedgerReserve] != 1 || types[model.LedgerSettle] != 1 {
		t.Fatalf("账本类型不符: %v", types)
	}
	sum, err := st.SumLedgerAmount(ctx, userID)
	if err != nil {
		t.Fatalf("账本求和: %v", err)
	}
	if sum != qacct.TotalAmount-qacct.UsedAmount-qacct.ReservedAmount {
		t.Fatalf("账本与账户快照不一致: sum=%d available=%d", sum, qacct.Available())
	}
}

func TestIntegrationStreamEndToEnd(t *testing.T) {
	deps, st, _, quotaSvc, userID, key := newRealStack(t, upstreamScript{
		deltas: []string{"Hel", "lo"},
		usage:  `{"prompt_tokens":9,"completion_tokens":3,"total_tokens":12}`,
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"claude-3-5-sonnet","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	routeFor(t, deps)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	events := parseSSE(t, rec.Body.String())
	want := "message_start,content_block_start,content_block_delta,content_block_delta,content_block_stop,message_delta,message_stop"
	if got := strings.Join(eventNames(events), ","); got != want {
		t.Fatalf("事件序列不符\n期望 %s\n实际 %s", want, got)
	}

	qacct, err := quotaSvc.Account(context.Background(), userID)
	if err != nil {
		t.Fatalf("读账户: %v", err)
	}
	if qacct.ReservedAmount != 0 || qacct.UsedAmount != 12 {
		t.Fatalf("流式结算不符: reserved=%d used=%d", qacct.ReservedAmount, qacct.UsedAmount)
	}
	rows, err := st.ListUsage(context.Background(), userID, 10, 0)
	if err != nil || len(rows) != 1 || rows[0].Status != quota.UsageStatusSettled {
		t.Fatalf("流式 usage 行不符: %+v (err=%v)", rows, err)
	}
}

func TestIntegrationUnknownModelIs404(t *testing.T) {
	deps, _, _, _, _, key := newRealStack(t, upstreamScript{deltas: []string{"x"}})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"gpt-nonexistent","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	routeFor(t, deps)(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("期望 404，实际 %d：%s", rec.Code, rec.Body.String())
	}
}

func TestIntegrationBadAPIKeyIs401(t *testing.T) {
	deps, _, _, _, _, _ := newRealStack(t, upstreamScript{deltas: []string{"x"}})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"claude-3-5-sonnet","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer ximo_sk_deadbeefdeadbeefdeadbeefdeadbeef")
	rec := httptest.NewRecorder()
	routeFor(t, deps)(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("期望 401，实际 %d：%s", rec.Code, rec.Body.String())
	}
}
