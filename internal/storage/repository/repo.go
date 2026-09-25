// Package repository 提供各表的 CRUD 与复合事务（第9章）。
//
// 所有写操作都必须经过 storage.Store 的单写队列；跨表复合事务
// （如 Tool 完成时 tool_results + run_events + runs + outbox 一起提交）
// 收敛在 RunRepo.CompleteToolCall，杜绝“工具已完成但 DB 说没完成”的
// 中间态（第9.2章）。
package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ximo888ok-netizen/ximo-agent/internal/storage"
)

// Repos 聚合所有 repository。
type Repos struct {
	Sessions    *SessionRepo
	Runs        *RunRepo
	Messages    *MessageRepo
	ToolCalls   *ToolCallRepo
	ToolResults *ToolResultRepo
	Checkpoints *CheckpointRepo
	Providers   *ProviderRepo
	Permissions *PermissionRepo
	McpServers  *McpServerRepo
	Experts     *ExpertRepo
	Skills      *SkillRepo
	Knowledge   *KnowledgeRepo
	Leases      *LeaseRepo
	KV          *KVRepo
	Events      *storage.EventLog
	Outbox      *storage.Outbox
}

// New 用同一个 Store 构造全部 repository。
func New(store *storage.Store) *Repos {
	db := store.DB()
	sessions := &SessionRepo{db: db}
	runs := &RunRepo{db: db, store: store}
	messages := &MessageRepo{db: db}
	toolCalls := &ToolCallRepo{db: db}
	toolResults := &ToolResultRepo{db: db}
	checkpoints := &CheckpointRepo{db: db}
	runs.calls = toolCalls
	runs.results = toolResults
	return &Repos{
		Sessions:    sessions,
		Runs:        runs,
		Messages:    messages,
		ToolCalls:   toolCalls,
		ToolResults: toolResults,
		Checkpoints: checkpoints,
		Providers:   &ProviderRepo{db: db},
		Permissions: &PermissionRepo{db: db},
		McpServers:  &McpServerRepo{db: db},
		Experts:     &ExpertRepo{db: db},
		Skills:      &SkillRepo{db: db},
		Knowledge:   &KnowledgeRepo{db: db},
		Leases:      &LeaseRepo{db: db},
		KV:          &KVRepo{db: db},
		Events:      storage.NewEventLog(db),
		Outbox:      storage.NewOutbox(db),
	}
}

// txExecer 是事务内可执行 SQL 的最小能力集。storage.Tx（*sql.Tx 或
// *sqlite.WriteTx）都满足它，因此复合事务对两种事务来源写法一致。
type txExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// dbHandle 是 repository 共用的底层句柄。
type dbHandle = sql.DB

func jsonOrNull(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return []byte(raw)
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func scanNullStr(v sql.NullString) string {
	if v.Valid {
		return v.String
	}
	return ""
}

func scanNullInt64(v sql.NullInt64) int64 {
	if v.Valid {
		return v.Int64
	}
	return 0
}

// ErrNotFound 表示查询目标不存在（所有 Get 类方法的统一哨兵错误）。
var ErrNotFound = errors.New("repository: not found")

// wrapErr 统一包装仓储层错误，保留 %w 链（sql.ErrNoRows 归一为 ErrNotFound）。
func wrapErr(op string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrNotFound, op)
	}
	return fmt.Errorf("repository: %s: %w", op, err)
}
