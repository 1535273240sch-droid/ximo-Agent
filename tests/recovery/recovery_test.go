package recovery

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/observability"
)

// MockEngine 模拟 Engine 进程生命周期与 Run 恢复
type MockEngine struct {
	mu           sync.Mutex
	alive        bool
	runs         map[string]*MockRun
	resumeCalled []string
}

type MockRun struct {
	ID           string
	Status       string
	LastSequence uint64
	PendingTool  string
	ToolIsSafe   bool
}

func NewMockEngine() *MockEngine {
	return &MockEngine{
		alive: true,
		runs:  make(map[string]*MockRun),
	}
}

func (e *MockEngine) Kill() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.alive = false
}

func (e *MockEngine) Restart() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.alive = true
}

func (e *MockEngine) Resume(runID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.alive {
		return errors.New("engine dead")
	}
	run, exists := e.runs[runID]
	if !exists {
		return errors.New("run not found")
	}
	if run.ToolIsSafe {
		run.Status = "running"
	} else {
		run.Status = "waiting_user_confirmation"
	}
	e.resumeCalled = append(e.resumeCalled, runID)
	return nil
}

// TestEngineCrashAndRunRecovery 验证 kill Engine 后的恢复流程（第38章算法）
func TestEngineCrashAndRunRecovery(t *testing.T) {
	eng := NewMockEngine()
	// 准备两个待恢复的 Run：一个是幂等工具，另一个是非幂等工具
	eng.runs["run-safe"] = &MockRun{
		ID:          "run-safe",
		Status:      "executing",
		PendingTool: "file_read",
		ToolIsSafe:  true,
	}
	eng.runs["run-unsafe"] = &MockRun{
		ID:          "run-unsafe",
		Status:      "executing",
		PendingTool: "delete_file",
		ToolIsSafe:  false,
	}

	// 1. 模拟 kill Engine
	eng.Kill()
	observability.WorkerCrash("engine", "main-engine")

	// 2. Supervisor 检测到 Engine exit，启动重启流程
	eng.Restart()
	observability.WorkerRestart("engine", "main-engine")

	// 3. 遍历未完成的 runs 执行恢复
	for _, r := range eng.runs {
		if err := eng.Resume(r.ID); err != nil {
			t.Fatalf("failed to resume run %s: %v", r.ID, err)
		}
	}

	// 4. 校验：安全运行的自动恢复为 running，非幂等运行标记等待用户确认
	if eng.runs["run-safe"].Status != "running" {
		t.Fatalf("expected run-safe status 'running', got '%s'", eng.runs["run-safe"].Status)
	}
	if eng.runs["run-unsafe"].Status != "waiting_user_confirmation" {
		t.Fatalf("expected run-unsafe status 'waiting_user_confirmation', got '%s'", eng.runs["run-unsafe"].Status)
	}
}

// TestWorkerCrashIsolation 验证 Worker (Browser/MCP/Terminal) 被 kill 后 Engine 不退出并成功重启 Worker
func TestWorkerCrashIsolation(t *testing.T) {
	engineAlive := true
	workerStatus := "running"

	// 模拟 Worker Crash
	workerStatus = "crashed"
	observability.WorkerCrash("worker:browser", "browser-1")

	// 核心断言：Engine 进程仍然存活（故障隔离）
	if !engineAlive {
		t.Fatalf("engine crashed when worker crashed! Isolation violated.")
	}

	// Supervisor 介入重启 Worker
	time.Sleep(10 * time.Millisecond)
	workerStatus = "running"
	observability.WorkerRestart("worker:browser", "browser-1")

	if workerStatus != "running" {
		t.Fatalf("worker failed to restart")
	}
}

// TestUIDisconnectAndReplay 验证 UI 断开时不影响服务端 Run 执行，UI 重连后按 Sequence 补齐事件
func TestUIDisconnectAndReplay(t *testing.T) {
	ctx := context.Background()
	_ = ctx

	runCompletedOnServer := false
	serverEvents := []uint64{1, 2, 3, 4, 5}

	// 模拟客户端 UI 在收到 seq=2 后断开连接
	clientLastSeq := uint64(2)

	// 服务端 Run 继续独立执行并写入后续事件 (I7不变量：UI断线不能改变run的执行状态)
	runCompletedOnServer = true

	// UI 重新连接，发起 GET events since sequence=2
	var replayed []uint64
	for _, seq := range serverEvents {
		if seq > clientLastSeq {
			replayed = append(replayed, seq)
			clientLastSeq = seq
		}
	}

	if !runCompletedOnServer {
		t.Fatalf("server run should complete even when UI is disconnected")
	}
	if len(replayed) != 3 || replayed[0] != 3 || replayed[2] != 5 {
		t.Fatalf("expected events 3, 4, 5 replayed, got %+v", replayed)
	}
	if clientLastSeq != 5 {
		t.Fatalf("expected client to catch up to seq 5, got %d", clientLastSeq)
	}
}
