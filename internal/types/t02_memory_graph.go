package types

import "context"

// MemoryGraphPort 是记忆图的读写面（审核文档 4.9 的 IPC 面，P1-c）。
//
// 为什么单独声明成一个可选接口，而不是加进 Engine：记忆页与 run 生命周期无关
// （记忆是跨 run 的长期资产），而且不是每个装配都有记忆后端。ipcapi 用
// `eng.(types.MemoryGraphPort)` 这样的能力断言注册 memory 帧——与
// Recoverer / ToolDecider 完全同一条思路：没有该能力时帧返回明确错误，而不是
// 静默成功让界面以为操作生效了。
//
// 实现方是 internal/memory：契约里的每个类型都有 JSON tag，因为这一份类型同时是
// IPC 帧的载荷形状（ipcapi 直接复用它们，不再抄一份 DTO 出来——两份形状必然漂移）。
// 前端 frontend/src/shared/types.ts 是同一契约的另一侧。
type MemoryGraphPort interface {
	// Graph 取一个子图（分页 / 过滤 / 邻域 / 搜索）。
	Graph(ctx context.Context, req MemoryGraphRequest) (MemoryGraph, error)
	// Node 取单个节点的完整详情（含完整正文、相邻边、最近召回记录）。
	Node(ctx context.Context, nodeID string) (MemoryNodeDetail, error)
	// UpdateNode 改标题 / 正文 / 重要度 / 置顶 / 状态。零值指针表示不改。
	UpdateNode(ctx context.Context, upd MemoryNodeUpdate) (MemoryNodeDetail, error)
	// DeleteNode 硬删除节点及其所有边（用户说「忘掉」时走这里，不是归档）。
	DeleteNode(ctx context.Context, nodeID string) error
	// Link 显式建立一条边；已存在则更新权重。
	Link(ctx context.Context, link MemoryLink) (MemoryGraphMutation, error)
	// Consolidate 手动触发一次「睡眠整理」。
	Consolidate(ctx context.Context) (MemoryGraphMutation, error)
	// Export 导出全部记忆（可读 JSON，用于备份/迁移）。
	Export(ctx context.Context) (MemoryExport, error)
	// Import 导入一份导出文件；按内容哈希幂等合并，不覆盖已有内容。
	Import(ctx context.Context, in MemoryExport) (MemoryGraphMutation, error)
	// Stats 返回计数快照与当前后端名。
	Stats(ctx context.Context) MemoryStats
	// Clear 清空全部记忆（不可撤销）。
	//
	// 它是一条显式能力，而不是让实现方在别的接口里顺手支持：不支持清空的实现
	// 必须返回错误，而不是「成功但什么都没做」——后者会让用户以为记忆已经删干净。
	Clear(ctx context.Context) (MemoryGraphMutation, error)
}

// 记忆节点的 kind。与 mem_nodes.kind 的 CHECK 约束逐字一致。
const (
	MemoryKindFact      = "fact"
	MemoryKindEntity    = "entity"
	MemoryKindEpisode   = "episode"
	MemoryKindProcedure = "procedure"
	MemoryKindTopic     = "topic"
)

// 记忆节点的 status。
const (
	MemoryStatusActive     = "active"
	MemoryStatusSuperseded = "superseded"
	MemoryStatusArchived   = "archived"
)

// 图的边关系。与 mem_edges.rel 的 CHECK 约束逐字一致。
const (
	MemoryRelMentions     = "mentions"
	MemoryRelRelated      = "related"
	MemoryRelPartOf       = "part_of"
	MemoryRelCauses       = "causes"
	MemoryRelDerivedFrom  = "derived_from"
	MemoryRelSupersedes   = "supersedes"
	MemoryRelContradicts  = "contradicts"
	MemoryRelSameTopic    = "same_topic"
	MemoryRelUsedWith     = "used_with"
)

// AllMemoryKinds / AllMemoryRels 供界面做筛选下拉与校验。
var AllMemoryKinds = []string{
	MemoryKindFact, MemoryKindEntity, MemoryKindEpisode, MemoryKindProcedure, MemoryKindTopic,
}

var AllMemoryRels = []string{
	MemoryRelMentions, MemoryRelRelated, MemoryRelPartOf, MemoryRelCauses,
	MemoryRelDerivedFrom, MemoryRelSupersedes, MemoryRelContradicts,
	MemoryRelSameTopic, MemoryRelUsedWith,
}

// MemoryGraphRequest 是一次子图查询。
type MemoryGraphRequest struct {
	// Limit 返回的最大节点数（0 表示由实现给一个有界默认值）。
	Limit int `json:"limit,omitempty"`
	// Offset 跳过的节点数，按 kind/importance 稳定排序，用于翻页。
	Offset int `json:"offset,omitempty"`
	// Kinds 只取这些 kind；空表示全部。
	Kinds []string `json:"kinds,omitempty"`
	// IncludeArchived 是否包含已归档节点。
	IncludeArchived bool `json:"include_archived,omitempty"`
	// Query 非空时只返回词法命中的节点及其直接关联。
	Query string `json:"query,omitempty"`
	// CenterOn 非空时以该节点为中心取邻域。
	CenterOn string `json:"center_on,omitempty"`
	// Depth 是 CenterOn 的邻域跳数（默认 1）。
	Depth int `json:"depth,omitempty"`
}

// MemoryGraphNode 是图视图里的一个节点。
type MemoryGraphNode struct {
	ID             string  `json:"id"`
	Kind           string  `json:"kind"`
	Title          string  `json:"title,omitempty"`
	ContentPreview string  `json:"content_preview,omitempty"`
	Importance     float64 `json:"importance"`
	Pinned         bool    `json:"pinned"`
	Status         string  `json:"status"`
	SourceRun      string  `json:"source_run,omitempty"`
	UseCount       int     `json:"use_count"`
	// Degree 是节点的度，图视图据此决定圆圈大小（与 importance 一起）。
	Degree    int   `json:"degree"`
	CreatedAt int64 `json:"created_at"`
	// LastUsed 为 0 表示从未被采用。
	LastUsed int64 `json:"last_used,omitempty"`
}

// MemoryGraphEdge 是图视图里的一条有向边。
type MemoryGraphEdge struct {
	Src string `json:"src"`
	Dst string `json:"dst"`
	Rel string `json:"rel"`
	// Weight 是原始权重；EffectiveWeight 是惰性衰减后的当前强度。
	//
	// 图视图必须按 EffectiveWeight 渲染粗细：只画原始权重的话，一条 45 天前
	// 用过、实际关联已经很弱的边看起来仍然和刚建立时一样粗，那是在骗用户。
	Weight          float64 `json:"weight"`
	EffectiveWeight float64 `json:"effective_weight"`
	FireCount       int     `json:"fire_count,omitempty"`
}

// MemoryGraph 是一次子图查询的结果。
type MemoryGraph struct {
	Nodes []MemoryGraphNode `json:"nodes"`
	Edges []MemoryGraphEdge `json:"edges"`
	// Total 是符合条件的节点总数（不受 Limit 影响）。
	Total int `json:"total"`
	// Truncated 为 true 表示结果被 Limit 截断。
	Truncated bool `json:"truncated,omitempty"`
}

// MemoryRecallEntry 是一条召回记录：这个节点什么时候被想起过、经由什么路径。
type MemoryRecallEntry struct {
	RunID string `json:"run_id"`
	Via   string `json:"via,omitempty"`
	Used  bool   `json:"used"`
	TS    int64  `json:"ts"`
}

// MemoryNodeDetail 是一个节点的完整详情。
type MemoryNodeDetail struct {
	Node MemoryGraphNode `json:"node"`
	// Content 是完整正文（列表里只有预览）。
	Content string `json:"content,omitempty"`
	// Edges / Neighbors 描述邻域，避免前端逐条再查。
	Edges     []MemoryGraphEdge `json:"edges"`
	Neighbors []MemoryGraphNode `json:"neighbors"`
	// Recalls 是最近若干条召回记录（4.9 第 3 条的「用过 N 次 / 最近使用」）。
	Recalls []MemoryRecallEntry `json:"recalls,omitempty"`
}

// MemoryNodeUpdate 改一个节点的可变字段。nil 表示「不改」，刻意用指针而不是零值：
// 把「用户没动这个字段」与「用户想把它置空」混为一谈，会静默清掉用户的数据。
type MemoryNodeUpdate struct {
	NodeID     string   `json:"node_id"`
	Title      *string  `json:"title,omitempty"`
	Content    *string  `json:"content,omitempty"`
	Importance *float64 `json:"importance,omitempty"`
	Pinned     *bool    `json:"pinned,omitempty"`
	Status     *string  `json:"status,omitempty"`
}

// MemoryLink 是一条待建立的边。
type MemoryLink struct {
	Src string `json:"src"`
	Dst string `json:"dst"`
	// Rel 空值表示 related。
	Rel    string  `json:"rel,omitempty"`
	Weight float64 `json:"weight,omitempty"`
}

// MemoryGraphMutation 是写操作的结果：报告做了什么，而不是只说「成功」。
type MemoryGraphMutation struct {
	OK bool `json:"ok"`
	// Affected 是受影响的节点/边数量，按操作含义解释（合并数、归档数……）。
	Affected int `json:"affected,omitempty"`
	Notes    []string `json:"notes,omitempty"`
	Stats    *MemoryStats `json:"stats,omitempty"`
}

// MemoryStats 是记忆后端的计数快照。
type MemoryStats struct {
	UserID         string `json:"user_id,omitempty"`
	Nodes          int    `json:"nodes"`
	Edges          int    `json:"edges"`
	RecallCalls    uint64 `json:"recall_calls"`
	RecallErrors   uint64 `json:"recall_errors"`
	RecallChars    uint64 `json:"recall_chars"`
	BgDropped      uint64 `json:"bg_dropped"`
	Consolidations uint64 `json:"consolidations"`
	MergedFacts    int    `json:"merged_facts"`
	ArchivedNodes  int    `json:"archived_nodes"`
	TopicsCreated  int    `json:"topics_created"`
	LastError      string `json:"last_error,omitempty"`
	LastErrorAt    int64  `json:"last_error_at,omitempty"`
	// Backend / Enabled 让界面能显示「现在用的是哪个后端、开着没有」。
	Backend string `json:"backend,omitempty"`
	Enabled bool   `json:"enabled"`
}

// MemoryExportNode 是导出文件里的一个节点（含完整正文）。
type MemoryExportNode struct {
	ID         string  `json:"id"`
	Kind       string  `json:"kind"`
	Title      string  `json:"title,omitempty"`
	Content    string  `json:"content"`
	Importance float64 `json:"importance"`
	Pinned     bool    `json:"pinned"`
	Status     string  `json:"status"`
	SourceRun  string  `json:"source_run,omitempty"`
	UseCount   int     `json:"use_count"`
	CreatedAt  int64   `json:"created_at"`
}

// MemoryExport 是一次完整导出。
type MemoryExport struct {
	Version    int                `json:"version"`
	ExportedAt int64              `json:"exported_at"`
	UserID     string             `json:"user_id,omitempty"`
	Nodes      []MemoryExportNode `json:"nodes"`
	Edges      []MemoryGraphEdge  `json:"edges"`
}
