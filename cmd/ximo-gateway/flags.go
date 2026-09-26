package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/observability"
	"github.com/ximo888ok-netizen/ximo-agent/internal/quota"
	"github.com/ximo888ok-netizen/ximo-agent/internal/storage"
	"github.com/ximo888ok-netizen/ximo-agent/internal/storage/migrations"
	"github.com/ximo888ok-netizen/ximo-agent/internal/storage/sqlite"
)

// 环境变量。adminToken 这一个与 internal/gateway 的同名未导出常量取同一个变量
// （契约 §12.1 要求 --admin-token 与它是同一条回退链）；其余三个是本二进制独有的
// 可选项，缺省不改变任何行为。
const (
	envAdminToken = "XIMO_GATEWAY_ADMIN_TOKEN"
	envHome       = "XIMO_GATEWAY_HOME"
	envLogLevel   = "XIMO_GATEWAY_LOG_LEVEL"
	envPepper     = "XIMO_GATEWAY_PEPPER"
)

// flag 默认值（契约 §12.1 逐字）。刻意不复用 gateway 包的 DefaultAddr /
// DefaultRateLimitPerMin：那是 HTTP 骨架库的通用默认值（8080 / 60），与本二进制
// 对外承诺的默认值（8600 / 不限流）不同，复用会在改动骨架时悄悄改掉对外契约。
const (
	defaultAddr              = "127.0.0.1:8600"
	defaultPriceMicroPerKTok = int64(1)
	defaultMaxOutputTokens   = int64(4096)
	defaultRequestTimeout    = 5 * time.Minute
	defaultRateLimitPerMin   = 0
)

// 预占回收（--reap-interval/--reap-limit）的默认值。这两个 flag 不在契约 §12.1
// 的冻结列表里，是本轮修 D6（ReapExpired 无调用方）时新增的**追加项**：默认值
// 保住了「启动即开始回收」的既有期望，不改任何既有 flag 的语义。
const (
	defaultReapInterval = time.Minute
	defaultReapLimit    = quota.DefaultReapLimit
)

// options 是命令行与环境变量的解析结果。
type options struct {
	addr              string
	dbPath            string
	migrationsDir     string
	adminToken        string
	priceMicroPerKTok int64
	maxOutputTokens   int64
	requestTimeout    time.Duration
	rateLimitPerMin   int
	reapInterval      time.Duration
	reapLimit         int
	migrateOnly       bool
	showVersion       bool
}

// parseOptions 解析命令行（优先级：显式 flag > 环境变量 > 默认值）。
//
// 返回的错误与用法输出都不含任何令牌内容。
func parseOptions(args []string, stderr io.Writer) (options, error) {
	var opt options
	fs := flag.NewFlagSet("ximo-gateway", flag.ContinueOnError)
	if stderr != nil {
		fs.SetOutput(stderr)
	}
	fs.StringVar(&opt.addr, "addr", defaultAddr, "监听地址；默认只绑回环，不要默认对外")
	fs.StringVar(&opt.dbPath, "db", defaultDBPath(), "SQLite 数据库文件路径")
	fs.StringVar(&opt.migrationsDir, "migrations", "", "迁移脚本目录；未指定时按 exe 同级 → ./migrations → ../migrations → ../../migrations 探测")
	fs.StringVar(&opt.adminToken, "admin-token", "", "管理口令（X-Admin-Token）；也可用环境变量 "+envAdminToken+"；两者都没有则拒绝启动")
	fs.Int64Var(&opt.priceMicroPerKTok, "price-micro-per-ktok", defaultPriceMicroPerKTok, "占位单价（微单位 / 1K token）；V1 无真实价目表")
	fs.Int64Var(&opt.maxOutputTokens, "max-output-tokens", defaultMaxOutputTokens, "额度预占时假定的最大输出 token 数")
	fs.DurationVar(&opt.requestTimeout, "request-timeout", defaultRequestTimeout, "单请求总时限，0 表示不限时")
	fs.IntVar(&opt.rateLimitPerMin, "rate-limit-per-min", defaultRateLimitPerMin, "每凭据每分钟请求数上限，0 表示不限流")
	fs.DurationVar(&opt.reapInterval, "reap-interval", defaultReapInterval, "超时预占（held）的回收周期，0 表示关闭回收")
	fs.IntVar(&opt.reapLimit, "reap-limit", defaultReapLimit, "单次回收的预占条数上限，<=0 时用 quota 包默认值")
	fs.BoolVar(&opt.migrateOnly, "migrate-only", false, "只跑迁移然后退出 0")
	fs.BoolVar(&opt.showVersion, "version", false, "打印版本后退出 0")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "ximo-gateway %s —— XIMO 中转站服务端\n\n用法: ximo-gateway [flags]\n\n", version)
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		return options{}, err
	}
	if rest := fs.Args(); len(rest) > 0 {
		// 位置参数一律拒绝：静默忽略会让 "--admin-token=<token>" 这类手滑（写成
		// 位置参数）变成"用默认配置启动了"。
		return options{}, fmt.Errorf("不支持位置参数: %v", rest)
	}

	opt.addr = strings.TrimSpace(opt.addr)
	opt.dbPath = strings.TrimSpace(opt.dbPath)
	opt.migrationsDir = strings.TrimSpace(opt.migrationsDir)
	// 令牌两侧空白一律去掉：AuthenticateAdmin 也做 TrimSpace，两边口径必须一致，
	// 否则"带空格的令牌"会出现"启动时校验通过、请求时永远 401"的假象。
	opt.adminToken = strings.TrimSpace(opt.adminToken)
	if opt.adminToken == "" {
		opt.adminToken = strings.TrimSpace(os.Getenv(envAdminToken))
	}
	return opt, nil
}

// defaultDBPath 返回默认数据库路径：<XIMO_GATEWAY_HOME|%APPDATA%\ximo-agent\gateway>\gateway.db。
func defaultDBPath() string {
	home := defaultGatewayHome()
	if home == "" {
		// 连 home 都解析不出来（APPDATA 与 USERPROFILE 都缺）时退化为工作目录下的
		// 相对路径，而不是拼出一个半截的 "AppData/Roaming/..." 相对路径。
		return "ximo-gateway.db"
	}
	return filepath.Join(home, "gateway.db")
}

// defaultGatewayHome 按平台给出网关数据目录，与 internal/config.DefaultPaths
// 的 baseDir 推导保持一致（只是多一层 gateway 子目录，避免与 Agent 运行时的
// data/logs 混在一起）。
func defaultGatewayHome() string {
	if custom := strings.TrimSpace(os.Getenv(envHome)); custom != "" {
		return custom
	}
	switch runtime.GOOS {
	case "windows":
		appData := strings.TrimSpace(os.Getenv("APPDATA"))
		if appData == "" {
			profile := strings.TrimSpace(os.Getenv("USERPROFILE"))
			if profile == "" {
				return ""
			}
			appData = filepath.Join(profile, "AppData", "Roaming")
		}
		return filepath.Join(appData, "ximo-agent", "gateway")
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return ""
		}
		return filepath.Join(home, "Library", "Application Support", "ximo-agent", "gateway")
	default:
		if xdg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); xdg != "" {
			return filepath.Join(xdg, "ximo-agent", "gateway")
		}
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return ""
		}
		return filepath.Join(home, ".config", "ximo-agent", "gateway")
	}
}

// gatewayPepper 返回口令 pepper（混入 PBKDF2 前的 HMAC 密钥）。
//
// 诚实说明（V1 已知缺口，不假装做到）：网关库里没有 settings/kv 表，而
// internal/secrets 的 ref 是由明文值本身派生的（secrets.RefForValue），没有
// "用固定名字找回同一秘密"的读回路径，因此本进程无法在首次启动时生成一个随机
// pepper、持久化、并在下次启动取回。默认传 nil，即 internal/account 退化为标准
// PBKDF2-SHA256（迭代次数满足契约 §0.1 的"同等级"要求）。
// 运维可用 XIMO_GATEWAY_PEPPER 显式提供更强的服务端秘密；一旦使用必须保证跨重启
// 一致，否则既有用户口令全部校验失败（表现为"全员登录不上"）。
func gatewayPepper() []byte {
	v := os.Getenv(envPepper)
	if v == "" {
		return nil
	}
	return []byte(v)
}

// openStore 打开 SQLite 并应用迁移。
//
// 复用仓库既有装配方式（internal/storage.Open + sqlite.DefaultConfig）：单写者
// 写队列、WAL、busy_timeout、外键开关都由 sqlite 层配好，本包绝不自己写 PRAGMA、
// 也不自己起第二个连接池（两个池争同一文件的写锁正是 bootstrap 注释里踩过的坑）。
func openStore(ctx context.Context, opt options, logger *observability.Logger) (*storage.Store, error) {
	if dir := filepath.Dir(opt.dbPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("创建数据库目录 %s 失败: %w", dir, err)
		}
	}
	store, err := storage.Open(sqlite.DefaultConfig(opt.dbPath))
	if err != nil {
		return nil, fmt.Errorf("打开数据库 %s 失败: %w", opt.dbPath, err)
	}
	if err := applyMigrations(ctx, store.DB(), opt.migrationsDir, logger); err != nil {
		_ = store.Close()
		return nil, err
	}
	return store, nil
}

// applyMigrations 定位迁移目录并执行迁移。
//
// 迁移 runner 一律复用 internal/storage/migrations（版本连续性、checksum 校验、
// migrations 表维护都在那里，绝不重写）；这里只负责"找目录"，探测顺序与
// internal/bootstrap.defaultMigrationDirs 一致：exe 同级 → ./migrations →
// ../migrations → ../../migrations。bootstrap 里的实现未导出，且 §0.5 禁止改动
// 该包，故在此复述同一套顺序。
//
// 显式传入 --migrations 时只认这一个目录：路径写错必须立刻报错，不能静默回退到
// 别的目录（否则"迁移到哪个库、用哪套 SQL"会变得不可预期）。
func applyMigrations(ctx context.Context, db *sqlite.DB, explicitDir string, logger *observability.Logger) error {
	dirs := defaultMigrationDirs()
	if explicitDir != "" {
		dirs = []string{explicitDir}
	}
	tried := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		if _, err := os.Stat(dir); err != nil {
			tried = append(tried, dir)
			continue
		}
		applied, err := migrations.ApplyFromDir(ctx, db, dir)
		if err != nil {
			return fmt.Errorf("执行迁移失败（目录 %s）: %w", dir, err)
		}
		logger.Info(ctx, "ximo-gateway: 迁移已应用", map[string]any{"dir": dir, "versions": applied})
		return nil
	}
	return fmt.Errorf("找不到迁移目录（已尝试 %v）", tried)
}

// defaultMigrationDirs 返回候选迁移目录（与 bootstrap 的探测顺序一致）。
func defaultMigrationDirs() []string {
	var out []string
	if exe, err := os.Executable(); err == nil {
		out = append(out, filepath.Join(filepath.Dir(exe), "migrations"))
	}
	if wd, err := os.Getwd(); err == nil {
		out = append(out,
			filepath.Join(wd, "migrations"),
			filepath.Join(wd, "..", "migrations"),
			filepath.Join(wd, "..", "..", "migrations"),
		)
	}
	return out
}
