package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/model"
)

// ---------------------------------------------------------------- gw_usage

const usageColumns = `request_id, user_id, model_id, provider_id, status,
	input_tokens, output_tokens, latency_ms, cost_micro, created_at`

func scanUsage(sc scanner) (model.UsageRecord, error) {
	var u model.UsageRecord
	err := sc.Scan(&u.RequestID, &u.UserID, &u.ModelID, &u.ProviderID, &u.Status,
		&u.InputTokens, &u.OutputTokens, &u.LatencyMS, &u.CostMicro, &u.CreatedAt)
	return u, err
}

// InsertUsage 写入用量记录。request_id 是主键，重复写入（重放同一请求）
// 视为成功——不覆盖已有行，也不报错，避免重试路径产生重复计费。
func (s *Store) InsertUsage(ctx context.Context, u model.UsageRecord) error {
	if u.RequestID == "" || u.UserID == "" {
		return fmt.Errorf("%w: insert usage requires request_id and user_id", ErrInvalidArgument)
	}
	if u.CreatedAt == 0 {
		u.CreatedAt = nowMS()
	}
	return s.db.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO gw_usage (`+usageColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?)
			 ON CONFLICT(request_id) DO NOTHING`,
			u.RequestID, u.UserID, u.ModelID, u.ProviderID, u.Status,
			u.InputTokens, u.OutputTokens, u.LatencyMS, u.CostMicro, u.CreatedAt)
		return wrap("insert usage", err)
	})
}

// ListUsage 列出某用户用量（最近优先）。
func (s *Store) ListUsage(ctx context.Context, userID string, limit, offset int) ([]model.UsageRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+usageColumns+` FROM gw_usage WHERE user_id=?
		 ORDER BY created_at DESC, request_id ASC LIMIT ? OFFSET ?`,
		userID, pageLimit(limit), pageOffset(offset))
	if err != nil {
		return nil, wrap("list usage", err)
	}
	return collect(rows, scanUsage, "list usage")
}

// SumUsageCost 汇总某用户在 [since, until] 内的成本与 token 数
// （since<=0 表示不限下界，until<=0 表示不限上界）。
func (s *Store) SumUsageCost(ctx context.Context, userID string, since, until int64) (costMicro int64, inTok, outTok int64, err error) {
	query := `SELECT COALESCE(SUM(cost_micro),0), COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0)
	          FROM gw_usage WHERE user_id=?`
	args := []any{userID}
	if since > 0 {
		query += ` AND created_at >= ?`
		args = append(args, since)
	}
	if until > 0 {
		query += ` AND created_at <= ?`
		args = append(args, until)
	}
	row := s.db.QueryRowContext(ctx, query, args...)
	if err := row.Scan(&costMicro, &inTok, &outTok); err != nil {
		return 0, 0, 0, wrap("sum usage cost", err)
	}
	return costMicro, inTok, outTok, nil
}

// ---------------------------------------------------------------- gw_audit

const auditColumns = `id, actor, action, target, result, ip, detail_json, created_at`

func scanAudit(sc scanner) (model.AuditLog, error) {
	var a model.AuditLog
	err := sc.Scan(&a.ID, &a.Actor, &a.Action, &a.Target, &a.Result, &a.IP, &a.DetailJSON, &a.CreatedAt)
	return a, err
}

// InsertAudit 写一条管理侧审计记录（append-only，不提供更新/删除入口）。
func (s *Store) InsertAudit(ctx context.Context, a model.AuditLog) error {
	if a.ID == "" || a.Action == "" {
		return fmt.Errorf("%w: insert audit requires id and action", ErrInvalidArgument)
	}
	if a.CreatedAt == 0 {
		a.CreatedAt = nowMS()
	}
	return s.db.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO gw_audit (`+auditColumns+`) VALUES (?,?,?,?,?,?,?,?)`,
			a.ID, a.Actor, a.Action, a.Target, a.Result, a.IP, a.DetailJSON, a.CreatedAt)
		return wrap("insert audit", err)
	})
}

// ListAudit 列出审计记录（最近优先）。
func (s *Store) ListAudit(ctx context.Context, limit, offset int) ([]model.AuditLog, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+auditColumns+` FROM gw_audit ORDER BY created_at DESC, id ASC LIMIT ? OFFSET ?`,
		pageLimit(limit), pageOffset(offset))
	if err != nil {
		return nil, wrap("list audit", err)
	}
	return collect(rows, scanAudit, "list audit")
}
