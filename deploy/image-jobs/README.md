# GPT Image 异步任务网关

新路径 `/v1/image-tasks` 独立于所有旧同步 API。保留旧渠道、全局 ModelPrice、旧路由与价格；参与新账务的额度主体通过兼容桥改为直接读写数据库。新功能默认关闭，价格固定为 1K ¥0.30（150000 quota）、2K ¥0.40（200000 quota）、4K ¥0.80（400000 quota），每任务一张图片。

## 启用前提

1. 在主库手工执行 `main.<mysql|postgres|sqlite>.sql`，在实际 LOG_DB 执行对应 `log.*.sql`。服务不会自动创建或修改这些表。MySQL 表必须使用 InnoDB；ClickHouse 日志暂不支持。
2. 保持既有 Redis/batch 全局配置。使用 Redis 或 batch 时，必须先实证只有一个网关额度 writer，并设置 `IMAGE_JOBS_SINGLE_WRITER=true`；禁止多网关或新旧版本同时写相同 token/user。新用户第一次创建图片任务时，在进程屏障内将该 token 和付款 user 的旧 pending 增量事务移交给 DB，再持久化永久 guard。随后这些主体的旧额度读写直接走 DB，其他主体继续沿用原缓存/批量链路。关闭新建开关不能删除 guard；已有 guard 但缺少 single-writer 确认时服务启动会失败，避免启动未受保护的旧计费路径。该部署条件不是自动获得的分布式锁。已知旧 batch 写库失败会持久化 `tokerr/usrerr` guard 及 `accounting-issues/<kind>-<id>` 文件并拒绝新图片预扣；运维须先核对原始消费和 DB 实际余额，修复后同时清理对应故障 guard 与文件，再重启。不能凭报错猜测重放旧增量；若两份标记都无法持久化，必须停新建并保留进程调查，不能盲目重启。
3. 全部处理图片任务的网关实例使用同一持久卷（数据库保存 lease，文件系统保存请求和结果），挂载路径相同；磁盘及数据库应一起备份。目录只对服务账号开放，文件含用户提示词和参考图。
4. 与 tbackend 同时部署支持 `image_jobs` 权威金额的配额同步：旧 logs 排除 `request_id LIKE 'imagejob_%'`；计入成功 `charged_quota` 并扣除未结算 `reserved_quota`。每次重算前先调用同 token 鉴权的 `POST /v1/image-tasks/accounting/prepare`，确认 `{"prepared":true}` 后才读日志/加减额度，避免旧已记录消费但尚未 batch 落库的增量被重复扣除。该准备接口暂停新建时仍可调用。锁序固定 token → job → user 钱包。新旧服务版本混跑会抹掉预扣，必须先完成兼容部署再启用。
5. 适配器先启用独立持久任务端点，再新增四条专用渠道；不要把新模型挂入旧渠道。

```text
IMAGE_JOBS_SINGLE_WRITER=true
TWORK_IMAGE_TASKS_ENABLED=true
TWORK_IMAGE_TASKS_ACCOUNTING_ENABLED=true
TWORK_IMAGE_TASKS_DATA_DIR=/var/lib/new-api/image-jobs
```

`TWORK_IMAGE_TASKS_ENABLED` 只控制新创建与 capabilities 的 async；暂停新任务时仍须保持 `TWORK_IMAGE_TASKS_ACCOUNTING_ENABLED=true`，让旧任务继续查询、领取、结算。相同请求 ID 的重放仍返回已有任务，不生成新图。tbackend 仅关闭本层创建开关时，转发 POST 须携带 `X-Twork-Image-Replay-Only: true`，防止网关创建开关仍开时误建任务；该头仅能缩小权限。

仅配置 feature flag 不代表可以上线。服务会检查持久目录、任务、日志凭证、额度屏障表及金额部署前提；不符合时 capabilities 返回 `async:false`，新任务路径返回 503。

部署顺序为手工 DDL → adapter → 网关账务开、创建关 → 全部 tbackend 实例账务开、创建关 → 验收 → 开启创建。数据库保留有符号净额度，展示快照才截断为零。已有 guard 后只能关闭创建回滚，不可直接降级到不认识 guard 的旧网关或旧额度同步代码。

## 专用渠道

| 提供方 | family | 公开模型 | 优先级 |
|---|---|---|---|
| Kie | flare | twork-image-flare-async | 200 |
| APIMart | flare | twork-image-flare-async | 100 |
| Kie | sunburst | twork-image-sunburst-async | 200 |
| APIMart | sunburst | twork-image-sunburst-async | 100 |

每条渠道 `setting` 必须包含 `twork_runtime:"image_async"`、`twork_image_provider:"kie"|"apimart"`、`twork_image_family:"flare"|"sunburst"`，不得配置聊天协议字段。`base_url` 指向内部适配器对应 route（如 `http://image-adapter:8090/kie-flare`），key 是内部适配器凭据，供应商密钥只进入适配器私有配置。tag 建议 `image-async`。还须配置相应 enabled ability/group、精确 `token_model_channels` 授权及 token model whitelist；共享管理员 user_id 不提供额外权限。

`configure_channels.py` 默认只读预览，使用私有环境变量 `IMAGE_CHANNEL_DATABASE_URL`（MySQL 连接串）和 `IMAGE_ADAPTER_INTERNAL_TOKEN`（适配器内部 key）。通过既有 tbackend 运维 Python 环境执行，不安装新的生产依赖；加 `--apply` 才以事务创建四条渠道及 abilities，同名异配拒绝覆盖。先审核预览，再执行同一命令加 `--apply`，整个过程不输出密钥。

```sh
python deploy/image-jobs/configure_channels.py --kie-base-url http://image-adapter:8090/kie-async --apimart-base-url http://image-adapter:8090/apimart-async
```

脚本不直接插入用户授权。须把返回的四条渠道加入**既有生图分发规则**，保留规则原用户及组织范围，再使用 tbackend 正式同步入口；后续新用户和撤销也沿同一规则。不要仅手工补 token 行，否则正式权限同步会覆盖临时授权。未获生图规则授权的账号、共享管理员 owner 均不因此获得新能力。回滚关闭创建后保留渠道及内部 key，保证在途任务仍可查询。

公共输入：单张参考图解码最多 20 MiB，蒙版 4 MiB，所有 Data URI 含前缀编码总量 60 MiB，HTTP JSON 64 MiB；结果最多 64 MiB、40,000,000 像素。产品当前只接受共同契约的 13 种比例，供应商额外比例不自动进入产品能力。

Kie 缺少指定输出格式、压缩、quality、蒙版以及部分比例参数，网关会在提交前过滤，选择具备相应能力的 APIMart 同档渠道。Kie 支持透明背景。1K 固定 Flare，2K/4K 固定 Sunburst，禁止降档。

## 生命周期与账务

- 创建先持久化不可覆盖请求文件，再在主库事务内预扣 token 和付款钱包并写入 job；同 token + client_request_id + 规范请求只返回同一 ID，变更需求返回 409。
- worker 在主库 CAS 获取持久租约。进程中断后查原 adapter 的同一 job_id；只有 adapter 明确未受理且 retryable 才能进入备用渠道。
- 供应商无任务 ID 且明确以 401/402/403/404/429 拒绝时可选同档备用；5xx、超时或响应丢失均保留不确定状态。有有效上游任务 ID 时始终查询该任务，不能因同时带错误码重新生成。
- 图片完整解码、格式/透明度/实际尺寸核验通过，原子写结果文件后进入 settling；只在一次主库事务中将 reserved 转为 charged，增加成功消费统计。失败退回一次，unknown 保留 reservation 等待人工核实，不能自动再次扣费或生成。
- APIMart 使用官方比例/档位尺寸表精确核验（4K 方图为 2880×2880）。Kie 未公布固定像素表，按保守像素下限及 1% 比例容差拒绝明显降档，并始终报告实际尺寸，不保证固定像素；1K 已测 1254×1254 文生图和 1024×1024 编辑，2K/4K 方图已测 2048×2048 / 2880×2880。Kie 其他比例仍待完整真实矩阵验收。
- 日志通过主库 outbox 投影；LOG_DB 中 receipt 唯一键与 consume log 同事务提交，分库故障或重启不会重复记录。`image_jobs` 始终是新图片金额事实，日志延迟不改变余额。
- 任务归属严格 token_id。余额耗尽可查询/领取本人结果，disabled/expired 或已删除 token 不可访问；成功结果可领取七天。幂等任务墓碑长期保留，不复用过期 ID。

## 本地验证

```sh
go test ./model ./service ./router ./middleware ./controller -run '^TestImageJob' -count=1
go test ./model -run 'TworkRuntime|TworkChannel|LegacyRuntime|ImageJob' -count=1
```

验收必须补生产相同 MySQL/PostgreSQL 的并发事务、真实提供方、旧用户同步回归以及缓存兼容后，才可声明可上线。当前本地 SQLite/httptest 不替代真实计费和生产验收。
