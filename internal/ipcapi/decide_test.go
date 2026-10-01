package ipcapi

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/ximo888ok-netizen/ximo-agent/internal/ipc"
	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// This file pins the IPC half of F5: engine.run.decide must reach the engine's
// Decide with the payload verbatim, and must never report success when it did
// not. A silent success would close the approval card while the run stays parked
// forever, which is the exact failure this feature exists to remove.

// stubDecider records the decisions it receives. It also satisfies types.Engine,
// which is what NewEngineService's optional-capability assertion expects.
type stubDecider struct {
	runID    string
	callID   string
	approve  bool
	remember string
	calls    int
	err      error
}

func (s *stubDecider) Submit(context.Context, types.SubmitRequest) (types.RunHandle, error) {
	return types.RunHandle{}, nil
}
func (s *stubDecider) Cancel(context.Context, string) error { return nil }
func (s *stubDecider) Resume(context.Context, string) error { return nil }
func (s *stubDecider) GetRun(context.Context, string) (types.Run, error) {
	return types.Run{}, nil
}
func (s *stubDecider) Events(context.Context, string, uint64) (<-chan types.Event, error) {
	ch := make(chan types.Event)
	close(ch)
	return ch, nil
}

func (s *stubDecider) Decide(_ context.Context, runID, callID string, approve bool, remember string) error {
	s.calls++
	s.runID, s.callID, s.approve, s.remember = runID, callID, approve, remember
	return s.err
}

// stubPlainEngine satisfies types.Engine but not types.ToolDecider, so a build
// without the capability can be exercised.
type stubPlainEngine struct{ calls int }

func (s *stubPlainEngine) Submit(context.Context, types.SubmitRequest) (types.RunHandle, error) {
	return types.RunHandle{}, nil
}
func (s *stubPlainEngine) Cancel(context.Context, string) error { return nil }
func (s *stubPlainEngine) Resume(context.Context, string) error { return nil }
func (s *stubPlainEngine) GetRun(context.Context, string) (types.Run, error) {
	return types.Run{}, nil
}
func (s *stubPlainEngine) Events(context.Context, string, uint64) (<-chan types.Event, error) {
	ch := make(chan types.Event)
	close(ch)
	return ch, nil
}

// decideFrame builds a request frame carrying a tool-decision payload.
func decideFrame(t *testing.T, payload any) *ipc.Frame {
	t.Helper()
	env, err := NewOK(payload)
	if err != nil {
		t.Fatalf("NewOK: %v", err)
	}
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return &ipc.Frame{
		Header:  ipc.FrameHeader{Version: 1, RequestID: "req-1", Type: ipc.TypeRunDecide},
		Payload: raw,
	}
}

// TestRunDecideHandlerForwardsTheDecision checks that both answers and the
// remember scope reach the engine with the run and call they name.
func TestRunDecideHandlerForwardsTheDecision(t *testing.T) {
	for _, approve := range []bool{true, false} {
		stub := &stubDecider{}
		svc, err := NewEngineService(stub)
		if err != nil {
			t.Fatalf("NewEngineService: %v", err)
		}

		resp, herr := svc.handleDecide(context.Background(), decideFrame(t, DecidePayload{
			RunID: "run-1", CallID: "call-1", Approve: approve, Remember: "session",
		}))
		if herr != nil {
			t.Fatalf("handleDecide: %v", herr)
		}
		if env := decodeResp(t, resp); !env.OK {
			t.Fatalf("approve=%v: response was not OK: %s", approve, env.Error)
		}
		if stub.calls != 1 {
			t.Fatalf("approve=%v: Decide called %d times, want 1", approve, stub.calls)
		}
		if stub.runID != "run-1" || stub.callID != "call-1" {
			t.Errorf("approve=%v: decide target = %s/%s, want run-1/call-1", approve, stub.runID, stub.callID)
		}
		// The boolean must survive the round trip in both directions: approve
		// and deny are opposite actions behind one endpoint.
		if stub.approve != approve {
			t.Errorf("approve = %v, want %v", stub.approve, approve)
		}
		if stub.remember != "session" {
			t.Errorf("remember = %q, want session", stub.remember)
		}
	}
}

// TestRunDecideHandlerRejectsBadRequests checks the input guards: a malformed or
// incomplete request must not reach the engine.
func TestRunDecideHandlerRejectsBadRequests(t *testing.T) {
	cases := map[string]struct {
		payload any
		raw     []byte
	}{
		"missing run id":  {payload: DecidePayload{CallID: "call-1", Approve: true}},
		"missing call id": {payload: DecidePayload{RunID: "run-1", Approve: true}},
		"malformed json":  {raw: []byte("{not json")},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			stub := &stubDecider{}
			svc, err := NewEngineService(stub)
			if err != nil {
				t.Fatalf("NewEngineService: %v", err)
			}
			frame := decideFrame(t, tc.payload)
			if tc.raw != nil {
				frame.Payload = tc.raw
			}
			resp, herr := svc.handleDecide(context.Background(), frame)
			if herr != nil {
				t.Fatalf("handleDecide: %v", herr)
			}
			if env := decodeResp(t, resp); env.OK {
				t.Fatal("a bad request must be refused")
			}
			if stub.calls != 0 {
				t.Error("a refused request must not reach the engine")
			}
		})
	}
}

// TestRunDecideHandlerRequiresTheCapability checks that an engine without a
// ToolDecider reports the missing capability instead of pretending success.
func TestRunDecideHandlerRequiresTheCapability(t *testing.T) {
	svc, err := NewEngineService(&stubPlainEngine{})
	if err != nil {
		t.Fatalf("NewEngineService: %v", err)
	}
	resp, herr := svc.handleDecide(context.Background(), decideFrame(t, DecidePayload{
		RunID: "run-1", CallID: "call-1", Approve: true,
	}))
	if herr != nil {
		t.Fatalf("handleDecide: %v", herr)
	}
	if env := decodeResp(t, resp); env.OK {
		t.Error("a missing engine capability must produce an error, not a silent success")
	}
}

// TestRunDecideHandlerSurfacesTheEngineError checks that a rejected decision
// (unknown run, wrong state, call not pending) is reported to the caller.
func TestRunDecideHandlerSurfacesTheEngineError(t *testing.T) {
	stub := &stubDecider{err: errors.New("run run-1 has no tool call call-1 awaiting authorization")}
	svc, err := NewEngineService(stub)
	if err != nil {
		t.Fatalf("NewEngineService: %v", err)
	}
	resp, herr := svc.handleDecide(context.Background(), decideFrame(t, DecidePayload{
		RunID: "run-1", CallID: "call-1", Approve: true,
	}))
	if herr != nil {
		t.Fatalf("handleDecide: %v", herr)
	}
	env := decodeResp(t, resp)
	if env.OK {
		t.Fatal("an engine error must not be reported as success")
	}
	if env.Error == "" {
		t.Error("the failure reason should be carried to the caller")
	}
}

// TestRunDecideFrameTypeMatchesContract pins the frame name, shared between this
// service, the supervisor's forwarder and the frontend.
func TestRunDecideFrameTypeMatchesContract(t *testing.T) {
	if ipc.TypeRunDecide != "engine.run.decide" {
		t.Errorf("decide frame = %q, want engine.run.decide", ipc.TypeRunDecide)
	}
}

// TestDecidePayloadWireNames pins the field names the frontend sends: a mismatch
// would let the UI approve one call while the engine answered another.
func TestDecidePayloadWireNames(t *testing.T) {
	raw, err := json.Marshal(DecidePayload{
		RunID: "run-1", CallID: "call-1", Approve: true, Remember: "session",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"run_id", "call_id", "approve", "remember"} {
		if _, ok := got[key]; !ok {
			t.Errorf("payload is missing %q: %s", key, raw)
		}
	}
	// The fields are append-only: an older client that omits them must still
	// decode, and an omitted approve must mean deny rather than approve.
	var legacy DecidePayload
	if err := json.Unmarshal([]byte(`{"run_id":"r","call_id":"c"}`), &legacy); err != nil {
		t.Fatalf("legacy payload must decode: %v", err)
	}
	if legacy.Approve {
		t.Error("an omitted approve must not default to true")
	}
}
