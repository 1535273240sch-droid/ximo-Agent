// vision.go —— 多模态（视觉）调用能力。
//
// 对应 v1 src/main/tools/Vision/（vision-api.ts、VisionTool.ts），归入本任务的
// 「多模态能力」。用途：让模型读图 —— UI 截图分析、设计稿评审、图表/文档理解。
//
// 与文本 Provider 的关系：复用同一套 重试/熔断/限流 链路与密钥注入，
// 但请求体是 OpenAI 的「content 数组」形态（text + image_url 混合），
// 因此单独构造 body，而不走 BuildRequestBody 的纯文本路径。
//
// v1 的 callVisionWithWait 有个 5 分钟「思考模式空响应持续等待」循环 ——
// 那个循环用固定 3s 间隔无上限重试同一请求，在 v2 里被更严格的重试策略取代：
// 空响应按「思考中」重试，但受 RetryPolicy 的次数上限约束（不能无限等）。
package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/observability"
)

// VisionSystemPrompt 视觉分析系统提示词（与 v1 逐条一致）。
//
// 这份提示词的核心要求是「完整覆盖 + 原文保留 + 细节优先」——
// 视觉分析的价值全在于不漏细节，概括性的描述对后续 UI/设计分析没有价值。
const VisionSystemPrompt = `你是一个专业的视觉分析助手。你的任务是对图像内容进行完整、详尽、不遗漏任何细节的描述。

## 强制规则
1. **完整覆盖**：必须描述图像中所有可见的内容，包括但不限于：
   - 所有 UI 元素（按钮、输入框、下拉框、复选框、标签、徽章、进度条等）
   - 所有文本内容（原文逐字提取，不得概括或省略）
   - 所有图标和图片（描述其外观和含义）
   - 布局结构（区域划分、网格、行列、层级关系）
   - 视觉样式（颜色、字体、间距、圆角、阴影、边框）
   - 状态信息（加载态、空态、错误提示、禁用态、选中态）
   - 交互元素（可点击区域、hover 效果、焦点状态）
2. **结构化输出**：按区域/模块组织描述，使用清晰的标题和列表
3. **原文保留**：所有文字内容必须原文保留，不得翻译、概括或省略
4. **细节优先**：宁可过度描述也不可遗漏。每个细节都可能对后续分析至关重要
5. **异常标注**：发现的任何 UI 问题、布局错位、文字溢出、对比度不足等异常必须明确标注`

// ContentPartType 内容块类型（OpenAI 多模态协议）。
type ContentPartType string

const (
	// PartText 文本块。
	PartText ContentPartType = "text"
	// PartImageURL 图片块（支持 http(s) URL 与 data: URI 的 base64）。
	PartImageURL ContentPartType = "image_url"
)

// ContentPart 多模态消息的一个内容块。
type ContentPart struct {
	Type ContentPartType
	// Text 当 Type == PartText 时使用。
	Text string
	// ImageURL 当 Type == PartImageURL 时使用。
	ImageURL string
}

// TextPart 构造文本块。
func TextPart(text string) ContentPart {
	return ContentPart{Type: PartText, Text: text}
}

// ImagePart 构造图片块，url 可为 http(s) 链接或 data:image/...;base64,xxx。
func ImagePart(url string) ContentPart {
	return ContentPart{Type: PartImageURL, ImageURL: url}
}

// VisionRequest 一次视觉分析请求。
type VisionRequest struct {
	Meta RequestMeta

	Model  string
	Prompt string
	// Images 待分析图片（可多张 —— v1 只传一张，v2 支持多图对比，
	// 例如「改前 vs 改后」的 UI 回归检查）。
	Images []string

	// EnableThinking 思考模式。开启时不发 temperature（与文本 provider 同规则）。
	EnableThinking bool
	MaxTokens      int

	// SystemPrompt 覆盖默认的 VisionSystemPrompt；空则用默认。
	SystemPrompt string
	Timeout      time.Duration
}

// VisionResponse 视觉分析结果。
type VisionResponse struct {
	Content          string
	ReasoningContent string
	Usage            *TokenUsage
	Attempts         int
	Meta             RequestMeta
}

// buildVisionBody 构造多模态请求体。
func (c *Client) buildVisionBody(req VisionRequest) ([]byte, error) {
	system := req.SystemPrompt
	if system == "" {
		system = VisionSystemPrompt
	}

	// user 消息的 content 是块数组：文本在前，图片依次在后。
	parts := make([]map[string]any, 0, len(req.Images)+1)
	parts = append(parts, map[string]any{"type": string(PartText), "text": SanitizeContent(req.Prompt)})
	for _, img := range req.Images {
		parts = append(parts, map[string]any{
			"type":      string(PartImageURL),
			"image_url": map[string]any{"url": img},
		})
	}

	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 8192
	}

	body := map[string]any{
		"model": req.Model,
		"messages": []map[string]any{
			{"role": string(RoleSystem), "content": system},
			{"role": string(RoleUser), "content": parts},
		},
		"max_tokens": ClampMaxTokens(maxTokens),
		// 视觉请求走单次响应，不用 SSE。
		"stream": false,
	}

	if req.EnableThinking {
		body["enable_thinking"] = true
	} else {
		body["temperature"] = 0.3
	}

	return json.Marshal(body)
}

// AnalyzeImages 执行一次视觉分析（复用 限流 → 熔断 → 分类重试 链路）。
//
// 与 v1 的差异：v1 的 callVisionWithWait 在思考模式下会以固定间隔无限重试空响应
// （最长 5 分钟）。v2 保留了「空响应 = 思考中，值得重试」的判断，
// 但重试次数由 RetryPolicy 约束 —— 避免一个持续返回空的端点把调用方挂死。
func (c *Client) AnalyzeImages(ctx context.Context, req VisionRequest) (VisionResponse, error) {
	if len(req.Images) == 0 {
		return VisionResponse{}, fmt.Errorf("provider: 视觉分析需要至少一张图片")
	}

	meta := req.Meta
	if meta.RequestID == "" {
		meta.RequestID = c.nextRequestID()
	}

	body, err := c.buildVisionBody(req)
	if err != nil {
		return VisionResponse{}, err
	}

	observability.ProviderRequest(c.cfg.Name, req.Model)
	start := time.Now()
	defer func() {
		observability.ProviderLatency(c.cfg.Name, req.Model, float64(time.Since(start).Milliseconds()))
	}()

	var lastErr error
	attempts := 0

	for attempt := 1; attempt <= c.retry.MaxAttempts; attempt++ {
		attempts = attempt
		meta.Attempt = attempt

		resp, empty, err := c.doVisionOnce(ctx, req, meta, body)
		if err == nil {
			if !empty {
				resp.Attempts = attempts
				resp.Meta = meta
				return resp, nil
			}
			// 空响应：思考模式下视为「还在思考」，值得再试一次；
			// 非思考模式下空响应是异常，同样重试（可能是瞬时服务端问题）。
			lastErr = fmt.Errorf("provider: 视觉 API 返回空内容")
			if attempt < c.retry.MaxAttempts {
				observability.ProviderRetry(c.cfg.Name, "empty_vision_response")
				if delayErr := backoff(ctx, c.retry.retryDelay(attempt, &ClassifiedError{Class: ClassServerError})); delayErr != nil {
					return VisionResponse{Attempts: attempts, Meta: meta}, delayErr
				}
				continue
			}
			return resp, lastErr
		}

		lastErr = err
		ce := Classify(err)
		if !Retryable(ce.Class) || attempt >= c.retry.MaxAttempts {
			return VisionResponse{Attempts: attempts, Meta: meta}, c.wrapFinalError(err, ce, attempt)
		}
		if c.retry.ShouldGiveUpOnRetryAfter(ce) {
			return VisionResponse{Attempts: attempts, Meta: meta}, c.wrapFinalError(err, ce, attempt)
		}

		c.logRetry(meta, ce)
		observability.ProviderRetry(c.cfg.Name, ce.Class.String())

		if delayErr := backoff(ctx, c.retry.retryDelay(attempt, ce)); delayErr != nil {
			return VisionResponse{Attempts: attempts, Meta: meta}, delayErr
		}
	}

	return VisionResponse{Attempts: attempts, Meta: meta}, lastErr
}

// doVisionOnce 执行一次视觉请求（限流 + 熔断 + HTTP），返回原始响应。
//
// empty=true 表示响应体里没有任何可用的文本内容。
func (c *Client) doVisionOnce(ctx context.Context, req VisionRequest, meta RequestMeta, body []byte) (VisionResponse, bool, error) {
	if c.limiter != nil {
		if err := c.limiter.Wait(ctx); err != nil {
			return VisionResponse{}, false, err
		}
	}

	halfOpen := false
	if c.breaker != nil {
		if !c.breaker.Allow() {
			return VisionResponse{}, false, &ClassifiedError{Class: ClassServerError, Err: ErrCircuitOpen}
		}
		halfOpen = c.breaker.State() == BreakerHalfOpen
	}

	resp, empty, err := c.doVisionHTTP(ctx, req, body)

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
	return resp, empty, err
}

// doVisionHTTP 发起 HTTP 并解析非流式响应。
func (c *Client) doVisionHTTP(ctx context.Context, req VisionRequest, body []byte) (VisionResponse, bool, error) {
	timeout := c.timeout
	if req.Timeout > 0 {
		timeout = req.Timeout
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	httpReq, err := newJSONRequest(reqCtx, c.cfg.BaseURL+"/chat/completions", body)
	if err != nil {
		return VisionResponse{}, false, err
	}

	apiKey, err := c.secrets.Get(ctx, c.cfg.SecretRef)
	if err != nil {
		return VisionResponse{}, false, fmt.Errorf("provider: 解析 API Key 失败: %w", err)
	}
	if apiKey == "" {
		return VisionResponse{}, false, &APIError{StatusCode: 401, Code: "invalid_api_key", Message: "未配置 API Key"}
	}
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)

	httpResp, err := c.http.Do(httpReq)
	if err != nil {
		return VisionResponse{}, false, err
	}
	defer func() { _ = httpResp.Body.Close() }()

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return VisionResponse{}, false, c.buildAPIError(httpResp)
	}

	resp, err := parseVisionResponse(httpResp.Body)
	if err != nil {
		return VisionResponse{}, false, err
	}
	empty := strings.TrimSpace(resp.Content) == ""
	return resp, empty, nil
}

// visionRaw 非流式响应体。
type visionRaw struct {
	Choices []struct {
		Message struct {
			// Content 可能是字符串，也可能是块数组（多模态返回形态）。
			Content          json.RawMessage `json:"content"`
			ReasoningContent string          `json:"reasoning_content"`
		} `json:"message"`
	} `json:"choices"`
	Usage *rawUsage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// parseVisionResponse 解析非流式响应，兼容字符串与块数组两种 content 形态。
func parseVisionResponse(r io.Reader) (VisionResponse, error) {
	raw, err := io.ReadAll(io.LimitReader(r, 16<<20))
	if err != nil {
		return VisionResponse{}, fmt.Errorf("provider: 读取视觉响应失败: %w", err)
	}

	var parsed visionRaw
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return VisionResponse{}, fmt.Errorf("provider: 解析视觉响应失败: %w", err)
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return VisionResponse{}, &APIError{StatusCode: 400, Code: parsed.Error.Type, Message: parsed.Error.Message}
	}
	if len(parsed.Choices) == 0 {
		return VisionResponse{}, nil
	}

	msg := parsed.Choices[0].Message
	content := extractContentText(msg.Content)

	var usage *TokenUsage
	if parsed.Usage != nil {
		u := NormalizeUsage(
			parsed.Usage.PromptTokens,
			parsed.Usage.CompletionTokens,
			parsed.Usage.TotalTokens,
			parsed.Usage.PromptCacheHitTokens,
			parsed.Usage.PromptCacheMissToken,
			usageNestedCached(parsed.Usage),
			usageReasoning(parsed.Usage),
		)
		usage = &u
	}

	return VisionResponse{
		Content:          content,
		ReasoningContent: msg.ReasoningContent,
		Usage:            usage,
	}, nil
}

// extractContentText 从 content 字段提取纯文本。
//
// 三种形态：字符串 / 块数组 / null。块数组形态下只取 type=="text" 的块
// （v1 vision-api.ts 同款过滤）。
func extractContentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}

	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return asString
	}

	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		texts := make([]string, 0, len(blocks))
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				texts = append(texts, b.Text)
			}
		}
		return strings.Join(texts, "\n")
	}

	return ""
}

// VisionFallback 在正文为空时回退到 reasoning_content（v1 同款降级）。
//
// 思考模式下模型可能只产出 reasoning，此时用思考内容兜底总好过返回空。
func VisionFallback(resp VisionResponse) string {
	if strings.TrimSpace(resp.Content) != "" {
		return resp.Content
	}
	return resp.ReasoningContent
}
