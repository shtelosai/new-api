-- New API 会话级缓存统计日志字段。
-- 生产启用 SKIP_AUTO_MIGRATE=true，必须先执行本文件，再发布读取该列的代码。

ALTER TABLE `logs`
  ADD COLUMN `conversation_hash` char(32)
    CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
  ALGORITHM=INSTANT;

ALTER TABLE `logs`
  ADD INDEX `idx_logs_channel_created_conversation`
    (`channel_id`, `created_at`, `conversation_hash`),
  ALGORITHM=INPLACE, LOCK=NONE;

SELECT column_name, column_type, character_set_name, collation_name
FROM information_schema.columns
WHERE table_schema = DATABASE()
  AND table_name = 'logs'
  AND column_name = 'conversation_hash';

SHOW INDEX FROM `logs`
WHERE Key_name = 'idx_logs_channel_created_conversation';
