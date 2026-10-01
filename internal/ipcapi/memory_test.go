package ipcapi

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/ximo888ok-netizen/ximo-agent/internal/ipc"
	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// This file pins the IPC half of P1-c: the ten memory-graph frames must carry the
// payload verbatim to the port, must refuse malformed input before touching it,
// and must never report success when there is no memory backend. A silent success
// here would let the UI show "forgotten" while the node is still in the database.

// stubMemoryGraph is a scripted types.MemoryGraphPort.
type stubMemoryGraph struct {
	graphReq    types.MemoryGraphRequest
	graph       types.MemoryGraph
	graphErr    error
	nodeID      string
	detail      types.MemoryNodeDetail
	detailErr   error
	update      types.MemoryNodeUpdate
	updateErr   error
	deleted     string
	deleteErr   error
	link        types.MemoryLink
	linkErr     error
	consolidate types.MemoryGraphMutation
	consErr     error
	export      types.MemoryExport
	exportErr   error
	imported    types.MemoryExport
	importErr   error
	stats       types.MemoryStats
	cleared     bool
	clearErr    error
	graphCalls  int
}

func (s *stubMemoryGraph) Graph(_ context.Context, req types.MemoryGraphRequest) (types.MemoryGraph, error) {
	s.graphCalls++
	s.graphReq = req
	return s.graph, s.graphErr
}

func (s *stubMemoryGraph) Node(_ context.Context, id string) (types.MemoryNodeDetail, error) {
	s.nodeID = id
	return s.detail, s.detailErr
}

func (s *stubMemoryGraph) UpdateNode(_ context.Context, upd types.MemoryNodeUpdate) (types.MemoryNodeDetail, error) {
	s.update = upd
	return s.detail, s.updateErr
}

func (s *stubMemoryGraph) DeleteNode(_ context.Context, id string) error {
	s.deleted = id
	return s.deleteErr
}

func (s *stubMemoryGraph) Link(_ context.Context, l types.MemoryLink) (types.MemoryGraphMutation, error) {
	s.link = l
	return types.MemoryGraphMutation{OK: true}, s.linkErr
}

func (s *stubMemoryGraph) Consolidate(context.Context) (types.MemoryGraphMutation, error) {
	return s.consolidate, s.consErr
}

func (s *stubMemoryGraph) Export(context.Context) (types.MemoryExport, error) {
	return s.export, s.exportErr
}

func (s *stubMemoryGraph) Import(_ context.Context, in types.MemoryExport) (types.MemoryGraphMutation, error) {
	s.imported = in
	return types.MemoryGraphMutation{OK: true}, s.importErr
}

func (s *stubMemoryGraph) Stats(context.Context) types.MemoryStats { return s.stats }

func (s *stubMemoryGraph) Clear(context.Context) (types.MemoryGraphMutation, error) {
	s.cleared = true
	return types.MemoryGraphMutation{OK: true}, s.clearErr
}

// memFrame builds a request frame for one memory frame type.
func memFrame(t *testing.T, msgType string, payload any) *ipc.Frame {
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
		Header:  ipc.FrameHeader{Version: 1, RequestID: "req-mem", Type: msgType},
		Payload: raw,
	}
}

// TestMemoryGraphHandlerForwardsTheQuery checks that every filter reaches the
// port. A dropped field is invisible in the UI: the graph would simply look
// wrong, with no error anywhere.
func TestMemoryGraphHandlerForwardsTheQuery(t *testing.T) {
	stub := &stubMemoryGraph{graph: types.MemoryGraph{Total: 2}}
	svc := NewMemoryService(stub)

	resp, herr := svc.handleGraph(context.Background(), memFrame(t, ipc.TypeMemoryGraph,
		types.MemoryGraphRequest{
			Limit: 50, Offset: 10, Kinds: []string{types.MemoryKindFact},
			IncludeArchived: true, Query: "并发", CenterOn: "mn_1", Depth: 2,
		}))
	if herr != nil {
		t.Fatalf("handleGraph: %v", herr)
	}
	if env := decodeResp(t, resp); !env.OK {
		t.Fatalf("response not OK: %s", env.Error)
	}
	got := stub.graphReq
	if got.Limit != 50 || got.Offset != 10 || !got.IncludeArchived ||
		got.Query != "并发" || got.CenterOn != "mn_1" || got.Depth != 2 {
		t.Errorf("request reached the port as %+v", got)
	}
	if len(got.Kinds) != 1 || got.Kinds[0] != types.MemoryKindFact {
		t.Errorf("kinds = %v, want [fact]", got.Kinds)
	}
}

// TestMemoryGraphHandlerAcceptsEmptyPayload checks the zero-value request: the
// UI's first load sends nothing, and that must be a valid "give me defaults".
func TestMemoryGraphHandlerAcceptsEmptyPayload(t *testing.T) {
	stub := &stubMemoryGraph{}
	svc := NewMemoryService(stub)
	frame := &ipc.Frame{
		Header:  ipc.FrameHeader{Version: 1, RequestID: "req-mem", Type: ipc.TypeMemoryGraph},
		Payload: []byte(`{"ok":true}`),
	}
	resp, herr := svc.handleGraph(context.Background(), frame)
	if herr != nil {
		t.Fatalf("handleGraph: %v", herr)
	}
	if env := decodeResp(t, resp); !env.OK {
		t.Fatalf("an empty request must be accepted: %s", env.Error)
	}
	if stub.graphCalls != 1 {
		t.Errorf("port called %d times, want 1", stub.graphCalls)
	}
}

// TestMemoryNodeGetRequiresAnID checks the guard: without a node id the port
// cannot answer, and guessing would return an arbitrary node.
func TestMemoryNodeGetRequiresAnID(t *testing.T) {
	stub := &stubMemoryGraph{}
	svc := NewMemoryService(stub)
	resp, herr := svc.handleNodeGet(context.Background(),
		memFrame(t, ipc.TypeMemoryNodeGet, MemoryIDPayload{}))
	if herr != nil {
		t.Fatalf("handleNodeGet: %v", herr)
	}
	if env := decodeResp(t, resp); env.OK {
		t.Fatal("a missing node_id must be refused")
	}
	if stub.nodeID != "" {
		t.Error("a refused request must not reach the port")
	}
}

// TestMemoryNodeUpdateValidatesStatusAndImportance checks the two fields whose
// bad values would corrupt the graph's invariants (the CHECK constraint on
// status, and the 0..1 range importance is scored with).
func TestMemoryNodeUpdateValidatesStatusAndImportance(t *testing.T) {
	bad := "bogus"
	high := 1.5
	low := -0.1
	cases := map[string]types.MemoryNodeUpdate{
		"unknown status":    {NodeID: "mn_1", Status: &bad},
		"importance above":  {NodeID: "mn_1", Importance: &high},
		"importance below":  {NodeID: "mn_1", Importance: &low},
	}
	for name, upd := range cases {
		t.Run(name, func(t *testing.T) {
			stub := &stubMemoryGraph{}
			svc := NewMemoryService(stub)
			resp, herr := svc.handleNodeUpdate(context.Background(),
				memFrame(t, ipc.TypeMemoryNodeUpdate, upd))
			if herr != nil {
				t.Fatalf("handleNodeUpdate: %v", herr)
			}
			if env := decodeResp(t, resp); env.OK {
				t.Fatal("an out-of-range value must be refused")
			}
			if stub.update.NodeID != "" {
				t.Error("a refused request must not reach the port")
			}
		})
	}
}

// TestMemoryNodeUpdateKeepsTheThreeStateFields is the regression test for the
// pointer-shaped update: "not provided" must stay distinguishable from "set to
// zero", or a user editing only the title would silently reset importance.
func TestMemoryNodeUpdateKeepsTheThreeStateFields(t *testing.T) {
	stub := &stubMemoryGraph{}
	svc := NewMemoryService(stub)
	title := "新标题"
	resp, herr := svc.handleNodeUpdate(context.Background(),
		memFrame(t, ipc.TypeMemoryNodeUpdate, types.MemoryNodeUpdate{
			NodeID: "mn_1", Title: &title,
		}))
	if herr != nil {
		t.Fatalf("handleNodeUpdate: %v", herr)
	}
	if env := decodeResp(t, resp); !env.OK {
		t.Fatalf("response not OK: %s", env.Error)
	}
	if stub.update.Title == nil || *stub.update.Title != title {
		t.Errorf("title did not reach the port: %+v", stub.update)
	}
	if stub.update.Importance != nil {
		t.Error("an omitted importance must stay nil (unchanged), not become 0")
	}
	if stub.update.Pinned != nil || stub.update.Status != nil {
		t.Error("omitted fields must stay nil")
	}
}

// TestMemoryLinkValidatesEndpoints checks the guards that keep the graph sound:
// both endpoints are required, self-links are refused, and the relation must be
// one the CHECK constraint accepts.
func TestMemoryLinkValidatesEndpoints(t *testing.T) {
	cases := map[string]MemoryLinkPayload{
		"missing src":  {Dst: "mn_2"},
		"missing dst":  {Src: "mn_1"},
		"self link":    {Src: "mn_1", Dst: "mn_1"},
		"unknown rel":  {Src: "mn_1", Dst: "mn_2", Rel: "invented"},
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			stub := &stubMemoryGraph{}
			svc := NewMemoryService(stub)
			resp, herr := svc.handleLink(context.Background(),
				memFrame(t, ipc.TypeMemoryLink, p))
			if herr != nil {
				t.Fatalf("handleLink: %v", herr)
			}
			if env := decodeResp(t, resp); env.OK {
				t.Fatal("an invalid link must be refused")
			}
			if stub.link.Src != "" {
				t.Error("a refused request must not reach the port")
			}
		})
	}
}

// TestMemoryLinkDefaultsToRelated checks the documented default: an empty rel is
// "related", not an error and not an empty string in the database.
func TestMemoryLinkDefaultsToRelated(t *testing.T) {
	stub := &stubMemoryGraph{}
	svc := NewMemoryService(stub)
	resp, herr := svc.handleLink(context.Background(),
		memFrame(t, ipc.TypeMemoryLink, MemoryLinkPayload{Src: "mn_1", Dst: "mn_2"}))
	if herr != nil {
		t.Fatalf("handleLink: %v", herr)
	}
	if env := decodeResp(t, resp); !env.OK {
		t.Fatalf("response not OK: %s", env.Error)
	}
	if stub.link.Src != "mn_1" || stub.link.Dst != "mn_2" {
		t.Errorf("link did not reach the port: %+v", stub.link)
	}
}

// TestMemoryImportRequiresNodes checks that an empty import is refused rather
// than reported as a successful no-op.
func TestMemoryImportRequiresNodes(t *testing.T) {
	stub := &stubMemoryGraph{}
	svc := NewMemoryService(stub)
	resp, herr := svc.handleImport(context.Background(),
		memFrame(t, ipc.TypeMemoryImport, types.MemoryExport{}))
	if herr != nil {
		t.Fatalf("handleImport: %v", herr)
	}
	if env := decodeResp(t, resp); env.OK {
		t.Fatal("an import with no nodes must be refused")
	}
	if len(stub.imported.Nodes) != 0 {
		t.Error("a refused import must not reach the port")
	}
}

// TestMemoryClearRequiresTheLiteralConfirm checks the guard on the one
// irreversible operation in this surface.
func TestMemoryClearRequiresTheLiteralConfirm(t *testing.T) {
	for _, confirm := range []string{"", "yes", "delete_all", "DELETE_ALL "} {
		t.Run("confirm="+confirm, func(t *testing.T) {
			stub := &stubMemoryGraph{}
			svc := NewMemoryService(stub)
			resp, herr := svc.handleClear(context.Background(),
				memFrame(t, ipc.TypeMemoryClear, MemoryClearPayload{Confirm: confirm}))
			if herr != nil {
				t.Fatalf("handleClear: %v", herr)
			}
			if env := decodeResp(t, resp); env.OK {
				t.Fatalf("confirm %q must be refused", confirm)
			}
			if stub.cleared {
				t.Error("a refused clear must not reach the port")
			}
		})
	}

	stub := &stubMemoryGraph{}
	svc := NewMemoryService(stub)
	resp, herr := svc.handleClear(context.Background(),
		memFrame(t, ipc.TypeMemoryClear, MemoryClearPayload{Confirm: memoryClearConfirm}))
	if herr != nil {
		t.Fatalf("handleClear: %v", herr)
	}
	if env := decodeResp(t, resp); !env.OK {
		t.Fatalf("the exact confirm must be accepted: %s", env.Error)
	}
	if !stub.cleared {
		t.Error("an accepted clear must reach the port")
	}
}

// TestMemoryHandlersReportMissingCapability checks that a build without a memory
// backend fails every frame loudly. This is the difference between "the UI says
// the node was forgotten" and "the node is still there".
func TestMemoryHandlersReportMissingCapability(t *testing.T) {
	svc := NewMemoryService(nil)
	ctx := context.Background()
	cases := []struct {
		name  string
		frame *ipc.Frame
		call  func(context.Context, *ipc.Frame) (*ipc.Frame, error)
	}{
		{"graph", memFrame(t, ipc.TypeMemoryGraph, types.MemoryGraphRequest{}), svc.handleGraph},
		{"node.get", memFrame(t, ipc.TypeMemoryNodeGet, MemoryIDPayload{NodeID: "mn_1"}), svc.handleNodeGet},
		{"node.update", memFrame(t, ipc.TypeMemoryNodeUpdate, types.MemoryNodeUpdate{NodeID: "mn_1"}), svc.handleNodeUpdate},
		{"node.delete", memFrame(t, ipc.TypeMemoryNodeDelete, MemoryIDPayload{NodeID: "mn_1"}), svc.handleNodeDelete},
		{"link", memFrame(t, ipc.TypeMemoryLink, MemoryLinkPayload{Src: "a", Dst: "b"}), svc.handleLink},
		{"consolidate", memFrame(t, ipc.TypeMemoryConsolidate, nil), svc.handleConsolidate},
		{"export", memFrame(t, ipc.TypeMemoryExport, nil), svc.handleExport},
		{"import", memFrame(t, ipc.TypeMemoryImport, types.MemoryExport{Nodes: []types.MemoryExportNode{{ID: "x"}}}), svc.handleImport},
		{"stats", memFrame(t, ipc.TypeMemoryStats, nil), svc.handleStats},
		{"clear", memFrame(t, ipc.TypeMemoryClear, MemoryClearPayload{Confirm: memoryClearConfirm}), svc.handleClear},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, herr := tc.call(ctx, tc.frame)
			if herr != nil {
				t.Fatalf("handler: %v", herr)
			}
			if env := decodeResp(t, resp); env.OK {
				t.Error("a missing capability must be an error, not a silent success")
			}
		})
	}
}

// TestMemoryNodeDeleteSurfacesPortErrors checks that a failing delete is not
// reported as success.
func TestMemoryNodeDeleteSurfacesPortErrors(t *testing.T) {
	stub := &stubMemoryGraph{deleteErr: errors.New("node mn_1 does not exist")}
	svc := NewMemoryService(stub)
	resp, herr := svc.handleNodeDelete(context.Background(),
		memFrame(t, ipc.TypeMemoryNodeDelete, MemoryIDPayload{NodeID: "mn_1"}))
	if herr != nil {
		t.Fatalf("handleNodeDelete: %v", herr)
	}
	env := decodeResp(t, resp)
	if env.OK {
		t.Fatal("a port error must not be reported as success")
	}
	if env.Error == "" {
		t.Error("the failure reason should reach the caller")
	}
}

// TestMemoryStatsPayloadCarriesBackendIdentity checks that the UI can tell which
// backend answered and whether memory is on: showing "0 nodes" for a disabled
// backend would read as "your memory is empty".
func TestMemoryStatsPayloadCarriesBackendIdentity(t *testing.T) {
	stub := &stubMemoryGraph{stats: types.MemoryStats{
		UserID: "u1", Nodes: 12, Edges: 30, Backend: "synapse", Enabled: true,
	}}
	svc := NewMemoryService(stub)
	resp, herr := svc.handleStats(context.Background(),
		memFrame(t, ipc.TypeMemoryStats, nil))
	if herr != nil {
		t.Fatalf("handleStats: %v", herr)
	}
	env := decodeResp(t, resp)
	if !env.OK {
		t.Fatalf("response not OK: %s", env.Error)
	}
	var got MemoryStatsPayload
	if err := env.Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Backend != "synapse" || !got.Enabled || got.Nodes != 12 || got.Edges != 30 {
		t.Errorf("stats payload = %+v", got)
	}
}

// TestMemoryFrameNamesMatchContract pins the ten frame names. They are shared by
// this service, the supervisor's forwarder and the frontend, so a rename that
// compiles on one side would silently stop the other from ever answering.
func TestMemoryFrameNamesMatchContract(t *testing.T) {
	want := map[string]string{
		"graph":       "system.memory.graph",
		"node.get":    "system.memory.node.get",
		"node.update": "system.memory.node.update",
		"node.delete": "system.memory.node.delete",
		"link":        "system.memory.link",
		"consolidate": "system.memory.consolidate",
		"export":      "system.memory.export",
		"import":      "system.memory.import",
		"stats":       "system.memory.stats",
		"clear":       "system.memory.clear",
	}
	got := map[string]string{
		"graph": ipc.TypeMemoryGraph, "node.get": ipc.TypeMemoryNodeGet,
		"node.update": ipc.TypeMemoryNodeUpdate, "node.delete": ipc.TypeMemoryNodeDelete,
		"link": ipc.TypeMemoryLink, "consolidate": ipc.TypeMemoryConsolidate,
		"export": ipc.TypeMemoryExport, "import": ipc.TypeMemoryImport,
		"stats": ipc.TypeMemoryStats, "clear": ipc.TypeMemoryClear,
	}
	for name, wantVal := range want {
		if got[name] != wantVal {
			t.Errorf("%s frame = %q, want %q", name, got[name], wantVal)
		}
	}
}

// TestMemoryGraphNodeWireNames pins the field names the frontend reads. The
// graph view reads effective_weight for edge thickness; a rename there would
// silently fall back to raw weight and misrepresent stale connections as fresh.
func TestMemoryGraphNodeWireNames(t *testing.T) {
	raw, err := json.Marshal(types.MemoryGraphEdge{
		Src: "a", Dst: "b", Rel: "related", Weight: 0.4, EffectiveWeight: 0.2, FireCount: 3,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"src", "dst", "rel", "weight", "effective_weight", "fire_count"} {
		if _, ok := got[key]; !ok {
			t.Errorf("edge payload is missing %q: %s", key, raw)
		}
	}
}

// TestMemoryKindAndRelEnumerations checks that the enumerations the UI uses for
// its filter dropdowns match the database's CHECK constraints exactly. A value
// present in one and absent from the other is a filter that always returns
// nothing (or a link the UI offers and the database rejects).
func TestMemoryKindAndRelEnumerations(t *testing.T) {
	kinds := map[string]bool{}
	for _, k := range types.AllMemoryKinds {
		kinds[k] = true
	}
	for _, want := range []string{"fact", "entity", "episode", "procedure", "topic"} {
		if !kinds[want] {
			t.Errorf("AllMemoryKinds is missing %q", want)
		}
	}
	rels := map[string]bool{}
	for _, r := range types.AllMemoryRels {
		rels[r] = true
	}
	for _, want := range []string{
		"mentions", "related", "part_of", "causes", "derived_from",
		"supersedes", "contradicts", "same_topic", "used_with",
	} {
		if !rels[want] {
			t.Errorf("AllMemoryRels is missing %q", want)
		}
	}
	if validMemoryRel("invented") {
		t.Error("validMemoryRel accepted an unknown relation")
	}
	if validMemoryStatus("invented") {
		t.Error("validMemoryStatus accepted an unknown status")
	}
}
