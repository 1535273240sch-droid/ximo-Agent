// Package secretref 提供 secret_ref 的格式与派生规则。
//
// 规则与 internal/secrets 的 RefForValue/IsRef 逐字一致（sha256 前 128bit 的 hex，
// 前缀 secretref:v1:），但**不依赖平台安全存储**，因此可以跨平台编译。
//
// 为什么需要它：中转站要部署到 Linux 服务器上，而 internal/secrets 目前只在 Windows
// 上能编译（internal/secrets/fallback.go 无条件引用了 DPAPI/CredentialBackend 这些
// Windows 专有符号，且没有加 build tag）。网关只需要"判断一个字符串是不是 secretref"
// 与"由明文派生 ref"这两件事，把它们放在这里就不会被那个包的平台限制拖住。
package secretref

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Prefix 是 secret_ref 的格式前缀。
const Prefix = "secretref:v1:"

// ForValue 由明文值确定性地派生 ref（sha256 前 128bit）。
// ref 是值的单向摘要，无法反推明文；同一值永远得到同一 ref（幂等）。
func ForValue(value string) string {
	sum := sha256.Sum256([]byte(value))
	return Prefix + hex.EncodeToString(sum[:16])
}

// IsRef 报告字符串是否是合法 secret_ref。
func IsRef(s string) bool {
	if !strings.HasPrefix(s, Prefix) {
		return false
	}
	body := strings.TrimPrefix(s, Prefix)
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
