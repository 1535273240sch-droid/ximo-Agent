package gateway

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestPickModelBranches 钉住选模型的每一条分支（契约 §3）。
func TestPickModelBranches(t *testing.T) {
	chat := Model{ID: "gpt-chat", DisplayName: "Chat", Enabled: true, Protocols: []string{ProtocolOpenAIChat}}
	anthropicOnly := Model{ID: "claude-x", Enabled: true, Protocols: []string{ProtocolAnthropicMessages}}
	noProtocols := Model{ID: "mystery", Enabled: true, Protocols: []string{}}
	disabledChat := Model{ID: "retired", Enabled: false, Protocols: []string{ProtocolOpenAIChat}}
	disabledOther := Model{ID: "retired2", Enabled: false}

	c := &Client{}

	t.Run("空 want 优先 openai-chat", func(t *testing.T) {
		got, err := c.PickModel([]Model{noProtocols, anthropicOnly, chat}, "")
		mustNoErr(t, err, "PickModel")
		if got.ID != chat.ID {
			t.Errorf("选中 %q, 期望 %q（优先 openai-chat 协议）", got.ID, chat.ID)
		}
	})

	t.Run("空 want 退化到第一个 enabled", func(t *testing.T) {
		got, err := c.PickModel([]Model{noProtocols, anthropicOnly}, "")
		mustNoErr(t, err, "PickModel")
		if got.ID != noProtocols.ID {
			t.Errorf("选中 %q, 期望第一个 enabled %q", got.ID, noProtocols.ID)
		}
	})

	t.Run("空 want 跳过停用模型", func(t *testing.T) {
		got, err := c.PickModel([]Model{disabledChat, anthropicOnly}, "")
		mustNoErr(t, err, "PickModel")
		if got.ID != anthropicOnly.ID {
			t.Errorf("选中 %q, 期望 %q", got.ID, anthropicOnly.ID)
		}
	})

	t.Run("空 want 无可用模型", func(t *testing.T) {
		for _, list := range [][]Model{nil, {}, {disabledChat, disabledOther}} {
			_, err := c.PickModel(list, "")
			if !errors.Is(err, ErrNoModels) {
				t.Errorf("list=%v 错误 = %v, 期望 ErrNoModels", list, err)
			}
		}
	})

	t.Run("显式 want 命中", func(t *testing.T) {
		got, err := c.PickModel([]Model{chat, anthropicOnly}, "  claude-x  ")
		mustNoErr(t, err, "PickModel")
		if got.ID != anthropicOnly.ID {
			t.Errorf("选中 %q", got.ID)
		}
	})

	t.Run("显式 want 大小写不敏感兜底", func(t *testing.T) {
		got, err := c.PickModel([]Model{chat, anthropicOnly}, "CLAUDE-X")
		mustNoErr(t, err, "PickModel")
		if got.ID != anthropicOnly.ID {
			t.Errorf("选中 %q", got.ID)
		}
	})

	t.Run("显式 want 不存在时给出可选列表", func(t *testing.T) {
		_, err := c.PickModel([]Model{chat, anthropicOnly}, "gpt-9")
		if !errors.Is(err, ErrModelUnavailable) {
			t.Fatalf("错误 = %v, 期望 ErrModelUnavailable", err)
		}
		msg := err.Error()
		for _, want := range []string{"gpt-9", "gpt-chat", "claude-x"} {
			if !strings.Contains(msg, want) {
				t.Errorf("错误信息应包含 %q 便于用户修正 --model: %s", want, msg)
			}
		}
	})

	t.Run("显式 want 命中但已停用", func(t *testing.T) {
		_, err := c.PickModel([]Model{disabledChat, chat}, "retired")
		if !errors.Is(err, ErrModelUnavailable) {
			t.Fatalf("错误 = %v, 期望 ErrModelUnavailable", err)
		}
		if !strings.Contains(err.Error(), "停用") || !strings.Contains(err.Error(), "gpt-chat") {
			t.Errorf("错误信息应说明已停用并列出可选模型: %s", err.Error())
		}
	})

	t.Run("模型很多时列表被截断", func(t *testing.T) {
		list := make([]Model, 0, 60)
		for i := 0; i < 60; i++ {
			list = append(list, Model{ID: "m" + strings.Repeat("x", i%3) + string(rune('a'+i%26)), Enabled: true})
		}
		_, err := c.PickModel(list, "nope")
		if err == nil {
			t.Fatal("应报错")
		}
		if len(err.Error()) > 2000 {
			t.Errorf("错误信息过长（%d 字节），应被截断", len(err.Error()))
		}
	})
}

// TestListModels 覆盖真实响应形状、未登录短路、凭据被拒时的脱敏。
func TestListModels(t *testing.T) {
	f := newFakeGateway(t)
	f.models = []Model{
		{ID: "gpt-4o-mini", DisplayName: "GPT-4o mini", Provider: "prov-main",
			Protocols:    []string{ProtocolOpenAIChat},
			Capabilities: Capabilities{Stream: true, Vision: true, Tools: true}, Enabled: true},
		{ID: "claude-3", DisplayName: "Claude 3", Provider: "",
			Protocols: []string{ProtocolAnthropicMessages, ProtocolOpenAIChat}, Enabled: true},
	}
	access, _ := f.seedTokens()
	c := f.client(testCredPath(t))

	models, err := c.ListModels(context.Background(), Cred{Gateway: f.srv.URL, AccessToken: access})
	mustNoErr(t, err, "ListModels")
	if len(models) != 2 {
		t.Fatalf("模型数 = %d, 期望 2", len(models))
	}
	first := models[0]
	if first.ID != "gpt-4o-mini" || first.DisplayName != "GPT-4o mini" || first.Provider != "prov-main" {
		t.Errorf("首条解析错误: %+v", first)
	}
	if !first.Capabilities.Stream || first.Capabilities.Reasoning {
		t.Errorf("能力解析错误: %+v", first.Capabilities)
	}
	if !first.OffersOpenAIChat() || first.OffersProtocol(ProtocolAnthropicMessages) {
		t.Error("协议判定错误")
	}

	// 空凭据：本地短路，不打网关。
	before := f.requestCount()
	if _, err := c.ListModels(context.Background(), Cred{}); !errors.Is(err, ErrNoCredential) {
		t.Errorf("空凭据错误 = %v, 期望 ErrNoCredential", err)
	}
	if f.requestCount() != before {
		t.Error("空凭据不该发请求")
	}

	// 凭据被拒：服务端回显凭据时错误文本里也只能有掩码。
	f.echoSecretInError = true
	bad := "gwa_BADTOKEN1234567890abcdef"
	_, err = c.ListModels(context.Background(), Cred{Gateway: f.srv.URL, AccessToken: bad})
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("错误 = %v, 期望 ErrUnauthorized", err)
	}
	assertNoSecrets(t, "错误文本", err.Error(), bad)
	if !strings.Contains(err.Error(), Mask(bad)) {
		t.Errorf("错误文本应含掩码后的凭据: %s", err.Error())
	}

	// 空目录（服务端返回 data: []）不能被当成 nil。
	f.models = nil
	empty, err := c.ListModels(context.Background(), Cred{Gateway: f.srv.URL, AccessToken: access})
	mustNoErr(t, err, "ListModels(空目录)")
	if empty == nil || len(empty) != 0 {
		t.Errorf("空目录应返回非 nil 空切片: %#v", empty)
	}
}

// TestListUsage 覆盖用量读取：limit 透传、上限收紧、非负校验、用户身份只来自凭据。
func TestListUsage(t *testing.T) {
	f := newFakeGateway(t)
	f.usage = []UsageRecord{
		{RequestID: "req-a", ModelID: "gpt-4o-mini", ProviderID: "prov-main", Status: "ok",
			InputTokens: 10, OutputTokens: 5, LatencyMS: 120, CostMicro: 3, CreatedAt: 3000},
		{RequestID: "req-b", ModelID: "claude-3", ProviderID: "", Status: "client_canceled",
			InputTokens: 1, OutputTokens: 0, LatencyMS: 9, CostMicro: 0, CreatedAt: 2000},
	}
	access, _ := f.seedTokens()
	cred := Cred{Gateway: f.srv.URL, AccessToken: access}
	c := f.client(testCredPath(t))

	records, err := c.ListUsage(context.Background(), cred, 5)
	mustNoErr(t, err, "ListUsage")
	if len(records) != 2 || records[0].RequestID != "req-a" || records[1].CostMicro != 0 {
		t.Errorf("用量解析错误: %+v", records)
	}
	if records[0].CostMicro != 3 || records[0].CreatedAt != 3000 {
		t.Errorf("字段映射错误: %+v", records[0])
	}
	if f.usageLimit != 5 {
		t.Errorf("limit 透传 = %d, 期望 5", f.usageLimit)
	}

	// limit<=0：不带参数，交给服务端取默认值。
	_, err = c.ListUsage(context.Background(), cred, 0)
	mustNoErr(t, err, "ListUsage(0)")
	if f.usageLimit != 0 {
		t.Errorf("limit=0 时不应带 limit 参数（服务端看到 %d）", f.usageLimit)
	}
	if !strings.Contains(strings.Join(f.requestList(), " "), "GET /v1/usage") {
		t.Errorf("请求记录异常: %v", f.requestList())
	}

	// 负数 limit：本地拒绝。
	if _, err := c.ListUsage(context.Background(), cred, -1); err == nil {
		t.Error("负数 limit 应被本地拒绝")
	}

	// 未登录 / 凭据被拒。
	if _, err := c.ListUsage(context.Background(), Cred{}, 0); !errors.Is(err, ErrNoCredential) {
		t.Errorf("空凭据错误 = %v, 期望 ErrNoCredential", err)
	}
	_, err = c.ListUsage(context.Background(), Cred{Gateway: f.srv.URL, AccessToken: "gwa_NOPE1234567890abcdef"}, 0)
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("错误 = %v, 期望 ErrUnauthorized", err)
	}
}

// TestAPIKeyToleratedAsBearer API Key 与 access token 走同一套 Bearer 鉴权。
func TestAPIKeyToleratedAsBearer(t *testing.T) {
	f := newFakeGateway(t)
	key := "ximo_sk_FAKEKEYFORBEARER01"
	f.addAPIKey(key)
	f.models = []Model{{ID: "m1", Enabled: true, Protocols: []string{ProtocolOpenAIChat}}}
	c := f.client(testCredPath(t))

	models, err := c.ListModels(context.Background(), Cred{Gateway: f.srv.URL, APIKey: key})
	mustNoErr(t, err, "ListModels")
	if len(models) != 1 {
		t.Errorf("模型数 = %d", len(models))
	}
	if !strings.Contains(strings.Join(f.requestList(), " "), "Bearer "+key) {
		t.Errorf("应以 API Key 作为 Bearer 发出: %v", f.requestList())
	}
}

// TestNormalizeGatewayURL 地址规整与拒绝非法输入。
func TestNormalizeGatewayURL(t *testing.T) {
	ok := []struct{ in, want string }{
		{"https://gw.example.com/", "https://gw.example.com"},
		{"  http://127.0.0.1:8080  ", "http://127.0.0.1:8080"},
		{"https://gw.example.com/sub", "https://gw.example.com/sub"},
	}
	for _, c := range ok {
		got, err := NormalizeGatewayURL(c.in)
		mustNoErr(t, err, "NormalizeGatewayURL("+c.in+")")
		if got != c.want {
			t.Errorf("NormalizeGatewayURL(%q) = %q, 期望 %q", c.in, got, c.want)
		}
	}

	bad := map[string]string{
		"空":     "",
		"缺协议":   "gw.example.com",
		"不支持协议": "ftp://gw.example.com",
		"缺主机名":  "https://",
		"带查询串":  "https://gw.example.com/?token=abc",
		"带片段":   "https://gw.example.com/#x",
	}
	for name, in := range bad {
		if got, err := NormalizeGatewayURL(in); err == nil {
			t.Errorf("%s: NormalizeGatewayURL(%q) = %q, 期望报错", name, in, got)
		}
	}

	// 端到端：地址非法时请求失败，而不是发出一个坏 URL。
	c := &Client{BaseURL: "gw.example.com", CredPath: testCredPath(t)}
	if _, err := c.Health(context.Background()); err == nil {
		t.Error("非法网关地址应导致请求失败")
	}
}

// TestClientZeroValueAndTimeout 零值/默认超时：不设置 HTTP 字段也能用，且不会无限挂起。
func TestClientZeroValueAndTimeout(t *testing.T) {
	f := newFakeGateway(t)
	c := &Client{BaseURL: f.srv.URL, CredPath: testCredPath(t)}
	if c.httpClient().Timeout != defaultHTTPTimeout {
		t.Errorf("默认超时 = %s, 期望 %s", c.httpClient().Timeout, defaultHTTPTimeout)
	}
	if _, err := c.Health(context.Background()); err != nil {
		t.Errorf("零值客户端应可直接使用: %v", err)
	}
	// 显式注入的 HTTP 客户端优先。
	custom := &http.Client{Timeout: 3 * time.Second}
	c.HTTP = custom
	if c.httpClient() != custom {
		t.Error("注入的 HTTP 客户端未被采用")
	}
}

// TestEnsureCredRefreshKeepsUsername 续期不应丢掉展示用的用户名。
func TestEnsureCredRefreshKeepsUsername(t *testing.T) {
	f := newFakeGateway(t)
	access, refresh := f.seedTokens()
	credPath := testCredPath(t)
	c := f.client(credPath)
	mustNoErr(t, c.SaveCred(Cred{
		Gateway: f.srv.URL, Username: "tester", AccessToken: access, RefreshToken: refresh,
		AccessExpiresAt: time.Now().Add(-time.Minute).Unix(),
	}), "SaveCred")

	got, err := c.EnsureCred(context.Background())
	mustNoErr(t, err, "EnsureCred")
	if got.Username != "tester" {
		t.Errorf("username = %q, 期望保留 tester", got.Username)
	}
}
