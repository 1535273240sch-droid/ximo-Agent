package agents

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Config 是 ApplyXimoAgent 的输入。字段命名与 engine.Inputs 对齐，便于 CLI
// 直接把命令行的值搬过来。
type Config struct {
	// GatewayURL 要写进 provider.base_url 的中转站地址（必填）。
	// 会按 ximo-agent 自己的规则规整：补 https://（缺协议头时）、必须 http(s)、去尾斜杠。
	GatewayURL string
	// Model 是模型名。留空表示不改动 agent 当前的模型。
	Model string
	// APIKey 是凭据明文。它只会经 system.secret.put 进平台安全存储，
	// **绝不**写进配置文件、日志或 audit。
	APIKey string
	// ProviderID / DisplayName 可选，非空时覆盖 agent 的 provider_id / provider_name
	// （这两项在 agent 侧是无条件覆盖的，不传就沿用现值）。
	ProviderID  string
	DisplayName string

	// Endpoint 显式指定 IPC 端点（CLI --ipc-endpoint）。空则按
	// XIMO_IPC_ENDPOINT → 平台默认 的顺序发现。
	Endpoint string
	// ConfigPath 覆盖 ximo-agent 的配置文件路径。空则用 <BaseDir>/config.json。
	ConfigPath string
	// Home 覆盖用户主目录（CLI --home，测试用）。非空时不再读 APPDATA 等环境变量，
	// 保证同一 --home 在不同机器上算出同一个 BaseDir。
	Home string

	// DryRun 只打印将要发生的变更：不写文件、不写安全存储、不发 config.set。
	DryRun bool
	// NoIPC 跳过 IPC，直接走配置文件回退路径（agent 未运行、或调试时用）。
	NoIPC bool
	// NoFileFallback 禁止在 IPC 连不上时回退到改配置文件。默认允许回退，
	// 但回退一定会在输出里告警。
	NoFileFallback bool

	// Timeout 单次 IPC 请求超时，0 时用 DefaultTimeout。
	Timeout time.Duration
	// FileIO 注入配置读写实现（契约签名，可传 internal/formats 的四个函数）。
	// 为 nil 时使用自带的 JSON 实现 LocalDocIO。
	FileIO DocIO
	// Output 接收人类可读的进度与告警；为 nil 时丢弃。凭据明文永不写入它。
	Output io.Writer
}

func (c Config) timeout() time.Duration {
	if c.Timeout <= 0 {
		return DefaultTimeout
	}
	return c.Timeout
}

func (c Config) out() io.Writer {
	if c.Output == nil {
		return io.Discard
	}
	return c.Output
}

func (c Config) docIO() DocIO {
	if c.FileIO == nil {
		return LocalDocIO{}
	}
	return c.FileIO
}

// connectError 标记「连不上 ximo-agent」。只有这一类错误才允许回退到改文件：
// agent 明确回了错误（例如安全存储不可用）时改文件只会掩盖真实问题。
type connectError struct{ err error }

func (e *connectError) Error() string { return e.err.Error() }
func (e *connectError) Unwrap() error { return e.err }

// ApplyXimoAgent 把中转站写进 ximo-agent 的配置。
//
// 流程（契约 §3）：
//
//	读现状（system.config.get）→ 备份配置文件 → system.secret.put 拿 secret_ref
//	→ system.config.set 写回 provider 段（base_url=网关、model、secret_ref）
//
// 端点发现顺序：显式 Endpoint → XIMO_IPC_ENDPOINT → 平台默认
// （Windows 命名管道 `\\.\pipe\ximo-agent-ipc`）。
//
// IPC 优先而不是先改文件，是因为 ximo-agent 的密钥只能经 IPC 进平台安全存储，
// 配置文件里放不下明文密钥；而且运行中的进程不会重读配置文件，改文件要等重启
// 才生效。文件路径只是 agent 没在跑时的回退，且回退时必定在输出里告警。
func ApplyXimoAgent(cfg Config) error {
	out := cfg.out()

	baseURL, err := normalizeBaseURL(cfg.GatewayURL)
	if err != nil {
		return err
	}
	if cfg.APIKey != "" {
		// 登记之后，任何经 redact 的错误/输出都不会包含这串明文。
		RegisterSecret(cfg.APIKey)
	}

	endpoint, source := DiscoverEndpoint(cfg.Endpoint)

	if !cfg.NoIPC {
		err := applyViaIPC(cfg, out, endpoint, source, baseURL)
		if err == nil {
			return nil
		}
		var ce *connectError
		if !errors.As(err, &ce) {
			return err
		}
		if cfg.NoFileFallback {
			return err
		}
		fmt.Fprintf(out, "[警告] %v\n", err)
		fmt.Fprintf(out, "[警告] 回退到直接改 ximo-agent 的配置文件（回退路径的限制见下面的逐条告警）\n")
	}

	return applyViaFile(cfg, out, baseURL)
}

// ---------------------------------------------------------------------------
// IPC 路径
// ---------------------------------------------------------------------------

func applyViaIPC(cfg Config, out io.Writer, endpoint, source, baseURL string) error {
	fmt.Fprintf(out, "IPC 端点：%s（来源：%s）\n", endpoint, source)

	ctx, cancel := context.WithTimeout(context.Background(), 3*cfg.timeout())
	defer cancel()

	client := NewClient(endpoint, cfg.timeout())
	if err := client.Connect(ctx); err != nil {
		return &connectError{err: err}
	}
	defer client.Close()

	cur, err := client.Settings(ctx)
	if err != nil {
		return fmt.Errorf("读取 ximo-agent 当前配置失败：%w", err)
	}

	configPath := cfg.ConfigPath
	if configPath == "" {
		configPath = cur.ConfigPath
	}
	if configPath == "" {
		configPath = DefaultConfigPath(cfg.Home)
	}

	next := buildSettings(cur, cfg, baseURL)

	// model 为空时保留现值，顺便把可用模型列出来当提示（只读，不改任何东西）。
	if strings.TrimSpace(cfg.Model) == "" {
		fmt.Fprintf(out, "未指定 --model：沿用当前模型 %s\n", orEmptyPlaceholder(cur.Model))
		if models, mErr := client.ListModels(ctx, baseURL); mErr != nil {
			fmt.Fprintf(out, "  （顺便查可用模型失败，忽略：%v）\n", mErr)
		} else if models.Error != "" {
			fmt.Fprintf(out, "  （服务端查询模型失败：%s）\n", models.Error)
		} else if len(models.Models) > 0 {
			ids := make([]string, 0, len(models.Models))
			for _, m := range models.Models {
				ids = append(ids, m.ID)
			}
			fmt.Fprintf(out, "  可用模型（最多显示 10 个）：%s\n", strings.Join(truncate(ids, 10), ", "))
		}
	}

	if cfg.DryRun {
		fmt.Fprintf(out, "--- %s（dry-run，未改动任何东西）\n", configPath)
		var lines []string
		fieldDiff(&lines, "base_url", cur.BaseURL, next.BaseURL)
		fieldDiff(&lines, "model", cur.Model, next.Model)
		fieldDiff(&lines, "provider_id", cur.ProviderID, next.ProviderID)
		fieldDiff(&lines, "provider_name", cur.ProviderName, next.ProviderName)
		fieldDiff(&lines, "workspace_root", cur.WorkspaceRoot, next.WorkspaceRoot)
		if len(lines) == 0 {
			fmt.Fprintln(out, "  (无差异)")
		} else {
			fmt.Fprintln(out, strings.Join(lines, "\n"))
		}
		if cfg.APIKey != "" {
			fmt.Fprintln(out, "  secret_ref        将由 system.secret.put 写入安全存储（dry-run 未执行）")
		}
		return nil
	}

	// 备份：config.set 会落盘到 agent 自己的 config.json，改之前先留一份。
	if backupPath, bErr := backupFile(configPath); bErr != nil {
		return fmt.Errorf("备份 %s 失败，已中止（不改动任何配置）：%w", configPath, bErr)
	} else if backupPath != "" {
		fmt.Fprintf(out, "已备份：%s\n", backupPath)
	} else {
		fmt.Fprintf(out, "（%s 尚不存在，无需备份）\n", configPath)
	}

	if cfg.APIKey != "" {
		ref, err := client.PutSecret(ctx, cfg.APIKey)
		if err != nil {
			return fmt.Errorf("把密钥写入平台安全存储失败，配置未改动：%w", err)
		}
		next.SecretRef = ref
		fmt.Fprintf(out, "密钥已存入平台安全存储，引用：%s\n", ref)
	}

	if err := client.ApplySettings(ctx, next); err != nil {
		return fmt.Errorf("写入 ximo-agent 运行时配置失败：%w", err)
	}

	fmt.Fprintf(out, "已通过 IPC 生效：base_url=%s model=%s secret_ref=%s\n",
		next.BaseURL, orEmptyPlaceholder(next.Model), orEmptyPlaceholder(next.SecretRef))
	fmt.Fprintln(out, "提示：base_url / model 在 agent 进程内立即重建生效；MCP 服务器清单等启动期装配的项需重启 agent。")
	return nil
}

// PatchForEcho 把 config.get 拿到的现状整理成**可以安全回传**的 config.set 载荷。
//
// 为什么必须过这一道：主仓库 ApplySettings 对 provider_id / provider_name /
// base_url / model / workspace_root 是无条件覆盖，漏回填就会把用户配好的值清空；
// 而 providers / mcp_servers 一旦原样回传就会走「整体替换」并把 UI 快照当作真相
// （候选池里非主服务商的 Timeout 会被主服务商的值顶掉）。置 nil 才是「未携带 =
// 沿用现有配置」这条被契约承认的语义。
//
// 调用方在这份载荷上改自己关心的字段即可。
func PatchForEcho(cur RuntimeSettings) RuntimeSettings {
	next := cur
	next.Providers = nil
	next.MCPServers = nil
	next.SubAgent = SubAgentSettingsPayload{}
	return next
}

// buildSettings 在 PatchForEcho 的基础上套上本次要改的字段，得到最终要提交的载荷。
func buildSettings(cur RuntimeSettings, cfg Config, baseURL string) RuntimeSettings {
	next := PatchForEcho(cur)
	next.BaseURL = baseURL
	if strings.TrimSpace(cfg.Model) != "" {
		next.Model = cfg.Model
	}
	if strings.TrimSpace(cfg.ProviderID) != "" {
		next.ProviderID = cfg.ProviderID
	}
	if strings.TrimSpace(cfg.DisplayName) != "" {
		next.ProviderName = cfg.DisplayName
	}
	return next
}

// ---------------------------------------------------------------------------
// 配置文件回退路径
// ---------------------------------------------------------------------------

func applyViaFile(cfg Config, out io.Writer, baseURL string) error {
	path := cfg.ConfigPath
	if path == "" {
		path = DefaultConfigPath(cfg.Home)
	}
	if path == "" {
		return errors.New("无法确定 ximo-agent 的配置文件位置，请用 --config-path 指定")
	}
	docIO := cfg.docIO()

	doc, err := docIO.Read(path, "json")
	if err != nil {
		return err
	}

	provider, err := providerObject(doc)
	if err != nil {
		return fmt.Errorf("%s：%w", path, err)
	}
	provider["base_url"] = baseURL
	if strings.TrimSpace(cfg.Model) != "" {
		provider["model"] = cfg.Model
	}
	if strings.TrimSpace(cfg.ProviderID) != "" {
		provider["id"] = cfg.ProviderID
	}
	if strings.TrimSpace(cfg.DisplayName) != "" {
		provider["name"] = cfg.DisplayName
	}

	// 回退路径的告警必须显眼：它同时是「生效时机」和「密钥存不进安全存储」
	// 两件事的说明。
	fmt.Fprintf(out, "[警告] IPC 不可用，改用配置文件：%s\n", path)
	fmt.Fprintln(out, "[警告] 该文件只在 ximo-agent **重启后**才被读回；正在运行的进程不会重读它。")
	fmt.Fprintln(out, "[警告] 且必须是启动时会自动加载 <BaseDir>/config.json 的构建："+
		"当前工作树的 cmd/ximo-agent/main.go 已自动加载（旧的 v2-go 分支不会，它 --config 默认为空、"+
		"LoadConfig(\"\") 直接返回默认值，那种构建下这次写入会被静默忽略）。")

	if cfg.APIKey != "" {
		ref := SecretRefForValue(cfg.APIKey)
		provider["secret_ref"] = ref
		fmt.Fprintf(out, "[警告] 已写入 secret_ref=%s（由密钥派生，幂等）：密钥**明文没有**存入平台安全存储，"+
			"因为安全存储只能经 IPC 写。\n", ref)
		fmt.Fprintln(out, "[警告] 生效方式：在 ximo-agent 设置界面填入同一个密钥（会得到同一个 ref），"+
			"或按 EnvBackend 约定设置环境变量 XIMO_SECRET_"+strings.ToUpper(strings.TrimPrefix(ref, "secretref:v1:"))+"。")
		fmt.Fprintln(out, "[警告] 只要密钥还没进安全存储，该 ref 就取不到值，模型调用会失败——这是预期行为，不是配置写错。")
	}

	if cfg.DryRun {
		diff, err := docIO.Diff(path, "json", doc)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "--- %s（dry-run，未写入）\n", path)
		if strings.TrimSpace(diff) == "" {
			fmt.Fprintln(out, "(无差异)")
		} else {
			fmt.Fprintln(out, strings.TrimRight(diff, "\n"))
		}
		return nil
	}

	if err := docIO.Write(path, "json", doc, true); err != nil {
		return err
	}
	fmt.Fprintf(out, "已写入 %s（未知字段原样保留，旧文件已备份为 %s.bak-<unix>）\n", path, path)
	fmt.Fprintf(out, "  base_url   = %s\n", baseURL)
	if m, ok := provider["model"].(string); ok && m != "" {
		fmt.Fprintf(out, "  model      = %s\n", m)
	}
	if r, ok := provider["secret_ref"].(string); ok && r != "" {
		fmt.Fprintf(out, "  secret_ref = %s\n", r)
	}
	return nil
}

// providerObject 取出（必要时创建）配置里的 provider 对象。它必须是对象，
// 撞上别的类型时宁可报错也不覆盖用户手写的东西。
func providerObject(doc map[string]any) (map[string]any, error) {
	switch v := doc["provider"].(type) {
	case nil:
		p := map[string]any{}
		doc["provider"] = p
		return p, nil
	case map[string]any:
		return v, nil
	default:
		return nil, fmt.Errorf("配置里的 provider 段不是 JSON 对象（实际是 %T），拒绝改写", doc["provider"])
	}
}

// ---------------------------------------------------------------------------
// 路径与 URL 规整
// ---------------------------------------------------------------------------

// DefaultConfigPath 返回 ximo-agent 的约定配置文件位置 <BaseDir>/config.json，
// 与主仓库 internal/config.DefaultPaths + DefaultConfigPath 的规则一致。
//
// home 非空时用它替代用户主目录，并且不再看 APPDATA / XDG_CONFIG_HOME：
// --home 的语义是「把这次操作当成在另一个家目录下执行」，混用环境变量会让
// 测试与 CI 结果随机器变化。XIMO_HOME 仍然优先（它是 ximo-agent 自己的开关）。
func DefaultConfigPath(home string) string {
	baseDir := DefaultBaseDir(home)
	if baseDir == "" {
		return ""
	}
	return filepath.Join(baseDir, "config.json")
}

// DefaultBaseDir 返回 ximo-agent 的 BaseDir。
func DefaultBaseDir(home string) string {
	if custom := strings.TrimSpace(os.Getenv("XIMO_HOME")); custom != "" {
		return custom
	}
	if strings.TrimSpace(home) == "" {
		switch runtime.GOOS {
		case "windows":
			if appData := strings.TrimSpace(os.Getenv("APPDATA")); appData != "" {
				return filepath.Join(appData, "ximo-agent")
			}
			home = os.Getenv("USERPROFILE")
		case "darwin":
			home, _ = os.UserHomeDir()
		default:
			if xdg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); xdg != "" {
				return filepath.Join(xdg, "ximo-agent")
			}
			home, _ = os.UserHomeDir()
		}
	}
	if strings.TrimSpace(home) == "" {
		home, _ = os.UserHomeDir()
	}
	if strings.TrimSpace(home) == "" {
		return ""
	}
	switch runtime.GOOS {
	case "windows":
		return filepath.Join(home, "AppData", "Roaming", "ximo-agent")
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "ximo-agent")
	default:
		return filepath.Join(home, ".config", "ximo-agent")
	}
}

// normalizeBaseURL 与 ximo-agent 自己的 config.NormalizeBaseURL 规则一致：
// 补 https://（缺协议头）、必须 http(s)、必须有主机名、去尾斜杠。
//
// 在插件侧先做一遍是为了「失败要早」：地址不合法时一个字节都不该落盘。
func normalizeBaseURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", errors.New("缺少网关地址（--gateway / Config.GatewayURL 为空）")
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("网关地址无法解析（%q）：%w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("网关地址的协议必须是 http(s)，当前为 %q", u.Scheme+"://")
	}
	if u.Host == "" {
		return "", fmt.Errorf("网关地址缺少主机名（例如 http://127.0.0.1:8600）")
	}
	return strings.TrimRight(s, "/"), nil
}

// SecretRefForValue 复刻 ximo-agent 的 secret_ref 派生规则：前缀
// "secretref:v1:" + sha256(明文) 前 16 字节的十六进制。
//
// 它只用于回退路径与诊断：ref 是单向摘要，反推不出明文，也确认不了明文。
func SecretRefForValue(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "secretref:v1:" + hex.EncodeToString(sum[:16])
}

func truncate(items []string, n int) []string {
	if len(items) <= n {
		return items
	}
	out := append([]string{}, items[:n]...)
	return append(out, fmt.Sprintf("…（共 %d 个）", len(items)))
}
