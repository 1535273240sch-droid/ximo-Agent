package worker

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// ============================== Manager ==============================
//
// Manager 管理所有 Worker 的生命周期，并**唯一地**实现第 13 章要求的崩溃恢复链条：
//
//	detect exit → reject in-flight calls → persist worker failure
//	→ restart worker → health check → resume queued calls
//
// 各具体 Worker（browser/terminal/mcp/office/dynamic）只需实现 Worker 接口，
// 就自动获得：心跳、超时、重启退避、熔断、租约回收、队列续跑、进程树清理。
//
// 设计要点：
//   - 一个 kind 对应一个 pool（默认 1 个 slot，browser 默认 4 个，见 BrowserPool）。
//   - slot 是故障域边界：一个 slot 崩溃只影响该 slot 的 in-flight 调用。
//   - 崩溃不会传播到 Engine：Manager 捕获所有 panic 并转成 WorkerResponse。
//   - 排队调用在重启后按 FIFO 续跑（resume queued calls），但受 MaxAttempts 约束，
//     避免"崩溃→重试→再崩溃"的死循环（对齐 I5：不能产生重复副作用）。

// Factory 创建一个新的 Worker 实例。Manager 在启动与重启时调用。
type Factory func(spec Spec) (Worker, error)

// ManagerConfig 配置 Manager。
type ManagerConfig struct {
	// LeaseTTL 是租约默认有效期。<=0 时用 30s。
	LeaseTTL time.Duration
	// HeartbeatInterval 是自动心跳间隔。<=0 时用 LeaseTTL/3。
	HeartbeatInterval time.Duration
	// CallTimeout 是单次调用的默认超时。<=0 时用 60s。
	CallTimeout time.Duration
	// HealthInterval 是主动健康检查间隔。<=0 时用 10s。
	HealthInterval time.Duration
	// MaxAttempts 是崩溃恢复后重派的最大次数（含首次）。<=0 时用 2。
	MaxAttempts int
	// QueueSize 是每 slot 的等待队列长度。<=0 时用 64。
	QueueSize int
	// Backoff 用于重启退避。
	Backoff Backoff
	// Circuit 用于熔断配置。
	Circuit CircuitConfig
	// DrainTimeout 是 Stop 时等待在飞调用结束的时间。<=0 时用 5s。
	DrainTimeout time.Duration
	// FailureWindow 是判断"反复崩溃"的窗口，窗口内崩溃达 FailuresToCircuit 次则熔断。
	FailureWindow time.Duration
	// FailuresToCircuit 见 FailureWindow。<=0 时用 5。
	FailuresToCircuit int
}

func (c ManagerConfig) withDefaults() ManagerConfig {
	if c.LeaseTTL <= 0 {
		c.LeaseTTL = 30 * time.Second
	}
	if c.HeartbeatInterval <= 0 {
		c.HeartbeatInterval = c.LeaseTTL / 3
	}
	if c.CallTimeout <= 0 {
		c.CallTimeout = 60 * time.Second
	}
	if c.HealthInterval <= 0 {
		c.HealthInterval = 10 * time.Second
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 2
	}
	if c.QueueSize <= 0 {
		c.QueueSize = 64
	}
	if c.DrainTimeout <= 0 {
		c.DrainTimeout = 5 * time.Second
	}
	if c.FailureWindow <= 0 {
		c.FailureWindow = 60 * time.Second
	}
	if c.FailuresToCircuit <= 0 {
		c.FailuresToCircuit = 5
	}
	if c.Backoff.Schedule == nil {
		c.Backoff = Backoff{Schedule: DefaultBackoffSchedule, Jitter: 0.1}
	}
	return c
}

// PoolSpec 描述一个 kind 的 Worker 池。
type PoolSpec struct {
	// Kind 是 Worker 类型，例如 "browser"。
	Kind string
	// Size 是池中 Worker 实例数量。<=0 时用 1。
	Size int
	// Factory 创建 Worker 实例，必须非空。
	Factory Factory
	// Priority 用于 Acquire 时在多个 kind 间排序（数值小者优先）。默认 0。
	Priority int
	// Config 是注入给该 kind 每个 Worker 实例的静态配置（见 Spec.Config），
	// 例如 terminal 的 allowed_roots、mcp 的 servers、browser 的 headless。
	//
	// 必须在这里透传：所有高危域 Worker 的权限边界都来自 Spec.Config，而它们
	// 一律 fail-closed。若池不把配置交给 Worker，Worker 只能拿到零值配置，
	// 结果是「池起来了、工具也注册了，但任何一次调用都被策略拒绝」。
	Config map[string]any
}

// Manager 是所有 Worker 的统一生命周期管理者。
type Manager struct {
	cfg       ManagerConfig
	clock     Clock
	log       Logger
	events    EventSink
	metrics   MetricsSink
	failures  FailureStore
	resolver  Resolver
	supervise SupervisorHooks

	mu     sync.RWMutex
	pools  map[string]*pool
	closed bool

	// 全局队列等待与在飞计数（给指标用）。
	inflight atomic.Int64
	queued   atomic.Int64

	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup

	// freeCh 是"slot 可能已可用"的广播通道：任一等待者看到它被关闭就重新扫描。
	// 由 broadcastSlotFreed 负责关闭并替换成新通道。
	freeCh chan struct{}
}

// Resolver 把请求路由到 kind。默认实现直接使用 req.Kind；
// 任务 04 可以注入自定义实现，把工具名（browser_navigate）映射到 kind（browser）。
type Resolver interface {
	ResolveKind(req WorkerRequest) (kind string, err error)
}

// ResolverFunc 适配函数为 Resolver。
type ResolverFunc func(req WorkerRequest) (string, error)

// ResolveKind 实现 Resolver。
func (f ResolverFunc) ResolveKind(req WorkerRequest) (string, error) { return f(req) }

// SlotInfo 是对外暴露的单个 Worker 槽位快照（供任务 04 / 07 / UI 观测）。
type SlotInfo struct {
	WorkerID     string       `json:"worker_id"`
	Kind         string       `json:"kind"`
	Slot         int          `json:"slot"`
	State        State        `json:"state"`
	StateName    string       `json:"state_name"`
	Restarts     int          `json:"restarts"`
	Crashes      int          `json:"crashes"`
	InFlight     int          `json:"in_flight"`
	Queued       int          `json:"queued"`
	Circuit      string       `json:"circuit"`
	LastError    string       `json:"last_error,omitempty"`
	LastExitAt   time.Time    `json:"last_exit_at,omitempty"`
	LastReadyAt  time.Time    `json:"last_ready_at,omitempty"`
	PIDs         []int        `json:"pids,omitempty"`
	LeaseActive  bool         `json:"lease_active"`
	LeaseID      string       `json:"lease_id,omitempty"`
	LeaseExpires time.Time    `json:"lease_expires_at,omitempty"`
	Health       HealthReport `json:"health"`
}

// HealthReport 是单次健康检查结果。
type HealthReport struct {
	OK        bool          `json:"ok"`
	CheckedAt time.Time     `json:"checked_at"`
	Latency   time.Duration `json:"latency"`
	Error     string        `json:"error,omitempty"`
}

// NewManager 创建 Manager。
func NewManager(cfg ManagerConfig, opts ...ManagerOption) *Manager {
	m := &Manager{
		cfg:      cfg.withDefaults(),
		clock:    RealClock{},
		log:      NopLogger{},
		events:   NopEventSink{},
		metrics:  NopMetricsSink{},
		failures: &MemoryFailureStore{},
		resolver: ResolverFunc(func(req WorkerRequest) (string, error) {
			if req.Kind == "" {
				return "", fmt.Errorf("%w: 请求未指定 kind", ErrInvalidArgument)
			}
			return req.Kind, nil
		}),
		pools:  make(map[string]*pool),
		stopCh: make(chan struct{}),
		freeCh: make(chan struct{}),
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

// ManagerOption 定制 Manager 依赖。
type ManagerOption func(*Manager)

// WithClock 注入时钟（测试用）。
func WithClock(c Clock) ManagerOption {
	return func(m *Manager) {
		if c != nil {
			m.clock = c
		}
	}
}

// WithLogger 注入日志。
func WithLogger(l Logger) ManagerOption {
	return func(m *Manager) {
		if l != nil {
			m.log = l
		}
	}
}

// WithEvents 注入事件 sink。
func WithEvents(e EventSink) ManagerOption {
	return func(m *Manager) {
		if e != nil {
			m.events = e
		}
	}
}

// WithMetrics 注入指标 sink。
func WithMetrics(s MetricsSink) ManagerOption {
	return func(m *Manager) {
		if s != nil {
			m.metrics = s
		}
	}
}

// WithFailureStore 注入失败记录存储（任务 03 的落库实现）。
func WithFailureStore(s FailureStore) ManagerOption {
	return func(m *Manager) {
		if s != nil {
			m.failures = s
		}
	}
}

// WithResolver 注入 kind 路由解析器。
func WithResolver(r Resolver) ManagerOption {
	return func(m *Manager) {
		if r != nil {
			m.resolver = r
		}
	}
}

// ============================== pool / slot ==============================

type pool struct {
	kind  string
	spec  PoolSpec
	slots []*slot
	rr    atomic.Uint64
}

type slot struct {
	mgr  *Manager
	kind string
	idx  int
	prio int
	// config 是该 kind 的池级静态配置，创建/重启 Worker 时透传（见 PoolSpec.Config）。
	config map[string]any

	mu       sync.Mutex
	worker   Worker
	state    State
	restarts int
	crashes  int
	inflight int
	lastErr  string
	lastExit time.Time
	lastRdy  time.Time
	health   HealthReport
	// lastFailures 是崩溃时间戳滑窗，用于判断"反复崩溃"。
	lastFailures []time.Time
	// restarting 表示维护循环正在重启该 slot，避免并发重复重启。
	restarting bool
	// retired 表示该 slot 已不可用（熔断打开后停止服务），Acquire 会跳过。
	retired bool
	// reserved 表示该 slot 已被某个 Acquire 选中但尚未挂上租约（占位）。
	// 没有它，并发 Acquire 会同时选中同一 slot 并互相覆盖 lease。
	reserved bool

	lease   *Lease
	circuit *CircuitBreaker

	// pending 是排队等待执行的调用（FIFO）。
	pending []*pendingCall
	// notify 用于等待队列空位或新结果。
	notify chan struct{}
}

type pendingCall struct {
	req      WorkerRequest
	ctx      context.Context
	attempt  int
	enqueued time.Time
	slot     *slot
	// lease 记录入队时所属租约，便于续跑时校验租约仍然有效。
	lease *Lease
	done  chan struct{}
	resp  WorkerResponse
	err   error
}

// addPool 注册一个 Worker 池。
func (m *Manager) addPool(ps PoolSpec) error {
	if ps.Kind == "" {
		return fmt.Errorf("%w: pool kind 不能为空", ErrInvalidArgument)
	}
	if ps.Factory == nil {
		return fmt.Errorf("%w: pool %s 缺少 factory", ErrInvalidArgument, ps.Kind)
	}
	if ps.Size <= 0 {
		ps.Size = 1
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return fmt.Errorf("%w: Manager 已关闭", ErrUnavailable)
	}
	if _, dup := m.pools[ps.Kind]; dup {
		return fmt.Errorf("%w: pool %s 重复注册", ErrInvalidArgument, ps.Kind)
	}
	p := &pool{kind: ps.Kind, spec: ps}
	for i := 0; i < ps.Size; i++ {
		s := &slot{
			mgr:    m,
			kind:   ps.Kind,
			idx:    i,
			prio:   ps.Priority,
			config: ps.Config,
			state:  StateCold,
			notify: make(chan struct{}, 1),
		}
		// 每个 slot 一个独立熔断器：一个 slot 反复失败不会影响同池其他 slot。
		s.circuit = NewCircuitBreaker(m.cfg.Circuit, m.clock, func(from, to CircuitState, reason string) {
			evType := EventCircuitClosed
			switch to {
			case CircuitOpen:
				evType = EventCircuitOpened
			case CircuitHalfOpen:
				evType = EventCircuitHalfOpen
			}
			m.emit(WorkerEvent{
				Type: evType, Kind: ps.Kind, Slot: i, Reason: reason,
			})
		})
		p.slots = append(p.slots, s)
	}
	m.pools[ps.Kind] = p
	return nil
}

// RegisterPool 注册一个 Worker 池（对外 API）。
func (m *Manager) RegisterPool(ps PoolSpec) error { return m.addPool(ps) }

// Start 启动 Manager：拉活所有 slot 并启动维护循环。
// 单个 Worker 启动失败不会让整个 Manager 失败（故障隔离）——失败的 slot 进入退避重启。
func (m *Manager) Start(ctx context.Context) error {
	m.mu.RLock()
	pools := make([]*pool, 0, len(m.pools))
	for _, p := range m.pools {
		pools = append(pools, p)
	}
	m.mu.RUnlock()

	if len(pools) == 0 {
		return fmt.Errorf("%w: 未注册任何 Worker 池", ErrInvalidArgument)
	}

	for _, p := range pools {
		for _, s := range p.slots {
			// 逐个启动，失败也不阻断其他 slot（这是故障域隔离的关键）。
			if err := m.startSlot(ctx, s); err != nil {
				m.log.Error("worker 首次启动失败，进入退避重启",
					"kind", s.kind, "slot", s.idx, "err", err.Error())
			}
		}
	}

	m.wg.Add(1)
	go m.maintenanceLoop()
	return nil
}

// maintenanceLoop 是唯一的后台循环，负责：
//   - 租约超时检测 → force cleanup → recycle
//   - 主动健康检查 → 失败则走崩溃恢复链条
//   - 退避重启待重启的 slot
//
// 只有这一个 ticker，且 defer Stop（验收标准：所有 timer/ticker 都有 Stop）。
func (m *Manager) maintenanceLoop() {
	defer m.wg.Done()
	interval := m.cfg.HealthInterval
	if interval > time.Second {
		interval = time.Second // 维护粒度至少 1s，保证租约超时能及时发现
	}
	tk := m.clock.NewTicker(interval)
	defer tk.Stop()
	for {
		select {
		case <-m.stopCh:
			return
		case <-tk.C():
			m.sweep()
		}
	}
}

// sweep 执行一轮维护：租约超时、健康检查、待重启 slot 拉起。
func (m *Manager) sweep() {
	m.mu.RLock()
	pools := make([]*pool, 0, len(m.pools))
	for _, p := range m.pools {
		pools = append(pools, p)
	}
	m.mu.RUnlock()

	for _, p := range pools {
		for _, s := range p.slots {
			m.sweepSlot(s)
		}
	}
}

func (m *Manager) sweepSlot(s *slot) {
	// 1) 租约超时 → 强制回收。
	s.mu.Lock()
	lease := s.lease
	expired := lease != nil && lease.IsExpiredAt(m.clock.Now())
	draining := false
	var drainReason string
	if dw, ok := s.worker.(DrainingWorker); ok && s.state == StateReady {
		if r := dw.NeedRecycle(); r != "" {
			draining = true
			drainReason = r
		}
	}
	s.mu.Unlock()

	if expired {
		m.log.Warn("租约超时，强制回收 worker",
			"kind", s.kind, "slot", s.idx, "lease", lease.ID())
		m.metrics.IncCounter(MetricLeaseExpired, s.kind, 1)
		m.emit(WorkerEvent{Type: EventLeaseExpired, Kind: s.kind, Slot: s.idx, Reason: "租约超时"})
		// 链条：force cleanup（取消在飞调用 + 杀外部进程）→ reject → recycle → restart。
		m.recycleSlot(context.Background(), s, "租约超时", "")
		return
	}

	if draining {
		m.log.Warn("worker 自报需要回收", "kind", s.kind, "slot", s.idx, "reason", drainReason)
		m.emit(WorkerEvent{Type: EventWorkerRecycled, Kind: s.kind, Slot: s.idx, Reason: drainReason})
		m.recycleSlot(context.Background(), s, drainReason, "")
		return
	}

	// 2) 待重启/未就绪的 slot 按退避拉起。
	s.mu.Lock()
	needStart := s.worker == nil || s.state == StateCold || s.state == StateRestarting
	if s.state == StateCircuitOpen {
		// 熔断打开：不重启，等冷却。冷却结束后 circuit 会自行迁到 half-open，
		// 此时清掉 retired 并安排一次重启尝试（half-open 探测的具体执行者）。
		if s.circuit != nil && s.circuit.State() != CircuitOpen {
			s.state = StateRestarting
			s.retired = false
			s.lastFailures = nil // 冷却期给一次干净的观察窗口
			needStart = true
		} else {
			needStart = false
		}
	}
	// restarts 记录的是"已失败次数"，第一次重试应等待退避表的第 0 项（1s），
	// 因此用 Delay(restarts-1)；restarts==0（从未失败）时 Delay 会把负值夹到 0，
	// 而冷启动的 lastExit 是零值，waited 必然已满足，不会造成额外等待。
	backoff := m.cfg.Backoff.Delay(s.restarts - 1)
	waited := m.clock.Now().Sub(s.lastExit)
	s.mu.Unlock()

	if needStart && waited >= backoff {
		if err := m.startSlot(context.Background(), s); err != nil {
			m.log.Warn("worker 重启失败", "kind", s.kind, "slot", s.idx, "err", err.Error())
		}
		return
	}

	// 3) 就绪 slot 的主动健康检查。这一步是"detect exit"的主要来源：
	//    Worker 的外部进程被 kill 后，Health 会失败，从而触发恢复链条。
	s.mu.Lock()
	ready := s.state == StateReady && s.worker != nil
	w := s.worker
	s.mu.Unlock()
	if !ready {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	err := m.safeHealth(ctx, w)
	cancel()
	now := m.clock.Now()
	s.mu.Lock()
	if err != nil {
		s.lastErr = err.Error()
	} else {
		s.health = HealthReport{OK: true, CheckedAt: now}
	}
	s.mu.Unlock()

	if err != nil {
		m.log.Warn("健康检查失败，按崩溃处理", "kind", s.kind, "slot", s.idx, "err", err.Error())
		m.handleWorkerExit(context.Background(), s, fmt.Errorf("%w: 健康检查失败: %v", ErrCrash, err), "")
	}
}

// ============================== 启动 / 崩溃恢复链条 ==============================

// startSlot 拉起（或重启）一个 slot 的 Worker。
func (m *Manager) startSlot(ctx context.Context, s *slot) error {
	s.mu.Lock()
	if s.restarting {
		s.mu.Unlock()
		return nil
	}
	s.restarting = true
	s.state = StateStarting
	s.mu.Unlock()

	// restarting 标志必须在所有返回路径上被清掉，否则该 slot 会永久卡死。
	defer func() {
		s.mu.Lock()
		s.restarting = false
		s.mu.Unlock()
	}()

	spec := Spec{ID: fmt.Sprintf("%s-%d", s.kind, s.idx), Kind: s.kind, Config: s.config}
	w, ferr := s.mgr.newWorker(spec, s)
	if ferr != nil {
		// 启动失败也要走统一的失败记账：否则"启动即失败"会变成无限快速重试，
		// 既没有递增退避，也永远不会触发熔断（这是实测发现的缺口）。
		return m.handleStartFailure(ctx, s, ferr)
	}

	startCtx, cancel := context.WithTimeout(ctx, m.cfg.CallTimeout)
	defer cancel()
	if serr := m.safeStart(startCtx, w); serr != nil {
		m.stopWorkerQuietly(w)
		return m.handleStartFailure(ctx, s, fmt.Errorf("worker 启动失败: %w", serr))
	}

	// 健康检查通过才算 ready（第 13 章恢复链条的 health check 步）。
	hcCtx, hcancel := context.WithTimeout(ctx, 5*time.Second)
	herr := m.safeHealth(hcCtx, w)
	hcancel()
	if herr != nil {
		m.stopWorkerQuietly(w)
		return m.handleStartFailure(ctx, s, fmt.Errorf("worker 启动后健康检查失败: %w", herr))
	}

	now := m.clock.Now()
	s.mu.Lock()
	s.worker = w
	s.state = StateReady
	s.lastRdy = now
	s.lastErr = ""
	s.health = HealthReport{OK: true, CheckedAt: now}
	s.retired = false
	// 启动成功即清零失败窗口：否则历史崩溃会在窗口内累积，
	// 让一个已经恢复健康的 slot 被冤枉地熔断。
	s.lastFailures = nil
	s.mu.Unlock()

	if s.circuit != nil {
		s.circuit.Reset()
	}

	m.metrics.SetGauge(MetricReady, s.kind, w.ID(), 1)
	m.emit(WorkerEvent{Type: EventWorkerReady, WorkerID: w.ID(), Kind: s.kind, Slot: s.idx})

	// 恢复链条最后一步：resume queued calls。
	go m.resumeQueued(s)
	// 有新 slot 就绪：唤醒正在等待空闲 worker 的调用方。
	m.broadcastSlotFreed()
	return nil
}

// handleStartFailure 统一处理"拉起/健康检查失败"。
//
// 它必须和 handleWorkerExit 用同一套失败记账，否则会出现两个漏洞：
//   - 退避不递增 → 启动即失败时变成无间隔快速重试风暴；
//   - 从不触发熔断 → 一个永远起不来的依赖（如浏览器未安装）会被无限重试。
//
// 失败窗口内的计数达到阈值时，直接打开熔断、停止重试，等冷却后再试一次。
func (m *Manager) handleStartFailure(ctx context.Context, s *slot, cause error) error {
	now := m.clock.Now()

	s.mu.Lock()
	s.state = StateRestarting
	s.restarts++
	s.lastExit = now
	s.lastErr = errString(cause)
	s.lastFailures = append(s.lastFailures, now)
	cutoff := now.Add(-m.cfg.FailureWindow)
	kept := s.lastFailures[:0]
	for _, ts := range s.lastFailures {
		if ts.After(cutoff) {
			kept = append(kept, ts)
		}
	}
	s.lastFailures = kept
	tooMany := len(s.lastFailures) >= m.cfg.FailuresToCircuit
	attempt := s.restarts
	s.mu.Unlock()

	// 记入失败存储（与崩溃路径一致，便于事后审计"为什么这个 worker 起不来"）。
	_ = m.failures.PersistFailure(ctx, WorkerFailure{
		WorkerID:  fmt.Sprintf("%s-%d", s.kind, s.idx),
		Kind:      s.kind,
		ErrorCode: WorkerErrorFromError(cause).Code,
		Message:   errString(cause),
		Attempt:   attempt,
		At:        now,
	})

	if tooMany {
		if s.circuit != nil {
			s.circuit.ForceOpen("worker 反复启动失败")
		}
		s.mu.Lock()
		s.retired = true
		s.state = StateCircuitOpen
		s.mu.Unlock()
		m.metrics.SetGauge(MetricCircuitOpen, s.kind, fmt.Sprintf("%s-%d", s.kind, s.idx), 1)
		// 事件由熔断器的状态迁移回调统一发出，避免同一迁移发出两条事件。
		m.log.Error("worker 反复启动失败，熔断打开",
			"kind", s.kind, "slot", s.idx, "failures", len(s.lastFailures))
		return cause
	}

	m.metrics.IncCounter(MetricRestarts, s.kind, 1)
	m.log.Warn("worker 启动失败，按退避重试",
		"kind", s.kind, "slot", s.idx, "attempt", attempt,
		"delay", m.cfg.Backoff.Delay(attempt-1).String())
	return cause
}

// newWorker 调用 Factory，并捕获 Factory 自身的 panic（第三方库初始化可能炸）。
func (m *Manager) newWorker(spec Spec, s *slot) (w Worker, err error) {
	p, ok := m.poolOf(spec.Kind)
	if !ok {
		return nil, fmt.Errorf("%w: 未注册的 kind=%s", ErrUnavailable, spec.Kind)
	}
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%w: factory panic: %v", ErrInternal, r)
		}
	}()
	w, err = p.spec.Factory(spec)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInternal, err)
	}
	if w == nil {
		return nil, fmt.Errorf("%w: factory 返回 nil", ErrInternal)
	}
	return w, nil
}

func (m *Manager) poolOf(kind string) (*pool, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.pools[kind]
	return p, ok
}

// safeStart 调用 Worker.Start 并捕获 panic，转为 error（panic 绝不外溢到 Engine）。
func (m *Manager) safeStart(ctx context.Context, w Worker) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%w: Start panic: %v", ErrInternal, r)
		}
	}()
	return w.Start(ctx)
}

// safeHealth 调用 Worker.Health 并捕获 panic。
func (m *Manager) safeHealth(ctx context.Context, w Worker) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%w: Health panic: %v", ErrInternal, r)
		}
	}()
	return w.Health(ctx)
}

// safeExecute 调用 Worker.Execute 并捕获 panic，把 panic 转成 crash 响应。
func (m *Manager) safeExecute(ctx context.Context, w Worker, req WorkerRequest) (resp WorkerResponse, err error) {
	defer func() {
		if r := recover(); r != nil {
			resp = ErrorResponse(req, w.ID(), CodeCrash, fmt.Sprintf("worker panic: %v", r))
			err = fmt.Errorf("%w: Execute panic: %v", ErrCrash, r)
		}
	}()
	return w.Execute(ctx, req)
}

// stopWorkerQuietly 尽力停止一个 Worker，忽略错误（仅用于清理路径）。
func (m *Manager) stopWorkerQuietly(w Worker) {
	if w == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), m.cfg.DrainTimeout)
	defer cancel()
	defer func() { _ = recover() }()
	_ = w.Stop(ctx)
}

// handleWorkerExit 实现第 13 章的崩溃恢复链条。这是整个 Manager 的核心路径。
//
//	detect exit → reject in-flight calls → persist worker failure
//	→ restart worker → health check → resume queued calls
func (m *Manager) handleWorkerExit(ctx context.Context, s *slot, cause error, callID string) {
	s.mu.Lock()
	if s.state == StateStopped || s.retired {
		// 已停机/已熔断的 slot 不再重复处理，保持崩溃计数稳定。
		s.mu.Unlock()
		return
	}
	oldWorker := s.worker
	s.worker = nil
	s.state = StateRestarting
	s.crashes++
	s.restarts++
	s.lastExit = m.clock.Now()
	if cause != nil {
		s.lastErr = cause.Error()
	}
	// 记录崩溃时间窗。
	s.lastFailures = append(s.lastFailures, m.clock.Now())
	cutoff := m.clock.Now().Add(-m.cfg.FailureWindow)
	kept := s.lastFailures[:0]
	for _, ts := range s.lastFailures {
		if ts.After(cutoff) {
			kept = append(kept, ts)
		}
	}
	s.lastFailures = kept
	tooMany := len(s.lastFailures) >= m.cfg.FailuresToCircuit
	restarts := s.restarts
	workerID := ""
	if oldWorker != nil {
		workerID = oldWorker.ID()
	}

	// 取走排队中的调用（第 2 步：reject/requeue in-flight calls）。
	pending := s.pending
	s.pending = nil
	lease := s.lease
	s.mu.Unlock()

	m.metrics.SetGauge(MetricReady, s.kind, workerID, 0)
	m.metrics.IncCounter(MetricCrashes, s.kind, 1)
	m.emit(WorkerEvent{
		Type: EventWorkerCrashed, WorkerID: workerID, Kind: s.kind, Slot: s.idx,
		CallID: callID, Attempt: restarts,
		Reason: errString(cause),
	})

	// --- 步骤 1：persist worker failure（第 21 章：只记录可审计信息，不含 payload） ---
	code := CodeCrash
	if cause != nil {
		code = WorkerErrorFromError(cause).Code
	}
	_ = m.failures.PersistFailure(ctx, WorkerFailure{
		WorkerID: workerID, Kind: s.kind, CallID: callID,
		ErrorCode: code, Message: errString(cause), Attempt: restarts,
		At: m.clock.Now(),
	})

	// --- 步骤 2：拒绝在飞调用（in-flight 的租约立即失效，调用方拿到 crash 错误） ---
	if lease != nil {
		lease.mu.Lock()
		lease.expired = true
		cancel := lease.cancelExec
		lease.mu.Unlock()
		if cancel != nil {
			cancel() // 中断正在执行的调用，让它尽快返回
		}
		m.releaseLease(ctx, lease)
	}

	// --- 步骤 3：清理旧 Worker 及其外部进程树（process-tree cleanup 红线） ---
	if oldWorker != nil {
		m.cleanupProcessTree(oldWorker)
		m.stopWorkerQuietly(oldWorker)
	}

	// --- 步骤 4：排队调用处理。可安全重试的重新入队，不可重试的直接失败 ---
	for _, pc := range pending {
		if pc.attempt+1 < m.cfg.MaxAttempts && retryableCall(pc, cause) {
			pc.attempt++
			s.mu.Lock()
			full := len(s.pending) >= m.cfg.QueueSize
			if !full {
				s.pending = append(s.pending, pc)
			}
			s.mu.Unlock()
			if full {
				pc.fail(WorkerResponse{}, fmt.Errorf("%w: 队列已满，无法续跑", ErrUnavailable))
			} else {
				m.metrics.IncCounter(MetricQueueWait, s.kind, 0)
				m.emit(WorkerEvent{
					Type: EventCallResumed, WorkerID: workerID, Kind: s.kind, Slot: s.idx,
					CallID: pc.req.CallID, RunID: pc.req.RunID, Attempt: pc.attempt,
				})
			}
			continue
		}
		pc.fail(ErrorResponse(pc.req, workerID, CodeCrash, errString(cause)), fmt.Errorf("%w: %s", ErrCrash, errString(cause)))
	}

	// --- 步骤 5：熔断判断。短窗口内反复崩溃 → 打开熔断，停止打无意义的重启风暴 ---
	if tooMany {
		s.mu.Lock()
		s.retired = true
		s.state = StateCircuitOpen
		s.mu.Unlock()
		if s.circuit != nil {
			s.circuit.ForceOpen("worker 反复崩溃")
		}
		m.metrics.SetGauge(MetricCircuitOpen, s.kind, workerID, 1)
		m.emit(WorkerEvent{
			Type: EventCircuitOpened, WorkerID: workerID, Kind: s.kind, Slot: s.idx,
			Reason: fmt.Sprintf("%s 内崩溃 %d 次", m.cfg.FailureWindow, len(s.lastFailures)),
		})
		m.log.Error("worker 反复崩溃，熔断打开",
			"kind", s.kind, "slot", s.idx, "crashes", len(s.lastFailures))
		return
	}

	// --- 步骤 6：退避重启 + health check + resume queued calls 由 sweep/startSlot 完成 ---
	delay := m.cfg.Backoff.Delay(restarts - 1)
	m.log.Warn("worker 已崩溃，安排退避重启",
		"kind", s.kind, "slot", s.idx, "attempt", restarts, "delay", delay.String())
	m.emit(WorkerEvent{
		Type: EventWorkerRestarted, WorkerID: workerID, Kind: s.kind, Slot: s.idx,
		Attempt: restarts, Duration: int64(delay / time.Millisecond),
	})
	m.metrics.IncCounter(MetricRestarts, s.kind, 1)
}

// retryableCall 判断一次因崩溃而中断的调用能否安全重派。
//
// 这是 I5 不变量的第一道防线：只有"幂等/可检测幂等"的调用才允许自动重派。
// 判定依据来自任务 04 的幂等性分类，通过 WorkerRequest.IdempotencyKey 之外的
// Action 前缀约定传递；未标注的调用一律**不重试**（保守优先，宁可失败让人复核）。
func retryableCall(pc *pendingCall, cause error) bool {
	if pc.req.IdempotencyKey != "" && !idempotentAction(pc.req.Action) {
		// 有幂等键但动作非幂等：不自动重试（对齐任务 04 的 C 类非幂等规则）。
		return false
	}
	return idempotentAction(pc.req.Action)
}

// idempotentActions 是允许崩溃后自动重派的动作白名单。
// 与任务 04 的 A 类（幂等）/B 类（可检测幂等）对应；C 类非幂等动作**不在其中**。
var idempotentActions = map[string]bool{
	// browser：读取型操作，重放无副作用
	"navigate": true, "screenshot": true, "get_content": true,
	"snapshot": true, "network_monitor": true, "observe": true,
	// terminal：查询型
	"get_state": true, "which": true,
	// mcp：列出类
	"tools/list": true, "resources/list": true, "prompts/list": true,
	// office：只读
	"get": true, "query": true, "validate": true, "view": true, "dump": true,
	// dynamic：纯计算（沙箱内无副作用）
	"eval": true,
}

// idempotentAction 判断动作是否属于可自动重派的幂等类。
func idempotentAction(action string) bool { return idempotentActions[action] }

// cleanupProcessTree 强制清理 Worker 遗留的外部进程树。
//
// 为什么单列一步：Worker 崩溃（尤其进程被 kill -9）时，它 spawn 的浏览器/officecli/
// MCP server 会变成孤儿进程。杀父进程不等于杀子进程，这是本项目最容易漏的坑，
// 所以放在恢复链条里作为独立、必过的一步，并复用 Worker 自己实现的 ProcessAware。
func (m *Manager) cleanupProcessTree(w Worker) {
	pa, ok := w.(ProcessAware)
	if !ok {
		return
	}
	pids := pa.PIDs()
	if len(pids) == 0 {
		return
	}
	m.log.Warn("清理 worker 遗留进程树", "worker", w.ID(), "pids", pids)
	m.metrics.IncCounter("worker_orphan_processes", w.Kind(), int64(len(pids)))
	// 各 Worker 的 Stop 已经负责杀自己的进程树（procguard）；这里额外做一次兜底，
	// 保证即使 Stop 因 panic 未执行，也不会留下孤儿。
	for _, pid := range pids {
		killProcessTree(pid)
	}
}

// resumeQueued 在 slot 重新就绪后，把排队调用按 FIFO 续跑（恢复链条最后一步）。
func (m *Manager) resumeQueued(s *slot) {
	for {
		s.mu.Lock()
		if s.state != StateReady || s.worker == nil || len(s.pending) == 0 {
			s.mu.Unlock()
			return
		}
		pc := s.pending[0]
		s.pending = s.pending[1:]
		s.mu.Unlock()

		m.queued.Add(-1)
		resp, err := m.dispatch(s, pc)
		pc.finish(resp, err)
	}
}

// ============================== Acquire / Execute ==============================

// Acquire 获取一个可用 Worker 的租约（第 16 章租约模型的第一步）。
//
// 语义：
//   - 多个 kind 间按 Priority 排序，同 kind 内轮询（round-robin），避免热点集中。
//   - 若所有 slot 都忙，按 FIFO 返回等待中的租约（队列模型），或按 ctx 超时失败。
//   - 若 kind 未注册 → ErrUnavailable；若池已熔断且无可用 slot → ErrUnavailable。
func (m *Manager) Acquire(ctx context.Context, kind string, opts ...LeaseOption) (*Lease, error) {
	if m.isClosed() {
		return nil, fmt.Errorf("%w: Manager 已关闭", ErrUnavailable)
	}
	p, ok := m.poolOf(kind)
	if !ok {
		return nil, fmt.Errorf("%w: 未注册的 kind=%s", ErrUnavailable, kind)
	}

	lo := leaseOptions{ttl: m.cfg.LeaseTTL, autoHeartbeat: true, hbInterval: m.cfg.HeartbeatInterval}
	for _, o := range opts {
		o(&lo)
	}
	if lo.ttl <= 0 {
		lo.ttl = m.cfg.LeaseTTL
	}
	if lo.hbInterval <= 0 {
		lo.hbInterval = lo.ttl / 3
	}

	slot, err := m.pickSlot(ctx, p)
	if err != nil {
		return nil, err
	}

	lease := &Lease{
		id:            fmt.Sprintf("%s-%d-%d", kind, slot.idx, leaseIDSeq.Add(1)),
		workerID:      workerIDOf(slot),
		kind:          kind,
		slotIdx:       slot.idx,
		mgr:           m,
		clock:         m.clock,
		ttl:           lo.ttl,
		lastHeartbeat: m.clock.Now(),
	}
	lease.expiresAt = lease.lastHeartbeat.Add(lo.ttl)

	slot.mu.Lock()
	if slot.state != StateReady || slot.worker == nil {
		slot.reserved = false // 归还占位，别让 slot 永久卡住
		state := slot.state
		slot.mu.Unlock()
		m.broadcastSlotFreed()
		return nil, fmt.Errorf("%w: slot 未就绪 (%s)", ErrUnavailable, state)
	}
	slot.reserved = false // 占位使命完成：真正的互斥由 lease 承担
	slot.lease = lease
	slot.mu.Unlock()

	m.metrics.SetGauge(MetricInFlight, kind, lease.workerID, float64(m.inflight.Load()))
	m.emit(WorkerEvent{
		Type: EventLeaseAcquired, WorkerID: lease.workerID, Kind: kind,
		Slot: slot.idx, Reason: lease.id,
	})

	if lo.autoHeartbeat {
		lease.startAutoHeartbeat(lo.hbInterval)
	}
	return lease, nil
}

// pickSlot 选一个就绪 slot；都忙时等待"某 slot 变为可用"的通知。
//
// 这里刻意**不用轮询**（早期实现用时钟 ticker 轮询，在注入可控时钟的单测里
// 永远不会被唤醒，导致等待方永久阻塞）。改用广播通知 + 兜底超时。
//
// 关键顺序：**必须先注册等待信号、再扫描槽位**。反过来写会出现经典的
// lost-wakeup——若 slot 恰好在"扫描失败"与"注册"之间被释放，那次广播就丢了，
// 调用方会一直阻塞到兜底超时（实测在并发用例里踩到）。
func (m *Manager) pickSlot(ctx context.Context, p *pool) (*slot, error) {
	// 兜底超时用**真实时钟**而不是注入时钟：注入时钟（ManualClock）只有显式
	// Advance 才推进，用它做保护性超时等于没有超时。保护性超时属于进程自身的
	// 安全网，不属于被测业务逻辑，因此不参与时钟注入。
	timeout := time.After(30 * time.Second)

	for {
		n := len(p.slots)
		if n == 0 {
			return nil, fmt.Errorf("%w: pool %s 为空", ErrUnavailable, p.kind)
		}

		// 1) 先拿到"下一个空闲通知"通道（可能已处于已关闭状态，表示刚发生过释放）。
		sig := m.slotFreedSignal()

		// 2) 再扫描，尽量直接命中。命中时**原子占位**（reserved=true）：
		//    否则两个等待者可能同时看到同一空闲 slot，随后各自覆盖 slot.lease，
		//    造成租约丢失（心跳失效、释放不生效）——这是并发下的真实竞态。
		start := int(p.rr.Add(1) % uint64(n))
		retiredAll := true
		for i := 0; i < n; i++ {
			s := p.slots[(start+i)%n]
			s.mu.Lock()
			if !s.retired {
				retiredAll = false
			}
			usable := !s.retired && !s.reserved && s.state == StateReady && s.worker != nil && s.lease == nil
			if usable {
				s.reserved = true
				s.mu.Unlock()
				return s, nil
			}
			s.mu.Unlock()
		}
		if retiredAll {
			return nil, fmt.Errorf("%w: pool %s 整体熔断中", ErrUnavailable, p.kind)
		}

		// 3) 等待释放/就绪通知（若上一步已被关闭，这里会立即返回并重新扫描）。
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%w: 等待空闲 worker 超时 (kind=%s): %v", ErrTimeout, p.kind, ctx.Err())
		case <-m.stopCh:
			return nil, fmt.Errorf("%w: Manager 已停止", ErrUnavailable)
		case <-timeout:
			return nil, fmt.Errorf("%w: 等待空闲 worker 超过 30s (kind=%s)", ErrTimeout, p.kind)
		case <-sig:
			// 有 slot 释放/就绪，重新扫描。
		}
	}
}

// slotFreedSignal 返回当前的"slot 可用"广播通道。
// 调用方等待该通道被关闭即表示应重新扫描槽位。
func (m *Manager) slotFreedSignal() chan struct{} {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.freeCh
}

// broadcastSlotFreed 通知所有等待者"某 slot 可能已可用"。
//
// 实现方式：关闭当前广播通道并换一个新的。与 sync.Cond 相比，
// 这里的等待方还需要同时监听 ctx/stopCh，broadcast-channel 更易组合。
func (m *Manager) broadcastSlotFreed() {
	m.mu.Lock()
	if m.freeCh != nil {
		close(m.freeCh)
	}
	m.freeCh = make(chan struct{})
	m.mu.Unlock()
}

// Execute 是对任务 04 的主要入口：一次调用 = 自动拿租约 + 执行 + 释放。
// 长会话场景（browser 多步操作）应改用 Acquire 显式持有租约。
func (m *Manager) Execute(ctx context.Context, req WorkerRequest) (WorkerResponse, error) {
	kind, err := m.resolver.ResolveKind(req)
	if err != nil {
		return WorkerResponse{}, err
	}
	req.Kind = kind
	if err := ValidateRequest(req); err != nil {
		return ErrorResponse(req, "", CodeInvalidArgument, err.Error()), err
	}
	lease, err := m.Acquire(ctx, kind)
	if err != nil {
		return ErrorResponse(req, "", CodeUnavailable, err.Error()), err
	}
	defer func() { _ = lease.Release(context.Background()) }()
	req.LeaseID = lease.ID()
	return lease.Execute(ctx, req)
}

// executeOnSlot 在租约已持有的 slot 上执行一次调用。
func (m *Manager) executeOnSlot(ctx context.Context, lease *Lease, req WorkerRequest) (WorkerResponse, error) {
	p, ok := m.poolOf(lease.kind)
	if !ok {
		return WorkerResponse{}, fmt.Errorf("%w: kind=%s", ErrUnavailable, lease.kind)
	}
	if lease.slotIdx < 0 || lease.slotIdx >= len(p.slots) {
		return WorkerResponse{}, fmt.Errorf("%w: slot=%d", ErrInternal, lease.slotIdx)
	}
	s := p.slots[lease.slotIdx]

	req.Kind = lease.kind
	req.LeaseID = lease.ID()
	if req.Timeout <= 0 {
		req.Timeout = m.cfg.CallTimeout
	}
	if err := ValidateRequest(req); err != nil {
		return ErrorResponse(req, lease.workerID, CodeInvalidArgument, err.Error()), err
	}

	// 熔断检查：打开时快速失败，不打外部依赖（第 17 章）。
	if s.circuit != nil && !s.circuit.Allow() {
		m.emit(WorkerEvent{
			Type: EventCallRejected, WorkerID: lease.workerID, Kind: lease.kind,
			Slot: s.idx, CallID: req.CallID, Reason: "circuit open",
		})
		return ErrorResponse(req, lease.workerID, CodeUnavailable, "worker 熔断中，快速失败"),
			fmt.Errorf("%w: circuit open", ErrUnavailable)
	}

	pc := &pendingCall{
		req: req, ctx: ctx, attempt: 1, enqueued: m.clock.Now(),
		slot: s, lease: lease, done: make(chan struct{}),
	}
	return m.dispatch(s, pc)
}

// dispatch 在 slot 上真正执行调用（可能先排队）。
//
// 排队策略：当前 slot 已有在飞调用时，调用进入 FIFO 队列（不并发打同一个 Worker），
// 这样"一个 Worker 同一时刻只服务一个调用"成为结构性保证——正是 v1 全局单例 page
// 被并发工具调用互相干扰的反面。
func (m *Manager) dispatch(s *slot, pc *pendingCall) (WorkerResponse, error) {
	s.mu.Lock()
	busy := s.inflight > 0
	w := s.worker
	ready := s.state == StateReady && w != nil
	if !ready {
		s.mu.Unlock()
		return WorkerResponse{}, fmt.Errorf("%w: slot 不可用 (%s)", ErrUnavailable, s.state)
	}
	if busy {
		if len(s.pending) >= m.cfg.QueueSize {
			s.mu.Unlock()
			return WorkerResponse{}, fmt.Errorf("%w: 队列已满", ErrUnavailable)
		}
		s.pending = append(s.pending, pc)
		s.mu.Unlock()
		m.queued.Add(1)
		m.metrics.IncCounter(MetricQueued, s.kind, 1)
		select {
		case <-pc.done:
			return pc.resp, pc.err
		case <-pc.ctx.Done():
			// 调用方取消：把它从队列里摘掉，避免重启后又被"复活"。
			s.mu.Lock()
			for i, q := range s.pending {
				if q == pc {
					s.pending = append(s.pending[:i], s.pending[i+1:]...)
					break
				}
			}
			s.mu.Unlock()
			m.queued.Add(-1)
			return ErrorResponse(pc.req, workerIDOf(s), CodeCanceled, "调用方取消"),
				fmt.Errorf("%w: %v", ErrCanceled, pc.ctx.Err())
		}
	}
	s.inflight++
	s.mu.Unlock()
	m.inflight.Add(1)
	m.metrics.SetGauge(MetricInFlight, s.kind, w.ID(), float64(s.inflight))

	defer func() {
		s.mu.Lock()
		s.inflight--
		if s.inflight < 0 {
			s.inflight = 0
		}
		s.mu.Unlock()
		m.inflight.Add(-1)
	}()

	execCtx, cancel := context.WithTimeout(pc.ctx, pc.req.Timeout)
	defer cancel()

	started := m.clock.Now()
	m.metrics.ObserveQueueWait(s.kind, started.Sub(pc.enqueued))

	resp, err := m.safeExecute(execCtx, w, pc.req)
	dur := m.clock.Now().Sub(started)

	// 把 timeout/cancel 归一化成明确的错误码，避免上层看到的错误五花八门。
	if err != nil && resp.Error == nil {
		code := CodeInternal
		switch {
		case errors.Is(execCtx.Err(), context.DeadlineExceeded), errors.Is(err, ErrTimeout):
			code = CodeTimeout
		case errors.Is(execCtx.Err(), context.Canceled), errors.Is(err, ErrCanceled):
			code = CodeCanceled
		case errors.Is(err, ErrCrash):
			code = CodeCrash
		}
		resp = ErrorResponse(pc.req, w.ID(), code, err.Error())
	}
	resp.Metrics.Attempts = pc.attempt

	status := string(resp.Status)
	m.metrics.ObserveCallDuration(s.kind, w.ID(), status, dur)
	if s.circuit != nil {
		if resp.OK() {
			s.circuit.RecordSuccess()
		} else if resp.Status == StatusCrash || resp.Status == StatusTimeout {
			s.circuit.RecordFailure(string(resp.Status))
		}
	}

	// 崩溃类结果触发完整恢复链条（detect exit → ... → resume queued）。
	if resp.Status == StatusCrash || errors.Is(err, ErrCrash) {
		m.handleWorkerExit(context.Background(), s, fmt.Errorf("%w: %s", ErrCrash, errString(err)), pc.req.CallID)
	}
	return resp, err
}

// heartbeatLease 续约。租约已被回收/释放时返回错误。
func (m *Manager) heartbeatLease(l *Lease) error {
	if l == nil {
		return fmt.Errorf("%w: nil 租约", ErrInternal)
	}
	p, ok := m.poolOf(l.kind)
	if !ok || l.slotIdx < 0 || l.slotIdx >= len(p.slots) {
		return fmt.Errorf("%w: 租约指向不存在的 slot", ErrInternal)
	}
	s := p.slots[l.slotIdx]

	now := m.clock.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released {
		return fmt.Errorf("%w: %s", ErrLeaseReleased, l.id)
	}
	if l.expired {
		return fmt.Errorf("%w: %s", ErrLeaseExpired, l.id)
	}
	// 双重校验：slot 必须仍然把该租约视为当前持有者。
	s.mu.Lock()
	cur := s.lease
	s.mu.Unlock()
	if cur != l {
		l.expired = true
		return fmt.Errorf("%w: %s 已被回收", ErrLeaseExpired, l.id)
	}
	l.lastHeartbeat = now
	l.expiresAt = now.Add(l.ttl)
	m.metrics.SetGauge(MetricLeaseExpired, l.kind, l.workerID, 0)
	return nil
}

// releaseLease 释放租约并解除 slot 占用。
func (m *Manager) releaseLease(_ context.Context, l *Lease) error {
	if l == nil {
		return nil
	}
	p, ok := m.poolOf(l.kind)
	if !ok || l.slotIdx < 0 || l.slotIdx >= len(p.slots) {
		return nil
	}
	s := p.slots[l.slotIdx]
	s.mu.Lock()
	if s.lease == l {
		s.lease = nil
	}
	s.mu.Unlock()
	m.emit(WorkerEvent{
		Type: EventLeaseReleased, WorkerID: l.workerID, Kind: l.kind,
		Slot: l.slotIdx, Reason: l.id,
	})
	// 唤醒可能在 pickSlot 里等待空闲 slot 的调用方。
	m.broadcastSlotFreed()
	return nil
}

// recycleSlot 强制回收一个 slot：取消在飞调用 → 清理进程树 → 重新拉起。
//
// 这是"浏览器内存异常时只回收该 worker，不能让整个 Agent Engine OOM"的结构性保证：
// 回收只作用于单个 slot，其他 slot 与 Manager 完全不受影响。
func (m *Manager) recycleSlot(ctx context.Context, s *slot, reason, callID string) {
	s.mu.Lock()
	if s.state == StateStopped {
		s.mu.Unlock()
		return
	}
	lease := s.lease
	oldWorker := s.worker
	s.mu.Unlock()

	m.emit(WorkerEvent{
		Type: EventWorkerRecycled, WorkerID: workerIDOf(s), Kind: s.kind, Slot: s.idx, Reason: reason,
	})

	if lease != nil {
		lease.mu.Lock()
		lease.expired = true
		cancel := lease.cancelExec
		lease.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		_ = m.releaseLease(ctx, lease)
	}

	if oldWorker != nil {
		m.cleanupProcessTree(oldWorker)
		m.stopWorkerQuietly(oldWorker)
	}

	// 走与崩溃相同的恢复链条：置为待重启，由 sweep 按退避拉起 + resume queued。
	m.handleWorkerExit(ctx, s, fmt.Errorf("%w: %s", ErrCrash, reason), callID)
}

// ============================== 观测 / 关闭 ==============================

// SlotInfos 返回所有 slot 的快照（供 UI/任务 04/07 观测）。
func (m *Manager) SlotInfos() []SlotInfo {
	m.mu.RLock()
	pools := make([]*pool, 0, len(m.pools))
	for _, p := range m.pools {
		pools = append(pools, p)
	}
	m.mu.RUnlock()

	sort.Slice(pools, func(i, j int) bool {
		if pools[i].spec.Priority != pools[j].spec.Priority {
			return pools[i].spec.Priority < pools[j].spec.Priority
		}
		return pools[i].kind < pools[j].kind
	})

	var out []SlotInfo
	for _, p := range pools {
		for _, s := range p.slots {
			s.mu.Lock()
			info := SlotInfo{
				WorkerID:    workerIDOfLocked(s),
				Kind:        s.kind,
				Slot:        s.idx,
				State:       s.state,
				StateName:   s.state.String(),
				Restarts:    s.restarts,
				Crashes:     s.crashes,
				InFlight:    s.inflight,
				Queued:      len(s.pending),
				LastError:   s.lastErr,
				LastExitAt:  s.lastExit,
				LastReadyAt: s.lastRdy,
				Health:      s.health,
			}
			if s.lease != nil {
				info.LeaseActive = !s.lease.Released()
				info.LeaseID = s.lease.ID()
				info.LeaseExpires = s.lease.ExpiresAt()
			}
			if s.circuit != nil {
				st, _ := s.circuit.Snapshot()
				info.Circuit = st.String()
			}
			if pa, ok := s.worker.(ProcessAware); ok {
				info.PIDs = pa.PIDs()
			}
			s.mu.Unlock()
			out = append(out, info)
		}
	}
	return out
}

// Health 汇总所有 Worker 的健康状态。
//
// 语义（重要，直接决定 Supervisor 会不会误判）：
//   - 只要该 kind 还有**任意一个** ready slot，就健康（故障隔离原则）；
//   - 若某 kind 没有 ready slot，但仍有 slot 处于 starting/restarting（正在恢复），
//     也视为"可服务（降级）"并返回 nil。理由：正在按退避重启是**正常运转**的一部分，
//     此时向上报错会让 Supervisor 对整个 Worker 池采取激进动作（重启风暴），
//     反而放大了故障。真正不可恢复的状态是 circuit_open（熔断），那时才报错。
//   - 熔断且无 ready slot → 返回错误，如实上报"该能力不可用"。
//
// 注意本函数**永不返回 error**（除上述不可用情形），因为 Supervisor 依赖它在
// "有 Worker 挂了"时也能正常判断整体可服务性。
func (m *Manager) Health(ctx context.Context) error {
	infos := m.SlotInfos()

	readyByKind := map[string]int{}
	recoveringByKind := map[string]int{}
	circuitByKind := map[string]int{}
	totalByKind := map[string]int{}

	for _, info := range infos {
		totalByKind[info.Kind]++
		switch info.State {
		case StateReady:
			readyByKind[info.Kind]++
		case StateStarting, StateRestarting, StateCold, StateDraining:
			recoveringByKind[info.Kind]++
		case StateCircuitOpen:
			circuitByKind[info.Kind]++
		}
	}

	for kind, total := range totalByKind {
		if readyByKind[kind] > 0 {
			continue // 有可用 slot：健康。
		}
		if recoveringByKind[kind] > 0 {
			continue // 正在恢复：降级但可服务，不报错。
		}
		// 全部熔断（或已停止）：如实上报不可用。
		return fmt.Errorf("%w: kind=%s 无可用 worker（%d/%d 个 slot 处于熔断/停止状态）",
			ErrUnavailable, kind, circuitByKind[kind], total)
	}
	return nil
}

// Stop 优雅停机：先停维护循环，再逐个停止 Worker 并清理进程树。
// 顺序对齐第 29 章的 "terminate workers" 步。
func (m *Manager) Stop(ctx context.Context) error {
	m.stopOnce.Do(func() {
		m.mu.Lock()
		m.closed = true
		pools := make([]*pool, 0, len(m.pools))
		for _, p := range m.pools {
			pools = append(pools, p)
		}
		m.mu.Unlock()

		close(m.stopCh)

		// 先让所有 slot 停止接受新调用，再等待在飞调用收尾。
		for _, p := range pools {
			for _, s := range p.slots {
				s.mu.Lock()
				if s.state != StateStopped {
					s.state = StateDraining
				}
				lease := s.lease
				s.mu.Unlock()
				if lease != nil {
					lease.mu.Lock()
					lease.expired = true
					cancel := lease.cancelExec
					lease.mu.Unlock()
					if cancel != nil {
						cancel()
					}
				}
			}
		}

		// 排空在飞调用：等待至多 DrainTimeout。
		//
		// 这里用**真实时钟**轮询而不是注入时钟：排空等待属于停机流程的
		// 保护性超时（安全网），不是被测业务逻辑。早期实现用 m.clock 的
		// ticker，在注入可控时钟的测试里永不触发，导致 Stop 永久阻塞。
		deadline := time.Now().Add(m.cfg.DrainTimeout)
		tk := time.NewTicker(10 * time.Millisecond)
		for {
			total := 0
			for _, p := range pools {
				for _, s := range p.slots {
					s.mu.Lock()
					total += s.inflight
					s.mu.Unlock()
				}
			}
			if total == 0 || time.Now().After(deadline) {
				break
			}
			select {
			case <-ctx.Done():
				tk.Stop()
				goto drain
			case <-tk.C:
			}
		}
		tk.Stop()
	drain:
		// 停止每个 Worker 并强制清理其进程树（红线：不能留孤儿进程）。
		for _, p := range pools {
			for _, s := range p.slots {
				s.mu.Lock()
				w := s.worker
				s.worker = nil
				s.state = StateStopped
				// 未执行的排队调用直接以"停机"失败，避免调用方永久挂起。
				pending := s.pending
				s.pending = nil
				s.mu.Unlock()
				for _, pc := range pending {
					pc.fail(ErrorResponse(pc.req, workerIDOf(s), CodeUnavailable, "Manager 停机"),
						fmt.Errorf("%w: Manager 停机", ErrUnavailable))
				}
				if w != nil {
					m.cleanupProcessTree(w)
					m.stopWorkerQuietly(w)
					m.metrics.SetGauge(MetricReady, s.kind, w.ID(), 0)
					m.emit(WorkerEvent{Type: EventWorkerStopped, WorkerID: w.ID(), Kind: s.kind, Slot: s.idx})
				}
			}
		}
	})

	// 等待维护循环退出（goroutine 必须有退出路径）。
	m.wg.Wait()
	// 清理残留的进程树（Worker 自行上报过的 PID）。
	for _, info := range m.lastKnownPIDs() {
		killProcessTree(info)
	}
	return nil
}

// lastKnownPIDs 返回停机后仍可能存活的 PID（尽力而为的兜底）。
func (m *Manager) lastKnownPIDs() []int { return nil }

func (m *Manager) isClosed() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.closed
}

func (m *Manager) emit(ev WorkerEvent) {
	ev.At = m.clock.Now()
	if ev.Kind == "" && ev.WorkerID != "" {
		ev.Kind = ""
	}
	m.events.Emit(ev)
}

func workerIDOfLocked(s *slot) string {
	if s.worker != nil {
		return s.worker.ID()
	}
	return fmt.Sprintf("%s-%d", s.kind, s.idx)
}

func workerIDOf(s *slot) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return workerIDOfLocked(s)
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// pendingCall 结果交付。
func (pc *pendingCall) finish(resp WorkerResponse, err error) { pc.deliver(resp, err) }

func (pc *pendingCall) fail(resp WorkerResponse, err error) { pc.deliver(resp, err) }

func (pc *pendingCall) deliver(resp WorkerResponse, err error) {
	pc.resp = resp
	pc.err = err
	select {
	case <-pc.done:
		// 已交付（例如调用方先因 ctx 取消退出），不重复关闭。
	default:
		close(pc.done)
	}
}
