package worker

import "github.com/ximo888ok-netizen/ximo-agent/internal/worker/procguard"

// killProcessTree 强制清理一个 PID 及其全部后代进程。
//
// Manager 在 Worker 崩溃后调用它做兜底清理（第 13 章恢复链条的 "force cleanup" 步）。
// 具体机制由 procguard 按平台实现：
//   - Windows：Assignment 进 Job Object 后 TerminateJobObject + Toolhelp32 后代枚举；
//   - Unix：kill(-pgid, SIGKILL) 进程组 + /proc 后代枚举。
//
// 这里刻意做成"尽力而为、永不 panic"的语义：清理路径上的失败不应再引发新的故障。
func killProcessTree(pid int) {
	if pid <= 0 {
		return
	}
	func() {
		defer func() { _ = recover() }()
		_ = procguard.KillTree(pid)
	}()
}

// ProcessTreeCapabilities 暴露当前平台的进程树管控能力，
// 供任务 07 的可观测性报告与 08 号的验收核对使用（避免文档承诺超出实现）。
func ProcessTreeCapabilities() procguard.Capabilities {
	return procguard.CapabilitiesOf()
}
