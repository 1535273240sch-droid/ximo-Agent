package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/worker"
	"github.com/ximo888ok-netizen/ximo-agent/internal/worker/procguard"
)

// ============================== stdio 传输 ==============================

// StdioTransport 通过子进程 stdin/stdout 以行分隔 JSON 通信。
//
// 相对 v1 的加固点：
//   - 经 procguard 拉起，Windows Job Object / Unix 进程组保证 server 进程树被彻底清理
//     （v1 只在 Close 时 kill 主进程 + 2s 后 SIGKILL，Node 子进程的孙子进程会漏）；
//   - 输出经限流缓冲，一个刷日志的 server 不会打爆宿主内存；
//   - 环境变量按最小化 + 密钥剔除构造（第 21 章），只合并配置里显式给出的 env。
type StdioTransport struct {
	cfg ServerConfig

	mu     sync.Mutex
	proc   *procguard.Proc
	stdin  io.WriteCloser
	enc    *json.Encoder
	closed bool

	// 响应分发：ID → channel。stdout 读取协程按 ID 投递。
	respMu   sync.Mutex
	respChan map[int64]chan Response
	// notifications 是 server → client 的通知（无需响应）。
	onNotify func(method string, params json.RawMessage)

	readErr   error
	readDone  chan struct{}
	startedAt time.Time
}

// NewStdioTransport 创建 stdio 传输。
func NewStdioTransport() *StdioTransport {
	return &StdioTransport{
		respChan: map[int64]chan Response{},
		readDone: make(chan struct{}),
	}
}

// Start 实现 Transport：拉起 MCP server 子进程。
func (t *StdioTransport) Start(ctx context.Context, cfg ServerConfig) error {
	if strings.TrimSpace(cfg.Command) == "" {
		return fmt.Errorf("%w: stdio server 缺少 command", worker.ErrInvalidArgument)
	}

	// 环境变量：最小化基座 + 配置显式给出的 env（密钥剔除由 procguard.MinimalEnv
	// + 这里的过滤共同保证）。
	env := procguard.MinimalEnv()
	deny := defaultEnvDenyKeys()
	for k, v := range cfg.Env {
		if envKeyDenied(k, deny) {
			// 第 21 章：即使是配置里写的，密钥类键也不注入子进程环境。
			continue
		}
		env = append(env, k+"="+v)
	}

	pc := procguard.Config{
		Path:           cfg.Command,
		Args:           cfg.Args,
		Dir:            cfg.WorkingDir,
		Env:            env,
		MaxOutputBytes: 2 << 20, // MCP 响应可能较大；2 MiB / 流上限
		KillGrace:      2 * time.Second,
		HideWindow:     true,
		CreateNoWindow: true,
		MaxProcesses:   16,
		// 交互模式：stdin/stdout 用于 JSON-RPC 长连接，stderr 仍走限流缓冲。
		// 进程树管控（Job Object / 进程组）在此模式下同样生效。
		Interactive: true,
	}
	p, err := procguard.Start(ctx, pc)
	if err != nil {
		return fmt.Errorf("%w: 启动 MCP server %q 失败: %v", worker.ErrUpstream, cfg.Name, err)
	}

	stdin, err := p.StdinPipe()
	if err != nil {
		_ = p.Kill(context.Background())
		_ = p.Close()
		return fmt.Errorf("%w: 获取 MCP server stdin 失败: %v", worker.ErrUpstream, err)
	}
	stdout, err := p.StdoutPipe()
	if err != nil {
		_ = p.Kill(context.Background())
		_ = p.Close()
		return fmt.Errorf("%w: 获取 MCP server stdout 失败: %v", worker.ErrUpstream, err)
	}

	t.mu.Lock()
	t.cfg = cfg
	t.proc = p
	t.stdin = stdin
	t.enc = json.NewEncoder(stdin)
	t.startedAt = time.Now()
	t.mu.Unlock()

	// 读取协程：按行解析 JSON-RPC 消息并分发。
	go t.readLoop(stdout)
	return nil
}

// readLoop 逐行读取 server 输出。
func (t *StdioTransport) readLoop(r io.Reader) {
	defer close(t.readDone)
	sc := bufio.NewScanner(r)
	// MCP 响应可能超过默认 64KiB 行上限，放大到 8 MiB（与帧 payload 上限一致）。
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var probe struct {
			ID     *int64          `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
			Error  *RPCError       `json:"error"`
		}
		if err := json.Unmarshal(line, &probe); err != nil {
			// 非 JSON 行是 server 的日志输出（很多 server 往 stdout 打日志）。
			// 忽略而不报错，避免把日志噪音当成协议错误。
			continue
		}
		if probe.ID == nil {
			// 通知（server → client）。
			if probe.Method != "" && t.onNotify != nil {
				t.onNotify(probe.Method, nil)
			}
			continue
		}
		resp := Response{JSONRPC: "2.0", ID: *probe.ID, Result: probe.Result, Error: probe.Error}
		t.respMu.Lock()
		ch := t.respChan[*probe.ID]
		delete(t.respChan, *probe.ID)
		t.respMu.Unlock()
		if ch != nil {
			select {
			case ch <- resp:
			default:
			}
		}
	}
	err := sc.Err()
	t.mu.Lock()
	t.readErr = err
	t.mu.Unlock()
	// 进程退出/读失败：让所有等待中的请求立即失败，而不是等超时。
	t.failAllPending(fmt.Errorf("%w: MCP server 连接中断", worker.ErrUpstream))
}

// failAllPending 让所有 pending 请求立即返回错误（对齐 v1 的 exit 处理，但做全）。
func (t *StdioTransport) failAllPending(err error) {
	t.respMu.Lock()
	chans := make([]chan Response, 0, len(t.respChan))
	for id, ch := range t.respChan {
		chans = append(chans, ch)
		delete(t.respChan, id)
	}
	t.respMu.Unlock()
	for _, ch := range chans {
		select {
		case ch <- Response{Error: &RPCError{Code: -32000, Message: err.Error()}}:
		default:
		}
	}
}

// Send 实现 Transport。
func (t *StdioTransport) Send(ctx context.Context, req Request) (Response, error) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return Response{}, fmt.Errorf("%w: stdio 传输已关闭", worker.ErrUnavailable)
	}
	enc := t.enc
	t.mu.Unlock()
	if enc == nil {
		return Response{}, fmt.Errorf("%w: stdio 传输未启动", worker.ErrUnavailable)
	}

	ch := make(chan Response, 1)
	t.respMu.Lock()
	t.respChan[req.ID] = ch
	t.respMu.Unlock()

	defer func() {
		t.respMu.Lock()
		delete(t.respChan, req.ID)
		t.respMu.Unlock()
	}()

	t.mu.Lock()
	err := enc.Encode(req)
	t.mu.Unlock()
	if err != nil {
		return Response{}, fmt.Errorf("%w: 写入 MCP 请求失败: %v", worker.ErrUpstream, err)
	}

	select {
	case resp := <-ch:
		return resp, nil
	case <-ctx.Done():
		return Response{}, ctx.Err()
	}
}

// Notify 实现 Transport。
func (t *StdioTransport) Notify(ctx context.Context, method string, params json.RawMessage) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.enc == nil || t.closed {
		return fmt.Errorf("%w: stdio 传输不可用", worker.ErrUnavailable)
	}
	msg := Request{JSONRPC: "2.0", Method: method, Params: params}
	return t.enc.Encode(msg)
}

// Ping 实现 Transport。stdio 传输的"心跳"是确认子进程仍存活。
func (t *StdioTransport) Ping(ctx context.Context) error {
	t.mu.Lock()
	p := t.proc
	closed := t.closed
	readErr := t.readErr
	t.mu.Unlock()
	if closed {
		return fmt.Errorf("%w: stdio 传输已关闭", worker.ErrUnavailable)
	}
	if p == nil {
		return fmt.Errorf("%w: stdio 传输未启动", worker.ErrUnavailable)
	}
	if readErr != nil {
		return fmt.Errorf("%w: MCP server 输出中断: %v", worker.ErrUpstream, readErr)
	}
	select {
	case <-t.readDone:
		return fmt.Errorf("%w: MCP server 已退出", worker.ErrUpstream)
	default:
	}
	if p.Exited() {
		return fmt.Errorf("%w: MCP server 进程已退出", worker.ErrUpstream)
	}
	return nil
}

// Close 实现 Transport：清 pending → 关 stdin → 进程树清理 → 释放句柄。
func (t *StdioTransport) Close(ctx context.Context) error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	stdin := t.stdin
	p := t.proc
	t.stdin = nil
	t.enc = nil
	t.mu.Unlock()

	t.failAllPending(fmt.Errorf("%w: stdio 传输已关闭", worker.ErrUnavailable))

	// 友好关闭：先关 stdin 让 server 自己退出（MCP 约定）。
	if stdin != nil {
		_ = stdin.Close()
	}
	if p != nil {
		// 等一小会儿让它优雅退出，再强杀整棵进程树。
		waitCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		done := make(chan struct{})
		go func() {
			_, _ = p.Wait(waitCtx)
			close(done)
		}()
		select {
		case <-done:
		case <-waitCtx.Done():
		}
		cancel()
		_ = p.Kill(ctx)
		_ = p.Close()
	}
	return nil
}

// PIDs 实现 Transport。
func (t *StdioTransport) PIDs() []int {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.proc == nil {
		return nil
	}
	pid := t.proc.PID()
	if pid <= 0 {
		return nil
	}
	return []int{pid}
}

// ============================== HTTP / SSE 传输 ==============================

// HTTPTransport 通过 HTTP POST 发送 JSON-RPC（MCP 的 streamable HTTP 约定）。
//
// 如实说明：v1 的 "sse" 传输实际上也是纯 POST（没有维护 SSE 长连接），
// 因此这里对 sse 与 http 采用同一实现路径**但明确标注**：真正的 SSE
// 事件流订阅列为待办，而不是假装支持。SSE 模式下的差异是会带 Accept 头声明
// 可接受 text/event-stream，并对响应做 SSE 解帧尝试。
type HTTPTransport struct {
	cfg    ServerConfig
	client *http.Client
	mu     sync.Mutex
	nextID int64
	closed bool
}

// NewHTTPTransport 创建 HTTP 传输。
func NewHTTPTransport() *HTTPTransport {
	return &HTTPTransport{
		client: &http.Client{Timeout: 0}, // 超时由 ctx 控制
	}
}

// Start 实现 Transport。
func (t *HTTPTransport) Start(ctx context.Context, cfg ServerConfig) error {
	if strings.TrimSpace(cfg.URL) == "" {
		return fmt.Errorf("%w: HTTP MCP server 缺少 url", worker.ErrInvalidArgument)
	}
	t.mu.Lock()
	t.cfg = cfg
	t.closed = false
	t.mu.Unlock()
	return nil
}

// Send 实现 Transport。
func (t *HTTPTransport) Send(ctx context.Context, req Request) (Response, error) {
	t.mu.Lock()
	cfg := t.cfg
	closed := t.closed
	t.mu.Unlock()
	if closed {
		return Response{}, fmt.Errorf("%w: HTTP 传输已关闭", worker.ErrUnavailable)
	}

	body, err := json.Marshal(req)
	if err != nil {
		return Response{}, fmt.Errorf("%w: 编码请求失败: %v", worker.ErrInternal, err)
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.URL, bytes.NewReader(body))
	if err != nil {
		return Response{}, fmt.Errorf("%w: 构造请求失败: %v", worker.ErrInvalidArgument, err)
	}
	hreq.Header.Set("Content-Type", "application/json")
	if cfg.Transport == TransportSSE {
		hreq.Header.Set("Accept", "application/json, text/event-stream")
	} else {
		hreq.Header.Set("Accept", "application/json")
	}
	for k, v := range cfg.Headers {
		// 认证头等由配置提供；但禁止把已知密钥头写进日志（本函数不记日志）。
		hreq.Header.Set(k, v)
	}

	resp, err := t.client.Do(hreq)
	if err != nil {
		return Response{}, fmt.Errorf("%w: 请求 MCP server 失败: %v", worker.ErrUpstream, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return Response{}, fmt.Errorf("%w: 读取响应失败: %v", worker.ErrUpstream, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Response{}, fmt.Errorf("%w: MCP server 返回 HTTP %d: %s",
			worker.ErrUpstream, resp.StatusCode, truncate(string(raw), 500))
	}

	// SSE 模式：响应可能是事件流，需要解帧。
	contentType := resp.Header.Get("Content-Type")
	if strings.Contains(contentType, "text/event-stream") {
		data, err := extractSSEData(raw)
		if err != nil {
			return Response{}, err
		}
		raw = data
	}
	var out Response
	if err := json.Unmarshal(bytes.TrimSpace(raw), &out); err != nil {
		return Response{}, fmt.Errorf("%w: 解析 MCP 响应失败: %v", worker.ErrUpstream, err)
	}
	return out, nil
}

// extractSSEData 从 SSE 事件流中取出最后一条 data 行（JSON-RPC 响应所在处）。
func extractSSEData(raw []byte) ([]byte, error) {
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	var last []byte
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "data:") {
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload != "" && payload != "[DONE]" {
				last = []byte(payload)
			}
		}
	}
	if last == nil {
		return nil, fmt.Errorf("%w: SSE 响应中没有 data 字段", worker.ErrUpstream)
	}
	return last, nil
}

// Notify 实现 Transport：通知也走 POST，但不等待业务响应。
func (t *HTTPTransport) Notify(ctx context.Context, method string, params json.RawMessage) error {
	req := Request{JSONRPC: "2.0", Method: method, Params: params}
	// 复用 Send 的编码路径；通知通常返回 202/204，因此忽略响应体解析错误。
	_, err := t.sendRaw(ctx, req)
	return err
}

// sendRaw 发送但不解析 JSON-RPC 响应（用于通知）。
func (t *HTTPTransport) sendRaw(ctx context.Context, req Request) ([]byte, error) {
	t.mu.Lock()
	cfg := t.cfg
	closed := t.closed
	t.mu.Unlock()
	if closed {
		return nil, fmt.Errorf("%w: HTTP 传输已关闭", worker.ErrUnavailable)
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("%w: 编码通知失败: %v", worker.ErrInternal, err)
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.URL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%w: 构造请求失败: %v", worker.ErrInvalidArgument, err)
	}
	hreq.Header.Set("Content-Type", "application/json")
	for k, v := range cfg.Headers {
		hreq.Header.Set(k, v)
	}
	resp, err := t.client.Do(hreq)
	if err != nil {
		return nil, fmt.Errorf("%w: 发送通知失败: %v", worker.ErrUpstream, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("%w: 通知返回 HTTP %d", worker.ErrUpstream, resp.StatusCode)
	}
	return raw, nil
}

// Ping 实现 Transport：HTTP 传输用心跳请求探测可达性。
// 这里不做额外的网络往返（避免给 server 增加无谓负载），只检查配置与关闭状态；
// 真正的可达性由心跳时的 tools/list 或显式 Ping 动作触发。
func (t *HTTPTransport) Ping(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return fmt.Errorf("%w: HTTP 传输已关闭", worker.ErrUnavailable)
	}
	return nil
}

// Close 实现 Transport。
func (t *HTTPTransport) Close(ctx context.Context) error {
	t.mu.Lock()
	t.closed = true
	t.mu.Unlock()
	t.client.CloseIdleConnections()
	return nil
}

// PIDs 实现 Transport：HTTP 传输没有本地子进程。
func (t *HTTPTransport) PIDs() []int { return nil }

// ============================== 默认工厂 ==============================

// DefaultTransportFactory 按 transport 类型选择内置传输实现。
//
// 集成期可由 08 号注入自定义工厂（例如带认证刷新、mTLS 的实现）。
func DefaultTransportFactory(cfg ServerConfig) (Transport, error) {
	switch cfg.Transport {
	case TransportStdio, "":
		return NewStdioTransport(), nil
	case TransportHTTP, TransportSSE:
		return NewHTTPTransport(), nil
	default:
		return nil, fmt.Errorf("%w: 不支持的 MCP 传输类型 %q", worker.ErrInvalidArgument, cfg.Transport)
	}
}

// envKeyDenied 判断环境变量键是否命中密钥类拒绝模式。
func envKeyDenied(key string, patterns []string) bool {
	up := strings.ToUpper(key)
	for _, p := range patterns {
		if globMatch(strings.ToUpper(p), up) {
			return true
		}
	}
	return false
}

// defaultEnvDenyKeys 与 terminal 包保持同一套模式（两处必须一致，故集中在此）。
func defaultEnvDenyKeys() []string {
	return []string{
		"*KEY*", "*TOKEN*", "*SECRET*", "*PASSWORD*", "*PASSWD*", "*CREDENTIAL*",
		"*APIKEY*", "AWS_*", "AZURE_*", "GCP_*", "OPENAI_*", "ANTHROPIC_*",
		"DEEPSEEK_*", "GITHUB_TOKEN", "GH_TOKEN", "NPM_TOKEN",
	}
}

// globMatch 做简单的 * 通配全串匹配。
func globMatch(pattern, s string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == s
	}
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	for i := 1; i < len(parts)-1; i++ {
		idx := strings.Index(s, parts[i])
		if idx < 0 {
			return false
		}
		s = s[idx+len(parts[i]):]
	}
	return strings.HasSuffix(s, parts[len(parts)-1])
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

var _ = errors.Is
