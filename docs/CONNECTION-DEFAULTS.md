# 客户连接默认值

本合同覆盖 C02 的连接身份选择路径；[本地基础交付](DELIVERY-2026-10-20-CONNECTION-DEFAULTS.md)已通过。默认数据库 Owner 迁移、任意外部 DDL
的持久 OID 对账及修复仍须单独实现和验收；不能据此关闭整个 C02。

项目 Admin 在数据库与角色页选择分支的客户连接默认角色和数据库。角色须为
ready、API 管理的非系统登录角色；数据库须为 ready 的受管理数据库，或该分支
原平台默认数据库。该原始库名由 Endpoint 初始化配置确定，可能为 `postgres`，
不能写死为 `neondb`；新投影的 `platform_role/platform_database` 提供这两个原始
名称，历史原回执仍保留当时表示。选择原始 `postgres` 仅改变客户连接默认值，
不会解除系统数据库的删除、改名或目录认领保护。服务通过原 Writer 的 Proxy 验证真实 tenant/timeline、角色
OID/分支标记、数据库 OID/Owner/标记及 CONNECT 权限。目录查询不读取用户表、
客户角色密码或 SCRAM verifier；连接继续使用原平台探测身份，不分配新的管理身份。

`GET /api/v1/projects/{project}/branches/{branch}/connection-defaults` 是 Editor/Admin
元数据读取，不唤醒 Compute。首次返回虚拟版本 `"1"`。同路径 `PATCH` 需要
Admin、Cookie CSRF 或受限 Admin API key、原版本的规范引号 `If-Match` 及
`Idempotency-Key`。提交体是 `{"mode":"managed","role":"app","database":"app_db"}`，
或恢复初始值的 `{"mode":"platform"}`。

提交在同一元数据事务中重查当前用户和项目/分支、角色凭据引用与版本、数据库
版本与 Owner、原 tenant/timeline、活动操作，并写默认值、原响应回执与审计。
任何一步失败均回滚。同一原请求优先返回原回执，即使默认值后来再次变更；不同
内容复用原键返回 409。Console 在未知回执后保留内存中的原内容、版本和键，
由用户明确重试。版本冲突需要刷新，不会自动覆盖其他人的选择。

目录页在当前浏览器标签页内按项目记住所选分支；只保存分支 ID，不保存凭据或
待提交请求。所选分支失效时重新选择可访问的就绪分支，避免返回目录页时先唤醒
其他分支。存储受限时正常退回可用分支选择。

连接串、Endpoint 的 `connection_defaults` 投影、SQL 单条及脚本省略的连接目标
使用客户默认值。SQL 编辑器在版本变化时重新选择目标并清除旧密码，仍须用户
输入所选角色的现有密码。显式 SQL 目标继续有效。数据库和表/Schema Owner、
ACL、已有会话、Backend 服务账号均不随连接默认值更改。

原 Endpoint `role_name/database_name`、平台 `cloud_admin/control_probe`、初始化
凭据、Compute 配置及 Proxy 角色凭据保持各自职责。不得通过修改默认值去改名
或删除系统身份，也不会复制客户角色密码覆盖平台凭据。当前作为客户默认值的
受管理角色/数据库在删除或改名前须先迁移连接默认值；密码轮换不改变对象身份。

当前时点分支 fork 在受保护的原分支事务下复制客户默认值及物理 OID，并绑定新
timeline；历史恢复不复制今天的客户默认值。Writer/Reader 使用同一分支默认值。
切换为平台模式递增版本、保留回执与审计，原客户数据库、角色及数据继续保留。

迁移 048 是仅向前的元数据扩展。普通 PostgreSQL 测试以显式自定义 GUC 验证
scope 拒绝逻辑；它们不替代真实 Neon tenant/timeline、Console 和 SQL 验证。
原 OID 观察是这次选择的证据，不等于历史对象追踪或自动外部 DDL 修复。
