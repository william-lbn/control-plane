# PostgreSQL Function 示例：空闲断开与重新连接

原真实 guest 的保护日志确认 `57P05` 空闲会话超时，以及未处理的连接池 `error`
事件导致 Node 退出。原日志只保留在 Linux，未把连接对象、凭据或原始日志上传 Git。
参考 [node-postgres pooling](https://node-postgres.com/features/pooling) 和
[pg.Pool error 事件](https://node-postgres.com/apis/pool#error)：空闲连接可能因后端/网络
错误断开，应用需要错误监听；损坏的空闲客户端由 pool 移除。

`examples/functions/postgres/index.mjs` 增加错误监听，仅记录有界诊断计数，不记录
error/client（可能包含密码），不退出 Node，不自动重试 SQL，不吞掉活动查询失败。
下一次明确调用由 pool 建立新连接。原 TLS/CA/主机名/Proxy selector 和受限角色不变。
没有修改 Neon、Postgres、autoscaling fork 或降低服务端/Endpoint 超时。

真实 Linux UI 验收在同一受控 Function 中：一次写入、明确断开、Node 存活、新连接
读取同一角色/两行原数据，再恢复此 SDK 版本并保留写入。准确版本和失败记录见
[0.1.25 现场报告](https://github.com/william-lbn/neon-helm/blob/v0.1.25/docs/ACCEPTANCE-2026-10-13-FUNCTIONS-ROLLBACK.md)。

`APP_IDLE_SESSION_PROBE=1s` 仅用于明确配置的示例验收，在该应用连接启动参数中
设置会话超时，保留原 selector。普通部署不配置它。`idle_client_disconnects` 是示例
进程内诊断值，不是可信平台计量，也不能授权自动停止、证明 HA 或实现写入 exactly-once。
未知写入结果先查看原数据；平台不得自动重放调用来恢复应用。
