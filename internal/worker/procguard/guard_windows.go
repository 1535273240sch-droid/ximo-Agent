//go:build windows

package procguard

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"unsafe"
)

// ============================== Windows 平台实现 ==============================
//
// 关键机制：Job Object + JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE。
//
// 为什么这是正解：Windows 没有进程组信号，taskkill /T 是"按父子关系递归枚举"的
// 用户态实现，既慢又有竞态（v1 就是 fire-and-forget 调用它）。Job Object 是
// 内核级容器：进程一旦 assign 进 job，它的所有后代自动属于同一个 job；
// 设置 KILL_ON_JOB_CLOSE 后，**只要 job 句柄被关闭，整族进程立刻被内核杀死**。
// 这样即使主进程被 taskkill /F 强杀、甚至宿主崩溃，也不会留下孤儿进程。
//
// 全部通过 kernel32/user32 lazydll 调用，不引入任何第三方依赖。

var (
	kernel32                     = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObjectW         = kernel32.NewProc("CreateJobObjectW")
	procAssignProcessToJobObject = kernel32.NewProc("AssignProcessToJobObject")
	procSetInformationJobObject  = kernel32.NewProc("SetInformationJobObject")
	procTerminateJobObject       = kernel32.NewProc("TerminateJobObject")
	procCreateToolhelp32Snapshot = kernel32.NewProc("CreateToolhelp32Snapshot")
	procProcess32FirstW          = kernel32.NewProc("Process32FirstW")
	procProcess32NextW           = kernel32.NewProc("Process32NextW")
	procOpenProcess              = kernel32.NewProc("OpenProcess")
	procTerminateProcess         = kernel32.NewProc("TerminateProcess")
	procCloseHandle              = kernel32.NewProc("CloseHandle")
	procGetExitCodeProcess       = kernel32.NewProc("GetExitCodeProcess")
)

const (
	jobObjectLimitKillOnJobClose  = 0x2000
	jobObjectLimitActiveProcess   = 0x00000008
	jobObjectLimitProcessMemory   = 0x00000100
	jobObjectExtendedLimitInfo    = 9
	jobObjectBasicLimitInfo       = 2
	jobObjectExtendedLimitInfoCls = 9

	processTerminate = 0x0001
	processQueryInfo = 0x0400

	stillActive = 259
	invalidPID  = 0

	th32csSnapProcess = 0x00000002
)

type jobHandle uintptr

// 与 Windows API 对齐的结构体定义（字段顺序/类型必须精确，否则内核解析会错位）。
type ioCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

type jobObjectBasicLimitInformation struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type jobObjectExtendedLimitInformation struct {
	BasicLimitInformation jobObjectBasicLimitInformation
	IoInfo                ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

type processEntry32 struct {
	Size            uint32
	CntUsage        uint32
	ProcessID       uint32
	DefaultHeapID   uintptr
	ModuleID        uint32
	CntThreads      uint32
	ParentProcessID uint32
	PriClassBase    int32
	Flags           uint32
	ExeFile         [260]uint16
}

// preparePlatform 创建 Job Object 并设置限制。在 cmd.Start() 之前调用。
func (p *Proc) preparePlatform(cfg *Config) error {
	h, _, err := procCreateJobObjectW.Call(0, 0)
	if h == 0 {
		return fmt.Errorf("CreateJobObject 失败: %w", err)
	}
	job := jobHandle(h)

	var info jobObjectExtendedLimitInformation
	info.BasicLimitInformation.LimitFlags = jobObjectLimitKillOnJobClose
	if cfg.MemoryLimitBytes > 0 {
		info.BasicLimitInformation.LimitFlags |= jobObjectLimitProcessMemory
		info.ProcessMemoryLimit = uintptr(cfg.MemoryLimitBytes)
	}
	if cfg.MaxProcesses > 0 {
		info.BasicLimitInformation.LimitFlags |= jobObjectLimitActiveProcess
		info.BasicLimitInformation.ActiveProcessLimit = uint32(cfg.MaxProcesses)
	}
	r, _, err := procSetInformationJobObject.Call(
		uintptr(job),
		uintptr(jobObjectExtendedLimitInfoCls),
		uintptr(unsafe.Pointer(&info)),
		unsafe.Sizeof(info),
	)
	if r == 0 {
		procCloseHandle.Call(uintptr(job))
		return fmt.Errorf("SetInformationJobObject 失败: %w", err)
	}
	p.job = job
	return nil
}

// attachPlatform 把刚启动的子进程 assign 进 Job Object。在 cmd.Start() 之后立即调用。
func (p *Proc) attachPlatform(pid int) error {
	if p.job == 0 {
		return errors.New("job object 未创建")
	}
	// 用 PROCESS_SET_QUOTA|PROCESS_TERMINATE 打开进程句柄。
	const processSetQuota = 0x0100
	h, _, err := procOpenProcess.Call(
		uintptr(processSetQuota|processTerminate),
		0,
		uintptr(pid),
	)
	if h == 0 {
		return fmt.Errorf("OpenProcess(%d) 失败: %w", pid, err)
	}
	defer procCloseHandle.Call(h)

	r, _, err := procAssignProcessToJobObject.Call(uintptr(p.job), h)
	if r == 0 {
		// 若当前进程自身已在某个"禁止嵌套"的 job 里（例如某些 CI/容器环境），
		// assign 会失败。此时退回后代枚举清扫模式，并如实上报能力降级。
		return fmt.Errorf("AssignProcessToJobObject 失败: %w", err)
	}
	return nil
}

// releasePlatform 关闭 Job Object 句柄。KILL_ON_JOB_CLOSE 保证残余后代被杀。
func (p *Proc) releasePlatform() error {
	if p.job != 0 {
		procCloseHandle.Call(uintptr(p.job))
		p.job = 0
	}
	return nil
}

// terminateJob 显式终止 job 内所有进程（比关句柄更直接，用于清扫路径）。
func (p *Proc) terminateJob() {
	if p.job != 0 {
		procTerminateJobObject.Call(uintptr(p.job), 1)
	}
}

// applySysProcAttr 设置 Windows 的进程创建属性。
func applySysProcAttr(cmd *exec.Cmd, cfg Config) {
	attr := &syscall.SysProcAttr{}
	if cfg.HideWindow || cfg.CreateNoWindow {
		attr.HideWindow = true
		attr.CreationFlags |= 0x08000000 // CREATE_NO_WINDOW
	}
	// CREATE_NEW_PROCESS_GROUP：让 Ctrl+C 等控制事件不串到父进程。
	attr.CreationFlags |= 0x00000200
	cmd.SysProcAttr = attr
}

// requestGracefulExit 在 Windows 上没有可靠的"整树友好退出"手段（无进程组信号），
// 因此这里只尝试向主进程发送 CTRL_BREAK 的等价物——实际上直接进入强杀阶段，
// 由 KillGrace 的等待窗口给主进程自然退出的机会（不做任何事即等待）。
func (p *Proc) requestGracefulExit() {
	// 有意留空：Windows 下"让主进程自己退出"的时间窗由 killTree 的 grace 计时器提供。
	// 不做 GenerateConsoleCtrlEvent 是因为它对无控制台的子进程无效，反而会误伤宿主。
}

// forceKillTree 强杀整棵进程树。
//
// 顺序：先 TerminateJobObject（一次调用杀掉全部成员），再枚举后代逐个兜底
// （覆盖 assign 之前的极短竞态窗口里 fork 出来的进程）。
func (p *Proc) forceKillTree() {
	if p.job != 0 {
		procTerminateJobObject.Call(uintptr(p.job), 1)
	}
	pid := p.PID()
	if pid <= 0 {
		return
	}
	// 后序遍历：先杀叶子，再杀父，避免"父还在时又生出新子"。
	for _, d := range Descendants(pid) {
		killSingle(d)
	}
	killSingle(pid)
}

// killTreeByPID 是不持有 *Proc 时的兜底清理入口。
func killTreeByPID(pid int) error {
	for _, d := range Descendants(pid) {
		killSingle(d)
	}
	killSingle(pid)
	return nil
}

// killSingle 强制杀死单个 PID。
func killSingle(pid int) {
	if pid <= 0 {
		return
	}
	h, _, _ := procOpenProcess.Call(uintptr(processTerminate), 0, uintptr(pid))
	if h == 0 {
		return
	}
	defer procCloseHandle.Call(h)
	procTerminateProcess.Call(h, 1)
}

// processAlive 判断 PID 是否存活（GetExitCodeProcess == STILL_ACTIVE）。
func processAlive(pid int) bool {
	h, _, _ := procOpenProcess.Call(uintptr(processQueryInfo), 0, uintptr(pid))
	if h == 0 {
		return false
	}
	defer procCloseHandle.Call(h)
	var code uint32
	r, _, _ := procGetExitCodeProcess.Call(h, uintptr(unsafe.Pointer(&code)))
	if r == 0 {
		return false
	}
	return code == stillActive
}

// Descendants 用 CreateToolhelp32Snapshot 枚举 pid 的**全部后代** PID，
// 返回顺序为"叶子优先"（深度大的在前），便于安全地逐层清理。
//
// 为什么需要它：Job Object 的 assign 存在极小竞态窗口；另外有些环境的 job 嵌套
// 会失败。有了后代枚举，即使 job 不可用也能保证进程树被清理（红线要求）。
func Descendants(pid int) []int {
	if pid <= 0 {
		return nil
	}
	// 先建立 pid -> children 的邻接表。
	children := make(map[uint32][]uint32)
	parent := make(map[uint32]uint32)

	h, _, _ := procCreateToolhelp32Snapshot.Call(uintptr(th32csSnapProcess), 0)
	if h == 0 || h == uintptr(syscall.InvalidHandle) {
		return []int{pid} // 拿不到快照时至少把主进程算进来，由调用方杀它
	}
	defer procCloseHandle.Call(h)

	var entry processEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	r, _, _ := procProcess32FirstW.Call(h, uintptr(unsafe.Pointer(&entry)))
	for r != 0 {
		ppid := entry.ParentProcessID
		cpid := entry.ProcessID
		children[ppid] = append(children[ppid], cpid)
		parent[cpid] = ppid
		entry.Size = uint32(unsafe.Sizeof(entry))
		r, _, _ = procProcess32NextW.Call(h, uintptr(unsafe.Pointer(&entry)))
	}

	// BFS 收集后代。
	var out []int
	queue := []uint32{uint32(pid)}
	seen := map[uint32]bool{uint32(pid): true}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, c := range children[cur] {
			if seen[c] {
				continue
			}
			seen[c] = true
			out = append(out, int(c))
			queue = append(queue, c)
		}
	}
	// 反转成"叶子优先"，避免先杀父导致子进程被 reparent 后更难枚举。
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// Capabilities 报告本平台能力。
func CapabilitiesOf() Capabilities {
	return Capabilities{
		ProcessTreeKill:  true,
		JobObject:        true,
		MemoryLimit:      true, // Job Object ProcessMemoryLimit
		ProcessCountLim:  true, // Job Object ActiveProcessLimit
		ProcessGroupKill: false,
		CPULimit:         false, // 需 cgroup/Job CPU rate control，未实现
		DescendantSweep:  true,  // CreateToolhelp32Snapshot
	}
}

// killWithContext 便捷封装：带 ctx 的强杀（供测试与外部调用）。
func killWithContext(ctx context.Context, pid int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	killTreeByPID(pid)
	return nil
}
