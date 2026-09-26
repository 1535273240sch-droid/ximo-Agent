package meta

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ximo888ok-netizen/ximo-agent/internal/account"
	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/model"
)

// setUserStatus 直接改内存用户状态（模拟管理员停用账号）。
func (f *fakeStore) setUserStatus(id, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[id]
	if !ok {
		return
	}
	u.Status = status
	f.users[id] = u
}

// newAuthEnv 用真实 account.Service（内存 store）搭出登录链。
func newAuthEnv(t *testing.T) (*fakeStore, *account.Service, *recLogger, *http.ServeMux, model.User) {
	t.Helper()
	ctx := context.Background()
	st := newFakeStore()
	svc := account.New(st, nil)
	u, err := svc.CreateUser(ctx, "plugin-user", "s3cret-password", "default")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	log := &recLogger{}
	mux := newRouter(t, Deps{Accounts: svc, Store: st, Logger: log})
	return st, svc, log, mux, u
}

// TestDeviceLoginChain 覆盖文档 §5.2 的完整插件登录链：
// 申请设备码 → 待授权轮询 → 用户批准 → 兑换令牌 → 设备码不可复用 → 刷新轮换。
func TestDeviceLoginChain(t *testing.T) {
	ctx := context.Background()
	_, svc, log, mux, u := newAuthEnv(t)

	// 1) 申请设备码。
	rec := do(t, mux, http.MethodPost, "/v1/auth/device", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("申请设备码状态码 = %d, body=%s", rec.Code, rec.Body.String())
	}
	dev := bodyMap(t, rec)
	assertKeys(t, dev, "device_code", "user_code", "expires_in", "interval")
	deviceCode, _ := dev["device_code"].(string)
	userCode, _ := dev["user_code"].(string)
	if !strings.HasPrefix(deviceCode, "gwd_") {
		t.Errorf("device_code 前缀不符: %q", deviceCode)
	}
	if len(userCode) != 8 {
		t.Errorf("user_code 长度 = %d, 期望 8", len(userCode))
	}
	if dev["expires_in"] != float64(account.DefaultDeviceCodeTTL.Seconds()) {
		t.Errorf("expires_in = %v", dev["expires_in"])
	}
	if dev["interval"] != float64(account.DefaultPollIntervalMS/1000) {
		t.Errorf("interval = %v", dev["interval"])
	}
	assertNoLeak(t, rec)

	// 2) 未授权：400 authorization_pending。
	rec = do(t, mux, http.MethodPost, "/v1/auth/token", map[string]any{"device_code": deviceCode})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("待授权状态码 = %d, 期望 400, body=%s", rec.Code, rec.Body.String())
	}
	if got := errorCode(t, rec); got != "authorization_pending" {
		t.Fatalf("待授权错误码 = %q, 期望 authorization_pending", got)
	}

	// 3) 用户在授权页确认（相当于 POST /admin/device/approve）。
	if err := svc.ApproveDeviceLogin(ctx, userCode, u.ID); err != nil {
		t.Fatalf("ApproveDeviceLogin: %v", err)
	}

	// 4) 兑换令牌对。
	rec = do(t, mux, http.MethodPost, "/v1/auth/token", map[string]any{"device_code": deviceCode})
	if rec.Code != http.StatusOK {
		t.Fatalf("兑换状态码 = %d, body=%s", rec.Code, rec.Body.String())
	}
	tok := bodyMap(t, rec)
	assertKeys(t, tok, "access_token", "refresh_token", "expires_in", "token_type")
	access, _ := tok["access_token"].(string)
	refresh, _ := tok["refresh_token"].(string)
	if !strings.HasPrefix(access, "gwa_") || !strings.HasPrefix(refresh, "gwr_") {
		t.Errorf("令牌前缀不符: %q / %q", access, refresh)
	}
	if tok["token_type"] != "Bearer" || tok["expires_in"] != float64(account.DefaultAccessTTL.Seconds()) {
		t.Errorf("token_type/expires_in = %v/%v", tok["token_type"], tok["expires_in"])
	}
	assertNoLeak(t, rec)

	// 发出去的 access 必须真的可用（走真实 account 校验）。
	got, err := svc.VerifyAccess(ctx, access)
	if err != nil || got.ID != u.ID {
		t.Fatalf("VerifyAccess: user=%v err=%v", got.ID, err)
	}

	// 5) 设备码不可复用（一次性消费）。
	rec = do(t, mux, http.MethodPost, "/v1/auth/token", map[string]any{"device_code": deviceCode})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("重复兑换状态码 = %d, 期望 401, body=%s", rec.Code, rec.Body.String())
	}
	if got := errorCode(t, rec); got != "credential_revoked" {
		t.Errorf("重复兑换错误码 = %q, 期望 credential_revoked", got)
	}

	// 6) 刷新轮换：新令牌可用，旧 refresh 立即失效。
	rec = do(t, mux, http.MethodPost, "/v1/auth/refresh", map[string]any{"refresh_token": refresh})
	if rec.Code != http.StatusOK {
		t.Fatalf("刷新状态码 = %d, body=%s", rec.Code, rec.Body.String())
	}
	tok2 := bodyMap(t, rec)
	access2, _ := tok2["access_token"].(string)
	refresh2, _ := tok2["refresh_token"].(string)
	if refresh2 == refresh || access2 == access {
		t.Errorf("刷新应换发新令牌")
	}
	if got, err := svc.VerifyAccess(ctx, access2); err != nil || got.ID != u.ID {
		t.Fatalf("刷新后 access 不可用: user=%v err=%v", got.ID, err)
	}
	if _, err := svc.VerifyAccess(ctx, access); !errors.Is(err, model.ErrRevoked) {
		t.Errorf("轮换后旧 access 应为 ErrRevoked, got %v", err)
	}
	rec = do(t, mux, http.MethodPost, "/v1/auth/refresh", map[string]any{"refresh_token": refresh})
	if rec.Code != http.StatusUnauthorized || errorCode(t, rec) != "credential_revoked" {
		t.Errorf("旧 refresh 复用应 401 credential_revoked: %d %s", rec.Code, rec.Body.String())
	}

	// 7) 凭据不进日志（含失败路径）。
	if blob := log.blob(); strings.Contains(blob, "gwa_") || strings.Contains(blob, "gwr_") || strings.Contains(blob, "gwd_") {
		t.Errorf("日志疑似包含凭据: %s", blob)
	}
	if blob := log.blob(); strings.Contains(blob, "s3cret-password") {
		t.Errorf("日志疑似包含口令: %s", blob)
	}
}

func TestDeviceTokenUnknownCode(t *testing.T) {
	_, _, _, mux, _ := newAuthEnv(t)
	rec := do(t, mux, http.MethodPost, "/v1/auth/token", map[string]any{"device_code": "gwd_does-not-exist"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("状态码 = %d, 期望 401, body=%s", rec.Code, rec.Body.String())
	}
	if got := errorCode(t, rec); got != "invalid_api_key" {
		t.Errorf("错误码 = %q, 期望 invalid_api_key", got)
	}
	assertNoLeak(t, rec)
}

func TestAuthRequestValidation(t *testing.T) {
	_, _, _, mux, _ := newAuthEnv(t)
	cases := []struct {
		name, target string
		body         any
	}{
		{"token 空设备码", "/v1/auth/token", map[string]any{"device_code": "  "}},
		{"token 缺字段", "/v1/auth/token", map[string]any{}},
		{"token 非法 JSON", "/v1/auth/token", nil},
		{"refresh 空令牌", "/v1/auth/refresh", map[string]any{"refresh_token": ""}},
		{"login 空用户名", "/v1/auth/login", map[string]any{"username": " ", "password": "s3cret-password"}},
		{"login 空口令", "/v1/auth/login", map[string]any{"username": "plugin-user", "password": ""}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := do(t, mux, http.MethodPost, c.target, c.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("状态码 = %d, 期望 400, body=%s", rec.Code, rec.Body.String())
			}
			if got := errorCode(t, rec); got != "invalid_request_error" {
				t.Errorf("错误码 = %q", got)
			}
			assertNoLeak(t, rec)
		})
	}
}

func TestLoginSuccessAndFailures(t *testing.T) {
	ctx := context.Background()
	st, svc, log, mux, u := newAuthEnv(t)

	rec := do(t, mux, http.MethodPost, "/v1/auth/login", map[string]any{"username": "plugin-user", "password": "s3cret-password"})
	if rec.Code != http.StatusOK {
		t.Fatalf("登录状态码 = %d, body=%s", rec.Code, rec.Body.String())
	}
	tok := bodyMap(t, rec)
	assertKeys(t, tok, "access_token", "refresh_token", "expires_in", "token_type")
	access, _ := tok["access_token"].(string)
	if got, err := svc.VerifyAccess(ctx, access); err != nil || got.ID != u.ID {
		t.Fatalf("登录返回的 access 不可用: %v %v", got.ID, err)
	}
	assertNoLeak(t, rec)

	// 口令错误与用户不存在必须给出同样的错误（不给账号枚举留差异）。
	for _, c := range []struct{ name, user, pass string }{
		{"口令错误", "plugin-user", "wrong-password"},
		{"用户不存在", "no-such-user", "s3cret-password"},
	} {
		rec := do(t, mux, http.MethodPost, "/v1/auth/login", map[string]any{"username": c.user, "password": c.pass})
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s 状态码 = %d, 期望 401", c.name, rec.Code)
		}
		if got := errorCode(t, rec); got != "invalid_api_key" {
			t.Errorf("%s 错误码 = %q", c.name, got)
		}
		assertNoLeak(t, rec)
	}

	// 账号被停用 → 403（验证过口令之后才判状态）。
	st.setUserStatus(u.ID, model.UserStatusDisabled)
	rec = do(t, mux, http.MethodPost, "/v1/auth/login", map[string]any{"username": "plugin-user", "password": "s3cret-password"})
	if rec.Code != http.StatusForbidden || errorCode(t, rec) != "account_disabled" {
		t.Errorf("停用账号 = %d %s", rec.Code, rec.Body.String())
	}
	st.setUserStatus(u.ID, model.UserStatusActive)

	if blob := log.blob(); strings.Contains(blob, "gwa_") || strings.Contains(blob, "gwr_") || strings.Contains(blob, "s3cret-password") {
		t.Errorf("日志疑似包含凭据: %s", blob)
	}
}

func TestLoginIssueSessionFailure(t *testing.T) {
	// 认证通过但会话落库失败：必须是 5xx，且不把内部细节回给客户端。
	log := &recLogger{}
	st := newFakeStore()
	svc := account.New(st, nil)
	if _, err := svc.CreateUser(context.Background(), "plugin-user", "s3cret-password", ""); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	st.mu.Lock()
	st.errCreateSession = errors.New("boom-internal-session")
	st.mu.Unlock()
	failMux := newRouter(t, Deps{Accounts: svc, Logger: log})

	rec := do(t, failMux, http.MethodPost, "/v1/auth/login", map[string]any{"username": "plugin-user", "password": "s3cret-password"})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d, 期望 500, body=%s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); strings.Contains(body, "boom-internal-session") {
		t.Errorf("响应泄漏内部错误细节: %s", body)
	}
	if _, errs := log.counts(); errs != 1 {
		t.Errorf("Error 日志次数 = %d, 期望 1", errs)
	}
	assertNoLeak(t, rec)
}

// TestAuthConcurrentDeviceToken 确认并发兑换同一设备码只成功一次（一次性消费）。
func TestAuthConcurrentDeviceToken(t *testing.T) {
	ctx := context.Background()
	_, svc, _, mux, u := newAuthEnv(t)

	rec := do(t, mux, http.MethodPost, "/v1/auth/device", nil)
	dev := bodyMap(t, rec)
	deviceCode, _ := dev["device_code"].(string)
	userCode, _ := dev["user_code"].(string)
	if err := svc.ApproveDeviceLogin(ctx, userCode, u.ID); err != nil {
		t.Fatalf("ApproveDeviceLogin: %v", err)
	}

	const n = 4
	// 并发前先造好请求，避免在 goroutine 里调用 testing 的 Fatal。
	reqs := make([]*http.Request, n)
	for i := range reqs {
		buf, err := json.Marshal(map[string]any{"device_code": deviceCode})
		if err != nil {
			t.Fatalf("编码请求体失败: %v", err)
		}
		reqs[i] = httptest.NewRequest(http.MethodPost, "/v1/auth/token", bytes.NewReader(buf))
	}
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, reqs[i])
			codes[i] = rec.Code
		}(i)
	}
	wg.Wait()

	ok := 0
	for _, c := range codes {
		if c == http.StatusOK {
			ok++
			continue
		}
		if c != http.StatusUnauthorized {
			t.Errorf("并发兑换出现意外状态码 %d", c)
		}
	}
	if ok != 1 {
		t.Errorf("成功兑换次数 = %d, 期望 1（codes=%v）", ok, codes)
	}
}
