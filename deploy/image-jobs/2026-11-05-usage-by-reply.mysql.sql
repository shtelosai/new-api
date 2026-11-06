-- 仅提供上线 DDL；本次开发不执行。部署写入新字段的网关代码之前执行。
-- logs 在日志库（未配置 LOG_SQL_DSN 时为主库）；image_jobs 在主库。
-- MySQL 8.0.13：仅在表末尾追加列，不指定 FIRST/AFTER，不新增索引。
-- 显式 INSTANT：不支持时直接报错，不回退到重建表。
-- 要求 InnoDB，非 ROW_FORMAT=COMPRESSED、无 FULLTEXT 索引等 INSTANT 限制。
-- https://dev.mysql.com/doc/refman/8.0/en/innodb-online-ddl-operations.html
ALTER TABLE logs ADD COLUMN twork_turn CHAR(32) NOT NULL DEFAULT '', ALGORITHM=INSTANT;

-- image_jobs 没有 properties/private_data 等扩展字段；将三个有界标记存为 JSON 字符串。
-- 使用 VARCHAR 默认空串以兼容 MySQL 5.7/8.0、SQLite、PostgreSQL；不新增 JSON 列。
-- 不能复用 client_session_id（用于任务列表）或请求文件（会影响幂等哈希且有清理生命周期）。
ALTER TABLE image_jobs ADD COLUMN usage_metadata VARCHAR(256) NOT NULL DEFAULT '', ALGORITHM=INSTANT;

-- 普通异步 tasks 复用现有 private_data，无需 DDL。历史行不回填；回滚代码无需删列。
-- 独立 PostgreSQL/SQLite 部署使用相同 ADD COLUMN 定义，删除 ALGORITHM=INSTANT 子句。
-- ClickHouse logs 的 twork_turn String DEFAULT '' 由对应迁移分支处理。
