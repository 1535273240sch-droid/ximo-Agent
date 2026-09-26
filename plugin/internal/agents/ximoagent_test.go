package agents

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestApplyXimoAgentViaIPC 是主路径：IPC 生效、不留明文、备份到位、
// 用户已配好的无关字段不被清空。
func TestApplyXimoAgentViaIPC(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	originalConfig := `{
  "supervisor": { "max_restart_retries": 7 },
  "provider": {
    "id": "custom",
    "name": "旧名字",
    "base_url": "https://api.openai.com/v1",
    "model": "gpt-4o",
    "secret_ref": "",
    "workspace_root": "/work/keep-me"
  },
  "unknown_future_section": { "keep": true }
}`
	if err := os.WriteFile(configPath, []byte(originalConfig), 0o644); err != nil {
		t.Fatal(err)
	}

	agent := newFakeAgent(t, RuntimeSettings{
		ProviderID:      "custom",
		ProviderName:    "第三方接口",
		BaseURL:         "https://api.openai.com/v1",
		Model:           "gpt-4o",
		WorkspaceRoot:   "/work/keep-me",
		ContextWindow:   131072,
		MaxOutputTokens: 8192,
		AutoMode:        "coding",
		ConfigPath:      configPath,
		Providers:       []json.RawMessage{json.RawMessage(`{"id":"pool-a"}`)},
	})

	key := "sk-ipc-path-abcdef123456"
	var out bytes.Buffer
	err := ApplyXimoAgent(Config{
		GatewayURL: "http://127.0.0.1:8600/",
		Model:      "gw-model-large",
		APIKey:     key,
		Endpoint:   agent.endpoint,
		Output:     &out,
		Timeout:    3 * time.Second,
	})
	if err != nil {
		t.Fatalf("ApplyXimoAgent: %v\n输出:\n%s", err, out.String())
	}

	got := agent.currentSettings()
	if got.BaseURL != "http://127.0.0.1:8600" {
		t.Errorf("base_url = %q, want http://127.0.0.1:8600（尾斜杠应被去掉）", got.BaseURL)
	}
	if got.Model != "gw-model-large" {
		t.Errorf("model = %q", got.Model)
	}
	if want := SecretRefForValue(key); got.SecretRef != want {
		t.Errorf("secret_ref = %q, want %q", got.SecretRef, want)
	}
	// 无条件覆盖的字段必须被回填，否则用户的配置会被抹掉。
	if got.ProviderID != "custom" || got.WorkspaceRoot != "/work/keep-me" ||
		got.ContextWindow != 131072 || got.MaxOutputTokens != 8192 || got.AutoMode != "coding" {
		t.Errorf("无关字段被清空: %+v", got)
	}
	if len(got.Providers) != 1 {
		t.Errorf("候选池被清空: %+v", got.Providers)
	}

	// 明文红线：终端输出与 config.set 载荷都不许出现密钥明文。
	text := out.String()
	if strings.Contains(text, key) {
		t.Errorf("输出里出现密钥明文:\n%s", text)
	}
	agent.mu.Lock()
	setPayload := mustJSON(t, agent.setPayloads)
	putValues := append([]string{}, agent.putValues...)
	agent.mu.Unlock()
	if strings.Contains(setPayload, key) {
		t.Errorf("config.set 载荷里出现密钥明文: %s", setPayload)
	}
	if len(putValues) != 1 || putValues[0] != key {
		t.Fatalf("secret.put 只应收到一次明文: %v", putValues)
	}

	// 备份：IPC 路径也会落盘，所以改之前必须有备份，且内容与改前一致。
	backups, _ := filepath.Glob(configPath + ".bak-*")
	if len(backups) != 1 {
		t.Fatalf("备份数量 = %d, want 1（%v）", len(backups), backups)
	}
	back, err := os.ReadFile(backups[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(back) != originalConfig {
		t.Errorf("备份内容与改前不一致:\n%s", string(back))
	}

	if !strings.Contains(text, "IPC 端点") || !strings.Contains(text, "密钥已存入平台安全存储，引用：") {
		t.Errorf("输出缺少关键说明:\n%s", text)
	}
}

// TestApplyXimoAgentDryRun 锁死 --dry-run：不写安全存储、不发 config.set、
// 不生成备份、不动文件。
func TestApplyXimoAgentDryRun(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	original := `{"provider":{"base_url":"https://api.openai.com/v1","model":"gpt-4o"},"keep":1}`
	if err := os.WriteFile(configPath, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	agent := newFakeAgent(t, RuntimeSettings{
		ProviderID: "custom", BaseURL: "https://api.openai.com/v1", Model: "gpt-4o", ConfigPath: configPath,
	})

	var out bytes.Buffer
	err := ApplyXimoAgent(Config{
		GatewayURL: "http://127.0.0.1:8600",
		Model:      "gw-model-small",
		APIKey:     "sk-dry-run-1234567890",
		Endpoint:   agent.endpoint,
		DryRun:     true,
		Output:     &out,
		Timeout:    3 * time.Second,
	})
	if err != nil {
		t.Fatalf("ApplyXimoAgent(dry-run): %v\n%s", err, out.String())
	}

	gets, sets, puts := agent.counts()
	if sets != 0 || puts != 0 {
		t.Fatalf("dry-run 却产生了副作用: sets=%d puts=%d", sets, puts)
	}
	if gets == 0 {
		t.Fatal("dry-run 也应读现状以生成差异")
	}
	if agent.currentSettings().BaseURL != "https://api.openai.com/v1" {
		t.Fatal("dry-run 改动了配置")
	}
	if backups, _ := filepath.Glob(configPath + ".bak-*"); len(backups) != 0 {
		t.Fatalf("dry-run 生成了备份: %v", backups)
	}
	if raw, _ := os.ReadFile(configPath); string(raw) != original {
		t.Fatal("dry-run 改动了文件")
	}
	text := out.String()
	if !strings.Contains(text, "dry-run，未改动任何东西") || !strings.Contains(text, "base_url") {
		t.Fatalf("dry-run 输出缺少差异:\n%s", text)
	}
	if strings.Contains(text, "sk-dry-run-1234567890") {
		t.Fatalf("dry-run 输出泄露了密钥:\n%s", text)
	}
}

// TestApplyXimoAgentFallsBackToFileWhenAgentDown 覆盖已知陷阱：agent 没在跑时
// 回退改文件，回退必须告警且保留未知字段、不落明文密钥。
func TestApplyXimoAgentFallsBackToFileWhenAgentDown(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	original := `{
  "provider": { "id": "custom", "base_url": "https://api.openai.com/v1", "model": "gpt-4o" },
  "unknown_future_section": { "keep": true },
  "mcp_servers": [ { "name": "keep-me-too" } ]
}`
	if err := os.WriteFile(configPath, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	key := "sk-fallback-abcdef123456"
	var out bytes.Buffer
	err := ApplyXimoAgent(Config{
		GatewayURL: "http://127.0.0.1:8600",
		Model:      "gw-model-large",
		APIKey:     key,
		// 端口 1 上没有人监听：连接必然失败，触发回退。
		Endpoint:   "tcp://127.0.0.1:1",
		ConfigPath: configPath,
		Output:     &out,
		Timeout:    300 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("回退路径应当成功: %v\n%s", err, out.String())
	}

	doc := map[string]any{}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("回退写出的不是合法 JSON: %v\n%s", err, string(raw))
	}

	provider, _ := doc["provider"].(map[string]any)
	if provider == nil {
		t.Fatal("provider 段丢失")
	}
	if provider["base_url"] != "http://127.0.0.1:8600" || provider["model"] != "gw-model-large" {
		t.Errorf("provider 未更新: %+v", provider)
	}
	if provider["id"] != "custom" {
		t.Errorf("原 provider.id 被改动: %+v", provider["id"])
	}
	if want := SecretRefForValue(key); provider["secret_ref"] != want {
		t.Errorf("secret_ref = %v, want %s", provider["secret_ref"], want)
	}
	if _, ok := doc["unknown_future_section"]; !ok {
		t.Error("未知字段 unknown_future_section 被丢弃")
	}
	if _, ok := doc["mcp_servers"]; !ok {
		t.Error("mcp_servers 被丢弃")
	}
	// 明文密钥绝不许进文件。
	if strings.Contains(string(raw), key) {
		t.Fatalf("配置文件里出现明文密钥:\n%s", string(raw))
	}

	text := out.String()
	for _, want := range []string{"[警告]", "回退", "重启", "明文没有"} {
		if !strings.Contains(text, want) {
			t.Errorf("回退告警缺少 %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, key) {
		t.Errorf("回退输出泄露密钥:\n%s", text)
	}
	if backups, _ := filepath.Glob(configPath + ".bak-*"); len(backups) != 1 {
		t.Errorf("回退路径没有备份: %v", backups)
	}
}

// TestApplyXimoAgentFileFallbackDryRun 回退路径的 dry-run 不落盘。
func TestApplyXimoAgentFileFallbackDryRun(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	original := `{"provider":{"base_url":"https://api.openai.com/v1"},"keep":1}`
	if err := os.WriteFile(configPath, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	err := ApplyXimoAgent(Config{
		GatewayURL: "http://127.0.0.1:8600",
		Model:      "gw-model",
		NoIPC:      true,
		ConfigPath: configPath,
		DryRun:     true,
		Output:     &out,
	})
	if err != nil {
		t.Fatalf("ApplyXimoAgent: %v\n%s", err, out.String())
	}
	if raw, _ := os.ReadFile(configPath); string(raw) != original {
		t.Fatalf("dry-run 改动了文件:\n%s", string(raw))
	}
	if backups, _ := filepath.Glob(configPath + ".bak-*"); len(backups) != 0 {
		t.Fatalf("dry-run 生成了备份: %v", backups)
	}
	text := out.String()
	if !strings.Contains(text, "-") || !strings.Contains(text, "+") {
		t.Fatalf("dry-run 输出不是差异格式:\n%s", text)
	}
}

// TestApplyXimoAgentDoesNotFallbackOnAgentError 反向控制：agent 明确回错时
// 不许回退改文件——那会把「安全存储不可用」这类真问题掩盖成一次「成功」。
func TestApplyXimoAgentDoesNotFallbackOnAgentError(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	original := `{"provider":{"base_url":"https://api.openai.com/v1"},"keep":1}`
	if err := os.WriteFile(configPath, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	agent := newFakeAgent(t, RuntimeSettings{ProviderID: "custom", BaseURL: "https://api.openai.com/v1"})
	agent.failConfigGet = true

	var out bytes.Buffer
	err := ApplyXimoAgent(Config{
		GatewayURL: "http://127.0.0.1:8600",
		Model:      "gw-model",
		Endpoint:   agent.endpoint,
		ConfigPath: configPath,
		Output:     &out,
		Timeout:    2 * time.Second,
	})
	if err == nil {
		t.Fatalf("agent 报错却成功返回:\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "settings are not available") {
		t.Fatalf("错误没有透出 agent 的原因: %v", err)
	}
	if raw, _ := os.ReadFile(configPath); string(raw) != original {
		t.Fatal("agent 报错后仍然改了配置文件")
	}
	if strings.Contains(out.String(), "[警告] 回退") {
		t.Fatalf("agent 报错时不应回退:\n%s", out.String())
	}
}

// TestApplyXimoAgentNoFileFallback NoFileFallback 时连不上就直接失败。
func TestApplyXimoAgentNoFileFallback(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	original := `{"provider":{"base_url":"https://api.openai.com/v1"},"keep":1}`
	if err := os.WriteFile(configPath, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	err := ApplyXimoAgent(Config{
		GatewayURL:     "http://127.0.0.1:8600",
		Endpoint:       "tcp://127.0.0.1:1",
		ConfigPath:     configPath,
		NoFileFallback: true,
		Output:         &out,
		Timeout:        300 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("禁止回退时连不上却成功了")
	}
	if raw, _ := os.ReadFile(configPath); string(raw) != original {
		t.Fatal("禁止回退时仍改了文件")
	}
}

// TestApplyXimoAgentValidation 早期失败：坏网关地址一个字节都不该落盘。
func TestApplyXimoAgentValidation(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")

	cases := []struct{ name, gateway string }{
		{"empty", ""},
		{"bad-scheme", "ftp://gateway.example.com"},
		{"no-host", "http:///v1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			err := ApplyXimoAgent(Config{
				GatewayURL: c.gateway, NoIPC: true, ConfigPath: configPath, Output: &out,
			})
			if err == nil {
				t.Fatalf("坏地址 %q 却成功", c.gateway)
			}
			if _, statErr := os.Stat(configPath); statErr == nil {
				t.Fatal("校验失败却创建了配置文件")
			}
		})
	}

	// 缺协议头时按 agent 的规则补 https://。
	normalized, err := normalizeBaseURL("gateway.example.com/v1/")
	if err != nil || normalized != "https://gateway.example.com/v1" {
		t.Fatalf("normalizeBaseURL = %q, %v", normalized, err)
	}
}

// TestDiscoverEndpointOrder 端点发现顺序：flag → env → 平台默认。
func TestDiscoverEndpointOrder(t *testing.T) {
	t.Setenv(EnvEndpoint, "tcp://127.0.0.1:9999")

	ep, src := DiscoverEndpoint("tcp://127.0.0.1:1234")
	if ep != "tcp://127.0.0.1:1234" || src != EndpointFromFlag {
		t.Fatalf("flag 未优先: %q %q", ep, src)
	}

	ep, src = DiscoverEndpoint("")
	if ep != "tcp://127.0.0.1:9999" || src != EndpointFromEnv {
		t.Fatalf("env 未生效: %q %q", ep, src)
	}

	t.Setenv(EnvEndpoint, "")
	ep, src = DiscoverEndpoint("")
	if src != EndpointFromPlatform {
		t.Fatalf("source = %q, want %q", src, EndpointFromPlatform)
	}
	if ep != DefaultEndpoint() {
		t.Fatalf("endpoint = %q, want %q", ep, DefaultEndpoint())
	}
	if runtime.GOOS == "windows" {
		if ep != `\\.\pipe\ximo-agent-ipc` {
			t.Fatalf("windows 默认端点 = %q", ep)
		}
	} else if ep != "/tmp/ximo-agent-ipc.sock" {
		t.Fatalf("unix 默认端点 = %q", ep)
	}
}

// TestDefaultConfigPath 路径规则必须与主仓库 config.DefaultPaths 一致。
func TestDefaultConfigPath(t *testing.T) {
	t.Setenv("XIMO_HOME", "")

	got := DefaultConfigPath(`C:\Users\someone`)
	switch runtime.GOOS {
	case "windows":
		want := filepath.Join(`C:\Users\someone`, "AppData", "Roaming", "ximo-agent", "config.json")
		if got != want {
			t.Fatalf("DefaultConfigPath = %q, want %q", got, want)
		}
	default:
		if !strings.HasSuffix(got, filepath.Join("ximo-agent", "config.json")) {
			t.Fatalf("DefaultConfigPath = %q", got)
		}
	}

	// XIMO_HOME 优先于 --home。
	t.Setenv("XIMO_HOME", filepath.Join(t.TempDir(), "ximo-home"))
	if got := DefaultConfigPath("ignored"); got != filepath.Join(os.Getenv("XIMO_HOME"), "config.json") {
		t.Fatalf("XIMO_HOME 未优先: %q", got)
	}
}

// TestSecretRefForValue 派生规则必须与主仓库 internal/secrets.RefForValue 一致：
// "secretref:v1:" + sha256(明文) 前 16 字节的十六进制。测试用标准库自己算一遍
// 期望值（独立于被测实现），而不是抄实现的输出。
func TestSecretRefForValue(t *testing.T) {
	sum := sha256.Sum256([]byte("sk-test"))
	want := "secretref:v1:" + hex.EncodeToString(sum[:16])

	got := SecretRefForValue("sk-test")
	if got != want {
		t.Fatalf("SecretRefForValue = %q, want %q", got, want)
	}
	if len(got) != len("secretref:v1:")+32 {
		t.Fatalf("ref 形态不对: %q", got)
	}
	if SecretRefForValue("sk-test") != got {
		t.Fatal("派生不确定（同一值必须得到同一 ref）")
	}
	if SecretRefForValue("sk-test2") == got {
		t.Fatal("不同值得到同一 ref")
	}
	if strings.Contains(got, "sk-test") {
		t.Fatalf("ref 里出现了明文: %q", got)
	}
}

// TestPatchForEchoDropsEchoUnsafeFields 回传载荷必须把 providers / mcp_servers
// 置 nil（未携带 = 沿用），否则 agent 会按「整体替换」把 UI 快照当真相。
func TestPatchForEchoDropsEchoUnsafeFields(t *testing.T) {
	cur := RuntimeSettings{
		ProviderID:      "custom",
		BaseURL:         "https://api.openai.com/v1",
		Model:           "gpt-4o",
		WorkspaceRoot:   "/work/keep-me",
		ContextWindow:   131072,
		MaxOutputTokens: 8192,
		AutoMode:        "coding",
		Providers:       []json.RawMessage{json.RawMessage(`{"id":"pool-a"}`)},
		MCPServers:      []json.RawMessage{json.RawMessage(`{"name":"mcp-a"}`)},
		SubAgent:        SubAgentSettingsPayload{Pool: []string{"pool-a"}},
	}
	next := PatchForEcho(cur)

	if next.Providers != nil || next.MCPServers != nil {
		t.Fatalf("providers/mcp_servers 应置 nil: %+v", next)
	}
	if next.SubAgent.Pool != nil {
		t.Fatalf("sub_agent 应留零值: %+v", next.SubAgent)
	}
	// 无条件覆盖的字段必须被原样带上。
	if next.ProviderID != cur.ProviderID || next.BaseURL != cur.BaseURL || next.Model != cur.Model ||
		next.WorkspaceRoot != cur.WorkspaceRoot || next.ContextWindow != cur.ContextWindow ||
		next.MaxOutputTokens != cur.MaxOutputTokens || next.AutoMode != cur.AutoMode {
		t.Fatalf("回填字段不完整: %+v", next)
	}
	// 序列化后不应出现 providers / mcp_servers / sub_agent 的非空内容。
	raw := mustJSON(t, next)
	if strings.Contains(raw, "pool-a") || strings.Contains(raw, "mcp-a") {
		t.Fatalf("载荷里仍带着 UI 快照: %s", raw)
	}
	if !strings.Contains(raw, `"sub_agent":{}`) {
		t.Fatalf("sub_agent 应为空对象: %s", raw)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
