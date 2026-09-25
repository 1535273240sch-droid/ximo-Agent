package bootstrap

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"

	"github.com/ximo888ok-netizen/ximo-agent/internal/config"
	"github.com/ximo888ok-netizen/ximo-agent/internal/ipcapi"
	"github.com/ximo888ok-netizen/ximo-agent/internal/ports"
	"github.com/ximo888ok-netizen/ximo-agent/internal/secrets"
)

// SecretStore 是密钥读写的对外接口，供 UI 的"填写 API 密钥"入口使用。
//
// 设计红线（第21章）：
//   - 写入：明文只在内存里存在一次，立即交给平台安全存储（Windows DPAPI /
//     Keychain / SecretService）加密落盘，返回的是引用（ref），不是密钥。
//   - 读取：绝不返回明文。只回"这个引用是否已配置"以及后端名称。
//     界面需要展示的是"已配置/未配置"，不是一个可以复制的密钥串。
type SecretStore struct {
	mgr *secrets.Manager
	app *App

	mu sync.Mutex
}

// Secrets 返回密钥存储，供 IPC 层调用。可能为 nil（装配时未启用安全存储）。
func (a *App) Secrets() *SecretStore {
	if a.secrets == nil {
		return nil
	}
	return &SecretStore{mgr: a.secrets, app: a}
}

// SecretStatus 描述某个密钥引用的配置状态。不含明文。
type SecretStatus struct {
	// Ref 是密钥引用（形如 dpapi:...）。界面用它做标识。
	Ref string `json:"ref"`
	// Configured 表示该引用已经在安全存储中写入过值。
	Configured bool `json:"configured"`
	// Backend 是实际使用的安全存储后端名，例如 windows-dpapi。
	Backend string `json:"backend"`
	// Available 表示平台安全存储是否可用。不可用时写入会失败而不是降级为明文。
	Available bool `json:"available"`
}

// ListSecretStatus 实现 ipcapi.SecretsAdmin：报告当前引用的配置状态。
//
// 刻意只有一个"查状态"的方法而没有"读明文"的方法：见 ipcapi/admin.go 的说明。
func (s *SecretStore) ListSecretStatus() ipcapi.SecretStatusPayload {
	st := s.Status("")
	return ipcapi.SecretStatusPayload{
		Configured: st.Configured,
		Ref:        st.Ref,
		Backend:    st.Backend,
		Available:  st.Available,
	}
}

// GetRuntimeSettings 实现 ipcapi.SettingsAdmin。
func (a *App) GetRuntimeSettings() ipcapi.RuntimeSettingsPayload {
	s := a.Settings()
	return ipcapi.RuntimeSettingsPayload{
		ProviderID:      s.ProviderID,
		ProviderName:    s.ProviderName,
		BaseURL:         s.BaseURL,
		Model:           s.Model,
		ContextWindow:   s.ContextWindow,
		MaxOutputTokens: s.MaxOutputTokens,
		SecretRef:       s.SecretRef,
		AutoMode:        s.AutoMode,
		WorkspaceRoot:   s.WorkspaceRoot,
		DBPath:          s.DBPath,
		ConfigPath:      a.ConfigPath(),
		Providers:       s.Providers,
		SubAgent:        s.SubAgent,
		MCPServers:      mcpServersToPayload(s.MCPServers),
	}
}

// ApplyRuntimeSettings 实现 ipcapi.SettingsAdmin。
func (a *App) ApplyRuntimeSettings(ctx context.Context, in ipcapi.RuntimeSettingsPayload) error {
	return a.ApplySettings(ctx, RuntimeSettings{
		ProviderID:      in.ProviderID,
		ProviderName:    in.ProviderName,
		BaseURL:         in.BaseURL,
		Model:           in.Model,
		ContextWindow:   in.ContextWindow,
		MaxOutputTokens: in.MaxOutputTokens,
		SecretRef:       in.SecretRef,
		AutoMode:        in.AutoMode,
		WorkspaceRoot:   in.WorkspaceRoot,
		DBPath:          in.DBPath,
		Providers:       in.Providers,
		SubAgent:        in.SubAgent,
		MCPServers:      mcpServersFromPayload(in.MCPServers),
	})
}

// ConfigFilePath 实现 ipcapi.SettingsAdmin。
func (a *App) ConfigFilePath() string { return a.ConfigPath() }

// PutSecret 写入密钥并返回引用。
//
// 明文绝不被记录、不被返回、不落日志：入参用完即弃，返回值只有引用。
func (s *SecretStore) PutSecret(_ context.Context, value string) (string, error) {
	if s == nil || s.mgr == nil {
		return "", fmt.Errorf("secrets: manager is not available in this build")
	}
	if value == "" {
		return "", fmt.Errorf("secrets: refusing to store an empty secret")
	}
	if !s.mgr.Available() {
		return "", secrets.ErrBackendUnavailable
	}
	ref, err := s.mgr.Put(value)
	if err != nil {
		// 把平台安全存储的真实失败原因带出来（例如 DPAPI 权限不足），
		// 否则界面只能显示一个含糊的"保存失败"，无从排查。
		return "", fmt.Errorf("安全存储写入失败（后端 %s）: %w", s.mgr.BackendName(), err)
	}

	// 密钥变更后立刻让 Provider 用新密钥重建，否则用户填了也得重启才生效。
	if err := s.app.rebuildProvider(ref); err != nil {
		// 重建失败不回滚密钥（密钥本身是对的），但必须让调用方知道当前 run 仍不可用。
		return ref, fmt.Errorf("secret stored as %s but provider rebuild failed: %w", ref, err)
	}
	return ref, nil
}

// Status 查询某个引用的配置状态。ref 为空时报告默认引用的状态。
func (s *SecretStore) Status(ref string) SecretStatus {
	if s == nil || s.mgr == nil {
		return SecretStatus{Ref: ref, Backend: "unavailable", Available: false}
	}
	if ref == "" && s.app != nil && s.app.cfg != nil {
		ref = s.app.cfg.Provider.SecretRef
	}
	st := SecretStatus{
		Ref:       ref,
		Backend:   s.mgr.BackendName(),
		Available: s.mgr.Available(),
	}
	if ref != "" {
		// Get 的返回值只用于判断"能否取到"，绝不外传。
		if _, err := s.mgr.Get(ref); err == nil {
			st.Configured = true
		}
	}
	return st
}

// DeleteSecret 删除引用对应的密钥。
func (s *SecretStore) DeleteSecret(_ context.Context, ref string) error {
	if s == nil || s.mgr == nil {
		return fmt.Errorf("secrets: manager is not available in this build")
	}
	return s.mgr.Delete(ref)
}

// Rebuild 用当前配置与密钥引用重新构造 Provider 并热替换。
func (s *SecretStore) Rebuild(_ context.Context, ref string) error {
	if s == nil || s.app == nil {
		return nil
	}
	return s.app.rebuildProvider(ref)
}

// rebuildProvider 重新构造 Provider 端口并原地替换。
//
// 为什么可以热替换：Engine 拿到的是 swappableProvider 这个稳定壳，换实现只是
// 换它内部的指针。这样用户在设置页填完密钥或改完模型名立即生效，
// 不需要重启后端；已经在跑的 run 不受影响（它已经取到了当时的调用路径）。
func (a *App) rebuildProvider(ref string) error {
	if a.cfg == nil {
		return nil
	}

	next := *a.cfg
	if ref != "" {
		next.Provider.SecretRef = ref
	}

	// 必须把 a 自身传进去：buildProvider 需要读 app.secrets 来解析密钥引用。
	// 传 nil 会在访问 app.secrets 时 panic，表现为密钥保存失败。
	prov, err := buildProvider(&next, Options{}, a)
	if err != nil {
		return err
	}

	a.cfg.Provider.SecretRef = next.Provider.SecretRef
	a.providerPort = prov
	if a.providerHolder != nil {
		a.providerHolder.Swap(prov)
	}
	// 服务商配置变了，子代理模型池里缓存的客户端（BaseURL/密钥引用）随之过期。
	a.invalidateSubAgentPool()
	return nil
}

// ProviderPort 返回当前生效的 Provider 端口。
func (a *App) ProviderPort() ports.Provider {
	if a == nil {
		return nil
	}
	return a.providerPort
}

// ---------------------------------------------------------------------------
// 运行时配置读写（不含密钥）
// ---------------------------------------------------------------------------

// RuntimeSettings 是界面可读写的运行时配置快照。
//
// 刻意不含任何密钥字段：密钥只以引用形式出现在 SecretStatus 里。
type RuntimeSettings struct {
	ProviderID      string `json:"provider_id"`
	ProviderName    string `json:"provider_name"`
	BaseURL         string `json:"base_url"`
	Model           string `json:"model"`
	ContextWindow   int    `json:"context_window"`
	MaxOutputTokens int    `json:"max_output_tokens"`
	SecretRef       string `json:"secret_ref"`
	AutoMode        string `json:"auto_mode"`
	WorkspaceRoot   string `json:"workspace_root"`
	DBPath          string `json:"db_path"`

	// Providers 是完整候选池（含主服务商，顺序即优先级）。nil = 未携带，
	// 沿用现有配置（任务 5）。
	Providers []ipcapi.ProviderEntryPayload `json:"providers,omitempty"`
	// SubAgent 是子代理模型分配（任务 5）。nil 的 map = 未携带，沿用。
	SubAgent ipcapi.SubAgentSettingsPayload `json:"sub_agent"`

	// MCPServers 是 MCP 服务器清单。nil = 未携带，沿用现有配置；
	// 非 nil 时整体替换（含清空），与 Providers 同语义。
	//
	// 生效时机与其他配置不同：MCP Worker 池在引擎启动时装配，改完需重启
	// 后端才生效（界面据此提示用户）。
	MCPServers []config.MCPServerConfig `json:"mcp_servers,omitempty"`
}

// Settings 返回当前运行时配置。
func (a *App) Settings() RuntimeSettings {
	if a.cfg == nil {
		return RuntimeSettings{}
	}
	c := a.cfg
	out := RuntimeSettings{
		ProviderID:      c.Provider.ID,
		ProviderName:    c.Provider.Name,
		BaseURL:         c.Provider.BaseURL,
		Model:           c.Provider.Model,
		ContextWindow:   c.Provider.ContextWindow,
		MaxOutputTokens: c.Provider.MaxOutputTokens,
		SecretRef:       c.Provider.SecretRef,
		AutoMode:        c.Runtime.AutoMode,
		WorkspaceRoot:   c.Runtime.WorkspaceRoot,
		DBPath:          c.ResolveDBPath(),
	}
	// 候选池：主服务商在首位，其后是池里其余候选（config.ProviderPool 的顺序）。
	for i, p := range c.ProviderPool() {
		entry := ipcapi.ProviderEntryPayload{
			ID:              p.ID,
			Name:            p.Name,
			BaseURL:         p.BaseURL,
			Model:           p.Model,
			SecretRef:       p.SecretRef,
			ContextWindow:   p.ContextWindow,
			MaxOutputTokens: p.MaxOutputTokens,
			RateLimitPerSec: p.RateLimitPerSec,
		}
		if i == 0 {
			// 主服务商的旧字段与新池视图必须一致，避免面板显示两个真相。
			entry.ID, entry.Name, entry.BaseURL = c.Provider.ID, c.Provider.Name, c.Provider.BaseURL
			entry.Model, entry.SecretRef = c.Provider.Model, c.Provider.SecretRef
			entry.ContextWindow, entry.MaxOutputTokens = c.Provider.ContextWindow, c.Provider.MaxOutputTokens
			entry.RateLimitPerSec = c.Provider.RateLimitPerSec
		}
		out.Providers = append(out.Providers, entry)
	}
	// 分配表拷贝一份，避免界面拿到内部 map 后并发改动。
	if len(c.SubAgent.Pool) > 0 {
		out.SubAgent.Pool = append([]string(nil), c.SubAgent.Pool...)
	}
	if len(c.SubAgent.ByDivision) > 0 {
		out.SubAgent.ByDivision = copyAssignment(c.SubAgent.ByDivision)
	}
	if len(c.SubAgent.ByExpert) > 0 {
		out.SubAgent.ByExpert = copyAssignment(c.SubAgent.ByExpert)
	}
	// MCP 清单同样拷贝一份，避免界面直接持有内部切片。
	if len(c.MCPServers) > 0 {
		out.MCPServers = append([]config.MCPServerConfig(nil), c.MCPServers...)
	}
	return out
}

// copyAssignment 深拷贝一张「键 → 候选列表」的分配表。
func copyAssignment(src map[string][]string) map[string][]string {
	out := make(map[string][]string, len(src))
	for k, v := range src {
		out[k] = append([]string(nil), v...)
	}
	return out
}

// mcpServersToPayload 把配置层类型转成协议层条目。
//
// nil 保持 nil（表示"未携带"），空切片保持空（表示"整体清空"）——
// 这两种语义在 ApplySettings 里含义不同，转换时不能合并。
func mcpServersToPayload(in []config.MCPServerConfig) []ipcapi.MCPServerEntryPayload {
	if in == nil {
		return nil
	}
	out := make([]ipcapi.MCPServerEntryPayload, 0, len(in))
	for _, s := range in {
		out = append(out, ipcapi.MCPServerEntryPayload{
			ID:           s.ID,
			Name:         s.Name,
			Transport:    s.Transport,
			Enabled:      s.Enabled,
			Command:      s.Command,
			Args:         s.Args,
			Env:          s.Env,
			Cwd:          s.Cwd,
			URL:          s.URL,
			Headers:      s.Headers,
			AllowedTools: s.AllowedTools,
			DeniedTools:  s.DeniedTools,
		})
	}
	return out
}

// mcpServersFromPayload 把协议层条目转成配置层类型，语义同 mcpServersToPayload。
func mcpServersFromPayload(in []ipcapi.MCPServerEntryPayload) []config.MCPServerConfig {
	if in == nil {
		return nil
	}
	out := make([]config.MCPServerConfig, 0, len(in))
	for _, s := range in {
		out = append(out, config.MCPServerConfig{
			ID:           s.ID,
			Name:         s.Name,
			Transport:    s.Transport,
			Enabled:      s.Enabled,
			Command:      s.Command,
			Args:         s.Args,
			Env:          s.Env,
			Cwd:          s.Cwd,
			URL:          s.URL,
			Headers:      s.Headers,
			AllowedTools: s.AllowedTools,
			DeniedTools:  s.DeniedTools,
		})
	}
	return out
}

// ApplySettings 应用界面提交的配置变更并持久化。
//
// 校验要点：BaseURL 必须非空（否则 Provider 建不起来），其余项允许留空表示
// "沿用默认"。任何非法值都返回错误而不是静默忽略。
func (a *App) ApplySettings(ctx context.Context, in RuntimeSettings) error {
	if a.cfg == nil {
		return fmt.Errorf("bootstrap: no config loaded")
	}
	if in.BaseURL == "" {
		return fmt.Errorf("provider base_url must not be empty")
	}
	// 用户手敲的地址先规整（补 https://、去尾斜杠），规整不了就报人话错误 ——
	// 绝不能把 "api.example.com/v1" 这种坏地址原样存进配置，那会让之后所有
	// 请求以隐晦的方式失败。
	baseURL, err := config.NormalizeBaseURL(in.BaseURL)
	if err != nil {
		return err
	}
	in.BaseURL = baseURL
	for i := range in.Providers {
		norm, err := config.NormalizeBaseURL(in.Providers[i].BaseURL)
		if err != nil {
			return err
		}
		in.Providers[i].BaseURL = norm
	}

	// 主服务商的最终落位在下方候选池合并之后一次性写入；这里先在副本上算出
	// 顶层字段的结果，避免「顶层先写、providers 再覆盖」的中间态。
	// cur 是当前落盘值，保留作「编辑检测」基准 —— next 已应用顶层值，不能当基准。
	cur := a.cfg.Provider
	next := cur
	next.ID = in.ProviderID
	next.Name = in.ProviderName
	next.BaseURL = in.BaseURL
	next.Model = in.Model
	if in.ContextWindow > 0 {
		next.ContextWindow = in.ContextWindow
	}
	if in.MaxOutputTokens > 0 {
		next.MaxOutputTokens = in.MaxOutputTokens
	}
	if in.SecretRef != "" {
		next.SecretRef = in.SecretRef
	}
	if in.AutoMode != "" {
		a.cfg.Runtime.AutoMode = in.AutoMode
	}
	a.cfg.Runtime.WorkspaceRoot = in.WorkspaceRoot

	// 任务 5：候选池整体替换。nil 表示调用方没带该字段（旧版前端），
	// 沿用现有配置 —— 决不能把用户已配好的池静默抹掉。
	//
	// 合并规则（重要）：providers[0] 与顶层字段指向同一个「主服务商」，但两个
	// 设置面板的编辑面相反 —— 凭据面板只改顶层（providers 是它拿到的旧快照），
	// 候选池面板只改 providers（顶层是它拿到的旧快照）。任何一方无条件胜出，
	// 另一边的保存都会被旧快照原样顶回 —— 界面表现为「一点保存就回退到默认」。
	// 因此逐字段取「与当前落盘值不同的那一侧」为编辑值；两侧都改了则顶层优先。
	if in.Providers != nil {
		if len(in.Providers) == 0 {
			a.cfg.Provider = next
			a.cfg.Providers = nil
		} else {
			entry := in.Providers[0]
			entry.ID = pickEdited(cur.ID, entry.ID, in.ProviderID)
			entry.Name = pickEdited(cur.Name, entry.Name, in.ProviderName)
			entry.BaseURL = pickEdited(cur.BaseURL, entry.BaseURL, in.BaseURL)
			entry.Model = pickEdited(cur.Model, entry.Model, in.Model)
			entry.SecretRef = pickEdited(cur.SecretRef, entry.SecretRef, in.SecretRef)
			entry.ContextWindow = pickEditedInt(cur.ContextWindow, entry.ContextWindow, in.ContextWindow)
			entry.MaxOutputTokens = pickEditedInt(cur.MaxOutputTokens, entry.MaxOutputTokens, in.MaxOutputTokens)
			a.cfg.Provider = providerFromEntry(entry, next)
			extras := make([]config.ProviderConfig, 0, len(in.Providers)-1)
			for _, extra := range in.Providers[1:] {
				extras = append(extras, providerFromEntry(extra, config.ProviderConfig{Timeout: a.cfg.Provider.Timeout}))
			}
			a.cfg.Providers = extras
		}
	} else {
		a.cfg.Provider = next
	}
	// 子代理分配：nil 的字段 = 未携带，沿用；非 nil = 整体替换（含清空）。
	if in.SubAgent.Pool != nil {
		a.cfg.SubAgent.Pool = in.SubAgent.Pool
	}
	if in.SubAgent.ByDivision != nil {
		a.cfg.SubAgent.ByDivision = in.SubAgent.ByDivision
	}
	if in.SubAgent.ByExpert != nil {
		a.cfg.SubAgent.ByExpert = in.SubAgent.ByExpert
	}
	// MCP 服务器清单：nil = 未携带，沿用；非 nil = 整体替换（含清空）。
	// 这里只落配置，不重建 Worker 池 —— 池的生命周期绑定在引擎启动上，
	// 改动在下一次后端启动时装配（界面会提示需要重启）。
	if in.MCPServers != nil {
		a.cfg.MCPServers = in.MCPServers
	}

	// 重新构造 Provider 让改动立即生效（尤其是模型名与 base_url）。
	if err := a.rebuildProvider(a.cfg.Provider.SecretRef); err != nil {
		return err
	}

	// 落盘：下次启动沿用，否则用户在界面上改的配置一重启就没了。
	// 写入失败只作为警告——本次会话内配置已经生效，不该因为磁盘问题回滚。
	if path := a.ConfigPath(); path != "" {
		if err := a.cfg.SaveConfig(path); err != nil {
			return fmt.Errorf("settings applied in-memory but could not be persisted: %w", err)
		}
	}
	return nil
}

// providerFromEntry 把候选池载荷映射成配置项。base 提供载荷里没有的字段
// （如 Timeout），这样面板只改它认识的字段，其余配置项不会被无意清零。
func providerFromEntry(in ipcapi.ProviderEntryPayload, base config.ProviderConfig) config.ProviderConfig {
	out := base
	out.ID = in.ID
	out.Name = in.Name
	out.BaseURL = in.BaseURL
	out.Model = in.Model
	out.SecretRef = in.SecretRef
	if in.ContextWindow > 0 {
		out.ContextWindow = in.ContextWindow
	}
	if in.MaxOutputTokens > 0 {
		out.MaxOutputTokens = in.MaxOutputTokens
	}
	out.RateLimitPerSec = in.RateLimitPerSec
	return out
}

// pickEdited 从两份「同一主服务商」的快照里挑出被编辑过的字段值：old 是当前
// 落盘值，a 是顶层表单携带值，b 是候选池快照携带值。与 old 不同的一侧视为
// 编辑值；两侧都改了取顶层 a（主表单是凭据面板）；都没改取 b（等价于沿用）。
func pickEdited(old, b, a string) string {
	switch {
	case a != old:
		return a
	case b != old:
		return b
	default:
		return b
	}
}

// pickEditedInt 是 pickEdited 的整数版：0 表示「该侧未携带此字段」。
func pickEditedInt(old, b, a int) int {
	switch {
	case a != 0 && a != old:
		return a
	case b != 0 && b != old:
		return b
	case a != 0:
		return a
	case b != 0:
		return b
	default:
		return old
	}
}

// ConfigPath 返回配置文件的落盘位置，供界面显示"配置存在哪"。
func (a *App) ConfigPath() string {
	if a == nil || a.cfg == nil {
		return ""
	}
	if a.cfgPath != "" {
		return a.cfgPath
	}
	return configFilePath(a.cfg.Paths.BaseDir)
}

// configFilePath 返回配置文件的约定位置：<BaseDir>/config.json。
func configFilePath(baseDir string) string {
	if baseDir == "" {
		return ""
	}
	return filepath.Join(baseDir, "config.json")
}
