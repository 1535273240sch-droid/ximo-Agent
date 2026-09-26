// Package model 定义 XIMO 中转站（gateway）的跨包共享类型与哨兵错误。
//
// 本包由主代理冻结：只放类型与常量，不放逻辑。任何子代理不得修改本包，
// 需要新字段请报告，由主代理统一变更。
package model

import "errors"

// 哨兵错误。跨包判定一律用 errors.Is，不要比较错误字符串。
var (
	ErrNotFound             = errors.New("gwmodel: not found")
	ErrInsufficientQuota    = errors.New("gwmodel: insufficient quota")
	ErrAuthorizationPending = errors.New("gwmodel: authorization pending")
	ErrExpired              = errors.New("gwmodel: expired")
	ErrRevoked              = errors.New("gwmodel: revoked")
	ErrDisabled             = errors.New("gwmodel: disabled")
	ErrBadCredentials       = errors.New("gwmodel: bad credentials")
	ErrConflict             = errors.New("gwmodel: conflict")
)

// 账户状态。
const (
	UserStatusActive   = "active"
	UserStatusDisabled = "disabled"

	KeyStatusActive  = "active"
	KeyStatusRevoked = "revoked"

	QuotaStatusActive = "active"
	QuotaStatusFrozen = "frozen"

	ReservationHeld     = "held"
	ReservationSettled  = "settled"
	ReservationReleased = "released"
	ReservationExpired  = "expired"

	DeviceStatusPending  = "pending"
	DeviceStatusApproved = "approved"
	DeviceStatusConsumed = "consumed"
	DeviceStatusExpired  = "expired"

	ProviderStatusEnabled  = "enabled"
	ProviderStatusDisabled = "disabled"

	ProtocolOpenAIChat = "openai-chat"
	ProtocolAnthropic  = "anthropic-messages"
)

// 账本类型白名单（文档 §7.2：每次增加/减少额度都要有流水）。
const (
	LedgerReserve  = "reserve"
	LedgerRelease  = "release"
	LedgerSettle   = "settle"
	LedgerTopup    = "topup"
	LedgerDebit    = "debit"
	LedgerFreeze   = "freeze"
	LedgerUnfreeze = "unfreeze"
	LedgerExpire   = "expire"
)

// User 是登录主体。PasswordHash 形如 "pbkdf2-sha256$<iter>$<saltB64>$<hashB64>"。
type User struct {
	ID           string
	Username     string
	PasswordHash string
	Status       string
	GroupID      string
	CreatedAt    int64
	UpdatedAt    int64
}

// APIKey 只保存哈希；KeyPrefix 仅用于展示与识别（例如 "ximo_sk_ab12"）。
type APIKey struct {
	ID         string
	UserID     string
	KeyPrefix  string
	KeyHash    string
	Status     string
	ExpiresAt  int64
	CreatedAt  int64
	LastUsedAt int64
}

// AuthSession 是不透明令牌会话；AccessHash/RefreshHash 均为 sha256 十六进制。
type AuthSession struct {
	ID               string
	UserID           string
	AccessHash       string
	RefreshHash      string
	RotatedFrom      string
	AccessExpiresAt  int64
	RefreshExpiresAt int64
	RevokedAt        int64
	CreatedAt        int64
}

// DeviceCode 对应文档 §5.2 的设备授权登录流程。
type DeviceCode struct {
	DeviceCodeHash string
	UserCode       string
	UserID         string
	Status         string
	ExpiresAt      int64
	CreatedAt      int64
	LastPolledAt   int64
	PollIntervalMS int64
}

// QuotaAccount 是额度账户快照。金额单位一律为微单位 int64（1e-6 credit），禁止 float64。
type QuotaAccount struct {
	UserID         string
	TotalAmount    int64
	UsedAmount     int64
	ReservedAmount int64
	Version        int64
	Status         string
	UpdatedAt      int64
}

// Available 是当前可继续预占的额度，任何时刻不得为负（文档 §22 额度红线）。
func (a QuotaAccount) Available() int64 {
	return a.TotalAmount - a.UsedAmount - a.ReservedAmount
}

// Reservation 是一次请求的额度预占。同一 (UserID, RequestID) 只应存在一条 held 记录。
type Reservation struct {
	ID            string
	UserID        string
	RequestID     string
	Status        string
	Amount        int64
	SettledAmount int64
	ExpiresAt     int64
	CreatedAt     int64
	UpdatedAt     int64
}

// LedgerEntry 是 append-only 账本行，必须带 BalanceAfter 与 ReservedAfter 以便对账。
type LedgerEntry struct {
	ID             string
	UserID         string
	Type           string
	RequestID      string
	IdempotencyKey string
	OperatorID     string
	Reason         string
	Amount         int64
	BalanceAfter   int64
	ReservedAfter  int64
	CreatedAt      int64
}

// ModelSpec 是模型目录条目；CapabilitiesJSON 形如 {"stream":true,"vision":true,"tools":true,"reasoning":true}。
type ModelSpec struct {
	ModelID          string
	DisplayName      string
	CapabilitiesJSON string
	Enabled          bool
	CreatedAt        int64
	UpdatedAt        int64
}

// ProviderSpec 是上游服务商条目。APIKeyRef 为 internal/secrets 的引用（secretref:v1:<hex>），不得存明文。
type ProviderSpec struct {
	ID         string
	Name       string
	Endpoint   string
	Protocol   string
	Status     string
	APIKeyRef  string
	ConfigJSON string
	TimeoutMS  int64
	Weight     int64
	CreatedAt  int64
	UpdatedAt  int64
}

// ProviderModel 把模型目录条目映射到某 provider 的上游模型名与优先级。
type ProviderModel struct {
	ProviderID      string
	ModelID         string
	UpstreamModelID string
	Enabled         bool
	Priority        int64
}

// UsageRecord 是一次上游调用的用量与成本记录。
type UsageRecord struct {
	RequestID    string
	UserID       string
	ModelID      string
	ProviderID   string
	Status       string
	InputTokens  int64
	OutputTokens int64
	LatencyMS    int64
	CostMicro    int64
	CreatedAt    int64
}

// AuditLog 记录管理侧写操作，必须能追溯到操作者（文档 §22 后台验收）。
type AuditLog struct {
	ID         string
	Actor      string
	Action     string
	Target     string
	Result     string
	IP         string
	DetailJSON string
	CreatedAt  int64
}
