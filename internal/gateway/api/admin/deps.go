// Package admin 实现 XIMO 中转站的管理 API（契约 §11.3 的全部 /admin/* 路由）。
//
// 边界与约定：
//   - 鉴权用 X-Admin-Token（与用户态 Bearer 凭据完全独立），恒定时间比较，缺失/不符 401；
//   - 所有写操作在同一请求内落一条 gw_audit（actor=admin、action、target、result、ip、detail）；
//   - 依赖用消费方窄接口声明（契约 §10.2）：*gateway/store.Store、*account.Service、
//     *quota.Service、*secrets.Manager 逐字满足下面的接口，可直接传入，不必写适配器；
//   - 本包不直连数据库，读写一律经 Store，写事务由存储层（sqlite.DB.WithTx）负责。
package admin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/account"
	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/httpx"
	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/model"
	"github.com/ximo888ok-netizen/ximo-agent/internal/quota"
)

// Deps 是管理 API 的装配依赖。零值不可用（所有路由会返回 500，而不是 nil 解引用崩进程）。
type Deps struct {
	// Store 是管理面需要的数据访问能力。
	Store Store
	// Accounts 提供用户与凭据的创建（口令哈希只在本包之外发生）。
	Accounts Accounts
	// Quota 提供额度调整与账户快照。
	Quota Quota
	// Secrets 把上游明文密钥存进平台安全存储并返回 secretref；未装配时
	// 「带明文密钥的 provider 写入」直接失败，绝不降级为明文落库。
	Secrets Secrets
	// AdminToken 是 X-Admin-Token 的期望值。为空时管理面整体关闭（一律 401），
	// 避免「忘了配置」被解释为「无需令牌即可管理」。
	AdminToken string
	// Now 注入时钟（unix 毫秒），nil 时用 time.Now。测试与审计时间戳用。
	Now func() int64
}

// Store 是管理 API 需要的最小数据访问能力。*gateway/store.Store 的方法签名与此逐字一致。
type Store interface {
	CreateUser(ctx context.Context, u model.User) error
	GetUser(ctx context.Context, id string) (model.User, error)
	ListUsers(ctx context.Context, limit, offset int) ([]model.User, error)
	UpdateUserStatus(ctx context.Context, id, status string) error

	CreateAPIKey(ctx context.Context, k model.APIKey) error
	ListAPIKeysByUser(ctx context.Context, userID string) ([]model.APIKey, error)
	RevokeAPIKey(ctx context.Context, id string) error

	UpsertModel(ctx context.Context, m model.ModelSpec) error
	GetModel(ctx context.Context, modelID string) (model.ModelSpec, error)
	ListModels(ctx context.Context, onlyEnabled bool) ([]model.ModelSpec, error)

	UpsertProvider(ctx context.Context, p model.ProviderSpec) error
	GetProvider(ctx context.Context, id string) (model.ProviderSpec, error)
	ListProviders(ctx context.Context, onlyEnabled bool) ([]model.ProviderSpec, error)
	UpsertProviderModel(ctx context.Context, pm model.ProviderModel) error

	ListUsage(ctx context.Context, userID string, limit, offset int) ([]model.UsageRecord, error)
	ListLedger(ctx context.Context, userID string, limit, offset int) ([]model.LedgerEntry, error)

	InsertAudit(ctx context.Context, a model.AuditLog) error
	ListAudit(ctx context.Context, limit, offset int) ([]model.AuditLog, error)
}

// Accounts 是账号服务的最小面（*account.Service 满足）。
type Accounts interface {
	CreateUser(ctx context.Context, username, password, groupID string) (model.User, error)
	CreateAPIKey(ctx context.Context, userID string, ttl time.Duration) (string, model.APIKey, error)
	ApproveDeviceLogin(ctx context.Context, userCode, userID string) error
}

// Quota 是额度服务的最小面（*quota.Service 满足）。
type Quota interface {
	Adjust(ctx context.Context, userID, kind string, amount int64, reason, operatorID, idempotencyKey string) (model.LedgerEntry, error)
	Account(ctx context.Context, userID string) (model.QuotaAccount, error)
}

// Secrets 是秘密写入面（*secrets.Manager 满足）。只暴露 Put：管理面不需要读回明文。
type Secrets interface {
	Put(value string) (string, error)
}

// clientError 是 admin 层的入参校验失败：映射 400，文案可原样回给调用方。
// 文案一律由本包构造，不含请求里的自由文本（避免把误填的密钥回显到响应或审计里）。
type clientError struct{ msg string }

func (e clientError) Error() string { return e.msg }

func badRequest(msg string) error { return clientError{msg: msg} }

// classify 把错误映射为 HTTP 状态码与错误码：
//   - admin 入参校验（clientError）与服务层的参数错误 → 400；
//   - model.* 哨兵 → 交给 httpx.StatusFor（404/409/401/402/403）；
//   - 其余（含未装配、DB 故障）→ 500，绝不当业务错误静默降级。
func classify(err error) (int, string) {
	if err == nil {
		return http.StatusOK, ""
	}
	var ce clientError
	if errors.As(err, &ce) {
		return http.StatusBadRequest, "invalid_request_error"
	}
	if isArgumentError(err) {
		return http.StatusBadRequest, "invalid_request_error"
	}
	status, code := httpx.StatusFor(err)
	if status == http.StatusOK {
		return http.StatusInternalServerError, "server_error"
	}
	return status, code
}

// messageFor 决定回给调用方的文案：400 是调用方可修的入参问题，回显具体原因；
// 其它状态回通用文案，绝不把内部错误文本（可能含库结构、路径）当响应体传出去。
func messageFor(err error, status int, code string) string {
	if status == http.StatusBadRequest {
		return err.Error()
	}
	switch status {
	case http.StatusNotFound:
		return "目标不存在"
	case http.StatusConflict:
		return "状态冲突（重复提交或状态已变更）"
	case http.StatusUnauthorized:
		return "凭据无效"
	case http.StatusForbidden:
		return "账号已禁用"
	case http.StatusPaymentRequired:
		return "额度不足"
	}
	return "内部错误（详见服务端日志）"
}

// isArgumentError 判定服务层的「入参不合法」错误：重试无用，HTTP 层回 400。
func isArgumentError(err error) bool {
	return errors.Is(err, account.ErrInvalidUsername) ||
		errors.Is(err, account.ErrWeakPassword) ||
		errors.Is(err, account.ErrInvalidArgument) ||
		errors.Is(err, quota.ErrInvalidArgument)
}

// ID 前缀（沿用仓库「前缀_hex」风格，日志与审计里可读）。
const (
	prefixAudit    = "aud"
	prefixProvider = "prv"
)

// 审计常量（契约 §11.3：actor=admin、action、target、result、ip、detail）。
const (
	actorAdmin    = "admin"
	resultOK      = "ok"
	resultError   = "error"
	operatorAdmin = actorAdmin
)

// 分页边界（契约 §11.3：默认 100、上限 500）。
const (
	defaultPageLimit = 100
	maxPageLimit     = 500
)

// defaultAPIKeyTTL 是 POST /admin/keys 未指定 ttl_seconds 时的有效期（0 = 不过期）。
// 插件侧需要长期凭据，默认不设过期，撤销是主要失效手段；需要限时的调用方显式传 ttl_seconds。
const defaultAPIKeyTTL time.Duration = 0

var fallbackIDCounter atomic.Uint64

// newID 生成「前缀_16位hex」ID。与 quota 侧的实现同构：网关不依赖 Agent 运行时的
// internal/types（§0.5 网关与运行时解耦），因此本地保留一份。
func newID(prefix string) string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		n := fallbackIDCounter.Add(1)
		for i := range buf {
			buf[i] = byte(n >> (8 * (len(buf) - 1 - i)))
		}
	}
	return prefix + "_" + hex.EncodeToString(buf[:])
}

// parsePage 解析 limit/offset（契约 §11.3）：默认 limit=100、上限 500；
// 越界或非整数直接 400（不静默截断，否则调用方无法发现自己的分页参数写错了）。
func parsePage(r *http.Request) (limit, offset int, err error) {
	q := r.URL.Query()
	limit = defaultPageLimit
	if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
		n, convErr := strconv.Atoi(raw)
		if convErr != nil || n < 1 || n > maxPageLimit {
			return 0, 0, badRequest(fmt.Sprintf("limit 必须是 1~%d 之间的整数", maxPageLimit))
		}
		limit = n
	}
	if raw := strings.TrimSpace(q.Get("offset")); raw != "" {
		n, convErr := strconv.Atoi(raw)
		if convErr != nil || n < 0 {
			return 0, 0, badRequest("offset 必须是非负整数")
		}
		offset = n
	}
	return limit, offset, nil
}

// paginate 在内存里分页（store 的 ListModels / ListAPIKeysByUser 不支持 SQL 分页）。
// 返回的切片一定非 nil，保证 JSON 是 [] 而不是 null。
func paginate[T any](items []T, limit, offset int) []T {
	out := make([]T, 0, limit)
	if offset >= len(items) {
		return out
	}
	end := offset + limit
	if end > len(items) {
		end = len(items)
	}
	return append(out, items[offset:end]...)
}

// jsonField 把库中的 JSON 文本转成可序列化的值：空或非法文本回退为 null，
// 否则 json.Marshal 会因 json.RawMessage 非法而让整条响应编码失败。
func jsonField(s string) any {
	s = strings.TrimSpace(s)
	if s == "" || !json.Valid([]byte(s)) {
		return nil
	}
	return json.RawMessage(s)
}

// validateEndpoint 校验上游地址：必须能解析出 scheme+host，且 scheme 为 http/https。
func validateEndpoint(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", badRequest("endpoint 不能为空")
	}
	u, err := url.Parse(raw)
	if err != nil {
		// 不回显原值：endpoint 是自由文本，误填密钥时不能被回显或落审计。
		return "", badRequest("endpoint 无法解析为 URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", badRequest("endpoint 的 scheme 必须是 http 或 https")
	}
	if u.Host == "" {
		return "", badRequest("endpoint 缺少 host")
	}
	return raw, nil
}

// validateProtocol 校验上游协议。V1 上游只支持 openai-chat（契约 §11.1.4）：
// 其它值必须明确报错，不能静默接受——静默接受会写进目录，再被调用方按协议过滤掉，
// 表现为「配置成功但请求永远不走这条上游」，很难排查。
func validateProtocol(raw string) (string, error) {
	p := strings.TrimSpace(raw)
	if p == "" {
		return model.ProtocolOpenAIChat, nil
	}
	if p != model.ProtocolOpenAIChat {
		return "", badRequest("protocol 不支持：V1 上游只支持 " + model.ProtocolOpenAIChat)
	}
	return p, nil
}

// validateProviderStatus 校验服务商状态取值（缺省 enabled）。
func validateProviderStatus(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return model.ProviderStatusEnabled, nil
	}
	if s != model.ProviderStatusEnabled && s != model.ProviderStatusDisabled {
		return "", badRequest("status 只能是 enabled 或 disabled")
	}
	return s, nil
}

// validateUserStatus 校验用户状态取值（store 不校验，管理面必须挡住非法值）。
func validateUserStatus(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s != model.UserStatusActive && s != model.UserStatusDisabled {
		return "", badRequest("status 只能是 active 或 disabled")
	}
	return s, nil
}

// validateJSONObject 把请求里的 capabilities 归一为合法 JSON 对象文本：
// 空值 → "{}"；数组/标量/语法错误 → 400（契约 §5.3 的形状要求）。
func validateJSONObject(raw, field string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" || s == "null" {
		return "{}", nil
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(s), &obj); err != nil {
		return "", badRequest(field + " 必须是合法 JSON 对象")
	}
	if obj == nil {
		return "{}", nil
	}
	buf, err := json.Marshal(obj)
	if err != nil {
		return "", badRequest(field + " 无法归一化")
	}
	return string(buf), nil
}
