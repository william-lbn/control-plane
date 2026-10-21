# Functions 对象事件

0.54.0 / migration 043 基础闭环已完成 Linux、真实 UI 与原生 SQL 验收，见[精确交付](DELIVERY-2026-10-18-FUNCTION-OBJECT-EVENTS.md)。性能、压力、故障注入和 HA/DR 专项按用户要求后置。

控制台 Functions 选择“对象上传”，填写桶名、可选且区分大小写的对象键前缀、已部署函数和无查询参数的相对路径。先保存暂停的触发器，再确认可能重复送达后启用。需要当前分支已启用 Object Storage 和函数公开 HTTP 策略。桶按名称订阅，可先于桶创建；同名重建的桶仍匹配。触发器类型不可修改，编辑、删除需精确版本与持久 Idempotency-Key。列表、调用记录沿用项目/分支权限，写入仅限编辑者。

REST、签名上传、S3 PUT、COPY 和 multipart 完成在对象目录发布事务中记录事件。仅修改元数据、写入未完成分片、失败或回滚的发布不产生事件；完成请求的保留回执重放不再发布。对象内容不会进入控制面事件账本。每次成功覆盖是独立事件，不承诺不同对象或不同请求间的全局顺序。

原生分支数据库保存 HMAC 签名 outbox，绑定项目、分支、timeline、密钥版本、对象键、digest/大小和发布时的目标快照。控制面先写脏分支标记，原生目录锁覆盖发布及导出。只有脏分支会按需连接 SQL；无事件的休眠 Compute 不被周期唤醒。导出先提交确定性调用 ID，再确认原生事件，最后以 generation CAS 清标记。复制到子分支的父分支 outbox 不会被采用。无效签名保留为 rejected，不派发。

调用为 POST JSON，平台签名准入后由 Guest 注入 `X-Neon-Trigger-Invocation-Id`。普通调用者同名头会被移除。示例：

```json
{"version":1,"invocation_id":"fti_<64hex>","trigger":{"type":"storage_object_created","id":"ftr_<24hex>","name":"new uploads"},"data":{"bucket_name":"photos","object_key":"raw/photo.jpg","sha256":"<64hex>","size":123,"event_id":"soe_<32hex>"}}
```

`invocation_id` 是原始逻辑事件标识，自动尝试和手动重投递保持一致。应用必须在自己的 SQL 事务中用此 ID 去重副作用。平台最多自动尝试三次，完整 HTTP 429/5xx 响应可重试；网络结果未知或响应不完整保留为 outcome_unknown，不自动推断未执行。控制面保存每次派发的独立请求账本引用，原回执保留第一次请求引用。

失败或未知回执可明确确认重复副作用后手动重投递。POST `.../functions-triggers/{trigger}/invocations/{invocation}/retry`，`If-Match` 为带双引号的所选不可变回执 ID，JSON 为 `{"acknowledge_duplicate_side_effects":true}`。相同持久键/输入返回同一个 202 回执。重投递追加新 delivery ID 并指向原回执，原失败、对象、时间、目标快照、原操作者和逻辑 ID 不改写。原操作者仍须有授权，原 Function 必须可用或可自动冷醒；原触发器的后续编辑不会重新路由已接收事件。一个原事件最多三次手动重投递，有进行中或已成功的子回执时拒绝新增。每个操作者每小时最多 100 次手动准入。

暂停或删除触发器停止捕获新发布，既有事件和回执保留。运营开关 `functions.objectEvents.enabled` 默认 false；停派发期间已有启用订阅仍写入 outbox，避免静默丢失。每次发布最多 16 个匹配目标，每分支原生 pending 上限 512；导出时中央 queued/dispatching 达到 512 会保留原生待导出事件，手动重投递另受每个原事件和操作者额度约束。额度不足时明确拒绝对应发布或保留待导出事件。历史事件/对象不做物理清理。

首次升级必须分两步：先部署 migration 043 和新 API/Worker/Web，保持开关 false，确认旧 Worker 已退出；随后只开启对象事件开关。旧 Worker 不识别新事件类型，禁止混用旧派发器接收对象事件。当前沿用已验证 Guest 26d35375…，无需再次下载或生成 Guest 镜像。完整商业 SDK、分支触发器继承、事件计费、自动保留清理与无限制事件总线不属于本片基础闭环。

真实去重示例见 `examples/functions/object-events/index.mjs`，Linux UI 验收见 `web/e2e/functions-object-events.spec.ts`。交付状态以对应不可变证据报告为准。
