package agents

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DocIO 是「结构化配置文件读写」的端口，方法签名与契约
// （recon/插件契约-冻结.md §3）里 internal/formats 的四个函数逐字对应。
//
// 为什么用端口而不是直接 import internal/formats：本包属于 plugin 模块，可以
// import 同模块的 formats，但那会把 agents 的编译期绑死在 formats 上（契约冻结
// 的是签名，不是实现）。端口让「复用 formats」变成一行装配代码，同时本包自带
// 一个零依赖的 JSON 回退实现，任何一侧缺失都不会让构建挂掉。
//
// 复用 formats 的装配方式（P-Core 的 cmd 侧一行）：
//
//	agents.FuncDocIO{
//	    ReadFn: formats.Read, WriteFn: formats.Write,
//	    DiffFn: formats.Diff, SetPathFn: formats.SetPath,
//	}
type DocIO interface {
	Read(path, format string) (map[string]any, error)
	Write(path, format string, doc map[string]any, backup bool) error
	Diff(path, format string, next map[string]any) (string, error)
	SetPath(doc map[string]any, dotPath string, value any) error
}

// FuncDocIO 把同签名的四个函数适配成 DocIO，便于直接注入 internal/formats。
type FuncDocIO struct {
	ReadFn    func(path, format string) (map[string]any, error)
	WriteFn   func(path, format string, doc map[string]any, backup bool) error
	DiffFn    func(path, format string, next map[string]any) (string, error)
	SetPathFn func(doc map[string]any, dotPath string, value any) error
}

func (f FuncDocIO) Read(path, format string) (map[string]any, error) {
	if f.ReadFn == nil {
		return nil, fmt.Errorf("agents: FuncDocIO.ReadFn 未设置")
	}
	return f.ReadFn(path, format)
}

func (f FuncDocIO) Write(path, format string, doc map[string]any, backup bool) error {
	if f.WriteFn == nil {
		return fmt.Errorf("agents: FuncDocIO.WriteFn 未设置")
	}
	return f.WriteFn(path, format, doc, backup)
}

func (f FuncDocIO) Diff(path, format string, next map[string]any) (string, error) {
	if f.DiffFn == nil {
		return "", fmt.Errorf("agents: FuncDocIO.DiffFn 未设置")
	}
	return f.DiffFn(path, format, next)
}

func (f FuncDocIO) SetPath(doc map[string]any, dotPath string, value any) error {
	if f.SetPathFn == nil {
		return fmt.Errorf("agents: FuncDocIO.SetPathFn 未设置")
	}
	return f.SetPathFn(doc, dotPath, value)
}

// LocalDocIO 是本包自带的 JSON 配置读写器：原样保留未知字段、原子写、
// 写前备份 `<path>.bak-<unix>`。它与 internal/formats 的 json 能力等价，
// 但只认 json（其它格式交给 formats）。
type LocalDocIO struct{}

// NewLocalDocIO 返回自带实现的实例。
func NewLocalDocIO() LocalDocIO { return LocalDocIO{} }

// Read 读 JSON 配置。文件不存在 → 空 map + nil（是否创建由调用方决定）。
func (LocalDocIO) Read(path, format string) (map[string]any, error) {
	if err := requireJSON(format); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, fmt.Errorf("读取 %s 失败: %w", path, err)
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return map[string]any{}, nil
	}
	doc := map[string]any{}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("%s 不是合法的 JSON 对象: %w（拒绝改写，以免覆盖用户的手写配置）", path, err)
	}
	return doc, nil
}

// Write 原子写入 JSON 配置。backup 为 true 时先复制一份 `<path>.bak-<unix>`。
func (LocalDocIO) Write(path, format string, doc map[string]any, backup bool) error {
	if err := requireJSON(format); err != nil {
		return err
	}
	if doc == nil {
		doc = map[string]any{}
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化配置失败: %w", err)
	}
	data = append(data, '\n')

	if backup {
		if _, err := backupFile(path); err != nil {
			return err
		}
	}

	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("创建目录 %s 失败: %w", dir, err)
		}
	}
	// 原子写：先写同目录临时文件再 rename。直接截断写会在进程被杀时留下
	// 半个 JSON，而这是 ximo-agent 启动时唯一读的配置文件。
	tmp, err := os.CreateTemp(dir, ".ximo-plugin-*.tmp")
	if err != nil {
		return fmt.Errorf("创建临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("写入临时文件失败: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("刷盘失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关闭临时文件失败: %w", err)
	}
	// 与 ximo-agent 自己的 SaveConfig 保持一致（0644）：该文件不含凭据，
	// 只含 base_url / model / secret_ref。
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("设置权限失败: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("替换 %s 失败: %w", path, err)
	}
	return nil
}

// Diff 返回「现文件 → next」的行级差异文本，供 --dry-run 打印。
func (LocalDocIO) Diff(path, format string, next map[string]any) (string, error) {
	if err := requireJSON(format); err != nil {
		return "", err
	}
	cur, err := LocalDocIO{}.Read(path, format)
	if err != nil {
		return "", err
	}
	nextData, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return "", fmt.Errorf("序列化待写内容失败: %w", err)
	}
	curData, err := json.MarshalIndent(cur, "", "  ")
	if err != nil {
		return "", fmt.Errorf("序列化现有内容失败: %w", err)
	}
	return unifiedDiff(string(curData), string(nextData)), nil
}

// SetPath 按点号路径写入值，数字段表示数组下标（例 "a.b.0.c"）。
//
// 中间层缺失时按下一段的形态补出来（数字段补数组，其余补对象）；类型冲突
// 直接报错，而不是悄悄把用户的数组换成对象。
func (LocalDocIO) SetPath(doc map[string]any, dotPath string, value any) error {
	return setPath(doc, dotPath, value)
}

func requireJSON(format string) error {
	switch format {
	case "", "json":
		return nil
	default:
		return fmt.Errorf("agents: 自带的配置读写器只支持 json，收到 %q；"+
			"其它格式请用 --file-io 注入 internal/formats 的实现", format)
	}
}

// backupFile 复制 path 到 `<path>.bak-<unix 秒>` 并返回备份路径。
// path 不存在时返回 ("", nil)：没有东西要备份不是错误。
func backupFile(path string) (string, error) {
	src, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("打开 %s 备份失败: %w", path, err)
	}
	defer src.Close()

	info, err := src.Stat()
	if err != nil {
		return "", fmt.Errorf("读取 %s 信息失败: %w", path, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s 是目录，不是配置文件", path)
	}

	backupPath := fmt.Sprintf("%s.bak-%d", path, time.Now().Unix())
	dst, err := os.OpenFile(backupPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return "", fmt.Errorf("创建备份 %s 失败: %w", backupPath, err)
	}
	defer dst.Close()
	if _, err := dst.ReadFrom(src); err != nil {
		return "", fmt.Errorf("写入备份 %s 失败: %w", backupPath, err)
	}
	return backupPath, nil
}

// setPath 是 SetPath 的实现。
//
// 规则：
//   - 中间层缺失时按后续路径形态补出来（数字段补数组、其余补对象）；
//   - **已存在**的数组只允许落在既有下标上，越界报错——静默扩容会替用户
//     造出一串 null，还可能把工具列表的顺序改乱；
//   - 撞上非容器值（例如路径 a.b 但 a 是字符串）时报错，不覆盖用户手写的内容。
func setPath(doc map[string]any, dotPath string, value any) error {
	segs := strings.Split(dotPath, ".")
	valid := make([]string, 0, len(segs))
	for _, s := range segs {
		if s == "" {
			return fmt.Errorf("agents: 非法路径 %q（存在空段）", dotPath)
		}
		valid = append(valid, s)
	}
	if len(valid) == 0 {
		return fmt.Errorf("agents: 空路径")
	}

	var cur any = doc
	for i, seg := range valid {
		last := i == len(valid)-1
		switch node := cur.(type) {
		case map[string]any:
			if last {
				node[seg] = value
				return nil
			}
			next, ok := node[seg]
			if !ok || next == nil {
				// 整条剩余路径都由我们创建，数组可以按需增长。
				node[seg] = buildPath(valid[i+1:], value)
				return nil
			}
			cur = next
		case []any:
			idx, err := strconv.Atoi(seg)
			if err != nil {
				return fmt.Errorf("agents: 路径 %q 的第 %d 段 %q 需要是数组下标", dotPath, i, seg)
			}
			if idx < 0 || idx >= len(node) {
				return fmt.Errorf("agents: 路径 %q 的下标 %d 越界（数组长度 %d）", dotPath, idx, len(node))
			}
			if last {
				node[idx] = value
				return nil
			}
			next := node[idx]
			if next == nil {
				node[idx] = buildPath(valid[i+1:], value)
				return nil
			}
			cur = next
		default:
			return fmt.Errorf("agents: 路径 %q 的第 %d 段 %q 撞上了非容器值（%T）",
				dotPath, i, seg, cur)
		}
	}
	return nil
}

// buildPath 为「尚不存在的路径尾部」造出容器并放入 value。数字段造数组
// （按需增长到该下标），其余造对象。
func buildPath(segs []string, value any) any {
	if len(segs) == 0 {
		return value
	}
	seg := segs[0]
	if idx, err := strconv.Atoi(seg); err == nil && idx >= 0 {
		arr := make([]any, idx+1)
		arr[idx] = buildPath(segs[1:], value)
		return arr
	}
	return map[string]any{seg: buildPath(segs[1:], value)}
}

// unifiedDiff 是极简行级差异：先算 LCS，再输出带前缀的行。配置文件的规模
// 让 O(n*m) 完全可接受（几百行以内）。
func unifiedDiff(oldText, newText string) string {
	a := strings.Split(oldText, "\n")
	b := strings.Split(newText, "\n")

	lcs := make([][]int, len(a)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}

	var out []string
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			out = append(out, " "+a[i])
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			out = append(out, "-"+a[i])
			i++
		default:
			out = append(out, "+"+b[j])
			j++
		}
	}
	for ; i < len(a); i++ {
		out = append(out, "-"+a[i])
	}
	for ; j < len(b); j++ {
		out = append(out, "+"+b[j])
	}

	// 只保留有变化的行及其紧邻上下文，配置文件的完整输出会把变化淹没。
	return trimContext(out, 3)
}

func trimContext(lines []string, ctx int) string {
	keep := make([]bool, len(lines))
	for i, l := range lines {
		if strings.HasPrefix(l, "+") || strings.HasPrefix(l, "-") {
			for k := maxInt(0, i-ctx); k <= minInt(len(lines)-1, i+ctx); k++ {
				keep[k] = true
			}
		}
	}
	var out []string
	gap := false
	for i, l := range lines {
		if !keep[i] {
			gap = true
			continue
		}
		if gap && len(out) > 0 {
			out = append(out, "...")
		}
		gap = false
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// fieldDiff 打印两个字符串字段的「旧 → 新」，供 IPC 路径的 --dry-run 使用。
// 空值显示为 <空>，否则用户分不清「没变」和「被清空」。
func fieldDiff(lines *[]string, name, oldVal, newVal string) {
	if oldVal == newVal {
		return
	}
	*lines = append(*lines, fmt.Sprintf("  %-16s %s → %s", name, orEmptyPlaceholder(oldVal), orEmptyPlaceholder(newVal)))
}

func orEmptyPlaceholder(v string) string {
	if strings.TrimSpace(v) == "" {
		return "<空>"
	}
	return v
}

// sortedKeys 让诊断输出的键顺序稳定（map 遍历顺序是随机的）。
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
