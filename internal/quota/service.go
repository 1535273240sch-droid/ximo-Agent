package quota

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/model"
)

// 本包生成的 ID 前缀（沿用仓库「前缀_hex」风格，日志与 crash dump 里可读）。
const (
	prefixReservation = "rsv"
	prefixLedger      = "led"
)

const (
	// DefaultReservationTTL 预占默认存活时长。上游挂住（既不返回也不断开）时，
	// 预占不会把额度永久占死，由 ReapExpired 回收。
	DefaultReservationTTL = 10 * time.Minute

	// DefaultReapLimit ReapExpired 的 limit <= 0 时单次回收条数上限，
	// 避免一次扫描把所有 held 行都锁在同一个写事务里。
	DefaultReapLimit = 500

	// UsageStatusSettled UsageRecord.Status 的取值：该请求已按上游真实 usage 结算。
	// model 包未定义用量状态常量，这里补一个，免得调用方到处写字符串字面量。
	UsageStatusSettled = "settled"
)

// accountStore 是额度服务需要的最小存储能力（窄接口由消费方定义，实现方无需依赖本包）。
// *gateway/store.Store 的方法签名与下面逐字一致，可直接传入 New。
//
// 约定：所有方法内部必须使用存储层的单写队列事务（WithTx + _txlock=immediate），
// 原子性（读账户→校验→改预留→插预占→插账本）由存储层负责，本包不做补偿。
type accountStore interface {
	ReserveTx(ctx context.Context, r model.Reservation, ledgerID string, now int64) (model.Reservation, error)
	SettleTx(ctx context.Context, reservationID string, actual int64, ledgerID, requestID string, now int64) error
	ReleaseTx(ctx context.Context, reservationID string, ledgerID string, now int64) error
	AdjustTx(ctx context.Context, userID string, kind string, amount int64, reason, operatorID, idempotencyKey, ledgerID string, now int64) (model.LedgerEntry, error)
	GetQuotaAccount(ctx context.Context, userID string) (model.QuotaAccount, error)
	ExpireReservations(ctx context.Context, now int64, limit int) (int, error)
	InsertUsage(ctx context.Context, u model.UsageRecord) error
}

// Service 是额度服务。零值不可用，请用 New 构造。
type Service struct {
	st accountStore
	// now 注入时钟（unix 毫秒），默认 time.Now().UnixMilli，测试可替换。
	now func() int64
	// reservationTTL 预占存活时长，默认 DefaultReservationTTL。
	reservationTTL time.Duration
}

// New 构造额度服务。st 传 nil 不 panic，但所有方法会返回 ErrUnavailable
// （装配错误要显式报出来，而不是到第一次请求时 nil 解引用崩进程）。
func New(st accountStore) *Service {
	return &Service{
		st:             st,
		now:            func() int64 { return time.Now().UnixMilli() },
		reservationTTL: DefaultReservationTTL,
	}
}

// SetReservationTTL 调整预占存活时长；d <= 0 时恢复 DefaultReservationTTL。
// TTL 决定 ReapExpired 多久之后能回收上游挂住的请求，是运维可调项。
func (s *Service) SetReservationTTL(d time.Duration) {
	if s == nil {
		return
	}
	if d <= 0 {
		d = DefaultReservationTTL
	}
	s.reservationTTL = d
}

// mustStore 返回已装配的存储；未装配时返回 ErrUnavailable。
func (s *Service) mustStore() (accountStore, error) {
	if s == nil || s.st == nil {
		return nil, ErrUnavailable
	}
	return s.st, nil
}

func (s *Service) nowMS() int64 {
	if s == nil || s.now == nil {
		return time.Now().UnixMilli()
	}
	return s.now()
}

func (s *Service) ttlMS() int64 {
	ttl := DefaultReservationTTL
	if s != nil && s.reservationTTL > 0 {
		ttl = s.reservationTTL
	}
	return int64(ttl / time.Millisecond)
}

// Reserve 为一次即将发往上游的请求预占额度。
//
// estimateMicro 由调用方用 EstimateMicro/EstimateInputTokens 算出（发请求前的上界估算）。
//
// 幂等：同一 (userID, requestID) 已存在 held 预占时，存储层返回既有行且不重复扣减；
// 此时返回的 Reservation.ID 是**第一次**那条的 ID，调用方必须用返回值，不要用自己拼的。
//
// 额度不足返回包装了 model.ErrInsufficientQuota 的错误，账户被冻结返回 model.ErrDisabled，
// 用户不存在返回 model.ErrNotFound——都可判定，绝不静默降级放行。
func (s *Service) Reserve(ctx context.Context, userID, requestID string, estimateMicro int64) (model.Reservation, error) {
	st, err := s.mustStore()
	if err != nil {
		return model.Reservation{}, err
	}
	if userID == "" {
		return model.Reservation{}, fmt.Errorf("quota: 预占缺少 userID: %w", ErrInvalidArgument)
	}
	if requestID == "" {
		return model.Reservation{}, fmt.Errorf("quota: 预占缺少 requestID（幂等键的一半，不能为空）: %w", ErrInvalidArgument)
	}
	if estimateMicro < 0 {
		return model.Reservation{}, fmt.Errorf("quota: 预占金额为负 %d: %w", estimateMicro, ErrInvalidArgument)
	}

	now := s.nowMS()
	r := model.Reservation{
		ID:        newID(prefixReservation),
		UserID:    userID,
		RequestID: requestID,
		Status:    model.ReservationHeld,
		Amount:    estimateMicro,
		ExpiresAt: now + s.ttlMS(),
		CreatedAt: now,
		UpdatedAt: now,
	}
	out, err := st.ReserveTx(ctx, r, newID(prefixLedger), now)
	if err != nil {
		return model.Reservation{}, fmt.Errorf("quota: 预占失败 user=%s request=%s amount=%d: %w", userID, requestID, estimateMicro, err)
	}
	return out, nil
}

// Settle 用上游真实 usage 结算一次预占，并落一条用量记录。
//
// actual = CostMicro(inputTok, outputTok, priceMicroPerKTok)：
//   - actual <= 预占额：差额随结算归还可用额度（reserved 减掉 held，used 加 actual）；
//   - actual > 预占额：不允许为了「可用额度不为负」而拒绝结算——上游已经产生了这些
//     消耗，拒绝只会丢账。超出部分记为欠账，账户 used 照实增加（可能把 available 顶到负数，
//     之后由 Adjust/topup 补平）。
//
// 返回的 UsageRecord 已填好 CostMicro 与 Status；ModelID/ProviderID/LatencyMS 默认为空，
// 需要这些字段的调用方用 SettleWithUsage 传入。
//
// 事务边界：先结算账本、再写用量记录，两步不共享事务（契约的存储接口没有合并方法）。
// 因此 InsertUsage 失败时账本已经生效，错误里会写明这一点，返回的 UsageRecord 仍然有效，
// 调用方可以直接重试 Settle 落库：存储层的 SettleTx 对已结算的预占是幂等的（不再动账户），
// 而 InsertUsage 按 request_id 幂等，重试只会补齐那条缺失的用量行，不会重复扣费。
// 注意：只有在调用方手里的 Reservation 仍是 held（没从库里刷成 settled）时才能这样重试，
// 否则本方法的状态校验会先返回 model.ErrConflict。
func (s *Service) Settle(ctx context.Context, r model.Reservation, inputTok, outputTok int64, priceMicroPerKTok int64) (model.UsageRecord, error) {
	return s.settleUsage(ctx, r, model.UsageRecord{
		RequestID:    r.RequestID,
		UserID:       r.UserID,
		InputTokens:  inputTok,
		OutputTokens: outputTok,
	}, priceMicroPerKTok)
}

// SettleWithUsage 与 Settle 相同，但由调用方补齐 UsageRecord 的 ModelID/ProviderID/
// LatencyMS 等字段（契约里的 Settle 签名没有这些参数，只用 Settle 会让 usage 行的
// model/provider 为空，后台无法按模型或上游聚合）。
// u.InputTokens/u.OutputTokens 以这里的为准，CostMicro/Status/CreatedAt 由本方法计算。
func (s *Service) SettleWithUsage(ctx context.Context, r model.Reservation, u model.UsageRecord, priceMicroPerKTok int64) (model.UsageRecord, error) {
	return s.settleUsage(ctx, r, u, priceMicroPerKTok)
}

func (s *Service) settleUsage(ctx context.Context, r model.Reservation, u model.UsageRecord, priceMicroPerKTok int64) (model.UsageRecord, error) {
	st, err := s.mustStore()
	if err != nil {
		return u, err
	}
	if r.ID == "" {
		return u, fmt.Errorf("quota: 结算缺少预占 ID: %w", ErrInvalidArgument)
	}
	if r.UserID == "" {
		return u, fmt.Errorf("quota: 结算缺少 userID: %w", ErrInvalidArgument)
	}
	if u.InputTokens < 0 || u.OutputTokens < 0 {
		return u, fmt.Errorf("quota: 结算 token 为负 in=%d out=%d: %w", u.InputTokens, u.OutputTokens, ErrInvalidArgument)
	}
	if err := checkHeld(r, "结算"); err != nil {
		return u, err
	}

	now := s.nowMS()
	if u.RequestID == "" {
		u.RequestID = r.RequestID
	}
	if u.UserID == "" {
		u.UserID = r.UserID
	}
	u.CostMicro = CostMicro(u.InputTokens, u.OutputTokens, priceMicroPerKTok)
	u.Status = UsageStatusSettled
	u.CreatedAt = now

	if err := st.SettleTx(ctx, r.ID, u.CostMicro, newID(prefixLedger), r.RequestID, now); err != nil {
		return u, fmt.Errorf("quota: 结算失败 reservation=%s actual=%d: %w", r.ID, u.CostMicro, err)
	}
	if err := st.InsertUsage(ctx, u); err != nil {
		return u, fmt.Errorf("quota: 用量记录写入失败（结算已生效，需重试落库）reservation=%s request=%s: %w", r.ID, u.RequestID, err)
	}
	return u, nil
}

// Release 归还一次预占：上游失败、用户取消、请求被拒时调用，让额度立刻可用。
//
// 幂等：状态已是 released 或 expired 时直接返回 nil —— 额度早已归还，重复释放不该报错。
// （上游挂住导致预占被 ReapExpired 回收、随后请求失败再走释放路径，是真实存在的时序。）
// 已 settled 的预占再释放是调用方的逻辑错误，返回 model.ErrConflict。
//
// reason 是短标签（如 "upstream_failed"/"user_cancelled"），只用于错误上下文与排查；
// 契约的 ReleaseTx 没有 reason 参数，因此它不会落库。
func (s *Service) Release(ctx context.Context, r model.Reservation, reason string) error {
	st, err := s.mustStore()
	if err != nil {
		return err
	}
	if r.ID == "" {
		return fmt.Errorf("quota: 释放缺少预占 ID: %w", ErrInvalidArgument)
	}
	switch r.Status {
	case model.ReservationReleased, model.ReservationExpired:
		return nil
	case model.ReservationSettled:
		return fmt.Errorf("quota: 预占状态为 %s 不能释放: %w", r.Status, model.ErrConflict)
	}

	now := s.nowMS()
	if err := st.ReleaseTx(ctx, r.ID, newID(prefixLedger), now); err != nil {
		return fmt.Errorf("quota: 释放失败 reservation=%s reason=%q: %w", r.ID, reason, err)
	}
	return nil
}

// adjustKinds Adjust 允许的账本类型（§3/§6）。reserve/settle/release 只能由额度流程自身
// 产生，后台不得用 Adjust 伪造，否则对账不再可信。
var adjustKinds = []string{
	model.LedgerTopup,
	model.LedgerDebit,
	model.LedgerFreeze,
	model.LedgerUnfreeze,
	model.LedgerExpire,
}

// Adjust 是管理侧的额度增减/冻结入口。
//
// kind 必须在 adjustKinds 白名单内；amount 一律是**非负数量级**，符号由存储层按 kind 落账
// （§6：topup/unfreeze 为正，debit/freeze/expire 为负）——符号只由 kind 决定，sign 不会
// 由两层各翻一次，避免双重取反把扣款做成充值。
//
// idempotencyKey 必填：它是后台防重入的唯一依据（§7.2、§17）。同 key 重复调用不会二次入账，
// 存储层返回既有账本行。operatorID 用于审计追溯，同样要求非空。
func (s *Service) Adjust(ctx context.Context, userID, kind string, amount int64, reason, operatorID, idempotencyKey string) (model.LedgerEntry, error) {
	st, err := s.mustStore()
	if err != nil {
		return model.LedgerEntry{}, err
	}
	if userID == "" {
		return model.LedgerEntry{}, fmt.Errorf("quota: 额度调整缺少 userID: %w", ErrInvalidArgument)
	}
	if !isAdjustKind(kind) {
		return model.LedgerEntry{}, fmt.Errorf("quota: 非法账本类型 %q（仅支持 %v）: %w", kind, adjustKinds, ErrInvalidArgument)
	}
	if amount < 0 {
		return model.LedgerEntry{}, fmt.Errorf("quota: 调整金额为负 %d（请传数量级，符号由 kind=%s 决定）: %w", amount, kind, ErrInvalidArgument)
	}
	if idempotencyKey == "" {
		return model.LedgerEntry{}, fmt.Errorf("quota: 额度调整缺少 idempotencyKey（防重入唯一依据）: %w", ErrInvalidArgument)
	}
	if operatorID == "" {
		return model.LedgerEntry{}, fmt.Errorf("quota: 额度调整缺少 operatorID（审计必须可追溯）: %w", ErrInvalidArgument)
	}

	now := s.nowMS()
	entry, err := st.AdjustTx(ctx, userID, kind, amount, reason, operatorID, idempotencyKey, newID(prefixLedger), now)
	if err != nil {
		return model.LedgerEntry{}, fmt.Errorf("quota: 额度调整失败 user=%s kind=%s amount=%d: %w", userID, kind, amount, err)
	}
	return entry, nil
}

// Account 返回账户快照（只读）。可用额度用 QuotaAccount.Available() 计算，
// 不落库、不缓存，避免冗余字段与 total/used/reserved 漂移。
func (s *Service) Account(ctx context.Context, userID string) (model.QuotaAccount, error) {
	st, err := s.mustStore()
	if err != nil {
		return model.QuotaAccount{}, err
	}
	if userID == "" {
		return model.QuotaAccount{}, fmt.Errorf("quota: 查询账户缺少 userID: %w", ErrInvalidArgument)
	}
	acct, err := st.GetQuotaAccount(ctx, userID)
	if err != nil {
		return model.QuotaAccount{}, fmt.Errorf("quota: 查询账户失败 user=%s: %w", userID, err)
	}
	return acct, nil
}

// ReapExpired 回收超时未结算的 held 预占（上游挂住、进程崩溃后遗留），返回回收条数。
//
// now <= 0 时取当前时钟，limit <= 0 时取 DefaultReapLimit：这样定时任务可以直接
// 用 ReapExpired(ctx, 0, 0) 而不用自己拼参数。
func (s *Service) ReapExpired(ctx context.Context, now int64, limit int) (int, error) {
	st, err := s.mustStore()
	if err != nil {
		return 0, err
	}
	if now <= 0 {
		now = s.nowMS()
	}
	if limit <= 0 {
		limit = DefaultReapLimit
	}
	n, err := st.ExpireReservations(ctx, now, limit)
	if err != nil {
		return 0, fmt.Errorf("quota: 回收超时预占失败 now=%d limit=%d: %w", now, limit, err)
	}
	return n, nil
}

// checkHeld 校验传进来的预占还能被结算/释放。Status 为空视为 held
// （调用方有时只持有 ID，不该因此被拒）。
func checkHeld(r model.Reservation, action string) error {
	if r.Status == "" || r.Status == model.ReservationHeld {
		return nil
	}
	return fmt.Errorf("quota: 预占状态为 %s 不能%s: %w", r.Status, action, model.ErrConflict)
}

func isAdjustKind(kind string) bool {
	for _, k := range adjustKinds {
		if k == kind {
			return true
		}
	}
	return false
}

var idCounter atomic.Uint64

// newID 生成「前缀_16位hex」ID。与 internal/types.NewID 同构，但网关侧不依赖
// Agent 运行时（§0.5：网关与 Agent 运行时解耦），因此本地保留一份。
// crypto/rand 失败时退化为计数器，仍保证同一进程内不重复。
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
