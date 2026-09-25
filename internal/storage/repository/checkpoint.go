package repository

import (
	"context"
	"database/sql"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/checkpoint"
	"github.com/ximo888ok-netizen/ximo-agent/internal/storage/sqlite"
)

// Checkpoint 对应 checkpoints 表（manifest 的 DB 索引；内容在 CAS 目录）。
type Checkpoint struct {
	ID        string `json:"id"`
	RunID     string `json:"runId"`
	SessionID string `json:"sessionId,omitempty"`
	TurnID    string `json:"turnId"`
	Label     string `json:"label,omitempty"`
	Status    string `json:"status"`
	Pinned    bool   `json:"pinned"`
	BlobCount int64  `json:"blobCount"`
	TotalSize int64  `json:"totalSize"`
	CreatedAt int64  `json:"createdAt"`
}

// checkpoint 状态常量。
const (
	CheckpointActive     = "active"
	CheckpointSuperseded = "superseded"
)

// CheckpointRepo 管 checkpoints / checkpoint_blobs 表，并为
// checkpoint.GC 提供保留策略数据（实现 checkpoint.GCStore）。
type CheckpointRepo struct{ db *sqlite.DB }

// Record 登记一个 manifest（调用方在 CAS+manifest 落盘后调用）。
// 同一 run 的旧 manifest 会被标记为 superseded（当前状态只有一个）。
func (r *CheckpointRepo) Record(ctx context.Context, m *checkpoint.Manifest, pinned bool) error {
	now := time.Now().UnixMilli()
	return r.db.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`UPDATE checkpoints SET status=? WHERE run_id=? AND status=?`,
			CheckpointSuperseded, m.RunID, CheckpointActive); err != nil {
			return wrapErr("checkpoint supersede", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO checkpoints (id, run_id, session_id, turn_id, label, status, pinned, blob_count, total_size, created_at)
			 VALUES (?,?,?,?,?,?,?,?,?,?)`,
			m.ID, m.RunID, nullStr(m.SessionID), m.TurnID, nullStr(m.Label),
			CheckpointActive, boolToInt(pinned), int64(len(m.Files)), m.TotalSize(), m.CreatedAt); err != nil {
			return wrapErr("checkpoint record", err)
		}
		for _, f := range m.Files {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO checkpoint_blobs (manifest_id, path, blob_hash, size, media_type, mode, created_at)
				 VALUES (?,?,?,?,?,?,?)`,
				m.ID, f.Path, checkpoint.HexHash(f.Ref.Hash), f.Ref.Size, f.Ref.MediaType, int64(f.Ref.Mode), now); err != nil {
				return wrapErr("checkpoint blob record", err)
			}
		}
		return nil
	})
}

// Get 取 checkpoint 元数据。
func (r *CheckpointRepo) Get(ctx context.Context, id string) (*Checkpoint, error) {
	var c Checkpoint
	var sessionID, label sql.NullString
	var pinned int64
	row := r.db.QueryRowContext(ctx,
		`SELECT id, run_id, session_id, turn_id, label, status, pinned, blob_count, total_size, created_at
		 FROM checkpoints WHERE id=?`, id)
	if err := row.Scan(&c.ID, &c.RunID, &sessionID, &c.TurnID, &label, &c.Status, &pinned, &c.BlobCount, &c.TotalSize, &c.CreatedAt); err != nil {
		return nil, wrapErr("checkpoint get", err)
	}
	c.SessionID = scanNullStr(sessionID)
	c.Label = scanNullStr(label)
	c.Pinned = pinned != 0
	return &c, nil
}

// ListByRun 列出 run 的 checkpoint（新→旧）。
func (r *CheckpointRepo) ListByRun(ctx context.Context, runID string, limit int) ([]*Checkpoint, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, run_id, session_id, turn_id, label, status, pinned, blob_count, total_size, created_at
		 FROM checkpoints WHERE run_id=? ORDER BY created_at DESC LIMIT ?`, runID, limit)
	if err != nil {
		return nil, wrapErr("checkpoint list", err)
	}
	defer rows.Close()
	var out []*Checkpoint
	for rows.Next() {
		var c Checkpoint
		var sessionID, label sql.NullString
		var pinned int64
		if err := rows.Scan(&c.ID, &c.RunID, &sessionID, &c.TurnID, &label, &c.Status, &pinned, &c.BlobCount, &c.TotalSize, &c.CreatedAt); err != nil {
			return nil, wrapErr("checkpoint list scan", err)
		}
		c.SessionID = scanNullStr(sessionID)
		c.Label = scanNullStr(label)
		c.Pinned = pinned != 0
		out = append(out, &c)
	}
	return out, wrapErr("checkpoint list iterate", rows.Err())
}

// SetPinned 固定/取消固定。
func (r *CheckpointRepo) SetPinned(ctx context.Context, id string, pinned bool) error {
	return r.db.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE checkpoints SET pinned=? WHERE id=?`, boolToInt(pinned), id)
		return wrapErr("checkpoint set pinned", err)
	})
}

// Delete 删除单个 checkpoint 的 DB 登记（级联删 blob 行）。
func (r *CheckpointRepo) Delete(ctx context.Context, id string) error {
	return r.db.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM checkpoints WHERE id=?`, id)
		return wrapErr("checkpoint delete", err)
	})
}

// ---------------------------------------------------------------- GCStore 实现

// ActiveManifestIDs 实现 checkpoint.GCStore（进行中 run 的当前状态）。
func (r *CheckpointRepo) ActiveManifestIDs(ctx context.Context) ([]string, error) {
	return r.queryIDs(ctx, `
		SELECT id FROM checkpoints
		WHERE status=? AND run_id IN (SELECT id FROM runs WHERE status IN ('pending','running'))`,
		CheckpointActive)
}

// PinnedManifestIDs 实现 checkpoint.GCStore。
func (r *CheckpointRepo) PinnedManifestIDs(ctx context.Context) ([]string, error) {
	return r.queryIDs(ctx, `SELECT id FROM checkpoints WHERE pinned=1`)
}

// RecentRunManifestIDs 实现 checkpoint.GCStore。
func (r *CheckpointRepo) RecentRunManifestIDs(ctx context.Context, keepRuns int) ([]string, error) {
	if keepRuns <= 0 {
		return nil, nil
	}
	return r.queryIDs(ctx, `
		SELECT id FROM checkpoints
		WHERE run_id IN (SELECT id FROM runs ORDER BY created_at DESC LIMIT ?)`, keepRuns)
}

// UnfinishedRunManifestIDs 实现 checkpoint.GCStore。
func (r *CheckpointRepo) UnfinishedRunManifestIDs(ctx context.Context) ([]string, error) {
	return r.queryIDs(ctx, `
		SELECT id FROM checkpoints
		WHERE run_id IN (SELECT id FROM runs WHERE status IN ('pending','running'))`)
}

// ManifestsWithinWindow 实现 checkpoint.GCStore。
func (r *CheckpointRepo) ManifestsWithinWindow(ctx context.Context, sinceUnixMS int64) ([]string, error) {
	return r.queryIDs(ctx, `SELECT id FROM checkpoints WHERE created_at >= ?`, sinceUnixMS)
}

// DeleteManifests 实现 checkpoint.GCStore。
func (r *CheckpointRepo) DeleteManifests(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	return r.db.WithTx(ctx, func(tx *sql.Tx) error {
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx, `DELETE FROM checkpoints WHERE id=?`, id); err != nil {
				return wrapErr("checkpoint gc delete", err)
			}
		}
		return nil
	})
}

// ReferencedBlobHashes 实现 checkpoint.GCStore。
func (r *CheckpointRepo) ReferencedBlobHashes(ctx context.Context) ([]string, error) {
	return r.queryIDs(ctx, `SELECT DISTINCT blob_hash FROM checkpoint_blobs`)
}

func (r *CheckpointRepo) queryIDs(ctx context.Context, query string, args ...any) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, wrapErr("checkpoint query ids", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, wrapErr("checkpoint query ids scan", err)
		}
		out = append(out, id)
	}
	return out, wrapErr("checkpoint query ids iterate", rows.Err())
}
