// Package file 提供文件系统轻量工具（file_read/file_list/file_search/
// file_write/file_delete），全部纯 Go 实现，经 Guard 做写入白名单与敏感
// 文件拦截。
package file

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ximo888ok-netizen/ximo-agent/internal/tool"
	"github.com/ximo888ok-netizen/ximo-agent/internal/tool/domains"
)

// Tools 返回文件域全部工具。
func Tools(guard *domains.Guard) []tool.Tool {
	if guard == nil {
		guard = domains.NewGuard(nil)
	}
	return []tool.Tool{
		&readTool{guard: guard},
		&listTool{guard: guard},
		&searchTool{guard: guard},
		&writeTool{guard: guard},
		&deleteTool{guard: guard},
	}
}

const (
	maxFileReadBytes = 8 << 20 // 单次读取 8MB
	maxSearchFiles   = 20000   // 搜索遍历文件数上限
	maxSearchHits    = 200     // 搜索结果条数上限
	defaultMaxLines  = 500
)

// ---------------------------------------------------------------------------
// file_read
// ---------------------------------------------------------------------------

type readTool struct{ guard *domains.Guard }

func (t *readTool) Definition() tool.ToolDefinition {
	return tool.ToolDefinition{
		Name: "file_read",
		Description: "读取本地文件内容。支持文本文件（utf-8）与 base64 编码输出。" +
			"支持 startLine/endLine 精准区段读取与 maxLines 截断，避免大文件整读。",
		Parameters: tool.ObjectSchema(map[string]tool.JSONSchema{
			"filePath":  {Type: "string", Description: "文件路径（绝对路径或相对工作目录）"},
			"encoding":  {Type: "string", Description: "输出编码", Enum: []any{"auto", "utf8", "base64"}, Default: "auto"},
			"startLine": {Type: "integer", Description: "起始行（从 1 开始）", Default: 1},
			"endLine":   {Type: "integer", Description: "结束行（含）；0 表示到末尾", Default: 0},
			"maxLines":  {Type: "integer", Description: "最多显示行数；0 表示不限制", Default: defaultMaxLines},
		}, "filePath"),
		Risk:        tool.RiskLow,
		Idempotency: tool.ClassIdempotent,
		Domain:      tool.DomainInProcess,
	}
}

func (t *readTool) Execute(ctx context.Context, req tool.ToolRequest) tool.ToolResponse {
	fail := func(format string, args ...any) tool.ToolResponse {
		return domains.Fail(req.ToolCallID, "file_read", format, args...)
	}
	path := domains.StringArg(req.Arguments, "filePath")
	if path == "" {
		return fail("缺少 filePath 参数")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return fail("路径解析失败: %v", err)
	}
	if err := t.guard.CheckSensitiveFile(abs); err != nil {
		return fail("%v", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return fail("文件不存在或不可访问: %v", err)
	}
	if info.IsDir() {
		return fail("%q 是目录，请使用 file_list", abs)
	}
	if info.Size() > maxFileReadBytes {
		return fail("文件过大（%.1f KB），超过 %d KB 限制", float64(info.Size())/1024, maxFileReadBytes>>10)
	}
	if err := ctx.Err(); err != nil {
		return fail("调用已取消: %v", err)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return fail("读取失败: %v", err)
	}

	encoding := domains.StringArgDefault(req.Arguments, "encoding", "auto")
	if encoding == "base64" {
		content := fmt.Sprintf("**文件**: `%s` (%.1f KB, base64)\n```\n%s\n```", abs, float64(len(data))/1024, base64Encode(data))
		return domains.Code(req.ToolCallID, "file_read", content)
	}

	text := string(data)
	lines := strings.Split(text, "\n")
	total := len(lines)
	start := domains.IntArg(req.Arguments, "startLine", 1)
	if start < 1 {
		start = 1
	}
	end := domains.IntArg(req.Arguments, "endLine", 0)
	maxLines := domains.IntArg(req.Arguments, "maxLines", defaultMaxLines)

	var display []string
	var notice string
	switch {
	case end > 0 || start > 1:
		if end <= 0 || end > total {
			end = total
		}
		if start > total {
			start = total
		}
		display = lines[start-1 : end]
		notice = fmt.Sprintf("\n(显示第 %d-%d 行，共 %d 行)", start, end, total)
	default:
		if maxLines > 0 && total > maxLines {
			display = lines[:maxLines]
			notice = fmt.Sprintf("\n...(仅显示前 %d 行，共 %d 行，可用 startLine/endLine 读取后续内容)", maxLines, total)
		} else {
			display = lines
		}
	}

	var b strings.Builder
	for i, line := range display {
		fmt.Fprintf(&b, "%4d | %s\n", start+i, line)
	}
	ext := strings.TrimPrefix(filepath.Ext(abs), ".")
	content := fmt.Sprintf("**文件**: `%s` (%d 行)\n```%s\n%s%s\n```", abs, total, ext, strings.TrimRight(b.String(), "\n"), notice)
	resp := domains.Code(req.ToolCallID, "file_read", content)
	resp.Metadata = map[string]any{"filePath": abs, "lines": total, "size": info.Size()}
	return resp
}

// ---------------------------------------------------------------------------
// file_list
// ---------------------------------------------------------------------------

type listTool struct{ guard *domains.Guard }

func (t *listTool) Definition() tool.ToolDefinition {
	return tool.ToolDefinition{
		Name:        "file_list",
		Description: "列出目录内容（树形，可控制深度）。默认跳过 .git/node_modules/dist 等噪声目录。",
		Parameters: tool.ObjectSchema(map[string]tool.JSONSchema{
			"path":       {Type: "string", Description: "目录路径", Default: "."},
			"maxDepth":   {Type: "integer", Description: "遍历深度", Default: 3},
			"maxEntries": {Type: "integer", Description: "条目数上限", Default: 500},
		}),
		Risk:        tool.RiskLow,
		Idempotency: tool.ClassIdempotent,
		Domain:      tool.DomainInProcess,
	}
}

func (t *listTool) Execute(ctx context.Context, req tool.ToolRequest) tool.ToolResponse {
	fail := func(format string, args ...any) tool.ToolResponse {
		return domains.Fail(req.ToolCallID, "file_list", format, args...)
	}
	path := domains.StringArgDefault(req.Arguments, "path", ".")
	abs, err := filepath.Abs(path)
	if err != nil {
		return fail("路径解析失败: %v", err)
	}
	if err := t.guard.CheckSensitiveFile(abs); err != nil {
		return fail("%v", err)
	}
	maxDepth := domains.IntArg(req.Arguments, "maxDepth", 3)
	maxEntries := domains.IntArg(req.Arguments, "maxEntries", 500)

	var b strings.Builder
	count := 0
	truncated := false
	var walk func(dir string, depth int) error
	walk = func(dir string, depth int) error {
		if count >= maxEntries {
			truncated = true
			return filepath.SkipAll
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil
		}
		sort.Slice(entries, func(i, j int) bool {
			if entries[i].IsDir() != entries[j].IsDir() {
				return entries[i].IsDir()
			}
			return entries[i].Name() < entries[j].Name()
		})
		for _, e := range entries {
			if count >= maxEntries {
				truncated = true
				return filepath.SkipAll
			}
			name := e.Name()
			if e.IsDir() && skipDir(name) {
				continue
			}
			indent := strings.Repeat("  ", depth)
			if e.IsDir() {
				fmt.Fprintf(&b, "%s%s/\n", indent, name)
				count++
				if depth < maxDepth-1 {
					if err := walk(filepath.Join(dir, name), depth+1); err != nil {
						return err
					}
				}
			} else {
				info, err := e.Info()
				size := int64(0)
				if err == nil {
					size = info.Size()
				}
				fmt.Fprintf(&b, "%s%s (%d B)\n", indent, name, size)
				count++
			}
		}
		return nil
	}
	if err := walk(abs, 0); err != nil && err != filepath.SkipAll {
		if ctx.Err() != nil {
			return fail("调用已取消: %v", ctx.Err())
		}
		return fail("遍历失败: %v", err)
	}
	if truncated {
		fmt.Fprintf(&b, "...(条目数超过上限 %d，已截断)\n", maxEntries)
	}
	resp := domains.Text(req.ToolCallID, "file_list", fmt.Sprintf("**目录**: `%s`\n```\n%s```", abs, strings.TrimRight(b.String(), "\n")))
	resp.Metadata = map[string]any{"path": abs, "entries": count, "truncated": truncated}
	return resp
}

func skipDir(name string) bool {
	switch name {
	case ".git", "node_modules", "dist", "out", ".next", ".nuxt", "__pycache__", ".venv", "venv", "target":
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// file_search（文件名 glob + 内容子串）
// ---------------------------------------------------------------------------

type searchTool struct{ guard *domains.Guard }

func (t *searchTool) Definition() tool.ToolDefinition {
	return tool.ToolDefinition{
		Name:        "file_search",
		Description: "在目录中搜索文件。mode=name 按文件名 glob 匹配；mode=content 按内容子串匹配（返回文件:行号:内容）。",
		Parameters: tool.ObjectSchema(map[string]tool.JSONSchema{
			"path":       {Type: "string", Description: "搜索根目录", Default: "."},
			"pattern":    {Type: "string", Description: "文件名 glob 或内容子串"},
			"mode":       {Type: "string", Description: "name | content", Enum: []any{"name", "content"}, Default: "name"},
			"maxResults": {Type: "integer", Description: "结果条数上限", Default: 50},
		}, "pattern"),
		Risk:        tool.RiskLow,
		Idempotency: tool.ClassIdempotent,
		Domain:      tool.DomainInProcess,
	}
}

func (t *searchTool) Execute(ctx context.Context, req tool.ToolRequest) tool.ToolResponse {
	fail := func(format string, args ...any) tool.ToolResponse {
		return domains.Fail(req.ToolCallID, "file_search", format, args...)
	}
	root := domains.StringArgDefault(req.Arguments, "path", ".")
	pattern := domains.StringArg(req.Arguments, "pattern")
	if pattern == "" {
		return fail("缺少 pattern 参数")
	}
	mode := domains.StringArgDefault(req.Arguments, "mode", "name")
	maxResults := domains.IntArg(req.Arguments, "maxResults", 50)
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return fail("路径解析失败: %v", err)
	}
	if err := t.guard.CheckSensitiveFile(absRoot); err != nil {
		return fail("%v", err)
	}

	var hits []string
	visited := 0
	stop := false
	err = filepath.WalkDir(absRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			if path != absRoot && skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		visited++
		if visited > maxSearchFiles {
			stop = true
			return filepath.SkipAll
		}
		if len(hits) >= maxResults {
			stop = true
			return filepath.SkipAll
		}
		rel, relErr := filepath.Rel(absRoot, path)
		if relErr != nil {
			rel = path
		}
		switch mode {
		case "content":
			info, statErr := d.Info()
			if statErr != nil || info.Size() > maxFileReadBytes {
				return nil
			}
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return nil
			}
			if isBinary(data) {
				return nil
			}
			for i, line := range strings.Split(string(data), "\n") {
				if strings.Contains(line, pattern) {
					hits = append(hits, fmt.Sprintf("%s:%d:%s", rel, i+1, strings.TrimSpace(line)))
					if len(hits) >= maxResults {
						stop = true
						return filepath.SkipAll
					}
				}
			}
		default:
			matched, matchErr := filepath.Match(pattern, d.Name())
			if matchErr != nil {
				return fmt.Errorf("glob 无效: %w", matchErr)
			}
			if matched {
				hits = append(hits, rel)
			}
		}
		return nil
	})
	if err != nil && ctx.Err() != nil {
		return fail("调用已取消: %v", ctx.Err())
	}
	if err != nil {
		return fail("搜索失败: %v", err)
	}
	if len(hits) == 0 {
		resp := domains.Text(req.ToolCallID, "file_search", fmt.Sprintf("未找到匹配 %q 的文件。", pattern))
		resp.Metadata = map[string]any{"hits": 0}
		return resp
	}
	content := fmt.Sprintf("🔍 搜索 %q（mode=%s）— %d 个结果\n\n%s", pattern, mode, len(hits), strings.Join(hits, "\n"))
	resp := domains.Text(req.ToolCallID, "file_search", content)
	resp.Metadata = map[string]any{"hits": len(hits), "truncated": stop}
	return resp
}

func isBinary(data []byte) bool {
	if len(data) > 8000 {
		data = data[:8000]
	}
	for _, b := range data {
		if b == 0 {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// file_write（B 类可检测幂等：expected_hash 状态检测）
// ---------------------------------------------------------------------------

type writeTool struct{ guard *domains.Guard }

func (t *writeTool) Definition() tool.ToolDefinition {
	return tool.ToolDefinition{
		Name: "file_write",
		Description: "写入或创建文件（自动创建父目录）。支持 overwrite/append。" +
			"传入 expectedHash（目标内容或既有内容的 sha256）时可进行状态检测：当前内容已匹配则跳过写入。",
		Parameters: tool.ObjectSchema(map[string]tool.JSONSchema{
			"filePath":     {Type: "string", Description: "文件路径"},
			"content":      {Type: "string", Description: "要写入的内容"},
			"mode":         {Type: "string", Description: "overwrite | append", Enum: []any{"overwrite", "append"}, Default: "overwrite"},
			"expectedHash": {Type: "string", Description: "期望的既有内容 sha256（hex）；匹配则视为已完成"},
		}, "filePath", "content"),
		Risk:        tool.RiskMedium,
		Idempotency: tool.ClassDetectable,
		Domain:      tool.DomainInProcess,
		SideEffect:  true,
	}
}

func (t *writeTool) Execute(ctx context.Context, req tool.ToolRequest) tool.ToolResponse {
	fail := func(format string, args ...any) tool.ToolResponse {
		return domains.Fail(req.ToolCallID, "file_write", format, args...)
	}
	path := domains.StringArg(req.Arguments, "filePath")
	content := domains.StringArg(req.Arguments, "content")
	mode := domains.StringArgDefault(req.Arguments, "mode", "overwrite")
	if path == "" {
		return fail("缺少 filePath 参数")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return fail("路径解析失败: %v", err)
	}
	if err := t.guard.CheckWriteAccess(abs); err != nil {
		return fail("%v", err)
	}
	if err := t.guard.CheckSensitiveFile(abs); err != nil {
		return fail("%v", err)
	}
	if err := ctx.Err(); err != nil {
		return fail("调用已取消: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return fail("创建父目录失败: %v", err)
	}

	flag := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if mode == "append" {
		flag = os.O_WRONLY | os.O_CREATE | os.O_APPEND
	}
	f, err := os.OpenFile(abs, flag, 0o644)
	if err != nil {
		return fail("打开文件失败: %v", err)
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		return fail("写入失败: %v", err)
	}
	if err := f.Sync(); err != nil {
		return fail("fsync 失败: %v", err)
	}

	lines := strings.Count(content, "\n") + 1
	resp := domains.Text(req.ToolCallID, "file_write", fmt.Sprintf("文件已%s：`%s`\n- %d 行\n- %.1f KB",
		map[string]string{"append": "追加写入", "overwrite": "创建/覆盖"}[mode], abs, lines, float64(len(content))/1024))
	resp.Metadata = map[string]any{
		"filePath": abs,
		"fileName": filepath.Base(abs),
		"lines":    lines,
		"mode":     mode,
		"sha256":   sha256Hex(content),
	}
	return resp
}

// CheckState 实现 tool.StateChecker：当前内容 sha256 与 expectedHash 匹配时
// 判定为已完成，避免崩溃恢复后重复写入（第19章 B 类）。
func (t *writeTool) CheckState(ctx context.Context, req tool.ToolRequest) (tool.StateStatus, error) {
	path := domains.StringArg(req.Arguments, "filePath")
	expected := domains.StringArg(req.Arguments, "expectedHash")
	if path == "" || expected == "" {
		return tool.StateUnknown, nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return tool.StateUnknown, err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return tool.StateNotDone, nil
		}
		return tool.StateUnknown, err
	}
	if sha256Hex(string(data)) == strings.ToLower(expected) {
		return tool.StateAlreadyDone, nil
	}
	// 内容不匹配：可能已被其他写入改变，按未完成处理（由工具自身覆盖写）。
	return tool.StateNotDone, nil
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func base64Encode(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}

// ---------------------------------------------------------------------------
// file_delete（C 类非幂等）
// ---------------------------------------------------------------------------

type deleteTool struct{ guard *domains.Guard }

func (t *deleteTool) Definition() tool.ToolDefinition {
	return tool.ToolDefinition{
		Name:        "file_delete",
		Description: "删除文件或目录（递归）。不可逆操作，权限引擎会要求用户确认；崩溃恢复时绝不自动重复。",
		Parameters: tool.ObjectSchema(map[string]tool.JSONSchema{
			"filePath": {Type: "string", Description: "要删除的文件或目录路径"},
		}, "filePath"),
		Risk:        tool.RiskHigh,
		Idempotency: tool.ClassNonIdempotent,
		Domain:      tool.DomainInProcess,
		SideEffect:  true,
	}
}

func (t *deleteTool) Execute(ctx context.Context, req tool.ToolRequest) tool.ToolResponse {
	fail := func(format string, args ...any) tool.ToolResponse {
		return domains.Fail(req.ToolCallID, "file_delete", format, args...)
	}
	path := domains.StringArg(req.Arguments, "filePath")
	if path == "" {
		return fail("缺少 filePath 参数")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return fail("路径解析失败: %v", err)
	}
	if err := t.guard.CheckWriteAccess(abs); err != nil {
		return fail("%v", err)
	}
	if err := t.guard.CheckSensitiveFile(abs); err != nil {
		return fail("%v", err)
	}
	// 防呆：拒绝删除文件系统根与写入根自身。
	for _, root := range append([]string{filepath.VolumeName(abs) + string(filepath.Separator)}, t.guard.AllowedWriteRoots...) {
		if abs == root {
			return fail("拒绝删除根目录 %q", abs)
		}
	}
	if err := ctx.Err(); err != nil {
		return fail("调用已取消: %v", err)
	}
	if err := os.RemoveAll(abs); err != nil {
		return fail("删除失败: %v", err)
	}
	resp := domains.Text(req.ToolCallID, "file_delete", fmt.Sprintf("已删除：`%s`", abs))
	resp.Metadata = map[string]any{"filePath": abs}
	return resp
}
