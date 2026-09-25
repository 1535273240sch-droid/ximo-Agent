package ipcapi

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ximo888ok-netizen/ximo-agent/internal/ipc"
)

// 本文件把「密钥与配置管理」暴露成 IPC 帧。
//
// 安全约束（第21章），也是本文件存在的核心理由：
//   - 写密钥：请求体里有一次明文，处理完立即交给平台安全存储加密；不进日志、
//     不进事件、不进错误消息（错误经 types.RedactString 脱敏后才会外传）。
//   - 读密钥：**没有**读取明文的接口。查询只返回「是否已配置 + 后端名」。
//     这是刻意的：界面需要的是"填没填过"，不是一个可以复制的密钥串；
//     一旦提供读取接口，明文就会经由 IPC 落到渲染进程内存里，脱敏红线失守。

// SecretsAdmin 是密钥管理能力，由 bootstrap.App 的 SecretStore 满足。
type SecretsAdmin interface {
	ListSecretStatus() SecretStatusPayload
	PutSecret(ctx context.Context, value string) (string, error)
	DeleteSecret(ctx context.Context, ref string) error
}

// SecretStatusPayload 是查询密钥状态的结果。不含任何明文。
type SecretStatusPayload struct {
	// Configured 表示当前配置引用的密钥是否已存在于安全存储。
	Configured bool `json:"configured"`
	// Ref 是密钥引用（不是密钥本身）。
	Ref string `json:"ref"`
	// Backend 是实际使用的安全存储后端，例如 windows-dpapi。
	Backend string `json:"backend"`
	// Available 表示平台安全存储是否可用。不可用时写入会失败，不会降级为明文。
	Available bool `json:"available"`
}

// SettingsAdmin 是运行时配置读写能力。
type SettingsAdmin interface {
	GetRuntimeSettings() RuntimeSettingsPayload
	ApplyRuntimeSettings(ctx context.Context, in RuntimeSettingsPayload) error
	ConfigFilePath() string
	// ListModels 向服务商查询可用模型列表。baseURL 非空时以它为准（设置页
	// 会传表单当前值，用户可能还没保存）；为空时用已保存配置。
	ListModels(ctx context.Context, baseURL string) ModelListPayload
}

// ModelInfoPayload 是单个可用模型。
type ModelInfoPayload struct {
	ID      string `json:"id"`
	OwnedBy string `json:"owned_by,omitempty"`
}

// ModelListPayload 是模型查询结果。
type ModelListPayload struct {
	Models  []ModelInfoPayload `json:"models"`
	BaseURL string             `json:"base_url"`
	Error   string             `json:"error,omitempty"`
}

// ProviderEntryPayload 是子代理模型池里的一个候选服务商（任务 5）。
//
// SecretRef 仍是引用而非明文 —— 脱敏红线与密钥帧一致。
type ProviderEntryPayload struct {
	ID              string  `json:"id"`
	Name            string  `json:"name"`
	BaseURL         string  `json:"base_url"`
	Model           string  `json:"model"`
	SecretRef       string  `json:"secret_ref"`
	ContextWindow   int     `json:"context_window"`
	MaxOutputTokens int     `json:"max_output_tokens"`
	RateLimitPerSec float64 `json:"rate_limit_per_sec"`
}

// SubAgentSettingsPayload 是子代理的候选模型分配（任务 5 设置面板读写）。
//
// 候选一律用服务商 ID 表示；顺序即优先级，失败转移按这个顺序轮询。
// nil 的字段表示「调用方没带这个字段，沿用现有配置」—— 旧版前端不带
// 新字段时不能把用户已配好的分配抹掉。
type SubAgentSettingsPayload struct {
	// Pool 全局子代理池的候选 ID 顺序。
	Pool []string `json:"pool,omitempty"`
	// ByDivision 专家分类 → 候选 ID 顺序。
	ByDivision map[string][]string `json:"by_division,omitempty"`
	// ByExpert 专家 ID → 候选 ID 顺序（优先级高于分类）。
	ByExpert map[string][]string `json:"by_expert,omitempty"`
}

// RuntimeSettingsPayload 是界面可读写的运行时配置。
//
// 刻意不含密钥字段：密钥只以 secret_ref 的形式出现。
type RuntimeSettingsPayload struct {
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
	ConfigPath      string `json:"config_path"`

	// Providers 是完整候选池（含主服务商，顺序即优先级）。nil 表示调用方
	// 未携带该字段，沿用现有配置；非 nil 时整体替换（任务 5）。
	Providers []ProviderEntryPayload `json:"providers,omitempty"`
	// SubAgent 是子代理模型分配（任务 5）。
	SubAgent SubAgentSettingsPayload `json:"sub_agent"`
}

// AdminOption 配置 AdminService。
type AdminOption func(*AdminService)

// AdminService 把密钥与配置管理注册到 IPC 服务端。
type AdminService struct {
	secrets  SecretsAdmin
	settings SettingsAdmin
}

// NewAdminService 构造管理服务。两者都可为 nil，对应的帧会返回明确错误。
func NewAdminService(secrets SecretsAdmin, settings SettingsAdmin) *AdminService {
	return &AdminService{secrets: secrets, settings: settings}
}

// Register 注册管理类帧。
func (s *AdminService) Register(srv *ipc.Server) {
	srv.RegisterHandler(ipc.TypeSecretStatus, s.handleSecretStatus)
	srv.RegisterHandler(ipc.TypeSecretPut, s.handleSecretPut)
	srv.RegisterHandler(ipc.TypeConfigGet, s.handleConfigGet)
	srv.RegisterHandler(ipc.TypeConfigSet, s.handleConfigSet)
	srv.RegisterHandler(ipc.TypeModelList, s.handleModelList)
}

// handleModelList 查询服务商可用模型列表。
//
// 请求载荷可选：{"base_url":"..."} 非空时按该地址查询（用户表单里刚改过、
// 还没保存的场景），缺省用已保存配置。
func (s *AdminService) handleModelList(ctx context.Context, req *ipc.Frame) (*ipc.Frame, error) {
	if s.settings == nil {
		return ErrorFrame(ipc.TypeModelList, req,
			fmt.Errorf("settings are not available in this build")), nil
	}
	baseURL := ""
	if env, err := decodeEnvelope(req); err == nil {
		var p struct {
			BaseURL string `json:"base_url"`
		}
		if env.Decode(&p) == nil {
			baseURL = p.BaseURL
		}
	}
	res := s.settings.ListModels(ctx, baseURL)
	out, err := EnvelopeFrom(ipc.TypeModelList, res,
		FrameOpts{RequestID: req.Header.RequestID, SessionID: req.Header.SessionID})
	if err != nil {
		return ErrorFrame(ipc.TypeModelList, req, err), nil
	}
	return out, nil
}

// handleSecretStatus 查询密钥配置状态。绝不返回明文。
func (s *AdminService) handleSecretStatus(_ context.Context, req *ipc.Frame) (*ipc.Frame, error) {
	if s.secrets == nil {
		return ErrorFrame(ipc.TypeSecretStatus, req,
			fmt.Errorf("secrets are not available in this build")), nil
	}
	out, err := EnvelopeFrom(ipc.TypeSecretStatus, s.secrets.ListSecretStatus(),
		FrameOpts{RequestID: req.Header.RequestID, SessionID: req.Header.SessionID})
	if err != nil {
		return ErrorFrame(ipc.TypeSecretStatus, req, err), nil
	}
	return out, nil
}

// handleSecretPut 写入密钥。
//
// 请求体形如 {"value":"sk-..."}。收到后立即转交安全存储，明文不落任何日志。
// 响应只返回引用（ref），调用方可把它填进配置的 secret_ref。
func (s *AdminService) handleSecretPut(ctx context.Context, req *ipc.Frame) (*ipc.Frame, error) {
	if s.secrets == nil {
		return ErrorFrame(ipc.TypeSecretPut, req,
			fmt.Errorf("secrets are not available in this build")), nil
	}
	env, err := decodeEnvelope(req)
	if err != nil {
		return ErrorFrame(ipc.TypeSecretPut, req, err), nil
	}
	var p struct {
		Value string `json:"value"`
	}
	if err := env.Decode(&p); err != nil {
		return ErrorFrame(ipc.TypeSecretPut, req, err), nil
	}
	if p.Value == "" {
		return ErrorFrame(ipc.TypeSecretPut, req, fmt.Errorf("value must not be empty")), nil
	}

	ref, err := s.secrets.PutSecret(ctx, p.Value)
	if err != nil {
		// 错误消息经脱敏后才外传，避免密钥通过错误路径泄露。
		return ErrorFrame(ipc.TypeSecretPut, req, err), nil
	}
	if s.settings != nil {
		// 顺手把引用写进配置，用户不必再手动填一遍 secret_ref。
		cur := s.settings.GetRuntimeSettings()
		cur.SecretRef = ref
		if err := s.settings.ApplyRuntimeSettings(ctx, cur); err != nil {
			// 引用写入失败不影响密钥已存的事实，但要让调用方知道配置没更新。
			return ErrorFrame(ipc.TypeSecretPut, req,
				fmt.Errorf("secret stored as %s but settings update failed: %w", ref, err)), nil
		}
	}

	out, err := EnvelopeFrom(ipc.TypeSecretPut, map[string]any{"ref": ref, "ok": true},
		FrameOpts{RequestID: req.Header.RequestID, SessionID: req.Header.SessionID})
	if err != nil {
		return ErrorFrame(ipc.TypeSecretPut, req, err), nil
	}
	return out, nil
}

// handleConfigGet 读取运行时配置。
func (s *AdminService) handleConfigGet(_ context.Context, req *ipc.Frame) (*ipc.Frame, error) {
	if s.settings == nil {
		return ErrorFrame(ipc.TypeConfigGet, req,
			fmt.Errorf("settings are not available in this build")), nil
	}
	p := s.settings.GetRuntimeSettings()
	p.ConfigPath = s.settings.ConfigFilePath()
	out, err := EnvelopeFrom(ipc.TypeConfigGet, p,
		FrameOpts{RequestID: req.Header.RequestID, SessionID: req.Header.SessionID})
	if err != nil {
		return ErrorFrame(ipc.TypeConfigGet, req, err), nil
	}
	return out, nil
}

// handleConfigSet 应用运行时配置变更。
func (s *AdminService) handleConfigSet(ctx context.Context, req *ipc.Frame) (*ipc.Frame, error) {
	if s.settings == nil {
		return ErrorFrame(ipc.TypeConfigSet, req,
			fmt.Errorf("settings are not available in this build")), nil
	}
	env, err := decodeEnvelope(req)
	if err != nil {
		return ErrorFrame(ipc.TypeConfigSet, req, err), nil
	}
	var in RuntimeSettingsPayload
	if err := env.Decode(&in); err != nil {
		return ErrorFrame(ipc.TypeConfigSet, req, err), nil
	}
	if err := s.settings.ApplyRuntimeSettings(ctx, in); err != nil {
		return ErrorFrame(ipc.TypeConfigSet, req, err), nil
	}
	out, err := EnvelopeFrom(ipc.TypeConfigSet, map[string]any{"ok": true},
		FrameOpts{RequestID: req.Header.RequestID, SessionID: req.Header.SessionID})
	if err != nil {
		return ErrorFrame(ipc.TypeConfigSet, req, err), nil
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 客户端方法
// ---------------------------------------------------------------------------

// SecretStatus 查询密钥状态。
func (c *Client) SecretStatus(ctx context.Context) (SecretStatusPayload, error) {
	var out SecretStatusPayload
	err := c.call(ctx, ipc.TypeSecretStatus, map[string]any{}, &out)
	return out, err
}

// PutSecret 写入密钥，返回引用。
func (c *Client) PutSecret(ctx context.Context, value string) (string, error) {
	var out struct {
		Ref string `json:"ref"`
	}
	err := c.call(ctx, ipc.TypeSecretPut, map[string]any{"value": value}, &out)
	return out.Ref, err
}

// Settings 读取运行时配置。
func (c *Client) Settings(ctx context.Context) (RuntimeSettingsPayload, error) {
	var out RuntimeSettingsPayload
	err := c.call(ctx, ipc.TypeConfigGet, map[string]any{}, &out)
	return out, err
}

// ApplySettings 提交运行时配置。
func (c *Client) ApplySettings(ctx context.Context, in RuntimeSettingsPayload) error {
	return c.call(ctx, ipc.TypeConfigSet, in, nil)
}

// ListModels 查询服务商可用模型列表。baseURL 非空时按该地址查询（表单当前值）。
func (c *Client) ListModels(ctx context.Context, baseURL string) (ModelListPayload, error) {
	var out ModelListPayload
	err := c.call(ctx, ipc.TypeModelList, map[string]any{"base_url": baseURL}, &out)
	return out, err
}

// asJSONForDebug 便于排查协议问题时打印载荷形状（不含密钥值）。
func asJSONForDebug(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return "<unmarshalable>"
	}
	return string(raw)
}
