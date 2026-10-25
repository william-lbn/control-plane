# 平台目录身份与 Backend 名称迁移

实现合同以 [OpenAPI](../contracts/openapi-v1.json) 为准。现场基础结果见[验收回执](DELIVERY-2026-10-26-CATALOG-PLATFORM.md)；本文描述固定功能合同，维护环境资格与生产门槛另列。

## 平台与默认身份

目录页提供显式“核对平台与默认身份”。观察通过当前原 Writer 的受管理 `control_probe` 凭据连接固定维护库 `template1`，读取同一 tenant/timeline 的 OID、名称、属主、选项及管理标记摘要。观察不读取密码、密码 verifier、原始注释或客户表数据。

首次记录必须由当前项目 Admin 确认，并提交当前完整观察的 SHA-256、确认字段和原 Idempotency-Key。Editor 可以观察，Viewer 无权读取。原记录与原请求回执、审计在同一元数据事务提交；一个分支的同一原生 tenant/timeline 只允许一份初始记录。它证明记录时点的身份，不补写更早历史。

记录包括原 Endpoint 的默认角色、`control_probe`、默认数据库、`postgres` 与 `template1`，同名去重。后续始终按原 OID 比较；原名称被其他 OID 占据、原对象改名、Owner/角色选项/数据库选项/标记变化分别展示。Endpoint 或 Secret UID 变化也单独显示，不替换原生身份记录。新 timeline 不继承旧 timeline 的平台身份结论。

维护库与原 Proxy 路径可连接时，观察按原 OID 比较默认库外部改名；维护库、探测角色或连接路径不可用时返回无法观察。不重建系统对象、不重置密码、不将未知结果标记为正常。系统对象仍受普通 CRUD 和 rename 保护。当前 Neon 的 compute_monitor、vm-monitor 与 pg_cron 持有默认 `postgres`，外部改名返回 `55006`；该实际维护窗口验证单列，不能标为已通过。

`GET /api/v1/projects/{project}/branches/{branch}/catalog-platform-identities` 返回候选观察或原记录差异。`POST` 接受 `acknowledge_initial_observation: true` 和 `observation_sha256`。同一原键重放返回原记录；观察变更返回 412，已有原记录禁止再次初始化。浏览器丢失 POST 响应时保留原输入与原键，使用“恢复原平台身份记录”。

## Backend 数据库名称依赖

普通改名仍拒绝默认连接和 Backend 引用。当前项目 Admin 可在数据库改名请求中显式传入 `migrate_backend_dependencies: true`，或在目录页勾选同步迁移。角色改名不接受该选项，系统数据库、默认连接及默认 Owner 的原保护保持有效。

前提是 Auth、Data API、Storage 已停用；相关已初始化 Functions 已挂起，所有实例已退休。未完成的服务安装、活动 Function、外部启用的 Deployment、尚未退出的 Pod、替换 UID、活动分支操作或来源快照变化均拒绝。删除的 Function 定义保留为历史，不改写其旧名称和凭据回执。

一个原 Operation 绑定原生数据库 OID、所有 Compute 配置及 Proxy 的 UID、服务元数据快照、原凭据 UID、Function 版本和原签名 SQL 引用。Worker 在原数据库改名前准备确定名称的新不可变连接文件；未知创建结果先读取并验证原提案。原 Secret 始终保留。

| 依赖 | 名称迁移行为 |
|---|---|
| Auth | 只改变新不可变配置内 databaseURL 的数据库路径；密码、verifier、Cookie/TOTP 密钥、OAuth 与其他原私有配置保留 |
| Data API | 只改变新不可变 PostgREST 配置的 db-uri 路径；密码、verifier、委派签名种子、公钥与其他文件保留 |
| Storage | 只改变停用实例的数据库名称；原安装、事件和数据代际保留，业务对象字节不复制 |
| Functions | 原数据库范围的不可变 SQL 凭据回执保留；新代际使用原密码/verifier 签名新数据库范围，后续恢复读取当前代际 |

数据库原 OID、ACL、Owner 和数据由 PostgreSQL 改名保留。最后的短事务同步提交受管理目录名称、Backend 引用、当前 Functions SQL 代际、不可变回执及审计。新 Function 数据库范围缺少对应凭据回执时，延迟约束拒绝整个提交。活动名称迁移期间，Function 定义变更受到元数据保护。

迁移不会自动启用服务。迁移后的显式启用继续遵循各服务原有代际与凭据策略；这与名称迁移本身保留原凭据是两个独立动作。客户自行保存的连接字符串仍需更新。

## 验证边界

只使用专用新建项目和小数据验证身份、权限、原键恢复、事务原子性、服务连接和数据保留。既有目录和初始化回执继续复用。大对象、吞吐、并发压力、HA、故障可靠性与完整环境认证按既定计划后置。
