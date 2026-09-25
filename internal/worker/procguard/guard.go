// Package procguard 负责"外部进程"的安全托管：拉起、超时、输出限流、**整棵进程树清理**。
//
// 存在的理由（任务书第 15 章审计重点）：
// v1 的 TerminalExecTool 在 Unix 上只对直接子进程发 SIGTERM，也没有独立进程组，
// 因此 `npm run dev` 之类的命令退出后，其子孙进程会被 init 收养并永久泄漏；
// 在 Windows 上虽然调了 `taskkill /T /F`，但那是 fire-and-forget 且没监听结果。
// v2 的要求是：**杀掉 shell 不等于杀掉 shell 的子进程**，必须用 OS 级机制兜住：
//
//   - Windows：Job Object + JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE（关句柄即全族退出），
//     并以 CreateToolhelp32Snapshot 枚举后代 PID 做二次兜底（覆盖 job 分配前的竞态窗口）。
//   - Unix：独立进程组（Setpgid）+ kill(-pgid, SIGKILL)，并以 /proc、ps 枚举后代兜底。
//
// 所有导出函数在目标平台上无第三方依赖，Windows 部分直接用 syscall 调 kernel32。
package procguard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// 错误哨兵。
var (
	// ErrNoSuchProcess 目标进程已不存在（kill 视为成功）。
	ErrNoSuchProcess = errors.New("procguard: 进程不存在")
	// ErrOutputLimit 输出超过上限（当前实现只截断不报错，保留此错误以便调用方选择严格模式）。
	ErrOutputLimit = errors.New("procguard: 输出超过上限")
	// ErrAlreadyStarted 重复 Start。
	ErrAlreadyStarted = errors.New("procguard: 进程已启动")
	// ErrNotStarted 未启动就调用 Wait/Kill。
	ErrNotStarted = errors.New("procguard: 进程未启动")
	// ErrProcessTreeLeak 主进程退出后仍检测到存活的后代进程（已被强制清理）。
	ErrProcessTreeLeak = errors.New("procguard: 检测到子进程泄漏并已清理")
)

// Config 描述要托管的一个外部进程。
//
// 安全默认值：Env 为 nil 时**不继承**父进程环境变量，而是只给一个最小环境。
// 这是与 v1 的关键差异——v1 直接 `...process.env` 全量透传，会把主进程里的
// API Key（第 21 章红线）泄漏给任意被调命令。调用方要继承时必须显式填写。
type Config struct {
	// Path 是要执行的绝对路径或 PATH 中的名字。建议传绝对路径（避免 PATH 劫持）。
	Path string
	// Args 是参数数组。**绝不接受整行命令字符串**——参数数组直传是防 shell 注入的根本。
	Args []string
	// Dir 是工作目录。空表示继承当前目录。调用方应先用 Policy 校验（AllowedRoots）。
	Dir string
	// Env 是完整环境变量列表（"K=V" 形式）。为 nil 时使用 MinimalEnv()。
	Env []string
	// InheritEnv 为 true 时才把 os.Environ() 合并进来（显式选择，默认关闭）。
	InheritEnv bool

	// Stdin 提供标准输入，nil 表示空。
	Stdin io.Reader
	// Interactive 为 true 时，调用方需要直接读写子进程的 stdin/stdout
	// （典型场景：MCP stdio server 的行分隔 JSON-RPC 长连接）。
	//
	// 此时 stdout **不会**经过限流缓冲，而是通过 Proc.StdoutPipe() 原样交给调用方，
	// 由调用方自己按行解析并限流；stderr 仍然走限流缓冲（server 日志通常打在 stderr）。
	// 语义上仍是"整棵进程树受管控"，只是输出通路交给调用方。
	Interactive bool
	// MaxOutputBytes 是 stdout/stderr **各自**的保留上限（字节）。
	// <=0 时使用 DefaultMaxOutputBytes。超限部分被丢弃但继续读取，
	// 保证子进程不会因管道写满而阻塞（v1 把全量输出堆在内存里会 OOM）。
	MaxOutputBytes int64
	// HeadBytes / TailBytes 控制超限时保留哪一段：默认保留头部（错误信息通常在头部）。
	// 两者都 >0 时表示"保留头部 N 字节 + 尾部 M 字节"，中间写省略标记。
	HeadBytes int64
	TailBytes int64

	// OnStdout / OnStderr 是流式回调（注意：回调里的数据同样受 MaxOutputBytes 约束）。
	OnStdout func([]byte)
	OnStderr func([]byte)

	// KillGrace 是终止时的友好等待时间：先 KillTree（强杀），Unix 上先 SIGTERM。
	// <=0 时使用 DefaultKillGrace。
	KillGrace time.Duration

	// MemoryLimitBytes >0 时通过 OS 机制限制整棵进程树的内存（Windows Job Object）。
	// Unix 上该字段暂不生效（需 cgroup，见文档说明），会通过 Capabilities() 如实上报。
	MemoryLimitBytes uint64
	// MaxProcesses >0 时限制进程树内的进程数（Windows Job Object ActiveProcessLimit）。
	MaxProcesses int
	// CPUQuotaPercent 预留字段（Unix cgroup 用），当前实现不生效，通过 Capabilities 上报。
	CPUQuotaPercent int

	// HideWindow（Windows）隐藏子进程窗口。
	HideWindow bool
	// CreateNoWindow（Windows）不创建控制台窗口。
	CreateNoWindow bool
}

// 默认值。
const (
	DefaultMaxOutputBytes int64 = 1 << 20 // 1 MiB / 流
	DefaultKillGrace            = 3 * time.Second
	// MinEnvKeep 是即使调用方自定义环境也强制保留的最小键，保证子进程能找到系统目录与临时目录。
	envPathKey = "PATH"
)

// MinimalEnv 返回一个最小环境变量集合：不继承父进程，避免密钥外泄（第 21 章）。
// 保留系统运行必需项（PATH/TEMP/HOME 等），并强制 UTF-8 相关变量（对齐 v1 的编码修复）。
func MinimalEnv() []string {
	keep := []string{
		"PATH", "Path", "SystemRoot", "SystemDrive", "windir", "COMSPEC",
		"TEMP", "TMP", "TMPDIR", "HOME", "USERPROFILE", "HOMEDRIVE", "HOMEPATH",
		"LANG", "LC_ALL", "TERM", "SHELL", "PATHEXT", "NUMBER_OF_PROCESSORS",
		"PROCESSOR_ARCHITECTURE", "OS", "ProgramData", "ProgramFiles", "ProgramFiles(x86)",
		"APPDATA", "LOCALAPPDATA", "USERNAME", "USER", "USERDOMAIN",
	}
	out := make([]string, 0, len(keep)+5)
	for _, k := range keep {
		if v, ok := os.LookupEnv(k); ok {
			out = append(out, k+"="+v)
		}
	}
	// 强制 UTF-8，避免 Windows 中文输出乱码（v1 也是这么修的，属于功能对等）。
	out = append(out,
		"FORCE_COLOR=0",
		"NO_COLOR=1",
		"PYTHONIOENCODING=utf-8",
		"PYTHONUTF8=1",
		"LANG=C.UTF-8",
	)
	return out
}

// Capabilities 如实上报当前平台对高级隔离能力的支持情况。
// 供 08 号集成时判断"哪些限制在本平台真正生效"，避免文档承诺与实现不符。
type Capabilities struct {
	ProcessTreeKill  bool
	JobObject        bool
	MemoryLimit      bool
	ProcessCountLim  bool
	ProcessGroupKill bool
	CPULimit         bool
	DescendantSweep  bool
}

// Result 是一次进程运行的完整结果。
type Result struct {
	PID       int
	ExitCode  int
	Signal    string
	StartedAt time.Time
	EndedAt   time.Time
	// Stdout/Stderr 是受上限约束后的输出。
	Stdout []byte
	Stderr []byte
	// Truncated 表示输出被截断（stdout 或 stderr）。
	Truncated   bool
	StdoutTotal int64
	StderrTotal int64
	// Leaked 表示主进程退出后仍检测到后代进程（已清理）。
	Leaked bool
	// TimedOut 表示因超时被杀。
	TimedOut bool
	// Killed 表示被显式 Kill。
	Killed bool
}

// Proc 托管一个外部进程及其整棵进程树。
//
// 生命周期：
//
//	p, err := procguard.Start(ctx, cfg)
//	res, err := p.Wait(ctx)      // 读取输出 + 等待退出 + 清理残留后代
//	_ = p.Close()                // 释放 Job Object 句柄（Windows 关句柄即全族退出）
type Proc struct {
	cfg  Config
	cmd  *exec.Cmd
	job  jobHandle // Windows Job Object；Unix 上为 nil 实现
	pgid int       // Unix 进程组 ID（= 子进程 PID）

	outBuf *limitBuffer
	errBuf *limitBuffer

	startedAt time.Time
	mu        sync.Mutex
	started   bool
	exited    bool
	exitCode  int
	signal    string
	killed    bool
	timedOut  bool
	leaked    bool

	waitOnce sync.Once
	waitErr  error

	// 交互模式（Config.Interactive）下由调用方直接读写的管道。
	stdinPipe io.WriteCloser
	stdoutRaw io.ReadCloser
}

// StdoutPipe 返回原生 stdout 管道（仅 Config.Interactive 模式可用）。
// 调用方负责按自己的协议解析与限流。
func (p *Proc) StdoutPipe() (io.ReadCloser, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stdoutRaw == nil {
		return nil, ErrNotStarted
	}
	return p.stdoutRaw, nil
}

// StdinPipe 返回 stdin 管道（仅 Config.Interactive 模式可用）。
func (p *Proc) StdinPipe() (io.WriteCloser, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stdinPipe == nil {
		return nil, ErrNotStarted
	}
	return p.stdinPipe, nil
}

// Start 拉起进程。
//
// 顺序很重要：
//  1. Windows 上先建 Job Object（带 KILL_ON_JOB_CLOSE），再启动子进程，然后立即 assign；
//  2. Unix 上设置独立进程组（Setpgid），使 kill(-pgid) 能覆盖整棵树。
func Start(ctx context.Context, cfg Config) (*Proc, error) {
	if strings.TrimSpace(cfg.Path) == "" {
		return nil, errors.New("procguard: Path 不能为空")
	}
	if cfg.MaxOutputBytes <= 0 {
		cfg.MaxOutputBytes = DefaultMaxOutputBytes
	}
	if cfg.KillGrace <= 0 {
		cfg.KillGrace = DefaultKillGrace
	}

	env := cfg.Env
	if env == nil {
		env = MinimalEnv()
		if cfg.InheritEnv {
			env = append(append([]string(nil), os.Environ()...), env...)
		}
	}

	cmd := exec.Command(cfg.Path, cfg.Args...)
	cmd.Dir = cfg.Dir
	cmd.Env = env
	// 交互模式下 stdin 必须留给 StdinPipe（两者互斥，同时设置会直接报错）。
	if !cfg.Interactive {
		cmd.Stdin = cfg.Stdin
	}

	outBuf := newLimitBuffer(cfg.MaxOutputBytes, cfg.HeadBytes, cfg.TailBytes, cfg.OnStdout)
	errBuf := newLimitBuffer(cfg.MaxOutputBytes, cfg.HeadBytes, cfg.TailBytes, cfg.OnStderr)

	p := &Proc{cfg: cfg, cmd: cmd, outBuf: outBuf, errBuf: errBuf}

	// 非交互模式：把限流缓冲**直接**作为 cmd.Stdout/Stderr。
	// 这样 os/exec 自己建立管道并起拷贝 goroutine，cmd.Wait() 会正确地等它们收尾；
	// 手写 StdoutPipe + 另起 goroutine 读是 os/exec 文档明确指出的错误用法
	// （Wait 会关闭管道，与读取方竞争，可能丢数据或死锁）。
	// limitBuffer 自带互斥锁，stdout/stderr 分别写入两个实例，无竞争。
	if !cfg.Interactive {
		cmd.Stdout = outBuf
		cmd.Stderr = errBuf
		return p.start(nil, false)
	}

	// 交互模式：stdin/stdout 交给调用方做协议读写，stderr 仍走限流缓冲。
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("procguard: 取 stdin 管道失败: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("procguard: 取 stdout 管道失败: %w", err)
	}
	cmd.Stderr = errBuf
	p.stdinPipe = stdin
	return p.start(stdout, true)
}

// start 完成 SysProcAttr 设置、平台准备、启动与纳入管控。
//
// interactive 为 true 时，stdout 交由调用方按协议读（此时 stdout 非 nil）；
// 非交互模式下 os/exec 已经接管 stdout/stderr 的拷贝，本函数不做额外装配。
func (p *Proc) start(stdout io.ReadCloser, interactive bool) (*Proc, error) {
	cmd := p.cmd
	cfg := p.cfg

	// 平台相关的 SysProcAttr 与 Job Object 准备。
	applySysProcAttr(cmd, cfg)
	if err := p.preparePlatform(&cfg); err != nil {
		p.releasePlatform()
		return nil, err
	}

	if err := cmd.Start(); err != nil {
		p.releasePlatform()
		return nil, fmt.Errorf("procguard: 启动 %s 失败: %w", cfg.Path, err)
	}

	p.mu.Lock()
	p.started = true
	p.startedAt = time.Now()
	p.mu.Unlock()

	if err := p.attachPlatform(cmd.Process.Pid); err != nil {
		// 附带失败意味着进程树不可靠：宁可杀掉也不要留下管不住的孤儿。
		p.forceKillTree()
		p.releasePlatform()
		return nil, fmt.Errorf("procguard: 纳入进程树管控失败: %w", err)
	}

	if interactive {
		// stdout 交给调用方做协议读写（已在 Start 里取好并存入 stdoutRaw）。
		p.stdoutRaw = stdout
	}
	return p, nil
}

// PID 返回主进程 PID。未启动时返回 0。
func (p *Proc) PID() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

// PIDs 返回主进程 + 当前存活后代进程的 PID 快照（用于泄漏审计与兜底清理）。
func (p *Proc) PIDs() []int {
	pid := p.PID()
	if pid <= 0 {
		return nil
	}
	return Descendants(pid)
}

// Command 返回便于日志展示的命令行（已脱敏：只显示路径与参数，不含 env）。
func (p *Proc) Command() string {
	return strings.Join(append([]string{p.cfg.Path}, p.cfg.Args...), " ")
}

// Wait 等待进程退出，随后清理残留后代并返回结果。
//
// 超时/取消由 ctx 控制：ctx 结束时进程树被强制清理，Result.TimedOut 置位。
func (p *Proc) Wait(ctx context.Context) (Result, error) {
	p.mu.Lock()
	if !p.started {
		p.mu.Unlock()
		return Result{}, ErrNotStarted
	}
	p.mu.Unlock()

	waitErrCh := make(chan error, 1)
	go func() {
		// cmd.Wait 会等待 os/exec 内部的输出拷贝 goroutine 收尾，
		// 因此返回时 outBuf/errBuf 已包含全部受限输出。
		waitErrCh <- p.cmd.Wait()
	}()

	var waitErr error
	select {
	case waitErr = <-waitErrCh:
	case <-ctx.Done():
		p.mu.Lock()
		p.timedOut = errors.Is(ctx.Err(), context.DeadlineExceeded)
		p.killed = !p.timedOut
		p.mu.Unlock()
		p.forceKillTree()
		// 杀掉后必须把退出事件读出来，否则 waitErrCh 泄漏 goroutine。
		waitErr = <-waitErrCh
	}

	p.mu.Lock()
	p.exited = true
	if p.cmd.ProcessState != nil {
		p.exitCode = p.cmd.ProcessState.ExitCode()
	}
	p.mu.Unlock()

	// 关键一步：主进程退出 ≠ 后代退出。这里做一次后代清扫，
	// 彻底解决"命令结束后子进程仍在运行"的泄漏（v1 完全没做）。
	leaked := p.sweepDescendants()
	p.mu.Lock()
	p.leaked = leaked
	if leaked {
		p.killed = true
	}
	// 直接读 cmd.Process.Pid，不能调 p.PID()——那会二次获取 p.mu 造成自死锁。
	pid := 0
	if p.cmd != nil && p.cmd.Process != nil {
		pid = p.cmd.Process.Pid
	}
	res := Result{
		PID:         pid,
		ExitCode:    p.exitCode,
		StartedAt:   p.startedAt,
		EndedAt:     time.Now(),
		Stdout:      p.outBuf.Bytes(),
		Stderr:      p.errBuf.Bytes(),
		Truncated:   p.outBuf.Truncated() || p.errBuf.Truncated(),
		StdoutTotal: p.outBuf.Total(),
		StderrTotal: p.errBuf.Total(),
		Leaked:      p.leaked,
		TimedOut:    p.timedOut,
		Killed:      p.killed,
	}
	p.mu.Unlock()

	if waitErr != nil && !res.TimedOut && !res.Killed {
		var ee *exec.ExitError
		if !errors.As(waitErr, &ee) {
			return res, fmt.Errorf("procguard: 等待进程失败: %w", waitErr)
		}
	}
	return res, nil
}

// Kill 主动终止整棵进程树（先友好后强制）。幂等。
func (p *Proc) Kill(ctx context.Context) error {
	p.mu.Lock()
	if !p.started || p.exited {
		p.mu.Unlock()
		return nil
	}
	p.killed = true
	p.mu.Unlock()
	return p.killTree(ctx)
}

// Runtime 返回进程已运行时长。
func (p *Proc) Runtime() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.startedAt.IsZero() {
		return 0
	}
	return time.Since(p.startedAt)
}

// Exited 报告进程是否已退出。
func (p *Proc) Exited() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exited
}

// Close 释放平台资源（Windows 上关闭 Job Object 句柄会连带杀掉残留后代）。幂等。
func (p *Proc) Close() error {
	return p.releasePlatform()
}

// killTree 执行"友好 → 强制"的进程树终止。
func (p *Proc) killTree(ctx context.Context) error {
	pid := p.PID()
	if pid <= 0 {
		return nil
	}
	grace := p.cfg.KillGrace
	if grace > 0 {
		// 友好阶段：请求整棵树退出（Unix: SIGTERM 到进程组；Windows: 无对应信号，跳过）。
		p.requestGracefulExit()
		timer := time.NewTimer(grace)
		defer timer.Stop()
		tk := time.NewTicker(50 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				p.forceKillTree()
				return ctx.Err()
			case <-timer.C:
				p.forceKillTree()
				return nil
			case <-tk.C:
				if !ProcessAlive(pid) {
					return nil
				}
			}
		}
	}
	p.forceKillTree()
	return nil
}

// sweepDescendants 在主进程退出后清理残留后代，并返回是否发生过泄漏。
func (p *Proc) sweepDescendants() bool {
	pid := p.PID()
	if pid <= 0 {
		return false
	}
	// 给操作系统一点时间回收进程表（否则刚退出的进程还挂在快照里）。
	leaked := false
	for i := 0; i < 3; i++ {
		pids := Descendants(pid)
		if len(pids) == 0 {
			break
		}
		leaked = true
		for _, d := range pids {
			killSingle(d)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// 清空该进程树在 Windows Job Object 里的残留。
	p.terminateJob()
	return leaked
}

// ProcessAlive 报告 PID 是否仍存活。
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return processAlive(pid)
}

// KillTree 按 PID 强制清理整棵进程树（不依赖 *Proc 句柄的兜底入口）。
// Manager 在 Worker 崩溃后用它清理孤儿进程。
func KillTree(pid int) error {
	if pid <= 0 {
		return nil
	}
	return killTreeByPID(pid)
}

// parseEnvList 把 "K=V" 列表转成 map（内部使用，便于白名单过滤）。
func parseEnvList(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i > 0 {
			out[kv[:i]] = kv[i+1:]
		}
	}
	return out
}
