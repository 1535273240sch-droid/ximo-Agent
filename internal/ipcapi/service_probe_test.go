package ipcapi

// 任务2 Part A 的诊断探针（临时）：验证事件长轮询在「连续流式」场景下的
// 归还节奏。修复前，handleEvents 只靠 eventsIdleWait（1.2s 空闲）归还，
// 连续流式永远凑不出空闲，一批要攒满 eventsLimit（1000 条）或等请求超时；
// 修复后受 eventsMaxDwell（500ms）约束，单次轮询应小批快返。

import (
	"context"
	"testing"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/ipc"
	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// dwellEngine 是只支持 Events 的最小 types.Engine 桩。
type dwellEngine struct {
	ch <-chan types.Event
}

func (e *dwellEngine) Submit(context.Context, types.SubmitRequest) (types.RunHandle, error) {
	return types.RunHandle{}, nil
}
func (e *dwellEngine) Cancel(context.Context, string) error { return nil }
func (e *dwellEngine) Resume(context.Context, string) error { return nil }
func (e *dwellEngine) GetRun(context.Context, string) (types.Run, error) {
	return types.Run{}, nil
}
func (e *dwellEngine) Events(_ context.Context, _ string, _ uint64) (<-chan types.Event, error) {
	return e.ch, nil
}

// TestHandleEventsDwellBound 模拟 100 个 token.delta 以 50ms 间隔持续到达
// （对应 coalescer 的 50ms 帧距）。连续流式下没有空闲间隙，单次轮询必须由
// dwell 上界收口：在远小于总流时长内返回一个远小于总量的小批量。
func TestHandleEventsDwellBound(t *testing.T) {
	const total = 100
	const pace = 50 * time.Millisecond

	src := make(chan types.Event, 16)
	go func() {
		defer close(src)
		for i := 0; i < total; i++ {
			src <- types.Event{
				RunID: "run-1",
				Type:  types.EventTokenDelta,
				Data:  map[string]any{"content": "x"},
			}
			time.Sleep(pace)
		}
	}()

	svc, err := NewEngineService(&dwellEngine{ch: src})
	if err != nil {
		t.Fatalf("NewEngineService: %v", err)
	}

	req, err := EnvelopeFrom(ipc.TypeEventStream,
		map[string]any{"run_id": "run-1", "after_seq": uint64(0)},
		FrameOpts{RequestID: "probe-1"})
	if err != nil {
		t.Fatalf("build request frame: %v", err)
	}

	start := time.Now()
	resp, err := svc.handleEvents(context.Background(), req)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("handleEvents: %v", err)
	}

	env, err := decodeEnvelope(resp)
	if err != nil {
		t.Fatalf("decode response: %v", err)
	}
	var out EventsPayload
	if err := env.Decode(&out); err != nil {
		t.Fatalf("decode payload: %v", err)
	}

	// 总流时长为 total*pace = 5s。修复后单次轮询应在 dwell+余量内归还；
	// 若退化回「攒满/等空闲」，这里会逼近 5s。
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("long-poll held for %v during continuous streaming; want return within dwell bound", elapsed)
	}
	if len(out.Events) >= total {
		t.Fatalf("single poll returned %d/%d events; want a small batch", len(out.Events), total)
	}
	if !out.More {
		t.Fatalf("More should be true when the batch was cut short by the dwell bound")
	}
	t.Logf("probe: single poll returned %d events in %v (More=%v)", len(out.Events), elapsed, out.More)
}
