// Package git 提供纯 Go 的只读 git 工具（git_status/git_log/git_branch/git_diff）。
// 直接解析 .git 目录（index/refs/objects，含 packfile 与 delta），不依赖
// git 二进制、不产生任何副作用。写操作（add/commit/push 等）在 v2 中路由到
// Terminal Worker，不在本包范围。
package git

import (
	"context"
	"fmt"
	"strings"

	"github.com/ximo888ok-netizen/ximo-agent/internal/tool"
	"github.com/ximo888ok-netizen/ximo-agent/internal/tool/domains"
)

// Tools 返回 git 域全部工具。
func Tools(guard *domains.Guard) []tool.Tool {
	if guard == nil {
		guard = domains.NewGuard(nil)
	}
	return []tool.Tool{
		&statusTool{guard: guard},
		&logTool{guard: guard},
		&branchTool{guard: guard},
		&diffTool{guard: guard},
	}
}

func openRepo(guard *domains.Guard, args map[string]any) (*Repo, error) {
	path := domains.StringArgDefault(args, "repoPath", ".")
	repo, err := Discover(path)
	if err != nil {
		return nil, err
	}
	if len(guard.AllowedWriteRoots) > 0 && !withinAny(guard.AllowedWriteRoots, repo.workTree) {
		return nil, fmt.Errorf("仓库 %q 不在允许的根目录内", repo.workTree)
	}
	return repo, nil
}

func withinAny(roots []string, path string) bool {
	for _, root := range roots {
		if path == root || strings.HasPrefix(path, root+string(sep)) {
			return true
		}
	}
	return false
}

const sep = '/'

// ---------------------------------------------------------------------------
// git_status（B 类可检测幂等）
// ---------------------------------------------------------------------------

type statusTool struct{ guard *domains.Guard }

func (t *statusTool) Definition() tool.ToolDefinition {
	return tool.ToolDefinition{
		Name:        "git_status",
		Description: "查看 git 仓库状态：当前分支、HEAD 提交、已暂存/已修改/已删除/未跟踪的文件（等价 git status --porcelain）。",
		Parameters: tool.ObjectSchema(map[string]tool.JSONSchema{
			"repoPath": {Type: "string", Description: "仓库路径，默认当前目录", Default: "."},
		}),
		Risk:        tool.RiskLow,
		Idempotency: tool.ClassDetectable,
		Domain:      tool.DomainInProcess,
	}
}

func (t *statusTool) Execute(ctx context.Context, req tool.ToolRequest) tool.ToolResponse {
	repo, err := openRepo(t.guard, req.Arguments)
	if err != nil {
		return domains.Fail(req.ToolCallID, "git_status", "%v", err)
	}
	if err := ctx.Err(); err != nil {
		return domains.Fail(req.ToolCallID, "git_status", "调用已取消: %v", err)
	}
	status, err := repo.Status()
	if err != nil {
		return domains.Fail(req.ToolCallID, "git_status", "读取状态失败: %v", err)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "## 📊 Git Status\n仓库：`%s`\n", repo.workTree)
	branch := status.Branch
	if branch == "" {
		branch = "(detached HEAD)"
	}
	fmt.Fprintf(&b, "分支：%s\n", branch)
	if status.Commit != "" {
		fmt.Fprintf(&b, "HEAD：`%s`\n", short(status.Commit))
	}
	writeSection(&b, "已暂存", status.Staged)
	writeSection(&b, "已修改", status.Modified)
	writeSection(&b, "已删除", status.Deleted)
	writeSection(&b, "未跟踪", status.Untracked)
	if len(status.Staged)+len(status.Modified)+len(status.Deleted)+len(status.Untracked) == 0 {
		b.WriteString("\n工作区干净，无待提交的更改。")
	}

	resp := domains.Text(req.ToolCallID, "git_status", b.String())
	resp.Metadata = map[string]any{
		"branch":    status.Branch,
		"commit":    status.Commit,
		"staged":    len(status.Staged),
		"modified":  len(status.Modified),
		"deleted":   len(status.Deleted),
		"untracked": len(status.Untracked),
	}
	return resp
}

func writeSection(b *strings.Builder, title string, items []string) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(b, "\n**%s (%d)：**\n", title, len(items))
	for _, item := range items {
		b.WriteString("  " + item + "\n")
	}
}

// ---------------------------------------------------------------------------
// git_log（A 类幂等）
// ---------------------------------------------------------------------------

type logTool struct{ guard *domains.Guard }

func (t *logTool) Definition() tool.ToolDefinition {
	return tool.ToolDefinition{
		Name:        "git_log",
		Description: "查看提交历史（从 HEAD 沿首父提交回溯），返回 hash/日期/作者/主题。",
		Parameters: tool.ObjectSchema(map[string]tool.JSONSchema{
			"repoPath": {Type: "string", Description: "仓库路径", Default: "."},
			"count":    {Type: "integer", Description: "条数（1-100，默认 10）", Default: 10},
		}),
		Risk:        tool.RiskLow,
		Idempotency: tool.ClassIdempotent,
		Domain:      tool.DomainInProcess,
	}
}

func (t *logTool) Execute(ctx context.Context, req tool.ToolRequest) tool.ToolResponse {
	repo, err := openRepo(t.guard, req.Arguments)
	if err != nil {
		return domains.Fail(req.ToolCallID, "git_log", "%v", err)
	}
	count := domains.IntArg(req.Arguments, "count", 10)
	if count < 1 {
		count = 1
	}
	if count > 100 {
		count = 100
	}
	_, commit, err := repo.Head()
	if err != nil {
		return domains.Fail(req.ToolCallID, "git_log", "读取 HEAD 失败: %v", err)
	}
	if commit == "" {
		return domains.Text(req.ToolCallID, "git_log", "仓库尚无提交。")
	}
	if err := ctx.Err(); err != nil {
		return domains.Fail(req.ToolCallID, "git_log", "调用已取消: %v", err)
	}
	commits, err := repo.Log(commit, count)
	if err != nil {
		return domains.Fail(req.ToolCallID, "git_log", "读取历史失败: %v", err)
	}
	var b strings.Builder
	b.WriteString("## 📜 Git Log\n\n")
	for _, c := range commits {
		date := c.Date.Format("2006-01-02")
		fmt.Fprintf(&b, "- `%s` %s — **%s** (%s)\n", short(c.SHA), date, c.Subject, c.Author)
	}
	resp := domains.Text(req.ToolCallID, "git_log", b.String())
	resp.Metadata = map[string]any{"count": len(commits)}
	return resp
}

// ---------------------------------------------------------------------------
// git_branch（A 类幂等）
// ---------------------------------------------------------------------------

type branchTool struct{ guard *domains.Guard }

func (t *branchTool) Definition() tool.ToolDefinition {
	return tool.ToolDefinition{
		Name:        "git_branch",
		Description: "列出本地分支并标记当前分支。",
		Parameters: tool.ObjectSchema(map[string]tool.JSONSchema{
			"repoPath": {Type: "string", Description: "仓库路径", Default: "."},
		}),
		Risk:        tool.RiskLow,
		Idempotency: tool.ClassIdempotent,
		Domain:      tool.DomainInProcess,
	}
}

func (t *branchTool) Execute(ctx context.Context, req tool.ToolRequest) tool.ToolResponse {
	repo, err := openRepo(t.guard, req.Arguments)
	if err != nil {
		return domains.Fail(req.ToolCallID, "git_branch", "%v", err)
	}
	branches, err := repo.Branches()
	if err != nil {
		return domains.Fail(req.ToolCallID, "git_branch", "读取分支失败: %v", err)
	}
	if len(branches) == 0 {
		return domains.Text(req.ToolCallID, "git_branch", "仓库没有本地分支。")
	}
	var b strings.Builder
	b.WriteString("## 🌿 Git Branches\n\n")
	for _, br := range branches {
		if br.Current {
			fmt.Fprintf(&b, "- **`%s`** ← 当前\n", br.Name)
		} else {
			fmt.Fprintf(&b, "- `%s`\n", br.Name)
		}
	}
	resp := domains.Text(req.ToolCallID, "git_branch", b.String())
	resp.Metadata = map[string]any{"branches": len(branches)}
	return resp
}

// ---------------------------------------------------------------------------
// git_diff（A 类幂等）：工作区 vs index 的文本差异
// ---------------------------------------------------------------------------

type diffTool struct{ guard *domains.Guard }

func (t *diffTool) Definition() tool.ToolDefinition {
	return tool.ToolDefinition{
		Name:        "git_diff",
		Description: "查看工作区相对暂存区（index）的文本差异（unified diff）。仅支持文本文件。",
		Parameters: tool.ObjectSchema(map[string]tool.JSONSchema{
			"repoPath": {Type: "string", Description: "仓库路径", Default: "."},
			"maxLines": {Type: "integer", Description: "diff 输出行数上限", Default: 400},
		}),
		Risk:        tool.RiskLow,
		Idempotency: tool.ClassIdempotent,
		Domain:      tool.DomainInProcess,
	}
}

func (t *diffTool) Execute(ctx context.Context, req tool.ToolRequest) tool.ToolResponse {
	repo, err := openRepo(t.guard, req.Arguments)
	if err != nil {
		return domains.Fail(req.ToolCallID, "git_diff", "%v", err)
	}
	if err := ctx.Err(); err != nil {
		return domains.Fail(req.ToolCallID, "git_diff", "调用已取消: %v", err)
	}
	status, err := repo.Status()
	if err != nil {
		return domains.Fail(req.ToolCallID, "git_diff", "读取状态失败: %v", err)
	}
	if len(status.Modified) == 0 && len(status.Deleted) == 0 {
		return domains.Text(req.ToolCallID, "git_diff", "工作区无差异。")
	}
	maxLines := domains.IntArg(req.Arguments, "maxLines", 400)

	var b strings.Builder
	lines := 0
	truncated := false
	for _, entry := range append(append([]string{}, status.Modified...), status.Deleted...) {
		// 条目不带头部的 XY 状态码，如 " M a.txt" -> "a.txt"
		path := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(entry), "MAD?"))
		if lines >= maxLines {
			truncated = true
			break
		}
		var oldText, newText string
		if indexEntry, ok := repo.indexEntryFor(path); ok {
			oldText = repo.blobText(indexEntry.sha)
		}
		if !strings.HasPrefix(strings.TrimSpace(entry), "D") {
			if data, err := repo.readWorktreeFile(path); err == nil {
				newText = string(data)
			}
		}
		fmt.Fprintf(&b, "diff --git a/%s b/%s\n", path, path)
		for _, line := range unifiedDiff(path, oldText, newText) {
			if lines >= maxLines {
				truncated = true
				break
			}
			b.WriteString(line + "\n")
			lines++
		}
	}
	if truncated {
		b.WriteString(fmt.Sprintf("\n...(diff 超过 %d 行，已截断)", maxLines))
	}
	resp := domains.Code(req.ToolCallID, "git_diff", fmt.Sprintf("## 📝 Git Diff\n```diff\n%s\n```", strings.TrimRight(b.String(), "\n")))
	resp.Metadata = map[string]any{"files": len(status.Modified) + len(status.Deleted), "truncated": truncated}
	return resp
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
