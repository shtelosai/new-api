# 2026-10-29 上游安全与工具完整性补丁

## 范围与来源

从公司 `main` 的 `320abe7ca` 适配以下官方修复，保留公司现有协议转换路径、日志关联、断连取消、冷却与选路实现，不整包合并上游：

| 官方提交 / PR | 本次行为 |
| --- | --- |
| `30748c655` / [#7972](https://github.com/QuantumNous/new-api/pull/7972) | 访问日志脱敏查询凭据；OAuth 回调不记录查询参数。仅修改日志副本，处理器的 URL / RequestURI、request ID 与 route tag 保持原行为。 |
| `7d8b91268` / [#7942](https://github.com/QuantumNous/new-api/pull/7942) | Go 最低版本、三个 Docker 构建入口统一为 1.26.10；release / Electron CI 从 go.mod 读取版本。 |
| `4d962e464` / [#7996](https://github.com/QuantumNous/new-api/pull/7996) | OpenAI → Claude 非流式工具参数保留原始 JSON 数值，避免大整数和高精度小数被 float64 改写；空、坏 JSON、null、非对象仍回退为空对象。 |
| `0165fef98` / [#7998](https://github.com/QuantumNous/new-api/pull/7998) | Gemini → OpenAI 同条消息的工具调用与文字同时保留。 |
| `55515dbae` / [#8074](https://github.com/QuantumNous/new-api/pull/8074) | 精确识别裸和 data 前缀的 `[DONE]`，兼容周边空白；`[DONE]extra` 不作为终态。 |

`Dockerfile` / `Dockerfile.shtlcloud` 固定的 `golang:1.26.10-alpine` 摘要采用本次 Docker Hub manifest API 实际回读值 `sha256:a2012b4a1d1f9f7e9f44e0d39b805a901e856b1dd80b4e6ee4338db547050308`，与上游提交当时固定的摘要不同。

## 验收与回归

本地使用 `GOTOOLCHAIN=go1.26.10`。先只加入回归测试，在修复前复现日志泄露、工具数值精度丢失、Gemini 文字丢失和 DONE 误判，再加入实现确认通过。

- `middleware/logger_test.go`：重复/编码/混合大小写的凭据键、特殊字符和坏查询编码、OAuth 回调脱敏；处理器收到的请求保持不变，日志关联字段保留。
- `service/relayconvert/internal/oai_chat/to_claude_messages_resp_test.go` 及 Claude 兼容入口：精确断言序列化 JSON 中的 `9007199254740993`、嵌套 uint64 最大值、高精度小数，以及原有非对象回退。
- `service/relayconvert/request_registry_test.go`：单段/多段文字与工具调用并存、仅文字、仅工具，经过 DTO 序列化后核对上游请求。
- `relay/helper/stream_scanner_test.go`：裸、带空白、data 前缀、无空格的 DONE；终态后不处理数据；DONE 前缀的普通 payload 不被误判。
- `controller/upstream_safety_integration_test.go`：临时 SQLite + 本地 TCP 网关 + 本地 TCP 上游，经真实 Distribute / Relay 入口验证 Claude 工具数字回执、Gemini 上游文字和工具共存、上游不发送 EOF 时裸 DONE 仍主动结束且不泄露尾部数据。测试中间件注入测试身份，不替代生产登录验收。

以下检查全部通过：

```sh
GOTOOLCHAIN=go1.26.10 go test ./middleware ./relay/common ./relay/helper ./relay/channel/claude ./relay/channel/openai ./service/relayconvert/... ./controller -count=1 -timeout=5m

GOTOOLCHAIN=go1.26.10 go test -race ./middleware ./service/relayconvert/... ./relay/helper ./relay/channel/claude ./controller -run 'TestSetUpLoggerMasksQueryCredentials|TestResponseOpenAI2ClaudeToolUseInputIsObject|TestGeminiToOpenAIChatPreservesTextAlongsideToolCalls|TestStreamScannerHandler_DoneStopsScanner|TestStreamScannerHandler_ClientCancelAbortsUpstreamAndReturns|TestUpstreamSafetyLocalHTTP|TestResponsesFailoverRebindsAndSkipsCoolingChannels|TestResponsesFailedStreamDoesNotBindOrAppendAnotherReply|TestResponsesStreamClientDisconnectAfterEndKeepsRules' -count=1 -timeout=5m

GOTOOLCHAIN=go1.26.10 go vet ./middleware ./relay/common ./relay/helper ./relay/channel/claude ./relay/channel/openai ./service/relayconvert/... ./controller

git diff --check
```

controller 回归包含现有选路、固定渠道隔离、故障切换和断连收尾测试。未运行真实 OMP SDK / 付费上游验收或完整项目测试；本次只改变构建版本配置，未执行 Docker 镜像构建或跨平台产物验证。

## Database / DDL

无 schema、迁移或生产数据变更，不需要 DDL。

## Production Impact

本次仅合入公司源码，不发布、不改生产配置、不重启服务。线上行为暂不变化；未来发布需按公司 main 的生产源校验、候选切流与回滚流程另行验收。

未纳入 GPT 6.1 参数兼容、缓存计费或其他上游功能；原开发目录的未提交改动未纳入。

## Release Notes

待后续发版时使用：修复日志查询凭据泄露、工具参数数值精度丢失、Gemini 工具历史文字丢失和流终态识别问题；Go 构建基线升级至 1.26.10。

## 后续发布验收提示

本轮无人工生产验收要求。后续获得发布授权后，在候选实例验证日志脱敏与原请求不变、工具调用/结果回传和普通聊天完整收尾，并核对消费记录、冷却恢复与固定渠道隔离，再执行生产切换。
