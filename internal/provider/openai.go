// openai.go —— OpenAI 兼容 HTTP 客户端，实现第 18 章调用链。
//
//	Client.Complete / Client.Stream
//	    → RateLimiter.Wait
//	    → CircuitBreaker.Allow
//	    → RetryPolicy（分类重试 + Retry-After）
//	    → http.Client（SSE 解析在 stream.go）
//
// 安全红线（第 21 章）：API Key 只在构造 Authorization 头时出现一次，
// 不写入任何日志、事件、错误消息或返回值。
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/observability"
)

// SecretResolver 在发起单次请求时解析密钥引用。
//
// 由任务 03（secret_ref）→ 任务 04（SecretsProvider.Get）实现。
// 本层不缓存解析结果，避免明文 key 常驻内存。
type SecretResolver interface {
	// Get 解析 secretRef 并返回明文密钥。实现方负责审计与脱敏。
	Get(ctx context.Context, secretRef string) (string, error)
}

// SecretResolverFunc 让普通函数满足 SecretResolver。
type SecretResolverFunc func(ctx context.Context, secretRef string) (string, error)

func (f SecretResolverFunc) Get(ctx context.Context, ref string) (string, error) {
	return f(ctx, ref)
}

// HTTPDoer 抽象 http.Client，便于测试注入。
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// ClientOptions 客户端构造参数。
type ClientOptions struct {
	Config     ProviderConfig
	Secrets    SecretResolver
	HTTPClient HTTPDoer
	Retry      RetryPolicy
	Limiter    *RateLimiter
	Breaker    *CircuitBreaker
	// DefaultTimeout 单次请求超时（不含重试退避）。0 用 5 分钟。
	DefaultTimeout time.Duration
	// Logger 可选；缺省用 observability 全局日志。
	Logger *observability.Logger
}

// Client 一个服务商的 LLM 客户端。
type Client struct {
	cfg     ProviderConfig
	secrets SecretResolver
	http    HTTPDoer
	retry   RetryPolicy
	limiter *RateLimiter
	breaker *CircuitBreaker
	timeout time.Duration
	logger  *observability.Logger

	reqSeq atomic.Uint64
}

// NewClient 按 options 构造客户端并补齐缺省值。
func NewClient(opts ClientOptions) (*Client, error) {
	if opts.Secrets == nil {
		return nil, fmt.Errorf("provider: 缺少 SecretResolver，无法解析 API Key")
	}
	cfg := opts.Config
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, fmt.Errorf("provider: 缺少 BaseURL")
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")

	hc := opts.HTTPClient
	if hc == nil {
		// 代理感知客户端（环境变量 → 系统代理 → 直连）：后端是 spawn 出来的
		// 子进程，环境变量里通常没有代理；只靠默认 Transport 会让系统代理
		// 用户直连无法直达的服务商地址直到超时。超时仍由 ctx 控制。
		hc = ProxyHTTPClient()
	}
	retry := opts.Retry
	if retry.MaxAttempts <= 0 {
		retry = DefaultRetryPolicy()
	}
	timeout := opts.DefaultTimeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}

	return &Client{
		cfg:     cfg,
		secrets: opts.Secrets,
		http:    hc,
		retry:   retry,
		limiter: opts.Limiter,
		breaker: opts.Breaker,
		timeout: timeout,
		logger:  opts.Logger,
	}, nil
}

// Name 实现 Provider。
func (c *Client) Name() string { return c.cfg.Name }

// ContextWindow 实现 Provider。
func (c *Client) ContextWindow() int { return c.cfg.ContextWindow }

// MaxOutputTokens 实现 Provider。
func (c *Client) MaxOutputTokens() int { return c.cfg.MaxOutputTokens }

// Config 返回客户端持有的服务商配置副本（不含任何密钥）。
func (c *Client) Config() ProviderConfig { return c.cfg }

// nextRequestID 生成本次请求的 request_id（第 18 章要求写入日志）。
func (c *Client) nextRequestID() string {
	n := c.reqSeq.Add(1)
	return fmt.Sprintf("req_%d_%d", time.Now().UnixNano(), n)
}

// ---------------------------------------------------------------------------
// 非流式 Complete
// ---------------------------------------------------------------------------

// Complete 发起一次非流式补全，内部走完整重试链。
//
// 注意：请求体仍然带 stream:true（与 v1 一致，服务端按流式返回），
// 本方法在解析阶段把流式响应聚合为单个 CompletionResponse。
func (c *Client) Complete(ctx context.Context, req CompletionRequest) (CompletionResponse, error) {
	meta := req.Meta
	if meta.RequestID == "" {
		meta.RequestID = c.nextRequestID()
	}

	observability.ProviderRequest(c.cfg.Name, req.Model)
	start := time.Now()
	defer func() {
		observability.ProviderLatency(c.cfg.Name, req.Model, float64(time.Since(start).Milliseconds()))
	}()

	body, err := c.buildBody(req)
	if err != nil {
		return CompletionResponse{}, err
	}

	var lastErr error
	attempts := 0

	for attempt := 1; attempt <= c.retry.MaxAttempts; attempt++ {
		attempts = attempt
		meta.Attempt = attempt

		resp, err := c.doOnce(ctx, req, meta, body)
		if err == nil {
			resp.Attempts = attempts
			resp.Meta = meta
			return resp, nil
		}
		lastErr = err

		ce := Classify(err)
		if !Retryable(ce.Class) || attempt >= c.retry.MaxAttempts {
			return CompletionResponse{Attempts: attempts, Meta: meta}, c.wrapFinalError(err, ce, attempt)
		}
		if c.retry.ShouldGiveUpOnRetryAfter(ce) {
			return CompletionResponse{Attempts: attempts, Meta: meta}, c.wrapFinalError(err, ce, attempt)
		}

		c.logRetry(meta, ce)
		observability.ProviderRetry(c.cfg.Name, ce.Class.String())

		if delayErr := backoff(ctx, c.retry.retryDelay(attempt, ce)); delayErr != nil {
			return CompletionResponse{Attempts: attempts, Meta: meta}, delayErr
		}
	}

	return CompletionResponse{Attempts: attempts, Meta: meta},
		c.wrapFinalError(lastErr, Classify(lastErr), attempts)
}

// doOnce 执行一次完整的 限流 → 熔断 → HTTP 调用。
func (c *Client) doOnce(ctx context.Context, req CompletionRequest, meta RequestMeta, body []byte) (CompletionResponse, error) {
	if c.limiter != nil {
		// 限流等待不占用熔断额度。
		if err := c.limiter.Wait(ctx); err != nil {
			return CompletionResponse{}, err
		}
	}

	halfOpen := false
	if c.breaker != nil {
		if !c.breaker.Allow() {
			return CompletionResponse{}, &ClassifiedError{Class: ClassServerError, Err: ErrCircuitOpen}
		}
		halfOpen = c.breaker.State() == BreakerHalfOpen
	}

	resp, err := c.doHTTP(ctx, req, meta, body)

	// 熔断器记账：只有可重试类别计入（见 breaker.go 注释）。
	if c.breaker != nil {
		if err != nil {
			c.breaker.RecordFailure(Classify(err).Class)
		} else {
			c.breaker.RecordSuccess()
		}
		if halfOpen {
			c.breaker.ReleaseHalfOpenSlot()
		}
	}
	return resp, err
}

// doHTTP 真正发起 HTTP 并解析响应。
func (c *Client) doHTTP(ctx context.Context, req CompletionRequest, meta RequestMeta, body []byte) (CompletionResponse, error) {
	timeout := c.timeout
	if req.Timeout > 0 {
		timeout = req.Timeout
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodPost, c.cfg.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return CompletionResponse{}, err
	}

	apiKey, err := c.secrets.Get(ctx, c.cfg.SecretRef)
	if err != nil {
		return CompletionResponse{}, fmt.Errorf("provider: 解析 API Key 失败: %w", err)
	}
	if apiKey == "" {
		return CompletionResponse{}, &APIError{StatusCode: 401, Code: "invalid_api_key", Message: "未配置 API Key"}
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)

	httpResp, err := c.http.Do(httpReq)
	if err != nil {
		// 这里的 err 可能含 URL，但绝不含 Authorization 头。
		return CompletionResponse{}, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(httpResp.Body, 1<<20))
		_ = httpResp.Body.Close()
	}()

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return CompletionResponse{}, c.buildAPIError(httpResp)
	}

	return c.readNonStreamBody(httpResp.Body)
}

// readNonStreamBody 把 SSE 流聚合为单个响应。
func (c *Client) readNonStreamBody(body io.Reader) (CompletionResponse, error) {
	acc := newStreamAccumulator()
	dec := newSSEDecoder(body)
	for {
		payload, ok, err := dec.Next()
		if err != nil {
			return acc.toResponse(), err
		}
		if !ok {
			break
		}
		if stop, err := acc.consume(payload, nil); err != nil {
			return acc.toResponse(), err
		} else if stop {
			break
		}
	}
	return acc.toResponse(), nil
}

// buildAPIError 从非 2xx 响应构造 APIError，并解析 Retry-After。
func (c *Client) buildAPIError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	msg := strings.TrimSpace(string(raw))
	code := ""
	var parsed struct {
		Error struct {
			Message string `json:"message"`
			Code    any    `json:"code"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &parsed) == nil && parsed.Error.Message != "" {
		msg = parsed.Error.Message
		code = parsed.Error.Type
		if code == "" {
			if s, ok := parsed.Error.Code.(string); ok {
				code = s
			}
		}
	}
	if len(msg) > 2048 {
		msg = msg[:2048]
	}

	apiErr := &APIError{StatusCode: resp.StatusCode, Code: code, Message: msg}

	if ra := strings.TrimSpace(resp.Header.Get("Retry-After")); ra != "" {
		if secs, err := strconv.ParseFloat(ra, 64); err == nil && secs >= 0 {
			apiErr.RetryAfter = time.Duration(secs * float64(time.Second))
			apiErr.HasRetryAfter = true
		} else if t, err := http.ParseTime(ra); err == nil {
			if d := time.Until(t); d > 0 {
				apiErr.RetryAfter = d
				apiErr.HasRetryAfter = true
			}
		}
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		observability.Provider429(c.cfg.Name)
	}
	return apiErr
}

// buildBody 构造请求体字节。
func (c *Client) buildBody(req CompletionRequest) ([]byte, error) {
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = c.cfg.MaxOutputTokens
	}
	body := BuildRequestBody(
		req.Model,
		req.Messages,
		req.Tools,
		req.ThinkingMode,
		req.ReasoningEffort,
		req.Temperature,
		maxTokens,
		c.cfg.Capabilities,
	)
	data, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("provider: 序列化请求体失败: %w", err)
	}
	return data, nil
}

// wrapFinalError 在重试耗尽后，把底层错误包装成分类错误并记录日志。
func (c *Client) wrapFinalError(err error, ce *ClassifiedError, attempts int) error {
	if err == nil {
		return nil
	}
	if ce.Class == ClassUserCancelled {
		return err
	}
	c.logFailure(ce, attempts)
	return &ClassifiedError{Class: ce.Class, Err: err, RetryAfter: ce.RetryAfter}
}

// logRetry 记录一次重试（不含密钥）。
func (c *Client) logRetry(meta RequestMeta, ce *ClassifiedError) {
	fields := map[string]any{
		"provider":    c.cfg.Name,
		"error_class": ce.Class.String(),
	}
	for k, v := range meta.Labels() {
		fields[k] = v
	}
	c.log().Info(context.Background(), "provider 请求重试", fields)
}

// logFailure 记录最终失败（不含密钥）。
func (c *Client) logFailure(ce *ClassifiedError, attempts int) {
	fields := map[string]any{
		"provider":    c.cfg.Name,
		"error_class": ce.Class.String(),
		"attempts":    attempts,
	}
	// 只记录错误消息的文本，observability 的脱敏器会再扫一遍 sk- 模式。
	if ce.Err != nil {
		fields["error"] = ce.Err.Error()
	}
	c.log().Error(context.Background(), "provider 请求失败", fields)
}

func (c *Client) log() *observability.Logger {
	if c.logger != nil {
		return c.logger
	}
	return observability.DefaultLogger()
}
