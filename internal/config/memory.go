package config

import "time"

// MemoryConfig 是长期记忆（mem0）的配置段。
//
// 为什么单独一个文件：config.go 已经承担了装配期的大部分结构，而记忆段是与模型
// 服务商并列的一块独立能力，单独成文件便于对照 internal/memory 的 Config。
//
// 字段的零值语义（这是老配置兼容的关键）：
//   - Enabled 零值为假，即「默认关闭」——没配过的人行为与本特性不存在时完全一致；
//   - WriteBack 用 *bool 而不是 bool：只有显式写了才覆盖默认（默认开启）。
//     用 bool 的话，一个打开了 enabled 却没有写 write_back 的配置会被零值静默
//     关掉回填，表现为「记忆永远只有读没有写」，是最难排查的一类问题；
//   - 其余数值/时长字段零值表示「用 internal/memory 的默认值」，0 不是合法业务值。
type MemoryConfig struct {
	// Enabled 总开关。默认关闭：记忆依赖一个外部服务，不该在用户没配之前生效。
	Enabled bool `json:"enabled"`
	// Backend 选择后端："mem0"（HTTP，需要外部服务）或 "embedded"（进程内 SQLite，
	// 零依赖）。留空按 endpoint 推断：填了走 mem0，没填走进程内。
	Backend string `json:"backend,omitempty"`
	// Endpoint 是 mem0 服务地址，例如 http://127.0.0.1:8888（上游 compose 把
	// REST API 发布在宿主机 8888；3000 是 dashboard，它不代理 /memories、/search）。
	Endpoint string `json:"endpoint,omitempty"`
	// SecretRef 指向密钥库中的 mem0 API Key（内部密钥库的 ref，不是明文）。
	// 为空表示服务端未开鉴权（AUTH_DISABLED 的本地部署）。
	SecretRef string `json:"secret_ref,omitempty"`
	// UserID / AgentID 是 mem0 的归属维度：UserID 决定「谁的记忆」，
	// 跨会话共享的正是这一维度。
	UserID  string `json:"user_id,omitempty"`
	AgentID string `json:"agent_id,omitempty"`
	// TopK 召回条数上限。
	TopK int `json:"top_k,omitempty"`
	// RecallMaxChars 注入提示词的记忆块字符上限。
	RecallMaxChars int `json:"recall_max_chars,omitempty"`
	// WriteBack 是否在 run 结束后回填抽取；nil 表示用默认（开启）。
	WriteBack *bool `json:"write_back,omitempty"`
	// Timeout / ExtractTimeout 单位为纳秒，与 supervisor / ipc 段保持一致
	// （该文件里所有 Duration 都是裸纳秒，混用两种单位更容易出错）。
	Timeout        time.Duration `json:"timeout,omitempty"`
	ExtractTimeout time.Duration `json:"extract_timeout,omitempty"`
	// QueueDepth / MaxInflight 回填的有界并发参数。
	QueueDepth  int `json:"queue_depth,omitempty"`
	MaxInflight int `json:"max_inflight,omitempty"`
}
