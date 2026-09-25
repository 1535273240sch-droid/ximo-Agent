package supervisor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

// RingBuffer 用于高效在内存中保留最新 N 字节日志的环形缓冲区（防止内存膨胀）
type RingBuffer struct {
	mu   sync.Mutex
	buf  []byte
	size int
	full bool
	head int
}

func NewRingBuffer(size int) *RingBuffer {
	if size <= 0 {
		size = 64 * 1024 // 默认 64KB
	}
	return &RingBuffer{
		buf:  make([]byte, size),
		size: size,
	}
}

func (r *RingBuffer) Write(p []byte) (n int, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	n = len(p)
	for _, b := range p {
		r.buf[r.head] = b
		r.head = (r.head + 1) % r.size
		if r.head == 0 {
			r.full = true
		}
	}
	return n, nil
}

// Bytes 返回按时序排列的完整内容切片
func (r *RingBuffer) Bytes() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.full {
		out := make([]byte, r.head)
		copy(out, r.buf[:r.head])
		return out
	}

	out := make([]byte, r.size)
	copy(out, r.buf[r.head:])
	copy(out[r.size-r.head:], r.buf[:r.head])
	return out
}

// CrashReport 结构化崩溃转储报告
type CrashReport struct {
	ID            string    `json:"id"`
	ProcessID     int       `json:"process_id"`
	Role          string    `json:"role"`
	Kind          string    `json:"kind"`
	ExitCode      int       `json:"exit_code"`
	ExitTime      time.Time `json:"exit_time"`
	LastHeartbeat time.Time `json:"last_heartbeat"`
	StderrTail    string    `json:"stderr_tail"`
	OS            string    `json:"os"`
	Arch          string    `json:"arch"`
	NumCPU        int       `json:"num_cpu"`
}

// CrashCollector 崩溃采集器
type CrashCollector struct {
	mu      sync.Mutex
	dumpDir string
}

func NewCrashCollector(dumpDir string) *CrashCollector {
	if dumpDir == "" {
		dumpDir = filepath.Join(".", "data", "crashes")
	}
	_ = os.MkdirAll(dumpDir, 0755)
	return &CrashCollector{dumpDir: dumpDir}
}

// Collect 捕获崩溃并写入持久化存储
func (c *CrashCollector) Collect(role, kind string, pid int, exitCode int, lastHb time.Time, stderr []byte) (*CrashReport, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	reportID := fmt.Sprintf("crash-%s-%s-%d", role, time.Now().Format("20060102-150405.000"), pid)
	report := &CrashReport{
		ID:            reportID,
		ProcessID:     pid,
		Role:          role,
		Kind:          kind,
		ExitCode:      exitCode,
		ExitTime:      time.Now(),
		LastHeartbeat: lastHb,
		StderrTail:    string(stderr),
		OS:            runtime.GOOS,
		Arch:          runtime.GOARCH,
		NumCPU:        runtime.NumCPU(),
	}

	_ = os.MkdirAll(c.dumpDir, 0755)
	filePath := filepath.Join(c.dumpDir, fmt.Sprintf("%s.json", reportID))
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal crash report failed: %w", err)
	}

	if err := os.WriteFile(filePath, data, 0644); err != nil {
		return nil, fmt.Errorf("write crash dump file failed: %w", err)
	}

	return report, nil
}

// ListReports 列出所有持久化的崩溃报告
func (c *CrashCollector) ListReports() ([]*CrashReport, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	files, err := os.ReadDir(c.dumpDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var reports []*CrashReport
	for _, f := range files {
		if filepath.Ext(f.Name()) == ".json" {
			data, err := os.ReadFile(filepath.Join(c.dumpDir, f.Name()))
			if err != nil {
				continue
			}
			var rep CrashReport
			if err := json.Unmarshal(data, &rep); err == nil {
				reports = append(reports, &rep)
			}
		}
	}
	return reports, nil
}
