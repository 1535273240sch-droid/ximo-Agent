package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/storage/migrations"
	"github.com/ximo888ok-netizen/ximo-agent/internal/storage/sqlite"
)

// TestStressManyRunsZeroCorruption 是第43章 P0 红线（"10000 次 run 零损坏"）
// 的缩小版证据：并发跑大量 run，每个 run 写多条 event 并走 outbox，
// 最后核对 ① 数据库完整 ② event sequence 无倒退/无空洞/无重复
// ③ outbox 全部可送达 ④ 无残留写事务。
func TestStressManyRunsZeroCorruption(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test skipped in -short mode")
	}
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "stress.db")
	store, err := Open(sqlite.DefaultConfig(path))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	if _, err := migrations.ApplyFromDir(ctx, store.DB(), migrationsDir); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	log := NewEventLog(store.DB())
	outbox := NewOutbox(store.DB())

	const (
		runs         = 200
		eventsPerRun = 5
		concurrency  = 16
	)
	runIDs := make([]string, runs)
	for i := range runIDs {
		runIDs[i] = fmt.Sprintf("stress-run-%04d", i)
	}

	// 并发创建 session + run
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for i, runID := range runIDs {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, runID string) {
			defer wg.Done()
			defer func() { <-sem }()
			sessionID := fmt.Sprintf("stress-sess-%04d", i)
			if err := store.DB().WithTx(ctx, func(tx dbTx) error {
				now := time.Now().UnixMilli()
				if _, err := tx.ExecContext(ctx,
					`INSERT INTO sessions (id, title, mode, created_at, updated_at) VALUES (?,?,?,?,?)`,
					sessionID, "stress", "default", now, now); err != nil {
					return err
				}
				_, err := tx.ExecContext(ctx,
					`INSERT INTO runs (id, session_id, status, created_at, updated_at) VALUES (?,?,?,?,?)`,
					runID, sessionID, "running", now, now)
				return err
			}); err != nil {
				t.Errorf("create run %s: %v", runID, err)
			}
		}(i, runID)
	}
	wg.Wait()

	// 并发写 event（同一 run 内 seq 必须严格递增，跨 run 并发）
	var seqErrMu sync.Mutex
	var seqErrs []string
	for _, runID := range runIDs {
		wg.Add(1)
		sem <- struct{}{}
		go func(runID string) {
			defer wg.Done()
			defer func() { <-sem }()
			for e := 0; e < eventsPerRun; e++ {
				payload, _ := json.Marshal(map[string]any{"run": runID, "n": e})
				// event 与 outbox 同事务提交（第9.2章形态）
				var seq uint64
				err := store.WithTx(ctx, func(tx Tx) error {
					var eventID string
					var err error
					seq, eventID, err = log.AppendTx(ctx, tx, runID, "stress.event", payload)
					if err != nil {
						return err
					}
					return outbox.Enqueue(ctx, tx, Event{
						EventID:     eventID,
						RunID:       runID,
						Seq:         seq,
						Type:        "stress.event",
						PayloadJSON: payload,
						CreatedAt:   time.Now().UnixMilli(),
					})
				})
				if err != nil {
					seqErrMu.Lock()
					seqErrs = append(seqErrs, err.Error())
					seqErrMu.Unlock()
					return
				}
				if seq != uint64(e+1) {
					seqErrMu.Lock()
					seqErrs = append(seqErrs, fmt.Sprintf("run %s: seq %d at position %d", runID, seq, e))
					seqErrMu.Unlock()
					return
				}
			}
		}(runID)
	}
	wg.Wait()
	seqErrMu.Lock()
	defer seqErrMu.Unlock()
	if len(seqErrs) > 0 {
		t.Fatalf("event errors (first 3): %v", seqErrs[:min(3, len(seqErrs))])
	}

	// 核对每个 run 的 event 序列：连续、无重复、last_seq 一致
	for _, runID := range runIDs {
		events, err := log.Since(ctx, runID, 0)
		if err != nil {
			t.Fatalf("Since %s: %v", runID, err)
		}
		if len(events) != eventsPerRun {
			t.Fatalf("run %s events = %d, want %d", runID, len(events), eventsPerRun)
		}
		for i, ev := range events {
			if ev.Seq != uint64(i+1) {
				t.Fatalf("run %s event %d seq = %d: sequence must be dense", runID, i, ev.Seq)
			}
		}
		last, err := log.LastSeq(ctx, runID)
		if err != nil {
			t.Fatalf("LastSeq %s: %v", runID, err)
		}
		if last != uint64(eventsPerRun) {
			t.Fatalf("run %s last_seq = %d, want %d", runID, last, eventsPerRun)
		}
	}

	// outbox 全部可送达（poll 有批量上限，用 Depth 断言总量）
	depth, err := outbox.Depth(ctx)
	if err != nil {
		t.Fatalf("depth: %v", err)
	}
	if depth != runs*eventsPerRun {
		t.Fatalf("outbox depth = %d, want %d", depth, runs*eventsPerRun)
	}
	delivered := 0
	d := NewDispatcher(outbox, DeliverFunc(func(ctx context.Context, e Event) error {
		delivered++
		return nil
	}), DispatcherConfig{Interval: time.Millisecond, BatchSize: 500})
	for i := 0; i < 40; i++ {
		if _, err := d.RunOnce(ctx); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
		if depth, _ := outbox.Depth(ctx); depth == 0 {
			break
		}
	}
	if depth, _ := outbox.Depth(ctx); depth != 0 {
		t.Fatalf("outbox depth = %d after dispatch, want 0", depth)
	}
	if delivered != runs*eventsPerRun {
		t.Errorf("delivered = %d, want %d", delivered, runs*eventsPerRun)
	}

	// 数据库完整性 + 无残留写事务
	if err := store.CheckIntegrity(ctx); err != nil {
		t.Fatalf("integrity: %v", err)
	}
	st := store.Stats()
	if st.OpenWriteTxs != 0 {
		t.Errorf("open write txs = %d, want 0 (no leaked transactions)", st.OpenWriteTxs)
	}
	if st.WriteOpen != 1 {
		t.Errorf("write connections = %d, want 1 (single writer preserved)", st.WriteOpen)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
