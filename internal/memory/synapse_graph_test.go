package memory

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// 本文件是记忆图读写面（types.MemoryGraphPort）的验收测试。全部用临时库，
// 生产路径不出现任何模拟数据。

// graphClock 是可推进的注入时钟（衰减测试必须能"把时间拨到 45 天后"）。
type graphClock struct {
	mu sync.Mutex
	t  time.Time
}

func newGraphClock() *graphClock {
	return &graphClock{t: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
}

func (c *graphClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *graphClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// newGraphFixture 开一个临时库上的 synapse 后端 + 图读写面。
func newGraphFixture(t *testing.T, opts SynapseOptions) (*SynapseBackend, *SynapseGraph) {
	t.Helper()
	if opts.UserID == "" {
		opts.UserID = "u1"
	}
	b, err := NewSynapseBackend(filepath.Join(t.TempDir(), "memory.db"), opts)
	if err != nil {
		t.Fatalf("打开 synapse 后端: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b, NewSynapseGraph(b)
}

// mkNode 直接落一个节点（绕开抽取器，保证测试确定）。
func mkNode(t *testing.T, b *SynapseBackend, kind, title, content string) string {
	t.Helper()
	id, _, err := b.insertNode(context.Background(), b.db, synapseNode{
		ID: newSynapseID("mn_"), Kind: kind, Title: title, Content: content, CreatedAt: b.now(),
	})
	if err != nil {
		t.Fatalf("写节点 %q: %v", content, err)
	}
	return id
}

func graphNodeIDs(nodes []types.MemoryGraphNode) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.ID)
	}
	return out
}

// ---------------------------------------------------------------------------
// Graph：分页 / 过滤 / 邻域 / 搜索
// ---------------------------------------------------------------------------

func TestSynapseGraphPaginationAndFilters(t *testing.T) {
	b, g := newGraphFixture(t, SynapseOptions{})
	ctx := context.Background()

	ids := make([]string, 0, 5)
	for _, text := range []string{"事实一", "事实二", "事实三", "事实四", "事实五"} {
		ids = append(ids, mkNode(t, b, NodeFact, text, text))
	}
	entityID := mkNode(t, b, NodeEntity, "XimoAgent", "XimoAgent")

	// 分页：两页各 2 条，互不重叠，合计 4 条，Total 报全量 5。
	page1, err := g.Graph(ctx, types.MemoryGraphRequest{Kinds: []string{NodeFact}, Limit: 2})
	if err != nil {
		t.Fatalf("Graph 第一页: %v", err)
	}
	if len(page1.Nodes) != 2 || page1.Total != 5 || !page1.Truncated {
		t.Fatalf("第一页 = %d 节点 / total %d / truncated %v，期望 2/5/true",
			len(page1.Nodes), page1.Total, page1.Truncated)
	}
	page2, err := g.Graph(ctx, types.MemoryGraphRequest{Kinds: []string{NodeFact}, Limit: 2, Offset: 2})
	if err != nil {
		t.Fatalf("Graph 第二页: %v", err)
	}
	if len(page2.Nodes) != 2 || page2.Total != 5 {
		t.Fatalf("第二页 = %d 节点 / total %d，期望 2/5", len(page2.Nodes), page2.Total)
	}
	seen := map[string]bool{}
	for _, id := range append(graphNodeIDs(page1.Nodes), graphNodeIDs(page2.Nodes)...) {
		if seen[id] {
			t.Fatalf("分页出现重复节点 %s", id)
		}
		seen[id] = true
	}

	// 过滤：只要 entity。
	onlyEntity, err := g.Graph(ctx, types.MemoryGraphRequest{Kinds: []string{NodeEntity}})
	if err != nil {
		t.Fatalf("Graph entity 过滤: %v", err)
	}
	if len(onlyEntity.Nodes) != 1 || onlyEntity.Nodes[0].ID != entityID || onlyEntity.Total != 1 {
		t.Fatalf("entity 过滤 = %+v，期望只有 %s", onlyEntity.Nodes, entityID)
	}

	// 未知 kind 必须报错，而不是静默返回空图。
	if _, err := g.Graph(ctx, types.MemoryGraphRequest{Kinds: []string{"nonsense"}}); err == nil {
		t.Fatal("未知 kind 应当报错")
	}

	// 归档过滤：默认不返回 archived，IncludeArchived 时返回。
	if _, err := g.UpdateNode(ctx, types.MemoryNodeUpdate{
		NodeID: ids[0], Status: strPtr(StatusArchived),
	}); err != nil {
		t.Fatalf("归档节点: %v", err)
	}
	active, err := g.Graph(ctx, types.MemoryGraphRequest{Kinds: []string{NodeFact}})
	if err != nil {
		t.Fatalf("Graph 默认过滤: %v", err)
	}
	if active.Total != 4 {
		t.Fatalf("默认应排除 archived，Total=%d，期望 4", active.Total)
	}
	withArchived, err := g.Graph(ctx, types.MemoryGraphRequest{
		Kinds: []string{NodeFact}, IncludeArchived: true,
	})
	if err != nil {
		t.Fatalf("Graph IncludeArchived: %v", err)
	}
	if withArchived.Total != 5 {
		t.Fatalf("IncludeArchived 应返回 5，得到 %d", withArchived.Total)
	}
}

func strPtr(s string) *string { return &s }

func TestSynapseGraphNeighborhoodAndSearch(t *testing.T) {
	b, g := newGraphFixture(t, SynapseOptions{})
	ctx := context.Background()

	entityID := mkNode(t, b, NodeEntity, "XimoAgent", "XimoAgent")
	factA := mkNode(t, b, NodeFact, "深色主题", "项目偏好深色主题")
	factB := mkNode(t, b, NodeFact, "并发偏好", "项目使用 Go 引擎")
	if _, err := b.upsertEdge(ctx, b.db, synapseEdge{Src: factA, Dst: entityID, Rel: RelMentions,
		Weight: SynapseWeightMentions, CreatedAt: b.now()}); err != nil {
		t.Fatalf("写 mentions 边: %v", err)
	}
	if _, err := b.upsertEdge(ctx, b.db, synapseEdge{Src: factB, Dst: entityID, Rel: RelMentions,
		Weight: SynapseWeightMentions, CreatedAt: b.now()}); err != nil {
		t.Fatalf("写 mentions 边: %v", err)
	}

	// 邻域：以 entity 为中心 1 跳，应带出两条 fact，且度数按全图算。
	nb, err := g.Graph(ctx, types.MemoryGraphRequest{CenterOn: entityID, Depth: 1})
	if err != nil {
		t.Fatalf("Graph 邻域: %v", err)
	}
	if nb.Total != 3 || len(nb.Nodes) != 3 {
		t.Fatalf("邻域 = total %d / %d 节点，期望 3/3", nb.Total, len(nb.Nodes))
	}
	var entityNode types.MemoryGraphNode
	for _, n := range nb.Nodes {
		if n.ID == entityID {
			entityNode = n
		}
	}
	if entityNode.Degree != 2 {
		t.Fatalf("中心节点度数 = %d，期望 2", entityNode.Degree)
	}
	if len(nb.Edges) != 2 {
		t.Fatalf("邻域边数 = %d，期望 2", len(nb.Edges))
	}

	// 邻域不存在的中心：明确报错。
	if _, err := g.Graph(ctx, types.MemoryGraphRequest{CenterOn: "mn_missing"}); err == nil {
		t.Fatal("不存在的中心节点应当报错")
	}

	// 搜索：命中"深色"，并把它经 mentions 连到的 entity 一起带出来。
	hit, err := g.Graph(ctx, types.MemoryGraphRequest{Query: "深色"})
	if err != nil {
		t.Fatalf("Graph 搜索: %v", err)
	}
	if hit.Total < 1 {
		t.Fatalf("搜索 total = %d，期望至少 1", hit.Total)
	}
	found := false
	for _, n := range hit.Nodes {
		if n.ID == factA {
			found = true
		}
	}
	if !found {
		t.Fatalf("搜索结果里没有命中节点 %s：%+v", factA, graphNodeIDs(hit.Nodes))
	}

	// 无命中：返回空图而不是错误。
	empty, err := g.Graph(ctx, types.MemoryGraphRequest{Query: "zzzz-不存在的词-zzzz"})
	if err != nil {
		t.Fatalf("Graph 空搜索: %v", err)
	}
	if len(empty.Nodes) != 0 || empty.Total != 0 {
		t.Fatalf("空搜索应返回空图，得到 %d 节点 / total %d", len(empty.Nodes), empty.Total)
	}
}

// ---------------------------------------------------------------------------
// Node：完整正文 / 相邻边 / 邻居 / 召回记录
// ---------------------------------------------------------------------------

func TestSynapseGraphNodeDetail(t *testing.T) {
	b, g := newGraphFixture(t, SynapseOptions{})
	ctx := context.Background()

	entityID := mkNode(t, b, NodeEntity, "XimoAgent", "XimoAgent")
	factID := mkNode(t, b, NodeFact, "主题", "项目偏好深色主题，正文要完整返回")
	if _, err := b.upsertEdge(ctx, b.db, synapseEdge{Src: factID, Dst: entityID, Rel: RelMentions,
		Weight: SynapseWeightMentions, CreatedAt: b.now()}); err != nil {
		t.Fatalf("写 mentions 边: %v", err)
	}
	// 一条召回记录（used=1 表示被采用）。
	if _, err := b.db.ExecContext(ctx, `
INSERT INTO mem_recall_log (run_id,node_id,activation,via,used,ts) VALUES (?,?,?,?,?,?)`,
		"rc_test", factID, 0.9, "seed", recallLogAdopted, b.now().Unix()); err != nil {
		t.Fatalf("写召回记录: %v", err)
	}

	detail, err := g.Node(ctx, factID)
	if err != nil {
		t.Fatalf("Node: %v", err)
	}
	if detail.Content != "项目偏好深色主题，正文要完整返回" {
		t.Fatalf("完整正文 = %q", detail.Content)
	}
	if detail.Node.Degree != 1 {
		t.Fatalf("度数 = %d，期望 1", detail.Node.Degree)
	}
	if len(detail.Edges) != 1 || len(detail.Neighbors) != 1 || detail.Neighbors[0].ID != entityID {
		t.Fatalf("邻域 = %d 边 / %d 邻居，期望 1/1 且邻居是 %s",
			len(detail.Edges), len(detail.Neighbors), entityID)
	}
	if len(detail.Recalls) != 1 || !detail.Recalls[0].Used || detail.Recalls[0].Via != "seed" {
		t.Fatalf("召回记录 = %+v", detail.Recalls)
	}

	if _, err := g.Node(ctx, "mn_missing"); err == nil {
		t.Fatal("不存在的节点应当报错")
	}
}

// ---------------------------------------------------------------------------
// UpdateNode：FTS 必须跟着新正文走
// ---------------------------------------------------------------------------

func TestSynapseGraphUpdateNodeKeepsFTSInSync(t *testing.T) {
	b, g := newGraphFixture(t, SynapseOptions{})
	ctx := context.Background()

	id := mkNode(t, b, NodeFact, "主题", "项目使用深色主题")

	updated, err := g.UpdateNode(ctx, types.MemoryNodeUpdate{
		NodeID:  id,
		Content: strPtr("项目改用浅色主题"),
		Title:   strPtr("主题（改）"),
	})
	if err != nil {
		t.Fatalf("UpdateNode: %v", err)
	}
	if updated.Content != "项目改用浅色主题" || updated.Node.Title != "主题（改）" {
		t.Fatalf("更新后详情 = %+v", updated)
	}

	// FTS 命中新正文。
	recs, err := b.Search(ctx, "浅色主题", SearchOptions{})
	if err != nil {
		t.Fatalf("更新后搜索: %v", err)
	}
	found := false
	for _, r := range recs {
		if r.ID == id && strings.Contains(r.Memory, "浅色主题") {
			found = true
		}
	}
	if !found {
		t.Fatalf("更新后的正文搜不到，得到 %+v", recs)
	}
	// 旧正文里的词不应再命中这条节点（"主题"仍在标题里，所以用一个只出现在
	// 旧正文里的二元组来断言 FTS 真的被重建了）。
	old, err := b.Search(ctx, "深色", SearchOptions{})
	if err != nil {
		t.Fatalf("旧正文搜索: %v", err)
	}
	for _, r := range old {
		if r.ID == id {
			t.Fatalf("旧正文仍然命中已更新的节点: %+v", r)
		}
	}

	// 空正文被拒绝（要用删除而不是清空）。
	if _, err := g.UpdateNode(ctx, types.MemoryNodeUpdate{NodeID: id, Content: strPtr("   ")}); err == nil {
		t.Fatal("把正文改成空应当被拒绝")
	}
	// 非法状态被拒绝。
	if _, err := g.UpdateNode(ctx, types.MemoryNodeUpdate{NodeID: id, Status: strPtr("bogus")}); err == nil {
		t.Fatal("非法 status 应当被拒绝")
	}
	// 置顶与重要度。
	if _, err := g.UpdateNode(ctx, types.MemoryNodeUpdate{
		NodeID: id, Pinned: boolPtr(true), Importance: floatPtr(0.9),
	}); err != nil {
		t.Fatalf("置顶: %v", err)
	}
	after, err := g.Node(ctx, id)
	if err != nil {
		t.Fatalf("Node: %v", err)
	}
	if !after.Node.Pinned || after.Node.Importance != 0.9 {
		t.Fatalf("置顶/重要度未生效: %+v", after.Node)
	}
}

func boolPtr(v bool) *bool        { return &v }
func floatPtr(v float64) *float64 { return &v }

// ---------------------------------------------------------------------------
// DeleteNode：硬删除，边一起走，且不可再召回
// ---------------------------------------------------------------------------

func TestSynapseGraphDeleteNodeRemovesEdgesAndRecall(t *testing.T) {
	b, g := newGraphFixture(t, SynapseOptions{})
	ctx := context.Background()

	entityID := mkNode(t, b, NodeEntity, "XimoAgent", "XimoAgent")
	factID := mkNode(t, b, NodeFact, "主题", "项目使用深色主题")
	if _, err := b.upsertEdge(ctx, b.db, synapseEdge{Src: factID, Dst: entityID, Rel: RelMentions,
		Weight: SynapseWeightMentions, CreatedAt: b.now()}); err != nil {
		t.Fatalf("写 mentions 边: %v", err)
	}
	if _, err := b.db.ExecContext(ctx, `
INSERT INTO mem_recall_log (run_id,node_id,activation,via,used,ts) VALUES (?,?,?,?,0,?)`,
		"rc_del", factID, 0.5, "seed", b.now().Unix()); err != nil {
		t.Fatalf("写召回记录: %v", err)
	}

	if err := g.DeleteNode(ctx, factID); err != nil {
		t.Fatalf("DeleteNode: %v", err)
	}
	var nodes, edges, recalls int
	if err := b.db.QueryRow(`SELECT COUNT(*) FROM mem_nodes WHERE id=?`, factID).Scan(&nodes); err != nil {
		t.Fatalf("统计节点: %v", err)
	}
	if err := b.db.QueryRow(`SELECT COUNT(*) FROM mem_edges WHERE src=? OR dst=?`, factID, factID).Scan(&edges); err != nil {
		t.Fatalf("统计边: %v", err)
	}
	if err := b.db.QueryRow(`SELECT COUNT(*) FROM mem_recall_log WHERE node_id=?`, factID).Scan(&recalls); err != nil {
		t.Fatalf("统计召回记录: %v", err)
	}
	if nodes != 0 || edges != 0 || recalls != 0 {
		t.Fatalf("删除后残留 节点=%d 边=%d 召回=%d", nodes, edges, recalls)
	}
	if recs, err := b.Search(ctx, "深色主题", SearchOptions{}); err != nil {
		t.Fatalf("删除后搜索: %v", err)
	} else {
		for _, r := range recs {
			if r.ID == factID {
				t.Fatalf("已删除的节点仍被召回: %+v", r)
			}
		}
	}
	if err := g.DeleteNode(ctx, factID); err == nil {
		t.Fatal("重复删除同一个节点应当报错")
	}
}

// ---------------------------------------------------------------------------
// Link：建立 / 覆盖权重
// ---------------------------------------------------------------------------

func TestSynapseGraphLink(t *testing.T) {
	b, g := newGraphFixture(t, SynapseOptions{})
	ctx := context.Background()

	a := mkNode(t, b, NodeFact, "A", "事实 A")
	c := mkNode(t, b, NodeFact, "C", "事实 C")

	mut, err := g.Link(ctx, types.MemoryLink{Src: a, Dst: c})
	if err != nil {
		t.Fatalf("Link: %v", err)
	}
	if !mut.OK || mut.Affected != 1 {
		t.Fatalf("Link 结果 = %+v", mut)
	}
	w, ok := edgeWeight(t, b, a, c, RelRelated)
	if !ok || w != SynapseWeightRelated {
		t.Fatalf("默认权重 = %v（存在=%v），期望 %v", w, ok, SynapseWeightRelated)
	}

	// 已存在则覆盖权重（人工编辑优先于自动学习的取大策略）。
	if _, err := g.Link(ctx, types.MemoryLink{Src: a, Dst: c, Rel: RelRelated, Weight: 0.05}); err != nil {
		t.Fatalf("Link 更新: %v", err)
	}
	if w, _ := edgeWeight(t, b, a, c, RelRelated); w != 0.05 {
		t.Fatalf("更新后的权重 = %v，期望 0.05", w)
	}

	// 非法输入。
	if _, err := g.Link(ctx, types.MemoryLink{Src: a, Dst: a}); err == nil {
		t.Fatal("自环应当被拒绝")
	}
	if _, err := g.Link(ctx, types.MemoryLink{Src: a, Dst: c, Rel: "bogus"}); err == nil {
		t.Fatal("非法 rel 应当被拒绝")
	}
	if _, err := g.Link(ctx, types.MemoryLink{Src: a, Dst: "mn_missing"}); err == nil {
		t.Fatal("不存在的终点应当被拒绝")
	}
}

// ---------------------------------------------------------------------------
// Export / Import：幂等合并，不覆盖已有内容
// ---------------------------------------------------------------------------

func TestSynapseGraphExportImportIsIdempotent(t *testing.T) {
	ctx := context.Background()
	src, gsrc := newGraphFixture(t, SynapseOptions{})
	entityID := mkNode(t, src, NodeEntity, "XimoAgent", "XimoAgent")
	factID := mkNode(t, src, NodeFact, "主题", "项目偏好深色主题")
	if _, err := src.upsertEdge(ctx, src.db, synapseEdge{Src: factID, Dst: entityID, Rel: RelMentions,
		Weight: SynapseWeightMentions, CreatedAt: src.now()}); err != nil {
		t.Fatalf("写 mentions 边: %v", err)
	}

	doc, err := gsrc.Export(ctx)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if len(doc.Nodes) != 2 || len(doc.Edges) != 1 || doc.Version != GraphExportVersion {
		t.Fatalf("导出 = %d 节点 / %d 边 / v%d", len(doc.Nodes), len(doc.Edges), doc.Version)
	}
	if doc.Nodes[0].Content == "" {
		t.Fatal("导出节点必须带完整正文")
	}

	dst, gdst := newGraphFixture(t, SynapseOptions{})
	first, err := gdst.Import(ctx, doc)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if first.Affected != 2 {
		t.Fatalf("首次导入新建 %d 个节点，期望 2", first.Affected)
	}
	statsAfterFirst := gdst.Stats(ctx)

	second, err := gdst.Import(ctx, doc)
	if err != nil {
		t.Fatalf("二次 Import: %v", err)
	}
	if second.Affected != 0 {
		t.Fatalf("重复导入新建了 %d 个节点，期望 0（幂等）", second.Affected)
	}
	statsAfterSecond := gdst.Stats(ctx)
	if statsAfterSecond.Nodes != statsAfterFirst.Nodes || statsAfterSecond.Edges != statsAfterFirst.Edges {
		t.Fatalf("重复导入改变了计数: %d/%d → %d/%d",
			statsAfterFirst.Nodes, statsAfterFirst.Edges, statsAfterSecond.Nodes, statsAfterSecond.Edges)
	}

	// 不覆盖已有内容：本地改过正文后，再导入同一份文件不应把它改回去。
	var importedID string
	if err := dst.db.QueryRow(`SELECT id FROM mem_nodes WHERE user_id=? AND kind=?`, dst.opts.UserID, NodeFact).Scan(&importedID); err != nil {
		t.Fatalf("查导入节点: %v", err)
	}
	if _, err := gdst.UpdateNode(ctx, types.MemoryNodeUpdate{
		NodeID: importedID, Content: strPtr("本地已经改过的正文"),
	}); err != nil {
		t.Fatalf("本地更新: %v", err)
	}
	if _, err := gdst.Import(ctx, doc); err != nil {
		t.Fatalf("三次 Import: %v", err)
	}
	after, err := gdst.Node(ctx, importedID)
	if err != nil {
		t.Fatalf("Node: %v", err)
	}
	if after.Content != "本地已经改过的正文" {
		t.Fatalf("导入覆盖了已有内容: %q", after.Content)
	}

	// 空导入明确报错。
	if _, err := gdst.Import(ctx, types.MemoryExport{}); err == nil {
		t.Fatal("空导入应当报错")
	}
}

// ---------------------------------------------------------------------------
// Clear：Stats.Nodes == 0
// ---------------------------------------------------------------------------

func TestSynapseGraphClear(t *testing.T) {
	b, g := newGraphFixture(t, SynapseOptions{})
	ctx := context.Background()

	entityID := mkNode(t, b, NodeEntity, "XimoAgent", "XimoAgent")
	factID := mkNode(t, b, NodeFact, "主题", "项目偏好深色主题")
	if _, err := b.upsertEdge(ctx, b.db, synapseEdge{Src: factID, Dst: entityID, Rel: RelMentions,
		Weight: SynapseWeightMentions, CreatedAt: b.now()}); err != nil {
		t.Fatalf("写 mentions 边: %v", err)
	}
	if _, err := b.db.ExecContext(ctx, `
INSERT INTO mem_recall_log (run_id,node_id,activation,via,used,ts) VALUES (?,?,?,?,0,?)`,
		"rc_clear", factID, 0.5, "seed", b.now().Unix()); err != nil {
		t.Fatalf("写召回记录: %v", err)
	}
	before := g.Stats(ctx)
	if before.Nodes == 0 || before.Edges == 0 {
		t.Fatalf("清空前应有数据: %+v", before)
	}
	if before.Backend != BackendSynapse || !before.Enabled {
		t.Fatalf("Stats 必须标明后端: %+v", before)
	}

	mut, err := g.Clear(ctx)
	if err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if !mut.OK || mut.Affected != before.Nodes {
		t.Fatalf("Clear 结果 = %+v，期望 Affected=%d", mut, before.Nodes)
	}
	after := g.Stats(ctx)
	if after.Nodes != 0 || after.Edges != 0 {
		t.Fatalf("清空后仍有 节点=%d 边=%d", after.Nodes, after.Edges)
	}
	// 库文件保留（连接仍被持有），并且可以继续写入。
	if b.Path() == "" {
		t.Fatal("清空后库文件路径不应丢失")
	}
	if recs, err := b.Search(ctx, "深色主题", SearchOptions{}); err != nil {
		t.Fatalf("清空后搜索: %v", err)
	} else if len(recs) != 0 {
		t.Fatalf("清空后仍能召回 %d 条", len(recs))
	}
	newID := mkNode(t, b, NodeFact, "新事实", "清空之后写的新事实")
	if detail, err := g.Node(ctx, newID); err != nil || detail.Content == "" {
		t.Fatalf("清空后应能继续写入: %v", err)
	}
}

// ---------------------------------------------------------------------------
// EffectiveWeight：45 天前的 used_with 边衰减到约一半
// ---------------------------------------------------------------------------

func TestSynapseGraphReportsEffectiveWeight(t *testing.T) {
	clock := newGraphClock()
	b, g := newGraphFixture(t, SynapseOptions{Now: clock.now})
	ctx := context.Background()

	a := mkNode(t, b, NodeFact, "A", "事实 A")
	c := mkNode(t, b, NodeFact, "C", "事实 C")
	if _, err := b.upsertEdge(ctx, b.db, synapseEdge{
		Src: a, Dst: c, Rel: RelUsedWith, Weight: 0.6,
		LastFired: clock.now(), CreatedAt: clock.now(),
	}); err != nil {
		t.Fatalf("写 used_with 边: %v", err)
	}

	// 刚写下：有效权重 == 原始权重。
	now, err := g.Graph(ctx, types.MemoryGraphRequest{Kinds: []string{NodeFact}})
	if err != nil {
		t.Fatalf("Graph: %v", err)
	}
	e := findEdge(t, now.Edges, a, c)
	if e.Weight != 0.6 || e.EffectiveWeight != 0.6 {
		t.Fatalf("刚建立时 weight=%v effective=%v，期望 0.6/0.6", e.Weight, e.EffectiveWeight)
	}

	// 45 天（一个半衰期）后：有效权重 ≈ 0.3，而原始权重不变。
	clock.advance(45 * 24 * time.Hour)
	after, err := g.Graph(ctx, types.MemoryGraphRequest{Kinds: []string{NodeFact}})
	if err != nil {
		t.Fatalf("Graph（45 天后）: %v", err)
	}
	e = findEdge(t, after.Edges, a, c)
	if e.Weight != 0.6 {
		t.Fatalf("原始权重被改动了: %v", e.Weight)
	}
	if diff := e.EffectiveWeight - 0.3; diff > 0.02 || diff < -0.02 {
		t.Fatalf("45 天后的有效权重 = %v，期望约 0.3", e.EffectiveWeight)
	}

	// 置顶节点的边不衰减。
	if _, err := g.UpdateNode(ctx, types.MemoryNodeUpdate{NodeID: a, Pinned: boolPtr(true)}); err != nil {
		t.Fatalf("置顶: %v", err)
	}
	pinned, err := g.Graph(ctx, types.MemoryGraphRequest{Kinds: []string{NodeFact}})
	if err != nil {
		t.Fatalf("Graph（置顶后）: %v", err)
	}
	e = findEdge(t, pinned.Edges, a, c)
	if e.EffectiveWeight != 0.6 {
		t.Fatalf("置顶节点的边不应衰减，得到 %v", e.EffectiveWeight)
	}

	// 结构边（mentions）不衰减。
	d := mkNode(t, b, NodeFact, "D", "事实 D")
	if _, err := b.upsertEdge(ctx, b.db, synapseEdge{Src: d, Dst: a, Rel: RelMentions,
		Weight: SynapseWeightMentions, CreatedAt: clock.now().Add(-400 * 24 * time.Hour)}); err != nil {
		t.Fatalf("写 mentions 边: %v", err)
	}
	structural, err := g.Graph(ctx, types.MemoryGraphRequest{Kinds: []string{NodeFact}})
	if err != nil {
		t.Fatalf("Graph（结构边）: %v", err)
	}
	if e := findEdge(t, structural.Edges, d, a); e.EffectiveWeight != SynapseWeightMentions {
		t.Fatalf("mentions 边不应衰减，得到 %v", e.EffectiveWeight)
	}
}

// ---------------------------------------------------------------------------
// 基准：图查询必须有界（2 万节点）
// ---------------------------------------------------------------------------

// BenchmarkSynapseGraphPage 量的是记忆页默认打开时的那一次全量分页查询
// （limit=200，含 Total 计数、度数统计与边装配）。它是交互路径，必须远快于
// 召回预算，否则用户点开记忆页会先卡一下。
func BenchmarkSynapseGraphPage(b *testing.B) {
	be := newSynapseB(b, 20000)
	g := NewSynapseGraph(be)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := g.Graph(ctx, types.MemoryGraphRequest{Limit: DefaultGraphLimit})
		if err != nil {
			b.Fatal(err)
		}
		if len(res.Nodes) == 0 {
			b.Fatal("分页结果为空")
		}
	}
}

// BenchmarkSynapseGraphNeighborhood 量的是"点一个节点看它的邻域"（2 跳）。
func BenchmarkSynapseGraphNeighborhood(b *testing.B) {
	be := newSynapseB(b, 20000)
	g := NewSynapseGraph(be)
	ctx := context.Background()
	center := "mn_bench_entity_0"
	if _, err := g.Graph(ctx, types.MemoryGraphRequest{CenterOn: center, Depth: 1}); err != nil {
		b.Fatalf("预热: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := g.Graph(ctx, types.MemoryGraphRequest{CenterOn: center, Depth: 2})
		if err != nil {
			b.Fatal(err)
		}
		if res.Total == 0 {
			b.Fatal("邻域为空")
		}
	}
}

// ---------------------------------------------------------------------------
// Consolidate：图接口这一版（与 Backend 侧的 ConsolidateResult 同名不同形）
// ---------------------------------------------------------------------------

func TestSynapseGraphConsolidateArchivesStaleNode(t *testing.T) {
	clock := newGraphClock()
	b, g := newGraphFixture(t, SynapseOptions{Now: clock.now})
	ctx := context.Background()

	id := mkNode(t, b, NodeFact, "旧事实", "很久以前记下的一条事实")

	// 时间拨到 400 天后（默认归档门槛 180 天），手动整理应把它归档。
	clock.advance(400 * 24 * time.Hour)
	mut, err := g.Consolidate(ctx)
	if err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if !mut.OK || mut.Stats == nil {
		t.Fatalf("Consolidate 结果 = %+v", mut)
	}
	if len(mut.Notes) == 0 {
		t.Fatal("Consolidate 应报告做了什么")
	}
	if status := nodeStatus(t, b, id); status != StatusArchived {
		t.Fatalf("长期未用的节点应被归档，实际状态 %q", status)
	}
	if g.Stats(ctx).Consolidations == 0 {
		t.Fatal("整理次数应计入 Stats")
	}

	// 归档节点默认不出现在图里，IncludeArchived 时出现。
	def, err := g.Graph(ctx, types.MemoryGraphRequest{})
	if err != nil {
		t.Fatalf("Graph: %v", err)
	}
	if def.Total != 0 {
		t.Fatalf("归档节点默认不应出现，Total=%d", def.Total)
	}
	withArchived, err := g.Graph(ctx, types.MemoryGraphRequest{IncludeArchived: true})
	if err != nil {
		t.Fatalf("Graph IncludeArchived: %v", err)
	}
	if withArchived.Total != 1 {
		t.Fatalf("IncludeArchived 应看到归档节点，Total=%d", withArchived.Total)
	}
}

func findEdge(t *testing.T, edges []types.MemoryGraphEdge, src, dst string) types.MemoryGraphEdge {
	t.Helper()
	for _, e := range edges {
		if e.Src == src && e.Dst == dst {
			return e
		}
	}
	t.Fatalf("图里没有 %s → %s 这条边（共 %d 条）", src, dst, len(edges))
	return types.MemoryGraphEdge{}
}

// ---------------------------------------------------------------------------
// 迁移重跑：幂等，且不得伪造 use_count
// ---------------------------------------------------------------------------

func TestSynapseMigrationRerunDoesNotInflateUseCount(t *testing.T) {
	ctx := context.Background()
	old := newEmbedded(t, DefaultUserID)
	if _, err := old.Add(ctx, []Message{{Role: "user", Content: "项目 XimoAgent 使用 Go 引擎"}}, AddOptions{RunID: "old-1"}); err != nil {
		t.Fatalf("旧库写入: %v", err)
	}
	b := newSynapse(t, SynapseOptions{})
	for i := 0; i < 3; i++ {
		if _, err := b.MigrateFromLegacy(ctx, old); err != nil {
			t.Fatalf("第 %d 次迁移: %v", i+1, err)
		}
	}
	var maxUse int
	if err := b.db.QueryRow(`SELECT COALESCE(MAX(use_count),0) FROM mem_nodes WHERE user_id=?`, b.opts.UserID).Scan(&maxUse); err != nil {
		t.Fatalf("读 use_count: %v", err)
	}
	if maxUse != 0 {
		t.Fatalf("迁移重跑把 use_count 顶到了 %d，期望 0（迁移不是「被使用」）", maxUse)
	}
	if n := countNodesByKind(t, b, NodeFact); n != 1 {
		t.Fatalf("迁移重跑产生了 %d 条 fact，期望 1", n)
	}
}
