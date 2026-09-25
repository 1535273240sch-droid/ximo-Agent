package main

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/config"
	"github.com/ximo888ok-netizen/ximo-agent/internal/ipc"
	"github.com/ximo888ok-netizen/ximo-agent/internal/supervisor"
)

// 模拟完整的 Engine 实体
type mockEngineProcess struct {
	id          string
	kind        string
	mu          sync.Mutex
	alive       bool
	startCount  int32
	client      *ipc.Client
	endpoint    string
	resumedRuns []string
}

func (m *mockEngineProcess) ID() string   { return m.id }
func (m *mockEngineProcess) Kind() string { return m.kind }

func (m *mockEngineProcess) Start() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	atomic.AddInt32(&m.startCount, 1)
	m.alive = true

	// 启动 IPC 客户端连回 Supervisor
	m.client = ipc.NewClient(m.endpoint, 1024*1024, true)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = m.client.Connect(ctx)

	return nil
}

func (m *mockEngineProcess) Heartbeat() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.alive {
		return fmt.Errorf("engine killed")
	}
	return m.client.Ping(500 * time.Millisecond)
}

func (m *mockEngineProcess) Kill() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.alive = false
	if m.client != nil {
		_ = m.client.Close()
		m.client = nil
	}
	return nil
}

func (m *mockEngineProcess) Resume(ctx context.Context, runID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.resumedRuns = append(m.resumedRuns, runID)
	return nil
}

// 模拟 Worker 实体
type mockWorkerProcess struct {
	id         string
	kind       string
	mu         sync.Mutex
	alive      bool
	startCount int32
	heartbeatFail bool
}

func (w *mockWorkerProcess) ID() string   { return w.id }
func (w *mockWorkerProcess) Kind() string { return w.kind }

func (w *mockWorkerProcess) Start() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	atomic.AddInt32(&w.startCount, 1)
	w.alive = true
	w.heartbeatFail = false
	return nil
}

func (w *mockWorkerProcess) Heartbeat() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.alive || w.heartbeatFail {
		return fmt.Errorf("worker heartbeat failed")
	}
	return nil
}

func (w *mockWorkerProcess) Kill() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.alive = false
	return nil
}

// 验收标准第 1 项：kill Engine → 自动重启 → UI 重连 → run 状态完整（P0 验收）
func TestIntegration_KillEngine_AutoRestart_UIReconnect(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "ximo-int-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	endpoint := fmt.Sprintf("tcp://127.0.0.1:%d", 25000+rand.Intn(5000))

	cfg := config.SupervisorConfig{
		HeartbeatInterval:       30 * time.Millisecond,
		HeartbeatTimeout:        80 * time.Millisecond,
		GracefulShutdownTimeout: 2 * time.Second,
		MaxRestartRetries:       5,
		RestartBackoffSteps:     []int{1, 2},
	}

	ipcServer := ipc.NewServer(endpoint, 1024*1024)
	if err := ipcServer.Start(); err != nil {
		t.Fatalf("start ipc server failed: %v", err)
	}
	defer ipcServer.Stop()

	engine := &mockEngineProcess{
		id:       "engine-01",
		kind:     "engine",
		endpoint: endpoint,
	}

	sup := supervisor.NewSupervisor(cfg, filepath.Join(tmpDir, "crashes"), engine)
	sup.SetIPCServer(ipcServer)

	if err := sup.RegisterEntity(engine, nil); err != nil {
		t.Fatalf("register engine entity failed: %v", err)
	}

	// 模拟当前活跃的一个 Run
	activeRunID := "run-session-p0-test-999"
	sup.TrackUnrecoveredRun(activeRunID)

	if err := sup.Start(); err != nil {
		t.Fatalf("start supervisor failed: %v", err)
	}
	defer sup.GracefulShutdown()

	time.Sleep(50 * time.Millisecond)

	// UI 客户端接入 Supervisor IPC
	uiClient := ipc.NewClient(endpoint, 1024*1024, true)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := uiClient.Connect(ctx); err != nil {
		t.Fatalf("UI client connect failed: %v", err)
	}
	defer uiClient.Close()

	if err := uiClient.Ping(1 * time.Second); err != nil {
		t.Fatalf("UI ping failed: %v", err)
	}

	// 模拟 KILL Engine (崩溃模拟)
	_ = engine.Kill()

	// 等待 Supervisor 检测、捕获崩溃、执行退避并拉起 Engine、调用 Resume。
	//
	// 这里用轮询而不是固定的 time.Sleep：检测（心跳间隔 30ms + 超时 80ms）之后
	// 还要走完一次退避（配置为 1s 起），总时长远超单次睡眠能给的余量。
	// 在 -race 且并行跑全部包时，固定 1200ms 会偶发地不够，导致"引擎未重启"的假失败。
	deadline := time.Now().Add(8 * time.Second)
	for atomic.LoadInt32(&engine.startCount) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("expected engine to be restarted within 8s, startCount=%d",
				atomic.LoadInt32(&engine.startCount))
		}
		time.Sleep(20 * time.Millisecond)
	}

	// 2. 验证 Engine.Resume 是否成功被触发，使得 run 状态完整恢复
	engine.mu.Lock()
	recovered := len(engine.resumedRuns) > 0 && engine.resumedRuns[0] == activeRunID
	engine.mu.Unlock()
	if !recovered {
		t.Fatalf("expected run %s to be resumed by engine recovery", activeRunID)
	}

	// 3. 验证 UI 连接保持健康或自动重连
	if err := uiClient.Ping(1 * time.Second); err != nil {
		t.Fatalf("UI ping failed after engine restart: %v", err)
	}
}

// 验收标准第 2 项：所有 worker 都有 heartbeat、timeout、restart，Worker 挂掉不影响 Engine
func TestIntegration_WorkerHeartbeatTimeoutAndRestart(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "ximo-worker-int-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	cfg := config.SupervisorConfig{
		HeartbeatInterval:       30 * time.Millisecond,
		HeartbeatTimeout:        80 * time.Millisecond,
		GracefulShutdownTimeout: 2 * time.Second,
		MaxRestartRetries:       5,
		RestartBackoffSteps:     []int{1, 2},
	}

	engine := &mockEngineProcess{id: "engine-main", kind: "engine"}
	sup := supervisor.NewSupervisor(cfg, filepath.Join(tmpDir, "crashes"), engine)

	browserWorker := &mockWorkerProcess{id: "worker-browser-1", kind: "worker:browser"}
	mcpWorker := &mockWorkerProcess{id: "worker-mcp-1", kind: "worker:mcp"}
	terminalWorker := &mockWorkerProcess{id: "worker-terminal-1", kind: "worker:terminal"}

	_ = sup.RegisterEntity(browserWorker, nil)
	_ = sup.RegisterEntity(mcpWorker, nil)
	_ = sup.RegisterEntity(terminalWorker, nil)

	if err := sup.Start(); err != nil {
		t.Fatalf("start supervisor failed: %v", err)
	}
	defer sup.GracefulShutdown()

	time.Sleep(50 * time.Millisecond)

	// 模拟 browserWorker 发生心跳故障
	browserWorker.mu.Lock()
	browserWorker.heartbeatFail = true
	browserWorker.mu.Unlock()

	// 等待看门狗超时并重启
	time.Sleep(1200 * time.Millisecond)

	if atomic.LoadInt32(&browserWorker.startCount) < 2 {
		t.Fatalf("expected browserWorker to be restarted after heartbeat failure")
	}

	// 验证其他 worker（mcp, terminal）仍然保持健康，未受任何波及
	st, err := sup.GetEntityState("worker-mcp-1")
	if err != nil || st != supervisor.StateRunning {
		t.Fatalf("mcp worker should remain running, got state: %s", st)
	}
}
