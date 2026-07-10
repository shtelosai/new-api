# Codex 对抗审查日志 — new-api 模型健康机制改造

计划文件：`~/.claude/plans/new-api-sprightly-cook.md`（plan mode 单源；批准后同步 spec/）

## Round 1（盲出方案）

Codex（gpt-5.6-sol, xhigh, read-only, session 019f49ec-182b-7553-bcfe-6b7a42a62f2f）基于原始需求独立出方案，骨架：
- Approach：复用「确认成功即恢复」事务，把手测成功与 3 分钟被动探活统一接入；passive 改为只探 auto/relay 禁用模型、每轮一次、失败只观测不禁用；手动全测保持原样。
- 里程碑：①先补回归测试 ②手测成功恢复 ③passive 收窄（仅禁用模型/全渠道类型/单次探活）④探活超时配置对齐+后台文案 ⑤本地验证 ⑥生产部署（停巡检→部署→两步切配置→canary）。
- 关键主张：恢复键用原始模型名；仅显式 ?model= 触发恢复；HandleConfirmedProbeResult 返回 error 供手测接口判定（恢复失败不得报成功）；不新增 API 字段；passive 不再跑 performChannelTests 整渠道测试；getModelProbeTimeoutSec 改用 channel_health_setting.probe_timeout_sec（修死配置）。

### 对比合并决策表

| # | 差异点 | Codex 盲出 | 我的原计划 | 处置 | 理由 |
|---|---|---|---|---|---|
| 1 | passive 探活失败处理 | 每 pair 每轮 1 次、失败只观测不进状态机 | 沿用 probeModelWithImmediateRetries（同轮重试 3 次）+ HandleConfirmedProbeResult(false) | **吸收** | 目标已是禁用态，失败无需确认；省 2/3 探活费用；不改写 relay 原始 reason |
| 2 | 恢复键 | 原始模型名（与 relay 禁用写入一致） | testChannel 解析后的 testedModel | **吸收表述** | 两者在显式 model 场景等价，明确口径 |
| 3 | 无 model 参数的手测 | 不触发恢复（不猜对象） | 用解析出的默认测试模型恢复（需加 testedModel 字段） | **吸收** | 更外科手术：testChannel 零改动；核心场景（模型圆点点测）前端本就带 model |
| 4 | 手测恢复失败反馈 | HandleConfirmedProbeResult 返回 error，恢复失败→success:false | 返回 action + 响应加 recovered 字段 | **吸收 error；砍 recovered 字段** | 恢复失败仍报成功=本次要修的"假成功"变体；recovered 字段是投机性 API 扩展 |
| 5 | passive 是否继续测整渠道（status=3） | 移除 performChannelTests 分支 | 保留现状 | **吸收** | 余额禁用渠道每 3 分钟白测一次费用更高；恢复本依赖 AutomaticEnableChannelEnabled（生产值待核）；行为变化写入 Production Impact |
| 6 | 探活超时死配置 | 改用 probe_timeout_sec(20s)，修 5s 误用 | 探索时发现但未纳入 | **吸收** | 真实移植遗留 bug，5s 会让慢模型/SDK 探针超时误判→永不恢复，直接影响本功能质量 |
| 7 | 执行顺序 | 测试先行（M1 先补测试） | 实现后补测试 | **吸收** | 符合用户全局规则第 4 节 |
| 8 | 部署顺序 | 先关巡检→部署→两步切配置→再开→canary | 一步切三项配置 | **吸收** | 避免中间态短暂触发旧全量巡检 |
| 9 | 后台 passive 文案 + i18n 同步 | 纳入 | 未纳入 | **吸收** | 语义变了文案要跟 |
| 10 | 手测成功后 UI 反馈闭环 | 未提 | 前端确认/补列表刷新让圆点变绿 | **保留（我的）** | 用户核心诉求就是"成功=可见恢复"，缺刷新则圆点仍红 |
| 11 | AGENTS.md 契约追加 | 完成后追加 ≤15 行 | 未纳入 | **吸收** | 符合仓库治理惯例 |

无需用户仲裁的方向级分歧。合并后计划已更新为 M1~M6 结构。

## Round 2（首轮对抗复审）

Codex（session 019f49fa-d38b, xhigh, read-only）verdict=**需修订**。

Findings（保留 Codex 分档）：
- **BLOCKER 正确性 / M2**：「探活链路天然支持所有渠道类型」不成立——`runProbe` 硬编码非流式 + `getModelHealthProbeEndpointType` 恒空；Codex 类渠道 `shouldUseStreamForAutomaticChannelTest` 要求流式（渠道级测试有用、模型探针没用）；图像/TTS/ASR 模型默认按 chat 构造会持续误判。
- **SHOULD 正确性**：relay 禁用还有渠道 `auto_ban=1` 前置（nil 视为 false），部署前需审计；`c.Query("model")` 未在 handler 层 trim，带空格时恢复键错位。
- **SHOULD 遗漏**：AGENTS.md 契约步骤在合并计划中缺失（决策表#11 声称吸收但未落）；前端验证缺 lint/format:check。
- **SHOULD 风险**：DB 清除成功但 InitChannelCache 失败不可传播——需明确接口成功语义；「≤3 分钟」过度承诺（调度抖动+同渠道串行排队）；跨渠道重试非无条件兜底（指定渠道/亲和跳过重试）；「sidecar 不计费」错误（真实上游调用）；M6 缺发布物 commit/digest 确认（分支 ahead 2 + 脏文件）。
- **NIT**：scheduled_all 成本基线表述（非全渠道×全模型）；前端刷新闭环现状已满足应零改动；`(HealthAction, error)` 返回值无消费者，只需 error；勿为错误测试加生产注入框架。

本轮修订（全部吸收，无仲裁点）：
1. BLOCKER → M2 增加协议等价性界定：runProbe `isStream` 接线 `shouldUseStreamForAutomaticChannelTest(ch)`；passive 支持范围=testChannel 能自动构造有效请求的模型；非 chat 模型明示「探活可能持续失败维持禁用（不恶化），恢复走手动测试（可带 endpoint_type）」，写入 AGENTS.md 契约与后台文案。
2. M1 handler 层 `strings.TrimSpace(c.Query("model"))` 统一测试/恢复键 + trim 回归测试。
3. M1 明确接口成功语义=「DB 已清除」，缓存 10s 周期同步最终一致，写入契约。
4. M6 增加：发布物 commit/digest 确认（含 2 个未发布 UI commit）、auto_ban 审计步骤。
5. M4 落实 AGENTS.md 契约条目（M4.4）；前端刷新改为「确认现状零改动」。
6. M5 补 `bun run lint` + `format:check`。
7. HandleConfirmedProbeResult 只返回 `error`；错误传播测试用测试侧 DB 故障手段。
8. 措辞修正：恢复时延 3~4 分钟口径、跨渠道兜底限定、sidecar 非免费、巡检成本基线。

## Round 3（复审修订后计划）

Codex（read-only, xhigh）verdict=**需修订**。上轮核销：流式接线/TrimSpace/时延口径/重试限定/sidecar 成本/AGENTS.md/lint 均「解决」；协议等价性、缓存语义、发布物确认「部分解决」。

新 findings：
- **BLOCKER1 正确性/遗漏**：非 chat（TTS/ASR/视频）模型的「手动测试恢复兜底」不存在——buildTestRequest 无音频构造、EndpointType 无音频类型、前端弹窗无入口；把这类禁用模型纳入 3 分钟探活=持续错误协议请求，比现状恶化且无恢复通道。
- **BLOCKER2 风险/遗漏**：Manual Acceptance 步骤 4 在生产写 manual 禁用行且无清理步骤；主键 (channel_id,model)、自动恢复不清 manual → 可能永久留下不可路由模型。
- **SHOULD1**：InitChannelCache 并非全面 fail-closed（仅禁用表读失败保留旧缓存；channels/abilities 错误被忽略可能空缓存），计划语义失真。
- **SHOULD2**：probe 超时 5s→20s 同样作用于 scheduled_all Anthropic 探针（共享 getModelProbeTimeoutSec），「全部不变」表述不准。
- **SHOULD3**：发布物确认不可复现（COPY . . 无 SHA、本地 tag 无 RepoDigest、工作树脏）→ 需 clean checkout+SHA tag+immutable image ID 对照+按 image ID 回滚。
- 另：auto_ban 审计 SQL 需把 NULL 计入（GetAutoBan(nil)=false）。
- 过度设计维度：未发现。

本轮修订（全部吸收，无仲裁点）：
1. BLOCKER1 → 新增 M2「解除禁用」显式管理动作（新端点删任意 source 行含 manual + 重置计数 + 刷缓存 + 前端模型状态 Popover 按钮）——既是非 chat 模型的真实恢复通道，也补上全系统缺失的运维入口；探活承诺范围限定为可探活模型；非 chat 模型的失败探针代价（每轮一次小失败请求）如实写入边界与 Production Impact。
2. BLOCKER2 → Manual Acceptance 重写：专用 Canary 渠道隔离、逐步快照、逐场景清理（步骤 4 的清理动作本身用「解除禁用」按钮作为验收对象）、终态零残留核对。
3. SHOULD1 → 缓存语义按真实行为改写（预存在行为、不改热路径），接口成功语义=DB 已清除。
4. SHOULD2 → M4 如实声明影响 scheduled_all Anthropic 探针（预期修复）+ 双路径超时来源回归测试。
5. SHOULD3 → M7.1 发布物可复现流程（先提交→clean checkout 构建→SHA tag→image ID 记录与对照→按 image ID 回滚）。
6. auto_ban 审计 SQL 显式 `auto_ban IS NULL OR auto_ban != 1`。
里程碑重排为 M1 手测恢复 / M2 解除禁用 / M3 passive 收窄 / M4 超时修复 / M5 前端文档 / M6 本地验证 / M7 部署。

## Round 4（三轮复审）

Codex verdict=**需修订**。上轮核销：BLOCKER1/BLOCKER2/SHOULD1-3/auto_ban NULL 全部「已解决」。

新 findings：
- **BLOCKER 状态语义**：manual 不是可靠人工锁——`UpsertChannelModelDisabled` 无条件覆盖 source（relay 路径注释自认故意覆盖 manual）；在途异步失败与指定渠道请求（跳过模型禁用检查）可把 manual 翻成 relay，随后 passive 探活成功即清除，管理员意图被静默撤销。要求二选一：relay 写入不覆盖 manual（并发安全三库兼容+竞态回归）或删除"人工锁死"承诺。
- SHOULD1：M2 权限需钉死 `RequirePermission(authz.ChannelOperate)`（非管理员被 AdminAuth 拒是 200+success:false，"非管理员 403"测试与现状冲突）。
- SHOULD2：新 DELETE 路由只有 generic 审计且不记 query 参数，需精确审计（channel_id/model/previous_source/changed）。
- SHOULD3：删除+健康重置需单事务（复用行锁模式）+输入规范；解除后真实状态是 unknown 非绿。
- SHOULD4：前端现状是 Tooltip 内 10px span 非 Popover，需明确交互改造 + api 封装 + 防重复 + capability 门控；模型状态列 mobileHidden，移动端无入口。
- SHOULD5：seedream/embedding 启发式在 requestPath 判定与 buildTestRequest 构造两处不对称，"可靠覆盖"声明过宽。
- SHOULD6：零残留漏 channel_model_health 表与 channels.response_time/test_time；Canary 隔离需独立分组/别名/无生产 token。

本轮修订（全部吸收；manual 硬锁选「relay 不覆盖 manual」——与用户原始诉求「保留 manual 禁用」一致）：
1. M2.1 新增 manual 硬锁语义修复：relay 禁用事务内先查、manual 存在跳过覆盖仅摘缓存 + 竞态回归。
2. M2.2 端点权限 `RequirePermission(authz.ChannelOperate)`；单事务删除+健康重置；TrimSpace/校验；响应 changed/previous_source。
3. M2.3 精确审计。M2.4 前端 Tooltip→可交互 Popover 改造细节 + 移动端无入口记已知限制。
4. M3.2 可靠覆盖收窄为 chat+Anthropic 探针，启发式类型标尽力而为。
5. Manual Acceptance：快照/零残留扩展到 health 表与渠道测试字段；Canary 隔离细化；解除后状态 unknown 措辞。
6. Test Plan 补权限语义、manual 竞态、事务化用例。

## Round 5（终轮复审）

**未执行**：第 5 次 `codex exec` 因 OpenAI 用量限额失败（"You've hit your usage limit... try again at 1:16 PM"，session 019f4a1b-5d6a）。Round 4 的 1 BLOCKER + 6 SHOULD 已全部修订入计划第四版，但该版未经 Codex 复核。

## 收敛结论

- 共实际执行 4 次 Codex 调用（盲出 1 + 复审 3），第 5 次因限额未执行。
- 轨迹：Round 2 需修订（1 BLOCKER）→ Round 3 需修订（2 BLOCKER，上轮全核销）→ Round 4 需修订（1 新 BLOCKER，上轮全核销）→ Round 5 未执行。
- 每轮 findings 均全量吸收修订，无遗留分歧、无用户仲裁点；但**最终版（含 M2.1 manual 硬锁修复、M2.2 解除禁用端点细化）未经终轮核销**，按纪律标记「未收敛」，不硬宣布通过。
- 补救选项：13:16 后补跑终审一轮；或用户直接审阅 M2 章节后批准进入执行（执行阶段 sp/exec 的逐里程碑验收仍会独立把关）。
