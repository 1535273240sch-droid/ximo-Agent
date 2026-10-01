package bootstrap

import (
	"context"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/ports"
	"github.com/ximo888ok-netizen/ximo-agent/internal/tool"
	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// toolRuntimeAdapter 让 internal/tool 的运行时满足 Engine 的 ToolRuntime 端口。
//
// 三处必须转换的差异（是编译期硬约束，不是风格问题）：
//  1. 返回形状：tool 的 Execute 只返回 ToolResponse，把一切失败都编码进
//     ErrorCode；端口的 Execute 返回 (types.ToolResult, error)。
//  2. 字段名：端口叫 ToolName，tool 包叫 Name。
//  3. 超时语义：端口给 Timeout duration，tool 包只接受 Deadline 时间点。
//
// 关于 error 返回值的约定：端口规定「非 nil error 表示运行时自身坏了，而不是
// 工具失败」。因此这里只把「进程内 panic」「Worker 崩溃」这类基础设施故障升级
// 为 error，其余（参数非法、权限拒绝、工具业务失败）都作为 Success=false 的
// 正常结果返回，交给 Agent 循环喂回模型自纠——这样一次工具失败不会terminate
// 整个 run。
// toolExecutor 是适配器对内层工具运行时的最小需求。
//
// 为什么是接口而不是直接写 *tool.ToolRuntime：Execute 这一个方法就是适配器用到
// 的全部表面，把它表达成接口之后，F5 的握手（权限层的「需要确认」标记与用户的
// 批准如何穿过适配器）就能在不装配整套工具运行时的情况下被测试。*tool.ToolRuntime
// 自动满足它，生产装配不需要任何改动。
type toolExecutor interface {
	Execute(ctx context.Context, req tool.ToolRequest) tool.ToolResponse
}

type toolRuntimeAdapter struct {
	rt toolExecutor
	// defaultMode 是引擎未指定权限姿态时使用的兜底模式。
	//
	// 必要性：Engine 调用工具端口时并不填 AutoModeLevel（见 engine.executeTool），
	// 若适配器不补一个值，权限引擎会拿到空 Mode 并回退到 ModeCoding 的内置规则，
	// 于是配置里的 runtime.auto_mode 完全失效。在这里补默认值，使配置真正生效。
	defaultMode tool.Mode
}

var _ ports.ToolRuntime = (*toolRuntimeAdapter)(nil)

func newToolRuntimeAdapter(rt *tool.ToolRuntime, defaultMode tool.Mode) *toolRuntimeAdapter {
	if defaultMode == "" {
		defaultMode = tool.ModeSafe
	}
	return &toolRuntimeAdapter{rt: rt, defaultMode: defaultMode}
}

// toolAvailabilityProbe 是内层运行时「这个工具到底注册了没有」的能力探测。
//
// 用可选接口而不是往 toolExecutor 里加方法：toolExecutor 只写适配器真正依赖的
// 最小表面，加了方法所有测试替身都得跟着改。内层换成别的实现时，没有这个能力
// 就退化成「未知」，而不是假定存在——假定存在正是专家子 Agent 把 ui_generate
// 这类本 build 没注册的工具塞给模型的根因。
type toolAvailabilityProbe interface {
	Has(name string) bool
}

// toolDescriberProbe 是内层运行时导出工具真实定义的能力探测。
type toolDescriberProbe interface {
	Describe(name string) (string, map[string]any, bool)
}

// Has 报告某个工具是否真的可以执行。
//
// 与 ports.ToolRuntime 的 Execute 分开表达：Execute 回答「怎么执行」，
// Has 回答「这个 build 里有没有」。调用方（专家子 Agent）必须先问后者，
// 否则会把从未注册的工具名写进 function schema，模型选中后必然报「工具未注册」，
// 白烧一轮往返，还会让子 Agent 以为自己有能力而实际没有。
func (a *toolRuntimeAdapter) Has(name string) bool {
	if a == nil || name == "" {
		return false
	}
	probe, ok := a.rt.(toolAvailabilityProbe)
	if !ok {
		return false
	}
	return probe.Has(name)
}

// Describe 返回工具对模型可见的真实定义（描述 + 参数 schema）。
//
// 第二个返回值为 false 表示查不到。查不到时调用方应回退到空参数占位，
// 而不是编一份看起来很像的 schema——编出来的必填参数模型一定会照着填，
// 然后被工具的参数校验拒绝。
func (a *toolRuntimeAdapter) Describe(name string) (string, map[string]any, bool) {
	if a == nil || name == "" {
		return "", nil, false
	}
	probe, ok := a.rt.(toolDescriberProbe)
	if !ok {
		return "", nil, false
	}
	return probe.Describe(name)
}

// Execute 实现 ports.ToolRuntime。
func (a *toolRuntimeAdapter) Execute(ctx context.Context, req ports.ToolRequest) (types.ToolResult, error) {
	mode := tool.Mode(req.AutoModeLevel)
	if mode == "" {
		mode = a.defaultMode
	}

	toolReq := tool.ToolRequest{
		RunID:      req.RunID,
		SessionID:  req.SessionID,
		ToolCallID: req.ToolCallID,
		Name:       req.ToolName,
		Arguments:  req.Arguments,
		Mode:       mode,
	}
	// F5: the user's approval for this call has to reach the permission layer,
	// or a call that came back as requires-confirmation would be handed back for
	// confirmation forever. Confirmed only removes the "ask" step — an explicit
	// deny rule still wins, because the permission engine evaluates it after
	// reading this field.
	//
	// The scope is always single here: the user's click authorizes *this* call.
	// Session-wide approvals are tracked by the Engine (per tool name) and
	// re-applied per call, because the permission layer's session scope is bound
	// to a specific rule ID that the UI never sees.
	if req.Confirmed || req.ConfirmedForSession {
		toolReq.Confirmation = tool.Confirmation{Confirmed: true, Scope: tool.ScopeSingle}
	}
	if req.Timeout > 0 {
		toolReq.Deadline = time.Now().Add(req.Timeout)
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}

	resp := a.rt.Execute(ctx, toolReq)

	result := types.ToolResult{
		ToolCallID: resp.ToolCallID,
		ToolName:   resp.ToolName,
		Content:    resp.Content,
		Success:    resp.Success,
		Error:      resp.Error,
		Metadata:   resp.Metadata,
		Duration:   resp.Duration,
	}

	// The permission layer's "this needs a human" signal lives in dedicated
	// ToolResponse fields, while the Engine reads it from ToolResult.Metadata
	// (types.ToolResult is a frozen contract with no field for it). Copying the
	// two keys across is what turns a flagged call into the loop's park-and-ask
	// path; without it the flag is silently dropped and the UI never asks.
	//
	// The keys are the same two names internal/engine reads
	// (metaRequiresConfirmation / metaConfirmationMessage). They are duplicated
	// as literals on purpose: bootstrap must not import the engine, and a shared
	// constant in types would be a contract change.
	if resp.RequiresConfirmation {
		if result.Metadata == nil {
			result.Metadata = make(map[string]any, 2)
		}
		result.Metadata["requiresConfirmation"] = true
		result.Metadata["confirmationMessage"] = types.RedactString(resp.ConfirmationMessage)
	}

	switch resp.ErrorCode {
	case tool.ErrToolPanicked, tool.ErrWorkerCrashed:
		return result, types.NewError(types.CodeInternal, "%s", resp.Error)
	default:
		return result, nil
	}
}
