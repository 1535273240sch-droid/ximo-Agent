// Package ipcapi 定义 Engine 与 Supervisor/UI 之间的业务帧契约，并把一个
// 已装配的 Engine 暴露成 IPC 服务端处理器。
//
// 为什么单独成包：internal/ipc 只提供「帧的读写与分发」这一层机制
// （Type/Header/Payload + 收发原语），它刻意不定义业务语义。而 TypeRunSubmit /
// TypeRunResume 这些帧一旦要在两个进程之间传，就必须有共同的载荷格式。把这份
// 契约放在这里，可以让未来的 UI 进程、Supervisor 和 Engine 三边共用同一份定义，
// 而不是各自按猜测拼 JSON。
//
// 帧载荷统一用 Envelope 包装：{ok, error, data}。这样「业务失败」与「传输失败」
// 在协议层面可区分——前者是 ok=false 的正常响应，后者是帧读写错误。
package ipcapi

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/ipc"
	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// Envelope 是所有业务帧的统一载荷外壳。
type Envelope struct {
	OK    bool            `json:"ok"`
	Error string          `json:"error,omitempty"`
	Data  json.RawMessage `json:"data,omitempty"`
}

// NewOK 构造成功信封。
func NewOK(v any) (*Envelope, error) {
	if v == nil {
		return &Envelope{OK: true}, nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("ipcapi: marshal payload: %w", err)
	}
	return &Envelope{OK: true, Data: raw}, nil
}

// NewError 构造失败信封。错误文本经 types.RedactString 脱敏后才上网，
// 避免密钥通过错误消息泄露到另一个进程（第21章）。
func NewError(err error) *Envelope {
	if err == nil {
		return &Envelope{OK: true}
	}
	return &Envelope{OK: false, Error: types.RedactString(err.Error())}
}

// Decode 从信封中解出业务数据。
func (e *Envelope) Decode(dst any) error {
	if e == nil {
		return fmt.Errorf("ipcapi: empty envelope")
	}
	if !e.OK {
		if e.Error == "" {
			return fmt.Errorf("ipcapi: request failed")
		}
		return fmt.Errorf("%s", e.Error)
	}
	if len(e.Data) == 0 || dst == nil {
		return nil
	}
	return json.Unmarshal(e.Data, dst)
}

// ---------------------------------------------------------------------------
// 业务请求/响应 DTO
// ---------------------------------------------------------------------------

// SubmitPayload 是 TypeRunSubmit 的载荷。
//
// 字段只能追加，不能改动或删除已有字段：前端 frontend/src/shared/types.ts 的
// SubmitPayload 接口与本结构体是同一份契约的两侧，只改一边编译器查不出来。
type SubmitPayload struct {
	SessionID    string `json:"session_id,omitempty"`
	Prompt       string `json:"prompt"`
	SystemPrompt string `json:"system_prompt,omitempty"`
	Model        string `json:"model,omitempty"`
	LongTask     bool   `json:"long_task,omitempty"`
	Priority     string `json:"priority,omitempty"`
	MaxRounds    int    `json:"max_rounds,omitempty"`
	// PlanMode 开启后先出计划、等用户确认再执行（任务4）。此处只做追加，
	// 不改动也不删除已有字段；前端 SubmitPayload 保持同名字段。
	PlanMode bool `json:"plan_mode,omitempty"`
	// ExpertID 用户手选的专家 ID（任务3）。非空时 Engine 直接激活该专家编排，
	// 跳过「等主模型自己决定要不要调用 agent_expert 工具」这一步。同样是追加。
	ExpertID string `json:"expert_id,omitempty"`
	// ClusterSize > 0 时以「Agent 集群」模式执行：引擎按任务内容自动挑选
	// ClusterSize 位专家并行处理同一任务，再汇总产出。0 表示关闭。
	// 与 ExpertID 互斥，由前端保证；引擎侧 ExpertID 优先。
	ClusterSize int `json:"cluster_size,omitempty"`
}

// HandlePayload 是提交/查询 run 的统一响应体。
type HandlePayload struct {
	RunID     string `json:"run_id"`
	SessionID string `json:"session_id"`
	State     string `json:"state"`
}

// RunPayload 是 TypeRunStatus 的响应体。
type RunPayload struct {
	RunID        string   `json:"run_id"`
	SessionID    string   `json:"session_id"`
	State        string   `json:"state"`
	Answer       string   `json:"answer,omitempty"`
	Round        int      `json:"round"`
	Error        string   `json:"error,omitempty"`
	UncertainIDs []string `json:"uncertain_tool_calls,omitempty"`
}

// EventsPayload 是 TypeEventStream 的响应体。
type EventsPayload struct {
	RunID  string        `json:"run_id"`
	Events []types.Event `json:"events"`
	// More 为 true 表示还有更多事件，调用方应以最后一条的 Seq 继续拉取。
	More bool `json:"more"`
}

// RunIDPayload 是取消/恢复类请求的载荷。
type RunIDPayload struct {
	RunID string `json:"run_id"`
}

// DecidePayload 是 TypeRunDecide 的载荷：用户对一个停在 waiting_user 的
// 工具授权请求的答复（F5）。
//
// 与 ConfirmPlan 的区别是它回答的是「某一次工具调用能不能执行」，不是
// 「这份计划行不行」。两者用不同的帧，是为了让引擎能分别校验：把计划确认
// 当成工具授权，等于让用户无意中放行了一个他从未看过的工具调用。
//
// 字段只能追加。前端 frontend/src/shared/types.ts 的 DecidePayload 与本结构
// 体是同一契约的两侧，必须同一个提交里一起改。
type DecidePayload struct {
	RunID string `json:"run_id"`
	// CallID 定位待授权的工具调用；空值由引擎拒绝（否则无法判断批准的是哪一次）。
	CallID string `json:"call_id"`
	// Approve 为 true 表示批准执行，false 表示拒绝。拒绝不是失败：循环把
	// 「用户拒绝执行」作为工具结果喂回模型，让它换一条路。
	Approve bool `json:"approve"`
	// Remember 为 "session" 时，本次批准在同一会话内对同类调用生效。
	// 空值/"once" 表示只批准这一次。当前实现按一次处理并在回报中说明。
	Remember string `json:"remember,omitempty"`
}

// DecideResultPayload 是 TypeRunDecide 的响应体。
type DecideResultPayload struct {
	RunID   string `json:"run_id"`
	CallID  string `json:"call_id"`
	Approve bool   `json:"approve"`
	// Ok 为 true 表示决定已被接受并将推动 run 继续。
	Ok bool `json:"ok"`
}

// ---------------------------------------------------------------------------
// 记忆图（P1-c，审核文档 4.9）
// ---------------------------------------------------------------------------
//
// 这一组帧是「记忆网络视图」的数据来源。它们与 run 生命周期无关（记忆是跨 run 的
// 长期资产），因此挂在 system.memory.* 命名空间下，而不是 engine.run.*。
//
// **单一真相**：请求/响应形状的定义在 internal/types/t02_memory_graph.go，与
// `types.MemoryGraphPort` 接口放在一起——因为那一份类型同时是端口的参数类型，
// 而端口是 bootstrap 与 ipcapi 都要遵守的契约。这里只保留类型别名，不再抄第二份
// 结构体：两份形状一旦并存，改一边漏一边不会编译失败，只会让界面永远拿到零值。
//
// 与其它 DTO 同一条规则：字段只能追加；前端 frontend/src/shared/types.ts 的对应
// 接口是同一契约的另一侧，必须同一个提交里一起改。

// MemoryIDPayload 是「按节点 id 操作」类请求的载荷。
type MemoryIDPayload struct {
	NodeID string `json:"node_id"`
}

// MemoryGraphPayload 是 TypeMemoryGraph 的请求体（分页取子图）。
type MemoryGraphPayload = types.MemoryGraphRequest

// MemoryGraphResult 是 TypeMemoryGraph 的响应体。
type MemoryGraphResult = types.MemoryGraph

// MemoryGraphNode / MemoryGraphEdge / MemoryRecallEntry 是图元素。
type (
	MemoryGraphNode   = types.MemoryGraphNode
	MemoryGraphEdge   = types.MemoryGraphEdge
	MemoryRecallEntry = types.MemoryRecallEntry
)

// MemoryNodePayload 是 TypeMemoryNodeGet / node.update 的响应体。
type MemoryNodePayload = types.MemoryNodeDetail

// MemoryNodeUpdatePayload 是 TypeMemoryNodeUpdate 的请求体。
//
// 可选字段一律用指针的三态（nil = 不改）。用零值表示「清空」会把「用户没动这个
// 字段」与「用户想把它置空」混为一谈，而这是记忆数据，改错一条无法自动还原。
type MemoryNodeUpdatePayload = types.MemoryNodeUpdate

// MemoryLinkPayload 是 TypeMemoryLink 的请求体：显式建立一条边。
type MemoryLinkPayload = types.MemoryLink

// MemoryExportPayload 是 TypeMemoryExport 的响应体，同时是 TypeMemoryImport 的
// 请求体形状。
//
// 导出必须是可读的 JSON，而不是数据库文件：用户的诉求是「把我的记忆带走/备份」，
// 一个需要 SQLite 才能打开的二进制文件做不到这件事。
type MemoryExportPayload = types.MemoryExport

// MemoryImportPayload 是 TypeMemoryImport 的请求体。
type MemoryImportPayload = types.MemoryExport

// MemoryExportNode 是导出文件里的一个节点（含完整正文）。
type MemoryExportNode = types.MemoryExportNode

// ---------------------------------------------------------------------------
// 专家目录（v2.6.0）
// ---------------------------------------------------------------------------
//
// 请求/响应形状的**单一真相**在 internal/types/t02_expert_catalog.go：同一份类型
// 既是 ExpertDirectoryPort 的参数类型，也是线上 DTO。这里只保留别名，不抄第二份
// 结构体（两份形状并存时，改一边漏一边不会编译失败，只会让界面永远拿到零值）。
//
// 与其它 DTO 同一条规则：字段只能追加；frontend/src/shared/types.ts 的对应接口
// 是同一契约的另一侧，必须同一个提交里一起改。

// ExpertCardPayload 是专家目录里的单条记录。
type ExpertCardPayload = types.ExpertCard

// ExpertListPayload 是 TypeExpertList 的响应体。
type ExpertListPayload = ExpertListResult

// ExpertIDPayload 是「按专家 id 操作」类请求的载荷。
//
// 键名是 expert_id（与 SubmitPayload.expert_id 一致）：同一个概念在线上只有一种
// 拼写，免得界面在提交 run 与删除专家时用两套字段名。
type ExpertIDPayload struct {
	ExpertID string `json:"expert_id"`
}

// ExpertDeletePayload 是 TypeExpertDelete 的响应体。
type ExpertDeletePayload = ExpertDeleteResult

// MemoryGraphMutationResult 是图写操作（link / node.update / consolidate / import）
// 的统一响应：报告做了什么，而不是只说「成功」。
//
// 它**不是**别名：端口契约里 Stats 是值类型（「一定有统计」），而线上 DTO 用指针
// （「这次没带统计」要能和「统计全为 0」区分）。这一层差异由 mutationPayload 转换。
type MemoryGraphMutationResult struct {
	OK bool `json:"ok"`
	// Affected 是受影响的节点/边数量，按操作含义解释（合并数、归档数……）。
	Affected int `json:"affected,omitempty"`
	// Notes 是给用户看的补充说明（例如「3 条矛盾由较新者取代」）。
	Notes []string `json:"notes,omitempty"`
	// Stats 是操作后的计数快照，方便界面就地刷新。
	Stats *MemoryStatsPayload `json:"stats,omitempty"`
}

// MemoryStatsPayload 是记忆后端的计数快照。同样是别名：字段与
// types.MemoryStats 完全一致，分开定义只会多一处漂移点。
type MemoryStatsPayload = types.MemoryStats

// MemoryClearPayload 是 TypeMemoryClear 的请求体：清空全部记忆。
//
// Confirm 必须显式为字面量 "DELETE_ALL"，否则拒绝执行：这是一个不可撤销的操作，
// 而 IPC 参数是可以被脚本拼出来的，需要一个「人写的」确认值。
type MemoryClearPayload struct {
	Confirm string `json:"confirm"`
}

// RecoveryPayload 是恢复完成后的回报载荷。
type RecoveryPayload struct {
	// Plans 是本次恢复扫描得出的计划（含 skip）。
	Plans []types.RecoveryPlan `json:"plans"`
	// Resumed 是实际被重新推进的 run。
	Resumed []string `json:"resumed,omitempty"`
	// Failed 是无法恢复、已被标记失败的 run。
	Failed []string `json:"failed,omitempty"`
	// NeedsConfirm 是必须在用户确认后才可继续的 run（非幂等调用结果未知）。
	NeedsConfirm []string `json:"needs_confirm,omitempty"`
}

// ---------------------------------------------------------------------------
// 帧构造助手
// ---------------------------------------------------------------------------

// FrameOpts 控制构造出的帧头字段。
type FrameOpts struct {
	RequestID string
	SessionID string
	Sequence  uint64
	// Timeout 为 0 表示不设截止时间。
	Timeout time.Duration
}

// NewFrame 构造一个业务帧。
func NewFrame(msgType string, payload any, opts FrameOpts) (*ipc.Frame, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("ipcapi: marshal frame payload: %w", err)
	}
	h := ipc.FrameHeader{
		Version:   1,
		RequestID: opts.RequestID,
		SessionID: opts.SessionID,
		Sequence:  opts.Sequence,
		Type:      msgType,
	}
	if opts.Timeout > 0 {
		h.SetDeadline(time.Now().Add(opts.Timeout))
	}
	return &ipc.Frame{Header: h, Payload: raw}, nil
}

// EnvelopeFrom 把一个业务值包装成帧载荷。
func EnvelopeFrom(msgType string, v any, opts FrameOpts) (*ipc.Frame, error) {
	env, err := NewOK(v)
	if err != nil {
		return nil, err
	}
	return NewFrame(msgType, env, opts)
}

// ErrorFrame 构造一个 ok=false 的响应帧。
func ErrorFrame(msgType string, req *ipc.Frame, err error) *ipc.Frame {
	return &ipc.Frame{
		Header: ipc.FrameHeader{
			Version:   1,
			RequestID: req.Header.RequestID,
			SessionID: req.Header.SessionID,
			Sequence:  req.Header.Sequence + 1,
			Type:      msgType,
		},
		Payload: mustMarshal(NewError(err)),
	}
}

func mustMarshal(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		// 信封本身无法序列化是编程错误；退化成最小可解析载荷而不是 panic，
		// 以免一个畸形响应拖垮整个 IPC 连接。
		return []byte(`{"ok":false,"error":"ipcapi: failed to encode error envelope"}`)
	}
	return raw
}

// decodeEnvelope 解析帧载荷为信封。
func decodeEnvelope(f *ipc.Frame) (*Envelope, error) {
	if f == nil {
		return nil, fmt.Errorf("ipcapi: nil frame")
	}
	var env Envelope
	if len(f.Payload) == 0 {
		return &Envelope{OK: true}, nil
	}
	if err := json.Unmarshal(f.Payload, &env); err != nil {
		return nil, fmt.Errorf("ipcapi: decode envelope: %w", err)
	}
	return &env, nil
}

// requestSequence 是构造客户端请求帧时使用的进程内单调序号。
// 序号在协议里用于诊断与重放检测，不承担排序语义（RequestID 才是配对依据）。
func nextSequence() uint64 { return 0 }

// WithTimeout 返回带超时的 context 及其 cancel。
func WithTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		d = 15 * time.Second
	}
	return context.WithTimeout(ctx, d)
}
