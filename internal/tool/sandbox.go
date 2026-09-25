package tool

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dop251/goja"
)

// ---------------------------------------------------------------------------
// 沙箱策略（任务04 定义，任务05 的 dynamic worker 宿主施加同样数值）
// ---------------------------------------------------------------------------

// SandboxPolicy 是 DynamicTool 沙箱的硬限制集合。对应第14章：
// memory limit / instruction limit / execution timeout / 禁止直接 OS 访问 /
// 禁止无限制文件系统 / 禁止任意网络。goja 本身不提供这些保证，全部由本策略
// 在运行时叠加。
type SandboxPolicy struct {
	// MaxInstructions 指令预算。goja 没有字节码计数器，实现上按“循环迭代”
	// 计数：每次循环迭代消耗 1 预算（见 instrumentLoops），耗尽即硬中断。
	MaxInstructions int64
	// MaxMemoryBytes 执行期间累计分配上限（通过 MemStats 采样 + 硬中断）。
	MaxMemoryBytes int64
	// Timeout 执行超时（硬中断；capability 调用同时受 ctx 截止约束）。
	Timeout time.Duration
	// MaxOutputBytes tool.output / 返回值大小上限。
	MaxOutputBytes int
	// MaxLogEntries tool.log 条数上限。
	MaxLogEntries int
	// MaxHTTPRequests tool.http 次数上限。
	MaxHTTPRequests int
	// MaxHTTPResponseBytes 单次 http 响应体上限。
	MaxHTTPResponseBytes int
	// MaxFSReadBytes / MaxFSWriteBytes 文件读写单次字节上限。
	MaxFSReadBytes  int
	MaxFSWriteBytes int
	// AllowedHTTPHosts tool.http 主机白名单（glob，如 "*.example.com"）。
	AllowedHTTPHosts []string
	// AllowedHTTPMethods 允许的 HTTP 方法（默认仅 GET）。
	AllowedHTTPMethods []string
	// AllowedFSRoots tool.fs 根目录白名单（绝对路径）。
	AllowedFSRoots []string
	// FSWritable 是否允许 tool.fs 写操作（默认只读）。
	FSWritable bool
	// MaxScriptBytes 脚本大小上限。
	MaxScriptBytes int
}

// DefaultSandboxPolicy 返回保守的默认沙箱策略。
func DefaultSandboxPolicy() SandboxPolicy {
	return SandboxPolicy{
		MaxInstructions:      1_000_000,
		MaxMemoryBytes:       64 << 20, // 64MB
		Timeout:              10 * time.Second,
		MaxOutputBytes:       1 << 20, // 1MB
		MaxLogEntries:        200,
		MaxHTTPRequests:      8,
		MaxHTTPResponseBytes: 1 << 20,
		MaxFSReadBytes:       1 << 20,
		MaxFSWriteBytes:      1 << 20,
		AllowedHTTPMethods:   []string{"GET"},
		AllowedFSRoots:       nil,
		FSWritable:           false,
		MaxScriptBytes:       256 << 10, // 256KB
	}
}

// ---------------------------------------------------------------------------
// 违规错误
// ---------------------------------------------------------------------------

// ViolationKind 沙箱违规类型。
type ViolationKind string

const (
	ViolationBannedAPI         ViolationKind = "banned_api"
	ViolationUnboundedLoop     ViolationKind = "unbounded_loop"
	ViolationScriptTooLarge    ViolationKind = "script_too_large"
	ViolationInstructionBudget ViolationKind = "instruction_budget"
	ViolationMemoryBudget      ViolationKind = "memory_budget"
	ViolationTimeout           ViolationKind = "timeout"
	ViolationOutputTooLarge    ViolationKind = "output_too_large"
	ViolationScanFailed        ViolationKind = "scan_failed"
	ViolationCompileFailed     ViolationKind = "compile_failed"
	ViolationReservedName      ViolationKind = "reserved_name"
	// ViolationCapabilityDenied capability 策略拒绝（主机/方法/路径白名单
	// 之外、超出配额等）。这类拒绝是安全边界，不允许被脚本 try/catch 吞掉。
	ViolationCapabilityDenied ViolationKind = "capability_denied"
)

// SandboxViolationError 沙箱策略违规。
type SandboxViolationError struct {
	Kind   ViolationKind
	Detail string
}

func (e *SandboxViolationError) Error() string {
	return fmt.Sprintf("sandbox 违规(%s): %s", e.Kind, e.Detail)
}

// ---------------------------------------------------------------------------
// Capability API（tool.input / tool.log / tool.http / tool.fs / tool.output）
// ---------------------------------------------------------------------------

// HTTPRequest tool.http 请求。
type HTTPRequest struct {
	Method  string
	URL     string
	Headers map[string]string
	Body    string
}

// HTTPResponse tool.http 响应。
type HTTPResponse struct {
	Status  int
	Body    string
	Headers map[string]string
}

// FSOp 文件操作类型。
type FSOp string

const (
	FSRead  FSOp = "read"
	FSWrite FSOp = "write"
	FSList  FSOp = "list"
	FSStat  FSOp = "stat"
)

// FSRequest tool.fs 请求。
type FSRequest struct {
	Op      FSOp
	Path    string
	Content string
}

// FSEntry 目录条目。
type FSEntry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size"`
}

// FSResponse tool.fs 响应。
type FSResponse struct {
	Content string    `json:"content,omitempty"`
	Entries []FSEntry `json:"entries,omitempty"`
	Size    int64     `json:"size,omitempty"`
	IsDir   bool      `json:"is_dir,omitempty"`
}

// CapabilityHost 是 capability API 的宿主实现。Worker（任务05）可以替换为
// 自己的实现（例如把 fs 路由到任务03 的 checkpoint 存储）。
type CapabilityHost interface {
	HTTP(ctx context.Context, req HTTPRequest) (HTTPResponse, error)
	FS(ctx context.Context, req FSRequest) (FSResponse, error)
}

// DefaultCapabilityHost 是基于策略白名单的默认宿主实现：
// http 只允许 AllowedHTTPHosts + AllowedHTTPMethods；fs 只允许
// AllowedFSRoots 之内的路径，且默认只读。
type DefaultCapabilityHost struct {
	policy   SandboxPolicy
	client   *http.Client
	mu       sync.Mutex
	httpUsed int
	fsUsed   int
}

// NewDefaultCapabilityHost 创建默认宿主。
func NewDefaultCapabilityHost(policy SandboxPolicy) *DefaultCapabilityHost {
	transport := &http.Transport{
		Proxy: nil, // 禁止走环境代理，避免绕过主机白名单
		DialContext: (&net.Dialer{
			Timeout: 5 * time.Second,
		}).DialContext,
		TLSClientConfig:   &tls.Config{MinVersion: tls.VersionTLS12},
		DisableKeepAlives: true,
	}
	return &DefaultCapabilityHost{
		policy: policy,
		client: &http.Client{
			Transport: transport,
			// 不设置全局 Timeout：由 ctx 截止 + 响应体读取上限控制。
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return errors.New("sandbox: 重定向次数超过 5")
				}
				if !hostAllowed(req.URL.Hostname(), policy.AllowedHTTPHosts) {
					return fmt.Errorf("sandbox: 重定向目标主机 %q 不在白名单", req.URL.Hostname())
				}
				return nil
			},
		},
	}
}

func hostAllowed(host string, patterns []string) bool {
	if len(patterns) == 0 {
		return false
	}
	for _, pattern := range patterns {
		if globMatch(pattern, host) {
			return true
		}
	}
	return false
}

// HTTP 实现 CapabilityHost。
func (h *DefaultCapabilityHost) HTTP(ctx context.Context, req HTTPRequest) (HTTPResponse, error) {
	h.mu.Lock()
	h.httpUsed++
	used := h.httpUsed
	h.mu.Unlock()
	if h.policy.MaxHTTPRequests > 0 && used > h.policy.MaxHTTPRequests {
		return HTTPResponse{}, fmt.Errorf("sandbox: http 调用次数超过上限 %d", h.policy.MaxHTTPRequests)
	}

	parsed, err := url.Parse(req.URL)
	if err != nil {
		return HTTPResponse{}, fmt.Errorf("sandbox: URL 解析失败: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return HTTPResponse{}, fmt.Errorf("sandbox: 仅允许 http/https，收到 %q", parsed.Scheme)
	}
	// 白名单同时匹配裸主机名与 host:port 两种写法。
	if !hostAllowed(parsed.Hostname(), h.policy.AllowedHTTPHosts) &&
		!hostAllowed(parsed.Host, h.policy.AllowedHTTPHosts) {
		return HTTPResponse{}, fmt.Errorf("sandbox: 主机 %q 不在白名单 %v", parsed.Host, h.policy.AllowedHTTPHosts)
	}
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	if method == "" {
		method = "GET"
	}
	allowed := false
	for _, m := range h.policy.AllowedHTTPMethods {
		if strings.EqualFold(m, method) {
			allowed = true
			break
		}
	}
	if !allowed {
		return HTTPResponse{}, fmt.Errorf("sandbox: HTTP 方法 %q 不在白名单 %v", method, h.policy.AllowedHTTPMethods)
	}

	var bodyReader io.Reader
	if req.Body != "" {
		bodyReader = strings.NewReader(req.Body)
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, req.URL, bodyReader)
	if err != nil {
		return HTTPResponse{}, fmt.Errorf("sandbox: 构造请求失败: %w", err)
	}
	for k, v := range req.Headers {
		httpReq.Header.Set(k, v)
	}

	resp, err := h.client.Do(httpReq)
	if err != nil {
		return HTTPResponse{}, fmt.Errorf("sandbox: 请求失败: %w", err)
	}
	defer resp.Body.Close()

	limit := h.policy.MaxHTTPResponseBytes
	if limit <= 0 {
		limit = 1 << 20
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit)+1))
	if err != nil {
		return HTTPResponse{}, fmt.Errorf("sandbox: 读取响应失败: %w", err)
	}
	if len(body) > limit {
		return HTTPResponse{}, fmt.Errorf("sandbox: 响应体超过上限 %d 字节", limit)
	}
	headers := make(map[string]string, len(resp.Header))
	for k := range resp.Header {
		headers[k] = resp.Header.Get(k)
	}
	return HTTPResponse{Status: resp.StatusCode, Body: string(body), Headers: headers}, nil
}

// FS 实现 CapabilityHost。路径必须解析到 AllowedFSRoots 之内（含 symlink
// 解析后的真实路径），否则拒绝。
func (h *DefaultCapabilityHost) FS(ctx context.Context, req FSRequest) (FSResponse, error) {
	h.mu.Lock()
	h.fsUsed++
	h.mu.Unlock()

	if req.Op == FSWrite && !h.policy.FSWritable {
		return FSResponse{}, errors.New("sandbox: 当前策略禁止文件写入")
	}
	target, err := h.resolvePath(req.Path)
	if err != nil {
		return FSResponse{}, err
	}

	switch req.Op {
	case FSRead:
		info, err := os.Stat(target)
		if err != nil {
			return FSResponse{}, err
		}
		if info.IsDir() {
			return FSResponse{}, errors.New("sandbox: 目标是目录，请使用 list")
		}
		if h.policy.MaxFSReadBytes > 0 && info.Size() > int64(h.policy.MaxFSReadBytes) {
			return FSResponse{}, fmt.Errorf("sandbox: 文件 %d 字节超过读取上限 %d", info.Size(), h.policy.MaxFSReadBytes)
		}
		data, err := os.ReadFile(target)
		if err != nil {
			return FSResponse{}, err
		}
		return FSResponse{Content: string(data), Size: info.Size()}, nil
	case FSWrite:
		if len(req.Content) > h.policy.MaxFSWriteBytes {
			return FSResponse{}, fmt.Errorf("sandbox: 写入内容 %d 字节超过上限 %d", len(req.Content), h.policy.MaxFSWriteBytes)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return FSResponse{}, err
		}
		if err := os.WriteFile(target, []byte(req.Content), 0o644); err != nil {
			return FSResponse{}, err
		}
		return FSResponse{Size: int64(len(req.Content))}, nil
	case FSList:
		entries, err := os.ReadDir(target)
		if err != nil {
			return FSResponse{}, err
		}
		out := make([]FSEntry, 0, len(entries))
		for _, e := range entries {
			info, err := e.Info()
			if err != nil {
				continue
			}
			out = append(out, FSEntry{Name: e.Name(), IsDir: e.IsDir(), Size: info.Size()})
		}
		return FSResponse{Entries: out}, nil
	case FSStat:
		info, err := os.Stat(target)
		if err != nil {
			return FSResponse{}, err
		}
		return FSResponse{Size: info.Size(), IsDir: info.IsDir()}, nil
	default:
		return FSResponse{}, fmt.Errorf("sandbox: 未知 fs 操作 %q", req.Op)
	}
}

func (h *DefaultCapabilityHost) resolvePath(path string) (string, error) {
	if path == "" {
		return "", errors.New("sandbox: fs 路径不能为空")
	}
	if len(h.policy.AllowedFSRoots) == 0 {
		return "", errors.New("sandbox: 未配置文件系统白名单根目录")
	}
	clean := filepath.Clean(path)
	abs := clean
	if !filepath.IsAbs(abs) {
		return "", fmt.Errorf("sandbox: fs 路径必须是绝对路径：%q", path)
	}
	for _, root := range h.policy.AllowedFSRoots {
		rootAbs, err := filepath.Abs(filepath.Clean(root))
		if err != nil {
			continue
		}
		// symlink 解析后复核包含关系（防 symlink 越权）。
		realRoot, err := filepath.EvalSymlinks(rootAbs)
		if err != nil {
			realRoot = rootAbs
		}
		realTarget, err := filepath.EvalSymlinks(abs)
		if err != nil {
			// 目标不存在时沿父目录解析。
			realTarget = evalSymlinksParent(abs)
		}
		if withinRoot(realTarget, realRoot) {
			return abs, nil
		}
	}
	return "", fmt.Errorf("sandbox: 路径 %q 不在白名单根目录 %v 内", path, h.policy.AllowedFSRoots)
}

func evalSymlinksParent(path string) string {
	dir := filepath.Dir(path)
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		return filepath.Join(real, filepath.Base(path))
	}
	return path
}

func withinRoot(target, root string) bool {
	if target == root {
		return true
	}
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// ---------------------------------------------------------------------------
// Sandbox 运行器
// ---------------------------------------------------------------------------

// SandboxResult 沙箱执行结果。
type SandboxResult struct {
	Content string
	Success bool
	Error   string
	Logs    []string
	Stats   SandboxStats
}

// SandboxStats 执行统计（供观测与审计）。
type SandboxStats struct {
	LoopIterations  int64
	HTTPCalls       int
	FSCalls         int
	OutputBytes     int
	Duration        time.Duration
	TimedOut        bool
	MemoryExceeded  bool
	BudgetExhausted bool
}

// Sandbox 是 goja 沙箱执行器。每次 Run 创建独立 VM（工具之间无状态共享），
// 看门狗 goroutine 由 Run 拥有并在返回前等待退出。
type Sandbox struct {
	policy SandboxPolicy
	host   CapabilityHost
	now    func() time.Time
}

// NewSandbox 使用默认 capability 宿主创建沙箱。
func NewSandbox(policy SandboxPolicy) *Sandbox {
	return &Sandbox{policy: policy, host: NewDefaultCapabilityHost(policy), now: time.Now}
}

// NewSandboxWithHost 使用自定义 capability 宿主创建沙箱（Worker 可注入）。
func NewSandboxWithHost(policy SandboxPolicy, host CapabilityHost) *Sandbox {
	return &Sandbox{policy: policy, host: host, now: time.Now}
}

// Policy 返回沙箱策略。
func (s *Sandbox) Policy() SandboxPolicy { return s.policy }

// sandboxReserved 沙箱内部标识符，用户代码引用即拒绝。
// args（工具输入参数名）不在此列：它与 v1 DynamicTool 契约一致。
var sandboxReserved = map[string]string{
	"__sbTick":   "沙箱内部预算函数",
	"__sbResult": "沙箱内部结果变量",
	"__sbError":  "沙箱内部错误变量",
}

// Run 在沙箱中执行 DynamicTool 代码。
// code 是函数体（v1 契约：接收 args，返回字符串或 {content, success}）。
func (s *Sandbox) Run(ctx context.Context, code string, input map[string]any) (SandboxResult, error) {
	start := s.now()

	if s.policy.MaxScriptBytes > 0 && len(code) > s.policy.MaxScriptBytes {
		return SandboxResult{}, &SandboxViolationError{Kind: ViolationScriptTooLarge, Detail: fmt.Sprintf("脚本 %d 字节超过上限 %d", len(code), s.policy.MaxScriptBytes)}
	}

	// 1. 静态扫描 + 插桩（fail-closed：无法可靠分析即拒绝）。
	tokens, err := scanJS(code)
	if err != nil {
		return SandboxResult{}, &SandboxViolationError{Kind: ViolationScanFailed, Detail: err.Error()}
	}
	if err := scanViolations(tokens); err != nil {
		return SandboxResult{}, err
	}
	for _, tok := range tokens {
		if tok.kind == tokIdent {
			if why, reserved := sandboxReserved[tok.text]; reserved {
				if !precededByDot(tokens, tok) {
					return SandboxResult{}, &SandboxViolationError{Kind: ViolationReservedName, Detail: fmt.Sprintf("标识符 %q 是%s，禁止使用", tok.text, why)}
				}
			}
		}
	}
	instrumented := instrumentLoops(code, tokens)

	// 2. 组装驱动脚本。args 是工具输入（与 v1 DynamicTool 契约一致），
	// __sbTick 是循环预算函数（由插桩注入调用）。
	driver := "(async function () { try { const __r = await (async function (args, __sbTick) {" +
		instrumented +
		"})(__sbInputArg, __sbTickFn); __sbResult = __r; } catch (e) { __sbError = (e && e.message) ? String(e.message) : String(e); } })();"

	prg, err := goja.Compile("dynamic_tool", driver, false)
	if err != nil {
		return SandboxResult{}, &SandboxViolationError{Kind: ViolationCompileFailed, Detail: err.Error()}
	}

	// 3. 创建 VM 并挂载 capability API。
	run := &sandboxRun{
		policy: s.policy,
		host:   s.host,
		ctx:    ctx,
		done:   make(chan struct{}),
		budget: s.policy.MaxInstructions,
	}
	run.vm = goja.New()
	if n := s.policy.MaxInstructions; n > 0 {
		run.vm.SetMaxCallStackSize(512)
	}
	toolObj := run.vm.NewObject()
	run.vm.Set("tool", toolObj)
	run.vm.Set("__sbInputArg", run.vm.ToValue(input))
	run.vm.Set("__sbTickFn", run.vm.ToValue(func(call goja.FunctionCall) goja.Value {
		remaining := atomic.AddInt64(&run.budget, -1)
		if remaining < 0 {
			run.budgetExhausted.Store(true)
			panic(run.vm.NewGoError(errors.New("sandbox: 指令预算耗尽")))
		}
		return goja.Undefined()
	}))
	run.mountCapabilities(toolObj, input)

	// 4. 看门狗：超时 / 内存 / 取消，全部走 vm.Interrupt 硬中断。
	var wg sync.WaitGroup
	wg.Add(1)
	watchCtx, cancelWatch := context.WithCancel(ctx)
	go func() {
		defer wg.Done()
		run.watchdog(watchCtx, start)
	}()

	// 5. 执行。
	_, runErr := run.vm.RunProgram(prg)
	close(run.done)
	cancelWatch()
	wg.Wait()

	stats := SandboxStats{
		LoopIterations:  s.policy.MaxInstructions - atomic.LoadInt64(&run.budget),
		HTTPCalls:       run.httpCalls,
		FSCalls:         run.fsCalls,
		Duration:        s.now().Sub(start),
		TimedOut:        run.interruptKind.Load() == "timeout",
		MemoryExceeded:  run.interruptKind.Load() == "memory",
		BudgetExhausted: run.budgetExhausted.Load(),
	}
	if stats.LoopIterations < 0 {
		stats.LoopIterations = 0
	}

	// 6. 归类失败原因。预算耗尽即使被脚本自己的 try/catch 捕获也要视为
	// 沙箱违规（硬限制，不随脚本错误处理逻辑放行）；capability 策略拒绝
	// 同理——安全边界不允许被脚本吞掉。
	if run.budgetExhausted.Load() {
		return SandboxResult{Stats: stats}, &SandboxViolationError{
			Kind:   ViolationInstructionBudget,
			Detail: fmt.Sprintf("循环迭代超过预算 %d", s.policy.MaxInstructions),
		}
	}
	if violation, ok := run.policyViolation.Load().(*SandboxViolationError); ok && violation != nil {
		return SandboxResult{Stats: stats}, violation
	}
	if runErr != nil {
		var interrupted *goja.InterruptedError
		if errors.As(runErr, &interrupted) {
			switch run.interruptKind.Load() {
			case "timeout":
				return SandboxResult{Stats: stats}, &SandboxViolationError{Kind: ViolationTimeout, Detail: fmt.Sprintf("执行超过 %s", s.policy.Timeout)}
			case "memory":
				return SandboxResult{Stats: stats}, &SandboxViolationError{Kind: ViolationMemoryBudget, Detail: fmt.Sprintf("累计分配超过 %d 字节", s.policy.MaxMemoryBytes)}
			case "canceled":
				return SandboxResult{Stats: stats}, context.Canceled
			}
		}
		return SandboxResult{Stats: stats}, &SandboxViolationError{Kind: ViolationCompileFailed, Detail: runErr.Error()}
	}

	// 7. 提取结果。
	result, err := run.extractResult()
	if err != nil {
		return SandboxResult{Stats: stats}, err
	}
	result.Stats = stats
	if s.policy.MaxOutputBytes > 0 && len(result.Content) > s.policy.MaxOutputBytes {
		return SandboxResult{Stats: stats}, &SandboxViolationError{Kind: ViolationOutputTooLarge, Detail: fmt.Sprintf("输出 %d 字节超过上限 %d", len(result.Content), s.policy.MaxOutputBytes)}
	}
	return result, nil
}

type sandboxRun struct {
	policy          SandboxPolicy
	host            CapabilityHost
	ctx             context.Context
	vm              *goja.Runtime
	done            chan struct{}
	budget          int64
	budgetExhausted atomic.Bool
	interruptKind   atomic.Value // string
	// policyViolation 粘性的 capability 策略违规：脚本即使 try/catch 了
	// 异常，沙箱仍在收尾时按违规失败（安全边界不可被脚本吞掉）。
	policyViolation atomic.Value // *SandboxViolationError
	httpCalls       int
	fsCalls         int
	mu              sync.Mutex
	logs            []string
	outputSet       bool
	outputValue     string
}

// denyCapability 记录一次 capability 策略拒绝（粘性，首次生效）。
func (r *sandboxRun) denyCapability(detail string) {
	r.policyViolation.CompareAndSwap(nil, &SandboxViolationError{Kind: ViolationCapabilityDenied, Detail: detail})
}

func (r *sandboxRun) watchdog(ctx context.Context, start time.Time) {
	deadline := start.Add(r.policy.Timeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()

	var memTicker *time.Ticker
	var memStop chan struct{}
	if r.policy.MaxMemoryBytes > 0 {
		memTicker = time.NewTicker(5 * time.Millisecond)
		defer memTicker.Stop()
		memStop = make(chan struct{})
		var startAlloc uint64 = readTotalAlloc()
		go func() {
			for {
				select {
				case <-memStop:
					return
				case <-memTicker.C:
					if readTotalAlloc()-startAlloc > uint64(r.policy.MaxMemoryBytes) {
						r.interrupt("memory")
						return
					}
				}
			}
		}()
		defer close(memStop)
	}

	select {
	case <-r.done:
		return
	case <-ctx.Done():
		r.interrupt("canceled")
		return
	case <-timer.C:
		r.interrupt("timeout")
		return
	}
}

func (r *sandboxRun) interrupt(kind string) {
	r.interruptKind.Store(kind)
	r.vm.Interrupt(fmt.Sprintf("sandbox: %s", kind))
}

// readTotalAlloc 读取进程累计分配字节数（内存预算的采样源）。
func readTotalAlloc() uint64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.TotalAlloc
}

func precededByDot(tokens []jsToken, tok jsToken) bool {
	for i, t := range tokens {
		if t.start == tok.start && t.end == tok.end {
			return i > 0 && tokens[i-1].kind == tokPunct && (tokens[i-1].text == "." || tokens[i-1].text == "?.")
		}
	}
	return false
}

// mountCapabilities 挂载 tool.input/log/http/fs/output/parseURL。
func (r *sandboxRun) mountCapabilities(toolObj *goja.Object, input map[string]any) {
	vm := r.vm

	toolObj.Set("input", vm.ToValue(input))

	// tool.log(msg) — 有界日志缓冲
	toolObj.Set("log", vm.ToValue(func(call goja.FunctionCall) goja.Value {
		if len(call.Arguments) == 0 {
			return goja.Undefined()
		}
		msg := call.Argument(0).String()
		r.mu.Lock()
		if r.policy.MaxLogEntries <= 0 || len(r.logs) < r.policy.MaxLogEntries {
			r.logs = append(r.logs, msg)
		}
		r.mu.Unlock()
		return goja.Undefined()
	}))

	// tool.output(value) — 显式设置最终输出
	toolObj.Set("output", vm.ToValue(func(call goja.FunctionCall) goja.Value {
		if len(call.Arguments) == 0 {
			return goja.Undefined()
		}
		r.mu.Lock()
		r.outputSet = true
		r.outputValue = call.Argument(0).String()
		r.mu.Unlock()
		return goja.Undefined()
	}))

	// tool.http({method,url,headers,body})
	toolObj.Set("http", vm.ToValue(func(call goja.FunctionCall) goja.Value {
		req := HTTPRequest{
			Method:  jsStringField(call.Argument(0), "method"),
			URL:     jsStringField(call.Argument(0), "url"),
			Body:    jsStringField(call.Argument(0), "body"),
			Headers: jsStringMapField(call.Argument(0), "headers"),
		}
		callCtx, cancel := r.capabilityContext()
		defer cancel()
		resp, err := r.host.HTTP(callCtx, req)
		r.mu.Lock()
		r.httpCalls++
		r.mu.Unlock()
		if err != nil {
			r.denyCapability(err.Error())
			panic(vm.NewGoError(err))
		}
		return vm.ToValue(map[string]any{
			"status":  resp.Status,
			"body":    resp.Body,
			"headers": resp.Headers,
		})
	}))

	// tool.fs({op,path,content})
	toolObj.Set("fs", vm.ToValue(func(call goja.FunctionCall) goja.Value {
		req := FSRequest{
			Op:      FSOp(jsStringField(call.Argument(0), "op")),
			Path:    jsStringField(call.Argument(0), "path"),
			Content: jsStringField(call.Argument(0), "content"),
		}
		callCtx, cancel := r.capabilityContext()
		defer cancel()
		resp, err := r.host.FS(callCtx, req)
		r.mu.Lock()
		r.fsCalls++
		r.mu.Unlock()
		if err != nil {
			r.denyCapability(err.Error())
			panic(vm.NewGoError(err))
		}
		return vm.ToValue(map[string]any{
			"content": resp.Content,
			"entries": resp.Entries,
			"size":    resp.Size,
			"is_dir":  resp.IsDir,
		})
	}))

	// tool.parseURL(url) — 纯函数，供动态工具解析 URL（不发起请求）
	toolObj.Set("parseURL", vm.ToValue(func(call goja.FunctionCall) goja.Value {
		raw := call.Argument(0).String()
		parsed, err := url.Parse(raw)
		if err != nil {
			panic(vm.NewGoError(fmt.Errorf("sandbox: URL 解析失败: %w", err)))
		}
		return vm.ToValue(map[string]any{
			"protocol": parsed.Scheme,
			"host":     parsed.Hostname(),
			"port":     parsed.Port(),
			"path":     parsed.Path,
			"query":    parsed.RawQuery,
		})
	}))
}

// jsStringField 从 JS 对象参数中读取字符串字段（不存在/非字符串时返回空串）。
// 不依赖 goja 的 ExportTo 字段名映射，显式读取更可控。
func jsStringField(arg goja.Value, key string) string {
	obj, ok := arg.(*goja.Object)
	if !ok || obj == nil {
		return ""
	}
	v := obj.Get(key)
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return ""
	}
	return v.String()
}

// jsStringMapField 从 JS 对象参数中读取 string->string 映射字段。
func jsStringMapField(arg goja.Value, key string) map[string]string {
	obj, ok := arg.(*goja.Object)
	if !ok || obj == nil {
		return nil
	}
	v := obj.Get(key)
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return nil
	}
	sub, ok := v.(*goja.Object)
	if !ok || sub == nil {
		return nil
	}
	out := make(map[string]string)
	for _, k := range sub.Keys() {
		out[k] = sub.Get(k).String()
	}
	return out
}

// capabilityContext 为单次 capability 调用派生带截止的 ctx：capability
// 不得比沙箱总超时活得更久（I9：所有 timeout 最终必须释放资源）。
func (r *sandboxRun) capabilityContext() (context.Context, context.CancelFunc) {
	base := r.ctx
	if base == nil {
		base = context.Background()
	}
	timeout := r.policy.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return context.WithTimeout(base, timeout)
}

// extractResult 从 VM 全局提取最终结果（v1 语义：返回值，或 {content, success}）。
func (r *sandboxRun) extractResult() (SandboxResult, error) {
	r.mu.Lock()
	logs := append([]string(nil), r.logs...)
	outputSet, outputValue := r.outputSet, r.outputValue
	r.mu.Unlock()

	result := SandboxResult{Logs: logs, Success: true}

	if errVal := r.vm.Get("__sbError"); errVal != nil && !goja.IsUndefined(errVal) && !goja.IsNull(errVal) {
		result.Success = false
		result.Error = errVal.String()
		return result, nil
	}

	if outputSet {
		result.Content = outputValue
		return result, nil
	}

	val := r.vm.Get("__sbResult")
	if val == nil || goja.IsUndefined(val) {
		result.Content = ""
		return result, nil
	}
	if s, ok := val.(goja.String); ok {
		result.Content = s.String()
		return result, nil
	}
	// 对象：兼容 {content, success}（v1 DynamicTool 契约）
	if obj, ok := val.(*goja.Object); ok {
		if content := obj.Get("content"); content != nil && !goja.IsUndefined(content) {
			result.Content = content.String()
			if success := obj.Get("success"); success != nil && !goja.IsUndefined(success) {
				result.Success = success.ToBoolean()
			}
			return result, nil
		}
	}
	var exported any
	if err := r.vm.ExportTo(val, &exported); err == nil {
		if data, err := json.Marshal(exported); err == nil {
			result.Content = string(data)
			return result, nil
		}
	}
	result.Content = val.String()
	return result, nil
}
