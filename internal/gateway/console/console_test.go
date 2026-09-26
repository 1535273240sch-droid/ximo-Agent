package console

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newMux 按 server.go 的方式把控制台路由装进 ServeMux（与生产同一条路径）。
func newMux(t *testing.T) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	for _, rt := range Routes() {
		mux.HandleFunc(rt.Pattern, rt.Handler)
	}
	return mux
}

func TestConsoleRoutes(t *testing.T) {
	mux := newMux(t)

	cases := []struct {
		name     string
		path     string
		want     int
		ctype    string
		contains string
	}{
		{"根路径重定向", "/", http.StatusFound, "", ""},
		{"无斜杠重定向", "/console", http.StatusFound, "", ""},
		{"首页", "/console/", http.StatusOK, "text/html", "管理控制台"},
		{"样式", "/console/app.css", http.StatusOK, "text/css", ""},
		{"脚本", "/console/app.js", http.StatusOK, "", "X-Admin-Token"},
		{"不存在的资源", "/console/nope.css", http.StatusNotFound, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, c.path, nil))
			if rec.Code != c.want {
				t.Fatalf("%s %s = %d, want %d", http.MethodGet, c.path, rec.Code, c.want)
			}
			if c.ctype != "" {
				got := rec.Header().Get("Content-Type")
				if !strings.Contains(got, c.ctype) {
					t.Errorf("Content-Type = %q, want 含 %q", got, c.ctype)
				}
			}
			if c.contains != "" && !strings.Contains(rec.Body.String(), c.contains) {
				t.Errorf("响应体不含 %q", c.contains)
			}
		})
	}
}

// 根路由必须是精确匹配（"GET /{$}"），否则它会变成 catch-all：未知的 API 路径
// 会被 302 到控制台，客户端拿到一个 HTML 而不是 JSON 错误 —— 这是实际踩过的坑。
func TestNoCatchAllRoute(t *testing.T) {
	for _, rt := range Routes() {
		if rt.Pattern == "GET /" {
			t.Fatalf("出现了 catch-all 路由 %q；根路径请用 \"GET /{$}\"", rt.Pattern)
		}
	}
	mux := newMux(t)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/does-not-exist", nil))
	if rec.Code == http.StatusFound {
		t.Errorf("未知 API 路径被重定向了，说明存在 catch-all：status=%d Location=%q",
			rec.Code, rec.Header().Get("Location"))
	}
}

// 控制台不应泄露任何令牌：它只服务静态资源，令牌由使用者在页面上输入。
func TestStaticAssetsCarryNoToken(t *testing.T) {
	mux := newMux(t)
	for _, p := range []string{"/console/", "/console/app.css", "/console/app.js"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		body := rec.Body.String()
		for _, bad := range []string{"gwa_", "ximo_sk_", "secretref:v1:"} {
			if strings.Contains(body, bad) {
				t.Errorf("%s 的响应里出现了敏感串 %q", p, bad)
			}
		}
	}
}
