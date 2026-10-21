# 原默认数据库 Owner

部署要求启用版本化 Backend 凭据密钥配置；API 和 Worker 都只读挂载同一份
保留密钥 Secret，以便原操作跨进程解封。Helm 使用现有
`backendCredentials.existingSecret`，不生成替代密钥，不增加 Secret 或 RBAC。
Worker 的挂载遗漏会导致操作在原 SQL 迁移前失败，保留其封存凭据和原操作；
补齐相同保留密钥的挂载后，从原操作重试，不能创建替代请求绕过保留状态。

`GET /api/v1/projects/{project}/branches/{branch}/default-database-owner` 由项目
Editor 读取实际数据库 OID、Owner OID、原平台身份、当前版本、候选受管理角色和
原操作。读取经原 Writer、Proxy 和固定 `template1` 维护库，可能唤醒 Compute。
不返回密码、verifier、内部凭据摘要或任意数据库注释内容。

`PATCH` 由 Admin 提交，要求会话 CSRF（或具有 Admin 范围的 API key）、规范带
引号 `If-Match`、原 `Idempotency-Key`、已观察的数据库/旧 Owner OID、明确迁移
确认及当前原平台 `cloud_admin` 密码。首次记录必须另外确认当前观察，不能将
这次观察声明为更早历史的证明。managed 模式选择原生 OID、标记及选项匹配的
ready 受管理角色；platform 模式从原平台身份推导目标，不接受任意系统角色。
已有身份基准记录的同名替换对象拒绝接管。

原操作绑定 tenant/timeline、原数据库/平台/新旧 Owner OID、当前注释摘要、
数据库选项和 ACL、目标角色版本及不可变凭据 UID、平台/探针/目标凭据意图、
全部保留 Compute 配置 UID 和原 Proxy 路由 UID。密码只用于认证、带密钥请求
摘要及用途隔离 AEAD 封存，不进入操作 payload、审计、公开回执或 SQL 字面量。
封存绑定原操作/项目/分支/Endpoint/原凭据 UID/原数据库 OID/原生 scope；成功
结果、当前 Owner 版本、成功审计与凭据清除同事务提交。

操作先验证原对象和配置，再以 CAS 更新 Reader 冷配置、最后更新 Writer，仅
改变默认数据库 Owner，保留其他配置及并发无关变更。不会调用全量 compute
configure，也不会更新 Proxy 路由、平台密码或探针账号。随后使用已授权原
平台密码在事务中 `ALTER DATABASE ... OWNER`，核对原 OID、Owner、注释、选项、
目标角色及 ACL 后提交。原数据库、表、Schema 的 OID、数据及原表/Schema 属主
保留；PostgreSQL 会重写数据库 ACL 中旧属主的 grantee/grantor 引用并合并重复
权限，不能把这声明为 ACL 文本逐字不变。保留数据库原禁止连接状态。

SQL 成功但元数据回执未知时，原操作可由低权限探针确认严格目标状态，无须
再次解封密码。原结果已提交后，恢复只补原步骤和回执，不重新应用 SQL、密码
或撤销后续 Owner 变更。原 HTTP 请求的幂等重放先于当前 Writer 和版本检查。
失败后保持原操作及分支保留状态，通过原操作重试；其他目录修改、Endpoint
集合变化及无关旧操作重试不能绕过保留状态。外部冲突须保全并处理原对象，
不得以 DROP/CREATE、自动改密码或替换 Secret 来消除冲突。

当前默认 Owner 角色不可直接删除或改名；先通过本流程迁移 Owner。原默认
数据库不可通过普通目录删除、改名或改 Owner。角色密码独立轮换仍可进行，
既有初始历史不冒充轮换后的当前凭据观察。客户连接默认值、已有连接、Backend
服务账号及现有表/Schema 属主独立于数据库 Owner。

新 Endpoint 的冷配置投影当前 Owner。live fork 保留数据库/Owner 物理 OID，
在子分支新 timeline 下复制当前 Owner 绑定，保留用户任意数据库注释；historical
restore 不复制今天的 Owner 意图，也不把当前属主重放到历史 SQL。

当前边界：这是默认数据库属主的功能闭环；普通目录 CRUD/分支持续 OID 身份
谱系和 Backend 名称依赖迁移仍属于后续 C02 工作。会话锁阻止正常内部休眠，
不代表外部 DDL 分布式 fencing、HA/DR 或长期可靠性验证。性能和可靠性专项后置。

现场功能回执见[2026-10-21 交付](DELIVERY-2026-10-21-DEFAULT-OWNER.md)；已知继承角色替换的保留处理见[独立合同](CATALOG-FORK-EXCLUSIONS.md)。
