# 模型健康被动探活改造 执行日志

执行方式：用户指示 Claude 直接执行（未走 Codex 执行流程）。发布 commit：`a61f1246`。

## M1 手测恢复 —— ✅
- `HandleConfirmedProbeResult` 返回 error；`TestChannel` trim model、显式 model 成功即恢复、恢复失败返回失败响应。
- 测试：service 4 用例（manual 保留 / relay 清除 / tx 失败传播 / relay 禁用语义）。

## M2 解除禁用 + manual 硬锁 —— ✅
- `UpsertChannelModelDisabledPreservingManual`（relay 不覆盖 manual，行锁事务）+ `ClearChannelModelDisabledInTx`（任意 source、单事务重置健康、幂等）。
- `DELETE /api/channel/model-disabled`：`RequirePermission(ChannelOperate)` + 精确审计 `channel.model_disabled_clear`（含 previous_source/changed）。
- 前端：禁用红点 Tooltip → 可交互 Popover +「解除禁用」按钮（两步确认/防重复/OPERATE 门控/成功刷新）；移动端无入口记为已知限制。
- 测试：model 4 + controller 2 + router 权限绑定 1。

## M3 passive 收窄 —— ✅
- targets 放宽到全部常规渠道类型（排除 Midjourney/视频类）；`relayChannels=nil` 不再测整渠道；每 pair 每轮 1 次；失败不进状态机（`shouldSkipProbeHealthStateMachine(result, recoveryOnly)`）；runProbe 流式对齐 `shouldUseStreamForAutomaticChannelTest`。

## M4 超时死配置 —— ✅
- `getModelProbeTimeoutSec` 改读 `probe_timeout_sec`；生产旧口径 `ChannelDisableThreshold=30`，故 options 落 `channel_health_setting.probe_timeout_sec=30`（非默认 20）。

## M5 文档/i18n —— ✅
- 后台 passive 文案更新、七语言 sync（en/zh 人工校准）；AGENTS.md 追加模型健康契约 5 条。

## M6 本地验证 —— ✅
- `go build` OK / `go vet` 仅上游基线告警 / `go test ./...` 零 FAIL（router/model/service/controller 重点包单独复跑绿）。
- 前端 `typecheck`、`build` 绿；`lint` 基线 507 errors → 507→506（我的文件零新增且净修一个）；`format:check` 我的文件干净（10 个基线脏文件未动）。
- 本地 Docker 镜像构建跳过（生产历来服务器构建，与 M7 合并验证）。

## M7 生产部署 —— ✅（2026-07-10 12:37）
- 发布物：commit `a61f1246` → 服务器 `/www/twork/new-api-build` clean checkout 构建 `new-api:twork-a61f1246`（image `99b7fdcf7a99`）；旧镜像 `dafd644ace86`（tag twork-v1rc20）。
- 部署前核查：容器 env 无 `CHANNEL_TEST_FREQUENCY`/`CHANNEL_TEST_ENABLED`，`SKIP_AUTO_MIGRATE=true`；先关 `auto_test_channel_enabled` 并等在跑 channel_test 任务 succeeded。
- 回滚点：`/www/twork/backups/newapi-passive-recovery-20260710/`（旧 image id + compose + options + 禁用表快照）。
- 切换：compose image tag 改 `twork-a61f1246`，`docker-compose up -d --no-deps new-api`，停机 ~11s，healthy + `migration skipped`。
- 配置（两步）：`channel_test_mode=passive_recovery`、`auto_test_channel_minutes=3`、`probe_timeout_sec=30` → 最后 `auto_test_channel_enabled=true`。

## 线上验收（首两轮）
- 12:39 轮：tested=11（仅禁用模型，对比旧全量 72）、succeeded=1、**enabled=1**（channel74 claude-opus-4-6 一次成功即恢复）、disabled=0（失败不新增禁用）✅
- 12:43 轮（间隔精确 3 分钟）：tested=10、enabled=2（含 claude-sonnet-4-6）✅
- 禁用行 19 → 17；剩余多为上游真故障（401/500/超时）与 1 个无法探活的 gpt-image-2（relay 408，按设计走「解除禁用」人工通道）。

## 遗留 / 待用户 UI 验收
- 管理后台：对禁用模型手动测试成功 → 圆点变绿（手测恢复）；红点点击 → Popover「解除禁用」（建议用 121 gpt-image-2 验证）。
- auto_ban 关闭的 11 个启用渠道不参与 relay 禁用/探活（aliyun-kimi、aliyun-qwen 系、dmx/cat-nano-banana、cat-minimax-m2.7、cat-kimi-k2.6、qwen-embedding、zeta-image），如需纳入请在渠道设置打开 auto_ban。
- 回滚：关 `auto_test_channel_enabled` → compose image 改回 `twork-v1rc20`（image `dafd644ace86`）up -d → options 按备份还原。
