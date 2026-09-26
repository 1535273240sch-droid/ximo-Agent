package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// 入站协议标识（与主仓库 internal/gateway/model 的同名常量一致；插件不 import 它，
// 只复刻取值，避免跨 module 依赖）。
const (
	// ProtocolOpenAIChat 是 V1 上游当前唯一实现的协议，也是适配时的首选。
	ProtocolOpenAIChat = "openai-chat"
	// ProtocolAnthropicMessages 是 /v1/messages 形态（Anthropic 系客户端）。
	ProtocolAnthropicMessages = "anthropic-messages"
)

// Capabilities 是模型能力（服务端 deviceResponse 之外的白名单投影字段）。
type Capabilities struct {
	Stream    bool `json:"stream"`
	Vision    bool `json:"vision"`
	Tools     bool `json:"tools"`
	Reasoning bool `json:"reasoning"`
}

// Model 是 GET /v1/models 的一条模型（服务端已只返回 enabled 的）。
type Model struct {
	ID           string       `json:"id"`
	DisplayName  string       `json:"display_name"`
	Provider     string       `json:"provider"`
	Protocols    []string     `json:"protocols"`
	Capabilities Capabilities `json:"capabilities"`
	Enabled      bool         `json:"enabled"`
}

// OffersProtocol 判定模型是否声明支持某个入站协议。
func (m Model) OffersProtocol(protocol string) bool {
	for _, p := range m.Protocols {
		if p == protocol {
			return true
		}
	}
	return false
}

// OffersOpenAIChat 判定模型是否支持 openai-chat 协议（V1 上游唯一协议）。
func (m Model) OffersOpenAIChat() bool { return m.OffersProtocol(ProtocolOpenAIChat) }

type modelsResponse struct {
	Data []Model `json:"data"`
}

// ListModels 拉取模型目录（GET /v1/models，用户态）。
//
// 未携带凭据时直接报 ErrNoCredential（不发必然 401 的请求）。服务端的 provider /
// protocols 由上游候选推导，熔断或未配置上游时可能为空 —— 那是"当前没有可路由
// 上游"的真实信号，本函数不做补偿，交由 PickModel/调用方处理。
func (c *Client) ListModels(ctx context.Context, cred Cred) ([]Model, error) {
	bearer := cred.Bearer()
	if bearer == "" {
		return nil, fmt.Errorf("%w：无法调用 /v1/models，请先登录", ErrNoCredential)
	}
	var out modelsResponse
	err := c.do(ctx, request{
		method:  http.MethodGet,
		path:    pathModels,
		bearer:  bearer,
		out:     &out,
		secrets: []string{bearer},
	})
	if err != nil {
		return nil, fmt.Errorf("读取模型列表失败: %w", err)
	}
	if out.Data == nil {
		out.Data = []Model{}
	}
	return out.Data, nil
}

// PickModel 选出一个要写进 Agent 的模型（契约 §3）。
//
// want 为空：优先第一个 enabled 且支持 openai-chat 的模型（V1 上游唯一协议），
// 其次第一个 enabled；两者都没有时返回 ErrNoModels。
// want 非空：精确匹配模型 ID（其次是大小写不敏感匹配）；不存在或已停用则返回
// ErrModelUnavailable，错误文本里带上**可选模型列表**，便于用户直接改 --model。
func (c *Client) PickModel(models []Model, want string) (Model, error) {
	want = strings.TrimSpace(want)

	if want != "" {
		for _, m := range models {
			if m.ID == want {
				return pickable(m, want, models)
			}
		}
		for _, m := range models {
			if strings.EqualFold(m.ID, want) {
				return pickable(m, want, models)
			}
		}
		return Model{}, fmt.Errorf("%w：网关未提供模型 %q；可选模型：%s",
			ErrModelUnavailable, want, modelIDList(models))
	}

	for _, m := range models {
		if m.Enabled && m.OffersOpenAIChat() {
			return m, nil
		}
	}
	for _, m := range models {
		if m.Enabled {
			return m, nil
		}
	}
	if len(models) == 0 {
		return Model{}, fmt.Errorf("%w：/v1/models 返回空列表（请管理员在网关启用模型与上游）", ErrNoModels)
	}
	return Model{}, fmt.Errorf("%w：没有任何 enabled 模型；可选模型：%s", ErrNoModels, modelIDList(models))
}

// pickable 处理"ID 命中了但模型已停用"的情况：不静默返回一个不可用的模型。
func pickable(m Model, want string, all []Model) (Model, error) {
	if !m.Enabled {
		return Model{}, fmt.Errorf("%w：模型 %q 已停用；可选模型：%s",
			ErrModelUnavailable, want, modelIDList(all))
	}
	return m, nil
}

// modelIDList 拼出可选模型列表，供错误信息引导用户修正 --model。
// 条数很多时截断，避免错误文本变成一堵墙。
func modelIDList(models []Model) string {
	const maxShown = 40
	ids := make([]string, 0, len(models))
	for i, m := range models {
		if i == maxShown {
			ids = append(ids, fmt.Sprintf("…（共 %d 个）", len(models)))
			break
		}
		id := m.ID
		if m.DisplayName != "" && m.DisplayName != m.ID {
			id = fmt.Sprintf("%s(%s)", m.ID, m.DisplayName)
		}
		if !m.Enabled {
			id += "[已停用]"
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return "(列表为空)"
	}
	return strings.Join(ids, ", ")
}

// UsageRecord 是 GET /v1/usage 的一条记录。
//
// CostMicro 的单位是**微额度**（1e-6 credit，整数），与网关账本一致；展示成
// credit 时除以 1e6。
type UsageRecord struct {
	RequestID    string `json:"request_id"`
	ModelID      string `json:"model_id"`
	ProviderID   string `json:"provider_id"`
	Status       string `json:"status"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	LatencyMS    int64  `json:"latency_ms"`
	CostMicro    int64  `json:"cost_micro"`
	CreatedAt    int64  `json:"created_at"`
}

type usageResponse struct {
	Data []UsageRecord `json:"data"`
}

// ListUsage 拉取当前用户名下的用量（GET /v1/usage，用户态）。
//
// limit <= 0 时不带该参数（由服务端取默认值 100）；服务端上限 200，超出会被静默
// 收紧（这里是服务端的真实行为，本函数不额外报错）。用户身份只来自凭据，客户端
// 无法指定 user_id —— 服务端也不接受该参数。
func (c *Client) ListUsage(ctx context.Context, cred Cred, limit int) ([]UsageRecord, error) {
	bearer := cred.Bearer()
	if bearer == "" {
		return nil, fmt.Errorf("%w：无法调用 /v1/usage，请先登录", ErrNoCredential)
	}
	if limit < 0 {
		return nil, errors.New("limit 不能为负")
	}
	path := pathUsage
	if limit > 0 {
		path += "?" + url.Values{"limit": []string{strconv.Itoa(limit)}}.Encode()
	}

	var out usageResponse
	err := c.do(ctx, request{
		method:  http.MethodGet,
		path:    path,
		bearer:  bearer,
		out:     &out,
		secrets: []string{bearer},
	})
	if err != nil {
		return nil, fmt.Errorf("读取用量失败: %w", err)
	}
	if out.Data == nil {
		out.Data = []UsageRecord{}
	}
	return out.Data, nil
}
