package ctxmgr

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 有界 LRU —— 第 23 章硬性要求，72h soak 验收的核心
// ---------------------------------------------------------------------------

// TestLRUBoundedByEntries 确认条目数上限被严格执行。
//
// 对应任务书：「绝对不能用无限增长的 map[string][]string」。
// 这里插入远超上限的条目，断言内部条目数始终不超过 MaxEntries。
func TestLRUBoundedByEntries(t *testing.T) {
	const maxEntries = 100
	c := NewLRU(LRUOptions{MaxEntries: maxEntries})

	for i := 0; i < maxEntries*50; i++ {
		c.Put(fmt.Sprintf("key-%d", i), []string{"a", "b", "c"})
	}

	if got := c.Len(); got > maxEntries {
		t.Fatalf("条目数 %d 超过上限 %d", got, maxEntries)
	}
	if got := c.Len(); got == 0 {
		t.Fatal("不应把缓存清空")
	}
}

// TestLRUBoundedByBytes 确认字节上限被严格执行。
func TestLRUBoundedByBytes(t *testing.T) {
	const maxBytes = 4096
	c := NewLRU(LRUOptions{MaxBytes: maxBytes})

	// 每条约 500 字节，插入 200 条 → 远超 4KB。
	value := []string{strings.Repeat("x", 500)}
	for i := 0; i < 200; i++ {
		c.Put(fmt.Sprintf("k%d", i), value)
	}

	if got := c.Bytes(); got > maxBytes {
		t.Fatalf("字节占用 %d 超过上限 %d", got, maxBytes)
	}
}

// TestLRUEvictsLeastRecentlyUsed 确认按 LRU 顺序淘汰（而非随机）。
func TestLRUEvictsLeastRecentlyUsed(t *testing.T) {
	c := NewLRU(LRUOptions{MaxEntries: 3})

	c.Put("a", []string{"1"})
	c.Put("b", []string{"2"})
	c.Put("c", []string{"3"})

	// 访问 a → a 变为最近使用，b 成为最久未使用。
	if _, ok := c.Get("a"); !ok {
		t.Fatal("a 应命中")
	}

	// 插入 d → 应淘汰 b。
	c.Put("d", []string{"4"})

	if _, ok := c.Get("b"); ok {
		t.Fatal("b 应已被淘汰")
	}
	if _, ok := c.Get("a"); !ok {
		t.Fatal("a 最近被访问，不应被淘汰")
	}
	if _, ok := c.Get("d"); !ok {
		t.Fatal("d 刚插入，应存在")
	}
}

// TestLRUTTLExpiry 确认 TTL 过期。
func TestLRUTTLExpiry(t *testing.T) {
	var now atomic.Int64
	base := time.Now()
	now.Store(base.UnixNano())

	c := NewLRU(LRUOptions{
		MaxEntries: 10,
		TTL:        100 * time.Millisecond,
		now:        func() time.Time { return time.Unix(0, now.Load()) },
	})

	c.Put("k", []string{"v"})
	if _, ok := c.Get("k"); !ok {
		t.Fatal("未过期时应命中")
	}

	now.Store(base.Add(200 * time.Millisecond).UnixNano())
	if _, ok := c.Get("k"); ok {
		t.Fatal("过期后不应命中")
	}
	if c.Len() != 0 {
		t.Fatalf("过期条目应被清理，实际剩 %d", c.Len())
	}
}

// TestLRUPurgeExpired 确认后台清理能回收过期条目。
func TestLRUPurgeExpired(t *testing.T) {
	var now atomic.Int64
	base := time.Now()
	now.Store(base.UnixNano())

	c := NewLRU(LRUOptions{
		MaxEntries: 100,
		TTL:        50 * time.Millisecond,
		now:        func() time.Time { return time.Unix(0, now.Load()) },
	})

	for i := 0; i < 50; i++ {
		c.Put(fmt.Sprintf("k%d", i), []string{"v"})
	}
	now.Store(base.Add(100 * time.Millisecond).UnixNano())

	purged := c.PurgeExpired()
	if purged != 50 {
		t.Fatalf("应清理 50 条过期条目，实际 %d", purged)
	}
	if c.Len() != 0 || c.Bytes() != 0 {
		t.Fatalf("清理后应为空，实际 entries=%d bytes=%d", c.Len(), c.Bytes())
	}
}

// TestLRUOnEvictCallback 确认淘汰回调（降级阶梯的可观测性依赖它）。
func TestLRUOnEvictCallback(t *testing.T) {
	var reasons []EvictReason
	var mu sync.Mutex

	c := NewLRU(LRUOptions{
		MaxEntries: 2,
		OnEvict: func(key string, reason EvictReason) {
			mu.Lock()
			reasons = append(reasons, reason)
			mu.Unlock()
		},
	})

	c.Put("a", []string{"1"})
	c.Put("b", []string{"2"})
	c.Put("c", []string{"3"}) // 触发容量淘汰

	mu.Lock()
	defer mu.Unlock()
	if len(reasons) != 1 || reasons[0] != EvictEntries {
		t.Fatalf("应有 1 次容量淘汰，实际 %v", reasons)
	}
}

// TestLRUClearReleasesBytes 确认 Clear 返回释放的字节数（降级阶梯第②步）。
func TestLRUClearReleasesBytes(t *testing.T) {
	c := NewLRU(LRUOptions{MaxEntries: 100, MaxBytes: 1 << 20})
	for i := 0; i < 20; i++ {
		c.Put(fmt.Sprintf("k%d", i), []string{strings.Repeat("y", 100)})
	}

	before := c.Bytes()
	if before <= 0 {
		t.Fatal("应有字节占用")
	}
	freed := c.Clear()
	if freed != before {
		t.Fatalf("Clear 返回 %d，实际占用 %d", freed, before)
	}
	if c.Len() != 0 || c.Bytes() != 0 {
		t.Fatal("Clear 后应为空")
	}
}

// TestLRUResizeShrinks 确认内存压力下收紧容量能立即回收。
func TestLRUResizeShrinks(t *testing.T) {
	c := NewLRU(LRUOptions{MaxEntries: 1000})
	for i := 0; i < 1000; i++ {
		c.Put(fmt.Sprintf("k%d", i), []string{"v"})
	}

	c.Resize(50, 0)
	if got := c.Len(); got > 50 {
		t.Fatalf("收缩后条目数 %d 应 ≤ 50", got)
	}
}

// TestLRUConcurrentAccess 确认并发安全（配合 -race）。
func TestLRUConcurrentAccess(t *testing.T) {
	c := NewLRU(LRUOptions{MaxEntries: 200, MaxBytes: 1 << 20, TTL: time.Minute})

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				key := fmt.Sprintf("k%d-%d", i, j%50)
				c.Put(key, []string{"a", "b"})
				c.Get(key)
				if j%100 == 0 {
					_ = c.Stats()
				}
			}
		}(i)
	}
	wg.Wait()

	if c.Len() > 200 {
		t.Fatalf("并发后条目数 %d 超上限", c.Len())
	}
}

// TestLRUStatsTrackHitsMisses 确认命中率统计准确。
func TestLRUStatsTrackHitsMisses(t *testing.T) {
	c := NewLRU(LRUOptions{MaxEntries: 10})
	c.Put("a", []string{"1"})

	c.Get("a") // hit
	c.Get("a") // hit
	c.Get("z") // miss

	st := c.Stats()
	if st.Hits != 2 {
		t.Errorf("Hits = %d, want 2", st.Hits)
	}
	if st.Misses != 1 {
		t.Errorf("Misses = %d, want 1", st.Misses)
	}
}

// TestLRUNoUnboundedGrowthSoak 是 72h soak 的加速替代：
// 用大量随机插入模拟长期运行，断言内存占用收敛在界内。
func TestLRUNoUnboundedGrowthSoak(t *testing.T) {
	const (
		maxEntries = 500
		maxBytes   = 256 << 10
		iterations = 200_000
	)
	c := NewLRU(LRUOptions{MaxEntries: maxEntries, MaxBytes: maxBytes, TTL: time.Hour})

	// 用确定性的伪随机键值，避免引入 math/rand 的种子不确定性。
	seed := uint64(12345)
	next := func() uint64 {
		seed ^= seed << 13
		seed ^= seed >> 7
		seed ^= seed << 17
		return seed
	}

	for i := 0; i < iterations; i++ {
		key := fmt.Sprintf("token-%d", next()%50_000)
		val := []string{strings.Repeat("z", int(next()%64))}
		c.Put(key, val)
		if i%1000 == 0 {
			c.Get(fmt.Sprintf("token-%d", next()%50_000))
		}
	}

	st := c.Stats()
	if st.Entries > maxEntries {
		t.Fatalf("soak 后条目数 %d 超上限 %d", st.Entries, maxEntries)
	}
	if st.Bytes > maxBytes {
		t.Fatalf("soak 后字节 %d 超上限 %d", st.Bytes, maxBytes)
	}
	t.Logf("soak 完成：%d 次插入后 entries=%d bytes=%d evictions=%d",
		iterations, st.Entries, st.Bytes, st.Evictions)
}

// ---------------------------------------------------------------------------
// Token 预算（第 22.1 章）
// ---------------------------------------------------------------------------

// TestBudgetSevenSegmentsMatchSpec 确认七段比例符合第 22.1 章。
func TestBudgetSevenSegmentsMatchSpec(t *testing.T) {
	const window = 100_000
	b := ComputeBudget(window, DefaultBudgetOptions())

	if err := b.Validate(); err != nil {
		t.Fatalf("预算自洽性校验失败: %v", err)
	}

	// 安全边际 5% → usable = 95000。
	if b.SafetyMargin != 5000 {
		t.Errorf("SafetyMargin = %d, want 5000", b.SafetyMargin)
	}
	if b.Usable != 95_000 {
		t.Errorf("Usable = %d, want 95000", b.Usable)
	}

	// 各段应为 usable * ratio（允许 1 token 的取整误差）。
	checks := []struct {
		name  string
		got   int
		ratio float64
	}{
		{"system", b.System, 0.05},
		{"tools", b.Tools, 0.15},
		{"recent", b.RecentConv, 0.25},
		{"working_memory", b.WorkingMem, 0.15},
		{"knowledge", b.Knowledge, 0.10},
		{"reasoning", b.Reasoning, 0.10},
		{"output", b.Output, 0.20},
	}
	for _, c := range checks {
		want := int(float64(95_000) * c.ratio)
		if diff := c.got - want; diff > 1 || diff < -1 {
			t.Errorf("%s = %d, want ≈%d", c.name, c.got, want)
		}
	}
}

// TestBudgetScalesWithWindow 确认预算随 Provider 窗口动态变化（不是固定值）。
func TestBudgetScalesWithWindow(t *testing.T) {
	small := ComputeBudget(8_000, DefaultBudgetOptions())
	large := ComputeBudget(1_000_000, DefaultBudgetOptions())

	if !(small.InputBudget() < large.InputBudget()) {
		t.Fatalf("小窗口预算 %d 应小于大窗口 %d", small.InputBudget(), large.InputBudget())
	}
	if small.ContextWindow != 8_000 || large.ContextWindow != 1_000_000 {
		t.Fatal("窗口字段应回填原始值")
	}
}

// TestBudgetSmallWindowMinimums 确认小窗口下每段不会归零。
func TestBudgetSmallWindowMinimums(t *testing.T) {
	opts := DefaultBudgetOptions()
	opts.MinSegmentTokens = 100
	b := ComputeBudget(2_000, opts)

	for name, v := range map[string]int{
		"system": b.System, "tools": b.Tools, "recent": b.RecentConv,
		"working": b.WorkingMem, "knowledge": b.Knowledge,
		"reasoning": b.Reasoning, "output": b.Output,
	} {
		if v < 100 {
			t.Errorf("%s = %d，低于 MinSegmentTokens=100", name, v)
		}
	}
	if err := b.Validate(); err != nil {
		t.Fatalf("小窗口预算应自洽: %v", err)
	}
}

// TestBudgetShrinksWhenOverWindow 确认分段合计超窗口时会被回收。
func TestBudgetShrinksWhenOverWindow(t *testing.T) {
	opts := DefaultBudgetOptions()
	opts.MinSegmentTokens = 1000 // 强制放大最小值，制造超限
	b := ComputeBudget(5_000, opts)

	if err := b.Validate(); err != nil {
		t.Fatalf("超限时应自动回收至自洽: %v", err)
	}
	if total := b.InputBudget() + b.Output; total > b.ContextWindow {
		t.Fatalf("合计 %d 仍超窗口 %d", total, b.ContextWindow)
	}
}

// TestBudgetDisabledForZeroWindow 确认 window<=0 时禁用预算（不崩）。
func TestBudgetDisabledForZeroWindow(t *testing.T) {
	b := ComputeBudget(0, DefaultBudgetOptions())
	if b.ContextWindow != 0 {
		t.Fatal("window<=0 应返回禁用态")
	}
	if err := b.Validate(); err != nil {
		t.Fatalf("禁用态不应报错: %v", err)
	}
}

// TestBudgetCarriesVersions 确认预算携带版本号（第 22.2 章）。
func TestBudgetCarriesVersions(t *testing.T) {
	b := ComputeBudget(100_000, DefaultBudgetOptions())
	if err := b.Versions.Validate(); err != nil {
		t.Fatalf("版本号应齐全: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 压缩版本化（第 22.2 章）
// ---------------------------------------------------------------------------

// TestVersionsAllFourRequired 确认四个版本号缺一不可。
func TestVersionsAllFourRequired(t *testing.T) {
	v := CurrentVersions()
	if err := v.Validate(); err != nil {
		t.Fatalf("当前版本集合应有效: %v", err)
	}

	// 逐个清空，断言都能被检出。
	fields := []struct {
		name string
		mut  func(*Versions)
	}{
		{"context_version", func(x *Versions) { x.ContextVersion = "" }},
		{"compression_version", func(x *Versions) { x.CompressionVersion = "" }},
		{"tokenizer_version", func(x *Versions) { x.TokenizerVersion = "" }},
		{"prompt_schema_version", func(x *Versions) { x.PromptSchemaVersion = "" }},
	}
	for _, f := range fields {
		bad := CurrentVersions()
		f.mut(&bad)
		if err := bad.Validate(); err == nil {
			t.Errorf("缺少 %s 时应校验失败", f.name)
		}
	}
}

// TestCompactedContextAlwaysVersioned 确认压缩产物必带版本号。
func TestCompactedContextAlwaysVersioned(t *testing.T) {
	m := newTestManager(t)
	msgs := buildConversation(t, 50)

	out, err := m.Compact(contextTODO(), SessionSnapshot{
		Messages:     msgs,
		PromptTokens: 900_000, // 高到触发压缩
	})
	if err != nil {
		t.Fatalf("Compact 报错: %v", err)
	}
	if err := out.Versions.Validate(); err != nil {
		t.Fatalf("压缩产物版本号不齐全: %v", err)
	}
	if out.Versions.CompressionVersion != CompressionVersion {
		t.Fatalf("CompressionVersion = %q", out.Versions.CompressionVersion)
	}
	if out.Versions.TokenizerVersion != TokenizerVersion {
		t.Fatalf("TokenizerVersion = %q", out.Versions.TokenizerVersion)
	}
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

// fakeTokenizer 让压缩/预算逻辑可测，无需加载 7.8MB 词表。
type fakeTokenizer struct{ perMessage int }

func (f fakeTokenizer) CountTokens(text string) int { return len([]rune(text)) / 4 }
func (f fakeTokenizer) CountMessageTokens(msgs []Message) int {
	return f.perMessage * len(msgs)
}
func (f fakeTokenizer) Version() string { return "fake-v1" }
func (f fakeTokenizer) Ready() bool     { return true }

func newTestManager(t *testing.T) *ContextManager {
	t.Helper()
	cfg := DefaultAgentConfig()
	cfg.RecentKeep = 3
	return NewContextManager(ContextManagerOptions{
		Config:    cfg,
		Tokenizer: fakeTokenizer{perMessage: 100},
	})
}

// buildConversation 构造一段带工具调用的对话（含 tool_calls 配对）。
func buildConversation(t *testing.T, rounds int) []Message {
	t.Helper()
	_ = t
	msgs := []Message{
		{Role: "system", Content: "你是助手"},
		{Role: "user", Content: "帮我读文件"},
	}
	for i := 0; i < rounds; i++ {
		callID := fmt.Sprintf("call_%d", i)
		msgs = append(msgs,
			Message{
				Role:    "assistant",
				Content: fmt.Sprintf("第 %d 轮思考", i),
				ToolCalls: []byte(fmt.Sprintf(
					`[{"id":"%s","type":"function","function":{"name":"file_read","arguments":"{}"}}]`, callID)),
			},
			Message{
				Role:       "tool",
				Content:    strings.Repeat("文件内容", 200),
				ToolCallID: callID,
			},
		)
	}
	return msgs
}

func contextTODO() context.Context { return context.Background() }
