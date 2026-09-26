package agents

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 本文件是与**主仓库真实实现**对拍的互操作用例。两个用例默认都跳过（没有
// 环境变量就 skip），因此 `go test ./...` 永远是绿的；要跑它们需要一个用
// 主仓库 internal/ipc + internal/ipcapi 编出来的对端程序。
//
// 为什么要这一层：protocol_test.go 的黄金字节是我按注释布局手算的，只能证明
// 「我理解的和文档一致」；只有让插件客户端跟真实服务端在真实 socket 上跑一遍，
// 才能证明「我的字节和主仓库的字节一致」。反向同理。

// TestLiveInteropClientAgainstRealServer 插件客户端 →（真实主仓库服务端）。
//
// 需要：
//
//	XIMO_INTEROP_ENDPOINT=tcp://127.0.0.1:8611   真实服务端（go run . -mode server）
//	XIMO_INTEROP_CONFIG=<path>                   服务端自己的 config.json 路径（可选）
func TestLiveInteropClientAgainstRealServer(t *testing.T) {
	endpoint := strings.TrimSpace(os.Getenv("XIMO_INTEROP_ENDPOINT"))
	if endpoint == "" {
		t.Skip("未设置 XIMO_INTEROP_ENDPOINT，跳过：插件客户端 ↔ 主仓库真实 IPC 服务端")
	}
	configPath := strings.TrimSpace(os.Getenv("XIMO_INTEROP_CONFIG"))
	if configPath == "" {
		configPath = filepath.Join(t.TempDir(), "config.json")
	}
	original := `{"provider":{"id":"custom","base_url":"https://api.openai.com/v1","model":"gpt-4o"},` +
		`"unknown_future_section":{"keep":true}}`
	if err := os.WriteFile(configPath, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	key := "sk-interop-abcdef123456"
	var out bytes.Buffer
	start := time.Now()
	err := ApplyXimoAgent(Config{
		GatewayURL: "http://127.0.0.1:8600/",
		Model:      "gw-model-large",
		APIKey:     key,
		Endpoint:   endpoint,
		ConfigPath: configPath,
		Output:     &out,
		Timeout:    5 * time.Second,
	})
	elapsed := time.Since(start)
	text := out.String()
	t.Logf("耗时 %s\n输出:\n%s", elapsed, text)

	if err != nil {
		t.Fatalf("对真实服务端执行失败: %v\n输出:\n%s", err, text)
	}
	if strings.Contains(text, key) {
		t.Fatalf("输出泄露密钥:\n%s", text)
	}
	if !strings.Contains(text, "已通过 IPC 生效") {
		t.Fatalf("没有走到 IPC 生效路径:\n%s", text)
	}
	if !strings.Contains(text, "密钥已存入平台安全存储") {
		t.Fatalf("secret.put 没有成功:\n%s", text)
	}
	// 备份由插件侧完成（IPC 路径也要落盘，所以改前必须留一份）。
	if backups, _ := filepath.Glob(configPath + ".bak-*"); len(backups) != 1 {
		t.Fatalf("备份数量 = %d (%v)", len(backups), backups)
	}
	// 真实服务端回写的 config_path 必须被识别（说明 config.get 的载荷解对了）。
	if !strings.Contains(text, configPath) {
		t.Fatalf("没有识别到服务端返回的 config_path=%s:\n%s", configPath, text)
	}
}

// TestLiveInteropFallbackConfigIsLoadableByAgent 回退路径的端到端证据：
// 让插件走配置文件回退路径写一份配置，路径由 XIMO_INTEROP_FALLBACK_OUT 指定；
// 随后由外部用**主仓库真实的 config.LoadConfig** 读回来核对（见交付报告里的命令）。
func TestLiveInteropFallbackConfigIsLoadableByAgent(t *testing.T) {
	path := strings.TrimSpace(os.Getenv("XIMO_INTEROP_FALLBACK_OUT"))
	if path == "" {
		t.Skip("未设置 XIMO_INTEROP_FALLBACK_OUT，跳过：回退路径写出的配置由真实 LoadConfig 读回")
	}
	var out bytes.Buffer
	err := ApplyXimoAgent(Config{
		GatewayURL: "http://127.0.0.1:8600",
		Model:      "gw-model-large",
		APIKey:     "sk-fallback-interop-123456",
		NoIPC:      true,
		ConfigPath: path,
		Output:     &out,
	})
	t.Logf("输出:\n%s", out.String())
	if err != nil {
		t.Fatalf("回退路径失败: %v", err)
	}
	if !strings.Contains(out.String(), "[警告]") {
		t.Fatalf("回退路径没有告警:\n%s", out.String())
	}

	// 顺带对拍路径规则：XIMO_INTEROP_REAL_DEFAULT_PATH 由主仓库真实的
	// config.DefaultConfigPath() 打印出来，两边必须逐字相同——否则插件会往
	// 一个 agent 根本不读的地方写配置，而用户只会看到「改了没用」。
	if realPath := strings.TrimSpace(os.Getenv("XIMO_INTEROP_REAL_DEFAULT_PATH")); realPath != "" {
		if got := DefaultConfigPath(""); got != realPath {
			t.Fatalf("DefaultConfigPath = %q，主仓库真实值 = %q（不一致会导致写入被静默忽略）", got, realPath)
		}
		t.Logf("DefaultConfigPath 与主仓库一致: %s", realPath)
	}
}

// TestLiveInteropRealClientAgainstMyCodec （真实主仓库客户端）→ 插件自己的编解码。
//
// 需要 XIMO_INTEROP_CLIENT_BIN=<对端程序路径>，该程序以 -mode client 连上
// 本用例起在 fakeAgent 端点，用 ipcapi.Client 走完 ping/get/put/set/models。
func TestLiveInteropRealClientAgainstMyCodec(t *testing.T) {
	bin := strings.TrimSpace(os.Getenv("XIMO_INTEROP_CLIENT_BIN"))
	if bin == "" {
		t.Skip("未设置 XIMO_INTEROP_CLIENT_BIN，跳过：主仓库真实 IPC 客户端 ↔ 插件编解码")
	}

	agent := newFakeAgent(t, RuntimeSettings{
		ProviderID:    "custom",
		ProviderName:  "第三方接口",
		BaseURL:       "https://api.openai.com/v1",
		Model:         "gpt-4o",
		WorkspaceRoot: "/work/keep-me",
		ConfigPath:    "/tmp/ximo-agent/config.json",
	})
	agent.modelLists = []ModelInfo{{ID: "gw-model-small"}, {ID: "gw-model-large"}}
	agent.preludeFrames = 1

	key := "sk-interop-reverse-987654321"
	cmd := exec.Command(bin, "-mode", "client", "-endpoint", agent.endpoint, "-key", key)
	raw, err := cmd.CombinedOutput()
	out := string(raw)
	t.Logf("对端输出:\n%s", out)
	if err != nil {
		t.Fatalf("真实客户端对插件编解码执行失败: %v\n%s", err, out)
	}
	for _, want := range []string{"CLIENT ping=ok", "CLIENT apply=ok", "CLIENT models=2", "CLIENT DONE"} {
		if !strings.Contains(out, want) {
			t.Errorf("对端输出缺少 %q:\n%s", want, out)
		}
	}

	// 真实客户端发来的 config.set 必须被我的解码器正确解出并落到假服务端上。
	got := agent.currentSettings()
	if got.BaseURL != "http://127.0.0.1:8600" || got.Model != "gw-model-large" {
		t.Fatalf("真实客户端写入的配置没被正确解码: %+v", got)
	}
	if want := SecretRefForValue(key); got.SecretRef != want {
		t.Fatalf("secret_ref = %q, want %q（双方的 ref 派生规则应一致）", got.SecretRef, want)
	}
	if got.WorkspaceRoot != "/work/keep-me" {
		t.Fatalf("无条件覆盖字段被清空: %+v", got)
	}
}
