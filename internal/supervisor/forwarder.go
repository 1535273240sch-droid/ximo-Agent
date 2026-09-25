package supervisor

import (
	"context"
	"fmt"
	"sync"

	"github.com/ximo888ok-netizen/ximo-agent/internal/ipc"
)

// 本文件实现「把业务帧代理给 Engine 进程」。
//
// 为什么需要它：业务真相（SQLite、密钥库、模型调用）全在 Engine 进程里，而 UI
// 连的是 Supervisor 的 IPC 端点。Supervisor 的职责是进程监管，不该自己实现业务，
// 因此对未注册的帧应当**代理**给 Engine 而不是回 "unknown method"。
//
// 之前的实现缺了这一层，表现为界面上所有依赖 Engine 的能力（保存 API 密钥、
// 获取模型列表、读取配置）全部返回 "unknown method: ..."。

// EngineForwarder 把一个未命中的帧转发到 Engine 的 IPC 端点并等待响应。
type EngineForwarder struct {
	endpoint string
	maxPay   uint32

	mu     sync.Mutex
	client *ipc.Client
	// seq 为转发出去的帧分配序号，避免复用同一序号造成对端诊断混乱。
	seq uint64
}

// 编译期断言：必须满足 ipc.FrameForwarder。
var _ ipc.FrameForwarder = (*EngineForwarder)(nil)

// NewEngineForwarder 构造转发器。endpoint 是 Engine 进程的监听地址。
func NewEngineForwarder(endpoint string, maxPayload uint32) *EngineForwarder {
	if maxPayload == 0 {
		maxPayload = 16 * 1024 * 1024
	}
	return &EngineForwarder{endpoint: endpoint, maxPay: maxPayload}
}

// ensureClient 惰性建立到 Engine 的连接，断线后自动重连。
//
// 惰性而非启动即连：Engine 可能比 Supervisor 后启动（甚至还没起来），
// 启动即连会产生一连串无意义的失败日志；等真正有业务帧要转发时再连，
// 失败也只影响那一次请求，且错误信息对用户更有意义。
func (f *EngineForwarder) ensureClient(ctx context.Context) (*ipc.Client, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.client != nil {
		return f.client, nil
	}
	c := ipc.NewClient(f.endpoint, f.maxPay, true)
	if err := c.Connect(ctx); err != nil {
		return nil, fmt.Errorf("cannot reach engine at %s: %w", f.endpoint, err)
	}
	f.client = c
	return f.client, nil
}

// Forward 实现 ipc.FrameForwarder。
func (f *EngineForwarder) Forward(ctx context.Context, frame *ipc.Frame) (*ipc.Frame, error) {
	if f == nil || f.endpoint == "" {
		return nil, fmt.Errorf("engine forwarder is not configured")
	}

	client, err := f.ensureClient(ctx)
	if err != nil {
		return nil, err
	}

	f.mu.Lock()
	f.seq++
	seq := f.seq
	f.mu.Unlock()

	// 原样保留 Type 与 Payload（业务语义），只替换序号与截止时间：
	// RequestID 由 ipc.Client 内部重新分配，因此这里不必保留。
	out := &ipc.Frame{
		Header: ipc.FrameHeader{
			Version:   frame.Header.Version,
			RequestID: frame.Header.RequestID,
			SessionID: frame.Header.SessionID,
			Sequence:  seq,
			Type:      frame.Header.Type,
		},
		Payload: frame.Payload,
	}
	if d := frame.Header.Deadline(); !d.IsZero() {
		out.Header.SetDeadline(d)
	}

	resp, err := client.SendRequest(ctx, out)
	if err != nil {
		// 连接可能已失效（Engine 重启过）：丢弃缓存的客户端，下次重连。
		f.mu.Lock()
		if f.client == client {
			_ = f.client.Close()
			f.client = nil
		}
		f.mu.Unlock()
		return nil, fmt.Errorf("forward to engine failed: %w", err)
	}
	return resp, nil
}

// Close 释放到 Engine 的连接。
func (f *EngineForwarder) Close() error {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.client != nil {
		err := f.client.Close()
		f.client = nil
		return err
	}
	return nil
}
