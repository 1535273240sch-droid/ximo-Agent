package office

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ximo888ok-netizen/ximo-agent/internal/worker"
)

// ============================== 动作清单（功能对等 v1） ==============================

// TestActionsMatchV1 验证动作清单与 v1 office-docs-helpers.ts 完全一致。
func TestActionsMatchV1(t *testing.T) {
	want := []string{
		"create", "get", "query", "set", "add", "remove", "move",
		"batch", "merge", "validate", "dump", "view", "save", "help",
	}
	if len(Actions) != len(want) {
		t.Fatalf("动作数不一致: got=%v want=%v", Actions, want)
	}
	for i := range want {
		if Actions[i] != want[i] {
			t.Errorf("第 %d 个动作: got=%q want=%q", i, Actions[i], want[i])
		}
	}
}

// TestWriteActionsMatchV1 验证写动作集合与 v1 WRITE_ACTIONS 一致
// （写前快照备份是 v1 已有的安全网，必须保留）。
func TestWriteActionsMatchV1(t *testing.T) {
	wantWrite := []string{"create", "set", "add", "remove", "move", "batch", "merge", "dump", "save"}
	wantRead := []string{"get", "query", "validate", "view", "help"}
	for _, a := range wantWrite {
		if !IsWriteAction(a) {
			t.Errorf("%q 应被标记为写动作（需要快照备份）", a)
		}
	}
	for _, a := range wantRead {
		if IsWriteAction(a) {
			t.Errorf("%q 不应被标记为写动作", a)
		}
	}
}

// ============================== 路径校验（fail-closed） ==============================

// TestResolvePathFailClosedWithoutRoots 验证未配置 AllowedRoots 时拒绝一切文件操作。
func TestResolvePathFailClosedWithoutRoots(t *testing.T) {
	w := NewWorker("office-0", Config{}) // 无 AllowedRoots
	if _, err := w.resolvePath("C:\\some\\doc.docx"); err == nil {
		t.Fatal("未配置 AllowedRoots 时必须 fail-closed 拒绝")
	}
}

// TestResolvePathOutsideRootsRejected 验证白名单外路径被拒绝。
func TestResolvePathOutsideRootsRejected(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	w := NewWorker("office-0", Config{AllowedRoots: []string{root}})

	// 必须是**策略拒绝**，而不是碰巧因为"路径解析失败"报错：错误类型错了说明
	// 边界是被绕过后又撞上别的分支，语义上不能算拒绝。
	if _, err := w.resolvePath(filepath.Join(outside, "x.docx")); err == nil {
		t.Error("白名单外路径应被拒绝")
	} else if !errors.Is(err, worker.ErrPolicyDenied) {
		t.Errorf("白名单外路径应以 ErrPolicyDenied 拒绝，实际: %v", err)
	}
	// 白名单内应通过（目标文件允许尚不存在，create 场景必须照常通过）。
	inside := filepath.Join(root, "x.docx")
	if _, err := w.resolvePath(inside); err != nil {
		t.Errorf("白名单内路径应通过: %v", err)
	}
}

// TestResolvePathDotDotEscapeRejected 验证 ".." 逃逸被拒绝。
// 这里刻意用字符串拼接（而不是 filepath.Join）把 ".." 原样交给 resolvePath，
// 确保规范化发生在包含性判断之前，且是 Clean 语义而不是字符串前缀比较。
func TestResolvePathDotDotEscapeRejected(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "ws")
	evil := filepath.Join(base, "evil")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(evil, 0o755); err != nil {
		t.Fatal(err)
	}

	w := NewWorker("office-0", Config{AllowedRoots: []string{root}})
	sep := string(filepath.Separator)

	// 越权目标已存在：借 ".." 出去必须被拒。
	if _, err := w.resolvePath(root + sep + ".." + sep + "evil" + sep + "x.docx"); err == nil {
		t.Error("带 .. 的越权路径必须被拒绝")
	}
	// 越权目标不存在（create 场景）同样不能借 ".." 逃出白名单。
	if _, err := w.resolvePath(root + sep + ".." + sep + "evil" + sep + "new.docx"); err == nil {
		t.Error("带 .. 的不存在路径必须被拒绝")
	}
	// 反向确认 root 自身仍可用（避免把 ".." 处理成"一律拒绝"）。
	if _, err := w.resolvePath(root + sep + "ok.docx"); err != nil {
		t.Errorf("白名单根内的普通路径应通过: %v", err)
	}
}

// TestResolvePathPrefixTrickRejected 验证前缀相同的兄弟目录不被误放行
// （经典 "C:\data" vs "C:\database" 错误）。
func TestResolvePathPrefixTrickRejected(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "data")
	sibling := filepath.Join(base, "database")
	_ = os.MkdirAll(root, 0o755)
	_ = os.MkdirAll(sibling, 0o755)

	w := NewWorker("office-0", Config{AllowedRoots: []string{root}})
	if _, err := w.resolvePath(filepath.Join(sibling, "x.docx")); err == nil {
		t.Error("前缀相同的兄弟目录不应被放行")
	}
	// 兄弟目录里尚不存在的文件同样不能因为前缀相同而放行。
	if _, err := w.resolvePath(filepath.Join(sibling, "new.docx")); err == nil {
		t.Error("前缀相同的兄弟目录（目标不存在）不应被放行")
	}
}

// TestResolvePathSymlinkEscapeRejected 验证 symlink 逃逸被拒绝：
// 白名单内的链接指向白名单外时，必须按链接的真实目标判定，而不是按链接自身的字面路径。
func TestResolvePathSymlinkEscapeRejected(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "ws")
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("当前环境无法创建符号链接，跳过: %v", err)
	}

	w := NewWorker("office-0", Config{AllowedRoots: []string{root}})
	// 链接指向的白名单外文件已存在。
	if err := os.WriteFile(filepath.Join(outside, "x.docx"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := w.resolvePath(filepath.Join(link, "x.docx")); err == nil {
		t.Error("通过 symlink 指向白名单外的已存在路径必须被拒绝")
	}
	// 链接指向的白名单外文件尚不存在（create 场景）也不能跟着链接写出去。
	if _, err := w.resolvePath(filepath.Join(link, "new.docx")); err == nil {
		t.Error("通过 symlink 指向白名单外的新建路径必须被拒绝")
	}

	// 悬空 symlink：目标不存在，真实位置不可知，必须 fail-closed。
	dangling := filepath.Join(root, "dangling")
	if err := os.Symlink(filepath.Join(base, "not-created"), dangling); err != nil {
		t.Skipf("当前环境无法创建符号链接，跳过: %v", err)
	}
	if _, err := w.resolvePath(filepath.Join(dangling, "new.docx")); err == nil {
		t.Error("悬空 symlink 下的路径必须被拒绝（目标不可知，fail-closed）")
	}
}

// TestResolvePathAcceptsEquivalentRootSpelling 验证"同一目录的两种等价写法"都算在白名单内。
//
// 这是本机 t.TempDir() 触发的假性拒绝的跨平台复现：当目标文件**尚不存在**时，
// EvalSymlinks 会直接报错，旧实现于是拿"未规范化的路径"去比对"已规范化的 root"，
// 同一目录的等价写法被判成了越权。Windows 上的 8.3 短名（%TEMP% 常是短名）就是
// 这种等价写法之一，见 worker_windows_test.go。
func TestResolvePathAcceptsEquivalentRootSpelling(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Skipf("当前环境无法创建符号链接，跳过: %v", err)
	}

	// 白名单写真实目录，调用方用别名给出"尚未创建"的文件。
	w := NewWorker("office-0", Config{AllowedRoots: []string{real}})
	got, err := w.resolvePath(filepath.Join(alias, "new.docx"))
	if err != nil {
		t.Errorf("等价写法（symlink 别名）下的新文件应通过: %v", err)
	} else {
		// 返回值必须是规范化后的真实路径：透传别名给 officecli 会让"校验的路径"和
		// "真正被写入的路径"再次出现两套写法。期望值也必须先 EvalSymlinks，
		// 否则测试自己就会掉进"短名 vs 长名"的坑（本机 %TEMP% 正是短名）。
		canonicalReal, err := filepath.EvalSymlinks(real)
		if err != nil {
			t.Fatal(err)
		}
		if want := filepath.Join(canonicalReal, "new.docx"); !strings.EqualFold(got, want) {
			t.Errorf("应返回规范化后的真实路径: got=%s want=%s", got, want)
		}
	}
	// 多级尚不存在的子目录同样要按"已存在前缀"规范化。
	if _, err := w.resolvePath(filepath.Join(alias, "a", "b", "new.docx")); err != nil {
		t.Errorf("别名下多级新目录中的文件应通过: %v", err)
	}

	// 反向：白名单写别名，调用方写真实目录。
	w2 := NewWorker("office-0", Config{AllowedRoots: []string{alias}})
	if _, err := w2.resolvePath(filepath.Join(real, "new.docx")); err != nil {
		t.Errorf("真实路径在别名白名单下应通过: %v", err)
	}
}

// TestResolvePathEmptyRootDoesNotWidenPolicy 验证 allowed_roots 里的空串不会把 cwd
// 变成合法根（filepath.Abs("") 会返回当前工作目录，等于凭空放宽白名单）。
func TestResolvePathEmptyRootDoesNotWidenPolicy(t *testing.T) {
	w := NewWorker("office-0", Config{AllowedRoots: []string{"  "}})
	if _, err := w.resolvePath("x.docx"); err == nil {
		t.Error("空 root 不应让相对路径（cwd）通过白名单")
	}
}

// ============================== 二进制探测 ==============================

// TestResolveBinaryMissingGivesActionableError 验证找不到 officecli 时错误可操作。
func TestResolveBinaryMissingGivesActionableError(t *testing.T) {
	t.Setenv("OFFICECLI_PATH", "")
	t.Setenv("PATH", t.TempDir()) // 清空 PATH，确保找不到
	w := NewWorker("office-0", Config{})
	_, err := w.resolveBinary()
	if err == nil {
		t.Skip("系统 PATH 中确实存在 officecli，跳过")
	}
	if !strings.Contains(err.Error(), "OFFICECLI_PATH") {
		t.Errorf("错误信息应提示如何配置，实际: %v", err)
	}
}

// TestResolveBinaryFromConfig 验证配置路径优先。
func TestResolveBinaryFromConfig(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "officecli-fake")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	w := NewWorker("office-0", Config{BinaryPath: bin})
	got, err := w.resolveBinary()
	if err != nil {
		t.Fatalf("应能从配置解析: %v", err)
	}
	if got != bin {
		t.Errorf("应返回配置路径，实际 %s", got)
	}
}

// TestResolveBinaryFromEnv 验证 OFFICECLI_PATH 支持目录语义（对齐 v1 的定位顺序）。
func TestResolveBinaryFromEnv(t *testing.T) {
	dir := t.TempDir()
	// 必须用平台约定的可执行文件名，因为 OFFICECLI_PATH 是指向"目录"的。
	bin := filepath.Join(dir, exeName())
	if err := os.WriteFile(bin, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OFFICECLI_PATH", dir)
	w := NewWorker("office-0", Config{})
	got, err := w.resolveBinary()
	if err != nil {
		t.Fatalf("应能从 OFFICECLI_PATH 解析: %v", err)
	}
	if got != bin {
		t.Errorf("got=%s want=%s", got, bin)
	}
}

// ============================== 快照备份（可逆写安全网） ==============================

// TestSnapshotCreatedForWrite 验证写操作前创建快照。
func TestSnapshotCreatedForWrite(t *testing.T) {
	root := t.TempDir()
	snapDir := t.TempDir()
	doc := filepath.Join(root, "doc.docx")
	original := []byte("original-content")
	if err := os.WriteFile(doc, original, 0o600); err != nil {
		t.Fatal(err)
	}

	w := NewWorker("office-0", Config{
		AllowedRoots: []string{root},
		SnapshotDir:  snapDir,
	})
	snap, err := w.snapshot(doc)
	if err != nil {
		t.Fatalf("快照失败: %v", err)
	}
	// 快照内容必须与原文件一致。
	got, err := os.ReadFile(snap)
	if err != nil {
		t.Fatalf("读取快照失败: %v", err)
	}
	if string(got) != string(original) {
		t.Errorf("快照内容不一致: %q", got)
	}
	// 快照文件应在配置目录下（不是写死 %TEMP%）。
	if !strings.HasPrefix(snap, snapDir) {
		t.Errorf("快照应位于配置目录 %s 下，实际 %s", snapDir, snap)
	}
}

// TestSnapshotSkippedWhenTooLarge 验证超大文件跳过备份（避免复制爆磁盘）。
func TestSnapshotSkippedWhenTooLarge(t *testing.T) {
	root := t.TempDir()
	snapDir := t.TempDir()
	doc := filepath.Join(root, "big.docx")
	if err := os.WriteFile(doc, make([]byte, 2048), 0o600); err != nil {
		t.Fatal(err)
	}
	w := NewWorker("office-0", Config{
		AllowedRoots:     []string{root},
		SnapshotDir:      snapDir,
		MaxSnapshotBytes: 100, // 远小于文件
	})
	if _, err := w.snapshot(doc); err == nil {
		t.Error("超过 MaxSnapshotBytes 时应跳过备份并报告原因")
	}
}

// TestSnapshotPruning 验证快照保留数量生效。
func TestSnapshotPruning(t *testing.T) {
	root := t.TempDir()
	snapDir := t.TempDir()
	doc := filepath.Join(root, "doc.docx")

	w := NewWorker("office-0", Config{
		AllowedRoots:  []string{root},
		SnapshotDir:   snapDir,
		KeepSnapshots: 2,
	})
	for i := 0; i < 5; i++ {
		if err := os.WriteFile(doc, []byte(strings.Repeat("x", i+1)), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := w.snapshot(doc); err != nil {
			t.Fatalf("第 %d 次快照失败: %v", i, err)
		}
	}
	entries, err := os.ReadDir(snapDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) > 2 {
		t.Errorf("应只保留 2 份快照，实际 %d", len(entries))
	}
}

// ============================== 参数映射（对齐 v1） ==============================

// TestBuildArgsMapsProperties 验证 properties → --prop key=val，且 type 提升为 --type。
func TestBuildArgsMapsProperties(t *testing.T) {
	args := buildArgs(docsArgs{
		Action:   "set",
		FilePath: "/tmp/a.docx",
		Path:     "/body/p[1]",
		Properties: map[string]any{
			"text": "hello",
			"type": "paragraph",
		},
	}, "/tmp/a.docx", "")

	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "set") || !strings.Contains(joined, "--file /tmp/a.docx") {
		t.Errorf("基本参数缺失: %v", args)
	}
	if !strings.Contains(joined, "--type paragraph") {
		t.Errorf("properties.type 应提升为 --type: %v", args)
	}
	if !strings.Contains(joined, "--prop text=hello") {
		t.Errorf("其余属性应为 --prop k=v: %v", args)
	}
}

// TestBuildArgsViewDefaultMode 验证 view 默认 mode=screenshot（对齐 v1）。
func TestBuildArgsViewDefaultMode(t *testing.T) {
	args := buildArgs(docsArgs{Action: "view", FilePath: "/tmp/a.docx"}, "/tmp/a.docx", "")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--mode screenshot") {
		t.Errorf("view 应默认 mode=screenshot: %v", args)
	}
}

// TestBuildArgsBatchOperations 验证 batch 的 operations 序列化为 JSON。
func TestBuildArgsBatchOperations(t *testing.T) {
	ops := []any{
		map[string]any{"command": "set", "path": "/a", "props": map[string]any{"x": 1}},
	}
	args := buildArgs(docsArgs{Action: "batch", FilePath: "/tmp/a.docx", Operations: ops}, "/tmp/a.docx", "")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--operations") {
		t.Errorf("batch 应带 --operations: %v", args)
	}
	if !strings.Contains(joined, "\"command\":\"set\"") {
		t.Errorf("operations 应为 JSON: %v", args)
	}
}

// TestBuildArgsMergeTemplateData 验证 merge 的 templateData → --data。
func TestBuildArgsMergeTemplateData(t *testing.T) {
	args := buildArgs(docsArgs{
		Action:       "merge",
		FilePath:     "/tmp/tpl.docx",
		TemplateData: map[string]any{"name": "X"},
	}, "/tmp/tpl.docx", "")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--data") || !strings.Contains(joined, `"name":"X"`) {
		t.Errorf("merge 应带 --data JSON: %v", args)
	}
}

// TestBuildArgsOutput 验证 outputPath → --output。
func TestBuildArgsOutput(t *testing.T) {
	args := buildArgs(docsArgs{Action: "create", FilePath: "/tmp/a.docx"},
		"/tmp/a.docx", "/tmp/out.docx")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--output /tmp/out.docx") {
		t.Errorf("应带 --output: %v", args)
	}
}

// ============================== 执行与错误处理 ==============================

// TestExecuteUnknownActionRejected 验证未知动作被拒绝。
func TestExecuteUnknownActionRejected(t *testing.T) {
	w := NewWorker("office-0", Config{AllowedRoots: []string{t.TempDir()}})
	_ = w.Start(context.Background())
	defer w.Stop(context.Background())

	args, _ := json.Marshal(docsArgs{Action: "explode", FilePath: "/tmp/x.docx"})
	resp, _ := w.Execute(context.Background(), worker.WorkerRequest{
		CallID: "c1", Action: ActionDocs, Args: args,
	})
	if resp.Error == nil || resp.Error.Code != worker.CodeInvalidArgument {
		t.Errorf("未知动作应返回 invalid_argument，实际 %+v", resp.Error)
	}
}

// TestExecuteRequiresFilePath 验证 filePath 必填（help 除外）。
func TestExecuteRequiresFilePath(t *testing.T) {
	w := NewWorker("office-0", Config{AllowedRoots: []string{t.TempDir()}})
	_ = w.Start(context.Background())
	defer w.Stop(context.Background())

	args, _ := json.Marshal(docsArgs{Action: "get"})
	resp, _ := w.Execute(context.Background(), worker.WorkerRequest{
		CallID: "c1", Action: ActionDocs, Args: args,
	})
	if resp.Error == nil || resp.Error.Code != worker.CodeInvalidArgument {
		t.Errorf("缺少 filePath 应返回 invalid_argument，实际 %+v", resp.Error)
	}
}

// TestExecutePathOutsideRootsDenied 验证越权路径在执行入口被拒（fail-closed）。
func TestExecutePathOutsideRootsDenied(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	w := NewWorker("office-0", Config{AllowedRoots: []string{root}})
	_ = w.Start(context.Background())
	defer w.Stop(context.Background())

	args, _ := json.Marshal(docsArgs{
		Action:   "get",
		FilePath: filepath.Join(outside, "secret.docx"),
	})
	resp, _ := w.Execute(context.Background(), worker.WorkerRequest{
		CallID: "c1", Action: ActionDocs, Args: args,
	})
	if resp.Error == nil || resp.Error.Code != worker.CodePolicyDenied {
		t.Errorf("越权路径应返回 policy_denied，实际 %+v", resp.Error)
	}
}

// TestExecuteBinaryMissingIsReported 验证 officecli 缺失时明确上报（不静默成功）。
func TestExecuteBinaryMissingIsReported(t *testing.T) {
	t.Setenv("OFFICECLI_PATH", "")
	t.Setenv("PATH", t.TempDir())
	root := t.TempDir()
	w := NewWorker("office-0", Config{AllowedRoots: []string{root}, BinaryPath: ""})
	_ = w.Start(context.Background())
	defer w.Stop(context.Background())

	args, _ := json.Marshal(docsArgs{Action: "get", FilePath: filepath.Join(root, "a.docx")})
	resp, _ := w.Execute(context.Background(), worker.WorkerRequest{
		CallID: "c1", Action: ActionDocs, Args: args,
	})
	if resp.Error == nil {
		t.Skip("系统 PATH 中确实存在 officecli，跳过")
	}
	if resp.Status == worker.StatusOK {
		t.Error("officecli 缺失时不应返回成功")
	}
}

// TestHealthReportsUnavailableWhenBinaryMissing 验证健康检查如实报告不可用。
func TestHealthReportsUnavailableWhenBinaryMissing(t *testing.T) {
	t.Setenv("OFFICECLI_PATH", "")
	t.Setenv("PATH", t.TempDir())
	w := NewWorker("office-0", Config{})
	err := w.Health(context.Background())
	if err == nil {
		t.Skip("系统 PATH 中确实存在 officecli，跳过")
	}
	if !strings.Contains(err.Error(), "officecli") {
		t.Errorf("健康检查应说明缺失原因: %v", err)
	}
}

// ============================== 输出解析 ==============================

// TestTryParseJSONPlain 验证纯 JSON 输出解析。
func TestTryParseJSONPlain(t *testing.T) {
	v := tryParseJSON(`{"a":1}`)
	m, ok := v.(map[string]any)
	if !ok || m["a"].(float64) != 1 {
		t.Errorf("应解析出 JSON 对象，实际 %#v", v)
	}
}

// TestTryParseJSONWithLogNoise 验证混有日志行时仍能提取 JSON（对齐 v1 的区间解析）。
func TestTryParseJSONWithLogNoise(t *testing.T) {
	out := "info: starting\n{\"result\":\"ok\"}\ndone"
	v := tryParseJSON(out)
	m, ok := v.(map[string]any)
	if !ok || m["result"] != "ok" {
		t.Errorf("应从日志中提取 JSON，实际 %#v", v)
	}
}

// TestTryParseJSONInvalid 验证非 JSON 返回 nil（不 panic）。
func TestTryParseJSONInvalid(t *testing.T) {
	if v := tryParseJSON("not json at all"); v != nil {
		t.Errorf("非 JSON 应返回 nil，实际 %#v", v)
	}
	if v := tryParseJSON(""); v != nil {
		t.Errorf("空串应返回 nil，实际 %#v", v)
	}
}

// TestKindAndID 验证身份方法。
func TestKindAndID(t *testing.T) {
	w := NewWorker("office-3", Config{})
	if w.ID() != "office-3" || w.Kind() != "office" {
		t.Errorf("身份错误: %s/%s", w.ID(), w.Kind())
	}
}

// TestNewWorkerFromSpec 验证从 Spec 构造。
func TestNewWorkerFromSpec(t *testing.T) {
	spec := worker.Spec{
		ID: "office-1", Kind: "office",
		Config: map[string]any{
			"allowed_roots": []any{"/tmp/work"},
			"timeout":       "45s",
		},
	}
	w, err := NewWorkerFromSpec(spec)
	if err != nil {
		t.Fatal(err)
	}
	ow := w.(*Worker)
	if len(ow.cfg.AllowedRoots) != 1 || ow.cfg.Timeout.String() != "45s" {
		t.Errorf("配置未正确读取: %+v", ow.cfg)
	}
}
