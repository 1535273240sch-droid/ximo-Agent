package config

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Feature Flag 标识常量（第40章）
const (
	FlagEngineV2        = "engine.v2"
	FlagWorkerBrowser   = "worker.browser"
	FlagWorkerDynamicJS = "worker.dynamic_js"
	FlagWorkerMCP       = "worker.mcp"
	FlagCheckpointCAS   = "checkpoint.cas"
	FlagContextV2       = "context.v2"
	FlagSchedulerV2     = "scheduler.v2"
	// FlagMemory 是长期记忆（mem0）的总开关。它与配置段的 memory.enabled 是
	// 「与」的关系：特性开关用于整机/整环境熔断，配置段用于用户级开关。
	FlagMemory = "memory.mem0"
)

// DefaultFeatureFlags 返回默认开启的特性开关
func DefaultFeatureFlags() map[string]bool {
	return map[string]bool{
		FlagEngineV2:        true,
		FlagWorkerBrowser:   true,
		FlagWorkerDynamicJS: true,
		FlagWorkerMCP:       true,
		FlagCheckpointCAS:   true,
		FlagContextV2:       true,
		FlagSchedulerV2:     true,
		// 长期记忆默认关闭：它依赖一个外部服务（mem0）与一份额外密钥，在用户
		// 显式配置之前不该改变任何行为。这也是 DefaultFeatureFlags 里唯一的
		// 例外，所以单独说明。
		FlagMemory: false,
	}
}

// FeatureManager 管理系统运行时 Feature Flags，支持动态修改与监听
type FeatureManager struct {
	mu       sync.RWMutex
	flags    map[string]bool
	watchers []func(flag string, enabled bool)
}

// NewFeatureManager 创建特性管理器并初始化
func NewFeatureManager(initial map[string]bool) *FeatureManager {
	fm := &FeatureManager{
		flags: make(map[string]bool),
	}
	defaults := DefaultFeatureFlags()
	for k, v := range defaults {
		fm.flags[k] = v
	}
	for k, v := range initial {
		fm.flags[k] = v
	}
	fm.applyEnvOverrides()
	return fm
}

func (fm *FeatureManager) applyEnvOverrides() {
	envPrefix := "XIMO_FEATURE_"
	for _, env := range os.Environ() {
		if strings.HasPrefix(env, envPrefix) {
			parts := strings.SplitN(env, "=", 2)
			if len(parts) != 2 {
				continue
			}
			key := parts[0][len(envPrefix):]
			val := strings.ToLower(strings.TrimSpace(parts[1]))
			// 转换 XIMO_FEATURE_ENGINE_V2 -> engine.v2
			flagName := strings.ToLower(strings.ReplaceAll(key, "_", "."))
			enabled := val == "1" || val == "true" || val == "yes" || val == "on"
			fm.flags[flagName] = enabled
		}
	}
}

// IsEnabled 查询指定 flag 是否开启
func (fm *FeatureManager) IsEnabled(flag string) bool {
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	return fm.flags[flag]
}

// Set 动态修改指定 flag 的状态，并通知所有注册的监听器
func (fm *FeatureManager) Set(flag string, enabled bool) {
	fm.mu.Lock()
	old, exists := fm.flags[flag]
	fm.flags[flag] = enabled
	watchers := make([]func(flag string, enabled bool), len(fm.watchers))
	copy(watchers, fm.watchers)
	fm.mu.Unlock()

	if !exists || old != enabled {
		for _, w := range watchers {
			w(flag, enabled)
		}
	}
}

// RegisterWatcher 注册 flag 变更监听回调
func (fm *FeatureManager) RegisterWatcher(watcher func(flag string, enabled bool)) {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	fm.watchers = append(fm.watchers, watcher)
}

// AllSnapshot 返回当前所有 Feature Flag 快照
func (fm *FeatureManager) AllSnapshot() map[string]bool {
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	res := make(map[string]bool, len(fm.flags))
	for k, v := range fm.flags {
		res[k] = v
	}
	return res
}

// HealthStatus 版本健康状态
type HealthStatus string

const (
	HealthStatusHealthy   HealthStatus = "healthy"
	HealthStatusUnhealthy HealthStatus = "unhealthy"
	HealthStatusPending   HealthStatus = "pending"
)

// VersionMeta 版本元数据
type VersionMeta struct {
	Version      string       `json:"version"`
	GitCommit    string       `json:"git_commit,omitempty"`
	BuildTime    string       `json:"build_time,omitempty"`
	HealthStatus HealthStatus `json:"health_status"`
	InstalledAt  time.Time    `json:"installed_at"`
}

// HealthCheckHook 健康检查钩子函数契约（由任务07升级脚本挂载）
type HealthCheckHook func(ctx context.Context, versionDir string) error

// PathsConfig 统一定义系统路径，包含双版本（current/previous 指针与实体目录）约定，完全对齐任务07
type PathsConfig struct {
	BaseDir             string `json:"base_dir"`
	VersionsDir         string `json:"versions_dir"`
	CurrentPointerFile  string `json:"current_pointer_file"`  // 对齐07: <Base>/current 文本指针文件
	PreviousPointerFile string `json:"previous_pointer_file"` // 对齐07: <Base>/previous 文本指针文件
	CurrentVersionDir   string `json:"current_version_dir"`   // 兼容字段: <Base>/versions/current
	PreviousVersionDir  string `json:"previous_version_dir"`  // 兼容字段: <Base>/versions/previous
	DataDir             string `json:"data_dir"`
	LogsDir             string `json:"logs_dir"`
	CrashDumpDir        string `json:"crash_dump_dir"`
	RunDir              string `json:"run_dir"`
}

// DefaultPaths 获取系统默认路径配置
func DefaultPaths() PathsConfig {
	var baseDir string
	if custom := os.Getenv("XIMO_HOME"); custom != "" {
		baseDir = custom
	} else {
		switch runtime.GOOS {
		case "windows":
			appData := os.Getenv("APPDATA")
			if appData == "" {
				appData = filepath.Join(os.Getenv("USERPROFILE"), "AppData", "Roaming")
			}
			baseDir = filepath.Join(appData, "ximo-agent")
		case "darwin":
			home, _ := os.UserHomeDir()
			baseDir = filepath.Join(home, "Library", "Application Support", "ximo-agent")
		default:
			// linux / other unix
			if xdgConfig := os.Getenv("XDG_CONFIG_HOME"); xdgConfig != "" {
				baseDir = filepath.Join(xdgConfig, "ximo-agent")
			} else {
				home, _ := os.UserHomeDir()
				baseDir = filepath.Join(home, ".config", "ximo-agent")
			}
		}
	}

	versionsDir := filepath.Join(baseDir, "versions")
	dataDir := filepath.Join(baseDir, "data")

	return PathsConfig{
		BaseDir:             baseDir,
		VersionsDir:         versionsDir,
		CurrentPointerFile:  filepath.Join(baseDir, "current"),
		PreviousPointerFile: filepath.Join(baseDir, "previous"),
		CurrentVersionDir:   filepath.Join(versionsDir, "current"),
		PreviousVersionDir:  filepath.Join(versionsDir, "previous"),
		DataDir:             dataDir,
		LogsDir:             filepath.Join(dataDir, "logs"),
		CrashDumpDir:        filepath.Join(dataDir, "crashes"),
		RunDir:              filepath.Join(dataDir, "run"),
	}
}

// EnsureDirectories 创建基础目录结构
func (p *PathsConfig) EnsureDirectories() error {
	dirs := []string{
		p.BaseDir,
		p.VersionsDir,
		p.DataDir,
		p.LogsDir,
		p.CrashDumpDir,
		p.RunDir,
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0755); err != nil {
			return fmt.Errorf("failed to create directory %s: %w", d, err)
		}
	}
	return nil
}

// VersionManager 提供双版本安装与健康检查管理（发布双版本策略）
type VersionManager struct {
	paths           PathsConfig
	mu              sync.RWMutex
	healthCheckHook HealthCheckHook
}

// NewVersionManager 创建版本管理器
func NewVersionManager(paths PathsConfig) *VersionManager {
	return &VersionManager{
		paths: paths,
	}
}

// SetHealthCheckHook 注册健康检查钩子
func (vm *VersionManager) SetHealthCheckHook(hook HealthCheckHook) {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	vm.healthCheckHook = hook
}

// ResolveCurrentVersionDir 解析当前真实版本目录路径（优先读 current 指针文件，不存在则回退至兼容目录）
func (vm *VersionManager) ResolveCurrentVersionDir() (string, error) {
	if vm.paths.CurrentPointerFile != "" {
		if data, err := os.ReadFile(vm.paths.CurrentPointerFile); err == nil {
			target := strings.TrimSpace(string(data))
			if target != "" {
				return target, nil
			}
		}
	}
	if _, err := os.Stat(vm.paths.CurrentVersionDir); err == nil {
		return vm.paths.CurrentVersionDir, nil
	}
	return "", fmt.Errorf("current version directory not found")
}

// ResolvePreviousVersionDir 解析上一真实版本目录路径
func (vm *VersionManager) ResolvePreviousVersionDir() (string, error) {
	if vm.paths.PreviousPointerFile != "" {
		if data, err := os.ReadFile(vm.paths.PreviousPointerFile); err == nil {
			target := strings.TrimSpace(string(data))
			if target != "" {
				return target, nil
			}
		}
	}
	if _, err := os.Stat(vm.paths.PreviousVersionDir); err == nil {
		return vm.paths.PreviousVersionDir, nil
	}
	return "", fmt.Errorf("previous version directory not found")
}

// GetCurrentVersion 获取当前版本信息
func (vm *VersionManager) GetCurrentVersion() (*VersionMeta, error) {
	curDir, err := vm.ResolveCurrentVersionDir()
	if err != nil {
		return nil, err
	}
	metaFile := filepath.Join(curDir, "version.json")
	return vm.readVersionMeta(metaFile)
}

// GetPreviousVersion 获取上一版本信息
func (vm *VersionManager) GetPreviousVersion() (*VersionMeta, error) {
	prevDir, err := vm.ResolvePreviousVersionDir()
	if err != nil {
		return nil, err
	}
	metaFile := filepath.Join(prevDir, "version.json")
	return vm.readVersionMeta(metaFile)
}

// VerifyHealth 执行当前版本的健康检查钩子
func (vm *VersionManager) VerifyHealth(ctx context.Context) error {
	vm.mu.RLock()
	hook := vm.healthCheckHook
	vm.mu.RUnlock()

	curDir, err := vm.ResolveCurrentVersionDir()
	if err != nil {
		return fmt.Errorf("verify health failed: %w", err)
	}

	if hook == nil {
		return vm.defaultHealthCheck(ctx, curDir)
	}
	return hook(ctx, curDir)
}

func (vm *VersionManager) defaultHealthCheck(_ context.Context, dir string) error {
	if _, err := os.Stat(dir); err != nil {
		return fmt.Errorf("version directory inaccessible: %w", err)
	}
	return nil
}

func (vm *VersionManager) readVersionMeta(filePath string) (*VersionMeta, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("read version meta %s failed: %w", filePath, err)
	}
	var meta VersionMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("unmarshal version meta failed: %w", err)
	}
	return &meta, nil
}

// WriteVersionMeta 写入指定版本的元数据
func (vm *VersionManager) WriteVersionMeta(dir string, meta *VersionMeta) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "version.json"), data, 0644)
}

// PromoteToCurrent 将新版本安装目录提升为 current（完全对齐 07 updater 文本指针与原子切换逻辑）
func (vm *VersionManager) PromoteToCurrent(newVersionDir string, meta *VersionMeta) error {
	vm.mu.Lock()
	defer vm.mu.Unlock()

	_ = vm.paths.EnsureDirectories()

	if meta != nil {
		if err := vm.WriteVersionMeta(newVersionDir, meta); err != nil {
			return fmt.Errorf("write current version meta failed: %w", err)
		}
	}

	// 1. 将现有的 current 指针备份为 previous
	if currTarget, err := os.ReadFile(vm.paths.CurrentPointerFile); err == nil && len(currTarget) > 0 {
		_ = os.WriteFile(vm.paths.PreviousPointerFile, currTarget, 0644)
	} else if _, err := os.Stat(vm.paths.CurrentVersionDir); err == nil {
		_ = os.WriteFile(vm.paths.PreviousPointerFile, []byte(vm.paths.CurrentVersionDir), 0644)
	}

	// 2. 将 current 指针指向新版本目录绝对路径
	absTarget, err := filepath.Abs(newVersionDir)
	if err != nil {
		absTarget = newVersionDir
	}
	if err := os.WriteFile(vm.paths.CurrentPointerFile, []byte(absTarget), 0644); err != nil {
		return fmt.Errorf("write current pointer file failed: %w", err)
	}

	return nil
}

// RollbackToPrevious 回滚到上一稳定版本（完全对齐 07 的指针回滚）
func (vm *VersionManager) RollbackToPrevious() error {
	vm.mu.Lock()
	defer vm.mu.Unlock()

	prevTarget, err := os.ReadFile(vm.paths.PreviousPointerFile)
	if err != nil || len(prevTarget) == 0 {
		// 尝试回退至兼容目录
		if _, errStat := os.Stat(vm.paths.PreviousVersionDir); errStat == nil {
			prevTarget = []byte(vm.paths.PreviousVersionDir)
		} else {
			return fmt.Errorf("previous version pointer not found for rollback")
		}
	}

	targetDir := strings.TrimSpace(string(prevTarget))
	if _, err := os.Stat(targetDir); err != nil {
		return fmt.Errorf("previous target directory %s inaccessible: %w", targetDir, err)
	}

	if err := os.WriteFile(vm.paths.CurrentPointerFile, []byte(targetDir), 0644); err != nil {
		return fmt.Errorf("failed to revert current pointer: %w", err)
	}

	return nil
}

// SupervisorConfig 包含监管者核心参数配置
type SupervisorConfig struct {
	HeartbeatInterval       time.Duration `json:"heartbeat_interval"`
	HeartbeatTimeout        time.Duration `json:"heartbeat_timeout"`
	GracefulShutdownTimeout time.Duration `json:"graceful_shutdown_timeout"`
	MaxRestartRetries       int           `json:"max_restart_retries"`
	RestartBackoffSteps     []int         `json:"restart_backoff_steps"` // 单位: 秒 [1, 2, 4, 8, 16, 30]
	EnableProcessTreeKill   bool          `json:"enable_process_tree_kill"`
}

// DefaultSupervisorConfig 返回默认 Supervisor 配置
func DefaultSupervisorConfig() SupervisorConfig {
	return SupervisorConfig{
		HeartbeatInterval:       2 * time.Second,
		HeartbeatTimeout:        6 * time.Second,
		GracefulShutdownTimeout: 30 * time.Second,
		MaxRestartRetries:       10,
		RestartBackoffSteps:     []int{1, 2, 4, 8, 16, 30},
		EnableProcessTreeKill:   true,
	}
}

// IPCConfig IPC 通信配置
type IPCConfig struct {
	PipeName         string        `json:"pipe_name"`
	SocketPath       string        `json:"socket_path"`
	DefaultTimeout   time.Duration `json:"default_timeout"`
	MaxPayloadLength uint32        `json:"max_payload_length"`
	ReadBufferSize   int           `json:"read_buffer_size"`
}

// DefaultIPCConfig 返回默认 IPC 配置
func DefaultIPCConfig() IPCConfig {
	return IPCConfig{
		PipeName:         `\\.\pipe\ximo-agent-ipc`,
		SocketPath:       `/tmp/ximo-agent-ipc.sock`,
		DefaultTimeout:   15 * time.Second,
		MaxPayloadLength: 16 * 1024 * 1024, // 16MB limit
		ReadBufferSize:   64 * 1024,        // 64KB
	}
}

// ProviderConfig 是模型服务商（第18/22章）的运行时配置。
//
// SecretRef 是 secrets 管理器中的密钥引用（形如 "dpapi:ximo"），绝不是
// 明文 API Key：明文只允许出现在 secrets 管理器内部，配置文件里只留存引用，
// 这是第21章的脱敏红线。
type ProviderConfig struct {
	ID              string        `json:"id"`       // 服务商标识，缺省 custom（第三方 OpenAI 兼容接口）
	Name            string        `json:"name"`     // 展示名
	BaseURL         string        `json:"base_url"` // 第三方接口地址，例如 https://api.openai.com/v1
	Model           string        `json:"model"`    // 模型名，例如 gpt-4o
	SecretRef       string        `json:"secret_ref"`
	ContextWindow   int           `json:"context_window"`
	MaxOutputTokens int           `json:"max_output_tokens"`
	RateLimitPerSec float64       `json:"rate_limit_per_sec"` // <=0 表示不限流
	Timeout         time.Duration `json:"timeout"`
}

// DefaultProviderConfig 返回第三方 OpenAI 兼容接口的默认值。
//
// 本产品不内置任何具体厂商：默认值只是 OpenAI 兼容协议的中性基准，
// Base URL / 模型名均可在设置界面改为任意第三方接口，且会持久化到 config.json。
// BaseURL 不能为空——Provider 客户端在装配期要求非空，否则引擎无法启动。
func DefaultProviderConfig() ProviderConfig {
	return ProviderConfig{
		ID:              "custom",
		Name:            "第三方接口",
		BaseURL:         "https://api.openai.com/v1",
		Model:           "gpt-4o",
		ContextWindow:   131_072,
		MaxOutputTokens: 8192,
		Timeout:         5 * time.Minute,
	}
}

// StorageConfig 是本地持久化（第9/10章）的配置。
type StorageConfig struct {
	// DBPath 是 SQLite 主库文件路径；留空时落在 <DataDir>/ximo-agent.db。
	DBPath string `json:"db_path"`
	// Profile 选择 SQLite 调优档位：safe | balanced | performance。
	Profile string `json:"profile"`
}

// DefaultStorageConfig 返回 balanced 档的默认配置。
func DefaultStorageConfig() StorageConfig {
	return StorageConfig{Profile: "balanced"}
}

// RuntimeConfig 汇总引擎运行时（第5/6/22章）的可调参数。
//
// 零值会被 types.DefaultEngineConfig() 覆盖，因此只需填写想改的项。
type RuntimeConfig struct {
	MaxRecoveryAttempts int           `json:"max_recovery_attempts"`
	MaxRunningTasks     int           `json:"max_running_tasks"`
	MaxToolRounds       int           `json:"max_tool_rounds"`
	ToolExecTimeout     time.Duration `json:"tool_exec_timeout"`
	// AutoMode 是权限姿态：yolo | safe | coding | office | design。
	AutoMode string `json:"auto_mode"`
	// WorkspaceRoot 是工具可访问的项目根目录（file/git 域沙箱边界）。
	// 为空时退化为进程当前目录，绝不退化为整个文件系统。
	WorkspaceRoot string `json:"workspace_root"`
	// WorkerPools 按 kind 配置各类 Worker 池的槽位数量，
	// 例如 {"terminal": 2, "browser": 1}。未列出的 kind 不启动。
	WorkerPools map[string]int `json:"worker_pools"`
}

// DefaultRuntimeConfig 返回安全的默认运行时配置。
func DefaultRuntimeConfig() RuntimeConfig {
	return RuntimeConfig{
		MaxRecoveryAttempts: 3,
		AutoMode:            "safe",
		// 默认启用不依赖额外外部二进制的 Worker 池，让 Agent 开箱即有工具可用。
		// office 需要 officecli、computer-use 需要 pi-helper，缺失时其 Worker
		// 会在启动后 health 失败并熔断，因此默认不预置，由用户按需在配置里开启。
		// 这不影响安全：各高危域自身都是 fail-closed 的（allowed_roots 为空、
		// enabled=false 即拒绝执行），默认启动池不等于放开权限。
		WorkerPools: map[string]int{
			"terminal":   2,
			"browser":    1,
			"mcp":        1,
			"dynamic-js": 1,
		},
	}
}

// MCPServerConfig 描述一个 MCP 服务器。
//
// 字段与 worker/mcp.ServerConfig 对齐，但刻意不复用该类型：config 是被所有层
// 依赖的最底层包，反向依赖 worker 会形成循环。类型转换在 bootstrap 层完成。
type MCPServerConfig struct {
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Transport string `json:"transport,omitempty"`
	// Enabled 用指针区分「未写」与「显式 false」：未写时视为启用（列出即想用），
	// 只有显式写 false 才跳过。worker 侧的 ServerConfig.Enabled 是普通 bool 且
	// 默认 false，若这里直接透传零值，用户写的每个服务器都会被静默跳过。
	Enabled *bool `json:"enabled,omitempty"`
	// stdio 传输
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Cwd     string            `json:"cwd,omitempty"`
	// http / sse 传输
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	// 工具收敛：DeniedTools 中的工具永不暴露；AllowedTools 非空时只暴露其中工具。
	AllowedTools []string `json:"allowed_tools,omitempty"`
	DeniedTools  []string `json:"denied_tools,omitempty"`
}

// ToWorkerMap 把配置转成 worker/mcp.ServerConfig 可反序列化的形态。
// 需要它是因为 Spec.Config 是 map[string]any，跨包时不保留 Go 类型信息。
func (c MCPServerConfig) ToWorkerMap() map[string]any {
	enabled := true
	if c.Enabled != nil {
		enabled = *c.Enabled
	}
	m := map[string]any{
		"id":      c.ID,
		"name":    c.Name,
		"enabled": enabled,
	}
	if c.Transport != "" {
		m["transport"] = c.Transport
	}
	if c.Command != "" {
		m["command"] = c.Command
	}
	if len(c.Args) > 0 {
		m["args"] = c.Args
	}
	if len(c.Env) > 0 {
		m["env"] = c.Env
	}
	if c.Cwd != "" {
		m["cwd"] = c.Cwd
	}
	if c.URL != "" {
		m["url"] = c.URL
	}
	if len(c.Headers) > 0 {
		m["headers"] = c.Headers
	}
	if len(c.AllowedTools) > 0 {
		m["allowed_tools"] = c.AllowedTools
	}
	if len(c.DeniedTools) > 0 {
		m["denied_tools"] = c.DeniedTools
	}
	return m
}

// SubAgentConfig 子代理模型分配（任务 5 设置面板的持久化形态）。
//
// 候选用服务商 ID 表示 —— 每个服务商配置自带模型名，所以「给专家/分类配
// 多个候选模型」就是给它们配一个有序的服务商 ID 列表。
type SubAgentConfig struct {
	// Pool 全局子代理池的候选顺序；为空表示使用整个服务商池的配置顺序。
	Pool []string `json:"pool,omitempty"`
	// ByDivision 按专家分类覆盖候选顺序（键 = 专家的 division 名）。
	ByDivision map[string][]string `json:"by_division,omitempty"`
	// ByExpert 按专家 ID 覆盖候选顺序，优先级高于分类。
	ByExpert map[string][]string `json:"by_expert,omitempty"`
}

// Config 全局配置对象
type Config struct {
	Supervisor   SupervisorConfig `json:"supervisor"`
	IPC          IPCConfig        `json:"ipc"`
	Paths        PathsConfig      `json:"paths"`
	Provider     ProviderConfig   `json:"provider"`
	Storage      StorageConfig    `json:"storage"`
	Runtime      RuntimeConfig    `json:"runtime"`
	FeatureFlags map[string]bool  `json:"feature_flags"`
	// MCPServers 是挂载的 MCP 服务器清单。引擎启动时据此拉起 MCP Worker 池，
	// 并把服务器暴露的工具桥接给 Agent（见 bootstrap/worker_tools.go）。
	MCPServers []MCPServerConfig `json:"mcp_servers,omitempty"`

	// Providers 是子代理模型池的额外候选（任务 5）。
	//
	// 语义：主服务商永远是 Provider（优先级最高），Providers 列出其余候选，
	// 失败转移按 ProviderPool() 的顺序轮询。主服务商单独一个字段而不是把
	// 整个池塞进一个数组，是因为老配置文件只有 `provider` 键 —— 保留它，
	// 老用户的 config.json 一行都不用改就能继续加载（验收红线）。
	Providers []ProviderConfig `json:"providers,omitempty"`
	// SubAgent 是子代理模型分配配置（任务 5 设置面板写入）。
	SubAgent SubAgentConfig `json:"sub_agent,omitempty"`
	// Memory 是长期记忆（mem0）配置。零值时整段为空，行为与本特性不存在时
	// 完全一致——老配置文件不含该键也照常加载。
	Memory MemoryConfig `json:"memory,omitempty"`
}

// ResolveDBPath 返回 SQLite 主库的最终路径：显式配置优先，否则落在 DataDir。
func (c *Config) ResolveDBPath() string {
	if c.Storage.DBPath != "" {
		return c.Storage.DBPath
	}
	return filepath.Join(c.Paths.DataDir, "ximo-agent.db")
}

// ProviderPool 返回生效的服务商候选池：主服务商恒为第一项（优先级最高），
// 其后依次是 Providers 里的候选。同一 ID 只保留主字段里那份，避免两个视图
// 各改各的之后池里出现两条互相矛盾的同一服务商。
//
// 顺序即优先级 —— 失败转移与轮询都按这个顺序进行。
func (c *Config) ProviderPool() []ProviderConfig {
	pool := make([]ProviderConfig, 0, 1+len(c.Providers))
	pool = append(pool, c.Provider)
	seen := map[string]bool{c.Provider.ID: true}
	for _, p := range c.Providers {
		if p.ID != "" && seen[p.ID] {
			continue
		}
		if p.ID != "" {
			seen[p.ID] = true
		}
		pool = append(pool, p)
	}
	return pool
}

// SubAgentCandidates 返回某位专家应使用的候选服务商 ID 顺序（优先级从高到低）：
// 专家 ID 覆盖 > 专家分类覆盖 > 全局子代理池 > 整个服务商池的配置顺序。
//
// 返回的 ID 只是「希望用谁」的请求；候选是否真的可用由 provider.Pool 在
// 选路时按熔断状态裁决，这里不做健康检查（config 层没有那个信息）。
func (c *Config) SubAgentCandidates(expertID, division string) []string {
	if order := c.SubAgent.ByExpert[expertID]; len(order) > 0 {
		return order
	}
	if division != "" {
		if order := c.SubAgent.ByDivision[division]; len(order) > 0 {
			return order
		}
	}
	if len(c.SubAgent.Pool) > 0 {
		return c.SubAgent.Pool
	}
	return nil
}

// normalizeProviders 清理池候选与主服务商的重复项（同一 ID 以主字段为准），
// 让落盘的配置文件不出现两个视图互相矛盾的同一服务商。
func (c *Config) normalizeProviders() {
	if len(c.Providers) == 0 {
		return
	}
	kept := c.Providers[:0]
	for _, p := range c.Providers {
		if p.ID != "" && p.ID == c.Provider.ID {
			continue
		}
		kept = append(kept, p)
	}
	c.Providers = kept
}

// UnmarshalJSON 兼容三种服务商写法 —— 这是任务 5 的向后兼容要求，老用户的
// config.json 必须一行不改就能加载：
//
//	{"provider": {…}}            老配置：单服务商（主服务商）
//	{"providers": [{…}, …]}      新配置：候选池
//	{"provider": [{…}, …]}       手写列表：同样视为候选池，首项即主服务商
//
// 除 provider/providers 之外的键沿用标准解码；这里刻意保留「键缺失不改现有
// 值」的合并语义，与 LoadConfig「先填默认值再叠加文件」的约定一致。
func (c *Config) UnmarshalJSON(data []byte) error {
	// plain 是无方法的别名，避免解码时递归调用本方法。
	type plain Config
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	var (
		singleRaw json.RawMessage
		fromArray []ProviderConfig
		extras    []ProviderConfig
	)
	if v, ok := raw["provider"]; ok {
		delete(raw, "provider")
		trimmed := bytes.TrimSpace(v)
		if len(trimmed) > 0 && trimmed[0] == '[' {
			if err := json.Unmarshal(trimmed, &fromArray); err != nil {
				return fmt.Errorf("provider: %w", err)
			}
		} else {
			singleRaw = trimmed
		}
	}
	if v, ok := raw["providers"]; ok {
		delete(raw, "providers")
		if err := json.Unmarshal(v, &extras); err != nil {
			return fmt.Errorf("providers: %w", err)
		}
	}

	rest, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(rest, (*plain)(c)); err != nil {
		return err
	}

	// provider 键给了数组：首项是主服务商，其余并入候选池。
	if len(fromArray) > 0 {
		if data, mErr := json.Marshal(fromArray[0]); mErr == nil {
			singleRaw = data
		}
		extras = append(fromArray[1:], extras...)
	}

	// 主服务商对象按「合并」处理：JSON 里没写的字段保留加载前的值，
	// 与其它键（以及 LoadConfig 先默认值后叠加）的解码语义一致。
	if len(singleRaw) > 0 {
		merged := c.Provider
		if err := json.Unmarshal(singleRaw, &merged); err != nil {
			return fmt.Errorf("provider: %w", err)
		}
		c.Provider = merged
	} else if len(extras) > 0 {
		// 只写了候选池：主服务商取池首项，避免默认值被误当成主服务商混进池里。
		c.Provider = extras[0]
		extras = extras[1:]
	}
	c.Providers = extras
	c.normalizeProviders()
	return nil
}

// NewDefaultConfig 创建带有合理默认值的配置
func NewDefaultConfig() *Config {
	paths := DefaultPaths()
	ipc := DefaultIPCConfig()
	// Unix下将 SocketPath 放置在 RunDir 目录下
	if runtime.GOOS != "windows" {
		ipc.SocketPath = filepath.Join(paths.RunDir, "ximo-agent-ipc.sock")
	}

	return &Config{
		Supervisor:   DefaultSupervisorConfig(),
		IPC:          ipc,
		Paths:        paths,
		Provider:     DefaultProviderConfig(),
		Storage:      DefaultStorageConfig(),
		Runtime:      DefaultRuntimeConfig(),
		FeatureFlags: DefaultFeatureFlags(),
	}
}

// LoadConfig 从指定 JSON 文件加载配置
func LoadConfig(filePath string) (*Config, error) {
	cfg := NewDefaultConfig()
	if filePath == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, fmt.Errorf("read config file failed: %w", err)
	}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("unmarshal config failed: %w", err)
	}
	// 老配置可能只写单个 provider：迁移逻辑保证候选池至少含主服务商这一项，
	// 上层（模型池）无需再区分两种写法。
	cfg.normalizeProviders()
	return cfg, nil
}

// SaveConfig 将配置持久化至指定文件
func (c *Config) SaveConfig(filePath string) error {
	dir := filepath.Dir(filePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create config dir failed: %w", err)
	}
	c.normalizeProviders()
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config failed: %w", err)
	}
	return os.WriteFile(filePath, data, 0644)
}

// DefaultConfigPath 返回配置文件的约定位置：<BaseDir>/config.json。
//
// 设置界面保存的配置就落在这里。进程启动时若未显式传 --config，应当
// 自动加载它——否则用户保存的服务商设置只存在于旧进程内存，一重启就
// "丢失"，引擎永远拿着出厂默认值去调模型。
func DefaultConfigPath() string {
	return filepath.Join(DefaultPaths().BaseDir, "config.json")
}
