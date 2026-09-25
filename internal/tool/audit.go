package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// EventSink 是任务03 EventStore 的消费接口（契约：
// Append(ctx, runID, eventType, payload) (seq, error)）。工具运行时把
// 审计事件与工具生命周期事件写入 durable event log。
type EventSink interface {
	Append(ctx context.Context, runID string, eventType string, payload []byte) (seq uint64, err error)
}

// SecretRedactor 是 secrets.Redactor 的结构化接口。审计与事件序列化在
// 写出前必须过一遍 Redact*，防止带 key 的对象被整体序列化进事件（I12）。
type SecretRedactor interface {
	// RedactString 替换字符串中出现的已知秘密值与高危模式。
	RedactString(s string) string
	// RedactJSON 对 JSON payload 做键名+值的深度过滤。
	RedactJSON(payload []byte) ([]byte, error)
	// RedactValue 对任意 Go 值（map/slice/标量）做深度过滤。
	RedactValue(v any) any
	// ContainsSecret 报告字符串中是否含已知秘密（用于断言/测试）。
	ContainsSecret(s string) bool
}

// ---------------------------------------------------------------------------
// 审计事件
// ---------------------------------------------------------------------------

// AuditEventType 审计事件类型。
type AuditEventType string

const (
	AuditToolStarted       AuditEventType = "tool.started"
	AuditToolCompleted     AuditEventType = "tool.completed"
	AuditToolFailed        AuditEventType = "tool.failed"
	AuditToolPanicked      AuditEventType = "tool.panicked"
	AuditPermissionDenied  AuditEventType = "permission.denied"
	AuditPermissionAsk     AuditEventType = "permission.ask"
	AuditWorkerCrashed     AuditEventType = "worker.crashed"
	AuditWorkerRecovered   AuditEventType = "worker.recovered"
	AuditSandboxViolation  AuditEventType = "sandbox.violation"
	AuditSecretAccessed    AuditEventType = "secret.accessed"
	AuditIdempotencyReplay AuditEventType = "idempotency.replay"
)

// AuditRecord 一条审计记录。所有字符串/Detail 字段在写出前都会经过脱敏过滤。
type AuditRecord struct {
	At         time.Time
	RunID      string
	SessionID  string
	ToolCallID string
	ToolName   string
	Event      AuditEventType
	// Decision 权限决策（含 RuleID/Reason，供 UI 解释）。
	Decision *Decision
	// ErrorCode 失败分类。
	ErrorCode ErrorCode
	// Risk 有效风险等级。
	Risk RiskLevel
	// Idempotency 幂等分类。
	Idempotency IdempotencyClass
	// Cached 是否命中幂等缓存（未产生新副作用）。
	Cached bool
	// Duration 执行耗时。
	Duration time.Duration
	// Detail 附加详情（会被深度脱敏）。
	Detail map[string]any
}

// AuditLogger 审计日志接口。实现必须保证：任何字段都不含明文秘密（I12）。
type AuditLogger interface {
	Log(ctx context.Context, rec AuditRecord) error
}

// auditLogger 把审计记录写入 EventSink，payload 经过 SecretRedactor 过滤。
type auditLogger struct {
	sink     EventSink
	redactor SecretRedactor
	now      func() time.Time
}

// NewAuditLogger 创建审计日志器。sink 为 nil 时审计记录仅丢弃（测试用）；
// redactor 为 nil 时落到 BasicRedactor（内置格式/键名过滤）——**绝不静默放行**
// （审核报告 B-1：fail-closed）。生产环境应注入 secrets.Manager.Redactor()。
func NewAuditLogger(sink EventSink, redactor SecretRedactor) AuditLogger {
	return &auditLogger{sink: sink, redactor: redactorOrDefault(redactor)}
}

// Log 实现 AuditLogger。写出路径：构造 payload → 深度脱敏 → JSON 序列化 →
// 二次字符串脱敏 → 写入 event log。
func (a *auditLogger) Log(ctx context.Context, rec AuditRecord) error {
	if a.sink == nil {
		return nil
	}
	payload := map[string]any{
		"event":        string(rec.Event),
		"session_id":   rec.SessionID,
		"tool_call_id": rec.ToolCallID,
		"tool_name":    rec.ToolName,
		"error_code":   string(rec.ErrorCode),
		"risk":         rec.Risk.String(),
		"idempotency":  rec.Idempotency.String(),
		"cached":       rec.Cached,
		"duration_ms":  rec.Duration.Milliseconds(),
	}
	if rec.Decision != nil {
		payload["decision"] = map[string]any{
			"effect":  rec.Decision.Effect.String(),
			"rule_id": rec.Decision.RuleID,
			"reason":  rec.Decision.Reason,
		}
	}
	if len(rec.Detail) > 0 {
		payload["detail"] = rec.Detail
	}
	if !rec.At.IsZero() {
		payload["at"] = rec.At.UTC().Format(time.RFC3339Nano)
	}

	data, err := json.Marshal(a.redactor.RedactValue(payload))
	if err != nil {
		return fmt.Errorf("tool: 序列化审计 payload 失败: %w", err)
	}
	// 兜底：对序列化结果再做一次字符串级脱敏（防止值以意外形态拼接出现）。
	if redacted, err := a.redactor.RedactJSON(data); err == nil {
		data = redacted
	}
	_, err = a.sink.Append(ctx, rec.RunID, "audit", data)
	return err
}

// EmitEvent 供运行时发送工具生命周期事件（tool.started/completed/...），
// 同样经过脱敏兜底。redactor 为 nil 时落到 BasicRedactor（fail-closed，B-1）。
func EmitEvent(ctx context.Context, sink EventSink, redactor SecretRedactor, runID, eventType string, payload map[string]any) error {
	if sink == nil {
		return nil
	}
	redactor = redactorOrDefault(redactor)
	data, err := json.Marshal(redactor.RedactValue(payload))
	if err != nil {
		return fmt.Errorf("tool: 序列化事件 payload 失败: %w", err)
	}
	if redacted, err := redactor.RedactJSON(data); err == nil {
		data = redacted
	}
	_, err = sink.Append(ctx, runID, eventType, data)
	return err
}
