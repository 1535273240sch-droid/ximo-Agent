// Package meta 提供 XIMO 中转站的元信息与插件登录链路由（契约 §11.2 / §11.3）：
//
//	GET  /v1/health        存活探测（无鉴权）
//	GET  /v1/capabilities  协议能力与限额声明（无鉴权）
//	GET  /v1/models        对外模型目录（用户态）
//	GET  /v1/usage         当前用户名下的用量（用户态）
//	POST /v1/auth/device   插件登录链：申请设备码（文档 §5.2，无鉴权）
//	POST /v1/auth/token    插件登录链：轮询兑换令牌对（无鉴权）
//	POST /v1/auth/refresh  插件登录链：刷新令牌对（轮换，无鉴权）
//	POST /v1/auth/login    插件登录链：用户名口令登录（无鉴权）
//
// 职责边界：本包只做 HTTP 编解码与错误映射。鉴权（API Key / access token 的
// 校验）由 W-Server 的中间件完成，本包只通过 Deps.User 取「已鉴权用户 ID」；
// 账号与令牌语义全部在 internal/account，目录语义在 internal/gateway/catalog，
// 用量读取在 internal/gateway/store。本包不写任何表、不生成凭据、不判断额度，
// 也不做限流（§11.4 的限流在 W-Server 的 ratelimit.go）。
package meta

import (
	"context"
	"net/http"
	"strings"

	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/catalog"
	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/httpx"
	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/model"
	"github.com/ximo888ok-netizen/ximo-agent/internal/observability"
)

// DefaultVersion 是 Deps.Version 为空时上报的服务器版本串。真实构建号由装配方
// 注入（本包不猜版本号，也不去读构建期变量）。
const DefaultVersion = "dev"

// UsageStore 是本包需要的用量读取能力（窄接口由消费方定义，契约 §10.2）。
// *gwstore.Store 的方法签名与下面逐字一致，可直接传入 Deps.Store。
type UsageStore interface {
	// ListUsage 列出某用户用量（最近优先）。userID 由本包从鉴权结果取，绝不由客户端指定。
	ListUsage(ctx context.Context, userID string, limit, offset int) ([]model.UsageRecord, error)
}

// AccountService 是本包需要的账号能力（登录链）。
// *account.Service 的方法签名与下面逐字一致，可直接传入 Deps.Accounts。
type AccountService interface {
	// StartDeviceLogin 申请设备码，返回设备码、用户码与建议轮询间隔（毫秒）。
	StartDeviceLogin(ctx context.Context) (deviceCode, userCode string, intervalMS int64, err error)
	// PollDeviceLogin 轮询设备码；待授权返回 model.ErrAuthorizationPending，
	// 成功返回令牌对并（由 account 内部）一次性消费设备码。
	PollDeviceLogin(ctx context.Context, deviceCode string) (access, refresh string, err error)
	// Refresh 轮换令牌对，旧 refresh 立即失效。
	Refresh(ctx context.Context, refreshToken string) (access, refresh2 string, err error)
	// Authenticate 校验用户名口令；口令错误返回 model.ErrBadCredentials。
	Authenticate(ctx context.Context, username, password string) (model.User, error)
	// IssueSession 为已通过身份验证的用户签发令牌对。
	IssueSession(ctx context.Context, userID string) (access, refresh string, err error)
}

// CatalogService 是本包需要的目录能力。
// *catalog.Catalog 的方法签名与下面逐字一致，可直接传入 Deps.Catalog。
type CatalogService interface {
	// PublicModels 返回可公开的模型目录（只含 enabled）。
	PublicModels(ctx context.Context) ([]model.ModelSpec, error)
	// Candidates 返回某模型当前可用的上游候选（优先级升序），
	// 本包只用它推导 /v1/models 的 provider 与 protocols 字段。
	Candidates(ctx context.Context, modelID string) ([]catalog.Candidate, error)
}

// Logger 是本包需要的日志能力（窄接口）。*observability.Logger 直接满足它；
// 未装配时退化为 observability 的默认日志器。实现方必须自行脱敏（observability
// 的 Logger 会做敏感键与 Bearer 模式脱敏），本包不会把任何凭据写进 fields。
type Logger interface {
	Warn(ctx context.Context, msg string, fields ...map[string]any)
	Error(ctx context.Context, msg string, fields ...map[string]any)
}

// Deps 是本包的窄依赖集合，由装配方（cmd/ximo-gateway）填充。
//
// 刻意不含 quota.Service：本包这八个路由都不读额度（额度与账本展示属于
// /admin/* 与后续插件用量页），按「只放真正需要的依赖」不引入空字段。
type Deps struct {
	// Store 提供用量读取；未装配时 /v1/usage 返回 503。
	Store UsageStore
	// Accounts 提供登录链；未装配时 /v1/auth/* 返回 503。
	Accounts AccountService
	// Catalog 提供模型目录；未装配时 /v1/models 返回 503。
	Catalog CatalogService
	// Logger 记录降级与内部错误；nil 时用 observability 默认日志器。
	Logger Logger
	// Version 是 /v1/health 与 /v1/capabilities 上报的版本；空则用 DefaultVersion。
	Version string
	// User 取出当前请求已鉴权的用户 ID（W-Server 中间件的取值方式）。
	// 返回 ok=false 或空 ID 一律按「未鉴权」处理（401），不做匿名降级。
	// nil 时使用本包的 UserID（读 WithUserID 写入 context 的值）。
	//
	// 装配示例（把本包的用户态路由交给 W-Server 的鉴权中间件）：
	//
	//	User: func(ctx context.Context) (string, bool) {
	//		p, ok := gateway.PrincipalFrom(ctx)
	//		if !ok {
	//			return "", false
	//		}
	//		return p.UserID(), true
	//	}
	User func(ctx context.Context) (userID string, ok bool)
}

// routeTable 是单一路由真源：每条路由附上它是否需要用户态鉴权。
//
// 需要用户态的两条（models / usage）必须由装配方用 gateway.Authenticator.
// RequireUserRoutes 包裹，并把 Deps.User 接到 gateway.PrincipalFrom(...).UserID()；
// 其余四条一旦被套上 RequireUser 就会 401，插件将永远无法登录。
type routeEntry struct {
	route     httpx.Route
	needsUser bool
}

func routeTable(d Deps) []routeEntry {
	h := &handlers{d: d, log: loggerOf(d.Logger)}
	return []routeEntry{
		{route: httpx.Route{Pattern: "GET /v1/health", Handler: h.health}},
		{route: httpx.Route{Pattern: "GET /v1/capabilities", Handler: h.capabilities}},
		{route: httpx.Route{Pattern: "GET /v1/models", Handler: h.models}, needsUser: true},
		{route: httpx.Route{Pattern: "GET /v1/usage", Handler: h.usage}, needsUser: true},
		{route: httpx.Route{Pattern: "POST /v1/auth/device", Handler: h.authDevice}},
		{route: httpx.Route{Pattern: "POST /v1/auth/token", Handler: h.authToken}},
		{route: httpx.Route{Pattern: "POST /v1/auth/refresh", Handler: h.authRefresh}},
		{route: httpx.Route{Pattern: "POST /v1/auth/login", Handler: h.authLogin}},
	}
}

// Routes 返回本包的全部路由，由装配方挂到 http.ServeMux（Go 1.22+ 模式语法）。
func Routes(d Deps) []httpx.Route {
	entries := routeTable(d)
	out := make([]httpx.Route, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.route)
	}
	return out
}

// UserRoutes 只返回需要用户态鉴权的路由（/v1/models、/v1/usage），供装配方
// 单独套 gateway.Authenticator.RequireUserRoutes —— 契约 §11.2 要求 api 子包
// 自带路由，鉴权由 W-Server 的中间件统一完成。
func UserRoutes(d Deps) []httpx.Route {
	return pickRoutes(d, true)
}

// PublicRoutes 返回无需鉴权的路由（health / capabilities / 登录链四条）。
func PublicRoutes(d Deps) []httpx.Route {
	return pickRoutes(d, false)
}

func pickRoutes(d Deps, needsUser bool) []httpx.Route {
	entries := routeTable(d)
	out := make([]httpx.Route, 0, len(entries))
	for _, e := range entries {
		if e.needsUser == needsUser {
			out = append(out, e.route)
		}
	}
	return out
}

// handlers 持有已解析的依赖，避免每个请求重复判空。
type handlers struct {
	d   Deps
	log Logger
}

func (h *handlers) version() string {
	if v := strings.TrimSpace(h.d.Version); v != "" {
		return v
	}
	return DefaultVersion
}

// requireUser 取出已鉴权用户 ID；缺失时写好 401 并返回 false。
//
// 用户态路由的 userID 一律来自这里，绝不从 query/body 读取：契约 §11.3 的
// /v1/usage 只能回当前用户名下的记录。
func (h *handlers) requireUser(w http.ResponseWriter, r *http.Request) (string, bool) {
	get := h.d.User
	if get == nil {
		get = UserID
	}
	id, ok := get(r.Context())
	id = strings.TrimSpace(id)
	if !ok || id == "" {
		httpx.WriteError(w, r, http.StatusUnauthorized, "invalid_api_key",
			"缺少用户凭据：请携带 Authorization: Bearer <API key 或 access token>")
		return "", false
	}
	return id, true
}

// unavailable 是依赖未装配时的统一响应（503）：装配错误要显式暴露，但不能
// 让 nil 接口调用 panic 掉整个进程。
func unavailable(w http.ResponseWriter, r *http.Request, what string) {
	httpx.WriteError(w, r, http.StatusServiceUnavailable, "service_unavailable", what+"未装配")
}

// ---------------------------------------------------------------- 用户身份上下文

type ctxKey int

const userIDKey ctxKey = iota

// WithUserID 把已鉴权用户 ID 放进请求 context。供 W-Server 的中间件（或
// 拒绝在 W-Server 与 api 子包之间引入依赖的装配层）使用；本包不解析任何凭据。
func WithUserID(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, userIDKey, userID)
}

// UserID 取出 WithUserID 写入的用户 ID；未写入时 ok=false。
func UserID(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(userIDKey).(string)
	return id, ok
}

// ---------------------------------------------------------------- 日志

// loggerOf 在未装配日志器时回退到 observability 默认日志器（自带脱敏与级别过滤）。
func loggerOf(l Logger) Logger {
	if l == nil {
		return obsLogger{}
	}
	return l
}

type obsLogger struct{}

func (obsLogger) Warn(ctx context.Context, msg string, fields ...map[string]any) {
	observability.LogWarn(ctx, msg, fields...)
}

func (obsLogger) Error(ctx context.Context, msg string, fields ...map[string]any) {
	observability.LogError(ctx, msg, fields...)
}

// logFields 组装日志字段，并统一带上 request_id 便于串联（不包含任何凭据）。
func logFields(r *http.Request, extra map[string]any) map[string]any {
	out := make(map[string]any, len(extra)+1)
	if id := httpx.RequestID(r); id != "" {
		out["request_id"] = id
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}
