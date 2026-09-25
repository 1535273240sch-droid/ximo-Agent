package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/ximo888ok-netizen/ximo-agent/internal/ports"
	"github.com/ximo888ok-netizen/ximo-agent/internal/ports/mem"
	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// TestClusterRunFanOutToExperts 是 Agent 集群模式的核心验收：
// 提交带 ClusterSize 的 run 后，引擎必须按任务内容自动组队、并行激活多位
// 专家子代理，并把它们的产出汇总进最终答案。
func TestClusterRunFanOutToExperts(t *testing.T) {
	// 每位专家走两阶段编排（方案 + 实施），故模型调用数是 2×集群规模；
	// 给足脚本，避免走到 Exhausted 兜底分支。
	script := make([]ports.ProviderResponse, 0, clusterMaxSize*2)
	for i := 0; i < clusterMaxSize*2; i++ {
		script = append(script, mem.FinalRound(fmt.Sprintf("专家产出 %d", i)))
	}
	h := newExpertHarness(t, script...)

	const size = 3
	ctx := context.Background()
	handle, err := h.engine.Submit(ctx, types.SubmitRequest{
		Prompt:      "设计一个支持十万并发连接的消息网关",
		ClusterSize: size,
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if err := h.engine.WaitRun(ctx, handle.RunID); err != nil {
		t.Fatalf("WaitRun: %v", err)
	}

	run, err := h.engine.GetRun(ctx, handle.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.State != types.StateCompleted {
		t.Fatalf("state = %s, want completed (err=%v)", run.State, run.Err)
	}
	if !strings.Contains(run.Answer, fmt.Sprintf("Agent 集群报告（%d 位专家并行）", size)) {
		t.Errorf("answer 不是集群报告格式：%q", run.Answer)
	}

	// 关键断言：真的派出了 size 位子代理。用模型调用次数而不是答案文本来判断，
	// 是因为并发下「哪位专家产出哪段文本」不确定，而调用次数是确定的。
	// 每位专家两阶段 → 至少 2×size 次调用。
	if got, want := len(h.provider.Calls), 2*size; got < want {
		t.Errorf("模型调用次数 = %d，期望至少 %d（%d 位专家 × 两阶段）；"+
			"调用数不足说明子代理没有真正并行跑起来", got, want, size)
	}

	// 事件里必须能看到集群规模，前端据此渲染集群卡片。
	var clusterEvent bool
	for _, ev := range h.events.All(handle.RunID) {
		if ev.Data == nil {
			continue
		}
		if n, ok := ev.Data["clusterSize"]; ok {
			clusterEvent = true
			if fmt.Sprint(n) != fmt.Sprint(size) {
				t.Errorf("clusterSize 事件 = %v，期望 %d", n, size)
			}
		}
	}
	if !clusterEvent {
		t.Error("没有记录集群启动事件，前端无法显示本次派出了多少个子代理")
	}
}

// TestClusterRunSizeMatchesResourceGate 锁定集群规模上限与资源闸门容量一致。
//
// 设置面板对用户的承诺是「同时最多 8 个」，该数字来自
// types.ResourceCapacities[ResourceClassExpertAgent]。集群规模上限若与之脱钩，
// 承诺就会失效（或者反过来浪费掉闸门容量）。
func TestClusterRunSizeMatchesResourceGate(t *testing.T) {
	capacity := types.ResourceCapacities[types.ResourceClassExpertAgent]
	if clusterMaxSize != capacity {
		t.Fatalf("clusterMaxSize = %d，但 expert_agent 资源闸门容量 = %d；"+
			"集群规模上限必须等于闸门容量，否则会超出对用户的承诺", clusterMaxSize, capacity)
	}
}

// TestClusterRunSizeIsCapped 验证请求的规模超限时被夹到上限，而不是原样透传。
func TestClusterRunSizeIsCapped(t *testing.T) {
	script := make([]ports.ProviderResponse, 0, clusterMaxSize*2)
	for i := 0; i < clusterMaxSize*2; i++ {
		script = append(script, mem.FinalRound("产出"))
	}
	h := newExpertHarness(t, script...)

	ctx := context.Background()
	handle, err := h.engine.Submit(ctx, types.SubmitRequest{
		Prompt:      "任务",
		ClusterSize: clusterMaxSize + 5,
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if err := h.engine.WaitRun(ctx, handle.RunID); err != nil {
		t.Fatalf("WaitRun: %v", err)
	}

	for _, ev := range h.events.All(handle.RunID) {
		if ev.Data == nil {
			continue
		}
		if n, ok := ev.Data["clusterSize"]; ok {
			if fmt.Sprint(n) != fmt.Sprint(clusterMaxSize) {
				t.Fatalf("clusterSize = %v，期望被夹到上限 %d", n, clusterMaxSize)
			}
			return
		}
	}
	t.Fatal("没有记录集群启动事件")
}

// TestClusterRunFailsLoudlyWithoutRegistry 验证专家库不可用时集群明确失败。
//
// 用户是主动打开集群开关的；静默退回主 Agent Loop 会得到一个「看起来正常、
// 但一个子代理都没跑」的结果，比直接报错难排查得多。
func TestClusterRunFailsLoudlyWithoutRegistry(t *testing.T) {
	h := newHarness(t, mem.FinalRound("这条回答不该出现"))
	h.engine.expertRegistry = nil

	ctx := context.Background()
	handle, err := h.engine.Submit(ctx, types.SubmitRequest{
		Prompt:      "任务",
		ClusterSize: 3,
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if err := h.engine.WaitRun(ctx, handle.RunID); err != nil {
		t.Fatalf("WaitRun: %v", err)
	}

	run, err := h.engine.GetRun(ctx, handle.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.State != types.StateFailed {
		t.Fatalf("state = %s，期望 failed（专家库不可用时集群必须明确失败）", run.State)
	}
	if strings.Contains(run.Answer, "这条回答不该出现") {
		t.Error("集群在专家库不可用时静默退回了主 Agent Loop —— 这会让" +
			"「开了集群却一个子代理都没跑」难以察觉")
	}
}

// TestClusterRunPrefersExplicitExpert 验证 ExpertID 与 ClusterSize 同时存在时，
// 手选专家优先（用户的明确选择比"自动组队"更具体）。
func TestClusterRunPrefersExplicitExpert(t *testing.T) {
	h := newExpertHarness(t,
		mem.FinalRound("1. 计划"),
		mem.FinalRound("直连专家产出"),
	)

	ctx := context.Background()
	handle, err := h.engine.Submit(ctx, types.SubmitRequest{
		Prompt:      "帮我审查代码",
		ExpertID:    "engineering-frontend-developer",
		ClusterSize: 4,
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if err := h.engine.WaitRun(ctx, handle.RunID); err != nil {
		t.Fatalf("WaitRun: %v", err)
	}

	run, err := h.engine.GetRun(ctx, handle.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if strings.Contains(run.Answer, "Agent 集群报告") {
		t.Error("ExpertID 与 ClusterSize 同时存在时走了集群路径；" +
			"手选专家更具体，应当优先")
	}
	if !strings.Contains(run.Answer, "直连专家产出") {
		t.Errorf("answer = %q，期望走专家直连路径", run.Answer)
	}
}
