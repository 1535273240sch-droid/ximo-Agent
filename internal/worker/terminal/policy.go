// Package terminal 实现 TerminalWorker：终端命令执行的独立故障域。
//
// 对应任务书第 15 章（TerminalPolicy）与 v1 的 src/main/tools/Terminal/。
//
// v1 审计结论（本包存在的直接理由，逐条对照 v1 缺陷）：
//
//	缺陷 1  全量继承 process.env，导致 API Key 泄漏给任意被调命令
//	        → 本包默认 EnvironmentPolicy=Minimal，需要显式配置才继承。
//	缺陷 2  Unix 上只 SIGTERM 直接子进程且未设独立进程组，子孙进程永久泄漏
//	        → 本包统一经 procguard：Windows Job Object / Unix 进程组 + 后代枚举。
//	缺陷 3  输出先无上限累积、退出后才截断，一条失控日志能打爆宿主
//	        → 本包用 procguard 的 limitBuffer 在写入路径就设上界。
//	缺陷 4  没有任何并发/进程数上限，100 个调用就起 100 个 powershell
//	        → 本包用信号量做 MaxProcesses 准入控制。
//	缺陷 5  cwd 完全不校验，可传 C:\Windows\System32
//	        → 本包用 AllowedRoots + EvalSymlinks 解析真实路径（防 symlink 越权）。
//	缺陷 6  白名单把 powershell/pwsh 自身列入，且只校验首词
//	        → 本包默认拒绝 shell 解释器作为被调命令，且对整条命令做拒绝规则匹配。
//	缺陷 7  整条命令字符串拼进 -Command，无注入防护
//	        → 本包默认 argv 直传（不经 shell），shell 模式须显式开启并默认拒绝
//	          命令链接符（; && || | > < 等）。
package terminal

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/worker"
	"github.com/ximo888ok-netizen/ximo-agent/internal/worker/procguard"
)

// ============================== TerminalPolicy（任务书原文结构，字段名不可改） ==============================

// NetworkMode 控制命令的网络访问能力。
//
// 实现说明（如实记录，不谎报）：OS 级网络隔离需要防火墙/网络命名空间/cgroup，
// 本实现提供的是**命令级 + 环境级**强制：
//   - NetworkDeny：拒绝一切已知网络客户端命令（curl/wget/ping/nc/ssh/...），
//     并清除/污染代理类环境变量，使常见 HTTP 客户端无法直连。
//   - NetworkLoopbackOnly：在 Deny 基础上放行 loopback 目标（不拦截 DNS）。
//
// 这不是内核级隔离。任务 07/08 若要更强保证，应在部署层叠加防火墙规则。
type NetworkMode string

const (
	NetworkAllow        NetworkMode = "allow"
	NetworkLoopbackOnly NetworkMode = "loopback"
	NetworkDeny         NetworkMode = "deny"
)

// EnvPolicy 控制子进程的环境变量来源（第 21 章红线：密钥不得外泄）。
type EnvPolicy string

const (
	// EnvMinimal 只注入系统运行必需 + UTF-8 相关变量。**默认值**。
	EnvMinimal EnvPolicy = "minimal"
	// EnvAllowlist 只注入 Allowlist 中列出的键。
	EnvAllowlist EnvPolicy = "allowlist"
	// EnvInherit 全量继承父进程环境。**危险**：会把 API Key 带进子进程，
	// 仅当调用方明确知情时使用；无论如何都会剔除 DenyKeys。
	EnvInherit EnvPolicy = "inherit"
)

// TerminalPolicy 是终端执行的完整安全边界（任务书给定结构，逐字保留）。
type TerminalPolicy struct {
	AllowedRoots      []string
	DeniedCommands    []string
	MaxRuntime        time.Duration
	MaxOutputBytes    int64
	MaxProcesses      int
	NetworkMode       NetworkMode
	EnvironmentPolicy EnvPolicy

	// ---- 以下为 v2 新增的加固项（v1 完全没有，属"必须实现不能跳过"的红线范围） ----

	// AllowedCommands 非空时启用**白名单模式**：只允许执行列表中的可执行文件名。
	// 这是比 DeniedCommands（黑名单）强得多的默认姿态。
	AllowedCommands []string
	// AllowShell 为 true 时才允许 ShellMode（把命令交给 shell 解释）。
	// 默认 false：默认走 argv 直传，从根上消除 shell 注入面。
	AllowShell bool
	// AllowCommandChaining 为 true 时才允许 shell 模式下的命令链接符（; && || | 等）。
	// 默认 false。即使开启 shell，也默认拒绝链接符——这正是 v1 "首词合法即放行"的补丁。
	AllowCommandChaining bool
	// EnvAllowlist 在 EnvPolicy==EnvAllowlist 时生效。
	EnvAllowlist []string
	// EnvDenyKeys 是无论何种策略都被剔除的键（默认包含 *KEY*/*TOKEN*/*SECRET*/*PASSWORD*）。
	EnvDenyKeys []string
	// MaxArgs 限制参数个数（防参数爆炸）。<=0 时用 256。
	MaxArgs int
	// MaxCommandBytes 限制命令字符串长度。<=0 时用 32KiB。
	MaxCommandBytes int
	// ResolveSymlinks 为 true（默认）时对工作目录做 EvalSymlinks 防 symlink 越权。
	ResolveSymlinks *bool
	// DenyAbsolutePathsOutsideRoots 为 true（默认）时，argv 中出现的绝对路径必须落在
	// AllowedRoots 内，否则拒绝。这是"用绝对路径绕过 cwd 限制"的补丁。
	DenyAbsolutePathsOutsideRoots *bool
	// GracefulKillAfter 是强杀前的友好等待时长。
	GracefulKillAfter time.Duration
}

// DefaultPolicy 返回安全默认值。注意 AllowedRoots 为空时**拒绝一切执行**
// （fail-closed：宁可拒绝，不可放行）。
func DefaultPolicy() TerminalPolicy {
	t := true
	return TerminalPolicy{
		AllowedRoots:                  nil,
		DeniedCommands:                DefaultDeniedCommands(),
		MaxRuntime:                    60 * time.Second,
		MaxOutputBytes:                1 << 20, // 1 MiB / 流
		MaxProcesses:                  4,
		NetworkMode:                   NetworkAllow,
		EnvironmentPolicy:             EnvMinimal,
		AllowedCommands:               nil,
		AllowShell:                    false,
		AllowCommandChaining:          false,
		EnvAllowlist:                  nil,
		EnvDenyKeys:                   DefaultEnvDenyKeys(),
		MaxArgs:                       256,
		MaxCommandBytes:               32 << 10,
		ResolveSymlinks:               &t,
		DenyAbsolutePathsOutsideRoots: &t,
		GracefulKillAfter:             3 * time.Second,
	}
}

// DefaultEnvDenyKeys 是默认剔除的环境变量键模式（大小写不敏感的子串/通配）。
// 覆盖第 21 章列举的密钥形态。
func DefaultEnvDenyKeys() []string {
	return []string{
		"*KEY*", "*TOKEN*", "*SECRET*", "*PASSWORD*", "*PASSWD*", "*CREDENTIAL*",
		"*APIKEY*", "*API_KEY*", "*ACCESS_KEY*", "*PRIVATE_KEY*", "*SESSION*",
		"AWS_*", "AZURE_*", "GCP_*", "OPENAI_*", "ANTHROPIC_*", "DEEPSEEK_*",
		"GITHUB_TOKEN", "GH_TOKEN", "NPM_TOKEN", "DOCKER_AUTH*",
	}
}

// DefaultDeniedCommands 是默认拒绝的可执行文件名（不区分大小写，含扩展名与否都匹配）。
//
// 与 v1 的黑名单相比，这里做三件事：
//  1. 把 python/node/curl 之类的"通用解释器/下载器"交给白名单模式去管，不在黑名单里重复；
//  2. 补齐 v1 漏掉的 Windows 高危项（bcdedit、vssadmin、wbadmin、cipher、takeown 等）；
//  3. **明确拒绝 shell 解释器本身**（cmd/powershell/pwsh/bash/sh 等）——
//     这是 v1 最大的洞：白名单里有 powershell，等于白名单形同虚设。
//     若确实需要 shell，必须显式设置 AllowShell=true 并单独放开该命令。
func DefaultDeniedCommands() []string {
	return []string{
		// 破坏性文件操作
		"rm", "rmdir", "del", "erase", "format", "mkfs", "fdisk", "diskpart", "shred",
		// 磁盘/引导
		"dd", "bcdedit", "bootrec", "vssadmin", "wbadmin", "cipher", "diskpart",
		// 权限/所有者操纵
		"takeown", "icacls", "cacls", "attrib", "chown", "chmod", "chgrp", "setfacl",
		// 系统/账户管理
		"shutdown", "reboot", "halt", "poweroff", "reg", "regedit", "net", "netsh",
		"sc", "schtasks", "at", "wmic", "runas", "sudo", "su", "doas",
		"useradd", "userdel", "usermod", "passwd", "groupadd", "groupdel",
		// 进程/服务杀伤
		"taskkill", "tskill", "kill", "killall", "pkill", "killall5",
		"systemctl", "service", "launchctl",
		// 远程/横向
		"ssh", "scp", "sftp", "telnet", "rdesktop", "mstsc", "winrs", "psexec",
		// shell 解释器（必须显式 AllowShell 才能用）
		"cmd", "cmd.exe", "powershell", "powershell.exe", "pwsh", "pwsh.exe",
		"bash", "sh", "zsh", "ksh", "csh", "fish", "wsl", "cscript", "wscript",
		"mshta", "rundll32", "regsvr32",
	}
}

// networkClients 是 NetworkMode 拒绝模式下要拦的网络客户端。
var networkClients = []string{
	"curl", "wget", "nc", "ncat", "netcat", "telnet", "ssh", "scp", "sftp",
	"ftp", "tftp", "ping", "tracert", "traceroute", "nslookup", "dig", "host",
	"nmap", "masscan", "socat", "openssl", "aria2c", "httpie", "http", "ab", "wrk",
	"iwr", "irm", "invoke-webrequest", "invoke-restmethod",
}

// shellMetachars 是 shell 模式下默认拒绝的命令链接/重定向符。
var shellMetachars = []struct {
	tok string
	re  *regexp.Regexp
}{
	{";", regexp.MustCompile(`;`)},
	{"&&", regexp.MustCompile(`&&`)},
	{"||", regexp.MustCompile(`\|\|`)},
	{"|", regexp.MustCompile(`\|`)},
	{">", regexp.MustCompile(`>`)},
	{"<", regexp.MustCompile(`<`)},
	{"`", regexp.MustCompile("`")},
	{"$(", regexp.MustCompile(`\$\(`)},
	{"${", regexp.MustCompile(`\$\{`)},
	{"&", regexp.MustCompile(`&`)},
}

// deniedPatterns 是**内容级**拒绝规则：即使首词合法，命令里出现这些模式也拒绝。
// 这是对 v1 "只校验首词，`echo a; rm -rf /x` 直接放行" 那个漏洞的正面修补。
var deniedPatterns = []struct {
	name string
	re   *regexp.Regexp
}{
	{"fork 炸弹", regexp.MustCompile(`:\s*\(\s*\)\s*\{`)},
	{"递归删除根", regexp.MustCompile(`(?i)\brm\s+(-[a-z]*\s+)*-?[a-z]*r[a-z]*f?[a-z]*\s+(/|~|\*)`)},
	{"递归删除家目录", regexp.MustCompile(`(?i)\brm\s+-rf\s+(~|\$HOME|/)`)},
	{"Windows 递归强删", regexp.MustCompile(`(?i)\b(rd|rmdir|del)\s+/s\s+/q`)},
	{"diskpart 脚本", regexp.MustCompile(`(?i)\bdiskpart\b`)},
	{"下载即执行管道", regexp.MustCompile(`(?i)\b(curl|wget)\b[^|]*\|\s*(ba|z|k)?sh\b`)},
	{"PowerShell 下载即执行", regexp.MustCompile(`(?i)\b(iwr|irm|invoke-webrequest|invoke-restmethod)\b[^|]*\|\s*(iex|invoke-expression)\b`)},
	{"base64 解码即执行", regexp.MustCompile(`(?i)\bbase64\s+-d\b[^|]*\|\s*(ba|z|k)?sh\b`)},
	{"history 篡改", regexp.MustCompile(`(?i)\bhistory\s+-c\b`)},
	{"覆盖磁盘设备", regexp.MustCompile(`>\s*/dev/(sd|hd|nvme|vd)`)},

	{"sudo 提权", regexp.MustCompile(`(?i)\bsudo\s`)},
	{"Windows 计划任务", regexp.MustCompile(`(?i)\bschtasks\b`)},
	{"注册表写入", regexp.MustCompile(`(?i)\breg\s+(add|delete|import|load)\b`)},
	{"账户操作", regexp.MustCompile(`(?i)\bnet\s+(user|localgroup)\b`)},
	{"强制推送", regexp.MustCompile(`(?i)\bgit\s+push\b[^\n]*(--force\b|\s-f\b)`)},
	{"硬重置", regexp.MustCompile(`(?i)\bgit\s+reset\s+--hard\b`)},
	{"git 清空未跟踪", regexp.MustCompile(`(?i)\bgit\s+clean\s+-[a-z]*f`)},
}

// PolicyError 是策略拒绝的详细原因。UI 需要能解释"为什么被拒绝、是哪条规则"（第 20 章）。
type PolicyError struct {
	Rule   string `json:"rule"`
	Reason string `json:"reason"`
	Detail string `json:"detail,omitempty"`
}

func (e *PolicyError) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("策略拒绝[%s]: %s (%s)", e.Rule, e.Reason, e.Detail)
	}
	return fmt.Sprintf("策略拒绝[%s]: %s", e.Rule, e.Reason)
}

// Unwrap 让 errors.Is(err, worker.ErrPolicyDenied) 成立。
func (e *PolicyError) Unwrap() error { return worker.ErrPolicyDenied }

// ============================== 策略校验 ==============================

// Validator 是策略校验器。它把 TerminalPolicy 变成可执行的判定，
// 并缓存已解析的 AllowedRoots 真实路径。
type Validator struct {
	policy      TerminalPolicy
	roots       []string // EvalSymlinks 之后的真实路径
	deniedExact map[string]bool
	deniedPat   []*regexp.Regexp
	allowedEx   map[string]bool
	netClients  map[string]bool
	envDeny     []*regexp.Regexp
	envAllow    map[string]bool
}

// NewValidator 编译策略。AllowedRoots 会被解析为真实路径（防 symlink 越权）。
func NewValidator(p TerminalPolicy) (*Validator, error) {
	if p.MaxRuntime <= 0 {
		p.MaxRuntime = 60 * time.Second
	}
	if p.MaxOutputBytes <= 0 {
		p.MaxOutputBytes = 1 << 20
	}
	if p.MaxProcesses <= 0 {
		p.MaxProcesses = 4
	}
	if p.MaxArgs <= 0 {
		p.MaxArgs = 256
	}
	if p.MaxCommandBytes <= 0 {
		p.MaxCommandBytes = 32 << 10
	}
	if p.GracefulKillAfter <= 0 {
		p.GracefulKillAfter = 3 * time.Second
	}
	switch p.NetworkMode {
	case NetworkAllow, NetworkLoopbackOnly, NetworkDeny:
	case "":
		p.NetworkMode = NetworkAllow
	default:
		return nil, fmt.Errorf("%w: 未知 NetworkMode=%q", worker.ErrInvalidArgument, p.NetworkMode)
	}
	switch p.EnvironmentPolicy {
	case EnvMinimal, EnvAllowlist, EnvInherit:
	case "":
		p.EnvironmentPolicy = EnvMinimal
	default:
		return nil, fmt.Errorf("%w: 未知 EnvironmentPolicy=%q", worker.ErrInvalidArgument, p.EnvironmentPolicy)
	}
	if len(p.EnvDenyKeys) == 0 {
		p.EnvDenyKeys = DefaultEnvDenyKeys()
	}
	if len(p.DeniedCommands) == 0 {
		p.DeniedCommands = DefaultDeniedCommands()
	}

	v := &Validator{
		policy:      p,
		deniedExact: make(map[string]bool, len(p.DeniedCommands)),
		allowedEx:   make(map[string]bool, len(p.AllowedCommands)),
		netClients:  make(map[string]bool, len(networkClients)),
		envAllow:    make(map[string]bool, len(p.EnvAllowlist)),
	}
	for _, c := range p.DeniedCommands {
		v.deniedExact[normalizeExe(c)] = true
	}
	for _, c := range p.AllowedCommands {
		v.allowedEx[normalizeExe(c)] = true
	}
	for _, c := range networkClients {
		v.netClients[normalizeExe(c)] = true
	}
	for _, k := range p.EnvAllowlist {
		v.envAllow[strings.ToUpper(k)] = true
	}
	for _, g := range p.EnvDenyKeys {
		re, err := globToRegexp(g)
		if err != nil {
			return nil, fmt.Errorf("%w: EnvDenyKeys 模式 %q 非法: %v", worker.ErrInvalidArgument, g, err)
		}
		v.envDeny = append(v.envDeny, re)
	}
	// AllowedRoots 转成真实路径。解析失败的 root 直接报错（fail-closed）。
	resolve := p.ResolveSymlinks == nil || *p.ResolveSymlinks
	for _, r := range p.AllowedRoots {
		abs, err := filepath.Abs(r)
		if err != nil {
			return nil, fmt.Errorf("%w: AllowedRoot 非法 %q: %v", worker.ErrInvalidArgument, r, err)
		}
		if resolve {
			if real, err := filepath.EvalSymlinks(abs); err == nil {
				abs = real
			} else if !errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("%w: 解析 AllowedRoot %q 失败: %v", worker.ErrInvalidArgument, r, err)
			}
		}
		v.roots = append(v.roots, filepath.Clean(abs))
	}
	return v, nil
}

// Policy 返回生效的策略副本（已填默认值）。
func (v *Validator) Policy() TerminalPolicy { return v.policy }

// Validate 校验一次终端调用。返回的 *PolicyError 会同时满足 errors.Is(.., ErrPolicyDenied)。
func (v *Validator) Validate(req Spec) error {
	if strings.TrimSpace(req.Command) == "" && len(req.Argv) == 0 {
		return &PolicyError{Rule: "empty_command", Reason: "命令不能为空"}
	}
	if len(req.Command) > v.policy.MaxCommandBytes {
		return &PolicyError{
			Rule:   "command_too_long",
			Reason: "命令字符串超过上限",
			Detail: fmt.Sprintf("%d > %d", len(req.Command), v.policy.MaxCommandBytes),
		}
	}
	if len(req.Argv) > v.policy.MaxArgs {
		return &PolicyError{
			Rule:   "too_many_args",
			Reason: "参数个数超过上限",
			Detail: fmt.Sprintf("%d > %d", len(req.Argv), v.policy.MaxArgs),
		}
	}

	// 1) 解析出将要执行的 argv（不解释 shell）。
	argv, err := v.resolveArgv(req)
	if err != nil {
		return err
	}
	if len(argv) == 0 {
		return &PolicyError{Rule: "empty_command", Reason: "命令解析后为空"}
	}

	// 2) 工作目录校验（AllowedRoots + symlink 解析）。
	if _, err := v.resolveWorkdir(req.Cwd); err != nil {
		return err
	}

	// 3) 可执行文件名校验：黑名单 → 白名单 → 网络客户端。
	exe := normalizeExe(argv[0])
	if v.deniedExact[exe] && !v.shellExplicitlyAllowed(exe, req) {
		return &PolicyError{
			Rule: "denied_command", Reason: "命令在拒绝列表中", Detail: argv[0],
		}
	}
	if len(v.allowedEx) > 0 && !v.allowedEx[exe] {
		return &PolicyError{
			Rule: "command_not_allowed", Reason: "命令不在白名单中", Detail: argv[0],
		}
	}
	if v.policy.NetworkMode == NetworkDeny || v.policy.NetworkMode == NetworkLoopbackOnly {
		if v.netClients[exe] {
			return &PolicyError{
				Rule: "network_denied", Reason: "NetworkMode 禁止使用网络客户端命令", Detail: argv[0],
			}
		}
	}

	// 4) 参数级校验：绝对路径必须落在 AllowedRoots 内；网络目标在 deny 模式下拒绝。
	for _, a := range argv[1:] {
		if err := v.validateArg(a); err != nil {
			return err
		}
	}

	// 5) 内容级拒绝规则（覆盖"首词合法但后续串联高危操作"）。
	//    无论 argv 还是 shell 模式都检查原始文本，这是 v1 漏洞的正面修补。
	raw := req.Command
	if raw == "" {
		raw = strings.Join(req.Argv, " ")
	}
	for _, dp := range deniedPatterns {
		if dp.re.MatchString(raw) {
			return &PolicyError{Rule: "denied_pattern", Reason: "命令内容命中拒绝规则", Detail: dp.name}
		}
	}

	// 6) Shell 模式额外校验：默认拒绝命令链接符与重定向。
	if req.Mode == ModeShell {
		if !v.policy.AllowShell {
			return &PolicyError{
				Rule: "shell_not_allowed", Reason: "策略未允许 shell 模式执行（AllowShell=false）",
			}
		}
		if !v.policy.AllowCommandChaining {
			for _, mc := range shellMetachars {
				if mc.re.MatchString(req.Command) {
					return &PolicyError{
						Rule:   "command_chaining_denied",
						Reason: "shell 模式下禁止命令链接/重定向符",
						Detail: mc.tok,
					}
				}
			}
		}
		// shell 模式下可执行文件是解释器本身，必须显式放行。
		if v.deniedExact[normalizeExe(req.Shell)] && !v.shellExplicitlyAllowed(normalizeExe(req.Shell), req) {
			return &PolicyError{
				Rule: "denied_shell", Reason: "shell 解释器在拒绝列表中且未显式放行", Detail: req.Shell,
			}
		}
	}
	return nil
}

// shellExplicitlyAllowed 判断"拒绝列表里的 shell 解释器"是否被显式放行。
// 同时要求 AllowShell=true，避免只改一处就绕过。
func (v *Validator) shellExplicitlyAllowed(exe string, req Spec) bool {
	if !v.policy.AllowShell {
		return false
	}
	if req.Mode != ModeShell {
		return false
	}
	return v.allowedEx[normalizeExe(exe)]
}

// validateArg 校验单个参数。
func (v *Validator) validateArg(a string) error {
	if a == "" {
		return nil
	}
	// 网络目标（URL / host:port）在 deny 模式下拒绝。
	if v.policy.NetworkMode == NetworkDeny {
		if looksLikeNetworkTarget(a) {
			return &PolicyError{Rule: "network_denied", Reason: "NetworkMode=deny 禁止网络目标", Detail: a}
		}
	}
	// 绝对路径越权检查（"用绝对路径绕过 cwd 限制"的补丁）。
	denyOutside := v.policy.DenyAbsolutePathsOutsideRoots == nil || *v.policy.DenyAbsolutePathsOutsideRoots
	if !denyOutside {
		return nil
	}
	if !looksLikePath(a) {
		return nil
	}
	// 去掉可能的前缀（如 git 的 --file=...）后再判断。
	cand := a
	if i := strings.IndexByte(a, '='); i > 0 && strings.HasPrefix(a, "-") {
		cand = a[i+1:]
	}
	if !filepath.IsAbs(cand) {
		// 相对路径：不在此处校验，因为它是相对于已校验过的 cwd。
		// 但它可能是 "../.." 逃逸，因此对 ".." 显式检查。
		if escapesRoot(cand) {
			return &PolicyError{
				Rule: "path_escape", Reason: "参数包含向上越界的相对路径", Detail: a,
			}
		}
		return nil
	}
	if !v.withinRoots(cand) {
		return &PolicyError{
			Rule: "path_outside_roots", Reason: "绝对路径不在 AllowedRoots 内", Detail: a,
		}
	}
	return nil
}

// resolveArgv 决定最终执行的 argv。
func (v *Validator) resolveArgv(req Spec) ([]string, error) {
	switch req.Mode {
	case ModeArgv, "":
		if len(req.Argv) > 0 {
			return append([]string(nil), req.Argv...), nil
		}
		// 提供了 command 字符串但走 argv 模式：按空白切分（**不是** shell 解释）。
		fields, err := SplitCommand(req.Command)
		if err != nil {
			return nil, err
		}
		if len(fields) == 0 {
			return nil, &PolicyError{Rule: "empty_command", Reason: "命令解析后为空"}
		}
		return fields, nil
	case ModeShell:
		shell := req.Shell
		if shell == "" {
			shell = defaultShell()
		}
		args := shellArgs(shell, req.Command)
		return append([]string{shell}, args...), nil
	default:
		return nil, &PolicyError{Rule: "bad_mode", Reason: "未知执行模式", Detail: string(req.Mode)}
	}
}

// resolveWorkdir 校验并返回工作目录真实路径。
func (v *Validator) resolveWorkdir(cwd string) (string, error) {
	if cwd == "" {
		cwd = "."
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return "", &PolicyError{Rule: "bad_cwd", Reason: "工作目录非法", Detail: err.Error()}
	}
	// 关键：EvalSymlinks 解析真实路径，防止用软链接指向 AllowedRoots 之外（v1 用 path.resolve，漏了这步）。
	resolve := v.policy.ResolveSymlinks == nil || *v.policy.ResolveSymlinks
	if resolve {
		if real, err := filepath.EvalSymlinks(abs); err == nil {
			abs = real
		} else if errors.Is(err, os.ErrNotExist) {
			return "", &PolicyError{Rule: "cwd_not_exist", Reason: "工作目录不存在", Detail: cwd}
		} else {
			return "", &PolicyError{Rule: "bad_cwd", Reason: "工作目录无法解析", Detail: err.Error()}
		}
	}
	abs = filepath.Clean(abs)
	if len(v.roots) == 0 {
		// fail-closed：没有配置 AllowedRoots 就拒绝一切执行。
		return "", &PolicyError{
			Rule: "no_allowed_roots", Reason: "策略未配置 AllowedRoots，拒绝执行（fail-closed）",
		}
	}
	if !v.withinRoots(abs) {
		return "", &PolicyError{
			Rule: "cwd_outside_roots", Reason: "工作目录不在 AllowedRoots 内", Detail: abs,
		}
	}
	return abs, nil
}

// withinRoots 判断绝对路径是否落在任一 AllowedRoot 内（含本身）。
// 用 filepath.Rel 判断，避免 "C:\a" 前缀匹配 "C:\ab" 这种经典错误。
func (v *Validator) withinRoots(abs string) bool {
	abs = filepath.Clean(abs)
	for _, root := range v.roots {
		if samePath(abs, root) {
			return true
		}
		rel, err := filepath.Rel(root, abs)
		if err != nil {
			continue
		}
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		if filepath.IsAbs(rel) {
			continue
		}
		return true
	}
	return false
}

// BuildEnv 按 EnvironmentPolicy 构造子进程环境变量。
//
// 这是第 21 章红线在 Worker 侧的落点：默认不继承，且无论何种策略都剔除密钥类键。
func (v *Validator) BuildEnv(extra map[string]string) []string {
	var base []string
	switch v.policy.EnvironmentPolicy {
	case EnvInherit:
		base = os.Environ()
	case EnvAllowlist:
		for _, kv := range os.Environ() {
			i := strings.IndexByte(kv, '=')
			if i <= 0 {
				continue
			}
			if v.envAllow[strings.ToUpper(kv[:i])] {
				base = append(base, kv)
			}
		}
	default: // EnvMinimal
		base = procguard.MinimalEnv()
	}

	// 统一剔除密钥类键（即使在 inherit 模式下）。
	out := make([]string, 0, len(base)+8)
	seen := map[string]bool{}
	for _, kv := range base {
		i := strings.IndexByte(kv, '=')
		if i <= 0 {
			continue
		}
		key := kv[:i]
		if v.envDenied(key) {
			continue
		}
		if seen[strings.ToUpper(key)] {
			continue
		}
		seen[strings.ToUpper(key)] = true
		out = append(out, kv)
	}

	// 调用方提供的额外变量也要过同一套剔除规则。
	for k, val := range extra {
		if v.envDenied(k) {
			continue
		}
		if seen[strings.ToUpper(k)] {
			continue
		}
		seen[strings.ToUpper(k)] = true
		out = append(out, k+"="+val)
	}

	// 网络限制的环境级加固（尽力而为，见 NetworkMode 注释）。
	switch v.policy.NetworkMode {
	case NetworkDeny:
		out = append(out,
			"HTTP_PROXY=http://127.0.0.1:0",
			"HTTPS_PROXY=http://127.0.0.1:0",
			"ALL_PROXY=http://127.0.0.1:0",
			"NO_PROXY=",
			"GIT_TERMINAL_PROMPT=0",
			"GIT_ASKPASS=echo",
		)
	case NetworkLoopbackOnly:
		out = append(out, "NO_PROXY=127.0.0.1,localhost,::1")
	}
	return out
}

func (v *Validator) envDenied(key string) bool {
	up := strings.ToUpper(key)
	for _, re := range v.envDeny {
		if re.MatchString(up) {
			return true
		}
	}
	return false
}

// ============================== 辅助函数 ==============================

// normalizeExe 把可执行文件路径/名字归一化为"小写去扩展名的基名"，用于黑/白名单匹配。
// 例如 "C:\Windows\System32\CMD.EXE" → "cmd"。
func normalizeExe(name string) string {
	name = strings.TrimSpace(name)
	name = strings.Trim(name, `"'`)
	base := filepath.Base(filepath.Clean(name))
	lower := strings.ToLower(base)
	// 去掉 Windows 常见可执行扩展名。先整体匹配 ".exe" 再判基本名，
	// 因为 "cmd.exe" 应该在拒绝列表里以 "cmd.exe" 或 "cmd" 出现都能命中。
	switch filepath.Ext(lower) {
	case ".exe", ".bat", ".cmd", ".com", ".ps1", ".sh", ".py", ".js", ".vbs", ".msi":
		lower = strings.TrimSuffix(lower, filepath.Ext(lower))
	}
	return lower
}

// globToRegexp 把 * 通配的键模式转成正则（大小写不敏感，全串匹配）。
func globToRegexp(pattern string) (*regexp.Regexp, error) {
	var sb strings.Builder
	sb.WriteString("^")
	for _, r := range strings.ToUpper(pattern) {
		switch r {
		case '*':
			sb.WriteString(".*")
		case '?':
			sb.WriteString(".")
		default:
			sb.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	sb.WriteString("$")
	return regexp.Compile(sb.String())
}

// samePath 比较两个路径是否指同一位置（Windows 大小写不敏感，且忽略尾部斜杠）。
func samePath(a, b string) bool {
	a = filepath.Clean(a)
	b = filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// looksLikePath 判断参数是否像文件路径。
func looksLikePath(a string) bool {
	if filepath.IsAbs(a) {
		return true
	}
	if strings.HasPrefix(a, "./") || strings.HasPrefix(a, "../") || strings.HasPrefix(a, ".\\") || strings.HasPrefix(a, "..\\") {
		return true
	}
	return false
}

// escapesRoot 判断相对路径是否向上越界。
func escapesRoot(p string) bool {
	depth := 0
	seps := func(r rune) bool { return r == '/' || r == '\\' }
	for _, part := range strings.FieldsFunc(p, seps) {
		switch part {
		case "..":
			depth--
			if depth < 0 {
				return true
			}
		case ".", "":
		default:
			depth++
		}
	}
	return false
}

// looksLikeNetworkTarget 判断参数是否是 URL 或 host:port 形态。
func looksLikeNetworkTarget(a string) bool {
	lower := strings.ToLower(a)
	for _, scheme := range []string{"http://", "https://", "ftp://", "ftps://", "ws://", "wss://", "ssh://", "git://", "tcp://", "udp://"} {
		if strings.HasPrefix(lower, scheme) {
			return true
		}
	}
	// host:port 形态（排除 Windows 盘符 "C:\..."）。
	if i := strings.LastIndexByte(a, ':'); i > 0 && i < len(a)-1 {
		host := a[:i]
		port := a[i+1:]
		if len(host) == 1 && strings.ContainsAny(host, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ") {
			return false // 盘符
		}
		allDigits := port != ""
		for _, r := range port {
			if r < '0' || r > '9' {
				allDigits = false
				break
			}
		}
		if allDigits && !strings.ContainsAny(host, `/\`) {
			return true
		}
	}
	return false
}

// defaultShell 返回当前平台的默认 shell（仅在 AllowShell=true 且 Mode=shell 时用）。
func defaultShell() string {
	if runtime.GOOS == "windows" {
		if p, err := lookPath("pwsh"); err == nil {
			return p
		}
		return "powershell"
	}
	if p, err := lookPath("bash"); err == nil {
		return p
	}
	return "/bin/sh"
}

// shellArgs 构造 shell 的解释参数。
// Windows 上注入 UTF-8 前置语句（对齐 v1 的编码修复，属功能对等）。
func shellArgs(shell, command string) []string {
	base := strings.ToLower(filepath.Base(shell))
	switch {
	case strings.Contains(base, "powershell"), strings.Contains(base, "pwsh"):
		prefix := "chcp 65001 > $null; [Console]::OutputEncoding = [System.Text.Encoding]::UTF8; $OutputEncoding = [System.Text.Encoding]::UTF8; "
		return []string{"-NoProfile", "-NonInteractive", "-Command", prefix + command}
	case strings.Contains(base, "cmd"):
		return []string{"/d", "/s", "/c", command}
	default: // bash / sh / zsh
		return []string{"-c", command}
	}
}

// lookPath 是 exec.LookPath 的薄包装（隔离依赖，便于测试替换）。
var lookPath = func(name string) (string, error) { return execLookPath(name) }

// acquireSem 是带 ctx 的信号量获取。
func acquireSem(ctx context.Context, ch chan struct{}) error {
	select {
	case ch <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func releaseSem(ch chan struct{}) {
	select {
	case <-ch:
	default:
	}
}

// ensureImportUsed 保持 sync 包被显式使用（Validator 内部若改为带锁实现时无需改 import）。
var _ = sync.Mutex{}
