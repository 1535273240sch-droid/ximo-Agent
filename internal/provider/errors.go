// errors.go —— LLM 调用错误分类（架构文档第 18 章错误分类表的照抄实现）。
//
// 分类结果直接决定 RetryPolicy 的行为，是全模块最需要代码审查的一段：
// 分类错了会导致「400 被重试」或「429 不重试」这类线上事故。
package provider

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

// ErrorClass 错误类别。与第 18 章分类表一一对应。
type ErrorClass int

const (
	// ClassUnknown 未分类错误 —— 保守处理为不可重试（避免对未知副作用重试）。
	ClassUnknown ErrorClass = iota
	// ClassDNS DNS 临时失败 —— 可重试。
	ClassDNS
	// ClassConnectTimeout 连接超时 —— 可重试。
	ClassConnectTimeout
	// ClassRateLimit 429 —— 可重试且遵守 Retry-After。
	ClassRateLimit
	// ClassServerError 500/502/503/504 —— 可重试。
	ClassServerError
	// ClassBadRequest 400 —— 不可重试。
	ClassBadRequest
	// ClassAuthInvalidKey invalid API key —— 不可重试。
	ClassAuthInvalidKey
	// ClassInvalidToolSchema invalid tool schema —— 不可重试。
	ClassInvalidToolSchema
	// ClassContextTooLong context too long —— 不可重试，先触发 compact。
	ClassContextTooLong
	// ClassUserCancelled user cancelled —— 不可重试。
	ClassUserCancelled
	// ClassToolSideEffect tool side effect 已提交 —— 不自动重试（重试会重复副作用）。
	ClassToolSideEffect
)

func (c ErrorClass) String() string {
	switch c {
	case ClassDNS:
		return "dns"
	case ClassConnectTimeout:
		return "connect_timeout"
	case ClassRateLimit:
		return "rate_limit"
	case ClassServerError:
		return "server_error"
	case ClassBadRequest:
		return "bad_request"
	case ClassAuthInvalidKey:
		return "invalid_api_key"
	case ClassInvalidToolSchema:
		return "invalid_tool_schema"
	case ClassContextTooLong:
		return "context_too_long"
	case ClassUserCancelled:
		return "user_cancelled"
	case ClassToolSideEffect:
		return "tool_side_effect"
	default:
		return "unknown"
	}
}

// APIError 服务端返回的结构化错误（非 2xx 响应）。
type APIError struct {
	StatusCode int
	// Code 服务商错误码（如 DeepSeek 的 "invalid_request_error"）。
	Code string
	// Message 已脱敏的错误描述 —— 调用方保证这里不含 API Key。
	Message string
	// RetryAfter 服务端要求的等待时长（解析自 Retry-After 头）。
	RetryAfter time.Duration
	// HasRetryAfter 是否真的带了这个头（0 值有歧义，单独标记）。
	HasRetryAfter bool
}

func (e *APIError) Error() string {
	return fmt.Sprintf("provider: HTTP %d: %s", e.StatusCode, e.Message)
}

// ErrContextCancelled 上下文被取消（用户取消或上层超时）。
var ErrContextCancelled = errors.New("provider: 请求已取消")

// ErrToolSideEffectCommitted 标记「副作用已提交」的工具调用。
//
// 由任务 04 的工具运行时在副作用落盘后包一层返回；本层见到它一律不重试。
var ErrToolSideEffectCommitted = errors.New("provider: 工具副作用已提交，不自动重试")

// ClassifiedError 带分类信息的错误包装。
type ClassifiedError struct {
	Class ErrorClass
	Err   error
	// RetryAfter 仅 ClassRateLimit 有意义。
	RetryAfter time.Duration
}

func (e *ClassifiedError) Error() string {
	if e.Err == nil {
		return e.Class.String()
	}
	return fmt.Sprintf("%s: %v", e.Class, e.Err)
}

func (e *ClassifiedError) Unwrap() error { return e.Err }

// Classify 对任意错误做分类。这是错误分类表的唯一实现点。
//
// 判定顺序很关键 —— 先判「绝不可重试」的确定性错误，再判可重试的传输层错误，
// 否则 400 里的 "timeout" 字样会被误判成 connect timeout。
func Classify(err error) *ClassifiedError {
	if err == nil {
		return &ClassifiedError{Class: ClassUnknown}
	}

	// 0) 用户取消 / 上层超时 —— 最高优先级，任何包装都不能覆盖。
	if errors.Is(err, context.Canceled) || errors.Is(err, ErrContextCancelled) {
		return &ClassifiedError{Class: ClassUserCancelled, Err: err}
	}

	// 1) 副作用已提交 —— 显式标记，永不自动重试。
	if errors.Is(err, ErrToolSideEffectCommitted) {
		return &ClassifiedError{Class: ClassToolSideEffect, Err: err}
	}

	// 2) 结构化 APIError —— 按 HTTP 状态码 + 错误码精确分类。
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		class := classifyAPIError(apiErr)
		return &ClassifiedError{Class: class, Err: err, RetryAfter: apiErr.RetryAfter}
	}

	// 3) 网络层错误 —— 用 net.Error 判定，比字符串匹配可靠。
	//    context.DeadlineExceeded 归入 connect timeout（可重试）。
	if errors.Is(err, context.DeadlineExceeded) {
		return &ClassifiedError{Class: ClassConnectTimeout, Err: err}
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() {
			return &ClassifiedError{Class: ClassConnectTimeout, Err: err}
		}
		// 非超时的网络错误按 DNS 处理（临时失败可重试），
		// 但排除「连接被拒绝」这类确定性失败。
		if isDNSError(netErr) {
			return &ClassifiedError{Class: ClassDNS, Err: err}
		}
	}

	// 4) 字符串兜底 —— 兼容第三方服务商把错误塞在 message 里的情况。
	return &ClassifiedError{Class: classifyByMessage(err.Error()), Err: err}
}

// classifyAPIError 按状态码与错误码分类（对应第 18 章表格）。
func classifyAPIError(e *APIError) ErrorClass {
	msg := strings.ToLower(e.Message + " " + e.Code)

	switch e.StatusCode {
	case 429:
		return ClassRateLimit
	case 400:
		// 400 需要细分：context too long / invalid tool schema 都有自己的处置方式，
		// 虽然都是「不重试」，但上层要据此触发 compact 或修 schema。
		switch {
		case containsAny(msg, "context length", "context_length_exceeded", "too long",
			"maximum context", "context window", "token limit"):
			return ClassContextTooLong
		case containsAny(msg, "tool", "schema", "function", "parameters"):
			return ClassInvalidToolSchema
		default:
			return ClassBadRequest
		}
	case 401, 403:
		return ClassAuthInvalidKey
	case 500, 502, 503, 504:
		return ClassServerError
	}

	// 状态码之外的显式错误码。
	switch {
	case containsAny(msg, "invalid api key", "invalid_api_key", "incorrect api key",
		"authentication", "unauthorized"):
		return ClassAuthInvalidKey
	case containsAny(msg, "context length", "context_length_exceeded", "too long",
		"maximum context", "context window", "token limit"):
		return ClassContextTooLong
	case containsAny(msg, "invalid tool", "tool schema", "invalid schema"):
		return ClassInvalidToolSchema
	}

	// 4xx 除 429 外一律不重试；5xx 可重试。
	if e.StatusCode >= 500 {
		return ClassServerError
	}
	if e.StatusCode >= 400 {
		return ClassBadRequest
	}
	// 状态码缺失（0 或非 HTTP 错误被包装成 APIError）→ 退回消息文本判定。
	return classifyByMessage(msg)
}

// classifyByMessage 无结构化信息时的字符串兜底分类。
func classifyByMessage(msg string) ErrorClass {
	m := strings.ToLower(msg)
	switch {
	case containsAny(m, "user cancelled", "user canceled", "context canceled",
		"客户端取消", "用户取消"):
		return ClassUserCancelled
	case containsAny(m, "invalid api key", "invalid_api_key", "unauthorized", "401"):
		return ClassAuthInvalidKey
	case containsAny(m, "invalid tool", "tool schema", "invalid schema"):
		return ClassInvalidToolSchema
	case containsAny(m, "context length", "too long", "maximum context", "token limit"):
		return ClassContextTooLong
	case containsAny(m, "429", "rate limit", "too many requests", "限频"):
		return ClassRateLimit
	case containsAny(m, "no such host", "dns", "temporary failure in name resolution",
		"server misbehaving"):
		return ClassDNS
	case containsAny(m, "timeout", "timed out", "deadline exceeded", "超时"):
		return ClassConnectTimeout
	case containsAny(m, "500", "502", "503", "504", "bad gateway",
		"service unavailable", "internal server error"):
		return ClassServerError
	case containsAny(m, "400", "bad request"):
		return ClassBadRequest
	default:
		return ClassUnknown
	}
}

// isDNSError 判定是否 DNS 解析类失败。
func isDNSError(err net.Error) bool {
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr)
}

func containsAny(haystack string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}
