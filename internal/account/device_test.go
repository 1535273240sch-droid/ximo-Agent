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

func TestDeviceLoginHappyPath(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	s, clock := newTestService(fs, "")
	u := mustUser(t, s, "alice", "s3cret-password")

	deviceCode, userCode, intervalMS, err := s.StartDeviceLogin(ctx)
	if err != nil {
		t.Fatalf("StartDeviceLogin: %v", err)
	}
	if !strings.HasPrefix(deviceCode, devicePrefix) {
		t.Errorf("设备码前缀错误: %q", deviceCode)
	}
	if intervalMS != DefaultPollIntervalMS {
		t.Errorf("intervalMS = %d, 期望 %d", intervalMS, DefaultPollIntervalMS)
	}
	if !validUserCode(userCode) {
		t.Fatalf("用户码不合法: %q", userCode)
	}
	if _, _, err := s.PollDeviceLogin(ctx, deviceCode); !errors.Is(err, model.ErrAuthorizationPending) {
		t.Fatalf("待授权轮询: err = %v, 期望 ErrAuthorizationPending", err)
	}

	// 授权时容忍小写与连字符写法。
	pretty := strings.ToLower(userCode[:4] + "-" + userCode[4:])
	if err := s.ApproveDeviceLogin(ctx, pretty, u.ID); err != nil {
		t.Fatalf("ApproveDeviceLogin: %v", err)
	}
	if _, ok := fs.deviceByUserCode(userCode); !ok {
		t.Fatal("设备码未落库")
	}

	clock.advance(time.Second)
	access, refresh, err := s.PollDeviceLogin(ctx, deviceCode)
	if err != nil {
		t.Fatalf("授权后轮询: %v", err)
	}
	got, err := s.VerifyAccess(ctx, access)
	if err != nil {
		t.Fatalf("VerifyAccess: %v", err)
	}
	if got.ID != u.ID {
		t.Errorf("登录用户 = %q, 期望 %q", got.ID, u.ID)
	}
	if _, _, err := s.Refresh(ctx, refresh); err != nil {
		t.Errorf("设备登录得到的 refresh 不可用: %v", err)
	}

	// 一次性消费：设备码不可再次兑换。
	if _, _, err := s.PollDeviceLogin(ctx, deviceCode); !errors.Is(err, model.ErrRevoked) {
		t.Errorf("重复轮询: err = %v, 期望 ErrRevoked", err)
	}
	if _, err := s.VerifyAccess(ctx, access); !errors.Is(err, model.ErrRevoked) {
		t.Errorf("兑换出的 access 在轮换后仍有效: err = %v", err)
	}
}

func TestDeviceLoginConsumedByStore(t *testing.T) {
	ctx := context.Background()
	base := newFakeStore()
	fs := &fakeConsumerStore{fakeStore: base}
	s, _ := newTestService(fs, "")
	u := mustUser(t, s, "alice", "s3cret-password")

	deviceCode, userCode, _, err := s.StartDeviceLogin(ctx)
	if err != nil {
		t.Fatalf("StartDeviceLogin: %v", err)
	}
	if err := s.ApproveDeviceLogin(ctx, userCode, u.ID); err != nil {
		t.Fatalf("ApproveDeviceLogin: %v", err)
	}
	if _, _, err := s.PollDeviceLogin(ctx, deviceCode); err != nil {
		t.Fatalf("PollDeviceLogin: %v", err)
	}
	d, ok := base.deviceByUserCode(userCode)
	if !ok {
		t.Fatal("设备码未落库")
	}
	if d.Status != model.DeviceStatusConsumed {
		t.Errorf("设备码状态 = %q, 期望 %q（store 支持消费时应落库）", d.Status, model.DeviceStatusConsumed)
	}
	if _, _, err := s.PollDeviceLogin(ctx, deviceCode); !errors.Is(err, model.ErrRevoked) {
		t.Errorf("重复轮询: err = %v, 期望 ErrRevoked", err)
	}
}

func TestApproveDeviceLoginErrors(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	s, clock := newTestService(fs, "")
	u := mustUser(t, s, "alice", "s3cret-password")

	_, userCode, _, err := s.StartDeviceLogin(ctx)
	if err != nil {
		t.Fatalf("StartDeviceLogin: %v", err)
	}
	if err := s.ApproveDeviceLogin(ctx, userCode, ""); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("空 userID: err = %v, 期望 ErrInvalidArgument", err)
	}
	if err := s.ApproveDeviceLogin(ctx, "ABCD-1234", u.ID); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("未知用户码: err = %v, 期望 ErrNotFound", err)
	}
	if err := s.ApproveDeviceLogin(ctx, "0O0O0O0O", u.ID); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("含易混字符的用户码: err = %v, 期望 ErrNotFound", err)
	}
	if err := s.ApproveDeviceLogin(ctx, userCode, "usr_missing"); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("未知用户: err = %v, 期望 ErrNotFound", err)
	}
	fs.setUserStatus(u.ID, model.UserStatusDisabled)
	if err := s.ApproveDeviceLogin(ctx, userCode, u.ID); !errors.Is(err, model.ErrDisabled) {
		t.Errorf("禁用用户授权: err = %v, 期望 ErrDisabled", err)
	}
	fs.setUserStatus(u.ID, model.UserStatusActive)

	if err := s.ApproveDeviceLogin(ctx, userCode, u.ID); err != nil {
		t.Fatalf("ApproveDeviceLogin: %v", err)
	}
	if err := s.ApproveDeviceLogin(ctx, userCode, u.ID); !errors.Is(err, model.ErrConflict) {
		t.Errorf("重复授权: err = %v, 期望 ErrConflict", err)
	}

	// 过期分支：新的设备码过期后既不能授权也不能兑换。
	deviceCode2, userCode2, _, err := s.StartDeviceLogin(ctx)
	if err != nil {
		t.Fatalf("StartDeviceLogin: %v", err)
	}
	clock.advance(DefaultDeviceCodeTTL)
	if err := s.ApproveDeviceLogin(ctx, userCode2, u.ID); !errors.Is(err, model.ErrExpired) {
		t.Errorf("过期设备码授权: err = %v, 期望 ErrExpired", err)
	}
	if _, _, err := s.PollDeviceLogin(ctx, deviceCode2); !errors.Is(err, model.ErrExpired) {
		t.Errorf("过期设备码轮询: err = %v, 期望 ErrExpired", err)
	}
	if _, _, err := s.PollDeviceLogin(ctx, "gwd_"+strings.Repeat("a", 64)); !errors.Is(err, model.ErrBadCredentials) {
		t.Errorf("未知设备码: err = %v, 期望 ErrBadCredentials", err)
	}
	if _, _, err := s.PollDeviceLogin(ctx, "bogus"); !errors.Is(err, model.ErrBadCredentials) {
		t.Errorf("无前缀设备码: err = %v, 期望 ErrBadCredentials", err)
	}
}

func TestStartDeviceLoginUserCodeAlphabet(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	s, _ := newTestService(fs, "")

	const rounds = 300
	seen := make(map[string]struct{}, rounds)
	for i := 0; i < rounds; i++ {
		_, userCode, _, err := s.StartDeviceLogin(ctx)
		if err != nil {
			t.Fatalf("StartDeviceLogin(%d): %v", i, err)
		}
		if len(userCode) != userCodeLen {
			t.Fatalf("用户码长度 = %d, 期望 %d: %q", len(userCode), userCodeLen, userCode)
		}
		if strings.ContainsAny(userCode, "01OI") {
			t.Fatalf("用户码含易混字符: %q", userCode)
		}
		for _, r := range userCode {
			if !strings.ContainsRune(userCodeAlphabet, r) {
				t.Fatalf("用户码含非法字符 %q: %q", r, userCode)
			}
		}
		if _, dup := seen[userCode]; dup {
			t.Fatalf("用户码重复: %q", userCode)
		}
		seen[userCode] = struct{}{}
	}
}

func TestStartDeviceLoginRetriesOnUserCodeCollision(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	s, _ := newTestService(fs, "")
	fs.deviceConflicts = 2 // 模拟前两次 user_code 撞车

	if _, userCode, _, err := s.StartDeviceLogin(ctx); err != nil {
		t.Fatalf("StartDeviceLogin 未按预期重试: %v", err)
	} else if !validUserCode(userCode) {
		t.Fatalf("用户码不合法: %q", userCode)
	}
	if got := fs.deviceCount(); got != 1 {
		t.Fatalf("设备码落库数 = %d, 期望 1（撞车应被重试吸收）", got)
	}
}

func TestConcurrentDevicePollOnlyOneWins(t *testing.T) {
	for _, tc := range []struct {
		name string
		make func() Store
	}{
		{"进程内兜底", func() Store { return newFakeStore() }},
		{"store 消费", func() Store { return &fakeConsumerStore{fakeStore: newFakeStore()} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := tc.make()
			s, _ := newTestService(st, "")
			u := mustUser(t, s, "alice", "s3cret-password")

			deviceCode, userCode, _, err := s.StartDeviceLogin(ctx)
			if err != nil {
				t.Fatalf("StartDeviceLogin: %v", err)
			}
			if err := s.ApproveDeviceLogin(ctx, userCode, u.ID); err != nil {
				t.Fatalf("ApproveDeviceLogin: %v", err)
			}

			const n = 8
			type result struct {
				access string
				err    error
			}
			results := make([]result, n)
			start := make(chan struct{})
			var wg sync.WaitGroup
			for i := 0; i < n; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					<-start
					access, _, err := s.PollDeviceLogin(ctx, deviceCode)
					results[i] = result{access: access, err: err}
				}(i)
			}
			close(start)
			wg.Wait()

			success := 0
			for i, r := range results {
				switch {
				case r.err == nil:
					success++
					if _, err := s.VerifyAccess(ctx, r.access); err != nil {
						t.Errorf("获胜者拿到的 access 不可用: %v", err)
					}
				case errors.Is(r.err, model.ErrRevoked):
					// 允许：设备码已被别的请求消费。
				default:
					t.Errorf("goroutine %d: 非预期错误 %v", i, r.err)
				}
			}
			if success != 1 {
				t.Fatalf("并发兑换成功 %d 次, 期望 1 次", success)
			}
			if live := liveSessionCount(st); live != 1 {
				t.Fatalf("活动会话数 = %d, 期望 1", live)
			}
		})
	}
}

func TestDeviceCodeConsumeFailureRevokesSession(t *testing.T) {
	ctx := context.Background()
	fs := &fakeConsumerStore{fakeStore: newFakeStore(), consumeErr: model.ErrConflict}
	s, _ := newTestService(fs, "")
	u := mustUser(t, s, "alice", "s3cret-password")

	deviceCode, userCode, _, err := s.StartDeviceLogin(ctx)
	if err != nil {
		t.Fatalf("StartDeviceLogin: %v", err)
	}
	if err := s.ApproveDeviceLogin(ctx, userCode, u.ID); err != nil {
		t.Fatalf("ApproveDeviceLogin: %v", err)
	}
	if _, _, err := s.PollDeviceLogin(ctx, deviceCode); !errors.Is(err, model.ErrRevoked) {
		t.Fatalf("消费失败: err = %v, 期望 ErrRevoked", err)
	}
	if live := liveSessionCount(fs); live != 0 {
		t.Fatalf("消费失败后残留活动会话 %d 个, 期望 0", live)
	}
}
