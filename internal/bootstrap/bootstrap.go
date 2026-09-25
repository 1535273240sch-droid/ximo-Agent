package bootstrap

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/agent"
	"github.com/ximo888ok-netizen/ximo-agent/internal/config"
	ctxmgr "github.com/ximo888ok-netizen/ximo-agent/internal/context"
	"github.com/ximo888ok-netizen/ximo-agent/internal/engine"
	"github.com/ximo888ok-netizen/ximo-agent/internal/expert"
	"github.com/ximo888ok-netizen/ximo-agent/internal/observability"
	"github.com/ximo888ok-netizen/ximo-agent/internal/ports"
	"github.com/ximo888ok-netizen/ximo-agent/internal/provider"
	"github.com/ximo888ok-netizen/ximo-agent/internal/secrets"
	"github.com/ximo888ok-netizen/ximo-agent/internal/storage"
	"github.com/ximo888ok-netizen/ximo-agent/internal/storage/migrations"
	"github.com/ximo888ok-netizen/ximo-agent/internal/storage/sqlite"
	"github.com/ximo888ok-netizen/ximo-agent/internal/tool"
	"github.com/ximo888ok-netizen/ximo-agent/internal/tool/domains"
	"github.com/ximo888ok-netizen/ximo-agent/internal/tool/domains/file"
	gitdomain "github.com/ximo888ok-netizen/ximo-agent/internal/tool/domains/git"
	"github.com/ximo888ok-netizen/ximo-agent/internal/tool/domains/knowledge"
	"github.com/ximo888ok-netizen/ximo-agent/internal/tool/domains/web"
	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
	"github.com/ximo888ok-netizen/ximo-agent/internal/worker"
	"github.com/ximo888ok-netizen/ximo-agent/internal/worker/browser"
	"github.com/ximo888ok-netizen/ximo-agent/internal/worker/computeruse"
	"github.com/ximo888ok-netizen/ximo-agent/internal/worker/dynamic"
	"github.com/ximo888ok-netizen/ximo-agent/internal/worker/mcp"
	"github.com/ximo888ok-netizen/ximo-agent/internal/worker/office"
	"github.com/ximo888ok-netizen/ximo-agent/internal/worker/terminal"
)

// App 是一次装配完成的运行实例，持有全部需要显式释放的资源。
type App struct {
	Engine *engine.Engine

	// db 与 store 由 Engine 通过适配器间接使用，这里保留句柄以便统一 Close。
	db      *sqlite.DB
	store   *storage.Store
	workers *worker.Manager
	secrets *secrets.Manager

	// cfg 是本次装配使用的配置。界面修改配置时需要它作为基准。
	cfg *config.Config
	// providerPort 是当前生效的 Provider 端口。密钥或模型变更后会被热替换，
	// 因此不能只存在 Engine 的 deps 里（那里是值拷贝）。
	providerPort ports.Provider
	// providerHolder 是交给 Engine 的可替换壳，换 Provider 就是换它的指针。
	providerHolder *swappableProvider
	// cfgPath 是配置文件的落盘路径（可能为空，表示未持久化）。
	cfgPath string
	// opts 保存装配期注入点。子代理模型池是懒构造的，重建时必须使用与
	// buildProvider 相同的注入（尤其是测试注入的 SecretResolver），否则池里
	// 的客户端会拿不到密钥。
	opts Options

	// subAgentPool 缓存任务 05 的子代理模型池（懒构造，配置变更后失效重建）。
	subAgentPool subAgentPoolState
}

// Options 覆盖装配时的外部注入点，主要供测试使用。
type Options struct {
	// MigrationsDir 指向 SQL 迁移脚本目录；为空时依次尝试
	// <可执行文件同级>/migrations 与 <工作目录>/migrations。
	MigrationsDir string

	// Provider 允许测试注入一个假的 Provider 端口，跳过真实 HTTP 客户端。
	// 非 nil 时不再尝试构造真实 provider.Client。
	Provider ports.Provider

	// SecretResolver 允许测试/集成环境用环境变量提供密钥，
	// 绕开平台安全存储（DPAPI/Keychain）在无人值守环境下的交互问题。
	// 非 nil 时优先于 secrets.Manager。
	SecretResolver provider.SecretResolver

	// WorkspaceRoot 覆盖配置中的工具根目录（测试常指向临时目录）。
	WorkspaceRoot string

	// DisableWorkers 为真时不启动任何 Worker 池（最小装配，仅进程内工具）。
	DisableWorkers bool
}

// New 按配置装配出一个可运行的 App。
//
// 装配顺序刻意如此，因为每一步都依赖前一步：
// 目录 -> SQLite -> 迁移 -> 各端口适配器 -> Provider/Tool/Worker -> Engine。
// 任一「硬依赖」（事件库、Provider、工具运行时）失败即返回错误；可选依赖
// （Worker 池、上下文压缩、专家）失败只降级，不影响引擎可用。
func New(cfg *config.Config, opts Options) (*App, error) {
	if cfg == nil {
		return nil, fmt.Errorf("bootstrap: nil config")
	}
	if err := cfg.Paths.EnsureDirectories(); err != nil {
		return nil, fmt.Errorf("bootstrap: create directories: %w", err)
	}

	app := &App{cfg: cfg, opts: opts}

	// --- 1. SQLite 主库 + 存储门面 ------------------------------------------
	// 只打开一次连接池：storage.Open 内部已按「单写者 + 多读者」配好
	// busy_timeout/WAL，db 句柄从它取出复用。若此处再 sqlite.Open 一次，
	// 就会出现两个连接池争同一文件的写锁。
	dbPath := cfg.ResolveDBPath()
	dbCfg := sqlite.DefaultConfig(dbPath)
	dbCfg.Profile = parseProfile(cfg.Storage.Profile)
	store, err := storage.Open(dbCfg)
	if err != nil {
		return nil, fmt.Errorf("bootstrap: open sqlite (%s): %w", dbPath, err)
	}
	app.store = store
	db := store.DB()
	app.db = db

	// --- 2. 迁移 ------------------------------------------------------------
	if err := applyMigrations(context.Background(), db, opts.MigrationsDir); err != nil {
		app.Close()
		return nil, fmt.Errorf("bootstrap: apply migrations: %w", err)
	}

	// --- 3. 密钥管理器 -------------------------------------------------------
	// 必须在 Provider 之前无条件初始化：界面上的"保存 API 密钥"只依赖它，
	// 与 Provider 能否构造成功无关。历史上它被放在 buildProvider 的某个分支里，
	// 一旦那条分支被跳过（测试注入 Provider、或 Provider 构造失败），
	// app.secrets 就是 nil，导致密钥功能在运行中的进程里整体不可用。
	if opts.SecretResolver == nil {
		mgr, err := secrets.NewDefaultManager()
		if err == nil && mgr.Available() {
			app.secrets = mgr
		} else if envMgr, envErr := secrets.NewEnvManager(); envErr == nil {
			app.secrets = envMgr
		} else {
			// 完全不阻断启动：没有密钥只意味着不能调用模型，
			// 界面仍应能打开并提示用户去配置。
			observability.LogWarn(context.Background(),
				"secrets backend unavailable; API key configuration will be disabled",
				map[string]any{"err": fmt.Sprint(err)})
		}
	}

	// --- 4. Provider --------------------------------------------------------
	// Provider 是 Engine 的硬依赖之一：没有模型调用能力，run 无法推进。
	prov, err := buildProvider(cfg, opts, app)
	if err != nil {
		app.Close()
		return nil, fmt.Errorf("bootstrap: build provider: %w", err)
	}
	app.providerPort = prov

	// --- 5. Worker 池（可选）------------------------------------------------
	// 必须先于工具运行时：工具运行时要把池中的动作桥接成 Agent 可见的工具
	// （见 worker_tools.go），所以池要先存在。
	// 失败不致命：进程内工具（file/git/knowledge/web）仍可用，只是浏览器、
	// 终端等高风险域会因找不到 Worker 而明确拒绝，这是可接受的降级。
	if !opts.DisableWorkers {
		mgr, err := buildWorkerManager(cfg, opts)
		if err != nil {
			observability.LogWarn(context.Background(),
				"worker pools disabled: bootstrap could not start any pool", map[string]any{"err": err.Error()})
		} else if mgr != nil {
			app.workers = mgr
		}
	}

	// --- 6. 工具运行时（含 Worker 工具桥接）----------------------------------
	tools, idemStore, err := buildToolRuntime(cfg, opts, app, db)
	if err != nil {
		app.Close()
		return nil, fmt.Errorf("bootstrap: build tool runtime: %w", err)
	}

	// --- 6. Engine 依赖 -----------------------------------------------------
	engineCfg := buildEngineConfig(cfg)
	// Engine 拿到的是一个稳定的可替换壳，这样用户改密钥/模型后无需重启。
	holder := &swappableProvider{cur: prov}
	app.providerHolder = holder
	deps := engine.Dependencies{
		Events:      newEventStoreAdapter(db, store),
		Outbox:      newOutboxAdapter(db, store),
		Checkpoints: newCheckpointAdapter(db),
		Idempotency: idemStore,
		Tools:       tools,
		Provider:    holder,
		Context:     newContextAdapter(ctxmgr.NewContextManager(ctxmgr.ContextManagerOptions{})),
		// 任务 05：engine 内的专家直连路径（用户手选专家）与 agent_expert 工具
		// 路径（App.ExpertOrchestrator）共用同一个子代理模型池与候选分配。池是
		// 懒构造 + 配置变更失效重建的，所以这里给取池函数而不是池本身。
		SubAgentPool: func() (expert.ModelPool, error) {
			p, err := app.SubAgentPool()
			if err != nil || p == nil {
				return nil, err
			}
			return p, nil
		},
		SubAgentCandidates: func(expertID, division string) []string {
			if app.cfg == nil {
				return nil
			}
			return app.cfg.SubAgentCandidates(expertID, division)
		},
	}
	if cfg.Runtime.MaxRecoveryAttempts > 0 {
		engineCfg.MaxRecoveryAttempts = cfg.Runtime.MaxRecoveryAttempts
	}

	guard := agent.NewPanicGuard(agent.GuardConfig{DumpDir: cfg.Paths.CrashDumpDir})
	eng, err := engine.New(engineCfg, deps, guard)
	if err != nil {
		app.Close()
		return nil, fmt.Errorf("bootstrap: create engine: %w", err)
	}
	app.Engine = eng
	return app, nil
}

// Close 释放装配期创建的资源。可重复调用。
func (a *App) Close() {
	if a.Engine != nil {
		a.Engine.Close()
		a.Engine = nil
	}
	if a.workers != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		_ = a.workers.Stop(ctx)
		cancel()
		a.workers = nil
	}
	if a.store != nil {
		_ = a.store.Close()
		a.store = nil
	}
	if a.db != nil {
		_ = a.db.Close()
		a.db = nil
	}
}

// ---------------------------------------------------------------------------
// 各子装配
// ---------------------------------------------------------------------------

// applyMigrations 找到并执行 SQL 迁移。
//
// migrations 包不内嵌 SQL（只接受 fs.FS），所以这里需要在运行时定位脚本目录。
// 搜索顺序覆盖「随二进制分发」与「从仓库根运行」两种常见部署形态。
func applyMigrations(ctx context.Context, db *sqlite.DB, explicitDir string, altDirs ...string) error {
	dirs := append([]string{explicitDir}, altDirs...)
	dirs = append(dirs, defaultMigrationDirs()...)

	var lastErr error
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		if _, err := os.Stat(dir); err != nil {
			continue
		}
		applied, err := migrations.ApplyFromDir(ctx, db, dir)
		if err != nil {
			return fmt.Errorf("apply from %s: %w", dir, err)
		}
		if len(applied) > 0 {
			observability.LogInfo(ctx, "applied database migrations",
				map[string]any{"dir": dir, "versions": applied})
		}
		return nil
	}
	return fmt.Errorf("no migrations directory found (looked in %v): %w", dirs, lastErr)
}

// defaultMigrationDirs 返回候选的迁移脚本目录（可执行文件同级、当前工作目录）。
func defaultMigrationDirs() []string {
	var out []string
	if exe, err := os.Executable(); err == nil {
		out = append(out, filepath.Join(filepath.Dir(exe), "migrations"))
	}
	if wd, err := os.Getwd(); err == nil {
		out = append(out, filepath.Join(wd, "migrations"))
		// 从仓库子目录运行时的兜底（例如 go test ./tests/e2e）。
		out = append(out, filepath.Join(wd, "..", "migrations"))
		out = append(out, filepath.Join(wd, "..", "..", "migrations"))
	}
	return out
}

// parseProfile 把配置字符串映射成 SQLite 调优档位。
func parseProfile(name string) sqlite.Profile {
	switch name {
	case "safe":
		return sqlite.ProfileSafe
	case "performance":
		return sqlite.ProfilePerformance
	default:
		return sqlite.ProfileBalanced
	}
}

// buildProvider 装配真实的模型客户端，或返回测试注入的 Provider。
func buildProvider(cfg *config.Config, opts Options, app *App) (ports.Provider, error) {
	if opts.Provider != nil {
		return opts.Provider, nil
	}

	pcfg := cfg.Provider
	if pcfg.BaseURL == "" {
		return nil, fmt.Errorf("provider base_url is empty")
	}

	client, err := newProviderClient(pcfg, resolveSecretResolver(opts, app))
	if err != nil {
		return nil, err
	}
	return newProviderAdapter(client, pcfg.Model), nil
}

// resolveSecretResolver 选择密钥解析器：优先用调用方注入的（测试），否则
// 复用第 3 步已建好的 manager。manager 由 New() 无条件初始化，这里只做取用。
//
// app 允许为 nil（某些仅需构造 provider 的调用路径），此时退化为一个始终
// 报"未配置"的解析器，而不是崩溃——这样密钥缺失只会让某次模型调用失败，
// 不会把整个进程拖垮。
func resolveSecretResolver(opts Options, app *App) provider.SecretResolver {
	if opts.SecretResolver != nil {
		return opts.SecretResolver
	}
	if app != nil && app.secrets != nil {
		return secretResolverAdapter{mgr: app.secrets}
	}
	return unresolvedSecretResolver{}
}

// providerConfigForClient 把 config 层的服务商配置映射成 provider 层的契约类型。
func providerConfigForClient(pcfg config.ProviderConfig) provider.ProviderConfig {
	return provider.ProviderConfig{
		ID:              pcfg.ID,
		Name:            pcfg.Name,
		BaseURL:         pcfg.BaseURL,
		ContextWindow:   pcfg.ContextWindow,
		MaxOutputTokens: pcfg.MaxOutputTokens,
		// 不区分内置厂商：一律按第三方 OpenAI 兼容接口对待。
		IsDeepSeek: false,
		// 第三方兼容优先：不发 enable_thinking/reasoning_effort 等非标准
		// 参数（严格网关会 400），reasoning_content 出站时同步剥离；
		// stream_options.include_usage 属 OpenAI 兼容标准字段，保留以
		// 正常统计 token 用量。
		Capabilities: provider.Capabilities{SendReasoningParams: false, SendStreamUsage: true},
		SecretRef:    pcfg.SecretRef,
	}
}

// newProviderClient 按单个服务商配置构造客户端。主 provider 与任务 05 的
// 模型池共用这一条构造路径，保证两者的重试/熔断/限流行为完全一致。
func newProviderClient(pcfg config.ProviderConfig, resolver provider.SecretResolver) (*provider.Client, error) {
	clientOpts := provider.ClientOptions{
		Config:         providerConfigForClient(pcfg),
		Secrets:        resolver,
		DefaultTimeout: pcfg.Timeout,
	}
	if pcfg.RateLimitPerSec > 0 {
		clientOpts.Limiter = provider.NewRateLimiter(pcfg.RateLimitPerSec, 1)
	}
	clientOpts.Breaker = provider.NewCircuitBreaker(5, 30*time.Second)

	return provider.NewClient(clientOpts)
}

// secretResolverAdapter 把无 ctx 的 secrets 接口适配成 provider 需要的带上
// 下文的接口。provider 的 ctx 在这里没有实际用途（DPAPI/环境变量都是本地同步
// 读取），但保留签名可以让 provider 侧未来接入远程密钥服务时不用改接口。
type secretResolverAdapter struct {
	mgr *secrets.Manager
}

func (a secretResolverAdapter) Get(_ context.Context, secretRef string) (string, error) {
	return a.mgr.Get(secretRef)
}

// unresolvedSecretResolver 在密钥后端缺失时使用：任何解析都返回明确错误。
//
// 刻意不返回空字符串而不报错——那会让 provider 带着空 Authorization 头去请求，
// 得到的 401 无法区分"密钥没配"与"密钥配错了"。
type unresolvedSecretResolver struct{}

func (unresolvedSecretResolver) Get(_ context.Context, secretRef string) (string, error) {
	return "", fmt.Errorf("bootstrap: no secrets backend is available (cannot resolve %q)", secretRef)
}

// buildToolRuntime 装配工具运行时并注册内置工具集。
//
// 返回的第二个值是 Engine 恢复期使用的幂等分类端口。
func buildToolRuntime(cfg *config.Config, opts Options, app *App, db *sqlite.DB) (ports.ToolRuntime, ports.IdempotencyStore, error) {
	absRoot, err := resolveWorkspaceRoot(cfg, opts)
	if err != nil {
		return nil, nil, err
	}

	registry := tool.NewRegistry()
	guard := domains.NewGuard([]string{absRoot})

	register := func(ts []tool.Tool) error {
		for _, t := range ts {
			if err := registry.Register(t); err != nil {
				return err
			}
		}
		return nil
	}
	if err := register(file.Tools(guard)); err != nil {
		return nil, nil, fmt.Errorf("register file tools: %w", err)
	}
	if err := register(gitdomain.Tools(guard)); err != nil {
		return nil, nil, fmt.Errorf("register git tools: %w", err)
	}
	if err := registry.Register(knowledge.New(nil)); err != nil {
		return nil, nil, fmt.Errorf("register knowledge tool: %w", err)
	}
	if err := registry.Register(web.New()); err != nil {
		return nil, nil, fmt.Errorf("register web tool: %w", err)
	}

	// Worker 池工具（browser / terminal / office / dynamic-js / mcp / computer-use）。
	// 只为配置里真正启用的 kind 注册：默认配置下模型看不到 office / computer-use
	// 的工具，避免给出「看起来能调、实际必定失败」的假能力。
	if app.workers != nil {
		enabled := make(map[string]bool, len(cfg.Runtime.WorkerPools))
		for kind, size := range cfg.Runtime.WorkerPools {
			if size > 0 {
				enabled[kind] = true
			}
		}
		if err := registerWorkerTools(registry, app.workers, enabled); err != nil {
			return nil, nil, fmt.Errorf("register worker tools: %w", err)
		}
	}

	permission, err := tool.NewPermissionEngine(tool.DefaultPermissionConfigs(), nil)
	if err != nil {
		return nil, nil, fmt.Errorf("build permission engine: %w", err)
	}

	// 幂等存储复用主库的连接（单写者 + busy_timeout 已在 sqlite 层配好），
	// 这样并发认领不会因为缺 busy_timeout 而退化成 SQLITE_BUSY。
	resolver := toolCallResolver{}
	idemStore := tool.NewIdempotencyStore(
		tool.NewSQLIdempotencyDB(db.ReadDB()),
		resolver,
		nil,           // nil 表示使用 DefaultClassPolicy（与 adapter 保持一致）
		5*time.Minute, // inflight 认领租约
	)

	sandbox := tool.NewSandbox(tool.DefaultSandboxPolicy())

	rt := &tool.ToolRuntime{
		Registry:       registry,
		Permission:     permission,
		Idempotency:    idemStore,
		Audit:          tool.NewAuditLogger(nil, nil),
		Redactor:       tool.NewBasicRedactor(),
		Sandbox:        sandbox,
		DefaultTimeout: cfg.Runtime.ToolExecTimeout,
		Now:            time.Now,
	}
	if rt.DefaultTimeout <= 0 {
		rt.DefaultTimeout = types.DefaultAgentConfig().ToolExecTimeout
	}

	return newToolRuntimeAdapter(rt, tool.Mode(autoModeOrDefault(cfg))), newIdempotencyAdapter(idemStore), nil
}

// toolCallResolver 为幂等存储解析 tool call 的参数。
//
// 生产环境下 tool call 的完整参数由引擎随事件一并落库；幂等存储只在「同一
// 幂等键重复到达」时需要用它做比对。当前装配没有把 repository 层接进来，因此
// 返回 unknown 让幂等存储按其策略处理（未知调用按最保守的非幂等类对待），
// 这比凭空造一份参数更安全。
type toolCallResolver struct{}

func (toolCallResolver) ResolveToolCall(_ context.Context, toolCallID string) (string, map[string]any, error) {
	return "", nil, fmt.Errorf("bootstrap: tool call %q is not resolvable in this build", toolCallID)
}

// buildWorkerManager 按配置启动 Worker 池。
//
// 只注册配置里显式列出的 kind，且每个 kind 的工厂都从各自子包取现成的
// NewWorkerFromSpec（mcp 包的工厂名叫 NewFromSpec）。所有高危域的工厂默认
// fail-closed（例如 terminal 的 AllowRoots 为空即拒绝、computer-use 默认
// Enabled=false），因此这里不做额外放开。
func buildWorkerManager(cfg *config.Config, opts Options) (*worker.Manager, error) {
	if len(cfg.Runtime.WorkerPools) == 0 {
		return nil, nil
	}

	root, err := resolveWorkspaceRoot(cfg, opts)
	if err != nil {
		return nil, err
	}

	mgr := worker.NewManager(worker.ManagerConfig{})
	registered := 0
	for kind, size := range cfg.Runtime.WorkerPools {
		if size <= 0 {
			continue
		}
		factory, ok := workerFactoryFor(kind)
		if !ok {
			observability.LogWarn(context.Background(),
				"unknown worker kind in config, skipping pool", map[string]any{"kind": kind})
			continue
		}
		spec := worker.PoolSpec{
			Kind:    kind,
			Size:    size,
			Factory: factory,
			Config:  workerPoolConfig(cfg, kind, root),
		}
		if err := mgr.RegisterPool(spec); err != nil {
			return nil, fmt.Errorf("register pool %q: %w", kind, err)
		}
		registered++
	}
	if registered == 0 {
		return nil, nil
	}
	if err := mgr.Start(context.Background()); err != nil {
		return nil, fmt.Errorf("start worker manager: %w", err)
	}
	return mgr, nil
}

// resolveWorkspaceRoot 解析工具沙箱的根目录。
//
// 退化顺序：显式 Options > 配置文件 > 进程工作目录。绝不退化成 "/" 或盘符根：
// 那会让 file 工具与所有 Worker 拥有整块磁盘的访问权限。
func resolveWorkspaceRoot(cfg *config.Config, opts Options) (string, error) {
	root := opts.WorkspaceRoot
	if root == "" {
		root = cfg.Runtime.WorkspaceRoot
	}
	if root == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("resolve workspace root: %w", err)
		}
		root = wd
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve absolute workspace root: %w", err)
	}
	return abs, nil
}

// workerPoolConfig 构造某个 kind 的 Worker 静态配置。
//
// 这里的每个 key 都是对应 Worker 的 fail-closed 边界，缺一个就整类能力不可用：
//   - terminal / office / dynamic-js：allowed_roots 为空 ⇒ 一切路径操作被拒
//     （terminal 连命令都过不了 resolveWorkdir，每条都会返回 no_allowed_roots）；
//   - mcp：不给 servers ⇒ 0 个会话，tools/list 恒为空。
//
// 因此必须把工作区根注入进来，否则会出现「池起来了、工具也注册了，
// 但每次调用都被策略拒绝」的空转状态。
func workerPoolConfig(cfg *config.Config, kind, workspaceRoot string) map[string]any {
	conf := map[string]any{
		"allowed_roots": []string{workspaceRoot},
	}
	switch kind {
	case "mcp":
		if len(cfg.MCPServers) > 0 {
			servers := make([]map[string]any, 0, len(cfg.MCPServers))
			for _, s := range cfg.MCPServers {
				servers = append(servers, s.ToWorkerMap())
			}
			conf["servers"] = servers
		}
	case "browser":
		// 显式写出无头模式，不依赖 NewWorkerFromSpec 的隐式默认值。
		conf["headless"] = true
	}
	return conf
}

// workerFactoryFor 返回指定 kind 的 Worker 工厂。
func workerFactoryFor(kind string) (worker.Factory, bool) {
	switch kind {
	case "browser":
		return browser.NewWorkerFromSpec, true
	case "dynamic-js":
		return dynamic.NewWorkerFromSpec, true
	case "mcp":
		return mcp.NewFromSpec, true
	case "terminal":
		return terminal.NewWorkerFromSpec, true
	case "office":
		return office.NewWorkerFromSpec, true
	case "computer-use":
		return computeruse.NewWorkerFromSpec, true
	default:
		return nil, false
	}
}

// buildEngineConfig 把外部配置映射为引擎配置，未填项沿用 types 的默认值。
func buildEngineConfig(cfg *config.Config) engine.EngineConfig {
	ec := engine.DefaultEngineConfig()
	if cfg.Runtime.MaxRunningTasks > 0 {
		ec.Scheduler.MaxRunningTasks = cfg.Runtime.MaxRunningTasks
	}
	if cfg.Runtime.MaxToolRounds > 0 {
		ec.Agent.MaxToolRounds = cfg.Runtime.MaxToolRounds
	}
	if cfg.Provider.ContextWindow > 0 {
		ec.Agent.ContextWindow = cfg.Provider.ContextWindow
	}
	if cfg.Paths.CrashDumpDir != "" {
		ec.CrashDumpDir = cfg.Paths.CrashDumpDir
	}
	return ec
}

// autoModeOrDefault 返回配置中的权限姿态，缺省为 safe。
func autoModeOrDefault(cfg *config.Config) string {
	if cfg.Runtime.AutoMode != "" {
		return cfg.Runtime.AutoMode
	}
	return "safe"
}
