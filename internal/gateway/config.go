// Package gateway 是 XIMO 中转站的 HTTP 服务器骨架：配置解析、路由装配、中间件链，
// 以及鉴权（auth.go）与限流（ratelimit.go）两个可复用原语。
//
// 本包**不 import 任何 api 子包**（契约 §11.2）：各 api 子包各自导出
// `Routes(Deps) []httpx.Route`，由 cmd/ximo-gateway 在 main 里把路由、鉴权与限流
// 装配到一起后交给 New。典型装配：
//
//	auth := gateway.NewAuthenticator(accountSvc, cfg.AdminToken, logger)
//	lim := gateway.NewKeyLimiter(cfg.RateLimitPerMin)
//	routes := append(meta.Routes(deps), auth.RequireUserRoutes(openai.Routes(deps))...)
//	routes = append(routes, auth.RequireAdminRoutes(admin.Routes(deps))...)
//	srv, err := gateway.New(cfg, lim.LimitRoutes(routes), logger)
//
// 生命周期顺序（契约 §11.4）由路由包装保证：request_id 由本包的全局链注入 →
// 认证（RequireUser/RequireAdmin）→ 限流（KeyLimiter）→ handler。
package gateway

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"
)

// 配置默认值。除管理令牌外都有合理默认，且都写进 flag 帮助，避免「默认值只存在于代码里」。
const (
	// DefaultAddr 默认只监听回环地址：中转站默认不对公网暴露，需要对外时必须显式指定。
	DefaultAddr = "127.0.0.1:8080"
	// DefaultDBPath 默认在工作目录下建库（相对路径由使用方保证工作目录正确）。
	DefaultDBPath = "ximo-gateway.db"
	// DefaultPriceMicroPerKTok 是 V1 的占位价：1 微单位/1K token。
	// 真实价目表本轮不做（契约 §11.6），0 表示不计价。
	DefaultPriceMicroPerKTok int64 = 1
	// DefaultMaxOutputTokens 是预占额度时假定的最大输出量（契约 §6）。
	DefaultMaxOutputTokens int64 = 4096
	// DefaultRequestTimeout 是单个请求的总时限。流式生成可能很长，故给足 5 分钟；
	// 设为 0 表示不限时（此时只受上游自身超时约束）。
	DefaultRequestTimeout = 5 * time.Minute
	// DefaultRateLimitPerMin 是每凭据（API Key / 用户）每分钟请求数上限，0 表示不限流。
	DefaultRateLimitPerMin = 60
	// DefaultMaxBodyBytes 是请求体上限，与 httpx.DecodeJSON 的 32MiB 解码上限一致。
	DefaultMaxBodyBytes int64 = 32 << 20
)

// envAdminToken 是 --admin-token 的环境变量回退项。
const envAdminToken = "XIMO_GATEWAY_ADMIN_TOKEN"

var (
	// ErrAdminTokenRequired 表示既没有 --admin-token 也没有环境变量。
	// 管理令牌没有默认值：任何硬编码的默认令牌都等于把 /admin/* 公开。
	ErrAdminTokenRequired = errors.New("gateway: 缺少管理令牌，请用 --admin-token 或环境变量 " + envAdminToken + " 提供")
	// ErrInvalidConfig 是配置项取值的兜底错误（具体项在消息里说明）。
	ErrInvalidConfig = errors.New("gateway: 配置不合法")
)

// Config 是网关运行参数。
type Config struct {
	// Addr 是 HTTP 监听地址，形如 "127.0.0.1:8080"。
	Addr string
	// DBPath 是 SQLite 数据库文件路径（存储层由 main 打开，本包只持有路径）。
	DBPath string
	// AdminToken 是管理接口的共享令牌（X-Admin-Token）。明文只存在于启动参数与
	// Authenticator 的摘要里；日志输出一律省略（见 LogFields）。
	AdminToken string
	// PriceMicroPerKTok 是占位价（微单位/1K token），用于预占估算与结算。
	PriceMicroPerKTok int64
	// MaxOutputTokens 是请求未指定 max_tokens 时用于预占估算的上限。
	MaxOutputTokens int64
	// RequestTimeout 是单请求总时限，<=0 表示不限时。
	RequestTimeout time.Duration
	// RateLimitPerMin 是每凭据每分钟请求数上限，<=0 表示不限流。
	RateLimitPerMin int
	// MaxBodyBytes 是请求体字节上限，<=0 表示不限制（不建议）。
	MaxBodyBytes int64
}

// DefaultConfig 返回带默认值的配置（管理令牌为空，必须由使用方补齐）。
func DefaultConfig() Config {
	return Config{
		Addr:              DefaultAddr,
		DBPath:            DefaultDBPath,
		PriceMicroPerKTok: DefaultPriceMicroPerKTok,
		MaxOutputTokens:   DefaultMaxOutputTokens,
		RequestTimeout:    DefaultRequestTimeout,
		RateLimitPerMin:   DefaultRateLimitPerMin,
		MaxBodyBytes:      DefaultMaxBodyBytes,
	}
}

// ParseConfig 解析命令行参数并叠加环境变量回退，最后做一次 Validate。
//
// 优先级：显式 flag > 环境变量（目前只有管理令牌有 env 回退）> 默认值。
// 返回的错误不含任何令牌内容。
func ParseConfig(args []string) (Config, error) {
	return parseConfig(args, os.Stderr)
}

func parseConfig(args []string, out io.Writer) (Config, error) {
	cfg := DefaultConfig()
	fs := flag.NewFlagSet("ximo-gateway", flag.ContinueOnError)
	if out != nil {
		fs.SetOutput(out)
	}
	fs.StringVar(&cfg.Addr, "addr", cfg.Addr, "HTTP 监听地址，例如 127.0.0.1:8080")
	fs.StringVar(&cfg.DBPath, "db", cfg.DBPath, "SQLite 数据库文件路径")
	fs.StringVar(&cfg.AdminToken, "admin-token", "", "管理接口令牌（X-Admin-Token）；也可用环境变量 "+envAdminToken+", 无默认值")
	fs.Int64Var(&cfg.PriceMicroPerKTok, "price-micro-per-ktok", cfg.PriceMicroPerKTok, "占位价：微单位/1K token（V1 无真实价目表）")
	fs.Int64Var(&cfg.MaxOutputTokens, "max-output-tokens", cfg.MaxOutputTokens, "预占额度时假定的最大输出 token 数")
	fs.DurationVar(&cfg.RequestTimeout, "request-timeout", cfg.RequestTimeout, "单请求总时限，0 表示不限时")
	fs.IntVar(&cfg.RateLimitPerMin, "rate-limit-per-min", cfg.RateLimitPerMin, "每凭据每分钟请求数上限，0 表示不限流")
	fs.Int64Var(&cfg.MaxBodyBytes, "max-body-bytes", cfg.MaxBodyBytes, "请求体字节上限")

	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	if cfg.AdminToken == "" {
		cfg.AdminToken = os.Getenv(envAdminToken)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate 校验配置。管理令牌缺失一律报错（不静默放过）：
// 少了它要么 /admin/* 全开，要么全 401，两种都不是「静默可用」。
func (c Config) Validate() error {
	if c.Addr == "" {
		return fmt.Errorf("%w: --addr 不能为空", ErrInvalidConfig)
	}
	if c.DBPath == "" {
		return fmt.Errorf("%w: --db 不能为空", ErrInvalidConfig)
	}
	if c.AdminToken == "" {
		return ErrAdminTokenRequired
	}
	if c.PriceMicroPerKTok < 0 {
		return fmt.Errorf("%w: --price-micro-per-ktok 不能为负（%d）", ErrInvalidConfig, c.PriceMicroPerKTok)
	}
	if c.MaxOutputTokens <= 0 {
		return fmt.Errorf("%w: --max-output-tokens 必须为正（%d）", ErrInvalidConfig, c.MaxOutputTokens)
	}
	if c.RequestTimeout < 0 {
		return fmt.Errorf("%w: --request-timeout 不能为负（%s）", ErrInvalidConfig, c.RequestTimeout)
	}
	if c.RateLimitPerMin < 0 {
		return fmt.Errorf("%w: --rate-limit-per-min 不能为负（%d）", ErrInvalidConfig, c.RateLimitPerMin)
	}
	return nil
}

// LogFields 返回可安全写进日志的配置字段。管理令牌只以布尔量出现 ——
// 这样「打印配置」这个动作本身不可能泄漏令牌。
func (c Config) LogFields() map[string]any {
	return map[string]any{
		"addr":                   c.Addr,
		"db":                     c.DBPath,
		"admin_token_configured": c.AdminToken != "",
		"price_micro_per_ktok":   c.PriceMicroPerKTok,
		"max_output_tokens":      c.MaxOutputTokens,
		"request_timeout":        c.RequestTimeout.String(),
		"rate_limit_per_min":     c.RateLimitPerMin,
		"max_body_bytes":         c.MaxBodyBytes,
	}
}
