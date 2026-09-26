//go:build !windows

package agents

import (
	"context"
	"net"
)

// dialPipe 在非 Windows 平台没有命名管道，直接报错而不是退回别的东西：
// 静默退化成 unix socket 会让用户以为连上了，实际连的是另一个端点。
func dialPipe(_ context.Context, _ string) (net.Conn, error) {
	return nil, ErrPipeUnsupported
}
