package memory

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"strings"
	"time"
)

// 本文件是学习与遗忘的纯函数层（文档 4.6）：惰性衰减、赫布强化、被想起却未被
// 采用的惩罚、以及"是否被采用"的确定性判定。
//
// 全是纯函数、无 IO、可单独测试：神经网络式的记忆最容易出问题的地方就是这些
// 数值规则，把它们从 SQL 里拆出来才能确定性地验证（文档验收项 4.12 的赫布 /
// 衰减两条就是直接测它们）。

// synapseLn2 是半衰期公式里的 ln2。
const synapseLn2 = 0.6931471805599453

// halfLifeDays 返回某类边的半衰期（天）与是否衰减。
//
// 文档 4.6：used_with 45 天；related 90 天；mentions / derived_from / part_of
// 不衰减（它们是结构性的连接，不是"被想起"的证据）。
func halfLifeDays(rel string) (float64, bool) {
	switch rel {
	case RelUsedWith:
		return SynapseHalfLifeUsedWithDays, true
	case RelRelated:
		return SynapseHalfLifeRelatedDays, true
	default:
		// mentions / derived_from / part_of / causes / same_topic 等结构边不衰减。
		return 0, false
	}
}

// edgeEffectiveWeight 读取边时按惰性衰减算有效权重（文档 4.6）。
//
// pinnedEndpoints 为真表示边的任一端是置顶节点：置顶节点及其 mentions 边
// 不衰减；这里把"任一端置顶 → 整条边不衰减"作为更宽的解释，理由是置顶是用户
// 明确的"这条别忘"信号，任何与它相连的联想都该保留。
func edgeEffectiveWeight(e synapseEdge, pinnedEndpoints bool, now time.Time) float64 {
	halfLife, decays := halfLifeDays(e.Rel)
	if !decays || pinnedEndpoints {
		return clamp01(e.Weight)
	}
	last := e.lastFiredOrCreated()
	if last.IsZero() || now.IsZero() || !now.After(last) {
		return clamp01(e.Weight)
	}
	days := now.Sub(last).Hours() / 24
	lambda := synapseLn2 / halfLife
	return clamp01(e.Weight * math.Exp(-lambda*days))
}

// decayedWeight 是 effectiveWeight 的标量形式（测试与整理任务直接用它）。
func decayedWeight(weight float64, last time.Time, halfLifeDaysValue float64, decays bool, now time.Time) float64 {
	if !decays || halfLifeDaysValue <= 0 {
		return clamp01(weight)
	}
	if last.IsZero() || now.IsZero() || !now.After(last) {
		return clamp01(weight)
	}
	days := now.Sub(last).Hours() / 24
	lambda := synapseLn2 / halfLifeDaysValue
	return clamp01(weight * math.Exp(-lambda*days))
}

// hebbianStep 是一次赫布更新：weight += η·(1-weight)，向 1 饱和，永远不会
// 达到或超过 1（文档 4.6 明确要求"不会爆"）。
func hebbianStep(weight float64) float64 {
	return clamp01(weight + SynapseHebbianEta*(1-clamp01(weight)))
}

// hebbianSaturate 连续做 n 次赫布更新，便于测试与批量写入。
func hebbianSaturate(weight float64, n int) float64 {
	for i := 0; i < n; i++ {
		weight = hebbianStep(weight)
	}
	return weight
}

// penalizedWeight 是"被想起却没用上"的惩罚：×0.97（文档 4.6）。
func penalizedWeight(weight float64) float64 {
	return clamp01(weight * SynapsePenaltyFactor)
}

// recencyBoost = 1 + 0.15·exp(-Δdays/30)（文档 4.5 第 4 条）。
//
// Δdays 从 last_used 起算；从未被用过则从 created_at 起算——"新写下的东西"
// 与"刚被用过的东西"都该得到同一份新近加成。
func recencyBoost(n synapseNode, now time.Time) float64 {
	base := n.LastUsed
	if base.IsZero() {
		base = n.CreatedAt
	}
	if base.IsZero() || now.IsZero() {
		return 1
	}
	days := now.Sub(base).Hours() / 24
	if days < 0 {
		days = 0
	}
	return 1 + 0.15*math.Exp(-days/30)
}

// scoreNode 是文档 4.5 第 4 条的打分函数：
//
//	score = activation × (0.6 + 0.4·importance) × recencyBoost × statusFactor
//	pinned 直接 +0.3
//
// superseded / archived 不参与召回，由调用方先过滤（这里 statusFactor 对
// active 恒为 1，公式保留以保证与文档逐字一致）。
func scoreNode(activation float64, n synapseNode, now time.Time) float64 {
	status := 1.0
	if n.Status != StatusActive {
		status = 0
	}
	score := activation * (0.6 + 0.4*clamp01(n.Importance)) * recencyBoost(n, now) * status
	if n.Pinned {
		score += SynapsePinnedBoost
	}
	return score
}

// adoptedBy 判定一个被召回的节点是否被最终答案"采用"（文档 4.6 的确定性规则）：
//
//   - 关键词重叠率 ≥ 阈值；或
//   - 该节点关联的 entity 名出现在答案里；或
//   - 有嵌入且余弦 ≥ 阈值（无嵌入时这条自然不成立）。
//
// 工具参数在文档里与"答案"并列作为证据来源，但 Backend 只拿到答案文本，
// 因此这里只按答案判定（调用方可以在喂答案时把工具参数摘要拼进去）。
func adoptedBy(n synapseNode, answerTokens []string, answerLower string, linkedEntities []string, cosine, threshold float64) bool {
	if threshold <= 0 {
		threshold = SynapseAdoptionOverlap
	}
	if overlapRatio(n, answerTokens) >= threshold {
		return true
	}
	for _, name := range linkedEntities {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			continue
		}
		if strings.Contains(answerLower, name) {
			return true
		}
	}
	if len(n.Embedding) > 0 && cosine >= threshold {
		return true
	}
	return false
}

// overlapRatio 是"关键词重叠率"，用较小的一侧做分母（containment）：
//
//	|node ∩ answer| / min(|node|, |answer|)
//
// 为什么不用并集或节点侧做分母：节点往往很长（一段完整的结论），而答案里对
// 它的引用可能只有一句话。用节点侧做分母会把比例稀释到阈值以下，于是"答案
// 明明复述了这条记忆"却被判为没用上。用较短的一侧做分母，含义是"较短的那段
// 文本的关键词有多少来自对方"，这才是"用上了"的合理判据。
func overlapRatio(n synapseNode, answerTokens []string) float64 {
	if len(answerTokens) == 0 {
		return 0
	}
	nodeToks := tokenize(strings.TrimSpace(n.Title + " " + n.Content))
	if len(nodeToks) == 0 {
		return 0
	}
	answer := make(map[string]bool, len(answerTokens))
	for _, t := range answerTokens {
		answer[t] = true
	}
	hit := 0
	seen := make(map[string]bool, len(nodeToks))
	for _, t := range nodeToks {
		if seen[t] {
			continue
		}
		seen[t] = true
		if answer[t] {
			hit++
		}
	}
	denom := len(seen)
	if len(answer) < denom {
		denom = len(answer)
	}
	if denom == 0 {
		return 0
	}
	return float64(hit) / float64(denom)
}

// cosineSimilarity 是两个等长 float32 向量的余弦相似度；任一为空或长度不等
// 时返回 0（没有嵌入就等价于"这条证据不存在"）。
func cosineSimilarity(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		av, bv := float64(a[i]), float64(b[i])
		dot += av * bv
		na += av * av
		nb += bv * bv
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// mergedImportance 把模型给的重要度与规则分加权（文档 4.4 第 3 条）：
// 强信号词（偏好 / 总是 / 不要 / 记住 …）给规则分 +0.2；模型不可用时直接用
// 规则分（低重要度 0.5 基线）。
func mergedImportance(modelValue float64, text string) float64 {
	rule := 0.5
	if hasStrongSignal(text) {
		rule += SynapseStrongSignalBonus
	}
	if modelValue <= 0 {
		return clamp01(rule)
	}
	return clamp01(0.5*modelValue + 0.5*rule)
}

// strongSignals 是"这条信息未来大概率还要用"的强信号词。
var strongSignals = []string{
	"偏好", "喜欢", "总是", "从不", "不要", "别用", "记住", "以后", "默认",
	"prefer", "always", "never", "remember", "do not", "don't",
}

func hasStrongSignal(text string) bool {
	lower := strings.ToLower(text)
	for _, s := range strongSignals {
		if strings.Contains(lower, s) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 赫布更新的落库侧（文档 4.4 第 8 步 / 4.6）
// ---------------------------------------------------------------------------

// recallLogUsedAdopted / recallLogUsedRejected 是 mem_recall_log.used 的两个
// 回填值。0 始终表示"尚未结算"。
//
// 为什么需要 -1 这个值：文档只说 used 表示"答案是否采用"，但"是否已经结算过"
// 必须可辨——否则每次写入都会把同一批召回再结算一遍，权重会被反复推高。把
// -1 定义为"已结算、未被采用"，既保留了"为什么想起它"的解释记录（不删除），
// 又让结算幂等。这是对既有列取值语义的追加，不改表结构。
const (
	recallLogPending  = 0
	recallLogAdopted  = 1
	recallLogRejected = -1
)

// settleLearning 结算"回溯窗口内所有未结算的召回批次"：被采用的节点两两强化
// used_with；被想起却没被采用的节点，其串联边轻微惩罚；只对 adopted 节点推进
// use_count / last_used（文档 4.6 明确要求）。
//
// 为什么按 mem_recall_log 而不是内存标志：批次是持久化的（used=0 的行），
// 崩溃重启后仍能补结算，不需要额外的可恢复状态。
func (b *SynapseBackend) settleLearning(ctx context.Context, q synQuerier, answer string, now time.Time) error {
	// 只回填"最近"的批次：更早的悬空批次对应的答案早已不可考，用当下的答案去
	// 判定它们会把联想学歪。
	cutoff := now.Add(-24 * time.Hour).Unix()
	rows, err := q.QueryContext(ctx,
		`SELECT run_id, node_id FROM mem_recall_log WHERE used=? AND ts >= ? ORDER BY ts`, recallLogPending, cutoff)
	if err != nil {
		return err
	}
	batches := map[string][]string{}
	var order []string
	for rows.Next() {
		var runID, nodeID string
		if err := rows.Scan(&runID, &nodeID); err != nil {
			rows.Close()
			return err
		}
		if _, ok := batches[runID]; !ok {
			order = append(order, runID)
		}
		batches[runID] = append(batches[runID], nodeID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if len(order) == 0 {
		return nil
	}

	answerTokens := answerTokensOf(answer)
	answerLower := strings.ToLower(answer)
	hasAnswer := strings.TrimSpace(answer) != ""

	for _, runID := range order {
		ids := batches[runID]
		nodes, err := b.getNodes(ctx, q, ids)
		if err != nil {
			return err
		}
		entityNames := b.entitiesOfQuery(ctx, q, ids)

		adopted := make(map[string]bool, len(nodes))
		for id, n := range nodes {
			if n.Status != StatusActive {
				continue
			}
			if hasAnswer && adoptedBy(n, answerTokens, answerLower, entityNames[id], 0, SynapseAdoptionOverlap) {
				adopted[id] = true
			}
		}

		// (a) 被采用的节点两两强化 used_with：η=0.15 向 1 饱和。
		usedIDs := make([]string, 0, len(adopted))
		for id := range adopted {
			usedIDs = append(usedIDs, id)
		}
		sortStrings(usedIDs)
		for i := 0; i < len(usedIDs); i++ {
			for j := i + 1; j < len(usedIDs); j++ {
				src, dst := usedIDs[i], usedIDs[j]
				if src > dst {
					src, dst = dst, src
				}
				w := SynapseUsedWithInit
				var existing float64
				err := q.QueryRowContext(ctx,
					`SELECT weight FROM mem_edges WHERE src=? AND dst=? AND rel=?`,
					src, dst, RelUsedWith).Scan(&existing)
				switch {
				case err == nil:
					w = existing
				case !errors.Is(err, sql.ErrNoRows):
					return err
				}
				w = hebbianStep(w)
				if _, err := b.upsertEdge(ctx, q, synapseEdge{
					Src: src, Dst: dst, Rel: RelUsedWith, Weight: w, FireCount: 1,
					LastFired: now, CreatedAt: now,
				}); err != nil {
					return err
				}
				if _, err := q.ExecContext(ctx,
					`UPDATE mem_edges SET fire_count = fire_count + 1, last_fired = ? WHERE src=? AND dst=? AND rel=?`,
					now.Unix(), src, dst, RelUsedWith); err != nil {
					return err
				}
			}
		}

		// (b) 被想起却没被采用的节点：把"把它带出来"的边 ×0.97。
		if hasAnswer {
			for _, id := range ids {
				if adopted[id] {
					continue
				}
				if _, ok := nodes[id]; !ok {
					continue
				}
				if err := b.penalizeEdgesTo(ctx, q, id, adopted, now); err != nil {
					return err
				}
			}
		}

		// (c) 只有 adopted 节点推进 use_count / last_used。
		for _, id := range usedIDs {
			if _, err := q.ExecContext(ctx,
				`UPDATE mem_nodes SET use_count = use_count + 1, last_used = ? WHERE id = ? AND user_id = ?`,
				now.Unix(), id, b.opts.UserID); err != nil {
				return err
			}
		}

		// (d) 回填 used：1=采用，-1=已结算未采用，保证幂等。
		for _, id := range ids {
			val := recallLogRejected
			if adopted[id] {
				val = recallLogAdopted
			}
			if _, err := q.ExecContext(ctx,
				`UPDATE mem_recall_log SET used=? WHERE run_id=? AND node_id=?`,
				val, runID, id); err != nil {
				return err
			}
		}
	}
	return nil
}

// penalizeEdgesTo 把 id 与任一 adopted 节点之间的边权重 ×0.97（并刷新
// last_fired）。只惩罚"同一批召回内部"的边——正是这些边把 id 带了出来，
// 它却没用上，说明这条联想不准。
func (b *SynapseBackend) penalizeEdgesTo(ctx context.Context, q synQuerier, id string, adopted map[string]bool, now time.Time) error {
	rows, err := q.QueryContext(ctx,
		`SELECT src, dst, rel FROM mem_edges WHERE src=? OR dst=?`, id, id)
	if err != nil {
		return err
	}
	type edgeRef struct{ src, dst, rel string }
	var refs []edgeRef
	for rows.Next() {
		var src, dst, rel string
		if err := rows.Scan(&src, &dst, &rel); err != nil {
			rows.Close()
			return err
		}
		if rel == RelSupersedes || rel == RelContradicts {
			continue
		}
		other := dst
		if dst == id {
			other = src
		}
		if !adopted[other] {
			continue
		}
		refs = append(refs, edgeRef{src: src, dst: dst, rel: rel})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	// 在 SQL 里直接算 weight*0.97，避免"读—改—写"之间的竞态。
	for _, r := range refs {
		if _, err := q.ExecContext(ctx,
			`UPDATE mem_edges SET weight = MIN(1.0, weight * ?), last_fired = ? WHERE src=? AND dst=? AND rel=?`,
			SynapsePenaltyFactor, now.Unix(), r.src, r.dst, r.rel); err != nil {
			return err
		}
	}
	return nil
}

// sortStrings 是插入排序的小包装，避免为几个元素 import sort（本文件其余
// 纯函数不依赖排序包）。
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
