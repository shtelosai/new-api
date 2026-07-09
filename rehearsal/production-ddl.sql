-- ============================================================
-- new-api 升级：生产库 tlcloud_newapi DDL（照演练实测 schema_after.sql 固化）
-- 目标：把上游 v1.0.0-rc.20 新增的表/列补齐，与迁移版代码期望一致
-- 特性：全部 additive（只加不删/不改现有数据）；老 fork 在跑时执行无感
-- 执行方式：低峰先执行本脚本 → 切换时新版用 SKIP_AUTO_MIGRATE=true 启动
-- ⚠️ 执行前务必已备份 tlcloud_newapi；确认连的是生产主库（非只读节点）
-- ⚠️ 本脚本只应执行一次（ADD COLUMN 不可重复；CREATE/索引部分已做幂等）
-- ============================================================

USE tlcloud_newapi;   -- ⚠️ 确认库名正确再执行

-- ---------- 1) 6 张新表（IF NOT EXISTS 幂等，instant） ----------

CREATE TABLE IF NOT EXISTS `authz_roles` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `key` varchar(64) COLLATE utf8mb4_general_ci NOT NULL,
  `name` varchar(100) COLLATE utf8mb4_general_ci NOT NULL,
  `description` text COLLATE utf8mb4_general_ci,
  `built_in` tinyint(1) DEFAULT NULL,
  `enabled` tinyint(1) DEFAULT NULL,
  `sort` bigint(20) DEFAULT NULL,
  `created_at` bigint(20) DEFAULT NULL,
  `updated_at` bigint(20) DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `idx_authz_roles_key` (`key`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE IF NOT EXISTS `casbin_rule` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `ptype` varchar(100) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `v0` varchar(100) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `v1` varchar(100) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `v2` varchar(100) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `v3` varchar(100) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `v4` varchar(100) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `v5` varchar(100) COLLATE utf8mb4_general_ci DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `idx_casbin_rule_unique` (`ptype`,`v0`,`v1`,`v2`,`v3`,`v4`,`v5`),
  KEY `idx_casbin_rule` (`ptype`,`v0`,`v1`,`v2`,`v3`,`v4`,`v5`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE IF NOT EXISTS `perf_metrics` (
  `id` bigint(20) NOT NULL AUTO_INCREMENT,
  `model_name` varchar(128) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `group` varchar(64) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `bucket_ts` bigint(20) DEFAULT NULL,
  `request_count` bigint(20) DEFAULT '0',
  `success_count` bigint(20) DEFAULT '0',
  `total_latency_ms` bigint(20) DEFAULT '0',
  `ttft_sum_ms` bigint(20) DEFAULT '0',
  `ttft_count` bigint(20) DEFAULT '0',
  `output_tokens` bigint(20) DEFAULT '0',
  `generation_ms` bigint(20) DEFAULT '0',
  PRIMARY KEY (`id`),
  UNIQUE KEY `idx_perf_model_group_bucket` (`model_name`,`group`,`bucket_ts`),
  KEY `idx_perf_bucket_ts` (`bucket_ts`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE IF NOT EXISTS `system_instances` (
  `node_name` varchar(128) COLLATE utf8mb4_general_ci NOT NULL,
  `info` text COLLATE utf8mb4_general_ci,
  `started_at` bigint(20) DEFAULT NULL,
  `last_seen_at` bigint(20) DEFAULT NULL,
  `created_at` bigint(20) DEFAULT NULL,
  `updated_at` bigint(20) DEFAULT NULL,
  PRIMARY KEY (`node_name`),
  KEY `idx_system_instances_started_at` (`started_at`),
  KEY `idx_system_instances_last_seen_at` (`last_seen_at`),
  KEY `idx_system_instances_created_at` (`created_at`),
  KEY `idx_system_instances_updated_at` (`updated_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE IF NOT EXISTS `system_task_locks` (
  `type` varchar(64) COLLATE utf8mb4_general_ci NOT NULL,
  `task_id` varchar(64) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `locked_by` varchar(128) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `locked_until` bigint(20) DEFAULT NULL,
  `updated_at` bigint(20) DEFAULT NULL,
  PRIMARY KEY (`type`),
  KEY `idx_system_task_locks_task_id` (`task_id`),
  KEY `idx_system_task_locks_locked_by` (`locked_by`),
  KEY `idx_system_task_locks_locked_until` (`locked_until`),
  KEY `idx_system_task_locks_updated_at` (`updated_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE IF NOT EXISTS `system_tasks` (
  `id` bigint(20) NOT NULL AUTO_INCREMENT,
  `task_id` varchar(64) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `type` varchar(64) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `status` varchar(32) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `active_key` varchar(64) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `payload` text COLLATE utf8mb4_general_ci,
  `state` text COLLATE utf8mb4_general_ci,
  `result` text COLLATE utf8mb4_general_ci,
  `error` text COLLATE utf8mb4_general_ci,
  `locked_by` varchar(128) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `created_at` bigint(20) DEFAULT NULL,
  `updated_at` bigint(20) DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `idx_system_tasks_task_id` (`task_id`),
  UNIQUE KEY `idx_system_tasks_active_key` (`active_key`),
  KEY `idx_system_tasks_type` (`type`),
  KEY `idx_system_tasks_status` (`status`),
  KEY `idx_system_tasks_locked_by` (`locked_by`),
  KEY `idx_system_tasks_created_at` (`created_at`),
  KEY `idx_system_tasks_updated_at` (`updated_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

-- ---------- 2) 已存在表加列（8.0 自动走 INSTANT，秒级不锁） ----------

ALTER TABLE `users`
  ADD COLUMN `created_at` bigint(20) DEFAULT NULL,
  ADD COLUMN `last_login_at` bigint(20) DEFAULT 0;

ALTER TABLE `top_ups`
  ADD COLUMN `payment_provider` varchar(50) COLLATE utf8mb4_general_ci DEFAULT '';

ALTER TABLE `subscription_orders`
  ADD COLUMN `payment_provider` varchar(50) COLLATE utf8mb4_general_ci DEFAULT '';

ALTER TABLE `subscription_plans`
  ADD COLUMN `allow_balance_pay` tinyint(1) DEFAULT NULL,
  ADD COLUMN `allow_wallet_overflow` tinyint(1) DEFAULT NULL,
  ADD COLUMN `waffo_pancake_product_id` varchar(128) COLLATE utf8mb4_general_ci DEFAULT '',
  ADD COLUMN `downgrade_group` varchar(64) COLLATE utf8mb4_general_ci DEFAULT '';

ALTER TABLE `user_subscriptions`
  ADD COLUMN `downgrade_group` varchar(64) COLLATE utf8mb4_general_ci DEFAULT '',
  ADD COLUMN `allow_wallet_overflow` tinyint(1) DEFAULT NULL;

-- quota_data：加 4 列（小表 1.7 万行，秒级）
ALTER TABLE `quota_data`
  ADD COLUMN `use_group` varchar(64) COLLATE utf8mb4_general_ci DEFAULT '',
  ADD COLUMN `token_id` bigint(20) DEFAULT 0,
  ADD COLUMN `channel_id` bigint(20) DEFAULT 0,
  ADD COLUMN `node_name` varchar(64) COLLATE utf8mb4_general_ci DEFAULT '';

-- ---------- 3) logs（110 万行/1.4G）：加列 INSTANT，加索引 ONLINE 不阻塞 ----------

-- 加列（不指定 ALGORITHM，MySQL 8.0 自动 INSTANT）
ALTER TABLE `logs`
  ADD COLUMN `upstream_request_id` varchar(128) COLLATE utf8mb4_general_ci DEFAULT '';

-- 加索引（在线，读写不阻塞）
ALTER TABLE `logs`
  ADD INDEX `idx_logs_upstream_request_id` (`upstream_request_id`),
  ALGORITHM=INPLACE, LOCK=NONE;

-- quota_data 索引（小表，一起在线加）
ALTER TABLE `quota_data`
  ADD INDEX `idx_quota_data_use_group` (`use_group`),
  ADD INDEX `idx_quota_data_token_id` (`token_id`),
  ADD INDEX `idx_quota_data_channel_id` (`channel_id`),
  ADD INDEX `idx_quota_data_node_name` (`node_name`),
  ALGORITHM=INPLACE, LOCK=NONE;

-- ---------- 4) tokens.key 扩容 char(48) → varchar(128)（482 行，秒级） ----------
-- 只做 MODIFY；不建 AutoMigrate 会多建的那个冗余 UNIQUE KEY `key`（原有 idx_tokens_key 已够）
ALTER TABLE `tokens`
  MODIFY COLUMN `key` varchar(128) COLLATE utf8mb4_general_ci DEFAULT NULL;

-- ============================================================
-- 5) 执行后核对（期望：新表 6 张齐、tokens.key=varchar(128)、关键列在）
-- ============================================================
SELECT COUNT(*) AS new_tables_should_be_6
FROM information_schema.tables
WHERE table_schema='tlcloud_newapi'
  AND table_name IN ('authz_roles','casbin_rule','perf_metrics','system_instances','system_task_locks','system_tasks');

SELECT column_type AS tokens_key_should_be_varchar128
FROM information_schema.columns
WHERE table_schema='tlcloud_newapi' AND table_name='tokens' AND column_name='key';

SELECT table_name, column_name
FROM information_schema.columns
WHERE table_schema='tlcloud_newapi' AND (
     (table_name='users' AND column_name IN ('created_at','last_login_at'))
  OR (table_name='logs' AND column_name='upstream_request_id')
  OR (table_name='quota_data' AND column_name IN ('use_group','token_id','channel_id','node_name'))
  OR (table_name='top_ups' AND column_name='payment_provider'))
ORDER BY table_name, column_name;
