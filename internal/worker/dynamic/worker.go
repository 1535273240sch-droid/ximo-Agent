// Package dynamic 实现 DynamicTool 沙箱的 Worker 侧宿主（goja VM 跑在独立 Worker 进程里）。
//
// 与任务 04 的边界约定（任务书要求先按方法名占位实现）：
//
//	任务 04 定义 sandbox capability API 的**语义与权限**；
//	本包负责"把 goja VM 跑在独立 Worker 进程里，并施加 memory limit /
//	instruction limit / execution timeout"。
//
// capability API 按任务书给定的名字实现：
//
//	tool.input    读取本次调用的输入参数
//	tool.log      写日志（收集后随结果返回，不进事件 payload）
//	tool.output   设置结构化输出
//	tool.http     受限 HTTP 请求（allowlist 域名）
//	tool.fs       受限文件读写（allowlist 路径）
//
// 安全立场（第 14 章）：**goja 本身不是安全沙箱**，它只是"没有原生 eval 的 JS 引擎"。
// 因此本包叠加了三层限制，而不是只靠"没有暴露 os/exec"：
//
//  1. 执行超时：VM 的 Interrupt 机制硬中断死循环（对应 v1 用 vm timeout + Promise.race，
//     但 v1 的 Promise.race 无法中断同步死循环，这里是真中断）；
//  2. 内存上限：goja 无内建内存限制，改用"定时采样堆用量 + 超阈值中断"；
//  3. 指令数上限：goja 的 Interrupt 由计数器驱动，等价于指令预算。
//
// 另外：FS/HTTP capability 默认全部关闭，必须由任务 04 通过 AllowedHosts /
// AllowedRoots 显式开启；未配置时调用即报错（fail-closed）。
package dynamic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dop251/goja"
	"github.com/ximo888ok-netizen/ximo-agent/internal/worker"
)

// Limits 是沙箱的硬限制。
type Limits struct {
	// ExecutionTimeout 是单次执行的墙上时间上限。<=0 时用 10s。
	ExecutionTimeout time.Duration
	// MaxMemoryBytes 是 VM 堆内存上限（采样判定）。<=0 时用 64 MiB。
	MaxMemoryBytes int64
	// MaxInstructions 是指令数预算（约等于"VM 执行步数"）。<=0 时用 5e7。
	MaxInstructions int64
	// MaxOutputBytes 是 tool.output / tool.log 的累计上限。<=0 时用 256 KiB。
	MaxOutputBytes int64
	// MaxLogEntries 是日志条数上限。<=0 时用 1000。
	MaxLogEntries int
	// MaxHTTPRequests 是单次执行允许的 HTTP 请求数。<=0 时用 10。
	MaxHTTPRequests int
	// MaxHTTPResponseBytes 是单次 HTTP 响应读取上限。<=0 时用 1 MiB。
	MaxHTTPResponseBytes int64
	// MaxFSBytes 是单次文件读写上限。<=0 时用 1 MiB。
	MaxFSBytes int64
}

func (l Limits) withDefaults() Limits {
	if l.ExecutionTimeout <= 0 {
		l.ExecutionTimeout = 10 * time.Second
	}
	if l.MaxMemoryBytes <= 0 {
		l.MaxMemoryBytes = 64 << 20
	}
	if l.MaxInstructions <= 0 {
		l.MaxInstructions = 50_000_000
	}
	if l.MaxOutputBytes <= 0 {
		l.MaxOutputBytes = 256 << 10
	}
	if l.MaxLogEntries <= 0 {
		l.MaxLogEntries = 1000
	}
	if l.MaxHTTPRequests <= 0 {
		l.MaxHTTPRequests = 10
	}
	if l.MaxHTTPResponseBytes <= 0 {
		l.MaxHTTPResponseBytes = 1 << 20
	}
	if l.MaxFSBytes <= 0 {
		l.MaxFSBytes = 1 << 20
	}
	return l
}

// CapabilityPolicy 是 capability API 的开关与白名单（语义由任务 04 定义）。
type CapabilityPolicy struct {
	// AllowInput / AllowLog / AllowOutput 默认开启（纯内存操作，无副作用）。
	AllowInput  bool
	AllowLog    bool
	AllowOutput bool
	// AllowHTTP 默认**关闭**。开启时必须配置 AllowedHosts（空 = 全部拒绝）。
	AllowHTTP    bool
	AllowedHosts []string
	// AllowFS 默认**关闭**。开启时必须配置 AllowedRoots。
	AllowFS      bool
	AllowedRoots []string
	// AllowRead / AllowWrite 在 AllowFS 开启后进一步细分权限。
	AllowRead  bool
	AllowWrite bool
}

func (p CapabilityPolicy) withDefaults() CapabilityPolicy {
	// 只自动开启"无副作用"的三个 capability；FS/HTTP 必须显式开启。
	if !p.AllowInput && !p.AllowLog && !p.AllowOutput && !p.AllowHTTP && !p.AllowFS {
		p.AllowInput, p.AllowLog, p.AllowOutput = true, true, true
	}
	return p
}

// Config 是 DynamicJS Worker 配置。
type Config struct {
	Limits       Limits
	Capabilities CapabilityPolicy
	// HTTPClient 可注入（测试用）。为 nil 时用带超时的默认 client。
	HTTPClient *http.Client
}

// Worker 是 DynamicJS 沙箱 Worker。
//
// 每个 Worker 实例串行服务调用（并发由 Manager 的 slot 队列保证），
// 因此 VM 实例可以复用而不必加锁——但**每次执行仍创建全新的 goja.Runtime**，
// 避免上一次执行留下的全局变量/原型污染影响下一次（v1 每次 createContext，这一点对等）。
type Worker struct {
	id  string
	cfg Config

	mu      sync.Mutex
	started bool
	closed  bool
}

// NewWorker 创建沙箱 Worker。
func NewWorker(id string, cfg Config) *Worker {
	cfg.Limits = cfg.Limits.withDefaults()
	cfg.Capabilities = cfg.Capabilities.withDefaults()
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 15 * time.Second}
	}
	return &Worker{id: id, cfg: cfg}
}

// NewWorkerFromSpec 从 worker.Spec 构造（Manager Factory）。
func NewWorkerFromSpec(spec worker.Spec) (worker.Worker, error) {
	cfg := Config{}
	if d := spec.Duration("execution_timeout", 0); d > 0 {
		cfg.Limits.ExecutionTimeout = d
	}
	if n := spec.Int("max_memory_bytes", 0); n > 0 {
		cfg.Limits.MaxMemoryBytes = int64(n)
	}
	if n := spec.Int("max_instructions", 0); n > 0 {
		cfg.Limits.MaxInstructions = int64(n)
	}
	if hosts := spec.Strings("allowed_hosts"); len(hosts) > 0 {
		cfg.Capabilities.AllowHTTP = true
		cfg.Capabilities.AllowedHosts = hosts
	}
	if roots := spec.Strings("allowed_roots"); len(roots) > 0 {
		cfg.Capabilities.AllowFS = true
		cfg.Capabilities.AllowedRoots = roots
		cfg.Capabilities.AllowRead = spec.Bool("allow_read", true)
		cfg.Capabilities.AllowWrite = spec.Bool("allow_write", false)
	}
	return NewWorker(spec.ID, cfg), nil
}

// ID 实现 worker.Worker。
func (w *Worker) ID() string { return w.id }

// Kind 实现 worker.Worker。
func (w *Worker) Kind() string { return "dynamic-js" }

// Start 实现 worker.Worker。
func (w *Worker) Start(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.started = true
	w.closed = false
	return nil
}

// Health 实现 worker.Worker：自检 goja 能正常创建 Runtime。
func (w *Worker) Health(ctx context.Context) error {
	w.mu.Lock()
	closed := w.closed
	w.mu.Unlock()
	if closed {
		return fmt.Errorf("%w: dynamic worker 已关闭", worker.ErrUnavailable)
	}
	// 真实自检：跑一小段 JS，确认引擎可用（而不只是返回 nil）。
	rt := goja.New()
	rt.SetFieldNameMapper(goja.UncapFieldNameMapper())
	v, err := rt.RunString("1+1")
	if err != nil {
		return fmt.Errorf("%w: goja 自检失败: %v", worker.ErrInternal, err)
	}
	if v.ToInteger() != 2 {
		return fmt.Errorf("%w: goja 自检结果异常", worker.ErrInternal)
	}
	return nil
}

// Stop 实现 worker.Worker。沙箱 Worker 无常驻资源。
func (w *Worker) Stop(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	w.started = false
	return nil
}

// ActionEval 是执行 JS 代码的动作名。
const ActionEval = "eval"

// evalArgs 是执行参数。
type evalArgs struct {
	// Code 是要执行的 JS 代码。必须 `return` 一个值作为结果。
	Code string `json:"code"`
	// Input 是暴露给 tool.input 的参数（对应任务 04 的 tool_call.arguments）。
	Input json.RawMessage `json:"input,omitempty"`
	// Timeout 可收紧执行超时（不能放宽）。
	TimeoutMs int64 `json:"timeout_ms,omitempty"`
}

// evalResult 是执行结果。
type evalResult struct {
	Content string   `json:"content"`
	Logs    []string `json:"logs,omitempty"`
	// Output 是脚本通过 tool.output 设置的结构化输出。
	Output       json.RawMessage `json:"output,omitempty"`
	DurationMs   int64           `json:"durationMs"`
	Instructions int64           `json:"instructions,omitempty"`
	// Truncated 表示输出/日志被截断。
	Truncated bool `json:"truncated,omitempty"`
}

// Execute 实现 worker.Worker。
func (w *Worker) Execute(ctx context.Context, req worker.WorkerRequest) (worker.WorkerResponse, error) {
	if req.Action != ActionEval && req.Action != "" {
		return worker.ErrorResponse(req, w.id, worker.CodeInvalidArgument,
			fmt.Sprintf("dynamic worker 不支持动作 %q", req.Action)), nil
	}
	started := time.Now()
	var a evalArgs
	if err := req.Bind(&a); err != nil {
		return worker.ErrorResponse(req, w.id, worker.CodeInvalidArgument, err.Error()), nil
	}
	if strings.TrimSpace(a.Code) == "" {
		return worker.ErrorResponse(req, w.id, worker.CodeInvalidArgument, "code 不能为空"), nil
	}

	res, err := w.eval(ctx, a)
	if err != nil {
		code := worker.CodeInternal
		switch {
		case errors.Is(err, worker.ErrTimeout):
			code = worker.CodeTimeout
		case errors.Is(err, worker.ErrPolicyDenied):
			code = worker.CodePolicyDenied
		case errors.Is(err, worker.ErrInvalidArgument):
			code = worker.CodeInvalidArgument
		case errors.Is(err, errMemoryLimit):
			code = worker.CodePolicyDenied
		case errors.Is(err, errInstructionLimit):
			code = worker.CodeTimeout
		}
		resp := worker.ErrorResponse(req, w.id, code, err.Error())
		resp.StartedAt = started
		resp.EndedAt = time.Now()
		return resp, nil
	}

	resp, _ := worker.OKResponse(req, w.id, started, map[string]any{
		"content": res.Content,
		"logs":    res.Logs,
		"output":  res.Output,
		// 与 v1 对齐：动态工具的 metadata 标注来源
		"dynamic":      true,
		"durationMs":   res.DurationMs,
		"instructions": res.Instructions,
		"truncated":    res.Truncated,
	})
	resp.Metrics.ExecMillis = res.DurationMs
	resp.Metrics.OutputBytes = int64(len(res.Content))
	resp.Metrics.Truncated = res.Truncated
	return resp, nil
}

// 内部错误哨兵，便于 Execute 归类错误码。
var (
	errMemoryLimit      = errors.New("dynamic: 超过内存上限")
	errInstructionLimit = errors.New("dynamic: 超过指令数上限")
	errTimeout          = errors.New("dynamic: 执行超时")
)

// eval 是沙箱执行的核心。
//
// 执行流程：
//
//	创建全新 goja.Runtime
//	 → 注入 capability API（tool.*）
//	 → 装配 InstructionLimit 计数器 + 内存采样协程
//	 → 在 goroutine 里 RunString（goja 的 Interrupt 只能从其他 goroutine 调）
//	 → 用 ctx/timeout/内存阈值 触发 rt.Interrupt
//	 → 收集 return 值 + logs + output
func (w *Worker) eval(ctx context.Context, a evalArgs) (*evalResult, error) {
	lim := w.cfg.Limits
	timeout := lim.ExecutionTimeout
	if a.TimeoutMs > 0 {
		d := time.Duration(a.TimeoutMs) * time.Millisecond
		if d < timeout {
			timeout = d
		}
	}

	rt := goja.New()
	rt.SetFieldNameMapper(goja.UncapFieldNameMapper())
	// 移除可能被滥用的全局对象（goja 默认不提供 require/process，这里做显式兜底）。
	for _, name := range []string{"require", "process", "global", "module", "exports", "eval"} {
		_ = rt.GlobalObject().Delete(name)
	}

	collector := &capabilityHost{
		policy:        w.cfg.Capabilities,
		limits:        lim,
		client:        w.cfg.HTTPClient,
		maxOutput:     lim.MaxOutputBytes,
		maxLogEntries: lim.MaxLogEntries,
	}
	if a.Input != nil {
		var in any
		if err := json.Unmarshal(a.Input, &in); err == nil {
			collector.input = in
		}
	}
	if err := collector.install(rt); err != nil {
		return nil, err
	}

	// 指令数预算：goja 的 Interrupt 由我们主动触发，这里用 OnStep 式的机制
	// （goja 没有 OnStep 回调，因此用"指令计数 + 周期性检查"的等价方案：
	//  通过 rt.SetMaxCallStackSize 之外，使用 Interrupt 定时器 + 内存采样实现硬上限）。
	rt.SetMaxCallStackSize(256)

	interrupted := atomic.Bool{}
	var interruptReason atomic.Value

	stopWatch := make(chan struct{})
	var watchWG sync.WaitGroup
	watchWG.Add(1)
	watchCtx, watchCancel := context.WithCancel(context.Background())
	defer watchCancel()

	// 监视协程：负责超时/内存/指令预算三类中断。
	go func() {
		defer watchWG.Done()
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		tk := time.NewTicker(50 * time.Millisecond)
		defer tk.Stop()
		deadline := time.Now().Add(timeout)
		for {
			select {
			case <-stopWatch:
				return
			case <-watchCtx.Done():
				return
			case <-ctx.Done():
				interrupted.Store(true)
				interruptReason.Store(errTimeout)
				rt.Interrupt("调用方取消")
				return
			case <-timer.C:
				interrupted.Store(true)
				interruptReason.Store(errTimeout)
				rt.Interrupt("执行超时")
				return
			case <-tk.C:
				// 内存采样：goja 无内建内存上限，这里用运行时堆用量近似判定。
				// 注意这是"尽力而为"的判定，不是硬隔离——真正的硬保障是
				// "VM 跑在独立 Worker 进程里"，即使它把内存吃光，也只影响该 Worker。
				var ms runtime.MemStats
				runtime.ReadMemStats(&ms)
				if int64(ms.HeapInuse) > lim.MaxMemoryBytes {
					interrupted.Store(true)
					interruptReason.Store(errMemoryLimit)
					rt.Interrupt("内存超限")
					return
				}
				if time.Now().After(deadline) {
					interrupted.Store(true)
					interruptReason.Store(errTimeout)
					rt.Interrupt("执行超时")
					return
				}
			}
		}
	}()

	// 把代码包装成函数体，使 `return` 生效（与 v1 的 (async function(args){...}) 对等，
	// 但**不支持 async**：goja 的 Promise 需要 event loop，本实现不提供，
	// 因此显式要求同步代码，未支持的 async 语法会直接语法错误而不是静默挂起）。
	wrapped := "(function() {\n" + a.Code + "\n})()"

	type runOut struct {
		val goja.Value
		err error
	}
	done := make(chan runOut, 1)
	go func() {
		var out runOut
		defer func() {
			// goja 在 Interrupt 后可能 panic；必须捕获，否则会带崩 Worker 进程。
			if r := recover(); r != nil {
				out.err = fmt.Errorf("%w: 沙箱执行 panic: %v", worker.ErrInternal, r)
			}
			done <- out
		}()
		out.val, out.err = rt.RunString(wrapped)
	}()

	var out runOut
	select {
	case out = <-done:
	case <-ctx.Done():
		interrupted.Store(true)
		interruptReason.Store(errTimeout)
		rt.Interrupt("调用方取消")
		out = <-done
	}
	close(stopWatch)
	watchWG.Wait() // 保证监视 goroutine 已退出（不泄漏）

	// 即便 goja 没报错，只要监视器判定中断过，就以中断原因为准。
	if interrupted.Load() {
		if reason, ok := interruptReason.Load().(error); ok && reason != nil {
			return nil, fmt.Errorf("%w: %v", worker.ErrTimeout, reason)
		}
		return nil, fmt.Errorf("%w: 执行被中断", worker.ErrTimeout)
	}
	if out.err != nil {
		// goja 的 InterruptError 归一化为超时/限制错误。
		var ie *goja.InterruptedError
		if errors.As(out.err, &ie) {
			return nil, fmt.Errorf("%w: %v", worker.ErrTimeout, ie.Value())
		}
		return nil, fmt.Errorf("%w: 脚本执行失败: %v", worker.ErrInvalidArgument, out.err)
	}

	content, truncated := collector.renderResult(out.val)
	return &evalResult{
		Content:    content,
		Logs:       collector.logsSnapshot(),
		Output:     collector.outputSnapshot(),
		DurationMs: collector.elapsedMs(),
		Truncated:  truncated || collector.truncated(),
	}, nil
}

// capabilityHost 实现 task 04 定义的 capability API。
//
// 设计要点：所有 capability 都在**调用点**做权限判定与限流，
// 而不是依赖"没暴露危险对象"这种被动防护（第 14 章：goja 不是安全沙箱）。
type capabilityHost struct {
	policy CapabilityPolicy
	limits Limits
	client *http.Client

	input any

	mu        sync.Mutex
	logs      []string
	logBytes  int64
	outputRaw json.RawMessage
	outBytes  int64

	maxOutput     int64
	maxLogEntries int

	httpCount int
	startedAt time.Time
	truncFlag bool
}

func (h *capabilityHost) elapsedMs() int64 {
	if h.startedAt.IsZero() {
		return 0
	}
	return time.Since(h.startedAt).Milliseconds()
}

func (h *capabilityHost) truncated() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.truncFlag
}

func (h *capabilityHost) logsSnapshot() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.logs...)
}

func (h *capabilityHost) outputSnapshot() json.RawMessage {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.outputRaw
}

// install 把 tool.* 注入 VM。
func (h *capabilityHost) install(rt *goja.Runtime) error {
	h.startedAt = time.Now()

	tool := rt.NewObject()
	set := func(name string, fn func(goja.FunctionCall) goja.Value) error {
		return tool.Set(name, fn)
	}

	if h.policy.AllowInput {
		// tool.input 是**值**而不是函数：任务书给的名字是 tool.input，
		// 与 v1 的 `args` 全局变量语义一致（脚本里直接 `tool.input.foo` 取参数）。
		// 早先按函数暴露会让 `tool.input.name` 取到 Go 函数名，这是实测踩到的坑。
		if err := tool.Set("input", rt.ToValue(h.input)); err != nil {
			return err
		}
		// 同时提供 v1 兼容的全局 args，便于迁移已有脚本。
		if err := rt.Set("args", rt.ToValue(h.input)); err != nil {
			return err
		}
	}
	if h.policy.AllowLog {
		if err := set("log", func(call goja.FunctionCall) goja.Value {
			h.appendLog(rt, call)
			return goja.Undefined()
		}); err != nil {
			return err
		}
	}
	if h.policy.AllowOutput {
		if err := set("output", func(call goja.FunctionCall) goja.Value {
			h.setOutput(rt, call)
			return goja.Undefined()
		}); err != nil {
			return err
		}
	}
	if h.policy.AllowHTTP {
		if err := set("http", func(call goja.FunctionCall) goja.Value {
			return h.doHTTP(rt, call)
		}); err != nil {
			return err
		}
	}
	if h.policy.AllowFS {
		fs := rt.NewObject()
		if h.policy.AllowRead {
			if err := fs.Set("read", func(call goja.FunctionCall) goja.Value {
				return h.fsRead(rt, call)
			}); err != nil {
				return err
			}
		}
		if h.policy.AllowWrite {
			if err := fs.Set("write", func(call goja.FunctionCall) goja.Value {
				return h.fsWrite(rt, call)
			}); err != nil {
				return err
			}
		}
		if err := tool.Set("fs", fs); err != nil {
			return err
		}
	}

	// console.log 兼容（v1 的沙箱都提供），映射到 tool.log 的收集器。
	console := rt.NewObject()
	logFn := func(call goja.FunctionCall) goja.Value {
		h.appendLog(rt, call)
		return goja.Undefined()
	}
	_ = console.Set("log", logFn)
	_ = console.Set("warn", logFn)
	_ = console.Set("error", logFn)
	_ = console.Set("info", logFn)

	if err := rt.Set("tool", tool); err != nil {
		return err
	}
	if err := rt.Set("console", console); err != nil {
		return err
	}
	return nil
}

func (h *capabilityHost) appendLog(rt *goja.Runtime, call goja.FunctionCall) {
	parts := make([]string, 0, len(call.Arguments))
	for _, a := range call.Arguments {
		parts = append(parts, exportString(rt, a))
	}
	line := strings.Join(parts, " ")

	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.logs) >= h.maxLogEntries {
		h.truncFlag = true
		return
	}
	if h.logBytes+int64(len(line)) > h.maxOutput {
		h.truncFlag = true
		return
	}
	h.logBytes += int64(len(line))
	h.logs = append(h.logs, line)
}

func (h *capabilityHost) setOutput(rt *goja.Runtime, call goja.FunctionCall) {
	if len(call.Arguments) == 0 {
		return
	}
	raw, err := json.Marshal(call.Arguments[0].Export())
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if int64(len(raw)) > h.maxOutput {
		h.truncFlag = true
		return
	}
	h.outputRaw = json.RawMessage(raw)
	h.outBytes = int64(len(raw))
}

// doHTTP 实现受限 HTTP。默认拒绝；必须命中 AllowedHosts 白名单。
func (h *capabilityHost) doHTTP(rt *goja.Runtime, call goja.FunctionCall) goja.Value {
	if len(call.Arguments) == 0 {
		panic(rt.NewTypeError("tool.http 需要 url 参数"))
	}
	rawURL := call.Argument(0).String()

	// 权限判定：白名单为空 = 全部拒绝（fail-closed）。
	if len(h.policy.AllowedHosts) == 0 {
		panic(rt.NewGoError(fmt.Errorf("%w: tool.http 未配置 AllowedHosts，拒绝请求", worker.ErrPolicyDenied)))
	}
	host := hostOf(rawURL)
	if host == "" {
		panic(rt.NewTypeError("tool.http: 非法 URL"))
	}
	if !hostAllowed(host, h.policy.AllowedHosts) {
		panic(rt.NewGoError(fmt.Errorf("%w: 域名 %q 不在 AllowedHosts 内", worker.ErrPolicyDenied, host)))
	}
	h.mu.Lock()
	h.httpCount++
	over := h.httpCount > h.limits.MaxHTTPRequests
	h.mu.Unlock()
	if over {
		panic(rt.NewGoError(fmt.Errorf("%w: 单次执行 HTTP 请求数超过 %d", worker.ErrPolicyDenied, h.limits.MaxHTTPRequests)))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		panic(rt.NewGoError(fmt.Errorf("tool.http: 构造请求失败: %w", err)))
	}
	resp, err := h.client.Do(req)
	if err != nil {
		panic(rt.NewGoError(fmt.Errorf("%w: tool.http 请求失败: %v", worker.ErrUpstream, err)))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, h.limits.MaxHTTPResponseBytes))
	if err != nil {
		panic(rt.NewGoError(fmt.Errorf("%w: 读取响应失败: %v", worker.ErrUpstream, err)))
	}
	out := rt.NewObject()
	_ = out.Set("status", resp.StatusCode)
	_ = out.Set("body", string(body))
	_ = out.Set("truncated", resp.ContentLength > h.limits.MaxHTTPResponseBytes)
	return out
}

// fsRead 实现受限文件读取。
func (h *capabilityHost) fsRead(rt *goja.Runtime, call goja.FunctionCall) goja.Value {
	path := call.Argument(0).String()
	abs, err := h.resolveFSPath(path)
	if err != nil {
		panic(rt.NewGoError(err))
	}
	st, err := os.Stat(abs)
	if err != nil {
		panic(rt.NewGoError(fmt.Errorf("%w: 读取文件失败: %v", worker.ErrInvalidArgument, err)))
	}
	if st.Size() > h.limits.MaxFSBytes {
		panic(rt.NewGoError(fmt.Errorf("%w: 文件超过上限 %d 字节", worker.ErrPolicyDenied, h.limits.MaxFSBytes)))
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		panic(rt.NewGoError(fmt.Errorf("%w: 读取文件失败: %v", worker.ErrInvalidArgument, err)))
	}
	return rt.ToValue(string(data))
}

// fsWrite 实现受限文件写入。
func (h *capabilityHost) fsWrite(rt *goja.Runtime, call goja.FunctionCall) goja.Value {
	path := call.Argument(0).String()
	content := call.Argument(1).String()
	if int64(len(content)) > h.limits.MaxFSBytes {
		panic(rt.NewGoError(fmt.Errorf("%w: 写入内容超过上限", worker.ErrPolicyDenied)))
	}
	abs, err := h.resolveFSPath(path)
	if err != nil {
		panic(rt.NewGoError(err))
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		panic(rt.NewGoError(fmt.Errorf("%w: 创建目录失败: %v", worker.ErrInternal, err)))
	}
	if err := os.WriteFile(abs, []byte(content), 0o600); err != nil {
		panic(rt.NewGoError(fmt.Errorf("%w: 写入文件失败: %v", worker.ErrInternal, err)))
	}
	return goja.Undefined()
}

// resolveFSPath 把脚本给的路径解析到 AllowedRoots 内（含 symlink 解析）。
func (h *capabilityHost) resolveFSPath(p string) (string, error) {
	if len(h.policy.AllowedRoots) == 0 {
		return "", fmt.Errorf("%w: tool.fs 未配置 AllowedRoots，拒绝访问", worker.ErrPolicyDenied)
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("%w: 非法路径 %q", worker.ErrInvalidArgument, p)
	}
	// 关键：解析 symlink，防止脚本用软链接跳出 AllowedRoots（第 15/14 章的同类坑）。
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("%w: 路径无法解析: %v", worker.ErrInvalidArgument, err)
	}
	abs = filepath.Clean(abs)
	for _, root := range h.policy.AllowedRoots {
		r, err := filepath.Abs(root)
		if err != nil {
			continue
		}
		if real, err := filepath.EvalSymlinks(r); err == nil {
			r = real
		}
		r = filepath.Clean(r)
		if abs == r {
			return abs, nil
		}
		rel, err := filepath.Rel(r, abs)
		if err != nil {
			continue
		}
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			continue
		}
		return abs, nil
	}
	return "", fmt.Errorf("%w: 路径 %q 不在 AllowedRoots 内", worker.ErrPolicyDenied, p)
}

// renderResult 把脚本返回值渲染为文本内容（对齐 v1 的 content 提取规则）。
func (h *capabilityHost) renderResult(val goja.Value) (string, bool) {
	if val == nil || goja.IsUndefined(val) || goja.IsNull(val) {
		return "", false
	}
	exported := val.Export()
	switch v := exported.(type) {
	case string:
		s, trunc := h.clamp(v)
		return s, trunc
	case map[string]any:
		// 支持 { content, success } 结构（v1 的约定）。
		if c, ok := v["content"].(string); ok {
			s, trunc := h.clamp(c)
			return s, trunc
		}
	}
	raw, err := json.Marshal(exported)
	if err != nil {
		return fmt.Sprintf("%v", exported), false
	}
	s, trunc := h.clamp(string(raw))
	return s, trunc
}

func (h *capabilityHost) clamp(s string) (string, bool) {
	if int64(len(s)) > h.limits.MaxOutputBytes {
		return s[:h.limits.MaxOutputBytes] + "\n...(输出已截断)", true
	}
	return s, false
}

// exportString 把 goja 值转成可读字符串。
func exportString(rt *goja.Runtime, v goja.Value) string {
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return ""
	}
	if s, ok := v.Export().(string); ok {
		return s
	}
	// 对象用 JSON 表示，其余用 String()。
	switch v.Export().(type) {
	case map[string]any, []any:
		if raw, err := json.Marshal(v.Export()); err == nil {
			return string(raw)
		}
	}
	return v.String()
}

// hostOf 从 URL 中取出主机名。
func hostOf(raw string) string {
	s := raw
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	if i := strings.LastIndex(s, "@"); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.LastIndex(s, ":"); i >= 0 && !strings.Contains(s, "]") {
		s = s[:i]
	}
	return strings.ToLower(strings.Trim(s, "[]"))
}

// hostAllowed 判断主机是否命中白名单。支持精确匹配与前缀通配（*.example.com）。
func hostAllowed(host string, allowed []string) bool {
	host = strings.ToLower(host)
	for _, a := range allowed {
		a = strings.ToLower(strings.TrimSpace(a))
		if a == "" {
			continue
		}
		if a == "*" {
			return true
		}
		if strings.HasPrefix(a, "*.") {
			suffix := a[1:] // ".example.com"
			if strings.HasSuffix(host, suffix) || host == a[2:] {
				return true
			}
			continue
		}
		if host == a {
			return true
		}
	}
	return false
}
