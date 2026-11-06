-- 仅提供上线 DDL；部署写入新字段的网关代码之前执行。image_jobs 在主库。
-- 空串表示对话生图任务，非空表示画布任务的模型。历史行不回填，回滚代码无需删列。
ALTER TABLE image_jobs ADD COLUMN image_model VARCHAR(64) NOT NULL DEFAULT '', ALGORITHM=INSTANT;
