package openai

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/catalog"
	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/httpx"
	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/model"
	"github.com/ximo888ok-netizen/ximo-agent/internal/observability"
	"github.com/ximo888ok-netizen/ximo-agent/internal/provider"
	"github.com/ximo888ok-netizen/ximo-agent/internal/quota"
)

const (
	testUserID = "u_test"
	testAPIKey = "sk-test-0123456789abcdefghij"
	testReqID  = "req_test_0001"
)

// TestMain 把全局日志重定向到 io.Discard：provider 层在失败路径上会走全局日志，
// 测试输出里不需要这些噪声（本包自己的日志已由 Deps.Logger 注入）。
func TestMain(m *testing.M) {
	observability.SetDefaultLogger(observability.NewLogger(io.Discard, observability.LevelError))
	m.Run()
}

// --- 额度服务替身 ---

// fakeQuota 复刻 *quota.Service 的对外口径：Reserve 返回 held 预占，SettleWithUsage
// 按 quota.CostMicro 计费并把 status 写成 quota.UsageStatusSettled，Release 归还。
type fakeQuota struct {
	mu sync.Mutex

	reserveErr    error
	reservations  []model.Reservation
	settled       []model.Reservation
	settleRecords []model.UsageRecord
	released      []model.Reservation

	// releaseCh 在每次 Release 时投递信号，便于测试等待异步收尾。
	releaseCh chan struct{}
	// settleCh 在每次结算时投递信号。
	settleCh chan struct{}
}

func newFakeQuota() *fakeQuota {
	return &fakeQuota{releaseCh: make(chan struct{}, 8), settleCh: make(chan struct{}, 8)}
}

func (f *fakeQuota) Reserve(_ context.Context, userID, requestID string, estimateMicro int64) (model.Reservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reserveErr != nil {
		return model.Reservation{}, f.reserveErr
	}
	r := model.Reservation{
		ID:        "rsv_" + requestID,
		UserID:    userID,
		RequestID: requestID,
		Status:    model.ReservationHeld,
		Amount:    estimateMicro,
		ExpiresAt: time.Now().Add(time.Minute).UnixMilli(),
	}
	f.reservations = append(f.reservations, r)
	return r, nil
}

func (f *fakeQuota) SettleWithUsage(_ context.Context, r model.Reservation, u model.UsageRecord, priceMicroPerKTok int64) (model.UsageRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if u.RequestID == "" {
		u.RequestID = r.RequestID
	}
	u.CostMicro = quota.CostMicro(u.InputTokens, u.OutputTokens, priceMicroPerKTok)
	u.Status = quota.UsageStatusSettled
	f.settled = append(f.settled, r)
	f.settleRecords = append(f.settleRecords, u)
	select {
	case f.settleCh <- struct{}{}:
	default:
	}
	return u, nil
}

func (f *fakeQuota) Release(_ context.Context, r model.Reservation, _ string) error {
	f.mu.Lock()
	f.released = append(f.released, r)
	f.mu.Unlock()
	select {
	case f.releaseCh <- struct{}{}:
	default:
	}
	return nil
}

func (f *fakeQuota) counts() (held, settled, released int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reservations), len(f.settled), len(f.released)
}

func (f *fakeQuota) settleRecord() (model.UsageRecord, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.settleRecords) == 0 {
		return model.UsageRecord{}, false
	}
	return f.settleRecords[len(f.settleRecords)-1], true
}

// --- 目录替身 ---

type fakeModels struct{ specs map[string]model.ModelSpec }

func (f *fakeModels) GetModel(_ context.Context, id string) (model.ModelSpec, error) {
	spec, ok := f.specs[id]
	if !ok {
		return model.ModelSpec{}, model.ErrNotFound
	}
	return spec, nil
}

type fakeCatalog struct {
	byModel map[string][]catalog.Candidate
}

func (f *fakeCatalog) Candidates(_ context.Context, modelID string) ([]catalog.Candidate, error) {
	return f.byModel[modelID], nil
}

// --- 用量替身（失败终态行）---

type fakeUsage struct {
	mu   sync.Mutex
	rows []model.UsageRecord
}

func (f *fakeUsage) InsertUsage(_ context.Context, u model.UsageRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows = append(f.rows, u)
	return nil
}

func (f *fakeUsage) statuses() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.rows))
	for _, r := range f.rows {
		out = append(out, r.Status)
	}
	return out
}

func (f *fakeUsage) last() (model.UsageRecord, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.rows) == 0 {
		return model.UsageRecord{}, false
	}
	return f.rows[len(f.rows)-1], true
}

// --- 上游替身 ---

// fastRetry 关掉 provider 内部重试（MaxAttempts=1）：候选切换的测试不必等退避，
// 也让「一次候选尝试 = 一次上游命中」的断言成立。
func fastRetry() provider.RetryPolicy {
	return provider.RetryPolicy{
		MaxAttempts:   1,
		BaseDelay:     time.Millisecond,
		MaxDelay:      2 * time.Millisecond,
		Multiplier:    1,
		MaxRetryAfter: time.Millisecond,
	}
}

// fakeUpstream 用**真实** provider.Client（指向假上游 httptest.Server）而不是手写
// stub：被验证的是网关 → provider.Client → HTTP 的完整链路，含 Authorization 注入、
// SSE 聚合与错误分类。
type fakeUpstream struct {
	mu        sync.Mutex
	endpoints map[string]string
	keys      map[string]string
	// defaultKey 未逐 provider 指定密钥时使用的默认密钥。
	defaultKey string
	buildErr   map[string]error
	timeout    time.Duration
}

func newFakeUpstream() *fakeUpstream {
	return &fakeUpstream{
		endpoints:  map[string]string{},
		keys:       map[string]string{},
		defaultKey: testAPIKey,
		buildErr:   map[string]error{},
		timeout:    3 * time.Second,
	}
}

func (f *fakeUpstream) Client(_ context.Context, providerID string) (*provider.Client, error) {
	f.mu.Lock()
	endpoint, ok := f.endpoints[providerID]
	buildErr := f.buildErr[providerID]
	key := f.keys[providerID]
	if key == "" {
		key = f.defaultKey
	}
	timeout := f.timeout
	f.mu.Unlock()

	if buildErr != nil {
		return nil, buildErr
	}
	if !ok {
		return nil, fmt.Errorf("upstream: 未配置 provider %s", providerID)
	}
	return provider.NewClient(provider.ClientOptions{
		Config: provider.ProviderConfig{
			ID:              providerID,
			Name:            providerID,
			BaseURL:         endpoint,
			MaxOutputTokens: 8192,
			Capabilities:    provider.DefaultCapabilities(),
			SecretRef:       "secretref:" + providerID,
		},
		Secrets: provider.SecretResolverFunc(func(context.Context, string) (string, error) {
			return key, nil
		}),
		Retry:          fastRetry(),
		DefaultTimeout: timeout,
	})
}

// --- 假上游 HTTP server ---

// upstreamRecorder 记录假上游收到的请求（用于断言「有没有打到上游」「带没带密钥」）。
type upstreamRecorder struct {
	mu   sync.Mutex
	hits int
	auth string
	path string
	body string
}

func (r *upstreamRecorder) record(req *http.Request, body []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hits++
	r.auth = req.Header.Get("Authorization")
	r.path = req.URL.Path
	r.body = string(body)
}

func (r *upstreamRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hits
}

func (r *upstreamRecorder) authorization() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.auth
}

// startUpstream 启动一个假上游：status != 200 时回 JSON 错误，否则按 events 逐段
// 写 SSE 并 flush。
func startUpstream(t *testing.T, status int, events []string) (*httptest.Server, *upstreamRecorder) {
	t.Helper()
	rec := &upstreamRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec.record(r, body)
		if status != http.StatusOK {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"error":{"message":"upstream failure","type":"server_error"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, ev := range events {
			_, _ = io.WriteString(w, ev)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// startHangingUpstream 写出一段 SSE 后挂住，直到上游请求被取消（客户端断开）——
// 用于验证「客户端取消 → 归还预占」。
func startHangingUpstream(t *testing.T, events []string) (*httptest.Server, *upstreamRecorder) {
	t.Helper()
	rec := &upstreamRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec.record(r, body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, ev := range events {
			_, _ = io.WriteString(w, ev)
			if flusher != nil {
				flusher.Flush()
			}
		}
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// --- 测试环境 ---

type env struct {
	mux    *http.ServeMux
	quota  *fakeQuota
	models *fakeModels
	cat    *fakeCatalog
	up     *fakeUpstream
	usage  *fakeUsage

	authUser model.User
	authErr  error
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{
		quota:  newFakeQuota(),
		models: &fakeModels{specs: map[string]model.ModelSpec{}},
		cat:    &fakeCatalog{byModel: map[string][]catalog.Candidate{}},
		up:     newFakeUpstream(),
		usage:  &fakeUsage{},
		authUser: model.User{
			ID:       testUserID,
			Username: "tester",
			Status:   model.UserStatusActive,
		},
	}
	mux := http.NewServeMux()
	for _, rt := range Routes(Deps{
		Auth:     e.auth,
		Quota:    e.quota,
		Models:   e.models,
		Catalog:  e.cat,
		Upstream: e.up,
		Usage:    e.usage,
		Logger:   observability.NewLogger(io.Discard, observability.LevelError),
	}) {
		mux.HandleFunc(rt.Pattern, rt.Handler)
	}
	e.mux = mux
	return e
}

func (e *env) auth(*http.Request) (model.User, error) {
	if e.authErr != nil {
		return model.User{}, e.authErr
	}
	return e.authUser, nil
}

// allowModel 注册模型 + 候选（按传入顺序，即优先级升序）。
func (e *env) allowModel(t *testing.T, modelID string, providerIDs ...string) {
	t.Helper()
	e.models.specs[modelID] = model.ModelSpec{ModelID: modelID, DisplayName: modelID, Enabled: true}
	cands := make([]catalog.Candidate, 0, len(providerIDs))
	for _, id := range providerIDs {
		cands = append(cands, catalog.Candidate{
			Provider: model.ProviderSpec{
				ID:       id,
				Name:     id,
				Protocol: model.ProtocolOpenAIChat,
				Status:   model.ProviderStatusEnabled,
			},
			UpstreamModel: "upstream-" + id,
		})
	}
	e.cat.byModel[modelID] = cands
}

// post 直接调用处理器（带 request_id，模拟中间件注入）。
func (e *env) post(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req = req.WithContext(httpx.WithRequestID(req.Context(), testReqID))
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec
}

// sseData 组装一条 SSE data 行。
func sseData(payload string) string { return "data: " + payload + "\n\n" }

// waitFor 轮询等待异步收尾（处理器在另一个 goroutine 里跑时的断言用）。
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待超时：%s", what)
}
