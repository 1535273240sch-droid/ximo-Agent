package ctxmgr

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestDegradationLadderOrderIsExact 是本文件最重要的断言：
// 第 24 章的七级降级顺序不能颠倒，这是代码审查要点。
//
// 用常量序列把顺序固化在测试里 —— 任何改动导致顺序变化都会立刻失败。
func TestDegradationLadderOrderIsExact(t *testing.T) {
	want := []DegradationLevel{
		LevelDropUIDelta,             // ① 丢弃可重建 UI delta
		LevelReclaimTokenizerCache,   // ② 回收 tokenizer cache
		LevelReclaimWebCache,         // ③ 回收 web cache
		LevelReclaimBrowser,          // ④ 回收 inactive browser
		LevelLowerBackgroundPriority, // ⑤ 降低 background worker 优先级
		LevelRejectBackground,        // ⑥ 拒绝新的 background 任务
		LevelDegradeInteractive,      // ⑦ 最后才影响 interactive 任务
	}

	got := AllDegradationLevels()
	if len(got) != len(want) {
		t.Fatalf("降级档位数 = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 级 = %v, want %v（降级顺序被颠倒）", i+1, got[i], want[i])
		}
	}

	// 数值必须严格递增 —— 执行器按数值推进，乱序会被破坏。
	for i := 1; i < len(got); i++ {
		if got[i] <= got[i-1] {
			t.Fatalf("档位数值未严格递增: %v(%d) <= %v(%d)",
				got[i], got[i], got[i-1], got[i-1])
		}
	}

	// interactive 必须排最后。
	if got[len(got)-1] != LevelDegradeInteractive {
		t.Fatal("interactive 降级必须是最后一级")
	}
}

// TestDegradationExecutesInOrder 确认实际执行时按档位顺序回收资源。
//
// 这是「顺序不能颠倒」的行为断言，而不仅是常量检查。
func TestDegradationExecutesInOrder(t *testing.T) {
	var mu sync.Mutex
	var executed []string

	m := NewMemoryBudgetManager(MemoryBudgetOptions{
		SoftLimitBytes:      1000,
		HardLimitBytes:      2000,
		EmergencyLimitBytes: 3000,
		UsageFn:             func() int64 { return 0 },
	})

	// 故意用「逆序」注册，验证管理器会自行排序（而不是按注册顺序执行）。
	resources := []struct {
		name  string
		level DegradationLevel
	}{
		{"interactive", LevelDegradeInteractive},
		{"reject_bg", LevelRejectBackground},
		{"lower_bg", LevelLowerBackgroundPriority},
		{"browser", LevelReclaimBrowser},
		{"web_cache", LevelReclaimWebCache},
		{"tokenizer", LevelReclaimTokenizerCache},
		{"ui_delta", LevelDropUIDelta},
	}
	for _, r := range resources {
		level := r.level
		name := r.name
		m.Register(Reclaimable{
			Name:  name,
			Level: level,
			Reclaim: func() int64 {
				mu.Lock()
				executed = append(executed, name)
				mu.Unlock()
				return 100
			},
		})
	}

	// 直接把占用推到 emergency，触发全部七级。
	m.EvaluateWithUsage(context.Background(), 5000)

	mu.Lock()
	defer mu.Unlock()

	want := []string{"ui_delta", "tokenizer", "web_cache", "browser", "lower_bg", "reject_bg", "interactive"}
	if len(executed) != len(want) {
		t.Fatalf("应执行 %d 个回收动作，实际 %d: %v", len(want), len(executed), executed)
	}
	for i := range want {
		if executed[i] != want[i] {
			t.Fatalf("第 %d 个执行的是 %q，want %q（顺序错误）\n实际顺序: %v",
				i+1, executed[i], want[i], executed)
		}
	}
}

// TestDegradationNeverSkipsLevels 确认升档时逐级推进，不跳档。
func TestDegradationNeverSkipsLevels(t *testing.T) {
	var mu sync.Mutex
	var levels []DegradationLevel

	m := NewMemoryBudgetManager(MemoryBudgetOptions{
		SoftLimitBytes:      100,
		HardLimitBytes:      200,
		EmergencyLimitBytes: 300,
		UsageFn:             func() int64 { return 0 },
	})

	for _, lv := range AllDegradationLevels() {
		level := lv
		m.Register(Reclaimable{
			Name:  level.String(),
			Level: level,
			Reclaim: func() int64 {
				mu.Lock()
				levels = append(levels, level)
				mu.Unlock()
				return 1
			},
		})
	}

	// 一步推到最高档。
	m.EvaluateWithUsage(context.Background(), 9999)

	mu.Lock()
	defer mu.Unlock()
	if len(levels) != 7 {
		t.Fatalf("应逐级执行 7 次，实际 %d 次: %v", len(levels), levels)
	}
	for i, lv := range levels {
		if lv != AllDegradationLevels()[i] {
			t.Fatalf("第 %d 次执行的是 %v，应为 %v（跳档了）", i+1, lv, AllDegradationLevels()[i])
		}
	}
}

// TestNoDegradationWhenUnderSoftLimit 确认未超软限时不触发任何回收。
func TestNoDegradationWhenUnderSoftLimit(t *testing.T) {
	var called int

	m := NewMemoryBudgetManager(MemoryBudgetOptions{
		SoftLimitBytes: 1000,
		UsageFn:        func() int64 { return 0 },
	})
	m.Register(Reclaimable{
		Name:  "cache",
		Level: LevelReclaimTokenizerCache,
		Reclaim: func() int64 {
			called++
			return 10
		},
	})

	st := m.EvaluateWithUsage(context.Background(), 500)
	if st.Level != LevelNormal {
		t.Fatalf("低占用下档位应为 normal，实际 %v", st.Level)
	}
	if called != 0 {
		t.Fatal("未超软限不应执行回收")
	}
}

// TestReclaimCacheThenRecover 确认压力回落后档位逐级回退。
func TestReclaimCacheThenRecover(t *testing.T) {
	m := NewMemoryBudgetManager(MemoryBudgetOptions{
		SoftLimitBytes:      1000,
		HardLimitBytes:      2000,
		EmergencyLimitBytes: 3000,
		UsageFn:             func() int64 { return 0 },
	})
	m.Register(LRUReclaimable("tokenizer", LevelReclaimTokenizerCache,
		NewLRU(LRUOptions{MaxEntries: 10})))

	// 升到中间档。
	hi := m.EvaluateWithUsage(context.Background(), 2500)
	if hi.Level == LevelNormal {
		t.Fatal("高占用应进入降级")
	}

	// 压力回落。
	lo := m.EvaluateWithUsage(context.Background(), 10)
	if lo.Level != LevelNormal {
		t.Fatalf("回落后应为 normal，实际 %v", lo.Level)
	}
}

// TestShouldAcceptBackgroundGatesAtLevelSix 确认第 ⑥ 级起拒绝新 background 任务。
func TestShouldAcceptBackgroundGatesAtLevelSix(t *testing.T) {
	m := NewMemoryBudgetManager(MemoryBudgetOptions{
		SoftLimitBytes:      1000,
		HardLimitBytes:      2000,
		EmergencyLimitBytes: 3000,
		UsageFn:             func() int64 { return 0 },
	})

	// 低压力：接受。
	m.EvaluateWithUsage(context.Background(), 100)
	if !m.ShouldAcceptBackground() {
		t.Fatal("低压力下应接受 background 任务")
	}

	// 推到第 ⑥ 级：拒绝。
	m.EvaluateWithUsage(context.Background(), 2999)
	if m.Level() < LevelRejectBackground {
		t.Fatalf("应已到第 ⑥ 级，实际 %v", m.Level())
	}
	if m.ShouldAcceptBackground() {
		t.Fatal("第 ⑥ 级起应拒绝新的 background 任务")
	}
}

// TestInteractiveLastToBeAffected 确认 interactive 只在最高档受影响，且只降级不拒绝。
func TestInteractiveLastToBeAffected(t *testing.T) {
	m := NewMemoryBudgetManager(MemoryBudgetOptions{
		SoftLimitBytes:      1000,
		HardLimitBytes:      2000,
		EmergencyLimitBytes: 3000,
		UsageFn:             func() int64 { return 0 },
	})

	// 任何非最高档：interactive 不受影响。
	m.EvaluateWithUsage(context.Background(), 2500)
	if m.Level() >= LevelDegradeInteractive {
		t.Fatal("测试前提：此占用不应到最高档")
	}
	if got := m.InteractiveBudgetScale(); got != 1.0 {
		t.Fatalf("interactive 缩放应为 1.0，实际 %v", got)
	}

	// 最高档：降级但仍有配额（不是 0，即「影响」而非「拒绝」）。
	m.EvaluateWithUsage(context.Background(), 5000)
	if m.Level() != LevelDegradeInteractive {
		t.Fatalf("应为最高档，实际 %v", m.Level())
	}
	scale := m.InteractiveBudgetScale()
	if scale <= 0 || scale >= 1.0 {
		t.Fatalf("interactive 缩效应为 (0,1)，实际 %v", scale)
	}
}

// TestLevelChangeHookFires 确认档位变化回调（observability 埋点依赖）。
func TestLevelChangeHookFires(t *testing.T) {
	m := NewMemoryBudgetManager(MemoryBudgetOptions{
		SoftLimitBytes:      1000,
		HardLimitBytes:      2000,
		EmergencyLimitBytes: 3000,
		UsageFn:             func() int64 { return 0 },
	})
	m.Register(LRUReclaimable("tok", LevelReclaimTokenizerCache, NewLRU(LRUOptions{MaxEntries: 5})))

	var transitions [][2]DegradationLevel
	m.SetLevelChangeHook(func(from, to DegradationLevel) {
		transitions = append(transitions, [2]DegradationLevel{from, to})
	})

	m.EvaluateWithUsage(context.Background(), 2500)
	if len(transitions) == 0 {
		t.Fatal("档位变化应触发回调")
	}
	// 首位应是从 normal 起步。
	if transitions[0][0] != LevelNormal {
		t.Fatalf("首个跃迁起点应为 normal，实际 %v", transitions[0][0])
	}
}

// TestMemoryBudgetDefaultsNonZero 确认默认预算各池非零（防止漏配某池）。
func TestMemoryBudgetDefaultsNonZero(t *testing.T) {
	b := DefaultMemoryBudget()
	for name, v := range map[string]int64{
		"MaxMessagesBytes":     b.MaxMessagesBytes,
		"MaxToolResultBytes":   b.MaxToolResultBytes,
		"MaxEventBufferBytes":  b.MaxEventBufferBytes,
		"MaxCheckpointBytes":   b.MaxCheckpointBytes,
		"MaxWorkerOutputBytes": b.MaxWorkerOutputBytes,
	} {
		if v <= 0 {
			t.Errorf("%s = %d，应为正数", name, v)
		}
	}
	if b.Total() <= 0 {
		t.Fatal("总预算应为正数")
	}
}

// TestLRUReclaimableFreesRegisteredCaches 确认 LRUReclaimable 回收多个缓存。
func TestLRUReclaimableFreesRegisteredCaches(t *testing.T) {
	c1 := NewLRU(LRUOptions{MaxEntries: 100})
	c2 := NewLRU(LRUOptions{MaxEntries: 100})
	for i := 0; i < 50; i++ {
		c1.Put(string(rune('a'+i%26)), []string{"value"})
		c2.Put(string(rune('a'+i%26)), []string{"value"})
	}

	r := LRUReclaimable("combined", LevelReclaimTokenizerCache, c1, c2)
	freed := r.Reclaim()

	if freed <= 0 {
		t.Fatal("应报告释放了字节")
	}
	if c1.Len() != 0 || c2.Len() != 0 {
		t.Fatal("两个缓存都应被清空")
	}
}

// TestReclaimIsIdempotent 确认回收动作可安全重复调用（降级/回退反复触发）。
func TestReclaimIsIdempotent(t *testing.T) {
	c := NewLRU(LRUOptions{MaxEntries: 10})
	for i := 0; i < 10; i++ {
		c.Put(string(rune('a'+i)), []string{"v"})
	}
	r := LRUReclaimable("c", LevelReclaimTokenizerCache, c)

	first := r.Reclaim()
	// 第二次已无内容可回收，不应 panic 或返回负数。
	second := r.Reclaim()
	if second != 0 {
		t.Fatalf("已清空后再次回收应返回 0，实际 %d", second)
	}
	if first <= 0 {
		t.Fatalf("首次应释放 >0，实际 %d", first)
	}
}

// TestPressureStateReportsThresholds 确认压力状态上报三级阈值（前端/告警依赖）。
func TestPressureStateReportsThresholds(t *testing.T) {
	m := NewMemoryBudgetManager(MemoryBudgetOptions{
		SoftLimitBytes:      1000,
		HardLimitBytes:      2000,
		EmergencyLimitBytes: 3000,
		UsageFn:             func() int64 { return 0 },
	})

	st := m.EvaluateWithUsage(context.Background(), 1500)
	if st.SoftLimitBytes != 1000 || st.HardLimitBytes != 2000 || st.EmergencyLimitBytes != 3000 {
		t.Fatalf("阈值上报有误: %+v", st)
	}
	if st.UsageBytes != 1500 {
		t.Fatalf("UsageBytes = %d", st.UsageBytes)
	}
	if st.LevelName == "" {
		t.Fatal("应上报档位名称")
	}
	if st.Timestamp.IsZero() {
		t.Fatal("应上报采样时间")
	}
}

// TestManagerConcurrentEvaluate 确认并发评估安全（配合 -race）。
func TestManagerConcurrentEvaluate(t *testing.T) {
	m := NewMemoryBudgetManager(MemoryBudgetOptions{
		SoftLimitBytes:      1000,
		HardLimitBytes:      2000,
		EmergencyLimitBytes: 3000,
		UsageFn:             func() int64 { return 0 },
	})
	for _, lv := range AllDegradationLevels() {
		level := lv
		m.Register(Reclaimable{
			Name:    level.String(),
			Level:   level,
			Reclaim: func() int64 { return 1 },
		})
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				usage := int64((i*100 + j*37) % 4000)
				m.EvaluateWithUsage(context.Background(), usage)
				_ = m.Level()
				_ = m.InteractiveBudgetScale()
				_ = m.ShouldAcceptBackground()
			}
		}(i)
	}
	wg.Wait()

	_ = m.Snapshot()
}

// TestCountersTrackShed 确认拒绝计数被记录（运维需要知道 shed 发生了多少次）。
func TestCountersTrackShed(t *testing.T) {
	m := NewMemoryBudgetManager(MemoryBudgetOptions{
		SoftLimitBytes:      100,
		HardLimitBytes:      200,
		EmergencyLimitBytes: 300,
		UsageFn:             func() int64 { return 0 },
	})

	m.EvaluateWithUsage(context.Background(), 5000)

	rejected, degraded := m.Counters()
	if rejected == 0 {
		t.Error("应记录拒绝 background 的次数")
	}
	if degraded == 0 {
		t.Error("应记录影响 interactive 的次数")
	}
}

// TestDefaultUsageFnReturnsPositive 确认默认内存采样可用。
func TestDefaultUsageFnReturnsPositive(t *testing.T) {
	// 先分配一点内存，确保 HeapAlloc > 0。
	buf := make([]byte, 1<<20)
	for i := range buf {
		buf[i] = byte(i)
	}
	_ = buf

	if got := DefaultUsageFn(); got <= 0 {
		t.Fatalf("DefaultUsageFn = %d, 应为正数", got)
	}
}

// TestEvaluateUsesInjectedUsageFn 确认 Evaluate 使用注入的采样函数。
func TestEvaluateUsesInjectedUsageFn(t *testing.T) {
	var calls int
	m := NewMemoryBudgetManager(MemoryBudgetOptions{
		SoftLimitBytes: 1000,
		UsageFn: func() int64 {
			calls++
			return 50
		},
	})

	st := m.Evaluate(context.Background())
	if calls != 1 {
		t.Fatalf("采样函数应被调用 1 次，实际 %d", calls)
	}
	if st.UsageBytes != 50 {
		t.Fatalf("UsageBytes = %d, want 50", st.UsageBytes)
	}
}

// TestBudgetManagerClockInjection 确认时间戳可注入（测试可复现）。
func TestBudgetManagerClockInjection(t *testing.T) {
	fixed := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m := NewMemoryBudgetManager(MemoryBudgetOptions{
		SoftLimitBytes: 1000,
		UsageFn:        func() int64 { return 0 },
	})
	m.now = func() time.Time { return fixed }

	st := m.EvaluateWithUsage(context.Background(), 10)
	if !st.Timestamp.Equal(fixed) {
		t.Fatalf("时间戳 = %v, want %v", st.Timestamp, fixed)
	}
}
