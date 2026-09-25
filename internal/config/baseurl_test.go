package config

import "testing"

// TestNormalizeBaseURL 锁死用户手填地址的规整规则：不合法的输入要么被修好、
// 要么给出明确错误，绝不原样透传去给 HTTP 层制造含糊失败。
func TestNormalizeBaseURL(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{name: "空值原样放行", in: "", want: ""},
		{name: "标准地址去尾斜杠", in: "https://api.example.com/v1/", want: "https://api.example.com/v1"},
		{name: "缺 scheme 补 https", in: "api.example.com/v1", want: "https://api.example.com/v1"},
		{name: "前后空白", in: "  https://api.example.com/v1  ", want: "https://api.example.com/v1"},
		{name: "本地端口可用 http", in: "http://127.0.0.1:8080/v1", want: "http://127.0.0.1:8080/v1"},
		{name: "非 http scheme 拒绝", in: "ftp://api.example.com", wantErr: true},
		{name: "纯主机名补全", in: "api.example.com", want: "https://api.example.com"},
		{name: "包含空格的主机名拒绝", in: "ht tp://x", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeBaseURL(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("输入 %q 应报错, got %q", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("输入 %q 不应报错: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("输入 %q = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
