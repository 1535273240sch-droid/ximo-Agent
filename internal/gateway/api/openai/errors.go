package openai

import (
	"context"
	"errors"
	"strings"

	"github.com/ximo888ok-netizen/ximo-agent/internal/provider"
)

// upstreamFailure 描述一次终止性上游失败如何回给客户端。
type upstreamFailure struct {
	Status  int
	Code    string
	Message string
	// RetryAfter 秒数；0 表示不带 Retry-After 响应头。
	RetryAfter int64
}

// describeUpstreamFailure 把上游错误映射为对外响应。
//
// 原则：
//   - 上游的鉴权/配置错误**不能**回 401/403 —— 客户端手里的凭据是有效的，回 401 会让
//     客户端去换自己的 key，把运维问题伪装成用户问题；统一 502。
//   - 400/413/422 是客户端请求本身被上游拒绝，回 400 并带上（脱敏、截断后的）上游说明，
//     否则客户端无从修正请求。
//   - 其余（5xx / 网络 / 未知）统一 502 provider_unavailable，不泄露上游细节。
func describeUpstreamFailure(err error) upstreamFailure {
	fallback := upstreamFailure{
		Status:  502,
		Code:    "provider_unavailable",
		Message: "上游服务不可用，请稍后重试",
	}
	var apiErr *provider.APIError
	if !errors.As(err, &apiErr) {
		return fallback
	}
	switch apiErr.StatusCode {
	case 400, 413, 422:
		msg := sanitizeUpstreamMessage(apiErr.Message)
		if msg == "" {
			msg = "上游拒绝了该请求"
		}
		return upstreamFailure{Status: 400, Code: "invalid_request_error", Message: msg}
	case 401, 403:
		return upstreamFailure{Status: 502, Code: "upstream_auth_failed", Message: "上游服务商鉴权失败，请联系管理员"}
	case 404:
		return upstreamFailure{Status: 502, Code: "upstream_model_unavailable", Message: "上游未找到该模型（目录映射可能已过期）"}
	case 429:
		msg := sanitizeUpstreamMessage(apiErr.Message)
		if msg == "" {
			msg = "上游限流，请稍后重试"
		}
		out := upstreamFailure{Status: 429, Code: "rate_limit_exceeded", Message: msg}
		if apiErr.RetryAfter > 0 {
			secs := int64(apiErr.RetryAfter.Seconds())
			if secs < 1 {
				secs = 1
			}
			out.RetryAfter = secs
		}
		return out
	default:
		return fallback
	}
}

// sanitizeUpstreamMessage 清洗上游错误文案：折叠空白、截断长度、抹掉可能夹带的
// URL 与凭据片段。上游正文是第三方文本，直接回给客户端之前必须先过这一道。
func sanitizeUpstreamMessage(msg string) string {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return ""
	}
	fields := strings.Fields(msg)
	for i, f := range fields {
		if looksLikeURLOrSecret(f) {
			fields[i] = "[已隐藏]"
		}
	}
	out := strings.Join(fields, " ")
	const maxRunes = 512
	if r := []rune(out); len(r) > maxRunes {
		out = string(r[:maxRunes]) + "…"
	}
	return out
}

// looksLikeURLOrSecret 判定一个词是否像 URL 或凭据（大小写不敏感）。
func looksLikeURLOrSecret(token string) bool {
	lower := strings.ToLower(token)
	switch {
	case strings.HasPrefix(lower, "http://"), strings.HasPrefix(lower, "https://"):
		return true
	case strings.Contains(lower, "bearer"):
		return true
	case strings.Contains(lower, "api_key="), strings.Contains(lower, "apikey="), strings.Contains(lower, "api-key="):
		return true
	case strings.Contains(lower, "sk-"):
		return true
	case strings.Contains(lower, "authorization"):
		return true
	default:
		return false
	}
}

// isClientCanceled 判定失败是否源于本请求被取消/超时（客户端断开或中间件的请求超时）。
//
// r.Context() 是上游调用的祖先 context：它结束就说明这次失败不是上游的问题，
// 不该换候选、也不该把它算到 provider 头上。
func isClientCanceled(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return true
	}
	return provider.Classify(err).Class == provider.ClassUserCancelled
}

// outcomeFor 给出 usage 行的终态标签（§11.5 词表）。
func outcomeFor(ctx context.Context, err error) string {
	if ctx.Err() != nil {
		return OutcomeClientCanceled
	}
	if err != nil && provider.Classify(err).Class == provider.ClassConnectTimeout {
		return OutcomeUpstreamTimeout
	}
	return OutcomeUpstreamError
}
