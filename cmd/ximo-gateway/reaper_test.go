package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/model"
	gwstore "github.com/ximo888ok-netizen/ximo-agent/internal/gateway/store"
	"github.com/ximo888ok-netizen/ximo-agent/internal/observability"
	"github.com/ximo888ok-netizen/ximo-agent/internal/quota"
	"github.com/ximo888ok-netizen/ximo-agent/internal/storage"
)

// 本文件针对缺陷 D6：quota.Service.ReapExpired 全仓无调用方，网关结算失败时故意
// 不释放的 held 预占（避免双重退款）因此会永久占住用户额度。测试覆盖三层：
//
//	1. 回收协程本身（真 SQLite + 真 store）：过期 held → expired + 账户归零 + release 账本行；
//	2. --reap-interval 0 时不启动；
//	3. flag 默认值/解析；
//	4. 真整栈（真 runWithContext + 真 HTTP + 预先造好数据的库）：证明 flag 真的接到了主程序。

const (
	reapTestUser    = "usr_reap_test"
	reapTestRequest = "req-reap-1"
	reapTestReserve = "rsv_reap_test"
	reapTestFundLed = "led_reap_fund"
	reapTestTotal   = int64(1000)
	reapTestHeld    = int64(400)
)

// ---------------------------------------------------------------------------
// 辅助：真库造数
// ---------------------------------------------------------------------------

// openReaperTestStore 打开（必要时迁移）指定路径的 SQLite，返回真 store 与额度服务。
// Close 幂等，调用方自行关闭；dbPath 由调用方给，便于「造完数关掉、再让真网关打开」。
func openReaperTestStore(t *testing.T, dbPath string) (*storage.Store, *gwstore.Store, *quota.Service) {
	t.Helper()
	logger := observability.NewLogger(io.Discard, observability.LevelError)
	st, err := openStore(context.Background(), options{
		dbPath:        dbPath,
		migrationsDir: repoMigrationsDir(t),
	}, logger)
	if err != nil {
		t.Fatalf("打开测试库 %s 失败: %v", dbPath, err)
	}
	gwSt := gwstore.New(st.DB())
	return st, gwSt, quota.New(gwSt)
}

// seedExpiredHold 造一条「已过期但未结算」的 held 预占：直接走 store 的 ReserveTx
// 插入 expires_at 在过去的行（不是先预占再等 TTL），账户预占额 = reapTestHeld。
//
// 建用户走 store.CreateUser：它同时建出额度账户行，随后的 Adjust 充值把 total 顶到
// reapTestTotal，于是造数后 available = total - held。
func seedExpiredHold(t *testing.T, gwSt *gwstore.Store, quotaSvc *quota.Service) {
	t.Helper()
	ctx := context.Background()
	if err := gwSt.CreateUser(ctx, model.User{
		ID: reapTestUser, Username: "reaper-user", PasswordHash: "test-hash-not-a-secret",
		Status: model.UserStatusActive, GroupID: "default",
	}); err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	if _, err := quotaSvc.Adjust(ctx, reapTestUser, model.LedgerTopup, reapTestTotal,
		"reaper test fund", "reaper-test", reapTestFundLed); err != nil {
		t.Fatalf("充值失败: %v", err)
	}
	now := time.Now().UnixMilli()
	if _, err := gwSt.ReserveTx(ctx, model.Reservation{
		ID: reapTestReserve, UserID: reapTestUser, RequestID: reapTestRequest,
		Status: model.ReservationHeld, Amount: reapTestHeld,
		ExpiresAt: now - 1_000, CreatedAt: now - 2_000, UpdatedAt: now - 2_000,
	}, "led_reap_reserve", now); err != nil {
		t.Fatalf("插入过期 held 预占失败: %v", err)
	}
}

func mustAccount(t *testing.T, gwSt *gwstore.Store, userID string) model.QuotaAccount {
	t.Helper()
	acct, err := gwSt.GetQuotaAccount(context.Background(), userID)
	if err != nil {
		t.Fatalf("读账户失败: %v", err)
	}
	return acct
}

func reservationStatus(t *testing.T, st *storage.Store, id string) string {
	t.Helper()
	var status string
	if err := st.DB().QueryRowContext(context.Background(),
		`SELECT status FROM gw_quota_reservations WHERE id=?`, id).Scan(&status); err != nil {
		t.Fatalf("读预占状态失败: %v", err)
	}
	return status
}

// waitForReserved 轮询账户直到 reserved_amount 等于 want（或超时，返回最后一次快照）。
func waitForReserved(t *testing.T, gwSt *gwstore.Store, userID string, want int64, within time.Duration) model.QuotaAccount {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		acct := mustAccount(t, gwSt, userID)
		if acct.ReservedAmount == want || time.Now().After(deadline) {
			return acct
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func findLedger(entries []model.LedgerEntry, kind, requestID string) (model.LedgerEntry, bool) {
	for _, e := range entries {
		if e.Type == kind && e.RequestID == requestID {
			return e, true
		}
	}
	return model.LedgerEntry{}, false
}

// waitForLog 轮询等待日志出现。回收的账户写入在事务里先可见、日志随后才写，
// 直接断言日志会偶发假失败（实测 -count=5 有 2 次）。
func waitForLog(buf *syncBuffer, want string, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for {
		if strings.Contains(buf.String(), want) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// 1) 回收协程
// ---------------------------------------------------------------------------

func TestReaperReclaimsExpiredHold(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st, gwSt, quotaSvc := openReaperTestStore(t, filepath.Join(t.TempDir(), "reaper.db"))
	defer func() { _ = st.Close() }()
	seedExpiredHold(t, gwSt, quotaSvc)

	if acct := mustAccount(t, gwSt, reapTestUser); acct.ReservedAmount != reapTestHeld ||
		acct.Available() != reapTestTotal-reapTestHeld {
		t.Fatalf("造数后账户 = %+v，期望 reserved=%d available=%d",
			acct, reapTestHeld, reapTestTotal-reapTestHeld)
	}

	logs := &syncBuffer{}
	stop := startReaper(ctx, quotaSvc, 20*time.Millisecond, 100,
		observability.NewLogger(logs, observability.LevelInfo))
	defer stop()

	acct := waitForReserved(t, gwSt, reapTestUser, 0, 5*time.Second)
	if acct.ReservedAmount != 0 {
		t.Fatalf("回收后 reserved_amount = %d，期望 0（账户=%+v 日志=%s）",
			acct.ReservedAmount, acct, logs.String())
	}
	if acct.Available() != reapTestTotal {
		t.Errorf("回收后 available = %d，期望 %d", acct.Available(), reapTestTotal)
	}
	if got := reservationStatus(t, st, reapTestReserve); got != model.ReservationExpired {
		t.Errorf("预占状态 = %q，期望 %q", got, model.ReservationExpired)
	}

	sum, err := gwSt.SumLedgerAmount(ctx, reapTestUser)
	if err != nil {
		t.Fatalf("账本累计失败: %v", err)
	}
	if sum != acct.Available() {
		t.Errorf("账本累计 %d != 可用额度 %d（对账不平）", sum, acct.Available())
	}
	entries, err := gwSt.ListLedger(ctx, reapTestUser, 20, 0)
	if err != nil {
		t.Fatalf("读账本失败: %v", err)
	}
	release, ok := findLedger(entries, model.LedgerRelease, reapTestRequest)
	if !ok {
		t.Fatalf("账本缺少回收产生的 release 行: %+v", entries)
	}
	if release.Amount != reapTestHeld || release.IdempotencyKey != "resv-expire-"+reapTestReserve {
		t.Errorf("release 行 = %+v，期望 amount=%d idempotency_key=resv-expire:%s",
			release, reapTestHeld, reapTestReserve)
	}

	// 日志必须给出回收条数，运维才能核对「这一轮到底回收了几条」。
	// 账户在写事务里就已归零，而日志是事务提交之后才写的，因此这里要轮询等日志落盘。
	if !waitForLog(logs, `"reaped":1`, time.Second) {
		t.Errorf("日志未给出回收条数: %s", logs.String())
	}

	// stop 之后不得再回收（也不能二次入账）。
	stop()
	time.Sleep(80 * time.Millisecond)
	if sum2, err := gwSt.SumLedgerAmount(ctx, reapTestUser); err != nil || sum2 != sum {
		t.Errorf("停止后账本仍在变化: %d -> %d (err=%v)", sum, sum2, err)
	}
	if got := reservationStatus(t, st, reapTestReserve); got != model.ReservationExpired {
		t.Errorf("停止后预占状态 = %q", got)
	}
}

// TestReaperDisabledWhenIntervalZero 验证 --reap-interval 0 = 关闭：不回收、不打日志、
// 停等函数立即返回且可重复调用；nil 服务同样不启动（不 panic）。
func TestReaperDisabledWhenIntervalZero(t *testing.T) {
	ctx := context.Background()
	st, gwSt, quotaSvc := openReaperTestStore(t, filepath.Join(t.TempDir(), "reaper-off.db"))
	defer func() { _ = st.Close() }()
	seedExpiredHold(t, gwSt, quotaSvc)

	logs := &syncBuffer{}
	stop := startReaper(ctx, quotaSvc, 0, 100, observability.NewLogger(logs, observability.LevelInfo))

	// 等一个远大于「启用时用的周期」的时间：期间若回收被启动，账户必然变化。
	time.Sleep(300 * time.Millisecond)
	if acct := mustAccount(t, gwSt, reapTestUser); acct.ReservedAmount != reapTestHeld {
		t.Fatalf("--reap-interval 0 仍在回收: reserved_amount = %d，期望 %d",
			acct.ReservedAmount, reapTestHeld)
	}
	if got := reservationStatus(t, st, reapTestReserve); got != model.ReservationHeld {
		t.Errorf("关闭状态下预占状态 = %q，期望 %q", got, model.ReservationHeld)
	}
	if strings.Contains(logs.String(), "回收") {
		t.Errorf("关闭状态下不应有回收日志: %s", logs.String())
	}

	done := make(chan struct{})
	go func() {
		stop()
		stop() // 幂等
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("关闭状态下的 stop 未立即返回")
	}

	// nil 服务：装配错误不该在后台崩进程。
	startReaper(ctx, nil, time.Millisecond, 10, nil)()
}

// fakeReaper 是回收协程的单测替身：可注入错误或 panic，并统计调用次数。
type fakeReaper struct {
	mu     sync.Mutex
	calls  int
	reaped int
	err    error
	boom   bool
}

func (f *fakeReaper) ReapExpired(context.Context, int64, int) (int, error) {
	f.mu.Lock()
	f.calls++
	n, err, boom := f.reaped, f.err, f.boom
	f.mu.Unlock()
	if boom {
		panic("reaper boom")
	}
	return n, err
}

func (f *fakeReaper) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func waitForCalls(f *fakeReaper, want int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for {
		if f.callCount() >= want {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestReaperSurvivesFailuresAndPanics 覆盖「回收是兜底任务」这条硬要求：失败只记 Warn、
// 循环继续重试、panic 只影响本轮且绝不打死进程（panic 未被 recover 会直接终止测试进程）。
func TestReaperSurvivesFailuresAndPanics(t *testing.T) {
	ctx := context.Background()

	failing := &fakeReaper{err: errors.New("store down")}
	logs := &syncBuffer{}
	stop := startReaper(ctx, failing, 10*time.Millisecond, 10,
		observability.NewLogger(logs, observability.LevelInfo))
	if !waitForLog(logs, "回收超时预占失败", time.Second) {
		t.Errorf("回收失败未记 Warn 日志: %s", logs.String())
	}
	if !waitForCalls(failing, 3, time.Second) {
		t.Errorf("回收失败后协程停止重试: 调用 %d 次", failing.callCount())
	}
	stop()

	booming := &fakeReaper{boom: true}
	logs2 := &syncBuffer{}
	stop2 := startReaper(ctx, booming, 10*time.Millisecond, 10,
		observability.NewLogger(logs2, observability.LevelInfo))
	defer stop2()
	if !waitForLog(logs2, "panic", time.Second) {
		t.Errorf("panic 未被转成日志: %s", logs2.String())
	}
	if !waitForCalls(booming, 3, time.Second) {
		t.Errorf("panic 后协程停止工作: 调用 %d 次", booming.callCount())
	}
}

// ---------------------------------------------------------------------------
// 2) flag 语义
// ---------------------------------------------------------------------------

func TestReapFlagsDefaultsAndDisable(t *testing.T) {
	def, err := parseOptions(nil, io.Discard)
	if err != nil {
		t.Fatalf("解析默认参数失败: %v", err)
	}
	if def.reapInterval != time.Minute {
		t.Errorf("默认 --reap-interval = %v，期望 %v", def.reapInterval, time.Minute)
	}
	if def.reapLimit != 500 {
		t.Errorf("默认 --reap-limit = %d，期望 500（quota.DefaultReapLimit）", def.reapLimit)
	}
	if got := effectiveReapLimit(0); got != quota.DefaultReapLimit {
		t.Errorf("effectiveReapLimit(0) = %d，期望 %d", got, quota.DefaultReapLimit)
	}

	off, err := parseOptions([]string{"--reap-interval", "0"}, io.Discard)
	if err != nil {
		t.Fatalf("解析 --reap-interval 0 失败: %v", err)
	}
	if off.reapInterval != 0 {
		t.Errorf("--reap-interval 0 解析为 %v，期望 0（关闭）", off.reapInterval)
	}

	custom, err := parseOptions([]string{"--reap-interval", "1500ms", "--reap-limit", "7"}, io.Discard)
	if err != nil {
		t.Fatalf("解析自定义回收参数失败: %v", err)
	}
	if custom.reapInterval != 1500*time.Millisecond || custom.reapLimit != 7 {
		t.Errorf("自定义回收参数 = %v/%d，期望 1.5s/7", custom.reapInterval, custom.reapLimit)
	}
}

// ---------------------------------------------------------------------------
// 3) 真整栈：flag 确实接到了主程序（HTTP 观测 + 真进程日志）
// ---------------------------------------------------------------------------

// startGatewayOnDB 与 startTestGateway 相同，但用调用方给的库路径启动
// （需要「先造好过期预占再启动网关」的场景，startTestGateway 固定用全新空库）。
func startGatewayOnDB(t *testing.T, dbPath string, extraArgs ...string) *testGateway {
	t.Helper()
	args := append([]string{
		"--addr", "127.0.0.1:0",
		"--db", dbPath,
		"--migrations", repoMigrationsDir(t),
		"--admin-token", testAdminToken,
	}, extraArgs...)

	ctx, cancel := context.WithCancel(context.Background())
	g := &testGateway{
		dbPath: dbPath,
		exit:   make(chan int, 1),
		cancel: cancel,
		stdout: &syncBuffer{},
		stderr: &syncBuffer{},
	}
	addrCh := make(chan string, 1)
	go func() {
		g.exit <- runWithContext(ctx, args, g.stdout, g.stderr, func(addr string) { addrCh <- addr })
	}()

	select {
	case addr := <-addrCh:
		g.base = "http://" + addr
	case code := <-g.exit:
		cancel()
		t.Fatalf("网关启动失败（退出码 %d）:\n%s", code, g.stderr.String())
	case <-time.After(30 * time.Second):
		cancel()
		t.Fatalf("等待监听地址超时:\n%s", g.stderr.String())
	}
	t.Cleanup(func() { g.stop(t) })
	g.waitHealthy(t)
	return g
}

type reapAccountView struct {
	TotalAmount    int64 `json:"total_amount"`
	UsedAmount     int64 `json:"used_amount"`
	ReservedAmount int64 `json:"reserved_amount"`
	Available      int64 `json:"available"`
}

func TestGatewayReapsExpiredHoldOverHTTP(t *testing.T) {
	t.Setenv(envLogLevel, "info")
	dbPath := filepath.Join(t.TempDir(), "seeded.db")

	// 造数连接必须在启动网关前关闭，避免两个写者同时打开同一个库。
	st, gwSt, quotaSvc := openReaperTestStore(t, dbPath)
	seedExpiredHold(t, gwSt, quotaSvc)
	if err := st.Close(); err != nil {
		t.Fatalf("关闭造数连接失败: %v", err)
	}

	g := startGatewayOnDB(t, dbPath, "--reap-interval", "20ms")

	deadline := time.Now().Add(10 * time.Second)
	var view reapAccountView
	for {
		resp := g.do(t, http.MethodGet, "/admin/quota/accounts/"+reapTestUser, "", adminHeaders())
		if resp.status != http.StatusOK {
			t.Fatalf("读账户状态 = %d（body=%s）", resp.status, resp.body)
		}
		if err := json.Unmarshal(resp.body, &view); err != nil {
			t.Fatalf("账户响应不是 JSON: %s", resp.body)
		}
		if view.ReservedAmount == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if view.ReservedAmount != 0 {
		t.Fatalf("网关运行 10s 后 reserved_amount 仍为 %d（--reap-interval 未接到主程序？日志=%s）",
			view.ReservedAmount, g.stderr.String())
	}
	if view.Available != reapTestTotal || view.TotalAmount != reapTestTotal {
		t.Errorf("回收后账户视图 = %+v，期望 total=%d available=%d", view, reapTestTotal, reapTestTotal)
	}

	// 账本经 HTTP 视图也能看到回收产生的 release 行（与库内一致）。
	ledger := g.do(t, http.MethodGet,
		"/admin/quota/ledger?user_id="+reapTestUser+"&limit=20", "", adminHeaders())
	if ledger.status != http.StatusOK {
		t.Fatalf("读账本状态 = %d（body=%s）", ledger.status, ledger.body)
	}
	if !strings.Contains(string(ledger.body), "resv-expire-"+reapTestReserve) {
		t.Errorf("账本视图缺少回收行 resv-expire-%s: %s", reapTestReserve, ledger.body)
	}

	// 启动日志必须如实报告回收参数（运维核对「开着还是关了」）。
	if !strings.Contains(g.stderr.String(), "reap_interval") {
		t.Errorf("启动日志缺少 reap_interval: %s", g.stderr.String())
	}
}
