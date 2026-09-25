// Package e2e 提供「真实多进程系统能跑通」的端到端证据。
//
// 为什么需要这个包：仓库里绝大多数测试都对着 mock 跑，各模块各自证明自己，
// 却没有一条测试把「UI 通过 IPC 提交 run → Engine 真实执行 → 结果与事件回到
// 调用方」这条链路整体串起来。本包用真实的 SQLite + 真实工具运行时 + 脚本化
// Provider 装配出 Engine，把它挂在一个真实的 ipc.Server 上，再用一个**独立的
// ipcapi.Client 连接**（等价于一个真实的 UI 进程）驱动它，全部断言都来自线上
// 帧的往返结果，而不是内存里的直调。
//
// 本包证明：
//  1. 业务帧链路真实可用：Submit 经 IPC 进 Engine，Status 轮询到终态，
//     最终答复与 Provider 脚本一致，事件确实跨线流动。
//  2. 取消能穿过 IPC 传播到真正阻塞中的 Provider 调用，run 落到 cancelled。
//  3. 恢复入口经 IPC 可用：对库里真实的「未完成 run」（在 bootstrap 装配前
//     直接用 SQL 写入非终态事件日志）执行 Resume，Engine 真的把它推进并停到
//     waiting_user；本次 IPC 回报的计划、决策、序列校验都正确。
//  4. 第26章的断线重连数据面：换一条新连接后以 lastSeq 续拉只拿到更新的事件，
//     无重复无回退；并验证服务端 → 客户端的推送（Broadcast）路径可用。
//
// 本包刻意不做：
//   - 不模拟真正的进程崩溃。真正的 SIGKILL 崩溃恢复由
//     internal/engine/crash_test.go 用「子进程 + Kill」实现，那才是权威的崩溃
//     证据；本包第3项用的是「装配前预置一段未完成的持久化日志」，是同一条恢复
//     代码路径的另一种注入口，不能替代真崩溃测试。
//   - 不测 Supervisor 拉起子进程（进程管理层另有测试），本包只走
//     ipc.Server + ipcapi.EngineService 这层真实业务协议。
//
// 所有断言都要求接线真正成立：任何一处 IPC 注册、帧编解码、Engine 装配或
// 恢复逻辑断掉，对应用例都会失败，不存在恒真用例。
package e2e

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/bootstrap"
	"github.com/ximo888ok-netizen/ximo-agent/internal/config"
	"github.com/ximo888ok-netizen/ximo-agent/internal/engine"
	"github.com/ximo888ok-netizen/ximo-agent/internal/ipc"
	"github.com/ximo888ok-netizen/ximo-agent/internal/ipcapi"
	"github.com/ximo888ok-netizen/ximo-agent/internal/ports"
	"github.com/ximo888ok-netizen/ximo-agent/internal/ports/mem"
	"github.com/ximo888ok-netizen/ximo-agent/internal/storage/migrations"
	"github.com/ximo888ok-netizen/ximo-agent/internal/storage/sqlite"
	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// ipcMaxPayload 与 config 默认值一致（16MB），够任何一个单帧测试用。
const ipcMaxPayload = 16 * 1024 * 1024

// ---------------------------------------------------------------------------
// 测试夹具
// ---------------------------------------------------------------------------

// harness 是一次「Engine 进程侧」的最小装配：真实 App + 真实 IPC 服务端 +
// 已注册的 ipcapi.EngineService。UI 侧连接由 dial() 单独建立，两者之间只通过
// 帧通信。
type harness struct {
	t        *testing.T
	cfg      *config.Config
	app      *bootstrap.App
	srv      *ipc.Server
	endpoint string
	// provider 是脚本化 Provider，留着做「Engine 真的调了模型」这类断言。
	provider *mem.Provider
}

// newHarness 装配一个指向临时目录的真实 App，并在随机高位端口上启动 IPC 服务。
//
// 注意：这里刻意**不**调用启动恢复（main.go 的 runEngine 会在启动时自行
// Recover 一次）。测试3要验证的正是「由 UI 经 IPC 触发 resume」这条路，
// 若夹具预先恢复了，那条路径就被绕过了。
func newHarness(t *testing.T, provider *mem.Provider) *harness {
	t.Helper()

	cfg := newTestConfig(t)
	app, err := bootstrap.New(cfg, bootstrap.Options{
		MigrationsDir:  migrationsDir(t),
		Provider:       provider,
		WorkspaceRoot:  t.TempDir(),
		DisableWorkers: true,
	})
	if err != nil {
		t.Fatalf("bootstrap.New: %v", err)
	}
	t.Cleanup(app.Close)

	endpoint, srv := startEngineIPC(t, app, cfg)
	return &harness{t: t, cfg: cfg, app: app, srv: srv, endpoint: endpoint, provider: provider}
}

// startEngineIPC 起一个监听随机端口的 ipc.Server，注册 EngineService。
// 端口冲突时重试，避免测试机上的偶发占用导致假失败。
func startEngineIPC(t *testing.T, app *bootstrap.App, cfg *config.Config) (string, *ipc.Server) {
	t.Helper()
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		endpoint := freeTCPEndpoint(t)
		srv := ipc.NewServer(endpoint, cfg.IPC.MaxPayloadLength)
		if err := srv.Start(); err != nil {
			lastErr = err
			continue
		}
		svc, err := ipcapi.NewEngineService(app.Engine)
		if err != nil {
			_ = srv.Stop()
			t.Fatalf("ipcapi.NewEngineService: %v", err)
		}
		svc.Register(srv)
		t.Cleanup(func() { _ = srv.Stop() })
		return endpoint, srv
	}
	t.Fatalf("could not start engine ipc server after retries: %v", lastErr)
	return "", nil
}

// dial 建立一条独立的 UI 侧连接。它是「另一个客户端」而不是引擎内部的直调，
// 因此每一次断言都经过真实的帧编解码与网络往返。
func (h *harness) dial() *ipcapi.Client {
	h.t.Helper()
	c := ipcapi.NewClient(h.endpoint, ipcMaxPayload, 10*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		h.t.Fatalf("ipc client connect to %s: %v", h.endpoint, err)
	}
	h.t.Cleanup(func() { _ = c.Close() })
	// 探活一次，确认对端确实在服务，而不是恰好连上了一个空壳监听。
	if err := c.Ping(); err != nil {
		h.t.Fatalf("ipc client ping: %v", err)
	}
	return c
}

// newTestConfig 构造指向临时目录的完整配置（与 bootstrap_test 同构）。
func newTestConfig(t *testing.T) *config.Config {
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
	cfg.Storage.DBPath = filepath.Join(dir, "test.db")
	cfg.Provider.BaseURL = "https://example.invalid"
	cfg.Provider.SecretRef = "test"
	return cfg
}

// freeTCPEndpoint 返回一个当前空闲的 tcp:// 端点。先监听再释放拿到端口号；
// 存在理论上的抢占窗口，故 startEngineIPC 对 Start 失败做重试。
func freeTCPEndpoint(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a free tcp port: %v", err)
	}
	addr, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		_ = l.Close()
		t.Fatalf("unexpected listener address type %T", l.Addr())
	}
	port := addr.Port
	_ = l.Close()
	return fmt.Sprintf("tcp://127.0.0.1:%d", port)
}

// migrationsDir 定位仓库 SQL 迁移目录：测试运行在 tests/e2e，仓库根在上两级。
func migrationsDir(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	dir := filepath.Join(wd, "..", "..", "migrations")
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("migrations directory not found at %s: %v", dir, err)
	}
	return dir
}

// waitTerminalIPC 经 IPC 轮询 run 状态直到终态或超时。
func waitTerminalIPC(ctx context.Context, t *testing.T, c *ipcapi.Client, runID string) ipcapi.RunPayload {
	t.Helper()
	deadline := time.Now().Add(25 * time.Second)
	var last ipcapi.RunPayload
	for time.Now().Before(deadline) {
		p, err := c.Status(ctx, runID)
		if err == nil {
			last = p
			if types.RunState(p.State).Terminal() {
				return p
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("run %s did not reach a terminal state over IPC within the deadline (last=%+v)", runID, last)
	return ipcapi.RunPayload{}
}

// waitFor 轮询 cond 直到为真或超时。
func waitFor(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", within, what)
}

// typesOf 渲染事件类型链，失败信息里用。
func typesOf(events []types.Event) string {
	out := ""
	for i, e := range events {
		if i > 0 {
			out += " → "
		}
		out += string(e.Type)
	}
	return out
}

// hasType 判断事件链里是否含某类型。
func hasType(events []types.Event, want types.EventType) bool {
	for _, e := range events {
		if e.Type == want {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 1. IPC 端到端：提交 → 轮询 → 结果与事件回到调用方
// ---------------------------------------------------------------------------

// TestIPCEndToEndRunCompletes 是最核心的一条证据：
// 一个独立客户端经 IPC 提交 run，Engine 用真实 SQLite + 脚本化 Provider 跑完，
// 客户端再经 IPC 取回终态、最终答复与完整事件流。
func TestIPCEndToEndRunCompletes(t *testing.T) {
	const answer = "IPC 端到端答复"

	provider := mem.NewProvider(mem.FinalRound(answer))
	h := newHarness(t, provider)
	ui := h.dial()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	handle, err := ui.Submit(ctx, ipcapi.SubmitPayload{
		SessionID: "sess-ipc-e2e",
		Prompt:    "请给出一句话结论",
	})
	if err != nil {
		t.Fatalf("Submit over IPC: %v", err)
	}
	if handle.RunID == "" {
		t.Fatal("Submit over IPC returned an empty run id")
	}
	if handle.SessionID != "sess-ipc-e2e" {
		t.Errorf("Submit echoed session %q, want %q", handle.SessionID, "sess-ipc-e2e")
	}
	if handle.State != string(types.StateQueued) {
		// Submit 必须立即返回一个已排队的句柄，而不是等到跑完。
		t.Errorf("Submit state = %q, want %q", handle.State, string(types.StateQueued))
	}

	final := waitTerminalIPC(ctx, t, ui, handle.RunID)
	if final.State != string(types.StateCompleted) {
		t.Fatalf("final state = %s (error=%q), want completed", final.State, final.Error)
	}
	if final.Answer != answer {
		t.Errorf("final answer = %q, want %q", final.Answer, answer)
	}

	// Provider 真的被调用过：证明装配把脚本 Provider 接到了 Agent 循环上，
	// 而不是 run 在别处以某种短路方式「完成」了。
	if provider.RoundCount() == 0 {
		t.Error("provider was never called: the engine did not really run the model round")
	}

	// 事件确实跨线流动，且顺序权威（Seq 严格递增且连续）。
	evPayload, err := ui.Events(ctx, handle.RunID, 0)
	if err != nil {
		t.Fatalf("Events over IPC: %v", err)
	}
	if len(evPayload.Events) == 0 {
		t.Fatal("no events crossed the wire for a completed run")
	}
	if evPayload.RunID != handle.RunID {
		t.Errorf("Events echoed run id %q, want %q", evPayload.RunID, handle.RunID)
	}
	for i, ev := range evPayload.Events {
		if ev.RunID != handle.RunID {
			t.Errorf("event %d belongs to run %q, want %q", i, ev.RunID, handle.RunID)
		}
		if i > 0 && ev.Seq != evPayload.Events[i-1].Seq+1 {
			t.Errorf("event sequence is not contiguous at index %d: %d after %d (chain: %s)",
				i, ev.Seq, evPayload.Events[i-1].Seq, typesOf(evPayload.Events))
		}
	}
	// 注意：Submit 直接从 created 走到 queued，因此日志里第一条生命周期事件是
	// run.queued，而不是 run.created（后者在 v2 里不作为独立事件发出）。
	for _, want := range []types.EventType{
		types.EventRunQueued, types.EventFinalAnswer, types.EventRunCompleted,
	} {
		if !hasType(evPayload.Events, want) {
			t.Errorf("event stream over IPC is missing %s; got %s", want, typesOf(evPayload.Events))
		}
	}
	// final_answer 事件必须承载与终态一致的答复。
	for _, ev := range evPayload.Events {
		if ev.Type == types.EventFinalAnswer && ev.Message != answer {
			t.Errorf("final_answer event message = %q, want %q", ev.Message, answer)
		}
	}
	// 模型轮次真的发生了：事件链里必须有 round.started/round.completed，
	// 这证明 Agent 循环被真实驱动过，而不是在别处短路完成。
	// （注意：Round 计数本身不随 IPC 帧携带，因此这里断言事件类型。）
	if !hasType(evPayload.Events, types.EventRoundStarted) ||
		!hasType(evPayload.Events, types.EventRoundCompleted) {
		t.Errorf("event stream shows no model round boundary; chain: %s", typesOf(evPayload.Events))
	}
	// 终态事件必须排在最后，否则 UI 会在收到 "completed" 后还看到新事件。
	last := evPayload.Events[len(evPayload.Events)-1]
	if last.Type != types.EventRunCompleted {
		t.Errorf("last event type = %s, want run.completed (chain: %s)", last.Type, typesOf(evPayload.Events))
	}
}

// ---------------------------------------------------------------------------
// 2. 取消经 IPC 传播到阻塞中的 Provider
// ---------------------------------------------------------------------------

// TestIPCCancelInFlightRun 证明取消不是「本地把状态改成 cancelled」：
// Provider 卡在一个永不关闭的 channel 上，run 只能在收到取消、上下文被取消后
// 才可能结束；而取消是从另一条 IPC 连接发过来的。
func TestIPCCancelInFlightRun(t *testing.T) {
	block := make(chan struct{})
	// 测试结束时兜底释放，避免取消路径万一失效时留下永久阻塞的 goroutine。
	t.Cleanup(func() { close(block) })

	provider := mem.NewProvider(mem.FinalRound("永远不会到达这里"))
	provider.Block = block

	h := newHarness(t, provider)
	ui := h.dial()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	handle, err := ui.Submit(ctx, ipcapi.SubmitPayload{
		SessionID: "sess-ipc-cancel",
		Prompt:    "启动一个会被取消的任务",
	})
	if err != nil {
		t.Fatalf("Submit over IPC: %v", err)
	}

	// 等到 Provider 真的进入了 Complete：此刻调用是在飞行中的，
	// 取消才有意义（而不是取消一个还没开始的 run）。
	waitFor(t, 10*time.Second, "provider call to be in flight", func() bool {
		return provider.RoundCount() >= 1
	})

	if p, err := ui.Status(ctx, handle.RunID); err == nil && types.RunState(p.State).Terminal() {
		t.Fatalf("run reached terminal state %q before cancel was sent; the blocking provider did not block", p.State)
	}

	if err := ui.Cancel(ctx, handle.RunID); err != nil {
		t.Fatalf("Cancel over IPC: %v", err)
	}

	final := waitTerminalIPC(ctx, t, ui, handle.RunID)
	if final.State != string(types.StateCancelled) {
		t.Fatalf("final state = %s (error=%q), want cancelled; the cancel did not reach the in-flight provider",
			final.State, final.Error)
	}

	// 事件流里必须有取消的持久化痕迹。
	evPayload, err := ui.Events(ctx, handle.RunID, 0)
	if err != nil {
		t.Fatalf("Events over IPC after cancel: %v", err)
	}
	if !hasType(evPayload.Events, types.EventRunCancelled) {
		t.Errorf("event stream has no run.cancelled after an IPC cancel; got %s", typesOf(evPayload.Events))
	}
}

// ---------------------------------------------------------------------------
// 3. 恢复经 IPC：对预置的「未完成 run」执行 Resume
// ---------------------------------------------------------------------------

// TestIPCRecoveryResume 覆盖「kill 后由 UI/Supervisor 经 IPC 触发恢复」这条链路。
//
// 关于真实性的边界：这里不做 SIGKILL（那需要子进程，见
// internal/engine/crash_test.go 的 TestRandomKillThenRecover）。这里改用另一种
// 诚实的注入口——在 bootstrap 装配**之前**，直接用 SQL 往真实的 SQLite 里写入
// 一个非终态的 run 及其事件日志，模拟「上一个进程死在 thinking 阶段」留下的
// 持久化现场。随后 Engine 装配起来、经 IPC 收到 resume 帧，走的是与真崩溃恢复
// 完全相同的 Scan → Inspect → applyRecoveryPlan 代码路径。
func TestIPCRecoveryResume(t *testing.T) {
	t.Run("clean_database_resumes_nothing", func(t *testing.T) {
		h := newHarness(t, mem.NewProvider(mem.FinalRound("unused")))
		ui := h.dial()

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		rec, err := ui.Resume(ctx, "")
		if err != nil {
			t.Fatalf("Resume over IPC on a clean database: %v", err)
		}
		if len(rec.Plans) != 0 || len(rec.Resumed) != 0 || len(rec.Failed) != 0 || len(rec.NeedsConfirm) != 0 {
			t.Errorf("clean database produced a non-empty recovery payload: %+v", rec)
		}
	})

	t.Run("seeded_incomplete_run_is_resumed", func(t *testing.T) {
		cfg := newTestConfig(t)
		const (
			runID     = "run-seeded-incomplete"
			sessionID = "sess-seeded"
		)
		seedIncompleteRun(t, cfg.ResolveDBPath(), runID, sessionID)

		app, err := bootstrap.New(cfg, bootstrap.Options{
			MigrationsDir:  migrationsDir(t),
			Provider:       mem.NewProvider(mem.FinalRound("unused")),
			WorkspaceRoot:  t.TempDir(),
			DisableWorkers: true,
		})
		if err != nil {
			t.Fatalf("bootstrap.New over a seeded database: %v", err)
		}
		t.Cleanup(app.Close)

		endpoint, _ := startEngineIPC(t, app, cfg)
		ui := ipcapi.NewClient(endpoint, ipcMaxPayload, 10*time.Second)
		connCtx, cancelConn := context.WithTimeout(context.Background(), 5*time.Second)
		if err := ui.Connect(connCtx); err != nil {
			cancelConn()
			t.Fatalf("connect: %v", err)
		}
		cancelConn()
		t.Cleanup(func() { _ = ui.Close() })

		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()

		rec, err := ui.Resume(ctx, "")
		if err != nil {
			t.Fatalf("Resume over IPC with a seeded incomplete run: %v", err)
		}

		// 计划里必须有这个 run，且决策是「可自动续跑」、序列校验通过。
		var plan *types.RecoveryPlan
		for i := range rec.Plans {
			if rec.Plans[i].RunID == runID {
				plan = &rec.Plans[i]
			}
		}
		if plan == nil {
			t.Fatalf("recovery returned no plan for the seeded run %s: %+v", runID, rec.Plans)
		}
		if !plan.SequenceOK {
			t.Errorf("seeded run failed sequence verification: %v", plan.Notes)
		}
		if plan.Decision != types.DecisionResumeAuto {
			t.Errorf("decision = %q, want %q (notes: %v)", plan.Decision, types.DecisionResumeAuto, plan.Notes)
		}
		if plan.ResumeState != types.StateThinking {
			t.Errorf("resume state = %q, want %q", plan.ResumeState, types.StateThinking)
		}
		if !containsStr(rec.Resumed, runID) {
			t.Errorf("resumed list = %v, want it to contain %s", rec.Resumed, runID)
		}

		// 最强的一条断言：恢复不只是「算了个计划」，它真的把 run 推进并停到了
		// waiting_user（等用户确认后再续跑）。状态通过 IPC 查询可见，说明
		// 引擎的物化状态确实变了。
		got, err := ui.Status(ctx, runID)
		if err != nil {
			t.Fatalf("Status after resume: %v", err)
		}
		if got.State != string(types.StateWaitingUser) {
			t.Errorf("run state after resume = %q, want %q", got.State, string(types.StateWaitingUser))
		}

		// 只针对指定 run 的 resume 也必须被接受并保持过滤语义：
		// 一个不存在的 run id 会让 Resumed/Failed/NeedsConfirm 全为空，
		// 但 Plans 仍回报本次扫描到的计划。
		scoped, err := ui.Resume(ctx, "run-does-not-exist")
		if err != nil {
			t.Fatalf("scoped Resume over IPC: %v", err)
		}
		if len(scoped.Resumed) != 0 || len(scoped.Failed) != 0 || len(scoped.NeedsConfirm) != 0 {
			t.Errorf("scoped resume for an unknown run returned non-empty buckets: %+v", scoped)
		}
	})
}

// seedIncompleteRun 在装配 Engine 之前，往真实 SQLite 里写一个非终态的 run：
// 会话行 + run 行（status=thinking, last_seq=3）+ 三条连续事件（created →
// queued → thinking）。这等价于「上一个进程死在 thinking 阶段」的持久化现场。
func seedIncompleteRun(t *testing.T, dbPath, runID, sessionID string) {
	t.Helper()
	ctx := context.Background()

	db, err := sqlite.Open(sqlite.DefaultConfig(dbPath))
	if err != nil {
		t.Fatalf("open sqlite for seeding: %v", err)
	}
	defer func() { _ = db.Close() }()

	if _, err := migrations.ApplyFromDir(ctx, db, migrationsDir(t)); err != nil {
		t.Fatalf("apply migrations before seeding: %v", err)
	}

	now := time.Now().UnixMilli()
	err = db.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO sessions (id, title, mode, created_at, updated_at) VALUES (?,?,?,?,?)`,
			sessionID, "e2e-seed", "default", now, now); err != nil {
			return fmt.Errorf("insert session: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO runs (id, session_id, status, prompt, model, created_at, updated_at, last_seq)
			 VALUES (?,?,?,?,?,?,?,0)`,
			runID, sessionID, "thinking", "(seeded: in flight when the previous process died)",
			"test", now, now); err != nil {
			return fmt.Errorf("insert run: %w", err)
		}
		for _, se := range seededEvents(sessionID) {
			payload, err := json.Marshal(se.env)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO run_events (run_id, seq, event_id, event_type, payload_json, created_at)
				 VALUES (?,?,?,?,?,?)`,
				runID, se.seq, fmt.Sprintf("%s#%d", runID, se.seq), se.typ, payload, now); err != nil {
				return fmt.Errorf("insert event seq %d: %w", se.seq, err)
			}
		}
		// 事件与 last_seq 同事务推进（I4）。
		if _, err := tx.ExecContext(ctx, `UPDATE runs SET last_seq=? WHERE id=?`, 3, runID); err != nil {
			return fmt.Errorf("bump last_seq: %w", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed incomplete run: %v", err)
	}
}

// seedEnvelope 与 bootstrap 的 eventEnvelope 字段对齐：引擎从 payload_json
// 反序列化这些字段来重建 run 状态（state/sessionId/round 等）。
type seedEnvelope struct {
	SessionID string         `json:"sessionId,omitempty"`
	SeqInRun  uint64         `json:"seqInRun,omitempty"`
	State     string         `json:"state,omitempty"`
	PrevState string         `json:"prevState,omitempty"`
	Round     int            `json:"round,omitempty"`
	Message   string         `json:"message,omitempty"`
	Data      map[string]any `json:"data,omitempty"`
}

type seedEvent struct {
	seq uint64
	typ string
	env seedEnvelope
}

// seededEvents 返回与真实生命周期一致的三条连续事件。
func seededEvents(sessionID string) []seedEvent {
	withData := func(from, to, reason string) map[string]any {
		return map[string]any{"from": from, "to": to, "reason": reason}
	}
	return []seedEvent{
		{seq: 1, typ: string(types.EventRunCreated), env: seedEnvelope{
			SessionID: sessionID, SeqInRun: 1, State: string(types.StateCreated),
			Message: "seeded", Data: withData("", string(types.StateCreated), "seeded"),
		}},
		{seq: 2, typ: string(types.EventRunQueued), env: seedEnvelope{
			SessionID: sessionID, SeqInRun: 2, State: string(types.StateQueued),
			PrevState: string(types.StateCreated), Message: "seeded",
			Data: withData(string(types.StateCreated), string(types.StateQueued), "seeded"),
		}},
		{seq: 3, typ: string(types.EventRunStateChanged), env: seedEnvelope{
			SessionID: sessionID, SeqInRun: 3, State: string(types.StateThinking),
			PrevState: string(types.StateQueued), Round: 1, Message: "seeded",
			Data: withData(string(types.StateQueued), string(types.StateThinking), "seeded"),
		}},
	}
}

func containsStr(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 4. 第26章断线重连数据面：lastSeq 续拉 + 服务端推送
// ---------------------------------------------------------------------------

// TestIPCEventResumeAfterReconnect 验证前端 reconnect-manager 依赖的数据面：
// 断开后用一条**新连接**以 lastSeq 续拉事件，只会拿到更新的部分（无重复、
// 无回退）；afterSeq=0 时能完整重放；并验证服务端 Broadcast 推送可达客户端。
func TestIPCEventResumeAfterReconnect(t *testing.T) {
	provider := mem.NewProvider(mem.FinalRound("重连测试答复"))
	h := newHarness(t, provider)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 第一条连接：提交并跑到终态，然后拉一次全量事件。
	ui := h.dial()
	handle, err := ui.Submit(ctx, ipcapi.SubmitPayload{
		SessionID: "sess-reconnect",
		Prompt:    "跑一个用于重连验证的任务",
	})
	if err != nil {
		t.Fatalf("Submit over IPC: %v", err)
	}
	waitTerminalIPC(ctx, t, ui, handle.RunID)

	full, err := ui.Events(ctx, handle.RunID, 0)
	if err != nil {
		t.Fatalf("initial Events: %v", err)
	}
	if len(full.Events) < 2 {
		t.Fatalf("expected several durable events, got %d (%s)", len(full.Events), typesOf(full.Events))
	}
	// 断开（模拟 UI 掉线）。Close 会同时停掉重连循环。
	if err := ui.Close(); err != nil {
		t.Fatalf("close the first client: %v", err)
	}

	// 第二条连接：等价于 UI 重连后新建的连接。
	reconnected := h.dial()

	// (a) 以「已见到的最后一条 seq」续拉：应恰好为空（没有更新的了）。
	lastSeq := full.Events[len(full.Events)-1].Seq
	none, err := reconnected.Events(ctx, handle.RunID, lastSeq)
	if err != nil {
		t.Fatalf("Events(afterSeq=lastSeq): %v", err)
	}
	if len(none.Events) != 0 {
		t.Errorf("resume from the last seen seq returned %d events, want 0 (duplicates/regression): %s",
			len(none.Events), typesOf(none.Events))
	}

	// (b) 从中间一条 seq 续拉：应恰好等于全量列表的尾部，逐条 seq/type 相同。
	cut := len(full.Events) / 2
	afterSeq := full.Events[cut-1].Seq
	got, err := reconnected.Events(ctx, handle.RunID, afterSeq)
	if err != nil {
		t.Fatalf("Events(afterSeq=mid): %v", err)
	}
	want := full.Events[cut:]
	if len(got.Events) != len(want) {
		t.Fatalf("resume returned %d events, want %d (afterSeq=%d, chain: %s)",
			len(got.Events), len(want), afterSeq, typesOf(got.Events))
	}
	for i := range want {
		if got.Events[i].Seq != want[i].Seq {
			t.Errorf("resumed event %d has seq %d, want %d", i, got.Events[i].Seq, want[i].Seq)
		}
		if got.Events[i].Type != want[i].Type {
			t.Errorf("resumed event %d has type %s, want %s", i, got.Events[i].Type, want[i].Type)
		}
		if got.Events[i].Seq <= afterSeq {
			t.Errorf("resumed event %d has seq %d <= afterSeq %d: a stale event was redelivered",
				i, got.Events[i].Seq, afterSeq)
		}
	}

	// (c) afterSeq=0 的完整重放：证明新 UI 从头渲染是可行的。
	replay, err := reconnected.Events(ctx, handle.RunID, 0)
	if err != nil {
		t.Fatalf("Events(afterSeq=0) on a reconnected client: %v", err)
	}
	if len(replay.Events) != len(full.Events) {
		t.Errorf("full replay returned %d events, want %d", len(replay.Events), len(full.Events))
	}

	// (d) 服务端 → 客户端推送路径（Broker 方向）。UI 的事件推送依赖
	//     Subscribe + Broadcast，这里用一条自定义帧类型验证它确实可达。
	gotPush := make(chan *ipc.Frame, 1)
	reconnected.Raw().Subscribe("system.test.push", func(f *ipc.Frame) {
		select {
		case gotPush <- f:
		default:
		}
	})
	const pushPayload = `{"kind":"ui.push","n":1}`
	h.srv.Broadcast(&ipc.Frame{
		Header:  ipc.FrameHeader{Version: 1, Type: "system.test.push", SessionID: "sess-reconnect"},
		Payload: []byte(pushPayload),
	})
	select {
	case f := <-gotPush:
		if string(f.Payload) != pushPayload {
			t.Errorf("broadcast payload = %q, want %q", string(f.Payload), pushPayload)
		}
		if f.Header.Type != "system.test.push" {
			t.Errorf("broadcast frame type = %q, want system.test.push", f.Header.Type)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server Broadcast never reached the reconnected client: the UI push path is not wired")
	}
}

// 编译期断言：本夹具装配出的具体类型，必须满足 ipcapi.EngineService 所消费的
// 接口。这不是同义反复——EngineService 只依赖 types.Engine / types.Recoverer，
// 一旦两者漂移（例如 Engine 少实现了 RecoverForIPC），本包会在编译期而不是
// 运行期失败，从而保证下面这些 e2e 断言确实跑在真实的引擎契约之上。
var (
	_ types.Engine    = (*engine.Engine)(nil)
	_ types.Recoverer = (*engine.Engine)(nil)
	_ ports.Provider  = (*mem.Provider)(nil)
)
