package ipcapi

import (
	"context"
	"fmt"

	"github.com/ximo888ok-netizen/ximo-agent/internal/ipc"
)

// 本文件把「用户确认/否决执行计划」暴露成 IPC 帧（任务4）。
//
// 为什么单独成文件：任务4 是本批任务里唯一新增 RPC 的，独立文件可以把它与
// service.go（run 生命周期）、admin.go（密钥与配置）分开，避免和并行任务在
// 同一文件里产生文本行冲突。
//
// 与 cancel 的写法保持一致：请求体是 run_id（外加 approved），成功回
// {ok:true}，失败回 ok=false 的信封。

// PlanConfirmer 是「确认执行计划」能力，由 Engine 满足。
//
// 刻意不把 ConfirmPlan 加进 types.Engine：那是已发布的跨任务契约，任务01 的
// UI 宿主和测试里的最小桩都按它编译，往里面加方法会让所有实现方一起失败。
// 这里用「可选能力 + 类型断言」的方式接入，与 Recoverer 的处理方式一致——
// 引擎没实现该能力时，帧返回明确错误，而不是静默成功。
type PlanConfirmer interface {
	// ConfirmPlan 记录用户对 run 已提出计划的决定：approved 为 true 表示确认
	// 执行，false 表示要求重新规划。
	ConfirmPlan(ctx context.Context, runID string, approved bool) error
}

// PlanService 把计划确认注册到 IPC 服务端。
type PlanService struct {
	confirmer PlanConfirmer
}

// NewPlanService 构造计划服务。confirmer 可为 nil，对应的帧会返回明确错误。
func NewPlanService(confirmer PlanConfirmer) *PlanService {
	return &PlanService{confirmer: confirmer}
}

// Register 注册计划类帧。
func (s *PlanService) Register(srv *ipc.Server) {
	srv.RegisterHandler(ipc.TypePlanConfirm, s.handlePlanConfirm)
}

// PlanConfirmPayload 是 TypePlanConfirm 的请求载荷。
type PlanConfirmPayload struct {
	RunID string `json:"run_id"`
	// Approved 为 true 表示确认执行，false 表示要求重新规划。
	Approved bool `json:"approved"`
}

// handlePlanConfirm 处理计划确认：engine.plan.confirm。
func (s *PlanService) handlePlanConfirm(ctx context.Context, req *ipc.Frame) (*ipc.Frame, error) {
	if s.confirmer == nil {
		return ErrorFrame(ipc.TypePlanConfirm, req,
			fmt.Errorf("plan confirmation is not available in this build")), nil
	}
	env, err := decodeEnvelope(req)
	if err != nil {
		return ErrorFrame(ipc.TypePlanConfirm, req, err), nil
	}
	var p PlanConfirmPayload
	if err := env.Decode(&p); err != nil {
		return ErrorFrame(ipc.TypePlanConfirm, req, err), nil
	}
	if p.RunID == "" {
		return ErrorFrame(ipc.TypePlanConfirm, req, fmt.Errorf("run_id is required")), nil
	}

	if err := s.confirmer.ConfirmPlan(ctx, p.RunID, p.Approved); err != nil {
		return ErrorFrame(ipc.TypePlanConfirm, req, err), nil
	}
	out, err := EnvelopeFrom(ipc.TypePlanConfirm, map[string]any{
		"run_id":   p.RunID,
		"approved": p.Approved,
		"ok":       true,
	}, FrameOpts{RequestID: req.Header.RequestID, SessionID: req.Header.SessionID})
	if err != nil {
		return ErrorFrame(ipc.TypePlanConfirm, req, err), nil
	}
	return out, nil
}

// ConfirmPlan 提交对某个 run 计划的决定。
func (c *Client) ConfirmPlan(ctx context.Context, runID string, approved bool) error {
	return c.call(ctx, ipc.TypePlanConfirm,
		PlanConfirmPayload{RunID: runID, Approved: approved}, nil)
}
