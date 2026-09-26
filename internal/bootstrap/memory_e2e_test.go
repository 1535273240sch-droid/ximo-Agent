package bootstrap_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/bootstrap"
	"github.com/ximo888ok-netizen/ximo-agent/internal/config"
	"github.com/ximo888ok-netizen/ximo-agent/internal/memory"
	"github.com/ximo888ok-netizen/ximo-agent/internal/ports"
	"github.com/ximo888ok-netizen/ximo-agent/internal/ports/mem"
	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// 本文件是「长期记忆在**真实装配**里接通了」的可执行证据。
//
// 与 internal/memory 的单测分工不同：那里验的是客户端与 mem0 的报文契约，这里
// 走的是完整链路——config.json → bootstrap 装配 → memory.Service → HTTP →
// Engine 注入 → 模型请求 → 收尾回填。中间任何一段没接上，这里都会失败。
//
// 用 HTTP 替身而不是真 mem0：真服务需要 Docker + Postgres + 一份向量模型密钥，
// 不能让单元测试依赖它们。替身只实现本工程真正用到的那几个端点，形状取自
// mem0 仓库的 server/main.py。

// fakeMem0 是 mem0 自托管服务的 HTTP 替身。
type fakeMem0 struct {
	mu       sync.Mutex
	searches []map[string]any
	adds     []map[string]any
	calls    int
	// memory 是 /search 会返回的记忆正文。
	memory string
}

func (f *fakeMem0) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
		f.mu.Lock()
		f.calls++
		isSearch := r.Method == http.MethodPost && r.URL.Path == "/search"
		switch {
		case isSearch:
			f.searches = append(f.searches, body)
		case r.Method == http.MethodPost && r.URL.Path == "/memories":
			f.adds = append(f.adds, body)
		}
		text := f.memory
		f.mu.Unlock()

		if isSearch {
			_, _ = fmt.Fprintf(w, `{"results":[{"id":"m1","memory":%q,"score":0.9}]}`, text)
			return
		}
		_, _ = w.Write([]byte(`{"results":[]}`))
	}
}

func (f *fakeMem0) snapshot() (searches, adds []map[string]any, calls int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	searches = append([]map[string]any(nil), f.searches...)
	adds = append([]map[string]any(nil), f.adds...)
	return searches, adds, f.calls
}

// enabledMemoryConfig 是「用户按文档把记忆打开」的那份配置。
func enabledMemoryConfig(t *testing.T, endpoint string) *config.Config {
	t.Helper()
	cfg := newTestConfig(t)
	cfg.Memory = config.MemoryConfig{
		Enabled:        true,
		Endpoint:       endpoint,
		UserID:         "e2e-user",
		Timeout:        2 * time.Second,
		ExtractTimeout: 5 * time.Second,
	}
	cfg.FeatureFlags[config.FlagMemory] = true
	return cfg
}

func TestAssemble_MemoryRecallsAndBackfillsEndToEnd(t *testing.T) {
	mem0 := &fakeMem0{memory: "用户要求所有界面用暗色主题"}
	srv := httptest.NewServer(mem0.handler())
	defer srv.Close()

	cfg := enabledMemoryConfig(t, srv.URL)
	scripted := mem.NewProvider(ports.ProviderResponse{
		FinishReason: ports.FinishStop,
		Content:      "好的，已按暗色主题处理",
		Emitted:      true,
	})

	app, err := bootstrap.New(cfg, bootstrap.Options{
		MigrationsDir:  migrationsDir(t),
		Provider:       scripted,
		WorkspaceRoot:  t.TempDir(),
		DisableWorkers: true,
	})
	if err != nil {
		t.Fatalf("bootstrap.New: %v", err)
	}
	defer app.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const prompt = "帮我调一下配色"
	handle, err := app.Engine.Submit(ctx, types.SubmitRequest{
		SessionID:    "sess-memory-e2e",
		Prompt:       prompt,
		SystemPrompt: "系统提示词",
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if final := waitForTerminal(t, ctx, app, handle.RunID); final.State != types.StateCompleted {
		t.Fatalf("run state = %s (err %v), want completed", final.State, final.Err)
	}

	searches, adds, _ := mem0.snapshot()

	// ---- 读路径：召回真的发出去了，且查询词与归属正确 ----
	if len(searches) != 1 {
		t.Fatalf("应当恰好检索一次，实际 %d 次", len(searches))
	}
	if searches[0]["query"] != prompt {
		t.Fatalf("检索查询词应为本轮提示，实际 %v", searches[0]["query"])
	}
	filters, _ := searches[0]["filters"].(map[string]any)
	if filters["user_id"] != "e2e-user" {
		t.Fatalf("检索应限定在配置的 user_id 上，实际 %v", searches[0]["filters"])
	}

	// ---- 注入：模型收到的请求里，记忆是紧随系统提示词的独立 system 消息 ----
	if len(scripted.Calls) == 0 {
		t.Fatal("provider 未被调用")
	}
	first := scripted.Calls[0]
	if len(first.Messages) != 3 {
		t.Fatalf("消息表应为 [system, memory, user]，实际 %d 条: %+v", len(first.Messages), first.Messages)
	}
	if first.Messages[0].Content != "系统提示词" {
		t.Fatalf("系统提示词必须仍在最前，实际 %q", first.Messages[0].Content)
	}
	if first.Messages[1].Role != ports.RoleSystem ||
		!strings.Contains(first.Messages[1].Content, memory.BlockHeader) {
		t.Fatalf("第二条应为记忆 system 消息，实际 %+v", first.Messages[1])
	}
	if !strings.Contains(first.Messages[1].Content, "暗色主题") {
		t.Fatalf("记忆内容应来自 mem0 的应答，实际 %q", first.Messages[1].Content)
	}
	if first.Messages[2].Content != prompt {
		t.Fatalf("用户消息位置被挤动，实际 %+v", first.Messages[2])
	}

	// ---- 写路径：收尾回填（异步，等到为止） ----
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, pending, _ := mem0.snapshot(); len(pending) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	_, adds, _ = mem0.snapshot()
	if len(adds) != 1 {
		t.Fatalf("应当恰好回填一次，实际 %d 次", len(adds))
	}
	if adds[0]["user_id"] != "e2e-user" || adds[0]["run_id"] != handle.RunID {
		t.Fatalf("回填归属不符: %v", adds[0])
	}
	msgs, _ := adds[0]["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("回填应带上本轮问答两条消息，实际 %v", adds[0]["messages"])
	}
	userMsg, _ := msgs[0].(map[string]any)
	assistantMsg, _ := msgs[1].(map[string]any)
	if userMsg["content"] != prompt {
		t.Fatalf("回填的提问不符: %v", userMsg)
	}
	if assistantMsg["content"] != "好的，已按暗色主题处理" {
		t.Fatalf("回填的答复应为模型最终答复，实际 %v", assistantMsg)
	}
}

// TestAssemble_MemoryFeatureFlagOffIsInert 覆盖两级开关中的熔断那一级：
// 配置段打开了，但特性开关为关——整条链路必须一次请求都不发。
func TestAssemble_MemoryFeatureFlagOffIsInert(t *testing.T) {
	mem0 := &fakeMem0{memory: "不该被看到"}
	srv := httptest.NewServer(mem0.handler())
	defer srv.Close()

	cfg := enabledMemoryConfig(t, srv.URL)
	cfg.FeatureFlags[config.FlagMemory] = false

	scripted := mem.NewProvider(ports.ProviderResponse{
		FinishReason: ports.FinishStop, Content: "完成", Emitted: true,
	})
	app, err := bootstrap.New(cfg, bootstrap.Options{
		MigrationsDir:  migrationsDir(t),
		Provider:       scripted,
		WorkspaceRoot:  t.TempDir(),
		DisableWorkers: true,
	})
	if err != nil {
		t.Fatalf("bootstrap.New: %v", err)
	}
	defer app.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	handle, err := app.Engine.Submit(ctx, types.SubmitRequest{Prompt: "干点活", SystemPrompt: "系统提示词"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if final := waitForTerminal(t, ctx, app, handle.RunID); final.State != types.StateCompleted {
		t.Fatalf("run state = %s, want completed", final.State)
	}

	if _, _, calls := mem0.snapshot(); calls != 0 {
		t.Fatalf("特性开关为关时不应发出任何 mem0 请求，实际 %d 次", calls)
	}
	if first := scripted.Calls[0]; len(first.Messages) != 2 {
		t.Fatalf("消息表应保持 [system, user]，实际 %+v", first.Messages)
	}
}

// TestAssemble_MemoryBackendDownStillCompletes 是降级证据：mem0 连不上时，
// run 必须照常跑完，不能失败、不能卡住。
func TestAssemble_MemoryBackendDownStillCompletes(t *testing.T) {
	cfg := enabledMemoryConfig(t, "http://127.0.0.1:1") // 该端口无人监听
	cfg.Memory.Timeout = 300 * time.Millisecond

	scripted := mem.NewProvider(ports.ProviderResponse{
		FinishReason: ports.FinishStop, Content: "照常完成", Emitted: true,
	})
	app, err := bootstrap.New(cfg, bootstrap.Options{
		MigrationsDir:  migrationsDir(t),
		Provider:       scripted,
		WorkspaceRoot:  t.TempDir(),
		DisableWorkers: true,
	})
	if err != nil {
		t.Fatalf("bootstrap.New: %v", err)
	}
	defer app.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	start := time.Now()
	handle, err := app.Engine.Submit(ctx, types.SubmitRequest{Prompt: "干点活", SystemPrompt: "系统提示词"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	final := waitForTerminal(t, ctx, app, handle.RunID)
	if final.State != types.StateCompleted || final.Answer != "照常完成" {
		t.Fatalf("记忆不可用不应影响 run: state=%s answer=%q err=%v", final.State, final.Answer, final.Err)
	}
	// 召回失败的等待必须有界：不能把 300ms 的超时拖成秒级。
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("记忆不可用时 run 耗时异常: %v", elapsed)
	}

	// 模型收到的请求不应带上任何记忆消息。
	if first := scripted.Calls[0]; len(first.Messages) != 2 {
		t.Fatalf("召回失败时不应注入消息，实际 %+v", first.Messages)
	}
}
