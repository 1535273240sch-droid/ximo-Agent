package agents

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestClientCallsAgainstFakeAgent 用进程内假服务端验证帧收发与三类调用
// （config.get / secret.put / config.set），并顺带覆盖 model.list 与心跳。
func TestClientCallsAgainstFakeAgent(t *testing.T) {
	initial := RuntimeSettings{
		ProviderID:      "custom",
		ProviderName:    "第三方接口",
		BaseURL:         "https://api.openai.com/v1",
		Model:           "gpt-4o",
		SecretRef:       "",
		WorkspaceRoot:   "/work/keep-me",
		ContextWindow:   131072,
		MaxOutputTokens: 8192,
		AutoMode:        "coding",
		ConfigPath:      `C:\fake\config.json`,
		Providers:       []json.RawMessage{json.RawMessage(`{"id":"pool-a"}`)},
	}
	agent := newFakeAgent(t, initial)
	agent.modelLists = []ModelInfo{{ID: "gw-model-small"}, {ID: "gw-model-large"}}
	// 让服务端每次响应前先塞一条无关广播帧：客户端必须按 RequestID 配对，
	// 否则会把广播当成响应，后面全部错位。
	agent.preludeFrames = 1

	ctx := context.Background()
	c := NewClient(agent.endpoint, 3*time.Second)
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer c.Close()

	if err := c.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	cur, err := c.Settings(ctx)
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}
	if cur.BaseURL != initial.BaseURL || cur.Model != initial.Model || cur.WorkspaceRoot != initial.WorkspaceRoot {
		t.Fatalf("Settings 返回值不符: %+v", cur)
	}
	if cur.ConfigPath != initial.ConfigPath {
		t.Fatalf("ConfigPath = %q, want %q", cur.ConfigPath, initial.ConfigPath)
	}
	if len(cur.Providers) != 1 {
		t.Fatalf("候选池未回传: %+v", cur.Providers)
	}

	// secret.put：明文只在请求里出现一次，响应只回引用。
	key := "sk-fake-call-987654321"
	ref, err := c.PutSecret(ctx, key)
	if err != nil {
		t.Fatalf("PutSecret: %v", err)
	}
	if want := SecretRefForValue(key); ref != want {
		t.Fatalf("ref = %q, want %q", ref, want)
	}
	if ref == key || strings.Contains(ref, key) {
		t.Fatalf("响应里的引用泄露了明文: %q", ref)
	}

	// 空值必须被本地拒掉，不能发出去。
	if _, err := c.PutSecret(ctx, ""); err == nil {
		t.Fatal("空密钥没有被拒绝")
	}

	models, err := c.ListModels(ctx, "http://127.0.0.1:8600")
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models.Models) != 2 || models.Models[1].ID != "gw-model-large" {
		t.Fatalf("模型列表不符: %+v", models)
	}

	// config.set：只改 base_url/model，不清空其它字段。
	next := cur
	next.Providers = nil
	next.BaseURL = "http://127.0.0.1:8600"
	next.Model = "gw-model-large"
	next.SecretRef = ref
	if err := c.ApplySettings(ctx, next); err != nil {
		t.Fatalf("ApplySettings: %v", err)
	}

	got := agent.currentSettings()
	if got.BaseURL != "http://127.0.0.1:8600" || got.Model != "gw-model-large" || got.SecretRef != ref {
		t.Fatalf("配置未写回: %+v", got)
	}
	if got.WorkspaceRoot != initial.WorkspaceRoot || got.ContextWindow != initial.ContextWindow ||
		got.MaxOutputTokens != initial.MaxOutputTokens || got.AutoMode != initial.AutoMode {
		t.Fatalf("无关字段被清空: %+v", got)
	}
	if len(got.Providers) != 1 {
		t.Fatalf("候选池被清空（providers 应保持 nil=沿用）: %+v", got.Providers)
	}

	if _, _, puts := agent.counts(); puts != 1 {
		t.Fatalf("secret.put 调用次数 = %d, want 1", puts)
	}
}

// TestClientUnknownFrameIsError 未注册帧回的是裸文本 system.error，必须变成错误。
func TestClientUnknownFrameIsError(t *testing.T) {
	agent := newFakeAgent(t, RuntimeSettings{ProviderID: "custom"})
	ctx := context.Background()
	c := NewClient(agent.endpoint, 2*time.Second)
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer c.Close()

	var out struct{}
	err := c.call(ctx, "system.not.a.real.type", map[string]any{}, &out)
	if err == nil || !strings.Contains(err.Error(), "unknown message type") {
		t.Fatalf("err = %v, want unknown message type", err)
	}
}

// TestClientConnectToNowhere 连不上时报错必须带「端点 + 怎么改」的可操作信息。
func TestClientConnectToNowhere(t *testing.T) {
	c := NewClient("tcp://127.0.0.1:1", 500*time.Millisecond)
	err := c.Connect(context.Background())
	if err == nil {
		t.Fatal("连不上的端点却连接成功")
	}
	if !strings.Contains(err.Error(), "XIMO_IPC_ENDPOINT") {
		t.Fatalf("错误里没有给出排查方式: %v", err)
	}
}

// TestClientPutSecretRedactsError 反向控制：服务端把明文塞进错误消息时，
// 到调用方手上的错误也必须已脱敏。
func TestClientPutSecretRedactsError(t *testing.T) {
	agent := newFakeAgent(t, RuntimeSettings{ProviderID: "custom"})
	agent.secretAvail = false
	agent.leakKeyInError = true
	ctx := context.Background()
	c := NewClient(agent.endpoint, 2*time.Second)
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer c.Close()

	key := "sk-leak-check-1234567890"
	_, err := c.PutSecret(ctx, key)
	if err == nil {
		t.Fatal("安全存储不可用却返回成功")
	}
	if strings.Contains(err.Error(), key) {
		t.Fatalf("错误消息泄露了明文: %v", err)
	}
	if !strings.Contains(err.Error(), "***") {
		t.Fatalf("错误消息没有被脱敏: %v", err)
	}
}
