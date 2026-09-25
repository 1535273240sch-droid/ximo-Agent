// Package mock 提供 tool 包消费的外部接口的内存实现（测试替身）。
// 任务02/03/05 交付真实实现后，由 08 号主 Agent 统一接线替换。
package mock

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/tool"
)

// ---------------------------------------------------------------------------
// EventSink / AuditLogger / CrashSink
// ---------------------------------------------------------------------------

// Event 记录一次事件追加。
type Event struct {
	RunID      string
	Type       string
	Payload    []byte
	Seq        uint64
	AppendedAt time.Time
}

// EventSink 是 tool.EventSink 的内存实现（按 run 递增 seq）。
type EventSink struct {
	mu     sync.Mutex
	events []Event
	seqs   map[string]uint64
}

// NewEventSink 创建内存事件出口。
func NewEventSink() *EventSink {
	return &EventSink{seqs: make(map[string]uint64)}
}

// Append 实现 tool.EventSink。
func (s *EventSink) Append(ctx context.Context, runID string, eventType string, payload []byte) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seqs[runID]++
	seq := s.seqs[runID]
	s.events = append(s.events, Event{
		RunID:      runID,
		Type:       eventType,
		Payload:    append([]byte(nil), payload...),
		Seq:        seq,
		AppendedAt: time.Now(),
	})
	return seq, nil
}

// Events 返回全部事件（按追加顺序）。
func (s *EventSink) Events() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Event(nil), s.events...)
}

// Types 返回指定 run 的事件类型序列（按 seq 排序）。
func (s *EventSink) Types(runID string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var types []string
	for _, e := range s.events {
		if e.RunID == runID {
			types = append(types, e.Type)
		}
	}
	return types
}

// Payloads 返回指定 run 的事件 payload（按 seq 排序）。
func (s *EventSink) Payloads(runID string) [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out [][]byte
	for _, e := range s.events {
		if e.RunID == runID {
			out = append(out, e.Payload)
		}
	}
	return out
}

// AuditRecord 记录一条审计。
type AuditRecord = tool.AuditRecord

// AuditLogger 捕获审计记录（不写外部存储）。
type AuditLogger struct {
	mu      sync.Mutex
	records []AuditRecord
}

// NewAuditLogger 创建内存审计记录器。
func NewAuditLogger() *AuditLogger { return &AuditLogger{} }

// Log 实现 tool.AuditLogger。
func (a *AuditLogger) Log(ctx context.Context, rec AuditRecord) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.records = append(a.records, rec)
	return nil
}

// Records 返回全部审计记录。
func (a *AuditLogger) Records() []AuditRecord {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]AuditRecord(nil), a.records...)
}

// CrashRecord 记录一次崩溃。
type CrashRecord = tool.CrashRecord

// CrashSink 捕获崩溃记录。
type CrashSink struct {
	mu      sync.Mutex
	records []CrashRecord
}

// NewCrashSink 创建内存崩溃记录器。
func NewCrashSink() *CrashSink { return &CrashSink{} }

// RecordCrash 实现 tool.CrashSink。
func (c *CrashSink) RecordCrash(ctx context.Context, rec CrashRecord) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, rec)
}

// Records 返回全部崩溃记录。
func (c *CrashSink) Records() []CrashRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]CrashRecord(nil), c.records...)
}

// ---------------------------------------------------------------------------
// ResourcePool / AdmissionController
// ---------------------------------------------------------------------------

// ResourcePool 是 tool.ResourcePool 的内存实现：可配置容量、统计未释放租约。
type ResourcePool struct {
	mu       sync.Mutex
	capacity map[tool.ResourceKind]int
	used     map[tool.ResourceKind]int
	nextID   int
	leases   map[string]tool.Lease
	released int
	failOn   map[tool.ResourceKind]bool
}

// NewResourcePool 创建总能拿到资源的资源池（默认容量 1024/类）。
func NewResourcePool() *ResourcePool {
	return &ResourcePool{
		capacity: map[tool.ResourceKind]int{
			tool.ResourceBrowser:  4,
			tool.ResourceComputer: 2,
			tool.ResourceTerminal: 16,
			tool.ResourceOffice:   4,
			tool.ResourceMCP:      16,
			tool.ResourceVision:   8,
			tool.ResourceProvider: 32,
			tool.ResourceToolCall: 1024,
		},
		used:   make(map[tool.ResourceKind]int),
		leases: make(map[string]tool.Lease),
		failOn: make(map[tool.ResourceKind]bool),
	}
}

// SetCapacity 设置某类资源容量。
func (p *ResourcePool) SetCapacity(kind tool.ResourceKind, n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.capacity[kind] = n
}

// FailReservations 让某类资源的 Reserve 返回错误（测试资源不足路径）。
func (p *ResourcePool) FailReservations(kind tool.ResourceKind) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failOn[kind] = true
}

// Reserve 实现 tool.ResourcePool。
func (p *ResourcePool) Reserve(ctx context.Context, kind tool.ResourceKind, n int) (tool.Lease, error) {
	if err := ctx.Err(); err != nil {
		return tool.Lease{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failOn[kind] {
		return tool.Lease{}, fmt.Errorf("mock: 资源 %s 预留被配置为失败", kind)
	}
	if p.used[kind]+n > p.capacity[kind] {
		return tool.Lease{}, fmt.Errorf("mock: 资源 %s 不足（已用 %d，容量 %d）", kind, p.used[kind], p.capacity[kind])
	}
	p.used[kind] += n
	p.nextID++
	lease := tool.Lease{
		ID:         fmt.Sprintf("lease-%d", p.nextID),
		Kind:       kind,
		Amount:     n,
		AcquiredAt: time.Now(),
	}
	p.leases[lease.ID] = lease
	return lease, nil
}

// Release 实现 tool.ResourcePool（幂等）。
func (p *ResourcePool) Release(ctx context.Context, lease tool.Lease) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	existing, ok := p.leases[lease.ID]
	if !ok {
		return nil // 幂等：重复释放不报错
	}
	delete(p.leases, lease.ID)
	p.used[existing.Kind] -= existing.Amount
	p.released++
	return nil
}

// Outstanding 返回未释放的租约数量（I9 / 验收：0 未释放 lease）。
func (p *ResourcePool) Outstanding() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.leases)
}

// Released 返回累计释放次数。
func (p *ResourcePool) Released() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.released
}

// Admission 是 tool.AdmissionController 的内存实现：按 session 限流。
type Admission struct {
	mu            sync.Mutex
	maxPerSession int
	sessions      map[string]int
	failNext      bool
}

// NewAdmission 创建准入控制器（默认每 session 并发 8）。
func NewAdmission(maxPerSession int) *Admission {
	if maxPerSession <= 0 {
		maxPerSession = 8
	}
	return &Admission{maxPerSession: maxPerSession, sessions: make(map[string]int)}
}

// FailNext 让下一次 AdmitToolCall 返回错误。
func (a *Admission) FailNext() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.failNext = true
}

// AdmitToolCall 实现 tool.AdmissionController。
func (a *Admission) AdmitToolCall(ctx context.Context, req tool.AdmissionRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.failNext {
		a.failNext = false
		return fmt.Errorf("mock: 准入被配置为失败")
	}
	key := req.SessionID
	if key == "" {
		key = req.RunID
	}
	a.sessions[key]++
	if a.sessions[key] > a.maxPerSession {
		a.sessions[key]--
		return fmt.Errorf("mock: session %q 并发工具调用超过上限 %d", key, a.maxPerSession)
	}
	return nil
}

// ReleaseSession 手动释放一次准入计数（测试辅助）。
func (a *Admission) ReleaseSession(sessionID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.sessions[sessionID] > 0 {
		a.sessions[sessionID]--
	}
}

// ---------------------------------------------------------------------------
// ConfirmationHandler
// ---------------------------------------------------------------------------

// ApproveAllConfirmer 批准所有确认请求。
type ApproveAllConfirmer struct {
	mu       sync.Mutex
	requests []tool.ConfirmationRequest
}

// RequestConfirmation 实现 tool.ConfirmationHandler。
func (c *ApproveAllConfirmer) RequestConfirmation(ctx context.Context, req tool.ConfirmationRequest) (bool, error) {
	c.mu.Lock()
	c.requests = append(c.requests, req)
	c.mu.Unlock()
	return true, nil
}

// Requests 返回收到的确认请求。
func (c *ApproveAllConfirmer) Requests() []tool.ConfirmationRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]tool.ConfirmationRequest(nil), c.requests...)
}

// DenyAllConfirmer 拒绝所有确认请求。
type DenyAllConfirmer struct{}

// RequestConfirmation 实现 tool.ConfirmationHandler。
func (c *DenyAllConfirmer) RequestConfirmation(ctx context.Context, req tool.ConfirmationRequest) (bool, error) {
	return false, nil
}

// ScriptedConfirmer 按脚本返回确认结果（第 n 次请求返回 answers[n]）。
type ScriptedConfirmer struct {
	mu      sync.Mutex
	answers []bool
	errors  []error
	calls   int
}

// NewScriptedConfirmer 创建脚本化确认器。
func NewScriptedConfirmer(answers []bool, errs []error) *ScriptedConfirmer {
	return &ScriptedConfirmer{answers: answers, errors: errs}
}

// RequestConfirmation 实现 tool.ConfirmationHandler。
func (c *ScriptedConfirmer) RequestConfirmation(ctx context.Context, req tool.ConfirmationRequest) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	i := c.calls
	c.calls++
	var err error
	if i < len(c.errors) {
		err = c.errors[i]
	}
	if i < len(c.answers) {
		return c.answers[i], err
	}
	return false, err
}

// Calls 返回被调用次数。
func (c *ScriptedConfirmer) Calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// ---------------------------------------------------------------------------
// WorkerRouter / WorkerFailureStore
// ---------------------------------------------------------------------------

// EchoWorkerRouter 回显工具参数的 Worker 路由（开发期 mock）。
type EchoWorkerRouter struct {
	mu       sync.Mutex
	requests []tool.WorkerRequest
	// Handler 可覆盖默认回显行为（返回 error 模拟崩溃）。
	Handler func(ctx context.Context, req tool.WorkerRequest) (tool.WorkerResponse, error)
}

// NewEchoWorkerRouter 创建回显路由。
func NewEchoWorkerRouter() *EchoWorkerRouter { return &EchoWorkerRouter{} }

// Route 实现 tool.WorkerRouter。
func (r *EchoWorkerRouter) Route(ctx context.Context, req tool.WorkerRequest) (tool.WorkerResponse, error) {
	r.mu.Lock()
	r.requests = append(r.requests, req)
	handler := r.Handler
	r.mu.Unlock()
	if handler != nil {
		return handler(ctx, req)
	}
	args, _ := json.Marshal(req.Arguments)
	return tool.WorkerResponse{
		ToolCallID: req.ToolCallID,
		ToolName:   req.ToolName,
		Content:    fmt.Sprintf("echo(%s): %s", req.Domain, string(args)),
		Success:    true,
		Metadata:   map[string]any{"worker": "mock-echo", "domain": req.Domain.String()},
	}, nil
}

// Requests 返回收到的路由请求。
func (r *EchoWorkerRouter) Requests() []tool.WorkerRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]tool.WorkerRequest(nil), r.requests...)
}

// WorkerController 是 tool.WorkerController 的内存实现：可脚本化重启/健康检查。
type WorkerController struct {
	mu           sync.Mutex
	restarts     map[tool.ExecutionDomain]int
	healthErr    map[tool.ExecutionDomain]int // 剩余失败次数
	restartErr   map[tool.ExecutionDomain]int
	healthChecks map[tool.ExecutionDomain]int
	onRestart    func(tool.ExecutionDomain)
}

// NewWorkerController 创建控制器。
func NewWorkerController() *WorkerController {
	return &WorkerController{
		restarts:     make(map[tool.ExecutionDomain]int),
		healthErr:    make(map[tool.ExecutionDomain]int),
		restartErr:   make(map[tool.ExecutionDomain]int),
		healthChecks: make(map[tool.ExecutionDomain]int),
	}
}

// FailHealthChecks 让接下来 n 次健康检查失败。
func (c *WorkerController) FailHealthChecks(domain tool.ExecutionDomain, n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.healthErr[domain] = n
}

// FailRestarts 让接下来 n 次重启失败。
func (c *WorkerController) FailRestarts(domain tool.ExecutionDomain, n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.restartErr[domain] = n
}

// OnRestart 注册重启回调（测试观察用）。
func (c *WorkerController) OnRestart(fn func(tool.ExecutionDomain)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onRestart = fn
}

// Restart 实现 tool.WorkerController。
func (c *WorkerController) Restart(ctx context.Context, domain tool.ExecutionDomain) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.restarts[domain]++
	if c.restartErr[domain] > 0 {
		c.restartErr[domain]--
		return fmt.Errorf("mock: 重启 %s 失败", domain)
	}
	if c.onRestart != nil {
		c.onRestart(domain)
	}
	return nil
}

// Health 实现 tool.WorkerController。
func (c *WorkerController) Health(ctx context.Context, domain tool.ExecutionDomain) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.healthChecks[domain]++
	if c.healthErr[domain] > 0 {
		c.healthErr[domain]--
		return fmt.Errorf("mock: %s 健康检查失败", domain)
	}
	return nil
}

// RestartCount 返回某域重启次数。
func (c *WorkerController) RestartCount(domain tool.ExecutionDomain) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.restarts[domain]
}

// HealthCheckCount 返回某域健康检查次数。
func (c *WorkerController) HealthCheckCount(domain tool.ExecutionDomain) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.healthChecks[domain]
}

// WorkerFailure 记录一次 worker 失败。
type WorkerFailure = tool.WorkerFailure

// WorkerFailureStore 是 tool.WorkerFailureStore 的内存实现。
type WorkerFailureStore struct {
	mu        sync.Mutex
	failures  []WorkerFailure
	recovered []tool.ExecutionDomain
}

// NewWorkerFailureStore 创建失败记录存储。
func NewWorkerFailureStore() *WorkerFailureStore { return &WorkerFailureStore{} }

// RecordFailure 实现 tool.WorkerFailureStore。
func (s *WorkerFailureStore) RecordFailure(ctx context.Context, f WorkerFailure) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures = append(s.failures, f)
	return nil
}

// MarkRecovered 实现 tool.WorkerFailureStore。
func (s *WorkerFailureStore) MarkRecovered(ctx context.Context, domain tool.ExecutionDomain, recoveredAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recovered = append(s.recovered, domain)
	return nil
}

// Failures 返回全部失败记录。
func (s *WorkerFailureStore) Failures() []WorkerFailure {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]WorkerFailure(nil), s.failures...)
}

// RecoveredDomains 返回恢复过的域。
func (s *WorkerFailureStore) RecoveredDomains() []tool.ExecutionDomain {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]tool.ExecutionDomain(nil), s.recovered...)
}

// ---------------------------------------------------------------------------
// ToolCallResolver / RuleStore
// ---------------------------------------------------------------------------

// ToolCallResolver 把 toolCallID 静态映射到工具名/参数。
type ToolCallResolver struct {
	mu    sync.Mutex
	calls map[string]struct {
		Tool string
		Args map[string]any
	}
}

// NewToolCallResolver 创建解析器。
func NewToolCallResolver() *ToolCallResolver {
	return &ToolCallResolver{calls: make(map[string]struct {
		Tool string
		Args map[string]any
	})}
}

// Add 登记一个 tool call。
func (r *ToolCallResolver) Add(toolCallID, toolName string, args map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls[toolCallID] = struct {
		Tool string
		Args map[string]any
	}{Tool: toolName, Args: args}
}

// ResolveToolCall 实现 tool.ToolCallResolver。
func (r *ToolCallResolver) ResolveToolCall(ctx context.Context, toolCallID string) (string, map[string]any, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.calls[toolCallID]
	if !ok {
		return "", nil, tool.ErrToolCallUnknown
	}
	return entry.Tool, entry.Args, nil
}

// RuleStore 是 tool.RuleStore 的内存实现。
type RuleStore struct {
	mu    sync.Mutex
	rules []tool.Rule
}

// NewRuleStore 创建规则存储。
func NewRuleStore() *RuleStore { return &RuleStore{} }

// List 实现 tool.RuleStore。
func (s *RuleStore) List(ctx context.Context) ([]tool.Rule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]tool.Rule(nil), s.rules...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Add 实现 tool.RuleStore。
func (s *RuleStore) Add(ctx context.Context, rule tool.Rule) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.rules {
		if existing.ID == rule.ID {
			return fmt.Errorf("mock: 规则 %q 已存在", rule.ID)
		}
	}
	s.rules = append(s.rules, rule)
	return nil
}

// Remove 实现 tool.RuleStore。
func (s *RuleStore) Remove(ctx context.Context, ruleID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, existing := range s.rules {
		if existing.ID == ruleID {
			s.rules = append(s.rules[:i], s.rules[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("mock: 规则 %q 不存在", ruleID)
}

// Rules 返回全部规则。
func (s *RuleStore) Rules() []tool.Rule {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]tool.Rule(nil), s.rules...)
}

// ---------------------------------------------------------------------------
// 测试用工具
// ---------------------------------------------------------------------------

// StaticTool 是最简单的工具实现：固定定义 + 固定/可编程响应。
type StaticTool struct {
	Def      tool.ToolDefinition
	Response tool.ToolResponse
	// Fn 覆盖 Response 的行为。
	Fn func(ctx context.Context, req tool.ToolRequest) tool.ToolResponse
	// Panic 为 true 时 Execute 直接 panic（测试 panic 隔离）。
	Panic bool
	// StateCheckerFn 实现 tool.StateChecker（测试 B 类）。
	StateCheckerFn func(ctx context.Context, req tool.ToolRequest) (tool.StateStatus, error)
}

// Definition 实现 tool.Tool。
func (t *StaticTool) Definition() tool.ToolDefinition { return t.Def }

// Execute 实现 tool.Tool。
func (t *StaticTool) Execute(ctx context.Context, req tool.ToolRequest) tool.ToolResponse {
	if t.Panic {
		panic("mock: 工具主动 panic")
	}
	if t.Fn != nil {
		return t.Fn(ctx, req)
	}
	resp := t.Response
	if resp.ToolCallID == "" {
		resp.ToolCallID = req.ToolCallID
	}
	if resp.ToolName == "" {
		resp.ToolName = t.Def.Name
	}
	return resp
}

// CheckState 实现 tool.StateChecker（当 StateCheckerFn 非空时）。
func (t *StaticTool) CheckState(ctx context.Context, req tool.ToolRequest) (tool.StateStatus, error) {
	if t.StateCheckerFn == nil {
		return tool.StateUnknown, nil
	}
	return t.StateCheckerFn(ctx, req)
}

// ---------------------------------------------------------------------------
// Secrets mock（供审计脱敏接线测试）
// ---------------------------------------------------------------------------

// Secrets 是 tool.SecretsProvider 的内存实现（开发期 mock）。
// 生产使用 internal/secrets 的平台后端。
type Secrets struct {
	mu     sync.Mutex
	values map[string]string // ref -> value
	next   int
}

// NewSecrets 创建内存秘密提供者。
func NewSecrets() *Secrets {
	return &Secrets{values: make(map[string]string)}
}

// Get 实现 tool.SecretsProvider。
func (s *Secrets) Get(ref string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.values[ref]
	if !ok {
		return "", fmt.Errorf("mock: 秘密 %q 不存在", ref)
	}
	return v, nil
}

// Put 实现 tool.SecretsProvider。
func (s *Secrets) Put(value string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	ref := fmt.Sprintf("mockref:%d", s.next)
	s.values[ref] = value
	return ref, nil
}

// Refs 返回全部 ref。
func (s *Secrets) Refs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	refs := make([]string, 0, len(s.values))
	for ref := range s.values {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	return refs
}

// Has 报告 ref 是否存在。
func (s *Secrets) Has(ref string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.values[ref]
	return ok
}

// Count 返回秘密数量。
func (s *Secrets) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.values)
}

// ContainsValue 报告某字符串是否包含任一秘密值（测试断言用）。
func (s *Secrets) ContainsValue(text string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range s.values {
		if v != "" && strings.Contains(text, v) {
			return true
		}
	}
	return false
}
