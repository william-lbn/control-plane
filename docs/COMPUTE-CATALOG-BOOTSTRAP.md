# Compute 初始化与冷启动目录保留

首次启动需要 Neon 初始化平台角色、探测角色、扩展和继承目录。初始化完成后，
PostgreSQL/WAL 保存实际目录；平台的角色、数据库和 Owner 操作先执行明确 SQL，
再发布配置。后续冷启动不应再次按旧配置修改 SQL 目录。

此路径使用原生 `spec.skip_pg_catalog_updates`。首次初始化仍使用原配置；
成功 SQL 探测及继承目录处理完成后，Worker 在同一原创建流程中记录初始化
见证，再以固定 Secret UID/resourceVersion 发布该选项并读回确认。
新 Writer、Reader 都有自己的记录；Reader 原有 Replica/streaming 检查保持。
重试创建在重新创建 VM 前恢复原初始化见证，防止退回首次目录重放。

## 见证与发布

迁移 055 的不可变初始化记录绑定原 Operation、项目、分支、Endpoint 类型、
tenant/timeline、配置 Secret UID 和凭据 Secret UID；发布确认另有不可变记录。
新执行验证原租约、原 payload、原创建资源、正在进行的 SQL 探测步骤和父资源。
首次状态对应现有模型：项目/Endpoint 为 `provisioning`，分支为 `creating`。
原创建重试可以恢复它自己的 `error` 资源；不能把其他父项目/分支的 error、
删除或退休状态视为可用父资源。

旧 Endpoint 需要唯一原成功创建操作和成功 SQL 探测步骤，再记录当前固定
backing 的首次观察边界。它证明从该边界开始使用这份配置与凭据，不补认此前
原生对象或 Kubernetes UID 的历史。没有原创建记录、归属或完整 backing 的
导入资源保持原样，需要独立确定其管理来源。

核对包含项目标签、原生 tenant/timeline、Primary/Replica 模式、平台/探测角色
配置和受保护探测密码。早期凭据缺少 `probeVerifier` 字段时，仅在内存中核对
配置中的有界 SCRAM verifier 与原受保护探测密码；不生成 verifier，不修改
凭据 Secret。字段存在而不匹配、格式错误或密码不匹配都会拒绝。

发布只改 `skip_pg_catalog_updates`，保留角色、数据库、Owner、delta operations、
设置及其他配置字段。冲突重新读取原 UID，未知写入结果由后续重试读回确认；
新 UID 不能替代原 backing。记录完成不触发 configure、强制重启或新凭据创建。
带有额外 local proxy 或启动前删除订阅要求的配置需要独立迁移，不自动接入。

## 明确新建角色

原生 `configure` 接口也遵守目录跳过选项。平台明确新建角色时，先只读检查
目标名不存在、角色配额、平台权限角色存在且没有会触发原生清理的 invalid
数据库。它在临时请求中仅携带本次新角色及 `ADMIN control_probe`，数据库
定义和 delta operations 均为空，临时启用 apply。成功后再发送跳过目录
更新的完整运行视图。持久 Secret 的目录保护保持启用。

这样新角色可以获得 PostgreSQL 16 后续管理所需的权限，其他角色、密码、
数据库 Owner 和历史删除意图不参与该次 apply。原生引擎自身的幂等平台
初始化阶段仍按固定 Guest 执行；invalid 数据库会先被拒绝，保留待核对数据。
已有同名角色不会通过这条新建路径被认领。原结果重放仍先读取原不可变结果，
不会再次提交这些临时请求。

## 原生语义和边界

适用版本和真实原生验证以对应交付报告为准。当前固定 Guest 的正常 PostgreSQL
启动、配置加载和 Neon 扩展启动后处理仍执行；跳过完整目录 apply 也会跳过其中
的启动迁移/可选服务初始化。Guest 或这些可选服务升级需要自己的迁移方案，
不能把已有初始化记录当作新版本迁移已完成的证明。

冷启动保留 SQL 中的独立同名角色、原 OID、密码 verifier、属性、数据库 Owner、
选项和客户数据。配置中仍有旧角色/Owner 意图，也不会据此认领或重写替换对象。
外部 SQL 密码不会自动成为 Proxy 的新凭据；这里的密码保留验证比较原生密码
摘要，不声称已经接入该外部密码登录。

普通 CRUD 的当前 OID/凭据代际、系统身份漂移和 Backend 名称依赖继续按 C02
推进。本记录不替代这些身份绑定，也不构成跨实例或外部 DDL 栅栏。
性能、可靠性、HA/DR 和外部 fencing 专项按用户要求后置。

关联合同：[目录观察](CATALOG-IDENTITIES.md)、[原结果恢复](CATALOG-RENAME.md)、
[已知继承替换](CATALOG-FORK-EXCLUSIONS.md)。
