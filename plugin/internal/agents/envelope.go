package agents

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// Envelope 是主仓库 ipcapi 定义的业务载荷外壳：{ok, error, data}。它与
// "传输失败" 在协议层可区分——业务失败是 ok=false 的正常响应，传输失败是
// 帧读写错误。
//
// 注意：只有 config.get/set、secret.put、model.list 这类业务帧用信封；
// system.error 帧的载荷是裸文本（见 TypeError）。
type Envelope struct {
	OK    bool            `json:"ok"`
	Error string          `json:"error,omitempty"`
	Data  json.RawMessage `json:"data,omitempty"`
}

// Decode 从信封里解出业务数据。
func (e *Envelope) Decode(dst any) error {
	if e == nil {
		return fmt.Errorf("ipc: empty envelope")
	}
	if !e.OK {
		if e.Error == "" {
			return fmt.Errorf("ipc: request failed")
		}
		return fmt.Errorf("%s", e.Error)
	}
	if len(e.Data) == 0 || dst == nil {
		return nil
	}
	return json.Unmarshal(e.Data, dst)
}

// parseEnvelope 解析响应帧的载荷。
func parseEnvelope(f *Frame) (*Envelope, error) {
	if f == nil {
		return nil, fmt.Errorf("ipc: nil frame")
	}
	if f.Header.Type == TypeError {
		return nil, fmt.Errorf("%s", redact(string(f.Payload)))
	}
	if len(f.Payload) == 0 {
		return &Envelope{OK: true}, nil
	}
	var env Envelope
	if err := json.Unmarshal(f.Payload, &env); err != nil {
		return nil, fmt.Errorf("ipc: decode envelope: %w", err)
	}
	// 即便服务端忘了脱敏，这里再过一遍：错误文本最终可能被打进终端或日志。
	env.Error = redact(env.Error)
	return &env, nil
}

// ---------------------------------------------------------------------------
// 脱敏
// ---------------------------------------------------------------------------

// secretPatterns 覆盖主仓库 internal/types.RedactString 的前缀集合，并额外
// 补上冒号形态（token: / api_key: 等）——主仓库只列了等号形态，而日志里
// "token: xxx" 一样常见。这里宁可多抹一点噪声，也不放过一个凭据。
var secretPatterns = []string{
	"sk-", "sk_", "api_key=", "api-key=", "apikey=", "api_key:", "api-key:", "apikey:",
	"access_token=", "refresh_token=", "token=", "token:", "password=", "password:", "secret=", "secret:",
	"authorization:", "authorization=", "bearer ", "basic ",
}

// authSchemes 是可能夹在认证头与凭据之间的词。
var authSchemes = []string{"bearer", "basic", "token", "digest"}

func isSecretDelimiter(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', '"', '\'', ',', ';', '}', ')', ']', '&', '?':
		return true
	default:
		return false
	}
}

func skipSeparators(s string, i int) int {
	for i < len(s) {
		switch s[i] {
		case ' ', '\t', ':', '=', '"', '\'':
			i++
			continue
		}
		return i
	}
	return i
}

func skipAuthScheme(s string, i int) int {
	lower := strings.ToLower(s[i:])
	for _, scheme := range authSchemes {
		if strings.HasPrefix(lower, scheme) {
			end := i + len(scheme)
			// 只有后面跟分隔符才算认证方案词，避免把 "tokenizer" 吃掉。
			if end >= len(s) || isSecretDelimiter(s[end]) {
				return end
			}
		}
	}
	return i
}

// knownSecrets 记录本次会话见到过的凭据明文，用于「无论它出现在哪都抹掉」。
// 这是 pattern 匹配之外的第二道闸：用户给的 key 可能不带 sk- 前缀。
var (
	knownSecretsMu sync.RWMutex
	knownSecrets   = map[string]struct{}{}
)

// RegisterSecret 登记一个凭据明文，之后任何经 redact 的文本都不会包含它。
// 明文本身不出进程、不进日志，只留在这个包的内存里。
func RegisterSecret(value string) {
	if len(value) < 6 {
		// 太短的值（例如 "test"）满屏误命中，登记了反而制造噪音。
		return
	}
	knownSecretsMu.Lock()
	knownSecrets[value] = struct{}{}
	knownSecretsMu.Unlock()
}

// redact 抹掉文本里的凭据。任何要输出到终端、日志或错误里的文本都必须过它。
func redact(s string) string {
	if s == "" {
		return s
	}
	knownSecretsMu.RLock()
	for v := range knownSecrets {
		if strings.Contains(s, v) {
			s = strings.ReplaceAll(s, v, "***")
		}
	}
	knownSecretsMu.RUnlock()

	lower := strings.ToLower(s)
	var out strings.Builder
	for i := 0; i < len(s); {
		// 从当前位置起找最早出现的模式；命中位置永远严格大于被扫描的起点，
		// 因此游标单调前进，不存在死循环。
		hit, hitPat := -1, ""
		for _, p := range secretPatterns {
			idx := strings.Index(lower[i:], p)
			if idx < 0 {
				continue
			}
			if abs := i + idx; hit < 0 || abs < hit {
				hit, hitPat = abs, p
			}
		}
		if hit < 0 {
			out.WriteString(s[i:])
			break
		}
		out.WriteString(s[i:hit])

		// 跳过模式自身、分隔符与认证方案词，定位凭据本体起点。
		start := hit + len(hitPat)
		for {
			before := start
			start = skipSeparators(s, start)
			start = skipAuthScheme(s, start)
			if start == before {
				break
			}
		}
		if start >= len(s) {
			// 模式后面什么都没有，整段原样保留（保留前缀便于定位）。
			out.WriteString(s[hit:])
			break
		}
		end := start + 1
		for j := start; j < len(s); j++ {
			if isSecretDelimiter(s[j]) {
				end = j
				break
			}
			end = len(s)
		}
		// 保留前缀（含 "Authorization: Bearer" 这类头名），只抹凭据本体。
		out.WriteString(s[hit:start])
		out.WriteString("***")
		i = end
	}
	return out.String()
}
