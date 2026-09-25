// Package web 提供联网轻量工具（web_fetch）：GET 抓取 + SSRF 防护 +
// 大小/超时限制 + HTML 转文本。
package web

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/tool"
	"github.com/ximo888ok-netizen/ximo-agent/internal/tool/domains"
)

const (
	maxFetchBytes   = 4 << 20 // 响应体上限 4MB
	defaultTimeout  = 20 * time.Second
	defaultMaxChars = 20000 // 返回文本上限
)

// Tool 实现 web_fetch。
type Tool struct {
	client *http.Client
	now    func() time.Time
	// AllowLoopback 允许访问回环/内网地址。仅用于本地开发与测试；
	// 生产环境必须保持 false（SSRF 防护红线）。
	AllowLoopback bool
}

// New 创建 web_fetch 工具。
func New() *Tool {
	return &Tool{
		client: &http.Client{
			Timeout: defaultTimeout,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return fmt.Errorf("重定向次数超过 5")
				}
				return domains.CheckSSRF(req.URL.String())
			},
		},
		now: time.Now,
	}
}

// NewForLocalTesting 创建允许回环地址的 web_fetch 工具（仅供测试）。
func NewForLocalTesting() *Tool {
	tl := New()
	tl.AllowLoopback = true
	return tl
}

// Definition 实现 tool.Tool。
func (t *Tool) Definition() tool.ToolDefinition {
	return tool.ToolDefinition{
		Name: "web_fetch",
		Description: "抓取网页内容并转换为纯文本（自动去除 script/style/html 标签）。" +
			"内置 SSRF 防护：禁止访问内网、回环与云元数据端点。仅支持 GET。",
		Parameters: tool.ObjectSchema(map[string]tool.JSONSchema{
			"url":      {Type: "string", Description: "目标 URL（http/https）"},
			"maxChars": {Type: "integer", Description: "返回文本字符上限", Default: defaultMaxChars},
		}, "url"),
		Risk:        tool.RiskLow,
		Idempotency: tool.ClassIdempotent,
		Domain:      tool.DomainInProcess,
	}
}

// Execute 实现 tool.Tool。
func (t *Tool) Execute(ctx context.Context, req tool.ToolRequest) tool.ToolResponse {
	fail := func(format string, args ...any) tool.ToolResponse {
		return domains.Fail(req.ToolCallID, "web_fetch", format, args...)
	}
	rawURL := domains.StringArg(req.Arguments, "url")
	if rawURL == "" {
		return fail("缺少 url 参数")
	}
	if !t.AllowLoopback {
		if err := domains.CheckSSRF(rawURL); err != nil {
			return fail("%v", err)
		}
	}
	maxChars := domains.IntArg(req.Arguments, "maxChars", defaultMaxChars)
	if maxChars <= 0 {
		maxChars = defaultMaxChars
	}

	fetchCtx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fail("构造请求失败: %v", err)
	}
	httpReq.Header.Set("User-Agent", "XimoAgent/2.0 (+https://github.com/ximo888ok-netizen/ximo-Agent)")
	httpReq.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain;q=0.9,*/*;q=0.8")

	resp, err := t.client.Do(httpReq)
	if err != nil {
		return fail("请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fail("HTTP %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFetchBytes+1))
	if err != nil {
		return fail("读取响应失败: %v", err)
	}
	if len(body) > maxFetchBytes {
		return fail("响应体超过 %d KB 上限", maxFetchBytes>>10)
	}

	contentType := resp.Header.Get("Content-Type")
	text := string(body)
	if strings.Contains(contentType, "html") {
		text = htmlToText(text)
	}
	text = strings.TrimSpace(collapseBlankLines(text))
	truncated := false
	if len([]rune(text)) > maxChars {
		text = string([]rune(text)[:maxChars])
		truncated = true
	}
	if truncated {
		text += fmt.Sprintf("\n\n...(内容超过 %d 字符，已截断)", maxChars)
	}

	resp2 := domains.Text(req.ToolCallID, "web_fetch", text)
	resp2.Metadata = map[string]any{
		"url":          rawURL,
		"status":       resp.StatusCode,
		"content_type": contentType,
		"chars":        len([]rune(text)),
		"truncated":    truncated,
	}
	return resp2
}

// htmlToText 去除 script/style，剥掉标签并解码常见实体。
func htmlToText(html string) string {
	var b strings.Builder
	i := 0
	for i < len(html) {
		lt := strings.IndexByte(html[i:], '<')
		if lt < 0 {
			b.WriteString(html[i:])
			break
		}
		b.WriteString(html[i : i+lt])
		gt := strings.IndexByte(html[i+lt:], '>')
		if gt < 0 {
			break
		}
		tag := strings.ToLower(html[i+lt+1 : i+lt+gt])
		i += lt + gt + 1
		// 块级标签转换为换行，保留段落结构
		if isBlockTag(tag) {
			b.WriteString("\n")
		}
		// 跳过 script/style 内容
		if strings.HasPrefix(tag, "script") || strings.HasPrefix(tag, "style") {
			closeTag := "</" + strings.Fields(tag)[0]
			if idx := strings.Index(strings.ToLower(html[i:]), closeTag); idx >= 0 {
				i += idx + len(closeTag)
				if next := strings.IndexByte(html[i:], '>'); next >= 0 {
					i += next + 1
				}
			}
		}
	}
	return decodeEntities(b.String())
}

func isBlockTag(tag string) bool {
	name := strings.Fields(tag)
	if len(name) == 0 {
		return false
	}
	switch name[0] {
	case "p", "div", "br", "li", "tr", "h1", "h2", "h3", "h4", "h5", "h6",
		"section", "article", "header", "footer", "main", "ul", "ol", "table", "blockquote", "pre":
		return true
	}
	return false
}

func decodeEntities(s string) string {
	replacer := strings.NewReplacer(
		"&nbsp;", " ",
		"&amp;", "&",
		"&lt;", "<",
		"&gt;", ">",
		"&quot;", "\"",
		"&#39;", "'",
		"&apos;", "'",
		"&mdash;", "—",
		"&hellip;", "…",
	)
	return replacer.Replace(s)
}

func collapseBlankLines(s string) string {
	lines := strings.Split(s, "\n")
	var out []string
	blank := 0
	for _, line := range lines {
		trimmed := strings.TrimRight(line, " \t")
		if trimmed == "" {
			blank++
			if blank > 1 {
				continue
			}
		} else {
			blank = 0
		}
		out = append(out, trimmed)
	}
	return strings.Join(out, "\n")
}
