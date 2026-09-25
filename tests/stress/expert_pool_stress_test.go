package stress

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/bootstrap"
	"github.com/ximo888ok-netizen/ximo-agent/internal/config"
	"github.com/ximo888ok-netizen/ximo-agent/internal/expert"
	"github.com/ximo888ok-netizen/ximo-agent/internal/provider"
	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// 本文件是任务 05（多代理并行 + 独立资源类 + 模型池 + 失败转移）的压测口径：
//
//   - 用真实的 bootstrap 装配（真实 Engine/调度器/资源池 + 真实 provider 客户端），
//     服务商用本地 httptest 模拟 OpenAI 兼容 SSE 端点 —— 验收标准要求
//     「人为让第一个返回 429，观察自动换到第二个 provider」，mock server 是
//     任务书点名的手段；
//   - 失败转移与并发的行为细节由 internal/provider 与 internal/expert 的单测
//     确定性覆盖，这里补的是「配置 → 装配 → 真实 HTTP → 换模型跑完」的
//     端到端证据，以及走 Engine 共享资源池的并发上限。
//
// SSE 响应刻意用最小合法形态（data: choices[].delta.content + [DONE]），
// 与 provider 包 stream_test 的固件一致。

// sseAnswer 返回一段最小合法的 OpenAI 兼容流式响应。
func sseAnswer(content string) string {
	return "data: {\"choices\":[{\"delta\":{\"content\":\"" + content + "\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
}

// mockProviderServer 起一个本地 OpenAI 兼容端点：status 非 200 时按状态码拒绝
// （用于制造 429），否则返回 SSE 回答；并发计量供并行断言使用。
type mockProviderServer struct {
	server   *httptest.Server
	answer   string
	inFlight atomic.Int64
	peak     atomic.Int64
	latency  time.Duration
}

func newMockProviderServer(answer string) *mockProviderServer {
	m := &mockProviderServer{answer: answer}
	mux := http.NewServeMux()
	mux.HandleFunc("/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		cur := m.inFlight.Add(1)
		for {
			peak := m.peak.Load()
			if cur <= peak || m.peak.CompareAndSwap(peak, cur) {
				break
			}
		}
		defer m.inFlight.Add(-1)
		if m.latency > 0 {
			time.Sleep(m.latency)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, sseAnswer(m.answer))
	})
	m.server = httptest.NewServer(mux)
	return m
}

// rejectServer 始终返回 429（不带 Retry-After，配合单次尝试策略快速失败）。
func newRejectServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = fmt.Fprint(w, `{"error":{"message":"too many requests"}}`)
	}))
}

// newExpertPoolConfig 构造指向 mock 服务端的服务商池配置。
// 候选顺序 = 传入的服务商顺序（主服务商在前）。
func newExpertPoolConfig(t *testing.T, entries ...string) *config.Config {
	t.Helper()
	cfg := newStressConfig(t)
	cfg.Runtime.AutoMode = "yolo"
	primary := cfg.Provider
	for i, base := range entries {
		if i == 0 {
			primary.BaseURL = base
			primary.ID = "main"
			primary.Model = "model-main"
			cfg.Provider = primary
			continue
		}
		cfg.Providers = append(cfg.Providers, config.ProviderConfig{
			ID:      fmt.Sprintf("backup-%d", i),
			BaseURL: base,
			Model:   fmt.Sprintf("model-backup-%d", i),
		})
	}
	return cfg
}

// newExpertPoolApp 装配真实 App 并注入测试密钥解析器（池内客户端同样生效）。
func newExpertPoolApp(t *testing.T, cfg *config.Config) *bootstrap.App {
	t.Helper()
	app, err := bootstrap.New(cfg, bootstrap.Options{
		MigrationsDir: migrationsDir(t),
		SecretResolver: provider.SecretResolverFunc(func(context.Context, string) (string, error) {
			return "test-key", nil
		}),
		WorkspaceRoot:  t.TempDir(),
		DisableWorkers: true,
	})
	if err != nil {
		t.Fatalf("bootstrap.New: %v", err)
	}
	t.Cleanup(app.Close)
	return app
}

// firstExpertDivision 返回第一个专家的 ID 与分类（注册表加载是确定性的）。
func firstExpertDivision(t *testing.T) (string, string) {
	t.Helper()
	list, err := expert.NewRegistry(nil).Load()
	if err != nil {
		t.Fatalf("load registry: %v", err)
	}
	return list[0].ID, list[0].Division
}

// TestExpertSubAgentFailoverOn429 验收标准 2 的端到端证据：主服务商持续 429，
// 同一个 task 同一个 expert 自动换到第二个 provider 并把子任务跑完。
func TestExpertSubAgentFailoverOn429(t *testing.T) {
	reject := newRejectServer(t)
	defer reject.Close()
	backup := newMockProviderServer("backup-ok")
	defer backup.server.Close()

	cfg := newExpertPoolConfig(t, reject.URL, backup.server.URL)
	app := newExpertPoolApp(t, cfg)

	// 装配证据：候选池按配置顺序拿齐两个候选。
	pool, err := app.SubAgentPool()
	if err != nil {
		t.Fatalf("SubAgentPool: %v", err)
	}
	if pool == nil {
		t.Fatal("候选池不应为 nil")
	}
	if pool.Len() != 2 || pool.HealthyCount() != 2 {
		t.Fatalf("候选池应有 2 个健康候选, got len=%d healthy=%d", pool.Len(), pool.HealthyCount())
	}

	orch, err := app.ExpertOrchestrator()
	if err != nil {
		t.Fatalf("ExpertOrchestrator: %v", err)
	}
	expertID, _ := firstExpertDivision(t)

	out, err := orch.Activate(context.Background(), expert.ExpertRequest{
		ExpertID: expertID,
		Task:     "把同一个子任务跑完",
	})
	if err != nil {
		t.Fatalf("激活失败: %v", err)
	}
	if out.Error != "" {
		t.Fatalf("失败转移后不应报错: %s", out.Error)
	}
	if !out.SubAgentMode {
		t.Fatalf("应处于子代理模式: %+v", out)
	}
	if !strings.Contains(out.Content, "backup-ok") {
		t.Fatalf("回答应来自备用模型, got %q", out.Content)
	}
	// 过程事件要能看出「为什么慢了」。
	found := false
	for _, ev := range out.Events {
		if strings.Contains(ev.Detail, "main") && strings.Contains(ev.Detail, "backup-1") {
			found = true
		}
	}
	if !found {
		t.Fatalf("缺少失败转移说明事件: %+v", out.Events)
	}
}

// TestExpertSubAgentsParallelWithinCap 验收标准 3 的端到端证据：同时触发 4 个
// 不同专家的子任务，真实并行执行，且走的是 Engine 的同一个资源池
// （expert_agent 容量 8），结束後槽位全部归还。
func TestExpertSubAgentsParallelWithinCap(t *testing.T) {
	backend := newMockProviderServer("parallel-ok")
	backend.latency = 150 * time.Millisecond
	defer backend.server.Close()

	cfg := newExpertPoolConfig(t, backend.server.URL)
	app := newExpertPoolApp(t, cfg)
	orch, err := app.ExpertOrchestrator()
	if err != nil {
		t.Fatalf("ExpertOrchestrator: %v", err)
	}

	// 资源槽位必须来自 Engine 的调度器资源池 —— 换一个自建池就不是 8 的真上限。
	resources := app.Engine.Scheduler().Resources()
	if resources == nil {
		t.Fatal("Engine 资源池不可用")
	}
	if got := resources.Capacity(types.ResourceClassExpertAgent); got != 8 {
		t.Fatalf("expert_agent 容量 = %d, want 8", got)
	}

	list, err := expert.NewRegistry(nil).Load()
	if err != nil {
		t.Fatalf("load registry: %v", err)
	}
	const experts = 4
	var wg sync.WaitGroup
	for _, e := range list[:experts] {
		wg.Add(1)
		go func(expertID string) {
			defer wg.Done()
			out, err := orch.Activate(context.Background(), expert.ExpertRequest{
				ExpertID: expertID,
				Task:     "并行子任务",
			})
			if err != nil {
				t.Errorf("专家 %s 激活失败: %v", expertID, err)
				return
			}
			if out.Error != "" {
				t.Errorf("专家 %s 子任务失败: %s", expertID, out.Error)
			}
		}(e.ID)
	}
	wg.Wait()

	// 150ms 的窗口内 4 个请求必然重叠：重叠数 < 4 说明被错误串行化；
	// 资源类失效则可能 > 8（此处用真实引擎池，>8 等价于没有闸门）。
	if got := backend.peak.Load(); got != experts {
		t.Fatalf("4 个专家应并行执行, 服务端峰值并发 = %d", got)
	}
	if got := resources.InUse(types.ResourceClassExpertAgent); got != 0 {
		t.Fatalf("结束后槽位应全部归还, InUse = %d", got)
	}
}

// TestExpertSubAgentDivisionAssignment 验收标准 4 的链路证据：分类维度的候选
// 分配能穿过装配层直达模型池（被分配的分类优先用指定候选）。
func TestExpertSubAgentDivisionAssignment(t *testing.T) {
	reject := newRejectServer(t)
	defer reject.Close()
	backup := newMockProviderServer("assigned-ok")
	defer backup.server.Close()

	cfg := newExpertPoolConfig(t, reject.URL, backup.server.URL)
	expertID, division := firstExpertDivision(t)
	// 该分类先主后备：主候选 429 后失败转移到后备 —— 分配顺序真实生效。
	cfg.SubAgent.ByDivision = map[string][]string{division: {"main", "backup-1"}}
	app := newExpertPoolApp(t, cfg)

	orch, err := app.ExpertOrchestrator()
	if err != nil {
		t.Fatalf("ExpertOrchestrator: %v", err)
	}
	out, err := orch.Activate(context.Background(), expert.ExpertRequest{
		ExpertID: expertID,
		Task:     "按分类分配跑子任务",
	})
	if err != nil {
		t.Fatalf("激活失败: %v", err)
	}
	if !strings.Contains(out.Content, "assigned-ok") {
		t.Fatalf("回答应来自分配的候选, got %q", out.Content)
	}
}
