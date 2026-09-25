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
