// expert_run.go —— 任务3：用户手选专家的直连执行路径。
//
// 背景：专家能力此前只以「工具」的形式存在，要走通必须先让主模型自己决定调用
// agent_expert。用户在输入框旁明确点了一位专家，却仍要等模型"心情好"才生效，
// 这是本任务要消除的落差。
//
// 本文件只做「接线」：把 Engine 已经装配好的 Provider 与工具运行时，适配成
// internal/expert 需要的接口，然后调用现成的 Orchestrator.Activate()。两阶段
// 编排（planPhase → runPhase）与子 Agent 执行都在 expert 包内部，这里不复制、
// 也不改写它们的逻辑。
//
// 与主 Agent Loop 的关系：两者是平行分支。Submit → executeRun 在
// rec.request.ExpertID 非空时走这里，否则走原来的 agent.NewLoop 路径，那条
// 路径一字未改。
package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/expert"
	"github.com/ximo888ok-netizen/ximo-agent/internal/ports"
	"github.com/ximo888ok-netizen/ximo-agent/internal/provider"
	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// executeExpertRun 以用户指定的专家身份处理本次 run。
//
// 终态与主循环保持一致：成功 StateCompleted + answer，失败 StateFailed，取消
// StateCancelled。这样前端的渲染、对账与恢复逻辑不需要为专家路径开特例。
func (e *Engine) executeExpertRun(ctx context.Context, rec *runRecord) {
	expertID := rec.request.ExpertID
	runID := rec.runID()
	sessionID := rec.sessionID()

	// 专家路径的终态收敛口：closure 事件必须与终态跃迁成对出现。
	//
	// 收敛成两个闭包而不是在每条 return 前各写一遍，是因为漏写一条就会让该路径
	// 的 Work Log 缺收尾结论 —— 而这正是本次修的问题（专家/集群路径此前一条
	// run.closure 都不发，前端工作卡片永远没有判据）。answer 为空表示这次 run
	// 没有产出可用答案，closure 的 verdict 会是 failed。
	fail := func(cause error) {
		e.emitRunClosure(ctx, rec, types.ClosureReasonFailed, "", 0)
		e.finishRun(ctx, rec, types.StateFailed, "", cause)
	}
	cancel := func() {
		e.emitRunClosure(ctx, rec, types.ClosureReasonCancelled, "", 0)
		e.finishRun(ctx, rec, types.StateCancelled, "",
			types.NewError(types.CodeCancelled, "run cancelled"))
	}

	// 专家不存在时必须明确失败，而不是静默退回通用 Agent 路径：用户明确点了
	// 这位专家，退回主循环会得到一个"看起来正常但专家根本没参与"的结果，那比
	// 直接报错更难排查。
	if e.expertRegistry == nil {
		fail(types.NewError(types.CodeInternal, "专家库不可用"))
		return
	}
	if e.deps.Provider == nil {
		fail(types.NewError(types.CodeInternal, "provider is not configured"))
		return
	}
	if _, ok := e.expertRegistry.Get(expertID); !ok {
		e.publishAndAppend(ctx, types.Event{
			RunID: runID, SessionID: sessionID,
			Type: types.EventError, State: types.StateThinking,
			Timestamp: time.Now(),
			Message:   fmt.Sprintf("未知专家 %q，无法激活", expertID),
			Data:      map[string]any{"expertId": expertID, "reason": "expert_not_found"},
		})
		fail(types.NewError(types.CodeNotFound, "expert %q not found", expertID))
		return
	}

	// 子 Agent 的工具调用必须带上 run 上下文，并且走 Engine 的受控派发链路
	// （admission → 公平队列 → 资源租约）。expert.ToolExecutor 的签名里没有
	// run，所以走 context 传递——详见 ports.ToolExecution 的说明。
	runCtx := ports.WithToolExecution(ctx, ports.ToolExecution{
		RunID:     runID,
		SessionID: sessionID,
		Dispatch: func(dctx context.Context, req ports.ToolRequest) (types.ToolResult, error) {
			return e.dispatchToolCall(dctx, rec, types.ToolCall{
				ID:        req.ToolCallID,
				Name:      req.ToolName,
				Arguments: req.Arguments,
			})
		},
	})

	orch := e.newExpertOrchestrator(rec.request.Model)
	// 阶段跃迁事件：让"专家当前在规划还是在实施"可见（前端工作卡片的数据源）。
	// 回调与下面的 Activate 在同一个 goroutine 上同步执行，因此这里不需要加锁。
	// run 被取消后立刻静默，不再向外广播废弃状态。
	orch.OnPhase = func(ph expert.PhaseEvent) {
		if ctx.Err() != nil || rec.terminal() {
			return
		}
		e.emitExpertPhase(ctx, rec, ph)
	}

	// prompt 作为 ExpertRequest.Task —— 契约明确要求。
	outcome, err := orch.Activate(runCtx, expert.ExpertRequest{
		ExpertID: expertID,
		Task:     rec.request.Prompt,
	})

	// 只要 context 被取消（用户调用了 Cancel），无论是哪一步中止，都必须立刻走 cancelled 终态，
	// 绝不能继续发 final_answer 事件或落库已作废的结果。
	if ctx.Err() != nil {
		cancel()
		return
	}

	if err != nil {
		e.publishAndAppend(ctx, types.Event{
			RunID: runID, SessionID: sessionID,
			Type: types.EventError, State: types.StateThinking,
			Timestamp: time.Now(),
			Message:   types.RedactString(err.Error()),
			Data:      map[string]any{"expertId": expertID},
		})
		fail(err)
		return
	}

	// 子 Agent 失败时 SubAgentMode 为 false，Content 降级为「专家信息 + 手动
	// 指引」。Orchestrator 刻意按「失败也返回可操作信息」处理（v1 同款），但这
	// 段文案本身写着「子 Agent 调用失败」——所以它要作为 error 事件明确暴露，
	// 并让本次 run 以 failed 收尾（见下方 SubAgentMode 判定），不能只当提示。
	if outcome.Error != "" {
		e.publishAndAppend(ctx, types.Event{
			RunID: runID, SessionID: sessionID,
			Type: types.EventError, State: types.StateThinking,
			Timestamp: time.Now(),
			Message:   types.RedactString(outcome.Error),
			Data: map[string]any{
				"expertId":       expertID,
				"expertName":     outcome.Expert.Name,
				"subAgentMode":   false,
				"degradedToInfo": true,
			},
		})
	}

	// 最终回答。事件顺序与主循环一致：先 final_answer 再终态跃迁——反过来，订阅
	// 者会在流结束前看到"completed"却没有答案。
	answer := outcome.Content
	e.publishAndAppend(ctx, types.Event{
		RunID: runID, SessionID: sessionID,
		Type: types.EventFinalAnswer, State: types.StateThinking,
		Timestamp: time.Now(),
		Message:   answer,
		Data: map[string]any{
			"answer":       answer,
			"expertId":     outcome.Expert.ID,
			"expertName":   outcome.Expert.Name,
			"subAgentMode": outcome.SubAgentMode,
			"plan":         outcome.Plan,
			"phaseEvents":  outcome.PlanPhaseEvents,
			"deviation":    outcome.Deviation,
			"workEvents":   len(outcome.Events),
		},
	})

	if ctx.Err() != nil {
		cancel()
		return
	}

	// 降级（子 Agent 没真正执行）时**不能**报 completed。
	//
	// 这里改掉了此前的「失败也报 completed」：那段降级文案（buildFallbackContent）
	// 是有内容可交付的，但它明确写着「子 Agent 调用失败」，把它连同 completed 一起
	// 交给用户，正是审核文档要求消除的那类假完成 —— 与集群路径 buildClusterReport
	// 的统计口径、与 F1–F4「绝不把没完成说成完成」是同一条规则。
	//
	// 答案文本依然发出去（final_answer 事件在前），用户看到的是「失败 + 可操作指引」，
	// 而不是一段看起来正常的专家结论。
	if !outcome.SubAgentMode {
		cause := types.NewError(types.CodeInternal, "专家未能真正执行，已降级为手动指引")
		if outcome.Error != "" {
			cause = types.NewError(types.CodeInternal, "%s", types.RedactString(outcome.Error))
		}
		e.emitRunClosure(ctx, rec, types.ClosureReasonFailed, "", 1)
		e.finishRun(ctx, rec, types.StateFailed, answer, cause)
		return
	}

	e.emitRunClosure(ctx, rec, types.ClosureReasonCompleted, answer, 0)
	e.finishRun(ctx, rec, types.StateCompleted, answer, nil)
}

// newExpertOrchestrator 用 Engine 现有的依赖装配一个专家调度器。
//
// 每次调用构造一个：Orchestrator 本身只是「注册表 + 一组执行参数 + 回调」的
// 组合，构造成本极低；做成 Engine 的字段反而要多一把锁来保护 OnPhase 这类
// per-run 回调。
//
// model 是本次 run 用户显式选择的模型（SubmitPayload.model，可为空）。
func (e *Engine) newExpertOrchestrator(model string) *expert.Orchestrator {
	opts := expert.OrchestratorOptions{
		Registry: e.expertRegistry,
		// 两阶段编排全开：这是 v2 相对 v1 的核心增量（v1 的编排逻辑藏在 prompt
		// 文案里），专家路径没有理由把它关掉。
		EnableTwoPhase: true,
		Runner: expert.SubAgentOptions{
			// expert 包要的是 provider.Provider（任务06 的具名接口），Engine 持有
			// 的是 ports.Provider（任务02 的消费侧契约）。两者方法名相同但签名
			// 不同，必须适配——见文件末尾的 portsProviderAsExpert。
			Provider: &portsProviderAsExpert{inner: e.deps.Provider},
			Executor: &expertToolExecutor{runtime: e.deps.Tools},
			// 子 Agent 的总超时直接取本次 run 的墙钟预算：再叠一个更小的值会让
			// 专家在半途被掐断，而 run 本身还没到期（留下"跑了一半"的现场）。
			Timeout:         e.cfg.Scheduler.Limits.MaxRunDuration,
			ReasoningEffort: provider.EffortHigh,
		},
		// 资源闸门与引擎调度器的工具路径共用同一个 ResourcePool：
		// 「同时在跑的子代理数 ≤ 8」对专家直连与集群都必须成立，否则就是两个
		// 各配 8 槽的半吊子池。
		Resources: e.Scheduler().Resources(),
	}

	// 用户显式选了模型 → 这一次专家执行就用它，且不挂模型池。
	//
	// 池选路会用自己的候选覆盖 Model（subagent.go 的池分支），于是「输入框旁边
	// 明明选了模型，跑出来却是配置里的默认模型」——此前专家/集群路径就是这样把
	// 用户的选择静默丢掉的。显式选择优先于自动选路：这与主 Agent 路径的行为一致
	// （主循环同样只把 Model 交给 provider，没有任何池）。
	//
	// 代价是这一次执行失去「限流/连接类失败换下一个候选」的能力——但那是自动
	// 选路的补偿机制，不该反过来覆盖用户的显式决定。
	if m := strings.TrimSpace(model); m != "" {
		opts.Runner.Model = m
		return expert.NewOrchestrator(opts)
	}

	// 装配层提供候选池（任务 05）时，子代理走池选路 + 失败转移 + 按专家/分类
	// 的候选分配。池取不到（未装配/出错/无候选）时保持 Provider 单通道——
	// 直连路径不能因为池的问题而比没有池时更差。
	if e.deps.SubAgentPool != nil {
		if pool, err := e.deps.SubAgentPool(); err == nil && pool != nil {
			opts.Runner.Pool = pool
			if e.deps.SubAgentCandidates != nil {
				opts.Allocator = func(ex expert.Expert) []string {
					return e.deps.SubAgentCandidates(ex.ID, ex.Division)
				}
			}
		}
	}
	return expert.NewOrchestrator(opts)
}

// emitRunClosure 发出 run.closure（收尾判据）。
//
// 为什么专家/集群路径必须自己发：run.closure 此前只由主 Agent 循环发出，而前端
// 的工作卡片（steps.ts 的 'run.closure' 分支）与「继续」按钮引用的失败项都读它。
// 于是专家直连与 Agent 集群的 run 永远没有收尾结论 —— 工作日志画不出「这次到底
// 算不算干完」，用户也就无法判断要不要再来一轮。
//
// 这里用 BuildRunClosureReport(nil, …)（nil tracker）：子 Agent 的工具/待办证据
// 不在主循环的 ClosureTracker 里，专家路径能如实提供的只有「答案是否为空」
// 「终止原因」与「有多少专家降级」。宁可少报几项检查，也不把主循环的 tracker
// 状态张冠李戴过来。
//
// degraded 是**没有真正执行**的专家数（直连路径是 0 或 1，集群是 0..N）。
// 它让「部分专家降级」得到 partial 而不是 closed —— 答案非空是通过
// answer_nonempty 的必要条件，但一段降级指引不该让整份报告看起来是干完了。
//
// 两条硬约束（都是踩过的坑）：
//
//  1. 终态之后绝不再写。用户在子 Agent 跑到一半点了取消时，Cancel 会先一步把
//     run 落库成 run.cancelled；此时再追加一条 closure，日志重建时「最新事件
//     携带的状态」会折叠回 thinking（foldEvents 取最后一个带 State 的事件），
//     于是 run 看起来永远不是终态，WaitRun 会一直等下去。
//  2. State 必须取 run 当时的真实状态，而不是写死的 thinking：万一在守卫与落库
//     之间被别的 goroutine 抢先终态化，写死的 thinking 同样会把折叠结果拽回去。
func (e *Engine) emitRunClosure(ctx context.Context, rec *runRecord, reason, answer string, degraded int) {
	// 约束 1：终态之后事件流已关闭，再也写不得（理由见上面的注释）。
	// 判据取状态机而不是 rec.state()：Cancel 走的是 machine.Transition，
	// 状态机的 closed 位先于引擎缓存记录更新，只看记录会漏判。
	e.mu.Lock()
	machine := rec.machine
	e.mu.Unlock()
	if machine != nil && machine.Terminal() {
		return
	}
	if rec.terminal() {
		return
	}

	rep := types.BuildRunClosureReport(nil, types.ClosureInput{
		Reason: reason, Answer: answer, Degraded: degraded,
	})
	checks := make([]map[string]any, 0, len(rep.Checks))
	for _, c := range rep.Checks {
		checks = append(checks, map[string]any{
			"id": c.ID, "label": c.Label, "pass": c.Pass, "note": c.Note,
		})
	}
	e.publishAndAppend(ctx, types.Event{
		RunID:     rec.runID(),
		SessionID: rec.sessionID(),
		Type:      types.EventRunClosure,
		State:     rec.state(),
		Timestamp: time.Now(),
		Message:   rep.Verdict,
		Data: map[string]any{
			"verdict":    rep.Verdict,
			"checks":     checks,
			"incomplete": rep.Incomplete,
			"reason":     reason,
			// expertPath 让前端（与事后排查）能区分这条 closure 来自专家路径：
			// 它的检查项是「答案/预算/专家执行」几项，不含工具与待办证据。
			"expertPath": true,
			"degraded":   degraded,
		},
	})
}

// emitExpertPhase 把阶段跃迁事件推送出去。
//
// 用 EventRunStateChanged（持久事件）而不是 EventProgress：合并器只保留
// content/reasoning 载荷（backpressure.go flushKey 会重建 Data），阶段信息
// 挂在 progress 上会在合并时被静默剥掉；而阶段跃迁是用户可见的逻辑边界
// （「专家正在规划/正在实施」），落库后前端断线重连也能从持久日志补上当前
// 阶段。State 取 thinking：专家子代理执行期间 run 本来就处于 thinking，
// 前端对该事件只更新状态、不读 Data，语义不受影响。
func (e *Engine) emitExpertPhase(ctx context.Context, rec *runRecord, ph expert.PhaseEvent) {
	if ctx.Err() != nil || rec.terminal() {
		return
	}
	data := map[string]any{
		"expertId":   ph.ExpertID,
		"expertName": ph.ExpertName,
		"phase":      ph.Phase,
	}
	if ph.Plan != "" {
		// Plan 阶段的方案文本是这一步唯一有信息量的载荷。
		data["plan"] = ph.Plan
	}

	e.publishAndAppend(ctx, types.Event{
		RunID:     rec.runID(),
		SessionID: rec.sessionID(),
		Type:      types.EventRunStateChanged,
		State:     types.StateThinking,
		Timestamp: time.Now(),
		Message:   expertPhaseMessage(ph),
		Data:      data,
	})
}

// expertPhaseMessage 给阶段跃迁一句人类可读的说明。
func expertPhaseMessage(ph expert.PhaseEvent) string {
	switch ph.Phase {
	case expert.PhasePlan:
		return fmt.Sprintf("专家 %s 正在制定实施方案…", ph.ExpertName)
	case expert.PhaseExecute:
		return fmt.Sprintf("专家 %s 正在按方案实施…", ph.ExpertName)
	default:
		return fmt.Sprintf("专家 %s：%s", ph.ExpertName, ph.Phase)
	}
}

// publishAndAppend 把事件写入持久日志并推送。
//
// 与 engineSink.Emit 的差别：那条路径面向 agent.LoopEvent，这条面向专家路径直接
// 构造的 types.Event。两者遵守同一套「durable 才落库、全部推送」的规则，专家 run
// 与普通 run 的事件流语义因此完全一致。
//
// 落库失败不阻断：事件是观测手段，不是 run 的产出；因为它失败而把一次已经成功
// 的专家编排改成失败，是得不偿失的。
func (e *Engine) publishAndAppend(ctx context.Context, ev types.Event) {
	if ctx == nil {
		ctx = context.Background()
	}
	if ev.Type.Durable() && e.deps.Events != nil {
		if seq, err := e.deps.Events.Append(ctx, ev.RunID, ev); err == nil {
			ev.Seq = seq
		}
	}
	e.coalescer.Publish(ev)
}

// ---------------------------------------------------------------------------
// expert.ToolExecutor 适配器
// ---------------------------------------------------------------------------

// expertToolExecutor 让专家子 Agent 的工具调用走 Engine 的受控派发链路。
//
// 关键点：子 Agent 调 Executor.Execute 时只带 call ID / 工具名 / 参数，没有 run
// 上下文。若直接调 ToolRuntime.Execute，工具运行时会因缺少 run_id 而按 I2 不变量
// 拒绝（"tool request 缺少 run_id/tool_call_id"）。因此这里从 context 取 Engine
// 挂上的 ToolExecution，用其中的 Dispatch（即 engine.dispatchToolCall）执行——
// admission、公平队列、资源租约三层限制因此全部生效，专家路径不会成为绕过调度器
// 的后门。
type expertToolExecutor struct {
	runtime ports.ToolRuntime
}

var _ expert.ToolExecutor = (*expertToolExecutor)(nil)

// toolAvailability 是工具运行时可选的「是否已注册」探查接口。
//
// 用可选接口而不是往 ports.ToolRuntime 里加方法：ports 是任务02 冻结的跨任务
// 契约，为专家路径的一个细节去改它，会波及所有实现方。
type toolAvailability interface {
	Has(name string) bool
}

// toolDescription 是工具运行时可选的「导出真实定义」探查接口。
//
// 与 toolAvailability 一样是可选能力：装配层（bootstrap.toolRuntimeAdapter）
// 实现了就生效，没实现就退回占位 schema。方法名与 ports 契约无关，纯粹是
// 适配层与引擎之间的约定。
type toolDescription interface {
	Describe(name string) (string, map[string]any, bool)
}

// Available 报告某个工具是否可用。
//
// 专家的推荐工具集（AnalyzeExpert 按部门 + 关键词推出）里包含大量本 build 未
// 注册的名字（ui_generate、code_execute、browser_navigate…）。子 Agent 的 schema
// 只应包含可用工具，否则模型会拿到一个执行必然失败的工具，白烧一轮往返。
//
// 判定方式是「该工具名是否真的已注册」，通过可选接口探查工具运行时。生产装配
// 的 deps.Tools 是 bootstrap.toolRuntimeAdapter，它实现了 Has，因此这条判定在
// 真实运行中总能给出答案；只有遇到既没有注册表也没有 Has 的替身实现时才会落到
// 「未知」分支，此时按可用处理——宁可由运行时的「工具未注册」错误自纠，也不
// 因为探测能力缺失把专家的工具全部阉割。
func (x *expertToolExecutor) Available(name string) bool {
	if x == nil || x.runtime == nil || name == "" {
		return false
	}
	if probe, ok := x.runtime.(toolAvailability); ok {
		return probe.Has(name)
	}
	return true
}

// Describe 实现 expert.ToolDescriber（可选能力）。
//
// 为什么需要它：子 Agent 的 function schema 若只带 `{"type":"object","properties":{}}`，
// 模型看不到任何参数名与必填项，只能凭工具名瞎猜参数，通常一轮就被工具的参数校验
// 打回。这里把工具真实定义透传上去，让子 Agent 第一次调用就有正确的参数形状。
//
// 第二个返回值为 false 表示查不到（未注册或内层不支持导出）——调用方会退回占位
// schema，而不是发一份编造的参数表。
func (x *expertToolExecutor) Describe(name string) (expert.ToolDescriptor, bool) {
	if x == nil || x.runtime == nil || name == "" {
		return expert.ToolDescriptor{}, false
	}
	probe, ok := x.runtime.(toolDescription)
	if !ok {
		return expert.ToolDescriptor{}, false
	}
	desc, params, ok := probe.Describe(name)
	if !ok {
		return expert.ToolDescriptor{}, false
	}
	return expert.ToolDescriptor{Description: desc, Parameters: params}, true
}

var _ expert.ToolDescriber = (*expertToolExecutor)(nil)

// Execute 执行一次子 Agent 的工具调用。
func (x *expertToolExecutor) Execute(ctx context.Context, req expert.ToolCallRequest) (expert.ToolCallResult, error) {
	scope, ok := ports.ToolExecutionFrom(ctx)
	if !ok || scope.RunID == "" {
		// 没有 run 上下文时明确失败，而不是拿空 runID 去调运行时——那会被 I2
		// 不变量拒绝，而错误信息看不出真正的原因。
		return expert.ToolCallResult{
			Success: false,
			Error:   "专家工具调用缺少 run 上下文（Engine 未挂载 ToolExecution）",
		}, nil
	}

	toolReq := ports.ToolRequest{
		RunID:      scope.RunID,
		SessionID:  scope.SessionID,
		ToolCallID: req.ID,
		ToolName:   req.Name,
		Arguments:  req.Arguments,
		// Timeout 留 0：Engine 与工具运行时各自都有超时兜底，这里再叠一层只会
		// 让三处配置互相打架。
	}

	// 优先走 Engine 的受控派发：三层限流 + 幂等 + 权限判定都在那里。
	if scope.Dispatch != nil {
		res, err := scope.Dispatch(ctx, toolReq)
		if err != nil {
			return expert.ToolCallResult{
				Success: false,
				Error:   types.RedactString(err.Error()),
				Content: types.RedactString(err.Error()),
			}, nil
		}
		return expert.ToolCallResult{
			Content: res.Content,
			Success: res.Success,
			Error:   res.Error,
		}, nil
	}

	if x.runtime == nil {
		return expert.ToolCallResult{Success: false, Error: "工具运行时不可用"}, nil
	}
	res, err := x.runtime.Execute(ctx, toolReq)
	if err != nil {
		return expert.ToolCallResult{Success: false, Error: types.RedactString(err.Error())}, nil
	}
	return expert.ToolCallResult{Content: res.Content, Success: res.Success, Error: res.Error}, nil
}

// ---------------------------------------------------------------------------
// Provider 适配：ports.Provider → provider.Provider
// ---------------------------------------------------------------------------

// portsProviderAsExpert 把 ports.Provider 适配成 provider.Provider。
//
// 为什么需要它：expert.SubAgentOptions.Provider 的类型是 provider.Provider
// （任务06 的具名接口：Complete/Stream/Name/ContextWindow/MaxOutputTokens），而
// Engine 持有的是 ports.Provider（任务02 冻结的消费侧契约：只有一个 Complete，
// 且参数与返回类型都不同）。Go 的隐式接口满足在这里不成立——方法名相同但签名
// 不同。bootstrap 里已有一份反向适配（providerAdapter），这里需要的是倒过来。
//
// 只实现 Complete：子 Agent 刻意使用非流式调用（v1 同款，子 Agent 不需要把
// token 流推给前端），Stream 因此永远不会被 expert 包调用。
type portsProviderAsExpert struct {
	inner ports.Provider
}

var _ provider.Provider = (*portsProviderAsExpert)(nil)

func (p *portsProviderAsExpert) Complete(ctx context.Context, req provider.CompletionRequest) (provider.CompletionResponse, error) {
	if p.inner == nil {
		return provider.CompletionResponse{}, fmt.Errorf("engine: provider 未配置")
	}
	if ctx.Err() != nil {
		return provider.CompletionResponse{FinishReason: provider.FinishCancelled}, ctx.Err()
	}
	resp, err := p.inner.Complete(ctx, ports.ProviderRequest{
		Model:     req.Model,
		Messages:  toPortsMessages(req.Messages),
		Tools:     toPortsToolDefs(req.Tools),
		Effort:    types.ReasoningEffort(req.ReasoningEffort),
		MaxTokens: req.MaxTokens,
	})
	if err != nil {
		return provider.CompletionResponse{}, err
	}
	if ctx.Err() != nil || resp.FinishReason == ports.FinishCancelled {
		return provider.CompletionResponse{FinishReason: provider.FinishCancelled}, context.Canceled
	}
	return fromPortsResponse(resp), nil
}

// Stream 在专家路径上不被使用：子 Agent 全程非流式。
func (p *portsProviderAsExpert) Stream(context.Context, provider.CompletionRequest) (<-chan provider.StreamChunk, error) {
	return nil, fmt.Errorf("engine: 专家子 Agent 不支持流式调用")
}

func (p *portsProviderAsExpert) Name() string         { return "engine-provider" }
func (p *portsProviderAsExpert) ContextWindow() int   { return 0 }
func (p *portsProviderAsExpert) MaxOutputTokens() int { return 0 }

// toPortsMessages 把 provider 的消息转换成 ports 的消息。
func toPortsMessages(msgs []provider.Message) []ports.Message {
	out := make([]ports.Message, 0, len(msgs))
	for _, m := range msgs {
		calls := make([]types.ToolCall, 0, len(m.ToolCalls))
		for _, tc := range m.ToolCalls {
			var args map[string]any
			if strings.TrimSpace(tc.Arguments) != "" {
				// 参数是模型给的 JSON 文本。解析失败时保留空参数，由工具运行时以
				// 「参数不合法」的明确错误回给模型自纠——与
				// bootstrap.fromProviderResponse 的处理保持一致。
				_ = json.Unmarshal([]byte(tc.Arguments), &args)
			}
			calls = append(calls, types.ToolCall{ID: tc.ID, Name: tc.Name, Arguments: args})
		}
		out = append(out, ports.Message{
			Role:             ports.MessageRole(m.Role),
			Content:          m.Content,
			ReasoningContent: m.ReasoningContent,
			ToolCalls:        calls,
			ToolCallID:       m.ToolCallID,
		})
	}
	return out
}

// toPortsToolDefs 把 provider 的工具 schema 转换成 ports 的工具 schema。
func toPortsToolDefs(defs []provider.ToolDefinition) []types.ToolDefinition {
	out := make([]types.ToolDefinition, 0, len(defs))
	for _, d := range defs {
		out = append(out, types.ToolDefinition{
			Name:        d.Name,
			Description: d.Description,
			Parameters:  d.Parameters,
		})
	}
	return out
}

// fromPortsResponse 把 ports 的响应转换回 provider 的响应。
func fromPortsResponse(resp ports.ProviderResponse) provider.CompletionResponse {
	calls := make([]provider.ToolCall, 0, len(resp.ToolCalls))
	for _, tc := range resp.ToolCalls {
		args, err := json.Marshal(tc.Arguments)
		if err != nil {
			// 参数是模型给的任意 JSON 值，理论上总能序列化；真失败时给一个空对象
			// 让工具运行时按"参数不合法"回给模型，而不是让整个子 Agent 崩掉。
			args = []byte("{}")
		}
		calls = append(calls, provider.ToolCall{ID: tc.ID, Name: tc.Name, Arguments: string(args)})
	}

	out := provider.CompletionResponse{
		FinishReason:     provider.FinishReason(resp.FinishReason),
		Content:          resp.Content,
		ReasoningContent: resp.ReasoningContent,
		ToolCalls:        calls,
		Emitted:          resp.Emitted,
	}
	if resp.Usage != (ports.Usage{}) {
		out.Usage = &provider.TokenUsage{
			PromptTokens:     resp.Usage.PromptTokens,
			CompletionTokens: resp.Usage.CompletionTokens,
			TotalTokens:      resp.Usage.TotalTokens,
			CacheHitTokens:   resp.Usage.CacheHitTokens,
			CacheMissTokens:  resp.Usage.CacheMissTokens,
			ReasoningTokens:  resp.Usage.ReasoningTokens,
		}
	}
	return out
}
