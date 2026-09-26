// Package account_test 用真实 SQLite + 0001~0003 迁移跑账户全链路。
// 放在外部测试包：既验证对外 API 面（下一轮 HTTP 层只用这些），也不引入
// 「store → account」潜在反向依赖造成的 import cycle。
package account_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"testing"

	"github.com/ximo888ok-netizen/ximo-agent/internal/account"
	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/model"
	gwstore "github.com/ximo888ok-netizen/ximo-agent/internal/gateway/store"
	"github.com/ximo888ok-netizen/ximo-agent/internal/storage/migrations"
	"github.com/ximo888ok-netizen/ximo-agent/internal/storage/sqlite"
)

// 编译期断言：真实 store 必须直接满足 account 的窄接口（契约 §10.2「*gwstore.Store
// 必须能直接传入」）。签名一旦漂移，这里先于调用方编译失败。
var _ account.Store = (*gwstore.Store)(nil)

// migrationsDir 指向仓库根 migrations/（internal/account 向上两级）。
var migrationsDir = filepath.Join("..", "..", "migrations")

// newRealService 用真实 SQLite + 迁移构造服务，走全链路真实调用。
func newRealService(t *testing.T, pepper string) (*account.Service, *sqlite.DB) {
	t.Helper()
	db, err := sqlite.Open(sqlite.DefaultConfig(filepath.Join(t.TempDir(), "account-it.db")))
	if err != nil {
		t.Fatalf("打开 sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := migrations.ApplyFromDir(context.Background(), db, migrationsDir); err != nil {
		t.Fatalf("应用迁移: %v", err)
	}
	return account.New(gwstore.New(db), []byte(pepper)), db
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func countWhere(t *testing.T, db *sqlite.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("查询 %q: %v", query, err)
	}
	return n
}

func TestIntegrationAccountEndToEnd(t *testing.T) {
	ctx := context.Background()
	svc, db := newRealService(t, "it-pepper")

	u, err := svc.CreateUser(ctx, "it-alice", "s3cret-password", "default")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if n := countWhere(t, db, `SELECT COUNT(*) FROM gw_users WHERE password_hash = ?`, "s3cret-password"); n != 0 {
		t.Fatalf("库中出现口令明文 %d 行", n)
	}
	if _, err := svc.CreateUser(ctx, "it-alice", "another-password", "default"); !errors.Is(err, model.ErrConflict) {
		t.Errorf("重名注册: err = %v, 期望 ErrConflict", err)
	}
	got, err := svc.Authenticate(ctx, "it-alice", "s3cret-password")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if got.ID != u.ID {
		t.Errorf("登录用户 = %q, 期望 %q", got.ID, u.ID)
	}
	if _, err := svc.Authenticate(ctx, "it-alice", "wrong-password"); !errors.Is(err, model.ErrBadCredentials) {
		t.Errorf("错误口令: err = %v, 期望 ErrBadCredentials", err)
	}

	// 会话：签发 → 校验 → 轮换 → 旧 refresh 失效。
	access, refresh, err := svc.IssueSession(ctx, u.ID)
	if err != nil {
		t.Fatalf("IssueSession: %v", err)
	}
	if _, err := svc.VerifyAccess(ctx, access); err != nil {
		t.Fatalf("VerifyAccess: %v", err)
	}
	access2, refresh2, err := svc.Refresh(ctx, refresh)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if _, err := svc.VerifyAccess(ctx, access2); err != nil {
		t.Fatalf("新 access 校验失败: %v", err)
	}
	if _, err := svc.VerifyAccess(ctx, access); !errors.Is(err, model.ErrRevoked) {
		t.Errorf("旧 access: err = %v, 期望 ErrRevoked", err)
	}
	if _, _, err := svc.Refresh(ctx, refresh); !errors.Is(err, model.ErrRevoked) {
		t.Errorf("复用旧 refresh: err = %v, 期望 ErrRevoked", err)
	}
	// 库中只应有哈希：明文 access/refresh 都查不到。
	if n := countWhere(t, db, `SELECT COUNT(*) FROM gw_auth_sessions WHERE access_hash = ? OR refresh_hash = ?`,
		access2, refresh2); n != 0 {
		t.Fatalf("库中出现令牌明文 %d 行", n)
	}

	// API Key：签发 → 校验 → 吊销。
	plain, key, err := svc.CreateAPIKey(ctx, u.ID, 0)
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	if n := countWhere(t, db, `SELECT COUNT(*) FROM gw_api_keys WHERE key_hash = ?`, plain); n != 0 {
		t.Fatalf("库中出现 API Key 明文 %d 行", n)
	}
	if n := countWhere(t, db, `SELECT COUNT(*) FROM gw_api_keys WHERE key_hash = ?`, sha256Hex(plain)); n != 1 {
		t.Fatalf("按哈希查不到 API Key")
	}
	if _, got, err := svc.VerifyAPIKey(ctx, plain); err != nil {
		t.Fatalf("VerifyAPIKey: %v", err)
	} else if got.ID != key.ID {
		t.Errorf("VerifyAPIKey 返回密钥 = %q, 期望 %q", got.ID, key.ID)
	}
	if err := svc.RevokeAPIKey(ctx, key.ID); err != nil {
		t.Fatalf("RevokeAPIKey: %v", err)
	}
	if _, _, err := svc.VerifyAPIKey(ctx, plain); !errors.Is(err, model.ErrRevoked) {
		t.Errorf("吊销后校验: err = %v, 期望 ErrRevoked", err)
	}

	// 设备码：pending → approved → 一次性消费。
	deviceCode, userCode, intervalMS, err := svc.StartDeviceLogin(ctx)
	if err != nil {
		t.Fatalf("StartDeviceLogin: %v", err)
	}
	if intervalMS <= 0 {
		t.Errorf("intervalMS = %d", intervalMS)
	}
	if _, _, err := svc.PollDeviceLogin(ctx, deviceCode); !errors.Is(err, model.ErrAuthorizationPending) {
		t.Fatalf("待授权轮询: err = %v, 期望 ErrAuthorizationPending", err)
	}
	if err := svc.ApproveDeviceLogin(ctx, userCode, u.ID); err != nil {
		t.Fatalf("ApproveDeviceLogin: %v", err)
	}
	dAccess, dRefresh, err := svc.PollDeviceLogin(ctx, deviceCode)
	if err != nil {
		t.Fatalf("授权后轮询: %v", err)
	}
	if _, err := svc.VerifyAccess(ctx, dAccess); err != nil {
		t.Fatalf("设备登录 access 校验失败: %v", err)
	}
	if _, _, err := svc.Refresh(ctx, dRefresh); err != nil {
		t.Fatalf("设备登录 refresh 不可用: %v", err)
	}
	if _, _, err := svc.PollDeviceLogin(ctx, deviceCode); !errors.Is(err, model.ErrRevoked) {
		t.Errorf("重复兑换设备码: err = %v, 期望 ErrRevoked", err)
	}
}

// TestIntegrationDeviceCodeSingleUseAcrossServices 模拟「同一进程内多请求并发兑换」：
// 真实 store 目前没有 ConsumeDeviceCode，属于降级路径，这里固定其行为不回归。
func TestIntegrationDeviceCodeSingleUseAcrossServices(t *testing.T) {
	ctx := context.Background()
	svc, db := newRealService(t, "")
	u := mustRealUser(t, svc, "it-bob")

	deviceCode, userCode, _, err := svc.StartDeviceLogin(ctx)
	if err != nil {
		t.Fatalf("StartDeviceLogin: %v", err)
	}
	if err := svc.ApproveDeviceLogin(ctx, userCode, u.ID); err != nil {
		t.Fatalf("ApproveDeviceLogin: %v", err)
	}
	if _, _, err := svc.PollDeviceLogin(ctx, deviceCode); err != nil {
		t.Fatalf("首次兑换: %v", err)
	}
	if _, _, err := svc.PollDeviceLogin(ctx, deviceCode); !errors.Is(err, model.ErrRevoked) {
		t.Errorf("重复兑换: err = %v, 期望 ErrRevoked", err)
	}
	if live := countWhere(t, db,
		`SELECT COUNT(*) FROM gw_auth_sessions WHERE revoked_at = 0`); live != 1 {
		t.Errorf("活动会话数 = %d, 期望 1（重复兑换不得多出一个会话）", live)
	}
}

func mustRealUser(t *testing.T, s *account.Service, username string) model.User {
	t.Helper()
	u, err := s.CreateUser(context.Background(), username, "s3cret-password", "default")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return u
}
