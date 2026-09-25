package expert

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/provider"
)

// ---------------------------------------------------------------------------
// 测试替身：可编排失败序列的 Provider 与假模型池
// ---------------------------------------------------------------------------

// scriptedProvider 按脚本返回：errors 非空时先逐个返回错误，随后返回
// responses（或默认成功回答）。
type scriptedProvider struct {
	mu        sync.Mutex
	errs      []error
	responses []provider.CompletionResponse
	models    []string // 每次调用看到的 Model
}

func (f *scriptedProvider) Complete(_ context.Context, req provider.CompletionRequest) (provider.CompletionResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.models = append(f.models, req.Model)
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		return provider.CompletionResponse{}, err
	}
	if len(f.responses) > 0 {
		resp := f.responses[0]
		f.responses = f.responses[1:]
		return resp, nil
	}
	return provider.CompletionResponse{FinishReason: provider.FinishStop, Content: "默认回答"}, nil
}

func (f *scriptedProvider) Stream(context.Context, provider.CompletionRequest) (<-chan provider.StreamChunk, error) {
	ch := make(chan provider.StreamChunk, 1)
	ch <- provider.StreamChunk{Done: true}
	close(ch)
	return ch, nil
}

func (f *scriptedProvider) Name() string         { return "scripted" }
func (f *scriptedProvider) ContextWindow() int   { return 100000 }
func (f *scriptedProvider) MaxOutputTokens() int { return 8192 }

func (f *scriptedProvider) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.models)
}

func (f *scriptedProvider) calledModels() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.models...)
}

// fakeModelPool 实现 expert.ModelPool：按 targets 顺序返回未排除的候选，
// 并记录每次 Select 的参数供断言。
type fakeModelPool struct {
	mu       sync.Mutex
	targets  []provider.Target
	orders   [][]string
	excludes [][]string
}

func (f *fakeModelPool) Select(order []string, exclude ...string) (provider.Target, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.orders = append(f.orders, append([]string(nil), order...))
	f.excludes = append(f.excludes, append([]string(nil), exclude...))
	// 与真池一致的语义：给定了顺序就按顺序找；没给就按登记顺序轮询。
	if len(order) > 0 {
		for _, want := range order {
			for _, t := range f.targets {
				if t.ID != want {
					continue
				}
				excluded := false
				for _, id := range exclude {
					if id == t.ID {
						excluded = true
						break
					}
				}
				if !excluded {
					return t, true
				}
			}
		}
		return provider.Target{}, false
	}
	for _, t := range f.targets {
		excluded := false
		for _, id := range exclude {
			if id == t.ID {
				excluded = true
				break
			}
		}
		if !excluded {
			return t, true
		}
	}
	return provider.Target{}, false
}

func (f *fakeModelPool) FailoverLimit() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.targets)
}

func (f *fakeModelPool) selectCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.excludes)
}

func (f *fakeModelPool) lastExclude() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.excludes) == 0 {
		return nil
	}
	return f.excludes[len(f.excludes)-1]
}

func err429() error {
	return &provider.APIError{StatusCode: 429, Code: "rate_limit", Message: "too many requests"}
}

func err400() error {
	return &provider.APIError{StatusCode: 400, Code: "invalid_request_error", Message: "bad request"}
}

// blockingTimeoutProvider 模拟「API 调用进行到一半时子代理的墙钟到期」：
// Complete 挂起直到 ctx 被取消，然后原样返回裸的 DeadlineExceeded —— 与真实
// HTTP 客户端被 runCtx 掐断时的错误形态一致（未包 ClassifiedError）。
type blockingTimeoutProvider struct {
	mu    sync.Mutex
	calls int
}

func (p *blockingTimeoutProvider) Complete(ctx context.Context, _ provider.CompletionRequest) (provider.CompletionResponse, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	<-ctx.Done()
	return provider.CompletionResponse{}, ctx.Err()
}

func (p *blockingTimeoutProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func (p *blockingTimeoutProvider) Stream(context.Context, provider.CompletionRequest) (<-chan provider.StreamChunk, error) {
	ch := make(chan provider.StreamChunk, 1)
	ch <- provider.StreamChunk{Done: true}
	close(ch)
	return ch, nil
}

func (p *blockingTimeoutProvider) Name() string         { return "blocking-timeout" }
func (p *blockingTimeoutProvider) ContextWindow() int   { return 100000 }
func (p *blockingTimeoutProvider) MaxOutputTokens() int { return 8192 }

// ---------------------------------------------------------------------------
// 失败转移
// ---------------------------------------------------------------------------

// TestSubAgentFailsOverOnRateLimit 锁死验收标准 2 的核心：第一个候选 429，
// 自动换第二个候选把同一个任务跑完，并在事件里说明原因。
func TestSubAgentFailsOverOnRateLimit(t *testing.T) {
	first := &scriptedProvider{errs: []error{err429()}}
	second := &scriptedProvider{responses: []provider.CompletionResponse{
		{FinishReason: provider.FinishStop, Content: "备用模型完成了任务"},
	}}
	pool := &fakeModelPool{targets: []provider.Target{
		{ID: "main", Model: "model-main", Provider: first},
		{ID: "backup", Model: "model-backup", Provider: second},
	}}

	res, err := RunSubAgent(context.Background(), SubAgentRequest{
		SystemPrompt: "你是专家",
		Task:         "同一个任务",
		Options:      SubAgentOptions{Pool: pool},
	})
	if err != nil {
		t.Fatalf("失败转移后应成功: %v", err)
	}
	if res.Content != "备用模型完成了任务" {
		t.Fatalf("Content = %q", res.Content)
	}
	// 同一个任务、同一个 expert：主候选只被调了一次就放弃，没有死磕。
	if got := first.callCount(); got != 1 {
		t.Fatalf("主候选被调用 %d 次, want 1（内部重试交给 provider 层）", got)
	}
	// Model 的取值来源是池选出来的候选，而不是写死的全局配置。
	if models := second.calledModels(); len(models) != 1 || models[0] != "model-backup" {
		t.Fatalf("备用候选应收到池分配的模型, got %v", models)
	}
	// 事件里要能看出「为什么慢了」。
	found := false
	for _, ev := range res.Events {
		if strings.Contains(ev.Detail, "main") && strings.Contains(ev.Detail, "backup") {
			found = true
		}
	}
	if !found {
		t.Fatalf("缺少失败转移说明事件: %+v", res.Events)
	}
}

// TestSubAgentFailsOverHonorCandidateOrder 专家/分类分配的顺序要生效。
func TestSubAgentFailsOverHonorCandidateOrder(t *testing.T) {
	first := &scriptedProvider{responses: []provider.CompletionResponse{
		{FinishReason: provider.FinishStop, Content: "ok"},
	}}
	second := &scriptedProvider{}
	pool := &fakeModelPool{targets: []provider.Target{
		{ID: "a", Model: "ma", Provider: first},
		{ID: "b", Model: "mb", Provider: second},
	}}

	_, err := RunSubAgent(context.Background(), SubAgentRequest{
		Task:    "任务",
		Options: SubAgentOptions{Pool: pool, CandidateOrder: []string{"b", "a"}},
	})
	if err != nil {
		t.Fatalf("报错: %v", err)
	}
	pool.mu.Lock()
	order := pool.orders[0]
	pool.mu.Unlock()
	if len(order) != 2 || order[0] != "b" || order[1] != "a" {
		t.Fatalf("候选顺序未传递给池: %v", order)
	}
	if models := second.calledModels(); len(models) != 1 || models[0] != "mb" {
		t.Fatalf("应优先使用分配列表里的 b, got %v", models)
	}
}

// TestSubAgentNoFailoverOnDeterministicError 确定性错误（400/密钥/schema/取消/
// 子代理超时）换谁都会再犯，绝不转移。
func TestSubAgentNoFailoverOnDeterministicError(t *testing.T) {
	cases := map[string]error{
		"400":              err400(),
		"invalid_key":      &provider.APIError{StatusCode: 401, Message: "invalid api key"},
		"context_too_long": &provider.APIError{StatusCode: 400, Message: "context length exceeded"},
		"cancelled":        context.Canceled,
		"sub_timeout":      fmt.Errorf("expert: %w", ErrSubAgentTimeout),
	}
	for name, cause := range cases {
		t.Run(name, func(t *testing.T) {
			first := &scriptedProvider{errs: []error{cause}}
			pool := &fakeModelPool{targets: []provider.Target{
				{ID: "main", Model: "m1", Provider: first},
				{ID: "backup", Model: "m2", Provider: &scriptedProvider{}},
			}}

			_, err := RunSubAgent(context.Background(), SubAgentRequest{
				Task:    "任务",
				Options: SubAgentOptions{Pool: pool},
			})
			if err == nil {
				t.Fatalf("%s 应该失败而不是被吞掉", name)
			}
			if got := pool.selectCount(); got != 1 {
				t.Fatalf("%s 不应触发失败转移, Select 被调用 %d 次", name, got)
			}
		})
	}
}

// TestSubAgentNoFailoverOnMidCallTimeout API 调用进行到一半时子代理超时到期：
// 不转移。provider 会把裸的 DeadlineExceeded 分类成「连接超时」（可转移类），
// 若不归一成 ErrSubAgentTimeout，每个候选都会把整个任务从头重跑一遍
// （最坏 N×超时上限），且大概率再次超时。
func TestSubAgentNoFailoverOnMidCallTimeout(t *testing.T) {
	first := &blockingTimeoutProvider{}
	backup := &scriptedProvider{responses: []provider.CompletionResponse{
		{FinishReason: provider.FinishStop, Content: "不应到达"},
	}}
	pool := &fakeModelPool{targets: []provider.Target{
		{ID: "main", Model: "m1", Provider: first},
		{ID: "backup", Model: "m2", Provider: backup},
	}}

	res, err := RunSubAgent(context.Background(), SubAgentRequest{
		Task:    "慢任务",
		Options: SubAgentOptions{Pool: pool, Timeout: 40 * time.Millisecond},
	})
	if !errors.Is(err, ErrSubAgentTimeout) {
		t.Fatalf("中途到期的超时应归一为 ErrSubAgentTimeout, got %v", err)
	}
	if got := pool.selectCount(); got != 1 {
		t.Fatalf("中途超时不应触发失败转移, Select 被调用 %d 次", got)
	}
	if got := backup.callCount(); got != 0 {
		t.Fatalf("备用候选不应被调用, got %d", got)
	}
	if res == nil {
		t.Fatalf("失败时也应返回已收集的事件供排查")
	}
}

// TestSubAgentFailoverExhaustsPool 所有候选都 429：轮询一遍后失败，
// 上限是候选数，不会死循环。
func TestSubAgentFailoverExhaustsPool(t *testing.T) {
	targets := make([]provider.Target, 0, 3)
	provs := make([]*scriptedProvider, 0, 3)
	for _, id := range []string{"a", "b", "c"} {
		p := &scriptedProvider{errs: []error{err429()}}
		provs = append(provs, p)
		targets = append(targets, provider.Target{ID: id, Model: "m-" + id, Provider: p})
	}
	pool := &fakeModelPool{targets: targets}

	res, err := RunSubAgent(context.Background(), SubAgentRequest{
		Task:    "任务",
		Options: SubAgentOptions{Pool: pool},
	})
	if err == nil {
		t.Fatalf("全部候选失败时应报错")
	}
	if res == nil {
		t.Fatalf("失败时也应返回已收集的事件供排查")
	}
	if got := pool.selectCount(); got != 3 {
		t.Fatalf("应恰好轮询一遍（3 次）, got %d", got)
	}
	for i, id := range []string{"a", "b", "c"} {
		if got := provs[i].callCount(); got != 1 {
			t.Fatalf("候选 %s 被调用 %d 次, want 1", id, got)
		}
	}
	// 最后一轮的排除集合应包含前面所有候选。
	if last := pool.lastExclude(); len(last) != 2 {
		t.Fatalf("最后一次 Select 的排除集合应含前两个候选, got %v", last)
	}
}

// TestSubAgentMaxFailoversCap MaxFailovers 覆盖轮询上限。
func TestSubAgentMaxFailoversCap(t *testing.T) {
	targets := []provider.Target{
		{ID: "a", Model: "ma", Provider: &scriptedProvider{errs: []error{err429()}}},
		{ID: "b", Model: "mb", Provider: &scriptedProvider{errs: []error{err429()}}},
		{ID: "c", Model: "mc", Provider: &scriptedProvider{errs: []error{err429()}}},
	}
	pool := &fakeModelPool{targets: targets}

	_, err := RunSubAgent(context.Background(), SubAgentRequest{
		Task:    "任务",
		Options: SubAgentOptions{Pool: pool, MaxFailovers: 1},
	})
	if err == nil {
		t.Fatal("应失败")
	}
	if got := pool.selectCount(); got != 1 {
		t.Fatalf("MaxFailovers=1 时只应尝试 1 次, got %d", got)
	}
}

// TestSubAgentWithoutPoolUnchanged 未配置池时行为与改动前完全一致：
// Provider/Model 原样使用，失败即失败。
func TestSubAgentWithoutPoolUnchanged(t *testing.T) {
	cause := err429()
	p := &scriptedProvider{errs: []error{cause}}
	_, err := RunSubAgent(context.Background(), SubAgentRequest{
		SystemPrompt: "你是专家",
		Task:         "任务",
		Options:      SubAgentOptions{Provider: p, Model: "m"},
	})
	if err == nil {
		t.Fatal("无池时应原样失败")
	}
	if !errors.Is(err, cause) {
		t.Fatalf("错误应保留原始分类: %v", err)
	}
	if models := p.calledModels(); len(models) != 1 || models[0] != "m" {
		t.Fatalf("应使用传入的 Model, got %v", models)
	}
}
