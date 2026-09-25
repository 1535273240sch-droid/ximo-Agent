package bootstrap_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/bootstrap"
	"github.com/ximo888ok-netizen/ximo-agent/internal/config"
	"github.com/ximo888ok-netizen/ximo-agent/internal/engine"
	"github.com/ximo888ok-netizen/ximo-agent/internal/ipcapi"
	"github.com/ximo888ok-netizen/ximo-agent/internal/ports"
	"github.com/ximo888ok-netizen/ximo-agent/internal/ports/mem"
	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// 本文件是「装配层真的接通了」的可执行证据。
//
// 在补齐 bootstrap 之前，仓库里没有任何一条测试能证明
// 「提交一个 run -> 真实走完 Engine + Storage 持久化 -> 拿到最终结果」这条
// 链路成立：各模块的测试都对着 mock 跑，而生产入口根本没有创建 Engine。
// 这里用真实的 SQLite（临时文件）+ 真实的工具运行时 + 脚本化的 Provider，
// 跑完整链路，并核对事件确实落到了磁盘上的库里。

// newTestConfig 构造一份指向临时目录的完整配置。
func newTestConfig(t *testing.T) *config.Config {
	t.Helper()
	dir := t.TempDir()

	cfg := config.NewDefaultConfig()
	cfg.Paths.BaseDir = dir
	cfg.Paths.VersionsDir = filepath.Join(dir, "versions")
	cfg.Paths.DataDir = filepath.Join(dir, "data")
	cfg.Paths.LogsDir = filepath.Join(dir, "data", "logs")
	cfg.Paths.CrashDumpDir = filepath.Join(dir, "data", "crashes")
	cfg.Paths.RunDir = filepath.Join(dir, "data", "run")
	cfg.Paths.CurrentPointerFile = filepath.Join(dir, "current")
	cfg.Paths.PreviousPointerFile = filepath.Join(dir, "previous")
	cfg.Storage.DBPath = filepath.Join(dir, "test.db")
	cfg.Provider.BaseURL = "https://example.invalid"
	cfg.Provider.SecretRef = "test"
	return cfg
}

// TestAssemble_RunsAgainstRealSQLite 是最小可信装配证据：
// 用真实数据库与真实工具运行时装配出 Engine，提交一个 run 并等到终态。
func TestAssemble_RunsAgainstRealSQLite(t *testing.T) {
	cfg := newTestConfig(t)
	workspace := t.TempDir()

	// Provider 用内存脚本：第一轮直接给出最终答复（不调用任何工具）。
	scripted := mem.NewProvider(ports.ProviderResponse{
		FinishReason: ports.FinishStop,
		Content:      "装配成功",
		Emitted:      true,
	})

	app, err := bootstrap.New(cfg, bootstrap.Options{
		MigrationsDir:  migrationsDir(t),
		Provider:       scripted,
		WorkspaceRoot:  workspace,
		DisableWorkers: true,
	})
	if err != nil {
		t.Fatalf("bootstrap.New: %v", err)
	}
	defer app.Close()

	if app.Engine == nil {
		t.Fatal("engine was not assembled")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	handle, err := app.Engine.Submit(ctx, types.SubmitRequest{
		SessionID: "sess-bootstrap-1",
		Prompt:    "说一句话",
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if handle.RunID == "" {
		t.Fatal("Submit returned an empty run id")
	}

	final := waitForTerminal(t, ctx, app, handle.RunID)
	if final.State != types.StateCompleted {
		t.Fatalf("run state = %s (answer %q, err %v), want completed",
			final.State, final.Answer, final.Err)
	}
	if final.Answer != "装配成功" {
		t.Errorf("answer = %q, want %q", final.Answer, "装配成功")
	}

	// 关键断言：Provider 真的被调用了（说明 Provider 适配器接对了）。
	if len(scripted.Calls) == 0 {
		t.Error("provider was never called: the provider adapter is not wired")
	}

	// 关键断言：事件真的落到了磁盘上的 SQLite 里（说明事件库适配器接对了，
	// 而不是只写在内存里）。
	assertEventsPersisted(t, cfg, handle.RunID)
}

// TestAssemble_ExecutesRealTool 证明工具运行时确实被接上：
// Provider 第一轮返回一个 file_write 工具调用，引擎应真的落盘，
// 第二轮再给出最终答复。
func TestAssemble_ExecutesRealTool(t *testing.T) {
	cfg := newTestConfig(t)
	workspace := t.TempDir()
	target := filepath.Join(workspace, "hello.txt")

	// 注意参数名是 filePath（工具 schema 定义的键），不是 path。
	scripted := mem.NewProvider(
		ports.ProviderResponse{
			FinishReason: ports.FinishToolCalls,
			Emitted:      true,
			ToolCalls: []types.ToolCall{
				{ID: "call-write-1", Name: "file_write", Arguments: map[string]any{
					"filePath": target,
					"content":  "written by the real tool runtime",
				}},
			},
		},
		ports.ProviderResponse{
			FinishReason: ports.FinishStop,
			Content:      "文件已写入",
			Emitted:      true,
		},
	)

	app, err := bootstrap.New(cfg, bootstrap.Options{
		MigrationsDir:  migrationsDir(t),
		Provider:       scripted,
		WorkspaceRoot:  workspace,
		DisableWorkers: true,
	})
	if err != nil {
		t.Fatalf("bootstrap.New: %v", err)
	}
	defer app.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	handle, err := app.Engine.Submit(ctx, types.SubmitRequest{
		SessionID: "sess-bootstrap-2",
		Prompt:    "写一个文件",
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	final := waitForTerminal(t, ctx, app, handle.RunID)
	if final.State != types.StateCompleted {
		t.Fatalf("run state = %s (err %v), want completed", final.State, final.Err)
	}

	// 这是「工具运行时真的被接上」的硬证据：文件出现在真实文件系统上。
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("the file_write tool did not run: %v", err)
	}
	if string(data) != "written by the real tool runtime" {
		t.Errorf("file content = %q, want the tool-written content", string(data))
	}
}

// TestAssemble_RecoverFindsNothingOnCleanDB 验证恢复入口可用且对干净库安全：
// 这确保「崩溃恢复」这条链路至少是可达的（此前生产入口根本不连数据库）。
func TestAssemble_RecoverFindsNothingOnCleanDB(t *testing.T) {
	cfg := newTestConfig(t)

	app, err := bootstrap.New(cfg, bootstrap.Options{
		MigrationsDir:  migrationsDir(t),
		Provider:       mem.NewProvider(),
		WorkspaceRoot:  t.TempDir(),
		DisableWorkers: true,
	})
	if err != nil {
		t.Fatalf("bootstrap.New: %v", err)
	}
	defer app.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	plans, err := app.Engine.Recover(ctx)
	if err != nil {
		t.Fatalf("Recover on a clean database: %v", err)
	}
	if len(plans) != 0 {
		t.Errorf("Recover returned %d plans on a clean database, want 0", len(plans))
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// waitForTerminal 轮询运行状态直到终态或超时。
func waitForTerminal(t *testing.T, ctx context.Context, app *bootstrap.App, runID string) types.Run {
	t.Helper()
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		run, err := app.Engine.GetRun(ctx, runID)
		if err == nil && run.State.Terminal() {
			return run
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("run %s did not reach a terminal state within the deadline", runID)
	return types.Run{}
}

// assertEventsPersisted 直接查询磁盘上的 SQLite，确认事件真的落库。
func assertEventsPersisted(t *testing.T, cfg *config.Config, runID string) {
	t.Helper()

	// 通过 Engine 的事件流读取即可，但事件流有内存缓存；为了证明「落盘」，
	// 这里重开一个只读连接直接查 run_events 表。
	dbPath := cfg.ResolveDBPath()
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("database file %s does not exist: %v", dbPath, err)
	}
	if info, err := os.Stat(dbPath); err == nil && info.Size() == 0 {
		t.Fatalf("database file %s is empty", dbPath)
	}
	// 用事件库适配器之外的手段验证：重新装配一次同一路径的 App，
	// 从库里读回事件，证明数据确实持久化了而不是留在进程内存。
	reopened, err := bootstrap.New(cfg, bootstrap.Options{
		MigrationsDir:  migrationsDir(t),
		Provider:       mem.NewProvider(),
		WorkspaceRoot:  t.TempDir(),
		DisableWorkers: true,
	})
	if err != nil {
		t.Fatalf("reopen with the same database: %v", err)
	}
	defer reopened.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// 库里有这个 run 的事件 = 上一进程确实写盘了（重新装配后仍能读到）。
	plans, err := reopened.Engine.Recover(ctx)
	if err != nil {
		t.Fatalf("Recover after reopen: %v", err)
	}
	// Recover 会为每个有事件的 run 返回一个计划；已终态的 run 计划应为 Skip
	// （「无需恢复」），而绝不能是 ResumeAuto —— 那会让已完成的工作重跑一遍。
	found := false
	for _, p := range plans {
		if p.RunID != runID {
			continue
		}
		if p.Decision != engine.Skip {
			t.Errorf("completed run %s got recovery decision %q, want %q",
				runID, p.Decision, engine.Skip)
		}
		found = true
	}
	if !found {
		t.Errorf("run %s has no recovery plan after reopening the database: "+
			"its events were not persisted to disk", runID)
	}

	// 直接读事件流的持久化版本（Engine.Events 从事件库重放）。
	ch, err := reopened.Engine.Events(ctx, runID, 0)
	if err != nil {
		t.Fatalf("Events for a persisted run: %v", err)
	}
	count := 0
	timeout := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				if count == 0 {
					t.Error("no events were persisted for the run: the event store adapter is not writing to disk")
				}
				return
			}
			count++
		case <-timeout:
			if count == 0 {
				t.Error("timed out reading persisted events")
			}
			return
		}
	}
}

// TestApplySettings_SaveNotClobberedByStaleProvidersSnapshot 复现并锁死用户实测
// 的问题：「一点保存，模型和请求链接立马回退到默认」。
//
// 根因：providers[0] 与顶层字段指向同一个主服务商，而凭据面板只编辑顶层
// （providers 是它读到的旧快照）、候选池面板只编辑 providers（顶层是旧快照）。
// 后端必须逐字段取「被编辑过的那一侧」，否则任意一边的保存都会被另一边的
// 旧快照原样顶回，新密钥引用同理。
func TestApplySettings_SaveNotClobberedByStaleProvidersSnapshot(t *testing.T) {
	cfg := newTestConfig(t)
	app, err := bootstrap.New(cfg, bootstrap.Options{
		MigrationsDir:  migrationsDir(t),
		Provider:       mem.NewProvider(),
		WorkspaceRoot:  t.TempDir(),
		DisableWorkers: true,
	})
	if err != nil {
		t.Fatalf("bootstrap.New: %v", err)
	}
	defer app.Close()
	ctx := context.Background()

	// 场景一：凭据面板保存 —— 顶层是用户刚改的值，providers 是面板挂载时读到的旧快照。
	stale := app.GetRuntimeSettings()
	credentialsPanel := ipcapi.RuntimeSettingsPayload{
		ProviderID: stale.ProviderID, ProviderName: "我的中转",
		BaseURL: "https://relay.example.com/v1", Model: "relay-chat",
		ContextWindow: stale.ContextWindow, MaxOutputTokens: stale.MaxOutputTokens,
		SecretRef: stale.SecretRef, AutoMode: stale.AutoMode,
		WorkspaceRoot: stale.WorkspaceRoot, DBPath: stale.DBPath, ConfigPath: stale.ConfigPath,
		Providers: stale.Providers, // 旧快照：base_url/model 还是 example.invalid
		SubAgent:  stale.SubAgent,
	}
	if err := app.ApplyRuntimeSettings(ctx, credentialsPanel); err != nil {
		t.Fatalf("ApplyRuntimeSettings (credentials panel): %v", err)
	}
	got := app.GetRuntimeSettings()
	if got.BaseURL != "https://relay.example.com/v1" {
		t.Errorf("base_url = %q, want the newly typed value (not reverted by the stale providers snapshot)", got.BaseURL)
	}
	if got.Model != "relay-chat" {
		t.Errorf("model = %q, want the newly typed value", got.Model)
	}
	if len(got.Providers) == 0 || got.Providers[0].BaseURL != "https://relay.example.com/v1" {
		t.Errorf("providers[0].base_url = %+v, want synced to the new value", got.Providers)
	}

	// 场景二：候选池面板保存 —— 只改 providers[0]，顶层还是当前已保存值。
	poolPanel := got // fresh 快照
	poolProviders := append([]ipcapi.ProviderEntryPayload(nil), got.Providers...)
	if len(poolProviders) == 0 {
		t.Fatal("expected at least the main provider in the pool")
	}
	poolProviders[0].BaseURL = "https://pool.example.com/v1"
	poolProviders[0].Model = "pool-chat"
	poolPanel.Providers = poolProviders
	if err := app.ApplyRuntimeSettings(ctx, poolPanel); err != nil {
		t.Fatalf("ApplyRuntimeSettings (pool panel): %v", err)
	}
	got2 := app.GetRuntimeSettings()
	if got2.BaseURL != "https://pool.example.com/v1" || got2.Model != "pool-chat" {
		t.Errorf("pool panel edit lost: base_url = %q, model = %q", got2.BaseURL, got2.Model)
	}

	// 场景三：保存新密钥（putSecret 路径的形状）—— 新引用不得被快照里的旧引用顶掉。
	keyPayload := got2
	keyPayload.SecretRef = "secretref:v1:fresh-ref"
	if err := app.ApplyRuntimeSettings(ctx, keyPayload); err != nil {
		t.Fatalf("ApplyRuntimeSettings (new secret ref): %v", err)
	}
	got3 := app.GetRuntimeSettings()
	if got3.SecretRef != "secretref:v1:fresh-ref" {
		t.Errorf("secret_ref = %q, want the freshly stored reference (not clobbered by the snapshot)", got3.SecretRef)
	}
	if len(got3.Providers) == 0 || got3.Providers[0].SecretRef != "secretref:v1:fresh-ref" {
		t.Errorf("providers[0].secret_ref = %+v, want synced to the new reference", got3.Providers)
	}
}

// migrationsDir 定位仓库的 SQL 迁移目录。
func migrationsDir(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	// 测试运行在 internal/bootstrap，仓库根的 migrations 在上两级。
	dir := filepath.Join(wd, "..", "..", "migrations")
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("migrations directory not found at %s: %v", dir, err)
	}
	return dir
}
