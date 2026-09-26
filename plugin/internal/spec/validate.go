package spec

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	// ID 用小写 slug：字母/数字开头，允许 - 与 _，最长 64。
	idRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	// 环境变量名（POSIX 风格；Windows 上这些名字同样合法）。
	envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	// 点号路径的一段：文件里的键，或数组下标（数字也匹配本表达式）。
	fieldKeyRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	// 路径开头的 %VAR%。
	percentVarRe = regexp.MustCompile(`^%[A-Za-z_][A-Za-z0-9_]*%`)
	// 路径开头的 $VAR / ${VAR}。
	dollarVarRe = regexp.MustCompile(`^\$(?:[A-Za-z_][A-Za-z0-9_]*|\{[A-Za-z_][A-Za-z0-9_]*\})`)
)

// Validate 校验一份规格数据是否可用。只做**语法与语义**校验，不碰真实文件系统：
// 同一份 JSON 在 Windows / macOS / Linux 上都必须给出同样的结论，所以路径只查
// 「写法是否合法」（是否绝对、是否含 .. 穿越），不查是否存在——展开与存在性判断
// 由 ExpandPath 与 engine 在运行时负责。
//
// 返回的 error 会一次性列出全部问题（数据文件排错比「只报第一条」有用）。
func Validate(s Spec) error {
	var errs []string
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Sprintf(format, args...))
	}

	// 身份
	if s.ID == "" {
		add("id 不能为空")
	} else if !idRe.MatchString(s.ID) {
		add("id %q 不合法：只允许小写字母/数字/-/_，字母或数字开头，最长 64 字符", s.ID)
	}
	if strings.TrimSpace(s.Name) == "" {
		add("name 不能为空")
	}
	if strings.TrimSpace(s.Description) == "" {
		add("description 不能为空（不确定的 schema 必须在这里写明「需用户确认」）")
	}
	if s.Docs != "" && !strings.HasPrefix(s.Docs, "http://") && !strings.HasPrefix(s.Docs, "https://") {
		add("docs %q 必须是 http(s) 链接", s.Docs)
	}

	// 检测证据
	if s.Detect.Empty() {
		add("detect 不能为空：binaries / paths / env 至少要有一项，否则该规格无法被 detect 命中")
	}
	for _, b := range s.Detect.Binaries {
		if strings.TrimSpace(b) == "" {
			add("detect.binaries 含空项")
		}
	}
	for _, p := range s.Detect.Paths {
		if err := validatePath(p); err != nil {
			add("detect.paths[%s]: %v", p, err)
		}
	}
	for _, e := range s.Detect.Env {
		if !envNameRe.MatchString(e) {
			add("detect.env[%s] 不是合法的环境变量名", e)
		}
	}

	if strings.TrimSpace(s.RestartNote) == "" {
		add("restart_note 不能为空：改完配置要不要重启进程必须写清楚")
	}

	// 入站协议
	for _, p := range s.Protocols {
		if !contains(ProtocolValues, p) {
			add("protocols[%s] 不在白名单 %v 内", p, ProtocolValues)
		}
	}

	// 动作：至少能干活（写文件 或 输出环境变量）
	if len(s.Files) == 0 && s.Env.BaseURL == "" && s.Env.APIKey == "" {
		add("files 为空且 env.base_url / env.api_key 都未提供：该规格没有任何可执行动作")
	}

	errs = append(errs, validateEnv(s.Env)...)
	errs = append(errs, validateFiles(s.Files)...)

	if len(errs) > 0 {
		return fmt.Errorf("规格 %q 校验失败: %s", s.ID, strings.Join(errs, "; "))
	}
	return nil
}

func validateEnv(e EnvSpec) []string {
	var errs []string
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Sprintf(format, args...))
	}
	for label, v := range map[string]string{"env.base_url": e.BaseURL, "env.api_key": e.APIKey, "env.model": e.Model} {
		if v == "" {
			continue
		}
		if !envNameRe.MatchString(v) {
			add("%s=%q 必须是环境变量名而不是值（不要把 URL / key 字面量写进规格）", label, v)
		}
	}
	for _, extra := range e.Extra {
		if !envNameRe.MatchString(extra) {
			add("env.extra[%s] 不是合法的环境变量名", extra)
		}
	}
	if e.Style != "" && !contains(Styles, e.Style) {
		add("env.style=%q 不在白名单 %v 内（空串表示由 CLI --style 决定）", e.Style, Styles)
	}
	return errs
}

func validateFiles(files []Target) []string {
	var errs []string
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Sprintf(format, args...))
	}
	seenPath := map[string]int{}
	for i, t := range files {
		where := fmt.Sprintf("files[%d]", i)
		if err := validatePath(t.Path); err != nil {
			add("%s.path: %v", where, err)
		} else if prev, dup := seenPath[t.Path]; dup {
			add("%s.path 与 files[%d].path 重复: %s", where, prev, t.Path)
		} else {
			seenPath[t.Path] = i
		}
		if !contains(Formats, t.Format) {
			add("%s.format=%q 不在白名单 %v 内", where, t.Format, Formats)
		}
		if len(t.Fields) == 0 {
			add("%s.fields 不能为空", where)
		}
		seenField := map[string]int{}
		for j, f := range t.Fields {
			fw := fmt.Sprintf("%s.fields[%d]", where, j)
			if err := validateDotPath(f.Path); err != nil {
				add("%s.path: %v", fw, err)
			} else if prev, dup := seenField[f.Path]; dup {
				add("%s.path 与 %s.fields[%d].path 重复: %s", fw, where, prev, f.Path)
			} else {
				seenField[f.Path] = j
			}
			if !contains(FromValues, f.From) {
				add("%s.from=%q 不在白名单 %v 内", fw, f.From, FromValues)
			}
			// from=literal 必须有常量；反之不得带常量（否则是写错了 from）。
			if f.From == FromLiteral && f.Value == "" {
				add("%s.from=literal 时 value 不能为空", fw)
			}
			if f.From != FromLiteral && f.Value != "" {
				add("%s.value=%q 无意义：value 只允许与 from=literal 搭配", fw, f.Value)
			}
		}
	}
	return errs
}

// validatePath 校验一个规格里写的路径「写法」是否合法。
func validatePath(raw string) error {
	if raw == "" {
		return fmt.Errorf("路径不能为空")
	}
	if strings.ContainsRune(raw, 0) {
		return fmt.Errorf("路径含 NUL 字符")
	}
	if strings.ContainsAny(raw, "\r\n\t") {
		return fmt.Errorf("路径含控制字符")
	}
	if !isAbsoluteish(raw) {
		return fmt.Errorf("路径 %q 必须是绝对路径（~/...、/...、%%VAR%%/...、$VAR/...、C:/...）", raw)
	}
	for _, seg := range strings.FieldsFunc(raw, func(r rune) bool { return r == '/' || r == '\\' }) {
		if seg == ".." {
			return fmt.Errorf("路径 %q 含 .. 目录穿越", raw)
		}
	}
	return nil
}

// isAbsoluteish 判断路径写法是否为绝对路径（含 ~ / %VAR% / $VAR 形式）。
func isAbsoluteish(p string) bool {
	switch {
	case strings.HasPrefix(p, "/"), strings.HasPrefix(p, `\`): // 含 UNC 的 \\server\share
		return true
	case p == "~", strings.HasPrefix(p, "~/"), strings.HasPrefix(p, `~\`):
		return true
	case strings.HasPrefix(p, "~"): // ~user 形式不支持
		return false
	case strings.HasPrefix(p, "%"):
		return percentVarRe.MatchString(p)
	case strings.HasPrefix(p, "$"):
		return dollarVarRe.MatchString(p)
	case len(p) >= 3 && isASCIIAlpha(p[0]) && p[1] == ':' && (p[2] == '/' || p[2] == '\\'):
		return true
	}
	return false
}

// validateDotPath 校验点号路径（"a.b.0.c"）。
func validateDotPath(p string) error {
	if p == "" {
		return fmt.Errorf("点号路径不能为空")
	}
	if strings.HasPrefix(p, ".") || strings.HasSuffix(p, ".") || strings.Contains(p, "..") {
		return fmt.Errorf("点号路径 %q 含空段（不要以 . 开头/结尾，也不要连续 ..）", p)
	}
	for _, seg := range strings.Split(p, ".") {
		if !fieldKeyRe.MatchString(seg) {
			return fmt.Errorf("点号路径 %q 的段 %q 含非法字符", p, seg)
		}
	}
	return nil
}

func isASCIIAlpha(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
