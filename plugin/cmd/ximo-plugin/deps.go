package main

// 本文件集中放与 P-Spec / P-Client / P-AgentIPC 的连接点，便于跨包对齐时只改这一处。

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"

	"github.com/1535273240sch-droid/ximo-plugin/internal/gateway"
	"github.com/1535273240sch-droid/ximo-plugin/internal/spec"
)

// loadSpecsFn 连接 P-Spec：内置规格（embed）+ 用户规格（<home>/.ximo-plugin/specs）。
var loadSpecsFn = func(home string) ([]spec.Spec, error) { return spec.Load(home) }

// gatewayAPI 是 cmd 侧定义的最小网关能力。
// 前半部分是契约 §3 冻结的签名（逐字一致，因此下面的断言恒成立）；后半部分是
// P-Client 已提供、本 CLI 用到的能力，缺一个就编译失败（比运行期才发现好）。
type gatewayAPI interface {
	// —— 契约 §3 冻结 ——
	Login(ctx context.Context, ref string) (gateway.Cred, error)
	StartDeviceLogin(ctx context.Context) (gateway.DeviceAuth, error)
	PollDeviceLogin(ctx context.Context, deviceCode string) (gateway.Cred, error)
	ListModels(ctx context.Context, cred gateway.Cred) ([]gateway.Model, error)
	PickModel(models []gateway.Model, want string) (gateway.Model, error)

	// —— P-Client 额外提供 ——
	EnsureCred(ctx context.Context) (gateway.Cred, error)
	LoadCred() (gateway.Cred, error)
	SaveCred(cred gateway.Cred) error
	CredPerm() (gateway.PermStatus, error)
	ListUsage(ctx context.Context, cred gateway.Cred, limit int) ([]gateway.UsageRecord, error)
	Health(ctx context.Context) (gateway.Health, error)
}

var _ gatewayAPI = (*gateway.Client)(nil)

// newGatewayClient 按契约 §3 构造客户端，凭据文件固定为 <home>/.ximo-plugin/cred.json
// （--home 生效，便于测试）。声明为变量以便单测注入 fake。
var newGatewayClient = func(baseURL, home string) gatewayAPI {
	return gateway.NewClient(baseURL, gateway.CredPathIn(home))
}

func credPath(home string) string { return gateway.CredPathIn(home) }

// expand 是本次调用的路径展开上下文（P-Spec 的 ExpandOptions）。--home 显式生效
// 时连 %APPDATA% / $XIMO_HOME 一起按隔离 home 解析，否则这些变量按真实环境解析。
func (a *app) expand() spec.ExpandOptions {
	return spec.ExpandOptions{Home: a.home, IsolateHome: a.isolateHome}
}

func marshalIndent(v any) ([]byte, error) { return json.MarshalIndent(v, "", "  ") }

// defaultIPCEndpoint 是 ximo-agent 的 IPC 端点：Windows 命名管道（P-AgentIPC 的
// transport_windows.go）；其它平台 P-AgentIPC 明确不支持（ErrPipeUnsupported），返回空串。
func defaultIPCEndpoint() string {
	if runtime.GOOS == "windows" {
		return `\\.\pipe\ximo-agent-ipc`
	}
	return ""
}

// resolveGateway 校验并规整 --gateway。
func (a *app) resolveGateway(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", fmt.Errorf("必须指定 --gateway <url>")
	}
	return gateway.NormalizeGatewayURL(raw)
}

// gatewayCred 用 --api-key 在本地构造凭据（只在本进程内存里，不落盘）。
func gatewayCred(gatewayURL, apiKey string) gateway.Cred {
	return gateway.Cred{Gateway: gatewayURL, APIKey: apiKey}
}

// credFor 取调用网关的凭据：--api-key 优先（只在本进程内存里，不落盘），
// 否则读本机凭据文件（access token 过期时自动续期）。
func (a *app) credFor(ctx context.Context, gatewayURL, apiKey string) (gateway.Cred, error) {
	if k := strings.TrimSpace(apiKey); k != "" {
		return gatewayCred(gatewayURL, k), nil
	}
	cred, err := newGatewayClient(gatewayURL, a.home).EnsureCred(ctx)
	if err != nil {
		return cred, fmt.Errorf("%w；也可用 --api-key 直接指定密钥", err)
	}
	if cred.Empty() {
		return cred, fmt.Errorf("凭据文件 %s 里既没有 API Key 也没有访问令牌，请重新 login", credPath(a.home))
	}
	return cred, nil
}
