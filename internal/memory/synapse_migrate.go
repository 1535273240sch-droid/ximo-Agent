package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// 本文件是迁移（文档 4.10）：首次启用 synapse 时把旧数据导入新图，幂等（以
// hash 去重），旧数据不删除。
//
//   - 旧 memories 表（EmbeddedBackend 的库文件）：每行导为 fact 节点
//     （importance=0.5），并用规则从文本里抽实体建边；
//   - knowledge 条目：导入为 fact / procedure，其 tags 变成 entity / topic
//     节点 + mentions 边。
//
// knowledge_entries 属于引擎库（internal/storage 的 schema），本包不 import
// 它；这里提供一个接收 *sql.DB 的重载，由装配方（bootstrap）在需要时传入。
// 未传入时只做 memories 的迁移，并在结果里标记 KnowledgeSkipped。

// MigrationResult 是一次迁移的统计。
type MigrationResult struct {
	// SourceMemories / SourceKnowledge 读到的旧行数。
	SourceMemories  int
	SourceKnowledge int
	// NodesCreated / NodesExisting 新图里新建 / 命中去重的节点数。
	NodesCreated  int
	NodesExisting int
	// EdgesCreated 新建的边数。
	EdgesCreated int
	// KnowledgeSkipped 为真表示没有可用的 knowledge 源（表缺失或未传入）。
	KnowledgeSkipped bool
}

// synapseLegacyRow 是旧 memories 表里迁移需要的一行。
type synapseLegacyRow struct {
	id, userID, agentID, runID, memory, createdAt string
}

// synapseKnowledgeRow 是 knowledge_entries 里迁移需要的一行。
type synapseKnowledgeRow struct {
	id, mode, title, content, tags, source, createdAt string
}

// MigrateFromLegacy 把旧 EmbeddedBackend 的 memories 表导入本图。重复调用不
// 会产生重复节点（同 hash 命中去重）。
func (b *SynapseBackend) MigrateFromLegacy(ctx context.Context, old *EmbeddedBackend) (MigrationResult, error) {
	var res MigrationResult
	if b == nil || b.db == nil {
		return res, ErrDisabled
	}
	if old == nil || old.db == nil {
		return res, errors.New("memory: 迁移需要可用的旧进程内后端")
	}
	before := b.countAllNodes(ctx)

	rows, err := old.db.QueryContext(ctx, `
SELECT id, user_id, agent_id, run_id, memory, created_at
FROM memories ORDER BY created_at`)
	if err != nil {
		return res, fmt.Errorf("memory: 读取旧 memories 失败: %w", err)
	}
	type legacyRow = synapseLegacyRow
	var batch []legacyRow
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := b.importLegacyRows(ctx, batch, &res); err != nil {
			return err
		}
		batch = batch[:0]
		return nil
	}
	for rows.Next() {
		var r legacyRow
		if err := rows.Scan(&r.id, &r.userID, &r.agentID, &r.runID, &r.memory, &r.createdAt); err != nil {
			rows.Close()
			return res, fmt.Errorf("memory: 读取旧 memories 失败: %w", err)
		}
		batch = append(batch, r)
		res.SourceMemories++
		if len(batch) >= 500 {
			if err := flush(); err != nil {
				rows.Close()
				return res, err
			}
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return res, fmt.Errorf("memory: 遍历旧 memories 失败: %w", err)
	}
	rows.Close()
	if err := flush(); err != nil {
		return res, err
	}
	res.NodesCreated = b.countAllNodes(ctx) - before
	return res, nil
}

// importLegacyRows 在一个事务里导入一批旧行（fact + 规则实体）。
func (b *SynapseBackend) importLegacyRows(ctx context.Context, rows []synapseLegacyRow, res *MigrationResult) error {
	now := b.now()
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, r := range rows {
		text := strings.TrimSpace(types.RedactString(r.memory))
		if text == "" || normalizeContent(text) == "" {
			continue
		}
		// 幂等闸放在 insertNode 之前：insertNode 命中同 hash 时会 use_count++，
		// 那是"这条记忆又被用到了一次"的语义，迁移重跑不该伪造它。这里先查
		// 存在性，存在就只记统计、不碰任何计数。
		if _, exists, err := b.nodeIDByHash(ctx, tx, NodeFact, contentHash(text)); err != nil {
			return fmt.Errorf("memory: 迁移旧记忆失败: %w", err)
		} else if exists {
			res.NodesExisting++
			continue
		}
		created := now
		if t, err := time.Parse(time.RFC3339, r.createdAt); err == nil {
			created = t.UTC()
		}
		id, createdNow, err := b.insertNode(ctx, tx, synapseNode{
			ID: newSynapseID("mn_"), Kind: NodeFact, Title: truncateRunes(text, 32),
			Content: text, Importance: 0.5, SourceRun: r.runID, CreatedAt: created,
		})
		if err != nil {
			return fmt.Errorf("memory: 迁移旧记忆失败: %w", err)
		}
		if !createdNow {
			res.NodesExisting++
			continue // 幂等：同 hash 已在图里
		}
		for _, name := range ruleEntities(text) {
			ent, ok, err := b.linkEntity(ctx, tx, name, now)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			if created, err := b.upsertEdge(ctx, tx, synapseEdge{Src: id, Dst: ent.ID, Rel: RelMentions,
				Weight: SynapseWeightMentions, CreatedAt: now}); err != nil {
				return err
			} else if created {
				res.EdgesCreated++
			}
		}
	}
	return tx.Commit()
}

// MigrateFromKnowledge 导入 knowledge_entries 表。tags 变成 entity / topic
// 节点 + mentions 边；内容像步骤的条目导入为 procedure，其余为 fact。
//
// db 为 nil 或表不存在时返回 KnowledgeSkipped=true 而不是错误：迁移是首次启用
// 的顺带动作，缺一张表不该阻止 memories 的导入。
func (b *SynapseBackend) MigrateFromKnowledge(ctx context.Context, db *sql.DB) (MigrationResult, error) {
	var res MigrationResult
	if b == nil || b.db == nil {
		return res, ErrDisabled
	}
	if db == nil {
		res.KnowledgeSkipped = true
		return res, nil
	}
	if !tableExists(ctx, db, "knowledge_entries") {
		res.KnowledgeSkipped = true
		return res, nil
	}
	before := b.countAllNodes(ctx)
	rows, err := db.QueryContext(ctx, `
SELECT id, mode, title, content, tags_json, COALESCE(source,''), created_at
FROM knowledge_entries ORDER BY created_at`)
	if err != nil {
		return res, fmt.Errorf("memory: 读取 knowledge 条目失败: %w", err)
	}
	type knowledgeRow = synapseKnowledgeRow
	var batch []knowledgeRow
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := b.importKnowledgeRows(ctx, batch, &res); err != nil {
			return err
		}
		batch = batch[:0]
		return nil
	}
	for rows.Next() {
		var r knowledgeRow
		if err := rows.Scan(&r.id, &r.mode, &r.title, &r.content, &r.tags, &r.source, &r.createdAt); err != nil {
			rows.Close()
			return res, fmt.Errorf("memory: 读取 knowledge 条目失败: %w", err)
		}
		batch = append(batch, r)
		res.SourceKnowledge++
		if len(batch) >= 500 {
			if err := flush(); err != nil {
				rows.Close()
				return res, err
			}
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return res, fmt.Errorf("memory: 遍历 knowledge 条目失败: %w", err)
	}
	rows.Close()
	if err := flush(); err != nil {
		return res, err
	}
	res.NodesCreated = b.countAllNodes(ctx) - before
	return res, nil
}

func (b *SynapseBackend) importKnowledgeRows(ctx context.Context, rows []synapseKnowledgeRow, res *MigrationResult) error {
	now := b.now()
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	tagUse := map[string]int{}
	tagNodes := map[string]string{}
	for _, r := range rows {
		title := strings.TrimSpace(types.RedactString(r.title))
		content := strings.TrimSpace(types.RedactString(r.content))
		if title == "" && content == "" {
			continue
		}
		if content == "" {
			content = title
		}
		created := now
		if t, err := time.Parse(time.RFC3339, r.createdAt); err == nil {
			created = t.UTC()
		}
		kind := NodeFact
		if looksLikeProcedure(content) {
			kind = NodeProcedure
		}
		// 同 importLegacyRows：先查存在性，避免重跑时把 use_count 顶高。
		if _, exists, err := b.nodeIDByHash(ctx, tx, kind, contentHash(content)); err != nil {
			return fmt.Errorf("memory: 迁移 knowledge 条目失败: %w", err)
		} else if exists {
			res.NodesExisting++
			continue
		}
		id, createdNow, err := b.insertNode(ctx, tx, synapseNode{
			ID: newSynapseID("mn_"), Kind: kind, Title: title, Content: content,
			Importance: 0.6, SourceRun: r.source, CreatedAt: created,
		})
		if err != nil {
			return fmt.Errorf("memory: 迁移 knowledge 条目失败: %w", err)
		}
		if !createdNow {
			res.NodesExisting++
			continue
		}
		for _, tag := range parseTags(r.tags) {
			tagUse[tag]++
			ent, ok, err := b.linkEntity(ctx, tx, tag, now)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			tagNodes[tag] = ent.ID
			if created, err := b.upsertEdge(ctx, tx, synapseEdge{Src: id, Dst: ent.ID, Rel: RelMentions,
				Weight: SynapseWeightMentions, CreatedAt: now}); err != nil {
				return err
			} else if created {
				res.EdgesCreated++
			}
		}
	}
	// 出现 ≥3 次的 tag 单独建一个 topic 节点，把用到它的条目 part_of 进去
	// （"tags 变 entity/topic"）。
	for tag, n := range tagUse {
		if n < 3 {
			continue
		}
		entID := tagNodes[tag]
		if entID == "" {
			continue
		}
		topicID, _, err := b.insertNode(ctx, tx, synapseNode{
			ID: newSynapseID("mn_"), Kind: NodeTopic, Title: "主题：" + entityTitle(tag),
			Content: "由 knowledge tags 聚合：" + tag, Importance: 0.6, CreatedAt: now,
		})
		if err != nil {
			return err
		}
		if _, err := b.upsertEdge(ctx, tx, synapseEdge{Src: topicID, Dst: entID, Rel: RelSameTopic,
			Weight: SynapseWeightRelated, CreatedAt: now}); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// looksLikeProcedure 用步骤标记判断一条知识是不是"可复用的做法"。
func looksLikeProcedure(content string) bool {
	lower := strings.ToLower(content)
	for _, marker := range []string{"步骤", "第1步", "第 1 步", "step 1", "steps:", "\n1.", "1.", "1、"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// parseTags 解析 knowledge_entries.tags_json（[]string 或 {"tags":[...]}）。
func parseTags(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" {
		return nil
	}
	var list []string
	if err := json.Unmarshal([]byte(raw), &list); err == nil {
		return dedupeStrings(list)
	}
	var wrapped struct {
		Tags []string `json:"tags"`
	}
	if err := json.Unmarshal([]byte(raw), &wrapped); err == nil {
		return dedupeStrings(wrapped.Tags)
	}
	return nil
}

// nodeIDByHash 按 (user_id, kind, hash) 找已有节点，用于迁移的幂等闸。
//
// 为什么不用 insertNode 自带的去重：insertNode 命中同 hash 时会 use_count++，
// 那是"这条记忆又被用到了一次"的语义。迁移重跑（例如上次迁移中途失败）不该
// 伪造这个计数，否则用户会看到"我什么都没干，记忆的使用次数涨了"。
func (b *SynapseBackend) nodeIDByHash(ctx context.Context, q synQuerier, kind, hash string) (string, bool, error) {
	var id string
	err := q.QueryRowContext(ctx, `SELECT id FROM mem_nodes WHERE user_id=? AND kind=? AND hash=?`,
		b.opts.UserID, kind, hash).Scan(&id)
	switch {
	case err == nil:
		return id, true, nil
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	default:
		return "", false, err
	}
}

// countAllNodes 数当前用户的全部节点（迁移统计的差值基线）。
func (b *SynapseBackend) countAllNodes(ctx context.Context) int {
	var n int
	if err := b.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM mem_nodes WHERE user_id=?`, b.opts.UserID).Scan(&n); err != nil {
		return 0
	}
	return n
}

// tableExists 检查一张表是否存在（knowledge 迁移的前置探测）。
func tableExists(ctx context.Context, db *sql.DB, name string) bool {
	var n int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type IN ('table','view') AND name=?`, name).Scan(&n)
	return err == nil && n > 0
}

// MigrateAll 是一次性的迁移入口：memories 必做，knowledge 有就做。
func (b *SynapseBackend) MigrateAll(ctx context.Context, old *EmbeddedBackend, knowledgeDB *sql.DB) (MigrationResult, error) {
	memRes, err := b.MigrateFromLegacy(ctx, old)
	if err != nil {
		return memRes, err
	}
	kbRes, err := b.MigrateFromKnowledge(ctx, knowledgeDB)
	if err != nil {
		return memRes, err
	}
	memRes.SourceKnowledge = kbRes.SourceKnowledge
	memRes.KnowledgeSkipped = kbRes.KnowledgeSkipped
	return memRes, nil
}

// synapseMigrationUserVersion 是"旧数据迁移已完成"在 SQLite user_version 里的
// 标记值。
//
// 为什么用 PRAGMA user_version 而不是新增一张标记表：迁移状态是库自己的元数据，
// 不是记忆数据模型的一部分（文档 4.3 的表清单里不该多出一张）。user_version 是
// SQLite 内建的、不占 schema、不参与任何业务查询，正好放这类信息。
//
// 为什么需要它：只按"图里有没有节点"判断是不够的——用户清空全部记忆之后重启，
// 图是空的，会把早已迁移过的旧数据又导回来。
const synapseMigrationUserVersion = 1

// MigrationDone 报告旧数据迁移是否已经完成过。
func (b *SynapseBackend) MigrationDone(ctx context.Context) (bool, error) {
	if b == nil || b.db == nil {
		return false, ErrDisabled
	}
	var v int
	if err := b.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&v); err != nil {
		return false, fmt.Errorf("memory: 读取迁移标记失败: %w", err)
	}
	return v >= synapseMigrationUserVersion, nil
}

// MarkMigrationDone 记录迁移已完成。只有迁移成功返回后才应调用它。
func (b *SynapseBackend) MarkMigrationDone(ctx context.Context) error {
	if b == nil || b.db == nil {
		return ErrDisabled
	}
	// PRAGMA 不接受绑定参数，这里的值是一个包级常量，不存在注入面。
	if _, err := b.db.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version=%d`, synapseMigrationUserVersion)); err != nil {
		return fmt.Errorf("memory: 写入迁移标记失败: %w", err)
	}
	return nil
}
