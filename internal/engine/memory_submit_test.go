package engine

import (
	"context"
	"testing"

	"github.com/ximo888ok-netizen/ximo-agent/internal/ports"
	"github.com/ximo888ok-netizen/ximo-agent/internal/ports/mem"
	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// recordingProvider 包住测试用的 provider，把每一轮真正收到的请求留下来。
//
// 这是断言「记忆真的进了发给模型的请求」的唯一办法：只看「Recall 被调用过」
// 无法证明注入生效，而注入失效正是这个特性最容易悄悄坏掉的地方。
type recordingProvider struct {
	inner ports.Provider
	reqs  []ports.ProviderRequest
}

func (p *recordingProvider) Complete(ctx context.Context, req ports.ProviderRequest) (ports.ProviderResponse, error) {
	p.reqs = append(p.reqs, req)
	return p.inner.Complete(ctx, req)
}

func (p *recordingProvider) first() ports.ProviderRequest {
	if len(p.reqs) == 0 {
		return ports.ProviderRequest{}
	}
	return p.reqs[0]
}

// TestSubmitInjectsRecalledMemoryIntoTheFirstRound 走完整条链路：
// Submit → 召回 → 注入 → 第一轮模型请求里出现记忆消息。
func TestSubmitInjectsRecalledMemoryIntoTheFirstRound(t *testing.T) {
	h := newHarness(t, mem.FinalRound("已改好"))

	rec := &recordingProvider{inner: h.provider}
	const block = "--- 长期记忆 (mem0) ---\n- 用户偏好暗色主题"
	fake := &fakeMemory{block: block}
	// deps 在 Submit 之前写入，此后只有 run goroutine 读它们，不存在竞争。
	h.engine.deps.Provider = rec
	h.engine.deps.Memory = fake

	ctx := context.Background()
	handle, err := h.engine.Submit(ctx, types.SubmitRequest{
		Prompt:       "帮我改一下界面",
		SystemPrompt: "系统提示词",
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if err := h.engine.WaitRun(ctx, handle.RunID); err != nil {
		t.Fatalf("WaitRun: %v", err)
	}

	if fake.recallQuery != "帮我改一下界面" {
		t.Fatalf("召回应以本轮提示为查询词，实际 %q", fake.recallQuery)
	}

	first := rec.first()
	if len(first.Messages) < 3 {
		t.Fatalf("第一轮请求至少应有 [system, memory, user] 三条消息，实际 %d 条: %+v",
			len(first.Messages), first.Messages)
	}
	if first.Messages[0].Content != "系统提示词" {
		t.Fatalf("系统提示词必须仍在最前（prompt cache 前缀），实际 %q", first.Messages[0].Content)
	}
	if first.Messages[1].Role != ports.RoleSystem || first.Messages[1].Content != block {
		t.Fatalf("记忆应作为独立 system 消息紧随系统提示词，实际 %+v", first.Messages[1])
	}
	if first.Messages[2].Content != "帮我改一下界面" {
		t.Fatalf("用户消息位置被挤动，实际 %+v", first.Messages[2])
	}

	// 收尾时同一轮问答应被投递给记忆端口（写路径）。
	if len(fake.turns) != 1 {
		t.Fatalf("完成的 run 应投递一轮问答，实际 %d 轮: %+v", len(fake.turns), fake.turns)
	}
	if fake.turns[0].RunID != handle.RunID || fake.turns[0].SessionID != handle.SessionID {
		t.Fatalf("投递的 run/session 不符: %+v", fake.turns[0])
	}
	if fake.turns[0].Prompt != "帮我改一下界面" || fake.turns[0].Answer != "已改好" {
		t.Fatalf("投递内容不符: %+v", fake.turns[0])
	}
}

// TestSubmitWithoutMemoryPortIsUnchanged 是最重要的回归：没配记忆的人，
// 消息表必须与改动前逐字节一致（不多出一条空记忆消息）。
func TestSubmitWithoutMemoryPortIsUnchanged(t *testing.T) {
	h := newHarness(t, mem.FinalRound("完成"))
	rec := &recordingProvider{inner: h.provider}
	h.engine.deps.Provider = rec

	ctx := context.Background()
	handle, err := h.engine.Submit(ctx, types.SubmitRequest{
		Prompt:       "干点活",
		SystemPrompt: "系统提示词",
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if err := h.engine.WaitRun(ctx, handle.RunID); err != nil {
		t.Fatalf("WaitRun: %v", err)
	}

	first := rec.first()
	if len(first.Messages) != 2 {
		t.Fatalf("未装配记忆时消息表应保持 [system, user]，实际 %d 条: %+v",
			len(first.Messages), first.Messages)
	}
}

// TestSubmitWithEmptyRecallDoesNotAddAMessage 覆盖「记忆服务在，但没有相关记忆」：
// 这种情况同样不能留下任何多余字节。
func TestSubmitWithEmptyRecallDoesNotAddAMessage(t *testing.T) {
	h := newHarness(t, mem.FinalRound("完成"))
	rec := &recordingProvider{inner: h.provider}
	h.engine.deps.Provider = rec
	h.engine.deps.Memory = &fakeMemory{block: ""}

	ctx := context.Background()
	handle, err := h.engine.Submit(ctx, types.SubmitRequest{Prompt: "干点活", SystemPrompt: "系统提示词"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if err := h.engine.WaitRun(ctx, handle.RunID); err != nil {
		t.Fatalf("WaitRun: %v", err)
	}

	if first := rec.first(); len(first.Messages) != 2 {
		t.Fatalf("没有命中记忆时不应插入消息，实际 %d 条: %+v", len(first.Messages), first.Messages)
	}
}
