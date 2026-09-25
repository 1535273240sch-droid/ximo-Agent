package observability

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// 第27章指标名称常量定义
const (
	// Run 指标（聚合态低基数指标，禁止携带高基数 run_id 标签）
	MetricRunStartedTotal   = "run_started_total"
	MetricRunCompletedTotal = "run_completed_total"
	MetricRunFailedTotal    = "run_failed_total"
	MetricRunRecoveredTotal = "run_recovered_total"

	// Tool 指标
	MetricToolStartedTotal   = "tool_started_total"
	MetricToolCompletedTotal = "tool_completed_total"
	MetricToolFailedTotal    = "tool_failed_total"
	MetricToolTimeoutTotal   = "tool_timeout_total"

	// Provider 指标
	MetricProviderRequestTotal = "provider_request_total"
	MetricProviderRetryTotal   = "provider_retry_total"
	MetricProvider429Total     = "provider_429_total"
	MetricProviderLatencyMs    = "provider_latency_ms"

	// Worker 指标
	MetricWorkerRestartTotal = "worker_restart_total"
	MetricWorkerCrashTotal   = "worker_crash_total"

	// 队列与并发指标
	MetricQueueDepth   = "queue_depth"
	MetricQueueWaitMs  = "queue_wait_ms"
	MetricActiveRuns   = "active_runs"
	MetricActiveTools  = "active_tools"

	// 数据库指标
	MetricDBCommitLatencyMs = "db_commit_latency_ms"
	MetricDBBusyTotal       = "db_busy_total"

	// 运行时系统指标
	MetricMemoryBytes  = "memory_bytes"
	MetricGoroutines   = "goroutines"
	MetricBrowserCount = "browser_count"
)

// 禁止进入 Metrics 的高基数标签黑名单（这些属于 Tracing / Logging）
var highCardinalityLabelKeys = map[string]struct{}{
	"run_id":       {},
	"session_id":   {},
	"tool_call_id": {},
	"call_id":      {},
	"user_id":      {},
	"task_id":      {},
	"request_id":   {},
	"trace_id":     {},
	"span_id":      {},
}

const (
	// DefaultMaxSeriesPerMetric 单指标最大时间序列数（防维度爆炸）
	DefaultMaxSeriesPerMetric = 100
	// DefaultMaxTotalSeries 全局最大时间序列上限
	DefaultMaxTotalSeries = 2000
)

// Label 键值对元数据
type Label struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// L 创建 Label 的简便工具函数
func L(key, val string) Label {
	return Label{Key: key, Value: val}
}

// Metrics 标准指标上报接口（契约）
type Metrics interface {
	Inc(name string, labels ...Label)
	Observe(name string, value float64, labels ...Label)
	Gauge(name string, value float64, labels ...Label)
}

// MetricType 指标类型
type MetricType string

const (
	MetricTypeCounter   MetricType = "counter"
	MetricTypeGauge     MetricType = "gauge"
	MetricTypeHistogram MetricType = "histogram"
)

// MetricSnapshot 指标快照
type MetricSnapshot struct {
	Name        string             `json:"name"`
	Type        MetricType         `json:"type"`
	Labels      map[string]string  `json:"labels,omitempty"`
	Value       float64            `json:"value"`
	Count       uint64             `json:"count,omitempty"`
	Sum         float64            `json:"sum,omitempty"`
	Percentiles map[string]float64 `json:"percentiles,omitempty"`
	Timestamp   time.Time          `json:"timestamp"`
}

// MemoryMetrics 内存态指标收集器（具备高基数防护与有界容量控制）
type MemoryMetrics struct {
	mu                  sync.RWMutex
	maxSeriesPerMetric  int
	maxTotalSeries      int
	seriesCountByMetric map[string]int
	totalSeriesCount    int
	counters            map[string]*counterEntry
	gauges              map[string]*gaugeEntry
	histograms          map[string]*histogramEntry
}

type counterEntry struct {
	labels map[string]string
	value  atomic.Uint64
}

type gaugeEntry struct {
	labels map[string]string
	bits   atomic.Uint64
}

func (g *gaugeEntry) set(v float64) {
	g.bits.Store(math.Float64bits(v))
}

func (g *gaugeEntry) get() float64 {
	return math.Float64frombits(g.bits.Load())
}

type histogramEntry struct {
	mu     sync.Mutex
	labels map[string]string
	count  uint64
	sum    float64
	values []float64
}

func (h *histogramEntry) observe(v float64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.count++
	h.sum += v
	// 保留最新 2000 个采样用于分位数统计
	if len(h.values) >= 2000 {
		h.values = h.values[1:]
	}
	h.values = append(h.values, v)
}

func (h *histogramEntry) snapshot() (uint64, float64, map[string]float64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.values) == 0 {
		return h.count, h.sum, map[string]float64{
			"p50": 0, "p90": 0, "p95": 0, "p99": 0,
		}
	}
	sorted := make([]float64, len(h.values))
	copy(sorted, h.values)
	sort.Float64s(sorted)

	pct := func(p float64) float64 {
		idx := int(math.Ceil(p*float64(len(sorted)))) - 1
		if idx < 0 {
			idx = 0
		}
		if idx >= len(sorted) {
			idx = len(sorted) - 1
		}
		return sorted[idx]
	}

	return h.count, h.sum, map[string]float64{
		"p50": pct(0.50),
		"p90": pct(0.90),
		"p95": pct(0.95),
		"p99": pct(0.99),
	}
}

// NewMemoryMetrics 构造内存指标实例
func NewMemoryMetrics() *MemoryMetrics {
	return &MemoryMetrics{
		maxSeriesPerMetric:  DefaultMaxSeriesPerMetric,
		maxTotalSeries:      DefaultMaxTotalSeries,
		seriesCountByMetric: make(map[string]int),
		counters:            make(map[string]*counterEntry),
		gauges:              make(map[string]*gaugeEntry),
		histograms:          make(map[string]*histogramEntry),
	}
}

// SetLimits 动态设置基数上限（测试与调优使用）
func (m *MemoryMetrics) SetLimits(maxSeriesPerMetric, maxTotalSeries int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.maxSeriesPerMetric = maxSeriesPerMetric
	m.maxTotalSeries = maxTotalSeries
}

// sanitizeLabels 过滤高基数黑名单标签，防内存泄漏
func sanitizeLabels(labels []Label) []Label {
	if len(labels) == 0 {
		return nil
	}
	var clean []Label
	for _, l := range labels {
		k := strings.ToLower(strings.TrimSpace(l.Key))
		if _, isHighCard := highCardinalityLabelKeys[k]; isHighCard {
			// 过滤高基数标签，避免指标基数爆炸
			continue
		}
		clean = append(clean, l)
	}
	return clean
}

func (m *MemoryMetrics) formatSafeKey(name string, labels []Label) (string, map[string]string) {
	safeLabels := sanitizeLabels(labels)
	if len(safeLabels) == 0 {
		return name, nil
	}
	labelMap := make(map[string]string, len(safeLabels))
	keys := make([]string, 0, len(safeLabels))
	for _, l := range safeLabels {
		labelMap[l.Key] = l.Value
		keys = append(keys, l.Key)
	}
	sort.Strings(keys)
	var sb strings.Builder
	sb.WriteString(name)
	sb.WriteString("{")
	for i, k := range keys {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(k)
		sb.WriteString("=")
		sb.WriteString(labelMap[k])
	}
	sb.WriteString("}")

	formatted := sb.String()

	// 检查基数上限是否超出
	if m.seriesCountByMetric[name] >= m.maxSeriesPerMetric || m.totalSeriesCount >= m.maxTotalSeries {
		// 超出上限，归并至 overflow 桶
		overflowKey := name + "{overflow=\"true\"}"
		return overflowKey, map[string]string{"overflow": "true"}
	}

	return formatted, labelMap
}

// Inc 递增计数器
func (m *MemoryMetrics) Inc(name string, labels ...Label) {
	m.mu.Lock()
	key, labelMap := m.formatSafeKey(name, labels)
	c, ok := m.counters[key]
	if !ok {
		c = &counterEntry{labels: labelMap}
		m.counters[key] = c
		m.seriesCountByMetric[name]++
		m.totalSeriesCount++
	}
	m.mu.Unlock()

	c.value.Add(1)
}

// Observe 观测采样（耗时、延迟等）
func (m *MemoryMetrics) Observe(name string, value float64, labels ...Label) {
	m.mu.Lock()
	key, labelMap := m.formatSafeKey(name, labels)
	h, ok := m.histograms[key]
	if !ok {
		h = &histogramEntry{labels: labelMap, values: make([]float64, 0, 100)}
		m.histograms[key] = h
		m.seriesCountByMetric[name]++
		m.totalSeriesCount++
	}
	m.mu.Unlock()

	h.observe(value)
}

// Gauge 设置当前值
func (m *MemoryMetrics) Gauge(name string, value float64, labels ...Label) {
	m.mu.Lock()
	key, labelMap := m.formatSafeKey(name, labels)
	g, ok := m.gauges[key]
	if !ok {
		g = &gaugeEntry{labels: labelMap}
		m.gauges[key] = g
		m.seriesCountByMetric[name]++
		m.totalSeriesCount++
	}
	m.mu.Unlock()

	g.set(value)
}

// GetCounter 获取计数器当前值
func (m *MemoryMetrics) GetCounter(name string, labels ...Label) uint64 {
	m.mu.RLock()
	safeLabels := sanitizeLabels(labels)
	key, _ := formatKeyDirect(name, safeLabels)
	c, ok := m.counters[key]
	m.mu.RUnlock()
	if ok {
		return c.value.Load()
	}
	return 0
}

// GetGauge 获取仪表盘当前值
func (m *MemoryMetrics) GetGauge(name string, labels ...Label) float64 {
	m.mu.RLock()
	safeLabels := sanitizeLabels(labels)
	key, _ := formatKeyDirect(name, safeLabels)
	g, ok := m.gauges[key]
	m.mu.RUnlock()
	if ok {
		return g.get()
	}
	return 0
}

func formatKeyDirect(name string, labels []Label) (string, map[string]string) {
	if len(labels) == 0 {
		return name, nil
	}
	labelMap := make(map[string]string, len(labels))
	keys := make([]string, 0, len(labels))
	for _, l := range labels {
		labelMap[l.Key] = l.Value
		keys = append(keys, l.Key)
	}
	sort.Strings(keys)
	var sb strings.Builder
	sb.WriteString(name)
	sb.WriteString("{")
	for i, k := range keys {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(k)
		sb.WriteString("=")
		sb.WriteString(labelMap[k])
	}
	sb.WriteString("}")
	return sb.String(), labelMap
}

// SeriesCount 返回当前总时间序列数（用于指标基数测试）
func (m *MemoryMetrics) SeriesCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.totalSeriesCount
}

// Snapshot 获取全部指标快照
func (m *MemoryMetrics) Snapshot() []MetricSnapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()

	now := time.Now()
	var snapshots []MetricSnapshot

	for k, c := range m.counters {
		name := extractMetricName(k)
		snapshots = append(snapshots, MetricSnapshot{
			Name:      name,
			Type:      MetricTypeCounter,
			Labels:    c.labels,
			Value:     float64(c.value.Load()),
			Timestamp: now,
		})
	}

	for k, g := range m.gauges {
		name := extractMetricName(k)
		snapshots = append(snapshots, MetricSnapshot{
			Name:      name,
			Type:      MetricTypeGauge,
			Labels:    g.labels,
			Value:     g.get(),
			Timestamp: now,
		})
	}

	for k, h := range m.histograms {
		name := extractMetricName(k)
		cnt, sum, pcts := h.snapshot()
		snapshots = append(snapshots, MetricSnapshot{
			Name:        name,
			Type:        MetricTypeHistogram,
			Labels:      h.labels,
			Count:       cnt,
			Sum:         sum,
			Percentiles: pcts,
			Timestamp:   now,
		})
	}

	return snapshots
}

// Reset 清空指标
func (m *MemoryMetrics) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.counters = make(map[string]*counterEntry)
	m.gauges = make(map[string]*gaugeEntry)
	m.histograms = make(map[string]*histogramEntry)
	m.seriesCountByMetric = make(map[string]int)
	m.totalSeriesCount = 0
}

func extractMetricName(key string) string {
	idx := strings.Index(key, "{")
	if idx >= 0 {
		return key[:idx]
	}
	return key
}

// 全局默认单例
var (
	defaultMetrics Metrics = NewMemoryMetrics()
	defaultMu      sync.RWMutex
)

// Default 获取全局指标单例
func Default() Metrics {
	defaultMu.RLock()
	defer defaultMu.RUnlock()
	return defaultMetrics
}

// SetDefault 设置全局指标单例
func SetDefault(m Metrics) {
	defaultMu.Lock()
	defer defaultMu.Unlock()
	defaultMetrics = m
}

// 便捷全局上报函数，供各任务直接调用（低基数聚合指标，杜绝 run_id 爆炸）

// RunStarted 任务02调用：Run开始执行（低基数指标）
func RunStarted(labels ...Label) {
	Default().Inc(MetricRunStartedTotal, labels...)
}

// RunCompleted 任务02调用：Run成功结束
func RunCompleted(labels ...Label) {
	Default().Inc(MetricRunCompletedTotal, labels...)
}

// RunFailed 任务02调用：Run失败（只保留低基数 reason，如 timeout, rate_limit, internal_error）
func RunFailed(reason string) {
	Default().Inc(MetricRunFailedTotal, L("reason", reason))
}

// RunRecovered 任务02/01调用：Run成功崩溃恢复
func RunRecovered(labels ...Label) {
	Default().Inc(MetricRunRecoveredTotal, labels...)
}

// ToolStarted 任务04调用：工具调用开始
func ToolStarted(toolName string) {
	Default().Inc(MetricToolStartedTotal, L("tool", toolName))
}

// ToolCompleted 任务04调用：工具调用成功
func ToolCompleted(toolName string) {
	Default().Inc(MetricToolCompletedTotal, L("tool", toolName))
}

// ToolFailed 任务04调用：工具调用失败
func ToolFailed(toolName, reason string) {
	Default().Inc(MetricToolFailedTotal, L("tool", toolName), L("reason", reason))
}

// ToolTimeout 任务04调用：工具调用超时
func ToolTimeout(toolName string) {
	Default().Inc(MetricToolTimeoutTotal, L("tool", toolName))
}

// ProviderRequest 任务06调用：Provider发起请求
func ProviderRequest(provider, model string) {
	Default().Inc(MetricProviderRequestTotal, L("provider", provider), L("model", model))
}

// ProviderRetry 任务06调用：Provider重试
func ProviderRetry(provider, reason string) {
	Default().Inc(MetricProviderRetryTotal, L("provider", provider), L("reason", reason))
}

// Provider429 任务06调用：触发429限频
func Provider429(provider string) {
	Default().Inc(MetricProvider429Total, L("provider", provider))
}

// ProviderLatency 任务06调用：记录Provider耗时
func ProviderLatency(provider, model string, ms float64) {
	Default().Observe(MetricProviderLatencyMs, ms, L("provider", provider), L("model", model))
}

// WorkerRestart 任务05/01调用：Worker重启
func WorkerRestart(workerKind, workerID string) {
	Default().Inc(MetricWorkerRestartTotal, L("kind", workerKind), L("id", workerID))
}

// WorkerCrash 任务05/01调用：Worker崩溃
func WorkerCrash(workerKind, workerID string) {
	Default().Inc(MetricWorkerCrashTotal, L("kind", workerKind), L("id", workerID))
}

// SetQueueDepth 任务02调用：更新排队深度
func SetQueueDepth(depth int) {
	Default().Gauge(MetricQueueDepth, float64(depth))
}

// RecordQueueWait 任务02调用：排队等待耗时
func RecordQueueWait(ms float64) {
	Default().Observe(MetricQueueWaitMs, ms)
}

// SetActiveRuns 任务02调用：活跃Run数
func SetActiveRuns(count int) {
	Default().Gauge(MetricActiveRuns, float64(count))
}

// SetActiveTools 任务04调用：活跃Tool数
func SetActiveTools(count int) {
	Default().Gauge(MetricActiveTools, float64(count))
}

// DBCommitLatency 任务03调用：事务提交耗时
func DBCommitLatency(ms float64) {
	Default().Observe(MetricDBCommitLatencyMs, ms)
}

// DBBusy 任务03调用：触发SQLITE_BUSY
func DBBusy() {
	Default().Inc(MetricDBBusyTotal)
}

// UpdateRuntimeSystemMetrics 更新系统级别指标
func UpdateRuntimeSystemMetrics(memoryBytes uint64, goroutines int, browserCount int) {
	Default().Gauge(MetricMemoryBytes, float64(memoryBytes))
	Default().Gauge(MetricGoroutines, float64(goroutines))
	Default().Gauge(MetricBrowserCount, float64(browserCount))
}

// FormatPrometheus 将当前指标格式化为 Prometheus 文本格式
func (m *MemoryMetrics) FormatPrometheus() string {
	snapshots := m.Snapshot()
	var sb strings.Builder
	for _, s := range snapshots {
		var labelParts []string
		for k, v := range s.Labels {
			labelParts = append(labelParts, fmt.Sprintf("%s=\"%s\"", k, v))
		}
		sort.Strings(labelParts)
		labelStr := ""
		if len(labelParts) > 0 {
			labelStr = "{" + strings.Join(labelParts, ",") + "}"
		}

		switch s.Type {
		case MetricTypeCounter, MetricTypeGauge:
			sb.WriteString(fmt.Sprintf("%s%s %v\n", s.Name, labelStr, s.Value))
		case MetricTypeHistogram:
			sb.WriteString(fmt.Sprintf("%s_count%s %d\n", s.Name, labelStr, s.Count))
			sb.WriteString(fmt.Sprintf("%s_sum%s %v\n", s.Name, labelStr, s.Sum))
			for pctKey, pctVal := range s.Percentiles {
				pctLabel := ""
				if len(labelParts) > 0 {
					pctLabel = "{" + strings.Join(labelParts, ",") + fmt.Sprintf(",quantile=\"%s\"}", pctKey)
				} else {
					pctLabel = fmt.Sprintf("{quantile=\"%s\"}", pctKey)
				}
				sb.WriteString(fmt.Sprintf("%s%s %v\n", s.Name, pctLabel, pctVal))
			}
		}
	}
	return sb.String()
}
