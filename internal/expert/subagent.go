// subagent.go —— 专家子 Agent 执行器（v1 src/main/tools/Skill/sub-agent.ts 的 Go 对等实现）。
//
// 与主 Agent Loop 类似但更精简：
//   - 非流式调用（子 Agent 不需要把 token 流推给前端）
//   - 通过事件回调推送执行过程（工具调用/结果/中间思考），供前端可视化
//   - 多轮工具调用直到子 Agent 给出最终回答，或达到 MaxSubAgentRounds
//
// 依赖注入：本执行器不直接持有具体 Provider，而是通过 ToolExecutor 与
// provider.Provider 两个接口拿能力 —— 这样 expert 包不依赖工具运行时（任务 04）
// 的具体实现，也便于单测注入 mock。
package expert

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/provider"
)

// Stage 子 Agent 工作过程事件的阶段（与 v1 subAgentEvent.stage 对齐）。
type Stage string

const (
	StageStarted    Stage = "started"
	StageTool       Stage = "tool"
	StageToolResult Stage = "toolResult"
	StageMessage    Stage = "message"
	StageFinished   Stage = "finished"
)

// WorkEvent 专家工作过程事件 —— 前端 ExpertWorkCard 的数据来源。
type WorkEvent struct {
	ExpertID    string `json:"expert_id"`
	ExpertName  string `json:"expert_name"`
	Stage       Stage  `json:"stage"`
	TaskSummary string `json:"task_summary,omitempty"`
	Detail      string `json:"detail,omitempty"`
	ToolArgs    string `json:"tool_args,omitempty"`
	Result      string `json:"result,omitempty"`
	Timestamp   int64  `json:"timestamp"`
}

// ToolCallRequest 子 Agent 发起的一次工具调用。
type ToolCallRequest struct {
	ID        string
	Name      string
	Arguments map[string]any
}

// ToolCallResult 工具执行结果。
type ToolCallResult struct {
	Content string
	Success bool
	Error   string
}

// ToolExecutor 执行工具调用的抽象（由任务 04 的工具运行时实现）。
type ToolExecutor interface {
	// Execute 执行一次工具调用。实现方负责权限判定与沙箱。
	Execute(ctx context.Context, req ToolCallRequest) (ToolCallResult, error)
	// Available 报告该工具是否可用（未注册的工具应被过滤掉，不进 schema）。
	Available(name string) bool
}

// ModelPool 抽象「子代理用哪个服务商/哪个模型」的分配能力（任务 05）。
//
// 接口定义在消费方（expert 包），实现方（provider.Pool）结构化满足 —— expert
// 的单测因此可以注入假池，不必构造真实 HTTP 客户端，也不产生反向依赖。
type ModelPool interface {
	// Select 挑选一个健康候选。order 是期望的候选 ID 优先级顺序（来自设置
	// 面板的专家/分类分配，空 = 池默认顺序）；exclude 是已试过且失败的候选，
	// 不再返回。
	Select(order []string, exclude ...string) (provider.Target, bool)
	// FailoverLimit 失败转移次数上限（最多把候选轮询一遍，避免死循环）。
	FailoverLimit() int
}

// SubAgentOptions 子 Agent 执行参数。
type SubAgentOptions struct {
	Provider  provider.Provider
	Executor  ToolExecutor
	Model     string
	MaxTokens int
	// Timeout 子 Agent 总超时（v1 默认 120s）。
	Timeout time.Duration
	// MaxRounds 覆盖默认轮次上限（0 用 MaxSubAgentRounds）。
	MaxRounds int

	// Pool 为子代理挑选服务商与模型（任务 05）。非 nil 时 Model 不再取
	// 写死的全局配置，而是从池里按 CandidateOrder 挑选；调用因限流/连接类
	// 错误失败时，换下一个健康候选、用同一个 task 同一个 expert 重新执行。
	// nil 时 Provider/Model 的取值与改动前完全一致。
	Pool ModelPool
	// CandidateOrder 候选服务商 ID 的优先级顺序（专家/分类维度的模型分配）。
	CandidateOrder []string
	// MaxFailovers 覆盖失败转移次数上限（0 = 池的 FailoverLimit，最多轮询一遍）。
	MaxFailovers int

	// ToolNames 该专家被配置的工具名列表（来自 AnalyzeExpert）。
	ToolNames []string

	// ReasoningEffort 思考强度；Off 时用 Temperature。
	ReasoningEffort provider.ReasoningEffort
	Temperature     float64

	// ExpertID / ExpertName 用于事件标注；缺省从系统提示词反解。
	ExpertID   string
	ExpertName string

	// OnEvent 事件回调（可空）。
	OnEvent func(WorkEvent)
	// EventSink 额外的事件收集器（v1 用它把事件持久化到工具结果里）。
	EventSink func(WorkEvent)
}

// SubAgentRequest 一次子 Agent 执行的完整请求。
type SubAgentRequest struct {
	// SystemPrompt 系统提示词。为空时由 Orchestrator 用专家定义生成。
	SystemPrompt string
	// Task 用户/主 Agent 交给专家的任务。
	Task string
	// Options 执行参数（Provider/Executor/模型/超时等）。
	Options SubAgentOptions
}

// SubAgentResult 子 Agent 执行结果。
type SubAgentResult struct {
	Content string
	Events  []WorkEvent
	Rounds  int
	// Empty 表示模型没有返回任何内容（Content 是占位文案）。
	// 规划阶段据此判断「没产出方案」，而不是去匹配占位文本。
	Empty bool
}

// ErrSubAgentTimeout 子 Agent 超时。
var ErrSubAgentTimeout = errors.New("expert: 子 Agent 执行超时")

// RunSubAgent 以专家视角执行子任务。
//
// 流程：system(专家提示词) + user(task) → 多轮 tool_calls → 最终回答。
// 达到轮次上限时追加一条「请直接给最终回答」的指令再问一次（v1 同款兜底）。
//
// 任务 05 的失败转移：Options.Pool 非 nil 时，Provider/Model 由池按候选顺序
// 挑选；调用因限流（429）或连接类错误失败时，换下一个健康候选、用同一个
// task 同一个 expert 重新执行一遍 —— 派新代理接手旧工作，而不是把失败抛给
// 用户。重试上限是池的 FailoverLimit（最多把候选轮询一遍），不会死循环；
// 用户侧只是感觉慢了一点。
func RunSubAgent(ctx context.Context, req SubAgentRequest) (*SubAgentResult, error) {
	if req.Options.Pool == nil {
		return runSubAgentOnce(ctx, req.SystemPrompt, req.Task, req.Options)
	}
	return runSubAgentWithPool(ctx, req)
}

// runSubAgentWithPool 带失败转移的执行：按候选顺序逐个尝试，直到某个候选
// 成功、或遇到不该转移的错误（确定性错误/取消/超时）、或候选全部试完。
func runSubAgentWithPool(ctx context.Context, req SubAgentRequest) (*SubAgentResult, error) {
	pool := req.Options.Pool
	maxAttempts := pool.FailoverLimit()
	if req.Options.MaxFailovers > 0 && req.Options.MaxFailovers < maxAttempts {
		maxAttempts = req.Options.MaxFailovers
	}
	if maxAttempts < 1 {
		maxAttempts = 1
	}

	var (
		tried   []string
		lastRes *SubAgentResult
		lastErr error
		lastID  string
	)
	for attempt := 0; attempt < maxAttempts; attempt++ {
		target, ok := pool.Select(req.Options.CandidateOrder, tried...)
		if !ok {
			break
		}
		tried = append(tried, target.ID)

		opts := req.Options
		opts.Pool = nil // 单次尝试不得再进池化路径
		if target.Provider != nil {
			opts.Provider = target.Provider
		}
		if target.Model != "" {
			// Model 的取值来源从写死的全局配置改为池里按分配策略选出来的那个。
			opts.Model = target.Model
		}

		res, err := runSubAgentOnce(ctx, req.SystemPrompt, req.Task, opts)
		if err == nil {
			if attempt > 0 {
				annotateFailover(res, lastID, target.ID, lastErr)
			}
			return res, nil
		}
		lastRes, lastErr, lastID = res, err, target.ID

		// 取消/整体超时换谁都救不回来，确定性错误（400/密钥/schema/上下文超长）
		// 换谁都会再犯 —— 这两类都不转移。
		if ctx.Err() != nil || errors.Is(err, ErrSubAgentTimeout) || !failoverWorthy(err) {
			return res, err
		}
	}
	if lastRes == nil {
		return nil, fmt.Errorf("expert: 模型池中没有可用候选")
	}
	return lastRes, lastErr
}

// annotateFailover 把一次失败转移记进事件流（只插一条，避免 ExpertWorkCard
// 出现两段重复的「开始处理任务」），让用户明白慢的原因而不是只看到报错。
func annotateFailover(res *SubAgentResult, fromID, toID string, cause error) {
	if res == nil {
		return
	}
	ev := WorkEvent{
		Stage:     StageMessage,
		Detail:    fmt.Sprintf("模型 %s 调用失败（%v），已切换到 %s 继续同一任务", fromID, cause, toID),
		Timestamp: time.Now().UnixMilli(),
	}
	res.Events = append([]WorkEvent{ev}, res.Events...)
}

// failoverWorthy 判断一次失败是否值得换模型重试。
//
// 口径与熔断器的记账一致（breaker.go countsTowardBreaker）：只有服务端/传输层
// 失败才转移 —— 429 限流、DNS、连接超时、5xx。分类结果直接复用 errors.go 的
// 现成产出，这里不重新分类。
func failoverWorthy(err error) bool {
	if err == nil {
		return false
	}
	var ce *provider.ClassifiedError
	if errors.As(err, &ce) {
		return switchWorthyClass(ce.Class)
	}
	return switchWorthyClass(provider.Classify(err).Class)
}

func switchWorthyClass(class provider.ErrorClass) bool {
	switch class {
	case provider.ClassRateLimit, provider.ClassDNS,
		provider.ClassConnectTimeout, provider.ClassServerError:
		return true
	default:
		return false
	}
}

// runSubAgentOnce 执行一次完整的子 Agent 会话（不换服务商）。
func runSubAgentOnce(ctx context.Context, systemPrompt, task string, o SubAgentOptions) (*SubAgentResult, error) {
	if o.Provider == nil {
		return nil, fmt.Errorf("expert: 缺少 Provider")
	}

	maxRounds := o.MaxRounds
	if maxRounds <= 0 {
		maxRounds = MaxSubAgentRounds
	}
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = 120 * time.Second
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	expertID, expertName := o.ExpertID, o.ExpertName
	if expertID == "" || expertName == "" {
		n, _ := ParseExpertIdentity(systemPrompt)
		if expertName == "" {
			expertName = n
		}
		if expertID == "" {
			expertID = n
		}
	}

	res := &SubAgentResult{}

	emit := func(stage Stage, detail, toolArgs, result string) {
		ev := WorkEvent{
			ExpertID:    expertID,
			ExpertName:  expertName,
			Stage:       stage,
			TaskSummary: truncateRunes(task, 120),
			Detail:      detail,
			ToolArgs:    toolArgs,
			Result:      result,
			Timestamp:   time.Now().UnixMilli(),
		}
		res.Events = append(res.Events, ev)
		if o.OnEvent != nil {
			o.OnEvent(ev)
		}
		if o.EventSink != nil {
			o.EventSink(ev)
		}
	}

	emit(StageStarted, fmt.Sprintf("专家开始处理任务：%s", truncateRunes(task, 100)), "", "")

	messages := []provider.Message{
		{Role: provider.RoleSystem, Content: systemPrompt},
		{Role: provider.RoleUser, Content: task},
	}

	// 子 Agent 的工具 schema 只包含可用工具（未注册工具进 schema 会被服务端判 invalid）。
	tools := o.resolveTools()

	for round := 0; round < maxRounds; round++ {
		if err := runCtx.Err(); err != nil {
			emit(StageFinished, "专家任务已取消或超时", "", "")
			return res, ctxErr(runCtx, ctx)
		}
		res.Rounds = round + 1

		resp, err := o.Provider.Complete(runCtx, provider.CompletionRequest{
			Model:           o.model(),
			Messages:        messages,
			Tools:           tools,
			ThinkingMode:    o.ReasoningEffort != "" && o.ReasoningEffort != provider.EffortOff,
			ReasoningEffort: o.ReasoningEffort,
			Temperature:     o.temperature(),
			MaxTokens:       o.maxTokens(),
		})
		if err != nil {
			emit(StageFinished, fmt.Sprintf("专家 API 调用失败：%v", err), "", "")
			// API 调用中途到期的整体超时/取消必须归一成与轮次边界同样的口径
			// （ErrSubAgentTimeout / ctx.Err()）：provider 会把裸的
			// DeadlineExceeded 分类成连接超时，若原样上抛会被池误判为「可转移」，
			// 让每个候选把整个任务从头重跑一遍（最坏 N×超时上限），且大概率
			// 再次超时。换谁都救不回来的失败不该转移。
			if runCtx.Err() != nil {
				return res, ctxErr(runCtx, ctx)
			}
			return res, fmt.Errorf("expert: 子 Agent API 调用失败: %w", err)
		}

		// 无工具调用 → 最终回答。
		if len(resp.ToolCalls) == 0 {
			final := resp.Content
			empty := strings.TrimSpace(final) == ""
			if empty {
				final = "(子 Agent 未返回内容)"
			}
			emit(StageFinished, "专家已完成任务，返回最终结果", "", final)
			res.Content = final
			res.Empty = empty
			return res, nil
		}

		// 有工具调用 → 记录 assistant 轮次并逐个执行。
		messages = append(messages, provider.Message{
			Role:             provider.RoleAssistant,
			Content:          resp.Content,
			ReasoningContent: resp.ReasoningContent,
			ToolCalls:        resp.ToolCalls,
		})
		if strings.TrimSpace(resp.Content) != "" {
			emit(StageMessage, truncateRunes(resp.Content, 300), "", "")
		}

		for _, tc := range resp.ToolCalls {
			toolMsg := o.executeTool(runCtx, tc, emit)
			// 必须逐条追加：assistant(tool_calls) 后紧跟对应的 tool 响应，
			// 缺任何一条都会让下一轮请求因配对不完整而 400。
			messages = append(messages, toolMsg)
		}
	}

	// 达到轮次上限 —— 请求最终总结（v1 同款兜底）。
	emit(StageMessage, "已达最大工具调用轮次，正在生成最终总结…", "", "")
	messages = append(messages, provider.Message{
		Role:    provider.RoleUser,
		Content: "你已经完成了所有工具调用。请基于已有信息直接给出最终回答，不要再调用任何工具。",
	})

	resp, err := o.Provider.Complete(runCtx, provider.CompletionRequest{
		Model:           o.model(),
		Messages:        messages,
		ThinkingMode:    o.ReasoningEffort != "" && o.ReasoningEffort != provider.EffortOff,
		ReasoningEffort: o.ReasoningEffort,
		Temperature:     o.temperature(),
		MaxTokens:       o.maxTokens(),
	})
	if err != nil {
		emit(StageFinished, "专家最终总结请求失败", "", "")
		if runCtx.Err() != nil {
			return res, ctxErr(runCtx, ctx)
		}
		return res, fmt.Errorf("expert: 子 Agent 最终总结失败: %w", err)
	}

	final := resp.Content
	empty := strings.TrimSpace(final) == ""
	if empty {
		final = "(子 Agent 达到最大工具调用轮次，且最终总结未返回内容)"
	}
	emit(StageFinished, "专家已完成任务，返回最终结果", "", final)
	res.Content = final
	res.Empty = empty
	return res, nil
}

// executeTool 执行一个工具调用，返回应追加到对话的 tool 响应消息。
func (o SubAgentOptions) executeTool(
	ctx context.Context,
	tc provider.ToolCall,
	emit func(Stage, string, string, string),
) provider.Message {
	args := map[string]any{}
	if tc.Arguments != "" {
		if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
			// 参数不是合法 JSON —— 不执行，把错误作为工具结果回给模型让它自我修正。
			result := fmt.Sprintf("工具参数解析失败（不是合法 JSON）：%v", err)
			emit(StageToolResult, result, "", "")
			return provider.Message{Role: provider.RoleTool, Content: result, ToolCallID: tc.ID}
		}
	}

	emit(StageTool, fmt.Sprintf("调用工具 %s", tc.Name), summarizeArgs(args), "")

	var result ToolCallResult
	if o.Executor == nil || !o.Executor.Available(tc.Name) {
		result = ToolCallResult{
			Content: fmt.Sprintf("工具 %s 未注册，无法执行", tc.Name),
			Success: false,
			Error:   "工具未注册",
		}
	} else {
		r, err := o.Executor.Execute(ctx, ToolCallRequest{ID: tc.ID, Name: tc.Name, Arguments: args})
		if err != nil {
			result = ToolCallResult{Content: fmt.Sprintf("工具执行出错：%v", err), Success: false, Error: err.Error()}
		} else {
			result = r
		}
	}

	summary := result.Content
	if !result.Success {
		summary = "执行失败：" + firstNonEmpty(result.Error, "未知错误")
	}
	emit(StageToolResult, truncateRunes(summary, 150), "", "")

	// 截断超长结果防止上下文溢出（v1 MAX_SUB_TOOL_RESULT）。
	content := result.Content
	if len(content) > MaxSubToolResult {
		content = content[:MaxSubToolResult] + "\n[...子 Agent 结果已截断]"
	}

	return provider.Message{Role: provider.RoleTool, Content: content, ToolCallID: tc.ID}
}

// resolveTools 过滤出实际可用的工具并构造 schema。
func (o SubAgentOptions) resolveTools() []provider.ToolDefinition {
	if o.Executor == nil {
		return nil
	}
	names := o.ToolNames
	if len(names) == 0 {
		return nil
	}
	defs := make([]provider.ToolDefinition, 0, len(names))
	for _, n := range names {
		if !o.Executor.Available(n) {
			continue
		}
		defs = append(defs, provider.ToolDefinition{
			Name:        n,
			Description: fmt.Sprintf("专家可用工具：%s", n),
			Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
		})
	}
	return defs
}

func (o SubAgentOptions) model() string { return o.Model }

func (o SubAgentOptions) maxTokens() int {
	if o.MaxTokens > 0 {
		return o.MaxTokens
	}
	return 8192
}

func (o SubAgentOptions) temperature() float64 {
	if o.Temperature > 0 {
		return o.Temperature
	}
	return 0.7
}

// summarizeArgs 生成参数摘要（最多 3 项，每项截断 80 字符）。
func summarizeArgs(args map[string]any) string {
	if len(args) == 0 {
		return "(无参数)"
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	// 稳定输出顺序，便于前端展示与日志对比。
	sortStrings(keys)

	parts := make([]string, 0, 3)
	for i, k := range keys {
		if i >= 3 {
			break
		}
		v := args[k]
		var s string
		if str, ok := v.(string); ok {
			s = truncateRunes(str, 80)
		} else if data, err := json.Marshal(v); err == nil {
			s = truncateRunes(string(data), 80)
		}
		parts = append(parts, fmt.Sprintf("%s: %s", k, s))
	}
	return strings.Join(parts, ", ")
}

func ctxErr(runCtx, parent context.Context) error {
	if parent.Err() != nil {
		return parent.Err()
	}
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return ErrSubAgentTimeout
	}
	return runCtx.Err()
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// truncateRunes 按字符（非字节）截断，避免切坏多字节 UTF-8。
func truncateRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "…"
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
