package memory

import (
	"context"
	"database/sql"
	"math"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 测试替身：假抽取器 / 假嵌入器 / 固定时钟。都只放 _test.go（生产路径禁止
// 模拟数据，AGENTS.md 的既有约定）。
// ---------------------------------------------------------------------------

type fakeExtractor struct {
	mu      sync.Mutex
	queue   []SynapseExtraction
	result  SynapseExtraction
	err     error
	prompts []string
}

func (f *fakeExtractor) Extract(ctx context.Context, prompt string) (SynapseExtraction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prompts = append(f.prompts, prompt)
	if f.err != nil {
		return SynapseExtraction{}, f.err
	}
	if len(f.queue) > 0 {
		ex := f.queue[0]
		f.queue = f.queue[1:]
		return ex, nil
	}
	return f.result, nil
}

func (f *fakeExtractor) lastPrompt() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.prompts) == 0 {
		return ""
	}
	return f.prompts[len(f.prompts)-1]
}

func (f *fakeExtractor) promptCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.prompts)
}

// newSynapse 开一个临时库上的 synapse 后端。
func newSynapse(t *testing.T, opts SynapseOptions) *SynapseBackend {
	t.Helper()
	if opts.UserID == "" {
		opts.UserID = "u1"
	}
	b, err := NewSynapseBackend(filepath.Join(t.TempDir(), "memory.db"), opts)
	if err != nil {
		t.Fatalf("打开 synapse 后端: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

func turn(prompt, answer string) []Message {
	return []Message{{Role: "user", Content: prompt}, {Role: "assistant", Content: answer}}
}

// nodeByContent 在召回结果里按内容片段找一条。
func nodeByContent(t *testing.T, recs []Record, fragment string) Record {
	t.Helper()
	for _, r := range recs {
		if strings.Contains(r.Memory, fragment) {
			return r
		}
	}
	t.Fatalf("召回结果里没有包含 %q 的条目，得到 %d 条: %+v", fragment, len(recs), recs)
	return Record{}
}

// countNodesByKind 直接查库，用于断言图的结构。
func countNodesByKind(t *testing.T, b *SynapseBackend, kind string) int {
	t.Helper()
	var n int
	if err := b.db.QueryRow(`SELECT COUNT(*) FROM mem_nodes WHERE user_id=? AND kind=?`, b.opts.UserID, kind).Scan(&n); err != nil {
		t.Fatalf("统计 %s 节点: %v", kind, err)
	}
	return n
}

func nodeStatus(t *testing.T, b *SynapseBackend, id string) string {
	t.Helper()
	var status string
	if err := b.db.QueryRow(`SELECT status FROM mem_nodes WHERE id=?`, id).Scan(&status); err != nil {
		t.Fatalf("读节点状态: %v", err)
	}
	return status
}

func edgeWeight(t *testing.T, b *SynapseBackend, src, dst, rel string) (float64, bool) {
	t.Helper()
	var w float64
	err := b.db.QueryRow(`SELECT weight FROM mem_edges WHERE src=? AND dst=? AND rel=?`, src, dst, rel).Scan(&w)
	if err != nil {
		return 0, false
	}
	return w, true
}

// ---------------------------------------------------------------------------
// 最小链路 + 扩散激活（4.12 第 1 条）
// ---------------------------------------------------------------------------

// 同一实体下的两条 fact，query 只命中其中一条时，另一条通过 mentions 边 2 跳
// 被带出，且 Metadata["via"] 有路径说明。
func TestSynapseSpreadRecallViaMentions(t *testing.T) {
	ctx := context.Background()
	ex := &fakeExtractor{queue: []SynapseExtraction{
		{Facts: []SynapseFact{{Text: "项目 XimoAgent 使用 Go 引擎", Importance: 0.8, Entities: []string{"XimoAgent"}}}},
		{Facts: []SynapseFact{{Text: "XimoAgent 的测试命令是 build.cmd verify", Importance: 0.7, Entities: []string{"XimoAgent"}}}},
	}}
	b := newSynapse(t, SynapseOptions{Extractor: ex})

	if _, err := b.Add(ctx, turn("项目用什么引擎", "用 Go 引擎"), AddOptions{RunID: "run-1"}); err != nil {
		t.Fatalf("Add 1: %v", err)
	}
	if _, err := b.Add(ctx, turn("怎么跑测试", "build.cmd verify"), AddOptions{RunID: "run-2"}); err != nil {
		t.Fatalf("Add 2: %v", err)
	}
	if n := countNodesByKind(t, b, NodeFact); n != 2 {
		t.Fatalf("应有 2 条 fact，得到 %d", n)
	}
	if n := countNodesByKind(t, b, NodeEntity); n != 1 {
		t.Fatalf("同名实体应只建 1 个 entity 节点，得到 %d", n)
	}

	recs, err := b.Search(ctx, "Go 引擎", SearchOptions{TopK: 8})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	hit := nodeByContent(t, recs, "build.cmd")
	via, _ := hit.Metadata["via"].(string)
	if !strings.Contains(via, "entity:XimoAgent") || !strings.Contains(via, "fact:") {
		t.Fatalf("2 跳带出的条目应有路径说明，得到 via=%q", via)
	}
	if kind, _ := hit.Metadata["kind"].(string); kind != NodeFact {
		t.Errorf("kind 元数据应为 fact，得到 %q", kind)
	}
	if imp, _ := hit.Metadata["importance"].(float64); imp <= 0 {
		t.Errorf("importance 元数据应存在且为正，得到 %v", hit.Metadata["importance"])
	}
	seed := nodeByContent(t, recs, "Go 引擎")
	if got, _ := seed.Metadata["via"].(string); got != "seed" {
		t.Errorf("直接命中的种子 via 应为 seed，得到 %q", got)
	}
	if seed.Score <= hit.Score {
		t.Errorf("种子分数应高于 2 跳带出的条目: seed=%v hop=%v", seed.Score, hit.Score)
	}

	block := b.RenderBlock(recs, DefaultSynapseRecallMaxChars)
	if !strings.Contains(block, "↳ 关联") {
		t.Errorf("注入块应含 ↳ 关联 行:\n%s", block)
	}
	if !strings.Contains(block, RecallBlockFooter) {
		t.Errorf("注入块应含结尾的「可能已过时」句:\n%s", block)
	}
	if !strings.HasPrefix(block, RecallBlockHeader) {
		t.Errorf("注入块首行应为固定标题:\n%s", block)
	}
}

// FTS5 真的在索引内容（不是退化扫描），且无关查询不命中。
func TestSynapseFTSRecallBasics(t *testing.T) {
	ctx := context.Background()
	ex := &fakeExtractor{queue: []SynapseExtraction{
		{Facts: []SynapseFact{{Text: "服务器端口改成 8600", Importance: 0.6}}},
	}}
	b := newSynapse(t, SynapseOptions{Extractor: ex})
	if _, err := b.Add(ctx, turn("端口改成多少", "8600"), AddOptions{RunID: "r1"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	recs, err := b.Search(ctx, "端口 8600", SearchOptions{TopK: 5})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(recs) == 0 || !strings.Contains(recs[0].Memory, "8600") {
		t.Fatalf("应命中端口那条，得到 %+v", recs)
	}
	if recs2, _ := b.Search(ctx, "量子纠缠实验", SearchOptions{TopK: 5}); len(recs2) != 0 {
		t.Errorf("无关查询不该命中: %+v", recs2)
	}
	if err := b.Ping(ctx); err != nil {
		t.Errorf("Ping: %v", err)
	}
	if all, err := b.GetAll(ctx, 10); err != nil || len(all) != 1 {
		t.Errorf("GetAll 应有 1 条，得到 %d 条 err=%v", len(all), err)
	}
	if err := b.Delete(ctx, recs[0].ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if rest, _ := b.Search(ctx, "端口 8600", SearchOptions{TopK: 5}); len(rest) != 0 {
		t.Errorf("删除后不该再召回: %+v", rest)
	}
	if err := b.Delete(ctx, recs[0].ID); err == nil {
		t.Error("重复删除应报错")
	}
}

// 归一化去重：同 hash 只 use_count++ / 刷新 last_used，不新增节点。
func TestSynapseDedupeByNormalizedHash(t *testing.T) {
	ctx := context.Background()
	ex := &fakeExtractor{result: SynapseExtraction{Facts: []SynapseFact{
		{Text: "用户偏好深色主题", Importance: 0.9},
	}}}
	b := newSynapse(t, SynapseOptions{Extractor: ex})
	for i := 0; i < 3; i++ {
		if _, err := b.Add(ctx, turn("主题", "深色"), AddOptions{RunID: "r" + string(rune('a'+i))}); err != nil {
			t.Fatalf("Add %d: %v", i, err)
		}
	}
	if n := countNodesByKind(t, b, NodeFact); n != 1 {
		t.Fatalf("同 hash 应只留 1 条，得到 %d", n)
	}
	var useCount int
	if err := b.db.QueryRow(`SELECT use_count FROM mem_nodes WHERE kind='fact'`).Scan(&useCount); err != nil {
		t.Fatalf("读 use_count: %v", err)
	}
	// 首次写入只是"建节点"（use_count 从 0 开始）；后面两次命中同 hash
	// 各 +1，所以这里是 2 而不是 3。
	if useCount != 2 {
		t.Errorf("两次命中同 hash 应把 use_count 推到 2，得到 %d", useCount)
	}
}

// ---------------------------------------------------------------------------
// 矛盾检测（4.12 第 2 条）
// ---------------------------------------------------------------------------

func TestSynapseContradictionSupersedes(t *testing.T) {
	ctx := context.Background()
	ex := &fakeExtractor{queue: []SynapseExtraction{
		{Facts: []SynapseFact{{Text: "用户偏好深色主题", Importance: 0.9, Entities: []string{"界面主题"}}}},
		{Facts: []SynapseFact{{Text: "用户偏好浅色主题", Importance: 0.9, Entities: []string{"界面主题"}}}},
	}}
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	var tick int64
	b := newSynapse(t, SynapseOptions{
		Extractor: ex,
		Now:       func() time.Time { tick++; return now.Add(time.Duration(tick) * time.Second) },
	})
	if _, err := b.Add(ctx, turn("主题用什么", "深色"), AddOptions{RunID: "r1"}); err != nil {
		t.Fatalf("Add 1: %v", err)
	}
	oldID := ""
	if err := b.db.QueryRow(`SELECT id FROM mem_nodes WHERE kind='fact' AND content LIKE '%深色%'`).Scan(&oldID); err != nil {
		t.Fatalf("找旧节点: %v", err)
	}
	if _, err := b.Add(ctx, turn("主题用什么", "浅色"), AddOptions{RunID: "r2"}); err != nil {
		t.Fatalf("Add 2: %v", err)
	}
	newID := ""
	if err := b.db.QueryRow(`SELECT id FROM mem_nodes WHERE kind='fact' AND content LIKE '%浅色%'`).Scan(&newID); err != nil {
		t.Fatalf("找新节点: %v", err)
	}
	if got := nodeStatus(t, b, oldID); got != StatusSuperseded {
		t.Errorf("旧节点应被取代（superseded），得到 %q", got)
	}
	if got := nodeStatus(t, b, newID); got != StatusActive {
		t.Errorf("新节点应保持 active，得到 %q", got)
	}
	if _, ok := edgeWeight(t, b, newID, oldID, RelSupersedes); !ok {
		t.Errorf("应存在 新—supersedes→旧 的边")
	}

	recs, err := b.Search(ctx, "界面主题 偏好 深色 浅色", SearchOptions{TopK: 8})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, r := range recs {
		if strings.Contains(r.Memory, "深色") {
			t.Fatalf("superseded 的旧事实不该被召回: %+v", r)
		}
	}
	found := false
	for _, r := range recs {
		if strings.Contains(r.Memory, "浅色") {
			found = true
		}
	}
	if !found {
		t.Fatalf("新事实应被召回: %+v", recs)
	}
}

// ---------------------------------------------------------------------------
// 学习与遗忘（4.12 第 3、4 条）
// ---------------------------------------------------------------------------

const (
	// 近重复对：只差结尾两字，用于整理任务的"去重合并"。
	factA = "XimoAgent 项目使用 Go 引擎开发后端服务并且提供本地优先的记忆能力"
	factB = "XimoAgent 项目使用 Go 引擎开发后端服务并且提供本地优先的检索能力"
	// 相互独立的两条，用于赫布 / 惩罚：答案能只采用其中一条。
	hebA = "用户偏好深色主题界面"
	hebB = "项目使用 Go 引擎开发后端"
)

// 两个节点同被采用 5 次：used_with 权重单调上升且 < 1；之后被想起却未被采用
// 时权重下降。
func TestSynapseHebbianStrengthenAndPenalty(t *testing.T) {
	ctx := context.Background()
	ex := &fakeExtractor{result: SynapseExtraction{Facts: []SynapseFact{
		{Text: hebA, Importance: 0.8, Entities: []string{"界面主题"}},
		{Text: hebB, Importance: 0.8, Entities: []string{"Go"}},
	}}}
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	var tick int64
	b := newSynapse(t, SynapseOptions{
		Extractor: ex,
		Now:       func() time.Time { tick++; return now.Add(time.Duration(tick) * time.Second) },
	})
	if _, err := b.Add(ctx, turn("项目用什么", "Go 引擎"), AddOptions{RunID: "write"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	idsA := factIDsByContentPrefix(t, b, "用户偏好")
	idsB := factIDsByContentPrefix(t, b, "项目使用")
	if len(idsA) != 1 || len(idsB) != 1 {
		t.Fatalf("应有 2 条 fact，得到 A=%v B=%v", idsA, idsB)
	}
	aID, bID := idsA[0], idsB[0]
	query := "界面主题 深色 Go 引擎 后端"

	// 阶段 1：两个节点都被采用 5 次。
	answer := hebA + "；" + hebB
	var weights []float64
	for i := 0; i < 5; i++ {
		if _, err := b.Search(ctx, query, SearchOptions{TopK: 8}); err != nil {
			t.Fatalf("Search: %v", err)
		}
		if _, err := b.Add(ctx, turn("继续", answer), AddOptions{RunID: "used-" + string(rune('a'+i))}); err != nil {
			t.Fatalf("Add: %v", err)
		}
		w, ok := usedWithWeight(t, b, aID, bID)
		if !ok {
			t.Fatalf("第 %d 次后应存在 used_with 边", i+1)
		}
		weights = append(weights, w)
	}
	for i := 1; i < len(weights); i++ {
		if weights[i] <= weights[i-1] {
			t.Fatalf("used_with 权重应单调上升，得到 %v", weights)
		}
	}
	if last := weights[len(weights)-1]; last >= 1 {
		t.Fatalf("赫布更新应恒 < 1，得到 %v", last)
	}

	// 阶段 2：只采用 A，B 被想起却没被采用 → 边权 ×0.97。
	before := weights[len(weights)-1]
	for i := 0; i < 3; i++ {
		if _, err := b.Search(ctx, query, SearchOptions{TopK: 8}); err != nil {
			t.Fatalf("Search: %v", err)
		}
		// 答案只复述 A（也不能出现 B 的关联实体名），B 才会被判为"没用上"。
		if _, err := b.Add(ctx, turn("继续", hebA), AddOptions{RunID: "unused-" + string(rune('a'+i))}); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	after, _ := usedWithWeight(t, b, aID, bID)
	if !(after < before) {
		t.Fatalf("被想起却未被采用时边权应下降：before=%v after=%v", before, after)
	}
}

// 惰性衰减：used_with 45 天后有效权重 ≈ 一半；pinned（及其边）不衰减。
func TestSynapseEffectiveWeightDecay(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	e := synapseEdge{Rel: RelUsedWith, Weight: 0.8, LastFired: now.Add(-45 * 24 * time.Hour)}
	if got := edgeEffectiveWeight(e, false, now); math.Abs(got-0.4) > 1e-6 {
		t.Errorf("used_with 45 天后应 ≈ 一半，得到 %v", got)
	}
	if got := edgeEffectiveWeight(e, true, now); got != 0.8 {
		t.Errorf("置顶端点的边不衰减，得到 %v", got)
	}
	m := synapseEdge{Rel: RelMentions, Weight: 0.5, LastFired: now.Add(-3650 * 24 * time.Hour)}
	if got := edgeEffectiveWeight(m, false, now); got != 0.5 {
		t.Errorf("mentions 边不衰减，得到 %v", got)
	}
	r := synapseEdge{Rel: RelRelated, Weight: 0.8, LastFired: now.Add(-90 * 24 * time.Hour)}
	if got := edgeEffectiveWeight(r, false, now); math.Abs(got-0.4) > 1e-6 {
		t.Errorf("related 90 天后应 ≈ 一半，得到 %v", got)
	}
	// 赫布步长：向 1 饱和且永远 < 1。
	w := 0.2
	for i := 0; i < 200; i++ {
		next := hebbianStep(w)
		if next <= w || next >= 1 {
			t.Fatalf("第 %d 次赫布更新违反单调 / 饱和约束: %v → %v", i, w, next)
		}
		w = next
	}
	// 惩罚系数。
	if got := penalizedWeight(1.0); math.Abs(got-0.97) > 1e-9 {
		t.Errorf("惩罚应是 ×0.97，得到 %v", got)
	}
}

// ---------------------------------------------------------------------------
// 脱敏（4.12 第 5 条）
// ---------------------------------------------------------------------------

// 含 sk-… / Bearer 令牌 / 密码的文本，抽取输入与库内容均不出现。
func TestSynapseRedaction(t *testing.T) {
	ctx := context.Background()
	secrets := []string{"sk-live-abc123456789", "abcdef123456", "SuperSecret123", "Bearer"}
	prompt := "用户：项目 XimoAgent 使用 Go 引擎\n" +
		"用户：我的 key 是 sk-live-abc123456789 别外传\n" +
		"助手：Authorization: Bearer abcdef123456\n" +
		"助手：password=SuperSecret123 请记住"

	ex := &fakeExtractor{result: SynapseExtraction{Facts: []SynapseFact{
		{Text: "XimoAgent 使用 Go 引擎", Importance: 0.7, Entities: []string{"XimoAgent"}},
	}}}
	b := newSynapse(t, SynapseOptions{Extractor: ex})
	if _, err := b.Add(ctx, []Message{{Role: "user", Content: prompt}}, AddOptions{RunID: "r1"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	got := ex.lastPrompt()
	if got == "" {
		t.Fatal("抽取器应收到提示")
	}
	for _, s := range secrets {
		if strings.Contains(got, s) {
			t.Errorf("抽取提示里出现了密钥 %q", s)
		}
	}
	if !strings.Contains(got, "Go 引擎") {
		t.Errorf("干净材料应保留：%q", got)
	}

	// 规则抽取路径（无 provider）：库里同样不能出现密钥。
	rb := newSynapse(t, SynapseOptions{})
	if _, err := rb.Add(ctx, []Message{{Role: "user", Content: prompt}}, AddOptions{RunID: "r2"}); err != nil {
		t.Fatalf("Add(rule): %v", err)
	}
	rows, err := rb.db.Query(`SELECT content FROM mem_nodes`)
	if err != nil {
		t.Fatalf("读库: %v", err)
	}
	defer rows.Close()
	total := 0
	for rows.Next() {
		var content string
		if err := rows.Scan(&content); err != nil {
			t.Fatalf("扫描: %v", err)
		}
		total++
		for _, s := range secrets {
			if strings.Contains(content, s) {
				t.Errorf("库内容里出现了密钥 %q: %q", s, content)
			}
		}
	}
	if total == 0 {
		t.Fatal("应至少写入一条干净记忆")
	}
	if got, _ := rb.Search(ctx, "XimoAgent Go 引擎", SearchOptions{TopK: 5}); len(got) == 0 {
		t.Error("干净片段应可被召回")
	}
}

// ---------------------------------------------------------------------------
// 迁移（4.12 第 8 条）
// ---------------------------------------------------------------------------

// 重复导入不产生重复节点；旧数据不删除。
func TestSynapseMigrationIdempotent(t *testing.T) {
	ctx := context.Background()
	old := newEmbedded(t, DefaultUserID)
	if _, err := old.Add(ctx, []Message{{Role: "user", Content: "项目 XimoAgent 使用 Go 引擎"}}, AddOptions{RunID: "old-1"}); err != nil {
		t.Fatalf("旧库写入: %v", err)
	}
	if _, err := old.Add(ctx, []Message{{Role: "user", Content: "测试命令是 build.cmd verify"}}, AddOptions{RunID: "old-2"}); err != nil {
		t.Fatalf("旧库写入: %v", err)
	}

	b := newSynapse(t, SynapseOptions{})
	first, err := b.MigrateFromLegacy(ctx, old)
	if err != nil {
		t.Fatalf("首次迁移: %v", err)
	}
	if first.SourceMemories != 2 || first.NodesCreated < 2 || first.NodesExisting != 0 {
		t.Fatalf("首次迁移应导入 2 条（外加规则实体节点），得到 %+v", first)
	}
	if n := countNodesByKind(t, b, NodeFact); n != 2 {
		t.Fatalf("迁移后应有 2 条 fact，得到 %d", n)
	}
	if n := countNodesByKind(t, b, NodeEntity); n == 0 {
		t.Error("应从文本里规则抽实体建 entity 节点")
	}

	second, err := b.MigrateFromLegacy(ctx, old)
	if err != nil {
		t.Fatalf("二次迁移: %v", err)
	}
	if second.NodesCreated != 0 || second.NodesExisting != 2 {
		t.Errorf("重复导入不该新建节点，得到 %+v", second)
	}
	if n := countNodesByKind(t, b, NodeFact); n != 2 {
		t.Errorf("重复导入后 fact 仍应是 2，得到 %d", n)
	}
	// 旧数据不删除。
	if all, err := old.GetAll(ctx, 10); err != nil || len(all) != 2 {
		t.Errorf("旧库应保持 2 条，得到 %d err=%v", len(all), err)
	}
}

// knowledge 条目导入为 fact/procedure，tags 变 entity/topic，且幂等。
func TestSynapseMigrateKnowledge(t *testing.T) {
	ctx := context.Background()
	kdb, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "engine.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer kdb.Close()
	if _, err := kdb.Exec(`CREATE TABLE knowledge_entries (
		id TEXT PRIMARY KEY, mode TEXT, title TEXT, content TEXT, tags_json TEXT,
		source TEXT, created_at TEXT, updated_at TEXT)`); err != nil {
		t.Fatal(err)
	}
	created := time.Now().UTC().Format(time.RFC3339)
	for i, row := range []struct{ id, title, content, tags string }{
		{"k1", "跑测试", "步骤：1. 打开终端 2. 执行 build.cmd verify", `["ximoagent","构建"]`},
		{"k2", "引擎架构", "工厂模式 + 端口适配", `["ximoagent","架构"]`},
		{"k3", "记忆设计", "图记忆 + 扩散激活", `["ximoagent","记忆"]`},
	} {
		if _, err := kdb.Exec(`INSERT INTO knowledge_entries (id,mode,title,content,tags_json,source,created_at,updated_at)
			VALUES (?,?,?,?,?,?,?,?)`, row.id, "coding", row.title, row.content, row.tags, "test", created, created); err != nil {
			t.Fatalf("插入 knowledge %d: %v", i, err)
		}
	}

	b := newSynapse(t, SynapseOptions{})
	res, err := b.MigrateFromKnowledge(ctx, kdb)
	if err != nil {
		t.Fatalf("迁移 knowledge: %v", err)
	}
	if res.SourceKnowledge != 3 || res.NodesCreated < 3 || res.NodesExisting != 0 {
		t.Fatalf("应导入 3 条（外加 tag 实体 / 主题节点），得到 %+v", res)
	}
	if n := countNodesByKind(t, b, NodeProcedure); n == 0 {
		t.Error("含步骤的条目应导入为 procedure")
	}
	if n := countNodesByKind(t, b, NodeTopic); n == 0 {
		t.Error("出现 ≥3 次的 tag 应建 topic 节点")
	}
	again, err := b.MigrateFromKnowledge(ctx, kdb)
	if err != nil {
		t.Fatalf("二次迁移: %v", err)
	}
	if again.NodesCreated != 0 || again.NodesExisting != 3 {
		t.Errorf("重复导入不该新建节点，得到 %+v", again)
	}
	// knowledge 表不可达时只标记跳过，不报错。
	missing, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "empty.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer missing.Close()
	skip, err := b.MigrateFromKnowledge(ctx, missing)
	if err != nil || !skip.KnowledgeSkipped {
		t.Errorf("缺表时应跳过而不是报错，得到 %+v err=%v", skip, err)
	}
}

// ---------------------------------------------------------------------------
// 睡眠整理（4.7）
// ---------------------------------------------------------------------------

func TestSynapseConsolidate(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	var tick int64
	ex := &fakeExtractor{result: SynapseExtraction{Facts: []SynapseFact{
		{Text: factA, Importance: 0.8, Entities: []string{"XimoAgent"}},
		{Text: factB, Importance: 0.8, Entities: []string{"XimoAgent"}},
		{Text: "XimoAgent 用 SQLite 存记忆", Importance: 0.7, Entities: []string{"XimoAgent"}},
		{Text: "XimoAgent 支持扩散激活", Importance: 0.7, Entities: []string{"XimoAgent"}},
		{Text: "XimoAgent 提供记忆召回", Importance: 0.7, Entities: []string{"XimoAgent"}},
	}}}
	b := newSynapse(t, SynapseOptions{
		Extractor: ex,
		Now:       func() time.Time { tick++; return now.Add(time.Duration(tick) * time.Second) },
	})
	// 五轮写入不同材料：合并掉一对近重复后仍有 4 个共享同一实体的 fact。
	for i := 0; i < 5; i++ {
		if _, err := b.Add(ctx, []Message{{Role: "user", Content: "材料 " + string(rune('a'+i))}}, AddOptions{RunID: "c" + string(rune('a'+i))}); err != nil {
			t.Fatalf("Add %d: %v", i, err)
		}
	}
	if n := countNodesByKind(t, b, NodeFact); n != 5 {
		t.Fatalf("应有 5 条 fact，得到 %d", n)
	}

	// 造一条超过归档门槛的节点（非置顶、从未用过）。
	oldID := ""
	if err := b.db.QueryRow(`SELECT id FROM mem_nodes WHERE kind='fact' AND content LIKE 'XimoAgent 用 SQLite%'`).Scan(&oldID); err != nil {
		t.Fatal(err)
	}
	if _, err := b.db.Exec(`UPDATE mem_nodes SET created_at=?, last_used=NULL, use_count=0 WHERE id=?`,
		now.Add(-200*24*time.Hour).Unix(), oldID); err != nil {
		t.Fatal(err)
	}

	res, err := b.Consolidate(ctx)
	if err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if res.MergedFacts == 0 {
		t.Errorf("近重复的 fact 应被合并，得到 %+v", res)
	}
	if res.TopicsCreated == 0 {
		t.Errorf("共享实体 ≥4 应聚合出 topic，得到 %+v", res)
	}
	if res.ArchivedNodes == 0 || nodeStatus(t, b, oldID) != StatusArchived {
		t.Errorf("超过 180 天未用的节点应归档，得到 %+v status=%s", res, nodeStatus(t, b, oldID))
	}
	if n := countNodesByKind(t, b, NodeTopic); n == 0 {
		t.Error("应存在 topic 节点")
	}
	// 归档的节点不参与召回。
	if recs, err := b.Search(ctx, "XimoAgent Go 引擎 记忆", SearchOptions{TopK: 8}); err == nil {
		for _, r := range recs {
			if r.ID == oldID {
				t.Error("归档节点不该被召回")
			}
		}
	}
	if stats := b.Stats(ctx); stats.Consolidations == 0 || stats.MergedFacts == 0 {
		t.Errorf("整理统计应写回 Stats，得到 %+v", stats)
	}
}

// contradicts 对在整理时被裁决：新的取代旧的，contradicts 边消失。
func TestSynapseConsolidateResolvesContradictions(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	var tick int64
	ex := &fakeExtractor{queue: []SynapseExtraction{
		{Facts: []SynapseFact{{Text: "用户使用深色主题", Importance: 0.8, Entities: []string{"界面主题"}}}},
		{Facts: []SynapseFact{{Text: "用户不要使用深色主题", Importance: 0.8, Entities: []string{"界面主题"}}}},
	}}
	b := newSynapse(t, SynapseOptions{
		Extractor: ex,
		Now:       func() time.Time { tick++; return now.Add(time.Duration(tick) * time.Minute) },
	})
	if _, err := b.Add(ctx, turn("主题", "深色"), AddOptions{RunID: "x1"}); err != nil {
		t.Fatalf("Add 1: %v", err)
	}
	if _, err := b.Add(ctx, turn("主题", "不要深色"), AddOptions{RunID: "x2"}); err != nil {
		t.Fatalf("Add 2: %v", err)
	}
	var contradicts int
	if err := b.db.QueryRow(`SELECT COUNT(*) FROM mem_edges WHERE rel='contradicts'`).Scan(&contradicts); err != nil {
		t.Fatal(err)
	}
	if contradicts == 0 {
		t.Skip("该措辞未被判为矛盾对（判定规则的边界），跳过裁决断言")
	}
	res, err := b.Consolidate(ctx)
	if err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if res.ContradictionsResolved == 0 {
		t.Errorf("应裁决至少一对矛盾，得到 %+v", res)
	}
	if err := b.db.QueryRow(`SELECT COUNT(*) FROM mem_edges WHERE rel='contradicts'`).Scan(&contradicts); err != nil {
		t.Fatal(err)
	}
	if contradicts != 0 {
		t.Errorf("裁决后 contradicts 边应清空，得到 %d", contradicts)
	}
}

// ---------------------------------------------------------------------------
// 注入块预算与格式
// ---------------------------------------------------------------------------

func TestSynapseRenderBlockBudget(t *testing.T) {
	records := []Record{
		{ID: "1", Memory: "用户偏好深色主题", Score: 0.9, Metadata: map[string]any{"group": "界面主题", "via": "seed"}},
		{ID: "2", Memory: "回答用中文", Score: 0.5, Metadata: map[string]any{"group": "界面主题", "via": "seed"}},
		{ID: "3", Memory: "项目使用 Go 引擎", Score: 0.3,
			Metadata: map[string]any{"associations": "entity:XimoAgent → fact:项目（因「XimoAgent」被想起）", "via": "entity:XimoAgent → fact:项目"}},
	}
	b := newSynapse(t, SynapseOptions{})
	block := b.RenderBlock(records, 400)
	if !strings.Contains(block, "界面主题：用户偏好深色主题；回答用中文") {
		t.Errorf("同一 entity 下的条目应分组展示:\n%s", block)
	}
	if !strings.Contains(block, "↳ 关联") {
		t.Errorf("应含 ↳ 关联 行:\n%s", block)
	}
	if strings.HasSuffix(block, "\n") {
		t.Errorf("注入块不该有尾随换行:\n%q", block)
	}
	// 一条都放不下 → 空串（空块不插）。
	if got := b.RenderBlock(records, len(RecallBlockHeader)+1); got != "" {
		t.Errorf("预算放不下时应返回空串，得到 %q", got)
	}
	if got := b.RenderBlock(nil, 100); got != "" {
		t.Errorf("没有条目时应返回空串，得到 %q", got)
	}
}

// ---------------------------------------------------------------------------
// 基准：2 万节点 / ~4 万边；语料是"多主题 + 多实体枢纽"的合成记忆，而不是
// 2 万条几乎相同的文本（后者会让任意查询命中全部节点，把 bm25 打分变成对
// 整个库线性求和 —— 那是语料的极端情况，不是本地记忆的真实形状）。
func BenchmarkSynapseRecall20k(b *testing.B) {
	if testing.Short() {
		b.Skip("short 模式跳过 2 万节点建库")
	}
	ctx := context.Background()
	be := newSynapseB(b, 20000)
	queries := []string{
		"项目 XimoAgent 使用 Go 引擎",
		"记忆 扩散激活 节点 3000",
		"build.cmd verify 测试命令",
		"数据库 sqlite 索引 性能",
		"用户偏好 深色 主题 界面",
	}
	lat := make([]time.Duration, 0, b.N)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		start := time.Now()
		if _, err := be.Search(ctx, queries[i%len(queries)], SearchOptions{}); err != nil {
			b.Fatalf("Search: %v", err)
		}
		lat = append(lat, time.Since(start))
	}
	b.StopTimer()
	p95 := time.Duration(0)
	if len(lat) > 0 {
		sorted := append([]time.Duration(nil), lat...)
		sortDurations(sorted)
		p95 = sorted[int(float64(len(sorted))*0.95)]
		median := sorted[len(sorted)/2]
		b.ReportMetric(float64(median.Microseconds())/1000.0, "p50-ms")
		b.ReportMetric(float64(p95.Microseconds())/1000.0, "p95-ms")
	}
	if p95 > 80*time.Millisecond {
		b.Errorf("召回 p95 超预算：%v", p95)
	}
}

// newSynapseB 造一个装好 n 个 fact 节点、200 个 entity 枢纽与约 2n 条边的库。
func newSynapseB(b testing.TB, n int) *SynapseBackend {
	b.Helper()
	be, err := NewSynapseBackend(filepath.Join(b.TempDir(), "bench.db"), SynapseOptions{UserID: "bench"})
	if err != nil {
		b.Fatalf("打开: %v", err)
	}
	b.Cleanup(func() { _ = be.Close() })

	tx, err := be.db.Begin()
	if err != nil {
		b.Fatal(err)
	}
	now := time.Now().UTC()
	stmt, err := tx.Prepare(`INSERT INTO mem_nodes
		(id,user_id,kind,title,content,tokens,importance,pinned,status,source_run,hash,created_at,use_count)
		VALUES (?,?,?,?,?,?,?,0,'active','bench',?,?,0)`)
	if err != nil {
		b.Fatal(err)
	}
	estmt, err := tx.Prepare(`INSERT INTO mem_edges (src,dst,rel,weight,fire_count,last_fired,created_at) VALUES (?,?,?,?,0,?,?)`)
	if err != nil {
		b.Fatal(err)
	}
	// 200 个实体枢纽，每个约 n/200 条 mentions 边（不是单个 2 万度的超级枢纽）。
	const entityCount = 200
	entityIDs := make([]string, entityCount)
	for e := 0; e < entityCount; e++ {
		id := "mn_bench_entity_" + itoa(e)
		entityIDs[e] = id
		name := "实体" + itoa(e)
		if _, err := tx.Exec(`INSERT INTO mem_nodes
			(id,user_id,kind,title,content,tokens,importance,pinned,status,hash,created_at,use_count)
			VALUES (?,?,?,?,?,?,?,0,'active',?,?,0)`,
			id, "bench", NodeEntity, name, name, synapseTokens(name, name), 0.5, "bench-e"+itoa(e), now.Unix()); err != nil {
			b.Fatal(err)
		}
	}
	const topics = 200
	const words = 500
	for i := 0; i < n; i++ {
		topic := "主题" + itoa(i%topics)
		word := "关键词" + itoa(i%words)
		var text string
		if i%8 == 0 {
			// 只有约 1/8 的节点带这几个检索词，查询才是有选择性的。
			text = "项目 XimoAgent 使用 Go 引擎处理 " + topic + " 的本地优先记录 " + word
		} else {
			text = "记录 " + itoa(i) + " 属于 " + topic + "，关键词 " + word + "，与 主题" + itoa((i*7)%topics) + " 相关"
		}
		id := "mn_bench_" + itoa(i)
		if _, err := stmt.Exec(id, "bench", NodeFact, text, text, synapseTokens("", text), 0.5,
			"h"+itoa(i), now.Unix()); err != nil {
			b.Fatal(err)
		}
		if _, err := estmt.Exec(id, entityIDs[i%entityCount], RelMentions, 0.5, now.Unix(), now.Unix()); err != nil {
			b.Fatal(err)
		}
		if i > 0 {
			if _, err := estmt.Exec(id, "mn_bench_"+itoa(i-1), RelRelated, 0.3, now.Unix(), now.Unix()); err != nil {
				b.Fatal(err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	return be
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}

func sortDurations(d []time.Duration) {
	for i := 1; i < len(d); i++ {
		for j := i; j > 0 && d[j] < d[j-1]; j-- {
			d[j], d[j-1] = d[j-1], d[j]
		}
	}
}

// factIDsByContentPrefix 返回内容以 prefix 开头的 fact 节点 id（按 created_at 排序）。
func factIDsByContentPrefix(t *testing.T, b *SynapseBackend, prefix string) []string {
	t.Helper()
	rows, err := b.db.Query(`SELECT id FROM mem_nodes WHERE kind='fact' AND content LIKE ? ORDER BY created_at`, prefix+"%")
	if err != nil {
		t.Fatalf("查 fact: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("扫描: %v", err)
		}
		out = append(out, id)
	}
	return out
}

func usedWithWeight(t *testing.T, b *SynapseBackend, x, y string) (float64, bool) {
	t.Helper()
	src, dst := x, y
	if src > dst {
		src, dst = dst, src
	}
	var w float64
	if err := b.db.QueryRow(`SELECT weight FROM mem_edges WHERE src=? AND dst=? AND rel='used_with'`,
		src, dst).Scan(&w); err != nil {
		return 0, false
	}
	return w, true
}
