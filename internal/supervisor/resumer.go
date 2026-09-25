package supervisor

import (
	"context"
	"fmt"
	"sync"

	"github.com/ximo888ok-netizen/ximo-agent/internal/ipc"
)

// 本文件提供 EngineResumer 的真实实现。
//
// 背景：supervisor.go 只定义了 EngineResumer 接口和一个开发期的
// MockEngineResumer（什么都不做）。此前 cmd/ximo-agent/main.go 装配的正是那个
// mock，导致「Supervisor 重启 Engine 之后通知它恢复未完成 run」这一步在真实
// 二进制里从未发生——「kill Engine 自动恢复」这条 P0 红线在进程级别是断的。
//
// 真实的恢复必须跨进程：Supervisor 手里只有 IPC 服务端，它无法直接调用另一个
// 进程里的 Engine.Recover()。所以这里做成「发帧」：Supervisor 通过 IPC 发送
// engine.run.resume 帧，由 Engine 进程侧的处理器执行真正的恢复扫描。

// IPCResumer 通过 IPC 向 Engine 进程发送恢复指令。
//
// 两个依赖都是可注入的，便于测试：
//   - sender：把恢复帧发给对端（通常是管着 Engine 连接的服务端）。
//   - tracker：登记「哪些 run 需要恢复」。
type IPCResumer struct {
	sender ResumeSender

	mu    sync.Mutex
	calls []string
}

// ResumeSender 是发送恢复帧的能力抽象。
//
// 之所以不直接用 *ipc.Server：Supervisor 的看门狗只应「请求」恢复，不应关心
// 帧怎么送。把发送做成接口，既让本类型可以单测（用一个记录调用的假发送器），
// 也让将来的实现可以从「广播」换成「点对点发给 Engine 连接」而不改这里。
type ResumeSender interface {
	// SendResume 请求对端恢复。runID 为空表示全量扫描恢复。
	SendResume(ctx context.Context, runID string) error
}

// ResumeSenderFunc 让普通函数满足 ResumeSender。
type ResumeSenderFunc func(ctx context.Context, runID string) error

// SendResume 实现 ResumeSender。
func (f ResumeSenderFunc) SendResume(ctx context.Context, runID string) error { return f(ctx, runID) }

// NewIPCResumer 构造一个真实的恢复器。
func NewIPCResumer(sender ResumeSender) *IPCResumer {
	return &IPCResumer{sender: sender}
}

// Resume 实现 EngineResumer：请求 Engine 进程恢复指定 run。
func (r *IPCResumer) Resume(ctx context.Context, runID string) error {
	if r == nil || r.sender == nil {
		return fmt.Errorf("supervisor: no resume sender configured")
	}
	r.mu.Lock()
	r.calls = append(r.calls, runID)
	r.mu.Unlock()
	return r.sender.SendResume(ctx, runID)
}

// Calls 返回本恢复器处理过的 runID 列表，供测试断言。
func (r *IPCResumer) Calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

// ---------------------------------------------------------------------------
// 基于 ipc.Server 广播的发送器
// ---------------------------------------------------------------------------

// ServerBroadcastResumer 用 IPC 服务端的 Broadcast 下发恢复帧。
//
// 为什么用广播：Supervisor 的 IPC 服务端同时服务 UI、Engine 和 Worker 多条
// 连接，而服务端目前不记录「哪条连接是 Engine」（只维护 conns 集合）。恢复帧
// 只有 Engine 会处理（UI 与 Worker 没有对应 handler，收到后按未知类型忽略），
// 因此广播是当前连接模型下正确且最小的做法。
//
// 代价是恢复帧会发给所有连接。这不影响正确性，但如果将来连接数很大，应改为
// 「按角色路由」——届时只需换一个 ResumeSender 实现，本文件其余部分不变。
type ServerBroadcastResumer struct {
	server *ipc.Server
	// seq 生成帧序号，避免复用同一序号造成对端诊断混乱。
	seq func() uint64
}

// NewServerBroadcastResumer 构造广播式恢复器。
func NewServerBroadcastResumer(server *ipc.Server, seq func() uint64) *ServerBroadcastResumer {
	if seq == nil {
		seq = func() uint64 { return 0 }
	}
	return &ServerBroadcastResumer{server: server, seq: seq}
}

// SendResume 实现 ResumeSender：广播一个 engine.run.resume 帧。
func (s *ServerBroadcastResumer) SendResume(_ context.Context, runID string) error {
	if s == nil || s.server == nil {
		return fmt.Errorf("supervisor: no ipc server configured for resume")
	}

	// 载荷格式与 ipcapi 的 RunIDPayload 一致（{"run_id": "..."}）。
	// 这里不 import ipcapi，是为了让 supervisor 不依赖业务协议包：Supervisor
	// 只负责在正确的时机把恢复意图送出去，帧的具体语义由协议层和 Engine 约定。
	payload := []byte(`{}`)
	if runID != "" {
		payload = []byte(fmt.Sprintf(`{"run_id":%q}`, runID))
	}

	s.server.Broadcast(&ipc.Frame{
		Header: ipc.FrameHeader{
			Version:   1,
			RequestID: "resume-" + runID,
			Type:      ipc.TypeRunResume,
			Sequence:  s.seq(),
		},
		Payload: payload,
	})
	return nil
}
