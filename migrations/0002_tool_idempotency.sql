-- 0002_tool_idempotency.sql — 工具幂等键表（第19章 DDL，08 号裁决 D-5：schema 唯一归 03）
--
-- 与 04 号 internal/tool/idempotency.go 的 IdempotencySchema 逐字一致
-- （04 侧常量在集成时降级为测试用内存 schema，本文件是唯一生产来源）。
--
-- I5：同一个 idempotency key 不能产生两个 durable side effects。
-- Claim 的原子性由 key 主键 + 单写队列共同保证：
--   - key 不存在       → INSERT inflight，认领成功；
--   - inflight 未陈旧  → 认领失败（别人正在做）；
--   - inflight 已陈旧  → 接管（崩溃恢复路径）。
CREATE TABLE tool_idempotency (
    key TEXT PRIMARY KEY,
    status TEXT NOT NULL,
    result BLOB,
    created_at INTEGER NOT NULL
);

-- stale 扫描：Claim 需要按 status+created_at 找可接管的陈旧 inflight 记录。
-- （此索引是 03 对第19章的补充，04 的常量里没有，不影响 DDL 一致性。）
CREATE INDEX idx_tool_idempotency_status_created ON tool_idempotency(status, created_at);
