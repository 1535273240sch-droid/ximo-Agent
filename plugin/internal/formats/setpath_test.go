package formats

import (
	"strings"
	"testing"
)

func TestSetPathCreatesMiddleLevels(t *testing.T) {
	doc := map[string]any{}
	mustSetPath(t, doc, "a.b.c", "deep")
	mustSetPath(t, doc, "a.b.d", 7)
	mustSetPath(t, doc, "top", "value")

	inner, ok := doc["a"].(map[string]any)
	if !ok {
		t.Fatalf("a should be an object, got %#v", doc["a"])
	}
	leaf, ok := inner["b"].(map[string]any)
	if !ok {
		t.Fatalf("a.b should be an object, got %#v", inner["b"])
	}
	if leaf["c"] != "deep" || leaf["d"] != 7 {
		t.Errorf("a.b = %#v", leaf)
	}
	if doc["top"] != "value" {
		t.Errorf("top = %#v", doc["top"])
	}
}

func TestSetPathArrayIndex(t *testing.T) {
	doc := map[string]any{}
	mustSetPath(t, doc, "models.0.api_base", "http://one")
	mustSetPath(t, doc, "models.1.api_base", "http://two")
	mustSetPath(t, doc, "models.1.name", "second")

	models, ok := doc["models"].([]any)
	if !ok {
		t.Fatalf("models should be an array, got %#v", doc["models"])
	}
	if len(models) != 2 {
		t.Fatalf("models has %d entries: %#v", len(models), models)
	}
	first, ok := models[0].(map[string]any)
	if !ok {
		t.Fatalf("models[0] should be an object, got %#v", models[0])
	}
	if first["api_base"] != "http://one" {
		t.Errorf("models[0] = %#v", first)
	}
	second, ok := models[1].(map[string]any)
	if !ok {
		t.Fatalf("models[1] should be an object, got %#v", models[1])
	}
	if second["api_base"] != "http://two" || second["name"] != "second" {
		t.Errorf("models[1] = %#v", second)
	}
}

func TestSetPathMergesIntoExistingStructures(t *testing.T) {
	doc := map[string]any{
		"models": []any{
			map[string]any{"name": "keep", "api_base": "old"},
			map[string]any{"name": "second"},
		},
	}
	mustSetPath(t, doc, "models.0.api_base", "new")

	models := doc["models"].([]any)
	if len(models) != 2 {
		t.Fatalf("models should keep both entries: %#v", models)
	}
	first := models[0].(map[string]any)
	if first["name"] != "keep" || first["api_base"] != "new" {
		t.Errorf("models[0] = %#v", first)
	}
}

func TestSetPathAppendsAtTheEndOfAnArray(t *testing.T) {
	doc := map[string]any{"models": []any{map[string]any{"name": "first"}}}
	mustSetPath(t, doc, "models.1.name", "second")
	models := doc["models"].([]any)
	if len(models) != 2 {
		t.Fatalf("models = %#v", models)
	}
	if models[1].(map[string]any)["name"] != "second" {
		t.Errorf("models[1] = %#v", models[1])
	}
}

func TestSetPathDigitsWithLeadingZeroAreKeys(t *testing.T) {
	doc := map[string]any{}
	mustSetPath(t, doc, "policy.01", "audit")
	policy, ok := doc["policy"].(map[string]any)
	if !ok {
		t.Fatalf("policy should be an object, got %#v", doc["policy"])
	}
	if policy["01"] != "audit" {
		t.Errorf("policy = %#v", policy)
	}
	mustSetPath(t, doc, "list.0", "first")
	if list, ok := doc["list"].([]any); !ok || len(list) != 1 {
		t.Errorf("list = %#v", doc["list"])
	}
}

func TestSetPathTypeConflicts(t *testing.T) {
	cases := []struct {
		name string
		doc  map[string]any
		path string
		want string
	}{
		{
			name: "descend through a string",
			doc:  map[string]any{"a": "scalar"},
			path: "a.b",
			want: `"a" is a string, not an object`,
		},
		{
			name: "descend through a number",
			doc:  map[string]any{"a": map[string]any{"b": 1}},
			path: "a.b.c",
			want: `"b" is a number, not an object`,
		},
		{
			name: "index into an object",
			doc:  map[string]any{"models": map[string]any{"name": "x"}},
			path: "models.0.name",
			want: `"models" is an object, not an array`,
		},
		{
			name: "key inside an array",
			doc:  map[string]any{"models": []any{}},
			path: "models.name.x",
			want: `"models" is an array, not an object`,
		},
		{
			name: "index the document root",
			doc:  map[string]any{},
			path: "0.name",
			want: "not an array",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := SetPath(tc.doc, tc.path, "boom")
			if err == nil {
				t.Fatal("want an error")
			}
			if !strings.Contains(err.Error(), tc.path) {
				t.Errorf("error should name the path %q: %v", tc.path, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error should explain the conflict (%q): %v", tc.want, err)
			}
		})
	}
}

func TestSetPathRejectsBadPaths(t *testing.T) {
	doc := map[string]any{}
	for _, path := range []string{"", "  ", "a..b", ".a", "a."} {
		if err := SetPath(doc, path, 1); err == nil {
			t.Errorf("SetPath(%q) should fail", path)
		}
	}
	if err := SetPath(nil, "a", 1); err == nil {
		t.Error("SetPath with a nil document should fail")
	}
}

func TestSetPathRefusesAnAbsurdIndex(t *testing.T) {
	doc := map[string]any{}
	err := SetPath(doc, "models.100000.name", "x")
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "out of range") {
		t.Errorf("error should mention the range: %v", err)
	}
	if _, exists := doc["models"]; exists {
		t.Errorf("nothing should have been created: %#v", doc)
	}
}
