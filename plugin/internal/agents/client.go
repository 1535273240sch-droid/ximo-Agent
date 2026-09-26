package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// DefaultTimeout 是单次 IPC 请求的默认超时。配置读写都是本地内存操作，
// 5 秒足够；再长只会在端点猜错时让 CLI 卡住。
const DefaultTimeout = 5 * time.Second

// ErrNotConnected 表示连接尚未建立或已断开。
var ErrNotConnected = errors.New("ipc: client is not connected")

// ---------------------------------------------------------------------------
// 业务载荷 DTO（与主仓库 internal/ipcapi 的字段逐一对齐）
// ---------------------------------------------------------------------------

// ProviderEntryPayload 是候选池里的一项。SecretRef 是引用，永远不是明文。
type ProviderEntryPayload struct {
	ID              string  `json:"id"`
	Name            string  `json:"name"`
	BaseURL         string  `json:"base_url"`
	Model           string  `json:"model"`
	SecretRef       string  `json:"secret_ref"`
	ContextWindow   int     `json:"context_window"`
	MaxOutputTokens int     `json:"max_output_tokens"`
	RateLimitPerSec float64 `json:"rate_limit_per_sec"`
}

// SubAgentSettingsPayload 是子代理的候选模型分配。字段全为 nil 表示「未携带，
// 沿用现有配置」——这是主仓库的语义，发送时应保持零值。
type SubAgentSettingsPayload struct {
	Pool       []string            `json:"pool,omitempty"`
	ByDivision map[string][]string `json:"by_division,omitempty"`
	ByExpert   map[string][]string `json:"by_expert,omitempty"`
}

// RuntimeSettings 是 system.config.get / set 的载荷。
//
// 注意主仓库 ApplySettings 的合并语义（决定本包怎么发 set）：
//   - provider_id / provider_name / base_url / model / workspace_root 是**无条件覆盖**，
//     发 set 时哪怕不想改也必须原样回填，否则会被清空；
//   - context_window / max_output_tokens / secret_ref / auto_mode 只在非零时覆盖；
//   - providers / mcp_servers 为 nil 表示「未携带，沿用现有配置」；
//   - sub_agent 里各字段为 nil 表示沿用。
//
// 因此写回时必须基于 config.get 的结果构造载荷，而不是凭空造一个。
type RuntimeSettings struct {
	ProviderID      string `json:"provider_id"`
	ProviderName    string `json:"provider_name"`
	BaseURL         string `json:"base_url"`
	Model           string `json:"model"`
	ContextWindow   int    `json:"context_window"`
	MaxOutputTokens int    `json:"max_output_tokens"`
	SecretRef       string `json:"secret_ref"`
	AutoMode        string `json:"auto_mode"`
	WorkspaceRoot   string `json:"workspace_root"`
	DBPath          string `json:"db_path"`
	ConfigPath      string `json:"config_path"`

	// Providers / MCPServers 用 json.RawMessage 承接：本包从不修改它们，
	// 发 set 时一律置 nil（未携带），因此无需理解其内部结构。
	Providers  []json.RawMessage `json:"providers,omitempty"`
	MCPServers []json.RawMessage `json:"mcp_servers,omitempty"`

	SubAgent SubAgentSettingsPayload `json:"sub_agent"`
}

// ModelInfo 是 system.model.list 结果里的单个模型。
type ModelInfo struct {
	ID      string `json:"id"`
	OwnedBy string `json:"owned_by,omitempty"`
}

// ModelList 是 system.model.list 的响应。
type ModelList struct {
	Models  []ModelInfo `json:"models"`
	BaseURL string      `json:"base_url"`
	Error   string      `json:"error,omitempty"`
}

// SecretStatus 是 system.secret.status 的响应，不含任何明文。
type SecretStatus struct {
	Configured bool   `json:"configured"`
	Ref        string `json:"ref"`
	Backend    string `json:"backend"`
	Available  bool   `json:"available"`
}

// ---------------------------------------------------------------------------
// 客户端
// ---------------------------------------------------------------------------

// Client 是 ximo-agent IPC 的最小客户端：一问一答，不做自动重连。
//
// 为什么不做重连：CLI 是一次性进程，请求失败就该报错退出；悄悄重连会让
// 「agent 没在跑」变成「命令卡住」。
type Client struct {
	endpoint string
	timeout  time.Duration
	maxPay   uint32

	connMu sync.Mutex
	conn   net.Conn

	// callMu 串行化整个「发一帧、收一帧」的过程：本协议靠 RequestID 配对，
	// 并发复用同一连接会在读侧抢帧。配置写入量极小，串行完全够用。
	callMu  sync.Mutex
	seq     uint64
	reqSeq  uint64
	session string
}

// NewClient 构造客户端（尚未连接）。
func NewClient(endpoint string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &Client{endpoint: endpoint, timeout: timeout, maxPay: DefaultMaxPayload}
}

// Endpoint 返回目标端点。
func (c *Client) Endpoint() string { return c.endpoint }

// Connect 建立连接。
func (c *Client) Connect(ctx context.Context) error {
	conn, err := DialEndpoint(ctx, c.endpoint, c.timeout)
	if err != nil {
		return fmt.Errorf("%s", describeDialError(c.endpoint, err))
	}
	c.connMu.Lock()
	c.conn = conn
	c.connMu.Unlock()
	return nil
}

// Close 关闭连接。
func (c *Client) Close() error {
	c.connMu.Lock()
	defer c.connMu.Unlock()
	if c.conn == nil {
		return nil
	}
	err := c.conn.Close()
	c.conn = nil
	return err
}

// Ping 用 system.heartbeat 探活（服务端固定回 system.heartbeat_ack）。
//
// 心跳的载荷是裸文本而不是信封，所以这里不能走 call。
func (c *Client) Ping(ctx context.Context) error {
	resp, err := c.request(ctx, TypeHeartbeat, []byte("PING"))
	if err != nil {
		return err
	}
	if resp.Header.Type != TypeHeartbeatAck {
		return fmt.Errorf("ipc: 心跳应答类型异常: %s", resp.Header.Type)
	}
	return nil
}

// Settings 读取运行时配置（system.config.get）。
func (c *Client) Settings(ctx context.Context) (RuntimeSettings, error) {
	var out RuntimeSettings
	err := c.call(ctx, TypeConfigGet, map[string]any{}, &out)
	return out, err
}

// ApplySettings 提交运行时配置（system.config.set）。
func (c *Client) ApplySettings(ctx context.Context, in RuntimeSettings) error {
	return c.call(ctx, TypeConfigSet, in, nil)
}

// PutSecret 把凭据写进平台安全存储（system.secret.put），只返回引用。
//
// value 明文只在本进程内存里存在一次；它绝不进日志、不进错误消息
// （错误在 returned 前会过 redact）。
func (c *Client) PutSecret(ctx context.Context, value string) (string, error) {
	if value == "" {
		return "", errors.New("ipc: refusing to store an empty secret")
	}
	RegisterSecret(value)
	var out struct {
		Ref string `json:"ref"`
	}
	if err := c.call(ctx, TypeSecretPut, map[string]any{"value": value}, &out); err != nil {
		return "", err
	}
	return out.Ref, nil
}

// SecretStatus 查询密钥配置状态（system.secret.status）。
func (c *Client) SecretStatus(ctx context.Context) (SecretStatus, error) {
	var out SecretStatus
	err := c.call(ctx, TypeSecretStatus, map[string]any{}, &out)
	return out, err
}

// ListModels 向服务商查询可用模型（system.model.list）。baseURL 非空时以它为准。
func (c *Client) ListModels(ctx context.Context, baseURL string) (ModelList, error) {
	var out ModelList
	err := c.call(ctx, TypeModelList, map[string]any{"base_url": baseURL}, &out)
	return out, err
}

// call 发送一帧并等回同 RequestID 的响应帧，再解信封。
func (c *Client) call(ctx context.Context, msgType string, payload any, dst any) error {
	body, err := encodeRequest(payload)
	if err != nil {
		return err
	}
	resp, err := c.request(ctx, msgType, body)
	if err != nil {
		return err
	}
	env, err := parseEnvelope(resp)
	if err != nil {
		return err
	}
	return env.Decode(dst)
}

// request 是「发一帧、收同 RequestID 的响应帧」这层原语。
func (c *Client) request(ctx context.Context, msgType string, body []byte) (*Frame, error) {
	c.callMu.Lock()
	defer c.callMu.Unlock()

	c.connMu.Lock()
	conn := c.conn
	c.connMu.Unlock()
	if conn == nil {
		return nil, ErrNotConnected
	}

	deadline := time.Now().Add(c.timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)

	c.reqSeq++
	c.seq++
	reqID := fmt.Sprintf("%s-%d", msgType, c.reqSeq)

	req := &Frame{
		Header: FrameHeader{
			Version:    CurrentVersion,
			RequestID:  reqID,
			SessionID:  c.session,
			Sequence:   c.seq,
			Type:       msgType,
			DeadlineMs: deadline.UnixMilli(),
		},
		Payload: body,
	}
	if err := WriteFrame(conn, req); err != nil {
		return nil, fmt.Errorf("ipc: send %s: %s", msgType, redact(err.Error()))
	}

	// 服务端会广播事件、回心跳等，本客户端只认请求 ID 配对的那一帧；
	// 其他帧读掉丢弃，避免它们在流里堆积导致后续请求错位。
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		resp, err := ReadFrame(conn, c.maxPay)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil, fmt.Errorf("ipc: %s: 对端关闭了连接（ximo-agent 可能已退出）", msgType)
			}
			return nil, fmt.Errorf("ipc: %s: %s", msgType, redact(err.Error()))
		}
		if resp.Header.RequestID != reqID {
			continue
		}
		return resp, nil
	}
}

// encodeRequest 把业务载荷包成 {ok:true,data:...} 信封（与主仓库
// ipcapi.EnvelopeFrom 的请求侧形态一致）。
func encodeRequest(payload any) ([]byte, error) {
	switch v := payload.(type) {
	case nil:
		return json.Marshal(Envelope{OK: true})
	case []byte:
		// 裸文本载荷（心跳那种不含信封的帧）。
		return v, nil
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("ipc: marshal request: %w", err)
	}
	return json.Marshal(Envelope{OK: true, Data: raw})
}
