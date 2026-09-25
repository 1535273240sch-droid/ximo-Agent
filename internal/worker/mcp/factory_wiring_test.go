package mcp

import (
	"testing"

	"github.com/ximo888ok-netizen/ximo-agent/internal/worker"
)

// TestNewFromSpecWiresTransportFactory 锁定回归：NewFromSpec 构造出的 Supervisor
// 必须带可用的 TransportFactory。
//
// 背景：SupervisorConfig.Factory 的文档承诺「为 nil 时按 transport 类型选择内置
// 实现」，但 NewSupervisor 过去并未兑现该承诺，NewFromSpec 也不传 Factory ——
// 两者叠加的结果是每个 MCP server 都在 Connect 阶段以「未配置 TransportFactory」
// 失败并降级，外部表现正是「MCP 服务器永远连不上、tools/list 恒为空」，
// 而内置的 DefaultTransportFactory 从未被任何生产路径调用过。
func TestNewFromSpecWiresTransportFactory(t *testing.T) {
	w, err := NewFromSpec(worker.Spec{ID: "mcp-0", Kind: "mcp"})
	if err != nil {
		t.Fatalf("NewFromSpec: %v", err)
	}
	sup, ok := w.(*Supervisor)
	if !ok {
		t.Fatalf("NewFromSpec 返回 %T，期望 *Supervisor", w)
	}
	if sup.factory == nil {
		t.Fatal("Supervisor 的 TransportFactory 为 nil：所有 MCP server 都会以" +
			"「未配置 TransportFactory」连接失败，工具列表恒为空")
	}
}

// TestNewSupervisorDefaultsTransportFactory 验证即使直接调用 NewSupervisor
// 且不传 Factory，也会回退到内置工厂（兑现配置结构体上的文档承诺）。
func TestNewSupervisorDefaultsTransportFactory(t *testing.T) {
	sup := NewSupervisor("mcp-direct", SupervisorConfig{}, nil, nil, nil)
	if sup.factory == nil {
		t.Fatal("NewSupervisor 未为 nil Factory 提供内置回退实现")
	}
}

// TestNewFromSpecParsesServers 验证 spec.Config["servers"] 被正确解析成会话。
//
// 这条链路是 bootstrap 注入 MCP 配置的唯一入口：workerPoolConfig 把
// config.MCPServerConfig 转成 []map[string]any 塞进 Config["servers"]，
// 这里必须能解出与之一致的会话数。
func TestNewFromSpecParsesServers(t *testing.T) {
	w, err := NewFromSpec(worker.Spec{
		ID:   "mcp-0",
		Kind: "mcp",
		Config: map[string]any{
			"servers": []map[string]any{
				{"id": "demo", "name": "demo", "transport": "stdio", "command": "node", "enabled": true},
				{"id": "remote", "name": "remote", "enabled": true, "url": "https://example.com/mcp"},
			},
		},
	})
	if err != nil {
		t.Fatalf("NewFromSpec: %v", err)
	}
	sup, ok := w.(*Supervisor)
	if !ok {
		t.Fatalf("NewFromSpec 返回 %T，期望 *Supervisor", w)
	}
	if len(sup.sessions) != 2 {
		t.Fatalf("会话数 = %d，期望 2（servers 未被完整解析）", len(sup.sessions))
	}
	for _, id := range []string{"demo", "remote"} {
		if _, ok := sup.sessions[id]; !ok {
			t.Errorf("未找到 id=%q 的会话", id)
		}
	}
}

// TestNewFromSpecSkipsDisabledServers 验证显式 enabled=false 的服务器不建会话。
//
// 这条与 bootstrap 的转换互补：config 层把「未写 enabled」归一成 true，
// 只有用户显式写 false 才应被跳过。
func TestNewFromSpecSkipsDisabledServers(t *testing.T) {
	w, err := NewFromSpec(worker.Spec{
		ID:   "mcp-0",
		Kind: "mcp",
		Config: map[string]any{
			"servers": []map[string]any{
				{"id": "on", "name": "on", "command": "node", "enabled": true},
				{"id": "off", "name": "off", "command": "node", "enabled": false},
			},
		},
	})
	if err != nil {
		t.Fatalf("NewFromSpec: %v", err)
	}
	sup := w.(*Supervisor)
	if len(sup.sessions) != 1 {
		t.Fatalf("会话数 = %d，期望 1（enabled=false 的服务器必须被跳过）", len(sup.sessions))
	}
	if _, ok := sup.sessions["off"]; ok {
		t.Error("enabled=false 的服务器仍被建了会话")
	}
}
