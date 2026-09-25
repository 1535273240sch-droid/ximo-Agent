// Package tool 实现统一的工具调用协议、工具运行时、权限引擎、幂等性判定、
// DynamicTool 沙箱与审计。它是 Engine（任务02）与 Worker 池（任务05）之间的
// 中间层，也是安全红线最集中的模块。
//
// 本文件定义跨模块公共类型。任务02（Engine）与任务05（Worker池）直接消费
// ToolRequest / ToolResponse / ToolDefinition / WorkerRequest / WorkerResponse。
package tool

import (
	"context"
	"time"
)

// ---------------------------------------------------------------------------
// JSON Schema（工具参数 schema 校验用的最小子集）
// ---------------------------------------------------------------------------

// JSONSchema 是工具参数的 JSON Schema 描述，覆盖 v1 ToolDefinition.parameters
// 用到的字段：type / properties / required / enum / items / additionalProperties。
type JSONSchema struct {
	Type                 string                `json:"type"`
	Properties           map[string]JSONSchema `json:"properties,omitempty"`
	Required             []string              `json:"required,omitempty"`
	Enum                 []any                 `json:"enum,omitempty"`
	Items                *JSONSchema           `json:"items,omitempty"`
	AdditionalProperties *JSONSchema           `json:"additionalProperties,omitempty"`
	Default              any                   `json:"default,omitempty"`
	Description          string                `json:"description,omitempty"`
}

// ObjectSchema 构造一个 object 类型的 schema。
func ObjectSchema(properties map[string]JSONSchema, required ...string) JSONSchema {
	return JSONSchema{Type: "object", Properties: properties, Required: required}
}

// ---------------------------------------------------------------------------
// 风险等级 / 执行域 / 幂等分类
// ---------------------------------------------------------------------------

// RiskLevel 权限引擎 v2 的匹配维度之一，也是 Decision 的组成部分。
type RiskLevel int

const (
	// RiskUnknown 未声明风险，按 RiskMedium 处理。
	RiskUnknown RiskLevel = iota
	// RiskLow 只读、无副作用（file_read / knowledge_search）。
	RiskLow
	// RiskMedium 可逆写、有 checkpoint 回退保障（file_write / file_edit）。
	RiskMedium
	// RiskHigh 不可逆或影响面大的操作（file_delete / terminal_exec / git push）。
	RiskHigh
	// RiskCritical 系统级、凭据相关或跨信任边界的操作。
	RiskCritical
)

func (r RiskLevel) String() string {
	switch r {
	case RiskLow:
		return "low"
	case RiskMedium:
		return "medium"
	case RiskHigh:
		return "high"
	case RiskCritical:
		return "critical"
	default:
		return "unknown"
	}
}

// Valid 报告风险等级是否为已定义值。
func (r RiskLevel) Valid() bool { return r >= RiskUnknown && r <= RiskCritical }

// ExecutionDomain 描述工具在哪里执行。高风险工具绝不在 Engine 进程内运行，
// 一律路由给任务05的 Worker（第13章）。
type ExecutionDomain int

const (
	// DomainInProcess 纯 Go、无副作用的轻量工具，在 Engine 进程内由
	// safeExecute + recoverToError 隔离执行。
	DomainInProcess ExecutionDomain = iota
	// DomainWorkerBrowser 浏览器自动化 Worker。
	DomainWorkerBrowser
	// DomainWorkerTerminal 终端 Worker。
	DomainWorkerTerminal
	// DomainWorkerOffice 办公文档 Worker。
	DomainWorkerOffice
	// DomainWorkerDynamicJS 动态 JS 沙箱 Worker（goja VM 跑在 Worker 进程内）。
	DomainWorkerDynamicJS
	// DomainWorkerMCP MCP stdio / SSE / HTTP Worker。
	DomainWorkerMCP
	// DomainWorkerComputerUse 桌面操控 Worker。
	DomainWorkerComputerUse
	// DomainWorkerOCR 外部 OCR Worker。
	DomainWorkerOCR
)

// HighRisk 报告该执行域是否属于“不允许在 Engine 进程内执行”的高风险域。
func (d ExecutionDomain) HighRisk() bool { return d != DomainInProcess }

func (d ExecutionDomain) String() string {
	switch d {
	case DomainInProcess:
		return "in_process"
	case DomainWorkerBrowser:
		return "worker_browser"
	case DomainWorkerTerminal:
		return "worker_terminal"
	case DomainWorkerOffice:
		return "worker_office"
	case DomainWorkerDynamicJS:
		return "worker_dynamic_js"
	case DomainWorkerMCP:
		return "worker_mcp"
	case DomainWorkerComputerUse:
		return "worker_computer_use"
	case DomainWorkerOCR:
		return "worker_ocr"
	default:
		return "unknown"
	}
}

// IdempotencyClass 幂等性三分类（第19章）。任务02的 Engine 恢复逻辑直接消费
// 该分类：A 类可自动重试，B 类检查当前状态后决定，C 类崩溃恢复时绝不自动重复。
type IdempotencyClass int

const (
	// ClassIdempotent A 类幂等：重复执行无额外副作用，可自动重试。
	ClassIdempotent IdempotencyClass = iota
	// ClassDetectable B 类可检测幂等：执行前可检查当前状态判断是否已完成。
	ClassDetectable
	// ClassNonIdempotent C 类非幂等：崩溃恢复时绝不自动重复，标记 NEEDS_CONFIRMATION。
	ClassNonIdempotent
)

func (c IdempotencyClass) String() string {
	switch c {
	case ClassIdempotent:
		return "idempotent"
	case ClassDetectable:
		return "detectable"
	case ClassNonIdempotent:
		return "non_idempotent"
	default:
		return "unknown"
	}
}

// ---------------------------------------------------------------------------
// 模式
// ---------------------------------------------------------------------------

// Mode 会话模式，权限引擎 v2 的匹配维度之一。yolo/safe/off 是 v1 的三档
// autoModeLevel；coding/office/design 是 v1 的三种业务模式。
type Mode string

const (
	ModeYolo   Mode = "yolo"
	ModeSafe   Mode = "safe"
	ModeOff    Mode = "off"
	ModeCoding Mode = "coding"
	ModeOffice Mode = "office"
	ModeDesign Mode = "design"
)

// ---------------------------------------------------------------------------
// Tool 接口与定义
// ---------------------------------------------------------------------------

// Tool 是所有工具必须实现的接口（对应 v1 的 src/main/tools/Tool.ts，
// 第32章 Tool interface）。
type Tool interface {
	Definition() ToolDefinition
	Execute(ctx context.Context, req ToolRequest) ToolResponse
}

// ToolDefinition 工具元数据。Name/Description/Parameters 与 v1 对齐；
// Risk/Idempotency/Domain/SideEffect/Timeout 是 v2 新增字段，供权限引擎、
// 幂等性判定与运行时路由使用。
type ToolDefinition struct {
	Name        string           `json:"name"`
	Description string           `json:"description"`
	Parameters  JSONSchema       `json:"parameters"`
	Risk        RiskLevel        `json:"risk,omitempty"`
	Idempotency IdempotencyClass `json:"idempotency,omitempty"`
	Domain      ExecutionDomain  `json:"domain,omitempty"`
	// SideEffect 标记工具是否产生持久副作用（用于审计与 I5 不变量核查）。
	SideEffect bool `json:"side_effect,omitempty"`
	// Timeout 单次执行的默认超时；0 表示使用运行时默认值。
	Timeout time.Duration `json:"timeout,omitempty"`
}

// ---------------------------------------------------------------------------
// ToolRequest / ToolResponse
// ---------------------------------------------------------------------------

// ToolRequest 一次工具调用请求。RunID + ToolCallID 是幂等性 key 的输入
// （key = hash(run_id + tool_call_id)），任何 tool call 必须属于一个 run（I2）。
type ToolRequest struct {
	RunID      string         `json:"run_id"`
	ToolCallID string         `json:"tool_call_id"`
	Name       string         `json:"name"`
	Arguments  map[string]any `json:"arguments,omitempty"`
	// Mode 当前会话模式，权限匹配维度之一。
	Mode Mode `json:"mode,omitempty"`
	// SessionID 会话标识，用于 admission control 与审计。
	SessionID string `json:"session_id,omitempty"`
	// Deadline 执行截止时间；零值表示使用工具/运行时默认超时。
	Deadline time.Time `json:"deadline,omitempty"`
	// Confirmation 用户确认状态。Confirmed 为 true 时 Ask 决策可放行
	// （由 ConfirmationHandler 在交互确认后回填）。
	Confirmation Confirmation `json:"confirmation,omitempty"`
}

// Confirmation 描述一次用户确认。
type Confirmation struct {
	// Confirmed 用户是否已批准该调用（或其所属确认批次）。
	Confirmed bool `json:"confirmed,omitempty"`
	// Scope 确认范围：single（仅本次）/ session（本会话内同规则免确认）。
	Scope ConfirmationScope `json:"scope,omitempty"`
	// ApprovedRuleID 用户批准所对应的规则 ID（session 范围复用）。
	ApprovedRuleID string `json:"approved_rule_id,omitempty"`
}

// ConfirmationScope 确认范围。
type ConfirmationScope int

const (
	// ScopeSingle 仅对本次调用生效。
	ScopeSingle ConfirmationScope = iota
	// ScopeSession 本会话内对同一条规则的后续调用免确认。
	ScopeSession
)

// ToolResponse 工具执行结果。Content/Success/Error/DisplayType/Metadata 与
// v1 的 ToolResult 对齐；ErrorCode/Cached/Idempotency 是 v2 新增。
type ToolResponse struct {
	ToolCallID string `json:"tool_call_id"`
	ToolName   string `json:"tool_name"`
	Content    string `json:"content"`
	Success    bool   `json:"success"`
	Error      string `json:"error,omitempty"`
	// ErrorCode 机器可读的错误分类，Engine/Worker 据此决定重试或标记确认。
	ErrorCode ErrorCode `json:"error_code,omitempty"`
	// DisplayType UI 渲染提示（text/code/html/search-results）。
	DisplayType string         `json:"display_type,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
	// RequiresConfirmation / ConfirmationMessage 需要用户确认时由权限层回填。
	RequiresConfirmation bool   `json:"requires_confirmation,omitempty"`
	ConfirmationMessage  string `json:"confirmation_message,omitempty"`
	// Cached 为 true 表示结果来自幂等缓存，未产生新的副作用。
	Cached bool `json:"cached,omitempty"`
	// Idempotency 本次调用命中的幂等分类。
	Idempotency IdempotencyClass `json:"idempotency,omitempty"`
	// Duration 执行耗时（不含排队）。
	Duration time.Duration `json:"duration,omitempty"`
	// Domain 实际执行域（进程内 / 哪类 Worker）。
	Domain ExecutionDomain `json:"domain,omitempty"`
}

// ErrorCode 工具调用的机器可读错误分类。
type ErrorCode string

const (
	// ErrOK 无错误。
	ErrOK ErrorCode = ""
	// ErrSchemaInvalid 参数 schema 校验失败。
	ErrSchemaInvalid ErrorCode = "SCHEMA_INVALID"
	// ErrToolNotFound 工具未注册。
	ErrToolNotFound ErrorCode = "TOOL_NOT_FOUND"
	// ErrPermissionDenied 权限引擎拒绝（deny 或确认被拒/不可用）。
	ErrPermissionDenied ErrorCode = "PERMISSION_DENIED"
	// ErrNeedsConfirmation 非幂等调用崩溃恢复后需用户确认（第19章 C 类）。
	ErrNeedsConfirmation ErrorCode = "NEEDS_CONFIRMATION"
	// ErrAdmissionRejected 准入控制拒绝（队列/并发超限，reject fast）。
	ErrAdmissionRejected ErrorCode = "ADMISSION_REJECTED"
	// ErrIdempotencyInflight 同一 idempotency key 已有 in-flight 执行（I5）。
	ErrIdempotencyInflight ErrorCode = "IDEMPOTENCY_INFLIGHT"
	// ErrResourceExhausted 资源池无法预留。
	ErrResourceExhausted ErrorCode = "RESOURCE_EXHAUSTED"
	// ErrWorkerCrashed Worker 崩溃或不可用。
	ErrWorkerCrashed ErrorCode = "WORKER_CRASHED"
	// ErrWorkerTimeout Worker 执行超时。
	ErrWorkerTimeout ErrorCode = "WORKER_TIMEOUT"
	// ErrToolPanicked 进程内工具 panic（已被 safeExecute 隔离）。
	ErrToolPanicked ErrorCode = "TOOL_PANICKED"
	// ErrToolFailed 工具执行业务失败。
	ErrToolFailed ErrorCode = "TOOL_FAILED"
	// ErrNormalizationFailed 结果归一化失败。
	ErrNormalizationFailed ErrorCode = "NORMALIZATION_FAILED"
	// ErrCanceled 调用被取消（ctx canceled / deadline）。
	ErrCanceled ErrorCode = "CANCELED"
	// ErrSandboxViolation 沙箱策略违规（禁用 API / 超限）。
	ErrSandboxViolation ErrorCode = "SANDBOX_VIOLATION"
)

// IsRetryable 报告该错误码对应的调用是否可安全自动重试。
// 非幂等（C 类）失败绝不自动重试；WORKER_CRASHED 由恢复链处理。
func (c ErrorCode) IsRetryable() bool {
	switch c {
	case ErrSchemaInvalid, ErrPermissionDenied, ErrNeedsConfirmation,
		ErrIdempotencyInflight, ErrToolPanicked, ErrSandboxViolation,
		ErrNormalizationFailed, ErrCanceled:
		return false
	default:
		return true
	}
}
