package ipc

import (
	"context"
	"fmt"
	"net"
	"runtime"
	"strings"
	"time"
)

// IPCAddr 表示 IPC 地址
type IPCAddr struct {
	NetworkName string
	Address     string
}

func (a IPCAddr) Network() string {
	return a.NetworkName
}

func (a IPCAddr) String() string {
	return a.Address
}

// Transport 传输抽象工厂接口（支持本地 NamedPipe/UDS 平滑升级到远程 TCP/TLS，满足设计要求第4条）
type Transport interface {
	Listen(endpoint string) (net.Listener, error)
	Dial(ctx context.Context, endpoint string) (net.Conn, error)
}

// DefaultTransport 返回当前操作系统推荐的默认 Transport
func DefaultTransport() Transport {
	if runtime.GOOS == "windows" {
		return &WindowsNamedPipeTransport{}
	}
	return &UnixDomainSocketTransport{}
}

// ListenIPC 根据端点协议或当前平台创建监听器
func ListenIPC(endpoint string) (net.Listener, error) {
	if strings.HasPrefix(endpoint, `\\.\pipe\`) {
		t := &WindowsNamedPipeTransport{}
		return t.Listen(endpoint)
	}
	if strings.HasPrefix(endpoint, "unix://") || (!strings.Contains(endpoint, "://") && runtime.GOOS != "windows") {
		clean := strings.TrimPrefix(endpoint, "unix://")
		t := &UnixDomainSocketTransport{}
		return t.Listen(clean)
	}
	if strings.HasPrefix(endpoint, "tcp://") {
		clean := strings.TrimPrefix(endpoint, "tcp://")
		return net.Listen("tcp", clean)
	}
	// 默认走平台策略
	return DefaultTransport().Listen(endpoint)
}

// DialIPC 根据端点连接服务端
func DialIPC(ctx context.Context, endpoint string, timeout time.Duration) (net.Conn, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	if strings.HasPrefix(endpoint, `\\.\pipe\`) {
		t := &WindowsNamedPipeTransport{}
		return t.Dial(ctx, endpoint)
	}
	if strings.HasPrefix(endpoint, "unix://") || (!strings.Contains(endpoint, "://") && runtime.GOOS != "windows") {
		clean := strings.TrimPrefix(endpoint, "unix://")
		t := &UnixDomainSocketTransport{}
		return t.Dial(ctx, clean)
	}
	if strings.HasPrefix(endpoint, "tcp://") {
		clean := strings.TrimPrefix(endpoint, "tcp://")
		var d net.Dialer
		return d.DialContext(ctx, "tcp", clean)
	}

	return DefaultTransport().Dial(ctx, endpoint)
}

// FormatEndpoint 根据当前操作系统返回默认端点字符串
func FormatEndpoint(name string) string {
	if runtime.GOOS == "windows" {
		if strings.HasPrefix(name, `\\.\pipe\`) {
			return name
		}
		return fmt.Sprintf(`\\.\pipe\%s`, name)
	}
	if strings.HasPrefix(name, "/") {
		return name
	}
	return fmt.Sprintf("/tmp/%s.sock", name)
}
