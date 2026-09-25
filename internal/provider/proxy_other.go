//go:build !windows

// proxy_other.go —— 非 Windows 平台的系统代理解析暂无实现：
// 出网代理仅来自环境变量（http.Transport 的默认行为已覆盖）。
package provider

import "net/url"

// systemProxyFor 非 Windows 平台恒返回 nil（直连），代理走环境变量。
func systemProxyFor(string) *url.URL { return nil }
