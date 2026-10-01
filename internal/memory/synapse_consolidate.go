package memory

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// 本文件是睡眠整理（文档 4.7）：去重合并、主题聚合、矛盾裁决、归档、统计。
//
// 触发权交给调用方（不自动起后台协程）：应用空闲 ≥10 分钟且距上次整理 ≥24h，
// 或用户手动点"整理记忆"。每次限时（20s）、可中断、分批，写的是同一张
// SQLite（单写者 + busy_timeout=5000 保持），因此不会与 run 争抢写锁太久。

// ConsolidateResult 是一次整理的统计（文档 4.7 第 5 条要求写回 Stats）。
type ConsolidateResult struct {
	MergedFacts            int
	TopicsCreated          int
	ContradictionsResolved int
	ArchivedNodes          int
	PrunedEdges            int
	RecallLogPruned        int
	Duration               time.Duration
	// Interrupted 为真表示到达时限就停下了（已做的部分保留）。
	Interrupted bool
}

// Consolidate 执行一次整理。ctx 的 deadline 若比配置的 20s 更早则以其为准；
// 到时限就返回已完成的统计，不返回错误（可中断是设计要求）。
func (b *SynapseBackend) Consolidate(ctx context.Context) (ConsolidateResult, error) {
	var res ConsolidateResult
	if b == nil || b.db == nil {
		return res, ErrDisabled
	}
	start := b.now()
	deadline := start.Add(b.opts.ConsolidateTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	cctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	b.consolidates.Add(1)

	steps := []func(context.Context, *ConsolidateResult) error{
		b.mergeDuplicates,
		b.aggregateTopics,
		b.resolveContradictions,
		b.archiveStale,
		b.pruneDeadEdges,
		b.pruneRecallLog,
	}
	for _, step := range steps {
		if err := step(cctx, &res); err != nil {
			return res, err
		}
		if cctx.Err() != nil {
			res.Interrupted = true
			break
		}
	}
	res.Duration = b.now().Sub(start)
	b.mergedFacts.Add(uint64(res.MergedFacts))
	b.topicsCreated.Add(uint64(res.TopicsCreated))
	b.archivedNodes.Add(uint64(res.ArchivedNodes))
	return res, nil
}

// ---------------------------------------------------------------------------
// 1. 去重合并
// ---------------------------------------------------------------------------

// mergeDuplicates 合并"同实体下高相似（无嵌入时词法 Jaccard ≥ 0.8）"的 fact
// （文档 4.7 第 1 条）。保留内容更长（同长取更新）的那条，另一条置为
// archived 并建 supersedes 边——不删除，用户可恢复。
func (b *SynapseBackend) mergeDuplicates(ctx context.Context, res *ConsolidateResult) error {
	type factEntry struct {
		node synapseNode
		toks []string
	}
	facts, err := b.activeNodes(ctx, NodeFact, DefaultSynapseConsolidateBatch)
	if err != nil {
		return err
	}
	if len(facts) < 2 {
		return nil
	}
	byEntity, err := b.entityIndex(ctx, nodeIDs(facts))
	if err != nil {
		return err
	}
	entries := make(map[string]factEntry, len(facts))
	for _, n := range facts {
		entries[n.ID] = factEntry{node: n, toks: tokenize(n.Title + " " + n.Content)}
	}

	merged := make(map[string]bool)
	// 只比较"共享至少一个实体"的事实对：不同实体的事实即使措辞相似也不该合并
	// （那通常是两件不同的事）。
	buckets := map[string][]string{}
	for id, ents := range byEntity {
		for _, e := range ents {
			buckets[e] = append(buckets[e], id)
		}
	}
	for _, ids := range buckets {
		sortStrings(ids)
		for i := 0; i < len(ids); i++ {
			for j := i + 1; j < len(ids); j++ {
				if ctx.Err() != nil {
					return nil
				}
				a, aok := entries[ids[i]]
				c, cok := entries[ids[j]]
				if !aok || !cok || merged[a.node.ID] || merged[c.node.ID] {
					continue
				}
				if tokenJaccard(a.toks, c.toks) < SynapseSimilarJaccard {
					continue
				}
				keep, drop := a.node, c.node
				if len([]rune(drop.Content)) > len([]rune(keep.Content)) ||
					(len([]rune(drop.Content)) == len([]rune(keep.Content)) && drop.CreatedAt.After(keep.CreatedAt)) {
					keep, drop = drop, keep
				}
				if err := b.mergeNodeInto(ctx, keep, drop); err != nil {
					return err
				}
				merged[drop.ID] = true
				res.MergedFacts++
			}
		}
	}
	return nil
}

// mergeNodeInto 把 drop 合并进 keep：drop 置 archived，建 keep—supersedes→drop。
func (b *SynapseBackend) mergeNodeInto(ctx context.Context, keep, drop synapseNode) error {
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `UPDATE mem_nodes SET status=? WHERE id=? AND user_id=?`,
		StatusArchived, drop.ID, b.opts.UserID); err != nil {
		return err
	}
	if _, err := b.upsertEdge(ctx, tx, synapseEdge{Src: keep.ID, Dst: drop.ID, Rel: RelSupersedes,
		Weight: 0.9, CreatedAt: b.now()}); err != nil {
		return err
	}
	return tx.Commit()
}

// ---------------------------------------------------------------------------
// 2. 主题聚合
// ---------------------------------------------------------------------------

// aggregateTopics 对"还没有 topic 的 fact/episode"用共同实体做连通分量（简单的
// 标签传播 / 并查集），簇 ≥4 时生成一个 topic 节点，成员以 part_of 连接
// （文档 4.7 第 2 条）。
func (b *SynapseBackend) aggregateTopics(ctx context.Context, res *ConsolidateResult) error {
	nodes, err := b.activeNonTopicNodes(ctx, DefaultSynapseConsolidateBatch)
	if err != nil {
		return err
	}
	if len(nodes) < SynapseTopicClusterMin {
		return nil
	}
	byEntity, err := b.entityIndex(ctx, nodeIDs(nodes))
	if err != nil {
		return err
	}
	// 已经有 topic 的节点跳过（"对无 topic 的 fact/episode"）。
	withTopic, err := b.nodesWithTopic(ctx, nodeIDs(nodes))
	if err != nil {
		return err
	}
	entityNames := map[string]string{}
	if titles, err := b.entityTitles(ctx); err == nil {
		entityNames = titles
	}

	dsu := newUnionFind()
	for id := range byEntity {
		dsu.find(id)
	}
	// 共同实体 → 同一个连通分量。注意并的是"节点"，不是实体 id：实体只是
	// 把事实连起来的枢纽，簇的成员必须是 fact/episode 本身。
	byEntityToNodes := map[string][]string{}
	for nodeID, ents := range byEntity {
		for _, e := range ents {
			byEntityToNodes[e] = append(byEntityToNodes[e], nodeID)
		}
	}
	for _, members := range byEntityToNodes {
		for i := 1; i < len(members); i++ {
			dsu.union(members[0], members[i])
		}
	}
	clusters := map[string][]string{}
	for id := range byEntity {
		if withTopic[id] {
			continue
		}
		root := dsu.find(id)
		clusters[root] = append(clusters[root], id)
	}
	byID := make(map[string]synapseNode, len(nodes))
	for _, n := range nodes {
		byID[n.ID] = n
	}
	for _, members := range clusters {
		if ctx.Err() != nil {
			return nil
		}
		if len(members) < SynapseTopicClusterMin {
			continue
		}
		sortStrings(members)
		if err := b.createTopicForCluster(ctx, members, byID, byEntity, entityNames); err != nil {
			return err
		}
		res.TopicsCreated++
	}
	return nil
}

// createTopicForCluster 生成 topic 节点与成员 part_of 边。有 Extractor 时请它
// 起标题 / 摘要，失败或未注入时用规则标题（前两个实体名），绝不因模型失败而
// 放弃聚类结果。
func (b *SynapseBackend) createTopicForCluster(ctx context.Context, members []string, byID map[string]synapseNode,
	byEntity map[string][]string, entityNames map[string]string) error {
	titles := make([]string, 0, 4)
	var contents []string
	for _, id := range members {
		if n, ok := byID[id]; ok {
			titles = append(titles, nodeDisplayName(n))
			contents = append(contents, strings.Join(strings.Fields(n.Content), " "))
		}
	}
	if len(titles) == 0 {
		return nil
	}

	title, summary := "", ""
	if b.opts.Extractor != nil {
		prompt := "下面是同一主题下的若干条记忆。只输出 JSON：{\"episode\":{\"title\":\"主题名\",\"summary\":\"一段摘要\"}}\n" +
			strings.Join(titles, "\n")
		ectx, cancel := context.WithTimeout(ctx, b.opts.ExtractTimeout)
		if ex, err := b.opts.Extractor.Extract(ectx, prompt); err == nil && ex.Episode != nil {
			title = strings.TrimSpace(types.RedactString(ex.Episode.Title))
			summary = strings.TrimSpace(types.RedactString(ex.Episode.Summary))
		} else if err != nil {
			b.noteError(fmt.Errorf("memory: synapse 主题抽取失败，回退规则标题: %w", err))
		}
		cancel()
	}
	if title == "" {
		var names []string
		for _, id := range members {
			for _, e := range byEntity[id] {
				if t := entityNames[e]; t != "" {
					names = append(names, t)
				}
			}
		}
		names = dedupeStrings(names)
		if len(names) > 2 {
			names = names[:2]
		}
		if len(names) == 0 {
			title = "主题：" + truncateRunes(titles[0], 20)
		} else {
			title = "主题：" + strings.Join(names, "、")
		}
	}
	if summary == "" {
		summary = truncateRunes(strings.Join(contents, "；"), 200)
	}

	now := b.now()
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	topicID, _, err := b.insertNode(ctx, tx, synapseNode{
		ID: newSynapseID("mn_"), Kind: NodeTopic, Title: title, Content: summary,
		Importance: 0.6, CreatedAt: now,
	})
	if err != nil {
		return err
	}
	for _, id := range members {
		if _, err := b.upsertEdge(ctx, tx, synapseEdge{Src: id, Dst: topicID, Rel: RelPartOf,
			Weight: SynapseWeightRelated, CreatedAt: now}); err != nil {
			return err
		}
		// 主题成员之间也建 same_topic，便于召回时一次带出整簇。
		if _, err := b.upsertEdge(ctx, tx, synapseEdge{Src: topicID, Dst: id, Rel: RelSameTopic,
			Weight: SynapseWeightRelated, CreatedAt: now}); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ---------------------------------------------------------------------------
// 3. 矛盾裁决
// ---------------------------------------------------------------------------

// resolveContradictions 对 contradicts 对取 created_at 新者 supersedes 旧者；
// 用户置顶者优先（文档 4.7 第 3 条）。
func (b *SynapseBackend) resolveContradictions(ctx context.Context, res *ConsolidateResult) error {
	rows, err := b.db.QueryContext(ctx, `
SELECT e.src, e.dst FROM mem_edges e
JOIN mem_nodes n ON n.id = e.src
WHERE e.rel=? AND n.user_id=?`, RelContradicts, b.opts.UserID)
	if err != nil {
		return err
	}
	var pairs [][2]string
	for rows.Next() {
		var src, dst string
		if err := rows.Scan(&src, &dst); err != nil {
			rows.Close()
			return err
		}
		pairs = append(pairs, [2]string{src, dst})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	now := b.now()
	for _, p := range pairs {
		if ctx.Err() != nil {
			return nil
		}
		a, aok, err := b.getNode(ctx, b.db, p[0])
		if err != nil {
			return err
		}
		c, cok, err := b.getNode(ctx, b.db, p[1])
		if err != nil {
			return err
		}
		if !aok || !cok {
			continue
		}
		winner, loser := a, c
		switch {
		case a.Pinned && !c.Pinned:
			winner, loser = a, c
		case c.Pinned && !a.Pinned:
			winner, loser = c, a
		case c.CreatedAt.After(a.CreatedAt):
			winner, loser = c, a
		}
		tx, err := b.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE mem_nodes SET status=? WHERE id=? AND user_id=?`,
			StatusSuperseded, loser.ID, b.opts.UserID); err != nil {
			tx.Rollback()
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM mem_edges WHERE rel=? AND ((src=? AND dst=?) OR (src=? AND dst=?))`,
			RelContradicts, a.ID, c.ID, c.ID, a.ID); err != nil {
			tx.Rollback()
			return err
		}
		if _, err := b.upsertEdge(ctx, tx, synapseEdge{Src: winner.ID, Dst: loser.ID, Rel: RelSupersedes,
			Weight: 0.8, CreatedAt: now}); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		res.ContradictionsResolved++
	}
	return nil
}

// ---------------------------------------------------------------------------
// 4. 归档
// ---------------------------------------------------------------------------

// archiveStale 把"长期无用（非置顶、use_count==0、超过 ArchiveAfter）"的节点
// 置为 archived，不删除（文档 4.7 第 4 条）。
func (b *SynapseBackend) archiveStale(ctx context.Context, res *ConsolidateResult) error {
	cutoff := b.now().Add(-b.opts.ArchiveAfter).Unix()
	res2, err := b.db.ExecContext(ctx, `
UPDATE mem_nodes SET status=?
WHERE user_id=? AND status=? AND pinned=0 AND use_count=0
  AND COALESCE(last_used, created_at) < ?`,
		StatusArchived, b.opts.UserID, StatusActive, cutoff)
	if err != nil {
		return err
	}
	if n, err := res2.RowsAffected(); err == nil {
		res.ArchivedNodes += int(n)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 5. 清理
// ---------------------------------------------------------------------------

// pruneDeadEdges 删除"有效权重低于 0.03 且从未 fire"的边（文档 4.6 末段）。
func (b *SynapseBackend) pruneDeadEdges(ctx context.Context, res *ConsolidateResult) error {
	now := b.now()
	rows, err := b.db.QueryContext(ctx, `
SELECT e.src,e.dst,e.rel,e.weight,COALESCE(e.last_fired,0),e.created_at
FROM mem_edges e JOIN mem_nodes n ON n.id = e.src
WHERE n.user_id=? AND e.fire_count=0 AND e.weight < ?`, b.opts.UserID, SynapseMinEdgeWeight*4)
	if err != nil {
		return err
	}
	type dead struct{ src, dst, rel string }
	var deads []dead
	for rows.Next() {
		var e synapseEdge
		var lastFired, created int64
		var fire int
		if err := rows.Scan(&e.Src, &e.Dst, &e.Rel, &e.Weight, &lastFired, &created); err != nil {
			rows.Close()
			return err
		}
		e.FireCount = fire
		if lastFired > 0 {
			e.LastFired = time.Unix(lastFired, 0).UTC()
		} else {
			e.CreatedAt = time.Unix(created, 0).UTC()
		}
		if edgeEffectiveWeight(e, false, now) < SynapseMinEdgeWeight {
			deads = append(deads, dead{src: e.Src, dst: e.Dst, rel: e.Rel})
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, d := range deads {
		if ctx.Err() != nil {
			return nil
		}
		if _, err := b.db.ExecContext(ctx, `DELETE FROM mem_edges WHERE src=? AND dst=? AND rel=?`,
			d.src, d.dst, d.rel); err != nil {
			return err
		}
		res.PrunedEdges++
	}
	return nil
}

// pruneRecallLog 清理超过保留期的召回日志（文档 4.3：保留最近 N 天）。
func (b *SynapseBackend) pruneRecallLog(ctx context.Context, res *ConsolidateResult) error {
	cutoff := b.now().Add(-b.opts.RecallLogRetention).Unix()
	r, err := b.db.ExecContext(ctx, `DELETE FROM mem_recall_log WHERE ts < ?`, cutoff)
	if err != nil {
		return err
	}
	if n, err := r.RowsAffected(); err == nil {
		res.RecallLogPruned = int(n)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 整理用的小查询与并查集
// ---------------------------------------------------------------------------

// activeNodes 取一批活跃节点。
func (b *SynapseBackend) activeNodes(ctx context.Context, kind string, limit int) ([]synapseNode, error) {
	if limit <= 0 {
		limit = DefaultSynapseConsolidateBatch
	}
	rows, err := b.db.QueryContext(ctx, `SELECT `+synapseNodeCols+`
FROM mem_nodes WHERE user_id=? AND status=? AND kind=? ORDER BY created_at LIMIT ?`,
		b.opts.UserID, StatusActive, kind, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]synapseNode, 0, limit)
	for rows.Next() {
		n, err := scanSynapseNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// activeNonTopicNodes 取一批活跃的 fact/episode（主题聚合的输入）。
func (b *SynapseBackend) activeNonTopicNodes(ctx context.Context, limit int) ([]synapseNode, error) {
	if limit <= 0 {
		limit = DefaultSynapseConsolidateBatch
	}
	rows, err := b.db.QueryContext(ctx, `SELECT `+synapseNodeCols+`
FROM mem_nodes WHERE user_id=? AND status=? AND kind IN (?,?) ORDER BY created_at LIMIT ?`,
		b.opts.UserID, StatusActive, NodeFact, NodeEpisode, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]synapseNode, 0, limit)
	for rows.Next() {
		n, err := scanSynapseNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// entityIndex 返回 nodeID → entity 节点 id 列表（mentions 边）。
func (b *SynapseBackend) entityIndex(ctx context.Context, ids []string) (map[string][]string, error) {
	out := map[string][]string{}
	if len(ids) == 0 {
		return out, nil
	}
	placeholders := strings.Repeat("?,", len(ids))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, 0, len(ids)+3)
	args = append(args, RelMentions, b.opts.UserID, NodeEntity)
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := b.db.QueryContext(ctx, `
SELECT e.src, e.dst FROM mem_edges e
JOIN mem_nodes n ON n.id = e.dst
WHERE e.rel=? AND n.user_id=? AND n.kind=? AND e.src IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var src, dst string
		if err := rows.Scan(&src, &dst); err != nil {
			return nil, err
		}
		out[src] = append(out[src], dst)
	}
	return out, rows.Err()
}

// nodesWithTopic 返回已有 part_of 边指向 topic 的节点集合。
func (b *SynapseBackend) nodesWithTopic(ctx context.Context, ids []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	placeholders := strings.Repeat("?,", len(ids))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, 0, len(ids)+1)
	args = append(args, RelPartOf)
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := b.db.QueryContext(ctx, `
SELECT DISTINCT src FROM mem_edges WHERE rel=? AND src IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var src string
		if err := rows.Scan(&src); err != nil {
			return nil, err
		}
		out[src] = true
	}
	return out, rows.Err()
}

// entityTitles 返回 entityID → 展示名。
func (b *SynapseBackend) entityTitles(ctx context.Context) (map[string]string, error) {
	rows, err := b.db.QueryContext(ctx, `SELECT id, title FROM mem_nodes WHERE user_id=? AND kind=?`, b.opts.UserID, NodeEntity)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, title string
		if err := rows.Scan(&id, &title); err != nil {
			return nil, err
		}
		out[id] = title
	}
	return out, rows.Err()
}

func nodeIDs(nodes []synapseNode) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.ID)
	}
	return out
}

// unionFind 是主题聚合用的并查集（标签传播的最简形式）。
type unionFind struct{ parent map[string]string }

func newUnionFind() *unionFind { return &unionFind{parent: map[string]string{}} }

func (u *unionFind) find(x string) string {
	p, ok := u.parent[x]
	if !ok {
		u.parent[x] = x
		return x
	}
	if p == x {
		return x
	}
	root := u.find(p)
	u.parent[x] = root
	return root
}

func (u *unionFind) union(a, b string) {
	ra, rb := u.find(a), u.find(b)
	if ra == rb {
		return
	}
	// 固定按字典序挂靠，保证结果确定（测试可复现）。
	if ra < rb {
		u.parent[rb] = ra
	} else {
		u.parent[ra] = rb
	}
}
