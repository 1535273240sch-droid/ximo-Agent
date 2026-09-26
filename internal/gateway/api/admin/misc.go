package admin

import (
	"net/http"
	"strings"

	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/httpx"
)

// listUsage 对应 GET /admin/usage?user_id=&limit=&offset=（最近优先）。
func (h *handler) listUsage(w http.ResponseWriter, r *http.Request) {
	limit, offset, ok := h.page(w, r)
	if !ok {
		return
	}
	userID, err := requireQuery(r, "user_id")
	if err != nil {
		h.readFail(w, r, err)
		return
	}
	usage, err := h.d.Store.ListUsage(r.Context(), userID, limit, offset)
	if err != nil {
		h.readFail(w, r, err)
		return
	}
	writeList(w, r, limit, offset, viewUsages(usage))
}

// listAudit 对应 GET /admin/audit?limit=&offset=（最近优先，含失败留痕）。
func (h *handler) listAudit(w http.ResponseWriter, r *http.Request) {
	limit, offset, ok := h.page(w, r)
	if !ok {
		return
	}
	logs, err := h.d.Store.ListAudit(r.Context(), limit, offset)
	if err != nil {
		h.readFail(w, r, err)
		return
	}
	writeList(w, r, limit, offset, viewAudits(logs))
}

type deviceApproveRequest struct {
	UserCode string `json:"user_code"`
	UserID   string `json:"user_id"`
}

// approveDevice 对应 POST /admin/device/approve：把设备码与用户绑定（文档 §5.2）。
// 用户码是给用户抄写的短码，不是密钥；仍不进 detail 之外的任何位置。
func (h *handler) approveDevice(w http.ResponseWriter, r *http.Request) {
	var req deviceApproveRequest
	if !h.decode(w, r, actionDeviceApprove, &req) {
		return
	}
	userCode := strings.TrimSpace(req.UserCode)
	userID := strings.TrimSpace(req.UserID)
	if userCode == "" {
		h.fail(w, r, actionDeviceApprove, userID, badRequest("user_code 不能为空"))
		return
	}
	if userID == "" {
		h.fail(w, r, actionDeviceApprove, userCode, badRequest("user_id 不能为空"))
		return
	}

	if err := h.d.Accounts.ApproveDeviceLogin(r.Context(), userCode, userID); err != nil {
		h.fail(w, r, actionDeviceApprove, userID, err)
		return
	}
	if !h.auditOK(w, r, actionDeviceApprove, userID, map[string]any{"user_code": userCode}) {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, struct {
		Status   string `json:"status"`
		UserCode string `json:"user_code"`
		UserID   string `json:"user_id"`
	}{Status: "approved", UserCode: userCode, UserID: userID})
}
