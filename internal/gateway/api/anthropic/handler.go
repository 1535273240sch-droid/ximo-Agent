package anthropic

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/catalog"
	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/httpx"
	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/model"
	"github.com/ximo888ok-netizen/ximo-agent/internal/observability"
	"github.com/ximo888ok-netizen/ximo-agent/internal/provider"
	"github.com/ximo888ok-netizen/ximo-agent/internal/quota"
)

// 本文件实现 POST /v1/messages 的请求生命周期（契约 §11.4，顺序不可变）：
//
//	request_id → 认证 →（限流由 W-Server 中间件负责）→ 模型启用校验 → 额度预占 →
//	候选路由（协议过滤 openai-chat）→ 逐个候选尝试 → 结算 + 写 usage → 返回
//
// 依赖一律走本包自定义的窄接口（契约 §10.2）：*gwstore.Store / *catalog.Catalog /
// *upstream.Pool / *quota.Service / *account.Service 的方法签名逐字一致，可直接传入
// Deps 的对应字段。

// modelSource 提供模型启用校验（*gwstore.Store 满足）。
type modelSource interface {
	// GetModel 未找到返回 model.ErrNotFound。
	GetModel(ctx context.Context, modelID string) (model.ModelSpec, error)
}

// candidateSource 提供候选路由（*catalog.Catalog 满足）。
type candidateSource interface {
	Candidates(ctx context.Context, modelID string) ([]catalog.Candidate, error)
}

// clientPool 提供上游客户端（*upstream.Pool 满足）。
//
// 刻意只取 ClientFor：健康度过滤与 MarkFailure/MarkSuccess 不需要本包参与 ——
// catalog 已用 Probe 过滤熔断中的 provider，而 provider.Client 内部已经记账，
// 本包再调一次 MarkFailure 会把失败重复计入熔断器。
type clientPool interface {
	ClientFor(ctx context.Context, spec model.ProviderSpec) (*provider.Client, error)
}

// quotaService 是额度链路需要的最小能力（*quota.Service 满足）。
type quotaService interface {
	Reserve(ctx context.Context, userID, requestID string, estimateMicro int64) (model.Reservation, error)
	SettleWithUsage(ctx context.Context, r model.Reservation, u model.UsageRecord, priceMicroPerKTok int64) (model.UsageRecord, error)
	Release(ctx context.Context, r model.Reservation, reason string) error
}

// authenticator 校验用户态凭据（*account.Service 满足）。
type authenticator interface {
	VerifyAPIKey(ctx context.Context, plain string) (model.User, model.APIKey, error)
	VerifyAccess(ctx context.Context, accessToken string) (model.User, error)
}

// usageWriter 用于失败终态写 usage 行（*gwstore.Store 满足）。
type usageWriter interface {
	// InsertUsage 以 request_id 为主键幂等写入。
	InsertUsage(ctx context.Context, u model.UsageRecord) error
}

// Deps 是本包的装配参数。字段为窄接口类型，main 直接把共享组件传进来即可：
//
//	anthropic.Deps{
//	    Models:   gwStore,        // *gwstore.Store
//	    Catalog:  cat,            // *catalog.Catalog
//	    Upstream: pool,           // *upstream.Pool
//	    Quota:    quotaSvc,       // *quota.Service
//	    Auth:     accountSvc,     // *account.Service
//	    Usage:    gwStore,        // *gwstore.Store（可选，失败终态用）
//	}
//
// 说明：本包自带认证（而非依赖 W-Server 中间件注入用户），因为 httpx 里没有跨包共享的
// 「已认证用户」context 键，且 W-Server 不得 import api 子包，无法反向传递。
type Deps struct {
	// Models 模型目录（模型不存在/未启用 → 404，不进上游）。必填。
	Models modelSource
	// Catalog 候选路由。必填。
	Catalog candidateSource
	// Upstream 上游客户端池。必填。
	Upstream clientPool
	// Quota 额度服务。必填。
	Quota quotaService
	// Auth 用户态凭据校验。必填（除非提供 AuthFunc）。
	Auth authenticator
	// AuthFunc 可选：由 W-Server 认证中间件解析已认证用户，与 api/openai 的
	// Deps.Auth 同形。非空时优先于 Auth —— 这样 main 可以用同一套装配代码接两个入口：
	// 要么注入中间件的用户解析函数，要么直接把 *account.Service 传进 Auth。
	AuthFunc func(r *http.Request) (model.User, error)
	// Usage 失败终态用量记录；nil 时失败路径不落 usage 行（仍会释放预占并记日志）。
	Usage usageWriter
	// PriceMicroPerKTok 单价（微单位/1K token）。V1 无真实价目表（契约 §11.6），
	// <=0 时用 quota.DefaultPriceMicroPerKTok 占位。
	PriceMicroPerKTok int64
	// Logger 结构化日志；缺省用 observability.DefaultLogger()。
	Logger *observability.Logger
}

// Routes 返回本包负责的路由（契约 §11.2）。
func Routes(d Deps) []httpx.Route {
	h := &handler{d: d}
	return []httpx.Route{
		{Pattern: "POST /v1/messages", Handler: h.messages},
	}
}

type handler struct {
	d Deps
}

// callInfo 是一次上游尝试的关联信息，供落 usage 与日志使用。
type callInfo struct {
	requestID  string
	userID     string
	modelID    string
	providerID string
	latencyMS  int64
	inputTok   int64
	outputTok  int64
}

// reservationGuard 保证一次预占**恰好**有一个终局：结算或归还，二者必居其一。
//
// 为什么需要它：§11.4 要求任何失败路径都归还预占，但分支很多（无候选、候选全失败、
// 不可重试错误、客户端取消、中途断流、panic 展开）。没有这个状态机，漏掉一条分支就会
// 把用户额度占住到预占 TTL 到期；写错一条分支（先结算再归还）则会双重退款。
// 所有归还/结算都跑在脱离客户端取消的 context 上（见 cleanupCtx）。
//
// 与 api/openai 的 charge 类型语义一致（那边是独立实现），待其抽出共享执行器后两边应收敛。
type reservationGuard struct {
	h   *handler
	rsv model.Reservation
	// done 为 true 表示已有终局，后续调用一律忽略。
	done bool
}

// settle 用上游真实 usage 结算并落 usage 行。
func (g *reservationGuard) settle(ctx context.Context, info callInfo) {
	if g.done {
		return
	}
	g.done = true
	g.h.settleCall(ctx, g.rsv, info)
}

// fail 归还未结算的预占并落终态 usage 行。
func (g *reservationGuard) fail(ctx context.Context, info callInfo, status, reason string) {
	if g.done {
		return
	}
	g.done = true
	g.h.failCall(ctx, g.rsv, info, status, reason)
}

// abandon 是顶层 defer 的兜底：走到这里说明某条分支既没结算也没归还
// （新增分支漏处理，或 panic 展开）。归还额度优先。
func (g *reservationGuard) abandon(ctx context.Context) {
	if g.done {
		return
	}
	g.done = true
	g.h.log().Warn(ctx, "anthropic: 请求异常收尾，兜底归还预占", map[string]any{
		"request_id":     g.rsv.RequestID,
		"reservation_id": g.rsv.ID,
	})
	status := usageStatusUpstreamError
	if ctx.Err() != nil {
		status = usageStatusClientCanceled
	}
	g.h.failCall(ctx, g.rsv, callInfo{
		requestID: g.rsv.RequestID,
		userID:    g.rsv.UserID,
	}, status, "aborted")
}

func (h *handler) messages(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if missing := h.missingDeps(); missing != "" {
		// 装配缺失是部署错误：明确 503 + Error 日志，不要退化成 500 让排查者猜。
		h.log().Error(ctx, "anthropic: 依赖未装配完整，拒绝请求", map[string]any{"missing": missing})
		httpx.WriteError(w, r, http.StatusServiceUnavailable, "server_misconfigured", "gateway dependency not wired")
		return
	}

	// §11.4 顺序：认证先于解析请求体（未认证者不必浪费解析）。
	user, err := h.authenticate(ctx, r)
	if err != nil {
		httpx.WriteMappedError(w, r, err, "invalid credentials")
		return
	}

	var req messagesRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	tr, err := translateRequest(&req)
	if err != nil {
		var re *requestError
		if errors.As(err, &re) {
			httpx.WriteError(w, r, re.status, re.code, re.message)
			return
		}
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "malformed request")
		return
	}
	if tr.stream {
		if _, ok := w.(http.Flusher); !ok {
			// 必须在调用上游之前判定：一旦开始上游调用就再也回不了普通 JSON 错误。
			httpx.WriteError(w, r, http.StatusInternalServerError, "streaming_unsupported", "streaming is not supported by this server")
			return
		}
	}

	requestID := httpx.RequestID(r)
	if requestID == "" {
		// 中间件未注入 request_id 时自留一个：它是额度幂等键的一半，不能为空。
		requestID = newID("req")
	}
	w.Header().Set("Request-Id", requestID)

	// 模型启用校验（§11.4：不存在/未启用 → 404，不进上游）。
	spec, err := h.d.Models.GetModel(ctx, tr.model)
	if err != nil {
		if errors.Is(err, model.ErrNotFound) {
			httpx.WriteError(w, r, http.StatusNotFound, "model_not_found", "model not found or not enabled")
			return
		}
		h.log().Error(ctx, "anthropic: 读取模型目录失败", map[string]any{
			"request_id": requestID,
			"model":      tr.model,
			"error":      err.Error(),
		})
		httpx.WriteError(w, r, http.StatusInternalServerError, "server_error", "model lookup failed")
		return
	}
	if !spec.Enabled {
		httpx.WriteError(w, r, http.StatusNotFound, "model_not_found", "model not found or not enabled")
		return
	}

	if len(tr.warnings) > 0 {
		h.log().Warn(ctx, "anthropic: 入站参数存在无法转发的部分", map[string]any{
			"request_id": requestID,
			"model":      tr.model,
			"warnings":   tr.warnings,
		})
	}

	// 额度预占（额度不足 → 402，且**不进上游**）。
	estimate := quota.EstimateMicro(tr.estimatedInputTokens(), int64(tr.maxTokens), h.price())
	reservation, err := h.d.Quota.Reserve(ctx, user.ID, requestID, estimate)
	if err != nil {
		httpx.WriteMappedError(w, r, err, "quota reservation failed")
		return
	}

	cands, err := h.d.Catalog.Candidates(ctx, tr.model)
	if err != nil {
		h.release(ctx, reservation, "catalog_error")
		h.log().Error(ctx, "anthropic: 候选路由失败", map[string]any{
			"request_id": requestID,
			"model":      tr.model,
			"error":      err.Error(),
		})
		httpx.WriteError(w, r, http.StatusInternalServerError, "server_error", "routing failed")
		return
	}
	cands = filterProtocol(cands, model.ProtocolOpenAIChat)
	if len(cands) == 0 {
		// 候选全不健康/协议不匹配：必须释放预占（§11.4）。
		h.release(ctx, reservation, "no_candidate")
		httpx.WriteError(w, r, http.StatusBadGateway, "provider_unavailable", "no upstream provider could serve this request")
		return
	}

	if tr.stream {
		h.streamMessage(w, r, tr, requestID, reservation, cands)
		return
	}
	h.completeMessage(w, r, tr, requestID, reservation, cands)
}

// completeMessage 是非流式路径：逐个候选尝试，成功后结算并返回 Anthropic 响应。
func (h *handler) completeMessage(w http.ResponseWriter, r *http.Request, tr *translated, requestID string, reservation model.Reservation, cands []catalog.Candidate) {
	ctx := r.Context()
	guard := &reservationGuard{h: h, rsv: reservation}
	defer guard.abandon(ctx)

	var lastErr error
	var lastInfo callInfo

	for _, cand := range cands {
		client, err := h.d.Upstream.ClientFor(ctx, cand.Provider)
		if err != nil {
			// 构建失败（缺 endpoint/密钥引用）不换熔断账，直接试下一个候选。
			lastErr = err
			lastInfo = h.baseInfo(requestID, reservation, tr.model, cand)
			h.log().Warn(ctx, "anthropic: 上游客户端构建失败", map[string]any{
				"request_id":  requestID,
				"provider_id": cand.Provider.ID,
				"error":       err.Error(),
			})
			continue
		}

		info := h.baseInfo(requestID, reservation, tr.model, cand)
		start := time.Now()
		resp, err := client.Complete(ctx, tr.completionRequest(cand.UpstreamModel, requestID))
		info.latencyMS = time.Since(start).Milliseconds()

		if err != nil {
			lastErr, lastInfo = err, info
			// §11.4：不可重试错误（401/400/…）不换候选，直接返回并释放预占。
			if !retryableForCandidate(err) {
				guard.fail(ctx, info, failureStatus(err, ctx.Err()), "upstream_rejected")
				writeUpstreamError(w, r, err)
				return
			}
			h.log().Warn(ctx, "anthropic: 候选失败，尝试下一个上游", map[string]any{
				"request_id":  requestID,
				"provider_id": cand.Provider.ID,
				"error_class": provider.Classify(err).Class.String(),
			})
			continue
		}

		if resp.Usage != nil {
			info.inputTok = int64(resp.Usage.PromptTokens)
			info.outputTok = int64(resp.Usage.CompletionTokens)
		}
		out := translateResponse(resp, tr.model, tr.stopSequences)
		guard.settle(ctx, info)
		httpx.WriteJSON(w, http.StatusOK, out)
		return
	}

	guard.fail(ctx, lastInfo, failureStatus(lastErr, ctx.Err()), "no_candidate_succeeded")
	writeUpstreamError(w, r, lastErr)
}

// authenticate 解析 Authorization: Bearer <API key 或 access token>（契约 §11.3）。
// 先按 ximo_sk_ 前缀走 API Key，其余按 access token（account 侧会校验 gwa_ 前缀）。
// 装配方提供了 AuthFunc（中间件解析结果）时以它为准，避免两处认证口径不一致。
func (h *handler) authenticate(ctx context.Context, r *http.Request) (model.User, error) {
	if h.d.AuthFunc != nil {
		return h.d.AuthFunc(r)
	}
	token := httpx.Bearer(r)
	if token == "" {
		return model.User{}, model.ErrBadCredentials
	}
	if strings.HasPrefix(token, "ximo_sk_") {
		u, _, err := h.d.Auth.VerifyAPIKey(ctx, token)
		return u, err
	}
	return h.d.Auth.VerifyAccess(ctx, token)
}

// baseInfo 构造一次尝试的关联信息（latency 与 token 由调用方补）。
//
// modelID 记**对外模型 ID**（而不是映射到的上游模型名）：usage 行要能按网关模型聚合，
// 上游模型名属于路由细节，不进账目。
func (h *handler) baseInfo(requestID string, reservation model.Reservation, modelID string, cand catalog.Candidate) callInfo {
	return callInfo{
		requestID:  requestID,
		userID:     reservation.UserID,
		modelID:    modelID,
		providerID: cand.Provider.ID,
	}
}

// settleCall 用上游真实 usage 结算并落 usage 行。
//
// 结算失败（写库失败）不让已生成的响应变成 5xx —— 用户已经拿到内容，把它变成错误只会
// 让客户端重试并再次计费；但绝不静默：记 Error 日志。V1 没有 outbox 异步重试（契约 §11.6）。
func (h *handler) settleCall(ctx context.Context, reservation model.Reservation, info callInfo) {
	cctx, cancel := cleanupCtx(ctx)
	defer cancel()

	rec := model.UsageRecord{
		RequestID:    info.requestID,
		UserID:       info.userID,
		ModelID:      info.modelID,
		ProviderID:   info.providerID,
		InputTokens:  info.inputTok,
		OutputTokens: info.outputTok,
		LatencyMS:    info.latencyMS,
	}
	settled, err := h.d.Quota.SettleWithUsage(cctx, reservation, rec, h.price())
	if err != nil {
		h.log().Error(cctx, "anthropic: 结算失败（账本与用量可能不一致，需人工核对）", map[string]any{
			"request_id":     info.requestID,
			"reservation_id": reservation.ID,
			"provider_id":    info.providerID,
			"error":          err.Error(),
		})
		return
	}
	h.log().Info(cctx, "anthropic: 结算完成", map[string]any{
		"request_id":    info.requestID,
		"provider_id":   info.providerID,
		"status":        settled.Status,
		"input_tokens":  settled.InputTokens,
		"output_tokens": settled.OutputTokens,
		"cost_micro":    settled.CostMicro,
		"latency_ms":    info.latencyMS,
	})
}

// failCall 处理上游失败/客户端取消：释放预占 + 落终态 usage 行（契约 §11.4/§11.5）。
//
// 语义选择（与 §11.5 的终态要求一致）：失败路径一律**归还预占**、usage 记 0 成本，
// 即使中途已产生部分 token 也不按估算收费 —— 契约 §6 禁止用估算值计费，而
// quota.SettleWithUsage 会把状态固定写成 settled，无法同时表达「按实收费 + 失败终态」。
// 已知 token 数会如实记下，便于事后核对。
func (h *handler) failCall(ctx context.Context, reservation model.Reservation, info callInfo, status, reason string) {
	cctx, cancel := cleanupCtx(ctx)
	defer cancel()

	if err := h.d.Quota.Release(cctx, reservation, reason); err != nil {
		h.log().Error(cctx, "anthropic: 释放预占失败（额度可能被占住，等待 ReapExpired 回收）", map[string]any{
			"request_id":     info.requestID,
			"reservation_id": reservation.ID,
			"reason":         reason,
			"error":          err.Error(),
		})
	}
	if h.d.Usage == nil {
		h.log().Warn(cctx, "anthropic: 未装配 usage 写入器，失败终态未能落库", map[string]any{
			"request_id": info.requestID,
			"status":     status,
		})
		return
	}
	if err := h.d.Usage.InsertUsage(cctx, model.UsageRecord{
		RequestID:    info.requestID,
		UserID:       info.userID,
		ModelID:      info.modelID,
		ProviderID:   info.providerID,
		Status:       status,
		InputTokens:  info.inputTok,
		OutputTokens: info.outputTok,
		LatencyMS:    info.latencyMS,
	}); err != nil {
		h.log().Error(cctx, "anthropic: 失败终态用量写入失败", map[string]any{
			"request_id": info.requestID,
			"status":     status,
			"error":      err.Error(),
		})
	}
}

// release 只归还预占（用于「未真正到达上游」的失败，如路由失败/无候选）。
// reason 只进日志与错误上下文：契约的 ReleaseTx 没有 reason 参数。
func (h *handler) release(ctx context.Context, reservation model.Reservation, reason string) {
	cctx, cancel := cleanupCtx(ctx)
	defer cancel()
	if err := h.d.Quota.Release(cctx, reservation, reason); err != nil {
		h.log().Error(cctx, "anthropic: 释放预占失败（额度可能被占住，等待 ReapExpired 回收）", map[string]any{
			"request_id":     reservation.RequestID,
			"reservation_id": reservation.ID,
			"reason":         reason,
			"error":          err.Error(),
		})
	}
}

// filterProtocol 按候选的 Provider.Protocol 过滤（契约 §11.1.4：协议过滤在调用方做）。
func filterProtocol(cands []catalog.Candidate, protocol string) []catalog.Candidate {
	out := make([]catalog.Candidate, 0, len(cands))
	for _, c := range cands {
		if c.Provider.Protocol == protocol {
			out = append(out, c)
		}
	}
	return out
}

// price 返回本次请求使用的单价（V1 无真实价目表，占位 1 micro/1K token）。
func (h *handler) price() int64 {
	if h.d.PriceMicroPerKTok > 0 {
		return h.d.PriceMicroPerKTok
	}
	return quota.DefaultPriceMicroPerKTok
}

// missingDeps 返回缺失的必填依赖名（空串表示齐备）。
func (h *handler) missingDeps() string {
	missing := make([]string, 0, 5)
	if h.d.Models == nil {
		missing = append(missing, "Models")
	}
	if h.d.Catalog == nil {
		missing = append(missing, "Catalog")
	}
	if h.d.Upstream == nil {
		missing = append(missing, "Upstream")
	}
	if h.d.Quota == nil {
		missing = append(missing, "Quota")
	}
	if h.d.Auth == nil && h.d.AuthFunc == nil {
		missing = append(missing, "Auth")
	}
	return strings.Join(missing, ",")
}

func (h *handler) log() *observability.Logger {
	if h.d.Logger != nil {
		return h.d.Logger
	}
	return observability.DefaultLogger()
}

var idCounter atomic.Uint64

// newID 生成「前缀_16位hex」ID。与 quota 包同构：网关侧不依赖 Agent 运行时的
// internal/types（§0.5 解耦），故本地保留一份；crypto/rand 失败时退化为计数器。
func newID(prefix string) string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		n := idCounter.Add(1)
		for i := range buf {
			buf[i] = byte(n >> (8 * (len(buf) - 1 - i)))
		}
	}
	return prefix + "_" + hex.EncodeToString(buf[:])
}
