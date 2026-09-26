package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/provider"
)

// ErrDisabled 表示长期记忆未启用（或被配置为试用不到 endpoint）。
var ErrDisabled = errors.New("memory: 长期记忆未启用")

const (
	// apiKeyHeader 是 mem0 自托管服务识别 API Key 的请求头。
	//
	// 刻意只发这一个头：server/auth.py 的 verify_auth() 一旦看到
	// Authorization: Bearer，就把它当 JWT 解析并直接返回结果，不会回退到
	// X-API-Key。把 API Key 塞进 Bearer 会得到 401，而不是「换一种方式重试」。
	apiKeyHeader = "X-API-Key"

	// maxErrorBody 是错误信息里保留的响应体字符数上限。
	maxErrorBody = 200
)

// ClientOptions 是 Client 的可选装配参数。
type ClientOptions struct {
	// HTTPClient 为空时使用 provider.ProxyHTTPClient()：它已经实现了
	// 「环境变量代理 → Windows 系统代理 → 直连」的解析顺序，回环地址始终直连，
	// 与模型请求走同一条出网路径。
	HTTPClient *http.Client
	// APIKey 返回 mem0 的 API Key。为空或返回空串时不发鉴权头（服务端
	// AUTH_DISABLED 的本地开发部署）。它被设计成函数是因为密钥库读取可能失败，
	// 而「读不到密钥」必须是一次可诊断的请求错误，不是构造期 panic。
	APIKey func(ctx context.Context) (string, error)
	// Now 供测试注入时钟。
	Now func() time.Time
}

// Client 是 mem0 自托管服务的 REST 客户端。
//
// 它只覆盖本工程真正使用的端点：写入（抽取）、检索、列出、删除、健康检查。
// 端点与请求体形状取自 mem0 仓库的 server/main.py（v3 OSS）。
type Client struct {
	cfg    Config
	http   *http.Client
	apiKey func(ctx context.Context) (string, error)
	now    func() time.Time

	mu        sync.Mutex
	lastErr   string
	lastErrAt time.Time
}

// NewClient 构造客户端。cfg 会被补齐默认值。
func NewClient(cfg Config, opts ClientOptions) *Client {
	cfg = cfg.WithDefaults()
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = provider.ProxyHTTPClient()
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Client{
		cfg:    cfg,
		http:   httpClient,
		apiKey: opts.APIKey,
		now:    now,
	}
}

// Config 返回生效配置（只读副本）。
func (c *Client) Config() Config { return c.cfg }

// LastError 返回最近一次失败的脱敏描述与时间。
func (c *Client) LastError() (string, time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastErr, c.lastErrAt
}

// ---------------------------------------------------------------------------
// 端点
// ---------------------------------------------------------------------------

// Add 调用 POST /memories：把消息交给 mem0 抽取并落库。
func (c *Client) Add(ctx context.Context, msgs []Message, opts AddOptions) ([]Record, error) {
	if !c.cfg.Active() {
		return nil, ErrDisabled
	}
	if len(msgs) == 0 {
		return nil, fmt.Errorf("memory: 抽取需要至少一条消息")
	}
	body := map[string]any{
		"messages": msgs,
		"user_id":  c.cfg.UserID,
	}
	if c.cfg.AgentID != "" {
		body["agent_id"] = c.cfg.AgentID
	}
	if opts.RunID != "" {
		body["run_id"] = opts.RunID
	}
	if len(opts.Metadata) > 0 {
		body["metadata"] = opts.Metadata
	}

	var out struct {
		Results []Record `json:"results"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/memories", body, &out); err != nil {
		return nil, err
	}
	return out.Results, nil
}

// Search 调用 POST /search 做语义检索。
func (c *Client) Search(ctx context.Context, query string, opts SearchOptions) ([]Record, error) {
	if !c.cfg.Active() {
		return nil, ErrDisabled
	}
	topK := opts.TopK
	if topK <= 0 {
		topK = c.cfg.TopK
	}
	body := map[string]any{
		"query":   query,
		"top_k":   topK,
		"filters": map[string]any{"user_id": c.cfg.UserID},
	}

	var out struct {
		Results []Record `json:"results"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/search", body, &out); err != nil {
		return nil, err
	}
	return out.Results, nil
}

// GetAll 调用 GET /memories 列出归属当前 user 的记忆。
func (c *Client) GetAll(ctx context.Context, topK int) ([]Record, error) {
	if !c.cfg.Active() {
		return nil, ErrDisabled
	}
	if topK <= 0 {
		topK = c.cfg.TopK
	}
	path := fmt.Sprintf("/memories?user_id=%s&top_k=%d", urlQueryEscape(c.cfg.UserID), topK)

	var out struct {
		Results []Record `json:"results"`
	}
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out.Results, nil
}

// Delete 调用 DELETE /memories/{id} 删除一条记忆（forget）。
func (c *Client) Delete(ctx context.Context, id string) error {
	if !c.cfg.Active() {
		return ErrDisabled
	}
	if id == "" {
		return fmt.Errorf("memory: 删除需要 id")
	}
	return c.doJSON(ctx, http.MethodDelete, "/memories/"+urlQueryEscape(id), nil, nil)
}

// Ping 调用 GET /configure 做一次带鉴权的健康检查。
//
// 选它而不是 /memories：它同时验证「服务活着」与「密钥可用」，且不返回记忆内容。
func (c *Client) Ping(ctx context.Context) error {
	if c.cfg.Endpoint == "" {
		return ErrDisabled
	}
	return c.doJSON(ctx, http.MethodGet, "/configure", nil, nil)
}

// ---------------------------------------------------------------------------
// 传输
// ---------------------------------------------------------------------------

func (c *Client) doJSON(ctx context.Context, method, path string, body any, out any) error {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()

	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("memory: 序列化请求失败: %w", err)
		}
		reader = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.cfg.Endpoint+path, reader)
	if err != nil {
		return fmt.Errorf("memory: 构造请求失败: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	key, err := c.resolveKey(ctx)
	if err != nil {
		c.noteError(err)
		return err
	}
	if key != "" {
		req.Header.Set(apiKeyHeader, key)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		wrapped := fmt.Errorf("memory: 请求 %s %s 失败: %w", method, path, err)
		c.noteError(wrapped)
		return wrapped
	}
	defer func() { _ = resp.Body.Close() }()

	payload, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		wrapped := fmt.Errorf("memory: %s %s 返回 %d: %s",
			method, path, resp.StatusCode, c.redact(snippet(payload, key)))
		c.noteError(wrapped)
		return wrapped
	}
	if readErr != nil {
		wrapped := fmt.Errorf("memory: 读取 %s %s 响应失败: %w", method, path, readErr)
		c.noteError(wrapped)
		return wrapped
	}
	if out == nil || len(payload) == 0 {
		return nil
	}
	if err := json.Unmarshal(payload, out); err != nil {
		wrapped := fmt.Errorf("memory: 解析 %s %s 响应失败: %w", method, path, err)
		c.noteError(wrapped)
		return wrapped
	}
	return nil
}

func (c *Client) resolveKey(ctx context.Context) (string, error) {
	if c.apiKey == nil {
		return "", nil
	}
	key, err := c.apiKey(ctx)
	if err != nil {
		return "", fmt.Errorf("memory: 读取 mem0 密钥失败: %w", err)
	}
	return strings.TrimSpace(key), nil
}

func (c *Client) noteError(err error) {
	if err == nil {
		return
	}
	c.mu.Lock()
	c.lastErr = err.Error()
	c.lastErrAt = c.now()
	c.mu.Unlock()
}

// redact 保证密钥绝不出现在错误信息里。
//
// 关闭日志里出现明文密钥是最容易犯、代价最大的一类错误；这里对拼接好的消息再
// 兜一次底，而不是依赖「调用方记得别打印」。
func (c *Client) redact(s string) string {
	key, _ := c.resolveKey(context.Background())
	if key == "" {
		return s
	}
	return strings.ReplaceAll(s, key, "***")
}

// snippet 截断响应体，避免把整页 HTML 错误页塞进日志。
func snippet(b []byte, key string) string {
	s := strings.TrimSpace(string(b))
	if key != "" {
		s = strings.ReplaceAll(s, key, "***")
	}
	if len(s) > maxErrorBody {
		return s[:maxErrorBody] + "…"
	}
	return s
}

// urlQueryEscape 只做最小的查询串转义，避免为一个 ID/user 引入 net/url 的
// 完整解析路径（ID 是 uuid，user 是配置项）。
func urlQueryEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.', r == '~':
			b.WriteRune(r)
		default:
			for _, by := range []byte(string(r)) {
				fmt.Fprintf(&b, "%%%02X", by)
			}
		}
	}
	return b.String()
}
