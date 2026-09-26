// Package store 是 XIMO 中转站（gateway）的数据访问层，对应迁移
// 0003_gateway.sql：账号、令牌会话、设备码、额度账户与账本、模型/Provider
// 目录、用量与审计。
//
// 约定：
//   - 一切写操作走 sqlite.DB.WithTx（单写队列 + immediate 事务），读走连接池；
//   - 额度写操作是原子的：账户快照、预占行、账本行在同一事务内提交，
//     失败整体回滚，不留“已扣额度但没有流水”的中间态（文档 §22 额度红线）；
//   - 账本 append-only。amount 统一记为“对 available 的有符号增量”：
//     reserve 记 -amount、release / expire 回补记 +amount、
//     settle 记 held-actual（释放预占并计入实际消耗的净额）、
//     topup 记 +amount、debit / expire 记 -amount、freeze / unfreeze 记 0。
//     因此只要充值也经由 AdjustTx 入账，就有对账不变量
//     SumLedgerAmount(userID) == 账户快照的 Available()。
//   - 跨包哨兵错误一律用 model.*（§10），判定用 errors.Is。
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/model"
	"github.com/ximo888ok-netizen/ximo-agent/internal/storage"
	"github.com/ximo888ok-netizen/ximo-agent/internal/storage/sqlite"
)

// 哨兵错误。权威定义在 model 包（§10）；这里提供同名别名，兼容 §3 里
// “写在 store 包里”的写法。
var (
	ErrNotFound          = model.ErrNotFound
	ErrInsufficientQuota = model.ErrInsufficientQuota
)

// ErrInvalidArgument 表示调用参数不合法（空 ID、负金额、未知账本类型等）。
// 这类错误是调用方的编程错误，不做静默修正。
var ErrInvalidArgument = errors.New("gwstore: invalid argument")

// defaultPageSize 是 limit<=0 时的分页默认值。
const defaultPageSize = 100

// Store 是网关数据访问门面。所有方法并发安全（写经单写队列串行化）。
type Store struct {
	db *sqlite.DB
}

// New 用底层连接管理构造 Store。
func New(db *sqlite.DB) *Store { return &Store{db: db} }

// scanner 是 *sql.Row 与 *sql.Rows 的共同最小能力集。
type scanner interface {
	Scan(dest ...any) error
}

// wrap 统一包装 store 层错误（保留 %w 链）。
func wrap(op string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("gwstore: %s: %w", op, err)
}

// wrapNotFound 把 sql.ErrNoRows 归一为 model.ErrNotFound。
func wrapNotFound(op string, err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s", model.ErrNotFound, op)
	}
	return wrap(op, err)
}

// collect 把结果集按 scan 逐行转成切片。
func collect[T any](rows *sql.Rows, scan func(scanner) (T, error), op string) ([]T, error) {
	defer rows.Close()
	var out []T
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, wrap(op+": scan", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap(op+": iterate", err)
	}
	return out, nil
}

func pageLimit(limit int) int {
	if limit <= 0 {
		return defaultPageSize
	}
	return limit
}

func pageOffset(offset int) int {
	if offset < 0 {
		return 0
	}
	return offset
}

func nowMS() int64 { return time.Now().UnixMilli() }

// nullStr 把空串写成 NULL（用于可空且带 UNIQUE 的列，例如
// gw_quota_ledger.idempotency_key：NULL 之间互不冲突）。
func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func scanNullStr(v sql.NullString) string {
	if v.Valid {
		return v.String
	}
	return ""
}

// isUniqueViolation 复用既有判定（modernc 驱动的 UNIQUE/PK 冲突错误文本）。
func isUniqueViolation(err error) bool { return storage.IsUniqueViolation(err) }

// isForeignKeyViolation 判定外键约束失败（例如给不存在的 user 入账）。
func isForeignKeyViolation(err error) bool {
	return strings.Contains(errText(err), "FOREIGN KEY constraint failed")
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// isNoRows 判断查询是否无结果。
func isNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }
