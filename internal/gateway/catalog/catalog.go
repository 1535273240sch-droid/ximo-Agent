// Package catalog 实现模型目录与候选路由（XIMO 中转站文档 §5.3 / §7）。
//
// 职责边界：本包**只读**目录数据并做「哪些上游可用 + 按什么顺序试」的判定，
// 不写任何表、不解析密钥、不发网络请求。上游客户端的构建与凭据解析属于
// internal/gateway/upstream；Provider 健康度由 upstream 的熔断器经 Probe 注入。
package catalog

import (
	"context"
	"fmt"
	"sort"

	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/model"
)

// store 是本包用到的数据访问能力（窄接口，Go 惯例：接口由消费方定义）。
// gwstore.Store 的同名方法签名与冻结契约逐字一致，可直接传入 New。
type store interface {
	// ListProviderModels 返回某模型已配置的全部上游映射（含 enabled=false 的行，
	// 由本包过滤 —— 后台需要看到被停用的映射，故不能要求底层只回 enabled）。
	ListProviderModels(ctx context.Context, modelID string) ([]model.ProviderModel, error)
	// GetProvider 按 ID 读 provider 条目；未找到返回 model.ErrNotFound。
	GetProvider(ctx context.Context, id string) (model.ProviderSpec, error)
	// ListModels 列模型目录；onlyEnabled 为 true 时只回 enabled。
	ListModels(ctx context.Context, onlyEnabled bool) ([]model.ModelSpec, error)
}

// Probe 由 upstream 包实现并注入：Healthy 返回 false 表示该 provider 当前
// 熔断打开或已被判定不可用。候选尝试失败后由调用方调用 upstream 的
// MarkFailure / MarkSuccess 更新该状态。
type Probe interface {
	Healthy(providerID string) bool
}

// Candidate 是某个模型的一个可尝试上游。
//
// Provider 携带 APIKeyRef / ConfigJSON 等内部字段 —— 这是给 upstream 解析凭据与
// 构建客户端用的，**禁止**直接序列化给客户端（对外投影见 PublicModels）。
type Candidate struct {
	Provider      model.ProviderSpec
	UpstreamModel string
}

// Catalog 是只读的目录视图，不含可变状态，可被多个请求 goroutine 并发使用。
type Catalog struct {
	st    store
	probe Probe
}

// New 构造目录。st 不可为 nil；probe 允许为 nil —— 未注入探针时视为「全部健康」
// （启动早期 upstream 池尚未就绪，此时按不出故障处理比让网关整体不可用更合理）。
func New(st store, probe Probe) *Catalog {
	return &Catalog{st: st, probe: probe}
}

// Candidates 返回 modelID 当前可用的上游候选，按优先级从高到低排序。
//
// 过滤条件（按判定开销从低到高）：
//  1. provider_models.enabled = false
//  2. Provider.Status 不是 enabled（model.ProviderStatusEnabled）
//  3. Probe.Healthy(providerID) = false（熔断打开 / 已被判定不健康）
//
// 排序：Priority 升序（**数值越小优先级越高**，0 最先试，与 0003 迁移里
// idx_gw_provider_models_model(model_id, enabled, priority) 的索引序一致），
// 同优先级按 ProviderID 升序兜底，保证同一份目录数据每次返回的顺序一致
// （上游失败切换时不会在两个同优先级候选之间抖动）。
//
// 返回空切片且 err == nil 表示「当前没有可用上游」：模型不存在、模型没有任何
// 上游映射、或全部候选被上述条件过滤掉。调用方据此回 model_not_found /
// provider_unavailable，而不是把空候选当内部错误。空切片非 nil，便于直接序列化。
//
// 已知契约缺口：§7 提到「协议不匹配者」也应过滤，但 Candidates 没有「本次请求
// 要求的协议」入参，本包无从判定；请调用方按 Candidate.Provider.Protocol 与
// 请求所需协议自行比对后再试用。
func (c *Catalog) Candidates(ctx context.Context, modelID string) ([]Candidate, error) {
	if modelID == "" {
		// 空模型 ID 等价于「模型不存在」：不查库、不报错，交由调用方回 model_not_found。
		return []Candidate{}, nil
	}

	links, err := c.st.ListProviderModels(ctx, modelID)
	if err != nil {
		return nil, fmt.Errorf("catalog: 读取模型 %q 的上游映射失败: %w", modelID, err)
	}

	// 优先级在 provider_models 行上，不在 Provider 里，排序前先随候选一起带上。
	type ranked struct {
		cand     Candidate
		priority int64
	}
	rankedOut := make([]ranked, 0, len(links))

	for _, pm := range links {
		if !pm.Enabled {
			continue
		}
		if c.probe != nil && !c.probe.Healthy(pm.ProviderID) {
			continue
		}

		p, err := c.st.GetProvider(ctx, pm.ProviderID)
		if err != nil {
			// 不静默跳过：provider_models 指向不存在的 provider 说明目录数据不一致，
			// 掩盖它只会让路由凭空少一条候选而无人发现。
			return nil, fmt.Errorf("catalog: 读取 provider %q 失败: %w", pm.ProviderID, err)
		}
		if p.Status != model.ProviderStatusEnabled {
			continue
		}

		rankedOut = append(rankedOut, ranked{
			cand:     Candidate{Provider: p, UpstreamModel: pm.UpstreamModelID},
			priority: pm.Priority,
		})
	}

	// gw_provider_models 主键是 (provider_id, model_id)，同一模型下 ProviderID 唯一，
	// 因此 (priority, ProviderID) 已构成全序 —— 排序结果与行输入顺序无关。
	sort.SliceStable(rankedOut, func(i, j int) bool {
		a, b := rankedOut[i], rankedOut[j]
		if a.priority != b.priority {
			return a.priority < b.priority
		}
		return a.cand.Provider.ID < b.cand.Provider.ID
	})

	out := make([]Candidate, 0, len(rankedOut))
	for _, r := range rankedOut {
		out = append(out, r.cand)
	}
	return out, nil
}

// PublicModels 返回 /v1/models 可公开的模型目录：只回 enabled 的条目。
//
// 逐字段白名单：输出经 publicModel 重建，只带 ModelID / DisplayName /
// CapabilitiesJSON / Enabled / CreatedAt / UpdatedAt —— 全是可对外字段。
// 该投影由 TestPublicModelProjectionIsWhitelisted 用反射守住：model.ModelSpec
// 一旦新增字段而未被确认为可公开，测试即失败，避免内部字段（密钥引用、上游配置等）
// 被顺手带出去。
func (c *Catalog) PublicModels(ctx context.Context) ([]model.ModelSpec, error) {
	all, err := c.st.ListModels(ctx, true)
	if err != nil {
		return nil, fmt.Errorf("catalog: 读取模型目录失败: %w", err)
	}

	out := make([]model.ModelSpec, 0, len(all))
	for _, m := range all {
		// 双保险：即使底层实现忽略 onlyEnabled，也不把停用模型暴露出去。
		if !m.Enabled {
			continue
		}
		out = append(out, publicModel(m))
	}

	// 底层未声明排序时保证对外列表稳定，便于客户端缓存与比对。
	sort.SliceStable(out, func(i, j int) bool { return out[i].ModelID < out[j].ModelID })

	return out, nil
}

// publicModel 把目录条目重建为可公开字段的投影（逐字段白名单）。
func publicModel(m model.ModelSpec) model.ModelSpec {
	return model.ModelSpec{
		ModelID:          m.ModelID,
		DisplayName:      m.DisplayName,
		CapabilitiesJSON: m.CapabilitiesJSON,
		Enabled:          true, // 只投影 enabled 条目，这里是常量而非拷贝
		CreatedAt:        m.CreatedAt,
		UpdatedAt:        m.UpdatedAt,
	}
}
