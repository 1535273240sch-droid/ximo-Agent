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

// 本文件是写入流水线与图的基本读写门面（文档 4.4）：
//
//	run 完成 → 采集材料 → 脱敏 → 抽取 → 归一化 & 去重 → 实体链接 → 建边
//	→ 矛盾检测 → 赫布更新 → 单事务落库
//
// 以及 Backend 接口里的 GetAll / Delete。

// Add 写入一轮材料：抽取、去重、链接、建边、结算上一轮召回的赫布学习，全部落
// 在一个事务里（文档 4.4 第 9 条：失败整体回滚）。
func (b *SynapseBackend) Add(ctx context.Context, msgs []Message, opts AddOptions) ([]Record, error) {
	if b == nil || b.db == nil {
		return nil, ErrDisabled
	}
	now := b.now()

	// 幂等闸（可选）：沿用 ExtractionKey(runID) 的 Claim 语义，同一个 run 无论
	// 收尾多少次都只抽一次。经 Service 调用时不要注入 Ledger —— Service 已经在
	// extract() 里认领过一次，这里再认领会把自己挡掉。
	if b.opts.Ledger != nil && strings.TrimSpace(opts.RunID) != "" {
		claimed, err := b.opts.Ledger.Claim(ctx, ExtractionKey(opts.RunID))
		if err != nil {
			b.noteError(err)
			return nil, fmt.Errorf("memory: synapse 幂等认领失败: %w", err)
		}
		if !claimed {
			return []Record{}, nil
		}
	}

	// 1. 采集材料（总长硬上限，超限按"失败信息 > 结论 > 过程"裁剪）。
	material := collectMaterial(msgs, b.opts.MaterialBytes)
	if material == "" {
		return []Record{}, nil
	}
	// 2. 脱敏：全文过 RedactString；含密钥 / 令牌 / 口令的片段整段丢弃。
	clean, droppedSecrets := redactAndDropSecrets(material)
	if droppedSecrets > 0 {
		// 只记一条计数性的告警，不把被丢掉的原文写进任何地方。
		b.noteError(fmt.Errorf("memory: synapse 丢弃了 %d 个含疑似凭据的材料片段", droppedSecrets))
	}
	if strings.TrimSpace(clean) == "" {
		// 整份材料都是凭据：不落库、不进抽取提示，但仍结算上一轮的学习
		// （召回日志与答案无关，不能因为这一轮没材料就永远悬着）。
		return b.settleOnly(ctx, opts.RunID, "", now)
	}

	// 3. 抽取（模型优先，失败回退规则抽取，不重试）。
	extraction := b.extractMaterial(ctx, clean)

	// 异步的召回日志必须先落盘：下面的赫布结算要读它（有界等待，见 drainBackground）。
	b.drainBackground(b.opts.BackgroundFlushWait)

	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		b.noteError(err)
		return nil, fmt.Errorf("memory: synapse 开始写事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 4. 先说赫布：上一轮召回里"被采用"的节点强化，"被想起没用上"的边轻微惩罚。
	if err := b.settleLearning(ctx, tx, b.answerFrom(msgs), now); err != nil {
		b.noteError(err)
		return nil, fmt.Errorf("memory: synapse 赫布更新失败: %w", err)
	}

	// 5. 落库抽取结果。
	records, err := b.storeExtraction(ctx, tx, extraction, opts.RunID, now)
	if err != nil {
		b.noteError(err)
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		b.noteError(err)
		return nil, fmt.Errorf("memory: synapse 提交写事务失败: %w", err)
	}
	return records, nil
}

// settleOnly 在没有可写材料时只结算学习，保持写入流水线"最后一步总是赫布"的
// 语义一致。
func (b *SynapseBackend) settleOnly(ctx context.Context, runID, answer string, now time.Time) ([]Record, error) {
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := b.settleLearning(ctx, tx, answer, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return []Record{}, nil
}

// GetAll 列出最近 topK 条活跃节点（按创建时间倒序）。
func (b *SynapseBackend) GetAll(ctx context.Context, topK int) ([]Record, error) {
	if b == nil || b.db == nil {
		return nil, ErrDisabled
	}
	if topK <= 0 {
		topK = b.opts.TopK
	}
	rows, err := b.db.QueryContext(ctx, `SELECT `+synapseNodeCols+`
FROM mem_nodes WHERE user_id=? AND status=? ORDER BY created_at DESC LIMIT ?`,
		b.opts.UserID, StatusActive, topK)
	if err != nil {
		b.noteError(err)
		return nil, fmt.Errorf("memory: synapse 查询记忆失败: %w", err)
	}
	defer rows.Close()
	out := make([]Record, 0, topK)
	for rows.Next() {
		n, err := scanSynapseNode(rows)
		if err != nil {
			b.noteError(err)
			return nil, fmt.Errorf("memory: synapse 读取记忆失败: %w", err)
		}
		out = append(out, b.nodeRecord(n, 0, "list", ""))
	}
	if err := rows.Err(); err != nil {
		b.noteError(err)
		return nil, fmt.Errorf("memory: synapse 遍历记忆失败: %w", err)
	}
	return out, nil
}

// Delete 硬删除一个节点及其所有边（文档 4.8 的 forget：不是归档）。
func (b *SynapseBackend) Delete(ctx context.Context, id string) error {
	if b == nil || b.db == nil {
		return ErrDisabled
	}
	if strings.TrimSpace(id) == "" {
		return errors.New("memory: 删除需要 id")
	}
	res, err := b.db.ExecContext(ctx, `DELETE FROM mem_nodes WHERE id=? AND user_id=?`, id, b.opts.UserID)
	if err != nil {
		b.noteError(err)
		return fmt.Errorf("memory: synapse 删除失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("memory: 没有 id=%s 这条记忆", id)
	}
	return nil
}

// nodeRecord 把一个节点转成对外的 Record（Metadata 带 kind / importance / via）。
func (b *SynapseBackend) nodeRecord(n synapseNode, score float64, via, association string) Record {
	meta := map[string]any{
		"kind":       n.Kind,
		"importance": n.Importance,
		"via":        via,
		"pinned":     n.Pinned,
		"status":     n.Status,
	}
	if n.SourceRun != "" {
		meta["source_run"] = n.SourceRun
	}
	if n.Title != "" {
		meta["title"] = n.Title
	}
	if association != "" {
		meta["associations"] = association
	}
	return Record{
		ID:        n.ID,
		Memory:    n.Content,
		UserID:    b.opts.UserID,
		AgentID:   b.opts.AgentID,
		RunID:     n.SourceRun,
		Hash:      n.Hash,
		Score:     score,
		Metadata:  meta,
		CreatedAt: n.CreatedAt.Format(time.RFC3339),
		UpdatedAt: n.LastUsed.Format(time.RFC3339),
	}
}

// ---------------------------------------------------------------------------
// 1. 采集材料
// ---------------------------------------------------------------------------

// collectMaterial 把一轮消息折成材料文本，并在超过 maxBytes 时按
// "失败信息 > 结论 > 过程" 的优先级裁剪（文档 4.4 第 1 条）。
func collectMaterial(msgs []Message, maxBytes int) string {
	if maxBytes <= 0 {
		maxBytes = DefaultSynapseMaterialBytes
	}
	type line struct {
		text     string
		priority int
		order    int
	}
	var lines []line
	order := 0
	for _, m := range msgs {
		body := strings.TrimSpace(m.Content)
		if body == "" {
			continue
		}
		role := strings.TrimSpace(m.Role)
		switch role {
		case "user":
			role = "用户"
		case "assistant":
			role = "助手"
		case "":
			role = "记录"
		}
		for _, raw := range strings.Split(body, "\n") {
			t := strings.TrimSpace(raw)
			if t == "" {
				continue
			}
			lines = append(lines, line{text: role + "：" + t, priority: materialPriority(m.Role, t), order: order})
			order++
		}
	}
	if len(lines) == 0 {
		return ""
	}
	total := 0
	for _, l := range lines {
		total += len(l.text) + 1
		if total > maxBytes {
			break
		}
	}
	if total <= maxBytes {
		parts := make([]string, 0, len(lines))
		for _, l := range lines {
			parts = append(parts, l.text)
		}
		return strings.Join(parts, "\n")
	}
	// 超限：按优先级从高到低取，取够预算后按原始顺序还原（保留可读性）。
	ordered := make([]line, len(lines))
	copy(ordered, lines)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].priority > ordered[j].priority })
	kept := make([]line, 0, len(ordered))
	used := 0
	for _, l := range ordered {
		if used+len(l.text)+1 > maxBytes {
			continue
		}
		kept = append(kept, l)
		used += len(l.text) + 1
	}
	sort.SliceStable(kept, func(i, j int) bool { return kept[i].order < kept[j].order })
	parts := make([]string, 0, len(kept))
	for _, l := range kept {
		parts = append(parts, l.text)
	}
	return strings.Join(parts, "\n")
}

// materialPriority：失败信息最高，其次是助手结论，最后是过程。
func materialPriority(role, text string) int {
	lower := strings.ToLower(text)
	for _, marker := range []string{"失败", "错误", "报错", "error", "failed", "panic", "exception"} {
		if strings.Contains(lower, marker) {
			return 3
		}
	}
	if strings.TrimSpace(role) == "assistant" {
		return 2
	}
	return 1
}

// answerFrom 取最后一轮助手答复（用于"是否被采用"的判定）。
func (b *SynapseBackend) answerFrom(msgs []Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if strings.TrimSpace(msgs[i].Role) == "assistant" && strings.TrimSpace(msgs[i].Content) != "" {
			clean, _ := redactAndDropSecrets(msgs[i].Content)
			return clean
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// 3. 抽取
// ---------------------------------------------------------------------------

// extractMaterial 调用注入的抽取器；模型不可用或结果不可用时不重试，直接回退
// 规则抽取（文档 4.4 第 3 条：不重试风暴）。
func (b *SynapseBackend) extractMaterial(ctx context.Context, material string) SynapseExtraction {
	if b.opts.Extractor != nil {
		ectx, cancel := context.WithTimeout(ctx, b.opts.ExtractTimeout)
		defer cancel()
		ex, err := b.opts.Extractor.Extract(ectx, buildExtractionPrompt(material))
		if err == nil {
			return sanitizeExtraction(ex, b.opts.MaxFactsPerExtraction)
		}
		b.noteError(fmt.Errorf("memory: synapse 抽取失败，回退规则抽取: %w", err))
	}
	return b.ruleExtraction(material)
}

// buildExtractionPrompt 是要求严格 JSON 的抽取提示（文档 4.4 第 3 条的协议）。
//
// material 在进来之前已经过脱敏与"整段丢弃"，因此提示里不会出现凭据。
func buildExtractionPrompt(material string) string {
	return `你是长期记忆抽取器。从下面的材料里抽取"未来可能复用、且相对稳定"的信息；
一次性闲聊、显然的常识不要抽。每次最多 8 条 facts。
只输出一个 JSON 对象，不要任何解释文字、不要 Markdown 代码块：
{"episode":{"title":"","summary":""},"facts":[{"text":"","importance":0.0,"entities":["Go","XimoAgent"]}],"procedures":[{"title":"","steps":""}],"relations":[{"from":"","to":"","rel":"causes|related|part_of"}]}
importance 取 0..1；entities 写规范名（人 / 项目 / 工具 / 文件 / 概念）。
材料：
` + material
}

// sanitizeExtraction 清洗模型输出：丢空条目、截断到上限、把重要度夹到 0..1。
func sanitizeExtraction(ex SynapseExtraction, maxFacts int) SynapseExtraction {
	out := SynapseExtraction{}
	if ex.Episode != nil {
		ep := *ex.Episode
		ep.Title = strings.TrimSpace(ep.Title)
		ep.Summary = strings.TrimSpace(ep.Summary)
		if ep.Title != "" || ep.Summary != "" {
			out.Episode = &ep
		}
	}
	if maxFacts <= 0 {
		maxFacts = DefaultSynapseMaxFacts
	}
	seen := map[string]bool{}
	for _, f := range ex.Facts {
		text := strings.TrimSpace(types.RedactString(f.Text))
		if text == "" {
			continue
		}
		key := normalizeContent(text)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out.Facts = append(out.Facts, SynapseFact{
			Text:       text,
			Importance: clamp01(f.Importance),
			Entities:   dedupeStrings(f.Entities),
		})
		if len(out.Facts) >= maxFacts {
			break
		}
	}
	for _, p := range ex.Procedures {
		title := strings.TrimSpace(types.RedactString(p.Title))
		steps := strings.TrimSpace(types.RedactString(p.Steps))
		if title == "" && steps == "" {
			continue
		}
		out.Procedures = append(out.Procedures, SynapseProcedure{Title: title, Steps: steps})
	}
	for _, r := range ex.Relations {
		from := strings.TrimSpace(types.RedactString(r.From))
		to := strings.TrimSpace(types.RedactString(r.To))
		if from == "" || to == "" {
			continue
		}
		out.Relations = append(out.Relations, SynapseRelation{From: from, To: to, Rel: normalizeRel(r.Rel)})
	}
	return out
}

// normalizeRel 把模型给的 rel 收窄到文档允许的三类；未知的按 related 处理
// （表有 CHECK 约束，写进库之前必须先收窄）。
func normalizeRel(rel string) string {
	switch strings.ToLower(strings.TrimSpace(rel)) {
	case RelCauses:
		return RelCauses
	case RelPartOf:
		return RelPartOf
	case RelRelated:
		return RelRelated
	default:
		return RelRelated
	}
}

// ruleExtraction 是回退路径：把一问一答直存为一条低重要度 fact（沿用现有
// embedded 后端的做法），不编造抽取不出来的事实。
func (b *SynapseBackend) ruleExtraction(material string) SynapseExtraction {
	text := strings.TrimSpace(material)
	if text == "" {
		return SynapseExtraction{}
	}
	imp := 0.35 // 低重要度：没有模型背书，只是"原话留档"
	if hasStrongSignal(text) {
		imp = clamp01(imp + SynapseStrongSignalBonus)
	}
	return SynapseExtraction{Facts: []SynapseFact{{
		Text:       text,
		Importance: imp,
		Entities:   ruleEntities(text),
	}}}
}

// synapseRuleStopwords 是规则抽实体时要排除的高频英文词（它们不是实体）。
var synapseRuleStopwords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "this": true, "that": true,
	"from": true, "have": true, "will": true, "should": true, "must": true, "not": true,
	"are": true, "was": true, "were": true, "you": true, "your": true, "our": true,
	"user": true, "assistant": true, "error": true, "failed": true, "true": true,
	"false": true, "null": true, "json": true, "http": true, "https": true,
}

// ruleEntities 用规则从文本里抽实体（文档 4.4 第 5 条的回退路径）：
//   - ASCII 标识符（含 . _ - /），要求含字母、长度 ≥ 2，且首字母大写或含分隔符
//     （"go" 作为语言名单独放行）；
//   - 「引号」/《书名号》/`反引号` 里的名字。
func ruleEntities(text string) []string {
	var out []string
	for _, tok := range asciiIdentifiers(text) {
		lower := strings.ToLower(tok)
		if synapseRuleStopwords[lower] {
			continue
		}
		if len([]rune(tok)) < 2 || !hasASCIILetter(tok) {
			continue // 纯数字 / "1.2" 这类版本号不是实体
		}
		first, _ := firstRune(tok)
		if isUpperLetter(first) || strings.ContainsAny(tok, "._-/") || lower == "go" {
			out = append(out, tok)
		}
	}
	out = append(out, quotedNames(text)...)
	return dedupeStrings(out)
}

func hasASCIILetter(s string) bool {
	for _, r := range s {
		if isASCIILetter(r) {
			return true
		}
	}
	return false
}

// asciiIdentifiers 扫出连续的 ASCII 字母 / 数字 / . _ - / 片段。
func asciiIdentifiers(s string) []string {
	var out []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() >= 2 {
			out = append(out, cur.String())
		}
		cur.Reset()
	}
	for _, r := range s {
		switch {
		case r < 128 && (isASCIILetter(r) || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' || r == '/'):
			cur.WriteRune(r)
		default:
			flush()
		}
	}
	flush()
	return out
}

// quotedNames 抽 「X」/《X》/`X` 里的名字。
func quotedNames(s string) []string {
	var out []string
	pairs := [][2]rune{{'「', '」'}, {'《', '》'}, {'`', '`'}}
	for _, p := range pairs {
		rest := s
		for {
			i := strings.IndexRune(rest, p[0])
			if i < 0 {
				break
			}
			rest = rest[i+len(string(p[0])):]
			j := strings.IndexRune(rest, p[1])
			if j < 0 {
				break
			}
			name := strings.TrimSpace(rest[:j])
			if name != "" && len([]rune(name)) <= 40 {
				out = append(out, name)
			}
			rest = rest[j+len(string(p[1])):]
		}
	}
	return out
}

func isASCIILetter(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

func isUpperLetter(r rune) bool { return r >= 'A' && r <= 'Z' }

func firstRune(s string) (rune, bool) {
	for _, r := range s {
		return r, true
	}
	return 0, false
}

// ---------------------------------------------------------------------------
// 5~9. 落库：节点、实体链接、建边、矛盾检测
// ---------------------------------------------------------------------------

// storeExtraction 在一个事务里完成归一化去重、实体链接、建边与矛盾检测。
func (b *SynapseBackend) storeExtraction(ctx context.Context, tx *sql.Tx, ex SynapseExtraction, runID string, now time.Time) ([]Record, error) {
	records := make([]Record, 0, len(ex.Facts))
	factIDs := make(map[string]string, len(ex.Facts)) // 原文 → 节点 id
	factNodes := make(map[string]synapseNode, len(ex.Facts))
	entityIDs := make(map[string]string, 16) // entityKey → 节点 id

	var episodeID string
	if ex.Episode != nil {
		title := strings.TrimSpace(ex.Episode.Title)
		content := strings.TrimSpace(ex.Episode.Summary)
		if content == "" {
			content = title
		}
		if content != "" {
			id, _, err := b.insertNode(ctx, tx, synapseNode{
				ID: newSynapseID("mn_"), Kind: NodeEpisode, Title: title, Content: content,
				Importance: 0.5, SourceRun: runID, CreatedAt: now,
			})
			if err != nil {
				return nil, fmt.Errorf("memory: synapse 写 episode 失败: %w", err)
			}
			episodeID = id
		}
	}

	// --- facts ---
	for _, f := range ex.Facts {
		text := strings.TrimSpace(types.RedactString(f.Text))
		if text == "" || normalizeContent(text) == "" {
			continue
		}
		imp := mergedImportance(f.Importance, text)
		n := synapseNode{
			ID:         newSynapseID("mn_"),
			Kind:       NodeFact,
			Title:      truncateRunes(text, 32),
			Content:    text,
			Importance: imp,
			SourceRun:  runID,
			CreatedAt:  now,
		}
		id, created, err := b.insertNode(ctx, tx, n)
		if err != nil {
			return nil, fmt.Errorf("memory: synapse 写 fact 失败: %w", err)
		}
		factIDs[text] = id
		if !created {
			// 归一化后同 hash：只 use_count++ / 刷新 last_used，不新增、不重建边。
			records = append(records, b.nodeRecord(synapseNode{
				ID: id, Kind: NodeFact, Title: n.Title, Content: text,
				Importance: imp, SourceRun: runID, CreatedAt: now, Hash: n.Hash,
			}, 0, "write", ""))
			continue
		}
		stored := n
		stored.ID = id
		stored.Hash = contentHash(text)
		factNodes[id] = stored
		records = append(records, b.nodeRecord(stored, 0, "write", ""))

		if episodeID != "" {
			if _, err := b.upsertEdge(ctx, tx, synapseEdge{Src: id, Dst: episodeID, Rel: RelDerivedFrom,
				Weight: SynapseWeightDerivedFrom, CreatedAt: now}); err != nil {
				return nil, fmt.Errorf("memory: synapse 写 derived_from 失败: %w", err)
			}
		}

		// 实体链接：每个 fact 与其实体建 mentions 边（初始 0.5）。
		var linked []synapseNode
		for _, name := range f.Entities {
			ent, ok, err := b.linkEntity(ctx, tx, name, now)
			if err != nil {
				return nil, fmt.Errorf("memory: synapse 实体链接失败: %w", err)
			}
			if !ok {
				continue
			}
			linked = append(linked, ent)
			entityIDs[entityKey(name)] = ent.ID
			if _, err := b.upsertEdge(ctx, tx, synapseEdge{Src: id, Dst: ent.ID, Rel: RelMentions,
				Weight: SynapseWeightMentions, CreatedAt: now}); err != nil {
				return nil, fmt.Errorf("memory: synapse 写 mentions 失败: %w", err)
			}
		}
		// 矛盾检测（同实体 + 同谓词类别 + 值不同）。
		if err := b.detectContradictions(ctx, tx, id, text, linked, now); err != nil {
			return nil, fmt.Errorf("memory: synapse 矛盾检测失败: %w", err)
		}
	}

	// --- 同一次抽取里共现的 fact 两两建 related（初始 0.2）---
	ids := make([]string, 0, len(factNodes))
	for id := range factNodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for i := 0; i < len(ids); i++ {
		for j := i + 1; j < len(ids); j++ {
			if _, err := b.upsertEdge(ctx, tx, synapseEdge{Src: ids[i], Dst: ids[j], Rel: RelRelated,
				Weight: SynapseWeightRelated, CreatedAt: now}); err != nil {
				return nil, fmt.Errorf("memory: synapse 写 related 失败: %w", err)
			}
		}
	}

	// --- procedures ---
	procedureIDs := make(map[string]string, len(ex.Procedures))
	for _, p := range ex.Procedures {
		title := strings.TrimSpace(types.RedactString(p.Title))
		steps := strings.TrimSpace(types.RedactString(p.Steps))
		content := steps
		if content == "" {
			content = title
		}
		if content == "" {
			continue
		}
		id, _, err := b.insertNode(ctx, tx, synapseNode{
			ID: newSynapseID("mn_"), Kind: NodeProcedure, Title: title, Content: content,
			Importance: 0.6, SourceRun: runID, CreatedAt: now,
		})
		if err != nil {
			return nil, fmt.Errorf("memory: synapse 写 procedure 失败: %w", err)
		}
		procedureIDs[title] = id
		if episodeID != "" {
			if _, err := b.upsertEdge(ctx, tx, synapseEdge{Src: id, Dst: episodeID, Rel: RelDerivedFrom,
				Weight: SynapseWeightDerivedFrom, CreatedAt: now}); err != nil {
				return nil, err
			}
		}
	}

	// --- 模型给出的 relations（初始 0.4）---
	for _, r := range ex.Relations {
		src := resolveRef(factIDs, procedureIDs, entityIDs, r.From)
		dst := resolveRef(factIDs, procedureIDs, entityIDs, r.To)
		if src == "" || dst == "" || src == dst {
			continue
		}
		if _, err := b.upsertEdge(ctx, tx, synapseEdge{Src: src, Dst: dst, Rel: normalizeRel(r.Rel),
			Weight: SynapseWeightRelation, CreatedAt: now}); err != nil {
			return nil, fmt.Errorf("memory: synapse 写 relation 失败: %w", err)
		}
	}

	return records, nil
}

// linkEntity 按规范化名字找已有 entity 节点，没有就新建（文档 4.4 第 5 条）。
func (b *SynapseBackend) linkEntity(ctx context.Context, q synQuerier, name string, now time.Time) (synapseNode, bool, error) {
	key := entityKey(name)
	if key == "" {
		return synapseNode{}, false, nil
	}
	title := entityTitle(name)
	hash := hashString("entity\x00" + key)
	n := synapseNode{
		ID:         newSynapseID("mn_"),
		Kind:       NodeEntity,
		Title:      title,
		Content:    title,
		Hash:       hash,
		Tokens:     synapseTokens(title, title),
		Importance: 0.5,
		CreatedAt:  now,
	}
	id, _, err := b.insertNode(ctx, q, n)
	if err != nil {
		return synapseNode{}, false, err
	}
	n.ID = id
	return n, true, nil
}

// resolveRef 把模型给的 from/to 引用解析成节点 id：先按原文精确匹配 fact，
// 再按标题匹配 procedure，最后按 entity 名匹配。
func resolveRef(factIDs, procedureIDs, entityIDs map[string]string, ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	if id, ok := factIDs[ref]; ok {
		return id
	}
	if id, ok := procedureIDs[ref]; ok {
		return id
	}
	if id, ok := entityIDs[entityKey(ref)]; ok {
		return id
	}
	// 退化到规范化匹配（大小写 / 空白差异）。
	norm := normalizeContent(ref)
	for text, id := range factIDs {
		if normalizeContent(text) == norm {
			return id
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// 7. 矛盾检测
// ---------------------------------------------------------------------------

// detectContradictions 对"与同一实体相连的已有 fact"做冲突判定：
//   - 同实体 + 同谓词类别 + 值不同（如"偏好深色 / 偏好浅色"）→ 旧节点
//     status='superseded'，建 新—supersedes→旧；
//   - 有否定差异但无法确定谁取代谁 → 建 contradicts 边，两者都保留。
//
// 有嵌入时的"相似度高但含否定/相反词"路径同理，只是多一条余弦证据；本后端
// 在无嵌入时也完整可用（文档 4.11 的有意取舍）。
func (b *SynapseBackend) detectContradictions(ctx context.Context, tx *sql.Tx, newID, newText string, entities []synapseNode, now time.Time) error {
	if len(entities) == 0 {
		return nil
	}
	placeholders := strings.Repeat("?,", len(entities))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, 0, len(entities)+2)
	args = append(args, b.opts.UserID, newID)
	for _, e := range entities {
		args = append(args, e.ID)
	}
	rows, err := tx.QueryContext(ctx, `
SELECT `+synapseNodeCols+`
FROM mem_nodes
WHERE user_id=? AND kind='fact' AND status='active' AND id <> ?
  AND id IN (SELECT e.src FROM mem_edges e WHERE e.rel='mentions' AND e.dst IN (`+placeholders+`))`,
		args...)
	if err != nil {
		return err
	}
	var existing []synapseNode
	for rows.Next() {
		n, err := scanSynapseNode(rows)
		if err != nil {
			rows.Close()
			return err
		}
		existing = append(existing, n)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	for _, old := range existing {
		switch contradictionKind(newText, old.Content) {
		case "supersede":
			if _, err := tx.ExecContext(ctx, `UPDATE mem_nodes SET status=? WHERE id=? AND user_id=?`,
				StatusSuperseded, old.ID, b.opts.UserID); err != nil {
				return err
			}
			if _, err := b.upsertEdge(ctx, tx, synapseEdge{Src: newID, Dst: old.ID, Rel: RelSupersedes,
				Weight: 0.8, CreatedAt: now}); err != nil {
				return err
			}
		case "contradict":
			if _, err := b.upsertEdge(ctx, tx, synapseEdge{Src: newID, Dst: old.ID, Rel: RelContradicts,
				Weight: 0.5, CreatedAt: now}); err != nil {
				return err
			}
		}
	}
	return nil
}

// synapsePredicateMarkers 是"谓词类别"的识别词：两条事实都含同一个词，才可能
// 在说同一件事（"偏好 / 使用 / 默认 …"）。
var synapsePredicateMarkers = []string{
	"偏好", "喜欢", "习惯", "使用", "采用", "默认", "不要", "别用", "禁用", "启用",
	"prefer", "use", "default",
}

// synapseValueDimensions 是互斥取值维度：同一维度里取了不同的值 = 冲突。
var synapseValueDimensions = [][]string{
	{"深色", "暗色", "黑色", "dark", "浅色", "亮色", "白色", "light"},
	{"中文", "汉语", "chinese", "英文", "英语", "english"},
	{"开启", "启用", "打开", "on", "关闭", "禁用", "关掉", "off"},
	{"windows", "linux", "macos", "darwin"},
}

// contradictionKind 返回 "" / "supersede" / "contradict"。
func contradictionKind(a, b string) string {
	la, lb := strings.ToLower(a), strings.ToLower(b)
	sharedPredicate := false
	for _, p := range synapsePredicateMarkers {
		if strings.Contains(la, p) && strings.Contains(lb, p) {
			sharedPredicate = true
			break
		}
	}
	if !sharedPredicate {
		return ""
	}
	if va, vb := conflictingValues(la, lb); va != "" && vb != "" && va != vb {
		return "supersede"
	}
	// 否定差异 + 高重合：无法确定谁取代谁 → 交给整理任务或用户裁决。
	if negated(la) != negated(lb) && tokenJaccard(tokenize(la), tokenize(lb)) >= 0.5 {
		return "contradict"
	}
	return ""
}

// conflictingValues 返回两条文本在某个互斥维度里各自命中的取值；不在同一维度
// 或取值相同则返回空串。
func conflictingValues(a, b string) (string, string) {
	for _, dim := range synapseValueDimensions {
		va := firstContained(a, dim)
		vb := firstContained(b, dim)
		if va != "" && vb != "" {
			return va, vb
		}
	}
	return "", ""
}

func firstContained(text string, values []string) string {
	for _, v := range values {
		if strings.Contains(text, v) {
			return v
		}
	}
	return ""
}

func negated(text string) bool {
	for _, n := range []string{"不要", "不用", "别", "禁止", "不", "never", "not ", "don't", "do not"} {
		if strings.Contains(text, n) {
			return true
		}
	}
	return false
}

// tokenJaccard 是两个词集合的 Jaccard 相似度。
func tokenJaccard(a, b []string) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	setA := make(map[string]bool, len(a))
	for _, t := range a {
		setA[t] = true
	}
	setB := make(map[string]bool, len(b))
	for _, t := range b {
		setB[t] = true
	}
	inter := 0
	for t := range setA {
		if setB[t] {
			inter++
		}
	}
	union := len(setA) + len(setB) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// answerTokensOf 是采用判定用的答案词集。
func answerTokensOf(answer string) []string { return tokenize(strings.ToLower(answer)) }
