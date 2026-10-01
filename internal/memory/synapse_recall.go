package memory

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// 本文件是召回（文档 4.5）：查询构造 → 种子选取（FTS5 bm25 + entity 最长匹配
// + 可选嵌入）→ 融合 → 2 跳扩散激活 → 打分 → 预算裁剪 → 格式化注入文本 →
// 异步写 mem_recall_log。
//
// 上层（Service.Recall → Block）拿到的是 []Record；为了让"为什么想起它"可见，
// 每条 Record 的 Metadata 都带 via（路径描述，种子是 "seed"）、kind、importance、
// entities 与 recall_batch。SynapseBackend.RenderBlock 按文档 4.5 第 6 条的格式
// 渲染完整注入块（含 ↳ 关联 行与结尾"可能已过时"句），供后续装配阶段使用。

// RecallBlockHeader 是 Synapse 注入块的首行（文档 4.5 第 6 条）。
const RecallBlockHeader = "[长期记忆 · 与当前任务相关]"

// RecallBlockFooter 是注入块的结尾句。文档要求必须保留：防止旧记忆压过用户
// 当下的明确指令。
const RecallBlockFooter = "（以上是过往交互中沉淀的信息，可能已过时；与用户本次明确要求冲突时，以本次为准。）"

// recallCandidate 是打分阶段的一个候选。
type recallCandidate struct {
	node       synapseNode
	activation float64
	via        string
	reason     string
	score      float64
}

// Search 按扩散激活召回相关记忆。
func (b *SynapseBackend) Search(ctx context.Context, query string, opts SearchOptions) ([]Record, error) {
	if b == nil || b.db == nil {
		return nil, ErrDisabled
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return []Record{}, nil
	}
	b.recallCalls.Add(1)
	topK := opts.TopK
	if topK <= 0 {
		topK = b.opts.TopK
	}
	now := b.now()
	tokens := tokenize(query)

	// ---- 1. 词法种子：FTS5 bm25 前 SeedLimit ----
	hits, err := b.ftsSearch(ctx, b.db, tokens, b.opts.SeedLimit)
	if err != nil {
		b.recallErrors.Add(1)
		b.noteError(err)
		return nil, fmt.Errorf("memory: synapse 召回失败: %w", err)
	}

	activation := make(map[string]float64, len(hits)+8)
	parent := make(map[string]string, len(hits)+8)
	seeds := make(map[string]bool, len(hits)+8)

	maxRank := 0.0
	for _, h := range hits {
		if -h.Rank > maxRank {
			maxRank = -h.Rank
		}
	}

	// ---- 2. 可选嵌入：对 query 取向量，与候选节点余弦 ----
	cosines := map[string]float64{}
	if b.opts.Embedder != nil && len(hits) > 0 {
		ids := make([]string, 0, len(hits))
		for _, h := range hits {
			ids = append(ids, h.ID)
		}
		if nodes, err := b.getNodes(ctx, b.db, ids); err == nil {
			if qv, err := b.embedQuery(ctx, query); err == nil && len(qv) > 0 {
				for id, n := range nodes {
					if c := cosineSimilarity(qv, n.Embedding); c > 0 {
						cosines[id] = c
					}
				}
			}
		}
	}

	// ---- 3. 融合：0.5·norm(bm25) + 0.4·cos + 0.1·pinnedBonus；无嵌入时归一 ----
	hitNodes, _ := b.getNodes(ctx, b.db, keysOfHits(hits))
	pinnedBonus := func(id string) float64 {
		if n, ok := hitNodes[id]; ok && n.Pinned {
			return 1
		}
		return 0
	}
	for _, h := range hits {
		norm := 0.0
		if maxRank > 0 {
			norm = (-h.Rank) / maxRank
		}
		var fused float64
		if b.opts.Embedder != nil {
			fused = 0.5*norm + 0.4*cosines[h.ID] + 0.1*pinnedBonus(h.ID)
		} else {
			// 无嵌入：权重重新归一（0.5 / 0.1 → 除以 0.6）。
			fused = (0.5*norm + 0.1*pinnedBonus(h.ID)) / 0.6
		}
		activation[h.ID] = clamp01(fused)
		seeds[h.ID] = true
	}

	// ---- 4. entity 命中：query 中出现已知 entity 名（最长匹配）→ 种子 0.8 ----
	if ents, err := b.entitySeeds(ctx, b.db, query); err == nil {
		for _, e := range ents {
			if activation[e.ID] < SynapseSeedActivation {
				activation[e.ID] = SynapseSeedActivation
			}
			seeds[e.ID] = true
		}
	}

	// ---- 5. 2 跳扩散激活（文档 4.5 第 3 条） ----
	frontier := make(map[string]float64, len(activation))
	all := make(map[string]float64, len(activation))
	for id, a := range activation {
		frontier[id] = a
		all[id] = a
	}
	for hop := 1; hop <= b.opts.Hops; hop++ {
		frontier = trimFrontier(frontier, b.opts.MaxFrontier)
		next := make(map[string]float64)
		nextParent := make(map[string]string)
		for id, act := range frontier {
			if act < b.opts.SpreadThreshold {
				continue
			}
			edges, err := b.edgesFor(ctx, id, b.opts.FanOut, now)
			if err != nil {
				b.recallErrors.Add(1)
				b.noteError(err)
				return nil, fmt.Errorf("memory: synapse 扩散激活失败: %w", err)
			}
			for _, e := range edges {
				// supersedes / contradicts 不传播激活（文档明确要求）。
				if e.Rel == RelSupersedes || e.Rel == RelContradicts {
					continue
				}
				other := e.other(id)
				gain := act * e.Weight * hopDecay(hop)
				if gain <= 0 {
					continue
				}
				if cur, ok := next[other]; !ok || gain > cur {
					next[other] = gain
					nextParent[other] = id
				}
			}
		}
		for id, v := range next {
			// merge：取较大者。用 max 而不是 sum，是为了让激活有界（种子最大
			// 0.8/1.0），不会被一个大度数枢纽节点反复累加顶到天上。
			if cur, ok := all[id]; !ok || v > cur {
				all[id] = v
				if !seeds[id] {
					parent[id] = nextParent[id]
				}
			}
		}
		frontier = next
	}

	// ---- 6. 打分排序 ----
	ids := make([]string, 0, len(all))
	for id := range all {
		ids = append(ids, id)
	}
	nodes, err := b.getNodes(ctx, b.db, ids)
	if err != nil {
		b.recallErrors.Add(1)
		b.noteError(err)
		return nil, fmt.Errorf("memory: synapse 读取候选失败: %w", err)
	}
	cands := make([]recallCandidate, 0, len(nodes))
	for id, n := range nodes {
		// superseded / archived 不参与召回；用户隔离由 getNodes 保证。
		if n.Status != StatusActive {
			continue
		}
		act := all[id]
		sc := scoreNode(act, n, now)
		if sc <= 0 {
			continue
		}
		cands = append(cands, recallCandidate{
			node:       n,
			activation: act,
			via:        b.viaPath(id, parent, nodes),
			reason:     b.viaReason(id, parent, nodes),
			score:      sc,
		})
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].score != cands[j].score {
			return cands[i].score > cands[j].score
		}
		return cands[i].node.CreatedAt.After(cands[j].node.CreatedAt)
	})
	if len(cands) > topK {
		cands = cands[:topK]
	}

	entityTitles := b.entitiesOf(ctx, candidateIDs(cands))

	out := make([]Record, 0, len(cands))
	batchID := "rc_" + newSynapseID("")
	for _, c := range cands {
		meta := map[string]any{
			"kind":         c.node.Kind,
			"importance":   c.node.Importance,
			"via":          c.via,
			"pinned":       c.node.Pinned,
			"activation":   c.activation,
			"recall_batch": batchID,
			"source_run":   c.node.SourceRun,
			"associations": associationLine(c, entityTitles[c.node.ID]),
		}
		if title := strings.TrimSpace(c.node.Title); title != "" {
			meta["title"] = title
		}
		if names := entityTitles[c.node.ID]; len(names) > 0 {
			meta["entities"] = names
			meta["group"] = names[0]
		}
		out = append(out, Record{
			ID:        c.node.ID,
			Memory:    c.node.Content,
			UserID:    b.opts.UserID,
			AgentID:   b.opts.AgentID,
			RunID:     c.node.SourceRun,
			Hash:      c.node.Hash,
			Score:     c.score,
			Metadata:  meta,
			CreatedAt: c.node.CreatedAt.Format(time.RFC3339),
			UpdatedAt: c.node.LastUsed.Format(time.RFC3339),
		})
	}

	// ---- 7. 记录 mem_recall_log（异步，绝不阻塞召回） ----
	b.enqueue(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		b.writeRecallLog(ctx, batchID, cands, now)
	})

	b.recallChars.Add(uint64(len(out)))
	return out, nil
}

// writeRecallLog 把本次召回写进 mem_recall_log（used 初始为 0，等 run 结束由
// 写入流水线的赫布步骤回填）。
func (b *SynapseBackend) writeRecallLog(ctx context.Context, batchID string, cands []recallCandidate, now time.Time) {
	if len(cands) == 0 {
		return
	}
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		b.noteError(err)
		return
	}
	defer func() { _ = tx.Rollback() }()
	for _, c := range cands {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO mem_recall_log (run_id,node_id,activation,via,used,ts) VALUES (?,?,?,?,0,?)
ON CONFLICT(run_id,node_id) DO UPDATE SET activation=excluded.activation, via=excluded.via, ts=excluded.ts`,
			batchID, c.node.ID, c.activation, c.via, now.Unix()); err != nil {
			b.noteError(err)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		b.noteError(err)
	}
}

// trimFrontier 按激活降序保留前 n 个前沿节点（n<=0 表示不限）。见
// DefaultSynapseMaxFrontier 的说明：这是把"每跳 O(前沿×一次边查询)"压进召回
// p95 预算的工程界。
func trimFrontier(frontier map[string]float64, n int) map[string]float64 {
	if n <= 0 || len(frontier) <= n {
		return frontier
	}
	type kv struct {
		id  string
		act float64
	}
	items := make([]kv, 0, len(frontier))
	for id, act := range frontier {
		items = append(items, kv{id: id, act: act})
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].act > items[j].act })
	out := make(map[string]float64, n)
	for _, it := range items[:n] {
		out[it.id] = it.act
	}
	return out
}

// hopDecay 返回第 hop 跳的扩散衰减（文档：hopDecay = {1: 0.6, 2: 0.35}）。
func hopDecay(hop int) float64 {
	switch hop {
	case 1:
		return SynapseHopDecay1
	case 2:
		return SynapseHopDecay2
	default:
		return 0
	}
}

// viaPath 生成 "为什么想起它" 的路径描述（文档 4.5 第 6 条的 ↳ 关联 行来源）。
// 种子为 "seed"；扩散带出的节点给出 "entity:Go → fact:并发偏好" 这样的链。
func (b *SynapseBackend) viaPath(id string, parent map[string]string, nodes map[string]synapseNode) string {
	chain := make([]string, 0, 4)
	cur := id
	seen := map[string]bool{}
	for cur != "" && !seen[cur] {
		seen[cur] = true
		if n, ok := nodes[cur]; ok {
			chain = append(chain, n.label())
		} else {
			chain = append(chain, cur)
		}
		cur = parent[cur]
	}
	// chain = [目标, …, 第一跳, 种子]；文档示例从第一跳开始（不含种子）。
	if len(chain) <= 1 {
		return "seed"
	}
	chain = chain[:len(chain)-1]
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return strings.Join(chain, " → ")
}

// viaReason 返回关联行的原因短句里的主语（"因「XimoAgent」被想起"）。
func (b *SynapseBackend) viaReason(id string, parent map[string]string, nodes map[string]synapseNode) string {
	p := parent[id]
	if p == "" {
		return ""
	}
	if n, ok := nodes[p]; ok {
		return nodeDisplayName(n)
	}
	return ""
}

// associationLine 是 ↳ 关联 行的内容（不含前缀，供 Metadata 与 RenderBlock 共用）。
func associationLine(c recallCandidate, entities []string) string {
	if c.via == "seed" {
		return ""
	}
	who := c.reason
	if who == "" && len(entities) > 0 {
		who = entities[0]
	}
	if who == "" {
		return c.via
	}
	return fmt.Sprintf("%s（因「%s」被想起）", c.via, who)
}

// entitiesOf 取这些节点通过 mentions 边连到的 entity 名（每个节点保序）。
func (b *SynapseBackend) entitiesOf(ctx context.Context, ids []string) map[string][]string {
	return b.entitiesOfQuery(ctx, b.db, ids)
}

// entitiesOfQuery 是 entitiesOf 的可传事务版本。
func (b *SynapseBackend) entitiesOfQuery(ctx context.Context, q synQuerier, ids []string) map[string][]string {
	out := make(map[string][]string, len(ids))
	if len(ids) == 0 {
		return out
	}
	placeholders := strings.Repeat("?,", len(ids))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, 0, len(ids)+1)
	args = append(args, b.opts.UserID)
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := q.QueryContext(ctx, `
SELECT e.src, n.title
FROM mem_edges e
JOIN mem_nodes n ON n.id = e.dst
WHERE e.rel = 'mentions' AND n.kind = 'entity' AND n.user_id = ? AND e.src IN (`+placeholders+`)
ORDER BY e.weight DESC`, args...)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var src, title string
		if err := rows.Scan(&src, &title); err != nil {
			return out
		}
		out[src] = append(out[src], title)
	}
	return out
}

// RenderBlock 按文档 4.5 第 6 条渲染注入文本；没有可用条目时返回空串
// （"空块不插"是既有的 prompt-cache 不变量）。
//
// 格式：
//
//	[长期记忆 · 与当前任务相关]
//	• 项目 XimoAgent：使用 Go 引擎；测试命令 build.cmd verify
//	  ↳ 关联：…（因「XimoAgent」被想起）
//	（以上是过往交互中沉淀的信息，可能已过时；…）
func (b *SynapseBackend) RenderBlock(records []Record, maxChars int) string {
	if len(records) == 0 {
		return ""
	}
	if maxChars <= 0 {
		maxChars = DefaultSynapseRecallMaxChars
	}
	ordered := make([]Record, len(records))
	copy(ordered, records)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Score > ordered[j].Score })

	// 同一 entity 下的 fact 分组展示，避免重复主语（文档 4.5 第 5 条）。
	type group struct {
		title string
		items []Record
	}
	var groups []group
	index := map[string]int{}
	for _, r := range ordered {
		key := ""
		if g, ok := r.Metadata["group"].(string); ok {
			key = g
		}
		if key == "" {
			// 无实体的条目各自成组，键用 id 保证不合并。
			groups = append(groups, group{items: []Record{r}})
			continue
		}
		if i, ok := index[key]; ok {
			groups[i].items = append(groups[i].items, r)
			continue
		}
		index[key] = len(groups)
		groups = append(groups, group{title: key, items: []Record{r}})
	}

	var lines []string
	for _, g := range groups {
		parts := make([]string, 0, len(g.items))
		for _, r := range g.items {
			text := strings.Join(strings.Fields(r.Memory), " ")
			if text == "" {
				continue
			}
			parts = append(parts, text)
		}
		if len(parts) == 0 {
			continue
		}
		line := "• "
		if g.title != "" {
			line += g.title + "："
		}
		line += strings.Join(parts, "；")
		lines = append(lines, line)
		// ↳ 关联 行：只给扩散带出的节点（种子不写，避免噪声）。
		for _, r := range g.items {
			if assoc, _ := r.Metadata["associations"].(string); assoc != "" {
				lines = append(lines, "  ↳ 关联："+assoc)
			}
		}
	}

	used := len(RecallBlockHeader)
	var b2 strings.Builder
	b2.WriteString(RecallBlockHeader)
	keptLines := 0
	for _, line := range lines {
		if used+len(line)+1 > maxChars {
			continue
		}
		b2.WriteString("\n" + line)
		used += len(line) + 1
		keptLines++
	}
	if keptLines == 0 {
		return ""
	}
	// 结尾句必须保留：它是"旧记忆不得压过当下指令"的最后一道保险。
	if used+len(RecallBlockFooter)+1 <= maxChars {
		b2.WriteString("\n" + RecallBlockFooter)
	}
	return b2.String()
}

// embedQuery 调一次嵌入（可选路径）。失败只当没有向量。
func (b *SynapseBackend) embedQuery(ctx context.Context, query string) ([]float32, error) {
	if b.opts.Embedder == nil {
		return nil, nil
	}
	vecs, err := b.opts.Embedder.Embed(ctx, []string{query})
	if err != nil {
		return nil, err
	}
	if len(vecs) == 0 {
		return nil, nil
	}
	return vecs[0], nil
}

func keysOfHits(hits []ftsHit) []string {
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.ID)
	}
	return out
}

func candidateIDs(cands []recallCandidate) []string {
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.node.ID)
	}
	return out
}
