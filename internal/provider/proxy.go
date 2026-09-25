// proxy.go —— 后端出网请求的统一 HTTP 客户端（代理感知）。
//
// 背景：后端由 Electron spawn 而来，进程环境变量里通常没有 HTTP(S)_PROXY；而
// Go 默认 Transport 只读环境变量。用户机器上的代理（Clash/V2Ray 等）通常以
// 「Windows 系统代理」形式生效，对子进程完全不可见 —— 表现为设置页「获取模型」
// 直连无法直达的服务商地址时 20s 超时，chat/completions 同理，界面只能看到
// 一句含糊的「请求服务商失败」。
//
// 代理解析优先级（与浏览器/系统行为对齐）：
//  1. 环境变量 HTTPS_PROXY / HTTP_PROXY / NO_PROXY（显式配置最优先）；
//  2. 平台系统代理（Windows 见 proxy_windows.go；其他平台暂无实现）；
//  3. 都没有 → 直连。
//
// 超时刻意留给调用方的 ctx 控制（与 retry/breaker 的预算配合），客户端本身
// 不设 Timeout，避免两套超时互相打架。
package provider

import (
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// proxyHTTPClient 是全进程共享的出网客户端：连接池跨所有服务商候选复用。
var proxyHTTPClient = sync.OnceValue(func() *http.Client {
	return &http.Client{Timeout: 0, Transport: proxyTransport()}
})

// ProxyHTTPClient 返回代理感知的共享 HTTP 客户端。所有对服务商的出网请求
// （chat/completions、models 列表）都应经由它，保证代理行为一致。
func ProxyHTTPClient() *http.Client { return proxyHTTPClient() }

// proxyTransport 构造代理解析规则如上的 Transport。
func proxyTransport() http.RoundTripper {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = func(req *http.Request) (*url.URL, error) {
		// 回环地址永远直连：本地 mock 服务、进程内自测端点走代理只会得到
		// 502（代理进程通常拒绝回环目标），与浏览器「绕过本地地址」同款。
		if isLoopbackHost(req.URL.Hostname()) {
			return nil, nil
		}
		u, err := http.ProxyFromEnvironment(req)
		if err != nil {
			return nil, err
		}
		if u != nil {
			return u, nil
		}
		return systemProxyFor(req.URL.Scheme), nil
	}
	return t
}

func isLoopbackHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "localhost" || h == "127.0.0.1" || h == "::1" || h == "[::1]" {
		return true
	}
	return strings.HasSuffix(h, ".localhost")
}

// parseProxySpec 把 "host:port" 形态的代理地址解析成 URL；缺 scheme 时按 http
// 代理处理（本地系统代理几乎都是这一形态）。主机名带明显非法字符（分号等）
// 时视为无效返回 nil，而不是原样透传给 HTTP 层。
func parseProxySpec(spec string) *url.URL {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil
	}
	if !strings.Contains(spec, "://") {
		spec = "http://" + spec
	}
	u, err := url.Parse(spec)
	if err != nil || u.Host == "" {
		return nil
	}
	if strings.ContainsAny(u.Host, ";<>'\" ") {
		return nil
	}
	return u
}
