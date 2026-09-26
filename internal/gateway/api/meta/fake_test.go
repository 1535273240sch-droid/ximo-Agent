package meta

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/ximo888ok-netizen/ximo-agent/internal/account"
	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/catalog"
	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/model"
)

// fakeStore 是内存实现，同时满足 account.Store（供真实 account.Service 使用）与
// 本包的 UsageStore。语义与 internal/gateway/store 的约定对齐：未命中返回
// model.ErrNotFound，唯一冲突返回 model.ErrConflict。
//
// 刻意**不**实现 ConsumeDeviceCode：这样覆盖的是「store 无消费能力时 account 的
// 进程内兜底」分支；真实 store 已实现该方法，数据库侧的一次性消费由
// integration_test.go 直接核对 gw_device_codes.status 来覆盖。
type fakeStore struct {
	mu sync.Mutex

	users       map[string]model.User
	userNames   map[string]string
	keys        map[string]model.APIKey // by key hash
	keyIDs      map[string]string       // key id -> key hash
	sessions    map[string]model.AuthSession
	devices     map[string]model.DeviceCode // by device code hash
	deviceNames map[string]string           // user code -> device code hash
	usage       []model.UsageRecord

	// 故障注入与调用观测。
	errListUsage     error
	errCreateSession error
	listUsageCalls   int
	lastUsageUser    string
	lastUsageLimit   int
	lastUsageOffset  int
}

var (
	_ account.Store = (*fakeStore)(nil)
	_ UsageStore    = (*fakeStore)(nil)
)

func newFakeStore() *fakeStore {
	return &fakeStore{
		users:       make(map[string]model.User),
		userNames:   make(map[string]string),
		keys:        make(map[string]model.APIKey),
		keyIDs:      make(map[string]string),
		sessions:    make(map[string]model.AuthSession),
		devices:     make(map[string]model.DeviceCode),
		deviceNames: make(map[string]string),
	}
}

func (f *fakeStore) CreateUser(_ context.Context, u model.User) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.userNames[u.Username]; ok {
		return model.ErrConflict
	}
	f.users[u.ID] = u
	f.userNames[u.Username] = u.ID
	return nil
}

func (f *fakeStore) GetUser(_ context.Context, id string) (model.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[id]
	if !ok {
		return model.User{}, model.ErrNotFound
	}
	return u, nil
}

func (f *fakeStore) GetUserByName(_ context.Context, username string) (model.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.userNames[username]
	if !ok {
		return model.User{}, model.ErrNotFound
	}
	return f.users[id], nil
}

func (f *fakeStore) CreateAPIKey(_ context.Context, k model.APIKey) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.keys[k.KeyHash]; ok {
		return model.ErrConflict
	}
	f.keys[k.KeyHash] = k
	f.keyIDs[k.ID] = k.KeyHash
	return nil
}

func (f *fakeStore) GetAPIKeyByHash(_ context.Context, hash string) (model.APIKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k, ok := f.keys[hash]
	if !ok {
		return model.APIKey{}, model.ErrNotFound
	}
	return k, nil
}

func (f *fakeStore) RevokeAPIKey(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	hash, ok := f.keyIDs[id]
	if !ok {
		return model.ErrNotFound
	}
	k := f.keys[hash]
	k.Status = model.KeyStatusRevoked
	f.keys[hash] = k
	return nil
}

func (f *fakeStore) TouchAPIKey(_ context.Context, id string, at int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	hash, ok := f.keyIDs[id]
	if !ok {
		return model.ErrNotFound
	}
	k := f.keys[hash]
	k.LastUsedAt = at
	f.keys[hash] = k
	return nil
}

func (f *fakeStore) CreateAuthSession(_ context.Context, a model.AuthSession) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.errCreateSession != nil {
		return f.errCreateSession
	}
	if _, ok := f.sessions[a.ID]; ok {
		return model.ErrConflict
	}
	f.sessions[a.ID] = a
	return nil
}

func (f *fakeStore) GetAuthSessionByRefreshHash(_ context.Context, hash string) (model.AuthSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.sessions {
		if s.RefreshHash == hash {
			return s, nil
		}
	}
	return model.AuthSession{}, model.ErrNotFound
}

func (f *fakeStore) GetAuthSessionByAccessHash(_ context.Context, hash string) (model.AuthSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.sessions {
		if s.AccessHash == hash {
			return s, nil
		}
	}
	return model.AuthSession{}, model.ErrNotFound
}

func (f *fakeStore) RotateAuthSession(_ context.Context, oldID string, next model.AuthSession) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	old, ok := f.sessions[oldID]
	if !ok {
		return model.ErrNotFound
	}
	if _, exists := f.sessions[next.ID]; exists {
		return model.ErrConflict
	}
	if old.RevokedAt == 0 {
		old.RevokedAt = next.CreatedAt
	}
	f.sessions[oldID] = old
	f.sessions[next.ID] = next
	return nil
}

func (f *fakeStore) RevokeAuthSession(_ context.Context, id string, at int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[id]
	if !ok {
		return model.ErrNotFound
	}
	s.RevokedAt = at
	f.sessions[id] = s
	return nil
}

func (f *fakeStore) CreateDeviceCode(_ context.Context, d model.DeviceCode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.devices[d.DeviceCodeHash]; ok {
		return model.ErrConflict
	}
	if _, ok := f.deviceNames[d.UserCode]; ok {
		return model.ErrConflict
	}
	f.devices[d.DeviceCodeHash] = d
	f.deviceNames[d.UserCode] = d.DeviceCodeHash
	return nil
}

func (f *fakeStore) GetDeviceCodeByDeviceHash(_ context.Context, hash string) (model.DeviceCode, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.devices[hash]
	if !ok {
		return model.DeviceCode{}, model.ErrNotFound
	}
	return d, nil
}

func (f *fakeStore) GetDeviceCodeByUserCode(_ context.Context, userCode string) (model.DeviceCode, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	hash, ok := f.deviceNames[userCode]
	if !ok {
		return model.DeviceCode{}, model.ErrNotFound
	}
	return f.devices[hash], nil
}

func (f *fakeStore) ApproveDeviceCode(_ context.Context, userCode, userID string, at int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	hash, ok := f.deviceNames[userCode]
	if !ok {
		return model.ErrNotFound
	}
	d := f.devices[hash]
	if d.Status != model.DeviceStatusPending {
		return model.ErrConflict
	}
	d.UserID = userID
	d.Status = model.DeviceStatusApproved
	d.LastPolledAt = at
	f.devices[hash] = d
	return nil
}

func (f *fakeStore) TouchDeviceCodePoll(_ context.Context, hash string, at int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.devices[hash]
	if !ok {
		return model.ErrNotFound
	}
	d.LastPolledAt = at
	f.devices[hash] = d
	return nil
}

// ListUsage 只回该用户的记录（与真实 store 的 WHERE user_id=? 同语义），
// 最近优先，并记录调用参数供断言。
func (f *fakeStore) ListUsage(_ context.Context, userID string, limit, offset int) ([]model.UsageRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listUsageCalls++
	f.lastUsageUser = userID
	f.lastUsageLimit = limit
	f.lastUsageOffset = offset
	if f.errListUsage != nil {
		return nil, f.errListUsage
	}
	var mine []model.UsageRecord
	for _, u := range f.usage {
		if u.UserID == userID {
			mine = append(mine, u)
		}
	}
	sort.SliceStable(mine, func(i, j int) bool { return mine[i].CreatedAt > mine[j].CreatedAt })
	if limit > 0 && offset >= 0 {
		if offset >= len(mine) {
			return []model.UsageRecord{}, nil
		}
		mine = mine[offset:]
		if len(mine) > limit {
			mine = mine[:limit]
		}
	}
	return mine, nil
}

func (f *fakeStore) addUsage(recs ...model.UsageRecord) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.usage = append(f.usage, recs...)
}

// ---------------------------------------------------------------- catalog

type fakeCatalog struct {
	models    []model.ModelSpec
	errModels error
	cands     map[string][]catalog.Candidate
	errCands  map[string]error

	modelsCalls int
}

var _ CatalogService = (*fakeCatalog)(nil)

func (f *fakeCatalog) PublicModels(context.Context) ([]model.ModelSpec, error) {
	f.modelsCalls++
	if f.errModels != nil {
		return nil, f.errModels
	}
	return f.models, nil
}

func (f *fakeCatalog) Candidates(_ context.Context, modelID string) ([]catalog.Candidate, error) {
	if err, ok := f.errCands[modelID]; ok {
		return nil, err
	}
	return f.cands[modelID], nil
}

// ---------------------------------------------------------------- logger

type recLogger struct {
	mu          sync.Mutex
	warns       []string
	errs        []string
	warnFields  []string
	errorFields []string
}

var _ Logger = (*recLogger)(nil)

func (l *recLogger) Warn(_ context.Context, msg string, fields ...map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warns = append(l.warns, msg)
	l.warnFields = append(l.warnFields, flatten(fields...))
}

func (l *recLogger) Error(_ context.Context, msg string, fields ...map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.errs = append(l.errs, msg)
	l.errorFields = append(l.errorFields, flatten(fields...))
}

func (l *recLogger) counts() (warn, errCount int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.warns), len(l.errs)
}

// blob 汇总所有日志文本，用于「凭据/内部字段不进日志」断言。
func (l *recLogger) blob() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(append(append(append([]string{}, l.warns...), l.errs...), append(l.warnFields, l.errorFields...)...), "\n")
}

func flatten(fields ...map[string]any) string {
	var b strings.Builder
	for _, m := range fields {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			b.WriteString(k)
			b.WriteString("=")
			b.WriteString(strings.TrimSpace(toText(m[k])))
			b.WriteString(" ")
		}
	}
	return b.String()
}

func toText(v any) string {
	buf, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(buf)
}

// ---------------------------------------------------------------- 测试辅助

// newRouter 按装配方的方式把 Routes 挂到 ServeMux，顺带验证 Pattern 合法。
func newRouter(t *testing.T, d Deps) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	for _, rt := range Routes(d) {
		mux.HandleFunc(rt.Pattern, rt.Handler)
	}
	return mux
}

type requestOpt func(*http.Request)

func withUser(userID string) requestOpt {
	return func(r *http.Request) { *r = *r.WithContext(WithUserID(r.Context(), userID)) }
}

func withBearer(token string) requestOpt {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }
}

// do 发请求。body 非 nil 时按 JSON 编码。
func do(t *testing.T, h http.Handler, method, target string, body any, opts ...requestOpt) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("编码请求体失败: %v", err)
		}
		rdr = bytes.NewReader(buf)
	}
	req := httptest.NewRequest(method, target, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, opt := range opts {
		opt(req)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func bodyMap(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是 JSON 对象: %v, body=%s", err, rec.Body.String())
	}
	return out
}

// dataList 取出 {"data":[...]} 里的数组。
func dataList(t *testing.T, rec *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
	raw, ok := bodyMap(t, rec)["data"]
	if !ok {
		t.Fatalf("响应缺少 data 字段: %s", rec.Body.String())
	}
	arr, ok := raw.([]any)
	if !ok {
		t.Fatalf("data 不是数组: %T", raw)
	}
	out := make([]map[string]any, 0, len(arr))
	for _, v := range arr {
		item, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("data 元素不是对象: %T", v)
		}
		out = append(out, item)
	}
	return out
}

// errorCode 取出 {"error":{"code":...}}。
func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	body := bodyMap(t, rec)
	inner, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺少 error 对象: %s", rec.Body.String())
	}
	code, _ := inner["code"].(string)
	return code
}

// assertKeys 断言对象的键集合与期望完全一致（多一个少一个都失败）。
func assertKeys(t *testing.T, m map[string]any, want ...string) {
	t.Helper()
	got := make([]string, 0, len(m))
	for k := range m {
		got = append(got, k)
	}
	sort.Strings(got)
	sorted := append([]string{}, want...)
	sort.Strings(sorted)
	if strings.Join(got, ",") != strings.Join(sorted, ",") {
		t.Fatalf("键集合不符\n实际: %v\n期望: %v", got, sorted)
	}
}

// assertNoLeak 断言响应体里没有内部字段名或凭据形态的字符串。
func assertNoLeak(t *testing.T, rec *httptest.ResponseRecorder, extra ...string) {
	t.Helper()
	body := rec.Body.String()
	for _, bad := range append([]string{
		"api_key_ref", "config_json", "capabilities_json", "password_hash", "key_hash",
		"refresh_hash", "access_hash", "device_code_hash", "user_id", "sk-",
	}, extra...) {
		if strings.Contains(body, bad) {
			t.Fatalf("响应疑似泄漏内部字段 %q: %s", bad, body)
		}
	}
}
