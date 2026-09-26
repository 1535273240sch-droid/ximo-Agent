package meta

import (
	"net/http"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/account"
	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/httpx"
)

// ProtocolVersion 是中转站与插件之间的对外协议版本（契约 §11.3）。
const ProtocolVersion = 1

const (
	// maxRequestBytes 与 httpx.DecodeJSON 的 MaxBytesReader 上限一致
	// （httpx 未导出该常量，此处按能力声明的口径复述；超限请求由 httpx 拒绝）。
	maxRequestBytes = 32 << 20
	// defaultPageSize 与 store 的分页默认值一致（limit<=0 时生效）。
	defaultPageSize = 100
	// maxPageSize 是本包对 /v1/usage 的 limit 上限，与 capabilities 声明一致。
	maxPageSize = 200
)

// healthResponse 是 GET /v1/health 的响应体。
type healthResponse struct {
	Status  string `json:"status"`
	Version string `json:"version"`
	TimeMS  int64  `json:"time_ms"`
}

// featuresResponse / limitsResponse 用结构体而非 map，保证 JSON 字段顺序稳定
// （便于客户端缓存与测试断言）。
type featuresResponse struct {
	DeviceLogin       bool `json:"device_login"`
	TokenRefresh      bool `json:"token_refresh"`
	APIKeys           bool `json:"api_keys"`
	OpenAIChat        bool `json:"openai_chat"`
	AnthropicMessages bool `json:"anthropic_messages"`
	Streaming         bool `json:"streaming"`
}

type limitsResponse struct {
	MaxRequestBytes           int64 `json:"max_request_bytes"`
	DefaultPageSize           int64 `json:"default_page_size"`
	MaxPageSize               int64 `json:"max_page_size"`
	AccessTokenTTLSeconds     int64 `json:"access_token_ttl_seconds"`
	RefreshTokenTTLSeconds    int64 `json:"refresh_token_ttl_seconds"`
	DeviceCodeTTLSeconds      int64 `json:"device_code_ttl_seconds"`
	DevicePollIntervalSeconds int64 `json:"device_poll_interval_seconds"`
}

type capabilitiesResponse struct {
	ProtocolVersion int              `json:"protocol_version"`
	ServerVersion   string           `json:"server_version"`
	Features        featuresResponse `json:"features"`
	Limits          limitsResponse   `json:"limits"`
}

// health 是存活探测：只报告进程存活与服务器时间。
//
// 刻意不做「依赖深度体检」：store/account 的瞬时故障会让健康检查抖动，
// 进而被编排系统放大成重启。依赖是否可用由各路由自身回答（未装配 → 503，
// 查询失败 → 502/500），这样故障信息落在真正受影响的请求上。
func (h *handlers) health(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, healthResponse{
		Status:  "ok",
		Version: h.version(),
		TimeMS:  time.Now().UnixMilli(),
	})
}

// capabilities 声明协议版本、能力与限额，供插件在配置前自查。
//
// features 的取值来自契约 §11.3 冻结的路由表：device_login / token_refresh /
// api_keys 由本包与 W-Server 提供，openai_chat / anthropic_messages / streaming
// 由 W-OpenAI、W-Anthropic 提供（同一版 V1 契约的一部分）。这些是「协议承诺」
// 而非「运行时探测结果」，装配方若要按实际注册情况调整，需要新增 Deps 字段。
//
// limits 里的令牌寿命直接引用 internal/account 的常量，不做副本，避免声明与
// 实际签发行为漂移。
func (h *handlers) capabilities(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, capabilitiesResponse{
		ProtocolVersion: ProtocolVersion,
		ServerVersion:   h.version(),
		Features: featuresResponse{
			DeviceLogin:       true,
			TokenRefresh:      true,
			APIKeys:           true,
			OpenAIChat:        true,
			AnthropicMessages: true,
			Streaming:         true,
		},
		Limits: limitsResponse{
			MaxRequestBytes:           maxRequestBytes,
			DefaultPageSize:           defaultPageSize,
			MaxPageSize:               maxPageSize,
			AccessTokenTTLSeconds:     int64(account.DefaultAccessTTL / time.Second),
			RefreshTokenTTLSeconds:    int64(account.DefaultRefreshTTL / time.Second),
			DeviceCodeTTLSeconds:      int64(account.DefaultDeviceCodeTTL / time.Second),
			DevicePollIntervalSeconds: int64(account.DefaultPollIntervalMS / 1000),
		},
	})
}
