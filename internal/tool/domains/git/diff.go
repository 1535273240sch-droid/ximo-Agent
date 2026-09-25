package git

import (
	"fmt"
	"strings"
)

// unifiedDiff 生成简化版 unified diff（3 行上下文，LCS 行对齐）。
// 大文件（> 2000 行）退化为“文件不同”提示，避免 O(N*M) 爆炸。
func unifiedDiff(path, oldText, newText string) []string {
	if oldText == newText {
		return nil
	}
	oldLines := splitLines(oldText)
	newLines := splitLines(newText)
	if len(oldLines) > 2000 || len(newLines) > 2000 {
		return []string{
			fmt.Sprintf("--- a/%s", path),
			fmt.Sprintf("+++ b/%s", path),
			"@@ 文件过大（>2000 行），退化为差异提示 @@",
			fmt.Sprintf("- %d 行", len(oldLines)),
			fmt.Sprintf("+ %d 行", len(newLines)),
		}
	}
	ops := diffLines(oldLines, newLines)
	if len(ops) == 0 {
		return nil
	}
	out := []string{
		fmt.Sprintf("--- a/%s", path),
		fmt.Sprintf("+++ b/%s", path),
	}
	const context = 3
	// 收集变更块（带上下文）
	type hunk struct {
		oldStart, newStart int
		lines              []string
	}
	var hunks []hunk
	i := 0
	for i < len(ops) {
		if ops[i].kind == ' ' {
			i++
			continue
		}
		// 变更块起点（向前扩展上下文）
		start := i
		ctxBefore := 0
		for start > 0 && ops[start-1].kind == ' ' && ctxBefore < context {
			start--
			ctxBefore++
		}
		end := i
		for end < len(ops) && ops[end].kind != ' ' {
			end++
		}
		// 向后扩展上下文
		ctxAfter := 0
		for end < len(ops) && ops[end].kind == ' ' && ctxAfter < context {
			end++
			ctxAfter++
		}
		var lines []string
		oldCount, newCount := 0, 0
		for _, op := range ops[start:end] {
			lines = append(lines, string(op.kind)+op.text)
			if op.kind == ' ' {
				oldCount++
				newCount++
			} else if op.kind == '-' {
				oldCount++
			} else {
				newCount++
			}
		}
		hunks = append(hunks, hunk{
			oldStart: opOldIndex(ops, start) + 1,
			newStart: opNewIndex(ops, start) + 1,
			lines:    lines,
		})
		_ = oldCount
		_ = newCount
		i = end
	}
	for _, h := range hunks {
		out = append(out, fmt.Sprintf("@@ -%d,%d +%d,%d @@", h.oldStart, countKind(h.lines, '-', ' '), h.newStart, countKind(h.lines, '+', ' ')))
		out = append(out, h.lines...)
	}
	return out
}

func countKind(lines []string, kinds ...byte) int {
	n := 0
	for _, line := range lines {
		if len(line) == 0 {
			continue
		}
		for _, k := range kinds {
			if line[0] == k {
				n++
				break
			}
		}
	}
	return n
}

func opOldIndex(ops []diffOp, i int) int {
	n := 0
	for j := 0; j < i; j++ {
		if ops[j].kind == '-' || ops[j].kind == ' ' {
			n++
		}
	}
	return n
}

func opNewIndex(ops []diffOp, i int) int {
	n := 0
	for j := 0; j < i; j++ {
		if ops[j].kind == '+' || ops[j].kind == ' ' {
			n++
		}
	}
	return n
}

func splitLines(text string) []string {
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	// 末尾换行产生的空元素去掉
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

type diffOp struct {
	kind byte // ' ' | '-' | '+'
	text string
}

// diffLines 用 LCS 计算行级 diff。
func diffLines(a, b []string) []diffOp {
	n, m := len(a), len(b)
	// LCS 长度表（滚动数组以控制内存）
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}
	var ops []diffOp
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, diffOp{' ', a[i]})
			i++
			j++
		case dp[i+1][j] >= dp[i][j+1]:
			ops = append(ops, diffOp{'-', a[i]})
			i++
		default:
			ops = append(ops, diffOp{'+', b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, diffOp{'-', a[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, diffOp{'+', b[j]})
	}
	return ops
}
