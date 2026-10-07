-- 仅在确认备份及目标主库后由运维手工执行，不由服务自动迁移。
CREATE TABLE image_jobs (
  id VARCHAR(64) PRIMARY KEY,
  token_id INTEGER NOT NULL,
  user_id INTEGER NOT NULL,
  client_request_id VARCHAR(128) NOT NULL,
  client_session_id VARCHAR(128) NOT NULL,
  request_hash VARCHAR(64) NOT NULL,
  status VARCHAR(20) NOT NULL,
  model_family VARCHAR(16) NOT NULL,
  resolution VARCHAR(4) NOT NULL,
  size VARCHAR(8) NOT NULL,
  price_cents INTEGER NOT NULL DEFAULT 0,
  reserved_quota INTEGER NOT NULL DEFAULT 0,
  charged_quota INTEGER NOT NULL DEFAULT 0,
  channel_id INTEGER NOT NULL DEFAULT 0,
  attempted_channels TEXT NOT NULL,
  image_provider VARCHAR(16) NOT NULL DEFAULT '',
  adapter_base_url VARCHAR(1024) NOT NULL DEFAULT '',
  provider_task_id VARCHAR(160) NOT NULL DEFAULT '',
  error_code VARCHAR(64) NOT NULL DEFAULT '',
  actual_size VARCHAR(32) NOT NULL DEFAULT '',
  output_format VARCHAR(8) NOT NULL DEFAULT '',
  bytes BIGINT NOT NULL DEFAULT 0,
  result_hash VARCHAR(64) NOT NULL DEFAULT '',
  created_at BIGINT NOT NULL,
  updated_at BIGINT NOT NULL,
  settled_at BIGINT NOT NULL DEFAULT 0,
  files_removed_at BIGINT NOT NULL DEFAULT 0,
  expires_at BIGINT NOT NULL,
  next_poll_at BIGINT NOT NULL DEFAULT 0,
  lease_owner VARCHAR(64) NOT NULL DEFAULT '',
  lease_until BIGINT NOT NULL DEFAULT 0,
  CONSTRAINT idx_image_job_request UNIQUE (token_id, client_request_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
CREATE INDEX idx_image_job_token_session ON image_jobs (token_id, client_session_id);
CREATE INDEX idx_image_job_work ON image_jobs (status, next_poll_at);
CREATE INDEX idx_image_job_lease ON image_jobs (lease_until);
CREATE INDEX idx_image_job_settled ON image_jobs (settled_at);
CREATE TABLE image_job_projections (
  id VARCHAR(80) PRIMARY KEY,
  job_id VARCHAR(64) NOT NULL,
  created_at BIGINT NOT NULL,
  completed_at BIGINT NOT NULL DEFAULT 0
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
CREATE INDEX idx_image_job_projection_pending ON image_job_projections (completed_at);
CREATE INDEX idx_image_job_projection_job ON image_job_projections (job_id);

-- 永久额度屏障与在线批量增量移交凭证；关闭创建入口后仍保留。
CREATE TABLE image_job_accounting_guards (
  kind VARCHAR(8) NOT NULL,
  subject_id INTEGER NOT NULL,
  created_at BIGINT NOT NULL,
  PRIMARY KEY (kind, subject_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
CREATE TABLE image_job_accounting_transfers (
  id VARCHAR(36) PRIMARY KEY,
  token_id INTEGER NOT NULL,
  user_id INTEGER NOT NULL,
  token_delta INTEGER NOT NULL,
  user_delta INTEGER NOT NULL,
  used_delta INTEGER NOT NULL,
  request_delta INTEGER NOT NULL,
  created_at BIGINT NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
