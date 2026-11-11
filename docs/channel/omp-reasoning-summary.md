# OMP 历史推理摘要不回放为助手发言

## 长期约束

- 模型级 Responses 请求重建历史时，`reasoning` 项的明文摘要（`summary_text`）只对 Pi 1.0 / 1.1 回退为 `role=assistant` 文本；OMP 版本（`X-Twork-Omp-Version`）整项丢弃，和密文、恢复状态一样不回放。
- 原因：摘要改写成助手发言后，模型把它当成自己对用户说过的话。GPT 6.1 Terra 等模型在长任务里照着历史，在调工具前把「**英文小标题** + I need to…」写进正文，用户看到中文回复中间夹英文思考（Twork 飞书 #44，4.2.6）。
- 固定渠道、Chat Completions 适配不受影响。
- OMP 的 `/v1/messages`（`portableOMPAnthropicRequest`）同理：历史 `thinking` / `redacted_thinking` 整块丢弃，不再改成 `text`（2026-11-11 Twork 飞书 #76：Opus 5.5 中文对话里成段写英文过程话，结论也变英文）。只剩思考的消息整条去掉，前后同角色消息由网关合并。原先担心「Claude 工具循环要求最后一条助手消息以 thinking 开头」：旧做法改成 text 同样不满足且线上一直正常；修复前用本机代理把历史思考删掉实测 Opus 5.5 两轮 22 次请求（最后一条助手消息以 `tool_use` 开头）全部 200。
- 版本放开时不得再把 OMP 加回这条回退分支；`AGENTS.md`「Twork 运行方式隔离」同步记录。

## 回归验证

- `relay/common/twork_reasoning_summary_test.go`：OMP 版本（含与 Pi 1.0 同号、同时带 Pi 头）多段摘要、多个推理项均不出现在出站体，公开消息（含 `phase`）与工具配对保留。
- `relay/twork_reasoning_summary_outbound_test.go`：出站矩阵 `model_omp` 一组，在真实 HTTP 客户端边界确认摘要不出站；Pi 1.0 / 1.1、固定渠道、旧版行为不变。
- 旧代码下上述两条均失败。
- `relay/common/twork_omp_request_test.go`：OMP Claude 历史思考丢弃、只剩思考的消息去掉并合并相邻同角色、工具配对与顺序、非思考形状原样透传；`relay/twork_model_request_test.go` 走完整 Anthropic 适配链路确认思考不出站。旧代码下两条失败。

```sh
go test ./relay/common/ ./relay/ ./dto/ ./middleware/ -count=1
```

## 2026-10-29 发布

- 代码提交：`320abe7ca56ee2b065331697036f7b907b69f9b6`；原生产版本：`1214038fc2d07436e774a4fedb6d2b03e01543db`。服务器用干净公司 main、Go 1.26.1 构建，依赖与编译参数同原生产版本，前端资源原样复用；镜像只替换 `/new-api`。运行二进制 SHA-256：`6af01ce2741700c9b9ca90f46290758a7729da275bd5d6e76e41d4cff30ad963`。
- 10-29 02:00:16（UTC+8）由一次性定时器在零在途窗口单写切换，用时约 2.41 秒，未命中 0 次；环境变量未变，Nginx 恢复指向 3001，等待桥已停。无 DDL。
- 自动观察 02:00—02:15：33 个 OMP 请求全部成功（deepseek-flash 19、auto 14），未触发回滚。
- 11-03 真实验证：历史放带暗号的推理摘要，问模型「列出你说过的助手消息」，修复前 Terra 会把摘要英文当自己说过的话列出，修复后 3 次均不出现；真实 OMP 18.12.2 宿主用 GPT 6.1 Terra 跑两轮长报告任务，18 条助手消息、25 次工具调用、0 出错，10 段推理摘要全部未进正文。
- 发布及回滚材料：服务器 `/www/twork/releases/image-omp-summary-drop-20261028-320abe7ca/`，回滚入口 `rollback/release-when-quiet.py`（回到 1214038fc，同样等零在途，不设凌晨截止）。
- 已漏出英文的旧对话：漏出的英文已是真实正文，留在历史里仍可能被模型模仿，建议新开对话。

## 2026-11-12 发布（OMP Claude 历史思考）

- 代码提交：`9b2b26c3bbd8ab595ce09e1ea17d0c4b74f5f840`；原生产版本：`4dd1639d8`，只带这一个改动。服务器用同一 Go 镜像与编译参数构建，前端资源原样复用；镜像只替换 `/new-api`。运行二进制 SHA-256：`9a7ba26961f20ad6f761ef52dbaa4453585f81f111ffd6f6f72e2bb7782bc47e`。
- 11-12 02:00:05（UTC+8）由一次性定时器在零在途窗口单写切换，用时约 3.1 秒，未命中 0 次；环境变量未变，Nginx 指向 3001，等待桥已停。无 DDL。
- 自动观察 02:00—02:15：6 个 OMP 请求全部成功；截至 06:47 共 86 个全部成功（均为 deepseek-flash / auto，夜间无 OMP Claude 流量），无 panic，未触发回滚。
- 06:49 真实验证：Twork 开发实例经线上网关用 Opus 5.5（模型级路由，渠道 269）跑修复前能复现的中文多步看板任务，21 次请求全部 ok；20 条助手消息、21 次工具调用，结论全中文。修复前同一任务从第 6 步起成段写英文、结论整段英文。
- 仍有残留：4 段过程正文里有 2 句英文短句，都紧跟在同一次回复的英文思考之后，像是把刚想的话转述出来。这是单次回复内的语言惯性，历史里已没有思考可模仿，网关层管不到；如需再压，只能在客户端系统提示词里要求「工具调用之间的简短说明也用用户的语言」。
- 发布及回滚材料：服务器 `/www/twork/releases/omp-claude-thinking-drop-20261111-9b2b26c3b/`，回滚入口 `rollback/release-when-quiet.py`（回到 4dd1639d8，同样等零在途，不设凌晨截止），回退镜像 `new-api:omp-claude-thinking-rollback-9b2b26c3b`。
- 上线时进行中的长对话会有一次缓存未命中；已漏出英文的旧对话建议新开对话。
