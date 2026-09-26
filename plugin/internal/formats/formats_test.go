package formats

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

type set struct {
	path  string
	value any
}

type formatCase struct {
	name    string
	format  string
	ext     string
	content string
	sets    []set
	keeps   []set
	// markers are literal strings that must still be present in the file on
	// disk after Write, proving unknown fields survived byte for byte.
	markers []string
}

var roundTripCases = []formatCase{
	{
		name:   "json",
		format: FormatJSON,
		ext:    ".json",
		content: `{
  "env": {
    "ANTHROPIC_BASE_URL": "http://old.example",
    "ANTHROPIC_AUTH_TOKEN": "sk-old",
    "KEEP_ME": "1"
  },
  "unknown_top": {
    "deep": [1, 2, 3]
  },
  "models": [
    {"name": "first", "api_base": "http://old.example", "extra": true},
    {"name": "second"}
  ]
}
`,
		sets: []set{
			{"env.ANTHROPIC_BASE_URL", "http://gateway.local:8080/v1"},
			{"models.0.api_base", "http://gateway.local:8080/v1"},
		},
		keeps: []set{
			{"env.KEEP_ME", "1"},
			{"env.ANTHROPIC_AUTH_TOKEN", "sk-old"},
			{"unknown_top.deep", []any{1, 2, 3}},
			{"models.0.name", "first"},
			{"models.0.extra", true},
			{"models.1.name", "second"},
		},
		markers: []string{"unknown_top", "KEEP_ME", "extra"},
	},
	{
		name:   "toml",
		format: FormatTOML,
		ext:    ".toml",
		content: `model = "old-model"

[env]
ANTHROPIC_BASE_URL = "http://old.example"
ANTHROPIC_AUTH_TOKEN = "sk-old"
KEEP_ME = "1"

[unknown_top]
deep = [1, 2, 3]

[[models]]
name = "first"
api_base = "http://old.example"

[[models]]
name = "second"
`,
		sets: []set{
			{"env.ANTHROPIC_BASE_URL", "http://gateway.local:8080/v1"},
			{"models.0.api_base", "http://gateway.local:8080/v1"},
		},
		keeps: []set{
			{"model", "old-model"},
			{"env.KEEP_ME", "1"},
			{"env.ANTHROPIC_AUTH_TOKEN", "sk-old"},
			{"unknown_top.deep", []any{1, 2, 3}},
			{"models.0.name", "first"},
			{"models.1.name", "second"},
		},
		markers: []string{"unknown_top", "KEEP_ME"},
	},
	{
		name:   "yaml",
		format: FormatYAML,
		ext:    ".yaml",
		content: `# gateway settings
model: old-model
env:
  # the token below must stay
  ANTHROPIC_BASE_URL: http://old.example
  ANTHROPIC_AUTH_TOKEN: "sk-old"
  KEEP_ME: "1"
unknown_top:
  deep: [1, 2, 3]
models:
  - name: first
    api_base: http://old.example
    extra: true
  - name: second
`,
		sets: []set{
			{"env.ANTHROPIC_BASE_URL", "http://gateway.local:8080/v1"},
			{"models.0.api_base", "http://gateway.local:8080/v1"},
		},
		keeps: []set{
			{"model", "old-model"},
			{"env.KEEP_ME", "1"},
			{"env.ANTHROPIC_AUTH_TOKEN", "sk-old"},
			{"unknown_top.deep", []any{1, 2, 3}},
			{"models.1.name", "second"},
			{"models.0.name", "first"},
			{"models.0.extra", true},
		},
		markers: []string{"# gateway settings", "# the token below must stay", "unknown_top"},
	},
	{
		name:   "dotenv",
		format: FormatDotenv,
		ext:    ".env",
		content: `# gateway settings
ANTHROPIC_BASE_URL=http://old.example
ANTHROPIC_AUTH_TOKEN="sk-old"

KEEP_ME=1
`,
		sets: []set{
			{"ANTHROPIC_BASE_URL", "http://gateway.local:8080/v1"},
		},
		keeps: []set{
			{"ANTHROPIC_AUTH_TOKEN", "sk-old"},
			{"KEEP_ME", "1"},
		},
		markers: []string{"# gateway settings"},
	},
}

func writeTemp(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func mustSetPath(t *testing.T, doc map[string]any, path string, value any) {
	t.Helper()
	if err := SetPath(doc, path, value); err != nil {
		t.Fatalf("SetPath(%q): %v", path, err)
	}
}

// canonJSON renders a document in a way that hides the difference between the
// number types the four parsers produce (json.Number, int64, float64).
func canonJSON(t *testing.T, doc map[string]any) string {
	t.Helper()
	encoded, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("canonical json: %v", err)
	}
	return string(encoded)
}

func TestRoundTripAllFormats(t *testing.T) {
	for _, tc := range roundTripCases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := writeTemp(t, dir, "config"+tc.ext, tc.content)

			doc := map[string]any{}
			for _, s := range tc.sets {
				mustSetPath(t, doc, s.path, s.value)
			}
			if err := Write(path, tc.format, doc, false); err != nil {
				t.Fatalf("Write: %v", err)
			}

			got, err := Read(path, tc.format)
			if err != nil {
				t.Fatalf("Read after Write: %v", err)
			}
			want := map[string]any{}
			for _, s := range append(append([]set{}, tc.keeps...), tc.sets...) {
				mustSetPath(t, want, s.path, s.value)
			}
			if canonJSON(t, got) != canonJSON(t, want) {
				t.Errorf("round trip mismatch\n got: %s\nwant: %s", canonJSON(t, got), canonJSON(t, want))
			}

			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read file: %v", err)
			}
			for _, marker := range tc.markers {
				if !strings.Contains(string(raw), marker) {
					t.Errorf("unknown content %q disappeared from the file:\n%s", marker, raw)
				}
			}
		})
	}
}

func TestReadMissingFileIsEmptyNotAnError(t *testing.T) {
	dir := t.TempDir()
	for _, format := range Formats() {
		path := filepath.Join(dir, "absent-"+format)
		doc, err := Read(path, format)
		if err != nil {
			t.Fatalf("Read(%s) on a missing file: %v", format, err)
		}
		if doc == nil || len(doc) != 0 {
			t.Fatalf("Read(%s) on a missing file: want an empty document, got %#v", format, doc)
		}
	}
}

func TestWriteCreatesNewFileWithLF(t *testing.T) {
	for _, tc := range roundTripCases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "new"+tc.ext)
			doc := map[string]any{}
			for _, s := range tc.sets {
				mustSetPath(t, doc, s.path, s.value)
			}
			if err := Write(path, tc.format, doc, false); err != nil {
				t.Fatalf("Write: %v", err)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if strings.Contains(string(raw), "\r") {
				t.Errorf("new file should use LF, got CR in:\n%q", raw)
			}
			got, err := Read(path, tc.format)
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if canonJSON(t, got) != canonJSON(t, doc) {
				t.Errorf("new file mismatch\n got: %s\nwant: %s", canonJSON(t, got), canonJSON(t, doc))
			}
		})
	}
}

func TestCRLFIsPreserved(t *testing.T) {
	for _, tc := range roundTripCases {
		t.Run(tc.name, func(t *testing.T) {
			crlf := strings.ReplaceAll(tc.content, "\n", "\r\n")
			dir := t.TempDir()
			path := writeTemp(t, dir, "crlf"+tc.ext, crlf)

			doc := map[string]any{}
			for _, s := range tc.sets {
				mustSetPath(t, doc, s.path, s.value)
			}
			if err := Write(path, tc.format, doc, false); err != nil {
				t.Fatalf("Write: %v", err)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if n := strings.Count(string(raw), "\n"); n == 0 {
				t.Fatalf("file lost its newlines:\n%q", raw)
			} else if crlf := strings.Count(string(raw), "\r\n"); crlf != n {
				t.Errorf("want every newline to be CRLF, got %d of %d in:\n%q", crlf, n, raw)
			}
			if _, err := Read(path, tc.format); err != nil {
				t.Fatalf("Read after a CRLF write: %v", err)
			}
		})
	}
}

func TestLFIsPreserved(t *testing.T) {
	for _, tc := range roundTripCases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := writeTemp(t, dir, "lf"+tc.ext, tc.content)
			doc := map[string]any{}
			for _, s := range tc.sets {
				mustSetPath(t, doc, s.path, s.value)
			}
			if err := Write(path, tc.format, doc, false); err != nil {
				t.Fatalf("Write: %v", err)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if strings.Contains(string(raw), "\r") {
				t.Errorf("LF file must stay LF, got:\n%q", raw)
			}
		})
	}
}

func TestWriteBackupNamingAndContent(t *testing.T) {
	for _, tc := range roundTripCases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := writeTemp(t, dir, "config"+tc.ext, tc.content)
			doc := map[string]any{}
			for _, s := range tc.sets {
				mustSetPath(t, doc, s.path, s.value)
			}
			if err := Write(path, tc.format, doc, true); err != nil {
				t.Fatalf("Write with backup: %v", err)
			}
			backups, err := filepath.Glob(path + ".bak-*")
			if err != nil {
				t.Fatalf("glob: %v", err)
			}
			if len(backups) != 1 {
				t.Fatalf("want exactly one backup, got %v", backups)
			}
			if !regexp.MustCompile(`\.bak-\d{9,}$`).MatchString(backups[0]) {
				t.Errorf("backup %q is not <path>.bak-<unix seconds>", backups[0])
			}
			saved, err := os.ReadFile(backups[0])
			if err != nil {
				t.Fatalf("read backup: %v", err)
			}
			if string(saved) != tc.content {
				t.Errorf("backup content differs from the original file:\n%q", saved)
			}
			live, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read target: %v", err)
			}
			if string(live) == tc.content {
				t.Errorf("target was not updated")
			}
		})
	}
}

func TestWriteWithoutBackupLeavesNoBackup(t *testing.T) {
	dir := t.TempDir()
	path := writeTemp(t, dir, "config.json", `{"a": "1"}`)
	if err := Write(path, FormatJSON, map[string]any{"a": "2"}, false); err != nil {
		t.Fatalf("Write: %v", err)
	}
	backups, _ := filepath.Glob(path + ".bak-*")
	if len(backups) != 0 {
		t.Errorf("backup=false still produced %v", backups)
	}
}

func TestWriteIsAtomicAndLeavesNoDebris(t *testing.T) {
	t.Run("encode failure keeps the old file", func(t *testing.T) {
		dir := t.TempDir()
		const original = `{"keep": "me"}`
		path := writeTemp(t, dir, "config.json", original)
		err := Write(path, FormatJSON, map[string]any{"broken": make(chan int)}, true)
		if err == nil {
			t.Fatal("want an error when the document cannot be encoded")
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatalf("read: %v", readErr)
		}
		if string(raw) != original {
			t.Errorf("failed write modified the file: %q", raw)
		}
		assertNoTempFiles(t, dir)
	})

	t.Run("unwritable directory leaves no file", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "missing", "config.json")
		if err := Write(path, FormatJSON, map[string]any{"a": "1"}, true); err == nil {
			t.Fatal("want an error when the parent directory does not exist")
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("target should not exist, stat said: %v", err)
		}
		assertNoTempFiles(t, dir)
	})

	t.Run("invalid document is rejected before touching the file", func(t *testing.T) {
		dir := t.TempDir()
		path := writeTemp(t, dir, "config.env", "GOOD=1\n")
		if err := Write(path, FormatDotenv, map[string]any{"1BAD": "x"}, false); err == nil {
			t.Fatal("want an error for an invalid variable name")
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if string(raw) != "GOOD=1\n" {
			t.Errorf("failed write modified the file: %q", raw)
		}
		assertNoTempFiles(t, dir)
	})
}

func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	leftovers, err := filepath.Glob(filepath.Join(dir, ".ximo-plugin-*"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(leftovers) != 0 {
		t.Errorf("temporary files left behind: %v", leftovers)
	}
}

func TestWriteEmptyDocumentIsANoOp(t *testing.T) {
	dir := t.TempDir()
	const minified = `{"a":1,"b":2}`
	path := writeTemp(t, dir, "config.json", minified)
	if err := Write(path, FormatJSON, map[string]any{}, true); err != nil {
		t.Fatalf("Write: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(raw) != minified {
		t.Errorf("an empty document must not rewrite the file, got %q", raw)
	}
	assertNoTempFiles(t, dir)
}

// Write always stores the file and always takes the backup, even when the merged
// document matches what is already there: that is what the engine's plan (which
// rebuilds the whole document from the file's current content) relies on. The
// second apply of an unchanged document must therefore leave the file byte for
// byte the same as the first write's output.
func TestWriteOfAnUnchangedDocumentStillWrites(t *testing.T) {
	for _, tc := range roundTripCases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := writeTemp(t, dir, "config"+tc.ext, tc.content)

			doc := map[string]any{}
			for _, s := range tc.sets {
				mustSetPath(t, doc, s.path, s.value)
			}
			if err := Write(path, tc.format, doc, true); err != nil {
				t.Fatalf("first Write: %v", err)
			}
			afterFirst, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			firstBackups, _ := filepath.Glob(path + ".bak-*")
			if len(firstBackups) != 1 {
				t.Fatalf("first write should leave one backup, got %v", firstBackups)
			}

			// The engine builds its plan from the file's own current content.
			again, err := Read(path, tc.format)
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			for _, s := range tc.sets {
				mustSetPath(t, again, s.path, s.value)
			}
			if err := Write(path, tc.format, again, true); err != nil {
				t.Fatalf("second Write: %v", err)
			}
			afterSecond, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if string(afterSecond) != string(afterFirst) {
				t.Errorf("re-applying the same document changed the file:\n--- first ---\n%s\n--- second ---\n%s", afterFirst, afterSecond)
			}
			backups, _ := filepath.Glob(path + ".bak-*")
			if len(backups) == 0 {
				t.Fatal("the second write should leave a backup too")
			}
			raw, err := os.ReadFile(backups[len(backups)-1])
			if err != nil {
				t.Fatalf("read backup: %v", err)
			}
			if string(raw) != string(afterFirst) {
				t.Errorf("the backup of the second write should be what the file held before it:\n%q", raw)
			}
		})
	}
}

func TestPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no unix file modes; Go reports 0666/0444 only")
	}
	dir := t.TempDir()
	fresh := filepath.Join(dir, "fresh.json")
	if err := Write(fresh, FormatJSON, map[string]any{"a": "1"}, false); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if fi, err := os.Stat(fresh); err != nil {
		t.Fatalf("stat: %v", err)
	} else if fi.Mode().Perm() != 0o600 {
		t.Errorf("new file mode = %v, want 0600", fi.Mode().Perm())
	}

	existing := writeTemp(t, dir, "existing.json", `{"a": "1"}`)
	if err := os.Chmod(existing, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if err := Write(existing, FormatJSON, map[string]any{"a": "2"}, false); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if fi, err := os.Stat(existing); err != nil {
		t.Fatalf("stat: %v", err)
	} else if fi.Mode().Perm() != 0o644 {
		t.Errorf("existing file mode = %v, want 0644", fi.Mode().Perm())
	}
}

func TestBOMIsPreserved(t *testing.T) {
	dir := t.TempDir()
	path := writeTemp(t, dir, "config.json", "\ufeff{\"a\": \"1\"}")
	doc, err := Read(path, FormatJSON)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if doc["a"] != "1" {
		t.Fatalf("Read: got %#v", doc)
	}
	if err := Write(path, FormatJSON, map[string]any{"a": "2"}, false); err != nil {
		t.Fatalf("Write: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.HasPrefix(string(raw), "\ufeff") {
		t.Errorf("BOM was dropped: %q", raw)
	}
}

func TestUnsupportedFormatIsRejected(t *testing.T) {
	dir := t.TempDir()
	path := writeTemp(t, dir, "config.ini", "a=1")
	if _, err := Read(path, "ini"); err == nil {
		t.Error("Read: want an error for an unsupported format")
	}
	if err := Write(path, "ini", map[string]any{"a": "1"}, false); err == nil {
		t.Error("Write: want an error for an unsupported format")
	}
	if _, err := Diff(path, "ini", map[string]any{"a": "1"}); err == nil {
		t.Error("Diff: want an error for an unsupported format")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(raw) != "a=1" {
		t.Errorf("file changed: %q", raw)
	}
}

func TestFormatAliases(t *testing.T) {
	for input, want := range map[string]string{
		"JSON": FormatJSON, "toml": FormatTOML, "yml": FormatYAML,
		"YAML": FormatYAML, "env": FormatDotenv, " dotenv ": FormatDotenv,
	} {
		got, err := Normalize(input)
		if err != nil {
			t.Fatalf("Normalize(%q): %v", input, err)
		}
		if got != want {
			t.Errorf("Normalize(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestDirtyFilesReportTheLineNumber(t *testing.T) {
	cases := []struct {
		name    string
		format  string
		ext     string
		content string
		want    string
	}{
		{"json", FormatJSON, ".json", "{\n  \"a\": 1,\n  \"b\" 2\n}\n", "json: line 3"},
		{"toml", FormatTOML, ".toml", "model = \"x\"\nnot toml at all\n", "line 2"},
		{"dotenv", FormatDotenv, ".env", "GOOD=1\n1BAD=2\n", "line 2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := writeTemp(t, dir, "broken"+tc.ext, tc.content)
			_, err := Read(path, tc.format)
			if err == nil {
				t.Fatal("want a parse error")
			}
			if !strings.Contains(err.Error(), path) {
				t.Errorf("error should name the file %s: %v", path, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error should point at %q: %v", tc.want, err)
			}
			raw, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatalf("read: %v", readErr)
			}
			if string(raw) != tc.content {
				t.Errorf("Read modified the file: %q", raw)
			}
		})
	}

	t.Run("yaml", func(t *testing.T) {
		dir := t.TempDir()
		path := writeTemp(t, dir, "broken.yaml", "env:\n  A: 1\n  B: [1, 2\n")
		_, err := Read(path, FormatYAML)
		if err == nil {
			t.Fatal("want a parse error")
		}
		if !regexp.MustCompile(`line \d+`).MatchString(err.Error()) {
			t.Errorf("error should carry a line number: %v", err)
		}
	})

	t.Run("write refuses to patch a file it cannot parse", func(t *testing.T) {
		dir := t.TempDir()
		const broken = "{\n  \"a\": ,\n}\n"
		path := writeTemp(t, dir, "broken.json", broken)
		err := Write(path, FormatJSON, map[string]any{"a": "1"}, true)
		if err == nil {
			t.Fatal("want an error")
		}
		if !strings.Contains(err.Error(), "line") {
			t.Errorf("error should carry a line number: %v", err)
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatalf("read: %v", readErr)
		}
		if string(raw) != broken {
			t.Errorf("failed write modified the file: %q", raw)
		}
	})
}

func TestWrittenJSONKeepsAmpersandsAndNumbers(t *testing.T) {
	dir := t.TempDir()
	path := writeTemp(t, dir, "config.json", `{"url": "http://old", "ratio": 1.0, "big": 12345678901234567890}`)
	if err := Write(path, FormatJSON, map[string]any{"url": "http://gw.local/v1?a=1&b=2"}, false); err != nil {
		t.Fatalf("Write: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(raw), "a=1&b=2") {
		t.Errorf("URL was escaped: %q", raw)
	}
	for _, want := range []string{"1.0", "12345678901234567890"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("number %s was not preserved verbatim: %q", want, raw)
		}
	}
}
