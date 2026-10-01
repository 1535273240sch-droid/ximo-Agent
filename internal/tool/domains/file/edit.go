package file

// 本文件实现 file_edit / multi_edit：在既有文本文件上做精确字符串替换。
//
// 为什么需要它们：file_write 只能整文件覆盖，模型哪怕只改一行，也得把全文重新
// 输出一遍——大文件下既慢，又容易在重写时丢内容。精确替换把「模型描述的最小
// 变更」与「磁盘上真正改变的字节」对齐：old_string 是校验锚点，找不到就失败，
// 绝不盲写；其余字节原样保留（含 CRLF/LF 与末尾换行）。
//
// 两个工具走与 file_write 完全相同的 Guard 通道（写入白名单 + 敏感文件拦截），
// 并且都不创建新文件：新建是 file_write 的职责。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/ximo888ok-netizen/ximo-agent/internal/tool"
	"github.com/ximo888ok-netizen/ximo-agent/internal/tool/domains"
)

// editSpec 是一次替换请求：file_edit 只有一条，multi_edit 有多条。
type editSpec struct {
	old        string
	new        string
	replaceAll bool
}

// span 是一条编辑在**原始内容**上解析出的替换区间（字节偏移，半开区间）。
type span struct {
	index int // 0 基编辑序号；报错时转成人类可读的「第 N 条」
	start int
	end   int
	text  string
}

// editError 是单条编辑的校验失败，携带序号让调用方决定怎么包装：
// file_edit 说「在 <path> 中：未找到 …」，multi_edit 必须点明是第几条编辑。
type editError struct {
	index int
	msg   string
}

func (e *editError) Error() string { return e.msg }

// resolveEdits 在原始内容上校验并解析全部编辑，但不改动任何字节。
//
// 为什么先整体校验、再统一应用：multi_edit 的原子性来自「只写一次盘」。若边校验
// 边替换，第二条编辑失败时第一条已经改了内存中的 content，落盘就会得到既不是
// 原文、也不是目标态的半成品，用户无法直接撤销。所以这里只做纯计算，任何一条
// 不合法都原样返回错误，让调用方在写盘之前退出。
func resolveEdits(content string, specs []editSpec) ([]span, error) {
	var spans []span
	for i, spec := range specs {
		if spec.old == "" {
			return nil, &editError{index: i, msg: "old_string 不能为空"}
		}
		offsets := allOccurrences(content, spec.old)
		switch {
		case len(offsets) == 0:
			return nil, &editError{index: i, msg: fmt.Sprintf("未找到 %s", snippet(spec.old))}
		case len(offsets) > 1 && !spec.replaceAll:
			return nil, &editError{index: i, msg: fmt.Sprintf("找到 %d 处，请提供更长的上下文或使用 replace_all", len(offsets))}
		}
		for _, off := range offsets {
			spans = append(spans, span{index: i, start: off, end: off + len(spec.old), text: spec.new})
		}
	}
	// 重叠检查：两条编辑改到同一段字节时，谁先谁后没有确定语义（后一条的
	// old_string 可能已被前一条改掉），宁可直接拒绝，让模型自己合并区间。
	// 按起点排序后，只要后一条的起点落在前一条区间内即为重叠。
	sort.Slice(spans, func(a, b int) bool {
		if spans[a].start != spans[b].start {
			return spans[a].start < spans[b].start
		}
		return spans[a].index < spans[b].index
	})
	for i := 1; i < len(spans); i++ {
		if spans[i].start < spans[i-1].end {
			return nil, &editError{
				index: spans[i].index,
				msg: fmt.Sprintf("替换区间与第 %d 条编辑（索引 %d）重叠（字节 %d-%d 与 %d-%d），请合并或拆分成互不重叠的编辑",
					spans[i-1].index+1, spans[i-1].index, spans[i-1].start, spans[i-1].end, spans[i].start, spans[i].end),
			}
		}
	}
	return spans, nil
}

// allOccurrences 返回 old 在 content 中所有互不重叠的出现位置（从左到右）。
func allOccurrences(content, old string) []int {
	if old == "" {
		return nil
	}
	var offsets []int
	for from := 0; from <= len(content)-len(old); {
		i := strings.Index(content[from:], old)
		if i < 0 {
			break
		}
		offsets = append(offsets, from+i)
		from += i + len(old)
	}
	return offsets
}

// applySpans 把已校验、已按位置排好序的替换区间应用到原始内容，返回新内容与
// 实际替换次数。这里只做切片拼接、不做任何换行归一化，所以 CRLF 与「末尾有没有
// 换行」都被完整保留——编辑一行不应该顺手重写整个文件的字节。
func applySpans(content string, spans []span) (string, int) {
	var b strings.Builder
	b.Grow(len(content))
	prev := 0
	for _, s := range spans {
		b.WriteString(content[prev:s.start])
		b.WriteString(s.text)
		prev = s.end
	}
	b.WriteString(content[prev:])
	return b.String(), len(spans)
}

// snippet 截断过长的 old_string 再做 %q 转义：诊断消息应当短到能一眼看完，而不是
// 把模型刚传来的整段代码原样回灌回去。
func snippet(s string) string {
	const limit = 60
	if len(s) <= limit {
		return strconv.Quote(s)
	}
	// 按字节截断可能切断多字节字符，退到最后一个完整 rune 的边界。
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strconv.Quote(s[:cut]) + "…"
}

// editTarget 是一次编辑所需的文件状态。
type editTarget struct {
	abs     string
	content string
}

// openEditTarget 执行 file_edit / multi_edit 共同的路径与内容前置检查。
//
// 与 file_write 的关键差异：编辑**不创建文件**。如果把不存在的路径当成空文件，
// 模型一次路径笔误就会凭空写出一份意外文件（而且 old_string 必然找不到）；直接
// 报「文件不存在」并把新建引向 file_write，返工成本最低。
func openEditTarget(ctx context.Context, guard *domains.Guard, path string) (editTarget, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return editTarget{}, fmt.Errorf("路径解析失败: %v", err)
	}
	// 与 file_write 相同的两道 Guard：写入白名单 + 敏感凭据文件拦截。
	if err := guard.CheckWriteAccess(abs); err != nil {
		return editTarget{}, err
	}
	if err := guard.CheckSensitiveFile(abs); err != nil {
		return editTarget{}, err
	}
	if err := ctx.Err(); err != nil {
		return editTarget{}, fmt.Errorf("调用已取消: %v", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return editTarget{}, fmt.Errorf("文件不存在: %q（新建文件请使用 file_write）", abs)
		}
		return editTarget{}, fmt.Errorf("文件不存在或不可访问: %v", err)
	}
	if info.IsDir() {
		return editTarget{}, fmt.Errorf("%q 是目录，无法编辑；请用 file_list 查看目录内容", abs)
	}
	if info.Size() > maxFileReadBytes {
		return editTarget{}, fmt.Errorf("文件过大（%.1f KB），超过 %d KB 编辑上限", float64(info.Size())/1024, maxFileReadBytes>>10)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return editTarget{}, fmt.Errorf("读取失败: %v", err)
	}
	if isBinary(data) {
		return editTarget{}, fmt.Errorf("%q 是二进制文件（含 NUL 字节），不支持文本替换", abs)
	}
	return editTarget{abs: abs, content: string(data)}, nil
}

// writeEditedFile 原地写回编辑结果。
//
// 新内容在内存里已经算完整，这里一次写入：中途不会出现「写了一半」的中间态
// （若真的写失败，是磁盘/权限问题，与编辑语义无关）。不带 O_CREATE，保留原文件
// 的权限位与属性；fsync 与 file_write 保持一致，返回成功即已落盘。
func writeEditedFile(abs, content string) error {
	f, err := os.OpenFile(abs, os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("打开文件失败: %v", err)
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		return fmt.Errorf("写入失败: %v", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("fsync 失败: %v", err)
	}
	return nil
}

// requiredStringArg 读取必须显式给出的字符串参数，区分「未提供」与「空串」。
// 编辑工具里这两者语义相反：new_string 缺席多半是模型漏参，而空串是明确的删除
// 意图——把漏参当删除执行，等于静默删掉用户文件里的一段内容。
func requiredStringArg(args map[string]any, key string) (string, error) {
	v, ok := args[key]
	if !ok {
		return "", fmt.Errorf("缺少 %s 参数", key)
	}
	s, isString := v.(string)
	if !isString {
		return "", fmt.Errorf("%s 必须是字符串", key)
	}
	return s, nil
}

// ---------------------------------------------------------------------------
// file_edit（B 类可检测幂等：old_string 本身就是状态锚点）
// ---------------------------------------------------------------------------

type editTool struct{ guard *domains.Guard }

func (t *editTool) Definition() tool.ToolDefinition {
	return tool.ToolDefinition{
		Name: "file_edit",
		Description: "对已存在的文本文件做精确字符串替换，只改动命中的字节（CRLF/LF 与末尾换行原样保留）。" +
			"replace_all 为假（默认）时 old_string 必须在文件中唯一出现：0 处报「未找到」，多处报出实际数量并失败；" +
			"replace_all 为真时替换全部匹配。new_string 传空字符串表示删除该片段。",
		Parameters: tool.ObjectSchema(map[string]tool.JSONSchema{
			"path":        {Type: "string", Description: "要编辑的文件路径（必须已存在；新建请用 file_write）"},
			"old_string":  {Type: "string", Description: "被替换的原文，须与文件内容逐字节一致（含缩进与换行）；不能为空"},
			"new_string":  {Type: "string", Description: "替换后的文本；空字符串表示删除该片段", Default: ""},
			"replace_all": {Type: "boolean", Description: "是否替换全部匹配；默认 false，要求唯一匹配", Default: false},
		}, "path", "old_string", "new_string"),
		Risk:        tool.RiskMedium,
		Idempotency: tool.ClassDetectable,
		Domain:      tool.DomainInProcess,
		SideEffect:  true,
	}
}

func (t *editTool) Execute(ctx context.Context, req tool.ToolRequest) tool.ToolResponse {
	fail := func(format string, args ...any) tool.ToolResponse {
		return domains.Fail(req.ToolCallID, "file_edit", format, args...)
	}
	path := domains.StringArg(req.Arguments, "path")
	if path == "" {
		return fail("缺少 path 参数")
	}
	oldText := domains.StringArg(req.Arguments, "old_string")
	if oldText == "" {
		return fail("old_string 不能为空；请给出被替换的原文（要删除内容时，让 old_string 包含被删内容、new_string 留空）")
	}
	newText, err := requiredStringArg(req.Arguments, "new_string")
	if err != nil {
		return fail("%v（如需删除内容，请显式传空字符串）", err)
	}

	target, err := openEditTarget(ctx, t.guard, path)
	if err != nil {
		return fail("%v", err)
	}
	specs := []editSpec{{old: oldText, new: newText, replaceAll: domains.BoolArg(req.Arguments, "replace_all", false)}}
	spans, err := resolveEdits(target.content, specs)
	if err != nil {
		return fail("在 %q 中：%v", target.abs, err)
	}
	updated, replacements := applySpans(target.content, spans)
	if err := writeEditedFile(target.abs, updated); err != nil {
		return fail("%v", err)
	}

	resp := domains.Text(req.ToolCallID, "file_edit", fmt.Sprintf("已替换 %d 处：`%s`", replacements, target.abs))
	resp.Metadata = map[string]any{
		"path":         target.abs,
		"replacements": replacements,
		"bytes_before": len(target.content),
		"bytes_after":  len(updated),
	}
	return resp
}

// ---------------------------------------------------------------------------
// multi_edit（B 类可检测幂等；多条编辑对同一文件原子生效）
// ---------------------------------------------------------------------------

type multiEditTool struct{ guard *domains.Guard }

func (t *multiEditTool) Definition() tool.ToolDefinition {
	return tool.ToolDefinition{
		Name: "multi_edit",
		Description: "对同一个文本文件应用多条精确替换：先用**原始内容**校验每一条，全部通过后才写盘一次（原子）。" +
			"每条编辑在不设 replace_all 时必须唯一命中；任意两条编辑的替换区间不得重叠。任一条失败则整批不写，" +
			"文件保持原样，并指明是第几条编辑、为什么失败。适合一次改多处、且不能接受「只改了一半」的场景。",
		Parameters: tool.ObjectSchema(map[string]tool.JSONSchema{
			"path": {Type: "string", Description: "要编辑的文件路径（必须已存在；新建请用 file_write）"},
			"edits": {
				Type:        "array",
				Description: "编辑列表，按给定顺序应用；每条都在原始内容上匹配",
				Items: &tool.JSONSchema{
					Type: "object",
					Properties: map[string]tool.JSONSchema{
						"old_string":  {Type: "string", Description: "被替换的原文，须与文件内容逐字节一致；不能为空"},
						"new_string":  {Type: "string", Description: "替换后的文本；空字符串表示删除"},
						"replace_all": {Type: "boolean", Description: "是否替换全部匹配；默认 false", Default: false},
					},
					Required: []string{"old_string", "new_string"},
				},
			},
		}, "path", "edits"),
		Risk:        tool.RiskMedium,
		Idempotency: tool.ClassDetectable,
		Domain:      tool.DomainInProcess,
		SideEffect:  true,
	}
}

// parseEditSpecs 解析 multi_edit 的 edits 数组。
// 逐条给出可定位的错误，而不是笼统的「参数非法」：一次 multi_edit 常有十几条
// 编辑，模型需要知道该修哪一条才能自愈。
func parseEditSpecs(args map[string]any) ([]editSpec, error) {
	raw, ok := args["edits"]
	if !ok {
		return nil, fmt.Errorf("缺少 edits 参数")
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("edits 必须是数组")
	}
	if len(arr) == 0 {
		return nil, fmt.Errorf("edits 不能为空；单条编辑请直接使用 file_edit")
	}
	specs := make([]editSpec, 0, len(arr))
	for i, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("第 %d 条编辑（索引 %d）：必须是对象 {old_string, new_string, replace_all?}", i+1, i)
		}
		oldText, ok := m["old_string"].(string)
		if !ok || oldText == "" {
			return nil, fmt.Errorf("第 %d 条编辑（索引 %d）：old_string 必须是非空字符串", i+1, i)
		}
		newRaw, present := m["new_string"]
		if !present {
			return nil, fmt.Errorf("第 %d 条编辑（索引 %d）：缺少 new_string（删除内容请显式传空字符串）", i+1, i)
		}
		newText, ok := newRaw.(string)
		if !ok {
			return nil, fmt.Errorf("第 %d 条编辑（索引 %d）：new_string 必须是字符串", i+1, i)
		}
		specs = append(specs, editSpec{old: oldText, new: newText, replaceAll: domains.BoolArg(m, "replace_all", false)})
	}
	return specs, nil
}

func (t *multiEditTool) Execute(ctx context.Context, req tool.ToolRequest) tool.ToolResponse {
	fail := func(format string, args ...any) tool.ToolResponse {
		return domains.Fail(req.ToolCallID, "multi_edit", format, args...)
	}
	path := domains.StringArg(req.Arguments, "path")
	if path == "" {
		return fail("缺少 path 参数")
	}
	specs, err := parseEditSpecs(req.Arguments)
	if err != nil {
		return fail("%v", err)
	}

	target, err := openEditTarget(ctx, t.guard, path)
	if err != nil {
		return fail("%v", err)
	}
	// 全部编辑先对原始内容校验：任何一条不合法都在写盘之前返回，文件一个字节
	// 都不会变——这正是 multi_edit 相对「连续多次 file_edit」的价值所在。
	spans, err := resolveEdits(target.content, specs)
	if err != nil {
		if ee, ok := err.(*editError); ok {
			return fail("%q 第 %d 条编辑（索引 %d）：%s", target.abs, ee.index+1, ee.index, ee.msg)
		}
		return fail("%q：%v", target.abs, err)
	}
	updated, replacements := applySpans(target.content, spans)
	if err := writeEditedFile(target.abs, updated); err != nil {
		return fail("%v", err)
	}

	resp := domains.Text(req.ToolCallID, "multi_edit",
		fmt.Sprintf("已应用 %d 条编辑，共替换 %d 处：`%s`", len(specs), replacements, target.abs))
	resp.Metadata = map[string]any{
		"path":         target.abs,
		"edits":        len(specs),
		"replacements": replacements,
		"bytes_before": len(target.content),
		"bytes_after":  len(updated),
	}
	return resp
}
