package meta

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/account"
	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/catalog"
	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/model"
	"github.com/ximo888ok-netizen/ximo-agent/internal/observability"
)

func TestRoutesMountWithLegalPatterns(t *testing.T) {
	routes := Routes(Deps{})
	want := []string{
		"GET /v1/health",
		"GET /v1/capabilities",
		"GET /v1/models",
		"GET /v1/usage",
		"POST /v1/auth/device",
		"POST /v1/auth/token",
		"POST /v1/auth/refresh",
		"POST /v1/auth/login",
	}
	if len(routes) != len(want) {
		t.Fatalf("路由条数 = %d, 期望 %d", len(routes), len(want))
	}
	seen := make(map[string]bool, len(routes))
	for i, rt := range routes {
		if rt.Pattern != want[i] {
			t.Errorf("路由 %d = %q, 期望 %q", i, rt.Pattern, want[i])
		}
		if rt.Handler == nil {
			t.Errorf("路由 %q 的 handler 为 nil", rt.Pattern)
		}
		if seen[rt.Pattern] {
			t.Errorf("路由 %q 重复", rt.Pattern)
		}
		seen[rt.Pattern] = true
	}
	// 挂到 ServeMux：模式非法会 panic，同时验证「先方法后路径」的写法可用。
	_ = newRouter(t, Deps{})
}

// TestRouteGroupsSplit 钉住「哪些路由需要用户态鉴权」：装配方要能只把
// /v1/models 与 /v1/usage 交给 RequireUser 中间件，否则登录链会被 401 挡住。
func TestRouteGroupsSplit(t *testing.T) {
	all := Routes(Deps{})
	user := UserRoutes(Deps{})
	pub := PublicRoutes(Deps{})

	wantUser := []string{"GET /v1/models", "GET /v1/usage"}
	if len(user) != len(wantUser) {
		t.Fatalf("用户态路由条数 = %d, 期望 %d", len(user), len(wantUser))
	}
	for i, rt := range user {
		if rt.Pattern != wantUser[i] {
			t.Errorf("用户态路由 %d = %q, 期望 %q", i, rt.Pattern, wantUser[i])
		}
	}
	if len(pub)+len(user) != len(all) {
		t.Fatalf("分组条数 %d+%d != 总数 %d", len(pub), len(user), len(all))
	}
	counts := make(map[string]int, len(all))
	for _, rt := range user {
		counts[rt.Pattern]++
	}
	for _, rt := range pub {
		counts[rt.Pattern]++
	}
	for _, rt := range all {
		if counts[rt.Pattern] != 1 {
			t.Errorf("路由 %q 在分组中出现 %d 次（应恰好 1 次）", rt.Pattern, counts[rt.Pattern])
		}
		delete(counts, rt.Pattern)
	}
	if len(counts) != 0 {
		t.Errorf("分组包含 Routes 之外的路由: %v", counts)
	}
}

// Deps 里的接口必须由真实实现直接满足（签名漂移在这里先失败）。
var (
	_ AccountService = (*account.Service)(nil)
	_ CatalogService = (*catalog.Catalog)(nil)
	_ Logger         = (*recLogger)(nil)
)

func TestHealth(t *testing.T) {
	mux := newRouter(t, Deps{Version: "v9.9.9"})
	before := time.Now().UnixMilli()
	rec := do(t, mux, http.MethodGet, "/v1/health", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200", rec.Code)
	}
	body := bodyMap(t, rec)
	assertKeys(t, body, "status", "version", "time_ms")
	if body["status"] != "ok" {
		t.Errorf("status = %v, 期望 ok", body["status"])
	}
	if body["version"] != "v9.9.9" {
		t.Errorf("version = %v, 期望 v9.9.9", body["version"])
	}
	ts, _ := body["time_ms"].(float64)
	if int64(ts) < before {
		t.Errorf("time_ms = %v 早于请求时间 %d", body["time_ms"], before)
	}
	assertNoLeak(t, rec)

	// 未注入 Version 时用默认值，而不是空串。
	rec = do(t, newRouter(t, Deps{}), http.MethodGet, "/v1/health", nil)
	if got := bodyMap(t, rec)["version"]; got != DefaultVersion {
		t.Errorf("默认 version = %v, 期望 %q", got, DefaultVersion)
	}
}

func TestCapabilities(t *testing.T) {
	mux := newRouter(t, Deps{Version: "v1.2.3"})
	rec := do(t, mux, http.MethodGet, "/v1/capabilities", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200", rec.Code)
	}
	body := bodyMap(t, rec)
	assertKeys(t, body, "protocol_version", "server_version", "features", "limits")
	if body["protocol_version"] != float64(ProtocolVersion) {
		t.Errorf("protocol_version = %v", body["protocol_version"])
	}
	if body["server_version"] != "v1.2.3" {
		t.Errorf("server_version = %v", body["server_version"])
	}

	features, ok := body["features"].(map[string]any)
	if !ok {
		t.Fatalf("features 不是对象: %T", body["features"])
	}
	assertKeys(t, features, "device_login", "token_refresh", "api_keys", "openai_chat", "anthropic_messages", "streaming")
	for k, v := range features {
		if v != true {
			t.Errorf("features[%s] = %v, 期望 true（V1 契约已冻结这些能力）", k, v)
		}
	}

	limits, ok := body["limits"].(map[string]any)
	if !ok {
		t.Fatalf("limits 不是对象: %T", body["limits"])
	}
	wantLimits := map[string]float64{
		"max_request_bytes":            maxRequestBytes,
		"default_page_size":            defaultPageSize,
		"max_page_size":                maxPageSize,
		"access_token_ttl_seconds":     float64(account.DefaultAccessTTL / time.Second),
		"refresh_token_ttl_seconds":    float64(account.DefaultRefreshTTL / time.Second),
		"device_code_ttl_seconds":      float64(account.DefaultDeviceCodeTTL / time.Second),
		"device_poll_interval_seconds": float64(account.DefaultPollIntervalMS / 1000),
	}
	assertKeys(t, limits, "max_request_bytes", "default_page_size", "max_page_size",
		"access_token_ttl_seconds", "refresh_token_ttl_seconds", "device_code_ttl_seconds",
		"device_poll_interval_seconds")
	for k, want := range wantLimits {
		if got, _ := limits[k].(float64); got != want {
			t.Errorf("limits[%s] = %v, 期望 %v", k, limits[k], want)
		}
	}
	assertNoLeak(t, rec)
}

func TestModelsShapeAndWhitelist(t *testing.T) {
	cat := &fakeCatalog{
		models: []model.ModelSpec{
			{
				ModelID:     "gpt-4o-mini",
				DisplayName: "GPT-4o mini",
				// 多带一个内部键：不得出现在响应里。
				CapabilitiesJSON: `{"stream":true,"vision":true,"tools":true,"reasoning":false,"internal_note":"do-not-leak"}`,
				Enabled:          true,
			},
			{ModelID: "caps-broken", DisplayName: "Broken", CapabilitiesJSON: `{"stream":`, Enabled: true},
			{ModelID: "caps-missing", DisplayName: "Missing", Enabled: true},
			{ModelID: "retired", DisplayName: "Retired", Enabled: false, CapabilitiesJSON: `{"stream":true}`},
		},
		cands: map[string][]catalog.Candidate{
			"gpt-4o-mini": {{
				Provider:      model.ProviderSpec{ID: "prov-main", Protocol: model.ProtocolOpenAIChat, APIKeyRef: "secretref:v1:deadbeef"},
				UpstreamModel: "gpt-4o-mini-2024-07-18",
			}},
		},
	}
	log := &recLogger{}
	mux := newRouter(t, Deps{Catalog: cat, Logger: log})

	rec := do(t, mux, http.MethodGet, "/v1/models", nil, withUser("usr-1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200, body=%s", rec.Code, rec.Body.String())
	}
	items := dataList(t, rec)
	if len(items) != 3 {
		t.Fatalf("模型数 = %d, 期望 3（停用模型不出现）", len(items))
	}

	first := items[0]
	assertKeys(t, first, "id", "display_name", "provider", "protocols", "capabilities", "enabled")
	if first["id"] != "gpt-4o-mini" || first["display_name"] != "GPT-4o mini" {
		t.Errorf("模型首条 = %v", first)
	}
	if first["provider"] != "prov-main" {
		t.Errorf("provider = %v, 期望 prov-main", first["provider"])
	}
	if first["enabled"] != true {
		t.Errorf("enabled = %v, 期望 true", first["enabled"])
	}
	protos, ok := first["protocols"].([]any)
	if !ok || len(protos) != 1 || protos[0] != model.ProtocolOpenAIChat {
		t.Errorf("protocols = %v, 期望 [%s]", first["protocols"], model.ProtocolOpenAIChat)
	}
	caps, ok := first["capabilities"].(map[string]any)
	if !ok {
		t.Fatalf("capabilities 不是对象: %T", first["capabilities"])
	}
	assertKeys(t, caps, "stream", "vision", "tools", "reasoning")
	for k, want := range map[string]any{"stream": true, "vision": true, "tools": true, "reasoning": false} {
		if caps[k] != want {
			t.Errorf("capabilities[%s] = %v, 期望 %v", k, caps[k], want)
		}
	}

	// 解析失败的模型：能力全 false，其余字段照常。
	broken := items[1]
	if broken["id"] != "caps-broken" {
		t.Fatalf("第二条 = %v", broken)
	}
	for k, v := range broken["capabilities"].(map[string]any) {
		if v != false {
			t.Errorf("解析失败模型 capabilities[%s] = %v, 期望 false", k, v)
		}
	}
	if broken["provider"] != "" {
		t.Errorf("无候选模型的 provider = %v, 期望空串", broken["provider"])
	}
	if pl, ok := broken["protocols"].([]any); !ok || len(pl) != 0 {
		t.Errorf("无候选模型的 protocols = %v, 期望空数组", broken["protocols"])
	}

	// 空能力字段不算错误：不记日志。
	if _, errs := log.counts(); errs != 0 {
		t.Errorf("不应有 Error 日志: %v", log.errs)
	}
	if warns, _ := log.counts(); warns != 1 {
		t.Errorf("Warn 次数 = %d, 期望 1（仅 caps-broken）: %v", warns, log.warns)
	}
	assertNoLeak(t, rec, "internal_note", "secretref", "do-not-leak", "gpt-4o-mini-2024-07-18")
}

func TestModelsCatalogFailure(t *testing.T) {
	cat := &fakeCatalog{errModels: errors.New("boom-internal-catalog")}
	log := &recLogger{}
	mux := newRouter(t, Deps{Catalog: cat, Logger: log})
	rec := do(t, mux, http.MethodGet, "/v1/models", nil, withUser("usr-1"))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d, 期望 500", rec.Code)
	}
	if got := errorCode(t, rec); got != "server_error" {
		t.Errorf("错误码 = %q, 期望 server_error", got)
	}
	// 兜底文案给客户端，内部错误细节只进日志。
	if body := rec.Body.String(); strings.Contains(body, "boom-internal-catalog") {
		t.Errorf("响应泄漏内部错误细节: %s", body)
	}
	if _, errs := log.counts(); errs != 1 {
		t.Errorf("Error 日志次数 = %d, 期望 1", errs)
	}
	assertNoLeak(t, rec)
}

func TestModelsCandidatesFailureKeepsListing(t *testing.T) {
	cat := &fakeCatalog{
		models: []model.ModelSpec{
			{ModelID: "dirty", DisplayName: "Dirty", Enabled: true, CapabilitiesJSON: `{"stream":true}`},
			{ModelID: "clean", DisplayName: "Clean", Enabled: true},
		},
		errCands: map[string]error{"dirty": errors.New("gwstore: 读取 provider 失败")},
		cands: map[string][]catalog.Candidate{
			"clean": {{Provider: model.ProviderSpec{ID: "prov-x", Protocol: model.ProtocolOpenAIChat}}},
		},
	}
	log := &recLogger{}
	mux := newRouter(t, Deps{Catalog: cat, Logger: log})
	rec := do(t, mux, http.MethodGet, "/v1/models", nil, withUser("usr-1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("单个模型候选读取失败不应打掉整个列表: %d %s", rec.Code, rec.Body.String())
	}
	items := dataList(t, rec)
	if len(items) != 2 {
		t.Fatalf("模型数 = %d, 期望 2", len(items))
	}
	if items[0]["provider"] != "" {
		t.Errorf("脏数据模型 provider = %v, 期望空串", items[0]["provider"])
	}
	if items[1]["provider"] != "prov-x" {
		t.Errorf("正常模型 provider = %v, 期望 prov-x", items[1]["provider"])
	}
	if _, errs := log.counts(); errs != 1 {
		t.Errorf("Error 日志次数 = %d, 期望 1", errs)
	}
}

func TestUserRoutesRequireAuth(t *testing.T) {
	cat := &fakeCatalog{models: []model.ModelSpec{{ModelID: "m", Enabled: true}}}
	st := newFakeStore()
	mux := newRouter(t, Deps{Catalog: cat, Store: st})

	for _, target := range []string{"/v1/models", "/v1/usage"} {
		rec := do(t, mux, http.MethodGet, target, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s 未鉴权状态码 = %d, 期望 401", target, rec.Code)
		}
		if got := errorCode(t, rec); got != "invalid_api_key" {
			t.Errorf("%s 未鉴权错误码 = %q", target, got)
		}
		assertNoLeak(t, rec)
	}
	if cat.modelsCalls != 0 {
		t.Errorf("未鉴权请求不应查目录: calls=%d", cat.modelsCalls)
	}
	if st.listUsageCalls != 0 {
		t.Errorf("未鉴权请求不应查用量: calls=%d", st.listUsageCalls)
	}
}

func TestUserGetterOverrideAndReject(t *testing.T) {
	st := newFakeStore()
	mux := newRouter(t, Deps{Store: st, User: func(ctx context.Context) (string, bool) { return "usr-from-middleware", true }})
	rec := do(t, mux, http.MethodGet, "/v1/usage", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200", rec.Code)
	}
	if st.lastUsageUser != "usr-from-middleware" {
		t.Errorf("传给 store 的 userID = %q", st.lastUsageUser)
	}

	// 取值函数返回 ok=false：一律 401，不匿名降级。
	deny := newRouter(t, Deps{Store: st, User: func(context.Context) (string, bool) { return "", false }})
	rec = do(t, deny, http.MethodGet, "/v1/usage", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("状态码 = %d, 期望 401", rec.Code)
	}
	// 走 WithUserID 时同样被拒（空 ID 不算已鉴权）。
	rec = do(t, deny, http.MethodGet, "/v1/usage", nil, withUser(""))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("空 userID 状态码 = %d, 期望 401", rec.Code)
	}
}

func TestDependenciesNotWired(t *testing.T) {
	mux := newRouter(t, Deps{})
	cases := []struct {
		method, target string
		body           any
		user           bool
	}{
		{http.MethodGet, "/v1/models", nil, true},
		{http.MethodGet, "/v1/usage", nil, true},
		{http.MethodPost, "/v1/auth/device", nil, false},
		{http.MethodPost, "/v1/auth/token", map[string]any{"device_code": "gwd_x"}, false},
		{http.MethodPost, "/v1/auth/refresh", map[string]any{"refresh_token": "gwr_x"}, false},
		{http.MethodPost, "/v1/auth/login", map[string]any{"username": "u", "password": "p"}, false},
	}
	for _, c := range cases {
		var opts []requestOpt
		if c.user {
			opts = append(opts, withUser("usr-1"))
		}
		rec := do(t, mux, c.method, c.target, c.body, opts...)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s 状态码 = %d, 期望 503", c.target, rec.Code)
		}
		if got := errorCode(t, rec); got != "service_unavailable" {
			t.Errorf("%s 错误码 = %q", c.target, got)
		}
		assertNoLeak(t, rec)
	}
}

func TestUsageOnlyOwnRecords(t *testing.T) {
	st := newFakeStore()
	st.addUsage(
		model.UsageRecord{RequestID: "req-a1", UserID: "usr-a", ModelID: "m1", ProviderID: "p1", Status: "ok",
			InputTokens: 10, OutputTokens: 5, LatencyMS: 120, CostMicro: 3, CreatedAt: 3000},
		model.UsageRecord{RequestID: "req-b1", UserID: "usr-b", ModelID: "m2", ProviderID: "p2", Status: "ok",
			InputTokens: 99, OutputTokens: 99, CostMicro: 99, CreatedAt: 4000},
		model.UsageRecord{RequestID: "req-a2", UserID: "usr-a", ModelID: "m3", ProviderID: "p1", Status: "client_canceled",
			InputTokens: 1, OutputTokens: 0, CreatedAt: 2000},
	)
	mux := newRouter(t, Deps{Store: st})

	rec := do(t, mux, http.MethodGet, "/v1/usage?limit=10&offset=0", nil, withUser("usr-a"))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200, body=%s", rec.Code, rec.Body.String())
	}
	items := dataList(t, rec)
	if len(items) != 2 {
		t.Fatalf("记录数 = %d, 期望 2（只含 usr-a）", len(items))
	}
	if items[0]["request_id"] != "req-a1" || items[1]["request_id"] != "req-a2" {
		t.Errorf("顺序应为最近优先: %v", items)
	}
	assertKeys(t, items[0], "request_id", "model_id", "provider_id", "status",
		"input_tokens", "output_tokens", "latency_ms", "cost_micro", "created_at")
	if items[0]["cost_micro"] != float64(3) || items[0]["created_at"] != float64(3000) {
		t.Errorf("字段值不符: %v", items[0])
	}
	if st.lastUsageUser != "usr-a" {
		t.Errorf("传给 store 的 userID = %q, 期望 usr-a", st.lastUsageUser)
	}
	// 客户端不能通过 query 参数换用户。
	rec = do(t, mux, http.MethodGet, "/v1/usage?user_id=usr-b", nil, withUser("usr-a"))
	if st.lastUsageUser != "usr-a" {
		t.Errorf("query 里的 user_id 影响了查询用户: %q", st.lastUsageUser)
	}
	if len(dataList(t, rec)) != 2 {
		t.Errorf("user_id 参数不应改变结果")
	}
	assertNoLeak(t, rec)
}

func TestUsageEmptyAndPaging(t *testing.T) {
	st := newFakeStore()
	mux := newRouter(t, Deps{Store: st})

	rec := do(t, mux, http.MethodGet, "/v1/usage", nil, withUser("usr-empty"))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200", rec.Code)
	}
	if items := dataList(t, rec); len(items) != 0 {
		t.Errorf("空结果应为空数组, 得到 %v", items)
	}
	if !strings.Contains(rec.Body.String(), `"data":[]`) {
		t.Errorf("空结果应序列化为 []：%s", rec.Body.String())
	}

	// limit 超上限静默收紧；offset 原样传递；limit<=0 交由存储层取默认值。
	rec = do(t, mux, http.MethodGet, "/v1/usage?limit=1000&offset=7", nil, withUser("usr-1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", rec.Code)
	}
	if st.lastUsageLimit != maxPageSize || st.lastUsageOffset != 7 {
		t.Errorf("limit/offset = %d/%d, 期望 %d/7", st.lastUsageLimit, st.lastUsageOffset, maxPageSize)
	}
	rec = do(t, mux, http.MethodGet, "/v1/usage?limit=0", nil, withUser("usr-1"))
	if st.lastUsageLimit != 0 || rec.Code != http.StatusOK {
		t.Errorf("limit=0 应交给存储层取默认值, got limit=%d code=%d", st.lastUsageLimit, rec.Code)
	}
}

func TestUsageInvalidQuery(t *testing.T) {
	st := newFakeStore()
	mux := newRouter(t, Deps{Store: st})
	for _, target := range []string{"/v1/usage?limit=abc", "/v1/usage?limit=-1", "/v1/usage?offset=x"} {
		rec := do(t, mux, http.MethodGet, target, nil, withUser("usr-1"))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s 状态码 = %d, 期望 400", target, rec.Code)
		}
		if got := errorCode(t, rec); got != "invalid_request_error" {
			t.Errorf("%s 错误码 = %q", target, got)
		}
		assertNoLeak(t, rec)
	}
	if st.listUsageCalls != 0 {
		t.Errorf("非法参数不应查库: calls=%d", st.listUsageCalls)
	}
}

func TestUsageStoreFailure(t *testing.T) {
	st := newFakeStore()
	st.errListUsage = errors.New("boom-internal-usage")
	log := &recLogger{}
	mux := newRouter(t, Deps{Store: st, Logger: log})
	rec := do(t, mux, http.MethodGet, "/v1/usage", nil, withUser("usr-1"))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d, 期望 500", rec.Code)
	}
	if body := rec.Body.String(); strings.Contains(body, "boom-internal-usage") {
		t.Errorf("响应泄漏内部错误细节: %s", body)
	}
	if _, errs := log.counts(); errs != 1 {
		t.Errorf("Error 日志次数 = %d, 期望 1", errs)
	}
	assertNoLeak(t, rec)
}

func TestMethodNotAllowed(t *testing.T) {
	mux := newRouter(t, Deps{})
	rec := do(t, mux, http.MethodPost, "/v1/health", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("状态码 = %d, 期望 405（模式带方法）", rec.Code)
	}
}

func TestCeilSeconds(t *testing.T) {
	for _, c := range []struct{ in, want int64 }{{0, 0}, {1, 1}, {999, 1}, {1000, 1}, {5000, 5}, {5001, 6}} {
		if got := ceilSeconds(c.in); got != c.want {
			t.Errorf("ceilSeconds(%d) = %d, 期望 %d", c.in, got, c.want)
		}
	}
}

func TestQueryIntEmpty(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/usage", nil)
	if n, err := queryInt(req, "limit"); n != 0 || err != nil {
		t.Fatalf("空参数应返回 0/nil, got %d/%v", n, err)
	}
}

// TestNilLoggerFallsBackToObservability 确认 Deps.Logger 未装配时走 observability
// 默认日志器（而不是丢弃日志），并且日志内容不含被解析失败的原始 JSON。
func TestNilLoggerFallsBackToObservability(t *testing.T) {
	var buf bytes.Buffer
	old := observability.DefaultLogger()
	observability.SetDefaultLogger(observability.NewLogger(&buf, observability.LevelWarn))
	t.Cleanup(func() { observability.SetDefaultLogger(old) })

	cat := &fakeCatalog{models: []model.ModelSpec{
		{ModelID: "caps-bad", DisplayName: "Bad", Enabled: true, CapabilitiesJSON: `{"stream":"yes"}`},
	}}
	mux := newRouter(t, Deps{Catalog: cat})
	rec := do(t, mux, http.MethodGet, "/v1/models", nil, withUser("usr-1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", rec.Code)
	}
	out := buf.String()
	if !strings.Contains(out, "模型能力字段解析失败") || !strings.Contains(out, "caps-bad") {
		t.Errorf("未走 observability 默认日志器: %q", out)
	}
	if strings.Contains(out, `"stream":"yes"`) {
		t.Errorf("日志泄露了原始能力 JSON: %q", out)
	}
}
