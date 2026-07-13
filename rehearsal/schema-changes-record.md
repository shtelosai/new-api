# 迁移版对生产库的 Schema 改动记录（演练实测）

> 来源：本地 new-api:shtlcloud（v1.0.0-rc.20 + 4 项自研迁移）连 RDS `tlcloud_twork`（生产 tlcloud_newapi 的完整克隆）跑 AutoMigrate 实测。
> 时间：2026-07-09。基线 `rehearsal/schema_before.sql`（30 表）→ 迁移后 `rehearsal/schema_after.sql`（36 表）。
> **用途：这份就是正式切换 tlcloud_newapi 时 AutoMigrate 会做的全部改动；照此在生产核对/预执行。**

## 一、启动结果
- `using MySQL` → `database migration started` → `New API started`，AutoMigrate 约 17s 完成，无报错/panic/denied
- `/api/status` 正常，`channels synced`，system task runner 起来
- 全部改动 **additive**，未删任何表/列/索引；`logs.idx_created_at_id` 未被重建（仍 `(id,created_at)`）

## 二、新增表（6，均上游 v1.0.0-rc.20）
`authz_roles`、`casbin_rule`、`perf_metrics`、`system_instances`、`system_task_locks`、`system_tasks`
→ CREATE TABLE，老 fork/tbackend 均忽略，零影响。

## 三、已存在表的列 / 索引改动

| 表 | 改动 | 计划是否预测 |
|---|---|---|
| **tokens** | `key` 改类型 char(48) → **varchar(128)**；482 行秒级 | ✅ 预测到 |
| **tokens** | ⚠️ **多建冗余唯一索引 `key`(key)**（与原有 `idx_tokens_key` 重复，GORM 命名不匹配所致） | ❌ 静态分析未预见 |
| **logs** | 加列 `upstream_request_id` varchar(128) DEFAULT ''；加索引 `idx_logs_upstream_request_id`（110万行/1.4G，8.0 加列 instant、加索引 online） | ✅ 预测到 |
| **quota_data** | 加列 `use_group`/`token_id`/`channel_id`/`node_name` + 各自索引 | ✅ 预测到 |
| **users** | 加列 `created_at`、`last_login_at` | ✅ 预测到 |
| **top_ups** | 加列 `payment_provider` | ✅ 预测到 |
| **subscription_orders** | 加列 `payment_provider` | ❌ 计划未列（订阅表） |
| **subscription_plans** | 加列 `allow_balance_pay`、`allow_wallet_overflow`、`waffo_pancake_product_id`、`downgrade_group` | ❌ 计划未列（订阅表） |
| **user_subscriptions** | 加列 `downgrade_group`、`allow_wallet_overflow` | ❌ 计划未列（订阅表） |

> 订阅相关列（subscription_*、user_subscriptions）是上游支付/订阅体系的字段，纯 additive、有默认值，对不使用订阅的当前业务无影响，但正式切换会一并加上。

## 四、复用（生产已有，迁移版不动）
`channels.ratio`、`token_model_channels`（1.4万+行）、`channel_model_health`、`channel_model_disabled`——老 fork 已建，迁移版直接读写，无 schema 变化。

## 五、生产影响评估（结合真实规模）
1. 全部 additive，MySQL 8.0 下加列 instant、加索引 online（logs 1.4G 也不阻塞），改 tokens.key 秒级 → **对在跑的老 fork（连 tlcloud_newapi）和 tbackend 无阻塞、无破坏**
2. tokens 冗余 `key` 索引：功能无害（双唯一索引都强制唯一），仅极小空间/写开销；可切换后择机 `DROP INDEX \`key\` ON tokens`（先确认应用只认 idx_tokens_key）
3. `logs.idx_created_at_id` 列序 AutoMigrate 不动，若要对齐上游 (created_at,id) 仍需手工，纯性能、非必须

## 六、生产正式切换的 DDL（可选预执行，减少切换窗口 AutoMigrate 耗时）
> 不预执行也行——切换时 AutoMigrate 会自动做。预执行只是把窗口压到秒级。在生产 `tlcloud_newapi` 低峰执行：
```sql
-- 加列（8.0 INSTANT）
ALTER TABLE users ADD COLUMN created_at bigint DEFAULT NULL, ADD COLUMN last_login_at bigint DEFAULT 0;
ALTER TABLE top_ups ADD COLUMN payment_provider varchar(50) DEFAULT '';
ALTER TABLE subscription_orders ADD COLUMN payment_provider varchar(50) DEFAULT '';
ALTER TABLE subscription_plans ADD COLUMN allow_balance_pay tinyint(1) DEFAULT NULL, ADD COLUMN allow_wallet_overflow tinyint(1) DEFAULT NULL, ADD COLUMN waffo_pancake_product_id varchar(128) DEFAULT '', ADD COLUMN downgrade_group varchar(64) DEFAULT '';
ALTER TABLE user_subscriptions ADD COLUMN downgrade_group varchar(64) DEFAULT '', ADD COLUMN allow_wallet_overflow tinyint(1) DEFAULT NULL;
ALTER TABLE quota_data ADD COLUMN use_group varchar(64) DEFAULT '', ADD COLUMN token_id bigint DEFAULT 0, ADD COLUMN channel_id bigint DEFAULT 0, ADD COLUMN node_name varchar(64) DEFAULT '';
-- logs 加列 + 索引（online）
ALTER TABLE logs ADD COLUMN upstream_request_id varchar(128) DEFAULT '', ADD INDEX idx_logs_upstream_request_id (upstream_request_id), ALGORITHM=INPLACE, LOCK=NONE;
ALTER TABLE quota_data ADD INDEX idx_quota_data_use_group (use_group), ADD INDEX idx_quota_data_token_id (token_id), ADD INDEX idx_quota_data_channel_id (channel_id), ADD INDEX idx_quota_data_node_name (node_name), ALGORITHM=INPLACE, LOCK=NONE;
-- tokens.key 扩容（秒级）
ALTER TABLE tokens MODIFY COLUMN `key` varchar(128) COLLATE utf8mb4_general_ci DEFAULT NULL;
-- 6 张新表：让 AutoMigrate 自动建即可（结构见 schema_after.sql），或从 schema_after.sql 提取 CREATE TABLE
```
> 冗余 `key` 索引不必预建（AutoMigrate 会加，或干脆切换后不理它/DROP）。

---

## 附：为演练访问，Claude 手工对克隆库做的数据改动（**仅克隆，生产切换时不要照搬**）
> 这些是为了能登录/看新 UI 做的运行时数据改动，不是 AutoMigrate 的 schema 改动，也不影响生产：
1. `options`：新增 `theme.frontend='default'`（切到 web/default 新 UI；生产是否切新 UI 另行决定，默认 classic）
2. `users(id=1 superadmin)`：password 重置为 bcrypt(`Rehearsal@123`)（生产 superadmin 密码**不动**）
3. `two_fas(user_id=1)`：is_enabled=0（关掉 superadmin 2FA；生产**不动**）
- 登录入口：http://localhost:3001，superadmin / Rehearsal@123

---

## 附二：生产切换推荐流程（配合 SKIP_AUTO_MIGRATE）
1. 低峰在生产 tlcloud_newapi 手动执行第六节全部 DDL + 6 张新表 CREATE（从 schema_after.sql 提取）
2. 核对库结构 = schema_after.sql
3. 停老 fork + tbackend（避免双版本抢库）
4. 起迁移版，env 设 `SKIP_AUTO_MIGRATE=true` → 服务不跑 AutoMigrate、秒级起来、不碰 schema
5. 验证后重启 tbackend
> 好处：DDL 完全人工可控、可回滚；服务启动零 DDL 风险（不经 rwlb 跑 DDL、不会多建冗余 key 索引）

---

## 附三：生产库 tlcloud_newapi DDL 已执行并核对（2026-07-10）
- 用户手动执行 rehearsal/production-ddl.sql（照 schema_after 固化）
- Claude 只读核对结果：**全部落地正确**
  - 6 张新表齐 ✅ / 15 个新列全在 ✅ / 5 个新索引全在 ✅ / tokens.key=varchar(128) ✅
  - 生产现状 vs 演练 schema_after 归一化对比：**除故意没建的冗余 tokens.`key` 唯一索引外，0 差异**
- 结论：生产 schema = 已测通过的目标结构，**可用 SKIP_AUTO_MIGRATE=true 启动新版，不会因缺列/缺表运行时报错**
- 快照留存：rehearsal/schema_prod_after.sql

---

## 附四：生产切换已完成（2026-07-10 01:10）
- 生产库 DDL 已执行核对通过；shtelosai/new-api main = 9c56dd63（迁移版）
- 服务器 `ssh twork` 切换：备份老镜像 → fresh clone 构建 `new-api:twork-v1rc20`（双前端）→ compose 切镜像 + `.env.shtlcloud` 加 SKIP_AUTO_MIGRATE=true → 停老容器/刷 redis DB0/起新版
- 结果：`/api/status` 4s 健康，日志确认 `migration skipped` + `ready in 423ms`；**实际停机 ~4-6s，tbackend 全程未停**
- 验证：新版身份确认（SKIP_AUTO_MIGRATE 日志为新代码独有）、tbackend active+200、真实请求已落库、无用户请求错误（仅渠道巡检噪音）
- **回滚**：`ssh twork /www/twork/new-api/ROLLBACK.sh`（一键，恢复 .precutover 配置 + 老镜像 twork-source，几秒回到老 fork；DB additive 无需还原）
- 备份位置：/www/twork/backups/newapi-cutover-20260710-003633；老镜像 new-api:twork-source(5762ce5048dc) + twork-oldfork-rollback-20260710-003633
