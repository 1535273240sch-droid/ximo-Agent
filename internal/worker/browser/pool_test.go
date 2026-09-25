package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/worker"
)

// ============================== 可编程的假驱动（替代真实浏览器） ==============================

type fakePage struct {
	id       string
	mu       sync.Mutex
	closed   bool
	navCount int
	// failNext 让下一次动作返回错误。
	failNext error
}

func (p *fakePage) ID() string { return p.id }
func (p *fakePage) Closed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

func (p *fakePage) takeErr() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	err := p.failNext
	p.failNext = nil
	return err
}

func (p *fakePage) Navigate(ctx context.Context, url string, timeout time.Duration) (NavigateResult, error) {
	if err := p.takeErr(); err != nil {
		return NavigateResult{}, err
	}
	p.mu.Lock()
	p.navCount++
	p.mu.Unlock()
	return NavigateResult{URL: url, Title: "标题-" + url}, nil
}

func (p *fakePage) Screenshot(ctx context.Context, selector string, fullPage bool) (ScreenshotResult, error) {
	if err := p.takeErr(); err != nil {
		return ScreenshotResult{}, err
	}
	return ScreenshotResult{Data: []byte("\x89PNG-fake"), MIME: "image/png", Width: 1280, Height: 800}, nil
}

func (p *fakePage) Click(ctx context.Context, selector string, timeout time.Duration) error {
	return p.takeErr()
}

func (p *fakePage) Type(ctx context.Context, selector, text string, timeout time.Duration) error {
	return p.takeErr()
}

func (p *fakePage) GetContent(ctx context.Context, selector string, maxLength int) (ContentResult, error) {
	if err := p.takeErr(); err != nil {
		return ContentResult{}, err
	}
	text := strings.Repeat("content ", 50)
	trunc := false
	if len(text) > maxLength {
		text = text[:maxLength]
		trunc = true
	}
	return ContentResult{Text: text, Length: len(text), Truncated: trunc}, nil
}

func (p *fakePage) ExecuteJS(ctx context.Context, code string) (json.RawMessage, error) {
	if err := p.takeErr(); err != nil {
		return nil, err
	}
	return json.RawMessage(`{"jsResult":"ok"}`), nil
}

func (p *fakePage) NetworkRequests(ctx context.Context, filter string, max int) ([]NetworkRequest, error) {
	if err := p.takeErr(); err != nil {
		return nil, err
	}
	return []NetworkRequest{
		{Method: "GET", URL: "https://example.com/a", Status: 200, ResourceTy: "xhr"},
	}, nil
}

func (p *fakePage) Close(ctx context.Context) error {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	return nil
}

type fakeBrowserProcess struct {
	pid      int
	mu       sync.Mutex
	closed   bool
	ctxs     []*fakeBrowserContext
	connectF bool
}

func (b *fakeBrowserProcess) PID() int { return b.pid }

func (b *fakeBrowserProcess) NewContext(ctx context.Context) (BrowserContext, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, errors.New("browser closed")
	}
	bc := &fakeBrowserContext{id: "ctx"}
	b.ctxs = append(b.ctxs, bc)
	return bc, nil
}

func (b *fakeBrowserProcess) Connected() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return !b.closed && !b.connectF
}

func (b *fakeBrowserProcess) Close(ctx context.Context) error {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	return nil
}

// disconnect 模拟"浏览器进程被 kill"。
func (b *fakeBrowserProcess) disconnect() {
	b.mu.Lock()
	b.connectF = true
	b.mu.Unlock()
}

type fakeBrowserContext struct {
	id     string
	mu     sync.Mutex
	closed bool
	pages  []*fakePage
	seq    int
}

func (c *fakeBrowserContext) ID() string { return c.id }

func (c *fakeBrowserContext) NewPage(ctx context.Context) (Page, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, errors.New("context closed")
	}
	c.seq++
	// 页面 ID 必须唯一：Worker 用 ID 作为 pages map 的键，
	// 若驱动返回重复 ID 会让"页面数上限"失效（实测踩到的假驱动缺陷）。
	p := &fakePage{id: fmt.Sprintf("page-%d", c.seq)}
	c.pages = append(c.pages, p)
	return p, nil
}

func (c *fakeBrowserContext) Close(ctx context.Context) error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return nil
}

// fakeDriver 返回可编程的假浏览器进程。
type fakeDriver struct {
	mu        sync.Mutex
	procs     []*fakeBrowserProcess
	launchN   int
	launchErr error
}

func (d *fakeDriver) Launch(ctx context.Context, cfg Config, userDataDir string) (BrowserProcess, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.launchN++
	if d.launchErr != nil {
		return nil, d.launchErr
	}
	bp := &fakeBrowserProcess{pid: 1000 + d.launchN}
	d.procs = append(d.procs, bp)
	return bp, nil
}

func (d *fakeDriver) last() *fakeBrowserProcess {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.procs) == 0 {
		return nil
	}
	return d.procs[len(d.procs)-1]
}

// memArtifactStore 模拟 CAS 落盘。
type memArtifactStore struct {
	mu   sync.Mutex
	data map[string][]byte
}

func newMemStore() *memArtifactStore { return &memArtifactStore{data: map[string][]byte{}} }

func (m *memArtifactStore) Put(ctx context.Context, name, mime string, data []byte) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ref := "cas://" + name
	m.data[ref] = append([]byte(nil), data...)
	return ref, nil
}

func newFakeWorker(t *testing.T, cfg Config) (*Worker, *fakeDriver) {
	t.Helper()
	d := &fakeDriver{}
	cfg.Driver = d
	if cfg.ViewportWidth == 0 {
		cfg.ViewportWidth = 1280
	}
	return NewWorker("browser-0", cfg), d
}

func execBrowser(t *testing.T, w *Worker, action string, args any) worker.WorkerResponse {
	t.Helper()
	var raw json.RawMessage
	if args != nil {
		b, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		raw = b
	}
	resp, err := w.Execute(context.Background(), worker.WorkerRequest{
		CallID: "c1", Action: action, Args: raw,
	})
	if err != nil {
		t.Fatalf("Execute 返回错误: %v", err)
	}
	return resp
}

// ============================== 三层结构：browser → context → page ==============================

// TestStartBuildsThreeLayers 验证启动严格建立 browser process → context → page。
func TestStartBuildsThreeLayers(t *testing.T) {
	w, d := newFakeWorker(t, Config{})
	if err := w.Start(context.Background()); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	defer w.Stop(context.Background())

	if d.launchN != 1 {
		t.Errorf("应启动 1 个浏览器进程，实际 %d", d.launchN)
	}
	w.mu.Lock()
	hasProc := w.proc != nil
	hasCtx := w.bctx != nil
	hasPage := w.page != nil
	w.mu.Unlock()
	if !hasProc || !hasCtx || !hasPage {
		t.Errorf("三层结构不完整: proc=%v ctx=%v page=%v", hasProc, hasCtx, hasPage)
	}
	if err := w.Health(context.Background()); err != nil {
		t.Errorf("启动后应健康: %v", err)
	}
}

// TestIsolationBetweenWorkers 验证不同 Worker 实例使用独立浏览器进程
// （v1 全局单例的反面）。
func TestIsolationBetweenWorkers(t *testing.T) {
	d := &fakeDriver{}
	w1 := NewWorker("b-0", Config{Driver: d})
	w2 := NewWorker("b-1", Config{Driver: d})
	if err := w1.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := w2.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer w1.Stop(context.Background())
	defer w2.Stop(context.Background())

	if d.launchN != 2 {
		t.Fatalf("两个 worker 应各起一个浏览器进程，实际 %d", d.launchN)
	}
	w1.mu.Lock()
	p1 := w1.proc
	w1.mu.Unlock()
	w2.mu.Lock()
	p2 := w2.proc
	w2.mu.Unlock()
	if p1 == p2 {
		t.Error("两个 worker 不应共享同一个浏览器进程")
	}
}

// ============================== 崩溃检测（验收标准：随机 kill 后不拖垮 Engine） ==============================

// TestHealthDetectsKilledBrowser 验证浏览器进程被 kill 后健康检查失败。
func TestHealthDetectsKilledBrowser(t *testing.T) {
	w, d := newFakeWorker(t, Config{})
	_ = w.Start(context.Background())
	defer w.Stop(context.Background())

	if err := w.Health(context.Background()); err != nil {
		t.Fatalf("初始应健康: %v", err)
	}
	// 模拟浏览器进程被 kill。
	d.last().disconnect()

	err := w.Health(context.Background())
	if err == nil {
		t.Fatal("浏览器进程断开后健康检查应失败（detect exit）")
	}
	if !errors.Is(err, worker.ErrCrash) {
		t.Errorf("应归类为 crash，实际 %v", err)
	}
}

// TestNeedRecycleAfterDisconnect 验证自报回收（DrainingWorker）。
func TestNeedRecycleAfterDisconnect(t *testing.T) {
	w, d := newFakeWorker(t, Config{})
	_ = w.Start(context.Background())
	defer w.Stop(context.Background())

	if r := w.NeedRecycle(); r != "" {
		t.Errorf("健康时不应要求回收，实际: %s", r)
	}
	d.last().disconnect()
	if r := w.NeedRecycle(); r == "" {
		t.Error("浏览器断开后应自报需要回收")
	}
}

// TestNeedRecycleAfterIdle 验证空闲超时自报回收。
func TestNeedRecycleAfterIdle(t *testing.T) {
	w, _ := newFakeWorker(t, Config{IdleTimeout: 10 * time.Millisecond})
	_ = w.Start(context.Background())
	defer w.Stop(context.Background())

	time.Sleep(30 * time.Millisecond)
	if r := w.NeedRecycle(); r == "" {
		t.Error("空闲超过 IdleTimeout 应自报回收")
	}
}

// TestPIDsReportedForCleanup 验证上报 PID 供进程树清理。
func TestPIDsReportedForCleanup(t *testing.T) {
	w, _ := newFakeWorker(t, Config{})
	_ = w.Start(context.Background())
	defer w.Stop(context.Background())

	pids := w.PIDs()
	if len(pids) == 0 {
		t.Fatal("应上报浏览器进程 PID（供 Manager 崩溃后清理进程树）")
	}
}

// ============================== 动作映射与执行 ==============================

// TestToolNameToActionMapping 验证工具名 → 动作名映射表覆盖 v1 全部工具。
func TestToolNameToActionMapping(t *testing.T) {
	// v1 的浏览器工具清单，必须全部有映射（功能对等）。
	v1Tools := []string{
		"browser_navigate", "browser_screenshot", "browser_click", "browser_type",
		"browser_get_content", "browser_execute_js", "browser_network_monitor",
	}
	for _, name := range v1Tools {
		if _, ok := ToolNameToAction[name]; !ok {
			t.Errorf("v1 工具 %q 缺少动作映射", name)
		}
	}
}

// TestExecuteNavigate 验证导航动作。
func TestExecuteNavigate(t *testing.T) {
	w, _ := newFakeWorker(t, Config{})
	_ = w.Start(context.Background())
	defer w.Stop(context.Background())

	resp := execBrowser(t, w, ActionNavigate, navigateArgs{URL: "https://example.com"})
	if resp.Status != worker.StatusOK {
		t.Fatalf("期望成功，实际 %s (%v)", resp.Status, resp.Error)
	}
	var body struct {
		URL     string `json:"url"`
		Title   string `json:"title"`
		Content string `json:"content"`
	}
	_ = json.Unmarshal(resp.Result, &body)
	if body.URL != "https://example.com" || body.Title == "" {
		t.Errorf("导航结果不完整: %+v", body)
	}
}

// TestExecuteAcceptsV1ToolName 验证直接传 v1 工具名也能工作（兼容层）。
func TestExecuteAcceptsV1ToolName(t *testing.T) {
	w, _ := newFakeWorker(t, Config{})
	_ = w.Start(context.Background())
	defer w.Stop(context.Background())

	resp := execBrowser(t, w, "browser_navigate", navigateArgs{URL: "https://x.com"})
	if resp.Status != worker.StatusOK {
		t.Errorf("用 v1 工具名调用应成功，实际 %s (%v)", resp.Status, resp.Error)
	}
}

// TestExecuteNavigateRequiresURL 验证 URL 必填。
func TestExecuteNavigateRequiresURL(t *testing.T) {
	w, _ := newFakeWorker(t, Config{})
	_ = w.Start(context.Background())
	defer w.Stop(context.Background())

	resp := execBrowser(t, w, ActionNavigate, navigateArgs{URL: "  "})
	if resp.Error == nil || resp.Error.Code != worker.CodeInvalidArgument {
		t.Errorf("空 URL 应返回 invalid_argument，实际 %+v", resp.Error)
	}
}

// TestScreenshotGoesToCAS 验证截图字节落 CAS、只回传引用（第 8/21 章）。
func TestScreenshotGoesToCAS(t *testing.T) {
	store := newMemStore()
	w, _ := newFakeWorker(t, Config{ArtifactStore: store})
	_ = w.Start(context.Background())
	defer w.Stop(context.Background())

	resp := execBrowser(t, w, ActionScreenshot, screenshotArgs{FullPage: true})
	if resp.Status != worker.StatusOK {
		t.Fatalf("期望成功，实际 %s (%v)", resp.Status, resp.Error)
	}
	if len(resp.Artifacts) == 0 || resp.Artifacts[0].Ref == "" {
		t.Fatal("截图应产出一个 CAS 引用")
	}
	// 关键：响应体里不能出现图片字节（否则会污染事件/日志 payload）。
	if strings.Contains(string(resp.Result), "\x89PNG") {
		t.Error("截图字节不应出现在 JSON 响应里，只能出现 CAS 引用")
	}
	store.mu.Lock()
	_, ok := store.data[resp.Artifacts[0].Ref]
	store.mu.Unlock()
	if !ok {
		t.Error("CAS 里应能取到截图内容")
	}
}

// TestScreenshotWithoutCASReportsError 验证未配置 CAS 时明确报错而非静默丢图。
func TestScreenshotWithoutCASReportsError(t *testing.T) {
	w, _ := newFakeWorker(t, Config{ArtifactStore: worker.NoopArtifactStore{}})
	_ = w.Start(context.Background())
	defer w.Stop(context.Background())

	resp := execBrowser(t, w, ActionScreenshot, screenshotArgs{})
	if resp.Status != worker.StatusError {
		t.Errorf("未配置 CAS 时应明确报错，实际 %s", resp.Status)
	}
}

// TestExecuteClickAndType 验证点击与输入。
func TestExecuteClickAndType(t *testing.T) {
	w, _ := newFakeWorker(t, Config{})
	_ = w.Start(context.Background())
	defer w.Stop(context.Background())

	if resp := execBrowser(t, w, ActionClick, clickArgs{Selector: "#btn"}); resp.Status != worker.StatusOK {
		t.Errorf("点击应成功: %v", resp.Error)
	}
	resp := execBrowser(t, w, ActionType, typeArgs{Selector: "#in", Text: "secret-text"})
	if resp.Status != worker.StatusOK {
		t.Fatalf("输入应成功: %v", resp.Error)
	}
	// 第 21 章：输入内容不应回显进响应（可能是密码）。
	if strings.Contains(string(resp.Result), "secret-text") {
		t.Error("输入文本不应回显到响应中（可能是敏感信息）")
	}
}

// TestExecuteClickRequiresSelector 验证 selector 必填。
func TestExecuteClickRequiresSelector(t *testing.T) {
	w, _ := newFakeWorker(t, Config{})
	_ = w.Start(context.Background())
	defer w.Stop(context.Background())
	resp := execBrowser(t, w, ActionClick, clickArgs{})
	if resp.Error == nil || resp.Error.Code != worker.CodeInvalidArgument {
		t.Errorf("缺少 selector 应返回 invalid_argument，实际 %+v", resp.Error)
	}
}

// TestGetContentRespectsMaxLength 验证内容提取的长度上限（对齐 v1 的 10000/50000）。
func TestGetContentRespectsMaxLength(t *testing.T) {
	w, _ := newFakeWorker(t, Config{})
	_ = w.Start(context.Background())
	defer w.Stop(context.Background())

	resp := execBrowser(t, w, ActionGetContent, contentArgs{Selector: "body", MaxLength: 20})
	var body struct {
		Length    int  `json:"length"`
		Truncated bool `json:"truncated"`
	}
	_ = json.Unmarshal(resp.Result, &body)
	if body.Length > 20 {
		t.Errorf("长度应受 MaxLength 约束，实际 %d", body.Length)
	}
	if !body.Truncated {
		t.Error("超长内容应标记 truncated")
	}
}

// TestExecuteJSResultTruncated 验证 JS 结果超长被截断（对齐 v1 的 30000）。
func TestExecuteJSResultTruncated(t *testing.T) {
	w, _ := newFakeWorker(t, Config{})
	_ = w.Start(context.Background())
	defer w.Stop(context.Background())

	resp := execBrowser(t, w, ActionExecuteJS, execJSArgs{Code: "return 1;"})
	if resp.Status != worker.StatusOK {
		t.Fatalf("应成功: %v", resp.Error)
	}
	var body struct {
		DisplayType string `json:"displayType"`
	}
	_ = json.Unmarshal(resp.Result, &body)
	if body.DisplayType != "code" {
		t.Errorf("JS 结果 displayType 应为 code，实际 %s", body.DisplayType)
	}
}

// TestNetworkMonitorMaxResults 验证网络请求条数上限（对齐 v1 的 30/100）。
func TestNetworkMonitorMaxResults(t *testing.T) {
	w, _ := newFakeWorker(t, Config{})
	_ = w.Start(context.Background())
	defer w.Stop(context.Background())

	resp := execBrowser(t, w, ActionNetworkMonitor, networkArgs{MaxResults: 5})
	if resp.Status != worker.StatusOK {
		t.Fatalf("应成功: %v", resp.Error)
	}
	var body struct {
		Count int `json:"count"`
	}
	_ = json.Unmarshal(resp.Result, &body)
	if body.Count > 5 {
		t.Errorf("结果数应受 MaxResults 约束，实际 %d", body.Count)
	}
}

// TestExecuteUnknownAction 验证未知动作被拒绝。
func TestExecuteUnknownAction(t *testing.T) {
	w, _ := newFakeWorker(t, Config{})
	_ = w.Start(context.Background())
	defer w.Stop(context.Background())
	resp := execBrowser(t, w, "not_a_real_action", map[string]any{})
	if resp.Error == nil || resp.Error.Code != worker.CodeInvalidArgument {
		t.Errorf("未知动作应返回 invalid_argument，实际 %+v", resp.Error)
	}
}

// ============================== page 级恢复（分层结构的价值） ==============================

// TestPageCrashRecoversWithoutWorkerRecycle 验证 page 崩溃就地重建，
// 不升级为整个 worker 回收——这是三层结构相对 v1 单例的实际好处。
func TestPageCrashRecoversWithoutWorkerRecycle(t *testing.T) {
	w, d := newFakeWorker(t, Config{})
	_ = w.Start(context.Background())
	defer w.Stop(context.Background())

	// 模拟 page 崩溃（关闭但浏览器进程仍健康）。
	w.mu.Lock()
	page := w.page
	w.mu.Unlock()
	_ = page.Close(context.Background())

	// 健康检查不应失败（page 可恢复）。
	if err := w.Health(context.Background()); err != nil {
		t.Errorf("page 崩溃不应让整个 worker 不健康: %v", err)
	}
	// 下一次操作应自动重建 page 并成功。
	resp := execBrowser(t, w, ActionNavigate, navigateArgs{URL: "https://after-crash.com"})
	if resp.Status != worker.StatusOK {
		t.Errorf("page 崩溃后应能就地恢复，实际 %s (%v)", resp.Status, resp.Error)
	}
	// 浏览器进程没有被重启（launchN 仍为 1）。
	if d.launchN != 1 {
		t.Errorf("不应重启浏览器进程，实际 launch 次数 %d", d.launchN)
	}
}

// TestOperationFailsWhenBrowserDisconnected 验证浏览器断开后操作给出 crash 错误。
func TestOperationFailsWhenBrowserDisconnected(t *testing.T) {
	w, d := newFakeWorker(t, Config{})
	_ = w.Start(context.Background())
	defer w.Stop(context.Background())

	d.last().disconnect()
	resp := execBrowser(t, w, ActionNavigate, navigateArgs{URL: "https://x.com"})
	if resp.Error == nil {
		t.Fatal("浏览器断开后操作应失败")
	}
	if resp.Error.Code != worker.CodeCrash && resp.Error.Code != worker.CodeUpstream {
		t.Errorf("应归类为 crash/upstream，实际 %s", resp.Error.Code)
	}
}

// ============================== Driver 未接入时的诚实行为 ==============================

// TestUnconnectedPageFailsLoudly 验证未接入 CDP 时明确报错，
// 而不是静默返回空结果（静默成功比报错更危险）。
func TestUnconnectedPageFailsLoudly(t *testing.T) {
	p := &unconnectedPage{id: "p1"}
	if _, err := p.Navigate(context.Background(), "https://x.com", time.Second); err == nil {
		t.Error("未接入驱动时 Navigate 应明确报错")
	}
	if _, err := p.Screenshot(context.Background(), "", false); err == nil {
		t.Error("未接入驱动时 Screenshot 应明确报错")
	}
	if !errors.Is(errDriverNotWired, worker.ErrUnavailable) {
		t.Error("驱动未接入错误应归类为 unavailable")
	}
}

// ============================== 生命周期 ==============================

// TestStopClosesLayersInOrder 验证停机按 page → context → process 逆序关闭。
func TestStopClosesLayersInOrder(t *testing.T) {
	w, d := newFakeWorker(t, Config{})
	_ = w.Start(context.Background())

	w.mu.Lock()
	page := w.page
	ctxObj := w.bctx
	w.mu.Unlock()

	if err := w.Stop(context.Background()); err != nil {
		t.Fatalf("Stop 失败: %v", err)
	}
	if !page.(*fakePage).Closed() {
		t.Error("page 应被关闭")
	}
	if !ctxObj.(*fakeBrowserContext).closed {
		t.Error("context 应被关闭")
	}
	if d.last().Connected() {
		t.Error("浏览器进程应被关闭")
	}
	// 幂等。
	if err := w.Stop(context.Background()); err != nil {
		t.Errorf("重复 Stop 应幂等: %v", err)
	}
}

// TestHealthFailsAfterStop 验证停机后健康检查失败。
func TestHealthFailsAfterStop(t *testing.T) {
	w, _ := newFakeWorker(t, Config{})
	_ = w.Start(context.Background())
	_ = w.Stop(context.Background())
	if err := w.Health(context.Background()); err == nil {
		t.Error("停机后健康检查应失败")
	}
}

// TestStartFailurePropagates 验证启动失败被如实上报（不吞错）。
func TestStartFailurePropagates(t *testing.T) {
	d := &fakeDriver{launchErr: errors.New("浏览器没装")}
	w := NewWorker("b-0", Config{Driver: d})
	err := w.Start(context.Background())
	if err == nil {
		t.Fatal("启动失败应返回错误")
	}
	if !strings.Contains(err.Error(), "浏览器没装") {
		t.Errorf("错误信息应保留原因: %v", err)
	}
}

// TestKindAndID 验证身份方法。
func TestKindAndID(t *testing.T) {
	w, _ := newFakeWorker(t, Config{})
	if w.Kind() != "browser" {
		t.Errorf("Kind 应为 browser，实际 %s", w.Kind())
	}
	if w.ID() != "browser-0" {
		t.Errorf("ID 错误: %s", w.ID())
	}
}

// TestMaxPagesPerContextEnforced 验证单 context 页面数上限。
func TestMaxPagesPerContextEnforced(t *testing.T) {
	w, _ := newFakeWorker(t, Config{MaxPagesPerContext: 2})
	_ = w.Start(context.Background())
	defer w.Stop(context.Background())

	// 起始已 1 个 page，再新建 1 个应成功，第 3 个应失败。
	if resp := execBrowser(t, w, ActionNewPage, nil); resp.Status != worker.StatusOK {
		t.Fatalf("第 2 个页面应可创建: %v", resp.Error)
	}
	resp := execBrowser(t, w, ActionNewPage, nil)
	if resp.Error == nil {
		t.Error("超过 MaxPagesPerContext 应被拒绝")
	}
}

// TestNewWorkerFromSpec 验证从 Spec 构造（Manager Factory 路径）。
func TestNewWorkerFromSpec(t *testing.T) {
	spec := worker.Spec{
		ID: "browser-2", Kind: "browser",
		Config: map[string]any{
			"headless":        true,
			"viewport_width":  float64(1920),
			"viewport_height": float64(1080),
			"idle_timeout":    "2m",
		},
	}
	w, err := NewWorkerFromSpec(spec)
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	if w.ID() != "browser-2" || w.Kind() != "browser" {
		t.Errorf("身份错误: %s/%s", w.ID(), w.Kind())
	}
	bw := w.(*Worker)
	if bw.cfg.ViewportWidth != 1920 || bw.cfg.IdleTimeout != 2*time.Minute {
		t.Errorf("配置未正确读取: %+v", bw.cfg)
	}
}
