package main

import (
	"context"
	"fmt"

	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/upstream"
)

// secretStore 是网关对"密钥后端"的最小需求：能读、能写、能报后端名。
//
// 平台实现见 secrets_windows.go（凭据管理器 → DPAPI）与 secrets_other.go（0600 文件）。
// 把接口放在这里而不是直接依赖 *secrets.Manager，是为了让网关能跨平台编译：
// internal/secrets 目前只在 Windows 上编得过。
type secretStore interface {
	Get(ref string) (string, error)
	Put(value string) (string, error)
	BackendName() string
	Available() bool
}

// secretResolver 把 secretStore 适配成 upstream.SecretResolver。
//
// 解析失败一律返回错误而不是空串：带着空 Authorization 头去请求上游，会得到一个
// 分不清"密钥没配"与"密钥配错"的 401。错误信息里只出现 secret_ref（是明文的单向
// 摘要，不是密钥本身）。
type secretResolver struct{ store secretStore }

var _ upstream.SecretResolver = secretResolver{}

func (r secretResolver) Resolve(_ context.Context, ref string) (string, error) {
	if r.store == nil {
		return "", fmt.Errorf("ximo-gateway: 没有可用的密钥后端，无法解析 %s", ref)
	}
	return r.store.Get(ref)
}

// errString 把可能为 nil 的错误转成日志字段值。
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
