package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestMetricsRegistry(t *testing.T) {
	m := NewMemoryMetrics()

	// 1. Counter 测试（低基数标签）
	m.Inc(MetricRunStartedTotal, L("mode", "interactive"))
	m.Inc(MetricRunStartedTotal, L("mode", "interactive"))
	if cnt := m.GetCounter(MetricRunStartedTotal, L("mode", "interactive")); cnt != 2 {
		t.Fatalf("expected counter 2, got %d", cnt)
	}

	// 2. Gauge 测试
	m.Gauge(MetricQueueDepth, 42)
	if g := m.GetGauge(MetricQueueDepth); g != 42 {
		t.Fatalf("expected gauge 42, got %v", g)
	}

	// 3. Histogram / Observe 测试
	m.Observe(MetricProviderLatencyMs, 100, L("provider", "deepseek"))
	m.Observe(MetricProviderLatencyMs, 200, L("provider", "deepseek"))
	m.Observe(MetricProviderLatencyMs, 300, L("provider", "deepseek"))

	// 4. Snapshot 测试
	snapshots := m.Snapshot()
	if len(snapshots) < 3 {
		t.Fatalf("expected at least 3 snapshots, got %d", len(snapshots))
	}

	// 5. Prometheus 输出验证
	promText := m.FormatPrometheus()
	if !strings.Contains(promText, "run_started_total") || !strings.Contains(promText, "queue_depth 42") {
		t.Fatalf("prometheus format mismatch: \n%s", promText)
	}

	// 6. 重置测试
	m.Reset()
	if cnt := m.GetCounter(MetricRunStartedTotal, L("mode", "interactive")); cnt != 0 {
		t.Fatalf("expected counter 0 after reset, got %d", cnt)
	}
}

// TestHighCardinalityProtection 验证指标高基数防护：黑名单剥离与容量上限溢出归并
func TestHighCardinalityProtection(t *testing.T) {
	m := NewMemoryMetrics()
	// 设置单指标最大允许 5 个不同标签序列
	m.SetLimits(5, 20)

	// 1. 验证黑名单高基数标签（run_id, session_id 等）被自动过滤，不产生独立时序
	for i := 0; i < 100; i++ {
		m.Inc(MetricRunStartedTotal, L("run_id", fmt.Sprintf("run-%05d", i)))
	}
	// 因为 run_id 被自动过滤，全部归并到了无标签的 MetricRunStartedTotal，总序列数应严格为 1
	if m.SeriesCount() != 1 {
		t.Fatalf("high-cardinality labels should be sanitized, expected 1 series, got %d", m.SeriesCount())
	}
	if cnt := m.GetCounter(MetricRunStartedTotal); cnt != 100 {
		t.Fatalf("expected total count 100, got %d", cnt)
	}

	// 2. 验证非黑名单但大量随机标签时，触发容量上限并归并至 overflow 桶
	for i := 0; i < 50; i++ {
		m.Inc("custom_event_total", L("dynamic_tag", fmt.Sprintf("val-%d", i)))
	}
	// 单指标最大 5 个序列 + 1 个 overflow 序列 = 最大 6 个序列
	snap := m.Snapshot()
	customSeries := 0
	hasOverflow := false
	for _, s := range snap {
		if s.Name == "custom_event_total" {
			customSeries++
			if s.Labels["overflow"] == "true" {
				hasOverflow = true
			}
		}
	}
	if customSeries > 6 {
		t.Fatalf("cardinality guard failed: expected <=6 series for custom_event_total, got %d", customSeries)
	}
	if !hasOverflow {
		t.Fatalf("expected overflow bucket to be created when cardinality limit reached")
	}
}

func TestTracerSpanTree(t *testing.T) {
	tracer := NewMemoryTracer()
	ctx := context.Background()

	// Root: Run
	runCtx, runSpan := tracer.StartSpan(ctx, "run:execute")
	runSpan.SetTag("run_id", "run-999")
	time.Sleep(5 * time.Millisecond)

	// Child: Turn 1
	turnCtx, turnSpan := tracer.StartSpan(runCtx, "turn:1")
	time.Sleep(5 * time.Millisecond)

	// Grandchild 1: Provider call
	pCtx, pSpan := tracer.StartSpan(turnCtx, "provider:complete")
	pSpan.SetTag("model", "deepseek-coder")
	time.Sleep(5 * time.Millisecond)
	pSpan.End()

	// Grandchild 2: Tool call
	tCtx, tSpan := tracer.StartSpan(turnCtx, "tool:file_read")
	tSpan.SetTag("file", "test.go")
	time.Sleep(5 * time.Millisecond)

	// Great-grandchild: Worker exec
	_, wSpan := tracer.StartSpan(tCtx, "worker:terminal")
	wSpan.RecordError(errors.New("connection failed"))
	wSpan.End()

	tSpan.End()
	turnSpan.End()
	runSpan.End()

	tc, ok := FromContext(pCtx)
	if !ok || tc.TraceID == "" {
		t.Fatalf("expected trace context to be propagated")
	}

	tree := tracer.GetTraceTree(tc.TraceID)
	if len(tree) != 1 {
		t.Fatalf("expected 1 root span, got %d", len(tree))
	}
	root := tree[0]
	if root.Name != "run:execute" {
		t.Fatalf("expected root span 'run:execute', got '%s'", root.Name)
	}
	if len(root.Children) != 1 || root.Children[0].Name != "turn:1" {
		t.Fatalf("expected child 'turn:1'")
	}
	turnChild := root.Children[0]
	if len(turnChild.Children) != 2 {
		t.Fatalf("expected 2 children under turn:1, got %d", len(turnChild.Children))
	}
}

// TestTracerEvictionAndMemoryBound 验证 Tracer 容量淘汰与 TTL 机制，杜绝内存持续泄露
func TestTracerEvictionAndMemoryBound(t *testing.T) {
	tracer := NewMemoryTracer()
	// 设置最大容量为 5 个 Trace
	tracer.SetCapacity(5)

	var traceIDs []string
	ctx := context.Background()

	// 连续创建 10 个 Trace
	for i := 0; i < 10; i++ {
		tCtx, span := tracer.StartSpan(ctx, fmt.Sprintf("trace-%d", i))
		tc, _ := FromContext(tCtx)
		traceIDs = append(traceIDs, tc.TraceID)
		span.End()
	}

	// 校验当前活跃 Trace 数严格不超过容量上限 5
	if active := tracer.ActiveTracesCount(); active != 5 {
		t.Fatalf("expected active traces strictly capped at 5, got %d", active)
	}

	// 校验前 5 个最老的 Trace 已被安全淘汰释放
	for i := 0; i < 5; i++ {
		tree := tracer.GetTraceTree(traceIDs[i])
		if tree != nil {
			t.Fatalf("old trace %s should have been evicted to prevent memory leak", traceIDs[i])
		}
	}

	// 校验后 5 个最新的 Trace 依然健在
	for i := 5; i < 10; i++ {
		tree := tracer.GetTraceTree(traceIDs[i])
		if tree == nil {
			t.Fatalf("recent trace %s should exist in memory", traceIDs[i])
		}
	}

	// 验证 TTL 清理
	expired := tracer.CleanExpired(0 * time.Millisecond) // 立即过期
	if expired != 5 || tracer.ActiveTracesCount() != 0 {
		t.Fatalf("expected all 5 traces to be cleaned by TTL, expired: %d, remaining: %d", expired, tracer.ActiveTracesCount())
	}
}

func TestStructuredLoggingAndRedaction(t *testing.T) {
	buf := &bytes.Buffer{}
	logger := NewLogger(buf, LevelDebug)

	fields := map[string]any{
		"run_id":        "run-123",
		"api_key":       "sk-secret1234567890abcdef",
		"Authorization": "Bearer sk-jwttokenxyz123456",
		"cookie":        "session_id=abc; other=def",
		"nested": map[string]any{
			"password":     "SuperSecretPass123",
			"normal_field": "ok_value",
		},
	}

	logger.Info(context.Background(), "Calling provider with key sk-abcdef12345678901234", fields)

	var entry StructuredLogEntry
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("failed to parse json log: %v", err)
	}

	if entry.Message != "Calling provider with key [REDACTED]" {
		t.Fatalf("message sensitive string not redacted, got: %s", entry.Message)
	}
	if entry.Fields["api_key"] != Redacted {
		t.Fatalf("api_key not redacted")
	}
	if entry.Fields["Authorization"] != Redacted {
		t.Fatalf("Authorization not redacted")
	}
	if entry.Fields["cookie"] != Redacted {
		t.Fatalf("cookie not redacted")
	}

	nested, ok := entry.Fields["nested"].(map[string]any)
	if !ok || nested["password"] != Redacted {
		t.Fatalf("nested password not redacted")
	}
	if nested["normal_field"] != "ok_value" {
		t.Fatalf("normal_field should remain unredacted")
	}
}

func TestHealthManager(t *testing.T) {
	hm := NewHealthManager()

	hm.RegisterLiveness("engine", func(ctx context.Context) ComponentHealth {
		return ComponentHealth{Status: StatusUp}
	})

	hm.RegisterReadiness("db", func(ctx context.Context) ComponentHealth {
		return ComponentHealth{
			Status:  StatusUp,
			Details: map[string]any{"latency_ms": 0.8},
		}
	})

	liveRep := hm.CheckLiveness(context.Background())
	if liveRep.Status != StatusUp {
		t.Fatalf("expected liveness UP, got %s", liveRep.Status)
	}

	readyRep := hm.CheckReadiness(context.Background())
	if readyRep.Status != StatusUp {
		t.Fatalf("expected readiness UP, got %s", readyRep.Status)
	}

	// 测试组件降级与失败
	hm.RegisterReadiness("mcp_server", func(ctx context.Context) ComponentHealth {
		return ComponentHealth{Status: StatusDown, Message: "mcp unreachable"}
	})

	readyRepAfterDown := hm.CheckReadiness(context.Background())
	if readyRepAfterDown.Status != StatusDown {
		t.Fatalf("expected readiness DOWN when component fails, got %s", readyRepAfterDown.Status)
	}
}
