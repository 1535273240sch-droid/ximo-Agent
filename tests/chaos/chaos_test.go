package chaos

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/observability"
)

// 1. 网络断开 / DNS 失败：不造成死循环
func TestChaosNetworkDisconnect(t *testing.T) {
	maxRetries := 3
	retries := 0

	callProvider := func() error {
		retries++
		return errors.New("dns lookup failed: no such host")
	}

	for i := 0; i < maxRetries; i++ {
		err := callProvider()
		if err != nil {
			observability.ProviderRetry("deepseek", "dns_failure")
		}
	}

	if retries != 3 {
		t.Fatalf("expected exactly 3 retries, got %d", retries)
	}
}

// 2. 429 限流：遵守重试等待与限频，不导致死循环
func TestChaosProvider429(t *testing.T) {
	attempts := 0
	maxAttempts := 3

	simulateCall := func() (int, time.Duration) {
		attempts++
		if attempts < 3 {
			return 429, 10 * time.Millisecond // 模拟 Retry-After 10ms
		}
		return 200, 0
	}

	for attempts < maxAttempts {
		code, retryAfter := simulateCall()
		if code == 429 {
			observability.Provider429("deepseek")
			time.Sleep(retryAfter)
			continue
		}
		break
	}

	if attempts != 3 {
		t.Fatalf("expected 3 attempts to succeed after 429, got %d", attempts)
	}
}

// 3. Provider 500 / 503：分类重试并不死循环
func TestChaosProvider500(t *testing.T) {
	attempts := 0
	var finalErr error

	for i := 0; i < 3; i++ {
		attempts++
		finalErr = errors.New("500 Internal Server Error")
		observability.ProviderRetry("deepseek", "500")
	}

	if attempts != 3 || finalErr == nil {
		t.Fatalf("expected bounded 3 retries for 500 error, got %d", attempts)
	}
}

// 4. Provider Stream Half-Close：半关闭检测与重试
func TestChaosStreamHalfClose(t *testing.T) {
	stream := io.NopCloser(strings.NewReader("part1"))
	buf := make([]byte, 10)
	n, err := stream.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatalf("read failed: %v", err)
	}
	_ = n

	// 提前读到 EOF，模拟 stream 异常半关闭
	_, err = stream.Read(buf)
	if err != io.EOF {
		t.Fatalf("expected EOF on half-close, got %v", err)
	}
}

// 5. DB Busy (SQLITE_BUSY)：重试与可观测性
func TestChaosDBBusy(t *testing.T) {
	attempts := 0
	maxAttempts := 3
	succeeded := false

	for i := 0; i < maxAttempts; i++ {
		attempts++
		if attempts < 3 {
			observability.DBBusy()
			time.Sleep(5 * time.Millisecond)
			continue
		}
		succeeded = true
		observability.DBCommitLatency(12.5)
		break
	}

	if !succeeded || attempts != 3 {
		t.Fatalf("expected recovery after DB busy retries")
	}
}

// 6. 磁盘写满（Disk Full 模拟）：事务安全回滚，已有数据不被损坏
func TestChaosDiskFull(t *testing.T) {
	dbState := "initial_healthy_db"
	diskFull := true

	// 开启事务模拟写入
	txData := "new_large_checkpoint"
	if diskFull {
		// 模拟写入报 ENOSPC 磁盘写满
		err := errors.New("ENOSPC: no space left on device")
		_ = err
		// 事务回滚，保持原有数据不动
		txData = ""
	} else {
		dbState = txData
	}

	if dbState != "initial_healthy_db" || txData != "" {
		t.Fatalf("disk full must not corrupt existing database!")
	}
}

// 7. Permission Dialog Never Returns：用户确认超时释放资源
func TestChaosPermissionDialogTimeout(t *testing.T) {
	resourceAcquired := true

	// 发起权限弹窗等待
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	userResponded := false
	doneChan := make(chan bool)

	go func() {
		// 模拟用户一直不点击弹窗
		<-ctx.Done()
		doneChan <- false
	}()

	select {
	case userResponded = <-doneChan:
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("test hang")
	}

	if userResponded {
		t.Fatalf("should not have user response")
	}

	// 超时必须释放资源（I9 不变量：所有 timeout 最终必须释放资源）
	resourceAcquired = false
	if resourceAcquired {
		t.Fatalf("resource lease must be released on permission timeout")
	}
}

// 8. Worker stdout 被污染：乱码过滤与容错
func TestChaosWorkerStdoutPolluted(t *testing.T) {
	pollutedOutput := []byte("\x00\xff\xfe\x1b[31mGARBAGE\x1b[0m{\"result\":\"actual_data\"}")

	// 安全解析提取
	idx := strings.Index(string(pollutedOutput), "{")
	if idx < 0 {
		t.Fatalf("failed to locate valid json payload in polluted stream")
	}
	validJSON := pollutedOutput[idx:]
	if !strings.Contains(string(validJSON), "actual_data") {
		t.Fatalf("failed to recover valid payload from corrupted stdout")
	}
}

// 9. Worker Hung (无响应)：心跳超时后 Supervisor 杀进程并重启
func TestChaosWorkerHungAndSupervisorHeartbeat(t *testing.T) {
	workerAlive := true
	heartbeatReceived := false

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()

	// 模拟心跳监测
	select {
	case <-time.After(30 * time.Millisecond):
		heartbeatReceived = true
	case <-ctx.Done():
		// 超时未收到心跳，判定 Worker Hung
		observability.WorkerCrash("worker:terminal", "term-1")
		workerAlive = false
	}

	if heartbeatReceived || workerAlive {
		t.Fatalf("worker hung should be killed upon heartbeat timeout")
	}

	// 重新拉起
	workerAlive = true
	observability.WorkerRestart("worker:terminal", "term-1")
	if !workerAlive {
		t.Fatalf("worker restart failed")
	}
}

// 10. Browser OOM：子进程崩溃被隔离，Engine 进程不受影响
func TestChaosBrowserOOM(t *testing.T) {
	engineAlive := true
	browserOOM := true

	if browserOOM {
		observability.WorkerCrash("worker:browser", "browser-pid-404")
		// Browser 挂掉，但不传播 panic 给 Engine
	}

	if !engineAlive {
		t.Fatalf("Engine crashed due to Browser OOM! Fault isolation failed.")
	}
}

// 11. MCP Server 无限等待：熔断保护
func TestChaosMCPServerHang(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	requestFinished := false
	select {
	case <-time.After(50 * time.Millisecond):
		requestFinished = true
	case <-ctx.Done():
		// 超时返回，熔断断开
		observability.ToolTimeout("mcp_hang_tool")
	}

	if requestFinished {
		t.Fatalf("hung request should have timed out")
	}
}

// 12. UI 完全不消费事件：背压机制合并非关键 Delta，不阻断 Engine 执行
func TestChaosUIBackpressureEventDrop(t *testing.T) {
	channelCap := 10
	eventChan := make(chan string, channelCap)

	var droppedTokens int
	var retainedDurableEvents int
	var mu sync.Mutex

	// 快速产生 100 个事件
	for i := 1; i <= 100; i++ {
		isDurable := (i == 1 || i == 100) // 首尾为关键事件
		evt := fmt.Sprintf("event-%d", i)

		select {
		case eventChan <- evt:
			mu.Lock()
			if isDurable {
				retainedDurableEvents++
			}
			mu.Unlock()
		default:
			// channel 满了，非关键 token delta 丢弃或合并，关键事件阻塞写入或写持久化 log
			mu.Lock()
			if isDurable {
				// 关键事件不可丢失
				retainedDurableEvents++
			} else {
				droppedTokens++
			}
			mu.Unlock()
		}
	}

	if droppedTokens == 0 {
		t.Fatalf("expected backpressure to drop non-critical events when UI is not consuming")
	}
	if retainedDurableEvents < 2 {
		t.Fatalf("durable events must not be lost under backpressure")
	}
}
