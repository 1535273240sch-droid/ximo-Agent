package openai

import (
	"context"
	"net/http"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/catalog"
	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/model"
	"github.com/ximo888ok-netizen/ximo-agent/internal/observability"
	"github.com/ximo888ok-netizen/ximo-agent/internal/provider"
	"github.com/ximo888ok-netizen/ximo-agent/internal/quota"
)

const (
	// DefaultMaxOutputTokens 请求未给 max_tokens 时用于预占估算的输出上界。
	// 与 upstream 侧 provider 缺省 MaxOutputTokens(8192) 对齐：预占宁可多占，
	// 也不要因为估少了让真实消耗超额的请求绕过预占闸门。
	DefaultMaxOutputTokens int64 = 8192

	// DefaultPriceMicroPerKTok V1 占位单价（微单位 / 1K token）。
	// 没有真实价目表（§11.6），金额只用于跑通额度链路，不要当账单。
	DefaultPriceMicroPerKTok int64 = quota.DefaultPriceMicroPerKTok

	// settlementTimeout 结算 / 归还 / 写终态 usage 的超时。这些写操作都要脱离客户端
	// 取消（客户端收到后立刻断开是常态），但也不能无界等待，故单独限时。
	settlementTimeout = 10 * time.Second
)

// usage 行的终态。契约 §11.5 的词表在本实现的落点：
//
//	ok               → usage.status = quota.UsageStatusSettled("settled")，由
//	                   quota.SettleWithUsage 写入（成功终态即「已结算」）。
//	upstream_error   → 本包 release 路径写入。
//	upstream_timeout → 本包 release 路径写入（上游超时/DNS 类终止）。
//	client_canceled  → 本包 release 路径写入（r.Context() 结束或 SSE 写不进去）。
//
// status 是 UsageRecord 上唯一的终态载体（没有备注/口径字段），因此「上游未报 usage 时
// 按估算金额结算」在不伪造 token 数的前提下无法表达：本包选择如实记 0（§6.1 不伪造
// 精确值），代价是该次请求不计费——详见 charge.go 与流式路径的注释。
const (
	OutcomeSettled         = quota.UsageStatusSettled
	OutcomeUpstreamError   = "upstream_error"
	OutcomeUpstreamTimeout = "upstream_timeout"
	OutcomeClientCanceled  = "client_canceled"
)

// Authenticator 解析本次请求的已认证用户。
//
// 认证本身由 W-Server 的中间件实现（`ximo_sk_` API Key / `gwa_` access token，§11.3），
// 本包只消费结果。装配方在 main 里把它接到中间件的用户解析上（通常是「取出中间件放进
// request context 的用户」的一行闭包）。为 nil 时每个请求都会回 500，让装配错误显式
// 暴露，而不是静默放行成匿名请求。
type Authenticator func(r *http.Request) (model.User, error)

// QuotaService 是额度服务的最小面（*quota.Service 结构性满足）。
type QuotaService interface {
	Reserve(ctx context.Context, userID, requestID string, estimateMicro int64) (model.Reservation, error)
	SettleWithUsage(ctx context.Context, r model.Reservation, u model.UsageRecord, priceMicroPerKTok int64) (model.UsageRecord, error)
	Release(ctx context.Context, r model.Reservation, reason string) error
}

// ModelSource 读模型目录（*gwstore.Store 结构性满足）：模型不存在/未启用是 404
// model_not_found 的唯一依据（§11.4：必须能区分「模型不存在」与「上游不可用」）。
type ModelSource interface {
	GetModel(ctx context.Context, modelID string) (model.ModelSpec, error)
}

// CandidateSource 候选路由（*catalog.Catalog 结构性满足）。返回的候选已按 priority
// 升序（§11.1.5）、已过滤停用与熔断打开的 provider；协议过滤由本包做（§11.1.4）。
type CandidateSource interface {
	Candidates(ctx context.Context, modelID string) ([]catalog.Candidate, error)
}

// ClientPool 上游客户端池（*upstream.Pool 结构性满足）。
//
// 熔断/限流记账由 provider.Client 内部完成（upstream.Pool 的 Mark* 是给「不走 Client
// 直接发请求」的调用方用的），所以这里只要 Client 一个方法就够。
type ClientPool interface {
	Client(ctx context.Context, providerID string) (*provider.Client, error)
}

// UsageWriter 落**未结算**的终态 usage 行（*gwstore.Store 结构性满足）。
//
// 为什么需要它：quota 只有「结算」入口（SettleWithUsage 会把 status 写成 quota 的
// "settled" 并按 token 计费），没有「不计费但记终态」的入口；而 §11.5 要求流式失败也
// 必须留下带终态的 usage 行。失败路径上预占已归还、账本已平衡，这一行纯粹是审计与
// 可观测性数据（cost_micro=0），直接落库不会与账务冲突。
type UsageWriter interface {
	InsertUsage(ctx context.Context, u model.UsageRecord) error
}

// Deps 是本包的窄依赖集合：每个字段都能由既有服务包直接满足，装配方无需适配代码。
type Deps struct {
	// Auth 必需：解析已认证用户。
	Auth Authenticator
	// Quota 必需：额度预占 / 结算 / 归还。
	Quota QuotaService
	// Models 必需：模型启用校验。
	Models ModelSource
	// Catalog 必需：候选路由。
	Catalog CandidateSource
	// Upstream 必需：逐候选取上游客户端。
	Upstream ClientPool
	// Usage 必需：失败终态 usage 行。
	Usage UsageWriter
	// PriceMicroPerKTok 结算单价（微单位 / 1K token）；<=0 用 DefaultPriceMicroPerKTok。
	PriceMicroPerKTok int64
	// MaxOutputTokensEstimate 请求未给 max_tokens 时的预占上界；<=0 用 DefaultMaxOutputTokens。
	MaxOutputTokensEstimate int64
	// Logger 结构化日志；nil 用 observability.DefaultLogger()。
	Logger *observability.Logger
	// Now 时钟（响应 created 与延迟统计）；nil 用 time.Now。测试可注入。
	Now func() time.Time
}

func (d Deps) price() int64 {
	if d.PriceMicroPerKTok > 0 {
		return d.PriceMicroPerKTok
	}
	return DefaultPriceMicroPerKTok
}

func (d Deps) maxOutputEstimate() int64 {
	if d.MaxOutputTokensEstimate > 0 {
		return d.MaxOutputTokensEstimate
	}
	return DefaultMaxOutputTokens
}

func (d Deps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// logger 返回日志器；缺省用全局日志（写日志时 observability 统一脱敏）。
func (d Deps) logger() *observability.Logger {
	if d.Logger != nil {
		return d.Logger
	}
	return observability.DefaultLogger()
}

// estimateMicro 按 §6 公式估算本次预占金额：
// (输入 token 粗估 + 输出 token 上界) * 单价 / 1000。
func (d Deps) estimateMicro(norm *normalizedRequest) int64 {
	maxOut := int64(norm.MaxTokens)
	if maxOut <= 0 {
		maxOut = d.maxOutputEstimate()
	}
	return quota.EstimateMicro(norm.InputTokensEstimate, maxOut, d.price())
}

// handler 是路由的接收者，只持有 Deps（无其它可变状态，可并发使用）。
type handler struct {
	d Deps
}

func (h *handler) log() *observability.Logger { return h.d.logger() }

// cleanupCtx 返回用于结算/归还的 context：脱离客户端取消，但限时。
//
// 必须脱离取消：客户端断开（或网关超时）之后额度账务仍要落库；用已取消的 context
// 会让归还/结算静默失败，把额度永久占住。
func (h *handler) cleanupCtx(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), settlementTimeout)
}
