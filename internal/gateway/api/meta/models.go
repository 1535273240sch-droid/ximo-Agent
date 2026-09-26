package meta

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/httpx"
)

// modelCapabilities 是 /v1/models 对外声明的能力（文档 §5.3）。
//
// 它是 model.ModelSpec.CapabilitiesJSON 的**白名单投影**：库里的 JSON 可能带上
// 内部字段或后续新增字段，一律按下面四个字段解析后重建响应，绝不把原始 JSON
// 原文（或未知键）带回客户端。
type modelCapabilities struct {
	Stream    bool `json:"stream"`
	Vision    bool `json:"vision"`
	Tools     bool `json:"tools"`
	Reasoning bool `json:"reasoning"`
}

// modelItem 是 /v1/models 的单条模型。
type modelItem struct {
	ID           string            `json:"id"`
	DisplayName  string            `json:"display_name"`
	Provider     string            `json:"provider"`
	Protocols    []string          `json:"protocols"`
	Capabilities modelCapabilities `json:"capabilities"`
	Enabled      bool              `json:"enabled"`
}

type modelsResponse struct {
	Data []modelItem `json:"data"`
}

// models 返回对外模型目录（用户态）。
//
// provider / protocols 由目录的候选路由信息推导：provider 取优先级最高的可用候选
// （priority 数值小者优先，见 catalog.Candidates），protocols 是该模型全部候选的
// 协议去重集合。因此熔断打开或未配置上游时这两个字段可能为空 —— 那是「当前没有
// 可路由上游」的真实信号（模型本身仍是 enabled，契约只要求按 enabled 过滤）。
//
// 每个模型一次 Candidates 查询（N+1）：V1 的目录规模是几十条，且这是只读的
// 目录端点，换来的是「对外声明与真实路由一致」；规模变大时应改成一次批量查询。
func (h *handlers) models(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireUser(w, r); !ok {
		return
	}
	if h.d.Catalog == nil {
		unavailable(w, r, "模型目录服务")
		return
	}

	ms, err := h.d.Catalog.PublicModels(r.Context())
	if err != nil {
		h.log.Error(r.Context(), "meta: 读取模型目录失败", logFields(r, map[string]any{"error": err.Error()}))
		httpx.WriteMappedError(w, r, err, "读取模型目录失败")
		return
	}

	out := make([]modelItem, 0, len(ms))
	for _, m := range ms {
		// 双保险：即使实现方忽略 onlyEnabled，也不把停用模型暴露出去。
		if !m.Enabled {
			continue
		}
		caps := modelCapabilities{}
		if err := json.Unmarshal([]byte(m.CapabilitiesJSON), &caps); err != nil {
			// 能力字段写坏不该让整个目录不可用：降级为「全部不支持」并留下日志。
			// 只记模型 ID 与解析错误，不记原始 JSON（防内部字段进日志）。
			caps = modelCapabilities{}
			if !emptyJSON(m.CapabilitiesJSON) {
				h.log.Warn(r.Context(), "meta: 模型能力字段解析失败，按全部不支持处理",
					logFields(r, map[string]any{"model_id": m.ModelID, "error": err.Error()}))
			}
		}
		provider, protocols := h.routeInfo(r.Context(), m.ModelID)
		out = append(out, modelItem{
			ID:           m.ModelID,
			DisplayName:  m.DisplayName,
			Provider:     provider,
			Protocols:    protocols,
			Capabilities: caps,
			Enabled:      m.Enabled,
		})
	}
	httpx.WriteJSON(w, http.StatusOK, modelsResponse{Data: out})
}

// emptyJSON 判定能力字段是否「未声明」（空串或空白）。空值不算解析失败：
// 迁移里 NULL 会被读成空串，那不是错误数据。
func emptyJSON(s string) bool { return strings.TrimSpace(s) == "" }

// routeInfo 推导某模型的对外 provider 与 protocols。目录数据不一致（例如映射指向
// 不存在的 provider）时只记录并留空，不让单个模型的脏数据打掉整个 /v1/models。
func (h *handlers) routeInfo(ctx context.Context, modelID string) (string, []string) {
	cands, err := h.d.Catalog.Candidates(ctx, modelID)
	if err != nil {
		h.log.Error(ctx, "meta: 读取模型候选失败，provider/protocols 留空",
			map[string]any{"model_id": modelID, "error": err.Error()})
		return "", []string{}
	}
	provider := ""
	protocols := make([]string, 0, 1)
	seen := make(map[string]struct{}, len(cands))
	for _, c := range cands {
		if provider == "" {
			provider = c.Provider.ID
		}
		p := strings.TrimSpace(c.Provider.Protocol)
		if p == "" {
			continue
		}
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		protocols = append(protocols, p)
	}
	sort.Strings(protocols)
	return provider, protocols
}
