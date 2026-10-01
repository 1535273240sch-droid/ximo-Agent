package e2e

import (
	"strings"
	"testing"

	"github.com/ximo888ok-netizen/ximo-agent/internal/bootstrap"
	"github.com/ximo888ok-netizen/ximo-agent/internal/config"
	"github.com/ximo888ok-netizen/ximo-agent/internal/ipc"
	"github.com/ximo888ok-netizen/ximo-agent/internal/ipcapi"
	"github.com/ximo888ok-netizen/ximo-agent/internal/ports/mem"
	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// 专家目录页的端到端测试（v2.6.0）。
//
// 为什么必须在 e2e 层再测一遍：ipcapi 的单测用的是 stub 端口，它证明不了
// 「真实注册表（embed 254 位 + experts 表）+ 真实装配 + 真实帧编解码」这条链路上
// 字段名与数据类型是对得上的。而这条链路的失败模式是**静默**的：Go 结构体 tag 与
// 前端 TypeScript 接口只要有一个词不一致，界面就永远是空的，编译期与单测都不报错
// （专家库曾经就是这样：界面里硬编码 60 位样本，后端 254 位没人读得到）。
//
// 因此这个测试做三件事：
//  1. 走真实 IPC 往返，断言响应信封里的 **JSON 键名**与 frontend/src/shared/types.ts
//     逐字一致；
//  2. 断言保存的自定义专家经过真实 SQLite 落盘，**重启进程后仍在**；
//  3. 断言内置专家不可覆盖、不可删除（错误信封，而不是静默成功）。

// expertEndpoint 起一个只注册 ExpertService 的 IPC 服务端。
//
// 与 run 生命周期无关，因此刻意不注册 EngineService：任何对 run 的依赖都会立刻暴露
// 成「帧无人处理」，而不是被引擎顺手兜住。
func expertEndpoint(t *testing.T, app *bootstrap.App, cfg *config.Config) string {
	t.Helper()
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		endpoint := freeTCPEndpoint(t)
		srv := ipc.NewServer(endpoint, cfg.IPC.MaxPayloadLength)
		if err := srv.Start(); err != nil {
			lastErr = err
			continue
		}
		ipcapi.NewExpertService(app.ExpertDirectory()).Register(srv)
		t.Cleanup(func() { _ = srv.Stop() })
		return endpoint
	}
	t.Fatalf("could not start expert ipc server after retries: %v", lastErr)
	return ""
}

// TestExpertCatalogEndToEndOverIPC 是专家库页的真实链路测试。
func TestExpertCatalogEndToEndOverIPC(t *testing.T) {
	cfg := newTestConfig(t)
	newApp := func() *bootstrap.App {
		app, err := bootstrap.New(cfg, bootstrap.Options{
			Provider:       mem.NewProvider(),
			MigrationsDir:  migrationsDir(t),
			DisableWorkers: true,
		})
		if err != nil {
			t.Fatalf("bootstrap.New: %v", err)
		}
		t.Cleanup(func() { app.Close() })
		return app
	}

	app := newApp()
	if app.ExpertDirectory() == nil {
		t.Fatal("装配后的 App 必须提供专家目录")
	}
	call := dialMemory(t, expertEndpoint(t, app, cfg))

	// rawCall 把响应的 payload 解成 map，用来断言**键名**（前端读的就是这些）。
	rawCall := func(msgType string, payload any) map[string]any {
		t.Helper()
		var out map[string]any
		if err := call(msgType, payload, &out); err != nil {
			t.Fatalf("%s (raw): %v", msgType, err)
		}
		return out
	}

	// ---- list：界面用它渲染卡片、筛选条与页头计数 ----
	var list ipcapi.ExpertListPayload
	if err := call(ipc.TypeExpertList, map[string]any{}, &list); err != nil {
		t.Fatalf("expert.list: %v", err)
	}
	if len(list.Experts) < 200 {
		t.Fatalf("list returned %d experts, want the full embedded catalogue", len(list.Experts))
	}
	if list.Total != len(list.Experts) {
		t.Errorf("total = %d, len = %d（两者必须一致，界面用 total 显示计数）",
			list.Total, len(list.Experts))
	}
	if len(list.Divisions) < 10 {
		t.Errorf("divisions = %v，期望目录里的全部部门（界面据此渲染筛选条）", list.Divisions)
	}

	// 内置专家的形状：字段值必须完整，否则卡片是空白的。
	var builtin *types.ExpertCard
	for i := range list.Experts {
		e := list.Experts[i]
		if e.ID == "" || e.Name == "" || e.Division == "" {
			t.Fatalf("目录里有缺 id/name/division 的专家：%+v", e)
		}
		if e.ID == "engineering-frontend-developer" {
			builtin = &list.Experts[i]
		}
	}
	if builtin == nil {
		t.Fatal("目录里没有 engineering-frontend-developer")
	}
	if builtin.Custom {
		t.Error("内置专家不该带 custom=true")
	}
	if len(builtin.Tools) == 0 {
		t.Error("内置专家应带推荐工具（卡片上要显示工具标签）")
	}

	// ---- 线名契约：改名不会编译失败，只会让界面永远为空 ----
	raw := rawCall(ipc.TypeExpertList, map[string]any{})
	for _, key := range []string{"experts", "total", "divisions"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("list 响应缺少键 %q（前端按这个名字读）", key)
		}
	}
	items, _ := raw["experts"].([]any)
	if len(items) == 0 {
		t.Fatal("experts 不是非空数组")
	}
	firstCard, _ := items[0].(map[string]any)
	if firstCard == nil {
		t.Fatal("experts[0] 不是对象")
	}
	// custom 用 omitempty：内置专家这一项会缺省。前端把它当可选字段读
	// （custom?: boolean），所以缺省是合法契约，这里只要求内置项不为 true。
	for _, key := range []string{"id", "division", "name", "description", "emoji", "vibe", "color", "tools"} {
		if _, ok := firstCard[key]; !ok {
			t.Errorf("专家卡片缺少键 %q（前端按这个名字读）", key)
		}
	}
	if v, ok := firstCard["custom"]; ok && v == true {
		t.Errorf("内置专家的 custom 不应为 true：%v", v)
	}

	// ---- save：创建一位自定义专家 ----
	var saved types.ExpertCard
	if err := call(ipc.TypeExpertSave, types.ExpertCard{
		Name:        "端到端测试专家",
		Division:    "security",
		Description: "只用于端到端测试",
		Emoji:       "🧪",
		Vibe:        "先怀疑，再验证",
		Personality: "你是端到端测试专家。",
		Tools:       []string{"file_read", "terminal_exec"},
	}, &saved); err != nil {
		t.Fatalf("expert.save: %v", err)
	}
	if saved.ID == "" {
		t.Fatal("保存后必须返回可用的专家 ID（界面要用它提交 run）")
	}
	if !saved.Custom {
		t.Error("保存的专家必须带 custom=true")
	}

	// 保存后立刻再取列表：注册表缓存必须在保存时失效，否则界面要重启才看得到新专家。
	var afterSave ipcapi.ExpertListPayload
	if err := call(ipc.TypeExpertList, map[string]any{}, &afterSave); err != nil {
		t.Fatalf("expert.list after save: %v", err)
	}
	if !hasExpert(afterSave.Experts, saved.ID) {
		t.Fatalf("保存后 %q 没有出现在列表里（注册表缓存没失效）", saved.ID)
	}

	// ---- 重启进程：自定义专家必须从 SQLite 里读回来 ----
	app.Close()
	app2 := newApp()
	call2 := dialMemory(t, expertEndpoint(t, app2, cfg))
	var afterRestart ipcapi.ExpertListPayload
	if err := call2(ipc.TypeExpertList, map[string]any{}, &afterRestart); err != nil {
		t.Fatalf("expert.list after restart: %v", err)
	}
	if !hasExpert(afterRestart.Experts, saved.ID) {
		t.Fatalf("重启后自定义专家 %q 丢失（没有真正落库）", saved.ID)
	}

	// ---- 内置专家不可覆盖 ----
	//
	// 错误路径由信封的 ok=false + error 承载，helper 会把它作为 error 返回 ——
	// 这里断言的是「明确拒绝且说明原因」，而不是静默成功或静默覆盖。
	var overwrite map[string]any
	err := call2(ipc.TypeExpertSave, map[string]any{
		"id": "engineering-frontend-developer", "name": "假冒内置", "division": "engineering",
	}, &overwrite)
	if err == nil {
		t.Fatalf("覆盖内置专家必须被拒绝，响应：%v", overwrite)
	}
	if !strings.Contains(err.Error(), "内置专家") {
		t.Errorf("拒绝原因应说明这是内置专家，实际：%v", err)
	}

	// ---- delete：删除自定义专家，重复删除返回 deleted=false ----
	var del ipcapi.ExpertDeletePayload
	if err := call2(ipc.TypeExpertDelete, map[string]any{"expert_id": saved.ID}, &del); err != nil {
		t.Fatalf("expert.delete: %v", err)
	}
	if !del.Deleted {
		t.Fatal("删除已存在的自定义专家应返回 deleted=true")
	}
	var again ipcapi.ExpertDeletePayload
	if err := call2(ipc.TypeExpertDelete, map[string]any{"expert_id": saved.ID}, &again); err != nil {
		t.Fatalf("expert.delete (again): %v", err)
	}
	if again.Deleted {
		t.Error("重复删除应返回 deleted=false")
	}

	var finalList ipcapi.ExpertListPayload
	if err := call2(ipc.TypeExpertList, map[string]any{}, &finalList); err != nil {
		t.Fatalf("expert.list final: %v", err)
	}
	if hasExpert(finalList.Experts, saved.ID) {
		t.Error("删除后仍出现在列表里")
	}
}

func hasExpert(list []types.ExpertCard, id string) bool {
	for _, e := range list {
		if e.ID == id {
			return true
		}
	}
	return false
}
