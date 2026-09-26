package anthropic

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ximo888ok-netizen/ximo-agent/internal/provider"
)

// encoded 把消息列表序列化为 JSON 文本，避免对 nil 与空切片做脆弱的深度比较。
func encoded(t *testing.T, v any) string {
	t.Helper()
	buf, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	return string(buf)
}

func TestTranslateRequest(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantMsgs   string // 期望的上游 messages JSON（nil 表示只校验其它字段）
		wantTools  int
		wantMax    int
		wantTemp   float64
		wantStop   []string
		wantWarn   int
		wantCode   string // 非空表示期望的错误码
		wantModel  string
		wantStream bool
	}{
		{
			name: "system 字符串 + user 文本",
			body: `{
				"model":"claude-3-5-sonnet","max_tokens":128,
				"system":"you are helpful",
				"messages":[{"role":"user","content":"hi"}]
			}`,
			wantMsgs:  `[{"role":"system","content":"you are helpful"},{"role":"user","content":"hi"}]`,
			wantMax:   128,
			wantTemp:  1,
			wantModel: "claude-3-5-sonnet",
		},
		{
			name: "system 为 text block 数组（多块以换行连接）",
			body: `{
				"model":"m","max_tokens":16,
				"system":[{"type":"text","text":"a"},{"type":"text","text":"b"}],
				"messages":[{"role":"user","content":[{"type":"text","text":"q"}]}]
			}`,
			wantMsgs: `[{"role":"system","content":"a\nb"},{"role":"user","content":"q"}]`,
			wantMax:  16,
			wantTemp: 1,
		},
		{
			name:     "temperature 显式给出时透传",
			body:     `{"model":"m","max_tokens":8,"temperature":0.2,"messages":[{"role":"user","content":"x"}]}`,
			wantMax:  8,
			wantTemp: 0.2,
		},
		{
			name:     "缺 max_tokens（Anthropic 必填）",
			body:     `{"model":"m","messages":[{"role":"user","content":"x"}]}`,
			wantCode: "max_tokens",
		},
		{
			name:     "缺 model",
			body:     `{"max_tokens":8,"messages":[{"role":"user","content":"x"}]}`,
			wantCode: "model",
		},
		{
			name:     "max_tokens 为 0",
			body:     `{"model":"m","max_tokens":0,"messages":[{"role":"user","content":"x"}]}`,
			wantCode: "max_tokens",
		},
		{
			name: "图片块：V1 显式拒绝（provider 无多模态入口）",
			body: `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":[
				{"type":"text","text":"看图"},
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]}]}`,
			wantCode: "unsupported_content_block",
		},
		{
			name: "system 数组里的图片块同样拒绝",
			body: `{"model":"m","max_tokens":8,
				"system":[{"type":"image","source":{"type":"url","url":"http://x/y.png"}}],
				"messages":[{"role":"user","content":"x"}]}`,
			wantCode: "unsupported_content_block",
		},
		{
			name:     "非法 role",
			body:     `{"model":"m","max_tokens":8,"messages":[{"role":"system","content":"x"}]}`,
			wantCode: "messages",
		},
		{
			name:     "全空消息",
			body:     `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":""}]}`,
			wantCode: "messages",
		},
		{
			name: "tools → OpenAI function 形状",
			body: `{"model":"m","max_tokens":32,"messages":[{"role":"user","content":"x"}],
				"tools":[{"name":"get_weather","description":"查天气",
					"input_schema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}],
				"tool_choice":{"type":"auto"}}`,
			wantTools: 1,
			wantMax:   32,
			wantTemp:  1,
		},
		{
			name: "tool_choice=any 降级为 auto 并留告警",
			body: `{"model":"m","max_tokens":32,"messages":[{"role":"user","content":"x"}],
				"tools":[{"name":"t","input_schema":{"type":"object"}}],
				"tool_choice":{"type":"any"}}`,
			wantTools: 1,
			wantWarn:  1,
			wantMax:   32,
			wantTemp:  1,
		},
		{
			name:     "top_p 无法转发 → 告警",
			body:     `{"model":"m","max_tokens":8,"top_p":0.5,"messages":[{"role":"user","content":"x"}]}`,
			wantWarn: 1,
			wantMax:  8,
			wantTemp: 1,
		},
		{
			name: "assistant tool_use + user tool_result 往返",
			body: `{"model":"m","max_tokens":64,"messages":[
				{"role":"user","content":"北京天气"},
				{"role":"assistant","content":[
					{"type":"text","text":"我查一下"},
					{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"北京"}}]},
				{"role":"user","content":[
					{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"晴 25℃"}]}]}
			]}`,
			wantMsgs: `[
				{"role":"user","content":"北京天气"},
				{"role":"assistant","content":"我查一下","tool_calls":[{"id":"toolu_1","name":"get_weather","arguments":"{\"city\":\"北京\"}"}]},
				{"role":"tool","content":"晴 25℃","tool_call_id":"toolu_1"}
			]`,
			wantMax:  64,
			wantTemp: 1,
		},
		{
			name: "tool_result 缺少 tool_use_id → 400",
			body: `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":[
				{"type":"tool_result","content":"x"}]}]}`,
			wantCode: "tool_result",
		},
		{
			name: "stop_sequences 原样保留（输出侧截断，见 stopseq.go）",
			body: `{"model":"m","max_tokens":8,"stop_sequences":["\n\nHuman:","END"],
				"messages":[{"role":"user","content":"x"}]}`,
			wantStop: []string{"\n\nHuman:", "END"},
			wantMax:  8,
			wantTemp: 1,
		},
		{
			name:       "stream 标志透传",
			body:       `{"model":"m","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"x"}]}`,
			wantMax:    8,
			wantTemp:   1,
			wantStream: true,
		},
		{
			name: "user 消息里的 text 与 tool_result 保持相对顺序",
			body: `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":[
				{"type":"text","text":"结果如下"},
				{"type":"tool_result","tool_use_id":"t1","content":"42"},
				{"type":"text","text":"继续"}]}]}`,
			wantMsgs: `[
				{"role":"user","content":"结果如下"},
				{"role":"tool","content":"42","tool_call_id":"t1"},
				{"role":"user","content":"继续"}
			]`,
			wantMax:  8,
			wantTemp: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var req messagesRequest
			if err := json.Unmarshal([]byte(tc.body), &req); err != nil {
				t.Fatalf("测试用例本身非法: %v", err)
			}
			got, err := translateRequest(&req)
			if tc.wantCode != "" {
				if err == nil {
					t.Fatalf("期望错误码 %q，实际成功: %+v", tc.wantCode, got)
				}
				var re *requestError
				if !errors.As(err, &re) {
					t.Fatalf("期望 *requestError，实际 %T: %v", err, err)
				}
				if !strings.Contains(re.code, tc.wantCode) {
					t.Fatalf("期望错误码包含 %q，实际 %q", tc.wantCode, re.code)
				}
				if re.status != 400 {
					t.Fatalf("期望 400，实际 %d", re.status)
				}
				return
			}
			if err != nil {
				t.Fatalf("翻译失败: %v", err)
			}
			if tc.wantMsgs != "" {
				if gotJSON := encoded(t, got.messages); gotJSON != compact(t, tc.wantMsgs) {
					t.Fatalf("messages 不符\n期望 %s\n实际 %s", compact(t, tc.wantMsgs), gotJSON)
				}
			}
			if len(got.tools) != tc.wantTools {
				t.Fatalf("tools 数量期望 %d，实际 %d", tc.wantTools, len(got.tools))
			}
			if got.maxTokens != tc.wantMax {
				t.Fatalf("max_tokens 期望 %d，实际 %d", tc.wantMax, got.maxTokens)
			}
			if got.temperature != tc.wantTemp {
				t.Fatalf("temperature 期望 %v，实际 %v", tc.wantTemp, got.temperature)
			}
			if len(got.warnings) != tc.wantWarn {
				t.Fatalf("告警数期望 %d，实际 %d（%v）", tc.wantWarn, len(got.warnings), got.warnings)
			}
			if tc.wantStop != nil {
				if encoded(t, got.stopSequences) != encoded(t, tc.wantStop) {
					t.Fatalf("stop_sequences 期望 %v，实际 %v", tc.wantStop, got.stopSequences)
				}
			}
			if tc.wantModel != "" && got.model != tc.wantModel {
				t.Fatalf("model 期望 %q，实际 %q", tc.wantModel, got.model)
			}
			if got.stream != tc.wantStream {
				t.Fatalf("stream 期望 %v，实际 %v", tc.wantStream, got.stream)
			}
		})
	}
}

func TestTranslateToolsShape(t *testing.T) {
	var req messagesRequest
	body := `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"x"}],
		"tools":[{"name":"t1","description":"d1","input_schema":{"type":"object","properties":{"a":{"type":"string"}}}}]}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("用例非法: %v", err)
	}
	got, err := translateRequest(&req)
	if err != nil {
		t.Fatalf("翻译失败: %v", err)
	}
	if len(got.tools) != 1 {
		t.Fatalf("期望 1 个工具，实际 %d", len(got.tools))
	}
	tool := got.tools[0]
	if tool.Name != "t1" || tool.Description != "d1" {
		t.Fatalf("工具名/描述不符: %+v", tool)
	}
	if _, ok := tool.Parameters["properties"]; !ok {
		t.Fatalf("input_schema 未透传为 parameters: %+v", tool.Parameters)
	}
}

func TestTranslateToolsEmptySchemaGetsObject(t *testing.T) {
	var req messagesRequest
	if err := json.Unmarshal([]byte(`{"model":"m","max_tokens":4,"messages":[{"role":"user","content":"x"}],
		"tools":[{"name":"t"}]}`), &req); err != nil {
		t.Fatalf("用例非法: %v", err)
	}
	got, err := translateRequest(&req)
	if err != nil {
		t.Fatalf("翻译失败: %v", err)
	}
	if got.tools[0].Parameters["type"] != "object" {
		t.Fatalf("空 schema 应补成对象: %+v", got.tools[0].Parameters)
	}
}

func TestTranslateResponseMappings(t *testing.T) {
	cases := []struct {
		name       string
		resp       provider.CompletionResponse
		stopSeqs   []string
		wantReason string
		wantBlocks string
		wantIn     int64
		wantOut    int64
		wantSeq    string
	}{
		{
			name:       "stop → end_turn",
			resp:       provider.CompletionResponse{FinishReason: provider.FinishStop, Content: "hi", Usage: &provider.TokenUsage{PromptTokens: 7, CompletionTokens: 2}},
			wantReason: stopReasonEndTurn,
			wantBlocks: `[{"type":"text","text":"hi"}]`,
			wantIn:     7,
			wantOut:    2,
		},
		{
			name:       "length → max_tokens",
			resp:       provider.CompletionResponse{FinishReason: provider.FinishLength, Content: "cut"},
			wantReason: stopReasonMaxTokens,
			wantBlocks: `[{"type":"text","text":"cut"}]`,
		},
		{
			name: "tool_calls → tool_use",
			resp: provider.CompletionResponse{
				FinishReason: provider.FinishToolCalls,
				Content:      "thinking",
				ToolCalls:    []provider.ToolCall{{ID: "call_1", Name: "get_weather", Arguments: `{"city":"北京"}`}},
			},
			wantReason: stopReasonToolUse,
			wantBlocks: `[{"type":"text","text":"thinking"},{"type":"tool_use","id":"call_1","name":"get_weather","input":{"city":"北京"}}]`,
		},
		{
			name: "非法 arguments 退化为空对象",
			resp: provider.CompletionResponse{
				FinishReason: provider.FinishToolCalls,
				ToolCalls:    []provider.ToolCall{{ID: "c", Name: "t", Arguments: `{"trunc`}},
			},
			wantReason: stopReasonToolUse,
			wantBlocks: `[{"type":"tool_use","id":"c","name":"t","input":{}}]`,
		},
		{
			name:       "空内容 → content 为空数组",
			resp:       provider.CompletionResponse{FinishReason: provider.FinishLength},
			wantReason: stopReasonMaxTokens,
			wantBlocks: `[]`,
		},
		{
			name:       "命中 stop 序列：截断并报 stop_sequence",
			resp:       provider.CompletionResponse{FinishReason: provider.FinishStop, Content: "前半END后半"},
			stopSeqs:   []string{"END"},
			wantReason: stopReasonStopSequence,
			wantBlocks: `[{"type":"text","text":"前半"}]`,
			wantSeq:    "END",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := translateResponse(tc.resp, "claude-3-5-sonnet", tc.stopSeqs)
			if out.Model != "claude-3-5-sonnet" || out.Type != "message" || out.Role != "assistant" {
				t.Fatalf("响应外壳不符: %+v", out)
			}
			if out.StopReason == nil || *out.StopReason != tc.wantReason {
				t.Fatalf("stop_reason 期望 %q，实际 %v", tc.wantReason, out.StopReason)
			}
			if got := encoded(t, out.Content); got != compact(t, tc.wantBlocks) {
				t.Fatalf("content 不符\n期望 %s\n实际 %s", compact(t, tc.wantBlocks), got)
			}
			if out.Usage.InputTokens != tc.wantIn || out.Usage.OutputTokens != tc.wantOut {
				t.Fatalf("usage 期望 %d/%d，实际 %d/%d", tc.wantIn, tc.wantOut, out.Usage.InputTokens, out.Usage.OutputTokens)
			}
			switch {
			case tc.wantSeq == "" && out.StopSequence != nil:
				t.Fatalf("stop_sequence 应为 null，实际 %v", *out.StopSequence)
			case tc.wantSeq != "" && (out.StopSequence == nil || *out.StopSequence != tc.wantSeq):
				t.Fatalf("stop_sequence 期望 %q，实际 %v", tc.wantSeq, out.StopSequence)
			}
		})
	}
}

func TestStopReasonForPriority(t *testing.T) {
	// 命中客户端停止序列优先于其它原因。
	if got := stopReasonFor(provider.FinishLength, true, "END"); got != stopReasonStopSequence {
		t.Fatalf("期望 stop_sequence，实际 %q", got)
	}
	// 截断优先于工具调用（工具调用可能是不完整的）。
	if got := stopReasonFor(provider.FinishLength, true, ""); got != stopReasonMaxTokens {
		t.Fatalf("期望 max_tokens，实际 %q", got)
	}
}

func TestEstimatedInputTokensIsRoughUpperBound(t *testing.T) {
	tr := &translated{}
	tr.messages = []provider.Message{{Role: provider.RoleUser, Content: strings.Repeat("a", 400)}}
	if got := tr.estimatedInputTokens(); got != 100 {
		t.Fatalf("400 字符应粗估为 100 token，实际 %d", got)
	}
	empty := &translated{}
	if got := empty.estimatedInputTokens(); got != 0 {
		t.Fatalf("空请求应估 0，实际 %d", got)
	}
}

func compact(t *testing.T, s string) string {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(s)); err != nil {
		t.Fatalf("用例 JSON 非法: %v", err)
	}
	return buf.String()
}
