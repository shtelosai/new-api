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

这是同步兼容服务，不是后台持久任务队列：断连/超时后停止等待，供应商已接受的任务可能继续计费；不会重新提交，也不承诺重启后恢复或跨请求去重。日志只记录本地请求序号、路由、任务 ID、安全错误码和耗时，不记录密钥、提示词或图片。

## 构建与部署

从公司 main 已提交且推送的干净源码构建；先运行 `python3 scripts/verify-production-source.py`。已有 Go 依赖即可，不新增生产依赖或 DDL。

```sh
go test ./cmd/image-adapter -count=1
go test -race ./cmd/image-adapter -count=1
go vet ./cmd/image-adapter
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
  -ldflags "-s -w -X main.revision=$(git rev-parse HEAD)" \
  -o image-adapter ./cmd/image-adapter
```

制品安装到 `/opt/twork-image-adapter/releases/<revision>/`，`current` 指向该版本。复制本目录配置到 `/etc/twork-image-adapter/config.json`（不含凭据、0644）；由 root 建立 `/etc/twork-image-adapter/service.env`（0600），设置配置引用的两个环境变量。内部令牌使用独立随机值，New API 渠道 key 填该内部令牌，上游 key 只留在服务环境。

安装本目录 systemd unit，`daemon-reload` 后仅启动 `image-adapter.service`；不重启其他服务。只监听确认属于 Docker 网桥的内网 IP，不开公网端口。健康检查 `GET /healthz` 返回 revision、在途数和计数。更新本服务时 SIGTERM 关闭接入并等待最多 280 秒排空，systemd 停止预算 300 秒。

New API 新增类型 1 渠道：Base URL `http://172.20.0.1:8320/apimart`，模型 `gpt-image-2.5-flare`，ratio=1，无参数/请求头覆盖；不要配置 `image_provider`。先限制测试范围并显式指定渠道验收，再开放当前图片用户范围、priority=110，并在现有图片池中加入 ID。新 ID 的池配置需要 Twork 后端重新载入配置；不等同于 New API 发布。

验收：真实生图及编辑 1K/2K/4K；实际像素；两个旧图片入口；request ID 对应新增渠道；每张唯一账单；未授权拒绝；明确失败与结果未知无重复任务；原网关镜像和启动时间未变。

回滚先停用新增渠道/恢复池配置，保留旧渠道、授权快照和账目。随后按需要停独立服务或恢复其 previous 制品；不替换 New API 镜像。
