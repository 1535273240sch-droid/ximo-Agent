package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ximo888ok-netizen/ximo-agent/internal/expert"
	"github.com/ximo888ok-netizen/ximo-agent/internal/ports/mem"
	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// ---------------------------------------------------------------------------
// 专家子系统的行为修复（工具可用性 / 假完成 / closure / 模型选择）
// ---------------------------------------------------------------------------

// closureEvents 返回某次 run 记录的全部 run.closure 事件。
func closureEvents(t *testing.T, h *harness, runID string) []types.Event {
	t.Helper()
	var out []types.Event
	for _, ev := range h.events.All(runID) {
		if ev.Type == types.EventRunClosure {
			out = append(out, ev)
		}
	}
	return out
}

// eventIndex 返回某类事件第一次出现的下标；-1 表示不存在。
func eventIndex(events []types.Event, want types.EventType) int {
	for i, ev := range events {
		if ev.Type == want {
			return i
		}
	}
	return -1
}

// TestExpertRunHonoursExplicitModel 锁住「用户手选的模型必须真的被用上」。
//
// 此前专家/集群路径把 SubmitPayload.model 直接丢掉：有模型池时池选路会用候选
// 覆盖它，没有池时 provider 适配层回退到配置里的默认模型。用户在输入框旁选了
// 模型却跑出另一个模型的答案，是这个子系统最难自查的一类偏差。
func TestExpertRunHonoursExplicitModel(t *testing.T) {
	h := newExpertHarness(t,
		mem.FinalRound("1. 计划"),
		mem.FinalRound("按计划完成"),
	)

	ctx := context.Background()
	handle, err := h.engine.Submit(ctx, types.SubmitRequest{
		Prompt:   "审查代码",
		ExpertID: "engineering-frontend-developer",
		Model:    "chosen-model-42",
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if err := h.engine.WaitRun(ctx, handle.RunID); err != nil {
		t.Fatalf("WaitRun: %v", err)
	}

	if len(h.provider.Calls) == 0 {
		t.Fatal("子 Agent 没有发起任何模型调用")
	}
	for i, call := range h.provider.Calls {
		if call.Model != "chosen-model-42" {
			t.Errorf("第 %d 次调用的 model = %q，期望 chosen-model-42（用户显式选择被忽略了）",
				i+1, call.Model)
		}
	}
}

// TestExpertRunWithoutExplicitModelKeepsProviderDefault 确认没有显式选择时
// 不会凭空填一个模型名 —— 适配层需要靠空值回退到配置里的默认模型。
func TestExpertRunWithoutExplicitModelKeepsProviderDefault(t *testing.T) {
	h := newExpertHarness(t,
		mem.FinalRound("1. 计划"),
		mem.FinalRound("完成"),
	)

	ctx := context.Background()
	handle, err := h.engine.Submit(ctx, types.SubmitRequest{
		Prompt:   "审查代码",
		ExpertID: "engineering-frontend-developer",
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if err := h.engine.WaitRun(ctx, handle.RunID); err != nil {
		t.Fatalf("WaitRun: %v", err)
	}

	for i, call := range h.provider.Calls {
		if call.Model != "" {
			t.Errorf("第 %d 次调用的 model = %q，期望空值（由适配层回退默认模型）",
				i+1, call.Model)
		}
	}
}

// TestExpertRunEmitsClosureBeforeTerminal 是「收尾判据不能缺」的验收。
//
// run.closure 此前只由主 Agent 循环发出，专家/集群路径一条都不发，于是前端工作
// 卡片（steps.ts 读 run.closure）对这两条路径永远没有结论，用户也无法判断要不要
// 再来一轮。契约要求：恰好一次，且在终态跃迁之前。
func TestExpertRunEmitsClosureBeforeTerminal(t *testing.T) {
	h := newExpertHarness(t,
		mem.FinalRound("1. 计划"),
		mem.FinalRound("专家结论"),
	)

	ctx := context.Background()
	handle, err := h.engine.Submit(ctx, types.SubmitRequest{
		Prompt:   "审查代码",
		ExpertID: "engineering-frontend-developer",
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if err := h.engine.WaitRun(ctx, handle.RunID); err != nil {
		t.Fatalf("WaitRun: %v", err)
	}

	closures := closureEvents(t, h, handle.RunID)
	if len(closures) != 1 {
		t.Fatalf("run.closure 事件数 = %d，期望恰好 1 条", len(closures))
	}
	if got := fmt.Sprint(closures[0].Data["verdict"]); got != types.ClosureClosed {
		t.Errorf("verdict = %q，期望 %q（答案非空、无降级）", got, types.ClosureClosed)
	}
	if got, ok := closures[0].Data["expertPath"]; !ok || got != true {
		t.Errorf("closure 未标记 expertPath: %v", closures[0].Data)
	}
	if got := fmt.Sprint(closures[0].Data["degraded"]); got != "0" {
		t.Errorf("degraded = %q，期望 0", got)
	}

	events := h.events.All(handle.RunID)
	final, closure, terminal := eventIndex(events, types.EventFinalAnswer),
		eventIndex(events, types.EventRunClosure),
		eventIndex(events, types.EventRunCompleted)
	if final < 0 || closure < 0 || terminal < 0 {
		t.Fatalf("事件缺失：final_answer=%d closure=%d completed=%d", final, closure, terminal)
	}
	if !(final < closure && closure < terminal) {
		t.Errorf("事件顺序错误：final_answer=%d closure=%d terminal=%d；"+
			"closure 必须在答案之后、终态之前（终态一到事件流就关闭）", final, closure, terminal)
	}
}

// TestExpertRunDegradedIsFailedNotCompleted 是「假完成」修复的验收。
//
// 子 Agent 失败时 Orchestrator 返回的是「专家信息 + 手动指引」的降级文案，而
// 这段文案非空。此前它被当作正常产出并以 completed 收尾：用户拿到一份看起来
// 像专家结论的东西，实际没有任何专家跑过。
func TestExpertRunDegradedIsFailedNotCompleted(t *testing.T) {
	h := newExpertHarness(t)
	h.provider.Err = errors.New("provider 崩了")

	ctx := context.Background()
	handle, err := h.engine.Submit(ctx, types.SubmitRequest{
		Prompt:   "审查代码",
		ExpertID: "engineering-frontend-developer",
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if err := h.engine.WaitRun(ctx, handle.RunID); err != nil {
		t.Fatalf("WaitRun: %v", err)
	}

	run, err := h.engine.GetRun(ctx, handle.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.State != types.StateFailed {
		t.Fatalf("state = %s，期望 failed（子 Agent 没真正执行时报 completed 就是假完成）",
			run.State)
	}

	// 降级指引仍然要发给用户：失败不等于什么都不给。
	if !strings.Contains(run.Answer, "子 Agent 调用失败") {
		t.Errorf("answer 缺少降级说明：%q", run.Answer)
	}

	var degraded bool
	for _, ev := range h.events.All(handle.RunID) {
		if ev.Type == types.EventError && ev.Data != nil && ev.Data["degradedToInfo"] == true {
			degraded = true
		}
	}
	if !degraded {
		t.Error("没有记录 degradedToInfo 错误事件，界面无法解释为什么没有专家产出")
	}

	closures := closureEvents(t, h, handle.RunID)
	if len(closures) != 1 {
		t.Fatalf("run.closure 事件数 = %d，期望 1", len(closures))
	}
	if got := fmt.Sprint(closures[0].Data["verdict"]); got != types.ClosureFailed {
		t.Errorf("verdict = %q，期望 %q", got, types.ClosureFailed)
	}
}

// TestClusterRunAllDegradedFails 验证集群「全员降级」不再报 completed。
//
// 报告文案里那种「N 位专家并行」的多视角报告，如果实际一位专家都没跑，就只是
// 降级指引的拼装。终态必须如实报 failed，事件里也要给出真正执行的人数。
func TestClusterRunAllDegradedFails(t *testing.T) {
	h := newExpertHarness(t)
	h.provider.Err = errors.New("provider 崩了")

	ctx := context.Background()
	handle, err := h.engine.Submit(ctx, types.SubmitRequest{
		Prompt:      "设计一个消息网关",
		ClusterSize: 2,
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if err := h.engine.WaitRun(ctx, handle.RunID); err != nil {
		t.Fatalf("WaitRun: %v", err)
	}

	run, err := h.engine.GetRun(ctx, handle.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.State != types.StateFailed {
		t.Fatalf("state = %s，期望 failed（全部专家降级时不能报 completed）", run.State)
	}
	if !strings.Contains(run.Answer, "没有任何专家真正执行成功") {
		t.Errorf("报告没有明确说明没有专家产出：%q", run.Answer)
	}

	var executed, degraded any
	for _, ev := range h.events.All(handle.RunID) {
		if ev.Type != types.EventFinalAnswer || ev.Data == nil {
			continue
		}
		executed, degraded = ev.Data["executedExperts"], ev.Data["degradedExperts"]
	}
	if fmt.Sprint(executed) != "0" || fmt.Sprint(degraded) != "2" {
		t.Errorf("final_answer 的 experts 统计 = executed:%v degraded:%v，期望 0/2",
			executed, degraded)
	}

	closures := closureEvents(t, h, handle.RunID)
	if len(closures) != 1 {
		t.Fatalf("run.closure 事件数 = %d，期望 1", len(closures))
	}
	if got := fmt.Sprint(closures[0].Data["verdict"]); got != types.ClosureFailed {
		t.Errorf("verdict = %q，期望 %q", got, types.ClosureFailed)
	}
}

// TestClusterRunEmitsClosureWithDegradedCount 验证「部分专家降级」得到 partial，
// 而不是看起来干完了的 closed。
func TestClusterRunEmitsClosureWithDegradedCount(t *testing.T) {
	results := []clusterResult{
		// 一位真正执行。
		{
			Expert: expert.Expert{ID: "e1", Name: "专家一"},
			Outcome: &expert.ExpertOutcome{
				SubAgentMode: true,
				Content:      "真结论",
			},
		},
		// 一位降级：有内容，但内容是手动指引。
		{
			Expert: expert.Expert{ID: "e2", Name: "专家二"},
			Outcome: &expert.ExpertOutcome{
				SubAgentMode: false,
				Error:        "限流",
				Content:      "子 Agent 调用失败：限流",
			},
		},
		// 一位直接报错。
		{Expert: expert.Expert{ID: "e3", Name: "专家三"}, Err: errors.New("蹦了")},
	}

	report, executed := buildClusterReport("任务", results)
	if executed != 1 {
		t.Fatalf("executed = %d，期望 1（只有第一位真正执行）", executed)
	}
	if !strings.Contains(report, "集群共 3 位专家，1 位真正执行并产出") {
		t.Errorf("统计口径不对：%q", report)
	}
	// 数字必须对得上：1 位降级 + 1 位报错 = 2 位未真正执行。
	if !strings.Contains(report, "另有 2 位未真正执行") {
		t.Errorf("报告没有如实说明未执行的专家数：%q", report)
	}
	if !strings.Contains(report, "1 位为降级指引，1 位执行报错") {
		t.Errorf("报告没有区分降级与报错：%q", report)
	}

	// 同一条统计口径必须驱动 closure：答案非空但有人降级 → partial。
	rep := types.BuildRunClosureReport(nil, types.ClosureInput{
		Reason:   types.ClosureReasonCompleted,
		Answer:   report,
		Degraded: len(results) - executed,
	})
	if rep.Verdict != types.ClosurePartial {
		t.Errorf("verdict = %q，期望 %q（有专家降级就不算干完）", rep.Verdict, types.ClosurePartial)
	}
	var found bool
	for _, c := range rep.Checks {
		if c.ID == types.CheckExpertsExecuted {
			found = true
			if c.Pass {
				t.Error("experts_executed 检查不应通过")
			}
		}
	}
	if !found {
		t.Error("closure 报告缺少 experts_executed 检查")
	}
}
