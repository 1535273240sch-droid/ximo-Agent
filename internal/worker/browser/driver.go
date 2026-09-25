// Package browser 实现 BrowserPool：多个 BrowserWorker，每个 worker 的结构是
// browser process → context → page（对应任务书第 16 章）。
//
// 与 v1 的根本差异（v1 审计结论）：
//
//	v1 是 BrowserManager 全局单例，只有一个 browser/context/page，所有工具调用共享它，
//	并发调用会在同一页面上互相干扰；只有 5 分钟空闲超时，没有租约、没有心跳、
//	没有崩溃事件监听，也没有内存限制。
//
// v2 的设计：
//   - 池化：默认 4 个 worker（任务书要求），每个 worker 是独立进程 + 独立 context，
//     因此"浏览器内存异常只回收该 worker，不会让整个 Engine OOM"是结构性保证；
//   - 租约：Acquire → use → heartbeat → release，租约超时由 Manager 强制
//     回收该 worker 并重启；
//   - 层次结构：每个 worker 严格按 browser process → context → page 三层管理生命周期，
//     context 是隔离边界（相当于独立会话，不共享 cookie/storage）。
//
// 关于浏览器驱动：本包不绑定 Playwright/CDP 的具体 Go 库，而是通过 Driver 接口注入。
// 原因有二：一是任务书要求"不引入未提及的第三方依赖"；二是 CDP 客户端选型
// （chromedp / rod / 自研）属于集成决策，应由 08 号统一拍板。
// 本包提供 HeadlessProcessDriver（拉起真实浏览器进程 + 进程树管控）作为默认实现，
// 以及一个可测试的脚本化驱动。
package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/worker"
	"github.com/ximo888ok-netizen/ximo-agent/internal/worker/procguard"
)

// Config 是浏览器 Worker 的配置。
type Config struct {
	// Headless 是否无头模式。默认 true。
	Headless bool
	// ExecutablePath 浏览器可执行文件（chromium/chrome/msedge）。空则自动探测。
	ExecutablePath string
	// UserDataRoot 是用户数据目录根。每个 worker 使用其下的独立子目录，
	// 保证 worker 之间 cookie/storage 完全隔离（v1 共享一个 context 的问题）。
	UserDataRoot string
	// Viewport 默认视口。
	ViewportWidth  int
	ViewportHeight int
	// NavigateTimeout 默认导航超时。
	NavigateTimeout time.Duration
	// ActionTimeout 默认元素操作超时（v1 是硬编码 10s）。
	ActionTimeout time.Duration
	// IdleTimeout 空闲多久后主动回收该 worker（v1 是 5 分钟全局空闲）。
	IdleTimeout time.Duration
	// MaxMemoryBytes 单 worker 内存上限（Windows 经 Job Object 生效）。
	// 超过后 DrainingWorker 会自报需要回收——这正是"内存异常只回收该 worker"的落点。
	MaxMemoryBytes uint64
	// MaxPagesPerContext 单 context 允许的最大 page 数。
	MaxPagesPerContext int
	// ExtraArgs 传给浏览器的附加启动参数。
	ExtraArgs []string
	// LaunchArgs 完全覆盖默认启动参数（谨慎使用）。
	LaunchArgs []string
	// Driver 为 nil 时使用 HeadlessProcessDriver。
	Driver Driver
	// ArtifactStore 用于把截图落 CAS，只把引用放进响应（第 8/21 章）。
	ArtifactStore worker.ArtifactStore
}

func (c Config) withDefaults() Config {
	if c.ViewportWidth <= 0 {
		c.ViewportWidth = 1280
	}
	if c.ViewportHeight <= 0 {
		c.ViewportHeight = 800
	}
	if c.NavigateTimeout <= 0 {
		c.NavigateTimeout = 30 * time.Second
	}
	if c.ActionTimeout <= 0 {
		c.ActionTimeout = 10 * time.Second
	}
	if c.IdleTimeout <= 0 {
		c.IdleTimeout = 5 * time.Minute
	}
	if c.MaxPagesPerContext <= 0 {
		c.MaxPagesPerContext = 8
	}
	if c.MaxMemoryBytes == 0 {
		// 单浏览器 worker 默认 1.5 GiB 上限：给足正常业务空间，同时把
		// "某个页面把内存吃到几十 G" 限制在单个 worker 内。
		c.MaxMemoryBytes = 1536 << 20
	}
	return c
}

// defaultLaunchArgs 是浏览器启动参数。
//
// 关键几项：
//   - --no-sandbox / --disable-setuid-sandbox：v1 也用了（容器/CI 环境必需）；
//   - --js-flags=--max-old-space-size：限制渲染进程 JS 堆，防止单页吃满内存；
//   - --disable-dev-shm-usage：避免 /dev/shm 在容器里过小导致崩溃；
//   - --headless=new：新版无头模式（更接近真实浏览器，反自动化检测更友好）。
func defaultLaunchArgs(headless bool, viewportW, viewportH int) []string {
	args := []string{
		"--no-sandbox",
		"--disable-setuid-sandbox",
		"--disable-dev-shm-usage",
		"--disable-gpu",
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-background-networking",
		"--disable-sync",
		"--disable-extensions",
		"--disable-default-apps",
		"--metrics-recording-only",
		"--mute-audio",
		"--hide-scrollbars",
		"--js-flags=--max-old-space-size=512",
		fmt.Sprintf("--window-size=%d,%d", viewportW, viewportH),
	}
	if headless {
		args = append(args, "--headless=new")
	}
	return args
}

// Driver 是浏览器驱动抽象。
//
// 层次结构由本包管理，Driver 只负责"三类原子操作"：
//
//	launch/close      —— browser process 生命周期
//	newContext/closeContext —— context 生命周期（隔离边界）
//	newPage/closePage + 页面动作 —— page 生命周期与具体交互
//
// 这样把"browser process → context → page"的结构固定在本包，
// 换驱动（chromedp / rod / 原生 CDP）不需要改池化与租约逻辑。
type Driver interface {
	// Launch 拉起浏览器进程并返回句柄。
	Launch(ctx context.Context, cfg Config, userDataDir string) (BrowserProcess, error)
}

// BrowserProcess 是对一个已启动的浏览器进程的抽象。
type BrowserProcess interface {
	// PID 返回浏览器主进程 PID（用于进程树清理）。
	PID() int
	// NewContext 新建一个隔离上下文（独立 cookie/storage）。
	NewContext(ctx context.Context) (BrowserContext, error)
	// Connected 报告进程是否仍然连通。
	Connected() bool
	// Close 关闭浏览器进程及其全部后代进程。
	Close(ctx context.Context) error
}

// BrowserContext 是一个隔离的浏览器上下文。
type BrowserContext interface {
	// NewPage 新建页面。
	NewPage(ctx context.Context) (Page, error)
	// Close 关闭上下文及其所有页面。
	Close(ctx context.Context) error
	// ID 返回上下文标识（用于日志与审计）。
	ID() string
}

// Page 是一个浏览器页面。
type Page interface {
	// ID 返回页面标识。
	ID() string
	// Closed 报告页面是否已关闭/崩溃。
	Closed() bool
	// Navigate 导航到 URL。
	Navigate(ctx context.Context, url string, timeout time.Duration) (NavigateResult, error)
	// Screenshot 截图。fullPage 为真时截整页，否则截视口；selector 非空时截该元素。
	Screenshot(ctx context.Context, selector string, fullPage bool) (ScreenshotResult, error)
	// Click 点击元素。
	Click(ctx context.Context, selector string, timeout time.Duration) error
	// Type 清空并输入文本。
	Type(ctx context.Context, selector, text string, timeout time.Duration) error
	// GetContent 提取文本内容。
	GetContent(ctx context.Context, selector string, maxLength int) (ContentResult, error)
	// ExecuteJS 在页面上下文中执行 JS。
	ExecuteJS(ctx context.Context, code string) (json.RawMessage, error)
	// NetworkRequests 返回捕获的网络请求（受上限约束）。
	NetworkRequests(ctx context.Context, filter string, max int) ([]NetworkRequest, error)
	// Close 关闭页面。
	Close(ctx context.Context) error
}

// NavigateResult 是导航结果。
type NavigateResult struct {
	URL   string `json:"url"`
	Title string `json:"title"`
	// StatusCode 为 0 表示驱动未提供。
	StatusCode int `json:"statusCode,omitempty"`
}

// ScreenshotResult 是截图结果。Data 是原始 PNG 字节，由 Worker 落 CAS。
type ScreenshotResult struct {
	Data   []byte `json:"-"`
	MIME   string `json:"mime"`
	Width  int    `json:"width,omitempty"`
	Height int    `json:"height,omitempty"`
}

// ContentResult 是内容提取结果。
type ContentResult struct {
	Text      string `json:"text"`
	Length    int    `json:"length"`
	Truncated bool   `json:"truncated"`
}

// NetworkRequest 是一条被捕获的网络请求。
type NetworkRequest struct {
	Method     string `json:"method"`
	URL        string `json:"url"`
	Status     int    `json:"status,omitempty"`
	ResourceTy string `json:"resourceType,omitempty"`
}

// ============================== 驱动：HeadlessProcessDriver ==============================

// HeadlessProcessDriver 通过拉起真实浏览器进程实现 Driver。
//
// 它负责本任务书明确要求的"进程树管控"部分：
//   - 浏览器进程经 procguard 拉起（Windows Job Object / Unix 进程组），
//     因此 worker 被回收时整棵浏览器进程树（含 GPU/renderer/utility 子进程）一起退出；
//   - 支持内存上限与进程数上限（Windows Job Object 内核级生效）；
//   - ExitHandler 可注入真正的 CDP 客户端（集成期由 08 号接线）。
//
// Page 层动作默认返回"未接入 CDP"的明确错误，而不是静默成功——
// 静默成功会让上层以为操作生效了，是比报错更危险的行为。
type HeadlessProcessDriver struct {
	// PageFactory 在 context 上创建 Page 的实现。集成期注入真实 CDP 实现。
	PageFactory func(ctx context.Context, bp *procBrowserProcess) (Page, func(context.Context) error, error)
}

// Launch 实现 Driver。
func (d *HeadlessProcessDriver) Launch(ctx context.Context, cfg Config, userDataDir string) (BrowserProcess, error) {
	exe := cfg.ExecutablePath
	if exe == "" {
		found, err := detectBrowser()
		if err != nil {
			return nil, err
		}
		exe = found
	}
	args := cfg.LaunchArgs
	if len(args) == 0 {
		args = defaultLaunchArgs(cfg.Headless, cfg.ViewportWidth, cfg.ViewportHeight)
	}
	args = append(args, cfg.ExtraArgs...)
	if userDataDir != "" {
		if err := os.MkdirAll(userDataDir, 0o700); err != nil {
			return nil, fmt.Errorf("创建 user data dir 失败: %w", err)
		}
		args = append(args, "--user-data-dir="+userDataDir)
	}
	// 关键：--remote-debugging-port=0 让浏览器自己选端口并在 DevToolsActivePort 文件里
	// 写出实际端口，避免固定端口的冲突与探测面。CDP 客户端据此连接。
	args = append(args, "--remote-debugging-port=0")

	pc := procguard.Config{
		Path:             exe,
		Args:             args,
		Dir:              userDataDir,
		MaxOutputBytes:   256 << 10, // 浏览器 stderr 只留少量用于诊断
		KillGrace:        5 * time.Second,
		MaxProcesses:     64, // 浏览器自带多进程；给足但设上限
		MemoryLimitBytes: cfg.MaxMemoryBytes,
		HideWindow:       true,
		CreateNoWindow:   true,
	}
	p, err := procguard.Start(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("启动浏览器失败: %w", err)
	}
	return &procBrowserProcess{proc: p, cfg: cfg, driver: d, userDataDir: userDataDir}, nil
}

// detectBrowser 在常见位置探测浏览器可执行文件。
func detectBrowser() (string, error) {
	candidates := browserCandidates()
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c, nil
		}
	}
	// 退回 PATH 查找。
	for _, name := range browserNames() {
		if p, err := lookPath(name); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("%w: 未找到浏览器可执行文件，请设置 browser.executable_path", worker.ErrUnavailable)
}

func browserCandidates() []string {
	switch runtime.GOOS {
	case "windows":
		var out []string
		for _, root := range []string{os.Getenv("PROGRAMFILES"), os.Getenv("PROGRAMFILES(X86)"), os.Getenv("LOCALAPPDATA")} {
			if root == "" {
				continue
			}
			out = append(out,
				filepath.Join(root, `Google\Chrome\Application\chrome.exe`),
				filepath.Join(root, `Microsoft\Edge\Application\msedge.exe`),
				filepath.Join(root, `Chromium\Application\chrome.exe`),
			)
		}
		return out
	case "darwin":
		return []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
		}
	default:
		return []string{
			"/usr/bin/google-chrome",
			"/usr/bin/chromium",
			"/usr/bin/chromium-browser",
			"/usr/bin/microsoft-edge",
			"/snap/bin/chromium",
		}
	}
}

func browserNames() []string {
	switch runtime.GOOS {
	case "windows":
		return []string{"chrome.exe", "msedge.exe", "chromium.exe"}
	default:
		return []string{"google-chrome", "chromium", "chromium-browser", "microsoft-edge"}
	}
}

// procBrowserProcess 是基于真实浏览器进程的 BrowserProcess 实现。
type procBrowserProcess struct {
	proc        *procguard.Proc
	cfg         Config
	driver      *HeadlessProcessDriver
	userDataDir string

	mu       sync.Mutex
	contexts map[string]*procBrowserContext
	closed   bool
	seq      int
}

func (b *procBrowserProcess) PID() int { return b.proc.PID() }

func (b *procBrowserProcess) Connected() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return !b.closed && b.proc != nil && !b.proc.Exited()
}

func (b *procBrowserProcess) NewContext(ctx context.Context) (BrowserContext, error) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil, fmt.Errorf("%w: 浏览器进程已关闭", worker.ErrUnavailable)
	}
	b.seq++
	id := fmt.Sprintf("ctx-%d", b.seq)
	bc := &procBrowserContext{id: id, bp: b, pages: map[string]Page{}}
	b.contexts[id] = bc
	b.mu.Unlock()
	return bc, nil
}

func (b *procBrowserProcess) Close(ctx context.Context) error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	ctxs := make([]*procBrowserContext, 0, len(b.contexts))
	for _, c := range b.contexts {
		ctxs = append(ctxs, c)
	}
	b.contexts = map[string]*procBrowserContext{}
	b.mu.Unlock()

	for _, c := range ctxs {
		_ = c.Close(ctx)
	}
	// 关掉浏览器进程树：procguard 的 Kill+Close 会终止整棵树
	// （Windows Job Object / Unix 进程组），不留 GPU/renderer 孤儿。
	_ = b.proc.Kill(ctx)
	_ = b.proc.Close()
	return nil
}

// procBrowserContext 是隔离上下文。
type procBrowserContext struct {
	id     string
	bp     *procBrowserProcess
	mu     sync.Mutex
	pages  map[string]Page
	seq    int
	closed bool
}

func (c *procBrowserContext) ID() string { return c.id }

func (c *procBrowserContext) NewPage(ctx context.Context) (Page, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, fmt.Errorf("%w: context 已关闭", worker.ErrUnavailable)
	}
	if len(c.pages) >= c.bp.cfg.MaxPagesPerContext {
		return nil, fmt.Errorf("%w: 单 context 页面数已达上限 %d",
			worker.ErrUnavailable, c.bp.cfg.MaxPagesPerContext)
	}
	c.seq++
	id := fmt.Sprintf("%s-page-%d", c.id, c.seq)

	// 若注入了 PageFactory（真实 CDP 客户端），用它创建；否则用未接入实现的占位 Page。
	var (
		page   Page
		closer func(context.Context) error
		err    error
	)
	if c.bp.driver != nil && c.bp.driver.PageFactory != nil {
		page, closer, err = c.bp.driver.PageFactory(ctx, c.bp)
		if err != nil {
			return nil, err
		}
	} else {
		page = &unconnectedPage{id: id}
		closer = func(context.Context) error { return nil }
	}
	if page == nil {
		return nil, fmt.Errorf("%w: PageFactory 返回 nil", worker.ErrInternal)
	}
	c.pages[id] = &managedPage{Page: page, closer: closer}
	return c.pages[id], nil
}

func (c *procBrowserContext) Close(ctx context.Context) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	pages := make([]Page, 0, len(c.pages))
	for _, p := range c.pages {
		pages = append(pages, p)
	}
	c.pages = map[string]Page{}
	c.mu.Unlock()

	for _, p := range pages {
		_ = p.Close(ctx)
	}
	return nil
}

// managedPage 把可选的 closer 与 Page 组合起来（便于 Close 时释放驱动资源）。
type managedPage struct {
	Page
	closer func(context.Context) error
}

func (m *managedPage) Close(ctx context.Context) error {
	err := m.Page.Close(ctx)
	if m.closer != nil {
		if cerr := m.closer(ctx); cerr != nil && err == nil {
			err = cerr
		}
	}
	return err
}

// unconnectedPage 是尚未接入 CDP 客户端时的 Page 实现。
//
// 它**明确报错**而不是假装成功：静默返回空结果会让上层误判操作已生效，
// 比直接失败危险得多（第 13 章"不允许兜底继续跑"的精神）。
type unconnectedPage struct {
	id     string
	closed bool
	mu     sync.Mutex
}

var errDriverNotWired = fmt.Errorf("%w: 浏览器驱动未接入 CDP 客户端（请注入 Config.Driver / PageFactory）", worker.ErrUnavailable)

func (p *unconnectedPage) ID() string { return p.id }

func (p *unconnectedPage) Closed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

func (p *unconnectedPage) Navigate(context.Context, string, time.Duration) (NavigateResult, error) {
	return NavigateResult{}, errDriverNotWired
}

func (p *unconnectedPage) Screenshot(context.Context, string, bool) (ScreenshotResult, error) {
	return ScreenshotResult{}, errDriverNotWired
}

func (p *unconnectedPage) Click(context.Context, string, time.Duration) error {
	return errDriverNotWired
}

func (p *unconnectedPage) Type(context.Context, string, string, time.Duration) error {
	return errDriverNotWired
}

func (p *unconnectedPage) GetContent(context.Context, string, int) (ContentResult, error) {
	return ContentResult{}, errDriverNotWired
}

func (p *unconnectedPage) ExecuteJS(context.Context, string) (json.RawMessage, error) {
	return nil, errDriverNotWired
}

func (p *unconnectedPage) NetworkRequests(context.Context, string, int) ([]NetworkRequest, error) {
	return nil, errDriverNotWired
}

func (p *unconnectedPage) Close(context.Context) error {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	return nil
}

// lookPath 隔离 exec.LookPath 依赖。
var lookPath = func(name string) (string, error) { return execLookPath(name) }

// ErrUnavailableDriver 供测试断言"未接入驱动"这一状态。
var ErrUnavailableDriver = errors.New("browser: 驱动未接入")

// ensure strings used
var _ = strings.TrimSpace
