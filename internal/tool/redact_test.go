package tool

import (
	"context"
	"strings"
	"testing"
)

// TestBasicRedactorFiltersWithoutInjection 是审核报告 B-1 的回归测试：
// 未注入 Redactor 时也绝不明文放行（fail-closed）。
func TestBasicRedactorFiltersWithoutInjection(t *testing.T) {
	r := NewBasicRedactor()

	// 高置信度模式
	secrets := []string{
		"sk-abcdefghijklmnopqrstuvwxyz012345",
		"AKIAIOSFODNN7EXAMPLE",
		"AIzaSyA1234567890abcdefghijklmnopqrstuv",
		"ghp_1234567890abcdefghijklmnopqrstuvwx",
		"-----BEGIN RSA PRIVATE KEY-----",
	}
	for _, s := range secrets {
		if got := r.RedactString("prefix " + s + " suffix"); strings.Contains(got, s) {
			t.Fatalf("内置模式未过滤 %q: %q", s, got)
		}
	}

	// 敏感键名
	value := r.RedactValue(map[string]any{
		"api_key":  "whatever",
		"token":    "whatever",
		"password": "whatever",
		"note":     "visible",
	})
	m := value.(map[string]any)
	if m["api_key"] != redactedPlaceholder || m["token"] != redactedPlaceholder || m["password"] != redactedPlaceholder {
		t.Fatalf("敏感键名未脱敏: %v", m)
	}
	if m["note"] != "visible" {
		t.Fatalf("非敏感键被误伤: %v", m)
	}

	// JSON 路径（含 struct 往返）
	data, err := r.RedactJSON([]byte(`{"content":"key=sk-abcdefghijklmnopqrstuvwxyz012345","Authorization":"Bearer x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "sk-abcdefghijklmnopqrstuvwxyz012345") {
		t.Fatalf("JSON 路径未过滤: %s", data)
	}
}

func TestAuditLoggerNilRedactorStillFilters(t *testing.T) {
	sink := newTestEventSink()
	// 不注入 Redactor（模拟 08 号拼装时漏配）
	audit := NewAuditLogger(sink, nil)
	err := audit.Log(context.Background(), AuditRecord{
		RunID:      "run-1",
		ToolCallID: "call-1",
		ToolName:   "web_fetch",
		Event:      AuditToolCompleted,
		Detail: map[string]any{
			"note":    "used sk-abcdefghijklmnopqrstuvwxyz012345",
			"api_key": "sk-abcdefghijklmnopqrstuvwxyz012345",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	events := sink.events
	if len(events) != 1 {
		t.Fatalf("事件数 = %d", len(events))
	}
	payload := string(events[0].payload)
	if strings.Contains(payload, "sk-abcdefghijklmnopqrstuvwxyz012345") {
		t.Fatalf("B-1 回归失败：nil Redactor 下秘密明文进入审计 payload: %s", payload)
	}
	if !strings.Contains(payload, redactedPlaceholder) {
		t.Fatalf("未出现脱敏标记: %s", payload)
	}
}

func TestEmitEventNilRedactorStillFilters(t *testing.T) {
	sink := newTestEventSink()
	err := EmitEvent(context.Background(), sink, nil, "run-1", "tool.completed", map[string]any{
		"content": "result with AKIAIOSFODNN7EXAMPLE inside",
	})
	if err != nil {
		t.Fatal(err)
	}
	events := sink.events
	if len(events) != 1 || strings.Contains(string(events[0].payload), "AKIAIOSFODNN7EXAMPLE") {
		t.Fatalf("B-1 回归失败：nil Redactor 下事件未脱敏: %s", events[0].payload)
	}
}

func TestRuntimeNilRedactorCommitStillFilters(t *testing.T) {
	f := newFixture(t)
	// 不注入 Redactor
	tool := &countingTool{def: readToolDef(), response: func(req ToolRequest) ToolResponse {
		return ToolResponse{ToolCallID: req.ToolCallID, ToolName: "file_read", Content: "token sk-abcdefghijklmnopqrstuvwxyz012345", Success: true}
	}}
	f.register(t, tool)
	f.resolverAdd(t, "call-1", "file_read", map[string]any{"filePath": "/tmp/a"})

	resp := f.rt.Execute(context.Background(), ToolRequest{
		RunID: "run-1", ToolCallID: "call-1", Name: "file_read",
		Arguments: map[string]any{"filePath": "/tmp/a"}, Mode: ModeCoding,
	})
	if !resp.Success {
		t.Fatalf("执行失败: %+v", resp)
	}
	_, result, found, _ := f.idem.Get(context.Background(), Key("run-1", "call-1"))
	if !found {
		t.Fatalf("缺少持久化结果")
	}
	if strings.Contains(string(result), "sk-abcdefghijklmnopqrstuvwxyz012345") {
		t.Fatalf("B-1 回归失败：nil Redactor 下落库结果含明文: %s", result)
	}
}

func TestBasicRedactorStructViaJSON(t *testing.T) {
	r := NewBasicRedactor()
	type payload struct {
		APIKey string `json:"api_key"`
		Note   string `json:"note"`
	}
	value := r.RedactValue(payload{APIKey: "sk-abcdefghijklmnopqrstuvwxyz012345", Note: "keep"})
	m, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("struct 应经 JSON 往返转为 map，得到 %T", value)
	}
	if m["api_key"] != redactedPlaceholder {
		t.Fatalf("struct 敏感字段未脱敏: %v", m)
	}
	if m["note"] != "keep" {
		t.Fatalf("struct 普通字段被误伤: %v", m)
	}
}
