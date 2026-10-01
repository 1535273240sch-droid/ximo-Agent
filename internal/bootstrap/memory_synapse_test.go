package bootstrap_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/bootstrap"
	"github.com/ximo888ok-netizen/ximo-agent/internal/config"
	"github.com/ximo888ok-netizen/ximo-agent/internal/memory"
	"github.com/ximo888ok-netizen/ximo-agent/internal/ports"
	"github.com/ximo888ok-netizen/ximo-agent/internal/ports/mem"
	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// 本文件是「图记忆后端在真实装配里接通了」的可执行证据：配置 → bootstrap →
// MemoryGraphPort → IPC 层要用的那 10 个方法，以及旧数据迁移只做一次。

// synapseConfig 是「用户把后端切到图记忆」的那份配置。
func synapseConfig(t *testing.T, userID string) *config.Config {
	t.Helper()
	cfg := newTestConfig(t)
	cfg.Memory = config.MemoryConfig{
		Enabled:        true,
		Backend:        "synapse",
		UserID:         userID,
		Timeout:        time.Second,
		ExtractTimeout: 2 * time.Second,
	}
	cfg.FeatureFlags[config.FlagMemory] = true
	return cfg
}

func scriptedProvider() *mem.Provider {
	return mem.NewProvider(ports.ProviderResponse{
		FinishReason: ports.FinishStop, Content: "完成", Emitted: true,
	})
}

func TestAssemble_SynapseBackendExposesMemoryGraph(t *testing.T) {
	cfg := synapseConfig(t, "graph-user")
	app, err := bootstrap.New(cfg, bootstrap.Options{
		MigrationsDir:  migrationsDir(t),
		Provider:       scriptedProvider(),
		WorkspaceRoot:  t.TempDir(),
		DisableWorkers: true,
	})
	if err != nil {
		t.Fatalf("bootstrap.New: %v", err)
	}
	defer app.Close()

	g := app.MemoryGraph()
	if g == nil {
		t.Fatal("synapse 后端应通过 App.MemoryGraph() 暴露 MemoryGraphPort")
	}
	ctx := context.Background()

	// 用 Import 灌一份最小数据（不依赖模型），再验证图读写面真的落在装配好的库上。
	mut, err := g.Import(ctx, types.MemoryExport{
		Version: 1,
		Nodes: []types.MemoryExportNode{
			{ID: "n1", Kind: types.MemoryKindFact, Content: "项目偏好深色主题", Importance: 0.7, Status: types.MemoryStatusActive},
			{ID: "n2", Kind: types.MemoryKindEntity, Title: "XimoAgent", Content: "XimoAgent", Status: types.MemoryStatusActive},
		},
		Edges: []types.MemoryGraphEdge{{Src: "n1", Dst: "n2", Rel: types.MemoryRelMentions, Weight: 0.5}},
	})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if !mut.OK || mut.Affected != 2 {
		t.Fatalf("Import 结果 = %+v", mut)
	}

	st := g.Stats(ctx)
	if st.Backend != memory.BackendSynapse || !st.Enabled {
		t.Fatalf("Stats 应标明后端为 synapse 且已启用: %+v", st)
	}
	if st.UserID != "graph-user" || st.Nodes != 2 || st.Edges != 1 {
		t.Fatalf("Stats 计数不符: %+v", st)
	}

	graph, err := g.Graph(ctx, types.MemoryGraphRequest{})
	if err != nil {
		t.Fatalf("Graph: %v", err)
	}
	if graph.Total != 2 || len(graph.Nodes) != 2 {
		t.Fatalf("图应有 2 个节点，得到 total=%d nodes=%d", graph.Total, len(graph.Nodes))
	}
	if len(graph.Edges) != 1 || graph.Edges[0].EffectiveWeight != 0.5 {
		t.Fatalf("图应有 1 条有效权重 0.5 的边，得到 %+v", graph.Edges)
	}
	for _, n := range graph.Nodes {
		if n.ID == "n2" && n.Degree != 1 {
			t.Fatalf("entity 节点度数应为 1，实际 %d", n.Degree)
		}
	}

	// 不存在的节点：明确报错（IPC 层据此回 ok=false）。
	if _, err := g.Node(ctx, "mn_missing"); err == nil {
		t.Fatal("不存在的节点应当报错")
	}
}

// TestAssemble_SynapseMigratesLegacyMemoriesOnce 覆盖文档 4.10 的迁移：
// 首次启用把旧 embedded 库的 memories 导进图，之后不再重复导入——包括
// 「用户清空全部记忆之后重启」这种最容易把旧数据又导回来的情形。
func TestAssemble_SynapseMigratesLegacyMemoriesOnce(t *testing.T) {
	cfg := synapseConfig(t, "graph-user")
	legacyPath := filepath.Join(cfg.Paths.DataDir, "memory.db")

	// 先用旧后端（embedded）写一条记忆——这正是升级前用户的库形态。
	legacyCfg := memory.DefaultConfig()
	legacyCfg.Enabled = true
	legacyCfg.Backend = memory.BackendEmbedded
	legacyCfg.UserID = "graph-user"
	old, err := memory.NewEmbeddedBackend(legacyPath, legacyCfg)
	if err != nil {
		t.Fatalf("打开旧 embedded 库: %v", err)
	}
	if _, err := old.Add(context.Background(), []memory.Message{
		{Role: "user", Content: "项目 XimoAgent 使用 Go 引擎"},
	}, memory.AddOptions{RunID: "legacy-1"}); err != nil {
		t.Fatalf("旧库写入: %v", err)
	}
	if err := old.Close(); err != nil {
		t.Fatalf("关闭旧库: %v", err)
	}

	// 第一次装配：应当把旧记忆迁移进图。
	app1, err := bootstrap.New(cfg, bootstrap.Options{
		MigrationsDir:  migrationsDir(t),
		Provider:       scriptedProvider(),
		WorkspaceRoot:  t.TempDir(),
		DisableWorkers: true,
	})
	if err != nil {
		t.Fatalf("bootstrap.New #1: %v", err)
	}
	g1 := app1.MemoryGraph()
	if g1 == nil {
		t.Fatal("MemoryGraph 为空")
	}
	st1 := g1.Stats(context.Background())
	if st1.Nodes == 0 {
		t.Fatal("旧 memories 应被迁移进图，实际 0 个节点")
	}
	app1.Close()

	// 第二次装配：迁移标记已置位，节点数不应变化。
	app2, err := bootstrap.New(cfg, bootstrap.Options{
		MigrationsDir:  migrationsDir(t),
		Provider:       scriptedProvider(),
		WorkspaceRoot:  t.TempDir(),
		DisableWorkers: true,
	})
	if err != nil {
		t.Fatalf("bootstrap.New #2: %v", err)
	}
	st2 := app2.MemoryGraph().Stats(context.Background())
	if st2.Nodes != st1.Nodes {
		t.Fatalf("二次装配重复迁移: %d → %d 个节点", st1.Nodes, st2.Nodes)
	}

	// 清空全部记忆之后再重启：旧数据不得被重新导入。
	if _, err := app2.MemoryGraph().Clear(context.Background()); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if got := app2.MemoryGraph().Stats(context.Background()).Nodes; got != 0 {
		t.Fatalf("清空后应剩 0 个节点，实际 %d", got)
	}
	app2.Close()

	app3, err := bootstrap.New(cfg, bootstrap.Options{
		MigrationsDir:  migrationsDir(t),
		Provider:       scriptedProvider(),
		WorkspaceRoot:  t.TempDir(),
		DisableWorkers: true,
	})
	if err != nil {
		t.Fatalf("bootstrap.New #3: %v", err)
	}
	defer app3.Close()
	if got := app3.MemoryGraph().Stats(context.Background()).Nodes; got != 0 {
		t.Fatalf("清空后重启不应重新导入旧数据，实际 %d 个节点", got)
	}
}

// 记忆未启用时不得暴露记忆图能力：IPC 层据此对每个记忆帧返回明确错误，
// 而不是「成功但什么都没做」。
func TestAssemble_MemoryDisabledExposesNoGraph(t *testing.T) {
	cfg := newTestConfig(t)
	cfg.Memory = config.MemoryConfig{Enabled: false}
	app, err := bootstrap.New(cfg, bootstrap.Options{
		MigrationsDir:  migrationsDir(t),
		Provider:       scriptedProvider(),
		WorkspaceRoot:  t.TempDir(),
		DisableWorkers: true,
	})
	if err != nil {
		t.Fatalf("bootstrap.New: %v", err)
	}
	defer app.Close()
	if g := app.MemoryGraph(); g != nil {
		t.Fatalf("未启用记忆时 MemoryGraph 应为 nil，实际 %#v", g)
	}
}
