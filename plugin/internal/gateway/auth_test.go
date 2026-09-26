package gateway

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// logSink 收集客户端日志，供"日志里没有明文凭据"的断言使用。
type logSink struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (l *logSink) printf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(&l.buf, format+"\n", args...)
}

func (l *logSink) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// assertNoSecrets 断言 text 里没有出现任何明文凭据。失败信息只用掩码，避免把
// 凭据写进测试输出（否则"证明没泄漏"的过程本身就泄漏了）。
func assertNoSecrets(t *testing.T, where, text string, secrets ...string) {
	t.Helper()
	for _, s := range secrets {
		if s == "" {
			continue
		}
		if strings.Contains(text, s) {
			t.Errorf("%s 出现明文凭据 %s：%s", where, Mask(s), text)
		}
	}
}

func testCredPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), ".ximo-plugin", "cred.json")
}

// TestDeviceLoginPendingThenSuccess 覆盖设备码登录主路径：申请设备码 → 两次
// authorization_pending → 授权成功 → 令牌落盘。
func TestDeviceLoginPendingThenSuccess(t *testing.T) {
	f := newFakeGateway(t)
	f.approved = true
	f.pendingBefore = 2
	credPath := testCredPath(t)

	sink := &logSink{}
	var shown DeviceAuth
	c := f.client(credPath)
	c.Logf = sink.printf
	c.OnDeviceCode = func(d DeviceAuth) { shown = d }

	cred, err := c.Login(context.Background(), "")
	if err != nil {
		t.Fatalf("Login 失败: %v", err)
	}

	if shown.UserCode != fakeUserCode {
		t.Errorf("展示的用户码 = %q, 期望 %q", shown.UserCode, fakeUserCode)
	}
	if shown.DeviceCode != fakeDeviceCode {
		t.Errorf("回调拿到的设备码不对: %v", shown.DeviceCode != fakeDeviceCode)
	}
	if shown.Interval != time.Millisecond {
		t.Errorf("轮询间隔 = %s, 期望 1ms（客户端显式覆盖了服务端建议的 5s 以便测试提速）", shown.Interval)
	}
	if shown.ExpiresIn != 10*time.Minute {
		t.Errorf("设备码寿命 = %s, 期望 10m", shown.ExpiresIn)
	}
	if shown.VerificationURI != "" {
		t.Errorf("V1 的 /v1/auth/device 不返回 verification_uri，期望空串，得到 %q", shown.VerificationURI)
	}
	if got := f.pollCount(); got != 3 {
		t.Errorf("轮询次数 = %d, 期望 3（2 次 pending + 1 次成功）", got)
	}
	if cred.Empty() || cred.Gateway != f.srv.URL {
		t.Errorf("凭据不完整: gateway=%q empty=%v", cred.Gateway, cred.Empty())
	}
	if !f.accessValid(cred.AccessToken) {
		t.Errorf("网关不认这次下发的 access token")
	}

	// 落盘内容必须与返回的凭据一致（refresh 也存下来，供后续自动续期）。
	loaded, err := c.LoadCred()
	if err != nil {
		t.Fatalf("LoadCred 失败: %v", err)
	}
	if loaded.AccessToken != cred.AccessToken || loaded.RefreshToken != cred.RefreshToken {
		t.Errorf("落盘的凭据与返回的不一致")
	}
	if loaded.AccessExpiresAt == 0 {
		t.Errorf("落盘凭据缺少到期时间（无法判断是否需要续期）")
	}

	// 日志与错误输出不得含明文凭据（设备码、用户码里的设备码、access/refresh）。
	logged := sink.String()
	if strings.TrimSpace(logged) == "" {
		t.Fatal("日志为空：本测试要验证「日志里没有明文」，空日志说明钩子没被调用")
	}
	assertNoSecrets(t, "日志", logged, fakeDeviceCode, fakeUserCode, cred.AccessToken, cred.RefreshToken)
	if !strings.Contains(logged, Mask(fakeDeviceCode)) {
		t.Errorf("日志里应出现掩码后的设备码 %s，实际：%s", Mask(fakeDeviceCode), logged)
	}
}

// TestPollDeviceLoginOnceKeepsPendingAPIError 钉住"待授权原样上抛"：既能
// errors.Is(ErrAuthorizationPending)，也能 errors.As 拿到结构化错误码，
// 且服务端若回显设备码必须被掩码。
func TestPollDeviceLoginOnceKeepsPendingAPIError(t *testing.T) {
	f := newFakeGateway(t)
	f.approved = false
	f.echoSecretInError = true

	c := f.client(testCredPath(t))
	_, err := c.PollDeviceLoginOnce(context.Background(), fakeDeviceCode)
	if err == nil {
		t.Fatal("未授权时应返回错误")
	}
	if !errors.Is(err, ErrAuthorizationPending) {
		t.Errorf("errors.Is(ErrAuthorizationPending) = false: %v", err)
	}
	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatalf("应能取出 *APIError: %v", err)
	}
	if ae.Status != 400 || ae.Code != "authorization_pending" {
		t.Errorf("结构化错误 = %d/%s, 期望 400/authorization_pending", ae.Status, ae.Code)
	}
	if ae.RequestID != "req-fake-0001" {
		t.Errorf("request_id = %q（应透传给调用方便于追查）", ae.RequestID)
	}
	assertNoSecrets(t, "错误文本", err.Error(), fakeDeviceCode)
	if !strings.Contains(err.Error(), Mask(fakeDeviceCode)) {
		t.Errorf("错误文本应含掩码后的设备码 %s：%s", Mask(fakeDeviceCode), err.Error())
	}

	// 反向控制：不传 secrets 时同一份响应体确实会带明文 —— 证明上面的断言不是
	// 恒真，掩码这一步真的在起作用。
	raw := []byte(`{"error":{"message":"pending device_code=` + fakeDeviceCode + `","code":"authorization_pending"}}`)
	if !strings.Contains(newAPIError(400, raw).Error(), fakeDeviceCode) {
		t.Error("反向控制失败：未掩码时应能看到明文，说明掩码测试没有测到点子上")
	}
}

// TestDeviceLoginTimeout 授权窗口内没人确认时必须明确超时，而不是无限挂机。
func TestDeviceLoginTimeout(t *testing.T) {
	f := newFakeGateway(t)
	f.approved = false

	c := f.client(testCredPath(t))
	c.PollInterval = 5 * time.Millisecond
	c.DeviceLoginTimeout = 60 * time.Millisecond
	c.OnDeviceCode = func(DeviceAuth) {}

	start := time.Now()
	_, err := c.Login(context.Background(), "")
	elapsed := time.Since(start)

	if !errors.Is(err, ErrLoginTimeout) {
		t.Fatalf("错误 = %v, 期望 ErrLoginTimeout", err)
	}
	if elapsed < 60*time.Millisecond {
		t.Errorf("只等了 %s，早于 60ms 的窗口就放弃了", elapsed)
	}
	if f.pollCount() == 0 {
		t.Error("超时前一次都没轮询")
	}
	if _, statErr := c.LoadCred(); !errors.Is(statErr, ErrNoCredential) {
		t.Errorf("超时不应留下凭据文件: %v", statErr)
	}
}

// TestDeviceLoginExpiredAndRevoked 设备码过期/已消费都是明确的失败，不重试。
func TestDeviceLoginExpiredAndRevoked(t *testing.T) {
	t.Run("expired", func(t *testing.T) {
		f := newFakeGateway(t)
		f.expiredDevice = true
		c := f.client(testCredPath(t))
		_, err := c.Login(context.Background(), fakeDeviceCode)
		if !errors.Is(err, ErrCredentialExpired) {
			t.Fatalf("错误 = %v, 期望 ErrCredentialExpired", err)
		}
	})
	t.Run("revoked", func(t *testing.T) {
		f := newFakeGateway(t)
		f.approved = true
		f.consumed = true
		c := f.client(testCredPath(t))
		_, err := c.Login(context.Background(), fakeDeviceCode)
		if !errors.Is(err, ErrCredentialRevoked) {
			t.Fatalf("错误 = %v, 期望 ErrCredentialRevoked", err)
		}
	})
}

// TestLoginRefSemantics 钉住 Login(ctx, ref) 的三种入参语义。
func TestLoginRefSemantics(t *testing.T) {
	t.Run("空 ref 需要展示回调", func(t *testing.T) {
		f := newFakeGateway(t)
		c := f.client(testCredPath(t)) // 不设 OnDeviceCode
		_, err := c.Login(context.Background(), "")
		if err == nil || !strings.Contains(err.Error(), "OnDeviceCode") {
			t.Fatalf("错误 = %v, 期望提示缺少 OnDeviceCode 回调", err)
		}
		if f.requestCount() != 1 {
			t.Errorf("请求数 = %d：应只发了 /v1/auth/device（未展示用户码就不该继续轮询）", f.requestCount())
		}
	})

	t.Run("用户码不可直接换令牌", func(t *testing.T) {
		f := newFakeGateway(t)
		c := f.client(testCredPath(t))
		_, err := c.Login(context.Background(), fakeUserCode)
		if !errors.Is(err, ErrUserCodeNotUsable) {
			t.Fatalf("错误 = %v, 期望 ErrUserCodeNotUsable", err)
		}
		if f.requestCount() != 0 {
			t.Errorf("请求数 = %d, 期望 0（无可用的换取端点，不该打网关）", f.requestCount())
		}
		if strings.Contains(err.Error(), fakeUserCode) {
			t.Errorf("用户码不应明文回显: %s", err.Error())
		}
	})

	t.Run("设备码直接轮询", func(t *testing.T) {
		f := newFakeGateway(t)
		f.approved = true
		credPath := testCredPath(t)
		c := f.client(credPath)
		cred, err := c.Login(context.Background(), "  "+fakeDeviceCode+"  ")
		if err != nil {
			t.Fatalf("Login 失败: %v", err)
		}
		if !f.accessValid(cred.AccessToken) {
			t.Error("凭据未被网关认可")
		}
		if _, err := c.LoadCred(); err != nil {
			t.Errorf("凭据应落盘: %v", err)
		}
	})
}

// TestLoginPassword 用户名口令登录：成功落盘、失败不落盘、口令不进错误文本。
func TestLoginPassword(t *testing.T) {
	f := newFakeGateway(t)
	f.echoSecretInError = true // 最坏情况：服务端把口令回显在错误里
	credPath := testCredPath(t)
	sink := &logSink{}
	c := f.client(credPath)
	c.Logf = sink.printf

	cred, err := c.LoginPassword(context.Background(), "tester", fakePassword)
	if err != nil {
		t.Fatalf("口令登录失败: %v", err)
	}
	if cred.Username != "tester" {
		t.Errorf("username = %q", cred.Username)
	}
	loaded, err := c.LoadCred()
	if err != nil || loaded.AccessToken != cred.AccessToken {
		t.Fatalf("凭据未正确落盘: %v", err)
	}
	assertNoSecrets(t, "日志", sink.String(), fakePassword, cred.AccessToken, cred.RefreshToken)

	// 口令错误：401 invalid_api_key，且错误文本里只有掩码。
	_, err = c.LoginPassword(context.Background(), "tester", fakePassword+"-wrong-long")
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("错误 = %v, 期望 ErrUnauthorized", err)
	}
	assertNoSecrets(t, "错误文本", err.Error(), fakePassword+"-wrong-long")

	// 账号停用：403 account_disabled。
	f.disabled = true
	_, err = c.LoginPassword(context.Background(), "tester", fakePassword)
	if !errors.Is(err, ErrAccountDisabled) {
		t.Fatalf("错误 = %v, 期望 ErrAccountDisabled", err)
	}

	// 空口令：本地就拒绝，不打网关（口令本身不做 trim：空格也可能是口令的一部分，
	// 交给服务端判定）。
	before := f.requestCount()
	if _, err := c.LoginPassword(context.Background(), "tester", ""); err == nil {
		t.Error("空口令应被本地拒绝")
	}
	if f.requestCount() != before {
		t.Error("空口令不该发请求")
	}
}

// TestHealthAndPlainErrorBody 覆盖探活与"非 JSON 错误体"的降级路径。
func TestHealthAndPlainErrorBody(t *testing.T) {
	f := newFakeGateway(t)
	c := f.client(testCredPath(t))

	h, err := c.Health(context.Background())
	if err != nil {
		t.Fatalf("Health 失败: %v", err)
	}
	if h.Status != "ok" || h.Version != fakeVersion || h.TimeMS == 0 {
		t.Errorf("Health = %+v", h)
	}

	// 反代返回 HTML（解不出结构化错误体）：截断 + 掩码后作为 Body。
	secret := "gwa_LEAKEDSECRET1234567890"
	f.healthStatus = 502
	f.healthBody = "<html>bad gateway token=" + secret + "</html>"
	_, err = c.Health(context.Background())
	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatalf("错误 = %v, 期望 *APIError", err)
	}
	if ae.Status != 502 || ae.Code != "" {
		t.Errorf("错误 = %d/%q, 期望 502/空码", ae.Status, ae.Code)
	}
	if !strings.Contains(ae.Error(), "bad gateway") {
		t.Errorf("应保留响应摘要便于排障: %s", ae.Error())
	}
	// 该请求没带凭据，因此这里能看到的明文是"响应体里本来就有的"，
	// 说明只有本次请求携带过的凭据才会被掩码 —— 记录这个边界。
	if !strings.Contains(ae.Error(), secret) {
		t.Log("响应体里的第三方串未被掩码（符合预期：只掩本次请求携带的凭据）")
	}
}

// TestNextPollInterval 钉住轮询节奏规则（nextPollInterval：RFC 8628 的 slow_down 加时、
// 连续 pending 退避，以及 [minPollInterval, maxPollInterval] 夹取）。
func TestNextPollInterval(t *testing.T) {
	cases := []struct {
		name    string
		cur     time.Duration
		pending int
		slow    bool
		want    time.Duration
	}{
		{"保持服务端间隔", 5 * time.Second, 1, false, 5 * time.Second},
		{"slow_down 加 5 秒", 5 * time.Second, 1, true, 10 * time.Second},
		{"连续 10 次 pending 后 1.5 倍", 5 * time.Second, 10, false, 7500 * time.Millisecond},
		{"下界夹到 1s", 200 * time.Millisecond, 1, false, time.Second},
		{"上界夹到 60s", 55 * time.Second, 1, true, 60 * time.Second},
	}
	for _, c := range cases {
		if got := nextPollInterval(c.cur, c.pending, c.slow); got != c.want {
			t.Errorf("%s: nextPollInterval(%s,%d,%v) = %s, 期望 %s", c.name, c.cur, c.pending, c.slow, got, c.want)
		}
	}
}

// TestDevicePollIntervalUnits 服务端 interval 的单位容忍规则。
func TestDevicePollIntervalUnits(t *testing.T) {
	cases := []struct {
		name string
		in   deviceWire
		want time.Duration
	}{
		{"服务端真实形状（秒）", deviceWire{Interval: 5}, 5 * time.Second},
		{"毫秒字段优先", deviceWire{Interval: 5, IntervalMS: 1200}, 1200 * time.Millisecond},
		{"大数值按毫秒解释", deviceWire{Interval: 5000}, 5 * time.Second},
		{"缺失时用默认 5s", deviceWire{}, 5 * time.Second},
		{"0 与负数按缺失处理", deviceWire{Interval: -3}, 5 * time.Second},
		{"毫秒值过小夹到 1s", deviceWire{IntervalMS: 1}, time.Second},
		{"天量值夹到 60s", deviceWire{Interval: 86400}, time.Minute},
	}
	for _, c := range cases {
		if got := c.in.devicePollInterval(); got != c.want {
			t.Errorf("%s: = %s, 期望 %s", c.name, got, c.want)
		}
	}
}

// TestSlowDownPath slow_down 时放慢而不是失败。
func TestSlowDownPath(t *testing.T) {
	f := newFakeGateway(t)
	f.approved = true
	f.slowDownAt = 1 // 第一次轮询返回 slow_down，之后才授权

	c := f.client(testCredPath(t))
	c.DeviceLoginTimeout = 5 * time.Second
	c.OnDeviceCode = func(DeviceAuth) {}

	cred, err := c.Login(context.Background(), "")
	if err != nil {
		t.Fatalf("slow_down 不应导致失败: %v", err)
	}
	if got := f.pollCount(); got != 2 {
		t.Errorf("轮询次数 = %d, 期望 2（先 slow_down 再成功）", got)
	}
	if !f.accessValid(cred.AccessToken) {
		t.Error("凭据未被网关认可")
	}
}

// TestPollWaitBoundedByWindow 服务端给的间隔（5s）长于授权窗口时，必须在窗口
// 用尽后立即报超时，而不是傻等一个完整的 interval。
func TestPollWaitBoundedByWindow(t *testing.T) {
	f := newFakeGateway(t)
	f.approved = false

	c := f.client(testCredPath(t))
	c.PollInterval = 0 // 用服务端返回的 5 秒间隔
	c.DeviceLoginTimeout = 100 * time.Millisecond
	c.OnDeviceCode = func(DeviceAuth) {}

	start := time.Now()
	_, err := c.Login(context.Background(), "")
	elapsed := time.Since(start)

	if !errors.Is(err, ErrLoginTimeout) {
		t.Fatalf("错误 = %v, 期望 ErrLoginTimeout", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("耗时 %s：等待没有被授权窗口截断（应约 100ms）", elapsed)
	}
	if got := f.pollCount(); got != 0 {
		t.Errorf("轮询次数 = %d, 期望 0（首次等待就超过窗口）", got)
	}
}
