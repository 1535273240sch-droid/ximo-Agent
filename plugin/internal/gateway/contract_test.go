package gateway

import (
	"context"
	"testing"
)

// 契约（插件契约-冻结.md §3）冻结的签名：签名漂移在这里先失败。
// 其他包（P-Core 的 CLI/engine、P-AgentIPC）按这些签名编译，改了会连带编译不过。
var (
	_ func(*Client, context.Context, string) (Cred, error)             = (*Client).Login
	_ func(*Client, context.Context) (DeviceAuth, error)               = (*Client).StartDeviceLogin
	_ func(*Client, context.Context, string) (Cred, error)             = (*Client).PollDeviceLogin
	_ func(*Client, context.Context, Cred) ([]Model, error)            = (*Client).ListModels
	_ func(*Client, []Model, string) (Model, error)                    = (*Client).PickModel
	_ func(*Client, context.Context, Cred, int) ([]UsageRecord, error) = (*Client).ListUsage
	_ func(*Client, context.Context) (Cred, error)                     = (*Client).EnsureCred
	_ func(*Client, context.Context) (Health, error)                   = (*Client).Health
)

// TestContractStructLiteral 钉住契约里的结构体字面量用法（P-Core 可能这么写）。
func TestContractStructLiteral(t *testing.T) {
	c := &Client{BaseURL: "https://gw.example.com", CredPath: "/tmp/cred.json"}
	if c.BaseURL == "" || c.CredPath == "" {
		t.Fatal("契约字段名 BaseURL/CredPath 被改动")
	}
	// 零值可用：不设 HTTP/Logf/Now 也能构造并调用（不上网的部分）。
	if _, err := c.PickModel(nil, ""); err == nil {
		t.Error("空目录应报错")
	}
	if got := Mask("gwa_ABCDEFGHIJK"); got != "gwa_****HIJK" {
		t.Errorf("Mask = %q", got)
	}
}
