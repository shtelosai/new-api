# new-api 模型健康机制改造：停用全量巡检 + 被动探活恢复 + 手动测试即恢复

## Context

生产 new-api 网关（fork，含自研「渠道×模型」级健康状态机）当前的模型健康机制有三个问题：

1. **手动测试成功 ≠ 恢复上线**：管理后台对被禁用模型点「测试」成功后，后端只返回耗时（`controller/channel-test.go` `TestChannel`），不清 `channel_model_disabled` 禁用行、不刷缓存，模型仍处于下线状态，违背后台操作直觉。
2. **每 10 分钟定时巡检浪费费用**：`scheduled_all` 模式对全部渠道发测试请求（Anthropic 渠道逐模型探针、其他渠道测默认模型），且巡检失败还会主动禁用——而真实用户请求失败本来就会触发 relay 即时禁用并（对普通可重试请求）自动负载到下一渠道，主动巡检的禁用价值低、成本高。
3. **被禁用模型的恢复链路残缺**：现有 `passive_recovery` 模式中，`buildPassiveRecoveryProbeTargets` 要求渠道必须是 **Anthropic 类型且配置了 Claude Agent 探针**（`shouldProbeChannelModels`），导致 Gemini/DeepSeek/GPT 等渠道的模型被 relay 禁用后**永远不会被探活恢复**；且全系统没有任何「解除模型禁用」的管理入口，无法探活的模型只能直接改数据库恢复。

目标形态（用户已确认）：
- **禁用只来自真实流量**：保留 relay 失败即时模型级禁用（`DisableChannelModelFromRelay`，软错误 429/过载已豁免）。
- **停止主动定时巡检**：定时任务切到 `passive_recovery`，只探活 `channel_model_disabled` 中 source∈(auto,relay) 的模型，间隔 3 分钟；禁用表为空时零探活请求。
- **探活成功一次即恢复**：清 auto/relay 禁用、**保留 manual**、刷新渠道缓存（复用现有 `HandleConfirmedProbeResult` → `ApplyConfirmedProbeResultInTx` 事务语义）。
- **手动测试成功立即恢复**：同上语义，不等下一轮探活。
- 手动「测试全部渠道」按钮**保留现状**（`TestAllChannels` 显式提交 `scheduled_all`、允许禁用——管理员显式排查动作，误禁下一轮探活即恢复）。

## 现状关键事实（已核实）

- `runProbe`（`controller/channel-test.go:1088`）内部调 `testChannel(channel, model)`，Claude Agent 探针只是 `testChannel` 内部对 Anthropic 的短路（`shouldUseClaudeAgentProbe`）。但 `runProbe` 当前**硬编码非流式**、端点自动推断（`getModelHealthProbeEndpointType` 恒空）；Codex 类渠道的渠道级自动测试要求流式（`shouldUseStreamForAutomaticChannelTest`），模型探针未接该规则。
- `testChannel` 的自动请求构造覆盖：chat completions（默认）、embedding/rerank 命名启发式、VolcEngine seedream 图像、responses；**没有 TTS/ASR/视频构造**，后端 `EndpointType` 无音频类型——此类模型无法被正确探活，也无法通过手动测试成功。
- `HandleConfirmedProbeResult(success=true)` → `ApplyConfirmedProbeResultInTx`（`model/channel_model_health.go:250`）：事务内 `DELETE ... WHERE source IN ('auto','relay')`（保留 manual）、重置健康计数，有删除行时 `InitChannelCache()` 全量重建缓存。无禁用行时为廉价幂等 no-op。
- `InitChannelCache`（`model/channel_cache.go:26`）真实容错语义：**仅**禁用表读取失败会提前返回保留旧缓存；channels/abilities 读取错误被忽略、极端情况下可能以空数据替换缓存——这是**预存在行为**（全仓 30+ 调用点共享），本次不改热路径；周期 `SyncChannelCache`（生产 `SYNC_FREQUENCY=10s`）持续重建兜底。
- relay 模型级禁用的完整前置：`common.AutomaticDisableChannelEnabled=true` + `channel_health_setting.model_level_auto_disable_enabled=true` + **该渠道 `auto_ban=1`**（`GetAutoBan()`，`nil` 视为 false）。
- `GetMonitorSetting()` 中 env `CHANNEL_TEST_FREQUENCY` 会**强制覆盖回 scheduled_all**；`CHANNEL_TEST_ENABLED` 会覆盖后台开关。
- 定时任务经 system_task DB 租约锁跨实例去重；`passive_recovery` 模式 `allowDisable=false`。
- 前端「系统设置」已有 `channel_test_mode` / `auto_test_channel_minutes` 配置 UI；渠道测试弹窗单模型/批量测试后**已有**列表刷新（`channel-test-dialog.tsx` `refreshChannelLists`），前端无需新增刷新逻辑。
- **移植遗留 bug**：`getModelProbeTimeoutSec()`（`channel-test.go:1186`）误用 `common.ChannelDisableThreshold`（默认 5s）；`channel_health_setting.probe_timeout_sec`（默认 20s）从未被使用。该超时由 `runAllChannelModelProbes` 共享——修复后 **scheduled_all 的 Anthropic 探针超时同样 5s→20s**（预期内的行为变化，5s 本来就是错的）。
- 恢复键口径：relay 禁用写入的是用户请求的原始模型名；手测 `?model=` 传的也是原始名。handler 层需统一 `strings.TrimSpace` 后再用于测试与恢复（`testChannel` 只 trim 内部副本）。
- 生产已于 2026-07-10 完成 rc.20 迁移版切换（`9c56dd63`）；当前分支另有 2 个未发布 UI commit（模型状态圆点），本次发布将一并带上。

## 改动方案

> 执行顺序采用测试先行：每个里程碑先补能暴露现状缺口的行为测试，再实现让其通过。

### M1 后端：手动单模型测试成功 → 模型级恢复

文件：`controller/channel-test.go`、`service/channel_model_health.go`

1. `HandleConfirmedProbeResult` 增加返回值 `error`（只返回 error；现有巡检调用点忽略 error 仅记日志，行为不变）。
2. `TestChannel` handler（`:952`）：`testModel := strings.TrimSpace(c.Query("model"))`，测试、非空判断、恢复键统一用该值。成功分支（`localErr == nil && newAPIError == nil`）且 `testModel != ""` 时调用 `service.HandleConfirmedProbeResult(channelId, testModel, true, "", latencyMs, 1)`。
   - 恢复事务返回 error → 接口返回 `success:false` + 明确错误消息。
   - **接口成功语义 = 禁用行已从 DB 清除**（DB 为唯一真值）；内存缓存重建为 best-effort，由 10s 周期同步持续兜底（缓存容错是预存在行为，本次不改）。此语义写入代码注释与 AGENTS.md 契约。
   - 不带 `model` 参数的渠道级测试**不触发恢复**；手动测试**失败**分支维持现状（不进状态机、不禁用）；响应结构不新增字段。
3. manual 禁用行为：测试成功照常返回成功，但 manual 行不被自动清除，列表刷新后仍显示禁用；解除走 M2 显式动作。

### M2 后端+前端：「解除禁用」显式管理动作 + manual 硬锁语义修复

文件：`controller/`（新 endpoint）、`router/channel-router.go`、`model/channel_model_disabled.go`、`service/channel_model_health.go`、`web/default/src/features/channels/`（列组件 + api.ts）、i18n locales

1. **manual 硬锁语义修复**：现有 `DisableChannelModelFromRelay` 的 Upsert 会无条件把 manual 行覆盖为 relay（`channel_model_health.go:88` 注释自认故意）——在被动探活的新世界里，异步在途失败/指定渠道请求可把 manual 覆盖成 relay，随后被探活自动清除，管理员意图被静默撤销。改为：relay 禁用写入在事务内先查现有行（复用现有行锁模式，三库兼容），**source=manual 时跳过覆盖**（模型本就已禁用，只做 `RemoveChannelModelFromCache` 幂等摘除），补竞态回归测试。用户原始诉求「保留 manual 禁用」由此成为可靠契约。
2. 新增管理端点（挂 `RequirePermission(authz.ChannelOperate)` 细粒度权限，与渠道管理动作一致）：`DELETE /api/channel/model-disabled?channel_id=&model=`。行为：TrimSpace + 非空/长度校验 → **单事务**删除该 (channel,model) 禁用行（**任意 source，含 manual**——显式管理动作区别于自动恢复）并重置健康计数（复用现有事务+行锁模式）→ 提交后 `InitChannelCache()`。幂等：行不存在返回成功。响应含 `changed` / `previous_source`（供前端提示与审计）。
3. **审计**：登记精确审计记录（channel_id、trim 后 model、previous_source、changed），不依赖 generic 兜底审计（其不记录 query 参数），保证"谁解除了哪个人工锁"可追责。
4. 前端：模型状态展示从纯 Tooltip 改造为**可交互 Popover**（当前圆点是 Tooltip 内 10px span，无法承载按钮），disabled 模型显示「解除禁用」按钮：api.ts 请求封装 + 确认提示 + 防重复提交 + 按 ChannelOperate capability 门控 + 成功后刷新列表。i18n 补 en/zh key。**已知限制**：模型状态列 `mobileHidden`，移动端/卡片视图暂无该入口（管理操作以桌面为主，记录为已知限制，不在本次补）。
5. 该动作是 TTS/ASR/视频等**无法构造探活请求**的模型的恢复通道，也用于生产验收的清理步骤。解除禁用后模型状态如实显示为 unknown（灰，无最近成功记录），待下次成功请求/测试转绿——验收按此措辞。

### M3 后端：passive_recovery 收窄为纯模型级探活恢复（扩大渠道类型覆盖）

文件：`controller/channel-test.go`

1. `buildPassiveRecoveryProbeTargets`（`:1226`）过滤条件放宽：由 `shouldProbeChannelModels(channel)` 改为 `channel.Status == common.ChannelStatusEnabled && !isUnsupportedProbeChannelType(channel.Type)`。
2. **协议等价性界定**：
   - `runProbe` 的 `isStream` 由硬编码 `false` 改为 `shouldUseStreamForAutomaticChannelTest(ch)`（只对 Codex 类渠道生效；scheduled_all 的探针 targets 仍仅 Anthropic，不受影响——已核实该函数对 Anthropic 返回 false）。
   - passive 探活**可靠覆盖 = chat 类模型（默认构造）+ Anthropic 渠道（探针短路）**。embedding/rerank/seedream 等启发式在 `requestPath` 判定与 `buildTestRequest` 构造两处不对称（判定为 image/embedding 但自动分支仍构造 chat DTO，可能类型不匹配失败），只能算尽力而为，本次不统一两处启发式。**TTS/ASR/视频类模型被禁用后探活会以错误协议持续失败而维持禁用**：代价为每轮一次小失败请求（通常上游即时 4xx），恢复通道为 M2「解除禁用」。此边界与代价如实写入 AGENTS.md 契约与后台文案（承诺范围限定为可靠覆盖的模型）。
3. passive 模式下**每个 (channel,model) 每轮只探活 1 次**（不做同轮 FailureThreshold 重试）。`scheduled_all` 的探针路径保持现有重试语义不变。
4. passive 模式下探活**失败不进状态机**：只 `UpdateHealthObservability`，不改写禁用行 source/reason；成功仍走 `HandleConfirmedProbeResult(true)` 一次成功即恢复。
5. passive 模式**不再调用 `performChannelTests`**：不再对 `channels.status=AutoDisabled` 的整渠道每轮发默认模型测试。行为变化见 Production Impact。
6. `scheduled_all` 与手动「测试全部渠道」的集合、重试、禁用/恢复副作用不变（例外：M4 的探针超时修复对 scheduled_all Anthropic 探针同样生效，属预期修复）。

### M4 后端：修复探活超时死配置

文件：`controller/channel-test.go`

- `getModelProbeTimeoutSec()` 改为读 `operation_setting.GetChannelHealthProbeTimeoutSec()`（默认 20s）。**影响范围如实声明**：passive 与 scheduled_all 的模型级探针共享该超时，两者都从 5s 修正为 20s（5s 是移植错误，SDK 冷启动探针必然超时误判）；渠道级测试的响应时间禁用阈值（`ChannelDisableThreshold`）语义不变。补两条超时来源回归测试（passive + scheduled_all 路径）。

### M5 前端与文档

文件：`routing-reliability-section.tsx`、`web/default/src/i18n/locales/*.json`、`AGENTS.md`

1. passive_recovery 模式与间隔的后台说明文案修正为「对失败禁用的模型探活恢复」语义，注明非 chat 类模型探活可能持续失败、需「解除禁用」动作恢复。
2. 手动测试后的列表刷新**现状已满足**（确认后零改动）。
3. i18n：`bun run i18n:sync` 同步（en 源 key + zh 必须准确）。
4. 实现完成后在 `AGENTS.md` 追加 ≤15 行模型健康契约：passive 只处理 auto/relay 禁用、失败仅观测不进状态机、成功一次恢复、探活超时读 `probe_timeout_sec`（passive 与 scheduled 探针共享）、手测无显式 model 不恢复、手测成功语义=DB 已清除+缓存周期同步最终一致、非 chat 模型探活覆盖不到走「解除禁用」。

### M6 本地验证

- 后端：定向包测试（`controller`、`service`、`model`）→ 全量 `go build ./...` + `go vet ./...`（仅上游基线告警）+ `go test ./...` 零 FAIL。
- 前端（web/default）：`bun run typecheck` + `bun run lint` + `bun run format:check` + `bun run build` 全绿。
- 镜像：`Dockerfile.shtlcloud` 构建通过；本地 compose 校验（自动巡检保持关闭，避免真实上游费用）。

### M7 生产部署与切配置

1. **发布物可复现确认**：改动经用户确认后先提交；从指定 SHA 的 clean checkout 构建；镜像 tag 含 short SHA（如 `new-api:twork-<shortsha>`）；记录 `docker image inspect` 的 immutable image ID；部署后 `docker inspect new-api --format '{{.Image}}'` 与目标 image ID 对照；回滚按旧容器 image ID（不依赖可移动 tag）。发布基线含分支上 2 个未发布 UI commit。
2. 服务器只读确认实际 compose、容器内实际生效 env：无 `CHANNEL_TEST_FREQUENCY` / `CHANNEL_TEST_ENABLED`；`SKIP_AUTO_MIGRATE=true` 生效。
3. **auto_ban 审计**：`SELECT COUNT(*) FROM channels WHERE status=1 AND (auto_ban IS NULL OR auto_ban != 1)`（NULL 视为不参与）——这些渠道的模型不参与 relay 禁用/探活恢复，向用户列出确认。
4. 后台先置 `monitor_setting.auto_test_channel_enabled = false`，确认无 pending/running 的 `channel_test` system task。
5. 备份当前镜像 image ID、compose、相关 option 值（独立回滚点）。
6. 构建新镜像、仅重建 new-api 容器（sidecar 不动）；验证 `/api/status`、容器健康、sidecar `/health`、image ID 对照。
7. 两步切配置（避免中间态触发旧全量巡检）：
   - 第一步（保持 disabled）：`channel_test_mode=passive_recovery`、`auto_test_channel_minutes=3`；核对 `AutomaticDisableChannelEnabled=true`、`model_level_auto_disable_enabled=true`、`probe_timeout_sec=20`。
   - 第二步：单独打开 `auto_test_channel_enabled=true`。
8. Canary 验收（见 Manual Acceptance，含隔离与清理要求）。

回滚：关定时任务 → 按 image ID 恢复旧容器与配置；无数据库回滚需求。

## 边界情况

- **manual 禁用**永不被手测或探活自动清除，且经 M2.1 修复后不再被 relay 写入覆盖（成为可靠人工锁）；解除唯一通道是 M2 显式动作。
- **Midjourney/Suno/Kling/视频类渠道**（`unsupportedProbeChannelTypes`）：不进探活 targets，维持禁用，走「解除禁用」。
- **TTS/ASR/视频等非 chat 模型**（chat 类渠道上）：探活按 chat 构造持续失败 → 维持禁用 + 每轮一次小失败请求；恢复走「解除禁用」。相比现状（永不探测、无任何恢复入口）：多了失败探针成本、也多了恢复通道。
- **渠道整体禁用或已删除**：`buildPassiveRecoveryProbeTargets` 跳过非 Enabled 渠道与孤儿禁用行。
- **手测成功但无禁用行 / 解除禁用但行不存在**：幂等廉价 no-op。
- **恢复事务 DB 失败**：模型保持禁用，手测接口返回失败；探活路径记日志下轮再试。
- **DB 已清但缓存重建异常**：接口按"DB 已清除"返回成功；缓存容错为预存在行为（禁用表读失败保留旧缓存，其余错误 best-effort），10s 周期同步持续兜底。
- **探活恢复了实际仍坏的模型**：下一个真实用户请求再次 relay 禁用；普通可重试请求通常有跨渠道兜底（指定渠道/亲和性请求除外）；震荡上限每模型每轮一次。
- **软错误（429/过载/无可用账号）**：`IsSoftModelHealthError` 豁免，真实流量限流不禁用；探活失败也不写禁用。
- **`auto_ban != 1`（含 NULL）的渠道**：relay 失败不禁用其模型，不进探活；部署前审计确认。
- **多实例**：system_task DB 租约锁保证单实例探活；缓存即时刷新只在执行实例，其他实例靠周期同步（生产当前单实例）。
- **恢复时延语义**：可探活模型 = 下一轮调度（间隔 3 分钟 + 调度扫描抖动 ~15s + 同渠道内串行排队、每个最多 20s），典型 3~4 分钟；不可探活模型 = 人工「解除禁用」。

## Database / DDL

无。复用现有 `channel_model_disabled` / `channel_model_health` 表与 options 配置（仅 options 表 DML），无新表、无新列、无迁移。

## Production Impact

- **省费用**：每 10 分钟的定时巡检请求消失；探活只打禁用模型、每轮每模型 1 次；正常时禁用表为空 = 零探活请求。Anthropic 探针经 sidecar 发最小请求（`maxTurns:1`/`max_tokens:1`），**仍产生上游调用与少量费用**，规模极小。
- **恢复时延改善**：可探活的禁用模型从「非 Anthropic 渠道永不恢复 / Anthropic ≤10 分钟」变为「典型 3~4 分钟自动探活」或「手动测试立即恢复」；不可探活模型获得「解除禁用」人工通道（此前只能改库）。
- **行为变化（需知晓）**：①`channels.status=3` 渠道（余额不足等）不再被 passive 轮次测试与自动恢复，充值后需手动启用；②scheduled_all 的 Anthropic 探针超时 5s→20s（修复移植错误）；③非 chat 被禁用模型会产生每轮一次的小失败探针请求。
- 风险：坏模型被探活误恢复 → 真实用户一次失败再禁用（普通可重试请求有跨渠道兜底）；部署重启短暂中断（前次 cutover 实测 ~4-6s，低峰执行）。

## Test Plan

Go（testify require/assert、显式 fixture、SQLite/MySQL/PG 兼容；先写测试暴露缺口再实现）：
- **手测恢复**：auto/relay/manual 行 fixture → 显式 model 测试成功后 auto/relay 清、manual 保留、计数重置；model 带首尾空格 trim 回归；不带 model 不清除；测试失败不清除；恢复事务 error 传播到接口失败响应（测试侧 DB 故障手段，不加生产注入框架）。
- **解除禁用端点**：删任意 source 行（含 manual）+ 同事务重置健康计数 + 幂等（行不存在成功，changed=false）+ 权限（有 ChannelOperate/root → 成功；管理员但无 operate → 403，按 `RequirePermission` 语义）+ 响应含 changed/previous_source。
- **manual 硬锁竞态回归**：已存在 manual 行时 `DisableChannelModelFromRelay` 不覆盖 source（仍摘缓存）；auto/relay 行照常覆盖。
- **passive targets**：非 Anthropic 渠道的 auto/relay 禁用行生成 target；manual 行、非 Enabled 渠道、孤儿行、unsupportedProbeChannelTypes 排除。
- **passive 单次探活 + 失败不改禁用行**：失败恰好 1 次请求；source/reason 不变；成功一次清 auto/relay。
- **流式规则**：Codex 类渠道探活 `isStream=true`；Anthropic 探活仍非流式（`shouldUseStreamForAutomaticChannelTest` 接线回归）。
- **超时来源**：passive 与 scheduled_all 探针路径均读 `channel_health_setting.probe_timeout_sec`（两条回归）。
- **scheduled_all 回归**：探针路径保持 FailureThreshold 同轮重试与禁用/恢复语义。
- 全量：`go build ./...`、`go vet ./...`、`go test ./...` 零 FAIL。

前端：`bun run typecheck` + `bun run lint` + `bun run format:check` + `bun run build` 全绿；i18n sync 后 en/zh key 就位。

## Release Notes

- 手动单模型测试成功后立即恢复被自动/流量禁用的模型上线（人工禁用不受影响）
- 新增「解除禁用」管理动作：可显式解除任意模型禁用（含人工禁用与无法探活的模型），带精确审计
- 人工禁用（manual）成为可靠硬锁：不再被真实流量失败覆盖为 relay 后被探活误恢复
- 渠道巡检切换为被动恢复模式：停止定时全量测试，仅对被禁用模型每 3 分钟探活一次，成功即恢复
- 探活恢复扩展到全部常规渠道类型（此前仅 Anthropic）；Codex 类渠道探活对齐流式规则
- 修复探活超时误用响应时间禁用阈值（5s→20s 专用配置）导致慢模型误判、永不恢复的问题

## Manual Acceptance Prompt

> 隔离要求：全程使用**专用 Canary 渠道**（独立分组/模型别名、无生产 token 可达，不承载生产流量）；每步开始前 `SELECT` 快照 `channel_model_disabled` **与 `channel_model_health`** 相关行及该渠道 `response_time`/`test_time`；每个场景结束后立即清理；全部完成后核对上述 DB 状态、缓存、渠道状态相对测试前**零残留**（探活/手测会写 health 表与渠道测试字段，需一并恢复核对）。

1. Canary 渠道造模型级禁用（SQL 插入 source='relay'）→ 圆点红、模型不可选路。
2. 带 model 手动测试成功 → 接口成功 + 禁用行清除 + 模型可选路 + 圆点绿。
3. 再造禁用行，等下一轮探活（典型 3~4 分钟）→ 日志 `[channel_health] auto-recover` + 圆点绿；确认该轮对该模型只发 1 次探活请求。
4. 造 source='manual' 行 → 手测成功与探活均不清除 → 用「解除禁用」按钮清除 → 状态转 unknown（灰），下次成功测试转绿。【清理动作即验收对象】
5. 非 Anthropic Canary（如 Gemini/DeepSeek 渠道）重复步骤 3 → 同样下一轮恢复。
6. 终态核对：`channel_model_disabled` 无 Canary 残留行、渠道状态与测试前一致。
7. 观察 24h：无定时全量巡检流量、上游测试费用下降；真实流量失败仍即时禁用并切换渠道；无禁用行时探活零请求。

## Codex 对抗审查

- Round 1（盲出方案）：Codex 独立方案与本计划方向一致；吸收其 9 项要点（passive 单次探活失败不进状态机、仅显式 model 触发恢复、恢复失败传播 error、去 performChannelTests、probe_timeout_sec 死配置修复、测试先行、两步切配置、文案 i18n、AGENTS.md 契约），保留本计划的前端反馈闭环关注点，砍掉原有的 testedModel/recovered 字段（过度设计）。
- Round 2（首轮复审）：verdict=需修订。BLOCKER：passive 探针协议等价性（非流式硬编码、非 chat 模型按 chat 构造）→ 修订 runProbe 流式接线 + 范围界定。SHOULD 全部吸收：auto_ban 审计、TrimSpace、缓存语义、时延口径、重试兜底限定、sidecar 非免费、发布物确认、AGENTS.md 契约落地、lint/format。NIT 吸收：只返回 error、前端刷新零改动、成本基线表述。
- Round 3（二轮复审）：verdict=需修订。BLOCKER1：非 chat 模型手动恢复兜底不存在（无 TTS/ASR 测试构造与端点类型）→ 修订：新增 M2「解除禁用」显式管理动作（任意 source、幂等、刷缓存）作为真实恢复通道，探活承诺范围限定为可探活模型。BLOCKER2：生产验收留 manual 残留 → 修订：Manual Acceptance 增加专用 Canary 渠道隔离、快照、逐场景清理、终态零残留核对。SHOULD 吸收：InitChannelCache 容错语义按真实行为改写（预存在行为不改热路径）、探活超时影响 scheduled_all Anthropic 探针如实声明+双路径回归测试、发布物可复现（clean checkout+SHA tag+image ID 对照+按 image ID 回滚）、auto_ban 审计 SQL 计入 NULL。
- Round 4（三轮复审）：verdict=需修订。上轮 2 BLOCKER + 3 SHOULD 全部核销通过。新 BLOCKER：manual 非可靠人工锁——relay Upsert 会覆盖 manual→relay（在途异步失败/指定渠道旁路可触发），随后被探活自动清除 → 修订 M2.1：relay 禁用写入事务内先查、source=manual 跳过覆盖（用户原始诉求"保留 manual 禁用"由此成为可靠契约）+ 竞态回归测试。SHOULD 吸收：端点权限钉死 `RequirePermission(authz.ChannelOperate)`（403 语义按此测试）、精确审计（channel_id/model/previous_source/changed）、删除+健康重置单事务化+响应 changed/previous_source、前端明确 Tooltip→可交互 Popover 改造（移动端无入口记为已知限制）、探活可靠覆盖收窄为 chat+Anthropic 探针（embedding/seedream 启发式两处不对称标尽力而为）、零残留验收补 health 表与渠道测试字段+Canary 隔离细化（独立分组/别名/无生产 token）、解除禁用后状态如实 unknown。
- Round 5（终轮复审）：因 Codex 用量限额未执行（第 5 次调用失败）。Round 4 的 1 BLOCKER + 6 SHOULD 已全部按其建议方向修订入本计划，无遗留分歧、无未处置 finding；最终版 M2（manual 硬锁修复+解除禁用端点）未经 Codex 复核。**2026-07-10 用户裁决：终审由用户人工审阅替代，直接放行**（执行阶段逐里程碑验收独立把关）。

verdict: 通过
