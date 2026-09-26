package admin

import (
	"net/http"
	"strings"

	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/httpx"
	"github.com/ximo888ok-netizen/ximo-agent/internal/observability"
)

type quotaAdjustRequest struct {
	UserID         string `json:"user_id"`
	Kind           string `json:"kind"`
	Amount         int64  `json:"amount"`
	Reason         string `json:"reason"`
	IdempotencyKey string `json:"idempotency_key"`
}

type quotaAdjustResponse struct {
	Ledger ledgerView `json:"ledger"`
	// Account 是调整后的账户快照；快照读取失败时为 null（调整本身已经生效）。
	Account *accountView `json:"account"`
}

// adjustQuota 对应 POST /admin/quota/adjust（文档 §7.2 的额度增减/冻结入口）。
//
// 幂等：idempotency_key 必填，服务层按唯一约束返回既有账本行而不重复入账，
// 因此后台「点两次」「断线重试」都只会入账一次。
// kind 白名单与金额符号由 quota 服务与存储层校验（本包不重复维护白名单）。
func (h *handler) adjustQuota(w http.ResponseWriter, r *http.Request) {
	var req quotaAdjustRequest
	if !h.decode(w, r, actionQuotaAdjust, &req) {
		return
	}
	userID := strings.TrimSpace(req.UserID)
	if userID == "" {
		h.fail(w, r, actionQuotaAdjust, "", badRequest("user_id 不能为空"))
		return
	}
	if strings.TrimSpace(req.IdempotencyKey) == "" {
		h.fail(w, r, actionQuotaAdjust, userID,
			badRequest("idempotency_key 不能为空：它是额度调整的防重入唯一依据"))
		return
	}

	entry, err := h.d.Quota.Adjust(r.Context(), userID, strings.TrimSpace(req.Kind),
		req.Amount, req.Reason, operatorAdmin, strings.TrimSpace(req.IdempotencyKey))
	if err != nil {
		h.fail(w, r, actionQuotaAdjust, userID, err)
		return
	}

	// 账户快照是「顺手带上」的附加值，读失败不该把已经生效的调整报成失败。
	var acctView *accountView
	acct, acctErr := h.d.Quota.Account(r.Context(), userID)
	if acctErr != nil {
		observability.LogWarn(r.Context(), "admin: 额度调整后读取账户快照失败",
			map[string]any{"user_id": userID, "err": acctErr.Error()})
	} else {
		v := viewAccount(acct)
		acctView = &v
	}

	if !h.auditOK(w, r, actionQuotaAdjust, userID, map[string]any{
		"kind":            entry.Type,
		"amount":          entry.Amount,
		"idempotency_key": entry.IdempotencyKey,
		"ledger_id":       entry.ID,
		"balance_after":   entry.BalanceAfter,
		"reserved_after":  entry.ReservedAfter,
		"reason":          req.Reason,
	}) {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, quotaAdjustResponse{Ledger: viewLedger(entry), Account: acctView})
}

// getQuotaAccount 对应 GET /admin/quota/accounts/{user_id}。
func (h *handler) getQuotaAccount(w http.ResponseWriter, r *http.Request) {
	userID := pathValue(r, "user_id")
	if strings.TrimSpace(userID) == "" {
		h.readFail(w, r, badRequest("user_id 不能为空"))
		return
	}
	acct, err := h.d.Quota.Account(r.Context(), userID)
	if err != nil {
		h.readFail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, viewAccount(acct))
}

// listLedger 对应 GET /admin/quota/ledger?user_id=&limit=&offset=（时间正序，对账用）。
func (h *handler) listLedger(w http.ResponseWriter, r *http.Request) {
	limit, offset, ok := h.page(w, r)
	if !ok {
		return
	}
	userID, err := requireQuery(r, "user_id")
	if err != nil {
		h.readFail(w, r, err)
		return
	}
	entries, err := h.d.Store.ListLedger(r.Context(), userID, limit, offset)
	if err != nil {
		h.readFail(w, r, err)
		return
	}
	writeList(w, r, limit, offset, viewLedgers(entries))
}
