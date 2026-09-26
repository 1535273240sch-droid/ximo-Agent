package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/account"
	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/model"
	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/store"
	"github.com/ximo888ok-netizen/ximo-agent/internal/quota"
	"github.com/ximo888ok-netizen/ximo-agent/internal/secrets"
	"github.com/ximo888ok-netizen/ximo-agent/internal/storage/migrations"
	"github.com/ximo888ok-netizen/ximo-agent/internal/storage/sqlite"
)

// 编译期钉住契约 §10.2：*gateway/store.Store 必须能直接传进 Deps，不需要适配器。
var _ Store = (*store.Store)(nil)

const (
	testAdminToken = "test-admin-token-0123456789abcdef"
	testClientIP   = "203.0.113.7"
)

// memBackend 是 internal/secrets 的内存后端（测试用真后端需要凭证管理器，不适合单测）。
type memBackend struct {
	mu     sync.Mutex
	values map[string]string
}

func (b *memBackend) Get(ref string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	v, ok := b.values[ref]
	if !ok {
		return "", secrets.ErrSecretNotFound
	}
	return v, nil
}

func (b *memBackend) Put(ref, value string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.values[ref] = value
	return nil
}

func (b *memBackend) Delete(ref string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.values, ref)
	return nil
}

func (b *memBackend) Available() bool { return true }
func (b *memBackend) Name() string    { return "memory-test" }

type env struct {
	t        *testing.T
	dir      string
	db       *sqlite.DB
	store    *store.Store
	accounts *account.Service
	secrets  *secrets.Manager
	srv      *httptest.Server
}

func newEnv(t *testing.T, mutate func(*Deps)) *env {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "gateway.db")
	db, err := sqlite.Open(sqlite.DefaultConfig(dbPath))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	// 用真实迁移建库（含 0003_gateway.sql），贴近生产形状。
	if _, err := migrations.ApplyFromDir(context.Background(), db, filepath.Join("..", "..", "..", "..", "migrations")); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	st := store.New(db)
	accts := account.New(st, nil)
	sec, err := secrets.NewManager(&memBackend{values: map[string]string{}})
	if err != nil {
		t.Fatalf("secrets manager: %v", err)
	}
	d := Deps{
		Store:      st,
		Accounts:   accts,
		Quota:      quota.New(st),
		Secrets:    sec,
		AdminToken: testAdminToken,
	}
	if mutate != nil {
		mutate(&d)
	}
	mux := http.NewServeMux()
	for _, rt := range Routes(d) {
		mux.HandleFunc(rt.Pattern, rt.Handler)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return &env{t: t, dir: dir, db: db, store: st, accounts: accts, secrets: sec, srv: srv}
}

type httpResp struct {
	status int
	body   map[string]any
	raw    string
}

func (e *env) doAs(method, path, token string, body any) httpResp {
	e.t.Helper()
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			e.t.Fatalf("marshal body: %v", err)
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequest(method, e.srv.URL+path, rdr)
	if err != nil {
		e.t.Fatalf("new request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("X-Admin-Token", token)
	}
	req.Header.Set("X-Forwarded-For", testClientIP)
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatalf("read body: %v", err)
	}
	out := httpResp{status: resp.StatusCode, raw: string(raw)}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out.body); err != nil {
			e.t.Fatalf("%s %s 响应不是 JSON: %s", method, path, string(raw))
		}
	}
	return out
}

func (e *env) do(method, path string, body any) httpResp {
	e.t.Helper()
	return e.doAs(method, path, testAdminToken, body)
}

// seedUser 直接写库建用户（不经过管理 API，避免污染审计计数）。
func (e *env) seedUser(username string) string {
	e.t.Helper()
	id := "usr_" + username
	now := time.Now().UnixMilli()
	u := model.User{
		ID: id, Username: username,
		PasswordHash: "pbkdf2-sha256$210000$c2FsdA==$aGFzaA==",
		Status:       model.UserStatusActive, GroupID: "default",
		CreatedAt: now, UpdatedAt: now,
	}
	if err := e.store.CreateUser(context.Background(), u); err != nil {
		e.t.Fatalf("seed user %s: %v", username, err)
	}
	return id
}

type auditRow struct {
	Actor  string
	Action string
	Target string
	Result string
	IP     string
	Detail string
}

func (e *env) auditRows(action string) []auditRow {
	e.t.Helper()
	rows, err := e.db.QueryContext(context.Background(),
		`SELECT actor, action, target, result, ip, detail_json FROM gw_audit
		 WHERE action=? ORDER BY created_at ASC, rowid ASC`, action)
	if err != nil {
		e.t.Fatalf("query audit: %v", err)
	}
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var r auditRow
		if err := rows.Scan(&r.Actor, &r.Action, &r.Target, &r.Result, &r.IP, &r.Detail); err != nil {
			e.t.Fatalf("scan audit: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		e.t.Fatalf("iterate audit: %v", err)
	}
	return out
}

func (e *env) countAudit() int {
	e.t.Helper()
	var n int
	if err := e.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM gw_audit`).Scan(&n); err != nil {
		e.t.Fatalf("count audit: %v", err)
	}
	return n
}

func (e *env) countRows(query string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.db.QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		e.t.Fatalf("count %q: %v", query, err)
	}
	return n
}

// assertDBFilesDoNotContain 直接扫描库文件（含 WAL/SHM）：明文密钥绝不允许以任何
// 形式落盘，这比逐个字段断言更接近安全红线本身的要求。
func (e *env) assertDBFilesDoNotContain(needle string) {
	e.t.Helper()
	entries, err := os.ReadDir(e.dir)
	if err != nil {
		e.t.Fatalf("read dir: %v", err)
	}
	checked := 0
	for _, ent := range entries {
		name := ent.Name()
		if !strings.HasPrefix(name, "gateway.db") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(e.dir, name))
		if err != nil {
			e.t.Fatalf("read %s: %v", name, err)
		}
		checked++
		if bytes.Contains(data, []byte(needle)) {
			e.t.Fatalf("明文密钥出现在数据库文件 %s 里", name)
		}
	}
	if checked == 0 {
		e.t.Fatalf("没有找到任何数据库文件（%s）", e.dir)
	}
}

func clone(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func errorCode(t *testing.T, r httpResp) string {
	t.Helper()
	errObj, ok := r.body["error"].(map[string]any)
	if !ok {
		t.Fatalf("响应没有 error 对象: %s", r.raw)
	}
	code, _ := errObj["code"].(string)
	return code
}

// ---------------------------------------------------------------- 鉴权

func TestAdminTokenRequired(t *testing.T) {
	e := newEnv(t, nil)
	cases := []struct {
		name  string
		token string
		want  int
	}{
		{"缺失令牌", "", http.StatusUnauthorized},
		{"错误令牌", "not-the-token", http.StatusUnauthorized},
		{"长度接近的错误令牌", testAdminToken[:len(testAdminToken)-1] + "x", http.StatusUnauthorized},
		{"正确令牌", testAdminToken, http.StatusOK},
	}
	for _, tc := range cases {
		r := e.doAs(http.MethodGet, "/admin/users", tc.token, nil)
		if r.status != tc.want {
			t.Fatalf("%s: status = %d, want %d (%s)", tc.name, r.status, tc.want, r.raw)
		}
		if tc.want == http.StatusUnauthorized && errorCode(t, r) != "unauthorized" {
			t.Errorf("%s: code = %q, want unauthorized", tc.name, errorCode(t, r))
		}
	}
	// 401 路径不该写库（否则可用未鉴权请求放大写入）。
	if n := e.countAudit(); n != 0 {
		t.Errorf("鉴权失败的请求产生了 %d 条审计行，want 0", n)
	}

	// 没配 AdminToken 时管理面整体关闭。
	e2 := newEnv(t, func(d *Deps) { d.AdminToken = "" })
	if r := e2.doAs(http.MethodGet, "/admin/users", "", nil); r.status != http.StatusUnauthorized {
		t.Errorf("未配置令牌时 status = %d, want 401", r.status)
	}
	if r := e2.doAs(http.MethodGet, "/admin/users", testAdminToken, nil); r.status != http.StatusUnauthorized {
		t.Errorf("未配置令牌时带任意令牌 status = %d, want 401", r.status)
	}
}

func TestDepsMissingFailsClosed(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.Quota = nil })
	r := e.do(http.MethodGet, "/admin/users", nil)
	if r.status != http.StatusInternalServerError {
		t.Errorf("Quota 未装配时 status = %d, want 500", r.status)
	}
}

// ---------------------------------------------------------------- 用户

func TestCreateUserHidesPasswordHashAndAudits(t *testing.T) {
	e := newEnv(t, nil)
	const password = "correct-horse-battery"
	r := e.do(http.MethodPost, "/admin/users", map[string]any{
		"username": "alice", "password": password, "group_id": "default",
	})
	if r.status != http.StatusOK {
		t.Fatalf("create user status = %d (%s)", r.status, r.raw)
	}
	if strings.Contains(r.raw, "password_hash") || strings.Contains(r.raw, "pbkdf2") {
		t.Fatalf("响应体泄露了口令哈希: %s", r.raw)
	}
	id, _ := r.body["id"].(string)
	if id == "" || r.body["username"] != "alice" || r.body["status"] != model.UserStatusActive {
		t.Fatalf("响应体缺字段: %s", r.raw)
	}

	var hash string
	if err := e.db.QueryRowContext(context.Background(),
		`SELECT password_hash FROM gw_users WHERE id=?`, id).Scan(&hash); err != nil {
		t.Fatalf("查用户: %v", err)
	}
	if !strings.HasPrefix(hash, "pbkdf2-sha256$") || hash == password {
		t.Fatalf("库中口令不是 PBKDF2 哈希: %q", hash)
	}
	if _, err := e.accounts.Authenticate(context.Background(), "alice", password); err != nil {
		t.Fatalf("口令无法通过认证（说明没走 account.CreateUser）: %v", err)
	}

	rows := e.auditRows(actionUserCreate)
	if len(rows) != 1 {
		t.Fatalf("user.create 审计行数 = %d, want 1", len(rows))
	}
	row := rows[0]
	if row.Actor != actorAdmin || row.Target != id || row.Result != resultOK || row.IP != testClientIP {
		t.Fatalf("审计字段不对: %+v", row)
	}
	if !strings.Contains(row.Detail, `"username":"alice"`) {
		t.Errorf("审计 detail = %s, want 含 username", row.Detail)
	}
	if strings.Contains(row.Detail, password) {
		t.Errorf("审计 detail 含明文口令: %s", row.Detail)
	}

	// 弱口令走服务层校验 → 400，并且也要留审计（失败也要可追溯）。
	r = e.do(http.MethodPost, "/admin/users", map[string]any{"username": "bob", "password": "short"})
	if r.status != http.StatusBadRequest {
		t.Fatalf("弱口令 status = %d, want 400 (%s)", r.status, r.raw)
	}
	rows = e.auditRows(actionUserCreate)
	if len(rows) != 2 || rows[1].Result != resultError {
		t.Fatalf("失败路径审计不对: %+v", rows)
	}

	// 重名 → 409。
	r = e.do(http.MethodPost, "/admin/users", map[string]any{"username": "alice", "password": password})
	if r.status != http.StatusConflict {
		t.Fatalf("重名 status = %d, want 409 (%s)", r.status, r.raw)
	}
}

func TestSetUserStatus(t *testing.T) {
	e := newEnv(t, nil)
	id := e.seedUser("carol")

	r := e.do(http.MethodPost, "/admin/users/"+id+"/status", map[string]any{"status": model.UserStatusDisabled})
	if r.status != http.StatusOK {
		t.Fatalf("set status = %d (%s)", r.status, r.raw)
	}
	if r.body["status"] != model.UserStatusDisabled {
		t.Fatalf("响应 status = %v, want disabled", r.body["status"])
	}
	var status string
	if err := e.db.QueryRowContext(context.Background(), `SELECT status FROM gw_users WHERE id=?`, id).Scan(&status); err != nil {
		t.Fatalf("查用户: %v", err)
	}
	if status != model.UserStatusDisabled {
		t.Errorf("库中 status = %q, want disabled", status)
	}
	rows := e.auditRows(actionUserStatus)
	if len(rows) != 1 || rows[0].Target != id || rows[0].Result != resultOK {
		t.Fatalf("user.status 审计不对: %+v", rows)
	}

	// 非法状态值必须 400（不能写进库）。
	r = e.do(http.MethodPost, "/admin/users/"+id+"/status", map[string]any{"status": "banana"})
	if r.status != http.StatusBadRequest {
		t.Fatalf("非法 status = %d, want 400 (%s)", r.status, r.raw)
	}
	if rows = e.auditRows(actionUserStatus); len(rows) != 2 || rows[1].Result != resultError {
		t.Fatalf("失败审计不对: %+v", rows)
	}

	// 不存在的用户 → 404。
	r = e.do(http.MethodPost, "/admin/users/no-such-user/status", map[string]any{"status": model.UserStatusActive})
	if r.status != http.StatusNotFound {
		t.Fatalf("不存在用户 status = %d, want 404 (%s)", r.status, r.raw)
	}
}

// ---------------------------------------------------------------- 额度

func TestQuotaAdjustIdempotency(t *testing.T) {
	e := newEnv(t, nil)
	userID := e.seedUser("dave")
	body := map[string]any{
		"user_id": userID, "kind": model.LedgerTopup, "amount": 1_000_000,
		"reason": "首次充值", "idempotency_key": "topup-batch-1",
	}

	first := e.do(http.MethodPost, "/admin/quota/adjust", body)
	if first.status != http.StatusOK {
		t.Fatalf("adjust status = %d (%s)", first.status, first.raw)
	}
	ledger1, _ := first.body["ledger"].(map[string]any)
	firstLedgerID, _ := ledger1["id"].(string)
	if firstLedgerID == "" || ledger1["amount"].(float64) != 1_000_000 {
		t.Fatalf("ledger 响应不对: %s", first.raw)
	}
	acct1, _ := first.body["account"].(map[string]any)
	if acct1["total_amount"].(float64) != 1_000_000 {
		t.Fatalf("首次充值后 total = %v, want 1000000", acct1["total_amount"])
	}

	// 同一 idempotency_key 重复提交：返回既有账本行，不再入账（文档 §7.2 防重复点击）。
	second := e.do(http.MethodPost, "/admin/quota/adjust", body)
	if second.status != http.StatusOK {
		t.Fatalf("重复 adjust status = %d (%s)", second.status, second.raw)
	}
	ledger2, _ := second.body["ledger"].(map[string]any)
	if ledger2["id"] != firstLedgerID {
		t.Fatalf("重复提交返回了新的账本行: %v vs %v", ledger2["id"], firstLedgerID)
	}
	acct2, _ := second.body["account"].(map[string]any)
	if acct2["total_amount"].(float64) != 1_000_000 {
		t.Fatalf("重复提交后 total = %v, want 仍为 1000000（重复入账）", acct2["total_amount"])
	}
	if n := e.countRows(`SELECT COUNT(*) FROM gw_quota_ledger WHERE user_id=?`, userID); n != 1 {
		t.Fatalf("账本行数 = %d, want 1（幂等失效）", n)
	}

	// 缺 idempotency_key → 400，且不入账。
	noKey := clone(body)
	delete(noKey, "idempotency_key")
	r := e.do(http.MethodPost, "/admin/quota/adjust", noKey)
	if r.status != http.StatusBadRequest {
		t.Fatalf("缺幂等键 status = %d, want 400 (%s)", r.status, r.raw)
	}
	if !strings.Contains(r.raw, "idempotency_key") {
		t.Errorf("400 文案未说明缺幂等键: %s", r.raw)
	}
	if n := e.countRows(`SELECT COUNT(*) FROM gw_quota_ledger WHERE user_id=?`, userID); n != 1 {
		t.Fatalf("缺幂等键的请求也入账了：账本行数 = %d", n)
	}

	// 超额扣款 → 402（额度不足，不进账本）。
	debit := map[string]any{
		"user_id": userID, "kind": model.LedgerDebit, "amount": 2_000_000,
		"reason": "测试扣款", "idempotency_key": "debit-1",
	}
	r = e.do(http.MethodPost, "/admin/quota/adjust", debit)
	if r.status != http.StatusPaymentRequired || errorCode(t, r) != "insufficient_quota" {
		t.Fatalf("超额扣款 status = %d code = %q, want 402 insufficient_quota", r.status, errorCode(t, r))
	}

	// 每次尝试都有审计：3 次 adjust（成功、成功、缺键 400）+ 1 次 402。
	rows := e.auditRows(actionQuotaAdjust)
	if len(rows) != 4 {
		t.Fatalf("quota.adjust 审计行数 = %d, want 4: %+v", len(rows), rows)
	}
	if rows[0].Result != resultOK || rows[1].Result != resultOK || rows[2].Result != resultError || rows[3].Result != resultError {
		t.Fatalf("审计 result 序列不对: %+v", rows)
	}
	if rows[0].Target != userID || !strings.Contains(rows[0].Detail, "topup-batch-1") {
		t.Fatalf("审计 target/detail 不对: %+v", rows[0])
	}

	// 账户快照与账本查询。
	r = e.do(http.MethodGet, "/admin/quota/accounts/"+userID, nil)
	if r.status != http.StatusOK || r.body["available"].(float64) != 1_000_000 {
		t.Fatalf("账户快照不对: %d %s", r.status, r.raw)
	}
	r = e.do(http.MethodGet, "/admin/quota/ledger?user_id="+userID, nil)
	data, _ := r.body["data"].([]any)
	if r.status != http.StatusOK || len(data) != 1 {
		t.Fatalf("账本列表不对: %d %s", r.status, r.raw)
	}
	r = e.do(http.MethodGet, "/admin/quota/accounts/no-such-user", nil)
	if r.status != http.StatusNotFound {
		t.Fatalf("不存在账户 status = %d, want 404", r.status)
	}
}

// ---------------------------------------------------------------- Provider

func TestProviderWriteValidation(t *testing.T) {
	e := newEnv(t, nil)
	base := map[string]any{
		"id": "p1", "name": "OpenRouter",
		"endpoint": "https://openrouter.ai/api/v1", "protocol": model.ProtocolOpenAIChat,
	}

	bad := clone(base)
	bad["protocol"] = model.ProtocolAnthropic
	r := e.do(http.MethodPost, "/admin/providers", bad)
	if r.status != http.StatusBadRequest {
		t.Fatalf("非法协议 status = %d, want 400 (%s)", r.status, r.raw)
	}
	if !strings.Contains(r.raw, model.ProtocolOpenAIChat) {
		t.Errorf("400 文案未说明 V1 只支持 openai-chat: %s", r.raw)
	}

	for _, endpoint := range []string{"openrouter.ai/api/v1", "ftp://openrouter.ai/api", "https://", "://x"} {
		bad := clone(base)
		bad["endpoint"] = endpoint
		r := e.do(http.MethodPost, "/admin/providers", bad)
		if r.status != http.StatusBadRequest {
			t.Fatalf("endpoint %q status = %d, want 400 (%s)", endpoint, r.status, r.raw)
		}
	}

	// 缺 endpoint（新建）→ 400。
	noEndpoint := clone(base)
	delete(noEndpoint, "endpoint")
	if r = e.do(http.MethodPost, "/admin/providers", noEndpoint); r.status != http.StatusBadRequest {
		t.Fatalf("缺 endpoint status = %d, want 400 (%s)", r.status, r.raw)
	}

	// 非法请求一律不留行。
	if n := e.countRows(`SELECT COUNT(*) FROM gw_providers`); n != 0 {
		t.Fatalf("非法请求写进了 %d 个 provider", n)
	}

	ok := clone(base)
	ok["weight"] = 10
	r = e.do(http.MethodPost, "/admin/providers", ok)
	if r.status != http.StatusOK {
		t.Fatalf("合法 provider status = %d (%s)", r.status, r.raw)
	}
	if r.body["protocol"] != model.ProtocolOpenAIChat || r.body["status"] != model.ProviderStatusEnabled {
		t.Fatalf("provider 响应不对: %s", r.raw)
	}

	// 局部更新：只改 name，endpoint/protocol 保留。
	r = e.do(http.MethodPost, "/admin/providers", map[string]any{"id": "p1", "name": "OpenRouter CN"})
	if r.status != http.StatusOK || r.body["name"] != "OpenRouter CN" ||
		r.body["endpoint"] != "https://openrouter.ai/api/v1" {
		t.Fatalf("局部更新不对: %d %s", r.status, r.raw)
	}

	// 非法状态值 → 400。
	if r = e.do(http.MethodPost, "/admin/providers", map[string]any{"id": "p1", "status": "paused"}); r.status != http.StatusBadRequest {
		t.Fatalf("非法 status = %d, want 400", r.status)
	}

	// GET 列表与详情。
	r = e.do(http.MethodGet, "/admin/providers", nil)
	data, _ := r.body["data"].([]any)
	if r.status != http.StatusOK || len(data) != 1 || r.body["limit"].(float64) != defaultPageLimit {
		t.Fatalf("provider 列表不对: %d %s", r.status, r.raw)
	}
	r = e.do(http.MethodGet, "/admin/providers/p1", nil)
	if r.status != http.StatusOK || r.body["id"] != "p1" {
		t.Fatalf("provider 详情不对: %d %s", r.status, r.raw)
	}
	r = e.do(http.MethodGet, "/admin/providers/ghost", nil)
	if r.status != http.StatusNotFound {
		t.Fatalf("不存在 provider status = %d, want 404", r.status)
	}

	rows := e.auditRows(actionProviderUpsert)
	if len(rows) != 9 { // 1 非法协议 + 4 非法 endpoint + 1 缺 endpoint + 1 新建 + 1 改名 + 1 非法 status
		t.Fatalf("provider.upsert 审计行数 = %d, want 9: %+v", len(rows), rows)
	}
	okCount := 0
	for _, row := range rows {
		if row.Actor != actorAdmin || row.IP != testClientIP {
			t.Fatalf("审计字段不对: %+v", row)
		}
		if row.Result == resultOK {
			okCount++
		}
	}
	if okCount != 2 {
		t.Fatalf("成功的 provider.upsert 审计 = %d, want 2", okCount)
	}
}

func TestProviderPlaintextKeyNeverPersisted(t *testing.T) {
	e := newEnv(t, nil)
	const plaintext = "sk-live-XIMOplaintext0123456789abcdef"

	r := e.do(http.MethodPost, "/admin/providers", map[string]any{
		"id": "p-openrouter", "name": "OpenRouter",
		"endpoint": "https://openrouter.ai/api/v1", "protocol": model.ProtocolOpenAIChat,
		"api_key_ref": plaintext, "weight": 10,
	})
	if r.status != http.StatusOK {
		t.Fatalf("provider 写入 status = %d (%s)", r.status, r.raw)
	}
	if strings.Contains(r.raw, plaintext) {
		t.Fatalf("响应回显了明文密钥: %s", r.raw)
	}
	ref, _ := r.body["api_key_ref"].(string)
	if !secrets.IsRef(ref) {
		t.Fatalf("api_key_ref = %q, want secretref:v1:...", ref)
	}

	// 库里只存 ref，且文件全文不得出现明文。
	var dbRef, config string
	if err := e.db.QueryRowContext(context.Background(),
		`SELECT api_key_ref, config_json FROM gw_providers WHERE id=?`, "p-openrouter").
		Scan(&dbRef, &config); err != nil {
		t.Fatalf("查 provider: %v", err)
	}
	if dbRef != ref || strings.Contains(dbRef, plaintext) {
		t.Fatalf("库中 api_key_ref = %q, want %q（且不含明文）", dbRef, ref)
	}
	if strings.Contains(config, plaintext) {
		t.Fatalf("config_json 含明文密钥: %q", config)
	}
	e.assertDBFilesDoNotContain(plaintext)

	// 明文确实进了安全存储（证明走的是 secrets 而不是丢弃）。
	if got, err := e.secrets.Get(ref); err != nil || got != plaintext {
		t.Fatalf("安全存储里的值不对: %q, %v", got, err)
	}

	// 审计 detail 与详情接口都不允许出现明文。
	for _, row := range e.auditRows(actionProviderUpsert) {
		if strings.Contains(row.Detail, plaintext) {
			t.Fatalf("审计 detail 含明文密钥: %s", row.Detail)
		}
	}
	r = e.do(http.MethodGet, "/admin/providers/p-openrouter", nil)
	if strings.Contains(r.raw, plaintext) {
		t.Fatalf("详情接口回显了明文密钥: %s", r.raw)
	}

	// api_key 与 api_key_ref 同时给出 → 400（歧义）。
	r = e.do(http.MethodPost, "/admin/providers", map[string]any{
		"id": "p2", "endpoint": "https://api.example.com/v1",
		"api_key": "sk-other-0123456789abcdefghij", "api_key_ref": plaintext,
	})
	if r.status != http.StatusBadRequest {
		t.Fatalf("两处同时给密钥 status = %d, want 400 (%s)", r.status, r.raw)
	}

	// 明文也可以写在 api_key 字段里（同样只落 ref）。
	const plaintext2 = "sk-other-0123456789abcdefghij"
	r = e.do(http.MethodPost, "/admin/providers", map[string]any{
		"id": "p2", "endpoint": "https://api.example.com/v1", "api_key": plaintext2,
	})
	if r.status != http.StatusOK || strings.Contains(r.raw, plaintext2) {
		t.Fatalf("api_key 字段处理不对: %d %s", r.status, r.raw)
	}
	e.assertDBFilesDoNotContain(plaintext2)

	// Secrets 未装配时必须失败，绝不退化为明文落库。
	e2 := newEnv(t, func(d *Deps) { d.Secrets = nil })
	r = e2.do(http.MethodPost, "/admin/providers", map[string]any{
		"id": "p1", "endpoint": "https://api.example.com/v1", "api_key": plaintext2,
	})
	if r.status != http.StatusInternalServerError {
		t.Fatalf("Secrets 未装配时 status = %d, want 500 (%s)", r.status, r.raw)
	}
	if n := e2.countRows(`SELECT COUNT(*) FROM gw_providers`); n != 0 {
		t.Fatalf("Secrets 未装配时仍写了 %d 行 provider", n)
	}
}

// ---------------------------------------------------------------- 模型

func TestModelCapabilitiesValidation(t *testing.T) {
	e := newEnv(t, nil)
	for _, bad := range []string{`[1,2]`, `{oops`, `"nope"`, `123`} {
		r := e.do(http.MethodPost, "/admin/models", map[string]any{"id": "m1", "capabilities_json": bad})
		if r.status != http.StatusBadRequest {
			t.Fatalf("capabilities_json %s status = %d, want 400 (%s)", bad, r.status, r.raw)
		}
	}
	if n := e.countRows(`SELECT COUNT(*) FROM gw_models`); n != 0 {
		t.Fatalf("非法 capabilities 写进了 %d 行模型", n)
	}

	r := e.do(http.MethodPost, "/admin/models", map[string]any{
		"id": "gpt-4o", "display_name": "GPT-4o",
		"capabilities": map[string]any{"stream": true, "tools": true},
	})
	if r.status != http.StatusOK {
		t.Fatalf("模型写入 status = %d (%s)", r.status, r.raw)
	}
	caps, _ := r.body["capabilities"].(map[string]any)
	if caps["stream"] != true || caps["tools"] != true {
		t.Fatalf("capabilities 响应不对: %s", r.raw)
	}

	r = e.do(http.MethodGet, "/admin/models/gpt-4o", nil)
	caps, _ = r.body["capabilities"].(map[string]any)
	if r.status != http.StatusOK || r.body["display_name"] != "GPT-4o" || caps["stream"] != true {
		t.Fatalf("模型详情不对: %d %s", r.status, r.raw)
	}
	if r = e.do(http.MethodGet, "/admin/models/ghost", nil); r.status != http.StatusNotFound {
		t.Fatalf("不存在模型 status = %d, want 404", r.status)
	}

	// 局部更新：只改 enabled，其余保留。
	r = e.do(http.MethodPost, "/admin/models", map[string]any{"id": "gpt-4o", "enabled": false})
	if r.status != http.StatusOK || r.body["enabled"] != false || r.body["display_name"] != "GPT-4o" {
		t.Fatalf("模型局部更新不对: %d %s", r.status, r.raw)
	}
	caps, _ = r.body["capabilities"].(map[string]any)
	if caps["stream"] != true {
		t.Fatalf("局部更新丢了 capabilities: %s", r.raw)
	}

	// capabilities_json 字符串形式同样接受；enabled=true 过滤。
	r = e.do(http.MethodPost, "/admin/models", map[string]any{"id": "m2", "capabilities_json": `{"vision":true}`})
	if r.status != http.StatusOK {
		t.Fatalf("capabilities_json 字符串形式 status = %d (%s)", r.status, r.raw)
	}
	r = e.do(http.MethodGet, "/admin/models?enabled=true", nil)
	data, _ := r.body["data"].([]any)
	if len(data) != 1 || data[0].(map[string]any)["id"] != "m2" {
		t.Fatalf("enabled 过滤不对: %s", r.raw)
	}

	rows := e.auditRows(actionModelUpsert)
	if len(rows) != 7 { // 4 非法 + 1 新建 + 1 局部更新 + 1 字符串形式
		t.Fatalf("model.upsert 审计行数 = %d, want 7: %+v", len(rows), rows)
	}
}

// ---------------------------------------------------------------- 密钥与设备码

func TestAPIKeyLifecycle(t *testing.T) {
	e := newEnv(t, nil)
	userID := e.seedUser("erin")

	r := e.do(http.MethodPost, "/admin/keys", map[string]any{"user_id": userID})
	if r.status != http.StatusOK {
		t.Fatalf("签密钥 status = %d (%s)", r.status, r.raw)
	}
	plain, _ := r.body["api_key"].(string)
	if !strings.HasPrefix(plain, "ximo_sk_") || len(plain) < 20 {
		t.Fatalf("明文密钥形状不对: %q", plain)
	}
	key, _ := r.body["key"].(map[string]any)
	keyID, _ := key["id"].(string)
	if keyID == "" || key["user_id"] != userID {
		t.Fatalf("key 响应不对: %s", r.raw)
	}

	// 明文只回一次，库里只有 sha256，库文件里不得出现明文。
	var hash, prefix, status string
	if err := e.db.QueryRowContext(context.Background(),
		`SELECT key_hash, key_prefix, status FROM gw_api_keys WHERE id=?`, keyID).
		Scan(&hash, &prefix, &status); err != nil {
		t.Fatalf("查密钥: %v", err)
	}
	if hash == plain || len(hash) != 64 || status != model.KeyStatusActive {
		t.Fatalf("库中密钥不对: hash=%q status=%q", hash, status)
	}
	if prefix != key["key_prefix"] || prefix == plain {
		t.Fatalf("key_prefix = %q, want %v", prefix, key["key_prefix"])
	}
	e.assertDBFilesDoNotContain(plain)
	for _, row := range e.auditRows(actionKeyCreate) {
		if strings.Contains(row.Detail, plain) {
			t.Fatalf("审计 detail 含明文 API Key: %s", row.Detail)
		}
	}

	// ttl_seconds 为负 → 400。
	if r = e.do(http.MethodPost, "/admin/keys", map[string]any{"user_id": userID, "ttl_seconds": -1}); r.status != http.StatusBadRequest {
		t.Fatalf("负 ttl status = %d, want 400", r.status)
	}

	// 列表。
	r = e.do(http.MethodGet, "/admin/keys?user_id="+userID, nil)
	data, _ := r.body["data"].([]any)
	if r.status != http.StatusOK || len(data) != 1 {
		t.Fatalf("密钥列表不对: %d %s", r.status, r.raw)
	}
	if r = e.do(http.MethodGet, "/admin/keys", nil); r.status != http.StatusBadRequest {
		t.Fatalf("缺 user_id status = %d, want 400", r.status)
	}

	// 撤销。
	r = e.do(http.MethodDelete, "/admin/keys/"+keyID, nil)
	if r.status != http.StatusOK {
		t.Fatalf("撤销 status = %d (%s)", r.status, r.raw)
	}
	if err := e.db.QueryRowContext(context.Background(), `SELECT status FROM gw_api_keys WHERE id=?`, keyID).Scan(&status); err != nil {
		t.Fatalf("查密钥: %v", err)
	}
	if status != model.KeyStatusRevoked {
		t.Errorf("库中 status = %q, want revoked", status)
	}
	if r = e.do(http.MethodDelete, "/admin/keys/ghost", nil); r.status != http.StatusNotFound {
		t.Fatalf("撤销不存在的密钥 status = %d, want 404", r.status)
	}
	if rows := e.auditRows(actionKeyRevoke); len(rows) != 2 || rows[0].Result != resultOK || rows[1].Result != resultError {
		t.Fatalf("key.revoke 审计不对: %+v", rows)
	}
}

func TestDeviceApprove(t *testing.T) {
	e := newEnv(t, nil)
	userID := e.seedUser("frank")

	_, userCode, _, err := e.accounts.StartDeviceLogin(context.Background())
	if err != nil {
		t.Fatalf("start device login: %v", err)
	}

	r := e.do(http.MethodPost, "/admin/device/approve", map[string]any{"user_code": userCode, "user_id": userID})
	if r.status != http.StatusOK {
		t.Fatalf("批准设备码 status = %d (%s)", r.status, r.raw)
	}
	d, err := e.store.GetDeviceCodeByUserCode(context.Background(), userCode)
	if err != nil {
		t.Fatalf("查设备码: %v", err)
	}
	if d.Status != model.DeviceStatusApproved || d.UserID != userID {
		t.Fatalf("设备码状态不对: %+v", d)
	}

	// 重复批准 → 409（account 语义），不存在 → 404。
	if r = e.do(http.MethodPost, "/admin/device/approve", map[string]any{"user_code": userCode, "user_id": userID}); r.status != http.StatusConflict {
		t.Fatalf("重复批准 status = %d, want 409 (%s)", r.status, r.raw)
	}
	if r = e.do(http.MethodPost, "/admin/device/approve", map[string]any{"user_code": "ZZZZ9999", "user_id": userID}); r.status != http.StatusNotFound {
		t.Fatalf("不存在的用户码 status = %d, want 404 (%s)", r.status, r.raw)
	}
	if r = e.do(http.MethodPost, "/admin/device/approve", map[string]any{"user_id": userID}); r.status != http.StatusBadRequest {
		t.Fatalf("缺 user_code status = %d, want 400", r.status)
	}
	if rows := e.auditRows(actionDeviceApprove); len(rows) != 4 {
		t.Fatalf("device.approve 审计行数 = %d, want 4: %+v", len(rows), rows)
	}
}

// ---------------------------------------------------------------- 分页与用量

func TestPaginationBounds(t *testing.T) {
	e := newEnv(t, nil)
	for i := 0; i < 3; i++ {
		e.seedUser(fmt.Sprintf("page-%d", i))
	}

	r := e.do(http.MethodGet, "/admin/users", nil)
	data, _ := r.body["data"].([]any)
	if r.status != http.StatusOK || len(data) != 3 {
		t.Fatalf("默认分页不对: %d %s", r.status, r.raw)
	}
	if r.body["limit"].(float64) != defaultPageLimit || r.body["offset"].(float64) != 0 {
		t.Fatalf("默认 limit/offset 不对: %s", r.raw)
	}

	r = e.do(http.MethodGet, "/admin/users?limit=2&offset=1", nil)
	data, _ = r.body["data"].([]any)
	if len(data) != 2 || r.body["limit"].(float64) != 2 || r.body["offset"].(float64) != 1 {
		t.Fatalf("分页切页不对: %s", r.raw)
	}

	if r = e.do(http.MethodGet, "/admin/users?limit=500", nil); r.status != http.StatusOK {
		t.Fatalf("limit=500 status = %d, want 200", r.status)
	}
	for _, q := range []string{"limit=0", "limit=501", "limit=abc", "limit=-1", "offset=-1", "offset=x"} {
		r = e.do(http.MethodGet, "/admin/users?"+q, nil)
		if r.status != http.StatusBadRequest {
			t.Fatalf("%s status = %d, want 400 (%s)", q, r.status, r.raw)
		}
	}

	// 越界 offset 回空数组而不是 null。
	r = e.do(http.MethodGet, "/admin/users?offset=100", nil)
	data, _ = r.body["data"].([]any)
	if r.status != http.StatusOK || data == nil || len(data) != 0 {
		t.Fatalf("越界 offset 响应不对: %s", r.raw)
	}
}

func TestListUsageAndAudit(t *testing.T) {
	e := newEnv(t, nil)
	userID := e.seedUser("gina")
	now := time.Now().UnixMilli()
	if err := e.store.InsertUsage(context.Background(), model.UsageRecord{
		RequestID: "req-1", UserID: userID, ModelID: "gpt-4o", ProviderID: "p1",
		Status: "ok", InputTokens: 10, OutputTokens: 5, CostMicro: 7, CreatedAt: now,
	}); err != nil {
		t.Fatalf("insert usage: %v", err)
	}

	r := e.do(http.MethodGet, "/admin/usage?user_id="+userID, nil)
	data, _ := r.body["data"].([]any)
	if r.status != http.StatusOK || len(data) != 1 {
		t.Fatalf("usage 列表不对: %d %s", r.status, r.raw)
	}
	if r = e.do(http.MethodGet, "/admin/usage", nil); r.status != http.StatusBadRequest {
		t.Fatalf("缺 user_id 的 usage status = %d, want 400", r.status)
	}

	// GET 不产生审计行（只有写操作才留痕）。
	if n := e.countAudit(); n != 0 {
		t.Fatalf("只读操作产生了 %d 条审计行", n)
	}

	if r = e.do(http.MethodPost, "/admin/users", map[string]any{"username": "hank", "password": "long-enough-password"}); r.status != http.StatusOK {
		t.Fatalf("create user: %d %s", r.status, r.raw)
	}
	r = e.do(http.MethodGet, "/admin/audit?limit=10", nil)
	data, _ = r.body["data"].([]any)
	if r.status != http.StatusOK || len(data) != 1 {
		t.Fatalf("audit 列表不对: %d %s", r.status, r.raw)
	}
	row := data[0].(map[string]any)
	if row["actor"] != actorAdmin || row["action"] != actionUserCreate || row["ip"] != testClientIP {
		t.Fatalf("audit 行不对: %s", r.raw)
	}
	if _, ok := row["detail"].(map[string]any); !ok {
		t.Fatalf("audit detail 未解析成对象: %s", r.raw)
	}
}

// ---------------------------------------------------------------- 其它

func TestMalformedBodyStillAudited(t *testing.T) {
	e := newEnv(t, nil)
	req, err := http.NewRequest(http.MethodPost, e.srv.URL+"/admin/users", strings.NewReader("{not json"))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-Admin-Token", testAdminToken)
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("坏请求体 status = %d, want 400", resp.StatusCode)
	}
	rows := e.auditRows(actionUserCreate)
	if len(rows) != 1 || rows[0].Result != resultError {
		t.Fatalf("坏请求体的失败审计不对: %+v", rows)
	}
}

func TestRoutesCoverContract(t *testing.T) {
	want := []string{
		"POST /admin/users", "GET /admin/users", "POST /admin/users/{id}/status",
		"POST /admin/keys", "GET /admin/keys", "DELETE /admin/keys/{id}",
		"POST /admin/quota/adjust", "GET /admin/quota/accounts/{user_id}", "GET /admin/quota/ledger",
		"GET /admin/models", "POST /admin/models", "GET /admin/models/{id}",
		"GET /admin/providers", "POST /admin/providers", "GET /admin/providers/{id}",
		"POST /admin/providers/{id}/models",
		"GET /admin/usage", "GET /admin/audit", "POST /admin/device/approve",
	}
	got := map[string]bool{}
	for _, rt := range Routes(Deps{}) {
		got[rt.Pattern] = true
	}
	if len(got) != len(want) {
		t.Fatalf("路由数 = %d, want %d", len(got), len(want))
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("缺路由 %q", w)
		}
	}
}

func TestProviderModelMapping(t *testing.T) {
	e := newEnv(t, nil)
	if r := e.do(http.MethodPost, "/admin/models", map[string]any{"id": "m1"}); r.status != http.StatusOK {
		t.Fatalf("写模型: %d %s", r.status, r.raw)
	}
	if r := e.do(http.MethodPost, "/admin/providers", map[string]any{
		"id": "p1", "endpoint": "https://api.example.com/v1"}); r.status != http.StatusOK {
		t.Fatalf("写 provider: %d %s", r.status, r.raw)
	}

	r := e.do(http.MethodPost, "/admin/providers/p1/models", map[string]any{
		"model_id": "m1", "upstream_model_id": "vendor/m1", "priority": 1})
	if r.status != http.StatusOK || r.body["upstream_model_id"] != "vendor/m1" {
		t.Fatalf("映射 status = %d (%s)", r.status, r.raw)
	}
	pms, err := e.store.ListProviderModels(context.Background(), "m1")
	if err != nil || len(pms) != 1 || pms[0].UpstreamModelID != "vendor/m1" {
		t.Fatalf("映射未落库: %+v %v", pms, err)
	}

	// model 不存在 → 404；provider 不存在 → 404；缺 upstream_model_id → 400。
	if r = e.do(http.MethodPost, "/admin/providers/p1/models", map[string]any{
		"model_id": "ghost", "upstream_model_id": "x"}); r.status != http.StatusNotFound {
		t.Fatalf("不存在模型 status = %d, want 404 (%s)", r.status, r.raw)
	}
	if r = e.do(http.MethodPost, "/admin/providers/ghost/models", map[string]any{
		"model_id": "m1", "upstream_model_id": "x"}); r.status != http.StatusNotFound {
		t.Fatalf("不存在 provider status = %d, want 404 (%s)", r.status, r.raw)
	}
	if r = e.do(http.MethodPost, "/admin/providers/p1/models", map[string]any{"model_id": "m1"}); r.status != http.StatusBadRequest {
		t.Fatalf("缺 upstream_model_id status = %d, want 400 (%s)", r.status, r.raw)
	}
	if rows := e.auditRows(actionProviderModelUpsert); len(rows) != 4 {
		t.Fatalf("provider.model.upsert 审计行数 = %d, want 4: %+v", len(rows), rows)
	}
}

func TestPickIDAndConfigHelpers(t *testing.T) {
	if _, err := pickID("a", "b"); err == nil {
		t.Error("id 与 model_id 冲突时应报错")
	}
	if id, err := pickID("", "m1"); err != nil || id != "m1" {
		t.Errorf("别名取值失败: %q %v", id, err)
	}
	if _, err := pickID("", ""); err == nil {
		t.Error("空 ID 应报错")
	}
	cfg, provided, err := pickConfig(nil, nil)
	if err != nil || provided || cfg != "" {
		t.Errorf("空 config 不该标记为已提供: %q %v %v", cfg, provided, err)
	}
	cfg, provided, err = pickConfig(json.RawMessage(`{"region":"cn"}`), nil)
	if err != nil || !provided || cfg != `{"region":"cn"}` {
		t.Errorf("config 归一化失败: %q %v %v", cfg, provided, err)
	}
	if _, _, err = pickConfig(json.RawMessage(`[1]`), nil); err == nil {
		t.Error("数组 config 应被拒")
	}
	status, code := classify(badRequest("x"))
	if status != http.StatusBadRequest || code != "invalid_request_error" {
		t.Errorf("classify(badRequest) = %d %q", status, code)
	}
	if status, code = classify(model.ErrNotFound); status != http.StatusNotFound {
		t.Errorf("classify(ErrNotFound) = %d %q", status, code)
	}
	if status, code = classify(errors.New("boom")); status != http.StatusInternalServerError || code != "server_error" {
		t.Errorf("classify(unknown) = %d %q", status, code)
	}
}
