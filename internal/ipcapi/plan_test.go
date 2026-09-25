package ipcapi

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/ximo888ok-netizen/ximo-agent/internal/ipc"
)

// stubConfirmer records the decisions it receives.
type stubConfirmer struct {
	runID    string
	approved bool
	calls    int
	err      error
}

func (s *stubConfirmer) ConfirmPlan(_ context.Context, runID string, approved bool) error {
	s.calls++
	s.runID = runID
	s.approved = approved
	return s.err
}

// confirmFrame builds a request frame carrying a plan-confirmation payload.
func confirmFrame(t *testing.T, payload any) *ipc.Frame {
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
		Header:  ipc.FrameHeader{Version: 1, RequestID: "req-1", Type: ipc.TypePlanConfirm},
		Payload: raw,
	}
}

// decodeResp unwraps a handler response so the test can assert on ok/error.
func decodeResp(t *testing.T, f *ipc.Frame) *Envelope {
	t.Helper()
	env, err := decodeEnvelope(f)
	if err != nil {
		t.Fatalf("decodeEnvelope: %v", err)
	}
	return env
}

// TestPlanConfirmHandlerForwardsDecision checks both answers reach the engine
// with the run they name.
func TestPlanConfirmHandlerForwardsDecision(t *testing.T) {
	for _, approved := range []bool{true, false} {
		stub := &stubConfirmer{}
		svc := NewPlanService(stub)

		resp, err := svc.handlePlanConfirm(context.Background(),
			confirmFrame(t, PlanConfirmPayload{RunID: "run-1", Approved: approved}))
		if err != nil {
			t.Fatalf("handlePlanConfirm: %v", err)
		}
		if env := decodeResp(t, resp); !env.OK {
			t.Fatalf("approved=%v: response was not OK: %s", approved, env.Error)
		}
		if stub.calls != 1 {
			t.Fatalf("approved=%v: ConfirmPlan called %d times, want 1", approved, stub.calls)
		}
		if stub.runID != "run-1" {
			t.Errorf("approved=%v: run id = %q, want run-1", approved, stub.runID)
		}
		// The boolean must survive the round trip in both directions, since
		// "approve" and "re-plan" are opposite actions behind one endpoint.
		if stub.approved != approved {
			t.Errorf("approved = %v, want %v", stub.approved, approved)
		}
	}
}

// TestPlanConfirmHandlerRejectsBadRequests checks the input guards.
func TestPlanConfirmHandlerRejectsBadRequests(t *testing.T) {
	t.Run("missing run id", func(t *testing.T) {
		stub := &stubConfirmer{}
		svc := NewPlanService(stub)
		resp, err := svc.handlePlanConfirm(context.Background(),
			confirmFrame(t, PlanConfirmPayload{RunID: "", Approved: true}))
		if err != nil {
			t.Fatalf("handlePlanConfirm: %v", err)
		}
		if env := decodeResp(t, resp); env.OK {
			t.Error("a request without run_id must be refused")
		}
		if stub.calls != 0 {
			t.Error("a refused request must not reach the engine")
		}
	})

	t.Run("no confirmer available", func(t *testing.T) {
		svc := NewPlanService(nil)
		resp, err := svc.handlePlanConfirm(context.Background(),
			confirmFrame(t, PlanConfirmPayload{RunID: "run-1", Approved: true}))
		if err != nil {
			t.Fatalf("handlePlanConfirm: %v", err)
		}
		// A build without the capability must say so rather than pretend success,
		// or the UI would close the card while the run stays parked.
		if env := decodeResp(t, resp); env.OK {
			t.Error("a missing confirmer must produce an error, not a silent success")
		}
	})

	t.Run("malformed payload", func(t *testing.T) {
		stub := &stubConfirmer{}
		svc := NewPlanService(stub)
		resp, err := svc.handlePlanConfirm(context.Background(), &ipc.Frame{
			Header:  ipc.FrameHeader{Version: 1, RequestID: "req-1", Type: ipc.TypePlanConfirm},
			Payload: []byte("{not json"),
		})
		if err != nil {
			t.Fatalf("handlePlanConfirm: %v", err)
		}
		if env := decodeResp(t, resp); env.OK {
			t.Error("a malformed payload must be refused")
		}
		if stub.calls != 0 {
			t.Error("a malformed payload must not reach the engine")
		}
	})
}

// TestPlanConfirmHandlerSurfacesEngineError checks that a rejected confirmation
// (for example a run with no pending plan) is reported to the caller instead of
// looking like success.
func TestPlanConfirmHandlerSurfacesEngineError(t *testing.T) {
	stub := &stubConfirmer{err: errors.New("run run-1 has no plan awaiting confirmation")}
	svc := NewPlanService(stub)

	resp, err := svc.handlePlanConfirm(context.Background(),
		confirmFrame(t, PlanConfirmPayload{RunID: "run-1", Approved: true}))
	if err != nil {
		t.Fatalf("handlePlanConfirm: %v", err)
	}
	env := decodeResp(t, resp)
	if env.OK {
		t.Fatal("an engine error must not be reported as success")
	}
	if env.Error == "" {
		t.Error("the failure reason should be carried to the caller")
	}
}

// TestPlanConfirmFrameTypeMatchesContract pins the frame name, which is a shared
// contract between this service, the supervisor's forwarder and the frontend.
func TestPlanConfirmFrameTypeMatchesContract(t *testing.T) {
	if ipc.TypePlanConfirm != "engine.plan.confirm" {
		t.Errorf("plan confirm frame = %q, want engine.plan.confirm", ipc.TypePlanConfirm)
	}
}

// TestSubmitPayloadCarriesPlanMode checks the appended field's wire name, since
// the frontend sends plan_mode and a mismatch would silently disable the feature.
func TestSubmitPayloadCarriesPlanMode(t *testing.T) {
	raw, err := json.Marshal(SubmitPayload{Prompt: "hi", PlanMode: true})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if v, _ := got["plan_mode"].(bool); !v {
		t.Errorf("plan_mode missing from the wire payload: %s", raw)
	}
	// The pre-existing fields must still be decodable: this task only appends.
	// They are omitempty, so they are asserted by decoding a payload that sets
	// them rather than by looking for them in a minimal one.
	full, err := json.Marshal(SubmitPayload{
		SessionID: "sess-1", Prompt: "hi", SystemPrompt: "sys", Model: "m",
		LongTask: true, Priority: "interactive", MaxRounds: 7, PlanMode: true,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded SubmitPayload
	if err := json.Unmarshal(full, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := SubmitPayload{
		SessionID: "sess-1", Prompt: "hi", SystemPrompt: "sys", Model: "m",
		LongTask: true, Priority: "interactive", MaxRounds: 7, PlanMode: true,
	}
	if decoded != want {
		t.Errorf("round trip changed the payload:\n got %+v\nwant %+v", decoded, want)
	}
	// A run that does not opt in must omit the field entirely, so the payload is
	// byte-identical to the pre-task-4 one.
	rawOff, err := json.Marshal(SubmitPayload{Prompt: "hi"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var off map[string]any
	if err := json.Unmarshal(rawOff, &off); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, present := off["plan_mode"]; present {
		t.Errorf("plan_mode should be omitted when false: %s", rawOff)
	}
}
