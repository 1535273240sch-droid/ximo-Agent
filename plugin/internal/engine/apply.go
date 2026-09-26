package engine

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/1535273240sch-droid/ximo-plugin/internal/formats"
)

// Out 是计划与差异的输出目标；测试可替换。
var Out io.Writer = os.Stdout

// dirPerm 是给 create=true 的目标新建父目录时用的权限：配置里可能含明文密钥，
// 目录不该让同机其他用户可读。
const dirPerm os.FileMode = 0o700

// undoEntry 是一个文件的写前快照，用于失败回滚。
type undoEntry struct {
	path    string
	data    []byte
	existed bool
	mode    os.FileMode
	// dirs 是本轮为该目标新建的父目录（由内到外），回滚时按同样顺序删掉。
	dirs []string
}

// Apply 执行计划。dryRun 为真时只调用 formats.Diff 打印差异，绝不落盘。
// 真写时：先按 0700 补建缺失的父目录（create=true 的目标常常落在还不存在的目录里），
// 再备份（由 formats.Write 落 <path>.bak-<unix>），失败则把本轮已写文件与新建目录回滚到
// 写前状态，并在返回的错误里说明回滚了哪些、哪些没回滚成功。
// kind=env / kind=ipc 的步骤不改文件（环境变量由用户自行 export，凭据不写进 Agent 配置）。
func Apply(steps []PlanStep, dryRun bool) error {
	if dryRun {
		return applyDryRun(steps)
	}
	var done []undoEntry
	for _, st := range steps {
		if st.Kind != KindFile {
			continue
		}
		if st.doc == nil {
			return rollback(done, fmt.Errorf("%s：计划步骤缺少待写文档（应来自 BuildPlan）", st.Target))
		}
		u := undoEntry{path: st.Target}
		fi, err := os.Stat(st.Target)
		switch {
		case err == nil:
			b, rerr := os.ReadFile(st.Target)
			if rerr != nil {
				return rollback(done, fmt.Errorf("读取 %s 快照失败: %w", st.Target, rerr))
			}
			u.data, u.existed, u.mode = b, true, fi.Mode().Perm()
		case errors.Is(err, os.ErrNotExist):
			u.existed = false
			dirs, derr := makeParentDirs(filepath.Dir(st.Target))
			if derr != nil {
				return rollback(done, fmt.Errorf("创建 %s 的父目录失败: %w", st.Target, derr))
			}
			u.dirs = dirs
		default:
			return rollback(done, fmt.Errorf("探测 %s 失败: %w", st.Target, err))
		}
		done = append(done, u)
		if err := formats.Write(st.Target, st.Format, st.doc, true); err != nil {
			return rollback(done, fmt.Errorf("写入 %s 失败: %w", st.Target, err))
		}
	}
	return nil
}

// makeParentDirs 补建 dir 缺失的每一层（0700），返回本轮真正新建的目录（由内到外），
// 供失败回滚删掉。dir 已存在时返回 nil。dir 的某一层是个文件时返回错误（此时写入必然
// 失败，早点报出来比"临时文件建不出来"更清楚）。
func makeParentDirs(dir string) ([]string, error) {
	if dir == "" || dir == "." {
		return nil, nil
	}
	var missing []string
	for p := dir; ; {
		if _, err := os.Stat(p); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		missing = append(missing, p)
		parent := filepath.Dir(p)
		if parent == p {
			break
		}
		p = parent
	}
	if len(missing) == 0 {
		return nil, nil
	}
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return nil, err
	}
	return missing, nil
}

// removeDirs 由内到外删掉已建目录；删不掉的（例如里面已有别的内容）忽略——留个空目录
// 无害，不值得为此谎报"回滚失败"。
func removeDirs(dirs []string) {
	for _, d := range dirs {
		_ = os.Remove(d)
	}
}

// MaskSecretsInDryRun 控制 dry-run 差异里是否把密钥打码（默认 true，避免差异被
// 误存进 CI 日志）。CLI 的 --show-secrets 会把它置为 false。CLI 单线程使用，不做并发保护。
var MaskSecretsInDryRun = true

func applyDryRun(steps []PlanStep) error {
	for _, st := range steps {
		switch st.Kind {
		case KindFile:
			if st.doc == nil {
				return fmt.Errorf("%s：计划步骤缺少待写文档（应来自 BuildPlan）", st.Target)
			}
			diff, err := formats.Diff(st.Target, st.Format, st.doc)
			if err != nil {
				return fmt.Errorf("生成 %s 的差异失败: %w", st.Target, err)
			}
			if MaskSecretsInDryRun {
				diff = maskDiffSecrets(diff, st.secret)
			}
			fmt.Fprintf(Out, "--- %s (dry-run，未写入)\n", st.Target)
			if strings.TrimSpace(diff) == "" {
				fmt.Fprintln(Out, "(无差异)")
			} else {
				fmt.Fprintln(Out, strings.TrimRight(diff, "\n"))
			}
		case KindEnv:
			fmt.Fprintf(Out, "--- 环境变量（不写盘）：%s\n", strings.Join(st.Vars, " "))
		case KindIPC:
			fmt.Fprintf(Out, "--- %s：需要 Agent 自身的接口（由 agents 包执行）\n", st.Target)
		}
	}
	return nil
}

// sensitiveKeyNames 是"这个字段名装的是凭据"的词表，按 _ - . 空格切成段后逐段比对
// （所以 api_key / apiKey / OPENAI_API_KEY / ANTHROPIC_AUTH_TOKEN 都命中，而
// max_tokens / token_limit 这类计数不命中）。刻意不收 auth 这种太宽泛的词：
// auth_type / auth_method 装的是模式名，打码只会让人看不懂差异。
var sensitiveKeyNames = map[string]bool{
	"key":          true,
	"apikey":       true,
	"accesskey":    true,
	"secretkey":    true,
	"token":        true,
	"accesstoken":  true,
	"authtoken":    true,
	"refreshtoken": true,
	"secret":       true,
	"password":     true,
	"passwd":       true,
	"credential":   true,
	"credentials":  true,
}

// maskDiffSecrets 给 dry-run 差异里的凭据打码，两层：
//   - 按字段名屏蔽取值（api_key / *_token / *secret* / password 等）：目标文件里
//     **原有的**旧密钥不经过 BuildPlan，只有这一层能挡住它，否则 --dry-run 会把别人
//     早就写在那里的明文密钥原样打印出来；
//   - 再按已知的新密钥值做一次精确替换，兜住字段名不在词表里的情况。
//
// 只作用于 dry-run 的输出；--show-secrets 会整体跳过它。
func maskDiffSecrets(diff, secret string) string {
	lines := strings.Split(diff, "\n")
	for i, line := range lines {
		lines[i] = maskLineValue(line)
	}
	out := strings.Join(lines, "\n")
	if len(secret) >= 6 {
		out = strings.ReplaceAll(out, secret, MaskKey(secret))
	}
	return out
}

// maskLineValue 只处理"看起来是 <字段名><: 或 => <值>"的差异行；其余行原样返回。
func maskLineValue(line string) string {
	prefix, body := "", line
	if len(body) > 0 && (body[0] == ' ' || body[0] == '-' || body[0] == '+') {
		prefix, body = body[:1], body[1:]
	}
	sep := strings.IndexAny(body, ":=")
	if sep < 0 {
		return line
	}
	name := strings.Trim(strings.TrimSpace(body[:sep]), `"'`)
	if name == "" || !sensitiveName(name) {
		return line
	}
	val := body[sep+1:]
	lead := len(val) - len(strings.TrimLeft(val, " \t"))
	rest := val[lead:]
	if rest == "" {
		return line
	}
	raw, tail := rest, ""
	if i := strings.IndexByte(rest, ','); i >= 0 { // json / toml 的行尾逗号
		raw, tail = rest[:i], rest[i:]
	}
	if isDigits(strings.Trim(raw, `"'`)) { // 计数类取值（token_limit: 4096）不动它
		return line
	}
	return prefix + body[:sep+1] + val[:lead] + maskValueLiteral(raw) + tail
}

func sensitiveName(name string) bool {
	for _, seg := range strings.FieldsFunc(strings.ToLower(name), func(r rune) bool {
		return r == '_' || r == '-' || r == '.' || r == ' '
	}) {
		if sensitiveKeyNames[seg] {
			return true
		}
	}
	return false
}

func maskValueLiteral(v string) string {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		inner := v[1 : len(v)-1]
		if strings.TrimSpace(inner) == "" {
			return v
		}
		return v[:1] + MaskKey(inner) + v[len(v)-1:]
	}
	return MaskKey(v)
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// rollback 逆序还原本轮已写文件（以及为它们新建的父目录）；无论回滚是否成功都返回错误，
// 并写清哪些没滚成功。
func rollback(done []undoEntry, cause error) error {
	if len(done) == 0 {
		return cause
	}
	var restored, failed []string
	for i := len(done) - 1; i >= 0; i-- {
		u := done[i]
		if !u.existed {
			// 先删本轮写进去的文件，再删本轮新建的目录——反过来目录会因非空删不掉。
			removeErr := os.Remove(u.path)
			removeDirs(u.dirs)
			if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				failed = append(failed, fmt.Sprintf("%s (%v)", u.path, removeErr))
				continue
			}
			restored = append(restored, u.path)
			continue
		}
		if err := os.WriteFile(u.path, u.data, u.mode); err != nil {
			failed = append(failed, fmt.Sprintf("%s (%v)", u.path, err))
			continue
		}
		restored = append(restored, u.path)
	}
	msg := cause.Error()
	if len(restored) > 0 {
		msg += "；已回滚到写前状态：" + strings.Join(restored, ", ")
	}
	if len(failed) > 0 {
		msg += "；回滚失败（需人工处理）：" + strings.Join(failed, ", ")
	}
	return errors.New(msg)
}
