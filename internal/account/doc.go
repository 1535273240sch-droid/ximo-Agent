// Package account 实现 XIMO 中转站的账号与凭据域：用户名口令注册/登录、
// API Key 签发与校验、不透明令牌会话（access/refresh 轮换）、设备码授权登录。
//
// 安全红线（文档第21章）：
//   - 口令只以 PBKDF2-HMAC-SHA256（迭代次数见 pwdIterations）+ 每用户随机盐
//     落库，可选叠加服务端 pepper（见 Service 文档）；
//   - API Key / access / refresh / device code 的**明文只在创建时返回一次**，
//     库中只存 sha256 十六进制摘要，服务端不持有可回放的凭据；
//   - 一切凭据比较走固定时间比较，不做长度/前缀短路；校验失败一律返回
//     model.ErrBadCredentials，不区分「不存在」与「口令错」，避免账号枚举；
//   - 本包不写 SQL：只依赖 internal/gateway/model 与本包自定义的窄接口 Store
//     （契约 §10.2），具体存储实现由调用方注入。
package account
