package engine

import (
	"context"
	"testing"

	"github.com/ximo888ok-netizen/ximo-agent/internal/agent"
	ctxmgr "github.com/ximo888ok-netizen/ximo-agent/internal/context"
	"github.com/ximo888ok-netizen/ximo-agent/internal/ports"
)

// fakeMemory 是 MemoryPort 的测试替身：记录被问了什么、被投递了什么。
type fakeMemory struct {
	block       string
	recallCalls int
	recallQuery string
	turns       []MemoryTurn
}

func (f *fakeMemory) Recall(_ context.Context, query string) string {
	f.recallCalls++
	f.recallQuery = query
	return f.block
}

func (f *fakeMemory) Remember(turn MemoryTurn) { f.turns = append(f.turns, turn) }

func roles(msgs []ports.Message) []ports.MessageRole {
	out := make([]ports.MessageRole, len(msgs))
	for i, m := range msgs {
		out[i] = m.Role
	}
	return out
}

func TestInjectMemoryMessageSitsBetweenSystemPromptAndUser(t *testing.T) {
	conv := agent.NewConversation("系统提示词", "用户提问", nil)
	injectMemoryMessage(conv, "--- 长期记忆 (mem0) ---\n- 用户偏好暗色主题")

	want := []ports.MessageRole{ports.RoleSystem, ports.RoleSystem, ports.RoleUser}
	got := roles(conv.Messages)
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("消息顺序应为 [system, memory, user]，实际 %v", got)
	}
	if conv.Messages[0].Content != "系统提示词" {
		t.Fatalf("系统提示词被改动了: %q", conv.Messages[0].Content)
	}
	if conv.Messages[2].Content != "用户提问" {
		t.Fatalf("用户提问位置被挤动了: %q", conv.Messages[2].Content)
	}
}

func TestInjectMemoryMessageHandlesMissingSystemPrompt(t *testing.T) {
	conv := agent.NewConversation("", "用户提问", nil)
	injectMemoryMessage(conv, "记忆块")

	if got := roles(conv.Messages); len(got) != 2 || got[0] != ports.RoleSystem || got[1] != ports.RoleUser {
		t.Fatalf("没有系统提示词时记忆应插在最前面，实际 %v", got)
	}
}

func TestInjectMemoryMessageIsNoopWithoutBlock(t *testing.T) {
	conv := agent.NewConversation("sys", "prompt", nil)
	injectMemoryMessage(conv, "")
	injectMemoryMessage(conv, "   ")
	if len(conv.Messages) != 2 {
		t.Fatalf("空记忆块不应产生任何消息，实际 %d 条", len(conv.Messages))
	}
	// nil 会话不能 panic：引擎路径上 conv 理论上不会是 nil，但这个函数是纯函数，
	// 不做无谓的假设。
	injectMemoryMessage(nil, "记忆块")
}

// TestInjectMemoryMessageKeepsPrefixShapeStable 是这次改动最需要守住的性质：
// 注入记忆不得改变稳定前缀（系统提示词 + 工具列表）的字节，否则服务商的
// prompt cache 会整段失效，每一次带记忆的对话都要重付一次 25KB+ 的全价输入。
func TestInjectMemoryMessageKeepsPrefixShapeStable(t *testing.T) {
	systemPrompt := "很长的系统提示词…"
	tools := []ctxmgr.ToolSchema{
		{Name: "file_read", Description: "读文件"},
		{Name: "memory", Description: "长期记忆"},
	}
	before := ctxmgr.CaptureShape(systemPrompt, tools, 0, "v1")

	conv := agent.NewConversation(systemPrompt, "用户提问", nil)
	injectMemoryMessage(conv, "--- 长期记忆 (mem0) ---\n- 记住的事")

	after := ctxmgr.CaptureShape(conv.Messages[0].Content, tools, 0, "v1")
	if before.SystemHash != after.SystemHash {
		t.Fatalf("系统提示词哈希变了: %q → %q", before.SystemHash, after.SystemHash)
	}
	if before.ToolsHash != after.ToolsHash || before.PrefixHash != after.PrefixHash {
		t.Fatalf("前缀形状变了: %+v → %+v", before, after)
	}
}

func TestRecallMemoryPortContract(t *testing.T) {
	fake := &fakeMemory{block: "记忆块"}

	// 未装配记忆端口：不报错、不注入。
	var bare Engine
	if got := bare.recallMemory(context.Background(), "问一句"); got != "" {
		t.Fatalf("未装配时应返回空串，实际 %q", got)
	}

	eng := &Engine{deps: Dependencies{Memory: fake}}
	if got := eng.recallMemory(context.Background(), "问一句"); got != "记忆块" {
		t.Fatalf("应返回端口给的内容，实际 %q", got)
	}
	if fake.recallQuery != "问一句" {
		t.Fatalf("提问应原样传给端口，实际 %q", fake.recallQuery)
	}

	// 空提问不发请求：没有查询词的召回只会白白多一次网络往返。
	before := fake.recallCalls
	if got := eng.recallMemory(context.Background(), "  "); got != "" {
		t.Fatalf("空提问应返回空串，实际 %q", got)
	}
	if fake.recallCalls != before {
		t.Fatal("空提问不应触发召回")
	}
}

func TestRememberTurnFiltersContentlessTurns(t *testing.T) {
	fake := &fakeMemory{}
	eng := &Engine{deps: Dependencies{Memory: fake}}

	// 有答复的终态：投递。
	eng.rememberTurn(MemoryTurn{RunID: "r1", SessionID: "s1", Prompt: "p", Answer: "a"})
	// 没有答复（失败/中途）与没有提问的轮次：不投递，它们没有可抽取的内容。
	eng.rememberTurn(MemoryTurn{RunID: "r2", Prompt: "p"})
	eng.rememberTurn(MemoryTurn{RunID: "r3", Answer: "a"})
	eng.rememberTurn(MemoryTurn{RunID: "", Prompt: "p", Answer: "a"})

	if len(fake.turns) != 1 {
		t.Fatalf("只应投递一轮完整问答，实际 %d 轮: %+v", len(fake.turns), fake.turns)
	}
	if fake.turns[0].RunID != "r1" || fake.turns[0].Answer != "a" {
		t.Fatalf("投递内容不符: %+v", fake.turns[0])
	}

	// 未装配记忆端口时不得 panic。
	var bare Engine
	bare.rememberTurn(MemoryTurn{RunID: "r", Prompt: "p", Answer: "a"})
}

// 保证端口契约不随时间漂移：MemoryPort 的两个签名是刻意设计成「不报错、不
// 阻塞」的，改动它们会破坏引擎两个调用点的假设。
var _ MemoryPort = (*fakeMemory)(nil)
