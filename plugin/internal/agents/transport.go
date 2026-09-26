package agents

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"strings"
	"time"
)

// EndpointFromFlag / EndpointFromEnv / EndpointFromPlatform 是 DiscoverEndpoint
// 返回的来源标记，用于在输出里说明「这次连的是哪来的端点」——端点猜错时
// 用户第一件事就是想知道它是怎么被选出来的。
const (
	EndpointFromFlag     = "flag"
	EndpointFromEnv      = "env"
	EndpointFromPlatform = "platform-default"
)

// EnvEndpoint 是覆盖 IPC 端点的环境变量名。
const EnvEndpoint = "XIMO_IPC_ENDPOINT"

// DefaultPipeName 与主仓库 cmd/ximo-agent/main.go 的默认端点名一致
// （ipc.FormatEndpoint("ximo-agent-ipc")）。
const DefaultPipeName = "ximo-agent-ipc"

var (
	// ErrUnixSocketUnsupported 是 Windows 上尝试用 unix:// 端点时的错误。
	ErrUnixSocketUnsupported = errors.New("ipc: unix domain socket is not supported on windows natively")
	// ErrPipeUnsupported 是非 Windows 上尝试用命名管道端点时的错误。
	ErrPipeUnsupported = errors.New(`ipc: windows named pipe endpoint is only supported on windows`)
)

// FormatEndpoint 返回某个名字在当前平台的默认端点：
// Windows 下是 `\\.\pipe\<name>`，其他平台是 `/tmp/<name>.sock`。
// 与主仓库 ipc.FormatEndpoint 行为一致（名字已带管道前缀时原样返回）。
func FormatEndpoint(name string) string {
	if runtime.GOOS == "windows" {
		if strings.HasPrefix(name, `\\.\pipe\`) {
			return name
		}
		return `\\.\pipe\` + name
	}
	if strings.HasPrefix(name, "/") {
		return name
	}
	return "/tmp/" + name + ".sock"
}

// DefaultEndpoint 返回平台默认端点（Windows 命名管道 `\\.\pipe\ximo-agent-ipc`）。
func DefaultEndpoint() string { return FormatEndpoint(DefaultPipeName) }

// DiscoverEndpoint 按 显式 flag → XIMO_IPC_ENDPOINT → 平台默认 的顺序确定端点，
// 同时返回来源标记。explicit 为空表示调用方没传。
func DiscoverEndpoint(explicit string) (endpoint, source string) {
	if s := strings.TrimSpace(explicit); s != "" {
		return s, EndpointFromFlag
	}
	if s := strings.TrimSpace(os.Getenv(EnvEndpoint)); s != "" {
		return s, EndpointFromEnv
	}
	return DefaultEndpoint(), EndpointFromPlatform
}

// IsPipeEndpoint 报告端点是否是 Windows 命名管道形态。
func IsPipeEndpoint(endpoint string) bool {
	return strings.HasPrefix(endpoint, `\\.\pipe\`) || strings.HasPrefix(strings.ToLower(endpoint), "npipe://")
}

// DialEndpoint 按端点形态选传输并连接：
//
//	`\\.\pipe\...` / `npipe://...` → Windows 命名管道
//	`unix://...`                   → Unix 域套接字
//	`tcp://host:port`              → TCP（跨机/容器场景）
//	无 scheme                      → 非 Windows 当 unix 路径，Windows 当管道名
//
// timeout > 0 时整个连接过程受其约束。
func DialEndpoint(ctx context.Context, endpoint string, timeout time.Duration) (net.Conn, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	switch {
	case IsPipeEndpoint(endpoint):
		name := strings.TrimPrefix(endpoint, "npipe://")
		return dialPipe(ctx, name)
	case strings.HasPrefix(endpoint, "unix://"):
		if runtime.GOOS == "windows" {
			return nil, ErrUnixSocketUnsupported
		}
		var d net.Dialer
		return d.DialContext(ctx, "unix", strings.TrimPrefix(endpoint, "unix://"))
	case strings.HasPrefix(endpoint, "tcp://"):
		var d net.Dialer
		return d.DialContext(ctx, "tcp", strings.TrimPrefix(endpoint, "tcp://"))
	}

	if runtime.GOOS == "windows" {
		// 没带 scheme 的名字在 Windows 上按管道名处理，与主仓库 DialIPC 一致。
		return dialPipe(ctx, FormatEndpoint(endpoint))
	}
	var d net.Dialer
	return d.DialContext(ctx, "unix", endpoint)
}

// endpointKind 给用户看的可读形态（不含任何凭据）。
func endpointKind(endpoint string) string {
	switch {
	case IsPipeEndpoint(endpoint):
		return "named-pipe"
	case strings.HasPrefix(endpoint, "unix://"):
		return "unix-socket"
	case strings.HasPrefix(endpoint, "tcp://"):
		return "tcp"
	default:
		if runtime.GOOS == "windows" {
			return "named-pipe"
		}
		return "unix-socket"
	}
}

// describeDialError 把连接失败整理成一句人话，附上「怎么改」的提示。
func describeDialError(endpoint string, err error) string {
	return fmt.Sprintf("连不上 ximo-agent 的 IPC 端点 %s（%s）：%v；"+
		"确认 ximo-agent 正在运行，或用 --ipc-endpoint / %s 指定端点",
		endpoint, endpointKind(endpoint), err, EnvEndpoint)
}
