package ui

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/1535273240sch-droid/ximo-plugin/internal/gateway"
	"github.com/1535273240sch-droid/ximo-plugin/internal/spec"
)

// fakeBackend 记录被调用的次数，用来断言「handler 层就把不该执行的动作拦住了」。
type fakeBackend struct {
	state      State
	adapters   []Adapter
	loginStart LoginStart
	loginPoll  LoginPoll
	models     []gateway.Model
	plan       Plan
	applyItems []ApplyItem
	doctor     []DoctorItem
	specs      []spec.Spec
	err        error

	applyCalls int
	lastReq    PlanRequest
}

func (f *fakeBackend) State(context.Context) (State, error)            { return f.state, f.err }
func (f *fakeBackend) Detect(context.Context) ([]Adapter, error)       { return f.adapters, f.err }
func (f *fakeBackend) LoginStart(context.Context) (LoginStart, error)  { return f.loginStart, f.err }
func (f *fakeBackend) LoginPoll(context.Context) (LoginPoll, error)    { return f.loginPoll, f.err }
func (f *fakeBackend) Models(context.Context) ([]gateway.Model, error) { return f.models, f.err }
func (f *fakeBackend) Plan(context.Context, PlanRequest) (Plan, error) { return f.plan, f.err }
func (f *fakeBackend) Doctor(context.Context) ([]DoctorItem, error)    { return f.doctor, f.err }
func (f *fakeBackend) Specs(context.Context) ([]spec.Spec, error)      { return f.specs, f.err }

func (f *fakeBackend) Apply(_ context.Context, req PlanRequest) ([]ApplyItem, error) {
	f.applyCalls++
	f.lastReq = req
	return f.applyItems, f.err
}

func newTestServer(t *testing.T, be Backend) *Server {
	t.Helper()
	srv, err := New(Config{Port: 0, Backend: be})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv
}

// api 用给定令牌打一次接口。
func api(t *testing.T, srv *Server, method, path, token, body string) (*http.Response, map[string]any) {
	t.Helper()
	var rdr *strings.Reader
	if body == "" {
		rdr = strings.NewReader("")
	} else {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	if token != "" {
		req.Header.Set(TokenHeader, token)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	resp := rec.Result()
	var payload map[string]any
	raw := rec.Body.Bytes()
	if len(raw) > 0 && strings.Contains(resp.Header.Get("Content-Type"), "json") {
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatalf("%s %s：响应不是合法 JSON: %v (%s)", method, path, err, raw)
		}
	}
	return resp, payload
}

// —— 契约 §2.2：所有 /api/* 都要带正确令牌 ——

func TestAPITokenGate(t *testing.T) {
	srv := newTestServer(t, &fakeBackend{state: State{Version: "test-1"}})

	cases := []struct {
		name   string
		token  string
		status int
	}{
		{"无令牌", "", http.StatusUnauthorized},
		{"错误令牌", strings.Repeat("f", 32), http.StatusUnauthorized},
		{"长度正确但不同", strings.Repeat("a", 32), http.StatusUnauthorized},
		{"正确令牌", srv.Token(), http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, payload := api(t, srv, "GET", "/api/state", c.token, "")
			if resp.StatusCode != c.status {
				t.Fatalf("状态码 %d，想要 %d", resp.StatusCode, c.status)
			}
			if c.status == http.StatusOK {
				if payload["ok"] != true {
					t.Fatalf("ok=%v，想要 true", payload["ok"])
				}
				return
			}
			if payload["ok"] != false {
				t.Fatalf("401 时 ok 应为 false，得到 %v", payload["ok"])
			}
			e, _ := payload["error"].(map[string]any)
			if e["code"] != "unauthorized" {
				t.Fatalf("error.code=%v，想要 unauthorized", e["code"])
			}
			if msg, _ := e["message"].(string); !strings.Contains(msg, TokenHeader) {
				t.Fatalf("错误消息应提示带 %s：%v", TokenHeader, msg)
			}
		})
	}
}

// 每个 /api/* 都必须走令牌闸门，不能有漏网的端点。
func TestAllAPIEndpointsRequireToken(t *testing.T) {
	srv := newTestServer(t, &fakeBackend{})
	for _, ep := range []struct{ method, path string }{
		{"GET", "/api/state"},
		{"POST", "/api/detect"},
		{"POST", "/api/login/start"},
		{"POST", "/api/login/poll"},
		{"GET", "/api/models"},
		{"POST", "/api/plan"},
		{"POST", "/api/apply"},
		{"GET", "/api/doctor"},
		{"GET", "/api/specs"},
	} {
		resp, _ := api(t, srv, ep.method, ep.path, "", "{}")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s：无令牌时状态码 %d，想要 401", ep.method, ep.path, resp.StatusCode)
		}
	}
}

// 令牌不能出现在任何响应体里（它只该出现在地址栏/请求头）。
func TestTokenNeverInResponseBody(t *testing.T) {
	srv := newTestServer(t, &fakeBackend{state: State{Version: "test-1", CredMasked: "ximo****"}})
	for _, p := range []string{"/api/state", "/api/detect", "/api/specs", "/api/doctor"} {
		_, payload := api(t, srv, "GET", p, srv.Token(), "")
		raw, _ := json.Marshal(payload)
		if strings.Contains(string(raw), srv.Token()) {
			t.Errorf("%s 的响应体里出现了会话令牌", p)
		}
	}
	if strings.Contains(srv.BaseURL(), srv.Token()) {
		t.Errorf("BaseURL 不应含令牌: %s", srv.BaseURL())
	}
	if !strings.Contains(srv.URL(), "?t="+srv.Token()) {
		t.Errorf("URL 应带 ?t=<token>: %s", srv.URL())
	}
}

// —— 契约 §2.1：令牌形状 ——

func TestTokenShapeAndRandomness(t *testing.T) {
	a := newTestServer(t, &fakeBackend{})
	b := newTestServer(t, &fakeBackend{})
	for _, s := range []*Server{a, b} {
		if len(s.Token()) != 2*TokenBytes {
			t.Fatalf("令牌长度 %d，想要 %d", len(s.Token()), 2*TokenBytes)
		}
		if _, err := hex.DecodeString(s.Token()); err != nil {
			t.Fatalf("令牌不是十六进制: %q", s.Token())
		}
	}
	if a.Token() == b.Token() {
		t.Fatal("两次启动的令牌相同：必须每次用 crypto/rand 重新生成")
	}
	if !strings.HasPrefix(a.BaseURL(), "http://127.0.0.1:") {
		t.Fatalf("只应绑回环地址：%s", a.BaseURL())
	}
}

// —— 契约 §3：信封形状 ——

func TestEnvelopeShapes(t *testing.T) {
	be := &fakeBackend{
		state:      State{Version: "1.2.3", Home: `C:\home`, IsolatedHome: true, Gateway: "http://gw:1", LoggedIn: true, CredMasked: "ximo****", Adapters: []Adapter{{ID: "claude-code", Name: "Claude Code", Detected: true}}},
		adapters:   []Adapter{{ID: "codex-cli", Name: "Codex CLI"}},
		loginStart: LoginStart{UserCode: "ABCD-1234", ExpiresIn: 600},
		loginPoll:  LoginPoll{Status: "pending"},
		models:     []gateway.Model{{ID: "m1", DisplayName: "模型一", Provider: "p", Protocols: []string{"openai-chat"}}},
		plan:       Plan{Items: []PlanItem{{SpecID: "claude-code", Kind: "file", Target: "/t.json", Summary: "设置 2 个字段"}}, Diff: "--- a\n+++ b\n"},
		applyItems: []ApplyItem{{SpecID: "claude-code", Target: "/t.json", Result: "ok", Message: "已写入"}},
		doctor:     []DoctorItem{{Name: "适配器规格", Status: "pass", Detail: "3 个"}},
		specs:      []spec.Spec{{ID: "claude-code", Name: "Claude Code"}},
	}
	srv := newTestServer(t, be)

	t.Run("state", func(t *testing.T) {
		_, p := api(t, srv, "GET", "/api/state", srv.Token(), "")
		d := p["data"].(map[string]any)
		for _, k := range []string{"version", "home", "isolated_home", "gateway", "logged_in", "cred_masked", "adapters"} {
			if _, ok := d[k]; !ok {
				t.Errorf("state 缺字段 %q", k)
			}
		}
		ad := d["adapters"].([]any)[0].(map[string]any)
		for _, k := range []string{"id", "name", "detected", "evidence"} {
			if _, ok := ad[k]; !ok {
				t.Errorf("adapter 缺字段 %q", k)
			}
		}
	})

	t.Run("detect", func(t *testing.T) {
		_, p := api(t, srv, "POST", "/api/detect", srv.Token(), "")
		list, ok := p["data"].([]any)
		if !ok || len(list) != 1 {
			t.Fatalf("detect 的 data 应是适配器数组: %v", p["data"])
		}
	})

	t.Run("login", func(t *testing.T) {
		_, p := api(t, srv, "POST", "/api/login/start", srv.Token(), "")
		d := p["data"].(map[string]any)
		if d["user_code"] != "ABCD-1234" || d["expires_in"] != float64(600) {
			t.Errorf("login/start 载荷不符: %v", d)
		}
		// device_code 等价于短期凭据，绝不能出现在响应里。
		if _, leaked := d["device_code"]; leaked {
			t.Error("login/start 不应回传 device_code")
		}
		_, p = api(t, srv, "POST", "/api/login/poll", srv.Token(), "")
		if d := p["data"].(map[string]any); d["status"] != "pending" {
			t.Errorf("login/poll 载荷不符: %v", d)
		}
	})

	t.Run("models", func(t *testing.T) {
		_, p := api(t, srv, "GET", "/api/models", srv.Token(), "")
		d := p["data"].(map[string]any)
		list, ok := d["data"].([]any)
		if !ok || len(list) != 1 {
			t.Fatalf("models 载荷应是 {data:[...]}: %v", p["data"])
		}
	})

	t.Run("plan", func(t *testing.T) {
		_, p := api(t, srv, "POST", "/api/plan", srv.Token(), `{"spec_id":"claude-code"}`)
		d := p["data"].(map[string]any)
		items, ok := d["items"].([]any)
		if !ok || len(items) != 1 {
			t.Fatalf("plan 载荷缺 items: %v", p["data"])
		}
		it := items[0].(map[string]any)
		for _, k := range []string{"spec_id", "kind", "target", "summary"} {
			if _, ok := it[k]; !ok {
				t.Errorf("plan item 缺字段 %q", k)
			}
		}
		if _, ok := d["diff"]; !ok {
			t.Error("plan 载荷缺 diff")
		}
	})

	t.Run("doctor", func(t *testing.T) {
		_, p := api(t, srv, "GET", "/api/doctor", srv.Token(), "")
		it := p["data"].([]any)[0].(map[string]any)
		if it["status"] != "pass" {
			t.Errorf("doctor 的 status 应用小写: %v", it["status"])
		}
	})

	t.Run("specs", func(t *testing.T) {
		_, p := api(t, srv, "GET", "/api/specs", srv.Token(), "")
		if d, ok := p["data"].([]any); !ok || len(d) != 1 {
			t.Fatalf("specs 的 data 应是规格数组: %v", p["data"])
		}
	})

	t.Run("未知接口", func(t *testing.T) {
		resp, p := api(t, srv, "GET", "/api/nope", srv.Token(), "")
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("未知接口状态码 %d，想要 404", resp.StatusCode)
		}
		if e, _ := p["error"].(map[string]any); e["code"] != "not_found" {
			t.Fatalf("未知接口应回 not_found: %v", p)
		}
	})

	t.Run("方法不符", func(t *testing.T) {
		resp, _ := api(t, srv, "GET", "/api/apply", srv.Token(), "")
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("GET /api/apply 状态码 %d，想要 405", resp.StatusCode)
		}
	})
}

// 后端错误：CodeError 用它的码与状态，其余按 500。
func TestBackendErrorMapping(t *testing.T) {
	be := &fakeBackend{err: Errorf("no_credential", "%s", "没有凭据")}
	srv := newTestServer(t, be)
	resp, p := api(t, srv, "GET", "/api/models", srv.Token(), "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("状态码 %d，想要 400", resp.StatusCode)
	}
	if e, _ := p["error"].(map[string]any); e["code"] != "no_credential" {
		t.Fatalf("错误码不符: %v", p)
	}

	be2 := &fakeBackend{err: fmt.Errorf("内部炸了")}
	srv2 := newTestServer(t, be2)
	resp, p = api(t, srv2, "GET", "/api/models", srv2.Token(), "")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("普通 error 状态码 %d，想要 500", resp.StatusCode)
	}
	if e, _ := p["error"].(map[string]any); e["code"] != "internal" {
		t.Fatalf("普通 error 的错误码应是 internal: %v", p)
	}
}

// —— 契约 §3：apply 必须带 confirm:true ——

func TestApplyRequiresConfirm(t *testing.T) {
	be := &fakeBackend{applyItems: []ApplyItem{{SpecID: "claude-code", Target: "/t.json", Result: "ok", Message: "已写入"}}}
	srv := newTestServer(t, be)

	cases := []struct {
		name string
		body string
	}{
		{"没有 confirm 字段", `{"spec_id":"claude-code"}`},
		{"confirm 为 false", `{"spec_id":"claude-code","confirm":false}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, p := api(t, srv, "POST", "/api/apply", srv.Token(), c.body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("状态码 %d，想要 400", resp.StatusCode)
			}
			if e, _ := p["error"].(map[string]any); e["code"] != "confirm_required" {
				t.Fatalf("错误码应为 confirm_required: %v", p)
			}
			if be.applyCalls != 0 {
				t.Fatalf("未确认时不应调用后端 Apply（实际调用 %d 次）", be.applyCalls)
			}
		})
	}

	t.Run("confirm 为 true", func(t *testing.T) {
		resp, p := api(t, srv, "POST", "/api/apply", srv.Token(), `{"spec_id":"claude-code","model":"m1","confirm":true}`)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("状态码 %d，想要 200（%v）", resp.StatusCode, p)
		}
		if be.applyCalls != 1 {
			t.Fatalf("Apply 调用次数 %d，想要 1", be.applyCalls)
		}
		if !be.lastReq.Confirm || be.lastReq.Model != "m1" {
			t.Fatalf("后端拿到的入参不对: %+v", be.lastReq)
		}
		it := p["data"].([]any)[0].(map[string]any)
		if it["result"] != "ok" || it["target"] != "/t.json" {
			t.Fatalf("apply 结果形状不符: %v", it)
		}
	})

	t.Run("缺 spec_id", func(t *testing.T) {
		resp, _ := api(t, srv, "POST", "/api/apply", srv.Token(), `{"confirm":true}`)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("状态码 %d，想要 400", resp.StatusCode)
		}
	})

	t.Run("非法 JSON", func(t *testing.T) {
		resp, _ := api(t, srv, "POST", "/api/plan", srv.Token(), `{nope`)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("状态码 %d，想要 400", resp.StatusCode)
		}
	})
}

// —— 契约 §1：内嵌静态资源 ——

func TestEmbeddedAssets(t *testing.T) {
	srv := newTestServer(t, &fakeBackend{})

	t.Run("index.html", func(t *testing.T) {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET / 状态码 %d，想要 200", rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
			t.Fatalf("GET / 的 Content-Type=%q，想要 text/html", ct)
		}
		body := rec.Body.String()
		if !strings.Contains(body, "<html") {
			t.Fatalf("GET / 的正文不像 HTML: %.120q", body)
		}
		// 样式标记：正式的玻璃/羊皮卷界面一定带样式表或内联样式。
		if !strings.Contains(body, "stylesheet") && !strings.Contains(body, "<style") {
			t.Fatalf("GET / 的正文里找不到样式标记: %.200q", body)
		}
	})

	t.Run("assets", func(t *testing.T) {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/assets/app.css", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /assets/app.css 状态码 %d，想要 200", rec.Code)
		}
		if rec.Body.Len() == 0 {
			t.Fatal("GET /assets/app.css 正文为空")
		}
	})

	t.Run("静态资源不需要令牌", func(t *testing.T) {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/assets/app.js", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /assets/app.js 状态码 %d，想要 200（页面子资源带不上令牌）", rec.Code)
		}
	})

	t.Run("不存在的资源 404", func(t *testing.T) {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/assets/nope.css", nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("状态码 %d，想要 404", rec.Code)
		}
	})
}

// —— 契约 §1：端口被占用要报错，不静默换端口 ——

func TestListenPortInUse(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("占位监听失败: %v", err)
	}
	defer func() { _ = ln.Close() }()
	port := ln.Addr().(*net.TCPAddr).Port

	srv, err := New(Config{Port: port, Backend: &fakeBackend{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = srv.Listen()
	if err == nil {
		t.Fatalf("端口 %d 已被占用，Listen 应报错而不是静默换端口", port)
	}
	for _, want := range []string{strconv.Itoa(port), "--port"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息里应含 %q：%v", want, err)
		}
	}
	if got, want := srv.URL(), fmt.Sprintf("http://127.0.0.1:%d/?t=%s", port, srv.Token()); got != want {
		t.Errorf("报错后地址不应变成别的端口：%s", got)
	}
}

// 真起一个回环 socket 跑完整链路（不碰浏览器）。
func TestServeOverLoopback(t *testing.T) {
	be := &fakeBackend{state: State{Version: "test-1", Home: "h"}}
	srv := newTestServer(t, be) // Port=0：随机空闲端口
	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()

	var lastErr error
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(srv.BaseURL())
		if err == nil {
			_ = resp.Body.Close()
			lastErr = nil
			break
		}
		lastErr = err
		time.Sleep(20 * time.Millisecond)
	}
	if lastErr != nil {
		t.Fatalf("服务未就绪: %v", lastErr)
	}

	req, _ := http.NewRequest("GET", srv.BaseURL()+"api/state", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("无令牌请求失败: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无令牌状态码 %d，想要 401", resp.StatusCode)
	}

	req, _ = http.NewRequest("GET", srv.BaseURL()+"api/state", nil)
	req.Header.Set(TokenHeader, srv.Token())
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("带令牌请求失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("带令牌状态码 %d，想要 200", resp.StatusCode)
	}
	var p map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if p["ok"] != true {
		t.Fatalf("ok=%v，想要 true", p["ok"])
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve 返回错误: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ctx 取消后 Serve 未退出")
	}
}

func TestNewRejectsBadConfig(t *testing.T) {
	if _, err := New(Config{Port: 0}); err == nil {
		t.Error("缺少 Backend 时应报错")
	}
	if _, err := New(Config{Port: 70000, Backend: &fakeBackend{}}); err == nil {
		t.Error("端口越界时应报错")
	}
}
