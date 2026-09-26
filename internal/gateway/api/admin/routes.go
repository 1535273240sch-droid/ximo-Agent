package admin

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/httpx"
	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/model"
	"github.com/ximo888ok-netizen/ximo-agent/internal/observability"
)

// 管理侧写操作的审计 action（gw_audit.action）。
const (
	actionUserCreate          = "user.create"
	actionUserStatus          = "user.status"
	actionKeyCreate           = "key.create"
	actionKeyRevoke           = "key.revoke"
	actionQuotaAdjust         = "quota.adjust"
	actionModelUpsert         = "model.upsert"
	actionProviderUpsert      = "provider.upsert"
	actionProviderModelUpsert = "provider.model.upsert"
	actionDeviceApprove       = "device.approve"
)

type handler struct{ d Deps }

// Routes 返回全部 /admin/* 路由（契约 §11.3）。Pattern 用 Go 1.22 ServeMux 语法，
// 由装配方（cmd/ximo-gateway）注册进 http.ServeMux；本包不关心中间件与端口。
func Routes(d Deps) []httpx.Route {
	h := &handler{d: d}
	return []httpx.Route{
		{Pattern: "POST /admin/users", Handler: h.guard(h.createUser)},
		{Pattern: "GET /admin/users", Handler: h.guard(h.listUsers)},
		{Pattern: "POST /admin/users/{id}/status", Handler: h.guard(h.setUserStatus)},

		{Pattern: "POST /admin/keys", Handler: h.guard(h.createKey)},
		{Pattern: "GET /admin/keys", Handler: h.guard(h.listKeys)},
		{Pattern: "DELETE /admin/keys/{id}", Handler: h.guard(h.revokeKey)},

		{Pattern: "POST /admin/quota/adjust", Handler: h.guard(h.adjustQuota)},
		{Pattern: "GET /admin/quota/accounts/{user_id}", Handler: h.guard(h.getQuotaAccount)},
		{Pattern: "GET /admin/quota/ledger", Handler: h.guard(h.listLedger)},

		{Pattern: "GET /admin/models", Handler: h.guard(h.listModels)},
		{Pattern: "POST /admin/models", Handler: h.guard(h.upsertModel)},
		{Pattern: "GET /admin/models/{id}", Handler: h.guard(h.getModel)},

		{Pattern: "GET /admin/providers", Handler: h.guard(h.listProviders)},
		{Pattern: "POST /admin/providers", Handler: h.guard(h.upsertProvider)},
		{Pattern: "GET /admin/providers/{id}", Handler: h.guard(h.getProvider)},
		{Pattern: "POST /admin/providers/{id}/models", Handler: h.guard(h.upsertProviderModel)},

		{Pattern: "GET /admin/usage", Handler: h.guard(h.listUsage)},
		{Pattern: "GET /admin/audit", Handler: h.guard(h.listAudit)},

		{Pattern: "POST /admin/device/approve", Handler: h.guard(h.approveDevice)},
	}
}

// guard 做 X-Admin-Token 校验与依赖装配检查。鉴权失败返回 401 且**不**落审计：
// 未通过身份验证的请求不该触发管理面写库（否则 401 路径可被用来放大写入）。
func (h *handler) guard(next func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.authorized(r) {
			httpx.WriteError(w, r, http.StatusUnauthorized, "unauthorized", "missing or invalid admin token")
			return
		}
		if err := h.ready(); err != nil {
			observability.LogError(r.Context(), "admin: 依赖未装配", map[string]any{"err": err.Error()})
			httpx.WriteError(w, r, http.StatusInternalServerError, "server_error", "admin API is not configured")
			return
		}
		next(w, r)
	}
}

// ready 检查必需依赖。admin 服务没有 store/account/quota 任何写都会失败，
// 与其在业务路径上逐个判空，不如在入口一次性挡住。
func (h *handler) ready() error {
	switch {
	case h.d.Store == nil:
		return errors.New("admin: Store 未装配")
	case h.d.Accounts == nil:
		return errors.New("admin: Accounts 未装配")
	case h.d.Quota == nil:
		return errors.New("admin: Quota 未装配")
	}
	return nil
}

// authorized 恒定时间比较 X-Admin-Token。先对两侧取 sha256 再比较：直接比字节会在
// 长度不等时提前返回，泄露 token 长度；哈希后比较与长度、前缀无关。
func (h *handler) authorized(r *http.Request) bool {
	want := h.d.AdminToken
	if want == "" {
		// 未配置管理令牌 = 管理面关闭（fail-closed），而不是「无令牌即可管理」。
		return false
	}
	gotSum := sha256.Sum256([]byte(r.Header.Get("X-Admin-Token")))
	wantSum := sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(gotSum[:], wantSum[:]) == 1
}

func (h *handler) now() int64 {
	if h.d.Now != nil {
		return h.d.Now()
	}
	return time.Now().UnixMilli()
}

// decode 解析请求体。失败时 httpx.DecodeJSON 已写 400，这里补一条失败审计
// （写操作即使被请求体格式挡住也要留痕，便于排查后台 bug）。
func (h *handler) decode(w http.ResponseWriter, r *http.Request, action string, dst any) bool {
	if httpx.DecodeJSON(w, r, dst) {
		return true
	}
	h.audit(r, action, "", resultError, map[string]any{"error": "malformed JSON body"})
	return false
}

// audit 落一条管理侧审计行（契约 §11.3）。
//
// 事务边界（诚实说明）：store 没有「业务写 + 审计」的合并事务接口，因此审计是
// **同一请求内的独立写**——业务写提交后审计失败不会回滚业务写。对应处理见 auditOK：
// 审计失败时返回 500 且文案写明「操作已生效但未留痕」，绝不假装成功。
func (h *handler) audit(r *http.Request, action, target, result string, detail map[string]any) error {
	entry := model.AuditLog{
		ID:        newID(prefixAudit),
		Actor:     actorAdmin,
		Action:    action,
		Target:    target,
		Result:    result,
		IP:        httpx.ClientIP(r),
		CreatedAt: h.now(),
	}
	if len(detail) > 0 {
		buf, err := json.Marshal(detail)
		if err != nil {
			return fmt.Errorf("admin: 审计 detail 编码失败: %w", err)
		}
		entry.DetailJSON = string(buf)
	}
	if err := h.d.Store.InsertAudit(r.Context(), entry); err != nil {
		return fmt.Errorf("admin: 审计写入失败 action=%s target=%s: %w", action, target, err)
	}
	return nil
}

// auditOK 落成功审计，返回 false 表示审计写失败（已写好 500 响应）。
// 契约要求写操作必须留痕，留不下痕就不该让调用方以为已经成功。
func (h *handler) auditOK(w http.ResponseWriter, r *http.Request, action, target string, detail map[string]any) bool {
	if err := h.audit(r, action, target, resultOK, detail); err != nil {
		observability.LogError(r.Context(), "admin: 审计写入失败",
			map[string]any{"action": action, "target": target, "err": err.Error()})
		httpx.WriteError(w, r, http.StatusInternalServerError, "audit_failed",
			"operation applied but the audit log could not be written")
		return false
	}
	return true
}

// fail 是写操作失败路径的统一出口：先落失败审计，再写错误响应。
// 审计本身失败不覆盖业务错误（只在日志里报），业务错误才是调用方要知道的。
func (h *handler) fail(w http.ResponseWriter, r *http.Request, action, target string, err error) {
	status, code := classify(err)
	msg := messageFor(err, status, code)
	if status >= http.StatusInternalServerError {
		observability.LogError(r.Context(), "admin: 写操作失败",
			map[string]any{"action": action, "target": target, "err": err.Error()})
	}
	if auditErr := h.audit(r, action, target, resultError, map[string]any{
		"status": status, "code": code, "error": msg,
	}); auditErr != nil {
		observability.LogError(r.Context(), "admin: 失败审计写入失败",
			map[string]any{"action": action, "target": target, "err": auditErr.Error()})
	}
	httpx.WriteError(w, r, status, code, msg)
}

// readFail 是只读路径的失败出口：只读操作不产生审计行。
func (h *handler) readFail(w http.ResponseWriter, r *http.Request, err error) {
	status, code := classify(err)
	msg := messageFor(err, status, code)
	if status >= http.StatusInternalServerError {
		observability.LogError(r.Context(), "admin: 读操作失败",
			map[string]any{"path": r.URL.Path, "err": err.Error()})
	}
	httpx.WriteError(w, r, status, code, msg)
}

// page 解析分页参数，非法时已写好 400 响应。
func (h *handler) page(w http.ResponseWriter, r *http.Request) (limit, offset int, ok bool) {
	limit, offset, err := parsePage(r)
	if err != nil {
		h.readFail(w, r, err)
		return 0, 0, false
	}
	return limit, offset, true
}

// listResponse 是列表接口的统一信封（契约 §11.3 的 {"data":[...]} 形状，
// 附带 limit/offset 回显便于分页调试）。
type listResponse struct {
	Data   any `json:"data"`
	Limit  int `json:"limit"`
	Offset int `json:"offset"`
}

func writeList(w http.ResponseWriter, r *http.Request, limit, offset int, data any) {
	httpx.WriteJSON(w, http.StatusOK, listResponse{Data: data, Limit: limit, Offset: offset})
}

// pathValue 取路径参数（Go 1.22 ServeMux 的 {name} 占位）。
func pathValue(r *http.Request, name string) string {
	return r.PathValue(name)
}

// requireQuery 取必填 query 参数。
func requireQuery(r *http.Request, name string) (string, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return "", badRequest(name + " 不能为空")
	}
	return v, nil
}

// ctxFor 便于在闭包里拿到请求上下文（保持 handler 里不重复写 r.Context()）。
func ctxFor(r *http.Request) context.Context { return r.Context() }
