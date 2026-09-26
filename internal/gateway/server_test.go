package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/httpx"
	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/model"
	"github.com/ximo888ok-netizen/ximo-agent/internal/observability"
)

// ---------------------------------------------------------------------------
// 测试公共设施
// ---------------------------------------------------------------------------

// logCapture 是并发安全的日志收集器（日志可能来自 handler 之外的 goroutine）。
type logCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *logCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *logCapture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

// entries 解析已采集的日志行（每行一个 JSON 对象）。
func (c *logCapture) entries(t *testing.T) []observability.StructuredLogEntry {
	t.Helper()
	var out []observability.StructuredLogEntry
	for _, line := range strings.Split(strings.TrimSpace(c.String()), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var e observability.StructuredLogEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("日志行不是合法 JSON: %v\n%s", err, line)
		}
		out = append(out, e)
	}
	return out
}

func (c *logCapture) find(t *testing.T, msg string) map[string]any {
	t.Helper()
	entries := c.entries(t)
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Message == msg {
			return entries[i].Fields
		}
	}
	t.Fatalf("未找到日志 %q；实际日志:\n%s", msg, c.String())
	return nil
}

func testLogger() (*observability.Logger, *logCapture) {
	cap := &logCapture{}
	return observability.NewLogger(cap, observability.LevelDebug), cap
}

func testConfig() Config {
	cfg := DefaultConfig()
	cfg.AdminToken = "test-admin-token"
	return cfg
}

// newTestServer 构造一个可用 Server（logger 走内存收集器），返回收集器便于断言日志。
func newTestServer(t *testing.T, routes []httpx.Route, mutate ...func(*Config)) (*Server, *logCapture) {
	t.Helper()
	cfg := testConfig()
	for _, m := range mutate {
		m(&cfg)
	}
	logger, cap := testLogger()
	srv, err := New(cfg, routes, logger)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv, cap
}

func do(srv *Server, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, r)
	return rec
}

func jsonBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v\n%s", err, rec.Body.String())
	}
	return body
}

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

func TestDefaultConfigHasSaneDefaults(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Addr == "" || cfg.DBPath == "" {
		t.Fatalf("默认 addr/db 不能为空: %+v", cfg)
	}
	if cfg.AdminToken != "" {
		t.Fatal("管理令牌不得有硬编码默认值")
	}
	if err := cfg.Validate(); !errors.Is(err, ErrAdminTokenRequired) {
		t.Fatalf("缺管理令牌应报 ErrAdminTokenRequired，得到 %v", err)
	}
	if cfg.MaxOutputTokens <= 0 || cfg.RateLimitPerMin <= 0 || cfg.MaxBodyBytes <= 0 {
		t.Fatalf("数值类默认值应为正: %+v", cfg)
	}
}

func TestParseConfigFlagsEnvAndErrors(t *testing.T) {
	t.Setenv(envAdminToken, "")

	t.Run("缺管理令牌报错", func(t *testing.T) {
		if _, err := parseConfig(nil, io.Discard); !errors.Is(err, ErrAdminTokenRequired) {
			t.Fatalf("期望 ErrAdminTokenRequired，得到 %v", err)
		}
	})

	t.Run("环境变量回退", func(t *testing.T) {
		t.Setenv(envAdminToken, "env-token")
		cfg, err := parseConfig(nil, io.Discard)
		if err != nil {
			t.Fatalf("parseConfig: %v", err)
		}
		if cfg.AdminToken != "env-token" {
			t.Fatalf("应取环境变量，得到 %q", cfg.AdminToken)
		}
	})

	t.Run("flag 覆盖环境变量与默认值", func(t *testing.T) {
		t.Setenv(envAdminToken, "env-token")
		cfg, err := parseConfig([]string{
			"--addr=127.0.0.1:18080",
			"--db=/tmp/gw.db",
			"--admin-token=flag-token",
			"--price-micro-per-ktok=7",
			"--max-output-tokens=512",
			"--request-timeout=30s",
			"--rate-limit-per-min=5",
		}, io.Discard)
		if err != nil {
			t.Fatalf("parseConfig: %v", err)
		}
		want := Config{
			Addr: "127.0.0.1:18080", DBPath: "/tmp/gw.db", AdminToken: "flag-token",
			PriceMicroPerKTok: 7, MaxOutputTokens: 512, RequestTimeout: 30 * time.Second,
			RateLimitPerMin: 5, MaxBodyBytes: DefaultMaxBodyBytes,
		}
		if cfg != want {
			t.Fatalf("配置不符:\n got %+v\nwant %+v", cfg, want)
		}
	})

	t.Run("非法取值报错且消息不含令牌", func(t *testing.T) {
		_, err := parseConfig([]string{"--admin-token=secret-token", "--max-output-tokens=0"}, io.Discard)
		if err == nil {
			t.Fatal("max-output-tokens=0 应报错")
		}
		if !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("期望 ErrInvalidConfig，得到 %v", err)
		}
		if strings.Contains(err.Error(), "secret-token") {
			t.Fatal("配置错误消息不得包含令牌明文")
		}
	})
}

func TestConfigLogFieldsNeverCarryToken(t *testing.T) {
	cfg := testConfig()
	blob, err := json.Marshal(cfg.LogFields())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(blob), cfg.AdminToken) {
		t.Fatal("日志字段不得包含管理令牌明文")
	}
	if !strings.Contains(string(blob), `"admin_token_configured":true`) {
		t.Fatalf("应报告令牌已配置，得到 %s", blob)
	}
}

// ---------------------------------------------------------------------------
// 路由装配与 ServeMux 能力
// ---------------------------------------------------------------------------

func TestNewRejectsBrokenRoutes(t *testing.T) {
	ok := httpx.Route{Pattern: "GET /v1/health", Handler: func(http.ResponseWriter, *http.Request) {}}
	cases := []struct {
		name    string
		routes  []httpx.Route
		wantSub string
	}{
		{"重复 pattern", []httpx.Route{ok, ok}, "重复注册"},
		{"空 pattern", []httpx.Route{{Handler: ok.Handler}}, "不能为空"},
		{"nil handler", []httpx.Route{{Pattern: "GET /v1/x"}}, "handler 为 nil"},
		{"非法 pattern", []httpx.Route{{Pattern: "/v1/{x", Handler: ok.Handler}}, "注册路由"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logger, _ := testLogger()
			_, err := New(testConfig(), tc.routes, logger)
			if err == nil {
				t.Fatal("期望装配失败")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("错误应包含 %q，得到 %v", tc.wantSub, err)
			}
		})
	}
}

func TestPathValueAndMethodMatching(t *testing.T) {
	routes := []httpx.Route{
		{Pattern: "POST /admin/users/{id}/status", Handler: func(w http.ResponseWriter, r *http.Request) {
			httpx.WriteJSON(w, http.StatusOK, map[string]string{"id": r.PathValue("id")})
		}},
		{Pattern: "GET /v1/models/{model}/usage", Handler: func(w http.ResponseWriter, r *http.Request) {
			httpx.WriteJSON(w, http.StatusOK, map[string]string{"model": r.PathValue("model")})
		}},
	}
	srv, _ := newTestServer(t, routes)

	rec := do(srv, httptest.NewRequest(http.MethodPost, "/admin/users/u_123/status", nil))
	if rec.Code != http.StatusOK || jsonBody(t, rec)["id"] != "u_123" {
		t.Fatalf("路径参数未生效: %d %s", rec.Code, rec.Body.String())
	}

	rec = do(srv, httptest.NewRequest(http.MethodGet, "/v1/models/gpt-4o/usage", nil))
	if rec.Code != http.StatusOK || jsonBody(t, rec)["model"] != "gpt-4o" {
		t.Fatalf("路径参数未生效: %d %s", rec.Code, rec.Body.String())
	}

	if rec := do(srv, httptest.NewRequest(http.MethodGet, "/admin/users/u_123/status", nil)); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("方法不匹配应为 405，得到 %d", rec.Code)
	}
	if rec := do(srv, httptest.NewRequest(http.MethodGet, "/nope", nil)); rec.Code != http.StatusNotFound {
		t.Fatalf("未知路径应为 404，得到 %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// 中间件链
// ---------------------------------------------------------------------------

func TestRecoverPanicReturns500WithoutStackOrSecret(t *testing.T) {
	const secret = "ximo_sk_0123456789abcdef0123456789abcdef"
	routes := []httpx.Route{{Pattern: "GET /boom", Handler: func(http.ResponseWriter, *http.Request) {
		panic("boom leaked " + secret)
	}}}
	srv, logs := newTestServer(t, routes)

	rec := do(srv, httptest.NewRequest(http.MethodGet, "/boom", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("panic 应回 500，得到 %d", rec.Code)
	}
	body := rec.Body.String()
	for _, bad := range []string{"goroutine", "runtime.", "boom", secret, "gateway/middleware.go"} {
		if strings.Contains(body, bad) {
			t.Fatalf("响应体不得包含 %q: %s", bad, body)
		}
	}
	if code := jsonBody(t, rec)["error"].(map[string]any)["code"]; code != "internal_error" {
		t.Fatalf("错误码应为 internal_error，得到 %v", code)
	}

	fields := logs.find(t, "gateway: handler panic 已恢复")
	if !strings.Contains(fmt.Sprint(fields["panic"]), "boom") {
		t.Fatalf("日志应记录 panic 值: %v", fields["panic"])
	}
	if strings.Contains(fmt.Sprint(fields["panic"]), secret) {
		t.Fatalf("日志里的 panic 值必须脱敏: %v", fields["panic"])
	}
	if !strings.Contains(fmt.Sprint(fields["stack"]), "gateway") {
		t.Fatalf("日志应记录栈: %v", fields["stack"])
	}
}

func TestRecoverAfterStreamStartedOnlyLogs(t *testing.T) {
	routes := []httpx.Route{{Pattern: "GET /half", Handler: func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("partial"))
		panic("late panic")
	}}}
	srv, logs := newTestServer(t, routes)

	rec := do(srv, httptest.NewRequest(http.MethodGet, "/half", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("已发出的状态码不应被改写，得到 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "partial") {
		t.Fatalf("已写出的内容应保留: %s", rec.Body.String())
	}
	logs.find(t, "gateway: handler panic 已恢复")
}

func TestRequestIDGeneratedAndPropagated(t *testing.T) {
	routes := []httpx.Route{{Pattern: "GET /id", Handler: func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(httpx.RequestID(r)))
	}}}
	srv, logs := newTestServer(t, routes)

	t.Run("透传客户端 ID 并写入 context", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/id", nil)
		req.Header.Set(headerRequestID, "abc-123_XYZ")
		rec := do(srv, req)
		if got := rec.Header().Get(headerRequestID); got != "abc-123_XYZ" {
			t.Fatalf("响应头应回带请求 ID，得到 %q", got)
		}
		if body := rec.Body.String(); body != "abc-123_XYZ" {
			t.Fatalf("handler 应读到请求 ID，得到 %q", body)
		}
		fields := logs.find(t, "gateway: 请求完成")
		if fields["request_id"] != "abc-123_XYZ" {
			t.Fatalf("访问日志应带请求 ID，得到 %v", fields["request_id"])
		}
	})

	t.Run("缺失或非法时生成新 ID", func(t *testing.T) {
		first := do(srv, httptest.NewRequest(http.MethodGet, "/id", nil))
		second := do(srv, httptest.NewRequest(http.MethodGet, "/id", nil))
		a, b := first.Header().Get(headerRequestID), second.Header().Get(headerRequestID)
		if a == "" || !strings.HasPrefix(a, "req_") {
			t.Fatalf("应生成 req_ 前缀的请求 ID，得到 %q", a)
		}
		if a == b {
			t.Fatal("两次请求不应复用同一个 ID")
		}
	})

	t.Run("非法 ID 被丢弃", func(t *testing.T) {
		for _, bad := range []string{"bad id", "line\nbreak", strings.Repeat("x", maxRequestIDLen+1)} {
			if got := sanitizeRequestID(bad); got != "" {
				t.Fatalf("非法 ID %q 应被判空，得到 %q", bad, got)
			}
		}
	})
}

func TestAccessLogFieldsAndTokenRedaction(t *testing.T) {
	const secret = "ximo_sk_ffffffffffffffffffffffffffffffff"
	routes := []httpx.Route{{Pattern: "GET /v1/models", Handler: func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"ok": "1"})
	}}}
	srv, logs := newTestServer(t, routes, func(c *Config) {
		c.RequestTimeout = time.Second
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/models?api_key="+secret, nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.1")
	rec := do(srv, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，得到 %d", rec.Code)
	}

	fields := logs.find(t, "gateway: 请求完成")
	for k, want := range map[string]any{
		"method": "GET", "path": "/v1/models", "status": float64(200), "ip": "203.0.113.7",
	} {
		if fields[k] != want {
			t.Fatalf("日志字段 %s = %v，期望 %v", k, fields[k], want)
		}
	}
	if _, ok := fields["duration_ms"]; !ok {
		t.Fatal("日志缺少 duration_ms")
	}
	if fields["request_id"] == "" || fields["request_id"] == nil {
		t.Fatal("日志缺少 request_id")
	}
	if blob := logs.String(); strings.Contains(blob, secret) {
		t.Fatal("日志不得包含令牌明文（含查询串）")
	}
}

func TestLimitBodyRejectsOversizedRequest(t *testing.T) {
	routes := []httpx.Route{{Pattern: "POST /echo", Handler: func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			httpx.WriteError(w, r, http.StatusRequestEntityTooLarge, "payload_too_large", "body too large")
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]int{"len": len(body)})
	}}}
	srv, _ := newTestServer(t, routes, func(c *Config) { c.MaxBodyBytes = 16 })

	small := httptest.NewRequest(http.MethodPost, "/echo", strings.NewReader("hello"))
	if rec := do(srv, small); rec.Code != http.StatusOK || jsonBody(t, rec)["len"] != float64(5) {
		t.Fatalf("小请求应通过: %d %s", rec.Code, rec.Body.String())
	}

	big := httptest.NewRequest(http.MethodPost, "/echo", strings.NewReader(strings.Repeat("x", 1024)))
	if rec := do(srv, big); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超限请求应为 413，得到 %d", rec.Code)
	}
}

func TestTimeoutCancelsHandlerContext(t *testing.T) {
	sawCancel := make(chan bool, 1)
	routes := []httpx.Route{{Pattern: "GET /slow", Handler: func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			sawCancel <- true
			httpx.WriteError(w, r, http.StatusGatewayTimeout, "request_timeout", "request timed out")
		case <-time.After(3 * time.Second):
			sawCancel <- false
			httpx.WriteJSON(w, http.StatusOK, map[string]string{"late": "yes"})
		}
	}}}
	srv, _ := newTestServer(t, routes, func(c *Config) { c.RequestTimeout = 50 * time.Millisecond })

	rec := do(srv, httptest.NewRequest(http.MethodGet, "/slow", nil))
	if !<-sawCancel {
		t.Fatal("handler 未观察到 context 取消")
	}
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("期望 504，得到 %d", rec.Code)
	}
}

func TestSSEFlushAndLoggingPassThroughChain(t *testing.T) {
	routes := []httpx.Route{{Pattern: "POST /v1/chat/completions", Handler: func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for i := 0; i < 3; i++ {
			_, _ = w.Write([]byte("data: {\"chunk\":1}\n\n"))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}}}
	srv, logs := newTestServer(t, routes)
	rec := do(srv, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，得到 %d", rec.Code)
	}
	if !rec.Flushed {
		t.Fatal("Flush 必须透传到底层 writer，否则 SSE 会被缓冲住")
	}
	if !strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Fatalf("流式内容应完整写出: %s", rec.Body.String())
	}
	if fields := logs.find(t, "gateway: 请求完成"); fields["status"] != float64(200) {
		t.Fatalf("流式响应也要落访问日志，得到 %v", fields["status"])
	}
}

func TestChainOrder(t *testing.T) {
	var got []string
	mw := func(name string) func(http.Handler) http.Handler {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = append(got, "in:"+name)
				next.ServeHTTP(w, r)
				got = append(got, "out:"+name)
			})
		}
	}
	h := Chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { got = append(got, "handler") }),
		mw("a"), mw("b"))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	want := "in:a,in:b,handler,out:b,out:a"
	if strings.Join(got, ",") != want {
		t.Fatalf("中间件顺序错误:\n got %s\nwant %s", strings.Join(got, ","), want)
	}
}

// ---------------------------------------------------------------------------
// 健康端点、优雅停机
// ---------------------------------------------------------------------------

func TestHealthRouteIsUnauthenticatedButHasRequestID(t *testing.T) {
	routes := []httpx.Route{
		// 健康端点故意不套鉴权（契约 §11.3）。
		{Pattern: "GET /v1/health", Handler: func(w http.ResponseWriter, r *http.Request) {
			httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok", "id": httpx.RequestID(r)})
		}},
	}
	srv, _ := newTestServer(t, routes)
	rec := do(srv, httptest.NewRequest(http.MethodGet, "/v1/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("健康端点应可匿名访问，得到 %d", rec.Code)
	}
	if rec.Header().Get(headerRequestID) == "" {
		t.Fatal("健康端点也必须带 request id")
	}
	if jsonBody(t, rec)["id"] == "" {
		t.Fatal("健康端点的 handler 应能读到 request id")
	}
}

func TestGracefulShutdownWaitsForInflightRequest(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	routes := []httpx.Route{{Pattern: "GET /slow", Handler: func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"done": "yes"})
	}}}
	srv, _ := newTestServer(t, routes)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ctx, ln) }()

	type result struct {
		code int
		err  error
	}
	resCh := make(chan result, 1)
	go func() {
		resp, err := http.Get("http://" + addr + "/slow")
		if err != nil {
			resCh <- result{err: err}
			return
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		resCh <- result{code: resp.StatusCode}
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("请求未进入 handler")
	}

	cancel()       // 触发停机：listener 立即关闭，在途请求应被等待
	close(release) // 放行在途请求
	select {
	case res := <-resCh:
		if res.err != nil {
			t.Fatalf("在途请求应被优雅等待，得到错误: %v", res.err)
		}
		if res.code != http.StatusOK {
			t.Fatalf("在途请求应返回 200，得到 %d", res.code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("在途请求未在停机宽限期内完成")
	}

	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("优雅停机应返回 nil，得到 %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve 未返回")
	}

	// 停机后端口必须已释放，不能再建连。
	conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		t.Fatal("停机后不应再接受新连接")
	}
}

func TestRunFailsOnBadAddr(t *testing.T) {
	srv, _ := newTestServer(t, nil, func(c *Config) { c.Addr = "127.0.0.1:-1" })
	if err := srv.Run(context.Background()); err == nil {
		t.Fatal("非法监听地址应返回错误")
	}
}

// ---------------------------------------------------------------------------
// 限流
// ---------------------------------------------------------------------------

func TestKeyLimiterPerKey(t *testing.T) {
	l := NewKeyLimiter(2) // 每分钟 2 次：桶容量 2
	if !l.Enabled() {
		t.Fatal("perMin=2 应启用限流")
	}
	if !l.Allow("key:a") || !l.Allow("key:a") {
		t.Fatal("桶内前两次应放行")
	}
	if l.Allow("key:a") {
		t.Fatal("第三次应被拒")
	}
	if !l.Allow("key:b") {
		t.Fatal("不同 key 的桶互不影响")
	}
	if !l.Allow("") {
		t.Fatal("空键由调用方兜底（Allow 本身放行），这里锁住该约定")
	}
}

func TestKeyLimiterDisabled(t *testing.T) {
	l := NewKeyLimiter(0)
	if l.Enabled() {
		t.Fatal("perMin=0 应不限流")
	}
	for i := 0; i < 100; i++ {
		if !l.Allow("key:a") {
			t.Fatal("不限流时恒放行")
		}
	}
	// 透传：Middleware 不拦截
	h := l.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("不限流时应透传，得到 %d", rec.Code)
	}
}

func TestKeyLimiterMiddlewareExceedsAndFalls(t *testing.T) {
	l := NewKeyLimiter(1)
	user := Principal{Kind: PrincipalUser, User: model.User{ID: "u1"}, Key: model.APIKey{ID: "k1"}}
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	h := l.Middleware(inner)

	call := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req = req.WithContext(WithPrincipal(req.Context(), user))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := call(); rec.Code != http.StatusOK {
		t.Fatalf("首次应放行，得到 %d", rec.Code)
	}
	rec := call()
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("超限应为 429，得到 %d", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("429 应带 Retry-After")
	}

	// 未认证请求按 IP 分桶，独立于上面的 key 桶。
	anon := httptest.NewRecorder()
	h.ServeHTTP(anon, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if anon.Code != http.StatusOK {
		t.Fatalf("未认证请求应走 IP 桶，得到 %d", anon.Code)
	}
}

func TestKeyLimiterBucketCountIsBounded(t *testing.T) {
	l := NewKeyLimiter(60)
	l.maxKeys = 4
	l.idleTTL = time.Hour // 关掉空闲回收，强制走 LRU 路径
	for i := 0; i < 50; i++ {
		l.Allow(fmt.Sprintf("key:%d", i))
	}
	if n := l.Buckets(); n > 4 {
		t.Fatalf("桶数量应被限制在 4 以内，得到 %d", n)
	}
	if n := l.Buckets(); n == 0 {
		t.Fatal("不应把所有桶都清空")
	}
}
