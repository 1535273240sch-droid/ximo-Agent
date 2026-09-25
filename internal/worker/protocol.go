// Package worker 实现 XimoAgent Go v2 的 Worker 池（任务 05，对应原文档第 15/16/17 章，
// 以及第 13 章"高风险工具一律不进 Engine 进程"的落地）。
//
// 核心设计约束：
//
//  1. 故障隔离：browser / terminal / mcp / office / dynamic-js / computer-use 全部是
//     独立故障域的 Worker。Worker 崩溃（进程被 kill、宿主 panic、OOM）不得让 Engine 退出。
//  2. 统一契约：所有 Worker 实现同一个 Worker 接口（第 32.3 节），Manager 负责生命周期，
//     具体 Worker 只关心自己的业务动作。
//  3. 统一恢复链条（呼应第 13 章）：
//     detect exit → reject in-flight calls → persist worker failure → restart worker
//     → health check → resume queued calls
//     这条链条只在 manager.go 里实现一次，所有 Worker 类型自动复用。
//  4. 租约模型：Acquire → use → heartbeat → release；租约超时 → force cleanup → worker recycle。
//     浏览器等"重"Worker 用它把"内存异常只回收该 worker"变成结构性保证。
//  5. 所有外部进程都必须有 process-tree cleanup（procguard 子包），
//     因为"杀掉 shell 不等于杀掉 shell 的子进程"是本项目最容易漏的坑。
//
// 与相邻任务的边界：
//   - 任务 01：本包通过 contract.go 里的 Supervisable 镜像接口 + 适配器接入 Supervisor
//     watchdog；IPC 复用 FrameHeader（本文件内定义，字段与任务 01 internal/ipc 一致）。
//   - 任务 04：任务 04 定义 capability API 与 ToolRequest/ToolResponse；本包提供
//     ResultNormalizer 的默认实现与字段映射表（见 contract.go），由 08 号主 Agent 复核。
//   - 任务 07：可观测性通过本文件里的 Logger / MetricsSink / EventSink 接口注入，
//     本包不直接依赖 observability 包，避免并发开发期间互相阻塞。
package worker

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// ProtocolVersion 是 worker 协议版本。FrameHeader.Version 必须等于该值，
// 否则接收方拒绝该帧（第 8 章：所有消息必须带 version）。
const ProtocolVersion uint8 = 1

const (
	// MaxFrameHeaderBytes 是帧头 JSON 的硬上限，防止畸形长度导致内存放大。
	MaxFrameHeaderBytes = 64 << 10 // 64 KiB
	// MaxFramePayloadBytes 是单帧 payload 的默认硬上限（第 8 章 payload size limit）。
	MaxFramePayloadBytes = 8 << 20 // 8 MiB
)

// ============================== 帧协议（复用任务 01 的 IPC Frame） ==============================

// FrameHeader 与任务 01 的 internal/ipc.FrameHeader 字段完全一致。
//
// 任务 01 交付后应把本类型替换为 ipc.FrameHeader 的别名：
//
//	type FrameHeader = ipc.FrameHeader
//
// 二者字段顺序/类型刻意保持相同，替换时不需要改动任何调用点。
type FrameHeader struct {
	Version       uint8  `json:"version"`
	RequestID     string `json:"request_id"`
	SessionID     string `json:"session_id"`
	Sequence      uint64 `json:"sequence"`
	Type          string `json:"type"`
	PayloadLength uint32 `json:"payload_length"`
}

// 帧类型枚举。命名前缀 worker.* 与任务 01 的 IPC type 命名空间保持一致，
// 便于将来同一条件管道上复用（Engine 与 Worker 共用一条 Named Pipe/UDS）。
const (
	FrameTypeRequest   = "worker.request"
	FrameTypeResponse  = "worker.response"
	FrameTypeHeartbeat = "worker.heartbeat"
	FrameTypeHealth    = "worker.health"
	FrameTypeControl   = "worker.control"
	FrameTypeEvent     = "worker.event"
	FrameTypeCrash     = "worker.crash"
)

// 帧编解码错误。
var (
	ErrFramePayloadTooLarge = errors.New("worker: frame payload 超过上限")
	ErrFrameHeaderTooLarge  = errors.New("worker: frame header 超过上限")
	ErrFrameVersionMismatch = errors.New("worker: 帧协议版本不匹配")
	ErrFrameTruncated       = errors.New("worker: 帧数据被截断")
)

// FrameCodec 是长度前缀帧编解码器：
//
//	[4B big-endian headerLen][header JSON][payload]
//
// 它对底层通道无假设，因此可以直接跑在任务 01 的 Named Pipe（Windows）或
// Unix Domain Socket 上，也可以跑在 os/exec 的 stdin/stdout 上。
type FrameCodec struct {
	// MaxPayload 为 0 时使用 MaxFramePayloadBytes。
	MaxPayload uint32
}

func (c FrameCodec) maxPayload() uint32 {
	if c.MaxPayload == 0 {
		return MaxFramePayloadBytes
	}
	return c.MaxPayload
}

// WriteFrame 写出一帧。payload 超限时返回 ErrFramePayloadTooLarge，且不写出任何字节。
func (c FrameCodec) WriteFrame(w io.Writer, h FrameHeader, payload []byte) error {
	if uint32(len(payload)) > c.maxPayload() {
		return fmt.Errorf("%w: %d > %d", ErrFramePayloadTooLarge, len(payload), c.maxPayload())
	}
	h.Version = ProtocolVersion
	h.PayloadLength = uint32(len(payload))
	head, err := json.Marshal(h)
	if err != nil {
		return fmt.Errorf("worker: 编码帧头失败: %w", err)
	}
	if len(head) > MaxFrameHeaderBytes {
		return fmt.Errorf("%w: %d > %d", ErrFrameHeaderTooLarge, len(head), MaxFrameHeaderBytes)
	}
	buf := make([]byte, 4+len(head)+len(payload))
	binary.BigEndian.PutUint32(buf[:4], uint32(len(head)))
	copy(buf[4:], head)
	copy(buf[4+len(head):], payload)
	_, err = w.Write(buf)
	return err
}

// ReadFrame 读取一帧。任何畸形长度都在分配内存之前被拒绝。
func (c FrameCodec) ReadFrame(r io.Reader) (FrameHeader, []byte, error) {
	var h FrameHeader
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return h, nil, ErrFrameTruncated
		}
		return h, nil, err
	}
	headLen := binary.BigEndian.Uint32(lenBuf[:])
	if headLen == 0 || headLen > MaxFrameHeaderBytes {
		return h, nil, fmt.Errorf("%w: headerLen=%d", ErrFrameHeaderTooLarge, headLen)
	}
	head := make([]byte, headLen)
	if _, err := io.ReadFull(r, head); err != nil {
		return h, nil, ErrFrameTruncated
	}
	if err := json.Unmarshal(head, &h); err != nil {
		return h, nil, fmt.Errorf("worker: 解析帧头失败: %w", err)
	}
	if h.Version != ProtocolVersion {
		return h, nil, fmt.Errorf("%w: got=%d want=%d", ErrFrameVersionMismatch, h.Version, ProtocolVersion)
	}
	if h.PayloadLength > c.maxPayload() {
		return h, nil, fmt.Errorf("%w: %d > %d", ErrFramePayloadTooLarge, h.PayloadLength, c.maxPayload())
	}
	if h.PayloadLength == 0 {
		return h, nil, nil
	}
	payload := make([]byte, h.PayloadLength)
	if _, err := io.ReadFull(r, payload); err != nil {
		return h, nil, ErrFrameTruncated
	}
	return h, payload, nil
}

// Transport 是 Worker 与 Manager/Engine 之间的消息通道抽象。
// 开发期用 InProcTransport 直接跑通逻辑，集成期换成任务 01 的 Named Pipe/UDS 实现。
type Transport interface {
	Send(ctx context.Context, typ string, payload []byte) error
	Recv(ctx context.Context) (FrameHeader, []byte, error)
	Close() error
}

// InProcTransport 是进程内 mock 通道：开发期与单测使用，语义与真实通道一致（有界缓冲、
// ctx 取消、关闭后返回 io.EOF）。缓冲满时会阻塞而不是丢弃，避免测试里出现"静默丢帧"。
type InProcTransport struct {
	ch     chan inProcFrame
	closed chan struct{}
	once   sync.Once
}

type inProcFrame struct {
	h       FrameHeader
	payload []byte
}

// NewInProcTransport 创建进程内通道，buffer 为 0 时使用 64。
func NewInProcTransport(buffer int) *InProcTransport {
	if buffer <= 0 {
		buffer = 64
	}
	return &InProcTransport{ch: make(chan inProcFrame, buffer), closed: make(chan struct{})}
}

// Send 实现 Transport。
func (t *InProcTransport) Send(ctx context.Context, typ string, payload []byte) error {
	f := inProcFrame{h: FrameHeader{
		Version:       ProtocolVersion,
		Type:          typ,
		PayloadLength: uint32(len(payload)),
		Sequence:      uint64(time.Now().UnixNano()),
	}, payload: payload}
	select {
	case t.ch <- f:
		return nil
	case <-t.closed:
		return io.EOF
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Recv 实现 Transport。
func (t *InProcTransport) Recv(ctx context.Context) (FrameHeader, []byte, error) {
	select {
	case f := <-t.ch:
		return f.h, f.payload, nil
	case <-t.closed:
		// 尽量把缓冲里剩余的帧读完再报 EOF。
		select {
		case f := <-t.ch:
			return f.h, f.payload, nil
		default:
			return FrameHeader{}, nil, io.EOF
		}
	case <-ctx.Done():
		return FrameHeader{}, nil, ctx.Err()
	}
}

// Close 实现 Transport。重复调用安全。
func (t *InProcTransport) Close() error {
	t.once.Do(func() { close(t.closed) })
	return nil
}

// ============================== Worker 请求 / 响应 ==============================

// WorkerRequest 是 Manager/Engine 发给 Worker 的一次调用。
//
// 字段与任务 04 的 ToolRequest 的映射关系（由 08 号主 Agent 复核，见 contract.go）：
//
//	ToolRequest.ToolCallID  ↔ WorkerRequest.CallID
//	ToolRequest.ToolName    ↔ WorkerRequest.Action（工具名 → 动作名由各 Worker 内部映射表决定）
//	ToolRequest.Arguments   ↔ WorkerRequest.Args
//	ToolRequest.RunID       ↔ WorkerRequest.RunID
//	ToolRequest.SessionID   ↔ WorkerRequest.SessionID
//	ToolRequest.Timeout     ↔ WorkerRequest.Timeout
//	（幂等键由任务 04 计算，随 IdempotencyKey 透传，Worker 不使用它做判断）
type WorkerRequest struct {
	// CallID 是本次调用的唯一标识，等于任务 04 的 tool_call_id。
	CallID string `json:"call_id"`
	// RunID / SessionID 用于日志、审计与取消传播。
	RunID     string `json:"run_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	// Kind 是目标 Worker 类型（browser/terminal/...），由 Manager 校验，防止路由到错误 Worker。
	Kind string `json:"kind,omitempty"`
	// Action 是 Worker 内的具体动作名，例如 "navigate" / "exec" / "tools/call"。
	Action string `json:"action"`
	// Args 是动作参数，原样透传给 Worker。Worker 必须自己做 schema 校验。
	Args json.RawMessage `json:"args,omitempty"`
	// Timeout 为 0 时由 Manager 填充默认调用超时。
	Timeout time.Duration `json:"timeout,omitempty"`
	// IdempotencyKey 由任务 04 计算并透传；Worker 只记录，不做重试判定。
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	// LeaseID 由 Manager 在派发时填充，Worker 可据此打日志（不要用它做鉴权）。
	LeaseID string `json:"lease_id,omitempty"`
	// Attempt 是第几次尝试（1 起）。crash 恢复后重派会递增。
	Attempt int `json:"attempt,omitempty"`
	// Meta 是少量附加信息（例如 trace_id）。禁止放密钥（第 21 章红线）。
	Meta map[string]string `json:"meta,omitempty"`
}

// Bind 把 Args 反序列化到 v。Args 为空时按零值处理，不报错。
func (r WorkerRequest) Bind(v any) error {
	if len(r.Args) == 0 {
		return nil
	}
	if err := json.Unmarshal(r.Args, v); err != nil {
		return fmt.Errorf("%w: 参数解析失败: %v", ErrInvalidArgument, err)
	}
	return nil
}

// Clone 深拷贝请求，避免跨 goroutine 共享可变字段。
func (r WorkerRequest) Clone() WorkerRequest {
	c := r
	if r.Args != nil {
		c.Args = append(json.RawMessage(nil), r.Args...)
	}
	if r.Meta != nil {
		c.Meta = make(map[string]string, len(r.Meta))
		for k, v := range r.Meta {
			c.Meta[k] = v
		}
	}
	return c
}

// ResponseStatus 是调用结果的粗分类。
type ResponseStatus string

const (
	StatusOK       ResponseStatus = "ok"
	StatusError    ResponseStatus = "error"
	StatusTimeout  ResponseStatus = "timeout"
	StatusCanceled ResponseStatus = "canceled"
	StatusRejected ResponseStatus = "rejected"
	StatusCrash    ResponseStatus = "crash"
)

// WorkerResponse 是 Worker 对一次调用的回应。
//
// 与任务 04 ToolResponse 的映射：
//
//	ToolResponse.ToolCallID ↔ WorkerResponse.CallID
//	ToolResponse.Success    ↔ WorkerResponse.Status == StatusOK
//	ToolResponse.Content    ↔ WorkerResponse.Result
//	ToolResponse.Error      ↔ WorkerResponse.Error.Message
//	（Artifacts 里的二进制附件应落 CAS，只把 ref 放进 Result，见 Artifact 注释）
type WorkerResponse struct {
	CallID   string          `json:"call_id"`
	WorkerID string          `json:"worker_id"`
	Status   ResponseStatus  `json:"status"`
	Result   json.RawMessage `json:"result,omitempty"`
	Error    *WorkerError    `json:"error,omitempty"`
	// Artifacts 是本次调用的产物描述。**注意**：大对象（截图等）必须由 Worker 落 CAS，
	// 这里只放 ref/元信息，禁止把 base64 大图直接塞进响应（第 8 章 payload limit +
	// 第 21 章"秘密/大 payload 不进事件"）。
	Artifacts []Artifact `json:"artifacts,omitempty"`
	// Metrics 用于任务 07 的可观测性汇总。
	Metrics   CallMetrics `json:"metrics,omitempty"`
	StartedAt time.Time   `json:"started_at"`
	EndedAt   time.Time   `json:"ended_at"`
}

// Artifact 描述一次调用的产物（文件/截图等），只带引用不带内容。
type Artifact struct {
	Name     string `json:"name"`
	MIME     string `json:"mime,omitempty"`
	Ref      string `json:"ref,omitempty"` // CAS 引用，例如 cas://sha256:...
	SizeByte int64  `json:"size_bytes,omitempty"`
}

// CallMetrics 是单次调用的耗时/尝试统计。
type CallMetrics struct {
	QueueMillis  int64 `json:"queue_ms"`
	ExecMillis   int64 `json:"exec_ms"`
	Attempts     int   `json:"attempts"`
	OutputBytes  int64 `json:"output_bytes,omitempty"`
	Truncated    bool  `json:"truncated,omitempty"`
	ChildProcess int   `json:"child_processes,omitempty"`
}

// OK 是 StatusOK 的便捷判断。
func (r WorkerResponse) OK() bool { return r.Status == StatusOK }

// Err 把响应里的错误转成 error（无错误时返回 nil），便于调用方用 errors.Is 判断。
func (r WorkerResponse) Err() error {
	if r.Error == nil {
		return nil
	}
	return r.Error
}

// ============================== 错误模型 ==============================

// 错误码。Engine/任务 04 依赖这些码决定"是否可重试"。
const (
	// CodeInvalidArgument：参数不合法，重试无意义。
	CodeInvalidArgument = "E_INVALID_ARGUMENT"
	// CodePolicyDenied：被 TerminalPolicy / 权限策略拒绝。
	CodePolicyDenied = "E_POLICY_DENIED"
	// CodeTimeout：调用或租约超时。
	CodeTimeout = "E_TIMEOUT"
	// CodeCanceled：调用方主动取消。
	CodeCanceled = "E_CANCELED"
	// CodeUnavailable：Worker 不可用（重启中/circuit open/池为空）。
	CodeUnavailable = "E_WORKER_UNAVAILABLE"
	// CodeCrash：Worker 在调用过程中崩溃/退出。
	CodeCrash = "E_WORKER_CRASH"
	// CodeRejected：Worker 收到调用后主动拒绝（例如正在 draining）。
	CodeRejected = "E_REJECTED"
	// CodePayloadTooLarge：请求或响应超过 payload 上限。
	CodePayloadTooLarge = "E_PAYLOAD_TOO_LARGE"
	// CodeUpstream：外部依赖（MCP server、浏览器、officecli）返回错误。
	CodeUpstream = "E_UPSTREAM"
	// CodeInternal：Worker 内部错误。
	CodeInternal = "E_INTERNAL"
)

// 哨兵错误：便于调用方 errors.Is 判断，而不用解析错误码字符串。
var (
	ErrInvalidArgument  = errors.New("worker: 参数不合法")
	ErrPolicyDenied     = errors.New("worker: 被策略拒绝")
	ErrTimeout          = errors.New("worker: 调用超时")
	ErrCanceled         = errors.New("worker: 调用被取消")
	ErrUnavailable      = errors.New("worker: worker 不可用")
	ErrCrash            = errors.New("worker: worker 崩溃")
	ErrRejected         = errors.New("worker: 调用被拒绝")
	ErrPayloadTooLarge  = errors.New("worker: payload 过大")
	ErrUpstream         = errors.New("worker: 外部依赖错误")
	ErrInternal         = errors.New("worker: 内部错误")
	ErrLeaseExpired     = errors.New("worker: 租约已过期")
	ErrLeaseReleased    = errors.New("worker: 租约已释放")
	ErrWorkerNotStarted = errors.New("worker: worker 未启动")
)

// codeSentinels 把错误码映射回哨兵错误。
var codeSentinels = map[string]error{
	CodeInvalidArgument: ErrInvalidArgument,
	CodePolicyDenied:    ErrPolicyDenied,
	CodeTimeout:         ErrTimeout,
	CodeCanceled:        ErrCanceled,
	CodeUnavailable:     ErrUnavailable,
	CodeCrash:           ErrCrash,
	CodeRejected:        ErrRejected,
	CodePayloadTooLarge: ErrPayloadTooLarge,
	CodeUpstream:        ErrUpstream,
	CodeInternal:        ErrInternal,
}

// WorkerError 是可跨进程传输的错误表示。Message 必须是脱敏后的文本：
// 禁止把 API Key、完整环境变量、命令原文里的密钥片段写进来（第 21 章红线）。
type WorkerError struct {
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	Retryable bool           `json:"retryable"`
	Details   map[string]any `json:"details,omitempty"`
}

func (e *WorkerError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return e.Code + ": " + e.Message
}

// Unwrap 让 errors.Is(err, ErrTimeout) 这类判断可以穿透到哨兵错误。
func (e *WorkerError) Unwrap() error {
	if e == nil {
		return nil
	}
	return codeSentinels[e.Code]
}

// NewWorkerError 构造一个 WorkerError。
func NewWorkerError(code, msg string, retryable bool) *WorkerError {
	return &WorkerError{Code: code, Message: msg, Retryable: retryable}
}

// NewWorkerErrorf 同 NewWorkerError，带格式化。
func NewWorkerErrorf(code string, retryable bool, format string, a ...any) *WorkerError {
	return &WorkerError{Code: code, Message: fmt.Sprintf(format, a...), Retryable: retryable}
}

// WorkerErrorFromError 把任意 error 归类到标准错误码。
func WorkerErrorFromError(err error) *WorkerError {
	if err == nil {
		return nil
	}
	var we *WorkerError
	if errors.As(err, &we) {
		return we
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return NewWorkerError(CodeTimeout, err.Error(), true)
	case errors.Is(err, context.Canceled):
		return NewWorkerError(CodeCanceled, err.Error(), false)
	case errors.Is(err, ErrPolicyDenied):
		return NewWorkerError(CodePolicyDenied, err.Error(), false)
	case errors.Is(err, ErrInvalidArgument):
		return NewWorkerError(CodeInvalidArgument, err.Error(), false)
	case errors.Is(err, ErrPayloadTooLarge):
		return NewWorkerError(CodePayloadTooLarge, err.Error(), false)
	case errors.Is(err, ErrUpstream):
		return NewWorkerError(CodeUpstream, err.Error(), true)
	case errors.Is(err, ErrTimeout):
		return NewWorkerError(CodeTimeout, err.Error(), true)
	case errors.Is(err, ErrRejected):
		return NewWorkerError(CodeRejected, err.Error(), true)
	case errors.Is(err, ErrCrash):
		return NewWorkerError(CodeCrash, err.Error(), true)
	case errors.Is(err, ErrUnavailable):
		return NewWorkerError(CodeUnavailable, err.Error(), true)
	default:
		return NewWorkerError(CodeInternal, err.Error(), false)
	}
}

// ============================== Worker 契约（第 32.3 节） ==============================

// Worker 是任务 05 对外提供的统一契约（第 32.3 节原文签名，禁止改动）。
//
// 任务 04 通过 Manager.Execute / Lease.Execute 把 ToolCall 路由进来；
// 任务 01 通过 Supervisable 适配器（contract.go）把每个 Worker 纳入 watchdog。
type Worker interface {
	ID() string
	Kind() string
	Start(ctx context.Context) error
	Execute(ctx context.Context, req WorkerRequest) (WorkerResponse, error)
	Health(ctx context.Context) error
	Stop(ctx context.Context) error
}

// ProcessAware 是可选扩展：Worker 报告自己拥有的外部进程 PID，
// 供 Manager 在崩溃回收时做进程树清理与泄漏审计。实现该接口的 Worker 更安全。
type ProcessAware interface {
	// PIDs 返回当前仍存活的外部进程 PID 快照。
	PIDs() []int
}

// DrainingWorker 是可选扩展：Worker 可以提前告知 Manager 自己即将不可用
// （例如浏览器检测到内存超阈值），Manager 会安排 recycle 而不是等它崩。
type DrainingWorker interface {
	// NeedRecycle 返回非空原因时，Manager 会把该 Worker 标记为待回收。
	NeedRecycle() string
}

// State 是 Worker 实例在 Manager 内部的状态机取值。
type State int32

const (
	StateCold State = iota
	StateStarting
	StateReady
	StateDraining
	StateRestarting
	StateCircuitOpen
	StateStopped
)

func (s State) String() string {
	switch s {
	case StateCold:
		return "cold"
	case StateStarting:
		return "starting"
	case StateReady:
		return "ready"
	case StateDraining:
		return "draining"
	case StateRestarting:
		return "restarting"
	case StateCircuitOpen:
		return "circuit_open"
	case StateStopped:
		return "stopped"
	default:
		return "unknown"
	}
}

// Spec 是创建一个 Worker 所需的静态描述。Config 来自任务 01 的 config/feature flags。
type Spec struct {
	ID     string         `json:"id"`
	Kind   string         `json:"kind"`
	Config map[string]any `json:"config,omitempty"`
}

// String 读取字符串配置项。
func (s Spec) String(key, def string) string {
	if s.Config == nil {
		return def
	}
	if v, ok := s.Config[key]; ok {
		if str, ok := v.(string); ok && str != "" {
			return str
		}
	}
	return def
}

// Bool 读取布尔配置项。
func (s Spec) Bool(key string, def bool) bool {
	if s.Config == nil {
		return def
	}
	if v, ok := s.Config[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return def
}

// Int 读取整型配置项。JSON 反序列化后的数字是 float64，这里一并处理。
func (s Spec) Int(key string, def int) int {
	if s.Config == nil {
		return def
	}
	switch v := s.Config[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return int(n)
		}
	}
	return def
}

// Duration 读取时长配置项。支持 time.Duration、数字（毫秒）与 "30s" 这类字符串。
func (s Spec) Duration(key string, def time.Duration) time.Duration {
	if s.Config == nil {
		return def
	}
	switch v := s.Config[key].(type) {
	case time.Duration:
		return v
	case int:
		return time.Duration(v) * time.Millisecond
	case int64:
		return time.Duration(v) * time.Millisecond
	case float64:
		return time.Duration(v) * time.Millisecond
	case string:
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

// Strings 读取字符串数组配置项。
func (s Spec) Strings(key string) []string {
	if s.Config == nil {
		return nil
	}
	switch v := s.Config[key].(type) {
	case []string:
		return append([]string(nil), v...)
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if str, ok := item.(string); ok {
				out = append(out, str)
			}
		}
		return out
	}
	return nil
}

// ============================== 响应构造辅助（供各 Worker 子包复用） ==============================

// OKResponse 构造成功响应，result 会被 JSON 编码。
func OKResponse(req WorkerRequest, workerID string, started time.Time, result any) (WorkerResponse, error) {
	var raw json.RawMessage
	if result != nil {
		b, err := json.Marshal(result)
		if err != nil {
			return ErrorResponse(req, workerID, CodeInternal,
				fmt.Sprintf("结果编码失败: %v", err)), nil
		}
		raw = b
	}
	return WorkerResponse{
		CallID:    req.CallID,
		WorkerID:  workerID,
		Status:    StatusOK,
		Result:    raw,
		StartedAt: started,
		EndedAt:   time.Now(),
	}, nil
}

// ErrorResponse 按错误码构造失败响应。
func ErrorResponse(req WorkerRequest, workerID, code, msg string) WorkerResponse {
	now := time.Now()
	return WorkerResponse{
		CallID:    req.CallID,
		WorkerID:  workerID,
		Status:    statusForCode(code),
		Error:     NewWorkerError(code, msg, retryableForCode(code)),
		StartedAt: now,
		EndedAt:   now,
	}
}

// ErrorResponseFrom 按 error 构造失败响应（自动归类错误码）。
func ErrorResponseFrom(req WorkerRequest, workerID string, started time.Time, err error) WorkerResponse {
	we := WorkerErrorFromError(err)
	if we == nil {
		we = NewWorkerError(CodeInternal, "未知错误", false)
	}
	return WorkerResponse{
		CallID:    req.CallID,
		WorkerID:  workerID,
		Status:    statusForCode(we.Code),
		Error:     we,
		StartedAt: started,
		EndedAt:   time.Now(),
	}
}

func statusForCode(code string) ResponseStatus {
	switch code {
	case CodeTimeout:
		return StatusTimeout
	case CodeCanceled:
		return StatusCanceled
	case CodeCrash:
		return StatusCrash
	case CodeRejected, CodeUnavailable:
		return StatusRejected
	default:
		return StatusError
	}
}

func retryableForCode(code string) bool {
	switch code {
	case CodeTimeout, CodeUnavailable, CodeCrash, CodeRejected, CodeUpstream:
		return true
	default:
		return false
	}
}

// ValidateRequest 校验请求的基本合法性。Manager 在派发前调用。
func ValidateRequest(req WorkerRequest) error {
	if strings.TrimSpace(req.CallID) == "" {
		return fmt.Errorf("%w: call_id 不能为空", ErrInvalidArgument)
	}
	if strings.TrimSpace(req.Action) == "" {
		return fmt.Errorf("%w: action 不能为空", ErrInvalidArgument)
	}
	if len(req.Args) > MaxFramePayloadBytes {
		return fmt.Errorf("%w: args=%d", ErrPayloadTooLarge, len(req.Args))
	}
	return nil
}
