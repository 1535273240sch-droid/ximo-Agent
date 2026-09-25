package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/scheduler"
	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// This file is the Engine↔Scheduler seam named in the task book's deliverable
// list.
//
// The scheduler package knows nothing about runs, conversations or events: it
// queues and rate-limits opaque tasks. The engine knows nothing about virtual
// time, semaphores or QoS weights. This file is the only place the two meet, so
// the coupling stays in one reviewable file rather than leaking into either
// side.

// SchedulerFacade exposes the scheduler's interface to the rest of the engine
// in engine terms, and is the type task 08 wires into the UI host when it needs
// scheduler statistics.
type SchedulerFacade struct {
	sched *scheduler.Scheduler
}

// SubmitRunTask enqueues a run-level task so a run occupies a global running
// slot through the same mechanism as everything else.
//
// The engine deliberately does not call this for the run's own execution: a run
// is executed by its own goroutine and the scheduler's run task exists purely as
// capacity accounting. It is called here so the accounting is real rather than
// nominal, and so Stats reports the run.
func (f *SchedulerFacade) SubmitRunTask(ctx context.Context, rec *runRecord, cost int) error {
	if f == nil || f.sched == nil {
		return nil
	}
	if cost <= 0 {
		// Pricing a run in "rounds" keeps a long run from monopolising a slot
		// that a short interactive run would finish with.
		cost = 1
	}
	task := types.Task{
		ID:        types.NewID(types.PrefixTask),
		Kind:      types.TaskKindRun,
		RunID:     rec.run.ID,
		SessionID: rec.run.SessionID,
		Priority:  rec.run.Priority,
		Cost:      cost,
	}
	return f.sched.SubmitWait(ctx, task)
}

// CancelRun withdraws every queued or running scheduler task for a run.
func (f *SchedulerFacade) CancelRun(runID string) int {
	if f == nil || f.sched == nil {
		return 0
	}
	return f.sched.CancelRun(runID)
}

// Stats reports the scheduler's three-layer snapshot.
func (f *SchedulerFacade) Stats() types.SchedulerStats {
	if f == nil || f.sched == nil {
		return types.SchedulerStats{}
	}
	return f.sched.Stats()
}

// Scheduler exposes the facade for callers that need to reason about capacity.
func (e *Engine) Scheduler() *SchedulerFacade {
	return &SchedulerFacade{sched: e.sched}
}

// Resources exposes the engine's resource pool.
//
// 子代理编排（任务 05）必须从这里拿 expert_agent 槽位，而不是自建一个池：
// 只有共用同一个 ResourcePool，"同时在跑的子代理数 ≤ 8" 才是真的上限，
// 否则就是两个各配 8 槽的半吊子池。返回 nil 表示引擎尚未装配调度器。
func (f *SchedulerFacade) Resources() *scheduler.ResourcePool {
	if f == nil || f.sched == nil {
		return nil
	}
	return f.sched.Resources()
}

// ResourceClassFor maps a tool name onto its resource class, so the UI can
// explain which pool a queued call is waiting on.
func ResourceClassFor(toolName string) types.ResourceClass {
	return scheduler.DefaultToolResource(toolName)
}

// ExplainRejection renders a rejection in terms a user can act on. It is used
// by the UI host to explain why a submit failed.
func ExplainRejection(err error) string {
	if err == nil {
		return ""
	}
	switch types.CodeOf(err) {
	case types.CodeAdmissionRejected:
		return "系统当前并发已达上限，请稍后重试，或降低任务优先级。"
	case types.CodeQueueFull:
		return "队列已满：为避免内存耗尽，请求被直接拒绝，请稍后重试。"
	case types.CodeResourceUnavailable:
		return "所需资源（浏览器/终端等）已全部占用，请稍后重试。"
	case types.CodeMemoryLimitExceeded:
		return "内存预算已耗尽，请等待当前任务结束后重试。"
	case types.CodeRunDurationExceeded:
		return "任务超出最长执行时间限制，已被终止。"
	case types.CodeOutputLimitExceeded:
		return "任务输出量超出上限，已被终止。"
	case types.CodeEngineClosed:
		return "引擎正在关闭，无法接受新任务。"
	default:
		return types.RedactString(err.Error())
	}
}

// NewAdmissionForTest builds an admission controller with a fresh budget. It
// exists so tests in other packages can exercise admission in isolation; the
// engine always uses the scheduler's shared budget.
func NewAdmissionForTest(cfg AdmissionConfig) (*Admission, error) {
	return NewAdmission(cfg, scheduler.NewBudget(cfg.Limits))
}

// WaitIdle blocks until no scheduler task and no engine run is active, or ctx
// is done. Tests use it to reach a quiescent state before asserting.
func (e *Engine) WaitIdle(ctx context.Context) error {
	tick := time.NewTicker(2 * time.Millisecond)
	defer tick.Stop()
	for {
		if e.sched.RunningCount() == 0 && e.sched.QueueDepth() == 0 && e.activeRuns() == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return types.WrapError(types.CodeDeadlineExceeded, ctx.Err(), "waiting for engine to go idle")
		case <-tick.C:
		}
	}
}

// activeRuns counts runs that have not reached a terminal state.
func (e *Engine) activeRuns() int {
	e.mu.Lock()
	recs := make([]*runRecord, 0, len(e.runs))
	for _, rec := range e.runs {
		recs = append(recs, rec)
	}
	e.mu.Unlock()

	// The state is read through the record's own accessor, after releasing
	// e.mu, so the lock order is always e.mu → rec.mu and never the reverse.
	n := 0
	for _, rec := range recs {
		if !rec.terminal() {
			n++
		}
	}
	return n
}

// WaitRun blocks until a specific run reaches a terminal state, or ctx is done.
// It is the engine-level equivalent of awaiting a future.
func (e *Engine) WaitRun(ctx context.Context, runID string) error {
	tick := time.NewTicker(2 * time.Millisecond)
	defer tick.Stop()
	for {
		e.mu.Lock()
		rec, ok := e.runs[runID]
		e.mu.Unlock()

		if ok && rec.terminal() {
			return nil
		}
		if !ok {
			// The record was removed on terminal completion, which WaitRun's
			// callers interpret as "finished".
			if r, err := e.loadRunFromLog(ctx, runID); err == nil && r.State.Terminal() {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return types.WrapError(types.CodeDeadlineExceeded, ctx.Err(), "waiting for run %s", runID)
		case <-tick.C:
		}
	}
}

// RunResult is a run's terminal outcome, for callers that want the answer
// rather than the whole record.
type RunResult struct {
	RunID  string
	State  types.RunState
	Answer string
	Err    error
}

// AwaitRun waits for a run and returns its outcome.
func (e *Engine) AwaitRun(ctx context.Context, runID string) (RunResult, error) {
	if err := e.WaitRun(ctx, runID); err != nil {
		return RunResult{}, err
	}
	run, err := e.GetRun(ctx, runID)
	if err != nil {
		return RunResult{}, err
	}
	out := RunResult{RunID: run.ID, State: run.State, Answer: run.Answer}
	if run.Err != nil {
		out.Err = run.Err
	}
	return out, nil
}

// sprintAny renders an arbitrary value; a local helper so engine.go does not
// need fmt at the top level for one call.
func sprintAny(v any) string { return fmt.Sprint(v) }
