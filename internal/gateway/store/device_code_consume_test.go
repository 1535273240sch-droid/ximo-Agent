package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/model"
)

// newTestDeviceCode 插入一个指定状态的设备码。
func newTestDeviceCode(t *testing.T, s *Store, hash, userCode, status string, expiresAt int64) model.DeviceCode {
	t.Helper()
	d := model.DeviceCode{
		DeviceCodeHash: hash, UserCode: userCode, Status: status,
		ExpiresAt: expiresAt, CreatedAt: time.Now().UnixMilli(), PollIntervalMS: 2000,
	}
	if err := s.CreateDeviceCode(context.Background(), d); err != nil {
		t.Fatalf("CreateDeviceCode(%s): %v", hash, err)
	}
	return d
}

func TestConsumeDeviceCodeOnceOnly(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u := newTestUser(t, s, "u1")
	now := time.Now().UnixMilli()

	newTestDeviceCode(t, s, "dev-hash-1", "ABCD1234", model.DeviceStatusPending, now+600_000)
	if err := s.ApproveDeviceCode(ctx, "ABCD1234", u.ID, now+10); err != nil {
		t.Fatalf("ApproveDeviceCode: %v", err)
	}

	if err := s.ConsumeDeviceCode(ctx, "dev-hash-1", now+20); err != nil {
		t.Fatalf("ConsumeDeviceCode(approved): %v", err)
	}
	got, err := s.GetDeviceCodeByDeviceHash(ctx, "dev-hash-1")
	if err != nil {
		t.Fatalf("GetDeviceCodeByDeviceHash: %v", err)
	}
	if got.Status != model.DeviceStatusConsumed {
		t.Errorf("status = %q, want %q", got.Status, model.DeviceStatusConsumed)
	}
	if got.LastPolledAt != now+20 {
		t.Errorf("LastPolledAt = %d, want %d", got.LastPolledAt, now+20)
	}
	if got.UserID != u.ID {
		t.Errorf("UserID = %q, want %q（消费不得改动绑定用户）", got.UserID, u.ID)
	}

	// 第二次消费必须失败，且不得把 last_polled_at 再往后推。
	if err := s.ConsumeDeviceCode(ctx, "dev-hash-1", now+30); !errors.Is(err, model.ErrConflict) {
		t.Fatalf("重复消费 err = %v, want model.ErrConflict", err)
	}
	got, _ = s.GetDeviceCodeByDeviceHash(ctx, "dev-hash-1")
	if got.LastPolledAt != now+20 {
		t.Errorf("重复消费改动了 LastPolledAt = %d, want %d", got.LastPolledAt, now+20)
	}
}

func TestConsumeDeviceCodeRequiresApproved(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	newTestDeviceCode(t, s, "dev-pending", "PEND1234", model.DeviceStatusPending, now+600_000)
	if err := s.ConsumeDeviceCode(ctx, "dev-pending", now); !errors.Is(err, model.ErrConflict) {
		t.Errorf("消费 pending err = %v, want model.ErrConflict", err)
	}
	if got, _ := s.GetDeviceCodeByDeviceHash(ctx, "dev-pending"); got.Status != model.DeviceStatusPending {
		t.Errorf("pending 被改动为 %q", got.Status)
	}

	newTestDeviceCode(t, s, "dev-consumed", "CONS1234", model.DeviceStatusConsumed, now+600_000)
	if err := s.ConsumeDeviceCode(ctx, "dev-consumed", now); !errors.Is(err, model.ErrConflict) {
		t.Errorf("消费 consumed err = %v, want model.ErrConflict", err)
	}

	newTestDeviceCode(t, s, "dev-expired", "EXPD1234", model.DeviceStatusExpired, now+600_000)
	if err := s.ConsumeDeviceCode(ctx, "dev-expired", now); !errors.Is(err, model.ErrConflict) {
		t.Errorf("消费 expired 状态 err = %v, want model.ErrConflict", err)
	}
}

func TestConsumeDeviceCodeMissingAndInvalid(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.ConsumeDeviceCode(ctx, "no-such-device-code", 0); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("不存在的设备码 err = %v, want model.ErrNotFound", err)
	}
	if err := s.ConsumeDeviceCode(ctx, "", 0); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("空哈希 err = %v, want ErrInvalidArgument", err)
	}

	// at<=0 取当前时间，不写 0。
	u := newTestUser(t, s, "u-at")
	now := time.Now().UnixMilli()
	newTestDeviceCode(t, s, "dev-at", "ATAT1234", model.DeviceStatusPending, now+600_000)
	if err := s.ApproveDeviceCode(ctx, "ATAT1234", u.ID, now); err != nil {
		t.Fatalf("ApproveDeviceCode: %v", err)
	}
	if err := s.ConsumeDeviceCode(ctx, "dev-at", 0); err != nil {
		t.Fatalf("ConsumeDeviceCode(at=0): %v", err)
	}
	if got, _ := s.GetDeviceCodeByDeviceHash(ctx, "dev-at"); got.LastPolledAt <= 0 {
		t.Errorf("LastPolledAt = %d, want > 0（at<=0 应取当前时间）", got.LastPolledAt)
	}
}

func TestConsumeDeviceCodeConcurrentSingleWinner(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u := newTestUser(t, s, "u-race")
	now := time.Now().UnixMilli()

	newTestDeviceCode(t, s, "dev-race", "RACE1234", model.DeviceStatusPending, now+600_000)
	if err := s.ApproveDeviceCode(ctx, "RACE1234", u.ID, now); err != nil {
		t.Fatalf("ApproveDeviceCode: %v", err)
	}

	const workers = 8
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		ok   int
		bad  []string
		errs = map[string]int{}
	)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := s.ConsumeDeviceCode(ctx, "dev-race", now+int64(i)+1)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, model.ErrConflict):
				errs["conflict"]++
			default:
				bad = append(bad, fmt.Sprintf("worker %d: %v", i, err))
			}
		}(i)
	}
	wg.Wait()

	if len(bad) > 0 {
		t.Fatalf("并发消费出现意外错误: %v", bad)
	}
	if ok != 1 {
		t.Fatalf("成功次数 = %d, want 1（设备码只能兑换一次），conflict=%d", ok, errs["conflict"])
	}
	if errs["conflict"] != workers-1 {
		t.Errorf("conflict 次数 = %d, want %d", errs["conflict"], workers-1)
	}
	if got, _ := s.GetDeviceCodeByDeviceHash(ctx, "dev-race"); got.Status != model.DeviceStatusConsumed {
		t.Errorf("status = %q, want consumed", got.Status)
	}
}
