package supervisor

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"
)

// ProcessExitEvent 当子进程退出时触发的事件
type ProcessExitEvent struct {
	ProcessID int
	ExitCode  int
	Error     error
	ExitTime  time.Time
}

// ManagedProcess 管理单个子进程的完整生命周期与资源清理
type ManagedProcess struct {
	mu         sync.RWMutex
	binaryPath string
	args       []string
	env        []string
	workingDir string

	cmd      *exec.Cmd
	pid      int
	alive    bool
	exitChan chan ProcessExitEvent

	stderrBuf *RingBuffer
	stdoutBuf *RingBuffer

	cancel context.CancelFunc
}

// NewManagedProcess 创建托管子进程对象
func NewManagedProcess(binaryPath string, args []string, env []string, workingDir string) *ManagedProcess {
	if binaryPath == "" {
		// 默认使用当前可执行文件自身（同一个二进制多角色分发原则，第2.2章）
		self, err := os.Executable()
		if err == nil {
			binaryPath = self
		}
	}
	return &ManagedProcess{
		binaryPath: binaryPath,
		args:       args,
		env:        env,
		workingDir: workingDir,
		stderrBuf:  NewRingBuffer(64 * 1024), // 保留最新 64KB stderr
		stdoutBuf:  NewRingBuffer(32 * 1024), // 保留最新 32KB stdout
		exitChan:   make(chan ProcessExitEvent, 1),
	}
}

// Start 启动子进程，绑定操作系统进程组/JobObject，建立日志流与退出监听
func (p *ManagedProcess) Start() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.alive {
		return fmt.Errorf("process already running (PID: %d)", p.pid)
	}

	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel

	cmd := exec.CommandContext(ctx, p.binaryPath, p.args...)
	cmd.Dir = p.workingDir

	// 注入环境
	cmd.Env = append(os.Environ(), p.env...)

	// 配置操作系统级进程树隔离 (Windows JobObject / Unix Process Group)
	configureSysProcAttr(cmd)

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return fmt.Errorf("stdout pipe failed: %w", err)
	}

	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return fmt.Errorf("stderr pipe failed: %w", err)
	}

	if err := cmd.Start(); err != nil {
		cancel()
		return fmt.Errorf("start process failed: %w", err)
	}

	p.cmd = cmd
	p.pid = cmd.Process.Pid
	p.alive = true
	p.exitChan = make(chan ProcessExitEvent, 1)

	// 异步读取 stdout & stderr 送入 RingBuffer
	go p.drainStream(stdoutPipe, p.stdoutBuf)
	go p.drainStream(stderrPipe, p.stderrBuf)

	// 监控进程退出
	go p.waitLoop(cmd)

	return nil
}

func (p *ManagedProcess) drainStream(r io.Reader, ring *RingBuffer) {
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			_, _ = ring.Write(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

func (p *ManagedProcess) waitLoop(cmd *exec.Cmd) {
	err := cmd.Wait()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
	}

	p.mu.Lock()
	p.alive = false
	pid := p.pid
	exitChan := p.exitChan
	p.mu.Unlock()

	select {
	case exitChan <- ProcessExitEvent{
		ProcessID: pid,
		ExitCode:  exitCode,
		Error:     err,
		ExitTime:  time.Now(),
	}:
	default:
	}
}

// Kill 递归彻底清理子进程及由其衍生的全部子进程树（对齐45章验收标准与I10不变量）
func (p *ManagedProcess) Kill() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.alive || p.cmd == nil || p.cmd.Process == nil {
		return nil
	}

	if p.cancel != nil {
		p.cancel()
	}

	pid := p.cmd.Process.Pid
	// 平台特定的进程树终结实现
	err := killProcessTree(pid)
	p.alive = false
	return err
}

// IsAlive 返回当前是否存活
func (p *ManagedProcess) IsAlive() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.alive
}

// PID 获取进程 ID
func (p *ManagedProcess) PID() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.pid
}

// ExitChan 返回监听退出的 channel
func (p *ManagedProcess) ExitChan() <-chan ProcessExitEvent {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.exitChan
}

// StderrTail 获取最近收集的 stderr 日志切片
func (p *ManagedProcess) StderrTail() []byte {
	return p.stderrBuf.Bytes()
}

// StdoutTail 获取最近收集的 stdout 日志切片
func (p *ManagedProcess) StdoutTail() []byte {
	return p.stdoutBuf.Bytes()
}
