package observability

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// HealthStatus 健康状态枚举
type HealthStatus string

const (
	StatusUp       HealthStatus = "UP"
	StatusDown     HealthStatus = "DOWN"
	StatusDegraded HealthStatus = "DEGRADED"
)

// ComponentHealth 单个组件健康状况
type ComponentHealth struct {
	Status    HealthStatus   `json:"status"`
	Message   string         `json:"message,omitempty"`
	Details   map[string]any `json:"details,omitempty"`
	CheckedAt time.Time      `json:"checked_at"`
}

// HealthReport 汇总健康检查报告
type HealthReport struct {
	Status     HealthStatus               `json:"status"`
	Timestamp  time.Time                  `json:"timestamp"`
	Components map[string]ComponentHealth `json:"components"`
}

// HealthChecker 组件健康检查函数定义
type HealthChecker func(ctx context.Context) ComponentHealth

// HealthManager 健康监测管理器
type HealthManager struct {
	mu        sync.RWMutex
	liveness  map[string]HealthChecker
	readiness map[string]HealthChecker
}

// NewHealthManager 实例化健康管理器
func NewHealthManager() *HealthManager {
	return &HealthManager{
		liveness:  make(map[string]HealthChecker),
		readiness: make(map[string]HealthChecker),
	}
}

// RegisterLiveness 注册存活检查项（进程是否挂掉/死锁）
func (h *HealthManager) RegisterLiveness(name string, checker HealthChecker) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.liveness[name] = checker
}

// RegisterReadiness 注册就绪检查项（是否能够接受并处理请求）
func (h *HealthManager) RegisterReadiness(name string, checker HealthChecker) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.readiness[name] = checker
}

// CheckLiveness 执行存活检查
func (h *HealthManager) CheckLiveness(ctx context.Context) HealthReport {
	h.mu.RLock()
	checkers := copyCheckers(h.liveness)
	h.mu.RUnlock()

	return runChecks(ctx, checkers)
}

// CheckReadiness 执行就绪检查
func (h *HealthManager) CheckReadiness(ctx context.Context) HealthReport {
	h.mu.RLock()
	checkers := copyCheckers(h.readiness)
	h.mu.RUnlock()

	return runChecks(ctx, checkers)
}

func copyCheckers(src map[string]HealthChecker) map[string]HealthChecker {
	dst := make(map[string]HealthChecker, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func runChecks(ctx context.Context, checkers map[string]HealthChecker) HealthReport {
	report := HealthReport{
		Status:     StatusUp,
		Timestamp:  time.Now(),
		Components: make(map[string]ComponentHealth, len(checkers)),
	}

	var mu sync.Mutex
	var wg sync.WaitGroup

	for name, fn := range checkers {
		wg.Add(1)
		go func(compName string, checker HealthChecker) {
			defer wg.Done()

			checkCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()

			res := checker(checkCtx)
			if res.CheckedAt.IsZero() {
				res.CheckedAt = time.Now()
			}

			mu.Lock()
			report.Components[compName] = res
			if res.Status == StatusDown {
				report.Status = StatusDown
			} else if res.Status == StatusDegraded && report.Status != StatusDown {
				report.Status = StatusDegraded
			}
			mu.Unlock()
		}(name, fn)
	}

	wg.Wait()
	return report
}

// LivenessHandler HTTP 存活探针 Handler
func (h *HealthManager) LivenessHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		report := h.CheckLiveness(r.Context())
		writeJSONResponse(w, report)
	}
}

// ReadinessHandler HTTP 就绪探针 Handler
func (h *HealthManager) ReadinessHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		report := h.CheckReadiness(r.Context())
		writeJSONResponse(w, report)
	}
}

func writeJSONResponse(w http.ResponseWriter, report HealthReport) {
	w.Header().Set("Content-Type", "application/json")
	if report.Status == StatusDown {
		w.WriteHeader(http.StatusServiceUnavailable)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	_ = json.NewEncoder(w).Encode(report)
}

// 全局默认健康管理器
var (
	defaultHealthManager = NewHealthManager()
)

// DefaultHealthManager 全局默认健康检查器
func DefaultHealthManager() *HealthManager {
	return defaultHealthManager
}
