package memory

import "time"

// Message 是交给 mem0 抽取的一条对话消息。
//
// 字段名与 OpenAI 风格一致，因为 mem0 的 add() 直接消费这个形状。
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Record 是 mem0 返回的一条记忆。
//
// 字段取自自托管服务 server/main.py 的 _serialize_memory()：payload 里的
// data/user_id/agent_id/run_id/hash 被摊平到顶层，其余键归入 metadata。
type Record struct {
	ID        string         `json:"id"`
	Memory    string         `json:"memory"`
	UserID    string         `json:"user_id,omitempty"`
	AgentID   string         `json:"agent_id,omitempty"`
	RunID     string         `json:"run_id,omitempty"`
	Hash      string         `json:"hash,omitempty"`
	Score     float64        `json:"score,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
	CreatedAt string         `json:"created_at,omitempty"`
	UpdatedAt string         `json:"updated_at,omitempty"`
}

// AddOptions 是一次写入（抽取）请求的归属与元数据。
type AddOptions struct {
	RunID    string
	Metadata map[string]any
}

// SearchOptions 是一次召回请求的参数。
type SearchOptions struct {
	// TopK 为 0 时用配置里的默认值。
	TopK int
}

// Turn 是一次已完成 run 的可抽取内容，由引擎在收尾时投递。
type Turn struct {
	RunID     string
	SessionID string
	Prompt    string
	Answer    string
}

// Messages 把一轮问答折成 mem0 需要的消息序列。
//
// 只投递这一轮（用户提问 + 最终答复），而不是整个会话历史：引擎每次 run 只拿到
// 当前提示词，会话历史由前端拼在系统提示词里，本进程无从取回完整原文。mem0 的
// 抽取器面对的是一对问答，这已经足够它提炼出「用户偏好/结论」这类长期事实，
// 而伪造一份它没有的历史才是真正有害的（AGENTS.md：生产路径禁止模拟数据）。
func (t Turn) Messages() []Message {
	msgs := make([]Message, 0, 2)
	if t.Prompt != "" {
		msgs = append(msgs, Message{Role: "user", Content: t.Prompt})
	}
	if t.Answer != "" {
		msgs = append(msgs, Message{Role: "assistant", Content: t.Answer})
	}
	return msgs
}

// ExtractionKey 是「某个 run 是否已回填」的幂等键。
//
// 复用 tool_idempotency 表的 Claim 语义：同一个 run 无论被收尾多少次（取消、
// 崩溃恢复、重复 finish），都只抽取一次。
func ExtractionKey(runID string) string { return "memory.extract:" + runID }

// Stats 是长期记忆的运行时计数，用于日志与诊断。
type Stats struct {
	// RecallCalls / RecallHits / RecallErrors 召回侧计数。
	RecallCalls  uint64
	RecallHits   uint64
	RecallErrors uint64
	// RecallChars 累计注入的字符数。
	RecallChars uint64
	// BackfillEnqueued / BackfillDone / BackfillFailed / BackfillSkipped /
	// BackfillDropped 回填侧计数。Skipped 表示幂等键已存在（这次不重复抽取），
	// Dropped 表示队列满被丢弃。
	BackfillEnqueued uint64
	BackfillDone     uint64
	BackfillFailed   uint64
	BackfillSkipped  uint64
	BackfillDropped  uint64
	// Closed 报告服务是否已关闭。
	Closed bool
	// Enabled 报告配置是否启用。
	Enabled bool
	// Endpoint 是当前使用的服务地址（不含任何密钥）。
	Endpoint string
	// LastError 是最近一次失败的原因（已脱敏的短文本），空表示暂无失败。
	LastError string
	// LastErrorAt 是最近一次失败的时间，零值表示暂无失败。
	LastErrorAt time.Time
}
