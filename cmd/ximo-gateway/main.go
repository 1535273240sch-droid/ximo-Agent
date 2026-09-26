// ximo-gateway 是 XIMO 中转站（服务端中转网关）的进程入口（契约 §12.1）。
//
// 本包只做装配，顺序刻意如此（每一步都依赖前一步）：
//
//	解析 flag/env → 打开 SQLite 并跑迁移 → 组装 store/account/quota/catalog/upstream
//	+ internal/secrets 的 SecretResolver 适配器 → 取各 api 子包的 Routes(Deps)
//	→ 套鉴权（gateway.Authenticator）与限流（gateway.KeyLimiter）
//	→ gateway.New → 起预占回收协程（reaper.go，随主 ctx 优雅退出）
//	→ 监听 → 收到 SIGINT/SIGTERM 后优雅停机。
//
// 边界：本包不写 SQL、不解析凭据、不判断额度与计费，这些都在各自的服务包内；
// 路由形状由各 api 子包自带（§11.2），本包只负责挂载与套中间件。管理令牌只存在于
// 启动参数与 gateway.Config 中，**绝不打印**：日志最多给出其 sha256 前 8 位
// （见 adminFingerprint），既能核对"配的是不是同一个令牌"，又无法反推明文。
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway"
	"github.com/ximo888ok-netizen/ximo-agent/internal/observability"
)

// version / gitCommit / buildTime 由构建脚本用 -ldflags 注入（与 cmd/ximo-agent 同约定）。
var (
	version   = "v1.0.0-alpha"
	gitCommit = "dev"
	buildTime = "unknown"
)

// 退出码。2 沿用 Go flag 包的"用法错误"约定，1 表示运行期错误。
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, nil))
}

// run 是 main 的可测入口：安装信号处理器后交给 runWithContext。
//
// onListen 非 nil 时在真正开始监听后被回调真实地址（测试用 --addr 127.0.0.1:0
// 时靠它拿到随机端口；生产传 nil）。
func run(args []string, stdout, stderr io.Writer, onListen func(addr string)) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runWithContext(ctx, args, stdout, stderr, onListen)
}

// runWithContext 装配并运行网关，返回进程退出码。ctx 取消即优雅停机。
func runWithContext(ctx context.Context, args []string, stdout, stderr io.Writer, onListen func(addr string)) int {
	opt, err := parseOptions(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		// --help 是用户主动请求，不是错误（parseOptions 已把用法打到 stderr）。
		return exitOK
	}
	if err != nil {
		fmt.Fprintf(stderr, "ximo-gateway: %v\n", err)
		return exitUsage
	}
	if opt.showVersion {
		fmt.Fprintf(stdout, "ximo-gateway %s (commit: %s, built: %s)\n", version, gitCommit, buildTime)
		return exitOK
	}

	logger := newLogger(stderr)

	// --migrate-only 是维护模式：只跑迁移并退出 0（§12.1/§12.3），因此既不监听端口，
	// 也不需要管理令牌（升级脚本就是拿它来"把库迁到最新"的）。
	if opt.migrateOnly {
		store, err := openStore(ctx, opt, logger)
		if err != nil {
			fmt.Fprintf(stderr, "ximo-gateway: %v\n", err)
			return exitError
		}
		defer func() { _ = store.Close() }()
		logger.Info(ctx, "ximo-gateway: --migrate-only 完成，正常退出", map[string]any{"db": opt.dbPath})
		return exitOK
	}

	// 管理令牌 fail closed：缺令牌不启动，而不是让 /admin/* 静默全开或全 401。
	// 放在打开数据库之前：配置错误不该留下副作用（连库文件都不该被创建）。
	if opt.adminToken == "" {
		fmt.Fprintf(stderr, "ximo-gateway: %v\n", gateway.ErrAdminTokenRequired)
		return exitUsage
	}

	store, err := openStore(ctx, opt, logger)
	if err != nil {
		fmt.Fprintf(stderr, "ximo-gateway: %v\n", err)
		return exitError
	}
	defer func() { _ = store.Close() }()

	cfg, routes, quotaSvc, err := buildStack(ctx, opt, store.DB(), logger)
	if err != nil {
		fmt.Fprintf(stderr, "ximo-gateway: 装配失败: %v\n", err)
		return exitError
	}
	srv, err := gateway.New(cfg, routes, logger)
	if err != nil {
		fmt.Fprintf(stderr, "ximo-gateway: %v\n", err)
		return exitUsage
	}

	// 预占回收协程（D6）：结算失败时故意不 Release（避免双重退款），遗留的 held
	// 预占只能靠过期回收兜底，否则会永久占住用户额度。
	// defer 顺序：stopReaper 在 store.Close 之后注册 → 先执行，保证协程停稳后
	// 才关库；且它在 runWithContext 返回前就等到协程退出，绝不会「Run 之后还在跑」。
	stopReaper := startReaper(ctx, quotaSvc, opt.reapInterval, opt.reapLimit, logger)
	defer stopReaper()

	// 自行 net.Listen 而不是用 Server.Run：这样才能把真实监听地址报出来
	// （--addr 传 :0 时端口由内核分配，只有拿到 listener 才知道）。
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		fmt.Fprintf(stderr, "ximo-gateway: 监听 %s 失败: %v\n", cfg.Addr, err)
		return exitError
	}
	logger.Info(ctx, "ximo-gateway: 启动", startFields(opt, cfg, ln.Addr().String()))
	if onListen != nil {
		onListen(ln.Addr().String())
	}

	if err := srv.Serve(ctx, ln); err != nil {
		logger.Error(ctx, "ximo-gateway: 服务异常退出", map[string]any{"err": err.Error()})
		return exitError
	}
	logger.Info(ctx, "ximo-gateway: 已优雅退出", map[string]any{"listen": ln.Addr().String()})
	return exitOK
}

// newLogger 构造结构化日志器并设为全局默认：本二进制是常驻服务，日志走 stderr
// （stdout 留给 --version 之类的一次性输出），并统一过 observability 的脱敏器。
// 未显式注入 logger 的包（meta/upstream 的降级路径）也因此写到同一个地方。
func newLogger(stderr io.Writer) *observability.Logger {
	lvl := observability.ParseLogLevel(os.Getenv(envLogLevel))
	logger := observability.NewLogger(stderr, lvl)
	observability.SetDefaultLogger(logger)
	return logger
}

// startFields 组装启动日志字段。基线一律取 cfg.LogFields()（管理令牌只以布尔量
// 出现），附加的字段名避开 observability 的敏感键名（token/secret/credential…），
// 且附加的值本身不含任何秘密。
func startFields(opt options, cfg gateway.Config, listen string) map[string]any {
	fields := cfg.LogFields()
	fields["listen"] = listen
	fields["migrations"] = opt.migrationsDir
	fields["version"] = version
	fields["admin_fingerprint"] = adminFingerprint(cfg.AdminToken)
	// 回收参数随启动日志一起给出：运维不必翻 flag 就能核对「回收是开着还是关了」。
	fields["reap_interval"] = opt.reapInterval.String()
	fields["reap_limit"] = effectiveReapLimit(opt.reapLimit)
	return fields
}

// adminFingerprint 返回管理令牌 sha256 的前 8 位十六进制。它是单向摘要的前缀，
// 无法反推令牌，仅供运维核对"两个环境/副本配的是不是同一个令牌"。
func adminFingerprint(adminToken string) string {
	if adminToken == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(adminToken))
	return hex.EncodeToString(sum[:4])
}
