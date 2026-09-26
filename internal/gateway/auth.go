package gateway

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/httpx"
	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/model"
	"github.com/ximo888ok-netizen/ximo-agent/internal/observability"
)

// 凭据明文前缀。这些常量在 internal/account/token.go 里未导出，此处按契约 §5 复述，
// 只用于「选哪条校验路径」，不作为安全边界（真正的判定在 account 服务里）。
const (
	credentialPrefixAPIKey  = "ximo_sk_"
	credentialPrefixAccess  = "gwa_"
	credentialPrefixRefresh = "gwr_"
	credentialPrefixDevice  = "gwd_"

	// HeaderAdminToken 是管理接口的令牌头（契约 §11.3）。
	HeaderAdminToken = "X-Admin-Token"
)

// Principal.Kind 取值。
const (
	PrincipalUser  = "user"
	PrincipalAdmin = "admin"
)

// Principal.Credential 取值（审计与日志用，不含令牌本身）。
const (
	CredAPIKey      = "api_key"
	CredAccessToken = "access_token"
	CredAdminToken  = "admin_token"
)

// ErrVerifierUnavailable 表示未注入凭据校验器（装配错误）。
var ErrVerifierUnavailable = errors.New("gateway: 未注入凭据校验器")

// ErrAdminTokenNotConfigured 表示进程未配置管理令牌。管理路由此时一律 401（fail closed）。
var ErrAdminTokenNotConfigured = errors.New("gateway: 未配置管理令牌")

// CredentialVerifier 是本包需要的凭据校验能力（窄接口由消费方定义，契约 §10.2）。
// internal/account.Service 的 VerifyAPIKey / VerifyAccess 签名与本接口逐字一致，可直接传入。
//
// 约定：凭据本身的问题一律用 model.* 哨兵错误表达
// （ErrBadCredentials / ErrExpired / ErrRevoked / ErrDisabled），本包据此决定 HTTP 状态。
type CredentialVerifier interface {
	VerifyAPIKey(ctx context.Context, plain string) (model.User, model.APIKey, error)
	VerifyAccess(ctx context.Context, accessToken string) (model.User, error)
}

// Principal 是通过鉴权的请求主体。它只携带**非机密**信息：用户行、密钥行（不含明文与哈希）
// 与凭据种类，可以安全地写进日志与审计。
type Principal struct {
	Kind       string
	User       model.User
	Key        model.APIKey
	Credential string
}

func (p Principal) IsAdmin() bool { return p.Kind == PrincipalAdmin }

func (p Principal) UserID() string { return p.User.ID }

// RateKey 是限流分桶键：优先 API Key（同一用户的多个 Key 各自计量），
// 其次用户、最后管理令牌（所有管理操作共享一个桶）。空键由调用方按 IP 兜底。
func (p Principal) RateKey() string {
	switch {
	case p.Key.ID != "":
		return "key:" + p.Key.ID
	case p.User.ID != "":
		return "user:" + p.User.ID
	case p.IsAdmin():
		return "admin"
	default:
		return ""
	}
}

type principalCtxKey struct{}

// WithPrincipal / PrincipalFrom 在 context 中传递已鉴权主体。
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalCtxKey{}, p)
}

func PrincipalFrom(ctx context.Context) (Principal, bool) {
	if ctx == nil {
		return Principal{}, false
	}
	p, ok := ctx.Value(principalCtxKey{}).(Principal)
	return p, ok
}

// Authenticator 做两件事：用户态凭据校验（Bearer）与管理态令牌校验（X-Admin-Token）。
//
// 管理令牌只保留 SHA-256 摘要：比较在固定长度上进行，且字符串长度不构成旁路信息。
// 明文只存在于 Config 与构造参数里，本类型不长期驻留明文。
type Authenticator struct {
	verifier        CredentialVerifier
	logger          *observability.Logger
	adminDigest     [sha256.Size]byte
	adminConfigured bool
}

// NewAuthenticator 构造鉴权器。verifier 为 nil 是装配错误，直接 panic
// （与 internal/account.New 对 nil store 的处理一致），避免每个请求各自崩。
func NewAuthenticator(verifier CredentialVerifier, adminToken string, logger *observability.Logger) *Authenticator {
	if verifier == nil {
		panic("gateway: NewAuthenticator 需要非 nil 的 CredentialVerifier")
	}
	a := &Authenticator{verifier: verifier, logger: nonNilLogger(logger)}
	if adminToken != "" {
		a.adminDigest = sha256.Sum256([]byte(adminToken))
		a.adminConfigured = true
	}
	return a
}

// AdminTokenConfigured 报告是否配置了管理令牌。启动时应据此拒绝挂载 /admin/*。
func (a *Authenticator) AdminTokenConfigured() bool { return a.adminConfigured }

// Authenticate 解析 Authorization: Bearer <token>，按前缀分派：
// "ximo_sk_" → API Key，"gwa_" → 会话 access token，其它一律 ErrBadCredentials。
//
// 失败时的错误仍是 model.* 哨兵（可 errors.Is），文本不含令牌。
func (a *Authenticator) Authenticate(r *http.Request) (Principal, error) {
	if a.verifier == nil {
		return Principal{}, ErrVerifierUnavailable
	}
	token := httpx.Bearer(r)
	if token == "" {
		return Principal{}, model.ErrBadCredentials
	}
	ctx := r.Context()
	switch {
	case strings.HasPrefix(token, credentialPrefixAPIKey):
		u, k, err := a.verifier.VerifyAPIKey(ctx, token)
		if err != nil {
			return Principal{}, fmt.Errorf("gateway: API Key 校验失败: %w", err)
		}
		return Principal{Kind: PrincipalUser, User: u, Key: k, Credential: CredAPIKey}, nil
	case strings.HasPrefix(token, credentialPrefixAccess):
		u, err := a.verifier.VerifyAccess(ctx, token)
		if err != nil {
			return Principal{}, fmt.Errorf("gateway: access token 校验失败: %w", err)
		}
		return Principal{Kind: PrincipalUser, User: u, Credential: CredAccessToken}, nil
	default:
		return Principal{}, model.ErrBadCredentials
	}
}

// AuthenticateAdmin 以恒定时间比较 X-Admin-Token。
// 缺头、空值、值不符、未配置令牌 → 统一错误（对客户端一律 401，不给区分信号）。
func (a *Authenticator) AuthenticateAdmin(r *http.Request) error {
	if !a.adminConfigured {
		return ErrAdminTokenNotConfigured
	}
	got := strings.TrimSpace(r.Header.Get(HeaderAdminToken))
	if got == "" {
		return model.ErrBadCredentials
	}
	digest := sha256.Sum256([]byte(got))
	if subtle.ConstantTimeCompare(a.adminDigest[:], digest[:]) != 1 {
		return model.ErrBadCredentials
	}
	return nil
}

// RequireUser 是用户态路由的包装：认证通过则把 Principal 注入 context 后再交给 next。
// 失败的凭据只以「前缀 + 长度」写日志（见 tokenHint），任何 token 明文都不进日志。
func (a *Authenticator) RequireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := a.Authenticate(r)
		if err != nil {
			a.logger.Warn(r.Context(), "gateway: 用户凭据校验失败", map[string]any{
				"path":            r.URL.Path,
				"ip":              httpx.ClientIP(r),
				"request_id":      httpx.RequestID(r),
				"credential_hint": tokenHint(httpx.Bearer(r)),
				"reason":          redactTokens(err.Error()),
			})
			httpx.WriteMappedError(w, r, err, "invalid or missing credentials")
			return
		}
		next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
	})
}

// RequireAdmin 是管理态路由的包装：校验 X-Admin-Token，通过后注入 Kind=admin 的主体
// （各 admin handler 据此写审计 actor）。失败一律 401，且**不记录令牌内容**。
func (a *Authenticator) RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := a.AuthenticateAdmin(r); err != nil {
			a.logger.Warn(r.Context(), "gateway: 管理令牌校验失败", map[string]any{
				"path":          r.URL.Path,
				"ip":            httpx.ClientIP(r),
				"request_id":    httpx.RequestID(r),
				"token_present": r.Header.Get(HeaderAdminToken) != "",
				"reason":        err.Error(),
			})
			httpx.WriteError(w, r, http.StatusUnauthorized, "invalid_admin_token", "invalid or missing admin token")
			return
		}
		p := Principal{Kind: PrincipalAdmin, Credential: CredAdminToken}
		next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
	})
}

// RequireUserRoutes 批量包装用户态路由（main 装配时用，见包文档）。
func (a *Authenticator) RequireUserRoutes(routes []httpx.Route) []httpx.Route {
	return wrapRoutes(routes, a.RequireUser)
}

// RequireAdminRoutes 批量包装管理态路由。未配置管理令牌时全部 401，
// 因此「忘了配令牌」不会静默变成「后台全公开」。
func (a *Authenticator) RequireAdminRoutes(routes []httpx.Route) []httpx.Route {
	return wrapRoutes(routes, a.RequireAdmin)
}

func wrapRoutes(routes []httpx.Route, mw func(http.Handler) http.Handler) []httpx.Route {
	out := make([]httpx.Route, 0, len(routes))
	for _, rt := range routes {
		if rt.Handler == nil {
			out = append(out, rt)
			continue
		}
		out = append(out, httpx.Route{Pattern: rt.Pattern, Handler: http.HandlerFunc(mw(rt.Handler).ServeHTTP)})
	}
	return out
}

// tokenHint 生成可安全进日志的凭据提示：只保留设计上公开的明文前缀与总长度，
// 其余（真正承载熵的部分）一律丢弃。非本产品的凭据格式一律只报 redacted。
//
// 调用方必须把它挂在**不含 credential/token 字样的字段名**下：observability 的
// 脱敏器对这类键名一律整体替换为 [REDACTED]，值再安全也读不到。
func tokenHint(token string) string {
	if token == "" {
		return ""
	}
	for _, p := range []string{credentialPrefixAPIKey, credentialPrefixAccess, credentialPrefixRefresh, credentialPrefixDevice} {
		if strings.HasPrefix(token, p) {
			return fmt.Sprintf("%s…(len=%d)", p, len(token))
		}
	}
	return fmt.Sprintf("redacted(len=%d)", len(token))
}

// tokenPattern 匹配本产品的三类明文凭据（API Key / 会话 access / 会话 refresh / 设备码）。
var tokenPattern = regexp.MustCompile(`(?:` + credentialPrefixAPIKey + `|` + credentialPrefixAccess +
	`|` + credentialPrefixRefresh + `|` + credentialPrefixDevice + `)[A-Za-z0-9_-]+`)

// redactTokens 从任意自由文本里抹掉明文凭据。
//
// 存在的理由：internal/observability 与 internal/secrets 的脱敏器覆盖的是
// OpenAI/Anthropic/GitHub 等**第三方**密钥格式，正则不匹配 ximo_sk_ / gwa_ 前缀；
// panic 值、net/http 内部错误、错误链文本都可能夹带这类凭据，因此写日志前必须先过这一层。
func redactTokens(s string) string {
	if s == "" {
		return s
	}
	return tokenPattern.ReplaceAllString(s, "[REDACTED]")
}
