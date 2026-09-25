package repository

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/checkpoint"
	"github.com/ximo888ok-netizen/ximo-agent/internal/storage"
	"github.com/ximo888ok-netizen/ximo-agent/internal/storage/migrations"
	"github.com/ximo888ok-netizen/ximo-agent/internal/storage/sqlite"
)

func newTestRepos(t *testing.T) *Repos {
	t.Helper()
	path := filepath.Join(t.TempDir(), "repo.db")
	store, err := storage.Open(sqlite.DefaultConfig(path))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := migrations.ApplyFromDir(context.Background(), store.DB(), filepath.Join("..", "..", "..", "migrations")); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return New(store)
}

func newSessionAndRun(t *testing.T, r *Repos) (sessionID, runID string) {
	t.Helper()
	ctx := context.Background()
	sessionID = "sess-1"
	if err := r.Sessions.Create(ctx, &Session{ID: sessionID, Title: "t", Mode: "default"}); err != nil {
		t.Fatalf("session create: %v", err)
	}
	runID = "run-1"
	created, err := r.Runs.Create(ctx, &Run{ID: runID, SessionID: sessionID, Status: RunStatusRunning})
	if err != nil || !created {
		t.Fatalf("run create: %v created=%v", err, created)
	}
	return sessionID, runID
}

func TestSessionCRUD(t *testing.T) {
	r := newTestRepos(t)
	ctx := context.Background()
	if err := r.Sessions.Create(ctx, &Session{ID: "s1", Title: "hello", Mode: "default"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := r.Sessions.Get(ctx, "s1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Title != "hello" || got.Mode != "default" {
		t.Errorf("got %+v", got)
	}
	if err := r.Sessions.Update(ctx, &Session{ID: "s1", Title: "renamed"}); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, _ = r.Sessions.Get(ctx, "s1")
	if got.Title != "renamed" {
		t.Errorf("title = %q, want renamed", got.Title)
	}
	list, err := r.Sessions.List(ctx, 10, 0)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v len=%d", err, len(list))
	}
	if err := r.Sessions.Delete(ctx, "s1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := r.Sessions.Get(ctx, "s1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("get after delete: %v, want ErrNotFound", err)
	}
}

func TestRunIdempotencyKey(t *testing.T) {
	r := newTestRepos(t)
	ctx := context.Background()
	if err := r.Sessions.Create(ctx, &Session{ID: "s1", Mode: "default"}); err != nil {
		t.Fatalf("session: %v", err)
	}
	created, err := r.Runs.Create(ctx, &Run{ID: "r1", SessionID: "s1", IdempotencyKey: "key-1"})
	if err != nil || !created {
		t.Fatalf("create: %v created=%v", err, created)
	}
	// Same idempotency key: no second durable side effect (I5).
	created2, err := r.Runs.Create(ctx, &Run{ID: "r2", SessionID: "s1", IdempotencyKey: "key-1"})
	if err != nil {
		t.Fatalf("create dup: %v", err)
	}
	if created2 {
		t.Error("duplicate idempotency key must not create a second run")
	}
	got, err := r.Runs.GetByIdempotencyKey(ctx, "key-1")
	if err != nil {
		t.Fatalf("get by key: %v", err)
	}
	if got.ID != "r1" {
		t.Errorf("run id = %q, want r1", got.ID)
	}
}

func TestCompleteToolCallAtomicSuccess(t *testing.T) {
	r := newTestRepos(t)
	ctx := context.Background()
	_, runID := newSessionAndRun(t, r)

	tc := &ToolCall{ID: "tc-1", RunID: runID, Name: "file_write", IdempotencyKey: "tc-key-1"}
	created, err := r.ToolCalls.Create(ctx, tc)
	if err != nil || !created {
		t.Fatalf("tool call create: %v created=%v", err, created)
	}

	seq, eventID, err := r.Runs.CompleteToolCall(ctx, "tc-1", ToolCompletion{
		Result:    &ToolResult{ID: "tr-1", RunID: runID, Output: json.RawMessage(`{"ok":true}`), DurationMS: 12},
		EventType: "tool.completed",
		EventBody: json.RawMessage(`{"tool":"file_write"}`),
	})
	if err != nil {
		t.Fatalf("CompleteToolCall: %v", err)
	}
	if seq != 1 {
		t.Errorf("seq = %d, want 1", seq)
	}
	if eventID == "" {
		t.Error("eventID must be returned for outbox correlation")
	}

	// All four effects landed in one transaction (chapter 9.2).
	res, err := r.ToolResults.GetByToolCall(ctx, "tc-1")
	if err != nil {
		t.Fatalf("tool result: %v", err)
	}
	if string(res.Output) != `{"ok":true}` || res.IsError {
		t.Errorf("result = %+v", res)
	}
	call, err := r.ToolCalls.Get(ctx, "tc-1")
	if err != nil {
		t.Fatalf("tool call: %v", err)
	}
	if call.Status != ToolCallCompleted {
		t.Errorf("tool call status = %q, want completed", call.Status)
	}
	events, err := r.Events.Since(ctx, runID, 0)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if len(events) != 1 || events[0].Type != "tool.completed" {
		t.Fatalf("events = %+v, want one tool.completed", events)
	}
	depth, err := r.Outbox.Depth(ctx)
	if err != nil {
		t.Fatalf("outbox depth: %v", err)
	}
	if depth != 1 {
		t.Errorf("outbox depth = %d, want 1", depth)
	}
	lastSeq, err := r.Events.LastSeq(ctx, runID)
	if err != nil {
		t.Fatalf("last seq: %v", err)
	}
	if lastSeq != 1 {
		t.Errorf("runs.last_seq = %d, want 1", lastSeq)
	}
}

func TestCompleteToolCallRollsBackOnFailure(t *testing.T) {
	r := newTestRepos(t)
	ctx := context.Background()
	_, runID := newSessionAndRun(t, r)
	if _, err := r.ToolCalls.Create(ctx, &ToolCall{ID: "tc-1", RunID: runID, Name: "file_write"}); err != nil {
		t.Fatalf("tool call create: %v", err)
	}

	// RunID does not exist -> the event append fails mid-transaction.
	_, _, err := r.Runs.CompleteToolCall(ctx, "tc-1", ToolCompletion{
		Result:    &ToolResult{ID: "tr-1", RunID: "ghost-run", Output: json.RawMessage(`{}`)},
		EventType: "tool.completed",
		EventBody: json.RawMessage(`{}`),
	})
	if err == nil {
		t.Fatal("expected failure (unknown run)")
	}

	// No half-committed state: the tool result must NOT exist (I6).
	if _, err := r.ToolResults.GetByToolCall(ctx, "tc-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("tool result after failed tx: %v, want ErrNotFound", err)
	}
	call, err := r.ToolCalls.Get(ctx, "tc-1")
	if err != nil {
		t.Fatalf("tool call: %v", err)
	}
	if call.Status != ToolCallPending {
		t.Errorf("tool call status = %q, want pending (tx rolled back)", call.Status)
	}
	depth, _ := r.Outbox.Depth(ctx)
	if depth != 0 {
		t.Errorf("outbox depth = %d, want 0", depth)
	}
}

func TestCompleteToolCallIsIdempotent(t *testing.T) {
	r := newTestRepos(t)
	ctx := context.Background()
	_, runID := newSessionAndRun(t, r)
	if _, err := r.ToolCalls.Create(ctx, &ToolCall{ID: "tc-1", RunID: runID, Name: "file_write"}); err != nil {
		t.Fatalf("tool call create: %v", err)
	}
	in := ToolCompletion{
		Result:    &ToolResult{ID: "tr-1", RunID: runID, Output: json.RawMessage(`{"ok":true}`)},
		EventType: "tool.completed",
		EventBody: json.RawMessage(`{}`),
	}
	if _, _, err := r.Runs.CompleteToolCall(ctx, "tc-1", in); err != nil {
		t.Fatalf("first complete: %v", err)
	}
	// Duplicate completion: no second result, no second event (I5).
	seq2, _, err := r.Runs.CompleteToolCall(ctx, "tc-1", in)
	if err != nil {
		t.Fatalf("second complete: %v", err)
	}
	if seq2 != 0 {
		t.Errorf("duplicate completion produced seq %d, want 0", seq2)
	}
	results, err := r.ToolResults.ListByRun(ctx, runID, 0)
	if err != nil {
		t.Fatalf("list results: %v", err)
	}
	if len(results) != 1 {
		t.Errorf("results = %d, want 1", len(results))
	}
	events, _ := r.Events.Since(ctx, runID, 0)
	if len(events) != 1 {
		t.Errorf("events = %d, want 1", len(events))
	}
}

func TestMessagesAndToolsListing(t *testing.T) {
	r := newTestRepos(t)
	ctx := context.Background()
	_, runID := newSessionAndRun(t, r)
	for i := 0; i < 3; i++ {
		m := &Message{ID: "m" + string(rune('1'+i)), RunID: runID, SessionID: "sess-1", Role: RoleUser, Content: "hi", Seq: int64(i + 1)}
		if err := r.Messages.Create(ctx, m); err != nil {
			t.Fatalf("message create: %v", err)
		}
	}
	msgs, err := r.Messages.ListByRun(ctx, runID, 0)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	if len(msgs) != 3 || msgs[0].Seq != 1 {
		t.Fatalf("messages = %+v", msgs)
	}
	for i := 0; i < 2; i++ {
		if _, err := r.ToolCalls.Create(ctx, &ToolCall{ID: "tc" + string(rune('1'+i)), RunID: runID, Name: "ls"}); err != nil {
			t.Fatalf("tool call: %v", err)
		}
	}
	calls, err := r.ToolCalls.ListByRun(ctx, runID, 0)
	if err != nil {
		t.Fatalf("list tool calls: %v", err)
	}
	if len(calls) != 2 {
		t.Errorf("tool calls = %d, want 2", len(calls))
	}
}

func TestLeaseLifecycle(t *testing.T) {
	r := newTestRepos(t)
	ctx := context.Background()

	l, err := r.Leases.Acquire(ctx, "worker-1", "owner-a", "run-1", 50*time.Millisecond)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if l.FencingToken != 1 {
		t.Errorf("fencing token = %d, want 1", l.FencingToken)
	}

	// Second owner cannot take a live lease.
	if _, err := r.Leases.Acquire(ctx, "worker-1", "owner-b", "run-2", 50*time.Millisecond); !errors.Is(err, ErrLeaseTaken) {
		t.Errorf("second acquire: %v, want ErrLeaseTaken", err)
	}

	// Renew by the owner works.
	if err := r.Leases.Renew(ctx, "worker-1", "owner-a", 100*time.Millisecond); err != nil {
		t.Fatalf("renew: %v", err)
	}
	// Renew by a stranger fails.
	if err := r.Leases.Renew(ctx, "worker-1", "owner-b", 100*time.Millisecond); !errors.Is(err, ErrLeaseTaken) {
		t.Errorf("renew by stranger: %v, want ErrLeaseTaken", err)
	}

	// Release then re-acquire: fencing token increments.
	if err := r.Leases.Release(ctx, "worker-1", "owner-a"); err != nil {
		t.Fatalf("release: %v", err)
	}
	l2, err := r.Leases.Acquire(ctx, "worker-1", "owner-b", "run-2", time.Minute)
	if err != nil {
		t.Fatalf("re-acquire: %v", err)
	}
	if l2.FencingToken <= l.FencingToken {
		t.Errorf("fencing token did not increase: %d -> %d", l.FencingToken, l2.FencingToken)
	}

	// Expired leases are listable for supervisor reaping.
	expired, err := r.Leases.ListExpired(ctx, time.Now().Add(time.Hour), 0)
	if err != nil {
		t.Fatalf("list expired: %v", err)
	}
	if len(expired) != 1 || expired[0].Name != "worker-1" {
		t.Errorf("expired = %+v, want worker-1", expired)
	}
}

func TestKVRepo(t *testing.T) {
	r := newTestRepos(t)
	ctx := context.Background()
	if err := r.KV.Set(ctx, "k1", []byte("v1")); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := r.KV.Set(ctx, "k1", []byte("v2")); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	v, err := r.KV.Get(ctx, "k1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(v) != "v2" {
		t.Errorf("value = %q, want v2", v)
	}
	v, err = r.KV.Get(ctx, "missing")
	if err != nil || v != nil {
		t.Errorf("missing key: v=%q err=%v, want nil/nil", v, err)
	}
	if err := r.KV.Set(ctx, "other", []byte("x")); err != nil {
		t.Fatalf("set other: %v", err)
	}
	all, err := r.KV.List(ctx, "k", 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 1 {
		t.Errorf("prefix list = %d, want 1", len(all))
	}
	if err := r.KV.Delete(ctx, "k1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
}

func TestCatalogRepos(t *testing.T) {
	r := newTestRepos(t)
	ctx := context.Background()

	if err := r.Providers.Upsert(ctx, &Provider{ID: "p1", Name: "deepseek", Type: "openai", APIKeyRef: "secret:p1", Priority: 10}); err != nil {
		t.Fatalf("provider upsert: %v", err)
	}
	providers, err := r.Providers.List(ctx)
	if err != nil {
		t.Fatalf("provider list: %v", err)
	}
	if len(providers) != 1 || providers[0].APIKeyRef != "secret:p1" {
		t.Fatalf("providers = %+v", providers)
	}

	if err := r.McpServers.Upsert(ctx, &McpServer{ID: "mcp1", Name: "fs", Transport: "stdio", Command: "npx", Args: []string{"-y", "srv"}, Env: map[string]string{"K": "V"}}); err != nil {
		t.Fatalf("mcp upsert: %v", err)
	}
	servers, err := r.McpServers.List(ctx)
	if err != nil {
		t.Fatalf("mcp list: %v", err)
	}
	if len(servers) != 1 || servers[0].Command != "npx" || len(servers[0].Args) != 2 || servers[0].Env["K"] != "V" {
		t.Fatalf("servers = %+v", servers)
	}

	if err := r.Skills.Upsert(ctx, &Skill{ID: "sk1", Name: "commit", Steps: json.RawMessage(`[{"tool":"bash"}]`)}); err != nil {
		t.Fatalf("skill upsert: %v", err)
	}
	skills, err := r.Skills.List(ctx)
	if err != nil {
		t.Fatalf("skill list: %v", err)
	}
	if len(skills) != 1 || string(skills[0].Steps) != `[{"tool":"bash"}]` {
		t.Fatalf("skills = %+v", skills)
	}

	if err := r.Experts.Upsert(ctx, &Expert{ID: "e1", Name: "planner", Tools: []string{"plan"}}); err != nil {
		t.Fatalf("expert upsert: %v", err)
	}
	experts, err := r.Experts.List(ctx)
	if err != nil {
		t.Fatalf("expert list: %v", err)
	}
	if len(experts) != 1 || experts[0].Name != "planner" {
		t.Fatalf("experts = %+v", experts)
	}

	if err := r.Knowledge.Upsert(ctx, &KnowledgeEntry{ID: "k1", Mode: "default", Title: "t", Content: "c", Tags: []string{"a"}}); err != nil {
		t.Fatalf("knowledge upsert: %v", err)
	}
	entries, err := r.Knowledge.ListByMode(ctx, "default", 0, 0)
	if err != nil {
		t.Fatalf("knowledge list: %v", err)
	}
	if len(entries) != 1 || entries[0].Title != "t" {
		t.Fatalf("entries = %+v", entries)
	}

	// Permissions: scope precedence and expiry.
	if err := r.Permissions.Grant(ctx, &Permission{ID: "pm1", SubjectType: "tool", SubjectID: "file_write", Action: "exec", Decision: DecisionAllow}); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if err := r.Permissions.Grant(ctx, &Permission{ID: "pm2", SubjectType: "tool", SubjectID: "file_write", Action: "exec", Decision: DecisionDeny, Scope: "/tmp"}); err != nil {
		t.Fatalf("grant scoped: %v", err)
	}
	d, found, err := r.Permissions.Check(ctx, "tool", "file_write", "exec", "/tmp")
	if err != nil || !found || d != DecisionDeny {
		t.Errorf("check scoped: d=%q found=%v err=%v, want deny", d, found, err)
	}
	d, found, _ = r.Permissions.Check(ctx, "tool", "file_write", "exec", "/other")
	if !found || d != DecisionAllow {
		t.Errorf("check global: d=%q found=%v, want allow", d, found)
	}
	d, found, _ = r.Permissions.Check(ctx, "tool", "file_read", "exec", "")
	if found {
		t.Errorf("check unknown subject: found=true, want false")
	}
}

func TestCheckpointRepoRecordAndList(t *testing.T) {
	r := newTestRepos(t)
	ctx := context.Background()
	_, runID := newSessionAndRun(t, r)

	m := &checkpoint.Manifest{
		ID:        "cp-1",
		RunID:     runID,
		SessionID: "sess-1",
		TurnID:    "t1",
		CreatedAt: time.Now().UnixMilli(),
		Files: []checkpoint.ManifestFile{
			{
				Path: filepath.Join(t.TempDir(), "a.txt"),
				Ref:  checkpoint.BlobRef{Size: 100, MediaType: "text/plain"},
			},
		},
	}
	if err := r.Checkpoints.Record(ctx, m, false); err != nil {
		t.Fatalf("record: %v", err)
	}
	list, err := r.Checkpoints.ListByRun(ctx, runID, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].ID != "cp-1" || list[0].TotalSize != 100 {
		t.Fatalf("list = %+v", list)
	}
	if err := r.Checkpoints.SetPinned(ctx, "cp-1", true); err != nil {
		t.Fatalf("pin: %v", err)
	}
	got, err := r.Checkpoints.Get(ctx, "cp-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.Pinned {
		t.Error("pinned flag not persisted")
	}

	// GCStore views.
	active, err := r.Checkpoints.ActiveManifestIDs(ctx)
	if err != nil {
		t.Fatalf("active: %v", err)
	}
	if len(active) != 1 {
		t.Errorf("active = %v, want 1", active)
	}
	pinned, err := r.Checkpoints.PinnedManifestIDs(ctx)
	if err != nil {
		t.Fatalf("pinned: %v", err)
	}
	if len(pinned) != 1 {
		t.Errorf("pinned = %v, want 1", pinned)
	}
	unfinished, err := r.Checkpoints.UnfinishedRunManifestIDs(ctx)
	if err != nil {
		t.Fatalf("unfinished: %v", err)
	}
	if len(unfinished) != 1 {
		t.Errorf("unfinished = %v, want 1 (run is still running)", unfinished)
	}
	recent, err := r.Checkpoints.RecentRunManifestIDs(ctx, 10)
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	if len(recent) != 1 {
		t.Errorf("recent = %v, want 1", recent)
	}
	refs, err := r.Checkpoints.ReferencedBlobHashes(ctx)
	if err != nil {
		t.Fatalf("referenced: %v", err)
	}
	if len(refs) != 1 {
		t.Errorf("referenced = %v, want 1", refs)
	}
}
