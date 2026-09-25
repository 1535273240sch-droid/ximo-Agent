package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/storage"
	"github.com/ximo888ok-netizen/ximo-agent/internal/storage/sqlite"
)

// Lease 对应 leases 表。fencing_token 单调递增，用于防止脑裂写入
// （旧 owner 持过期租约继续写会被拒绝）。
type Lease struct {
	Name         string `json:"name"`
	Owner        string `json:"owner"`
	RunID        string `json:"runId,omitempty"`
	AcquiredAt   int64  `json:"acquiredAt"`
	ExpiresAt    int64  `json:"expiresAt"`
	FencingToken int64  `json:"fencingToken"`
}

// ErrLeaseTaken 表示租约已被其他 owner 持有且未过期。
var ErrLeaseTaken = errors.New("repository: lease already held")

// LeaseRepo 管 leases 表。
type LeaseRepo struct{ db *sqlite.DB }

// Acquire 尝试获取租约。已被持有（未过期）时返回 ErrLeaseTaken；
// 过期租约可被抢占（fencing_token 递增，旧持有者的后续写入因 token
// 过期被拒绝）。
//
// Release 不删行而是把 owner 清空、expires_at 归零，因此 fencing_token
// 在“释放→再获取”之后仍然单调递增——延迟到达的旧 owner 写请求会因
// token 过期被拒绝（脑裂防护）。
func (r *LeaseRepo) Acquire(ctx context.Context, name, owner, runID string, ttl time.Duration) (*Lease, error) {
	now := time.Now().UnixMilli()
	expiresAt := now + ttl.Milliseconds()
	var lease *Lease
	err := r.db.WithTx(ctx, func(tx *sql.Tx) error {
		var exp int64
		var heldBy string
		row := tx.QueryRowContext(ctx, "SELECT expires_at, owner FROM leases WHERE name=?", name)
		switch err := row.Scan(&exp, &heldBy); {
		case err == nil:
			if exp > now && heldBy != "" {
				return ErrLeaseTaken
			}
			// 过期或已释放：抢占，fencing_token+1
			if _, err := tx.ExecContext(ctx,
				`UPDATE leases SET owner=?, run_id=?, acquired_at=?, expires_at=?, fencing_token=fencing_token+1
				 WHERE name=?`, owner, nullStr(runID), now, expiresAt, name); err != nil {
				return wrapErr("lease take over", err)
			}
		case errors.Is(err, sql.ErrNoRows):
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO leases (name, owner, run_id, acquired_at, expires_at, fencing_token)
				 VALUES (?,?,?,?,?,1)`, name, owner, nullStr(runID), now, expiresAt); err != nil {
				if storage.IsUniqueViolation(err) {
					return ErrLeaseTaken // 并发插入竞争
				}
				return wrapErr("lease insert", err)
			}
		default:
			return wrapErr("lease read", err)
		}
		var l Lease
		var rid sql.NullString
		row2 := tx.QueryRowContext(ctx,
			"SELECT name, owner, run_id, acquired_at, expires_at, fencing_token FROM leases WHERE name=?", name)
		if err := row2.Scan(&l.Name, &l.Owner, &rid, &l.AcquiredAt, &l.ExpiresAt, &l.FencingToken); err != nil {
			return wrapErr("lease reload", err)
		}
		l.RunID = scanNullStr(rid)
		lease = &l
		return nil
	})
	if err != nil {
		return nil, err
	}
	return lease, nil
}

// Renew 续租（仅 owner 本人可续）。fencing_token 不变。
func (r *LeaseRepo) Renew(ctx context.Context, name, owner string, ttl time.Duration) error {
	expiresAt := time.Now().UnixMilli() + ttl.Milliseconds()
	return r.db.WithTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE leases SET expires_at=? WHERE name=? AND owner=?`, expiresAt, name, owner)
		if err != nil {
			return wrapErr("lease renew", err)
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return ErrLeaseTaken
		}
		return nil
	})
}

// Release 释放租约（仅 owner 本人可释放）。行被保留（owner 清空、
// expires_at 归零）以维持 fencing_token 的单调性；已释放的租约不出现在
// ListExpired 中（owner 为空即无人持有）。
func (r *LeaseRepo) Release(ctx context.Context, name, owner string) error {
	return r.db.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE leases SET owner='', run_id=NULL, expires_at=0 WHERE name=? AND owner=?`,
			name, owner)
		return wrapErr("lease release", err)
	})
}

// Get 取租约（不存在返回 ErrNotFound）。
func (r *LeaseRepo) Get(ctx context.Context, name string) (*Lease, error) {
	var l Lease
	var rid sql.NullString
	row := r.db.QueryRowContext(ctx,
		"SELECT name, owner, run_id, acquired_at, expires_at, fencing_token FROM leases WHERE name=?", name)
	if err := row.Scan(&l.Name, &l.Owner, &rid, &l.AcquiredAt, &l.ExpiresAt, &l.FencingToken); err != nil {
		return nil, wrapErr("lease get", err)
	}
	l.RunID = scanNullStr(rid)
	return &l, nil
}

// ListExpired 列出已过期且仍被持有的租约（Supervisor 回收扫描用）。
// 已释放的租约（owner 为空）不列入。
func (r *LeaseRepo) ListExpired(ctx context.Context, now time.Time, limit int) ([]*Lease, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT name, owner, run_id, acquired_at, expires_at, fencing_token
		 FROM leases WHERE expires_at <= ? AND owner != '' ORDER BY expires_at ASC LIMIT ?`, now.UnixMilli(), limit)
	if err != nil {
		return nil, wrapErr("lease list expired", err)
	}
	defer rows.Close()
	var out []*Lease
	for rows.Next() {
		var l Lease
		var rid sql.NullString
		if err := rows.Scan(&l.Name, &l.Owner, &rid, &l.AcquiredAt, &l.ExpiresAt, &l.FencingToken); err != nil {
			return nil, wrapErr("lease list expired scan", err)
		}
		l.RunID = scanNullStr(rid)
		out = append(out, &l)
	}
	return out, wrapErr("lease list expired iterate", rows.Err())
}

// ---------------------------------------------------------------- kv

// KVRepo 管 kv 表（设置、feature flag、数据迁移版本标记等）。
type KVRepo struct{ db *sqlite.DB }

// Set 写入键值。
func (r *KVRepo) Set(ctx context.Context, key string, value []byte) error {
	return r.db.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO kv (key, value, updated_at) VALUES (?,?,?)
			 ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
			key, value, time.Now().UnixMilli())
		return wrapErr("kv set", err)
	})
}

// Get 读取键值。不存在返回 (nil, nil)。
func (r *KVRepo) Get(ctx context.Context, key string) ([]byte, error) {
	var v []byte
	row := r.db.QueryRowContext(ctx, "SELECT value FROM kv WHERE key=?", key)
	if err := row.Scan(&v); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, wrapErr("kv get", err)
	}
	return v, nil
}

// Delete 删除键。
func (r *KVRepo) Delete(ctx context.Context, key string) error {
	return r.db.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "DELETE FROM kv WHERE key=?", key)
		return wrapErr("kv delete", err)
	})
}

// List 列出全部键值（key 前缀过滤，空前缀=全部）。
func (r *KVRepo) List(ctx context.Context, prefix string, limit int) (map[string][]byte, error) {
	if limit <= 0 {
		limit = 1000
	}
	var rows *sql.Rows
	var err error
	if prefix == "" {
		rows, err = r.db.QueryContext(ctx, "SELECT key, value FROM kv ORDER BY key ASC LIMIT ?", limit)
	} else {
		rows, err = r.db.QueryContext(ctx, "SELECT key, value FROM kv WHERE key LIKE ? ORDER BY key ASC LIMIT ?", prefix+"%", limit)
	}
	if err != nil {
		return nil, wrapErr("kv list", err)
	}
	defer rows.Close()
	out := make(map[string][]byte)
	for rows.Next() {
		var k string
		var v []byte
		if err := rows.Scan(&k, &v); err != nil {
			return nil, wrapErr("kv list scan", err)
		}
		out[k] = v
	}
	return out, wrapErr("kv list iterate", rows.Err())
}
