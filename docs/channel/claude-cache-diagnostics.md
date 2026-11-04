# Claude 入出站缓存诊断

默认关闭。指定 `CLAUDE_DIAGNOSTIC_DIR`、RFC3339 `CLAUDE_DIAGNOSTIC_UNTIL`，且目录内存在至少 32 字节的 `hmac.key` 后开启；过期自动拒绝新采集，不影响已在途请求完成。无 DDL、计费/选路/缓存策略更改。生产需走公司 main 发布验证。

分发器读取原始 JSON 后、转换前采集入站；HTTP client 发送的实际 body 通过只读包装采集出站；响应 body 在协议转换和用量补丁之前提取原始 usage 对象。数据关联 request_id、token_name、完整 conversation_hash、attempt、channel、逐请求客户端/OMP版本及白名单协议头；upstream request IDs 同步记录。原始用户会话 ID、普通正文、工具参数、输出/思考文本和鉴权头不落盘。

`events.jsonl` 只包含 HMAC-SHA256 正文/字段/逐块/累计前缀摘要、缓存断点及 usage。wire_hash 表示字节完全相同；semantic_hash 忽略 cache_control；blocks 保留 string/array 差异、角色及消息额外字段。累积顺序 tools/system/messages。断点移动不会改变历史内容哈希，但任何正文或工具/system改变会改变后续前缀。比较同用户、模型、渠道、conversation_hash 的入站与出站相邻请求；不能以总 token 相近代替前缀一致。

完整性边界：单 body 32MiB，最多16个同时观察的请求/入站摘要，队列32条，累计4GiB。超限、队列满、文件错误计入 dropped，忙/过大有显式事件；每10秒刷新 status.json。正文读取未到EOF明确 read_to_eof=false（可能客户端关闭或HTTP发送已知长度后Close）；usage缺字段不补0；单响应最多256个usage事件。不可称为无损全量。关闭、故障不阻塞或改变业务响应；诊断摘要计算本身有CPU开销。

配置目录/root-only，文件0600。测试覆盖稳定历史追加、断点移动、system修改、版本/请求关联、请求/响应字节不变、缺失与零区别、部分读取、重复Close及正文/鉴权不落盘。上线需实际HTTP验证采集闭环和状态中的丢弃计数，关闭后保留有限期诊断摘要供版本对比。
