package bootstrap

import (
	"sort"
	"testing"

	"github.com/ximo888ok-netizen/ximo-agent/internal/config"
	"github.com/ximo888ok-netizen/ximo-agent/internal/expert"
	"github.com/ximo888ok-netizen/ximo-agent/internal/tool"
	"github.com/ximo888ok-netizen/ximo-agent/internal/tool/domains"
	"github.com/ximo888ok-netizen/ximo-agent/internal/tool/domains/file"
	gitdomain "github.com/ximo888ok-netizen/ximo-agent/internal/tool/domains/git"
	"github.com/ximo888ok-netizen/ximo-agent/internal/tool/domains/knowledge"
	memorytool "github.com/ximo888ok-netizen/ximo-agent/internal/tool/domains/memory"
	"github.com/ximo888ok-netizen/ximo-agent/internal/tool/domains/web"
	"github.com/ximo888ok-netizen/ximo-agent/internal/worker"
)

// registeredToolNamesForTest 复刻 buildToolRuntime 的注册顺序，返回注册表里的
// 全部工具名（默认运行时配置对应的 worker 池）。
//
// 为什么不直接调 buildToolRuntime：它需要 *config.Config / Options / *App / *sqlite.DB
// 四个真实对象，为一个「名字清单」把整套装配拖进单测得不偿失。这里只复刻注册部分，
// 并由 TestRegisteredToolNamesMatchProductionWiring 锁住「注册来源没有变化」。
func registeredToolNamesForTest(t *testing.T) []string {
	t.Helper()

	guard := domains.NewGuard([]string{t.TempDir()})
	registry := tool.NewRegistry()
	register := func(ts []tool.Tool) {
		for _, x := range ts {
			if err := registry.Register(x); err != nil {
				t.Fatalf("register %s: %v", x.Definition().Name, err)
			}
		}
	}
	register(file.Tools(guard))
	register(gitdomain.Tools(guard))
	if err := registry.Register(knowledge.New(nil)); err != nil {
		t.Fatalf("register knowledge: %v", err)
	}
	if err := registry.Register(web.New()); err != nil {
		t.Fatalf("register web: %v", err)
	}
	if err := registry.Register(memorytool.New(nil)); err != nil {
		t.Fatalf("register memory: %v", err)
	}

	// 默认配置的 worker 池（config.DefaultRuntimeConfig）。
	mgr := worker.NewManager(worker.ManagerConfig{})
	enabled := map[string]bool{}
	for kind, size := range config.DefaultRuntimeConfig().WorkerPools {
		if size > 0 {
			enabled[kind] = true
		}
	}
	if err := registerWorkerTools(registry, mgr, enabled); err != nil {
		t.Fatalf("registerWorkerTools: %v", err)
	}

	names := registry.Names()
	sort.Strings(names)
	return names
}

// productionToolNames 是上面那份清单的快照（默认配置下真实注册的工具）。
//
// 它存在的意义不是「清单好看」，而是当注册表发生变化时测试会失败，逼着人回来
// 同步 expert 的部门推荐表 —— 部门表里那些不存在的名字正是专家「拿着假工具空转」
// 的根源（见下方 TestExpertRecommendationsExistInRegistry）。
func productionToolNames() []string {
	return []string{
		// file 域
		"file_delete", "file_list", "file_read", "file_search", "file_write",
		// git 域
		"git_branch", "git_diff", "git_log", "git_status",
		// 单工具域
		"knowledge", "memory", "web_fetch",
		// 默认启用的 worker 池：terminal / browser / mcp / dynamic-js
		"terminal_exec",
		"browser_click", "browser_execute_js", "browser_get_content",
		"browser_navigate", "browser_network_monitor", "browser_screenshot",
		"browser_type",
		"dynamic_eval",
		"mcp_call_tool", "mcp_list_tools", "mcp_refresh_tools", "mcp_status",
	}
}

// TestRegisteredToolNamesMatchProductionWiring 把「生产注册表里到底有哪些工具」
// 钉成一份可核对的快照。
//
// 这条测试是下面那条「专家推荐工具是否存在」的前提：如果它失真，专家工具链的
// 结论就是建在沙上的。
func TestRegisteredToolNamesMatchProductionWiring(t *testing.T) {
	got := registeredToolNamesForTest(t)
	want := productionToolNames()
	sort.Strings(want)

	if len(got) != len(want) {
		t.Fatalf("注册工具数 = %d，期望 %d\n实际：%v\n期望：%v", len(got), len(want), got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("注册工具清单不一致\n实际：%v\n期望：%v", got, want)
		}
	}
}

// knownMissingRecommendedTools 是部门推荐表里**本 build 没有实现**的工具名。
//
// 它们是审核排查发现的真实差距：推荐表按 v1 的能力清单写，而 v2 只实现了其中
// 一部分。留着这份名单是为了两件事：
//
//  1. 让差距显式（不是「没人注意」），并且新注册一个工具时测试会失败，提醒同步
//     推荐表与本文档；
//  2. 提醒 BuildSystemPrompt 必须只宣传**实际可用**的工具 —— 否则专家被告知
//     "你已被配置 code_execute / git_operations"，模型会照着去调用，得到一次
//     「工具未注册」的失败往返，甚至直接谎称自己跑过（见 fixedPromptTools）。
func knownMissingRecommendedTools() map[string]bool {
	return map[string]bool{
		// 工程类：v2 只有 file_read/list/search/write/delete，没有编辑与执行
		"file_edit": true, "multi_edit": true,
		"code_execute": true, "code_lint": true, "code_format": true,
		"project_context": true, "project_index": true, "dependency_check": true,
		// v1 的 git 工具在 v2 拆成了 git_status/log/branch/diff
		"git_operations": true,
		// web 域只实现了抓取
		"web_search": true, "web_research": true, "web_cache": true,
		// 设计类整体未实现
		"ui_generate": true, "design_preview": true, "design_critique": true,
		"design_audit": true, "design_a11y": true, "design_color": true,
		// 需要专门 worker 且默认/本 build 未提供
		"computer_use":     true,
		"network_capture":  true,
		"network_replay":   true,
		"api_extract":      true,
		"browser_click_js": true,
		// 待办工具在 v2 没有实现（但 F1 闭环与部门推荐都在引用它）
		"todo_write": true,
		// 浏览器族的两个动作名与注册表不同名
		"browser_click_type": true,
	}
}

// TestExpertRecommendationsExistInRegistry 检查专家部门推荐表的每个名字：
// 要么真的注册了，要么在 knownMissingRecommendedTools 里明确登记。
//
// 为什么必须有人管这件事：resolveTools 现在会过滤掉未注册的工具（v2.5.0 的修复），
// 过滤本身是对的，但**过滤后剩几个**决定了专家到底有没有工具可用。没有这条测试，
// 推荐表可以整体漂移成一份「全是假工具」的清单，而专家静默退化成无工具状态。
func TestExpertRecommendationsExistInRegistry(t *testing.T) {
	registered := map[string]bool{}
	for _, n := range registeredToolNamesForTest(t) {
		registered[n] = true
	}
	missing := knownMissingRecommendedTools()

	seen := map[string]bool{}
	check := func(origin string, names []string) {
		for _, n := range names {
			if registered[n] || missing[n] {
				continue
			}
			t.Errorf("%s 推荐的工具 %q 既没注册、也没登记在 knownMissingRecommendedTools，"+
				"专家会拿到一个不存在的工具名", origin, n)
		}
		seen[origin] = true
	}

	divisions := make([]string, 0, len(expert.DivisionTools))
	for d := range expert.DivisionTools {
		divisions = append(divisions, d)
	}
	sort.Strings(divisions)
	for _, d := range divisions {
		check("部门 "+d, expert.DivisionTools[d])
	}
	for i, rule := range expert.KeywordToolRules {
		check("关键词规则", rule.Tools)
		_ = i
	}
	check("默认工具集", expert.DefaultTools)
}

// TestEveryDivisionKeepsAtLeastOneRealTool 是这条链路的硬底线：
// 过滤掉未注册的工具之后，每个部门的专家必须至少还剩一个真能调的工具。
//
// 只剩 0 个意味着「这位专家不可能使用任何工具」——而系统提示词若还宣称他有一堆
// 工具，用户会看到一个只会说「我已经用 X 工具处理了」的模型。
func TestEveryDivisionKeepsAtLeastOneRealTool(t *testing.T) {
	registered := map[string]bool{}
	for _, n := range registeredToolNamesForTest(t) {
		registered[n] = true
	}

	divisions := make([]string, 0, len(expert.DivisionTools))
	for d := range expert.DivisionTools {
		divisions = append(divisions, d)
	}
	sort.Strings(divisions)

	for _, d := range divisions {
		kept := make([]string, 0, 4)
		for _, n := range expert.DivisionTools[d] {
			if registered[n] {
				kept = append(kept, n)
			}
		}
		if len(kept) == 0 {
			t.Errorf("部门 %s 过滤后没有任何可用工具（推荐表：%v）", d, expert.DivisionTools[d])
			continue
		}
		t.Logf("部门 %-20s 可用工具：%v", d, kept)
	}
}
