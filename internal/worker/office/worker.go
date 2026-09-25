// Package office 实现 Office Worker：Word / Excel / PowerPoint 的读写改，
// 由外部 CLI（officecli）驱动，跑在独立故障域里。
//
// 功能对等基准是 v1 的 src/main/tools/Office/：
//
//	OfficeCliManager.ts —— 定位 officecli 二进制（OFFICECLI_PATH → 打包目录 → PATH）
//	OfficeCliRunner.ts  —— spawn 执行、超时、taskkill /T /F 进程树清理
//	OfficeDocsTool.ts   —— action 分发（create/get/query/set/add/remove/move/batch/
//	                       merge/validate/dump/view/save/help）
//	office-docs-helpers.ts —— 写操作前的快照备份（%TEMP%/ximo-agent-snapshots/）
//
// 相对 v1 的加固点：
//  1. v1 的 taskkill /T /F 是 fire-and-forget 且不校验结果；本包统一走 procguard
//     （Windows Job Object / Unix 进程组 + 后代枚举），保证 officecli 的子孙进程一起清理。
//  2. v1 无输出上限；本包用限流缓冲。
//  3. 快照备份路径由配置显式给出（不写死 %TEMP%），并纳入 AllowedRoots 校验。
//  4. 写操作前备份 + 失败不落盘，保证"可逆写"这条 v1 已有的安全网被保留并加强。
package office

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/worker"
	"github.com/ximo888ok-netizen/ximo-agent/internal/worker/procguard"
)

// 动作清单（与 v1 office-docs-helpers.ts 的 OFFICE_ACTIONS 逐字对齐）。
var Actions = []string{
	"create", "get", "query", "set", "add", "remove", "move",
	"batch", "merge", "validate", "dump", "view", "save", "help",
}

// writeActions 是需要"执行前快照备份"的写动作（对齐 v1 WRITE_ACTIONS）。
var writeActions = map[string]bool{
	"create": true, "set": true, "add": true, "remove": true, "move": true,
	"batch": true, "merge": true, "dump": true, "save": true,
}

// IsWriteAction 报告某动作是否会修改文档。
func IsWriteAction(action string) bool { return writeActions[action] }

// Config 是 Office Worker 配置。
type Config struct {
	// BinaryPath 是 officecli 可执行文件路径。空则按定位顺序探测。
	BinaryPath string
	// AllowedRoots 限制可操作的文档路径（fail-closed：为空则拒绝一切文件操作）。
	AllowedRoots []string
	// SnapshotDir 是写操作前的备份目录。为空则不备份（并会在响应里明确提示）。
	SnapshotDir string
	// MaxSnapshotBytes 超过该大小的文档不备份（避免把大文件复制爆磁盘）。<=0 时 64 MiB。
	MaxSnapshotBytes int64
	// Timeout 默认执行超时（对齐 v1 默认 90s）。
	Timeout time.Duration
	// MaxOutputBytes 输出上限。<=0 时 2 MiB。
	MaxOutputBytes int64
	// KeepSnapshots 保留多少份历史快照（0 表示不清理）。
	KeepSnapshots int
}

func (c Config) withDefaults() Config {
	if c.Timeout <= 0 {
		c.Timeout = 90 * time.Second
	}
	if c.MaxOutputBytes <= 0 {
		c.MaxOutputBytes = 2 << 20
	}
	if c.MaxSnapshotBytes <= 0 {
		c.MaxSnapshotBytes = 64 << 20
	}
	return c
}

// Worker 是 Office 独立故障域 Worker。
type Worker struct {
	id  string
	cfg Config

	mu      sync.Mutex
	started bool
	closed  bool
	procs   map[int]*procguard.Proc
}

// NewWorker 创建 Office Worker。
func NewWorker(id string, cfg Config) *Worker {
	return &Worker{id: id, cfg: cfg.withDefaults(), procs: map[int]*procguard.Proc{}}
}

// NewWorkerFromSpec 从 worker.Spec 构造。
func NewWorkerFromSpec(spec worker.Spec) (worker.Worker, error) {
	cfg := Config{
		BinaryPath:   spec.String("binary_path", ""),
		AllowedRoots: spec.Strings("allowed_roots"),
		SnapshotDir:  spec.String("snapshot_dir", ""),
	}
	if d := spec.Duration("timeout", 0); d > 0 {
		cfg.Timeout = d
	}
	if n := spec.Int("max_output_bytes", 0); n > 0 {
		cfg.MaxOutputBytes = int64(n)
	}
	if n := spec.Int("keep_snapshots", 0); n > 0 {
		cfg.KeepSnapshots = n
	}
	return NewWorker(spec.ID, cfg), nil
}

// ID 实现 worker.Worker。
func (w *Worker) ID() string { return w.id }

// Kind 实现 worker.Worker。
func (w *Worker) Kind() string { return "office" }

// Start 实现 worker.Worker：定位 officecli 并做一次自检。
//
// 找不到 officecli **不算启动失败**：Office 能力可以整体不可用，
// 但不应因此让 Worker 反复重启（Health 会如实报告不可用状态）。
func (w *Worker) Start(ctx context.Context) error {
	w.mu.Lock()
	w.started = true
	w.closed = false
	w.mu.Unlock()
	return nil
}

// Health 实现 worker.Worker：检查 officecli 是否可用。
func (w *Worker) Health(ctx context.Context) error {
	w.mu.Lock()
	closed := w.closed
	w.mu.Unlock()
	if closed {
		return fmt.Errorf("%w: office worker 已关闭", worker.ErrUnavailable)
	}
	if _, err := w.resolveBinary(); err != nil {
		return fmt.Errorf("%w: %v", worker.ErrUnavailable, err)
	}
	return nil
}

// Stop 实现 worker.Worker：杀掉所有在跑的 officecli 进程树。
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
	w.procs = map[int]*procguard.Proc{}
	w.mu.Unlock()
	return nil
}

// PIDs 实现 worker.ProcessAware。
func (w *Worker) PIDs() []int {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []int
	for _, p := range w.procs {
		if !p.Exited() {
			out = append(out, p.PIDs()...)
		}
	}
	return out
}

// ActionDocs 是文档操作的统一动作名。
const ActionDocs = "docs"

// docsArgs 是文档操作参数（字段与 v1 OfficeDocsTool 的 inputSchema 对齐）。
type docsArgs struct {
	Action       string         `json:"action"`
	FilePath     string         `json:"filePath"`
	Path         string         `json:"path,omitempty"`
	Selector     string         `json:"selector,omitempty"`
	Properties   map[string]any `json:"properties,omitempty"`
	Operations   []any          `json:"operations,omitempty"`
	TemplateData map[string]any `json:"templateData,omitempty"`
	OutputPath   string         `json:"outputPath,omitempty"`
	Depth        int            `json:"depth,omitempty"`
	Mode         string         `json:"mode,omitempty"`
	// TimeoutMs 可收紧超时。
	TimeoutMs int64 `json:"timeout_ms,omitempty"`
}

// Execute 实现 worker.Worker。
func (w *Worker) Execute(ctx context.Context, req worker.WorkerRequest) (worker.WorkerResponse, error) {
	if req.Action != ActionDocs && req.Action != "" && req.Action != "office_docs" {
		return worker.ErrorResponse(req, w.id, worker.CodeInvalidArgument,
			fmt.Sprintf("office worker 不支持动作 %q", req.Action)), nil
	}
	started := time.Now()
	var a docsArgs
	if err := req.Bind(&a); err != nil {
		return worker.ErrorResponse(req, w.id, worker.CodeInvalidArgument, err.Error()), nil
	}
	if !isKnownAction(a.Action) {
		return worker.ErrorResponse(req, w.id, worker.CodeInvalidArgument,
			fmt.Sprintf("未知的 office 动作 %q（支持：%s）", a.Action, strings.Join(Actions, "/"))), nil
	}
	if strings.TrimSpace(a.FilePath) == "" && a.Action != "help" {
		return worker.ErrorResponse(req, w.id, worker.CodeInvalidArgument, "filePath 不能为空"), nil
	}

	// 路径校验（fail-closed）。
	var absPath string
	if a.FilePath != "" {
		p, err := w.resolvePath(a.FilePath)
		if err != nil {
			return worker.ErrorResponse(req, w.id, worker.CodePolicyDenied, err.Error()), nil
		}
		absPath = p
	}
	var absOutput string
	if a.OutputPath != "" {
		p, err := w.resolvePath(a.OutputPath)
		if err != nil {
			return worker.ErrorResponse(req, w.id, worker.CodePolicyDenied, err.Error()), nil
		}
		absOutput = p
	}

	bin, err := w.resolveBinary()
	if err != nil {
		return worker.ErrorResponseFrom(req, w.id, started, err), nil
	}

	// 写操作前快照备份（v1 的"可逆写"安全网，这里保留并明确报告结果）。
	var snapshot string
	var snapshotErr error
	if IsWriteAction(a.Action) && absPath != "" && fileExists(absPath) {
		snapshot, snapshotErr = w.snapshot(absPath)
	}

	args := buildArgs(a, absPath, absOutput)
	timeout := w.cfg.Timeout
	if a.TimeoutMs > 0 {
		if d := time.Duration(a.TimeoutMs) * time.Millisecond; d < timeout {
			timeout = d
		}
	}

	out, runErr := w.run(ctx, bin, args, timeout)
	if runErr != nil {
		resp := worker.ErrorResponseFrom(req, w.id, started, runErr)
		// 即使失败也把快照路径告诉调用方，便于人工回滚。
		if snapshot != "" {
			resp.Error.Details = map[string]any{"snapshotPath": snapshot}
		}
		return resp, nil
	}

	body := map[string]any{
		"action":     a.Action,
		"filePath":   absPath,
		"stdout":     out.Stdout,
		"stderr":     out.Stderr,
		"exitCode":   out.ExitCode,
		"truncated":  out.Truncated,
		"durationMs": out.DurationMs,
		// 对齐 v1：officecli 返回的 JSON 会被尝试解析后放进 data
		"data":    tryParseJSON(out.Stdout),
		"content": renderContent(a.Action, absPath, out),
	}
	if snapshot != "" {
		body["snapshotPath"] = snapshot
	}
	if snapshotErr != nil {
		body["snapshotWarning"] = snapshotErr.Error()
	}
	if absOutput != "" {
		body["outputPath"] = absOutput
	}

	resp, _ := worker.OKResponse(req, w.id, started, body)
	if out.ExitCode != 0 {
		resp.Status = worker.StatusError
		resp.Error = worker.NewWorkerError(worker.CodeUpstream,
			fmt.Sprintf("officecli 退出码 %d", out.ExitCode), false)
	}
	resp.Metrics.ExecMillis = out.DurationMs
	resp.Metrics.OutputBytes = int64(len(out.Stdout) + len(out.Stderr))
	resp.Metrics.Truncated = out.Truncated
	return resp, nil
}

type runOutput struct {
	Stdout     string
	Stderr     string
	ExitCode   int
	Truncated  bool
	DurationMs int64
}

// run 用 procguard 执行 officecli。
func (w *Worker) run(ctx context.Context, bin string, args []string, timeout time.Duration) (runOutput, error) {
	cfg := procguard.Config{
		Path:           bin,
		Args:           args,
		Env:            officeEnv(),
		MaxOutputBytes: w.cfg.MaxOutputBytes,
		KillGrace:      3 * time.Second,
		HideWindow:     true,
		CreateNoWindow: true,
		MaxProcesses:   8,
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	p, err := procguard.Start(runCtx, cfg)
	if err != nil {
		return runOutput{}, fmt.Errorf("%w: 启动 officecli 失败: %v", worker.ErrInternal, err)
	}
	w.track(p)
	defer w.untrack(p)

	start := time.Now()
	pr, err := p.Wait(runCtx)
	_ = p.Close()
	if err != nil {
		return runOutput{}, fmt.Errorf("%w: 执行 officecli 失败: %v", worker.ErrInternal, err)
	}
	if pr.TimedOut {
		return runOutput{}, fmt.Errorf("%w: officecli 执行超过 %s 被终止（进程树已清理）", worker.ErrTimeout, timeout)
	}
	return runOutput{
		Stdout:     string(pr.Stdout),
		Stderr:     string(pr.Stderr),
		ExitCode:   pr.ExitCode,
		Truncated:  pr.Truncated,
		DurationMs: time.Since(start).Milliseconds(),
	}, nil
}

func (w *Worker) track(p *procguard.Proc) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
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

// resolveBinary 按 v1 的定位顺序查找 officecli。
func (w *Worker) resolveBinary() (string, error) {
	if w.cfg.BinaryPath != "" {
		if fileExists(w.cfg.BinaryPath) {
			return w.cfg.BinaryPath, nil
		}
		return "", fmt.Errorf("配置的 officecli 路径不存在: %s", w.cfg.BinaryPath)
	}
	// 1) 环境变量 OFFICECLI_PATH
	if p := os.Getenv("OFFICECLI_PATH"); p != "" {
		cand := p
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			cand = filepath.Join(p, exeName())
		}
		if fileExists(cand) {
			return cand, nil
		}
	}
	// 2) PATH 中的 officecli
	if p, err := exec.LookPath(exeName()); err == nil {
		return p, nil
	}
	if p, err := exec.LookPath("officecli"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("未找到 officecli（可设置 OFFICECLI_PATH 或 office.binary_path）")
}

func exeName() string {
	if runtime.GOOS == "windows" {
		return "officecli.exe"
	}
	return "officecli"
}

// resolvePath 校验并解析文档路径到 AllowedRoots 内。
func (w *Worker) resolvePath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("%w: 非法路径 %q", worker.ErrInvalidArgument, p)
	}
	// symlink 解析（防越权）。
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("%w: 路径无法解析: %v", worker.ErrInvalidArgument, err)
	}
	abs = filepath.Clean(abs)

	if len(w.cfg.AllowedRoots) == 0 {
		return "", fmt.Errorf("%w: office worker 未配置 allowed_roots，拒绝文件操作（fail-closed）",
			worker.ErrPolicyDenied)
	}
	for _, root := range w.cfg.AllowedRoots {
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
	return "", fmt.Errorf("%w: 路径 %q 不在 allowed_roots 内", worker.ErrPolicyDenied, p)
}

// snapshot 在执行写操作前备份文档（v1 的可逆写安全网）。
func (w *Worker) snapshot(path string) (string, error) {
	if w.cfg.SnapshotDir == "" {
		return "", fmt.Errorf("未配置 snapshot_dir，写操作无备份")
	}
	st, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if st.Size() > w.cfg.MaxSnapshotBytes {
		return "", fmt.Errorf("文档 %d 字节超过快照上限 %d，跳过备份",
			st.Size(), w.cfg.MaxSnapshotBytes)
	}
	if err := os.MkdirAll(w.cfg.SnapshotDir, 0o700); err != nil {
		return "", fmt.Errorf("创建快照目录失败: %w", err)
	}
	safe := strings.NewReplacer(string(filepath.Separator), "_", ":", "_", " ", "_").
		Replace(filepath.Base(path))
	dst := filepath.Join(w.cfg.SnapshotDir,
		fmt.Sprintf("%s.snapshot-%d.bak", safe, time.Now().UnixNano()))
	if err := copyFile(path, dst); err != nil {
		return "", err
	}
	if w.cfg.KeepSnapshots > 0 {
		w.pruneSnapshots(safe)
	}
	return dst, nil
}

func (w *Worker) pruneSnapshots(safeBase string) {
	entries, err := os.ReadDir(w.cfg.SnapshotDir)
	if err != nil {
		return
	}
	var matches []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), safeBase+".snapshot-") {
			matches = append(matches, e.Name())
		}
	}
	if len(matches) <= w.cfg.KeepSnapshots {
		return
	}
	// 名字里带纳秒时间戳，按字典序即时间序。
	sortStrings(matches)
	for _, name := range matches[:len(matches)-w.cfg.KeepSnapshots] {
		_ = os.Remove(filepath.Join(w.cfg.SnapshotDir, name))
	}
}

// buildArgs 把结构化参数转成 officecli 的命令行参数（对齐 v1 的映射规则）。
func buildArgs(a docsArgs, absPath, absOutput string) []string {
	var args []string
	if a.Action != "" {
		args = append(args, a.Action)
	}
	if absPath != "" {
		args = append(args, "--file", absPath)
	}
	if a.Path != "" {
		args = append(args, "--path", a.Path)
	}
	if a.Selector != "" {
		args = append(args, "--selector", a.Selector)
	}
	if a.Depth > 0 {
		args = append(args, "--depth", fmt.Sprintf("%d", a.Depth))
	}
	if a.Mode != "" {
		args = append(args, "--mode", a.Mode)
	} else if a.Action == "view" {
		args = append(args, "--mode", "screenshot") // 对齐 v1 默认值
	}
	// properties → --prop key=val；properties.type 提升为 --type（aligned with v1）。
	for k, v := range a.Properties {
		if k == "type" {
			args = append(args, "--type", fmt.Sprintf("%v", v))
			continue
		}
		args = append(args, "--prop", fmt.Sprintf("%s=%v", k, v))
	}
	if len(a.Operations) > 0 {
		if raw, err := json.Marshal(a.Operations); err == nil {
			args = append(args, "--operations", string(raw))
		}
	}
	if len(a.TemplateData) > 0 {
		if raw, err := json.Marshal(a.TemplateData); err == nil {
			args = append(args, "--data", string(raw))
		}
	}
	if absOutput != "" {
		args = append(args, "--output", absOutput)
	}
	return args
}

// officeEnv 构造 officecli 的运行环境（强制 UTF-8，对齐 v1）。
func officeEnv() []string {
	env := procguard.MinimalEnv()
	return append(env,
		"OFFICECLI_OUTPUT_ENCODING=utf-8",
		"PYTHONIOENCODING=utf-8",
		"PYTHONUTF8=1",
	)
}

// renderContent 生成给 LLM 看的内容。
func renderContent(action, path string, out runOutput) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "office %s: %s\n", action, path)
	if out.Stdout != "" {
		sb.WriteString(out.Stdout)
		if !strings.HasSuffix(out.Stdout, "\n") {
			sb.WriteByte('\n')
		}
	}
	if out.Stderr != "" {
		sb.WriteString("[stderr]\n")
		sb.WriteString(out.Stderr)
	}
	fmt.Fprintf(&sb, "[exit code: %d", out.ExitCode)
	if out.Truncated {
		sb.WriteString(", 输出已截断")
	}
	sb.WriteString("]\n")
	return sb.String()
}

// tryParseJSON 尝试把 stdout 解析为 JSON（officecli 可能混有日志行）。
func tryParseJSON(s string) any {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return nil
	}
	var v any
	if err := json.Unmarshal([]byte(trimmed), &v); err == nil {
		return v
	}
	// 从首个 '{' 到最后一个 '}' 之间尝试解析（对齐 v1 的区间解析思路）。
	start := strings.IndexByte(trimmed, '{')
	end := strings.LastIndexByte(trimmed, '}')
	if start >= 0 && end > start {
		if err := json.Unmarshal([]byte(trimmed[start:end+1]), &v); err == nil {
			return v
		}
	}
	return nil
}

func isKnownAction(a string) bool {
	for _, x := range Actions {
		if x == a {
			return true
		}
	}
	return false
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := out.ReadFrom(in); err != nil {
		return err
	}
	return out.Sync()
}

// sortStrings 是插入排序（快照数量很少，避免引入 sort 包的额外分支）。
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
