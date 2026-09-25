package storage

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/ximo888ok-netizen/ximo-agent/internal/storage/sqlite"
)

// Tx 是写事务句柄。*sql.Tx 与 *sqlite.WriteTx 都满足它，因此复合事务
// （如 Tool 完成时 INSERT tool_results + INSERT run_events + UPDATE runs +
// INSERT outbox 后一起 COMMIT，第9.2章）对两种事务来源写法一致。
type Tx interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	Commit() error
	Rollback() error
}

// Store 是持久层门面：持有连接管理实例，对外暴露事务与查询入口。
type Store struct {
	db *sqlite.DB
}

// Open 打开存储层（不含 schema migration；启动流程应先执行
// migrations.Runner.Apply 再 Open，或使用 OpenWithMigrations）。
func Open(cfg sqlite.Config) (*Store, error) {
	db, err := sqlite.Open(cfg)
	if err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

// WithTx 执行一个复合写事务（走单写队列，天然串行）。
//
// 注意：fn 内部不允许再调用任何走写队列的方法（WithTx / BeginWrite /
// repository 的写方法 / Outbox.Ack 等）——单写队列下这必然死等，框架会以
// sqlite.ErrNestedWrite 明确拒绝而不是挂起。
func (s *Store) WithTx(ctx context.Context, fn func(tx Tx) error) error {
	return s.db.WithTx(ctx, func(stx *sql.Tx) error {
		return fn(stx)
	})
}

// BeginWrite 开启跨调用保持的写事务，用法与 database/sql 一致，
// 必须 Commit 或 Rollback。
func (s *Store) BeginWrite(ctx context.Context) (Tx, error) {
	return s.db.BeginWrite(ctx)
}

// QueryContext 走读连接池查询。
func (s *Store) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return s.db.QueryContext(ctx, query, args...)
}

// QueryRowContext 走读连接池查询单行。
func (s *Store) QueryRowContext(ctx context.Context, query string, args ...any) sqlite.Row {
	return s.db.QueryRowContext(ctx, query, args...)
}

// DB 暴露底层连接管理（repository / migrations / dispatcher 使用）。
func (s *Store) DB() *sqlite.DB { return s.db }

// Stats 返回连接池/写队列指标。
func (s *Store) Stats() sqlite.Stats { return s.db.Stats() }

// Ping 探测连通性。
func (s *Store) Ping(ctx context.Context) error { return s.db.Ping(ctx) }

// Close 关闭存储层。
func (s *Store) Close() error { return s.db.Close() }

// CheckIntegrity 执行 PRAGMA integrity_check，确认数据库文件未损坏
// （WAL 恢复测试 / 磁盘写满测试的断言依据）。
func (s *Store) CheckIntegrity(ctx context.Context) error {
	row := s.db.QueryRowContext(ctx, "PRAGMA integrity_check")
	var result string
	if err := row.Scan(&result); err != nil {
		return fmt.Errorf("storage: integrity_check: %w", err)
	}
	if result != "ok" {
		return fmt.Errorf("storage: integrity_check: %s", result)
	}
	return nil
}
