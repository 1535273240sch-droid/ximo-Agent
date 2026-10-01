package e2e

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/bootstrap"
	"github.com/ximo888ok-netizen/ximo-agent/internal/config"
	"github.com/ximo888ok-netizen/ximo-agent/internal/ipc"
	"github.com/ximo888ok-netizen/ximo-agent/internal/ipcapi"
	"github.com/ximo888ok-netizen/ximo-agent/internal/ports/mem"
	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// 记忆页的端到端测试（P1-c）。
//
// 为什么必须在 e2e 层再测一遍：ipcapi 的单测用的是脚本化的 stub 端口，它证明不了
// 「真实的 Synapse 后端 + 真实装配 + 真实帧编解码」这条链路上字段名是对得上的。
// 而这条链路的失败模式恰恰是**静默**的——Go 结构体 tag 与前端 TypeScript 接口只要
// 有一个词不一致，界面就永远是空的，编译期与单测都不会报错。
//
// 因此这个测试做两件事：
//  1. 走真实 IPC 往返（独立客户端 → 监听中的服务端），断言响应信封里的 **JSON 键名**
//     与前端 frontend/src/shared/types.ts 逐字一致；
//  2. 断言数据本身经过真实 SQLite 落盘后能读回来（不是内存里的假数据）。

// memoryEndpoint 起一个只注册 MemoryService 的 IPC 服务端。
//
// 它与 startEngineIPC 分开：记忆页与 run 生命周期无关，这里刻意不注册 EngineService，
// 这样任何对 run 的依赖都会立刻暴露成「帧无人处理」，而不是被引擎顺手兜住。
func memoryEndpoint(t *testing.T, app *bootstrap.App, cfg *config.Config) string {
	t.Helper()
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		endpoint := freeTCPEndpoint(t)
		srv := ipc.NewServer(endpoint, cfg.IPC.MaxPayloadLength)
		if err := srv.Start(); err != nil {
			lastErr = err
			continue
		}
		ipcapi.NewMemoryService(app.MemoryGraph()).Register(srv)
		t.Cleanup(func() { _ = srv.Stop() })
		return endpoint
	}
	t.Fatalf("could not start memory ipc server after retries: %v", lastErr)
	return ""
}

// synapseConfig 返回一份启用了 synapse 记忆的配置。
//
// 必须同时打开特性开关：`memory.enabled` 是用户级开关，`feature_flags.memory.mem0`
// 是整机/整环境熔断，两者都开才生效（这是既有设计，不是本次引入的）。漏掉前者会
// 表现为「配置看起来是对的，但 MemoryGraph() 返回 nil」——正是这个 e2e 测试第一次
// 跑出来的现象。
func synapseConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := newTestConfig(t)
	cfg.Memory.Enabled = true
	cfg.Memory.Backend = "synapse"
	cfg.Memory.UserID = "e2e-user"
	if cfg.FeatureFlags == nil {
		cfg.FeatureFlags = map[string]bool{}
	}
	cfg.FeatureFlags[config.FlagMemory] = true
	return cfg
}

// dialMemory 连上一个独立的客户端，返回一个泛化的帧调用器。
//
// 它刻意走底层的 ipc.Client.SendRequest，而不是 ipcapi.Client 上的具名方法：
// 记忆帧目前只有 ipcapi.MemoryService 的服务端一半，客户端一半由前端的
// backend-client 直接发帧名，所以这里也用帧名调用——这样测到的就是前端走的同一条路。
func dialMemory(t *testing.T, endpoint string) func(msgType string, payload any, dst any) error {
	t.Helper()
	c := ipc.NewClient(endpoint, ipcMaxPayload, true)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if err := c.Ping(5 * time.Second); err != nil {
		t.Fatalf("ping: %v", err)
	}
	return func(msgType string, payload any, dst any) error {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		frame, err := ipcapi.EnvelopeFrom(msgType, payload, ipcapi.FrameOpts{
			RequestID: "e2e-" + msgType,
			Timeout:   20 * time.Second,
		})
		if err != nil {
			return err
		}
		resp, err := c.SendRequest(ctx, frame)
		if err != nil {
			return err
		}
		var env ipcapi.Envelope
		if err := json.Unmarshal(resp.Payload, &env); err != nil {
			return err
		}
		return env.Decode(dst)
	}
}

// TestMemoryGraphEndToEndOverIPC 是记忆页的真实链路测试。
func TestMemoryGraphEndToEndOverIPC(t *testing.T) {
	cfg := synapseConfig(t)
	app, err := bootstrap.New(cfg, bootstrap.Options{
		Provider: mem.NewProvider(),
		MigrationsDir: migrationsDir(t),
	})
	if err != nil {
		t.Fatalf("bootstrap.New: %v", err)
	}
	t.Cleanup(func() { app.Close() })

	graph := app.MemoryGraph()
	if graph == nil {
		t.Fatal("enabled synapse memory must expose a MemoryGraphPort")
	}

	// 直接往真实后端写两个节点与一条边（模拟「run 结束后抽取入库」的结果）。
	// 这里用 Export/Import 而不是 Add：Import 是幂等的写入口，不需要 provider 参与，
	// 因此这条测试不依赖任何模型调用。
	ctx := context.Background()
	now := time.Now().Unix()
	doc := types.MemoryExport{
		Version:    1,
		ExportedAt: now,
		UserID:     "e2e-user",
		Nodes: []types.MemoryExportNode{
			{ID: "n-go", Kind: types.MemoryKindEntity, Title: "Go", Content: "Go", Importance: 0.7, Status: types.MemoryStatusActive, CreatedAt: now},
			{ID: "n-pref", Kind: types.MemoryKindFact, Title: "偏好深色主题", Content: "用户偏好深色主题", Importance: 0.9, Status: types.MemoryStatusActive, CreatedAt: now},
		},
		Edges: []types.MemoryGraphEdge{
			{Src: "n-pref", Dst: "n-go", Rel: types.MemoryRelMentions, Weight: 0.5},
		},
	}
	if _, err := graph.Import(ctx, doc); err != nil {
		t.Fatalf("seed import: %v", err)
	}

	endpoint := memoryEndpoint(t, app, cfg)
	call := dialMemory(t, endpoint)

	// ---- stats：界面用它在页头显示「N 个节点 / M 条边 / 用的哪个后端」 ----
	var stats ipcapi.MemoryStatsPayload
	if err := call(ipc.TypeMemoryStats, map[string]any{}, &stats); err != nil {
		t.Fatalf("memory.stats: %v", err)
	}
	if stats.Nodes < 2 || stats.Edges < 1 {
		t.Fatalf("stats after seeding = %+v, want at least 2 nodes and 1 edge", stats)
	}
	if stats.Backend != "synapse" || !stats.Enabled {
		t.Errorf("stats must identify the live backend: %+v", stats)
	}

	// ---- graph：界面用它在画布上画点与线 ----
	var graphResp ipcapi.MemoryGraphResult
	if err := call(ipc.TypeMemoryGraph, types.MemoryGraphRequest{Limit: 50}, &graphResp); err != nil {
		t.Fatalf("memory.graph: %v", err)
	}
	if len(graphResp.Nodes) < 2 {
		t.Fatalf("graph returned %d nodes, want at least 2", len(graphResp.Nodes))
	}
	if graphResp.Total < 2 {
		t.Errorf("graph total = %d, want at least 2", graphResp.Total)
	}
	var factID, entityID string
	for _, n := range graphResp.Nodes {
		switch n.Kind {
		case types.MemoryKindFact:
			factID = n.ID
			if n.ContentPreview == "" {
				t.Error("a node in the graph list must carry a content preview, or the list view shows blanks")
			}
		case types.MemoryKindEntity:
			entityID = n.ID
		}
	}
	if factID == "" {
		t.Fatal("the seeded fact node is missing from the graph result")
	}
	// 导入会按内容哈希重新分配节点 id（导出文件里的 id 只是来源标记，不是主键），
	// 因此后续操作用**读回来的** id，而不是构造时写下的那个。这正是界面必须
	// 「先查再点」的原因：任何硬编码 id 的调用都会在真实后端上失败。
	if entityID == "" {
		t.Fatal("the seeded entity node is missing from the graph result")
	}
	if len(graphResp.Edges) == 0 {
		t.Fatal("the seeded edge is missing from the graph result")
	}
	for _, e := range graphResp.Edges {
		if e.EffectiveWeight <= 0 {
			t.Errorf("edge %s→%s has effective_weight %v; the canvas sizes edges by it, so 0 means "+
				"every edge would be drawn invisible", e.Src, e.Dst, e.EffectiveWeight)
		}
	}

	// ---- 线协议键名：与前端 shared/types.ts 逐字一致 ----
	//
	// 这是这个测试存在的主要理由。上面的结构体解码只能证明「Go 自己认得自己」，
	// 证明不了前端读得到：前端是按 snake_case 键名解析 JSON 的。
	raw, err := json.Marshal(graphResp)
	if err != nil {
		t.Fatalf("marshal graph: %v", err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatalf("unmarshal graph: %v", err)
	}
	for _, key := range []string{"nodes", "edges", "total"} {
		if _, ok := keys[key]; !ok {
			t.Errorf("graph payload is missing %q; the frontend reads that exact name", key)
		}
	}
	var nodeList []map[string]json.RawMessage
	if err := json.Unmarshal(keys["nodes"], &nodeList); err != nil {
		t.Fatalf("unmarshal node list: %v", err)
	}
	if len(nodeList) > 0 {
		for _, key := range []string{"id", "kind", "importance", "pinned", "status", "degree", "created_at"} {
			if _, ok := nodeList[0][key]; !ok {
				t.Errorf("graph node is missing %q", key)
			}
		}
	}
	var edgeList []map[string]json.RawMessage
	if err := json.Unmarshal(keys["edges"], &edgeList); err != nil {
		t.Fatalf("unmarshal edge list: %v", err)
	}
	if len(edgeList) > 0 {
		for _, key := range []string{"src", "dst", "rel", "weight", "effective_weight"} {
			if _, ok := edgeList[0][key]; !ok {
				t.Errorf("graph edge is missing %q", key)
			}
		}
	}

	// ---- node.get：详情面板 ----
	var detail ipcapi.MemoryNodePayload
	if err := call(ipc.TypeMemoryNodeGet, ipcapi.MemoryIDPayload{NodeID: factID}, &detail); err != nil {
		t.Fatalf("memory.node.get: %v", err)
	}
	if detail.Node.ID != factID {
		t.Errorf("detail node id = %q, want %q", detail.Node.ID, factID)
	}
	if detail.Content == "" {
		t.Error("detail must carry the full content, not just the preview")
	}
	if len(detail.Neighbors) == 0 {
		t.Error("the seeded mentions edge should give this node a neighbour")
	}

	// ---- node.update：编辑与置顶（指针三态必须能穿过 JSON） ----
	title := "偏好：深色主题"
	pinned := true
	var updated ipcapi.MemoryNodePayload
	if err := call(ipc.TypeMemoryNodeUpdate, types.MemoryNodeUpdate{
		NodeID: factID, Title: &title, Pinned: &pinned,
	}, &updated); err != nil {
		t.Fatalf("memory.node.update: %v", err)
	}
	if updated.Node.Title != title {
		t.Errorf("title after update = %q, want %q", updated.Node.Title, title)
	}
	if !updated.Node.Pinned {
		t.Error("pinned after update = false, want true")
	}
	// importance 没传就必须保持原值——这正是指针三态要保证的事。
	if updated.Node.Importance != 0.9 {
		t.Errorf("importance = %v, want it unchanged at 0.9 (an omitted field must not be zeroed)",
			updated.Node.Importance)
	}

	// ---- link：手动连线（用读回来的 id，见上） ----
	var linked ipcapi.MemoryGraphMutationResult
	if err := call(ipc.TypeMemoryLink, ipcapi.MemoryLinkPayload{
		Src: entityID, Dst: factID, Rel: types.MemoryRelRelated, Weight: 0.4,
	}, &linked); err != nil {
		t.Fatalf("memory.link: %v", err)
	}
	if !linked.OK {
		t.Errorf("link response = %+v, want ok", linked)
	}

	// ---- export / import：备份与恢复 ----
	var exported ipcapi.MemoryExportPayload
	if err := call(ipc.TypeMemoryExport, map[string]any{}, &exported); err != nil {
		t.Fatalf("memory.export: %v", err)
	}
	if len(exported.Nodes) < 2 {
		t.Fatalf("export returned %d nodes, want at least 2", len(exported.Nodes))
	}
	exportRaw, err := json.Marshal(exported)
	if err != nil {
		t.Fatalf("marshal export: %v", err)
	}
	var exportKeys map[string]json.RawMessage
	if err := json.Unmarshal(exportRaw, &exportKeys); err != nil {
		t.Fatalf("unmarshal export: %v", err)
	}
	for _, key := range []string{"version", "exported_at", "nodes", "edges"} {
		if _, ok := exportKeys[key]; !ok {
			t.Errorf("export payload is missing %q", key)
		}
	}
	var reimported ipcapi.MemoryGraphMutationResult
	if err := call(ipc.TypeMemoryImport, exported, &reimported); err != nil {
		t.Fatalf("memory.import: %v", err)
	}
	// 幂等：导入一份与库里完全相同的数据不应新增节点。
	if reimported.Affected != 0 {
		t.Errorf("re-importing the same document created %d entries, want 0 (import must be idempotent)",
			reimported.Affected)
	}

	// ---- node.delete：遗忘 ----
	var deleted map[string]any
	if err := call(ipc.TypeMemoryNodeDelete, ipcapi.MemoryIDPayload{NodeID: factID}, &deleted); err != nil {
		t.Fatalf("memory.node.delete: %v", err)
	}
	var afterDelete ipcapi.MemoryGraphResult
	if err := call(ipc.TypeMemoryGraph, types.MemoryGraphRequest{Limit: 50}, &afterDelete); err != nil {
		t.Fatalf("memory.graph after delete: %v", err)
	}
	for _, n := range afterDelete.Nodes {
		if n.ID == factID {
			t.Fatalf("node %s is still in the graph after being forgotten", factID)
		}
	}

	// ---- consolidate：手动整理 ----
	var consolidated ipcapi.MemoryGraphMutationResult
	if err := call(ipc.TypeMemoryConsolidate, map[string]any{}, &consolidated); err != nil {
		t.Fatalf("memory.consolidate: %v", err)
	}
	if !consolidated.OK {
		t.Errorf("consolidate response = %+v, want ok", consolidated)
	}

	// ---- clear：需要确认字面量 ----
	var refused map[string]any
	if err := call(ipc.TypeMemoryClear, ipcapi.MemoryClearPayload{Confirm: "yes"}, &refused); err == nil {
		t.Error("clearing memory with a wrong confirm value must fail")
	}
	var cleared ipcapi.MemoryGraphMutationResult
	if err := call(ipc.TypeMemoryClear, ipcapi.MemoryClearPayload{Confirm: "DELETE_ALL"}, &cleared); err != nil {
		t.Fatalf("memory.clear with the literal confirm: %v", err)
	}
	if got := graph.Stats(context.Background()).Nodes; got != 0 {
		t.Errorf("nodes after clear = %d, want 0", got)
	}
}

// TestMemoryGraphDisabledFailsLoudly 检查未启用记忆时每个帧都明确报错。
//
// 这一条比它看起来重要：如果未启用时返回「成功但空图」，用户会以为自己的记忆
// 被清空了；返回明确错误才能让人去设置里打开开关。
func TestMemoryGraphDisabledFailsLoudly(t *testing.T) {
	cfg := newTestConfig(t)
	cfg.Memory.Enabled = false

	app, err := bootstrap.New(cfg, bootstrap.Options{
		Provider:      mem.NewProvider(),
		MigrationsDir: migrationsDir(t),
	})
	if err != nil {
		t.Fatalf("bootstrap.New: %v", err)
	}
	t.Cleanup(func() { app.Close() })

	if g := app.MemoryGraph(); g != nil {
		t.Fatalf("memory disabled must not expose a graph, got %#v", g)
	}

	endpoint := memoryEndpoint(t, app, cfg)
	call := dialMemory(t, endpoint)

	var out map[string]any
	if err := call(ipc.TypeMemoryGraph, types.MemoryGraphRequest{}, &out); err == nil {
		t.Error("memory.graph without a backend must fail, not return an empty graph")
	}
	if err := call(ipc.TypeMemoryStats, map[string]any{}, &out); err == nil {
		t.Error("memory.stats without a backend must fail")
	}
}

// TestSynapseMemoryDatabaseLivesInDataDir 钉住库文件位置：它必须落在配置的 DataDir
// 下，而不是进程当前目录——后者会把用户数据写在安装目录里，升级时丢失。
func TestSynapseMemoryDatabaseLivesInDataDir(t *testing.T) {
	cfg := synapseConfig(t)
	app, err := bootstrap.New(cfg, bootstrap.Options{
		Provider:      mem.NewProvider(),
		MigrationsDir: migrationsDir(t),
	})
	if err != nil {
		t.Fatalf("bootstrap.New: %v", err)
	}
	t.Cleanup(func() { app.Close() })

	// 写一条真实数据，确保库文件确实被创建（只是构造可能不落地）。
	now := time.Now().Unix()
	if _, err := app.MemoryGraph().Import(context.Background(), types.MemoryExport{
		Version: 1, ExportedAt: now, UserID: "e2e-user",
		Nodes: []types.MemoryExportNode{{
			ID: "n1", Kind: types.MemoryKindFact, Content: "memory lives in DataDir",
			Importance: 0.5, Status: types.MemoryStatusActive, CreatedAt: now,
		}},
	}); err != nil {
		t.Fatalf("import: %v", err)
	}

	want := filepath.Join(cfg.Paths.DataDir, "memory.db")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("expected the synapse database at %s: %v", want, err)
	}
}
