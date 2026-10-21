# 旧目录身份与当前代际衔接

0.70 功能已通过本地实机验收，见 [2026-10-21 旧目录身份衔接](DELIVERY-2026-10-21-CATALOG-CURRENT-OBSERVATION.md)。

`POST /api/v1/projects/{project}/branches/{branch}/catalog-identities/{binding}/observe-current` 将已有首次观察的原 OID 衔接到当前管理代际。需要当前项目 Admin、会话 CSRF 或有范围的 Admin key、`Idempotency-Key`、当前版本的规范引号 `If-Match`，以及 `acknowledge_current_observation: true`。`name` 指向当前受管理名称。

服务重新读取完整原生目录，核对原租户/timeline、原 OID、当前名称与分支标记；数据库还需已有持续身份的当前 Owner 角色。原生对象缺失、同名重建、名称占用、系统对象、超级用户冲突、管理排除、待执行分支操作或已有持续身份历史均拒绝。改名后的名称需要原成功 rename 回执，当前凭据须匹配原基线 UID 或原成功操作中记录的不可变 Secret UID。旧普通回执只证明它记录的凭据 UID，不会被补写为旧 OID 证明。

事务内再次检查权限、管理代际、凭据及 Owner 记录，原子写入持续身份、058 不可变观察、请求回执和审计。原请求重放优先于当前凭据和原生目录读取；后来改名或改密不会改变原回执。首次观察历史保持原样，历史选项为 `not_recorded`，密码为 `not_observed`。此入口不执行 SQL 修复，不重建对象，也不迁移数据或 ACL。

首次观察一个继承的旧角色时，其凭据可能仍属于父分支 Compute。只有原 057 已发布继承来源、成功 SQL 观察、原 OID 和原 Secret UID 全部匹配，才能使用该凭据背书。仅复制项目/Compute 标签的替代 Secret 不会通过。旧继承意图须仍是原第一代，不能以今日父分支状态补认历史。

控制台提供“衔接当前身份”。先衔接 Owner 角色，再衔接数据库；选择、确认和原请求重试保留相同输入。读取和衔接均不观察客户密码，不将私有凭据引用、UID、verifier 或原始管理标记返回浏览器。

局部功能测试入口为 `catalog-current-observation.spec.ts`。使用既有继承分支和客户数据验证首次观察、显式衔接、受控改名/改密、原回执重放和冷启动后的目录保留。性能、压力、HA/DR 与外部并发栅栏按当前任务要求后置。
