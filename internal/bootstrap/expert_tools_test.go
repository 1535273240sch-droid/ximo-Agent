package bootstrap

import (
	"sort"
	"strings"
	"testing"

	"github.com/ximo888ok-netizen/ximo-agent/internal/config"
	"github.com/ximo888ok-netizen/ximo-agent/internal/expert"
	"github.com/ximo888ok-netizen/ximo-agent/internal/tool"
	"github.com/ximo888ok-netizen/ximo-agent/internal/tool/domains"
	"github.com/ximo888ok-netizen/ximo-agent/internal/tool/domains/file"
	gitdomain "github.com/ximo888ok-netizen/ximo-agent/internal/tool/domains/git"
	"github.com/ximo888ok-netizen/ximo-agent/internal/tool/domains/knowledge"
	memorytool "github.com/ximo888ok-netizen/ximo-agent/internal/tool/domains/memory"
	tododomain "github.com/ximo888ok-netizen/ximo-agent/internal/tool/domains/todo"
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
	if err := registry.Register(tododomain.New()); err != nil {
		t.Fatalf("register todo: %v", err)
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

// implementedToolNamesForTest 返回**本 build 实现过的**全部工具名：核心域 + 所有
// worker 动作，不管对应的池在当前配置里是否启用。
//
// 与默认配置快照的区别很重要：推荐表该不该列某个工具，取决于「这个 build 有没有
// 实现它」，而不是「默认配置有没有把它打开」。`computer_observe` 这类工具默认池没
// 启用但确实实现了 —— 推荐它是对的（运行时会被可用性过滤掉）；而 `web_search`
// 这种压根没实现的名字，列出来就是谎报能力。
func implementedToolNamesForTest(t *testing.T) []string {
	t.Helper()

	guard := domains.NewGuard([]string{t.TempDir()})
	set := map[string]bool{}
	for _, x := range file.Tools(guard) {
		set[x.Definition().Name] = true
	}
	for _, x := range gitdomain.Tools(guard) {
		set[x.Definition().Name] = true
	}
	for _, x := range []tool.Tool{
		knowledge.New(nil), web.New(), memorytool.New(nil), tododomain.New(),
	} {
		set[x.Definition().Name] = true
	}
	for _, spec := range workerToolSpecs() {
		set[spec.Name] = true
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// productionToolNames 是上面那份清单的快照（默认配置下真实注册的工具）。
//
// 它存在的意义不是「清单好看」，而是当注册表发生变化时测试会失败，逼着人回来
// 同步 expert 的部门推荐表 —— 部门表里那些不存在的名字正是专家「拿着假工具空转」
// 的根源（见下方 TestExpertRecommendationsExistInRegistry）。
func productionToolNames() []string {
	return []string{
		// file 域（v2.6.0 起含精确编辑）
		"file_delete", "file_edit", "file_list", "file_read", "file_search", "file_write",
		"multi_edit",
		// git 域
		"git_branch", "git_diff", "git_log", "git_status",
		// 单工具域（todo_write 是 v2.6.0 补上的：F1 闭环的前提）
		"knowledge", "memory", "todo_write", "web_fetch",
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

// knownMissingRecommendedTools 是推荐表里**本 build 没有实现**、但被有意登记下来的
// 工具名。
//
// v2.6.0 起它是空的：推荐表（DivisionTools / KeywordToolRules / DefaultTools）与
// 工作流文案都已改成只列真实工具，"推荐一个不存在的工具"这件事在代码里不再存在。
// 保留这张表是为了下一次出现类似差距时有一个**显式的登记处**，而不是把它悄悄塞进
// 推荐表 —— 空表本身就是"当前没有已知缺口"的声明。
func knownMissingRecommendedTools() map[string]bool {
	return map[string]bool{}
}

// TestExpertRecommendationsExistInRegistry 检查推荐表与工作流里提到的每个工具名，
// 要么本 build 真的实现了，要么在 knownMissingRecommendedTools 里明确登记。
//
// 为什么必须有人管这件事：resolveTools 会过滤掉未实现的工具（v2.5.0 的修复），
// 过滤本身是对的，但**过滤后剩几个**决定了专家到底有没有工具可用；而写进系统提示词
// 与工作流的假名字更糟 —— 模型会照着去调用一个必然失败的工具，或者直接在回答里
// 声称自己用过了。没有这条测试，两张表可以整体漂移成一份"全是假工具"的清单。
func TestExpertRecommendationsExistInRegistry(t *testing.T) {
	implemented := map[string]bool{}
	for _, n := range implementedToolNamesForTest(t) {
		implemented[n] = true
	}
	missing := knownMissingRecommendedTools()

	check := func(origin string, names []string) {
		for _, n := range names {
			if implemented[n] || missing[n] {
				continue
			}
			t.Errorf("%s 提到的工具 %q 本 build 没有实现，也没登记在 knownMissingRecommendedTools；"+
				"模型会拿到一个不存在的工具名", origin, n)
		}
	}

	divisions := make([]string, 0, len(expert.DivisionTools))
	for d := range expert.DivisionTools {
		divisions = append(divisions, d)
	}
	sort.Strings(divisions)
	for _, d := range divisions {
		check("部门 "+d, expert.DivisionTools[d])
	}
	for _, rule := range expert.KeywordToolRules {
		check("关键词规则", rule.Tools)
	}
	check("默认工具集", expert.DefaultTools)

	// 工作流是直接写进提示词的操作指南：里面的工具名同样必须真实存在。
	// 只扫反引号里的名字太脆，这里扫"看起来像工具名"的 token（含下划线的小写词）。
	workflows := make([]string, 0, len(expert.DivisionWorkflows)+1)
	for d, w := range expert.DivisionWorkflows {
		workflows = append(workflows, d+"："+w)
	}
	workflows = append(workflows, "默认："+expert.DefaultWorkflow)
	for _, w := range workflows {
		for _, name := range toolLikeTokens(w) {
			if implemented[name] || missing[name] {
				continue
			}
			t.Errorf("工作流文案提到了未实现的工具 %q：%s", name, firstLine(w))
		}
	}
}

// toolLikeTokens 从文案里挑出「像工具名」的 token：小写字母/数字/下划线，且含下划线。
//
// 含下划线是刻意的筛选：工具名在 v2 全部是 snake_case（file_read / todo_write /
// browser_navigate），而自然语言里的英文单词与参数名（path、start_line 之外）很少
// 带下划线。这样既不漏工具名，也不会把普通动词误报成工具。
func toolLikeTokens(s string) []string {
	var out []string
	cur := make([]rune, 0, 24)
	flush := func() {
		if len(cur) == 0 {
			return
		}
		tok := string(cur)
		cur = cur[:0]
		if !strings.Contains(tok, "_") {
			return
		}
		for _, r := range tok {
			if !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && r != '_' {
				return
			}
		}
		out = append(out, tok)
	}
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			cur = append(cur, r)
			continue
		}
		flush()
	}
	flush()
	return out
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	if len(s) > 80 {
		return s[:80]
	}
	return s
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
