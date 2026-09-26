package config

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

// TestDefaultConfigFileMemoryEndpoint 钉住随包发货的 config.default.json 的 memory 段。
//
// 上游 mem0 的 server/docker-compose.yaml 把 REST API 发布在宿主机 8888（容器内
// 8000），3000 是 dashboard —— 它只代理 /api/health 与 /api/auth/refresh，不代理
// /memories、/search。默认示例写成 3000，用户照抄之后四步验证会全 404，却看不出
// 是端口写错。这个默认值必须跟着上游走，所以在这里钉死。
func TestDefaultConfigFileMemoryEndpoint(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config.default.json"))
	if err != nil {
		t.Fatalf("读取随包发货的默认配置失败: %v", err)
	}
	var cfg struct {
		FeatureFlags map[string]bool `json:"feature_flags"`
		Memory       struct {
			Enabled  bool   `json:"enabled"`
			Endpoint string `json:"endpoint"`
		} `json:"memory"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("config.default.json 不是合法 JSON: %v", err)
	}

	u, err := url.Parse(cfg.Memory.Endpoint)
	if err != nil {
		t.Fatalf("memory.endpoint %q 不是合法 URL: %v", cfg.Memory.Endpoint, err)
	}
	if u.Port() == "3000" {
		t.Errorf("memory.endpoint 指向 3000：那是 mem0 的 dashboard，"+
			"/memories 与 /search 在它上面会 404（当前 %q）", cfg.Memory.Endpoint)
	}
	if u.Port() != "8888" {
		t.Errorf("memory.endpoint 的端口 = %q, want \"8888\"（上游 compose 的 API 宿主端口）",
			u.Port())
	}

	// 默认必须是关的：长期记忆依赖一个外部服务与一份额外密钥，在用户显式打开
	// 之前不该改变任何行为。
	if cfg.Memory.Enabled {
		t.Error("memory.enabled 的默认值必须为 false")
	}
	if enabled, ok := cfg.FeatureFlags[FlagMemory]; !ok || enabled {
		t.Errorf("feature_flags[%q] 的默认值必须为 false（ok=%v enabled=%v）",
			FlagMemory, ok, enabled)
	}
}
