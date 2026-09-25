//go:build windows

package provider

import "testing"

// TestParseSystemProxy 锁死 Windows ProxyServer 两种形态的解析规则：
// 全局 "host:port" 与按协议的 "http=h1;https=h2"。解析错会让出网请求要么
// 全走直连、要么打错代理端口。
func TestParseSystemProxy(t *testing.T) {
	cases := []struct {
		name   string
		spec   string
		scheme string
		want   string // 期望的代理地址（host:port），空串表示无代理
	}{
		{"全局地址对 https 生效", "127.0.0.1:7897", "https", "127.0.0.1:7897"},
		{"全局地址对 http 生效", "127.0.0.1:7897", "http", "127.0.0.1:7897"},
		{"按协议命中 https", "http=127.0.0.1:8080;https=127.0.0.1:8443", "https", "127.0.0.1:8443"},
		{"按协议 http 请求命中 http", "http=127.0.0.1:8080;https=127.0.0.1:8443", "http", "127.0.0.1:8080"},
		{"https 缺省回退 http 条目", "http=127.0.0.1:8080", "https", "127.0.0.1:8080"},
		{"跳过 <local> 绕过标记", "<local>;https=127.0.0.1:8443", "https", "127.0.0.1:8443"},
		{"空串无代理", "", "https", ""},
		{"纯分号垃圾无代理", ";;;", "https", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := parseSystemProxy(tc.spec, tc.scheme)
			got := ""
			if u != nil {
				got = u.Host
			}
			if got != tc.want {
				t.Fatalf("parseSystemProxy(%q, %q) = %q, want %q", tc.spec, tc.scheme, got, tc.want)
			}
		})
	}
}
