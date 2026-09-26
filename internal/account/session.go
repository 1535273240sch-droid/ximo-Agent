package account

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/model"
	"github.com/ximo888ok-netizen/ximo-agent/internal/observability"
	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// buildSession 生成一对 access/refresh 凭据与会话行，但**不落库**：首次签发用
// issueSession（CreateAuthSession），轮换由 RotateAuthSession 在自己的事务里插入
// 新行（先 Create 再 Rotate 会撞主键）。
// refreshExpiresAt 是会话的绝对寿命上限，access 不会活过它。
func (s *Service) buildSession(userID, rotatedFrom string, refreshExpiresAt int64) (string, string, model.AuthSession, error) {
	now := s.nowMS()
	if refreshExpiresAt <= now {
		return "", "", model.AuthSession{}, model.ErrExpired
	}
	access, accessHash, err := newToken(accessPrefix, tokenEntropyBytes)
	if err != nil {
		return "", "", model.AuthSession{}, err
	}
	refresh, refreshHash, err := newToken(refreshPrefix, tokenEntropyBytes)
	if err != nil {
		return "", "", model.AuthSession{}, err
	}
	accessExpiresAt := now + DefaultAccessTTL.Milliseconds()
	if accessExpiresAt > refreshExpiresAt {
		accessExpiresAt = refreshExpiresAt
	}
	sess := model.AuthSession{
		ID:               types.NewID("ses"),
		UserID:           userID,
		AccessHash:       accessHash,
		RefreshHash:      refreshHash,
		RotatedFrom:      rotatedFrom,
		AccessExpiresAt:  accessExpiresAt,
		RefreshExpiresAt: refreshExpiresAt,
		CreatedAt:        now,
	}
	return access, refresh, sess, nil
}

// issueSession 生成凭据并落库（只落摘要）。
func (s *Service) issueSession(ctx context.Context, userID, rotatedFrom string, refreshExpiresAt int64) (string, string, model.AuthSession, error) {
	access, refresh, sess, err := s.buildSession(userID, rotatedFrom, refreshExpiresAt)
	if err != nil {
		return "", "", model.AuthSession{}, err
	}
	if err := s.st.CreateAuthSession(ctx, sess); err != nil {
		return "", "", model.AuthSession{}, fmt.Errorf("account: 创建会话失败: %w", err)
	}
	return access, refresh, sess, nil
}

// IssueSession 为已通过身份验证的用户签发一对 access/refresh 凭据。
//
// 契约 §5 冻结了 Refresh / VerifyAccess，但没有「登录成功后发令牌」的入口，而
// 口令登录、设备登录、刷新三处都需要，故按纯新增方式补上（未改动任何既有签名）。
func (s *Service) IssueSession(ctx context.Context, userID string) (access, refresh string, err error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return "", "", fmt.Errorf("%w: userID 为空", ErrInvalidArgument)
	}
	u, err := s.st.GetUser(ctx, userID)
	if err != nil {
		if errors.Is(err, model.ErrNotFound) {
			return "", "", model.ErrNotFound
		}
		return "", "", fmt.Errorf("account: 查询用户失败: %w", err)
	}
	if u.Status != model.UserStatusActive {
		return "", "", model.ErrDisabled
	}
	access, refresh, _, err = s.issueSession(ctx, userID, "", s.nowMS()+DefaultRefreshTTL.Milliseconds())
	if err != nil {
		return "", "", err
	}
	return access, refresh, nil
}

// Refresh 用 refresh token 换取新的 access/refresh（轮换）。旧 refresh 在同一次
// 事务里被吊销，复用旧 refresh 返回 model.ErrRevoked；过期返回 model.ErrExpired。
func (s *Service) Refresh(ctx context.Context, refreshToken string) (string, string, error) {
	if !strings.HasPrefix(refreshToken, refreshPrefix) {
		return "", "", model.ErrBadCredentials
	}
	hash := hashToken(refreshToken)

	// 同一 refresh 的并发刷新串行化：否则两个请求可能都读到「未吊销」并各自
	// 换发一个会话。
	mu := s.locks.lock(hash)
	defer mu.Unlock()

	sess, err := s.st.GetAuthSessionByRefreshHash(ctx, hash)
	if err != nil {
		if errors.Is(err, model.ErrNotFound) {
			return "", "", model.ErrBadCredentials
		}
		return "", "", fmt.Errorf("account: 查询会话失败: %w", err)
	}
	now := s.nowMS()
	if sess.RevokedAt != 0 {
		return "", "", model.ErrRevoked
	}
	if sess.RefreshExpiresAt > 0 && now >= sess.RefreshExpiresAt {
		return "", "", model.ErrExpired
	}
	u, err := s.st.GetUser(ctx, sess.UserID)
	if err != nil {
		if errors.Is(err, model.ErrNotFound) {
			return "", "", model.ErrBadCredentials
		}
		return "", "", fmt.Errorf("account: 查询用户失败: %w", err)
	}
	if u.Status != model.UserStatusActive {
		return "", "", model.ErrDisabled
	}

	// 新会话不单独落库：RotateAuthSession 在同一事务里吊销旧行并插入新行，
	// 这样不存在「旧 refresh 已失效但新会话没落库」或反向的中间态。
	access, refresh, next, err := s.buildSession(sess.UserID, sess.ID, refreshDeadline(sess.RefreshExpiresAt, now))
	if err != nil {
		return "", "", err
	}
	if err := s.st.RotateAuthSession(ctx, sess.ID, next); err != nil {
		// store 的轮换是事务性的，理论上失败即未落库；这里仍兜底吊销一次，
		// 防止实现方漏掉原子性时留下一个没人持有的活动会话。
		s.revokeQuiet(ctx, next.ID, now)
		return "", "", fmt.Errorf("account: 轮换会话失败: %w", err)
	}
	return access, refresh, nil
}

// revokeQuiet 是「回滚刚发出的会话」的收尾动作：失败不改写已经返回给调用方的
// 错误，但要留下痕迹；会话不存在属于预期情况（事务已回滚），不记为异常。
func (s *Service) revokeQuiet(ctx context.Context, sessionID string, now int64) {
	err := s.st.RevokeAuthSession(ctx, sessionID, now)
	if err != nil && !errors.Is(err, model.ErrNotFound) {
		observability.LogWarn(ctx, "account: 吊销会话失败", map[string]any{
			"session_id": sessionID,
			"error":      err.Error(),
		})
	}
}

// refreshDeadline 计算新会话的 refresh 绝对期限：沿用旧会话的期限（轮换不延长
// 会话总寿命）；旧行没有期限（异常数据）时退化为从当前时间起的默认寿命。
func refreshDeadline(oldRefreshExpiresAt, now int64) int64 {
	if oldRefreshExpiresAt > 0 {
		return oldRefreshExpiresAt
	}
	return now + DefaultRefreshTTL.Milliseconds()
}

// VerifyAccess 校验 access token，返回其归属用户。
func (s *Service) VerifyAccess(ctx context.Context, accessToken string) (model.User, error) {
	if !strings.HasPrefix(accessToken, accessPrefix) {
		return model.User{}, model.ErrBadCredentials
	}
	sess, err := s.st.GetAuthSessionByAccessHash(ctx, hashToken(accessToken))
	if err != nil {
		if errors.Is(err, model.ErrNotFound) {
			return model.User{}, model.ErrBadCredentials
		}
		return model.User{}, fmt.Errorf("account: 查询会话失败: %w", err)
	}
	if sess.RevokedAt != 0 {
		return model.User{}, model.ErrRevoked
	}
	if sess.AccessExpiresAt > 0 && s.nowMS() >= sess.AccessExpiresAt {
		return model.User{}, model.ErrExpired
	}
	u, err := s.st.GetUser(ctx, sess.UserID)
	if err != nil {
		if errors.Is(err, model.ErrNotFound) {
			return model.User{}, model.ErrBadCredentials
		}
		return model.User{}, fmt.Errorf("account: 查询用户失败: %w", err)
	}
	if u.Status != model.UserStatusActive {
		return model.User{}, model.ErrDisabled
	}
	return u, nil
}
