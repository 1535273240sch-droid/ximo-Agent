// cluster_run.go —— Agent 集群模式：一次提交，多位专家子代理并行处理，汇总产出。
//
// 与专家直连（executeExpertRun）的分工：
//   - 直连：用户点了一位专家 → 那位专家独自完成任务（两阶段编排）。
//   - 集群：用户没点专家、但打开了「集群模式」→ 引擎按任务内容自动挑选
//     多位专家，并行处理同一个任务，最后汇总成一份多视角报告。
//
// 实现刻意复用现成的 expert.Orchestrator：Provider、子代理的工具执行器
// （expertToolExecutor）、expert_agent 资源闸门都已由 newExpertOrchestrator
// 接好。集群不复制子代理的执行逻辑，只是把它并发调用 N 次 —— 并发上限天然
// 由 ResourceClassExpertAgent 的容量（8）兜住，不会绕过限流。
package engine

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/expert"
	"github.com/ximo888ok-netizen/ximo-agent/internal/ports"
	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// clusterMaxSize 是集群规模上限。
//
// 与 types.ResourceCapacities[ResourceClassExpertAgent] 保持一致：设置面板里
// 对用户承诺的是「复杂任务会拆给多位专家子代理并行处理（同时最多 8 个）」，
// 集群派出的子代理数不能超出这个数字，否则承诺就失效了。
const clusterMaxSize = 8

// executeClusterRun 以「Agent 集群」模式处理本次 run。
//
// 终态与其它路径保持一致（成功 completed + answer / 取消 cancelled /
// 失败 failed），前端渲染与对账无需为集群开特例。
func (e *Engine) executeClusterRun(ctx context.Context, rec *runRecord) {
	runID := rec.runID()
	sessionID := rec.sessionID()

	// 终态收敛口：closure 与终态跃迁成对出现（与专家直连同款），
	// 详见 emitRunClosure 的说明。
	fail := func(cause error) {
		e.emitRunClosure(ctx, rec, types.ClosureReasonFailed, "", 0)
		e.finishRun(ctx, rec, types.StateFailed, "", cause)
	}
	cancel := func() {
		e.emitRunClosure(ctx, rec, types.ClosureReasonCancelled, "", 0)
		e.finishRun(ctx, rec, types.StateCancelled, "",
			types.NewError(types.CodeCancelled, "run cancelled"))
	}

	// 专家库与 Provider 是硬依赖：缺任何一个，集群都无从谈起。这里明确失败
	// 而不是静默退回主循环 —— 用户是主动打开了集群开关的，退回通用路径会得到
	// 一个"看起来正常但一个子代理都没跑"的结果，比直接报错更难排查。
	if e.expertRegistry == nil {
		fail(types.NewError(types.CodeInternal, "专家库不可用"))
		return
	}
	if e.deps.Provider == nil {
		fail(types.NewError(types.CodeInternal, "provider is not configured"))
		return
	}

	size := rec.request.ClusterSize
	if size <= 0 {
		size = clusterMaxSize
	}
	if size > clusterMaxSize {
		size = clusterMaxSize
	}

	selected, err := e.selectClusterExperts(rec.request.Prompt, size)
	if err != nil || len(selected) == 0 {
		msg := "未能为本次任务挑选出任何专家"
		if err != nil {
			msg = types.RedactString(err.Error())
		}
		e.publishAndAppend(ctx, types.Event{
			RunID: runID, SessionID: sessionID,
			Type: types.EventError, State: types.StateThinking,
			Timestamp: time.Now(),
			Message:   msg,
			Data:      map[string]any{"reason": "no_expert_selected"},
		})
		fail(types.NewError(types.CodeInternal, "no expert selected for cluster run"))
		return
	}

	ids := make([]string, 0, len(selected))
	for _, ex := range selected {
		ids = append(ids, ex.ID)
	}
	e.publishAndAppend(ctx, types.Event{
		RunID: runID, SessionID: sessionID,
		// 用 EventRunStateChanged（持久事件）而不是 EventProgress：
		// 合并器只保留 content/reasoning 载荷，挂在 progress 上的结构化字段
		// 会在合并时被静默剥掉；而且只有 Durable 事件才会落库，断线重连或
		// run 被驱逐后重建时才能从日志里补回「本次派出了多少子代理」。
		// 与 emitExpertPhase 的选择保持一致。
		Type:      types.EventRunStateChanged,
		State:     types.StateThinking,
		Timestamp: time.Now(),
		Message:   fmt.Sprintf("Agent 集群启动：%d 位专家并行处理", len(selected)),
		Data: map[string]any{
			"clusterSize": len(selected),
			"expertIds":   ids,
		},
	})

	// 子代理的工具调用必须带上 run 上下文并走 Engine 的受控派发链路
	// （admission → 公平队列 → 资源租约）。与专家直连用同一套机制。
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

	results := e.runClusterExperts(runCtx, ctx, rec, selected)

	// 取消优先级最高：只要 context 被取消，绝不落任何作废结果。
	if ctx.Err() != nil || rec.terminal() {
		cancel()
		return
	}

	answer, executed := buildClusterReport(rec.request.Prompt, results)
	degraded := len(results) - executed

	// 最终回答。事件顺序与其它执行路径一致：先 final_answer，再终态跃迁 ——
	// 反过来，订阅者会在流结束前看到 "completed" 却拿不到答案。
	// finishRun 只负责终态与落库，它不会替调用方补这个事件。
	e.publishAndAppend(ctx, types.Event{
		RunID: runID, SessionID: sessionID,
		Type: types.EventFinalAnswer, State: types.StateThinking,
		Timestamp: time.Now(),
		Message:   answer,
		Data: map[string]any{
			"answer":      answer,
			"clusterSize": len(results),
			"expertIds":   ids,
			// 真正跑过子代理的专家数。前端（与事后排查）据此判断这份报告里
			// 有多少是真结论，而不是去数正文里的警告行。
			"executedExperts": executed,
			"degradedExperts": degraded,
		},
	})

	// 一位都没真正执行 → 这次 run 没有任何专家结论，必须报 failed。
	//
	// 此前无论结果如何都以 StateCompleted 收尾，而内容此时是「N 位专家的降级
	// 指引」拼装 —— 用户看到的是一个"已完成"的多专家报告，实际一个专家都没跑。
	// 这正是审核文档要求消除的假完成（与 v2.3.0 的 closure 修复同类）。
	if executed == 0 {
		e.emitRunClosure(ctx, rec, types.ClosureReasonFailed, "", degraded)
		e.finishRun(ctx, rec, types.StateFailed, answer, types.NewError(types.CodeInternal,
			"集群内 %d 位专家均未真正执行，报告内容为降级指引", len(results)))
		return
	}

	e.emitRunClosure(ctx, rec, types.ClosureReasonCompleted, answer, degraded)
	e.finishRun(ctx, rec, types.StateCompleted, answer, nil)
}

// clusterResult 单个专家子代理的执行结果。
type clusterResult struct {
	Expert  expert.Expert
	Outcome *expert.ExpertOutcome
	Err     error
}

// runClusterExperts 并行激活全部选中的专家。
//
// 每个 goroutine 用自己的 Orchestrator 实例：Orchestrator 持有 OnPhase 等
// 可变字段，共享一个实例就必须为它们加锁。而真正需要共享的是资源池
// （e.Scheduler().Resources()），newExpertOrchestrator 每次都接同一个池，
// 所以「同时在跑的子代理 ≤ 8」这条硬上限对集群同样成立 —— 超出的调用会在
// acquireExpertSlot 里 FIFO 排队，而不是被拒绝。
func (e *Engine) runClusterExperts(
	runCtx context.Context,
	lifecycleCtx context.Context,
	rec *runRecord,
	selected []expert.Expert,
) []clusterResult {
	results := make([]clusterResult, len(selected))
	var wg sync.WaitGroup

	for i, ex := range selected {
		wg.Add(1)
		go func(idx int, ex expert.Expert) {
			defer wg.Done()
			results[idx] = e.runOneClusterExpert(runCtx, lifecycleCtx, rec, ex)
		}(i, ex)
	}
	wg.Wait()
	return results
}

// runOneClusterExpert 跑单个专家的子代理会话。
func (e *Engine) runOneClusterExpert(
	runCtx context.Context,
	lifecycleCtx context.Context,
	rec *runRecord,
	ex expert.Expert,
) clusterResult {
	out := clusterResult{Expert: ex}

	// 生命周期已结束就不再起新的模型调用（例如用户已经取消）。
	if lifecycleCtx.Err() != nil {
		out.Err = lifecycleCtx.Err()
		return out
	}

	orch := e.newExpertOrchestrator(rec.request.Model)
	// 阶段跃迁事件：集群里同样要让「谁在规划/谁在实施」可见。
	// 回调与 Activate 在同一 goroutine 上执行，且每个 goroutine 有独立实例，
	// 因此这里不需要加锁。
	orch.OnPhase = func(ph expert.PhaseEvent) {
		if lifecycleCtx.Err() != nil || rec.terminal() {
			return
		}
		e.emitExpertPhase(lifecycleCtx, rec, ph)
	}

	res, err := orch.Activate(runCtx, expert.ExpertRequest{
		ExpertID: ex.ID,
		Task:     rec.request.Prompt,
	})
	out.Outcome = res
	out.Err = err
	return out
}

// selectClusterExperts 按任务内容挑选集群成员。
//
// 先用注册表的 Search 按任务关键词匹配（与 v1 agent_expert 的 search 分支
// 同一套匹配字段），匹配不足时用全量专家补齐到目标规模 —— 集群规模是用户
// 显式选择的，不能因为关键词没命中就悄悄缩水。
func (e *Engine) selectClusterExperts(task string, n int) ([]expert.Expert, error) {
	out := make([]expert.Expert, 0, n)
	seen := make(map[string]bool, n)

	appendExpert := func(ex expert.Expert) {
		if len(out) >= n || seen[ex.ID] {
			return
		}
		seen[ex.ID] = true
		out = append(out, ex)
	}

	matched, err := e.expertRegistry.Search(task)
	if err != nil {
		return nil, err
	}
	for _, ex := range matched {
		if len(out) >= n {
			break
		}
		appendExpert(ex)
	}

	if len(out) < n {
		all, loadErr := e.expertRegistry.Load()
		if loadErr != nil {
			if len(out) == 0 {
				return nil, loadErr
			}
			return out, nil
		}
		for _, ex := range all {
			if len(out) >= n {
				break
			}
			appendExpert(ex)
		}
	}
	return out, nil
}

// buildClusterReport 把各专家的产出汇总成一份带来源标注的报告。
//
// 刻意不做「再调一次模型来综合」：那会引入一次额外的、不可控的模型调用，
// 且失败时整份报告都会丢。直接拼装既保留了每位专家的原始判断（用户能自己
// 交叉验证），也保证只要有专家跑成功就一定交付得出东西。
//
// 第二个返回值是**真正执行并产出**的专家数。「真正执行」的判据只有一处定义
// （ranSubAgent），调用方据此决定终态（一位都没跑 = failed）与 closure 判据，
// 避免统计口径在报告文案与终态之间漂移。
func buildClusterReport(task string, results []clusterResult) (string, int) {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("# Agent 集群报告（%d 位专家并行）\n\n", len(results)))
	if strings.TrimSpace(task) != "" {
		sb.WriteString("**任务**：")
		sb.WriteString(strings.TrimSpace(task))
		sb.WriteString("\n\n")
	}

	// 统计口径：只有**真的跑过子代理**（SubAgentMode=true 且无错误）才算「产出」。
	//
	// 这里曾经只判断 Content 非空就 succeeded++，而子代理失败时 Content 恰恰是那段
	// 「子 Agent 调用失败：…」的降级文案（orchestrator.go buildFallbackContent）。
	// 于是 8 位专家全挂的报告会以「8 位给出了产出」收尾——与本次改动要修的
	// 「让用户以为完成了其实没完成」是同一类错误，只是发生在集群路径上。
	executed := 0
	degraded := 0
	failed := 0
	for i, r := range results {
		name := r.Expert.Name
		if name == "" {
			name = r.Expert.ID
		}
		sb.WriteString(fmt.Sprintf("## %d. %s\n\n", i+1, name))
		if r.Expert.Division != "" {
			sb.WriteString(fmt.Sprintf("*领域：%s*\n\n", r.Expert.Division))
		}

		if r.Err != nil {
			failed++
			sb.WriteString("> ⚠️ 该专家执行失败：")
			sb.WriteString(types.RedactString(r.Err.Error()))
			sb.WriteString("\n\n")
			continue
		}
		if r.Outcome == nil {
			failed++
			sb.WriteString("> ⚠️ 该专家未返回结果。\n\n")
			continue
		}

		body := strings.TrimSpace(r.Outcome.Content)
		if body == "" {
			failed++
			sb.WriteString("> ⚠️ 该专家没有产出内容。\n\n")
			continue
		}

		// 子代理失败时会降级成「手动指引」，那段文案是有内容的，但它不是产出。
		// 先如实标注，再决定是否计入统计——顺序很重要：用户必须先看到这是降级。
		ranSubAgent := r.Outcome.SubAgentMode && r.Outcome.Error == ""
		if !ranSubAgent {
			degraded++
			sb.WriteString("> ⚠️ 该专家**未真正执行**（已降级为手动指引）")
			if r.Outcome.Error != "" {
				sb.WriteString("：")
				sb.WriteString(types.RedactString(r.Outcome.Error))
			}
			sb.WriteString("\n\n")
		}

		sb.WriteString(body)
		sb.WriteString("\n\n")
		if ranSubAgent {
			executed++
		}
	}

	// 收尾统计必须让数字对得上：executed + degraded + failed == len(results)。
	// 此前只提「未真正执行……其内容为手动指引」的降级数，把「执行直接报错」的
	// 专家整段漏掉，读起来像是全部专家都有产出、只是少数降级了。
	sb.WriteString("---\n\n")
	sb.WriteString(fmt.Sprintf("集群共 %d 位专家，%d 位真正执行并产出。", len(results), executed))
	switch notExecuted := degraded + failed; {
	case notExecuted > 0 && degraded > 0 && failed > 0:
		sb.WriteString(fmt.Sprintf(" 另有 %d 位未真正执行（%d 位为降级指引，%d 位执行报错）。",
			notExecuted, degraded, failed))
	case notExecuted > 0 && degraded > 0:
		sb.WriteString(fmt.Sprintf(" 另有 %d 位未真正执行，其内容为手动指引。", notExecuted))
	case notExecuted > 0:
		sb.WriteString(fmt.Sprintf(" 另有 %d 位执行报错，没有任何产出。", notExecuted))
	}
	if executed == 0 {
		// 全挂时不能只靠一句统计让用户自己去数：明确说这份报告没有专家产出。
		sb.WriteString("\n\n> ⚠️ 本次没有任何专家真正执行成功，以上内容均为降级指引，不代表专家结论。")
	}
	return sb.String(), executed
}
