package git

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Repo 是一个 git 仓库的只读视图（纯 Go 解析 .git 目录，不调用 git 二进制）。
type Repo struct {
	gitDir   string
	workTree string
	store    *objectStore
}

// Discover 从 path 向上查找 .git 目录并打开仓库。
func Discover(path string) (*Repo, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	dir := abs
	for {
		gitDir := filepath.Join(dir, ".git")
		if info, err := os.Stat(gitDir); err == nil && info.IsDir() {
			return &Repo{
				gitDir:   gitDir,
				workTree: dir,
				store:    newObjectStore(gitDir),
			}, nil
		}
		// 支持 .git 文件（worktree/submodule 场景）。
		if data, err := os.ReadFile(gitDir); err == nil && strings.HasPrefix(string(data), "gitdir:") {
			realGitDir := strings.TrimSpace(strings.TrimPrefix(string(data), "gitdir:"))
			if !filepath.IsAbs(realGitDir) {
				realGitDir = filepath.Join(dir, realGitDir)
			}
			return &Repo{
				gitDir:   realGitDir,
				workTree: dir,
				store:    newObjectStore(realGitDir),
			}, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil, fmt.Errorf("%q 不在 git 仓库内", abs)
		}
		dir = parent
	}
}

// WorkTree 返回工作区根目录。
func (r *Repo) WorkTree() string { return r.workTree }

// Head 返回当前分支名（detached 时为空）与 HEAD commit sha。
func (r *Repo) Head() (branch, commit string, err error) {
	data, err := os.ReadFile(filepath.Join(r.gitDir, "HEAD"))
	if err != nil {
		return "", "", err
	}
	content := strings.TrimSpace(string(data))
	if strings.HasPrefix(content, "ref:") {
		ref := strings.TrimSpace(strings.TrimPrefix(content, "ref:"))
		branch = strings.TrimPrefix(ref, "refs/heads/")
		sha, err := r.ResolveRef(ref)
		if err != nil {
			// 未诞生提交的仓库：HEAD 存在但 ref 不存在。
			return branch, "", nil
		}
		return branch, sha, nil
	}
	return "", strings.ToLower(content), nil
}

// ResolveRef 解析引用（refs/... 或完整 sha），依次查松散引用与 packed-refs。
func (r *Repo) ResolveRef(ref string) (string, error) {
	if len(ref) == 40 && isHex(ref) {
		return strings.ToLower(ref), nil
	}
	loose := filepath.Join(r.gitDir, filepath.FromSlash(ref))
	if data, err := os.ReadFile(loose); err == nil {
		sha := strings.TrimSpace(string(data))
		if len(sha) == 40 && isHex(sha) {
			return strings.ToLower(sha), nil
		}
	}
	packed, err := os.ReadFile(filepath.Join(r.gitDir, "packed-refs"))
	if err == nil {
		for _, line := range strings.Split(string(packed), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "^") {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) == 2 && fields[1] == ref {
				return strings.ToLower(fields[0]), nil
			}
		}
	}
	return "", fmt.Errorf("引用 %q 不存在", ref)
}

// Branch 本地分支。
type Branch struct {
	Name    string
	Current bool
}

// Branches 列出本地分支（refs/heads + packed-refs）。
func (r *Repo) Branches() ([]Branch, error) {
	current, _, err := r.Head()
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	var names []string
	headsDir := filepath.Join(r.gitDir, "refs", "heads")
	_ = filepath.WalkDir(headsDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(headsDir, path)
		if relErr != nil {
			return nil
		}
		name := filepath.ToSlash(rel)
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
		return nil
	})
	if data, err := os.ReadFile(filepath.Join(r.gitDir, "packed-refs")); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "^") {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) == 2 && strings.HasPrefix(fields[1], "refs/heads/") {
				name := strings.TrimPrefix(fields[1], "refs/heads/")
				if !seen[name] {
					seen[name] = true
					names = append(names, name)
				}
			}
		}
	}
	sort.Strings(names)
	branches := make([]Branch, 0, len(names))
	for _, name := range names {
		branches = append(branches, Branch{Name: name, Current: name == current})
	}
	return branches, nil
}

// Commit 提交概要。
type Commit struct {
	SHA     string
	Author  string
	Date    time.Time
	Subject string
}

// Log 从 fromSHA 沿首父提交遍历 n 条。
func (r *Repo) Log(fromSHA string, n int) ([]Commit, error) {
	if fromSHA == "" {
		return nil, nil
	}
	var commits []Commit
	sha := fromSHA
	for i := 0; i < n; i++ {
		obj, err := r.store.read(sha)
		if err != nil {
			if len(commits) == 0 {
				return nil, err
			}
			break
		}
		if obj.Type != "commit" {
			break
		}
		commits = append(commits, parseCommit(sha, obj.Data))
		parent := firstParent(obj.Data)
		if parent == "" {
			break
		}
		sha = parent
	}
	return commits, nil
}

func parseCommit(sha string, data []byte) Commit {
	c := Commit{SHA: sha}
	lines := strings.Split(string(data), "\n")
	inMessage := false
	var message []string
	for _, line := range lines {
		if inMessage {
			message = append(message, line)
			continue
		}
		if line == "" {
			inMessage = true
			continue
		}
		switch {
		case strings.HasPrefix(line, "author "):
			// author Name <email> 1234567890 +0800
			rest := strings.TrimPrefix(line, "author ")
			c.Author, c.Date = parseIdent(rest)
		}
	}
	c.Subject = strings.TrimSpace(strings.Split(strings.Join(message, "\n"), "\n")[0])
	return c
}

func firstParent(data []byte) string {
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			break
		}
		if strings.HasPrefix(line, "parent ") {
			sha := strings.TrimSpace(strings.TrimPrefix(line, "parent "))
			if len(sha) == 40 {
				return strings.ToLower(sha)
			}
		}
	}
	return ""
}

// parseIdent 解析 "Name <email> 1695459200 +0800"。
func parseIdent(s string) (name string, when time.Time) {
	lt := strings.LastIndex(s, "<")
	gt := strings.Index(s, ">")
	if lt < 0 || gt < 0 || gt < lt {
		return strings.TrimSpace(s), time.Time{}
	}
	name = strings.TrimSpace(s[:lt])
	rest := strings.Fields(strings.TrimSpace(s[gt+1:]))
	if len(rest) >= 1 {
		if secs, err := parseInt64(rest[0]); err == nil {
			when = time.Unix(secs, 0).UTC()
		}
	}
	return name, when
}

func parseInt64(s string) (int64, error) {
	var v int64
	if _, err := fmt.Sscanf(s, "%d", &v); err != nil {
		return 0, err
	}
	return v, nil
}

// ---------------------------------------------------------------------------
// 索引（.git/index）
// ---------------------------------------------------------------------------

type indexEntry struct {
	mode uint32
	size uint32
	sha  [20]byte
	path string
}

// parseIndex 解析 .git/index（v2/v3）。
func parseIndex(data []byte) ([]indexEntry, error) {
	if len(data) < 12 {
		return nil, fmt.Errorf("index 文件过小")
	}
	if !bytes.Equal(data[:4], []byte("DIRC")) {
		return nil, fmt.Errorf("index 魔法数非法")
	}
	version := binary.BigEndian.Uint32(data[4:8])
	if version < 2 || version > 3 {
		return nil, fmt.Errorf("仅支持 index v2/v3，收到 v%d", version)
	}
	count := int(binary.BigEndian.Uint32(data[8:12]))
	pos := 12
	entries := make([]indexEntry, 0, count)
	for i := 0; i < count; i++ {
		if pos+62 > len(data) {
			return nil, fmt.Errorf("index 条目 %d 越界", i)
		}
		mode := binary.BigEndian.Uint32(data[pos+24 : pos+28])
		size := binary.BigEndian.Uint32(data[pos+36 : pos+40])
		var sha [20]byte
		copy(sha[:], data[pos+40:pos+60])
		flags := binary.BigEndian.Uint16(data[pos+60 : pos+62])
		pos += 62
		nameLen := int(flags & 0x0fff)
		extended := flags&0x4000 != 0
		if extended && version >= 3 {
			pos += 2 // 扩展 flags
		}
		if nameLen < 0xfff {
			if pos+nameLen > len(data) {
				return nil, fmt.Errorf("index 条目 %d 路径越界", i)
			}
			path := string(data[pos : pos+nameLen])
			pos += nameLen
			// 条目以 NUL 结尾并填充到 8 字节边界
			pos++ // 跳过 NUL
			if pad := (pos - 12) % 8; pad != 0 {
				pos += 8 - pad
			}
			entries = append(entries, indexEntry{mode: mode, size: size, sha: sha, path: path})
		} else {
			// 长路径：读到 NUL 为止，再对齐
			end := bytes.IndexByte(data[pos:], 0)
			if end < 0 {
				return nil, fmt.Errorf("index 条目 %d 长路径未闭合", i)
			}
			path := string(data[pos : pos+end])
			pos += end + 1
			if pad := (pos - 12) % 8; pad != 0 {
				pos += 8 - pad
			}
			entries = append(entries, indexEntry{mode: mode, size: size, sha: sha, path: path})
		}
	}
	return entries, nil
}

func (r *Repo) readIndex() ([]indexEntry, error) {
	data, err := os.ReadFile(filepath.Join(r.gitDir, "index"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return parseIndex(data)
}

// indexEntryFor 返回 index 中某路径的条目。
func (r *Repo) indexEntryFor(path string) (indexEntry, bool) {
	entries, err := r.readIndex()
	if err != nil {
		return indexEntry{}, false
	}
	for _, e := range entries {
		if e.path == path {
			return e, true
		}
	}
	return indexEntry{}, false
}

// blobText 读取 blob 对象内容（解码失败时返回空串）。
func (r *Repo) blobText(sha [20]byte) string {
	obj, err := r.store.read(hex.EncodeToString(sha[:]))
	if err != nil || obj.Type != "blob" {
		return ""
	}
	return string(obj.Data)
}

// readWorktreeFile 读取工作区文件（限制 4MB）。
func (r *Repo) readWorktreeFile(path string) ([]byte, error) {
	clean := filepath.Clean(filepath.FromSlash(path))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("非法路径 %q", path)
	}
	return os.ReadFile(filepath.Join(r.workTree, clean))
}

// ---------------------------------------------------------------------------
// 树（tree 对象）
// ---------------------------------------------------------------------------

type treeEntry struct {
	mode string
	name string
	sha  [20]byte
}

// parseTree 解析 tree 对象数据："<mode> <name>\0<20-byte sha>" 重复。
func parseTree(data []byte) []treeEntry {
	var entries []treeEntry
	pos := 0
	for pos < len(data) {
		sp := bytes.IndexByte(data[pos:], ' ')
		if sp < 0 {
			break
		}
		mode := string(data[pos : pos+sp])
		pos += sp + 1
		nul := bytes.IndexByte(data[pos:], 0)
		if nul < 0 {
			break
		}
		name := string(data[pos : pos+nul])
		pos += nul + 1
		if pos+20 > len(data) {
			break
		}
		var sha [20]byte
		copy(sha[:], data[pos:pos+20])
		pos += 20
		entries = append(entries, treeEntry{mode: mode, name: name, sha: sha})
	}
	return entries
}

// treeFiles 递归展开 tree，返回 路径 -> blob sha（目录不包含在内）。
func (r *Repo) treeFiles(treeSHA string, prefix string, out map[string][20]byte, depth int) error {
	if depth > 32 {
		return fmt.Errorf("目录树深度超过 32")
	}
	obj, err := r.store.read(treeSHA)
	if err != nil {
		return err
	}
	if obj.Type != "tree" {
		return fmt.Errorf("对象 %s 不是 tree", treeSHA)
	}
	for _, e := range parseTree(obj.Data) {
		path := e.name
		if prefix != "" {
			path = prefix + "/" + e.name
		}
		switch e.mode {
		case "40000", "040000":
			if err := r.treeFiles(hex.EncodeToString(e.sha[:]), path, out, depth+1); err != nil {
				return err
			}
		case "160000":
			// gitlink（submodule）：跳过
		default:
			out[path] = e.sha
		}
	}
	return nil
}

// commitTree 返回 commit 的 tree sha。
func commitTree(data []byte) string {
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			break
		}
		if strings.HasPrefix(line, "tree ") {
			sha := strings.TrimSpace(strings.TrimPrefix(line, "tree "))
			if len(sha) == 40 {
				return strings.ToLower(sha)
			}
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// 状态（git status --porcelain 等价）
// ---------------------------------------------------------------------------

// Status 仓库状态。
type Status struct {
	Branch    string
	Commit    string
	Staged    []string // 已暂存（新增/修改/删除）
	Modified  []string // 工作区已修改
	Deleted   []string // 工作区已删除
	Untracked []string // 未跟踪
}

// Status 计算仓库状态（index vs HEAD tree vs 工作区）。
func (r *Repo) Status() (*Status, error) {
	branch, commit, err := r.Head()
	if err != nil {
		return nil, err
	}
	status := &Status{Branch: branch, Commit: commit}

	index, err := r.readIndex()
	if err != nil {
		return nil, err
	}
	indexMap := make(map[string]indexEntry, len(index))
	for _, e := range index {
		indexMap[e.path] = e
	}

	headFiles := make(map[string][20]byte)
	if commit != "" {
		obj, err := r.store.read(commit)
		if err == nil {
			if tree := commitTree(obj.Data); tree != "" {
				if err := r.treeFiles(tree, "", headFiles, 0); err != nil {
					return nil, err
				}
			}
		}
	}

	// staged：index 与 HEAD 的差异
	for _, e := range index {
		headSHA, inHead := headFiles[e.path]
		if !inHead {
			status.Staged = append(status.Staged, "A  "+e.path)
		} else if headSHA != e.sha {
			status.Staged = append(status.Staged, "M  "+e.path)
		}
	}
	// HEAD 中存在但 index 没有 → 暂存区删除
	for path := range headFiles {
		if _, ok := indexMap[path]; !ok {
			status.Staged = append(status.Staged, "D  "+path)
		}
	}
	sort.Strings(status.Staged)

	// 工作区扫描：与 index 对比
	seen := make(map[string]bool)
	err = filepath.WalkDir(r.workTree, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if path == r.workTree {
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, relErr := filepath.Rel(r.workTree, path)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		seen[rel] = true
		entry, tracked := indexMap[rel]
		if !tracked {
			status.Untracked = append(status.Untracked, "?? "+rel)
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		if blobSHA(data) != entry.sha {
			status.Modified = append(status.Modified, " M "+rel)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// index 中存在但工作区没有 → 已删除
	for path := range indexMap {
		if !seen[path] {
			status.Deleted = append(status.Deleted, " D "+path)
		}
	}
	sort.Strings(status.Modified)
	sort.Strings(status.Deleted)
	sort.Strings(status.Untracked)
	return status, nil
}

// blobSHA 计算文件内容的 git blob sha1（"blob <size>\0" + content）。
func blobSHA(data []byte) [20]byte {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(data))
	h.Write(data)
	var out [20]byte
	copy(out[:], h.Sum(nil))
	return out
}

func isHex(s string) bool {
	if len(s) == 0 {
		return false
	}
	for _, c := range strings.ToLower(s) {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}
