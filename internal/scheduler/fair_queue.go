package scheduler

import (
	"container/list"
	"sort"
	"sync"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// vtimeScale fixes the virtual-time arithmetic in integers. Weighted fair
// queueing needs fractional increments (a cost-1 task on the interactive class
// advances virtual time by 1/8), and integer scaling avoids the slow drift of
// accumulated float error in a long-running scheduler.
const vtimeScale = 1000

// FairQueue is the second layer of rate limiting: a weighted fair queue per
// QoS class, plus the per-session tool-concurrency gate.
//
// Selection is weighted fair queueing on virtual time. Every class keeps a
// virtual finish time; the non-empty class with the smallest finish time wins,
// and each dispatch advances that class by cost/weight. A lower-weight class
// therefore still makes progress — it is delayed, never starved — which is the
// property a strict-priority queue would lose.
//
// Weights come from types.Priority.Weight(): interactive 8, normal 4,
// background 1, knowledge 1.
type FairQueue struct {
	mu sync.Mutex

	weights map[types.Priority]int
	classes map[types.Priority]*queueClass
	// total is the current number of queued tasks across all classes.
	total    int
	totalCap int
	// perCap optionally caps each class individually.
	perCap map[types.Priority]int

	// sessions tracks per-session in-flight tool calls (layer 2).
	sessions map[string]int
	// sessionCap is MaxConcurrentToolsPerSession.
	sessionCap int
}

// queueEntry is one queued task.
type queueEntry struct {
	task *Task
}

// queueClass is one QoS class's FIFO plus its virtual time.
type queueClass struct {
	priority types.Priority
	weight   int
	items    *list.List
	// vtime is the class's virtual finish time, in vtimeScale units.
	vtime int64
	// dispatched and rejected are lifetime counters.
	dispatched uint64
	rejected   uint64
}

// NewFairQueue builds a fair queue with the documented weights. totalCap is
// the hard ceiling on queued tasks, which the architecture requires to be
// finite; a task beyond it is rejected immediately rather than queued.
func NewFairQueue(totalCap int, sessionCap int) *FairQueue {
	q := &FairQueue{
		weights:    make(map[types.Priority]int, len(types.AllPriorities)),
		classes:    make(map[types.Priority]*queueClass, len(types.AllPriorities)),
		totalCap:   totalCap,
		perCap:     make(map[types.Priority]int),
		sessions:   make(map[string]int),
		sessionCap: sessionCap,
	}
	for _, p := range types.AllPriorities {
		w := p.Weight()
		q.weights[p] = w
		q.classes[p] = &queueClass{priority: p, weight: w, items: list.New()}
	}
	return q
}

// SetPerPriorityCap installs a per-class ceiling. A class left unset is
// bounded only by the total cap, so the default configuration is exactly the
// documented single hard cap.
func (q *FairQueue) SetPerPriorityCap(caps map[types.Priority]int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.perCap = caps
}

// Enqueue adds a task to its class. It fails fast when the queue is at a cap:
// the architecture forbids growing a queue without bound to absorb load.
func (q *FairQueue) Enqueue(task *Task) error {
	if task == nil {
		return types.NewError(types.CodeInvalidArgument, "cannot enqueue a nil task")
	}
	p := task.Priority
	if p == "" {
		p = types.PriorityNormal
		task.Priority = p
	}
	if !p.Valid() {
		return types.NewError(types.CodeInvalidArgument, "unknown priority %q", string(p))
	}
	if task.Cost <= 0 {
		task.Cost = 1
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	cl := q.classes[p]
	if q.total >= q.totalCap {
		cl.rejected++
		return types.NewError(types.CodeQueueFull,
			"fair queue is full (%d/%d)", q.total, q.totalCap)
	}
	if cap, ok := q.perCap[p]; ok && cl.items.Len() >= cap {
		cl.rejected++
		return types.NewError(types.CodeQueueFull,
			"priority %q queue is full (%d/%d)", string(p), cl.items.Len(), cap)
	}

	e := &queueEntry{task: task}
	cl.items.PushBack(e)
	task.EnqueuedAt = time.Now()
	q.total++
	return nil
}

// Dequeue returns the next dispatchable task, or nil when nothing can run now.
//
// A task is dispatchable when its session is below MaxConcurrentToolsPerSession.
// Tool tasks are the only ones subject to the session cap; run tasks have their
// own global gate and always dispatch when selected.
//
// The returned task's lease must be released with Release, otherwise the
// session's in-flight count leaks.
func (q *FairQueue) Dequeue() *Task {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.dequeueLocked()
}

// dequeueLocked is Dequeue without the lock, so ReleaseMany can drain a batch
// under a single acquisition.
func (q *FairQueue) dequeueLocked() *Task {
	// Consider classes in ascending virtual time. Sorting at most four
	// classes keeps the cost independent of queue depth.
	order := make([]*queueClass, 0, len(types.AllPriorities))
	for _, p := range types.AllPriorities {
		cl := q.classes[p]
		if cl.items.Len() > 0 {
			order = append(order, cl)
		}
	}
	if len(order) == 0 {
		return nil
	}
	sort.SliceStable(order, func(i, j int) bool { return order[i].vtime < order[j].vtime })

	// Take the earliest-virtual-time class that actually has a runnable task.
	// If a class is blocked on its session cap, fall through to the next
	// class rather than stalling the whole scheduler behind one busy session.
	for _, cl := range order {
		for e := cl.items.Front(); e != nil; e = e.Next() {
			ent := e.Value.(*queueEntry)
			if !q.sessionHasCapacityLocked(ent.task) {
				continue
			}
			cl.items.Remove(e)
			q.total--
			cl.vtime += int64(ent.task.Cost) * vtimeScale / int64(cl.weight)
			cl.dispatched++
			if ent.task.Kind == types.TaskKindToolCall && ent.task.SessionID != "" {
				q.sessions[ent.task.SessionID]++
			}
			return ent.task
		}
	}
	return nil
}

// Release returns a dispatched task's lease. It must be called exactly once
// per successful Dequeue.
func (q *FairQueue) Release(task *Task) {
	if task == nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.releaseLocked(task)
}

func (q *FairQueue) releaseLocked(task *Task) {
	if task.Kind != types.TaskKindToolCall || task.SessionID == "" {
		return
	}
	if n := q.sessions[task.SessionID]; n <= 1 {
		delete(q.sessions, task.SessionID)
	} else {
		q.sessions[task.SessionID] = n - 1
	}
}

// Drain removes and returns up to max dispatchable tasks, releasing each
// immediately so callers can inspect a batch without holding leases.
// It is a convenience for tests and for the stats path.
func (q *FairQueue) Drain(max int) []*Task {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []*Task
	for len(out) < max {
		t := q.dequeueLocked()
		if t == nil {
			break
		}
		q.releaseLocked(t)
		out = append(out, t)
	}
	return out
}

// Cancel removes a still-queued task. It reports whether the task was found;
// a task already dispatched is not in the queue and cannot be cancelled this
// way.
func (q *FairQueue) Cancel(taskID string) bool {
	if taskID == "" {
		return false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, cl := range q.classes {
		for e := cl.items.Front(); e != nil; e = e.Next() {
			ent := e.Value.(*queueEntry)
			if ent.task.ID == taskID {
				cl.items.Remove(e)
				q.total--
				return true
			}
		}
	}
	return false
}

// CancelRun removes every queued task belonging to a run. It returns how many
// tasks were dropped.
func (q *FairQueue) CancelRun(runID string) int {
	if runID == "" {
		return 0
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	n := 0
	for _, cl := range q.classes {
		for e := cl.items.Front(); e != nil; {
			next := e.Next()
			ent := e.Value.(*queueEntry)
			if ent.task.RunID == runID {
				cl.items.Remove(e)
				q.total--
				n++
			}
			e = next
		}
	}
	return n
}

// DrainAll removes every queued task and returns them, so a caller can account
// for work it is abandoning rather than silently dropping it.
func (q *FairQueue) DrainAll() []*Task {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]*Task, 0, q.total)
	for _, cl := range q.classes {
		for e := cl.items.Front(); e != nil; e = e.Next() {
			out = append(out, e.Value.(*queueEntry).task)
		}
		cl.items.Init()
	}
	q.total = 0
	return out
}

// InFlight reports how many tool calls a session currently has running.
func (q *FairQueue) InFlight(sessionID string) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.sessions[sessionID]
}

// Len returns the total queued depth.
func (q *FairQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.total
}

// ClassLen returns one class's queued depth.
func (q *FairQueue) ClassLen(p types.Priority) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	cl, ok := q.classes[p]
	if !ok {
		return 0
	}
	return cl.items.Len()
}

// Stats snapshots per-class counters.
func (q *FairQueue) Stats() []types.QueueStats {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]types.QueueStats, 0, len(types.AllPriorities))
	for _, p := range types.AllPriorities {
		cl := q.classes[p]
		out = append(out, types.QueueStats{
			Priority:   p,
			Weight:     cl.weight,
			Queued:     cl.items.Len(),
			MaxQueued:  q.capForLocked(p),
			Dispatched: cl.dispatched,
			Rejected:   cl.rejected,
		})
	}
	return out
}

// sessionHasCapacityLocked reports whether a task may run given its session's
// in-flight count. Caller must hold the lock.
func (q *FairQueue) sessionHasCapacityLocked(t *Task) bool {
	if t.Kind != types.TaskKindToolCall || t.SessionID == "" {
		return true
	}
	return q.sessions[t.SessionID] < q.sessionCap
}

func (q *FairQueue) capForLocked(p types.Priority) int {
	if c, ok := q.perCap[p]; ok {
		return c
	}
	return q.totalCap
}
