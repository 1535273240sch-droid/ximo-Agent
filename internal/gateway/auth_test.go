package gateway

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/httpx"
	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/model"
)

// ---------------------------------------------------------------------------
// 假凭据校验器：只回放预设结果，并记录走了哪条校验路径
// ---------------------------------------------------------------------------

type fakeVerifier struct {
	mu        sync.Mutex
	calls     []string
	user      model.User
	key       model.APIKey
	apiKeyErr error
	accessErr error
}

func (f *fakeVerifier) VerifyAPIKey(_ context.Context, _ string) (model.User, model.APIKey, error) {
	f.record("api_key")
	if f.apiKeyErr != nil {
		return model.User{}, model.APIKey{}, f.apiKeyErr
	}
	return f.user, f.key, nil
}

func (f *fakeVerifier) VerifyAccess(_ context.Context, _ string) (model.User, error) {
	f.record("access_token")
	if f.accessErr != nil {
		return model.User{}, f.accessErr
	}
	return f.user, nil
}

func (f *fakeVerifier) record(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name)
}

func (f *fakeVerifier) callsSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// activeUser 是默认的可用用户/密钥。
func activeUser() (model.User, model.APIKey) {
	return model.User{ID: "u_1", Username: "alice", Status: model.UserStatusActive},
		model.APIKey{ID: "k_1", UserID: "u_1", Status: model.KeyStatusActive}
}

func newAuthWithUser() (*Authenticator, *fakeVerifier, *logCapture) {
	u, k := activeUser()
	fv := &fakeVerifier{user: u, key: k}
	logger, cap := testLogger()
	return NewAuthenticator(fv, "test-admin-token", logger), fv, cap
}

func okHandler(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"ok": "1"})
}

// ---------------------------------------------------------------------------
// 用户态凭据
// ---------------------------------------------------------------------------

func TestAuthenticateDispatchesByPrefix(t *testing.T) {
	const apiKey = "ximo_sk_0123456789abcdef0123456789abcdef"
	const access = "gwa_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	t.Run("API Key 前缀", func(t *testing.T) {
		auth, fv, _ := newAuthWithUser()
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+apiKey)
		p, err := auth.Authenticate(req)
		if err != nil {
			t.Fatalf("Authenticate: %v", err)
		}
		if p.User.ID != "u_1" || p.Key.ID != "k_1" {
			t.Fatalf("主体不符: %+v", p)
		}
		if p.Kind != PrincipalUser || p.Credential != CredAPIKey {
			t.Fatalf("凭据种类不符: %+v", p)
		}
		if calls := fv.callsSnapshot(); len(calls) != 1 || calls[0] != "api_key" {
			t.Fatalf("应走 VerifyAPIKey，实际 %v", calls)
		}
	})

	t.Run("access token 前缀", func(t *testing.T) {
		auth, fv, _ := newAuthWithUser()
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+access)
		p, err := auth.Authenticate(req)
		if err != nil {
			t.Fatalf("Authenticate: %v", err)
		}
		if p.Credential != CredAccessToken || p.Key.ID != "" {
			t.Fatalf("凭据种类不符: %+v", p)
		}
		if calls := fv.callsSnapshot(); len(calls) != 1 || calls[0] != "access_token" {
			t.Fatalf("应走 VerifyAccess，实际 %v", calls)
		}
	})

	t.Run("未知前缀与缺失头不调校验器", func(t *testing.T) {
		auth, fv, _ := newAuthWithUser()
		for _, hdr := range []string{"", "Bearer sk-unknown-format", "Basic abc", "Bearer gwr_refresh_token"} {
			req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			if hdr != "" {
				req.Header.Set("Authorization", hdr)
			}
			if _, err := auth.Authenticate(req); !errors.Is(err, model.ErrBadCredentials) {
				t.Fatalf("Authorization=%q 应回 ErrBadCredentials，得到 %v", hdr, err)
			}
		}
		if calls := fv.callsSnapshot(); len(calls) != 0 {
			t.Fatalf("非本产品前缀不应进校验器，实际 %v", calls)
		}
	})
}

func TestRequireUserErrorMapping(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantCode int
		wantErr  string
	}{
		{"凭据无效", model.ErrBadCredentials, http.StatusUnauthorized, "invalid_api_key"},
		{"过期", model.ErrExpired, http.StatusUnauthorized, "credential_expired"},
		{"吊销", model.ErrRevoked, http.StatusUnauthorized, "credential_revoked"},
		{"用户禁用", model.ErrDisabled, http.StatusForbidden, "account_disabled"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fv := &fakeVerifier{apiKeyErr: tc.err}
			logger, _ := testLogger()
			auth := NewAuthenticator(fv, "test-admin-token", logger)

			req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			req.Header.Set("Authorization", "Bearer ximo_sk_0123456789abcdef0123456789abcdef")
			rec := httptest.NewRecorder()
			auth.RequireUser(http.HandlerFunc(okHandler)).ServeHTTP(rec, req)

			if rec.Code != tc.wantCode {
				t.Fatalf("状态码应为 %d，得到 %d（%s）", tc.wantCode, rec.Code, rec.Body.String())
			}
			detail := jsonBody(t, rec)["error"].(map[string]any)
			if detail["code"] != tc.wantErr {
				t.Fatalf("错误码应为 %s，得到 %v", tc.wantErr, detail["code"])
			}
		})
	}
}

func TestRequireUserInjectsPrincipalAndLogsRedactedCredential(t *testing.T) {
	const secret = "ximo_sk_0123456789abcdef0123456789abcdef"
	auth, _, _ := newAuthWithUser()

	var seen Principal
	var ok bool
	h := auth.RequireUser(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, ok = PrincipalFrom(r.Context())
		okHandler(w, r)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，得到 %d", rec.Code)
	}
	if !ok || seen.User.ID != "u_1" || seen.RateKey() != "key:k_1" {
		t.Fatalf("context 里的主体不符: %+v", seen)
	}

	// 失败路径的日志：只允许出现「前缀 + 长度」。
	failing := &fakeVerifier{apiKeyErr: model.ErrExpired}
	logger, failLogs := testLogger()
	failAuth := NewAuthenticator(failing, "test-admin-token", logger)
	req2 := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req2.Header.Set("Authorization", "Bearer "+secret)
	failAuth.RequireUser(http.HandlerFunc(okHandler)).ServeHTTP(httptest.NewRecorder(), req2)
	fields := failLogs.find(t, "gateway: 用户凭据校验失败")
	if got, want := fields["credential_hint"], fmt.Sprintf("ximo_sk_…(len=%d)", len(secret)); got != want {
		t.Fatalf("凭据提示应为 %q，得到 %v", want, got)
	}
	if strings.Contains(failLogs.String(), secret) {
		t.Fatal("失败日志不得包含凭据明文")
	}
	if !strings.Contains(fmt.Sprint(fields["reason"]), "expired") {
		t.Fatalf("日志应带上失败原因: %v", fields["reason"])
	}
}

func TestRequireUserRoutesWrapsEveryRoute(t *testing.T) {
	auth, _, _ := newAuthWithUser()
	routes := auth.RequireUserRoutes([]httpx.Route{
		{Pattern: "GET /v1/models", Handler: okHandler},
		{Pattern: "GET /v1/usage", Handler: okHandler},
		{Pattern: "POST /v1/chat/completions", Handler: nil}, // 保持原样，装配期会报错
	})
	if len(routes) != 3 {
		t.Fatalf("路由数量应保持，得到 %d", len(routes))
	}
	if routes[0].Pattern != "GET /v1/models" || routes[2].Handler != nil {
		t.Fatalf("pattern 必须原样保留: %+v", routes)
	}
	rec := httptest.NewRecorder()
	routes[0].Handler(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("包装后的路由应要求凭据，得到 %d", rec.Code)
	}
}

func TestPrincipalRateKeyAndContext(t *testing.T) {
	cases := []struct {
		name string
		p    Principal
		want string
	}{
		{"密钥优先", Principal{Kind: PrincipalUser, User: model.User{ID: "u1"}, Key: model.APIKey{ID: "k1"}}, "key:k1"},
		{"回退到用户", Principal{Kind: PrincipalUser, User: model.User{ID: "u1"}}, "user:u1"},
		{"管理主体", Principal{Kind: PrincipalAdmin, Credential: CredAdminToken}, "admin"},
		{"无主体", Principal{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.p.RateKey(); got != tc.want {
				t.Fatalf("RateKey = %q，期望 %q", got, tc.want)
			}
		})
	}

	ctx := WithPrincipal(context.Background(), Principal{Kind: PrincipalUser, User: model.User{ID: "u9"}})
	if p, ok := PrincipalFrom(ctx); !ok || p.UserID() != "u9" {
		t.Fatalf("PrincipalFrom 往返失败: %+v %v", p, ok)
	}
	if _, ok := PrincipalFrom(context.Background()); ok {
		t.Fatal("未注入主体时不应报告 ok")
	}
	if _, ok := PrincipalFrom(nil); ok {
		t.Fatal("nil context 不应 panic，也不应报告 ok")
	}
}

// ---------------------------------------------------------------------------
// 管理态令牌
// ---------------------------------------------------------------------------

func TestRequireAdminHappyPathAndPrincipal(t *testing.T) {
	const token = "test-admin-token"
	fv := &fakeVerifier{}
	logger, _ := testLogger()
	auth := NewAuthenticator(fv, token, logger)
	if !auth.AdminTokenConfigured() {
		t.Fatal("已配置令牌应报告 configured")
	}
	if auth.adminDigest != sha256.Sum256([]byte(token)) {
		t.Fatal("应只保留令牌摘要（明文不驻留）")
	}

	var p Principal
	h := auth.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ = PrincipalFrom(r.Context())
		okHandler(w, r)
	}))
	req := httptest.NewRequest(http.MethodPost, "/admin/users", nil)
	req.Header.Set(HeaderAdminToken, token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("正确令牌应放行，得到 %d", rec.Code)
	}
	if !p.IsAdmin() || p.Credential != CredAdminToken || p.Kind != PrincipalAdmin {
		t.Fatalf("管理主体不符: %+v", p)
	}
}

func TestRequireAdminConstantTimePathsGiveNoOracle(t *testing.T) {
	const token = "test-admin-token"
	logger, logs := testLogger()
	auth := NewAuthenticator(&fakeVerifier{}, token, logger)
	h := auth.RequireAdmin(http.HandlerFunc(okHandler))

	// 缺失、等长错值、更长、更短、真令牌的前缀：全部必须 401，且响应体完全一致
	// （不给攻击者任何区分信号，比较本身在固定长度摘要上用 ConstantTimeCompare 做）。
	badTokens := []string{"", "test-admin-toke0", "test-admin-tokens", "test", token[:8]}
	var bodies []string
	for _, tok := range badTokens {
		req := httptest.NewRequest(http.MethodPost, "/admin/users", nil)
		if tok != "" {
			req.Header.Set(HeaderAdminToken, tok)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("令牌 %q 应回 401，得到 %d", tok, rec.Code)
		}
		detail := jsonBody(t, rec)["error"].(map[string]any)
		if detail["code"] != "invalid_admin_token" {
			t.Fatalf("错误码应为 invalid_admin_token，得到 %v", detail["code"])
		}
		bodies = append(bodies, rec.Body.String())
	}
	for i := 1; i < len(bodies); i++ {
		if bodies[i] != bodies[0] {
			t.Fatalf("不同失败令牌的响应体不应有差异:\n%s\n%s", bodies[0], bodies[i])
		}
	}
	if strings.Contains(logs.String(), token) {
		t.Fatal("管理令牌校验日志不得出现令牌明文")
	}
}

func TestRequireAdminWithoutConfiguredTokenFailsClosed(t *testing.T) {
	logger, _ := testLogger()
	auth := NewAuthenticator(&fakeVerifier{}, "", logger)
	if auth.AdminTokenConfigured() {
		t.Fatal("空令牌不应报告 configured")
	}
	for _, tok := range []string{"", "anything"} {
		req := httptest.NewRequest(http.MethodGet, "/admin/users", nil)
		if tok != "" {
			req.Header.Set(HeaderAdminToken, tok)
		}
		rec := httptest.NewRecorder()
		auth.RequireAdmin(http.HandlerFunc(okHandler)).ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("未配置令牌时必须 fail closed（401），得到 %d", rec.Code)
		}
	}
	if err := auth.AuthenticateAdmin(httptest.NewRequest(http.MethodGet, "/", nil)); !errors.Is(err, ErrAdminTokenNotConfigured) {
		t.Fatalf("期望 ErrAdminTokenNotConfigured，得到 %v", err)
	}
}

func TestRequireAdminRoutesMountedOnServer(t *testing.T) {
	fv := &fakeVerifier{}
	logger, _ := testLogger()
	auth := NewAuthenticator(fv, "test-admin-token", logger)

	srv, _ := newTestServer(t, auth.RequireAdminRoutes([]httpx.Route{
		{Pattern: "POST /admin/users/{id}/status", Handler: func(w http.ResponseWriter, r *http.Request) {
			httpx.WriteJSON(w, http.StatusOK, map[string]string{"id": r.PathValue("id")})
		}},
	}), func(c *Config) { c.AdminToken = "test-admin-token" })

	req := httptest.NewRequest(http.MethodPost, "/admin/users/u_7/status", nil)
	if rec := do(srv, req); rec.Code != http.StatusUnauthorized {
		t.Fatalf("缺管理令牌应 401，得到 %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/admin/users/u_7/status", nil)
	req.Header.Set(HeaderAdminToken, "test-admin-token")
	rec := do(srv, req)
	if rec.Code != http.StatusOK || jsonBody(t, rec)["id"] != "u_7" {
		t.Fatalf("带令牌应通过且路径参数可用: %d %s", rec.Code, rec.Body.String())
	}
}

func TestNewAuthenticatorPanicsOnNilVerifier(t *testing.T) {
	defer func() {
		if rec := recover(); rec == nil {
			t.Fatal("nil 校验器应 panic（装配错误不得拖到请求期）")
		}
	}()
	NewAuthenticator(nil, "tok", nil)
}

// ---------------------------------------------------------------------------
// 脱敏原语
// ---------------------------------------------------------------------------

func TestTokenHintKeepsOnlyPublicPrefixAndLength(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"ximo_sk_" + strings.Repeat("a", 32), fmt.Sprintf("ximo_sk_…(len=%d)", 40)},
		{"gwa_" + strings.Repeat("b", 64), fmt.Sprintf("gwa_…(len=%d)", 68)},
		{"gwr_" + strings.Repeat("c", 8), fmt.Sprintf("gwr_…(len=%d)", 12)},
		{"gwd_" + strings.Repeat("d", 8), fmt.Sprintf("gwd_…(len=%d)", 12)},
		{"sk-openai-0123456789abcdef", "redacted(len=26)"},
	}
	for _, tc := range cases {
		got := tokenHint(tc.in)
		if got != tc.want {
			t.Fatalf("tokenHint(%q) = %q，期望 %q", tc.in, got, tc.want)
		}
		// 提示里绝不能出现承载熵的正文（测试凭据的正文是同一个字符的重复）。
		if len(tc.in) > 20 && strings.Contains(got, strings.Repeat(tc.in[len(tc.in)-1:], 8)) {
			t.Fatalf("tokenHint 泄漏了凭据正文: %q", got)
		}
	}
}

func TestRedactTokensRemovesOurCredentialsOnly(t *testing.T) {
	secret := "ximo_sk_0123456789abcdef0123456789abcdef"
	text := "auth failed for Bearer " + secret + " and gwa_deadbeef and sk-openai-0123456789abcdef"
	got := redactTokens(text)
	if strings.Contains(got, secret) || strings.Contains(got, "gwa_deadbeef") {
		t.Fatalf("本产品凭据必须被抹掉: %q", got)
	}
	if !strings.Contains(got, "sk-openai-0123456789abcdef") {
		t.Fatalf("不应误伤第三方密钥格式（那由 observability/secrets 负责）: %q", got)
	}
	if redactTokens("") != "" || redactTokens("plain text") != "plain text" {
		t.Fatal("空串与普通文本应原样返回")
	}
}
