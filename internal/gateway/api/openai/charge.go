package openai

import (
	"context"

	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/model"
)

// charge 管理一次请求的额度预占生命周期：held → settled | released。
//
// 为什么单独抽出来：§11.4 要求「任何失败路径都必须归还预占」，而调用路径上分支很多
// （无候选、候选全失败、上游不可重试错误、客户端取消、panic 展开）。把状态机收在
// 一个类型里，就不必在每条分支上重复写 release，也不会出现「结算之后又被归还」
// 这种双重退款。所有归还/结算都跑在脱离客户端取消的 context 上（见 cleanupCtx）。
type charge struct {
	h   *handler
	rsv model.Reservation
	// done 为 true 表示这次预占已有终局（已结算或已归还），后续调用全部忽略。
	done bool
}

// settle 用上游真实 usage 结算并落 usage 行（status 由 quota 写成 "settled"）。
//
// 事务边界：结算与写 usage 在 quota 内部是两段写（先账本、后 usage）。因此这里失败时
// **无法判定**账本是否已生效，绝不回退去 Release —— 若账本已生效，Release 会把预占
// 再还一次（双重退款）；若没生效，预占仍是 held，由 ReapExpired 在 TTL 后回收，
// 不会永久占用用户额度。错误一律落到日志，不阻塞给客户端的响应。
func (c *charge) settle(parent context.Context, rec model.UsageRecord) {
	if c.done {
		return
	}
	c.done = true

	ctx, cancel := c.h.cleanupCtx(parent)
	defer cancel()

	out, err := c.h.d.Quota.SettleWithUsage(ctx, c.rsv, rec, c.h.d.price())
	if err != nil {
		c.h.log().Error(ctx, "结算失败（账本可能已生效，预占保留给回收器处理）", map[string]any{
			"request_id":  rec.RequestID,
			"user_id":     rec.UserID,
			"model":       rec.ModelID,
			"reservation": c.rsv.ID,
			"error":       err.Error(),
		})
		return
	}
	c.h.log().Info(ctx, "请求已结算", map[string]any{
		"request_id":    out.RequestID,
		"provider_id":   rec.ProviderID,
		"model":         rec.ModelID,
		"input_tokens":  rec.InputTokens,
		"output_tokens": rec.OutputTokens,
		"cost_micro":    out.CostMicro,
		"latency_ms":    rec.LatencyMS,
		"outcome":       OutcomeSettled,
	})
}

// release 归还预占并落一条**未结算**的终态 usage 行（cost_micro=0）。
//
// 为什么在失败路径上也要写 usage 行：§11.5 要求流式失败也必须落 usage 行并标明终态。
// 该行的 token 数如实取自上游（没报就是 0），成本记 0 —— 预占已归还，这次请求没有计费。
func (c *charge) release(parent context.Context, rec model.UsageRecord, outcome, reason string) {
	if c.done {
		return
	}
	c.done = true
	c.h.releaseReservation(parent, c.rsv, rec, outcome, reason)
}

// releaseOnly 只归还预占，不写 usage 行（请求还没真正打到任何上游：无可用候选、目录读取失败）。
func (c *charge) releaseOnly(parent context.Context, reason string) {
	if c.done {
		return
	}
	c.done = true

	ctx, cancel := c.h.cleanupCtx(parent)
	defer cancel()
	if err := c.h.d.Quota.Release(ctx, c.rsv, reason); err != nil {
		c.h.log().Error(ctx, "预占归还失败（额度将在预占超时后被回收）", map[string]any{
			"reservation": c.rsv.ID,
			"reason":      reason,
			"error":       err.Error(),
		})
	}
}

// abandon 是顶层 defer 的兜底：走到这里说明某条路径既没结算也没归还（新增分支漏处理、
// 或 panic 展开）。归还额度优先，避免用户额度被永久占住。
func (c *charge) abandon(parent context.Context, requestID string) {
	if c.done {
		return
	}
	c.h.log().Warn(parent, "请求异常收尾，兜底归还预占", map[string]any{
		"request_id":  requestID,
		"reservation": c.rsv.ID,
	})
	c.releaseOnly(parent, "aborted")
}

// releaseReservation 归还预占 + 写终态 usage 行。
func (h *handler) releaseReservation(parent context.Context, rsv model.Reservation, rec model.UsageRecord, outcome, reason string) {
	ctx, cancel := h.cleanupCtx(parent)
	defer cancel()

	if err := h.d.Quota.Release(ctx, rsv, reason); err != nil {
		h.log().Error(ctx, "预占归还失败（额度将在预占超时后被回收）", map[string]any{
			"reservation": rsv.ID,
			"reason":      reason,
			"error":       err.Error(),
		})
	}
	if rec.Status == "" {
		rec.Status = outcome
	}
	if h.d.Usage == nil {
		h.log().Error(ctx, "未装配 usage 写入器，终态未能落库", map[string]any{
			"request_id": rec.RequestID,
			"outcome":    outcome,
		})
		return
	}
	// InsertUsage 以 request_id 为主键且幂等（冲突视为已存在），重放不会写重复行。
	if err := h.d.Usage.InsertUsage(ctx, rec); err != nil {
		h.log().Error(ctx, "终态 usage 写入失败", map[string]any{
			"request_id": rec.RequestID,
			"outcome":    outcome,
			"error":      err.Error(),
		})
		return
	}
	h.log().Warn(ctx, "请求终止（预占已归还）", map[string]any{
		"request_id":  rec.RequestID,
		"provider_id": rec.ProviderID,
		"model":       rec.ModelID,
		"outcome":     outcome,
		"reason":      reason,
		"latency_ms":  rec.LatencyMS,
	})
}
