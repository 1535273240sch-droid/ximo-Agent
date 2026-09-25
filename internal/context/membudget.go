// membudget.go —— 全局内存预算与降级策略（架构文档第 24 章）。
//
// 两级阈值 + 七级降级阶梯：
//
//	Soft limit   —— 开始回收可重建的缓存，不影响用户可见行为
//	Hard limit   —— 收紧缓存上限并压缩非关键缓冲
//	Emergency    —— 逐步 shed 负载，最后才影响 interactive 任务
//
// ★ 降级顺序是代码审查要点，不能颠倒。本文件用 DegradationLevel 常量把顺序
// 固化在类型里（数值即优先级），任何新增降级动作都必须插到正确的档位，
// 而不是在调用点随意排列。
//
// 七级顺序（第 24 章原文）：
//
//	① 丢弃可重建 UI delta
//	② 回收 tokenizer cache
//	③ 回收 web cache
//	④ 回收 inactive browser（通知任务 05）
//	⑤ 降低 background worker 优先级
//	⑥ 拒绝新的 background 任务
//	⑦ 最后才影响 interactive 任务
package ctxmgr

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// MemoryBudget 各内存池的字节上限（第 24 章给出的结构体，字段名照抄）。
type MemoryBudget struct {
	MaxMessagesBytes     int64
	MaxToolResultBytes   int64
	MaxEventBufferBytes  int64
	MaxCheckpointBytes   int64
	MaxWorkerOutputBytes int64
}

// Total 返回各池上限合计。
func (b MemoryBudget) Total() int64 {
	return b.MaxMessagesBytes + b.MaxToolResultBytes + b.MaxEventBufferBytes +
		b.MaxCheckpointBytes + b.MaxWorkerOutputBytes
}

// DefaultMemoryBudget 一组保守默认值（合计约 512MB 可回收池）。
func DefaultMemoryBudget() MemoryBudget {
	return MemoryBudget{
		MaxMessagesBytes:     192 << 20, // 192MB
		MaxToolResultBytes:   128 << 20, // 128MB
		MaxEventBufferBytes:  64 << 20,  // 64MB
		MaxCheckpointBytes:   64 << 20,  // 64MB
		MaxWorkerOutputBytes: 64 << 20,  // 64MB
	}
}

// DegradationLevel 降级档位。数值即优先级 —— 数值越大，越晚执行（越贴近 interactive）。
//
// 顺序是硬约束：执行器按数值升序推进，绝不允许跳档或逆序。
type DegradationLevel int

const (
	// LevelNormal 正常，无降级。
	LevelNormal DegradationLevel = 0
	// LevelDropUIDelta ① 丢弃可重建的 UI delta。
	LevelDropUIDelta DegradationLevel = 1
	// LevelReclaimTokenizerCache ② 回收 tokenizer cache。
	LevelReclaimTokenizerCache DegradationLevel = 2
	// LevelReclaimWebCache ③ 回收 web cache。
	LevelReclaimWebCache DegradationLevel = 3
	// LevelReclaimBrowser ④ 回收 inactive browser（通知任务 05）。
	LevelReclaimBrowser DegradationLevel = 4
	// LevelLowerBackgroundPriority ⑤ 降低 background worker 优先级。
	LevelLowerBackgroundPriority DegradationLevel = 5
	// LevelRejectBackground ⑥ 拒绝新的 background 任务。
	LevelRejectBackground DegradationLevel = 6
	// LevelDegradeInteractive ⑦ 最后才影响 interactive 任务。
	LevelDegradeInteractive DegradationLevel = 7
)

// degradationNames 档位名称，供日志与状态上报。
var degradationNames = map[DegradationLevel]string{
	LevelNormal:                  "normal",
	LevelDropUIDelta:             "drop_ui_delta",
	LevelReclaimTokenizerCache:   "reclaim_tokenizer_cache",
	LevelReclaimWebCache:         "reclaim_web_cache",
	LevelReclaimBrowser:          "reclaim_inactive_browser",
	LevelLowerBackgroundPriority: "lower_background_priority",
	LevelRejectBackground:        "reject_background_tasks",
	LevelDegradeInteractive:      "degrade_interactive",
}

func (l DegradationLevel) String() string {
	if n, ok := degradationNames[l]; ok {
		return n
	}
	return fmt.Sprintf("level_%d", int(l))
}

// AllDegradationLevels 按执行顺序返回全部档位（升序）。
//
// 供测试断言「顺序未被颠倒」以及执行器迭代使用。
func AllDegradationLevels() []DegradationLevel {
	return []DegradationLevel{
		LevelDropUIDelta,
		LevelReclaimTokenizerCache,
		LevelReclaimWebCache,
		LevelReclaimBrowser,
		LevelLowerBackgroundPriority,
		LevelRejectBackground,
		LevelDegradeInteractive,
	}
}

// Reclaimable 描述一个可被降级动作回收的资源池。
//
// Reclaim 返回释放的字节数；实现必须是幂等且可重入安全的。
type Reclaimable struct {
	// Name 资源名（进日志/指标）。
	Name string
	// Level 该资源对应的降级档位 —— 决定它何时被回收。
	Level DegradationLevel
	// Reclaim 执行回收，返回释放字节数。
	Reclaim func() int64
	// Notify 可选：回收时通知外部（例如通知任务 05 关闭 inactive browser）。
	Notify func()
}

// PressureState 当前内存压力状态。
type PressureState struct {
	// UsageBytes 当前估算占用。
	UsageBytes int64 `json:"usage_bytes"`
	// SoftLimitBytes / HardLimitBytes / EmergencyBytes 三级阈值。
	SoftLimitBytes      int64 `json:"soft_limit_bytes"`
	HardLimitBytes      int64 `json:"hard_limit_bytes"`
	EmergencyLimitBytes int64 `json:"emergency_limit_bytes"`
	// Level 当前降级档位。
	Level DegradationLevel `json:"level"`
	// LevelName 档位名称。
	LevelName string `json:"level_name"`
	// LastReclaimedBytes 最近一次回收释放的字节数。
	LastReclaimedBytes int64 `json:"last_reclaimed_bytes"`
	// Timestamp 采样时间。
	Timestamp time.Time `json:"timestamp"`
}

// MemoryBudgetManager 内存预算管理器。
//
// 它不直接持有各缓存，而是通过注册的 Reclaimable 列表执行降级 ——
// 这样 context/provider/knowledge 各模块只需注册自己的回收函数，
// 不必互相依赖（跨目录零耦合）。
type MemoryBudgetManager struct {
	mu sync.RWMutex

	budget MemoryBudget

	// soft/hard/emergency 三级阈值（字节）。
	soft      int64
	hard      int64
	emergency int64

	// reclaimables 按 Level 升序排列的可回收资源。
	reclaimables []Reclaimable

	// level 当前生效档位。
	level atomic.Int32

	// usageFn 采样当前内存占用；nil 时用 runtime.MemStats。
	usageFn func() int64

	// rejectedBackground / degradedInteractive 计数器（供测试与指标）。
	rejectedBackground  atomic.Uint64
	degradedInteractive atomic.Uint64

	// now 注入时钟。
	now func() time.Time

	// onLevelChange 档位变化回调（供 observability 埋点）。
	onLevelChange func(from, to DegradationLevel)
}

// MemoryBudgetOptions 管理器构造参数。
type MemoryBudgetOptions struct {
	Budget MemoryBudget
	// SoftLimitBytes 开始回收缓存。0 时按 Budget.Total() 推算。
	SoftLimitBytes int64
	// HardLimitBytes 收紧缓存上限。0 时按 soft * 1.25 推算。
	HardLimitBytes int64
	// EmergencyLimitBytes 进入紧急 shed。0 时按 soft * 1.5 推算。
	EmergencyLimitBytes int64
	// UsageFn 内存占用采样函数（默认 runtime.MemStats 的 HeapAlloc）。
	UsageFn func() int64
}

// NewMemoryBudgetManager 构造管理器。
func NewMemoryBudgetManager(opts MemoryBudgetOptions) *MemoryBudgetManager {
	budget := opts.Budget
	if budget.Total() == 0 {
		budget = DefaultMemoryBudget()
	}
	soft := opts.SoftLimitBytes
	if soft <= 0 {
		soft = budget.Total()
	}
	hard := opts.HardLimitBytes
	if hard <= 0 {
		hard = soft + soft/4
	}
	emergency := opts.EmergencyLimitBytes
	if emergency <= 0 {
		emergency = soft + soft/2
	}
	usageFn := opts.UsageFn
	if usageFn == nil {
		usageFn = DefaultUsageFn
	}

	return &MemoryBudgetManager{
		budget:    budget,
		soft:      soft,
		hard:      hard,
		emergency: emergency,
		usageFn:   usageFn,
		now:       time.Now,
	}
}

// DefaultUsageFn 用 runtime.MemStats.HeapAlloc 作为占用估算。
func DefaultUsageFn() int64 {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return int64(ms.HeapAlloc)
}

// Budget 返回内存预算副本。
func (m *MemoryBudgetManager) Budget() MemoryBudget {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.budget
}

// Register 注册一个可回收资源。
//
// 注册顺序无关 —— 内部会按 Level 排序，保证执行顺序永远符合第 24 章阶梯。
func (m *MemoryBudgetManager) Register(r Reclaimable) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reclaimables = append(m.reclaimables, r)
	m.sortLocked()
}

// SetLevelChangeHook 设置档位变化回调。
func (m *MemoryBudgetManager) SetLevelChangeHook(fn func(from, to DegradationLevel)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onLevelChange = fn
}

// Level 返回当前降级档位。
func (m *MemoryBudgetManager) Level() DegradationLevel {
	return DegradationLevel(m.level.Load())
}

// Evaluate 采样内存占用并推进/回退降级档位，返回压力状态。
//
// 这是主循环每个 turn 都应调用的入口。
func (m *MemoryBudgetManager) Evaluate(ctx context.Context) PressureState {
	usage := m.usageFn()
	return m.EvaluateWithUsage(ctx, usage)
}

// EvaluateWithUsage 用指定占用值评估（便于测试注入确定数值）。
func (m *MemoryBudgetManager) EvaluateWithUsage(ctx context.Context, usage int64) PressureState {
	target := m.targetLevelFor(usage)

	// 逐档推进 —— 绝不跳档，保证每一级资源都被按序回收。
	for m.Level() < target {
		next := m.Level() + 1
		m.applyLevel(ctx, next)
	}

	// 压力回落时逐档回退（同样不跳档）。
	for m.Level() > target {
		m.applyLevel(ctx, m.Level()-1)
	}

	m.mu.RLock()
	defer m.mu.RUnlock()
	level := DegradationLevel(m.level.Load())
	return PressureState{
		UsageBytes:          usage,
		SoftLimitBytes:      m.soft,
		HardLimitBytes:      m.hard,
		EmergencyLimitBytes: m.emergency,
		Level:               level,
		LevelName:           level.String(),
		Timestamp:           m.now(),
	}
}

// targetLevelFor 根据占用值计算目标档位。
//
// 三段映射关系（每档都必须可达，否则该级资源永远不会被回收）：
//
//	占用 < soft                    → 0 normal
//	[soft, hard)                   → ①..③（丢弃 UI delta → 回收 tokenizer → 回收 web cache）
//	[hard, emergency)              → ④..⑥（回收 browser → 降 bg 优先级 → 拒绝 bg）
//	占用 ≥ emergency               → ⑦ 影响 interactive
func (m *MemoryBudgetManager) targetLevelFor(usage int64) DegradationLevel {
	m.mu.RLock()
	soft, hard, emergency := m.soft, m.hard, m.emergency
	m.mu.RUnlock()

	switch {
	case usage >= emergency:
		return LevelDegradeInteractive
	case usage >= hard:
		return bandLevel(usage, hard, emergency,
			LevelReclaimBrowser, LevelRejectBackground)
	case usage >= soft:
		return bandLevel(usage, soft, hard,
			LevelDropUIDelta, LevelReclaimWebCache)
	default:
		return LevelNormal
	}
}

// bandLevel 把 [lo, hi) 内的 usage 均匀映射到 [loLevel, hiLevel] 的档位区间（两端都可达）。
//
// 用四舍五入而不是截断 —— 截断会让区间末端那一级永远取不到，
// 导致「拒绝 background 任务」这类档位形同虚设。
func bandLevel(usage, lo, hi int64, loLevel, hiLevel DegradationLevel) DegradationLevel {
	if hi <= lo {
		return hiLevel
	}
	span := int(hiLevel - loLevel)
	if span <= 0 {
		return hiLevel
	}

	ratio := float64(usage-lo) / float64(hi-lo)
	if ratio < 0 {
		ratio = 0
	}
	if ratio > 1 {
		ratio = 1
	}

	level := loLevel + DegradationLevel(int(ratio*float64(span)+0.5))
	if level < loLevel {
		level = loLevel
	}
	if level > hiLevel {
		level = hiLevel
	}
	return level
}

// applyLevel 进入指定档位：执行该档位的回收动作（或记录回退）。
//
// 回退（数值变小）时不再执行回收 —— 缓存会自然重新填充。
func (m *MemoryBudgetManager) applyLevel(ctx context.Context, level DegradationLevel) {
	m.mu.Lock()
	prev := DegradationLevel(m.level.Load())
	hook := m.onLevelChange
	m.mu.Unlock()

	if prev == level {
		return
	}

	var reclaimed int64
	if level > prev {
		// 升档：执行 [prev+1, level] 区间内所有档位的回收动作。
		for _, r := range m.reclaimablesSnapshot() {
			if r.Level > prev && r.Level <= level {
				if r.Reclaim != nil {
					reclaimed += r.Reclaim()
				}
				if r.Notify != nil {
					r.Notify()
				}
			}
		}
		if level >= LevelRejectBackground {
			m.rejectedBackground.Add(1)
		}
		if level >= LevelDegradeInteractive {
			m.degradedInteractive.Add(1)
		}
	}

	m.level.Store(int32(level))
	if hook != nil {
		hook(prev, level)
	}
}

func (m *MemoryBudgetManager) reclaimablesSnapshot() []Reclaimable {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Reclaimable, len(m.reclaimables))
	copy(out, m.reclaimables)
	return out
}

// sortLocked 按 Level 升序排列 —— 保证降级阶梯顺序不被注册顺序破坏。
func (m *MemoryBudgetManager) sortLocked() {
	rs := m.reclaimables
	for i := 1; i < len(rs); i++ {
		for j := i; j > 0 && rs[j].Level < rs[j-1].Level; j-- {
			rs[j], rs[j-1] = rs[j-1], rs[j]
		}
	}
}

// ShouldAcceptBackground 报告当前是否还接受新的 background 任务。
//
// 档位 ≥ ⑥ 时返回 false —— 这是第 24 章「拒绝新的 background 任务」的判定入口。
func (m *MemoryBudgetManager) ShouldAcceptBackground() bool {
	return m.Level() < LevelRejectBackground
}

// InteractiveBudgetScale 返回 interactive 任务的资源缩放系数。
//
// 档位 < ⑦ 时恒为 1.0（interactive 不受影响）；档位 ⑦ 时降到 0.5 ——
// interactive 是最后被影响的，且只降级不拒绝。
func (m *MemoryBudgetManager) InteractiveBudgetScale() float64 {
	if m.Level() < LevelDegradeInteractive {
		return 1.0
	}
	return 0.5
}

// Snapshot 返回当前压力状态（不触发评估）。
func (m *MemoryBudgetManager) Snapshot() PressureState {
	m.mu.RLock()
	defer m.mu.RUnlock()
	level := DegradationLevel(m.level.Load())
	return PressureState{
		SoftLimitBytes:      m.soft,
		HardLimitBytes:      m.hard,
		EmergencyLimitBytes: m.emergency,
		Level:               level,
		LevelName:           level.String(),
		Timestamp:           m.now(),
	}
}

// Counters 返回拒绝计数，供测试与指标断言。
func (m *MemoryBudgetManager) Counters() (rejectedBackground, degradedInteractive uint64) {
	return m.rejectedBackground.Load(), m.degradedInteractive.Load()
}

// ---------------------------------------------------------------------------
// 便捷回收器：供各模块注册
// ---------------------------------------------------------------------------

// LRUReclaimable 把有界 LRU 包装成可回收资源。
func LRUReclaimable(name string, level DegradationLevel, caches ...*LRU) Reclaimable {
	return Reclaimable{
		Name:  name,
		Level: level,
		Reclaim: func() int64 {
			var freed int64
			for _, c := range caches {
				if c != nil {
					freed += c.Clear()
				}
			}
			return freed
		},
	}
}
