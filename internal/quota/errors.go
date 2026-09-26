package quota

import "errors"

// 本包哨兵错误。调用方判定一律用 errors.Is，不要比较错误字符串。
//
// 额度不足、账号不存在等由存储层抛出的错误会原样带 %w 上抛，因此
// errors.Is(err, model.ErrInsufficientQuota) 在 Reserve 失败路径上依然成立。
var (
	// ErrInvalidArgument 入参不合法（空 ID、负金额、非法账本类型、缺幂等键等）。
	// 这类错误重试无用，HTTP 层应直接映射为 400。
	ErrInvalidArgument = errors.New("quota: invalid argument")

	// ErrUnavailable 额度服务未装配存储（New(nil) 或零值 Service），属装配错误。
	ErrUnavailable = errors.New("quota: store unavailable")
)
