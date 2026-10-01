package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// fakeRunCatalog 是脚本化的 RunRequestCatalog。
type fakeRunCatalog struct {
	prompt string
	model  string
	ok     bool
	calls  int
}

func (f *fakeRunCatalog) RunRequest(context.Context, string) (string, string, bool) {
	f.calls++
	return f.prompt, f.model, f.ok
}

// TestRehydrateRunRestoresOriginalPromptAndModel 是审核文档 V1 的验收。
//
// prompt 与 model 不在事件日志里（日志只存边界），所以恢复出来的 run 此前用的是
// 占位 prompt + provider 默认模型：用户点「继续」后，模型收到的是
// "(recovered run: original prompt unavailable in this process)"，而模型名回落成
// 配置里的默认值 —— 用户当初选的那个模型被静默丢掉。
//
// runs 表本来就存了这两列，RunRequestCatalog 把它们读回来。
func TestRehydrateRunRestoresOriginalPromptAndModel(t *testing.T) {
	h := newHarness(t)
	catalog := &fakeRunCatalog{
		prompt: "把登录页改成暗色主题并跑一遍测试",
		model:  "chosen-model-9",
		ok:     true,
	}
	h.engine.deps.RunRequestCatalog = catalog

	plan := RecoveryPlan{
		RunID:       "run-recovered",
		SessionID:   "ses-recovered",
		ResumeState: types.StateThinking,
		Round:       3,
	}
	rec, err := h.engine.rehydrateRun(context.Background(), &plan)
	if err != nil {
		t.Fatalf("rehydrateRun: %v", err)
	}
	if rec == nil {
		t.Fatal("rehydrateRun 返回 nil")
	}
	if catalog.calls != 1 {
		t.Errorf("catalog 被调用 %d 次，期望 1", catalog.calls)
	}
	if got := rec.request.Prompt; got != "把登录页改成暗色主题并跑一遍测试" {
		t.Errorf("prompt = %q，期望原始 prompt（占位 prompt 会被当成用户任务发给模型）", got)
	}
	if got := rec.request.Model; got != "chosen-model-9" {
		t.Errorf("model = %q，期望原始模型（空值会让 provider 回落到默认模型）", got)
	}
	if !strings.Contains(strings.Join(plan.Notes, " "), "original prompt") {
		t.Errorf("恢复计划没有记录 prompt 来源：%v", plan.Notes)
	}
}

// TestRehydrateRunFallsBackToSyntheticPrompt 确认查不到时退回旧行为：
// 恢复流程必须仍然能推进，而不是因为回填失败卡住。
func TestRehydrateRunFallsBackToSyntheticPrompt(t *testing.T) {
	h := newHarness(t)
	h.engine.deps.RunRequestCatalog = &fakeRunCatalog{ok: false}

	rec, err := h.engine.rehydrateRun(context.Background(), &RecoveryPlan{
		RunID: "run-old", SessionID: "ses-old", ResumeState: types.StateThinking,
	})
	if err != nil {
		t.Fatalf("rehydrateRun: %v", err)
	}
	if rec == nil {
		t.Fatal("rehydrateRun 返回 nil")
	}
	if !strings.Contains(rec.request.Prompt, "recovered run") {
		t.Errorf("prompt = %q，期望退回占位提示", rec.request.Prompt)
	}
	if rec.request.Model != "" {
		t.Errorf("model = %q，期望留空（由 provider 回落到默认模型）", rec.request.Model)
	}
}

// TestRehydrateRunWithoutCatalogKeepsOldBehaviour 确认没装配 catalog 时行为不变
// （独立构建的引擎 / 只用了内存端口的测试）。
func TestRehydrateRunWithoutCatalogKeepsOldBehaviour(t *testing.T) {
	h := newHarness(t)

	rec, err := h.engine.rehydrateRun(context.Background(), &RecoveryPlan{
		RunID: "run-no-catalog", SessionID: "ses-no-catalog", ResumeState: types.StateThinking,
	})
	if err != nil {
		t.Fatalf("rehydrateRun: %v", err)
	}
	if rec == nil {
		t.Fatal("rehydrateRun 返回 nil")
	}
	if !strings.Contains(rec.request.Prompt, "recovered run") {
		t.Errorf("prompt = %q，期望占位提示", rec.request.Prompt)
	}
}

// TestRehydrateRunIgnoresEmptyCatalogValues 确认库里的空值不会把占位提示冲掉：
// 老行的 prompt 可能为空串，那与"查不到"应当同样处理。
func TestRehydrateRunIgnoresEmptyCatalogValues(t *testing.T) {
	h := newHarness(t)
	h.engine.deps.RunRequestCatalog = &fakeRunCatalog{prompt: "   ", model: "", ok: true}

	rec, err := h.engine.rehydrateRun(context.Background(), &RecoveryPlan{
		RunID: "run-blank", SessionID: "ses-blank", ResumeState: types.StateThinking,
	})
	if err != nil {
		t.Fatalf("rehydrateRun: %v", err)
	}
	if rec == nil {
		t.Fatal("rehydrateRun 返回 nil")
	}
	if !strings.Contains(rec.request.Prompt, "recovered run") {
		t.Errorf("prompt = %q，空值应退回占位提示", rec.request.Prompt)
	}
}
