# 独立异步图片适配服务

此目录的服务独立编译和运行，不启动 New API、不访问其数据库，也不替换现有网关镜像。旧客户端继续同步调用图片 API：New API 普通 OpenAI 渠道 → 本服务 → 异步供应商。

## 通用与供应商边界

- `cmd/image-adapter/engine.go`：统一总预算、同任务轮询、取消、同结果下载重试、完整解码和 b64_json 交付。
- `Provider.Submit/Poll`：供应商只负责上传/参数转换、提交 ID、状态映射和结果 URL/usage。只有 Submit 可以创建任务，Poll 不得创建或重建任务。
- `apimart.go`：第一个实现。其他异步协议新增 Provider 并注册到 `configuredServer`；多个同协议渠道只需添加 provider 配置，不复制轮询逻辑。
- 每个配置项拥有独立路由名、内部鉴权令牌、上游凭据和模型白名单。路由为 `/{name}/v1/images/generations`、`/{name}/v1/images/edits`。

目前支持同步 b64_json 生图、multipart 参考图与蒙版编辑；明确拒绝流式、URL 输出和 JSON 编辑。默认 medium，APIMart 数量范围 1–4。实际交付张数由原网关结算，服务自身不扣费。

## 错误和安全

任务提交结果未知、受理后的查询/下载失败使用 HTTP 400 + `image_result_invalid`，防止旧网关把 5xx 当作再次生图的理由，也让 Twork 停止跨渠道重试。明确未提交任务的容量限制返回 429；参考图上传失败返回 502。不要通过渠道状态码映射把 `image_result_invalid` 改成可重试错误。

结果下载只接受 HTTPS，校验全部解析 IP 并连接已校验地址，禁止内网、loopback、链路本地及保留网段；不向结果地址发送上游 Authorization。供应商接口不跟随重定向。请求体/全部输出各不超过 64 MiB；单图最大边 8192、总像素 32 Mi；通过完整解码后交付。参考图每张最多 20 MiB、蒙版 4 MiB；并发默认 4，满载在创建任务前拒绝。

APIMart 自身精确尺寸上限为单边 3840、总像素 8294400。超限请求等比例缩小至 16 的倍数：3840 方图实际 2880 方图，16:9 保留 3840×2160。原网关和 Twork 继续核验并如实报告实际像素/降档。

以上两个旧接口保留同步兼容语义：断连/超时后停止等待，供应商已接受的任务可能继续计费；不会重新提交，也不承诺重启后恢复或跨请求去重。日志只记录本地请求序号、路由、任务 ID、安全错误码和耗时，不记录密钥、提示词或图片。

## 构建与部署

从公司 main 已提交且推送的干净源码构建；先运行 `python3 scripts/verify-production-source.py`。已有 Go 依赖即可，不新增生产依赖。启用新版异步时，仅在适配器专用状态目录初始化 SQLite，不访问或修改原网关数据库。

```sh
go test ./cmd/image-adapter -count=1
go test -race ./cmd/image-adapter -count=1
go vet ./cmd/image-adapter
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
  -ldflags "-s -w -X main.revision=$(git rev-parse HEAD)" \
  -o image-adapter ./cmd/image-adapter
```

制品安装到 `/opt/twork-image-adapter/releases/<revision>/`，`current` 指向该版本。复制本目录配置到 `/etc/twork-image-adapter/config.json`（不含凭据、0644）；由 root 建立 `/etc/twork-image-adapter/service.env`（0600），设置配置引用的两个环境变量。内部令牌使用独立随机值，New API 渠道 key 填该内部令牌，上游 key 只留在服务环境。

安装本目录 systemd unit，`daemon-reload` 后仅启动 `image-adapter.service`；不重启其他服务。只监听 loopback、RFC1918 或 Tailscale CGNAT 地址，不开公网端口。生产示例部署在现有新加坡 sg-ecs 节点，监听 Tailscale `100.88.103.99:8320`；上海节点直连供应商存在异常解析和 TLS 重置，不应固定供应商 IP 或修改宿主机全局 DNS。健康检查 `GET /healthz` 返回 revision、在途数和计数。更新本服务时 SIGTERM 关闭接入并等待最多 280 秒排空，systemd 停止预算 300 秒。

首次上线新版持久任务可使用 `image-adapter-async.service` 单独监听 `100.88.103.99:8321`；独立 `/etc/twork-image-adapter-async/` 配置与 `/var/lib/twork-image-adapter-async/` 状态目录。旧 8320 同步服务继续运行，新四渠道只指向 8321，无需打断旧生图请求。异步 unknown 任务不按终态保留期清理，须核实受理及费用后再处理。

New API 新增类型 1 渠道：Base URL `http://100.88.103.99:8320/apimart`，模型 `gpt-image-2.5-flare`，ratio=1，无参数/请求头覆盖；不要配置 `image_provider`。先限制测试范围并显式指定渠道验收，再开放当前图片用户范围、priority=110，并在现有图片池中加入 ID。新 ID 的池配置需要 Twork 后端重新载入配置；不等同于 New API 发布。

验收：真实生图及编辑 1K/2K/4K；实际像素；两个旧图片入口；request ID 对应新增渠道；每张唯一账单；未授权拒绝；明确失败与结果未知无重复任务；原网关镜像和启动时间未变。

回滚先停用新增渠道/恢复池配置，保留旧渠道、授权快照和账目。随后按需要停独立服务或恢复其 previous 制品；不替换 New API 镜像。

## 新版持久异步任务（默认关闭）

新增协议与上面的旧同步兼容协议分开。原 `config.example.json` 无需修改，旧 generations/edits、并发配额、超时和返回结构均保持原样。`config.async.example.json` 提供独立 Kie/APIMart 路由；只有 `async.enabled=true` 且对应供应商 `async_enabled=true` 时开放新接口。部署应在保留现有配置的基础上合并新增项，不能直接覆盖旧渠道配置。

| 方法 | 路径 | 结果 |
| --- | --- | --- |
| POST | `/{route}/v1/image-tasks` | 202，任务已提交本地持久队列 |
| GET | `/{route}/v1/image-tasks/{job_id}` | 200，状态、实际尺寸/格式、安全错误码 |
| GET | `/{route}/v1/image-tasks/{job_id}/result` | 200，验证后的单张图片二进制；未完成409，结果过期410 |

所有接口复用该路由内部 Bearer 令牌，任务按 route + job_id 隔离。POST 参数为 `job_id/model/prompt/size/resolution`，可选 `image_urls/mask_url/output_format/output_compression/background/quality`；不接收 `n` 或用户回调地址。同 ID、相同规范化参数返回同一任务，不同参数409。建议将创建失败或断线视为**同 ID 查询/重发**，绝不能换 ID 自动重新生成。

1K 固定模型 `gpt-image-2.5-flare`，2K/4K 固定 `gpt-image-2.5-sunburst`。Kie 映射为对应 `gpt-image-2-5-{flare|sunburst}-{text-to-image|image-to-image}`，走 `/api/v1/jobs/createTask`、`/api/v1/jobs/recordInfo`。上传默认遵循 OpenAPI servers 的 `https://api.kie.ai/api/file-stream-upload`；文档 curl 示例出现另一个域名；本轮真实上传已验证 `https://kieai.redpandaai.co`，尚未验证 servers 地址的上传入口，部署时可单独配置 HTTPS `upload_base_url`，不可把密钥发送到用户输入的 URL。

Kie 支持 background，但不支持指定格式、压缩、quality、蒙版；未传格式时交付上游实际格式，显式要求不支持的能力返回 `unsupported_image_options`。其正式比例 enum 与文档扩展枚举有冲突，目前按正式 enum 接受 `auto/1:1/3:2/2:3/4:3/3:4/16:9/9:16/21:9/27:16/16:27/9:8/8:9`。其他能力交由网关在**提交前**选择 APIMart，不静默删参数。

APIMart 新接口直接传比例和原生档位，不使用旧同步接口的像素缩小算法；已知比例按官方档位表验证实际尺寸（例如1:1+4K=2880×2880）。Kie 原生档位不套用 APIMart 尺寸表或封闭白名单；按比例误差≤1%以及档位下限核验（1K面积≥655360且长边≥1024、2K长边≥2048、4K面积≥4900000且长边≥2880），记录真实 `actual_size`。真实方图样本1K1024/1254、2K2048、4K2880保留在测试中。两者均不插值扩图。显式格式不匹配、要求透明却交付不透明、损坏图片或超过像素上限均不能成为成功结果。

### 持久化、恢复与费用边界

- 状态：`queued/submitting/running/downloading/succeeded/failed/unknown`。SQLite 使用 WAL + FULL 同步落盘，结果临时文件 fsync 后原子替换。systemd `StateDirectory` 提供持久目录；单机同一目录只允许一个进程持锁，异步 worker 数1–8，队列容量有界，独立于旧同步 slots。
- 服务上下文与 HTTP 请求分离，客户端退出不取消任务。进程重启后，有上游 ID 只查询原任务；`submitting` 而没有 ID 的崩溃窗口标为 `unknown`，不自动再提交。恢复 ID 无法确定时需要运营核查。
- `retryable=true` **仅代表已确定没有创建上游生图任务**：队列满、参考图上传/下载暂不可用、提交前能力不支持、上游明确401/402/403/404/429拒绝且没有任务ID等。400/422参数拒绝不可切备用；5xx/读超时为未知提交。响应出现有效任务ID时，无论HTTP/code是否报错均保存该ID并只查询；无效非空ID也按未知提交处理。`unknown`、已受理失败、下载失败一律不能跨渠道重新生成。状态查询的 `retryable` 仅针对“创建新的生成尝试”，并不禁止继续查询原 ID。
- 下载失败回到查询同一个上游 ID，可刷新临时结果 URL；永远不重新生成。任务最多跟踪24小时，到期未能确认则保留 `unknown`。结果及输入默认7天后清理；保留 ID、请求哈希和终态墓碑，过期后重发相同 ID 不生成第二次。需要备份状态目录，不能通过删库解决故障。
- 本服务不扣费，也不决定 Kie/APIMart 顺序；渠道选择、一次性结算及退款由 New API/业务后端负责。网关必须先持久化稳定 `job_id`，创建响应丢失时先查同一 ID，不能把普通 HTTP 5xx 等同于允许重生图。
- 新异步接口输出最大64 MiB、40,000,000像素（单边8192）；输入单张解码后20 MiB、蒙版4 MiB，全部 Data URL 编码后60 MiB，HTTP请求体64 MiB。以上旧同步接口限额保持原样。
- 用户图片只接受有界图片 Data URL 或公网 HTTPS，下载验证所有 DNS 结果并固定已验证 IP；先完整解码，再转存到供应商临时文件。不会向用户提供的地址发送上游认证。日志不记录图片、提示词、供应商响应正文或密钥。

### 可选 Kie 回调

默认完全依赖安全轮询，不需要配置 HMAC 或开公网监听。若已有外部代理能把回调转发到私网，可在 Kie 路由设置 `callback_url` 和 `webhook_key_env`，路由为 `/{route}/v1/image-tasks/callback`。只有两项均配置才发送 callBackUrl；缺 HMAC 时正常轮询。

签名采用 Kie 文档约定的 Base64(HMAC-SHA256(`taskId.timestamp`))，验证时间戳±300秒和 route 内的任务关联。签名不覆盖结果正文，因此回调只能提前触发查询，绝不采信 callback 中的 URL、成功状态或扣费信息；重复通知也不创建任务。不要改成本服务监听公网。
