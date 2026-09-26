package memory

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// EmbeddedBackend 是进程内的长期记忆后端：一个 SQLite 文件，不用任何外部服务。
//
// 为什么自带一个库文件而不是用引擎自己的库：引擎的 schema 由 migrations/ 统一管
// 理（版本连续 + checksum 钉死 + 有既定的归属规则，见 docs/接口裁决记录.md），
// 为一个可选特性去动那张注册表不成比例。记忆数据独立成文件还有两个好处：
// 删掉它就等于"清空全部记忆"，且它不随引擎库的迁移与压缩一起变动。
//
// 它存在哪里：由装配方传入（bootstrap 用 <BaseDir>/data/memory.db）。
type EmbeddedBackend struct {
	cfg Config
	db  *sql.DB

	mu        sync.Mutex
	lastErr   string
	lastErrAt time.Time
}

var _ Backend = (*EmbeddedBackend)(nil)

// NewEmbeddedBackend 打开（必要时创建）进程内记忆库。
func NewEmbeddedBackend(path string, cfg Config) (*EmbeddedBackend, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("memory: 进程内后端需要库文件路径")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("memory: 创建数据目录失败: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("memory: 打开记忆库失败: %w", err)
	}
	// 与引擎库一致的做法：单写者、WAL、有界等待。记忆的写入量很小，但并发调用
	// （多个 run 同时收尾）是常态，busy_timeout 必须有。
	pragmas := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA busy_timeout=5000",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("memory: 设置 %q 失败: %w", p, err)
		}
	}
	if err := initEmbeddedSchema(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &EmbeddedBackend{cfg: cfg.WithDefaults(), db: db}, nil
}

func initEmbeddedSchema(db *sql.DB) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS memories (
  id            TEXT PRIMARY KEY,
  user_id       TEXT NOT NULL,
  agent_id      TEXT NOT NULL DEFAULT '',
  run_id        TEXT NOT NULL DEFAULT '',
  memory        TEXT NOT NULL,
  hash          TEXT NOT NULL DEFAULT '',
  metadata_json TEXT NOT NULL DEFAULT '{}',
  created_at    TEXT NOT NULL,
  updated_at    TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_memories_user_time ON memories(user_id, created_at DESC);
CREATE UNIQUE INDEX IF NOT EXISTS idx_memories_user_hash ON memories(user_id, hash);
`
	if _, err := db.Exec(ddl); err != nil {
		return fmt.Errorf("memory: 初始化表结构失败: %w", err)
	}
	return nil
}

// Close 关闭库文件。可重复调用。
func (b *EmbeddedBackend) Close() error {
	if b == nil || b.db == nil {
		return nil
	}
	return b.db.Close()
}

// Path 便于日志与诊断（不含任何密钥）。
func (b *EmbeddedBackend) Path() string {
	return fmt.Sprintf("%s（进程内 SQLite）", b.cfg.Backend)
}

// Ping 用一次最轻的查询确认库可读写。
func (b *EmbeddedBackend) Ping(ctx context.Context) error {
	if b == nil || b.db == nil {
		return ErrDisabled
	}
	if _, err := b.db.ExecContext(ctx, `SELECT 1`); err != nil {
		b.noteError(err)
		return fmt.Errorf("memory: 记忆库不可用: %w", err)
	}
	return nil
}

// Add 写入一轮消息。
//
// 不做抽取是有意的：没有 LLM 就不该假装能"提炼事实"。存原文至少可核查、不会编造。
// 同一 user 下内容相同则只保留一条（同 hash 更新 updated_at），因此重复投递不会
// 把库塞满副本。
func (b *EmbeddedBackend) Add(ctx context.Context, msgs []Message, opts AddOptions) ([]Record, error) {
	if b == nil || b.db == nil {
		return nil, ErrDisabled
	}
	text := joinMessages(msgs)
	if text == "" {
		return []Record{}, nil
	}
	now := time.Now().UTC().Format(time.RFC3339)
	metaJSON := "{}"
	if len(opts.Metadata) > 0 {
		if raw, err := json.Marshal(opts.Metadata); err == nil {
			metaJSON = string(raw)
		}
	}
	rec := Record{
		ID:        newEmbeddedID(),
		Memory:    text,
		UserID:    b.cfg.UserID,
		AgentID:   b.cfg.AgentID,
		RunID:     opts.RunID,
		Hash:      hashText(b.cfg.UserID + "\x00" + text),
		Metadata:  opts.Metadata,
		CreatedAt: now,
		UpdatedAt: now,
	}
	_, err := b.db.ExecContext(ctx, `
INSERT INTO memories (id,user_id,agent_id,run_id,memory,hash,metadata_json,created_at,updated_at)
VALUES (?,?,?,?,?,?,?,?,?)
ON CONFLICT(user_id, hash) DO UPDATE SET
  run_id=excluded.run_id, metadata_json=excluded.metadata_json, updated_at=excluded.updated_at`,
		rec.ID, rec.UserID, rec.AgentID, rec.RunID, rec.Memory, rec.Hash, metaJSON, rec.CreatedAt, rec.UpdatedAt)
	if err != nil {
		b.noteError(err)
		return nil, fmt.Errorf("memory: 写入记忆失败: %w", err)
	}
	// 回读库里真实的那条（冲突时 ID/创建时间以库内为准）
	var keptID, keptCreated string
	if err := b.db.QueryRowContext(ctx,
		`SELECT id, created_at FROM memories WHERE user_id=? AND hash=?`, rec.UserID, rec.Hash).
		Scan(&keptID, &keptCreated); err == nil {
		rec.ID, rec.CreatedAt = keptID, keptCreated
	}
	return []Record{rec}, nil
}

// Search 词法检索（见 lexical.go 的说明）。
func (b *EmbeddedBackend) Search(ctx context.Context, query string, opts SearchOptions) ([]Record, error) {
	if b == nil || b.db == nil {
		return nil, ErrDisabled
	}
	topK := opts.TopK
	if topK <= 0 {
		topK = b.cfg.TopK
	}
	qTokens := tokenize(query)
	if len(qTokens) == 0 {
		return []Record{}, nil
	}
	all, err := b.recent(ctx, 0)
	if err != nil {
		return nil, err
	}
	return rank(all, query, qTokens, topK), nil
}

// GetAll 列出最近 topK 条。
func (b *EmbeddedBackend) GetAll(ctx context.Context, topK int) ([]Record, error) {
	if b == nil || b.db == nil {
		return nil, ErrDisabled
	}
	if topK <= 0 {
		topK = b.cfg.TopK
	}
	return b.recent(ctx, topK)
}

// Delete 删除一条记忆；目标不存在返回错误（便于发现删错 ID）。
func (b *EmbeddedBackend) Delete(ctx context.Context, id string) error {
	if b == nil || b.db == nil {
		return ErrDisabled
	}
	if strings.TrimSpace(id) == "" {
		return errors.New("memory: 删除需要 id")
	}
	res, err := b.db.ExecContext(ctx, `DELETE FROM memories WHERE id=?`, id)
	if err != nil {
		b.noteError(err)
		return fmt.Errorf("memory: 删除失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("memory: 没有 id=%s 这条记忆", id)
	}
	return nil
}

// LastError 返回最近一次失败的脱敏描述。文本里不含任何密钥（本后端不外发请求）。
func (b *EmbeddedBackend) LastError() (string, time.Time) {
	if b == nil {
		return "", time.Time{}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastErr, b.lastErrAt
}

func (b *EmbeddedBackend) noteError(err error) {
	if err == nil {
		return
	}
	b.mu.Lock()
	b.lastErr = err.Error()
	b.lastErrAt = time.Now()
	b.mu.Unlock()
}

// recent 取该用户的最近若干条（limit<=0 表示取上限内的全部）。
func (b *EmbeddedBackend) recent(ctx context.Context, limit int) ([]Record, error) {
	if limit <= 0 {
		limit = 2000 // 与独立服务的 scan-limit 同量级：个人记忆量级足够，且保证查询有界
	}
	rows, err := b.db.QueryContext(ctx, `
SELECT id,user_id,agent_id,run_id,memory,hash,metadata_json,created_at,updated_at
FROM memories WHERE user_id=? ORDER BY created_at DESC LIMIT ?`, b.cfg.UserID, limit)
	if err != nil {
		b.noteError(err)
		return nil, fmt.Errorf("memory: 查询记忆失败: %w", err)
	}
	defer rows.Close()
	out := make([]Record, 0, 16)
	for rows.Next() {
		var rec Record
		var meta string
		if err := rows.Scan(&rec.ID, &rec.UserID, &rec.AgentID, &rec.RunID, &rec.Memory,
			&rec.Hash, &meta, &rec.CreatedAt, &rec.UpdatedAt); err != nil {
			b.noteError(err)
			return nil, fmt.Errorf("memory: 读取记忆失败: %w", err)
		}
		if meta != "" && meta != "{}" {
			_ = json.Unmarshal([]byte(meta), &rec.Metadata)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		b.noteError(err)
		return nil, fmt.Errorf("memory: 遍历记忆失败: %w", err)
	}
	return out, nil
}

// joinMessages 把一轮消息折成一条记忆文本（角色名本地化，便于日后人工阅读）。
func joinMessages(msgs []Message) string {
	var b strings.Builder
	for _, m := range msgs {
		c := strings.TrimSpace(m.Content)
		if c == "" {
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
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "%s：%s", role, c)
	}
	return strings.TrimSpace(b.String())
}

// embeddedIDSeq 保证同一进程内生成的 ID 严格递增且不重复。
//
// 不要只靠 time.Now().UnixNano()：Windows 的时钟精度较粗，同一个 tick 里连续两次
// 写入会得到相同的纳秒值，于是主键冲突（实测踩过：UNIQUE constraint failed: memories.id）。
var (
	embeddedIDMu  sync.Mutex
	embeddedIDSeq uint64
)

func newEmbeddedID() string {
	embeddedIDMu.Lock()
	embeddedIDSeq++
	seq := embeddedIDSeq
	embeddedIDMu.Unlock()
	sum := sha256.Sum256(fmt.Appendf(nil, "%d/%d/%d", time.Now().UnixNano(), os.Getpid(), seq))
	return "mem_" + hex.EncodeToString(sum[:12])
}

func hashText(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}
