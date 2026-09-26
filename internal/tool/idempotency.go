package tool

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

// IdempotencyClass 定义在 types.go（ClassIdempotent/ClassDetectable/ClassNonIdempotent）。

// tool_idempotency 表状态值（第19章）。
const (
	// StatusInflight 已认领、执行中（或执行中进程崩溃留下的陈旧认领）。
	StatusInflight = "inflight"
	// StatusDone 已完成且结果已持久化（durable side effect 已发生）。
	StatusDone = "done"
	// StatusFailed 执行失败，可重试。
	StatusFailed = "failed"
	// StatusNeedsConfirmation C 类非幂等调用崩溃后等待用户确认，绝不自动重复。
	StatusNeedsConfirmation = "needs_confirmation"
)

// IdempotencySchema 是 tool_idempotency 的建表 SQL（第19章原文）。
//
// 跨任务依赖（审核报告 B-2，08 号已裁决 D-5：migrations 唯一来源是任务03）：
// 本包**不创建**这张表。表由任务03 的 migrations 提供
// （`migrations/0002_tool_idempotency.sql`，DDL 与本常量逐字一致）。
// 生产环境若漏掉该 migration，工具调用的第一步幂等查询就会
// `no such table: tool_idempotency` 并以 ErrToolFailed 失败——这是**响亮的**
// 失败而不是静默错误。集成测试见 idempotency_sqlite_test.go（真实 SQLite）。
const IdempotencySchema = `CREATE TABLE tool_idempotency (
    key TEXT PRIMARY KEY,
    status TEXT NOT NULL,
    result BLOB,
    created_at INTEGER NOT NULL
);`

// IdempotencyStore 是任务02（Engine）消费的契约接口。
type IdempotencyStore interface {
	// Classify 判定某个 tool call 的幂等分类（A/B/C）。
	Classify(ctx context.Context, toolCallID string) (IdempotencyClass, error)
	// Get 按 idempotency key 查询状态与持久化结果。
	Get(ctx context.Context, key string) (status string, result []byte, found bool, err error)
}

// ErrToolCallUnknown 表示无法解析 tool call 对应的工具（分类时fail-safe到非幂等）。
var ErrToolCallUnknown = errors.New("tool: 未知 tool call")

// ToolCallResolver 把 toolCallID 解析为工具名与参数（生产环境由任务03的
// tool_calls repository 提供；开发期用 mock）。
type ToolCallResolver interface {
	ResolveToolCall(ctx context.Context, toolCallID string) (toolName string, args map[string]any, err error)
}

// IdempotencyDB 是 tool_idempotency 表的存储交互接口。任务03提供 SQLite 实现
// （见 NewSQLIdempotencyDB）；开发期用内存实现。
//
// Claim 必须是原子的：同一 key 只能有一个调用方成功认领（I5 不变量）。
type IdempotencyDB interface {
	// Claim 原子认领 key。若 key 不存在则插入 inflight 记录并返回 true；
	// 若已存在未陈旧（created_at >= staleBefore）的 inflight 记录则返回 false；
	// 若已存在陈旧 inflight 记录则接管并返回 true。
	Claim(ctx context.Context, key string, now, staleBefore int64) (claimed bool, err error)
	// Get 查询状态与结果。
	Get(ctx context.Context, key string) (status string, result []byte, createdAt int64, found bool, err error)
	// Complete 把 inflight 记录标记为 done 并写入结果（仅当仍是 inflight）。
	Complete(ctx context.Context, key string, result []byte, now int64) error
	// Fail 把 inflight 记录标记为 failed（仅当仍是 inflight）。
	Fail(ctx context.Context, key string, now int64) error
	// MarkNeedsConfirmation 把记录标记为 needs_confirmation（C 类崩溃恢复）。
	MarkNeedsConfirmation(ctx context.Context, key string, now int64) error
}

// ---------------------------------------------------------------------------
// Store：分类 + 表交互
// ---------------------------------------------------------------------------

// ClassPolicy 工具 → 幂等分类的策略表。
type ClassPolicy struct {
	// Default 未列出工具的默认分类（fail-safe：非幂等）。
	Default IdempotencyClass
	// Tool 工具级分类。
	Tool map[string]IdempotencyClass
	// ToolAction 工具+动作级分类（key = "tool:action"），优先于 Tool。
	ToolAction map[string]IdempotencyClass
}

// DefaultClassPolicy 返回三分类的默认策略（第19章）：
//   - A 类幂等：file_read/web_fetch/knowledge_search 等纯读；
//   - B 类可检测幂等：git status/带 expected hash 的 file write；
//   - C 类非幂等：send message/delete file/外部 API mutation。
func DefaultClassPolicy() *ClassPolicy {
	return &ClassPolicy{
		Default: ClassNonIdempotent,
		Tool: map[string]IdempotencyClass{
			// A 类幂等 — 可自动重试
			"file_read":   ClassIdempotent,
			"file_list":   ClassIdempotent,
			"file_search": ClassIdempotent,
			"web_fetch":   ClassIdempotent,
			"web_search":  ClassIdempotent,
			"git_log":     ClassIdempotent,
			"git_diff":    ClassIdempotent,
			"git_branch":  ClassIdempotent,
			// B 类可检测幂等 — 检查当前状态后再决定
			"git_status":  ClassDetectable,
			"file_write":  ClassDetectable,
			"file_edit":   ClassDetectable,
			"multi_edit":  ClassDetectable,
			"move_file":   ClassDetectable,
			"create_tool": ClassDetectable,
			"todo_write":  ClassDetectable,
			// C 类非幂等 — 崩溃恢复绝不自动重复
			"file_delete":    ClassNonIdempotent,
			"terminal_exec":  ClassNonIdempotent,
			"send_message":   ClassNonIdempotent,
			"office_docs":    ClassNonIdempotent,
			"browser_*":      ClassNonIdempotent,
			"computer_use":   ClassNonIdempotent,
			"act_ui":         ClassNonIdempotent,
			"git_operations": ClassNonIdempotent,
			"mcp_*":          ClassNonIdempotent,
		},
		ToolAction: map[string]IdempotencyClass{
			// knowledge 按动作分类：search/list 只读（A），add/update 可检测（B），delete 不可重复（C）
			"knowledge:search": ClassIdempotent,
			"knowledge:list":   ClassIdempotent,
			"knowledge:add":    ClassDetectable,
			"knowledge:update": ClassDetectable,
			"knowledge:delete": ClassNonIdempotent,
			// memory 按动作分类：检索/浏览只读（A），写入可检测（B），
			// 遗忘是外部状态删除，崩溃恢复绝不自动重复（C）。
			"memory:search": ClassIdempotent,
			"memory:list":   ClassIdempotent,
			"memory:add":    ClassDetectable,
			"memory:forget": ClassNonIdempotent,
			// git_operations 的只读动作
			"git_operations:status": ClassDetectable,
			"git_operations:log":    ClassIdempotent,
			"git_operations:diff":   ClassIdempotent,
			"git_operations:branch": ClassIdempotent,
		},
	}
}

// ClassifyTool 按策略判定工具分类。
func (p *ClassPolicy) ClassifyTool(toolName, action string) IdempotencyClass {
	if action != "" {
		if class, ok := p.ToolAction[toolName+":"+action]; ok {
			return class
		}
	}
	if class, ok := p.Tool[toolName]; ok {
		return class
	}
	// 前缀通配（browser_* / mcp_* 等）
	for pattern, class := range p.Tool {
		if len(pattern) > 0 && pattern[len(pattern)-1] == '*' {
			if len(toolName) >= len(pattern)-1 && toolName[:len(pattern)-1] == pattern[:len(pattern)-1] {
				return class
			}
		}
	}
	return p.Default
}

// Store 是 IdempotencyStore 的完整实现，同时供 ToolRuntime 内部使用。
type Store struct {
	DB       IdempotencyDB
	Resolver ToolCallResolver
	Policy   *ClassPolicy

	// ClaimTTL inflight 认领的存活时间。超过该时间未完成的认领视为
	// 进程崩溃遗留，可被接管（A/B 类）或标记 needs_confirmation（C 类）。
	ClaimTTL time.Duration

	// Now 便于测试注入。
	Now func() time.Time

	mu sync.Mutex // 保护进程内缓存的已完成结果查询（DB 自身并发安全）
}

// NewIdempotencyStore 创建幂等性存储。
func NewIdempotencyStore(db IdempotencyDB, resolver ToolCallResolver, policy *ClassPolicy, claimTTL time.Duration) *Store {
	if policy == nil {
		policy = DefaultClassPolicy()
	}
	if claimTTL <= 0 {
		claimTTL = 5 * time.Minute
	}
	return &Store{DB: db, Resolver: resolver, Policy: policy, ClaimTTL: claimTTL, Now: time.Now}
}

// Key 计算 idempotency key = hash(run_id + tool_call_id)（第19章）。
func Key(runID, toolCallID string) string {
	sum := sha256.Sum256([]byte(runID + "\x00" + toolCallID))
	return hex.EncodeToString(sum[:])
}

// Classify 实现 IdempotencyStore。无法解析 tool call 时 fail-safe 到
// ClassNonIdempotent（绝不自动重复未知调用）。
func (s *Store) Classify(ctx context.Context, toolCallID string) (IdempotencyClass, error) {
	if s.Resolver == nil {
		return ClassNonIdempotent, fmt.Errorf("%w: 未配置 ToolCallResolver", ErrToolCallUnknown)
	}
	toolName, args, err := s.Resolver.ResolveToolCall(ctx, toolCallID)
	if err != nil {
		if errors.Is(err, ErrToolCallUnknown) {
			return ClassNonIdempotent, fmt.Errorf("%w: %v", ErrToolCallUnknown, toolCallID)
		}
		return ClassNonIdempotent, err
	}
	return s.Policy.ClassifyTool(toolName, actionOf(toolName, args)), nil
}

// actionOf 从参数中提取动作名（与权限引擎的 Action 维度保持一致）。
func actionOf(toolName string, args map[string]any) string {
	if v, ok := args["action"]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// Get 实现 IdempotencyStore。
func (s *Store) Get(ctx context.Context, key string) (status string, result []byte, found bool, err error) {
	status, result, _, found, err = s.DB.Get(ctx, key)
	return status, result, found, err
}

// Claim 原子认领 key。返回 false 表示已有未陈旧的 inflight 执行（I5：同一 key
// 不能并行产生两个 durable side effect）。
func (s *Store) Claim(ctx context.Context, key string) (bool, error) {
	now := s.nowUnix()
	staleBefore := now - int64(s.ClaimTTL/time.Second)
	return s.DB.Claim(ctx, key, now, staleBefore)
}

// Complete 持久化成功结果（durable commit）。仅当记录仍为 inflight 时生效，
// 保证同一 key 的结果只会被提交一次。
func (s *Store) Complete(ctx context.Context, key string, result []byte) error {
	return s.DB.Complete(ctx, key, result, s.nowUnix())
}

// Fail 标记执行失败（允许后续重试）。
func (s *Store) Fail(ctx context.Context, key string) error {
	return s.DB.Fail(ctx, key, s.nowUnix())
}

// MarkNeedsConfirmation 标记 C 类调用需要用户确认。
func (s *Store) MarkNeedsConfirmation(ctx context.Context, key string) error {
	return s.DB.MarkNeedsConfirmation(ctx, key, s.nowUnix())
}

func (s *Store) nowUnix() int64 { return s.Now().Unix() }

// RecoverInflight 处理崩溃后遗留的 inflight 记录（第19章恢复路径）：
//   - A 类：清除陈旧认领，允许重新执行；
//   - B 类：清除陈旧认领，允许重新执行（执行前会做状态检测）；
//   - C 类：标记 needs_confirmation，绝不自动重复。
//
// 返回是否放行重新执行。
func (s *Store) RecoverInflight(ctx context.Context, key string, class IdempotencyClass) (resumeAllowed bool, err error) {
	status, _, createdAt, found, err := s.DB.Get(ctx, key)
	if err != nil {
		return false, err
	}
	if !found || status != StatusInflight {
		return status == StatusDone, nil
	}
	if s.Now().Unix()-createdAt < int64(s.ClaimTTL/time.Second) {
		// 认领仍新鲜，可能有其他进程正在执行。
		return false, nil
	}
	switch class {
	case ClassNonIdempotent:
		return false, s.DB.MarkNeedsConfirmation(ctx, key, s.Now().Unix())
	default:
		// A/B 类：接管陈旧认领，允许恢复执行。
		claimed, err := s.DB.Claim(ctx, key, s.nowUnix(), s.nowUnix()-int64(s.ClaimTTL/time.Second))
		if err != nil {
			return false, err
		}
		return claimed, nil
	}
}

// ---------------------------------------------------------------------------
// B 类“可检测幂等”的状态检测
// ---------------------------------------------------------------------------

// StateStatus B 类工具执行前的状态检测结果。
type StateStatus int

const (
	// StateUnknown 无法检测当前状态（按未完成处理）。
	StateUnknown StateStatus = iota
	// StateNotDone 尚未完成，需要执行。
	StateNotDone
	// StateAlreadyDone 当前状态已满足目标，无需重复执行。
	StateAlreadyDone
)

// StateChecker 由 B 类工具实现：执行前检查当前状态，崩溃恢复时据此避免
// 重复副作用（第19章）。
type StateChecker interface {
	CheckState(ctx context.Context, req ToolRequest) (StateStatus, error)
}

// ---------------------------------------------------------------------------
// 内存实现（开发期 mock；生产由任务03 的 SQLite 实现顶上）
// ---------------------------------------------------------------------------

// memoryDB 是 IdempotencyDB 的内存实现，语义与 SQL 实现一致。
type memoryDB struct {
	mu      sync.Mutex
	records map[string]memoryRecord
}

type memoryRecord struct {
	status    string
	result    []byte
	createdAt int64
}

// NewMemoryIdempotencyDB 创建内存实现。
func NewMemoryIdempotencyDB() IdempotencyDB {
	return &memoryDB{records: make(map[string]memoryRecord)}
}

func (m *memoryDB) Claim(ctx context.Context, key string, now, staleBefore int64) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.records[key]
	if !ok {
		m.records[key] = memoryRecord{status: StatusInflight, createdAt: now}
		return true, nil
	}
	if rec.status == StatusInflight && rec.createdAt >= staleBefore {
		return false, nil // 新鲜的 inflight：他人执行中
	}
	// 不存在 / 已完成过 / 陈旧 inflight：接管为 inflight
	rec = memoryRecord{status: StatusInflight, createdAt: now}
	m.records[key] = rec
	return true, nil
}

func (m *memoryDB) Get(ctx context.Context, key string) (string, []byte, int64, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", nil, 0, false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.records[key]
	if !ok {
		return "", nil, 0, false, nil
	}
	return rec.status, rec.result, rec.createdAt, true, nil
}

func (m *memoryDB) Complete(ctx context.Context, key string, result []byte, now int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.records[key]
	if !ok || rec.status != StatusInflight {
		return fmt.Errorf("tool: key %q 不是 inflight 状态，拒绝重复提交（I5）", key)
	}
	rec.status = StatusDone
	rec.result = append([]byte(nil), result...)
	m.records[key] = rec
	return nil
}

func (m *memoryDB) Fail(ctx context.Context, key string, now int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.records[key]
	if !ok || rec.status != StatusInflight {
		return fmt.Errorf("tool: key %q 不是 inflight 状态", key)
	}
	rec.status = StatusFailed
	m.records[key] = rec
	return nil
}

func (m *memoryDB) MarkNeedsConfirmation(ctx context.Context, key string, now int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.records[key]
	if !ok {
		rec = memoryRecord{createdAt: now}
	}
	rec.status = StatusNeedsConfirmation
	m.records[key] = rec
	return nil
}

// ---------------------------------------------------------------------------
// SQL 实现（database/sql，驱动由任务03注入）
// ---------------------------------------------------------------------------

// sqlDB 基于 database/sql 的 tool_idempotency 表实现。任务03 用其 SQLite
// 连接（*sql.DB）构造；SQL 与 IdempotencySchema 对应。
type sqlDB struct {
	db *sql.DB
}

// NewSQLIdempotencyDB 创建 SQL 实现。调用方负责先执行 IdempotencySchema
// （migration）并以单写队列模型使用该 *sql.DB（第10章）。
func NewSQLIdempotencyDB(db *sql.DB) IdempotencyDB {
	return &sqlDB{db: db}
}

func (s *sqlDB) Claim(ctx context.Context, key string, now, staleBefore int64) (bool, error) {
	// 原子认领：插入；冲突时仅在“非 inflight 或 inflight 已陈旧”时接管。
	res, err := s.db.ExecContext(ctx, `
INSERT INTO tool_idempotency (key, status, result, created_at)
VALUES (?, 'inflight', NULL, ?)
ON CONFLICT(key) DO UPDATE SET status = 'inflight', created_at = ?
WHERE tool_idempotency.status <> 'inflight' OR tool_idempotency.created_at < ?`,
		key, now, now, staleBefore)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *sqlDB) Get(ctx context.Context, key string) (string, []byte, int64, bool, error) {
	var (
		status    string
		result    []byte
		createdAt int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT status, result, created_at FROM tool_idempotency WHERE key = ?`, key).
		Scan(&status, &result, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, 0, false, nil
	}
	if err != nil {
		return "", nil, 0, false, err
	}
	return status, result, createdAt, true, nil
}

func (s *sqlDB) Complete(ctx context.Context, key string, result []byte, now int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE tool_idempotency SET status = 'done', result = ? WHERE key = ? AND status = 'inflight'`,
		result, key)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("tool: key %q 不是 inflight 状态，拒绝重复提交（I5）", key)
	}
	return nil
}

func (s *sqlDB) Fail(ctx context.Context, key string, now int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE tool_idempotency SET status = 'failed' WHERE key = ? AND status = 'inflight'`, key)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("tool: key %q 不是 inflight 状态", key)
	}
	return nil
}

func (s *sqlDB) MarkNeedsConfirmation(ctx context.Context, key string, now int64) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO tool_idempotency (key, status, result, created_at) VALUES (?, 'needs_confirmation', NULL, ?)
ON CONFLICT(key) DO UPDATE SET status = 'needs_confirmation'`, key, now)
	return err
}

// ---------------------------------------------------------------------------
// 结果序列化辅助
// ---------------------------------------------------------------------------

// EncodeResult 把 ToolResponse 序列化为可持久化的结果（去掉运行时字段）。
func EncodeResult(resp ToolResponse) ([]byte, error) {
	clean := ToolResponse{
		ToolCallID:  resp.ToolCallID,
		ToolName:    resp.ToolName,
		Content:     resp.Content,
		Success:     resp.Success,
		Error:       resp.Error,
		ErrorCode:   resp.ErrorCode,
		DisplayType: resp.DisplayType,
		Metadata:    resp.Metadata,
	}
	return json.Marshal(clean)
}

// DecodeResult 反序列化持久化的 ToolResponse。
func DecodeResult(data []byte) (ToolResponse, error) {
	var resp ToolResponse
	if len(data) == 0 {
		return resp, errors.New("tool: 空结果")
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return ToolResponse{}, err
	}
	return resp, nil
}
