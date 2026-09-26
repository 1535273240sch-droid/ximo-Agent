package gateway

import "strings"

// maskEdge 是掩码保留的字符数：前后各 4 个。
const maskEdge = 4

// maskMinRedact 是 Redact 参与替换的最短 secret 长度。太短的串（例如 3 个字符）
// 出现在正常文本里的概率很高，替换会把消息打碎；凭据本身的长度远大于它。
const maskMinRedact = 4

// Mask 把敏感串变成可安全展示的摘要：保留前后各 4 个字符，中间以 **** 代替。
//
// 长度不足时不展示任何片段（整体 ****），避免"掩码本身泄露了原文"。空串返回
// (未配置)，让调用方不必区分"没有凭据"与"凭据是空串"。
func Mask(s string) string {
	if s == "" {
		return "(未配置)"
	}
	r := []rune(s)
	if len(r) <= 2*maskEdge {
		return "****"
	}
	return string(r[:maskEdge]) + "****" + string(r[len(r)-maskEdge:])
}

// Redact 把 s 里出现的每个 secret 换成掩码，返回可安全输出的文本。
//
// 它是"最后一层防线"：网关回包、反向代理错误页、第三方库的错误文本都可能意外带上
// 凭据（服务端本身也会做脱敏，但客户端不能假设对方一定做对）。调用方把本次请求
// 携带过的凭据都传进来即可；短于 maskMinRedact 的串不参与替换。
func Redact(s string, secrets ...string) string {
	out := s
	for _, sec := range secrets {
		if len([]rune(sec)) < maskMinRedact {
			continue
		}
		out = strings.ReplaceAll(out, sec, Mask(sec))
	}
	return out
}
