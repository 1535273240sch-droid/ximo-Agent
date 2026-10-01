package bootstrap_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ximo888ok-netizen/ximo-agent/internal/bootstrap"
	"github.com/ximo888ok-netizen/ximo-agent/internal/ports"
	"github.com/ximo888ok-netizen/ximo-agent/internal/ports/mem"
	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// TestExpertCatalogRoundTrip 是「专家库真的接上了」的端到端证据：
// 内置目录可见 → 保存自定义专家 → 重启后仍在 → 删除后消失。
//
// 这一整条链路在 v2.6.0 之前根本不存在：expert.CustomStore 只有测试假件、
// Dependencies.Experts 从未被赋值（引擎内部退化成 NewRegistry(nil)）、也没有任何
// IPC 路由能创建自定义专家。没有这条测试，"自定义专家"永远只是接口签名。
func TestExpertCatalogRoundTrip(t *testing.T) {
	cfg := newTestConfig(t)
	workspace := t.TempDir()
	newApp := func() *bootstrap.App {
		app, err := bootstrap.New(cfg, bootstrap.Options{
			MigrationsDir:  migrationsDir(t),
			Provider:       mem.NewProvider(ports.ProviderResponse{FinishReason: ports.FinishStop, Content: "ok", Emitted: true}),
			WorkspaceRoot:  workspace,
			DisableWorkers: true,
		})
		if err != nil {
			t.Fatalf("bootstrap.New: %v", err)
		}
		return app
	}

	ctx := context.Background()
	app := newApp()

	dir := app.ExpertDirectory()
	if dir == nil {
		t.Fatal("装配后的 App 必须提供专家目录（ExpertDirectory 返回 nil）")
	}

	// 1. 内置目录可见：embed 的 254 位专家必须出现在列表里，且带推荐工具。
	list, err := dir.ListExperts(ctx)
	if err != nil {
		t.Fatalf("ListExperts: %v", err)
	}
	if len(list) < 200 {
		t.Fatalf("内置目录只有 %d 位专家，期望 embed 的全量目录", len(list))
	}
	var builtin *types.ExpertCard
	for i := range list {
		if list[i].ID == "engineering-frontend-developer" {
			builtin = &list[i]
			break
		}
	}
	if builtin == nil {
		t.Fatal("目录里没有 engineering-frontend-developer")
	}
	if builtin.Custom {
		t.Error("内置专家不该被标记为自定义")
	}
	if len(builtin.Tools) == 0 {
		t.Error("内置专家应带推荐工具（界面要显示工具标签）")
	}

	// 2. 保存自定义专家：ID 由后端按名字生成（确定性），并强制 Custom。
	saved, err := dir.SaveExpert(ctx, types.ExpertCard{
		Name:        "渗透测试专家",
		Division:    "security",
		Description: "专做授权范围内的渗透测试",
		Emoji:       "🗡️",
		Vibe:        "先假设系统有洞，再证明或推翻它",
		Personality: "你是渗透测试专家，只做授权范围内的测试。",
		Tools:       []string{"file_read", "terminal_exec"},
	})
	if err != nil {
		t.Fatalf("SaveExpert: %v", err)
	}
	if saved.ID == "" {
		t.Fatal("保存后应返回生成的 ID")
	}
	if !saved.Custom {
		t.Error("保存的专家必须标记为 Custom")
	}

	// 3. 立刻可见（注册表的缓存必须在保存时失效）。
	after, err := dir.ListExperts(ctx)
	if err != nil {
		t.Fatalf("ListExperts: %v", err)
	}
	if !containsExpert(after, saved.ID) {
		t.Fatalf("保存后 %q 没有出现在目录里（缓存没失效）", saved.ID)
	}
	if containsExpert(list, saved.ID) {
		t.Error("保存前的列表不该包含刚保存的专家（测试前提不成立）")
	}

	// 4. 重启后仍在：这是"真的落库了"的唯一证据。
	app.Close()
	app2 := newApp()
	defer app2.Close()
	dir2 := app2.ExpertDirectory()
	if dir2 == nil {
		t.Fatal("重启后的 App 没有专家目录")
	}
	persisted, err := dir2.ListExperts(ctx)
	if err != nil {
		t.Fatalf("ListExperts: %v", err)
	}
	var found *types.ExpertCard
	for i := range persisted {
		if persisted[i].ID == saved.ID {
			found = &persisted[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("重启后自定义专家 %q 丢失（没有真正落库）", saved.ID)
	}
	if found.Name != "渗透测试专家" || found.Division != "security" {
		t.Errorf("重启后读回的字段不一致：%+v", *found)
	}
	if found.Vibe == "" || found.Personality == "" || found.Emoji == "" {
		t.Errorf("重启后丢了风格/人格/emoji：%+v", *found)
	}
	if len(found.Tools) != 2 {
		t.Errorf("重启后工具列表 = %v，期望 2 项", found.Tools)
	}

	// 5. 内置专家不可覆盖、不可删除。
	if _, err := dir2.SaveExpert(ctx, types.ExpertCard{
		ID: "engineering-frontend-developer", Name: "假冒内置专家", Division: "engineering",
	}); err == nil {
		t.Error("覆盖内置专家必须失败")
	} else if !strings.Contains(err.Error(), "内置专家") {
		t.Errorf("覆盖内置专家的错误应说明原因，实际：%v", err)
	}
	if _, err := dir2.DeleteExpert(ctx, "engineering-frontend-developer"); err == nil {
		t.Error("删除内置专家必须失败")
	}

	// 6. 删除自定义专家后消失；重复删除返回 false。
	ok, err := dir2.DeleteExpert(ctx, saved.ID)
	if err != nil {
		t.Fatalf("DeleteExpert: %v", err)
	}
	if !ok {
		t.Fatal("删除已存在的自定义专家应返回 true")
	}
	remaining, err := dir2.ListExperts(ctx)
	if err != nil {
		t.Fatalf("ListExperts: %v", err)
	}
	if containsExpert(remaining, saved.ID) {
		t.Error("删除后仍在目录里")
	}
	if ok, err := dir2.DeleteExpert(ctx, saved.ID); err != nil || ok {
		t.Errorf("重复删除 = (%v, %v)，期望 (false, nil)", ok, err)
	}
}

// TestExpertCatalogSurvivesConfigReloadOfRegistry 确认目录实例在 App 上是同一份：
// IPC 保存的专家与引擎执行时读到的目录必须是同一个注册表，否则"刚保存就找不到"。
func TestExpertCatalogIsSharedWithEngine(t *testing.T) {
	cfg := newTestConfig(t)
	app, err := bootstrap.New(cfg, bootstrap.Options{
		MigrationsDir:  migrationsDir(t),
		Provider:       mem.NewProvider(ports.ProviderResponse{FinishReason: ports.FinishStop, Content: "ok", Emitted: true}),
		WorkspaceRoot:  t.TempDir(),
		DisableWorkers: true,
	})
	if err != nil {
		t.Fatalf("bootstrap.New: %v", err)
	}
	defer app.Close()

	if app.ExpertDirectory() == nil {
		t.Fatal("ExpertDirectory 不应为 nil")
	}
	// 引擎与目录服务必须共用同一份注册表：保存走目录，执行走引擎。
	// 这里通过「保存后立刻能按 id 提交 run 之前的查找路径」间接验证 —— 引擎在
	// executeExpertRun 里用 e.expertRegistry.Get(expertID)，若两者不是同一实例，
	// 用户刚保存的专家在提交时就会报「未知专家」。
	ctx := context.Background()
	dir := app.ExpertDirectory()
	saved, err := dir.SaveExpert(ctx, types.ExpertCard{
		Name: "共享目录专家", Division: "specialized", Description: "d",
	})
	if err != nil {
		t.Fatalf("SaveExpert: %v", err)
	}

	// 用同一个 id 提交 run。断言的是「引擎解析到了这位专家」，而不是 run 必须成功：
	// 这个测试用的 cfg 把 Provider.SecretRef 指向一个不存在的密钥引用，子代理模型池
	// 会在真正发起调用时报「解析 API Key 失败」—— 那与目录无关。
	//
	// 关键证据有两条，都指向同一件事（引擎与 IPC 目录共用一份注册表）：
	//   1. 失败原因不是「未知专家」（查不到才会走到那条分支）；
	//   2. 降级文案里带着这位自定义专家的名字、部门工作流与推荐工具。
	handle, err := app.Engine.Submit(ctx, types.SubmitRequest{
		Prompt:   "打个招呼",
		ExpertID: saved.ID,
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if err := app.Engine.WaitRun(ctx, handle.RunID); err != nil {
		t.Fatalf("WaitRun: %v", err)
	}
	run, err := app.Engine.GetRun(ctx, handle.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if strings.Contains(run.Answer, "未知专家") || strings.Contains(run.Answer, "not found") {
		t.Fatalf("引擎查不到刚保存的自定义专家：answer=%q", run.Answer)
	}
	if !strings.Contains(run.Answer, "共享目录专家") {
		t.Fatalf("run 的输出里没有这位自定义专家，说明引擎用的不是同一份目录；answer=%q",
			run.Answer)
	}
}

func containsExpert(list []types.ExpertCard, id string) bool {
	for _, e := range list {
		if e.ID == id {
			return true
		}
	}
	return false
}
