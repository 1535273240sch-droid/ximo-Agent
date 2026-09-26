package account

import (
	"context"
	"errors"

	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/model"
)

// 校验类错误。凭据本身的判定一律用 model.* 哨兵（ErrBadCredentials / ErrExpired
// / ErrRevoked / ErrDisabled），下面的错误只覆盖「调用方传参不合法」。
var (
	// ErrInvalidUsername 用户名不满足格式要求。
	ErrInvalidUsername = errors.New("account: 用户名不合法")
	// ErrWeakPassword 口令不满足强度要求。
	ErrWeakPassword = errors.New("account: 口令强度不足")
	// ErrInvalidArgument 其它入参缺失或非法。
	ErrInvalidArgument = errors.New("account: 参数不合法")
)

// Store 是 account 服务所需的最小存储能力。接口由消费方定义（契约 §10.2），
// internal/gateway/store.Store 的方法签名与本接口逐字一致，可直接传入 New
// （编译期由 integration_test.go 的 var _ Store 断言钉住）。
//
// 对实现的约定（缺失会破坏本包的安全语义）：
//   - CreateUser 在用户名重复时必须返回 model.ErrConflict；
//   - Get* 未命中一律返回 model.ErrNotFound；
//   - RotateAuthSession 必须在**同一事务**里吊销旧行并插入 next（本包不再预先
//     CreateAuthSession，重复插入会撞主键）。注意它不校验旧行此前是否已吊销，
//     因此「同一 refresh 只能换发一次」靠本包按 refresh 哈希分片加锁保证；
//   - ApproveDeviceCode 在 userCode 不存在时返回 model.ErrNotFound，过期返回
//     model.ErrExpired，状态不是 pending 时返回 model.ErrConflict；
//   - 所有写方法内部必须走 sqlite.DB.WithTx，不得使用 ReadDB() 写。
type Store interface {
	// --- 账户 ---
	CreateUser(ctx context.Context, u model.User) error
	GetUser(ctx context.Context, id string) (model.User, error)
	GetUserByName(ctx context.Context, username string) (model.User, error)

	// --- API Key ---
	CreateAPIKey(ctx context.Context, k model.APIKey) error
	GetAPIKeyByHash(ctx context.Context, hash string) (model.APIKey, error)
	RevokeAPIKey(ctx context.Context, id string) error
	TouchAPIKey(ctx context.Context, id string, at int64) error

	// --- 会话 ---
	CreateAuthSession(ctx context.Context, a model.AuthSession) error
	GetAuthSessionByRefreshHash(ctx context.Context, hash string) (model.AuthSession, error)
	GetAuthSessionByAccessHash(ctx context.Context, hash string) (model.AuthSession, error)
	RotateAuthSession(ctx context.Context, oldID string, next model.AuthSession) error
	RevokeAuthSession(ctx context.Context, id string, at int64) error

	// --- 设备码 ---
	CreateDeviceCode(ctx context.Context, d model.DeviceCode) error
	GetDeviceCodeByDeviceHash(ctx context.Context, hash string) (model.DeviceCode, error)
	GetDeviceCodeByUserCode(ctx context.Context, userCode string) (model.DeviceCode, error)
	ApproveDeviceCode(ctx context.Context, userCode, userID string, at int64) error
	TouchDeviceCodePoll(ctx context.Context, hash string, at int64) error
}

// DeviceCodeConsumer 是设备码**一次性消费**的可选扩展能力：store 若实现它，
// PollDeviceLogin 用数据库状态做一次性消费（跨进程/跨重启有效）；未实现时退化
// 为进程内一次性保护（见 Service 文档与 usedDeviceCodes）。
//
// 期望语义：把 status 由 approved 置为 consumed；若行不存在或状态不是 approved
// （已被消费）返回 model.ErrConflict / model.ErrNotFound，不得静默成功。
type DeviceCodeConsumer interface {
	ConsumeDeviceCode(ctx context.Context, deviceCodeHash string, at int64) error
}
