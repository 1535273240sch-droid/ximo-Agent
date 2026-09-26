package account

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/model"
)

func TestCreateAPIKeyStoresOnlyHash(t *testing.T) {
	fs := newFakeStore()
	s, clock := newTestService(fs, "")
	u := mustUser(t, s, "alice", "s3cret-password")

	plain, k, err := s.CreateAPIKey(context.Background(), u.ID, 0)
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	if !strings.HasPrefix(plain, "ximo_sk_") || len(plain) != len("ximo_sk_")+32 {
		t.Fatalf("明文格式错误: %q", plain)
	}
	if _, err := hex.DecodeString(plain[len("ximo_sk_"):]); err != nil {
		t.Fatalf("明文主体不是 32 位 hex: %q", plain)
	}
	want := sha256.Sum256([]byte(plain))
	if k.KeyHash != hex.EncodeToString(want[:]) {
		t.Errorf("KeyHash 不是明文 sha256: %q", k.KeyHash)
	}
	if k.KeyPrefix != plain[:apiKeyPrefixLen] {
		t.Errorf("KeyPrefix = %q, 期望明文前 %d 位", k.KeyPrefix, apiKeyPrefixLen)
	}
	if k.Status != model.KeyStatusActive || k.ExpiresAt != 0 {
		t.Errorf("新密钥字段异常: %+v", k)
	}
	if k.CreatedAt != clock.ms() {
		t.Errorf("CreatedAt = %d, 期望 %d", k.CreatedAt, clock.ms())
	}

	for _, v := range fs.storedStrings() {
		if strings.Contains(v, plain) {
			t.Fatalf("库中出现 API Key 明文: %q", v)
		}
	}
	if _, _, err := s.VerifyAPIKey(context.Background(), plain); err != nil {
		t.Fatalf("VerifyAPIKey: %v", err)
	}
}

func TestVerifyAPIKeySuccessAndLastUsed(t *testing.T) {
	fs := newFakeStore()
	s, clock := newTestService(fs, "")
	u := mustUser(t, s, "alice", "s3cret-password")
	plain, k, err := s.CreateAPIKey(context.Background(), u.ID, time.Hour)
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}

	clock.advance(time.Minute)
	got, gotKey, err := s.VerifyAPIKey(context.Background(), plain)
	if err != nil {
		t.Fatalf("VerifyAPIKey: %v", err)
	}
	if got.ID != u.ID || gotKey.ID != k.ID {
		t.Errorf("返回了错误的用户/密钥: %+v %+v", got, gotKey)
	}
	if gotKey.LastUsedAt != clock.ms() {
		t.Errorf("LastUsedAt = %d, 期望 %d", gotKey.LastUsedAt, clock.ms())
	}
}

func TestVerifyAPIKeyFailures(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	s, clock := newTestService(fs, "")
	u := mustUser(t, s, "alice", "s3cret-password")

	good, _, err := s.CreateAPIKey(ctx, u.ID, 0)
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	shortLived, expiredKey, err := s.CreateAPIKey(ctx, u.ID, time.Minute)
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	toRevoke, revokedKey, err := s.CreateAPIKey(ctx, u.ID, 0)
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	if err := s.RevokeAPIKey(ctx, revokedKey.ID); err != nil {
		t.Fatalf("RevokeAPIKey: %v", err)
	}
	clock.advance(2 * time.Minute) // shortLived 过期

	cases := []struct {
		name  string
		plain string
		want  error
	}{
		{"前缀错误", "sk_" + strings.Repeat("a", 32), model.ErrBadCredentials},
		{"空串", "", model.ErrBadCredentials},
		{"未知密钥", "ximo_sk_" + strings.Repeat("a", 32), model.ErrBadCredentials},
		{"已过期", shortLived, model.ErrExpired},
		{"已吊销", toRevoke, model.ErrRevoked},
	}
	for _, tc := range cases {
		if _, _, err := s.VerifyAPIKey(ctx, tc.plain); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, 期望 %v", tc.name, err, tc.want)
		}
	}
	if _, _, err := s.VerifyAPIKey(ctx, good); err != nil {
		t.Errorf("长期密钥被误判: %v", err)
	}
	if _, _, err := s.VerifyAPIKey(ctx, expiredKey.KeyHash); !errors.Is(err, model.ErrBadCredentials) {
		t.Errorf("传入哈希而非明文: err = %v, 期望 ErrBadCredentials", err)
	}

	fs.setUserStatus(u.ID, model.UserStatusDisabled)
	if _, _, err := s.VerifyAPIKey(ctx, good); !errors.Is(err, model.ErrDisabled) {
		t.Errorf("用户被禁用: err = %v, 期望 ErrDisabled", err)
	}
}

func TestRevokeAPIKeyErrors(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	s, _ := newTestService(fs, "")
	u := mustUser(t, s, "alice", "s3cret-password")
	plain, k, err := s.CreateAPIKey(ctx, u.ID, 0)
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	if err := s.RevokeAPIKey(ctx, "key_missing"); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("吊销不存在的密钥: err = %v, 期望 ErrNotFound", err)
	}
	if err := s.RevokeAPIKey(ctx, "  "); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("空 id: err = %v, 期望 ErrInvalidArgument", err)
	}
	if err := s.RevokeAPIKey(ctx, k.ID); err != nil {
		t.Fatalf("RevokeAPIKey: %v", err)
	}
	if _, _, err := s.VerifyAPIKey(ctx, plain); !errors.Is(err, model.ErrRevoked) {
		t.Errorf("吊销后校验: err = %v, 期望 ErrRevoked", err)
	}
}

func TestCreateAPIKeyRejectsUnknownOrDisabledUser(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	s, _ := newTestService(fs, "")
	u := mustUser(t, s, "alice", "s3cret-password")

	if _, _, err := s.CreateAPIKey(ctx, "usr_missing", 0); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("未知用户: err = %v, 期望 ErrNotFound", err)
	}
	if _, _, err := s.CreateAPIKey(ctx, "", 0); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("空 userID: err = %v, 期望 ErrInvalidArgument", err)
	}
	fs.setUserStatus(u.ID, model.UserStatusDisabled)
	if _, _, err := s.CreateAPIKey(ctx, u.ID, 0); !errors.Is(err, model.ErrDisabled) {
		t.Errorf("禁用用户: err = %v, 期望 ErrDisabled", err)
	}
}

func TestVerifyAPIKeySurvivesTouchFailure(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	s, _ := newTestService(fs, "")
	u := mustUser(t, s, "alice", "s3cret-password")
	plain, _, err := s.CreateAPIKey(ctx, u.ID, 0)
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	// 最近使用时间只用于展示，写入失败不应导致鉴权失败（非静默：记告警）。
	fs.touchKeyErr = errors.New("db is busy")
	if _, _, err := s.VerifyAPIKey(ctx, plain); err != nil {
		t.Fatalf("写 LastUsedAt 失败时鉴权不应失败: %v", err)
	}
	if fs.touchKeyCalls != 1 {
		t.Errorf("TouchAPIKey 调用次数 = %d, 期望 1", fs.touchKeyCalls)
	}
}
