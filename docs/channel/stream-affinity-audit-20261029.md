# 2026-10-29 流式收尾、客户端断开与会话粘性上游核对

## 来源与去重

基于公司 `main` 的 `67cf83b24`，对照官方 `origin/main` 的 `e449cc700`。本轮按行为补齐，不整体合并上游新版 relaykit、指标分类或 WebSocket 架构。

| 上游修复 | 公司现状与处理 |
| --- | --- |
| [0c2af9780 / #7956](https://github.com/QuantumNous/new-api/commit/0c2af9780e45f6f9073528fb0fcbdcd64b5d9495)：Responses 完成帧送达后及时结束、透传写入/刷新错误、取消竞态收敛 | 补齐缺失行为；仅成功写入并刷新 `response.completed` / `response.done` 且 response 状态为 completed，才允许纠正并发取消。保留公司已有 guarded Responses 重试与计费边界。 |
| [7e6596728](https://github.com/QuantumNous/new-api/commit/7e6596728ff640392bd77169ebb6137a8fe0c309)：大流式事件不能受固定 30 秒写入限制 | 补齐 SSE 累计写入预算：按 8 KiB/s 排空时间加 30 秒余量；心跳和业务写入共享预算。当前公司无新版 Responses WebSocket 入口，不新增协议入口。 |
| [fcc241cdf / #8078](https://github.com/QuantumNous/new-api/commit/fcc241cdf)：粘性渠道失败后的最高优先级回退 | 已有失败渠道排除与最高剩余优先级选择，比仅偏移 retry 更完整；不重复合并，只补数据库/内存缓存和实际控制器链路回归。 |
| [55515dbae](https://github.com/QuantumNous/new-api/commit/55515dbae)：`[DONE]` 收尾 | 已在前批 `ca5725c1e` 覆盖，不重复合入。 |
| [54788117d / #7844](https://github.com/QuantumNous/new-api/commit/54788117d)：非流式取消后的指标成功判定 | 公司已明确保留保守恢复规则：非流式取消不能确认成功，embedding 有明确完整标记时例外。当前指标接口直接使用调用方的 success，不引入上游新版 outcome 架构或放宽公司保护。 |

公司已有客户端取消向上游传播、取消不污染渠道冷却、固定渠道隔离、失败渠道不重复尝试、成功后更新粘性，以及 Claude `message_stop` 写前记录完成的规则均保留。请求阶段心跳仍沿用公司既有的停止并等待交接；不迁入上游新版请求准备阶段保活架构。

## 本次补齐

- Responses 收到已完成终态并写出后主动结束扫描、关闭上游正文，无需等待上游 EOF。
- SSE 的 event、data、分隔符写入和 FlushError 向上传递；失败或协议错误不能靠结束原因名称确认断连后的渠道恢复。
- 上游已报告的 token、缓存明细与工具信息先采集再写下游；下游写入失败不丢失已收到的用量。`response.done` 别名和 incomplete 用量也纳入采集。保留原有非 guarded 路径的返回/结算策略，不将所有老协议错误重新定义为可重试错误。
- 所有扫描工作协程退出后，仅无历史错误且终态已写完并刷新的 Responses 可将收尾时 context cancellation 修正为正常完成。刷新前取消、失败写入、真正的上游读取异常都不能触发该修正；cleanup 关闭正文产生的读错不污染状态。
- 用流内累计字节预算替换每次写入固定 30 秒期限；客户端停止读取仍受有限期限约束。

## Test Plan 与证据

先用生命周期用例在旧实现复现完成帧后等待超时、下游写入错误丢失及取消误判，再加入实现验证。测试不调用真实付费供应商。

- `common/custom_event_error_test.go`：data 与分隔符写错透传。
- `relay/channel/openai/stream_lifecycle_test.go`：15 个可控生命周期场景，包括保持上游打开、完成别名、刷新时取消、刷新前取消、中途断开、cleanup EOF、三处写失败、刷新失败、前序协议错误、真实读错及失败/不完整终态；逐项核对用量与日志状态。
- 同文件真实 TCP smoke：本地上游发完终态后保持连接，网关交付完整 SSE 后及时结束并取消上游，客户端读完即关闭；核对 7 输入 / 5 输出 / 12 总 token。
- `relay/helper/stream_write_budget_test.go`：固定时钟核对累计预算、后续写入预算延续、空闲后重置，以及 3.5 MiB SSE 的实际 writer deadline。
- `service/affinity_upstream_audit_test.go`：数据库与内存缓存两条路径，低优先级粘性失败后从最高剩余优先级回退，继续失败时排除已尝试渠道，耗尽后明确报错。
- `controller/affinity_upstream_audit_test.go`：通过真实路由控制器建立低优先级绑定，模拟该渠道失败，确认先尝试绑定渠道、再回到最高优先级成功渠道，并在下一请求沿用新绑定。
- 既有 `TestResponsesStreamClientDisconnectAfterEndKeepsRules` 的 fixture 改为完整 data 事件刷新后取消，匹配原测试描述；刷新前取消由新增负例独立锁定。控制器成功判定新增历史软错误、异常结束的负例。

Go 1.26.10 下通过受影响包完整测试：`common`、`relay/helper`、`relay/channel`、`relay/channel/openai`、`relay/channel/claude`、`relay`、`controller`、`service`、`model`；完整控制器测试包含缓存计费、GPT 6.1、故障切换和取消后的恢复保护。共享 SSE 封装额外运行 `go test ./relay/channel/...` 检查其他供应商适配器。

```sh
GOTOOLCHAIN=go1.26.10 go test -race ./relay/helper ./relay/channel/openai ./relay/channel/claude ./controller ./service -run 'TestOaiResponsesStreamHandlerCompletionLifecycle|TestResponsesCompletionOverHTTP|TestStreamWriteBudget|TestSetEventStreamHeadersBoundsStreamWritesByBudget|TestAffinityAudit|TestResponsesAffinityAudit|TestResponsesStreamClientDisconnectAfterEndKeepsRules|TestRelayUpstreamFinished|Test.*Claude.*Completion' -count=1 -timeout=3m

GOTOOLCHAIN=go1.26.10 go vet ./relay/helper ./relay/channel ./relay/channel/openai ./relay/channel/claude ./relay ./controller ./service ./model

git diff --check
```

上述竞态检测和静态检查通过。`go vet ./common` 仍报告既有 CustomEvent 值拷贝 mutex 和 email 测试 IPv6 地址拼接告警；用 HEAD 原版 custom-event.go 的 Go overlay 复核得到相同告警，本次未顺带修改。首次 TCP 测试缺少测试环境 StreamingTimeout 初始化，已在 fixture 补齐；首次控制器回归暴露上述旧 fixture 的取消时机问题，修正后对应包完整回归通过。

未运行全仓库测试、真实供应商请求、生产验收或长时间慢速下载实测；大事件时长使用固定时钟与实际 deadline 检查，不以等待数分钟代替边界验证。

## Database / DDL

无表、字段、迁移或生产数据变更，无需 DDL。

## Production Impact / Release Notes

本轮只合入公司源码，**先不发布**；未构建镜像、部署、重启或调整生产渠道、粘性、价格和授权配置。

后续发布将改善 Responses 完成帧后的及时收尾、断连分类与用量完整性，并避免大 SSE 事件被固定写入超时截断。公司现有粘性失败回退行为通过回归锁定，未重复更换路由算法。

## 后续人工验收提示

本轮无人工生产操作。后续获得发布授权后，在候选实例用实际渠道验证 Responses 文本、工具结果和大图片 SSE；分别在完整结束帧后断开和输出中途取消，核对上游取消、用量、日志状态与冷却；再验证低优先级绑定失败后转到最高剩余优先级并重新绑定成功渠道，确认后按正式发布流程切流。
