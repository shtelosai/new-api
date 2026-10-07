-- 在实际 LOG_DB 执行；与主库相同也要执行本表DDL。仅支持可事务SQL日志库。
CREATE TABLE image_job_log_receipts (
  id VARCHAR(80) PRIMARY KEY,
  created_at BIGINT NOT NULL
);
