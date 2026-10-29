# 2026-10-29 Claude 协议转换完整性补丁

## 来源与去重

基于公司 `main` 的 `5b1bb823e`，适配三项官方修复：

| 上游提交 | 本次补齐 |
| --- | --- |
| [783735607 / #7921](https://github.com/QuantumNous/new-api/commit/783735607d863bc182a00b939e498cc22e92f2f2) | 相邻助手字符串消息合并时，按原顺序拼接两侧工具调用；只去除代码为无内容工具消息注入的占位符，保留用户真正提供的 `...`。 |
| [5ac756284](https://github.com/QuantumNous/new-api/commit/5ac75628409de7e3adde7d36771700714af50853) | Chat → Claude 流式首包和后续包同时包含推理与正文时，分别发出 thinking/text delta，并按块生命周期关闭旧块再打开新块。 |
| [bc795cc11 / #7935](https://github.com/QuantumNous/new-api/commit/bc795cc1185a0ecef0ca51f73d337667026ec62f) | stop 接受字符串、`[]string` 和 JSON 字符串数组；混合数组只保留字符串，其他类型忽略，避免类型断言 panic。 |

公司这三处仍缺少对应行为，已先以新增测试复现。仅修改现有 `service/relayconvert` 转换实现，不整体迁入上游 relaykit。已有工具参数大整数/小数精度、缓存计费用量、GPT 6.1 参数、DONE/Responses/Claude 收尾、断连取消、故障切换和会话粘性修复均保留，不重复合并。

## 公司路径适配

- 首个流式分片在公司实现中有独立转换分支，上游后续包修复之外也需要补齐首包的正文发出。
- 额外测试复现：后续混合分片同时携带 `finish_reason`，用量在下一独立包才到时，原有提前 return 丢弃整包内容。本次仅推迟终态，先输出该包推理和正文；下一包到达用量后正常收尾，保留公司延迟用量的原有契约。
- 不修改原始请求结构、鉴权、渠道选择、价格、结算算法或持久化结构。工具调用 ID、名称、参数及对应 tool_result 在实际序列化后核对。

## Test Plan 与结果

先运行 `TestClaudeConversion*` 在旧实现复现：`[]string` stop 丢失、混合数组 panic、前序 tool_use 丢失、同包正文丢失；再加入实现确认通过。随后加入延迟用量边界测试，复现混合结束包内容丢失并补齐。

新增回归：

- `service/relayconvert/internal/oai_chat/claude_conversion_regression_test.go`：stop 缺省、字符串、typed/JSON 数组、混合/非字符串/空数组、数字、对象、空字符串；工具历史双调用、空内容占位、真实省略号、两侧空字符串、中间插入纯文本；混合推理/正文位于首包、角色包后、已有推理/正文后，reasoning 别名、同包结束与延迟用量。核对内容块类型和开闭、正文/推理完整性、终态一次性、用量 7/3 及 stop_reason。
- `controller/claude_conversion_http_test.go`：临时 SQLite + 本地 TCP 上游和网关，经真实 `Distribute → Relay → Adaptor`。Chat 入口核对上游 Claude 的两个 tool_use、两个 tool_result 和过滤后的 stop；Claude 入口核对首包/后续包混合推理正文及末包混合内容后独立用量，检查 SSE 顺序、7/3 用量和单个 message_stop。测试注入身份、使用免费测试模型，不替代生产登录或扣费验收。

Go 1.26.10 下通过：

```sh
GOTOOLCHAIN=go1.26.10 go test ./service/relayconvert/... ./relay/channel/claude ./relay/channel/openai ./relay ./controller -count=1 -timeout=3m

GOTOOLCHAIN=go1.26.10 go test -race ./service/relayconvert/... ./controller -run '^TestClaudeConversion|TestResponseOpenAI2ClaudeToolUseInputIsObject|TestStreamResponseOpenAI2ClaudeClosesTextThinkingAndToolBlocks|TestUpstreamSafetyLocalHTTP|TestCacheBilling' -count=1 -timeout=3m

# 最后新增的延迟用量 HTTP 场景单独复核
GOTOOLCHAIN=go1.26.10 go test -race ./controller -run '^TestClaudeConversionLocalHTTP$' -count=1 -timeout=30s

GOTOOLCHAIN=go1.26.10 go vet ./service/relayconvert/... ./relay/channel/claude ./relay/channel/openai ./relay ./controller

git diff --check
```

受影响包完整回归包含既有工具精度、缓存用量、GPT 6.1、断连、冷却恢复和固定渠道隔离用例。新增延迟用量 HTTP fixture 的首次编辑误将正常输出放入 roleFirst 条件，导致无角色首包场景失败；修正测试服务分支后，包含全部四个本地 TCP 场景的 race 回归通过。

没有运行全仓库测试、真实付费供应商请求、生产扣费或 Docker 构建。没有改动上轮已通过验证的共享 SSE 超时/扫描封装，因此未重复全供应商适配器测试。

## Database / DDL

无 schema、迁移或生产数据修改，不需要 DDL。

## Production Impact / Release Notes

仅合入公司源码，**先不发布**；未构建镜像、部署、重启或改变生产配置。

后续发版后，Chat → Claude 工具历史合并不再丢前序调用；推理与正文共用流式分片时正文保留；异常 stop 数组不再触发转换 panic。延迟用量仍在完整用量到达后收尾。

## 后续人工验收提示

本轮无人工生产操作。后续获得发布授权后，在候选实例使用真实 Chat/Claude 渠道完成两次工具调用及结果回传，验证推理尾段与正文首段同包时答案完整，并核对消费用量、正常结束和中途取消行为，再按正式发布流程决定切流。
