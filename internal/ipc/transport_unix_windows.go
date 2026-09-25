//go:build windows

package ipc

import (
	"context"
	"errors"
	"net"
)

var errUnixNotSupportedOnWindows = errors.New("ipc: unix domain socket is not supported on windows natively")

// UnixDomainSocketTransport Windows 上的占位实现
type UnixDomainSocketTransport struct{}

func (t *UnixDomainSocketTransport) Listen(endpoint string) (net.Listener, error) {
	return nil, errUnixNotSupportedOnWindows
}

func (t *UnixDomainSocketTransport) Dial(ctx context.Context, endpoint string) (net.Conn, error) {
	return nil, errUnixNotSupportedOnWindows
}
