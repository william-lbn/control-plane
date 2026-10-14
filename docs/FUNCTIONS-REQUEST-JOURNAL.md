# Functions 已调度请求日志

控制面 0.30.0 / metadata migration 030 / chart 0.9.14 已本地部署。日志是实际调度状态元数据，不采集 customer stdout、路径/query/headers/body，不能代替完整访问日志或 billing。

## 请求流程

1. 现有租户、函数、公开 policy、实例/generation、健康与共享 admission 锁校验。
2. 在控制 PostgreSQL 插入 in_progress 记录；插入失败返回 503，明确尚未 dispatch，不发送 guest 请求。
3. 原请求只 dispatch 一次；Console 收集受限响应，公开 HTTP 转发原流。
4. 写 completed/outcome_unknown/response_incomplete/client_disconnected。更新失败保留 durable in_progress，不把可能已执行的请求标成功，也不自动 replay。过 deadline 的读记录显示 completion_unconfirmed。

计时从 dispatch admission 到完成，未包括先前 cold wake。响应字节指应用响应 payload：Console 已接收数据，公开 HTTP 已转发部分。不会暴露客户 headers/payload，开发者 stdout 有独立后续 gate。

## 模型

function_request_events：id、request_id（唯一）、function/project/branch/deployment/instance identity、channel、method、outcome、application_status、response_bytes、duration_ms、started_at、finished_at、deadline_at。复合外键保证函数、deployment、instance、项目和分支相同；索引支持 started_at/id 降序分页。

## API / UI

GET /api/v1/projects/{project}/branches/{branch}/functions/{slug}/request-logs?limit=25&after=...

现行项目读取权限必须通过，日志是纯 metadata read，不读 Secret、不唤醒 compute/guest。limit 1–100，cursor 绑定函数，返回 items/next_after/function_id/capture/retention，no-store。来源 Console / public_http；状态区分已完成、执行中、结果未知、响应中断、客户端断开、完成未确认。Viewer 可查看。

当前保留策略 held_until_operator_policy：不自动删除原日志或证据；运营 retention/审计存储/export/指标告警为独立演进。函数物理 GC 须协调历史引用，不能删除有引用的记录冒充回收完成。

## 验收

新 PostgreSQL 测试验证 durable admission、一次完成状态、跨 tenant/branch 外键与 API、opaque keyset pagination、无敏感输入捕获。新 UI 套件将创建专属小函数作为测试 setup，不重复旧 CRUD/自动休眠循环；从 UI 和独立公开 origin 各调用一次 /health，检查真实 request ID 在日志页出现，再停止实例与 Compute，保留 journal/函数元数据/数据/证据。原案例真实 UI 已完成，见 [精确交付](DELIVERY-2026-10-15-FUNCTIONS-JOURNAL.md)；未标为生产准入。
