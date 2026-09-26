package admin

import (
	"net/http"
	"strings"

	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/httpx"
)

type createUserRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	GroupID  string `json:"group_id"`
}

// createUser 对应 POST /admin/users。口令只经 account.CreateUser 变成 PBKDF2 哈希，
// 响应体不含 password_hash（也不含口令本身）。
func (h *handler) createUser(w http.ResponseWriter, r *http.Request) {
	var req createUserRequest
	if !h.decode(w, r, actionUserCreate, &req) {
		return
	}
	if strings.TrimSpace(req.Username) == "" {
		h.fail(w, r, actionUserCreate, "", badRequest("username 不能为空"))
		return
	}
	if req.Password == "" {
		h.fail(w, r, actionUserCreate, "", badRequest("password 不能为空"))
		return
	}

	u, err := h.d.Accounts.CreateUser(r.Context(), req.Username, req.Password, req.GroupID)
	if err != nil {
		h.fail(w, r, actionUserCreate, "", err)
		return
	}
	if !h.auditOK(w, r, actionUserCreate, u.ID, map[string]any{
		"username": u.Username,
		"group_id": u.GroupID,
		"status":   u.Status,
	}) {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, viewUser(u))
}

// listUsers 对应 GET /admin/users?limit=&offset=。
func (h *handler) listUsers(w http.ResponseWriter, r *http.Request) {
	limit, offset, ok := h.page(w, r)
	if !ok {
		return
	}
	users, err := h.d.Store.ListUsers(r.Context(), limit, offset)
	if err != nil {
		h.readFail(w, r, err)
		return
	}
	writeList(w, r, limit, offset, viewUsers(users))
}

type userStatusRequest struct {
	Status string `json:"status"`
}

// setUserStatus 对应 POST /admin/users/{id}/status。
func (h *handler) setUserStatus(w http.ResponseWriter, r *http.Request) {
	id := pathValue(r, "id")
	var req userStatusRequest
	if !h.decode(w, r, actionUserStatus, &req) {
		return
	}
	status, err := validateUserStatus(req.Status)
	if err != nil {
		h.fail(w, r, actionUserStatus, id, err)
		return
	}
	if strings.TrimSpace(id) == "" {
		h.fail(w, r, actionUserStatus, id, badRequest("用户 ID 不能为空"))
		return
	}

	if err := h.d.Store.UpdateUserStatus(r.Context(), id, status); err != nil {
		h.fail(w, r, actionUserStatus, id, err)
		return
	}
	// 回读一次让响应带上完整的用户状态（UpdateUserStatus 只回 error）。
	u, err := h.d.Store.GetUser(r.Context(), id)
	if err != nil {
		h.fail(w, r, actionUserStatus, id, err)
		return
	}
	if !h.auditOK(w, r, actionUserStatus, id, map[string]any{"status": status}) {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, viewUser(u))
}
