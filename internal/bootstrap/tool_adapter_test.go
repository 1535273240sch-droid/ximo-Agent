package bootstrap

import (
	"context"
	"testing"

	"github.com/ximo888ok-netizen/ximo-agent/internal/ports"
	"github.com/ximo888ok-netizen/ximo-agent/internal/tool"
)

// F5 的生产链路握手：权限层 → 适配器 → 引擎。
//
// 这条链路上有三个不同的类型各说各话，而它们之间只靠字符串键连接：
//
//	internal/tool.ToolResponse.RequiresConfirmation / ConfirmationMessage
//	  → bootstrap/tool_adapter.go 复制到 types.ToolResult.Metadata
//	  → internal/engine 的 toolOutcomeFor 从 Metadata 里读出来
//
// 任何一环改名，编译期都不会报错，表现却是「Agent 想执行需确认的工具时界面
// 没有任何按钮、run 永久停在 waiting_user」（审核报告 C4）。所以这个测试直接
// 断言那两个字面量键，并覆盖批准后真正执行的另一半。

// fakeInnerRuntime 扮演适配器的内层工具运行时，记录它收到的内部请求形状
// （确认字段在 ToolRequest 上，这正是要被验证的那一半）。
type fakeInnerRuntime struct {
	calls []tool.ToolRequest
	resp  tool.ToolResponse
}

func (f *fakeInnerRuntime) Execute(_ context.Context, req tool.ToolRequest) tool.ToolResponse {
	f.calls = append(f.calls, req)
	return f.resp
}

// TestToolAdapterRelaysConfirmationRequestToEngineMetadata 断言「需要确认」这件事
// 从权限层一路走到引擎读得到的位置。
func TestToolAdapterRelaysConfirmationRequestToEngineMetadata(t *testing.T) {
	inner := &fakeInnerRuntime{resp: tool.ToolResponse{
		ToolCallID: "call-1", ToolName: "file_delete", Success: false,
		Error:                "path /tmp/x needs confirmation",
		ErrorCode:            tool.ErrNeedsConfirmation,
		RequiresConfirmation: true,
		ConfirmationMessage:  "path /tmp/x needs confirmation",
	}}
	adapter := &toolRuntimeAdapter{rt: inner, defaultMode: tool.ModeSafe}

	res, err := adapter.Execute(context.Background(), ports.ToolRequest{
		RunID: "run-1", SessionID: "ses-1", ToolCallID: "call-1",
		ToolName: "file_delete", Arguments: map[string]any{"filePath": "/tmp/x"},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Success {
		t.Fatal("needs-confirmation must not be a success result")
	}
	if got := res.Metadata["requiresConfirmation"]; got != true {
		t.Fatalf("metadata[requiresConfirmation] = %v, want true (this is the key the engine parks on)", got)
	}
	if got, _ := res.Metadata["confirmationMessage"].(string); got == "" {
		t.Fatal("metadata[confirmationMessage] is empty: the UI cannot say what is about to run")
	}
}

// TestToolAdapterIgnoresConfirmationFlagOnPlainFailure 保证普通失败不会被误判成
// 「需要人工确认」——否则每个失败的调用都会把 run 停住等一个没有意义的决定。
func TestToolAdapterIgnoresConfirmationFlagOnPlainFailure(t *testing.T) {
	inner := &fakeInnerRuntime{resp: tool.ToolResponse{
		ToolCallID: "call-1", ToolName: "file_read", Success: false,
		Error: "no such file", ErrorCode: tool.ErrToolFailed,
	}}
	adapter := &toolRuntimeAdapter{rt: inner, defaultMode: tool.ModeSafe}

	res, err := adapter.Execute(context.Background(), ports.ToolRequest{
		RunID: "run-1", ToolCallID: "call-1", ToolName: "file_read",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if _, present := res.Metadata["requiresConfirmation"]; present {
		t.Fatalf("a plain failure must not carry requiresConfirmation: %v", res.Metadata)
	}
}

// TestToolAdapterPassesApprovalDownToPermission 断言用户的批准真的被送到权限层。
//
// 缺少这一半，run 会永远停在 waiting_user：权限层每次都回答「需要确认」，
// 而引擎每次都问同一个问题。
func TestToolAdapterPassesApprovalDownToPermission(t *testing.T) {
	inner := &fakeInnerRuntime{resp: tool.ToolResponse{
		ToolCallID: "call-1", ToolName: "file_delete", Success: true, Content: "deleted",
	}}
	adapter := &toolRuntimeAdapter{rt: inner, defaultMode: tool.ModeSafe}

	if _, err := adapter.Execute(context.Background(), ports.ToolRequest{
		RunID: "run-1", ToolCallID: "call-1", ToolName: "file_delete",
		Confirmed: true,
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(inner.calls) != 1 {
		t.Fatalf("inner runtime called %d times, want 1", len(inner.calls))
	}
	got := inner.calls[0].Confirmation
	if !got.Confirmed {
		t.Fatal("the approval never reached the permission layer: Confirmation.Confirmed is false")
	}
	// A one-off approval must use ScopeSingle: the permission layer's session
	// scope is bound to a specific rule ID that the UI never sees, so using
	// ScopeSession here would silently make the approval ineffective.
	if got.Scope != tool.ScopeSingle {
		t.Fatalf("confirmation scope = %v, want ScopeSingle", got.Scope)
	}

	// Without an approval the adapter must not invent one.
	inner.calls = nil
	if _, err := adapter.Execute(context.Background(), ports.ToolRequest{
		RunID: "run-1", ToolCallID: "call-2", ToolName: "file_delete",
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if inner.calls[0].Confirmation.Confirmed {
		t.Fatal("an unapproved call was given Confirmed=true, which bypasses the permission layer")
	}
}
