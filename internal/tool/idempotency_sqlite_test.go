package tool

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite" // 测试专用驱动（与任务03 同版本，见交付说明）
)

// openSQLite 打开一个临时文件 SQLite 库并建表。
//
// DSN 必须显式携带 _pragma=busy_timeout(...)：modernc.org/sqlite 仅在 DSN
// 里出现该参数时才设置 busy_timeout，裸路径的默认值是 0ms。并发场景下
// 8 个连接会同时在 INSERT ... ON CONFLICT 上拿到 SQLITE_BUSY
// （"database is locked"）而立即失败，使并发测试形同虚设。
// 生产 DSN 同样带 busy_timeout(5000)（见 internal/storage/sqlite/dsn.go 的
// buildDSN）；测试只需 busy_timeout 即可验证并发语义，无需额外启用 WAL。
func openSQLite(t *testing.T) (*sql.DB, func()) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "idempotency.db")
	db, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("打开 SQLite 失败: %v", err)
	}
	if _, err := db.Exec(IdempotencySchema); err != nil {
		db.Close()
		t.Fatalf("建表失败（%s 是否与任务03 的 migration 一致？）: %v", IdempotencySchema, err)
	}
	return db, func() { db.Close() }
}

// TestSQLIdempotencyClaimCompleteReplay 是审核报告 B-2 要求的真实 SQLite
// 集成测试：跑 Claim → Complete → 重放 的完整路径（不再只用内存 mock）。
func TestSQLIdempotencyClaimCompleteReplay(t *testing.T) {
	db, closeDB := openSQLite(t)
	defer closeDB()

	store := NewIdempotencyStore(NewSQLIdempotencyDB(db), nil, nil, 5*time.Minute)
	ctx := context.Background()
	key := Key("run-sql", "call-1")

	// 认领
	claimed, err := store.Claim(ctx, key)
	if err != nil {
		t.Fatalf("Claim 失败: %v", err)
	}
	if !claimed {
		t.Fatalf("首次认领应当成功")
	}
	// I5：并发/重复认领必须失败
	claimed2, err := store.Claim(ctx, key)
	if err != nil {
		t.Fatalf("二次认领不应报错: %v", err)
	}
	if claimed2 {
		t.Fatalf("I5 违反：SQL 实现下同一 key 被认领两次")
	}

	// 完成并写入结果
	payload := []byte(`{"content":"sql-done","success":true}`)
	if err := store.Complete(ctx, key, payload); err != nil {
		t.Fatalf("Complete 失败: %v", err)
	}
	// 重复提交被拒绝
	if err := store.Complete(ctx, key, []byte(`{"content":"dup"}`)); err == nil {
		t.Fatalf("重复 Complete 应当被拒绝（I5）")
	}

	// 重放：Get 返回 done 与结果
	status, result, found, err := store.Get(ctx, key)
	if err != nil || !found {
		t.Fatalf("Get = %v %v", found, err)
	}
	if status != StatusDone {
		t.Fatalf("状态 = %q，期望 done", status)
	}
	if string(result) != string(payload) {
		t.Fatalf("结果 = %q，期望 %q", result, payload)
	}
	decoded, err := DecodeResult(result)
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	if decoded.Content != "sql-done" || !decoded.Success {
		t.Fatalf("解码结果 = %+v", decoded)
	}
}

// TestSQLIdempotencyStaleTakeover 验证陈旧 inflight 认领可被接管，
// 且 C 类被标记 needs_confirmation（第19章恢复路径）。
func TestSQLIdempotencyStaleTakeover(t *testing.T) {
	db, closeDB := openSQLite(t)
	defer closeDB()

	fixed := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store := NewIdempotencyStore(NewSQLIdempotencyDB(db), nil, nil, time.Minute)
	store.Now = func() time.Time { return fixed }
	ctx := context.Background()

	// A 类：陈旧后接管
	keyA := Key("run-sql", "call-a")
	if _, err := store.Claim(ctx, keyA); err != nil {
		t.Fatal(err)
	}
	store.Now = func() time.Time { return fixed.Add(2 * time.Minute) }
	resume, err := store.RecoverInflight(ctx, keyA, ClassIdempotent)
	if err != nil || !resume {
		t.Fatalf("A 类陈旧认领应当放行: %v %v", resume, err)
	}
	status, _, _, _ := store.Get(ctx, keyA)
	if status != StatusInflight {
		t.Fatalf("接管后状态 = %q", status)
	}

	// C 类：陈旧后标记 needs_confirmation，绝不自动重复
	keyC := Key("run-sql", "call-c")
	claimCAt := fixed.Add(2 * time.Minute)
	store.Now = func() time.Time { return claimCAt }
	if _, err := store.Claim(ctx, keyC); err != nil {
		t.Fatal(err)
	}
	store.Now = func() time.Time { return claimCAt.Add(2 * time.Minute) } // 推过期
	resume, err = store.RecoverInflight(ctx, keyC, ClassNonIdempotent)
	if err != nil || resume {
		t.Fatalf("C 类陈旧认领绝不放行: %v %v", resume, err)
	}
	status, _, _, _ = store.Get(ctx, keyC)
	if status != StatusNeedsConfirmation {
		t.Fatalf("C 类恢复后状态 = %q，期望 needs_confirmation", status)
	}

	// 新鲜认领不可恢复
	keyFresh := Key("run-sql", "call-fresh")
	if _, err := store.Claim(ctx, keyFresh); err != nil {
		t.Fatal(err)
	}
	store.Now = func() time.Time { return fixed.Add(3 * time.Minute) }
	if resume, _ := store.RecoverInflight(ctx, keyFresh, ClassNonIdempotent); resume {
		t.Fatalf("新鲜认领不应当允许恢复")
	}
}

// TestSQLIdempotencyMissingTableFailsLoudly 验证表缺失时是**响亮的**失败
// （B-2：绝不允许静默跳过幂等保护）。
func TestSQLIdempotencyMissingTableFailsLoudly(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "empty.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// 故意不建表

	store := NewIdempotencyStore(NewSQLIdempotencyDB(db), nil, nil, time.Minute)
	ctx := context.Background()
	key := Key("run-sql", "call-missing")

	// Claim 必须报错（no such table），而不是静默返回 true
	if _, err := store.Claim(ctx, key); err == nil {
		t.Fatalf("表缺失时 Claim 必须报错（fail-loud）")
	} else if !strings.Contains(err.Error(), "no such table") {
		t.Fatalf("错误应指向缺失的表: %v", err)
	}
	// Get 同样必须报错
	if _, _, _, err := store.Get(ctx, key); err == nil {
		t.Fatalf("表缺失时 Get 必须报错（fail-loud）")
	}
}

// TestSQLIdempotencyConcurrentClaims 用真实数据库验证 I5：
// 多 goroutine 并发认领同一 key，只有一个成功。
func TestSQLIdempotencyConcurrentClaims(t *testing.T) {
	db, closeDB := openSQLite(t)
	defer closeDB()

	store := NewIdempotencyStore(NewSQLIdempotencyDB(db), nil, nil, 5*time.Minute)
	ctx := context.Background()
	key := Key("run-sql", "call-concurrent")

	const workers = 8
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		success   int
		claimErrs []error
	)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			claimed, err := store.Claim(ctx, key)
			if err != nil {
				// 不得静默吞掉错误：SQLITE_BUSY 之类的可重试错误若被丢弃，
				// 会伪装成"0 次成功"的 I5 断言失败，掩盖真正的病根。
				mu.Lock()
				claimErrs = append(claimErrs, err)
				mu.Unlock()
				return
			}
			if claimed {
				mu.Lock()
				success++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(claimErrs) > 0 {
		t.Fatalf("并发认领出现 %d 个错误（首个：%v），不应发生", len(claimErrs), claimErrs[0])
	}
	if success != 1 {
		t.Fatalf("I5 违反：并发认领成功 %d 次，期望 1", success)
	}
}
