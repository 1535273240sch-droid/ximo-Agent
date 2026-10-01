package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ximo888ok-netizen/ximo-agent/internal/ports/mem"
	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// fakeMemoryReporter 是同时实现 MemoryPort 与 MemoryRecallReporter 的测试替身
// （图记忆后端的 adapter 就是这个形状）。
type fakeMemoryReporter struct {
	fakeMemory
	items []MemoryRecallItem
}

func (f *fakeMemoryReporter) RecallDetailed(_ context.Context, query string) (string, []MemoryRecallItem) {
	f.recallCalls++
	f.recallQuery = query
	return f.block, f.items
}

var _ MemoryRecallReporter = (*fakeMemoryReporter)(nil)

// memoryRecalledEvent 在事件日志里找 memory.recalled。
func memoryRecalledEvent(t *testing.T, h *harness, runID string) (types.Event, bool) {
	t.Helper()
	var found types.Event
	ok := false
	for _, ev := range h.events.All(runID) {
		if ev.Type == types.EventMemoryRecalled {
			found = ev
			ok = true
		}
	}
	return found, ok
}

// TestSubmitEmitsMemoryRecalledEvent 是「Work Log 的『回忆 N 条记忆』步骤真的会
// 出现」这条验收的直接证据：事件必须落进 durable 日志，载荷形状为
// {count, items:[{id,text,via}]}，且在终态之前。
func TestSubmitEmitsMemoryRecalledEvent(t *testing.T) {
	h := newHarness(t, mem.FinalRound("完成"))
	fake := &fakeMemoryReporter{
		fakeMemory: fakeMemory{block: "记忆块"},
		items: []MemoryRecallItem{
			{ID: "mn_1", Text: "用户偏好深色主题", Via: "seed"},
			{ID: "mn_2", Text: "项目使用 Go 引擎", Via: "entity:XimoAgent → fact:并发偏好"},
		},
	}
	h.engine.deps.Memory = fake

	ctx := context.Background()
	handle, err := h.engine.Submit(ctx, types.SubmitRequest{Prompt: "改界面", SystemPrompt: "sys"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if err := h.engine.WaitRun(ctx, handle.RunID); err != nil {
		t.Fatalf("WaitRun: %v", err)
	}
	if fake.recallQuery != "改界面" {
		t.Fatalf("结构化召回应以本轮提示为查询词，实际 %q", fake.recallQuery)
	}

	ev, ok := memoryRecalledEvent(t, h, handle.RunID)
	if !ok {
		t.Fatal("事件日志里没有 memory.recalled")
	}
	raw, err := json.Marshal(ev.Data)
	if err != nil {
		t.Fatalf("序列化事件载荷: %v", err)
	}
	payload := string(raw)
	for _, want := range []string{
		`"count":2`,
		`"id":"mn_1"`, `"text":"用户偏好深色主题"`, `"via":"seed"`,
		`"id":"mn_2"`, `"via":"entity:XimoAgent → fact:并发偏好"`,
	} {
		if !strings.Contains(payload, want) {
			t.Fatalf("载荷里缺少 %s：%s", want, payload)
		}
	}

	// 事件必须先于终态（否则订阅方在流关闭后永远看不到它）。
	seenRecalled, seenCompleted := -1, -1
	for i, e := range h.events.All(handle.RunID) {
		switch e.Type {
		case types.EventMemoryRecalled:
			seenRecalled = i
		case types.EventRunCompleted:
			seenCompleted = i
		}
	}
	if seenCompleted < 0 || seenRecalled > seenCompleted {
		t.Fatalf("memory.recalled 应在 run.completed 之前，位置 recalled=%d completed=%d",
			seenRecalled, seenCompleted)
	}
}

// TestSubmitWithoutReporterEmitsNoEvent 是最重要的回归：只实现 MemoryPort 的
// 端口（老装配、老测试替身）行为不变——不产生任何额外事件。
func TestSubmitWithoutReporterEmitsNoEvent(t *testing.T) {
	h := newHarness(t, mem.FinalRound("完成"))
	h.engine.deps.Memory = &fakeMemory{block: "记忆块"}

	ctx := context.Background()
	handle, err := h.engine.Submit(ctx, types.SubmitRequest{Prompt: "干点活", SystemPrompt: "sys"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if err := h.engine.WaitRun(ctx, handle.RunID); err != nil {
		t.Fatalf("WaitRun: %v", err)
	}
	if _, ok := memoryRecalledEvent(t, h, handle.RunID); ok {
		t.Fatal("端口没有 RecallDetailed 能力时不应发出 memory.recalled")
	}
}

// TestMemoryRecalledEventRedactsText：事件是另一条出站路径（事件日志 / outbox /
// IPC），正文在出口处必须再脱敏一次。
func TestMemoryRecalledEventRedactsText(t *testing.T) {
	h := newHarness(t, mem.FinalRound("完成"))
	h.engine.deps.Memory = &fakeMemoryReporter{
		fakeMemory: fakeMemory{block: "记忆块"},
		items: []MemoryRecallItem{
			{ID: "mn_secret", Text: "部署用的 key 是 sk-live-abcdef0123456789", Via: "seed"},
		},
	}

	ctx := context.Background()
	handle, err := h.engine.Submit(ctx, types.SubmitRequest{Prompt: "部署", SystemPrompt: "sys"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if err := h.engine.WaitRun(ctx, handle.RunID); err != nil {
		t.Fatalf("WaitRun: %v", err)
	}
	ev, ok := memoryRecalledEvent(t, h, handle.RunID)
	if !ok {
		t.Fatal("事件日志里没有 memory.recalled")
	}
	raw, _ := json.Marshal(ev.Data)
	if strings.Contains(string(raw), "sk-live-abcdef0123456789") {
		t.Fatalf("事件载荷里出现了未脱敏的密钥: %s", raw)
	}
}

// TestSubmitWithoutMemoryDoesNotEmitRecalledEvent：未装配记忆时连探测都不该发生。
func TestSubmitWithoutMemoryDoesNotEmitRecalledEvent(t *testing.T) {
	h := newHarness(t, mem.FinalRound("完成"))
	ctx := context.Background()
	handle, err := h.engine.Submit(ctx, types.SubmitRequest{Prompt: "干点活"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if err := h.engine.WaitRun(ctx, handle.RunID); err != nil {
		t.Fatalf("WaitRun: %v", err)
	}
	if _, ok := memoryRecalledEvent(t, h, handle.RunID); ok {
		t.Fatal("未装配记忆时不应发出 memory.recalled")
	}
}
