package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/storage"
	"github.com/ximo888ok-netizen/ximo-agent/internal/storage/sqlite"
)

// 消息角色常量。
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleSystem    = "system"
	RoleTool      = "tool"
)

// Message 对应 messages 表。
type Message struct {
	ID         string          `json:"id"`
	RunID      string          `json:"runId"`
	SessionID  string          `json:"sessionId"`
	Role       string          `json:"role"`
	Content    string          `json:"content"`
	Seq        int64           `json:"seq"`
	CreatedAt  int64           `json:"createdAt"`
	TokenCount sql.NullInt64   `json:"-"`
	Metadata   json.RawMessage `json:"metadata,omitempty"`
}

// MessageRepo 管 messages 表。
type MessageRepo struct{ db *sqlite.DB }

// Create 插入消息。
func (r *MessageRepo) Create(ctx context.Context, m *Message) error {
	if m.CreatedAt == 0 {
		m.CreatedAt = time.Now().UnixMilli()
	}
	return r.db.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO messages (id, run_id, session_id, role, content, seq, created_at, token_count, metadata_json)
			 VALUES (?,?,?,?,?,?,?,?,?)`,
			m.ID, m.RunID, m.SessionID, m.Role, m.Content, m.Seq, m.CreatedAt, m.TokenCount, jsonOrNull(m.Metadata))
		return wrapErr("message create", err)
	})
}

// ListByRun 按 seq 升序返回 run 的消息。
func (r *MessageRepo) ListByRun(ctx context.Context, runID string, limit int) ([]*Message, error) {
	if limit <= 0 {
		limit = 1000
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, run_id, session_id, role, content, seq, created_at, token_count, metadata_json
		 FROM messages WHERE run_id=? ORDER BY seq ASC LIMIT ?`, runID, limit)
	if err != nil {
		return nil, wrapErr("message list", err)
	}
	defer rows.Close()
	var out []*Message
	for rows.Next() {
		var m Message
		var meta sql.NullString
		if err := rows.Scan(&m.ID, &m.RunID, &m.SessionID, &m.Role, &m.Content, &m.Seq, &m.CreatedAt, &m.TokenCount, &meta); err != nil {
			return nil, wrapErr("message list scan", err)
		}
		if meta.Valid {
			m.Metadata = json.RawMessage(meta.String)
		}
		out = append(out, &m)
	}
	return out, wrapErr("message list iterate", rows.Err())
}

// ---------------------------------------------------------------- tool_calls

// 工具调用状态常量。
const (
	ToolCallPending   = "pending"
	ToolCallRunning   = "running"
	ToolCallCompleted = "completed"
	ToolCallFailed    = "failed"
	ToolCallCancelled = "cancelled"
)

// ToolCall 对应 tool_calls 表。
type ToolCall struct {
	ID             string          `json:"id"`
	RunID          string          `json:"runId"`
	MessageID      string          `json:"messageId,omitempty"`
	Name           string          `json:"name"`
	Arguments      json.RawMessage `json:"arguments,omitempty"`
	Status         string          `json:"status"`
	IdempotencyKey string          `json:"-"`
	Seq            sql.NullInt64   `json:"-"`
	Attempt        int64           `json:"attempt"`
	WorkerID       string          `json:"workerId,omitempty"`
	Error          string          `json:"error,omitempty"`
	CreatedAt      int64           `json:"createdAt"`
	UpdatedAt      int64           `json:"updatedAt"`
}

// ToolCallRepo 管 tool_calls 表。
type ToolCallRepo struct{ db *sqlite.DB }

// Create 插入 tool call。幂等键唯一：重复创建返回 created=false（已存在）。
func (r *ToolCallRepo) Create(ctx context.Context, tc *ToolCall) (created bool, err error) {
	now := time.Now().UnixMilli()
	if tc.CreatedAt == 0 {
		tc.CreatedAt = now
	}
	tc.UpdatedAt = now
	if tc.Status == "" {
		tc.Status = ToolCallPending
	}
	err = r.db.WithTx(ctx, func(tx *sql.Tx) error {
		_, e := tx.ExecContext(ctx,
			`INSERT INTO tool_calls (id, run_id, message_id, name, arguments_json, status,
			                        idempotency_key, seq, attempt, worker_id, error, created_at, updated_at)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			tc.ID, tc.RunID, nullStr(tc.MessageID), tc.Name, jsonOrNull(tc.Arguments), tc.Status,
			nullStr(tc.IdempotencyKey), tc.Seq, tc.Attempt, nullStr(tc.WorkerID), nullStr(tc.Error),
			tc.CreatedAt, tc.UpdatedAt)
		if e != nil {
			if storage.IsUniqueViolation(e) {
				created = false
				return nil
			}
			return wrapErr("tool_call create", e)
		}
		created = true
		return nil
	})
	return created, err
}

// Get 按 ID 取 tool call。
func (r *ToolCallRepo) Get(ctx context.Context, id string) (*ToolCall, error) {
	tc, err := scanToolCall(r.db.QueryRowContext(ctx,
		`SELECT id, run_id, message_id, name, arguments_json, status, idempotency_key,
		        seq, attempt, worker_id, error, created_at, updated_at FROM tool_calls WHERE id=?`, id))
	if err != nil {
		return nil, wrapErr("tool_call get", err)
	}
	return tc, nil
}

// GetByIdempotencyKey 按幂等键取 tool call（I5：防止同一 key 两次 durable 副作用）。
func (r *ToolCallRepo) GetByIdempotencyKey(ctx context.Context, key string) (*ToolCall, error) {
	if key == "" {
		return nil, wrapErr("tool_call get by idem", storage.ErrRunNotFound)
	}
	tc, err := scanToolCall(r.db.QueryRowContext(ctx,
		`SELECT id, run_id, message_id, name, arguments_json, status, idempotency_key,
		        seq, attempt, worker_id, error, created_at, updated_at FROM tool_calls WHERE idempotency_key=?`, key))
	if err != nil {
		return nil, wrapErr("tool_call get by idem", err)
	}
	return tc, nil
}

// UpdateStatus 更新状态/错误。
func (r *ToolCallRepo) UpdateStatus(ctx context.Context, id, status, runErr string) error {
	return r.db.WithTx(ctx, func(tx *sql.Tx) error {
		return r.updateStatusTx(ctx, tx, id, status, runErr)
	})
}

// UpdateStatusTx 在调用方事务内更新状态（复合事务第9.2章形态）。
func (r *ToolCallRepo) UpdateStatusTx(ctx context.Context, tx storage.Tx, id, status, runErr string) error {
	return r.updateStatusTx(ctx, tx, id, status, runErr)
}

func (r *ToolCallRepo) updateStatusTx(ctx context.Context, tx txExecer, id, status, runErr string) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE tool_calls SET status=?, error=?, updated_at=? WHERE id=?`,
		status, nullStr(runErr), time.Now().UnixMilli(), id)
	return wrapErr("tool_call update status", err)
}

// BumpAttempt 递增尝试次数（worker 崩溃重启后重试）。
func (r *ToolCallRepo) BumpAttempt(ctx context.Context, id string) error {
	return r.db.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE tool_calls SET attempt=attempt+1, updated_at=? WHERE id=?`, time.Now().UnixMilli(), id)
		return wrapErr("tool_call bump attempt", err)
	})
}

// ListByRun 列出 run 的 tool call。
func (r *ToolCallRepo) ListByRun(ctx context.Context, runID string, limit int) ([]*ToolCall, error) {
	if limit <= 0 {
		limit = 1000
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, run_id, message_id, name, arguments_json, status, idempotency_key,
		        seq, attempt, worker_id, error, created_at, updated_at
		 FROM tool_calls WHERE run_id=? ORDER BY created_at ASC LIMIT ?`, runID, limit)
	if err != nil {
		return nil, wrapErr("tool_call list", err)
	}
	defer rows.Close()
	var out []*ToolCall
	for rows.Next() {
		tc, err := scanToolCallRows(rows)
		if err != nil {
			return nil, wrapErr("tool_call list scan", err)
		}
		out = append(out, tc)
	}
	return out, wrapErr("tool_call list iterate", rows.Err())
}

func scanToolCall(row sqlite.Row) (*ToolCall, error) {
	var tc ToolCall
	var msgID, idem, worker, runErr, args sql.NullString
	err := row.Scan(&tc.ID, &tc.RunID, &msgID, &tc.Name, &args, &tc.Status, &idem,
		&tc.Seq, &tc.Attempt, &worker, &runErr, &tc.CreatedAt, &tc.UpdatedAt)
	if err != nil {
		return nil, err
	}
	tc.MessageID = scanNullStr(msgID)
	tc.IdempotencyKey = scanNullStr(idem)
	tc.WorkerID = scanNullStr(worker)
	tc.Error = scanNullStr(runErr)
	if args.Valid {
		tc.Arguments = json.RawMessage(args.String)
	}
	return &tc, nil
}

func scanToolCallRows(rows *sql.Rows) (*ToolCall, error) {
	var tc ToolCall
	var msgID, idem, worker, runErr, args sql.NullString
	err := rows.Scan(&tc.ID, &tc.RunID, &msgID, &tc.Name, &args, &tc.Status, &idem,
		&tc.Seq, &tc.Attempt, &worker, &runErr, &tc.CreatedAt, &tc.UpdatedAt)
	if err != nil {
		return nil, err
	}
	tc.MessageID = scanNullStr(msgID)
	tc.IdempotencyKey = scanNullStr(idem)
	tc.WorkerID = scanNullStr(worker)
	tc.Error = scanNullStr(runErr)
	if args.Valid {
		tc.Arguments = json.RawMessage(args.String)
	}
	return &tc, nil
}

// ---------------------------------------------------------------- tool_results

// ToolResult 对应 tool_results 表（一个 tool call 至多一条，I3）。
type ToolResult struct {
	ID         string          `json:"id"`
	ToolCallID string          `json:"toolCallId"`
	RunID      string          `json:"runId"`
	Output     json.RawMessage `json:"output,omitempty"`
	IsError    bool            `json:"isError"`
	DurationMS int64           `json:"durationMs"`
	Checksum   string          `json:"checksum,omitempty"`
	CreatedAt  int64           `json:"createdAt"`
}

// ToolResultRepo 管 tool_results 表。
type ToolResultRepo struct{ db *sqlite.DB }

// Create 插入结果。tool_call_id 唯一约束保证一个 call 只有一条 durable result。
func (r *ToolResultRepo) Create(ctx context.Context, tr *ToolResult) (created bool, err error) {
	if tr.CreatedAt == 0 {
		tr.CreatedAt = time.Now().UnixMilli()
	}
	err = r.db.WithTx(ctx, func(tx *sql.Tx) error {
		_, e := r.insertResult(ctx, tx, tr)
		if e != nil {
			if storage.IsUniqueViolation(e) {
				created = false
				return nil
			}
			return wrapErr("tool_result create", e)
		}
		created = true
		return nil
	})
	return created, err
}

// CreateTx 在调用方事务内插入结果（复合事务第9.2章形态）。
func (r *ToolResultRepo) CreateTx(ctx context.Context, tx storage.Tx, tr *ToolResult) (created bool, err error) {
	if tr.CreatedAt == 0 {
		tr.CreatedAt = time.Now().UnixMilli()
	}
	_, err = r.insertResult(ctx, tx, tr)
	if err != nil {
		if storage.IsUniqueViolation(err) {
			return false, nil
		}
		return false, wrapErr("tool_result create", err)
	}
	return true, nil
}

func (r *ToolResultRepo) insertResult(ctx context.Context, tx txExecer, tr *ToolResult) (sql.Result, error) {
	isErr := int64(0)
	if tr.IsError {
		isErr = 1
	}
	return tx.ExecContext(ctx,
		`INSERT INTO tool_results (id, tool_call_id, run_id, output, is_error, duration_ms, checksum, created_at)
		 VALUES (?,?,?,?,?,?,?,?)`,
		tr.ID, tr.ToolCallID, tr.RunID, jsonOrNull(tr.Output), isErr, tr.DurationMS, nullStr(tr.Checksum), tr.CreatedAt)
}

// GetByToolCall 取某个 tool call 的结果（不存在返回 ErrNotFound）。
func (r *ToolResultRepo) GetByToolCall(ctx context.Context, toolCallID string) (*ToolResult, error) {
	tr, err := scanToolResult(r.db.QueryRowContext(ctx,
		`SELECT id, tool_call_id, run_id, output, is_error, duration_ms, checksum, created_at
		 FROM tool_results WHERE tool_call_id=?`, toolCallID))
	if err != nil {
		return nil, wrapErr("tool_result get", err)
	}
	return tr, nil
}

// ListByRun 列出 run 的工具结果。
func (r *ToolResultRepo) ListByRun(ctx context.Context, runID string, limit int) ([]*ToolResult, error) {
	if limit <= 0 {
		limit = 1000
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, tool_call_id, run_id, output, is_error, duration_ms, checksum, created_at
		 FROM tool_results WHERE run_id=? ORDER BY created_at ASC LIMIT ?`, runID, limit)
	if err != nil {
		return nil, wrapErr("tool_result list", err)
	}
	defer rows.Close()
	var out []*ToolResult
	for rows.Next() {
		tr, err := scanToolResultRows(rows)
		if err != nil {
			return nil, wrapErr("tool_result list scan", err)
		}
		out = append(out, tr)
	}
	return out, wrapErr("tool_result list iterate", rows.Err())
}

func scanToolResult(row sqlite.Row) (*ToolResult, error) {
	var tr ToolResult
	var isErr int64
	var output, checksum sql.NullString
	err := row.Scan(&tr.ID, &tr.ToolCallID, &tr.RunID, &output, &isErr, &tr.DurationMS, &checksum, &tr.CreatedAt)
	if err != nil {
		return nil, err
	}
	tr.IsError = isErr != 0
	if output.Valid {
		tr.Output = json.RawMessage(output.String)
	}
	tr.Checksum = scanNullStr(checksum)
	return &tr, nil
}

func scanToolResultRows(rows *sql.Rows) (*ToolResult, error) {
	var tr ToolResult
	var isErr int64
	var output, checksum sql.NullString
	err := rows.Scan(&tr.ID, &tr.ToolCallID, &tr.RunID, &output, &isErr, &tr.DurationMS, &checksum, &tr.CreatedAt)
	if err != nil {
		return nil, err
	}
	tr.IsError = isErr != 0
	if output.Valid {
		tr.Output = json.RawMessage(output.String)
	}
	tr.Checksum = scanNullStr(checksum)
	return &tr, nil
}

// ---------------------------------------------------------------- 复合事务

// ToolCompletion 是第9.2章原子事务的输入：工具完成时，
// tool_results + run_events + runs 状态 + outbox 必须一起提交。
type ToolCompletion struct {
	Result      *ToolResult
	EventType   string
	EventBody   json.RawMessage
	SkipOutbox  bool // 跳过 outbox 入队（默认入队）
	FinalizeRun bool // 是否把 run 标记为 completed（单工具 run 场景）
}

// CompleteToolCall 在一个写事务里完成 Tool 调用：
// INSERT tool_results → UPDATE tool_calls(completed) → INSERT run_events →
// （可选）INSERT outbox → （可选）UPDATE runs(completed) → COMMIT。
// 任何一步失败整体回滚，杜绝“工具已完成但 DB 说没完成”的中间态（第9.2章）。
// 重复完成（结果已存在）时幂等返回，不产生第二次副作用（I5）。
func (r *RunRepo) CompleteToolCall(ctx context.Context, tcID string, in ToolCompletion) (seq uint64, eventID string, err error) {
	if in.Result == nil {
		return 0, "", errors.New("repository: nil tool result")
	}
	if in.Result.ToolCallID == "" {
		in.Result.ToolCallID = tcID
	}
	if in.Result.ToolCallID != tcID {
		return 0, "", errors.New("repository: tool result call id mismatch")
	}
	events := storage.NewEventLog(r.db)
	outbox := storage.NewOutbox(r.db)

	err = r.store.WithTx(ctx, func(tx storage.Tx) error {
		// 1) durable result（I3：completed tool call 必须存在 durable result）
		created, err := r.results.CreateTx(ctx, tx, in.Result)
		if err != nil {
			return err
		}
		if !created {
			// 结果已存在（重复完成）：幂等返回，不重复产生副作用（I5）。
			return nil
		}
		// 2) tool_call 状态推进
		if err := r.calls.UpdateStatusTx(ctx, tx, tcID, ToolCallCompleted, ""); err != nil {
			return err
		}
		// 3) event（seq 与 last_seq 同事务推进）
		seq, eventID, err = events.AppendTx(ctx, tx, in.Result.RunID, in.EventType, in.EventBody)
		if err != nil {
			return err
		}
		// 4) outbox（与 event 同事务落库，Engine 崩溃后可 replay）
		if !in.SkipOutbox {
			if err := outbox.Enqueue(ctx, tx, storage.Event{
				EventID:     eventID,
				RunID:       in.Result.RunID,
				Seq:         seq,
				Type:        in.EventType,
				PayloadJSON: in.EventBody,
				CreatedAt:   time.Now().UnixMilli(),
			}); err != nil {
				return err
			}
		}
		// 5) run 收尾（可选）
		if in.FinalizeRun {
			if err := r.finishTx(ctx, tx, in.Result.RunID, RunStatusCompleted, ""); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, "", err
	}
	return seq, eventID, nil
}

// finishTx 在事务内收尾 run（status + finished_at + error）。
func (r *RunRepo) finishTx(ctx context.Context, tx storage.Tx, id, status, runErr string) error {
	now := time.Now().UnixMilli()
	_, err := tx.ExecContext(ctx,
		`UPDATE runs SET status=?, finished_at=?, error=?, updated_at=? WHERE id=?`,
		status, now, nullStr(runErr), now, id)
	return wrapErr("run finish", err)
}
