package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/expert"
	"github.com/ximo888ok-netizen/ximo-agent/internal/ports"
	"github.com/ximo888ok-netizen/ximo-agent/internal/ports/mem"
	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// ---------------------------------------------------------------------------
// 子 Agent 的工作阶段可见性（expert.work）
//
// 此前专家/集群路径只把子代理的 WorkEvent 收进 Outcome.Events，然后在
// final_answer 里折成一个 workEvents 计数丢掉：界面看到的是一条永远不结束的
// 「tool」行（引擎为子代理工具调用发的 tool_call.started），既不知道子代理在调
// 什么工具，也看不到结果。这一组用例锁住新的契约：每个阶段恰好进持久日志一次，
// 且绝不越过终态。
// ---------------------------------------------------------------------------

// expertWorkEvents 返回某次 run 记录的全部 expert.work 事件（按日志顺序）。
func expertWorkEvents(h *harness, runID string) []types.Event {
	var out []types.Event
	for _, ev := range h.events.All(runID) {
		if ev.Type == types.EventExpertWork {
			out = append(out, ev)
		}
	}
	return out
}

// firstTerminalIndex 返回第一条终态事件（completed/failed/cancelled）的下标；
// -1 表示日志里没有终态事件。
func firstTerminalIndex(events []types.Event) int {
	for i, ev := range events {
		switch ev.Type {
		case types.EventRunCompleted, types.EventRunFailed, types.EventRunCancelled:
			return i
		}
	}
	return -1
}

// expertWorkStages 汇总事件里的 stage，仅用于失败时的可读报错。
func expertWorkStages(works []types.Event) []string {
	out := make([]string, 0, len(works))
	for _, ev := range works {
		s, _ := ev.Data["stage"].(string)
		out = append(out, s)
	}
	return out
}

// expertWorkStage 取一条事件的 stage；缺字段时返回空串（只用于断言，不 panic）。
func expertWorkStage(ev types.Event) string {
	s, _ := ev.Data["stage"].(string)
	return s
}

// scriptedExpertToolRun 跑完一次「子 Agent 会调用一次 file_read」的专家 run。
//
// 脚本三段对应两条真实路径：规划阶段（无工具）→ 实施阶段的工具轮 → 实施阶段的
// 最终回答。用例 1/2 共用同一现场，只有在脚本一致的前提下「恰好一次」才有意义。
func scriptedExpertToolRun(t *testing.T) (*harness, string) {
	t.Helper()
	h := newExpertHarness(t,
		mem.FinalRound("## 方案\n1. 读文件"),
		mem.ToolCallRound(mem.NewCall("call-1", "file_read", nil)),
		mem.FinalRound("读完了"),
	)
	h.tools.Handler = func(_ context.Context, req ports.ToolRequest) (types.ToolResult, error) {
		return types.ToolResult{
			ToolCallID: req.ToolCallID, ToolName: req.ToolName,
			Success: true, Content: "file contents",
		}, nil
	}

	ctx := context.Background()
	handle, err := h.engine.Submit(ctx, types.SubmitRequest{
		Prompt:   "帮我审查这段代码",
		ExpertID: "engineering-frontend-developer",
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if err := h.engine.WaitRun(ctx, handle.RunID); err != nil {
		t.Fatalf("WaitRun: %v", err)
	}
	return h, handle.RunID
}

// TestExpertRunPublishesSubAgentWorkEvents 是「子代理阶段必须可见」的验收：
// 子 Agent 调了一次工具，日志里就必须有对应的 expert.work。
func TestExpertRunPublishesSubAgentWorkEvents(t *testing.T) {
	h, runID := scriptedExpertToolRun(t)

	works := expertWorkEvents(h, runID)
	if len(works) == 0 {
		t.Fatal("没有 expert.work 事件：子代理的工作阶段仍然不可见")
	}

	var toolEv *types.Event
	for i := range works {
		if works[i].Data["stage"] == string(expert.StageTool) {
			toolEv = &works[i]
		}
	}
	if toolEv == nil {
		t.Fatalf("没有 stage=%q 的事件；实际阶段 = %v", expert.StageTool, expertWorkStages(works))
	}

	// Data 必须带得出「谁、在哪一步」：前端的稳定 id 就是 expertId+stage。
	if got := toolEv.Data["expertId"]; got != "engineering-frontend-developer" {
		t.Errorf("expertId = %v，期望 engineering-frontend-developer", got)
	}
	if got := expertWorkStage(*toolEv); got == "" {
		t.Error("stage 为空：前端无法为这一步生成稳定 id")
	}
	if got, _ := toolEv.Data["expertName"].(string); got == "" {
		t.Error("expertName 为空：时间线只会显示一个 ID")
	}
	if got, _ := toolEv.Data["timestamp"].(int64); got == 0 {
		t.Errorf("timestamp = %v，期望子代理上报的毫秒时间戳", toolEv.Data["timestamp"])
	}
	// 消息行要点名工具，否则这条事件和裸的 tool_call.started 没有区别。
	if !strings.Contains(toolEv.Message, "file_read") {
		t.Errorf("message = %q，期望点名被调用的工具", toolEv.Message)
	}
}

// TestExpertWorkEventsAreExactlyOnceBeforeTerminal 锁住两条不变式：
//  1. 每个上报的阶段恰好进日志一次（不漏发、不重复发）；
//  2. 全部 expert.work 都在终态事件之前 —— 终态一到事件流就关闭，之后写的事件
//     既到不了订阅者，还会把重放折叠出的状态拽回 thinking。
func TestExpertWorkEventsAreExactlyOnceBeforeTerminal(t *testing.T) {
	h, runID := scriptedExpertToolRun(t)

	events := h.events.All(runID)
	term := firstTerminalIndex(events)
	if term < 0 {
		t.Fatal("日志里没有任何终态事件")
	}

	var works []types.Event
	for i, ev := range events {
		if ev.Type != types.EventExpertWork {
			continue
		}
		if i > term {
			t.Errorf("第 %d 条 expert.work 出现在终态事件（下标 %d，%s）之后",
				i, term, events[term].Type)
		}
		works = append(works, ev)
	}

	// 「恰好一次」的判据：OnEvent 收到的阶段数 = 实施阶段的 Outcome.Events 条数
	// 加上规划阶段的 started/finished 两条（规划阶段只走 OnEvent，不进
	// EventSink，因此不会出现在 outcome.Events 里）。
	var finalWorkEvents int
	for _, ev := range events {
		if ev.Type == types.EventFinalAnswer && ev.Data != nil {
			finalWorkEvents, _ = ev.Data["workEvents"].(int)
		}
	}
	if finalWorkEvents == 0 {
		t.Fatal("final_answer 没有携带 workEvents：无法核对事件条数")
	}
	if len(works) != finalWorkEvents+2 {
		t.Errorf("expert.work 事件数 = %d，期望 %d（workEvents=%d + 规划阶段 2 条）；阶段 = %v",
			len(works), finalWorkEvents+2, finalWorkEvents, expertWorkStages(works))
	}

	// 一次工具调用 → 日志里只能有一条 tool / 一条 toolResult。
	counts := map[expert.Stage]int{}
	for _, ev := range works {
		counts[expert.Stage(expertWorkStage(ev))]++
	}
	if counts[expert.StageTool] != 1 || counts[expert.StageToolResult] != 1 {
		t.Errorf("工具阶段条数 = tool:%d toolResult:%d，期望各 1 条（重复发送会让前端多出一行）",
			counts[expert.StageTool], counts[expert.StageToolResult])
	}
}

// TestExpertWorkEventsStopAtCancel 是这条链路的陷阱：用户取消后，子 Agent 仍会
// 上报一条 StageFinished（subagent.go 先 emit 再判 ctx.Err），守卫必须把它拦在
// 库外 —— 已取消的 run 记录里出现终态之后的写入，会让它的状态折叠回 thinking。
func TestExpertWorkEventsStopAtCancel(t *testing.T) {
	release := make(chan struct{})
	p := mem.NewProvider(mem.FinalRound("should not be reached"))
	p.Block = release
	defer close(release)

	h := newExpertHarness(t)
	h.engine.deps.Provider = p
	h.engine.Close()
	engine, err := New(engineConfig(), Dependencies{
		Events: h.events, Outbox: h.outbox, Checkpoints: h.ckpts,
		Idempotency: h.idem, Tools: h.tools, Provider: p,
	}, h.guard)
	if err != nil {
		t.Fatal(err)
	}
	h.engine = engine
	defer engine.Close()

	ctx := context.Background()
	handle, err := h.engine.Submit(ctx, types.SubmitRequest{
		Prompt:   "慢慢来",
		ExpertID: "engineering-frontend-developer",
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	// 规划阶段的 StageStarted 在阻塞的 Complete 之前就已经发出，因此「有事件」
	// 是确定的；RoundCount() > 0 表示子代理确实卡在模型调用上。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && p.RoundCount() == 0 {
		time.Sleep(2 * time.Millisecond)
	}
	if p.RoundCount() == 0 {
		t.Fatal("the run never reached the provider")
	}

	if err := h.engine.Cancel(ctx, handle.RunID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if err := h.engine.WaitRun(ctx, handle.RunID); err != nil {
		t.Fatalf("WaitRun: %v", err)
	}
	run, err := h.engine.GetRun(ctx, handle.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.State != types.StateCancelled {
		t.Fatalf("state = %s，期望 cancelled", run.State)
	}

	events := h.events.All(handle.RunID)
	term := firstTerminalIndex(events)
	if term < 0 {
		t.Fatal("取消后的 run 没有任何终态事件")
	}
	// 终态（含）之后不允许再有任何 expert.work。
	for i := term; i < len(events); i++ {
		if events[i].Type == types.EventExpertWork {
			t.Errorf("取消后仍写入 expert.work（终态下标 %d，事件下标 %d）：stage=%v",
				term, i, events[i].Data["stage"])
		}
	}
	// 取消之前已经发出的阶段要保留：用户看得到专家被启动过，而不是一片空白。
	if works := expertWorkEvents(h, handle.RunID); len(works) == 0 {
		t.Error("取消前的 expert.work 事件不见了：守卫拦得过头")
	}
}
