// Package httpx 提供 XIMO 中转站 HTTP 层的共享原语：路由描述、JSON 收发、错误映射、请求 ID。
//
// 本包由主代理冻结：各 api 子包（api/meta、api/openai、api/anthropic、api/admin）都要用它，
// 任何子代理不得修改本包签名，需要新能力请报告。
package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"

	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/model"
)

type ctxKey int

const requestIDKey ctxKey = iota

// Route 是一个已绑定的路由：Pattern 用 Go 1.22+ ServeMux 语法，例如 "GET /v1/models"、"POST /admin/users/{id}/status"。
type Route struct {
	Pattern string
	Handler http.HandlerFunc
}

// ErrorDetail/ErrorBody 是统一的错误响应体，同时兼容 OpenAI 风格的 {"error":{...}}。
type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

type ErrorDetail struct {
	Message   string `json:"message"`
	Type      string `json:"type"`
	Code      string `json:"code"`
	RequestID string `json:"request_id,omitempty"`
}

// WithRequestID / RequestID 在 context 中传递请求 ID（贯穿日志与上游调用）。
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

func RequestID(r *http.Request) string {
	if v, ok := r.Context().Value(requestIDKey).(string); ok {
		return v
	}
	return ""
}

// WriteJSON 写 JSON 响应；编码失败时尽力返回 500，不 panic。
func WriteJSON(w http.ResponseWriter, status int, v any) {
	buf, err := json.Marshal(v)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"response encode failed","type":"server_error","code":"encode_failed"}}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(buf)
}

// WriteError 写统一错误体，并把请求 ID 回带给客户端便于追查。
func WriteError(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	WriteJSON(w, status, ErrorBody{Error: ErrorDetail{
		Message:   msg,
		Type:      TypeForStatus(status),
		Code:      code,
		RequestID: RequestID(r),
	}})
}

// DecodeJSON 解析请求体；失败时已写好 400 响应并返回 false，调用方直接 return。
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if r.Body == nil {
		WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "empty request body")
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<20))
	if err := dec.Decode(dst); err != nil {
		WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "malformed JSON body: "+err.Error())
		return false
	}
	return true
}

// Bearer 取出 Authorization: Bearer <token>；缺失返回空串。
func Bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const p = "Bearer "
	if len(h) > len(p) && strings.EqualFold(h[:len(p)], p) {
		return strings.TrimSpace(h[len(p):])
	}
	return ""
}

// ClientIP 用于审计日志；优先 X-Forwarded-For 首段，回退 RemoteAddr，去掉端口。
func ClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i > 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// StatusFor 把哨兵错误映射为 HTTP 状态码与错误码。
// 未识别的错误一律 500 + server_error，绝不把内部错误文本当业务错误静默降级。
func StatusFor(err error) (status int, code string) {
	switch {
	case err == nil:
		return http.StatusOK, ""
	case errors.Is(err, model.ErrInsufficientQuota):
		return http.StatusPaymentRequired, "insufficient_quota"
	case errors.Is(err, model.ErrBadCredentials):
		return http.StatusUnauthorized, "invalid_api_key"
	case errors.Is(err, model.ErrRevoked):
		return http.StatusUnauthorized, "credential_revoked"
	case errors.Is(err, model.ErrExpired):
		return http.StatusUnauthorized, "credential_expired"
	case errors.Is(err, model.ErrAuthorizationPending):
		return http.StatusBadRequest, "authorization_pending"
	case errors.Is(err, model.ErrDisabled):
		return http.StatusForbidden, "account_disabled"
	case errors.Is(err, model.ErrNotFound):
		return http.StatusNotFound, "not_found"
	case errors.Is(err, model.ErrConflict):
		return http.StatusConflict, "conflict"
	default:
		return http.StatusInternalServerError, "server_error"
	}
}

// WriteMappedError 是 WriteError 的发送方：按 StatusFor 决定状态码，未识别错误用传入的兜底文案（避免泄露内部细节）。
func WriteMappedError(w http.ResponseWriter, r *http.Request, err error, fallbackMsg string) {
	status, code := StatusFor(err)
	if status == http.StatusOK {
		status, code = http.StatusInternalServerError, "server_error"
	}
	msg := fallbackMsg
	if msg == "" {
		msg = code
	}
	WriteError(w, r, status, code, msg)
}

// TypeForStatus 给出 OpenAI 风格的错误类型串。
func TypeForStatus(status int) string {
	switch {
	case status == http.StatusBadRequest:
		return "invalid_request_error"
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return "authentication_error"
	case status == http.StatusNotFound:
		return "not_found_error"
	case status == http.StatusConflict:
		return "conflict_error"
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
