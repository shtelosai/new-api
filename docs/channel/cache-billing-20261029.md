# 2026-10-29 缓存计费补丁与扣费核对

## 来源与范围

基于公司 `main` 的 `ca5725c1e` 适配官方三项变更：

- `94cc3ff09`：Claude 未拆分 / 部分拆分的缓存写入总量不再漏计。
- `e9d4d6353` / [#7965](https://github.com/QuantumNous/new-api/pull/7965)：OpenAI 12 小时缓存写入独立计价，表达式增加 `cc12h`。
- `9823ebc7e` / [#7909](https://github.com/QuantumNous/new-api/pull/7909)：识别兼容上游的 `cache_creation_input_tokens`，与其他别名取最大值而非相加。

保留公司已有分组/渠道倍率、预扣与差额结算、附加费、溢出保护、原始计费来源快照、故障切换和客户端取消行为。没有整包升级上游，也没有自动替换模型默认价格或生产表达式。

## 实际链路

1. Chat / Responses 用量保留写入别名与 `cache_write_tokens_details`；12h 数量限定在总写入范围内。
2. 原生 Responses 的非流式、流式（含失败终态）、compact，以及 Chat / Responses / Claude 转换保留计费用量。TTL 指针深拷贝；后续稀疏 / 零值分段不抹掉先前缓存，累计正值覆盖旧值，不相加。
3. 传统倍率的 12h 写入使用通用写入倍率的 2 倍。表达式支持独立 `cc12h`；未单列长 TTL 时的折算规则见 `pkg/billingexpr/expr.md`。`len` 保留实际 token 数，不能使用折算值判断档位。
4. 只有缓存、没有文本输入/输出的 Claude 用量仍结算；真正无用量时返还预扣。
5. 消费日志保留写入总量、12h 数量与倍率；表达式另保存实际 `billing_tokens`，供逐项复算。后台默认主题接入 12h 价格编辑、估算、价格摘要和日志明细；短 TTL / 未分类余量与 12h 子集分开显示，不重复相加。
6. `prompt_cache_options` 在 Chat / Responses 请求转换中保留。计费依据实际响应，不依据请求 TTL 推测写入。

## 隔离库扣费核对

`service/cache_billing_accounting_test.go` 使用 service TestMain 的内存 SQLite，实际执行 `NewBillingSession → PostTextConsumeQuota → Settle`。每例分别验证传统倍率和表达式，检查钱包、令牌余额及 used_quota、用户/渠道 used_quota、单条消费日志、重复结算幂等。

所有案例预扣 100 quota，初始钱包及令牌各 1,000,000 quota；测试分组倍率 0.8、渠道倍率 1.5。下表为固定测试费率的独立手算与数据库回读核对，**不代表生产模型价格或真实供应商账单**。

| 案例 | 倍率前手算 quota | 实际最终 quota（倍率 / 表达式一致） |
| --- | ---: | ---: |
| 兼容缓存写入别名 | 525 | 630 |
| 三种别名重复报告，取最大值 | 525 | 630 |
| 未报告写入，仍按普通输入结算 | 450 | 540 |
| 30m 3000 + 12h 4000，写入总量 7000 | 16150 | 19380 |
| Claude 总写入 10000、未拆分 | 12705 | 15246 |
| Claude 总量 10000、5m 2000、1h 3000、余量 5000 | 14955 | 17946 |
| Claude 仅缓存写入 10000，无文本输入输出 | 12500 | 15000 |
| OpenAI 写入总量 7000、无 TTL 拆分 | 11150 | 13380 |
| 所有用量均缺失 | 0 | 0（预扣全额退还） |

例如混合 TTL 案例：输入总量 12000、读取 4000、写入 7000、输出 200；测试倍率为输出 5、读取 0.1、短写入 1.25、12h 写入 2.5。独立手算 `(12000-4000-7000 + 4000×0.1 + 3000×1.25 + 4000×2.5 + 200×5)×0.8×1.5 = 19380`。表达式使用 `p*2+c*10+cr*0.2+cc*2.5+cc12h*5`，按 `QuotaPerUnit=500000` 换算后相同。

## Test Plan 与结果

使用 Go 1.26.10。以下受影响包回归、定向 race、本地 TCP 协议验收、vet 已通过：

```sh
GOTOOLCHAIN=go1.26.10 go test ./dto ./pkg/billingexpr ./types ./service ./relay/helper ./relay/channel/openai ./relay/channel/claude ./service/relayconvert/... ./controller -count=1 -timeout=5m

GOTOOLCHAIN=go1.26.10 go test -race ./dto ./pkg/billingexpr ./types ./service ./relay/helper ./relay/channel/openai ./service/relayconvert/... ./controller -run 'Cache|Tiered|Responses.*(Usage|Billing|FailedStream)|ModelPriceHelper|ClientCancel' -count=1 -timeout=5m

GOTOOLCHAIN=go1.26.10 go test -race ./service/relayconvert/... ./relay/channel/openai -run 'CacheBilling' -count=1 -timeout=1m

GOTOOLCHAIN=go1.26.10 go test -race ./service -run '^TestCacheBillingAccounting$' -count=1 -timeout=1m

GOTOOLCHAIN=go1.26.10 go test -race ./relay/helper -run '^TestModelPriceHelperSets12hCacheWriteRatio$' -count=1 -timeout=1m

GOTOOLCHAIN=go1.26.10 go vet ./dto ./pkg/billingexpr ./types ./service ./relay/helper ./relay/channel/openai ./relay/channel/claude ./service/relayconvert/... ./controller
```

前端默认主题的 `bun run typecheck`、定向 `bun test ./src/features/pricing/lib/cache-billing.test.ts ./src/features/usage-logs/lib/usage-log-schema.test.ts` 通过。可视配置 round-trip 保留 `cc12h`，本地估算与后端混合 TTL 手算一致。

对涉及的前端 TS/TSX 文件执行 oxlint，仍有基线已有的 **19 个 error、2 个 warning**。将 `ca5725c1e` 的同组文件导出到临时目录，使用同一配置逐条按文件、等级、规则与消息比对，确认无新增诊断；未扩大范围修复已有 UI lint 问题。`git diff --check` 通过。

没有执行整个仓库测试、Docker 构建、真实模型 / 付费供应商请求、生产余额扣款或桌面 UI 验收；本地 TCP 上游为确定性测试服务。

## Database / DDL

无表、列或迁移变更，无需 DDL。新计费细节写入既有消费日志 JSON。

## Production Impact / Release Notes

本轮只合入公司源码，**未发布、未重启、未改生产价格/分发配置，未重算历史账单**。经典主题不在本次 12h 界面适配与前端验收范围内；后端结算规则不随主题变化。

未来发布后，原先漏计的兼容写入、Claude 未拆分写入及仅缓存请求会正常计费，12h 写入改用独立价格。没有显式列出缓存变量的旧表达式会按文档回并缓存；缺省长 TTL 变量会使用固定折算倍率。这会改变相关请求的实扣，不能以“表达式字符串没变”推断金额不变。

## 后续发布人工验收提示

本轮不要求人工生产操作。后续获得发布授权后，先冻结候选代码与生产价格快照，按实际供应商用量验证各协议的普通、缓存命中、短/长 TTL 写入及工具往返；逐笔核对钱包、令牌、日志与供应商用量，并确认失败流、取消和预扣返还边界，再决定是否切流。回滚仅针对候选代码，不覆盖生产价格或回写历史账单。
