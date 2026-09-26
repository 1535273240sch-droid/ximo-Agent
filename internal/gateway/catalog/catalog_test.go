package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/model"
)

// fakeStore 是 catalog.store 的内存实现。所有读取返回副本并带互斥锁，
// 使并发用例在 -race 下也能给出可信结论。
type fakeStore struct {
	mu sync.Mutex

	models    []model.ModelSpec
	providers map[string]model.ProviderSpec
	links     map[string][]model.ProviderModel

	linkErr     error
	modelErr    error
	providerErr map[string]error

	linkCalls     int
	providerCalls int
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		providers:   map[string]model.ProviderSpec{},
		links:       map[string][]model.ProviderModel{},
		providerErr: map[string]error{},
	}
}

func (f *fakeStore) ListProviderModels(_ context.Context, modelID string) ([]model.ProviderModel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.linkCalls++
	if f.linkErr != nil {
		return nil, f.linkErr
	}
	rows := f.links[modelID]
	out := make([]model.ProviderModel, len(rows))
	copy(out, rows)
	return out, nil
}

func (f *fakeStore) GetProvider(_ context.Context, id string) (model.ProviderSpec, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.providerCalls++
	if err := f.providerErr[id]; err != nil {
		return model.ProviderSpec{}, err
	}
	p, ok := f.providers[id]
	if !ok {
		return model.ProviderSpec{}, fmt.Errorf("fakeStore: provider %q: %w", id, model.ErrNotFound)
	}
	return p, nil
}

func (f *fakeStore) ListModels(_ context.Context, onlyEnabled bool) ([]model.ModelSpec, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.modelErr != nil {
		return nil, f.modelErr
	}
	out := make([]model.ModelSpec, 0, len(f.models))
	for _, m := range f.models {
		if onlyEnabled && !m.Enabled {
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

// addProvider 登记一个 provider；status 为空时按 enabled 处理（多数用例只关心个别字段）。
func (f *fakeStore) addProvider(id, protocol, status string) {
	if status == "" {
		status = model.ProviderStatusEnabled
	}
	f.providers[id] = model.ProviderSpec{
		ID:         id,
		Name:       "上游 " + id,
		Endpoint:   "https://" + id + ".example.com/v1",
		Protocol:   protocol,
		Status:     status,
		APIKeyRef:  "secretref:v1:" + id,
		ConfigJSON: `{"note":"internal"}`,
		TimeoutMS:  60000,
		Weight:     1,
	}
}

func (f *fakeStore) addLink(modelID, providerID, upstream string, priority int64, enabled bool) {
	f.links[modelID] = append(f.links[modelID], model.ProviderModel{
		ProviderID:      providerID,
		ModelID:         modelID,
		UpstreamModelID: upstream,
		Enabled:         enabled,
		Priority:        priority,
	})
}

// fakeProbe 用「不健康集合」模拟熔断器：不在集合里即视为健康。
type fakeProbe struct {
	mu        sync.Mutex
	unhealthy map[string]bool
	calls     int
}

func newFakeProbe(unhealthy ...string) *fakeProbe {
	p := &fakeProbe{unhealthy: map[string]bool{}}
	for _, id := range unhealthy {
		p.unhealthy[id] = true
	}
	return p
}

func (p *fakeProbe) Healthy(providerID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	return !p.unhealthy[providerID]
}

func candidateIDs(cands []Candidate) []string {
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.Provider.ID+"/"+c.UpstreamModel)
	}
	return out
}

func assertIDs(t *testing.T, got []Candidate, want ...string) {
	t.Helper()
	if g := candidateIDs(got); !reflect.DeepEqual(g, want) {
		t.Fatalf("候选不符\n实际: %v\n期望: %v", g, want)
	}
}

// TestCandidatesOrdersByPriority 确认按 Priority 升序（数值小者优先），
// 同优先级按 ProviderID 兜底，顺序稳定可复现且与行输入顺序无关。
func TestCandidatesOrdersByPriority(t *testing.T) {
	st := newFakeStore()
	for _, id := range []string{"prov-b", "prov-a", "prov-c", "prov-d"} {
		st.addProvider(id, model.ProtocolOpenAIChat, "")
	}

	// 故意乱序写入：prov-c 与 prov-a 同优先级，(provider_id, model_id) 是主键，
	// 同模型下 ProviderID 唯一，故 prov-a 必须恒在 prov-c 之前。
	st.addLink("gpt-x", "prov-b", "b-1", 20, true)
	st.addLink("gpt-x", "prov-c", "c-1", 10, true)
	st.addLink("gpt-x", "prov-d", "d-1", 0, true) // 0 = 默认最高优先级
	st.addLink("gpt-x", "prov-a", "a-1", 10, true)

	c := New(st, nil)
	got, err := c.Candidates(context.Background(), "gpt-x")
	if err != nil {
		t.Fatalf("Candidates 返回错误: %v", err)
	}
	assertIDs(t, got,
		"prov-d/d-1",
		"prov-a/a-1",
		"prov-c/c-1",
		"prov-b/b-1",
	)

	// 重复调用顺序必须一致（排序不能依赖 map 迭代顺序或底层行序）。
	for i := 0; i < 5; i++ {
		again, err := c.Candidates(context.Background(), "gpt-x")
		if err != nil {
			t.Fatalf("第 %d 次调用返回错误: %v", i, err)
		}
		assertIDs(t, again,
			"prov-d/d-1",
			"prov-a/a-1",
			"prov-c/c-1",
			"prov-b/b-1",
		)
	}

	// 候选必须带回上游模型名与 provider 完整条目（upstream 需要 APIKeyRef 构建客户端）。
	if got[0].UpstreamModel != "d-1" || got[0].Provider.Endpoint == "" || got[0].Provider.TimeoutMS != 60000 {
		t.Fatalf("候选字段不完整: %+v", got[0])
	}
}

// TestCandidatesFiltersDisabledMapping 确认 provider_models.enabled=false 的映射不出现。
func TestCandidatesFiltersDisabledMapping(t *testing.T) {
	st := newFakeStore()
	st.addProvider("prov-a", model.ProtocolOpenAIChat, "")
	st.addProvider("prov-b", model.ProtocolOpenAIChat, "")
	st.addLink("m1", "prov-a", "a-1", 1, true)
	st.addLink("m1", "prov-b", "b-1", 2, false)

	got, err := New(st, nil).Candidates(context.Background(), "m1")
	if err != nil {
		t.Fatalf("Candidates 返回错误: %v", err)
	}
	assertIDs(t, got, "prov-a/a-1")
	// 停用的映射应在读 provider 之前就被丢掉：只应读到一个 provider。
	if st.providerCalls != 1 {
		t.Fatalf("停用映射不应触发 provider 读取，期望 1 次，实际 %d 次", st.providerCalls)
	}
}

// TestCandidatesFiltersDisabledProvider 确认 provider 未启用（status != enabled）时被整体过滤，
// 即便映射本身是启用的。
func TestCandidatesFiltersDisabledProvider(t *testing.T) {
	st := newFakeStore()
	st.addProvider("prov-ok", model.ProtocolOpenAIChat, model.ProviderStatusEnabled)
	st.addProvider("prov-off", model.ProtocolOpenAIChat, model.ProviderStatusDisabled)
	st.addProvider("prov-empty", model.ProtocolOpenAIChat, "")
	{
		p := st.providers["prov-empty"]
		p.Status = "" // 空状态不当作 enabled
		st.providers["prov-empty"] = p
	}

	st.addLink("m1", "prov-off", "off-1", 1, true)
	st.addLink("m1", "prov-ok", "ok-1", 9, true)
	st.addLink("m1", "prov-empty", "empty-1", 2, true)

	got, err := New(st, newFakeProbe()).Candidates(context.Background(), "m1")
	if err != nil {
		t.Fatalf("Candidates 返回错误: %v", err)
	}
	assertIDs(t, got, "prov-ok/ok-1")
}

// TestCandidatesSkipsUnhealthyProvider 确认熔断打开（Probe 不健康）的候选被跳过，
// 其余候选顺序不受影响。
func TestCandidatesSkipsUnhealthyProvider(t *testing.T) {
	st := newFakeStore()
	for _, id := range []string{"prov-a", "prov-b", "prov-c"} {
		st.addProvider(id, model.ProtocolOpenAIChat, "")
	}
	st.addLink("m1", "prov-a", "a-1", 1, true)
	st.addLink("m1", "prov-b", "b-1", 2, true)
	st.addLink("m1", "prov-c", "c-1", 3, true)

	probe := newFakeProbe("prov-a")
	got, err := New(st, probe).Candidates(context.Background(), "m1")
	if err != nil {
		t.Fatalf("Candidates 返回错误: %v", err)
	}
	assertIDs(t, got, "prov-b/b-1", "prov-c/c-1")
	if probe.calls == 0 {
		t.Fatal("探查健康度必须经 Probe，而不是自行判断")
	}
	// 不健康的候选应连 provider 都不读（省一次库访问，且避免读到停用配置）。
	if st.providerCalls != 2 {
		t.Fatalf("熔断中的候选不应触发 provider 读取，期望 2 次，实际 %d 次", st.providerCalls)
	}

	// 熔断恢复后该候选应重新出现。
	probe.mu.Lock()
	delete(probe.unhealthy, "prov-a")
	probe.mu.Unlock()
	got, err = New(st, probe).Candidates(context.Background(), "m1")
	if err != nil {
		t.Fatalf("恢复后 Candidates 返回错误: %v", err)
	}
	assertIDs(t, got, "prov-a/a-1", "prov-b/b-1", "prov-c/c-1")
}

// TestCandidatesEmptyWhenSoleCandidateUnhealthy 是路由降级的关键用例：
// 唯一候选不健康时必须返回空候选而不是 panic / 报错，调用方据此回
// provider_unavailable。
func TestCandidatesEmptyWhenSoleCandidateUnhealthy(t *testing.T) {
	st := newFakeStore()
	st.addProvider("prov-only", model.ProtocolOpenAIChat, "")
	st.addLink("m1", "prov-only", "only-1", 1, true)

	got, err := New(st, newFakeProbe("prov-only")).Candidates(context.Background(), "m1")
	if err != nil {
		t.Fatalf("无可用上游不应报错，实际: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("期望空候选，实际 %v", candidateIDs(got))
	}
	if got == nil {
		t.Fatal("空候选应为非 nil 切片，便于直接序列化为 []")
	}
}

// TestCandidatesEmptyForUnknownOrEmptyModel 确认模型不存在、模型无映射、模型 ID 为空
// 三种情况都返回空候选且不查库（模型不存在由调用方转成 model_not_found）。
func TestCandidatesEmptyForUnknownOrEmptyModel(t *testing.T) {
	st := newFakeStore()
	st.addProvider("prov-a", model.ProtocolOpenAIChat, "")
	c := New(st, newFakeProbe())

	for _, modelID := range []string{"", "no-such-model"} {
		got, err := c.Candidates(context.Background(), modelID)
		if err != nil {
			t.Fatalf("modelID=%q: 不应报错，实际 %v", modelID, err)
		}
		if len(got) != 0 || got == nil {
			t.Fatalf("modelID=%q: 期望非 nil 空候选，实际 %v", modelID, got)
		}
	}
	if st.linkCalls != 1 {
		t.Fatalf("空模型 ID 不应查库：期望 1 次查询，实际 %d 次", st.linkCalls)
	}
}

// TestCandidatesWithoutProbeTreatsAllEnabledHealthy 确认未注入探针时不做健康度过滤
// （启动早期 upstream 池未就绪，按不出故障处理）。
func TestCandidatesWithoutProbeTreatsAllEnabledHealthy(t *testing.T) {
	st := newFakeStore()
	st.addProvider("prov-a", model.ProtocolOpenAIChat, "")
	st.addProvider("prov-b", model.ProtocolOpenAIChat, "")
	st.addLink("m1", "prov-a", "a-1", 1, true)
	st.addLink("m1", "prov-b", "b-1", 2, true)

	got, err := New(st, nil).Candidates(context.Background(), "m1")
	if err != nil {
		t.Fatalf("Candidates 返回错误: %v", err)
	}
	assertIDs(t, got, "prov-a/a-1", "prov-b/b-1")
}

// TestCandidatesPropagatesStoreError 确认存储故障被包装后原样上抛（errors.Is 可判定），
// 且不把故障伪装成「无候选」——否则调用方会把数据库故障当成模型下线，静默降级。
func TestCandidatesPropagatesStoreError(t *testing.T) {
	sentinel := errors.New("db: disk I/O error")

	t.Run("读上游映射失败", func(t *testing.T) {
		st := newFakeStore()
		st.linkErr = sentinel
		got, err := New(st, newFakeProbe()).Candidates(context.Background(), "m1")
		if !errors.Is(err, sentinel) {
			t.Fatalf("期望包装后仍可 Is(sentinel)，实际 %v", err)
		}
		if got != nil {
			t.Fatalf("出错时应返回 nil 候选，实际 %v", candidateIDs(got))
		}
	})

	t.Run("读 provider 失败", func(t *testing.T) {
		st := newFakeStore()
		st.addProvider("prov-a", model.ProtocolOpenAIChat, "")
		st.addLink("m1", "prov-a", "a-1", 1, true)
		st.providerErr["prov-a"] = sentinel

		got, err := New(st, newFakeProbe()).Candidates(context.Background(), "m1")
		if !errors.Is(err, sentinel) {
			t.Fatalf("期望包装后仍可 Is(sentinel)，实际 %v", err)
		}
		if got != nil {
			t.Fatalf("出错时应返回 nil 候选，实际 %v", candidateIDs(got))
		}
	})

	t.Run("悬空 provider 引用", func(t *testing.T) {
		st := newFakeStore()
		st.addLink("m1", "ghost", "g-1", 1, true) // 没有注册该 provider

		_, err := New(st, newFakeProbe()).Candidates(context.Background(), "m1")
		if !errors.Is(err, model.ErrNotFound) {
			t.Fatalf("目录不一致应显式报错，实际 %v", err)
		}
	})
}

// TestCandidatesConcurrent 确认 Catalog 无共享可变状态：并发读同一目录时
// 结果一致且数据不串（配合 -race 运行）。
func TestCandidatesConcurrent(t *testing.T) {
	st := newFakeStore()
	st.addProvider("prov-a", model.ProtocolOpenAIChat, "")
	st.addProvider("prov-b", model.ProtocolAnthropic, "")
	st.addProvider("prov-c", model.ProtocolOpenAIChat, model.ProviderStatusDisabled)
	st.addLink("m1", "prov-a", "a-1", 1, true)
	st.addLink("m1", "prov-c", "c-1", 0, true) // 优先级最高但 provider 停用
	st.addLink("m1", "prov-b", "b-1", 2, true)

	c := New(st, newFakeProbe())

	const workers = 32
	var wg sync.WaitGroup
	errs := make(chan error, workers*2)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				got, err := c.Candidates(context.Background(), "m1")
				if err != nil {
					errs <- err
					return
				}
				if ids := candidateIDs(got); !reflect.DeepEqual(ids, []string{"prov-a/a-1", "prov-b/b-1"}) {
					errs <- fmt.Errorf("并发结果不一致: %v", ids)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

// TestPublicModelsOnlyEnabled 确认对外模型列表只含 enabled 条目，且字段原样保留。
func TestPublicModelsOnlyEnabled(t *testing.T) {
	st := newFakeStore()
	st.models = []model.ModelSpec{
		{ModelID: "gpt-x", DisplayName: "GPT X", CapabilitiesJSON: `{"stream":true}`, Enabled: true, CreatedAt: 100, UpdatedAt: 200},
		{ModelID: "retired", DisplayName: "已下线", CapabilitiesJSON: `{}`, Enabled: false, CreatedAt: 1, UpdatedAt: 2},
		{ModelID: "claude-y", DisplayName: "Claude Y", CapabilitiesJSON: `{"vision":true}`, Enabled: true, CreatedAt: 3, UpdatedAt: 4},
	}

	got, err := New(st, nil).PublicModels(context.Background())
	if err != nil {
		t.Fatalf("PublicModels 返回错误: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("只应返回 enabled 模型，实际 %+v", got)
	}
	if got[0].ModelID != "claude-y" || got[1].ModelID != "gpt-x" {
		t.Fatalf("期望按 ModelID 稳定排序，实际 %s, %s", got[0].ModelID, got[1].ModelID)
	}
	if got[1].DisplayName != "GPT X" || got[1].CapabilitiesJSON != `{"stream":true}` ||
		got[1].CreatedAt != 100 || got[1].UpdatedAt != 200 || !got[1].Enabled {
		t.Fatalf("公开字段应原样保留，实际 %+v", got[1])
	}
}

// TestPublicModelsFiltersEnabledWhenStoreIgnoresFlag 确认底层忽略 onlyEnabled 时
// 本包仍不暴露停用模型（对外可见性的最终责任在本包）。
func TestPublicModelsFiltersEnabledWhenStoreIgnoresFlag(t *testing.T) {
	st := newFakeStore()
	st.models = []model.ModelSpec{
		{ModelID: "on", Enabled: true},
		{ModelID: "off", Enabled: false},
	}
	// 用一个无视 onlyEnabled 的底层实现模拟实现缺陷。
	sloppy := &sloppyStore{inner: st}

	got, err := New(sloppy, nil).PublicModels(context.Background())
	if err != nil {
		t.Fatalf("PublicModels 返回错误: %v", err)
	}
	if len(got) != 1 || got[0].ModelID != "on" {
		t.Fatalf("停用模型不得对外暴露，实际 %+v", got)
	}
}

// sloppyStore 故意忽略 ListModels 的 onlyEnabled 参数。
type sloppyStore struct{ inner *fakeStore }

func (s *sloppyStore) ListProviderModels(ctx context.Context, modelID string) ([]model.ProviderModel, error) {
	return s.inner.ListProviderModels(ctx, modelID)
}

func (s *sloppyStore) GetProvider(ctx context.Context, id string) (model.ProviderSpec, error) {
	return s.inner.GetProvider(ctx, id)
}

func (s *sloppyStore) ListModels(ctx context.Context, _ bool) ([]model.ModelSpec, error) {
	return s.inner.ListModels(ctx, false)
}

// TestPublicModelProjectionIsWhitelisted 用反射守住「逐字段白名单」：
// model.ModelSpec 新增任何字段都必须先在上面的允许清单里确认为可公开，
// 否则本测试失败 —— 防止内部字段（密钥引用、上游配置等）被顺手带出去。
func TestPublicModelProjectionIsWhitelisted(t *testing.T) {
	// model.ModelSpec 的全部字段。新增字段时本测试会失败，这正是设计意图：
	// 必须先确认新字段可对外公开，再把它加进 publicModel 的投影与下面的清单。
	allowed := map[string]bool{
		"ModelID":          true,
		"DisplayName":      true,
		"CapabilitiesJSON": true,
		"Enabled":          true,
		"CreatedAt":        true,
		"UpdatedAt":        true,
	}

	rt := reflect.TypeOf(model.ModelSpec{})
	for i := 0; i < rt.NumField(); i++ {
		name := rt.Field(i).Name
		if !allowed[name] {
			t.Fatalf("model.ModelSpec 出现了未经确认的可公开字段 %q："+
				"请确认它不是内部字段，再同步 publicModel 与本清单", name)
		}
	}

	// 投影必须逐字段落实：publicModel 的结果与允许清单完全一致。
	got := reflect.TypeOf(publicModel(model.ModelSpec{}))
	if got.NumField() != len(allowed) {
		t.Fatalf("投影字段数 %d 与允许清单 %d 不一致", got.NumField(), len(allowed))
	}
	for i := 0; i < got.NumField(); i++ {
		if !allowed[got.Field(i).Name] {
			t.Fatalf("投影出现未在白名单中的字段 %q", got.Field(i).Name)
		}
	}
}

// TestPublicModelsResponseCarriesNoInternalFields 用序列化结果断言：/v1/models
// 的响应体里不得出现任何内部字段名或密钥。
func TestPublicModelsResponseCarriesNoInternalFields(t *testing.T) {
	st := newFakeStore()
	st.addProvider("prov-a", model.ProtocolOpenAIChat, "")
	st.addLink("gpt-x", "prov-a", "upstream-secret-name", 1, true)
	st.models = []model.ModelSpec{
		{ModelID: "gpt-x", DisplayName: "GPT X", CapabilitiesJSON: `{"stream":true}`, Enabled: true},
	}

	c := New(st, nil)
	models, err := c.PublicModels(context.Background())
	if err != nil {
		t.Fatalf("PublicModels 返回错误: %v", err)
	}
	body, err := json.Marshal(models)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}

	forbidden := []string{
		"APIKeyRef", "api_key_ref", "secretref:", "ConfigJSON", "config_json",
		"KeyHash", "PasswordHash", "Endpoint", "http://", "https://",
		"UpstreamModelID", "upstream-secret-name", "Priority", "Weight", "Status",
	}
	for _, bad := range forbidden {
		if strings.Contains(string(body), bad) {
			t.Fatalf("/v1/models 响应体泄漏内部内容 %q: %s", bad, body)
		}
	}

	// 反向校验：同一份数据里的 Candidate 确实带内部字段 —— 说明上面的字符串检查
	// 有实际检出力，而不是「怎么都不会命中」。
	cands, err := c.Candidates(context.Background(), "gpt-x")
	if err != nil {
		t.Fatalf("Candidates 返回错误: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("期望 1 个候选，实际 %d", len(cands))
	}
	candJSON, err := json.Marshal(cands[0])
	if err != nil {
		t.Fatalf("序列化候选失败: %v", err)
	}
	if !strings.Contains(string(candJSON), "secretref:") || !strings.Contains(string(candJSON), "ConfigJSON") {
		t.Fatalf("Candidate 应携带内部字段供 upstream 使用，实际 %s", candJSON)
	}
}

// TestPublicModelsPropagatesStoreError 确认目录读取失败上抛而非返回空列表。
func TestPublicModelsPropagatesStoreError(t *testing.T) {
	sentinel := errors.New("db: busy")
	st := newFakeStore()
	st.modelErr = sentinel

	got, err := New(st, nil).PublicModels(context.Background())
	if !errors.Is(err, sentinel) {
		t.Fatalf("期望包装后仍可 Is(sentinel)，实际 %v", err)
	}
	if got != nil {
		t.Fatalf("出错时应返回 nil，实际 %+v", got)
	}
}
