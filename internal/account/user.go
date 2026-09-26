package account

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/model"
	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

const (
	// usernameMinLen / usernameMaxLen 是用户名长度边界（按字符计）。
	usernameMinLen = 3
	usernameMaxLen = 64
	// passwordMinLen / passwordMaxLen 是口令长度边界（按字节计）。PBKDF2 对超长
	// 口令不敏感，但上限防止用超长输入放大单次请求成本。
	passwordMinLen = 8
	passwordMaxLen = 512
)

// CreateUser 注册用户。用户名重复返回 model.ErrConflict。
func (s *Service) CreateUser(ctx context.Context, username, password, groupID string) (model.User, error) {
	name := normalizeUsername(username)
	if err := validateUsername(name); err != nil {
		return model.User{}, err
	}
	if err := validatePassword(password); err != nil {
		return model.User{}, err
	}

	// 先查一次给出明确冲突；真正的唯一性仍由库的唯一索引保证（并发注册时
	// CreateUser 返回 model.ErrConflict，见 Store 接口约定）。
	if _, err := s.st.GetUserByName(ctx, name); err == nil {
		return model.User{}, model.ErrConflict
	} else if !errors.Is(err, model.ErrNotFound) {
		return model.User{}, fmt.Errorf("account: 查询用户名失败: %w", err)
	}

	hash, err := HashPassword(mixPepper(s.pepper, password))
	if err != nil {
		return model.User{}, err
	}
	now := s.nowMS()
	u := model.User{
		ID:           types.NewID("usr"),
		Username:     name,
		PasswordHash: hash,
		Status:       model.UserStatusActive,
		GroupID:      groupID,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := s.st.CreateUser(ctx, u); err != nil {
		if errors.Is(err, model.ErrConflict) {
			return model.User{}, model.ErrConflict
		}
		return model.User{}, fmt.Errorf("account: 创建用户失败: %w", err)
	}
	return u, nil
}

// Authenticate 校验用户名口令。用户不存在与口令错误都返回
// model.ErrBadCredentials（且消耗等量 CPU），账号被禁用返回 model.ErrDisabled。
func (s *Service) Authenticate(ctx context.Context, username, password string) (model.User, error) {
	name := normalizeUsername(username)
	u, err := s.st.GetUserByName(ctx, name)
	if err != nil {
		if errors.Is(err, model.ErrNotFound) {
			// 抗账号枚举：不存在也要付一次 PBKDF2 的时间。
			VerifyPassword(dummyHash, mixPepper(s.pepper, password))
			return model.User{}, model.ErrBadCredentials
		}
		return model.User{}, fmt.Errorf("account: 查询用户失败: %w", err)
	}
	if !VerifyPassword(u.PasswordHash, mixPepper(s.pepper, password)) {
		return model.User{}, model.ErrBadCredentials
	}
	// 状态判定放在口令校验之后：未通过身份验证的调用方不应拿到状态差异。
	if u.Status != model.UserStatusActive {
		return model.User{}, model.ErrDisabled
	}
	return u, nil
}

func normalizeUsername(username string) string {
	return strings.TrimSpace(username)
}

func validateUsername(name string) error {
	if n := utf8.RuneCountInString(name); n < usernameMinLen || n > usernameMaxLen {
		return fmt.Errorf("%w: 长度需在 %d~%d 之间", ErrInvalidUsername, usernameMinLen, usernameMaxLen)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: 含控制字符", ErrInvalidUsername)
		}
	}
	return nil
}

func validatePassword(password string) error {
	if len(password) < passwordMinLen {
		return fmt.Errorf("%w: 至少 %d 个字节", ErrWeakPassword, passwordMinLen)
	}
	if len(password) > passwordMaxLen {
		return fmt.Errorf("%w: 至多 %d 个字节", ErrWeakPassword, passwordMaxLen)
	}
	return nil
}
