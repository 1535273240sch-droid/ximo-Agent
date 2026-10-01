-- 0004_expert_catalog.sql
--
-- 专家目录扩展：v1 的 experts 表只有 name/description/prompt/model/tools，
-- 而 v2 的专家定义（internal/expert.Expert）还带部门、emoji、执业风格、人格与配色。
-- 自定义专家此前**根本没有落库的地方** —— expert.CustomStore 只有测试假件，
-- Dependencies.Experts 从未被赋值，注册表永远是 NewRegistry(nil)。这一版把它接上。
--
-- 为什么用 ALTER TABLE ADD COLUMN 而不是新建表：experts 表的主键、name 与 tools_json
-- 已经被既有数据与仓库层使用，重建表要迁移数据、还可能破坏外键引用。追加列是纯
-- 增量操作，既有行取默认值（空串），读出来就是"没有这一项"，与旧数据语义一致。
--
-- 注意：ALTER TABLE ADD COLUMN 加 NOT NULL 列必须带默认值，这是 SQLite 的硬要求。

ALTER TABLE experts ADD COLUMN division    TEXT NOT NULL DEFAULT '';
ALTER TABLE experts ADD COLUMN emoji       TEXT NOT NULL DEFAULT '';
ALTER TABLE experts ADD COLUMN vibe        TEXT NOT NULL DEFAULT '';
ALTER TABLE experts ADD COLUMN color       TEXT NOT NULL DEFAULT '';
ALTER TABLE experts ADD COLUMN personality TEXT NOT NULL DEFAULT '';
