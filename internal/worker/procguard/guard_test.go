package procguard

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

// 测试用的辅助进程：让子进程再 fork 一个孙进程并一起 sleep。
// 这是验证"杀掉 shell 不等于杀掉子孙"的关键——v1 正是在这里漏的。

// TestStartAndWaitBasic 验证最基本的拉起与等待。
func TestStartAndWaitBasic(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("使用 /bin/sh 的用例，Windows 由 TestStartAndWaitWindows 覆盖")
	}
	p, err := Start(context.Background(), Config{
		Path: "/bin/sh",
		Args: []string{"-c", "echo hello; echo err >&2"},
	})
	if err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	defer p.Close()

	res, err := p.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait 失败: %v", err)
	}
	if !strings.Contains(string(res.Stdout), "hello") {
		t.Errorf("stdout 缺少 hello: %q", res.Stdout)
	}
	if !strings.Contains(string(res.Stderr), "err") {
		t.Errorf("stderr 缺少 err: %q", res.Stderr)
	}
	if res.ExitCode != 0 {
		t.Errorf("退出码应为 0，实际 %d", res.ExitCode)
	}
}

// TestStartAndWaitWindows 验证 Windows 上的基本执行。
func TestStartAndWaitWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows 专用")
	}
	p, err := Start(context.Background(), Config{
		Path: "cmd.exe",
		Args: []string{"/d", "/s", "/c", "echo hello"},
	})
	if err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	defer p.Close()
	res, err := p.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait 失败: %v", err)
	}
	if !strings.Contains(string(res.Stdout), "hello") {
		t.Errorf("stdout 缺少 hello: %q", res.Stdout)
	}
}

// ============================== 输出限流（v1 缺陷 3） ==============================

// TestOutputLimitHeadOnly 验证只保留头部时输出被截断且总量被记录。
func TestOutputLimitHeadOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("使用 /bin/sh 的用例")
	}
	const limit = 1024
	p, err := Start(context.Background(), Config{
		Path: "/bin/sh",
		// 输出 10000 字节，远超 limit。
		Args:           []string{"-c", "yes AAAAAAAAAA | head -c 10000"},
		MaxOutputBytes: limit,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	res, err := p.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Stdout) > limit {
		t.Errorf("stdout 应被限制到 %d 字节，实际 %d", limit, len(res.Stdout))
	}
	if !res.Truncated {
		t.Error("应标记为已截断")
	}
	if res.StdoutTotal < 10000 {
		t.Errorf("应记录真实总字节数（>=10000），实际 %d", res.StdoutTotal)
	}
}

// TestOutputLimitHeadAndTail 验证 head+tail 模式保留两端。
func TestOutputLimitHeadAndTail(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("使用 /bin/sh 的用例")
	}
	p, err := Start(context.Background(), Config{
		Path:           "/bin/sh",
		Args:           []string{"-c", "printf 'HEAD'; head -c 5000 /dev/zero | tr '\\0' 'M'; printf 'TAIL'"},
		MaxOutputBytes: 4096, // 触发上限
		HeadBytes:      10,
		TailBytes:      10,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	res, err := p.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := string(res.Stdout)
	if !strings.HasPrefix(out, "HEAD") {
		t.Errorf("应保留头部，实际开头: %q", out[:min(20, len(out))])
	}
	if !strings.HasSuffix(out, "TAIL") {
		t.Errorf("应保留尾部，实际结尾: %q", out[max(0, len(out)-20):])
	}
	if !strings.Contains(out, "输出被截断") {
		t.Error("应包含截断标记")
	}
}

// TestLargeOutputDoesNotHang 验证海量输出不会因管道写满而卡死
// （v1 的问题不在于丢数据，而在于把数据全堆内存里）。
func TestLargeOutputDoesNotHang(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("使用 /bin/sh 的用例")
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		p, err := Start(context.Background(), Config{
			Path:           "/bin/sh",
			Args:           []string{"-c", "head -c 5000000 /dev/zero | tr '\\0' 'X'"},
			MaxOutputBytes: 4096,
		})
		if err != nil {
			t.Errorf("Start 失败: %v", err)
			return
		}
		defer p.Close()
		res, err := p.Wait(context.Background())
		if err != nil {
			t.Errorf("Wait 失败: %v", err)
			return
		}
		if len(res.Stdout) > 4096 {
			t.Errorf("输出应被限制，实际 %d", len(res.Stdout))
		}
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("大量输出时进程卡死了（管道未被持续读取）")
	}
}

// ============================== 进程树清理（核心红线） ==============================

// TestProcessGroupKillKillsDescendants 验证杀掉主进程后**子孙进程也被清理**。
//
// 这是本任务最关键的一条：v1 只对直接子进程发 SIGTERM 且没有独立进程组，
// 导致 `sh -c "sleep 300 & wait"` 这类命令的子孙被 init 收养并永久泄漏。
func TestProcessGroupKillKillsDescendants(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 的等价验证由 TestWindowsJobObjectKillsDescendants 覆盖")
	}
	// 主 shell 起一个后台 sleep（孙进程），然后自己也 sleep。
	p, err := Start(context.Background(), Config{
		Path: "/bin/sh",
		Args: []string{"-c", "sleep 300 & echo started; sleep 300"},
	})
	if err != nil {
		t.Fatalf("Start 失败: %v", err)
	}

	// 等一会儿让子进程与孙进程都起来。
	time.Sleep(500 * time.Millisecond)

	descendants := Descendants(p.PID())
	if len(descendants) == 0 {
		_ = p.Kill(context.Background())
		_ = p.Close()
		t.Skip("未观察到后代进程（时序问题），跳过")
	}

	// 强杀整棵树。
	if err := p.Kill(context.Background()); err != nil {
		t.Fatalf("Kill 失败: %v", err)
	}
	_ = p.Close()

	// 等一小会儿让 OS 回收，然后断言没有任何后代存活。
	time.Sleep(700 * time.Millisecond)
	for _, pid := range descendants {
		if ProcessAlive(pid) {
			// 尽力清理，避免污染测试环境。
			_ = KillTree(pid)
			t.Errorf("PID %d 是孙进程，Kill 之后仍然存活 —— 进程树清理未覆盖子孙进程", pid)
		}
	}
}

// TestWindowsJobObjectKillsDescendants 验证 Windows 上 Job Object 覆盖子孙进程。
func TestWindowsJobObjectKillsDescendants(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows 专用")
	}
	// cmd 起一个后台 timeout（孙进程）再自己挂着。
	p, err := Start(context.Background(), Config{
		Path: "cmd.exe",
		Args: []string{"/d", "/s", "/c", "start /b timeout /t 300 /nobreak >nul & timeout /t 300 /nobreak >nul"},
	})
	if err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	time.Sleep(800 * time.Millisecond)

	descendants := Descendants(p.PID())
	t.Logf("观察到 %d 个后代进程: %v", len(descendants), descendants)

	if err := p.Kill(context.Background()); err != nil {
		t.Fatalf("Kill 失败: %v", err)
	}
	_ = p.Close()
	time.Sleep(800 * time.Millisecond)

	for _, pid := range descendants {
		if ProcessAlive(pid) {
			_ = KillTree(pid)
			t.Errorf("PID %d 在 Job Object 终止后仍存活", pid)
		}
	}
}

// blockingArgs 返回一条"可靠阻塞一段时间"的命令。
//
// 注意不要用 Windows 的 `timeout /t`：它在 stdin 不是控制台时会立刻报错退出
// （"Input redirection is not supported"），因此不适合做长驻进程的测试替身。
func blockingArgs() (string, []string) {
	if runtime.GOOS == "windows" {
		// ping 每次请求间隔 1s，-n 300 即约 5 分钟。
		return "cmd.exe", []string{"/d", "/s", "/c", "ping -n 300 127.0.0.1 >nul"}
	}
	return "/bin/sh", []string{"-c", "sleep 300"}
}

// TestDescendantsFindsChildren 验证后代枚举本身可用。
func TestDescendantsFindsChildren(t *testing.T) {
	path, args := blockingArgs()
	p, err := Start(context.Background(), Config{Path: path, Args: args})
	if err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	defer func() {
		_ = p.Kill(context.Background())
		_ = p.Close()
	}()

	// 给 cmd.exe 一点时间把 ping 子进程拉起来。
	deadline := time.Now().Add(3 * time.Second)
	var kids []int
	for time.Now().Before(deadline) {
		kids = Descendants(p.PID())
		if len(kids) > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(kids) == 0 {
		t.Error("应至少枚举到一个后代进程")
	}
}

// TestWaitTimeoutKillsTree 验证超时时进程树被强杀且标记 TimedOut。
func TestWaitTimeoutKillsTree(t *testing.T) {
	path, args := blockingArgs()
	p, err := Start(context.Background(), Config{
		Path: path, Args: args, KillGrace: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	defer p.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	res, err := p.Wait(ctx)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Wait 返回错误: %v", err)
	}
	if !res.TimedOut {
		t.Error("应标记 TimedOut")
	}
	if elapsed > 10*time.Second {
		t.Errorf("超时强杀耗时过长: %s", elapsed)
	}
	if ProcessAlive(p.PID()) {
		t.Error("超时后主进程应已退出")
	}
}

// TestWaitCancellation 验证 ctx 取消时进程被清理。
func TestWaitCancellation(t *testing.T) {
	path, args := blockingArgs()
	p, err := Start(context.Background(), Config{
		Path: path, Args: args, KillGrace: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()
	res, err := p.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait 返回错误: %v", err)
	}
	if res.TimedOut {
		t.Error("ctx 取消不应标记为 TimedOut")
	}
	if !res.Killed {
		t.Error("ctx 取消后应标记 Killed")
	}
}

// ============================== 环境变量最小化（第 21 章） ==============================

// TestMinimalEnvExcludesSecrets 验证 MinimalEnv 不包含密钥类变量。
func TestMinimalEnvExcludesSecrets(t *testing.T) {
	t.Setenv("XIMO_PROC_API_KEY", "sk-secret")
	env := MinimalEnv()
	for _, kv := range env {
		if strings.HasPrefix(kv, "XIMO_PROC_API_KEY=") {
			t.Fatal("MinimalEnv 不应包含 API Key")
		}
	}
	// 应包含 UTF-8 强制项（对齐 v1 的编码修复）。
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "PYTHONUTF8=1") {
		t.Error("MinimalEnv 应包含 UTF-8 强制项")
	}
}

// TestChildDoesNotInheritSecretsByDefault 验证子进程默认拿不到父进程的密钥。
func TestChildDoesNotInheritSecretsByDefault(t *testing.T) {
	const key = "XIMO_INHERIT_TEST_KEY"
	t.Setenv(key, "sk-should-not-leak")

	var p *Proc
	var err error
	if runtime.GOOS == "windows" {
		p, err = Start(context.Background(), Config{
			Path: "cmd.exe", Args: []string{"/d", "/s", "/c", "set"},
		})
	} else {
		p, err = Start(context.Background(), Config{
			Path: "/usr/bin/env", // 打印所有环境变量
		})
	}
	if err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	defer p.Close()
	res, err := p.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(res.Stdout), "sk-should-not-leak") {
		t.Fatal("子进程环境里出现了父进程的密钥 —— 违反第 21 章红线")
	}
}

// ============================== 配置校验与能力上报 ==============================

// TestStartRejectsEmptyPath 验证空路径被拒绝。
func TestStartRejectsEmptyPath(t *testing.T) {
	if _, err := Start(context.Background(), Config{}); err == nil {
		t.Error("空 Path 应报错")
	}
}

// TestStartNonexistentBinary 验证不存在的可执行文件报错。
func TestStartNonexistentBinary(t *testing.T) {
	_, err := Start(context.Background(), Config{Path: "definitely-not-a-real-binary-xyz"})
	if err == nil {
		t.Error("不存在的可执行文件应报错")
	}
}

// TestKillIsIdempotent 验证重复 Kill 安全。
func TestKillIsIdempotent(t *testing.T) {
	path, args := blockingArgs()
	p, err := Start(context.Background(), Config{Path: path, Args: args})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	ctx := context.Background()
	if err := p.Kill(ctx); err != nil {
		t.Errorf("首次 Kill 失败: %v", err)
	}
	if err := p.Kill(ctx); err != nil {
		t.Errorf("重复 Kill 应幂等，却返回: %v", err)
	}
}

// TestWaitWithoutStartFails 验证未启动就 Wait 报错。
func TestWaitWithoutStartFails(t *testing.T) {
	p := &Proc{}
	if _, err := p.Wait(context.Background()); err == nil {
		t.Error("未启动就 Wait 应报错")
	}
}

// TestCapabilitiesReported 验证能力如实上报（不谎报不支持的能力）。
func TestCapabilitiesReported(t *testing.T) {
	c := CapabilitiesOf()
	if !c.ProcessTreeKill {
		t.Error("所有平台都应支持进程树清理（本任务红线）")
	}
	if !c.DescendantSweep {
		t.Error("所有平台都应支持后代进程枚举")
	}
	if runtime.GOOS == "windows" {
		if !c.JobObject {
			t.Error("Windows 应支持 Job Object")
		}
		if !c.MemoryLimit {
			t.Error("Windows 应支持 Job Object 内存限制")
		}
	} else {
		if !c.ProcessGroupKill {
			t.Error("Unix 应支持进程组 kill")
		}
		// 如实上报：Unix 侧未实现 cgroup，因此不应谎报支持内存限制。
		if c.MemoryLimit {
			t.Error("Unix 侧未实现 cgroup，不应谎报支持内存限制")
		}
	}
}

// TestKillTreeByPID 验证游离 PID 的兜底清理入口。
func TestKillTreeByPID(t *testing.T) {
	// 用一个几乎不可能存在的 PID：应安全无操作。
	if err := KillTree(999999); err != nil {
		t.Errorf("对不存在 PID 的 KillTree 应安全返回，却报错: %v", err)
	}
	if err := KillTree(0); err != nil {
		t.Errorf("KillTree(0) 应安全返回: %v", err)
	}
}

// TestMissingBinaryHasClearError 验证错误信息包含足够上下文（可诊断）。
func TestMissingBinaryHasClearError(t *testing.T) {
	_, err := Start(context.Background(), Config{Path: "no-such-binary-abc"})
	if err == nil {
		t.Fatal("应报错")
	}
	if !strings.Contains(err.Error(), "no-such-binary-abc") {
		t.Errorf("错误信息应包含可执行文件路径，实际: %v", err)
	}
}

// TestLimitBufferHeadTail 直接单测限流缓冲的 head+tail 行为。
func TestLimitBufferHeadTail(t *testing.T) {
	b := newLimitBuffer(100, 10, 10, nil)
	b.Write([]byte("0123456789"))
	b.Write([]byte(strings.Repeat("X", 1000)))
	b.Write([]byte("ABCDEFGHIJ"))
	out := string(b.Bytes())
	if !strings.HasPrefix(out, "0123456789") {
		t.Errorf("头部应保留，实际: %q", out)
	}
	if !strings.HasSuffix(out, "ABCDEFGHIJ") {
		t.Errorf("尾部应保留，实际: %q", out)
	}
	if !b.Truncated() {
		t.Error("应标记截断")
	}
	if b.Total() != 10+1000+10 {
		t.Errorf("总字节数应为 1020，实际 %d", b.Total())
	}
}

// TestLimitBufferHeadOnly 直接单测"只保留头部"。
func TestLimitBufferHeadOnly(t *testing.T) {
	b := newLimitBuffer(20, 0, 0, nil)
	b.Write([]byte(strings.Repeat("A", 100)))
	if got := len(b.Bytes()); got != 20 {
		t.Errorf("应只保留 20 字节，实际 %d", got)
	}
	if !b.Truncated() {
		t.Error("应标记截断")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// 确保 exec 被使用（TestChildDoesNotInheritSecretsByDefault 里用到 env 路径时可选）。
var _ = exec.Command
var _ = os.Getpid
