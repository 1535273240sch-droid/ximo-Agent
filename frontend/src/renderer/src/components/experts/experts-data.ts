/**
 * 专家库静态数据。
 *
 * 数据来源：后端 internal/expert/assets/agents-raw.json（真实导入的 254 位专家）。
 * 这里只保留 60 位跨全部 17 个部门的代表样本 —— 把 254 条内联进 TS 会让
 * bundle 与可读性同时失控，而 UI 需要的是"每个部门都有代表"的丰富感。
 * 部门的完整清单与真实计数仍然全量保留，筛选项才能反映真实目录结构。
 */

/** 一位专家（字段名与后端 assets JSON 对齐）。 */
export interface Expert {
  id: string
  division: string
  name: string
  description: string
  emoji: string
  /** 原始色值。注意：后端数据里既有 #RRGGBB，也有 'blue'/'teal' 这类 CSS 颜色名。 */
  color: string
  vibe?: string
  tools?: string[]
}

/**
 * 部门 → 推荐工具集。
 *
 * 后端的 agents-raw.json 里每位专家的 tools 字段都是空数组，真正的工具集由
 * internal/expert/division.go 的 DivisionTools 按部门下发（AnalyzeExpert 的第一步）。
 * 因此这里照搬该映射，抽屉里展示的工具才是专家实际会拿到的能力，而非虚构。
 */
export const DIVISION_TOOLS: Record<string, string[]> = {
  'engineering': [
    'file_read',
    'file_write',
    'file_edit',
    'file_search',
    'multi_edit',
    'code_execute',
    'code_lint',
    'code_format',
    'terminal_exec',
    'git_operations',
    'project_context',
    'project_index',
    'dependency_check',
    'todo_write'
  ],
  'design': [
    'ui_generate',
    'design_preview',
    'design_critique',
    'design_audit',
    'design_a11y',
    'design_color',
    'file_read',
    'file_write',
    'web_search',
    'todo_write'
  ],
  'academic': [
    'web_search',
    'web_fetch',
    'web_research',
    'web_cache',
    'file_read',
    'file_write',
    'todo_write'
  ],
  'marketing': [
    'web_search',
    'web_fetch',
    'web_research',
    'file_read',
    'file_write',
    'browser_navigate',
    'browser_screenshot',
    'todo_write'
  ],
  'finance': [
    'web_search',
    'web_fetch',
    'file_read',
    'file_write',
    'todo_write',
    'code_execute'
  ],
  'game-development': [
    'file_read',
    'file_write',
    'file_edit',
    'file_search',
    'code_execute',
    'code_lint',
    'terminal_exec',
    'dependency_check',
    'todo_write'
  ],
  'gis': [
    'file_read',
    'file_write',
    'file_edit',
    'code_execute',
    'terminal_exec',
    'web_search',
    'todo_write'
  ],
  'healthcare': [
    'web_search',
    'web_fetch',
    'web_research',
    'file_read',
    'file_write',
    'todo_write'
  ],
  'paid-media': [
    'web_search',
    'web_fetch',
    'web_research',
    'file_read',
    'file_write',
    'browser_navigate',
    'browser_screenshot',
    'todo_write'
  ],
  'product': [
    'web_search',
    'web_fetch',
    'web_research',
    'file_read',
    'file_write',
    'todo_write',
    'ui_generate',
    'design_preview'
  ],
  'project-management': [
    'web_search',
    'web_fetch',
    'file_read',
    'file_write',
    'todo_write',
    'terminal_exec'
  ],
  'sales': [
    'web_search',
    'web_fetch',
    'web_research',
    'file_read',
    'file_write',
    'todo_write'
  ],
  'security': [
    'file_read',
    'file_edit',
    'file_search',
    'code_execute',
    'terminal_exec',
    'web_search',
    'web_fetch',
    'todo_write'
  ],
  'spatial-computing': [
    'file_read',
    'file_write',
    'file_edit',
    'code_execute',
    'terminal_exec',
    'web_search',
    'todo_write'
  ],
  'specialized': [
    'web_search',
    'web_fetch',
    'web_research',
    'file_read',
    'file_write',
    'todo_write'
  ],
  'support': [
    'web_search',
    'web_fetch',
    'file_read',
    'file_write',
    'todo_write'
  ],
  'testing': [
    'file_read',
    'file_edit',
    'file_search',
    'code_execute',
    'code_lint',
    'terminal_exec',
    'todo_write'
  ],
}

/** 部门 → 中文展示名。slug 保持英文原值（与后端一致），只在界面上做本地化。 */
export const DIVISION_LABELS: Record<string, string> = {
  'engineering': '工程研发',
  'specialized': '专业领域',
  'marketing': '市场营销',
  'gis': '地理信息',
  'security': '安全合规',
  'design': '界面设计',
  'sales': '销售增长',
  'testing': '测试质量',
  'paid-media': '付费媒体',
  'project-management': '项目管理',
  'academic': '学术研究',
  'spatial-computing': '空间计算',
  'support': '客户支持',
  'finance': '财务金融',
  'game-development': '游戏开发',
  'product': '产品',
  'healthcare': '医疗健康',
}

/** 部门 → 真实专家数量（全量 254 位，非样本数）。 */
export const DIVISION_COUNTS: Record<string, number> = {
  'engineering': 58,
  'specialized': 57,
  'marketing': 36,
  'gis': 13,
  'security': 12,
  'design': 10,
  'sales': 9,
  'testing': 9,
  'paid-media': 7,
  'project-management': 7,
  'academic': 6,
  'spatial-computing': 6,
  'support': 6,
  'finance': 5,
  'game-development': 5,
  'product': 5,
  'healthcare': 3,
}

/** 全部 17 个真实部门，按专家数量降序 —— 专家最多的部门排在筛选项最前。 */
export const DIVISIONS: string[] = [
  'engineering',
  'specialized',
  'marketing',
  'gis',
  'security',
  'design',
  'sales',
  'testing',
  'paid-media',
  'project-management',
  'academic',
  'spatial-computing',
  'support',
  'finance',
  'game-development',
  'product',
  'healthcare',
]

/** 后端目录里的真实专家总数。 */
export const TOTAL_EXPERT_COUNT = 254

/** 从 254 位中挑选的代表样本（覆盖全部部门）。 */
const SAMPLED: Expert[] = [
  { id: 'academic-anthropologist', division: 'academic', name: '人类学家', description: '专精文化系统、仪式、亲属关系、信仰体系和民族志方法——构建具有真实生活感而非凭空捏造的文化社会', emoji: '🌍', color: '#D97706', vibe: '没有哪种文化是随机的——每一种实践都在解决你可能尚未察觉的问题' },
  { id: 'academic-historian', division: 'academic', name: '历史学家', description: '专精历史分析、断代、物质文化和史学方法——构建具有历史深度和真实感的叙事', emoji: '📚', color: '#B45309', vibe: '历史不是过去的记录，而是现在的镜子和未来的指南' },
  { id: 'academic-psychologist', division: 'academic', name: '心理学家', description: '专精人类行为、人格理论、动机和认知模式——构建心理上真实可信的角色和行为', emoji: '🧠', color: '#EC4899', vibe: '每个行为背后都有一个动机——找到它，你就理解了这个人' },
  { id: 'academic-statistician', division: 'academic', name: '统计学家', description: '专精定量研究方法、实验设计和统计分析——确保数据驱动的决策严谨可靠', emoji: '📊', color: '#8B5CF6', vibe: '数据不会说谎，但错误的方法会让真相隐身' },
  { id: 'design-ui-designer', division: 'design', name: 'UI 设计师', description: '专精界面视觉设计、组件系统和设计规范——创建美观且一致的数字产品界面', emoji: '🎨', color: 'purple', vibe: '界面是产品与人对话的语言——每个像素都在说话' },
  { id: 'design-ux-architect', division: 'design', name: 'UX 架构师', description: '专精用户体验架构、信息架构和用户流程设计——构建逻辑清晰的产品体验框架', emoji: '📐', color: 'purple', vibe: '好的架构是看不见的——用户只觉得一切都很顺畅' },
  { id: 'design-ux-researcher', division: 'design', name: 'UX 研究员', description: '专精用户研究方法、可用性测试和用户洞察——以证据驱动设计决策', emoji: '🔬', color: 'green', vibe: '不要猜测——去问用户，去观察，去倾听' },
  { id: 'design-visual-storyteller', division: 'design', name: '视觉叙事师', description: '专精视觉叙事、信息图和数据可视化——将复杂信息转化为引人入胜的视觉故事', emoji: '🎬', color: 'purple', vibe: '一张图胜过千言万语——好的视觉叙事胜过一百张图' },
  { id: 'engineering-frontend-developer', division: 'engineering', name: '前端开发工程师', description: '专精前端开发——React/Vue/TypeScript，构建流畅的用户界面和交互体验', emoji: '🖥️', color: 'cyan', vibe: '用户看到的就是前端——每一帧都是承诺' },
  { id: 'engineering-senior-developer', division: 'engineering', name: '高级开发工程师', description: '资深全栈开发工程师——精通架构设计、代码质量和团队指导', emoji: '💎', color: 'green', vibe: '代码写多了就知道——简单比复杂更难做到' },
  { id: 'engineering-backend-architect', division: 'engineering', name: '后端架构师', description: '专精后端系统架构、微服务设计和分布式系统——构建高性能、高可用的服务端', emoji: '🏗️', color: 'blue', vibe: '前端是面子，后端是里子——好的架构内外兼修' },
  { id: 'engineering-devops-automator', division: 'engineering', name: 'DevOps 自动化工程师', description: '专精 DevOps 自动化、基础设施即代码和部署管道——让交付又快又稳', emoji: '⚙️', color: 'orange', vibe: '能自动化的就不该手动——重复劳动是技术的敌人' },
  { id: 'engineering-ai-engineer', division: 'engineering', name: 'AI 工程师', description: '专精 AI/ML 系统设计、模型集成和推理优化——将 AI 能力融入产品', emoji: '🤖', color: 'blue', vibe: '模型只是工具——解决问题的产品才是目标' },
  { id: 'engineering-code-reviewer', division: 'engineering', name: '代码审查员', description: '专精代码质量审查、最佳实践推广和技术债务管理——确保代码库健康可持续', emoji: '👁️', color: 'purple', vibe: '代码审查不是挑错——是让每个人变得更好' },
  { id: 'finance-financial-analyst', division: 'finance', name: '财务分析师', description: '专精财务分析和估值——从财务报表到投资决策', emoji: '📊', color: 'green', vibe: '每个数字背后都有故事——财务分析就是读懂这些故事' },
  { id: 'finance-fpa-analyst', division: 'finance', name: 'FP&A 财务规划分析师', description: '专精财务规划与分析——预算、差异分析、滚动预测和战略决策支持', emoji: '📈', color: 'green', vibe: '预算的低语者——将计划变成数字，将数字变成行动' },
  { id: 'finance-investment-researcher', division: 'finance', name: '投资研究员', description: '专精投资研究——市场调研、尽职调查、投资组合分析和资产估值', emoji: '🔍', color: 'green', vibe: '比共识挖得更深——在脚注中找到 alpha，在叙事中发现风险' },
  { id: 'game-designer', division: 'game-development', name: '游戏设计师', description: '系统与机制架构师——精通 GDD 编写、玩家心理学、经济平衡和玩法循环设计', emoji: '🎮', color: 'yellow', vibe: '以循环、杠杆和玩家动机来思考——构建引人入胜的玩法' },
  { id: 'level-designer', division: 'game-development', name: '关卡设计师', description: '空间叙事与流程专家——精通布局理论、节奏架构、遭遇战设计和环境叙事', emoji: '🗺️', color: 'teal', vibe: '把每个关卡当作一次精心设计的体验——空间在讲故事' },
  { id: 'technical-artist', division: 'game-development', name: '技术美术', description: '美术到引擎的管线专家——精通着色器、VFX 系统、LOD 管线和跨引擎资产优化', emoji: '🎨', color: 'pink', vibe: '艺术愿景与引擎现实之间的桥梁' },
  { id: 'gis-analyst', division: 'gis', name: 'GIS 分析师', description: '专精 GIS 数据分析和可视化——从空间数据中提取洞察', emoji: '🖥️', color: 'teal', vibe: '地图是最古老的数据可视化——但 GIS 让它变得无比强大' },
  { id: 'gis-web-gis-developer', division: 'gis', name: 'Web GIS 开发工程师', description: '专精 Web GIS 开发——Mapbox、OpenLayers 和地理空间 Web 服务', emoji: '🌐', color: 'blue', vibe: 'Web GIS 让地图无处不在——从桌面到浏览器到手机' },
  { id: 'gis-cartography-designer', division: 'gis', name: '制图设计师', description: '地图美学专家——设计美观、可读且有效的地图，精通色彩理论、字体和标签布局', emoji: '🎨', color: 'pink', vibe: '能优美传达的地图才是被使用的地图' },
  { id: 'healthcare-clinical-evidence-agent', division: 'healthcare', name: '临床证据 Agent', description: '专精临床证据分析和医学文献评估——为医疗决策提供循证支持', emoji: '🩺', color: '#1A5276', vibe: '医学决策不应基于直觉——应基于最佳证据' },
  { id: 'healthcare-innovation-strategist', division: 'healthcare', name: '医疗创新策略师', description: '专精医疗行业创新战略——从数字健康到精准医疗', emoji: '🧭', color: '#1B4F72', vibe: '医疗创新不只是技术——是让更多人活得更健康' },
  { id: 'healthcare-sovereign-health-systems-agent', division: 'healthcare', name: '主权健康系统 Agent', description: '专精国家级健康系统设计——数据主权、互操作性和公共卫生', emoji: '🌍', color: '#1B4F72', vibe: '健康数据是国家安全——主权系统不容妥协' },
  { id: 'marketing-seo-specialist', division: 'marketing', name: 'SEO 专家', description: '专精搜索引擎优化——技术 SEO、内容策略和链接建设', emoji: '🔍', color: '#4285F4', vibe: 'SEO 是马拉松不是短跑——但终点值得坚持' },
  { id: 'marketing-growth-hacker', division: 'marketing', name: '增长黑客', description: '专精增长黑客——数据驱动的快速实验，找到可扩展的增长路径', emoji: '🚀', color: 'green', vibe: '增长不是营销——是工程化的实验' },
  { id: 'marketing-content-creator', division: 'marketing', name: '内容创作者', description: '专精内容创作——文章、视频脚本、社交媒体帖子和品牌故事', emoji: '✍️', color: 'teal', vibe: '内容是营销的燃料——好内容自带传播力' },
  { id: 'marketing-xiaohongshu-specialist', division: 'marketing', name: '小红书专家', description: '专精小红书营销——种草笔记、KOC 合作和品牌号运营', emoji: '🌸', color: '#FF1B6D', vibe: '小红书是中国的种草机——用户的推荐比广告更有力' },
  { id: 'marketing-wechat-official-account', division: 'marketing', name: '微信公众号运营师', description: '专精微信公众号运营——图文创作、菜单设计和粉丝管理', emoji: '📱', color: '#09B83E', vibe: '公众号是品牌在中国的内容阵地——深度内容不可替代' },
  { id: 'paid-media-ppc-strategist', division: 'paid-media', name: 'PPC 广告策略师', description: '资深付费搜索策略师——精通 Google、Microsoft 和 Amazon 广告的大规模搜索、购物和效果最大化广告', emoji: '💰', color: 'orange', vibe: '设计从月费 1 万到 1000 万+可扩展的 PPC 广告架构' },
  { id: 'paid-media-programmatic-buyer', division: 'paid-media', name: '程序化与展示广告购买专家', description: '展示广告和程序化购买专家——覆盖 Google Display Network、DV360、合作媒体和 ABM 展示策略', emoji: '📺', color: 'orange', vibe: '以精准的外科手术式方式大规模购买展示和视频库存' },
  { id: 'paid-media-tracking-specialist', division: 'paid-media', name: '追踪与衡量专家', description: '专精转化追踪架构、标签管理和归因建模——GTM、GA4、Meta CAPI、服务端实现', emoji: '📡', color: 'orange', vibe: '如果没有正确追踪，就等于没发生' },
  { id: 'product-manager', division: 'product', name: '产品经理', description: '全面的产品负责人——从发现和策略到路线图、干系人对齐、上市和成果衡量', emoji: '🧭', color: 'blue', vibe: '交付对的东西，而不只是下一个东西——成果导向、用户为本' },
  { id: 'product-trend-researcher', division: 'product', name: '趋势研究员', description: '专精市场情报分析——识别新兴趋势、竞争分析和机会评估，驱动产品策略和创新决策', emoji: '🔭', color: 'purple', vibe: '在趋势进入主流之前就发现它' },
  { id: 'product-sprint-prioritizer', division: 'product', name: '迭代优先级专家', description: '专精敏捷迭代规划、功能优先级和资源分配——通过数据驱动框架最大化团队速度和业务价值', emoji: '🎯', color: 'green', vibe: '通过数据驱动的优先级和无情的聚焦最大化迭代价值' },
  { id: 'project-management-project-shepherd', division: 'project-management', name: '项目牧羊人', description: '专精跨职能项目协调、时间线管理和干系人对齐——从概念到完成全程护航', emoji: '🐑', color: 'blue', vibe: '将跨职能的混乱引导为按时、按范围的交付' },
  { id: 'project-management-meeting-notes-specialist', division: 'project-management', name: '会议记录专家', description: '从会议转录或粗略笔记中提取结构化的决策、行动项和待解问题——输出清晰的四段式摘要', emoji: '📋', color: 'blue', vibe: '精确的提取者——在噪音中找到信号，绝不捏造不存在的内容' },
  { id: 'project-management-studio-producer', division: 'project-management', name: '工作室制作人', description: '高级战略领导者——精通高层创意和技术项目编排、资源分配和多项目组合管理', emoji: '🎬', color: 'gold', vibe: '在复杂举措中将创意愿景与业务目标对齐' },
  { id: 'sales-deal-strategist', division: 'sales', name: '交易策略师', description: '资深交易策略师——精通 MEDDPICC 资格评估、竞争定位和复杂 B2B 销售周期的赢单规划', emoji: '♟️', color: '#1B4D3E', vibe: '像外科医生一样评估交易，消灭一厢情愿' },
  { id: 'sales-outbound-strategist', division: 'sales', name: '外联策略师', description: '基于信号的外联专家——设计多渠道获客序列、定义 ICP，通过研究驱动的个性化建立管道', emoji: '🎯', color: '#E8590C', vibe: '在竞争对手注意到之前，将购买信号转化为预约会议' },
  { id: 'sales-engineer', division: 'sales', name: '售前工程师', description: '专精技术售前——产品演示、技术方案和客户答疑', emoji: '🛠️', color: '#2E5090', vibe: '售前工程师是产品和客户之间的技术翻译官' },
  { id: 'security-appsec-engineer', division: 'security', name: '应用安全工程师', description: '专精应用安全——漏洞扫描、代码审计和安全开发生命周期', emoji: '🔐', color: '#059669', vibe: '安全不是事后补救——是开发全过程的一部分' },
  { id: 'security-penetration-tester', division: 'security', name: '渗透测试员', description: '专精渗透测试——模拟攻击发现安全漏洞', emoji: '🗡️', color: '#dc2626', vibe: '最好的防御是像攻击者一样思考' },
  { id: 'security-cloud-security-architect', division: 'security', name: '云安全架构师', description: '专精云安全架构——云原生安全、合规和零信任', emoji: '☁️', color: '#3b82f6', vibe: '云安全不是围墙——是分层的防御体系' },
  { id: 'visionos-spatial-engineer', division: 'spatial-computing', name: 'visionOS 空间工程师', description: '专精 visionOS 开发——Apple Vision Pro 的空间应用', emoji: '🥽', color: 'indigo', vibe: 'visionOS 不只是新平台——是空间计算时代的开端' },
  { id: 'xr-immersive-developer', division: 'spatial-computing', name: 'XR 沉浸式开发工程师', description: '专精 XR 沉浸式应用开发——Unity/Unreal 的 VR/AR 体验', emoji: '🌐', color: 'neon-cyan', vibe: '沉浸式不是技术——是让用户忘记自己在使用技术' },
  { id: 'macos-spatial-metal-engineer', division: 'spatial-computing', name: 'macOS Spatial/Metal 工程师', description: '专精 macOS 平台 Metal 图形和空间计算开发', emoji: '🍎', color: 'metallic-blue', vibe: 'Metal 是苹果图形的引擎——空间计算是下一个界面' },
  { id: 'agents-orchestrator', division: 'specialized', name: 'Agent 编排器', description: '专精多 Agent 编排——任务分解、Agent 调度和结果聚合', emoji: '🎛️', color: 'cyan', vibe: '一个 Agent 是工具——一群编排好的 Agent 是团队' },
  { id: 'business-strategist', division: 'specialized', name: '商业策略师', description: '专精商业战略——市场分析、竞争策略和商业模式设计', emoji: '♟️', color: 'indigo', vibe: '策略不是预测未来——是为未来做好准备' },
  { id: 'chief-financial-officer', division: 'specialized', name: '首席财务官', description: '企业财务最高决策者——资本运作、财务规划和投资者关系', emoji: '💼', color: 'navy', vibe: 'CFO 不只是管账——是企业价值的守护者' },
  { id: 'specialized-mcp-builder', division: 'specialized', name: 'MCP 构建师', description: '专精 Model Context Protocol（MCP）——构建 LLM 与外部工具的连接', emoji: '🔌', color: 'indigo', vibe: 'MCP 是 LLM 的手和眼——让模型不只是说话，还能行动' },
  { id: 'legal-document-review', division: 'specialized', name: '法律文档审查 Agent', description: '专精法律文档审查——合同分析、风险识别和合规检查', emoji: '⚖️', color: 'blue', vibe: '合同的魔鬼在细节——好的审查让风险无所遁形' },
  { id: 'support-support-responder', division: 'support', name: '客户支持响应员', description: '专精客户支持——提供卓越的客户服务、问题解决和技术支持', emoji: '💬', color: 'blue', vibe: '好的支持不只是解决问题——是让客户感到被重视' },
  { id: 'support-analytics-reporter', division: 'support', name: '分析报告师', description: '专精数据分析——将原始数据转化为可执行的商业洞察', emoji: '📊', color: 'teal', vibe: '数据不等于洞察——好的分析让数据说话' },
  { id: 'support-finance-tracker', division: 'support', name: '财务追踪师', description: '专精财务分析和控制——财务规划、预算管理和成本追踪', emoji: '💰', color: 'green', vibe: '你不追踪财务——财务就会追踪你' },
  { id: 'testing-test-automation-engineer', division: 'testing', name: '测试自动化工程师', description: '专精端到端测试自动化——Playwright 和 Cypress 的弹性测试', emoji: '🎭', color: '#2EAD33', vibe: '手动测试是债务——自动化测试是资产' },
  { id: 'testing-api-tester', division: 'testing', name: 'API 测试员', description: '专精 API 测试——全面的 API 验证、性能和安全测试', emoji: '🔌', color: 'purple', vibe: 'API 是系统的契约——测试就是在验证契约是否被遵守' },
  { id: 'testing-accessibility-auditor', division: 'testing', name: '无障碍审计员', description: '专精无障碍审计——WCAG 标准、技术测试和辅助技术兼容', emoji: '♿', color: '#0077B6', vibe: '无障碍不是可选项——是每个用户的基本权利' },
]

/** 对外数据集：为每位样本补齐所在部门的推荐工具集。 */
export const EXPERTS: Expert[] = SAMPLED.map((e) => ({
  ...e,
  tools: DIVISION_TOOLS[e.division] ?? []
}))

/** 非 16 进制颜色名的兜底色，保证 tint 永远产出合法 CSS。 */
const FALLBACK_HEX = '#8A8A8A'

/** 后端数据里出现的 CSS 颜色名 → 16 进制（仅用于生成 tint，不改动原始 color 字段）。 */
const NAMED_HEX: Record<string, string> = {
  blue: '#3B82F6',
  green: '#22C55E',
  purple: '#A855F7',
  teal: '#14B8A6',
  orange: '#F97316',
  amber: '#F59E0B',
  red: '#EF4444',
  pink: '#EC4899',
  indigo: '#6366F1',
  cyan: '#06B6D4',
  violet: '#8B5CF6',
  yellow: '#EAB308',
  slate: '#64748B',
  gold: '#C9A227',
  navy: '#1E3A5F',
  'metallic-blue': '#4A6FA5',
  'neon-cyan': '#22D3EE',
  'neon-green': '#39FF14'
}

/**
 * 由专家自身 color 生成低透明度 tint。
 *
 * 支持 #RGB / #RRGGBB / CSS 颜色名三种形态 —— 直接用 color + '22' 会在颜色名上
 * 产出 'blue22' 这种非法值，emoji 圆底就会整片透明。这是全组件唯一使用原始色值
 * 的位置（颜色是专家的真实数据，不属于主题令牌）。
 */
export function tintColor(color: string, alpha = '22'): string {
  const raw = color.trim()
  if (raw.startsWith('#')) {
    const hex = raw.slice(1)
    // #RGB → #RRGGBB，否则后两位透明度会盖掉颜色分量。
    const full =
      hex.length === 3
        ? hex
            .split('')
            .map((c) => c + c)
            .join('')
        : hex
    if (full.length === 6 || full.length === 8) return '#' + full.slice(0, 6) + alpha
    return FALLBACK_HEX + alpha
  }
  return (NAMED_HEX[raw.toLowerCase()] ?? FALLBACK_HEX) + alpha
}
