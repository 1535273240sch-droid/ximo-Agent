package bootstrap

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/ximo888ok-netizen/ximo-agent/internal/ports"
	"github.com/ximo888ok-netizen/ximo-agent/internal/provider"
)

// adapterSSE 是最小可解析的 OpenAI 兼容 SSE 补全（一个内容分片 + [DONE]）。
const adapterSSE = "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\ndata: [DONE]\n\n"

// fakeUpstream 是假的 OpenAI 兼容上游，只记录每个请求体里的 model 字段并按
// SSE 回一段固定补全。它只存在于本测试文件里，生产路径不引用任何模拟数据。
type fakeUpstream struct {
	server *httptest.Server
	mu     sync.Mutex
	models []string
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	t.Helper()
	up := &fakeUpstream{}
	up.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(raw, &body)

		up.mu.Lock()
		up.models = append(up.models, body.Model)
		up.mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, adapterSSE)
	}))
	t.Cleanup(up.server.Close)
	return up
}

// seenModels 返回上游依次收到的模型名（副本）。
func (u *fakeUpstream) seenModels() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]string, len(u.models))
	copy(out, u.models)
	return out
}

// newAdapterForTest 构造指向假上游的真 providerAdapter，重试关到 1 次避免单测退避。
func newAdapterForTest(t *testing.T, baseURL, defaultModel string) *providerAdapter {
	t.Helper()
	client, err := provider.NewClient(provider.ClientOptions{
		Config: provider.ProviderConfig{
			ID:              "fake",
			Name:            "fake",
			BaseURL:         baseURL,
			MaxOutputTokens: 64,
		},
		Secrets: provider.SecretResolverFunc(func(context.Context, string) (string, error) {
			return "test-key", nil
		}),
		Retry: provider.RetryPolicy{MaxAttempts: 1},
	})
	if err != nil {
		t.Fatalf("构造 provider 客户端失败: %v", err)
	}
	return newProviderAdapter(client, defaultModel)
}

// TestProviderAdapterCompleteEchoesRequestModel 断言非流式路径：当调用方显式指定
// 模型时，发到上游的 model 与响应回传的 Model 完全一致 —— 这就是「选中的模型
// 真的被用了」的证据链。
func TestProviderAdapterCompleteEchoesRequestModel(t *testing.T) {
	up := newFakeUpstream(t)
	adapter := newAdapterForTest(t, up.server.URL, "default-model")

	resp, err := adapter.Complete(context.Background(), ports.ProviderRequest{
		Model:    "chosen-model",
		Messages: []ports.Message{{Role: ports.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete 报错: %v", err)
	}
	if resp.Model != "chosen-model" {
		t.Fatalf("响应回传 Model = %q, want %q", resp.Model, "chosen-model")
	}
	seen := up.seenModels()
	if len(seen) != 1 || seen[0] != "chosen-model" {
		t.Fatalf("上游收到的 model = %v, want [chosen-model]", seen)
	}
	if resp.Content != "hello" {
		t.Fatalf("Content = %q, want hello", resp.Content)
	}
}

// TestProviderAdapterCompleteEchoesDefaultModel 断言「跟随全局默认」场景：请求里
// 的 Model 为空，适配器回退到默认模型后必须把回退结果回传，否则 UI 无法区分
// 「用了默认模型」和「没拿到模型信息」。这正是 ProviderResponse.Model 注释里
// 说的那条理由。
func TestProviderAdapterCompleteEchoesDefaultModel(t *testing.T) {
	up := newFakeUpstream(t)
	adapter := newAdapterForTest(t, up.server.URL, "default-model")

	resp, err := adapter.Complete(context.Background(), ports.ProviderRequest{
		Messages: []ports.Message{{Role: ports.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete 报错: %v", err)
	}
	if resp.Model != "default-model" {
		t.Fatalf("响应回传 Model = %q, want %q", resp.Model, "default-model")
	}
	seen := up.seenModels()
	if len(seen) != 1 || seen[0] != "default-model" {
		t.Fatalf("上游收到的 model = %v, want [default-model]", seen)
	}
}

// TestProviderAdapterStreamingEchoesActualModel 断言流式路径同样回传实际模型名，
// 且增量回调照常工作（回传字段的加入不改变流式语义）。
func TestProviderAdapterStreamingEchoesActualModel(t *testing.T) {
	up := newFakeUpstream(t)
	adapter := newAdapterForTest(t, up.server.URL, "default-model")

	var deltas []string
	resp, err := adapter.Complete(context.Background(), ports.ProviderRequest{
		Model:    "chosen-stream-model",
		Messages: []ports.Message{{Role: ports.RoleUser, Content: "hi"}},
		OnDelta: func(d ports.Delta) {
			if d.Content != "" {
				deltas = append(deltas, d.Content)
			}
		},
	})
	if err != nil {
		t.Fatalf("流式 Complete 报错: %v", err)
	}
	if resp.Model != "chosen-stream-model" {
		t.Fatalf("流式响应回传 Model = %q, want %q", resp.Model, "chosen-stream-model")
	}
	seen := up.seenModels()
	if len(seen) != 1 || seen[0] != "chosen-stream-model" {
		t.Fatalf("上游收到的 model = %v, want [chosen-stream-model]", seen)
	}
	if resp.Content != "hello" || len(deltas) != 1 || deltas[0] != "hello" {
		t.Fatalf("流式内容/增量异常: content=%q deltas=%v", resp.Content, deltas)
	}
}
