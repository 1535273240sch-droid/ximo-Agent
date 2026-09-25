package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

// dbTx 让测试里的 WithTx 回调签名短一点。
type dbTx = *sql.Tx

func tempDB(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "test.db")
}

func openTest(t *testing.T, cfg Config) *DB {
	t.Helper()
	db, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestOpenAppliesPragmas(t *testing.T) {
	db := openTest(t, DefaultConfig(tempDB(t)))

	ctx := context.Background()
	var mode string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want wal", mode)
	}
	var fk, sync, busy int64
	if err := db.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil {
		t.Fatalf("foreign_keys: %v", err)
	}
	if fk != 1 {
		t.Errorf("foreign_keys = %d, want 1", fk)
	}
	if err := db.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&sync); err != nil {
		t.Fatalf("synchronous: %v", err)
	}
	if sync != 2 { // FULL
		t.Errorf("synchronous = %d, want 2 (FULL)", sync)
	}
	if err := db.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busy); err != nil {
		t.Fatalf("busy_timeout: %v", err)
	}
	if busy != 5000 {
		t.Errorf("busy_timeout = %d, want 5000", busy)
	}
}

func TestProfiles(t *testing.T) {
	cases := []struct {
		profile Profile
		sync    int64
		busy    int64
	}{
		{ProfileSafe, 2, 10000},
		{ProfileBalanced, 2, 5000},
		{ProfilePerformance, 1, 15000},
	}
	for _, c := range cases {
		t.Run(c.profile.String(), func(t *testing.T) {
			cfg := DefaultConfig(tempDB(t))
			cfg.Profile = c.profile
			db := openTest(t, cfg)
			ctx := context.Background()
			var sync, busy int64
			if err := db.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&sync); err != nil {
				t.Fatalf("synchronous: %v", err)
			}
			if err := db.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busy); err != nil {
				t.Fatalf("busy_timeout: %v", err)
			}
			if sync != c.sync || busy != c.busy {
				t.Errorf("got sync=%d busy=%d, want sync=%d busy=%d", sync, busy, c.sync, c.busy)
			}
		})
	}
}

func TestConcurrentWritesFromManyGoroutines(t *testing.T) {
	db := openTest(t, DefaultConfig(tempDB(t)))
	ctx := context.Background()
	if _, err := db.ReadDB().ExecContext(ctx, "CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)"); err != nil {
		t.Fatalf("create: %v", err)
	}

	const goroutines, perG = 20, 10
	var wg sync.WaitGroup
	var mu sync.Mutex
	var failed []string
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				err := db.WithTx(ctx, func(tx dbTx) error {
					_, err := tx.ExecContext(ctx, "INSERT INTO t (v) VALUES (?)", fmt.Sprintf("g%d-i%d", g, i))
					return err
				})
				if err != nil {
					mu.Lock()
					failed = append(failed, err.Error())
					mu.Unlock()
				}
			}
		}(g)
	}
	wg.Wait()
	if len(failed) > 0 {
		t.Fatalf("write failures: %v", failed[:min(3, len(failed))])
	}
	var count int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM t").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if want := goroutines * perG; count != want {
		t.Errorf("count = %d, want %d", count, want)
	}
	st := db.Stats()
	if st.QueueCap == 0 {
		t.Error("write queue must have a capacity limit")
	}
	if st.WriteOpen != 1 {
		t.Errorf("write open conns = %d, want 1 (single writer)", st.WriteOpen)
	}
}

func TestWriteTxCommitAndRollback(t *testing.T) {
	db := openTest(t, DefaultConfig(tempDB(t)))
	ctx := context.Background()
	if _, err := db.ReadDB().ExecContext(ctx, "CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)"); err != nil {
		t.Fatalf("create: %v", err)
	}

	tx, err := db.BeginWrite(ctx)
	if err != nil {
		t.Fatalf("BeginWrite: %v", err)
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO t (v) VALUES ('a')"); err != nil {
		t.Fatalf("exec: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	tx2, err := db.BeginWrite(ctx)
	if err != nil {
		t.Fatalf("BeginWrite 2: %v", err)
	}
	if _, err := tx2.ExecContext(ctx, "INSERT INTO t (v) VALUES ('b')"); err != nil {
		t.Fatalf("exec 2: %v", err)
	}
	if err := tx2.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	var count int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM t").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("count = %d, want 1 (rollback must discard)", count)
	}
	if st := db.Stats(); st.OpenWriteTxs != 0 {
		t.Errorf("open write txs = %d, want 0", st.OpenWriteTxs)
	}
}

func TestWithTxRollbackOnError(t *testing.T) {
	db := openTest(t, DefaultConfig(tempDB(t)))
	ctx := context.Background()
	if _, err := db.ReadDB().ExecContext(ctx, "CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	err := db.WithTx(ctx, func(tx dbTx) error {
		if _, err := tx.ExecContext(ctx, "INSERT INTO t (v) VALUES ('x')"); err != nil {
			return err
		}
		return errors.New("boom")
	})
	if err == nil {
		t.Fatal("expected error from WithTx")
	}
	var count int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM t").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Errorf("count = %d, want 0 (error must roll back)", count)
	}
}

func TestPanicInWriteIsolated(t *testing.T) {
	db := openTest(t, DefaultConfig(tempDB(t)))
	ctx := context.Background()
	if _, err := db.ReadDB().ExecContext(ctx, "CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	err := db.WithTx(ctx, func(tx dbTx) error {
		panic("user code panic")
	})
	var pe *PanicError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want *PanicError", err)
	}
	// writer goroutine 必须存活：后续写正常
	if err := db.WithTx(ctx, func(tx dbTx) error {
		_, err := tx.ExecContext(ctx, "INSERT INTO t (v) VALUES ('after')")
		return err
	}); err != nil {
		t.Fatalf("write after panic: %v", err)
	}
	var count int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM t").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("count = %d, want 1 (panic tx rolled back, next tx committed)", count)
	}
	if st := db.Stats(); st.PanicsInWrite != 1 {
		t.Errorf("panics in write = %d, want 1", st.PanicsInWrite)
	}
}

func TestCloseIsIdempotentAndRejectsWrites(t *testing.T) {
	db, err := Open(DefaultConfig(tempDB(t)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	err = db.WithTx(context.Background(), func(tx dbTx) error { return nil })
	if !errors.Is(err, ErrClosed) {
		t.Errorf("WithTx after close = %v, want ErrClosed", err)
	}
	if err := db.Ping(context.Background()); err == nil {
		t.Error("Ping after close should fail")
	}
}

func TestNoGoroutineLeak(t *testing.T) {
	base := runtime.NumGoroutine()
	for i := 0; i < 5; i++ {
		db, err := Open(DefaultConfig(filepath.Join(t.TempDir(), "leak.db")))
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		ctx := context.Background()
		for j := 0; j < 5; j++ {
			if err := db.WithTx(ctx, func(tx dbTx) error {
				_, err := tx.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS t (id INTEGER PRIMARY KEY)")
				return err
			}); err != nil {
				t.Fatalf("WithTx: %v", err)
			}
		}
		if err := db.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}
	// 等待 writer goroutine 退出
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= base+2 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("goroutine leak: base=%d now=%d", base, runtime.NumGoroutine())
}

func TestContextCancelRejectsWrite(t *testing.T) {
	db := openTest(t, DefaultConfig(tempDB(t)))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := db.WithTx(ctx, func(tx dbTx) error { return nil })
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

// TestNestedWriteIsRejected 保证"写事务内再写"变成明确错误而不是死等。
func TestNestedWriteIsRejected(t *testing.T) {
	db := openTest(t, DefaultConfig(tempDB(t)))
	ctx := context.Background()
	if _, err := db.ReadDB().ExecContext(ctx, "CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		done <- db.WithTx(ctx, func(tx dbTx) error {
			// 外层事务正在 writer goroutine 上执行；这里再发起一次写。
			return db.WithTx(ctx, func(inner dbTx) error { return nil })
		})
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrNestedWrite) {
			t.Fatalf("nested write err = %v, want ErrNestedWrite", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("nested write deadlocked the write queue")
	}
	// writer 仍然健康
	if err := db.WithTx(ctx, func(tx dbTx) error {
		_, err := tx.ExecContext(ctx, "INSERT INTO t (v) VALUES ('ok')")
		return err
	}); err != nil {
		t.Fatalf("write after nested rejection: %v", err)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
