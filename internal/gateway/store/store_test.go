package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/model"
	"github.com/ximo888ok-netizen/ximo-agent/internal/storage/migrations"
	"github.com/ximo888ok-netizen/ximo-agent/internal/storage/sqlite"
)

// migrationsDir 指向仓库根 migrations/（本包位于 internal/gateway/store，向上三级）。
var migrationsDir = filepath.Join("..", "..", "..", "migrations")

func newTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gateway.db")
	db, err := sqlite.Open(sqlite.DefaultConfig(path))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := migrations.ApplyFromDir(context.Background(), db, migrationsDir); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	return New(db)
}

func newTestUser(t *testing.T, s *Store, id string) model.User {
	t.Helper()
	u := model.User{
		ID:           id,
		Username:     "user-" + id,
		PasswordHash: "pbkdf2-sha256$210000$c2FsdA==$aGFzaA==",
		Status:       model.UserStatusActive,
		GroupID:      "default",
		CreatedAt:    time.Now().UnixMilli(),
	}
	if err := s.CreateUser(context.Background(), u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	return u
}

func countRows(t *testing.T, s *Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return n
}

func TestMigrationsApplyGatewaySchema(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	var version int
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM migrations`).Scan(&version); err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	if version != 3 {
		t.Fatalf("schema version = %d, want 3 (0003_gateway.sql)", version)
	}

	want := []string{
		"gw_api_keys", "gw_audit", "gw_auth_sessions", "gw_device_codes",
		"gw_models", "gw_provider_models", "gw_providers", "gw_quota_accounts",
		"gw_quota_ledger", "gw_quota_reservations", "gw_usage", "gw_users",
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table' AND name LIKE 'gw_%' ORDER BY name`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	got, err := collect(rows, func(sc scanner) (string, error) {
		var name string
		err := sc.Scan(&name)
		return name, err
	}, "list tables")
	if err != nil {
		t.Fatalf("collect tables: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("tables = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tables = %v, want %v", got, want)
		}
	}
}

func TestUserRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u := newTestUser(t, s, "u1")

	got, err := s.GetUser(ctx, u.ID)
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if got.Username != u.Username || got.Status != model.UserStatusActive || got.GroupID != "default" {
		t.Fatalf("GetUser = %+v, want %+v", got, u)
	}
	byName, err := s.GetUserByName(ctx, u.Username)
	if err != nil {
		t.Fatalf("GetUserByName: %v", err)
	}
	if byName.ID != u.ID {
		t.Errorf("GetUserByName returned %q, want %q", byName.ID, u.ID)
	}
	if got.CreatedAt == 0 || got.UpdatedAt == 0 {
		t.Errorf("timestamps not filled: %+v", got)
	}

	if err := s.CreateUser(ctx, u); !errors.Is(err, model.ErrConflict) {
		t.Fatalf("duplicate CreateUser err = %v, want model.ErrConflict", err)
	}
	if _, err := s.GetUser(ctx, "no-such-user"); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("GetUser(missing) err = %v, want model.ErrNotFound", err)
	}

	if err := s.UpdateUserStatus(ctx, u.ID, model.UserStatusDisabled); err != nil {
		t.Fatalf("UpdateUserStatus: %v", err)
	}
	if got, _ = s.GetUser(ctx, u.ID); got.Status != model.UserStatusDisabled {
		t.Errorf("status = %q, want disabled", got.Status)
	}
	if err := s.UpdateUserStatus(ctx, "no-such-user", model.UserStatusDisabled); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("UpdateUserStatus(missing) err = %v, want model.ErrNotFound", err)
	}

	for i := 0; i < 3; i++ {
		newTestUser(t, s, fmt.Sprintf("page-%d", i))
	}
	page1, err := s.ListUsers(ctx, 2, 0)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(page1) != 2 {
		t.Fatalf("ListUsers(2,0) returned %d, want 2", len(page1))
	}
	page2, err := s.ListUsers(ctx, 2, 2)
	if err != nil {
		t.Fatalf("ListUsers(page2): %v", err)
	}
	if len(page2) == 0 || page2[0].ID == page1[0].ID {
		t.Fatalf("ListUsers pagination overlap: %+v", page2)
	}
	all, err := s.ListUsers(ctx, 0, 0)
	if err != nil {
		t.Fatalf("ListUsers(all): %v", err)
	}
	if len(all) != 4 {
		t.Errorf("ListUsers(all) = %d, want 4", len(all))
	}
}

func TestAPIKeyRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u := newTestUser(t, s, "u1")

	now := time.Now().UnixMilli()
	key := model.APIKey{
		ID: "k1", UserID: u.ID, KeyPrefix: "ximo_sk_ab12",
		KeyHash: "hash-k1", Status: model.KeyStatusActive,
		ExpiresAt: now + 3600_000, CreatedAt: now,
	}
	if err := s.CreateAPIKey(ctx, key); err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	got, err := s.GetAPIKeyByHash(ctx, "hash-k1")
	if err != nil {
		t.Fatalf("GetAPIKeyByHash: %v", err)
	}
	if got.ID != "k1" || got.KeyPrefix != "ximo_sk_ab12" || got.ExpiresAt != key.ExpiresAt || got.LastUsedAt != 0 {
		t.Fatalf("GetAPIKeyByHash = %+v, want %+v", got, key)
	}
	if _, err := s.GetAPIKeyByHash(ctx, "nope"); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("GetAPIKeyByHash(missing) err = %v, want model.ErrNotFound", err)
	}
	if err := s.CreateAPIKey(ctx, model.APIKey{ID: "k2", UserID: u.ID, KeyHash: "hash-k1"}); !errors.Is(err, model.ErrConflict) {
		t.Errorf("duplicate key hash err = %v, want model.ErrConflict", err)
	}

	if err := s.TouchAPIKey(ctx, "k1", now+5); err != nil {
		t.Fatalf("TouchAPIKey: %v", err)
	}
	if got, _ = s.GetAPIKeyByHash(ctx, "hash-k1"); got.LastUsedAt != now+5 {
		t.Errorf("LastUsedAt = %d, want %d", got.LastUsedAt, now+5)
	}
	if err := s.TouchAPIKey(ctx, "missing", now); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("TouchAPIKey(missing) err = %v, want model.ErrNotFound", err)
	}

	if err := s.CreateAPIKey(ctx, model.APIKey{ID: "k2", UserID: u.ID, KeyHash: "hash-k2", CreatedAt: now + 1}); err != nil {
		t.Fatalf("CreateAPIKey(k2): %v", err)
	}
	keys, err := s.ListAPIKeysByUser(ctx, u.ID)
	if err != nil {
		t.Fatalf("ListAPIKeysByUser: %v", err)
	}
	if len(keys) != 2 || keys[0].ID != "k1" || keys[1].ID != "k2" {
		t.Fatalf("ListAPIKeysByUser = %+v, want k1,k2 order", keys)
	}
	if err := s.RevokeAPIKey(ctx, "k1"); err != nil {
		t.Fatalf("RevokeAPIKey: %v", err)
	}
	if got, _ = s.GetAPIKeyByHash(ctx, "hash-k1"); got.Status != model.KeyStatusRevoked {
		t.Errorf("status = %q, want revoked", got.Status)
	}
	if err := s.RevokeAPIKey(ctx, "missing"); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("RevokeAPIKey(missing) err = %v, want model.ErrNotFound", err)
	}
}

func TestAuthSessionRotation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u := newTestUser(t, s, "u1")
	now := time.Now().UnixMilli()

	first := model.AuthSession{
		ID: "s1", UserID: u.ID, AccessHash: "acc-1", RefreshHash: "ref-1",
		AccessExpiresAt: now + 900_000, RefreshExpiresAt: now + 86400_000, CreatedAt: now,
	}
	if err := s.CreateAuthSession(ctx, first); err != nil {
		t.Fatalf("CreateAuthSession: %v", err)
	}
	got, err := s.GetAuthSessionByRefreshHash(ctx, "ref-1")
	if err != nil {
		t.Fatalf("GetAuthSessionByRefreshHash: %v", err)
	}
	if got.ID != "s1" || got.RevokedAt != 0 {
		t.Fatalf("session = %+v, want fresh s1", got)
	}
	if _, err := s.GetAuthSessionByAccessHash(ctx, "acc-1"); err != nil {
		t.Fatalf("GetAuthSessionByAccessHash: %v", err)
	}
	if _, err := s.GetAuthSessionByRefreshHash(ctx, "nope"); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("missing session err = %v, want model.ErrNotFound", err)
	}

	next := model.AuthSession{
		ID: "s2", UserID: u.ID, AccessHash: "acc-2", RefreshHash: "ref-2",
		AccessExpiresAt: now + 1800_000, RefreshExpiresAt: now + 90000_000, CreatedAt: now + 1,
	}
	if err := s.RotateAuthSession(ctx, "s1", next); err != nil {
		t.Fatalf("RotateAuthSession: %v", err)
	}
	old, _ := s.GetAuthSessionByRefreshHash(ctx, "ref-1")
	if old.RevokedAt != now+1 {
		t.Errorf("old session revoked_at = %d, want %d", old.RevokedAt, now+1)
	}
	fresh, err := s.GetAuthSessionByRefreshHash(ctx, "ref-2")
	if err != nil {
		t.Fatalf("rotated session: %v", err)
	}
	if fresh.RotatedFrom != "s1" {
		t.Errorf("rotated_from = %q, want s1", fresh.RotatedFrom)
	}
	if err := s.RotateAuthSession(ctx, "no-such-session", next); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("RotateAuthSession(missing) err = %v, want model.ErrNotFound", err)
	}

	if err := s.RevokeAuthSession(ctx, "s2", now+2); err != nil {
		t.Fatalf("RevokeAuthSession: %v", err)
	}
	// 重复撤销保留首次撤销时间。
	if err := s.RevokeAuthSession(ctx, "s2", now+999); err != nil {
		t.Fatalf("RevokeAuthSession(again): %v", err)
	}
	revoked, _ := s.GetAuthSessionByAccessHash(ctx, "acc-2")
	if revoked.RevokedAt != now+2 {
		t.Errorf("revoked_at = %d, want %d (首次撤销时间)", revoked.RevokedAt, now+2)
	}
}

func TestDeviceCodeFlow(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u := newTestUser(t, s, "u1")
	other := newTestUser(t, s, "u2")
	now := time.Now().UnixMilli()

	d := model.DeviceCode{
		DeviceCodeHash: "dev-hash-1", UserCode: "ABCD1234",
		Status: model.DeviceStatusPending, ExpiresAt: now + 600_000,
		CreatedAt: now, PollIntervalMS: 2000,
	}
	if err := s.CreateDeviceCode(ctx, d); err != nil {
		t.Fatalf("CreateDeviceCode: %v", err)
	}
	got, err := s.GetDeviceCodeByDeviceHash(ctx, "dev-hash-1")
	if err != nil {
		t.Fatalf("GetDeviceCodeByDeviceHash: %v", err)
	}
	if got.UserCode != "ABCD1234" || got.UserID != "" || got.PollIntervalMS != 2000 {
		t.Fatalf("device code = %+v, want pending without user", got)
	}
	if get, err := s.GetDeviceCodeByUserCode(ctx, "ABCD1234"); err != nil || get.DeviceCodeHash != "dev-hash-1" {
		t.Fatalf("GetDeviceCodeByUserCode = %+v, %v", get, err)
	}
	if _, err := s.GetDeviceCodeByUserCode(ctx, "ZZZZ"); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("missing user code err = %v, want model.ErrNotFound", err)
	}
	if err := s.TouchDeviceCodePoll(ctx, "dev-hash-1", now+10); err != nil {
		t.Fatalf("TouchDeviceCodePoll: %v", err)
	}
	if got, _ = s.GetDeviceCodeByDeviceHash(ctx, "dev-hash-1"); got.LastPolledAt != now+10 {
		t.Errorf("LastPolledAt = %d, want %d", got.LastPolledAt, now+10)
	}
	if err := s.TouchDeviceCodePoll(ctx, "missing", now); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("TouchDeviceCodePoll(missing) err = %v, want model.ErrNotFound", err)
	}

	if err := s.ApproveDeviceCode(ctx, "ABCD1234", u.ID, now+20); err != nil {
		t.Fatalf("ApproveDeviceCode: %v", err)
	}
	got, _ = s.GetDeviceCodeByDeviceHash(ctx, "dev-hash-1")
	if got.Status != model.DeviceStatusApproved || got.UserID != u.ID {
		t.Fatalf("approved device code = %+v", got)
	}
	if err := s.ApproveDeviceCode(ctx, "ABCD1234", u.ID, now+21); err != nil {
		t.Errorf("重复批准同一用户应为幂等成功, got %v", err)
	}
	if err := s.ApproveDeviceCode(ctx, "ABCD1234", other.ID, now+22); !errors.Is(err, model.ErrConflict) {
		t.Errorf("换用户批准 err = %v, want model.ErrConflict", err)
	}
	if err := s.ApproveDeviceCode(ctx, "NOPE", u.ID, now); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("ApproveDeviceCode(missing) err = %v, want model.ErrNotFound", err)
	}

	expired := model.DeviceCode{
		DeviceCodeHash: "dev-hash-2", UserCode: "WXYZ9876",
		Status: model.DeviceStatusPending, ExpiresAt: now - 1, CreatedAt: now - 1000,
	}
	if err := s.CreateDeviceCode(ctx, expired); err != nil {
		t.Fatalf("CreateDeviceCode(expired): %v", err)
	}
	if err := s.ApproveDeviceCode(ctx, "WXYZ9876", u.ID, now); !errors.Is(err, model.ErrExpired) {
		t.Errorf("过期批准 err = %v, want model.ErrExpired", err)
	}
}

func TestCatalogRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	m := model.ModelSpec{
		ModelID: "gpt-4o", DisplayName: "GPT-4o",
		CapabilitiesJSON: `{"stream":true,"vision":true,"tools":true,"reasoning":false}`,
		Enabled:          true, CreatedAt: now,
	}
	if err := s.UpsertModel(ctx, m); err != nil {
		t.Fatalf("UpsertModel: %v", err)
	}
	got, err := s.GetModel(ctx, "gpt-4o")
	if err != nil {
		t.Fatalf("GetModel: %v", err)
	}
	if got.DisplayName != "GPT-4o" || !got.Enabled || got.CapabilitiesJSON != m.CapabilitiesJSON {
		t.Fatalf("GetModel = %+v, want %+v", got, m)
	}
	if _, err := s.GetModel(ctx, "nope"); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("GetModel(missing) err = %v, want model.ErrNotFound", err)
	}

	m.DisplayName = "GPT-4o (renamed)"
	m.Enabled = false
	m.UpdatedAt = now + 10
	if err := s.UpsertModel(ctx, m); err != nil {
		t.Fatalf("UpsertModel(update): %v", err)
	}
	got, _ = s.GetModel(ctx, "gpt-4o")
	if got.DisplayName != "GPT-4o (renamed)" || got.Enabled || got.CreatedAt != now {
		t.Fatalf("updated model = %+v, want renamed/disabled with created_at 保留", got)
	}
	if err := s.UpsertModel(ctx, model.ModelSpec{ModelID: "claude-3-5", Enabled: true}); err != nil {
		t.Fatalf("UpsertModel(claude): %v", err)
	}
	all, err := s.ListModels(ctx, false)
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("ListModels(all) = %d, want 2", len(all))
	}
	enabled, err := s.ListModels(ctx, true)
	if err != nil {
		t.Fatalf("ListModels(enabled): %v", err)
	}
	if len(enabled) != 1 || enabled[0].ModelID != "claude-3-5" {
		t.Fatalf("ListModels(enabled) = %+v, want only claude-3-5", enabled)
	}

	p := model.ProviderSpec{
		ID: "p-openrouter", Name: "OpenRouter", Endpoint: "https://openrouter.ai/api",
		Protocol: model.ProtocolOpenAIChat, Status: model.ProviderStatusEnabled,
		APIKeyRef: "secretref:v1:abcd", ConfigJSON: `{"region":"cn"}`,
		TimeoutMS: 30000, Weight: 10, CreatedAt: now,
	}
	if err := s.UpsertProvider(ctx, p); err != nil {
		t.Fatalf("UpsertProvider: %v", err)
	}
	gp, err := s.GetProvider(ctx, "p-openrouter")
	if err != nil {
		t.Fatalf("GetProvider: %v", err)
	}
	if gp.Name != "OpenRouter" || gp.Protocol != model.ProtocolOpenAIChat || gp.APIKeyRef != p.APIKeyRef || gp.TimeoutMS != 30000 {
		t.Fatalf("GetProvider = %+v, want %+v", gp, p)
	}
	if _, err := s.GetProvider(ctx, "nope"); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("GetProvider(missing) err = %v, want model.ErrNotFound", err)
	}
	p.Status = model.ProviderStatusDisabled
	p.UpdatedAt = now + 5
	if err := s.UpsertProvider(ctx, p); err != nil {
		t.Fatalf("UpsertProvider(update): %v", err)
	}
	if err := s.UpsertProvider(ctx, model.ProviderSpec{ID: "p-backup", Name: "Backup", Protocol: model.ProtocolAnthropic}); err != nil {
		t.Fatalf("UpsertProvider(backup): %v", err)
	}
	providers, err := s.ListProviders(ctx, false)
	if err != nil {
		t.Fatalf("ListProviders: %v", err)
	}
	if len(providers) != 2 {
		t.Fatalf("ListProviders(all) = %d, want 2", len(providers))
	}
	enabledProviders, err := s.ListProviders(ctx, true)
	if err != nil {
		t.Fatalf("ListProviders(enabled): %v", err)
	}
	if len(enabledProviders) != 1 || enabledProviders[0].ID != "p-backup" {
		t.Fatalf("ListProviders(enabled) = %+v, want only p-backup", enabledProviders)
	}

	if err := s.UpsertProviderModel(ctx, model.ProviderModel{
		ProviderID: "p-backup", ModelID: "claude-3-5", UpstreamModelID: "anthropic/claude-3.5", Enabled: true, Priority: 2,
	}); err != nil {
		t.Fatalf("UpsertProviderModel: %v", err)
	}
	if err := s.UpsertProviderModel(ctx, model.ProviderModel{
		ProviderID: "p-openrouter", ModelID: "claude-3-5", UpstreamModelID: "anthropic/claude-3.5-sonnet", Enabled: true, Priority: 1,
	}); err != nil {
		t.Fatalf("UpsertProviderModel(2): %v", err)
	}
	// 同主键 upsert 覆盖 upstream_model_id 与优先级。
	if err := s.UpsertProviderModel(ctx, model.ProviderModel{
		ProviderID: "p-openrouter", ModelID: "claude-3-5", UpstreamModelID: "anthropic/claude-3.5-v2", Enabled: false, Priority: 1,
	}); err != nil {
		t.Fatalf("UpsertProviderModel(update): %v", err)
	}
	pms, err := s.ListProviderModels(ctx, "claude-3-5")
	if err != nil {
		t.Fatalf("ListProviderModels: %v", err)
	}
	if len(pms) != 2 {
		t.Fatalf("ListProviderModels = %+v, want 2", pms)
	}
	if pms[0].ProviderID != "p-openrouter" || pms[1].ProviderID != "p-backup" {
		t.Errorf("priority 顺序错误（数值小者优先）: %+v", pms)
	}
	if pms[0].UpstreamModelID != "anthropic/claude-3.5-v2" || pms[0].Enabled {
		t.Errorf("upsert 未覆盖旧值: %+v", pms[0])
	}
	// 未登记的 model 不允许建立映射（外键）。
	if err := s.UpsertProviderModel(ctx, model.ProviderModel{ProviderID: "p-backup", ModelID: "ghost"}); err == nil {
		t.Error("映射到未登记模型应当报错")
	}
}

func TestUsageAndAudit(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u := newTestUser(t, s, "u1")
	now := time.Now().UnixMilli()

	rec := model.UsageRecord{
		RequestID: "req-1", UserID: u.ID, ModelID: "gpt-4o", ProviderID: "p1",
		Status: "ok", InputTokens: 100, OutputTokens: 50, LatencyMS: 1200,
		CostMicro: 42, CreatedAt: now,
	}
	if err := s.InsertUsage(ctx, rec); err != nil {
		t.Fatalf("InsertUsage: %v", err)
	}
	// 同一 request_id 重放视为成功且不覆盖既有行。
	if err := s.InsertUsage(ctx, model.UsageRecord{
		RequestID: "req-1", UserID: u.ID, ModelID: "other", CostMicro: 999, CreatedAt: now + 1,
	}); err != nil {
		t.Fatalf("InsertUsage(replay): %v", err)
	}
	usage, err := s.ListUsage(ctx, u.ID, 0, 0)
	if err != nil {
		t.Fatalf("ListUsage: %v", err)
	}
	if len(usage) != 1 || usage[0].CostMicro != 42 || usage[0].ModelID != "gpt-4o" {
		t.Fatalf("ListUsage = %+v, want 原记录保留", usage)
	}

	if err := s.InsertUsage(ctx, model.UsageRecord{
		RequestID: "req-2", UserID: u.ID, ModelID: "gpt-4o",
		InputTokens: 10, OutputTokens: 5, CostMicro: 8, CreatedAt: now + 100,
	}); err != nil {
		t.Fatalf("InsertUsage(2): %v", err)
	}
	cost, in, out, err := s.SumUsageCost(ctx, u.ID, 0, 0)
	if err != nil {
		t.Fatalf("SumUsageCost: %v", err)
	}
	if cost != 50 || in != 110 || out != 55 {
		t.Errorf("SumUsageCost = (%d,%d,%d), want (50,110,55)", cost, in, out)
	}
	cost, _, _, err = s.SumUsageCost(ctx, u.ID, now+50, 0)
	if err != nil {
		t.Fatalf("SumUsageCost(since): %v", err)
	}
	if cost != 8 {
		t.Errorf("SumUsageCost(since) = %d, want 8", cost)
	}
	cost, _, _, err = s.SumUsageCost(ctx, u.ID, 0, now)
	if err != nil {
		t.Fatalf("SumUsageCost(until): %v", err)
	}
	if cost != 42 {
		t.Errorf("SumUsageCost(until) = %d, want 42", cost)
	}
	if cost, _, _, err = s.SumUsageCost(ctx, "no-such-user", 0, 0); err != nil || cost != 0 {
		t.Errorf("SumUsageCost(unknown user) = %d, %v; want 0, nil", cost, err)
	}
	newest, err := s.ListUsage(ctx, u.ID, 1, 0)
	if err != nil {
		t.Fatalf("ListUsage(limit): %v", err)
	}
	if len(newest) != 1 || newest[0].RequestID != "req-2" {
		t.Errorf("ListUsage 最近优先失败: %+v", newest)
	}

	if err := s.InsertAudit(ctx, model.AuditLog{
		ID: "a1", Actor: "admin", Action: "user.topup", Target: u.ID,
		Result: "ok", IP: "127.0.0.1", DetailJSON: `{"amount":100}`, CreatedAt: now,
	}); err != nil {
		t.Fatalf("InsertAudit: %v", err)
	}
	if err := s.InsertAudit(ctx, model.AuditLog{
		ID: "a2", Actor: "admin", Action: "key.revoke", Target: u.ID, Result: "ok", CreatedAt: now + 1,
	}); err != nil {
		t.Fatalf("InsertAudit(2): %v", err)
	}
	logs, err := s.ListAudit(ctx, 10, 0)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	if len(logs) != 2 || logs[0].ID != "a2" || logs[1].DetailJSON != `{"amount":100}` {
		t.Fatalf("ListAudit = %+v, want 最近优先", logs)
	}
}
