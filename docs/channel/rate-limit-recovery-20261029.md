# 2026-10-29 Redis 限流恢复与分组计数隔离

## 来源与去重

基于公司 `main` 的 `b74f27b4b`，对照两项官方修复：

- [e2a0c4b16 / #6486](https://github.com/QuantumNous/new-api/commit/e2a0c4b16aa6c2e365f0845cf073c52b933385a2)：Redis 限流 Lua 脚本在 `SCRIPT FLUSH` 或 Redis 重启后丢失，遇到 NOSCRIPT 时以相同 key 和参数执行一次 EVAL，恢复脚本缓存。
- [45fbb149f](https://github.com/QuantumNous/new-api/commit/45fbb149f0c89ff48d805335f22419be392d0350)：显式配置独立限流的分组，计数主体从用户改为用户+分组；未配置的分组继续共用该用户的全局计数。

核对确认公司两个对应实现尚无等效修复。先加入测试在旧实现复现 NOSCRIPT 不恢复，以及 Redis/内存总请求数和成功请求数四种组合的跨分组误限流，再补齐实现。保留前几批协议转换、缓存计费、流式结束和会话粘性修复，不重复合入。未迁入上游新版流结果指标或成功分类体系。

## 行为与边界

- EVAL 回退仅处理 NOSCRIPT；普通 Redis 错误、取消错误保留原错误链，不触发该回退。EVAL 自身失败时向上传递，不循环尝试。
- 恢复脚本不重置令牌桶，不额外放行请求；恢复后重新使用 EVALSHA。
- 独立配置分组的总请求和成功请求计数均隔离，Redis 与内存采用一致的主体构造规则。
- token 分组优先；没有 token 分组时沿用用户分组。不同用户仍隔离；未配置独立限额的多个分组共用全局额度，不能通过切换分组放大额度。
- 保留原有 Redis 令牌桶、内存窗口、成功请求判断、错误响应及限流开关语义；不改变限额数值或生产设置。

## Test Plan 与结果

新增 `common/limiter/limiter_test.go`：

- 先消费部分桶额度，再清脚本缓存；核对 EVALSHA → EVAL 回退及恢复后的正常 EVALSHA。
- 额度消耗完仍拒绝，证明恢复没有重置桶。
- WRONGTYPE、取消请求不回退；EVAL 失败保留原始错误且没有额外回退循环。

新增 `middleware/model_rate_limit_test.go`：

- Redis 与内存分别覆盖总请求限额、成功请求限额。
- 同一用户 a 组上限 3、b 组上限 1，交错访问验证低额度分组不缩小高额度分组余额；超过各自额度返回 429。
- 不同用户隔离；未配置 c/d 共用全局额度；无 token 分组时使用用户分组；关闭限流后恢复放行。
- 测试进程共用一个独立 miniredis/client，适配现有 limiter 缓存首个 client 的生命周期；用户 ID 按场景递增，支持 `-count=2`，不重置生产单例实现。

仅增加测试依赖 miniredis v2.38.0 及其 gopher-lua 依赖，与上游版本一致；没有更新既有依赖版本，也不增加生产限流服务或运行时配置。

Go 1.26.10 下通过：

```sh
GOTOOLCHAIN=go1.26.10 go test ./common/limiter ./middleware -run 'TestRedisLimiter|TestModelRateLimitConfiguredGroupIsolation' -count=2 -timeout=1m

GOTOOLCHAIN=go1.26.10 go test -race ./common/limiter ./middleware -count=2 -timeout=2m

GOTOOLCHAIN=go1.26.10 go vet ./common/limiter ./middleware

git diff --check
```

第二条命令覆盖两个受影响包的全部测试，不只新用例；竞态检测、静态检查均通过。未运行全仓库测试，前几批未受影响的有效证据不重复跑。

另做真实 Redis + HTTP smoke：使用本机已有 `redis:7-bookworm` 镜像启动临时 Redis 7.4.11，关闭持久化，仅绑定随机本地回环端口。临时 Go harness 经本地 TCP HTTP 网关调用实际 `ModelRequestRateLimit`，分别验证 Redis/内存 × 总数/成功数四种组合；每组 8 次明确请求，核对 3+1 独立限额、未配置分组共享、429 不进入实际处理器。Redis 总数场景在第一请求后执行 SCRIPT FLUSH，后续请求正常通过，已用额度不恢复。harness 使用 `go run -race`，全部通过。测试完成后已停止并自动删除容器；未连接生产 Redis、修改其他容器或清理共享实例数据。

本次没有付费上游请求、生产 Redis 重启/故障切换、生产扣费或网关镜像构建。清脚本测试验证 NOSCRIPT 恢复，不等同于完整生产 Redis 故障切换演练。

## Database / DDL

无数据库表、字段、迁移或生产数据修改，无需 DDL。

## Production Impact / Release Notes

本轮仅合入公司源码，**先不发布**；未构建网关镜像、部署、重启或调整生产限流配置。

后续发布时，显式独立配置的分组使用新计数键开始计数；未配置分组沿用用户全局计数键。无需迁移或清空 Redis，需注意切换时独立分组计数从新窗口开始。内存路径仍遵守现有进程内计数范围，不增加跨实例共享能力。

待发版说明：修复 Redis 脚本缓存丢失后的限流失败，以及同用户不同独立限额分组互相挤占计数的问题。

## 后续人工验收提示

本轮无生产操作。后续获得发布授权后，在候选环境分别开启 Redis 和内存限流，配置不同额度的两个分组及两个无独立额度的分组；核对各自 429 边界、不同用户隔离和全局共享，再在专用测试 Redis 清除脚本缓存验证恢复。不要为验收清空生产 Redis 的数据或脚本缓存。
