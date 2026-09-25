//go:build !windows

package supervisor

import (
	"fmt"
	"os/exec"
	"syscall"
)

func configureSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}
}

func killProcessTree(pid int) error {
	if pid <= 0 {
		return nil
	}

	// Unix 下终结整个 Process Group (-pid)
	err := syscall.Kill(-pid, syscall.SIGKILL)
	if err != nil {
		// 回退至直接 Kill 目标 PID
		errSingle := syscall.Kill(pid, syscall.SIGKILL)
		if errSingle != nil {
			return fmt.Errorf("kill process group -%d failed: %w, single kill: %v", pid, err, errSingle)
		}
	}
	return nil
}
