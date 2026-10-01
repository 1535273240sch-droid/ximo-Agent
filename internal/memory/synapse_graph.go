package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// 本文件是 SynapseBackend 的记忆图读写面（types.MemoryGraphPort，审核文档 4.9
// 的 IPC 面）。前端记忆页与 internal/ipcapi/memory.go 都对着这份契约编码。
//
// 为什么是一个包装类型 SynapseGraph 而不是直接给 *SynapseBackend 加方法：
//
//	types.MemoryGraphPort 要求 Consolidate(ctx) (MemoryGraphMutation, error) 与
//	Stats(ctx) MemoryStats；而 *SynapseBackend 作为 Backend 实现已经有
//	Consolidate(ctx) (ConsolidateResult, error) 与 Stats(ctx) SynapseStats。
//	Go 不允许同类型上两个同名方法，硬改既有签名会破坏 internal/memory 的既有
//	调用方与测试。包装类型用「外层方法遮蔽内嵌方法」解决命名冲突：图接口的
//	Consolidate / Stats 是薄的适配层，真正的实现在 SynapseBackend 上原样保留。
//
// 契约红线：本文件只读 mem_* 表，绝不改动 embedded / mem0 后端的任何行为。

// 记忆图查询的有界上限。
//
// 每一条都是刻意的：IPC 载荷由界面拼出来，界面可以传 limit=10_000_000。没有
// 上限的查询会把整库读进内存（2 万节点基准下这是一次几百毫秒的分配），而且
// 会把渲染成本转嫁到渲染进程。宁可少画一些并如实置 Truncated。
const (
	// DefaultGraphLimit 未指定 limit 时的默认节点数。
	DefaultGraphLimit = 200
	// MaxGraphLimit 单次 Graph 返回的节点数硬上限。
	MaxGraphLimit = 1000
	// MaxGraphDepth CenterOn 邻域的最大跳数（再深就等价于"整张图"）。
	MaxGraphDepth = 3
	// MaxGraphEdges 单次 Graph 返回的边数硬上限。
	MaxGraphEdges = 4000
	// MaxNodeEdges 单节点详情返回的相邻边上限。
	MaxNodeEdges = 500
	// MaxNodeNeighbors 单节点详情返回的邻居上限。
	MaxNodeNeighbors = 200
	// MaxNodeRecalls 单节点详情返回的最近召回记录条数。
	MaxNodeRecalls = 20
	// MaxExportNodes 一次导出的节点数上限。超过就明确报错而不是静默截断：
	// 一份被悄悄截断的备份比一次失败的导出危险得多。
	MaxExportNodes = 50000
	// MaxImportNodes 一次导入接受的节点数上限（同样是为了有界）。
	MaxImportNodes = 50000
	// graphNeighborFanOut 邻域展开时每个节点最多沿多少条边继续走。
	graphNeighborFanOut = 64
	// GraphPreviewRunes 图视图里节点预览正文的字符数。
	GraphPreviewRunes = 160
	// GraphExportVersion 是导出文件格式版本。
	GraphExportVersion = 1
)

// SynapseGraph 把 SynapseBackend 的能力暴露成 types.MemoryGraphPort。
//
// 它内嵌 *SynapseBackend，因此 Backend 侧的能力（Search/Add/GetAll/Delete/
// Ping/Close…）继续可用；十个图接口方法定义在外层，遮蔽同名的内嵌方法。
type SynapseGraph struct {
	*SynapseBackend
}

var _ types.MemoryGraphPort = (*SynapseGraph)(nil)

// NewSynapseGraph 构造图读写面。b 为 nil 时返回 nil（调用方据此走「能力不可用」
// 分支，而不是拿到一个每次调用都报错的非空接口）。
func NewSynapseGraph(b *SynapseBackend) *SynapseGraph {
	if b == nil {
		return nil
	}
	return &SynapseGraph{SynapseBackend: b}
}

// backend 返回底层后端；nil 安全。
func (g *SynapseGraph) backend() *SynapseBackend {
	if g == nil {
		return nil
	}
	return g.SynapseBackend
}

// ready 报告底层库是否可用。
func (g *SynapseGraph) ready() (*SynapseBackend, error) {
	b := g.backend()
	if b == nil || b.db == nil {
		return nil, ErrDisabled
	}
	return b, nil
}

// ---------------------------------------------------------------------------
// Graph：子图查询（分页 / 过滤 / 搜索 / 邻域）
// ---------------------------------------------------------------------------

// Graph 取一个子图。三种模式互斥，优先级 CenterOn > Query > 全量分页：
//
//   - CenterOn 非空：以该节点为中心的 BFS 邻域（Depth 跳，默认 1）；
//   - Query 非空：FTS5 词法命中的节点 + 它们的直接关联（文档 4.9 第 1 条的
//     「搜索高亮」需要把命中项与邻居一起画出来，否则高亮的点孤零零的）；
//   - 否则：按 kind/importance 稳定排序的全量分页。
//
// 无论哪种模式，返回的边都按**惰性衰减后的 EffectiveWeight** 上报：只画原始
// 权重的话，一条 45 天前用过、实际已经很弱的边看起来仍然和刚建立时一样粗。
func (g *SynapseGraph) Graph(ctx context.Context, req types.MemoryGraphRequest) (types.MemoryGraph, error) {
	out := types.MemoryGraph{Nodes: []types.MemoryGraphNode{}, Edges: []types.MemoryGraphEdge{}}
	b, err := g.ready()
	if err != nil {
		return out, err
	}
	now := b.now()
	limit := req.Limit
	if limit <= 0 {
		limit = DefaultGraphLimit
	}
	if limit > MaxGraphLimit {
		limit = MaxGraphLimit
	}
	offset := req.Offset
	if offset < 0 {
		offset = 0
	}
	kinds, err := sanitizeKinds(req.Kinds)
	if err != nil {
		return out, err
	}

	switch {
	case strings.TrimSpace(req.CenterOn) != "":
		return g.graphNeighborhood(ctx, b, req, kinds, limit, now)
	case strings.TrimSpace(req.Query) != "":
		return g.graphQuery(ctx, b, req, kinds, limit, now)
	default:
		return g.graphPage(ctx, b, req, kinds, limit, offset, now)
	}
}

// graphPage 是全量分页模式。
func (g *SynapseGraph) graphPage(ctx context.Context, b *SynapseBackend, req types.MemoryGraphRequest,
	kinds []string, limit, offset int, now time.Time) (types.MemoryGraph, error) {
	out := types.MemoryGraph{Nodes: []types.MemoryGraphNode{}, Edges: []types.MemoryGraphEdge{}}
	where, args := graphFilter(b.opts.UserID, kinds, req.IncludeArchived)

	if err := b.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM mem_nodes WHERE `+where, args...).Scan(&out.Total); err != nil {
		return out, fmt.Errorf("memory: 统计记忆节点失败: %w", err)
	}

	pageArgs := append(append([]any{}, args...), limit, offset)
	rows, err := b.db.QueryContext(ctx, `SELECT `+synapseNodeCols+` FROM mem_nodes WHERE `+where+
		` ORDER BY kind ASC, importance DESC, id ASC LIMIT ? OFFSET ?`, pageArgs...)
	if err != nil {
		return out, fmt.Errorf("memory: 查询记忆节点失败: %w", err)
	}
	nodes, err := scanSynapseNodes(rows)
	if err != nil {
		return out, fmt.Errorf("memory: 读取记忆节点失败: %w", err)
	}
	var edgesCapped bool
	out.Nodes, out.Edges, edgesCapped, err = g.assemble(ctx, b, nodes, now)
	if err != nil {
		return out, err
	}
	out.Truncated = offset+len(nodes) < out.Total || edgesCapped
	return out, nil
}

// graphQuery 是搜索模式：命中项在前，随后补它们的直接关联。
func (g *SynapseGraph) graphQuery(ctx context.Context, b *SynapseBackend, req types.MemoryGraphRequest,
	kinds []string, limit int, now time.Time) (types.MemoryGraph, error) {
	out := types.MemoryGraph{Nodes: []types.MemoryGraphNode{}, Edges: []types.MemoryGraphEdge{}}
	hits, err := b.ftsSearch(ctx, b.db, tokenize(req.Query), limit)
	if err != nil {
		return out, fmt.Errorf("memory: 搜索记忆失败: %w", err)
	}
	// Total 是「命中的节点数」，不含为解释命中而补出来的邻居——否则界面上的
	// 「共 N 条」会把邻居也算成搜索结果。
	out.Total = len(hits)
	out.Truncated = len(hits) >= limit

	seen := make(map[string]bool, len(hits)+8)
	ordered := make([]string, 0, len(hits)+8)
	for _, h := range hits {
		if seen[h.ID] {
			continue
		}
		seen[h.ID] = true
		ordered = append(ordered, h.ID)
	}
	// 补直接关联：只补一跳，且整体仍受 limit 约束。
	for _, id := range ordered {
		if len(ordered) >= limit {
			break
		}
		edges, err := b.edgesFor(ctx, id, graphNeighborFanOut, now)
		if err != nil {
			return out, fmt.Errorf("memory: 展开记忆关联失败: %w", err)
		}
		for _, e := range edges {
			if len(ordered) >= limit {
				break
			}
			other := e.other(id)
			if seen[other] {
				continue
			}
			seen[other] = true
			ordered = append(ordered, other)
		}
	}

	loaded, err := b.getNodes(ctx, b.db, ordered)
	if err != nil {
		return out, fmt.Errorf("memory: 读取记忆节点失败: %w", err)
	}
	nodes := make([]synapseNode, 0, len(ordered))
	for _, id := range ordered {
		n, ok := loaded[id]
		if !ok {
			continue // 已被删除 / 不属于该用户
		}
		if !nodeMatchesFilter(n, kinds, req.IncludeArchived) {
			continue
		}
		nodes = append(nodes, n)
	}
	var edgesCapped bool
	out.Nodes, out.Edges, edgesCapped, err = g.assemble(ctx, b, nodes, now)
	if err != nil {
		return out, err
	}
	out.Truncated = out.Truncated || edgesCapped
	return out, nil
}

// graphNeighborhood 是邻域模式：从 CenterOn 出发做有界 BFS。
//
// 过滤同样作用于展开过程（Kinds / IncludeArchived）：界面上的筛选器是"我只想看
// 这类节点"，如果邻域无视它就会把被筛掉的节点又画回来。
func (g *SynapseGraph) graphNeighborhood(ctx context.Context, b *SynapseBackend, req types.MemoryGraphRequest,
	kinds []string, limit int, now time.Time) (types.MemoryGraph, error) {
	out := types.MemoryGraph{Nodes: []types.MemoryGraphNode{}, Edges: []types.MemoryGraphEdge{}}
	center, ok, err := b.getNode(ctx, b.db, req.CenterOn)
	if err != nil {
		return out, fmt.Errorf("memory: 读取中心节点失败: %w", err)
	}
	if !ok {
		return out, fmt.Errorf("memory: 没有 id=%s 这条记忆", req.CenterOn)
	}
	depth := req.Depth
	if depth <= 0 {
		depth = 1
	}
	if depth > MaxGraphDepth {
		depth = MaxGraphDepth
	}

	// BFS：visited 上限用 MaxGraphLimit（而不是 limit），这样 Total 报的是
	// 真实邻域规模，而不是被分页截断后的数字；返回的节点再按 limit 截断。
	//
	// 每一跳先收集候选 id 再批量回查，而不是每个邻居单独 getNode：深度 2、
	// 每节点 64 条边的邻域会涉及上千个节点，逐条查是上千次往返（基准实测
	// 30ms+ / 26MB 分配），批量 IN 查询把每跳压成一次。
	visited := map[string]synapseNode{center.ID: center}
	order := []string{center.ID}
	frontier := []string{center.ID}
	for hop := 0; hop < depth && len(frontier) > 0; hop++ {
		candidates := make([]string, 0, len(frontier)*graphNeighborFanOut)
		seenCandidate := make(map[string]bool, len(frontier)*graphNeighborFanOut)
		for _, id := range frontier {
			neighbors, err := g.neighborIDs(ctx, b, id, graphNeighborFanOut)
			if err != nil {
				return out, fmt.Errorf("memory: 展开记忆邻域失败: %w", err)
			}
			for _, other := range neighbors {
				if _, ok := visited[other]; ok || seenCandidate[other] {
					continue
				}
				seenCandidate[other] = true
				candidates = append(candidates, other)
			}
			if len(candidates) >= MaxGraphLimit*2 {
				break
			}
		}
		if len(candidates) == 0 {
			break
		}
		loaded, err := b.getNodes(ctx, b.db, candidates)
		if err != nil {
			return out, fmt.Errorf("memory: 读取邻域节点失败: %w", err)
		}
		var next []string
		for _, id := range candidates {
			if len(visited) >= MaxGraphLimit {
				break
			}
			n, ok := loaded[id]
			if !ok || !nodeMatchesFilter(n, kinds, req.IncludeArchived) {
				continue
			}
			visited[id] = n
			order = append(order, id)
			next = append(next, id)
		}
		frontier = next
	}

	out.Total = len(order)
	if len(order) > limit {
		order = order[:limit]
		out.Truncated = true
	}
	nodes := make([]synapseNode, 0, len(order))
	for _, id := range order {
		nodes = append(nodes, visited[id])
	}
	var edgesCapped bool
	out.Nodes, out.Edges, edgesCapped, err = g.assemble(ctx, b, nodes, now)
	if err != nil {
		return out, err
	}
	out.Truncated = out.Truncated || edgesCapped
	return out, nil
}

// assemble 把一批节点补齐度数、边（含有效权重）后组装成图结果。
//
// 边只取"两端都在结果集里"的那些：一条指向未返回节点的边在画布上无处落脚，
// 返回它只会让前端拿到悬空引用。第三个返回值报告边是否触到上限（用于置
// Truncated）。
func (g *SynapseGraph) assemble(ctx context.Context, b *SynapseBackend,
	nodes []synapseNode, now time.Time) ([]types.MemoryGraphNode, []types.MemoryGraphEdge, bool, error) {
	ids := nodeIDs(nodes)
	degrees, err := g.degreesFor(ctx, b, ids)
	if err != nil {
		return nil, nil, false, err
	}
	pinned := make(map[string]bool, len(nodes))
	outNodes := make([]types.MemoryGraphNode, 0, len(nodes))
	for _, n := range nodes {
		pinned[n.ID] = n.Pinned
		outNodes = append(outNodes, toGraphNode(n, degrees[n.ID]))
	}

	edges, capped, err := g.loadEdgesAmong(ctx, b, ids)
	if err != nil {
		return nil, nil, false, err
	}
	return outNodes, g.toGraphEdges(edges, pinned, now), capped, nil
}

// ---------------------------------------------------------------------------
// Node：单节点详情
// ---------------------------------------------------------------------------

// Node 返回一个节点的完整详情：完整正文、相邻边、邻居、最近召回记录。
func (g *SynapseGraph) Node(ctx context.Context, nodeID string) (types.MemoryNodeDetail, error) {
	var out types.MemoryNodeDetail
	b, err := g.ready()
	if err != nil {
		return out, err
	}
	if strings.TrimSpace(nodeID) == "" {
		return out, errors.New("memory: 需要一个节点 id")
	}
	n, ok, err := b.getNode(ctx, b.db, nodeID)
	if err != nil {
		return out, fmt.Errorf("memory: 读取记忆节点失败: %w", err)
	}
	if !ok {
		return out, fmt.Errorf("memory: 没有 id=%s 这条记忆", nodeID)
	}
	now := b.now()

	edges, err := g.incidentEdges(ctx, b, nodeID, MaxNodeEdges)
	if err != nil {
		return out, err
	}
	neighborIDs := make([]string, 0, len(edges))
	seen := map[string]bool{nodeID: true}
	for _, e := range edges {
		other := e.other(nodeID)
		if seen[other] {
			continue
		}
		seen[other] = true
		neighborIDs = append(neighborIDs, other)
	}
	if len(neighborIDs) > MaxNodeNeighbors {
		neighborIDs = neighborIDs[:MaxNodeNeighbors]
	}
	neighbors, err := b.getNodes(ctx, b.db, neighborIDs)
	if err != nil {
		return out, fmt.Errorf("memory: 读取邻居节点失败: %w", err)
	}
	degrees, err := g.degreesFor(ctx, b, neighborIDs)
	if err != nil {
		return out, err
	}
	pinned := map[string]bool{n.ID: n.Pinned}
	outNeighbors := make([]types.MemoryGraphNode, 0, len(neighborIDs))
	for _, id := range neighborIDs {
		nb, ok := neighbors[id]
		if !ok {
			continue
		}
		pinned[id] = nb.Pinned
		outNeighbors = append(outNeighbors, toGraphNode(nb, degrees[id]))
	}

	recalls, err := g.recentRecalls(ctx, b, nodeID)
	if err != nil {
		return out, err
	}
	// 度数必须单独算：incidentEdges 是有界截断的，枢纽节点的边可能远超上限，
	// 用 len(edges) 当度数会让它的圆圈画小了。
	degree, err := g.degreesFor(ctx, b, []string{nodeID})
	if err != nil {
		return out, err
	}

	out = types.MemoryNodeDetail{
		Node:      toGraphNode(n, degree[nodeID]),
		Content:   n.Content,
		Edges:     g.toGraphEdges(edges, pinned, now),
		Neighbors: outNeighbors,
		Recalls:   recalls,
	}
	if out.Edges == nil {
		out.Edges = []types.MemoryGraphEdge{}
	}
	if out.Neighbors == nil {
		out.Neighbors = []types.MemoryGraphNode{}
	}
	return out, nil
}

// recentRecalls 取该节点最近若干条召回记录（4.9 第 3 条的「用过 N 次 / 最近使用」）。
//
// used 的三态（0=待结算 / 1=被采用 / -1=已结算未采用）在对外 DTO 里折成布尔：
// 界面要回答的是「这条记忆有没有真的帮上忙」，而不是内部的结算状态。
func (g *SynapseGraph) recentRecalls(ctx context.Context, b *SynapseBackend, nodeID string) ([]types.MemoryRecallEntry, error) {
	rows, err := b.db.QueryContext(ctx, `
SELECT run_id, COALESCE(via,''), used, ts FROM mem_recall_log
WHERE node_id=? ORDER BY ts DESC LIMIT ?`, nodeID, MaxNodeRecalls)
	if err != nil {
		return nil, fmt.Errorf("memory: 读取召回记录失败: %w", err)
	}
	defer rows.Close()
	out := make([]types.MemoryRecallEntry, 0, MaxNodeRecalls)
	for rows.Next() {
		var runID, via string
		var used int
		var ts int64
		if err := rows.Scan(&runID, &via, &used, &ts); err != nil {
			return nil, fmt.Errorf("memory: 读取召回记录失败: %w", err)
		}
		out = append(out, types.MemoryRecallEntry{
			RunID: runID, Via: via, Used: used == recallLogAdopted, TS: ts,
		})
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// UpdateNode / DeleteNode
// ---------------------------------------------------------------------------

// UpdateNode 改标题 / 正文 / 重要度 / 置顶 / 状态。零值指针表示不改。
//
// 改了 content（或 title）必须重算 tokens 与 hash，并让 FTS 跟上：mem_fts 是
// external-content 表，靠 synapse_store.go 里那三条 AFTER INSERT/UPDATE/DELETE
// 触发器同步——所以这里只要把 tokens 一起 UPDATE 掉，触发器就会用 new.tokens /
// new.title / new.content 重建索引项。**不重算 tokens 会让新正文永远搜不到**，
// 这是 external-content FTS 最典型的坑。
func (g *SynapseGraph) UpdateNode(ctx context.Context, upd types.MemoryNodeUpdate) (types.MemoryNodeDetail, error) {
	var out types.MemoryNodeDetail
	b, err := g.ready()
	if err != nil {
		return out, err
	}
	nodeID := strings.TrimSpace(upd.NodeID)
	if nodeID == "" {
		return out, errors.New("memory: 需要一个节点 id")
	}
	cur, ok, err := b.getNode(ctx, b.db, nodeID)
	if err != nil {
		return out, fmt.Errorf("memory: 读取记忆节点失败: %w", err)
	}
	if !ok {
		return out, fmt.Errorf("memory: 没有 id=%s 这条记忆", nodeID)
	}

	title, content := cur.Title, cur.Content
	contentChanged := false
	sets := make([]string, 0, 6)
	args := make([]any, 0, 8)

	if upd.Title != nil {
		title = strings.TrimSpace(types.RedactString(*upd.Title))
		sets = append(sets, "title=?")
		args = append(args, title)
	}
	if upd.Content != nil {
		content = strings.TrimSpace(types.RedactString(*upd.Content))
		if content == "" {
			// 空正文会让归一化哈希退化成"空串哈希"，并让这条记忆在所有搜索里
			// 消失。用户想删掉它应该用 DeleteNode，而不是把正文清空。
			return out, errors.New("memory: 正文不能为空；要移除这条记忆请用删除")
		}
		sets = append(sets, "content=?")
		args = append(args, content)
		contentChanged = true
	}
	if upd.Importance != nil {
		if *upd.Importance < 0 || *upd.Importance > 1 {
			return out, fmt.Errorf("memory: importance 必须在 [0,1]，当前为 %v", *upd.Importance)
		}
		sets = append(sets, "importance=?")
		args = append(args, *upd.Importance)
	}
	if upd.Pinned != nil {
		sets = append(sets, "pinned=?")
		args = append(args, boolInt(*upd.Pinned))
	}
	if upd.Status != nil {
		if !validGraphStatus(*upd.Status) {
			return out, fmt.Errorf("memory: 未知状态 %q", *upd.Status)
		}
		sets = append(sets, "status=?")
		args = append(args, *upd.Status)
	}

	if len(sets) > 0 {
		newHash := cur.Hash
		if contentChanged {
			newHash = contentHash(content)
			var clash string
			err := b.db.QueryRowContext(ctx,
				`SELECT id FROM mem_nodes WHERE user_id=? AND kind=? AND hash=? AND id<>?`,
				b.opts.UserID, cur.Kind, newHash, nodeID).Scan(&clash)
			switch {
			case err == nil:
				return out, fmt.Errorf("memory: 另一条记忆（id=%s）已有相同正文，未做修改", clash)
			case !errors.Is(err, sql.ErrNoRows):
				return out, fmt.Errorf("memory: 校验正文去重失败: %w", err)
			}
			sets = append(sets, "hash=?")
			args = append(args, newHash)
		}
		if contentChanged || upd.Title != nil {
			sets = append(sets, "tokens=?")
			args = append(args, synapseTokens(title, content))
		}
		args = append(args, nodeID, b.opts.UserID)
		if _, err := b.db.ExecContext(ctx,
			`UPDATE mem_nodes SET `+strings.Join(sets, ", ")+` WHERE id=? AND user_id=?`, args...); err != nil {
			return out, fmt.Errorf("memory: 更新记忆失败: %w", err)
		}
	}
	return g.Node(ctx, nodeID)
}

// DeleteNode 硬删除节点及其所有边（"忘掉"，不是归档）。
//
// mem_edges 的外键带 ON DELETE CASCADE（且连接上开了 foreign_keys=ON），
// 但这里仍然显式删一次边：显式语句让"删掉了什么"在代码里可读，也不依赖
// 每个连接都恰好开着外键开关。mem_recall_log 没有外键，必须单独清理，
// 否则会留下指向已删节点的悬空召回记录。
func (g *SynapseGraph) DeleteNode(ctx context.Context, nodeID string) error {
	b, err := g.ready()
	if err != nil {
		return err
	}
	nodeID = strings.TrimSpace(nodeID)
	if nodeID == "" {
		return errors.New("memory: 需要一个节点 id")
	}
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("memory: 开始删除事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM mem_recall_log WHERE node_id=?`, nodeID); err != nil {
		return fmt.Errorf("memory: 清理召回记录失败: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM mem_edges WHERE src=? OR dst=?`, nodeID, nodeID); err != nil {
		return fmt.Errorf("memory: 删除记忆边失败: %w", err)
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM mem_nodes WHERE id=? AND user_id=?`, nodeID, b.opts.UserID)
	if err != nil {
		return fmt.Errorf("memory: 删除记忆失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("memory: 没有 id=%s 这条记忆", nodeID)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("memory: 提交删除失败: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Link
// ---------------------------------------------------------------------------

// Link 显式建立一条边；已存在则**覆盖**权重（用户手动拉粗/拉细一条连线是明确
// 的意图，不该被写入流水线那条"取两者较大者"的健壮性策略挡掉——那条策略是为
// 自动学习准备的，不是为人工编辑准备的）。
func (g *SynapseGraph) Link(ctx context.Context, link types.MemoryLink) (types.MemoryGraphMutation, error) {
	var out types.MemoryGraphMutation
	b, err := g.ready()
	if err != nil {
		return out, err
	}
	src := strings.TrimSpace(link.Src)
	dst := strings.TrimSpace(link.Dst)
	if src == "" || dst == "" {
		return out, errors.New("memory: src 与 dst 都不能为空")
	}
	if src == dst {
		return out, errors.New("memory: 节点不能连到自己")
	}
	rel := strings.TrimSpace(link.Rel)
	if rel == "" {
		rel = RelRelated
	}
	if !validGraphRel(rel) {
		return out, fmt.Errorf("memory: 未知关系 %q", rel)
	}
	if _, ok, err := b.getNode(ctx, b.db, src); err != nil {
		return out, fmt.Errorf("memory: 读取起点失败: %w", err)
	} else if !ok {
		return out, fmt.Errorf("memory: 没有 id=%s 这条记忆", src)
	}
	if _, ok, err := b.getNode(ctx, b.db, dst); err != nil {
		return out, fmt.Errorf("memory: 读取终点失败: %w", err)
	} else if !ok {
		return out, fmt.Errorf("memory: 没有 id=%s 这条记忆", dst)
	}

	weight := link.Weight
	if weight <= 0 {
		weight = defaultRelWeight(rel)
	}
	weight = clamp01(weight)

	var existed int
	err = b.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM mem_edges WHERE src=? AND dst=? AND rel=?`,
		src, dst, rel).Scan(&existed)
	if err != nil {
		return out, fmt.Errorf("memory: 查询已有边失败: %w", err)
	}
	now := b.now()
	if _, err := b.db.ExecContext(ctx, `
INSERT INTO mem_edges (src,dst,rel,weight,fire_count,last_fired,created_at)
VALUES (?,?,?,?,0,NULL,?)
ON CONFLICT(src,dst,rel) DO UPDATE SET weight=excluded.weight`,
		src, dst, rel, weight, now.Unix()); err != nil {
		return out, fmt.Errorf("memory: 写入记忆边失败: %w", err)
	}

	verb := "建立"
	if existed > 0 {
		verb = "更新"
	}
	out = types.MemoryGraphMutation{
		OK: true, Affected: 1,
		Notes: []string{fmt.Sprintf("%s %s --%s--> %s（权重 %.2f）", verb, src, rel, dst, weight)},
	}
	return out, nil
}

// defaultRelWeight 给出一条边的默认初始权重（与写入流水线的常量保持一致）。
func defaultRelWeight(rel string) float64 {
	switch rel {
	case RelMentions:
		return SynapseWeightMentions
	case RelRelated:
		return SynapseWeightRelated
	case RelDerivedFrom:
		return SynapseWeightDerivedFrom
	case RelUsedWith:
		return SynapseUsedWithInit
	default:
		return 0.3
	}
}

// ---------------------------------------------------------------------------
// Consolidate
// ---------------------------------------------------------------------------

// Consolidate 手动触发一次「睡眠整理」（复用 SynapseBackend 的实现）。
//
// 这个方法之所以必须定义在外层：内嵌的 SynapseBackend.Consolidate 返回
// ConsolidateResult，与端口要求的 MemoryGraphMutation 不是同一个形状。
func (g *SynapseGraph) Consolidate(ctx context.Context) (types.MemoryGraphMutation, error) {
	var out types.MemoryGraphMutation
	b, err := g.ready()
	if err != nil {
		return out, err
	}
	res, err := b.Consolidate(ctx)
	if err != nil {
		return out, err
	}
	stats := g.Stats(ctx)
	affected := res.MergedFacts + res.TopicsCreated + res.ContradictionsResolved +
		res.ArchivedNodes + res.PrunedEdges
	notes := []string{
		fmt.Sprintf("合并重复事实 %d 条", res.MergedFacts),
		fmt.Sprintf("新建主题 %d 个", res.TopicsCreated),
		fmt.Sprintf("裁决矛盾 %d 对", res.ContradictionsResolved),
		fmt.Sprintf("归档长期未用节点 %d 个", res.ArchivedNodes),
		fmt.Sprintf("清理失效边 %d 条", res.PrunedEdges),
		fmt.Sprintf("清理过期召回日志 %d 条", res.RecallLogPruned),
	}
	if res.Interrupted {
		notes = append(notes, "整理到达时限后中断，已完成的部分已保留")
	}
	out = types.MemoryGraphMutation{OK: true, Affected: affected, Notes: notes, Stats: &stats}
	return out, nil
}

// ---------------------------------------------------------------------------
// Export / Import
// ---------------------------------------------------------------------------

// Export 导出该用户的全部记忆（含完整正文）与边，用于备份 / 迁移。
func (g *SynapseGraph) Export(ctx context.Context) (types.MemoryExport, error) {
	out := types.MemoryExport{Version: GraphExportVersion, Nodes: []types.MemoryExportNode{}, Edges: []types.MemoryGraphEdge{}}
	b, err := g.ready()
	if err != nil {
		return out, err
	}
	now := b.now()

	var total int
	if err := b.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM mem_nodes WHERE user_id=?`, b.opts.UserID).Scan(&total); err != nil {
		return out, fmt.Errorf("memory: 统计导出节点失败: %w", err)
	}
	if total > MaxExportNodes {
		return out, fmt.Errorf("memory: 记忆节点过多（%d > %d），拒绝导出以免静默截断备份", total, MaxExportNodes)
	}

	rows, err := b.db.QueryContext(ctx, `SELECT `+synapseNodeCols+
		` FROM mem_nodes WHERE user_id=? ORDER BY created_at, id`, b.opts.UserID)
	if err != nil {
		return out, fmt.Errorf("memory: 读取导出节点失败: %w", err)
	}
	nodes, err := scanSynapseNodes(rows)
	if err != nil {
		return out, fmt.Errorf("memory: 读取导出节点失败: %w", err)
	}
	pinned := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		pinned[n.ID] = n.Pinned
		out.Nodes = append(out.Nodes, types.MemoryExportNode{
			ID: n.ID, Kind: n.Kind, Title: n.Title, Content: n.Content,
			Importance: n.Importance, Pinned: n.Pinned, Status: n.Status,
			SourceRun: n.SourceRun, UseCount: n.UseCount, CreatedAt: n.CreatedAt.Unix(),
		})
	}

	edges, err := g.userEdges(ctx, b)
	if err != nil {
		return out, err
	}
	out.Edges = g.toGraphEdges(edges, pinned, now)
	out.ExportedAt = now.Unix()
	out.UserID = b.opts.UserID
	return out, nil
}

// Import 导入一份导出文件：按内容哈希幂等合并，不覆盖已有内容。
//
// 幂等的含义：重复导入同一份文件，第二次不会新增任何节点，也不会改变已有节点的
// 正文/重要度/置顶/状态——用户导进来的东西不能悄悄盖掉他现在正在用的记忆。
// 边只在"两端都存在于库里"时才建立，且已存在的边保持原权重。
func (g *SynapseGraph) Import(ctx context.Context, in types.MemoryExport) (types.MemoryGraphMutation, error) {
	var out types.MemoryGraphMutation
	b, err := g.ready()
	if err != nil {
		return out, err
	}
	if len(in.Nodes) == 0 {
		return out, errors.New("memory: 导入内容里没有任何节点")
	}
	if len(in.Nodes) > MaxImportNodes {
		return out, fmt.Errorf("memory: 导入节点过多（%d > %d）", len(in.Nodes), MaxImportNodes)
	}
	now := b.now()
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return out, fmt.Errorf("memory: 开始导入事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	idMap := make(map[string]string, len(in.Nodes))
	created, existing, skipped := 0, 0, 0
	for _, n := range in.Nodes {
		kind := n.Kind
		if !validGraphKind(kind) {
			kind = NodeFact
		}
		content := strings.TrimSpace(types.RedactString(n.Content))
		if content == "" {
			skipped++
			continue
		}
		hash := contentHash(content)
		title := strings.TrimSpace(types.RedactString(n.Title))
		if title == "" {
			title = truncateRunes(content, 32)
		}
		status := n.Status
		if !validGraphStatus(status) {
			status = StatusActive
		}
		createdAt := now
		if n.CreatedAt > 0 {
			createdAt = time.Unix(n.CreatedAt, 0).UTC()
		}

		var found string
		err := tx.QueryRowContext(ctx, `SELECT id FROM mem_nodes WHERE user_id=? AND kind=? AND hash=?`,
			b.opts.UserID, kind, hash).Scan(&found)
		switch {
		case err == nil:
			// 已存在：只记映射，绝不覆盖（"不覆盖已有内容"）。
			idMap[n.ID] = found
			existing++
			continue
		case !errors.Is(err, sql.ErrNoRows):
			return out, fmt.Errorf("memory: 导入去重查询失败: %w", err)
		}

		newID := newSynapseID("mn_")
		useCount := n.UseCount
		if useCount < 0 {
			useCount = 0
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO mem_nodes (id,user_id,kind,title,content,tokens,importance,pinned,status,source_run,hash,embedding,emb_model,created_at,last_used,use_count,meta)
VALUES (?,?,?,?,?,?,?,?,?,?,?,NULL,NULL,?,NULL,?, '{}')`,
			newID, b.opts.UserID, kind, title, content, synapseTokens(title, content),
			clamp01(n.Importance), boolInt(n.Pinned), status, nullIfEmpty(n.SourceRun),
			hash, createdAt.Unix(), useCount); err != nil {
			return out, fmt.Errorf("memory: 导入节点失败: %w", err)
		}
		idMap[n.ID] = newID
		created++
	}

	edgeCreated, edgeSkipped := 0, 0
	for _, e := range in.Edges {
		src, okSrc := idMap[e.Src]
		dst, okDst := idMap[e.Dst]
		if !okSrc || !okDst || src == dst {
			// 两端必须都存在（可能是被跳过的空节点，或导出文件来自另一份数据）。
			edgeSkipped++
			continue
		}
		rel := e.Rel
		if !validGraphRel(rel) {
			rel = RelRelated
		}
		weight := e.Weight
		if weight <= 0 {
			weight = defaultRelWeight(rel)
		}
		res, err := tx.ExecContext(ctx, `
INSERT INTO mem_edges (src,dst,rel,weight,fire_count,last_fired,created_at)
VALUES (?,?,?,?,0,NULL,?)
ON CONFLICT(src,dst,rel) DO NOTHING`,
			src, dst, rel, clamp01(weight), now.Unix())
		if err != nil {
			return out, fmt.Errorf("memory: 导入边失败: %w", err)
		}
		if n, err := res.RowsAffected(); err == nil && n > 0 {
			edgeCreated++
		}
	}

	if err := tx.Commit(); err != nil {
		return out, fmt.Errorf("memory: 提交导入失败: %w", err)
	}
	stats := g.Stats(ctx)
	notes := []string{
		fmt.Sprintf("新建节点 %d 个", created),
		fmt.Sprintf("已存在（未覆盖）%d 个", existing),
		fmt.Sprintf("新建边 %d 条", edgeCreated),
	}
	if skipped > 0 {
		notes = append(notes, fmt.Sprintf("跳过空内容节点 %d 个", skipped))
	}
	if edgeSkipped > 0 {
		notes = append(notes, fmt.Sprintf("跳过端点不存在的边 %d 条", edgeSkipped))
	}
	out = types.MemoryGraphMutation{OK: true, Affected: created, Notes: notes, Stats: &stats}
	return out, nil
}

// ---------------------------------------------------------------------------
// Stats / Clear
// ---------------------------------------------------------------------------

// Stats 返回计数快照，并标明后端名（复用 SynapseStats，不另起一套计数）。
func (g *SynapseGraph) Stats(ctx context.Context) types.MemoryStats {
	b := g.backend()
	if b == nil || b.db == nil {
		return types.MemoryStats{Backend: BackendSynapse, Enabled: false}
	}
	s := b.Stats(ctx)
	out := types.MemoryStats{
		UserID:         s.UserID,
		Nodes:          s.Nodes,
		Edges:          s.Edges,
		RecallCalls:    s.RecallCalls,
		RecallErrors:   s.RecallErrors,
		RecallChars:    s.RecallChars,
		BgDropped:      s.BgDropped,
		Consolidations: s.Consolidations,
		MergedFacts:    s.MergedFacts,
		ArchivedNodes:  s.ArchivedNodes,
		TopicsCreated:  s.TopicsCreated,
		LastError:      s.LastError,
		Backend:        BackendSynapse,
		Enabled:        true,
	}
	if !s.LastErrorAt.IsZero() {
		out.LastErrorAt = s.LastErrorAt.Unix()
	}
	return out
}

// Clear 清空该用户的全部节点、边与召回日志。
//
// 为什么是 DELETE 而不是删库文件：
//
//  1. 库文件此刻被本进程的连接池持有（单写者、WAL）。Windows 上删除一个仍被
//     打开的文件会直接失败（ERROR_SHARING_VIOLATION），而且失败点在半途——
//     文件可能已经不见了、-wal/-shm 还在，留下一个半开状态的库。
//  2. 删文件还会连带清掉别的用户/别的用途共用这张库的数据。DELETE ... WHERE
//     user_id=? 的作用域是明确的。
//  3. 库文件里除了记忆还有 schema；保留它意味着清空之后不需要再走一次建表，
//     也不会让正在进行的查询拿到"表不存在"的错误。
//
// 行删除会触发 mem_nodes 的 AFTER DELETE 触发器，mem_fts 随之同步。
func (g *SynapseGraph) Clear(ctx context.Context) (types.MemoryGraphMutation, error) {
	var out types.MemoryGraphMutation
	b, err := g.ready()
	if err != nil {
		return out, err
	}
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return out, fmt.Errorf("memory: 开始清空事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `
DELETE FROM mem_recall_log WHERE node_id IN (SELECT id FROM mem_nodes WHERE user_id=?)`,
		b.opts.UserID); err != nil {
		return out, fmt.Errorf("memory: 清空召回日志失败: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
DELETE FROM mem_edges WHERE src IN (SELECT id FROM mem_nodes WHERE user_id=?)
   OR dst IN (SELECT id FROM mem_nodes WHERE user_id=?)`,
		b.opts.UserID, b.opts.UserID); err != nil {
		return out, fmt.Errorf("memory: 清空记忆边失败: %w", err)
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM mem_nodes WHERE user_id=?`, b.opts.UserID)
	if err != nil {
		return out, fmt.Errorf("memory: 清空记忆失败: %w", err)
	}
	removed := 0
	if n, err := res.RowsAffected(); err == nil {
		removed = int(n)
	}
	if err := tx.Commit(); err != nil {
		return out, fmt.Errorf("memory: 提交清空失败: %w", err)
	}
	stats := g.Stats(ctx)
	out = types.MemoryGraphMutation{
		OK: true, Affected: removed,
		Notes: []string{
			fmt.Sprintf("已删除 %d 个记忆节点及其全部边与召回记录", removed),
			"库文件保留：连接仍被持有，删除文件在 Windows 上会失败并留下半开状态",
		},
		Stats: &stats,
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 查询小工具
// ---------------------------------------------------------------------------

// graphFilter 构造节点过滤条件（user + kind + archived）。
func graphFilter(userID string, kinds []string, includeArchived bool) (string, []any) {
	where := "user_id=?"
	args := []any{userID}
	if len(kinds) > 0 {
		ph := graphPlaceholders(len(kinds))
		where += " AND kind IN (" + ph + ")"
		for _, k := range kinds {
			args = append(args, k)
		}
	}
	if !includeArchived {
		// 只排除 archived：superseded 仍然画出来——"这条被新结论取代了"本身
		// 是图上一条有价值的信息（用户能看到记忆是怎么演化的）。
		where += " AND status<>?"
		args = append(args, StatusArchived)
	}
	return where, args
}

// nodeMatchesFilter 判断一个节点是否通过过滤（邻域 / 搜索模式的 Go 侧过滤）。
func nodeMatchesFilter(n synapseNode, kinds []string, includeArchived bool) bool {
	if len(kinds) > 0 {
		hit := false
		for _, k := range kinds {
			if k == n.Kind {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
	}
	if !includeArchived && n.Status == StatusArchived {
		return false
	}
	return true
}

// sanitizeKinds 把请求里的 kind 收窄到合法集合；未知值直接报错而不是静默忽略
// （静默忽略会让界面上的筛选看起来"生效了"却什么也没筛掉）。
func sanitizeKinds(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, k := range in {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		if !validGraphKind(k) {
			return nil, fmt.Errorf("memory: 未知节点类型 %q", k)
		}
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, k)
	}
	return out, nil
}

// validGraphKind 报告 kind 是否是 mem_nodes.kind 允许的取值。
func validGraphKind(k string) bool {
	switch k {
	case NodeFact, NodeEntity, NodeEpisode, NodeProcedure, NodeTopic:
		return true
	default:
		return false
	}
}

// validGraphStatus 报告 status 是否是 mem_nodes.status 允许的取值。
func validGraphStatus(s string) bool {
	switch s {
	case StatusActive, StatusSuperseded, StatusArchived:
		return true
	default:
		return false
	}
}

// validGraphRel 报告 rel 是否是 mem_edges.rel 的 CHECK 允许的取值。
func validGraphRel(rel string) bool {
	for _, r := range types.AllMemoryRels {
		if r == rel {
			return true
		}
	}
	return false
}

// graphPlaceholders 返回 n 个 "?" 的逗号列表。
func graphPlaceholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// toGraphNode 把一个存储行转成图视图节点。
func toGraphNode(n synapseNode, degree int) types.MemoryGraphNode {
	out := types.MemoryGraphNode{
		ID:             n.ID,
		Kind:           n.Kind,
		Title:          n.Title,
		ContentPreview: truncateRunes(strings.Join(strings.Fields(n.Content), " "), GraphPreviewRunes),
		Importance:     n.Importance,
		Pinned:         n.Pinned,
		Status:         n.Status,
		SourceRun:      n.SourceRun,
		UseCount:       n.UseCount,
		Degree:         degree,
		CreatedAt:      n.CreatedAt.Unix(),
	}
	if !n.LastUsed.IsZero() {
		out.LastUsed = n.LastUsed.Unix()
	}
	return out
}

// toGraphEdges 把存储边转成对外边：Weight 是原始权重，EffectiveWeight 是惰性
// 衰减后的当前强度（图视图必须按后者画粗细）。
func (g *SynapseGraph) toGraphEdges(edges []synapseEdge, pinned map[string]bool, now time.Time) []types.MemoryGraphEdge {
	out := make([]types.MemoryGraphEdge, 0, len(edges))
	for _, e := range edges {
		eff := edgeEffectiveWeight(e, pinned[e.Src] || pinned[e.Dst], now)
		out = append(out, types.MemoryGraphEdge{
			Src: e.Src, Dst: e.Dst, Rel: e.Rel,
			Weight: clamp01(e.Weight), EffectiveWeight: eff, FireCount: e.FireCount,
		})
	}
	return out
}

// degreesFor 返回这些节点在全图里的度数（入边 + 出边）。
//
// 度数不能从"当前结果集里的边"数出来：分页之后跨页的边不在结果里，枢纽节点的
// 圆圈会缩成普通大小。这里直接在库上按端点 GROUP BY，一次算清。
func (g *SynapseGraph) degreesFor(ctx context.Context, b *SynapseBackend, ids []string) (map[string]int, error) {
	out := make(map[string]int, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	ph := graphPlaceholders(len(ids))
	args := make([]any, 0, 2*len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	all := append(append([]any{}, args...), args...)
	rows, err := b.db.QueryContext(ctx, `
SELECT id, SUM(c) FROM (
  SELECT src AS id, COUNT(*) AS c FROM mem_edges WHERE src IN (`+ph+`) GROUP BY src
  UNION ALL
  SELECT dst AS id, COUNT(*) AS c FROM mem_edges WHERE dst IN (`+ph+`) GROUP BY dst
) GROUP BY id`, all...)
	if err != nil {
		return nil, fmt.Errorf("memory: 统计节点度数失败: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var c int
		if err := rows.Scan(&id, &c); err != nil {
			return nil, fmt.Errorf("memory: 统计节点度数失败: %w", err)
		}
		out[id] = c
	}
	return out, rows.Err()
}

// neighborIDs 只取一个节点的邻居 id（BFS 展开用，不读边本身）。
//
// 为什么不复用 edgesFor：那个函数为了"按有效权重排序后取前 N 条"会在 SQL 里
// 先捞 fanOut×25 条边再在 Go 侧排序（对召回是对的，见它的注释）。图邻域展开
// 只需要"连到了谁"，不需要权重，用它会为每个前沿节点多分配上千个边结构
// （20k 节点基准：深度 2 邻域 26MB 分配，几乎全在这里）。这里直接取 id。
func (g *SynapseGraph) neighborIDs(ctx context.Context, b *SynapseBackend, id string, limit int) ([]string, error) {
	if limit <= 0 {
		limit = graphNeighborFanOut
	}
	rows, err := b.db.QueryContext(ctx, `
SELECT id FROM (SELECT dst AS id, weight AS w FROM mem_edges WHERE src=? ORDER BY weight DESC LIMIT ?)
UNION
SELECT id FROM (SELECT src AS id, weight AS w FROM mem_edges WHERE dst=? ORDER BY weight DESC LIMIT ?)`,
		id, limit, id, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]string, 0, limit*2)
	for rows.Next() {
		var other string
		if err := rows.Scan(&other); err != nil {
			return nil, err
		}
		out = append(out, other)
	}
	return out, rows.Err()
}

// incidentEdges 取一个节点的全部相邻边（保留原始权重），按原始权重降序截断。
func (g *SynapseGraph) incidentEdges(ctx context.Context, b *SynapseBackend, id string, limit int) ([]synapseEdge, error) {
	if limit <= 0 {
		limit = MaxNodeEdges
	}
	rows, err := b.db.QueryContext(ctx, `
SELECT * FROM (
  SELECT src,dst,rel,weight,fire_count,COALESCE(last_fired,0),created_at
  FROM mem_edges WHERE src=? ORDER BY weight DESC LIMIT ?
)
UNION ALL
SELECT * FROM (
  SELECT src,dst,rel,weight,fire_count,COALESCE(last_fired,0),created_at
  FROM mem_edges WHERE dst=? ORDER BY weight DESC LIMIT ?
)`, id, limit, id, limit)
	if err != nil {
		return nil, fmt.Errorf("memory: 读取相邻边失败: %w", err)
	}
	defer rows.Close()
	edges, err := scanRawEdgeRows(rows, limit)
	if err != nil {
		return nil, fmt.Errorf("memory: 读取相邻边失败: %w", err)
	}
	return dedupeEdges(edges), nil
}

// loadEdgesAmong 取两端都在 ids 里的边（有界）。返回的第二个值报告是否触到上限。
func (g *SynapseGraph) loadEdgesAmong(ctx context.Context, b *SynapseBackend, ids []string) ([]synapseEdge, bool, error) {
	if len(ids) == 0 {
		return nil, false, nil
	}
	ph := graphPlaceholders(len(ids))
	args := make([]any, 0, 2*len(ids)+1)
	for _, id := range ids {
		args = append(args, id)
	}
	for _, id := range ids {
		args = append(args, id)
	}
	args = append(args, MaxGraphEdges)
	rows, err := b.db.QueryContext(ctx, `
SELECT src,dst,rel,weight,fire_count,COALESCE(last_fired,0),created_at
FROM mem_edges WHERE src IN (`+ph+`) AND dst IN (`+ph+`)
ORDER BY weight DESC LIMIT ?`, args...)
	if err != nil {
		return nil, false, fmt.Errorf("memory: 读取图边失败: %w", err)
	}
	defer rows.Close()
	edges, err := scanRawEdgeRows(rows, MaxGraphEdges)
	if err != nil {
		return nil, false, fmt.Errorf("memory: 读取图边失败: %w", err)
	}
	return edges, len(edges) >= MaxGraphEdges, nil
}

// userEdges 取该用户节点的全部出边（导出用）。
func (g *SynapseGraph) userEdges(ctx context.Context, b *SynapseBackend) ([]synapseEdge, error) {
	rows, err := b.db.QueryContext(ctx, `
SELECT e.src,e.dst,e.rel,e.weight,e.fire_count,COALESCE(e.last_fired,0),e.created_at
FROM mem_edges e JOIN mem_nodes n ON n.id = e.src
WHERE n.user_id=?`, b.opts.UserID)
	if err != nil {
		return nil, fmt.Errorf("memory: 读取导出边失败: %w", err)
	}
	defer rows.Close()
	edges, err := scanRawEdgeRows(rows, 0)
	if err != nil {
		return nil, fmt.Errorf("memory: 读取导出边失败: %w", err)
	}
	return edges, nil
}

// scanRawEdgeRows 读原始边行（保留存储权重，不在这里做衰减）。
func scanRawEdgeRows(rows *sql.Rows, capacity int) ([]synapseEdge, error) {
	if capacity < 0 {
		capacity = 0
	}
	out := make([]synapseEdge, 0, capacity)
	for rows.Next() {
		var e synapseEdge
		var fire, lastFired, created int64
		if err := rows.Scan(&e.Src, &e.Dst, &e.Rel, &e.Weight, &fire, &lastFired, &created); err != nil {
			return nil, err
		}
		e.FireCount = int(fire)
		if lastFired > 0 {
			e.LastFired = time.Unix(lastFired, 0).UTC()
		}
		e.CreatedAt = time.Unix(created, 0).UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

// dedupeEdges 按 (src,dst,rel) 去重（incidentEdges 的两个分支不会重叠，但去重
// 让"同一节点自环"这类边界也不会画两遍）。
func dedupeEdges(in []synapseEdge) []synapseEdge {
	if len(in) <= 1 {
		return in
	}
	seen := make(map[string]bool, len(in))
	out := make([]synapseEdge, 0, len(in))
	for _, e := range in {
		key := e.Src + "\x00" + e.Dst + "\x00" + e.Rel
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, e)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Weight > out[j].Weight })
	return out
}

// scanSynapseNodes 读完一个 mem_nodes 结果集。
func scanSynapseNodes(rows *sql.Rows) ([]synapseNode, error) {
	defer rows.Close()
	out := make([]synapseNode, 0, 16)
	for rows.Next() {
		n, err := scanSynapseNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
