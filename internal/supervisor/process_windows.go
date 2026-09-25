//go:build windows

package supervisor

import (
	"fmt"
	"os/exec"
	"strconv"
	"syscall"
)

func configureSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP,
	}
}

func killProcessTree(pid int) error {
	if pid <= 0 {
		return nil
	}

	// Windows 下使用官方标准 taskkill /F /T /PID 递归强杀整棵进程树（包括所有子进程与孙子进程）
	killCmd := exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(pid))
	if err := killCmd.Run(); err != nil {
		// 回退至直接句柄终止
		h, errProc := syscall.OpenProcess(syscall.PROCESS_TERMINATE, false, uint32(pid))
		if errProc == nil {
			_ = syscall.TerminateProcess(h, 1)
			_ = syscall.CloseHandle(h)
			return nil
		}
		return fmt.Errorf("taskkill and terminate process %d failed: %w", pid, err)
	}
	return nil
}
