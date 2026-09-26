package agents_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/1535273240sch-droid/ximo-plugin/internal/agents"
	"github.com/1535273240sch-droid/ximo-plugin/internal/formats"
)

// 本文件从**外部测试包**验证两件事：
//
//  1. 契约里 internal/formats 的四个函数可以直接装进 agents.DocIO
//     （编译期断言：签名对不上就编译不过）；
//  2. 用真实 formats 与用 agents 自带 LocalDocIO，回退路径的结果一致
//     （同样保留未知字段、同样备份、provider 段写出的值相同）。
//
// 用外部测试包而不是让 agents 直接 import formats：契约冻结的是签名，
// 不是实现。自带实现让 agents 在 formats 尚未落地/将来改动时都能独立构建，
// 而「复用 formats」由调用方一行装配完成。
var formatsPort agents.DocIO = agents.FuncDocIO{
	ReadFn:    formats.Read,
	WriteFn:   formats.Write,
	DiffFn:    formats.Diff,
	SetPathFn: formats.SetPath,
}

const sampleConfig = `{
  "provider": { "id": "custom", "name": "旧名字", "base_url": "https://api.openai.com/v1", "model": "gpt-4o" },
  "unknown_future_section": { "keep": true },
  "mcp_servers": [ { "name": "keep-me-too" } ]
}`

// TestFormatsAdapterMatchesLocalDocIO 用真实的 internal/formats 走一遍回退路径，
// 并与自带实现的结果逐字段比对。
func TestFormatsAdapterMatchesLocalDocIO(t *testing.T) {
	dir := t.TempDir()
	viaFormats := filepath.Join(dir, "via-formats.json")
	viaLocal := filepath.Join(dir, "via-local.json")
	for _, p := range []string{viaFormats, viaLocal} {
		if err := os.WriteFile(p, []byte(sampleConfig), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	key := "sk-adapter-check-123456"
	run := func(path string, port agents.DocIO) error {
		var out strings.Builder
		return agents.ApplyXimoAgent(agents.Config{
			GatewayURL: "http://127.0.0.1:8600",
			Model:      "gw-model-large",
			APIKey:     key,
			NoIPC:      true,
			ConfigPath: path,
			FileIO:     port,
			Output:     &out,
		})
	}
	if err := run(viaFormats, formatsPort); err != nil {
		t.Fatalf("用 internal/formats 走回退路径失败: %v", err)
	}
	if err := run(viaLocal, nil); err != nil {
		t.Fatalf("用自带 LocalDocIO 走回退路径失败: %v", err)
	}

	read := func(path string) map[string]any {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		doc := map[string]any{}
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		return doc
	}
	a, b := read(viaFormats), read(viaLocal)

	get := func(doc map[string]any, path ...string) any {
		var cur any = doc
		for _, seg := range path {
			m, ok := cur.(map[string]any)
			if !ok {
				t.Fatalf("路径 %v 在 %v 处断了", path, seg)
			}
			cur = m[seg]
		}
		return cur
	}

	for _, field := range []string{"base_url", "model", "secret_ref", "id", "name"} {
		if got, want := get(a, "provider", field), get(b, "provider", field); got != want {
			t.Errorf("provider.%s 两种实现不一致: formats=%v local=%v", field, got, want)
		}
	}
	// 未知字段两边都必须保留。
	for _, k := range []string{"unknown_future_section", "mcp_servers"} {
		if _, ok := a[k]; !ok {
			t.Errorf("formats 路径丢失未知字段 %s", k)
		}
		if _, ok := b[k]; !ok {
			t.Errorf("自带实现路径丢失未知字段 %s", k)
		}
	}
	// 两边都必须生成备份。
	for _, p := range []string{viaFormats, viaLocal} {
		if backups, _ := filepath.Glob(p + ".bak-*"); len(backups) != 1 {
			t.Errorf("%s 的备份数量 = %d", p, len(backups))
		}
	}
	// 明文密钥两边都不许落盘。
	for _, p := range []string{viaFormats, viaLocal} {
		raw, _ := os.ReadFile(p)
		if strings.Contains(string(raw), key) {
			t.Errorf("%s 里出现明文密钥", p)
		}
	}
}
