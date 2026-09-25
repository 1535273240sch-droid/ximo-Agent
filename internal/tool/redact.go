package tool

import (
	"encoding/json"
	"regexp"
)

// BasicRedactor 是不依赖 secrets 包的内置兜底脱敏器。
//
// 为什么需要它：审核报告 B-1 指出，未注入 Redactor 时旧的 nopRedactor 会
// **静默放行**所有秘密——这正是 I12 要防的最坏情形（08 号拼装时忘注入，
// 程序照常运行、秘密全部明文落进 event log，且没有任何报错）。
//
// 现在 nil Redactor 一律落到本实现：它仍然执行
//  1. 高置信度秘密格式过滤（sk-…/AKIA…/AIza…/PEM 私钥等）；
//  2. 敏感键名整体脱敏（api_key/token/password/credential/…）。
//
// 也就是说“没注入”最坏也只是退化为“只做内置规则过滤”，绝不等于“不过滤”。
// 生产环境仍应注入 secrets.Manager.Redactor()（它还维护已知秘密值登记表，
// 能过滤未被格式规则覆盖的自定义密钥）。
type BasicRedactor struct {
	patterns []*regexp.Regexp
}

// 编译期检查。
var _ SecretRedactor = (*BasicRedactor)(nil)

// basicSecretPatterns 与 secrets 包的默认模式保持一致（保守集合）。
var basicSecretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`sk-[A-Za-z0-9_-]{20,}`),
	regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{20,}`),
	regexp.MustCompile(`AIza[0-9A-Za-z_-]{35}`),
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{16,}`),
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{22,}`),
	regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{10,}`),
	regexp.MustCompile(`glpat-[A-Za-z0-9_-]{20,}`),
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),
}

// basicSensitiveKeyPattern 与 secrets 包的敏感键名模式一致。
var basicSensitiveKeyPattern = regexp.MustCompile(`(?i)^.*(api[-_]?key|secret|token|password|passwd|credential|private[-_]?key|access[-_]?key|auth|session[-_]?key|cookie|bearer).*$`)

// NewBasicRedactor 创建内置兜底脱敏器。
func NewBasicRedactor() *BasicRedactor {
	return &BasicRedactor{patterns: append([]*regexp.Regexp(nil), basicSecretPatterns...)}
}

// RedactString 实现 SecretRedactor。
func (r *BasicRedactor) RedactString(s string) string {
	if s == "" {
		return s
	}
	for _, pattern := range r.patterns {
		s = pattern.ReplaceAllString(s, redactedPlaceholder)
	}
	return s
}

// RedactJSON 实现 SecretRedactor。
func (r *BasicRedactor) RedactJSON(payload []byte) ([]byte, error) {
	if len(payload) == 0 {
		return payload, nil
	}
	var value any
	if err := json.Unmarshal(payload, &value); err != nil {
		// 非法 JSON：退化为字符串级过滤，绝不原样放行。
		return []byte(r.RedactString(string(payload))), nil
	}
	return json.Marshal(r.RedactValue(value))
}

// RedactValue 实现 SecretRedactor。struct 等复合类型经 JSON 往返转成
// map/slice 后脱敏——审计与事件 payload 最终都会序列化为 JSON，语义等价。
func (r *BasicRedactor) RedactValue(v any) any {
	switch val := v.(type) {
	case nil:
		return nil
	case string:
		return r.RedactString(val)
	case []byte:
		return []byte(r.RedactString(string(val)))
	case bool, float32, float64, int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64, uintptr:
		// 标量无需脱敏；必须显式放行，否则会掉进下面的 JSON 往返分支，
		// 而数字 marshal→unmarshal 仍是数字，形成无限递归。
		return val
	case []any:
		out := make([]any, len(val))
		for i, item := range val {
			out[i] = r.RedactValue(item)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(val))
		for k, item := range val {
			if basicSensitiveKeyPattern.MatchString(k) {
				out[k] = redactedPlaceholder
				continue
			}
			out[k] = r.RedactValue(item)
		}
		return out
	default:
		data, err := json.Marshal(v)
		if err != nil {
			return v
		}
		var generic any
		if err := json.Unmarshal(data, &generic); err != nil {
			return v
		}
		// 往返结果只可能是容器/字符串/标量；前两类继续走上面的分支，
		// 标量在此直接返回，保证递归收敛。
		switch generic.(type) {
		case map[string]any, []any, string:
			return r.RedactValue(generic)
		default:
			return generic
		}
	}
}

// ContainsSecret 实现 SecretRedactor（内置规则只能检测格式命中）。
func (r *BasicRedactor) ContainsSecret(s string) bool {
	for _, pattern := range r.patterns {
		if pattern.MatchString(s) {
			return true
		}
	}
	return false
}

// redactorOrDefault 保证任何路径都拿得到一个“会过滤”的脱敏器。
// 这是 B-1 修复的核心：nil 不再意味着不过滤。
func redactorOrDefault(r SecretRedactor) SecretRedactor {
	if r == nil {
		return NewBasicRedactor()
	}
	return r
}

// redactedPlaceholder 脱敏占位符（与 secrets 包保持一致）。
const redactedPlaceholder = "[REDACTED]"
