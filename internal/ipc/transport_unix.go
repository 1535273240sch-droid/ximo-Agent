//go:build !windows

package ipc

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

// UnixDomainSocketTransport 实现了基于 Unix Domain Socket 的 IPC 传输
type UnixDomainSocketTransport struct{}

func (t *UnixDomainSocketTransport) Listen(endpoint string) (net.Listener, error) {
	dir := filepath.Dir(endpoint)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create socket dir failed: %w", err)
	}

	// 移除可能遗留的旧 socket 文件
	_ = os.Remove(endpoint)

	l, err := net.Listen("unix", endpoint)
	if err != nil {
		return nil, fmt.Errorf("listen unix socket %s failed: %w", endpoint, err)
	}

	// 权限收敛为仅当前用户可读写
	_ = os.Chmod(endpoint, 0600)

	return &unixListener{
		Listener:   l,
		socketPath: endpoint,
	}, nil
}

func (t *UnixDomainSocketTransport) Dial(ctx context.Context, endpoint string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", endpoint)
}

type unixListener struct {
	net.Listener
	socketPath string
}

func (l *unixListener) Close() error {
	err := l.Listener.Close()
	_ = os.Remove(l.socketPath)
	return err
}
