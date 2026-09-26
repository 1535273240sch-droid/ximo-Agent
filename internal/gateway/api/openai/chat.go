package openai

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/catalog"
	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/httpx"
	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/model"
	"github.com/ximo888ok-netizen/ximo-agent/internal/provider"
)

// requestSeq 兜底请求 ID 的进程内序号（crypto/rand 失败时的退化路径）。
var requestSeq atomic.Uint64

// maxRetryAfterSeconds 回给客户端的 Retry-After 上限（不把上游的超长等待原样透传）。
const maxRetryAfterSeconds int64 = 600

// chatCompletions 是 POST /v1/chat/completions 的处理器，按 §11.4 的生命周期编排：
//
//	request_id → 认证 → 限流(中间件) → 模型启用校验 → 额度预占
//	→ 候选路由（过滤协议）→ 逐个候选尝试 → 结算 + 写 usage → 返回
func (h *handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w, r) {
		return
	}

	// request_id 是额度预占幂等键的一半，绝不能为空：中间件未注入时本包兜底生成。
	reqID := httpx.RequestID(r)
	if reqID == "" {
		reqID = newRequestID()
	}
	r = r.WithContext(httpx.WithRequestID(r.Context(), reqID))
	ctx := r.Context()
	start := h.d.now()

	user, ok := h.authenticate(w, r)
	if !ok {
		return
	}
	// 限流是中间件的职责（§11.4 第 2 步），本包不重复实现，避免两套阈值打架。

	var wire chatRequest
	if !httpx.DecodeJSON(w, r, &wire) {
		return
	}
	norm, err := normalizeChatRequest(wire, reqID)
	if err != nil {
		writeRequestError(w, r, err)
		return
	}

	spec, ok := h.modelSpec(w, r, norm.Model)
	if !ok {
		return
	}
	if err := checkStreamSupported(spec, norm.Stream); err != nil {
		writeRequestError(w, r, err)
		return
	}

	// 额度预占。额度不足 → 402（httpx.StatusFor 映射 model.ErrInsufficientQuota），
	// 此时**不进上游**（§11.4、文档 §17）。
	rsv, err := h.d.Quota.Reserve(ctx, user.ID, reqID, h.d.estimateMicro(norm))
	if err != nil {
		httpx.WriteMappedError(w, r, err, "额度预占失败")
		return
	}
	c := &charge{h: h, rsv: rsv}
	base := model.UsageRecord{
		RequestID: reqID,
		UserID:    user.ID,
		ModelID:   norm.Model,
		CreatedAt: start.UnixMilli(),
	}
	// 兜底：任何没有走到 settle/release 的返回路径（含 panic 展开）都在这里归还额度。
	defer c.abandon(ctx, reqID)

	cands, err := h.d.Catalog.Candidates(ctx, norm.Model)
	if err != nil {
		h.log().Error(ctx, "候选路由失败", map[string]any{
			"request_id": reqID,
			"model":      norm.Model,
			"error":      err.Error(),
		})
		c.releaseOnly(ctx, "catalog_error")
		httpx.WriteError(w, r, http.StatusInternalServerError, "server_error", "网关内部错误")
		return
	}
	// 协议过滤在调用方做（§11.1.4）：V1 上游只支持 OpenAI 兼容 chat。
	cands = filterOpenAIChat(cands)
	if len(cands) == 0 {
		// 模型存在但当前没有可用上游（全部停用 / 熔断打开 / 协议不符）→ 502 + 归还预占。
		c.releaseOnly(ctx, "no_candidate")
		httpx.WriteError(w, r, http.StatusBadGateway, "provider_unavailable", "当前没有可用的上游服务")
		return
	}

	if norm.Stream {
		h.streamToClient(w, r, norm, cands, base, c, start)
		return
	}
	h.completeToClient(w, r, norm, cands, base, c, start)
}

// completeToClient 走非流式路径：逐个候选调用 Complete，成功则结算并回 JSON。
func (h *handler) completeToClient(w http.ResponseWriter, r *http.Request, norm *normalizedRequest,
	cands []catalog.Candidate, base model.UsageRecord, c *charge, start time.Time) {
	ctx := r.Context()

	var (
		lastErr      error
		lastProvider string
	)
	for _, cand := range cands {
		if ctx.Err() != nil {
			// 客户端/网关侧已取消：归还预占，不再打上游。
			base.ProviderID = lastProvider
			base.LatencyMS = time.Since(start).Milliseconds()
			c.release(ctx, base, OutcomeClientCanceled, "client_canceled")
			return
		}

		client, err := h.d.Upstream.Client(ctx, cand.Provider.ID)
		if err != nil {
			// 客户端构建失败（缺 endpoint / 密钥引用、协议不符等）属配置问题：
			// 算这个候选不可用，换下一个（配置错误不该让整次请求直接失败）。
			lastErr, lastProvider = err, cand.Provider.ID
			h.log().Warn(ctx, "上游客户端不可用，尝试下一个候选", map[string]any{
				"request_id":  norm.RequestID,
				"provider_id": cand.Provider.ID,
				"error":       err.Error(),
			})
			continue
		}
		lastProvider = cand.Provider.ID

		resp, err := client.Complete(ctx, norm.completionRequest(cand))
		if err == nil {
			rec := base
			rec.ProviderID = cand.Provider.ID
			rec.LatencyMS = time.Since(start).Milliseconds()
			if u := resp.Usage; u != nil {
				// 如实记录上游报的 token；上游没报就是 0（§6.1：不编造精确值）。
				rec.InputTokens = int64(u.PromptTokens)
				rec.OutputTokens = int64(u.CompletionTokens)
			}
			c.settle(ctx, rec)
			h.writeCompletion(w, r, norm, resp, start)
			return
		}

		lastErr = err
		class := provider.Classify(err)
		if isClientCanceled(ctx, class) {
			rec := base
			rec.ProviderID = cand.Provider.ID
			rec.LatencyMS = time.Since(start).Milliseconds()
			c.release(ctx, rec, OutcomeClientCanceled, "client_canceled")
			return
		}
		if provider.Retryable(class.Class) {
			// 只有可重试错误才换候选（§11.4）：DNS / 连接超时 / 429 / 5xx。
			h.log().Warn(ctx, "上游可重试错误，切换候选", map[string]any{
				"request_id":  norm.RequestID,
				"provider_id": cand.Provider.ID,
				"class":       class.Class.String(),
			})
			continue
		}
		// 不可重试（401/400/…）：不换候选，直接返回（§11.4）。
		break
	}

	rec := base
	rec.ProviderID = lastProvider
	rec.LatencyMS = time.Since(start).Milliseconds()
	outcome := outcomeFor(ctx, lastErr)
	c.release(ctx, rec, outcome, "upstream_failed")

	status, code, msg, retryAfter := http.StatusBadGateway, "provider_unavailable", "上游服务不可用，请稍后重试", int64(0)
	if lastErr != nil && !provider.Retryable(provider.Classify(lastErr).Class) {
		f := describeUpstreamFailure(lastErr)
		status, code, msg, retryAfter = f.Status, f.Code, f.Message, f.RetryAfter
	}
	h.log().Warn(ctx, "上游调用失败", map[string]any{
		"request_id":  norm.RequestID,
		"provider_id": lastProvider,
		"class":       provider.Classify(lastErr).Class.String(),
		"outcome":     outcome,
	})
	if retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.FormatInt(clampRetryAfter(retryAfter), 10))
	}
	httpx.WriteError(w, r, status, code, msg)
}

// writeCompletion 输出非流式响应体（OpenAI chat.completion 形状）。
func (h *handler) writeCompletion(w http.ResponseWriter, r *http.Request, norm *normalizedRequest,
	resp provider.CompletionResponse, start time.Time) {
	body := completionResponseBody{
		ID:      completionID(norm.RequestID),
		Object:  "chat.completion",
		Created: start.Unix(),
		Model:   norm.Model,
		Choices: []completionChoice{{
			Index: 0,
			Message: completionMessage{
				Role:             "assistant",
				Content:          resp.Content,
				ReasoningContent: resp.ReasoningContent,
				ToolCalls:        toolCallsFrom(resp.ToolCalls),
			},
			FinishReason: wireFinishReason(resp.FinishReason),
		}},
		Usage: usageFrom(resp.Usage),
	}
	h.log().Info(r.Context(), "chat.completions 完成", map[string]any{
		"request_id":    norm.RequestID,
		"model":         norm.Model,
		"stream":        false,
		"input_tokens":  body.Usage.PromptTokens,
		"output_tokens": body.Usage.CompletionTokens,
		"latency_ms":    time.Since(start).Milliseconds(),
		"outcome":       OutcomeSettled,
	})
	httpx.WriteJSON(w, http.StatusOK, body)
}

// ready 校验必需的装配依赖：缺依赖时回 500，而不是 panic 或静默放行。
func (h *handler) ready(w http.ResponseWriter, r *http.Request) bool {
	var missing []string
	if h.d.Auth == nil {
		missing = append(missing, "Auth")
	}
	if h.d.Quota == nil {
		missing = append(missing, "Quota")
	}
	if h.d.Models == nil {
		missing = append(missing, "Models")
	}
	if h.d.Catalog == nil {
		missing = append(missing, "Catalog")
	}
	if h.d.Upstream == nil {
		missing = append(missing, "Upstream")
	}
	if h.d.Usage == nil {
		missing = append(missing, "Usage")
	}
	if len(missing) == 0 {
		return true
	}
	h.log().Error(r.Context(), "网关装配不完整", map[string]any{"missing": strings.Join(missing, ",")})
	httpx.WriteError(w, r, http.StatusInternalServerError, "server_error", "网关未正确装配")
	return false
}

// authenticate 取中间件注入的已认证用户（本包不解析 Authorization 头）。
func (h *handler) authenticate(w http.ResponseWriter, r *http.Request) (model.User, bool) {
	user, err := h.d.Auth(r)
	if err != nil {
		httpx.WriteMappedError(w, r, err, "认证失败")
		return model.User{}, false
	}
	if user.ID == "" {
		httpx.WriteError(w, r, http.StatusUnauthorized, "invalid_api_key", "认证信息不完整")
		return model.User{}, false
	}
	// 中间件应已挡住停用账号；这里再挡一次（只依赖一处的防护不算防护）。
	if user.Status == model.UserStatusDisabled {
		httpx.WriteError(w, r, http.StatusForbidden, "account_disabled", "账号已停用")
		return model.User{}, false
	}
	return user, true
}

// modelSpec 读取并校验模型目录条目。模型不存在或未启用 → 404 model_not_found，
// 且**不进上游、不预占额度**（§11.4、文档 §17）。
func (h *handler) modelSpec(w http.ResponseWriter, r *http.Request, modelID string) (model.ModelSpec, bool) {
	spec, err := h.d.Models.GetModel(r.Context(), modelID)
	if err != nil {
		if errors.Is(err, model.ErrNotFound) {
			httpx.WriteError(w, r, http.StatusNotFound, "model_not_found", "模型不存在或未启用")
			return model.ModelSpec{}, false
		}
		h.log().Error(r.Context(), "读取模型目录失败", map[string]any{"model": modelID, "error": err.Error()})
		httpx.WriteError(w, r, http.StatusInternalServerError, "server_error", "网关内部错误")
		return model.ModelSpec{}, false
	}
	if !spec.Enabled {
		httpx.WriteError(w, r, http.StatusNotFound, "model_not_found", "模型不存在或未启用")
		return model.ModelSpec{}, false
	}
	return spec, true
}

// checkStreamSupported 按模型目录声明的能力挡住流式请求。
//
// 只有 CapabilitiesJSON 显式声明 stream=false 才拒绝：字段缺失、JSON 非法、空串
// 都按「未声明」放行 —— 目录没填不该让请求不可用。
func checkStreamSupported(spec model.ModelSpec, stream bool) error {
	if !stream {
		return nil
	}
	raw := strings.TrimSpace(spec.CapabilitiesJSON)
	if raw == "" {
		return nil
	}
	var caps struct {
		Stream *bool `json:"stream"`
	}
	if err := json.Unmarshal([]byte(raw), &caps); err != nil || caps.Stream == nil || *caps.Stream {
		return nil
	}
	return unsupportedParam("模型 %s 不支持流式输出（stream=true）", spec.ModelID)
}

// writeRequestError 把入参校验错误写成 400。
func writeRequestError(w http.ResponseWriter, r *http.Request, err error) {
	var re *requestError
	if errors.As(err, &re) {
		httpx.WriteError(w, r, http.StatusBadRequest, re.Code, re.Message)
		return
	}
	httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", err.Error())
}

// filterOpenAIChat 只保留 OpenAI 兼容协议的候选（§11.1.4：协议过滤在调用方做）。
// Protocol 为空按 openai-chat 处理，与 upstream.spec 的容错保持一致。
func filterOpenAIChat(cands []catalog.Candidate) []catalog.Candidate {
	out := make([]catalog.Candidate, 0, len(cands))
	for _, c := range cands {
		if p := c.Provider.Protocol; p != "" && p != model.ProtocolOpenAIChat {
			continue
		}
		out = append(out, c)
	}
	return out
}

// newRequestID 生成兜底请求 ID（中间件未注入时使用），形如 req_<32hex>。
func newRequestID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand 失败极为罕见；退化为时间戳 + 序号，仍保证同一进程内不重复。
		return fmt.Sprintf("req_%d_%d", time.Now().UnixNano(), requestSeq.Add(1))
	}
	return "req_" + hex.EncodeToString(buf[:])
}

// clampRetryAfter 把 Retry-After 秒数收进 [1, maxRetryAfterSeconds]。
func clampRetryAfter(secs int64) int64 {
	if secs <= 0 {
		return 0
	}
	if secs > maxRetryAfterSeconds {
		return maxRetryAfterSeconds
	}
	return secs
}
