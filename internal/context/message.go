// message.go —— 消息模型、工具保留优先级与工具名回溯。
//
// 对应 v1：shared/cache/types.ts（MutableMessage）、shared/cache/tool-priority.ts。
package ctxmgr

import (
	"encoding/json"
	"strings"
)

// Message 可变消息（压缩会原地修改 Content）。
//
// ToolCalls 保持为原始 JSON 结构：压缩时必须保留 tool_calls 与 tool_call_id 的配对，
// 否则服务端会因为「assistant 有 tool_calls 但缺少对应 tool 响应」而 400。
type Message struct {
	Role             string          `json:"role"`
	Content          string          `json:"content"`
	ToolCalls        json.RawMessage `json:"tool_calls,omitempty"`
	ToolCallID       string          `json:"tool_call_id,omitempty"`
	ReasoningContent string          `json:"reasoning_content,omitempty"`
}

// Clone 深拷贝一条消息（压缩作用在副本上，不污染会话聚合状态）。
func (m Message) Clone() Message {
	c := m
	if len(m.ToolCalls) > 0 {
		c.ToolCalls = append(json.RawMessage(nil), m.ToolCalls...)
	}
	return c
}

// CloneMessages 深拷贝消息切片。
func CloneMessages(msgs []Message) []Message {
	out := make([]Message, len(msgs))
	for i, m := range msgs {
		out[i] = m.Clone()
	}
	return out
}

// ToolRetention 工具结果保留优先级。
type ToolRetention string

const (
	// RetentionHigh 代码变更记录 —— 永久保留，snip/prune 阶段不裁剪。
	RetentionHigh ToolRetention = "high"
	// RetentionMedium 构建/测试/lint 结果 —— snip 阶段跳过，prune 阶段裁剪。
	RetentionMedium ToolRetention = "medium"
	// RetentionLow 可重新获取的结果 —— 优先裁剪。
	RetentionLow ToolRetention = "low"
)

// highValueTools 与 v1 tool-priority.ts 的 HIGH_VALUE_TOOLS 一致。
var highValueTools = map[string]struct{}{
	"file_write":  {},
	"file_edit":   {},
	"file_create": {},
	"file_delete": {},
	"move_file":   {},
}

// mediumValueTools 与 v1 tool-priority.ts 的 MEDIUM_VALUE_TOOLS 一致。
var mediumValueTools = map[string]struct{}{
	"terminal_exec":    {},
	"dependency_check": {},
	"code_lint":        {},
	"code_format":      {},
	"code_review":      {},
	"git_operation":    {},
}

// GetToolRetention 返回工具结果的保留优先级（未知工具默认 low，与 v1 一致）。
func GetToolRetention(toolName string) ToolRetention {
	if toolName == "" {
		return RetentionLow
	}
	if _, ok := highValueTools[toolName]; ok {
		return RetentionHigh
	}
	if _, ok := mediumValueTools[toolName]; ok {
		return RetentionMedium
	}
	return RetentionLow
}

// FindToolName 从 tool 消息向前扫描，通过 tool_call_id 找到对应的工具名。//
// 消息结构：assistant(tool_calls) → tool(tool_call_id)。
// 按 ID 匹配比按内容格式推断可靠（v1 同款做法）。
func FindToolName(messages []Message, toolIdx int) string {
	if toolIdx < 0 || toolIdx >= len(messages) {
		return ""
	}
	toolCallID := messages[toolIdx].ToolCallID
	if toolCallID == "" {
		return ""
	}
	for i := toolIdx - 1; i >= 0; i-- {
		m := messages[i]
		if m.Role != "assistant" || len(m.ToolCalls) == 0 {
			continue
		}
		var calls []struct {
			ID       string `json:"id"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}
		if err := json.Unmarshal(m.ToolCalls, &calls); err != nil {
			continue
		}
		for _, tc := range calls {
			if tc.ID == toolCallID && tc.Function.Name != "" {
				return tc.Function.Name
			}
		}
	}
	return ""
}

// toolCallIDs 解析 assistant 消息的 tool_calls，提取全部调用 ID。
func toolCallIDs(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var calls []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &calls); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(calls))
	for _, c := range calls {
		ids = append(ids, c.ID)
	}
	return ids, nil
}

// TotalChars 计算消息列表总字符数（v1 totalChars 的对等实现）。
func TotalChars(messages []Message) int {
	total := 0
	for _, m := range messages {
		total += len(m.Content)
	}
	return total
}

// truncationMarkers 已截断标记 —— 用于避免对同一段内容重复截断。
//
// 覆盖三种截断形式：
//   - "[...已自动截断"  snip 阶段（SnippedKeep）
//   - "[...已省略"      prune 阶段（PrunedKeep）
//   - "结果已截断"      TruncateToolResult 的 MaxToolResultChars 截断
//
// v1 只列了前两种，导致 MaxToolResultChars 截断过的结果在后续 snip/prune 中
// 会被再截一次（叠加两段标记）。v2 把第三种也纳入，修掉这个隐患。
var truncationMarkers = []string{"[...已自动截断", "[...已省略", "结果已截断"}

// alreadyTruncated 判断内容是否已被截断过（v1 的双重截断防护）。
func alreadyTruncated(content string) bool {
	for _, marker := range truncationMarkers {
		if strings.Contains(content, marker) {
			return true
		}
	}
	return false
}
