# 2026-10-29 GPT 6.1 Chat 参数兼容

## 来源与范围

基于公司 `main` 的 `9d9661b0f`，适配官方提交 `ad5611282` / [#8064](https://github.com/QuantumNous/new-api/pull/8064)。公司分支尚无上游新版统一 Chat 能力层，本次使用仅识别 GPT 6.1 的共享解析器，供适配器、系统角色和渠道探测复用，不引入其他代际的参数策略变更。

处理 OpenAI / Azure 的 Chat Completions 转换路径。以渠道映射后的模型为准：公开旧 Sol 别名映射到 Terra 时执行 Terra 规则；公开 GPT 6.1 名称映射到 Qwen 时保持 Qwen 原行为。未扩展 OpenRouter 专用 reasoning 适配，也不根据未知 Azure 部署别名或供应商别名猜测真实模型。

## 请求行为

| 模型 / 有效推理档位 | 采样 / 概率参数 |
| --- | --- |
| `gpt-6.1-sol`，含显式 `none` | 移除 `temperature`、`top_p`、`logprobs`、`top_logprobs` |
| `gpt-6.1-terra`，未指定或开启推理 | 移除上述四项；未指定档位时不额外注入 `medium`，交由上游应用默认值 |
| `gpt-6.1-terra`，显式 `none` | 保留调用方原值，包括 `0` / `false`，不额外添加未提供的参数 |

- 合法 `YYYY-MM-DD` 形式的快照采用对应模型规则；日期只是名称识别规则，不代表供应商一定提供该快照。
- 已确认模型及其日期快照可识别 `-none/-minimal/-low/-medium/-high/-xhigh/-max` 推理后缀。发送到上游时去掉后缀并写入 `reasoning_effort`；后缀优先于显式字段，沿用既有后缀行为。不额外校验供应商实际允许的推理档位。
- `max_tokens` 迁移为 `max_completion_tokens`；后者已显式提供时优先保留，包括 `0`，并移除前者。两者均未提供时不注入默认上限。
- 首条 `system` 消息转换为 `developer`，渠道系统提示插入也识别相同角色。
- 渠道探测的自动 / 显式 Chat 请求使用 `max_completion_tokens=16`。显式 Responses 仍构建 Responses 请求，Anthropic 探测沿用原 token 字段。
- 未知命名变体、无效日期、未映射供应商别名、未来 GPT 版本不自动套用。GPT 5、o 系列及其他模型保持本次变更前的逻辑。
- 请求体透传模式继续跳过参数转换；管理员参数覆盖仍在兼容处理之后应用，不改变现有配置优先级。

## 验收与 Test Plan

先加入适配器与渠道探测测试，在旧代码上复现采样字段未移除、旧 token 字段与角色未适配、推理后缀未处理等失败，再加入实现确认通过。

- `relay/channel/openai/gpt61_compat_test.go`：序列化后检查 OpenAI / Azure 的实际参数；覆盖 Sol/Terra 默认与 none/high、后缀优先级、max、日期快照、未知名称/无效日期、映射后非 GPT 模型、旧模型控制、显式零值、冲突与缺省 token 上限。
- `controller/gpt61_probe_test.go`：自动/显式 Chat、流式/非流式探测与 OpenAI/Azure、Responses/Anthropic 协议边界。
- `dto/openai_request_zero_value_test.go`：系统角色与已知模型、后缀、日期和未知名称边界。
- `controller/gpt61_http_test.go`：临时 SQLite + 本地 TCP 网关/上游，经 `Distribute → ModelMappedHelper → Adaptor` 读取出站 JSON 并核对真实回执。覆盖旧别名映射、渠道系统提示、Terra 后缀、映射到 Qwen 和透传控制；每种均测试流式与非流式。测试身份由中间件注入，不替代生产登录验收。

Go 1.26.10 下，以下检查通过：

```sh
GOTOOLCHAIN=go1.26.10 go test ./dto ./relay ./relay/channel/openai ./relay/channel/claude ./service/relayconvert/... ./controller -count=1 -timeout=5m

GOTOOLCHAIN=go1.26.10 go test -race ./dto ./relay/channel/openai ./controller -run 'TestGPT61|TestGeneralOpenAIRequestGetSystemRoleName' -count=1 -timeout=3m

GOTOOLCHAIN=go1.26.10 go vet ./dto ./relay ./relay/channel/openai ./relay/channel/claude ./service/relayconvert/... ./controller

git diff --check
```

未执行全仓库测试、真实模型 / 付费上游请求、Docker 构建或桌面 UI 验收。既有缓存计费、协议转换、固定渠道和故障切换回归包含在上述受影响包测试中。

## Database / DDL

无表、字段、迁移或生产数据修改，无需 DDL。

## Production Impact / Release Notes

本次仅合入公司源码，**未发布、未重启、未改生产渠道、价格、授权或分发配置**。

后续发布后，符合范围的 GPT 6.1 Chat 请求按有效推理档位清理不支持的参数、使用新版 token 字段和系统角色，可减少参数不兼容造成的上游 400；不代表已经完成供应商真实兼容性验收。原生 Responses 参数、定价、请求重试和透传策略不在本次调整范围。

## 后续发布人工验收提示

本轮无人工生产操作要求。获得后续发布授权后，在候选实例使用实际配置的 OpenAI/Azure GPT 6.1 渠道验证 Sol、Terra 默认推理与显式 none，完成流式文本、工具调用和结果回传；核对实际映射与失败日志，再按发布流程决定是否切流。供应商未知部署别名及管理员覆盖需按该渠道实际配置单独核验。
