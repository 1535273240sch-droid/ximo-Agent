package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/model"
)

// fund 通过 AdjustTx 给账户充值，保证充值也进账本（对账不变量的前提）。
func fund(t *testing.T, s *Store, userID string, amount int64) model.LedgerEntry {
	t.Helper()
	e, err := s.AdjustTx(context.Background(), userID, model.LedgerTopup, amount,
		"test topup", "admin", "", "led-topup-"+userID, time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("topup %d: %v", amount, err)
	}
	return e
}

func mustAccount(t *testing.T, s *Store, userID string) model.QuotaAccount {
	t.Helper()
	acc, err := s.GetQuotaAccount(context.Background(), userID)
	if err != nil {
		t.Fatalf("GetQuotaAccount(%s): %v", userID, err)
	}
	return acc
}

func TestReserveSettleReleaseRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u := newTestUser(t, s, "u1")
	fund(t, s, u.ID, 100)
	now := time.Now().UnixMilli()

	r, err := s.ReserveTx(ctx, model.Reservation{
		ID: "r1", UserID: u.ID, RequestID: "req-1", Amount: 30,
		ExpiresAt: now + 60_000, CreatedAt: now + 1,
	}, "led-1", now+1)
	if err != nil {
		t.Fatalf("ReserveTx: %v", err)
	}
	if r.Status != model.ReservationHeld || r.Amount != 30 || r.ID != "r1" {
		t.Fatalf("reservation = %+v, want held/30/r1", r)
	}
	acc := mustAccount(t, s, u.ID)
	if acc.TotalAmount != 100 || acc.ReservedAmount != 30 || acc.Available() != 70 {
		t.Fatalf("account after reserve = %+v, want total=100 reserved=30 available=70", acc)
	}
	if acc.Version != 2 { // topup + reserve
		t.Errorf("version = %d, want 2", acc.Version)
	}

	if err := s.SettleTx(ctx, "r1", 12, "led-2", "req-1", now+1); err != nil {
		t.Fatalf("SettleTx: %v", err)
	}
	acc = mustAccount(t, s, u.ID)
	if acc.UsedAmount != 12 || acc.ReservedAmount != 0 || acc.Available() != 88 {
		t.Fatalf("account after settle = %+v, want used=12 reserved=0 available=88", acc)
	}
	// 重复结算幂等：不再扣减。
	if err := s.SettleTx(ctx, "r1", 12, "led-dup", "req-1", now+2); err != nil {
		t.Fatalf("SettleTx(again): %v", err)
	}
	if acc = mustAccount(t, s, u.ID); acc.UsedAmount != 12 || acc.Version != 3 {
		t.Errorf("重复结算改动了账户: %+v", acc)
	}
	// 已结算的预占不能再释放。
	if err := s.ReleaseTx(ctx, "r1", "led-3", now+3); !errors.Is(err, model.ErrConflict) {
		t.Errorf("ReleaseTx(settled) err = %v, want model.ErrConflict", err)
	}
	// 结算后同 request_id 不能重新预占（避免 traceability 丢失）。
	if _, err := s.ReserveTx(ctx, model.Reservation{
		ID: "r1b", UserID: u.ID, RequestID: "req-1", Amount: 5,
	}, "led-3b", now+4); !errors.Is(err, model.ErrConflict) {
		t.Errorf("re-reserve settled request err = %v, want model.ErrConflict", err)
	}

	// release 路径。
	r2, err := s.ReserveTx(ctx, model.Reservation{
		ID: "r2", UserID: u.ID, RequestID: "req-2", Amount: 40,
	}, "led-4", now+5)
	if err != nil {
		t.Fatalf("ReserveTx(2): %v", err)
	}
	if err := s.ReleaseTx(ctx, r2.ID, "led-5", now+6); err != nil {
		t.Fatalf("ReleaseTx: %v", err)
	}
	if acc = mustAccount(t, s, u.ID); acc.ReservedAmount != 0 || acc.Available() != 88 {
		t.Errorf("account after release = %+v, want reserved=0 available=88", acc)
	}
	if err := s.ReleaseTx(ctx, r2.ID, "led-dup2", now+7); err != nil {
		t.Errorf("ReleaseTx(again) err = %v, want nil (幂等)", err)
	}

	// 账本逐行语义：reserve -30 / settle +(30-12) / release +40。
	entries, err := s.ListLedger(ctx, u.ID, 0, 0)
	if err != nil {
		t.Fatalf("ListLedger: %v", err)
	}
	wantAmounts := []int64{100, -30, 18, -40, 40}
	if len(entries) != len(wantAmounts) {
		t.Fatalf("ledger = %d 行, want %d: %+v", len(entries), len(wantAmounts), entries)
	}
	for i, want := range wantAmounts {
		if entries[i].Amount != want {
			t.Errorf("ledger[%d] type=%s amount=%d, want %d", i, entries[i].Type, entries[i].Amount, want)
		}
	}
	sum, err := s.SumLedgerAmount(ctx, u.ID)
	if err != nil {
		t.Fatalf("SumLedgerAmount: %v", err)
	}
	if sum != acc.Available() {
		t.Errorf("SumLedgerAmount = %d, 账户 Available = %d, 对账不平", sum, acc.Available())
	}
	if err := s.SettleTx(ctx, "no-such-reservation", 1, "led-x", "", now); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("SettleTx(missing) err = %v, want model.ErrNotFound", err)
	}
	if err := s.ReleaseTx(ctx, "no-such-reservation", "led-y", now); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("ReleaseTx(missing) err = %v, want model.ErrNotFound", err)
	}
}

func TestReserveInsufficientQuotaAndIdempotency(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u := newTestUser(t, s, "u1")
	now := time.Now().UnixMilli()
	fund(t, s, u.ID, 100)

	_, err := s.ReserveTx(ctx, model.Reservation{
		ID: "r-over", UserID: u.ID, RequestID: "req-over", Amount: 101,
	}, "led-over", now)
	if !errors.Is(err, model.ErrInsufficientQuota) {
		t.Fatalf("超额预占 err = %v, want model.ErrInsufficientQuota", err)
	}
	acc := mustAccount(t, s, u.ID)
	if acc.ReservedAmount != 0 || acc.Version != 1 {
		t.Fatalf("失败的预占改动了账户（中间态）: %+v", acc)
	}
	if entries, _ := s.ListLedger(ctx, u.ID, 0, 0); len(entries) != 1 {
		t.Fatalf("失败的预占写入了账本: %d 行", len(entries))
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM gw_quota_reservations WHERE user_id=?`, u.ID); n != 0 {
		t.Fatalf("失败的预占留下了预占行: %d", n)
	}

	// 同 request_id 重复预占：返回既有行，不重复预占。
	first, err := s.ReserveTx(ctx, model.Reservation{
		ID: "r1", UserID: u.ID, RequestID: "req-1", Amount: 60, ExpiresAt: now + 60_000,
	}, "led-1", now)
	if err != nil {
		t.Fatalf("ReserveTx: %v", err)
	}
	again, err := s.ReserveTx(ctx, model.Reservation{
		ID: "r1-dup", UserID: u.ID, RequestID: "req-1", Amount: 60, ExpiresAt: now + 60_000,
	}, "led-1-dup", now)
	if err != nil {
		t.Fatalf("ReserveTx(dup): %v", err)
	}
	if again.ID != first.ID || again.Amount != first.Amount {
		t.Fatalf("重复预占返回 = %+v, want 既有行 %+v", again, first)
	}
	acc = mustAccount(t, s, u.ID)
	if acc.ReservedAmount != 60 || acc.Available() != 40 {
		t.Fatalf("重复预占被计了两次: %+v", acc)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM gw_quota_reservations WHERE user_id=?`, u.ID); n != 1 {
		t.Fatalf("重复预占产生 %d 条预占行, want 1", n)
	}

	// 冻结账户不可预占。
	if _, err := s.AdjustTx(ctx, u.ID, model.LedgerFreeze, 0, "admin freeze", "admin", "freeze-1", "led-freeze", now+1); err != nil {
		t.Fatalf("AdjustTx(freeze): %v", err)
	}
	if _, err := s.ReserveTx(ctx, model.Reservation{
		ID: "r2", UserID: u.ID, RequestID: "req-2", Amount: 1,
	}, "led-2", now+2); !errors.Is(err, model.ErrDisabled) {
		t.Errorf("冻结账户预占 err = %v, want model.ErrDisabled", err)
	}
	if _, err := s.AdjustTx(ctx, u.ID, model.LedgerUnfreeze, 0, "admin unfreeze", "admin", "unfreeze-1", "led-unfreeze", now+3); err != nil {
		t.Fatalf("AdjustTx(unfreeze): %v", err)
	}
	if _, err := s.ReserveTx(ctx, model.Reservation{
		ID: "r3", UserID: u.ID, RequestID: "req-3", Amount: 40,
	}, "led-3", now+4); err != nil {
		t.Fatalf("解冻后预占仍失败: %v", err)
	}
	if _, err := s.ReserveTx(ctx, model.Reservation{
		ID: "r4", UserID: u.ID, RequestID: "req-4", Amount: 1,
	}, "led-4", now+5); !errors.Is(err, model.ErrInsufficientQuota) {
		t.Errorf("可用额度耗尽后 err = %v, want model.ErrInsufficientQuota", err)
	}
	// 账户行不存在的用户（含用户本身不存在）→ 额度不足，而不是「资源不存在」：
	// 契约 §11.4 只允许「模型不存在/未启用」用 404，额度问题一律 402（缺陷 D5）。
	if _, err := s.ReserveTx(ctx, model.Reservation{
		ID: "r5", UserID: "no-such-user", RequestID: "req-5", Amount: 1,
	}, "led-5", now+6); !errors.Is(err, model.ErrInsufficientQuota) {
		t.Errorf("无账户用户预占 err = %v, want model.ErrInsufficientQuota", err)
	}
}

func TestSettleOverHeldIsAccountedAsDebt(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u := newTestUser(t, s, "u1")
	now := time.Now().UnixMilli()
	fund(t, s, u.ID, 100)

	if _, err := s.ReserveTx(ctx, model.Reservation{
		ID: "r1", UserID: u.ID, RequestID: "req-1", Amount: 100,
	}, "led-1", now); err != nil {
		t.Fatalf("ReserveTx: %v", err)
	}
	// 实际上游用量超出预占：全额计入 used，不做拒绝（差额即欠账）。
	if err := s.SettleTx(ctx, "r1", 120, "led-2", "req-1", now+1); err != nil {
		t.Fatalf("SettleTx(over): %v", err)
	}
	acc := mustAccount(t, s, u.ID)
	if acc.UsedAmount != 120 || acc.ReservedAmount != 0 || acc.Available() != -20 {
		t.Fatalf("account = %+v, want used=120 reserved=0 available=-20", acc)
	}
	entries, _ := s.ListLedger(ctx, u.ID, 0, 0)
	if entries[len(entries)-1].Amount != -20 {
		t.Errorf("欠账账本行 amount = %d, want -20 (held-actual)", entries[len(entries)-1].Amount)
	}
	if _, err := s.ReserveTx(ctx, model.Reservation{
		ID: "r2", UserID: u.ID, RequestID: "req-2", Amount: 1,
	}, "led-3", now+2); !errors.Is(err, model.ErrInsufficientQuota) {
		t.Errorf("欠账后预占 err = %v, want model.ErrInsufficientQuota", err)
	}
}

func TestAdjustIdempotentByKey(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u := newTestUser(t, s, "u1")
	now := time.Now().UnixMilli()

	first, err := s.AdjustTx(ctx, u.ID, model.LedgerTopup, 500, "首充", "admin", "topup-key-1", "led-1", now)
	if err != nil {
		t.Fatalf("AdjustTx: %v", err)
	}
	if first.Amount != 500 || first.BalanceAfter != 500 || first.ReservedAfter != 0 || first.OperatorID != "admin" {
		t.Fatalf("ledger entry = %+v", first)
	}
	// 同 idempotency_key 再来（不同 ledgerID、不同金额）：只认第一次。
	second, err := s.AdjustTx(ctx, u.ID, model.LedgerTopup, 500, "重复提交", "admin2", "topup-key-1", "led-1-dup", now+10)
	if err != nil {
		t.Fatalf("AdjustTx(dup): %v", err)
	}
	if second.ID != first.ID || second.Amount != first.Amount {
		t.Fatalf("幂等返回 = %+v, want 既有行 %+v", second, first)
	}
	acc := mustAccount(t, s, u.ID)
	if acc.TotalAmount != 500 || acc.Version != 1 {
		t.Fatalf("重复入账: %+v", acc)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM gw_quota_ledger WHERE idempotency_key=?`, "topup-key-1"); n != 1 {
		t.Fatalf("idempotency_key 落库 %d 行, want 1", n)
	}

	// 不同 key 正常入账。
	if _, err := s.AdjustTx(ctx, u.ID, model.LedgerDebit, 200, "扣减", "admin", "debit-key-1", "led-2", now+20); err != nil {
		t.Fatalf("AdjustTx(debit): %v", err)
	}
	acc = mustAccount(t, s, u.ID)
	if acc.TotalAmount != 300 || acc.Available() != 300 {
		t.Fatalf("after debit = %+v, want total=300", acc)
	}
	// 扣成负可用额被拒，且不入账。
	if _, err := s.AdjustTx(ctx, u.ID, model.LedgerDebit, 301, "超额扣减", "admin", "debit-key-2", "led-3", now+30); !errors.Is(err, model.ErrInsufficientQuota) {
		t.Errorf("超额扣减 err = %v, want model.ErrInsufficientQuota", err)
	}
	if acc = mustAccount(t, s, u.ID); acc.TotalAmount != 300 || acc.Version != 2 {
		t.Fatalf("被拒的扣减改动了账户: %+v", acc)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM gw_quota_ledger WHERE user_id=?`, u.ID); n != 2 {
		t.Fatalf("被拒的扣减写入账本: %d 行", n)
	}

	// 首次入账自动建账户（不存在 → ErrNotFound）。
	if _, err := s.GetQuotaAccount(ctx, "u2"); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("未开户账户 err = %v, want model.ErrNotFound", err)
	}
	if _, err := s.AdjustTx(ctx, "u2", model.LedgerTopup, 10, "开户", "admin", "", "led-4", now+40); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("给不存在的用户充值 err = %v, want model.ErrNotFound（外键拒绝）", err)
	}
}

func TestAdjustFreezeAndValidation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u := newTestUser(t, s, "u1")
	now := time.Now().UnixMilli()
	fund(t, s, u.ID, 50)

	fe, err := s.AdjustTx(ctx, u.ID, model.LedgerFreeze, 0, "风控冻结", "admin", "freeze-key", "led-f", now)
	if err != nil {
		t.Fatalf("AdjustTx(freeze): %v", err)
	}
	if fe.Amount != 0 || fe.Type != model.LedgerFreeze {
		t.Errorf("freeze 账本行 = %+v, want amount=0", fe)
	}
	acc := mustAccount(t, s, u.ID)
	if acc.Status != model.QuotaStatusFrozen || acc.TotalAmount != 50 || acc.Available() != 50 {
		t.Fatalf("freeze 后账户 = %+v, want frozen 且金额不变", acc)
	}
	if _, err := s.AdjustTx(ctx, u.ID, model.LedgerUnfreeze, 0, "解冻", "admin", "unfreeze-key", "led-u", now+1); err != nil {
		t.Fatalf("AdjustTx(unfreeze): %v", err)
	}
	if acc = mustAccount(t, s, u.ID); acc.Status != model.QuotaStatusActive {
		t.Errorf("unfreeze 后 status = %q, want active", acc.Status)
	}

	if _, err := s.AdjustTx(ctx, u.ID, "no-such-kind", 1, "", "admin", "", "led-x", now); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("未知 kind err = %v, want ErrInvalidArgument", err)
	}
	if _, err := s.AdjustTx(ctx, u.ID, model.LedgerTopup, -1, "", "admin", "", "led-x", now); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("负金额 err = %v, want ErrInvalidArgument", err)
	}
	if _, err := s.ReserveTx(ctx, model.Reservation{ID: "", UserID: u.ID, RequestID: "r", Amount: 1}, "led-x", now); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("空 ID err = %v, want ErrInvalidArgument", err)
	}

	// expire 与 debit 同向：从 total 扣减。
	if _, err := s.AdjustTx(ctx, u.ID, model.LedgerExpire, 20, "额度到期", "system", "expire-key", "led-e", now+2); err != nil {
		t.Fatalf("AdjustTx(expire): %v", err)
	}
	if acc = mustAccount(t, s, u.ID); acc.TotalAmount != 30 || acc.Available() != 30 {
		t.Errorf("expire 后账户 = %+v, want total=30", acc)
	}
	sum, err := s.SumLedgerAmount(ctx, u.ID)
	if err != nil {
		t.Fatalf("SumLedgerAmount: %v", err)
	}
	if sum != acc.Available() {
		t.Errorf("SumLedgerAmount = %d, Available = %d", sum, acc.Available())
	}
}

func TestExpireReservations(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u := newTestUser(t, s, "u1")
	now := time.Now().UnixMilli()
	fund(t, s, u.ID, 100)

	// 两条过期、一条仍有效。
	for i, exp := range []int64{now - 2000, now - 1000, now + 60_000} {
		if _, err := s.ReserveTx(ctx, model.Reservation{
			ID: fmt.Sprintf("r%d", i), UserID: u.ID, RequestID: fmt.Sprintf("req-%d", i),
			Amount: 20, ExpiresAt: exp, CreatedAt: now,
		}, fmt.Sprintf("led-%d", i), now); err != nil {
			t.Fatalf("ReserveTx(%d): %v", i, err)
		}
	}
	if acc := mustAccount(t, s, u.ID); acc.ReservedAmount != 60 || acc.Available() != 40 {
		t.Fatalf("account = %+v, want reserved=60 available=40", acc)
	}

	n, err := s.ExpireReservations(ctx, now, 10)
	if err != nil {
		t.Fatalf("ExpireReservations: %v", err)
	}
	if n != 2 {
		t.Fatalf("reaped = %d, want 2", n)
	}
	acc := mustAccount(t, s, u.ID)
	if acc.ReservedAmount != 20 || acc.Available() != 80 {
		t.Fatalf("account after reap = %+v, want reserved=20 available=80", acc)
	}
	if got := countRows(t, s, `SELECT COUNT(*) FROM gw_quota_reservations WHERE user_id=? AND status=?`, u.ID, model.ReservationExpired); got != 2 {
		t.Errorf("expired 行 = %d, want 2", got)
	}
	if got := countRows(t, s, `SELECT COUNT(*) FROM gw_quota_reservations WHERE user_id=? AND status=?`, u.ID, model.ReservationHeld); got != 1 {
		t.Errorf("held 行 = %d, want 1", got)
	}
	// 重复回收幂等，且不重复入账。
	if n, err = s.ExpireReservations(ctx, now, 10); err != nil || n != 0 {
		t.Fatalf("ExpireReservations(again) = %d, %v; want 0, nil", n, err)
	}
	if acc = mustAccount(t, s, u.ID); acc.ReservedAmount != 20 {
		t.Errorf("重复回收改动了账户: %+v", acc)
	}
	// 被回收的预占再 release 视为已完成。
	if err := s.ReleaseTx(ctx, "r0", "led-r0-dup", now+1); err != nil {
		t.Errorf("ReleaseTx(expired) err = %v, want nil", err)
	}
	sum, _ := s.SumLedgerAmount(ctx, u.ID)
	if sum != acc.Available() {
		t.Errorf("SumLedgerAmount = %d, Available = %d", sum, acc.Available())
	}
}

func TestLedgerReplayMatchesSnapshots(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u := newTestUser(t, s, "u1")
	fund(t, s, u.ID, 1000)
	base := time.Now().UnixMilli()

	step := base
	next := func() int64 { step++; return step }
	if _, err := s.ReserveTx(ctx, model.Reservation{
		ID: "r1", UserID: u.ID, RequestID: "req-1", Amount: 300, ExpiresAt: base + 60_000,
	}, "led-1", next()); err != nil {
		t.Fatalf("ReserveTx: %v", err)
	}
	if err := s.SettleTx(ctx, "r1", 250, "led-2", "req-1", next()); err != nil {
		t.Fatalf("SettleTx: %v", err)
	}
	if _, err := s.ReserveTx(ctx, model.Reservation{
		ID: "r2", UserID: u.ID, RequestID: "req-2", Amount: 400,
	}, "led-3", next()); err != nil {
		t.Fatalf("ReserveTx(2): %v", err)
	}
	if err := s.ReleaseTx(ctx, "r2", "led-4", next()); err != nil {
		t.Fatalf("ReleaseTx: %v", err)
	}
	if _, err := s.ReserveTx(ctx, model.Reservation{
		ID: "r3", UserID: u.ID, RequestID: "req-3", Amount: 100, ExpiresAt: base - 1,
	}, "led-5", next()); err != nil {
		t.Fatalf("ReserveTx(3): %v", err)
	}
	// 回收时间用 next()：账本 created_at 严格递增，重放顺序才确定。
	if n, err := s.ExpireReservations(ctx, next(), 10); err != nil || n != 1 {
		t.Fatalf("ExpireReservations = %d, %v; want 1, nil", n, err)
	}
	if _, err := s.AdjustTx(ctx, u.ID, model.LedgerDebit, 100, "扣减", "admin", "debit-key", "led-6", next()); err != nil {
		t.Fatalf("AdjustTx(debit): %v", err)
	}

	entries, err := s.ListLedger(ctx, u.ID, 1000, 0)
	if err != nil {
		t.Fatalf("ListLedger: %v", err)
	}
	if len(entries) < 7 {
		t.Fatalf("ledger = %d 行, want >= 7", len(entries))
	}
	var running int64
	for i, e := range entries {
		running += e.Amount
		available := e.BalanceAfter - e.ReservedAfter
		if available != running {
			t.Fatalf("ledger[%d] %s: balance_after-reserved_after=%d, 重放累计=%d（中间态不可追溯）",
				i, e.Type, available, running)
		}
		if available < 0 {
			t.Fatalf("ledger[%d] %s: available=%d < 0（额度红线）", i, e.Type, available)
		}
		if e.CreatedAt == 0 {
			t.Fatalf("ledger[%d] 缺少 created_at", i)
		}
	}
	acc := mustAccount(t, s, u.ID)
	if running != acc.Available() {
		t.Errorf("重放累计 = %d, 账户 Available = %d", running, acc.Available())
	}
	// 每条账本行对应一次账户更新，version 应与账本行数一致（每步可追溯）。
	if acc.Version != int64(len(entries)) {
		t.Errorf("version = %d, 账本行数 = %d（存在没有流水的账户变更）", acc.Version, len(entries))
	}
}

func TestConcurrentReserveKeepsAvailableNonNegative(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u := newTestUser(t, s, "u-concurrent")
	const (
		total   = 1000
		workers = 200
		each    = 10
	)
	base := time.Now().UnixMilli()
	fund(t, s, u.ID, total)

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		ok       int
		rejected int
		badAcc   []string
	)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			reqID := fmt.Sprintf("req-%03d", i)
			r, err := s.ReserveTx(ctx, model.Reservation{
				ID: fmt.Sprintf("resv-%03d", i), UserID: u.ID, RequestID: reqID,
				Amount: each, ExpiresAt: base + 60_000,
			}, fmt.Sprintf("led-%03d", i), base)

			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
				if r.RequestID != reqID || r.Amount != each || r.Status != model.ReservationHeld {
					badAcc = append(badAcc, fmt.Sprintf("返回行异常: %+v", r))
				}
			case errors.Is(err, model.ErrInsufficientQuota):
				rejected++
			default:
				badAcc = append(badAcc, fmt.Sprintf("意外错误: %v", err))
				return
			}
			// 抢到额度后立刻回读账户：任何时刻 available 都不得为负。
			acc, err := s.GetQuotaAccount(ctx, u.ID)
			if err != nil {
				badAcc = append(badAcc, fmt.Sprintf("回读账户失败: %v", err))
				return
			}
			if acc.Available() < 0 {
				badAcc = append(badAcc, fmt.Sprintf("available=%d < 0", acc.Available()))
			}
			if acc.ReservedAmount > total {
				badAcc = append(badAcc, fmt.Sprintf("reserved=%d > total=%d", acc.ReservedAmount, total))
			}
		}(i)
	}
	wg.Wait()

	if len(badAcc) > 0 {
		t.Fatalf("并发预占出现 %d 个异常，例如: %s", len(badAcc), badAcc[0])
	}
	if want := total / each; ok != want || rejected != workers-want {
		t.Fatalf("成功 %d 次 / 被拒 %d 次, want %d / %d", ok, rejected, want, workers-want)
	}

	acc := mustAccount(t, s, u.ID)
	if acc.ReservedAmount != total || acc.UsedAmount != 0 || acc.Available() != 0 {
		t.Fatalf("最终账户 = %+v, want reserved=%d available=0", acc, total)
	}
	if got := countRows(t, s, `SELECT COUNT(*) FROM gw_quota_reservations WHERE user_id=? AND status=?`, u.ID, model.ReservationHeld); got != total/each {
		t.Errorf("held 预占行 = %d, want %d", got, total/each)
	}
	if got := countRows(t, s, `SELECT COUNT(*) FROM gw_quota_reservations WHERE user_id=?`, u.ID); got != total/each {
		t.Errorf("预占行 = %d, want %d（失败的预占不得留下行）", got, total/each)
	}
	if got := countRows(t, s, `SELECT COUNT(*) FROM gw_quota_ledger WHERE user_id=? AND type=?`, u.ID, model.LedgerReserve); got != total/each {
		t.Errorf("reserve 账本行 = %d, want %d", got, total/each)
	}
	// 任何账本行的 available 都不得为负（DB 侧红线检查，顺序无关）。
	if got := countRows(t, s,
		`SELECT COUNT(*) FROM gw_quota_ledger WHERE user_id=? AND balance_after - reserved_after < 0`, u.ID); got != 0 {
		t.Errorf("有 %d 条账本行的 available < 0", got)
	}
	// 对账：账本累计 == 账户 available；held 预占总额 == reserved_amount。
	sum, err := s.SumLedgerAmount(ctx, u.ID)
	if err != nil {
		t.Fatalf("SumLedgerAmount: %v", err)
	}
	if sum != acc.Available() {
		t.Errorf("SumLedgerAmount = %d, Available = %d（对账不平）", sum, acc.Available())
	}
	var heldSum int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(amount),0) FROM gw_quota_reservations WHERE user_id=? AND status=?`,
		u.ID, model.ReservationHeld).Scan(&heldSum); err != nil {
		t.Fatalf("sum held: %v", err)
	}
	if heldSum != acc.ReservedAmount {
		t.Errorf("held 总额 %d != reserved_amount %d", heldSum, acc.ReservedAmount)
	}
	// 每个 request_id 只对应一条 held 行（幂等红线）。
	if got := countRows(t, s,
		`SELECT COUNT(*) FROM (SELECT user_id, request_id FROM gw_quota_reservations WHERE status='held' GROUP BY user_id, request_id HAVING COUNT(*) > 1)`); got != 0 {
		t.Errorf("同一 (user_id, request_id) 出现 %d 组重复 held 行", got)
	}
}

// TestNewUserHasZeroQuotaAccount 覆盖缺陷 D5：建户即建 0 额度账户，
// 「全新用户首次请求」拿到的是额度不足（402 语义），而不是资源不存在（404 语义）。
func TestNewUserHasZeroQuotaAccount(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u := newTestUser(t, s, "u-new")
	now := time.Now().UnixMilli()

	// 1) 建户后账户行必须已存在且为干净的 0 额度账户。
	acc := mustAccount(t, s, u.ID)
	if acc.TotalAmount != 0 || acc.UsedAmount != 0 || acc.ReservedAmount != 0 ||
		acc.Available() != 0 || acc.Version != 0 || acc.Status != model.QuotaStatusActive {
		t.Fatalf("新建用户的账户 = %+v, want 0/0/0 available=0 version=0 status=active", acc)
	}

	// 2) 未充值用户立即预占 → 额度不足（绝不能是 ErrNotFound）。
	_, err := s.ReserveTx(ctx, model.Reservation{
		ID: "r-fresh", UserID: u.ID, RequestID: "req-fresh", Amount: 1,
	}, "led-fresh", now)
	if !errors.Is(err, model.ErrInsufficientQuota) {
		t.Fatalf("全新用户首次预占 err = %v, want model.ErrInsufficientQuota", err)
	}
	if errors.Is(err, model.ErrNotFound) {
		t.Fatalf("全新用户首次预占被映射成 ErrNotFound（会变成 HTTP 404）: %v", err)
	}
	// 被拒的预占不得留下任何中间态。
	if acc = mustAccount(t, s, u.ID); acc.ReservedAmount != 0 || acc.Version != 0 {
		t.Errorf("被拒的预占改动了账户: %+v", acc)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM gw_quota_reservations WHERE user_id=?`, u.ID); n != 0 {
		t.Errorf("被拒的预占留下预占行: %d", n)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM gw_quota_ledger WHERE user_id=?`, u.ID); n != 0 {
		t.Errorf("被拒的预占写入账本: %d 行", n)
	}

	// 3) 充值后同一用户预占成功，账户与账本口径一致。
	fund(t, s, u.ID, 10)
	r, err := s.ReserveTx(ctx, model.Reservation{
		ID: "r-paid", UserID: u.ID, RequestID: "req-paid", Amount: 10,
	}, "led-paid", now+1)
	if err != nil {
		t.Fatalf("充值后预占失败: %v", err)
	}
	if r.Status != model.ReservationHeld || r.Amount != 10 {
		t.Fatalf("预占行 = %+v, want held/10", r)
	}
	if acc = mustAccount(t, s, u.ID); acc.Available() != 0 || acc.ReservedAmount != 10 {
		t.Errorf("充值后预占的账户 = %+v, want available=0 reserved=10", acc)
	}

	// 4) 用户 ID 本身不存在（没有账户行）→ 同样是额度不足，不泄露用户是否存在。
	if _, err := s.ReserveTx(ctx, model.Reservation{
		ID: "r-ghost", UserID: "no-such-user", RequestID: "req-ghost", Amount: 1,
	}, "led-ghost", now+2); !errors.Is(err, model.ErrInsufficientQuota) {
		t.Errorf("不存在的用户 ID 预占 err = %v, want model.ErrInsufficientQuota", err)
	}

	// 5) 历史数据（用户存在但账户行被删）也必须落到额度不足，而不是 404。
	if err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM gw_quota_accounts WHERE user_id=?`, u.ID)
		return err
	}); err != nil {
		t.Fatalf("删除账户行（模拟历史数据）: %v", err)
	}
	if _, err := s.ReserveTx(ctx, model.Reservation{
		ID: "r-legacy", UserID: u.ID, RequestID: "req-legacy", Amount: 1,
	}, "led-legacy", now+3); !errors.Is(err, model.ErrInsufficientQuota) {
		t.Errorf("无账户行的存量用户预占 err = %v, want model.ErrInsufficientQuota", err)
	}

	// 6) 重复建户仍因 username 唯一而失败（回滚掉第二个账户行）。
	dup := u
	dup.ID = "u-new-dup"
	if err := s.CreateUser(ctx, dup); !errors.Is(err, model.ErrConflict) {
		t.Fatalf("重复用户名的 CreateUser err = %v, want model.ErrConflict", err)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM gw_quota_accounts WHERE user_id=?`, "u-new-dup"); n != 0 {
		t.Errorf("失败的建户留下了孤儿账户行: %d", n)
	}
}
