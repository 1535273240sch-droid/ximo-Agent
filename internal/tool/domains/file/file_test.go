package file

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ximo888ok-netizen/ximo-agent/internal/tool"
	"github.com/ximo888ok-netizen/ximo-agent/internal/tool/domains"
)

func setup(t *testing.T, roots []string) ([]tool.Tool, string) {
	t.Helper()
	dir := t.TempDir()
	guard := domains.NewGuard(roots)
	return Tools(guard), dir
}

func toolByName(t *testing.T, tools []tool.Tool, name string) tool.Tool {
	t.Helper()
	for _, tl := range tools {
		if tl.Definition().Name == name {
			return tl
		}
	}
	t.Fatalf("工具 %q 不存在", name)
	return nil
}

func exec(t *testing.T, tl tool.Tool, callID string, args map[string]any) tool.ToolResponse {
	t.Helper()
	return tl.Execute(context.Background(), tool.ToolRequest{
		RunID: "run-1", ToolCallID: callID, Name: tl.Definition().Name, Arguments: args,
	})
}

func TestFileReadWriteListSearchDelete(t *testing.T) {
	tools, dir := setup(t, nil)
	target := filepath.Join(dir, "sub", "hello.txt")

	// 写文件（自动建目录）
	write := toolByName(t, tools, "file_write")
	resp := exec(t, write, "c1", map[string]any{"filePath": target, "content": "line1\nline2\n"})
	if !resp.Success {
		t.Fatalf("写入失败: %+v", resp)
	}

	// 读文件（带行号）
	read := toolByName(t, tools, "file_read")
	resp = exec(t, read, "c2", map[string]any{"filePath": target})
	if !resp.Success || !strings.Contains(resp.Content, "line1") || !strings.Contains(resp.Content, "line2") {
		t.Fatalf("读取结果 = %+v", resp)
	}
	if !strings.Contains(resp.Content, "   1 | line1") {
		t.Fatalf("读取结果应带行号: %s", resp.Content)
	}

	// 区段读取
	resp = exec(t, read, "c3", map[string]any{"filePath": target, "startLine": 2, "endLine": 2})
	if !resp.Success || !strings.Contains(resp.Content, "line2") || strings.Contains(resp.Content, "line1") {
		t.Fatalf("区段读取 = %s", resp.Content)
	}

	// base64 读取
	resp = exec(t, read, "c4", map[string]any{"filePath": target, "encoding": "base64"})
	if !resp.Success || !strings.Contains(resp.Content, "base64") {
		t.Fatalf("base64 读取 = %+v", resp)
	}

	// 列目录
	list := toolByName(t, tools, "file_list")
	resp = exec(t, list, "c5", map[string]any{"path": dir})
	if !resp.Success || !strings.Contains(resp.Content, "sub/") || !strings.Contains(resp.Content, "hello.txt") {
		t.Fatalf("列目录 = %s", resp.Content)
	}

	// 文件名搜索
	search := toolByName(t, tools, "file_search")
	resp = exec(t, search, "c6", map[string]any{"path": dir, "pattern": "*.txt", "mode": "name"})
	if !resp.Success || !strings.Contains(resp.Content, "hello.txt") {
		t.Fatalf("文件名搜索 = %s", resp.Content)
	}

	// 内容搜索
	resp = exec(t, search, "c7", map[string]any{"path": dir, "pattern": "line2", "mode": "content"})
	if !resp.Success || !strings.Contains(resp.Content, "hello.txt:2") {
		t.Fatalf("内容搜索 = %s", resp.Content)
	}

	// 删除
	del := toolByName(t, tools, "file_delete")
	resp = exec(t, del, "c8", map[string]any{"filePath": target})
	if !resp.Success {
		t.Fatalf("删除失败: %+v", resp)
	}
	resp = exec(t, read, "c9", map[string]any{"filePath": target})
	if resp.Success {
		t.Fatalf("删除后读取应当失败")
	}
}

func TestFileWriteAppendMode(t *testing.T) {
	tools, dir := setup(t, nil)
	target := filepath.Join(dir, "a.txt")
	write := toolByName(t, tools, "file_write")
	exec(t, write, "c1", map[string]any{"filePath": target, "content": "one\n"})
	resp := exec(t, write, "c2", map[string]any{"filePath": target, "content": "two\n", "mode": "append"})
	if !resp.Success {
		t.Fatalf("追加失败: %+v", resp)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "one\ntwo\n" {
		t.Fatalf("追加结果 = %q", data)
	}
}

func TestFileWriteStateCheckerHash(t *testing.T) {
	tools, dir := setup(t, nil)
	target := filepath.Join(dir, "b.txt")
	write := toolByName(t, tools, "file_write")
	content := "expected content"

	// 尚未写入：状态为未完成
	checker, ok := write.(tool.StateChecker)
	if !ok {
		t.Fatalf("file_write 应实现 StateChecker（B 类可检测幂等）")
	}
	state, err := checker.CheckState(context.Background(), tool.ToolRequest{
		Arguments: map[string]any{"filePath": target, "expectedHash": hashOf(content)},
	})
	if err != nil || state != tool.StateNotDone {
		t.Fatalf("未写入时应为 StateNotDone，得到 %v %v", state, err)
	}

	// 写入后：状态为已完成
	exec(t, write, "c1", map[string]any{"filePath": target, "content": content})
	state, err = checker.CheckState(context.Background(), tool.ToolRequest{
		Arguments: map[string]any{"filePath": target, "expectedHash": hashOf(content)},
	})
	if err != nil || state != tool.StateAlreadyDone {
		t.Fatalf("写入匹配后应为 StateAlreadyDone，得到 %v %v", state, err)
	}

	// 内容不匹配：未完成
	state, _ = checker.CheckState(context.Background(), tool.ToolRequest{
		Arguments: map[string]any{"filePath": target, "expectedHash": hashOf("other")},
	})
	if state != tool.StateNotDone {
		t.Fatalf("内容不匹配应为 StateNotDone，得到 %v", state)
	}
}

func TestFileSensitiveFilesBlocked(t *testing.T) {
	tools, _ := setup(t, nil)
	read := toolByName(t, tools, "file_read")
	for _, path := range []string{"/home/u/.ssh/id_rsa", "/home/u/.env", "/home/u/credentials.json", "/home/u/cert.pem"} {
		resp := exec(t, read, "c", map[string]any{"filePath": path})
		if resp.Success {
			t.Fatalf("敏感文件 %q 不应可读", path)
		}
		if !strings.Contains(resp.Error, "敏感") {
			t.Fatalf("错误信息应说明是敏感文件: %s", resp.Error)
		}
	}
}

func TestFileWriteRootsEnforced(t *testing.T) {
	allowed := t.TempDir()
	tools, _ := setup(t, []string{allowed})
	outside := t.TempDir()
	write := toolByName(t, tools, "file_write")

	// 白名单内允许
	resp := exec(t, write, "c1", map[string]any{"filePath": filepath.Join(allowed, "ok.txt"), "content": "x"})
	if !resp.Success {
		t.Fatalf("白名单内写入应成功: %+v", resp)
	}
	// 白名单外拒绝
	resp = exec(t, write, "c2", map[string]any{"filePath": filepath.Join(outside, "no.txt"), "content": "x"})
	if resp.Success {
		t.Fatalf("白名单外写入应被拒绝")
	}
	// 删除同样受白名单约束
	del := toolByName(t, tools, "file_delete")
	resp = exec(t, del, "c3", map[string]any{"filePath": filepath.Join(outside, "no.txt")})
	if resp.Success {
		t.Fatalf("白名单外删除应被拒绝")
	}
}

func TestFileDeleteRefusesRoots(t *testing.T) {
	allowed := t.TempDir()
	tools, _ := setup(t, []string{allowed})
	del := toolByName(t, tools, "file_delete")
	if resp := exec(t, del, "c1", map[string]any{"filePath": allowed}); resp.Success {
		t.Fatalf("拒绝删除白名单根本身")
	}
}

func TestFileToolDefinitions(t *testing.T) {
	tools, _ := setup(t, nil)
	want := map[string]struct {
		risk        tool.RiskLevel
		idempotency tool.IdempotencyClass
		domain      tool.ExecutionDomain
	}{
		"file_read":   {tool.RiskLow, tool.ClassIdempotent, tool.DomainInProcess},
		"file_list":   {tool.RiskLow, tool.ClassIdempotent, tool.DomainInProcess},
		"file_search": {tool.RiskLow, tool.ClassIdempotent, tool.DomainInProcess},
		"file_write":  {tool.RiskMedium, tool.ClassDetectable, tool.DomainInProcess},
		"file_delete": {tool.RiskHigh, tool.ClassNonIdempotent, tool.DomainInProcess},
	}
	for _, tl := range tools {
		w, ok := want[tl.Definition().Name]
		if !ok {
			t.Fatalf("意外工具 %q", tl.Definition().Name)
		}
		if tl.Definition().Risk != w.risk || tl.Definition().Idempotency != w.idempotency || tl.Definition().Domain != w.domain {
			t.Fatalf("工具 %q 定义不匹配: %+v", tl.Definition().Name, tl.Definition())
		}
	}
}

func hashOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
