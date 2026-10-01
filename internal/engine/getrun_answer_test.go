package engine

import (
	"context"
	"testing"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// TestFillAnswerFromLogCoversTheTerminalGap 复现并锁住「已完成但没有回答」的窗口。
//
// finishRun 必须先做状态跃迁（产生 run.state_changed）、再 CloseRun（产生
// run.completed 并把答案写进 actor）—— 顺序不能反，否则会在终态事件之后继续写事件
// （事件流在终态关闭）。两步之间必然存在一个瞬间：actor 说 completed、答案是空的。
// GetRun 是状态轮询与 IPC 状态帧的来源，谁在这个瞬间读到它，就会看到一条没有回复的
// 完成记录。读取方必须在终态 + 空答案时回到日志取权威答案。
func TestFillAnswerFromLogCoversTheTerminalGap(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// 日志里有一条 final_answer（真实运行中它先于状态跃迁落库）。
	runID := "run-gap"
	if _, err := h.events.Append(ctx, runID, types.Event{
		RunID: runID, SessionID: "ses-1",
		Type:  types.EventFinalAnswer,
		State: types.StateThinking, Round: 1,
		Timestamp: time.Now(),
		Message:   "这是最终回答",
	}); err != nil {
		t.Fatalf("append final_answer: %v", err)
	}

	// 模拟中间窗口：状态已终态、答案为空。
	got := h.engine.fillAnswerFromLog(ctx, runID, types.Run{
		ID: runID, SessionID: "ses-1", State: types.StateCompleted,
	})
	if got.Answer != "这是最终回答" {
		t.Errorf("answer = %q，期望从日志回填（否则界面显示「已完成但没有回答」）", got.Answer)
	}
}

// TestFillAnswerFromLogLeavesNonTerminalRunsAlone 确认运行中的 run 不会被回填干扰：
// 它的答案确实还没有，读日志只会读到历史事件。
func TestFillAnswerFromLogLeavesNonTerminalRunsAlone(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	runID := "run-running"
	if _, err := h.events.Append(ctx, runID, types.Event{
		RunID: runID, SessionID: "ses-1",
		Type: types.EventFinalAnswer, State: types.StateThinking,
		Timestamp: time.Now(), Message: "上一轮的答案",
	}); err != nil {
		t.Fatalf("append: %v", err)
	}

	got := h.engine.fillAnswerFromLog(ctx, runID, types.Run{
		ID: runID, State: types.StateThinking,
	})
	if got.Answer != "" {
		t.Errorf("运行中的 run 不该被回填答案，实际 %q", got.Answer)
	}
}

// TestFillAnswerFromLogKeepsExistingAnswer 确认已经带上答案的快照原样返回 ——
// 这条路径是常态（绝大多数读取发生在窗口之外），它必须零成本。
func TestFillAnswerFromLogKeepsExistingAnswer(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	run := types.Run{ID: "run-done", State: types.StateCompleted, Answer: "actor 里的答案"}
	got := h.engine.fillAnswerFromLog(ctx, run.ID, run)
	if got.Answer != "actor 里的答案" {
		t.Errorf("answer = %q，期望原样保留", got.Answer)
	}
}

// TestGetRunReturnsAnswerForCompletedRun 是端到端版本：正常跑完的 run 通过 GetRun
// 读到的答案必须完整（这条在并发负载下曾经偶发读到空答案）。
func TestGetRunReturnsAnswerForCompletedRun(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	handle, err := h.engine.Submit(ctx, types.SubmitRequest{Prompt: "说一句话"})
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
	if run.State != types.StateCompleted {
		t.Fatalf("state = %s，期望 completed", run.State)
	}
	if run.Answer == "" {
		t.Fatal("完成的 run 通过 GetRun 读到的答案为空")
	}
}
