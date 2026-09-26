package account

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/model"
	"github.com/ximo888ok-netizen/ximo-agent/internal/observability"
)

// StartDeviceLogin 开始设备授权登录（文档 §5.2），返回设备码（机器持有）、
// 用户码（人手抄）与建议轮询间隔。
func (s *Service) StartDeviceLogin(ctx context.Context) (deviceCode, userCode string, intervalMS int64, err error) {
	for attempt := 0; attempt < maxUserCodeAttempts; attempt++ {
		code, err := randomUserCode()
		if err != nil {
			return "", "", 0, err
		}
		plain, hash, err := newToken(devicePrefix, tokenEntropyBytes)
		if err != nil {
			return "", "", 0, err
		}
		now := s.nowMS()
		d := model.DeviceCode{
			DeviceCodeHash: hash,
			UserCode:       code,
			Status:         model.DeviceStatusPending,
			ExpiresAt:      now + DefaultDeviceCodeTTL.Milliseconds(),
			CreatedAt:      now,
			PollIntervalMS: DefaultPollIntervalMS,
		}
		err = s.st.CreateDeviceCode(ctx, d)
		if err == nil {
			return plain, code, DefaultPollIntervalMS, nil
		}
		// 用户码空间只 32^8，理论上可能撞上未失效的旧码，重试即可。
		if errors.Is(err, model.ErrConflict) {
			continue
		}
		return "", "", 0, fmt.Errorf("account: 创建设备码失败: %w", err)
	}
	return "", "", 0, model.ErrConflict
}

// PollDeviceLogin 轮询设备授权结果。待授权返回 model.ErrAuthorizationPending，
// 已授权则一次性兑换出 access/refresh（再次轮询返回 model.ErrRevoked）。
func (s *Service) PollDeviceLogin(ctx context.Context, deviceCode string) (string, string, error) {
	if !strings.HasPrefix(deviceCode, devicePrefix) {
		return "", "", model.ErrBadCredentials
	}
	hash := hashToken(deviceCode)

	mu := s.locks.lock(hash)
	defer mu.Unlock()

	d, err := s.st.GetDeviceCodeByDeviceHash(ctx, hash)
	if err != nil {
		if errors.Is(err, model.ErrNotFound) {
			return "", "", model.ErrBadCredentials
		}
		return "", "", fmt.Errorf("account: 查询设备码失败: %w", err)
	}
	now := s.nowMS()
	if d.ExpiresAt > 0 && now >= d.ExpiresAt {
		return "", "", model.ErrExpired
	}
	switch d.Status {
	case model.DeviceStatusPending:
		s.touchDevicePoll(ctx, hash, now)
		return "", "", model.ErrAuthorizationPending
	case model.DeviceStatusApproved:
		// 继续兑换。
	case model.DeviceStatusConsumed:
		return "", "", model.ErrRevoked
	default:
		// expired 等终态：设备码不可再用。
		return "", "", model.ErrExpired
	}

	u, err := s.st.GetUser(ctx, d.UserID)
	if err != nil {
		if errors.Is(err, model.ErrNotFound) {
			return "", "", model.ErrRevoked
		}
		return "", "", fmt.Errorf("account: 查询用户失败: %w", err)
	}
	if u.Status != model.UserStatusActive {
		return "", "", model.ErrDisabled
	}

	access, refresh, sess, err := s.issueSession(ctx, d.UserID, "", now+DefaultRefreshTTL.Milliseconds())
	if err != nil {
		return "", "", err
	}
	// 先发凭据再消费：消费失败（并发/重复兑换）时把刚建的会话吊销，既不烧掉
	// 合法设备码，也不会出现「一次设备码换出两个会话」。
	if err := s.consumeDeviceCode(ctx, hash, now, d.ExpiresAt); err != nil {
		s.revokeQuiet(ctx, sess.ID, now)
		return "", "", err
	}
	s.touchDevicePoll(ctx, hash, now)
	return access, refresh, nil
}

// ApproveDeviceLogin 由已登录用户在授权页确认用户码。用户码不存在返回
// model.ErrNotFound，已授权/已消费返回 model.ErrConflict，过期返回 model.ErrExpired。
func (s *Service) ApproveDeviceLogin(ctx context.Context, userCode, userID string) error {
	code := normalizeUserCode(userCode)
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return fmt.Errorf("%w: userID 为空", ErrInvalidArgument)
	}
	if !validUserCode(code) {
		// 格式非法的用户码不可能存在，直接按不存在处理，避免用错误差异探测。
		return model.ErrNotFound
	}
	d, err := s.st.GetDeviceCodeByUserCode(ctx, code)
	if err != nil {
		if errors.Is(err, model.ErrNotFound) {
			return model.ErrNotFound
		}
		return fmt.Errorf("account: 查询设备码失败: %w", err)
	}
	now := s.nowMS()
	if d.ExpiresAt > 0 && now >= d.ExpiresAt {
		return model.ErrExpired
	}
	if d.Status != model.DeviceStatusPending {
		return model.ErrConflict
	}
	u, err := s.st.GetUser(ctx, userID)
	if err != nil {
		if errors.Is(err, model.ErrNotFound) {
			return model.ErrNotFound
		}
		return fmt.Errorf("account: 查询用户失败: %w", err)
	}
	if u.Status != model.UserStatusActive {
		return model.ErrDisabled
	}
	if err := s.st.ApproveDeviceCode(ctx, code, userID, now); err != nil {
		switch {
		case errors.Is(err, model.ErrConflict), errors.Is(err, model.ErrNotFound), errors.Is(err, model.ErrExpired):
			return err
		default:
			return fmt.Errorf("account: 授权设备码失败: %w", err)
		}
	}
	return nil
}

// consumeDeviceCode 把已授权的设备码标记为已消费。store 支持 DeviceCodeConsumer
// 时以数据库状态为准，否则用进程内表兜底。
func (s *Service) consumeDeviceCode(ctx context.Context, codeHash string, now, expiresAt int64) error {
	if s.consumer != nil {
		err := s.consumer.ConsumeDeviceCode(ctx, codeHash, now)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, model.ErrConflict), errors.Is(err, model.ErrNotFound):
			// 已被别的请求消费。
			return model.ErrRevoked
		default:
			return fmt.Errorf("account: 消费设备码失败: %w", err)
		}
	}
	if !s.used.claim(codeHash, now, expiresAt) {
		return model.ErrRevoked
	}
	return nil
}

func (s *Service) touchDevicePoll(ctx context.Context, codeHash string, now int64) {
	// 轮询时间戳只用于诊断与限流观测，写失败不阻断授权流程。
	if err := s.st.TouchDeviceCodePoll(ctx, codeHash, now); err != nil {
		observability.LogWarn(ctx, "account: 更新设备码轮询时间失败", map[string]any{
			"error": err.Error(),
		})
	}
}
