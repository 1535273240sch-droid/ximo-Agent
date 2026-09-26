// Package meta_test 用真实 SQLite + 0001~0003 迁移跑本包的对外 HTTP 面：
// 真实 gwstore（读写）＋真实 account（PBKDF2、会话、设备码）＋真实 catalog。
// 放在外部测试包，一是只依赖本包的导出面，二是顺带钉住四个窄接口的签名。
package meta_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ximo888ok-netizen/ximo-agent/internal/account"
	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/api/meta"
	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/catalog"
	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/model"
	gwstore "github.com/ximo888ok-netizen/ximo-agent/internal/gateway/store"
	"github.com/ximo888ok-netizen/ximo-agent/internal/observability"
	"github.com/ximo888ok-netizen/ximo-agent/internal/storage/migrations"
	"github.com/ximo888ok-netizen/ximo-agent/internal/storage/sqlite"
)

// 编译期断言：真实实现必须直接满足本包的窄接口（签名漂移先在这里失败）。
var (
	_ meta.UsageStore     = (*gwstore.Store)(nil)
	_ meta.AccountService = (*account.Service)(nil)
	_ meta.CatalogService = (*catalog.Catalog)(nil)
	_ meta.Logger         = (*observability.Logger)(nil)
)

// migrationsDir 指向仓库根 migrations/（本包位于 internal/gateway/api/meta，向上四级）。
var migrationsDir = filepath.Join("..", "..", "..", "..", "migrations")

// newRealStore 用真实 SQLite + 迁移建库，返回 store 与底层连接（后者供测试直接
// 核对落库状态，例如设备码是否被消费）。
func newRealStore(t *testing.T) (*gwstore.Store, *sqlite.DB) {
	t.Helper()
	db, err := sqlite.Open(sqlite.DefaultConfig(filepath.Join(t.TempDir(), "meta-it.db")))
	if err != nil {
		t.Fatalf("打开 sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := migrations.ApplyFromDir(context.Background(), db, migrationsDir); err != nil {
		t.Fatalf("应用迁移: %v", err)
	}
	return gwstore.New(db), db
}

// countDeviceCodes 统计某状态的设备码行数。
func countDeviceCodes(t *testing.T, db *sqlite.DB, status string) int {
	t.Helper()
	var n int
	err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM gw_device_codes WHERE status=?`, status).Scan(&n)
	if err != nil {
		t.Fatalf("查询设备码状态: %v", err)
	}
	return n
}

func newMux(d meta.Deps) *http.ServeMux {
	mux := http.NewServeMux()
	for _, rt := range meta.Routes(d) {
		mux.HandleFunc(rt.Pattern, rt.Handler)
	}
	return mux
}

// call 发请求；userID 非空时模拟 W-Server 中间件的鉴权结果（Deps.User = meta.UserID）。
func call(t *testing.T, h http.Handler, method, target string, body any, userID string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("编码请求体: %v", err)
		}
		rdr = bytes.NewReader(buf)
	}
	req := httptest.NewRequest(method, target, rdr)
	if userID != "" {
		req = req.WithContext(meta.WithUserID(req.Context(), userID))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是 JSON: %v, body=%s", err, rec.Body.String())
	}
	return out
}

func dataOf(t *testing.T, rec *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
	raw, ok := decode(t, rec)["data"]
	if !ok {
		t.Fatalf("响应缺少 data: %s", rec.Body.String())
	}
	arr, ok := raw.([]any)
	if !ok {
		t.Fatalf("data 不是数组: %T", raw)
	}
	out := make([]map[string]any, 0, len(arr))
	for _, v := range arr {
		out = append(out, v.(map[string]any))
	}
	return out
}

func errCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	inner, ok := decode(t, rec)["error"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺少 error 对象: %s", rec.Body.String())
	}
	code, _ := inner["code"].(string)
	return code
}

func TestMetaRoutesAgainstRealStore(t *testing.T) {
	ctx := context.Background()
	st, db := newRealStore(t)
	acct := account.New(st, nil)
	cat := catalog.New(st, nil)

	alice, err := acct.CreateUser(ctx, "it-alice", "s3cret-password", "default")
	if err != nil {
		t.Fatalf("创建 alice: %v", err)
	}
	bob, err := acct.CreateUser(ctx, "it-bob", "s3cret-password", "default")
	if err != nil {
		t.Fatalf("创建 bob: %v", err)
	}

	// 目录：一个启用的模型 + 一个停用模型 + 一个上游映射。
	if err := st.UpsertModel(ctx, model.ModelSpec{
		ModelID: "gpt-4o-mini", DisplayName: "GPT-4o mini",
		CapabilitiesJSON: `{"stream":true,"vision":false,"tools":true,"reasoning":false}`,
		Enabled:          true,
	}); err != nil {
		t.Fatalf("UpsertModel: %v", err)
	}
	if err := st.UpsertModel(ctx, model.ModelSpec{
		ModelID: "retired-model", DisplayName: "Retired", CapabilitiesJSON: `{"stream":true}`, Enabled: false,
	}); err != nil {
		t.Fatalf("UpsertModel: %v", err)
	}
	if err := st.UpsertProvider(ctx, model.ProviderSpec{
		ID: "prov-main", Name: "Main", Endpoint: "https://upstream.invalid/v1",
		Protocol:  model.ProtocolOpenAIChat,
		Status:    model.ProviderStatusEnabled,
		APIKeyRef: "secretref:v1:deadbeefcafe",
	}); err != nil {
		t.Fatalf("UpsertProvider: %v", err)
	}
	if err := st.UpsertProviderModel(ctx, model.ProviderModel{
		ProviderID: "prov-main", ModelID: "gpt-4o-mini",
		UpstreamModelID: "gpt-4o-mini-2024-07-18", Enabled: true, Priority: 0,
	}); err != nil {
		t.Fatalf("UpsertProviderModel: %v", err)
	}

	// 用量：alice 两条（含终态），bob 一条。
	usages := []model.UsageRecord{
		{RequestID: "req-a2", UserID: alice.ID, ModelID: "gpt-4o-mini", ProviderID: "prov-main",
			Status: "ok", InputTokens: 20, OutputTokens: 8, LatencyMS: 300, CostMicro: 7, CreatedAt: 2_000},
		{RequestID: "req-a1", UserID: alice.ID, ModelID: "gpt-4o-mini", ProviderID: "prov-main",
			Status: "client_canceled", InputTokens: 10, OutputTokens: 0, LatencyMS: 90, CostMicro: 1, CreatedAt: 1_000},
		{RequestID: "req-b1", UserID: bob.ID, ModelID: "gpt-4o-mini", ProviderID: "prov-main",
			Status: "upstream_error", InputTokens: 5, OutputTokens: 0, CreatedAt: 1_500},
	}
	for _, u := range usages {
		if err := st.InsertUsage(ctx, u); err != nil {
			t.Fatalf("InsertUsage(%s): %v", u.RequestID, err)
		}
	}

	mux := newMux(meta.Deps{Store: st, Accounts: acct, Catalog: cat, Version: "it-1", User: meta.UserID})

	// --- /v1/health 与 /v1/capabilities ---
	rec := call(t, mux, http.MethodGet, "/v1/health", nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("health = %d", rec.Code)
	}
	if got := decode(t, rec)["version"]; got != "it-1" {
		t.Errorf("health version = %v", got)
	}
	rec = call(t, mux, http.MethodGet, "/v1/capabilities", nil, "")
	if rec.Code != http.StatusOK || decode(t, rec)["protocol_version"] != float64(1) {
		t.Errorf("capabilities = %d %s", rec.Code, rec.Body.String())
	}

	// --- /v1/models：停用模型不出现，provider/protocols 来自真实目录 ---
	rec = call(t, mux, http.MethodGet, "/v1/models", nil, alice.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("models = %d %s", rec.Code, rec.Body.String())
	}
	items := dataOf(t, rec)
	if len(items) != 1 {
		t.Fatalf("模型数 = %d, 期望 1（retired-model 已停用）: %v", len(items), items)
	}
	if items[0]["id"] != "gpt-4o-mini" || items[0]["provider"] != "prov-main" {
		t.Errorf("模型条目 = %v", items[0])
	}
	if caps, ok := items[0]["capabilities"].(map[string]any); !ok || caps["stream"] != true || caps["vision"] != false {
		t.Errorf("capabilities = %v", items[0]["capabilities"])
	}
	if protos, ok := items[0]["protocols"].([]any); !ok || len(protos) != 1 || protos[0] != model.ProtocolOpenAIChat {
		t.Errorf("protocols = %v", items[0]["protocols"])
	}
	// 上游凭据引用与端点不得出现在响应里。
	for _, bad := range []string{"secretref", "deadbeefcafe", "upstream.invalid", "api_key_ref", "config_json"} {
		if strings.Contains(rec.Body.String(), bad) {
			t.Errorf("models 响应泄漏 %q: %s", bad, rec.Body.String())
		}
	}

	// --- /v1/usage：只回当前用户名下的记录 ---
	rec = call(t, mux, http.MethodGet, "/v1/usage", nil, alice.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("usage = %d %s", rec.Code, rec.Body.String())
	}
	mine := dataOf(t, rec)
	if len(mine) != 2 {
		t.Fatalf("alice 用量条数 = %d, 期望 2: %v", len(mine), mine)
	}
	if mine[0]["request_id"] != "req-a2" || mine[1]["request_id"] != "req-a1" {
		t.Errorf("应为最近优先: %v", mine)
	}
	if mine[0]["model_id"] != "gpt-4o-mini" || mine[0]["cost_micro"] != float64(7) {
		t.Errorf("用量字段 = %v", mine[0])
	}
	if strings.Contains(rec.Body.String(), "req-b1") || strings.Contains(rec.Body.String(), bob.ID) {
		t.Errorf("alice 的用量里出现了别人的数据: %s", rec.Body.String())
	}
	rec = call(t, mux, http.MethodGet, "/v1/usage", nil, bob.ID)
	if got := dataOf(t, rec); len(got) != 1 || got[0]["request_id"] != "req-b1" {
		t.Errorf("bob 用量 = %v", got)
	}
	// 未鉴权一律 401。
	rec = call(t, mux, http.MethodGet, "/v1/usage", nil, "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("未鉴权 usage = %d", rec.Code)
	}
	// 分页参数生效。
	rec = call(t, mux, http.MethodGet, "/v1/usage?limit=1&offset=1", nil, alice.ID)
	if got := dataOf(t, rec); len(got) != 1 || got[0]["request_id"] != "req-a1" {
		t.Errorf("分页结果 = %v", got)
	}

	// --- 登录链：真实 PBKDF2 + 真实会话表 ---
	rec = call(t, mux, http.MethodPost, "/v1/auth/login",
		map[string]any{"username": "it-alice", "password": "s3cret-password"}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("login = %d %s", rec.Code, rec.Body.String())
	}
	tokens := decode(t, rec)
	access, _ := tokens["access_token"].(string)
	refresh, _ := tokens["refresh_token"].(string)
	if u, err := acct.VerifyAccess(ctx, access); err != nil || u.ID != alice.ID {
		t.Fatalf("登录返回的 access 不可用: %v %v", u.ID, err)
	}
	rec = call(t, mux, http.MethodPost, "/v1/auth/login",
		map[string]any{"username": "it-alice", "password": "wrong-password"}, "")
	if rec.Code != http.StatusUnauthorized || errCode(t, rec) != "invalid_api_key" {
		t.Errorf("错误口令 = %d %s", rec.Code, rec.Body.String())
	}

	rec = call(t, mux, http.MethodPost, "/v1/auth/refresh", map[string]any{"refresh_token": refresh}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh = %d %s", rec.Code, rec.Body.String())
	}
	refresh2, _ := decode(t, rec)["refresh_token"].(string)
	if refresh2 == refresh {
		t.Errorf("刷新应换发新 refresh")
	}
	rec = call(t, mux, http.MethodPost, "/v1/auth/refresh", map[string]any{"refresh_token": refresh}, "")
	if rec.Code != http.StatusUnauthorized || errCode(t, rec) != "credential_revoked" {
		t.Errorf("旧 refresh 复用 = %d %s", rec.Code, rec.Body.String())
	}

	// --- 设备码登录链：待授权 → 授权 → 兑换 → 不可复用 ---
	rec = call(t, mux, http.MethodPost, "/v1/auth/device", nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("device = %d %s", rec.Code, rec.Body.String())
	}
	dev := decode(t, rec)
	deviceCode, _ := dev["device_code"].(string)
	userCode, _ := dev["user_code"].(string)
	rec = call(t, mux, http.MethodPost, "/v1/auth/token", map[string]any{"device_code": deviceCode}, "")
	if rec.Code != http.StatusBadRequest || errCode(t, rec) != "authorization_pending" {
		t.Fatalf("待授权兑换 = %d %s", rec.Code, rec.Body.String())
	}
	if err := acct.ApproveDeviceLogin(ctx, userCode, alice.ID); err != nil {
		t.Fatalf("ApproveDeviceLogin: %v", err)
	}
	rec = call(t, mux, http.MethodPost, "/v1/auth/token", map[string]any{"device_code": deviceCode}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("兑换设备码 = %d %s", rec.Code, rec.Body.String())
	}
	accessFromDevice, _ := decode(t, rec)["access_token"].(string)
	if u, err := acct.VerifyAccess(ctx, accessFromDevice); err != nil || u.ID != alice.ID {
		t.Fatalf("设备码换出的 access 不可用: %v %v", u.ID, err)
	}
	rec = call(t, mux, http.MethodPost, "/v1/auth/token", map[string]any{"device_code": deviceCode}, "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("设备码重复兑换 = %d, 期望 401", rec.Code)
	}
	// 一次性消费必须落库（account 经 DeviceCodeConsumer 走真实 ConsumeDeviceCode），
	// 而不是仅靠进程内状态：库中该设备码应为 consumed。
	if n := countDeviceCodes(t, db, model.DeviceStatusConsumed); n != 1 {
		t.Errorf("gw_device_codes 里 consumed 行数 = %d, 期望 1", n)
	}

	// 口令只以哈希形式落库（这里直接读库核对）。
	aliceDB, err := st.GetUserByName(ctx, "it-alice")
	if err != nil {
		t.Fatalf("GetUserByName: %v", err)
	}
	if aliceDB.PasswordHash == "s3cret-password" || !strings.HasPrefix(aliceDB.PasswordHash, "pbkdf2-sha256$") {
		t.Errorf("口令哈希落库形式异常: %q", aliceDB.PasswordHash)
	}
}

// TestUnknownModelRoutingShapes 覆盖「目录存在但没有任何上游映射」的对外形状：
// 模型仍按 enabled 列出，provider/protocols 留空（当前不可路由）。
func TestModelWithoutUpstream(t *testing.T) {
	ctx := context.Background()
	st, _ := newRealStore(t)
	if err := st.UpsertModel(ctx, model.ModelSpec{ModelID: "orphan", DisplayName: "Orphan", Enabled: true,
		CapabilitiesJSON: `{"stream":true}`}); err != nil {
		t.Fatalf("UpsertModel: %v", err)
	}
	acct := account.New(st, nil)
	mux := newMux(meta.Deps{Store: st, Accounts: acct, Catalog: catalog.New(st, nil), User: meta.UserID})

	rec := call(t, mux, http.MethodGet, "/v1/models", nil, "usr-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("models = %d %s", rec.Code, rec.Body.String())
	}
	items := dataOf(t, rec)
	if len(items) != 1 {
		t.Fatalf("模型数 = %d", len(items))
	}
	if items[0]["provider"] != "" || len(items[0]["protocols"].([]any)) != 0 {
		t.Errorf("无上游映射时 provider/protocols 应为空: %v", items[0])
	}
	if items[0]["enabled"] != true {
		t.Errorf("enabled = %v", items[0]["enabled"])
	}
}

// TestStoreErrorSurfacesAsServerError 用已关闭的数据库制造真实读写失败，
// 确认错误被映射成 5xx 且内部错误文本不出现在响应里。
func TestRealStoreFailureMapping(t *testing.T) {
	db, err := sqlite.Open(sqlite.DefaultConfig(filepath.Join(t.TempDir(), "broken.db")))
	if err != nil {
		t.Fatalf("打开 sqlite: %v", err)
	}
	if _, err := migrations.ApplyFromDir(context.Background(), db, migrationsDir); err != nil {
		t.Fatalf("应用迁移: %v", err)
	}
	st := gwstore.New(db)
	if err := db.Close(); err != nil {
		t.Fatalf("关闭 sqlite: %v", err)
	}

	mux := newMux(meta.Deps{Store: st, Catalog: catalog.New(st, nil), User: meta.UserID,
		Logger: observability.NewLogger(io.Discard, observability.LevelError)})

	rec := call(t, mux, http.MethodGet, "/v1/usage", nil, "usr-1")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("关闭库后 usage = %d, 期望 500: %s", rec.Code, rec.Body.String())
	}
	if got := errCode(t, rec); got != "server_error" {
		t.Errorf("错误码 = %q", got)
	}
	if strings.Contains(rec.Body.String(), "sqlite") {
		t.Errorf("响应泄漏底层错误: %s", rec.Body.String())
	}

	// 目录读取失败同样是 5xx，而不是把空目录当成功返回。
	rec = call(t, mux, http.MethodGet, "/v1/models", nil, "usr-1")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("关闭库后 models = %d, 期望 500: %s", rec.Code, rec.Body.String())
	}
}
