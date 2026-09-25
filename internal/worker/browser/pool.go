package browser

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/worker"
)

// execLookPath 包装 exec.LookPath。
func execLookPath(name string) (string, error) { return exec.LookPath(name) }

// runtimeGOOS / errorsIsDeadline 是本包内部的小别名，
// 目的是把标准库依赖集中在一处，方便测试替换。
func runtimeGOOS() string { return runtime.GOOS }

func errorsIsDeadline(err error) bool { return errors.Is(err, context.DeadlineExceeded) }

// 浏览器动作名。与 v1 的工具名一一对应（功能对等），但去掉了 browser_ 前缀，
// 因为 kind=browser 已经表达了领域；工具名 → 动作名的映射表见 MapToolNameToAction。
const (
	ActionNavigate       = "navigate"
	ActionScreenshot     = "screenshot"
	ActionClick          = "click"
	ActionType           = "type"
	ActionGetContent     = "get_content"
	ActionExecuteJS      = "execute_js"
	ActionNetworkMonitor = "network_monitor"
	ActionClosePage      = "close_page"
	ActionNewPage        = "new_page"
)

// ToolNameToAction 把任务 04 传入的工具名映射为浏览器动作名。
//
// 这张表就是任务书"需要主Agent裁决"里提到的 WorkerRequest.Action 与
// ToolRequest.ToolName 的映射约定，08 号可据此核对两侧是否对得上。
var ToolNameToAction = map[string]string{
	"browser_navigate":        ActionNavigate,
	"browser_screenshot":      ActionScreenshot,
	"browser_click":           ActionClick,
	"browser_type":            ActionType,
	"browser_get_content":     ActionGetContent,
	"browser_execute_js":      ActionExecuteJS,
	"browser_network_monitor": ActionNetworkMonitor,
}

// Worker 是一个浏览器 Worker：browser process → context → page 三层结构。
//
// 一个 Worker 实例 = 一个浏览器进程 + 一个（或按需多个）隔离 context。
// 池化由 Manager 负责（默认 4 个 slot），因此"一个 worker 内存异常"只影响它自己。
type Worker struct {
	id    string
	cfg   Config
	store worker.ArtifactStore

	mu       sync.Mutex
	proc     BrowserProcess
	bctx     BrowserContext
	page     Page
	started  bool
	closed   bool
	lastUsed time.Time
	// crashReason 记录驱动层检测到的不可恢复错误，供 NeedRecycle 上报。
	crashReason string
	userDataDir string
	// pages 允许一个 context 内多页（v1 只有单页）。
	pages map[string]Page
}

// NewWorker 创建浏览器 Worker。
func NewWorker(id string, cfg Config) *Worker {
	cfg = cfg.withDefaults()
	if cfg.ArtifactStore == nil {
		cfg.ArtifactStore = worker.NoopArtifactStore{}
	}
	if cfg.Driver == nil {
		cfg.Driver = &HeadlessProcessDriver{}
	}
	return &Worker{
		id:    id,
		cfg:   cfg,
		store: cfg.ArtifactStore,
		pages: map[string]Page{},
	}
}

// NewWorkerFromSpec 从 worker.Spec 构造（Manager Factory）。
func NewWorkerFromSpec(spec worker.Spec) (worker.Worker, error) {
	cfg := Config{
		Headless:        spec.Bool("headless", true),
		ExecutablePath:  spec.String("executable_path", ""),
		UserDataRoot:    spec.String("user_data_root", ""),
		ViewportWidth:   spec.Int("viewport_width", 0),
		ViewportHeight:  spec.Int("viewport_height", 0),
		NavigateTimeout: spec.Duration("navigate_timeout", 0),
		ActionTimeout:   spec.Duration("action_timeout", 0),
		IdleTimeout:     spec.Duration("idle_timeout", 0),
		ExtraArgs:       spec.Strings("extra_args"),
	}
	if n := spec.Int("max_memory_bytes", 0); n > 0 {
		cfg.MaxMemoryBytes = uint64(n)
	}
	if n := spec.Int("max_pages_per_context", 0); n > 0 {
		cfg.MaxPagesPerContext = n
	}
	w := NewWorker(spec.ID, cfg)
	w.userDataDir = userDataDirFor(cfg.UserDataRoot, spec.ID)
	return w, nil
}

// userDataDirFor 给每个 worker 独立的 user data 目录，保证 cookie/storage 隔离。
func userDataDirFor(root, id string) string {
	if root == "" {
		return ""
	}
	safe := strings.NewReplacer("/", "_", "\\", "_", ":", "_", " ", "_").Replace(id)
	return root + string(sep()) + safe
}

func sep() rune {
	if isWindows() {
		return '\\'
	}
	return '/'
}

func isWindows() bool { return runtimeGOOS() == "windows" }

// ID 实现 worker.Worker。
func (w *Worker) ID() string { return w.id }

// Kind 实现 worker.Worker。
func (w *Worker) Kind() string { return "browser" }

// Start 实现 worker.Worker：拉起 browser process → 建 context → 建首个 page。
//
// 三层按序创建，任何一层失败都整体失败（由 Manager 走退避重启），
// 不做"跳层继续跑"的兜底——那正是文档要废弃的模式。
func (w *Worker) Start(ctx context.Context) error {
	w.mu.Lock()
	if w.started {
		w.mu.Unlock()
		return nil
	}
	w.closed = false
	w.mu.Unlock()

	launchCtx, cancel := context.WithTimeout(ctx, w.cfg.NavigateTimeout+15*time.Second)
	defer cancel()

	bp, err := w.cfg.Driver.Launch(launchCtx, w.cfg, w.userDataDir)
	if err != nil {
		return err
	}
	bctx, err := bp.NewContext(launchCtx)
	if err != nil {
		_ = bp.Close(launchCtx)
		return fmt.Errorf("创建 browser context 失败: %w", err)
	}
	page, err := bctx.NewPage(launchCtx)
	if err != nil {
		_ = bctx.Close(launchCtx)
		_ = bp.Close(launchCtx)
		return fmt.Errorf("创建 page 失败: %w", err)
	}

	w.mu.Lock()
	w.proc = bp
	w.bctx = bctx
	w.page = page
	w.pages = map[string]Page{page.ID(): page}
	w.started = true
	w.lastUsed = time.Now()
	w.crashReason = ""
	w.mu.Unlock()
	return nil
}

// Health 实现 worker.Worker：检查三层结构是否都还活着。
//
// 这是"detect exit"的关键：浏览器进程被随机 kill 后，Connected() 变 false，
// Manager 的 sweep 会立刻发现并走恢复链条（验收标准"随机 kill Browser Worker 后
// Engine 不退出"）。
func (w *Worker) Health(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return fmt.Errorf("%w: browser worker 已关闭", worker.ErrUnavailable)
	}
	if !w.started {
		return fmt.Errorf("%w: browser worker 未启动", worker.ErrWorkerNotStarted)
	}
	if w.proc == nil || !w.proc.Connected() {
		return fmt.Errorf("%w: 浏览器进程已断开", worker.ErrCrash)
	}
	if w.bctx == nil {
		return fmt.Errorf("%w: browser context 缺失", worker.ErrCrash)
	}
	if w.page == nil || w.page.Closed() {
		// page 崩溃可以就地恢复（重建 page），不必回收整个 worker——
		// 这正是分层结构的价值：能局部恢复的不升级为整 worker 回收。
		return nil
	}
	return nil
}

// NeedRecycle 实现 worker.DrainingWorker：自报需要回收的原因。
//
// 触发条件：
//   - 浏览器进程已断开（健康检查失败）；
//   - 空闲超过 IdleTimeout（长期占用内存，v1 是 5 分钟全局空闲回收）。
//
// Manager 收到原因后会走 recycle：只回收这一个 worker（force cleanup → recycle），
// 不会影响池内其他 worker，更不会影响 Engine。
func (w *Worker) NeedRecycle() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || !w.started {
		return ""
	}
	if w.crashReason != "" {
		return w.crashReason
	}
	if w.proc == nil || !w.proc.Connected() {
		return "浏览器进程已断开"
	}
	if w.cfg.IdleTimeout > 0 && !w.lastUsed.IsZero() &&
		time.Since(w.lastUsed) > w.cfg.IdleTimeout {
		return fmt.Sprintf("空闲超过 %s", w.cfg.IdleTimeout)
	}
	return ""
}

// Stop 实现 worker.Worker：按 page → context → browser process 逆序关闭。
// 浏览器进程树由 procguard 保证整棵退出（不留 renderer/GPU 孤儿）。
func (w *Worker) Stop(ctx context.Context) error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	page := w.page
	pages := make([]Page, 0, len(w.pages))
	for _, p := range w.pages {
		pages = append(pages, p)
	}
	bctx := w.bctx
	bp := w.proc
	w.page = nil
	w.pages = map[string]Page{}
	w.bctx = nil
	w.proc = nil
	w.started = false
	w.mu.Unlock()

	// 逆序关闭：page → context → process。
	for _, p := range pages {
		if p != nil {
			_ = p.Close(ctx)
		}
	}
	_ = page
	if bctx != nil {
		_ = bctx.Close(ctx)
	}
	if bp != nil {
		_ = bp.Close(ctx)
	}
	return nil
}

// PIDs 实现 worker.ProcessAware：上报浏览器进程及其全部后代 PID，
// 供 Manager 在 Worker 崩溃后清理进程树（红线要求）。
func (w *Worker) PIDs() []int {
	w.mu.Lock()
	bp := w.proc
	w.mu.Unlock()
	if bp == nil {
		return nil
	}
	pid := bp.PID()
	if pid <= 0 {
		return nil
	}
	// 返回主进程 + 后代（procguard 的 Descendants 在 Worker 包内不可见，
	// 因此这里把主进程 PID 交给 Manager 的 killProcessTree 递归处理）。
	return []int{pid}
}

// 各动作的请求参数结构。

type navigateArgs struct {
	URL     string `json:"url"`
	Timeout int64  `json:"timeout_ms,omitempty"`
	NewPage bool   `json:"new_page,omitempty"`
}

type screenshotArgs struct {
	Selector string `json:"selector,omitempty"`
	FullPage bool   `json:"full_page,omitempty"`
}

type clickArgs struct {
	Selector   string `json:"selector"`
	Timeout    int64  `json:"timeout_ms,omitempty"`
	Screenshot bool   `json:"screenshot,omitempty"`
}

type typeArgs struct {
	Selector string `json:"selector"`
	Text     string `json:"text"`
	Timeout  int64  `json:"timeout_ms,omitempty"`
}

type contentArgs struct {
	Selector  string `json:"selector,omitempty"`
	MaxLength int    `json:"max_length,omitempty"`
}

type execJSArgs struct {
	Code string `json:"code"`
}

type networkArgs struct {
	Filter     string `json:"filter,omitempty"`
	MaxResults int    `json:"max_results,omitempty"`
}

// Execute 实现 worker.Worker：把动作路由到 Page 层。
//
// 每个动作都：
//   - 受 ActionTimeout 约束（v1 是各处硬编码 10s/15s/30s）；
//   - 更新 lastUsed（用于空闲回收判定）；
//   - 把驱动层的错误归一化为 worker 错误码（保留可重试标记）。
func (w *Worker) Execute(ctx context.Context, req worker.WorkerRequest) (worker.WorkerResponse, error) {
	started := time.Now()

	action := req.Action
	if mapped, ok := ToolNameToAction[action]; ok {
		action = mapped
	}

	// 取当前 page（含 page 崩溃后自动重建）。
	page, err := w.acquirePage(ctx)
	if err != nil {
		return worker.ErrorResponseFrom(req, w.id, started, err), nil
	}
	defer w.touch()

	switch action {
	case ActionNavigate:
		var a navigateArgs
		if err := req.Bind(&a); err != nil {
			return worker.ErrorResponse(req, w.id, worker.CodeInvalidArgument, err.Error()), nil
		}
		if strings.TrimSpace(a.URL) == "" {
			return worker.ErrorResponse(req, w.id, worker.CodeInvalidArgument, "url 不能为空"), nil
		}
		to := w.timeoutOr(a.Timeout, w.cfg.NavigateTimeout)
		res, err := page.Navigate(ctx, a.URL, to)
		if err != nil {
			return w.fail(req, started, err), nil
		}
		return w.ok(req, started, map[string]any{
			"url": res.URL, "title": res.Title, "statusCode": res.StatusCode,
			"content": fmt.Sprintf("已导航到：%s\n页面标题：%s", res.URL, res.Title),
		})

	case ActionScreenshot:
		var a screenshotArgs
		if err := req.Bind(&a); err != nil {
			return worker.ErrorResponse(req, w.id, worker.CodeInvalidArgument, err.Error()), nil
		}
		shot, err := page.Screenshot(ctx, a.Selector, a.FullPage)
		if err != nil {
			return w.fail(req, started, err), nil
		}
		// 关键：截图字节落 CAS，只把引用放进响应（第 8 章 payload limit + 第 21 章）。
		ref, err := w.store.Put(ctx, fmt.Sprintf("%s-screenshot.png", req.CallID), "image/png", shot.Data)
		if err != nil {
			return w.fail(req, started, fmt.Errorf("%w: 截图落 CAS 失败: %v", worker.ErrInternal, err)), nil
		}
		resp, _ := worker.OKResponse(req, w.id, started, map[string]any{
			"content":     fmt.Sprintf("已截图（%d 字节）", len(shot.Data)),
			"sizeBytes":   len(shot.Data),
			"selector":    a.Selector,
			"fullPage":    a.FullPage,
			"mime":        shot.MIME,
			"displayType": "image",
		})
		resp.Metrics.OutputBytes = int64(len(shot.Data))
		if ref != "" {
			resp.Artifacts = append(resp.Artifacts, worker.Artifact{
				Name: "screenshot.png", MIME: "image/png", Ref: ref, SizeByte: int64(len(shot.Data)),
			})
		} else {
			// 未接入 CAS 时不能把二进制塞进 JSON（会给事件/日志带来巨大 payload），
			// 因此明确报"未落存储"，而不是静默丢失。
			resp.Status = worker.StatusError
			resp.Error = worker.NewWorkerError(worker.CodeInternal,
				"截图已生成但未配置 ArtifactStore（CAS），为避免大 payload 未放入响应", false)
		}
		return resp, nil

	case ActionClick:
		var a clickArgs
		if err := req.Bind(&a); err != nil {
			return worker.ErrorResponse(req, w.id, worker.CodeInvalidArgument, err.Error()), nil
		}
		if a.Selector == "" {
			return worker.ErrorResponse(req, w.id, worker.CodeInvalidArgument, "selector 不能为空"), nil
		}
		if err := page.Click(ctx, a.Selector, w.timeoutOr(a.Timeout, w.cfg.ActionTimeout)); err != nil {
			return w.fail(req, started, err), nil
		}
		return w.ok(req, started, map[string]any{
			"content":  fmt.Sprintf("已点击元素：%s", a.Selector),
			"selector": a.Selector,
		})

	case ActionType:
		var a typeArgs
		if err := req.Bind(&a); err != nil {
			return worker.ErrorResponse(req, w.id, worker.CodeInvalidArgument, err.Error()), nil
		}
		if a.Selector == "" {
			return worker.ErrorResponse(req, w.id, worker.CodeInvalidArgument, "selector 不能为空"), nil
		}
		if err := page.Type(ctx, a.Selector, a.Text, w.timeoutOr(a.Timeout, w.cfg.ActionTimeout)); err != nil {
			return w.fail(req, started, err), nil
		}
		return w.ok(req, started, map[string]any{
			"content":  fmt.Sprintf("已在 %q 中输入 %d 个字符", a.Selector, len(a.Text)),
			"selector": a.Selector,
			// 注意：不回显输入内容的前缀，避免把用户输入的密码类文本写进事件/日志（第 21 章）。
			"textLength": len(a.Text),
		})

	case ActionGetContent:
		var a contentArgs
		if err := req.Bind(&a); err != nil {
			return worker.ErrorResponse(req, w.id, worker.CodeInvalidArgument, err.Error()), nil
		}
		if a.MaxLength <= 0 {
			a.MaxLength = 10000 // 对齐 v1 默认值
		}
		if a.MaxLength > 50000 {
			a.MaxLength = 50000 // 对齐 v1 上限
		}
		res, err := page.GetContent(ctx, a.Selector, a.MaxLength)
		if err != nil {
			return w.fail(req, started, err), nil
		}
		return w.ok(req, started, map[string]any{
			"content":   res.Text,
			"length":    res.Length,
			"truncated": res.Truncated,
		})

	case ActionExecuteJS:
		var a execJSArgs
		if err := req.Bind(&a); err != nil {
			return worker.ErrorResponse(req, w.id, worker.CodeInvalidArgument, err.Error()), nil
		}
		raw, err := page.ExecuteJS(ctx, a.Code)
		if err != nil {
			return w.fail(req, started, err), nil
		}
		// 对齐 v1：结果以 JSON 文本给出并设上限，避免超长返回值打爆 payload。
		text := string(raw)
		const maxJSResult = 30000
		if len(text) > maxJSResult {
			text = text[:maxJSResult] + "\n...(结果已截断)"
		}
		return w.ok(req, started, map[string]any{
			"content":     text,
			"displayType": "code",
		})

	case ActionNetworkMonitor:
		var a networkArgs
		if err := req.Bind(&a); err != nil {
			return worker.ErrorResponse(req, w.id, worker.CodeInvalidArgument, err.Error()), nil
		}
		if a.MaxResults <= 0 {
			a.MaxResults = 30 // 对齐 v1 默认值
		}
		if a.MaxResults > 100 {
			a.MaxResults = 100 // 对齐 v1 上限
		}
		reqs, err := page.NetworkRequests(ctx, a.Filter, a.MaxResults)
		if err != nil {
			return w.fail(req, started, err), nil
		}
		return w.ok(req, started, map[string]any{
			"content":  formatNetwork(reqs),
			"count":    len(reqs),
			"requests": reqs,
		})

	case ActionNewPage:
		p, err := w.newPage(ctx)
		if err != nil {
			return worker.ErrorResponseFrom(req, w.id, started, err), nil
		}
		return w.ok(req, started, map[string]any{
			"content": fmt.Sprintf("已新建页面 %s", p.ID()),
			"pageId":  p.ID(),
		})

	case ActionClosePage:
		if err := w.closeCurrentPage(ctx); err != nil {
			return worker.ErrorResponseFrom(req, w.id, started, err), nil
		}
		return w.ok(req, started, map[string]any{"content": "已关闭当前页面"})

	default:
		return worker.ErrorResponse(req, w.id, worker.CodeInvalidArgument,
			fmt.Sprintf("未知的浏览器动作 %q", req.Action)), nil
	}
}

// acquirePage 返回当前可用 page；page 崩溃时就地重建（不升级为整 worker 回收）。
func (w *Worker) acquirePage(ctx context.Context) (Page, error) {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil, fmt.Errorf("%w: browser worker 已关闭", worker.ErrUnavailable)
	}
	if w.proc == nil || !w.proc.Connected() {
		w.mu.Unlock()
		return nil, fmt.Errorf("%w: 浏览器进程已断开", worker.ErrCrash)
	}
	page := w.page
	if page != nil && !page.Closed() {
		w.mu.Unlock()
		return page, nil
	}
	bctx := w.bctx
	w.mu.Unlock()

	if bctx == nil {
		return nil, fmt.Errorf("%w: browser context 缺失", worker.ErrCrash)
	}
	// page 级恢复：重建一个 page，worker 与其浏览器进程继续复用。
	newPage, err := bctx.NewPage(ctx)
	if err != nil {
		w.mu.Lock()
		w.crashReason = "page 重建失败: " + err.Error()
		w.mu.Unlock()
		return nil, fmt.Errorf("%w: page 重建失败: %v", worker.ErrCrash, err)
	}
	w.mu.Lock()
	w.page = newPage
	w.pages[newPage.ID()] = newPage
	w.mu.Unlock()
	return newPage, nil
}

func (w *Worker) newPage(ctx context.Context) (Page, error) {
	w.mu.Lock()
	bctx := w.bctx
	// 上限在 Worker 层再校验一次：这是本层要保证的策略，
	// 不能只依赖具体驱动实现（换驱动就可能被绕过）。
	if len(w.pages) >= w.cfg.MaxPagesPerContext {
		limit := w.cfg.MaxPagesPerContext
		w.mu.Unlock()
		return nil, fmt.Errorf("%w: 单 context 页面数已达上限 %d", worker.ErrUnavailable, limit)
	}
	w.mu.Unlock()
	if bctx == nil {
		return nil, fmt.Errorf("%w: browser context 缺失", worker.ErrCrash)
	}
	p, err := bctx.NewPage(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", worker.ErrUnavailable, err)
	}
	w.mu.Lock()
	w.page = p
	w.pages[p.ID()] = p
	w.mu.Unlock()
	return p, nil
}

func (w *Worker) closeCurrentPage(ctx context.Context) error {
	w.mu.Lock()
	p := w.page
	if p != nil {
		delete(w.pages, p.ID())
	}
	w.page = nil
	w.mu.Unlock()
	if p == nil {
		return nil
	}
	return p.Close(ctx)
}

func (w *Worker) touch() {
	w.mu.Lock()
	w.lastUsed = time.Now()
	w.mu.Unlock()
}

func (w *Worker) timeoutOr(ms int64, def time.Duration) time.Duration {
	if ms <= 0 {
		return def
	}
	d := time.Duration(ms) * time.Millisecond
	// 不允许超过策略默认值的 10 倍，避免调用方绕过超时约束。
	if d > def*10 {
		return def * 10
	}
	return d
}

// ok 构造成功响应。
func (w *Worker) ok(req worker.WorkerRequest, started time.Time, body map[string]any) (worker.WorkerResponse, error) {
	resp, err := worker.OKResponse(req, w.id, started, body)
	return resp, err
}

// fail 把驱动层错误归一化为 worker 错误码。
func (w *Worker) fail(req worker.WorkerRequest, started time.Time, err error) worker.WorkerResponse {
	// 上下文超时 → 可重试的 timeout；其余驱动错误 → upstream（外部依赖）。
	if ctxErr := ctxTimeout(err); ctxErr != nil {
		resp := worker.ErrorResponse(req, w.id, worker.CodeTimeout, err.Error())
		resp.StartedAt = started
		return resp
	}
	resp := worker.ErrorResponse(req, w.id, worker.CodeUpstream, err.Error())
	resp.StartedAt = started
	return resp
}

func ctxTimeout(err error) error {
	if err == nil {
		return nil
	}
	if errorsIsDeadline(err) {
		return err
	}
	return nil
}

// formatNetwork 生成人读的网络请求列表（对齐 v1 的 Markdown 分组风格）。
func formatNetwork(reqs []NetworkRequest) string {
	if len(reqs) == 0 {
		return "未捕获到网络请求"
	}
	var sb strings.Builder
	for _, r := range reqs {
		u := r.URL
		if len(u) > 120 {
			u = u[:120] + "..."
		}
		fmt.Fprintf(&sb, "- %s %s", r.Method, u)
		if r.Status != 0 {
			fmt.Fprintf(&sb, " [%d]", r.Status)
		}
		sb.WriteByte('\n')
	}
	return sb.String()
}
