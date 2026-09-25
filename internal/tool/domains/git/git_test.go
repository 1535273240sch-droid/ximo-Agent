package git

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/tool"
	"github.com/ximo888ok-netizen/ximo-agent/internal/tool/domains"
)

// ---------------------------------------------------------------------------
// 合成 .git 仓库的测试辅助
// ---------------------------------------------------------------------------

func hashObject(t *testing.T, gitDir, typ string, content []byte) string {
	t.Helper()
	full := append([]byte(typ+" "+fmt.Sprint(len(content))+"\x00"), content...)
	sum := sha1.Sum(full)
	sha := hex.EncodeToString(sum[:])
	dir := filepath.Join(gitDir, "objects", sha[:2])
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	if _, err := zw.Write(full); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sha[2:]), buf.Bytes(), 0o444); err != nil {
		t.Fatal(err)
	}
	return sha
}

func blob(t *testing.T, gitDir, content string) string {
	return hashObject(t, gitDir, "blob", []byte(content))
}

// testTreeEntry 描述一个 tree 条目（仅文件）。
type testTreeEntry struct {
	mode string
	name string
	sha  string
}

func writeTree(t *testing.T, gitDir string, entries []testTreeEntry) string {
	t.Helper()
	var buf bytes.Buffer
	for _, e := range entries {
		buf.WriteString(e.mode + " " + e.name)
		buf.WriteByte(0)
		raw, err := hex.DecodeString(e.sha)
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(raw)
	}
	return hashObject(t, gitDir, "tree", buf.Bytes())
}

func writeCommit(t *testing.T, gitDir, treeSHA, message string, parent string) string {
	t.Helper()
	when := time.Date(2026, 1, 15, 10, 30, 0, 0, time.UTC)
	ident := fmt.Sprintf("Test Author <test@example.com> %d +0000", when.Unix())
	var b strings.Builder
	fmt.Fprintf(&b, "tree %s\n", treeSHA)
	if parent != "" {
		fmt.Fprintf(&b, "parent %s\n", parent)
	}
	fmt.Fprintf(&b, "author %s\n", ident)
	fmt.Fprintf(&b, "committer %s\n", ident)
	fmt.Fprintf(&b, "\n%s\n", message)
	return hashObject(t, gitDir, "commit", []byte(b.String()))
}

// writeIndex 写入 .git/index（v2）。
func writeIndex(t *testing.T, gitDir string, files map[string]struct{ sha, content string }) {
	t.Helper()
	var buf bytes.Buffer
	buf.WriteString("DIRC")
	binary.Write(&buf, binary.BigEndian, uint32(2))
	binary.Write(&buf, binary.BigEndian, uint32(len(files)))
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sortStrings(names)
	for _, name := range names {
		f := files[name]
		raw, _ := hex.DecodeString(f.sha)
		entry := make([]byte, 62)
		binary.BigEndian.PutUint32(entry[24:28], 0o100644) // mode
		binary.BigEndian.PutUint32(entry[36:40], uint32(len(f.content)))
		copy(entry[40:60], raw)
		binary.BigEndian.PutUint16(entry[60:62], uint16(len(name)))
		buf.Write(entry)
		buf.WriteString(name)
		buf.WriteByte(0)
		for (buf.Len()-12)%8 != 0 {
			buf.WriteByte(0)
		}
	}
	if err := os.WriteFile(filepath.Join(gitDir, "index"), buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// buildRepo 构造一个仓库：两个提交、一个分支、index 与工作区。
func buildRepo(t *testing.T) (workTree, gitDir, commit2 string) {
	t.Helper()
	workTree = t.TempDir()
	gitDir = filepath.Join(workTree, ".git")
	for _, dir := range []string{
		filepath.Join(gitDir, "objects"),
		filepath.Join(gitDir, "refs", "heads"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// 提交 1：a.txt = "one\n"
	a1 := blob(t, gitDir, "one\n")
	tree1 := writeTree(t, gitDir, []testTreeEntry{{mode: "100644", name: "a.txt", sha: a1}})
	commit1 := writeCommit(t, gitDir, tree1, "initial commit", "")

	// 提交 2：a.txt = "one\ntwo\n"，新增 b.txt
	a2 := blob(t, gitDir, "one\ntwo\n")
	b2 := blob(t, gitDir, "bee\n")
	tree2 := writeTree(t, gitDir, []testTreeEntry{
		{mode: "100644", name: "a.txt", sha: a2},
		{mode: "100644", name: "b.txt", sha: b2},
	})
	commit2 = writeCommit(t, gitDir, tree2, "second commit", commit1)

	// HEAD -> refs/heads/main -> commit2
	if err := os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "refs", "heads", "main"), []byte(commit2+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 另一个分支指向 commit1
	if err := os.WriteFile(filepath.Join(gitDir, "refs", "heads", "dev"), []byte(commit1+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// index 与工作区：a.txt 已修改，c.txt 未跟踪，b.txt 与 index 一致
	writeIndex(t, gitDir, map[string]struct{ sha, content string }{
		"a.txt": {sha: a2, content: "one\ntwo\n"},
		"b.txt": {sha: b2, content: "bee\n"},
	})
	if err := os.WriteFile(filepath.Join(workTree, "a.txt"), []byte("one\ntwo\nMODIFIED\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workTree, "b.txt"), []byte("bee\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workTree, "c.txt"), []byte("untracked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return workTree, gitDir, commit2
}

func execTool(t *testing.T, tl tool.Tool, args map[string]any) tool.ToolResponse {
	t.Helper()
	return tl.Execute(context.Background(), tool.ToolRequest{
		RunID: "r", ToolCallID: "c", Name: tl.Definition().Name, Arguments: args,
	})
}

func toolOf(t *testing.T, tools []tool.Tool, name string) tool.Tool {
	t.Helper()
	for _, tl := range tools {
		if tl.Definition().Name == name {
			return tl
		}
	}
	t.Fatalf("工具 %q 不存在", name)
	return nil
}

// ---------------------------------------------------------------------------
// 测试
// ---------------------------------------------------------------------------

func TestGitStatus(t *testing.T) {
	workTree, _, _ := buildRepo(t)
	repo, err := Discover(workTree)
	if err != nil {
		t.Fatal(err)
	}
	status, err := repo.Status()
	if err != nil {
		t.Fatal(err)
	}
	if status.Branch != "main" {
		t.Fatalf("分支 = %q，期望 main", status.Branch)
	}
	if status.Commit == "" {
		t.Fatalf("HEAD commit 为空")
	}
	// a.txt 已修改
	if len(status.Modified) != 1 || !strings.Contains(status.Modified[0], "a.txt") {
		t.Fatalf("Modified = %v，期望 a.txt", status.Modified)
	}
	// c.txt 未跟踪
	if len(status.Untracked) != 1 || !strings.Contains(status.Untracked[0], "c.txt") {
		t.Fatalf("Untracked = %v，期望 c.txt", status.Untracked)
	}
	// staged：index 与 HEAD 一致（a/b 都在 tree2 里且 sha 相同）-> 无 staged
	if len(status.Staged) != 0 {
		t.Fatalf("Staged = %v，期望空", status.Staged)
	}
}

func TestGitStatusDetectsStagedAndDeleted(t *testing.T) {
	workTree, gitDir, commit2 := buildRepo(t)
	// 暂存一个新文件（index 里有，HEAD 没有）
	newBlob := blob(t, gitDir, "staged content\n")
	writeIndex(t, gitDir, map[string]struct{ sha, content string }{
		"a.txt":     {sha: blobSHAStr("one\ntwo\n"), content: "one\ntwo\n"},
		"b.txt":     {sha: blobSHAStr("bee\n"), content: "bee\n"},
		"added.txt": {sha: newBlob, content: "staged content\n"},
	})
	// 删除工作区的 b.txt（index 有、工作区没有）
	if err := os.Remove(filepath.Join(workTree, "b.txt")); err != nil {
		t.Fatal(err)
	}
	_ = commit2

	repo, err := Discover(workTree)
	if err != nil {
		t.Fatal(err)
	}
	status, err := repo.Status()
	if err != nil {
		t.Fatal(err)
	}
	foundStagedAdd := false
	for _, s := range status.Staged {
		if strings.Contains(s, "added.txt") && strings.HasPrefix(s, "A") {
			foundStagedAdd = true
		}
	}
	if !foundStagedAdd {
		t.Fatalf("Staged 应包含 added.txt：%v", status.Staged)
	}
	foundDeleted := false
	for _, s := range status.Deleted {
		if strings.Contains(s, "b.txt") {
			foundDeleted = true
		}
	}
	if !foundDeleted {
		t.Fatalf("Deleted 应包含 b.txt：%v", status.Deleted)
	}
}

func blobSHAStr(content string) string {
	data := []byte(content)
	full := append([]byte(fmt.Sprintf("blob %d\x00", len(data))), data...)
	sum := sha1.Sum(full)
	return hex.EncodeToString(sum[:])
}

func TestGitLog(t *testing.T) {
	workTree, _, commit2 := buildRepo(t)
	repo, err := Discover(workTree)
	if err != nil {
		t.Fatal(err)
	}
	commits, err := repo.Log(commit2, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(commits) != 2 {
		t.Fatalf("提交数 = %d，期望 2", len(commits))
	}
	if commits[0].Subject != "second commit" {
		t.Fatalf("最新提交主题 = %q", commits[0].Subject)
	}
	if commits[1].Subject != "initial commit" {
		t.Fatalf("次新提交主题 = %q", commits[1].Subject)
	}
	if commits[0].Author != "Test Author" {
		t.Fatalf("作者 = %q", commits[0].Author)
	}
	if commits[0].Date.Year() != 2026 {
		t.Fatalf("日期 = %v", commits[0].Date)
	}
}

func TestGitBranches(t *testing.T) {
	workTree, _, _ := buildRepo(t)
	repo, err := Discover(workTree)
	if err != nil {
		t.Fatal(err)
	}
	branches, err := repo.Branches()
	if err != nil {
		t.Fatal(err)
	}
	if len(branches) != 2 {
		t.Fatalf("分支数 = %d，期望 2", len(branches))
	}
	var current string
	for _, b := range branches {
		if b.Current {
			current = b.Name
		}
	}
	if current != "main" {
		t.Fatalf("当前分支 = %q，期望 main", current)
	}
}

func TestGitToolsEndToEnd(t *testing.T) {
	workTree, _, _ := buildRepo(t)
	tools := Tools(domains.NewGuard(nil))

	status := execTool(t, toolOf(t, tools, "git_status"), map[string]any{"repoPath": workTree})
	if !status.Success || !strings.Contains(status.Content, "main") {
		t.Fatalf("git_status = %+v", status)
	}
	if !strings.Contains(status.Content, "c.txt") {
		t.Fatalf("git_status 应包含未跟踪文件: %s", status.Content)
	}

	log := execTool(t, toolOf(t, tools, "git_log"), map[string]any{"repoPath": workTree, "count": 5})
	if !log.Success || !strings.Contains(log.Content, "second commit") {
		t.Fatalf("git_log = %+v", log)
	}

	branch := execTool(t, toolOf(t, tools, "git_branch"), map[string]any{"repoPath": workTree})
	if !branch.Success || !strings.Contains(branch.Content, "dev") {
		t.Fatalf("git_branch = %+v", branch)
	}

	diff := execTool(t, toolOf(t, tools, "git_diff"), map[string]any{"repoPath": workTree})
	if !diff.Success || !strings.Contains(diff.Content, "MODIFIED") {
		t.Fatalf("git_diff = %s", diff.Content)
	}
}

func TestGitDiscoverFromSubdirectory(t *testing.T) {
	workTree, _, _ := buildRepo(t)
	sub := filepath.Join(workTree, "deep", "nested")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	repo, err := Discover(sub)
	if err != nil {
		t.Fatalf("从子目录发现仓库失败: %v", err)
	}
	if repo.WorkTree() != workTree {
		t.Fatalf("工作区 = %q，期望 %q", repo.WorkTree(), workTree)
	}
}

func TestGitNotARepo(t *testing.T) {
	if _, err := Discover(t.TempDir()); err == nil {
		t.Fatalf("非仓库目录应当报错")
	}
	tools := Tools(domains.NewGuard(nil))
	resp := execTool(t, toolOf(t, tools, "git_status"), map[string]any{"repoPath": t.TempDir()})
	if resp.Success {
		t.Fatalf("非仓库应当返回失败")
	}
}

// ---------------------------------------------------------------------------
// packfile 读取（含 ofs-delta）
// ---------------------------------------------------------------------------

func writePackVarint(buf *bytes.Buffer, typ byte, size int) {
	b := byte(typ<<4) | byte(size&0x0f)
	size >>= 4
	if size > 0 {
		b |= 0x80
	}
	buf.WriteByte(b)
	for size > 0 {
		b := byte(size & 0x7f)
		size >>= 7
		if size > 0 {
			b |= 0x80
		}
		buf.WriteByte(b)
	}
}

func writeOfsDeltaOffset(buf *bytes.Buffer, offset uint64) {
	// git 的负偏移编码：每字节 7 位，延续时先 +1
	var bytesBuf [16]byte
	n := 0
	bytesBuf[n] = byte(offset & 0x7f)
	for offset >>= 7; offset > 0; offset >>= 7 {
		offset--
		n++
		bytesBuf[n] = byte(offset & 0x7f)
	}
	for i := n; i >= 0; i-- {
		b := bytesBuf[i]
		if i > 0 {
			b |= 0x80
		}
		buf.WriteByte(b)
	}
}

func zlibCompress(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestPackfileReadWithOfsDelta(t *testing.T) {
	workTree, gitDir, _ := buildRepo(t)
	packDir := filepath.Join(gitDir, "objects", "pack")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatal(err)
	}

	baseContent := []byte("hello world\n")
	// delta：copy[0,6) + insert "WORLD" + copy[11,1) -> "hello WORLD\n"
	delta := []byte{
		12, 12, // baseSize, resultSize
		0x90, 0x06, // copy offset=0 size=6 ("hello ")
		0x05, 'W', 'O', 'R', 'L', 'D',
		0x91, 0x0B, 0x01, // copy offset=11 size=1 ("\n")
	}

	var pack bytes.Buffer
	pack.WriteString("PACK")
	binary.Write(&pack, binary.BigEndian, uint32(2))
	binary.Write(&pack, binary.BigEndian, uint32(2))

	baseOffset := pack.Len()
	writePackVarint(&pack, objBlob, len(baseContent))
	pack.Write(zlibCompress(t, baseContent))

	deltaOffset := pack.Len()
	writePackVarint(&pack, objOfsDelta, len(delta))
	writeOfsDeltaOffset(&pack, uint64(deltaOffset-baseOffset))
	pack.Write(zlibCompress(t, delta))

	packBytes := pack.Bytes()

	// 计算两个对象的 sha（git 对象 sha，与 pack 内编码无关）
	baseSHA := blobSHAStr(string(baseContent))
	resultContent := "hello WORLD\n"
	deltaSHA := blobSHAStr(resultContent)

	// 写 .idx v2
	shas := []string{baseSHA, deltaSHA}
	offsets := []uint64{uint64(baseOffset), uint64(deltaOffset)}
	// fanout
	var idx bytes.Buffer
	idx.Write([]byte{0xff, 0x74, 0x4f, 0x63})
	binary.Write(&idx, binary.BigEndian, uint32(2))
	var fanout [256]uint32
	for _, sha := range shas {
		raw, _ := hex.DecodeString(sha)
		fanout[raw[0]]++
	}
	var cum uint32
	for i := 0; i < 256; i++ {
		cum += fanout[i]
		binary.Write(&idx, binary.BigEndian, cum)
	}
	// sha 需排序写入
	order := []int{0, 1}
	for i := 0; i < len(order); i++ {
		for j := i + 1; j < len(order); j++ {
			if shas[order[j]] < shas[order[i]] {
				order[i], order[j] = order[j], order[i]
			}
		}
	}
	for _, i := range order {
		raw, _ := hex.DecodeString(shas[i])
		idx.Write(raw)
	}
	for range order {
		binary.Write(&idx, binary.BigEndian, uint32(0)) // crc 占位
	}
	for _, i := range order {
		binary.Write(&idx, binary.BigEndian, uint32(offsets[i]))
	}
	idx.Write(make([]byte, 20)) // pack checksum 占位
	idx.Write(make([]byte, 20)) // idx checksum 占位

	if err := os.WriteFile(filepath.Join(packDir, "pack-test.pack"), packBytes, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packDir, "pack-test.idx"), idx.Bytes(), 0o444); err != nil {
		t.Fatal(err)
	}

	// 重新打开仓库，从 pack 读取两个对象
	store := newObjectStore(gitDir)
	defer store.Close()
	obj, err := store.read(baseSHA)
	if err != nil {
		t.Fatalf("读取 pack 基对象失败: %v", err)
	}
	if obj.Type != "blob" || string(obj.Data) != string(baseContent) {
		t.Fatalf("基对象 = %+v", obj)
	}
	obj, err = store.read(deltaSHA)
	if err != nil {
		t.Fatalf("读取 pack delta 对象失败: %v", err)
	}
	if obj.Type != "blob" || string(obj.Data) != resultContent {
		t.Fatalf("delta 对象 = %q，期望 %q", obj.Data, resultContent)
	}
	_ = workTree
}

func TestApplyPackDelta(t *testing.T) {
	base := gitObject{Type: "blob", Data: []byte("hello world\n")}
	delta := []byte{
		12, 12,
		0x90, 0x06,
		0x05, 'W', 'O', 'R', 'L', 'D',
		0x91, 0x0B, 0x01,
	}
	out, err := applyPackDelta(base, delta)
	if err != nil {
		t.Fatal(err)
	}
	if string(out.Data) != "hello WORLD\n" {
		t.Fatalf("delta 应用结果 = %q", out.Data)
	}
	// 基大小不匹配必须报错
	if _, err := applyPackDelta(gitObject{Data: []byte("short")}, delta); err == nil {
		t.Fatalf("基大小不匹配应当报错")
	}
}

func TestParseIndexRoundTrip(t *testing.T) {
	gitDir := filepath.Join(t.TempDir(), ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]struct{ sha, content string }{
		"a.txt":     {sha: blobSHAStr("one\n"), content: "one\n"},
		"dir/b.txt": {sha: blobSHAStr("two\n"), content: "two\n"},
		"very/long/path/that/exceeds/twelve/chars/c.txt": {sha: blobSHAStr("three\n"), content: "three\n"},
	}
	writeIndex(t, gitDir, files)
	entries, err := parseIndexMust(t, gitDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("条目数 = %d，期望 3", len(entries))
	}
	byName := map[string]string{}
	for _, e := range entries {
		byName[e.path] = hex.EncodeToString(e.sha[:])
	}
	for name, f := range files {
		if byName[name] != f.sha {
			t.Fatalf("条目 %q sha = %q，期望 %q", name, byName[name], f.sha)
		}
	}
}

func parseIndexMust(t *testing.T, gitDir string) ([]indexEntry, error) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(gitDir, "index"))
	if err != nil {
		return nil, err
	}
	return parseIndex(data)
}

func TestUnifiedDiff(t *testing.T) {
	old := "line1\nline2\nline3\n"
	new := "line1\nline2 changed\nline3\n"
	lines := unifiedDiff("f.txt", old, new)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "-line2") || !strings.Contains(joined, "+line2 changed") {
		t.Fatalf("diff 不包含变更行:\n%s", joined)
	}
	if !strings.Contains(joined, " line1") || !strings.Contains(joined, " line3") {
		t.Fatalf("diff 应包含上下文行:\n%s", joined)
	}
	// 相同内容无 diff
	if got := unifiedDiff("f.txt", old, old); len(got) != 0 {
		t.Fatalf("相同内容应无 diff: %v", got)
	}
}
