package supervisor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/config"
)

func TestExponentialBackoff(t *testing.T) {
	b := NewDefaultBackoff()
	expected := []time.Duration{
		1 * time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
		30 * time.Second,
		30 * time.Second,
	}

	for i, exp := range expected {
		actual := b.Next(i + 1)
		if actual != exp {
			t.Fatalf("attempt %d: expected %v, got %v", i+1, exp, actual)
		}
	}
}

func TestRestartTrackerCircuitBreaker(t *testing.T) {
	tracker := NewRestartTracker(3, 100*time.Millisecond)

	delay, retry := tracker.RecordFailure()
	if !retry || delay != 1*time.Second {
		t.Fatalf("unexpected first failure: delay=%v, retry=%v", delay, retry)
	}

	_, retry = tracker.RecordFailure()
	if !retry {
		t.Fatalf("expected retry allowed on 2nd failure")
	}

	_, retry = tracker.RecordFailure()
	if retry {
		t.Fatalf("expected circuit open after 3rd failure")
	}
	if tracker.State() != CircuitOpen {
		t.Fatalf("expected state CircuitOpen, got %s", tracker.State())
	}
}

func TestRingBufferAndCrashDump(t *testing.T) {
	ring := NewRingBuffer(10)
	_, _ = ring.Write([]byte("12345"))
	if string(ring.Bytes()) != "12345" {
		t.Fatalf("unexpected content: %s", string(ring.Bytes()))
	}
	_, _ = ring.Write([]byte("67890ABCDE"))
	// 超出10字节，只保留最新的10字节 "67890ABCDE"
	if string(ring.Bytes()) != "67890ABCDE" {
		t.Fatalf("unexpected ring overflow content: %s", string(ring.Bytes()))
	}

	tmpDir, err := os.MkdirTemp("", "ximo-crash-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	col := NewCrashCollector(tmpDir)
	report, err := col.Collect("engine", "engine", 1234, 1, time.Now(), []byte("panic: test error"))
	if err != nil {
		t.Fatalf("collect crash dump failed: %v", err)
	}
	if report.ProcessID != 1234 || report.Role != "engine" {
		t.Fatalf("unexpected report data: %+v", report)
	}

	reports, err := col.ListReports()
	if err != nil || len(reports) != 1 {
		t.Fatalf("expected 1 report, got %d, err: %v", len(reports), err)
	}
}

type mockSupervisable struct {
	id         string
	kind       string
	mu         sync.Mutex
	startCount int32
	hbErr      error
	killCount  int32
	alive      bool
}

func (m *mockSupervisable) ID() string   { return m.id }
func (m *mockSupervisable) Kind() string { return m.kind }

func (m *mockSupervisable) Start() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	atomic.AddInt32(&m.startCount, 1)
	m.alive = true
	return nil
}

func (m *mockSupervisable) Heartbeat() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.alive {
		return errors.New("process not alive")
	}
	return m.hbErr
}

func (m *mockSupervisable) Kill() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	atomic.AddInt32(&m.killCount, 1)
	m.alive = false
	return nil
}

func TestSupervisorWatchdogAndEngineRecovery(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "ximo-sup-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	cfg := config.SupervisorConfig{
		HeartbeatInterval:       30 * time.Millisecond,
		HeartbeatTimeout:        80 * time.Millisecond,
		GracefulShutdownTimeout: 1 * time.Second,
		MaxRestartRetries:       5,
		RestartBackoffSteps:     []int{1, 2},
	}

	resumer := &MockEngineResumer{}
	sup := NewSupervisor(cfg, filepath.Join(tmpDir, "crashes"), resumer)

	engineMock := &mockSupervisable{
		id:   "engine-main",
		kind: "engine",
	}

	if err := sup.RegisterEntity(engineMock, nil); err != nil {
		t.Fatalf("register entity failed: %v", err)
	}

	sup.TrackUnrecoveredRun("run-001")
	sup.TrackUnrecoveredRun("run-002")

	if err := sup.Start(); err != nil {
		t.Fatalf("start supervisor failed: %v", err)
	}
	defer sup.GracefulShutdown()

	time.Sleep(50 * time.Millisecond)
	st, err := sup.GetEntityState("engine-main")
	if err != nil || st != StateRunning {
		t.Fatalf("expected running state, got %s, err %v", st, err)
	}

	// 模拟 Engine 心跳失效 / 崩溃
	engineMock.mu.Lock()
	engineMock.hbErr = errors.New("heartbeat failed")
	engineMock.mu.Unlock()

	// 等待 Watchdog 检测并触发恢复。
	//
	// 这里用轮询而不是固定 sleep，原因（时间预算）：
	//   1. 检测：Watchdog 每 HeartbeatInterval(30ms) 探一次心跳，心跳失败后
	//      还要等 now-LastHeartbeat > HeartbeatTimeout(80ms) 才判定 unhealthy，
	//      即约 80~110ms；
	//   2. 退避：RestartBackoffSteps=[1,2] 决定首次重启延迟是 1s
	//      （见 NewBackoffFromSteps，Next(1)=steps[0]=1s）。
	// 合计 ≈1.08~1.11s，再加上 crash dump 写盘与 goroutine 调度，原先固定的
	// 1200ms 只剩约 100ms 余量；并行跑全部包 / 机器负载高时会被吃掉而假失败。
	// 与 cmd/ximo-agent 的集成测试同样的做法：轮询到 3 倍预期时长（3s）的截止点。
	deadline := time.Now().Add(3 * time.Second)
	restarted := false
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&engineMock.startCount) >= 2 {
			restarted = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// 验证 Engine 是否被重新拉起 (Start 被再次调用)
	if !restarted {
		t.Fatalf("expected engine to be restarted at least once, startCount=%d", atomic.LoadInt32(&engineMock.startCount))
	}

	// 验证未决 Run 是否被调用 Resume。
	// triggerEngineRecovery 在 Start 成功之后才同步执行，所以要在 startCount 达标
	// 之后再轮询，避免读到尚未写入的恢复记录。
	resumeDeadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(resumeDeadline) {
		resumer.mu.Lock()
		resumedCount := len(resumer.ResumedRuns)
		resumer.mu.Unlock()
		if resumedCount >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	resumer.mu.Lock()
	resumedCount := len(resumer.ResumedRuns)
	resumer.mu.Unlock()
	if resumedCount < 2 {
		t.Fatalf("expected at least 2 resumed runs, got %d", resumedCount)
	}
}

func TestGracefulShutdown9Steps(t *testing.T) {
	cfg := config.SupervisorConfig{
		HeartbeatInterval:       100 * time.Millisecond,
		HeartbeatTimeout:        500 * time.Millisecond,
		GracefulShutdownTimeout: 2 * time.Second,
	}

	sup := NewSupervisor(cfg, "", nil)

	var stepLog []int
	var mu sync.Mutex

	recordStep := func(step int) GracefulShutdownHook {
		return func(ctx context.Context) error {
			mu.Lock()
			stepLog = append(stepLog, step)
			mu.Unlock()
			return nil
		}
	}

	sup.SetShutdownPipeline(ShutdownPipeline{
		Step1StopAcceptingRuns:      recordStep(1),
		Step2StopAcceptingToolCalls: recordStep(2),
		Step3CancelInteractive:      recordStep(3),
		Step4FlushEventWriter:       recordStep(4),
		Step5FlushOutbox:            recordStep(5),
		Step6PersistRunStates:       recordStep(6),
		Step7TerminateWorkers:       recordStep(7),
		Step8CloseDB:                recordStep(8),
		Step9Exit:                   recordStep(9),
	})

	if err := sup.GracefulShutdown(); err != nil {
		t.Fatalf("graceful shutdown failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(stepLog) != 9 {
		t.Fatalf("expected 9 steps executed, got %d", len(stepLog))
	}
	for i := 0; i < 9; i++ {
		if stepLog[i] != i+1 {
			t.Fatalf("step sequence mismatch at %d: got %d, want %d", i, stepLog[i], i+1)
		}
	}
}

func TestRegisterDuplicateEntity(t *testing.T) {
	sup := NewSupervisor(config.DefaultSupervisorConfig(), "", nil)
	e1 := &mockSupervisable{id: "dup-id", kind: "worker:test"}
	e2 := &mockSupervisable{id: "dup-id", kind: "worker:test"}

	if err := sup.RegisterEntity(e1, nil); err != nil {
		t.Fatalf("first registration failed: %v", err)
	}

	err := sup.RegisterEntity(e2, nil)
	if !errors.Is(err, ErrDuplicateEntity) {
		t.Fatalf("expected ErrDuplicateEntity, got: %v", err)
	}
}
