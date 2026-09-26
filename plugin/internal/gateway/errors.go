package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// 哨兵错误。判定一律用 errors.Is/errors.As，不要比较错误字符串。
//
// APIError.Unwrap 把服务端业务错误码翻译成下面这些哨兵，因此
// errors.Is(err, ErrAuthorizationPending) 这类判断对"结构化错误码"与"本地构造的
// 错误"都成立。错误码到哨兵的映射来自主仓库 internal/gateway/httpx.StatusFor 与
// internal/account 的哨兵（例如 credential_expired、credential_revoked）。
var (
	// ErrNoCredential 本地没有可用凭据（文件不存在，或既无 API Key 也无令牌）。
	ErrNoCredential = errors.New("gateway: 本地没有可用凭据")
	// ErrGatewayMismatch 凭据文件里的网关地址与本次要访问的网关不一致。
	ErrGatewayMismatch = errors.New("gateway: 凭据属于其它网关地址")
	// ErrAuthorizationPending 设备授权尚未完成（服务端 400 authorization_pending）。
	ErrAuthorizationPending = errors.New("gateway: 授权尚未完成")
	// ErrSlowDown 服务端要求放慢轮询（RFC 8628 的 slow_down）。
	ErrSlowDown = errors.New("gateway: 服务端要求放慢轮询")
	// ErrLoginTimeout 在等待窗口内没有完成设备授权。
	ErrLoginTimeout = errors.New("gateway: 等待授权超时")
	// ErrCredentialExpired 凭据/设备码已过期，需要重新登录。
	ErrCredentialExpired = errors.New("gateway: 凭据或设备码已过期")
	// ErrCredentialRevoked 凭据/设备码已被吊销或使用过（refresh 不可重复使用）。
	ErrCredentialRevoked = errors.New("gateway: 凭据已吊销")
	// ErrUnauthorized 凭据被网关拒绝（未携带、无效、格式不对）。
	ErrUnauthorized = errors.New("gateway: 凭据被网关拒绝")
	// ErrAccountDisabled 账号被停用。
	ErrAccountDisabled = errors.New("gateway: 账号已被停用")
	// ErrInsufficientQuota 额度不足（服务端 402 insufficient_quota）。
	ErrInsufficientQuota = errors.New("gateway: 额度不足")
	// ErrNotFound 资源不存在。
	ErrNotFound = errors.New("gateway: 资源不存在")
	// ErrRateLimited 请求被限流。
	ErrRateLimited = errors.New("gateway: 请求被限流")
	// ErrServiceUnavailable 网关未就绪（服务端依赖未装配，503）。
	ErrServiceUnavailable = errors.New("gateway: 网关未就绪")
	// ErrDeviceCodeEmpty 调用方传了空的 device_code。
	ErrDeviceCodeEmpty = errors.New("gateway: device_code 为空")
	// ErrUserCodeNotUsable Login 收到了用户码：用户码要在网关授权页输入，
	// 不能换取令牌（V1 的 /v1/auth/device 只回 device_code 与 user_code，没有
	// "用用户码换令牌"的端点）。
	ErrUserCodeNotUsable = errors.New("gateway: 用户码不能直接换取令牌")
	// ErrNoModels 网关没有可用模型。
	ErrNoModels = errors.New("gateway: 网关没有可用的模型")
	// ErrModelUnavailable 指定模型不存在或已停用。
	ErrModelUnavailable = errors.New("gateway: 指定模型不可用")
)

// APIError 是网关返回的结构化错误体 {"error":{"message","type","code","request_id"}}。
//
// Message/Body 在构造时已用本次请求携带的凭据做过掩码，并截断到 maxErrorBody，
// 因此可以安全地进日志与终端。
type APIError struct {
	Status    int
	Code      string
	Message   string
	Type      string
	RequestID string
	// Body 只在响应体解不出结构化错误体时填充（例如反代返回的 HTML 页）。
	Body string
}

func (e *APIError) Error() string {
	if e == nil {
		return "<nil>"
	}
	msg := e.Message
	if msg == "" {
		msg = strings.TrimSpace(e.Body)
	}
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "HTTP %d", e.Status)
	if e.Code != "" {
		b.WriteString(" " + e.Code)
	}
	b.WriteString(": " + msg)
	if e.RequestID != "" {
		b.WriteString("（request_id=" + e.RequestID + "）")
	}
	return b.String()
}

// Unwrap 把服务端业务错误码翻译成哨兵，使 errors.Is 判定与错误码解耦。
func (e *APIError) Unwrap() error { return sentinelFor(e.Status, e.Code) }

// IsCode 判定错误是否带某个业务错误码（可传多个，任一命中即真）。
func (e *APIError) IsCode(codes ...string) bool {
	if e == nil {
		return false
	}
	for _, c := range codes {
		if e.Code == c {
			return true
		}
	}
	return false
}

// sentinelFor 是错误码/状态码到哨兵的映射。
//
// 主要依据 httpx.StatusFor（服务端真实返回的码）：
//
//	authorization_pending -> 400、credential_expired -> 401、credential_revoked -> 401、
//	invalid_api_key -> 401、account_disabled -> 403、insufficient_quota -> 402、
//	not_found -> 404、service_unavailable -> 503。
//
// 另外兼容若干历史/兼容码（invalid_grant、expired_token、device_code_expired、
// access_denied、slow_down）：服务端当前不会返回它们，但 OAuth 设备流的实现之间
// 容易互相漂移，容忍它们比把用户卡在"未知错误"里更好。
func sentinelFor(status int, code string) error {
	switch code {
	case "authorization_pending":
		return ErrAuthorizationPending
	case "slow_down":
		return ErrSlowDown
	case "credential_expired", "expired_token", "device_code_expired":
		return ErrCredentialExpired
	case "credential_revoked", "invalid_grant", "access_denied":
		return ErrCredentialRevoked
	case "invalid_api_key", "invalid_client", "unauthorized":
		return ErrUnauthorized
	case "account_disabled":
		return ErrAccountDisabled
	case "insufficient_quota":
		return ErrInsufficientQuota
	case "not_found":
		return ErrNotFound
	case "rate_limited":
		return ErrRateLimited
	case "service_unavailable":
		return ErrServiceUnavailable
	}
	switch status {
	case http.StatusUnauthorized:
		return ErrUnauthorized
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusPaymentRequired:
		return ErrInsufficientQuota
	case http.StatusTooManyRequests:
		return ErrRateLimited
	case http.StatusServiceUnavailable:
		return ErrServiceUnavailable
	}
	return nil
}

// redactedError 给错误文本套上凭据掩码，同时保留错误链（Unwrap），
// 使 errors.Is/As 与"绝不输出明文凭据"两条要求同时成立。
type redactedError struct {
	err     error
	secrets []string
}

func (e *redactedError) Error() string { return Redact(e.err.Error(), e.secrets...) }

func (e *redactedError) Unwrap() error { return e.err }

// redactErr 用 secrets 掩码包装 err（nil 进 nil 出）。
func redactErr(err error, secrets ...string) error {
	if err == nil || len(secrets) == 0 {
		return err
	}
	return &redactedError{err: err, secrets: secrets}
}

// errorWire 是服务端错误体的形状（httpx.ErrorBody）。
type errorWire struct {
	Error struct {
		Message   string `json:"message"`
		Type      string `json:"type"`
		Code      string `json:"code"`
		RequestID string `json:"request_id"`
	} `json:"error"`
}

// newAPIError 解析错误响应体。解不出结构化错误体时把响应体截断后作为 Body
// （仍可能是 HTML 页），并按 secrets 掩码 —— 绝不让凭据原文进到错误文本里。
func newAPIError(status int, raw []byte, secrets ...string) *APIError {
	ae := &APIError{Status: status}
	var wire errorWire
	if err := json.Unmarshal(raw, &wire); err == nil && wire.Error.Code != "" {
		ae.Code = wire.Error.Code
		ae.Type = wire.Error.Type
		ae.Message = Redact(wire.Error.Message, secrets...)
		ae.RequestID = wire.Error.RequestID
		return ae
	}
	body := raw
	if len(body) > maxErrorBody {
		body = body[:maxErrorBody]
	}
	trimmed := strings.TrimSpace(string(body))
	if trimmed != "" {
		ae.Body = Redact(trimmed, secrets...)
	}
	return ae
}
