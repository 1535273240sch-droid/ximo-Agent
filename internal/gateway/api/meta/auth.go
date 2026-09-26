package meta

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/account"
	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/httpx"
	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/model"
)

// tokenTypeBearer 是响应里的 token_type，固定为 Bearer（契约 §11.3）。
const tokenTypeBearer = "Bearer"

// deviceResponse 是 POST /v1/auth/device 的响应体（文档 §5.2）。
type deviceResponse struct {
	DeviceCode string `json:"device_code"`
	UserCode   string `json:"user_code"`
	ExpiresIn  int64  `json:"expires_in"`
	Interval   int64  `json:"interval"`
}

// tokenResponse 是登录链三个换发令牌端点的统一响应体。
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	TokenType    string `json:"token_type"`
}

type deviceTokenRequest struct {
	DeviceCode string `json:"device_code"`
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// authDevice 申请设备码。无鉴权（用户还没登录），也不需要请求体。
//
// 返回的 expires_in 用 account.DefaultDeviceCodeTTL：设备码寿命由账号服务决定，
// StartDeviceLogin 只回轮询间隔，故这里引用同一常量而非自造数值。
//
// 防爆破（登录链三个未鉴权端点都在内）由 W-Server 的 ratelimit.go 负责（§11.4 的
// 请求生命周期里限流排在认证之前），本包不重复实现。
func (h *handlers) authDevice(w http.ResponseWriter, r *http.Request) {
	if h.d.Accounts == nil {
		unavailable(w, r, "账号服务")
		return
	}
	deviceCode, userCode, intervalMS, err := h.d.Accounts.StartDeviceLogin(r.Context())
	if err != nil {
		h.log.Error(r.Context(), "meta: 申请设备码失败", logFields(r, map[string]any{"error": err.Error()}))
		httpx.WriteMappedError(w, r, err, "申请设备码失败")
		return
	}
	if intervalMS <= 0 {
		intervalMS = account.DefaultPollIntervalMS
	}
	httpx.WriteJSON(w, http.StatusOK, deviceResponse{
		DeviceCode: deviceCode,
		UserCode:   userCode,
		ExpiresIn:  int64(account.DefaultDeviceCodeTTL / time.Second),
		Interval:   ceilSeconds(intervalMS),
	})
}

// authToken 轮询设备码兑换令牌对。
//
// 待授权返回 400 authorization_pending（httpx.StatusFor 已把
// model.ErrAuthorizationPending 映射成该状态码/错误码），客户端按 interval 继续轮询。
//
// 设备码的一次性消费由 account.PollDeviceLogin 在内部完成：store 已实现
// DeviceCodeConsumer（契约 §11.1.3 的 ConsumeDeviceCode）时以库中状态为准
// （跨进程/跨重启有效）；store 未实现时 account 退化为进程内一次性保护。
// 这里不重复实现消费逻辑，以免出现两套「谁算消费成功」的判定。
func (h *handlers) authToken(w http.ResponseWriter, r *http.Request) {
	if h.d.Accounts == nil {
		unavailable(w, r, "账号服务")
		return
	}
	var req deviceTokenRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	deviceCode := strings.TrimSpace(req.DeviceCode)
	if deviceCode == "" {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "device_code 不能为空")
		return
	}

	access, refresh, err := h.d.Accounts.PollDeviceLogin(r.Context(), deviceCode)
	if err != nil {
		msg := "设备码兑换失败"
		if errors.Is(err, model.ErrAuthorizationPending) {
			msg = "授权尚未完成，请稍后按 interval 继续轮询"
		}
		// 不在日志里记 device_code（等价于凭据），也不回显底层错误文本
		// （错误链可能带上凭据摘要）；只记映射后的状态码便于观测轮询失败率。
		status, _ := httpx.StatusFor(err)
		h.log.Warn(r.Context(), "meta: 设备码兑换未成功",
			logFields(r, map[string]any{"status": status}))
		httpx.WriteMappedError(w, r, err, msg)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, newTokenResponse(access, refresh))
}

// authRefresh 用 refresh token 换一对新令牌（轮换，旧 refresh 立即失效）。
func (h *handlers) authRefresh(w http.ResponseWriter, r *http.Request) {
	if h.d.Accounts == nil {
		unavailable(w, r, "账号服务")
		return
	}
	var req refreshRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	refreshToken := strings.TrimSpace(req.RefreshToken)
	if refreshToken == "" {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "refresh_token 不能为空")
		return
	}

	access, refresh, err := h.d.Accounts.Refresh(r.Context(), refreshToken)
	if err != nil {
		// 只记判定结果，不记 refresh_token 与其摘要（凭据不得进日志）。
		status, _ := httpx.StatusFor(err)
		h.log.Warn(r.Context(), "meta: 刷新令牌失败", logFields(r, map[string]any{"status": status}))
		httpx.WriteMappedError(w, r, err, "刷新令牌失败")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, newTokenResponse(access, refresh))
}

// authLogin 用户名口令登录。口令校验与令牌签发都在 internal/account，
// 本包只把结果映射成 HTTP（用户名/口令错误 → 401，账号禁用 → 403）。
func (h *handlers) authLogin(w http.ResponseWriter, r *http.Request) {
	if h.d.Accounts == nil {
		unavailable(w, r, "账号服务")
		return
	}
	var req loginRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	username := strings.TrimSpace(req.Username)
	if username == "" || req.Password == "" {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "username 与 password 不能为空")
		return
	}

	u, err := h.d.Accounts.Authenticate(r.Context(), username, req.Password)
	if err != nil {
		// 审计口径：只记用户名（可能是攻击者输入的任意串）与映射后的状态码，
		// 不记口令，也不区分「用户不存在」与「口令错误」（account 已统一为
		// ErrBadCredentials，此处不自作聪明地补差异）。
		status, _ := httpx.StatusFor(err)
		h.log.Warn(r.Context(), "meta: 口令登录失败",
			logFields(r, map[string]any{"username": username, "status": status}))
		httpx.WriteMappedError(w, r, err, "用户名或口令不正确")
		return
	}

	access, refresh, err := h.d.Accounts.IssueSession(r.Context(), u.ID)
	if err != nil {
		h.log.Error(r.Context(), "meta: 签发会话失败", logFields(r, map[string]any{"user_id": u.ID, "error": err.Error()}))
		httpx.WriteMappedError(w, r, err, "签发会话失败")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, newTokenResponse(access, refresh))
}

// newTokenResponse 组装令牌对响应。expires_in 取 account 的 access 寿命，
// 与该服务实际签发行为同源（account 无运行时改 TTL 的入口）。
func newTokenResponse(access, refresh string) tokenResponse {
	return tokenResponse{
		AccessToken:  access,
		RefreshToken: refresh,
		ExpiresIn:    int64(account.DefaultAccessTTL / time.Second),
		TokenType:    tokenTypeBearer,
	}
}

// ceilSeconds 把毫秒向上取整成秒（interval 声明的是「不早于」的轮询间隔，
// 向下取整会让客户端比服务端期望的更频繁地轮询）。
func ceilSeconds(ms int64) int64 {
	if ms <= 0 {
		return 0
	}
	return (ms + 999) / 1000
}
