// Package ctxmgr 实现上下文压缩、Token 预算与内存预算（架构文档第 22、23、24 章）。
//
// 目录名是任务书规定的 internal/context/，但包名取 ctxmgr 而非 context ——
// 因为 Go 的 context 标准包被 Engine/Provider 全线使用，若本包也叫 context，
// 每个消费方都要 stdlib 起别名，代价高且易错。这是对任务书的 1 项登记偏离，
// 已在交付说明「需要主Agent裁决」中登记：目录路径不变，仅包标识符不同。
//
// lru.go —— 有界 LRU 缓存（第 23 章硬性要求）。
//
// v1 的 tokenizer.ts 用 `const bpeCache = new Map<string, string[]>()` 做 BPE 结果缓存，
// 没有任何上限 —— 长期运行（72h soak）下这是持续内存增长的典型来源。
//
// 本实现提供四个维度的界，全部可配：
//   - MaxEntries  条目数上限
//   - MaxBytes    字节上限（值大小累计估算）
//   - TTL         条目存活时长
//   - Eviction    淘汰回调（供 observability 记录，也便于测试断言）
//
// 淘汰策略：容量超限时按 LRU 顺序淘汰；每次访问惰性清理过期条目，
// 另有 PurgeExpired 供后台周期调用。
package ctxmgr

import (
	"container/list"
	"sync"
	"time"
)

// LRUOptions 有界缓存的容量参数。
type LRUOptions struct {
	// MaxEntries 条目数上限。<=0 表示不按条目数限制。
	MaxEntries int
	// MaxBytes 值字节总量上限。<=0 表示不按字节限制。
	MaxBytes int64
	// TTL 条目生存时长。<=0 表示不过期。
	TTL time.Duration
	// OnEvict 淘汰回调；参数为 key 与淘汰原因。
	OnEvict func(key string, reason EvictReason)

	// now 注入时钟便于测试。
	now func() time.Time
}

// EvictReason 淘汰原因，供指标区分「容量淘汰」与「过期淘汰」。
type EvictReason string

const (
	EvictSize    EvictReason = "size"
	EvictEntries EvictReason = "entries"
	EvictTTL     EvictReason = "ttl"
	EvictManual  EvictReason = "manual"
)

type lruEntry struct {
	key       string
	value     []string
	sizeBytes int64
	expiresAt time.Time
	elem      *list.Element
}

// LRU 是 `map[string][]string` 的有界替代品，并发安全。
//
// 之所以值类型固定为 []string：本缓存服务的对象是 BPE 分词结果，
// 固定类型可以避免 interface{} 装箱带来的额外分配 —— 这本身就是内存治理的一部分。
type LRU struct {
	mu      sync.Mutex
	entries map[string]*lruEntry
	order   *list.List // 前端 = 最近使用

	opts LRUOptions

	curBytes   int64
	curEntries int

	// 统计（供 soak 测试与 observability 断言）。
	hits, misses, evictions uint64
}

// NewLRU 构造有界缓存。
func NewLRU(opts LRUOptions) *LRU {
	if opts.now == nil {
		opts.now = time.Now
	}
	return &LRU{
		entries: make(map[string]*lruEntry),
		order:   list.New(),
		opts:    opts,
	}
}

// DefaultTokenizerLRUOptions 针对 BPE 缓存的一组保守默认值。
//
// 8 万条 / 32MB 足以覆盖常用代码与中文语料的 pre-token，
// 超出部分走 LRU 淘汰而不是无限增长。
func DefaultTokenizerLRUOptions() LRUOptions {
	return LRUOptions{
		MaxEntries: 80_000,
		MaxBytes:   32 << 20,
		TTL:        30 * time.Minute,
	}
}

// Get 取缓存。第二返回值为是否命中。
func (c *LRU) Get(key string) ([]string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	e, ok := c.entries[key]
	if !ok {
		c.misses++
		return nil, false
	}
	if c.expiredLocked(e) {
		c.removeLocked(e, EvictTTL)
		c.misses++
		return nil, false
	}

	c.order.MoveToFront(e.elem)
	c.hits++
	return e.value, true
}

// Put 写入缓存。已有 key 会更新值并重算容量。
func (c *LRU) Put(key string, value []string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	size := estimateStringsBytes(value)

	if e, ok := c.entries[key]; ok {
		c.curBytes += size - e.sizeBytes
		e.value = value
		e.sizeBytes = size
		e.expiresAt = c.expiryLocked()
		c.order.MoveToFront(e.elem)
		c.enforceLocked()
		return
	}

	e := &lruEntry{
		key:       key,
		value:     value,
		sizeBytes: size,
		expiresAt: c.expiryLocked(),
	}
	e.elem = c.order.PushFront(e)
	c.entries[key] = e
	c.curEntries++
	c.curBytes += size

	c.enforceLocked()
}

// Delete 显式删除。
func (c *LRU) Delete(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return false
	}
	c.removeLocked(e, EvictManual)
	return true
}

// Clear 清空缓存并返回释放的字节数。
//
// 这是第 24 章降级顺序第 ② 步「回收 tokenizer cache」的入口。
func (c *LRU) Clear() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()

	freed := c.curBytes
	for _, e := range c.entries {
		if c.opts.OnEvict != nil {
			c.opts.OnEvict(e.key, EvictManual)
		}
	}
	c.entries = make(map[string]*lruEntry)
	c.order.Init()
	c.curBytes = 0
	c.curEntries = 0
	return freed
}

// PurgeExpired 清理过期条目，返回清理数量。供后台周期任务调用。
func (c *LRU) PurgeExpired() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.opts.TTL <= 0 {
		return 0
	}
	now := c.opts.now()
	purged := 0
	for _, e := range c.entries {
		if !e.expiresAt.IsZero() && now.After(e.expiresAt) {
			c.removeLocked(e, EvictTTL)
			purged++
		}
	}
	return purged
}

// Resize 动态调整容量上限并立即执行淘汰（内存压力升高时收紧）。
func (c *LRU) Resize(maxEntries int, maxBytes int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.opts.MaxEntries = maxEntries
	c.opts.MaxBytes = maxBytes
	c.enforceLocked()
}

// Stats 返回缓存统计快照。
type LRUStats struct {
	Entries    int    `json:"entries"`
	Bytes      int64  `json:"bytes"`
	Hits       uint64 `json:"hits"`
	Misses     uint64 `json:"misses"`
	Evictions  uint64 `json:"evictions"`
	MaxEntries int    `json:"max_entries"`
	MaxBytes   int64  `json:"max_bytes"`
}

// Stats 返回统计快照。
func (c *LRU) Stats() LRUStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return LRUStats{
		Entries:    c.curEntries,
		Bytes:      c.curBytes,
		Hits:       c.hits,
		Misses:     c.misses,
		Evictions:  c.evictions,
		MaxEntries: c.opts.MaxEntries,
		MaxBytes:   c.opts.MaxBytes,
	}
}

// Len 返回当前条目数。
func (c *LRU) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.curEntries
}

// Bytes 返回当前字节占用估算。
func (c *LRU) Bytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.curBytes
}

// --- 内部 ---

func (c *LRU) expiryLocked() time.Time {
	if c.opts.TTL <= 0 {
		return time.Time{}
	}
	return c.opts.now().Add(c.opts.TTL)
}

func (c *LRU) expiredLocked(e *lruEntry) bool {
	if e.expiresAt.IsZero() {
		return false
	}
	return c.opts.now().After(e.expiresAt)
}

func (c *LRU) removeLocked(e *lruEntry, reason EvictReason) {
	delete(c.entries, e.key)
	c.order.Remove(e.elem)
	c.curEntries--
	c.curBytes -= e.sizeBytes
	if c.curBytes < 0 {
		c.curBytes = 0
	}
	if reason != EvictManual {
		c.evictions++
	}
	if c.opts.OnEvict != nil {
		c.opts.OnEvict(e.key, reason)
	}
}

// enforceLocked 从尾部（最久未使用）淘汰直到满足容量约束。
func (c *LRU) enforceLocked() {
	// 条目数上限。
	for c.opts.MaxEntries > 0 && c.curEntries > c.opts.MaxEntries {
		if !c.evictOldestLocked(EvictEntries) {
			break
		}
	}
	// 字节上限。
	for c.opts.MaxBytes > 0 && c.curBytes > c.opts.MaxBytes {
		if !c.evictOldestLocked(EvictSize) {
			break
		}
	}
}

func (c *LRU) evictOldestLocked(reason EvictReason) bool {
	back := c.order.Back()
	if back == nil {
		return false
	}
	e, ok := back.Value.(*lruEntry)
	if !ok {
		return false
	}
	c.removeLocked(e, reason)
	return true
}

// estimateStringsBytes 估算 []string 的内存占用。
//
// 用 len(s)+16 近似「字符串头 + 底层数组」开销；这是估算而非精确值，
// 目的是给内存预算一个稳定可比的量纲（测试断言也用它）。
func estimateStringsBytes(v []string) int64 {
	if len(v) == 0 {
		return 0
	}
	var total int64 = 24 // slice header
	for _, s := range v {
		total += int64(len(s)) + 16
	}
	return total
}

// EstimateStringsBytes 导出估算函数，供内存预算模块复用同一量纲。
func EstimateStringsBytes(v []string) int64 { return estimateStringsBytes(v) }
