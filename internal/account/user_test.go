package account

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/model"
)

func TestCreateUserAndAuthenticate(t *testing.T) {
	fs := newFakeStore()
	s, _ := newTestService(fs, "pepper-v1")
	u := mustUser(t, s, "alice", "s3cret-password")

	if u.ID == "" || u.Status != model.UserStatusActive {
		t.Fatalf("用户字段异常: %+v", u)
	}
	if strings.Contains(u.PasswordHash, "s3cret-password") {
		t.Fatal("口令明文出现在 PasswordHash 中")
	}
	if !strings.HasPrefix(u.PasswordHash, "pbkdf2-sha256$") {
		t.Errorf("PasswordHash = %q", u.PasswordHash)
	}

	got, err := s.Authenticate(context.Background(), "alice", "s3cret-password")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if got.ID != u.ID {
		t.Errorf("Authenticate 返回的用户 ID = %q, 期望 %q", got.ID, u.ID)
	}
	// 用户名两侧空白应被容忍。
	if _, err := s.Authenticate(context.Background(), "  alice  ", "s3cret-password"); err != nil {
		t.Errorf("带空白用户名登录失败: %v", err)
	}
}

func TestAuthenticateFailures(t *testing.T) {
	fs := newFakeStore()
	s, _ := newTestService(fs, "pepper-v1")
	u := mustUser(t, s, "bob", "s3cret-password")

	ctx := context.Background()
	if _, err := s.Authenticate(ctx, "bob", "wrong-password"); !errors.Is(err, model.ErrBadCredentials) {
		t.Errorf("错误口令: err = %v, 期望 ErrBadCredentials", err)
	}
	if _, err := s.Authenticate(ctx, "nobody", "s3cret-password"); !errors.Is(err, model.ErrBadCredentials) {
		t.Errorf("不存在的用户: err = %v, 期望 ErrBadCredentials", err)
	}

	fs.setUserStatus(u.ID, model.UserStatusDisabled)
	if _, err := s.Authenticate(ctx, "bob", "s3cret-password"); !errors.Is(err, model.ErrDisabled) {
		t.Errorf("禁用用户: err = %v, 期望 ErrDisabled", err)
	}
	if _, err := s.Authenticate(ctx, "bob", "wrong-password"); !errors.Is(err, model.ErrBadCredentials) {
		t.Errorf("禁用用户+错误口令: err = %v, 期望先判口令 ErrBadCredentials", err)
	}
}

func TestCreateUserRejectsDuplicate(t *testing.T) {
	fs := newFakeStore()
	s, _ := newTestService(fs, "")
	mustUser(t, s, "carol", "s3cret-password")
	if _, err := s.CreateUser(context.Background(), "carol", "another-password", ""); !errors.Is(err, model.ErrConflict) {
		t.Errorf("重名注册: err = %v, 期望 ErrConflict", err)
	}
}

func TestCreateUserValidation(t *testing.T) {
	fs := newFakeStore()
	s, _ := newTestService(fs, "")
	ctx := context.Background()

	cases := []struct {
		name     string
		username string
		password string
		want     error
	}{
		{"用户名为空", "", "s3cret-password", ErrInvalidUsername},
		{"用户名过短", "ab", "s3cret-password", ErrInvalidUsername},
		{"用户名过长", strings.Repeat("a", 65), "s3cret-password", ErrInvalidUsername},
		{"用户名含控制字符", "al\x01ce", "s3cret-password", ErrInvalidUsername},
		{"口令过短", "dave", "short", ErrWeakPassword},
		{"口令过长", "dave", strings.Repeat("x", 513), ErrWeakPassword},
	}
	for _, tc := range cases {
		if _, err := s.CreateUser(ctx, tc.username, tc.password, ""); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, 期望 %v", tc.name, err, tc.want)
		}
	}
}

func TestPepperMismatchBreaksLogin(t *testing.T) {
	fs := newFakeStore()
	s1, _ := newTestService(fs, "pepper-v1")
	s2, _ := newTestService(fs, "pepper-v2")
	mustUser(t, s1, "erin", "s3cret-password")

	if _, err := s2.Authenticate(context.Background(), "erin", "s3cret-password"); !errors.Is(err, model.ErrBadCredentials) {
		t.Errorf("pepper 不一致时: err = %v, 期望 ErrBadCredentials", err)
	}
}

func TestCreateUserDoesNotStorePlaintext(t *testing.T) {
	const pw = "s3cret-password"
	fs := newFakeStore()
	s, _ := newTestService(fs, "")
	mustUser(t, s, "frank", pw)
	for _, v := range fs.storedStrings() {
		if strings.Contains(v, pw) {
			t.Fatalf("库中出现口令明文: %q", v)
		}
	}
}
