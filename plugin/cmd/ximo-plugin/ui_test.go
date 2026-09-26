package main

import (
	"context"
	"strings"
	"testing"
)

// ui 子命令的用法错误必须先返回，不能真去起服务（否则单测会挂住）。
func TestUIUsageErrors(t *testing.T) {
	useSpecs(t, nil)
	cases := []struct {
		name string
		args []string
	}{
		{"端口越界", []string{"ui", "--port", "70000"}},
		{"端口非数字", []string{"ui", "--port", "abc"}},
		{"未知 flag", []string{"ui", "--bogus"}},
		{"网关地址非法", []string{"ui", "--gateway", "not-a-url"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, _, errOut := run(c.args, "")
			if code != exitUsage {
				t.Fatalf("%v：退出码 %d，想要 %d（stderr=%s）", c.args, code, exitUsage, errOut)
			}
			if strings.TrimSpace(errOut) == "" {
				t.Errorf("%v：用法错误应打印到 stderr", c.args)
			}
		})
	}
}

// --version 必须能被打包脚本用 -ldflags "-X main.version=..." 覆盖（变量名固定为
// version），界面的 /api/state 也从同一个变量取版本，不能各写一份。
func TestVersionIsInjectedVariable(t *testing.T) {
	useSpecs(t, nil)
	old := version
	version = "9.9.9-test"
	t.Cleanup(func() { version = old })

	code, out, _ := run([]string{"--version"}, "")
	if code != exitOK {
		t.Fatalf("退出码 %d，想要 0", code)
	}
	if !strings.Contains(out, "9.9.9-test") {
		t.Fatalf("--version 应输出注入的版本号，得到 %q", out)
	}

	be := &uiBackend{app: &app{home: t.TempDir()}}
	st, err := be.State(context.Background())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if st.Version != "9.9.9-test" {
		t.Fatalf("state.version=%q，想要注入的 9.9.9-test", st.Version)
	}
}
