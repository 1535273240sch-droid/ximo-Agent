// Package expert 实现 254 位专家库与专家调度（两阶段编排协议）。
//
// 对应 v1：src/main/tools/Skill/（AgentExpertTool、expert-config、sub-agent）
// 与 src/shared/agents-raw.json（156KB 原始数据）。
//
// 与 v1 的关键差异（架构文档要求）：
//
//	v1 用 Vite 构建时内联把 agents-raw.json 打进 bundle（`import agentsRawData`），
//	v2 按第 23 章的「embed → lazy load → immutable」模式处理 —— 数据 embed 进二进制，
//	进程启动时不解析 JSON，首次访问时才加载，加载后只读。
//
// 这样做的收益：进程启动路径不背 156KB JSON 解析 + 254 个对象的堆分配；
// 只在真正用到专家功能时才付出这份成本。
package expert

import (
	"embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
)

//go:embed assets/agents-raw.json
var expertFS embed.FS

//go:embed assets/design-systems
var designFS embed.FS

// Expert 一位专家的定义。字段与 v1 AgentEntry 对齐。
type Expert struct {
	ID          string   `json:"id"`
	Division    string   `json:"division"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Emoji       string   `json:"emoji"`
	Vibe        string   `json:"vibe"`
	Personality string   `json:"personality"`
	Tools       []string `json:"tools"`
	Color       string   `json:"color"`

	// Custom 标记自定义专家（v1 从 CustomDesignStore 合并而来）。
	Custom bool `json:"custom,omitempty"`
}

// agentsRaw agents-raw.json 的顶层结构。
type agentsRaw struct {
	Agents []Expert `json:"agents"`
	Total  int      `json:"total"`
}

// Registry 专家注册表：内置 + 自定义，懒加载、加载后只读。
type Registry struct {
	loadOnce sync.Once
	loadErr  error

	builtin []Expert

	// 索引：id → Expert（含自定义覆盖后的最终视图）。
	mu   sync.RWMutex
	byID map[string]Expert
	all  []Expert

	// customStore 自定义专家持久化（任务 03 存储层提供；nil 时纯内置）。
	customStore CustomStore
}

// CustomStore 自定义专家持久化接口（由任务 03 的存储层实现）。
type CustomStore interface {
	// Load 返回全部自定义专家。
	Load() ([]Expert, error)
	// Save 保存（同 ID 覆盖）。
	Save(e Expert) error
	// Delete 删除；返回 false 表示不存在或为内置。
	Delete(id string) (bool, error)
}

// NewRegistry 构造注册表。
func NewRegistry(custom CustomStore) *Registry {
	return &Registry{customStore: custom}
}

// loadBuiltin 懒加载内置专家数据。
func (r *Registry) loadBuiltin() error {
	r.loadOnce.Do(func() {
		raw, err := expertFS.ReadFile("assets/agents-raw.json")
		if err != nil {
			r.loadErr = fmt.Errorf("expert: 读取 embed 专家数据失败: %w", err)
			return
		}
		var parsed agentsRaw
		if err := json.Unmarshal(raw, &parsed); err != nil {
			r.loadErr = fmt.Errorf("expert: 解析专家 JSON 失败: %w", err)
			return
		}
		r.builtin = parsed.Agents
		// raw 失去引用，交由 GC 回收（内存预算考量）。
		raw = nil
		_ = raw
	})
	return r.loadErr
}

// rebuild 重建 id 索引与列表视图（自定义同 ID 覆盖内置，与 v1 语义一致）。
func (r *Registry) rebuild(custom []Expert) {
	byID := make(map[string]Expert, len(r.builtin)+len(custom))

	// 先登记自定义，后写内置 —— 用「自定义优先」的一次遍历避免 O(n²) 去重。
	customIDs := make(map[string]struct{}, len(custom))
	for _, e := range custom {
		e.Custom = true
		customIDs[e.ID] = struct{}{}
	}

	all := make([]Expert, 0, len(r.builtin)+len(custom))
	for _, e := range r.builtin {
		if _, overridden := customIDs[e.ID]; overridden {
			continue // 被自定义专家覆盖，跳过内置版本
		}
		all = append(all, e)
		byID[e.ID] = e
	}
	for _, e := range custom {
		e.Custom = true
		byID[e.ID] = e
		all = append(all, e)
	}

	r.all = all
	r.byID = byID
}

// Load 加载并返回全部专家（内置 + 自定义）。
//
// 结果会被缓存：包注释承诺的「embed → lazy load → immutable」要求加载只付出
// 一次成本，而这里的调用方（Get / Search / Count / ListByDivision）在每次专家
// 激活、每次集群选人都要问一次。此前 Load 无条件重建 254 条索引，于是
// SaveCustom / DeleteCustom 里的「使缓存失效」成了死代码 —— 缓存从来没有生效过，
// 失效也就没有意义。
//
// 缓存判据是 r.all != nil（rebuild 必然写入非 nil 切片），失效由 SaveCustom /
// DeleteCustom 显式完成：只有这两个入口能改 customStore，所以不存在外部写入
// 导致缓存过期的问题。
func (r *Registry) Load() ([]Expert, error) {
	if err := r.loadBuiltin(); err != nil {
		return nil, err
	}

	// 快路径：已建好索引就直接复制一份返回。
	r.mu.RLock()
	if r.all != nil {
		out := make([]Expert, len(r.all))
		copy(out, r.all)
		r.mu.RUnlock()
		return out, nil
	}
	r.mu.RUnlock()

	// 慢路径：读自定义 + 重建索引，全程持写锁。
	//
	// 自定义读取必须在锁内完成：否则「读到旧列表」与并发 SaveCustom 的失效
	// 可以交错（A 读列表 → B 保存并失效 → A 拿锁重建），新建的自定义专家会被
	// 一次陈旧的 rebuild 覆盖掉，直到下一次失效才出现。storage.Load 是纯读，
	// 不会回调注册表，因此这里不构成死锁风险。
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.all == nil {
		var custom []Expert
		if r.customStore != nil {
			c, err := r.customStore.Load()
			if err != nil {
				// 自定义读取失败不应让内置专家不可用 —— 降级为纯内置。
				custom = nil
			} else {
				custom = c
			}
		}
		r.rebuild(custom)
	}
	out := make([]Expert, len(r.all))
	copy(out, r.all)
	return out, nil
}

// Get 按 ID 查找专家。
func (r *Registry) Get(id string) (Expert, bool) {
	if _, err := r.Load(); err != nil {
		return Expert{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.byID[id]
	return e, ok
}

// Count 返回专家总数。
func (r *Registry) Count() (int, error) {
	list, err := r.Load()
	if err != nil {
		return 0, err
	}
	return len(list), nil
}

// ListByDivision 列出某部门的专家；division 为空表示全部。
func (r *Registry) ListByDivision(division string) ([]Expert, error) {
	list, err := r.Load()
	if err != nil {
		return nil, err
	}
	if division == "" {
		return list, nil
	}
	out := make([]Expert, 0, len(list))
	for _, e := range list {
		if e.Division == division {
			out = append(out, e)
		}
	}
	return out, nil
}

// GroupByDivision 按部门分组，返回有序的部门名列表与映射。
func (r *Registry) GroupByDivision() ([]string, map[string][]Expert, error) {
	list, err := r.Load()
	if err != nil {
		return nil, nil, err
	}
	groups := make(map[string][]Expert)
	for _, e := range list {
		groups[e.Division] = append(groups[e.Division], e)
	}
	names := make([]string, 0, len(groups))
	for name := range groups {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, groups, nil
}

// Search 按关键词搜索专家（name / description / division / vibe）。
//
// 与 v1 AgentExpertTool search 分支的匹配字段一致。
func (r *Registry) Search(query string) ([]Expert, error) {
	list, err := r.Load()
	if err != nil {
		return nil, err
	}
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return nil, nil
	}
	out := make([]Expert, 0, 8)
	for _, e := range list {
		if strings.Contains(strings.ToLower(e.Name), q) ||
			strings.Contains(strings.ToLower(e.Description), q) ||
			strings.Contains(strings.ToLower(e.Division), q) ||
			strings.Contains(strings.ToLower(e.Vibe), q) {
			out = append(out, e)
		}
	}
	return out, nil
}

// SaveCustom 保存自定义专家。
func (r *Registry) SaveCustom(e Expert) error {
	if r.customStore == nil {
		return fmt.Errorf("expert: 未配置自定义专家存储")
	}
	if err := r.customStore.Save(e); err != nil {
		return err
	}
	// 使缓存失效，下次 Load 重建。
	r.mu.Lock()
	r.byID = nil
	r.all = nil
	r.mu.Unlock()
	return nil
}

// DeleteCustom 删除自定义专家。
func (r *Registry) DeleteCustom(id string) (bool, error) {
	if r.customStore == nil {
		return false, fmt.Errorf("expert: 未配置自定义专家存储")
	}
	ok, err := r.customStore.Delete(id)
	if err != nil || !ok {
		return ok, err
	}
	r.mu.Lock()
	r.byID = nil
	r.all = nil
	r.mu.Unlock()
	return true, nil
}
