// Package quota 是 XIMO 中转站的额度服务：在 gateway/store 之上补业务语义
// （预占金额估算、账本类型白名单、账号状态与幂等键校验、超时预占回收），
// 自身不写 SQL、不持有事务。
//
// 金额单位一律为**微单位 int64**（1e-6 credit，禁止 float64）。账户快照的
// 不变量 available = total - used - reserved 由 store 的写事务保证；本包只在
// 调用 store 之前做参数与状态校验，不做本地记账、不做本地缓存。
//
// 并发与幂等（§22 红线）：
//   - 预占的幂等键是 (userID, requestID)：重复 Reserve 不会重复扣减，store 返回
//     既有 held 行。因此 Reserve 的返回值才是**权威** Reservation，调用方必须把它
//     交给 Settle/Release，不要自己拼一条。
//   - 管理类调整的幂等键是 idempotencyKey（库内唯一约束），本包强制非空——它是
//     后台操作防重入的唯一依据（§7.2、§17）。
//   - 所有写路径都落到 store 的单写队列（WithTx），本包不加锁，也就不存在需要
//     跨进程/跨 goroutine 协调的内存状态。
//
// 存储访问通过本包自定义的窄接口 accountStore 完成：实现方无需依赖本包，
// *gateway/store.Store 结构性满足该接口，可直接传入 New。
package quota
