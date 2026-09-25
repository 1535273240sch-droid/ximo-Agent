-- 0001_init.sql — XimoAgent Go v2 初始 schema（第9章）
--
-- 约定：
--   1. 所有时间戳为 unix 毫秒 INTEGER。
--   2. 所有 ID 为 TEXT（uuid / 业务键），由应用层生成，保证跨进程唯一。
--   3. payload / 大字段一律 BLOB（JSON 序列化后的字节），NULL 表示缺省。
--   4. 外键全部显式声明并配 ON DELETE 行为；连接层 PRAGMA foreign_keys=ON。
--   5. 本文件是不可变基线：后续变更必须新增 0002_*.sql，不得修改本文件。

-- ---------------------------------------------------------------- migrations
-- schema 版本登记表。每个 migration 文件应用后写一行（version/name/checksum）。
-- 注意：migrations.Runner 启动时会先用同样的 DDL bootstrap 这张表，
-- 因此这里用 IF NOT EXISTS，两边建表语句保持一致。
CREATE TABLE IF NOT EXISTS migrations (
    version    INTEGER PRIMARY KEY,
    name       TEXT    NOT NULL,
    checksum   TEXT    NOT NULL,
    applied_at INTEGER NOT NULL
);

-- ---------------------------------------------------------------- kv
-- 键值存储：设置、feature flag、数据迁移版本等零散持久化数据。
CREATE TABLE kv (
    key        TEXT PRIMARY KEY,
    value      BLOB    NOT NULL,
    updated_at INTEGER NOT NULL
);

-- ---------------------------------------------------------------- sessions
-- 会话（对应 v1 conversations.json 的会话维度）。
CREATE TABLE sessions (
    id            TEXT PRIMARY KEY,
    title         TEXT    NOT NULL DEFAULT '',
    mode          TEXT    NOT NULL DEFAULT 'default',
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL,
    metadata_json BLOB
);

-- ---------------------------------------------------------------- runs
-- 一次 Agent 运行。last_seq 与 run_events 同事务推进，是 I4（sequence 不倒退）
-- 的数据库侧防线：任何 event 写入都必须连带更新 last_seq。
CREATE TABLE runs (
    id              TEXT PRIMARY KEY,
    session_id      TEXT    NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    status          TEXT    NOT NULL,
    prompt          TEXT,
    model           TEXT,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    started_at      INTEGER,
    finished_at     INTEGER,
    last_seq        INTEGER NOT NULL DEFAULT 0,
    error           TEXT,
    idempotency_key TEXT    UNIQUE,
    worker_id       TEXT,
    metadata_json   BLOB
);
CREATE INDEX idx_runs_session   ON runs(session_id, created_at);
CREATE INDEX idx_runs_status    ON runs(status, updated_at);
CREATE INDEX idx_runs_idem      ON runs(idempotency_key);

-- ---------------------------------------------------------------- messages
-- 对话消息（用户/助手/工具摘要），按 run 维度顺序存储。
CREATE TABLE messages (
    id           TEXT PRIMARY KEY,
    run_id       TEXT    NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    session_id   TEXT    NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    role         TEXT    NOT NULL,
    content      TEXT,
    seq          INTEGER NOT NULL,
    created_at   INTEGER NOT NULL,
    token_count  INTEGER,
    metadata_json BLOB
);
CREATE INDEX idx_messages_run ON messages(run_id, seq);

-- ---------------------------------------------------------------- run_events
-- durable event log（第9.1章，结构照抄，勿改）。
--   (run_id, seq) 主键保证每个 run 内 sequence 唯一且递增；
--   event_id 全局唯一，是 UI 重连补事件与 outbox 去重的依据。
CREATE TABLE run_events (
    run_id       TEXT    NOT NULL,
    seq          INTEGER NOT NULL,
    event_id     TEXT    NOT NULL UNIQUE,
    event_type   TEXT    NOT NULL,
    payload_json BLOB    NOT NULL,
    created_at   INTEGER NOT NULL,
    PRIMARY KEY (run_id, seq)
);
CREATE INDEX idx_run_events_run_seq ON run_events(run_id, seq);

-- ---------------------------------------------------------------- tool_calls
-- 工具调用记录。idempotency_key 唯一，是 I5（同一幂等键不产生两次 durable
-- side effect）的数据库侧防线。
CREATE TABLE tool_calls (
    id              TEXT PRIMARY KEY,
    run_id          TEXT    NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    message_id      TEXT    REFERENCES messages(id) ON DELETE SET NULL,
    name            TEXT    NOT NULL,
    arguments_json   BLOB,
    status          TEXT    NOT NULL,
    idempotency_key TEXT    UNIQUE,
    seq             INTEGER,
    attempt         INTEGER NOT NULL DEFAULT 0,
    worker_id       TEXT,
    error           TEXT,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL
);
CREATE INDEX idx_tool_calls_run  ON tool_calls(run_id, seq);
CREATE INDEX idx_tool_calls_idem ON tool_calls(idempotency_key);

-- ---------------------------------------------------------------- tool_results
-- 工具结果。tool_call_id UNIQUE：一个 tool call 至多一条 durable result（I3）。
CREATE TABLE tool_results (
    id           TEXT PRIMARY KEY,
    tool_call_id TEXT    NOT NULL UNIQUE REFERENCES tool_calls(id) ON DELETE CASCADE,
    run_id       TEXT    NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    output       BLOB,
    is_error     INTEGER NOT NULL DEFAULT 0,
    duration_ms  INTEGER,
    checksum     TEXT,
    created_at   INTEGER NOT NULL
);
CREATE INDEX idx_tool_results_run ON tool_results(run_id, created_at);

-- ---------------------------------------------------------------- checkpoints
-- checkpoint manifest 索引（内容在 checkpoint 包的 CAS 目录，这里只存元数据）。
CREATE TABLE checkpoints (
    id          TEXT PRIMARY KEY,
    run_id      TEXT    NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    session_id  TEXT    REFERENCES sessions(id) ON DELETE CASCADE,
    turn_id     TEXT    NOT NULL,
    label       TEXT,
    status      TEXT    NOT NULL DEFAULT 'active',
    pinned      INTEGER NOT NULL DEFAULT 0,
    blob_count  INTEGER NOT NULL DEFAULT 0,
    total_size  INTEGER NOT NULL DEFAULT 0,
    created_at  INTEGER NOT NULL
);
CREATE INDEX idx_checkpoints_run   ON checkpoints(run_id, created_at);
CREATE INDEX idx_checkpoints_turn  ON checkpoints(turn_id);
CREATE INDEX idx_checkpoints_pin   ON checkpoints(pinned, created_at);

-- ---------------------------------------------------------------- checkpoint_blobs
-- manifest 内 路径 → blob 的映射（GC mark 阶段的数据源）。
CREATE TABLE checkpoint_blobs (
    manifest_id TEXT    NOT NULL REFERENCES checkpoints(id) ON DELETE CASCADE,
    path        TEXT    NOT NULL,
    blob_hash   TEXT    NOT NULL,
    size        INTEGER NOT NULL,
    media_type  TEXT    NOT NULL DEFAULT '',
    mode        INTEGER NOT NULL DEFAULT 0,
    created_at  INTEGER NOT NULL,
    PRIMARY KEY (manifest_id, path)
);
CREATE INDEX idx_checkpoint_blobs_hash ON checkpoint_blobs(blob_hash);

-- ---------------------------------------------------------------- providers
-- Provider 配置。api_key_ref 只存密钥引用（secrets 模块的 key），绝不存明文
-- 密钥本身（I12：秘密信息不能进入普通 event/log payload，同理不进普通表）。
CREATE TABLE providers (
    id           TEXT PRIMARY KEY,
    name         TEXT    NOT NULL,
    type         TEXT    NOT NULL,
    base_url     TEXT,
    api_key_ref  TEXT,
    model        TEXT,
    enabled      INTEGER NOT NULL DEFAULT 1,
    priority     INTEGER NOT NULL DEFAULT 0,
    config_json  BLOB,
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL
);

-- ---------------------------------------------------------------- permissions
-- 权限决策（allow/deny/ask），按 subject+action+scope 去重。
CREATE TABLE permissions (
    id           TEXT PRIMARY KEY,
    subject_type TEXT    NOT NULL,
    subject_id   TEXT    NOT NULL,
    action       TEXT    NOT NULL,
    decision     TEXT    NOT NULL,
    scope        TEXT    NOT NULL DEFAULT '',
    granted_by   TEXT,
    created_at   INTEGER NOT NULL,
    expires_at   INTEGER,
    UNIQUE (subject_type, subject_id, action, scope)
);
CREATE INDEX idx_permissions_subject ON permissions(subject_type, subject_id);

-- ---------------------------------------------------------------- mcp_servers
-- MCP 服务器配置（对应 v1 mcp-config.json）。
CREATE TABLE mcp_servers (
    id          TEXT PRIMARY KEY,
    name        TEXT    NOT NULL,
    transport   TEXT    NOT NULL,
    command     TEXT,
    args_json   BLOB,
    env_json    BLOB,
    url         TEXT,
    headers_json BLOB,
    enabled     INTEGER NOT NULL DEFAULT 1,
    imported_at INTEGER NOT NULL,
    config_json BLOB
);

-- ---------------------------------------------------------------- experts
-- 专家定义（对应 v1 experts.json / shared/agents-raw.json 的自定义部分）。
CREATE TABLE experts (
    id           TEXT PRIMARY KEY,
    name         TEXT    NOT NULL,
    description  TEXT,
    prompt       TEXT,
    model        TEXT,
    tools_json   BLOB,
    enabled      INTEGER NOT NULL DEFAULT 1,
    source       TEXT    NOT NULL DEFAULT 'custom',
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL
);

-- ---------------------------------------------------------------- skills
-- 技能（对应 v1 skills.json / imported-skills.json）。
CREATE TABLE skills (
    id           TEXT PRIMARY KEY,
    name         TEXT    NOT NULL,
    description  TEXT,
    source       TEXT    NOT NULL DEFAULT 'custom',
    steps_json   BLOB,
    enabled      INTEGER NOT NULL DEFAULT 1,
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL
);

-- ---------------------------------------------------------------- knowledge_entries
-- 知识条目（对应 v1 KnowledgeStore 的 Orama 数据，按 mode 隔离）。
CREATE TABLE knowledge_entries (
    id           TEXT PRIMARY KEY,
    mode         TEXT    NOT NULL DEFAULT 'default',
    title        TEXT    NOT NULL,
    content      TEXT    NOT NULL,
    tags_json    BLOB,
    source       TEXT,
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL
);
CREATE INDEX idx_knowledge_mode ON knowledge_entries(mode, updated_at);

-- ---------------------------------------------------------------- outbox
-- 事务外发箱（第25章）：与 run_events 同事务写入，后台 dispatcher 经 IPC 推送，
-- 送达后删除。DB committed 但 Engine 崩溃时，重启 replay 未送达事件。
CREATE TABLE outbox (
    event_id        TEXT PRIMARY KEY,
    run_id          TEXT    NOT NULL,
    seq             INTEGER NOT NULL,
    event_type      TEXT    NOT NULL,
    payload_json    BLOB    NOT NULL,
    enqueued_at     INTEGER NOT NULL,
    attempts        INTEGER NOT NULL DEFAULT 0,
    last_error      TEXT,
    next_attempt_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_outbox_due ON outbox(next_attempt_at, enqueued_at);

-- ---------------------------------------------------------------- leases
-- 租约（worker / 资源所有权）。fencing_token 单调递增，用于防止脑裂写入。
CREATE TABLE leases (
    name          TEXT PRIMARY KEY,
    owner         TEXT    NOT NULL,
    run_id        TEXT,
    acquired_at   INTEGER NOT NULL,
    expires_at    INTEGER NOT NULL,
    fencing_token INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_leases_expiry ON leases(expires_at);
