package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestSentinelFor 钉住服务端业务错误码 → 哨兵的映射（与 httpx.StatusFor 对齐）。
func TestSentinelFor(t *testing.T) {
	cases := []struct {
		status int
		code   string
		want   error
	}{
		{400, "authorization_pending", ErrAuthorizationPending},
		{400, "slow_down", ErrSlowDown},
		{401, "credential_expired", ErrCredentialExpired},
		{401, "expired_token", ErrCredentialExpired}, // 兼容码
		{401, "device_code_expired", ErrCredentialExpired},
		{401, "credential_revoked", ErrCredentialRevoked},
		{401, "invalid_grant", ErrCredentialRevoked}, // 兼容码
		{403, "access_denied", ErrCredentialRevoked},
		{401, "invalid_api_key", ErrUnauthorized},
		{403, "account_disabled", ErrAccountDisabled},
		{402, "insufficient_quota", ErrInsufficientQuota},
		{404, "not_found", ErrNotFound},
		{429, "rate_limited", ErrRateLimited},
		{503, "service_unavailable", ErrServiceUnavailable},
		// 没有错误码时按状态码兜底。
		{401, "", ErrUnauthorized},
		{402, "", ErrInsufficientQuota},
		{404, "", ErrNotFound},
		{429, "", ErrRateLimited},
		{503, "", ErrServiceUnavailable},
		// 认不出来就只留结构化错误，不硬塞哨兵。
		{500, "", nil},
		{500, "weird_code", nil},
	}
	for _, c := range cases {
		raw := []byte(fmt.Sprintf(`{"error":{"code":%q,"message":"m"}}`, c.code))
		ae := newAPIError(c.status, raw)
		if got := ae.Unwrap(); got != c.want {
			t.Errorf("sentinelFor(%d,%q) = %v, 期望 %v", c.status, c.code, got, c.want)
		}
		if c.want != nil && !errors.Is(ae, c.want) {
			t.Errorf("(%d,%q) errors.Is 失败", c.status, c.code)
		}
	}
}

// TestAPIErrorFormatting 错误文本的降级链：消息 → 响应摘要 → 状态文本。
func TestAPIErrorFormatting(t *testing.T) {
	full := &APIError{Status: 400, Code: "invalid_request_error", Message: "参数不对", RequestID: "req-1"}
	if got := full.Error(); !strings.Contains(got, "HTTP 400") || !strings.Contains(got, "invalid_request_error") ||
		!strings.Contains(got, "参数不对") || !strings.Contains(got, "req-1") {
		t.Errorf("Error() = %q", got)
	}
	if !full.IsCode("authorization_pending", "invalid_request_error") {
		t.Error("IsCode 应命中第二个码")
	}
	if full.IsCode("authorization_pending") {
		t.Error("IsCode 不应命中未列出的码")
	}

	// 只有响应摘要（反代 HTML）。
	plain := newAPIError(502, []byte("<html>502 Bad Gateway</html>"))
	if got := plain.Error(); !strings.HasPrefix(got, "HTTP 502:") || !strings.Contains(got, "502 Bad Gateway") {
		t.Errorf("Error() = %q", got)
	}
	// 什么都没有：退化到状态文本，不 panic。
	empty := &APIError{Status: 503}
	if got := empty.Error(); !strings.Contains(got, http.StatusText(503)) {
		t.Errorf("Error() = %q, 期望含状态文本", got)
	}
	var nilErr *APIError
	if got := nilErr.Error(); got != "<nil>" {
		t.Errorf("nil 接收者 Error() = %q", got)
	}
	if nilErr.IsCode("x") {
		t.Error("nil 接收者 IsCode 应为 false")
	}
}

// TestTransportErrorRedactionAndChain 传输层错误：文本脱敏，且错误链保留。
func TestTransportErrorRedactionAndChain(t *testing.T) {
	secret := "gwa_TRANSPORTLEAK1234567"
	c := &Client{BaseURL: "https://gw.example.com", HTTP: &http.Client{Transport: echoAuthTransport{}}}
	_, err := c.ListModels(context.Background(), Cred{Gateway: c.BaseURL, AccessToken: secret})
	if err == nil {
		t.Fatal("传输层失败应返回错误")
	}
	assertNoSecrets(t, "传输层错误", err.Error(), secret)
	if !strings.Contains(err.Error(), Mask(secret)) {
		t.Errorf("应把请求头里的凭据换成掩码: %s", err.Error())
	}
	if !errors.Is(err, errTransportBoom) {
		t.Errorf("原始错误应可通过 errors.Is 取出（%v）", err)
	}

	// redactErr 单元行为。
	if redactErr(nil, secret) != nil {
		t.Error("redactErr(nil) 应为 nil")
	}
	cause := errors.New("boom " + secret)
	wrapped := redactErr(cause, secret)
	if strings.Contains(wrapped.Error(), secret) {
		t.Errorf("redactErr 未脱敏: %s", wrapped.Error())
	}
	if !errors.Is(wrapped, cause) {
		t.Error("redactErr 应保留错误链")
	}
	if errors.Unwrap(wrapped) != cause {
		t.Error("redactErr 应保留 Unwrap")
	}
	if redactErr(cause, "ab").Error() != cause.Error() {
		t.Error("过短的 secret 不应参与替换")
	}
}

var errTransportBoom = errors.New("proxy refused connection")

type echoAuthTransport struct{}

func (echoAuthTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("%w: auth header was %q", errTransportBoom, r.Header.Get("Authorization"))
}

// TestNewClientAndNowHook 构造器与时钟注入口。
func TestNewClientAndNowHook(t *testing.T) {
	c := NewClient("https://gw.example.com/", "/tmp/x.json")
	if c.BaseURL != "https://gw.example.com/" || c.CredPath != "/tmp/x.json" {
		t.Errorf("NewClient = %+v", c)
	}
	// URL 在请求时规整：带结尾 / 也能用。
	f := newFakeGateway(t)
	c = NewClient(f.srv.URL+"/", testCredPath(t))
	if _, err := c.Health(context.Background()); err != nil {
		t.Errorf("结尾带 / 的地址应可用: %v", err)
	}

	fixed := func() time.Time { return time.Unix(1700000000, 0) }
	c.Now = fixed
	if !c.now().Equal(fixed()) {
		t.Error("Now 注入未生效")
	}
	c.Now = nil
	if c.now().IsZero() {
		t.Error("默认时钟应返回当前时间")
	}
}
