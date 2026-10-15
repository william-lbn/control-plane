# 个人 SQL 历史采集与保留

## 目标与默认值

这是个人执行历史的隐私/容量管理，作用于本人在所有项目的历史；API 0.39.0 / Chart 0.9.26。
不会更改数据库数据、
保存查询、其他成员历史或审计。迁移 035 是纯新增：没有用户显式配置时继续采集并
保留全部历史，不执行默认物理删除。现有任何测试记录不会因升级自动过期。
项目共享历史、单条手工删除、审计/合规保留和结果未知记录处理仍是后续独立需求。

`sql_history_preferences` 按 owner_id 保存 capture_enabled、retain_days、generation、
updated_at；没有行的虚拟默认版本为字符串 `0`。期限只接受 0/7/30/90 天，0 表示保留
全部。关闭采集只影响后续 admission，不移除在途请求或旧记录；全局运维开关仍优先。
`sql_history_policy_requests` 保存 owner/key hash、请求指纹和非敏感回复，支持持久
幂等；不保存 SQL、密码或结果。策略变更和审计、幂等回执同事务。

## API

| 方法 | 路径 | 要点 |
|---|---|---|
| GET | `/api/v1/account/query-history-policy` | 本人默认/当前设置；ETag 为 quoted version_token |
| PATCH | `/api/v1/account/query-history-policy` | Cookie/CSRF、Idempotency-Key、If-Match；完整 capture_enabled/retain_days；非零期限必须 acknowledge_expired_history_deletion=true |
| POST | `/api/v1/account/query-history-cleanup` | Cookie/CSRF；必须明确确认物理清理；仅本人、一批最多 1000 条 |

PATCH 重放同一 key/input 返回原回复，不再次变更 generation；不同输入 409、过期
If-Match 412、缺版本 428、错误输入 422、每账号每小时最多 60 次新策略 429。
API Key 不能管理个人策略。数据读取/设置无需密码或 Compute，不会唤醒数据库。
UI 在“组织”及 SQL 历史的设置区域使用同一组件。提交结果未知时保留原 key/input
重试；重新读取策略后才允许放弃原重试和发起新的编辑，不自动重复写入。

## 物理清理与并发

持有领导租约的 Worker 维护任务每小时尝试最多 1000 条；用户也可明确执行自己的一批。只选择本人
显式配置非零期限、started_at 超期、finished_at 不为空且 outcome 属于 completed /
failed / not_submitted 的记录。admitted、outcome_unknown 和缺少完成回执的记录保留，
不以时钟到期冒充 SQL 已完成。

清理事务同时锁定候选历史和策略行，SKIP LOCKED 防止并发清理重复处理；策略变更
与正在清理的一批有明确锁顺序。DELETE 与每账号聚合审计同事务：审计失败回滚删除。
默认无期限/其他账号/新记录不受影响。版本与删除回执不包含 SQL 文本。失败应先
查询历史，不能把请求超时解释成“删除未发生”。自动任务每批有 10 秒截止时间。

这里的物理清理指 DELETE 元数据库记录，而非软删除。WAL、备份和数据库死元组仍
遵循各自运维保留策略，不声称已经从存储介质安全擦除历史字节。

## 验证与安全复测

使用独立 `control_ci` PostgreSQL schema，构造合成历史验证：虚拟默认、采集关闭、
期限确认、幂等/版本冲突、其他账号隔离、三类未完成记录保护、审计原子性与并发清理。
实际 Console 以本批新建且没有 SQL 的隔离账号测试策略设置、持久重载、恢复默认和
空目录清理，不用已有业务/测试历史做破坏性验收。所有旧记录和原始文件保持。
实际验收见 [当前交付](DELIVERY-2026-10-16-CURRENT.md)：八项策略 UI 和三项补充 MFA
检查通过，原账号/原失败保留，未清理既有记录。代码存在不等于整个产品生产准入。
