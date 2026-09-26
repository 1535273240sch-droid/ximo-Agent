package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/model"
	"github.com/ximo888ok-netizen/ximo-agent/internal/observability"
	"github.com/ximo888ok-netizen/ximo-agent/internal/provider"
)

// testAPIKey 假上游密钥。真实密钥只在单次 Authorization 头里出现，测试用它验证注入。
const testAPIKey = "sk-test-0123456789abcdefghij"

// sseOKBody 最小 OpenAI 兼容流式响应：provider.Client 内部按 SSE 聚合。
const sseOKBody = "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\ndata: [DONE]\n\n"

// --- 假目录 ---

type fakeStore struct {
	mu    sync.Mutex
	specs map[string]model.ProviderSpec
	reads int
}

func newFakeStore(specs ...model.ProviderSpec) *fakeStore {
	m := make(map[string]model.ProviderSpec, len(specs))
	for _, s := range specs {
		m[s.ID] = s
	}
	return &fakeStore{specs: m}
}

func (f *fakeStore) GetProvider(_ context.Context, id string) (model.ProviderSpec, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	spec, ok := f.specs[id]
	if !ok {
		return model.ProviderSpec{}, model.ErrNotFound
	}
	return spec, nil
}

func (f *fakeStore) readCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

// --- 假上游（真实 HTTP server，走完整网络栈）---

type fakeUpstream struct {
	srv  *httptest.Server
	hits atomic.Int64
	// fail 为 true 时返回 500（可重试类别，会推进熔断计数）。
	fail atomic.Bool

	mu       sync.Mutex
	lastAuth string
	lastPath string
	lastBody string
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		body, _ := io.ReadAll(r.Body)

		f.mu.Lock()
		f.lastAuth = r.Header.Get("Authorization")
		f.lastPath = r.URL.Path
		f.lastBody = string(body)
		f.mu.Unlock()

		if f.fail.Load() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":{"message":"upstream boom"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, sseOKBody)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

type requestSnapshot struct {
	auth string
	path string
	body string
}

func (f *fakeUpstream) lastRequest() requestSnapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return requestSnapshot{auth: f.lastAuth, path: f.lastPath, body: f.lastBody}
}

// --- 假密钥解析器 ---

type recordingResolver struct {
	mu   sync.Mutex
	refs []string
	key  string
	err  error
}

func (r *recordingResolver) Resolve(_ context.Context, ref string) (string, error) {
	r.mu.Lock()
	r.refs = append(r.refs, ref)
	r.mu.Unlock()
	if r.err != nil {
		return "", r.err
	}
	return r.key, nil
}

func (r *recordingResolver) resolvedRefs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.refs...)
}

// --- 公共装配 ---

const testKeyRef = "secretref:v1:00000000000000000000000000000000"

func testSpec(id, endpoint string) model.ProviderSpec {
	return model.ProviderSpec{
		ID:        id,
		Name:      id,
		Endpoint:  endpoint,
		Protocol:  model.ProtocolOpenAIChat,
		Status:    model.ProviderStatusEnabled,
		APIKeyRef: testKeyRef,
	}
}

type poolOption func(*Options)

func withBreaker(factory func(providerID string) *provider.CircuitBreaker) poolOption {
	return func(o *Options) { o.BreakerFactory = factory }
}

func fastBreaker(threshold int, openTimeout time.Duration) func(string) *provider.CircuitBreaker {
	return func(string) *provider.CircuitBreaker {
		b := provider.NewCircuitBreaker(threshold, openTimeout)
		// half-open 下一次成功即闭合，测试里不必连打两次探测。
		b.SuccessThreshold = 1
		return b
	}
}

func newTestPool(t *testing.T, store providerSource, secrets SecretResolver, opts ...poolOption) *Pool {
	t.Helper()
	o := Options{
		// 测试里不重试：避免退避等待把用例拖慢，也让「一次 Complete = 一次上游请求」成立。
		Retry:          provider.RetryPolicy{MaxAttempts: 1},
		DefaultTimeout: 5 * time.Second,
		Logger:         observability.NewLogger(io.Discard, observability.LevelError),
	}
	for _, fn := range opts {
		fn(&o)
	}
	return NewWithOptions(store, secrets, o)
}

func complete(t *testing.T, client *provider.Client) (provider.CompletionResponse, error) {
	t.Helper()
	return client.Complete(context.Background(), provider.CompletionRequest{
		Model:    "test-model",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})
}

func completeMessages(t *testing.T, client *provider.Client, messages []provider.Message) (provider.CompletionResponse, error) {
	t.Helper()
	return client.Complete(context.Background(), provider.CompletionRequest{
		Model:    "test-model",
		Messages: messages,
	})
}

// --- 正常路径 ---

// TestClientCompletesAgainstFakeUpstream 覆盖正常路径与请求装配：
// 密钥经引用解析后注入 Authorization 头、路径为 /chat/completions、
// 请求带 include_usage（网关结算依赖上游返回真实 usage）。
func TestClientCompletesAgainstFakeUpstream(t *testing.T) {
	up := newFakeUpstream(t)
	spec := testSpec("openai-main", up.srv.URL)
	store := newFakeStore(spec)
	resolver := &recordingResolver{key: testAPIKey}
	pool := newTestPool(t, store, resolver)

	client, err := pool.Client(context.Background(), "openai-main")
	if err != nil {
		t.Fatalf("Client 报错: %v", err)
	}
	if client.Name() != "openai-main" {
		t.Fatalf("Name = %q, want openai-main", client.Name())
	}

	resp, err := complete(t, client)
	if err != nil {
		t.Fatalf("Complete 报错: %v", err)
	}
	if resp.Content != "hello" {
		t.Fatalf("Content = %q, want hello", resp.Content)
	}
	if got := up.hits.Load(); got != 1 {
		t.Fatalf("上游请求数 = %d, want 1", got)
	}

	req := up.lastRequest()
	if req.path != "/chat/completions" {
		t.Fatalf("路径 = %q, want /chat/completions", req.path)
	}
	if req.auth != "Bearer "+testAPIKey {
		t.Fatalf("Authorization = %q, 密钥未按引用注入", req.auth)
	}
	if !strings.Contains(req.body, `"include_usage":true`) {
		t.Fatalf("请求体缺少 stream_options.include_usage: %s", req.body)
	}
	if !strings.Contains(req.body, `"max_tokens":8192`) {
		t.Fatalf("请求体缺少 max_tokens 兜底: %s", req.body)
	}

	refs := resolver.resolvedRefs()
	if len(refs) != 1 || refs[0] != testKeyRef {
		t.Fatalf("解析的密钥引用 = %v, want [%s]", refs, testKeyRef)
	}
}

// TestEndpointNormalization 容错运营方粘贴完整 URL 或多余斜杠。
func TestEndpointNormalization(t *testing.T) {
	cases := []struct {
		name     string
		raw      func(base string) string
		wantPath string
	}{
		{"base url", func(base string) string { return base }, "/chat/completions"},
		{"带版本号", func(base string) string { return base + "/v1" }, "/v1/chat/completions"},
		{"尾斜杠", func(base string) string { return base + "/v1/" }, "/v1/chat/completions"},
		{"完整 URL", func(base string) string { return base + "/v1/chat/completions" }, "/v1/chat/completions"},
		{"完整 URL 带尾斜杠", func(base string) string { return base + "/v1/chat/completions/" }, "/v1/chat/completions"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := newFakeUpstream(t)
			pool := newTestPool(t, newFakeStore(testSpec("p", tc.raw(up.srv.URL))), &recordingResolver{key: testAPIKey})

			client, err := pool.Client(context.Background(), "p")
			if err != nil {
				t.Fatalf("Client 报错: %v", err)
			}
			if _, err := complete(t, client); err != nil {
				t.Fatalf("Complete 报错: %v", err)
			}
			if got := up.lastRequest().path; got != tc.wantPath {
				t.Fatalf("路径 = %q, want %q", got, tc.wantPath)
			}
		})
	}
}

// --- 熔断 ---

// TestBreakerOpensAndStopsHittingUpstream 连续失败触发熔断后，请求必须快速失败且不再打上游。
func TestBreakerOpensAndStopsHittingUpstream(t *testing.T) {
	up := newFakeUpstream(t)
	up.fail.Store(true)
	pool := newTestPool(t, newFakeStore(testSpec("p", up.srv.URL)), &recordingResolver{key: testAPIKey},
		withBreaker(fastBreaker(3, time.Minute)))

	client, err := pool.Client(context.Background(), "p")
	if err != nil {
		t.Fatalf("Client 报错: %v", err)
	}

	for i := 1; i <= 3; i++ {
		if _, err := complete(t, client); err == nil {
			t.Fatalf("第 %d 次失败调用应返回错误", i)
		}
	}
	if got := up.hits.Load(); got != 3 {
		t.Fatalf("上游请求数 = %d, want 3", got)
	}
	if pool.Healthy("p") {
		t.Fatal("连续 3 次失败后熔断器应打开，Healthy = true")
	}
	if state, fails := pool.BreakerState("p"); state != provider.BreakerOpen || fails != 3 {
		t.Fatalf("状态 = %v, 连续失败 = %d, want open/3", state, fails)
	}

	start := time.Now()
	for i := 0; i < 3; i++ {
		_, err := complete(t, client)
		if !errors.Is(err, provider.ErrCircuitOpen) {
			t.Fatalf("熔断后的错误应可判定为 ErrCircuitOpen, got %v", err)
		}
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("熔断后应快速失败，实际耗时 %v", elapsed)
	}
	if got := up.hits.Load(); got != 3 {
		t.Fatalf("熔断后不应再打上游，请求数 = %d, want 3", got)
	}
}

// TestHalfOpenRecoveryAfterSuccessProbe 冷却期结束后放行探测，探测成功即恢复。
func TestHalfOpenRecoveryAfterSuccessProbe(t *testing.T) {
	up := newFakeUpstream(t)
	up.fail.Store(true)
	pool := newTestPool(t, newFakeStore(testSpec("p", up.srv.URL)), &recordingResolver{key: testAPIKey},
		withBreaker(fastBreaker(2, 40*time.Millisecond)))

	client, err := pool.Client(context.Background(), "p")
	if err != nil {
		t.Fatalf("Client 报错: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := complete(t, client); err == nil {
			t.Fatal("上游 500 时应返回错误")
		}
	}
	if pool.Healthy("p") {
		t.Fatal("熔断器应已打开")
	}

	time.Sleep(60 * time.Millisecond)
	if !pool.Healthy("p") {
		t.Fatal("冷却期结束后应转为 half-open（Healthy = true）以放行探测")
	}

	up.fail.Store(false)
	if _, err := complete(t, client); err != nil {
		t.Fatalf("半开探测应成功, got %v", err)
	}
	if state, _ := pool.BreakerState("p"); state != provider.BreakerClosed {
		t.Fatalf("探测成功后熔断器应闭合, got %v", state)
	}
	if !pool.Healthy("p") {
		t.Fatal("恢复后 Healthy 应为 true")
	}
	if got := up.hits.Load(); got != 3 {
		t.Fatalf("上游请求数 = %d, want 3（含一次探测）", got)
	}
}

// TestPerProviderBreakerIsolation 是本包存在的主要理由：A 家熔断不得影响 B 家。
func TestPerProviderBreakerIsolation(t *testing.T) {
	upA := newFakeUpstream(t)
	upB := newFakeUpstream(t)
	upA.fail.Store(true)

	store := newFakeStore(testSpec("a", upA.srv.URL), testSpec("b", upB.srv.URL))
	pool := newTestPool(t, store, &recordingResolver{key: testAPIKey}, withBreaker(fastBreaker(3, time.Minute)))

	for _, id := range []string{"a", "b"} {
		if _, err := pool.Client(context.Background(), id); err != nil {
			t.Fatalf("Client(%s) 报错: %v", id, err)
		}
	}

	clientA, err := pool.Client(context.Background(), "a")
	if err != nil {
		t.Fatalf("Client(a) 报错: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := complete(t, clientA); err == nil {
			t.Fatal("A 上游 500 时应返回错误")
		}
	}
	if pool.Healthy("a") {
		t.Fatal("A 应已熔断")
	}
	if !pool.Healthy("b") {
		t.Fatal("B 的熔断状态被 A 污染了")
	}
	if stateA, _ := pool.BreakerState("a"); stateA != provider.BreakerOpen {
		t.Fatalf("A 状态 = %v, want open", stateA)
	}
	if stateB, failsB := pool.BreakerState("b"); stateB != provider.BreakerClosed || failsB != 0 {
		t.Fatalf("B 状态 = %v/%d, want closed/0", stateB, failsB)
	}

	clientB, err := pool.Client(context.Background(), "b")
	if err != nil {
		t.Fatalf("Client(b) 报错: %v", err)
	}
	if _, err := complete(t, clientB); err != nil {
		t.Fatalf("A 熔断时 B 应正常可用, got %v", err)
	}
	if got := upB.hits.Load(); got != 1 {
		t.Fatalf("B 上游请求数 = %d, want 1", got)
	}
	if got := upA.hits.Load(); got != 3 {
		t.Fatalf("A 上游请求数 = %d, want 3", got)
	}
}

// TestMarkFailureIgnoresDeterministicErrors 配错 key / 400 这类确定性错误不该推进熔断，
// 否则改配置也无法恢复；而真正恢复必须是「冷却 → half-open 探测成功」这条路。
func TestMarkFailureIgnoresDeterministicErrors(t *testing.T) {
	pool := newTestPool(t, newFakeStore(testSpec("p", "http://127.0.0.1:9")), &recordingResolver{key: testAPIKey},
		withBreaker(fastBreaker(3, 20*time.Millisecond)))

	badKey := &provider.APIError{StatusCode: 401, Message: "invalid api key"}
	for i := 0; i < 10; i++ {
		pool.MarkFailure("p", badKey)
	}
	if !pool.Healthy("p") {
		t.Fatal("401 不该计入熔断")
	}

	serverErr := &provider.APIError{StatusCode: 503, Message: "service unavailable"}
	for i := 0; i < 3; i++ {
		pool.MarkFailure("p", serverErr)
	}
	if pool.Healthy("p") {
		t.Fatal("5xx 应计入熔断")
	}

	// open 态下的成功不计入（熔断期间不放行请求），冷却结束后才允许探测。
	pool.MarkSuccess("p")
	time.Sleep(30 * time.Millisecond)
	if !pool.Healthy("p") {
		t.Fatal("冷却结束后应转为 half-open 放行探测")
	}
	pool.MarkSuccess("p")
	if state, _ := pool.BreakerState("p"); state != provider.BreakerClosed {
		t.Fatalf("探测成功后应闭合, got %v", state)
	}
}

// --- 缓存与失效 ---

// TestClientCachedUntilInvalidate 同一配置复用客户端；Invalidate 后重建并重置健康状态。
func TestClientCachedUntilInvalidate(t *testing.T) {
	up := newFakeUpstream(t)
	up.fail.Store(true)
	store := newFakeStore(testSpec("p", up.srv.URL))
	pool := newTestPool(t, store, &recordingResolver{key: testAPIKey}, withBreaker(fastBreaker(2, time.Minute)))

	ctx := context.Background()
	first, err := pool.Client(ctx, "p")
	if err != nil {
		t.Fatalf("Client 报错: %v", err)
	}
	second, err := pool.Client(ctx, "p")
	if err != nil {
		t.Fatalf("Client 报错: %v", err)
	}
	if first != second {
		t.Fatal("配置未变时应复用同一个客户端")
	}
	if reads := store.readCount(); reads != 2 {
		t.Fatalf("目录读取次数 = %d, want 2（每次调用都复核配置，才能感知热更新）", reads)
	}

	for i := 0; i < 2; i++ {
		if _, err := complete(t, first); err == nil {
			t.Fatal("上游 500 时应返回错误")
		}
	}
	if pool.Healthy("p") {
		t.Fatal("应已熔断")
	}

	pool.Invalidate("p")
	if !pool.Healthy("p") {
		t.Fatal("Invalidate 后应回到未熔断状态")
	}

	third, err := pool.Client(ctx, "p")
	if err != nil {
		t.Fatalf("Invalidate 后 Client 报错: %v", err)
	}
	if third == first {
		t.Fatal("Invalidate 后应重建客户端")
	}

	up.fail.Store(false)
	if _, err := complete(t, third); err != nil {
		t.Fatalf("重建后的客户端应可用, got %v", err)
	}

	pool.InvalidateAll()
	fourth, err := pool.Client(ctx, "p")
	if err != nil {
		t.Fatalf("InvalidateAll 后 Client 报错: %v", err)
	}
	if fourth == third {
		t.Fatal("InvalidateAll 后应重建客户端")
	}
}

// TestConfigChangeRebuildsClient 目录改了（Endpoint 变化）无需 Invalidate 也能生效。
func TestConfigChangeRebuildsClient(t *testing.T) {
	upA := newFakeUpstream(t)
	upB := newFakeUpstream(t)
	store := newFakeStore(testSpec("p", upA.srv.URL))
	pool := newTestPool(t, store, &recordingResolver{key: testAPIKey})

	ctx := context.Background()
	first, err := pool.Client(ctx, "p")
	if err != nil {
		t.Fatalf("Client 报错: %v", err)
	}
	if _, err := complete(t, first); err != nil {
		t.Fatalf("Complete 报错: %v", err)
	}

	changed := testSpec("p", upB.srv.URL)
	store.mu.Lock()
	store.specs["p"] = changed
	store.mu.Unlock()

	second, err := pool.Client(ctx, "p")
	if err != nil {
		t.Fatalf("Client 报错: %v", err)
	}
	if second == first {
		t.Fatal("Endpoint 变更后应重建客户端")
	}
	if _, err := complete(t, second); err != nil {
		t.Fatalf("Complete 报错: %v", err)
	}
	if got := upB.hits.Load(); got != 1 {
		t.Fatalf("新 Endpoint 请求数 = %d, want 1", got)
	}
	if got := upA.hits.Load(); got != 1 {
		t.Fatalf("旧 Endpoint 请求数 = %d, want 1", got)
	}

	// 同一份目录条目（按目录读回来的）重复调用应复用。
	third, err := pool.Client(ctx, "p")
	if err != nil {
		t.Fatalf("Client 报错: %v", err)
	}
	if third != second {
		t.Fatal("配置未变时应复用客户端")
	}
}

// --- 错误路径 ---

// TestSecretResolverFailureIsNotFatal 密钥解析失败必须是可读错误：不 panic、不发上游请求、
// 不计入熔断（否则凭据后端抖一次就会把整个 provider 打成假死）。
func TestSecretResolverFailureIsNotFatal(t *testing.T) {
	up := newFakeUpstream(t)
	store := newFakeStore(testSpec("p", up.srv.URL))

	resolverErr := errors.New("凭据管理器不可用")
	pool := newTestPool(t, store, &recordingResolver{key: testAPIKey, err: resolverErr}, withBreaker(fastBreaker(2, time.Minute)))

	client, err := pool.Client(context.Background(), "p")
	if err != nil {
		t.Fatalf("构建客户端不该依赖密钥解析结果, got %v", err)
	}
	for i := 0; i < 3; i++ {
		_, err := complete(t, client)
		if err == nil {
			t.Fatal("密钥解析失败时应返回错误")
		}
		if !errors.Is(err, resolverErr) {
			t.Fatalf("错误应可判定为密钥解析失败, got %v", err)
		}
	}
	if got := up.hits.Load(); got != 0 {
		t.Fatalf("密钥解析失败不该发出上游请求, 请求数 = %d", got)
	}
	if !pool.Healthy("p") {
		t.Fatal("密钥解析失败不该计入熔断")
	}

	// 未注入解析器时同样只报错、不 panic。
	noResolver := newTestPool(t, store, nil)
	client2, err := noResolver.Client(context.Background(), "p")
	if err != nil {
		t.Fatalf("Client 报错: %v", err)
	}
	if _, err := complete(t, client2); !errors.Is(err, ErrNoSecretResolver) {
		t.Fatalf("应返回 ErrNoSecretResolver, got %v", err)
	}
	if got := up.hits.Load(); got != 0 {
		t.Fatalf("不该发出上游请求, 请求数 = %d", got)
	}
}

// TestClientRejectsUnusableSpec 目录条目不可用时构建期就报错，而不是发出去变成看不懂的 HTTP 错误。
func TestClientRejectsUnusableSpec(t *testing.T) {
	valid := testSpec("p", "http://127.0.0.1:9")

	cases := []struct {
		name    string
		mutate  func(model.ProviderSpec) model.ProviderSpec
		wantErr error
	}{
		{"ID 为空", func(s model.ProviderSpec) model.ProviderSpec { s.ID = ""; return s }, nil},
		{"Endpoint 为空", func(s model.ProviderSpec) model.ProviderSpec { s.Endpoint = ""; return s }, ErrMissingEndpoint},
		{"Endpoint 无协议", func(s model.ProviderSpec) model.ProviderSpec { s.Endpoint = "api.example.com/v1"; return s }, nil},
		{"Endpoint 非法", func(s model.ProviderSpec) model.ProviderSpec { s.Endpoint = "http://[::1"; return s }, nil},
		{"已禁用", func(s model.ProviderSpec) model.ProviderSpec {
			s.Status = model.ProviderStatusDisabled
			return s
		}, model.ErrDisabled},
		{"协议不支持", func(s model.ProviderSpec) model.ProviderSpec {
			s.Protocol = model.ProtocolAnthropic
			return s
		}, ErrUnsupportedProtocol},
		{"无 APIKeyRef", func(s model.ProviderSpec) model.ProviderSpec { s.APIKeyRef = ""; return s }, ErrMissingAPIKeyRef},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore(tc.mutate(valid))
			pool := newTestPool(t, store, &recordingResolver{key: testAPIKey})

			_, err := pool.Client(context.Background(), tc.mutate(valid).ID)
			if err == nil {
				t.Fatal("应返回错误")
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("错误 = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// TestUnknownProviderAndMissingStore 目录未命中/未注入都返回可判定错误。
func TestUnknownProviderAndMissingStore(t *testing.T) {
	pool := newTestPool(t, newFakeStore(testSpec("p", "http://127.0.0.1:9")), &recordingResolver{key: testAPIKey})
	if _, err := pool.Client(context.Background(), "nope"); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("应返回 model.ErrNotFound, got %v", err)
	}

	empty := New(nil, &recordingResolver{key: testAPIKey})
	if _, err := empty.Client(context.Background(), "p"); !errors.Is(err, ErrNoStore) {
		t.Fatalf("应返回 ErrNoStore, got %v", err)
	}
}

// TestCapabilitiesFromConfigJSON 逐 provider 关掉扩展字段（第三方端点可能拒绝未知字段）。
func TestCapabilitiesFromConfigJSON(t *testing.T) {
	cases := []struct {
		name          string
		configJSON    string
		wantStream    bool
		wantReasoning bool
	}{
		{"缺省全开", "", true, true},
		{"只关 reasoning", `{"capabilities":{"reasoning":false}}`, true, false},
		{"只关 stream", `{"capabilities":{"stream":false}}`, false, true},
		{"全关", `{"capabilities":{"stream":false,"reasoning":false}}`, false, false},
		{"非法 JSON 退回默认", `{oops`, true, true},
		{"无 capabilities 字段退回默认", `{"weight":3}`, true, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := newFakeUpstream(t)
			spec := testSpec("p", up.srv.URL)
			spec.ConfigJSON = tc.configJSON
			pool := newTestPool(t, newFakeStore(spec), &recordingResolver{key: testAPIKey})

			client, err := pool.Client(context.Background(), "p")
			if err != nil {
				t.Fatalf("Client 报错: %v", err)
			}
			// 带 reasoning_content 的历史轮：关闭 reasoning 开关时必须被剥离
			// （第三方端点不接受未知字段时会直接 400）。
			_, err = completeMessages(t, client, []provider.Message{
				{Role: provider.RoleAssistant, Content: "prev", ReasoningContent: "thought"},
				{Role: provider.RoleUser, Content: "hi"},
			})
			if err != nil {
				t.Fatalf("Complete 报错: %v", err)
			}

			body := up.lastRequest().body
			if got := strings.Contains(body, `"include_usage":true`); got != tc.wantStream {
				t.Fatalf("include_usage 存在 = %v, want %v（body=%s）", got, tc.wantStream, body)
			}
			if got := strings.Contains(body, `"reasoning_content"`); got != tc.wantReasoning {
				t.Fatalf("reasoning_content 存在 = %v, want %v（body=%s）", got, tc.wantReasoning, body)
			}
		})
	}
}

// --- 并发 ---

// TestConcurrentClientBuildSharesOneInstance 并发首次构建必须收敛到同一个客户端与同一份
// 熔断状态（-race 下同时校验无数据竞争）。
func TestConcurrentClientBuildSharesOneInstance(t *testing.T) {
	up := newFakeUpstream(t)
	store := newFakeStore(testSpec("p", up.srv.URL))
	pool := newTestPool(t, store, &recordingResolver{key: testAPIKey})

	const n = 24
	gate := make(chan struct{})
	clients := make([]*provider.Client, n)
	errs := make([]error, n)

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-gate
			clients[i], errs[i] = pool.Client(context.Background(), "p")
		}(i)
	}
	close(gate)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d 报错: %v", i, err)
		}
	}
	for i, c := range clients {
		if c != clients[0] {
			t.Fatalf("goroutine %d 拿到不同的客户端实例", i)
		}
	}
}

// TestConcurrentHealthAndBuild 健康查询/记账/构建/失效并发交叉执行不得出现数据竞争，
// 且不同 provider 互不干扰。
func TestConcurrentHealthAndBuild(t *testing.T) {
	upA := newFakeUpstream(t)
	upB := newFakeUpstream(t)
	store := newFakeStore(testSpec("a", upA.srv.URL), testSpec("b", upB.srv.URL))
	pool := newTestPool(t, store, &recordingResolver{key: testAPIKey}, withBreaker(fastBreaker(3, 5*time.Millisecond)))

	ctx := context.Background()
	clientA, err := pool.Client(ctx, "a")
	if err != nil {
		t.Fatalf("Client(a) 报错: %v", err)
	}
	clientB, err := pool.Client(ctx, "b")
	if err != nil {
		t.Fatalf("Client(b) 报错: %v", err)
	}

	upA.fail.Store(true)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 8; j++ {
				_, _ = complete(t, clientA)
				_, _ = complete(t, clientB)
				pool.Healthy("a")
				pool.Healthy("b")
				pool.BreakerState("a")
				pool.MarkFailure("a", &provider.APIError{StatusCode: 503, Message: "boom"})
				if j%4 == 0 {
					pool.Invalidate("b")
				}
			}
		}(i)
	}
	wg.Wait()

	if !pool.Healthy("b") {
		t.Fatal("B 不该被 A 的失败牵连")
	}
}
