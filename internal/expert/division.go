// division.go —— 部门工具映射、关键词规则与预设工作流，以及专家系统提示词构建。
//
// 对应 v1 src/main/tools/Skill/expert-config.ts（DIVISION_TOOLS / KEYWORD_TOOL_RULES /
// DIVISION_WORKFLOWS / analyzeExpert / buildExpertSystemPrompt）。
//
// 这些静态映射是「功能对等」的基准 —— v1 有哪些部门与关键词规则，v2 必须一致，
// 否则专家拿到的工具集会缩水。
package expert

import (
	"fmt"
	"sort"
	"strings"
)

// DivisionTools 部门 → 推荐工具映射。
//
// **只列本 build 真实注册的工具**。这条规则由测试强制
// （internal/bootstrap/expert_tools_test.go 的 TestExpertRecommendationsExistInRegistry）：
// 推荐表里出现一个不存在的名字，专家就会拿着假工具空转 —— 模型要么发起一次必然
// 失败的调用白烧一轮，要么在回答里声称自己用过了。
//
// v1 的能力清单里有 20 多个名字在 v2 没有实现，这里按下面的映射改写（意图保留、
// 名字换成真实存在的能力）：
//
//	code_execute / code_lint / code_format → terminal_exec（终端 worker 也能跑脚本）
//	   注：没有独立的 lint/format 工具，代码规范检查请用终端里的对应命令。
//	git_operations                        → git_status / git_diff / git_log / git_branch
//	web_search / web_research / web_cache → web_fetch（已知 URL）+ browser_navigate /
//	   browser_get_content（需要"检索"时的唯一真实途径：打开搜索页并读取结果）
//	network_capture / network_replay      → browser_network_monitor
//	computer_use                          → computer_* 系列（需在 runtime.worker_pools
//	   里启用 computer-use，否则会被可用性过滤掉）
//	ui_generate / design_preview / design_critique / design_audit / design_a11y /
//	   design_color / project_index / project_context / dependency_check
//	                                      → **无对应实现，直接移除**：v2 没有设计生成、
//	   项目索引与依赖检查能力，继续"推荐"它们只是谎报能力。
var DivisionTools = map[string][]string{
	"engineering":        {"file_read", "file_search", "file_list", "file_write", "file_edit", "multi_edit", "terminal_exec", "git_status", "git_diff", "git_log", "todo_write"},
	"design":             {"file_read", "file_write", "file_edit", "browser_navigate", "browser_screenshot", "todo_write"},
	"academic":           {"web_fetch", "browser_navigate", "browser_get_content", "file_read", "file_write", "file_search", "todo_write"},
	"marketing":          {"web_fetch", "browser_navigate", "browser_get_content", "browser_screenshot", "file_read", "file_write", "todo_write"},
	"finance":            {"web_fetch", "file_read", "file_write", "terminal_exec", "todo_write"},
	"game-development":   {"file_read", "file_search", "file_write", "file_edit", "multi_edit", "terminal_exec", "todo_write"},
	"gis":                {"file_read", "file_search", "file_write", "file_edit", "terminal_exec", "web_fetch", "todo_write"},
	"healthcare":         {"web_fetch", "browser_navigate", "browser_get_content", "file_read", "file_write", "todo_write"},
	"paid-media":         {"web_fetch", "browser_navigate", "browser_get_content", "browser_screenshot", "file_read", "file_write", "todo_write"},
	"product":            {"web_fetch", "browser_navigate", "browser_get_content", "file_read", "file_write", "todo_write"},
	"project-management": {"todo_write", "web_fetch", "file_read", "file_write", "terminal_exec"},
	"sales":              {"web_fetch", "browser_navigate", "browser_get_content", "file_read", "file_write", "todo_write"},
	"security":           {"file_read", "file_search", "file_edit", "terminal_exec", "git_diff", "web_fetch", "todo_write"},
	"spatial-computing":  {"file_read", "file_search", "file_write", "file_edit", "terminal_exec", "web_fetch", "todo_write"},
	"specialized":        {"web_fetch", "browser_navigate", "browser_get_content", "file_read", "file_write", "todo_write"},
	"support":            {"web_fetch", "file_read", "file_write", "todo_write"},
	"testing":            {"file_read", "file_search", "file_edit", "multi_edit", "terminal_exec", "git_diff", "todo_write"},
}

// KeywordToolRule 关键词 → 额外工具补充规则。
type KeywordToolRule struct {
	Keywords []string
	Tools    []string
}

// KeywordToolRules 与 v1 KEYWORD_TOOL_RULES 的关键词一致，工具名按 v2 真实能力改写。
var KeywordToolRules = []KeywordToolRule{
	{[]string{"代码", "编程", "开发", "code", "programming", "develop", "前端", "后端", "frontend", "backend"}, []string{"file_read", "file_edit", "multi_edit", "file_search", "terminal_exec"}},
	{[]string{"设计", "UI", "界面", "design", "interface", "视觉", "visual"}, []string{"file_read", "file_write", "browser_navigate", "browser_screenshot"}},
	{[]string{"搜索", "研究", "search", "research", "调研", "分析"}, []string{"web_fetch", "browser_navigate", "browser_get_content"}},
	{[]string{"浏览器", "网页", "browser", "web", "爬虫", "crawl"}, []string{"browser_navigate", "browser_get_content", "browser_click", "browser_screenshot", "browser_execute_js"}},
	{[]string{"桌面", "操控", "电脑", "desktop", "computer", "自动化", "automate"}, []string{"computer_observe", "computer_screenshot", "computer_click_element", "computer_set_text"}},
	{[]string{"网络", "抓包", "network", "capture", "API", "接口"}, []string{"browser_network_monitor", "terminal_exec", "file_write"}},
	{[]string{"Git", "版本", "commit", "branch"}, []string{"git_status", "git_diff", "git_log", "git_branch"}},
	{[]string{"数据库", "database", "SQL", "DB"}, []string{"terminal_exec", "file_read"}},
	{[]string{"无障碍", "accessibility", "a11y", "WCAG"}, []string{"browser_navigate", "browser_get_content", "file_read"}},
	{[]string{"颜色", "color", "配色", "色彩"}, []string{"file_read", "file_write"}},
	{[]string{"安全", "security", "漏洞", "vulnerability", "penetration"}, []string{"file_search", "file_read", "terminal_exec", "git_diff"}},
	{[]string{"性能", "performance", "优化", "optimize"}, []string{"terminal_exec", "file_read", "file_write"}},
}

// DivisionWorkflows 部门 → 预设工作流。
//
// 与 DivisionTools 同一条规则：**步骤里提到的每个工具都必须真实存在**。工作流是
// 直接写进专家系统提示词的操作指南，出现一个不存在的工具名，模型就会照着去调用
// 它 —— 这比工具清单里的假名字危害更大，因为它是"第一步做什么"的明确指令。
var DivisionWorkflows = map[string]string{
	"engineering": `预设工作流（工程类）：
1. 理解需求 → 明确技术栈、目标和约束
2. file_list / file_search 摸清代码结构，定位关键文件
3. file_read 精准读取相关代码段（大文件用它的行范围参数分段读）
4. file_edit / multi_edit 做最小改动的精确修改（多处改动合并成一次 multi_edit）
5. terminal_exec 编译 / 运行 / 测试，用真实输出验证改动
6. git_diff 复核改动范围，git_status 看工作区状态
7. file_write 输出结论、风险与后续建议`,

	"design": `预设工作流（设计类）：
1. 理解设计需求和目标用户
2. file_read 读取现有界面代码与样式
3. browser_navigate 打开页面 + browser_screenshot 截图，先看真实渲染效果
4. 从层级、信息架构、认知负荷、对比度与无障碍角度逐条分析
5. file_edit / file_write 落地改动（小改动用 file_edit，重构成段用 file_write）
6. 再截图复核一次，确认改动真的生效
7. 输出设计说明与待确认项`,

	"academic": `预设工作流（学术研究类）：
1. 明确研究问题和范围
2. 已有明确来源时用 web_fetch 抓取原文；需要检索时用 browser_navigate 打开搜索页 +
   browser_get_content 读取结果（v2 没有独立的搜索 API 工具，浏览器是唯一检索途径）
3. file_write 记录来源与关键结论，便于后续引用
4. 分析、综合、归纳，明确区分"资料中的观点"与"你的推断"
5. file_write 撰写研究报告`,

	"marketing": `预设工作流（营销类）：
1. 理解营销目标和受众
2. browser_navigate + browser_get_content 查看竞品与市场信息
3. web_fetch 抓取已知 URL 的详细数据
4. browser_screenshot 留证，便于对照
5. 分析并制定策略
6. file_write 输出营销方案`,

	"finance": `预设工作流（财务类）：
1. 理解财务分析目标
2. web_fetch 抓取已知来源的市场与财经数据
3. terminal_exec 运行计算脚本（把口径与公式写清楚，让结果可复算）
4. 交叉验证关键数字，注明假设
5. file_write 撰写财务报告`,

	"game-development": `预设工作流（游戏开发类）：
1. 理解游戏设计文档和需求
2. file_search 搜索现有代码结构，file_read 读取相关实现
3. file_edit / multi_edit 修改游戏逻辑
4. terminal_exec 构建、运行测试、复现问题
5. git_diff 复核改动
6. file_write 输出改动说明与调参建议`,

	"gis": `预设工作流（GIS 类）：
1. 理解空间分析需求
2. file_search 搜索相关代码，file_read 读取数据源与配置
3. terminal_exec 运行空间分析脚本
4. web_fetch 查询参考数据（坐标系、标准、口径）
5. file_write 输出分析结果与地图/图层说明`,

	"healthcare": `预设工作流（医疗健康类）：
1. 理解医疗领域需求
2. web_fetch 抓取指南、文献等已知来源
3. 需要检索时用 browser_navigate + browser_get_content 读取结果
4. 分析综合，明确标注证据强度与不确定性
5. file_write 输出报告`,

	"paid-media": `预设工作流（付费媒体类）：
1. 理解广告投放目标
2. browser_navigate + browser_get_content 查看投放平台与行业基准
3. web_fetch 抓取已知来源的数据
4. browser_screenshot 留证
5. 分析并制定投放策略
6. file_write 输出方案`,

	"product": `预设工作流（产品类）：
1. 理解产品目标和用户需求
2. todo_write 拆解需要产出的文档与决策项
3. browser_navigate + browser_get_content 查看竞品
4. web_fetch 抓取已知来源的资料
5. 分析并制定产品策略
6. file_write 输出产品文档`,

	"project-management": `预设工作流（项目管理类）：
1. 理解项目目标和范围
2. todo_write 创建任务分解结构（这是本部门最核心的工具）
3. file_read 读取现有项目文档
4. terminal_exec 执行必要的检查命令
5. file_write 输出项目计划与风险清单`,

	"sales": `预设工作流（销售类）：
1. 理解销售目标
2. browser_navigate + browser_get_content 查看客户与行业公开信息
3. web_fetch 抓取已知来源的资料
4. 分析并制定销售策略
5. file_write 输出销售方案`,

	"security": `预设工作流（安全类）：
1. 理解安全评估目标
2. file_search 搜索代码中的安全相关模式，file_read 读取关键代码段
3. terminal_exec 运行可用的扫描/检查命令（v2 没有独立的安全扫描工具）
4. web_fetch 查询漏洞信息与修复方案
5. file_edit 修复问题，git_diff 复核改动范围
6. file_write 输出安全报告（含影响面与复现步骤）`,

	"spatial-computing": `预设工作流（空间计算类）：
1. 理解空间计算需求
2. file_search 搜索相关代码，file_read 读取现有实现
3. terminal_exec 构建与测试
4. web_fetch 查询平台文档
5. file_write 输出结果`,

	"specialized": `预设工作流（专业领域类）：
1. 理解专业需求
2. web_fetch 抓取已知来源的资料
3. 需要检索时用 browser_navigate + browser_get_content
4. 分析综合
5. file_write 输出报告`,

	"support": `预设工作流（支持类）：
1. 理解客户问题
2. web_fetch 抓取已知文档
3. file_read 查看本地文档与排障记录
4. 分析并给出可执行的处理步骤
5. file_write 输出回复`,

	"testing": `预设工作流（测试类）：
1. 理解测试需求与验收标准
2. file_search 搜索测试代码，file_read 读取被测实现与现有用例
3. file_edit / multi_edit 编写或修改用例
4. terminal_exec 运行测试并贴出真实结果
5. git_diff 复核改动范围
6. file_write 输出测试报告（通过/失败/未覆盖）`,
}

// DefaultTools 无法匹配部门时的默认工具集。
var DefaultTools = []string{"web_fetch", "file_read", "file_search", "file_write", "file_edit", "todo_write"}

// DefaultWorkflow 默认工作流。
const DefaultWorkflow = `预设工作流：
1. 理解任务目标和约束
2. todo_write 拆解可验证的步骤
3. file_read / file_search 读取相关材料；需要外部信息时用 web_fetch
4. 分析综合，必要时用 terminal_exec 验证
5. file_write 输出结果`

// 子 Agent 约束（与 v1 常量一致）。
const (
	// MaxSubAgentRounds 子 Agent 工具调用最大轮次。
	MaxSubAgentRounds = 15
	// MaxSubToolResult 子 Agent 工具结果截断长度。
	MaxSubToolResult = 8000
)

// ExpertAnalysis 分析专家的结果：推荐工具集与预设工作流。
type ExpertAnalysis struct {
	Tools    []string `json:"tools"`
	Workflow string   `json:"workflow"`
}

// AnalyzeExpert 分析专家，推断所需工具和预设工作流。
//
// 三步（与 v1 analyzeExpert 一致）：
//  1. 基于部门获取基础工具集
//  2. 关键词扫描（description/personality/vibe）叠加额外工具
//  3. 专家自带的 tools 字段也纳入
func AnalyzeExpert(e Expert) ExpertAnalysis {
	toolSet := make(map[string]struct{})

	base, ok := DivisionTools[e.Division]
	if !ok {
		base = DefaultTools
	}
	for _, t := range base {
		toolSet[t] = struct{}{}
	}

	fullText := strings.ToLower(e.Description + " " + e.Personality + " " + e.Vibe)
	for _, rule := range KeywordToolRules {
		for _, kw := range rule.Keywords {
			if strings.Contains(fullText, strings.ToLower(kw)) {
				for _, t := range rule.Tools {
					toolSet[t] = struct{}{}
				}
				break
			}
		}
	}

	for _, t := range e.Tools {
		toolSet[t] = struct{}{}
	}

	tools := make([]string, 0, len(toolSet))
	for t := range toolSet {
		tools = append(tools, t)
	}
	// 排序保证工具列表稳定 —— 工具顺序抖动会破坏 prompt cache 前缀
	// （与 context 包的 NormalizeToolSchemas 同一个设计意图）。
	sort.Strings(tools)

	workflow, ok := DivisionWorkflows[e.Division]
	if !ok {
		workflow = DefaultWorkflow
	}

	return ExpertAnalysis{Tools: tools, Workflow: workflow}
}

// BuildSystemPrompt 生成专家系统提示词（与 v1 buildExpertSystemPrompt 逐段一致）。
//
// 注意：提示词前缀的 `你现在扮演 **{name}**（{emoji}）。` 格式被 sub-agent.go
// 用来反解专家身份，改动格式需同步改解析正则。
//
// 工具一节用的是「推荐工具集」，即**未过滤**的名单。真正跑子代理时必须改用
// BuildSystemPromptWithTools：推荐表里有大量本 build 未实现的名字（code_execute /
// git_operations / todo_write…），照着它写进提示词，模型就会去调用一个必然失败的
// 工具，或者干脆在回答里声称自己用过了。本函数保留给「只看专家档案、不执行」的
// 信息型返回。
func BuildSystemPrompt(e Expert) string {
	return BuildSystemPromptWithTools(e, AnalyzeExpert(e).Tools)
}

// BuildSystemPromptWithTools 生成专家系统提示词，但「可用工具」一节只列 tools。
//
// tools 必须是调用方**核实过的**工具名（子代理路径传的是 resolveTools 过滤后的
// 结果），因为它会原样告诉模型「你有这些工具」。空列表不写成"你已被配置以下工具"
// 加一段空白，而是明确告知没有工具可用并禁止虚构工具调用 —— 「没工具」和
// 「有工具但列表为空」对模型来说是两种完全不同的指令。
func BuildSystemPromptWithTools(e Expert, tools []string) string {
	analysis := AnalyzeExpert(e)

	var b strings.Builder
	fmt.Fprintf(&b, "你现在扮演 **%s**（%s）。\n\n", e.Name, e.Emoji)
	b.WriteString(e.Personality)
	b.WriteString("\n\n## 你的核心能力\n")
	b.WriteString(e.Description)
	b.WriteString("\n\n## 你的工作风格\n")
	b.WriteString(e.Vibe)

	if len(tools) == 0 {
		b.WriteString("\n\n## 可用工具\n")
		b.WriteString("本次**没有任何可用工具**（本 build 未注册该专家的推荐工具）。\n")
		b.WriteString("- 请只依靠你自己的知识与推理作答，不要声称调用了任何工具。\n")
		b.WriteString("- 不要输出工具调用、工具名或伪造的执行结果；如果任务必须依赖某个工具才能完成，" +
			"直接说明缺少什么能力，让用户知道边界在哪里。\n")
	} else {
		b.WriteString("\n\n## 可用工具\n你已被配置以下工具，请在需要时主动使用：\n")
		for _, t := range tools {
			fmt.Fprintf(&b, "- `%s`\n", t)
		}
	}

	fmt.Fprintf(&b, "\n## %s\n", analysis.Workflow)

	b.WriteString("\n## 输出要求\n")
	fmt.Fprintf(&b, "- 始终以 %s 的专业视角分析和回答问题\n", e.Name)
	b.WriteString("- 使用该领域专业术语，但确保可理解\n")
	b.WriteString("- 给出可操作的具体建议，而非泛泛而谈\n")
	if len(tools) > 0 {
		b.WriteString("- 主动使用可用工具以提升回答质量\n")
	} else {
		b.WriteString("- 不要编造工具执行结果：没有工具可用时，如实给出你的分析与建议\n")
	}
	b.WriteString("- 按照预设工作流的步骤推进任务，确保有序执行")
	return b.String()
}

// ParseExpertIdentity 从系统提示词反解专家身份（v1 sub-agent.ts 的同款降级逻辑）。
//
// 用于调用方未显式传入身份时的事件标注。
func ParseExpertIdentity(systemPrompt string) (name, emoji string) {
	name, emoji = "unknown-expert", "🧠"
	const marker = "你现在扮演 **"
	idx := strings.Index(systemPrompt, marker)
	if idx < 0 {
		return name, emoji
	}
	rest := systemPrompt[idx+len(marker):]
	end := strings.Index(rest, "**")
	if end <= 0 {
		return name, emoji
	}
	name = rest[:end]

	// emoji 紧跟 name 之后的 （...）
	after := rest[end+2:]
	if strings.HasPrefix(after, "（") {
		if close := strings.Index(after, "）"); close > 0 {
			emoji = after[len("（"):close]
		}
	}
	return name, emoji
}
