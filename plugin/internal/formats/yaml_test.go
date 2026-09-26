package formats

import (
	"os"
	"strings"
	"testing"
)

const yamlFixture = `# top of file
model: old-model
env:
  # the token below must stay
  ANTHROPIC_AUTH_TOKEN: "sk-keep"
  ANTHROPIC_BASE_URL: http://old.example
other:
  keep: true
`

func TestYAMLKeepsCommentsAndKeyOrder(t *testing.T) {
	dir := t.TempDir()
	path := writeTemp(t, dir, "config.yaml", yamlFixture)
	if err := Write(path, FormatYAML, map[string]any{
		"env": map[string]any{"ANTHROPIC_BASE_URL": "http://gw.local/v1"},
	}, false); err != nil {
		t.Fatalf("Write: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	text := string(raw)
	for _, want := range []string{
		"# top of file",
		"# the token below must stay",
		`ANTHROPIC_AUTH_TOKEN: "sk-keep"`,
		"ANTHROPIC_BASE_URL: http://gw.local/v1",
		"keep: true",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	if !inOrder(text, "model: old-model", "env:", "ANTHROPIC_BASE_URL", "other:") {
		t.Errorf("key order changed:\n%s", text)
	}
}

func TestYAMLKeepsALineCommentOnTheEditedKey(t *testing.T) {
	dir := t.TempDir()
	path := writeTemp(t, dir, "config.yaml", "model: old # the model we use\n")
	if err := Write(path, FormatYAML, map[string]any{"model": "new"}, false); err != nil {
		t.Fatalf("Write: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(raw), "model: new # the model we use") {
		t.Errorf("the trailing comment was lost:\n%s", raw)
	}
}

func TestYAMLSequenceElementsMergeByIndex(t *testing.T) {
	dir := t.TempDir()
	content := "models:\n" +
		"  - name: first\n" +
		"    api_base: http://old\n" +
		"    extra: true\n" +
		"  - name: second\n"
	path := writeTemp(t, dir, "config.yaml", content)
	if err := Write(path, FormatYAML, map[string]any{
		"models": []any{map[string]any{"api_base": "http://gw"}},
	}, false); err != nil {
		t.Fatalf("Write: %v", err)
	}
	doc, err := Read(path, FormatYAML)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	models, ok := doc["models"].([]any)
	if !ok || len(models) != 2 {
		t.Fatalf("models = %#v", doc["models"])
	}
	first, _ := models[0].(map[string]any)
	if first["api_base"] != "http://gw" || first["name"] != "first" || first["extra"] != true {
		t.Errorf("models[0] = %#v", first)
	}
	second, _ := models[1].(map[string]any)
	if second["name"] != "second" {
		t.Errorf("models[1] = %#v", second)
	}
}

func TestYAMLRejectsANonMappingRoot(t *testing.T) {
	dir := t.TempDir()
	path := writeTemp(t, dir, "config.yaml", "- a\n- b\n")
	err := Write(path, FormatYAML, map[string]any{"model": "x"}, false)
	if err == nil {
		t.Fatal("want an error for a sequence root")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error should name the file: %v", err)
	}
	raw, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read: %v", readErr)
	}
	if string(raw) != "- a\n- b\n" {
		t.Errorf("failed write modified the file: %q", raw)
	}
}

func TestYAMLRendererRefusesANonMappingRoot(t *testing.T) {
	// The parse step usually rejects this first, so the renderer's own guard is
	// checked directly.
	_, err := (yamlCodec{}).render(map[string]any{"model": "x"}, []byte("- a\n"))
	if err == nil {
		t.Fatal("want an error for a sequence root")
	}
	if !strings.Contains(err.Error(), "expected a mapping") {
		t.Errorf("error = %v", err)
	}
}

func TestYAMLNewFileAndNestedCreation(t *testing.T) {
	dir := t.TempDir()
	path := writeTemp(t, dir, "config.yaml", "model: old\n")
	doc := map[string]any{}
	mustSetPath(t, doc, "env.ANTHROPIC_BASE_URL", "http://gw.local/v1")
	if err := Write(path, FormatYAML, doc, false); err != nil {
		t.Fatalf("Write: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(raw), "env:\n  ANTHROPIC_BASE_URL: http://gw.local/v1") {
		t.Errorf("nested mapping was not created with 2-space indent:\n%s", raw)
	}
	if !strings.Contains(string(raw), "model: old") {
		t.Errorf("existing key was lost:\n%s", raw)
	}
}

func inOrder(text string, needles ...string) bool {
	at := 0
	for _, needle := range needles {
		i := strings.Index(text[at:], needle)
		if i < 0 {
			return false
		}
		at += i + len(needle)
	}
	return true
}
