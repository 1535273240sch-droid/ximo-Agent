package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ============================== 与任务 01 的 Supervisable 适配 ==============================
//
// 冲突登记（已写入任务书"需要主Agent裁决"小节）：
//
//	任务 01 的 Supervisable 定义 `Start() error`（无 ctx），
//	任务 05 的 Worker 定义 `Start(ctx context.Context) error`（有 ctx）。
//	Go 不允许同名方法有两种签名，因此**无法**让一个类型同时直接实现两个接口。
//
// 解决方案：不改任何一方签名，而是提供 Supervisable 适配器。
// 适配器把 Supervisable 的生命周期语义（Start/Heartbeat/Kill）翻译到 Worker 池语义
// （Start 全部 slot / 逐个 Health / Stop 全部 slot），二者由 08 号在集成时接线。

// Supervisable 是任务 01 的监管接口在本包的**镜像定义**。
//
// 字段与方法签名与任务 01 的 internal/supervisor.Supervisable 逐字一致。
// 任务 01 交付后，本接口应替换为类型别名：
//
//	type Supervisable = supervisor.Supervisable
//
// 由于 Go 接口是结构化的（duck typing），本包的 SupervisorAdapter 无需任何改动
// 就同时满足两个包里的 Supervisable —— 这正是用接口而非具体类型的原因。
type Supervisable interface {
	ID() string
	Kind() string // "engine" | "ui" | "worker:browser" | "worker:mcp" | ...
	Start() error
	Heartbeat() error
	Kill() error
}

// SupervisorHooks 让 Supervisor 能感知 Manager 内部事件（可选注入）。
type SupervisorHooks struct {
	// OnCrash 在 Worker 崩溃时调用（Supervisor 可据此决定是否上报/降级）。
	OnCrash func(kind, workerID string, err error)
	// OnUnhealthy 在健康检查失败时调用。
	OnUnhealthy func(kind, workerID string, err error)
}

// SupervisorAdapter 把一个 Manager 包装成任务 01 可监管的 Supervisable。
//
// 语义映射：
//
//	Start()     → Manager.Start(ctx)（ctx 来自 adapter 构造时注入的 rootCtx）
//	Heartbeat() → Manager.Health(ctx)，失败时返回 error 让 Supervisor 触发重启
//	Kill()      → Manager.Stop(ctx)，包含进程树清理
//	ID()        → adapterID
//	Kind()      → "worker"（Supervisor 侧统一归类；具体 kind 由 SlotInfos 提供）
//
// 注意 Heartbeat 的语义差异：任务 01 期望 Heartbeat 是"轻量报活"，这里实现为
// "真实健康检查"，因为 Worker 的外部进程被 kill 时，只有真实检查才能发现。
// 这与第 13 章"detect exit"的要求一致。
type SupervisorAdapter struct {
	mgr      *Manager
	id       string
	kind     string
	rootCtx  context.Context
	hooks    SupervisorHooks
	timeout  time.Duration
	mu       sync.Mutex
	started  bool
	stopping bool
}

// NewSupervisorAdapter 创建适配器。rootCtx 是 adapter 生命周期上下文。
func NewSupervisorAdapter(mgr *Manager, id string, rootCtx context.Context, hooks SupervisorHooks) *SupervisorAdapter {
	if rootCtx == nil {
		rootCtx = context.Background()
	}
	if id == "" {
		id = "worker-pool"
	}
	return &SupervisorAdapter{
		mgr: mgr, id: id, kind: "worker",
		rootCtx: rootCtx, hooks: hooks,
		timeout: 5 * time.Second,
	}
}

// ID 实现 Supervisable。
func (a *SupervisorAdapter) ID() string { return a.id }

// Kind 实现 Supervisable。返回 "worker"（不带子类型前缀），
// 具体 kind 通过 SlotInfos() 暴露，避免 Supervisor 侧解析字符串。
func (a *SupervisorAdapter) Kind() string { return a.kind }

// Start 实现 Supervisable。
func (a *SupervisorAdapter) Start() error {
	a.mu.Lock()
	if a.started {
		a.mu.Unlock()
		return nil
	}
	a.started = true
	a.mu.Unlock()
	return a.mgr.Start(a.rootCtx)
}

// Heartbeat 实现 Supervisable：做真实健康检查，而非只是报活。
//
// 为什么：Worker 的外部进程（浏览器、MCP server）被随机 kill 后，
// Manager 的主动 sweep 会走恢复链条；Supervisor 这边的 Heartbeat 也必须能
// 独立发现"整池不可用"，两层 watchdog 互相兜底（对齐验收标准"随机 kill Worker
// 后 Engine 不退出"）。
func (a *SupervisorAdapter) Heartbeat() error {
	a.mu.Lock()
	stopping := a.stopping
	a.mu.Unlock()
	if stopping {
		return nil
	}
	ctx, cancel := context.WithTimeout(a.rootCtx, a.timeout)
	defer cancel()
	err := a.mgr.Health(ctx)
	if err != nil && a.hooks.OnUnhealthy != nil {
		a.hooks.OnUnhealthy(a.kind, a.id, err)
	}
	return err
}

// Kill 实现 Supervisable：停机并清理所有外部进程树。
func (a *SupervisorAdapter) Kill() error {
	a.mu.Lock()
	a.stopping = true
	a.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return a.mgr.Stop(ctx)
}

// SlotInfos 让 Supervisor/UI 看到池内明细（哪个 kind 的哪个 slot 崩了）。
func (a *SupervisorAdapter) SlotInfos() []SlotInfo { return a.mgr.SlotInfos() }

// ============================== 与任务 04 的 Result normalization ==============================
//
// 任务 04 负责"结果归一化"（ResultNormalizer），任务 05 负责产出 WorkerResponse。
// 为了让 08 号能一眼核对两侧字段是否对得上，这里给出：
//  1. 规范化的 ToolResponse 形状（与任务 04 任务书中的字段名一致）；
//  2. 双向转换函数；
//  3. 一份可执行的一致性自检（见 contract_test.go），字段漂移会直接测试失败。

// ToolResponse 是任务 04 的 ToolResponse 在任务 05 侧的镜像定义。
// 字段命名与任务 04 任务书一致（ToolCallID/Success/Content/Error）。
type ToolResponse struct {
	ToolCallID string         `json:"toolCallId"`
	ToolName   string         `json:"toolName,omitempty"`
	Success    bool           `json:"success"`
	Content    string         `json:"content,omitempty"`
	Error      string         `json:"error,omitempty"`
	Data       map[string]any `json:"data,omitempty"`
	// ArtifactRefs 是落 CAS 后的产物引用（不含内容，第 8/21 章约束）。
	ArtifactRefs []string  `json:"artifactRefs,omitempty"`
	DisplayType  string    `json:"displayType,omitempty"`
	EndedAt      time.Time `json:"endedAt"`
}

// ArtifactStore 把 Worker 产出的大对象落 CAS，返回引用。
// 由任务 03 的 CAS 实现；未注入时使用 NoopArtifactStore（把内容留在响应里，
// 仅适用于小 payload，测试用）。
type ArtifactStore interface {
	Put(ctx context.Context, name, mime string, data []byte) (ref string, err error)
}

// NoopArtifactStore 不落盘，只返回空引用。
type NoopArtifactStore struct{}

// Put 实现 ArtifactStore。
func (NoopArtifactStore) Put(context.Context, string, string, []byte) (string, error) {
	return "", nil
}

// DefaultResultNormalizer 是 ResultNormalizer 的默认实现（任务 04 可替换）。
//
// 归一化规则：
//   - Status==ok 且 decode 成功 → Success=true，Content 取结果里的 content/text 字段，
//     否则取整个 JSON 文本；同时把结构化结果放进 Data，供 UI 渲染。
//   - 其余状态 → Success=false，Error 取 WorkerError.Message（已脱敏）。
//   - Artifacts → ArtifactRefs（只传引用）。
//
// 注意：这里**不做**幂等性判断、不做权限判定——那是任务 04 的职责。
type DefaultResultNormalizer struct {
	store ArtifactStore
}

// NewDefaultResultNormalizer 创建归一化器。
func NewDefaultResultNormalizer(store ArtifactStore) *DefaultResultNormalizer {
	if store == nil {
		store = NoopArtifactStore{}
	}
	return &DefaultResultNormalizer{store: store}
}

// Normalize 实现 ResultNormalizer。
func (n *DefaultResultNormalizer) Normalize(raw WorkerResponse) (ToolResponse, error) {
	out := ToolResponse{
		ToolCallID: raw.CallID,
		Success:    raw.OK(),
		EndedAt:    raw.EndedAt,
	}
	if !raw.OK() {
		if raw.Error != nil {
			out.Error = raw.Error.Message
			out.DisplayType = "error"
		} else {
			out.Error = "worker 返回失败但未提供错误信息"
			out.DisplayType = "error"
		}
		for _, a := range raw.Artifacts {
			if a.Ref != "" {
				out.ArtifactRefs = append(out.ArtifactRefs, a.Ref)
			}
		}
		return out, nil
	}

	if len(raw.Result) > 0 {
		// 优先抽取 content/text 作为人读内容；失败则整体作为文本。
		var probe map[string]any
		if err := json.Unmarshal(raw.Result, &probe); err == nil {
			out.Data = probe
			if s, ok := probe["content"].(string); ok {
				out.Content = s
			} else if s, ok := probe["text"].(string); ok {
				out.Content = s
			} else {
				out.Content = string(raw.Result)
			}
			if dt, ok := probe["displayType"].(string); ok {
				out.DisplayType = dt
			}
		} else {
			out.Content = string(raw.Result)
		}
	}
	if out.DisplayType == "" {
		out.DisplayType = "text"
	}
	for _, a := range raw.Artifacts {
		if a.Ref != "" {
			out.ArtifactRefs = append(out.ArtifactRefs, a.Ref)
		}
	}
	return out, nil
}

// ToWorkerRequest 把任务 04 的 ToolRequest 映射为 WorkerRequest。
//
// 这是 08 号要核对的那张映射表的可执行版本（见 contract_test.go 的一致性测试）。
// kind 由 Resolver 决定，此处不猜。
func ToWorkerRequest(toolCallID, runID, sessionID, toolName string, args json.RawMessage, timeout time.Duration, idemKey string) WorkerRequest {
	return WorkerRequest{
		CallID:         toolCallID,
		RunID:          runID,
		SessionID:      sessionID,
		Action:         toolName,
		Args:           args,
		Timeout:        timeout,
		IdempotencyKey: idemKey,
		Attempt:        1,
	}
}

// FieldMappingTable 以数据形式记录字段映射，供 08 号逐条核对（也可被文档生成器消费）。
func FieldMappingTable() []FieldMapping { return append([]FieldMapping(nil), fieldMappings...) }

// FieldMapping 描述一对跨模块字段的对应关系。
type FieldMapping struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Note  string `json:"note,omitempty"`
	Equal bool   `json:"equal"`
}

var fieldMappings = []FieldMapping{
	{"ToolRequest.ToolCallID", "WorkerRequest.CallID", "调用唯一标识，全链路沿用", true},
	{"ToolRequest.ToolName", "WorkerRequest.Action", "工具名 → Worker 内动作名，由各 Worker 映射表决定", false},
	{"ToolRequest.Arguments", "WorkerRequest.Args", "原样透传的 JSON 参数", true},
	{"ToolRequest.RunID", "WorkerRequest.RunID", "用于日志/取消传播", true},
	{"ToolRequest.SessionID", "WorkerRequest.SessionID", "用于日志/取消传播", true},
	{"ToolRequest.Timeout", "WorkerRequest.Timeout", "0 表示由 Manager 填默认值", true},
	{"（任务04计算）", "WorkerRequest.IdempotencyKey", "幂等键只透传，Worker 不做判断", true},
	{"WorkerResponse.CallID", "ToolResponse.ToolCallID", "回程对应", true},
	{"WorkerResponse.Status", "ToolResponse.Success", "Status==ok ⇔ Success==true", false},
	{"WorkerResponse.Result", "ToolResponse.Content/Data", "抽取 content/text 作 Content，整体作 Data", false},
	{"WorkerResponse.Error.Message", "ToolResponse.Error", "已脱敏的错误文本", true},
	{"WorkerResponse.Artifacts[].Ref", "ToolResponse.ArtifactRefs", "只传 CAS 引用，不传内容", true},
}

// ValidateMappingConsistency 检查映射表自身的一致性（防手工维护时写错）。
func ValidateMappingConsistency() error {
	seen := map[string]bool{}
	for _, m := range fieldMappings {
		key := m.From + "->" + m.To
		if seen[key] {
			return fmt.Errorf("字段映射重复: %s", key)
		}
		seen[key] = true
		if m.From == "" || m.To == "" {
			return errors.New("字段映射存在空字段")
		}
	}
	return nil
}
