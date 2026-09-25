package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/storage/migrations"
	"github.com/ximo888ok-netizen/ximo-agent/internal/storage/sqlite"
)

// migrationsDir 指向仓库根的 migrations/（本包位于 internal/storage，向上两级）。
var migrationsDir = filepath.Join("..", "..", "migrations")

func newTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	store, err := Open(sqlite.DefaultConfig(path))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := migrations.ApplyFromDir(context.Background(), store.DB(), migrationsDir); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return store
}

// newTestRun 造一个 session + run，返回 runID。
func newTestRun(t *testing.T, store *Store) string {
	t.Helper()
	ctx := context.Background()
	sessionID := "sess-" + t.Name()
	if err := store.DB().WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO sessions (id, title, mode, created_at, updated_at) VALUES (?,?,?,?,?)`,
			sessionID, "test", "default", time.Now().UnixMilli(), time.Now().UnixMilli())
		return err
	}); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	runID := "run-" + t.Name()
	if err := store.DB().WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO runs (id, session_id, status, created_at, updated_at) VALUES (?,?,?,?,?)`,
			runID, sessionID, "running", time.Now().UnixMilli(), time.Now().UnixMilli())
		return err
	}); err != nil {
		t.Fatalf("insert run: %v", err)
	}
	return runID
}

// ---------------------------------------------------------------- events

func TestEventAppendAndSince(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	runID := newTestRun(t, store)
	log := NewEventLog(store.DB())

	var lastSeq uint64
	for i := 0; i < 5; i++ {
		payload, _ := json.Marshal(map[string]any{"i": i})
		seq, err := log.Append(ctx, runID, "test.event", payload)
		if err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
		if seq != lastSeq+1 {
			t.Fatalf("seq = %d, want %d (I4: strictly increasing)", seq, lastSeq+1)
		}
		lastSeq = seq
	}

	events, err := log.Since(ctx, runID, 0)
	if err != nil {
		t.Fatalf("Since: %v", err)
	}
	if len(events) != 5 {
		t.Fatalf("Since returned %d events, want 5", len(events))
	}
	for i, ev := range events {
		if ev.Seq != uint64(i+1) {
			t.Errorf("event %d seq = %d, want %d", i, ev.Seq, i+1)
		}
		if ev.RunID != runID {
			t.Errorf("event %d runID = %q, want %q", i, ev.RunID, runID)
		}
		if ev.EventID == "" {
			t.Errorf("event %d missing event_id", i)
		}
	}

	// afterSeq 过滤
	partial, err := log.Since(ctx, runID, 3)
	if err != nil {
		t.Fatalf("Since(3): %v", err)
	}
	if len(partial) != 2 || partial[0].Seq != 4 {
		t.Fatalf("Since(3) = %+v, want seq 4,5", partial)
	}

	// limit
	limited, err := log.SinceN(ctx, runID, 0, 2)
	if err != nil {
		t.Fatalf("SinceN: %v", err)
	}
	if len(limited) != 2 {
		t.Fatalf("SinceN(2) returned %d, want 2", len(limited))
	}

	// LastSeq 与 runs.last_seq 一致
	last, err := log.LastSeq(ctx, runID)
	if err != nil {
		t.Fatalf("LastSeq: %v", err)
	}
	if last != 5 {
		t.Errorf("LastSeq = %d, want 5", last)
	}
}

func TestEventSeqNeverRegressesUnderConcurrency(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	runID := newTestRun(t, store)
	log := NewEventLog(store.DB())

	const n = 40
	var wg sync.WaitGroup
	seqs := make(chan uint64, n)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			seq, err := log.Append(ctx, runID, "concurrent.event", []byte(fmt.Sprintf(`{"i":%d}`, i)))
			if err != nil {
				errs <- err
				return
			}
			seqs <- seq
		}(i)
	}
	wg.Wait()
	close(seqs)
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Append: %v", err)
	}

	seen := make(map[uint64]bool, n)
	var maxSeq uint64
	for s := range seqs {
		if seen[s] {
			t.Fatalf("duplicate seq %d (I4 violation)", s)
		}
		seen[s] = true
		if s > maxSeq {
			maxSeq = s
		}
	}
	if maxSeq != n {
		t.Errorf("max seq = %d, want %d", maxSeq, n)
	}
	if len(seen) != n {
		t.Errorf("unique seqs = %d, want %d", len(seen), n)
	}

	events, err := log.Since(ctx, runID, 0)
	if err != nil {
		t.Fatalf("Since: %v", err)
	}
	if len(events) != n {
		t.Fatalf("events = %d, want %d", len(events), n)
	}
	for i, ev := range events {
		if ev.Seq != uint64(i+1) {
			t.Fatalf("event %d has seq %d: sequence must be dense and ordered", i, ev.Seq)
		}
	}
}

func TestEventAppendUnknownRun(t *testing.T) {
	store := newTestStore(t)
	log := NewEventLog(store.DB())
	_, err := log.Append(context.Background(), "no-such-run", "x", []byte("{}"))
	if !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("err = %v, want ErrRunNotFound", err)
	}
}

func TestEventAppendWithSeqRejectsRegression(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	runID := newTestRun(t, store)
	log := NewEventLog(store.DB())

	if _, err := log.Append(ctx, runID, "e", []byte("{}")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	err := store.WithTx(ctx, func(tx Tx) error {
		return log.AppendWithSeq(ctx, tx, Event{
			EventID: "fixed-1", RunID: runID, Seq: 1, Type: "replay", PayloadJSON: []byte("{}"),
		})
	})
	if !errors.Is(err, ErrSeqRegression) {
		t.Fatalf("err = %v, want ErrSeqRegression (I4)", err)
	}
	// seq 大于当前最大值时允许（replay 重放）
	if err := store.WithTx(ctx, func(tx Tx) error {
		return log.AppendWithSeq(ctx, tx, Event{
			EventID: "fixed-2", RunID: runID, Seq: 2, Type: "replay", PayloadJSON: []byte("{}"),
		})
	}); err != nil {
		t.Fatalf("AppendWithSeq(2): %v", err)
	}
}

// ---------------------------------------------------------------- outbox

func enqueueTestEvent(t *testing.T, store *Store, runID string, seq uint64) Event {
	t.Helper()
	ev := Event{
		EventID:     fmt.Sprintf("ev-%s-%d", runID, seq),
		RunID:       runID,
		Seq:         seq,
		Type:        "test.event",
		PayloadJSON: []byte(`{"k":"v"}`),
		CreatedAt:   time.Now().UnixMilli(),
	}
	ob := NewOutbox(store.DB())
	if err := store.WithTx(context.Background(), func(tx Tx) error {
		return ob.Enqueue(context.Background(), tx, ev)
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	return ev
}

func TestOutboxEnqueuePollAck(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	runID := newTestRun(t, store)
	ob := NewOutbox(store.DB())

	ev1 := enqueueTestEvent(t, store, runID, 1)
	ev2 := enqueueTestEvent(t, store, runID, 2)

	events, err := ob.PollUndelivered(ctx)
	if err != nil {
		t.Fatalf("PollUndelivered: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("polled %d, want 2", len(events))
	}
	if events[0].EventID != ev1.EventID || events[1].EventID != ev2.EventID {
		t.Errorf("poll order wrong: %s, %s", events[0].EventID, events[1].EventID)
	}

	// 认领后短期内不再返回（防止重复投递）
	again, err := ob.PollUndelivered(ctx)
	if err != nil {
		t.Fatalf("PollUndelivered 2: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("claimed events re-polled: %d", len(again))
	}

	if err := ob.Ack(ctx, ev1.EventID); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	depth, err := ob.Depth(ctx)
	if err != nil {
		t.Fatalf("Depth: %v", err)
	}
	if depth != 1 {
		t.Errorf("depth = %d, want 1 after ack", depth)
	}
	if err := ob.Ack(ctx, ev2.EventID); err != nil {
		t.Fatalf("Ack 2: %v", err)
	}
	depth, _ = ob.Depth(ctx)
	if depth != 0 {
		t.Errorf("depth = %d, want 0", depth)
	}
}

func TestOutboxEnqueueIsIdempotent(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	runID := newTestRun(t, store)
	ob := NewOutbox(store.DB())
	ev := enqueueTestEvent(t, store, runID, 1)
	// 同一事件重复入队不得报错（replay 场景）
	if err := store.WithTx(ctx, func(tx Tx) error {
		return ob.Enqueue(ctx, tx, ev)
	}); err != nil {
		t.Fatalf("duplicate Enqueue: %v", err)
	}
	depth, _ := ob.Depth(ctx)
	if depth != 1 {
		t.Errorf("depth = %d, want 1", depth)
	}
}

func TestOutboxMarkFailedAndPark(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	runID := newTestRun(t, store)
	ob := NewOutbox(store.DB())
	ev := enqueueTestEvent(t, store, runID, 1)

	if err := ob.MarkFailed(ctx, ev.EventID, "ipc down", 3, 10*time.Millisecond, time.Second); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	// 退避窗口内不返回
	events, err := ob.PollUndelivered(ctx)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("failed event polled during backoff: %d", len(events))
	}

	// 达到 maxAttempts 后停放
	for i := 0; i < 2; i++ {
		time.Sleep(15 * time.Millisecond)
		if err := ob.MarkFailed(ctx, ev.EventID, "ipc down", 3, 10*time.Millisecond, time.Second); err != nil {
			t.Fatalf("MarkFailed %d: %v", i, err)
		}
	}
	depth, _ := ob.Depth(ctx)
	if depth != 1 {
		t.Errorf("depth = %d, want 1 (parked rows retained)", depth)
	}
	// RetryParked 解除停放
	n, err := ob.RetryParked(ctx)
	if err != nil {
		t.Fatalf("RetryParked: %v", err)
	}
	if n != 1 {
		t.Errorf("retried = %d, want 1", n)
	}
	events, err = ob.PollUndelivered(ctx)
	if err != nil {
		t.Fatalf("poll after retry: %v", err)
	}
	if len(events) != 1 {
		t.Errorf("polled = %d, want 1 after RetryParked", len(events))
	}
}

// ---------------------------------------------------------------- dispatcher

func TestDispatcherDeliversAndAcks(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	runID := newTestRun(t, store)
	enqueueTestEvent(t, store, runID, 1)

	var mu sync.Mutex
	var delivered []Event
	d := NewDispatcher(NewOutbox(store.DB()), DeliverFunc(func(ctx context.Context, e Event) error {
		mu.Lock()
		delivered = append(delivered, e)
		mu.Unlock()
		return nil
	}), DispatcherConfig{Interval: 10 * time.Millisecond})

	if !d.Start() {
		t.Fatal("Start returned false")
	}
	defer func() {
		if err := d.Stop(context.Background()); err != nil {
			t.Fatalf("Stop: %v", err)
		}
	}()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		depth, _ := NewOutbox(store.DB()).Depth(ctx)
		if depth == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	got := len(delivered)
	mu.Unlock()
	if got != 1 {
		t.Fatalf("delivered = %d, want 1", got)
	}
	depth, _ := NewOutbox(store.DB()).Depth(ctx)
	if depth != 0 {
		t.Errorf("depth = %d, want 0 (ack after delivery)", depth)
	}
}

func TestDispatcherRetriesFailedDelivery(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	runID := newTestRun(t, store)
	enqueueTestEvent(t, store, runID, 1)

	attempts := 0
	var mu sync.Mutex
	d := NewDispatcher(NewOutbox(store.DB()), DeliverFunc(func(ctx context.Context, e Event) error {
		mu.Lock()
		attempts++
		mu.Unlock()
		return errors.New("ipc unreachable")
	}), DispatcherConfig{
		Interval:    10 * time.Millisecond,
		BackoffBase: 30 * time.Millisecond,
		MaxBackoff:  100 * time.Millisecond,
	})

	n, err := d.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n != 0 {
		t.Errorf("delivered = %d, want 0 (delivery failing)", n)
	}
	mu.Lock()
	first := attempts
	mu.Unlock()
	if first != 1 {
		t.Fatalf("attempts = %d, want 1", first)
	}

	// 退避期内不再投递
	if _, err := d.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce 2: %v", err)
	}
	mu.Lock()
	second := attempts
	mu.Unlock()
	if second != 1 {
		t.Errorf("attempts during backoff = %d, want 1", second)
	}

	// 退避结束后重试
	time.Sleep(60 * time.Millisecond)
	if _, err := d.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce 3: %v", err)
	}
	mu.Lock()
	third := attempts
	mu.Unlock()
	if third != 2 {
		t.Errorf("attempts after backoff = %d, want 2", third)
	}

	// 事件仍在 outbox 中（未送达不删除，等待重试/重启 replay）
	depth, _ := NewOutbox(store.DB()).Depth(ctx)
	if depth != 1 {
		t.Errorf("depth = %d, want 1 (undelivered event retained)", depth)
	}
}

func TestDispatcherReplayAfterRestart(t *testing.T) {
	// 模拟 Engine 崩溃重启：dispatcher 未来得及投递就退出，
	// 重启后新 dispatcher 必须 replay 未送达事件（第25章）。
	store := newTestStore(t)
	ctx := context.Background()
	runID := newTestRun(t, store)
	crashed := enqueueTestEvent(t, store, runID, 1)
	enqueueTestEvent(t, store, runID, 2)

	// 第一轮：只投递不 ack（模拟崩溃）
	d1 := NewDispatcher(NewOutbox(store.DB()), DeliverFunc(func(ctx context.Context, e Event) error {
		if e.EventID == crashed.EventID {
			return errors.New("crash before ack")
		}
		return nil
	}), DispatcherConfig{Interval: 10 * time.Millisecond, BackoffBase: 5 * time.Millisecond, MaxBackoff: 10 * time.Millisecond})
	if _, err := d1.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	// 重启后的 dispatcher：全部最终送达
	var mu sync.Mutex
	got := map[string]bool{}
	d2 := NewDispatcher(NewOutbox(store.DB()), DeliverFunc(func(ctx context.Context, e Event) error {
		mu.Lock()
		got[e.EventID] = true
		mu.Unlock()
		return nil
	}), DispatcherConfig{Interval: 10 * time.Millisecond, BackoffBase: 5 * time.Millisecond, MaxBackoff: 10 * time.Millisecond})

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		depth, _ := NewOutbox(store.DB()).Depth(ctx)
		if depth == 0 {
			break
		}
		if _, err := d2.RunOnce(ctx); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if !got[crashed.EventID] {
		t.Error("crashed event was not replayed after restart")
	}
	depth, _ := NewOutbox(store.DB()).Depth(ctx)
	if depth != 0 {
		t.Errorf("depth = %d, want 0 after replay", depth)
	}
}

// ---------------------------------------------------------------- integrity

func TestCheckIntegrity(t *testing.T) {
	store := newTestStore(t)
	if err := store.CheckIntegrity(context.Background()); err != nil {
		t.Fatalf("CheckIntegrity: %v", err)
	}
}
