package account

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

const (
	// apiKeyPrefix 是 API Key 明文前缀，形如 "ximo_sk_<32hex>"。
	apiKeyPrefix = "ximo_sk_"
	// accessPrefix / refreshPrefix / devicePrefix 分别对应会话 access、会话
	// refresh 与设备码明文前缀。前缀只用于识别与误用排查，不参与校验强度。
	accessPrefix  = "gwa_"
	refreshPrefix = "gwr_"
	devicePrefix  = "gwd_"

	// apiKeyEntropyBytes 16 字节 → 32 位 hex，正好符合 "ximo_sk_<32hex>"。
	apiKeyEntropyBytes = 16
	// tokenEntropyBytes 32 字节 → 64 位 hex。会话与设备码是机器持有，取足熵。
	tokenEntropyBytes = 32
	// apiKeyPrefixLen 是落库的 KeyPrefix 长度（"ximo_sk_ab12"），仅用于展示。
	apiKeyPrefixLen = 12

	// userCodeAlphabet 排除易混字符 0/O/1/I，供用户手抄（文档 §5.2）。
	userCodeAlphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"
	// userCodeLen 是用户码长度。
	userCodeLen = 8
)

// newToken 生成一次性明文凭据及其落库摘要。
func newToken(prefix string, entropyBytes int) (plain, hash string, err error) {
	buf := make([]byte, entropyBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("account: 生成随机凭据失败: %w", err)
	}
	plain = prefix + hex.EncodeToString(buf)
	return plain, hashToken(plain), nil
}

// hashToken 是凭据的落库形式：sha256 十六进制。随机令牌本身熵足够，无需加盐。
func hashToken(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// randomUserCode 生成 8 位用户码。字母表长度 32 整除 256，因此按字节取模不会
// 引入偏差，无需拒绝采样。
func randomUserCode() (string, error) {
	buf := make([]byte, userCodeLen)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("account: 生成用户码失败: %w", err)
	}
	out := make([]byte, userCodeLen)
	for i, b := range buf {
		out[i] = userCodeAlphabet[int(b)%len(userCodeAlphabet)]
	}
	return string(out), nil
}

// normalizeUserCode 容忍用户输入时的大小写与小写连字符分隔（"abcd-efgh"）。
func normalizeUserCode(code string) string {
	code = strings.ToUpper(strings.TrimSpace(code))
	code = strings.ReplaceAll(code, "-", "")
	code = strings.ReplaceAll(code, " ", "")
	return code
}

func validUserCode(code string) bool {
	if len(code) != userCodeLen {
		return false
	}
	for i := 0; i < len(code); i++ {
		if !strings.ContainsRune(userCodeAlphabet, rune(code[i])) {
			return false
		}
	}
	return true
}
