package ctxmgr

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestTokenizerLazyLoadsEmbeddedVocab 确认词表从 embed.FS 懒加载。
//
// 这是与 v1 的核心差异：v1 在模块加载时同步 readFileSync 解析 7.8MB JSON；
// v2 首次 CountTokens 才加载，进程启动路径不背这份成本。
func TestTokenizerLazyLoadsEmbeddedVocab(t *testing.T) {
	tk := NewBPETokenizer(LRUOptions{MaxEntries: 100})

	if tk.Ready() {
		t.Fatal("构造后不应已加载（懒加载）")
	}
	if tk.load() != nil {
		t.Fatalf("加载 embed 词表失败: %v", tk.loadErr)
	}
	if !tk.Ready() {
		t.Fatal("加载后 Ready 应为 true")
	}
}

// TestTokenizerCountsTokens 确认能对中英文计数且结果合理。
func TestTokenizerCountsTokens(t *testing.T) {
	tk := DefaultTokenizer()

	cases := []struct {
		name string
		text string
		min  int
		max  int
	}{
		{"空串", "", 0, 0},
		{"英文单词", "hello world", 1, 4},
		{"中文短语", "你好世界", 2, 12},
		{"代码片段", "func main() { fmt.Println(1) }", 5, 25},
		{"混合", "调用 file_read 读取 a.txt", 4, 30},
		{"长中文", strings.Repeat("上下文压缩与预算管理", 100), 200, 2000},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tk.CountTokens(tc.text)
			if got < tc.min || got > tc.max {
				t.Errorf("CountTokens(%q) = %d, want [%d, %d]", tc.text, got, tc.min, tc.max)
			}
		})
	}
}

// TestTokenizerMonotonic 确认计数随文本增长单调不减（基本正确性）。
func TestTokenizerMonotonic(t *testing.T) {
	tk := DefaultTokenizer()
	base := "这是一段测试文本 for tokenizer validation."
	prev := 0
	for i := 1; i <= 20; i++ {
		text := strings.Repeat(base, i)
		got := tk.CountTokens(text)
		if got < prev {
			t.Fatalf("重复 %d 次时 token 数 %d 少于前次 %d", i, got, prev)
		}
		prev = got
	}
}

// TestTokenizerBPEUsesBoundedCache 确认 BPE 结果进入有界缓存（而非 v1 的无限 map）。
//
// 任务书把 v1 的 `bpeCache` 列为审计重点 —— 本断言确保 v2 的缓存有上限。
func TestTokenizerBPEUsesBoundedCache(t *testing.T) {
	const maxEntries = 64
	tk := NewBPETokenizer(LRUOptions{MaxEntries: maxEntries, MaxBytes: 32 << 10, TTL: time.Minute})

	// 大量不同文本 → 缓存条目数必须被界住。
	for i := 0; i < 5000; i++ {
		tk.CountTokens(strings.Repeat("字", i%50+1) + strings.Repeat("a", i%30+1))
	}

	st := tk.Cache().Stats()
	if st.Entries > maxEntries {
		t.Fatalf("BPE 缓存条目数 %d 超上限 %d（v1 的无限增长问题未修复）", st.Entries, maxEntries)
	}
	if st.Evictions == 0 {
		t.Fatal("插入远超上限的数据后应发生淘汰")
	}
	t.Logf("缓存统计: entries=%d bytes=%d evictions=%d", st.Entries, st.Bytes, st.Evictions)
}

// TestTokenizerCacheReclaimsMemory 确认缓存可被回收（降级阶梯第②步）。
func TestTokenizerCacheReclaimsMemory(t *testing.T) {
	tk := DefaultTokenizer()

	for i := 0; i < 2000; i++ {
		tk.CountTokens(strings.Repeat("词", i%40+1))
	}
	if tk.Cache().Bytes() == 0 {
		t.Skip("缓存未产生占用（可能所有文本都命中已有条目）")
	}

	freed := tk.Cache().Clear()
	if freed <= 0 {
		t.Fatal("Clear 应释放字节")
	}
	if tk.Cache().Stats().Entries != 0 {
		t.Fatal("Clear 后条目应为 0")
	}
}

// TestTokenizerCacheHitConsistency 确认缓存命中与未命中结果一致。
func TestTokenizerCacheHitConsistency(t *testing.T) {
	tk := DefaultTokenizer()
	text := "缓存一致性验证 consistency check 12345"

	first := tk.CountTokens(text)
	tk.Cache().Clear() // 强制未命中
	second := tk.CountTokens(text)

	if first != second {
		t.Fatalf("缓存前后计数不一致: %d vs %d", first, second)
	}
}

// TestCountMessageTokensIncludesRole 确认计入 role 与 reasoning 的开销（v1 同款）。
func TestCountMessageTokensIncludesRole(t *testing.T) {
	tk := DefaultTokenizer()
	msgs := []Message{
		{Role: "user", Content: "你好"},
		{Role: "assistant", Content: "回答", ReasoningContent: "思考过程"},
	}
	total := tk.CountMessageTokens(msgs)
	contentOnly := tk.CountTokens("你好") + tk.CountTokens("回答")

	if total <= contentOnly {
		t.Fatalf("含 role/reasoning 的总数 %d 应大于纯内容 %d", total, contentOnly)
	}
}

// TestTokenizerConcurrentUse 确认并发调用安全（懒加载用 sync.Once 保护）。
func TestTokenizerConcurrentUse(t *testing.T) {
	tk := NewBPETokenizer(DefaultTokenizerLRUOptions())

	var wg sync.WaitGroup
	results := make([]int, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = tk.CountTokens("并发加载与计数验证 concurrent")
		}(i)
	}
	wg.Wait()

	// 所有 goroutine 应得到同一结果（证明懒加载只执行一次）。
	for i := 1; i < len(results); i++ {
		if results[i] != results[0] {
			t.Fatalf("并发结果不一致: results[0]=%d results[%d]=%d", results[0], i, results[i])
		}
	}
}

// TestHeuristicFallbackNoPanic 确认词表不可用时退化为估算而非 panic。
func TestHeuristicFallbackNoPanic(t *testing.T) {
	got := heuristicCount("fallback test")
	if got < 1 {
		t.Fatalf("估算应至少为 1，实际 %d", got)
	}
	if heuristicCount("") != 1 {
		t.Fatal("空串估算应为 1（保守）")
	}
}

// ---------------------------------------------------------------------------
// 前缀形状（缓存命中诊断，v1 最有价值的性能设计）
// ---------------------------------------------------------------------------

// TestNormalizeToolSchemasIsStable 确认工具顺序不影响归一化结果。
//
// 这是 prompt cache 能命中的前提：工具列表顺序抖动会改变 tools JSON 字节。
func TestNormalizeToolSchemasIsStable(t *testing.T) {
	toolsA := []ToolSchema{
		{Name: "zebra", Description: "z", Parameters: map[string]any{"type": "object"}},
		{Name: "alpha", Description: "a", Parameters: map[string]any{"type": "object"}},
		{Name: "mid", Description: "m", Parameters: map[string]any{"type": "object"}},
	}
	// 打乱顺序。
	toolsB := []ToolSchema{toolsA[1], toolsA[2], toolsA[0]}

	na := NormalizeToolSchemas(toolsA)
	nb := NormalizeToolSchemas(toolsB)

	if len(na) != len(nb) {
		t.Fatalf("长度不一致: %d vs %d", len(na), len(nb))
	}
	for i := range na {
		if na[i].Name != nb[i].Name {
			t.Fatalf("归一化后顺序不一致: [%d] %q vs %q", i, na[i].Name, nb[i].Name)
		}
	}

	// 哈希也必须一致。
	sa := CaptureShape("system", toolsA, 1, CompressionVersion)
	sb := CaptureShape("system", toolsB, 1, CompressionVersion)
	if sa.ToolsHash != sb.ToolsHash {
		t.Fatal("不同顺序的工具列表应产生相同 ToolsHash")
	}
	if sa.PrefixHash != sb.PrefixHash {
		t.Fatal("不同顺序的工具列表应产生相同 PrefixHash")
	}
}

// TestCompareShapeDetectsSystemChange 确认 system 变化被归因。
func TestCompareShapeDetectsSystemChange(t *testing.T) {
	prev := CaptureShape("你是助手 A", nil, 1, CompressionVersion)
	cur := CaptureShape("你是助手 B", nil, 1, CompressionVersion)

	d := CompareShape(prev, cur, 0, 0)
	if !d.PrefixChanged {
		t.Fatal("system 变化应被检出")
	}
	if !contains(d.PrefixChangeReasons, "system") {
		t.Fatalf("归因应含 system，实际 %v", d.PrefixChangeReasons)
	}
}

// TestCompareShapeDetectsToolsChange 确认 tools 变化被归因。
func TestCompareShapeDetectsToolsChange(t *testing.T) {
	toolsA := []ToolSchema{{Name: "a", Description: "d", Parameters: map[string]any{}}}
	toolsB := []ToolSchema{{Name: "b", Description: "d", Parameters: map[string]any{}}}

	prev := CaptureShape("sys", toolsA, 1, CompressionVersion)
	cur := CaptureShape("sys", toolsB, 1, CompressionVersion)

	d := CompareShape(prev, cur, 0, 0)
	if !contains(d.PrefixChangeReasons, "tools") {
		t.Fatalf("归因应含 tools，实际 %v", d.PrefixChangeReasons)
	}
}

// TestCompareShapeDetectsRewriteVersion 确认压缩重写版本变化被归因。
func TestCompareShapeDetectsRewriteVersion(t *testing.T) {
	prev := CaptureShape("sys", nil, 1, CompressionVersion)
	cur := CaptureShape("sys", nil, 2, CompressionVersion) // 发生了一次压缩

	d := CompareShape(prev, cur, 0, 0)
	if !contains(d.PrefixChangeReasons, "log_rewrite") {
		t.Fatalf("归因应含 log_rewrite，实际 %v", d.PrefixChangeReasons)
	}
}

// TestCompareShapeDetectsCompressionVersionChange 确认压缩算法版本变化被归因。
//
// 这是 v2 新增的归因维度：改了压缩算法后 cache miss 要能解释清楚。
func TestCompareShapeDetectsCompressionVersionChange(t *testing.T) {
	prev := CaptureShape("sys", nil, 1, "compress-v1.0")
	cur := CaptureShape("sys", nil, 1, "compress-v2.0")

	d := CompareShape(prev, cur, 0, 0)
	if !contains(d.PrefixChangeReasons, "compression_version") {
		t.Fatalf("归因应含 compression_version，实际 %v", d.PrefixChangeReasons)
	}
}

// TestCompareShapeNoChangeWhenStable 确认稳定前缀不误报变化。
func TestCompareShapeNoChangeWhenStable(t *testing.T) {
	tools := []ToolSchema{{Name: "a", Description: "d", Parameters: map[string]any{}}}
	prev := CaptureShape("sys", tools, 3, CompressionVersion)
	cur := CaptureShape("sys", tools, 3, CompressionVersion)

	d := CompareShape(prev, cur, 950, 50)
	if d.PrefixChanged {
		t.Fatalf("前缀未变不应报变化，归因: %v", d.PrefixChangeReasons)
	}
	if d.CacheHitTokens != 950 || d.CacheMissTokens != 50 {
		t.Fatal("usage 应被透传")
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 工具保留优先级（v1 tool-priority.ts 对等）
// ---------------------------------------------------------------------------

// TestToolRetentionMatchesV1 确认三档划分与 v1 一致。
func TestToolRetentionMatchesV1(t *testing.T) {
	cases := map[string]ToolRetention{
		"file_write":       RetentionHigh,
		"file_edit":        RetentionHigh,
		"file_create":      RetentionHigh,
		"file_delete":      RetentionHigh,
		"move_file":        RetentionHigh,
		"terminal_exec":    RetentionMedium,
		"dependency_check": RetentionMedium,
		"code_lint":        RetentionMedium,
		"code_format":      RetentionMedium,
		"code_review":      RetentionMedium,
		"git_operation":    RetentionMedium,
		"file_read":        RetentionLow,
		"web_search":       RetentionLow,
		"unknown_tool":     RetentionLow,
		"":                 RetentionLow,
	}
	for name, want := range cases {
		if got := GetToolRetention(name); got != want {
			t.Errorf("GetToolRetention(%q) = %v, want %v", name, got, want)
		}
	}
}

// TestFindToolNameByCallID 确认通过 tool_call_id 回溯工具名（而非猜内容）。
func TestFindToolNameByCallID(t *testing.T) {
	msgs := []Message{
		{Role: "system", Content: "sys"},
		{Role: "assistant", Content: "",
			ToolCalls: []byte(`[{"id":"c1","type":"function","function":{"name":"file_read","arguments":"{}"}}]`)},
		{Role: "tool", Content: "内容", ToolCallID: "c1"},
	}

	if got := FindToolName(msgs, 2); got != "file_read" {
		t.Fatalf("FindToolName = %q, want file_read", got)
	}
	// 无 tool_call_id 时应返回空，而不是误判。
	if got := FindToolName(msgs, 1); got != "" {
		t.Fatalf("assistant 消息不应有工具名，实际 %q", got)
	}
}

// TestTruncateToolResultMarksTruncation 确认截断带标记（避免双重截断）。
func TestTruncateToolResultMarksTruncation(t *testing.T) {
	cfg := DefaultAgentConfig()
	cfg.MaxToolResultChars = 50

	long := strings.Repeat("内容", 100)
	out := TruncateToolResult(long, cfg)

	if len(out) >= len(long) {
		t.Fatal("超长内容应被截断")
	}
	if !strings.Contains(out, "结果已截断") {
		t.Fatalf("应带截断标记: %q", out)
	}
	if !alreadyTruncated(out) {
		t.Fatal("alreadyTruncated 应能识别截断标记")
	}

	// 短内容不截断。
	short := "短内容"
	if got := TruncateToolResult(short, cfg); got != short {
		t.Fatalf("短内容不应被改动: %q", got)
	}
}

// ---------------------------------------------------------------------------
// 机械压缩（第 22.2 章）
// ---------------------------------------------------------------------------

// TestCompressorSnipsOnlyLowRetention 确认 SNIP 只裁 LOW 优先级结果。
func TestCompressorSnipsOnlyLowRetention(t *testing.T) {
	cfg := DefaultAgentConfig()
	cfg.RecentKeep = 1
	cfg.SnippedKeep = 20
	c := NewCompressor(cfg)

	msgs := []Message{
		{Role: "system", Content: "sys"},
		// LOW：file_read → 应被 snip。
		{Role: "assistant", Content: "",
			ToolCalls: []byte(`[{"id":"c1","function":{"name":"file_read"}}]`)},
		{Role: "tool", Content: strings.Repeat("低价值", 200), ToolCallID: "c1"},
		// HIGH：file_write → 不应被 snip。
		{Role: "assistant", Content: "",
			ToolCalls: []byte(`[{"id":"c2","function":{"name":"file_write"}}]`)},
		{Role: "tool", Content: strings.Repeat("高价值变更", 200), ToolCallID: "c2"},
		{Role: "user", Content: "最近的消息"},
	}

	stats := c.SnipStaleToolResults(msgs)
	if stats.SnippedResults == 0 {
		t.Fatal("应裁掉 LOW 优先级结果")
	}
	if !strings.Contains(msgs[2].Content, "已自动截断") {
		t.Fatal("LOW 结果应被截断")
	}
	if strings.Contains(msgs[4].Content, "已自动截断") {
		t.Fatalf("HIGH 结果不应在 snip 阶段被裁: %q", msgs[4].Content)
	}
}

// TestCompressorProtectsRecentMessages 确认最近的 N 条不被裁剪。
func TestCompressorProtectsRecentMessages(t *testing.T) {
	cfg := DefaultAgentConfig()
	cfg.RecentKeep = 3
	cfg.SnippedKeep = 10
	c := NewCompressor(cfg)

	var msgs []Message
	msgs = append(msgs, Message{Role: "system", Content: "sys"})
	for i := 0; i < 6; i++ {
		id := "c" + string(rune('a'+i))
		msgs = append(msgs,
			Message{Role: "assistant", Content: "",
				ToolCalls: []byte(`[{"id":"` + id + `","function":{"name":"file_read"}}]`)},
			Message{Role: "tool", Content: strings.Repeat("内容", 100), ToolCallID: id},
		)
	}

	c.SnipStaleToolResults(msgs)

	// 最后 3 条（下标 len-3 起）受保护，不应被截断。
	protectFrom := len(msgs) - cfg.RecentKeep
	for i := protectFrom; i < len(msgs); i++ {
		if strings.Contains(msgs[i].Content, "已自动截断") {
			t.Fatalf("受保护的消息 %d 不应被截断", i)
		}
	}
}

// TestCompressorAvoidsDoubleTruncation 确认已截断内容不会被再次截断。
func TestCompressorAvoidsDoubleTruncation(t *testing.T) {
	cfg := DefaultAgentConfig()
	cfg.RecentKeep = 1
	cfg.SnippedKeep = 20
	c := NewCompressor(cfg)

	msgs := []Message{
		{Role: "system", Content: "sys"},
		{Role: "assistant", Content: "",
			ToolCalls: []byte(`[{"id":"c1","function":{"name":"file_read"}}]`)},
		{Role: "tool", Content: strings.Repeat("内容", 200), ToolCallID: "c1"},
		{Role: "user", Content: "最近"},
	}

	first := c.SnipStaleToolResults(msgs)
	if first.SnippedResults != 1 {
		t.Fatalf("首次应裁 1 条，实际 %d", first.SnippedResults)
	}
	second := c.SnipStaleToolResults(msgs)
	if second.SnippedResults != 0 {
		t.Fatal("已截断的内容不应被重复裁剪")
	}
}

// TestCompressorTrimContextPreservesToolPairing 是本文件最重要的断言：
// 压缩绝不能破坏 assistant(tool_calls) ↔ tool(tool_call_id) 的配对，
// 否则下一轮 API 调用会 400。
func TestCompressorTrimContextPreservesToolPairing(t *testing.T) {
	cfg := DefaultAgentConfig()
	cfg.RecentKeep = 2
	cfg.SnippedKeep = 50
	cfg.PrunedKeep = 20
	c := NewCompressor(cfg)

	msgs := buildConversation(t, 30)
	budget := BudgetOrChars{MaxChars: 2000} // 极低阈值 → 触发全部三级裁剪

	c.TrimContext(msgs, budget)

	// 用 CompactedContext 的校验器检查配对完整性。
	cc := CompactedContext{Messages: msgs, Versions: CurrentVersions()}
	if err := validateToolPairing(cc.Messages); err != nil {
		t.Fatalf("压缩破坏了 tool 配对: %v", err)
	}
}

// TestValidateToolPairingDetectsBreak 确认配对校验器本身有效（防止校验形同虚设）。
func TestValidateToolPairingDetectsBreak(t *testing.T) {
	broken := []Message{
		{Role: "system", Content: "sys"},
		{Role: "tool", Content: "孤儿工具结果", ToolCallID: "nonexistent"},
	}
	if err := validateToolPairing(broken); err == nil {
		t.Fatal("孤立的 tool 消息应被检出")
	}

	missingID := []Message{
		{Role: "system", Content: "sys"},
		{Role: "tool", Content: "缺 ID"},
	}
	if err := validateToolPairing(missingID); err == nil {
		t.Fatal("缺少 tool_call_id 应被检出")
	}
}

// ---------------------------------------------------------------------------
// ContextManager 四档决策与 stuck 保护
// ---------------------------------------------------------------------------

// TestManagerSoftTierNoticesOnce 确认 soft 档只通知一次（不反复抖动）。
func TestManagerSoftTierNoticesOnce(t *testing.T) {
	m := newTestManager(t)
	// 小窗口让阈值容易命中：window=10000。
	m.SetProviderWindow(10_000)

	budget := m.Budget(contextTODO(), 10_000)
	base := float64(budget.InputBudget()) * 0.8
	softTokens := int(base * softFactor)

	msgs := buildConversation(t, 5)

	first, err := m.CompactWithWindow(contextTODO(), SessionSnapshot{
		Messages: msgs, PromptTokens: softTokens,
	}, 10_000)
	if err != nil {
		t.Fatalf("报错: %v", err)
	}
	if first.Stats.Tier != TierSoft {
		t.Fatalf("首次应为 soft，实际 %v", first.Stats.Tier)
	}

	second, err := m.CompactWithWindow(contextTODO(), SessionSnapshot{
		Messages: msgs, PromptTokens: softTokens,
	}, 10_000)
	if err != nil {
		t.Fatalf("报错: %v", err)
	}
	if second.Stats.Tier == TierSoft {
		t.Fatal("soft 通知每次接近只应发生一次")
	}
}

// TestManagerStuckProtectionPausesAfterTwoCompacts 确认连续两次 compact 后进入 stuck 暂停。
//
// 这是 v1 compactStuck 机制的对等实现，防止「压缩→仍超限→再压缩」的抖动循环。
func TestManagerStuckProtectionPausesAfterTwoCompacts(t *testing.T) {
	m := newTestManager(t)
	m.SetProviderWindow(10_000)

	// 用远高于触发线的 promptTokens 连续触发。
	const highTokens = 9_000_000 // 超过 force 线
	msgs := buildConversation(t, 5)

	var sawCompact bool
	for i := 0; i < 4; i++ {
		out, err := m.CompactWithWindow(contextTODO(), SessionSnapshot{
			Messages: msgs, PromptTokens: highTokens,
		}, 10_000)
		if err != nil {
			t.Fatalf("第 %d 次报错: %v", i, err)
		}
		if out.Stats.Tier == TierCompact || out.Stats.Tier == TierForce {
			sawCompact = true
		}
		if i >= 2 && !out.Stats.StuckPaused {
			t.Fatalf("第 %d 次应已进入 stuck 暂停", i+1)
		}
	}
	if !sawCompact {
		t.Fatal("应至少触发过一次 compact/force")
	}
	if !m.StuckPaused() {
		t.Fatal("最终应处于 stuck 暂停状态")
	}
}

// TestManagerResetClearsStuck 确认切换会话时状态被重置。
func TestManagerResetClearsStuck(t *testing.T) {
	m := newTestManager(t)
	m.SetProviderWindow(10_000)

	msgs := buildConversation(t, 5)
	for i := 0; i < 3; i++ {
		_, _ = m.CompactWithWindow(contextTODO(), SessionSnapshot{
			Messages: msgs, PromptTokens: 9_000_000,
		}, 10_000)
	}
	if !m.StuckPaused() {
		t.Fatal("应先进入 stuck")
	}

	m.Reset()
	if m.StuckPaused() {
		t.Fatal("Reset 后应清除 stuck")
	}
	if m.RewriteVersion() != 0 {
		t.Fatalf("Reset 后 rewriteVersion 应为 0，实际 %d", m.RewriteVersion())
	}
}

// TestManagerCompactDoesNotMutateInput 确认压缩作用在副本上（不污染调用方会话状态）。
func TestManagerCompactDoesNotMutateInput(t *testing.T) {
	m := newTestManager(t)
	m.SetProviderWindow(10_000)

	msgs := buildConversation(t, 20)
	original := make([]string, len(msgs))
	for i, msg := range msgs {
		original[i] = msg.Content
	}

	_, err := m.CompactWithWindow(contextTODO(), SessionSnapshot{
		Messages: msgs, PromptTokens: 9_000_000,
	}, 10_000)
	if err != nil {
		t.Fatalf("报错: %v", err)
	}

	for i, msg := range msgs {
		if msg.Content != original[i] {
			t.Fatalf("第 %d 条消息被原地修改（压缩应作用在副本上）", i)
		}
	}
}

// TestManagerRewriteVersionIncrementsOnCompaction 确认每次压缩递增重写版本。
func TestManagerRewriteVersionIncrementsOnCompaction(t *testing.T) {
	m := newTestManager(t)
	m.SetProviderWindow(10_000)

	if m.RewriteVersion() != 0 {
		t.Fatal("初始应为 0")
	}

	_, err := m.CompactWithWindow(contextTODO(), SessionSnapshot{
		Messages: buildConversation(t, 5), PromptTokens: 9_000_000,
	}, 10_000)
	if err != nil {
		t.Fatalf("报错: %v", err)
	}
	if m.RewriteVersion() == 0 {
		t.Fatal("触发压缩后 rewriteVersion 应递增")
	}
}

// TestManagerCompactRespectsContextCancel 确认 ctx 取消能立即返回。
func TestManagerCompactRespectsContextCancel(t *testing.T) {
	m := newTestManager(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := m.Compact(ctx, SessionSnapshot{Messages: buildConversation(t, 3)}); err == nil {
		t.Fatal("已取消的 ctx 应返回错误")
	}
}

// TestValidatorCatchesTokenGrowth 确认「压缩后 token 反而增加」被检出。
func TestValidatorCatchesTokenGrowth(t *testing.T) {
	cc := CompactedContext{
		Messages:     buildConversation(t, 3),
		Versions:     CurrentVersions(),
		Stats:        CompactionStats{Tier: TierSnip},
		TokensBefore: 100,
		TokensAfter:  200, // 不合理
	}
	if err := cc.Validate(); err == nil {
		t.Fatal("压缩后 token 增加应被检出")
	}
}
