package observability

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// LogLevel 日志级别
type LogLevel int

const (
	LevelDebug LogLevel = iota
	LevelInfo
	LevelWarn
	LevelError
)

func (l LogLevel) String() string {
	switch l {
	case LevelDebug:
		return "DEBUG"
	case LevelInfo:
		return "INFO"
	case LevelWarn:
		return "WARN"
	case LevelError:
		return "ERROR"
	default:
		return "UNKNOWN"
	}
}

// ParseLogLevel 解析日志级别
func ParseLogLevel(str string) LogLevel {
	switch strings.ToUpper(strings.TrimSpace(str)) {
	case "DEBUG":
		return LevelDebug
	case "INFO":
		return LevelInfo
	case "WARN", "WARNING":
		return LevelWarn
	case "ERROR":
		return LevelError
	default:
		return LevelInfo
	}
}

// 敏感键名列表（大小写不敏感匹配）
var sensitiveKeys = map[string]struct{}{
	"api_key":       {},
	"apikey":        {},
	"authorization": {},
	"cookie":        {},
	"cookies":       {},
	"token":         {},
	"secret":        {},
	"secret_key":    {},
	"password":      {},
	"passwd":        {},
	"bearer":        {},
	"access_token":  {},
	"refresh_token": {},
	"private_key":   {},
	"credential":    {},
	"credentials":   {},
}

// 敏感模式正则匹配（例如 sk-..., Bearer ...）
var (
	bearerRegex = regexp.MustCompile(`(?i)Bearer\s+([A-Za-z0-9\-_.]+)`)
	keyRegex    = regexp.MustCompile(`(?i)(sk-[a-zA-Z0-9]{16,})`)
)

const Redacted = "[REDACTED]"

// SanitizeKey 判断字段名是否属于敏感字段
func SanitizeKey(key string) bool {
	normalized := strings.ToLower(strings.TrimSpace(key))
	normalized = strings.ReplaceAll(normalized, "-", "_")
	_, exists := sensitiveKeys[normalized]
	return exists
}

// SanitizeValue 对单个值进行递归或格式脱敏
func SanitizeValue(val any) any {
	if val == nil {
		return nil
	}
	switch v := val.(type) {
	case string:
		return sanitizeString(v)
	case map[string]any:
		return SanitizeMap(v)
	case []any:
		out := make([]any, len(v))
		for i, elem := range v {
			out[i] = SanitizeValue(elem)
		}
		return out
	case []string:
		out := make([]string, len(v))
		for i, elem := range v {
			out[i] = sanitizeString(elem)
		}
		return out
	default:
		return val
	}
}

// SanitizeMap 对 Map 中的敏感字段和内容进行全面脱敏
func SanitizeMap(input map[string]any) map[string]any {
	if input == nil {
		return nil
	}
	result := make(map[string]any, len(input))
	for k, v := range input {
		if SanitizeKey(k) {
			result[k] = Redacted
		} else {
			result[k] = SanitizeValue(v)
		}
	}
	return result
}

func sanitizeString(s string) string {
	res := bearerRegex.ReplaceAllString(s, "Bearer "+Redacted)
	res = keyRegex.ReplaceAllString(res, Redacted)
	return res
}

// StructuredLogEntry JSON 日志条目
type StructuredLogEntry struct {
	Timestamp time.Time      `json:"timestamp"`
	Level     string         `json:"level"`
	Message   string         `json:"message"`
	TraceID   string         `json:"trace_id,omitempty"`
	SpanID    string         `json:"span_id,omitempty"`
	RunID     string         `json:"run_id,omitempty"`
	Fields    map[string]any `json:"fields,omitempty"`
}

// Logger 结构化日志器
type Logger struct {
	mu     sync.Mutex
	writer io.Writer
	level  LogLevel
}

// NewLogger 创建日志器
func NewLogger(w io.Writer, lvl LogLevel) *Logger {
	if w == nil {
		w = os.Stdout
	}
	return &Logger{
		writer: w,
		level:  lvl,
	}
}

// SetLevel 动态修改级别
func (l *Logger) SetLevel(lvl LogLevel) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.level = lvl
}

// SetOutput 修改输出流
func (l *Logger) SetOutput(w io.Writer) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.writer = w
}

func (l *Logger) log(ctx context.Context, lvl LogLevel, msg string, fields map[string]any) {
	l.mu.Lock()
	if lvl < l.level {
		l.mu.Unlock()
		return
	}
	w := l.writer
	l.mu.Unlock()

	var traceID, spanID, runID string
	if ctx != nil {
		if tc, ok := FromContext(ctx); ok {
			traceID = tc.TraceID
			spanID = tc.SpanID
		}
		if r, ok := ctx.Value("run_id").(string); ok {
			runID = r
		}
	}

	sanitizedFields := SanitizeMap(fields)

	entry := StructuredLogEntry{
		Timestamp: time.Now(),
		Level:     lvl.String(),
		Message:   sanitizeString(msg),
		TraceID:   traceID,
		SpanID:    spanID,
		RunID:     runID,
		Fields:    sanitizedFields,
	}

	data, err := json.Marshal(entry)
	if err != nil {
		// 备用兜底
		data = []byte(fmt.Sprintf(`{"timestamp":%q,"level":"ERROR","message":"failed to marshal log entry"}`+"\n", time.Now().Format(time.RFC3339)))
	} else {
		data = append(data, '\n')
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = w.Write(data)
}

func (l *Logger) Debug(ctx context.Context, msg string, fields ...map[string]any) {
	l.log(ctx, LevelDebug, msg, mergeFields(fields...))
}

func (l *Logger) Info(ctx context.Context, msg string, fields ...map[string]any) {
	l.log(ctx, LevelInfo, msg, mergeFields(fields...))
}

func (l *Logger) Warn(ctx context.Context, msg string, fields ...map[string]any) {
	l.log(ctx, LevelWarn, msg, mergeFields(fields...))
}

func (l *Logger) Error(ctx context.Context, msg string, fields ...map[string]any) {
	l.log(ctx, LevelError, msg, mergeFields(fields...))
}

func mergeFields(fieldMaps ...map[string]any) map[string]any {
	if len(fieldMaps) == 0 {
		return nil
	}
	res := make(map[string]any)
	for _, m := range fieldMaps {
		for k, v := range m {
			res[k] = v
		}
	}
	return res
}

// 全局默认日志实例
var (
	defaultLogger   = NewLogger(os.Stdout, LevelInfo)
	defaultLoggerMu sync.RWMutex
)

// DefaultLogger 获取全局 Logger
func DefaultLogger() *Logger {
	defaultLoggerMu.RLock()
	defer defaultLoggerMu.RUnlock()
	return defaultLogger
}

// SetDefaultLogger 设置全局 Logger
func SetDefaultLogger(l *Logger) {
	defaultLoggerMu.Lock()
	defer defaultLoggerMu.Unlock()
	defaultLogger = l
}

// 便捷全局日志调用函数
func LogDebug(ctx context.Context, msg string, fields ...map[string]any) {
	DefaultLogger().Debug(ctx, msg, fields...)
}

func LogInfo(ctx context.Context, msg string, fields ...map[string]any) {
	DefaultLogger().Info(ctx, msg, fields...)
}

func LogWarn(ctx context.Context, msg string, fields ...map[string]any) {
	DefaultLogger().Warn(ctx, msg, fields...)
}

func LogError(ctx context.Context, msg string, fields ...map[string]any) {
	DefaultLogger().Error(ctx, msg, fields...)
}
