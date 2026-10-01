package file

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ximo888ok-netizen/ximo-agent/internal/tool"
)

// 本文件覆盖 file_edit / multi_edit。除功能本身，重点锁住三类「静默损坏」风险：
// 行尾被归一化、末尾换行被补/删、multi_edit 只应用了一部分。

func writeRaw(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readRaw(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func metaInt(t *testing.T, resp tool.ToolResponse, key string) int {
	t.Helper()
	v, ok := resp.Metadata[key]
	if !ok {
		t.Fatalf("metadata 缺少 %q：%+v", key, resp.Metadata)
	}
	n, ok := v.(int)
	if !ok {
		t.Fatalf("metadata[%q] 应为 int，实际 %T", key, v)
	}
	return n
}

func metaString(t *testing.T, resp tool.ToolResponse, key string) string {
	t.Helper()
	v, ok := resp.Metadata[key]
	if !ok {
		t.Fatalf("metadata 缺少 %q：%+v", key, resp.Metadata)
	}
	s, ok := v.(string)
	if !ok {
		t.Fatalf("metadata[%q] 应为 string，实际 %T", key, v)
	}
	return s
}

func TestFileEditUniqueMatch(t *testing.T) {
	tools, dir := setup(t, nil)
	target := filepath.Join(dir, "a.txt")
	writeRaw(t, target, "alpha\nbeta\ngamma\n")

	ed := toolByName(t, tools, "file_edit")
	resp := exec(t, ed, "c1", map[string]any{"path": target, "old_string": "beta", "new_string": "BETA"})
	if !resp.Success {
		t.Fatalf("编辑失败: %+v", resp)
	}
	if got := readRaw(t, target); got != "alpha\nBETA\ngamma\n" {
		t.Fatalf("内容 = %q", got)
	}
	if !strings.Contains(resp.Content, "已替换 1 处") || !strings.Contains(resp.Content, target) {
		t.Fatalf("摘要 = %q", resp.Content)
	}
	if got := metaString(t, resp, "path"); got != target {
		t.Fatalf("metadata.path = %q，期望 %q", got, target)
	}
	if got := metaInt(t, resp, "replacements"); got != 1 {
		t.Fatalf("replacements = %d，期望 1", got)
	}
	if got, want := metaInt(t, resp, "bytes_before"), len("alpha\nbeta\ngamma\n"); got != want {
		t.Fatalf("bytes_before = %d，期望 %d", got, want)
	}
	if got, want := metaInt(t, resp, "bytes_after"), len("alpha\nBETA\ngamma\n"); got != want {
		t.Fatalf("bytes_after = %d，期望 %d", got, want)
	}
}

func TestFileEditPreservesCRLF(t *testing.T) {
	tools, dir := setup(t, nil)
	target := filepath.Join(dir, "crlf.txt")
	writeRaw(t, target, "one\r\ntwo\r\nthree\r\n")

	ed := toolByName(t, tools, "file_edit")
	resp := exec(t, ed, "c1", map[string]any{"path": target, "old_string": "two", "new_string": "2"})
	if !resp.Success {
		t.Fatalf("编辑失败: %+v", resp)
	}
	got := readRaw(t, target)
	if got != "one\r\n2\r\nthree\r\n" {
		t.Fatalf("内容 = %q，期望 one\\r\\n2\\r\\nthree\\r\\n", got)
	}
	// 未被触碰的行也必须是 CRLF：不能因为一次编辑把全文归一化成 LF。
	if strings.Contains(strings.ReplaceAll(got, "\r\n", ""), "\n") {
		t.Fatalf("出现裸 LF，行尾被归一化: %q", got)
	}
}

func TestFileEditCRLFInOldString(t *testing.T) {
	tools, dir := setup(t, nil)
	target := filepath.Join(dir, "crlf-del.txt")
	writeRaw(t, target, "keep\r\ndrop\r\ntail\r\n")

	ed := toolByName(t, tools, "file_edit")
	// old_string 带换行时，模型必须按文件真实字节给出 CRLF，删掉整行。
	resp := exec(t, ed, "c1", map[string]any{"path": target, "old_string": "drop\r\n", "new_string": ""})
	if !resp.Success {
		t.Fatalf("删除行失败: %+v", resp)
	}
	if got := readRaw(t, target); got != "keep\r\ntail\r\n" {
		t.Fatalf("内容 = %q，期望 keep\\r\\ntail\\r\\n", got)
	}
}

func TestFileEditPreservesMissingTrailingNewline(t *testing.T) {
	tools, dir := setup(t, nil)
	target := filepath.Join(dir, "noeol.txt")
	writeRaw(t, target, "a\nb") // 末尾没有换行

	ed := toolByName(t, tools, "file_edit")
	resp := exec(t, ed, "c1", map[string]any{"path": target, "old_string": "b", "new_string": "B"})
	if !resp.Success {
		t.Fatalf("编辑失败: %+v", resp)
	}
	if got := readRaw(t, target); got != "a\nB" {
		t.Fatalf("内容 = %q，期望 a\\nB（不得补末尾换行）", got)
	}
}

func TestFileEditNoMatch(t *testing.T) {
	tools, dir := setup(t, nil)
	target := filepath.Join(dir, "nomatch.txt")
	writeRaw(t, target, "alpha\nbeta\n")

	ed := toolByName(t, tools, "file_edit")
	resp := exec(t, ed, "c1", map[string]any{"path": target, "old_string": "zzz", "new_string": "x"})
	if resp.Success {
		t.Fatalf("未找到匹配时不应成功: %+v", resp)
	}
	if !strings.Contains(resp.Error, "未找到") {
		t.Fatalf("错误信息应说明未找到: %q", resp.Error)
	}
	if resp.ErrorCode != tool.ErrToolFailed {
		t.Fatalf("ErrorCode = %q，期望 %q", resp.ErrorCode, tool.ErrToolFailed)
	}
	if got := readRaw(t, target); got != "alpha\nbeta\n" {
		t.Fatalf("失败时文件不应被改动: %q", got)
	}
}

func TestFileEditMultipleMatchesNeedFlag(t *testing.T) {
	tools, dir := setup(t, nil)
	target := filepath.Join(dir, "dup.txt")
	writeRaw(t, target, "dup dup dup\n")

	ed := toolByName(t, tools, "file_edit")
	resp := exec(t, ed, "c1", map[string]any{"path": target, "old_string": "dup", "new_string": "D"})
	if resp.Success {
		t.Fatalf("多处匹配且未开 replace_all 时不应成功: %+v", resp)
	}
	if !strings.Contains(resp.Error, "找到 3 处") || !strings.Contains(resp.Error, "replace_all") {
		t.Fatalf("错误信息应报出匹配数量并提示 replace_all: %q", resp.Error)
	}
	if got := readRaw(t, target); got != "dup dup dup\n" {
		t.Fatalf("失败时文件不应被改动: %q", got)
	}
}

func TestFileEditReplaceAll(t *testing.T) {
	tools, dir := setup(t, nil)
	target := filepath.Join(dir, "all.txt")
	writeRaw(t, target, "dup dup dup\n")

	ed := toolByName(t, tools, "file_edit")
	resp := exec(t, ed, "c1", map[string]any{
		"path": target, "old_string": "dup", "new_string": "D", "replace_all": true,
	})
	if !resp.Success {
		t.Fatalf("replace_all 失败: %+v", resp)
	}
	if got := readRaw(t, target); got != "D D D\n" {
		t.Fatalf("内容 = %q", got)
	}
	if got := metaInt(t, resp, "replacements"); got != 3 {
		t.Fatalf("replacements = %d，期望 3", got)
	}
	if !strings.Contains(resp.Content, "已替换 3 处") {
		t.Fatalf("摘要 = %q", resp.Content)
	}
}

func TestFileEditEmptyNewStringDeletes(t *testing.T) {
	tools, dir := setup(t, nil)
	target := filepath.Join(dir, "del.txt")
	writeRaw(t, target, "keep\nremove me\nkeep2\n")

	ed := toolByName(t, tools, "file_edit")
	resp := exec(t, ed, "c1", map[string]any{"path": target, "old_string": "remove me\n", "new_string": ""})
	if !resp.Success {
		t.Fatalf("删除片段失败: %+v", resp)
	}
	if got := readRaw(t, target); got != "keep\nkeep2\n" {
		t.Fatalf("内容 = %q", got)
	}
	if got := metaInt(t, resp, "bytes_after"); got != len("keep\nkeep2\n") {
		t.Fatalf("bytes_after = %d", got)
	}
}

func TestFileEditRejectsEmptyOldString(t *testing.T) {
	tools, dir := setup(t, nil)
	target := filepath.Join(dir, "x.txt")
	writeRaw(t, target, "abc\n")

	ed := toolByName(t, tools, "file_edit")
	resp := exec(t, ed, "c1", map[string]any{"path": target, "old_string": "", "new_string": "y"})
	if resp.Success {
		t.Fatalf("空 old_string 应被拒绝: %+v", resp)
	}
	if !strings.Contains(resp.Error, "old_string 不能为空") {
		t.Fatalf("错误信息 = %q", resp.Error)
	}
	if got := readRaw(t, target); got != "abc\n" {
		t.Fatalf("文件不应被改动: %q", got)
	}
}

func TestFileEditRequiresNewStringKey(t *testing.T) {
	tools, dir := setup(t, nil)
	target := filepath.Join(dir, "y.txt")
	writeRaw(t, target, "abc\n")

	ed := toolByName(t, tools, "file_edit")
	// 漏传 new_string 与「显式传空串（删除）」语义不同：前者是漏参，不能当成删除执行。
	resp := exec(t, ed, "c1", map[string]any{"path": target, "old_string": "abc"})
	if resp.Success {
		t.Fatalf("缺少 new_string 应被拒绝: %+v", resp)
	}
	if !strings.Contains(resp.Error, "new_string") {
		t.Fatalf("错误信息 = %q", resp.Error)
	}
	if got := readRaw(t, target); got != "abc\n" {
		t.Fatalf("文件不应被改动: %q", got)
	}
}

func TestFileEditRejectsMissingPathDirAndBinary(t *testing.T) {
	tools, dir := setup(t, nil)
	ed := toolByName(t, tools, "file_edit")

	// 不存在的路径：不创建新文件，明确引导到 file_write。
	missing := filepath.Join(dir, "nope.txt")
	resp := exec(t, ed, "c1", map[string]any{"path": missing, "old_string": "a", "new_string": "b"})
	if resp.Success || !strings.Contains(resp.Error, "文件不存在") {
		t.Fatalf("不存在的文件应报「文件不存在」: %+v", resp)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("编辑不得创建新文件")
	}

	// 目录
	resp = exec(t, ed, "c2", map[string]any{"path": dir, "old_string": "a", "new_string": "b"})
	if resp.Success || !strings.Contains(resp.Error, "目录") {
		t.Fatalf("目录应被拒绝: %+v", resp)
	}

	// 二进制（含 NUL 字节）
	bin := filepath.Join(dir, "bin.dat")
	writeRaw(t, bin, "abc\x00def")
	resp = exec(t, ed, "c3", map[string]any{"path": bin, "old_string": "abc", "new_string": "xyz"})
	if resp.Success || !strings.Contains(resp.Error, "二进制") {
		t.Fatalf("二进制文件应被拒绝: %+v", resp)
	}
	if got := readRaw(t, bin); got != "abc\x00def" {
		t.Fatalf("二进制文件不应被改动: %q", got)
	}
}

func TestFileEditHonoursWriteRoots(t *testing.T) {
	allowed := t.TempDir()
	tools, _ := setup(t, []string{allowed})
	outside := t.TempDir()
	target := filepath.Join(outside, "o.txt")
	writeRaw(t, target, "abc\n")

	ed := toolByName(t, tools, "file_edit")
	resp := exec(t, ed, "c1", map[string]any{"path": target, "old_string": "abc", "new_string": "xyz"})
	if resp.Success || !strings.Contains(resp.Error, "允许写入") {
		t.Fatalf("白名单外编辑应被 Guard 拒绝: %+v", resp)
	}
	if got := readRaw(t, target); got != "abc\n" {
		t.Fatalf("白名单外文件不应被改动: %q", got)
	}

	// 白名单内正常
	inside := filepath.Join(allowed, "i.txt")
	writeRaw(t, inside, "abc\n")
	resp = exec(t, ed, "c2", map[string]any{"path": inside, "old_string": "abc", "new_string": "xyz"})
	if !resp.Success {
		t.Fatalf("白名单内编辑应成功: %+v", resp)
	}
	if got := readRaw(t, inside); got != "xyz\n" {
		t.Fatalf("白名单内内容 = %q", got)
	}
}

func TestMultiEditAppliesAllEdits(t *testing.T) {
	tools, dir := setup(t, nil)
	target := filepath.Join(dir, "m.txt")
	writeRaw(t, target, "k1\nk2\nz\nend\n")

	me := toolByName(t, tools, "multi_edit")
	resp := exec(t, me, "c1", map[string]any{
		"path": target,
		"edits": []any{
			map[string]any{"old_string": "k", "new_string": "K", "replace_all": true},
			map[string]any{"old_string": "z", "new_string": "Z"},
		},
	})
	if !resp.Success {
		t.Fatalf("multi_edit 失败: %+v", resp)
	}
	if got := readRaw(t, target); got != "K1\nK2\nZ\nend\n" {
		t.Fatalf("内容 = %q", got)
	}
	if got := metaInt(t, resp, "edits"); got != 2 {
		t.Fatalf("edits = %d，期望 2", got)
	}
	if got := metaInt(t, resp, "replacements"); got != 3 {
		t.Fatalf("replacements = %d，期望 3", got)
	}
	if got, want := metaInt(t, resp, "bytes_before"), len("k1\nk2\nz\nend\n"); got != want {
		t.Fatalf("bytes_before = %d，期望 %d", got, want)
	}
	if got, want := metaInt(t, resp, "bytes_after"), len("K1\nK2\nZ\nend\n"); got != want {
		t.Fatalf("bytes_after = %d，期望 %d", got, want)
	}
	if got := metaString(t, resp, "path"); got != target {
		t.Fatalf("metadata.path = %q", got)
	}
}

func TestMultiEditAtomicOnValidationFailure(t *testing.T) {
	tools, dir := setup(t, nil)
	target := filepath.Join(dir, "atomic.txt")
	original := "a\nb\n"
	writeRaw(t, target, original)

	me := toolByName(t, tools, "multi_edit")
	resp := exec(t, me, "c1", map[string]any{
		"path": target,
		"edits": []any{
			map[string]any{"old_string": "a", "new_string": "A"},   // 合法
			map[string]any{"old_string": "zzz", "new_string": "X"}, // 找不到
		},
	})
	if resp.Success {
		t.Fatalf("含非法编辑时不应成功: %+v", resp)
	}
	if !strings.Contains(resp.Error, "第 2 条编辑") {
		t.Fatalf("错误信息应指出是第几条编辑: %q", resp.Error)
	}
	if !strings.Contains(resp.Error, "未找到") {
		t.Fatalf("错误信息应说明原因: %q", resp.Error)
	}
	// 原子性：第一条编辑绝不能已经落盘。
	if got := readRaw(t, target); got != original {
		t.Fatalf("校验失败时文件必须保持原样，实际 %q", got)
	}
}

func TestMultiEditValidatesAgainstOriginalContent(t *testing.T) {
	tools, dir := setup(t, nil)
	target := filepath.Join(dir, "chain.txt")
	original := "A\n"
	writeRaw(t, target, original)

	me := toolByName(t, tools, "multi_edit")
	// 第二条编辑针对的是「假设第一条已应用」后的内容：不允许，必须在原文上匹配。
	resp := exec(t, me, "c1", map[string]any{
		"path": target,
		"edits": []any{
			map[string]any{"old_string": "A", "new_string": "B"},
			map[string]any{"old_string": "B", "new_string": "C"},
		},
	})
	if resp.Success {
		t.Fatalf("编辑之间不得级联，应失败: %+v", resp)
	}
	if !strings.Contains(resp.Error, "第 2 条编辑") || !strings.Contains(resp.Error, "未找到") {
		t.Fatalf("错误信息 = %q", resp.Error)
	}
	if got := readRaw(t, target); got != original {
		t.Fatalf("文件必须保持原样，实际 %q", got)
	}
}

func TestMultiEditRejectsOverlappingRanges(t *testing.T) {
	tools, dir := setup(t, nil)
	target := filepath.Join(dir, "overlap.txt")
	original := "hello world\n"
	writeRaw(t, target, original)

	me := toolByName(t, tools, "multi_edit")
	resp := exec(t, me, "c1", map[string]any{
		"path": target,
		"edits": []any{
			map[string]any{"old_string": "hello world", "new_string": "X"},
			map[string]any{"old_string": "world", "new_string": "Y"},
		},
	})
	if resp.Success {
		t.Fatalf("区间重叠时应失败: %+v", resp)
	}
	if !strings.Contains(resp.Error, "重叠") {
		t.Fatalf("错误信息应说明重叠: %q", resp.Error)
	}
	if !strings.Contains(resp.Error, "第 2 条编辑") {
		t.Fatalf("错误信息应指出冲突的编辑序号: %q", resp.Error)
	}
	if got := readRaw(t, target); got != original {
		t.Fatalf("文件必须保持原样，实际 %q", got)
	}
}

func TestMultiEditRejectsDuplicateMatchingEdits(t *testing.T) {
	tools, dir := setup(t, nil)
	target := filepath.Join(dir, "dup-edit.txt")
	original := "same\n"
	writeRaw(t, target, original)

	me := toolByName(t, tools, "multi_edit")
	// 两条编辑指向同一段字节：即使都「唯一命中」，应用顺序也不确定。
	resp := exec(t, me, "c1", map[string]any{
		"path": target,
		"edits": []any{
			map[string]any{"old_string": "same", "new_string": "one"},
			map[string]any{"old_string": "same", "new_string": "two"},
		},
	})
	if resp.Success || !strings.Contains(resp.Error, "重叠") {
		t.Fatalf("同一区间的两条编辑应被拒绝: %+v", resp)
	}
	if got := readRaw(t, target); got != original {
		t.Fatalf("文件必须保持原样，实际 %q", got)
	}
}

func TestMultiEditRejectsAmbiguousMatchWithoutReplaceAll(t *testing.T) {
	tools, dir := setup(t, nil)
	target := filepath.Join(dir, "amb.txt")
	writeRaw(t, target, "x\nx\n")

	me := toolByName(t, tools, "multi_edit")
	resp := exec(t, me, "c1", map[string]any{
		"path": target,
		"edits": []any{
			map[string]any{"old_string": "x", "new_string": "y"},
		},
	})
	if resp.Success {
		t.Fatalf("多处匹配未开 replace_all 时应失败: %+v", resp)
	}
	if !strings.Contains(resp.Error, "找到 2 处") {
		t.Fatalf("错误信息应报出匹配数量: %q", resp.Error)
	}
}

func TestMultiEditRejectsBadArgumentShapes(t *testing.T) {
	tools, dir := setup(t, nil)
	target := filepath.Join(dir, "shape.txt")
	original := "abc\n"
	writeRaw(t, target, original)
	me := toolByName(t, tools, "multi_edit")

	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"缺少 edits", map[string]any{"path": target}, "edits"},
		{"edits 非数组", map[string]any{"path": target, "edits": "abc"}, "数组"},
		{"edits 为空", map[string]any{"path": target, "edits": []any{}}, "不能为空"},
		{"编辑非对象", map[string]any{"path": target, "edits": []any{"abc"}}, "必须是对象"},
		{"old_string 为空", map[string]any{"path": target, "edits": []any{map[string]any{"old_string": "", "new_string": "x"}}}, "old_string"},
		{"缺少 new_string", map[string]any{"path": target, "edits": []any{map[string]any{"old_string": "abc"}}}, "new_string"},
	}
	for _, tc := range cases {
		resp := exec(t, me, "c", tc.args)
		if resp.Success {
			t.Fatalf("%s：不应成功 %+v", tc.name, resp)
		}
		if !strings.Contains(resp.Error, tc.want) {
			t.Fatalf("%s：错误信息 %q 未包含 %q", tc.name, resp.Error, tc.want)
		}
		if got := readRaw(t, target); got != original {
			t.Fatalf("%s：文件必须保持原样，实际 %q", tc.name, got)
		}
	}
}

func TestMultiEditRejectsMissingFile(t *testing.T) {
	tools, dir := setup(t, nil)
	me := toolByName(t, tools, "multi_edit")
	missing := filepath.Join(dir, "nope.txt")
	resp := exec(t, me, "c1", map[string]any{
		"path":  missing,
		"edits": []any{map[string]any{"old_string": "a", "new_string": "b"}},
	})
	if resp.Success || !strings.Contains(resp.Error, "文件不存在") {
		t.Fatalf("不存在的文件应被拒绝: %+v", resp)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("multi_edit 不得创建新文件")
	}
}

func TestEditToolSchemas(t *testing.T) {
	tools, _ := setup(t, nil)

	ed := toolByName(t, tools, "file_edit")
	def := ed.Definition()
	if def.Name != "file_edit" || def.Risk != tool.RiskMedium || def.Idempotency != tool.ClassDetectable || def.Domain != tool.DomainInProcess {
		t.Fatalf("file_edit 定义不匹配: %+v", def)
	}
	// 空 new_string 是合法的删除语义，schema 必须放行。
	if err := tool.ValidateArguments(def.Parameters, map[string]any{"path": "x.txt", "old_string": "a", "new_string": ""}); err != nil {
		t.Fatalf("合法参数应通过 schema：%v", err)
	}
	if err := tool.ValidateArguments(def.Parameters, map[string]any{"path": "x.txt", "old_string": "a"}); err == nil {
		t.Fatalf("缺少 new_string 应被 schema 拒绝")
	}

	me := toolByName(t, tools, "multi_edit")
	def = me.Definition()
	if def.Name != "multi_edit" || def.Risk != tool.RiskMedium || def.Idempotency != tool.ClassDetectable || def.Domain != tool.DomainInProcess {
		t.Fatalf("multi_edit 定义不匹配: %+v", def)
	}
	valid := map[string]any{
		"path":  "x.txt",
		"edits": []any{map[string]any{"old_string": "a", "new_string": "b", "replace_all": true}},
	}
	if err := tool.ValidateArguments(def.Parameters, valid); err != nil {
		t.Fatalf("合法参数应通过 schema：%v", err)
	}
	missingNew := map[string]any{
		"path":  "x.txt",
		"edits": []any{map[string]any{"old_string": "a"}},
	}
	if err := tool.ValidateArguments(def.Parameters, missingNew); err == nil {
		t.Fatalf("编辑缺少 new_string 应被 schema 拒绝")
	}
	wrongType := map[string]any{
		"path":  "x.txt",
		"edits": []any{map[string]any{"old_string": "a", "new_string": 1}},
	}
	if err := tool.ValidateArguments(def.Parameters, wrongType); err == nil {
		t.Fatalf("new_string 类型错误应被 schema 拒绝")
	}
}
