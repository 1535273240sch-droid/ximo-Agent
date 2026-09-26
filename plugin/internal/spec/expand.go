package spec

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// ExpandOptions 是路径展开的上下文。engine 的 detect / BuildPlan 都必须用它，
// 不要各写一份展开逻辑：曾经有过两份实现、语义还不一样（一份对未定义变量报错，
// 一份静默展开成空串），后者会在 detect 证据里留下
// "/ximo-agent/config.json" 这种残渣路径。
type ExpandOptions struct {
	// Home 是用户主目录（CLI --home / $XIMO_PLUGIN_HOME / 系统主目录）。
	Home string
	// IsolateHome 为真表示 Home 是本次操作唯一认可的家目录（CLI --home 显式
	// 生效）：与家目录相关的环境变量（APPDATA / LOCALAPPDATA / USERPROFILE /
	// HOME / XDG_CONFIG_HOME / XIMO_HOME）一律按 Home 反推，不读真实环境。
	//
	// 不隔离时这些变量按真实环境解析——没给 --home 时 Home 只是「系统主目录」
	// 的默认值，此时必须尊重用户真实设置的 $XIMO_HOME / 被重定向的 %APPDATA%。
	IsolateHome bool
}

// envLookup 是环境变量查询端口。
type envLookup func(name string) (string, bool)

// lookup 返回本次展开使用的环境变量查询函数。
func (o ExpandOptions) lookup() envLookup {
	if !o.IsolateHome {
		return os.LookupEnv
	}
	over := isolatedEnv(o.Home)
	return func(name string) (string, bool) {
		if v, ok := over[lookupKey(name)]; ok {
			return v, true
		}
		return os.LookupEnv(name)
	}
}

// lookupKey 让覆盖表在 Windows 上大小写不敏感（%appdata% 与 %APPDATA% 同义），
// 与 os.LookupEnv 在该平台的行为一致。
func lookupKey(name string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(name)
	}
	return name
}

// isolatedEnv 返回隔离 home 下「家目录相关环境变量」的取值表。
//
// 为什么需要它：--home 的语义是「把这次操作当成在另一个家目录下执行」。若这些
// 变量仍读真实环境，--home 指向临时目录时 %APPDATA%/ximo-agent 依旧解析到真实
// 用户的 AppData\Roaming（实测过的坑），测试与非默认布局都隔离不了。
func isolatedEnv(home string) map[string]string {
	home = filepath.Clean(home)
	m := map[string]string{
		"HOME":        home,
		"USERPROFILE": home,
		// XIMO_HOME 是 ximo-agent 自己的 BaseDir 开关：隔离后它指向隔离 home 下
		// 的同一位置（布局规则见 agentBaseDirIn）。
		"XIMO_HOME": agentBaseDirIn(home),
	}
	switch runtime.GOOS {
	case "windows":
		m["APPDATA"] = filepath.Join(home, "AppData", "Roaming")
		m["LOCALAPPDATA"] = filepath.Join(home, "AppData", "Local")
	case "darwin":
		// macOS 的规格路径本来就写成 ~/Library/Application Support/...，靠 ~ 展开
		// 即可；真实 macOS 上没有 XDG_CONFIG_HOME，这里也不凭空造一个。
	default:
		m["XDG_CONFIG_HOME"] = filepath.Join(home, ".config")
	}
	return m
}

// agentBaseDirIn 按 ximo-agent 的平台约定算出 <BaseDir>，与主仓库
// internal/config.DefaultPaths、本模块 internal/agents.DefaultBaseDir 的布局一致。
// 这里只算布局、不读环境，因此 spec 不必（也不该）反向依赖 agents 包。
func agentBaseDirIn(home string) string {
	switch runtime.GOOS {
	case "windows":
		return filepath.Join(home, "AppData", "Roaming", "ximo-agent")
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "ximo-agent")
	default:
		return filepath.Join(home, ".config", "ximo-agent")
	}
}

// ExpandPath 把规格里写的路径展开成运行时绝对路径（不隔离家目录，见 ExpandPathOpts）：
//
//	~            → home（home 为空时取 os.UserHomeDir）
//	%APPDATA%    → 环境变量（Windows 风格，Windows 上大小写不敏感）
//	$HOME ${HOME} → 环境变量（POSIX 风格）
//
// 与 Validate 的分工：Validate 只查写法（保证一份 JSON 在任何平台上结论一致），
// ExpandPath 在运行时查环境，因此**引用了未定义的环境变量时返回错误**，而不是
// 静默展开成空串——否则 macOS 上的 "%APPDATA%/ximo-agent/config.json" 会变成
// "/ximo-agent/config.json"，那是一个可能被写坏的绝对路径。
//
// 展开结果必须是绝对路径。
func ExpandPath(path, home string) (string, error) {
	return ExpandPathOpts(path, ExpandOptions{Home: home})
}

// ExpandPathOpts 是 ExpandPath 的完整形态：额外支持「隔离 home」（CLI --home）。
// 本模块内所有路径展开都必须走这里。
func ExpandPathOpts(path string, opt ExpandOptions) (string, error) {
	if path == "" {
		return "", fmt.Errorf("路径为空")
	}
	if strings.ContainsRune(path, 0) {
		return "", fmt.Errorf("路径含 NUL 字符")
	}
	home := strings.TrimSpace(opt.Home)
	if home != "" {
		home = filepath.Clean(home)
	}
	if opt.IsolateHome && home == "" {
		return "", fmt.Errorf("路径 %q 需要在隔离 home 下展开，但 home 为空", path)
	}

	out := path
	if strings.HasPrefix(out, "~") {
		if out != "~" && !strings.HasPrefix(out, "~/") && !strings.HasPrefix(out, `~\`) {
			return "", fmt.Errorf("路径 %q 使用了不支持的 ~user 形式", path)
		}
		h := home
		if h == "" {
			h, _ = os.UserHomeDir()
		}
		if h == "" {
			return "", fmt.Errorf("路径 %q 需要展开 %%，但无法确定用户主目录", path)
		}
		out = h + out[1:]
	}

	var missing []string
	noteMissing := func(name string) {
		for _, m := range missing {
			if m == name {
				return
			}
		}
		missing = append(missing, name)
	}

	env := opt.lookup()
	out = expandPercent(out, env, noteMissing)
	out = os.Expand(out, func(name string) string {
		v, ok := env(name)
		if !ok {
			noteMissing(name)
		}
		return v
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("路径 %q 引用了未定义的环境变量: %s", path, strings.Join(missing, ", "))
	}

	out = filepath.Clean(out)
	if !filepath.IsAbs(out) {
		return "", fmt.Errorf("路径 %q 展开后不是绝对路径: %q", path, out)
	}
	return out, nil
}

// expandPercent 展开 %VAR%（单趟，不递归展开替换结果里的 %，避免自引用死循环）。
// 只把「%名字%」当变量，孤立或不合法的 % 原样保留。
func expandPercent(s string, env envLookup, noteMissing func(string)) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] != '%' {
			b.WriteByte(s[i])
			i++
			continue
		}
		end := strings.IndexByte(s[i+1:], '%')
		if end < 0 {
			b.WriteByte(s[i])
			i++
			continue
		}
		name := s[i+1 : i+1+end]
		if !envNameRe.MatchString(name) {
			b.WriteByte(s[i])
			i++
			continue
		}
		if v, ok := env(lookupKey(name)); ok {
			b.WriteString(v)
		} else {
			// 用空串占位并记录缺失：位置保持原样，便于错误里定位。
			noteMissing(name)
		}
		i += end + 2
	}
	return b.String()
}
