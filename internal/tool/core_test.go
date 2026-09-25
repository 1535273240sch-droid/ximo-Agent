package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ximo888ok-netizen/ximo-agent/internal/secrets"
)

// ---------------------------------------------------------------------------
// schema
// ---------------------------------------------------------------------------

func TestValidateArguments(t *testing.T) {
	schema := ObjectSchema(map[string]JSONSchema{
		"path":   {Type: "string"},
		"count":  {Type: "integer"},
		"mode":   {Type: "string", Enum: []any{"a", "b"}},
		"tags":   {Type: "array", Items: &JSONSchema{Type: "string"}},
		"nested": {Type: "object", Properties: map[string]JSONSchema{"x": {Type: "number"}}, Required: []string{"x"}},
	}, "path")

	cases := []struct {
		name    string
		args    map[string]any
		wantErr bool
	}{
		{"valid", map[string]any{"path": "/a", "count": 3, "mode": "a", "tags": []any{"x"}, "nested": map[string]any{"x": 1.5}}, false},
		{"missing_required", map[string]any{}, true},
		{"wrong_type", map[string]any{"path": 123}, true},
		{"enum_out_of_range", map[string]any{"path": "/a", "mode": "c"}, true},
		{"array_item_type", map[string]any{"path": "/a", "tags": []any{1}}, true},
		{"nested_missing_field", map[string]any{"path": "/a", "nested": map[string]any{}}, true},
		{"nested_field_type", map[string]any{"path": "/a", "nested": map[string]any{"x": "s"}}, true},
		{"extra_field_allowed", map[string]any{"path": "/a", "extra": true}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateArguments(schema, c.args)
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v，wantErr = %v", err, c.wantErr)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// registry
// ---------------------------------------------------------------------------

func TestRegistryRegisterGetReplace(t *testing.T) {
	reg := NewRegistry()
	tool := &countingTool{def: ToolDefinition{Name: "file_read"}}
	if err := reg.Register(tool); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(tool); err == nil {
		t.Fatalf("重复注册应当报错")
	}
	if !reg.Has("file_read") {
		t.Fatalf("Has 失败")
	}
	got, ok := reg.Get("file_read")
	if !ok || got.Definition().Name != "file_read" {
		t.Fatalf("Get 失败")
	}
	replacement := &countingTool{def: ToolDefinition{Name: "file_read", Description: "v2"}}
	if err := reg.Replace(replacement); err != nil {
		t.Fatal(err)
	}
	got, _ = reg.Get("file_read")
	if got.Definition().Description != "v2" {
		t.Fatalf("Replace 未生效")
	}
	if reg.Len() != 1 {
		t.Fatalf("Len = %d", reg.Len())
	}
	if names := reg.Names(); len(names) != 1 || names[0] != "file_read" {
		t.Fatalf("Names = %v", names)
	}
}

func TestRegistryRejectsInvalid(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Register(nil); err == nil {
		t.Fatalf("nil 工具应当报错")
	}
	empty := &countingTool{def: ToolDefinition{Name: ""}}
	if err := reg.Register(empty); err == nil {
		t.Fatalf("空名工具应当报错")
	}
}

func TestLazyRegistryLoadsGroupsOnce(t *testing.T) {
	reg := NewLazyRegistry()
	var factoryCalls int
	reg.RegisterGroup(GroupFileSystem, func() ([]Tool, error) {
		factoryCalls++
		return []Tool{
			&countingTool{def: ToolDefinition{Name: "file_read"}},
			&countingTool{def: ToolDefinition{Name: "file_write"}},
		}, nil
	})
	if err := reg.EnsureGroups(GroupFileSystem); err != nil {
		t.Fatal(err)
	}
	if err := reg.EnsureGroups(GroupFileSystem); err != nil {
		t.Fatal(err)
	}
	if factoryCalls != 1 {
		t.Fatalf("工厂被执行了 %d 次，懒加载失效", factoryCalls)
	}
	if !reg.Has("file_read") || !reg.Has("file_write") {
		t.Fatalf("组内工具未注册")
	}
	if err := reg.EnsureGroups(ToolGroup("unknown")); err == nil {
		t.Fatalf("未知组应当报错")
	}
}

func TestLazyRegistryConcurrentEnsure(t *testing.T) {
	reg := NewLazyRegistry()
	var calls int
	reg.RegisterGroup(GroupGit, func() ([]Tool, error) {
		calls++
		return []Tool{&countingTool{def: ToolDefinition{Name: "git_status"}}}, nil
	})
	done := make(chan error, 16)
	for i := 0; i < 16; i++ {
		go func() { done <- reg.EnsureGroups(GroupGit) }()
	}
	for i := 0; i < 16; i++ {
		if err := <-done; err != nil {
			t.Fatalf("并发 EnsureGroups 失败: %v", err)
		}
	}
	if calls != 1 {
		t.Fatalf("并发下工厂执行了 %d 次", calls)
	}
}

func TestLazyRegistryEnsureMode(t *testing.T) {
	reg := NewLazyRegistry()
	for _, group := range []ToolGroup{GroupFileSystem, GroupGit, GroupKnowledge, GroupWeb, GroupTerminal, GroupSkill, GroupPlanSpec} {
		g := group
		reg.RegisterGroup(g, func() ([]Tool, error) {
			return []Tool{&countingTool{def: ToolDefinition{Name: string(g) + "_tool"}}}, nil
		})
	}
	if err := reg.EnsureModeTools(ModeCoding); err != nil {
		t.Fatal(err)
	}
	if !reg.Has("git_tool") {
		t.Fatalf("coding 模式的 git 组未加载")
	}
}

// ---------------------------------------------------------------------------
// audit / 事件脱敏（I12）
// ---------------------------------------------------------------------------

func TestAuditLoggerRedactsPayload(t *testing.T) {
	sink := newTestEventSink()
	redactor := secrets.NewRedactor()
	redactor.Track("secretref:v1:abc", "sk-live-abcdef123456")
	audit := NewAuditLogger(sink, redactor)

	rec := AuditRecord{
		RunID:      "run-1",
		ToolCallID: "call-1",
		ToolName:   "web_fetch",
		Event:      AuditToolCompleted,
		Detail: map[string]any{
			"note":    "用了 key sk-live-abcdef123456 调用",
			"api_key": "sk-live-abcdef123456",
			"token":   "sk-live-abcdef123456",
		},
	}
	if err := audit.Log(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	events := sink.events
	if len(events) != 1 {
		t.Fatalf("事件数量 = %d", len(events))
	}
	payload := string(events[0].payload)
	if strings.Contains(payload, "sk-live-abcdef123456") {
		t.Fatalf("I12 违反：审计 payload 含明文秘密: %s", payload)
	}
	if !strings.Contains(payload, "REDACTED") {
		t.Fatalf("payload 未包含脱敏标记: %s", payload)
	}
}

func TestEmitEventRedacts(t *testing.T) {
	sink := newTestEventSink()
	redactor := secrets.NewRedactor()
	redactor.Track("secretref:v1:x", "topsecret-value")
	if err := EmitEvent(context.Background(), sink, redactor, "run-1", "tool.completed", map[string]any{
		"content": "result with topsecret-value inside",
	}); err != nil {
		t.Fatal(err)
	}
	events := sink.events
	if len(events) != 1 || strings.Contains(string(events[0].payload), "topsecret-value") {
		t.Fatalf("I12 违反：事件 payload 含秘密: %s", events[0].payload)
	}
}

func TestJSONRoundTripResponse(t *testing.T) {
	resp := ToolResponse{ToolCallID: "c", ToolName: "t", Content: "x", Success: true}
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var back ToolResponse
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if back.Content != "x" || back.ToolCallID != "c" {
		t.Fatalf("往返失败: %+v", back)
	}
}
