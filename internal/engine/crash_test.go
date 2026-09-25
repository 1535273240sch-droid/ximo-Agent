package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/agent"
	"github.com/ximo888ok-netizen/ximo-agent/internal/ports"
	"github.com/ximo888ok-netizen/ximo-agent/internal/ports/mem"
	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// This file implements the acceptance criterion "随机 kill Engine 后 Run 可恢复"
// as a real crash test rather than an in-process simulation.
//
// A true random kill cannot be tested inside one process: the point is that the
// process *dies* with no chance to clean up, leaving only what reached the
// durable log. So the test re-executes its own binary as a child process
// (the standard Go subprocess-test pattern), lets the child drive a run to a
// chosen point, then SIGKILLs it and inspects the surviving log.

// crashChildEnv names the environment variable that turns the test binary into
// the crash child.
const crashChildEnv = "XIMO_CRASH_CHILD"

// crashChildModeEnv selects which crash scenario the child runs.
const crashChildModeEnv = "XIMO_CRASH_MODE"

// crashChildDirEnv points the child at the shared durable directory.
const crashChildDirEnv = "XIMO_CRASH_DIR"

// fileEventStore is an append-only event store backed by a JSONL file, so a
// child process's writes survive its death. It is deliberately the simplest
// thing that is genuinely durable: append a line, flush, fsync.
type fileEventStore struct {
	path string
}

// crashRecord is one line of the on-disk log.
type crashRecord struct {
	RunID string      `json:"runId"`
	Event types.Event `json:"event"`
}

func (s *fileEventStore) Append(_ context.Context, runID string, event types.Event) (uint64, error) {
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	// Sequence numbers are allocated by counting the existing lines for this
	// run. Re-reading on every append is slow but correct, and this store only
	// exists to make the crash test honest.
	existing, err := s.readAll()
	if err != nil {
		return 0, err
	}
	var seq uint64
	for _, rec := range existing {
		if rec.RunID == runID && rec.Event.Seq > seq {
			seq = rec.Event.Seq
		}
	}
	seq++
	event.Seq = seq
	event.RunID = runID
	if event.SeqInRun == 0 {
		event.SeqInRun = seq
	}
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}

	line, err := json.Marshal(crashRecord{RunID: runID, Event: event})
	if err != nil {
		return 0, err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		return 0, err
	}
	// Flush to the OS, then to disk: a SIGKILL loses nothing that was synced,
	// which is what makes the surviving log a faithful record of what happened.
	if err := f.Sync(); err != nil {
		return 0, err
	}
	return seq, nil
}

func (s *fileEventStore) readAll() ([]crashRecord, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []crashRecord
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec crashRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			// A torn final line is expected after a kill mid-write; skip it
			// rather than failing, because that is exactly the situation the
			// recovery path has to tolerate.
			continue
		}
		out = append(out, rec)
	}
	return out, nil
}

func (s *fileEventStore) Read(_ context.Context, runID string, afterSeq uint64, limit int) ([]types.Event, error) {
	recs, err := s.readAll()
	if err != nil {
		return nil, err
	}
	var out []types.Event
	for _, rec := range recs {
		if rec.RunID != runID || rec.Event.Seq <= afterSeq {
			continue
		}
		out = append(out, rec.Event)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (s *fileEventStore) LastSeq(_ context.Context, runID string) (uint64, error) {
	recs, err := s.readAll()
	if err != nil {
		return 0, err
	}
	var seq uint64
	for _, rec := range recs {
		if rec.RunID == runID && rec.Event.Seq > seq {
			seq = rec.Event.Seq
		}
	}
	return seq, nil
}

func (s *fileEventStore) ListRuns(_ context.Context) ([]string, error) {
	recs, err := s.readAll()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, rec := range recs {
		if !seen[rec.RunID] {
			seen[rec.RunID] = true
			out = append(out, rec.RunID)
		}
	}
	return out, nil
}

// TestCrashChild is not a test: it is the child-process entry point. It is
// skipped unless the environment selects a crash mode.
func TestCrashChild(t *testing.T) {
	mode := os.Getenv(crashChildModeEnv)
	if mode == "" {
		t.Skip("not the crash child")
	}
	dir := os.Getenv(crashChildDirEnv)
	if dir == "" {
		t.Fatalf("crash child needs %s", crashChildDirEnv)
	}

	store := &fileEventStore{path: filepath.Join(dir, "events.jsonl")}
	ckpts := mem.NewCheckpointStore()
	idem := mem.NewIdempotencyStore()

	// The tool blocks forever: the child is killed while a tool call is
	// genuinely in flight, which is the interesting recovery case.
	tools := mem.NewToolRuntime(func(ctx context.Context, req ports.ToolRequest) (types.ToolResult, error) {
		// Announce that the tool has started by writing the started event
		// through the engine, then block. Signalling here is what lets the
		// parent kill at a moment when the call is provably in flight.
		close(childToolStarted)
		<-ctx.Done()
		return types.ToolResult{Success: false, Error: "cancelled"}, ctx.Err()
	})

	provider := mem.NewProvider(
		// Round 1 asks for the tool that will hang.
		mem.ToolCallRound(mem.NewCall("call-hang", "terminal", nil)),
		// If the child ever gets past the tool it would answer; it is killed
		// before that.
		mem.FinalRound("child finished"),
	)

	guard := agent.NewPanicGuard(agent.GuardConfig{DumpDir: filepath.Join(dir, "crashes")})
	engine, err := New(engineConfig(), Dependencies{
		Events:      store,
		Outbox:      mem.NewOutboxStore(),
		Checkpoints: ckpts,
		Idempotency: idem,
		Tools:       tools,
		Provider:    provider,
	}, guard)
	if err != nil {
		t.Fatalf("child engine: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := engine.Submit(ctx, types.SubmitRequest{
		Prompt: "run the hanging tool", SessionID: "ses-crash", Model: "test",
	}); err != nil {
		t.Fatalf("child submit: %v", err)
	}

	// Wait for the tool to be in flight, then idle. The parent kills this
	// process at that point. A ticker rather than a bare `select {}` keeps a
	// runtime thread scheduled, so the process is genuinely alive and killable
	// rather than parked in a way that could look like a hang to the parent.
	select {
	case <-childToolStarted:
	case <-time.After(30 * time.Second):
		t.Fatal("child never reached the tool call")
	}
	for range time.Tick(time.Second) {
	}
}

// childToolStarted lets the child signal that a tool call is in flight.
var childToolStarted = make(chan struct{})

// waitForStartedTool polls the durable log until some run has recorded a
// started tool call, and returns that run's ID.
//
// The run ID is discovered rather than predetermined: Engine.Submit allocates
// it, so the parent cannot know it up front. Polling ListRuns also proves the
// discovery path recovery itself relies on.
func waitForStartedTool(t *testing.T, store *fileEventStore, within time.Duration) string {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		runIDs, err := store.ListRuns(ctx)
		if err == nil {
			for _, id := range runIDs {
				for _, ev := range mustEvents(store, id) {
					if ev.Type == types.EventToolStarted {
						return id
					}
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return ""
}

// mustEvents reads a run's events, ignoring a torn trailing line.
func mustEvents(store *fileEventStore, runID string) []types.Event {
	events, err := store.Read(context.Background(), runID, 0, 0)
	if err != nil {
		return nil
	}
	return events
}

// TestRandomKillThenRecover is the acceptance test: kill the engine while a tool
// call is in flight, then verify a fresh engine recovers the run and refuses to
// repeat the non-idempotent call without confirmation.
func TestRandomKillThenRecover(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping crash test in short mode")
	}
	dir := t.TempDir()

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}

	cmd := exec.Command(self, "-test.run=TestCrashChild$", "-test.v")
	cmd.Env = append(os.Environ(),
		crashChildModeEnv+"=hang_in_tool",
		crashChildDirEnv+"="+dir,
	)
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &out

	if err := cmd.Start(); err != nil {
		t.Fatalf("start crash child: %v", err)
	}
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	}()

	// Wait until the child has durably recorded a tool call as started, which is
	// the moment the crash becomes interesting: a call is provably in flight.
	store := &fileEventStore{path: filepath.Join(dir, "events.jsonl")}
	runID := waitForStartedTool(t, store, 90*time.Second)
	if runID == "" {
		t.Fatalf("the child never durably recorded a started tool call; output:\n%s", out.String())
	}

	// SIGKILL: no cleanup, no flush beyond what was already synced, no
	// opportunity to write a shutdown event. This is the crash the architecture
	// has to survive.
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill child: %v", err)
	}
	_ = cmd.Wait()

	// The durable log must have survived, and must show the call as started but
	// never completed.
	events := mustEvents(store, runID)
	if len(events) == 0 {
		t.Fatal("no events survived the kill")
	}
	var sawStarted, sawCompleted bool
	for _, ev := range events {
		if ev.Type == types.EventToolStarted {
			sawStarted = true
		}
		if ev.Type == types.EventToolCompleted {
			sawCompleted = true
		}
	}
	if !sawStarted {
		t.Fatalf("the surviving log lost the started marker: %v", eventTypesOf(events))
	}
	if sawCompleted {
		t.Fatal("the tool reported completion although the process was killed mid-call")
	}

	// A fresh engine, over the same durable log, must recover the run.
	idem := mem.NewIdempotencyStore()
	tools := mem.NewToolRuntime(nil)
	engine, err := New(engineConfig(), Dependencies{
		Events:      store,
		Outbox:      mem.NewOutboxStore(),
		Checkpoints: mem.NewCheckpointStore(),
		Idempotency: idem,
		Tools:       tools,
		Provider:    mem.NewProvider(mem.FinalRound("recovered answer")),
	}, agent.NewPanicGuard(agent.GuardConfig{DumpDir: filepath.Join(dir, "recover-crashes")}))
	if err != nil {
		t.Fatalf("recovery engine: %v", err)
	}
	defer engine.Close()

	ctx := context.Background()
	plans, err := engine.Recover(ctx)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	var plan RecoveryPlan
	found := false
	for _, p := range plans {
		if p.RunID == runID {
			plan, found = p, true
		}
	}
	if !found {
		t.Fatalf("recovery produced no plan for the killed run %s: %+v", runID, plans)
	}
	if !plan.SequenceOK {
		t.Errorf("the surviving log failed sequence verification: %v", plan.Notes)
	}

	// terminal is non-idempotent, so the interrupted call must be flagged for
	// confirmation rather than replayed.
	if plan.Decision != ResumeAfterConfirm {
		t.Fatalf("decision = %s, want %s: a killed non-idempotent call must not be replayed automatically; notes=%v",
			plan.Decision, ResumeAfterConfirm, plan.Notes)
	}
	if len(plan.UncertainToolCalls) != 1 {
		t.Errorf("uncertain calls = %v, want the one interrupted tool call", plan.UncertainToolCalls)
	}

	// The recovered run must be parked, and the tool must not have run again.
	run, err := engine.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun after recovery: %v", err)
	}
	if run.State != types.StateWaitingUser {
		t.Errorf("state = %s, want waiting_user", run.State)
	}
	if tools.CallCount() != 0 {
		t.Errorf("recovery re-executed %d tool calls; a non-idempotent call must not be repeated",
			tools.CallCount())
	}

	// Finally, an explicit resume must be accepted and must record the decision.
	if err := engine.Resume(ctx, runID); err != nil {
		t.Fatalf("Resume after recovery: %v", err)
	}
	if !containsType(mustReadTypes(t, store, runID), types.EventUserInputReceived) {
		t.Errorf("the resume approval was not recorded durably; saw %v", mustReadTypes(t, store, runID))
	}
}

// TestRandomKillWithIdempotentToolRecovers checks the other side of the same
// scenario: when the interrupted call is idempotent, recovery resumes without
// demanding confirmation.
func TestRandomKillWithIdempotentToolRecovers(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping crash test in short mode")
	}
	dir := t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}

	cmd := exec.Command(self, "-test.run=TestCrashChildIdempotent$", "-test.v")
	cmd.Env = append(os.Environ(),
		crashChildModeEnv+"=hang_in_idempotent_tool",
		crashChildDirEnv+"="+dir,
	)
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &out

	if err := cmd.Start(); err != nil {
		t.Fatalf("start crash child: %v", err)
	}
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	}()

	store := &fileEventStore{path: filepath.Join(dir, "events.jsonl")}
	runID := waitForStartedTool(t, store, 90*time.Second)
	if runID == "" {
		t.Fatalf("child never recorded a started tool call; output:\n%s", out.String())
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill child: %v", err)
	}
	_ = cmd.Wait()

	engine, err := New(engineConfig(), Dependencies{
		Events:      store,
		Outbox:      mem.NewOutboxStore(),
		Checkpoints: mem.NewCheckpointStore(),
		Idempotency: mem.NewIdempotencyStore(),
		Tools:       mem.NewToolRuntime(nil),
		Provider:    mem.NewProvider(mem.FinalRound("recovered")),
	}, agent.NewPanicGuard(agent.GuardConfig{DumpDir: t.TempDir()}))
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()

	plans, err := engine.Recover(context.Background())
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	var plan RecoveryPlan
	found := false
	for _, p := range plans {
		if p.RunID == runID {
			plan, found = p, true
		}
	}
	if !found {
		t.Fatalf("no recovery plan for %s: %+v", runID, plans)
	}
	if plan.Decision != ResumeAuto {
		t.Errorf("decision = %s, want %s for an idempotent call: %v",
			plan.Decision, ResumeAuto, plan.Notes)
	}
	if len(plan.UncertainToolCalls) != 0 {
		t.Errorf("uncertain calls = %v, want none for an idempotent call", plan.UncertainToolCalls)
	}
}

// TestCrashChildIdempotent is the child entry point for the idempotent variant.
func TestCrashChildIdempotent(t *testing.T) {
	mode := os.Getenv(crashChildModeEnv)
	if mode != "hang_in_idempotent_tool" {
		t.Skip("not the idempotent crash child")
	}
	dir := os.Getenv(crashChildDirEnv)
	if dir == "" {
		t.Fatalf("crash child needs %s", crashChildDirEnv)
	}

	store := &fileEventStore{path: filepath.Join(dir, "events.jsonl")}
	tools := mem.NewToolRuntime(func(ctx context.Context, _ ports.ToolRequest) (types.ToolResult, error) {
		close(childToolStarted)
		<-ctx.Done()
		return types.ToolResult{Success: false, Error: "cancelled"}, ctx.Err()
	})
	provider := mem.NewProvider(
		// file_read is idempotent in the default classification table.
		mem.ToolCallRound(mem.NewCall("call-read", "file_read", nil)),
		mem.FinalRound("child finished"),
	)
	engine, err := New(engineConfig(), Dependencies{
		Events:      store,
		Outbox:      mem.NewOutboxStore(),
		Checkpoints: mem.NewCheckpointStore(),
		Idempotency: mem.NewIdempotencyStore(),
		Tools:       tools,
		Provider:    provider,
	}, agent.NewPanicGuard(agent.GuardConfig{DumpDir: filepath.Join(dir, "crashes")}))
	if err != nil {
		t.Fatalf("child engine: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := engine.Submit(ctx, types.SubmitRequest{
		Prompt: "read the file", SessionID: "ses-idem", Model: "test",
	}); err != nil {
		t.Fatalf("child submit: %v", err)
	}
	select {
	case <-childToolStarted:
	case <-time.After(30 * time.Second):
		t.Fatal("child never reached the tool call")
	}
	for range time.Tick(time.Second) {
	}
}

// mustReadTypes reads a run's event types from a file-backed store.
func mustReadTypes(t *testing.T, store *fileEventStore, runID string) []types.EventType {
	t.Helper()
	events, err := store.Read(context.Background(), runID, 0, 0)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	return eventTypesOf(events)
}

// TestFileEventStoreSurvivesReopen checks the crash test's own foundation: the
// log it relies on must genuinely round-trip through the filesystem.
func TestFileEventStoreSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")

	first := &fileEventStore{path: path}
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := first.Append(ctx, "run-1", types.Event{
			Type: types.EventRunStateChanged, State: types.StateThinking,
		}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	// A different store instance over the same file, as a restart would create.
	second := &fileEventStore{path: path}
	events, err := second.Read(ctx, "run-1", 0, 0)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("events = %d, want 3", len(events))
	}
	for i, ev := range events {
		if ev.Seq != uint64(i+1) {
			t.Errorf("event %d has seq %d, want %d", i, ev.Seq, i+1)
		}
	}
	if runs, err := second.ListRuns(ctx); err != nil || len(runs) != 1 {
		t.Errorf("ListRuns = %v (err=%v), want one run", runs, err)
	}
	// Sequence allocation must continue, not restart, after a reopen.
	seq, err := second.Append(ctx, "run-1", types.Event{Type: types.EventRunCompleted})
	if err != nil {
		t.Fatal(err)
	}
	if seq != 4 {
		t.Errorf("sequence after reopen = %d, want 4", seq)
	}
}

// TestRandomKillManyTimes is the "random kill" leg of the acceptance criterion:
// it repeats the kill-and-recover cycle with the kill landing at varying points
// along the run, and asserts that every surviving log is recoverable.
func TestRandomKillManyTimes(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping repeated crash test in short mode")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	// Each iteration kills after waiting a different amount, so the kill lands
	// at a different point in the run rather than always at the same instant.
	for _, wait := range []time.Duration{
		0, 5 * time.Millisecond, 15 * time.Millisecond, 40 * time.Millisecond,
	} {
		t.Run(fmt.Sprintf("kill-after-%s", wait), func(t *testing.T) {
			dir := t.TempDir()

			cmd := exec.Command(self, "-test.run=TestCrashChild$", "-test.v")
			cmd.Env = append(os.Environ(),
				crashChildModeEnv+"=hang_in_tool",
				crashChildDirEnv+"="+dir,
			)
			var out strings.Builder
			cmd.Stdout = &out
			cmd.Stderr = &out
			if err := cmd.Start(); err != nil {
				t.Fatalf("start child: %v", err)
			}

			time.Sleep(wait)
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()

			// Whatever survived, recovery must reach a coherent verdict: either
			// the log is too short to contain a run (nothing to recover), or it
			// yields a well-formed plan.
			store := &fileEventStore{path: filepath.Join(dir, "events.jsonl")}
			engine, err := New(engineConfig(), Dependencies{
				Events:      store,
				Outbox:      mem.NewOutboxStore(),
				Checkpoints: mem.NewCheckpointStore(),
				Idempotency: mem.NewIdempotencyStore(),
				Tools:       mem.NewToolRuntime(nil),
				Provider:    mem.NewProvider(mem.FinalRound("ok")),
			}, agent.NewPanicGuard(agent.GuardConfig{DumpDir: t.TempDir()}))
			if err != nil {
				t.Fatal(err)
			}
			defer engine.Close()

			plans, err := engine.Recover(context.Background())
			if err != nil {
				t.Fatalf("Recover after a kill at %s: %v", wait, err)
			}
			for _, plan := range plans {
				if !plan.Decision.Valid() {
					t.Errorf("run %s got an invalid recovery decision %q", plan.RunID, plan.Decision)
				}
				// An unsound sequence must never be resumed, whatever else the
				// plan says.
				if !plan.SequenceOK && plan.Decision != MarkFailed && plan.Decision != Skip {
					t.Errorf("run %s has an unsound sequence but was not marked failed: %v",
						plan.RunID, plan.Notes)
				}
			}
		})
	}
}
