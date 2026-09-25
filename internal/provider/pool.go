// pool.go —— 任务 05：多服务商模型池与失败转移的选路层。
//
// 职责边界（任务书）：
//   - 只做「这一轮该用哪个服务商/模型」的选路，不碰熔断器与限流器的内部实现
//     （breaker.go / limiter.go 原样复用），也不重复错误分类（errors.go 原样复用）。
//   - 每个候选 = 一个 *Client，各自持有自己的 ProviderConfig / 熔断器 / 限流器，
//     因此「每个子代理用不同模型」就是给不同候选不同的 Model。
//
// 选路策略（先简单做）：按优先级顺序轮询，跳过当前熔断打开的候选。
// 失败转移的上限 = 池中候选数（最多轮询一遍），由调用方执行，避免死循环。
package provider

import (
	"fmt"
	"sync"
)

// PoolCandidate 模型池中的一个候选服务商。
//
// Client 持有该候选的 HTTP 客户端与熔断/限流状态；Model 是子代理调用时使用
// 的模型名 —— 池的价值就在于不同候选可以配不同模型。
type PoolCandidate struct {
	// ID 候选标识（取自 ProviderConfig.ID）。失败转移时用它排除已试过的候选，
	// 因此必须非空：ID 为空时退化为 Name，再退化为序号。
	ID string
	// Model 该候选使用的模型名。
	Model string
	// Client 对应的客户端。
	Client *Client
}

// Target 一次选路的结果：用哪个服务商的哪个模型发请求。
//
// Provider 是接口而非 *Client，让上层（expert 包）可以用假实现单测失败转移。
type Target struct {
	// ID 候选标识，非空（见 PoolCandidate.ID）。
	ID string
	// Model 本次请求使用的模型名。
	Model string
	// Provider 发起请求的客户端。
	Provider Provider
}

// Pool 多服务商候选池。并发安全。
//
// 池本身不记录成功/失败 —— 那是每个 Client 自己的熔断器在记（doOnce 里
// RecordSuccess/RecordFailure）；池只读 State() 做选路。
type Pool struct {
	mu      sync.Mutex
	entries []PoolCandidate
	// next 是轮询游标：并发子代理从不同候选起步，负载自然摊开。
	next int
}

// NewPool 构造模型池。候选顺序即优先级顺序（下标越小优先级越高）。
//
// nil 客户端的候选会被丢弃，保证 Select 永远返回可用的 Target。
func NewPool(candidates []PoolCandidate) *Pool {
	entries := make([]PoolCandidate, 0, len(candidates))
	for i, c := range candidates {
		if c.Client == nil {
			continue
		}
		if c.ID == "" {
			c.ID = candidateID(c.Client, i)
		}
		entries = append(entries, c)
	}
	return &Pool{entries: entries}
}

// candidateID 为没有显式 ID 的候选生成稳定标识。
func candidateID(c *Client, index int) string {
	if name := c.Name(); name != "" {
		return name
	}
	return fmt.Sprintf("provider-%d", index)
}

// Len 返回池中候选数量。失败转移的重试上限参考它（最多轮询一遍）。
func (p *Pool) Len() int {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.entries)
}

// FailoverLimit 失败转移次数上限：最多把所有候选轮询一遍。
func (p *Pool) FailoverLimit() int { return p.Len() }

// Select 挑选一个候选。order 是期望的优先级顺序（候选 ID 列表，来自设置面板
// 的专家/分类分配）；为空时按池的配置顺序。exclude 中的候选 ID 不再返回
// （失败转移时排除已经试过的）。
//
// 策略：按优先级顺序轮询，优先返回熔断未打开的候选；若全部打开，退化为
// 第一个未排除的候选 —— 是否真正放行由该候选自己的熔断器裁决，这样池永远
// 给出「最不坏」的选择，而不是在恢复边界上直接罢工。
//
// 注意健康判定只用 State()：Allow() 会占用 half-open 的探测名额，用池做
// 「预检」会把这个名额泄漏掉，导致真正的探测请求被拒。
func (p *Pool) Select(order []string, exclude ...string) (Target, bool) {
	if p == nil {
		return Target{}, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	n := len(p.entries)
	if n == 0 {
		return Target{}, false
	}

	skip := make(map[string]bool, len(exclude))
	for _, id := range exclude {
		skip[id] = true
	}

	var fallback Target
	advance := func(i int) { p.next = (p.next + i + 1) % n }

	// scanDefault 按池的配置顺序（从轮询游标起步）找健康候选；全部熔断打开时
	// 返回第一个未排除的候选作为「最不坏」兜底。
	scanDefault := func() (Target, bool) {
		for i := 0; i < n; i++ {
			c := p.entries[(p.next+i)%n]
			if skip[c.ID] {
				continue
			}
			t := Target{ID: c.ID, Model: c.Model, Provider: c.Client}
			if p.healthy(c) {
				advance(i)
				return t, true
			}
			if fallback.Provider == nil {
				fallback = t
			}
		}
		if fallback.Provider != nil {
			// 全部熔断打开：仍然轮转游标，避免下一次还从同一个候选开始。
			advance(0)
			return fallback, true
		}
		return Target{}, false
	}

	if len(order) == 0 {
		return scanDefault()
	}

	byID := make(map[string]PoolCandidate, n)
	for _, c := range p.entries {
		if _, dup := byID[c.ID]; !dup {
			byID[c.ID] = c
		}
	}
	// order 里至少要有一个 ID 真实存在于池中，才按用户指定的优先级顺序来。
	// 否则（分配残留了已删除/改名的服务商 ID）若按「找不到可用候选」处理，
	// 池里剩下的健康候选会被整体浪费掉——退回池的默认顺序。
	pinned := false
	for _, id := range order {
		c, ok := byID[id]
		if !ok {
			continue
		}
		pinned = true
		if skip[c.ID] {
			continue
		}
		t := Target{ID: c.ID, Model: c.Model, Provider: c.Client}
		if p.healthy(c) {
			return t, true
		}
		if fallback.Provider == nil {
			fallback = t
		}
	}
	if pinned {
		if fallback.Provider != nil {
			advance(0)
			return fallback, true
		}
		return Target{}, false
	}
	return scanDefault()
}

// HealthyCount 返回当前熔断未打开的候选数量。
func (p *Pool) HealthyCount() int {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, c := range p.entries {
		if p.healthy(c) {
			n++
		}
	}
	return n
}

// healthy 只读判定候选是否可用。熔断器内部状态（open→half-open 的时间跃迁）
// 由 State() 自带的 maybeHalfOpen 逻辑推进，这里不写任何熔断状态。
func (p *Pool) healthy(c PoolCandidate) bool {
	if c.Client == nil {
		return false
	}
	if c.Client.breaker == nil {
		return true
	}
	return c.Client.breaker.State() != BreakerOpen
}

// Candidates 返回池内候选的快照（按优先级顺序），供诊断与设置页展示。
func (p *Pool) Candidates() []PoolCandidate {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]PoolCandidate, len(p.entries))
	copy(out, p.entries)
	return out
}
