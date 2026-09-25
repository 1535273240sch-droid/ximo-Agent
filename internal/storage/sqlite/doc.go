// Package sqlite 实现 SQLite 连接管理：多读连接池 + 单写队列（第10章）。
//
// 设计约束（第10.1章）：不允许上百个 goroutine 各自 db.Exec()。所有写操作
// 必须经过 WriteTx/WithTx 提交到单写队列，由唯一的 writer goroutine 串行
// 执行；读操作走读连接池（WAL 模式下读写不互斥）。
//
// PRAGMA 策略（第10.2章）：journal_mode=WAL、synchronous=FULL、
// foreign_keys=ON、busy_timeout、wal_autocheckpoint 通过 DSN 在每次建连时
// 应用，保证池里每条连接都带同一套参数。
package sqlite

import "errors"

// DriverName 是注册的 database/sql 驱动名（modernc.org/sqlite，纯 Go 无 cgo）。
const DriverName = "sqlite"

var (
	// ErrClosed 表示 DB 已关闭，不再接受新的读写。
	ErrClosed = errors.New("sqlite: database closed")
	// ErrWriteTxFinished 表示写事务已提交或回滚，不能再执行操作。
	ErrWriteTxFinished = errors.New("sqlite: write transaction already finished")
	// ErrNestedWrite 表示在写事务内部又发起了写操作。单写队列模型下这必然
	// 死等（writer 正忙着执行外层事务），因此直接拒绝而不是挂起。
	ErrNestedWrite = errors.New("sqlite: nested write on the single write queue")
)

// PanicError 包裹写队列执行过程中 recover 到的 panic。
// writer goroutine 不因用户代码 panic 而退出：事务回滚、错误返回调用方、
// 进程保持存活（故障隔离，而不是全局兜底继续跑）。
type PanicError struct {
	Value any
	Stack []byte
}

func (e *PanicError) Error() string { return "sqlite: panic in write transaction" }
