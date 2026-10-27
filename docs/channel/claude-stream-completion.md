# Claude 流式完成与客户端断开

## 长期约束

- 原生 Claude 流在解析到有效 `message_stop`、请求上下文仍有效且已存在有效用量或实际输出时，先通过 `StreamResult.Done()` 记录正常完成，再向客户端写出结束帧。客户端读到结束帧后立即关闭连接，不得把已经完成的请求改记为 `client_gone/error`。
- 客户端在结束帧前取消，即使 `message_stop` 已排队等待处理，也不能提升为成功。上游错误、异常读取、无用量且无输出的空流保持原有错误处理。
- `message_delta.stop_reason` 单独出现不触发此次提前完成规则；历史 EOF 兼容逻辑保持不变。日志按实际处理结果写入，既有历史记录不回写。
- 这项修改不改变转发的 Claude 事件、用量计算、价格、选路或授权，也不需要客户端升级和数据库 DDL。

## 回归验证

`relay/channel/claude/stream_completion_test.go` 包含 8 个场景：收到结束帧后立即关连接、上游保持连接、结束前取消、仅 stop_reason 时取消、上游错误、读取异常、真实 TCP 收尾、空结束帧拒绝。除内部状态外，同时检查消费日志生成器输出的 `stream_status`；正常完成核对输入/输出用量，真实 TCP 检查上游连接被关闭。

验证命令：

```sh
go test ./relay/channel/claude ./relay/helper ./relay/common ./service -timeout=120s
go test -race ./relay/channel/claude -run TestClaudeStreamCompletion -count=1 -timeout=60s
go vet ./relay/channel/claude
python3 -m unittest scripts/test_release_tcp_hold.py
```

## 2026-10-27 发布

- 代码提交：`1214038fc2d07436e774a4fedb6d2b03e01543db`；原生产版本：`49ac92d6b4de4c5dd992ef04f60f2e59debf438c`。
- 22:16:16（UTC+8）在零在途窗口切换，单写容器替换用时约 2.43 秒；暂存桥释放 6 条连接，失败 0。环境变量未变，Nginx 原配置已恢复；未执行 DDL。
- 发布前，相同隔离验收中，正常读完和中途取消均记录为 `client_gone/error`。发布后，正常请求 `202610271416178609494258268d9d6q9WQwKT3` 记录 `done/ok`，中途取消请求 `202610271416179502478668268d9d6Ytf9mbAH` 保持 `client_gone/error`，两次正常请求的用量与扣费一致。
- 真实 OMP 18.12.2 宿主返回 `completed`，对应请求 `202610271417338281514158268d9d6JO1zAXRX` 的网关日志为 `done/ok`。
- 发布及回滚材料：服务器 `/www/twork/releases/image-claude-stream-completion-20261027-1214038fc/`。回滚入口在其中 `rollback/release-when-quiet.py`，同样等待在途、图片任务和批量结算排空，不得直接强停承载请求的实例。
- 上线后 22:16:16—22:26:16（UTC+8）的 10 分钟观察：剔除隔离验收流量后，61 个真实 OMP 请求，60 个成功、1 个客户端断开、0 个最终失败。客户端断开来自 GPT 请求，不把它解释成 Claude 回归。
- 临时渠道 268、令牌 882 及其授权关系已删除；令牌缓存清除后公网鉴权返回 401。模拟上游与等待桥均停止，临时端口 18327 / 3003 无监听。回滚镜像和只读证据保留。
- 构建使用已推送的干净公司 main、Go 1.26.1 与原前端资源。服务器构建与本地参考构建只有主模块版本元数据因历史 tag 可见范围而不同；工具链、依赖校验和、编译参数及 VCS 提交一致。运行二进制 SHA-256：`9979d6ef3ae4696fdb6684f916ae0788a2903a0d9c14c21265764d366ce1458f`。

人工验收：用 Claude 正常问答或工具调用，确认客户端正常完成且新消费记录不再红标；在另一请求生成中主动停止，确认保留客户端断开状态。历史红标不作为本次修复的验收依据。
