package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// 本文件是一个"假网关"，按主仓库 internal/gateway/api/meta 的真实行为实现：
// 同样的路由、同样的错误体 {"error":{message,type,code,request_id}}、同样的业务
// 错误码（authorization_pending / credential_expired / credential_revoked /
// invalid_api_key / account_disabled）。这样测试验证的是"真实接口形状"，
// 而不是自造的协议。

const (
	fakeUserCode   = "BCDF-2345"
	fakeDeviceCode = "gwd_TESTDEVICECODE0123456789ABC"
	fakePassword   = "s3cret-password-do-not-log-me"
	fakeVersion    = "v-fake-1"
)

type fakeGateway struct {
	srv *httptest.Server

	mu sync.Mutex

	// 设备流
	deviceCode    string
	userCode      string
	approved      bool
	consumed      bool
	expiredDevice bool
	pendingBefore int // 前 N 次 token 轮询返回 authorization_pending
	slowDownAt    int // 第 N 次 token 轮询返回 slow_down（0 = 不做）
	tokenPolls    int
	intervalSec   int64
	intervalMS    int64
	expiresIn     int64
	accessTTL     int64

	// 会话
	accesses  map[string]bool
	refreshes map[string]bool
	rotated   map[string]bool
	apiKeys   map[string]bool
	seq       int
	rotations int

	// 口令登录
	username   string
	password   string
	disabled   bool
	loginCalls int

	// 目录 / 用量
	models      []Model
	modelsCalls int
	usage       []UsageRecord
	usageCalls  int
	usageLimit  int
	usageOffset int

	// 故障注入
	echoSecretInError bool
	healthStatus      int
	healthBody        string
	// onRefresh 在成功响应 /v1/auth/refresh 之前调用，供测试制造
	// "续期成功但回写失败"（TOCTOU）这类时序场景。
	onRefresh func()

	requests []string
}

func newFakeGateway(t *testing.T) *fakeGateway {
	t.Helper()
	f := &fakeGateway{
		deviceCode:  fakeDeviceCode,
		userCode:    fakeUserCode,
		intervalSec: 5, // 服务端真实取值：ceilSeconds(5000ms) = 5 秒
		expiresIn:   600,
		accessTTL:   900, // 服务端真实取值：account.DefaultAccessTTL
		username:    "tester",
		password:    fakePassword,
		accesses:    map[string]bool{},
		refreshes:   map[string]bool{},
		rotated:     map[string]bool{},
		apiKeys:     map[string]bool{},
	}

	mux := http.NewServeMux()
	// 路由表与主仓库 meta.Routes 逐条一致。
	mux.HandleFunc("POST /v1/auth/device", f.handleDevice)
	mux.HandleFunc("POST /v1/auth/token", f.handleToken)
	mux.HandleFunc("POST /v1/auth/refresh", f.handleRefresh)
	mux.HandleFunc("POST /v1/auth/login", f.handleLogin)
	mux.HandleFunc("GET /v1/models", f.handleModels)
	mux.HandleFunc("GET /v1/usage", f.handleUsage)
	mux.HandleFunc("GET /v1/health", f.handleHealth)

	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// client 构造一个指向假网关的客户端；推荐轮询间隔压到 1ms 让测试跑得快。
func (f *fakeGateway) client(credPath string) *Client {
	return &Client{BaseURL: f.srv.URL, CredPath: credPath, PollInterval: time.Millisecond}
}

func (f *fakeGateway) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *fakeGateway) requestList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

func (f *fakeGateway) pollCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokenPolls
}

func (f *fakeGateway) rotationCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rotations
}

// seedTokens 直接造一对有效令牌（模拟"之前登录过"）。
func (f *fakeGateway) seedTokens() (access, refresh string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.issueLocked()
}

func (f *fakeGateway) refreshValid(token string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refreshes[token]
}

func (f *fakeGateway) accessValid(token string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.accesses[token]
}

// markRefreshRotated 把某个 refresh 标记为"已轮换过"，模拟复用旧 refresh。
func (f *fakeGateway) markRefreshRotated(token string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.refreshes, token)
	f.rotated[token] = true
}

func mustNoErr(t *testing.T, err error, what string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

func (f *fakeGateway) addAPIKey(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.apiKeys[key] = true
}

// issueLocked 必须持锁调用。
func (f *fakeGateway) issueLocked() (string, string) {
	f.seq++
	access := fmt.Sprintf("gwa_FAKEACCESS%04d%s", f.seq, strings.Repeat("a", 16))
	refresh := fmt.Sprintf("gwr_FAKEREFRESH%04d%s", f.seq, strings.Repeat("r", 16))
	f.accesses[access] = true
	f.refreshes[refresh] = true
	return access, refresh
}

// --------------------------------------------------------------------------- 路由

func (f *fakeGateway) handleDevice(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recordLocked(r)

	body := map[string]any{
		"device_code": f.deviceCode,
		"user_code":   f.userCode,
		"expires_in":  f.expiresIn,
	}
	if f.intervalMS > 0 {
		body["interval_ms"] = f.intervalMS
	} else {
		body["interval"] = f.intervalSec
	}
	fakeWriteJSON(w, http.StatusOK, body)
}

func (f *fakeGateway) handleToken(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recordLocked(r)

	var req struct {
		DeviceCode string `json:"device_code"`
	}
	fakeReadJSON(r, &req)
	code := strings.TrimSpace(req.DeviceCode)

	f.tokenPolls++
	switch {
	case f.expiredDevice:
		fakeWriteError(w, http.StatusUnauthorized, "credential_expired", "设备码已过期")
		return
	case code == "":
		fakeWriteError(w, http.StatusBadRequest, "invalid_request_error", "device_code 不能为空")
		return
	case code != f.deviceCode:
		fakeWriteError(w, http.StatusBadRequest, "invalid_request_error", "device_code 无法识别")
		return
	case f.consumed:
		// 一次性消费：再来就是已吊销。
		fakeWriteError(w, http.StatusUnauthorized, "credential_revoked", "设备码已被使用")
		return
	case f.slowDownAt > 0 && f.tokenPolls == f.slowDownAt:
		fakeWriteError(w, http.StatusBadRequest, "slow_down", "轮询过快，请放慢")
		return
	case !f.approved || f.tokenPolls <= f.pendingBefore:
		msg := "授权尚未完成，请稍后按 interval 继续轮询"
		if f.echoSecretInError {
			// 模拟"服务端/代理回显了凭据"：客户端必须掩掉它。
			msg = "授权尚未完成（device_code=" + code + "）"
		}
		fakeWriteError(w, http.StatusBadRequest, "authorization_pending", msg)
		return
	}

	f.consumed = true
	access, refresh := f.issueLocked()
	fakeWriteJSON(w, http.StatusOK, tokenWire{
		AccessToken: access, RefreshToken: refresh, ExpiresIn: f.accessTTL, TokenType: "Bearer",
	})
}

func (f *fakeGateway) handleRefresh(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recordLocked(r)

	var req struct {
		RefreshToken string `json:"refresh_token"`
	}
	fakeReadJSON(r, &req)
	tok := strings.TrimSpace(req.RefreshToken)

	switch {
	case tok == "":
		fakeWriteError(w, http.StatusBadRequest, "invalid_request_error", "refresh_token 不能为空")
		return
	case f.rotated[tok]:
		// 旧 refresh 已被轮换掉：立刻失效（account.Refresh 的 ErrRevoked 语义）。
		fakeWriteError(w, http.StatusUnauthorized, "credential_revoked", "刷新令牌失败")
		return
	case !f.refreshes[tok]:
		// 从未签发过（或格式不对）：服务端按 ErrBadCredentials 处理。
		fakeWriteError(w, http.StatusUnauthorized, "invalid_api_key", "刷新令牌失败")
		return
	}

	delete(f.refreshes, tok)
	f.rotated[tok] = true
	f.rotations++
	if f.onRefresh != nil {
		f.onRefresh()
	}
	access, refresh := f.issueLocked()
	fakeWriteJSON(w, http.StatusOK, tokenWire{
		AccessToken: access, RefreshToken: refresh, ExpiresIn: f.accessTTL, TokenType: "Bearer",
	})
}

func (f *fakeGateway) handleLogin(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recordLocked(r)

	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	fakeReadJSON(r, &req)

	f.loginCalls++
	switch {
	case strings.TrimSpace(req.Username) == "" || req.Password == "":
		fakeWriteError(w, http.StatusBadRequest, "invalid_request_error", "username 与 password 不能为空")
		return
	case f.disabled:
		fakeWriteError(w, http.StatusForbidden, "account_disabled", "账号已被停用")
		return
	case req.Username != f.username || req.Password != f.password:
		msg := "用户名或口令不正确"
		if f.echoSecretInError {
			// 最坏情况：服务端把口令回显在错误里。
			msg = "用户名或口令不正确（收到 password=" + req.Password + "）"
		}
		fakeWriteError(w, http.StatusUnauthorized, "invalid_api_key", msg)
		return
	}

	access, refresh := f.issueLocked()
	fakeWriteJSON(w, http.StatusOK, tokenWire{
		AccessToken: access, RefreshToken: refresh, ExpiresIn: f.accessTTL, TokenType: "Bearer",
	})
}

func (f *fakeGateway) handleModels(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recordLocked(r)
	f.modelsCalls++

	if !f.authorizedLocked(w, r) {
		return
	}
	if f.models == nil {
		fakeWriteJSON(w, http.StatusOK, map[string]any{"data": []Model{}})
		return
	}
	fakeWriteJSON(w, http.StatusOK, map[string]any{"data": f.models})
}

func (f *fakeGateway) handleUsage(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recordLocked(r)
	f.usageCalls++

	if !f.authorizedLocked(w, r) {
		return
	}
	q := r.URL.Query()
	f.usageLimit, _ = parseIntDefault(q.Get("limit"))
	f.usageOffset, _ = parseIntDefault(q.Get("offset"))
	if f.usageLimit > 200 {
		f.usageLimit = 200 // 服务端真实行为：超出上限静默收紧
	}
	data := f.usage
	if data == nil {
		data = []UsageRecord{}
	}
	fakeWriteJSON(w, http.StatusOK, map[string]any{"data": data})
}

func (f *fakeGateway) handleHealth(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recordLocked(r)

	if f.healthStatus != 0 && f.healthStatus != http.StatusOK {
		fakeWriteErrorPlain(w, f.healthStatus, f.healthBody)
		return
	}
	fakeWriteJSON(w, http.StatusOK, map[string]any{
		"status": "ok", "version": fakeVersion, "time_ms": time.Now().UnixMilli(),
	})
}

// authorizedLocked 复刻服务端用户态鉴权的拒绝行为（401 invalid_api_key）。
func (f *fakeGateway) authorizedLocked(w http.ResponseWriter, r *http.Request) bool {
	bearer := bearerOf(r)
	if bearer != "" && (f.accesses[bearer] || f.apiKeys[bearer]) {
		return true
	}
	msg := "缺少用户凭据：请携带 Authorization: Bearer <API key 或 access token>"
	if f.echoSecretInError && bearer != "" {
		msg = "凭据被拒绝（Authorization: Bearer " + bearer + "）"
	}
	fakeWriteError(w, http.StatusUnauthorized, "invalid_api_key", msg)
	return false
}

func (f *fakeGateway) recordLocked(r *http.Request) {
	f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI()+" auth="+r.Header.Get("Authorization"))
}

func bearerOf(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const p = "Bearer "
	if len(h) > len(p) && strings.EqualFold(h[:len(p)], p) {
		return strings.TrimSpace(h[len(p):])
	}
	return ""
}

func parseIntDefault(s string) (int, bool) {
	if strings.TrimSpace(s) == "" {
		return 0, false
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}

// --------------------------------------------------------------------------- 响应原语
// （与 httpx.WriteJSON / WriteError / TypeForStatus 同形）

func fakeWriteJSON(w http.ResponseWriter, status int, v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		fakeWriteErrorPlain(w, http.StatusInternalServerError, "response encode failed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func fakeWriteError(w http.ResponseWriter, status int, code, msg string) {
	fakeWriteJSON(w, status, map[string]any{"error": map[string]any{
		"message":    msg,
		"type":       fakeTypeForStatus(status),
		"code":       code,
		"request_id": "req-fake-0001",
	}})
}

func fakeWriteErrorPlain(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func fakeTypeForStatus(status int) string {
	switch {
	case status == http.StatusBadRequest:
		return "invalid_request_error"
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return "authentication_error"
	case status == http.StatusNotFound:
		return "not_found_error"
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status == http.StatusPaymentRequired:
		return "quota_error"
	case status >= 500:
		return "server_error"
	default:
		return "invalid_request_error"
	}
}

func fakeReadJSON(r *http.Request, dst any) {
	if r.Body == nil {
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return
	}
	_ = json.Unmarshal(raw, dst)
}
