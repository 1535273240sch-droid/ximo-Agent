package gateway

import (
	"math"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/httpx"
	"github.com/ximo888ok-netizen/ximo-agent/internal/provider"
)

const (
	// DefaultMaxLimitedKeys 是限流桶数量软上限（配合空闲回收，防止被随机 Key 撑爆内存）。
	DefaultMaxLimitedKeys = 8192
	// defaultBucketIdleTTL 是桶的空闲回收阈值。
	defaultBucketIdleTTL = 10 * time.Minute
)

// KeyLimiter 是按凭据维度（API Key / 用户 / 管理令牌，见 Principal.RateKey）的令牌桶限流。
//
// 令牌桶算法直接复用 internal/provider.RateLimiter（每键一个实例），本包只负责分桶、
// 懒创建与空闲回收 —— 不重写一份算法。用 TryAcquire（非阻塞）而不是 Wait：
// 已认证的用户请求被拖慢不如直接 429，让客户端自己退避。
type KeyLimiter struct {
	enabled    bool
	ratePerSec float64
	burst      int
	maxKeys    int
	idleTTL    time.Duration
	now        func() time.Time

	mu      sync.Mutex
	buckets map[string]*limitBucket
}

type limitBucket struct {
	limiter  *provider.RateLimiter
	lastSeen time.Time
}

// NewKeyLimiter 构造限流器：perMin 是每凭据每分钟请求数上限，<=0 表示不限流
// （此时 Allow 恒为 true，Middleware 是透传）。
//
// burst 取 perMin（分钟额度允许一次性用完），速率按 perMin/60 每秒补充。
func NewKeyLimiter(perMin int) *KeyLimiter {
	l := &KeyLimiter{
		enabled: perMin > 0,
		maxKeys: DefaultMaxLimitedKeys,
		idleTTL: defaultBucketIdleTTL,
		now:     time.Now,
		buckets: make(map[string]*limitBucket),
	}
	if perMin > 0 {
		l.ratePerSec = float64(perMin) / 60.0
		l.burst = perMin
	}
	return l
}

// Enabled 报告是否真的在限流。
func (l *KeyLimiter) Enabled() bool { return l != nil && l.enabled }

// Allow 对 key 尝试取一个令牌。key 为空时按调用方给的兜底键处理（见 Middleware）。
func (l *KeyLimiter) Allow(key string) bool {
	if !l.Enabled() || key == "" {
		return true
	}
	return l.bucket(key).TryAcquire()
}

// Middleware 是生命周期里的「限流」环节（契约 §11.4：认证之后、handler 之前）。
// 分桶键取 Principal.RateKey()；未认证请求退化为按客户端 IP 分桶。
// 超限返回 429 + Retry-After，不再进入 handler。
func (l *KeyLimiter) Middleware(next http.Handler) http.Handler {
	if !l.Enabled() {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := rateKeyFrom(r)
		if !l.Allow(key) {
			w.Header().Set("Retry-After", strconv.Itoa(l.retryAfterSeconds()))
			httpx.WriteError(w, r, http.StatusTooManyRequests, "rate_limit_exceeded",
				"too many requests; retry after "+strconv.Itoa(l.retryAfterSeconds())+"s")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// LimitRoutes 批量包装路由（main 装配时用）。
func (l *KeyLimiter) LimitRoutes(routes []httpx.Route) []httpx.Route {
	return wrapRoutes(routes, l.Middleware)
}

// rateKeyFrom 取分桶键：已认证用凭据/用户维度，未认证用 IP 维度。
func rateKeyFrom(r *http.Request) string {
	if p, ok := PrincipalFrom(r.Context()); ok {
		if k := p.RateKey(); k != "" {
			return k
		}
	}
	return "ip:" + httpx.ClientIP(r)
}

// retryAfterSeconds 是给客户端的退避建议：补满一个令牌所需秒数（至少 1）。
func (l *KeyLimiter) retryAfterSeconds() int {
	if l.ratePerSec <= 0 {
		return 1
	}
	return max(1, int(math.Ceil(1/l.ratePerSec)))
}

func (l *KeyLimiter) bucket(key string) *provider.RateLimiter {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if b, ok := l.buckets[key]; ok {
		b.lastSeen = now
		return b.limiter
	}
	if len(l.buckets) >= l.maxKeys {
		l.sweepLocked(now)
	}
	b := &limitBucket{limiter: provider.NewRateLimiter(l.ratePerSec, l.burst), lastSeen: now}
	l.buckets[key] = b
	return b.limiter
}

// sweepLocked 回收空闲桶，并在仍超限时按最久未使用（LRU）淘汰到 7/8 容量。
//
// 这是内存保护而非硬配额：桶被回收的凭据等于重新获得一整桶额度，因此不能用作
// 「防止同一用户绕过限流」的手段 —— 那种绕过在客户端换 Key 时本来就存在，
// 真正的防线是账号体系本身。
func (l *KeyLimiter) sweepLocked(now time.Time) {
	for k, b := range l.buckets {
		if now.Sub(b.lastSeen) >= l.idleTTL {
			delete(l.buckets, k)
		}
	}
	if len(l.buckets) < l.maxKeys {
		return
	}
	target := l.maxKeys * 7 / 8
	if target < 1 {
		target = 1
	}
	type entry struct {
		key  string
		seen time.Time
	}
	all := make([]entry, 0, len(l.buckets))
	for k, b := range l.buckets {
		all = append(all, entry{key: k, seen: b.lastSeen})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].seen.Before(all[j].seen) })
	for i := 0; i < len(all) && len(l.buckets) > target; i++ {
		delete(l.buckets, all[i].key)
	}
}

// Buckets 返回当前桶数量（诊断与测试用）。
func (l *KeyLimiter) Buckets() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}
