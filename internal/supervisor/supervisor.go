package supervisor

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/config"
	"github.com/ximo888ok-netizen/ximo-agent/internal/ipc"
)

var (
	ErrEntityNotFound   = errors.New("supervisor: entity not found")
	ErrSupervisorClosed = errors.New("supervisor: supervisor is closed")
	ErrDuplicateEntity  = errors.New("supervisor: entity with same ID already exists")
)

// Supervisable 提供给外部任务（如 05 Worker 池）的契约接口
// 第54-60行原文：
//
//	type Supervisable interface {
//	    ID() string
//	    Kind() string       // "engine" | "ui" | "worker:browser" | "worker:mcp" | ...
//	    Start() error
//	    Heartbeat() error
//	    Kill() error
//	}
type Supervisable interface {
	ID() string
	Kind() string
	Start() error
	Heartbeat() error
	Kill() error
}

// EngineResumer 消费任务02的 Engine.Resume 接口（第65行）
type EngineResumer interface {
	Resume(ctx context.Context, runID string) error
}

// MockEngineResumer 默认的 Mock 实现，开发期不阻塞
type MockEngineResumer struct {
	ResumedRuns []string
	mu          sync.Mutex
}

func (m *MockEngineResumer) Resume(ctx context.Context, runID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ResumedRuns = append(m.ResumedRuns, runID)
	return nil
}

// WorkerHealthStopper 消费任务05的 Worker.Health / Stop 接口（第66行）
type WorkerHealthStopper interface {
	Health(ctx context.Context) error
	Stop(ctx context.Context) error
}

// MockWorkerHandler 默认的 Mock 实现
type MockWorkerHandler struct{}

func (m *MockWorkerHandler) Health(ctx context.Context) error { return nil }
func (m *MockWorkerHandler) Stop(ctx context.Context) error   { return nil }

// EntityState 托管实体的运行状态
type EntityState string

const (
	StateInitializing EntityState = "initializing"
	StateRunning      EntityState = "running"
	StateUnavailable  EntityState = "unavailable"
	StateRecovering   EntityState = "recovering"
	StateDegraded     EntityState = "degraded"
	StateStopped      EntityState = "stopped"
)

// ManagedEntity 监管者内部封装的实体信息
type ManagedEntity struct {
	Item           Supervisable
	State          EntityState
	LastHeartbeat  time.Time
	RestartTracker *RestartTracker
	Process        *ManagedProcess
	LeaseExpireAt  time.Time
}

// GracefulShutdownHook 优雅停机各步骤的执行钩子（对齐第29章）
type GracefulShutdownHook func(ctx context.Context) error

// ShutdownPipeline 第29章 9步优雅停机流水线
type ShutdownPipeline struct {
	Step1StopAcceptingRuns      GracefulShutdownHook
	Step2StopAcceptingToolCalls GracefulShutdownHook
	Step3CancelInteractive      GracefulShutdownHook
	Step4FlushEventWriter       GracefulShutdownHook
	Step5FlushOutbox            GracefulShutdownHook
	Step6PersistRunStates       GracefulShutdownHook
	Step7TerminateWorkers       GracefulShutdownHook
	Step8CloseDB                GracefulShutdownHook
	Step9Exit                   GracefulShutdownHook
}

// Supervisor 极简、高可靠的故障域监管者与看门狗
type Supervisor struct {
	cfg       config.SupervisorConfig
	crashCol  *CrashCollector
	resumer   EngineResumer
	ipcServer *ipc.Server

	mu       sync.RWMutex
	entities map[string]*ManagedEntity

	// 追踪待恢复的运行列表（供 Engine 重启后触发恢复）
	unrecoveredRunsMu sync.Mutex
	unrecoveredRuns   map[string]struct{}

	shutdownPipeline ShutdownPipeline

	ctx        context.Context
	cancel     context.CancelFunc
	closedChan chan struct{}
}

// NewSupervisor 创建监管者实例
func NewSupervisor(cfg config.SupervisorConfig, crashDumpDir string, resumer EngineResumer) *Supervisor {
	if resumer == nil {
		resumer = &MockEngineResumer{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Supervisor{
		cfg:             cfg,
		crashCol:        NewCrashCollector(crashDumpDir),
		resumer:         resumer,
		entities:        make(map[string]*ManagedEntity),
		unrecoveredRuns: make(map[string]struct{}),
		ctx:             ctx,
		cancel:          cancel,
		closedChan:      make(chan struct{}),
	}
	return s
}

// SetIPCServer 注入 IPC 服务端引用
func (s *Supervisor) SetIPCServer(srv *ipc.Server) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ipcServer = srv
}

// SetEngineResumer 设置 Engine.Resume 实现
func (s *Supervisor) SetEngineResumer(resumer EngineResumer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resumer = resumer
}

// SetShutdownPipeline 配置 9 步优雅关闭钩子
func (s *Supervisor) SetShutdownPipeline(p ShutdownPipeline) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.shutdownPipeline = p
}

// RegisterEntity 注册可监管实体（校验同 ID 重复注册，杜绝孤儿进程泄漏）
func (s *Supervisor) RegisterEntity(item Supervisable, proc *ManagedProcess) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	id := item.ID()
	if _, dup := s.entities[id]; dup {
		return fmt.Errorf("%w: %s", ErrDuplicateEntity, id)
	}

	// The backoff ladder comes from configuration rather than a hardcoded
	// default, so RestartBackoffSteps actually takes effect.
	tracker := NewRestartTrackerWithBackoff(
		s.cfg.MaxRestartRetries,
		60*time.Second,
		NewBackoffFromSteps(s.cfg.RestartBackoffSteps),
	)

	s.entities[id] = &ManagedEntity{
		Item:           item,
		State:          StateInitializing,
		LastHeartbeat:  time.Now(),
		RestartTracker: tracker,
		Process:        proc,
	}

	return nil
}

// UnregisterEntity 注销可监管实体
func (s *Supervisor) UnregisterEntity(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	entity, exists := s.entities[id]
	if !exists {
		return ErrEntityNotFound
	}

	_ = entity.Item.Kill()
	delete(s.entities, id)
	return nil
}

// Start 启动 Supervisor Watchdog 与心跳监控循环
func (s *Supervisor) Start() error {
	s.mu.Lock()
	// 启动所有初始实体
	for _, e := range s.entities {
		if err := e.Item.Start(); err != nil {
			// 记录失败
			e.State = StateDegraded
		} else {
			e.State = StateRunning
			e.LastHeartbeat = time.Now()
			e.RestartTracker.RecordSuccess()
		}
	}
	s.mu.Unlock()

	go s.watchdogLoop()
	return nil
}

// RecordHeartbeat 刷新指定实体的心跳
func (s *Supervisor) RecordHeartbeat(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if e, exists := s.entities[id]; exists {
		e.LastHeartbeat = time.Now()
		if e.State == StateUnavailable || e.State == StateInitializing {
			e.State = StateRunning
		}
	}
}

// AcquireLease 为 Worker 分配/续约租约
func (s *Supervisor) AcquireLease(id string, duration time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	e, exists := s.entities[id]
	if !exists || e.State != StateRunning {
		return false
	}
	e.LeaseExpireAt = time.Now().Add(duration)
	return true
}

// TrackUnrecoveredRun 登记需要恢复的 runID
func (s *Supervisor) TrackUnrecoveredRun(runID string) {
	s.unrecoveredRunsMu.Lock()
	defer s.unrecoveredRunsMu.Unlock()
	s.unrecoveredRuns[runID] = struct{}{}
}

func (s *Supervisor) watchdogLoop() {
	ticker := time.NewTicker(s.cfg.HeartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.checkHealth()
		}
	}
}

func (s *Supervisor) checkHealth() {
	s.mu.Lock()
	now := time.Now()

	type actionItem struct {
		id       string
		entity   *ManagedEntity
		action   string // "lease_expire" | "process_exit" | "unhealthy"
	}
	var actions []actionItem

	for id, e := range s.entities {
		// 1. 检查租约超时
		if !e.LeaseExpireAt.IsZero() && now.After(e.LeaseExpireAt) {
			e.State = StateDegraded
			actions = append(actions, actionItem{id: id, entity: e, action: "lease_expire"})
			continue
		}

		// 2. 检查进程是否意外退出
		if e.Process != nil && !e.Process.IsAlive() && e.State == StateRunning {
			e.State = StateUnavailable
			actions = append(actions, actionItem{id: id, entity: e, action: "process_exit"})
			continue
		}

		// 3. 调用实体的 Heartbeat 探测与超时判定
		if err := e.Item.Heartbeat(); err == nil {
			e.LastHeartbeat = now
		} else if now.Sub(e.LastHeartbeat) > s.cfg.HeartbeatTimeout {
			e.State = StateUnavailable
			actions = append(actions, actionItem{id: id, entity: e, action: "unhealthy"})
		}
	}
	s.mu.Unlock()

	// 锁外执行耗时的磁盘 IO (crash dump 写盘)、进程 Kill 与异步恢复
	for _, act := range actions {
		switch act.action {
		case "lease_expire":
			_ = act.entity.Item.Kill()
		case "process_exit":
			go s.handleProcessExit(act.id, act.entity)
		case "unhealthy":
			go s.handleEntityUnhealthy(act.id, act.entity)
		}
	}
}

func (s *Supervisor) handleProcessExit(id string, e *ManagedEntity) {
	var exitCode int = -1
	if e.Process != nil {
		select {
		case evt := <-e.Process.ExitChan():
			exitCode = evt.ExitCode
		default:
		}
	}

	// Snapshot the fields shared with the watchdog under the lock: a concurrent
	// recoverEntity() writes e.State and e.LastHeartbeat, so reading them
	// unlocked here is a data race.
	s.mu.Lock()
	kind := e.Item.Kind()
	lastBeat := e.LastHeartbeat
	var pid int
	var stderr []byte
	if e.Process != nil {
		pid = e.Process.PID()
	}
	s.mu.Unlock()

	// Collect the crash dump outside the lock; writing to disk must not stall
	// the watchdog.
	if e.Process != nil {
		stderr = e.Process.StderrTail()
	}
	_, _ = s.crashCol.Collect(kind, kind, pid, exitCode, lastBeat, stderr)

	s.recoverEntity(id, e)
}

func (s *Supervisor) handleEntityUnhealthy(id string, e *ManagedEntity) {
	_ = e.Item.Kill()

	// Same lock discipline as handleProcessExit: read shared entity state under
	// the lock, do the disk IO outside it.
	s.mu.Lock()
	kind := e.Item.Kind()
	lastBeat := e.LastHeartbeat
	var pid int
	if e.Process != nil {
		pid = e.Process.PID()
	}
	s.mu.Unlock()

	var stderr []byte
	if e.Process != nil {
		stderr = e.Process.StderrTail()
	}
	_, _ = s.crashCol.Collect(kind, kind, pid, -2, lastBeat, stderr)

	s.recoverEntity(id, e)
}

// 恢复算法（对齐第38章伪代码规范）：
// Engine exit → read last heartbeat → mark engine unavailable
// → scan runs where status ∈ {running, executing, waiting_worker}
// → 对每个run: load last durable event, inspect incomplete tool calls, classify idempotency
// → restart engine → engine readiness → recover safe runs → resume
func (s *Supervisor) recoverEntity(id string, e *ManagedEntity) {
	s.mu.Lock()
	if e.State == StateRecovering || e.State == StateStopped {
		s.mu.Unlock()
		return
	}
	e.State = StateRecovering
	s.mu.Unlock()

	delay, allowRetry := e.RestartTracker.RecordFailure()
	if !allowRetry {
		s.mu.Lock()
		e.State = StateDegraded
		s.mu.Unlock()
		return
	}

	// 严格按 1s->2s->4s->8s->16s->30s 封顶退避
	timer := time.NewTimer(delay)
	select {
	case <-s.ctx.Done():
		timer.Stop()
		return
	case <-timer.C:
	}

	// 重启实体
	if err := e.Item.Start(); err != nil {
		s.mu.Lock()
		e.State = StateUnavailable
		s.mu.Unlock()
		return
	}

	s.mu.Lock()
	e.State = StateRunning
	e.LastHeartbeat = time.Now()
	isEngine := e.Item.Kind() == "engine"
	s.mu.Unlock()

	// 若为 Engine，等待就绪后触发未决 Run 的 Resume（第38章算法）
	if isEngine {
		s.triggerEngineRecovery()
	}
}

// triggerEngineRecovery 在 Engine 重启成功后驱动 run 恢复（第38章算法）。
//
// 两种情形分别处理，且顺序语义被既有契约固定（有登记时只点名登记的 run，
// 不得额外插入其它 Resume 调用）：
//  1. 有登记过的未决 run：逐个点名恢复，成功后从集合移除。
//  2. 集合为空：发一次「全量恢复」请求（runID 为空）。
//
// 第 2 条是必需项而非优化：生产代码里没有任何模块调用 TrackUnrecoveredRun
// （登记发生在业务层，Supervisor 并不知道业务），若只在集合非空时才发帧，
// 那么「kill Engine 之后自动续跑」在真实进程组合下永远不触发——恢复帧一次都
// 发不出去。这正是该 P0 红线此前在进程级别断裂的第二个原因。
//
// 为什么情形 1 不额外补发全量恢复：Engine 进程在启动时已经自行调用过
// Recover()（见 cmd/ximo-agent/main.go 的 runEngine），未登记的 run 由那次
// 启动扫描兜底，因此这里无需重复，也避免改变既有调用序列语义。
func (s *Supervisor) triggerEngineRecovery() {
	s.unrecoveredRunsMu.Lock()
	runsToRecover := make([]string, 0, len(s.unrecoveredRuns))
	for r := range s.unrecoveredRuns {
		runsToRecover = append(runsToRecover, r)
	}
	s.unrecoveredRunsMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if len(runsToRecover) == 0 {
		// 全量恢复：让 Engine 自己扫库发现未完成的 run。
		_ = s.resumer.Resume(ctx, "")
		return
	}

	for _, runID := range runsToRecover {
		_ = s.resumer.Resume(ctx, runID)
		s.unrecoveredRunsMu.Lock()
		delete(s.unrecoveredRuns, runID)
		s.unrecoveredRunsMu.Unlock()
	}
}

// GracefulShutdown 严格按第29章的9步顺序执行关闭，并设置优雅超时窗口
func (s *Supervisor) GracefulShutdown() error {
	s.cancel()

	select {
	case <-s.closedChan:
		return nil
	default:
		close(s.closedChan)
	}

	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.GracefulShutdownTimeout)
	defer cancel()

	doneChan := make(chan error, 1)
	go func() {
		doneChan <- s.execute9StepShutdown(ctx)
	}()

	select {
	case err := <-doneChan:
		return err
	case <-ctx.Done():
		// 超时后触发 emergency snapshot 并强杀所有进程
		s.emergencySnapshotAndForceKill()
		return fmt.Errorf("graceful shutdown timed out after %v: %w", s.cfg.GracefulShutdownTimeout, ctx.Err())
	}
}

func (s *Supervisor) execute9StepShutdown(ctx context.Context) error {
	s.mu.RLock()
	p := s.shutdownPipeline
	s.mu.RUnlock()

	// 1. stop accepting runs
	if p.Step1StopAcceptingRuns != nil {
		_ = p.Step1StopAcceptingRuns(ctx)
	}

	// 2. stop accepting tool calls
	if p.Step2StopAcceptingToolCalls != nil {
		_ = p.Step2StopAcceptingToolCalls(ctx)
	}

	// 3. finish/cancel interactive
	if p.Step3CancelInteractive != nil {
		_ = p.Step3CancelInteractive(ctx)
	}

	// 4. flush event writer
	if p.Step4FlushEventWriter != nil {
		_ = p.Step4FlushEventWriter(ctx)
	}

	// 5. flush outbox
	if p.Step5FlushOutbox != nil {
		_ = p.Step5FlushOutbox(ctx)
	}

	// 6. persist run states
	if p.Step6PersistRunStates != nil {
		_ = p.Step6PersistRunStates(ctx)
	}

	// 7. terminate workers
	if p.Step7TerminateWorkers != nil {
		_ = p.Step7TerminateWorkers(ctx)
	} else {
		// 默认终止所有 Worker 实体与进程树
		s.mu.Lock()
		for _, e := range s.entities {
			if e.Item.Kind() != "engine" {
				_ = e.Item.Kill()
				e.State = StateStopped
			}
		}
		s.mu.Unlock()
	}

	// 终止 Engine 实体
	s.mu.Lock()
	for _, e := range s.entities {
		_ = e.Item.Kill()
		e.State = StateStopped
	}
	s.mu.Unlock()

	// 8. close DB
	if p.Step8CloseDB != nil {
		_ = p.Step8CloseDB(ctx)
	}

	// 9. exit
	if p.Step9Exit != nil {
		_ = p.Step9Exit(ctx)
	}

	if s.ipcServer != nil {
		_ = s.ipcServer.Stop()
	}

	return nil
}

func (s *Supervisor) emergencySnapshotAndForceKill() {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 强制杀除所有实体其进程树
	for _, e := range s.entities {
		_ = e.Item.Kill()
		e.State = StateStopped
	}
}

// GetEntityState 查询指定实体状态
func (s *Supervisor) GetEntityState(id string) (EntityState, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	e, exists := s.entities[id]
	if !exists {
		return "", ErrEntityNotFound
	}
	return e.State, nil
}
