package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/httpx"
	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/model"
	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/secretref"
)

// ---------------------------------------------------------------- 模型目录

type modelWriteRequest struct {
	ID string `json:"id"`
	// ModelID 是 id 的别名，两个字段同时出现且不一致时报 400（避免调用方以为写的是另一个模型）。
	ModelID          string          `json:"model_id"`
	DisplayName      *string         `json:"display_name"`
	Capabilities     json.RawMessage `json:"capabilities"`
	CapabilitiesJSON *string         `json:"capabilities_json"`
	Enabled          *bool           `json:"enabled"`
}

// upsertModel 对应 POST /admin/models（upsert）。
//
// 局部更新语义：请求里没出现的字段保留库中原值（整条覆盖会把后台没填的字段清空，
// 用起来很容易误伤），新建时 display_name 默认取 id、enabled 默认 true、
// capabilities 默认 {}。capabilities/capabilities_json 必须是合法 JSON 对象。
func (h *handler) upsertModel(w http.ResponseWriter, r *http.Request) {
	var req modelWriteRequest
	if !h.decode(w, r, actionModelUpsert, &req) {
		return
	}
	id, err := pickID(req.ID, req.ModelID)
	if err != nil {
		h.fail(w, r, actionModelUpsert, "", err)
		return
	}
	caps, capsProvided, err := pickCapabilities(req.Capabilities, req.CapabilitiesJSON)
	if err != nil {
		h.fail(w, r, actionModelUpsert, id, err)
		return
	}

	spec, err := h.loadModel(r.Context(), id)
	if err != nil {
		h.fail(w, r, actionModelUpsert, id, err)
		return
	}
	if req.DisplayName != nil {
		spec.DisplayName = strings.TrimSpace(*req.DisplayName)
	}
	if capsProvided {
		spec.CapabilitiesJSON = caps
	}
	if req.Enabled != nil {
		spec.Enabled = *req.Enabled
	}
	normalizeModelSpec(&spec, id, h.now())

	if err := h.d.Store.UpsertModel(r.Context(), spec); err != nil {
		h.fail(w, r, actionModelUpsert, id, err)
		return
	}
	if !h.auditOK(w, r, actionModelUpsert, id, map[string]any{
		"display_name": spec.DisplayName,
		"enabled":      spec.Enabled,
		"capabilities": jsonField(spec.CapabilitiesJSON),
	}) {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, viewModel(spec))
}

// loadModel 取既有条目作为局部更新的基底；不存在时返回一个空条目（新建）。
func (h *handler) loadModel(ctx context.Context, id string) (model.ModelSpec, error) {
	cur, err := h.d.Store.GetModel(ctx, id)
	if err == nil {
		return cur, nil
	}
	if errors.Is(err, model.ErrNotFound) {
		return model.ModelSpec{ModelID: id, Enabled: true, CapabilitiesJSON: "{}"}, nil
	}
	return model.ModelSpec{}, err
}

func normalizeModelSpec(spec *model.ModelSpec, id string, now int64) {
	spec.ModelID = id
	if strings.TrimSpace(spec.DisplayName) == "" {
		spec.DisplayName = id
	}
	if strings.TrimSpace(spec.CapabilitiesJSON) == "" {
		spec.CapabilitiesJSON = "{}"
	}
	spec.UpdatedAt = now
	if spec.CreatedAt == 0 {
		spec.CreatedAt = now
	}
}

// getModel 对应 GET /admin/models/{id}。
func (h *handler) getModel(w http.ResponseWriter, r *http.Request) {
	id := pathValue(r, "id")
	if strings.TrimSpace(id) == "" {
		h.readFail(w, r, badRequest("模型 ID 不能为空"))
		return
	}
	m, err := h.d.Store.GetModel(r.Context(), id)
	if err != nil {
		h.readFail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, viewModel(m))
}

// listModels 对应 GET /admin/models?limit=&offset=&enabled=
// （store 只支持「全部/仅启用」两种过滤，分页在内存里做）。
func (h *handler) listModels(w http.ResponseWriter, r *http.Request) {
	limit, offset, ok := h.page(w, r)
	if !ok {
		return
	}
	onlyEnabled := false
	switch strings.TrimSpace(r.URL.Query().Get("enabled")) {
	case "true", "1":
		onlyEnabled = true
	case "", "false", "0":
	default:
		h.readFail(w, r, badRequest("enabled 只能是 true/false/1/0"))
		return
	}
	models, err := h.d.Store.ListModels(r.Context(), onlyEnabled)
	if err != nil {
		h.readFail(w, r, err)
		return
	}
	writeList(w, r, limit, offset, viewModels(paginate(models, limit, offset)))
}

// ---------------------------------------------------------------- Provider

type providerWriteRequest struct {
	ID         string          `json:"id"`
	Name       *string         `json:"name"`
	Endpoint   *string         `json:"endpoint"`
	Protocol   *string         `json:"protocol"`
	Status     *string         `json:"status"`
	APIKeyRef  string          `json:"api_key_ref"`
	APIKey     string          `json:"api_key"`
	Config     json.RawMessage `json:"config"`
	ConfigJSON *string         `json:"config_json"`
	TimeoutMS  *int64          `json:"timeout_ms"`
	Weight     *int64          `json:"weight"`
}

// upsertProvider 对应 POST /admin/providers（upsert）。
//
// 与模型同样的局部更新语义。三处硬校验：
//  1. endpoint 必须能解析出 scheme+host 且为 http/https；
//  2. protocol 只接受 openai-chat（其它值明确 400，见 validateProtocol）；
//  3. api_key/api_key_ref 里若给的是明文，先经 internal/secrets 存入平台安全存储，
//     库里只写 secretref；明文绝不落库、不回显、不进审计 detail。
func (h *handler) upsertProvider(w http.ResponseWriter, r *http.Request) {
	var req providerWriteRequest
	if !h.decode(w, r, actionProviderUpsert, &req) {
		return
	}
	id := strings.TrimSpace(req.ID)
	if id == "" {
		id = newID(prefixProvider)
	}

	configJSON, configProvided, err := pickConfig(req.Config, req.ConfigJSON)
	if err != nil {
		h.fail(w, r, actionProviderUpsert, id, err)
		return
	}

	spec, err := h.loadProvider(r.Context(), id)
	if err != nil {
		h.fail(w, r, actionProviderUpsert, id, err)
		return
	}
	if req.Name != nil {
		spec.Name = strings.TrimSpace(*req.Name)
	}
	if req.Endpoint != nil {
		spec.Endpoint = *req.Endpoint
	}
	if req.Protocol != nil {
		spec.Protocol = *req.Protocol
	}
	if req.Status != nil {
		spec.Status = *req.Status
	}
	if configProvided {
		spec.ConfigJSON = configJSON
	}
	if req.TimeoutMS != nil {
		spec.TimeoutMS = *req.TimeoutMS
	}
	if req.Weight != nil {
		spec.Weight = *req.Weight
	}

	// 校验合并后的完整配置：即使库里有历史脏值（例如早于本校验写入的行），
	// 任何一次写入都必须把它修正，不允许带着非法值继续存在。
	spec.ID = id
	if spec.Endpoint, err = validateEndpoint(spec.Endpoint); err != nil {
		h.fail(w, r, actionProviderUpsert, id, err)
		return
	}
	if spec.Protocol, err = validateProtocol(spec.Protocol); err != nil {
		h.fail(w, r, actionProviderUpsert, id, err)
		return
	}
	if spec.Status, err = validateProviderStatus(spec.Status); err != nil {
		h.fail(w, r, actionProviderUpsert, id, err)
		return
	}
	if spec.TimeoutMS < 0 || spec.Weight < 0 {
		h.fail(w, r, actionProviderUpsert, id, badRequest("timeout_ms 与 weight 不能为负"))
		return
	}

	// 密钥解析放在校验之后：请求被拒时不该往平台安全存储里留下任何东西。
	keyRef, err := h.resolveKeyRef(req.APIKeyRef, req.APIKey)
	if err != nil {
		h.fail(w, r, actionProviderUpsert, id, err)
		return
	}
	if keyRef != "" {
		spec.APIKeyRef = keyRef
	}
	if spec.APIKeyRef != "" && !secretref.IsRef(spec.APIKeyRef) {
		// 兜底：任何情况下都不允许把非 ref 的字符串写进 api_key_ref。
		h.fail(w, r, actionProviderUpsert, id, badRequest("api_key_ref 必须是 secretref:v1:... 形式"))
		return
	}
	if spec.ConfigJSON == "" {
		spec.ConfigJSON = "{}"
	}
	spec.UpdatedAt = h.now()
	if spec.CreatedAt == 0 {
		spec.CreatedAt = spec.UpdatedAt
	}

	if err := h.d.Store.UpsertProvider(r.Context(), spec); err != nil {
		h.fail(w, r, actionProviderUpsert, id, err)
		return
	}
	if !h.auditOK(w, r, actionProviderUpsert, id, map[string]any{
		"name":        spec.Name,
		"endpoint":    spec.Endpoint,
		"protocol":    spec.Protocol,
		"status":      spec.Status,
		"api_key_ref": spec.APIKeyRef,
		"weight":      spec.Weight,
	}) {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, viewProvider(spec))
}

func (h *handler) loadProvider(ctx context.Context, id string) (model.ProviderSpec, error) {
	cur, err := h.d.Store.GetProvider(ctx, id)
	if err == nil {
		return cur, nil
	}
	if errors.Is(err, model.ErrNotFound) {
		return model.ProviderSpec{
			ID:         id,
			Protocol:   model.ProtocolOpenAIChat,
			Status:     model.ProviderStatusEnabled,
			ConfigJSON: "{}",
		}, nil
	}
	return model.ProviderSpec{}, err
}

// resolveKeyRef 把请求里的密钥字段归一为 secretref：
//   - 已经是 secretref:v1:... 的原样使用；
//   - 看起来是明文的（或写在 api_key 字段里的）先存入平台安全存储，再拿回 ref；
//   - 两个字段同时给出视为歧义，直接 400。
//
// 明文只在这个函数里作为局部变量存在，不返回、不落库、不进日志。
func (h *handler) resolveKeyRef(apiKeyRef, plainKey string) (string, error) {
	apiKeyRef = strings.TrimSpace(apiKeyRef)
	plainKey = strings.TrimSpace(plainKey)
	switch {
	case apiKeyRef == "" && plainKey == "":
		return "", nil
	case apiKeyRef != "" && plainKey != "":
		return "", badRequest("api_key 与 api_key_ref 不能同时提供")
	}
	value := apiKeyRef
	if plainKey != "" {
		value = plainKey
	}
	if secretref.IsRef(value) {
		return value, nil
	}
	if h.d.Secrets == nil {
		// 装配缺失时宁可失败，也不能把明文密钥写进数据库（安全红线）。
		return "", errors.New("admin: Secrets 未装配，无法把明文密钥存入平台安全存储")
	}
	ref, err := h.d.Secrets.Put(value)
	if err != nil {
		return "", err
	}
	if !secretref.IsRef(ref) {
		return "", errors.New("admin: 密钥后端未返回合法的 secretref")
	}
	return ref, nil
}

// getProvider 对应 GET /admin/providers/{id}。
func (h *handler) getProvider(w http.ResponseWriter, r *http.Request) {
	id := pathValue(r, "id")
	if strings.TrimSpace(id) == "" {
		h.readFail(w, r, badRequest("provider ID 不能为空"))
		return
	}
	p, err := h.d.Store.GetProvider(r.Context(), id)
	if err != nil {
		h.readFail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, viewProvider(p))
}

// listProviders 对应 GET /admin/providers?limit=&offset=&enabled=
// （enabled=true 只回 status=enabled）。
func (h *handler) listProviders(w http.ResponseWriter, r *http.Request) {
	limit, offset, ok := h.page(w, r)
	if !ok {
		return
	}
	onlyEnabled := false
	switch strings.TrimSpace(r.URL.Query().Get("enabled")) {
	case "true", "1":
		onlyEnabled = true
	case "", "false", "0":
	default:
		h.readFail(w, r, badRequest("enabled 只能是 true/false/1/0"))
		return
	}
	providers, err := h.d.Store.ListProviders(r.Context(), onlyEnabled)
	if err != nil {
		h.readFail(w, r, err)
		return
	}
	writeList(w, r, limit, offset, viewProviders(paginate(providers, limit, offset)))
}

type providerModelRequest struct {
	ModelID         string `json:"model_id"`
	UpstreamModelID string `json:"upstream_model_id"`
	Enabled         *bool  `json:"enabled"`
	Priority        *int64 `json:"priority"`
}

// upsertProviderModel 对应 POST /admin/providers/{id}/models：把模型目录条目
// 映射到该 provider 的上游模型名。priority 数值小者优先（契约 §11.1.5）。
func (h *handler) upsertProviderModel(w http.ResponseWriter, r *http.Request) {
	providerID := pathValue(r, "id")
	var req providerModelRequest
	if !h.decode(w, r, actionProviderModelUpsert, &req) {
		return
	}
	if strings.TrimSpace(providerID) == "" {
		h.fail(w, r, actionProviderModelUpsert, "", badRequest("provider ID 不能为空"))
		return
	}
	modelID := strings.TrimSpace(req.ModelID)
	if modelID == "" {
		h.fail(w, r, actionProviderModelUpsert, providerID, badRequest("model_id 不能为空"))
		return
	}
	upstream := strings.TrimSpace(req.UpstreamModelID)
	if upstream == "" {
		h.fail(w, r, actionProviderModelUpsert, modelID, badRequest("upstream_model_id 不能为空"))
		return
	}
	// 先确认两端都登记过：否则 SQLite 只会回一句外键失败（500），
	// 对后台来说「provider 不存在」与「模型不存在」是完全不同的处置。
	if _, err := h.d.Store.GetProvider(r.Context(), providerID); err != nil {
		h.fail(w, r, actionProviderModelUpsert, providerID, err)
		return
	}
	if _, err := h.d.Store.GetModel(r.Context(), modelID); err != nil {
		h.fail(w, r, actionProviderModelUpsert, modelID, err)
		return
	}

	pm := model.ProviderModel{
		ProviderID:      providerID,
		ModelID:         modelID,
		UpstreamModelID: upstream,
		Enabled:         true,
	}
	if req.Enabled != nil {
		pm.Enabled = *req.Enabled
	}
	if req.Priority != nil {
		pm.Priority = *req.Priority
	}
	if pm.Priority < 0 {
		h.fail(w, r, actionProviderModelUpsert, modelID, badRequest("priority 不能为负（数值小者优先）"))
		return
	}
	if err := h.d.Store.UpsertProviderModel(r.Context(), pm); err != nil {
		h.fail(w, r, actionProviderModelUpsert, modelID, err)
		return
	}
	if !h.auditOK(w, r, actionProviderModelUpsert, modelID, map[string]any{
		"provider_id":       pm.ProviderID,
		"upstream_model_id": pm.UpstreamModelID,
		"enabled":           pm.Enabled,
		"priority":          pm.Priority,
	}) {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, struct {
		ProviderID      string `json:"provider_id"`
		ModelID         string `json:"model_id"`
		UpstreamModelID string `json:"upstream_model_id"`
		Enabled         bool   `json:"enabled"`
		Priority        int64  `json:"priority"`
	}{pm.ProviderID, pm.ModelID, pm.UpstreamModelID, pm.Enabled, pm.Priority})
}

// pickID 取主键：id 与 model_id 同时出现且不一致时报 400。
func pickID(id, alias string) (string, error) {
	id = strings.TrimSpace(id)
	alias = strings.TrimSpace(alias)
	if id != "" && alias != "" && id != alias {
		return "", badRequest("id 与 model_id 不一致")
	}
	if id == "" {
		id = alias
	}
	if id == "" {
		return "", badRequest("id 不能为空")
	}
	return id, nil
}

// pickCapabilities 归一 capabilities：对象字段优先，其次才是 JSON 字符串字段；
// 两个都给了也以对象为准（对象字段表达的意图更直接）。返回的 provided 表示
// 请求里是否真的要改这一项（false 时调用方保留库中原值）。
func pickCapabilities(obj json.RawMessage, str *string) (string, bool, error) {
	if len(obj) > 0 {
		v, err := validateJSONObject(string(obj), "capabilities")
		return v, err == nil, err
	}
	if str != nil {
		v, err := validateJSONObject(*str, "capabilities_json")
		return v, err == nil, err
	}
	return "", false, nil
}

// pickConfig 与 pickCapabilities 同理，用于 provider 的 config/config_json。
func pickConfig(obj json.RawMessage, str *string) (string, bool, error) {
	if len(obj) > 0 {
		v, err := validateJSONObject(string(obj), "config")
		return v, err == nil, err
	}
	if str != nil {
		v, err := validateJSONObject(*str, "config_json")
		return v, err == nil, err
	}
	return "", false, nil
}
