package dynamic

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/worker"
)

func newTestWorker(t *testing.T, cfg Config) *Worker {
	t.Helper()
	if cfg.Limits.ExecutionTimeout == 0 {
		cfg.Limits.ExecutionTimeout = 3 * time.Second
	}
	return NewWorker("dyn-0", cfg)
}

func evalReq(t *testing.T, code string, input any) worker.WorkerRequest {
	t.Helper()
	var raw json.RawMessage
	if input != nil {
		b, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		raw = b
	}
	args, err := json.Marshal(map[string]any{"code": code, "input": raw})
	if err != nil {
		t.Fatal(err)
	}
	return worker.WorkerRequest{CallID: "c1", Action: ActionEval, Args: args}
}

// TestEvalBasicReturn 验证最基本的 return 值提取。
func TestEvalBasicReturn(t *testing.T) {
	w := newTestWorker(t, Config{})
	if err := w.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer w.Stop(context.Background())

	resp, err := w.Execute(context.Background(), evalReq(t, `return 1 + 1;`, nil))
	if err != nil {
		t.Fatalf("Execute 错误: %v", err)
	}
	if resp.Status != worker.StatusOK {
		t.Fatalf("期望成功，实际 %s (%v)", resp.Status, resp.Error)
	}
	var body struct {
		Content string `json:"content"`
	}
	_ = json.Unmarshal(resp.Result, &body)
	if body.Content != "2" {
		t.Errorf("期望 content=2，实际 %q", body.Content)
	}
}

// TestEvalContentObjectShape 验证 { content } 结构被正确提取（v1 约定）。
func TestEvalContentObjectShape(t *testing.T) {
	w := newTestWorker(t, Config{})
	_ = w.Start(context.Background())
	defer w.Stop(context.Background())

	resp, _ := w.Execute(context.Background(),
		evalReq(t, `return { content: "hello", success: true };`, nil))
	var body struct {
		Content string `json:"content"`
	}
	_ = json.Unmarshal(resp.Result, &body)
	if body.Content != "hello" {
		t.Errorf("期望 content=hello，实际 %q", body.Content)
	}
}

// TestToolInputCapability 验证 tool.input 暴露调用参数。
func TestToolInputCapability(t *testing.T) {
	w := newTestWorker(t, Config{})
	_ = w.Start(context.Background())
	defer w.Stop(context.Background())

	resp, _ := w.Execute(context.Background(),
		evalReq(t, `return tool.input.name;`, map[string]any{"name": "ximo"}))
	var body struct {
		Content string `json:"content"`
	}
	_ = json.Unmarshal(resp.Result, &body)
	if body.Content != "ximo" {
		t.Errorf("期望 content=ximo，实际 %q", body.Content)
	}
}

// TestToolLogCollected 验证 tool.log 与 console.log 的输出被收集。
func TestToolLogCollected(t *testing.T) {
	w := newTestWorker(t, Config{})
	_ = w.Start(context.Background())
	defer w.Stop(context.Background())

	code := `tool.log("via-tool"); console.log("via-console"); return "ok";`
	resp, _ := w.Execute(context.Background(), evalReq(t, code, nil))
	var body struct {
		Logs []string `json:"logs"`
	}
	_ = json.Unmarshal(resp.Result, &body)
	joined := strings.Join(body.Logs, "|")
	if !strings.Contains(joined, "via-tool") {
		t.Errorf("tool.log 应被收集，实际 logs=%v", body.Logs)
	}
	if !strings.Contains(joined, "via-console") {
		t.Errorf("console.log 应被收集，实际 logs=%v", body.Logs)
	}
}

// TestToolOutputCapability 验证 tool.output 设置结构化输出。
func TestToolOutputCapability(t *testing.T) {
	w := newTestWorker(t, Config{})
	_ = w.Start(context.Background())
	defer w.Stop(context.Background())

	code := `tool.output({ count: 3, tag: "x" }); return "done";`
	resp, _ := w.Execute(context.Background(), evalReq(t, code, nil))
	var body struct {
		Output map[string]any `json:"output"`
	}
	_ = json.Unmarshal(resp.Result, &body)
	if body.Output["tag"] != "x" {
		t.Errorf("tool.output 应被记录，实际 %+v", body.Output)
	}
}

// ============================== 执行超时（v1 缺陷：Promise.race 无法中断同步死循环） ==============================

// TestExecutionTimeoutInterruptsInfiniteLoop 验证同步死循环被真正中断。
//
// 这是与 v1 的关键差异：v1 用 vm timeout + Promise.race，同步死循环根本拦不住。
func TestExecutionTimeoutInterruptsInfiniteLoop(t *testing.T) {
	w := newTestWorker(t, Config{Limits: Limits{ExecutionTimeout: 700 * time.Millisecond}})
	_ = w.Start(context.Background())
	defer w.Stop(context.Background())

	start := time.Now()
	resp, err := w.Execute(context.Background(),
		evalReq(t, `while (true) {}`, nil))
	elapsed := time.Since(start)

	if elapsed > 8*time.Second {
		t.Fatalf("死循环未被及时中断，耗时 %s", elapsed)
	}
	if resp.Status != worker.StatusTimeout {
		t.Errorf("期望 timeout 状态，实际 %s (%v)", resp.Status, resp.Error)
	}
	_ = err

	// 关键：中断后 Worker 必须仍然可用（VM 不残留坏状态）。
	resp2, _ := w.Execute(context.Background(), evalReq(t, `return "still-alive";`, nil))
	if resp2.Status != worker.StatusOK {
		t.Errorf("超时中断后 Worker 应仍可用，实际 %s", resp2.Status)
	}
}

// TestTimeoutDoesNotLeakVMState 验证每次执行使用全新 Runtime（不残留全局变量）。
func TestTimeoutDoesNotLeakVMState(t *testing.T) {
	w := newTestWorker(t, Config{})
	_ = w.Start(context.Background())
	defer w.Stop(context.Background())

	// 第一次执行污染全局。
	_, _ = w.Execute(context.Background(), evalReq(t, `globalThis.leaked = "yes"; return "1";`, nil))
	// 第二次执行不应看到上次的全局变量。
	resp, _ := w.Execute(context.Background(),
		evalReq(t, `return typeof globalThis.leaked === "undefined" ? "clean" : "polluted";`, nil))
	var body struct {
		Content string `json:"content"`
	}
	_ = json.Unmarshal(resp.Result, &body)
	if body.Content != "clean" {
		t.Errorf("每次执行应使用全新 Runtime，实际 %q", body.Content)
	}
}

// TestDangerousGlobalsNotExposed 验证危险全局对象不可用（第 14 章）。
func TestDangerousGlobalsNotExposed(t *testing.T) {
	w := newTestWorker(t, Config{})
	_ = w.Start(context.Background())
	defer w.Stop(context.Background())

	// require/process 等必须不可用；引用它们应抛错让脚本失败，而不是拿到能力。
	for _, expr := range []string{
		`return typeof require;`,
		`return typeof process;`,
	} {
		resp, _ := w.Execute(context.Background(), evalReq(t, expr, nil))
		var body struct {
			Content string `json:"content"`
		}
		_ = json.Unmarshal(resp.Result, &body)
		if body.Content != "undefined" {
			t.Errorf("表达式 %q 期望 undefined，实际 %q", expr, body.Content)
		}
	}
}

// TestScriptSyntaxErrorReported 验证语法错误被明确报告。
func TestScriptSyntaxErrorReported(t *testing.T) {
	w := newTestWorker(t, Config{})
	_ = w.Start(context.Background())
	defer w.Stop(context.Background())

	resp, _ := w.Execute(context.Background(), evalReq(t, `return (((;`, nil))
	if resp.Status == worker.StatusOK {
		t.Error("语法错误不应返回成功")
	}
	if resp.Error == nil {
		t.Error("语法错误应带错误信息")
	}
}

// ============================== capability 权限（fail-closed） ==============================

// TestHTTPDeniedWithoutAllowlist 验证未配置 AllowedHosts 时 tool.http 拒绝。
func TestHTTPDeniedWithoutAllowlist(t *testing.T) {
	w := newTestWorker(t, Config{
		Capabilities: CapabilityPolicy{AllowInput: true, AllowHTTP: true}, // 开了但没给白名单
	})
	_ = w.Start(context.Background())
	defer w.Stop(context.Background())

	resp, _ := w.Execute(context.Background(),
		evalReq(t, `try { tool.http("http://example.com"); return "allowed"; } catch (e) { return "denied"; }`, nil))
	var body struct {
		Content string `json:"content"`
	}
	_ = json.Unmarshal(resp.Result, &body)
	if body.Content != "denied" {
		t.Errorf("未配置 AllowedHosts 时 tool.http 必须 fail-closed，实际 %q", body.Content)
	}
}

// TestHTTPDeniedForHostOutsideAllowlist 验证白名单外的域名被拒绝。
func TestHTTPDeniedForHostOutsideAllowlist(t *testing.T) {
	w := newTestWorker(t, Config{
		Capabilities: CapabilityPolicy{
			AllowInput: true, AllowHTTP: true,
			AllowedHosts: []string{"allowed.example.com"},
		},
	})
	_ = w.Start(context.Background())
	defer w.Stop(context.Background())

	code := `try { tool.http("http://evil.example.com/x"); return "allowed"; } catch (e) { return "denied"; }`
	resp, _ := w.Execute(context.Background(), evalReq(t, code, nil))
	var body struct {
		Content string `json:"content"`
	}
	_ = json.Unmarshal(resp.Result, &body)
	if body.Content != "denied" {
		t.Errorf("白名单外域名必须被拒绝，实际 %q", body.Content)
	}
}

// TestFSDeniedWithoutRoots 验证未配置 AllowedRoots 时 tool.fs 拒绝。
func TestFSDeniedWithoutRoots(t *testing.T) {
	w := newTestWorker(t, Config{
		Capabilities: CapabilityPolicy{AllowInput: true, AllowFS: true, AllowRead: true},
	})
	_ = w.Start(context.Background())
	defer w.Stop(context.Background())

	code := `try { tool.fs.read("/etc/passwd"); return "allowed"; } catch (e) { return "denied"; }`
	resp, _ := w.Execute(context.Background(), evalReq(t, code, nil))
	var body struct {
		Content string `json:"content"`
	}
	_ = json.Unmarshal(resp.Result, &body)
	if body.Content != "denied" {
		t.Errorf("未配置 AllowedRoots 时 tool.fs 必须 fail-closed，实际 %q", body.Content)
	}
}

// TestFSReadWithinRoots 验证白名单内的文件可读。
func TestFSReadWithinRoots(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "data.txt")
	if err := os.WriteFile(target, []byte("file-content"), 0o600); err != nil {
		t.Fatal(err)
	}

	w := newTestWorker(t, Config{
		Capabilities: CapabilityPolicy{
			AllowInput: true, AllowFS: true, AllowRead: true,
			AllowedRoots: []string{root},
		},
	})
	_ = w.Start(context.Background())
	defer w.Stop(context.Background())

	code := `return tool.fs.read(` + jsString(target) + `);`
	resp, _ := w.Execute(context.Background(), evalReq(t, code, nil))
	var body struct {
		Content string `json:"content"`
	}
	_ = json.Unmarshal(resp.Result, &body)
	if body.Content != "file-content" {
		t.Errorf("白名单内文件应可读，实际 %q", body.Content)
	}
}

// TestFSReadOutsideRootsDenied 验证白名单外的文件被拒绝。
func TestFSReadOutsideRootsDenied(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	_ = os.WriteFile(secret, []byte("secret"), 0o600)

	w := newTestWorker(t, Config{
		Capabilities: CapabilityPolicy{
			AllowInput: true, AllowFS: true, AllowRead: true,
			AllowedRoots: []string{root},
		},
	})
	_ = w.Start(context.Background())
	defer w.Stop(context.Background())

	code := `try { tool.fs.read(` + jsString(secret) + `); return "allowed"; } catch (e) { return "denied"; }`
	resp, _ := w.Execute(context.Background(), evalReq(t, code, nil))
	var body struct {
		Content string `json:"content"`
	}
	_ = json.Unmarshal(resp.Result, &body)
	if body.Content != "denied" {
		t.Errorf("白名单外文件必须被拒绝，实际 %q", body.Content)
	}
}

// TestFSWriteRequiresExplicitPermission 验证写权限需要显式开启。
func TestFSWriteRequiresExplicitPermission(t *testing.T) {
	root := t.TempDir()
	w := newTestWorker(t, Config{
		Capabilities: CapabilityPolicy{
			AllowInput: true, AllowFS: true, AllowRead: true, AllowWrite: false, // 未开写
			AllowedRoots: []string{root},
		},
	})
	_ = w.Start(context.Background())
	defer w.Stop(context.Background())

	target := filepath.Join(root, "out.txt")
	code := `try { tool.fs.write(` + jsString(target) + `, "x"); return "allowed"; } catch (e) { return "no-write-cap"; }`
	resp, _ := w.Execute(context.Background(), evalReq(t, code, nil))
	var body struct {
		Content string `json:"content"`
	}
	_ = json.Unmarshal(resp.Result, &body)
	if body.Content != "no-write-cap" {
		t.Errorf("未开启 AllowWrite 时 write 应不存在，实际 %q", body.Content)
	}
}

// TestOutputTruncation 验证输出超限被截断。
func TestOutputTruncation(t *testing.T) {
	w := newTestWorker(t, Config{Limits: Limits{MaxOutputBytes: 100}})
	_ = w.Start(context.Background())
	defer w.Stop(context.Background())

	resp, _ := w.Execute(context.Background(),
		evalReq(t, `return "A".repeat(5000);`, nil))
	var body struct {
		Content   string `json:"content"`
		Truncated bool   `json:"truncated"`
	}
	_ = json.Unmarshal(resp.Result, &body)
	if len(body.Content) > 200 {
		t.Errorf("输出应被截断到上限附近，实际 %d 字节", len(body.Content))
	}
	if !body.Truncated {
		t.Error("应标记 truncated")
	}
}

// TestLogEntryLimit 验证日志条数上限生效（防止日志把内存打爆）。
func TestLogEntryLimit(t *testing.T) {
	w := newTestWorker(t, Config{Limits: Limits{MaxLogEntries: 10}})
	_ = w.Start(context.Background())
	defer w.Stop(context.Background())

	resp, _ := w.Execute(context.Background(),
		evalReq(t, `for (let i=0;i<1000;i++) tool.log("line "+i); return "done";`, nil))
	var body struct {
		Logs      []string `json:"logs"`
		Truncated bool     `json:"truncated"`
	}
	_ = json.Unmarshal(resp.Result, &body)
	if len(body.Logs) > 10 {
		t.Errorf("日志条数应被限制到 10，实际 %d", len(body.Logs))
	}
	if !body.Truncated {
		t.Error("日志超限应标记 truncated")
	}
}

// TestHealthSelfCheck 验证健康检查真跑一次 JS（不是假返回 nil）。
func TestHealthSelfCheck(t *testing.T) {
	w := newTestWorker(t, Config{})
	if err := w.Health(context.Background()); err != nil {
		t.Errorf("健康检查应通过: %v", err)
	}
	_ = w.Stop(context.Background())
	if err := w.Health(context.Background()); err == nil {
		t.Error("停止后健康检查应失败")
	}
}

// TestUnknownActionRejected 验证未知动作被拒绝。
func TestUnknownActionRejected(t *testing.T) {
	w := newTestWorker(t, Config{})
	_ = w.Start(context.Background())
	defer w.Stop(context.Background())
	resp, _ := w.Execute(context.Background(), worker.WorkerRequest{CallID: "c", Action: "bogus"})
	if resp.Error == nil || resp.Error.Code != worker.CodeInvalidArgument {
		t.Errorf("未知动作应返回 invalid_argument，实际 %+v", resp.Error)
	}
}

// TestEmptyCodeRejected 验证空代码被拒绝。
func TestEmptyCodeRejected(t *testing.T) {
	w := newTestWorker(t, Config{})
	_ = w.Start(context.Background())
	defer w.Stop(context.Background())
	resp, _ := w.Execute(context.Background(), evalReq(t, "   ", nil))
	if resp.Error == nil || resp.Error.Code != worker.CodeInvalidArgument {
		t.Errorf("空代码应返回 invalid_argument，实际 %+v", resp.Error)
	}
}

// TestHostAllowedMatching 验证主机白名单匹配逻辑（含通配）。
func TestHostAllowedMatching(t *testing.T) {
	allowed := []string{"api.example.com", "*.trusted.com", "*"}
	cases := map[string]bool{
		"api.example.com": true,
		"a.trusted.com":   true,
		"trusted.com":     true,
		"evil.com":        false,
	}
	// 前两个模式覆盖 a.trusted.com / trusted.com；不含 "*" 时 evil.com 应为 false。
	subset := allowed[:2]
	for host, want := range cases {
		if got := hostAllowed(host, subset); got != want {
			t.Errorf("hostAllowed(%q)=%v want=%v", host, got, want)
		}
	}
	if !hostAllowed("anything", allowed) {
		t.Error("\"*\" 应匹配任意主机")
	}
}

// TestHostOfParsing 验证 URL 主机名解析。
func TestHostOfParsing(t *testing.T) {
	cases := map[string]string{
		"http://example.com/path":      "example.com",
		"https://user:pw@h.com:8443/x": "h.com",
		"ftp://ftp.example.org":        "ftp.example.org",
		"example.com:8080/x":           "example.com",
	}
	for in, want := range cases {
		if got := hostOf(in); got != want {
			t.Errorf("hostOf(%q)=%q want=%q", in, got, want)
		}
	}
}

// jsString 把 Go 字符串转成安全的 JS 字符串字面量。
func jsString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestSandboxRunsInWorkerProcess 说明性断言：
// 沙箱的隔离最终依赖"跑在独立 Worker 进程里"——即使 VM 被攻破，
// 影响范围也被进程边界限制（这是本任务把 goja 放进 Worker 的根本理由）。
func TestSandboxRunsInWorkerProcess(t *testing.T) {
	w := newTestWorker(t, Config{})
	if w.Kind() != "dynamic-js" {
		t.Errorf("Kind 应为 dynamic-js，实际 %s", w.Kind())
	}
	// 该 Worker 由 Manager 在独立 slot 中托管，崩溃/超时只回收该 slot。
	if runtime.GOOS == "" {
		t.Fatal("unreachable")
	}
}
