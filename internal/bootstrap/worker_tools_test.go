package bootstrap

import (
	"testing"

	"github.com/ximo888ok-netizen/ximo-agent/internal/config"
	"github.com/ximo888ok-netizen/ximo-agent/internal/tool"
	"github.com/ximo888ok-netizen/ximo-agent/internal/worker"
)

// TestWorkerPoolConfigInjectsWorkspaceRoot 锁定回归：每个 Worker 池都必须拿到
// allowed_roots。
//
// 背景：terminal / office / dynamic-js 的路径校验一律以 allowed_roots 为边界，
// 且全部 fail-closed —— 为空时不是"不限制"而是"一律拒绝"（terminal 甚至连
// resolveWorkdir 都过不去，每条命令都返回 no_allowed_roots）。少了注入，
// 表现就是「池起来了、工具也注册了，但每次调用都被策略拒绝」。
func TestWorkerPoolConfigInjectsWorkspaceRoot(t *testing.T) {
	root := `C:\ws\demo`
	for _, kind := range []string{"terminal", "office", "dynamic-js", "browser", "mcp", "computer-use"} {
		conf := workerPoolConfig(&config.Config{}, kind, root)
		roots, ok := conf["allowed_roots"].([]string)
		if !ok || len(roots) != 1 || roots[0] != root {
			t.Errorf("kind=%s 的 allowed_roots = %#v，期望 [%s]", kind, conf["allowed_roots"], root)
		}
	}
}

// TestWorkerPoolConfigPassesMCPServers 验证 mcp_servers 被转成 Worker 可解析的形态，
// 且「未写 enabled」被归一成 true。
//
// 必要性：worker 侧 mcp.ServerConfig.Enabled 是普通 bool 且默认 false，
// NewSupervisor 会直接跳过 enabled=false 的服务器。若 config 层把零值原样透传，
// 用户在配置里写下的每一个 MCP 服务器都会被静默忽略，且没有任何报错。
func TestWorkerPoolConfigPassesMCPServers(t *testing.T) {
	cfg := &config.Config{
		MCPServers: []config.MCPServerConfig{
			{Name: "demo", Command: "node", Args: []string{"srv.js"}},
		},
	}
	conf := workerPoolConfig(cfg, "mcp", `C:\ws`)
	servers, ok := conf["servers"].([]map[string]any)
	if !ok || len(servers) != 1 {
		t.Fatalf("servers = %#v，期望 1 项", conf["servers"])
	}
	if servers[0]["enabled"] != true {
		t.Errorf("enabled = %#v，期望 true（未显式配置时应视为启用）", servers[0]["enabled"])
	}
	if servers[0]["command"] != "node" {
		t.Errorf("command = %#v，期望 node", servers[0]["command"])
	}
}

// TestWorkerPoolConfigHonoursExplicitDisable 验证显式 enabled=false 会被保留，
// 避免「归一化」把用户的关闭意图也一起抹掉。
func TestWorkerPoolConfigHonoursExplicitDisable(t *testing.T) {
	off := false
	cfg := &config.Config{
		MCPServers: []config.MCPServerConfig{{Name: "demo", Command: "node", Enabled: &off}},
	}
	conf := workerPoolConfig(cfg, "mcp", `C:\ws`)
	servers := conf["servers"].([]map[string]any)
	if servers[0]["enabled"] != false {
		t.Errorf("enabled = %#v，期望 false（用户显式关闭必须保留）", servers[0]["enabled"])
	}
}

// TestRegisterWorkerToolsOnlyEnabledKinds 锁定：只为真正启用的 kind 注册工具。
//
// 必要性：注册了工具但对应 Worker 池没启动，模型会看到一批"看起来能调、
// 实际必定失败"的假能力，既浪费上下文也误导决策。
func TestRegisterWorkerToolsOnlyEnabledKinds(t *testing.T) {
	mgr := worker.NewManager(worker.ManagerConfig{})
	reg := tool.NewRegistry()
	if err := registerWorkerTools(reg, mgr, map[string]bool{"terminal": true, "mcp": true}); err != nil {
		t.Fatalf("registerWorkerTools: %v", err)
	}

	names := map[string]bool{}
	for _, n := range reg.Names() {
		names[n] = true
	}

	mustHave := []string{"terminal_exec", "mcp_list_tools", "mcp_call_tool", "mcp_status", "mcp_refresh_tools"}
	for _, n := range mustHave {
		if !names[n] {
			t.Errorf("缺少工具 %s（其 kind 已启用，必须注册）", n)
		}
	}

	mustNotHave := []string{
		"browser_navigate", "browser_screenshot", "dynamic_eval",
		"office_docs", "computer_screenshot", "computer_key_press",
	}
	for _, n := range mustNotHave {
		if names[n] {
			t.Errorf("不该注册工具 %s（其 kind 未启用）", n)
		}
	}
}

// TestRegisterWorkerToolsNilManager 验证没有 Worker 池时静默跳过且不 panic。
func TestRegisterWorkerToolsNilManager(t *testing.T) {
	reg := tool.NewRegistry()
	if err := registerWorkerTools(reg, nil, map[string]bool{"terminal": true}); err != nil {
		t.Fatalf("nil manager 应静默跳过，实际返回：%v", err)
	}
	if reg.Len() != 0 {
		t.Fatalf("注册了 %d 个工具，期望 0", reg.Len())
	}
}

// TestRegisterWorkerToolsNamingMatchesAction 验证注册的工具名与声明的 kind/action
// 一一对应，且没有重名（重名会在运行时报「工具已注册」并中断装配）。
func TestRegisterWorkerToolsNamingMatchesAction(t *testing.T) {
	all := map[string]bool{}
	for _, k := range []string{"terminal", "browser", "office", "dynamic-js", "mcp", "computer-use"} {
		all[k] = true
	}
	mgr := worker.NewManager(worker.ManagerConfig{})
	reg := tool.NewRegistry()
	if err := registerWorkerTools(reg, mgr, all); err != nil {
		t.Fatalf("registerWorkerTools: %v", err)
	}
	if reg.Len() != len(workerToolSpecs()) {
		t.Fatalf("注册了 %d 个工具，声明了 %d 个", reg.Len(), len(workerToolSpecs()))
	}
	for _, spec := range workerToolSpecs() {
		got, ok := reg.Get(spec.Name)
		if !ok {
			t.Fatalf("工具 %s 未注册", spec.Name)
		}
		def := got.Definition()
		if def.Name != spec.Name {
			t.Errorf("工具 %s 的定义名 = %s", spec.Name, def.Name)
		}
		if def.Domain.String() == "unknown" {
			t.Errorf("工具 %s 缺少执行域（Domain 未设置）", spec.Name)
		}
	}
}
