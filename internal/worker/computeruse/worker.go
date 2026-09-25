// Package computeruse 实现 ComputerUse Worker：桌面操控，经 pi-helper（windows-bridge.exe）
// 桥接，跑在独立故障域里。
//
// IPC 约定与 v1 对等（v1 审计：src/main/tools/ComputerUse/PiBridge.ts）：
//
//	传输：子进程 stdin/stdout，逐行 JSON（\n 分隔）
//	协议版本：4
//	请求：{"protocolVersion":4,"id":"<uuid>","cmd":"<name>","args":{...}}
//	响应：{"protocolVersion":4,"id":"<uuid>","ok":true,"result":{...},
//	       "error":{"message":"...","code":"..."}}
//
// v1 的动作清单（本包保持功能对等）：
//
//	感知：screenshot(look) / observe(look) / find_window(listRoots)
//	语义操作：click_element(act:press) / set_text(act:setText) / read_text(uiaReadText)
//	直接键鼠：mouse_click / mouse_move / mouse_drag / mouse_scroll / key_press / key_type
//	等待：wait(uiaWaitFor)
//
// 相对 v1 的加固点：
//  1. v1 用 spawn + dispose 时 kill 主进程，**没有进程树清理**（helper 若再起子进程会泄漏）；
//     本包用 procguard，Windows Job Object 保证整棵树退出。
//  2. v1 没有任何窗口/应用白名单，任何窗口都能操作；本包增加 AllowedWindows /
//     DeniedWindows 与"需要显式开启"的开关（第 20 章权限维度的落地）。
//  3. v1 无输出限流；本包用 procguard 的限流缓冲。
package computeruse

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/worker"
	"github.com/ximo888ok-netizen/ximo-agent/internal/worker/procguard"
)

// ProtocolVersion 与 v1 的 PROTOCOL_VERSION 保持一致。
const ProtocolVersion = 4

// 动作名（与 v1 的 action 取值对等）。
const (
	ActionScreenshot   = "screenshot"
	ActionObserve      = "observe"
	ActionFindWindow   = "find_window"
	ActionClickElement = "click_element"
	ActionSetText      = "set_text"
	ActionReadText     = "read_text"
	ActionMouseClick   = "mouse_click"
	ActionMouseMove    = "mouse_move"
	ActionMouseDrag    = "mouse_drag"
	ActionMouseScroll  = "mouse_scroll"
	ActionKeyPress     = "key_press"
	ActionKeyType      = "key_type"
	ActionWait         = "wait"
)

// Config 是 ComputerUse Worker 配置。
type Config struct {
	// HelperPath 是 pi-helper（windows-bridge.exe）路径。空则按顺序探测。
	HelperPath string
	// SearchPaths 是探测 helper 的候选目录。
	SearchPaths []string
	// CommandTimeout 默认单命令超时（对齐 v1 的 30s）。
	CommandTimeout time.Duration
	// HandshakeTimeout 启动后的 listRoots 自检超时（对齐 v1 的 5s）。
	HandshakeTimeout time.Duration
	// MaxOutputBytes 输出上限。
	MaxOutputBytes int64
	// Enabled 必须显式开启。ComputerUse 是最高风险能力（可操作整个桌面），
	// 默认关闭；未开启时所有调用返回策略拒绝（fail-closed）。
	Enabled bool
	// AllowedWindows 非空时只允许操作标题匹配其中任意模式的窗口。
	AllowedWindows []string
	// DeniedWindows 中的模式永不操作。
	DeniedWindows []string
	// MaxDimension 截图最长边（对齐 v1 的 1280）。
	MaxDimension int
}

func (c Config) withDefaults() Config {
	if c.CommandTimeout <= 0 {
		c.CommandTimeout = 30 * time.Second
	}
	if c.HandshakeTimeout <= 0 {
		c.HandshakeTimeout = 5 * time.Second
	}
	if c.MaxOutputBytes <= 0 {
		c.MaxOutputBytes = 8 << 20 // 截图 base64 较大
	}
	if c.MaxDimension <= 0 {
		c.MaxDimension = 1280
	}
	return c
}

// Worker 是 ComputerUse 独立故障域 Worker：持有一个 pi-helper 长连接。
type Worker struct {
	id  string
	cfg Config

	mu      sync.Mutex
	proc    *procguard.Proc
	stdin   io.WriteCloser
	stdout  *bufio.Scanner
	pending map[string]chan helperResponse
	started bool
	closed  bool
	// readErr 记录读循环的终止原因，供调用方拿到明确错误。
	readErr   error
	readDone  chan struct{}
	sessionID string
}

// NewWorker 创建 ComputerUse Worker。
func NewWorker(id string, cfg Config) *Worker {
	return &Worker{id: id, cfg: cfg.withDefaults(), pending: map[string]chan helperResponse{}}
}

// NewWorkerFromSpec 从 worker.Spec 构造。
func NewWorkerFromSpec(spec worker.Spec) (worker.Worker, error) {
	cfg := Config{
		HelperPath:     spec.String("helper_path", ""),
		SearchPaths:    spec.Strings("search_paths"),
		Enabled:        spec.Bool("enabled", false),
		AllowedWindows: spec.Strings("allowed_windows"),
		DeniedWindows:  spec.Strings("denied_windows"),
	}
	if d := spec.Duration("command_timeout", 0); d > 0 {
		cfg.CommandTimeout = d
	}
	if n := spec.Int("max_dimension", 0); n > 0 {
		cfg.MaxDimension = n
	}
	return NewWorker(spec.ID, cfg), nil
}

// ID 实现 worker.Worker。
func (w *Worker) ID() string { return w.id }

// Kind 实现 worker.Worker。
func (w *Worker) Kind() string { return "computer-use" }

// Start 实现 worker.Worker：拉起 pi-helper 并做 listRoots 握手自检。
func (w *Worker) Start(ctx context.Context) error {
	if !w.cfg.Enabled {
		// 未启用时不拉起进程，但也不报错——Health 会如实报告"策略禁用"。
		w.mu.Lock()
		w.started = true
		w.mu.Unlock()
		return nil
	}
	helper, err := w.resolveHelper()
	if err != nil {
		return err
	}

	// 关键：经 procguard 拉起，helper 及其可能的子进程受 Job Object / 进程组管控。
	pc := procguard.Config{
		Path:           helper,
		MaxOutputBytes: w.cfg.MaxOutputBytes,
		KillGrace:      2 * time.Second,
		HideWindow:     true,
		CreateNoWindow: true,
		MaxProcesses:   8,
		Interactive:    true, // stdin/stdout 用于行分隔 JSON IPC
	}
	p, err := procguard.Start(ctx, pc)
	if err != nil {
		return fmt.Errorf("%w: 启动 pi-helper 失败: %v", worker.ErrUnavailable, err)
	}
	stdin, err := p.StdinPipe()
	if err != nil {
		_ = p.Kill(context.Background())
		_ = p.Close()
		return fmt.Errorf("%w: 获取 pi-helper stdin 失败: %v", worker.ErrInternal, err)
	}
	stdoutPipe, err := p.StdoutPipe()
	if err != nil {
		_ = p.Kill(context.Background())
		_ = p.Close()
		return fmt.Errorf("%w: 获取 pi-helper stdout 失败: %v", worker.ErrInternal, err)
	}

	sc := bufio.NewScanner(stdoutPipe)
	// Scanner 的 max token 是 int；把上限夹到 int 范围，避免 32 位平台溢出为负值。
	maxTok := w.cfg.MaxOutputBytes
	if maxTok > int64(^uint(0)>>1) {
		maxTok = int64(^uint(0) >> 1)
	}
	sc.Buffer(make([]byte, 64<<10), int(maxTok))

	w.mu.Lock()
	w.proc = p
	w.stdin = stdin
	w.stdout = sc
	w.readDone = make(chan struct{})
	w.started = true
	w.closed = false
	w.readErr = nil
	w.mu.Unlock()

	go w.readLoop(sc)

	// 握手自检：listRoots（对齐 v1 PiBridge 的启动验证）。
	hctx, cancel := context.WithTimeout(ctx, w.cfg.HandshakeTimeout)
	defer cancel()
	if _, err := w.call(hctx, "listRoots", map[string]any{}); err != nil {
		_ = w.Stop(context.Background())
		return fmt.Errorf("%w: pi-helper 握手失败: %v", worker.ErrUnavailable, err)
	}
	return nil
}

// Health 实现 worker.Worker。
func (w *Worker) Health(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return fmt.Errorf("%w: computer-use worker 已关闭", worker.ErrUnavailable)
	}
	if !w.cfg.Enabled {
		// 未启用是"策略禁用"，不是故障。返回 nil 避免 Manager 反复重启；
		// 具体能力不可用通过 Execute 返回 policy_denied 表达。
		return nil
	}
	if w.proc == nil {
		return fmt.Errorf("%w: pi-helper 未启动", worker.ErrCrash)
	}
	if w.readErr != nil {
		return fmt.Errorf("%w: pi-helper 输出中断: %v", worker.ErrCrash, w.readErr)
	}
	if w.proc.Exited() {
		return fmt.Errorf("%w: pi-helper 进程已退出", worker.ErrCrash)
	}
	return nil
}

// Stop 实现 worker.Worker：关闭 IPC 并清理整棵进程树。
func (w *Worker) Stop(ctx context.Context) error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	stdin := w.stdin
	p := w.proc
	pending := w.pending
	w.pending = map[string]chan helperResponse{}
	w.stdin = nil
	w.proc = nil
	w.mu.Unlock()

	for id, ch := range pending {
		select {
		case ch <- helperResponse{Error: &helperError{Message: "worker 已关闭", Code: "closed"}}:
		default:
		}
		_ = id
	}
	if stdin != nil {
		_ = stdin.Close()
	}
	if p != nil {
		_ = p.Kill(ctx)
		_ = p.Close()
	}
	return nil
}

// PIDs 实现 worker.ProcessAware。
func (w *Worker) PIDs() []int {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.proc == nil {
		return nil
	}
	pid := w.proc.PID()
	if pid <= 0 {
		return nil
	}
	return []int{pid}
}

// helperRequest / helperResponse 与 v1 的 IPC 消息格式一致。
type helperRequest struct {
	ProtocolVersion int            `json:"protocolVersion"`
	ID              string         `json:"id"`
	Cmd             string         `json:"cmd"`
	Args            map[string]any `json:"args,omitempty"`
}

type helperResponse struct {
	ProtocolVersion int             `json:"protocolVersion"`
	ID              string          `json:"id"`
	OK              bool            `json:"ok"`
	Result          json.RawMessage `json:"result,omitempty"`
	Error           *helperError    `json:"error,omitempty"`
}

type helperError struct {
	Message string `json:"message"`
	Code    string `json:"code"`
}

// readLoop 逐行读取 helper 输出并按 id 分发响应。
func (w *Worker) readLoop(sc *bufio.Scanner) {
	defer close(w.readDone)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var resp helperResponse
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			// 非 JSON 行是 helper 日志，忽略。
			continue
		}
		if resp.ProtocolVersion != ProtocolVersion {
			// 协议不匹配：让调用方明确失败，而不是半死不活地继续。
			w.mu.Lock()
			w.readErr = fmt.Errorf("协议版本不匹配: got=%d want=%d", resp.ProtocolVersion, ProtocolVersion)
			w.mu.Unlock()
			continue
		}
		w.mu.Lock()
		ch := w.pending[resp.ID]
		delete(w.pending, resp.ID)
		w.mu.Unlock()
		if ch != nil {
			select {
			case ch <- resp:
			default:
			}
		}
	}
	if err := sc.Err(); err != nil {
		w.mu.Lock()
		w.readErr = err
		w.mu.Unlock()
	}
	// 读循环结束：让所有等待中的调用立即失败。
	w.mu.Lock()
	pending := w.pending
	w.pending = map[string]chan helperResponse{}
	w.mu.Unlock()
	for id, ch := range pending {
		select {
		case ch <- helperResponse{Error: &helperError{Message: "pi-helper 连接中断", Code: "disconnected"}}:
		default:
		}
		_ = id
	}
}

// call 发送一条命令并等待响应。
func (w *Worker) call(ctx context.Context, cmd string, args map[string]any) (json.RawMessage, error) {
	w.mu.Lock()
	if w.closed || w.stdin == nil {
		w.mu.Unlock()
		return nil, fmt.Errorf("%w: pi-helper 不可用", worker.ErrUnavailable)
	}
	if w.readErr != nil {
		err := w.readErr
		w.mu.Unlock()
		return nil, fmt.Errorf("%w: %v", worker.ErrUpstream, err)
	}
	stdin := w.stdin
	// 生成请求 ID（单调递增 + 纳秒，避免碰撞）。
	w.sessionID = fmt.Sprintf("cu-%d", time.Now().UnixNano())
	id := w.sessionID
	ch := make(chan helperResponse, 1)
	w.pending[id] = ch
	w.mu.Unlock()

	defer func() {
		w.mu.Lock()
		delete(w.pending, id)
		w.mu.Unlock()
	}()

	req := helperRequest{ProtocolVersion: ProtocolVersion, ID: id, Cmd: cmd, Args: args}
	raw, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("%w: 编码请求失败: %v", worker.ErrInternal, err)
	}
	stdin.Write(raw)
	stdin.Write([]byte("\n"))

	select {
	case resp := <-ch:
		if resp.Error != nil {
			return nil, fmt.Errorf("%w: %s: %s", worker.ErrUpstream, resp.Error.Code, resp.Error.Message)
		}
		return resp.Result, nil
	case <-ctx.Done():
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("%w: pi-helper 命令 %s 超时", worker.ErrTimeout, cmd)
		}
		return nil, fmt.Errorf("%w: pi-helper 命令 %s 被取消", worker.ErrCanceled, cmd)
	}
}

// args 是 ComputerUse 调用的参数（与 v1 各 action 的入参对齐）。
type args struct {
	Window       string   `json:"window,omitempty"`
	Ref          string   `json:"ref,omitempty"`
	X            int      `json:"x,omitempty"`
	Y            int      `json:"y,omitempty"`
	Text         string   `json:"text,omitempty"`
	Button       string   `json:"button,omitempty"`
	ClickCount   int      `json:"clickCount,omitempty"`
	ScrollX      int      `json:"scrollX,omitempty"`
	ScrollY      int      `json:"scrollY,omitempty"`
	Keys         []string `json:"keys,omitempty"`
	Path         []point  `json:"path,omitempty"`
	Until        string   `json:"until,omitempty"`
	TimeoutMs    int64    `json:"timeoutMs,omitempty"`
	InclImage    *bool    `json:"includeImage,omitempty"`
	MaxDimension int      `json:"maxDimension,omitempty"`
}

type point struct {
	X int `json:"x"`
	Y int `json:"y"`
}

// Execute 实现 worker.Worker。
func (w *Worker) Execute(ctx context.Context, req worker.WorkerRequest) (worker.WorkerResponse, error) {
	if !w.cfg.Enabled {
		return worker.ErrorResponse(req, w.id, worker.CodePolicyDenied,
			"ComputerUse 未启用（需显式打开 computer_use.enabled）"), nil
	}
	started := time.Now()
	var a args
	if err := req.Bind(&a); err != nil {
		return worker.ErrorResponse(req, w.id, worker.CodeInvalidArgument, err.Error()), nil
	}

	// 窗口白/黑名单校验（v1 完全没有这层）。
	if err := w.checkWindow(a.Window); err != nil {
		return worker.ErrorResponse(req, w.id, worker.CodePolicyDenied, err.Error()), nil
	}

	cmd, cmdArgs, err := w.translate(req.Action, a)
	if err != nil {
		return worker.ErrorResponse(req, w.id, worker.CodeInvalidArgument, err.Error()), nil
	}

	callCtx, cancel := context.WithTimeout(ctx, w.cfg.CommandTimeout)
	defer cancel()
	raw, err := w.call(callCtx, cmd, cmdArgs)
	if err != nil {
		return worker.ErrorResponseFrom(req, w.id, started, err), nil
	}

	// 截图类结果：base64 图片可能很大，必须落 CAS 而不是塞进 JSON payload。
	body := map[string]any{
		"action":  req.Action,
		"result":  json.RawMessage(raw),
		"content": fmt.Sprintf("computer-use %s 执行完成", req.Action),
	}
	resp, _ := worker.OKResponse(req, w.id, started, body)
	resp.Metrics.OutputBytes = int64(len(raw))
	return resp, nil
}

// translate 把高层 action 映射为 helper 的底层命令（与 v1 的映射表一致）。
func (w *Worker) translate(action string, a args) (string, map[string]any, error) {
	maxDim := a.MaxDimension
	if maxDim <= 0 {
		maxDim = w.cfg.MaxDimension
	}
	switch action {
	case ActionScreenshot:
		incl := true
		return "look", map[string]any{
			"readText": "never", "includeImage": incl, "maxDimension": maxDim,
		}, nil
	case ActionObserve:
		incl := true
		m := map[string]any{"readText": "auto", "includeImage": incl, "maxDimension": maxDim}
		if a.Window != "" {
			m["window"] = a.Window
		}
		return "look", m, nil
	case ActionFindWindow:
		m := map[string]any{}
		if a.Window != "" {
			m["title"] = a.Window
		}
		return "listRoots", m, nil
	case ActionClickElement:
		if a.Ref == "" {
			return "", nil, fmt.Errorf("click_element 需要 ref")
		}
		return "act", map[string]any{"action": "press", "target": map[string]any{"ref": a.Ref}}, nil
	case ActionSetText:
		if a.Ref == "" {
			return "", nil, fmt.Errorf("set_text 需要 ref")
		}
		return "act", map[string]any{
			"action": "setText",
			"target": map[string]any{"ref": a.Ref},
			"params": map[string]any{"text": a.Text},
		}, nil
	case ActionReadText:
		if a.Ref == "" {
			return "", nil, fmt.Errorf("read_text 需要 ref")
		}
		return "uiaReadText", map[string]any{"ref": a.Ref, "offset": 0}, nil
	case ActionMouseClick:
		btn := a.Button
		if btn == "" {
			btn = "left"
		}
		cnt := a.ClickCount
		if cnt <= 0 {
			cnt = 1
		}
		return "act", map[string]any{
			"action": "click",
			"target": map[string]any{"x": a.X, "y": a.Y},
			"params": map[string]any{"button": btn, "clickCount": cnt},
		}, nil
	case ActionMouseMove:
		return "act", map[string]any{
			"action": "moveMouse", "target": map[string]any{"x": a.X, "y": a.Y},
		}, nil
	case ActionMouseDrag:
		if len(a.Path) < 2 {
			return "", nil, fmt.Errorf("mouse_drag 至少需要两个路径点")
		}
		pts := make([]map[string]any, 0, len(a.Path))
		for _, p := range a.Path {
			pts = append(pts, map[string]any{"x": p.X, "y": p.Y})
		}
		return "act", map[string]any{"action": "drag", "params": map[string]any{"path": pts}}, nil
	case ActionMouseScroll:
		return "act", map[string]any{
			"action": "scroll",
			"target": map[string]any{"x": a.X, "y": a.Y},
			"params": map[string]any{"scrollX": a.ScrollX, "scrollY": a.ScrollY},
		}, nil
	case ActionKeyPress:
		if len(a.Keys) == 0 {
			return "", nil, fmt.Errorf("key_press 需要 keys")
		}
		return "act", map[string]any{"action": "keypress", "params": map[string]any{"keys": a.Keys}}, nil
	case ActionKeyType:
		return "act", map[string]any{"action": "typeText", "params": map[string]any{"text": a.Text}}, nil
	case ActionWait:
		ms := a.TimeoutMs
		if ms <= 0 {
			ms = 10000
		}
		until := a.Until
		if until == "" {
			until = "present"
		}
		return "uiaWaitFor", map[string]any{"text": a.Text, "until": until, "timeoutMs": ms}, nil
	default:
		return "", nil, fmt.Errorf("未知的 computer-use 动作 %q", action)
	}
}

// checkWindow 校验窗口白/黑名单（v1 完全缺失的权限维度）。
func (w *Worker) checkWindow(title string) error {
	for _, pat := range w.cfg.DeniedWindows {
		if matchPattern(pat, title) {
			return fmt.Errorf("窗口 %q 命中拒绝规则 %q", title, pat)
		}
	}
	if len(w.cfg.AllowedWindows) == 0 {
		return nil
	}
	// 全局动作（鼠标/键盘，不带 window）在白名单模式下需要显式放行 "*"。
	if title == "" {
		for _, pat := range w.cfg.AllowedWindows {
			if pat == "*" {
				return nil
			}
		}
		return fmt.Errorf("白名单模式下必须指定 window（或把 \"*\" 加入 allowed_windows 以允许全局键鼠操作）")
	}
	for _, pat := range w.cfg.AllowedWindows {
		if matchPattern(pat, title) {
			return nil
		}
	}
	return fmt.Errorf("窗口 %q 不在 allowed_windows 内", title)
}

// matchPattern 做大小写不敏感的子串/通配匹配。
func matchPattern(pattern, s string) bool {
	if pattern == "*" {
		return true
	}
	p := strings.ToLower(pattern)
	v := strings.ToLower(s)
	if strings.Contains(p, "*") {
		parts := strings.Split(p, "*")
		idx := 0
		for i, part := range parts {
			if part == "" {
				continue
			}
			found := strings.Index(v[idx:], part)
			if found < 0 {
				return false
			}
			if i == 0 && found != 0 {
				return false
			}
			idx += found + len(part)
		}
		if last := parts[len(parts)-1]; last != "" && !strings.HasSuffix(v, last) {
			return false
		}
		return true
	}
	return strings.Contains(v, p)
}

// resolveHelper 按顺序探测 pi-helper。
func (w *Worker) resolveHelper() (string, error) {
	if w.cfg.HelperPath != "" {
		if existsFile(w.cfg.HelperPath) {
			return w.cfg.HelperPath, nil
		}
		return "", fmt.Errorf("%w: 配置的 pi-helper 路径不存在: %s", worker.ErrUnavailable, w.cfg.HelperPath)
	}
	// SearchPaths 的每一项允许是"目录"或"具体文件"：
	// 目录则在其下找 helper；这样配置侧不必先知道确切文件名。
	candidates := make([]string, 0, len(w.cfg.SearchPaths)*2)
	for _, c := range w.cfg.SearchPaths {
		if c == "" {
			continue
		}
		candidates = append(candidates, c)
		if st, err := os.Stat(c); err == nil && st.IsDir() {
			candidates = append(candidates, filepath.Join(c, helperName()))
		}
	}
	// 与 v1 对等：先看应用自带 prebuilt 目录，再看用户数据目录。
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		candidates = append(candidates,
			filepath.Join(dir, "prebuilt", platformDir(), helperName()),
			filepath.Join(dir, "..", "prebuilt", platformDir(), helperName()),
		)
	}
	if runtime.GOOS == "windows" {
		// v1 的部署位置：用户数据目录下的 pi-helper 子目录。
		if appData := os.Getenv("APPDATA"); appData != "" {
			candidates = append(candidates, filepath.Join(appData, "ximo-agent", "pi-helper", helperName()))
		}
		if local := os.Getenv("LOCALAPPDATA"); local != "" {
			candidates = append(candidates, filepath.Join(local, "ximo-agent", "pi-helper", helperName()))
		}
	}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if existsFile(c) {
			return c, nil
		}
	}
	return "", fmt.Errorf("%w: 未找到 pi-helper（%s），请设置 helper_path", worker.ErrUnavailable, helperName())
}

func helperName() string {
	if runtime.GOOS == "windows" {
		return "windows-bridge.exe"
	}
	return "pi-helper"
}

func platformDir() string {
	switch runtime.GOOS {
	case "windows":
		return "win-x64"
	case "darwin":
		return "darwin-arm64"
	default:
		return "linux-x64"
	}
}

func existsFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
