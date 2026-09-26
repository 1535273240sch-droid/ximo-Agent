package account

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/model"
	"github.com/ximo888ok-netizen/ximo-agent/internal/observability"
	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// CreateAPIKey 为用户签发 API Key：返回明文一次（"ximo_sk_<32hex>"），库中只存
// sha256 摘要。ttl <= 0 表示永不过期（ExpiresAt = 0）。
func (s *Service) CreateAPIKey(ctx context.Context, userID string, ttl time.Duration) (string, model.APIKey, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return "", model.APIKey{}, fmt.Errorf("%w: userID 为空", ErrInvalidArgument)
	}
	u, err := s.st.GetUser(ctx, userID)
	if err != nil {
		if errors.Is(err, model.ErrNotFound) {
			return "", model.APIKey{}, model.ErrNotFound
		}
		return "", model.APIKey{}, fmt.Errorf("account: 查询用户失败: %w", err)
	}
	if u.Status != model.UserStatusActive {
		return "", model.APIKey{}, model.ErrDisabled
	}

	plain, hash, err := newToken(apiKeyPrefix, apiKeyEntropyBytes)
	if err != nil {
		return "", model.APIKey{}, err
	}
	now := s.nowMS()
	k := model.APIKey{
		ID:        types.NewID("key"),
		UserID:    userID,
		KeyPrefix: plain[:apiKeyPrefixLen],
		KeyHash:   hash,
		Status:    model.KeyStatusActive,
		CreatedAt: now,
	}
	if ttl > 0 {
		k.ExpiresAt = now + ttl.Milliseconds()
	}
	if err := s.st.CreateAPIKey(ctx, k); err != nil {
		return "", model.APIKey{}, fmt.Errorf("account: 创建 API Key 失败: %w", err)
	}
	return plain, k, nil
}

// VerifyAPIKey 校验 API Key 明文，返回其归属用户。失效原因可判定：
// 不存在/格式非法 → model.ErrBadCredentials，吊销 → model.ErrRevoked，
// 过期 → model.ErrExpired，用户被禁用 → model.ErrDisabled。
func (s *Service) VerifyAPIKey(ctx context.Context, plain string) (model.User, model.APIKey, error) {
	if !strings.HasPrefix(plain, apiKeyPrefix) {
		return model.User{}, model.APIKey{}, model.ErrBadCredentials
	}
	k, err := s.st.GetAPIKeyByHash(ctx, hashToken(plain))
	if err != nil {
		if errors.Is(err, model.ErrNotFound) {
			return model.User{}, model.APIKey{}, model.ErrBadCredentials
		}
		return model.User{}, model.APIKey{}, fmt.Errorf("account: 查询 API Key 失败: %w", err)
	}
	if k.Status != model.KeyStatusActive {
		return model.User{}, model.APIKey{}, model.ErrRevoked
	}
	now := s.nowMS()
	if k.ExpiresAt > 0 && now >= k.ExpiresAt {
		return model.User{}, model.APIKey{}, model.ErrExpired
	}
	u, err := s.st.GetUser(ctx, k.UserID)
	if err != nil {
		if errors.Is(err, model.ErrNotFound) {
			// 用户行已不存在，密钥不可再用；等同凭据无效。
			return model.User{}, model.APIKey{}, model.ErrBadCredentials
		}
		return model.User{}, model.APIKey{}, fmt.Errorf("account: 查询用户失败: %w", err)
	}
	if u.Status != model.UserStatusActive {
		return model.User{}, model.APIKey{}, model.ErrDisabled
	}

	// LastUsedAt 仅用于展示与审计，写失败不阻断鉴权；但不能静默，记一条告警。
	if err := s.st.TouchAPIKey(ctx, k.ID, now); err != nil {
		observability.LogWarn(ctx, "account: 更新 API Key 最近使用时间失败", map[string]any{
			"key_id": k.ID,
			"error":  err.Error(),
		})
	}
	k.LastUsedAt = now
	return u, k, nil
}

// RevokeAPIKey 吊销 API Key（幂等：已吊销再吊销同样成功）。
func (s *Service) RevokeAPIKey(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("%w: key id 为空", ErrInvalidArgument)
	}
	if err := s.st.RevokeAPIKey(ctx, id); err != nil {
		if errors.Is(err, model.ErrNotFound) {
			return model.ErrNotFound
		}
		return fmt.Errorf("account: 吊销 API Key 失败: %w", err)
	}
	return nil
}
