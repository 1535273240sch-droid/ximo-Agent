package tool

import (
	"fmt"
	"sort"
	"sync"
)

// Registry 工具注册表。对应 v1 的 src/main/tools/ToolRegistry.ts，
// 但并发安全，且拒绝空名/重复静默覆盖之外的行为（覆盖需显式 Replace）。
type Registry interface {
	// Register 注册工具。同名工具已存在时返回错误（调用方可用 Replace 显式覆盖）。
	Register(t Tool) error
	// Replace 覆盖注册同名工具（MCP 工具覆盖懒加载工具等场景）。
	Replace(t Tool) error
	// Get 按名称查找工具。
	Get(name string) (Tool, bool)
	// Has 报告工具是否已注册。
	Has(name string) bool
	// Names 返回已注册工具名（排序，稳定输出）。
	Names() []string
	// Definitions 返回所有已注册工具的定义（排序）。
	Definitions() []ToolDefinition
	// Len 返回已注册工具数量。
	Len() int
}

// registry 是 Registry 的内存实现。
type registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

// NewRegistry 创建空注册表。
func NewRegistry() Registry {
	return &registry{tools: make(map[string]Tool)}
}

func (r *registry) Register(t Tool) error {
	if t == nil {
		return fmt.Errorf("tool: 不能注册 nil 工具")
	}
	name := t.Definition().Name
	if name == "" {
		return fmt.Errorf("tool: 工具定义缺少 name")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.tools[name]; exists {
		return fmt.Errorf("tool: 工具 %q 已注册", name)
	}
	r.tools[name] = t
	return nil
}

func (r *registry) Replace(t Tool) error {
	if t == nil {
		return fmt.Errorf("tool: 不能注册 nil 工具")
	}
	name := t.Definition().Name
	if name == "" {
		return fmt.Errorf("tool: 工具定义缺少 name")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[name] = t
	return nil
}

func (r *registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

func (r *registry) Has(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.tools[name]
	return ok
}

func (r *registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (r *registry) Definitions() []ToolDefinition {
	r.mu.RLock()
	defer r.mu.RUnlock()
	defs := make([]ToolDefinition, 0, len(r.tools))
	for _, t := range r.tools {
		defs = append(defs, t.Definition())
	}
	sort.Slice(defs, func(i, j int) bool { return defs[i].Name < defs[j].Name })
	return defs
}

func (r *registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.tools)
}

// ---------------------------------------------------------------------------
// 懒加载注册表（保留 v1 lazy-registry 的“按需实例化”思路）
// ---------------------------------------------------------------------------

// ToolGroup 工具模块组标识（对应 v1 lazy-registry 的 moduleFactories key）。
type ToolGroup string

const (
	GroupFileSystem  ToolGroup = "file_system"
	GroupGit         ToolGroup = "git"
	GroupKnowledge   ToolGroup = "knowledge"
	GroupWeb         ToolGroup = "web_intelligence"
	GroupTerminal    ToolGroup = "terminal"
	GroupBrowser     ToolGroup = "browser"
	GroupOffice      ToolGroup = "office"
	GroupMCP         ToolGroup = "mcp"
	GroupDynamicJS   ToolGroup = "dynamic_js"
	GroupSkill       ToolGroup = "skill"
	GroupVision      ToolGroup = "vision"
	GroupPlanSpec    ToolGroup = "plan_spec"
	GroupDesign      ToolGroup = "design"
	GroupComputerUse ToolGroup = "computer_use"
)

// ToolFactory 构造一组工具。工厂函数在组内工具首次被需要时才执行，
// 实现“启动零工具实例化，按模式按需加载”。
type ToolFactory func() ([]Tool, error)

// LazyRegistry 在 Registry 之上增加模块组懒加载：每个组只实例化一次，
// 之后命中缓存。对应 v1 的 ensureModeToolsLoaded。
type LazyRegistry struct {
	Registry

	mu      sync.Mutex
	factory map[ToolGroup]ToolFactory
	loaded  map[ToolGroup]bool
	loading map[ToolGroup]bool
	waiters map[ToolGroup][]chan struct{}
}

// NewLazyRegistry 创建懒加载注册表。
func NewLazyRegistry() *LazyRegistry {
	return &LazyRegistry{
		Registry: NewRegistry(),
		factory:  make(map[ToolGroup]ToolFactory),
		loaded:   make(map[ToolGroup]bool),
		loading:  make(map[ToolGroup]bool),
		waiters:  make(map[ToolGroup][]chan struct{}),
	}
}

// RegisterGroup 注册一个模块组的工厂函数。
func (r *LazyRegistry) RegisterGroup(group ToolGroup, factory ToolFactory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.factory[group] = factory
}

// Loaded 报告某个模块组是否已实例化。
func (r *LazyRegistry) Loaded(group ToolGroup) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.loaded[group]
}

// EnsureGroups 确保给定模块组全部完成实例化。并发调用同一组时只执行一次工厂，
// 其余调用等待其结果。工厂返回的错误会向上传播，且组保持未加载状态可重试。
func (r *LazyRegistry) EnsureGroups(groups ...ToolGroup) error {
	for _, group := range groups {
		if err := r.ensureGroup(group); err != nil {
			return err
		}
	}
	return nil
}

func (r *LazyRegistry) ensureGroup(group ToolGroup) error {
	r.mu.Lock()
	if r.loaded[group] {
		r.mu.Unlock()
		return nil
	}
	if r.loading[group] {
		ch := make(chan struct{})
		r.waiters[group] = append(r.waiters[group], ch)
		r.mu.Unlock()
		<-ch // 等待首个调用方完成实例化（goroutine 有明确退出路径：channel 关闭）
		// 重新检查状态：首个调用方可能失败。
		r.mu.Lock()
		ok := r.loaded[group]
		r.mu.Unlock()
		if ok {
			return nil
		}
		return fmt.Errorf("tool: 模块组 %q 实例化失败", group)
	}
	factory, ok := r.factory[group]
	if !ok {
		r.mu.Unlock()
		return fmt.Errorf("tool: 未知模块组 %q", group)
	}
	r.loading[group] = true
	r.mu.Unlock()

	tools, err := factory()

	r.mu.Lock()
	r.loading[group] = false
	if err == nil {
		for _, t := range tools {
			// 与 v1 一致：不覆盖已注册的同名工具（如 MCP 工具后注册的场景）。
			if !r.Registry.Has(t.Definition().Name) {
				if regErr := r.Registry.Register(t); regErr != nil {
					err = fmt.Errorf("tool: 注册模块组 %q 工具失败: %w", group, regErr)
					break
				}
			}
		}
	}
	if err == nil {
		r.loaded[group] = true
	}
	waiters := r.waiters[group]
	delete(r.waiters, group)
	r.mu.Unlock()

	for _, ch := range waiters {
		close(ch)
	}
	return err
}

// ModeGroups 模式 → 模块组列表（对应 v1 lazy-registry 的 modeModules）。
var ModeGroups = map[Mode][]ToolGroup{
	ModeCoding: {GroupFileSystem, GroupGit, GroupKnowledge, GroupWeb, GroupTerminal, GroupSkill, GroupPlanSpec},
	ModeOffice: {GroupFileSystem, GroupGit, GroupKnowledge, GroupWeb, GroupOffice, GroupSkill},
	ModeDesign: {GroupFileSystem, GroupKnowledge, GroupWeb, GroupDesign, GroupSkill},
}

// EnsureModeTools 确保某个模式所需的模块组已加载（对应 v1 的 ensureModeToolsLoaded）。
func (r *LazyRegistry) EnsureModeTools(mode Mode) error {
	groups, ok := ModeGroups[mode]
	if !ok {
		return nil
	}
	return r.EnsureGroups(groups...)
}
