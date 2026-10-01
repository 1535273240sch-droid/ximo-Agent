package memory

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	_ "modernc.org/sqlite"
)

// 本文件是 Synapse Memory 的存储层：开库、建表（文档 4.3 原样的表名 / 列名）、
// 节点与边的读写、以及一个有界的后台任务队列。
//
// 沿用 embedded.go 的既有设定：独立库文件、WAL、synchronous=NORMAL、
// busy_timeout=5000、单写者。差别只在于这里显式把连接池钉成 1：Synapse 的
// 写入路径（抽取落库 + 赫布更新 + 整理）都是写事务，pool>1 只会制造
// SQLITE_BUSY 的概率，不会带来吞吐（召回是纯读，毫秒级）。

// SynapseBackend 是"神经网络式"记忆后端：带权图 + FTS5 词法种子 + 扩散激活
// + 赫布学习 + 惰性衰减 + 后台整理。它实现 Backend 接口。
type SynapseBackend struct {
	opts   SynapseOptions
	db     *sql.DB
	dbPath string

	mu        sync.Mutex
	lastErr   string
	lastErrAt time.Time

	recallCalls   atomic.Uint64
	recallErrors  atomic.Uint64
	recallChars   atomic.Uint64
	bgDropped     atomic.Uint64
	consolidates  atomic.Uint64
	mergedFacts   atomic.Uint64
	archivedNodes atomic.Uint64
	topicsCreated atomic.Uint64

	bg     chan func()
	bgStop chan struct{}
	bgOnce sync.Once
	bgWG   sync.WaitGroup

	// 召回热路径上的两条预编译语句（扩散激活每个前沿节点都要查一次）。它们
	// 只在 *sql.DB 上用：事务路径不调 edgesFor。预编译把单次查询从 ~150µs
	// 降到 ~60µs，2 万节点基准上是决定 p95 是否达标的那几毫秒。
	edgeSrcStmt *sql.Stmt
	edgeDstStmt *sql.Stmt

	closeOnce sync.Once
}

var _ Backend = (*SynapseBackend)(nil)

// NewSynapseBackend 打开（必要时创建）Synapse 记忆库。
func NewSynapseBackend(dbPath string, opts SynapseOptions) (*SynapseBackend, error) {
	if strings.TrimSpace(dbPath) == "" {
		return nil, errors.New("memory: synapse 后端需要库文件路径")
	}
	opts = opts.withDefaults()
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		return nil, fmt.Errorf("memory: 创建数据目录失败: %w", err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("memory: 打开 synapse 记忆库失败: %w", err)
	}
	// 单写者：所有写事务串行化，busy_timeout 兜底跨进程竞争。
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	pragmas := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA busy_timeout=5000",
		// mem_edges 的 ON DELETE CASCADE 需要它；文档 4.8 的 forget 是硬删除，
		// 必须带走节点上的全部边。
		"PRAGMA foreign_keys=ON",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("memory: 设置 %q 失败: %w", p, err)
		}
	}
	if err := initSynapseSchema(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	b := &SynapseBackend{
		opts:   opts,
		db:     db,
		dbPath: dbPath,
		bg:     make(chan func(), opts.QueueDepth),
		bgStop: make(chan struct{}),
	}
	if err := b.prepareEdgeStatements(); err != nil {
		_ = db.Close()
		return nil, err
	}
	b.bgWG.Add(1)
	go b.runBackground()
	return b, nil
}

// synapseEdgeCols 是读边时需要的列（含两端的 pinned，用于免衰减判断）。
const synapseEdgeCols = `e.src, e.dst, e.rel, e.weight, e.fire_count,
       COALESCE(e.last_fired,0), e.created_at, ns.pinned, nd.pinned`

// prepareEdgeStatements 预编译两个方向的"取前 N 条最重边"语句。
func (b *SynapseBackend) prepareEdgeStatements() error {
	mk := func(dir string) string {
		return `SELECT ` + synapseEdgeCols + `
FROM mem_edges e
JOIN mem_nodes ns ON ns.id = e.src
JOIN mem_nodes nd ON nd.id = e.dst
WHERE e.` + dir + `=?
ORDER BY e.weight DESC
LIMIT ?`
	}
	var err error
	if b.edgeSrcStmt, err = b.db.Prepare(mk("src")); err != nil {
		return fmt.Errorf("memory: 预编译边的 src 查询失败: %w", err)
	}
	if b.edgeDstStmt, err = b.db.Prepare(mk("dst")); err != nil {
		return fmt.Errorf("memory: 预编译边的 dst 查询失败: %w", err)
	}
	return nil
}

// initSynapseSchema 建表。表名与列名逐字取自审核文档 4.3，不得改名：
// mem_nodes / mem_fts / mem_edges / mem_recall_log 以及那三个索引。
//
// 额外追加的三条触发器不在文档里，但没有它们 FTS5 的 external-content 表
// 会与 mem_nodes 脱节（external content 不会自动同步）。它们只维护
// mem_fts，不改变任何表结构。
//
// 注意 "content" 必须加引号：不加引号时 FTS5 会把 `content='mem_nodes'` 解析成
// content 选项，那一列根本不存在（实测报 "table mem_fts has no column named
// content"）。加引号后列名仍是 content，且 external-content 选项同时生效，
// 列名与文档 4.3 逐字一致。
func initSynapseSchema(db *sql.DB) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS mem_nodes (
  id          TEXT PRIMARY KEY,
  user_id     TEXT NOT NULL,
  kind        TEXT NOT NULL CHECK (kind IN ('fact','entity','episode','procedure','topic')),
  title       TEXT NOT NULL DEFAULT '',
  content     TEXT NOT NULL,
  tokens      TEXT NOT NULL DEFAULT '',
  importance  REAL NOT NULL DEFAULT 0.5,
  pinned      INTEGER NOT NULL DEFAULT 0,
  status      TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','superseded','archived')),
  source_run  TEXT,
  hash        TEXT NOT NULL,
  embedding   BLOB,
  emb_model   TEXT,
  created_at  INTEGER NOT NULL,
  last_used   INTEGER,
  use_count   INTEGER NOT NULL DEFAULT 0,
  meta        TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS ux_mem_nodes_hash ON mem_nodes(user_id, kind, hash);
CREATE INDEX IF NOT EXISTS ix_mem_nodes_user ON mem_nodes(user_id, status, kind);

CREATE VIRTUAL TABLE IF NOT EXISTS mem_fts USING fts5(tokens, title, "content", content='mem_nodes', content_rowid='rowid', tokenize='unicode61');

CREATE TABLE IF NOT EXISTS mem_edges (
  src        TEXT NOT NULL REFERENCES mem_nodes(id) ON DELETE CASCADE,
  dst        TEXT NOT NULL REFERENCES mem_nodes(id) ON DELETE CASCADE,
  rel        TEXT NOT NULL CHECK (rel IN
             ('mentions','related','part_of','causes','derived_from','supersedes','contradicts','same_topic','used_with')),
  weight     REAL NOT NULL DEFAULT 0.3,
  fire_count INTEGER NOT NULL DEFAULT 0,
  last_fired INTEGER,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (src, dst, rel)
);
CREATE INDEX IF NOT EXISTS ix_mem_edges_dst ON mem_edges(dst);
CREATE INDEX IF NOT EXISTS ix_mem_edges_src_weight ON mem_edges(src, weight DESC);
CREATE INDEX IF NOT EXISTS ix_mem_edges_dst_weight ON mem_edges(dst, weight DESC);

CREATE TABLE IF NOT EXISTS mem_recall_log (
  run_id TEXT NOT NULL, node_id TEXT NOT NULL, activation REAL NOT NULL,
  via TEXT,
  used INTEGER NOT NULL DEFAULT 0,
  ts INTEGER NOT NULL, PRIMARY KEY (run_id, node_id)
);

CREATE TRIGGER IF NOT EXISTS mem_nodes_fts_ai AFTER INSERT ON mem_nodes BEGIN
  INSERT INTO mem_fts(rowid, tokens, title, content) VALUES (new.rowid, new.tokens, new.title, new.content);
END;
CREATE TRIGGER IF NOT EXISTS mem_nodes_fts_ad AFTER DELETE ON mem_nodes BEGIN
  INSERT INTO mem_fts(mem_fts, rowid, tokens, title, content) VALUES ('delete', old.rowid, old.tokens, old.title, old.content);
END;
CREATE TRIGGER IF NOT EXISTS mem_nodes_fts_au AFTER UPDATE ON mem_nodes BEGIN
  INSERT INTO mem_fts(mem_fts, rowid, tokens, title, content) VALUES ('delete', old.rowid, old.tokens, old.title, old.content);
  INSERT INTO mem_fts(rowid, tokens, title, content) VALUES (new.rowid, new.tokens, new.title, new.content);
END;
`
	if _, err := db.Exec(ddl); err != nil {
		return fmt.Errorf("memory: 初始化 synapse 表结构失败: %w", err)
	}
	return nil
}

// Path 便于日志与诊断（不含任何密钥）。
func (b *SynapseBackend) Path() string {
	if b == nil {
		return ""
	}
	return b.dbPath
}

// Close 停止后台队列并关闭库文件。可重复调用。
func (b *SynapseBackend) Close() error {
	if b == nil {
		return nil
	}
	b.closeOnce.Do(func() {
		b.bgOnce.Do(func() { close(b.bgStop) })
		done := make(chan struct{})
		go func() { b.bgWG.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			// 后台任务都是短事务，等待超时不是错误；库仍要关掉。
		}
		if b.db != nil {
			_ = b.db.Close()
		}
	})
	return nil
}

// Ping 用一次最轻的查询确认库可读写。
func (b *SynapseBackend) Ping(ctx context.Context) error {
	if b == nil || b.db == nil {
		return ErrDisabled
	}
	if _, err := b.db.ExecContext(ctx, `SELECT 1`); err != nil {
		b.noteError(err)
		return fmt.Errorf("memory: synapse 记忆库不可用: %w", err)
	}
	return nil
}

// LastError 返回最近一次失败的脱敏描述。文本里不含密钥（本后端不外发请求，
// 写入前已过 RedactString）。
func (b *SynapseBackend) LastError() (string, time.Time) {
	if b == nil {
		return "", time.Time{}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastErr, b.lastErrAt
}

func (b *SynapseBackend) noteError(err error) {
	if err == nil {
		return
	}
	b.mu.Lock()
	b.lastErr = err.Error()
	b.lastErrAt = time.Now()
	b.mu.Unlock()
}

// now 返回注入时钟或系统时间（UTC，秒级存储）。
func (b *SynapseBackend) now() time.Time {
	if b != nil && b.opts.Now != nil {
		return b.opts.Now().UTC()
	}
	return time.Now().UTC()
}

// enqueue 投递一个后台任务；队列满时丢弃并计数（BackfillDropped 语义）。
// 关键路径上它绝不阻塞。
func (b *SynapseBackend) enqueue(fn func()) {
	if b == nil || fn == nil {
		return
	}
	select {
	case <-b.bgStop:
		b.bgDropped.Add(1)
		return
	default:
	}
	select {
	case b.bg <- fn:
	case <-b.bgStop:
		b.bgDropped.Add(1)
	default:
		b.bgDropped.Add(1)
	}
}

func (b *SynapseBackend) runBackground() {
	defer b.bgWG.Done()
	for {
		select {
		case <-b.bgStop:
			// 关停前把已入队的短任务做完。
			for {
				select {
				case fn := <-b.bg:
					fn()
				default:
					return
				}
			}
		case fn := <-b.bg:
			fn()
		}
	}
}

// drainBackground 等后台队列排空（有界等待）。队列是 FIFO 且只有一个 worker，
// 因此投一个"标记任务"并等它执行完，就等价于"此前入队的任务都做完了"。
//
// 为什么写入流水线开始前要它：召回日志是异步写的，而 run 结束时的赫布更新
// 要读它。不等这一步就会出现"记忆明明被召回过，却因为日志还没落盘而不结算"
// 的竞态。等待有界（默认 250ms），超时就继续——宁可漏学一次，不拖慢收尾。
func (b *SynapseBackend) drainBackground(timeout time.Duration) bool {
	if b == nil {
		return true
	}
	done := make(chan struct{})
	select {
	case b.bg <- func() { close(done) }:
	case <-b.bgStop:
		return false
	case <-time.After(timeout):
		return false
	}
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// ---------------------------------------------------------------------------
// 查询接口（*sql.DB 与 *sql.Tx 都满足）
// ---------------------------------------------------------------------------

type synQuerier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

const synapseNodeCols = `id,user_id,kind,title,content,tokens,importance,pinned,status,
	COALESCE(source_run,''),hash,embedding,COALESCE(emb_model,''),created_at,last_used,use_count`

func scanSynapseNode(sc interface{ Scan(...any) error }) (synapseNode, error) {
	var n synapseNode
	var pinned int
	var created int64
	var lastUsed sql.NullInt64
	var emb []byte
	if err := sc.Scan(&n.ID, &n.UserID, &n.Kind, &n.Title, &n.Content, &n.Tokens,
		&n.Importance, &pinned, &n.Status, &n.SourceRun, &n.Hash, &emb, &n.EmbModel,
		&created, &lastUsed, &n.UseCount); err != nil {
		return n, err
	}
	n.Pinned = pinned != 0
	n.CreatedAt = time.Unix(created, 0).UTC()
	if lastUsed.Valid {
		n.LastUsed = time.Unix(lastUsed.Int64, 0).UTC()
	}
	n.Embedding = decodeFloat32s(emb)
	return n, nil
}

// getNode 按 id 读一条（限定 user，保证隔离）。
func (b *SynapseBackend) getNode(ctx context.Context, q synQuerier, id string) (synapseNode, bool, error) {
	row := q.QueryRowContext(ctx, `SELECT `+synapseNodeCols+` FROM mem_nodes WHERE id=? AND user_id=?`, id, b.opts.UserID)
	n, err := scanSynapseNode(row)
	if errors.Is(err, sql.ErrNoRows) {
		return synapseNode{}, false, nil
	}
	if err != nil {
		return synapseNode{}, false, err
	}
	return n, true, nil
}

// getNodes 批量按 id 读（缺失的跳过）。用户隔离在 Go 侧过滤：WHERE 里再带
// user_id 会让 SQLite 走 (user_id,status,kind) 索引逐行过滤，20 个 id 也要
// 十几毫秒；只用主键 IN 则是几次 B 树查找。
func (b *SynapseBackend) getNodes(ctx context.Context, q synQuerier, ids []string) (map[string]synapseNode, error) {
	out := make(map[string]synapseNode, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	placeholders := make([]string, 0, len(ids))
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		placeholders = append(placeholders, "?")
		args = append(args, id)
	}
	rows, err := q.QueryContext(ctx,
		`SELECT `+synapseNodeCols+` FROM mem_nodes WHERE id IN (`+strings.Join(placeholders, ",")+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		n, err := scanSynapseNode(rows)
		if err != nil {
			return nil, err
		}
		if n.UserID != b.opts.UserID {
			continue
		}
		out[n.ID] = n
	}
	return out, rows.Err()
}

// insertNode 插入节点。命中 (user_id,kind,hash) 唯一键时只 use_count++ 并刷新
// last_used（文档 4.4 第 4 条："命中同 hash → 只 use_count++ 并刷新 last_used，
// 不新增"），返回库里真实的那条 id 与是否新建。
func (b *SynapseBackend) insertNode(ctx context.Context, q synQuerier, n synapseNode) (string, bool, error) {
	if n.Status == "" {
		n.Status = StatusActive
	}
	if n.Importance == 0 {
		n.Importance = SynapseDefaultImportance
	}
	if n.Hash == "" {
		n.Hash = contentHash(n.Content)
	}
	if n.Tokens == "" {
		n.Tokens = synapseTokens(n.Title, n.Content)
	}
	var lastUsed any
	if !n.LastUsed.IsZero() {
		lastUsed = n.LastUsed.Unix()
	}

	var existingID string
	err := q.QueryRowContext(ctx, `SELECT id FROM mem_nodes WHERE user_id=? AND kind=? AND hash=?`,
		b.opts.UserID, n.Kind, n.Hash).Scan(&existingID)
	switch {
	case err == nil:
		if _, err := q.ExecContext(ctx,
			`UPDATE mem_nodes SET use_count = use_count + 1, last_used = COALESCE(?, last_used) WHERE id = ?`,
			lastUsed, existingID); err != nil {
			return "", false, err
		}
		return existingID, false, nil
	case !errors.Is(err, sql.ErrNoRows):
		return "", false, err
	}

	if _, err := q.ExecContext(ctx, `
INSERT INTO mem_nodes (id,user_id,kind,title,content,tokens,importance,pinned,status,source_run,hash,embedding,emb_model,created_at,last_used,use_count,meta)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		n.ID, b.opts.UserID, n.Kind, n.Title, n.Content, n.Tokens, clamp01(n.Importance), boolInt(n.Pinned),
		n.Status, nullIfEmpty(n.SourceRun), n.Hash, encodeFloat32s(n.Embedding), nullIfEmpty(n.EmbModel),
		n.CreatedAt.Unix(), lastUsed, n.UseCount, "{}"); err != nil {
		return "", false, err
	}
	return n.ID, true, nil
}

// upsertEdge 写边：已存在时取两者权重的大者（健壮性优于精度：一条边不该因为
// 一次低权重写入而变弱到失去意义）。返回是否新建。
func (b *SynapseBackend) upsertEdge(ctx context.Context, q synQuerier, e synapseEdge) (bool, error) {
	if e.Weight <= 0 {
		e.Weight = 0.3
	}
	created := e.CreatedAt
	if created.IsZero() {
		created = b.now()
	}
	var lastFired any
	if !e.LastFired.IsZero() {
		lastFired = e.LastFired.Unix()
	}
	res, err := q.ExecContext(ctx, `
INSERT INTO mem_edges (src,dst,rel,weight,fire_count,last_fired,created_at)
VALUES (?,?,?,?,?,?,?)
ON CONFLICT(src,dst,rel) DO UPDATE SET
  weight = MAX(mem_edges.weight, excluded.weight)`,
		e.Src, e.Dst, e.Rel, clamp01(e.Weight), e.FireCount, lastFired, created.Unix())
	if err != nil {
		return false, err
	}
	if n, err := res.RowsAffected(); err == nil && n == 1 {
		return true, nil
	}
	return false, nil
}

// edgesFor 取与 id 相连的边，按有效权重降序取前 fanOut 条（文档 4.5 的
// topEdges）。pinned 端点免衰减。
//
// 两个性能要点（2 万节点 / 6 万边的基准实测踩到过）：
//  1. 用 UNION ALL 而不是 `src=? OR dst=?`：OR 会让 SQLite 放弃索引、退化成
//     全表扫描。
//  2. 先按"存储权重"在 SQL 里截断到 fanOut×25 再来 Go 排序，而不是把所有
//     关联边捞回来：枢纽实体节点可能有几万条边，全量排序会把一次召回拖到
//     上百毫秒。截断半径 25 倍是留了衰减重排的余量（衰减只会让权重变小，
//     不可能把远在 200 名之外的边抬进前 8）。
//  3. 两个方向分别查询，各自用 (src,weight) / (dst,weight) 索引取前 N 条：
//     写成一条 UNION 再 ORDER BY 会让 ORDER BY 作用在合并结果上，索引用不上。
//
// 文档要求的 ix_mem_edges_dst 保持存在；(src,weight)/(dst,weight) 是为读取
// 路径追加的索引（纯追加，不改表结构与列名）。
func (b *SynapseBackend) edgesFor(ctx context.Context, id string, fanOut int, now time.Time) ([]synapseEdge, error) {
	bound := fanOut * 25
	if bound <= 0 {
		bound = 1000
	}
	out := make([]synapseEdge, 0, fanOut*2)
	for _, stmt := range []*sql.Stmt{b.edgeSrcStmt, b.edgeDstStmt} {
		if stmt == nil {
			return nil, errors.New("memory: synapse 边查询语句未初始化")
		}
		edges, err := b.scanEdges(ctx, stmt, id, bound, now)
		if err != nil {
			return nil, err
		}
		out = append(out, edges...)
	}
	sortEdgesByWeight(out)
	if fanOut > 0 && len(out) > fanOut {
		out = out[:fanOut]
	}
	return out, nil
}

// scanEdges 执行一条预编译的取边语句并按有效权重换算。
func (b *SynapseBackend) scanEdges(ctx context.Context, stmt *sql.Stmt, id string, bound int, now time.Time) ([]synapseEdge, error) {
	rows, err := stmt.QueryContext(ctx, id, bound)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]synapseEdge, 0, bound)
	for rows.Next() {
		var e synapseEdge
		var fire, lastFired, created int64
		var srcPinned, dstPinned int
		if err := rows.Scan(&e.Src, &e.Dst, &e.Rel, &e.Weight, &fire, &lastFired, &created, &srcPinned, &dstPinned); err != nil {
			return nil, err
		}
		e.FireCount = int(fire)
		if lastFired > 0 {
			e.LastFired = time.Unix(lastFired, 0).UTC()
		}
		e.CreatedAt = time.Unix(created, 0).UTC()
		e.Weight = edgeEffectiveWeight(e, srcPinned != 0 || dstPinned != 0, now)
		out = append(out, e)
	}
	return out, rows.Err()
}

// trimFTSTokens 把"已被二元组覆盖的单字 CJK 词"从 FTS 查询里去掉。
//
// 为什么必须去：tokenize 对中文同时产出单字与二元组（为了召回），但单字几乎
// 出现在每一条中文记忆里。拿它去 MATCH，等于把整库都变成候选，bm25 必须对
// 2 万条命中逐条打分 —— 实测一次召回 30-50ms 全花在这里。二元组已经覆盖了
// 单字的匹配能力（"引擎" 命中时必然也命中 "引"），去掉它只是减少了噪声候选。
//
// 只作用于查询侧：写入侧 mem_nodes.tokens 仍然保留单字（那才是"预分词"的
// 完整形态），因此不影响 FTS 索引本身。
func trimFTSTokens(tokens []string) []string {
	covered := map[rune]bool{}
	for _, t := range tokens {
		r := []rune(t)
		if len(r) < 2 {
			continue
		}
		for _, c := range r {
			if isCJKRune(c) {
				covered[c] = true
			}
		}
	}
	if len(covered) == 0 {
		return tokens
	}
	out := make([]string, 0, len(tokens))
	for _, t := range tokens {
		r := []rune(t)
		if len(r) == 1 && isCJKRune(r[0]) && covered[r[0]] {
			continue
		}
		out = append(out, t)
	}
	return out
}

func isCJKRune(r rune) bool {
	return unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r)
}

// ftsHit 是一次 FTS5 bm25 命中。
type ftsHit struct {
	ID   string
	Rank float64 // bm25 原值（越负越相关）
}

// ftsSearch 用 FTS5 bm25 取前 limit 个种子（文档 4.5 第 2 条）。queryTokens
// 是 tokenize 的结果，逐词用双引号包住（tokenize 已去掉标点，不会破坏语法）。
func (b *SynapseBackend) ftsSearch(ctx context.Context, q synQuerier, tokens []string, limit int) ([]ftsHit, error) {
	if len(tokens) == 0 || limit <= 0 {
		return nil, nil
	}
	tokens = trimFTSTokens(tokens)
	if len(tokens) == 0 {
		return nil, nil
	}
	if len(tokens) > 24 {
		tokens = tokens[:24]
	}
	quoted := make([]string, 0, len(tokens))
	for _, t := range tokens {
		t = strings.ReplaceAll(t, `"`, "")
		if t == "" {
			continue
		}
		quoted = append(quoted, `"`+t+`"`)
	}
	if len(quoted) == 0 {
		return nil, nil
	}
	// 关键性能点：先让 FTS5 在自己的索引里按 bm25 排好序并截断，再 JOIN 回
	// mem_nodes 做 user / status 过滤。反过来写（JOIN 带过滤 + ORDER BY rank）
	// 会让查询计划对每一个命中行都去 external-content 表读一次原始列，2 万条
	// 命中时一次召回要 50ms+（实测）；先排序后 JOIN 只需读最终那几十行。
	over := limit * 10
	if over < 100 {
		over = 100
	}
	// 分两步，而不是一条 JOIN：
	//  1) 让 FTS5 在自己的索引里按 bm25 排好序并截断（只读 mem_fts）；
	//  2) 用 rowid 主键批量回查 mem_nodes 做 user / status 过滤。
	// 写成一条 JOIN 时 SQLite 会选错连接顺序（EXPLAIN 实测：SEARCH n USING
	// INDEX ix_mem_nodes_user 逐行扫 user 索引，2 万节点白扫一遍）。两步走
	// 的第一步是纯 FTS 排名，第二步是几次 B 树查找。
	rankRows, err := q.QueryContext(ctx, `
SELECT rowid, bm25(mem_fts) AS rank
FROM mem_fts
WHERE mem_fts MATCH ?
ORDER BY bm25(mem_fts)
LIMIT ?`, strings.Join(quoted, " OR "), over)
	if err != nil {
		return nil, err
	}
	type ranked struct {
		rowid int64
		rank  float64
	}
	var rankedHits []ranked
	for rankRows.Next() {
		var r ranked
		if err := rankRows.Scan(&r.rowid, &r.rank); err != nil {
			rankRows.Close()
			return nil, err
		}
		rankedHits = append(rankedHits, r)
	}
	if err := rankRows.Err(); err != nil {
		rankRows.Close()
		return nil, err
	}
	rankRows.Close()
	if len(rankedHits) == 0 {
		return nil, nil
	}

	placeholders := make([]string, 0, len(rankedHits))
	args := make([]any, 0, len(rankedHits))
	for _, r := range rankedHits {
		placeholders = append(placeholders, "?")
		args = append(args, r.rowid)
	}
	meta, err := q.QueryContext(ctx,
		`SELECT rowid, id, user_id, status FROM mem_nodes WHERE rowid IN (`+strings.Join(placeholders, ",")+`)`, args...)
	if err != nil {
		return nil, err
	}
	type metaRow struct {
		id, userID, status string
	}
	byRowid := make(map[int64]metaRow, len(rankedHits))
	for meta.Next() {
		var rowid int64
		var m metaRow
		if err := meta.Scan(&rowid, &m.id, &m.userID, &m.status); err != nil {
			meta.Close()
			return nil, err
		}
		byRowid[rowid] = m
	}
	if err := meta.Err(); err != nil {
		meta.Close()
		return nil, err
	}
	meta.Close()

	out := make([]ftsHit, 0, limit)
	for _, r := range rankedHits {
		m, ok := byRowid[r.rowid]
		if !ok || m.userID != b.opts.UserID || m.status != StatusActive {
			continue
		}
		out = append(out, ftsHit{ID: m.id, Rank: r.rank})
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// entitySeeds 找出 query 里出现的已知 entity（最长匹配优先，文档 4.5 第 2 条）。
func (b *SynapseBackend) entitySeeds(ctx context.Context, q synQuerier, query string) ([]synapseNode, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT `+synapseNodeCols+` FROM mem_nodes WHERE user_id=? AND kind=? AND status=?`,
		b.opts.UserID, NodeEntity, StatusActive)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []synapseNode
	for rows.Next() {
		n, err := scanSynapseNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return longestEntityMatches(query, out), nil
}

// longestEntityMatches 在 query 中做 entity 名的原文最长匹配：已匹配的字符
// 区间不再被更短的实体重复占用（"XimoAgent" 命中后不再匹配 "Agent"）。
func longestEntityMatches(query string, ents []synapseNode) []synapseNode {
	if strings.TrimSpace(query) == "" || len(ents) == 0 {
		return nil
	}
	lower := strings.ToLower(query)
	type span struct{ start, end int }
	var taken []span
	// 长名优先。
	sorted := make([]synapseNode, len(ents))
	copy(sorted, ents)
	for i := 0; i < len(sorted); i++ {
		for j := i + 1; j < len(sorted); j++ {
			if len([]rune(sorted[j].Title)) > len([]rune(sorted[i].Title)) {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}
	var out []synapseNode
	for _, e := range sorted {
		name := strings.ToLower(strings.TrimSpace(e.Title))
		if name == "" {
			continue
		}
		idx := strings.Index(lower, name)
		if idx < 0 {
			continue
		}
		overlap := false
		for _, s := range taken {
			if idx < s.end && s.start < idx+len(name) {
				overlap = true
				break
			}
		}
		if overlap {
			continue
		}
		taken = append(taken, span{start: idx, end: idx + len(name)})
		out = append(out, e)
	}
	return out
}

// countNodes / countEdges 供整理与 Stats 使用。
func (b *SynapseBackend) countNodes(ctx context.Context, q synQuerier) (int, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM mem_nodes WHERE user_id=?`, b.opts.UserID).Scan(&n)
	return n, err
}

// ---------------------------------------------------------------------------
// 类型转换与排序小工具
// ---------------------------------------------------------------------------

func sortEdgesByWeight(edges []synapseEdge) {
	sort.SliceStable(edges, func(i, j int) bool { return edges[i].Weight > edges[j].Weight })
}

func clamp01(v float64) float64 {
	if math.IsNaN(v) {
		return 0
	}
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullIfEmpty(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

func encodeFloat32s(v []float32) any {
	if len(v) == 0 {
		return nil
	}
	buf := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(f))
	}
	return buf
}

func decodeFloat32s(b []byte) []float32 {
	if len(b) < 4 {
		return nil
	}
	out := make([]float32, len(b)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return out
}

// SynapseStats 是 Synapse 后端的运行计数快照（文档 4.7 第 5 条要求整理统计
// 写回 Stats）。
type SynapseStats struct {
	UserID string
	Nodes  int
	Edges  int
	// RecallCalls / RecallErrors / RecallChars 召回侧计数。
	RecallCalls  uint64
	RecallErrors uint64
	RecallChars  uint64
	// BgDropped 是后台队列满被丢弃的任务数（BackfillDropped 语义）。
	BgDropped uint64
	// Consolidations / MergedFacts / ArchivedNodes / TopicsCreated 整理侧计数。
	Consolidations uint64
	MergedFacts    int
	ArchivedNodes  int
	TopicsCreated  int
	// LastError / LastErrorAt 最近一次失败（已脱敏）。
	LastError   string
	LastErrorAt time.Time
}

// Stats 返回计数快照。它顺带数一次库里的节点与边（有界：单用户量级）。
func (b *SynapseBackend) Stats(ctx context.Context) SynapseStats {
	if b == nil || b.db == nil {
		return SynapseStats{}
	}
	s := SynapseStats{
		UserID:         b.opts.UserID,
		RecallCalls:    b.recallCalls.Load(),
		RecallErrors:   b.recallErrors.Load(),
		RecallChars:    b.recallChars.Load(),
		BgDropped:      b.bgDropped.Load(),
		Consolidations: b.consolidates.Load(),
	}
	s.MergedFacts = int(b.mergedFacts.Load())
	s.ArchivedNodes = int(b.archivedNodes.Load())
	s.TopicsCreated = int(b.topicsCreated.Load())
	s.LastError, s.LastErrorAt = b.LastError()
	if n, err := b.countNodes(ctx, b.db); err == nil {
		s.Nodes = n
	}
	var e int
	if err := b.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM mem_edges e
JOIN mem_nodes n ON n.id = e.src WHERE n.user_id=?`, b.opts.UserID).Scan(&e); err == nil {
		s.Edges = e
	}
	return s
}
