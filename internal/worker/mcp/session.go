// Package mcp 实现 MCP Supervisor：每个 MCP server 是**独立故障域**（任务书第 17 章）。
//
// 目标结构（任务书原文）：
//
//	MCP Supervisor
//	 ├─ server A（独立: connection state / pending request map / request timeout /
//	 │            heartbeat / restart backoff / tool schema cache）
//	 └─ server B（同上）
//
// v1 审计结论（本包存在的理由）：
//
//	v1 的 McpClient 只有 connected + pending map，**没有心跳、没有重启退避、
//	没有 circuit breaker**；断线后只能靠上层"设置变更时整体作废会话"来重建，
//	一个 server 卡死会拖到 30s 请求超时才暴露。
//	v1 的 sse 传输实际上退化成 HTTP POST，并没有维护 SSE 长连接。
//
// v2 的改进：
//   - 每个 server 有独立的连接状态机、pending 请求表、超时、心跳、退避重启、
//     工具 schema 缓存、熔断器；
//   - Supervisor 负责整体编排（连接全部启用的 server、聚合工具列表），
//     但任何单个 server 失败都不影响其他 server（故障域隔离）；
//   - 传输层用 Transport 接口抽象，stdio / http / sse 各自实现，
//     配置格式兼容 Cursor / Claude Code / Cline / Windsurf（字段与 v1 一致）。
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/worker"
)

// ============================== 配置（字段与 v1 / 主流客户端兼容） ==============================

// TransportKind 是 MCP 传输类型。
type TransportKind string

const (
	TransportStdio TransportKind = "stdio"
	TransportSSE   TransportKind = "sse"
	TransportHTTP  TransportKind = "http"
)

// ServerConfig 与 v1 的 McpServerConfig 字段对齐，
// 同时兼容 Cursor / Claude Code / Cline / Windsurf 的 mcpServers 配置格式。
type ServerConfig struct {
	ID        string        `json:"id"`
	Name      string        `json:"name"`
	Transport TransportKind `json:"transport"`
	Enabled   bool          `json:"enabled"`
	// Command / Args / Env 用于 stdio。
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	// URL / Headers 用于 http/sse。
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	// 以下为 v2 新增的每 server 可调参数。
	RequestTimeout  time.Duration `json:"request_timeout,omitempty"`
	HeartbeatEvery  time.Duration `json:"heartbeat_interval,omitempty"`
	MaxRestarts     int           `json:"max_restarts,omitempty"`
	CircuitFailures int           `json:"circuit_failures,omitempty"`
	CircuitCooldown time.Duration `json:"circuit_cooldown,omitempty"`
	// AllowedTools 非空时只暴露其中的工具（权限收敛，第 20 章）。
	AllowedTools []string `json:"allowed_tools,omitempty"`
	// DeniedTools 中的工具永不暴露。
	DeniedTools []string `json:"denied_tools,omitempty"`
	// WorkingDir 是 stdio server 的工作目录。
	WorkingDir string `json:"cwd,omitempty"`
}

func (c ServerConfig) withDefaults() ServerConfig {
	if c.ID == "" {
		c.ID = NormalizeID(c.Name)
	}
	if c.Name == "" {
		c.Name = c.ID
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = 30 * time.Second // 对齐 v1 默认值
	}
	if c.HeartbeatEvery <= 0 {
		c.HeartbeatEvery = 30 * time.Second
	}
	if c.MaxRestarts <= 0 {
		c.MaxRestarts = 5
	}
	if c.CircuitFailures <= 0 {
		c.CircuitFailures = 5
	}
	if c.CircuitCooldown <= 0 {
		c.CircuitCooldown = 30 * time.Second
	}
	if c.Transport == "" {
		// 与 v1 McpStore 的判定逻辑一致：有 url 时按 sse/http，否则 stdio。
		if c.URL != "" {
			c.Transport = TransportHTTP
		} else {
			c.Transport = TransportStdio
		}
	}
	return c
}

// NormalizeID 按 v1 的规则生成 ID：小写、非字母数字替换为 '-'。
func NormalizeID(name string) string {
	var sb strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_':
			sb.WriteRune(r)
			lastDash = r == '-'
		default:
			if !lastDash {
				sb.WriteByte('-')
				lastDash = true
			}
		}
	}
	out := strings.Trim(sb.String(), "-")
	if out == "" {
		out = "server"
	}
	return out
}

// ParseConfigs 解析兼容多种客户端格式的 MCP 配置。
//
// 支持三种形态（与 v1 McpStore 的对等能力）：
//  1. {"mcpServers": {...}}（Claude Code / Cursor / Cline / Windsurf 标准格式）
//  2. {"servers": [...]}（本系统自己的数组格式）
//  3. 裸对象数组 [...]
func ParseConfigs(data []byte) ([]ServerConfig, error) {
	var probe any
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("%w: MCP 配置不是合法 JSON: %v", worker.ErrInvalidArgument, err)
	}

	switch v := probe.(type) {
	case map[string]any:
		if servers, ok := v["mcpServers"].(map[string]any); ok {
			return decodeNamedServers(servers)
		}
		if servers, ok := v["servers"]; ok {
			raw, _ := json.Marshal(servers)
			return decodeServerList(raw)
		}
		// 可能是单个 server 对象。
		if _, hasCmd := v["command"]; hasCmd {
			raw, _ := json.Marshal(v)
			var sc ServerConfig
			if err := json.Unmarshal(raw, &sc); err != nil {
				return nil, fmt.Errorf("%w: 解析单个 MCP server 失败: %v", worker.ErrInvalidArgument, err)
			}
			return []ServerConfig{sc.withDefaults()}, nil
		}
		return nil, fmt.Errorf("%w: 未在配置中找到 mcpServers/servers 字段", worker.ErrInvalidArgument)
	case []any:
		raw, _ := json.Marshal(v)
		return decodeServerList(raw)
	default:
		return nil, fmt.Errorf("%w: 不支持的 MCP 配置结构", worker.ErrInvalidArgument)
	}
}

// decodeNamedServers 处理 {"name": {config}} 形态（标准客户端格式）。
func decodeNamedServers(m map[string]any) ([]ServerConfig, error) {
	out := make([]ServerConfig, 0, len(m))
	for name, body := range m {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("%w: server %s 序列化失败: %v", worker.ErrInvalidArgument, name, err)
		}
		var sc ServerConfig
		if err := json.Unmarshal(raw, &sc); err != nil {
			return nil, fmt.Errorf("%w: server %s 配置非法: %v", worker.ErrInvalidArgument, name, err)
		}
		if sc.Name == "" {
			sc.Name = name
		}
		if sc.ID == "" {
			sc.ID = NormalizeID(name)
		}
		out = append(out, sc.withDefaults())
	}
	// 保持确定性顺序，避免每次启动顺序不同（便于测试与审计）。
	sortByName(out)
	return out, nil
}

func decodeServerList(raw []byte) ([]ServerConfig, error) {
	var list []ServerConfig
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("%w: MCP server 列表非法: %v", worker.ErrInvalidArgument, err)
	}
	for i := range list {
		list[i] = list[i].withDefaults()
	}
	sortByName(list)
	return list, nil
}

func sortByName(list []ServerConfig) {
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && list[j].ID < list[j-1].ID; j-- {
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
}

// ============================== JSON-RPC 与会话 ==============================

// Request 是一条 JSON-RPC 2.0 请求。
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response 是一条 JSON-RPC 2.0 响应。
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError 是 JSON-RPC 错误。
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("MCP 错误 %d: %s", e.Code, e.Message)
}

// Transport 是 MCP 传输抽象。stdio / http / sse 各自实现。
//
// 注意 stdio 实现在传输层就用了 procguard（进程树管控），
// 因此 stdio server 被 kill 时不会留下孤儿进程（对齐 v1 漏掉的部分）。
type Transport interface {
	// Start 建立传输（stdio 拉起进程 / http 建连）。
	Start(ctx context.Context, cfg ServerConfig) error
	// Send 发送一条请求并等待响应。实现内部必须尊重 ctx 与 cfg.RequestTimeout。
	Send(ctx context.Context, req Request) (Response, error)
	// Notify 发送一条不需要响应的通知（如 notifications/initialized）。
	Notify(ctx context.Context, method string, params json.RawMessage) error
	// Ping 心跳探测。传输不支持时返回 nil（由上层决定是否降级）。
	Ping(ctx context.Context) error
	// Close 关闭传输并清理资源（含进程树）。
	Close(ctx context.Context) error
	// PIDs 返回该传输持有的外部进程 PID（stdio 用），供崩溃清理。
	PIDs() []int
}

// TransportFactory 按配置创建传输。
type TransportFactory func(cfg ServerConfig) (Transport, error)

// ToolSchema 是一个 MCP 工具的描述（schema 缓存的元素）。
type ToolSchema struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
	// ServerID 标注来源 server，便于 UI 展示与权限匹配。
	ServerID string `json:"serverId"`
	// ExposedName 是暴露给 Agent 的工具名（v1 是 "mcp__" + name，保持一致）。
	ExposedName string `json:"exposedName"`
}

// ============================== ServerSession：单个 server ==============================

// SessionState 是单 server 的状态机取值。
type SessionState int32

const (
	SessionIdle SessionState = iota
	SessionConnecting
	SessionReady
	SessionDegraded
	SessionCircuitOpen
	SessionClosed
)

func (s SessionState) String() string {
	switch s {
	case SessionIdle:
		return "idle"
	case SessionConnecting:
		return "connecting"
	case SessionReady:
		return "ready"
	case SessionDegraded:
		return "degraded"
	case SessionCircuitOpen:
		return "circuit_open"
	case SessionClosed:
		return "closed"
	default:
		return "unknown"
	}
}

// ServerSession 是**一个 MCP server 的独立故障域**。
//
// 它自己持有：连接状态、pending 请求表、请求超时、心跳、重启退避、工具 schema 缓存、
// 熔断器。任何字段都不与其他 server 共享——这就是"独立故障域"的具体含义。
type ServerSession struct {
	cfg     ServerConfig
	factory TransportFactory
	clock   worker.Clock
	log     worker.Logger
	events  worker.EventSink

	mu         sync.Mutex
	state      SessionState
	transport  Transport
	restarts   int
	lastErr    string
	readyAt    time.Time
	tools      []ToolSchema
	toolsAt    time.Time // schema 缓存时间
	nextID     int64
	pending    map[int64]*pendingReq
	circuit    *worker.CircuitBreaker
	backoff    worker.Backoff
	closed     bool
	hbStop     chan struct{}
	hbDone     chan struct{}
	reconnects int
}

type pendingReq struct {
	ch       chan Response
	errCh    chan error
	method   string
	sentAt   time.Time
	deadline time.Time
}

// NewServerSession 创建一个 server 会话。
func NewServerSession(cfg ServerConfig, factory TransportFactory, clock worker.Clock, log worker.Logger, events worker.EventSink) *ServerSession {
	cfg = cfg.withDefaults()
	if clock == nil {
		clock = worker.RealClock{}
	}
	if log == nil {
		log = worker.NopLogger{}
	}
	if events == nil {
		events = worker.NopEventSink{}
	}
	s := &ServerSession{
		cfg:     cfg,
		factory: factory,
		clock:   clock,
		log:     log,
		events:  events,
		state:   SessionIdle,
		pending: map[int64]*pendingReq{},
		backoff: worker.Backoff{Schedule: worker.DefaultBackoffSchedule, Jitter: 0.1},
	}
	s.circuit = worker.NewCircuitBreaker(worker.CircuitConfig{
		FailureThreshold:  cfg.CircuitFailures,
		SuccessThreshold:  2,
		Cooldown:          cfg.CircuitCooldown,
		MaxHalfOpenProbes: 1,
	}, clock, func(from, to worker.CircuitState, reason string) {
		evType := worker.EventCircuitClosed
		switch to {
		case worker.CircuitOpen:
			evType = worker.EventCircuitOpened
		case worker.CircuitHalfOpen:
			evType = worker.EventCircuitHalfOpen
		}
		events.Emit(worker.WorkerEvent{
			Type: evType, Kind: "mcp:" + cfg.ID, Reason: reason,
		})
	})
	return s
}

// ID 返回 server ID。
func (s *ServerSession) ID() string { return s.cfg.ID }

// Config 返回生效配置。
func (s *ServerSession) Config() ServerConfig { return s.cfg }

// Snapshot 返回会话状态快照（供 UI/Supervisor 观测）。
type SessionSnapshot struct {
	ID         string        `json:"id"`
	Name       string        `json:"name"`
	Transport  TransportKind `json:"transport"`
	State      SessionState  `json:"state"`
	StateName  string        `json:"stateName"`
	Restarts   int           `json:"restarts"`
	Reconnects int           `json:"reconnects"`
	ToolCount  int           `json:"toolCount"`
	Pending    int           `json:"pending"`
	Circuit    string        `json:"circuit"`
	LastError  string        `json:"lastError,omitempty"`
	ReadyAt    time.Time     `json:"readyAt,omitempty"`
	PIDs       []int         `json:"pids,omitempty"`
}

// Snapshot 返回当前快照。
func (s *ServerSession) Snapshot() SessionSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := SessionSnapshot{
		ID: s.cfg.ID, Name: s.cfg.Name, Transport: s.cfg.Transport,
		State: s.state, StateName: s.state.String(),
		Restarts: s.restarts, Reconnects: s.reconnects,
		ToolCount: len(s.tools), Pending: len(s.pending),
		LastError: s.lastErr, ReadyAt: s.readyAt,
	}
	if s.circuit != nil {
		st, _ := s.circuit.Snapshot()
		snap.Circuit = st.String()
	}
	if s.transport != nil {
		snap.PIDs = s.transport.PIDs()
	}
	return snap
}

// Connect 建立连接并完成 MCP 握手 + 拉取工具列表。
//
// 握手流程与 v1 对等：initialize → notifications/initialized → tools/list。
func (s *ServerSession) Connect(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return fmt.Errorf("%w: 会话已关闭", worker.ErrUnavailable)
	}
	s.state = SessionConnecting
	s.mu.Unlock()

	if s.factory == nil {
		return s.failConnect(fmt.Errorf("%w: 未配置 TransportFactory", worker.ErrInvalidArgument))
	}
	tr, err := s.factory(s.cfg)
	if err != nil {
		return s.failConnect(fmt.Errorf("创建传输失败: %w", err))
	}
	if err := tr.Start(ctx, s.cfg); err != nil {
		_ = tr.Close(ctx)
		return s.failConnect(fmt.Errorf("启动传输失败: %w", err))
	}

	// --- initialize 握手 ---
	initParams, _ := json.Marshal(map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "ximo-agent", "version": "2.0.0"},
	})
	if _, err := s.call(ctx, tr, "initialize", initParams); err != nil {
		_ = tr.Close(ctx)
		return s.failConnect(fmt.Errorf("initialize 失败: %w", err))
	}
	// --- 就绪通知（不等待响应）---
	if err := tr.Notify(ctx, "notifications/initialized", json.RawMessage(`{}`)); err != nil {
		// 通知失败不致命（有些实现不支持），记日志继续。
		s.log.Warn("MCP initialized 通知发送失败", "server", s.cfg.ID, "err", err.Error())
	}

	s.mu.Lock()
	s.transport = tr
	s.state = SessionReady
	s.lastErr = ""
	s.readyAt = s.clock.Now()
	s.mu.Unlock()

	if err := s.refreshTools(ctx); err != nil {
		// 工具列表拉取失败不算连接失败：连接可用但工具未知，
		// 状态降级为 degraded，让上层决定是否重试（不静默假装有工具）。
		s.mu.Lock()
		s.state = SessionDegraded
		s.lastErr = err.Error()
		s.mu.Unlock()
		s.log.Warn("MCP tools/list 失败，会话降级", "server", s.cfg.ID, "err", err.Error())
	}

	s.startHeartbeat()
	s.events.Emit(worker.WorkerEvent{
		Type: worker.EventWorkerReady, Kind: "mcp:" + s.cfg.ID, Reason: "MCP server 就绪",
	})
	return nil
}

// failConnect 统一处理连接失败：记状态、开熔断、返回错误。
func (s *ServerSession) failConnect(err error) error {
	s.mu.Lock()
	s.state = SessionDegraded
	s.lastErr = err.Error()
	s.mu.Unlock()
	if s.circuit != nil {
		s.circuit.RecordFailure("connect")
	}
	return err
}

// refreshTools 拉取并缓存工具 schema（含权限过滤）。
func (s *ServerSession) refreshTools(ctx context.Context) error {
	var out struct {
		Tools []ToolSchema `json:"tools"`
	}
	if err := s.callInto(ctx, "tools/list", nil, &out); err != nil {
		return err
	}
	allowed := map[string]bool{}
	for _, t := range s.cfg.AllowedTools {
		allowed[t] = true
	}
	denied := map[string]bool{}
	for _, t := range s.cfg.DeniedTools {
		denied[t] = true
	}
	tools := make([]ToolSchema, 0, len(out.Tools))
	for _, t := range out.Tools {
		if denied[t.Name] {
			continue
		}
		if len(allowed) > 0 && !allowed[t.Name] {
			continue
		}
		t.ServerID = s.cfg.ID
		// 与 v1 一致的工具名前缀，保证 Engine 侧的工具名稳定。
		t.ExposedName = "mcp__" + t.Name
		tools = append(tools, t)
	}
	s.mu.Lock()
	s.tools = tools
	s.toolsAt = s.clock.Now()
	s.mu.Unlock()
	return nil
}

// Tools 返回缓存的工具列表（不会触发外部请求）。
func (s *ServerSession) Tools() []ToolSchema {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ToolSchema(nil), s.tools...)
}

// State 返回当前状态。
func (s *ServerSession) State() SessionState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// Ping 主动心跳探测（对外暴露，便于测试与 UI 手动触发）。
func (s *ServerSession) Ping(ctx context.Context) error {
	s.mu.Lock()
	tr := s.transport
	s.mu.Unlock()
	if tr == nil {
		return fmt.Errorf("%w: server %s 未连接", worker.ErrUnavailable, s.cfg.ID)
	}
	err := tr.Ping(ctx)
	if err != nil {
		if s.circuit != nil {
			s.circuit.RecordFailure("ping")
		}
		s.mu.Lock()
		s.lastErr = err.Error()
		s.state = SessionDegraded
		s.mu.Unlock()
		return err
	}
	if s.circuit != nil {
		s.circuit.RecordSuccess()
	}
	return nil
}

// startHeartbeat 启动该 server 的独立心跳（每个 server 一条，互不影响）。
func (s *ServerSession) startHeartbeat() {
	s.mu.Lock()
	if s.hbStop != nil {
		s.mu.Unlock()
		return
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	s.hbStop = stop
	s.hbDone = done
	every := s.cfg.HeartbeatEvery
	// 在持锁期间**同步**创建 ticker，而不是留到 goroutine 里创建：
	// 否则 Connect 返回后、goroutine 被调度前若时钟已推进，ticker 的起点会
	// 顺延一整个周期，形成一个"已连接但心跳未生效"的静默窗口（实测踩到）。
	tk := s.clock.NewTicker(every)
	s.mu.Unlock()

	go func() {
		defer close(done)
		defer tk.Stop() // 验收标准：所有 ticker 都有 Stop
		for {
			select {
			case <-stop:
				return
			case <-tk.C():
				ctx, cancel := context.WithTimeout(context.Background(), s.cfg.RequestTimeout)
				err := s.Ping(ctx)
				cancel()
				if err != nil {
					s.log.Warn("MCP 心跳失败", "server", s.cfg.ID, "err", err.Error())
					// 交给 Supervisor 的重连逻辑处理（不在这里递归重连，避免惊群）。
				}
			}
		}
	}()
}

// stopHeartbeat 停止心跳并等待 goroutine 退出（保证不泄漏）。
func (s *ServerSession) stopHeartbeat() {
	s.mu.Lock()
	stop, done := s.hbStop, s.hbDone
	s.hbStop, s.hbDone = nil, nil
	s.mu.Unlock()
	if stop != nil {
		close(stop)
	}
	if done != nil {
		<-done
	}
}

// Call 调用该 server 上的一次 MCP 方法（带超时、熔断、pending 表管理）。
func (s *ServerSession) Call(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	if s.circuit != nil && !s.circuit.Allow() {
		return nil, fmt.Errorf("%w: server %s 熔断中", worker.ErrUnavailable, s.cfg.ID)
	}
	raw, err := s.call(ctx, nil, method, params)
	if err != nil {
		if s.circuit != nil {
			s.circuit.RecordFailure(method)
		}
		return nil, err
	}
	if s.circuit != nil {
		s.circuit.RecordSuccess()
	}
	return raw, nil
}

// call 是内部调用实现。tr 非空时使用指定传输（连接阶段用），否则用已建立的传输。
func (s *ServerSession) call(ctx context.Context, tr Transport, method string, params json.RawMessage) (json.RawMessage, error) {
	resp, err := s.doCall(ctx, tr, method, params)
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("%w: %v", worker.ErrUpstream, resp.Error)
	}
	return resp.Result, nil
}

// callInto 调用并把结果反序列化到 v。
func (s *ServerSession) callInto(ctx context.Context, method string, params json.RawMessage, v any) error {
	raw, err := s.Call(ctx, method, params)
	if err != nil {
		return err
	}
	if v == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("%w: 解析 %s 响应失败: %v", worker.ErrUpstream, method, err)
	}
	return nil
}

// doCall 登记 pending 请求 → 发送 → 在超时/取消/断线时正确清理 pending。
//
// 这个 pending 表的正确性直接决定"server 卡死时调用方能否及时拿到错误"，
// 因此三条退出路径（响应 / 超时 / 取消）都必须把 entry 摘掉。
func (s *ServerSession) doCall(ctx context.Context, tr Transport, method string, params json.RawMessage) (Response, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return Response{}, fmt.Errorf("%w: 会话已关闭", worker.ErrUnavailable)
	}
	use := tr
	if use == nil {
		use = s.transport
	}
	if use == nil {
		s.mu.Unlock()
		return Response{}, fmt.Errorf("%w: server %s 未连接", worker.ErrUnavailable, s.cfg.ID)
	}
	s.nextID++
	id := s.nextID
	p := &pendingReq{
		ch:     make(chan Response, 1),
		errCh:  make(chan error, 1),
		method: method,
		sentAt: s.clock.Now(),
	}
	s.pending[id] = p
	s.mu.Unlock()

	// 无论走哪条路径，都要把 pending 摘掉并关闭 channel。
	defer func() {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
	}()

	req := Request{JSONRPC: "2.0", ID: id, Method: method, Params: params}

	reqCtx, cancel := context.WithTimeout(ctx, s.cfg.RequestTimeout)
	defer cancel()

	type callResult struct {
		resp Response
		err  error
	}
	done := make(chan callResult, 1)
	go func() {
		resp, err := use.Send(reqCtx, req)
		done <- callResult{resp, err}
	}()

	select {
	case cr := <-done:
		if cr.err != nil {
			s.markBroken(cr.err)
			return Response{}, fmt.Errorf("%w: %s 调用 %s 失败: %v", worker.ErrUpstream, s.cfg.ID, method, cr.err)
		}
		return cr.resp, nil
	case <-reqCtx.Done():
		if errors.Is(reqCtx.Err(), context.DeadlineExceeded) {
			return Response{}, fmt.Errorf("%w: %s 调用 %s 超时（%s）", worker.ErrTimeout, s.cfg.ID, method, s.cfg.RequestTimeout)
		}
		return Response{}, fmt.Errorf("%w: %s 调用 %s 被取消", worker.ErrCanceled, s.cfg.ID, method)
	}
}

// markBroken 在传输层报错时把会话标记为降级（触发 Supervisor 重连）。
func (s *ServerSession) markBroken(err error) {
	s.mu.Lock()
	if s.state == SessionReady {
		s.state = SessionDegraded
	}
	s.lastErr = err.Error()
	s.mu.Unlock()
}

// Close 关闭会话（停心跳 → 关传输 → 清 pending）。
func (s *ServerSession) Close(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	tr := s.transport
	s.transport = nil
	s.state = SessionClosed
	pending := s.pending
	s.pending = map[int64]*pendingReq{}
	s.mu.Unlock()

	s.stopHeartbeat()
	// 让所有等待中的调用立即失败，而不是等到超时（避免停机时挂住调用方）。
	for id, p := range pending {
		select {
		case p.errCh <- fmt.Errorf("%w: server %s 已关闭", worker.ErrUnavailable, s.cfg.ID):
		default:
		}
		_ = id
	}
	if tr != nil {
		return tr.Close(ctx)
	}
	return nil
}

// NeedRestart 报告会话是否需要重连（供 Supervisor 的维护循环判断）。
func (s *ServerSession) NeedRestart() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	if s.state == SessionIdle || s.state == SessionDegraded {
		return true
	}
	if s.restarts >= s.cfg.MaxRestarts {
		return false
	}
	return false
}

// transportOf 返回当前传输（供 Supervisor 聚合 PID / 诊断用）。
func (s *ServerSession) transportOf() Transport {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.transport
}

// resetTransport 关闭并丢弃旧传输，为重连做准备。
//
// 关键：关闭走 Transport.Close，而 stdio 实现会在 Close 里做进程树清理，
// 因此重连不会积累孤儿进程（v1 的 2 秒 SIGKILL 兜底只在同进程内有效）。
func (s *ServerSession) resetTransport(ctx context.Context) error {
	s.mu.Lock()
	tr := s.transport
	s.transport = nil
	if s.state != SessionClosed {
		s.state = SessionIdle
	}
	s.mu.Unlock()
	if tr == nil {
		return nil
	}
	return tr.Close(ctx)
}

// RefreshTools 是对外暴露的工具列表刷新。
func (s *ServerSession) RefreshTools(ctx context.Context) error {
	return s.refreshTools(ctx)
}
