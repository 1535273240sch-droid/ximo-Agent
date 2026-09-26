package meta

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/httpx"
)

// usageItem 是 /v1/usage 的单条记录。
//
// 白名单投影：不直接序列化 model.UsageRecord，也不回 user_id（调用方就是该用户
// 本人），避免后续给 UsageRecord 加内部字段时顺手带出去。
type usageItem struct {
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
	Data []usageItem `json:"data"`
}

// usage 返回当前用户自己的用量记录（用户态）。
//
// userID 只来自 requireUser（鉴权结果），不接受 query 参数指定用户；limit 超过
// maxPageSize 时静默收紧到上限（与 /v1/capabilities 声明一致），非法参数返回 400。
func (h *handlers) usage(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	if h.d.Store == nil {
		unavailable(w, r, "用量存储")
		return
	}

	limit, err := queryInt(r, "limit")
	if err != nil {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	offset, err := queryInt(r, "offset")
	if err != nil {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if limit > maxPageSize {
		limit = maxPageSize
	}

	records, err := h.d.Store.ListUsage(r.Context(), userID, limit, offset)
	if err != nil {
		h.log.Error(r.Context(), "meta: 读取用量记录失败",
			logFields(r, map[string]any{"user_id": userID, "error": err.Error()}))
		httpx.WriteMappedError(w, r, err, "读取用量记录失败")
		return
	}

	out := make([]usageItem, 0, len(records))
	for _, rec := range records {
		out = append(out, usageItem{
			RequestID:    rec.RequestID,
			ModelID:      rec.ModelID,
			ProviderID:   rec.ProviderID,
			Status:       rec.Status,
			InputTokens:  rec.InputTokens,
			OutputTokens: rec.OutputTokens,
			LatencyMS:    rec.LatencyMS,
			CostMicro:    rec.CostMicro,
			CreatedAt:    rec.CreatedAt,
		})
	}
	httpx.WriteJSON(w, http.StatusOK, usageResponse{Data: out})
}

// queryInt 解析非负整数查询参数；空值返回 0（交由底层取默认值）。
// 错误信息只回显调用方自己传来的参数名，不含内部信息。
func queryInt(r *http.Request, name string) (int, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("参数 %s 必须是整数", name)
	}
	if n < 0 {
		return 0, fmt.Errorf("参数 %s 不能为负", name)
	}
	return n, nil
}
