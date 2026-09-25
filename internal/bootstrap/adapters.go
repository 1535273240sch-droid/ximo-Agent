// Package bootstrap 是 XimoAgent v2 的装配层（composition root）。
//
// 存在的理由：internal/ 下的七个模块都只依赖「契约」（internal/ports 与本包
// 用到的各模块接口），各自对着 mock 开发和测试，因此没有一个模块负责
// 「把真实实现装到一起」。在本次补齐之前，cmd/ximo-agent/main.go 只装配了
// config/ipc/supervisor 三个包，engine/storage/tool/provider/worker 等 15 个
// 模块的代码虽然编译得进仓库，却从未被链接进任何二进制——也就是说没有一条
// 生产路径能真正跑完一次 run。
//
// 本包就是那条路径：它把 SQLite 事件库、Provider HTTP 客户端、工具运行时、
// Worker 池、Agent 循环装配成 Engine，并返回一个可 Submit/Cancel/Recover 的
// 运行实例。
//
// 设计约定：
//   - 本包只做装配与适配，不实现业务逻辑。凡遇到接口形状不一致，都在本包内
//     写适配器，绝不为了迁就去改别的模块的公开接口（避免破坏各自的测试）。
//   - 任何一个可选依赖装配失败都不应让整个引擎起不来：降级到 nil 并由 Engine
//     自身的「nil 即降级」语义处理，只有 Engine 硬要求的 Events/Provider/Tools
//     三个依赖缺失才返回错误。
package bootstrap

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/ports"
	"github.com/ximo888ok-netizen/ximo-agent/internal/storage"
	"github.com/ximo888ok-netizen/ximo-agent/internal/storage/sqlite"
	"github.com/ximo888ok-netizen/ximo-agent/internal/tool"
	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// ---------------------------------------------------------------------------
// 事件库适配：storage.EventLog -> ports.EventStore
// ---------------------------------------------------------------------------

// eventStoreAdapter 把 internal/storage 的扁平事件表（run_events，6 列）
// 适配成 Engine 消费的丰富事件流（types.Event，17 字段）。
//
// 为什么需要转换而不是类型别名：run_events 是一张只有
// (run_id, seq, event_id, event_type, payload_json, created_at) 的数据库行，
// 而 types.Event 还带 SessionID/State/Round/ToolCallID/Data/Err 等领域字段。
// storage 包里的注释声称两者「逐字段一致、装配时改成 type Event = types.Event
// 即可」，这个说法与代码不符（见 storage/contract_test.go 对 6 列的精确定长
// 断言），因此这里采用显式转换：领域字段全部序列化进 payload_json，读取时再
// 反序列化还原。这样既不动 storage 的 SQL 层，也不破坏它的列契约测试。
type eventStoreAdapter struct {
	log   *storage.EventLog
	db    *sqlite.DB
	store *storage.Store

	// provisioned 记录已经补齐过 sessions/runs 行的 run，避免每条事件都
	// 多开一次写事务。仅在进程内有效，跨进程重复装配无副作用（INSERT OR IGNORE）。
	provisionedMu sync.RWMutex
	provisioned   map[string]bool
}

// 编译期断言：适配器必须满足 Engine 的事件库端口。
var _ ports.EventStore = (*eventStoreAdapter)(nil)

func newEventStoreAdapter(db *sqlite.DB, store *storage.Store) *eventStoreAdapter {
	return &eventStoreAdapter{
		log:         storage.NewEventLog(db),
		db:          db,
		store:       store,
		provisioned: make(map[string]bool),
	}
}

// eventEnvelope 是写入 payload_json 的封装：把 types.Event 的领域字段
// 与 Data 一起存下来，读取时按此结构还原。
type eventEnvelope struct {
	SessionID    string         `json:"sessionId,omitempty"`
	SeqInRun     uint64         `json:"seqInRun,omitempty"`
	State        types.RunState `json:"state,omitempty"`
	PrevState    types.RunState `json:"prevState,omitempty"`
	Round        int            `json:"round,omitempty"`
	ToolCallID   string         `json:"toolCallId,omitempty"`
	ToolName     string         `json:"toolName,omitempty"`
	CheckpointID string         `json:"checkpointId,omitempty"`
	Message      string         `json:"message,omitempty"`
	Coalesced    int            `json:"coalesced,omitempty"`
	Err          *types.Error   `json:"err,omitempty"`
	Data         map[string]any `json:"data,omitempty"`
}

// Append 实现 ports.EventStore：序列号由存储层在写事务内分配。
//
// 这里必须先保证 sessions / runs 两行存在：run_events 有指向 runs(id) 的外键，
// 且 appendEventTx 还会更新 runs.last_seq，缺行时存储层会明确返回
// ErrRunNotFound。内存实现没有这个约束，所以各模块的测试都不会暴露它——
// 这正是「装配层」要吸收的差异。Engine 的端口契约里没有「创建 run」这一步
// （它把 run 的创建也表现为一条事件），因此由适配器负责补齐行。
func (a *eventStoreAdapter) Append(ctx context.Context, runID string, event types.Event) (uint64, error) {
	sessionID := event.SessionID
	if sessionID == "" {
		// 没有显式会话时用 runID 兜底，保证外键可满足且语义稳定。
		sessionID = runID
	}
	if err := a.ensureRunRows(ctx, runID, sessionID); err != nil {
		return 0, err
	}

	payload, err := json.Marshal(eventEnvelope{
		SessionID:    event.SessionID,
		SeqInRun:     event.SeqInRun,
		State:        event.State,
		PrevState:    event.PrevState,
		Round:        event.Round,
		ToolCallID:   event.ToolCallID,
		ToolName:     event.ToolName,
		CheckpointID: event.CheckpointID,
		Message:      event.Message,
		Coalesced:    event.Coalesced,
		Err:          event.Err,
		Data:         event.Data,
	})
	if err != nil {
		return 0, fmt.Errorf("bootstrap: marshal event payload: %w", err)
	}
	return a.log.Append(ctx, runID, string(event.Type), payload)
}

// ensureRunRows 幂等地补齐 sessions / runs 两行。
//
// 用 INSERT OR IGNORE，因此重复调用（以及跨进程的重复装配）都是安全的；
// 已处理过的 run 记在内存集合里，避免每条事件都多开一次写事务——压力测试会
// 写上万条事件，这个跳过很重要。
func (a *eventStoreAdapter) ensureRunRows(ctx context.Context, runID, sessionID string) error {
	a.provisionedMu.RLock()
	done := a.provisioned[runID]
	a.provisionedMu.RUnlock()
	if done {
		return nil
	}

	now := time.Now().UnixMilli()
	err := a.store.WithTx(ctx, func(tx storage.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO sessions (id, title, mode, created_at, updated_at) VALUES (?,?,?,?,?)`,
			sessionID, "", "default", now, now); err != nil {
			return fmt.Errorf("bootstrap: provision session %s: %w", sessionID, err)
		}
		status := "running"
		if event, ok := ctx.Value(runStatusKey{}).(string); ok && event != "" {
			status = event
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO runs (id, session_id, status, created_at, updated_at, last_seq) VALUES (?,?,?,?,?,0)`,
			runID, sessionID, status, now, now); err != nil {
			return fmt.Errorf("bootstrap: provision run %s: %w", runID, err)
		}
		return nil
	})
	if err != nil {
		return err
	}

	a.provisionedMu.Lock()
	a.provisioned[runID] = true
	a.provisionedMu.Unlock()
	return nil
}

// runStatusKey 保留给未来按生命周期事件切换 runs.status 使用。
type runStatusKey struct{}

// Read 实现 ports.EventStore。
func (a *eventStoreAdapter) Read(ctx context.Context, runID string, afterSeq uint64, limit int) ([]types.Event, error) {
	rows, err := a.log.SinceN(ctx, runID, afterSeq, limit)
	if err != nil {
		return nil, err
	}
	out := make([]types.Event, 0, len(rows))
	for _, row := range rows {
		ev, err := toDomainEvent(row)
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, nil
}

// LastSeq 实现 ports.EventStore。
//
// 语义对齐内存实现（ports/mem）：对未知 run 返回 (0, nil) 而不是错误，
// 因为 Engine 在首次提交 run 时会先问一次「上次到哪了」。
func (a *eventStoreAdapter) LastSeq(ctx context.Context, runID string) (uint64, error) {
	seq, err := a.log.LastSeq(ctx, runID)
	if err != nil {
		if errors.Is(err, storage.ErrRunNotFound) {
			return 0, nil
		}
		return 0, err
	}
	return seq, nil
}

// ListRuns 实现 ports.EventStore：恢复流程靠它发现崩溃前未完成的 run。
func (a *eventStoreAdapter) ListRuns(ctx context.Context) ([]string, error) {
	rows, err := a.db.QueryContext(ctx, `SELECT DISTINCT run_id FROM run_events ORDER BY run_id`)
	if err != nil {
		return nil, fmt.Errorf("bootstrap: list runs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("bootstrap: scan run id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// toDomainEvent 把一行 run_events 还原成 types.Event。
func toDomainEvent(row storage.Event) (types.Event, error) {
	ev := types.Event{
		Seq:       row.Seq,
		RunID:     row.RunID,
		Type:      types.EventType(row.Type),
		Timestamp: time.UnixMilli(row.CreatedAt),
	}
	if len(row.PayloadJSON) > 0 {
		var env eventEnvelope
		if err := json.Unmarshal(row.PayloadJSON, &env); err != nil {
			return types.Event{}, fmt.Errorf("bootstrap: unmarshal event payload (seq %d): %w", row.Seq, err)
		}
		ev.SessionID = env.SessionID
		ev.SeqInRun = env.SeqInRun
		ev.State = env.State
		ev.PrevState = env.PrevState
		ev.Round = env.Round
		ev.ToolCallID = env.ToolCallID
		ev.ToolName = env.ToolName
		ev.CheckpointID = env.CheckpointID
		ev.Message = env.Message
		ev.Coalesced = env.Coalesced
		ev.Err = env.Err
		ev.Data = env.Data
	}
	if ev.SeqInRun == 0 {
		ev.SeqInRun = ev.Seq
	}
	return ev, nil
}

// ---------------------------------------------------------------------------
// Outbox 适配：storage.Outbox -> ports.OutboxStore
// ---------------------------------------------------------------------------

// outboxAdapter 把事务型 outbox 适配成 Engine 的投递端口。
//
// 差异说明：storage.Outbox.Enqueue 需要调用方传入写事务（设计上要求事件与业务
// 数据同事务提交）；Engine 的端口不带事务，因此适配器自行开一个写事务。两种
// 语义的唯一区别是原子性范围，而 Engine 只把 outbox 当作「通知镜像」，真正的
// 真相源是事件库，所以这里自行开事务是安全且可接受的。
type outboxAdapter struct {
	outbox *storage.Outbox
	store  *storage.Store
	db     *sqlite.DB
}

var _ ports.OutboxStore = (*outboxAdapter)(nil)

func newOutboxAdapter(db *sqlite.DB, store *storage.Store) *outboxAdapter {
	return &outboxAdapter{
		outbox: storage.NewOutbox(db),
		store:  store,
		db:     db,
	}
}

// Enqueue 实现 ports.OutboxStore。
func (a *outboxAdapter) Enqueue(ctx context.Context, event types.Event) error {
	row, err := toStorageEvent(event)
	if err != nil {
		return err
	}
	return a.store.WithTx(ctx, func(tx storage.Tx) error {
		return a.outbox.Enqueue(ctx, tx, row)
	})
}

// Pending 实现 ports.OutboxStore。
//
// storage.PollUndelivered 会顺带把取出的批次推后重试时间（相当于认领租约），
// 比端口的纯读语义更强。这里接受该差异：Engine 仅在崩溃恢复时读取未投递事件，
// 认领行为不会造成重复投递（同一事件重复投递在 UI 侧按 seq 幂等）。
func (a *outboxAdapter) Pending(ctx context.Context, limit int) ([]types.Event, error) {
	rows, err := a.outbox.PollUndelivered(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]types.Event, 0, len(rows))
	for _, row := range rows {
		ev, err := toDomainEvent(row)
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// Ack 实现 ports.OutboxStore：端口按 (runID, seq) 定位，存储层按 event_id 删除，
// 因此先查出 event_id。
func (a *outboxAdapter) Ack(ctx context.Context, runID string, seq uint64) error {
	var eventID string
	err := a.db.QueryRowContext(ctx,
		`SELECT event_id FROM outbox WHERE run_id=? AND seq=? LIMIT 1`, runID, seq).Scan(&eventID)
	if err != nil {
		if err == sql.ErrNoRows {
			// 已投递或从未入队：按幂等语义视为成功。
			return nil
		}
		return fmt.Errorf("bootstrap: resolve outbox event id: %w", err)
	}
	return a.outbox.Ack(ctx, eventID)
}

// toStorageEvent 把领域事件压扁成 outbox 行。
func toStorageEvent(event types.Event) (storage.Event, error) {
	payload, err := json.Marshal(event)
	if err != nil {
		return storage.Event{}, fmt.Errorf("bootstrap: marshal outbox event: %w", err)
	}
	ts := event.Timestamp
	if ts.IsZero() {
		ts = time.Now()
	}
	return storage.Event{
		RunID:       event.RunID,
		Seq:         event.Seq,
		EventID:     eventIDOf(event),
		Type:        string(event.Type),
		PayloadJSON: payload,
		CreatedAt:   ts.UnixMilli(),
	}, nil
}

func eventIDOf(event types.Event) string {
	if id := event.RunID; id != "" {
		return fmt.Sprintf("%s#%d", id, event.Seq)
	}
	return fmt.Sprintf("evt#%d", event.Seq)
}

// ---------------------------------------------------------------------------
// Checkpoint 适配：SQLite kv 表 -> ports.CheckpointStore
// ---------------------------------------------------------------------------

// checkpointAdapter 用 kv 表持久化运行快照。
//
// 为什么落在 kv 而不是新建一张表：kv(key, value, updated_at) 已经能表达
// 「每个 run 一串带顺序的快照」，而新增迁移会打乱 migrations 包对
// 「版本从 1 起严格连续」的既有测试与 checksum 断言。快照本身是不透明字节
// （types 里 Payload []byte），把它塞进 kv 不损失任何语义。
//
// key 布局：ckpt:<runID>:<seq 补零> -> JSON(ports.Checkpoint)
// unsafe 标记：ckpt-unsafe:<checkpointID> -> reason
type checkpointAdapter struct {
	db  *sqlite.DB
	seq uint64 // 用于生成 ID 的进程内单调计数
}

var _ ports.CheckpointStore = (*checkpointAdapter)(nil)

func newCheckpointAdapter(db *sqlite.DB) *checkpointAdapter {
	return &checkpointAdapter{db: db}
}

const (
	ckptKeyPrefix       = "ckpt:"
	ckptUnsafeKeyPrefix = "ckpt-unsafe:"
)

// Save 实现 ports.CheckpointStore。
func (a *checkpointAdapter) Save(ctx context.Context, cp ports.Checkpoint) (string, error) {
	if cp.ID == "" {
		a.seq++
		cp.ID = fmt.Sprintf("ckpt-%s-%d", cp.RunID, cp.Seq)
	}
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = time.Now()
	}
	blob, err := json.Marshal(cp)
	if err != nil {
		return "", fmt.Errorf("bootstrap: marshal checkpoint: %w", err)
	}
	key := fmt.Sprintf("%s%s:%020d", ckptKeyPrefix, cp.RunID, cp.Seq)
	err = a.db.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO kv (key, value, updated_at) VALUES (?,?,?)
			 ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
			key, blob, time.Now().UnixMilli())
		return err
	})
	if err != nil {
		return "", fmt.Errorf("bootstrap: save checkpoint: %w", err)
	}
	return cp.ID, nil
}

// Latest 实现 ports.CheckpointStore：返回该 run 序号最大的快照。
func (a *checkpointAdapter) Latest(ctx context.Context, runID string) (*ports.Checkpoint, error) {
	prefix := ckptKeyPrefix + runID + ":"
	var raw []byte
	err := a.db.QueryRowContext(ctx,
		`SELECT value FROM kv WHERE key LIKE ? ORDER BY key DESC LIMIT 1`, prefix+"%").Scan(&raw)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("bootstrap: load latest checkpoint: %w", err)
	}
	return decodeCheckpoint(raw)
}

// Restore 实现 ports.CheckpointStore：返回 Seq <= seq 且未被标记 unsafe 的
// 最新快照——即第11章定义的「最后一个安全检查点」。
func (a *checkpointAdapter) Restore(ctx context.Context, runID string, seq uint64) (*ports.Checkpoint, error) {
	prefix := ckptKeyPrefix + runID + ":"
	upper := fmt.Sprintf("%s%s:%020d", ckptKeyPrefix, runID, seq)
	rows, err := a.db.QueryContext(ctx,
		`SELECT key, value FROM kv WHERE key LIKE ? AND key <= ? ORDER BY key DESC`,
		prefix+"%", upper)
	if err != nil {
		return nil, fmt.Errorf("bootstrap: query checkpoints: %w", err)
	}
	defer func() { _ = rows.Close() }()

	unsafe := a.unsafeSet(ctx)
	for rows.Next() {
		var key string
		var raw []byte
		if err := rows.Scan(&key, &raw); err != nil {
			return nil, fmt.Errorf("bootstrap: scan checkpoint: %w", err)
		}
		cp, err := decodeCheckpoint(raw)
		if err != nil {
			return nil, err
		}
		if unsafe[cp.ID] {
			// 在非幂等工具调用中途拍的快照不可恢复，跳过它继续往前找。
			continue
		}
		return cp, nil
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return nil, nil
}

// MarkUnsafe 实现 ports.CheckpointStore。
func (a *checkpointAdapter) MarkUnsafe(ctx context.Context, runID, checkpointID string, reason string) error {
	key := ckptUnsafeKeyPrefix + checkpointID
	err := a.db.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO kv (key, value, updated_at) VALUES (?,?,?)
			 ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
			key, []byte(reason), time.Now().UnixMilli())
		return err
	})
	if err != nil {
		return fmt.Errorf("bootstrap: mark checkpoint unsafe: %w", err)
	}
	return nil
}

func (a *checkpointAdapter) unsafeSet(ctx context.Context) map[string]bool {
	out := make(map[string]bool)
	rows, err := a.db.QueryContext(ctx, `SELECT key FROM kv WHERE key LIKE ?`, ckptUnsafeKeyPrefix+"%")
	if err != nil {
		return out
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return out
		}
		out[key[len(ckptUnsafeKeyPrefix):]] = true
	}
	return out
}

func decodeCheckpoint(raw []byte) (*ports.Checkpoint, error) {
	var cp ports.Checkpoint
	if err := json.Unmarshal(raw, &cp); err != nil {
		return nil, fmt.Errorf("bootstrap: unmarshal checkpoint: %w", err)
	}
	return &cp, nil
}

// ---------------------------------------------------------------------------
// 幂等适配：tool.Store -> ports.IdempotencyStore
// ---------------------------------------------------------------------------

// idempotencyAdapter 把工具的幂等存储适配成 Engine 恢复时使用的分类端口。
//
// 两类差异：
//  1. 类型：tool.IdempotencyClass 是 int 枚举，ports.IdempotencyClass 是字符串。
//  2. 方法：tool.Store 只有 Classify（按 tool call ID 查），没有 ClassifyByName。
//     后者在恢复期使用——崩溃可能发生在「模型已决定调用」与「事件已落库」之间，
//     此时只有工具名可用。这里用同一个 ClassPolicy 按名字查，语义与实现一致。
type idempotencyAdapter struct {
	store  *tool.Store
	policy *tool.ClassPolicy
}

var _ ports.IdempotencyStore = (*idempotencyAdapter)(nil)

func newIdempotencyAdapter(store *tool.Store) *idempotencyAdapter {
	return &idempotencyAdapter{store: store, policy: tool.DefaultClassPolicy()}
}

// Classify 实现 ports.IdempotencyStore。
//
// 端口约定：未知 call ID 必须返回 CodeNotFound，不能静默给默认值——否则会
// 掩盖「这个调用从未被记录」的事实，而恢复流程正是靠这个错误去退回到按名字
// 分类的更保守路径。
func (a *idempotencyAdapter) Classify(ctx context.Context, toolCallID string) (ports.IdempotencyClass, error) {
	class, err := a.store.Classify(ctx, toolCallID)
	if err != nil {
		if errors.Is(err, tool.ErrToolCallUnknown) {
			return "", types.NewError(types.CodeNotFound, "tool call %q is not known", toolCallID)
		}
		return "", err
	}
	return toPortClass(class), nil
}

// ClassifyByName 实现 ports.IdempotencyStore。
func (a *idempotencyAdapter) ClassifyByName(_ context.Context, toolName string) (ports.IdempotencyClass, error) {
	return toPortClass(a.policy.ClassifyTool(toolName, "")), nil
}

// toPortClass 把 tool 包的 int 枚举映射成 ports 包的字符串枚举。
func toPortClass(c tool.IdempotencyClass) ports.IdempotencyClass {
	switch c {
	case tool.ClassIdempotent:
		return ports.Idempotent
	case tool.ClassDetectable:
		return ports.Detectable
	default:
		return ports.NonIdempotent
	}
}
