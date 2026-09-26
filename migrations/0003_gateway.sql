-- 0003_gateway.sql — XIMO 中转站（gateway）schema：账号 / 额度账本 / 模型目录 / 用量 / 审计
--
-- 约定与 0001_init.sql 一致：
--   1. 所有时间戳为 unix 毫秒 INTEGER（0 表示“无 / 未用”）。
--   2. 所有 ID 为 TEXT，由应用层生成。
--   3. 外键全部显式声明并配 ON DELETE 行为；连接层 PRAGMA foreign_keys=ON。
--   4. 索引显式创建。
--   5. 金额一律 INTEGER 微单位（1e-6 credit），禁止浮点。
--
-- 与 0001 的两点偏差（列类型由 internal/gateway/model 的冻结结构体决定）：
--   - capabilities_json / config_json / detail_json 用 TEXT 而非 BLOB：
--     model.ModelSpec.CapabilitiesJSON 等字段是 string，写 TEXT 避免
--     string↔[]byte 的混用与额外编解码。
--   - 表名前缀 gw_：网关域与 Agent 运行时表隔离，便于独立迁移与权限收敛。

-- ---------------------------------------------------------------- gw_users
-- 登录主体。password_hash 形如 "pbkdf2-sha256$<iter>$<saltB64>$<hashB64>"，
-- 绝不存明文口令。
CREATE TABLE gw_users (
    id            TEXT PRIMARY KEY,
    username      TEXT    NOT NULL UNIQUE,
    password_hash TEXT    NOT NULL,
    status        TEXT    NOT NULL DEFAULT 'active',
    group_id      TEXT    NOT NULL DEFAULT '',
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL
);
CREATE INDEX idx_gw_users_status ON gw_users(status, created_at);
CREATE INDEX idx_gw_users_group  ON gw_users(group_id, created_at);

-- ---------------------------------------------------------------- gw_api_keys
-- API Key 只存 sha256 哈希（key_hash UNIQUE 是“一个明文只对应一行”的防线），
-- key_prefix 仅用于列表展示与人工识别。
CREATE TABLE gw_api_keys (
    id           TEXT PRIMARY KEY,
    user_id      TEXT    NOT NULL REFERENCES gw_users(id) ON DELETE CASCADE,
    key_prefix   TEXT    NOT NULL DEFAULT '',
    key_hash     TEXT    NOT NULL UNIQUE,
    status       TEXT    NOT NULL DEFAULT 'active',
    expires_at   INTEGER NOT NULL DEFAULT 0,
    created_at   INTEGER NOT NULL,
    last_used_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_gw_api_keys_user   ON gw_api_keys(user_id, created_at);
CREATE INDEX idx_gw_api_keys_status ON gw_api_keys(status, expires_at);

-- ---------------------------------------------------------------- gw_auth_sessions
-- 不透明令牌会话（access/refresh 均为 sha256 十六进制）。access_hash 与
-- refresh_hash 各建索引以支持 O(1) 校验；不设 UNIQUE——换发瞬间旧行的
-- refresh_hash 与新行可能相同来源，唯一性由应用层轮换逻辑保证。
CREATE TABLE gw_auth_sessions (
    id                 TEXT PRIMARY KEY,
    user_id            TEXT    NOT NULL REFERENCES gw_users(id) ON DELETE CASCADE,
    access_hash        TEXT    NOT NULL,
    refresh_hash       TEXT    NOT NULL,
    rotated_from       TEXT    NOT NULL DEFAULT '',
    access_expires_at  INTEGER NOT NULL,
    refresh_expires_at INTEGER NOT NULL,
    revoked_at         INTEGER NOT NULL DEFAULT 0,
    created_at         INTEGER NOT NULL
);
CREATE INDEX idx_gw_auth_sessions_access  ON gw_auth_sessions(access_hash);
CREATE INDEX idx_gw_auth_sessions_refresh ON gw_auth_sessions(refresh_hash);
CREATE INDEX idx_gw_auth_sessions_user    ON gw_auth_sessions(user_id, created_at);

-- ---------------------------------------------------------------- gw_device_codes
-- 设备授权登录（文档 §5.2）。device_code_hash 是设备侧凭据（不落明文），
-- user_code 是人工核对码（UNIQUE，易读字符集）。user_id 待授权时为空，
-- 因此可空并配 ON DELETE SET NULL。
CREATE TABLE gw_device_codes (
    device_code_hash TEXT PRIMARY KEY,
    user_code        TEXT    NOT NULL UNIQUE,
    user_id          TEXT    REFERENCES gw_users(id) ON DELETE SET NULL,
    status           TEXT    NOT NULL DEFAULT 'pending',
    expires_at       INTEGER NOT NULL,
    created_at       INTEGER NOT NULL,
    last_polled_at   INTEGER NOT NULL DEFAULT 0,
    poll_interval_ms INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_gw_device_codes_status ON gw_device_codes(status, expires_at);
CREATE INDEX idx_gw_device_codes_user   ON gw_device_codes(user_id);

-- ---------------------------------------------------------------- gw_quota_accounts
-- 额度账户快照（每用户一行）。available = total - used - reserved，
-- reserved_amount 不得为负（DB 侧 CHECK）；available >= 0 由 ReserveTx /
-- AdjustTx 在单写事务内保证（§22 额度红线）。
CREATE TABLE gw_quota_accounts (
    user_id         TEXT PRIMARY KEY REFERENCES gw_users(id) ON DELETE CASCADE,
    total_amount    INTEGER NOT NULL DEFAULT 0,
    used_amount     INTEGER NOT NULL DEFAULT 0,
    reserved_amount INTEGER NOT NULL DEFAULT 0 CHECK (reserved_amount >= 0),
    version         INTEGER NOT NULL DEFAULT 0,
    status          TEXT    NOT NULL DEFAULT 'active',
    updated_at      INTEGER NOT NULL
);
CREATE INDEX idx_gw_quota_accounts_status ON gw_quota_accounts(status, updated_at);

-- ---------------------------------------------------------------- gw_quota_ledger
-- append-only 账本。idempotency_key 可空且 UNIQUE，是所有管理类操作的
-- 防重入唯一依据（文档 §7.2 / §17）；SQLite 中多个 NULL 互不冲突，
-- 故非幂等来源（reserve/release/settle）写 NULL 而非空串。
--   amount 语义：对 available 的有符号增量，故
--   SumLedgerAmount(user) == 账户 Available()（对账不变量，见 store 包注释）。
CREATE TABLE gw_quota_ledger (
    id              TEXT PRIMARY KEY,
    user_id         TEXT    NOT NULL REFERENCES gw_users(id) ON DELETE CASCADE,
    type            TEXT    NOT NULL,
    request_id      TEXT    NOT NULL DEFAULT '',
    idempotency_key TEXT    UNIQUE,
    operator_id     TEXT    NOT NULL DEFAULT '',
    reason          TEXT    NOT NULL DEFAULT '',
    amount          INTEGER NOT NULL,
    balance_after   INTEGER NOT NULL,
    reserved_after  INTEGER NOT NULL,
    created_at      INTEGER NOT NULL
);
CREATE INDEX idx_gw_quota_ledger_user ON gw_quota_ledger(user_id, created_at);
CREATE INDEX idx_gw_quota_ledger_idem ON gw_quota_ledger(idempotency_key);

-- ---------------------------------------------------------------- gw_quota_reservations
-- 预占行。部分唯一索引把“同一 (user_id, request_id) 至多一条 held”从
-- 应用层约定升级为 DB 约束（幂等的最后一道防线）；已 released/expired
-- 的历史行不占用该唯一性，允许同 request_id 在事后重新预占。
CREATE TABLE gw_quota_reservations (
    id             TEXT PRIMARY KEY,
    user_id        TEXT    NOT NULL REFERENCES gw_users(id) ON DELETE CASCADE,
    request_id     TEXT    NOT NULL,
    status         TEXT    NOT NULL DEFAULT 'held',
    amount         INTEGER NOT NULL,
    settled_amount INTEGER NOT NULL DEFAULT 0,
    expires_at     INTEGER NOT NULL DEFAULT 0,
    created_at     INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL
);
CREATE INDEX idx_gw_quota_reservations_user ON gw_quota_reservations(user_id, request_id);
CREATE INDEX idx_gw_quota_reservations_status ON gw_quota_reservations(status, expires_at);
CREATE UNIQUE INDEX idx_gw_quota_reservations_held
    ON gw_quota_reservations(user_id, request_id) WHERE status = 'held';

-- ---------------------------------------------------------------- gw_models
-- 对外模型目录。/v1/models 只回 enabled=1。
CREATE TABLE gw_models (
    model_id          TEXT PRIMARY KEY,
    display_name      TEXT    NOT NULL DEFAULT '',
    capabilities_json TEXT    NOT NULL DEFAULT '',
    enabled           INTEGER NOT NULL DEFAULT 1,
    created_at        INTEGER NOT NULL,
    updated_at        INTEGER NOT NULL
);
CREATE INDEX idx_gw_models_enabled ON gw_models(enabled, model_id);

-- ---------------------------------------------------------------- gw_providers
-- 上游服务商。api_key_ref 只是 internal/secrets 的引用，绝不存明文密钥（I12）。
CREATE TABLE gw_providers (
    id          TEXT PRIMARY KEY,
    name        TEXT    NOT NULL DEFAULT '',
    endpoint    TEXT    NOT NULL DEFAULT '',
    protocol    TEXT    NOT NULL,
    status      TEXT    NOT NULL DEFAULT 'enabled',
    api_key_ref TEXT    NOT NULL DEFAULT '',
    config_json TEXT    NOT NULL DEFAULT '',
    timeout_ms  INTEGER NOT NULL DEFAULT 0,
    weight      INTEGER NOT NULL DEFAULT 0,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);
CREATE INDEX idx_gw_providers_status ON gw_providers(status, id);

-- ---------------------------------------------------------------- gw_provider_models
-- 模型 → 上游模型名映射（路由候选表）。priority 数值小者优先。
-- 外键要求 provider 与 model 都已登记：映射到不存在的模型属于配置错误。
CREATE TABLE gw_provider_models (
    provider_id       TEXT    NOT NULL REFERENCES gw_providers(id) ON DELETE CASCADE,
    model_id          TEXT    NOT NULL REFERENCES gw_models(model_id) ON DELETE CASCADE,
    upstream_model_id TEXT    NOT NULL DEFAULT '',
    enabled           INTEGER NOT NULL DEFAULT 1,
    priority          INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (provider_id, model_id)
);
CREATE INDEX idx_gw_provider_models_model ON gw_provider_models(model_id, enabled, priority);

-- ---------------------------------------------------------------- gw_usage
-- 每次上游调用的用量/成本。request_id 主键即幂等键：重放同一请求不重复计费。
CREATE TABLE gw_usage (
    request_id    TEXT PRIMARY KEY,
    user_id       TEXT    NOT NULL REFERENCES gw_users(id) ON DELETE CASCADE,
    model_id      TEXT    NOT NULL DEFAULT '',
    provider_id   TEXT    NOT NULL DEFAULT '',
    status        TEXT    NOT NULL DEFAULT '',
    input_tokens  INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    latency_ms    INTEGER NOT NULL DEFAULT 0,
    cost_micro    INTEGER NOT NULL DEFAULT 0,
    created_at    INTEGER NOT NULL
);
CREATE INDEX idx_gw_usage_user     ON gw_usage(user_id, created_at);
CREATE INDEX idx_gw_usage_model    ON gw_usage(model_id, created_at);
CREATE INDEX idx_gw_usage_provider ON gw_usage(provider_id, created_at);

-- ---------------------------------------------------------------- gw_audit
-- 管理侧写操作审计：必须能追溯到操作者（文档 §22 后台验收）。
CREATE TABLE gw_audit (
    id          TEXT PRIMARY KEY,
    actor       TEXT    NOT NULL DEFAULT '',
    action      TEXT    NOT NULL,
    target      TEXT    NOT NULL DEFAULT '',
    result      TEXT    NOT NULL DEFAULT '',
    ip          TEXT    NOT NULL DEFAULT '',
    detail_json TEXT    NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL
);
CREATE INDEX idx_gw_audit_created ON gw_audit(created_at);
CREATE INDEX idx_gw_audit_actor   ON gw_audit(actor, created_at);
CREATE INDEX idx_gw_audit_target  ON gw_audit(target, created_at);
