package tool

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// 运行时消费的外部接口（对方任务交付前用 mock 顶上）
// ---------------------------------------------------------------------------

// ResourceKind 资源池资源种类（与任务02 scheduler.resource_pool 对应：
// browser/computer_use/terminal/office/mcp/vision/provider_requests）。
type ResourceKind string

const (
	ResourceBrowser  ResourceKind = "browser"
	ResourceComputer ResourceKind = "computer_use"
	ResourceTerminal ResourceKind = "terminal"
	ResourceOffice   ResourceKind = "office"
	ResourceMCP      ResourceKind = "mcp"
	ResourceVision   ResourceKind = "vision"
	ResourceProvider ResourceKind = "provider_requests"
	ResourceToolCall ResourceKind = "tool_call"
)

// domainResourceKind 把执行域映射到资源池种类（进程内工具占用 tool_call 槽位）。
func domainResourceKind(d ExecutionDomain) ResourceKind {
	switch d {
	case DomainWorkerBrowser:
		return ResourceBrowser
	case DomainWorkerTerminal:
		return ResourceTerminal
	case DomainWorkerOffice:
		return ResourceOffice
	case DomainWorkerMCP:
		return ResourceMCP
	case DomainWorkerComputerUse:
		return ResourceComputer
	case DomainWorkerOCR:
		return ResourceVision
	default:
		return ResourceToolCall
	}
}

// Lease 资源租约。Release 必须幂等；任何超时路径最终都要释放（I9）。
type Lease struct {
	ID         string
	Kind       ResourceKind
	Amount     int
	AcquiredAt time.Time
}

// ResourcePool 是任务02 scheduler.resource_pool 的消费接口。
type ResourcePool interface {
	// Reserve 预留 n 个某类资源；资源不足时返回错误（调用方 reject fast）。
	Reserve(ctx context.Context, kind ResourceKind, n int) (Lease, error)
	// Release 释放租约（幂等，重复释放不得 panic）。
	Release(ctx context.Context, lease Lease) error
}

// AdmissionRequest 准入控制请求。
type AdmissionRequest struct {
	SessionID  string
	RunID      string
	ToolCallID string
	ToolName   string
	Kind       ResourceKind
}

// AdmissionController 是任务02 调度器准入控制的消费接口（全局/session 层）。
type AdmissionController interface {
	// AdmitToolCall 对一次工具调用做准入判定；超限时返回错误。
	AdmitToolCall(ctx context.Context, req AdmissionRequest) error
}

// ConfirmationRequest 用户确认请求。
type ConfirmationRequest struct {
	ToolRequest
	Decision Decision
	Prompt   string
}

// ConfirmationHandler 解析 Ask 决策：向用户请求确认。返回 false 表示
// 用户拒绝或确认通道不可用（fail-closed：按拒绝处理）。
type ConfirmationHandler interface {
	RequestConfirmation(ctx context.Context, req ConfirmationRequest) (approved bool, err error)
}

// SandboxManager 是 DynamicTool 沙箱的管理接口（*Sandbox 实现）。
type SandboxManager interface {
	Run(ctx context.Context, code string, input map[string]any) (SandboxResult, error)
	Policy() SandboxPolicy
}

// IdempotencyClaimant 是运行时需要的完整幂等交互（*Store 实现）。
// IdempotencyStore 契约（Classify/Get）由任务02 消费。
type IdempotencyClaimant interface {
	IdempotencyStore
	Claim(ctx context.Context, key string) (bool, error)
	Complete(ctx context.Context, key string, result []byte) error
	Fail(ctx context.Context, key string) error
	MarkNeedsConfirmation(ctx context.Context, key string) error
	RecoverInflight(ctx context.Context, key string, class IdempotencyClass) (resumeAllowed bool, err error)
}

// CrashSink 接收 panic / worker crash 的结构化记录（任务07 可接入观测）。
type CrashSink interface {
	RecordCrash(ctx context.Context, rec CrashRecord)
}

// CrashRecord 一次崩溃记录。
type CrashRecord struct {
	At         time.Time
	Kind       string // tool_panic / worker_crash / sandbox_violation
	RunID      string
	ToolCallID string
	ToolName   string
	Domain     ExecutionDomain
	Detail     string
	Stack      []byte
}

// ---------------------------------------------------------------------------
// ToolRuntime
// ---------------------------------------------------------------------------

// ToolRuntime 是工具调用运行时。调用链固定为：
// Schema validation → Permission → Admission control → Idempotency check →
// Resource reservation → Worker execution → Result normalization →
// Durable commit → Release resource → Emit event。
// 任何一步失败都必须释放已占用资源（代码审查红线）。
type ToolRuntime struct {
	Registry     Registry
	Permission   PermissionEngine
	ResourcePool ResourcePool
	Idempotency  IdempotencyStore
	Audit        AuditLogger
	Sandbox      SandboxManager

	// Admission 准入控制（任务02 提供；nil 时跳过该步）。
	Admission AdmissionController
	// Workers 高风险工具路由（任务05 提供；nil 时高风险工具直接拒绝）。
	Workers WorkerRouter
	// Normalizer 结果归一化（nil 时用 DefaultNormalizer）。
	Normalizer ResultNormalizer
	// Confirmer Ask 决策解析（nil 时 Ask 一律按拒绝处理，fail-closed）。
	Confirmer ConfirmationHandler
	// Events 事件出口（nil 时不发事件）。
	Events EventSink
	// Redactor 秘密过滤器。nil 时落到 BasicRedactor（内置格式/键名过滤，
	// fail-closed，见审核报告 B-1）；生产环境应注入 secrets.Manager.Redactor()，
	// 它还维护已知秘密值登记表。
	Redactor SecretRedactor
	// Crashes 崩溃记录出口（nil 时丢弃）。
	Crashes CrashSink
	// DefaultTimeout 工具未声明超时时的默认执行超时。
	DefaultTimeout time.Duration
	// Now 便于测试注入。
	Now func() time.Time
}

// compile-time 检查：*Sandbox 满足 SandboxManager。
var _ SandboxManager = (*Sandbox)(nil)

// Execute 实现工具调用协议（任务02 消费）。
func (rt *ToolRuntime) Execute(ctx context.Context, req ToolRequest) ToolResponse {
	start := rt.now()
	resp := ToolResponse{ToolCallID: req.ToolCallID, ToolName: req.Name}

	if ctx == nil {
		ctx = context.Background()
	}
	// I2：任何 tool call 必须属于一个 run。
	if req.RunID == "" || req.ToolCallID == "" {
		return rt.finish(resp, ErrSchemaInvalid, "tool request 缺少 run_id/tool_call_id（I2）", start)
	}
	if err := ctx.Err(); err != nil {
		return rt.finish(resp, ErrCanceled, err.Error(), start)
	}

	// ---- 1. Schema validation ----
	t, ok := rt.Registry.Get(req.Name)
	if !ok {
		return rt.finish(resp, ErrToolNotFound, fmt.Sprintf("工具 %q 未注册", req.Name), start)
	}
	def := t.Definition()
	resp.ToolName = def.Name
	resp.Domain = def.Domain
	if err := ValidateArguments(def.Parameters, req.Arguments); err != nil {
		return rt.finish(resp, ErrSchemaInvalid, err.Error(), start)
	}

	// ---- 2. Permission ----
	decision := rt.evaluatePermission(ctx, req, def)
	resp.Metadata = map[string]any{}
	switch decision.Effect {
	case EffectDeny:
		rt.audit(ctx, req, AuditPermissionDenied, &decision, def, start, nil)
		return rt.finish(resp, ErrPermissionDenied, decision.Reason, start)
	case EffectAsk:
		approved, err := rt.requestConfirmation(ctx, req, decision, def)
		if err != nil || !approved {
			reason := decision.Reason
			if err != nil {
				reason = fmt.Sprintf("%s（确认通道异常：%v）", reason, err)
			}
			rt.audit(ctx, req, AuditPermissionAsk, &decision, def, start, map[string]any{"approved": false})
			return rt.finish(resp, ErrPermissionDenied, reason, start)
		}
		rt.audit(ctx, req, AuditPermissionAsk, &decision, def, start, map[string]any{"approved": true})
	}

	// ---- 3. Admission control ----
	kind := domainResourceKind(def.Domain)
	if rt.Admission != nil {
		if err := rt.Admission.AdmitToolCall(ctx, AdmissionRequest{
			SessionID:  req.SessionID,
			RunID:      req.RunID,
			ToolCallID: req.ToolCallID,
			ToolName:   def.Name,
			Kind:       kind,
		}); err != nil {
			return rt.finish(resp, ErrAdmissionRejected, fmt.Sprintf("准入拒绝: %v", err), start)
		}
	}

	// ---- 4. Idempotency check ----
	guard := &callGuard{rt: rt, ctx: ctx, key: Key(req.RunID, req.ToolCallID)}
	defer guard.cleanup()

	claimant, hasClaimant := rt.Idempotency.(IdempotencyClaimant)
	if !hasClaimant {
		return rt.finish(resp, ErrIdempotencyInflight, "Idempotency store 不支持 Claim/Complete（I5 无法保证），拒绝执行", start)
	}

	class := def.Idempotency
	if classified, err := claimant.Classify(ctx, req.ToolCallID); err == nil {
		// 策略表分类优先（工具声明的分类作为解析失败时的兜底）。
		class = classified
	}
	resp.Idempotency = class
	guard.class = class

	if status, result, found, err := claimant.Get(ctx, guard.key); err != nil {
		return rt.finish(resp, ErrToolFailed, fmt.Sprintf("查询幂等记录失败: %v", err), start)
	} else if found {
		switch status {
		case StatusDone:
			// 已持久化的结果：直接回放，不产生任何新副作用。
			cached, decErr := DecodeResult(result)
			if decErr != nil {
				return rt.finish(resp, ErrToolFailed, fmt.Sprintf("解析幂等结果失败: %v", decErr), start)
			}
			cached.Cached = true
			cached.Idempotency = class
			cached.Duration = rt.now().Sub(start)
			cached.Domain = def.Domain
			rt.audit(ctx, req, AuditIdempotencyReplay, &decision, def, start, map[string]any{"key": guard.key})
			return cached
		case StatusNeedsConfirmation:
			// C 类崩溃恢复：绝不自动重复。
			return rt.finish(resp, ErrNeedsConfirmation, "该调用此前未完成且为非幂等操作，需要用户确认后重试", start)
		case StatusInflight:
			resume, recErr := claimant.RecoverInflight(ctx, guard.key, class)
			if recErr != nil {
				return rt.finish(resp, ErrToolFailed, fmt.Sprintf("恢复 in-flight 记录失败: %v", recErr), start)
			}
			if !resume {
				// 区分两种“不恢复”：C 类已被标记 needs_confirmation（绝不
				// 自动重复），还是仍有其他执行持有新鲜认领（I5 冲突）。
				if status, _, found, _ := claimant.Get(ctx, guard.key); found && status == StatusNeedsConfirmation {
					return rt.finish(resp, ErrNeedsConfirmation, "该调用此前未完成且为非幂等操作，需要用户确认后重试", start)
				}
				return rt.finish(resp, ErrIdempotencyInflight, "同一 idempotency key 已有 in-flight 执行（I5：禁止并行重复副作用）", start)
			}
			guard.claimed = true
		case StatusFailed:
			// 允许重试：继续走 claim。
		}
	}

	if !guard.claimed {
		claimed, err := claimant.Claim(ctx, guard.key)
		if err != nil {
			return rt.finish(resp, ErrToolFailed, fmt.Sprintf("认领幂等 key 失败: %v", err), start)
		}
		if !claimed {
			return rt.finish(resp, ErrIdempotencyInflight, "同一 idempotency key 已被其他执行认领（I5）", start)
		}
		guard.claimed = true
	}

	// ---- 5. Resource reservation（此后任何失败都必须释放） ----
	lease, err := rt.reserve(ctx, kind, def)
	if err != nil {
		return rt.finish(resp, ErrResourceExhausted, fmt.Sprintf("资源预留失败: %v", err), start)
	}
	guard.lease = &lease

	// 执行 ctx：带截止时间，保证 I9（timeout 最终释放资源）。
	execCtx, cancel := rt.execContext(ctx, def)
	defer cancel()

	// B 类可检测幂等：执行前检查当前状态，已达标则不重复执行。
	if class == ClassDetectable {
		if checker, ok := t.(StateChecker); ok {
			state, err := checker.CheckState(execCtx, req)
			if err != nil {
				return rt.finish(resp, ErrToolFailed, fmt.Sprintf("状态检测失败: %v", err), start)
			}
			if state == StateAlreadyDone {
				synthetic := ToolResponse{
					ToolCallID:  req.ToolCallID,
					ToolName:    def.Name,
					Content:     "当前状态已满足目标，无需重复执行",
					Success:     true,
					Cached:      true,
					Idempotency: class,
					Domain:      def.Domain,
					Duration:    rt.now().Sub(start),
					Metadata:    map[string]any{"state_check": "already_done"},
				}
				return rt.commit(guard, claimant, synthetic, req, &decision, def, start)
			}
		}
	}

	rt.emit(ctx, req.RunID, string(AuditToolStarted), map[string]any{
		"tool_call_id": req.ToolCallID,
		"tool_name":    def.Name,
		"domain":       def.Domain.String(),
		"risk":         decision.Risk.String(),
	})

	// ---- 6. Execution（进程内 panic 隔离 / Worker 路由） ----
	var raw WorkerResponse
	var execErr error
	if def.Domain.HighRisk() {
		raw, execErr = rt.executeOnWorker(execCtx, req, def)
	} else {
		raw, execErr = rt.executeInProcess(execCtx, t, req, def)
	}
	if execErr != nil {
		return rt.failExecution(guard, claimant, req, &decision, def, start, execErr, ErrOK)
	}
	// 取消/超时结果绝不作为 durable result 提交：副作用可能已部分发生，
	// 必须走失败路径（A/B 类可重试，C 类标记 needs_confirmation）。
	if !raw.Success && (raw.ErrorCode == ErrCanceled || raw.ErrorCode == ErrWorkerTimeout) {
		cause := errors.New(raw.Error)
		if cause.Error() == "" {
			cause = context.Canceled
		}
		return rt.failExecution(guard, claimant, req, &decision, def, start, cause, raw.ErrorCode)
	}

	// ---- 7. Result normalization ----
	normalizer := rt.Normalizer
	if normalizer == nil {
		normalizer = NewDefaultNormalizer(0)
	}
	normalized, err := normalizer.Normalize(raw)
	if err != nil {
		return rt.failExecution(guard, claimant, req, &decision, def, start,
			fmt.Errorf("结果归一化失败: %w", err), ErrNormalizationFailed)
	}
	normalized.Domain = def.Domain
	normalized.Idempotency = class
	normalized.Duration = rt.now().Sub(start)
	if len(normalized.Metadata) == 0 {
		normalized.Metadata = map[string]any{}
	}
	normalized.Metadata["risk"] = decision.Risk.String()

	// ---- 8. Durable commit（I5：同一 key 只提交一次） ----
	return rt.commit(guard, claimant, normalized, req, &decision, def, start)
}

// commit 完成 durable commit → 释放资源 → 发事件 → 审计。
func (rt *ToolRuntime) commit(guard *callGuard, claimant IdempotencyClaimant, resp ToolResponse, req ToolRequest, decision *Decision, def ToolDefinition, start time.Time) ToolResponse {
	resultBytes, err := EncodeResult(resp)
	if err != nil {
		return rt.failExecution(guard, claimant, req, decision, def, start, fmt.Errorf("序列化结果失败: %w", err), ErrNormalizationFailed)
	}
	// 落库前兜底脱敏：工具结果可能意外包含密钥（第21章：数据库不存明文 api_key）。
	// 返回给调用方的内存副本保持原样（LLM 需要真实输出），持久化副本脱敏。
	// nil Redactor 也会落到 BasicRedactor，不过滤的路径不存在（B-1）。
	if redacted, redactErr := redactorOrDefault(rt.Redactor).RedactJSON(resultBytes); redactErr == nil {
		resultBytes = redacted
	}
	if err := claimant.Complete(guard.ctx, guard.key, resultBytes); err != nil {
		// commit 失败：保留 inflight 认领交给恢复流程（C 类将标记
		// needs_confirmation），绝不 Fail 后重试造成双重副作用（I5）。
		guard.commitFailed = true
		return rt.failExecution(guard, claimant, req, decision, def, start,
			fmt.Errorf("durable commit 失败（调用处于不确定状态，需恢复流程处理）: %w", err), ErrNormalizationFailed)
	}
	guard.completed = true
	resp.Duration = rt.now().Sub(start)

	// ---- 9. Release resource（guard.cleanup 也会兜底释放） ----
	guard.releaseLease()

	// ---- 10. Emit event + audit ----
	rt.emit(guard.ctx, req.RunID, "tool.completed", map[string]any{
		"tool_call_id": req.ToolCallID,
		"tool_name":    def.Name,
		"success":      resp.Success,
		"cached":       resp.Cached,
		"duration_ms":  resp.Duration.Milliseconds(),
	})
	rt.audit(guard.ctx, req, AuditToolCompleted, decision, def, start, map[string]any{
		"success": resp.Success,
		"cached":  resp.Cached,
	})
	return resp
}

// failExecution 统一失败路径：记录审计、释放资源、按需释放认领。
func (rt *ToolRuntime) failExecution(guard *callGuard, claimant IdempotencyClaimant, req ToolRequest, decision *Decision, def ToolDefinition, start time.Time, cause error, hint ErrorCode) ToolResponse {
	resp := ToolResponse{
		ToolCallID:  req.ToolCallID,
		ToolName:    def.Name,
		Success:     false,
		Error:       cause.Error(),
		ErrorCode:   ErrToolFailed,
		Domain:      def.Domain,
		Idempotency: def.Idempotency,
		Duration:    rt.now().Sub(start),
		Metadata:    map[string]any{},
	}
	var workerUnavailable *ErrWorkerUnavailable
	if errors.As(cause, &workerUnavailable) {
		resp.ErrorCode = ErrWorkerCrashed
	}
	var toolPanic *ToolPanicError
	if errors.As(cause, &toolPanic) {
		resp.ErrorCode = ErrToolPanicked
	}
	if errors.Is(cause, context.DeadlineExceeded) {
		resp.ErrorCode = ErrWorkerTimeout
	}
	if errors.Is(cause, context.Canceled) {
		resp.ErrorCode = ErrCanceled
	}
	var violation *SandboxViolationError
	if errors.As(cause, &violation) {
		resp.ErrorCode = ErrSandboxViolation
	}
	if resp.ErrorCode == ErrToolFailed && hint != ErrOK {
		resp.ErrorCode = hint
	}

	event := AuditToolFailed
	if resp.ErrorCode == ErrWorkerCrashed {
		event = AuditWorkerCrashed
	}
	if resp.ErrorCode == ErrSandboxViolation {
		event = AuditSandboxViolation
	}
	rt.audit(guard.ctx, req, event, decision, def, start, map[string]any{"error": cause.Error()})
	guard.cleanup()
	return resp
}

// finish 用于尚未取得任何资源的前置失败路径（schema/权限/准入/幂等查询）。
func (rt *ToolRuntime) finish(resp ToolResponse, code ErrorCode, reason string, start time.Time) ToolResponse {
	resp.Success = false
	resp.ErrorCode = code
	resp.Error = reason
	resp.Duration = rt.now().Sub(start)
	if resp.Metadata == nil {
		resp.Metadata = map[string]any{}
	}
	return resp
}

// ---------------------------------------------------------------------------
// 执行：进程内（panic 隔离）与 Worker 路由
// ---------------------------------------------------------------------------

// executeInProcess 在 Engine 进程内执行轻量工具，panic 被隔离为错误。
func (rt *ToolRuntime) executeInProcess(ctx context.Context, t Tool, req ToolRequest, def ToolDefinition) (raw WorkerResponse, err error) {
	resp, panicked, panicErr := safeExecute(ctx, t, req)
	if panicked {
		rt.recordCrash(ctx, CrashRecord{
			Kind:       "tool_panic",
			RunID:      req.RunID,
			ToolCallID: req.ToolCallID,
			ToolName:   def.Name,
			Domain:     def.Domain,
			Detail:     panicErr.Error(),
		})
		rt.audit(ctx, req, AuditToolPanicked, nil, def, rt.now(), map[string]any{"panic": panicErr.Error()})
		return WorkerResponse{}, &ToolPanicError{ToolName: def.Name, Cause: panicErr}
	}
	return WorkerResponse{
		ToolCallID: resp.ToolCallID,
		ToolName:   resp.ToolName,
		Content:    resp.Content,
		Success:    resp.Success,
		Error:      resp.Error,
		ErrorCode:  resp.ErrorCode,
		Metadata:   resp.Metadata,
	}, nil
}

// executeOnWorker 把高风险工具路由给 Worker（任务05）。
func (rt *ToolRuntime) executeOnWorker(ctx context.Context, req ToolRequest, def ToolDefinition) (WorkerResponse, error) {
	if rt.Workers == nil {
		return WorkerResponse{}, &ErrWorkerUnavailable{Domain: def.Domain, Reason: "未配置 Worker 路由，高风险工具拒绝在进程内执行"}
	}
	wreq := WorkerRequest{
		RunID:      req.RunID,
		ToolCallID: req.ToolCallID,
		ToolName:   def.Name,
		Domain:     def.Domain,
		Arguments:  req.Arguments,
		Deadline:   req.Deadline,
	}
	// 动态 JS 工具附带沙箱策略，Worker 侧宿主施加同样的硬限制。
	if def.Domain == DomainWorkerDynamicJS && rt.Sandbox != nil {
		policy := rt.Sandbox.Policy()
		wreq.SandboxPolicy = &policy
	}
	return rt.Workers.Route(ctx, wreq)
}

// ToolPanicError 进程内工具 panic（已被 safeExecute 隔离，不会扩散）。
type ToolPanicError struct {
	ToolName string
	Cause    error
}

func (e *ToolPanicError) Error() string {
	return fmt.Sprintf("工具 %q 执行时 panic（已隔离）: %v", e.ToolName, e.Cause)
}

func (e *ToolPanicError) Unwrap() error { return e.Cause }

// safeExecute 用 defer recover 隔离单个工具的 panic（第13章）。
// 只允许在“单个工具 goroutine/单个请求”边界使用；Engine 核心 goroutine
// 的 panic 不在此列（那必须走 Supervisor 重启）。
func safeExecute(ctx context.Context, t Tool, req ToolRequest) (resp ToolResponse, panicked bool, panicErr error) {
	defer func() {
		if r := recover(); r != nil {
			panicked = true
			panicErr = recoverToError(r)
		}
	}()
	resp = t.Execute(ctx, req)
	if resp.ToolCallID == "" {
		resp.ToolCallID = req.ToolCallID
	}
	if resp.ToolName == "" {
		resp.ToolName = req.Name
	}
	return resp, false, nil
}

// recoverToError 把 recover 到的任意值转换为 error。
func recoverToError(r any) error {
	if err, ok := r.(error); ok {
		return fmt.Errorf("tool panic: %w", err)
	}
	return fmt.Errorf("tool panic: %v", r)
}

// ---------------------------------------------------------------------------
// 资源与超时管理
// ---------------------------------------------------------------------------

func (rt *ToolRuntime) reserve(ctx context.Context, kind ResourceKind, def ToolDefinition) (Lease, error) {
	if rt.ResourcePool == nil {
		return Lease{}, nil
	}
	return rt.ResourcePool.Reserve(ctx, kind, 1)
}

func (rt *ToolRuntime) releaseLease(ctx context.Context, lease Lease) {
	if rt.ResourcePool == nil || lease.ID == "" {
		return
	}
	// Release 必须幂等；错误只进审计，不改变调用结果。
	if err := rt.ResourcePool.Release(ctx, lease); err != nil {
		rt.emit(ctx, "", "tool.lease_release_failed", map[string]any{
			"lease_id": lease.ID,
			"kind":     string(lease.Kind),
			"error":    err.Error(),
		})
	}
}

// execContext 为执行派生带截止时间的 ctx（I9：所有 timeout 最终必须释放资源）。
// 父 ctx 的截止时间更早时自动生效（context.WithTimeout 取更早者）。
func (rt *ToolRuntime) execContext(ctx context.Context, def ToolDefinition) (context.Context, context.CancelFunc) {
	timeout := def.Timeout
	if timeout <= 0 {
		timeout = rt.DefaultTimeout
	}
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return context.WithTimeout(ctx, timeout)
}

// ---------------------------------------------------------------------------
// 权限评估
// ---------------------------------------------------------------------------

func (rt *ToolRuntime) now() time.Time {
	if rt.Now != nil {
		return rt.Now()
	}
	return time.Now()
}

func (rt *ToolRuntime) evaluatePermission(ctx context.Context, req ToolRequest, def ToolDefinition) Decision {
	if rt.Permission == nil {
		// 没有权限引擎时 fail-closed。
		return Decision{Effect: EffectDeny, Reason: "未配置权限引擎，拒绝执行（fail-closed）", Risk: riskOf(def)}
	}
	return rt.Permission.Evaluate(ctx, PermissionRequest{
		Tool:           def.Name,
		Action:         actionOf(def.Name, req.Arguments),
		Path:           stringArg(req.Arguments, "filePath", "path", "repoPath"),
		Host:           stringArg(req.Arguments, "host", "url", "target"),
		Port:           intArg(req.Arguments, "port"),
		Command:        stringArg(req.Arguments, "command", "cmd"),
		Mode:           req.Mode,
		Risk:           riskOf(def),
		Confirmed:      req.Confirmation.Confirmed,
		ApprovedRuleID: req.Confirmation.ApprovedRuleID,
		Scope:          req.Confirmation.Scope,
		SessionID:      req.SessionID,
		RunID:          req.RunID,
		ToolCallID:     req.ToolCallID,
		Arguments:      req.Arguments,
	})
}

func (rt *ToolRuntime) requestConfirmation(ctx context.Context, req ToolRequest, decision Decision, def ToolDefinition) (bool, error) {
	if rt.Confirmer == nil {
		return false, nil // fail-closed
	}
	prompt := decision.Reason
	if prompt == "" {
		prompt = fmt.Sprintf("工具 %s 需要用户确认", def.Name)
	}
	return rt.Confirmer.RequestConfirmation(ctx, ConfirmationRequest{
		ToolRequest: req,
		Decision:    decision,
		Prompt:      prompt,
	})
}

func riskOf(def ToolDefinition) RiskLevel {
	if def.Risk != RiskUnknown {
		return def.Risk
	}
	return DefaultRisk(def.Name, "")
}

func stringArg(args map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := args[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

func intArg(args map[string]any, keys ...string) int {
	for _, k := range keys {
		if v, ok := args[k]; ok {
			switch n := v.(type) {
			case float64:
				return int(n)
			case int:
				return n
			case int64:
				return int(n)
			}
		}
	}
	return 0
}

// ---------------------------------------------------------------------------
// 审计 / 事件 / 崩溃记录
// ---------------------------------------------------------------------------

func (rt *ToolRuntime) audit(ctx context.Context, req ToolRequest, event AuditEventType, decision *Decision, def ToolDefinition, start time.Time, detail map[string]any) {
	if rt.Audit == nil {
		return
	}
	_ = rt.Audit.Log(ctx, AuditRecord{
		At:          rt.now(),
		RunID:       req.RunID,
		SessionID:   req.SessionID,
		ToolCallID:  req.ToolCallID,
		ToolName:    def.Name,
		Event:       event,
		Decision:    decision,
		Risk:        riskOf(def),
		Idempotency: def.Idempotency,
		Duration:    rt.now().Sub(start),
		Detail:      detail,
	})
}

func (rt *ToolRuntime) emit(ctx context.Context, runID, eventType string, payload map[string]any) {
	_ = EmitEvent(ctx, rt.Events, rt.Redactor, runID, eventType, payload)
}

func (rt *ToolRuntime) recordCrash(ctx context.Context, rec CrashRecord) {
	rec.At = rt.now()
	if rt.Crashes != nil {
		rt.Crashes.RecordCrash(ctx, rec)
	}
}

// ---------------------------------------------------------------------------
// callGuard：保证任何路径都释放租约与认领
// ---------------------------------------------------------------------------

// callGuard 跟踪一次调用占用的资源（幂等认领 + 资源租约）。
// cleanup 在 defer 中调用，即使 panic 也能释放（recover 后栈展开仍会执行 defer）。
type callGuard struct {
	rt           *ToolRuntime
	ctx          context.Context
	key          string
	class        IdempotencyClass
	claimed      bool
	completed    bool
	commitFailed bool
	lease        *Lease
	once         sync.Once
}

func (g *callGuard) releaseLease() {
	if g.lease != nil {
		lease := *g.lease
		g.lease = nil
		g.rt.releaseLease(g.ctx, lease)
	}
}

// cleanup 释放全部已占用资源：
//   - 租约总是释放；
//   - A/B 类失败：认领释放为 failed（允许重试，B 类重试前会做状态检测）；
//   - C 类失败：标记 needs_confirmation——无法确定副作用是否已发生，
//     绝不自动重复（第19章）；
//   - commit 失败时保留 inflight，交给崩溃恢复流程。
func (g *callGuard) cleanup() {
	g.once.Do(func() {
		g.releaseLease()
		if !g.claimed || g.completed || g.commitFailed {
			return
		}
		claimant, ok := g.rt.Idempotency.(IdempotencyClaimant)
		if !ok {
			return
		}
		if g.class == ClassNonIdempotent {
			_ = claimant.MarkNeedsConfirmation(g.ctx, g.key)
			return
		}
		_ = claimant.Fail(g.ctx, g.key)
	})
}
