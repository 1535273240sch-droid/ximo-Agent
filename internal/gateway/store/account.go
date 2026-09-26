package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/model"
)

// ---------------------------------------------------------------- gw_users

const userColumns = `id, username, password_hash, status, group_id, created_at, updated_at`

func scanUser(sc scanner) (model.User, error) {
	var u model.User
	err := sc.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Status, &u.GroupID, &u.CreatedAt, &u.UpdatedAt)
	return u, err
}

func normalizeUser(u model.User) model.User {
	if u.Status == "" {
		u.Status = model.UserStatusActive
	}
	if u.CreatedAt == 0 {
		u.CreatedAt = nowMS()
	}
	if u.UpdatedAt == 0 {
		u.UpdatedAt = u.CreatedAt
	}
	return u
}

// CreateUser 在同一个写事务里插入用户行与它的初始额度账户行
// （total/used/reserved 均 0、status=active、version=0）。
//
// 建户即建账户是缺陷 D5 的纵深防御第一层：新用户天然有账户，首次预占走的
// 就是「可用额度为 0 → 额度不足」这条正常路径，而不是「账户行缺失」。
// username 冲突返回 model.ErrConflict，且整个事务回滚（不留无主账户行）。
func (s *Store) CreateUser(ctx context.Context, u model.User) error {
	if u.ID == "" || u.Username == "" {
		return fmt.Errorf("%w: create user requires id and username", ErrInvalidArgument)
	}
	u = normalizeUser(u)
	return s.db.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO gw_users (`+userColumns+`) VALUES (?,?,?,?,?,?,?)`,
			u.ID, u.Username, u.PasswordHash, u.Status, u.GroupID, u.CreatedAt, u.UpdatedAt)
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: username %q already exists", model.ErrConflict, u.Username)
		}
		if err := wrap("create user", err); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO gw_quota_accounts (`+quotaAccountColumns+`) VALUES (?,?,?,?,?,?,?)`,
			u.ID, 0, 0, 0, 0, model.QuotaStatusActive, u.CreatedAt)
		return wrap("create user: quota account", err)
	})
}

// GetUserByName 按用户名查用户；不存在返回 model.ErrNotFound。
func (s *Store) GetUserByName(ctx context.Context, username string) (model.User, error) {
	u, err := scanUser(s.db.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM gw_users WHERE username=?`, username))
	if err != nil {
		return model.User{}, wrapNotFound("get user by name", err)
	}
	return u, nil
}

// GetUser 按 ID 查用户；不存在返回 model.ErrNotFound。
func (s *Store) GetUser(ctx context.Context, id string) (model.User, error) {
	u, err := scanUser(s.db.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM gw_users WHERE id=?`, id))
	if err != nil {
		return model.User{}, wrapNotFound("get user", err)
	}
	return u, nil
}

// ListUsers 按创建时间正序分页列出用户。
func (s *Store) ListUsers(ctx context.Context, limit, offset int) ([]model.User, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+userColumns+` FROM gw_users ORDER BY created_at ASC, id ASC LIMIT ? OFFSET ?`,
		pageLimit(limit), pageOffset(offset))
	if err != nil {
		return nil, wrap("list users", err)
	}
	return collect(rows, scanUser, "list users")
}

// UpdateUserStatus 改用户状态（active|disabled）；用户不存在返回 ErrNotFound。
func (s *Store) UpdateUserStatus(ctx context.Context, id, status string) error {
	if id == "" || status == "" {
		return fmt.Errorf("%w: update user status requires id and status", ErrInvalidArgument)
	}
	return s.db.WithTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE gw_users SET status=?, updated_at=? WHERE id=?`,
			status, nowMS(), id)
		if err != nil {
			return wrap("update user status", err)
		}
		return requireAffected(res, "user "+id)
	})
}

// ---------------------------------------------------------------- gw_api_keys

const apiKeyColumns = `id, user_id, key_prefix, key_hash, status, expires_at, created_at, last_used_at`

func scanAPIKey(sc scanner) (model.APIKey, error) {
	var k model.APIKey
	err := sc.Scan(&k.ID, &k.UserID, &k.KeyPrefix, &k.KeyHash, &k.Status,
		&k.ExpiresAt, &k.CreatedAt, &k.LastUsedAt)
	return k, err
}

func normalizeAPIKey(k model.APIKey) model.APIKey {
	if k.Status == "" {
		k.Status = model.KeyStatusActive
	}
	if k.CreatedAt == 0 {
		k.CreatedAt = nowMS()
	}
	return k
}

// CreateAPIKey 插入密钥行（只存哈希），key_hash 冲突返回 model.ErrConflict。
func (s *Store) CreateAPIKey(ctx context.Context, k model.APIKey) error {
	if k.ID == "" || k.UserID == "" || k.KeyHash == "" {
		return fmt.Errorf("%w: create api key requires id, user_id and key_hash", ErrInvalidArgument)
	}
	k = normalizeAPIKey(k)
	return s.db.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO gw_api_keys (`+apiKeyColumns+`) VALUES (?,?,?,?,?,?,?,?)`,
			k.ID, k.UserID, k.KeyPrefix, k.KeyHash, k.Status, k.ExpiresAt, k.CreatedAt, k.LastUsedAt)
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: api key hash already exists", model.ErrConflict)
		}
		return wrap("create api key", err)
	})
}

// GetAPIKeyByHash 按哈希查密钥。撤销/过期的行也会返回，由调用方判定状态。
func (s *Store) GetAPIKeyByHash(ctx context.Context, hash string) (model.APIKey, error) {
	k, err := scanAPIKey(s.db.QueryRowContext(ctx,
		`SELECT `+apiKeyColumns+` FROM gw_api_keys WHERE key_hash=?`, hash))
	if err != nil {
		return model.APIKey{}, wrapNotFound("get api key by hash", err)
	}
	return k, nil
}

// ListAPIKeysByUser 列出某用户的全部密钥（含已撤销，按创建时间正序）。
func (s *Store) ListAPIKeysByUser(ctx context.Context, userID string) ([]model.APIKey, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+apiKeyColumns+` FROM gw_api_keys WHERE user_id=? ORDER BY created_at ASC, id ASC`, userID)
	if err != nil {
		return nil, wrap("list api keys", err)
	}
	return collect(rows, scanAPIKey, "list api keys")
}

// RevokeAPIKey 撤销密钥（重复撤销幂等）；不存在返回 model.ErrNotFound。
func (s *Store) RevokeAPIKey(ctx context.Context, id string) error {
	return s.db.WithTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE gw_api_keys SET status=? WHERE id=?`,
			model.KeyStatusRevoked, id)
		if err != nil {
			return wrap("revoke api key", err)
		}
		return requireAffected(res, "api key "+id)
	})
}

// TouchAPIKey 记录最近使用时间（at<=0 时取当前时间）。
func (s *Store) TouchAPIKey(ctx context.Context, id string, at int64) error {
	if at <= 0 {
		at = nowMS()
	}
	return s.db.WithTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE gw_api_keys SET last_used_at=? WHERE id=?`, at, id)
		if err != nil {
			return wrap("touch api key", err)
		}
		return requireAffected(res, "api key "+id)
	})
}

// ---------------------------------------------------------------- gw_auth_sessions

const authSessionColumns = `id, user_id, access_hash, refresh_hash, rotated_from,
	access_expires_at, refresh_expires_at, revoked_at, created_at`

func scanAuthSession(sc scanner) (model.AuthSession, error) {
	var a model.AuthSession
	err := sc.Scan(&a.ID, &a.UserID, &a.AccessHash, &a.RefreshHash, &a.RotatedFrom,
		&a.AccessExpiresAt, &a.RefreshExpiresAt, &a.RevokedAt, &a.CreatedAt)
	return a, err
}

func normalizeAuthSession(a model.AuthSession) model.AuthSession {
	if a.CreatedAt == 0 {
		a.CreatedAt = nowMS()
	}
	return a
}

// CreateAuthSession 插入会话（登录/设备码换发后的首次落库）。
func (s *Store) CreateAuthSession(ctx context.Context, a model.AuthSession) error {
	if a.ID == "" || a.UserID == "" || a.AccessHash == "" || a.RefreshHash == "" {
		return fmt.Errorf("%w: create auth session requires id, user_id, access_hash and refresh_hash", ErrInvalidArgument)
	}
	a = normalizeAuthSession(a)
	return s.db.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO gw_auth_sessions (`+authSessionColumns+`) VALUES (?,?,?,?,?,?,?,?,?)`,
			a.ID, a.UserID, a.AccessHash, a.RefreshHash, a.RotatedFrom,
			a.AccessExpiresAt, a.RefreshExpiresAt, a.RevokedAt, a.CreatedAt)
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: auth session %s already exists", model.ErrConflict, a.ID)
		}
		return wrap("create auth session", err)
	})
}

// GetAuthSessionByRefreshHash 按 refresh 哈希查最新一行。已撤销的行同样返回
// （调用方需要区分“不存在”与“已撤销”，故这里不过滤）。
func (s *Store) GetAuthSessionByRefreshHash(ctx context.Context, hash string) (model.AuthSession, error) {
	return s.getAuthSession(ctx, "refresh_hash", hash)
}

// GetAuthSessionByAccessHash 按 access 哈希查最新一行。
func (s *Store) GetAuthSessionByAccessHash(ctx context.Context, hash string) (model.AuthSession, error) {
	return s.getAuthSession(ctx, "access_hash", hash)
}

func (s *Store) getAuthSession(ctx context.Context, column, hash string) (model.AuthSession, error) {
	if hash == "" {
		return model.AuthSession{}, fmt.Errorf("%w: empty session hash", ErrInvalidArgument)
	}
	a, err := scanAuthSession(s.db.QueryRowContext(ctx,
		`SELECT `+authSessionColumns+` FROM gw_auth_sessions WHERE `+column+`=?
		 ORDER BY created_at DESC, id DESC LIMIT 1`, hash))
	if err != nil {
		return model.AuthSession{}, wrapNotFound("get auth session by "+column, err)
	}
	return a, nil
}

// RotateAuthSession 在一个事务里把旧会话 RevokedAt 置位并插入新会话。
// oldID 为空表示不轮换旧行（首次签发）；旧行不存在返回 model.ErrNotFound。
func (s *Store) RotateAuthSession(ctx context.Context, oldID string, next model.AuthSession) error {
	if next.ID == "" || next.UserID == "" || next.AccessHash == "" || next.RefreshHash == "" {
		return fmt.Errorf("%w: rotate requires id, user_id, access_hash and refresh_hash", ErrInvalidArgument)
	}
	next = normalizeAuthSession(next)
	if next.RotatedFrom == "" {
		next.RotatedFrom = oldID
	}
	return s.db.WithTx(ctx, func(tx *sql.Tx) error {
		if oldID != "" {
			// 保留首次撤销时间，避免重复轮换把 revoked_at 往后推。
			res, err := tx.ExecContext(ctx,
				`UPDATE gw_auth_sessions SET revoked_at = CASE WHEN revoked_at=0 THEN ? ELSE revoked_at END WHERE id=?`,
				next.CreatedAt, oldID)
			if err != nil {
				return wrap("rotate auth session: revoke old", err)
			}
			if err := requireAffected(res, "auth session "+oldID); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO gw_auth_sessions (`+authSessionColumns+`) VALUES (?,?,?,?,?,?,?,?,?)`,
			next.ID, next.UserID, next.AccessHash, next.RefreshHash, next.RotatedFrom,
			next.AccessExpiresAt, next.RefreshExpiresAt, next.RevokedAt, next.CreatedAt)
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: auth session %s already exists", model.ErrConflict, next.ID)
		}
		return wrap("rotate auth session: insert next", err)
	})
}

// RevokeAuthSession 撤销会话（重复撤销幂等，保留首次撤销时间）。
func (s *Store) RevokeAuthSession(ctx context.Context, id string, at int64) error {
	if at <= 0 {
		at = nowMS()
	}
	return s.db.WithTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE gw_auth_sessions SET revoked_at = CASE WHEN revoked_at=0 THEN ? ELSE revoked_at END WHERE id=?`,
			at, id)
		if err != nil {
			return wrap("revoke auth session", err)
		}
		return requireAffected(res, "auth session "+id)
	})
}

// ---------------------------------------------------------------- gw_device_codes

const deviceCodeColumns = `device_code_hash, user_code, user_id, status,
	expires_at, created_at, last_polled_at, poll_interval_ms`

func scanDeviceCode(sc scanner) (model.DeviceCode, error) {
	var d model.DeviceCode
	var userID sql.NullString
	err := sc.Scan(&d.DeviceCodeHash, &d.UserCode, &userID, &d.Status,
		&d.ExpiresAt, &d.CreatedAt, &d.LastPolledAt, &d.PollIntervalMS)
	d.UserID = scanNullStr(userID)
	return d, err
}

func normalizeDeviceCode(d model.DeviceCode) model.DeviceCode {
	if d.Status == "" {
		d.Status = model.DeviceStatusPending
	}
	if d.CreatedAt == 0 {
		d.CreatedAt = nowMS()
	}
	return d
}

// CreateDeviceCode 插入设备码（设备码哈希为 PK，user_code 唯一）。
func (s *Store) CreateDeviceCode(ctx context.Context, d model.DeviceCode) error {
	if d.DeviceCodeHash == "" || d.UserCode == "" {
		return fmt.Errorf("%w: create device code requires device_code_hash and user_code", ErrInvalidArgument)
	}
	d = normalizeDeviceCode(d)
	return s.db.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO gw_device_codes (`+deviceCodeColumns+`) VALUES (?,?,?,?,?,?,?,?)`,
			d.DeviceCodeHash, d.UserCode, nullStr(d.UserID), d.Status,
			d.ExpiresAt, d.CreatedAt, d.LastPolledAt, d.PollIntervalMS)
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: device code already exists", model.ErrConflict)
		}
		return wrap("create device code", err)
	})
}

// GetDeviceCodeByDeviceHash 按设备码哈希查（设备轮询用）。
func (s *Store) GetDeviceCodeByDeviceHash(ctx context.Context, hash string) (model.DeviceCode, error) {
	d, err := scanDeviceCode(s.db.QueryRowContext(ctx,
		`SELECT `+deviceCodeColumns+` FROM gw_device_codes WHERE device_code_hash=?`, hash))
	if err != nil {
		return model.DeviceCode{}, wrapNotFound("get device code by hash", err)
	}
	return d, nil
}

// GetDeviceCodeByUserCode 按人工核对码查（浏览器侧批准用）。
func (s *Store) GetDeviceCodeByUserCode(ctx context.Context, userCode string) (model.DeviceCode, error) {
	d, err := scanDeviceCode(s.db.QueryRowContext(ctx,
		`SELECT `+deviceCodeColumns+` FROM gw_device_codes WHERE user_code=?`, userCode))
	if err != nil {
		return model.DeviceCode{}, wrapNotFound("get device code by user code", err)
	}
	return d, nil
}

// ApproveDeviceCode 批准设备码：绑定 user_id 并把状态置为 approved。
// 已过期返回 model.ErrExpired；已批准且用户一致视为幂等成功，用户不一致或
// 状态不可批准返回 model.ErrConflict。
func (s *Store) ApproveDeviceCode(ctx context.Context, userCode, userID string, at int64) error {
	if userCode == "" || userID == "" {
		return fmt.Errorf("%w: approve device code requires user_code and user_id", ErrInvalidArgument)
	}
	if at <= 0 {
		at = nowMS()
	}
	return s.db.WithTx(ctx, func(tx *sql.Tx) error {
		d, err := scanDeviceCode(tx.QueryRowContext(ctx,
			`SELECT `+deviceCodeColumns+` FROM gw_device_codes WHERE user_code=?`, userCode))
		if err != nil {
			return wrapNotFound("approve device code", err)
		}
		if d.ExpiresAt > 0 && d.ExpiresAt <= at {
			return fmt.Errorf("%w: device code %s expired at %d", model.ErrExpired, d.UserCode, d.ExpiresAt)
		}
		switch d.Status {
		case model.DeviceStatusApproved:
			if d.UserID == userID {
				return nil
			}
			return fmt.Errorf("%w: device code %s already approved by another user", model.ErrConflict, d.UserCode)
		case model.DeviceStatusPending:
			// 正常路径。
		default:
			return fmt.Errorf("%w: device code %s is %s", model.ErrConflict, d.UserCode, d.Status)
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE gw_device_codes SET user_id=?, status=?, last_polled_at=? WHERE user_code=?`,
			userID, model.DeviceStatusApproved, at, userCode)
		return wrap("approve device code: update", err)
	})
}

// TouchDeviceCodePoll 记录最近一次轮询时间；设备码不存在返回 ErrNotFound。
func (s *Store) TouchDeviceCodePoll(ctx context.Context, hash string, at int64) error {
	if at <= 0 {
		at = nowMS()
	}
	return s.db.WithTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE gw_device_codes SET last_polled_at=? WHERE device_code_hash=?`, at, hash)
		if err != nil {
			return wrap("touch device code poll", err)
		}
		return requireAffected(res, "device code")
	})
}

// ConsumeDeviceCode 把**已批准**的设备码一次性消费掉（approved → consumed）。
//
// 单写事务内做条件更新，只有 status='approved' 的行会被改到，因此并发/重复
// 兑换最多只有一个调用者拿到成功（契约 §11.1.3；account.DeviceCodeConsumer）。
// 影响 0 行时在同一事务内回查该行以区分原因：
//   - 行不存在 → model.ErrNotFound；
//   - 行存在但状态不是 approved（pending / consumed / expired）→ model.ErrConflict。
//
// 本方法只管状态机的一次性消费，不做 expires_at 判定：过期语义由 account 层
// 在轮询时按 ExpiresAt 判定（与本包其余设备码方法一致）。
func (s *Store) ConsumeDeviceCode(ctx context.Context, deviceCodeHash string, at int64) error {
	if deviceCodeHash == "" {
		return fmt.Errorf("%w: consume device code requires device_code_hash", ErrInvalidArgument)
	}
	if at <= 0 {
		at = nowMS()
	}
	return s.db.WithTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE gw_device_codes SET status=?, last_polled_at=? WHERE device_code_hash=? AND status=?`,
			model.DeviceStatusConsumed, at, deviceCodeHash, model.DeviceStatusApproved)
		if err != nil {
			return wrap("consume device code", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return wrap("consume device code: rows affected", err)
		}
		if n > 0 {
			return nil
		}
		var status string
		err = tx.QueryRowContext(ctx,
			`SELECT status FROM gw_device_codes WHERE device_code_hash=?`, deviceCodeHash).Scan(&status)
		switch {
		case isNoRows(err):
			return fmt.Errorf("%w: device code not found", model.ErrNotFound)
		case err != nil:
			return wrap("consume device code: reload status", err)
		default:
			return fmt.Errorf("%w: device code is %s", model.ErrConflict, status)
		}
	})
}

// requireAffected 把 0 行更新映射为 model.ErrNotFound。
func requireAffected(res sql.Result, what string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return wrap("rows affected", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", model.ErrNotFound, what)
	}
	return nil
}
