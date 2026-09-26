package admin

import (
	"net/http"
	"strings"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/httpx"
)

type createKeyRequest struct {
	UserID     string `json:"user_id"`
	TTLSeconds int64  `json:"ttl_seconds"`
}

// createKey 对应 POST /admin/keys。
//
// 明文 API Key 只在响应里回一次（account.CreateAPIKey 的语义：库里只存 sha256），
// 不进审计 detail、不进日志、不回显于任何其它接口。
func (h *handler) createKey(w http.ResponseWriter, r *http.Request) {
	var req createKeyRequest
	if !h.decode(w, r, actionKeyCreate, &req) {
		return
	}
	if strings.TrimSpace(req.UserID) == "" {
		h.fail(w, r, actionKeyCreate, "", badRequest("user_id 不能为空"))
		return
	}
	if req.TTLSeconds < 0 {
		h.fail(w, r, actionKeyCreate, req.UserID, badRequest("ttl_seconds 不能为负（0 表示不过期）"))
		return
	}

	ttl := defaultAPIKeyTTL
	if req.TTLSeconds > 0 {
		ttl = time.Duration(req.TTLSeconds) * time.Second
	}
	plain, key, err := h.d.Accounts.CreateAPIKey(r.Context(), req.UserID, ttl)
	if err != nil {
		h.fail(w, r, actionKeyCreate, req.UserID, err)
		return
	}
	if !h.auditOK(w, r, actionKeyCreate, key.ID, map[string]any{
		"user_id":    key.UserID,
		"key_prefix": key.KeyPrefix,
		"expires_at": key.ExpiresAt,
	}) {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, struct {
		// APIKey 是明文，仅此一次返回。
		APIKey string  `json:"api_key"`
		Key    keyView `json:"key"`
	}{APIKey: plain, Key: viewKey(key)})
}

// listKeys 对应 GET /admin/keys?user_id=&limit=&offset=。
// store 的 ListAPIKeysByUser 不支持分页，因此在内存里切页（用户维度密钥数量有限）。
func (h *handler) listKeys(w http.ResponseWriter, r *http.Request) {
	limit, offset, ok := h.page(w, r)
	if !ok {
		return
	}
	userID, err := requireQuery(r, "user_id")
	if err != nil {
		h.readFail(w, r, err)
		return
	}
	keys, err := h.d.Store.ListAPIKeysByUser(r.Context(), userID)
	if err != nil {
		h.readFail(w, r, err)
		return
	}
	writeList(w, r, limit, offset, viewKeys(paginate(keys, limit, offset)))
}

// revokeKey 对应 DELETE /admin/keys/{id}。重复撤销是幂等的（store 语义），
// 但目标不存在返回 404，方便后台发现删错 ID。
func (h *handler) revokeKey(w http.ResponseWriter, r *http.Request) {
	id := pathValue(r, "id")
	if strings.TrimSpace(id) == "" {
		h.fail(w, r, actionKeyRevoke, id, badRequest("密钥 ID 不能为空"))
		return
	}
	if err := h.d.Store.RevokeAPIKey(r.Context(), id); err != nil {
		h.fail(w, r, actionKeyRevoke, id, err)
		return
	}
	if !h.auditOK(w, r, actionKeyRevoke, id, map[string]any{"status": "revoked"}) {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}{ID: id, Status: "revoked"})
}
