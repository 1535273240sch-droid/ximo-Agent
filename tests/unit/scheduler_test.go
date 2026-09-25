package unit

import (
	"errors"
	"sync"
	"testing"
)

type Priority string

const (
	PriorityInteractive Priority = "interactive"
	PriorityNormal      Priority = "normal"
	PriorityBackground  Priority = "background"
	PriorityKnowledge   Priority = "knowledge"
)

type Task struct {
	ID       string
	Priority Priority
}

type FairQueue struct {
	mu          sync.Mutex
	maxCapacity int
	queues      map[Priority][]Task
	weights     map[Priority]int
	rounds      map[Priority]int
}

func NewFairQueue(maxCapacity int) *FairQueue {
	return &FairQueue{
		maxCapacity: maxCapacity,
		queues: map[Priority][]Task{
			PriorityInteractive: make([]Task, 0),
			PriorityNormal:      make([]Task, 0),
			PriorityBackground:  make([]Task, 0),
			PriorityKnowledge:   make([]Task, 0),
		},
		weights: map[Priority]int{
			PriorityInteractive: 8,
			PriorityNormal:      4,
			PriorityBackground:  1,
			PriorityKnowledge:   1,
		},
		rounds: map[Priority]int{
			PriorityInteractive: 0,
			PriorityNormal:      0,
			PriorityBackground:  0,
			PriorityKnowledge:   0,
		},
	}
}

func (fq *FairQueue) Enqueue(t Task) error {
	fq.mu.Lock()
	defer fq.mu.Unlock()

	total := 0
	for _, q := range fq.queues {
		total += len(q)
	}
	if total >= fq.maxCapacity {
		return errors.New("queue full, reject fast")
	}

	fq.queues[t.Priority] = append(fq.queues[t.Priority], t)
	return nil
}

func (fq *FairQueue) Dequeue() (Task, bool) {
	fq.mu.Lock()
	defer fq.mu.Unlock()

	order := []Priority{PriorityInteractive, PriorityNormal, PriorityBackground, PriorityKnowledge}

	for _, p := range order {
		q := fq.queues[p]
		if len(q) > 0 && fq.rounds[p] < fq.weights[p] {
			item := q[0]
			fq.queues[p] = q[1:]
			fq.rounds[p]++
			return item, true
		}
	}

	// 轮询计数重置
	for _, p := range order {
		fq.rounds[p] = 0
	}

	for _, p := range order {
		q := fq.queues[p]
		if len(q) > 0 {
			item := q[0]
			fq.queues[p] = q[1:]
			fq.rounds[p]++
			return item, true
		}
	}

	return Task{}, false
}

func TestSchedulerFairQueueAndCapacityLimit(t *testing.T) {
	fq := NewFairQueue(10)

	// 1. 正常入队
	for i := 0; i < 10; i++ {
		p := PriorityNormal
		if i%2 == 0 {
			p = PriorityInteractive
		}
		if err := fq.Enqueue(Task{ID: "task", Priority: p}); err != nil {
			t.Fatalf("unexpected enqueue error: %v", err)
		}
	}

	// 2. 超限入队被拒（Reject Fast）
	err := fq.Enqueue(Task{ID: "overflow", Priority: PriorityInteractive})
	if err == nil || err.Error() != "queue full, reject fast" {
		t.Fatalf("expected reject fast error, got %v", err)
	}

	// 3. 出队调度
	task, ok := fq.Dequeue()
	if !ok || task.Priority != PriorityInteractive {
		t.Fatalf("expected interactive task dequeued first according to weight")
	}
}
