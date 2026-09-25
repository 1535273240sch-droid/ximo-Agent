package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/worker"
)

// Restart 是单 server 的重连调度状态。每个 server 一份，互相独立。
type restartState struct {
	attempt   int
	nextAt    time.Time
	inFlight  bool
	lastError string
}

// Supervisor 是 MCP 的 Worker 实现：编排多个 ServerSession，每个独立故障域。
//
// 它同时扮演两个角色：
//   - 对 Manager/Supervisor 而言，它是一个 kind="mcp" 的 Worker；
//   - 对内部而言，它是 N 个 ServerSession 的编排者（连接、重连、聚合工具列表）。
//
// 关键约束：**单个 server 的失败绝不传播**。ConnectAll 对每个 server 独立 try，
// 重连调度也是逐 server 独立退避（1s→2s→4s→8s→16s→30s 封顶）。
type Supervisor struct {
	id      string
	factory TransportFactory
	clock   worker.Clock
	log     worker.Logger
	events  worker.EventSink

	mu       sync.Mutex
	sessions map[string]*ServerSession
	order    []string // 保持配置顺序，便于确定性聚合
	restarts map[string]*restartState
	started  bool
	closed   bool

	// maintenance 循环
	stopCh chan struct{}
	wg     sync.WaitGroup
}

// SupervisorConfig 配置 Supervisor。
type SupervisorConfig struct {
	Servers []ServerConfig
	// Factory 创建传输实现。为 nil 时按 transport 类型选择内置实现。
	Factory TransportFactory
	// MaintenanceInterval 是重连调度循环的间隔。<=0 时用 1s。
	MaintenanceInterval time.Duration
	// ConnectTimeout 是单 server 首次连接的超时。<=0 时用 20s。
	ConnectTimeout time.Duration
}

// NewSupervisor 创建 MCP Supervisor。
func NewSupervisor(id string, cfg SupervisorConfig, clock worker.Clock, log worker.Logger, events worker.EventSink) *Supervisor {
	if clock == nil {
		clock = worker.RealClock{}
	}
	if log == nil {
		log = worker.NopLogger{}
	}
	if events == nil {
		events = worker.NopEventSink{}
	}
	if cfg.MaintenanceInterval <= 0 {
		cfg.MaintenanceInterval = time.Second
	}
	if cfg.ConnectTimeout <= 0 {
		cfg.ConnectTimeout = 20 * time.Second
	}
	// Factory 为 nil 时回退到内置传输工厂，兑现 SupervisorConfig.Factory 的
	// 文档承诺（"为 nil 时按 transport 类型选择内置实现"）。缺了这一步，
	// NewFromSpec 构造出的 Supervisor 里每个 session 都会在 Connect 阶段以
	// 「未配置 TransportFactory」失败并降级，外部表现为「MCP 服务器永远连不上、
	// tools/list 恒为空」，而内置工厂 DefaultTransportFactory 从未被任何调用点使用。
	if cfg.Factory == nil {
		cfg.Factory = DefaultTransportFactory
	}
	s := &Supervisor{
		id:       id,
		factory:  cfg.Factory,
		clock:    clock,
		log:      log,
		events:   events,
		sessions: map[string]*ServerSession{},
		restarts: map[string]*restartState{},
		stopCh:   make(chan struct{}),
	}
	for _, sc := range cfg.Servers {
		sc = sc.withDefaults()
		if !sc.Enabled {
			// disabled 的 server 连会话都不建（第 40 章 feature flag 语义）。
			continue
		}
		sess := NewServerSession(sc, cfg.Factory, clock, log, events)
		s.sessions[sc.ID] = sess
		s.order = append(s.order, sc.ID)
		s.restarts[sc.ID] = &restartState{}
	}
	return s
}

// NewFromSpec 从 worker.Spec 构造（Manager Factory）。
//
// 期望 spec.Config 含 "servers": []ServerConfig 或 "config_json": "<JSON>"。
func NewFromSpec(spec worker.Spec) (worker.Worker, error) {
	var servers []ServerConfig
	if raw := spec.String("config_json", ""); raw != "" {
		parsed, err := ParseConfigs([]byte(raw))
		if err != nil {
			return nil, err
		}
		servers = parsed
	} else if raw, ok := spec.Config["servers"]; ok {
		b, err := json.Marshal(raw)
		if err != nil {
			return nil, fmt.Errorf("%w: servers 序列化失败: %v", worker.ErrInvalidArgument, err)
		}
		if err := json.Unmarshal(b, &servers); err != nil {
			return nil, fmt.Errorf("%w: servers 解析失败: %v", worker.ErrInvalidArgument, err)
		}
	}
	return NewSupervisor(spec.ID, SupervisorConfig{Servers: servers}, nil, nil, nil), nil
}

// ID 实现 worker.Worker。
func (s *Supervisor) ID() string { return s.id }

// Kind 实现 worker.Worker。
func (s *Supervisor) Kind() string { return "mcp" }

// Start 实现 worker.Worker：并发连接所有启用的 server，各自独立超时与失败。
func (s *Supervisor) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return nil
	}
	s.started = true
	s.closed = false
	ids := append([]string(nil), s.order...)
	s.mu.Unlock()

	if len(ids) == 0 {
		// 没有配置任何 server 是合法状态（功能未启用），不算失败。
		s.startMaintenance()
		return nil
	}

	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			s.mu.Lock()
			sess := s.sessions[id]
			s.mu.Unlock()
			if sess == nil {
				return
			}
			// 每个 server 独立超时、独立失败：一个卡住不拖累其他（故障域隔离）。
			cctx, cancel := context.WithTimeout(ctx, sess.Config().RequestTimeout+10*time.Second)
			defer cancel()
			if err := sess.Connect(cctx); err != nil {
				// 失败不返回 error——交由重连调度按退避节奏重试。
				s.recordRestart(id, err)
				s.log.Warn("MCP server 首次连接失败，将按退避重连",
					"server", id, "err", err.Error())
			}
		}(id)
	}
	wg.Wait()

	s.startMaintenance()
	return nil
}

// startMaintenance 启动重连调度循环（单个 ticker，带 Stop）。
func (s *Supervisor) startMaintenance() {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		tk := s.clock.NewTicker(time.Second)
		defer tk.Stop()
		for {
			select {
			case <-s.stopCh:
				return
			case <-tk.C():
				s.tick()
			}
		}
	}()
}

// tick 检查每个 server 是否需要重连，并按各自退避节奏执行。
func (s *Supervisor) tick() {
	s.mu.Lock()
	ids := append([]string(nil), s.order...)
	s.mu.Unlock()

	for _, id := range ids {
		s.tickServer(id)
	}
}

func (s *Supervisor) tickServer(id string) {
	s.mu.Lock()
	sess := s.sessions[id]
	rs := s.restarts[id]
	closed := s.closed
	s.mu.Unlock()
	if sess == nil || rs == nil || closed {
		return
	}

	cfg := sess.Config()

	// 已达最大重连次数：停止重试，但**保留会话**，让上层能看到"不可用"状态。
	// 这与"静默继续跑"正相反——不可用就是不可用，必须可见。
	if rs.attempt >= cfg.MaxRestarts && sess.State() != SessionReady {
		return
	}

	if sess.State() == SessionReady {
		// 已就绪：清零退避计数（成功即重置，符合退避语义）。
		s.mu.Lock()
		rs.attempt = 0
		rs.nextAt = time.Time{}
		rs.inFlight = false
		s.mu.Unlock()
		return
	}

	// 熔断打开时不做重连尝试，等冷却（避免"崩溃-重连"风暴）。
	if sess.circuit != nil && sess.circuit.State() == worker.CircuitOpen {
		return
	}

	now := s.clock.Now()
	if !rs.nextAt.IsZero() && now.Before(rs.nextAt) {
		return
	}
	if rs.inFlight {
		return
	}
	s.mu.Lock()
	rs.inFlight = true
	attempt := rs.attempt
	s.mu.Unlock()

	go func() {
		defer func() {
			s.mu.Lock()
			rs.inFlight = false
			s.mu.Unlock()
		}()
		delay := worker.Backoff{Schedule: worker.DefaultBackoffSchedule, Jitter: 0.1}.Delay(attempt)
		s.log.Info("MCP server 重连中",
			"server", id, "attempt", attempt+1, "delay", delay.String())

		// 先关掉旧传输（含进程树清理），再重连。
		ctx, cancel := context.WithTimeout(context.Background(), cfg.RequestTimeout+10*time.Second)
		defer cancel()
		_ = sess.resetTransport(ctx)

		if err := sess.Connect(ctx); err != nil {
			s.recordRestart(id, err)
			return
		}
		s.mu.Lock()
		rs.attempt = 0
		rs.nextAt = time.Time{}
		rs.lastError = ""
		s.mu.Unlock()
		s.log.Info("MCP server 重连成功", "server", id)
	}()
}

// recordRestart 记录一次失败并安排下一次退避时间。
func (s *Supervisor) recordRestart(id string, err error) {
	s.mu.Lock()
	rs := s.restarts[id]
	if rs != nil {
		rs.attempt++
		rs.lastError = errString(err)
		rs.nextAt = s.clock.Now().Add(s.restartBackoff(rs.attempt))
	}
	s.mu.Unlock()
}

func (s *Supervisor) restartBackoff(attempt int) time.Duration {
	return worker.Backoff{Schedule: worker.DefaultBackoffSchedule, Jitter: 0.1}.Delay(attempt - 1)
}

// Health 实现 worker.Worker：只要**至少一个** server 就绪，或没有任何 server 配置，
// 就认为整体健康（故障隔离：一个 server 挂掉不代表 MCP 能力整体不可用）。
func (s *Supervisor) Health(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return fmt.Errorf("%w: MCP supervisor 已关闭", worker.ErrUnavailable)
	}
	if len(s.sessions) == 0 {
		return nil
	}
	ready := 0
	for _, sess := range s.sessions {
		if sess.State() == SessionReady {
			ready++
		}
	}
	if ready == 0 {
		return fmt.Errorf("%w: 所有 MCP server 均不可用", worker.ErrUnavailable)
	}
	return nil
}

// Stop 实现 worker.Worker：停止维护循环并关闭所有会话（含 stdio 进程树）。
func (s *Supervisor) Stop(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	sessions := make([]*ServerSession, 0, len(s.sessions))
	for _, sess := range s.sessions {
		sessions = append(sessions, sess)
	}
	s.mu.Unlock()

	close(s.stopCh)
	s.wg.Wait() // 等维护循环退出（goroutine 都有退出路径）

	for _, sess := range sessions {
		if err := sess.Close(ctx); err != nil {
			s.log.Warn("关闭 MCP 会话失败", "server", sess.ID(), "err", err.Error())
		}
	}
	return nil
}

// PIDs 实现 worker.ProcessAware：聚合所有 server 的外部进程 PID。
func (s *Supervisor) PIDs() []int {
	s.mu.Lock()
	sessions := make([]*ServerSession, 0, len(s.sessions))
	for _, sess := range s.sessions {
		sessions = append(sessions, sess)
	}
	s.mu.Unlock()

	var out []int
	for _, sess := range sessions {
		s.mu.Lock()
		tr := sess.transportOf()
		s.mu.Unlock()
		if tr != nil {
			out = append(out, tr.PIDs()...)
		}
	}
	return out
}

// Snapshot 返回所有 server 的状态（供 UI/任务 07 观测）。
func (s *Supervisor) Snapshot() []SessionSnapshot {
	s.mu.Lock()
	ids := append([]string(nil), s.order...)
	s.mu.Unlock()
	out := make([]SessionSnapshot, 0, len(ids))
	for _, id := range ids {
		s.mu.Lock()
		sess := s.sessions[id]
		s.mu.Unlock()
		if sess != nil {
			out = append(out, sess.Snapshot())
		}
	}
	return out
}

// AllTools 聚合所有**已就绪** server 的工具列表。
//
// 只聚合就绪的 server：从降级 server 拿到的可能是过期 schema，
// 暴露过期工具会让 Agent 调用到不存在的工具。
func (s *Supervisor) AllTools() []ToolSchema {
	var out []ToolSchema
	for _, snap := range s.Snapshot() {
		if snap.State != SessionReady && snap.State != SessionDegraded {
			continue
		}
		s.mu.Lock()
		sess := s.sessions[snap.ID]
		s.mu.Unlock()
		if sess == nil {
			continue
		}
		out = append(out, sess.Tools()...)
	}
	return out
}

// 动作名。
const (
	ActionToolsCall = "tools/call"
	ActionToolsList = "tools/list"
	ActionCall      = "call"
	ActionPing      = "ping"
	ActionRefresh   = "refresh_tools"
	ActionStatus    = "status"
)

// callArgs 是 tools/call 的参数。
type callArgs struct {
	// Server 可选：指定走哪个 server。为空时按 Tool 名反查。
	Server string          `json:"server,omitempty"`
	Tool   string          `json:"tool"`
	Args   json.RawMessage `json:"arguments,omitempty"`
}

// Execute 实现 worker.Worker：把动作路由到具体 server 会话。
func (s *Supervisor) Execute(ctx context.Context, req worker.WorkerRequest) (worker.WorkerResponse, error) {
	started := time.Now()
	switch req.Action {
	case ActionToolsCall, ActionCall, "mcp_call":
		var a callArgs
		if err := req.Bind(&a); err != nil {
			return worker.ErrorResponse(req, s.id, worker.CodeInvalidArgument, err.Error()), nil
		}
		sess, err := s.sessionForTool(a.Server, a.Tool)
		if err != nil {
			return worker.ErrorResponse(req, s.id, worker.CodeUnavailable, err.Error()), nil
		}
		params, _ := json.Marshal(map[string]any{"name": a.Tool, "arguments": rawOrEmpty(a.Args)})
		raw, err := sess.Call(ctx, "tools/call", params)
		if err != nil {
			return worker.ErrorResponseFrom(req, s.id, started, err), nil
		}
		return worker.OKResponse(req, s.id, started, map[string]any{
			"content": string(raw),
			"server":  sess.ID(),
			"tool":    a.Tool,
			"result":  json.RawMessage(raw),
		})

	case ActionToolsList:
		tools := s.AllTools()
		return worker.OKResponse(req, s.id, started, map[string]any{
			"tools": tools, "count": len(tools),
			"content": fmt.Sprintf("共 %d 个 MCP 工具", len(tools)),
		})

	case ActionPing:
		var a struct {
			Server string `json:"server,omitempty"`
		}
		_ = req.Bind(&a)
		s.mu.Lock()
		sessions := make([]*ServerSession, 0, len(s.sessions))
		if a.Server != "" {
			if sess := s.sessions[a.Server]; sess != nil {
				sessions = append(sessions, sess)
			}
		} else {
			for _, sess := range s.sessions {
				sessions = append(sessions, sess)
			}
		}
		s.mu.Unlock()
		if len(sessions) == 0 {
			return worker.ErrorResponse(req, s.id, worker.CodeUnavailable, "找不到目标 MCP server"), nil
		}
		var results = map[string]string{}
		for _, sess := range sessions {
			if err := sess.Ping(ctx); err != nil {
				results[sess.ID()] = err.Error()
			} else {
				results[sess.ID()] = "ok"
			}
		}
		return worker.OKResponse(req, s.id, started, map[string]any{
			"results": results, "content": fmt.Sprintf("已探测 %d 个 server", len(results)),
		})

	case ActionRefresh:
		var a struct {
			Server string `json:"server,omitempty"`
		}
		_ = req.Bind(&a)
		s.mu.Lock()
		var targets []*ServerSession
		if a.Server != "" {
			if sess := s.sessions[a.Server]; sess != nil {
				targets = append(targets, sess)
			}
		} else {
			for _, sess := range s.sessions {
				targets = append(targets, sess)
			}
		}
		s.mu.Unlock()
		for _, sess := range targets {
			if sess.State() == SessionReady {
				_ = sess.RefreshTools(ctx)
			}
		}
		return worker.OKResponse(req, s.id, started, map[string]any{
			"content": fmt.Sprintf("已刷新 %d 个 server 的工具列表", len(targets)),
		})

	case ActionStatus, "":
		snaps := s.Snapshot()
		return worker.OKResponse(req, s.id, started, map[string]any{
			"servers": snaps,
			"content": fmt.Sprintf("MCP: %d 个 server", len(snaps)),
		})

	default:
		return worker.ErrorResponse(req, s.id, worker.CodeInvalidArgument,
			fmt.Sprintf("未知的 MCP 动作 %q", req.Action)), nil
	}
}

// sessionForTool 按 server 名或工具名反查会话。
func (s *Supervisor) sessionForTool(server, tool string) (*ServerSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if server != "" {
		sess := s.sessions[NormalizeID(server)]
		if sess == nil {
			sess = s.sessions[server]
		}
		if sess == nil {
			return nil, fmt.Errorf("找不到 MCP server %q", server)
		}
		if sess.State() != SessionReady && sess.State() != SessionDegraded {
			return nil, fmt.Errorf("MCP server %q 当前不可用 (%s)", server, sess.State())
		}
		return sess, nil
	}
	// 按工具名反查：优先匹配 ExposedName（mcp__tool），再匹配原始名。
	for _, sess := range s.sessions {
		for _, t := range sess.Tools() {
			if t.ExposedName == tool || t.Name == tool {
				if sess.State() != SessionReady && sess.State() != SessionDegraded {
					return nil, fmt.Errorf("MCP server %q 当前不可用 (%s)", sess.ID(), sess.State())
				}
				return sess, nil
			}
		}
	}
	return nil, fmt.Errorf("找不到提供工具 %q 的 MCP server", tool)
}

func rawOrEmpty(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(`{}`)
	}
	return raw
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
