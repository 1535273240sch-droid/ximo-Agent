package ipcapi

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/ipc"
	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// 事件长轮询的等待参数：
//   - eventsFirstWait 是首批事件（含持久化历史）的最长等待；
//   - eventsIdleWait 是已收到数据后通道空闲多久即返回，它同时是流式
//     增量到达界面的延迟上界。两者都远小于 UI 侧的请求超时，避免长阻塞。
//   - eventsMaxDwell 是收到首个事件后单次轮询继续收集的总时长上界。空闲
//     上界只在流出现 ≥eventsIdleWait 的停顿时生效；模型连续吐字时事件
//     间隔（约等于 coalescer 的 50ms 帧距）永远小于空闲阈值，一批会一直
//     攒到 eventsLimit 或请求超时才归还，前端收到的就是几十秒一遇的巨帧。
//     给出独立的总时长上界，把"增量归还频率"约束在可感知的节奏内。
const (
	eventsFirstWait = 10 * time.Second
	eventsIdleWait  = 1200 * time.Millisecond
	eventsMaxDwell  = 500 * time.Millisecond
)

// EngineService 把 types.Engine 契约暴露成 IPC 处理器。
//
// 它刻意只依赖 types.Engine 接口，而不是具体的 *engine.Engine：这样 Engine
// 进程与将来的 UI 进程、测试桩都能复用同一套帧语义，且本包不会反向依赖
// engine 包（避免 import 环，也便于在测试里换入假引擎）。
type EngineService struct {
	engine types.Engine
	// recoverer 是可选的恢复入口；Engine 实现了 types.Recoverer。
	recoverer types.Recoverer
	// eventsLimit 限制单次事件拉取的条数，防止一次请求把大 run 的全部历史
	// 灌进内存（UI 应以返回的 seq 续拉）。
	eventsLimit int
}

// ServiceOption 配置 EngineService。
type ServiceOption func(*EngineService)

// WithEventsLimit 设置单次事件拉取上限。
func WithEventsLimit(n int) ServiceOption {
	return func(s *EngineService) {
		if n > 0 {
			s.eventsLimit = n
		}
	}
}

// NewEngineService 构造服务。engine 必须非 nil。
func NewEngineService(eng types.Engine, opts ...ServiceOption) (*EngineService, error) {
	if eng == nil {
		return nil, fmt.Errorf("ipcapi: nil engine")
	}
	s := &EngineService{engine: eng, eventsLimit: 1000}
	// 只有实现了 Recoverer 的引擎才支持 run.resume；否则该帧返回明确错误，
	// 而不是静默成功——静默成功会让 Supervisor 误以为恢复已完成。
	if r, ok := eng.(types.Recoverer); ok {
		s.recoverer = r
	}
	for _, o := range opts {
		o(s)
	}
	return s, nil
}

// Register 把全部业务帧处理器注册到 IPC 服务端。
func (s *EngineService) Register(srv *ipc.Server) {
	srv.RegisterHandler(ipc.TypeRunSubmit, s.handleSubmit)
	srv.RegisterHandler(ipc.TypeRunCancel, s.handleCancel)
	srv.RegisterHandler(ipc.TypeRunStatus, s.handleStatus)
	srv.RegisterHandler(ipc.TypeEventStream, s.handleEvents)
	srv.RegisterHandler(ipc.TypeRunResume, s.handleResume)
}

// handleSubmit 处理提交请求：engine.run.submit。
func (s *EngineService) handleSubmit(ctx context.Context, req *ipc.Frame) (*ipc.Frame, error) {
	env, err := decodeEnvelope(req)
	if err != nil {
		return ErrorFrame(ipc.TypeRunSubmit, req, err), nil
	}
	var p SubmitPayload
	if err := env.Decode(&p); err != nil {
		return ErrorFrame(ipc.TypeRunSubmit, req, err), nil
	}
	if p.Prompt == "" {
		return ErrorFrame(ipc.TypeRunSubmit, req, fmt.Errorf("prompt is required")), nil
	}

	submitReq := types.SubmitRequest{
		SessionID:    p.SessionID,
		Prompt:       p.Prompt,
		SystemPrompt: p.SystemPrompt,
		Model:        p.Model,
		LongTask:     p.LongTask,
		MaxRounds:    p.MaxRounds,
		PlanMode:     p.PlanMode,
		// 任务3：把用户手选的专家带到 Engine，由它决定是否走「直连 Activate」
		// 那条路径。为空时下游行为与改动前完全一致。
		ExpertID: p.ExpertID,
	}
	if p.Priority != "" {
		submitReq.Priority = types.Priority(p.Priority)
	}

	handle, err := s.engine.Submit(ctx, submitReq)
	if err != nil {
		return ErrorFrame(ipc.TypeRunSubmit, req, err), nil
	}
	out, err := EnvelopeFrom(ipc.TypeRunSubmit, HandlePayload{
		RunID:     handle.RunID,
		SessionID: handle.SessionID,
		State:     string(handle.State),
	}, FrameOpts{RequestID: req.Header.RequestID, SessionID: req.Header.SessionID})
	if err != nil {
		return ErrorFrame(ipc.TypeRunSubmit, req, err), nil
	}
	return out, nil
}

// handleCancel 处理取消请求：engine.run.cancel。
func (s *EngineService) handleCancel(ctx context.Context, req *ipc.Frame) (*ipc.Frame, error) {
	runID, err := decodeRunID(req)
	if err != nil {
		return ErrorFrame(ipc.TypeRunCancel, req, err), nil
	}
	if err := s.engine.Cancel(ctx, runID); err != nil {
		return ErrorFrame(ipc.TypeRunCancel, req, err), nil
	}
	out, err := EnvelopeFrom(ipc.TypeRunCancel, map[string]string{"run_id": runID, "status": "cancelled"},
		FrameOpts{RequestID: req.Header.RequestID, SessionID: req.Header.SessionID})
	if err != nil {
		return ErrorFrame(ipc.TypeRunCancel, req, err), nil
	}
	return out, nil
}

// handleStatus 处理状态查询：engine.run.status。
func (s *EngineService) handleStatus(ctx context.Context, req *ipc.Frame) (*ipc.Frame, error) {
	runID, err := decodeRunID(req)
	if err != nil {
		return ErrorFrame(ipc.TypeRunStatus, req, err), nil
	}
	run, err := s.engine.GetRun(ctx, runID)
	if err != nil {
		return ErrorFrame(ipc.TypeRunStatus, req, err), nil
	}
	out, err := EnvelopeFrom(ipc.TypeRunStatus, toRunPayload(run),
		FrameOpts{RequestID: req.Header.RequestID, SessionID: req.Header.SessionID})
	if err != nil {
		return ErrorFrame(ipc.TypeRunStatus, req, err), nil
	}
	return out, nil
}

// handleEvents 处理事件拉取：engine.event.stream。
//
// 语义：以请求里的 afterSeq 为起点，最多返回 eventsLimit 条，More 指示是否还有。
// 这是 UI 断线重连后「拉取缺失事件并重放」的数据来源（第26章）。
//
// 返回时机（有界长轮询）：引擎的事件通道在 run 终态前不会关闭，若一直读到
// 通道关闭，本调用会阻塞到 run 结束——UI 的请求超时远早于此，表现为流式
// 增量永远到不了界面。因此这里在「首批等待」与「后续空闲」两个维度上都设了
// 上限：第一批事件最多等 10 秒；已拿到数据后，通道空闲超过 eventsIdleWait
// 即返回已收集的部分，More=true 提示客户端继续拉。终态或读满上限则照常返回。
func (s *EngineService) handleEvents(ctx context.Context, req *ipc.Frame) (*ipc.Frame, error) {
	env, err := decodeEnvelope(req)
	if err != nil {
		return ErrorFrame(ipc.TypeEventStream, req, err), nil
	}
	var p struct {
		RunID    string `json:"run_id"`
		AfterSeq uint64 `json:"after_seq"`
	}
	if err := env.Decode(&p); err != nil {
		return ErrorFrame(ipc.TypeEventStream, req, err), nil
	}
	if p.RunID == "" {
		return ErrorFrame(ipc.TypeEventStream, req, fmt.Errorf("run_id is required")), nil
	}

	// 本请求结束即取消引擎侧的流：Events 返回的订阅会一直向通道缓冲事件，
	// 若不随请求取消，每次"空闲即返回"都会遗留一个废弃订阅者，空闲 run
	// 反复轮询时逐步积累，最终触发 MaxSubscribers 上限。
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	ch, err := s.engine.Events(reqCtx, p.RunID, p.AfterSeq)
	if err != nil {
		return ErrorFrame(ipc.TypeEventStream, req, err), nil
	}

	var events []types.Event
	more := false

	readOne := func(wait time.Duration) (types.Event, bool) {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case ev, ok := <-ch:
			return ev, ok
		case <-timer.C:
			return types.Event{}, false
		case <-ctx.Done():
			return types.Event{}, false
		}
	}

	// 首批等待：给历史读取与首个实时事件一个宽限期；超时返回空集让客户端再拉。
	first, ok := readOne(eventsFirstWait)
	if ok {
		events = append(events, first)
		// 后续读取：一有空闲就交还增量，保证流式延迟以 eventsIdleWait 为上界；
		// 连续流式（事件间隔恒小于空闲阈值）则以 eventsMaxDwell 为上界归还，
		// 客户端凭 More=true 立即续拉，增量保持小批高频到达。
		dwellStart := time.Now()
		for len(events) < s.eventsLimit {
			next, ok := readOne(eventsIdleWait)
			if !ok {
				more = true // 通道空闲（或关闭）：关闭时 More 多报一次无害，客户端以终态事件为准
				break
			}
			events = append(events, next)
			if time.Since(dwellStart) >= eventsMaxDwell {
				more = true
				break
			}
		}
		if len(events) >= s.eventsLimit {
			more = true
		}
	} else if ctx.Err() == nil {
		more = true
	}

	out, err := EnvelopeFrom(ipc.TypeEventStream, EventsPayload{
		RunID:  p.RunID,
		Events: events,
		More:   more,
	}, FrameOpts{RequestID: req.Header.RequestID, SessionID: req.Header.SessionID})
	if err != nil {
		return ErrorFrame(ipc.TypeEventStream, req, err), nil
	}
	return out, nil
}

// handleResume 处理恢复请求：engine.run.resume。
//
// 这是「kill Engine 后 run 自动续跑」这条 P0 红线的服务端一半：Supervisor
// 在把 Engine 子进程重新拉起后，对它发这个帧，Engine 侧据此扫描持久化日志、
// 重建未完成 run 并继续推进。
//
// 载荷可以为空（表示「全量扫描恢复」）；带 run_id 时只针对该 run。
func (s *EngineService) handleResume(ctx context.Context, req *ipc.Frame) (*ipc.Frame, error) {
	if s.recoverer == nil {
		return ErrorFrame(ipc.TypeRunResume, req,
			fmt.Errorf("engine does not support recovery in this build")), nil
	}

	var (
		runID string
		env   *Envelope
	)
	if len(req.Payload) > 0 {
		e, err := decodeEnvelope(req)
		if err != nil {
			return ErrorFrame(ipc.TypeRunResume, req, err), nil
		}
		env = e
		var p RunIDPayload
		if err := env.Decode(&p); err != nil {
			return ErrorFrame(ipc.TypeRunResume, req, err), nil
		}
		runID = p.RunID
	}

	plans, err := s.recoverer.RecoverForIPC(ctx)
	if err != nil {
		return ErrorFrame(ipc.TypeRunResume, req, err), nil
	}

	payload := summarizeRecovery(plans, runID)
	out, err := EnvelopeFrom(ipc.TypeRunResume, payload,
		FrameOpts{RequestID: req.Header.RequestID, SessionID: req.Header.SessionID})
	if err != nil {
		return ErrorFrame(ipc.TypeRunResume, req, err), nil
	}
	return out, nil
}

// summarizeRecovery 把恢复计划按决策归类，形成可读回报。
//
// 注意 ResumeAuto 表示「引擎已经自动续跑」；ResumeAfterConfirm 表示「必须等
// 用户确认才能继续」，这类 run 会被明确列出而不是悄悄跳过——静默跳过正是
// 第38章要避免的「卡住的 run 被遗弃」。
func summarizeRecovery(plans []types.RecoveryPlan, onlyRunID string) RecoveryPayload {
	out := RecoveryPayload{Plans: plans}
	for _, p := range plans {
		if onlyRunID != "" && p.RunID != onlyRunID {
			continue
		}
		switch p.Decision {
		case types.DecisionResumeAuto:
			out.Resumed = append(out.Resumed, p.RunID)
		case types.DecisionResumeAfterConfirm:
			out.NeedsConfirm = append(out.NeedsConfirm, p.RunID)
		case types.DecisionMarkFailed:
			out.Failed = append(out.Failed, p.RunID)
		case types.DecisionSkip:
			// 已终态的 run 无需处理。
		}
	}
	return out
}

// toRunPayload 把运行状态转成线上 DTO。
func toRunPayload(run types.Run) RunPayload {
	p := RunPayload{
		RunID:        run.ID,
		SessionID:    run.SessionID,
		State:        string(run.State),
		Answer:       run.Answer,
		Round:        run.Round,
		UncertainIDs: run.UncertainToolCalls,
	}
	if run.Err != nil {
		p.Error = run.Err.Error()
	}
	return p
}

func decodeRunID(req *ipc.Frame) (string, error) {
	env, err := decodeEnvelope(req)
	if err != nil {
		return "", err
	}
	var p RunIDPayload
	if err := env.Decode(&p); err != nil {
		return "", err
	}
	if p.RunID == "" {
		return "", fmt.Errorf("run_id is required")
	}
	return p.RunID, nil
}

// ---------------------------------------------------------------------------
// 客户端：供 Supervisor / UI 调用对端
// ---------------------------------------------------------------------------

// Client 是对 ipc.Client 的业务封装，把「帧 + 信封」的细节收在内部。
type Client struct {
	c         *ipc.Client
	timeout   time.Duration
	requestID uint64
}

// NewClient 连接指定端点（尚未 Connect）。
func NewClient(endpoint string, maxPayload uint32, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	return &Client{
		c:       ipc.NewClient(endpoint, maxPayload, true),
		timeout: timeout,
	}
}

// Connect 建立连接。
func (c *Client) Connect(ctx context.Context) error { return c.c.Connect(ctx) }

// Close 关闭连接。
func (c *Client) Close() error { return c.c.Close() }

// Ping 探活。
func (c *Client) Ping() error { return c.c.Ping(c.timeout) }

// Raw 暴露底层客户端，供需要订阅推送的场景使用。
func (c *Client) Raw() *ipc.Client { return c.c }

// Submit 提交一个 run。
func (c *Client) Submit(ctx context.Context, p SubmitPayload) (HandlePayload, error) {
	var out HandlePayload
	err := c.call(ctx, ipc.TypeRunSubmit, p, &out)
	return out, err
}

// Cancel 取消一个 run。
func (c *Client) Cancel(ctx context.Context, runID string) error {
	return c.call(ctx, ipc.TypeRunCancel, RunIDPayload{RunID: runID}, nil)
}

// Status 查询一个 run。
func (c *Client) Status(ctx context.Context, runID string) (RunPayload, error) {
	var out RunPayload
	err := c.call(ctx, ipc.TypeRunStatus, RunIDPayload{RunID: runID}, &out)
	return out, err
}

// Events 拉取增量事件。
func (c *Client) Events(ctx context.Context, runID string, afterSeq uint64) (EventsPayload, error) {
	var out EventsPayload
	err := c.call(ctx, ipc.TypeEventStream, map[string]any{
		"run_id":    runID,
		"after_seq": afterSeq,
	}, &out)
	return out, err
}

// Resume 触发恢复。runID 为空表示全量扫描恢复。
func (c *Client) Resume(ctx context.Context, runID string) (RecoveryPayload, error) {
	var out RecoveryPayload
	err := c.call(ctx, ipc.TypeRunResume, RunIDPayload{RunID: runID}, &out)
	return out, err
}

// call 发送请求并解出信封里的业务结果。
func (c *Client) call(ctx context.Context, msgType string, payload any, dst any) error {
	c.requestID++
	frame, err := EnvelopeFrom(msgType, payload, FrameOpts{
		RequestID: fmt.Sprintf("%s-%d", msgType, c.requestID),
		Timeout:   c.timeout,
	})
	if err != nil {
		return err
	}

	reqCtx, cancel := WithTimeout(ctx, c.timeout)
	defer cancel()

	resp, err := c.c.SendRequest(reqCtx, frame)
	if err != nil {
		return err
	}
	env, err := decodeEnvelope(resp)
	if err != nil {
		return err
	}
	return env.Decode(dst)
}

// asJSON 便于调试输出。
func asJSON(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("<unmarshalable %T>", v)
	}
	return string(raw)
}
