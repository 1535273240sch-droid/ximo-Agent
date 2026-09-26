package spec

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// builtinFS 内嵌整个内置规格目录。
//
// go:embed 不能跨目录引用（写 "../specs" 直接编译失败），所以内置 JSON 必须落在
// internal/spec 下：internal/spec/specs/*.json。契约里"plugin/specs/ 放内置 JSON"
// 的位置约束因此改为「正文在 internal/spec/specs/，plugin/specs/README.md 指路」，
// 详见 plugin/specs/README.md。规格对外的加载入口（Builtin / Load）与物理位置无关，
// 调用方不需要知道这个差异。
//
//go:embed specs/*.json
var builtinFS embed.FS

// builtinDir 是内嵌目录名（相对 internal/spec）。
const builtinDir = "specs"

// UserSpecDirName 是用户自定义规格目录在 $HOME 下的相对路径：~/.ximo-plugin/specs。
const UserSpecDirName = ".ximo-plugin/specs"

// BuiltinFS 暴露内嵌规格文件系统，供 CLI 的 `adapters list --show-source` 之类使用。
func BuiltinFS() fs.FS { return builtinFS }

// Builtin 返回全部内置规格，按 ID 升序。任一内置 JSON 非法即报错（fail fast：
// 内置数据是我们自己写的，坏了必须在测试里就炸出来）。
func Builtin() ([]Spec, error) {
	return loadFS(builtinFS, builtinDir, "内置规格")
}

// UserDir 返回用户自定义规格目录：<home>/.ximo-plugin/specs。
// home 为空时取 os.UserHomeDir()；CLI 的 --home 就是从这里流进去的（便于测试）。
func UserDir(home string) string {
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	return filepath.Join(home, filepath.FromSlash(UserSpecDirName))
}

// Load 返回生效的规格集合 = 内置规格 + 用户自定义规格。
//
// 合并规则（契约：用户同名 ID 覆盖内置）：
//   - 用户目录不存在 → 只有内置；
//   - 同一 ID → 用户的那份**整体替换**内置的那份（不做字段级合并，避免出现
//     半内半外的缝合怪）；
//   - 新 ID → 追加；
//   - 结果按 ID 升序；同一目录内出现重复 ≥2 次 ID（或文件名与 id 不一致）报错。
//
// 用户目录里的文件必须是「一个文件 = 一个 Spec 对象」，文件名（去掉 .json）必须
// 与 spec.id 相同，未知字段会被拒绝（拼错字段名会立刻报错，而不是被静默忽略）。
func Load(home string) ([]Spec, error) {
	builtin, err := Builtin()
	if err != nil {
		return nil, err
	}
	user, err := LoadDir(UserDir(home))
	if err != nil {
		return nil, err
	}

	merged := make([]Spec, 0, len(builtin)+len(user))
	index := map[string]int{}
	for _, s := range builtin {
		index[s.ID] = len(merged)
		merged = append(merged, s)
	}
	for _, s := range user {
		if i, ok := index[s.ID]; ok {
			merged[i] = s // 用户覆盖内置
			continue
		}
		index[s.ID] = len(merged)
		merged = append(merged, s)
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].ID < merged[j].ID })
	return merged, nil
}

// LoadDir 加载并校验一个目录下的全部 *.json 规格。目录不存在返回 (nil, nil)。
// 非 .json 文件（例如 README.md）忽略；子目录忽略。
func LoadDir(dir string) ([]Spec, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("读取规格目录 %s 失败: %w", dir, err)
	}

	specs := make([]Spec, 0, len(entries))
	seen := map[string]string{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(name), ".json") || strings.HasPrefix(name, ".") {
			continue
		}
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("读取规格文件 %s 失败: %w", path, err)
		}
		s, err := parseSpec(data, path)
		if err != nil {
			return nil, err
		}
		if want := strings.TrimSuffix(name, filepath.Ext(name)); s.ID != want {
			return nil, fmt.Errorf("%s: 文件名必须与 id 一致（文件名 %q，id %q）", path, want, s.ID)
		}
		if prev, dup := seen[s.ID]; dup {
			return nil, fmt.Errorf("规格 id %q 在同一目录里重复：%s 与 %s", s.ID, prev, path)
		}
		seen[s.ID] = path
		specs = append(specs, s)
	}
	sort.Slice(specs, func(i, j int) bool { return specs[i].ID < specs[j].ID })
	return specs, nil
}

// LoadFile 加载单个规格文件（供 `adapters show --file` 之类的调试路径使用）。
func LoadFile(path string) (Spec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Spec{}, fmt.Errorf("读取规格文件 %s 失败: %w", path, err)
	}
	return parseSpec(data, path)
}

// loadFS 从内嵌文件系统里加载一个目录下的全部规格。
func loadFS(fsys fs.FS, dir, label string) ([]Spec, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("读取%s目录 %s 失败: %w", label, dir, err)
	}
	specs := make([]Spec, 0, len(entries))
	seen := map[string]string{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(name), ".json") {
			continue
		}
		path := dir + "/" + name
		data, err := fs.ReadFile(fsys, path)
		if err != nil {
			return nil, fmt.Errorf("读取%s文件 %s 失败: %w", label, path, err)
		}
		s, err := parseSpec(data, path)
		if err != nil {
			return nil, err
		}
		if want := strings.TrimSuffix(name, filepath.Ext(name)); s.ID != want {
			return nil, fmt.Errorf("%s: 文件名必须与 id 一致（文件名 %q，id %q）", path, want, s.ID)
		}
		if prev, dup := seen[s.ID]; dup {
			return nil, fmt.Errorf("%s 规格 id %q 重复：%s", label, s.ID, prev)
		}
		seen[s.ID] = path
		specs = append(specs, s)
	}
	if len(specs) == 0 {
		return nil, fmt.Errorf("%s目录 %s 里没有任何 *.json", label, dir)
	}
	sort.Slice(specs, func(i, j int) bool { return specs[i].ID < specs[j].ID })
	return specs, nil
}

// parseSpec 解析并校验一份规格。source 只用于错误信息（可以是磁盘路径或内嵌路径）。
func parseSpec(data []byte, source string) (Spec, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var s Spec
	if err := dec.Decode(&s); err != nil {
		return Spec{}, fmt.Errorf("%s: JSON 解析失败（注意：一个文件只能是一个 Spec 对象，字段名与契约 §2 必须一致）: %w", source, err)
	}
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return Spec{}, fmt.Errorf("%s: 文件里有不止一个 JSON 值（只能是一个 Spec 对象）", source)
	}
	if err := Validate(s); err != nil {
		return Spec{}, fmt.Errorf("%s: %w", source, err)
	}
	return s, nil
}
