package account

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

const (
	// pwdScheme 是口令哈希的算法标识，是存储格式的第一段。
	pwdScheme = "pbkdf2-sha256"
	// pwdIterations 是 PBKDF2 迭代次数。文档要求「Argon2id 或同等级」，此处取
	// PBKDF2-HMAC-SHA256 210000 次（> 契约下限 200000）。
	pwdIterations = 210000
	// pwdSaltBytes 是随机盐长度（128bit，满足 RFC 8018 建议）。
	pwdSaltBytes = 16
	// pwdKeyBytes 是派生密钥长度（256bit）。
	pwdKeyBytes = 32
	// maxPwdIterations 是解析既有哈希时的迭代次数上限：哈希串若被篡改成天文数字
	// 的迭代次数，校验会退化成 CPU DoS。
	maxPwdIterations = 5_000_000
	// minStoredKeyBytes 是接受的最小密钥长度，低于此值的哈希串视为损坏。
	minStoredKeyBytes = 16
)

// HashPassword 生成 "pbkdf2-sha256$<iter>$<saltB64>$<hashB64>" 形式的口令哈希。
// 盐每次随机，所以同一口令两次调用结果不同。
func HashPassword(password string) (string, error) {
	salt := make([]byte, pwdSaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("account: 生成口令盐失败: %w", err)
	}
	return hashPasswordWithSalt(password, salt, pwdIterations)
}

func hashPasswordWithSalt(password string, salt []byte, iter int) (string, error) {
	key, err := pbkdf2.Key(sha256.New, password, salt, iter, pwdKeyBytes)
	if err != nil {
		return "", fmt.Errorf("account: 派生口令密钥失败: %w", err)
	}
	return fmt.Sprintf("%s$%d$%s$%s", pwdScheme, iter,
		base64.StdEncoding.EncodeToString(salt),
		base64.StdEncoding.EncodeToString(key)), nil
}

// VerifyPassword 校验口令。哈希串损坏、算法不匹配、迭代次数越界都返回 false，
// 比较本身是固定时间的。
func VerifyPassword(hash, password string) bool {
	iter, salt, want, ok := parsePasswordHash(hash)
	if !ok {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iter, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

func parsePasswordHash(hash string) (iter int, salt, want []byte, ok bool) {
	parts := strings.Split(hash, "$")
	if len(parts) != 4 || parts[0] != pwdScheme {
		return 0, nil, nil, false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter <= 0 || iter > maxPwdIterations {
		return 0, nil, nil, false
	}
	salt, err = base64.StdEncoding.DecodeString(parts[2])
	if err != nil || len(salt) == 0 {
		return 0, nil, nil, false
	}
	want, err = base64.StdEncoding.DecodeString(parts[3])
	if err != nil || len(want) < minStoredKeyBytes {
		return 0, nil, nil, false
	}
	return iter, salt, want, true
}

// mixPepper 把服务端 pepper 混入口令：先做 HMAC-SHA256(pepper, password) 再进
// PBKDF2。这样即使数据库整份泄漏，没有 pepper 也无法离线爆破口令；pepper 为空
// 时退化为直接使用口令（单机开发/测试场景）。
//
// 注意：契约里的 HashPassword/VerifyPassword 是包级函数、不带 pepper 参数，因此
// pepper 的混合放在 Service 层，两处必须用同一个函数，否则会出现「注册能过、
// 登录不过」的隐性不一致。
func mixPepper(pepper []byte, password string) string {
	if len(pepper) == 0 {
		return password
	}
	mac := hmac.New(sha256.New, pepper)
	mac.Write([]byte(password))
	return hex.EncodeToString(mac.Sum(nil))
}

// dummyHash 是与真实口令无关的合法格式哈希串，用于「用户不存在」时消耗等量的
// PBKDF2 计算量，避免通过响应时间区分「用户存在但口令错」与「用户不存在」。
var dummyHash = func() string {
	salt := make([]byte, pwdSaltBytes)
	for i := range salt {
		salt[i] = 0xA5
	}
	return fmt.Sprintf("%s$%d$%s$%s", pwdScheme, pwdIterations,
		base64.StdEncoding.EncodeToString(salt),
		base64.StdEncoding.EncodeToString(make([]byte, pwdKeyBytes)))
}()
