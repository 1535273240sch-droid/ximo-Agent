package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/storage/sqlite"
)

// OutboxStore 是任务02/04 消费的外发箱接口（契约，勿改签名）。
// Enqueue 必须在调用方的事务内调用——event 与业务数据同事务落库，
// 这样即使 "DB committed 但 Engine 随后崩溃"，重启后仍能 replay
// 未送达事件（第25章）。
type OutboxStore interface {
	Enqueue(ctx context.Context, tx Tx, event Event) error
	PollUndelivered(ctx context.Context) ([]Event, error)
	Ack(ctx context.Context, eventID string) error
}

// Outbox 是基于 outbox 表的 OutboxStore 实现。
type Outbox struct {
	db         *sqlite.DB
	batchSize  int
	claimLease time.Duration
}

// NewOutbox 创建 Outbox。batchSize<=0 取默认 100，claimLease<=0 取 30s。
func NewOutbox(db *sqlite.DB) *Outbox {
	return &Outbox{db: db, batchSize: 100, claimLease: 30 * time.Second}
}

// parkedUntil 是“死信停放”时间戳（2100年）：超过 MaxAttempts 的事件被停放到
// 该时间，PollUndelivered 不再返回，但数据保留，可人工 RetryParked 重放。
const parkedUntil int64 = 4102444800000

// Enqueue 在事务内把事件写入 outbox。
func (o *Outbox) Enqueue(ctx context.Context, tx Tx, event Event) error {
	ex, ok := tx.(txExecer)
	if !ok {
		return fmt.Errorf("storage: unsupported Tx type %T", tx)
	}
	if event.EventID == "" {
		return errors.New("storage: outbox event missing event_id")
	}
	_, err := ex.ExecContext(ctx,
		`INSERT INTO outbox (event_id, run_id, seq, event_type, payload_json, enqueued_at, next_attempt_at)
		 VALUES (?,?,?,?,?,?,0)`,
		event.EventID, event.RunID, event.Seq, event.Type, event.PayloadJSON, event.CreatedAt)
	if err != nil {
		if IsUniqueViolation(err) {
			// 同一事件重复入队（重放场景）视为成功：幂等。
			return nil
		}
		return fmt.Errorf("storage: enqueue outbox: %w", err)
	}
	return nil
}

// PollUndelivered 取一批待送达事件并“认领”（把 next_attempt_at 推到
// now+claimLease），避免多个 dispatcher 重复投递；dispatcher 崩溃后
// 认领租约过期自动恢复可投递。
func (o *Outbox) PollUndelivered(ctx context.Context) ([]Event, error) {
	batch := o.batchSize
	if batch <= 0 {
		batch = 100
	}
	now := nowMS()
	leaseUntil := now + o.claimLease.Milliseconds()
	var events []Event
	err := o.db.WithTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx,
			`SELECT event_id, run_id, seq, event_type, payload_json, enqueued_at
			 FROM outbox WHERE next_attempt_at <= ? ORDER BY enqueued_at ASC, run_id ASC, seq ASC LIMIT ?`,
			now, batch)
		if err != nil {
			return fmt.Errorf("storage: poll outbox: %w", err)
		}
		for rows.Next() {
			var ev Event
			var enqueuedAt int64
			if err := rows.Scan(&ev.EventID, &ev.RunID, &ev.Seq, &ev.Type, &ev.PayloadJSON, &enqueuedAt); err != nil {
				rows.Close()
				return fmt.Errorf("storage: scan outbox: %w", err)
			}
			ev.CreatedAt = enqueuedAt
			events = append(events, ev)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("storage: iterate outbox: %w", err)
		}
		rows.Close()
		for _, ev := range events {
			if _, err := tx.ExecContext(ctx,
				"UPDATE outbox SET next_attempt_at=? WHERE event_id=?", leaseUntil, ev.EventID); err != nil {
				return fmt.Errorf("storage: claim outbox: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return events, nil
}

// Ack 删除已送达事件（送达即删除，未送达的保留待重启后 replay）。
// 必须在写事务之外调用（dispatcher 的典型用法）；嵌套在写事务里会触发
// sqlite.ErrNestedWrite。
func (o *Outbox) Ack(ctx context.Context, eventID string) error {
	return o.db.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "DELETE FROM outbox WHERE event_id=?", eventID)
		if err != nil {
			return fmt.Errorf("storage: ack outbox: %w", err)
		}
		return nil
	})
}

// MarkFailed 记录一次投递失败：attempts+1，按指数退避（base*2^(attempts-1)，
// 上限 maxBackoff）设置下次尝试时间。attempts 达到 maxAttempts（>0 时）
// 则停放到 parkedUntil。
func (o *Outbox) MarkFailed(ctx context.Context, eventID string, cause string, maxAttempts int, base, maxBackoff time.Duration) error {
	return o.db.WithTx(ctx, func(tx *sql.Tx) error {
		var attempts int64
		row := tx.QueryRowContext(ctx, "SELECT attempts FROM outbox WHERE event_id=?", eventID)
		if err := row.Scan(&attempts); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil // 已被 Ack，无需处理
			}
			return fmt.Errorf("storage: read outbox attempts: %w", err)
		}
		attempts++
		next := nowMS() + backoffFor(attempts, base, maxBackoff).Milliseconds()
		if maxAttempts > 0 && attempts >= int64(maxAttempts) {
			next = parkedUntil
		}
		if _, err := tx.ExecContext(ctx,
			"UPDATE outbox SET attempts=?, last_error=?, next_attempt_at=? WHERE event_id=?",
			attempts, cause, next, eventID); err != nil {
			return fmt.Errorf("storage: mark outbox failed: %w", err)
		}
		return nil
	})
}

// backoffFor 计算第 attempts 次失败后的退避时长。
func backoffFor(attempts int64, base, maxBackoff time.Duration) time.Duration {
	if base <= 0 {
		base = 100 * time.Millisecond
	}
	d := base
	for i := int64(1); i < attempts && d < maxBackoff; i++ {
		d *= 2
	}
	if maxBackoff > 0 && d > maxBackoff {
		d = maxBackoff
	}
	return d
}

// RetryParked 解除所有停放（死信）状态，立即重新可投递。
func (o *Outbox) RetryParked(ctx context.Context) (int64, error) {
	var n int64
	err := o.db.WithTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			"UPDATE outbox SET next_attempt_at=0, attempts=0, last_error=NULL WHERE next_attempt_at >= ?", parkedUntil)
		if err != nil {
			return fmt.Errorf("storage: retry parked: %w", err)
		}
		n, err = res.RowsAffected()
		return err
	})
	return n, err
}

// Depth 返回待送达事件数（可观测性指标）。
func (o *Outbox) Depth(ctx context.Context) (int, error) {
	var n int
	row := o.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM outbox")
	if err := row.Scan(&n); err != nil {
		return 0, fmt.Errorf("storage: outbox depth: %w", err)
	}
	return n, nil
}

// ---------------------------------------------------------------- dispatcher

// Deliverer 是把事件送达到外部（IPC → UI）的出口，由任务01（IPC 基座）实现。
type Deliverer interface {
	Deliver(ctx context.Context, event Event) error
}

// DeliverFunc 是函数式 Deliverer。
type DeliverFunc func(ctx context.Context, event Event) error

// Deliver 实现 Deliverer。
func (f DeliverFunc) Deliver(ctx context.Context, event Event) error { return f(ctx, event) }

// DispatcherConfig 控制后台 dispatcher 行为。
type DispatcherConfig struct {
	// Interval 轮询间隔，默认 50ms（UI 延迟与 CPU 占用的平衡点）。
	Interval time.Duration
	// BatchSize 单轮批量，默认 100（queue 容量上限）。
	BatchSize int
	// MaxAttempts 最大尝试次数，0 = 无限重试（默认）。超过则停放。
	MaxAttempts int
	// ClaimLease 认领租约，默认 30s。
	ClaimLease time.Duration
	// BackoffBase 退避基数，默认 100ms。
	BackoffBase time.Duration
	// MaxBackoff 退避上限，默认 1min。
	MaxBackoff time.Duration
	// OnError 投递失败回调（打点/日志用），可为 nil。
	OnError func(event Event, err error)
}

func (c *DispatcherConfig) withDefaults() DispatcherConfig {
	out := *c
	if out.Interval <= 0 {
		out.Interval = 50 * time.Millisecond
	}
	if out.BatchSize <= 0 {
		out.BatchSize = 100
	}
	if out.ClaimLease <= 0 {
		out.ClaimLease = 30 * time.Second
	}
	if out.BackoffBase <= 0 {
		out.BackoffBase = 100 * time.Millisecond
	}
	if out.MaxBackoff <= 0 {
		out.MaxBackoff = time.Minute
	}
	return out
}

// Dispatcher 是 outbox 的后台投递循环：poll → deliver → ack，
// 失败按指数退避重试，进程重启后自动 replay 未送达事件。
type Dispatcher struct {
	outbox  *Outbox
	deliver Deliverer
	cfg     DispatcherConfig

	mu      sync.Mutex
	running bool
	stop    chan struct{}
	done    chan struct{}

	delivered atomic.Int64
	failed    atomic.Int64
}

// NewDispatcher 创建 dispatcher（未启动）。
func NewDispatcher(outbox *Outbox, deliverer Deliverer, cfg DispatcherConfig) *Dispatcher {
	return &Dispatcher{
		outbox:  outbox,
		deliver: deliverer,
		cfg:     cfg.withDefaults(),
	}
}

// Start 启动后台循环。重复 Start 返回 false。
func (d *Dispatcher) Start() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.running {
		return false
	}
	d.running = true
	d.stop = make(chan struct{})
	d.done = make(chan struct{})
	go d.loop()
	return true
}

// Stop 停止循环并等待在途投递完成。ctx 超时返回错误。
func (d *Dispatcher) Stop(ctx context.Context) error {
	d.mu.Lock()
	if !d.running {
		d.mu.Unlock()
		return nil
	}
	stop, done := d.stop, d.done
	d.running = false
	d.mu.Unlock()

	close(stop)
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Running 返回是否运行中。
func (d *Dispatcher) Running() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.running
}

// Stats 返回投递计数。
func (d *Dispatcher) Stats() (delivered, failed int64) {
	return d.delivered.Load(), d.failed.Load()
}

func (d *Dispatcher) loop() {
	defer close(d.done)
	ticker := time.NewTicker(d.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-d.stop:
			return
		case <-ticker.C:
			if _, err := d.RunOnce(context.Background()); err != nil && d.cfg.OnError != nil {
				d.cfg.OnError(Event{}, fmt.Errorf("dispatcher run: %w", err))
			}
		}
	}
}

// RunOnce 执行一轮投递（测试与手动触发用）。返回本轮成功投递数。
func (d *Dispatcher) RunOnce(ctx context.Context) (int, error) {
	if d.deliver == nil {
		return 0, errors.New("storage: dispatcher has no deliverer")
	}
	events, err := d.outbox.PollUndelivered(ctx)
	if err != nil {
		return 0, err
	}
	delivered := 0
	for _, ev := range events {
		if err := ctx.Err(); err != nil {
			return delivered, err
		}
		if err := d.deliver.Deliver(ctx, ev); err != nil {
			d.failed.Add(1)
			if d.cfg.OnError != nil {
				d.cfg.OnError(ev, err)
			}
			_ = d.outbox.MarkFailed(ctx, ev.EventID, err.Error(), d.cfg.MaxAttempts, d.cfg.BackoffBase, d.cfg.MaxBackoff)
			continue
		}
		if err := d.outbox.Ack(ctx, ev.EventID); err != nil {
			d.failed.Add(1)
			if d.cfg.OnError != nil {
				d.cfg.OnError(ev, err)
			}
			continue
		}
		d.delivered.Add(1)
		delivered++
	}
	return delivered, nil
}
