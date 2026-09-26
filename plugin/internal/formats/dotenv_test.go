package formats

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const dotenvFixture = `# gateway settings
# keep both comment lines

ANTHROPIC_BASE_URL=http://old.example # production
ANTHROPIC_AUTH_TOKEN="sk-keep me"
export MODEL='claude-sonnet'

UNQUOTED=A B
HASH=value#not-a-comment
`

func TestDotenvKeepsCommentsBlankLinesAndOrder(t *testing.T) {
	dir := t.TempDir()
	path := writeTemp(t, dir, "config.env", dotenvFixture)

	if err := Write(path, FormatDotenv, map[string]any{"ANTHROPIC_BASE_URL": "http://gw.local/v1"}, false); err != nil {
		t.Fatalf("Write: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	want := []string{
		"# gateway settings",
		"# keep both comment lines",
		"",
		"ANTHROPIC_BASE_URL=http://gw.local/v1 # production",
		`ANTHROPIC_AUTH_TOKEN="sk-keep me"`,
		"export MODEL='claude-sonnet'",
		"",
		"UNQUOTED=A B",
		"HASH=value#not-a-comment",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("file changed more than it should:\n--- got ---\n%s\n--- want ---\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	doc, err := Read(path, FormatDotenv)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if doc["ANTHROPIC_BASE_URL"] != "http://gw.local/v1" {
		t.Errorf("ANTHROPIC_BASE_URL = %#v", doc["ANTHROPIC_BASE_URL"])
	}
	if doc["ANTHROPIC_AUTH_TOKEN"] != "sk-keep me" {
		t.Errorf("quoted value = %#v", doc["ANTHROPIC_AUTH_TOKEN"])
	}
	if doc["MODEL"] != "claude-sonnet" {
		t.Errorf("exported value = %#v", doc["MODEL"])
	}
	if doc["UNQUOTED"] != "A B" {
		t.Errorf("unquoted value = %#v", doc["UNQUOTED"])
	}
	if doc["HASH"] != "value#not-a-comment" {
		t.Errorf("# inside a value should not start a comment: %#v", doc["HASH"])
	}
}

func TestDotenvUnchangedLinesAreNotRewritten(t *testing.T) {
	dir := t.TempDir()
	path := writeTemp(t, dir, "config.env", dotenvFixture)
	if err := Write(path, FormatDotenv, map[string]any{"UNQUOTED": "A B"}, false); err != nil {
		t.Fatalf("Write: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(raw) != dotenvFixture {
		t.Errorf("setting a key to its current value rewrote the file:\n--- got ---\n%s\n--- want ---\n%s", raw, dotenvFixture)
	}
}

func TestDotenvQuotingStyleIsKept(t *testing.T) {
	cases := []struct {
		name    string
		content string
		value   any
		want    string
	}{
		{"double quoted stays double quoted", "KEY=\"old\"\n", "new value", "KEY=\"new value\"\n"},
		{"single quoted stays single quoted", "KEY='old'\n", "new", "KEY='new'\n"},
		{"unquoted and simple stays unquoted", "KEY=old\n", "http://gw.local/v1", "KEY=http://gw.local/v1\n"},
		{"unquoted value with a space gets quotes", "KEY=old\n", "two words", "KEY=\"two words\"\n"},
		{"single quote inside a single quoted value falls back", "KEY='old'\n", "it's", "KEY=\"it's\"\n"},
		{"numbers and booleans are stringified", "KEY=old\n", 42, "KEY=42\n"},
		{"empty value", "KEY=old\n", "", "KEY=\"\"\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := writeTemp(t, dir, "config.env", tc.content)
			if err := Write(path, FormatDotenv, map[string]any{"KEY": tc.value}, false); err != nil {
				t.Fatalf("Write: %v", err)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if string(raw) != tc.want {
				t.Errorf("got %q, want %q", raw, tc.want)
			}
			doc, err := Read(path, FormatDotenv)
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if stringifyDotenv(doc["KEY"]) != stringifyDotenv(tc.value) {
				t.Errorf("round trip changed the value: %#v", doc["KEY"])
			}
		})
	}
}

func TestDotenvAppendsNewKeys(t *testing.T) {
	dir := t.TempDir()
	path := writeTemp(t, dir, "config.env", "# head\nANTHROPIC_BASE_URL=http://old\n")
	doc := map[string]any{
		"ANTHROPIC_BASE_URL":   "http://gw.local",
		"ANTHROPIC_AUTH_TOKEN": "sk-x",
	}
	if err := Write(path, FormatDotenv, doc, false); err != nil {
		t.Fatalf("Write: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	want := "# head\nANTHROPIC_BASE_URL=http://gw.local\nANTHROPIC_AUTH_TOKEN=sk-x\n"
	if string(raw) != want {
		t.Errorf("got %q, want %q", raw, want)
	}
}

func TestDotenvAppendKeepsAMissingTrailingNewlineReadable(t *testing.T) {
	dir := t.TempDir()
	path := writeTemp(t, dir, "config.env", "A=1")
	if err := Write(path, FormatDotenv, map[string]any{"B": "2"}, false); err != nil {
		t.Fatalf("Write: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(raw) != "A=1\nB=2\n" {
		t.Errorf("got %q", raw)
	}
	doc, err := Read(path, FormatDotenv)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if doc["A"] != "1" || doc["B"] != "2" {
		t.Errorf("doc = %#v", doc)
	}
}

func TestDotenvDuplicateKeysAreAllUpdated(t *testing.T) {
	dir := t.TempDir()
	path := writeTemp(t, dir, "config.env", "KEY=first\nOTHER=1\nKEY=second\n")
	if err := Write(path, FormatDotenv, map[string]any{"KEY": "third"}, false); err != nil {
		t.Fatalf("Write: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(raw) != "KEY=third\nOTHER=1\nKEY=third\n" {
		t.Errorf("got %q, want both occurrences updated", raw)
	}
	doc, err := Read(path, FormatDotenv)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if doc["KEY"] != "third" {
		t.Errorf("KEY = %#v", doc["KEY"])
	}
}

func TestDotenvWriteCreatesAFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh.env")
	if err := Write(path, FormatDotenv, map[string]any{"A": "1", "B": "two words"}, false); err != nil {
		t.Fatalf("Write: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(raw) != "A=1\nB=\"two words\"\n" {
		t.Errorf("got %q", raw)
	}
}

func TestDotenvParseErrors(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"no equals sign", "GOOD=1\nthis is not a kv line\n", "line 2: expected KEY=VALUE"},
		{"invalid name", "1BAD=2\n", "line 1: invalid variable name"},
		{"unterminated double quote", "A=\"oops\n", "line 1: unterminated double-quoted value"},
		{"unterminated single quote", "A='oops\n", "line 1: unterminated single-quoted value"},
		{"text after a quoted value", `A="x" y`, "line 1: unexpected text after a quoted value"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := writeTemp(t, dir, "broken.env", tc.content)
			_, err := Read(path, FormatDotenv)
			if err == nil {
				t.Fatal("want a parse error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestDotenvExportKeywordVersusPrefixInTheName(t *testing.T) {
	doc, err := (dotenvCodec{}).parse([]byte("exportFOO=1\nexport BAR=2\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if doc["exportFOO"] != "1" {
		t.Errorf("exportFOO = %#v", doc["exportFOO"])
	}
	if doc["BAR"] != "2" {
		t.Errorf("BAR = %#v", doc["BAR"])
	}
}
