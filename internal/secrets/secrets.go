// Package secrets 提供统一的密钥管理：API Key 等秘密只以 secret_ref 形式
// 存在于数据库/配置/事件中，明文值存放在平台安全存储里
// （Windows Credential Manager/DPAPI、macOS Keychain、Linux Secret Service）。
//
// 安全红线（第21章）：
//   - API Key 绝不进入源码、默认配置、Git、日志、crash dump、SQLite 明文、
//     IPC debug 日志；
//   - 数据库只存 secret_ref，不存明文 api_key；
//   - API key 不得进入前端 state，也不得进入普通 event payload——本包的
//     Redactor 在 Audit/Event 序列化处做兜底过滤（I12）。
package secrets

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
)

// SecretsProvider 是全局消费的统一秘密接口（第32章契约）。
type SecretsProvider interface {
	// Get 按 ref 取回明文值。
	Get(ref string) (value string, err error)
	// Put 存入明文值，返回 ref（同一值得到同一 ref，幂等）。
	Put(value string) (ref string, err error)
}

// Backend 是平台安全存储的抽象。
type Backend interface {
	// Get 按 ref 读取明文值。
	Get(ref string) (string, error)
	// Put 写入 ref -> value。
	Put(ref, value string) error
	// Delete 删除 ref。
	Delete(ref string) error
	// Available 报告后端在当前环境是否可用。
	Available() bool
	// Name 后端名称（诊断用）。
	Name() string
}

// ErrSecretNotFound 秘密不存在。
var ErrSecretNotFound = errors.New("secrets: 秘密不存在")

// ErrBackendUnavailable 平台安全存储不可用。
var ErrBackendUnavailable = errors.New("secrets: 平台安全存储不可用")

// refPrefix 是 secret_ref 的格式前缀。
const refPrefix = "secretref:v1:"

// RefForValue 由明文值确定性地派生 ref（sha256 前 128bit）。
// ref 是值的单向摘要，无法反推明文；同一值永远得到同一 ref（幂等）。
func RefForValue(value string) string {
	sum := sha256.Sum256([]byte(value))
	return refPrefix + hex.EncodeToString(sum[:16])
}

// IsRef 报告字符串是否是合法 secret_ref。
func IsRef(s string) bool {
	if !strings.HasPrefix(s, refPrefix) {
		return false
	}
	body := strings.TrimPrefix(s, refPrefix)
	if len(body) != 32 {
		return false
	}
	for _, c := range body {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// Manager 是 SecretsProvider 的实现：平台后端 + 秘密登记（供脱敏过滤）。
type Manager struct {
	backend  Backend
	redactor *Redactor
}

// NewManager 创建秘密管理器。backend 为 nil 时返回错误（绝不退化为明文存储）。
func NewManager(backend Backend) (*Manager, error) {
	if backend == nil {
		return nil, fmt.Errorf("secrets: backend 不能为 nil（绝不退化为明文存储）")
	}
	return &Manager{backend: backend, redactor: NewRedactor()}, nil
}

// NewDefaultManager 使用当前平台的默认安全存储创建管理器。
func NewDefaultManager() (*Manager, error) {
	return NewManager(DefaultBackend())
}

// Get 实现 SecretsProvider。读取成功后把值登记进 Redactor，
// 使后续事件/日志序列化能过滤它（I12）。
func (m *Manager) Get(ref string) (string, error) {
	if !IsRef(ref) {
		return "", fmt.Errorf("secrets: 非法 secret_ref %q", ref)
	}
	value, err := m.backend.Get(ref)
	if err != nil {
		if errors.Is(err, ErrSecretNotFound) {
			return "", err
		}
		return "", fmt.Errorf("secrets: 读取 %s 失败: %w", ref, err)
	}
	m.redactor.Track(ref, value)
	return value, nil
}

// Put 实现 SecretsProvider。
func (m *Manager) Put(value string) (string, error) {
	if value == "" {
		return "", fmt.Errorf("secrets: 拒绝存入空值")
	}
	ref := RefForValue(value)
	if err := m.backend.Put(ref, value); err != nil {
		return "", fmt.Errorf("secrets: 写入 %s 失败: %w", ref, err)
	}
	m.redactor.Track(ref, value)
	return ref, nil
}

// MigratePlaintext 把 v1 遗留的明文 key 升级到安全存储并返回 ref。
// 迁移语义：先尝试 Put；若后端已存在该 ref 则直接返回（幂等），
// 调用方应随后删除原明文存储。
func (m *Manager) MigratePlaintext(value string) (ref string, err error) {
	if value == "" {
		return "", fmt.Errorf("secrets: 拒绝迁移空值")
	}
	return m.Put(value)
}

// WithSecret 以闭包形式使用秘密：明文只在闭包内可见，不逃逸到调用方栈
// （审核报告 S-2：Get 把明文交出去后，调用方一次 log.Debug 就会泄露）。
// 闭包返回的错误原样透传；闭包 panic 同样传播（不吞）。
//
// 推荐用法：
//
//	err := secrets.WithSecret(ref, func(key string) error {
//	    req.Header.Set("Authorization", "Bearer "+key)
//	    return doRequest(req)
//	})
func (m *Manager) WithSecret(ref string, fn func(plaintext string) error) error {
	value, err := m.Get(ref)
	if err != nil {
		return err
	}
	return fn(value)
}

// Delete 删除秘密。
func (m *Manager) Delete(ref string) error {
	if err := m.backend.Delete(ref); err != nil && !errors.Is(err, ErrSecretNotFound) {
		return err
	}
	m.redactor.Untrack(ref)
	return nil
}

// Redactor 返回管理器持有的脱敏器（供审计/事件序列化接线）。
func (m *Manager) Redactor() *Redactor { return m.redactor }

// BackendName 返回后端名称（诊断）。
func (m *Manager) BackendName() string { return m.backend.Name() }

// Available 报告平台后端是否可用。
func (m *Manager) Available() bool { return m.backend.Available() }

// ---------------------------------------------------------------------------
// Redactor：I12 兜底过滤
// ---------------------------------------------------------------------------

// redactedPlaceholder 脱敏后的占位符。
const redactedPlaceholder = "[REDACTED]"

// defaultSecretPatterns 高置信度的秘密格式（保守集合，避免误伤普通文本）。
var defaultSecretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`sk-[A-Za-z0-9_-]{20,}`),              // OpenAI 风格
	regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{20,}`),          // Anthropic 风格
	regexp.MustCompile(`AIza[0-9A-Za-z_-]{35}`),              // Google API Key
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),                   // AWS Access Key ID
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{16,}`),         // GitHub token
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{22,}`),       // GitHub fine-grained PAT
	regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{10,}`),       // Slack token
	regexp.MustCompile(`glpat-[A-Za-z0-9_-]{20,}`),           // GitLab PAT
	regexp.MustCompile(`shpat_[a-fA-F0-9]{32}`),              // Shopify
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`), // PEM 私钥
}

// sensitiveKeyPattern 键名匹配：这些键的值一律脱敏（无论内容）。
var sensitiveKeyPattern = regexp.MustCompile(`(?i)^.*(api[-_]?key|secret|token|password|passwd|credential|private[-_]?key|access[-_]?key|auth|session[-_]?key|cookie|bearer).*$`)

// Redactor 在事件/日志序列化前过滤秘密。双层策略：
//  1. 值匹配：进程内 Put/Get 过的秘密值，原样替换为占位符；
//  2. 模式匹配：高置信度秘密格式（sk-.../AKIA.../PEM 等）与敏感键名的值。
type Redactor struct {
	mu       sync.RWMutex
	byValue  map[string]string // value -> ref
	patterns []*regexp.Regexp
}

// NewRedactor 创建脱敏器。
func NewRedactor() *Redactor {
	return &Redactor{
		byValue:  make(map[string]string),
		patterns: append([]*regexp.Regexp(nil), defaultSecretPatterns...),
	}
}

// Track 登记一个已知秘密（值 -> ref）。
func (r *Redactor) Track(ref, value string) {
	if value == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byValue[value] = ref
}

// Untrack 取消登记（秘密被删除后）。
func (r *Redactor) Untrack(ref string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for value, r2 := range r.byValue {
		if r2 == ref {
			delete(r.byValue, value)
		}
	}
}

// TrackedCount 返回已登记的秘密数量（测试/诊断）。
func (r *Redactor) TrackedCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.byValue)
}

// ContainsSecret 报告字符串中是否包含任一已知秘密值。
func (r *Redactor) ContainsSecret(s string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for value := range r.byValue {
		if strings.Contains(s, value) {
			return true
		}
	}
	return false
}

// RedactString 替换字符串中的秘密值和高置信度模式。
func (r *Redactor) RedactString(s string) string {
	if s == "" {
		return s
	}
	r.mu.RLock()
	for value, ref := range r.byValue {
		if strings.Contains(s, value) {
			s = strings.ReplaceAll(s, value, redactedPlaceholder+"("+ref+")")
		}
	}
	patterns := r.patterns
	r.mu.RUnlock()

	for _, pattern := range patterns {
		s = pattern.ReplaceAllString(s, redactedPlaceholder)
	}
	return s
}

// RedactJSON 对 JSON payload 做深度脱敏后重新序列化。
// 解析失败时退化为字符串级脱敏（绝不原样放行）。
func (r *Redactor) RedactJSON(payload []byte) ([]byte, error) {
	if len(payload) == 0 {
		return payload, nil
	}
	var value any
	if err := json.Unmarshal(payload, &value); err != nil {
		return []byte(r.RedactString(string(payload))), nil
	}
	redacted := r.RedactValue(value)
	return json.Marshal(redacted)
}

// RedactValue 对任意 Go 值做深度脱敏：
//   - map：键名命中敏感模式时值整体替换；否则递归处理值；
//   - slice：逐元素递归；
//   - string：字符串级脱敏；
//   - struct：通过反射处理导出字符串字段。
func (r *Redactor) RedactValue(v any) any {
	switch val := v.(type) {
	case nil:
		return nil
	case string:
		return r.RedactString(val)
	case []any:
		out := make([]any, len(val))
		for i, item := range val {
			out[i] = r.RedactValue(item)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(val))
		for k, item := range val {
			if sensitiveKeyPattern.MatchString(k) {
				out[k] = redactedPlaceholder
				continue
			}
			out[k] = r.RedactValue(item)
		}
		return out
	case bool, float64, int, int64, uint64:
		return val
	default:
		return r.redactReflect(val)
	}
}
