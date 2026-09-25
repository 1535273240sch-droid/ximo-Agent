//go:build !windows

package ipc

import (
	"context"
	"errors"
	"net"
)

var errNamedPipeNotSupportedOnUnix = errors.New("ipc: windows named pipe is only supported on windows")

// WindowsNamedPipeTransport 非 Windows 平台上的占位实现
type WindowsNamedPipeTransport struct{}

func (t *WindowsNamedPipeTransport) Listen(endpoint string) (net.Listener, error) {
	return nil, errNamedPipeNotSupportedOnUnix
}

func (t *WindowsNamedPipeTransport) Dial(ctx context.Context, endpoint string) (net.Conn, error) {
	return nil, errNamedPipeNotSupportedOnUnix
}
