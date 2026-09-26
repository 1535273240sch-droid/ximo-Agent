// Package gateway 是 XIMO 中转站的独立 HTTP 客户端与本地凭据存储，
// 供 ximo-plugin 的 CLI 与适配引擎使用（契约 §1「internal/gateway」）。
//
// 独立性：本包属于 plugin/ 这个独立 module
// （module github.com/1535273240sch-droid/ximo-plugin），只依赖标准库 —— 既不
// import 主仓库 ximo-agent 的 internal/**（跨 module 也 import 不了），也不引入
// go.mod 里允许的那两个第三方库。任何 Go 项目都能直接复用它。
//
// 接口形状的来源：对网关的请求/响应按主仓库**真实实现**逐字复刻，不靠猜：
//
//	POST /v1/auth/device   -> {device_code, user_code, expires_in, interval}
//	POST /v1/auth/token    -> {access_token, refresh_token, expires_in, token_type}
//	POST /v1/auth/refresh  -> 同上（轮换：旧 refresh 立即失效）
//	POST /v1/auth/login    -> 同上（用户名 + 口令）
//	GET  /v1/models        -> {data:[{id, display_name, provider, protocols, capabilities, enabled}]}
//	GET  /v1/usage         -> {data:[{request_id, model_id, provider_id, status,
//	                                input_tokens, output_tokens, latency_ms, cost_micro, created_at}]}
//	GET  /v1/health        -> {status, version, time_ms}
//	错误体统一是 {"error":{"message","type","code","request_id"}}
//
// 参考实现位置（只读，不引用）：internal/gateway/api/meta/{auth,models,usage,health}.go、
// internal/gateway/httpx/httpx.go、internal/account/{token,session,device}.go。
//
// 脱敏红线：任何日志、错误、审计输出都不得含明文令牌或密钥。本包默认**不输出任何
// 东西**（Logf 为 nil 时静默），需要日志的调用方通过 Logf 注入；服务端回包会被
// 拿本请求携带的凭据做最后一道掩码（Redact），因此即使网关/代理回显了凭据，
// 也不会进到错误文本里。展示用的摘要是 Mask（只留前后各 4 字符）。
//
// 凭据文件：默认 ~/.ximo-plugin/cred.json，权限 0600（临时文件先 chmod 再写入 +
// rename 原子替换；Windows 上 POSIX 权限位表达不了访问控制，见 PermStatus.Warning）。
package gateway
