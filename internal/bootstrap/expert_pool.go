package bootstrap

import (
	"fmt"
	"sync"

	"github.com/ximo888ok-netizen/ximo-agent/internal/config"
	"github.com/ximo888ok-netizen/ximo-agent/internal/expert"
	"github.com/ximo888ok-netizen/ximo-agent/internal/provider"
	"github.com/ximo888ok-netizen/ximo-agent/internal/scheduler"
)

// 本文件装配任务 05 的「多代理并行 + 独立资源类 + 模型池 + 失败转移」：
//
//	SubAgentPool       —— 按 config.ProviderPool() 构造子代理候选池
//	ExpertOrchestrator —— 把模型池、expert_agent 资源类与候选分配接进专家编排器
//
// 刻意做成懒构造 + 缓存失效：agent_expert 工具的注册属于任务 04 的范围，
// 装配层当前没有稳定的调用方；在 New() 里急切构造会让所有既有测试的启动
// 路径多出一段与它们无关的失败模式（例如池里某个候选 BaseURL 为空）。

// subAgentPoolState 缓存子代理模型池。设置页改服务商配置后会整体重建，
// 因此用互斥锁保护，而不是 sync.Once。
type subAgentPoolState struct {
	mu   sync.Mutex
	pool *provider.Pool
}

// SubAgentPool 返回子代理模型池。候选顺序即优先级：主服务商恒为第一项
// （config.ProviderPool 的约定），其后是设置面板配置的其余候选。
//
// 没有任何 BaseURL 可用的服务商时返回 nil —— 此时子代理无从发起模型调用，
// 专家激活会走到 RunSubAgent 的「缺少 Provider」错误并降级为手动指引。
func (a *App) SubAgentPool() (*provider.Pool, error) {
	if a == nil || a.cfg == nil {
		return nil, nil
	}
	a.subAgentPool.mu.Lock()
	defer a.subAgentPool.mu.Unlock()
	if a.subAgentPool.pool != nil {
		return a.subAgentPool.pool, nil
	}

	resolver := resolveSecretResolver(a.opts, a)
	var candidates []provider.PoolCandidate
	for _, pcfg := range a.cfg.ProviderPool() {
		if pcfg.BaseURL == "" {
			// 与 buildProvider 同规则：没有地址的服务商建不出客户端。
			continue
		}
		client, err := newProviderClient(pcfg, resolver)
		if err != nil {
			// 单个候选配置坏了不拖垮整个池：跳过它，健康候选照常服务。
			continue
		}
		candidates = append(candidates, provider.PoolCandidate{
			ID:     poolCandidateID(pcfg, len(candidates)),
			Model:  pcfg.Model,
			Client: client,
		})
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	a.subAgentPool.pool = provider.NewPool(candidates)
	return a.subAgentPool.pool, nil
}

// poolCandidateID 生成候选标识：优先配置 ID，缺失时按位置兜底，
// 保证失败转移的排除集合永远有稳定的主键。
func poolCandidateID(pcfg config.ProviderConfig, index int) string {
	if pcfg.ID != "" {
		return pcfg.ID
	}
	return fmt.Sprintf("provider-%d", index)
}

// invalidateSubAgentPool 让缓存的模型池失效。服务商配置（含密钥）变更后
// 必须重建，否则池里还拿着旧 BaseURL/旧密钥引用的客户端。
func (a *App) invalidateSubAgentPool() {
	if a == nil {
		return
	}
	a.subAgentPool.mu.Lock()
	defer a.subAgentPool.mu.Unlock()
	a.subAgentPool.pool = nil
}

// ExpertOrchestrator 返回接好「模型池 + expert_agent 资源类 + 候选分配」的
// 专家编排器，供 agent_expert 工具（任务 04 注册）调用。
//
// 资源槽位取自引擎调度器的同一个 ResourcePool —— 只有两边共用一个池，
// 「同时在跑的子代理数 ≤ 8」才是硬上限，而不是两个各 8 槽的半吊子池。
// 引擎未装配时资源类不生效（不限并发），模型池仍可用。
//
// 子代理的工具执行器（expert.SubAgentOptions.Executor）刻意留空：工具的
// 注册与权限判定随 agent_expert 工具一起由任务 04 装配；RunSubAgent 对
// nil 执行器的既有行为是明确回报「工具未注册」并继续，不会崩。
func (a *App) ExpertOrchestrator() (*expert.Orchestrator, error) {
	if a == nil {
		return nil, fmt.Errorf("bootstrap: app is nil")
	}
	pool, err := a.SubAgentPool()
	if err != nil {
		return nil, err
	}

	var resources *scheduler.ResourcePool
	if a.Engine != nil {
		if facade := a.Engine.Scheduler(); facade != nil {
			resources = facade.Resources()
		}
	}

	opts := expert.OrchestratorOptions{
		Runner: expert.SubAgentOptions{
			// Provider/Model 由池按候选顺序挑选；这里不写死全局配置。
			Pool: pool,
		},
		Resources: resources,
	}
	if pool != nil {
		opts.Allocator = func(e expert.Expert) []string {
			if a.cfg == nil {
				return nil
			}
			return a.cfg.SubAgentCandidates(e.ID, e.Division)
		}
	}
	return expert.NewOrchestrator(opts), nil
}
