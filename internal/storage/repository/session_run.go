package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/storage"
	"github.com/ximo888ok-netizen/ximo-agent/internal/storage/sqlite"
)

// 会话模式常量。
const SessionModeDefault = "default"

// Session 对应 sessions 表。
type Session struct {
	ID        string          `json:"id"`
	Title     string          `json:"title"`
	Mode      string          `json:"mode"`
	CreatedAt int64           `json:"createdAt"`
	UpdatedAt int64           `json:"updatedAt"`
	Metadata  json.RawMessage `json:"metadata,omitempty"`
}

// SessionRepo 管 sessions 表。
type SessionRepo struct{ db *sqlite.DB }

// Create 插入会话。
func (r *SessionRepo) Create(ctx context.Context, s *Session) error {
	now := time.Now().UnixMilli()
	if s.CreatedAt == 0 {
		s.CreatedAt = now
	}
	s.UpdatedAt = now
	if s.Mode == "" {
		s.Mode = SessionModeDefault
	}
	return r.db.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO sessions (id, title, mode, created_at, updated_at, metadata_json) VALUES (?,?,?,?,?,?)`,
			s.ID, s.Title, s.Mode, s.CreatedAt, s.UpdatedAt, jsonOrNull(s.Metadata))
		return wrapErr("session create", err)
	})
}

// Get 按 ID 取会话。
func (r *SessionRepo) Get(ctx context.Context, id string) (*Session, error) {
	s, err := scanSession(r.db.QueryRowContext(ctx,
		`SELECT id, title, mode, created_at, updated_at, metadata_json FROM sessions WHERE id=?`, id))
	if err != nil {
		return nil, wrapErr("session get", err)
	}
	return s, nil
}

// Update 更新标题/模式/元数据。
func (r *SessionRepo) Update(ctx context.Context, s *Session) error {
	s.UpdatedAt = time.Now().UnixMilli()
	return r.db.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE sessions SET title=?, mode=?, updated_at=?, metadata_json=? WHERE id=?`,
			s.Title, s.Mode, s.UpdatedAt, jsonOrNull(s.Metadata), s.ID)
		return wrapErr("session update", err)
	})
}

// Touch 刷新 updated_at（新消息到达时调用）。
func (r *SessionRepo) Touch(ctx context.Context, id string) error {
	return r.db.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE sessions SET updated_at=? WHERE id=?`, time.Now().UnixMilli(), id)
		return wrapErr("session touch", err)
	})
}

// Delete 删除会话（级联删除其 runs/messages）。
func (r *SessionRepo) Delete(ctx context.Context, id string) error {
	return r.db.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE id=?`, id)
		return wrapErr("session delete", err)
	})
}

// List 按更新时间倒序分页列出会话。
func (r *SessionRepo) List(ctx context.Context, limit, offset int) ([]*Session, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, title, mode, created_at, updated_at, metadata_json FROM sessions
		 ORDER BY updated_at DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, wrapErr("session list", err)
	}
	defer rows.Close()
	var out []*Session
	for rows.Next() {
		s, err := scanSessionRows(rows)
		if err != nil {
			return nil, wrapErr("session list scan", err)
		}
		out = append(out, s)
	}
	return out, wrapErr("session list iterate", rows.Err())
}

func scanSession(row sqlite.Row) (*Session, error) {
	var s Session
	var meta sql.NullString
	err := row.Scan(&s.ID, &s.Title, &s.Mode, &s.CreatedAt, &s.UpdatedAt, &meta)
	if err != nil {
		return nil, err
	}
	if meta.Valid {
		s.Metadata = json.RawMessage(meta.String)
	}
	return &s, nil
}

func scanSessionRows(rows *sql.Rows) (*Session, error) {
	var s Session
	var meta sql.NullString
	err := rows.Scan(&s.ID, &s.Title, &s.Mode, &s.CreatedAt, &s.UpdatedAt, &meta)
	if err != nil {
		return nil, err
	}
	if meta.Valid {
		s.Metadata = json.RawMessage(meta.String)
	}
	return &s, nil
}

// ---------------------------------------------------------------- runs

// Run 状态常量。
const (
	RunStatusPending   = "pending"
	RunStatusRunning   = "running"
	RunStatusCompleted = "completed"
	RunStatusFailed    = "failed"
	RunStatusCancelled = "cancelled"
)

// Run 对应 runs 表。
type Run struct {
	ID             string          `json:"id"`
	SessionID      string          `json:"sessionId"`
	Status         string          `json:"status"`
	Prompt         string          `json:"prompt,omitempty"`
	Model          string          `json:"model,omitempty"`
	CreatedAt      int64           `json:"createdAt"`
	UpdatedAt      int64           `json:"updatedAt"`
	StartedAt      sql.NullInt64   `json:"-"`
	FinishedAt     sql.NullInt64   `json:"-"`
	LastSeq        uint64          `json:"lastSeq"`
	Error          string          `json:"error,omitempty"`
	IdempotencyKey string          `json:"-"`
	WorkerID       string          `json:"workerId,omitempty"`
	Metadata       json.RawMessage `json:"metadata,omitempty"`
}

// RunRepo 管 runs 表，并承载跨表复合事务（CompleteToolCall）。
type RunRepo struct {
	db      *sqlite.DB
	store   *storage.Store
	results *ToolResultRepo
	calls   *ToolCallRepo
}

// Create 插入 run。idempotencyKey 非空时受 UNIQUE 约束保护：
// 同一幂等键重复创建返回 created=false（I5：同一幂等键不产生两个副作用）。
func (r *RunRepo) Create(ctx context.Context, run *Run) (created bool, err error) {
	now := time.Now().UnixMilli()
	if run.CreatedAt == 0 {
		run.CreatedAt = now
	}
	run.UpdatedAt = now
	if run.Status == "" {
		run.Status = RunStatusPending
	}
	err = r.db.WithTx(ctx, func(tx *sql.Tx) error {
		_, e := tx.ExecContext(ctx,
			`INSERT INTO runs (id, session_id, status, prompt, model, created_at, updated_at,
			                   started_at, finished_at, last_seq, error, idempotency_key, worker_id, metadata_json)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			run.ID, run.SessionID, run.Status, nullStr(run.Prompt), nullStr(run.Model),
			run.CreatedAt, run.UpdatedAt, run.StartedAt, run.FinishedAt, run.LastSeq,
			nullStr(run.Error), nullStr(run.IdempotencyKey), nullStr(run.WorkerID),
			jsonOrNull(run.Metadata))
		if e != nil {
			if storage.IsUniqueViolation(e) {
				created = false
				return nil // 已存在（幂等）
			}
			return wrapErr("run create", e)
		}
		created = true
		return nil
	})
	return created, err
}

// Get 按 ID 取 run。
func (r *RunRepo) Get(ctx context.Context, id string) (*Run, error) {
	run, err := scanRun(r.db.QueryRowContext(ctx,
		`SELECT id, session_id, status, prompt, model, created_at, updated_at, started_at,
		        finished_at, last_seq, error, idempotency_key, worker_id, metadata_json
		 FROM runs WHERE id=?`, id))
	if err != nil {
		return nil, wrapErr("run get", err)
	}
	return run, nil
}

// GetByIdempotencyKey 按幂等键取 run（断线重发/重启恢复场景）。
func (r *RunRepo) GetByIdempotencyKey(ctx context.Context, key string) (*Run, error) {
	if key == "" {
		return nil, wrapErr("run get by idem", storage.ErrRunNotFound)
	}
	run, err := scanRun(r.db.QueryRowContext(ctx,
		`SELECT id, session_id, status, prompt, model, created_at, updated_at, started_at,
		        finished_at, last_seq, error, idempotency_key, worker_id, metadata_json
		 FROM runs WHERE idempotency_key=?`, key))
	if err != nil {
		return nil, wrapErr("run get by idem", err)
	}
	return run, nil
}

// UpdateStatus 更新状态（自动维护 started_at / finished_at）。
func (r *RunRepo) UpdateStatus(ctx context.Context, id, status, runErr string) error {
	now := time.Now().UnixMilli()
	return r.db.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE runs SET status=?, updated_at=?, error=?,
			   started_at=CASE WHEN ?='running' AND started_at IS NULL THEN ? ELSE started_at END,
			   finished_at=CASE WHEN ? IN ('completed','failed','cancelled') THEN ? ELSE finished_at END
			 WHERE id=?`,
			status, now, nullStr(runErr),
			status, now,
			status, now, id)
		return wrapErr("run update status", err)
	})
}

// SetWorker 绑定/解绑 worker。
func (r *RunRepo) SetWorker(ctx context.Context, id, workerID string) error {
	return r.db.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE runs SET worker_id=?, updated_at=? WHERE id=?`, nullStr(workerID), time.Now().UnixMilli(), id)
		return wrapErr("run set worker", err)
	})
}

// ListBySession 列出会话的 run（新→旧）。
func (r *RunRepo) ListBySession(ctx context.Context, sessionID string, limit int) ([]*Run, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, session_id, status, prompt, model, created_at, updated_at, started_at,
		        finished_at, last_seq, error, idempotency_key, worker_id, metadata_json
		 FROM runs WHERE session_id=? ORDER BY created_at DESC LIMIT ?`, sessionID, limit)
	if err != nil {
		return nil, wrapErr("run list by session", err)
	}
	defer rows.Close()
	out, err := scanRuns(rows)
	if err != nil {
		return nil, wrapErr("run list by session", err)
	}
	return out, nil
}

// ListUnfinished 列出未完成（pending/running）的 run——Engine 崩溃恢复与
// checkpoint GC 保留策略的输入。
func (r *RunRepo) ListUnfinished(ctx context.Context, limit int) ([]*Run, error) {
	if limit <= 0 {
		limit = 1000
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, session_id, status, prompt, model, created_at, updated_at, started_at,
		        finished_at, last_seq, error, idempotency_key, worker_id, metadata_json
		 FROM runs WHERE status IN ('pending','running') ORDER BY created_at ASC LIMIT ?`, limit)
	if err != nil {
		return nil, wrapErr("run list unfinished", err)
	}
	defer rows.Close()
	out, err := scanRuns(rows)
	if err != nil {
		return nil, wrapErr("run list unfinished", err)
	}
	return out, nil
}

// ListRecentRunIDs 返回最近 N 个 run 的 ID（GC 保留策略输入）。
func (r *RunRepo) ListRecentRunIDs(ctx context.Context, n int) ([]string, error) {
	if n <= 0 {
		return nil, nil
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT id FROM runs ORDER BY created_at DESC LIMIT ?`, n)
	if err != nil {
		return nil, wrapErr("run list recent", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, wrapErr("run list recent scan", err)
		}
		ids = append(ids, id)
	}
	return ids, wrapErr("run list recent iterate", rows.Err())
}

func scanRun(row sqlite.Row) (*Run, error) {
	var run Run
	var prompt, model, runErr, idem, worker, meta sql.NullString
	err := row.Scan(&run.ID, &run.SessionID, &run.Status, &prompt, &model,
		&run.CreatedAt, &run.UpdatedAt, &run.StartedAt, &run.FinishedAt, &run.LastSeq,
		&runErr, &idem, &worker, &meta)
	if err != nil {
		return nil, err
	}
	run.Prompt = scanNullStr(prompt)
	run.Model = scanNullStr(model)
	run.Error = scanNullStr(runErr)
	run.IdempotencyKey = scanNullStr(idem)
	run.WorkerID = scanNullStr(worker)
	if meta.Valid {
		run.Metadata = json.RawMessage(meta.String)
	}
	return &run, nil
}

func scanRuns(rows *sql.Rows) ([]*Run, error) {
	var out []*Run
	for rows.Next() {
		var run Run
		var prompt, model, runErr, idem, worker, meta sql.NullString
		if err := rows.Scan(&run.ID, &run.SessionID, &run.Status, &prompt, &model,
			&run.CreatedAt, &run.UpdatedAt, &run.StartedAt, &run.FinishedAt, &run.LastSeq,
			&runErr, &idem, &worker, &meta); err != nil {
			return nil, err
		}
		run.Prompt = scanNullStr(prompt)
		run.Model = scanNullStr(model)
		run.Error = scanNullStr(runErr)
		run.IdempotencyKey = scanNullStr(idem)
		run.WorkerID = scanNullStr(worker)
		if meta.Valid {
			run.Metadata = json.RawMessage(meta.String)
		}
		out = append(out, &run)
	}
	return out, rows.Err()
}
