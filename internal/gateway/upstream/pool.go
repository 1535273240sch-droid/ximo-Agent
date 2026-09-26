// Package upstream 管理网关到各上游 provider 的连接池。
//
// 既有 internal/provider 只装配「一个活跃服务商」，而中转站要同时对接多个上游，
// 且必须做到故障隔离：A 家挂了不能把 B 家的请求一起挡掉。因此本包为**每个
// providerID** 各持有一份 client + CircuitBreaker + RateLimiter，惰性构建、
// 按 providerID 缓存，构建失败返回错误而不是 panic。
//
// 状态归属与并发约定：
//   - 熔断/限流状态以 providerID 为粒度，跨客户端重建复用（同一 provider 换
//     endpoint 只是配置修复，不该把健康度清零）；显式 Invalidate 才整体丢弃。
//   - provider 目录每次调用都重新读一次（主键查询），因此配置改了不需要额外通知
//     就能生效；客户端按配置指纹判断要不要重建。
//   - 明文密钥只在单次请求构造 Authorization 头时解析（provider.Client 的行为），
//     本包不缓存明文；日志只记 providerID 与 scheme://host，不记完整 URL。
package upstream

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/model"
	"github.com/ximo888ok-netizen/ximo-agent/internal/observability"
	"github.com/ximo888ok-netizen/ximo-agent/internal/provider"
)

// 构建/运行期错误。一律用 errors.Is 判定。
var (
	// ErrNoStore 未注入 provider 目录来源。
	ErrNoStore = errors.New("upstream: 未注入 provider 目录")
	// ErrNoSecretResolver 未注入密钥解析器。
	ErrNoSecretResolver = errors.New("upstream: 未注入密钥解析器")
	// ErrMissingEndpoint provider 未配置 Endpoint。
	ErrMissingEndpoint = errors.New("upstream: provider 未配置 Endpoint")
	// ErrMissingAPIKeyRef provider 未配置 APIKeyRef（密钥引用）。
	ErrMissingAPIKeyRef = errors.New("upstream: provider 未配置 APIKeyRef")
	// ErrUnsupportedProtocol provider 协议不是 OpenAI 兼容。
	ErrUnsupportedProtocol = errors.New("upstream: 不支持的 provider 协议")
)

// SecretResolver 把 model.ProviderSpec.APIKeyRef 解析为明文密钥。
//
// 实现方应基于 internal/secrets（Windows 凭据管理器 / DPAPI 等平台安全存储）；
// 本接口按契约 §8 定义，方法名是 Resolve。
type SecretResolver interface {
	Resolve(ctx context.Context, ref string) (string, error)
}

// SecretResolverFunc 让普通函数满足 SecretResolver。
type SecretResolverFunc func(ctx context.Context, ref string) (string, error)

// Resolve 实现 SecretResolver。
func (f SecretResolverFunc) Resolve(ctx context.Context, ref string) (string, error) {
	return f(ctx, ref)
}

// providerSource 是本包需要的最小目录读取面。
//
// 按 Go 惯例由消费方定义接口：*gwstore.Store 只要方法签名逐字一致即可直接传入，
// 本包不必依赖 store 包，从而可独立编译与单测。
type providerSource interface {
	GetProvider(ctx context.Context, id string) (model.ProviderSpec, error)
}

// Options 装配参数，零值即默认。
type Options struct {
	// HTTPClient 全部 provider 共享的 HTTP 客户端（nil 时由 provider 自建）。
	HTTPClient provider.HTTPDoer
	// Retry 重试策略（零值用 provider.DefaultRetryPolicy）。
	Retry provider.RetryPolicy
	// DefaultTimeout 单次请求超时；ProviderSpec.TimeoutMS 优先。
	DefaultTimeout time.Duration
	// Logger 结构化日志（缺省用 observability 全局日志，写日志时统一过脱敏）。
	Logger *observability.Logger
	// Capabilities 上游能力开关；零值用 provider.DefaultCapabilities。
	// 逐 provider 可用 ProviderSpec.ConfigJSON 覆盖，见 spec.go。
	Capabilities provider.Capabilities
	// BreakerFactory 逐 provider 构造熔断器；nil 时用默认（5 次失败 / 30s 冷却）。
	// 便于按 provider 定制阈值，也便于测试注入短冷却时间。
	BreakerFactory func(providerID string) *provider.CircuitBreaker
	// LimiterFactory 逐 provider 构造限流器；nil 表示不限流。
	LimiterFactory func(providerID string) *provider.RateLimiter
}

// Pool 是各 provider 的客户端与健康状态容器，可安全并发使用。
type Pool struct {
	store   providerSource
	secrets SecretResolver
	opts    Options

	mu      sync.RWMutex
	entries map[string]*entry
}

// entry 是一个 providerID 的运行期状态。
type entry struct {
	breaker *provider.CircuitBreaker
	limiter *provider.RateLimiter

	// mu 串行化客户端构建；与 Pool.mu 的加锁顺序始终是 Pool.mu → entry.mu，
	// 且 entry.mu 内不做任何阻塞 I/O（构建客户端不读库、不解析密钥）。
	mu          sync.Mutex
	client      *provider.Client
	fingerprint string
}

// New 构造连接池。st 通常传 *gwstore.Store。
func New(st providerSource, secrets SecretResolver) *Pool {
	return NewWithOptions(st, secrets, Options{})
}

// NewWithOptions 构造连接池并指定装配参数。
func NewWithOptions(st providerSource, secrets SecretResolver, opts Options) *Pool {
	return &Pool{
		store:   st,
		secrets: secrets,
		opts:    opts,
		entries: make(map[string]*entry),
	}
}

// Client 返回某 provider 的客户端，必要时惰性构建。
//
// 每次都读一次目录（主键查询）并比对配置指纹：目录改了不必显式通知，
// 下一次调用就会重建客户端。
func (p *Pool) Client(ctx context.Context, providerID string) (*provider.Client, error) {
	if p.store == nil {
		return nil, ErrNoStore
	}
	spec, err := p.store.GetProvider(ctx, providerID)
	if err != nil {
		return nil, fmt.Errorf("upstream: 读取 provider %s 失败: %w", providerID, err)
	}
	return p.ClientFor(ctx, spec)
}

// ClientFor 用调用方已经拿到的目录条目构建客户端。
//
// 网关按候选列表（catalog.Candidates）派发时走这条路径，省掉一次重复读库。
// 配置指纹不变则复用既有客户端；变了则重建。
func (p *Pool) ClientFor(ctx context.Context, spec model.ProviderSpec) (*provider.Client, error) {
	id := strings.TrimSpace(spec.ID)
	if id == "" {
		// 先挡住空 ID：否则会在健康状态表里留下一个键为 "" 的垃圾条目。
		return nil, fmt.Errorf("upstream: provider ID 为空")
	}
	spec.ID = id
	e := p.entryFor(id)

	fp := fingerprint(spec)
	e.mu.Lock()
	var built bool
	defer func() {
		// 先解锁再写日志：entry.mu 内不做阻塞 I/O（见 entry 的注释）。
		e.mu.Unlock()
		if built {
			p.log().Info(ctx, "upstream 客户端已构建", map[string]any{
				"provider_id": id,
				"endpoint":    endpointHost(spec.Endpoint),
			})
		}
	}()
	if e.client != nil && e.fingerprint == fp {
		return e.client, nil
	}

	cfg, timeout, err := clientConfig(spec, p.opts)
	if err != nil {
		return nil, err
	}
	client, err := provider.NewClient(provider.ClientOptions{
		Config:         cfg,
		Secrets:        p.secretResolver(),
		HTTPClient:     p.opts.HTTPClient,
		Retry:          p.opts.Retry,
		Limiter:        e.limiter,
		Breaker:        e.breaker,
		DefaultTimeout: timeout,
		Logger:         p.opts.Logger,
	})
	if err != nil {
		return nil, fmt.Errorf("upstream: 构建 provider %s 客户端失败: %w", id, err)
	}

	e.client = client
	e.fingerprint = fp
	built = true
	return client, nil
}

// Healthy 实现 catalog.Probe：熔断器未处于 open 即可用。
//
// half-open 视为可用 —— 那正是放行探测请求的状态，若在此过滤掉，provider 永远
// 恢复不了。
func (p *Pool) Healthy(providerID string) bool {
	return p.entryFor(providerID).breaker.State() != provider.BreakerOpen
}

// MarkFailure 记录一次上游失败（调用方自行发起请求、未走 Client 记账时使用）。
//
// 与 provider 内部的记账规则一致：只有可重试类别（DNS / 连接超时 / 429 / 5xx）
// 才计入熔断，401/400 这类确定性错误不计 —— 否则配错一次 key 就让该 provider
// 一直假死，改配置也恢复不了。
func (p *Pool) MarkFailure(providerID string, err error) {
	p.entryFor(providerID).breaker.RecordFailure(provider.Classify(err).Class)
}

// MarkSuccess 记录一次上游成功（half-open 探测成功即靠它闭合熔断器）。
func (p *Pool) MarkSuccess(providerID string) {
	p.entryFor(providerID).breaker.RecordSuccess()
}

// BreakerState 返回熔断状态与连续失败数（管理后台健康页用）。
func (p *Pool) BreakerState(providerID string) (provider.BreakerState, int) {
	return p.entryFor(providerID).breaker.Snapshot()
}

// Invalidate 丢弃某 provider 的客户端缓存与熔断状态（配置/密钥热更新后调用）。
//
// 构建失败不会被缓存，所以这里不需要「清错误」；已在飞行中的请求继续用它持有的
// 旧 entry，不受影响。
func (p *Pool) Invalidate(providerID string) {
	p.mu.Lock()
	delete(p.entries, providerID)
	p.mu.Unlock()
}

// InvalidateAll 丢弃全部缓存（全局配置重载）。
func (p *Pool) InvalidateAll() {
	p.mu.Lock()
	p.entries = make(map[string]*entry)
	p.mu.Unlock()
}

// --- 内部 ---

// entryFor 返回 providerID 的状态容器，不存在则惰性创建。
//
// 只创建熔断器与限流器（很轻，且不碰存储/密钥）：这样 MarkFailure 在客户端还没
// 构建起来时也能记账。键来自 provider 目录，数量有界。
func (p *Pool) entryFor(providerID string) *entry {
	p.mu.RLock()
	e := p.entries[providerID]
	p.mu.RUnlock()
	if e != nil {
		return e
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if e = p.entries[providerID]; e != nil {
		return e
	}
	e = &entry{
		breaker: p.newBreaker(providerID),
		limiter: p.newLimiter(providerID),
	}
	p.entries[providerID] = e
	return e
}

func (p *Pool) newBreaker(providerID string) *provider.CircuitBreaker {
	if p.opts.BreakerFactory != nil {
		if b := p.opts.BreakerFactory(providerID); b != nil {
			return b
		}
	}
	return provider.NewCircuitBreaker(defaultFailureThreshold, defaultOpenTimeout)
}

func (p *Pool) newLimiter(providerID string) *provider.RateLimiter {
	if p.opts.LimiterFactory == nil {
		return nil
	}
	return p.opts.LimiterFactory(providerID)
}

// secretResolver 返回 provider 层需要的解析器（provider 的接口是 Get，本包是 Resolve）。
func (p *Pool) secretResolver() provider.SecretResolver {
	if p.secrets == nil {
		// 没注入解析器时给一个明确报错的实现：让构建成功、请求带着可读的错误失败，
		// 而不是 panic，也不是带着空 Authorization 头去撞一个分不清原因的 401。
		return unresolvedSecretResolver{}
	}
	return secretResolverAdapter{inner: p.secrets}
}

func (p *Pool) log() *observability.Logger {
	if p.opts.Logger != nil {
		return p.opts.Logger
	}
	return observability.DefaultLogger()
}

// secretResolverAdapter 把契约定义的 SecretResolver 适配成 provider 的取密钥接口。
type secretResolverAdapter struct {
	inner SecretResolver
}

func (a secretResolverAdapter) Get(ctx context.Context, ref string) (string, error) {
	return a.inner.Resolve(ctx, ref)
}

// unresolvedSecretResolver 在没有密钥解析器时使用。
type unresolvedSecretResolver struct{}

func (unresolvedSecretResolver) Get(context.Context, string) (string, error) {
	return "", ErrNoSecretResolver
}
