package anthropic

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/httpx"
	"github.com/ximo888ok-netizen/ximo-agent/internal/provider"
)

// usage 终态取值（契约 §11.5 的「流式三态」）。
//
// 注意：**成功路径**的 usage.Status 由 internal/quota 固定写为 "settled"
// （quota.UsageStatusSettled，契约 §11.1.2 强制 HTTP 层走 SettleWithUsage），
// 因此这里只列失败终态；"ok" 在本包里不会出现，统一口径需要主代理裁决。
const (
	usageStatusUpstreamError   = "upstream_error"
	usageStatusUpstreamTimeout = "upstream_timeout"
	usageStatusClientCanceled  = "client_canceled"
)

// cleanupTimeout 是收尾操作（结算/释放/落 usage）的超时。收尾必须脱离请求上下文
// （客户端一断开，r.Context() 就取消，用它写库会全部失败），但也不能无限期挂着。
const cleanupTimeout = 5 * time.Second

// requestError 是入站请求校验失败：带 HTTP 状态码与错误码，交给 httpx.WriteError 输出。
// 响应体形如 {"error":{"message","type":"invalid_request_error","code"}}，
// Anthropic SDK 解析错误时读的正是 error.type/message，故形状兼容。
type requestError struct {
	status  int
	code    string
	message string
}

func (e *requestError) Error() string { return e.code + ": " + e.message }

// badRequest 构造 400 校验错误。code 用参数名或缺口名（如 unsupported_content_block），
// 便于客户端与日志定位。
func badRequest(code, format string, args ...any) *requestError {
	return &requestError{
		status:  http.StatusBadRequest,
		code:    code,
		message: fmt.Sprintf(format, args...),
	}
}

// cleanupCtx 返回脱离请求取消的收尾上下文：客户端断开后仍必须完成释放/结算/落 usage。
func cleanupCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
}

// retryableForCandidate 判定一次上游失败是否值得换下一个候选。
//
// 复用 provider 的分类表（§11.4：不可重试错误不换候选、直接返回并释放预占）。
// ClassUnknown 保守视为不可重试 —— 未知错误可能已产生副作用。
func retryableForCandidate(err error) bool {
	if err == nil {
		return false
	}
	return provider.Retryable(provider.Classify(err).Class)
}

// failureStatus 把上游错误映射为 usage 终态（契约 §11.5）。
//
// 判定顺序：客户端先走（ctx 已取消）→ 超时 → 其余归 upstream_error。
func failureStatus(err, ctxErr error) string {
	switch {
	case errors.Is(ctxErr, context.Canceled), errors.Is(err, context.Canceled):
		return usageStatusClientCanceled
	case errors.Is(ctxErr, context.DeadlineExceeded), errors.Is(err, context.DeadlineExceeded):
		return usageStatusUpstreamTimeout
	}
	if ce := provider.Classify(err); ce.Class == provider.ClassConnectTimeout {
		return usageStatusUpstreamTimeout
	}
	return usageStatusUpstreamError
}

// writeUpstreamError 把上游失败翻成客户端 HTTP 错误。
//
// 客户端可见文案一律是固定串：上游错误消息可能回显请求内容，直接透传等于把上游细节
// 暴露给调用方；真实错误只进日志（observability 会再脱敏一次）。
// 状态码选择：4xx 类判定为调用方问题就回 4xx；上游自身故障（含上游 key 配错）回 5xx，
// 不把「网关自己的上游密钥失效」伪装成「你的密钥无效」。
func writeUpstreamError(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		// 客户端已断开，写什么都没人收。
		return
	}
	switch provider.Classify(err).Class {
	case provider.ClassBadRequest:
		httpx.WriteError(w, r, http.StatusBadRequest, "upstream_bad_request", "upstream rejected the request as invalid")
	case provider.ClassInvalidToolSchema:
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_tool_schema", "upstream rejected the tool schema")
	case provider.ClassContextTooLong:
		httpx.WriteError(w, r, http.StatusBadRequest, "context_too_long", "request exceeds the upstream context window")
	case provider.ClassRateLimit:
		httpx.WriteError(w, r, http.StatusTooManyRequests, "rate_limited", "upstream rate limit exceeded")
	case provider.ClassAuthInvalidKey:
		httpx.WriteError(w, r, http.StatusBadGateway, "upstream_auth_error", "gateway upstream credential is invalid")
	default:
		httpx.WriteError(w, r, http.StatusBadGateway, "provider_unavailable", "no upstream provider could serve this request")
	}
}
