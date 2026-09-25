package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ximo888ok-netizen/ximo-agent/internal/tool"
	"github.com/ximo888ok-netizen/ximo-agent/internal/tool/domains"
)

func exec(t *testing.T, tl tool.Tool, args map[string]any) tool.ToolResponse {
	t.Helper()
	return tl.Execute(context.Background(), tool.ToolRequest{
		RunID: "r", ToolCallID: "c", Name: tl.Definition().Name, Arguments: args,
	})
}

func TestWebFetchSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`<html><head><title>T</title><style>body{color:red}</style></head>
<body><h1>Hello</h1><p>World &amp; more</p><script>alert(1)</script></body></html>`))
	}))
	defer server.Close()

	tl := NewForLocalTesting()
	resp := exec(t, tl, map[string]any{"url": server.URL})
	if !resp.Success {
		t.Fatalf("抓取失败: %+v", resp)
	}
	if !strings.Contains(resp.Content, "Hello") || !strings.Contains(resp.Content, "World & more") {
		t.Fatalf("内容 = %q", resp.Content)
	}
	if strings.Contains(resp.Content, "alert(1)") {
		t.Fatalf("script 内容应被去除: %q", resp.Content)
	}
	if strings.Contains(resp.Content, "color:red") {
		t.Fatalf("style 内容应被去除: %q", resp.Content)
	}
	if resp.Metadata["status"] != 200 {
		t.Fatalf("status = %v", resp.Metadata["status"])
	}
}

func TestWebFetchTruncates(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(strings.Repeat("x", 5000)))
	}))
	defer server.Close()
	tl := NewForLocalTesting()
	resp := exec(t, tl, map[string]any{"url": server.URL, "maxChars": 100})
	if !resp.Success {
		t.Fatalf("抓取失败: %+v", resp)
	}
	if len([]rune(resp.Content)) > 200 {
		t.Fatalf("内容未被截断: %d 字符", len([]rune(resp.Content)))
	}
	if !strings.Contains(resp.Content, "已截断") {
		t.Fatalf("应提示截断: %q", resp.Content[len(resp.Content)-50:])
	}
}

func TestWebFetchHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	tl := NewForLocalTesting()
	resp := exec(t, tl, map[string]any{"url": server.URL})
	if resp.Success {
		t.Fatalf("404 应当失败")
	}
	if !strings.Contains(resp.Error, "404") {
		t.Fatalf("错误 = %q", resp.Error)
	}
}

func TestWebFetchSSRFBlocked(t *testing.T) {
	// SSRF 测试必须用严格模式（禁止回环）的实例。
	tl := New()
	blocked := []string{
		"http://127.0.0.1:8080/admin",
		"http://localhost/admin",
		"http://169.254.169.254/latest/meta-data/",
		"http://10.0.0.1/internal",
		"http://192.168.1.1/",
		"http://172.16.0.1/",
		"http://[::1]/",
		"file:///etc/passwd",
		"ftp://example.com/x",
		"http://metadata.google.internal/computeMetadata/v1/",
	}
	for _, u := range blocked {
		resp := exec(t, tl, map[string]any{"url": u})
		if resp.Success {
			t.Fatalf("URL %q 应当被 SSRF 防护拒绝", u)
		}
		if !strings.Contains(resp.Error, "SSRF") {
			t.Fatalf("URL %q 错误应说明 SSRF: %q", u, resp.Error)
		}
	}
}

func TestWebFetchRedirectToInternalBlocked(t *testing.T) {
	// 公网服务器重定向到内网地址必须被拒绝
	public := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://127.0.0.1:9/secret", http.StatusFound)
	}))
	defer public.Close()
	// httptest 默认监听 127.0.0.1，会被 SSRF 检查拦在第一步；
	// 这里直接验证重定向回调逻辑。
	tl := NewForLocalTesting()
	req, _ := http.NewRequest("GET", public.URL, nil)
	err := tl.client.CheckRedirect(req, []*http.Request{req})
	if err == nil {
		t.Fatalf("重定向到内网应当被拒绝")
	}
}

func TestWebFetchMissingURL(t *testing.T) {
	tl := NewForLocalTesting()
	resp := exec(t, tl, map[string]any{})
	if resp.Success {
		t.Fatalf("缺少 url 应当失败")
	}
}

func TestWebToolDefinition(t *testing.T) {
	tl := NewForLocalTesting()
	def := tl.Definition()
	if def.Name != "web_fetch" || def.Idempotency != tool.ClassIdempotent || def.Domain != tool.DomainInProcess {
		t.Fatalf("定义 = %+v", def)
	}
}

func TestHTMLToText(t *testing.T) {
	html := `<div><p>one</p><p>two</p><table><tr><td>cell</td></tr></table></div>`
	text := htmlToText(html)
	for _, want := range []string{"one", "two", "cell"} {
		if !strings.Contains(text, want) {
			t.Fatalf("文本 %q 应包含 %q", text, want)
		}
	}
	// 嵌套 script 跳过
	text = htmlToText(`<div>keep</div><script>var a = "</div>";</script><div>keep2</div>`)
	if strings.Contains(text, "var a") {
		t.Fatalf("script 未跳过: %q", text)
	}
	if !strings.Contains(text, "keep2") {
		t.Fatalf("script 后的内容丢失: %q", text)
	}
}

func TestSSRFHostParsing(t *testing.T) {
	cases := []struct {
		url     string
		wantErr bool
	}{
		{"https://example.com/a", false},
		{"https://example.com:8443/a", false},
		{"https://user:pass@example.com/a", false},
		{"http://127.0.0.1/a", true},
		{"not-a-url", true},
		{"https:///nohost", true},
	}
	for _, c := range cases {
		err := domains.CheckSSRF(c.url)
		if (err != nil) != c.wantErr {
			t.Errorf("CheckSSRF(%q) err = %v，wantErr = %v", c.url, err, c.wantErr)
		}
	}
}
