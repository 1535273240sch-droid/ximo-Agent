package worker

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// ============================== 租约（Lease） ==============================
//
// 租约模型（第 16 章）：Acquire → use → heartbeat → release。
// 租约超时由 Manager 的维护循环强制回收：force cleanup → worker recycle。
//
// 为什么需要 heartbeat 而不只是超时：一次 browser_navigate 可能合法地跑 30 秒，
// 一次 dynamic-js 可能跑 10 秒。如果只按"固定 TTL"判断，长调用会被误杀；如果只按
// "调用返回才释放"，那么调用卡死（CDP 卡住、goja 死循环）时 Worker 永远不会被回收。
// 因此：持有方必须周期性 Heartbeat 续约，Manager 只在"心跳停止超过 TTL"时动手。

// leaseIDSeq 生成全局单调租约 ID，便于日志串联。
var leaseIDSeq atomic.Uint64

// Lease 是对一个 Worker 槽位的独占使用权。
//
// 使用约定（照抄 v1 的 Acquire/use/heartbeat/release 语义，但补上超时强制回收）：
//
//	lease, err := mgr.Acquire(ctx, "browser")
//	if err != nil { return err }
//	defer lease.Release(ctx)          // 即使 panic 也要通过 Manager 的 recover 兜底释放
//	resp, err := lease.Execute(ctx, req)
//
// 也可以手动长时间持有：Acquire 后开一个 goroutine 定期 Heartbeat，用完 Release。
type Lease struct {
	id       string
	workerID string
	kind     string
	slotIdx  int
	mgr      *Manager
	clock    Clock

	ttl time.Duration
	// lastHeartbeat 是最近一次（含自动）心跳时间。Manager 据此判断是否过期。
	lastHeartbeat time.Time
	// expiresAt = lastHeartbeat + ttl，由 Manager 读取。
	expiresAt time.Time

	mu         sync.Mutex
	released   bool
	expired    bool
	cancelExec context.CancelFunc
	executing  bool
	// autoHeartbeat 为 true 时，Lease 会在 Execute/后台自动续约。
	autoHeartbeat bool
	stopHB        chan struct{}
	hbOnce        sync.Once
	// hbWG 保证心跳 goroutine 在 Release 返回前退出（验收标准：goroutine 都有退出路径）。
	hbWG sync.WaitGroup
}

// ID 返回租约 ID。
func (l *Lease) ID() string { return l.id }

// WorkerID 返回承载该租约的 Worker 实例 ID。
func (l *Lease) WorkerID() string { return l.workerID }

// Kind 返回 Worker 类型。
func (l *Lease) Kind() string { return l.kind }

// Heartbeat 续约。租约已释放或已过期时返回错误（调用方应立即停止使用该 Worker）。
func (l *Lease) Heartbeat() error {
	if l.mgr == nil {
		return fmt.Errorf("%w: 租约未绑定 Manager", ErrInternal)
	}
	return l.mgr.heartbeatLease(l)
}

// Expired 报告租约是否已被判定过期。
func (l *Lease) Expired() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.expired
}

// IsExpiredAt 报告在指定时间点租约是否已过期且未释放（线程安全）。
func (l *Lease) IsExpiredAt(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return !l.released && now.After(l.expiresAt)
}

// ExpiresAt 返回租约过期时间（线程安全）。
func (l *Lease) ExpiresAt() time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.expiresAt
}

// Released 报告租约是否已释放。
func (l *Lease) Released() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.released
}

// Execute 在租约保护的 Worker 上执行一次调用。
//
// 语义要点：
//   - 调用前后自动 heartBeat，避免长调用期间被误判过期。
//   - 执行中若 Manager 判定租约过期，会 cancel 该 ctx，Execute 快速返回并触发 worker recycle。
//   - 返回的 error 只在"无法通过响应表达"时非空（池不可用、租约失效）；业务失败以
//     WorkerResponse.Error 形式返回，与任务 04 的 ToolResponse 对齐。
func (l *Lease) Execute(ctx context.Context, req WorkerRequest) (WorkerResponse, error) {
	if err := l.Heartbeat(); err != nil {
		return WorkerResponse{}, err
	}
	// 执行期间使用可取消的子 context，供租约过期/停机时中断。
	execCtx, cancel := context.WithCancel(ctx)
	l.mu.Lock()
	if l.released || l.expired {
		l.mu.Unlock()
		cancel()
		return WorkerResponse{}, l.leaseErrLocked()
	}
	l.cancelExec = cancel
	l.executing = true
	l.mu.Unlock()

	defer func() {
		l.mu.Lock()
		l.executing = false
		l.cancelExec = nil
		l.mu.Unlock()
		cancel()
	}()

	resp, err := l.mgr.executeOnSlot(execCtx, l, req)

	// 调用结束后再次续约：即便这次调用跑得比 TTL 还长，也不该因为"忘了心跳"而回收。
	if hbErr := l.Heartbeat(); hbErr != nil && err == nil {
		return resp, hbErr
	}
	return resp, err
}

// leaseErrLocked 返回租约失效的具体原因。调用方必须持锁。
func (l *Lease) leaseErrLocked() error {
	if l.expired {
		return fmt.Errorf("%w: %s", ErrLeaseExpired, l.id)
	}
	if l.released {
		return fmt.Errorf("%w: %s", ErrLeaseReleased, l.id)
	}
	return nil
}

// Release 归还租约。幂等：重复调用返回 nil。
func (l *Lease) Release(ctx context.Context) error {
	l.mu.Lock()
	already := l.released
	l.released = true
	cancel := l.cancelExec
	l.mu.Unlock()
	if already {
		return nil
	}
	if cancel != nil {
		cancel()
	}
	l.stopHeartbeat()
	if l.mgr != nil {
		return l.mgr.releaseLease(ctx, l)
	}
	return nil
}

// startAutoHeartbeat 启动后台自动续约。Manager 在 Acquire 时调用。
func (l *Lease) startAutoHeartbeat(interval time.Duration) {
	if interval <= 0 {
		return
	}
	l.autoHeartbeat = true
	l.stopHB = make(chan struct{})
	l.hbWG.Add(1)
	go func() {
		defer l.hbWG.Done()
		tk := l.clock.NewTicker(interval)
		defer tk.Stop() // 所有 ticker 都有 Stop
		for {
			select {
			case <-l.stopHB:
				return
			case <-tk.C():
				// 续约失败（例如已被回收）就退出，不做无意义重试。
				if err := l.mgr.heartbeatLease(l); err != nil {
					return
				}
			}
		}
	}()
}

// stopHeartbeat 停止后台续约并等待其退出，保证不泄漏 goroutine。
func (l *Lease) stopHeartbeat() {
	if !l.autoHeartbeat {
		return
	}
	l.hbOnce.Do(func() {
		if l.stopHB != nil {
			close(l.stopHB)
		}
	})
	l.hbWG.Wait()
}

// LeaseOption 用于调整 Acquire 行为。
type LeaseOption func(*leaseOptions)

type leaseOptions struct {
	ttl           time.Duration
	autoHeartbeat bool
	hbInterval    time.Duration
}

// WithLeaseTTL 覆盖默认租约 TTL。TTL 必须显著大于心跳间隔，建议 TTL >= 3*interval。
func WithLeaseTTL(ttl time.Duration) LeaseOption {
	return func(o *leaseOptions) { o.ttl = ttl }
}

// WithAutoHeartbeat 控制是否自动心跳（默认开启，间隔为 TTL/3）。
func WithAutoHeartbeat(enabled bool) LeaseOption {
	return func(o *leaseOptions) { o.autoHeartbeat = enabled }
}
