# new-api 升级迁移 执行日志

- 计划：spec/newapi-upgrade-migration.md（Codex 三轮对抗审查通过）
- 分支：shtelos/main-v1.0.0-rc.20（基线 a79f9691）
- M0 spec 同步：✅ 提交 f86598f1，state spec_synced=true

## M1 F4 重试策略 —— ✅ 通过（重试 0 次）
- Codex 改动：status_code_ranges.go（504/524 移出 alwaysSkip）、controller/relay.go（shouldRetry 写出保护）、relay-claude.go（空流检测于 usage 回填前，ErrorCodeEmptyResponse）、service/channel.go（empty_response 禁用豁免）+ 测试：relay_retry_test.go（11 用例新建）、channel_test.go（新建）、relay_claude_test.go / status_code_ranges_test.go（并入）
- Claude 验收证据：
  - `go build ./...` OK（补 gitignored dist 占位后）
  - `go vet ./controller/ ./relay/... ./service/ ./setting/...` 仅上游基线自带 unreachable 告警（stash 验证与本段无关）
  - `go test ./setting/operation_setting/ ./relay/channel/claude/ ./service/ ./controller/` 全部 ok
  - 空流三用例 -v 验证：before-write 重试 / after-write 保持成功 / 正常流不受影响，全 PASS
  - diff 逐条对照验收标准符合；无范围蔓延（仅计划内 8 文件）
- 备注：Codex 沙箱内无法跑 go 命令（GOMODCACHE 写锁被拒），自检由 Claude 侧完成
## M2 F1 渠道倍率 —— ✅ 通过（重试 0 次）
- Codex 改动：13 个插入点全落地 + channel_authz.go（ratio 字段分类，必要联动）+ 测试（channel_ratio_test.go 新建、text_quota/task_billing/relay_info/price/tiered_settle 测试增量）
- 关键设计落实：composeTieredTextQuota 三分支收敛 finalizeTieredTextQuota 唯一乘点；TryTieredSettle fallback 返回 raw 估算（不再用已乘 FinalPreConsumedQuota）；ChannelRatio 用指针区分缺省/显式 0
- Claude 验收证据：
  - `go build ./...` OK；vet 无新告警
  - `go test ./service/ ./relay/common/ ./relay/helper/ ./controller/ ./model/ ./middleware/ ./relay/channel/claude/ ./setting/operation_setting/` 全 ok（含 M1 回归）
  - tiered/ChannelRatio -v：矩阵用例 + 不双乘断言 + 免费渠道用例 30+ 全 PASS
  - diff 核对 finalizeTieredTextQuota/price.go raw 字段符合 R2 二次修订公式；无范围蔓延
## M3 F2 定向选路 —— ✅ 通过（重试 0 次）
- Codex 改动：token_model_channel.go（表+30s 缓存+IsChannelAllowedForToken）、model/main.go 注册、main.go 启动、channel_cache.go tokenId 过滤、channel_select.go RetryParam、distributor.go（affinity 双分支白名单校验 + rejectedByTokenFilter 不清缓存）、relay.go 重试透传 + 3 个新测试文件
- Claude 验收证据：
  - `go build ./...` OK；model/service/middleware/controller/relay/... 全 ok
  - F2 三用例 -v 全 PASS；distributor 测试覆盖：双 token 同 affinity key 各落各自白名单、拒绝不清缓存（assertAffinityStillPointsTo）、空白名单放行、auto 分组变体
  - diff 核对：auto/普通两分支都有 IsChannelAllowedForToken；rejectedByTokenFilter 跳过 ClearCurrentChannelAffinityCache；空白名单 len==0 放行
## M4 F3-a 健康模型层 —— ✅ 通过（重试 1 次）
- Codex 改动：channel_model_disabled.go + channel_model_health.go（两表+状态机）、service/channel_model_health.go、service/model_health_error.go、channel_health_setting.go、model/main.go 注册、channel_cache.go disabledSet 排除、ability.go NOT EXISTS 选路排除、channel.go 清理钩子+ModelStatuses、controller/channel.go AttachChannelModelStatuses、relay.go 模型级禁用、service/channel.go 软错误豁免 + 3 个测试文件
- **回归修复（重试 1 次）**：M4 使 InitChannelCache 查 channel_model_disabled，middleware/distributor_token_affinity_test.go（M3 建）的 AutoMigrate 漏这两表 → F2 测试 panic。Codex 补表后 middleware 全绿。跨段回归被分段验收当场抓到
- Claude 验收证据：
  - `go build ./...` OK；vet 无新告警
  - `go test ./middleware/ ./model/ ./service/ ./controller/ ./setting/operation_setting/ ./relay/...` 全 ok（含 M1-M3 回归）
  - 状态机测试 26 用例：manual 不自动恢复 / 并发写 / 空模型名保护 / 软错误豁免全覆盖
  - FOR UPDATE 按 DatabaseTypeSQLite 退化，注释声明生产仅 MySQL（符合 R10）
## M5 F3-b 探针接入 —— ✅ 通过（重试 0 次）
- Codex 改动：probe/claude-agent/ 整目录拷入 + Dockerfile.claude-agent-probe + .dockerignore 放行、service/claude_agent_probe.go（HTTP 客户端）、controller/channel-test.go（手动分流 + runChannelTestTask 挂 system_task + passive_recovery pair 清单）+ 探针测试
- 关键设计落实：
  - 手动分流插在 request.SetModelName 之后、adaptor/relay 之前，Anthropic+探针配置走探针短路不记账
  - scheduled_all 全量 probe，passive_recovery pair 清单独立从 channel_model_disabled 取 `source IN (auto,relay)` + 渠道启用过滤（符合 R5）
  - probe 循环各迭代检查 ctx.Err()（lease 丢锁防双跑）
  - 老调度锁全删（grep 仅剩 spec 文档提及）：testAllChannelsLock/AutomaticallyTestChannels/autoTestChannelsOnce 等
- Claude 验收证据：
  - `go build ./...` OK；vet 无新告警
  - `go test ./controller/ ./service/ ./model/ ./middleware/ ./setting/operation_setting/ ./relay/channel/claude/` 全 ok（含 M1-M4 回归）
  - ProbeClaudeAgent + probeModelWithImmediateRetries + shouldProbeChannelModels 等探针用例全 PASS
- 备注：probe/claude-agent 为老 sidecar，package.json 无 test script（仅 build/start/dev），TS 侧测试老 fork 亦无——Go 侧全绿
## M6 前端 web/default 移植 —— ✅ 通过（重试 0 次）
- Codex 改动：channels/types.ts（ratio + model_statuses schema）、channels-columns.tsx（RatioCell 列 + 模型状态 Popover 列）、channel-mutate-drawer.tsx（ratio FormField）、channel-form.ts（schema/default/transform/两 payload）、usage-logs/types.ts + common-logs-columns.tsx + details-dialog.tsx（channel_ratio 展示）、7 语言 locale JSON
- 关键设计落实：
  - ratio create/update/default/transform 四处全用 `?? 1`，0=免费渠道原样发送不被吞
  - Codex 核对后端发现 ChannelModelStatus.Status 是字符串枚举 healthy/disabled/unknown，前端改用 z.enum 对齐（非计划里假设的 number）—— 主动纠偏
  - 模型状态列汇总徽章 healthy/total（全健康 success / 异常 danger）+ Popover 逐模型 StatusBadge
- Claude 验收证据（装 web workspace 依赖后真跑）：
  - `bun run typecheck`（tsgo -b）EXIT=0
  - `bun run build`（rsbuild）EXIT=0，产物正常出
  - 后端 model.ChannelModelStatus.Status 确认 string enum，z.enum 对齐无误
  - zh.json 译文准确（渠道倍率/模型状态/禁用原因），en.json 源 key 就位
## M7 部署形态 —— ✅ 通过（重试 0 次）
- Codex 改动：新建 Dockerfile.shtlcloud（上游 Dockerfile + GOPROXY/GOSUMDB 两行，四 stage 双前端保留）、改造 docker-compose.yml（本地构建 + MySQL 外部库 + MEMORY_CACHE_ENABLED/SYNC_FREQUENCY=10/PROBE env/SESSION_SECRET + claude-agent-probe sidecar + 删 postgres）、.env.example 增量
- Claude 验收证据：
  - `diff Dockerfile Dockerfile.shtlcloud` 仅多 GOPROXY/GOSUMDB 两行，四 stage 完整
  - postgres/pg_data grep 无残留（删净）
  - compose yaml 解析 OK，服务 = new-api/redis/claude-agent-probe

## 执行结论
- 完成到 M7（全部里程碑）；后端 M1-M5 + 前端 M6 + 部署 M7 全部验收通过
- 最终全量验证：
  - `go build ./...` OK
  - `go vet ./...` 仅上游基线自带告警（custom-event.go 锁拷贝 / email_test.go IPv6 / 各 adaptor unreachable，均非本次改动）
  - `go test ./...` 后端 26 个含测试包全 ok，零 FAIL
  - 前端 `bun run typecheck`（tsgo -b）+ `bun run build`（rsbuild）双 EXIT=0
- 重试统计：M4 一次（middleware 测试缺表回归，Codex 补表后绿）；其余里程碑 0 重试
- 遗留（非本次代码范围，属计划 M8/M9 运维阶段）：
  - DB 演练（B1-B2）：需生产服务器执行 clone tlcloud_newapi→tlcloud_twork + AutoMigrate 核对 + 手工 DDL（tokens.key/logs 索引）
  - 生产切换（B3）：低峰窗口，需用户拍板时间
  - tbackend ORM NewApiToken.key String(48)→String(128) 放宽（回滚兼容，随迁移同步，属 tbackend 仓库改动，未在本仓库执行）
  - SettingsMonitoring 前端对齐（A4 探针设置项增补）：计划标注与后端 commit 联动，本次未涉及探针专用设置 UI，如需可后续单独排

## 追加：SKIP_AUTO_MIGRATE 开关（2026-07-09，用户需求）
- 需求：生产线下手动执行 DDL，服务启动时不跑 AutoMigrate（避免 rwlb 端点意外 DDL、冗余 key 索引、启动时段跑重 DDL）
- 改动：`model/main.go` 加 `shouldSkipAutoMigrate()`（读 env `SKIP_AUTO_MIGRATE`，strconv.ParseBool），在 `InitDB`（migrateDB 前）和 `InitLogDB`（migrateLOGDB 前）两处加门；跳过时只 SysLog 提示、return nil，保留 root 账号初始化/setup/option 加载
- 验收：`go build ./...` OK；docker 重建后连已迁移的 tlcloud_twork 启动 → 日志 `database migration skipped`、`New API ready in 1637 ms`（对比迁移态 ~17s）、健康 200、system_instances 心跳正常
- 生产用法：低峰手动执行 rehearsal/schema-changes-record.md 第六节 DDL（含 6 张新表）→ 起服务时设 `SKIP_AUTO_MIGRATE=true` → 服务不碰 schema
- ⚠️ 前提：手动 DDL 必须与代码期望的库结构完全一致（缺列/缺表会运行时报错）；将来 pull 新上游若有新迁移，需同步补 DDL 或临时打开 AutoMigrate
