package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/model"
)

// ---------------------------------------------------------------- 列定义与扫描

const quotaAccountColumns = `user_id, total_amount, used_amount, reserved_amount, version, status, updated_at`

func scanQuotaAccount(sc scanner) (model.QuotaAccount, error) {
	var a model.QuotaAccount
	err := sc.Scan(&a.UserID, &a.TotalAmount, &a.UsedAmount, &a.ReservedAmount,
		&a.Version, &a.Status, &a.UpdatedAt)
	return a, err
}

// GetQuotaAccount 读账户快照；不存在返回 model.ErrNotFound。
func (s *Store) GetQuotaAccount(ctx context.Context, userID string) (model.QuotaAccount, error) {
	a, err := scanQuotaAccount(s.db.QueryRowContext(ctx,
		`SELECT `+quotaAccountColumns+` FROM gw_quota_accounts WHERE user_id=?`, userID))
	if err != nil {
		return model.QuotaAccount{}, wrapNotFound("get quota account", err)
	}
	return a, nil
}

const reservationColumns = `id, user_id, request_id, status, amount, settled_amount, expires_at, created_at, updated_at`

func scanReservation(sc scanner) (model.Reservation, error) {
	var r model.Reservation
	err := sc.Scan(&r.ID, &r.UserID, &r.RequestID, &r.Status, &r.Amount,
		&r.SettledAmount, &r.ExpiresAt, &r.CreatedAt, &r.UpdatedAt)
	return r, err
}

// getReservationTx 在写事务内按主键读预占行。
func getReservationTx(ctx context.Context, tx *sql.Tx, id string) (model.Reservation, error) {
	r, err := scanReservation(tx.QueryRowContext(ctx,
		`SELECT `+reservationColumns+` FROM gw_quota_reservations WHERE id=?`, id))
	if err != nil {
		return model.Reservation{}, wrapNotFound("get reservation", err)
	}
	return r, nil
}

const ledgerColumns = `id, user_id, type, request_id, idempotency_key, operator_id,
	reason, amount, balance_after, reserved_after, created_at`

func scanLedger(sc scanner) (model.LedgerEntry, error) {
	var e model.LedgerEntry
	var idem sql.NullString
	err := sc.Scan(&e.ID, &e.UserID, &e.Type, &e.RequestID, &idem, &e.OperatorID,
		&e.Reason, &e.Amount, &e.BalanceAfter, &e.ReservedAfter, &e.CreatedAt)
	e.IdempotencyKey = scanNullStr(idem)
	return e, err
}

func insertLedger(ctx context.Context, tx *sql.Tx, e model.LedgerEntry) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO gw_quota_ledger (`+ledgerColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		e.ID, e.UserID, e.Type, e.RequestID, nullStr(e.IdempotencyKey), e.OperatorID,
		e.Reason, e.Amount, e.BalanceAfter, e.ReservedAfter, e.CreatedAt)
	return wrap("insert ledger", err)
}

// updateAccountTx 写回账户快照，version 在 SQL 侧自增（调用方无需维护）。
func updateAccountTx(ctx context.Context, tx *sql.Tx, a model.QuotaAccount, now int64) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE gw_quota_accounts SET total_amount=?, used_amount=?, reserved_amount=?,
		   version=version+1, status=?, updated_at=? WHERE user_id=?`,
		a.TotalAmount, a.UsedAmount, a.ReservedAmount, a.Status, now, a.UserID)
	return wrap("update quota account", err)
}

func ledgerEntry(id, userID, kind, requestID, operatorID, reason string, amount, balanceAfter, reservedAfter, at int64) model.LedgerEntry {
	return model.LedgerEntry{
		ID: id, UserID: userID, Type: kind, RequestID: requestID,
		OperatorID: operatorID, Reason: reason, Amount: amount,
		BalanceAfter: balanceAfter, ReservedAfter: reservedAfter, CreatedAt: at,
	}
}

// ---------------------------------------------------------------- ReserveTx

// ReserveTx 在一个写事务内完成额度预占：
// 幂等检查（同 user_id+request_id 已有 held 行则原样返回）→ 读账户快照 →
// 校验 status 与 available → reserved += amount、version += 1 → 插预占行 →
// 插 ledger(type=reserve, amount=-amount)。任一环节失败整体回滚，
// 不会出现“扣了额度却没有预占行/流水”的中间态（文档 §22 额度红线）。
//
// 错误语义：余额不足 model.ErrInsufficientQuota；账户冻结 model.ErrDisabled；
// 账户行不存在同样返回 model.ErrInsufficientQuota（对调用方来说「没有可用额度」
// 是唯一有意义的语义，账户不存在与余额为 0 在 HTTP 层都是 402，契约 §11.4 /
// 缺陷 D5；用户 ID 不存在也走这条路径，不向调用方泄露用户是否存在）；
// 同 request_id 已有非 held 行（已结算/释放/过期）model.ErrConflict——
// 避免把已完成的请求当成一次新的预占。
func (s *Store) ReserveTx(ctx context.Context, r model.Reservation, ledgerID string, now int64) (model.Reservation, error) {
	if r.ID == "" || r.UserID == "" || r.RequestID == "" || ledgerID == "" {
		return model.Reservation{}, fmt.Errorf("%w: reserve requires id, user_id, request_id and ledger_id",
			ErrInvalidArgument)
	}
	if r.Amount < 0 {
		return model.Reservation{}, fmt.Errorf("%w: reserve amount %d must be >= 0", ErrInvalidArgument, r.Amount)
	}
	if r.Status != "" && r.Status != model.ReservationHeld {
		return model.Reservation{}, fmt.Errorf("%w: reserve status must be %q", ErrInvalidArgument, model.ReservationHeld)
	}
	r.Status = model.ReservationHeld
	r.SettledAmount = 0
	if r.CreatedAt == 0 {
		r.CreatedAt = now
	}
	if r.UpdatedAt == 0 {
		r.UpdatedAt = now
	}

	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		prev, err := scanReservation(tx.QueryRowContext(ctx,
			`SELECT `+reservationColumns+` FROM gw_quota_reservations
			 WHERE user_id=? AND request_id=? ORDER BY created_at DESC, id DESC LIMIT 1`,
			r.UserID, r.RequestID))
		switch {
		case err == nil:
			if prev.Status != model.ReservationHeld {
				return fmt.Errorf("%w: reservation for request %s already %s",
					model.ErrConflict, r.RequestID, prev.Status)
			}
			r = prev // 幂等：既有 held 预占原样返回，不重复预占、不重复入账
			return nil
		case !isNoRows(err):
			return wrap("reserve: read existing reservation", err)
		}

		acc, err := scanQuotaAccount(tx.QueryRowContext(ctx,
			`SELECT `+quotaAccountColumns+` FROM gw_quota_accounts WHERE user_id=?`, r.UserID))
		if isNoRows(err) {
			// 账户行不存在（历史数据 / 未建户 / 用户 ID 本身不存在）：语义上等价
			// 于「可用额度为 0」，归一为额度不足 → HTTP 402，而不是 404 资源不存在
			// （契约 §11.4 / 缺陷 D5）。建户时已建账户行，这条只是纵深防御。
			return fmt.Errorf("%w: user=%s has no quota account (available=0)",
				model.ErrInsufficientQuota, r.UserID)
		}
		if err != nil {
			return wrap("reserve: quota account", err)
		}
		if acc.Status != model.QuotaStatusActive {
			return fmt.Errorf("%w: quota account %s is %s", model.ErrDisabled, acc.UserID, acc.Status)
		}
		if acc.Available() < r.Amount {
			return fmt.Errorf("%w: user=%s available=%d want=%d",
				model.ErrInsufficientQuota, acc.UserID, acc.Available(), r.Amount)
		}

		if _, err := tx.ExecContext(ctx,
			`INSERT INTO gw_quota_reservations (`+reservationColumns+`) VALUES (?,?,?,?,?,?,?,?,?)`,
			r.ID, r.UserID, r.RequestID, r.Status, r.Amount, 0,
			r.ExpiresAt, r.CreatedAt, r.UpdatedAt); err != nil {
			return wrap("reserve: insert reservation", err)
		}
		acc.ReservedAmount += r.Amount
		if err := updateAccountTx(ctx, tx, acc, now); err != nil {
			return err
		}
		return insertLedger(ctx, tx, ledgerEntry(ledgerID, r.UserID, model.LedgerReserve,
			r.RequestID, "", "request reserve", -r.Amount,
			acc.TotalAmount-acc.UsedAmount, acc.ReservedAmount, now))
	})
	if err != nil {
		return model.Reservation{}, err
	}
	return r, nil
}

// ---------------------------------------------------------------- SettleTx

// SettleTx 结算预占：reserved -= held、used += actual（actual > held 时按
// actual 全额计入，差额即欠账，不做拒绝），预占行置 settled，并补
// ledger(type=settle, amount=held-actual)——即“释放预占 + 计入实际消耗”的净额。
// 重复结算（已 settled）返回 nil；released/expired 返回 model.ErrConflict。
func (s *Store) SettleTx(ctx context.Context, reservationID string, actual int64, ledgerID, requestID string, now int64) error {
	if reservationID == "" || ledgerID == "" {
		return fmt.Errorf("%w: settle requires reservation_id and ledger_id", ErrInvalidArgument)
	}
	if actual < 0 {
		return fmt.Errorf("%w: settle actual %d must be >= 0", ErrInvalidArgument, actual)
	}
	return s.db.WithTx(ctx, func(tx *sql.Tx) error {
		r, err := getReservationTx(ctx, tx, reservationID)
		if err != nil {
			return err
		}
		switch r.Status {
		case model.ReservationSettled:
			return nil // 幂等：不重复扣减
		case model.ReservationHeld:
			// 正常路径。
		default:
			return fmt.Errorf("%w: reservation %s is %s", model.ErrConflict, r.ID, r.Status)
		}

		acc, err := scanQuotaAccount(tx.QueryRowContext(ctx,
			`SELECT `+quotaAccountColumns+` FROM gw_quota_accounts WHERE user_id=?`, r.UserID))
		if err != nil {
			return wrapNotFound("settle: quota account", err)
		}
		if acc.ReservedAmount < r.Amount {
			return fmt.Errorf("%w: account %s reserved=%d < held=%d",
				model.ErrConflict, acc.UserID, acc.ReservedAmount, r.Amount)
		}
		acc.ReservedAmount -= r.Amount
		acc.UsedAmount += actual
		if err := updateAccountTx(ctx, tx, acc, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE gw_quota_reservations SET status=?, settled_amount=?, updated_at=? WHERE id=?`,
			model.ReservationSettled, actual, now, r.ID); err != nil {
			return wrap("settle: update reservation", err)
		}
		if requestID == "" {
			requestID = r.RequestID
		}
		return insertLedger(ctx, tx, ledgerEntry(ledgerID, r.UserID, model.LedgerSettle,
			requestID, "", "request settle", r.Amount-actual,
			acc.TotalAmount-acc.UsedAmount, acc.ReservedAmount, now))
	})
}

// ---------------------------------------------------------------- ReleaseTx

// ReleaseTx 释放预占：reserved -= held，预占行置 released，补
// ledger(type=release, amount=+held)。已 released/expired（含被回收）重复
// 调用返回 nil；已 settled 返回 model.ErrConflict。
func (s *Store) ReleaseTx(ctx context.Context, reservationID string, ledgerID string, now int64) error {
	if reservationID == "" || ledgerID == "" {
		return fmt.Errorf("%w: release requires reservation_id and ledger_id", ErrInvalidArgument)
	}
	return s.db.WithTx(ctx, func(tx *sql.Tx) error {
		r, err := getReservationTx(ctx, tx, reservationID)
		if err != nil {
			return err
		}
		switch r.Status {
		case model.ReservationReleased, model.ReservationExpired:
			return nil
		case model.ReservationHeld:
			// 正常路径。
		default:
			return fmt.Errorf("%w: reservation %s is %s", model.ErrConflict, r.ID, r.Status)
		}
		acc, err := scanQuotaAccount(tx.QueryRowContext(ctx,
			`SELECT `+quotaAccountColumns+` FROM gw_quota_accounts WHERE user_id=?`, r.UserID))
		if err != nil {
			return wrapNotFound("release: quota account", err)
		}
		if acc.ReservedAmount < r.Amount {
			return fmt.Errorf("%w: account %s reserved=%d < held=%d",
				model.ErrConflict, acc.UserID, acc.ReservedAmount, r.Amount)
		}
		acc.ReservedAmount -= r.Amount
		if err := updateAccountTx(ctx, tx, acc, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE gw_quota_reservations SET status=?, updated_at=? WHERE id=?`,
			model.ReservationReleased, now, r.ID); err != nil {
			return wrap("release: update reservation", err)
		}
		return insertLedger(ctx, tx, ledgerEntry(ledgerID, r.UserID, model.LedgerRelease,
			r.RequestID, "", "request release", r.Amount,
			acc.TotalAmount-acc.UsedAmount, acc.ReservedAmount, now))
	})
}

// ---------------------------------------------------------------- AdjustTx

// adjustDelta 把账本类型映射为对 total_amount 的增量与目标状态。
// freeze / unfreeze 只改账户状态，金额增量为 0（amount 参数被忽略）。
func adjustDelta(kind string, amount int64) (delta int64, status string, err error) {
	switch kind {
	case model.LedgerTopup:
		return amount, "", nil
	case model.LedgerDebit, model.LedgerExpire:
		return -amount, "", nil
	case model.LedgerFreeze:
		return 0, model.QuotaStatusFrozen, nil
	case model.LedgerUnfreeze:
		return 0, model.QuotaStatusActive, nil
	default:
		return 0, "", fmt.Errorf("%w: unknown ledger kind %q", ErrInvalidArgument, kind)
	}
}

// AdjustTx 是管理员/回收任务对额度的调整入口：
// topup→total += amount；debit / expire→total -= amount（会扣成负可用额时
// 返回 model.ErrInsufficientQuota）；freeze / unfreeze→只切账户状态。
// 全程单事务，并写一条带 balance_after / reserved_after 的账本行。
//
// 幂等：idempotencyKey 非空时先按唯一的 idempotency_key 查已有流水，命中即
// 返回既有行、不再入账；插入时再以唯一约束兜底回读（§7.2 / §17）。
func (s *Store) AdjustTx(ctx context.Context, userID string, kind string, amount int64, reason, operatorID, idempotencyKey, ledgerID string, now int64) (model.LedgerEntry, error) {
	if userID == "" || ledgerID == "" {
		return model.LedgerEntry{}, fmt.Errorf("%w: adjust requires user_id and ledger_id", ErrInvalidArgument)
	}
	if amount < 0 {
		return model.LedgerEntry{}, fmt.Errorf("%w: adjust amount %d must be >= 0", ErrInvalidArgument, amount)
	}
	delta, status, err := adjustDelta(kind, amount)
	if err != nil {
		return model.LedgerEntry{}, err
	}
	if now <= 0 {
		now = nowMS()
	}

	var out model.LedgerEntry
	err = s.db.WithTx(ctx, func(tx *sql.Tx) error {
		if idempotencyKey != "" {
			prev, err := scanLedger(tx.QueryRowContext(ctx,
				`SELECT `+ledgerColumns+` FROM gw_quota_ledger WHERE idempotency_key=?`, idempotencyKey))
			if err == nil {
				out = prev
				return nil
			}
			if !isNoRows(err) {
				return wrap("adjust: read idempotent ledger", err)
			}
		}

		acc, err := scanQuotaAccount(tx.QueryRowContext(ctx,
			`SELECT `+quotaAccountColumns+` FROM gw_quota_accounts WHERE user_id=?`, userID))
		if isNoRows(err) {
			// 首次入账（例如先充值再开户）：账户不存在则建一行。
			// user 必须已存在，外键失败即视为用户不存在——不留无主账户。
			acc = model.QuotaAccount{UserID: userID, Status: model.QuotaStatusActive}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO gw_quota_accounts (`+quotaAccountColumns+`) VALUES (?,?,?,?,?,?,?)`,
				acc.UserID, 0, 0, 0, 0, acc.Status, now); err != nil {
				if isForeignKeyViolation(err) {
					return fmt.Errorf("%w: user %s", model.ErrNotFound, userID)
				}
				return wrap("adjust: create quota account", err)
			}
		} else if err != nil {
			return wrap("adjust: quota account", err)
		}

		if delta < 0 {
			if acc.Available()+delta < 0 {
				return fmt.Errorf("%w: user=%s available=%d want=%d",
					model.ErrInsufficientQuota, userID, acc.Available(), -delta)
			}
		}
		acc.TotalAmount += delta
		if status != "" {
			acc.Status = status
		}
		if err := updateAccountTx(ctx, tx, acc, now); err != nil {
			return err
		}

		e := ledgerEntry(ledgerID, userID, kind, "", operatorID, reason, delta,
			acc.TotalAmount-acc.UsedAmount, acc.ReservedAmount, now)
		e.IdempotencyKey = idempotencyKey
		if err := insertLedger(ctx, tx, e); err != nil {
			if idempotencyKey != "" && isUniqueViolation(err) {
				prev, rerr := scanLedger(tx.QueryRowContext(ctx,
					`SELECT `+ledgerColumns+` FROM gw_quota_ledger WHERE idempotency_key=?`, idempotencyKey))
				if rerr == nil {
					out = prev
					return nil
				}
			}
			return err
		}
		out = e
		return nil
	})
	if err != nil {
		return model.LedgerEntry{}, err
	}
	return out, nil
}

// ---------------------------------------------------------------- 账本查询与回收

// ListLedger 按时间正序分页列出某用户的账本（对账用）。
func (s *Store) ListLedger(ctx context.Context, userID string, limit, offset int) ([]model.LedgerEntry, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+ledgerColumns+` FROM gw_quota_ledger WHERE user_id=?
		 ORDER BY created_at ASC, id ASC LIMIT ? OFFSET ?`,
		userID, pageLimit(limit), pageOffset(offset))
	if err != nil {
		return nil, wrap("list ledger", err)
	}
	return collect(rows, scanLedger, "list ledger")
}

// SumLedgerAmount 返回账本累计额（对账 / 一致性测试用）。见包注释的对账
// 不变量：只要充值也经由 AdjustTx 入账，它应恒等于账户快照的 Available()。
func (s *Store) SumLedgerAmount(ctx context.Context, userID string) (int64, error) {
	var sum int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(amount),0) FROM gw_quota_ledger WHERE user_id=?`, userID).Scan(&sum); err != nil {
		return 0, wrap("sum ledger amount", err)
	}
	return sum, nil
}

// ExpireReservations 回收超时未结算的 held 预占（expires_at>0 且 <= now）：
// 每条在同一事务内 reserved -= held、预占行置 expired，并补一条
// ledger(type=release)——带 idempotency_key "resv-expire:<id>" 与确定性
// ledger id，重复回收同一行不会二次入账。返回本次回收条数。
func (s *Store) ExpireReservations(ctx context.Context, now int64, limit int) (int, error) {
	if now <= 0 {
		now = nowMS()
	}
	reaped := 0
	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx,
			`SELECT `+reservationColumns+` FROM gw_quota_reservations
			 WHERE status=? AND expires_at>0 AND expires_at<=?
			 ORDER BY expires_at ASC, id ASC LIMIT ?`,
			model.ReservationHeld, now, pageLimit(limit))
		if err != nil {
			return wrap("expire reservations: list", err)
		}
		var pend []model.Reservation
		for rows.Next() {
			r, err := scanReservation(rows)
			if err != nil {
				rows.Close()
				return wrap("expire reservations: scan", err)
			}
			pend = append(pend, r)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return wrap("expire reservations: iterate", err)
		}
		// 写连接只有一条：必须先关闭结果集，再执行 UPDATE/INSERT。
		rows.Close()

		for _, r := range pend {
			acc, err := scanQuotaAccount(tx.QueryRowContext(ctx,
				`SELECT `+quotaAccountColumns+` FROM gw_quota_accounts WHERE user_id=?`, r.UserID))
			if err != nil {
				return wrapNotFound("expire reservations: quota account", err)
			}
			if acc.ReservedAmount < r.Amount {
				return fmt.Errorf("%w: account %s reserved=%d < held=%d",
					model.ErrConflict, acc.UserID, acc.ReservedAmount, r.Amount)
			}
			acc.ReservedAmount -= r.Amount
			if err := updateAccountTx(ctx, tx, acc, now); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE gw_quota_reservations SET status=?, updated_at=? WHERE id=? AND status=?`,
				model.ReservationExpired, now, r.ID, model.ReservationHeld); err != nil {
				return wrap("expire reservations: update", err)
			}
			e := ledgerEntry("gwl-expire-"+r.ID, r.UserID, model.LedgerRelease, r.RequestID,
				"", "reservation expired", r.Amount,
				acc.TotalAmount-acc.UsedAmount, acc.ReservedAmount, now)
			e.IdempotencyKey = "resv-expire-" + r.ID
			if err := insertLedger(ctx, tx, e); err != nil {
				return err
			}
			reaped++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return reaped, nil
}
