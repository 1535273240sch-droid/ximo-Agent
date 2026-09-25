package ports

import (
	"context"
	"sync/atomic"

	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// ToolExecution describes the run a tool call belongs to, and how that call must
// be executed.
//
// Why this exists: internal/expert executes its sub-agent's tool calls through
// the expert.ToolExecutor interface, which — by design — carries only the call
// ID, the tool name and the arguments. It has no notion of a run, because the
// expert package is deliberately ignorant of the engine's run model. But the
// tool runtime refuses a call without a run ID (invariant I2: every tool call
// belongs to a run), and the engine forbids tool calls that bypass admission,
// the fair queue and the resource pool (see engine.dispatchToolCall).
//
// The only channel available to carry that context across the interface is the
// context itself, so the engine attaches a ToolExecution before activating an
// expert and the runtime adapter picks it up.
type ToolExecution struct {
	// RunID and SessionID scope the call. Both are required by the runtime.
	RunID     string
	SessionID string
	// Dispatch, when non-nil, must be used instead of ToolRuntime.Execute. It is
	// the engine's fully governed path: durable started-marker, admission, fair
	// queue and resource lease. Callers that have a Dispatch must not bypass it,
	// or the limits the scheduler enforces stop meaning anything.
	Dispatch func(ctx context.Context, req ToolRequest) (types.ToolResult, error)
}

type toolExecutionKey struct{}

// WithToolExecution attaches the run scope (and optionally the governed
// dispatcher) for tool calls made under the returned context.
func WithToolExecution(ctx context.Context, te ToolExecution) context.Context {
	return context.WithValue(ctx, toolExecutionKey{}, te)
}

// ToolExecutionFrom returns the tool execution scope attached to ctx, if any.
func ToolExecutionFrom(ctx context.Context) (ToolExecution, bool) {
	if ctx == nil {
		return ToolExecution{}, false
	}
	te, ok := ctx.Value(toolExecutionKey{}).(ToolExecution)
	if !ok {
		return ToolExecution{}, false
	}
	return te, true
}

// ---------------------------------------------------------------------------
// 专家直连分支的候选序号
// ---------------------------------------------------------------------------

// expertSeq 为专家路径的合成 ID（run 内的工具调用、阶段事件）提供进程内单调
// 序号。ID 只需要在同一 run 的事件流里唯一，因此一个进程级计数器即可，不需要
// 密码学随机数——这里刻意不用 types.NewID，因为那是给跨进程标识用的。
var expertSeq atomic.Uint64

// NextExpertSequence 返回一个单调递增的序号，供专家路径合成事件 ID 使用。
func NextExpertSequence() uint64 { return expertSeq.Add(1) }
