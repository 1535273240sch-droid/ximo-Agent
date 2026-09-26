package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestClient 起一个假的 mem0 服务并返回客户端。
//
// 假服务断言的是**线上契约**：路径、方法、鉴权头、请求体字段名。契约来自
// mem0 仓库的 server/main.py，一旦上游改形状，这里的失败就是最早的一声警报。
func newTestClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	client := NewClient(Config{
		Enabled:  true,
		Endpoint: srv.URL,
		UserID:   "user-1",
		AgentID:  "agent-1",
		Timeout:  2 * time.Second,
	}, ClientOptions{
		HTTPClient: srv.Client(),
		APIKey:     func(context.Context) (string, error) { return "secret-key", nil },
	})
	return client, srv
}

func decodeBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Fatalf("请求体不是合法 JSON: %v", err)
	}
	return body
}

func TestClientAddSendsMem0Contract(t *testing.T) {
	var gotPath, gotMethod, gotAPIKey, gotAuth string
	var gotBody map[string]any
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		gotAPIKey = r.Header.Get("X-API-Key")
		gotAuth = r.Header.Get("Authorization")
		gotBody = decodeBody(t, r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"id":"m1","memory":"用户偏好暗色主题","event":"ADD"}]}`))
	})

	recs, err := client.Add(context.Background(), []Message{
		{Role: "user", Content: "以后都用暗色主题"},
		{Role: "assistant", Content: "好的"},
	}, AddOptions{RunID: "run-1", Metadata: map[string]any{"session_id": "s1"}})
	if err != nil {
		t.Fatalf("Add 失败: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/memories" {
		t.Fatalf("期望 POST /memories，实际 %s %s", gotMethod, gotPath)
	}
	if gotAPIKey != "secret-key" {
		t.Fatalf("期望 X-API-Key 头，实际 %q", gotAPIKey)
	}
	// mem0 的 verify_auth 一旦看到 Authorization: Bearer 就按 JWT 解析且不回退，
	// 所以这里必须断言「没有」这个头，防止后来者顺手加上去。
	if gotAuth != "" {
		t.Fatalf("不应发送 Authorization 头（会被当作 JWT 解析），实际 %q", gotAuth)
	}
	if gotBody["user_id"] != "user-1" || gotBody["agent_id"] != "agent-1" || gotBody["run_id"] != "run-1" {
		t.Fatalf("归属字段不符: %v", gotBody)
	}
	msgs, ok := gotBody["messages"].([]any)
	if !ok || len(msgs) != 2 {
		t.Fatalf("messages 不符: %v", gotBody["messages"])
	}
	meta, ok := gotBody["metadata"].(map[string]any)
	if !ok || meta["session_id"] != "s1" {
		t.Fatalf("metadata 不符: %v", gotBody["metadata"])
	}
	if len(recs) != 1 || recs[0].ID != "m1" || recs[0].Memory == "" {
		t.Fatalf("响应解析不符: %+v", recs)
	}
}

func TestClientSearchSendsFiltersAndParsesScore(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody = decodeBody(t, r)
		_, _ = w.Write([]byte(`{"results":[{"id":"m2","memory":"部署走 7897 代理","score":0.87}]}`))
	})

	recs, err := client.Search(context.Background(), "代理怎么走", SearchOptions{TopK: 3})
	if err != nil {
		t.Fatalf("Search 失败: %v", err)
	}
	if gotPath != "/search" {
		t.Fatalf("期望 /search，实际 %s", gotPath)
	}
	if gotBody["query"] != "代理怎么走" {
		t.Fatalf("query 不符: %v", gotBody)
	}
	if topK, _ := gotBody["top_k"].(float64); int(topK) != 3 {
		t.Fatalf("top_k 不符: %v", gotBody["top_k"])
	}
	filters, ok := gotBody["filters"].(map[string]any)
	if !ok || filters["user_id"] != "user-1" {
		t.Fatalf("filters 不符: %v", gotBody["filters"])
	}
	if len(recs) != 1 || recs[0].Score != 0.87 {
		t.Fatalf("响应解析不符: %+v", recs)
	}
}

func TestClientGetAllAndDeleteUseExpectedRoutes(t *testing.T) {
	var paths []string
	var query string
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		// EscapedPath 而不是 Path：net/url 会把 Path 解码，"id%20with%20space"
		// 在看 Path 时变成 "id with space"，那样的断言等于没验证转义。
		paths = append(paths, r.Method+" "+r.URL.EscapedPath())
		// 只记 GET /memories 的查询串：后面的 DELETE 没有查询串，如果每次都赋值，
		// 最后一次会把要断言的内容清空（这个 bug 第一版就有）。
		if r.URL.Path == "/memories" {
			query = r.URL.RawQuery
		}
		_, _ = w.Write([]byte(`{"results":[]}`))
	})

	if _, err := client.GetAll(context.Background(), 7); err != nil {
		t.Fatalf("GetAll 失败: %v", err)
	}
	if err := client.Delete(context.Background(), "id with space"); err != nil {
		t.Fatalf("Delete 失败: %v", err)
	}
	if len(paths) != 2 || paths[0] != "GET /memories" || paths[1] != "DELETE /memories/id%20with%20space" {
		t.Fatalf("路由不符: %v", paths)
	}
	if !strings.Contains(query, "user_id=user-1") || !strings.Contains(query, "top_k=7") {
		t.Fatalf("查询串不符: %q", query)
	}
}

func TestClientErrorIsReportedAndKeyIsRedacted(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		// 故意把密钥回显在响应体里：错误信息里也绝不能带出明文。
		_, _ = w.Write([]byte("boom secret-key"))
	})

	_, err := client.Search(context.Background(), "q", SearchOptions{})
	if err == nil {
		t.Fatal("期望 500 被报成错误")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Fatalf("错误信息应含状态码: %v", err)
	}
	if strings.Contains(err.Error(), "secret-key") {
		t.Fatalf("错误信息泄漏了密钥: %v", err)
	}
	if last, at := client.LastError(); last == "" || at.IsZero() {
		t.Fatal("失败应记录到 LastError")
	}
}

func TestClientTimesOutWithoutHanging(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_, _ = w.Write([]byte(`{"results":[]}`))
	})
	// 把超时压到远小于服务端延迟，验证 context 超时生效（而不是靠客户端挂死）。
	client.cfg.Timeout = 50 * time.Millisecond

	_, err := client.Search(context.Background(), "q", SearchOptions{})
	if err == nil {
		t.Fatal("期望超时被报成错误")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("期望超时类错误，实际: %v", err)
	}
}

func TestClientDisabledMakesNoRequest(t *testing.T) {
	called := false
	client := NewClient(Config{Enabled: false, Endpoint: "http://127.0.0.1:1"}, ClientOptions{
		HTTPClient: &http.Client{Timeout: time.Second},
	})
	client.http = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		called = true
		return nil, fmt.Errorf("should not be called")
	})}

	if _, err := client.Search(context.Background(), "q", SearchOptions{}); !errors.Is(err, ErrDisabled) {
		t.Fatalf("期望 ErrDisabled，实际 %v", err)
	}
	if called {
		t.Fatal("未启用时不应发出任何请求")
	}
}

func TestClientMissingKeyIsAnExplicitError(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"results":[]}`))
	})
	client.apiKey = func(context.Context) (string, error) {
		return "", errors.New("密钥库里没有这条 ref")
	}

	_, err := client.Search(context.Background(), "q", SearchOptions{})
	if err == nil {
		t.Fatal("期望读不到密钥时报错，而不是带着空鉴权头去请求")
	}
	if !strings.Contains(err.Error(), "密钥") {
		t.Fatalf("错误信息应说明是密钥问题: %v", err)
	}
}

// roundTripFunc 让测试可以注入一个不存在的传输层。
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
