package account

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/model"
)

func TestIssueSessionAndVerifyAccess(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	s, clock := newTestService(fs, "")
	u := mustUser(t, s, "alice", "s3cret-password")

	access, refresh, err := s.IssueSession(ctx, u.ID)
	if err != nil {
		t.Fatalf("IssueSession: %v", err)
	}
	if !strings.HasPrefix(access, accessPrefix) || !strings.HasPrefix(refresh, refreshPrefix) {
		t.Fatalf("令牌前缀错误: %q %q", access, refresh)
	}
	for _, v := range fs.storedStrings() {
		if strings.Contains(v, access) || strings.Contains(v, refresh) {
			t.Fatalf("库中出现令牌明文: %q", v)
		}
	}

	got, err := s.VerifyAccess(ctx, access)
	if err != nil {
		t.Fatalf("VerifyAccess: %v", err)
	}
	if got.ID != u.ID {
		t.Errorf("VerifyAccess 用户 = %q, 期望 %q", got.ID, u.ID)
	}
	if _, err := s.VerifyAccess(ctx, refresh); !errors.Is(err, model.ErrBadCredentials) {
		t.Errorf("用 refresh 当 access: err = %v, 期望 ErrBadCredentials", err)
	}
	if _, err := s.VerifyAccess(ctx, "gwa_"+strings.Repeat("a", 64)); !errors.Is(err, model.ErrBadCredentials) {
		t.Errorf("伪造 access: err = %v, 期望 ErrBadCredentials", err)
	}
	if _, err := s.VerifyAccess(ctx, "bogus"); !errors.Is(err, model.ErrBadCredentials) {
		t.Errorf("无前缀 access: err = %v, 期望 ErrBadCredentials", err)
	}

	// 过期边界。
	now := clock.ms()
	live := fs.liveSessions()
	if len(live) != 1 {
		t.Fatalf("活动会话数 = %d, 期望 1", len(live))
	}
	if live[0].AccessExpiresAt != now+DefaultAccessTTL.Milliseconds() {
		t.Errorf("AccessExpiresAt = %d, 期望 %d", live[0].AccessExpiresAt, now+DefaultAccessTTL.Milliseconds())
	}
	clock.advance(DefaultAccessTTL)
	if _, err := s.VerifyAccess(ctx, access); !errors.Is(err, model.ErrExpired) {
		t.Errorf("过期 access: err = %v, 期望 ErrExpired", err)
	}
	if _, err := s.VerifyAccess(ctx, "gwa_"); !errors.Is(err, model.ErrBadCredentials) {
		t.Errorf("仅前缀: err = %v, 期望 ErrBadCredentials", err)
	}
}

func TestIssueSessionRejectsUnknownOrDisabledUser(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	s, _ := newTestService(fs, "")
	u := mustUser(t, s, "alice", "s3cret-password")

	if _, _, err := s.IssueSession(ctx, "usr_missing"); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("未知用户: err = %v, 期望 ErrNotFound", err)
	}
	if _, _, err := s.IssueSession(ctx, " "); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("空 userID: err = %v, 期望 ErrInvalidArgument", err)
	}
	fs.setUserStatus(u.ID, model.UserStatusDisabled)
	if _, _, err := s.IssueSession(ctx, u.ID); !errors.Is(err, model.ErrDisabled) {
		t.Errorf("禁用用户: err = %v, 期望 ErrDisabled", err)
	}
}

func TestRefreshRotatesAndKillsOldTokens(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	s, clock := newTestService(fs, "")
	mustUser(t, s, "alice", "s3cret-password")
	u, err := s.Authenticate(ctx, "alice", "s3cret-password")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	access1, refresh1, err := s.IssueSession(ctx, u.ID)
	if err != nil {
		t.Fatalf("IssueSession: %v", err)
	}
	oldSession := fs.liveSessions()[0]

	clock.advance(time.Minute)
	access2, refresh2, err := s.Refresh(ctx, refresh1)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if access2 == access1 || refresh2 == refresh1 {
		t.Fatal("轮换后令牌未变化")
	}
	if _, err := s.VerifyAccess(ctx, access1); !errors.Is(err, model.ErrRevoked) {
		t.Errorf("旧 access: err = %v, 期望 ErrRevoked", err)
	}
	if _, err := s.VerifyAccess(ctx, access2); err != nil {
		t.Errorf("新 access 不可用: %v", err)
	}

	// 旧 refresh 复用必须失败（一次性）。
	if _, _, err := s.Refresh(ctx, refresh1); !errors.Is(err, model.ErrRevoked) {
		t.Errorf("复用旧 refresh: err = %v, 期望 ErrRevoked", err)
	}
	// 轮换不延长会话总寿命，且只留一个活动会话。
	live := fs.liveSessions()
	if len(live) != 1 {
		t.Fatalf("活动会话数 = %d, 期望 1", len(live))
	}
	if live[0].RefreshExpiresAt != oldSession.RefreshExpiresAt {
		t.Errorf("RefreshExpiresAt 被延长: %d → %d", oldSession.RefreshExpiresAt, live[0].RefreshExpiresAt)
	}
	if live[0].RotatedFrom != oldSession.ID {
		t.Errorf("RotatedFrom = %q, 期望 %q", live[0].RotatedFrom, oldSession.ID)
	}
}

func TestRefreshFailures(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	s, _ := newTestService(fs, "")
	u := mustUser(t, s, "alice", "s3cret-password")
	_, refresh, err := s.IssueSession(ctx, u.ID)
	if err != nil {
		t.Fatalf("IssueSession: %v", err)
	}

	if _, _, err := s.Refresh(ctx, "gwa_"+strings.Repeat("a", 64)); !errors.Is(err, model.ErrBadCredentials) {
		t.Errorf("用 access 刷新: err = %v, 期望 ErrBadCredentials", err)
	}
	if _, _, err := s.Refresh(ctx, "gwr_"+strings.Repeat("a", 64)); !errors.Is(err, model.ErrBadCredentials) {
		t.Errorf("伪造 refresh: err = %v, 期望 ErrBadCredentials", err)
	}

	// 用户被禁用后不能刷新。
	fs.setUserStatus(u.ID, model.UserStatusDisabled)
	if _, _, err := s.Refresh(ctx, refresh); !errors.Is(err, model.ErrDisabled) {
		t.Errorf("禁用用户刷新: err = %v, 期望 ErrDisabled", err)
	}
}

func TestRefreshExpiredSession(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	s, clock := newTestService(fs, "")
	u := mustUser(t, s, "alice", "s3cret-password")
	_, refresh, err := s.IssueSession(ctx, u.ID)
	if err != nil {
		t.Fatalf("IssueSession: %v", err)
	}
	clock.advance(DefaultRefreshTTL)
	if _, _, err := s.Refresh(ctx, refresh); !errors.Is(err, model.ErrExpired) {
		t.Errorf("过期 refresh: err = %v, 期望 ErrExpired", err)
	}
}

func TestRefreshRevokedSession(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	s, _ := newTestService(fs, "")
	u := mustUser(t, s, "alice", "s3cret-password")
	_, refresh, err := s.IssueSession(ctx, u.ID)
	if err != nil {
		t.Fatalf("IssueSession: %v", err)
	}
	sess := fs.liveSessions()[0]
	if err := fs.RevokeAuthSession(ctx, sess.ID, sess.CreatedAt+1); err != nil {
		t.Fatalf("RevokeAuthSession: %v", err)
	}
	if _, _, err := s.Refresh(ctx, refresh); !errors.Is(err, model.ErrRevoked) {
		t.Errorf("已吊销会话刷新: err = %v, 期望 ErrRevoked", err)
	}
}

func TestConcurrentRefreshOnlyOneWins(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	s, _ := newTestService(fs, "")
	u := mustUser(t, s, "alice", "s3cret-password")
	_, refresh, err := s.IssueSession(ctx, u.ID)
	if err != nil {
		t.Fatalf("IssueSession: %v", err)
	}

	const n = 8
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, _, errs[i] = s.Refresh(ctx, refresh)
		}(i)
	}
	close(start)
	wg.Wait()

	success, revoked := 0, 0
	for i, err := range errs {
		switch {
		case err == nil:
			success++
		case errors.Is(err, model.ErrRevoked):
			revoked++
		default:
			t.Errorf("goroutine %d: 非预期错误 %v", i, err)
		}
	}
	if success != 1 {
		t.Fatalf("并发刷新成功 %d 次, 期望 1 次", success)
	}
	if success+revoked != n {
		t.Fatalf("成功 %d + 已吊销 %d != %d", success, revoked, n)
	}
	if live := fs.liveSessions(); len(live) != 1 {
		t.Fatalf("活动会话数 = %d, 期望 1", len(live))
	}
}

func TestRefreshRollbackWhenRotateFails(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	s, _ := newTestService(fs, "")
	u := mustUser(t, s, "alice", "s3cret-password")
	_, refresh, err := s.IssueSession(ctx, u.ID)
	if err != nil {
		t.Fatalf("IssueSession: %v", err)
	}
	oldSession := fs.liveSessions()[0]

	rotateErr := errors.New("rotate failed")
	fs.rotateErr = rotateErr
	if _, _, err := s.Refresh(ctx, refresh); !errors.Is(err, rotateErr) {
		t.Fatalf("轮换失败应返回底层错误: %v", err)
	}
	// 轮换失败时不得留下新会话，也不得吊销旧会话（由 store 的同事务保证）。
	live := fs.liveSessions()
	if len(live) != 1 || live[0].ID != oldSession.ID {
		t.Fatalf("轮换失败后活动会话异常: %+v", live)
	}
	if _, ok := fs.sessionByID(live[0].ID); !ok {
		t.Fatal("旧会话丢失")
	}
}
