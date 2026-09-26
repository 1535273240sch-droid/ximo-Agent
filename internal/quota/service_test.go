package quota

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/model"
)

// ---------------------------------------------------------------------------
// fakeStore：accountStore 的内存实现
// ---------------------------------------------------------------------------
//
// 它按 §3/§6 的语义实现「读账户 → 校验 → 改 reserved → 落预占行 → 落账本行」这一个
// 原子序列（单把互斥锁代替存储层的单写队列），用来验证额度服务的调用参数、幂等键
// 传递、状态校验与账本对账。
//
// 账本列约定（仅本 fake）：Entry.Amount 是**可用额度增量**，BalanceAfter 是本行生效后的
// 可用额度，ReservedAfter 是本行生效后的 reserved_amount。真实列语义以 G-Store 为准，
// 本文件的断言只保证「服务层调用 + 不变量自洽」。

type fakeStore struct {
	mu       sync.Mutex
	accounts map[string]*model.QuotaAccount
	held     map[string]model.Reservation // key: userID + "\x00" + requestID
	resByID  map[string]string            // reservation.ID -> held key
	ledger   []model.LedgerEntry
	usage    map[string]model.UsageRecord // key: request_id（主键幂等）
	adjusts  map[string]model.LedgerEntry // key: idempotency_key
	calls    map[string]int
	errs     map[string]error
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		accounts: map[string]*model.QuotaAccount{},
		held:     map[string]model.Reservation{},
		resByID:  map[string]string{},
		usage:    map[string]model.UsageRecord{},
		adjusts:  map[string]model.LedgerEntry{},
		calls:    map[string]int{},
		errs:     map[string]error{},
	}
}

// inject 让指定方法下一次调用直接返回 err（模拟存储层故障）。
func (f *fakeStore) inject(method string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errs[method] = err
}

// enter 记一次调用并返回注入的错误。调用方必须已持锁。
func (f *fakeStore) enter(method string) error {
	f.calls[method]++
	return f.errs[method]
}

func (f *fakeStore) callCount(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[method]
}

// seed 建账户并（可选）用 topup 注入初始额度，保证账本能从头完整对账。
func (f *fakeStore) seed(userID string, total int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accounts[userID] = &model.QuotaAccount{UserID: userID, Status: model.QuotaStatusActive, UpdatedAt: 1}
	if total != 0 {
		f.adjustLocked(userID, model.LedgerTopup, total, "seed", "test", "seed:"+userID, "led_seed_"+userID, 1)
	}
}

func (f *fakeStore) setStatus(userID, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accounts[userID].Status = status
}

func (f *fakeStore) snapshot(userID string) model.QuotaAccount {
	f.mu.Lock()
	defer f.mu.Unlock()
	if acct, ok := f.accounts[userID]; ok {
		return *acct
	}
	return model.QuotaAccount{}
}

func (f *fakeStore) ledgerCopy() []model.LedgerEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]model.LedgerEntry, len(f.ledger))
	copy(out, f.ledger)
	return out
}

// ledgerOfUser 返回某用户的账本行（fake 的账本是全局的，对账必须按用户过滤）。
func (f *fakeStore) ledgerOfUser(userID string) []model.LedgerEntry {
	var out []model.LedgerEntry
	for _, e := range f.ledgerCopy() {
		if e.UserID == userID {
			out = append(out, e)
		}
	}
	return out
}

func (f *fakeStore) ledgerOfType(typ string) []model.LedgerEntry {
	var out []model.LedgerEntry
	for _, e := range f.ledgerCopy() {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

func (f *fakeStore) usageOf(requestID string) (model.UsageRecord, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.usage[requestID]
	return u, ok
}

// heldTotal 汇总某用户所有仍处于 held 的预占金额。
func (f *fakeStore) heldTotal(userID string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	var sum int64
	for _, r := range f.held {
		if r.UserID == userID && r.Status == model.ReservationHeld {
			sum += r.Amount
		}
	}
	return sum
}

func (f *fakeStore) reservationOf(requestID string) (model.Reservation, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.held {
		if r.RequestID == requestID {
			return r, true
		}
	}
	return model.Reservation{}, false
}

// appendLedgerLocked 追加账本行并写回该行生效后的快照。调用方必须已持锁。
func (f *fakeStore) appendLedgerLocked(e model.LedgerEntry, acct *model.QuotaAccount) model.LedgerEntry {
	e.BalanceAfter = acct.Available()
	e.ReservedAfter = acct.ReservedAmount
	f.ledger = append(f.ledger, e)
	return e
}

func (f *fakeStore) ReserveTx(ctx context.Context, r model.Reservation, ledgerID string, now int64) (model.Reservation, error) {
	if err := ctx.Err(); err != nil {
		return model.Reservation{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("ReserveTx"); err != nil {
		return model.Reservation{}, err
	}

	key := r.UserID + "\x00" + r.RequestID
	if existing, ok := f.held[key]; ok && existing.Status == model.ReservationHeld {
		return existing, nil // 幂等：同 (user, request) 不重复预占
	}
	acct, ok := f.accounts[r.UserID]
	if !ok {
		return model.Reservation{}, model.ErrNotFound
	}
	if acct.Status != model.QuotaStatusActive {
		return model.Reservation{}, fmt.Errorf("fake: 账户状态 %s: %w", acct.Status, model.ErrDisabled)
	}
	if acct.Available() < r.Amount {
		return model.Reservation{}, model.ErrInsufficientQuota
	}

	acct.ReservedAmount += r.Amount
	acct.Version++
	acct.UpdatedAt = now
	f.held[key] = r
	f.resByID[r.ID] = key
	f.appendLedgerLocked(model.LedgerEntry{
		ID: ledgerID, UserID: r.UserID, Type: model.LedgerReserve, RequestID: r.RequestID,
		Amount: -r.Amount, CreatedAt: now,
	}, acct)
	return r, nil
}

func (f *fakeStore) SettleTx(ctx context.Context, reservationID string, actual int64, ledgerID, requestID string, now int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("SettleTx"); err != nil {
		return err
	}

	key, ok := f.resByID[reservationID]
	if !ok {
		return model.ErrNotFound
	}
	r := f.held[key]
	switch r.Status {
	case model.ReservationSettled:
		return nil // 与 store 一致：重复结算是幂等 no-op，不再动账户
	case model.ReservationHeld:
		// 正常路径。
	default:
		return fmt.Errorf("fake: 预占状态 %s: %w", r.Status, model.ErrConflict)
	}
	acct := f.accounts[r.UserID]
	acct.ReservedAmount -= r.Amount
	acct.UsedAmount += actual // 超额（actual > held）照实记欠账，不拒绝
	acct.Version++
	acct.UpdatedAt = now
	r.Status = model.ReservationSettled
	r.SettledAmount = actual
	r.UpdatedAt = now
	f.held[key] = r
	f.appendLedgerLocked(model.LedgerEntry{
		ID: ledgerID, UserID: r.UserID, Type: model.LedgerSettle, RequestID: requestID,
		Amount: r.Amount - actual, CreatedAt: now,
	}, acct)
	return nil
}

func (f *fakeStore) ReleaseTx(ctx context.Context, reservationID string, ledgerID string, now int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("ReleaseTx"); err != nil {
		return err
	}

	key, ok := f.resByID[reservationID]
	if !ok {
		return model.ErrNotFound
	}
	r := f.held[key]
	switch r.Status {
	case model.ReservationReleased, model.ReservationExpired:
		return nil // 与 store 一致：已释放/已回收的预占重复释放是幂等 no-op
	case model.ReservationHeld:
		// 正常路径。
	default:
		return fmt.Errorf("fake: 预占状态 %s: %w", r.Status, model.ErrConflict)
	}
	acct := f.accounts[r.UserID]
	acct.ReservedAmount -= r.Amount
	acct.Version++
	acct.UpdatedAt = now
	r.Status = model.ReservationReleased
	r.UpdatedAt = now
	f.held[key] = r
	f.appendLedgerLocked(model.LedgerEntry{
		ID: ledgerID, UserID: r.UserID, Type: model.LedgerRelease, RequestID: r.RequestID,
		Amount: r.Amount, CreatedAt: now,
	}, acct)
	return nil
}

func (f *fakeStore) AdjustTx(ctx context.Context, userID string, kind string, amount int64, reason, operatorID, idempotencyKey, ledgerID string, now int64) (model.LedgerEntry, error) {
	if err := ctx.Err(); err != nil {
		return model.LedgerEntry{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("AdjustTx"); err != nil {
		return model.LedgerEntry{}, err
	}
	if existing, ok := f.adjusts[idempotencyKey]; ok {
		return existing, nil // 幂等：同 key 不重复入账
	}
	if _, ok := f.accounts[userID]; !ok {
		return model.LedgerEntry{}, model.ErrNotFound
	}
	return f.adjustLocked(userID, kind, amount, reason, operatorID, idempotencyKey, ledgerID, now), nil
}

// adjustLocked 是 AdjustTx 的记账主体：符号只由 kind 决定（§6），
// freeze/unfreeze 只切账户状态、不改总额（与 store 的 adjustDelta 一致）。调用方必须已持锁。
func (f *fakeStore) adjustLocked(userID, kind string, amount int64, reason, operatorID, idempotencyKey, ledgerID string, now int64) model.LedgerEntry {
	acct := f.accounts[userID]
	delta := amount
	switch kind {
	case model.LedgerTopup:
		acct.TotalAmount += amount
	case model.LedgerDebit, model.LedgerExpire:
		acct.TotalAmount -= amount
		delta = -amount
	case model.LedgerFreeze:
		acct.Status = model.QuotaStatusFrozen
		delta = 0
	case model.LedgerUnfreeze:
		acct.Status = model.QuotaStatusActive
		delta = 0
	}
	acct.Version++
	acct.UpdatedAt = now
	entry := f.appendLedgerLocked(model.LedgerEntry{
		ID: ledgerID, UserID: userID, Type: kind, IdempotencyKey: idempotencyKey,
		OperatorID: operatorID, Reason: reason, Amount: delta, CreatedAt: now,
	}, acct)
	f.adjusts[idempotencyKey] = entry
	return entry
}

func (f *fakeStore) GetQuotaAccount(ctx context.Context, userID string) (model.QuotaAccount, error) {
	if err := ctx.Err(); err != nil {
		return model.QuotaAccount{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("GetQuotaAccount"); err != nil {
		return model.QuotaAccount{}, err
	}
	acct, ok := f.accounts[userID]
	if !ok {
		return model.QuotaAccount{}, model.ErrNotFound
	}
	return *acct, nil
}

func (f *fakeStore) ExpireReservations(ctx context.Context, now int64, limit int) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("ExpireReservations"); err != nil {
		return 0, err
	}

	n := 0
	for key, r := range f.held {
		if n >= limit {
			break
		}
		if r.Status != model.ReservationHeld || r.ExpiresAt > now {
			continue
		}
		acct := f.accounts[r.UserID]
		acct.ReservedAmount -= r.Amount
		acct.Version++
		acct.UpdatedAt = now
		r.Status = model.ReservationExpired
		r.UpdatedAt = now
		f.held[key] = r
		f.appendLedgerLocked(model.LedgerEntry{
			// 与 store 的 ExpireReservations 一致：回收记 type=release、amount=+held，
			// 幂等键 resv-expire:<id> 保证不二次入账。
			ID: "gwl-expire-" + r.ID, UserID: r.UserID, Type: model.LedgerRelease, RequestID: r.RequestID,
			IdempotencyKey: "resv-expire-" + r.ID, Amount: r.Amount, CreatedAt: now,
		}, acct)
		n++
	}
	return n, nil
}

func (f *fakeStore) InsertUsage(ctx context.Context, u model.UsageRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("InsertUsage"); err != nil {
		return err
	}
	if _, ok := f.usage[u.RequestID]; ok {
		return nil // request_id 主键冲突视为已存在
	}
	f.usage[u.RequestID] = u
	return nil
}

// ---------------------------------------------------------------------------
// 测试辅助
// ---------------------------------------------------------------------------

// checkReconciled 断言账本与账户快照对账（§22：并发预占不得产生不可追溯余额）。
func checkReconciled(t *testing.T, f *fakeStore, userID string) {
	t.Helper()
	acct := f.snapshot(userID)
	ledger := f.ledgerOfUser(userID)

	var sum int64
	for _, e := range ledger {
		sum += e.Amount
	}
	if sum != acct.Available() {
		t.Errorf("账本可用额度增量合计 %d ≠ 账户可用额度 %d", sum, acct.Available())
	}
	if len(ledger) > 0 {
		last := ledger[len(ledger)-1]
		if last.BalanceAfter != acct.Available() || last.ReservedAfter != acct.ReservedAmount {
			t.Errorf("末行账本快照 (available=%d reserved=%d) ≠ 账户 (available=%d reserved=%d)",
				last.BalanceAfter, last.ReservedAfter, acct.Available(), acct.ReservedAmount)
		}
	}
	if held := f.heldTotal(userID); held != acct.ReservedAmount {
		t.Errorf("未结算预占合计 %d ≠ reserved_amount %d", held, acct.ReservedAmount)
	}
	if acct.ReservedAmount < 0 {
		t.Errorf("reserved_amount 为负：%d", acct.ReservedAmount)
	}
}

// ---------------------------------------------------------------------------
// Reserve
// ---------------------------------------------------------------------------

func TestReserveSuccessAndIdempotent(t *testing.T) {
	f := newFakeStore()
	f.seed("u1", 10_000)
	svc := New(f)
	svc.now = func() int64 { return 1_700_000_000_000 }
	svc.SetReservationTTL(3 * time.Minute)

	ctx := context.Background()
	first, err := svc.Reserve(ctx, "u1", "req-1", 2_500)
	if err != nil {
		t.Fatalf("首次预占应成功，实际 %v", err)
	}
	if first.Status != model.ReservationHeld || first.Amount != 2_500 {
		t.Fatalf("预占行不符：%+v", first)
	}
	if first.ID == "" || first.ExpiresAt != 1_700_000_000_000+180_000 {
		t.Fatalf("预占 ID/过期时间不符：%+v", first)
	}

	// 重复预占：同 (user, request) 必须幂等——返回既有行、不重复扣减。
	second, err := svc.Reserve(ctx, "u1", "req-1", 2_500)
	if err != nil {
		t.Fatalf("重复预占应幂等成功，实际 %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("幂等预占应返回既有行 ID %s，实际 %s", first.ID, second.ID)
	}
	acct := f.snapshot("u1")
	if acct.ReservedAmount != 2_500 {
		t.Fatalf("重复预占导致重复扣减：reserved=%d", acct.ReservedAmount)
	}
	if got := len(f.ledgerOfType(model.LedgerReserve)); got != 1 {
		t.Fatalf("账本应只有 1 条 reserve，实际 %d", got)
	}
	if acct.Version != 2 { // 1 次 seed topup + 1 次预占
		t.Errorf("账户 version=%d，期望 2", acct.Version)
	}
	checkReconciled(t, f, "u1")
}

func TestReserveInsufficientQuota(t *testing.T) {
	f := newFakeStore()
	f.seed("u1", 1_000)
	svc := New(f)

	_, err := svc.Reserve(context.Background(), "u1", "req-1", 1_001)
	if !errors.Is(err, model.ErrInsufficientQuota) {
		t.Fatalf("额度不足必须可判定为 ErrInsufficientQuota，实际 %v", err)
	}
	acct := f.snapshot("u1")
	if acct.ReservedAmount != 0 || len(f.ledgerOfType(model.LedgerReserve)) != 0 {
		t.Fatalf("预占失败不得留下任何痕迹：reserved=%d ledger=%d", acct.ReservedAmount, len(f.ledgerOfType(model.LedgerReserve)))
	}
	checkReconciled(t, f, "u1")

	// 恰好等于可用额度必须成功（边界）。
	if _, err := svc.Reserve(context.Background(), "u1", "req-2", 1_000); err != nil {
		t.Fatalf("恰好用满应成功，实际 %v", err)
	}
	if acct := f.snapshot("u1"); acct.Available() != 0 {
		t.Fatalf("用满后 available 应为 0，实际 %d", acct.Available())
	}
}

func TestReserveInvalidArguments(t *testing.T) {
	f := newFakeStore()
	f.seed("u1", 1_000)
	svc := New(f)
	ctx := context.Background()

	cases := []struct {
		name      string
		userID    string
		requestID string
		amount    int64
	}{
		{"空 userID", "", "req-1", 10},
		{"空 requestID", "u1", "", 10},
		{"负预占金额", "u1", "req-1", -1},
	}
	for _, tc := range cases {
		if _, err := svc.Reserve(ctx, tc.userID, tc.requestID, tc.amount); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%s：期望 ErrInvalidArgument，实际 %v", tc.name, err)
		}
	}
	if n := f.callCount("ReserveTx"); n != 0 {
		t.Fatalf("参数校验不通过时不得触碰存储，实际调用 %d 次", n)
	}
}

// 存储层错误必须原样带上下文抛给调用方，不能被吞掉或降级成「放行」。
func TestReserveStoreErrorsAreNotSwallowed(t *testing.T) {
	ctx := context.Background()

	t.Run("账户被冻结", func(t *testing.T) {
		f := newFakeStore()
		f.seed("u1", 1_000)
		f.setStatus("u1", model.QuotaStatusFrozen)
		_, err := New(f).Reserve(ctx, "u1", "req-1", 10)
		if !errors.Is(err, model.ErrDisabled) {
			t.Fatalf("期望 ErrDisabled，实际 %v", err)
		}
	})

	t.Run("用户不存在", func(t *testing.T) {
		f := newFakeStore()
		_, err := New(f).Reserve(ctx, "ghost", "req-1", 10)
		if !errors.Is(err, model.ErrNotFound) {
			t.Fatalf("期望 ErrNotFound，实际 %v", err)
		}
	})

	t.Run("存储故障", func(t *testing.T) {
		f := newFakeStore()
		f.seed("u1", 1_000)
		boom := errors.New("disk on fire")
		f.inject("ReserveTx", boom)
		_, err := New(f).Reserve(ctx, "u1", "req-1", 10)
		if !errors.Is(err, boom) {
			t.Fatalf("期望包装后的底层错误，实际 %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// Settle
// ---------------------------------------------------------------------------

func TestSettleReleasesDifference(t *testing.T) {
	f := newFakeStore()
	f.seed("u1", 10_000)
	svc := New(f)
	ctx := context.Background()

	r, err := svc.Reserve(ctx, "u1", "req-1", 5_000)
	if err != nil {
		t.Fatalf("预占失败：%v", err)
	}
	rec, err := svc.Settle(ctx, r, 800, 400, 1_000) // (800+400)*1000/1000 = 1200
	if err != nil {
		t.Fatalf("结算失败：%v", err)
	}
	if rec.CostMicro != 1_200 || rec.Status != UsageStatusSettled {
		t.Fatalf("用量记录不符：%+v", rec)
	}
	if rec.RequestID != "req-1" || rec.UserID != "u1" {
		t.Fatalf("用量记录未回填 request/user：%+v", rec)
	}

	acct := f.snapshot("u1")
	if acct.ReservedAmount != 0 {
		t.Fatalf("结算后 reserved 应归零，实际 %d", acct.ReservedAmount)
	}
	if acct.UsedAmount != 1_200 {
		t.Fatalf("结算后 used 应为 1200，实际 %d", acct.UsedAmount)
	}
	if acct.Available() != 8_800 {
		t.Fatalf("结算后可用额度应为 8800，实际 %d", acct.Available())
	}

	stored, ok := f.usageOf("req-1")
	if !ok || stored.CostMicro != 1_200 {
		t.Fatalf("用量未落库或成本不符：%+v ok=%v", stored, ok)
	}
	settles := f.ledgerOfType(model.LedgerSettle)
	if len(settles) != 1 || settles[0].RequestID != "req-1" {
		t.Fatalf("结算账本行不符：%+v", settles)
	}
	checkReconciled(t, f, "u1")
}

// 超额结算：不得为「余额不为负」而拒绝，必须记为欠账。
func TestSettleOverdraftRecordsDebt(t *testing.T) {
	f := newFakeStore()
	f.seed("u1", 1_000)
	svc := New(f)
	ctx := context.Background()

	r, err := svc.Reserve(ctx, "u1", "req-1", 100)
	if err != nil {
		t.Fatalf("预占失败：%v", err)
	}
	rec, err := svc.Settle(ctx, r, 2_000, 1_000, 1_000) // 实际 3000，远超预占 100
	if err != nil {
		t.Fatalf("超额结算不得失败（应记欠账），实际 %v", err)
	}
	if rec.CostMicro != 3_000 {
		t.Fatalf("超额成本应为 3000，实际 %d", rec.CostMicro)
	}
	acct := f.snapshot("u1")
	if acct.UsedAmount != 3_000 || acct.ReservedAmount != 0 {
		t.Fatalf("超额结算后 used=%d reserved=%d，期望 3000/0", acct.UsedAmount, acct.ReservedAmount)
	}
	if acct.Available() != -2_000 {
		t.Fatalf("欠账 2000 应体现在 available 上，实际 %d", acct.Available())
	}
	checkReconciled(t, f, "u1")

	// 欠账后可以补平：topup 2000 → available 归零。
	if _, err := svc.Adjust(ctx, "u1", model.LedgerTopup, 2_000, "补欠账", "admin-1", "topup-1"); err != nil {
		t.Fatalf("补账失败：%v", err)
	}
	if acct := f.snapshot("u1"); acct.Available() != 0 {
		t.Fatalf("补账后 available 应为 0，实际 %d", acct.Available())
	}
}

func TestSettleRejectsFinishedReservation(t *testing.T) {
	ctx := context.Background()
	for _, status := range []string{model.ReservationSettled, model.ReservationReleased, model.ReservationExpired} {
		f := newFakeStore()
		f.seed("u1", 1_000)
		svc := New(f)
		r, err := svc.Reserve(ctx, "u1", "req-1", 100)
		if err != nil {
			t.Fatalf("预占失败：%v", err)
		}
		r.Status = status
		if _, err := svc.Settle(ctx, r, 1, 1, 1); !errors.Is(err, model.ErrConflict) {
			t.Errorf("状态 %s 的预占不得重复结算，实际 %v", status, err)
		}
		if n := f.callCount("SettleTx"); n != 0 {
			t.Errorf("状态 %s：本地状态校验应拦在存储之前，实际调用 %d 次", status, n)
		}
	}
}

func TestSettleInvalidTokens(t *testing.T) {
	f := newFakeStore()
	f.seed("u1", 1_000)
	svc := New(f)
	ctx := context.Background()
	r, err := svc.Reserve(ctx, "u1", "req-1", 100)
	if err != nil {
		t.Fatalf("预占失败：%v", err)
	}
	if _, err := svc.Settle(ctx, r, -1, 0, 1); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("负 token 应拒绝，实际 %v", err)
	}
	if _, err := svc.Settle(ctx, model.Reservation{}, 1, 1, 1); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("空预占 ID 应拒绝，实际 %v", err)
	}
	if n := f.callCount("SettleTx"); n != 0 {
		t.Fatalf("参数校验不通过时不得触碰存储，实际 %d 次", n)
	}
}

// 结算已生效但用量落库失败时：必须报错，且把「结算已生效」写在错误里，
// 返回的 UsageRecord 仍然可用（调用方可重试落库）。
func TestSettleUsageInsertFailureIsReported(t *testing.T) {
	f := newFakeStore()
	f.seed("u1", 1_000)
	svc := New(f)
	ctx := context.Background()

	r, err := svc.Reserve(ctx, "u1", "req-1", 500)
	if err != nil {
		t.Fatalf("预占失败：%v", err)
	}
	boom := errors.New("usage table locked")
	f.inject("InsertUsage", boom)
	rec, err := svc.Settle(ctx, r, 100, 100, 1_000)
	if !errors.Is(err, boom) {
		t.Fatalf("用量落库失败必须上报，实际 %v", err)
	}
	if rec.CostMicro != 200 {
		t.Fatalf("失败时返回的用量记录仍应可用，实际 %+v", rec)
	}
	if acct := f.snapshot("u1"); acct.ReservedAmount != 0 || acct.UsedAmount != 200 {
		t.Fatalf("结算本身应已生效：reserved=%d used=%d", acct.ReservedAmount, acct.UsedAmount)
	}
	if _, ok := f.usageOf("req-1"); ok {
		t.Fatal("注入的落库失败不应写入用量")
	}

	// 故障恢复后重试：存储层的 SettleTx 对已结算预占幂等，重试只补齐用量行，
	// 不会二次扣费。
	f.inject("InsertUsage", nil)
	rec2, err := svc.Settle(ctx, r, 100, 100, 1_000)
	if err != nil {
		t.Fatalf("重试应成功：%v", err)
	}
	if rec2.CostMicro != 200 {
		t.Fatalf("重试的成本应与首次一致，实际 %d", rec2.CostMicro)
	}
	if acct := f.snapshot("u1"); acct.UsedAmount != 200 || acct.ReservedAmount != 0 {
		t.Fatalf("重试导致重复扣费：reserved=%d used=%d", acct.ReservedAmount, acct.UsedAmount)
	}
	if stored, ok := f.usageOf("req-1"); !ok || stored.CostMicro != 200 {
		t.Fatalf("重试后用量仍未落库：%+v ok=%v", stored, ok)
	}
	if n := len(f.ledgerOfType(model.LedgerSettle)); n != 1 {
		t.Fatalf("结算账本应只有 1 条，实际 %d 条", n)
	}
	checkReconciled(t, f, "u1")
}

func TestSettleWithUsageKeepsModelAndProvider(t *testing.T) {
	f := newFakeStore()
	f.seed("u1", 1_000)
	svc := New(f)
	ctx := context.Background()

	r, err := svc.Reserve(ctx, "u1", "req-1", 500)
	if err != nil {
		t.Fatalf("预占失败：%v", err)
	}
	rec, err := svc.SettleWithUsage(ctx, r, model.UsageRecord{
		ModelID: "gpt-4o", ProviderID: "p1", InputTokens: 10, OutputTokens: 20, LatencyMS: 42,
	}, 1_000)
	if err != nil {
		t.Fatalf("结算失败：%v", err)
	}
	stored, ok := f.usageOf("req-1")
	if !ok || stored.ModelID != "gpt-4o" || stored.ProviderID != "p1" || stored.LatencyMS != 42 {
		t.Fatalf("用量行的模型/上游字段丢失：%+v ok=%v", stored, ok)
	}
	if stored.CostMicro != 30 || rec.CostMicro != 30 {
		t.Fatalf("成本应为 30，实际 stored=%d rec=%d", stored.CostMicro, rec.CostMicro)
	}
}

// ---------------------------------------------------------------------------
// Release
// ---------------------------------------------------------------------------

func TestReleaseReturnsQuotaAndIsIdempotent(t *testing.T) {
	f := newFakeStore()
	f.seed("u1", 1_000)
	svc := New(f)
	ctx := context.Background()

	r, err := svc.Reserve(ctx, "u1", "req-1", 600)
	if err != nil {
		t.Fatalf("预占失败：%v", err)
	}
	if err := svc.Release(ctx, r, "upstream_failed"); err != nil {
		t.Fatalf("释放失败：%v", err)
	}
	if acct := f.snapshot("u1"); acct.ReservedAmount != 0 || acct.Available() != 1_000 {
		t.Fatalf("释放后额度未归还：reserved=%d available=%d", acct.ReservedAmount, acct.Available())
	}
	if n := len(f.ledgerOfType(model.LedgerRelease)); n != 1 {
		t.Fatalf("应只有 1 条 release 账本，实际 %d", n)
	}

	// 重复释放（上游失败路径上「错误处理 + defer」是常态）必须幂等且不再落账本。
	r.Status = model.ReservationReleased
	if err := svc.Release(ctx, r, "user_cancelled"); err != nil {
		t.Fatalf("重复释放应幂等成功，实际 %v", err)
	}
	if n := len(f.ledgerOfType(model.LedgerRelease)); n != 1 {
		t.Fatalf("重复释放不得再入账，实际 %d 条", n)
	}
	checkReconciled(t, f, "u1")
}

func TestReleaseRejectsSettledAndEmptyID(t *testing.T) {
	ctx := context.Background()

	t.Run("空预占 ID", func(t *testing.T) {
		f := newFakeStore()
		svc := New(f)
		if err := svc.Release(ctx, model.Reservation{}, "x"); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("空预占 ID 应拒绝，实际 %v", err)
		}
		if n := f.callCount("ReleaseTx"); n != 0 {
			t.Errorf("参数校验应拦在存储之前，实际调用 ReleaseTx %d 次", n)
		}
	})

	t.Run("本地状态已知为已结算", func(t *testing.T) {
		f := newFakeStore()
		f.seed("u1", 1_000)
		svc := New(f)
		r, err := svc.Reserve(ctx, "u1", "req-1", 100)
		if err != nil {
			t.Fatalf("预占失败：%v", err)
		}
		r.Status = model.ReservationSettled // 调用方从库里读到的真实状态
		if err := svc.Release(ctx, r, "user_cancelled"); !errors.Is(err, model.ErrConflict) {
			t.Errorf("已结算的预占不得释放，实际 %v", err)
		}
		if n := f.callCount("ReleaseTx"); n != 0 {
			t.Fatalf("状态校验应拦在存储之前，实际调用 ReleaseTx %d 次", n)
		}
	})

	// 调用方持有的状态陈旧（仍是 held），但库里已经结算：存储层兜底拒绝，
	// 服务层必须把这个错误带出来，而不是当成释放成功。
	t.Run("本地状态陈旧由存储层兜底", func(t *testing.T) {
		f := newFakeStore()
		f.seed("u1", 1_000)
		svc := New(f)
		r, err := svc.Reserve(ctx, "u1", "req-1", 100)
		if err != nil {
			t.Fatalf("预占失败：%v", err)
		}
		if _, err := svc.Settle(ctx, r, 1, 1, 1_000); err != nil { // 成本 (1+1)*1000/1000 = 2
			t.Fatalf("结算失败：%v", err)
		}
		if err := svc.Release(ctx, r, "user_cancelled"); !errors.Is(err, model.ErrConflict) {
			t.Errorf("期望存储层返回 ErrConflict，实际 %v", err)
		}
		if acct := f.snapshot("u1"); acct.ReservedAmount != 0 || acct.UsedAmount != 2 {
			t.Errorf("账本被错误释放污染：%+v", acct)
		}
	})

	// 上游挂住 → 预占被 ReapExpired 回收 → 请求随后失败走释放路径：
	// 额度早已归还，释放必须是幂等成功，不能报错、不能二次归还额度。
	t.Run("预占已被回收后释放是幂等成功", func(t *testing.T) {
		f := newFakeStore()
		f.seed("u1", 1_000)
		svc := New(f)
		svc.SetReservationTTL(time.Minute)
		svc.now = func() int64 { return 1_000 }
		r, err := svc.Reserve(ctx, "u1", "req-1", 400)
		if err != nil {
			t.Fatalf("预占失败：%v", err)
		}
		if n, err := svc.ReapExpired(ctx, r.ExpiresAt, 10); err != nil || n != 1 {
			t.Fatalf("回收失败：n=%d err=%v", n, err)
		}
		releasesBefore := len(f.ledgerOfType(model.LedgerRelease))

		// 状态陈旧（仍是 held）：服务层无从判断，交给存储层做幂等判定，返回 nil。
		if err := svc.Release(ctx, r, "upstream_failed"); err != nil {
			t.Errorf("已回收预占的释放应幂等成功，实际 %v", err)
		}
		if got := len(f.ledgerOfType(model.LedgerRelease)); got != releasesBefore {
			t.Errorf("幂等释放不得二次入账：%d → %d", releasesBefore, got)
		}
		if acct := f.snapshot("u1"); acct.ReservedAmount != 0 || acct.Available() != 1_000 {
			t.Fatalf("额度状态被破坏：%+v", acct)
		}

		// 状态已知为 expired（从库里刷新过）：本地即可判定，不触碰存储。
		callsBefore := f.callCount("ReleaseTx")
		expired := r
		expired.Status = model.ReservationExpired
		if err := svc.Release(ctx, expired, "upstream_failed"); err != nil {
			t.Errorf("已回收预占的释放应幂等成功，实际 %v", err)
		}
		if got := f.callCount("ReleaseTx"); got != callsBefore {
			t.Errorf("本地已知状态不应触碰存储，实际多调用 %d 次", got-callsBefore)
		}
	})
}

// ---------------------------------------------------------------------------
// Adjust / Account
// ---------------------------------------------------------------------------

func TestAdjustValidatesArguments(t *testing.T) {
	f := newFakeStore()
	f.seed("u1", 1_000)
	svc := New(f)
	ctx := context.Background()

	cases := []struct {
		name           string
		userID         string
		kind           string
		amount         int64
		operatorID     string
		idempotencyKey string
	}{
		{"空 userID", "", model.LedgerTopup, 10, "admin", "k1"},
		{"非法账本类型 reserve", "u1", model.LedgerReserve, 10, "admin", "k1"},
		{"非法账本类型 settle", "u1", model.LedgerSettle, 10, "admin", "k1"},
		{"非法账本类型随意字符串", "u1", "gift", 10, "admin", "k1"},
		{"负金额", "u1", model.LedgerTopup, -10, "admin", "k1"},
		{"缺幂等键", "u1", model.LedgerTopup, 10, "admin", ""},
		{"缺操作者", "u1", model.LedgerTopup, 10, "", "k1"},
	}
	for _, tc := range cases {
		if _, err := svc.Adjust(ctx, tc.userID, tc.kind, tc.amount, "r", tc.operatorID, tc.idempotencyKey); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%s：期望 ErrInvalidArgument，实际 %v", tc.name, err)
		}
	}
	if n := f.callCount("AdjustTx"); n != 0 {
		t.Fatalf("参数校验不通过时不得触碰存储，实际 %d 次", n)
	}
}

func TestAdjustIdempotentReplayAndSignByKind(t *testing.T) {
	f := newFakeStore()
	f.seed("u1", 0)
	svc := New(f)
	ctx := context.Background()

	first, err := svc.Adjust(ctx, "u1", model.LedgerTopup, 5_000, "首次充值", "admin-1", "op-1")
	if err != nil {
		t.Fatalf("充值失败：%v", err)
	}
	if first.Amount != 5_000 || first.OperatorID != "admin-1" || first.IdempotencyKey != "op-1" {
		t.Fatalf("账本行不符：%+v", first)
	}

	// 同 key 重放（后台重试/双击）：不得二次入账，返回既有行。
	replay, err := svc.Adjust(ctx, "u1", model.LedgerTopup, 5_000, "首次充值", "admin-1", "op-1")
	if err != nil {
		t.Fatalf("幂等重放应成功，实际 %v", err)
	}
	if replay.ID != first.ID {
		t.Fatalf("幂等重放应返回既有账本行 %s，实际 %s", first.ID, replay.ID)
	}
	if acct := f.snapshot("u1"); acct.TotalAmount != 5_000 {
		t.Fatalf("幂等重放导致重复入账：total=%d", acct.TotalAmount)
	}
	if n := len(f.ledgerOfType(model.LedgerTopup)); n != 1 {
		t.Fatalf("应只有 1 条 topup 账本，实际 %d", n)
	}

	// 扣款：调用方只传数量级，符号由 kind 决定。
	debit, err := svc.Adjust(ctx, "u1", model.LedgerDebit, 1_200, "违规扣减", "admin-1", "op-2")
	if err != nil {
		t.Fatalf("扣款失败：%v", err)
	}
	if debit.Amount != -1_200 {
		t.Fatalf("debit 应以负号入账，实际 %d", debit.Amount)
	}
	if acct := f.snapshot("u1"); acct.TotalAmount != 3_800 {
		t.Fatalf("扣款后总额应为 3800，实际 %d", acct.TotalAmount)
	}
	checkReconciled(t, f, "u1")
}

func TestAdjustStoreErrorPassthrough(t *testing.T) {
	f := newFakeStore()
	svc := New(f)
	_, err := svc.Adjust(context.Background(), "ghost", model.LedgerTopup, 100, "r", "admin", "k1")
	if !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("期望 ErrNotFound，实际 %v", err)
	}
}

func TestAccount(t *testing.T) {
	f := newFakeStore()
	f.seed("u1", 1_000)
	svc := New(f)
	ctx := context.Background()

	if _, err := svc.Account(ctx, "u1"); err != nil {
		t.Fatalf("查询账户失败：%v", err)
	}
	if _, err := svc.Account(ctx, "ghost"); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("未知用户应返回 ErrNotFound，实际 %v", err)
	}
	if _, err := svc.Account(ctx, ""); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("空 userID 应拒绝，实际 %v", err)
	}
}

// ---------------------------------------------------------------------------
// ReapExpired
// ---------------------------------------------------------------------------

func TestReapExpiredReclaimsTimedOutHolds(t *testing.T) {
	f := newFakeStore()
	f.seed("u1", 1_000)
	f.seed("u2", 1_000)
	svc := New(f)
	svc.SetReservationTTL(time.Minute)
	svc.now = func() int64 { return 1_000 }
	ctx := context.Background()

	r1, err := svc.Reserve(ctx, "u1", "req-1", 400)
	if err != nil {
		t.Fatalf("预占失败：%v", err)
	}
	if _, err := svc.Reserve(ctx, "u2", "req-2", 700); err != nil {
		t.Fatalf("预占失败：%v", err)
	}

	// 未到期：不回收。
	if n, err := svc.ReapExpired(ctx, r1.ExpiresAt-1, 100); err != nil || n != 0 {
		t.Fatalf("未到期的预占不得回收：n=%d err=%v", n, err)
	}
	// 到期：回收两条。
	if n, err := svc.ReapExpired(ctx, r1.ExpiresAt, 100); err != nil || n != 2 {
		t.Fatalf("到期预占应回收 2 条：n=%d err=%v", n, err)
	}
	for _, uid := range []string{"u1", "u2"} {
		if acct := f.snapshot(uid); acct.ReservedAmount != 0 || acct.Available() != 1_000 {
			t.Errorf("%s 回收后额度未归位：reserved=%d available=%d", uid, acct.ReservedAmount, acct.Available())
		}
		checkReconciled(t, f, uid)
	}
	// 回收可重复调用：已 expired 的行不再计数。
	if n, err := svc.ReapExpired(ctx, r1.ExpiresAt, 100); err != nil || n != 0 {
		t.Fatalf("重复回收应为 0：n=%d err=%v", n, err)
	}
	// 已回收的预占不能再结算。
	if _, err := svc.Settle(ctx, model.Reservation{ID: r1.ID, UserID: "u1", RequestID: "req-1"}, 1, 1, 1); err == nil {
		t.Error("已过期预占不得结算")
	}
}

func TestReapExpiredUsesClockAndDefaultLimit(t *testing.T) {
	f := newFakeStore()
	f.seed("u1", 1_000)
	svc := New(f)
	svc.SetReservationTTL(time.Minute)
	now := int64(5_000)
	svc.now = func() int64 { return now }
	ctx := context.Background()

	r, err := svc.Reserve(ctx, "u1", "req-1", 400)
	if err != nil {
		t.Fatalf("预占失败：%v", err)
	}
	now = r.ExpiresAt
	// now/limit 传 0：用注入时钟与默认 limit。
	if n, err := svc.ReapExpired(ctx, 0, 0); err != nil || n != 1 {
		t.Fatalf("默认参数应回收 1 条：n=%d err=%v", n, err)
	}
	if n := f.callCount("ExpireReservations"); n != 1 {
		t.Fatalf("应调用存储 1 次，实际 %d", n)
	}
}

func TestReapExpiredStoreError(t *testing.T) {
	f := newFakeStore()
	boom := errors.New("wal broken")
	f.inject("ExpireReservations", boom)
	if _, err := New(f).ReapExpired(context.Background(), 1, 1); !errors.Is(err, boom) {
		t.Fatalf("存储错误必须上报，实际 %v", err)
	}
}

// ---------------------------------------------------------------------------
// 估算
// ---------------------------------------------------------------------------

func TestEstimateInputTokens(t *testing.T) {
	cases := []struct {
		chars int
		want  int64
	}{
		{0, 0}, {-10, 0}, {1, 1}, {4, 1}, {5, 2}, {400, 100}, {1_000, 250},
	}
	for _, tc := range cases {
		if got := EstimateInputTokens(tc.chars); got != tc.want {
			t.Errorf("EstimateInputTokens(%d)=%d，期望 %d", tc.chars, got, tc.want)
		}
	}
}

func TestEstimateMicro(t *testing.T) {
	cases := []struct {
		name                 string
		input, output, price int64
		want                 int64
	}{
		{"零 token", 0, 0, 1_000, 0},
		{"契约公式", 1_000, 500, 2_000, 3_000},
		{"无价格用默认 1micro/1K", 100, 900, 0, 1},
		{"不足 1 微单位向下取整", 1, 0, 1, 0},
		{"负值按 0 处理", -5, -5, -2, 0},
		{"token 溢出饱和", math.MaxInt64, 1, 1, math.MaxInt64},
		{"单价溢出饱和", 5, 0, math.MaxInt64, math.MaxInt64},
	}
	for _, tc := range cases {
		if got := EstimateMicro(tc.input, tc.output, tc.price); got != tc.want {
			t.Errorf("%s：EstimateMicro(%d,%d,%d)=%d，期望 %d", tc.name, tc.input, tc.output, tc.price, got, tc.want)
		}
	}
	// 估算与结算共用同一段算术：同样的参数必须得到同样的金额。
	if EstimateMicro(1_000, 500, 2_000) != CostMicro(1_000, 500, 2_000) {
		t.Error("估算与结算公式出现漂移")
	}
}

// ---------------------------------------------------------------------------
// 并发
// ---------------------------------------------------------------------------

// 多 goroutine 争抢同一账户：不得超额、不得出现无法追溯的余额，
// 账本、快照、未结算预占三者必须对账。
func TestConcurrentReserveNeverOverdraws(t *testing.T) {
	const (
		userID  = "u1"
		total   = int64(10_000)
		unit    = int64(1_000)
		workers = 40
	)
	f := newFakeStore()
	f.seed(userID, total)
	svc := New(f)

	var (
		wg          sync.WaitGroup
		okCount     atomic.Int64
		insuffCount atomic.Int64
		ids         = make([]string, workers)
		errsOther   atomic.Int64
	)
	start := make(chan struct{})
	for i := range workers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			r, err := svc.Reserve(context.Background(), userID, fmt.Sprintf("req-%d", i), unit)
			switch {
			case err == nil:
				okCount.Add(1)
				ids[i] = r.ID
			case errors.Is(err, model.ErrInsufficientQuota):
				insuffCount.Add(1)
			default:
				errsOther.Add(1)
				t.Errorf("第 %d 个请求出现意外错误：%v", i, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if got := errsOther.Load(); got != 0 {
		t.Fatalf("出现 %d 个非额度不足的错误", got)
	}
	// total/unit = 10 个名额：并发下必须不多不少正好 10 个成功。
	if got := okCount.Load(); got != total/unit {
		t.Fatalf("成功预占 %d 次，期望 %d 次（不得超额/少算）", got, total/unit)
	}
	if got := insuffCount.Load(); got != workers-total/unit {
		t.Fatalf("额度不足 %d 次，期望 %d 次", got, workers-total/unit)
	}

	acct := f.snapshot(userID)
	if acct.ReservedAmount != total {
		t.Fatalf("reserved_amount=%d，期望 %d", acct.ReservedAmount, total)
	}
	if acct.Available() != 0 {
		t.Fatalf("available=%d，期望 0（额度恰好用满）", acct.Available())
	}
	if acct.Version != 1+total/unit {
		t.Fatalf("version=%d，期望 %d（每次成功预占 +1）", acct.Version, 1+total/unit)
	}
	// 账户自洽：total - used - reserved 恒等于 available，且永不为负。
	if acct.TotalAmount-acct.UsedAmount-acct.ReservedAmount != acct.Available() || acct.Available() < 0 {
		t.Fatalf("账户不变量被破坏：%+v", acct)
	}
	// 每个成功的预占都有独立 ID（不重复返还同一条行）。
	seen := map[string]bool{}
	for i, id := range ids {
		if id == "" {
			continue
		}
		if seen[id] {
			t.Fatalf("第 %d 个成功预占拿到了重复 ID %s", i, id)
		}
		seen[id] = true
	}
	if len(seen) != int(total/unit) {
		t.Fatalf("独立预占 ID %d 个，期望 %d 个", len(seen), total/unit)
	}
	if got := len(f.ledgerOfType(model.LedgerReserve)); got != int(total/unit) {
		t.Fatalf("reserve 账本 %d 条，期望 %d 条", got, total/unit)
	}
	checkReconciled(t, f, userID)
}

// 同一 requestID 被并发重发（客户端重试）：幂等键必须只允许一次预占，
// 所有调用者拿到同一条权威预占行。
func TestConcurrentReserveSameRequestIDReservesOnce(t *testing.T) {
	const (
		userID  = "u1"
		total   = int64(10_000)
		unit    = int64(5_000)
		workers = 16
	)
	f := newFakeStore()
	f.seed(userID, total)
	svc := New(f)

	var (
		wg  sync.WaitGroup
		ids = make([]string, workers)
	)
	start := make(chan struct{})
	for i := range workers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			r, err := svc.Reserve(context.Background(), userID, "req-same", unit)
			if err != nil {
				t.Errorf("幂等预占不应失败：%v", err)
				return
			}
			ids[i] = r.ID
		}(i)
	}
	close(start)
	wg.Wait()

	for i, id := range ids {
		if id == "" {
			t.Fatalf("第 %d 个调用者没有拿到预占", i)
		}
		if id != ids[0] {
			t.Fatalf("幂等预占返回了不同行：%s vs %s", id, ids[0])
		}
	}
	acct := f.snapshot(userID)
	if acct.ReservedAmount != unit {
		t.Fatalf("16 次同 requestID 预占后 reserved=%d，期望 %d（只应预占一次）", acct.ReservedAmount, unit)
	}
	if got := len(f.ledgerOfType(model.LedgerReserve)); got != 1 {
		t.Fatalf("reserve 账本 %d 条，期望 1 条", got)
	}
	if acct.Available() != total-unit {
		t.Fatalf("available=%d，期望 %d", acct.Available(), total-unit)
	}
	checkReconciled(t, f, userID)
}

// ---------------------------------------------------------------------------
// 装配
// ---------------------------------------------------------------------------

func TestServiceWithoutStore(t *testing.T) {
	ctx := context.Background()
	var zero Service
	for name, err := range map[string]error{
		"Reserve":     mustErr(zero.Reserve(ctx, "u", "r", 1)),
		"Settle":      mustErr2(zero.Settle(ctx, model.Reservation{ID: "x", UserID: "u"}, 1, 1, 1)),
		"Release":     zero.Release(ctx, model.Reservation{ID: "x"}, "r"),
		"Adjust":      mustErr2(zero.Adjust(ctx, "u", model.LedgerTopup, 1, "r", "op", "k")),
		"Account":     mustErr2(zero.Account(ctx, "u")),
		"ReapExpired": mustErr2(zero.ReapExpired(ctx, 0, 0)),
	} {
		if !errors.Is(err, ErrUnavailable) {
			t.Errorf("零值 Service 的 %s 应返回 ErrUnavailable，实际 %v", name, err)
		}
	}
	if err := New(nil).Release(ctx, model.Reservation{ID: "x"}, "r"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("New(nil) 应返回 ErrUnavailable，实际 %v", err)
	}
}

func mustErr(_ any, err error) error { return err }

func mustErr2[T any](_ T, err error) error { return err }
