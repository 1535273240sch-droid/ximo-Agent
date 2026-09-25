package storage

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ximo888ok-netizen/ximo-agent/internal/storage/migrations"
	"github.com/ximo888ok-netizen/ximo-agent/internal/storage/sqlite"
)

type dbTx = *sql.Tx

// TestDiskFullDoesNotCorruptDatabase 模拟磁盘写满（SQLITE_FULL）：
// 先用 max_page_count 把数据库大小锁死，再持续写入直到报错，
// 断言：① 错误真的发生 ② 已提交数据完好 ③ 数据库文件未损坏。
// 这与真实 ENOSPC 走同一条 SQLITE_FULL 错误路径。
func TestDiskFullDoesNotCorruptDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "full.db")

	// 1) 正常建库 + 灌一批基准数据
	base, err := Open(sqlite.DefaultConfig(path))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := migrations.ApplyFromDir(ctx, base.DB(), migrationsDir); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	const baseline = 50
	for i := 0; i < baseline; i++ {
		payload := strings.Repeat(fmt.Sprintf("row-%04d;", i), 200) // ~2KB/行
		if err := base.DB().WithTx(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, "INSERT INTO kv (key, value, updated_at) VALUES (?,?,?)",
				fmt.Sprintf("base-%04d", i), []byte(payload), 1)
			return err
		}); err != nil {
			t.Fatalf("baseline insert %d: %v", i, err)
		}
	}
	if err := base.Close(); err != nil {
		t.Fatalf("close base: %v", err)
	}

	// 2) 以 max_page_count 锁死大小后重开，持续写入直到 SQLITE_FULL
	probe, err := sqlite.Open(sqlite.DefaultConfig(path))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	var pageCount int
	if err := probe.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pageCount); err != nil {
		t.Fatalf("page_count: %v", err)
	}
	if err := probe.Close(); err != nil {
		t.Fatalf("close probe: %v", err)
	}

	limitedCfg := sqlite.DefaultConfig(path)
	limitedCfg.ExtraPragmas = []string{fmt.Sprintf("max_page_count(%d)", pageCount+2)}
	full, err := Open(limitedCfg)
	if err != nil {
		t.Fatalf("open limited: %v", err)
	}

	var fullErr error
	for i := 0; i < 2000 && fullErr == nil; i++ {
		payload := strings.Repeat(fmt.Sprintf("flood-%06d;", i), 400) // ~4KB/行
		fullErr = full.DB().WithTx(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, "INSERT INTO kv (key, value, updated_at) VALUES (?,?,?)",
				fmt.Sprintf("flood-%06d", i), []byte(payload), 1)
			return err
		})
	}
	if fullErr == nil {
		t.Fatal("expected SQLITE_FULL (disk full simulation) after hitting max_page_count")
	}
	t.Logf("disk-full error surfaced to caller: %v", fullErr)

	// 3) 已提交数据完好：基准行一条不少
	var got int
	if err := full.QueryRowContext(ctx, "SELECT COUNT(*) FROM kv WHERE key LIKE 'base-%'").Scan(&got); err != nil {
		t.Fatalf("count baseline: %v", err)
	}
	if got != baseline {
		t.Errorf("baseline rows = %d, want %d (existing data must survive a full disk)", got, baseline)
	}

	// 4) 数据库文件未损坏
	if err := full.CheckIntegrity(ctx); err != nil {
		t.Fatalf("integrity check after disk full: %v", err)
	}

	// 5) 去掉限制后仍可正常打开、写入
	if err := full.Close(); err != nil {
		t.Fatalf("close limited: %v", err)
	}
	after, err := Open(sqlite.DefaultConfig(path))
	if err != nil {
		t.Fatalf("reopen after full: %v", err)
	}
	defer after.Close()
	if err := after.CheckIntegrity(ctx); err != nil {
		t.Fatalf("integrity after reopen: %v", err)
	}
	if err := after.DB().WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "INSERT INTO kv (key, value, updated_at) VALUES ('after-full','x',1)")
		return err
	}); err != nil {
		t.Fatalf("write after full: %v", err)
	}
}
