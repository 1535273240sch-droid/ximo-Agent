package main

import (
	"context"
	"net/http"

	"github.com/ximo888ok-netizen/ximo-agent/internal/account"
	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway"
	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/api/admin"
	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/api/anthropic"
	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/api/meta"
	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/api/openai"
	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/catalog"
	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/console"
	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/httpx"
	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/model"
	gwstore "github.com/ximo888ok-netizen/ximo-agent/internal/gateway/store"
	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/upstream"
	"github.com/ximo888ok-netizen/ximo-agent/internal/observability"
	"github.com/ximo888ok-netizen/ximo-agent/internal/quota"
	"github.com/ximo888ok-netizen/ximo-agent/internal/storage/sqlite"
)

// buildStack 组装网关的全部运行期依赖，返回生效配置、已挂载中间件的路由集合，
// 以及装配出的额度服务（调用方要用它起预占回收协程，见 reaper.go）。
//
// 依赖形状刻意全部用各包的"窄接口"（契约 §10.2）：gwStore / accountSvc / quotaSvc /
// cat / pool 都是**直接传入**，本包不为它们写适配器，只有 internal/secrets.Manager
// 需要一层 Resolve 适配（见 secrets.go）。
func buildStack(ctx context.Context, opt options, db *sqlite.DB, logger *observability.Logger) (gateway.Config, []httpx.Route, *quota.Service, error) {
	cfg := gateway.Config{
		Addr:              opt.addr,
		DBPath:            opt.dbPath,
		AdminToken:        opt.adminToken,
		PriceMicroPerKTok: opt.priceMicroPerKTok,
		MaxOutputTokens:   opt.maxOutputTokens,
		RequestTimeout:    opt.requestTimeout,
		RateLimitPerMin:   opt.rateLimitPerMin,
		// --max-body-bytes 不在 §12.1 的 flag 列表里，直接用 HTTP 骨架的默认上限
		// （32MiB，与 httpx.DecodeJSON 的解码上限一致），不额外发明一个对外参数。
		MaxBodyBytes: gateway.DefaultMaxBodyBytes,
	}
	if err := cfg.Validate(); err != nil {
		return gateway.Config{}, nil, nil, err
	}

	gwStore := gwstore.New(db)
	accountSvc := account.New(gwStore, gatewayPepper())
	quotaSvc := quota.New(gwStore)

	// 密钥：先建后端，再把它适配成 upstream 需要的 SecretResolver。
	secretsMgr := secretsForGateway(ctx, logger, opt.dbPath)
	pool := upstream.NewWithOptions(gwStore, secretResolver{store: secretsMgr}, upstream.Options{Logger: logger})
	// catalog 的 Probe 传池子本身（Healthy 由上游熔断器状态决定），因此候选列表里
	// 不会出现正在熔断的 provider。
	cat := catalog.New(gwStore, pool)

	authn := gateway.NewAuthenticator(accountSvc, cfg.AdminToken, logger)
	if !authn.AdminTokenConfigured() {
		// cfg.Validate 已经保证令牌非空，这里再挡一次：管理面"未配置令牌"必须 fail
		// closed，绝不能因为别处改动而静默变成"无令牌即可管理"。
		return gateway.Config{}, nil, nil, gateway.ErrAdminTokenRequired
	}
	limiter := gateway.NewKeyLimiter(cfg.RateLimitPerMin)

	return cfg, mountRoutes(cfg, gwStore, accountSvc, quotaSvc, cat, pool, secretsMgr, authn, limiter, logger), quotaSvc, nil
}

// mountRoutes 把各 api 子包的路由按"无鉴权 / 用户态 / 管理态"三段挂载。
//
// 中间件顺序遵守 §11.4（认证 → 限流 → handler）：先套限流再套认证，最终执行顺序
// 就是 认证(RequireUser/RequireAdmin) → 限流(KeyLimiter) → handler。
// 反过来（把限流套在最外层）在未认证时取不到 Principal，分桶键会退化成客户端 IP，
// 同一 NAT 后的多个用户会互相顶掉配额。
func mountRoutes(
	cfg gateway.Config,
	gwStore *gwstore.Store,
	accountSvc *account.Service,
	quotaSvc *quota.Service,
	cat *catalog.Catalog,
	pool *upstream.Pool,
	secretsMgr secretStore,
	authn *gateway.Authenticator,
	limiter *gateway.KeyLimiter,
	logger *observability.Logger,
) []httpx.Route {
	// meta 的 Deps.User 从网关中间件注入的 Principal 取用户 ID（本包是唯一同时
	// 认识 W-Server 与 api 子包的地方，这层"接线"只能在这里做）。
	userIDOf := func(ctx context.Context) (string, bool) {
		p, ok := gateway.PrincipalFrom(ctx)
		if !ok || p.User.ID == "" {
			return "", false
		}
		return p.User.ID, true
	}
	// openai / anthropic 的认证入参形状是 func(*http.Request) (model.User, error)：
	// 同样只消费中间件的鉴权结果，不重复解析 Authorization 头。
	userOf := func(r *http.Request) (model.User, error) {
		p, ok := gateway.PrincipalFrom(r.Context())
		if !ok || p.User.ID == "" {
			return model.User{}, model.ErrBadCredentials
		}
		return p.User, nil
	}

	metaDeps := meta.Deps{
		Store:    gwStore,
		Accounts: accountSvc,
		Catalog:  cat,
		Logger:   logger,
		Version:  version,
		User:     userIDOf,
	}
	openaiDeps := openai.Deps{
		Auth:                    userOf,
		Quota:                   quotaSvc,
		Models:                  gwStore,
		Catalog:                 cat,
		Upstream:                pool,
		Usage:                   gwStore,
		PriceMicroPerKTok:       cfg.PriceMicroPerKTok,
		MaxOutputTokensEstimate: cfg.MaxOutputTokens,
		Logger:                  logger,
	}
	anthropicDeps := anthropic.Deps{
		Models:   gwStore,
		Catalog:  cat,
		Upstream: pool,
		Quota:    quotaSvc,
		// AuthFunc 优先于 Auth：路由已被 RequireUser 包住，这里直接消费中间件的
		// 鉴权结果；Auth 作为兜底保留，使本路由即使脱离中间件挂载也能认证。
		AuthFunc:          userOf,
		Auth:              accountSvc,
		Usage:             gwStore,
		PriceMicroPerKTok: cfg.PriceMicroPerKTok,
		Logger:            logger,
	}
	adminDeps := admin.Deps{
		Store:    gwStore,
		Accounts: accountSvc,
		Quota:    quotaSvc,
		// Secrets 为 nil 时"带明文密钥的 provider 写入"会明确失败（不会明文落库）。
		Secrets:    adminSecretsWriter(secretsMgr),
		AdminToken: cfg.AdminToken,
	}

	routes := make([]httpx.Route, 0, 32)

	// --- 无鉴权：health / capabilities / 全部 /v1/auth/* -------------------
	// /v1/auth/* 绝不能挂用户态鉴权：插件正是为了拿凭据才来调它们。只套限流
	// （此时无 Principal，按客户端 IP 分桶），登录链因此不会被无限爆破。
	routes = append(routes, limiter.LimitRoutes(meta.PublicRoutes(metaDeps))...)
	// 管理控制台：静态页面本身不需要令牌（令牌由使用者在页面上输入，数据请求全部走
	// 下面的 /admin/*，逐个请求校验）。它同时把 GET / 重定向到 /console/，避免直接
	// 访问根路径得到 404。
	routes = append(routes, console.Routes()...)

	// --- 用户态：Bearer <API key 或 access token> -------------------------
	userRoutes := make([]httpx.Route, 0, 8)
	userRoutes = append(userRoutes, meta.UserRoutes(metaDeps)...)
	userRoutes = append(userRoutes, openai.Routes(openaiDeps)...)
	userRoutes = append(userRoutes, anthropic.Routes(anthropicDeps)...)
	routes = append(routes, authn.RequireUserRoutes(limiter.LimitRoutes(userRoutes))...)

	// --- 管理态：X-Admin-Token（恒定时间比较，缺失/不符 401）---------------
	routes = append(routes, authn.RequireAdminRoutes(limiter.LimitRoutes(admin.Routes(adminDeps)))...)

	return routes
}

// adminSecretsWriter 返回管理面用的密钥写入面（admin.Secrets 只暴露 Put）。
//
// mgr 为 nil 时必须返回**真正的 nil 接口**：把带 nil 底层的接口值装进去会得到一个
// 非 nil 的接口，admin 里 "h.d.Secrets == nil" 的检查会失效，随后的 Put 会 panic。
// 这里显式挡掉这种情况。
func adminSecretsWriter(mgr secretStore) admin.Secrets {
	if mgr == nil {
		return nil
	}
	return mgr
}
