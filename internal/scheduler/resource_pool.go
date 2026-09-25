package scheduler

import (
	"container/list"
	"context"
	"sort"
	"sync"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// ResourcePool is the third layer of rate limiting: a bounded semaphore per
// resource class, so a burst of browser work cannot consume the whole worker
// pool and starve terminal calls.
//
// Capacities are fixed by architecture doc ch. 6 (browser=4, computer_use=2,
// terminal=16, office=4, mcp=16, vision=8, provider_requests=32). Acquisition
// is FIFO per class so a later caller cannot jump ahead of an older waiter.
type ResourcePool struct {
	mu       sync.Mutex
	caps     map[types.ResourceClass]int
	inUse    map[types.ResourceClass]int
	waiters  map[types.ResourceClass]*list.List
	acquired map[types.ResourceClass]uint64
	rejected map[types.ResourceClass]uint64
	timedOut map[types.ResourceClass]uint64
	// maxWaiters caps how many callers may block on one class. Without it a
	// thundering herd on a saturated class would grow memory without bound.
	maxWaiters int
	// closed rejects new acquisitions during shutdown.
	closed bool
}

// Lease is a held resource allocation. Always release what Acquire returns.
type Lease struct {
	class    types.ResourceClass
	pool     *ResourcePool
	released bool
}

// Class returns the resource class of this lease.
func (l *Lease) Class() types.ResourceClass { return l.class }

// Release returns the resource to the pool. It is idempotent, so a deferred
// release plus an explicit early release cannot double-free the capacity.
func (l *Lease) Release() {
	if l == nil || l.pool == nil || l.released {
		return
	}
	l.released = true
	l.pool.release(l.class)
}

// grant carries the outcome of a wait. The slot is transferred by the releaser
// *before* the grant is delivered, so the receiver must never re-increment.
type grant struct {
	// ok is false when the pool closed or the wait timed out.
	ok  bool
	err error
}

// waiter is one blocked Acquire.
type waiter struct {
	elem  *list.Element
	ch    chan grant
	class types.ResourceClass
}

// NewResourcePool builds a pool with the given capacities. A class absent from
// caps gets its types.ResourceCapacities value.
func NewResourcePool(caps map[types.ResourceClass]int, maxWaiters int) *ResourcePool {
	if maxWaiters <= 0 {
		maxWaiters = 1024
	}
	eff := make(map[types.ResourceClass]int, len(types.AllResourceClasses))
	for _, r := range types.AllResourceClasses {
		eff[r] = types.ResourceCapacities[r]
	}
	for r, c := range caps {
		eff[r] = c
	}
	p := &ResourcePool{
		caps:       eff,
		inUse:      make(map[types.ResourceClass]int, len(eff)),
		waiters:    make(map[types.ResourceClass]*list.List, len(eff)),
		acquired:   make(map[types.ResourceClass]uint64, len(eff)),
		rejected:   make(map[types.ResourceClass]uint64, len(eff)),
		timedOut:   make(map[types.ResourceClass]uint64, len(eff)),
		maxWaiters: maxWaiters,
	}
	for r := range eff {
		p.waiters[r] = list.New()
	}
	return p
}

// TryAcquire takes the resource without blocking. It returns false when the
// class is saturated, letting the caller defer rather than wait.
func (p *ResourcePool) TryAcquire(class types.ResourceClass) (*Lease, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, false
	}
	cap, ok := p.caps[class]
	if !ok {
		return nil, false
	}
	if p.inUse[class] >= cap {
		p.rejected[class]++
		return nil, false
	}
	p.inUse[class]++
	p.acquired[class]++
	return &Lease{class: class, pool: p}, true
}

// Acquire blocks until the resource is free, ctx is done, or the pool closes.
// Waiting is FIFO and bounded by maxWaiters; a caller that finds the wait list
// full is rejected instead of queued, preserving the required hard ceiling.
func (p *ResourcePool) Acquire(ctx context.Context, class types.ResourceClass) (*Lease, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, types.WrapError(types.CodeDeadlineExceeded, err,
			"context already done acquiring resource %q", string(class))
	}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, types.NewError(types.CodeEngineClosed, "resource pool is closed")
	}
	cap, ok := p.caps[class]
	if !ok {
		p.mu.Unlock()
		return nil, types.NewError(types.CodeInvalidArgument, "unknown resource class %q", string(class))
	}
	// Fast path under the lock: no waiter allocation needed.
	if p.inUse[class] < cap {
		p.inUse[class]++
		p.acquired[class]++
		p.mu.Unlock()
		return &Lease{class: class, pool: p}, nil
	}
	if p.waiters[class].Len() >= p.maxWaiters {
		p.rejected[class]++
		n := p.waiters[class].Len()
		p.mu.Unlock()
		return nil, types.NewError(types.CodeResourceUnavailable,
			"too many waiters for resource %q (%d)", string(class), n)
	}

	w := &waiter{class: class, ch: make(chan grant, 1)}
	w.elem = p.waiters[class].PushBack(w)
	p.mu.Unlock()

	select {
	case g := <-w.ch:
		if !g.ok {
			return nil, g.err
		}
		return &Lease{class: class, pool: p}, nil
	case <-ctx.Done():
		p.mu.Lock()
		// Either a releaser already transferred a slot to us asynchronously,
		// or our element is still queued. Only one of the two can be true,
		// because both paths take this lock.
		select {
		case g := <-w.ch:
			// We were granted the slot concurrently: give it back rather than
			// leaking capacity.
			if g.ok {
				p.inUse[class]--
				p.handOffLocked(class)
			}
		default:
			if w.elem != nil {
				p.waiters[class].Remove(w.elem)
				w.elem = nil
			}
		}
		p.timedOut[class]++
		p.mu.Unlock()
		return nil, types.WrapError(types.CodeDeadlineExceeded, ctx.Err(),
			"timed out waiting for resource %q", string(class))
	}
}

// release returns one unit of capacity and hands it to the oldest waiter.
func (p *ResourcePool) release(class types.ResourceClass) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.inUse[class] > 0 {
		p.inUse[class]--
	}
	p.handOffLocked(class)
}

// handOffLocked transfers one free slot to the oldest waiter, if any. The slot
// is consumed on transfer (inUse is left unchanged: it moves from the releaser
// to the waiter), so the cap can never be exceeded. Caller must hold the lock.
func (p *ResourcePool) handOffLocked(class types.ResourceClass) {
	cap, ok := p.caps[class]
	if !ok {
		return
	}
	if p.closed {
		return
	}
	if p.inUse[class] >= cap {
		return
	}
	wl := p.waiters[class]
	e := wl.Front()
	if e == nil {
		return
	}
	w := e.Value.(*waiter)
	wl.Remove(e)
	w.elem = nil
	p.inUse[class]++
	w.ch <- grant{ok: true}
}

// Close rejects new acquisitions and fails every waiter with a closed error.
func (p *ResourcePool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	p.closed = true
	for class, wl := range p.waiters {
		for e := wl.Front(); e != nil; e = e.Next() {
			w := e.Value.(*waiter)
			w.ch <- grant{ok: false,
				err: types.NewError(types.CodeEngineClosed,
					"resource pool closed while waiting for %q", string(class))}
		}
		wl.Init()
	}
}

// Closed reports whether the pool has been closed.
func (p *ResourcePool) Closed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

// InUse reports current utilisation for one class.
func (p *ResourcePool) InUse(class types.ResourceClass) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.inUse[class]
}

// Capacity reports the configured capacity for one class.
func (p *ResourcePool) Capacity(class types.ResourceClass) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.caps[class]
}

// Waiting reports how many callers are blocked on one class.
func (p *ResourcePool) Waiting(class types.ResourceClass) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.waiters[class].Len()
}

// Stats snapshots every class in a stable order.
func (p *ResourcePool) Stats() []types.ResourceStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	classes := make([]types.ResourceClass, 0, len(p.caps))
	for r := range p.caps {
		classes = append(classes, r)
	}
	sort.Slice(classes, func(i, j int) bool { return classes[i] < classes[j] })
	out := make([]types.ResourceStats, 0, len(classes))
	for _, r := range classes {
		out = append(out, types.ResourceStats{
			Resource: r,
			InUse:    p.inUse[r],
			Capacity: p.caps[r],
			Waiting:  p.waiters[r].Len(),
			Acquired: p.acquired[r],
			Rejected: p.rejected[r],
		})
	}
	return out
}

// AcquireWithTimeout is Acquire with an explicit deadline.
func (p *ResourcePool) AcquireWithTimeout(ctx context.Context, class types.ResourceClass, d time.Duration) (*Lease, error) {
	if d <= 0 {
		d = 5 * time.Minute
	}
	cctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	return p.Acquire(cctx, class)
}
