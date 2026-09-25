package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// legacyConfigJSON 是升级前老版本用户 config.json 的典型形态：只有单个
// provider 对象，没有 providers / sub_agent 键。加载它必须「无痛 + 行为不变」
// —— 这是任务 5 的验收红线，不是可选项。
const legacyConfigJSON = `{
  "supervisor": { "max_restart_retries": 7 },
  "provider": {
    "id": "custom",
    "name": "我的服务商",
    "base_url": "https://api.example.com/v1",
    "model": "my-model",
    "secret_ref": "dpapi:ximo",
    "context_window": 65536,
    "max_output_tokens": 4096,
    "rate_limit_per_sec": 2.5
  },
  "runtime": { "auto_mode": "coding" }
}`

// TestLoadLegacySingleProviderConfig 锁死验收标准 1：老配置单 provider 无痛加载。
func TestLoadLegacySingleProviderConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(legacyConfigJSON), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("老配置加载失败: %v", err)
	}

	// 主服务商字段逐项一致 —— 「行为不变」的落点。
	if cfg.Provider.ID != "custom" || cfg.Provider.BaseURL != "https://api.example.com/v1" ||
		cfg.Provider.Model != "my-model" || cfg.Provider.SecretRef != "dpapi:ximo" ||
		cfg.Provider.ContextWindow != 65536 || cfg.Provider.MaxOutputTokens != 4096 ||
		cfg.Provider.RateLimitPerSec != 2.5 {
		t.Fatalf("主服务商字段丢失或被改写: %+v", cfg.Provider)
	}
	// 其它段不受影响。
	if cfg.Supervisor.MaxRestartRetries != 7 || cfg.Runtime.AutoMode != "coding" {
		t.Fatalf("无关配置段被改写: retries=%d mode=%s", cfg.Supervisor.MaxRestartRetries, cfg.Runtime.AutoMode)
	}

	// 迁移语义：单 provider 自动包装成候选池的第一项。
	pool := cfg.ProviderPool()
	if len(pool) != 1 {
		t.Fatalf("候选池应有 1 项, got %d", len(pool))
	}
	if !reflect.DeepEqual(pool[0], cfg.Provider) {
		t.Fatalf("候选池首项应与主服务商一致: %+v vs %+v", pool[0], cfg.Provider)
	}
}

// TestLegacyConfigRoundTrip 确认老配置「加载→保存→加载」后行为不变，
// 且未被使用新功能时文件里不凭空长出 providers 键（减少对老用户的惊扰）。
func TestLegacyConfigRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(legacyConfigJSON), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	saved := filepath.Join(t.TempDir(), "out.json")
	if err := cfg.SaveConfig(saved); err != nil {
		t.Fatalf("保存失败: %v", err)
	}

	raw, err := os.ReadFile(saved)
	if err != nil {
		t.Fatal(err)
	}
	var shaped map[string]json.RawMessage
	if err := json.Unmarshal(raw, &shaped); err != nil {
		t.Fatal(err)
	}
	if _, ok := shaped["providers"]; ok {
		t.Fatalf("未使用候选池时不应写出 providers 键")
	}
	if _, ok := shaped["provider"]; !ok {
		t.Fatalf("provider 键丢失")
	}

	reloaded, err := LoadConfig(saved)
	if err != nil {
		t.Fatalf("回读失败: %v", err)
	}
	if reloaded.Provider.BaseURL != cfg.Provider.BaseURL || reloaded.Provider.Model != cfg.Provider.Model ||
		reloaded.Provider.SecretRef != cfg.Provider.SecretRef {
		t.Fatalf("回读后主服务商不一致: %+v vs %+v", reloaded.Provider, cfg.Provider)
	}
	if len(reloaded.ProviderPool()) != 1 {
		t.Fatalf("回读后候选池应有 1 项, got %d", len(reloaded.ProviderPool()))
	}
}

// TestUnmarshalProvidersList 新写法：providers 列表，首项即主服务商。
func TestUnmarshalProvidersList(t *testing.T) {
	cfg, err := LoadConfig(writeTemp(t, `{
	  "providers": [
	    {"id":"main","base_url":"https://a/v1","model":"m1","secret_ref":"ref-a"},
	    {"id":"backup","base_url":"https://b/v1","model":"m2","secret_ref":"ref-b"}
	  ]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider.ID != "main" {
		t.Fatalf("主服务商应取池首项, got %q", cfg.Provider.ID)
	}
	pool := cfg.ProviderPool()
	if len(pool) != 2 || pool[0].ID != "main" || pool[1].ID != "backup" {
		t.Fatalf("候选池 = %+v", pool)
	}
}

// TestUnmarshalProviderArray 老键名写列表（用户照文档手改 provider 为数组）。
func TestUnmarshalProviderArray(t *testing.T) {
	cfg, err := LoadConfig(writeTemp(t, `{
	  "provider": [
	    {"id":"main","base_url":"https://a/v1","model":"m1"},
	    {"id":"backup","base_url":"https://b/v1","model":"m2"}
	  ]
	}`))
	if err != nil {
		t.Fatalf("provider 数组形态应能加载: %v", err)
	}
	if cfg.Provider.ID != "main" {
		t.Fatalf("主服务商应取首项, got %q", cfg.Provider.ID)
	}
	if pool := cfg.ProviderPool(); len(pool) != 2 || pool[1].ID != "backup" {
		t.Fatalf("候选池 = %+v", pool)
	}
}

// TestUnmarshalSinglePlusList 主服务商与候选池同时给出：主服务商优先级最高，
// 且绝不能被池里的同名项或默认值顶掉。
func TestUnmarshalSinglePlusList(t *testing.T) {
	cfg, err := LoadConfig(writeTemp(t, `{
	  "provider": {"id":"main","base_url":"https://main/v1","model":"m-main"},
	  "providers": [
	    {"id":"second","base_url":"https://b/v1","model":"m-b"},
	    {"id":"third","base_url":"https://c/v1","model":"m-c"}
	  ]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider.ID != "main" || cfg.Provider.BaseURL != "https://main/v1" {
		t.Fatalf("主服务商被改写: %+v", cfg.Provider)
	}
	pool := cfg.ProviderPool()
	if len(pool) != 3 || pool[0].ID != "main" || pool[1].ID != "second" || pool[2].ID != "third" {
		t.Fatalf("候选池 = %+v", pool)
	}
}

// TestProviderPoolDedupsPrimary 列表里出现与主服务商同 ID 的项时去重。
func TestProviderPoolDedupsPrimary(t *testing.T) {
	cfg, err := LoadConfig(writeTemp(t, `{
	  "provider": {"id":"main","base_url":"https://main/v1","model":"m"},
	  "providers": [
	    {"id":"main","base_url":"https://stale/v1","model":"stale"},
	    {"id":"second","base_url":"https://b/v1"}
	  ]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	pool := cfg.ProviderPool()
	if len(pool) != 2 {
		t.Fatalf("同 ID 候选应去重, got %+v", pool)
	}
	if pool[0].BaseURL != "https://main/v1" {
		t.Fatalf("主服务商应以主字段为准: %+v", pool[0])
	}
}

// TestSubAgentCandidates 锁死分配的优先级：专家 > 分类 > 全局池 > 整个服务商池。
func TestSubAgentCandidates(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.SubAgent.Pool = []string{"p1", "p2"}
	cfg.SubAgent.ByDivision = map[string][]string{"工程": {"p2", "p1"}}
	cfg.SubAgent.ByExpert = map[string][]string{"expert-1": {"p3"}}

	if got := cfg.SubAgentCandidates("expert-1", "工程"); !reflect.DeepEqual(got, []string{"p3"}) {
		t.Fatalf("专家级覆盖应最高优先, got %v", got)
	}
	if got := cfg.SubAgentCandidates("expert-2", "工程"); !reflect.DeepEqual(got, []string{"p2", "p1"}) {
		t.Fatalf("分类覆盖次之, got %v", got)
	}
	if got := cfg.SubAgentCandidates("expert-2", "设计"); !reflect.DeepEqual(got, []string{"p1", "p2"}) {
		t.Fatalf("无分类覆盖时用全局池, got %v", got)
	}

	cfg.SubAgent.Pool = nil
	if got := cfg.SubAgentCandidates("expert-2", "设计"); got != nil {
		t.Fatalf("全未配置时返回 nil（用池默认顺序）, got %v", got)
	}
}

// TestSaveConfigPersistsSubAgent 验收标准 4 的后端半边：面板写入的分配要落盘。
func TestSaveConfigPersistsSubAgent(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.Provider.ID = "main"
	cfg.Provider.BaseURL = "https://main/v1"
	cfg.Providers = []ProviderConfig{{ID: "second", BaseURL: "https://b/v1", Model: "m2"}}
	cfg.SubAgent = SubAgentConfig{
		Pool:       []string{"main", "second"},
		ByDivision: map[string][]string{"工程": {"second"}},
	}

	saved := filepath.Join(t.TempDir(), "out.json")
	if err := cfg.SaveConfig(saved); err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadConfig(saved)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Providers) != 1 || reloaded.Providers[0].ID != "second" {
		t.Fatalf("候选未持久化: %+v", reloaded.Providers)
	}
	if reloaded.SubAgent.Pool[0] != "main" || reloaded.SubAgent.ByDivision["工程"][0] != "second" {
		t.Fatalf("分配未持久化: %+v", reloaded.SubAgent)
	}
	if got := reloaded.SubAgentCandidates("any", "工程"); len(got) != 1 || got[0] != "second" {
		t.Fatalf("回读后的分配解析不正确: %v", got)
	}
}

// TestUnmarshalInvalidProviderStillErrors 结构坏了必须报错而不是静默吞掉，
// 否则引擎会带着半个配置启动。
func TestUnmarshalInvalidProviderStillErrors(t *testing.T) {
	if _, err := LoadConfig(writeTemp(t, `{"provider": {"base_url": 42}}`)); err == nil {
		t.Fatalf("provider 字段类型错误应报错")
	}
	if _, err := LoadConfig(writeTemp(t, `{"providers": ["not-an-object"]}`)); err == nil {
		t.Fatalf("providers 元素类型错误应报错")
	}
}

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}
