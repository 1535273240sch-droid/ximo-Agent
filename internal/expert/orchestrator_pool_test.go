package expert

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/provider"
	"github.com/ximo888ok-netizen/ximo-agent/internal/scheduler"
	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// ---------------------------------------------------------------------------
// 测试替身：带并发计量的 Provider
// ---------------------------------------------------------------------------

// timingProvider 记录同时在飞的请求数，并让每次调用停留一小段时间，
// 用于观察真实并发度。
type timingProvider struct {
	mu          sync.Mutex
	inFlight    int
	maxInFlight int
	hold        time.Duration
}

func (p *timingProvider) Complete(ctx context.Context, _ provider.CompletionRequest) (provider.CompletionResponse, error) {
	p.mu.Lock()
	p.inFlight++
	if p.inFlight > p.maxInFlight {
		p.maxInFlight = p.inFlight
	}
	p.mu.Unlock()

	if p.hold > 0 {
		select {
		case <-time.After(p.hold):
		case <-ctx.Done():
		}
	}

	p.mu.Lock()
	p.inFlight--
	p.mu.Unlock()
	return provider.CompletionResponse{FinishReason: provider.FinishStop, Content: "done"}, nil
}

func (p *timingProvider) Stream(context.Context, provider.CompletionRequest) (<-chan provider.StreamChunk, error) {
	ch := make(chan provider.StreamChunk, 1)
	ch <- provider.StreamChunk{Done: true}
	close(ch)
	return ch, nil
}

func (p *timingProvider) Name() string         { return "timing" }
func (p *timingProvider) ContextWindow() int   { return 100000 }
func (p *timingProvider) MaxOutputTokens() int { return 8192 }

func (p *timingProvider) peak() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.maxInFlight
}

// barrierProvider 在第 need 个并发请求到齐时统一放行 —— 用确定性手段证明
// 「真的并行了」，而不是靠时序碰运气。cap 不足时永远到不齐，兜底定时器
// 会让测试以失败告终而不是卡死。
type barrierProvider struct {
	timingProvider
	need        int
	arrived     int
	release     chan struct{}
	releaseOnce sync.Once
}

func newBarrierProvider(need int) *barrierProvider {
	b := &barrierProvider{need: need, release: make(chan struct{})}
	// 放行后让每个请求停留片刻：没有 hold 的话，四个 goroutine 在屏障后
	// 可能被调度器逐个跑完，重叠窗口量为零，「并行」就测不出来了。
	b.hold = 50 * time.Millisecond
	time.AfterFunc(3*time.Second, func() { b.releaseOnce.Do(func() { close(b.release) }) })
	return b
}

func (p *barrierProvider) Complete(ctx context.Context, req provider.CompletionRequest) (provider.CompletionResponse, error) {
	p.mu.Lock()
	p.arrived++
	reached := p.arrived >= p.need
	p.mu.Unlock()
	if reached {
		p.releaseOnce.Do(func() { close(p.release) })
	}
	select {
	case <-p.release:
	case <-ctx.Done():
	}
	return p.timingProvider.Complete(ctx, req)
}

// firstExpertIDs 从真实专家库取前 n 个专家 ID（注册表加载是确定性的）。
func firstExpertIDs(t *testing.T, n int) []string {
	t.Helper()
	list, err := NewRegistry(nil).Load()
	if err != nil {
		t.Fatalf("加载专家库失败: %v", err)
	}
	if len(list) < n {
		t.Fatalf("专家库不足 %d 位", n)
	}
	ids := make([]string, 0, n)
	for _, e := range list[:n] {
		ids = append(ids, e.ID)
	}
	return ids
}

// TestOrchestratorCapsConcurrentSubAgents 锁死验收标准 3 的上限半边：
// expert_agent 类的容量是同时在跑的子代理数的硬上限。
func TestOrchestratorCapsConcurrentSubAgents(t *testing.T) {
	const (
		capacity   = 2
		experts    = 6
		holdMillis = 40
	)
	resources := scheduler.NewResourcePool(
		map[types.ResourceClass]int{types.ResourceClassExpertAgent: capacity}, 16)
	p := &timingProvider{hold: holdMillis * time.Millisecond}
	o := NewOrchestrator(OrchestratorOptions{
		Registry:  NewRegistry(nil),
		Runner:    SubAgentOptions{Provider: p, Model: "m"},
		Resources: resources,
	})
	ids := firstExpertIDs(t, experts)

	var wg sync.WaitGroup
	errs := make([]error, experts)
	for i, id := range ids {
		wg.Add(1)
		go func(idx int, expertID string) {
			defer wg.Done()
			out, err := o.Activate(context.Background(), ExpertRequest{ExpertID: expertID, Task: "子任务"})
			if err != nil {
				errs[idx] = err
				return
			}
			if out.Error != "" {
				errs[idx] = context.DeadlineExceeded
			}
		}(i, id)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("第 %d 个专家激活失败: %v", i, err)
		}
	}
	if got := p.peak(); got != capacity {
		t.Fatalf("子代理峰值并发 = %d, want %d（超过即资源类失效，小于即被错误串行化）", got, capacity)
	}
	if got := resources.InUse(types.ResourceClassExpertAgent); got != 0 {
		t.Fatalf("全部结束后槽位应归还, InUse = %d", got)
	}
}

// TestOrchestratorRunsFourExpertsInParallel 锁死验收标准 3 的并行半边：
// 4 个不同专家的子任务同时触发时真的重叠执行（用屏障确定性验证）。
func TestOrchestratorRunsFourExpertsInParallel(t *testing.T) {
	const experts = 4 // 默认容量 8 以内
	resources := scheduler.NewResourcePool(nil, 16)
	p := newBarrierProvider(experts)
	o := NewOrchestrator(OrchestratorOptions{
		Registry:  NewRegistry(nil),
		Runner:    SubAgentOptions{Provider: p, Model: "m"},
		Resources: resources,
	})
	ids := firstExpertIDs(t, experts)

	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func(expertID string) {
			defer wg.Done()
			out, err := o.Activate(context.Background(), ExpertRequest{ExpertID: expertID, Task: "子任务"})
			if err != nil {
				t.Errorf("专家 %s 激活失败: %v", expertID, err)
				return
			}
			if out.Error != "" {
				t.Errorf("专家 %s 子任务失败: %s", expertID, out.Error)
			}
		}(id)
	}
	wg.Wait()

	if got := p.peak(); got != experts {
		t.Fatalf("4 个专家应并行执行, 峰值并发 = %d", got)
	}
	if got := resources.Capacity(types.ResourceClassExpertAgent); got != types.ResourceCapacities[types.ResourceClassExpertAgent] {
		t.Fatalf("默认容量应是 ResourceCapacities 里的 8, got %d", got)
	}
}

// TestOrchestratorInfoPathDoesNotConsumeSlot 无 task 的激活只返回信息，
// 不应占用子代理槽位 —— 池打满时它也必须立即可用。
func TestOrchestratorInfoPathDoesNotConsumeSlot(t *testing.T) {
	resources := scheduler.NewResourcePool(
		map[types.ResourceClass]int{types.ResourceClassExpertAgent: 1}, 16)
	// 人为占满唯一的槽位且不归还。
	lease, err := resources.Acquire(context.Background(), types.ResourceClassExpertAgent)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()

	o := NewOrchestrator(OrchestratorOptions{
		Registry:  NewRegistry(nil),
		Runner:    SubAgentOptions{Provider: &timingProvider{}, Model: "m"},
		Resources: resources,
	})
	ids := firstExpertIDs(t, 1)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	out, err := o.Activate(ctx, ExpertRequest{ExpertID: ids[0]})
	if err != nil {
		t.Fatalf("无 task 激活不应被资源闸门挡住: %v", err)
	}
	if out.Error != "" || out.SubAgentMode {
		t.Fatalf("无 task 激活应只返回信息: %+v", out)
	}
	if out.Content == "" {
		t.Fatalf("应返回专家信息")
	}
}

// TestOrchestratorWithoutResourcesUnchanged 未接资源池时保持既有行为：
// 不做任何并发限制，激活照常成功。
func TestOrchestratorWithoutResourcesUnchanged(t *testing.T) {
	o := NewOrchestrator(OrchestratorOptions{
		Registry: NewRegistry(nil),
		Runner:   SubAgentOptions{Provider: &timingProvider{}, Model: "m"},
	})
	ids := firstExpertIDs(t, 1)
	out, err := o.Activate(context.Background(), ExpertRequest{ExpertID: ids[0], Task: "子任务"})
	if err != nil {
		t.Fatalf("报错: %v", err)
	}
	if !out.SubAgentMode || out.Error != "" {
		t.Fatalf("子代理应正常完成: %+v", out)
	}
}
