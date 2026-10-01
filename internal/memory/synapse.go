package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// 本文件是 Synapse Memory（审核文档第 4 章）的类型、常量与纯工具函数。
//
// 设计原则（文档 4.2）：图而不是表；检索 = 种子 + 沿带权边扩散激活；会学习
// （赫布）、会遗忘（惰性衰减）、会整理（Consolidate）；可解释（每条召回都能
// 说明"为什么想起它"）；本地优先（纯 SQLite，FTS5 + 图，无强依赖）。
//
// 与既有后端的关系（4.10）：SynapseBackend 只是 Backend 接口的第三个实现，
// EmbeddedBackend 与 mem0 后端保持可用、行为不变；未启用时请求与没有本特性
// 逐字节一致。本文件不改动任何既有实现的对外行为。

// ---------------------------------------------------------------------------
// 节点与边的字面量（文档 4.3；这些值进 CHECK 约束，是稳定的存储契约）
// ---------------------------------------------------------------------------

// 节点类型（mem_nodes.kind）。
const (
	// NodeFact 原子事实 / 偏好 / 结论。
	NodeFact = "fact"
	// NodeEntity 人、项目、工具、文件、概念的规范名，是把事实串起来的枢纽。
	NodeEntity = "entity"
	// NodeEpisode 一次任务的摘要，fact 通过 derived_from 指向它。
	NodeEpisode = "episode"
	// NodeProcedure 可复用的做法 / 步骤。
	NodeProcedure = "procedure"
	// NodeTopic 由整理任务聚合出的主题簇。
	NodeTopic = "topic"
)

// 边类型（mem_edges.rel）。
const (
	// RelMentions 事实 → 实体；初始权重 0.5，是"知识串联"的核心。
	RelMentions = "mentions"
	// RelRelated 同一次抽取里共现的事实；初始权重 0.2。
	RelRelated = "related"
	// RelPartOf 成员 → 主题簇。
	RelPartOf = "part_of"
	// RelCauses 因果。
	RelCauses = "causes"
	// RelDerivedFrom 事实 → 来源 episode。
	RelDerivedFrom = "derived_from"
	// RelSupersedes 新 → 旧，表示旧事实已被取代（不传播激活）。
	RelSupersedes = "supersedes"
	// RelContradicts 无法自动裁决的矛盾对（不传播激活）。
	RelContradicts = "contradicts"
	// RelSameTopic 同主题。
	RelSameTopic = "same_topic"
	// RelUsedWith 赫布学习出的"一起被使用"的关联边。
	RelUsedWith = "used_with"
)

// 节点状态（mem_nodes.status）。superseded / archived 不参与召回。
const (
	// StatusActive 活跃节点。
	StatusActive = "active"
	// StatusSuperseded 已被更新的事实取代。
	StatusSuperseded = "superseded"
	// StatusArchived 长期无用被归档（不删除，可恢复）。
	StatusArchived = "archived"
)

// ---------------------------------------------------------------------------
// 默认参数（文档 4.5 / 4.11 给出的建议值）
// ---------------------------------------------------------------------------

const (
	// DefaultSynapseTopK 单次召回条数上限。文档建议从 5 提到 8：现在每条更
	// 精炼且带关联。
	DefaultSynapseTopK = 8
	// DefaultSynapseRecallMaxChars 注入块字符上限。文档建议 1800。
	DefaultSynapseRecallMaxChars = 1800
	// DefaultSynapseSeedLimit 种子数量上限（FTS5 bm25 前 20）。
	DefaultSynapseSeedLimit = 20
	// DefaultSynapseFanOut 每个节点每跳最多取多少条出边（按有效权重降序）。
	DefaultSynapseFanOut = 8
	// DefaultSynapseHops 扩散跳数。
	DefaultSynapseHops = 2
	// DefaultSynapseSpreadThreshold 激活低于它就不再向外扩散。
	DefaultSynapseSpreadThreshold = 0.05
	// DefaultSynapseMaxFrontier 每跳参与扩散的前沿节点数上限（按激活降序取）。
	// 文档给的是"对所有前沿节点扩散"，但每跳的工作量 = 前沿节点数 × 一次边查询
	// （2 万节点基准实测约 0.9ms/节点，占召回耗时的大头）。第 1 跳的前沿就是
	// 种子（≤ SeedLimit=20，不动它，那是命中最强的一批）；这里限制的是第 2 跳
	// 从多少个第 1 跳节点继续扩散。
	//
	// 取 16 的依据：第 1 跳的激活在 0.1-0.25 量级，再乘 fanOut 边的有效权重
	// (0.2-0.5) 与 hopDecay 0.35，第 2 跳的增益只有 0.01-0.04，大多低于
	// SpreadThreshold=0.05（即它们本来就不会再往下传）。保留最强的 16 个已经
	// 能把"关联事实"带出来，代价是把每跳的边查询数钉在可预算的范围内。
	DefaultSynapseMaxFrontier = 16
	// DefaultSynapseMaxFacts 单次抽取最多接受的 fact 条数。
	DefaultSynapseMaxFacts = 8
	// DefaultSynapseMaterialBytes 采集材料的总长度硬上限。
	DefaultSynapseMaterialBytes = 6 * 1024
	// DefaultSynapseExtractTimeout 单次抽取调用的超时（文档：10s）。
	DefaultSynapseExtractTimeout = 10 * time.Second
	// DefaultSynapseConsolidateTimeout 睡眠整理的时限（文档：20s）。
	DefaultSynapseConsolidateTimeout = 20 * time.Second
	// DefaultSynapseArchiveDays 归档门槛：超过这么多天没用过就归档。
	DefaultSynapseArchiveDays = 180
	// DefaultSynapseRecallLogRetention 召回日志保留天数，整理时顺带清理。
	DefaultSynapseRecallLogRetention = 30 * 24 * time.Hour
	// DefaultSynapseQueueDepth 后台任务（召回日志、整理分片）的队列深度。
	// 满则丢弃并计数——沿用 BackfillDropped 的语义：丢一条日志可以，阻塞
	// run 收尾不行。
	DefaultSynapseQueueDepth = 64
	// DefaultSynapseBackgroundFlushWait 写入流水线开始前等待后台召回日志落盘的
	// 上界（赫布结算依赖它）。
	DefaultSynapseBackgroundFlushWait = 250 * time.Millisecond
	// DefaultSynapseConsolidateBatch 整理的批大小（可中断的粒度）。
	DefaultSynapseConsolidateBatch = 200
)

// 记忆动力学的数值参数（文档 4.5 / 4.6 / 4.7）。
const (
	// SynapseHebbianEta 赫布学习的步长：weight += η·(1-weight)，向 1 饱和。
	SynapseHebbianEta = 0.15
	// SynapsePenaltyFactor 被想起却未被采用的边权惩罚系数。
	SynapsePenaltyFactor = 0.97
	// SynapseUsedWithInit used_with 边首次创建时的权重。
	SynapseUsedWithInit = 0.2
	// SynapseWeightMentions mentions 边初始权重。
	SynapseWeightMentions = 0.5
	// SynapseWeightRelated related 边初始权重（同次抽取共现）。
	SynapseWeightRelated = 0.2
	// SynapseWeightRelation 模型给出的 relations 边初始权重。
	SynapseWeightRelation = 0.4
	// SynapseWeightDerivedFrom derived_from 边初始权重。文档只给了 mentions /
	// related / relations 三个初始值，derived_from 未点名，这里采用 mem_edges
	// 的列默认值 0.3 作为它的初始权重。
	SynapseWeightDerivedFrom = 0.3
	// SynapseHalfLifeUsedWithDays used_with 边的半衰期。
	SynapseHalfLifeUsedWithDays = 45.0
	// SynapseHalfLifeRelatedDays related 边的半衰期。
	SynapseHalfLifeRelatedDays = 90.0
	// SynapsePinnedBoost 置顶节点在打分时的直接加成。
	SynapsePinnedBoost = 0.3
	// SynapseSeedActivation entity 命中时的初始激活。
	SynapseSeedActivation = 0.8
	// SynapseHopDecay1 / SynapseHopDecay2 第 1 / 2 跳的扩散衰减。
	SynapseHopDecay1 = 0.6
	SynapseHopDecay2 = 0.35
	// SynapseMinEdgeWeight 整理时删除的边权门槛（且 use/fire 计数为 0）。
	SynapseMinEdgeWeight = 0.03
	// SynapseAdoptionOverlap 采用判定的关键词重叠率阈值。
	SynapseAdoptionOverlap = 0.34
	// SynapseSimilarJaccard 无嵌入时的去重合并阈值（词法 Jaccard）。
	SynapseSimilarJaccard = 0.8
	// SynapseTopicClusterMin 主题簇的最小规模，达到才生成 topic 节点。
	SynapseTopicClusterMin = 4
	// SynapseStrongSignalBonus 强信号词（偏好/总是/不要/记住…）给规则分的加成。
	SynapseStrongSignalBonus = 0.2
	// SynapseDefaultImportance 新节点的默认重要度。
	SynapseDefaultImportance = 0.5
)

// ---------------------------------------------------------------------------
// 注入给提取器的类型（文档 4.4 第 3 条的严格 JSON 形状）
// ---------------------------------------------------------------------------

// SynapseExtraction 是一次抽取的完整结果，形状与文档 4.4 第 3 条给出的 JSON
// 一一对应（JSON tag 即协议字段名，不得改动）。
type SynapseExtraction struct {
	Episode    *SynapseEpisode    `json:"episode,omitempty"`
	Facts      []SynapseFact      `json:"facts,omitempty"`
	Procedures []SynapseProcedure `json:"procedures,omitempty"`
	Relations  []SynapseRelation  `json:"relations,omitempty"`
}

// SynapseEpisode 是本次任务的一句话摘要。
type SynapseEpisode struct {
	Title   string `json:"title,omitempty"`
	Summary string `json:"summary,omitempty"`
}

// SynapseFact 是一条原子事实。
type SynapseFact struct {
	Text       string   `json:"text"`
	Importance float64  `json:"importance"`
	Entities   []string `json:"entities,omitempty"`
}

// SynapseProcedure 是一条可复用的做法。
type SynapseProcedure struct {
	Title string `json:"title,omitempty"`
	Steps string `json:"steps,omitempty"`
}

// SynapseRelation 是模型给出的节点间关系（rel 只取 causes|related|part_of）。
type SynapseRelation struct {
	From string `json:"from"`
	To   string `json:"to"`
	Rel  string `json:"rel"`
}

// Extractor 是抽取器接口。生产装配（把 provider 适配成它）留待后续阶段，
// 本包只定义契约并消费它；未注入时走规则抽取。
//
// Extract 收到的 prompt 已经过 types.RedactString（脱敏在调用方完成，见
// synapse_extract.go），实现方不得假设里面可能有密钥。
type Extractor interface {
	Extract(ctx context.Context, prompt string) (SynapseExtraction, error)
}

// Embedder 是可选的向量能力。未注入时功能完整，只是同义改写命中率较低
// （文档 4.11：这是有意取舍）。
type Embedder interface {
	// Embed 返回与 texts 等长的向量（float32），顺序一一对应。
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// ---------------------------------------------------------------------------
// 选项
// ---------------------------------------------------------------------------

// SynapseOptions 是 SynapseBackend 的构造选项。零值即一套可用默认。
type SynapseOptions struct {
	// UserID 记忆归属；必填（空则用 DefaultUserID）。
	UserID string
	// AgentID 写入者标识，仅作诊断与元数据。
	AgentID string
	// TopK / RecallMaxChars 召回预算。
	TopK           int
	RecallMaxChars int
	// SeedLimit / FanOut / Hops / SpreadThreshold 扩散激活参数。
	SeedLimit       int
	FanOut          int
	Hops            int
	SpreadThreshold float64
	// MaxFrontier 每跳参与扩散的前沿节点数上限（0 用默认 64；负数表示不限）。
	MaxFrontier int
	// MaxFactsPerExtraction 单次抽取最多接受的 fact 数。
	MaxFactsPerExtraction int
	// MaterialBytes 采集材料长度上限。
	MaterialBytes int
	// ExtractTimeout / ConsolidateTimeout 超时。
	ExtractTimeout     time.Duration
	ConsolidateTimeout time.Duration
	// ArchiveAfter 归档门槛时长。
	ArchiveAfter time.Duration
	// RecallLogRetention 召回日志保留时长。
	RecallLogRetention time.Duration
	// QueueDepth 后台队列深度。
	QueueDepth int
	// BackgroundFlushWait 写入流水线等待后台召回日志落盘的上界。
	BackgroundFlushWait time.Duration
	// Extractor 抽取器；nil 表示规则抽取。
	Extractor Extractor
	// Embedder 可选嵌入；nil 表示纯词法（默认）。
	Embedder Embedder
	// Ledger 可选幂等闸。非 nil 时写入前 Claim(ExtractionKey(runID))，
	// 已认领则跳过（沿用 tool_idempotency 的 Claim 语义）。经 Service 调用
	// 时不要注入它——Service 已经在 extract() 里认领过一次。
	Ledger Gate
	// Now 可注入时钟（测试用）。nil 表示 time.Now。
	Now func() time.Time
}

// withDefaults 补齐零值。
func (o SynapseOptions) withDefaults() SynapseOptions {
	if strings.TrimSpace(o.UserID) == "" {
		o.UserID = DefaultUserID
	}
	if o.TopK <= 0 {
		o.TopK = DefaultSynapseTopK
	}
	if o.RecallMaxChars <= 0 {
		o.RecallMaxChars = DefaultSynapseRecallMaxChars
	}
	if o.SeedLimit <= 0 {
		o.SeedLimit = DefaultSynapseSeedLimit
	}
	if o.FanOut <= 0 {
		o.FanOut = DefaultSynapseFanOut
	}
	if o.Hops <= 0 {
		o.Hops = DefaultSynapseHops
	}
	if o.SpreadThreshold <= 0 {
		o.SpreadThreshold = DefaultSynapseSpreadThreshold
	}
	if o.MaxFrontier == 0 {
		o.MaxFrontier = DefaultSynapseMaxFrontier
	}
	if o.MaxFactsPerExtraction <= 0 {
		o.MaxFactsPerExtraction = DefaultSynapseMaxFacts
	}
	if o.MaterialBytes <= 0 {
		o.MaterialBytes = DefaultSynapseMaterialBytes
	}
	if o.ExtractTimeout <= 0 {
		o.ExtractTimeout = DefaultSynapseExtractTimeout
	}
	if o.ConsolidateTimeout <= 0 {
		o.ConsolidateTimeout = DefaultSynapseConsolidateTimeout
	}
	if o.ArchiveAfter <= 0 {
		o.ArchiveAfter = DefaultSynapseArchiveDays * 24 * time.Hour
	}
	if o.RecallLogRetention <= 0 {
		o.RecallLogRetention = DefaultSynapseRecallLogRetention
	}
	if o.QueueDepth <= 0 {
		o.QueueDepth = DefaultSynapseQueueDepth
	}
	if o.BackgroundFlushWait <= 0 {
		o.BackgroundFlushWait = DefaultSynapseBackgroundFlushWait
	}
	return o
}

// ---------------------------------------------------------------------------
// 存储侧结构
// ---------------------------------------------------------------------------

// synapseNode 是 mem_nodes 的一行。
type synapseNode struct {
	ID         string
	UserID     string
	Kind       string
	Title      string
	Content    string
	Tokens     string
	Importance float64
	Pinned     bool
	Status     string
	SourceRun  string
	Hash       string
	Embedding  []float32
	EmbModel   string
	CreatedAt  time.Time
	LastUsed   time.Time // 零值表示从未被采用
	UseCount   int
}

// label 返回用于"为什么想起它"路径展示的短标签，如 "entity:Go"。
func (n synapseNode) label() string { return n.Kind + ":" + nodeDisplayName(n) }

func nodeDisplayName(n synapseNode) string {
	if s := strings.TrimSpace(n.Title); s != "" {
		return s
	}
	return truncateRunes(strings.Join(strings.Fields(n.Content), " "), 40)
}

// synapseEdge 是 mem_edges 的一行（有向）。
type synapseEdge struct {
	Src       string
	Dst       string
	Rel       string
	Weight    float64
	FireCount int
	LastFired time.Time // 零值表示从未 fire
	CreatedAt time.Time
}

// other 返回边的另一端（激活沿边双向传播：边有方向，但联想是相互的）。
func (e synapseEdge) other(id string) string {
	if e.Src == id {
		return e.Dst
	}
	return e.Src
}

// lastFiredOrCreated 是惰性衰减的起算点。
func (e synapseEdge) lastFiredOrCreated() time.Time {
	if !e.LastFired.IsZero() {
		return e.LastFired
	}
	return e.CreatedAt
}

// ---------------------------------------------------------------------------
// 纯工具函数
// ---------------------------------------------------------------------------

// synapseTokens 复用 lexical.go 的 tokenize（含 CJK 二元组），空格拼接后
// 存进 mem_nodes.tokens 供 FTS5 使用。
func synapseTokens(title, content string) string {
	toks := tokenize(strings.TrimSpace(title + " " + content))
	return strings.Join(toks, " ")
}

// normalizeContent 是归一化去重用的规范化：小写、去掉所有空白与标点，只留
// 字母与数字。文档 4.4 第 4 条。
func normalizeContent(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// contentHash 返回归一化内容的哈希（16 位十六进制）。
func contentHash(s string) string { return hashString(normalizeContent(s)) }

// hashString 是 SHA-256 前 8 字节的十六进制，与 embedded.go 的 hashText 同源。
func hashString(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

// entityKey 把实体名规范化成匹配键：小写、压缩空白、去掉少量通用后缀
// （"项目/模块/仓库/工具"），再查别名表。
func entityKey(name string) string {
	s := strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(name)), " "))
	s = strings.TrimSuffix(s, "的")
	for _, suf := range []string{"项目", "模块", "仓库", "工具"} {
		s = strings.TrimSuffix(s, suf)
	}
	if alias, ok := synapseEntityAliases[s]; ok {
		return alias
	}
	return s
}

// entityTitle 给出 entity 节点的展示名：别名表优先，否则保留输入的大小写
// 并按空白压缩（"Go" 不该被小写成 "go"）。
func entityTitle(name string) string {
	trimmed := strings.Join(strings.Fields(strings.TrimSpace(name)), " ")
	if alias, ok := synapseEntityAliases[strings.ToLower(trimmed)]; ok {
		return alias
	}
	if alias, ok := synapseEntityAliases[entityKey(trimmed)]; ok {
		return alias
	}
	return trimmed
}

// synapseEntityAliases 是最小别名表（文档 4.4 第 5 条提到"别名表"）。
// 键是 entityKey 的结果（小写、去后缀后）。
var synapseEntityAliases = map[string]string{
	"go":               "Go",
	"golang":           "Go",
	"go语言":             "Go",
	"ximo agent":       "XimoAgent",
	"ximo-agent":       "XimoAgent",
	"ximoagent":        "XimoAgent",
	"deepseekharness":  "DeepSeekHarness",
	"deepseek harness": "DeepSeekHarness",
	"deepseek-harness": "DeepSeekHarness",
	"sqlite3":          "SQLite",
	"electron前端":       "Electron",
}

// newSynapseID 生成带前缀的节点 / 事件 ID。与 embedded.go 的 ID 生成同源：
// 进程内严格递增，避免 Windows 粗时钟导致的纳秒重复。
func newSynapseID(prefix string) string {
	embeddedIDMu.Lock()
	embeddedIDSeq++
	seq := embeddedIDSeq
	embeddedIDMu.Unlock()
	sum := sha256.Sum256(fmt.Appendf(nil, "syn/%d/%d/%s", time.Now().UnixNano(), seq, prefix))
	return prefix + hex.EncodeToString(sum[:12])
}

// truncateRunes 按字符（不是字节）截断，避免把多字节字符切坏。
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// redactAndDropSecrets 是写入流水线的第 2 步：全文过 types.RedactString；
// 含疑似密钥 / 令牌 / 口令的**片段整段丢弃**（不是只把密钥打码），这样
// 抽取提示与库里都不会留下那半句话。
//
// 为什么按行切：材料天然是"用户：…\n助手：…"这样的行结构，丢弃一行即丢掉
// 一个含密钥的片段，同时保留其余可用信息。
func redactAndDropSecrets(text string) (clean string, dropped int) {
	if text == "" {
		return "", 0
	}
	lines := strings.Split(text, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if redacted := types.RedactString(line); redacted != line {
			dropped++
			continue
		}
		if looksLikeSecret(line) {
			dropped++
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n"), dropped
}

// looksLikeSecret 是 RedactString 之外的第二道闸：兜住没带标记词的裸令牌
// （如单独一行的 sk-xxxx、AKIA…）。宁可多丢一行材料，也不能把凭据写进库。
func looksLikeSecret(line string) bool {
	lower := strings.ToLower(line)
	for _, marker := range []string{"sk-", "sk_", "xoxb-", "ghp_", "akia", "-----begin"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// dedupeStrings 去重并保序，同时丢掉空白项。
func dedupeStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
