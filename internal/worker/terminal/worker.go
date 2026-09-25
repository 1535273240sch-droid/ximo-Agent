package terminal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/worker"
	"github.com/ximo888ok-netizen/ximo-agent/internal/worker/procguard"
)

// execLookPath 包装 exec.LookPath。
func execLookPath(name string) (string, error) { return exec.LookPath(name) }

// ExecMode 决定命令如何被执行。
type ExecMode string

const (
	// ModeArgv（默认）把命令作为参数数组直接交给 OS，**不经任何 shell**。
	// 这是从根上消除 shell 注入的模式：`echo; rm -rf /` 会被当作 echo 的字面参数。
	ModeArgv ExecMode = "argv"
	// ModeShell 把整条命令字符串交给 shell 解释。必须显式 AllowShell=true 才可用，
	// 且默认仍然拒绝命令链接符。慎用。
	ModeShell ExecMode = "shell"
)

// Spec 是一次终端执行的完整请求（Args 的反序列化目标）。
type Spec struct {
	// Argv 是参数数组（Mode=argv）。优先使用；为空时才切分 Command。
	Argv []string `json:"argv,omitempty"`
	// Command 是命令字符串。Mode=argv 时按空白安全切分；Mode=shell 时整串交给 shell。
	Command string `json:"command,omitempty"`
	// Cwd 是工作目录，必须落在 AllowedRoots 内。
	Cwd string `json:"cwd,omitempty"`
	// Env 是调用方附加的环境变量（仍会经过密钥剔除）。
	Env map[string]string `json:"env,omitempty"`
	// Mode 默认 argv。
	Mode ExecMode `json:"mode,omitempty"`
	// Shell 指定 Mode=shell 时使用的解释器路径（空则用平台默认）。
	Shell string `json:"shell,omitempty"`
	// Timeout 覆盖策略默认 MaxRuntime（只能更小，不能更大）。
	Timeout time.Duration `json:"timeout,omitempty"`
	// Stdin 是标准输入内容。
	Stdin string `json:"stdin,omitempty"`
}

// Result 是一次终端执行的结果（响应体的 Result 字段）。
type Result struct {
	Command    string   `json:"command"`
	Argv       []string `json:"argv"`
	Cwd        string   `json:"cwd"`
	Mode       ExecMode `json:"mode"`
	ExitCode   int      `json:"exitCode"`
	Stdout     string   `json:"stdout"`
	Stderr     string   `json:"stderr"`
	Truncated  bool     `json:"truncated"`
	TimedOut   bool     `json:"timedOut"`
	Killed     bool     `json:"killed"`
	DurationMs int64    `json:"durationMs"`
	// Leaked 表示主进程退出后仍有后代进程存活，已被强制清理。
	// 这是 v1 完全缺失的能力——出现该字段为 true 说明命令 fork 了后台进程。
	Leaked bool `json:"leakedProcesses"`
	// StdoutBytes / StderrBytes 是截断前的真实字节数，便于 UI 提示"输出过多"。
	StdoutBytes int64 `json:"stdoutBytes"`
	StderrBytes int64 `json:"stderrBytes"`
}

// SplitCommand 把命令字符串切分成 argv，**不经过 shell**。
//
// 支持单引号与双引号分组以及反斜杠转义，因此 `git commit -m "fix: a b"` 能被正确切分，
// 而 `echo a; rm -rf /` 会得到 ["echo","a;","rm","-rf","/"] —— 分号只是普通字符，
// 不会被任何 shell 解释。这正是与 v1（整串丢给 powershell -Command）的根本区别。
func SplitCommand(s string) ([]string, error) {
	var (
		out                         []string
		cur                         strings.Builder
		inSingle, inDouble, escaped bool
		started                     bool
	)
	flush := func() {
		if started || cur.Len() > 0 {
			out = append(out, cur.String())
		}
		cur.Reset()
		started = false
	}
	for _, r := range s {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
			started = true
		case r == '\\' && !inSingle:
			escaped = true
		case r == '\'' && !inDouble:
			inSingle = !inSingle
			started = true
		case r == '"' && !inSingle:
			inDouble = !inDouble
			started = true
		case (r == ' ' || r == '\t' || r == '\n' || r == '\r') && !inSingle && !inDouble:
			flush()
		default:
			cur.WriteRune(r)
			started = true
		}
	}
	if inSingle || inDouble {
		return nil, fmt.Errorf("%w: 命令中的引号未闭合", worker.ErrInvalidArgument)
	}
	if escaped {
		return nil, fmt.Errorf("%w: 命令以反斜杠结尾", worker.ErrInvalidArgument)
	}
	flush()
	return out, nil
}

// Worker 是终端执行的独立故障域 Worker。
//
// 安全边界的三层结构：
//
//	Validator（策略判定，fail-closed）
//	  → 信号量（MaxProcesses 准入控制）
//	    → procguard（进程树管控 + 输出限流 + 超时强杀）
type Worker struct {
	id        string
	policy    TerminalPolicy
	validator *Validator

	// sem 是并发/进程数上限。v1 完全没有，100 个调用就起 100 个 shell。
	sem chan struct{}

	// 追踪活跃进程，供 PIDs() 上报与崩溃清理。
	mu     sync.Mutex
	procs  map[int]*procguard.Proc
	closed bool

	started bool
}

// NewWorker 创建 TerminalWorker。policy 的 AllowedRoots 必须显式配置，
// 否则一切执行都会被 fail-closed 拒绝。
func NewWorker(id string, policy TerminalPolicy) (*Worker, error) {
	v, err := NewValidator(policy)
	if err != nil {
		return nil, err
	}
	np := v.Policy().MaxProcesses
	return &Worker{
		id:        id,
		policy:    v.Policy(),
		validator: v,
		sem:       make(chan struct{}, np),
		procs:     make(map[int]*procguard.Proc),
	}, nil
}

// NewWorkerFromSpec 从 worker.Spec 构造（供 Manager Factory 使用）。
func NewWorkerFromSpec(spec worker.Spec) (worker.Worker, error) {
	p := DefaultPolicy()
	p.AllowedRoots = spec.Strings("allowed_roots")
	if d := spec.Duration("max_runtime", 0); d > 0 {
		p.MaxRuntime = d
	}
	if n := spec.Int("max_output_bytes", 0); n > 0 {
		p.MaxOutputBytes = int64(n)
	}
	if n := spec.Int("max_processes", 0); n > 0 {
		p.MaxProcesses = n
	}
	if s := spec.String("network_mode", ""); s != "" {
		p.NetworkMode = NetworkMode(s)
	}
	if s := spec.String("env_policy", ""); s != "" {
		p.EnvironmentPolicy = EnvPolicy(s)
	}
	if allow := spec.Strings("allowed_commands"); len(allow) > 0 {
		p.AllowedCommands = allow
	}
	p.AllowShell = spec.Bool("allow_shell", false)
	p.AllowCommandChaining = spec.Bool("allow_command_chaining", false)
	if ks := spec.Strings("env_allowlist"); len(ks) > 0 {
		p.EnvAllowlist = ks
	}
	return NewWorker(spec.ID, p)
}

// ID 实现 worker.Worker。
func (w *Worker) ID() string { return w.id }

// Kind 实现 worker.Worker。
func (w *Worker) Kind() string { return "terminal" }

// Start 实现 worker.Worker。终端 Worker 无需常驻外部进程，只做策略自检。
func (w *Worker) Start(ctx context.Context) error {
	if len(w.policy.AllowedRoots) == 0 {
		// 不是错误：fail-closed 策略是合法的（所有执行都会被拒绝）。
		// 但要让调用方知道为什么命令全被拒，因此这里只做记录式返回 nil。
		return nil
	}
	w.mu.Lock()
	w.started = true
	w.mu.Unlock()
	return nil
}

// Health 实现 worker.Worker：检查策略可用、无泄漏进程堆积。
func (w *Worker) Health(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return fmt.Errorf("%w: terminal worker 已关闭", worker.ErrUnavailable)
	}
	return nil
}

// Stop 实现 worker.Worker：杀掉所有仍在运行的进程树（进程树清理红线）。
func (w *Worker) Stop(ctx context.Context) error {
	w.mu.Lock()
	w.closed = true
	procs := make([]*procguard.Proc, 0, len(w.procs))
	for _, p := range w.procs {
		procs = append(procs, p)
	}
	w.mu.Unlock()

	for _, p := range procs {
		_ = p.Kill(ctx)
		_ = p.Close()
	}
	w.mu.Lock()
	w.procs = make(map[int]*procguard.Proc)
	w.mu.Unlock()
	return nil
}

// PIDs 实现 worker.ProcessAware：上报仍存活的外部进程，供 Manager 崩溃后清理。
func (w *Worker) PIDs() []int {
	w.mu.Lock()
	procs := make([]*procguard.Proc, 0, len(w.procs))
	for _, p := range w.procs {
		procs = append(procs, p)
	}
	w.mu.Unlock()

	var out []int
	for _, p := range procs {
		if !p.Exited() {
			out = append(out, p.PIDs()...)
		}
	}
	return out
}

// 动作名常量。
const (
	ActionExec = "exec"
)

// Execute 实现 worker.Worker。
func (w *Worker) Execute(ctx context.Context, req worker.WorkerRequest) (worker.WorkerResponse, error) {
	if req.Action != ActionExec && req.Action != "" {
		return worker.ErrorResponse(req, w.id, worker.CodeInvalidArgument,
			fmt.Sprintf("terminal worker 不支持动作 %q（仅支持 %q）", req.Action, ActionExec)), nil
	}
	started := time.Now()

	var spec Spec
	if err := req.Bind(&spec); err != nil {
		return worker.ErrorResponse(req, w.id, worker.CodeInvalidArgument, err.Error()), nil
	}

	// 第一层：策略判定。任何拒绝都在这里发生，绝不进入执行阶段。
	if err := w.validator.Validate(spec); err != nil {
		var pe *PolicyError
		if errors.As(err, &pe) {
			return worker.ErrorResponse(req, w.id, worker.CodePolicyDenied, pe.Error()), nil
		}
		return worker.ErrorResponse(req, w.id, worker.CodeInvalidArgument, err.Error()), nil
	}

	// 解析出真实 argv 与 cwd（此处已通过校验，可安全取用）。
	pol := w.validator.Policy()
	argv, err := w.validator.resolveArgv(spec)
	if err != nil {
		return worker.ErrorResponse(req, w.id, worker.CodeInvalidArgument, err.Error()), nil
	}
	cwd, err := w.validator.resolveWorkdir(spec.Cwd)
	if err != nil {
		return worker.ErrorResponse(req, w.id, worker.CodePolicyDenied, err.Error()), nil
	}

	// 超时只能收紧，不能放宽策略上限。
	timeout := pol.MaxRuntime
	if spec.Timeout > 0 && spec.Timeout < timeout {
		timeout = spec.Timeout
	}

	// 第二层：进程数准入控制。拿不到名额就排队等，不会无限起进程。
	if err := acquireSem(ctx, w.sem); err != nil {
		return worker.ErrorResponse(req, w.id, worker.CodeCanceled, "等待进程配额被取消"), nil
	}
	defer releaseSem(w.sem)

	// 第三层：交给 procguard（进程树管控 + 输出限流 + 超时强杀）。
	res, err := w.run(ctx, pol, spec, argv, cwd, timeout)
	if err != nil {
		return worker.ErrorResponseFrom(req, w.id, started, err), nil
	}

	// 退出码非 0 视为命令失败，但保留完整输出（LLM 需要看 stderr 才能修错）。
	resp := worker.WorkerResponse{
		CallID:    req.CallID,
		WorkerID:  w.id,
		StartedAt: started,
		EndedAt:   time.Now(),
	}
	body := map[string]any{
		"command":         res.Command,
		"argv":            res.Argv,
		"cwd":             res.Cwd,
		"mode":            res.Mode,
		"exitCode":        res.ExitCode,
		"stdout":          res.Stdout,
		"stderr":          res.Stderr,
		"truncated":       res.Truncated,
		"timedOut":        res.TimedOut,
		"killed":          res.Killed,
		"durationMs":      res.DurationMs,
		"leakedProcesses": res.Leaked,
		"stdoutBytes":     res.StdoutBytes,
		"stderrBytes":     res.StderrBytes,
		"content":         formatForLLM(res),
	}
	raw, _ := json.Marshal(body)
	resp.Result = raw
	resp.Metrics = worker.CallMetrics{
		ExecMillis:   res.DurationMs,
		OutputBytes:  res.StdoutBytes + res.StderrBytes,
		Truncated:    res.Truncated,
		ChildProcess: 1,
	}

	switch {
	case res.TimedOut:
		resp.Status = worker.StatusTimeout
		resp.Error = worker.NewWorkerError(worker.CodeTimeout,
			fmt.Sprintf("命令执行超过 %s 被终止（进程树已清理）", timeout), true)
	case res.ExitCode != 0:
		resp.Status = worker.StatusError
		resp.Error = worker.NewWorkerError(worker.CodeUpstream,
			fmt.Sprintf("命令退出码 %d", res.ExitCode), false)
	default:
		resp.Status = worker.StatusOK
	}
	return resp, nil
}

// run 用 procguard 执行命令并收集结果。
func (w *Worker) run(ctx context.Context, pol TerminalPolicy, spec Spec, argv []string, cwd string, timeout time.Duration) (Result, error) {
	exe, err := resolveExecutable(argv[0])
	if err != nil {
		return Result{}, err
	}

	cfg := procguard.Config{
		Path:  exe,
		Args:  argv[1:],
		Dir:   cwd,
		Env:   w.validator.BuildEnv(spec.Env),
		Stdin: strings.NewReader(spec.Stdin),

		MaxOutputBytes: pol.MaxOutputBytes,
		KillGrace:      pol.GracefulKillAfter,
		HideWindow:     true,
		CreateNoWindow: true,
		// 进程数上限同时作用于 OS 层（Windows Job Object ActiveProcessLimit），
		// 与上面的信号量形成"调用级 + 内核级"双重约束。
		MaxProcesses: pol.MaxProcesses + 1,
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	p, err := procguard.Start(runCtx, cfg)
	if err != nil {
		return Result{}, fmt.Errorf("%w: %v", worker.ErrInternal, err)
	}
	w.track(p)
	defer w.untrack(p)

	start := time.Now()
	pr, err := p.Wait(runCtx)
	_ = p.Close()
	if err != nil {
		return Result{}, fmt.Errorf("%w: %v", worker.ErrInternal, err)
	}

	return Result{
		Command:     strings.Join(argv, " "),
		Argv:        argv,
		Cwd:         cwd,
		Mode:        modeOf(spec),
		ExitCode:    pr.ExitCode,
		Stdout:      string(pr.Stdout),
		Stderr:      string(pr.Stderr),
		Truncated:   pr.Truncated,
		TimedOut:    pr.TimedOut,
		Killed:      pr.Killed,
		Leaked:      pr.Leaked,
		DurationMs:  time.Since(start).Milliseconds(),
		StdoutBytes: pr.StdoutTotal,
		StderrBytes: pr.StderrTotal,
	}, nil
}

func modeOf(spec Spec) ExecMode {
	if spec.Mode == ModeShell {
		return ModeShell
	}
	return ModeArgv
}

// resolveExecutable 把 argv[0] 解析为可执行文件的绝对路径。
//
// 用 LookPath 而不是直接把它交给 OS：一是确保"PATH 里找不到"能给出清晰错误，
// 二是避免相对路径在不同 cwd 下解析到不同文件（间接的 PATH/相对路径劫持面）。
func resolveExecutable(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("%w: 可执行文件名为空", worker.ErrInvalidArgument)
	}
	if strings.ContainsAny(name, `/\`) {
		// 显式路径：策略层已校验过它落在 AllowedRoots 内。
		return name, nil
	}
	full, err := lookPath(name)
	if err != nil {
		return "", fmt.Errorf("%w: 找不到可执行文件 %q: %v", worker.ErrInvalidArgument, name, err)
	}
	return full, nil
}

// track / untrack 维护活跃进程表。
func (w *Worker) track(p *procguard.Proc) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		// 已在停机流程中：新进程不允许进入追踪表，直接杀掉。
		_ = p.Kill(context.Background())
		return
	}
	w.procs[p.PID()] = p
}

func (w *Worker) untrack(p *procguard.Proc) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.procs, p.PID())
}

// formatForLLM 生成给 LLM 看的人读内容。
//
// 设计取舍：stdout 与 stderr 都要给，且要明确标注退出码与截断状态，
// 否则模型无法区分"命令成功但没输出"与"命令失败但 stderr 为空"。
func formatForLLM(res Result) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "$ %s\n", res.Command)
	if res.Cwd != "" {
		fmt.Fprintf(&sb, "(cwd: %s)\n", res.Cwd)
	}
	if res.Stdout != "" {
		sb.WriteString(res.Stdout)
		if !strings.HasSuffix(res.Stdout, "\n") {
			sb.WriteByte('\n')
		}
	}
	if res.Stderr != "" {
		sb.WriteString("[stderr]\n")
		sb.WriteString(res.Stderr)
		if !strings.HasSuffix(res.Stderr, "\n") {
			sb.WriteByte('\n')
		}
	}
	fmt.Fprintf(&sb, "[exit code: %d", res.ExitCode)
	if res.TimedOut {
		sb.WriteString(", 超时被终止")
	} else if res.Killed {
		sb.WriteString(", 被终止")
	}
	if res.Truncated {
		fmt.Fprintf(&sb, ", 输出已截断（stdout %d 字节 / stderr %d 字节）", res.StdoutBytes, res.StderrBytes)
	}
	if res.Leaked {
		sb.WriteString(", 检测到后台子进程并已清理")
	}
	sb.WriteString("]\n")
	return sb.String()
}
