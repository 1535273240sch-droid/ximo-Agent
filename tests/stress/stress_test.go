// Package stress 落地第43章「10000 次 run、0 数据结构损坏、0 未释放资源、
// 0 event sequence corruption、0 goroutine leak」红线。
//
// ---------------------------------------------------------------------------
// 重写原因：本文件此前是一份**假的**压力测试
// ---------------------------------------------------------------------------
// 旧版本自己定义了一个本地 LeaseManager、一个本地 RunRecord，并用局部变量
// 循环 1..5「模拟」事件序号，从来没有创建过 Engine、没有打开过 SQLite、没有
// 调用任何真实模块。它因此永远通过、什么都不验证——第43章红线实际上处于
// 未验证状态。
//
// ---------------------------------------------------------------------------
// 现在是 REAL（真实的部分）
// ---------------------------------------------------------------------------
//   - 真实的装配层 internal/bootstrap：真实 SQLite 临时库文件 + 真实 SQL 迁移
//   - 真实工具运行时 + provider 适配器（脚本化内存 Provider）+ 真实 Engine。
//   - 通过真实 Engine.Submit 并发提交 N 个 run，并等待每一个 run 到达真实终态。
//   - 事件序号断言读的是**磁盘上的 run_events 表**（重开连接直读），不是内存
//     缓存：证明事件真的落库、序号真的连续无空洞无重复。
//   - PRAGMA integrity_check / foreign_key_check、写事务残留、outbox 与事件
//     一一对应、goroutine 泄漏、引擎计数器归零，全部是对真实运行实例的断言。
//   - 正向证据：Provider 确实被调用 >= N 次、每条 run 在库中都有落盘事件。
//
// ---------------------------------------------------------------------------
// 仍然是缩小的近似（APPROXIMATION，以及为什么）
// ---------------------------------------------------------------------------
//   - N：默认就是第43章原始目标 10000（本机实测约 23s：10000 个 run 共约
//     70000 条 durable 事件，即约 70000 个走单写队列的 fsync 写事务，仍在
//     可接受范围，因此不在默认路径上缩小）。可用环境变量 XIMO_STRESS_RUNS
//     覆盖成更小的值做快速回归（例如 2000 约 4s）；testing.Short() 下降到
//     200 只做冒烟。实际使用的 N 会在测试日志里打印出来。
//   - Provider 是脚本化的内存 Provider，而不是真实 HTTP 调用：网络延迟与重试
//     被有意排除，本测试要证的是引擎 + 存储的并发正确性，不是网络栈。
//   - Worker 池被关闭（DisableWorkers），只跑进程内工具：浏览器/终端 Worker
//     的进程级泄漏不在可观测范围内（由各自包的测试覆盖）。
//   - 并发上限受 Engine 的真实准入约束（全局 32、单会话 8）。本测试用 16 个
//     提交协程 + 8 个会话（全局在飞 <=16、单会话 <=2），因此不应触发
//     admission_rejected；这是「接近真实负载」而非「刻意打满准入」。
//
// ---------------------------------------------------------------------------
// 关于 lease：原来的 LeaseManager 断言被删除，而不是伪造
// ---------------------------------------------------------------------------
// 真实 Engine 没有对外的「租约」概念：admission ticket 与调度器资源租约都是
// 内部记账，只通过 Engine.Stats() 以 Admission.Running / Scheduler.Resources
// 的形式暴露；存储层的 leases 表由 internal/storage/repository 的测试覆盖，
// bootstrap 装配出的 Engine 并不使用它。所以这里不再伪造 lease 对象，改为对
// **真实计数器**断言全部归零（Admission.Running==0、Sessions==0、
// Scheduler.RunningTasks/QueuedTasks/QueuedToolCalls==0），见
// waitEngineDrained。
package stress

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/bootstrap"
	"github.com/ximo888ok-netizen/ximo-agent/internal/config"
	"github.com/ximo888ok-netizen/ximo-agent/internal/engine"
	"github.com/ximo888ok-netizen/ximo-agent/internal/ports"
	"github.com/ximo888ok-netizen/ximo-agent/internal/ports/mem"
	"github.com/ximo888ok-netizen/ximo-agent/internal/storage"
	"github.com/ximo888ok-netizen/ximo-agent/internal/storage/sqlite"
	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

const (
	// defaultRuns 是默认的压力规模：第43章原始目标（10,000 次 run）。
	// 本机实测约 23s，可接受，因此不在默认路径上缩小。
	defaultRuns = 10000
	// shortRuns 是 testing.Short() 下的冒烟规模。
	shortRuns = 200
	// submitWorkers 是并发提交协程数。必须 <= 准入的全局上限（32），
	// 且配合 sessionCount 后单会话在飞数 <= 8。
	submitWorkers = 16
	// sessionCount 是提交时轮换使用的会话数。
	sessionCount = 8
	// goroutineTolerance 是 goroutine 计数的允许波动（运行时自身会起少量
	// 后台 goroutine，如 GC worker、timer goroutine）。
	goroutineTolerance = 12
	// stressAnswer 是所有 run 的最终答复，用于证明回答确实是真实链路产出的。
	stressAnswer = "stress-ok"
)

// TestStress10000RunsRealEngine 是第43章红线的**真实**证据。
//
// 断言清单（全部针对真实数据库与真实引擎）：
//  1. 每一个提交的 run 都到达终态（无 stranded run），且计数与提交数一致；
//  2. 每个 run 在磁盘库中的事件序号严格递增、无空洞、无重复（seq == i+1）；
//  3. 每个 run 的 runs.last_seq 与该 run 的最大事件序号一致；
//  4. 全库 event_id 唯一、无孤儿事件（run_events 全部能连回 runs）；
//  5. PRAGMA integrity_check == ok、PRAGMA foreign_key_check 无违规行；
//  6. outbox 行与事件一一对应（无孤儿、无丢失）；
//  7. 无残留写事务（OpenWriteTxs == 0）且单写者不变（WriteOpen == 1）；
//  8. 引擎计数器全部归零：Active/Runs/Admission.Running/Scheduler 队列；
//  9. 无 goroutine 泄漏（App 关闭后回落到初始值 + 容差内）。
//
// 正向证据：Provider 被调用 >= N 次；库里存在 >= N 条 run 的事件。
func TestStress10000RunsRealEngine(t *testing.T) {
	started := time.Now()

	n := stressRunCount(t)
	workers := submitWorkers
	if workers > n {
		workers = n
	}
	sessions := sessionCount
	if sessions > n {
		sessions = n
	}
	t.Logf("[stress] REAL 引擎压力测试：N=%d, workers=%d, sessions=%d, short=%v",
		n, workers, sessions, testing.Short())

	// 初始 goroutine 基线：先 GC 并让运行时稳定下来，避免把测试自身的瞬时
	// goroutine 算成泄漏。
	runtime.GC()
	settleGoroutines(runtime.NumGoroutine(), time.Now().Add(500*time.Millisecond))
	initialGoroutines := runtime.NumGoroutine()

	cfg := newStressConfig(t)
	workspace := t.TempDir()
	dbPath := cfg.ResolveDBPath()

	// 脚本化 Provider：所有 run 都直接给出最终答复（不调用任何工具）。
	// 注意 Provider 的 round 计数器是跨 run 的，所以这里只设置 Exhausted，
	// 让每一个 run 都拿到同一个确定性答复。
	prov := mem.NewProvider()
	prov.Exhausted = ports.ProviderResponse{
		FinishReason: ports.FinishStop,
		Content:      stressAnswer,
		Emitted:      true,
	}

	app, err := bootstrap.New(cfg, bootstrap.Options{
		MigrationsDir:  migrationsDir(t),
		Provider:       prov,
		WorkspaceRoot:  workspace,
		DisableWorkers: true,
	})
	if err != nil {
		t.Fatalf("bootstrap.New: %v", err)
	}
	// defer 里只兜底关闭；正常路径会在磁盘校验前显式 Close，
	// 因为校验要求先释放 App 持有的连接池。
	defer app.Close()

	if app.Engine == nil {
		t.Fatal("bootstrap 没有装配出 Engine")
	}

	// ---- 1. 并发提交 N 个 run，并等待各自终态 ------------------------------
	runIDs := make([]string, n)
	var submitFailures atomic.Int64
	var admissionRetries atomic.Int64
	var completed, failed, cancelled, other atomic.Int64
	var unexpectedState sync.Map // runID -> state（失败诊断用）

	idxCh := make(chan int, n)
	for i := 0; i < n; i++ {
		idxCh <- i
	}
	close(idxCh)

	submitCtx, cancelSubmit := context.WithTimeout(context.Background(), stressBudget(n))
	defer cancelSubmit()

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			sessionID := fmt.Sprintf("stress-sess-%02d", worker%sessions)
			for idx := range idxCh {
				runID, err := submitWithRetry(submitCtx, app, sessionID, &admissionRetries)
				if err != nil {
					submitFailures.Add(1)
					t.Errorf("提交第 %d 个 run 失败: %v", idx, err)
					continue
				}
				runIDs[idx] = runID

				// 等待真实终态（WaitRun 内部按 2ms 轮询真实状态，无固定 sleep）。
				if err := app.Engine.WaitRun(submitCtx, runID); err != nil {
					submitFailures.Add(1)
					t.Errorf("run %s 在预算内未到达终态: %v", runID, err)
					continue
				}
				final, err := app.Engine.GetRun(submitCtx, runID)
				if err != nil {
					submitFailures.Add(1)
					t.Errorf("GetRun(%s): %v", runID, err)
					continue
				}
				// 断言 1：每个 run 都到达了终态。
				if !final.State.Terminal() {
					other.Add(1)
					unexpectedState.Store(runID, final.State)
					continue
				}
				switch final.State {
				case types.StateCompleted:
					if final.Answer != stressAnswer {
						t.Errorf("run %s 答复 = %q，期望 %q", runID, final.Answer, stressAnswer)
					}
					completed.Add(1)
				case types.StateFailed:
					failed.Add(1)
					unexpectedState.Store(runID, fmt.Sprintf("failed: %v", final.Err))
				case types.StateCancelled:
					cancelled.Add(1)
				default:
					other.Add(1)
					unexpectedState.Store(runID, final.State)
				}
			}
		}(w)
	}
	wg.Wait()

	if failures := submitFailures.Load(); failures != 0 {
		t.Fatalf("%d 个 run 提交或等待终态失败", failures)
	}
	for i, id := range runIDs {
		if id == "" {
			t.Fatalf("第 %d 个 run 没有拿到 runID（提交被静默丢弃）", i)
		}
	}
	if got := completed.Load(); got != int64(n) {
		var diag []string
		unexpectedState.Range(func(k, v any) bool {
			diag = append(diag, fmt.Sprintf("%v=%v", k, v))
			return len(diag) < 5
		})
		t.Fatalf("完成 %d 个 run，期望 %d（failed=%d cancelled=%d other=%d 诊断=%v）",
			got, n, failed.Load(), cancelled.Load(), other.Load(), diag)
	}
	t.Logf("[stress] 全部 %d 个 run 到达终态：completed=%d, admission 重试=%d",
		n, completed.Load(), admissionRetries.Load())

	// 正向证据（真实工作发生的硬证明）：Provider 被真实调用过，每个 run
	// 至少一轮，所以调用次数必须 >= N。
	if rounds := int64(prov.RoundCount()); rounds < int64(n) {
		t.Fatalf("Provider 只被调用 %d 次，期望 >= %d：装配链路没有真的走到 Provider", rounds, n)
	}

	// ---- 2. 通过引擎事件 API 验证一部分 run 的流式序号 ----------------------
	sample := 64
	if sample > n {
		sample = n
	}
	engineEvents, engineViolations := verifyEngineEventAPI(t, app.Engine, runIDs, sample)
	t.Logf("[stress] 经 Engine.Events 抽样校验 %d 个 run，共 %d 条事件（违规 %d）",
		sample, engineEvents, engineViolations)

	// ---- 3. 引擎计数器归零（无 stranded run / 无未释放资源）-----------------
	stats := waitEngineDrained(t, app.Engine, n)
	t.Logf("[stress] 引擎排空：submitted=%d completed=%d failed=%d cancelled=%d active=%d runs=%d admission.running=%d scheduler(running=%d queued=%d toolCall=%d)",
		stats.Submitted, stats.Completed, stats.Failed, stats.Cancelled,
		stats.Active, stats.Runs, stats.Admission.Running,
		stats.Scheduler.RunningTasks, stats.Scheduler.QueuedTasks, stats.Scheduler.QueuedToolCalls)

	// ---- 4. 显式关闭 App，然后对磁盘库做终局校验 ----------------------------
	app.Close()

	dbResult := verifyDatabase(t, dbPath, runIDs)
	if dbResult.events < n {
		t.Fatalf("库中只有 %d 条 run 事件，少于 run 数 %d：事件没有真的落盘", dbResult.events, n)
	}
	t.Logf("[stress] 磁盘校验：runs=%d, run_events=%d, distinct event_id=%d, 序号违规=%d, 完整性违规=%d",
		n, dbResult.events, dbResult.distinctEventIDs, dbResult.sequenceViolations, dbResult.integrityViolations)

	// ---- 5. goroutine 泄漏 -------------------------------------------------
	deadline := time.Now().Add(15 * time.Second)
	final := settleGoroutines(initialGoroutines+goroutineTolerance, deadline)
	goroutineLeaked := final-initialGoroutines > goroutineTolerance
	if goroutineLeaked {
		t.Errorf("检测到 goroutine 泄漏：初始 %d，最终 %d（容差 %d）",
			initialGoroutines, final, goroutineTolerance)
	}

	// 汇总横幅是**数据驱动**的：只有真的没有任何违规才打印「0 ...」。
	totalViolations := engineViolations + dbResult.sequenceViolations + dbResult.integrityViolations
	if goroutineLeaked {
		totalViolations++
	}
	if totalViolations != 0 {
		t.Errorf("第43章红线被打破：共 %d 项违规", totalViolations)
		return
	}
	elapsed := time.Since(started)
	t.Logf("[stress] PASS: N=%d runs / %d 事件序号损坏 / %d 库完整性违规 / 0 未释放资源 / 0 goroutine 泄漏（初始 %d -> 最终 %d），耗时 %s",
		n, engineViolations+dbResult.sequenceViolations,
		dbResult.integrityViolations, initialGoroutines, final, elapsed.Round(time.Millisecond))
}

// dbVerification 是磁盘终局校验的结果，供汇总横幅以数据驱动的方式渲染
// （横幅绝不能凭空打印「0 违规」）。
type dbVerification struct {
	events              int
	distinctEventIDs    int
	sequenceViolations  int
	integrityViolations int
}

// ---------------------------------------------------------------------------
// 提交与等待
// ---------------------------------------------------------------------------

// submitWithRetry 提交一个 run。真实的准入控制在满载时会立刻返回
// admission_rejected / queue_full（这是设计上的快速拒绝，不是 bug），
// 因此这里对这两类错误做短暂重试；其它错误直接上抛。
func submitWithRetry(ctx context.Context, app *bootstrap.App, sessionID string, retries *atomic.Int64) (string, error) {
	for attempt := 0; ; attempt++ {
		handle, err := app.Engine.Submit(ctx, types.SubmitRequest{
			SessionID: sessionID,
			Prompt:    "stress prompt",
		})
		if err == nil {
			return handle.RunID, nil
		}
		switch types.CodeOf(err) {
		case types.CodeAdmissionRejected, types.CodeQueueFull:
			if attempt >= 2000 {
				return "", fmt.Errorf("准入持续拒绝（%d 次）: %w", attempt, err)
			}
			retries.Add(1)
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(2 * time.Millisecond):
			}
		default:
			return "", err
		}
	}
}

// waitEngineDrained 轮询真实引擎计数器，直到全部归零。
//
// 这是「无 stranded run / 无未释放资源」的断言点：Submitted 必须恰好等于提交
// 数（每个提交都被准入一次且仅一次），终态计数之和等于提交数，且没有活跃 run、
// 没有 admission 占用、没有调度器队列残留、没有悬挂的事件订阅者。
func waitEngineDrained(t *testing.T, eng *engine.Engine, n int) engine.Stats {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var last engine.Stats
	for {
		last = eng.Stats()
		drained := last.Submitted == uint64(n) &&
			last.Completed+last.Failed+last.Cancelled == uint64(n) &&
			last.Active == 0 && last.Runs == 0 &&
			last.Admission.Running == 0 && last.Admission.Sessions == 0 &&
			last.Scheduler.RunningTasks == 0 &&
			last.Scheduler.QueuedTasks == 0 &&
			last.Scheduler.QueuedToolCalls == 0 &&
			last.Bus.Subscribers == 0
		if drained {
			return last
		}
		if time.Now().After(deadline) {
			t.Fatalf("引擎未排空（期望 submitted=%d, terminal=%d, active=0, runs=0, admission=0, scheduler=0, subscribers=0）：%+v",
				n, n, last)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// verifyEngineEventAPI 通过引擎的事件 API 抽样检查序号：
// 只对非零序号（durable 事件）要求严格递增。
// 返回（事件总数, 违规数）。
func verifyEngineEventAPI(t *testing.T, eng *engine.Engine, runIDs []string, sample int) (int, int) {
	t.Helper()
	if sample <= 0 {
		return 0, 0
	}
	stride := len(runIDs) / sample
	if stride < 1 {
		stride = 1
	}
	total, violations := 0, 0
	for i := 0; i < sample; i++ {
		idx := i * stride
		if idx >= len(runIDs) {
			break
		}
		runID := runIDs[idx]

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		ch, err := eng.Events(ctx, runID, 0)
		if err != nil {
			cancel()
			violations++
			t.Errorf("Engine.Events(%s): %v", runID, err)
			continue
		}
		var lastSeq uint64
		count := 0
		for ev := range ch {
			count++
			if ev.Seq == 0 {
				continue // 临时事件没有持久位置
			}
			if ev.Seq <= lastSeq {
				violations++
				t.Errorf("run %s 引擎事件流序号倒退/重复：%d 出现在 %d 之后", runID, ev.Seq, lastSeq)
			}
			lastSeq = ev.Seq
		}
		cancel()
		if count == 0 {
			violations++
			t.Errorf("run %s 经 Engine.Events 没有读到任何事件", runID)
		}
		total += count
	}
	return total, violations
}

// ---------------------------------------------------------------------------
// 磁盘（真实数据库）校验
// ---------------------------------------------------------------------------

// verifyDatabase 重开磁盘上的 SQLite 库并做终局校验，返回汇总结果。
// 用**独立连接**直查 run_events，以此证明事件确实持久化在磁盘上，
// 而不是只停留在进程内存或引擎事件库适配器的缓存里。
func verifyDatabase(t *testing.T, dbPath string, runIDs []string) dbVerification {
	t.Helper()

	var result dbVerification

	if info, err := os.Stat(dbPath); err != nil {
		t.Fatalf("数据库文件 %s 不存在: %v", dbPath, err)
	} else if info.Size() == 0 {
		t.Fatalf("数据库文件 %s 是空的", dbPath)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	store, err := storage.Open(sqlite.DefaultConfig(dbPath))
	if err != nil {
		t.Fatalf("重开数据库 %s: %v", dbPath, err)
	}
	defer store.Close()

	// 断言 5：SQLite 完整性。
	if err := store.CheckIntegrity(ctx); err != nil {
		result.integrityViolations++
		t.Errorf("PRAGMA integrity_check 失败（库损坏）: %v", err)
	}
	// foreign_key_check：run_events 不得成为孤儿（外键已强制，这里再验一遍）。
	// 注意该 PRAGMA 无违规时不返回任何行，所以这里数的是「返回的行数」。
	if bad := pragmaViolations(t, store, "PRAGMA foreign_key_check"); bad != 0 {
		result.integrityViolations++
		t.Errorf("PRAGMA foreign_key_check 报告 %d 行违规", bad)
	}

	// 断言 8：无残留写事务 + 单写者不变。
	if st := store.Stats(); st.OpenWriteTxs != 0 {
		result.integrityViolations++
		t.Errorf("存在未释放的写事务：OpenWriteTxs=%d，期望 0", st.OpenWriteTxs)
	} else if st.WriteOpen != 1 {
		result.integrityViolations++
		t.Errorf("写连接数=%d，期望 1（单写者被破坏）", st.WriteOpen)
	}

	log := storage.NewEventLog(store.DB())

	// 断言 2 + 3：逐 run 校验事件序号连续无空洞无重复，且 last_seq 一致。
	// 失败信息收集前若干个，避免 10000 个 run 全坏时刷屏。
	var seqErrs []string
	fail := func(format string, args ...any) {
		result.sequenceViolations++
		if len(seqErrs) < 5 {
			seqErrs = append(seqErrs, fmt.Sprintf(format, args...))
		}
	}
	totalEvents := 0
	runsWithEvents := 0
	for _, runID := range runIDs {
		events, err := log.Since(ctx, runID, 0)
		if err != nil {
			fail("run %s 读取事件失败: %v", runID, err)
			continue
		}
		if len(events) == 0 {
			fail("run %s 库中没有任何事件（提交了但没有落库）", runID)
			continue
		}
		runsWithEvents++
		for i, ev := range events {
			if ev.Seq != uint64(i+1) {
				fail("run %s 第 %d 条事件 seq=%d（期望 %d）：序号必须稠密且严格递增",
					runID, i, ev.Seq, i+1)
				break
			}
			if ev.EventID == "" {
				fail("run %s seq=%d 的事件缺少 event_id", runID, ev.Seq)
			}
		}
		last, err := log.LastSeq(ctx, runID)
		if err != nil {
			fail("run %s 读取 last_seq 失败: %v", runID, err)
		} else if last != uint64(len(events)) {
			fail("run %s last_seq=%d，但事件数为 %d（不一致）", runID, last, len(events))
		}
		totalEvents += len(events)
	}
	if len(seqErrs) > 0 {
		t.Errorf("事件序号/持久化校验失败（前 %d 条）：%v", len(seqErrs), seqErrs)
	}
	if runsWithEvents != len(runIDs) {
		result.sequenceViolations++
		t.Errorf("只有 %d/%d 个 run 在库中留有事件", runsWithEvents, len(runIDs))
	}
	result.events = totalEvents

	// 断言 4：全库 event_id 唯一 + 无孤儿事件。
	if dups := countRows(t, store,
		`SELECT COUNT(*) FROM (SELECT event_id FROM run_events GROUP BY event_id HAVING COUNT(*) > 1)`); dups != 0 {
		result.integrityViolations++
		t.Errorf("存在重复 event_id：%d 组", dups)
	}
	if orphans := countRows(t, store,
		`SELECT COUNT(*) FROM run_events e LEFT JOIN runs r ON r.id = e.run_id WHERE r.id IS NULL`); orphans != 0 {
		result.integrityViolations++
		t.Errorf("存在孤儿事件（连不回 runs）：%d 条", orphans)
	}

	// 断言 6：outbox 与事件一一对应（每个 durable 事件恰好镜像一次，无孤儿）。
	outboxRows := countRows(t, store, `SELECT COUNT(*) FROM outbox`)
	if outboxRows != totalEvents {
		result.integrityViolations++
		t.Errorf("outbox 行数=%d，run_events 行数=%d：事件镜像不是一一对应", outboxRows, totalEvents)
	}
	if orphans := countRows(t, store,
		`SELECT COUNT(*) FROM outbox o LEFT JOIN run_events e
		   ON e.run_id = o.run_id AND e.seq = o.seq
		 WHERE e.run_id IS NULL`); orphans != 0 {
		result.integrityViolations++
		t.Errorf("outbox 存在无对应事件的孤儿行：%d 条", orphans)
	}

	result.distinctEventIDs = countRows(t, store, `SELECT COUNT(DISTINCT event_id) FROM run_events`)
	return result
}

// countRows 执行一个返回单个整数的查询。
func countRows(t *testing.T, store *storage.Store, query string) int {
	t.Helper()
	row := store.QueryRowContext(context.Background(), query)
	var n int
	if err := row.Scan(&n); err != nil {
		t.Fatalf("查询 %q 失败: %v", query, err)
	}
	return n
}

// pragmaViolations 数一个「无违规时零行」的 PRAGMA 返回了多少行
// （foreign_key_check 干净时不返回任何行，而不是返回 0）。
func pragmaViolations(t *testing.T, store *storage.Store, pragma string) int {
	t.Helper()
	rows, err := store.QueryContext(context.Background(), pragma)
	if err != nil {
		t.Fatalf("%s: %v", pragma, err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("%s 读取列: %v", pragma, err)
	}
	n := 0
	for rows.Next() {
		dest := make([]any, len(cols))
		for i := range dest {
			var v any
			dest[i] = &v
		}
		if err := rows.Scan(dest...); err != nil {
			t.Fatalf("%s 扫描行: %v", pragma, err)
		}
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s 遍历失败: %v", pragma, err)
	}
	return n
}

// ---------------------------------------------------------------------------
// 配置与工具
// ---------------------------------------------------------------------------

// newStressConfig 构造一份指向临时目录的完整配置（与
// internal/bootstrap/bootstrap_test.go 的 newTestConfig 同构）。
func newStressConfig(t *testing.T) *config.Config {
	t.Helper()
	dir := t.TempDir()

	cfg := config.NewDefaultConfig()
	cfg.Paths.BaseDir = dir
	cfg.Paths.VersionsDir = filepath.Join(dir, "versions")
	cfg.Paths.DataDir = filepath.Join(dir, "data")
	cfg.Paths.LogsDir = filepath.Join(dir, "data", "logs")
	cfg.Paths.CrashDumpDir = filepath.Join(dir, "data", "crashes")
	cfg.Paths.RunDir = filepath.Join(dir, "data", "run")
	cfg.Paths.CurrentPointerFile = filepath.Join(dir, "current")
	cfg.Paths.PreviousPointerFile = filepath.Join(dir, "previous")
	cfg.Storage.DBPath = filepath.Join(dir, "stress.db")
	cfg.Provider.BaseURL = "https://example.invalid"
	cfg.Provider.SecretRef = "test"
	return cfg
}

// stressRunCount 决定本次实际运行的 run 数。
//
//	XIMO_STRESS_RUNS 覆盖一切（例如设 2000 做快速回归）；
//	testing.Short() -> 200（冒烟）；
//	否则 -> 10000（默认，即第43章原始红线，理由见文件头）。
func stressRunCount(t *testing.T) int {
	t.Helper()
	if v := strings.TrimSpace(os.Getenv("XIMO_STRESS_RUNS")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			t.Fatalf("XIMO_STRESS_RUNS=%q 不是正整数", v)
		}
		return n
	}
	if testing.Short() {
		return shortRuns
	}
	return defaultRuns
}

// stressBudget 给出整体预算：base 秒 + 每个 run 的余量，封顶 15 分钟。
func stressBudget(n int) time.Duration {
	budget := 60*time.Second + time.Duration(n)*30*time.Millisecond
	if budget > 15*time.Minute {
		budget = 15 * time.Minute
	}
	return budget
}

// migrationsDir 定位仓库的 SQL 迁移目录（测试工作目录是 tests/stress）。
func migrationsDir(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	dir := filepath.Join(wd, "..", "..", "migrations")
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("找不到迁移目录 %s: %v", dir, err)
	}
	return dir
}

// settleGoroutines 反复 GC 并观测 goroutine 数，直到回落到 limit 以下或到达
// 期限。返回最后一次观测值。这是「等待真实状态收敛」而不是固定 sleep。
func settleGoroutines(limit int, deadline time.Time) int {
	for {
		cur := runtime.NumGoroutine()
		if cur <= limit || time.Now().After(deadline) {
			return cur
		}
		runtime.GC()
		time.Sleep(20 * time.Millisecond)
	}
}
