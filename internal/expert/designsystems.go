// designsystems.go —— 设计系统静态资源（embed.FS 懒加载）。
//
// 对应 v1 src/main/tools/Design/design-systems/ —— 151 套设计系统，
// 每套含 DESIGN.md（设计语言说明）、manifest.json（元数据）、tokens.css（token 绑定）。
//
// 任务书把这些归入 internal/expert 的「设计系统等静态资源，embed.FS懒加载」。
// 与专家数据同一套加载策略：embed 进二进制 → 首次访问才解析 → 加载后只读。
//
// 为什么不全量预加载：151 套 × 约 18KB ≈ 2.8MB 文本，加上 254 位专家的 156KB。
// 进程启动时全部解析会让冷启动背上明显开销，而这些资源在多数会话里根本用不到。
package expert

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"sync"
)

// designSystemsRoot embed 中设计系统资源的根目录。
const designSystemsRoot = "assets/design-systems"

// DesignSystem 一套设计系统的元数据（manifest.json + 可选内容）。
type DesignSystem struct {
	// ID 目录名，如 "linear-app"。
	ID string `json:"id"`
	// Name 展示名（来自 manifest）。
	Name string `json:"name"`
	// Category 分类，如「效率与SaaS」。
	Category string `json:"category"`
	// Description 简介。
	Description string `json:"description"`

	// HasDesign 是否有 DESIGN.md。
	HasDesign bool `json:"has_design"`
	// HasTokens 是否有 tokens.css。
	HasTokens bool `json:"has_tokens"`
}

// designManifest 只解析我们关心的字段（不引整个 manifest 结构）。
type designManifest struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Category    string `json:"category"`
	Description string `json:"description"`
}

// DesignSystemRegistry 设计系统注册表，懒加载 + 只读。
type DesignSystemRegistry struct {
	loadOnce sync.Once
	loadErr  error

	mu      sync.RWMutex
	systems []DesignSystem
	byID    map[string]DesignSystem
}

// NewDesignSystemRegistry 构造注册表。
func NewDesignSystemRegistry() *DesignSystemRegistry {
	return &DesignSystemRegistry{}
}

// Load 加载（并缓存）全部设计系统元数据。
func (r *DesignSystemRegistry) Load() ([]DesignSystem, error) {
	r.loadOnce.Do(func() {
		entries, err := fs.ReadDir(designFS, designSystemsRoot)
		if err != nil {
			r.loadErr = fmt.Errorf("expert: 读取设计系统目录失败: %w", err)
			return
		}

		systems := make([]DesignSystem, 0, len(entries))
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			id := entry.Name()
			ds := DesignSystem{ID: id, Name: id}

			// manifest.json 提供展示名与分类。
			manifestPath := path.Join(designSystemsRoot, id, "manifest.json")
			if raw, err := designFS.ReadFile(manifestPath); err == nil {
				var m designManifest
				if json.Unmarshal(raw, &m) == nil {
					if m.Name != "" {
						ds.Name = m.Name
					}
					ds.Category = m.Category
					ds.Description = m.Description
					if m.ID != "" {
						ds.ID = m.ID
					}
				}
			}

			// 记录可选文件是否存在，供调用方判断能否取内容。
			if _, err := fs.Stat(designFS, path.Join(designSystemsRoot, id, "DESIGN.md")); err == nil {
				ds.HasDesign = true
			}
			if _, err := fs.Stat(designFS, path.Join(designSystemsRoot, id, "tokens.css")); err == nil {
				ds.HasTokens = true
			}

			systems = append(systems, ds)
		}
		sort.Slice(systems, func(i, j int) bool { return systems[i].ID < systems[j].ID })

		byID := make(map[string]DesignSystem, len(systems))
		for _, ds := range systems {
			byID[ds.ID] = ds
		}

		r.mu.Lock()
		r.systems = systems
		r.byID = byID
		r.mu.Unlock()
	})
	return r.snapshot(), r.loadErr
}

// Count 返回设计系统总数。
func (r *DesignSystemRegistry) Count() (int, error) {
	list, err := r.Load()
	if err != nil {
		return 0, err
	}
	return len(list), nil
}

// Get 按 ID 查找。
func (r *DesignSystemRegistry) Get(id string) (DesignSystem, bool) {
	if _, err := r.Load(); err != nil {
		return DesignSystem{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	ds, ok := r.byID[id]
	return ds, ok
}

// DesignDoc 返回某套设计系统的 DESIGN.md 全文。
func (r *DesignSystemRegistry) DesignDoc(id string) (string, error) {
	return r.readAsset(id, "DESIGN.md")
}

// TokensCSS 返回某套设计系统的 tokens.css 全文。
func (r *DesignSystemRegistry) TokensCSS(id string) (string, error) {
	return r.readAsset(id, "tokens.css")
}

// readAsset 读取设计系统目录下的指定文件。
//
// 防目录穿越：id 里若含路径分隔符或 ".."，直接拒绝（embed.FS 本身也不允许，
// 但显式校验让错误信息更清晰，也避免依赖底层实现细节）。
func (r *DesignSystemRegistry) readAsset(id, file string) (string, error) {
	if !validAssetID(id) {
		return "", fmt.Errorf("expert: 非法设计系统 ID: %q", id)
	}
	raw, err := designFS.ReadFile(path.Join(designSystemsRoot, id, file))
	if err != nil {
		return "", fmt.Errorf("expert: 读取 %s/%s 失败: %w", id, file, err)
	}
	return string(raw), nil
}

// Categories 返回全部分类及各自数量。
func (r *DesignSystemRegistry) Categories() (map[string]int, error) {
	list, err := r.Load()
	if err != nil {
		return nil, err
	}
	out := make(map[string]int)
	for _, ds := range list {
		cat := ds.Category
		if cat == "" {
			cat = "未分类"
		}
		out[cat]++
	}
	return out, nil
}

// ListByCategory 列出某分类下的设计系统；category 为空表示全部。
func (r *DesignSystemRegistry) ListByCategory(category string) ([]DesignSystem, error) {
	list, err := r.Load()
	if err != nil {
		return nil, err
	}
	if category == "" {
		return list, nil
	}
	out := make([]DesignSystem, 0, len(list))
	for _, ds := range list {
		if ds.Category == category {
			out = append(out, ds)
		}
	}
	return out, nil
}

// snapshot 返回列表副本。
func (r *DesignSystemRegistry) snapshot() []DesignSystem {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]DesignSystem, len(r.systems))
	copy(out, r.systems)
	return out
}

// validAssetID 校验资源 ID 只含安全字符。
func validAssetID(id string) bool {
	if id == "" || strings.Contains(id, "..") {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_':
		default:
			return false
		}
	}
	return true
}
