package formats

import (
	"fmt"
	"strings"
)

type opKind int

const (
	opEqual opKind = iota
	opDelete
	opInsert
)

type op struct {
	kind opKind
	line string
}

// diffContext is the number of unchanged lines shown around each change, and
// diffMaxD bounds the Myers search. A change bigger than that gives up on
// minimality and reports one hunk covering the whole difference.
const (
	diffContext = 3
	diffMaxD    = 512
)

// unifiedDiff renders a unified-style diff of two texts. It returns "" when the
// texts are identical. A trailing newline is not represented, so a change of
// line endings alone does not show up.
func unifiedDiff(path, before, after string) string {
	a, b := splitLines(before), splitLines(after)
	if equalLines(a, b) {
		return ""
	}
	ops, ok := myers(a, b)
	if !ok {
		ops = coarseOps(a, b)
	}
	body := renderHunks(ops)
	if body == "" {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "--- %s (current)\n", path)
	fmt.Fprintf(&sb, "+++ %s (after)\n", path)
	sb.WriteString(body)
	return sb.String()
}

func splitLines(text string) []string {
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	for i, line := range lines {
		lines[i] = strings.TrimSuffix(line, "\r")
	}
	return lines
}

func equalLines(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// myers returns the shortest edit script between a and b, or false when more
// than diffMaxD lines differ.
func myers(a, b []string) ([]op, bool) {
	n, m := len(a), len(b)
	limit := n + m
	if limit > diffMaxD {
		limit = diffMaxD
	}
	offset := limit
	v := make([]int, 2*limit+2)
	var trace [][]int
	var found int
	for d := 0; d <= limit; d++ {
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[offset+k-1] < v[offset+k+1]) {
				x = v[offset+k+1]
			} else {
				x = v[offset+k-1] + 1
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			v[offset+k] = x
			if x >= n && y >= m {
				found = d
				snapshot := make([]int, len(v))
				copy(snapshot, v)
				trace = append(trace, snapshot)
				return backtrack(trace, a, b, offset, found), true
			}
		}
		snapshot := make([]int, len(v))
		copy(snapshot, v)
		trace = append(trace, snapshot)
	}
	return nil, false
}

func backtrack(trace [][]int, a, b []string, offset, found int) []op {
	x, y := len(a), len(b)
	var reversed []op
	for d := found; d >= 0; d-- {
		v := trace[d]
		k := x - y
		var prevK int
		if k == -d || (k != d && v[offset+k-1] < v[offset+k+1]) {
			prevK = k + 1
		} else {
			prevK = k - 1
		}
		prevX := v[offset+prevK]
		prevY := prevX - prevK
		for x > prevX && y > prevY {
			reversed = append(reversed, op{opEqual, a[x-1]})
			x--
			y--
		}
		if d > 0 {
			if x == prevX {
				reversed = append(reversed, op{opInsert, b[y-1]})
				y--
			} else {
				reversed = append(reversed, op{opDelete, a[x-1]})
				x--
			}
		}
	}
	ops := make([]op, 0, len(reversed))
	for i := len(reversed) - 1; i >= 0; i-- {
		ops = append(ops, reversed[i])
	}
	return ops
}

// coarseOps is the fallback when the two texts differ in too many places: one
// replacement covering everything between the common prefix and suffix.
func coarseOps(a, b []string) []op {
	prefix := 0
	for prefix < len(a) && prefix < len(b) && a[prefix] == b[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(a)-prefix && suffix < len(b)-prefix &&
		a[len(a)-1-suffix] == b[len(b)-1-suffix] {
		suffix++
	}
	ops := make([]op, 0, len(a)+len(b))
	for _, line := range a[:prefix] {
		ops = append(ops, op{opEqual, line})
	}
	for _, line := range a[prefix : len(a)-suffix] {
		ops = append(ops, op{opDelete, line})
	}
	for _, line := range b[prefix : len(b)-suffix] {
		ops = append(ops, op{opInsert, line})
	}
	for _, line := range a[len(a)-suffix:] {
		ops = append(ops, op{opEqual, line})
	}
	return ops
}

// renderHunks formats the edit script with diffContext lines of context,
// merging changes that are close enough to share it.
func renderHunks(ops []op) string {
	n := len(ops)
	oldAt := make([]int, n+1)
	newAt := make([]int, n+1)
	oldLine, newLine := 1, 1
	for i, o := range ops {
		oldAt[i], newAt[i] = oldLine, newLine
		if o.kind != opInsert {
			oldLine++
		}
		if o.kind != opDelete {
			newLine++
		}
	}
	oldAt[n], newAt[n] = oldLine, newLine

	var changes []int
	for i, o := range ops {
		if o.kind != opEqual {
			changes = append(changes, i)
		}
	}
	if len(changes) == 0 {
		return ""
	}

	var sb strings.Builder
	for i := 0; i < len(changes); {
		last := changes[i]
		j := i
		for j+1 < len(changes) && changes[j+1]-last <= 2*diffContext+1 {
			j++
			last = changes[j]
		}
		start := changes[i] - diffContext
		if start < 0 {
			start = 0
		}
		stop := last + diffContext + 1
		if stop > n {
			stop = n
		}
		oldCount, newCount := 0, 0
		for _, o := range ops[start:stop] {
			if o.kind != opInsert {
				oldCount++
			}
			if o.kind != opDelete {
				newCount++
			}
		}
		oldStart, newStart := oldAt[start], newAt[start]
		// Unified diffs count an empty range from the preceding line.
		if oldCount == 0 {
			oldStart--
		}
		if newCount == 0 {
			newStart--
		}
		fmt.Fprintf(&sb, "@@ -%d,%d +%d,%d @@\n", oldStart, oldCount, newStart, newCount)
		for _, o := range ops[start:stop] {
			switch o.kind {
			case opEqual:
				sb.WriteString(" " + o.line + "\n")
			case opDelete:
				sb.WriteString("-" + o.line + "\n")
			case opInsert:
				sb.WriteString("+" + o.line + "\n")
			}
		}
		i = j + 1
	}
	return sb.String()
}
