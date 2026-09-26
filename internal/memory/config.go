// Package memory 把 mem0（https://github.com/mem0ai/mem0）接入 Go 引擎，提供
// 跨会话的长期记忆：
//
//   - 召回（读）：一次 run 开始前向 mem0 检索与本次提示相关的记忆，作为独立
//     system 消息注入上下文；
//   - 回填（写）：run 结束后把这一轮的用户提问与最终答复交给 mem0 抽取记忆，
//     抽取在 mem0 侧完成，本进程只负责投递。
//
// 依赖边界：本包只 import 标准库、internal/types 与 internal/provider（复用它的
// 「环境变量代理 → Windows 系统代理 → 直连」HTTP 客户端）。它不 import
// internal/engine / internal/tool*，因此引擎与工具层都能安全依赖它，不存在环。
//
// 失败策略：长期记忆是增强能力，不是运行的前提。endpoint 未配、服务不可达、
// 超时、鉴权失败都只降级为「这一轮没有记忆」，绝不阻塞、绝不失败一个 run。
package memory

import (
	"fmt"
	"strings"
	"time"
)

// 默认值。所有默认都偏向「宁可少用记忆，也不要拖慢或打断对话」。
const (
	// DefaultTopK 单次召回返回条数上限。
	DefaultTopK = 5
	// DefaultRecallMaxChars 注入提示词的记忆块字符上限。超过就整条截断丢弃，
	// 而不是把半条记忆塞进上下文。
	DefaultRecallMaxChars = 1200
	// DefaultTimeout 单次 mem0 HTTP 调用超时。它直接等于「每个 run 开始前多出的
	// 等待」，所以刻意取得远小于模型调用超时。
	DefaultTimeout = 700 * time.Millisecond
	// DefaultQueueDepth 回填队列深度。队列满时丢弃并计数，不阻塞 run 收尾。
	DefaultQueueDepth = 32
	// DefaultMaxInflight 回填并发上限。
	DefaultMaxInflight = 2
	// DefaultUserID mem0 侧的「用户」标识：长期记忆按它归属，跨会话共享。
	DefaultUserID = "ximo-user"
	// DefaultAgentID mem0 侧的「代理」标识。
	DefaultAgentID = "ximo-agent"
	// DefaultExtractTimeout 单次回填抽取的超时。它比召回超时大两个数量级是
	// 有意的：抽取在 mem0 侧要调一次 LLM，是「写」而不是「查」。这个超时只
	// 影响后台回填 goroutine 的存活时间，不影响任何一次对话的响应。
	DefaultExtractTimeout = 30 * time.Second
)

// Config 是长期记忆的运行配置。零值即「关闭」。
type Config struct {
	// Enabled 总开关。为假时引擎行为与没有本特性完全一致。
	Enabled bool
	// Backend 选择存储后端：BackendMem0（HTTP，需外部服务）或 BackendEmbedded
	// （进程内 SQLite，零依赖）。留空表示按 Endpoint 推断：填了 endpoint 走 mem0，
	// 没填则走进程内后端 —— 这样"打开开关就能用"是最短路径。
	Backend string
	// Endpoint 是 mem0 服务地址，例如 http://127.0.0.1:8888（上游 compose 的 API
	// 宿主端口；3000 是 dashboard，/memories 与 /search 在它上面会 404）。
	// 进程内后端不需要它。
	Endpoint string
	// SecretRef 指向密钥库中的 mem0 API Key（secrets.Manager 的 ref）。
	// 为空表示服务端未开鉴权（AUTH_DISABLED 的本地开发部署）。
	SecretRef string
	// UserID / AgentID 是 mem0 的归属维度。
	UserID  string
	AgentID string
	// TopK 召回条数上限。
	TopK int
	// RecallMaxChars 记忆块字符上限。
	RecallMaxChars int
	// WriteBack 是否在 run 结束后回填抽取。
	WriteBack bool
	// Timeout 单次调用超时。
	Timeout time.Duration
	// ExtractTimeout 单次回填抽取超时（mem0 侧要调一次 LLM，远长于召回）。
	ExtractTimeout time.Duration
	// QueueDepth / MaxInflight 回填的有界并发参数。
	QueueDepth  int
	MaxInflight int
}

// DefaultConfig 返回一组可直接使用的默认值：关闭，但其余字段都已填好，
// 用户只需打开开关并填 endpoint。
//
// 注意 WriteBack 默认为真：LoadConfig 先取默认值再反序列化覆盖，因此配置文件
// 里没写 write_back 的老用户拿到的是「开启」而不是被零值静默关掉。
func DefaultConfig() Config {
	return Config{
		Enabled:        false,
		Endpoint:       "",
		SecretRef:      "",
		UserID:         DefaultUserID,
		AgentID:        DefaultAgentID,
		TopK:           DefaultTopK,
		RecallMaxChars: DefaultRecallMaxChars,
		WriteBack:      true,
		Timeout:        DefaultTimeout,
		ExtractTimeout: DefaultExtractTimeout,
		QueueDepth:     DefaultQueueDepth,
		MaxInflight:    DefaultMaxInflight,
	}
}

// WithDefaults 把未填的字段补成默认值，并规整 endpoint。
func (c Config) WithDefaults() Config {
	d := DefaultConfig()
	if c.UserID == "" {
		c.UserID = d.UserID
	}
	if c.AgentID == "" {
		c.AgentID = d.AgentID
	}
	if c.TopK <= 0 {
		c.TopK = d.TopK
	}
	if c.RecallMaxChars <= 0 {
		c.RecallMaxChars = d.RecallMaxChars
	}
	if c.Timeout <= 0 {
		c.Timeout = d.Timeout
	}
	if c.QueueDepth <= 0 {
		c.QueueDepth = d.QueueDepth
	}
	if c.MaxInflight <= 0 {
		c.MaxInflight = d.MaxInflight
	}
	c.Endpoint = NormalizeEndpoint(c.Endpoint)
	return c
}

// Active 报告记忆是否可用：开关打开，且**后端所需的字段**已配好。
//
// 进程内后端不需要 endpoint，因此"打开开关就能用"不会因为少填一个地址而被判为未启用。
func (c Config) Active() bool {
	if !c.Enabled {
		return false
	}
	if c.EffectiveBackend() == BackendEmbedded {
		return true
	}
	return c.Endpoint != ""
}

// EffectiveBackend 返回生效的后端名：显式配置优先，留空则按 endpoint 推断。
func (c Config) EffectiveBackend() string {
	switch strings.ToLower(strings.TrimSpace(c.Backend)) {
	case BackendEmbedded, "local", "sqlite", "builtin":
		return BackendEmbedded
	case BackendMem0, "http":
		return BackendMem0
	case "":
		if strings.TrimSpace(c.Endpoint) != "" {
			return BackendMem0
		}
		return BackendEmbedded
	default:
		// 未识别的值交给 Validate 报错，这里先按 mem0 返回，避免静默改变语义。
		return BackendMem0
	}
}

// Validate 校验配置。它只在「启用」时严格：关闭状态下任何字段都可以为空，
// 这样老配置文件（完全没有 memory 段）永远能通过。
func (c Config) Validate() error {
	if !c.Enabled {
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(c.Backend)) {
	case "", BackendMem0, "http", BackendEmbedded, "local", "sqlite", "builtin":
	default:
		return fmt.Errorf("memory: backend 只能是 %q 或 %q，当前为 %q",
			BackendMem0, BackendEmbedded, c.Backend)
	}
	if c.EffectiveBackend() == BackendMem0 {
		if c.Endpoint == "" {
			return fmt.Errorf("memory: 使用 mem0 后端必须配置 endpoint（进程内后端不需要它）")
		}
		if !strings.HasPrefix(c.Endpoint, "http://") && !strings.HasPrefix(c.Endpoint, "https://") {
			return fmt.Errorf("memory: endpoint 必须是 http(s) 地址，当前为 %q", c.Endpoint)
		}
	}
	if c.Timeout <= 0 {
		return fmt.Errorf("memory: timeout 必须为正")
	}
	if c.TopK <= 0 {
		return fmt.Errorf("memory: top_k 必须为正")
	}
	return nil
}

// NormalizeEndpoint 去掉尾部斜杠并补全 scheme。
//
// 与 config.NormalizeBaseURL 同源的做法：把 "127.0.0.1:8888" 这类手敲地址规整
// 成可用的 URL，而不是等到第一次请求才以隐晦的方式失败。
func NormalizeEndpoint(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	s = strings.TrimRight(s, "/")
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		return s
	}
	return "http://" + s
}
