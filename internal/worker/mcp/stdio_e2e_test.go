package mcp

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/worker"
)

// TestStdioToolsEndToEnd 用真实的 stdio 子进程 MCP server 验证完整链路：
// 拉起进程 → initialize 握手 → tools/list → tools/call。
//
// 这是被修 bug 的针对性回归。此前 SupervisorConfig.Factory 无人接线，每个 server
// 都会在 Connect 阶段以「未配置 TransportFactory」失败并降级（外部表现即
// 「MCP 永远连不上、工具列表恒为空」）。这条路径只有真的拉起子进程并做 JSON-RPC
// 对话才走得到 —— 使用 fake transport 的单测覆盖不到它。
//
// 环境缺 node 时跳过（CI 容器可能未安装）。
func TestStdioToolsEndToEnd(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("未找到 node，跳过 stdio 端到端测试")
	}
	script, err := filepath.Abs(filepath.Join("testdata", "mcp_echo_server.mjs"))
	if err != nil {
		t.Fatalf("解析测试脚本路径: %v", err)
	}

	sup := NewSupervisor("mcp-e2e", SupervisorConfig{
		Servers: []ServerConfig{{
			ID:             "echo",
			Name:           "echo",
			Transport:      TransportStdio,
			Enabled:        true,
			Command:        node,
			Args:           []string{script},
			RequestTimeout: 10 * time.Second,
		}},
		ConnectTimeout: 10 * time.Second,
	}, nil, nil, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if err := sup.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sup.Stop(context.Background()) }()

	waitServerReady(t, ctx, sup, "echo", 20*time.Second)

	// --- tools/list ---
	listResp := execMCP(t, ctx, sup, ActionToolsList, nil)
	if !listResp.OK() {
		t.Fatalf("tools/list 失败: %v", listResp.Error)
	}
	var listed struct {
		Tools []ToolSchema `json:"tools"`
	}
	if err := json.Unmarshal(listResp.Result, &listed); err != nil {
		t.Fatalf("解析 tools/list 结果失败: %v（原始 %s）", err, string(listResp.Result))
	}
	if len(listed.Tools) != 1 || listed.Tools[0].Name != "echo" {
		t.Fatalf("tools/list 返回 %#v，期望恰好一个名为 echo 的工具", listed.Tools)
	}
	if listed.Tools[0].ExposedName != "mcp__echo" {
		t.Errorf("ExposedName = %q，期望 mcp__echo", listed.Tools[0].ExposedName)
	}

	// --- tools/call ---
	callArgs, err := json.Marshal(map[string]any{
		"server":    "echo",
		"tool":      "echo",
		"arguments": map[string]any{"text": "你好"},
	})
	if err != nil {
		t.Fatalf("构造调用参数: %v", err)
	}
	callResp := execMCP(t, ctx, sup, ActionToolsCall, callArgs)
	if !callResp.OK() {
		t.Fatalf("tools/call 失败: %v", callResp.Error)
	}
	if !strings.Contains(string(callResp.Result), "echo: 你好") {
		t.Fatalf("tools/call 结果未包含回显内容：%s", string(callResp.Result))
	}
}

// TestStdioMissingCommandReportsClearly 验证配置不完整时给出明确错误，
// 而不是静默成功 —— 静默成功会让用户以为 MCP 已连上。
func TestStdioMissingCommandReportsClearly(t *testing.T) {
	sup := NewSupervisor("mcp-bad", SupervisorConfig{
		Servers: []ServerConfig{{
			ID: "bad", Name: "bad", Transport: TransportStdio, Enabled: true,
			// Command 故意留空
		}},
		ConnectTimeout: 3 * time.Second,
	}, nil, nil, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := sup.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sup.Stop(context.Background()) }()

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		resp := execMCP(t, ctx, sup, ActionStatus, nil)
		var st struct {
			Servers []SessionSnapshot `json:"servers"`
		}
		if err := json.Unmarshal(resp.Result, &st); err == nil {
			for _, s := range st.Servers {
				if s.ID == "bad" && s.LastError != "" {
					if !strings.Contains(s.LastError, "command") {
						t.Fatalf("错误信息未提示缺少 command：%q", s.LastError)
					}
					return
				}
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatal("缺少 command 的 server 未上报任何错误（可能被静默忽略）")
}

// ---------------------------------------------------------------------------
// 测试辅助
// ---------------------------------------------------------------------------

func execMCP(t *testing.T, ctx context.Context, sup *Supervisor, action string, args json.RawMessage) worker.WorkerResponse {
	t.Helper()
	resp, err := sup.Execute(ctx, worker.WorkerRequest{
		CallID:  "test-" + action,
		Action:  action,
		Args:    args,
		Timeout: 15 * time.Second,
	})
	if err != nil {
		t.Fatalf("Execute(%s): %v", action, err)
	}
	return resp
}

// waitServerReady 轮询 status 直到指定 server 进入 ready 状态。
func waitServerReady(t *testing.T, ctx context.Context, sup *Supervisor, id string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := "<未查到>"
	for time.Now().Before(deadline) {
		resp := execMCP(t, ctx, sup, ActionStatus, nil)
		var st struct {
			Servers []SessionSnapshot `json:"servers"`
		}
		if err := json.Unmarshal(resp.Result, &st); err == nil {
			for _, s := range st.Servers {
				if s.ID == id {
					last = s.StateName
					if s.LastError != "" {
						last += " err=" + s.LastError
					}
					if s.StateName == "ready" {
						return
					}
				}
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("MCP server %q 在 %s 内未就绪，最后状态：%s", id, timeout, last)
}
