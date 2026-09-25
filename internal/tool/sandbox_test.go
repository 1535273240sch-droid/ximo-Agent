package tool

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testPolicy() SandboxPolicy {
	policy := DefaultSandboxPolicy()
	policy.Timeout = 3 * time.Second
	policy.MaxInstructions = 100000
	policy.MaxMemoryBytes = 32 << 20
	return policy
}

func runSandbox(t *testing.T, policy SandboxPolicy, code string, input map[string]any) (SandboxResult, error) {
	t.Helper()
	sandbox := NewSandbox(policy)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return sandbox.Run(ctx, code, input)
}

func TestSandboxBasicExecution(t *testing.T) {
	result, err := runSandbox(t, testPolicy(), `return "hello " + args.name;`, map[string]any{"name": "ximo"})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if result.Content != "hello ximo" {
		t.Fatalf("内容 = %q，期望 %q", result.Content, "hello ximo")
	}
	if !result.Success {
		t.Fatalf("Success = false")
	}
}

func TestSandboxAsyncAndAwait(t *testing.T) {
	// 验证异步函数 + await 的微任务队列被完整驱动（v1 的超时缺陷场景）。
	code := `
		let out = "";
		out += await Promise.resolve("a");
		out += await Promise.resolve("b");
		return out;
	`
	result, err := runSandbox(t, testPolicy(), code, nil)
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if result.Content != "ab" {
		t.Fatalf("内容 = %q，期望 %q", result.Content, "ab")
	}
}

func TestSandboxContentSuccessObject(t *testing.T) {
	// v1 契约：返回 {content, success} 对象
	result, err := runSandbox(t, testPolicy(), `return { content: "done", success: true };`, nil)
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if result.Content != "done" || !result.Success {
		t.Fatalf("结果 = %+v", result)
	}
}

func TestSandboxToolOutputAndLog(t *testing.T) {
	code := `
		tool.log("first");
		tool.log("second");
		tool.output("explicit");
		return "ignored";
	`
	result, err := runSandbox(t, testPolicy(), code, nil)
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if result.Content != "explicit" {
		t.Fatalf("内容 = %q，期望 tool.output 的值", result.Content)
	}
	if len(result.Logs) != 2 {
		t.Fatalf("日志 = %v，期望 2 条", result.Logs)
	}
}

func TestSandboxLogBounded(t *testing.T) {
	policy := testPolicy()
	policy.MaxLogEntries = 3
	code := `
		for (let i = 0; i < 100; i++) { tool.log("log-" + i); }
		return "ok";
	`
	result, err := runSandbox(t, policy, code, nil)
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if len(result.Logs) != 3 {
		t.Fatalf("日志 = %d 条，期望被限制为 3", len(result.Logs))
	}
}

func TestSandboxRejectsBannedAPIs(t *testing.T) {
	banned := []string{
		`const os = require("os"); return 1;`,
		`return process.version;`,
		`return eval("1+1");`,
		`const f = new Function("return 1"); return f();`,
		`return globalThis;`,
		`return fetch("https://example.com");`,
		`const cp = require("child_process"); return 1;`,
		`return typeof os === "undefined" ? 1 : os.platform();`,
	}
	for _, code := range banned {
		t.Run(code, func(t *testing.T) {
			_, err := runSandbox(t, testPolicy(), code, nil)
			if err == nil {
				t.Fatalf("代码 %q 应当被拒绝", code)
			}
			var violation *SandboxViolationError
			if !errors.As(err, &violation) {
				t.Fatalf("错误类型 = %T，期望 *SandboxViolationError", err)
			}
			if violation.Kind != ViolationBannedAPI {
				t.Fatalf("违规类型 = %q，期望 %q", violation.Kind, ViolationBannedAPI)
			}
		})
	}
}

func TestSandboxRejectsUnboundedLoops(t *testing.T) {
	unbounded := []string{
		`while (true) { }`,
		`for (;;) { }`,
		`while (1) { break; }`,
		`for (let i = 0; ; i++) { break; }`,
	}
	for _, code := range unbounded {
		t.Run(code, func(t *testing.T) {
			_, err := runSandbox(t, testPolicy(), code, nil)
			if err == nil {
				t.Fatalf("代码 %q 应当被拒绝", code)
			}
			var violation *SandboxViolationError
			if !errors.As(err, &violation) || violation.Kind != ViolationUnboundedLoop {
				t.Fatalf("错误 = %v，期望 unbounded_loop 违规", err)
			}
		})
	}
}

func TestSandboxNoOSAccessByDefault(t *testing.T) {
	// goja 不提供这些全局；沙箱也不注入。
	// typeof <name> 是合法无害的存在性检查（扫描器放行）。
	code := `return [typeof fetch, typeof process, typeof require, typeof os, typeof net, typeof fs, typeof XMLHttpRequest, typeof WebSocket].join(",");`
	result, err := runSandbox(t, testPolicy(), code, nil)
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	want := "undefined,undefined,undefined,undefined,undefined,undefined,undefined,undefined"
	if result.Content != want {
		t.Fatalf("内容 = %q，期望 %q", result.Content, want)
	}
}

func TestSandboxLoopBudgetEnforced(t *testing.T) {
	policy := testPolicy()
	policy.MaxInstructions = 500
	code := `
		let n = 0;
		for (let i = 0; i < 100000; i++) { n++; }
		return n;
	`
	_, err := runSandbox(t, policy, code, nil)
	if err == nil {
		t.Fatalf("超出循环预算应当被中断")
	}
	var violation *SandboxViolationError
	if !errors.As(err, &violation) || violation.Kind != ViolationInstructionBudget {
		t.Fatalf("错误 = %v，期望 instruction_budget 违规", err)
	}
}

func TestSandboxNestedLoopBudget(t *testing.T) {
	policy := testPolicy()
	policy.MaxInstructions = 1000
	code := `
		let n = 0;
		for (let i = 0; i < 100; i++) {
			for (let j = 0; j < 100; j++) { n++; }
		}
		return n;
	`
	_, err := runSandbox(t, policy, code, nil)
	if err == nil {
		t.Fatalf("嵌套循环超出预算应当被中断")
	}
	var violation *SandboxViolationError
	if !errors.As(err, &violation) || violation.Kind != ViolationInstructionBudget {
		t.Fatalf("错误 = %v，期望 instruction_budget 违规", err)
	}
}

func TestSandboxTimeoutEnforced(t *testing.T) {
	policy := testPolicy()
	policy.Timeout = 200 * time.Millisecond
	policy.MaxInstructions = 1 << 40 // 预算放大，确保先触发超时而非预算
	// 条件循环无法静态判定无界，但看门狗超时会硬中断（v1 的 Promise.race 不停执行）。
	code := `
		let n = 0;
		let done = false;
		while (!done) { n++; if (n < 0) done = true; }
		return n;
	`
	start := time.Now()
	_, err := runSandbox(t, policy, code, nil)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("超时应当中断执行")
	}
	var violation *SandboxViolationError
	if !errors.As(err, &violation) || violation.Kind != ViolationTimeout {
		t.Fatalf("错误 = %v，期望 timeout 违规", err)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("中断耗时 %v，说明执行没有被真正停止", elapsed)
	}
}

func TestSandboxMemoryLimitEnforced(t *testing.T) {
	policy := testPolicy()
	policy.MaxMemoryBytes = 4 << 20 // 4MB
	policy.Timeout = 10 * time.Second
	code := `
		let chunks = [];
		for (let i = 0; i < 2000; i++) { chunks.push("x".repeat(1024 * 1024)); }
		return chunks.length;
	`
	_, err := runSandbox(t, policy, code, nil)
	if err == nil {
		t.Fatalf("超出内存预算应当被中断")
	}
	var violation *SandboxViolationError
	if !errors.As(err, &violation) || violation.Kind != ViolationMemoryBudget {
		t.Fatalf("错误 = %v，期望 memory_budget 违规", err)
	}
}

func TestSandboxHTTPAllowlist(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Write([]byte("pong"))
	}))
	defer server.Close()

	host := strings.TrimPrefix(server.URL, "http://")
	policy := testPolicy()
	policy.AllowedHTTPHosts = []string{host}
	sandbox := NewSandbox(policy)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	code := `
		const resp = await tool.http({ method: "GET", url: args.url + "/ping" });
		return resp.status + ":" + resp.body;
	`
	result, err := sandbox.Run(ctx, code, map[string]any{"url": server.URL})
	if err != nil {
		t.Fatalf("白名单内请求失败: %v", err)
	}
	if result.Content != "200:pong" {
		t.Fatalf("内容 = %q", result.Content)
	}
	if gotPath != "/ping" {
		t.Fatalf("请求路径 = %q", gotPath)
	}

	// 非白名单主机必须被拒绝
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("should-not-happen"))
	}))
	defer other.Close()
	code2 := `await tool.http({ method: "GET", url: args.url }); return "leaked";`
	_, err = sandbox.Run(ctx, code2, map[string]any{"url": other.URL})
	if err == nil {
		t.Fatalf("非白名单主机请求应当失败")
	}
}

func TestSandboxHTTPMethodRestricted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	policy := testPolicy()
	policy.AllowedHTTPHosts = []string{host}
	policy.AllowedHTTPMethods = []string{"GET"} // 默认仅 GET
	sandbox := NewSandbox(policy)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := sandbox.Run(ctx, `await tool.http({ method: "POST", url: args.url }); return "x";`, map[string]any{"url": server.URL})
	if err == nil {
		t.Fatalf("POST 应当被方法白名单拒绝")
	}
}

func TestSandboxFSAllowlist(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "data.txt"), []byte("content-123"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}

	policy := testPolicy()
	policy.AllowedFSRoots = []string{root}
	sandbox := NewSandbox(policy)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// 白名单内读取
	result, err := sandbox.Run(ctx, `
		const r = await tool.fs({ op: "read", path: args.path });
		return r.content;
	`, map[string]any{"path": filepath.Join(root, "data.txt")})
	if err != nil {
		t.Fatalf("白名单内读取失败: %v", err)
	}
	if result.Content != "content-123" {
		t.Fatalf("内容 = %q", result.Content)
	}

	// 白名单外读取
	_, err = sandbox.Run(ctx, `
		const r = await tool.fs({ op: "read", path: args.path });
		return r.content;
	`, map[string]any{"path": filepath.Join(outside, "secret.txt")})
	if err == nil {
		t.Fatalf("白名单外路径应当被拒绝")
	}

	// 路径穿越
	_, err = sandbox.Run(ctx, `
		const r = await tool.fs({ op: "read", path: args.path });
		return r.content;
	`, map[string]any{"path": filepath.Join(root, "..", "..", "etc", "passwd")})
	if err == nil {
		t.Fatalf("路径穿越应当被拒绝")
	}

	// 默认只读：写入被拒绝
	_, err = sandbox.Run(ctx, `
		await tool.fs({ op: "write", path: args.path, content: "x" });
		return "written";
	`, map[string]any{"path": filepath.Join(root, "new.txt")})
	if err == nil {
		t.Fatalf("默认策略下写入应当被拒绝")
	}
}

func TestSandboxScriptTooLarge(t *testing.T) {
	policy := testPolicy()
	policy.MaxScriptBytes = 100
	_, err := runSandbox(t, policy, `return "`+strings.Repeat("a", 500)+`";`, nil)
	if err == nil {
		t.Fatalf("超大脚本应当被拒绝")
	}
	var violation *SandboxViolationError
	if !errors.As(err, &violation) || violation.Kind != ViolationScriptTooLarge {
		t.Fatalf("错误 = %v，期望 script_too_large", err)
	}
}

func TestSandboxReservedIdentifierRejected(t *testing.T) {
	_, err := runSandbox(t, testPolicy(), `const __sbTick = () => {}; return 1;`, nil)
	if err == nil {
		t.Fatalf("引用沙箱保留标识符应当被拒绝")
	}
	var violation *SandboxViolationError
	if !errors.As(err, &violation) || violation.Kind != ViolationReservedName {
		t.Fatalf("错误 = %v，期望 reserved_name", err)
	}
}

func TestSandboxOutputTooLarge(t *testing.T) {
	policy := testPolicy()
	policy.MaxOutputBytes = 64
	code := `return "x".repeat(1000);`
	_, err := runSandbox(t, policy, code, nil)
	if err == nil {
		t.Fatalf("超大输出应当被拒绝")
	}
	var violation *SandboxViolationError
	if !errors.As(err, &violation) || violation.Kind != ViolationOutputTooLarge {
		t.Fatalf("错误 = %v，期望 output_too_large", err)
	}
}

func TestSandboxParseURL(t *testing.T) {
	code := `const u = tool.parseURL("https://example.com:8443/a/b?x=1"); return u.protocol + "|" + u.host + "|" + u.port + "|" + u.path + "|" + u.query;`
	result, err := runSandbox(t, testPolicy(), code, nil)
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	want := "https|example.com|8443|/a/b|x=1"
	if result.Content != want {
		t.Fatalf("内容 = %q，期望 %q", result.Content, want)
	}
}

func TestSandboxJSExceptionIsolated(t *testing.T) {
	_, err := runSandbox(t, testPolicy(), `throw new Error("boom");`, nil)
	if err != nil {
		t.Fatalf("JS 异常应当转换为失败结果而不是 Go 错误: %v", err)
	}
	result, _ := runSandbox(t, testPolicy(), `throw new Error("boom");`, nil)
	if result.Success || !strings.Contains(result.Error, "boom") {
		t.Fatalf("结果 = %+v，期望 Success=false 且错误包含 boom", result)
	}
}

func TestSandboxContextCanceled(t *testing.T) {
	policy := testPolicy()
	policy.Timeout = 30 * time.Second
	policy.MaxInstructions = 1 << 40
	sandbox := NewSandbox(policy)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	code := `let n = 0; let done = false; while (!done) { n++; if (n < 0) done = true; } return n;`
	_, err := sandbox.Run(ctx, code, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("错误 = %v，期望 context.Canceled", err)
	}
}
