//go:build windows

// proxy_windows.go —— Windows 系统代理的读取。
//
// 数据源：HKCU\Software\Microsoft\Windows\CurrentVersion\Internet Settings
// 的 ProxyEnable（DWORD，0=关）与 ProxyServer（REG_SZ）。Clash/V2Ray 等「开启
// 系统代理」写的正是这两个值；Electron 自身走系统代理，但 spawn 出来的 Go 后端
// 不会继承，必须自己读。
//
// ProxyServer 有两种形态（见 parseSystemProxy）：全局 "host:port"，以及按协议
// 分号分隔的 "http=h1:1;https=h2:2"。读取带 30s TTL 缓存：用户中途开/关系统
// 代理无需重启应用，又不至于每个请求都查一次注册表。
package provider

import (
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sys/windows/registry"
)

const systemProxyTTL = 30 * time.Second

type systemProxySnapshot struct {
	spec string
	at   time.Time
}

var systemProxyCache atomic.Pointer[systemProxySnapshot]

// systemProxyFor 返回当前系统代理（对指定请求协议），无则 nil。
func systemProxyFor(scheme string) *url.URL {
	spec := currentSystemProxySpec()
	if spec == "" {
		return nil
	}
	return parseSystemProxy(spec, scheme)
}

func currentSystemProxySpec() string {
	now := time.Now()
	if s := systemProxyCache.Load(); s != nil && now.Sub(s.at) < systemProxyTTL {
		return s.spec
	}
	spec := readSystemProxy()
	systemProxyCache.Store(&systemProxySnapshot{spec: spec, at: now})
	return spec
}

func readSystemProxy() string {
	k, err := registry.OpenKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	enable, _, err := k.GetIntegerValue("ProxyEnable")
	if err != nil || enable == 0 {
		return ""
	}
	spec, _, err := k.GetStringValue("ProxyServer")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(spec)
}

// parseSystemProxy 解析 ProxyServer 的两种形态，返回对指定协议生效的代理。
// "http=h1:1;https=h2:2" 形态里，https 请求优先命中 https= 条目，其次 http=
// 条目（浏览器同款回退）；"<local>" 之类的绕过标记直接跳过。
func parseSystemProxy(spec, scheme string) *url.URL {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil
	}
	if !strings.Contains(spec, "=") {
		return parseProxySpec(spec)
	}
	byScheme := map[string]string{}
	for _, part := range strings.Split(spec, ";") {
		part = strings.TrimSpace(part)
		if part == "" || strings.HasPrefix(part, "<") {
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		if !ok || strings.TrimSpace(v) == "" {
			continue
		}
		byScheme[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
	}
	if v := byScheme[strings.ToLower(scheme)]; v != "" {
		return parseProxySpec(v)
	}
	if v := byScheme["http"]; v != "" {
		return parseProxySpec(v)
	}
	if v := byScheme["https"]; v != "" {
		return parseProxySpec(v)
	}
	return nil
}
