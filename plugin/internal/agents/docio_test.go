package agents

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestLocalDocIOReadMissing 不存在的文件返回空 map + nil（由调用方决定是否创建）。
func TestLocalDocIOReadMissing(t *testing.T) {
	doc, err := (LocalDocIO{}).Read(filepath.Join(t.TempDir(), "nope.json"), "json")
	if err != nil {
		t.Fatalf("Read(不存在) = %v", err)
	}
	if len(doc) != 0 {
		t.Fatalf("doc = %+v, want empty", doc)
	}
}

// TestLocalDocIOWritePreservesUnknownFields 未知字段必须原样保留，且写入是原子的
// （写完不留临时文件）。
func TestLocalDocIOWritePreservesUnknownFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	original := `{
  "provider": { "base_url": "https://old.example.com", "model": "old-model" },
  "weird": { "nested": [1, 2, {"deep": true}] },
  "emoji": "保留我 🚀"
}`
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	doc, err := (LocalDocIO{}).Read(path, "json")
	if err != nil {
		t.Fatal(err)
	}
	if err := (LocalDocIO{}).SetPath(doc, "provider.base_url", "http://127.0.0.1:8600"); err != nil {
		t.Fatal(err)
	}
	if err := (LocalDocIO{}).Write(path, "json", doc, true); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]any{}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("写出的不是合法 JSON: %v", err)
	}
	if _, ok := got["weird"]; !ok {
		t.Error("未知字段 weird 被丢弃")
	}
	if got["emoji"] != "保留我 🚀" {
		t.Errorf("非 ASCII 字段被改动: %v", got["emoji"])
	}
	provider, _ := got["provider"].(map[string]any)
	if provider["base_url"] != "http://127.0.0.1:8600" {
		t.Errorf("字段未更新: %+v", provider)
	}
	if provider["model"] != "old-model" {
		t.Errorf("未触碰的字段被改写: %+v", provider)
	}

	// 备份存在且内容等于写前内容。
	backups, _ := filepath.Glob(path + ".bak-*")
	if len(backups) != 1 {
		t.Fatalf("备份数量 = %d (%v)", len(backups), backups)
	}
	back, err := os.ReadFile(backups[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(back) != original {
		t.Errorf("备份内容 != 写前内容:\n%s", string(back))
	}

	// 原子写：目录里不该留下临时文件。
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".ximo-plugin-") {
			t.Errorf("残留临时文件: %s", e.Name())
		}
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o644 {
			t.Errorf("权限 = %v, want 0644", info.Mode().Perm())
		}
	}
}

// TestLocalDocIOBackupSkippedWhenMissing 目标不存在时不报错也不生成备份。
func TestLocalDocIOBackupSkippedWhenMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "new.json")
	if err := (LocalDocIO{}).Write(path, "json", map[string]any{"a": 1}, true); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if backups, _ := filepath.Glob(path + ".bak-*"); len(backups) != 0 {
		t.Fatalf("不该有备份: %v", backups)
	}
}

// TestLocalDocIORejectsBadJSON 坏 JSON 必须拒绝改写，而不是先清空再写。
func TestLocalDocIORejectsBadJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "broken.json")
	if err := os.WriteFile(path, []byte("{ this is not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := (LocalDocIO{}).Read(path, "json"); err == nil {
		t.Fatal("坏 JSON 被当成合法配置")
	}
	if raw, _ := os.ReadFile(path); string(raw) != "{ this is not json" {
		t.Fatal("坏 JSON 被改写")
	}
}

// TestLocalDocIOSetPath 点号路径（含数组下标）与类型冲突。
func TestLocalDocIOSetPath(t *testing.T) {
	doc := map[string]any{
		"models": []any{
			map[string]any{"name": "a"},
			map[string]any{"name": "b"},
		},
	}
	if err := (LocalDocIO{}).SetPath(doc, "models.1.name", "B"); err != nil {
		t.Fatalf("SetPath(数组下标): %v", err)
	}
	models := doc["models"].([]any)
	if models[1].(map[string]any)["name"] != "B" {
		t.Fatalf("数组元素未更新: %+v", models)
	}

	// 中间层缺失时按下一段的形态补出来。
	if err := (LocalDocIO{}).SetPath(doc, "provider.base_url", "http://gw"); err != nil {
		t.Fatalf("SetPath(补齐中间层): %v", err)
	}
	if doc["provider"].(map[string]any)["base_url"] != "http://gw" {
		t.Fatalf("中间层未补齐: %+v", doc)
	}
	if err := (LocalDocIO{}).SetPath(doc, "list.0.id", "x"); err != nil {
		t.Fatalf("SetPath(数组补齐): %v", err)
	}
	if doc["list"].([]any)[0].(map[string]any)["id"] != "x" {
		t.Fatalf("数组未按下标补齐: %+v", doc["list"])
	}

	// 出错的情形：越界、类型冲突、空段。
	if err := (LocalDocIO{}).SetPath(doc, "models.9.name", "x"); err == nil {
		t.Fatal("下标越界却成功")
	}
	if err := (LocalDocIO{}).SetPath(doc, "provider..base_url", "x"); err == nil {
		t.Fatal("空段却成功")
	}
	conflict := map[string]any{"scalar": "text"}
	if err := (LocalDocIO{}).SetPath(conflict, "scalar.deeper", "x"); err == nil {
		t.Fatal("撞上标量却成功（应报错而不是覆盖用户的字符串）")
	}
	if conflict["scalar"] != "text" {
		t.Fatal("报错路径上仍然改写了数据")
	}
}

// TestLocalDocIODiff 差异输出必须标出改动行。
func TestLocalDocIODiff(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	original := `{
  "provider": {
    "base_url": "https://old.example.com",
    "model": "old-model"
  },
  "keep": 1
}`
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	doc, err := (LocalDocIO{}).Read(path, "json")
	if err != nil {
		t.Fatal(err)
	}
	if err := (LocalDocIO{}).SetPath(doc, "provider.base_url", "http://127.0.0.1:8600"); err != nil {
		t.Fatal(err)
	}
	diff, err := (LocalDocIO{}).Diff(path, "json", doc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff, `-    "base_url": "https://old.example.com",`) {
		t.Errorf("差异里缺少删除行:\n%s", diff)
	}
	if !strings.Contains(diff, `+    "base_url": "http://127.0.0.1:8600",`) {
		t.Errorf("差异里缺少新增行:\n%s", diff)
	}
	if strings.Contains(diff, `+    "model": "old-model",`) {
		t.Errorf("未改动的行被当成新增:\n%s", diff)
	}
}

// TestLocalDocIORejectsNonJSONFormat 自带实现只认 json，其它格式要明确报错
// （避免用 JSON 解析器去写 TOML 把别人的文件弄坏）。
func TestLocalDocIORejectsNonJSONFormat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := (LocalDocIO{}).Write(path, "toml", map[string]any{"a": 1}, false); err == nil {
		t.Fatal("toml 格式被接受")
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("报错了却仍然创建了文件")
	}
}

// TestFuncDocIODelegates 端口适配器必须把调用转给注入的函数。
func TestFuncDocIODelegates(t *testing.T) {
	var called []string
	io := FuncDocIO{
		ReadFn: func(p, f string) (map[string]any, error) {
			called = append(called, "read")
			return map[string]any{}, nil
		},
		WriteFn:   func(p, f string, d map[string]any, b bool) error { called = append(called, "write"); return nil },
		DiffFn:    func(p, f string, n map[string]any) (string, error) { called = append(called, "diff"); return "", nil },
		SetPathFn: func(d map[string]any, p string, v any) error { called = append(called, "setpath"); return nil },
	}
	if _, err := io.Read("a", "json"); err != nil {
		t.Fatal(err)
	}
	if err := io.Write("a", "json", nil, true); err != nil {
		t.Fatal(err)
	}
	if _, err := io.Diff("a", "json", nil); err != nil {
		t.Fatal(err)
	}
	if err := io.SetPath(map[string]any{}, "a", 1); err != nil {
		t.Fatal(err)
	}
	if strings.Join(called, ",") != "read,write,diff,setpath" {
		t.Fatalf("调用未转发: %v", called)
	}

	// 未设置的函数必须报错而不是 panic。
	if _, err := (FuncDocIO{}).Read("a", "json"); err == nil {
		t.Fatal("空适配器没有报错")
	}
}

// TestApplyXimoAgentUsesInjectedDocIO 回退路径必须走注入的读写实现
// （P-Core 可以在这里塞 internal/formats，测试里塞记录型替身）。
func TestApplyXimoAgentUsesInjectedDocIO(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"provider":{"base_url":"https://old"},"keep":1}`), 0o644); err != nil {
		t.Fatal(err)
	}

	inner := LocalDocIO{}
	reads, writes, diffs := 0, 0, 0
	injected := FuncDocIO{
		ReadFn: func(p, f string) (map[string]any, error) {
			reads++
			return inner.Read(p, f)
		},
		WriteFn: func(p, f string, d map[string]any, b bool) error {
			writes++
			return inner.Write(p, f, d, b)
		},
		DiffFn: func(p, f string, n map[string]any) (string, error) {
			diffs++
			return inner.Diff(p, f, n)
		},
		SetPathFn: inner.SetPath,
	}

	var out strings.Builder
	if err := ApplyXimoAgent(Config{
		GatewayURL: "http://127.0.0.1:8600", Model: "m", NoIPC: true,
		ConfigPath: path, FileIO: injected, Output: &out,
	}); err != nil {
		t.Fatalf("ApplyXimoAgent: %v\n%s", err, out.String())
	}
	if reads != 1 || writes != 1 {
		t.Fatalf("注入的读写实现没被调用: reads=%d writes=%d diffs=%d", reads, writes, diffs)
	}
}
