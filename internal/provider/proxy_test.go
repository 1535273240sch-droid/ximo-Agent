package provider

import (
	"net/http"
	"testing"
)

// TestProxyTransportBypassesLoopback 锁死「回环地址直连」：本地 mock 服务与
// 自测端点绝不能被送进系统/环境变量代理 —— 代理对回环目标通常直接 502。
func TestProxyTransportBypassesLoopback(t *testing.T) {
	tr, ok := proxyTransport().(*http.Transport)
	if !ok || tr.Proxy == nil {
		t.Fatal("proxyTransport 应返回带 Proxy 规则的 *http.Transport")
	}
	for _, raw := range []string{
		"http://127.0.0.1:45998/v1/models",
		"http://localhost:8080/v1/models",
		"http://[::1]:8080/v1/models",
		"http://api.local.localhost/v1/models",
	} {
		req, err := http.NewRequest(http.MethodGet, raw, nil)
		if err != nil {
			t.Fatalf("构造请求: %v", err)
		}
		if u, err := tr.Proxy(req); err != nil || u != nil {
			t.Errorf("%s 应直连, got proxy=%v err=%v", raw, u, err)
		}
	}
	// 非回环地址不做"必然直连"的断言：取决于环境变量/系统代理状态，
	// 这里只断言函数不会 panic 且结果可解析。
	req, _ := http.NewRequest(http.MethodGet, "https://api.example.com/v1/models", nil)
	if _, err := tr.Proxy(req); err != nil {
		t.Errorf("非回环地址的代理解析不应报错: %v", err)
	}
}
