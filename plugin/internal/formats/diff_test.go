package formats

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestDiffReportsAValueChange(t *testing.T) {
	dir := t.TempDir()
	const original = `{
  "env": {
    "ANTHROPIC_BASE_URL": "http://old"
  }
}
`
	path := writeTemp(t, dir, "config.json", original)
	diff, err := Diff(path, FormatJSON, map[string]any{"env": map[string]any{"ANTHROPIC_BASE_URL": "http://new"}})
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if !strings.Contains(diff, "--- "+path+" (current)") || !strings.Contains(diff, "+++ "+path+" (after)") {
		t.Errorf("diff should carry both headers:\n%s", diff)
	}
	if !strings.Contains(diff, "-") || !strings.Contains(diff, `+    "ANTHROPIC_BASE_URL": "http://new"`) {
		t.Errorf("diff should show the removed and the added line:\n%s", diff)
	}
	if !regexp.MustCompile(`@@ -\d+,\d+ \+\d+,\d+ @@`).MatchString(diff) {
		t.Errorf("diff should carry a hunk header:\n%s", diff)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(raw) != original {
		t.Errorf("Diff modified the file: %q", raw)
	}
}

func TestDiffWithoutChangesIsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := writeTemp(t, dir, "config.json", `{"a": "1"}`)
	diff, err := Diff(path, FormatJSON, map[string]any{"a": "1"})
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if diff != "" {
		t.Errorf("want no diff, got:\n%s", diff)
	}
	if diff, err := Diff(path, FormatJSON, map[string]any{}); err != nil || diff != "" {
		t.Errorf("an empty document should diff to nothing, got %q / %v", diff, err)
	}
}

func TestDiffOfAMissingFileShowsItAsNew(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh.env")
	diff, err := Diff(path, FormatDotenv, map[string]any{"ANTHROPIC_BASE_URL": "http://gw"})
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if !strings.Contains(diff, "+ANTHROPIC_BASE_URL=http://gw") {
		t.Errorf("want the new line as an addition:\n%s", diff)
	}
	if strings.Contains(diff, "-ANTHROPIC_BASE_URL") {
		t.Errorf("nothing should be removed from a missing file:\n%s", diff)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("Diff must not create the file: %v", err)
	}
}

func TestDiffKeepsUntouchedYAMLCommentsOutOfTheDiff(t *testing.T) {
	dir := t.TempDir()
	path := writeTemp(t, dir, "config.yaml", "# keep me\nmodel: old\nother: 1\n")
	diff, err := Diff(path, FormatYAML, map[string]any{"model": "new"})
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if !strings.Contains(diff, "-model: old") || !strings.Contains(diff, "+model: new") {
		t.Errorf("diff should show only the changed key:\n%s", diff)
	}
	for _, line := range strings.Split(diff, "\n") {
		if (strings.HasPrefix(line, "-") || strings.HasPrefix(line, "+")) &&
			!strings.HasPrefix(line, "---") && !strings.HasPrefix(line, "+++") &&
			strings.Contains(line, "#") {
			t.Errorf("comments identical on both sides should only appear as context:\n%s", diff)
		}
	}
}

// TestDiffAppliesBackToTheNewText is the property that matters for --dry-run:
// patching the current text with the printed hunks must reproduce exactly what
// Write is about to store.
func TestDiffAppliesBackToTheNewText(t *testing.T) {
	cases := []struct {
		name   string
		before string
		after  string
	}{
		{"single change", "a\nb\nc\nd\ne\n", "a\nb\nC\nd\ne\n"},
		{"insertion at the top", "b\nc\n", "a\nb\nc\n"},
		{"insertion at the end", "a\nb\n", "a\nb\nc\nd\n"},
		{"blank file", "", "a\nb\n"},
		{"emptied file", "a\nb\n", ""},
		{"two hunks", "1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n11\n12\n13\n14\n15\n",
			"1\nX\n3\n4\n5\n6\n7\n8\n9\n10\n11\n12\n13\nY\n15\n"},
		{"delete and add", "a\nb\nc\n", "a\nx\ny\n"},
		{"identical", "a\nb\n", "a\nb\n"},
		{"reordered", "a\nb\nc\nd\n", "b\na\nd\nc\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			diff := unifiedDiff("test", tc.before, tc.after)
			if tc.before == tc.after && diff != "" {
				t.Fatalf("identical texts produced a diff:\n%s", diff)
			}
			if got := applyUnified(t, tc.before, diff); got != tc.after {
				t.Errorf("applying the diff did not reproduce the new text\n--- diff ---\n%s\n--- got ---\n%q\n--- want ---\n%q",
					diff, got, tc.after)
			}
		})
	}
}

// applyUnified is a minimal unified-diff reader, used only to check that the
// hunks we print describe the intended new text.
func applyUnified(t *testing.T, before, diff string) string {
	t.Helper()
	if strings.TrimSpace(diff) == "" {
		return before
	}
	old := splitLines(before)
	var out []string
	index := 0
	lines := strings.Split(diff, "\n")
	hunk := regexp.MustCompile(`^@@ -(\d+),(\d+) \+\d+,\d+ @@$`)
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		switch {
		case line == "" && i == len(lines)-1:
			// trailing newline of the diff itself
		case strings.HasPrefix(line, "---") || strings.HasPrefix(line, "+++"):
		case strings.HasPrefix(line, "@@"):
			m := hunk.FindStringSubmatch(line)
			if m == nil {
				t.Fatalf("unparsable hunk header: %q", line)
			}
			start, err := strconv.Atoi(m[1])
			if err != nil {
				t.Fatalf("hunk start: %v", err)
			}
			count, err := strconv.Atoi(m[2])
			if err != nil {
				t.Fatalf("hunk count: %v", err)
			}
			at := start - 1
			if count == 0 {
				at = start
			}
			if at < index || at > len(old) {
				t.Fatalf("hunk starts at %d, outside the already applied region (%d) of %d lines", at, index, len(old))
			}
			for index < at {
				out = append(out, old[index])
				index++
			}
		case strings.HasPrefix(line, "-"):
			if index >= len(old) || old[index] != line[1:] {
				t.Fatalf("hunk removes %q but the old text has %q", line[1:], old[index])
			}
			index++
		case strings.HasPrefix(line, "+"):
			out = append(out, line[1:])
		case strings.HasPrefix(line, " "):
			if index >= len(old) || old[index] != line[1:] {
				t.Fatalf("context line %q does not match the old text %q", line[1:], old[index])
			}
			out = append(out, old[index])
			index++
		default:
			t.Fatalf("unexpected diff line %q", line)
		}
	}
	out = append(out, old[index:]...)
	if len(out) == 0 {
		return ""
	}
	return strings.Join(out, "\n") + "\n"
}

func TestUnifiedDiffContextIsLimited(t *testing.T) {
	before := ""
	after := ""
	for i := 1; i <= 40; i++ {
		before += fmt.Sprintf("line %d\n", i)
	}
	for i := 1; i <= 40; i++ {
		if i == 20 {
			after += "changed\n"
			continue
		}
		after += fmt.Sprintf("line %d\n", i)
	}
	diff := unifiedDiff("test", before, after)
	hunks := strings.Count(diff, "@@ -")
	if hunks != 1 {
		t.Fatalf("want a single hunk, got %d:\n%s", hunks, diff)
	}
	context := strings.Count(diff, "\n ")
	if context != 2*diffContext {
		t.Errorf("want %d context lines, got %d:\n%s", 2*diffContext, context, diff)
	}
	if got := applyUnified(t, before, diff); got != after {
		t.Errorf("applying the diff did not reproduce the new text:\n%s", diff)
	}
}
