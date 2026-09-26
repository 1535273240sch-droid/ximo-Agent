package anthropic

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ximo888ok-netizen/ximo-agent/internal/provider"
)

// sseEvent 解析后的一条 SSE 事件。
type sseEvent struct {
	name string
	data map[string]any
}

// parseSSE 把响应体解析为事件序列，并校验 Anthropic 的 `event:` 行与 data 的 type 一致。
func parseSSE(t *testing.T, body string) []sseEvent {
	t.Helper()
	if !strings.HasSuffix(body, "\n\n") {
		t.Fatalf("SSE 体应以空行结尾: %q", body)
	}
	events := make([]sseEvent, 0, 8)
	for _, block := range strings.Split(strings.TrimRight(body, "\n"), "\n\n") {
		var (
			name string
			data string
		)
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
			default:
				t.Fatalf("非法 SSE 行: %q", line)
			}
		}
		if name == "" || data == "" {
			t.Fatalf("SSE 块缺少 event/data: %q", block)
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			t.Fatalf("data 不是合法 JSON: %v (%q)", err, data)
		}
		if payload["type"] != name {
			t.Fatalf("event 名与 data.type 不一致: %q vs %v", name, payload["type"])
		}
		events = append(events, sseEvent{name: name, data: payload})
	}
	return events
}

func eventNames(events []sseEvent) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e.name)
	}
	return out
}

func newTestStream(t *testing.T, model string, stopSeqs []string) (*messageStream, *httptest.ResponseRecorder) {
	t.Helper()
	rec := httptest.NewRecorder()
	return newMessageStream(newSSEWriter(rec), "msg_test", model, stopSeqs), rec
}

func TestMessageStreamTextOnly(t *testing.T) {
	ms, rec := newTestStream(t, "claude-3-5-sonnet", nil)
	// header 之前不应有任何字节：首字节前失败才能回普通 JSON 错误。
	if rec.Body.Len() != 0 {
		t.Fatalf("start 之前不应写响应体")
	}
	ms.start()
	ms.text("Hel")
	ms.text("lo")
	ms.finish(stopReasonEndTurn, 12, 5)

	events := parseSSE(t, rec.Body.String())
	want := []string{
		"message_start", "content_block_start", "content_block_delta", "content_block_delta",
		"content_block_stop", "message_delta", "message_stop",
	}
	if got := strings.Join(eventNames(events), ","); got != strings.Join(want, ",") {
		t.Fatalf("事件序列不符\n期望 %v\n实际 %v", want, eventNames(events))
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type 期望 text/event-stream，实际 %q", ct)
	}

	start := events[0].data["message"].(map[string]any)
	if start["model"] != "claude-3-5-sonnet" || start["role"] != "assistant" || start["type"] != "message" {
		t.Fatalf("message_start 的 message 不符: %v", start)
	}
	if start["stop_reason"] != nil || start["stop_sequence"] != nil {
		t.Fatalf("message_start 的 stop_reason/stop_sequence 应为 null: %v", start)
	}
	if content, ok := start["content"].([]any); !ok || len(content) != 0 {
		t.Fatalf("message_start 的 content 应为空数组: %v", start["content"])
	}
	usage := start["usage"].(map[string]any)
	if usage["input_tokens"].(float64) != 0 {
		t.Fatalf("message_start 的 input_tokens 应为 0（上游 usage 尚未到达）")
	}

	cb := events[1].data["content_block"].(map[string]any)
	if cb["type"] != "text" || cb["text"] != "" {
		t.Fatalf("content_block_start 应为空 text 块: %v", cb)
	}
	if events[1].data["index"].(float64) != 0 || events[2].data["index"].(float64) != 0 {
		t.Fatalf("首个内容块 index 应为 0")
	}
	texts := []string{}
	for _, e := range events {
		if e.name != "content_block_delta" {
			continue
		}
		d := e.data["delta"].(map[string]any)
		if d["type"] != "text_delta" {
			t.Fatalf("delta 类型不符: %v", d)
		}
		texts = append(texts, d["text"].(string))
	}
	if strings.Join(texts, "") != "Hello" {
		t.Fatalf("文本增量不符: %v", texts)
	}

	md := events[5].data
	delta := md["delta"].(map[string]any)
	if delta["stop_reason"] != stopReasonEndTurn || delta["stop_sequence"] != nil {
		t.Fatalf("message_delta 不符: %v", delta)
	}
	mdUsage := md["usage"].(map[string]any)
	if mdUsage["input_tokens"].(float64) != 12 || mdUsage["output_tokens"].(float64) != 5 {
		t.Fatalf("message_delta usage 不符: %v", mdUsage)
	}
}

func TestMessageStreamToolUse(t *testing.T) {
	ms, rec := newTestStream(t, "m", nil)
	ms.start()
	ms.toolCalls([]provider.ToolCallDelta{{Index: 0, ID: "call_1", Name: "get_weather"}})
	ms.toolCalls([]provider.ToolCallDelta{{Index: 0, Arguments: `{"city":`}})
	ms.toolCalls([]provider.ToolCallDelta{{Index: 0, Arguments: `"北京"}`}})
	ms.finish(stopReasonToolUse, 3, 9)

	events := parseSSE(t, rec.Body.String())
	want := []string{
		"message_start", "content_block_start", "content_block_delta", "content_block_delta",
		"content_block_stop", "message_delta", "message_stop",
	}
	if got := strings.Join(eventNames(events), ","); got != strings.Join(want, ",") {
		t.Fatalf("事件序列不符\n期望 %v\n实际 %v", want, eventNames(events))
	}
	start := events[1].data["content_block"].(map[string]any)
	if start["type"] != "tool_use" || start["id"] != "call_1" || start["name"] != "get_weather" {
		t.Fatalf("tool_use 块头不符: %v", start)
	}
	if _, ok := start["input"].(map[string]any); !ok {
		t.Fatalf("tool_use 块的 input 应为空对象: %v", start["input"])
	}
	var partial strings.Builder
	for _, e := range events {
		if e.name != "content_block_delta" {
			continue
		}
		d := e.data["delta"].(map[string]any)
		if d["type"] != "input_json_delta" {
			t.Fatalf("工具增量类型不符: %v", d)
		}
		partial.WriteString(d["partial_json"].(string))
	}
	if partial.String() != `{"city":"北京"}` {
		t.Fatalf("partial_json 拼接不符: %q", partial.String())
	}
	if events[5].data["delta"].(map[string]any)["stop_reason"] != stopReasonToolUse {
		t.Fatalf("stop_reason 应为 tool_use")
	}
}

func TestMessageStreamTextThenToolGetsSeparateBlocks(t *testing.T) {
	ms, rec := newTestStream(t, "m", nil)
	ms.start()
	ms.text("先想一想")
	ms.toolCalls([]provider.ToolCallDelta{{Index: 0, ID: "c1", Name: "t", Arguments: "{}"}})
	ms.finish(stopReasonToolUse, 1, 2)

	events := parseSSE(t, rec.Body.String())
	starts := []int{}
	types := []string{}
	for _, e := range events {
		if e.name != "content_block_start" {
			continue
		}
		starts = append(starts, int(e.data["index"].(float64)))
		types = append(types, e.data["content_block"].(map[string]any)["type"].(string))
	}
	if len(starts) != 2 || starts[0] != 0 || starts[1] != 1 {
		t.Fatalf("应有两个递增 index 的内容块，实际 %v", starts)
	}
	if types[0] != "text" || types[1] != "tool_use" {
		t.Fatalf("块类型不符: %v", types)
	}
	// 每个块开始前，前一个块必须已关闭。
	stops := 0
	for i, e := range events {
		if e.name == "content_block_stop" && i < len(events)-1 {
			stops++
		}
	}
	if stops != 2 {
		t.Fatalf("应关闭 2 个块，实际 %d", stops)
	}
}

func TestMessageStreamEmptyResponse(t *testing.T) {
	ms, rec := newTestStream(t, "m", nil)
	ms.start()
	ms.finish(stopReasonMaxTokens, 4, 0)

	events := parseSSE(t, rec.Body.String())
	want := []string{"message_start", "message_delta", "message_stop"}
	if got := strings.Join(eventNames(events), ","); got != strings.Join(want, ",") {
		t.Fatalf("空响应不应产生内容块事件，实际 %v", eventNames(events))
	}
}

func TestMessageStreamStopSequenceTruncates(t *testing.T) {
	ms, rec := newTestStream(t, "m", []string{"END"})
	ms.start()
	if stopped := ms.text("headEN"); stopped {
		t.Fatalf("此刻尚未命中")
	}
	stopped := ms.text("Dtail")
	if !stopped {
		t.Fatalf("应命中停止序列")
	}
	ms.finish(stopReasonStopSequence, 2, 3)

	events := parseSSE(t, rec.Body.String())
	var text strings.Builder
	for _, e := range events {
		if e.name != "content_block_delta" {
			continue
		}
		text.WriteString(e.data["delta"].(map[string]any)["text"].(string))
	}
	if text.String() != "head" {
		t.Fatalf("命中的序列及其之后的内容不得下发，实际 %q", text.String())
	}
	delta := events[len(events)-2].data["delta"].(map[string]any)
	if delta["stop_reason"] != stopReasonStopSequence || delta["stop_sequence"] != "END" {
		t.Fatalf("message_delta 不符: %v", delta)
	}
}

func TestMessageStreamFailure(t *testing.T) {
	ms, rec := newTestStream(t, "m", nil)
	ms.start()
	ms.text("partial")
	ms.fail("upstream stream failed")

	events := parseSSE(t, rec.Body.String())
	last := events[len(events)-1]
	if last.name != "error" {
		t.Fatalf("最后一个事件应为 error，实际 %v", eventNames(events))
	}
	body := last.data["error"].(map[string]any)
	if body["type"] != "api_error" || body["message"] != "upstream stream failed" {
		t.Fatalf("error 事件体不符: %v", body)
	}
	if got := strings.Join(eventNames(events), ","); got != "message_start,content_block_start,content_block_delta,content_block_stop,error" {
		t.Fatalf("失败事件序列不符: %v", eventNames(events))
	}
}

// stopReasonOf 从解析后的事件里取 message_delta 的 stop_reason。
func stopReasonOf(t *testing.T, events []sseEvent) string {
	t.Helper()
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].name != "message_delta" {
			continue
		}
		delta, ok := events[i].data["delta"].(map[string]any)
		if !ok {
			t.Fatalf("message_delta 缺少 delta 对象：%v", events[i].data)
		}
		reason, _ := delta["stop_reason"].(string)
		return reason
	}
	t.Fatalf("事件序列里没有 message_delta：%v", eventNames(events))
	return ""
}

// toolUseBlockCount 数一数事件流里打开了几个 tool_use 内容块（推断路径的判据）。
func toolUseBlockCount(events []sseEvent) int {
	n := 0
	for _, e := range events {
		if e.name != "content_block_start" {
			continue
		}
		if blk, ok := e.data["content_block"].(map[string]any); ok && blk["type"] == blockToolUse {
			n++
		}
	}
	return n
}

// streamStopReasonCase 是一次「上游 finish_reason → 出站 stop_reason」的用例。
type streamStopReasonCase struct {
	name   string
	script upstreamScript
	want   string
}

func runStreamStopReasonCases(t *testing.T, cases []streamStopReasonCase, maxTok int) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, tc.script, 1_000_000)
			rec := e.do(t, fmt.Sprintf(
				`{"model":%q,"max_tokens":%d,"stream":true,"messages":[{"role":"user","content":"hi"}]}`,
				testModel, maxTok), "ximo_sk_test")
			if rec.Code != http.StatusOK {
				t.Fatalf("期望 200，实际 %d：%s", rec.Code, rec.Body.String())
			}
			events := parseSSE(t, rec.Body.String())
			if last := events[len(events)-1].name; last != "message_stop" {
				t.Fatalf("最后一个事件应为 message_stop，实际 %q（%v）", last, eventNames(events))
			}
			if len(tc.script.toolCalls) > 0 && toolUseBlockCount(events) == 0 {
				t.Fatalf("用例前提不成立：上游发了工具调用增量，但事件流里没有 tool_use 块：%v", eventNames(events))
			}
			if got := stopReasonOf(t, events); got != tc.want {
				t.Fatalf("stop_reason = %q，期望 %q", got, tc.want)
			}
		})
	}
}

// TestStreamStopReasonPrefersUpstreamFinishReason 断言上游显式报告的 finish_reason
// 决定出站 stop_reason，且不被推断覆盖。
func TestStreamStopReasonPrefersUpstreamFinishReason(t *testing.T) {
	const maxTok = 64
	usage3 := `{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}`
	// 输出 token 恰好等于 max_tokens：推断会说 max_tokens。
	usageFull := fmt.Sprintf(`{"prompt_tokens":5,"completion_tokens":%d,"total_tokens":%d}`, maxTok, maxTok+5)

	runStreamStopReasonCases(t, []streamStopReasonCase{
		{
			// 有工具调用增量 + 上游报 length：推断会得 tool_use，必须以上游为准得 max_tokens。
			name: "上游报 length",
			script: upstreamScript{
				toolCalls: []string{`[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{}"}}]`},
				usage:     usage3,
				finish:    "length",
			},
			want: stopReasonMaxTokens,
		},
		{
			// 反向验证：推断会说 max_tokens，但上游明确报 stop。
			name:   "上游报 stop 且输出恰好等于 max_tokens",
			script: upstreamScript{deltas: []string{"cut"}, usage: usageFull, finish: "stop"},
			want:   stopReasonEndTurn,
		},
		{
			// 没有任何 tool_use 块，但上游明确报 tool_calls：不得被推断成 end_turn。
			name:   "上游报 tool_calls 且无工具增量",
			script: upstreamScript{deltas: []string{"done"}, usage: usage3, finish: "tool_calls"},
			want:   stopReasonToolUse,
		},
	}, maxTok)
}

// TestStreamStopReasonFallsBackToInference 是回归用例：上游不发 finish_reason
// （只发 delta + [DONE]）时仍走原有推断。
func TestStreamStopReasonFallsBackToInference(t *testing.T) {
	const maxTok = 64
	usage3 := `{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}`
	usageFull := fmt.Sprintf(`{"prompt_tokens":5,"completion_tokens":%d,"total_tokens":%d}`, maxTok, maxTok+5)

	runStreamStopReasonCases(t, []streamStopReasonCase{
		{
			name:   "输出达到 max_tokens",
			script: upstreamScript{deltas: []string{"cut"}, usage: usageFull, omitFinish: true},
			want:   stopReasonMaxTokens,
		},
		{
			name: "有工具调用",
			script: upstreamScript{
				toolCalls:  []string{`[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{}"}}]`},
				usage:      usage3,
				omitFinish: true,
			},
			want: stopReasonToolUse,
		},
		{
			name:   "普通结束",
			script: upstreamScript{deltas: []string{"hi"}, usage: usage3, omitFinish: true},
			want:   stopReasonEndTurn,
		},
	}, maxTok)
}

// TestInferredStopReason 覆盖推断口径本身（上游不报 finish_reason 时的回退路径）。
func TestInferredStopReason(t *testing.T) {
	cases := []struct {
		out, max  int64
		hasTools  bool
		matched   string
		wantValue string
	}{
		{out: 3, max: 16, wantValue: stopReasonEndTurn},
		{out: 16, max: 16, wantValue: stopReasonMaxTokens},
		{out: 3, max: 16, hasTools: true, wantValue: stopReasonToolUse},
		{out: 16, max: 16, hasTools: true, wantValue: stopReasonMaxTokens},
		{out: 3, max: 16, matched: "END", wantValue: stopReasonStopSequence},
		{out: 0, max: 0, wantValue: stopReasonEndTurn},
	}
	for _, tc := range cases {
		if got := inferredStopReason(tc.out, tc.max, tc.hasTools, tc.matched); got != tc.wantValue {
			t.Fatalf("out=%d max=%d tools=%v matched=%q 期望 %q，实际 %q", tc.out, tc.max, tc.hasTools, tc.matched, tc.wantValue, got)
		}
	}
}
