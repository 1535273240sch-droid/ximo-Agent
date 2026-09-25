package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/worker"
)

// ============================== 测试替身：可编程 MCP 传输 ==============================

type fakeTransport struct {
	cfg ServerConfig

	mu       sync.Mutex
	started  bool
	closed   bool
	sends    int
	handlers map[string]func(params json.RawMessage) (json.RawMessage, error)
	// startErr / pingErr / sendErr 用于注入故障。
	startErr error
	pingErr  error
	sendErr  error
	// pingCount 统计心跳次数。
	pingCount atomic.Int64
	// hang 为 true 时 Send 会阻塞直到 ctx 结束（模拟 server 卡死）。
	hang bool
	// tools 是 tools/list 返回的工具。
	tools []ToolSchema
	// pids 模拟该传输持有的外部进程。
	pids []int
}

func newFakeTransport(cfg ServerConfig) *fakeTransport {
	return &fakeTransport{cfg: cfg, handlers: map[string]func(json.RawMessage) (json.RawMessage, error){}}
}

func (f *fakeTransport) Start(ctx context.Context, cfg ServerConfig) error {
	if f.startErr != nil {
		return f.startErr
	}
	f.mu.Lock()
	f.started = true
	f.cfg = cfg
	f.mu.Unlock()
	return nil
}

func (f *fakeTransport) Send(ctx context.Context, req Request) (Response, error) {
	f.mu.Lock()
	f.sends++
	closed := f.closed
	sendErr := f.sendErr
	hang := f.hang
	h := f.handlers[req.Method]
	f.mu.Unlock()

	if closed {
		return Response{}, errors.New("transport closed")
	}
	if sendErr != nil {
		return Response{}, sendErr
	}
	if hang {
		<-ctx.Done()
		return Response{}, ctx.Err()
	}
	if h != nil {
		res, err := h(req.Params)
		if err != nil {
			return Response{JSONRPC: "2.0", ID: req.ID, Error: &RPCError{Code: -32000, Message: err.Error()}}, nil
		}
		return Response{JSONRPC: "2.0", ID: req.ID, Result: res}, nil
	}
	// 默认响应。
	return Response{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(`{}`)}, nil
}

func (f *fakeTransport) Notify(ctx context.Context, method string, params json.RawMessage) error {
	return nil
}

func (f *fakeTransport) Ping(ctx context.Context) error {
	f.pingCount.Add(1)
	if f.pingErr != nil {
		return f.pingErr
	}
	f.mu.Lock()
	closed := f.closed
	f.mu.Unlock()
	if closed {
		return errors.New("transport closed")
	}
	return nil
}

func (f *fakeTransport) Close(ctx context.Context) error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	return nil
}

func (f *fakeTransport) PIDs() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.pids...)
}

// 让 fakeTransport 能按需返回工具列表。
func (f *fakeTransport) withTools(tools ...ToolSchema) *fakeTransport {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tools = tools
	body, _ := json.Marshal(map[string]any{"tools": tools})
	f.handlers["tools/list"] = func(json.RawMessage) (json.RawMessage, error) {
		return body, nil
	}
	return f
}

func (f *fakeTransport) setHandler(method string, h func(json.RawMessage) (json.RawMessage, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers[method] = h
}

// countingFactory 记录创建的传输实例。
type countingFactory struct {
	mu    sync.Mutex
	made  []*fakeTransport
	build func(cfg ServerConfig) *fakeTransport
}

func (cf *countingFactory) factory(cfg ServerConfig) (Transport, error) {
	cf.mu.Lock()
	defer cf.mu.Unlock()
	t := newFakeTransport(cfg)
	if cf.build != nil {
		if custom := cf.build(cfg); custom != nil {
			t = custom
		}
	}
	cf.made = append(cf.made, t)
	return t, nil
}

func (cf *countingFactory) all() []*fakeTransport {
	cf.mu.Lock()
	defer cf.mu.Unlock()
	return append([]*fakeTransport(nil), cf.made...)
}

func (cf *countingFactory) count() int {
	cf.mu.Lock()
	defer cf.mu.Unlock()
	return len(cf.made)
}

// ============================== 配置解析（兼容主流客户端格式） ==============================

// TestParseConfigsClaudeStyle 验证标准 mcpServers 格式解析。
func TestParseConfigsClaudeStyle(t *testing.T) {
	data := []byte(`{
	  "mcpServers": {
	    "filesystem": {"command": "npx", "args": ["-y", "@modelcontextprotocol/server-filesystem", "/tmp"]},
	    "remote": {"url": "https://mcp.example.com/mcp", "headers": {"Authorization": "Bearer x"}}
	  }
	}`)
	cfgs, err := ParseConfigs(data)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(cfgs) != 2 {
		t.Fatalf("期望 2 个 server，实际 %d", len(cfgs))
	}
	byID := map[string]ServerConfig{}
	for _, c := range cfgs {
		byID[c.ID] = c
	}
	fs, ok := byID["filesystem"]
	if !ok {
		t.Fatal("应解析出 filesystem server")
	}
	if fs.Transport != TransportStdio || fs.Command != "npx" {
		t.Errorf("stdio 配置解析错误: %+v", fs)
	}
	if len(fs.Args) != 3 {
		t.Errorf("args 解析错误: %v", fs.Args)
	}
	rm := byID["remote"]
	if rm.Transport != TransportHTTP || rm.URL == "" {
		t.Errorf("有 url 的应识别为 http: %+v", rm)
	}
	if rm.Headers["Authorization"] != "Bearer x" {
		t.Errorf("headers 应保留: %v", rm.Headers)
	}
}

// TestParseConfigsArrayStyle 验证数组格式解析。
func TestParseConfigsArrayStyle(t *testing.T) {
	data := []byte(`[
	  {"id":"a","command":"node","args":["s.js"]},
	  {"id":"b","url":"http://x/mcp","transport":"sse"}
	]`)
	cfgs, err := ParseConfigs(data)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(cfgs) != 2 {
		t.Fatalf("期望 2 个，实际 %d", len(cfgs))
	}
	if cfgs[0].ID != "a" || cfgs[1].ID != "b" {
		t.Errorf("顺序或 ID 错误: %+v", cfgs)
	}
	if cfgs[1].Transport != TransportSSE {
		t.Errorf("显式 sse 应被保留: %s", cfgs[1].Transport)
	}
}

// TestParseConfigsServersWrapper 验证 {"servers": [...]} 包装格式。
func TestParseConfigsServersWrapper(t *testing.T) {
	data := []byte(`{"servers":[{"id":"x","command":"node"}]}`)
	cfgs, err := ParseConfigs(data)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(cfgs) != 1 || cfgs[0].ID != "x" {
		t.Errorf("servers 包装解析错误: %+v", cfgs)
	}
}

// TestParseConfigsInvalid 验证非法配置被拒绝。
func TestParseConfigsInvalid(t *testing.T) {
	for _, bad := range []string{`not json`, `123`, `{"nothing": true}`} {
		if _, err := ParseConfigs([]byte(bad)); err == nil {
			t.Errorf("非法配置 %q 应报错", bad)
		}
	}
}

// TestNormalizeIDMatchesV1Rules 验证 ID 归一化规则与 v1 一致。
func TestNormalizeIDMatchesV1Rules(t *testing.T) {
	cases := map[string]string{
		"My Server":    "my-server",
		"Filesystem!!": "filesystem",
		"a_b-c":        "a_b-c",
		"  spaced  ":   "spaced",
		"中文":           "server", // 全非 ASCII 时回退
		"UPPER":        "upper",
	}
	for in, want := range cases {
		if got := NormalizeID(in); got != want {
			t.Errorf("NormalizeID(%q)=%q want=%q", in, got, want)
		}
	}
}

// ============================== 单 server 连接与握手 ==============================

// TestSessionConnectHandshake 验证握手序列 initialize → initialized → tools/list。
func TestSessionConnectHandshake(t *testing.T) {
	var mu sync.Mutex
	var methods []string
	ft := newFakeTransport(ServerConfig{ID: "s1", Command: "node"})
	ft.setHandler("initialize", func(json.RawMessage) (json.RawMessage, error) {
		mu.Lock()
		methods = append(methods, "initialize")
		mu.Unlock()
		return json.RawMessage(`{"protocolVersion":"2024-11-05"}`), nil
	})
	ft.setHandler("tools/list", func(json.RawMessage) (json.RawMessage, error) {
		mu.Lock()
		methods = append(methods, "tools/list")
		mu.Unlock()
		return json.RawMessage(`{"tools":[{"name":"read_file","description":"read"}]}`), nil
	})

	cf := &countingFactory{build: func(ServerConfig) *fakeTransport { return ft }}
	sess := NewServerSession(ServerConfig{ID: "s1", Command: "node"}, cf.factory, nil, nil, nil)
	if err := sess.Connect(context.Background()); err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	defer sess.Close(context.Background())

	if sess.State() != SessionReady {
		t.Errorf("连接后应为 ready，实际 %s", sess.State())
	}
	mu.Lock()
	got := strings.Join(methods, ",")
	mu.Unlock()
	if !strings.Contains(got, "initialize") || !strings.Contains(got, "tools/list") {
		t.Errorf("握手方法序列不正确: %s", got)
	}
	tools := sess.Tools()
	if len(tools) != 1 {
		t.Fatalf("应有 1 个工具，实际 %d", len(tools))
	}
	// 工具名应带 mcp__ 前缀（与 v1 一致）。
	if tools[0].ExposedName != "mcp__read_file" {
		t.Errorf("暴露名应为 mcp__read_file，实际 %s", tools[0].ExposedName)
	}
	if tools[0].ServerID != "s1" {
		t.Errorf("应标注来源 server，实际 %s", tools[0].ServerID)
	}
}

// TestSessionToolFiltering 验证 AllowedTools/DeniedTools 过滤生效（权限收敛）。
func TestSessionToolFiltering(t *testing.T) {
	ft := newFakeTransport(ServerConfig{ID: "s1", Command: "node"}).withTools(
		ToolSchema{Name: "keep"},
		ToolSchema{Name: "drop_by_deny"},
		ToolSchema{Name: "drop_by_allow"},
	)
	cf := &countingFactory{build: func(ServerConfig) *fakeTransport { return ft }}
	sess := NewServerSession(ServerConfig{
		ID: "s1", Command: "node",
		AllowedTools: []string{"keep", "drop_by_deny"},
		DeniedTools:  []string{"drop_by_deny"},
	}, cf.factory, nil, nil, nil)
	if err := sess.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer sess.Close(context.Background())

	tools := sess.Tools()
	if len(tools) != 1 || tools[0].Name != "keep" {
		t.Errorf("过滤后应只剩 keep，实际 %+v", tools)
	}
}

// ============================== 独立故障域（第 17 章核心） ==============================

// TestOneServerFailureDoesNotAffectOthers 验证一个 server 失败不影响其他 server。
//
// 这是任务书"每个 MCP server 独立故障域"的直接验收。
func TestOneServerFailureDoesNotAffectOthers(t *testing.T) {
	good := newFakeTransport(ServerConfig{ID: "good", Command: "node"}).withTools(
		ToolSchema{Name: "good_tool"},
	)
	bad := newFakeTransport(ServerConfig{ID: "bad", Command: "node"})
	bad.startErr = errors.New("server 起不来")

	cf := &countingFactory{build: func(cfg ServerConfig) *fakeTransport {
		if cfg.ID == "bad" {
			return bad
		}
		return good
	}}

	sup := NewSupervisor("mcp-0", SupervisorConfig{
		Servers: []ServerConfig{
			{ID: "good", Command: "node", Enabled: true},
			{ID: "bad", Command: "node", Enabled: true},
		},
		Factory: cf.factory,
	}, nil, nil, nil)

	if err := sup.Start(context.Background()); err != nil {
		t.Fatalf("Supervisor.Start 失败: %v", err)
	}
	defer sup.Stop(context.Background())

	snaps := sup.Snapshot()
	stateByID := map[string]SessionState{}
	for _, s := range snaps {
		stateByID[s.ID] = s.State
	}
	if stateByID["good"] != SessionReady {
		t.Errorf("正常的 server 应为 ready，实际 %s", stateByID["good"])
	}
	if stateByID["bad"] == SessionReady {
		t.Error("启动失败的 server 不应为 ready")
	}
	// 关键断言：整体仍然健康（至少一个就绪），且 good 的工具仍可枚举。
	if err := sup.Health(context.Background()); err != nil {
		t.Errorf("有一个 server 挂掉时整体仍应健康: %v", err)
	}
	tools := sup.AllTools()
	found := false
	for _, tl := range tools {
		if tl.Name == "good_tool" {
			found = true
		}
	}
	if !found {
		t.Error("正常 server 的工具应仍可枚举（故障域隔离）")
	}
}

// TestDisabledServerIsSkipped 验证 disabled 的 server 不建立会话（feature flag 语义）。
func TestDisabledServerIsSkipped(t *testing.T) {
	cf := &countingFactory{}
	sup := NewSupervisor("mcp-0", SupervisorConfig{
		Servers: []ServerConfig{
			{ID: "on", Command: "node", Enabled: true},
			{ID: "off", Command: "node", Enabled: false},
		},
		Factory: cf.factory,
	}, nil, nil, nil)
	if err := sup.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer sup.Stop(context.Background())

	for _, s := range sup.Snapshot() {
		if s.ID == "off" {
			t.Error("disabled 的 server 不应出现在会话列表中")
		}
	}
	// 只应为启用的 server 创建传输。
	if cf.count() != 1 {
		t.Errorf("只应为 1 个启用的 server 创建传输，实际 %d", cf.count())
	}
}

// ============================== 请求超时与 pending 表 ==============================

// TestRequestTimeoutFailsFast 验证卡死的 server 让调用在超时后失败（不会永久挂住）。
func TestRequestTimeoutFailsFast(t *testing.T) {
	ft := newFakeTransport(ServerConfig{ID: "s1", Command: "node"})
	ft.mu.Lock()
	ft.hang = true
	ft.mu.Unlock()

	cf := &countingFactory{build: func(ServerConfig) *fakeTransport { return ft }}
	sess := NewServerSession(ServerConfig{
		ID: "s1", Command: "node", RequestTimeout: 300 * time.Millisecond,
	}, cf.factory, nil, nil, nil)

	// 直接建立传输但不走完整握手（把 hang 只作用于业务调用）。
	tr, _ := cf.factory(sess.Config())
	_ = tr.Start(context.Background(), sess.Config())
	sess.mu.Lock()
	sess.transport = tr
	sess.state = SessionReady
	sess.mu.Unlock()

	start := time.Now()
	_, err := sess.Call(context.Background(), "tools/call", json.RawMessage(`{}`))
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("卡死的 server 应返回超时错误")
	}
	if !errors.Is(err, worker.ErrTimeout) {
		t.Errorf("期望 ErrTimeout，实际 %v", err)
	}
	if elapsed > 3*time.Second {
		t.Errorf("应在 RequestTimeout 内返回，实际耗时 %s", elapsed)
	}
	// pending 表必须被清空（不能泄漏条目）。
	sess.mu.Lock()
	pending := len(sess.pending)
	sess.mu.Unlock()
	if pending != 0 {
		t.Errorf("超时后 pending 表应清空，实际残留 %d", pending)
	}
}

// TestCircuitOpensAfterRepeatedFailures 验证该 server 反复失败后熔断打开。
func TestCircuitOpensAfterRepeatedFailures(t *testing.T) {
	opened := make(chan worker.WorkerEvent, 4)
	ft := newFakeTransport(ServerConfig{ID: "s1", Command: "node"})
	ft.mu.Lock()
	ft.sendErr = errors.New("server 一直失败")
	ft.mu.Unlock()

	cf := &countingFactory{build: func(ServerConfig) *fakeTransport { return ft }}
	sess := NewServerSession(ServerConfig{
		ID: "s1", Command: "node",
		RequestTimeout: time.Second, CircuitFailures: 3, CircuitCooldown: 50 * time.Millisecond,
	}, cf.factory, nil, nil, sinkFunc(func(ev worker.WorkerEvent) {
		if ev.Type == worker.EventCircuitOpened {
			select {
			case opened <- ev:
			default:
			}
		}
	}))

	tr, _ := cf.factory(sess.Config())
	_ = tr.Start(context.Background(), sess.Config())
	sess.mu.Lock()
	sess.transport = tr
	sess.state = SessionReady
	sess.mu.Unlock()

	for i := 0; i < 5; i++ {
		_, _ = sess.Call(context.Background(), "tools/call", nil)
	}

	select {
	case <-opened:
		// 打开后应快速失败，不再打外部依赖。
		_, err := sess.Call(context.Background(), "tools/call", nil)
		if err == nil {
			t.Error("熔断打开后调用应快速失败")
		}
		if !errors.Is(err, worker.ErrUnavailable) {
			t.Errorf("期望 ErrUnavailable，实际 %v", err)
		}
	default:
		t.Fatal("连续失败后应触发熔断")
	}
}

// TestCircuitHalfOpenAfterCooldown 验证冷却后进入 half-open 并允许探测。
func TestCircuitHalfOpenAfterCooldown(t *testing.T) {
	clock := worker.NewManualClock(time.Now())
	cb := worker.NewCircuitBreaker(worker.CircuitConfig{
		FailureThreshold: 2, Cooldown: 10 * time.Second, SuccessThreshold: 1,
	}, clock, nil)
	cb.RecordFailure("a")
	cb.RecordFailure("b")
	if cb.State() != worker.CircuitOpen {
		t.Fatalf("应为 open，实际 %s", cb.State())
	}
	clock.Advance(11 * time.Second)
	if cb.State() != worker.CircuitHalfOpen {
		t.Errorf("冷却后应为 half-open，实际 %s", cb.State())
	}
	if !cb.Allow() {
		t.Error("half-open 应放行探测")
	}
	cb.RecordSuccess()
	if cb.State() != worker.CircuitClosed {
		t.Errorf("探测成功应闭合，实际 %s", cb.State())
	}
}

// ============================== 心跳 ==============================

// TestHeartbeatRunsPerServer 验证每个 server 有独立心跳，且 Stop 后停止。
func TestHeartbeatRunsPerServer(t *testing.T) {
	clock := worker.NewManualClock(time.Now())
	ft1 := newFakeTransport(ServerConfig{ID: "s1"}).withTools(ToolSchema{Name: "t1"})
	ft2 := newFakeTransport(ServerConfig{ID: "s2"}).withTools(ToolSchema{Name: "t2"})
	cf := &countingFactory{build: func(cfg ServerConfig) *fakeTransport {
		if cfg.ID == "s1" {
			return ft1
		}
		return ft2
	}}

	s1 := NewServerSession(ServerConfig{ID: "s1", Command: "node"}, cf.factory, clock, nil, nil)
	s2 := NewServerSession(ServerConfig{ID: "s2", Command: "node"}, cf.factory, clock, nil, nil)
	if err := s1.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s2.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}

	// 推进时钟触发心跳。
	clock.Advance(31 * time.Second)
	time.Sleep(50 * time.Millisecond)

	if ft1.pingCount.Load() == 0 {
		t.Error("s1 应有心跳")
	}
	if ft2.pingCount.Load() == 0 {
		t.Error("s2 应有独立心跳")
	}

	// 关闭后心跳必须停止（goroutine 有退出路径）。
	_ = s1.Close(context.Background())
	before := ft1.pingCount.Load()
	clock.Advance(60 * time.Second)
	time.Sleep(50 * time.Millisecond)
	if ft1.pingCount.Load() != before {
		t.Error("关闭后心跳应停止")
	}
	_ = s2.Close(context.Background())
}

// ============================== 重连退避 ==============================

// TestRestartBackoffSchedule 验证重连退避使用 1s→2s→4s→8s→16s→30s 封顶。
func TestRestartBackoffSchedule(t *testing.T) {
	sup := NewSupervisor("mcp-0", SupervisorConfig{}, nil, nil, nil)
	want := []time.Duration{
		1 * time.Second, 2 * time.Second, 4 * time.Second,
		8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second,
	}
	for i, w0 := range want {
		got := sup.restartBackoff(i + 1)
		// 有 10% 抖动，做区间断言。
		lo := time.Duration(float64(w0) * 0.85)
		hi := time.Duration(float64(w0) * 1.15)
		if got < lo || got > hi {
			t.Errorf("第 %d 次退避 %s 不在 [%s, %s] 内", i+1, got, lo, hi)
		}
	}
}

// ============================== 动作分发 ==============================

// TestExecuteToolsCall 验证 tools/call 被路由到正确的 server。
func TestExecuteToolsCall(t *testing.T) {
	ft := newFakeTransport(ServerConfig{ID: "s1", Command: "node"}).withTools(
		ToolSchema{Name: "echo"},
	)
	ft.setHandler("tools/call", func(params json.RawMessage) (json.RawMessage, error) {
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		_ = json.Unmarshal(params, &p)
		return json.RawMessage(fmt.Sprintf(`{"echoed":%q}`, p.Name)), nil
	})
	cf := &countingFactory{build: func(ServerConfig) *fakeTransport { return ft }}

	sup := NewSupervisor("mcp-0", SupervisorConfig{
		Servers: []ServerConfig{{ID: "s1", Command: "node", Enabled: true}},
		Factory: cf.factory,
	}, nil, nil, nil)
	if err := sup.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer sup.Stop(context.Background())

	args, _ := json.Marshal(map[string]any{"tool": "echo", "arguments": map[string]any{"m": "hi"}})
	resp, err := sup.Execute(context.Background(), worker.WorkerRequest{
		CallID: "c1", Action: ActionToolsCall, Args: args,
	})
	if err != nil {
		t.Fatalf("Execute 错误: %v", err)
	}
	if resp.Status != worker.StatusOK {
		t.Fatalf("期望成功，实际 %s (%v)", resp.Status, resp.Error)
	}
	if !strings.Contains(string(resp.Result), "echo") {
		t.Errorf("结果应包含工具名: %s", resp.Result)
	}
}

// TestExecuteToolsList 验证工具列表聚合。
func TestExecuteToolsList(t *testing.T) {
	ft := newFakeTransport(ServerConfig{ID: "s1"}).withTools(
		ToolSchema{Name: "a"}, ToolSchema{Name: "b"},
	)
	cf := &countingFactory{build: func(ServerConfig) *fakeTransport { return ft }}
	sup := NewSupervisor("mcp-0", SupervisorConfig{
		Servers: []ServerConfig{{ID: "s1", Command: "node", Enabled: true}},
		Factory: cf.factory,
	}, nil, nil, nil)
	_ = sup.Start(context.Background())
	defer sup.Stop(context.Background())

	resp, _ := sup.Execute(context.Background(), worker.WorkerRequest{
		CallID: "c1", Action: ActionToolsList,
	})
	var body struct {
		Count int `json:"count"`
	}
	_ = json.Unmarshal(resp.Result, &body)
	if body.Count != 2 {
		t.Errorf("应有 2 个工具，实际 %d", body.Count)
	}
}

// TestExecuteUnknownToolFails 验证未知工具给出明确错误。
func TestExecuteUnknownToolFails(t *testing.T) {
	ft := newFakeTransport(ServerConfig{ID: "s1"}).withTools(ToolSchema{Name: "known"})
	cf := &countingFactory{build: func(ServerConfig) *fakeTransport { return ft }}
	sup := NewSupervisor("mcp-0", SupervisorConfig{
		Servers: []ServerConfig{{ID: "s1", Command: "node", Enabled: true}},
		Factory: cf.factory,
	}, nil, nil, nil)
	_ = sup.Start(context.Background())
	defer sup.Stop(context.Background())

	args, _ := json.Marshal(map[string]any{"tool": "no_such_tool"})
	resp, _ := sup.Execute(context.Background(), worker.WorkerRequest{
		CallID: "c1", Action: ActionToolsCall, Args: args,
	})
	if resp.Error == nil || resp.Error.Code != worker.CodeUnavailable {
		t.Errorf("未知工具应返回 unavailable，实际 %+v", resp.Error)
	}
}

// TestExecuteUnknownActionRejected 验证未知动作被拒绝。
func TestExecuteUnknownActionRejected(t *testing.T) {
	sup := NewSupervisor("mcp-0", SupervisorConfig{}, nil, nil, nil)
	resp, _ := sup.Execute(context.Background(), worker.WorkerRequest{CallID: "c", Action: "bogus"})
	if resp.Error == nil || resp.Error.Code != worker.CodeInvalidArgument {
		t.Errorf("未知动作应返回 invalid_argument，实际 %+v", resp.Error)
	}
}

// TestExecutePingAll 验证 ping 动作探测所有 server。
func TestExecutePingAll(t *testing.T) {
	ft1 := newFakeTransport(ServerConfig{ID: "s1"}).withTools(ToolSchema{Name: "t"})
	ft2 := newFakeTransport(ServerConfig{ID: "s2"}).withTools(ToolSchema{Name: "t"})
	cf := &countingFactory{build: func(cfg ServerConfig) *fakeTransport {
		if cfg.ID == "s1" {
			return ft1
		}
		return ft2
	}}
	sup := NewSupervisor("mcp-0", SupervisorConfig{
		Servers: []ServerConfig{
			{ID: "s1", Command: "node", Enabled: true},
			{ID: "s2", Command: "node", Enabled: true},
		},
		Factory: cf.factory,
	}, nil, nil, nil)
	_ = sup.Start(context.Background())
	defer sup.Stop(context.Background())

	resp, _ := sup.Execute(context.Background(), worker.WorkerRequest{CallID: "c", Action: ActionPing})
	var body struct {
		Results map[string]string `json:"results"`
	}
	_ = json.Unmarshal(resp.Result, &body)
	if len(body.Results) != 2 {
		t.Errorf("应探测到 2 个 server，实际 %d", len(body.Results))
	}
}

// ============================== 生命周期 ==============================

// TestStopClosesAllSessions 验证 Stop 关闭所有会话且幂等。
func TestStopClosesAllSessions(t *testing.T) {
	cf := &countingFactory{}
	sup := NewSupervisor("mcp-0", SupervisorConfig{
		Servers: []ServerConfig{
			{ID: "s1", Command: "node", Enabled: true},
			{ID: "s2", Command: "node", Enabled: true},
		},
		Factory: cf.factory,
	}, nil, nil, nil)
	_ = sup.Start(context.Background())

	if err := sup.Stop(context.Background()); err != nil {
		t.Fatalf("Stop 失败: %v", err)
	}
	for i, tr := range cf.all() {
		tr.mu.Lock()
		closed := tr.closed
		tr.mu.Unlock()
		if !closed {
			t.Errorf("第 %d 个传输未被关闭", i)
		}
	}
	// 幂等。
	if err := sup.Stop(context.Background()); err != nil {
		t.Errorf("重复 Stop 应幂等: %v", err)
	}
}

// TestPIDsAggregatedForCleanup 验证 PIDs 聚合 stdio 子进程（供进程树清理）。
func TestPIDsAggregatedForCleanup(t *testing.T) {
	ft := newFakeTransport(ServerConfig{ID: "s1"}).withTools(ToolSchema{Name: "t"})
	ft.pids = []int{1234, 5678}
	cf := &countingFactory{build: func(ServerConfig) *fakeTransport { return ft }}
	sup := NewSupervisor("mcp-0", SupervisorConfig{
		Servers: []ServerConfig{{ID: "s1", Command: "node", Enabled: true}},
		Factory: cf.factory,
	}, nil, nil, nil)
	_ = sup.Start(context.Background())
	defer sup.Stop(context.Background())

	pids := sup.PIDs()
	if len(pids) != 2 {
		t.Errorf("应聚合 2 个 PID，实际 %v", pids)
	}
}

// TestSupervisorKindAndID 验证身份方法。
func TestSupervisorKindAndID(t *testing.T) {
	sup := NewSupervisor("mcp-7", SupervisorConfig{}, nil, nil, nil)
	if sup.ID() != "mcp-7" {
		t.Errorf("ID 错误: %s", sup.ID())
	}
	if sup.Kind() != "mcp" {
		t.Errorf("Kind 错误: %s", sup.Kind())
	}
}

// TestHealthWithNoServers 验证没有配置 server 时健康（能力未启用不等于故障）。
func TestHealthWithNoServers(t *testing.T) {
	sup := NewSupervisor("mcp-0", SupervisorConfig{}, nil, nil, nil)
	if err := sup.Health(context.Background()); err != nil {
		t.Errorf("无 server 配置时应视为健康: %v", err)
	}
}

// TestHealthFailsWhenAllServersDown 验证全部 server 不可用时健康检查报错。
func TestHealthFailsWhenAllServersDown(t *testing.T) {
	ft := newFakeTransport(ServerConfig{ID: "s1"})
	ft.startErr = errors.New("起不来")
	cf := &countingFactory{build: func(ServerConfig) *fakeTransport { return ft }}
	sup := NewSupervisor("mcp-0", SupervisorConfig{
		Servers: []ServerConfig{{ID: "s1", Command: "node", Enabled: true}},
		Factory: cf.factory,
	}, nil, nil, nil)
	_ = sup.Start(context.Background())
	defer sup.Stop(context.Background())

	if err := sup.Health(context.Background()); err == nil {
		t.Error("所有 server 不可用时健康检查应报错")
	}
}

// TestSupervisorConnectionInterruptedFailsPending 验证连接中断让等待中的调用立即失败。
func TestSupervisorConnectionInterruptedFailsPending(t *testing.T) {
	ft := newFakeTransport(ServerConfig{ID: "s1"})
	cf := &countingFactory{build: func(ServerConfig) *fakeTransport { return ft }}
	sess := NewServerSession(ServerConfig{
		ID: "s1", Command: "node", RequestTimeout: 5 * time.Second,
	}, cf.factory, nil, nil, nil)

	tr, _ := cf.factory(sess.Config())
	_ = tr.Start(context.Background(), sess.Config())
	sess.mu.Lock()
	sess.transport = tr
	sess.state = SessionReady
	sess.mu.Unlock()

	// 关闭传输后调用应失败（而不是等到超时）。
	_ = sess.resetTransport(context.Background())
	_, err := sess.Call(context.Background(), "tools/call", nil)
	if err == nil {
		t.Fatal("传输关闭后调用应失败")
	}
}

// sinkFunc 把函数适配成 worker.EventSink。
type sinkFunc func(worker.WorkerEvent)

func (f sinkFunc) Emit(ev worker.WorkerEvent) { f(ev) }

// TestDefaultTransportFactorySelectsByKind 验证内置工厂按 transport 选实现。
func TestDefaultTransportFactorySelectsByKind(t *testing.T) {
	stdio, err := DefaultTransportFactory(ServerConfig{Transport: TransportStdio, Command: "node"})
	if err != nil {
		t.Fatalf("stdio 工厂失败: %v", err)
	}
	if _, ok := stdio.(*StdioTransport); !ok {
		t.Errorf("stdio 应返回 StdioTransport，实际 %T", stdio)
	}
	http_, err := DefaultTransportFactory(ServerConfig{Transport: TransportHTTP, URL: "http://x"})
	if err != nil {
		t.Fatalf("http 工厂失败: %v", err)
	}
	if _, ok := http_.(*HTTPTransport); !ok {
		t.Errorf("http 应返回 HTTPTransport，实际 %T", http_)
	}
	if _, err := DefaultTransportFactory(ServerConfig{Transport: "bogus"}); err == nil {
		t.Error("未知 transport 应报错")
	}
}
