// Command ximo-plugin 是一个独立、通用的 Agent 接入工具：把任意中转站（OpenAI/Anthropic 兼容）
// 接进本机已安装的 Agent（Claude Code、Codex CLI、Continue、Cursor、aider 等）。
//
// 它只依赖本 module 的 internal/*，不 import 主仓库 ximo-Agent 的任何包。
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/1535273240sch-droid/ximo-plugin/internal/engine"
)

// version 是版本号；构建时可用 -ldflags "-X main.version=..." 注入（见 build.cmd）。
var version = "0.1.0"

// 退出码语义（契约 §4）：0 成功 / 1 运行失败 / 2 用法错误。
const (
	exitOK    = 0
	exitFail  = 1
	exitUsage = 2
)

type globals struct {
	home    string
	json    bool
	noColor bool
	help    bool
	version bool
}

type app struct {
	in    io.Reader
	out   io.Writer
	err   io.Writer
	home  string
	json  bool
	color bool
	tty   bool // stdin 是否交互终端

	// isolateHome 表示 home 是用户显式指定的（--home / $XIMO_PLUGIN_HOME）：
	// 此时 %APPDATA% / $XIMO_HOME 等也按它解析，一个字节都不落到真实用户目录。
	isolateHome bool
	// ipcEndpoint 是本次 apply 的 IPC 端点（--ipc-endpoint；空则按
	// flag → $XIMO_IPC_ENDPOINT → 平台默认 的顺序发现）。
	ipcEndpoint string
}

func main() {
	os.Exit(Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// Run 是可测试的入口：返回进程退出码，不调用 os.Exit。
func Run(args []string, in io.Reader, out, errOut io.Writer) int {
	g, rest, err := parseGlobal(args)
	if err != nil {
		fmt.Fprintln(errOut, "用法错误: "+err.Error())
		printUsage(errOut)
		return exitUsage
	}
	if g.version {
		fmt.Fprintf(out, "ximo-plugin %s\n", version)
		return exitOK
	}
	if g.help {
		printUsage(out)
		return exitOK
	}
	if len(rest) == 0 {
		printUsage(errOut)
		return exitUsage
	}

	home, isolated := resolveHome(g.home)
	a := &app{
		in:          in,
		out:         out,
		err:         errOut,
		home:        home,
		isolateHome: isolated,
		json:        g.json,
		color:       !g.noColor && isTTY(out),
		tty:         isTTY(in),
	}

	// engine 的差异/清单输出跟随本次调用：--json 时走 stderr，保证 stdout 只有 JSON。
	engine.Out = a.out
	if g.json {
		engine.Out = a.err
	}

	cmd, cmdArgs := rest[0], rest[1:]
	switch cmd {
	case "detect":
		return a.cmdDetect(cmdArgs)
	case "adapters":
		return a.cmdAdapters(cmdArgs)
	case "login":
		return a.cmdLogin(cmdArgs)
	case "models":
		return a.cmdModels(cmdArgs)
	case "usage":
		return a.cmdUsage(cmdArgs)
	case "apply":
		return a.cmdApply(cmdArgs)
	case "print-env":
		return a.cmdPrintEnv(cmdArgs)
	case "doctor":
		return a.cmdDoctor(cmdArgs)
	case "ui":
		return a.cmdUI(cmdArgs)
	case "version", "--version":
		fmt.Fprintf(out, "ximo-plugin %s\n", version)
		return exitOK
	default:
		fmt.Fprintf(errOut, "未知命令 %q\n", cmd)
		printUsage(errOut)
		return exitUsage
	}
}

func parseGlobal(args []string) (globals, []string, error) {
	var g globals
	var rest []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--":
			rest = append(rest, args[i+1:]...)
			i = len(args)
		case a == "--json":
			g.json = true
		case a == "--no-color":
			g.noColor = true
		case a == "-h" || a == "--help" || a == "help":
			g.help = true
		case a == "--version" || a == "-v":
			g.version = true
		case a == "--home":
			if i+1 >= len(args) {
				return g, nil, fmt.Errorf("--home 需要一个值")
			}
			i++
			g.home = args[i]
		case strings.HasPrefix(a, "--home="):
			g.home = strings.TrimPrefix(a, "--home=")
		default:
			rest = append(rest, a)
		}
	}
	return g, rest, nil
}

// resolveHome 确定本次调用的用户主目录；第二个返回值表示它是否由用户显式指定
// （--home / $XIMO_PLUGIN_HOME），也就是「隔离模式」——此时家目录相关的环境变量
// 一并按它解析，见 spec.ExpandOptions.IsolateHome。
func resolveHome(flagHome string) (string, bool) {
	if flagHome != "" {
		return flagHome, true
	}
	if env := os.Getenv("XIMO_PLUGIN_HOME"); env != "" {
		return env, true
	}
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return h, false
	}
	return ".", false
}

// isTTY 判断一个 writer/reader 是否指向交互终端；测试里传 bytes.Buffer 会得到 false。
var isTTY = defaultIsTTY

func defaultIsTTY(v any) bool {
	f, ok := v.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// flags 为子命令建一个静默的 FlagSet（错误由我们统一格式化，保证退出码 2）。
func (a *app) flags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

func (a *app) usageErr(err error) int {
	// flag 包在 `-h/--help` 时原样返回 flag.ErrHelp（未包装），这里当正常退出处理。
	if err == flag.ErrHelp {
		printUsage(a.out)
		return exitOK
	}
	fmt.Fprintln(a.err, "用法错误: "+err.Error())
	return exitUsage
}

func (a *app) fail(err error) int {
	fmt.Fprintln(a.err, "错误: "+err.Error())
	return exitFail
}

const (
	cReset  = "\x1b[0m"
	cGreen  = "\x1b[32m"
	cRed    = "\x1b[31m"
	cYellow = "\x1b[33m"
	cDim    = "\x1b[2m"
)

func (a *app) paint(code, s string) string {
	if !a.color {
		return s
	}
	return code + s + cReset
}

// noteWriter 是过程信息（含 agents 包的进度/告警）的出口：--json 时走 stderr，
// 保证 stdout 只有 JSON。
func (a *app) noteWriter() io.Writer {
	if a.json {
		return a.err
	}
	return a.out
}

// notef 输出过程信息：--json 时走 stderr，保证 stdout 只有 JSON。
func (a *app) notef(format string, args ...any) {
	fmt.Fprintf(a.noteWriter(), format+"\n", args...)
}

func (a *app) passf(format string, args ...any) {
	a.notef("%s %s", a.paint(cGreen, "PASS"), fmt.Sprintf(format, args...))
}

func (a *app) failf(format string, args ...any) {
	a.notef("%s %s", a.paint(cRed, "FAIL"), fmt.Sprintf(format, args...))
}

func (a *app) warnf(format string, args ...any) {
	a.notef("%s %s", a.paint(cYellow, "WARN"), fmt.Sprintf(format, args...))
}

func (a *app) emitJSON(v any) int {
	b, err := marshalIndent(v)
	if err != nil {
		return a.fail(fmt.Errorf("序列化 JSON 失败: %w", err))
	}
	fmt.Fprintln(a.out, string(b))
	return exitOK
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `ximo-plugin `+version+` —— 把任意中转站接进本机 Agent

用法:
  ximo-plugin <命令> [参数] [全局 flag]

命令:
  detect                        列出本机检测到的 Agent（含证据路径/变量）
  adapters [list|show <id>]     查看内置与自定义适配器规格
  login    --gateway <url>      设备码登录并保存凭据
  models   --gateway <url>      列出网关可用模型
  usage    --gateway <url>      查看用量（[--limit N]，默认 20）
  apply    --gateway <url>      写入 Agent 配置（[--spec <id>|--all] [--model <m>]
                                [--api-key <k>] [--dry-run] [--yes] [--no-ipc]
                                [--ipc-endpoint <p>] [--show-secrets]
                                [--allow-short-lived-token]）
  print-env --gateway <url>     打印环境变量形态（[--spec <id>] [--model <m>]
                                [--style export|set|powershell]）
  doctor   [--gateway <url>]    逐项体检，有问题时退出码非 0
  ui       [--port 8787]        启动本地网页界面（只绑 127.0.0.1，带会话令牌）
                                [--no-open] [--gateway <url>]

全局 flag:
  --home <dir>   覆盖用户主目录（默认 $XIMO_PLUGIN_HOME 或系统主目录）
  --json         以 JSON 输出（过程信息走 stderr）
  --no-color     关闭彩色输出
  --version      打印版本
  -h, --help     打印本帮助

退出码: 0 成功 / 1 运行失败 / 2 用法错误
`)
}
