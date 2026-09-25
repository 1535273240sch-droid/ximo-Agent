//go:build unix

package procguard

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ============================== Unix 平台实现 ==============================
//
// 关键机制：独立进程组 + 进程组级信号。
//
// v1 的致命缺陷：只对直接子进程（/bin/bash）发 SIGTERM，且没有 Setpgid，
// 因此 `npm run dev`、`python server.py` 这类命令的子孙会被 init 收养并永久泄漏。
//
// v2 的做法：
//   - Start 时 Setpgid=true，子进程成为新进程组的组长（pgid == 子进程 pid）；
//   - 所有后代（除非自己另开进程组）都属于该组，因此 kill(-pgid, SIGKILL) 一击全灭；
//   - 额外用 /proc（Linux）或 ps（其他 Unix）枚举后代做兜底，覆盖"后代自己 setsid"的情况。

// preparePlatform 在 Unix 上无需预分配资源（进程组由 SysProcAttr 设置）。
func (p *Proc) preparePlatform(*Config) error { return nil }

// attachPlatform 校验子进程确实处于自己的进程组中。
func (p *Proc) attachPlatform(pid int) error {
	got, err := syscall.Getpgid(pid)
	if err != nil {
		return err
	}
	if got != pid {
		// 未成为组长说明平台行为异常；仍可通过后代枚举兜底，但要留痕。
		return fmt.Errorf("进程组异常: pgid=%d pid=%d", got, pid)
	}
	p.pgid = pid
	return nil
}

// releasePlatform 在 Unix 上无句柄需要释放。幂等。
func (p *Proc) releasePlatform() error { return nil }

// terminateJob 在 Unix 上无 Job Object（cgroup 未实现，如实降级）。
func (p *Proc) terminateJob() {}

// applySysProcAttr 设置独立进程组。这是整个 Unix 侧进程树清理的基础。
func applySysProcAttr(cmd *exec.Cmd, _ Config) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true, // 子进程自成进程组，pgid == pid
	}
}

// requestGracefulExit 向整个进程组发 SIGTERM，给子进程清理现场的机会。
func (p *Proc) requestGracefulExit() {
	pid := p.PID()
	if pid <= 0 {
		return
	}
	// 负 PID 表示"进程组"。
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	_ = syscall.Kill(pid, syscall.SIGTERM)
}

// forceKillTree 向进程组发 SIGKILL，再逐个后代兜底。
func (p *Proc) forceKillTree() {
	pid := p.PID()
	if pid <= 0 {
		return
	}
	// 核心一击：杀死整个进程组。
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		_ = err
	}
	// 兜底：处理自行 setsid 脱离进程组的后代。
	for _, d := range Descendants(pid) {
		killSingle(d)
	}
	killSingle(pid)
}

// killTreeByPID 是不持有 *Proc 时的兜底清理入口。
func killTreeByPID(pid int) error {
	if pid <= 0 {
		return nil
	}
	// 先按进程组杀（覆盖 pid 恰好是组长的常见情况）。
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	for _, d := range Descendants(pid) {
		killSingle(d)
	}
	killSingle(pid)
	return nil
}

// killSingle 向单个 PID 发 SIGKILL。
func killSingle(pid int) {
	if pid <= 0 {
		return
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
}

// processAlive 用 signal 0 探测进程是否存在。
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// Descendants 返回 pid 的全部后代，叶子优先。
//
// Linux 优先读 /proc（准确且无外部依赖）；其他 Unix 回退到 ps -eo pid,ppid。
func Descendants(pid int) []int {
	if pid <= 0 {
		return nil
	}
	children, ok := readProcChildren()
	if !ok {
		children, ok = readPSChildren()
		if !ok {
			return []int{pid}
		}
	}
	var out []int
	queue := []int{pid}
	seen := map[int]bool{pid: true}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, c := range children[cur] {
			if seen[c] {
				continue
			}
			seen[c] = true
			out = append(out, c)
			queue = append(queue, c)
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// readProcChildren 解析 /proc/*/stat 构建父子关系（Linux）。
func readProcChildren() (map[int][]int, bool) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, false
	}
	children := make(map[int][]int, len(entries))
	found := false
	for _, e := range entries {
		name := e.Name()
		pid, err := strconv.Atoi(name)
		if err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/proc", name, "stat"))
		if err != nil {
			continue
		}
		// stat 格式: pid (comm) state ppid ...
		// comm 可能含空格与括号，因此从最后一个 ')' 之后开始切分。
		s := string(data)
		idx := strings.LastIndexByte(s, ')')
		if idx < 0 || idx+2 >= len(s) {
			continue
		}
		fields := strings.Fields(s[idx+2:])
		if len(fields) < 2 {
			continue
		}
		ppid, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		children[ppid] = append(children[ppid], pid)
		found = true
	}
	return children, found
}

// readPSChildren 用 ps 构建父子关系（非 Linux 的 Unix）。
func readPSChildren() (map[int][]int, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ps", "-eo", "pid=,ppid=")
	out, err := cmd.Output()
	if err != nil {
		return nil, false
	}
	children := make(map[int][]int)
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		pid, err1 := strconv.Atoi(fields[0])
		ppid, err2 := strconv.Atoi(fields[1])
		if err1 != nil || err2 != nil {
			continue
		}
		children[ppid] = append(children[ppid], pid)
	}
	return children, true
}

// Capabilities 报告本平台能力。
func CapabilitiesOf() Capabilities {
	return Capabilities{
		ProcessTreeKill:  true,
		JobObject:        false,
		MemoryLimit:      false, // 需 cgroup v2，未实现（如实上报，不谎报）
		ProcessCountLim:  false, // 同上
		ProcessGroupKill: true,  // Setpgid + kill(-pgid)
		CPULimit:         false,
		DescendantSweep:  true, // /proc 或 ps
	}
}

// killWithContext 便捷封装：带 ctx 的强杀。
func killWithContext(ctx context.Context, pid int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	killTreeByPID(pid)
	return nil
}
