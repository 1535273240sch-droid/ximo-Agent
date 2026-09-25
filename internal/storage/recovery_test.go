package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/storage/migrations"
	"github.com/ximo888ok-netizen/ximo-agent/internal/storage/sqlite"
)

// 崩溃子进程通过环境变量触发：
//
//	XIMO_CRASH_MODE=committed     写入并提交后硬退出（os.Exit，跳过所有 defer）
//	XIMO_CRASH_MODE=uncommitted   开着写事务未提交就硬退出
//	XIMO_CRASH_DB=<path>          目标数据库路径
const (
	envCrashMode = "XIMO_CRASH_MODE"
	envCrashDB   = "XIMO_CRASH_DB"
)

// crashChild 在子进程里跑“崩溃前”的写入。
func crashChild(mode, dbPath string) int {
	db, err := sqlite.Open(sqlite.DefaultConfig(dbPath))
	if err != nil {
		fmt.Fprintln(os.Stderr, "child open:", err)
		return 2
	}
	ctx := context.Background()
	if _, err := migrations.ApplyFromDir(ctx, db, migrationsDir); err != nil {
		fmt.Fprintln(os.Stderr, "child migrate:", err)
		return 2
	}
	// session + run，供 event 使用
	if err := db.WithTx(ctx, func(tx *sql.Tx) error {
		now := time.Now().UnixMilli()
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO sessions (id, title, mode, created_at, updated_at) VALUES ('sess-crash','crash','default',?,?)`, now, now); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO runs (id, session_id, status, created_at, updated_at) VALUES ('run-crash','sess-crash','running',?,?)`, now, now)
		return err
	}); err != nil {
		fmt.Fprintln(os.Stderr, "child seed:", err)
		return 2
	}

	switch mode {
	case "committed":
		// 写入 300 个 event + kv，全部提交，然后硬退出。
		// wal_autocheckpoint=1000 页（~4MB），这个量级的数据大部分还在 WAL 里，
		// 重开数据库必须能 replay。
		log := NewEventLog(db)
		for i := 0; i < 300; i++ {
			payload, _ := json.Marshal(map[string]any{"i": i, "blob": strings.Repeat("x", 200)})
			if _, err := log.Append(ctx, "run-crash", "crash.event", payload); err != nil {
				fmt.Fprintln(os.Stderr, "child append:", err)
				return 2
			}
		}
		if err := db.WithTx(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, "INSERT INTO kv (key, value, updated_at) VALUES ('crash-marker','committed',1)")
			return err
		}); err != nil {
			fmt.Fprintln(os.Stderr, "child kv:", err)
			return 2
		}
	case "uncommitted":
		// 开一个写事务，插一行，不提交，硬退出。
		tx, err := db.BeginWrite(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, "child begin:", err)
			return 2
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO kv (key, value, updated_at) VALUES ('ghost','uncommitted',1)"); err != nil {
			fmt.Fprintln(os.Stderr, "child exec:", err)
			return 2
		}
	case "outbox":
		// event + outbox 同事务提交，然后硬退出（dispatcher 从未运行）
		log := NewEventLog(db)
		ob := NewOutbox(db)
		err := db.WithTx(ctx, func(tx *sql.Tx) error {
			seq, eventID, err := log.AppendTx(ctx, tx, "run-crash", "crash.outbox", []byte(`{"k":"v"}`))
			if err != nil {
				return err
			}
			return ob.Enqueue(ctx, tx, Event{
				EventID:     eventID,
				RunID:       "run-crash",
				Seq:         seq,
				Type:        "crash.outbox",
				PayloadJSON: []byte(`{"k":"v"}`),
				CreatedAt:   time.Now().UnixMilli(),
			})
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "child outbox:", err)
			return 2
		}
	default:
		fmt.Fprintln(os.Stderr, "unknown mode:", mode)
		return 2
	}

	// 硬退出：不 Close、不 defer —— 模拟进程被杀/断电
	os.Exit(9)
	return 0
}

// runCrashChild 起一个子进程执行崩溃写入，返回其退出码。
func runCrashChild(t *testing.T, mode, dbPath string) int {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestWALRecoveryAfterHardKill$")
	cmd.Env = append(os.Environ(), envCrashMode+"="+mode, envCrashDB+"="+dbPath)
	out, err := cmd.CombinedOutput()
	t.Logf("child output: %s", out)
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		t.Fatalf("run child: %v", err)
	}
	return 0
}

// TestWALRecoveryAfterHardKill 是第45章"数据库 WAL 恢复测试"的对应实现：
// 子进程写入后硬退出（不走任何清理路径），父进程重开数据库验证
// ① 已提交数据全部在 ② 未提交数据不在 ③ 文件未损坏。
func TestWALRecoveryAfterHardKill(t *testing.T) {
	if mode := os.Getenv(envCrashMode); mode != "" {
		os.Exit(crashChild(mode, os.Getenv(envCrashDB)))
	}

	ctx := context.Background()

	t.Run("committed", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "wal.db")
		if code := runCrashChild(t, "committed", dbPath); code != 9 {
			t.Fatalf("child exit = %d, want 9 (hard kill)", code)
		}
		// WAL 文件必须还在（数据未经 checkpoint，只能靠 replay 恢复）
		if _, err := os.Stat(dbPath + "-wal"); err != nil {
			t.Fatalf("wal file missing: %v", err)
		}

		store, err := Open(sqlite.DefaultConfig(dbPath))
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		defer store.Close()

		// 未损坏
		if err := store.CheckIntegrity(ctx); err != nil {
			t.Fatalf("integrity: %v", err)
		}
		// event 全部恢复，且 sequence 连续不倒退（I4）
		log := NewEventLog(store.DB())
		events, err := log.Since(ctx, "run-crash", 0)
		if err != nil {
			t.Fatalf("Since: %v", err)
		}
		if len(events) != 300 {
			t.Fatalf("recovered events = %d, want 300 (WAL replay)", len(events))
		}
		for i, ev := range events {
			if ev.Seq != uint64(i+1) {
				t.Fatalf("event %d seq = %d: sequence must be dense after recovery", i, ev.Seq)
			}
		}
		last, err := log.LastSeq(ctx, "run-crash")
		if err != nil {
			t.Fatalf("LastSeq: %v", err)
		}
		if last != 300 {
			t.Errorf("runs.last_seq = %d, want 300", last)
		}
		var marker string
		if err := store.QueryRowContext(ctx, "SELECT value FROM kv WHERE key='crash-marker'").Scan(&marker); err != nil {
			t.Fatalf("read marker: %v", err)
		}
		if string(marker) != "committed" {
			t.Errorf("marker = %q, want committed", marker)
		}
		// 恢复后还能继续写
		if _, err := log.Append(ctx, "run-crash", "after.crash", []byte("{}")); err != nil {
			t.Fatalf("append after recovery: %v", err)
		}
	})

	t.Run("uncommitted", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "wal2.db")
		if code := runCrashChild(t, "uncommitted", dbPath); code != 9 {
			t.Fatalf("child exit = %d, want 9 (hard kill)", code)
		}
		store, err := Open(sqlite.DefaultConfig(dbPath))
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		defer store.Close()
		if err := store.CheckIntegrity(ctx); err != nil {
			t.Fatalf("integrity: %v", err)
		}
		// 半提交事务必须被回滚：ghost 行不存在（I6）
		var ghost int
		if err := store.QueryRowContext(ctx, "SELECT COUNT(*) FROM kv WHERE key='ghost'").Scan(&ghost); err != nil {
			t.Fatalf("count ghost: %v", err)
		}
		if ghost != 0 {
			t.Errorf("uncommitted row survived crash: %d rows (I6 violation)", ghost)
		}
		// seed 数据仍在
		var runs int
		if err := store.QueryRowContext(ctx, "SELECT COUNT(*) FROM runs WHERE id='run-crash'").Scan(&runs); err != nil {
			t.Fatalf("count runs: %v", err)
		}
		if runs != 1 {
			t.Errorf("runs = %d, want 1", runs)
		}
	})
}

// TestOutboxSurvivesCrash 验证第25章的核心承诺：event 已提交但 dispatcher
// 尚未送达时进程崩溃，重启后 outbox 仍能 replay 未送达事件。
func TestOutboxSurvivesCrash(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "outbox.db")

	// 阶段1：提交 run + event + outbox，然后硬退出（未来得及投递）
	if code := runCrashChild(t, "outbox", dbPath); code != 9 {
		t.Fatalf("child exit = %d, want 9", code)
	}

	// 阶段2：重启后 poll 到未送达事件并投递、ack
	store, err := Open(sqlite.DefaultConfig(dbPath))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer store.Close()
	if err := store.CheckIntegrity(ctx); err != nil {
		t.Fatalf("integrity: %v", err)
	}

	events, err := NewEventLog(store.DB()).Since(ctx, "run-crash", 0)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}

	ob := NewOutbox(store.DB())
	pending, err := ob.PollUndelivered(ctx)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("undelivered = %d, want 1 (outbox must survive the crash)", len(pending))
	}
	if pending[0].EventID != events[0].EventID {
		t.Errorf("outbox event %s != committed event %s", pending[0].EventID, events[0].EventID)
	}
	if err := ob.Ack(ctx, pending[0].EventID); err != nil {
		t.Fatalf("ack: %v", err)
	}
	depth, err := ob.Depth(ctx)
	if err != nil {
		t.Fatalf("depth: %v", err)
	}
	if depth != 0 {
		t.Errorf("depth = %d, want 0", depth)
	}
}
