package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite" // 注册 "sqlite" 驱动
)

// Config 是打开数据库的参数。
type Config struct {
	// Path 数据库文件路径（目录不存在会自动创建）。
	Path string
	// Profile PRAGMA 档位，默认 ProfileBalanced。
	Profile Profile
	// ReadConns 读连接数，<=0 时取档位默认值。
	ReadConns int
	// WriteQueueCap 写队列容量上限，<=0 时取 DefaultWriteQueueCap。
	WriteQueueCap int
	// ExtraPragmas 附加 PRAGMA（测试注入 max_page_count 等场景）。
	ExtraPragmas []string
}

// DefaultConfig 返回默认（balanced）配置。
func DefaultConfig(path string) Config {
	return Config{Path: path, Profile: ProfileBalanced}
}

// Stats 是连接池/写队列的运行指标（可观测性埋点用）。
type Stats struct {
	Profile       string
	ReadOpen      int
	ReadInUse     int
	ReadIdle      int
	WriteOpen     int
	WriteInUse    int
	WriteIdle     int
	QueueLen      int
	QueueCap      int
	OpenWriteTxs  int64
	TotalWrites   uint64
	FailedWrites  uint64
	TotalReads    uint64
	PanicsInWrite uint64
}

// DB 是 SQLite 连接门面：读走连接池，写走单写队列。
type DB struct {
	cfg     Config
	pragmas []string

	writeDB *sql.DB // MaxOpenConns=1，仅供 writer goroutine 使用
	readDB  *sql.DB // 读连接池

	queue chan *req
	quit  chan struct{}

	closeOnce  sync.Once
	closed     atomic.Bool
	writerDone chan struct{}
	writerGID  atomic.Uint64

	openWriteTxs  atomic.Int64
	totalWrites   atomic.Uint64
	failedWrites  atomic.Uint64
	totalReads    atomic.Uint64
	panicsInWrite atomic.Uint64
}

// req 是提交给 writer goroutine 的一个执行单元。
type req struct {
	ctx  context.Context
	fn   func(ctx context.Context) error
	resp chan error
}

func (r *req) reply(err error) {
	select {
	case r.resp <- err:
	default:
	}
}

// Open 打开（或创建）数据库并启动 writer goroutine。
func Open(cfg Config) (*DB, error) {
	if cfg.Path == "" {
		return nil, fmt.Errorf("sqlite: empty database path")
	}
	if cfg.Path != ":memory:" {
		if dir := filepath.Dir(cfg.Path); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, fmt.Errorf("sqlite: create data dir: %w", err)
			}
		}
	}
	if cfg.ReadConns <= 0 {
		cfg.ReadConns = cfg.Profile.spec().readConns
	}
	if cfg.WriteQueueCap <= 0 {
		cfg.WriteQueueCap = DefaultWriteQueueCap
	}

	spec := cfg.Profile.spec()
	pragmas := append(commonPragmas(spec), cfg.ExtraPragmas...)

	db := &DB{
		cfg:        cfg,
		pragmas:    pragmas,
		queue:      make(chan *req, cfg.WriteQueueCap),
		quit:       make(chan struct{}),
		writerDone: make(chan struct{}),
	}

	writeDB, err := sql.Open(DriverName, buildDSN(cfg.Path, pragmas, true))
	if err != nil {
		return nil, fmt.Errorf("sqlite: open write conn: %w", err)
	}
	writeDB.SetMaxOpenConns(1)
	writeDB.SetMaxIdleConns(1)
	writeDB.SetConnMaxLifetime(0)
	db.writeDB = writeDB

	readDB, err := sql.Open(DriverName, buildDSN(cfg.Path, pragmas, false))
	if err != nil {
		writeDB.Close()
		return nil, fmt.Errorf("sqlite: open read pool: %w", err)
	}
	readDB.SetMaxOpenConns(cfg.ReadConns)
	readDB.SetMaxIdleConns(cfg.ReadConns)
	readDB.SetConnMaxLifetime(0)
	db.readDB = readDB

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := db.verifyPragmas(ctx); err != nil {
		db.writeDB.Close()
		db.readDB.Close()
		return nil, err
	}

	go db.writerLoop()
	return db, nil
}

// verifyPragmas 在打开时校验 PRAGMA 实际生效，fail-fast，而不是等到第一次
// 写失败才发现配置没应用上。
func (db *DB) verifyPragmas(ctx context.Context) error {
	var errs []error
	check := func(name string, want any, got func() (any, error)) {
		v, err := got()
		if err != nil {
			errs = append(errs, fmt.Errorf("pragma %s: %w", name, err))
			return
		}
		if fmt.Sprint(v) != fmt.Sprint(want) {
			errs = append(errs, fmt.Errorf("pragma %s = %v, want %v", name, v, want))
		}
	}

	// journal_mode 是数据库级持久属性，读连接上验证即可。
	check("journal_mode", "wal", func() (any, error) {
		var mode string
		err := db.readDB.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode)
		return mode, err
	})
	spec := db.cfg.Profile.spec()
	syncWant := int64(2) // FULL
	switch spec.synchronous {
	case "NORMAL":
		syncWant = 1
	case "EXTRA":
		syncWant = 3
	}
	for _, pair := range []struct {
		label string
		pool  *sql.DB
	}{{"read", db.readDB}, {"write", db.writeDB}} {
		label, pool := pair.label, pair.pool
		check(label+" foreign_keys", int64(1), func() (any, error) {
			var v int64
			err := pool.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&v)
			return v, err
		})
		check(label+" synchronous", syncWant, func() (any, error) {
			var v int64
			err := pool.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&v)
			return v, err
		})
		check(label+" busy_timeout", int64(spec.busyTimeoutMS), func() (any, error) {
			var v int64
			err := pool.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&v)
			return v, err
		})
	}
	if err := db.writeDB.PingContext(ctx); err != nil {
		errs = append(errs, fmt.Errorf("ping write conn: %w", err))
	}
	if err := db.readDB.PingContext(ctx); err != nil {
		errs = append(errs, fmt.Errorf("ping read pool: %w", err))
	}
	if len(errs) > 0 {
		return fmt.Errorf("sqlite: pragma verification failed: %v", errs)
	}
	return nil
}

// writerLoop 是唯一的写事务执行 goroutine（第10.1章单 Writer）。
func (db *DB) writerLoop() {
	db.writerGID.Store(goroutineID())
	defer close(db.writerDone)
	for {
		select {
		case r := <-db.queue:
			db.execute(r)
		case <-db.quit:
			// 关闭：先把队列里已排队的请求以 ErrClosed 答复掉，
			// 保证没有调用方被永久挂起。
			for {
				select {
				case r := <-db.queue:
					r.reply(ErrClosed)
				default:
					return
				}
			}
		}
	}
}

// execute 在 writer goroutine 上执行一个请求。用户代码 panic 被隔离：
// 事务回滚、panic 包装为错误返回、writer goroutine 存活（进程不崩）。
func (db *DB) execute(r *req) {
	if err := r.ctx.Err(); err != nil {
		r.reply(err)
		return
	}
	db.totalWrites.Add(1)
	err := db.runGuarded(r.ctx, r.fn)
	if err != nil {
		db.failedWrites.Add(1)
		var pe *PanicError
		if errors.As(err, &pe) {
			db.panicsInWrite.Add(1)
		}
	}
	r.reply(err)
}

func (db *DB) runGuarded(ctx context.Context, fn func(context.Context) error) (err error) {
	defer func() {
		if v := recover(); v != nil {
			err = &PanicError{Value: v, Stack: debug.Stack()}
		}
	}()
	return fn(ctx)
}

// submit 把请求送入写队列。队列满时阻塞（背压），ctx 取消或 DB 关闭时返回错误。
// 如果调用方就是 writer goroutine 自己（嵌套写），直接返回 ErrNestedWrite——
// 否则会死等自己，把挂起变成明确错误。
func (db *DB) submit(ctx context.Context, fn func(context.Context) error) error {
	if db.closed.Load() {
		return ErrClosed
	}
	if gid := goroutineID(); gid != 0 && gid == db.writerGID.Load() {
		return ErrNestedWrite
	}
	r := &req{ctx: ctx, fn: fn, resp: make(chan error, 1)}
	select {
	case db.queue <- r:
	case <-db.quit:
		return ErrClosed
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-r.resp:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// WithTx 在写队列上执行一个完整事务：BEGIN → fn → COMMIT，fn 返回错误或
// panic 时 ROLLBACK。所有语句都在唯一的 writer goroutine 上串行执行，
// 因此多个 WithTx 之间天然串行化，不会出现两个写者互相 SQLITE_BUSY。
//
// fn 内部不允许再调用 WithTx/BeginWrite（会死等写队列）。
func (db *DB) WithTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	return db.submit(ctx, func(ctx context.Context) error {
		tx, err := db.writeDB.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("sqlite: begin: %w", err)
		}
		committed := false
		defer func() {
			if !committed {
				_ = tx.Rollback()
			}
		}()
		if err := fn(tx); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("sqlite: commit: %w", err)
		}
		committed = true
		return nil
	})
}

// BeginWrite 开启一个跨调用保持的写事务。返回的 WriteTx 的每个方法都会
// 被派发到 writer goroutine 执行；必须调用 Commit 或 Rollback 释放写队列
// （否则其他写操作全部阻塞）。ctx 取消后事务强制回滚。
func (db *DB) BeginWrite(ctx context.Context) (*WriteTx, error) {
	if db.closed.Load() {
		return nil, ErrClosed
	}
	db.openWriteTxs.Add(1)
	var (
		tx  *sql.Tx
		err error
	)
	submitErr := db.submit(ctx, func(ctx context.Context) error {
		tx, err = db.writeDB.BeginTx(ctx, nil)
		return err
	})
	if submitErr != nil {
		db.openWriteTxs.Add(-1)
		return nil, submitErr
	}
	if err != nil {
		db.openWriteTxs.Add(-1)
		return nil, fmt.Errorf("sqlite: begin: %w", err)
	}
	return &WriteTx{db: db, tx: tx}, nil
}

// WriteTx 是跨调用保持的写事务。所有方法在 writer goroutine 上执行。
type WriteTx struct {
	db       *DB
	tx       *sql.Tx
	finished bool
}

func (t *WriteTx) call(ctx context.Context, fn func() error) error {
	if t.finished {
		return ErrWriteTxFinished
	}
	return t.db.submit(ctx, func(context.Context) error {
		if t.finished {
			return ErrWriteTxFinished
		}
		return fn()
	})
}

// ExecContext 在写事务内执行语句。
func (t *WriteTx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	var res sql.Result
	err := t.call(ctx, func() error {
		var e error
		res, e = t.tx.ExecContext(ctx, query, args...)
		return e
	})
	return res, err
}

// QueryContext 在写事务内查询（同一事务内可见未提交数据）。
func (t *WriteTx) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	var rows *sql.Rows
	err := t.call(ctx, func() error {
		var e error
		rows, e = t.tx.QueryContext(ctx, query, args...)
		return e
	})
	return rows, err
}

// Commit 提交写事务。
func (t *WriteTx) Commit() error {
	return t.call(context.Background(), func() error {
		if err := t.tx.Commit(); err != nil {
			return fmt.Errorf("sqlite: commit: %w", err)
		}
		t.finished = true
		t.db.openWriteTxs.Add(-1)
		return nil
	})
}

// Rollback 回滚写事务。
func (t *WriteTx) Rollback() error {
	return t.call(context.Background(), func() error {
		err := t.tx.Rollback()
		t.finished = true
		t.db.openWriteTxs.Add(-1)
		if err != nil {
			return fmt.Errorf("sqlite: rollback: %w", err)
		}
		return nil
	})
}

// ---------------------------------------------------------------- 读路径

// Row 是单行查询结果。DB 关闭时也能安全返回（Scan 直接得到错误）。
type Row struct {
	err error
	row *sql.Row
}

// Scan 扫描单行结果。
func (r Row) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	return r.row.Scan(dest...)
}

// QueryContext 走读连接池执行查询。
func (db *DB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if db.closed.Load() {
		return nil, ErrClosed
	}
	db.totalReads.Add(1)
	return db.readDB.QueryContext(ctx, query, args...)
}

// QueryRowContext 走读连接池执行单行查询。
func (db *DB) QueryRowContext(ctx context.Context, query string, args ...any) Row {
	if db.closed.Load() {
		return Row{err: ErrClosed}
	}
	db.totalReads.Add(1)
	return Row{row: db.readDB.QueryRowContext(ctx, query, args...)}
}

// ReadDB 暴露底层读连接池（repository 层复杂查询用）。
func (db *DB) ReadDB() *sql.DB { return db.readDB }

// Profile 返回当前档位。
func (db *DB) Profile() Profile { return db.cfg.Profile }

// Stats 返回运行指标。
func (db *DB) Stats() Stats {
	rs, ws := db.readDB.Stats(), db.writeDB.Stats()
	return Stats{
		Profile:       db.cfg.Profile.String(),
		ReadOpen:      rs.OpenConnections,
		ReadInUse:     rs.InUse,
		ReadIdle:      rs.Idle,
		WriteOpen:     ws.OpenConnections,
		WriteInUse:    ws.InUse,
		WriteIdle:     ws.Idle,
		QueueLen:      len(db.queue),
		QueueCap:      cap(db.queue),
		OpenWriteTxs:  db.openWriteTxs.Load(),
		TotalWrites:   db.totalWrites.Load(),
		FailedWrites:  db.failedWrites.Load(),
		TotalReads:    db.totalReads.Load(),
		PanicsInWrite: db.panicsInWrite.Load(),
	}
}

// Ping 探测读写两端连通性。
func (db *DB) Ping(ctx context.Context) error {
	if err := db.readDB.PingContext(ctx); err != nil {
		return err
	}
	return db.writeDB.PingContext(ctx)
}

// Close 关闭数据库：先停止写队列（在途请求全部以 ErrClosed 答复），
// 等待 writer goroutine 退出，再关闭读写连接池。
func (db *DB) Close() error {
	var err error
	db.closeOnce.Do(func() {
		db.closed.Store(true)
		close(db.quit)
		select {
		case <-db.writerDone:
		case <-time.After(30 * time.Second):
			err = fmt.Errorf("sqlite: writer goroutine did not exit in time")
		}
		if e := db.readDB.Close(); e != nil && err == nil {
			err = e
		}
		if e := db.writeDB.Close(); e != nil && err == nil {
			err = e
		}
	})
	return err
}

// goroutineID 返回当前 goroutine 的 ID（runtime.Stack 解析，仅在写路径调用，
// 开销相对一次数据库事务可忽略）。用于识别"嵌套写"——写事务 fn 在 writer
// goroutine 上执行，若同一 goroutine 又向写队列提交请求，必然死等。
func goroutineID() uint64 {
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	s := buf[:n]
	const prefix = "goroutine "
	if !bytes.HasPrefix(s, []byte(prefix)) {
		return 0
	}
	s = s[len(prefix):]
	if i := bytes.IndexByte(s, ' '); i > 0 {
		if id, err := strconv.ParseUint(string(s[:i]), 10, 64); err == nil {
			return id
		}
	}
	return 0
}
