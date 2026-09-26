package memory

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// recordingMem0 是一个只记录调用的假 mem0，用来断言「写了几次、写了什么」。
type recordingMem0 struct {
	mu    sync.Mutex
	calls []map[string]any
	fail  bool
}

func (r *recordingMem0) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, req *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(req.Body).Decode(&body)
		r.mu.Lock()
		r.calls = append(r.calls, body)
		fail := r.fail
		r.mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"results":[{"id":"m1","memory":"已提取"}]}`))
	}
}

func (r *recordingMem0) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func (r *recordingMem0) last() map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.calls) == 0 {
		return nil
	}
	return r.calls[len(r.calls)-1]
}

// countingGate 是内存版幂等闸，与生产实现（tool_idempotency 表的 Claim）同语义。
type countingGate struct {
	mu     sync.Mutex
	claims map[string]int
}

func (g *countingGate) Claim(_ context.Context, key string) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.claims == nil {
		g.claims = map[string]int{}
	}
	g.claims[key]++
	return g.claims[key] == 1, nil
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待超时: %s", what)
}

func newWriteBackService(t *testing.T, mem *recordingMem0, gate Gate, tweak func(*Config)) *Service {
	t.Helper()
	srv := httptest.NewServer(mem.handler(t))
	t.Cleanup(srv.Close)
	cfg := Config{
		Enabled:        true,
		Endpoint:       srv.URL,
		UserID:         "u",
		Timeout:        time.Second,
		ExtractTimeout: 2 * time.Second,
		WriteBack:      true,
		QueueDepth:     4,
		MaxInflight:    2,
	}
	if tweak != nil {
		tweak(&cfg)
	}
	return NewService(cfg, NewClient(cfg, ClientOptions{HTTPClient: srv.Client()}), gate)
}

func TestWriteBackExtractsTurnOnce(t *testing.T) {
	mem := &recordingMem0{}
	gate := &countingGate{}
	svc := newWriteBackService(t, mem, gate, nil)
	defer svc.Close()

	turn := Turn{RunID: "run-1", SessionID: "s1", Prompt: "以后都用暗色主题", Answer: "好的"}
	svc.Remember(turn)
	// 同一个 run 再投递两次（模拟取消 / 崩溃恢复导致的重复收尾）。
	svc.Remember(turn)
	svc.Remember(turn)

	waitFor(t, "回填完成", func() bool { return svc.Stats().BackfillDone >= 1 })
	time.Sleep(50 * time.Millisecond) // 给多余投递一个落地机会

	if got := mem.count(); got != 1 {
		t.Fatalf("同一个 run 只应抽取一次，实际 %d 次", got)
	}
	body := mem.last()
	if body["run_id"] != "run-1" || body["user_id"] != "u" {
		t.Fatalf("归属字段不符: %v", body)
	}
	meta, _ := body["metadata"].(map[string]any)
	if meta["session_id"] != "s1" {
		t.Fatalf("metadata 应带上 session_id: %v", body["metadata"])
	}
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("一轮问答应折成 2 条消息: %v", body["messages"])
	}
	stats := svc.Stats()
	if stats.BackfillSkipped < 1 {
		t.Fatalf("重复投递应计入 skipped: %+v", stats)
	}
}

func TestWriteBackFailureIsCountedNotFatal(t *testing.T) {
	mem := &recordingMem0{fail: true}
	svc := newWriteBackService(t, mem, nil, nil)
	defer svc.Close()

	svc.Remember(Turn{RunID: "run-x", Prompt: "p", Answer: "a"})
	waitFor(t, "失败被计数", func() bool { return svc.Stats().BackfillFailed >= 1 })

	// 抽取失败不影响服务继续可用：后续 run 仍能投递并成功。
	mem.mu.Lock()
	mem.fail = false
	mem.mu.Unlock()
	svc.Remember(Turn{RunID: "run-y", Prompt: "p2", Answer: "a2"})
	waitFor(t, "后续 run 成功", func() bool { return svc.Stats().BackfillDone >= 1 })
}

func TestWriteBackDropsInsteadOfBlockingWhenQueueIsFull(t *testing.T) {
	mem := &recordingMem0{}
	// MaxInflight=1 + 队列 1：第 3 次投递时队列已满，必须丢弃而不是阻塞调用方。
	svc := newWriteBackService(t, mem, &countingGate{}, func(c *Config) {
		c.QueueDepth = 1
		c.MaxInflight = 1
	})
	defer svc.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			svc.Remember(Turn{RunID: "run-" + string(rune('a'+i%26)) + string(rune('0'+i/26)), Prompt: "p", Answer: "a"})
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("投递被阻塞了：Remember 必须是非阻塞的")
	}
	waitFor(t, "出现丢弃计数", func() bool { return svc.Stats().BackfillDropped > 0 })
}

func TestWriteBackDisabledByConfig(t *testing.T) {
	mem := &recordingMem0{}
	svc := newWriteBackService(t, mem, nil, func(c *Config) { c.WriteBack = false })
	defer svc.Close()

	svc.Remember(Turn{RunID: "run-1", Prompt: "p", Answer: "a"})
	time.Sleep(50 * time.Millisecond)
	if mem.count() != 0 {
		t.Fatal("write_back=false 时不应发起抽取")
	}
	if svc.Stats().BackfillEnqueued != 0 {
		t.Fatal("write_back=false 时不应入队")
	}
}

func TestWriteBackSkipsEmptyTurns(t *testing.T) {
	mem := &recordingMem0{}
	svc := newWriteBackService(t, mem, nil, nil)
	defer svc.Close()

	svc.Remember(Turn{RunID: "", Prompt: "p", Answer: "a"})     // 没有 run id
	svc.Remember(Turn{RunID: "run-1", Prompt: "", Answer: "a"}) // 没有提问
	svc.Remember(Turn{RunID: "run-2", Prompt: "p", Answer: ""}) // 没有答复
	time.Sleep(50 * time.Millisecond)
	if mem.count() != 0 {
		t.Fatal("不完整的轮次不应触发抽取")
	}
}

func TestServiceCloseIsIdempotentAndBounded(t *testing.T) {
	mem := &recordingMem0{}
	svc := newWriteBackService(t, mem, nil, nil)
	svc.Remember(Turn{RunID: "run-1", Prompt: "p", Answer: "a"})

	start := time.Now()
	svc.Close()
	svc.Close() // 重复关闭必须安全
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("Close 等待过久: %v", elapsed)
	}
	if !svc.Stats().Closed {
		t.Fatal("Close 后 Stats 应报告已关闭")
	}
}

func TestTurnMessagesShape(t *testing.T) {
	if got := (Turn{RunID: "r", Prompt: "p", Answer: "a"}).Messages(); len(got) != 2 ||
		got[0].Role != "user" || got[1].Role != "assistant" {
		t.Fatalf("一轮问答应折成 user+assistant: %+v", got)
	}
	if got := (Turn{Prompt: "p"}).Messages(); len(got) != 1 || got[0].Role != "user" {
		t.Fatalf("只有提问时应只有一条: %+v", got)
	}
	if got := (Turn{}).Messages(); len(got) != 0 {
		t.Fatalf("空轮次应没有消息: %+v", got)
	}
}

func TestExtractionKeyIsPerRun(t *testing.T) {
	if ExtractionKey("run-1") == ExtractionKey("run-2") {
		t.Fatal("不同 run 的幂等键必须不同")
	}
	if ExtractionKey("run-1") == "" {
		t.Fatal("幂等键不能为空")
	}
}

func TestGateErrorIsCountedAndSkipsExtraction(t *testing.T) {
	mem := &recordingMem0{}
	svc := newWriteBackService(t, mem, errGate{}, nil)
	defer svc.Close()

	svc.Remember(Turn{RunID: "run-1", Prompt: "p", Answer: "a"})
	waitFor(t, "闸门错误被计数", func() bool { return svc.Stats().BackfillFailed >= 1 })
	if mem.count() != 0 {
		t.Fatal("认领失败时不应发起抽取")
	}
}

type errGate struct{}

func (errGate) Claim(context.Context, string) (bool, error) {
	return false, errors.New("存储不可用")
}
