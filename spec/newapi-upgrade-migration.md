# new-api 升级迁移评估与实施计划

## 用户决策（2026-07-09 已确认）
1. **功能迁移**：F1 渠道倍率、F2 定向选路、F3 探针+模型级健康、F4 重试策略 —— 四项全迁；F5 杂项随部署带上
2. **数据库演练**：清空废弃老库 `tlcloud_twork`（表+数据），将生产库 `tlcloud_newapi` 全量克隆进去，新版先连演练库测试；确认后再切生产库
3. **前端**：fork 的 UI 改动只移植到 web/default（新架构）

## Context
- 生产在用：`../new-api-bak`（fork `shtelosai/new-api`，基于上游 v0.11.4-alpha.5，含 13 个自定义提交）
- 目标版本：`../new-api`（上游 QuantumNous/new-api，**迁移基线冻结在 commit `a79f9691`（v1.0.0-rc.20-7）**，实施期间不跟进上游新提交；计划中的行号/插入点均基于该 SHA）
- 目标：丢弃老版本，重新部署最新版；沿用现有数据库（不建新库）
- 上游历史被重写过，两仓库 commit hash 零重合，自定义提交靠作者（craiet/oyty）+ 标题差集锁定

## 已确认事实

### 自定义提交清单（13 个，2026-03 ~ 2026-07）
1. `754dc424` chore: update deployment and local changes —— channels.ratio 渠道倍率列 + 计费、usage-logs 前端列、docker-compose、model-ratio-calculator.html
2. `e5755e61` feat: token ID handling for channel selection —— token_model_channels 表 + 按 token×model 定向选渠道
3. `918b17d4` feat: 渠道倍率支持设为 0（免费渠道）
4. `0fe442ee` perf: SYNC_FREQUENCY 60s→10s + logs 表 idx_logs_token_type_created_at 复合索引
5. `2cbf50b4` feat: Claude Agent SDK 模型健康检查（probe sidecar + channel_model_health / channel_model_disabled 表 + 渠道×模型粒度禁用）
6. `a42be16f` chore: probe compose 服务配置
7. `be079bec` fix: 精简 probe 请求
8. `b4904dc9` feat: 渠道列表展示模型状态
9. `caea1264` fix: 避免 Anthropic probe 误判空响应
10. `b6464d08` fix: 渠道失败重试与健康禁用策略（504/524 继续重试）
11. `44fb1149` fix: Claude 流式空回复触发渠道重试（empty_response 不禁渠道）
12. `0dfcb643` docs: 计划文档
13. `0449dd2b` fix: 上游 400 触发渠道重试

### tbackend 硬依赖（直连 new-api MySQL）
- `channels.ratio` 列（fork 自定义）：channel_display_multiplier_service 读取算显示倍率
- `token_model_channels` 表（fork 自定义）：tbackend distribution_rule_service / model_access_service **写入**，new-api channel_select 读取选路 —— 渠道分发规则体系的核心
- `channel_model_disabled` 表（fork 自定义）：tbackend 只读（分发规则审计/模型过滤）
- `logs` 表：usage 统计全链路依赖（token_name=twork_{user_id}、type=2、other JSON cache_tokens 等）
- `tokens` 表：每用户 token 读写

## 待探索结果
- [x] Agent A：老仓库自定义提交细节 —— 已完成，结论见下
- [x] Agent B：新版本等价功能核查 —— 已完成，结论见下

## Agent B 结论：新版（v1.0.0-rc.20）等价功能核查

| fork 功能 | 新版状态 | 证据/说明 |
|---|---|---|
| 渠道级自动巡检+自动禁用 | ✅ 已有（渠道级） | system_task 框架 + CHANNEL_TEST_ENABLED，scheduled_all / passive_recovery 两模式 |
| 渠道×模型粒度健康/单模型禁用 | ❌ 没有 | ability 表有 (channel,model) 粒度但只随渠道整体开关；无 channel_model_health 类持久化 |
| Claude Agent SDK 探针 | ❌ 没有 | 测试走标准 relay 请求 |
| HTTP 400 重试 | ⚙️ 可配置 | ShouldRetryByStatusCode 状态码区间可配置，默认排除 400，改配置即可，无需代码 |
| 504/524 重试 | ❌ 且方向相反 | alwaysSkipRetryStatusCodes 硬编码 504/524 永不重试（status_code_ranges.go:31-34），迁移需改代码 |
| Claude 流式空回复检测重试 | ❌ 没有 | ErrorCodeEmptyResponse 仅 Gemini 用；Claude/OpenAI 无空流检测 |
| 渠道级倍率（含 0 免费） | ❌ 完全没有 | 上游只有分组倍率×模型倍率两层 |
| token×model 定向选路 | ⚠️ 语义不同 | 上游有 Channel Affinity（运行时自动记录粘性，服务 prompt cache，pkg/cachex 本地+Redis）；但 fork 是 tbackend 主动下发的管理端定向路由规则，上游覆盖不了，必须迁移。注：上游 affinity 可与迁移后的定向选路并存/互补 |
| logs (token_id,type,created_at) 索引 | ❌ 没有 | token_id 仅单列索引；反正沿用旧库，索引本来就在 |
| SYNC_FREQUENCY 默认 10s | ❌ 默认 60s | 部署 env 设 SYNC_FREQUENCY=10 即可，无需代码 |
| 渠道列表常驻模型级状态 | ❌ 没有 | 仅有手动 ModelTestModal 临时测试展示，不落库 |

### 新版架构要点（影响移植方式）
- pkg/ 新目录（billingexpr 分层计费、cachex、perf_metrics）；types/NewAPIError 结构化错误体系替代旧 OpenAIErrorWithStatusCode；relay 按厂商拆分 handler；setting 子包自注册
- 两套前端同时构建：web/default（新，rsbuild+React19）+ web/classic（旧结构，fork UI 改动可近似 1:1 移植到 classic）
- 新增大量业务：passkey/2FA、订阅支付、checkin、affiliate、casbin 权限、perf metrics
- [x] Agent C：两版数据库表结构差异 —— 已完成，结论见下

## Agent C 结论：数据库差异

### AutoMigrate 自动搞定（沿用旧库直接启动即生效）
- 新建表：perf_metrics、system_instances、system_tasks、system_task_locks、casbin_rule、authz_roles
- users 加列：created_at、last_login_at
- topups 加列：payment_provider
- quota_data 加列：use_group、token_id、channel_id、node_name（含索引）
- logs 加列：upstream_request_id（含索引）
- 预处理迁移（price_amount、model_limits varchar→text）两边相同

### 需人工 DDL / 关注
1. `tokens.key` char(48) → varchar(128)（唯一索引列；AutoMigrate 通常能 MODIFY，扩容安全，需事后确认生效）
2. `logs.idx_created_at_id` 列顺序 (id,created_at) → (created_at,id)：同名索引 AutoMigrate 不重建，需人工 DROP/CREATE 对齐（影响时间范围分页性能）
3. 孤儿索引 `idx_logs_token_type_created_at`（fork 加的）：新版无定义但保留，无害；若迁移 F2 功能则继续有用
4. `channels.ratio` 孤儿列 + 三张 fork 表（token_model_channels/channel_model_disabled/channel_model_health）：AutoMigrate 不删；若对应功能迁移则继续用，若放弃则人工评估 DROP

### DSN/初始化
- SQL_DSN 语义一致（MySQL 生产行为无变化）；新版新增 ClickHouse 仅限日志库（LOG_SQL_DSN）
- root 初始化 / CheckSetup 逻辑一致；无 option 键重命名

## Agent A 结论：老 fork 自定义功能明细

### F1 渠道倍率（channels.ratio 列 + 计费链路）
- `channels` 新列 `ratio`（float，default 1）；`GetRatio()` nil/<=0 回退 1.0（918b17d4 放开为允许 0=免费）
- 计费：模型倍率 × 分组倍率 × **渠道倍率** 第三乘数，覆盖 audio/wss/claude/普通全路径（service/quota.go）
- 日志 `other["channel_ratio"]`（仅 >0 且 !=1 时写）；前端渠道列表可编辑列 + 日志计费明细展示
- 附带独立运营工具页 model-ratio-calculator.html（纯前端，无耦合）

### F2 token×model 渠道亲和（token_model_channels 表）
- 表：id PK + (token_id, model_id, channel_id) 联合唯一 uq_token_model_channel + idx_token_model
- tbackend 写入（分发规则/模型访问配置），new-api 读：进程内存缓存 30s 全量刷新（无 Redis）
- 选路：候选渠道 ∩ 绑定渠道；无记录=不过滤（向后兼容）
- ⚠️ 仅 MEMORY_CACHE_ENABLED=true 时生效

### F3 Claude Agent 探针 + 渠道×模型级健康/禁用
- 表 `channel_model_health`：(channel_id, model) 复合 PK + 连续成败计数 + 观测字段
- 表 `channel_model_disabled`：(channel_id, model) 复合 PK + source(auto/relay/manual) + reason + disabled_at；idx_disabled_channel / idx_disabled_source
- probe sidecar：probe/claude-agent (Node22 + Claude Agent SDK)，容器 Dockerfile.claude-agent-probe，env CLAUDE_AGENT_PROBE_URL/TOKEN/TIMEOUT
- 状态机：失败阈值 → 写 disabled(source=auto)；成功阈值 → 清 auto/relay 保留 manual；渠道列表展示模型级状态
- 手动测试与自动巡检统一走探针

### F4 重试策略强化（b6464d08 / 44fb1149 / 0449dd2b）
- 504/524 继续换渠道重试；HTTP 400 触发重试（保留 skipRetry/已写出/次数用尽保护）
- Claude 流式空回复检测 → empty_response 错误 → 换渠道重试，且豁免渠道禁用

### F5 杂项
- SYNC_FREQUENCY 默认 60s→10s；logs 表 idx_logs_token_type_created_at (token_id, type, created_at)
- docker-compose 改本地构建；Dockerfile.shtlcloud（bun 前端 + goproxy.cn + alpine 运行时）
- CustomOAuthProvider / UserOAuthBinding 两边都有 = 上游功能，非自定义 ✅已排除


## 后端移植设计（Plan agent 产出，已勘察新版插入点）

### 新版架构差异对移植的影响
- 文本计费收敛到 `service/text_quota.go calculateTextQuotaSummary` + `PostTextConsumeQuota`（老 fork 在 compatible_handler.go 的注入点作废）
- 新增 `pkg/billingexpr` tiered_expr 分层计费路径：老 fork 没有的计费面，F1 必须给 tiered 路径补乘渠道倍率（在 service 层 `tiered_settle.go` 出口乘，**不改 pkg/billingexpr 保持纯净**）
- `types.PriceData` 的 otherRatios 机制不适合承载渠道倍率（拒绝 0、tiered/audio/wss 不经过），维持显式 `ChannelMeta.ChannelRatio` 方案
- 自动巡检走 system_task 框架（分布式 per-type 锁 + lease 心跳），老 fork 的进程内锁/裸 goroutine 调度全部删除
- `types.ErrorCodeEmptyResponse` 已存在（Gemini 在用），F4 直接复用

### F1 渠道倍率 — 13 个插入点
1. `model/channel.go`：Channel 加 `Ratio *float64 gorm:"default:1"` + `GetRatio()`（nil 或 <0→1.0，0 合法=免费）
2. `constant/context_key.go`：`ContextKeyChannelRatio`
3. `middleware/distributor.go:481` SetupContextForSelectedChannel 写 context
4. `relay/common/relay_info.go`：ChannelMeta.ChannelRatio + InitChannelMeta 读取 + GetChannelRatio()（nil-safe）
5. `service/text_quota.go calculateTextQuotaSummary`：非按次分支 ratio 补乘 dChannelRatio；按次分支补乘；channelRatio=0 时现有 `!ratio.IsZero()` 保底自动给 quota=0
6. **tiered 路径唯一口径（Codex R2 二次修订，堵死 fallback 双乘）**：**结算链路上 channelRatio 的唯一乘点是 `composeTieredTextQuota` 的最终返回处**，因此所有流入 compose 的输入必须是未乘 ratio 的裸值——包括 `TryTieredSettle` 正常返回的 `ActualQuotaAfterGroup`、tool surcharge 重算值、以及 **error fallback 返回的预扣估算（必须取未乘 ratio 的原始估算值，`modelPriceHelperTiered` 保留 raw 估算字段供 fallback 使用）**。预扣金额本身的 ×ratio 在 `modelPriceHelperTiered` 对独立副本进行，仅用于预扣额度，绝不回流结算路径。公式：`finalQuota = compose(裸值) × channelRatio`，全链路恰好乘一次。测试矩阵：tiered × ratio∈{2,0,缺省} × 带/不带 tool surcharge × **compute error fallback（断言不双乘）** × 预扣/结算一致性
7. `service/quota.go`：QuotaInfo.ChannelRatio + calculateAudioQuota 两分支 + Wss/Audio Pre/Post 填值 + logContent 拼"渠道倍率 %.2f"
8. `relay/helper/price.go`：ModelPriceHelper L120/L126、ModelPriceHelperPerCall L229 补乘；tiered 预扣 EstimatedQuotaAfterGroup 也乘（预扣≈实扣）
9. `service/log_info_generate.go`：appendChannelRatio（>0 且 ≠1 才写 other["channel_ratio"]），挂 GenerateTextOtherInfo/GenerateClaudeOtherInfo/GenerateMjOtherInfo
10. `model/task.go TaskBillingContext` 加 ChannelRatio
11. `controller/relay.go RelayTask` L587 填值
12. `service/task_billing.go`：LogTaskConsumption/taskBillingOther/RecalculateTaskQuotaByTokens 补乘
13. `controller/channel-test.go settleTestQuota` L534 补乘
- 风险：tiered 漏乘/双乘（composeTieredTextQuota 两分支都要改）；ratio=0 免费渠道预扣不为 0 但结算 0→退款路径要验收

### F2 定向选路 — 7 个插入点 + affinity 交互
1. `model/token_model_channel.go` 整文件 1:1 拷贝
2. `model/main.go` migrateDB/migrateDBFast 注册
3. `main.go` InitTokenModelChannelCache()
4. `model/channel_cache.go GetRandomSatisfiedChannel`：加 tokenId 第 5 参，requestPath 过滤后插 FilterChannelsByToken，空则 return nil,nil
5. `service/channel_select.go`：RetryParam.TokenId + 两处透传
6. `middleware/distributor.go`：RetryParam 填 TokenId + **affinity 白名单校验**
7. `controller/relay.go` L181/L510 RetryParam 补 TokenId（重试也受白名单约束）
- **Affinity 交互设计**：token 白名单是硬约束，affinity 是软偏好。affinity 命中渠道必须在 `LoadTokenModelChannels` 允许集内（封装 `IsChannelAllowedForToken`）；白名单拒绝时**不清 affinity 缓存**（用 rejectedByTokenFilter 标志跳过 ClearCurrentChannelAffinityCache——token 受限 ≠ 渠道故障）；Distribute 尾部 RecordChannelAffinity 记录白名单内渠道是期望行为
- **跨 token 缓存覆盖（Codex R4 裁定）**：不同 token 若共享同一 affinity key，缓存条目可能互相覆盖——**正确性由"每请求都过白名单校验"兜底**（污染渠道会被拒绝并重新随机选路，白名单永远不可被绕过），代价只是缓存命中率下降，接受该 tradeoff、不把 token_id 掺进上游 affinity key（避免大改上游语义）。补测试：双 token 同 affinity key 不同白名单 → 各自请求都只落自己白名单内
- 注意：key 用原始 model 名（与 tbackend 写入一致）；MemoryCacheEnabled=false 时过滤不生效（沿用老 fork 已知限制，保留注释）

### F3 探针 + 模型级健康 — 1:1 部分与适配部分
**1:1 拷贝**：probe/claude-agent/ 整目录 + Dockerfile.claude-agent-probe + compose 服务段 + .env 条目；model/channel_model_health.go + channel_model_disabled.go（注册 migrate）；service/claude_agent_probe.go / channel_model_health.go / model_health_error.go；setting/operation_setting/channel_health_setting.go（自注册模式与新版一致）
**适配**：
1. `model/channel_cache.go InitChannelCache`：disabledSet 排除 + RemoveChannelModelFromCache（新版结构微调需手动对）
2. `model/ability.go getPriority/getChannelQuery`：excludeDisabledChannelModels NOT EXISTS 子查询（新版几乎逐行相同）+ FixAbility 孤儿清理
3. `model/channel.go` Update/Delete 清理钩子 + ModelStatuses gorm:"-" 字段
4. `controller/channel.go` GetAllChannels/SearchChannels 调 AttachChannelModelStatuses（批量查询防 N+1）
5. `controller/channel-test.go` 手动测试：探针分流在新版 testChannel 的 InitChannelMeta 之后短路返回（探针路径不走计费）
6. `controller/channel-test.go` 巡检（最大重构点）：probe 循环挂进 `runChannelTestTask`（L997），Anthropic 且探针已配置的渠道走 probe，其余保持 performChannelTests；**删除**老 fork 的 testAllChannelsLock/AutomaticallyTestChannels 等（system_task 接管）；probe 循环每个迭代检查 ctx.Err()（lease 丢锁 cancel）；**passive_recovery 模式（Codex R5 二次修订）**：不能复用 selectChannelsForAutomaticTest（它只选整渠道 AutoDisabled，覆盖不了"渠道启用但单模型被禁"），passive_recovery 下 probe 工作清单**独立从 `channel_model_disabled` 表生成 (channel, model) pair，且限定 `source IN ('auto','relay')`**（manual 禁用永不自动恢复，probe 它是白耗且状态机也不会清它），再按渠道启用状态过滤；进度/汇总按 pair 计数；scheduled_all 仍全量 probe
7. `controller/relay.go processChannelError`：整渠道禁用改为 `DisableChannelModelFromRelay`（模型名空保护）
8. `service/channel.go ShouldDisableChannel`：保留 AutomaticDisableChannelEnabled 门槛（上游语义），只加 ErrorCodeEmptyResponse + IsSoftModelHealthError 两个豁免；模型级禁用开关在 DisableChannelModelFromRelay 内部读 channel_health_setting
- 风险：probe 短路不记账（与普通渠道测试日志形态不同）；多实例双跑防护靠 ctx 检查
- **跨 DB 兼容（Codex R10）**：新仓库 AGENTS 要求 model 层兼容 SQLite/MySQL/PG——channel_model_health 状态机的 `SELECT FOR UPDATE` 按老 fork 方式处理：MySQL/PG 用行锁，SQLite 退化为普通事务（单写者场景可接受），并在代码注释声明生产仅 MySQL；tiered 计费改动实施前先读 `pkg/billingexpr/expr.md` 的文档约束

### F4 重试策略 — 4 个插入点
1. `setting/operation_setting/status_code_ranges.go`：alwaysSkipRetryStatusCodes 清空 504/524（唯一必改代码处）；400 重试**走配置不改代码**：部署时下发 option `AutomaticRetryStatusCodes=100-199,300-399,400-499,500-599`（新版已支持配置，减少上游 diff）
2. `controller/relay.go shouldRetry`：开头加 `c.Writer.Written() → return false`（空流重试安全前提，必须移植）；不硬编码 400。**范围声明（Codex R8）**：F4 只覆盖 chat/text relay；`shouldRetryTaskRelay`（relay.go:616，task 类硬编码 400 不重试）保持上游原样不动（与老 fork 行为一致，task 类请求 400 重试无业务诉求）
3. `relay/channel/claude/relay-claude.go ClaudeStreamHandler L873-894`：HandleStreamFinalResponse **之前**插空流检测（`!Done && ResponseText.Len()==0 && CompletionTokens==0 && !Written()` → ErrorCodeEmptyResponse）——必须在 usage 回填逻辑之前，用老 fork caea1264 之后的最终版条件
4. `service/channel.go`：ErrorCodeEmptyResponse 豁免（与 F3 共用）

### 提交拆分与顺序（每功能独立 commit 便于将来 rebase 上游）
```
commit 1  F4 重试策略（最小、无表结构、独立可回滚）+ 移植空流/重试测试
commit 2  F1 渠道倍率（计费面早落地早暴露）+ channel_ratio/task_billing 测试
commit 3  F2 定向选路 + affinity 白名单交互 + channel_cache token 过滤测试
commit 4  F3-a 健康模型层（两表+状态机+选路排除+模型级禁用）+ 状态机测试
commit 5  F3-b 探针接入（sidecar + 手动测试分流 + system_task 巡检 + 渠道列表状态）
```
F2/F3 都改 channel_cache.go，拆开保证每 commit 语义单一。

### 验收
- 每 commit 后：`go build ./... && go vet ./... && go test ./service/ ./model/ ./controller/ ./relay/... ./setting/...`
- 老 fork 可移植测试：channel_ratio_test / channel_cache_test(token) / channel_model_health_test(388行状态机) / model_health_error_test / claude_agent_probe_test / channel_health_probe_test / relay_retry_test / relay_claude_test 空流用例（并入新版同名文件注意冲突）；channel_auto_test_schedule_test **作废**（老调度已删）
- 手工：ratio=2/0 各打 chat+claude+tiered 核对扣费与 other.channel_ratio；token_model_channels 写入 30s 生效 + affinity 拦截；探针巡检写 channel_model_disabled + 渠道列表 model_statuses；mock 空 SSE 流换渠道重试且不禁渠道

## 前端移植设计（web/default，TS + rsbuild + React19 + TanStack）

### 新前端关键约定
- API 层复用 `features/channels/api.ts updateChannel`（PUT /api/channel/，与老 manageChannel 同后端接口，无需新增）
- i18n：英文原文为 key，译文在 `src/i18n/locales/{en,zh,zh-TW,ja,fr,ru,vi}.json`（老 zh-CN → 新 zh.json）；跑 `bun run i18n:sync` 自动补 key；经常量传入 t() 的 key 必须登记 `src/i18n/static-keys.ts`

### A1 渠道倍率
- `features/channels/types.ts` channelSchema 加 `ratio: z.number().nullish()`
- `channels-columns.tsx`：仿 WeightCell(L232-285) 新建 RatioCell（NumericSpinnerInput min0/max10/step0.1，handleUpdateChannelField 零改动复用），插在 weight 列后；标签聚合行渲染 `-`（tag 批量 ratio 二期）
- `drawers/channel-mutate-drawer.tsx` L3598-3646 Routing Strategy 分区加第三个 FormField ratio；描述常量进 FIELD_DESCRIPTIONS + static-keys.ts
- `lib/channel-form.ts` 4 处：schema `ratio: z.number().min(0).max(10).optional()`、默认值 1、mapChannelToForm `?? 1`、payload 构造 **必须用 `?? 1` 不能 `|| null`**（0=免费渠道会被 `||` 吞掉）
- 可选：channel-card.tsx 移动端 renderCell('ratio')

### A2 模型状态列
- types.ts 加 channelModelStatusSchema + `model_statuses` 数组字段
- channels-columns.tsx status 列后加列（enableSorting:false, mobileHidden）；渲染用**新 UI 形态**：汇总 Badge（如 2/5 异常 danger / 全健康 success）点击弹 Popover 列出每模型 + StatusBadge + source/reason
- i18n key：Model Status / Healthy / Model disabled / Source / Disable Reason / Unknown

### A3 用量日志计费明细 channel_ratio
- `features/usage-logs/types.ts:95` LogOtherData 加 `channel_ratio?: number`
- `columns/common-logs-columns.tsx` L264-281 billing segments 在 Group Ratio 后追加 Channel Ratio segment（非空且 ≠1 才显示）
- `dialogs/details-dialog.tsx` L220-228 Group Ratio 行后追加一行；检查详情弹窗如有单价×ratio 复算逻辑需一并乘 channel_ratio 保持与实扣一致

### A4 SettingsMonitoring 对齐（老 fork 第 5 处 UI 改动）
- 新版对应 `features/system-settings/models/routing-reliability-section.tsx`（上游 rc.20 已重做 Auto-disable/Channel health/Request retry 设置 UI）
- **不照搬老 fork 删改**：只按后端移植后的 channel_health_setting option key 增补探针设置项/文案，保留上游新增能力（与后端 commit 5 联动对齐）

## 部署形态设计

### Dockerfile.shtlcloud（新建于 new-api 仓库）
- 以新版上游 Dockerfile 为底（**双前端构建必须保留**：main.go go:embed 两个 dist，缺一编译失败），3 处适配：builder2 加 `GOPROXY=https://goproxy.cn,direct GOSUMDB=sum.golang.google.cn`；基础镜像可去 digest pin 便于国内 mirror；runtime debian bookworm-slim 保留（TZ 原生生效、healthcheck wget 已装），apt 可换国内源
- VERSION 注入机制与老版相同（module path 同为 github.com/QuantumNous/new-api），原样保留

### probe sidecar 与 compose
- `Dockerfile.claude-agent-probe` + `probe/claude-agent/` 整目录从老 fork 原样拷入；检查新仓库 .dockerignore 放行 probe
- docker-compose.yml 以老 fork 为底：new-api 服务 build 指向 Dockerfile.shtlcloud；env 保留 SQL_DSN / REDIS_CONN_STRING / MEMORY_CACHE_ENABLED=true（定向选路依赖）/ SYNC_FREQUENCY=10 / CLAUDE_AGENT_PROBE_URL+TOKEN+TIMEOUT / TZ / ERROR_LOG_ENABLED / BATCH_UPDATE_ENABLED；新增 NODE_NAME + SESSION_SECRET（升级重启会话不失效）；probe sidecar 段原样；删 postgres
- 风险：双前端构建时长/磁盘显著增加；演练实例必须用独立 Redis DB/实例，避免与生产缓存互串

## 数据库演练与切换 Runbook

### B1 演练库准备（用户已批准清空 tlcloud_twork）
0. **前置核验硬证据（Codex R3 修订）**：清空前必须产出并向用户展示——host + `SELECT DATABASE()`、tlcloud_twork 全表清单+行数+各表最近写入时间（information_schema.tables.update_time / 业务时间列 MAX）、生产服务器上 grep 各服务配置（tbackend .env、systemd、compose）确认无进程仍连 tlcloud_twork；**先对 tlcloud_twork 做一份完整 mysqldump 备份落盘**，再执行清空
1. 清空 tlcloud_twork 全部表（FOREIGN_KEY_CHECKS=0 逐表 DROP，**所有语句 schema-qualified `tlcloud_twork`.`表名`**，禁止 USE 后裸表名）；确认两库无视图/存储过程
2. mysqldump 克隆：`--single-transaction --quick --no-tablespaces --default-character-set=utf8mb4 --set-gtid-purged=OFF --triggers --routines --events` + sed 剥 DEFINER → 导入 tlcloud_twork
3. 行数核对（logs 允许 dump 期间增量差异）

### B2 演练启动与核对
- 用已移植功能的新构建连 tlcloud_twork 启动，确认 AutoMigrate 无 error，逐项 SQL 核对：
  (1) 6 张新表齐全；(2) 8 个新列齐全；(3) tokens.key 是否已 MODIFY varchar(128)（否则手工 ALTER）；(4) logs.idx_created_at_id 列序（预期仍旧序，手工 `DROP INDEX + ADD INDEX (created_at,id), ALGORITHM=INPLACE, LOCK=NONE`，记录耗时供生产窗口参考）；(5) fork 3 表存活 + channels.ratio 未被动
- schema diff 固化 DDL 全集：`mysqldump --no-data` 两库对比，diff 输出即精确变更清单
- **大表 DDL 生产预执行**（切换日前低峰）：`logs ADD COLUMN upstream_request_id + ADD INDEX，ALGORITHM=INPLACE, LOCK=NONE`，把切换窗口 AutoMigrate 压到秒级
- 功能冒烟：管理员登录/渠道两新列/改倍率/定向选路 token 请求/logs.other.channel_ratio/probe 联通/tbackend 测试实例指向演练库跑读写路径（SQLAlchemy 只 SELECT 声明列，新增列有默认值，tokens.key 扩容对其透明）

### B2.5 写库端点预检（Codex R6 修订）
- 切换前在目标库执行临时表 create/drop 验证连接的是**可写主库**（slave 可查询但拒 DDL 的坑）；核对容器内实际生效 env（`docker compose exec new-api env | grep SQL`）
- 生产只配 `SQL_DSN` 单库（无独立 LOG_SQL_DSN），logs 相关 DDL 全部落主库；如未来拆分日志库需单列 DDL 清单

### B3 生产切换
1. 低峰**同时停 new-api 容器和 tbackend 服务（Codex R1 修订：tbackend 直写同库，备份/迁移窗口内必须阻断写入）**；redis/probe 不动
2. 最终 mysqldump 备份 tlcloud_newapi 异机保存（此刻两个写入方都已停，快照干净）
3. compose 切新镜像 up -d，盯到 database migrated + HTTP 就绪
4. B2 核对 SQL 换 tlcloud_newapi 执行；tokens.key 未生效则手工 MODIFY；idx_created_at_id 可当场或另择低峰（只影响性能不影响正确性）
5. 对 new-api 专用 Redis DB FLUSHDB（跨大版本缓存结构可能变化）；MEMORY_CACHE 10s 内重建
6. 验证 new-api：/api/status、后台登录、渠道两列、测试请求（定向选路 token + Claude 渠道）、日志明细渠道倍率、probe 状态刷新
7. 重启 tbackend → 两端冒烟（credentials/models/quota/usage 读写路径）→ 观察 30-60 分钟

### B4 回滚预案
- 回滚 = compose 切回老镜像（tag 保留如 new-api:prod-v0.11.4-fork），**无需还原 DB**（新增表/列老版本忽略；索引列序只是执行计划差异）
- 注意（Codex R9 修订）：新版生成 key 仍固定 48 位（GenerateRandomCharsKey(48)），唯一超长来源是人工造 key——**观察期内约定不手工创建 >48 位 token**；回滚前 `SELECT MAX(LENGTH(key)) FROM tokens` 确认 ≤48（老版 AutoMigrate 会 MODIFY 回 char(48)，超长会阻断启动）。随迁移同步把 tbackend `app/models/new_api.py NewApiToken.key` 的 String(48) 放宽为 String(128)（SQLAlchemy 元数据放宽无副作用）
- 备份仅灾难兜底，常规回滚不动 DB

## 实施里程碑（整体顺序）

工作目录：/Users/craiet/Workspace/fast/tcloud/new-api（新分支，如 `shtelos/main-v1.0.0-rc.20`；git 分支创建/提交按本计划批准执行）

- **M0 spec 同步**：本计划拷贝为 new-api 仓库 `spec/newapi-upgrade-migration.md` 并提交（预授权提交点）
- **M1 后端 commit 1**：F4 重试策略 + 测试移植 → go build/vet/test 绿
- **M2 后端 commit 2**：F1 渠道倍率（13 插入点含 tiered）+ 测试 → 绿
- **M3 后端 commit 3**：F2 定向选路 + affinity 白名单交互 + 测试 → 绿
- **M4 后端 commit 4**：F3-a 健康模型层（两表/状态机/选路排除/模型级禁用）+ 测试 → 绿
- **M5 后端 commit 5**：F3-b 探针接入（sidecar 拷入 + 手动测试分流 + system_task 巡检 + 渠道列表状态 API）+ 测试 → 绿
- **M6 前端 commit**：A1-A4（typecheck + i18n:sync + bun run build 通过）
- **M7 部署 commit**：Dockerfile.shtlcloud + Dockerfile.claude-agent-probe + docker-compose + .env.example 增量
- **M8 DB 演练**：B1→B2 全流程（生产服务器执行，需用户配合提供连接/确认）
- **M9 生产切换**：B3（低峰窗口，用户拍板时间）

## Database / DDL
- AutoMigrate 自动：6 新表 + 8 新列（见 Agent C 清单）
- 手工 DDL（演练确认后固化）：tokens.key MODIFY varchar(128)（如 AutoMigrate 未生效）；logs.idx_created_at_id 重建 (created_at,id)；生产预执行 logs ADD COLUMN upstream_request_id + 索引
- fork 3 表 + channels.ratio 随功能迁移继续使用，无 DROP

## Production Impact
- 切换窗口：**new-api 与 tbackend 同时停**（与 B3 一致，阻断双写方；大表 DDL 预执行后 AutoMigrate 秒级，窗口分钟级）；twork 客户端在窗口内登录/发消息/网关请求失败（恢复后自动可用）
- 回滚：镜像回切，DB 不动

## Release Notes（内部）
- new-api 网关升级至上游 v1.0.0-rc.20，保留渠道倍率/定向选路/Claude 探针/重试策略四项自研能力；管理后台切换全新 UI（web/default）

## Test Plan
- 每 commit：go build ./... && go vet ./... && go test ./service/ ./model/ ./controller/ ./relay/... ./setting/...
- 老 fork 测试移植清单见后端设计"验收"节；前端 bun typecheck + build；演练库全链路冒烟 + tbackend 兼容冒烟
- 生产：切换后验证清单 B3-6

## Manual Acceptance Prompt
- 管理后台（新 UI）渠道列表能看到并编辑"渠道倍率"，设 0 的渠道请求计费为 0
- 渠道列表能看到模型级健康状态；Anthropic 渠道手动测试走探针
- 指定了分发规则的用户（token_model_channels 有记录）请求只落在指定渠道
- 上游 400/504/空流自动换渠道重试，twork 端不再出现"未收到回复"的 0 token 假成功
- CMS 用量分析、twork 消费统计数据连续（logs 表口径不变）

## Codex 对抗审查

### 第 1 轮（verdict: 需修订，10 条：3 blocker / 6 major / 1 minor）
- R1 blocker｜生产切换只停 new-api 不停 tbackend，备份/迁移窗口写库竞态 → **已修订**：B3 窗口内同时停 tbackend，恢复后两端冒烟
- R2 blocker｜tiered 渠道倍率乘法缺唯一口径，双乘/漏乘风险 → **已修订**：唯一乘点定在 composeTieredTextQuota 最终返回处，TryTieredSettle/billingexpr 不乘，预扣估算单独乘；补测试矩阵
- R3 blocker｜清空 tlcloud_twork 缺硬证据与保护 → **已修订**：清空前产出 host/表清单/行数/最近写入/配置 grep 证据并先全量备份，DROP 语句 schema-qualified
- R4 major｜affinity 跨 token 缓存污染 → **已裁定**：正确性由每请求白名单校验兜底（不可绕过），接受缓存命中率损失，不改上游 affinity key；补双 token 测试
- R5 major｜passive_recovery 覆盖不了"渠道启用但单模型禁用" → **已修订**：passive 模式 probe 工作清单独立从 channel_model_disabled 生成 pair
- R6 major｜写库端点/LOG_SQL_DSN/DDL 风险控制不足 → **已修订**：新增 B2.5 写库端点预检（临时表 create/drop + 容器内 env 核对）；明确生产单库无 LOG_SQL_DSN
- R7 major｜目标基线是 v1.0.0-rc.20-7 非纯 tag → **已修订**：基线冻结 commit a79f9691，实施期不跟上游
- R8 major｜task relay 硬编码 400 不重试 → **已修订**：显式声明 F4 只覆盖 chat/text，task relay 保持上游（与老 fork 一致）
- R9 major｜tokens.key 128 回滚兼容 + tbackend ORM 仍 48 → **已修订**：观察期不手工造长 key + 回滚前校验 + tbackend ORM 放宽到 String(128)
- R10 minor｜跨 DB 兼容与 billingexpr 文档约束 → **已修订**：FOR UPDATE 按 DB 类型分支 + 实施前读 pkg/billingexpr/expr.md

### 第 2 轮（verdict: 需修订，核销 7/10，残留 R1/R2/R5）
- R1 残留｜Production Impact 文案仍写"tbackend 不停"与 B3 矛盾 → **已修订**：统一为窗口内 new-api 与 tbackend 同停
- R2 残留｜TryTieredSettle error fallback 返回已乘 ratio 的预扣估算，compose 再乘会双乘 → **已修订**：结算链路唯一乘点在 compose，所有流入 compose 的输入（含 fallback 用的估算）必须是未乘 ratio 裸值，helper 保留 raw 估算字段；补 fallback 不双乘断言
- R5 残留｜passive pair worklist 未排除 manual 禁用 → **已修订**：限定 source IN ('auto','relay')
- R3/R4/R6/R7/R8/R9/R10 已核销

### 第 3 轮（收口）
- R1/R2/R5 全部核销（B3 与 Production Impact 口径一致；tiered 唯一乘点 + fallback 裸值约束落文；passive worklist 排除 manual）
- 佐证：Codex 历史会话记录过同类事故（私网 RDS 可 select 1 但 polar slave 写保护导致 migration 失败被迫回滚），B2.5 写库端点预检正是针对此坑

verdict: 通过
