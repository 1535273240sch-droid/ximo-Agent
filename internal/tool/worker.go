package tool

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Worker 协议（与任务05对齐；Result normalization 由本模块负责）
// ---------------------------------------------------------------------------

// WorkerRequest 路由给 Worker 的请求。Task05 的 Worker.Execute 消费该结构，
// 08 号主 Agent 统一核对两边字段映射。
type WorkerRequest struct {
	RunID      string          `json:"run_id"`
	ToolCallID string          `json:"tool_call_id"`
	ToolName   string          `json:"tool_name"`
	Domain     ExecutionDomain `json:"domain"`
	Arguments  map[string]any  `json:"arguments,omitempty"`
	Deadline   time.Time       `json:"deadline,omitempty"`
	// SandboxPolicy 动态 JS 工具的沙箱策略（由任务04定义，任务05的
	// dynamic worker 宿主执行时施加）。
	SandboxPolicy *SandboxPolicy `json:"sandbox_policy,omitempty"`
}

// WorkerResponse Worker 回传的原始结果。
type WorkerResponse struct {
	ToolCallID string `json:"tool_call_id"`
	ToolName   string `json:"tool_name"`
	Content    string `json:"content"`
	Success    bool   `json:"success"`
	Error      string `json:"error,omitempty"`
	// ErrorCode 失败分类（进程内工具失败时透传，归一化时保留）。
	ErrorCode ErrorCode      `json:"error_code,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
	// Raw 归一化前的原始字节（如 base64 截图、结构化输出）。
	Raw []byte `json:"raw,omitempty"`
}

// ResultNormalizer 把 Worker 原始结果归一化为 ToolResponse（任务05消费方契约）。
type ResultNormalizer interface {
	Normalize(raw WorkerResponse) (ToolResponse, error)
}

// ErrWorkerResponseMismatch Worker 回传的 toolCallID 与请求不一致。
var ErrWorkerResponseMismatch = errors.New("tool: worker 响应的 tool_call_id 与请求不匹配")

// DefaultNormalizer 是 ResultNormalizer 的默认实现。
type DefaultNormalizer struct {
	// MaxContentBytes 内容长度上限；超出返回错误（防止 Worker 回传巨型输出）。
	MaxContentBytes int
}

// NewDefaultNormalizer 创建默认归一化器。
func NewDefaultNormalizer(maxContentBytes int) *DefaultNormalizer {
	if maxContentBytes <= 0 {
		maxContentBytes = 8 << 20 // 8MB
	}
	return &DefaultNormalizer{MaxContentBytes: maxContentBytes}
}

// Normalize 实现 ResultNormalizer：校验 ID 一致性、截断超限内容、补全字段。
func (n *DefaultNormalizer) Normalize(raw WorkerResponse) (ToolResponse, error) {
	if len(raw.Content) > n.MaxContentBytes {
		return ToolResponse{}, fmt.Errorf("内容 %d 字节超过上限 %d（%s）", len(raw.Content), n.MaxContentBytes, ErrNormalizationFailed)
	}
	resp := ToolResponse{
		ToolCallID: raw.ToolCallID,
		ToolName:   raw.ToolName,
		Content:    raw.Content,
		Success:    raw.Success,
		Error:      raw.Error,
		ErrorCode:  raw.ErrorCode,
		Metadata:   raw.Metadata,
	}
	if !resp.Success && resp.ErrorCode == ErrOK {
		resp.ErrorCode = ErrToolFailed
	}
	return resp, nil
}

// ---------------------------------------------------------------------------
// Worker 路由与崩溃恢复链（第13章）
// ---------------------------------------------------------------------------

// ErrWorkerUnavailable Worker 不可用（崩溃、重启中或队列已满）。
type ErrWorkerUnavailable struct {
	Domain     ExecutionDomain
	Reason     string
	Restarting bool
}

func (e *ErrWorkerUnavailable) Error() string {
	if e.Restarting {
		return fmt.Sprintf("tool: worker 域 %s 正在重启恢复：%s", e.Domain, e.Reason)
	}
	return fmt.Sprintf("tool: worker 域 %s 不可用：%s", e.Domain, e.Reason)
}

// WorkerRouter 把工具调用路由到对应域的 Worker（任务05的 Worker Manager
// 提供实现；开发期用 mock）。
type WorkerRouter interface {
	Route(ctx context.Context, req WorkerRequest) (WorkerResponse, error)
}

// WorkerController 控制 Worker 生命周期（任务05 的 Worker Manager 提供实现）。
type WorkerController interface {
	// Restart 重启指定域的 worker。
	Restart(ctx context.Context, domain ExecutionDomain) error
	// Health 对指定域执行健康检查。
	Health(ctx context.Context, domain ExecutionDomain) error
}

// WorkerFailure 一次 worker 失败记录（持久化，供恢复与审计）。
type WorkerFailure struct {
	Domain          ExecutionDomain
	WorkerID        string
	DetectedAt      time.Time
	Reason          string
	RejectedCalls   []string
	RestartAttempts int
	Recovered       bool
}

// WorkerFailureStore worker 失败记录的持久化接口（先用内存 mock 顶上，
// 生产由任务03 落库）。
type WorkerFailureStore interface {
	RecordFailure(ctx context.Context, f WorkerFailure) error
	MarkRecovered(ctx context.Context, domain ExecutionDomain, recoveredAt time.Time) error
}

// GatewayConfig Worker 网关配置。
type GatewayConfig struct {
	// MaxQueuedPerDomain 每域恢复排队上限；超出后新调用 reject fast（第6章）。
	MaxQueuedPerDomain int
	// HealthCheckInterval 重启后健康检查间隔。
	HealthCheckInterval time.Duration
	// RestartBackoff 重启退避基础步长（1s→2s→4s…封顶 MaxRestartBackoff）。
	RestartBackoff time.Duration
	// MaxRestartBackoff 退避上限。
	MaxRestartBackoff time.Duration
	// MaxRestartAttempts 连续重启失败上限；达到后该域标记为 dead，
	// 排队调用全部失败（需要 Supervisor/人工介入）。
	MaxRestartAttempts int
}

// DefaultGatewayConfig 默认配置。
func DefaultGatewayConfig() GatewayConfig {
	return GatewayConfig{
		MaxQueuedPerDomain:  64,
		HealthCheckInterval: 200 * time.Millisecond,
		RestartBackoff:      time.Second,
		MaxRestartBackoff:   30 * time.Second,
		MaxRestartAttempts:  8,
	}
}

// WorkerGateway 实现 Worker 路由与崩溃恢复链：
// detect exit → reject in-flight calls → persist worker failure →
// restart worker → health check → resume queued calls。
//
// 每个域由一个 supervisor goroutine 独占处理（owner 明确），通过
// Start(ctx)/Stop() 控制生命周期；所有 goroutine 退出前被 WaitGroup 等待，
// 保证“所有 goroutine 都有 owner 和退出路径”。
type WorkerGateway struct {
	router     WorkerRouter
	controller WorkerController
	failures   WorkerFailureStore
	cfg        GatewayConfig

	mu       sync.Mutex
	domains  map[ExecutionDomain]*domainState
	started  bool
	stopping bool
	// ctx 是 Start 时传入的上下文，供运行期新建域的 supervisor 使用。
	ctx    context.Context
	wg     sync.WaitGroup
	stopCh chan struct{}
}

type domainState struct {
	inFlight   map[string]ToolRequest
	queue      []queuedCall
	restarting bool
	healthy    bool
	dead       bool
	attempts   int
	restarts   chan struct{} // 通知 supervisor 处理重启
}

type queuedCall struct {
	req ToolRequest
}

// NewWorkerGateway 创建 Worker 网关。
func NewWorkerGateway(router WorkerRouter, controller WorkerController, failures WorkerFailureStore, cfg GatewayConfig) *WorkerGateway {
	return &WorkerGateway{
		router:     router,
		controller: controller,
		failures:   failures,
		cfg:        cfg,
		domains:    make(map[ExecutionDomain]*domainState),
		stopCh:     make(chan struct{}),
	}
}

// Start 启动网关（幂等）。ctx 取消时所有 supervisor goroutine 退出。
func (g *WorkerGateway) Start(ctx context.Context) error {
	g.mu.Lock()
	if g.started {
		g.mu.Unlock()
		return nil
	}
	g.started = true
	g.ctx = ctx
	domains := make([]ExecutionDomain, 0, len(g.domains))
	for d := range g.domains {
		domains = append(domains, d)
	}
	g.mu.Unlock()

	for _, d := range domains {
		g.startSupervisor(ctx, d)
	}
	return nil
}

// Stop 停止网关并等待所有 supervisor goroutine 退出。
func (g *WorkerGateway) Stop() {
	g.mu.Lock()
	if !g.started {
		g.mu.Unlock()
		return
	}
	// stopping 与 started 在同一把锁下翻转：保证 Stop 之后再无
	// wg.Add（避免 WaitGroup “Add after Wait” 误用）。
	g.started = false
	g.stopping = true
	close(g.stopCh)
	g.mu.Unlock()
	g.wg.Wait()
}

func (g *WorkerGateway) startSupervisor(ctx context.Context, domain ExecutionDomain) {
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		g.supervise(ctx, domain)
	}()
}

// state 返回（必要时创建）域状态。运行期首次出现的域会立即补一个
// supervisor goroutine——否则 Start 之后新建的域将永远没有恢复链。
// 调用方必须持有 g.mu；stopping 之后不再新建 goroutine（避免 WaitGroup
// “Add after Wait”）。
func (g *WorkerGateway) state(domain ExecutionDomain) *domainState {
	st, ok := g.domains[domain]
	if !ok {
		st = &domainState{
			inFlight: make(map[string]ToolRequest),
			healthy:  true,
			restarts: make(chan struct{}, 1),
		}
		g.domains[domain] = st
		if g.started && !g.stopping && g.ctx != nil {
			ctx := g.ctx
			g.startSupervisor(ctx, domain)
		}
	}
	return st
}

// Route 路由一次工具调用。Worker 失败时执行崩溃恢复链并返回
// *ErrWorkerUnavailable；调用方（ToolRuntime）应把该错误呈现为
// ErrWorkerCrashed，等待恢复后由队列重放。
func (g *WorkerGateway) Route(ctx context.Context, req WorkerRequest) (WorkerResponse, error) {
	domain := req.Domain
	g.mu.Lock()
	st := g.state(domain)
	if !g.started {
		// 未启动时自动启动（容错），保证路由永远有 supervisor 兜底。
		g.mu.Unlock()
		if err := g.Start(ctx); err != nil {
			return WorkerResponse{}, err
		}
		g.mu.Lock()
		st = g.state(domain)
	}
	if st.dead {
		g.mu.Unlock()
		return WorkerResponse{}, &ErrWorkerUnavailable{Domain: domain, Reason: "worker 已标记 dead（重启次数超限）"}
	}
	if st.restarting {
		// 恢复期：入队等待（有上限），否则 reject fast。
		if len(st.queue) >= g.cfg.MaxQueuedPerDomain {
			g.mu.Unlock()
			return WorkerResponse{}, &ErrWorkerUnavailable{Domain: domain, Reason: "恢复队列已满", Restarting: true}
		}
		st.queue = append(st.queue, queuedCall{req: req.toToolRequest()})
		g.mu.Unlock()
		return WorkerResponse{}, &ErrWorkerUnavailable{Domain: domain, Reason: "worker 重启中，调用已排队", Restarting: true}
	}
	st.inFlight[req.ToolCallID] = req.toToolRequest()
	g.mu.Unlock()

	resp, err := g.router.Route(ctx, req)

	g.mu.Lock()
	delete(st.inFlight, req.ToolCallID)
	g.mu.Unlock()

	if err != nil {
		if isWorkerCrash(err) {
			g.handleCrash(ctx, domain, req, err)
		}
		return WorkerResponse{}, err
	}
	return resp, nil
}

// handleCrash 执行崩溃恢复链：reject in-flight → persist failure → restart → health → resume。
func (g *WorkerGateway) handleCrash(ctx context.Context, domain ExecutionDomain, failedReq WorkerRequest, cause error) {
	g.mu.Lock()
	st := g.state(domain)
	// 1. 拒绝该域全部 in-flight 调用（包括刚失败的这个）。
	rejected := make([]string, 0, len(st.inFlight)+1)
	for id := range st.inFlight {
		rejected = append(rejected, id)
	}
	rejected = append(rejected, failedReq.ToolCallID)
	st.inFlight = make(map[string]ToolRequest)
	st.restarting = true
	st.attempts++
	attempts := st.attempts
	g.mu.Unlock()

	// 2. 持久化 worker 失败记录。
	if g.failures != nil {
		_ = g.failures.RecordFailure(ctx, WorkerFailure{
			Domain:          domain,
			DetectedAt:      time.Now(),
			Reason:          cause.Error(),
			RejectedCalls:   rejected,
			RestartAttempts: attempts,
		})
	}

	// 3. 通知 supervisor 重启（非阻塞；channel 容量 1，合并信号）。
	select {
	case st.restarts <- struct{}{}:
	default:
	}
}

// supervise 是每个域的 supervisor goroutine（owner = WorkerGateway）。
// 退出条件：Stop() 被调用（stopCh 关闭）或 ctx 取消。
func (g *WorkerGateway) supervise(ctx context.Context, domain ExecutionDomain) {
	g.mu.Lock()
	st := g.state(domain)
	restarts := st.restarts
	g.mu.Unlock()

	for {
		select {
		case <-ctx.Done():
			return
		case <-g.stopCh:
			return
		case <-restarts:
			if !g.restartAndRecover(ctx, domain) {
				return
			}
		}
	}
}

// restartAndRecover 执行重启 + 健康检查 + 恢复排队调用。
// 返回 false 表示该 supervisor 应退出（ctx 取消 / Stop）。
func (g *WorkerGateway) restartAndRecover(ctx context.Context, domain ExecutionDomain) bool {
	g.mu.Lock()
	st := g.state(domain)
	attempts := st.attempts
	g.mu.Unlock()

	if attempts > g.cfg.MaxRestartAttempts {
		// 重启次数超限：该域标记 dead，失败全部排队调用。
		g.mu.Lock()
		st.dead = true
		st.restarting = false
		queue := st.queue
		st.queue = nil
		g.mu.Unlock()
		_ = queue // 排队调用由后续 Route 以 ErrWorkerUnavailable 拒绝
		return true
	}

	backoff := g.cfg.RestartBackoff << min(attempts-1, 16)
	if backoff > g.cfg.MaxRestartBackoff || backoff <= 0 {
		backoff = g.cfg.MaxRestartBackoff
	}
	if !sleepCtx(ctx, g.stopCh, backoff) {
		return false
	}

	if g.controller != nil {
		if err := g.controller.Restart(ctx, domain); err != nil {
			// 重启失败计一次尝试；超过上限则标记 dead（需要 Supervisor/人工介入）。
			g.mu.Lock()
			st.attempts++
			exceeded := st.attempts > g.cfg.MaxRestartAttempts
			if exceeded {
				st.dead = true
				st.restarting = false
				queue := st.queue
				st.queue = nil
				g.mu.Unlock()
				_ = queue // 排队调用由后续 Route 以 ErrWorkerUnavailable 拒绝
				return true
			}
			g.mu.Unlock()
			// 退避后重试。
			select {
			case <-ctx.Done():
				return false
			case <-g.stopCh:
				return false
			case st.restarts <- struct{}{}:
			}
			return true
		}
	}

	// 健康检查通过后才恢复排队调用。
	if g.controller != nil {
		for {
			if err := g.controller.Health(ctx, domain); err == nil {
				break
			}
			if !sleepCtx(ctx, g.stopCh, g.cfg.HealthCheckInterval) {
				return false
			}
			select {
			case <-ctx.Done():
				return false
			case <-g.stopCh:
				return false
			default:
			}
		}
	}

	g.mu.Lock()
	st.healthy = true
	st.restarting = false
	queue := st.queue
	st.queue = nil
	g.mu.Unlock()

	if g.failures != nil {
		_ = g.failures.MarkRecovered(ctx, domain, time.Now())
	}

	// 恢复排队调用（按 FIFO）。单个调用失败不影响其余。
	for _, call := range queue {
		select {
		case <-ctx.Done():
			return false
		case <-g.stopCh:
			return false
		default:
		}
		wreq := WorkerRequest{
			RunID:      call.req.RunID,
			ToolCallID: call.req.ToolCallID,
			ToolName:   call.req.Name,
			Domain:     domain,
			Arguments:  call.req.Arguments,
			Deadline:   call.req.Deadline,
		}
		if _, err := g.router.Route(ctx, wreq); err != nil && isWorkerCrash(err) {
			g.handleCrash(ctx, domain, wreq, err)
			return true
		}
	}
	return true
}

// isWorkerCrash 判断错误是否表示 worker 崩溃/不可用（而非业务失败）。
func isWorkerCrash(err error) bool {
	if err == nil {
		return false
	}
	var unavailable *ErrWorkerUnavailable
	if errors.As(err, &unavailable) {
		return true
	}
	return errors.Is(err, context.DeadlineExceeded) || err.Error() == "worker crashed"
}

func (r WorkerRequest) toToolRequest() ToolRequest {
	return ToolRequest{
		RunID:      r.RunID,
		ToolCallID: r.ToolCallID,
		Name:       r.ToolName,
		Arguments:  r.Arguments,
		Deadline:   r.Deadline,
	}
}

// sleepCtx 可取消的 sleep。返回 false 表示 ctx 取消或网关停止。
func sleepCtx(ctx context.Context, stopCh <-chan struct{}, d time.Duration) bool {
	if d <= 0 {
		select {
		case <-ctx.Done():
			return false
		case <-stopCh:
			return false
		default:
			return true
		}
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-stopCh:
		return false
	case <-timer.C:
		return true
	}
}

// Stats 网关运行时统计（供观测）。
type GatewayStats struct {
	Domains map[ExecutionDomain]DomainStats
}

// DomainStats 单域统计。
type DomainStats struct {
	InFlight   int
	Queued     int
	Restarting bool
	Healthy    bool
	Dead       bool
	Attempts   int
}

// Stats 返回当前统计快照。
func (g *WorkerGateway) Stats() GatewayStats {
	g.mu.Lock()
	defer g.mu.Unlock()
	stats := GatewayStats{Domains: make(map[ExecutionDomain]DomainStats, len(g.domains))}
	for d, st := range g.domains {
		stats.Domains[d] = DomainStats{
			InFlight:   len(st.inFlight),
			Queued:     len(st.queue),
			Restarting: st.restarting,
			Healthy:    st.healthy,
			Dead:       st.dead,
			Attempts:   st.attempts,
		}
	}
	return stats
}
