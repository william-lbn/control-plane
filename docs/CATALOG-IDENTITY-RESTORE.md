# 恢复原目录身份

本路径在[不可变原身份基准](CATALOG-IDENTITIES.md)上，恢复外部改名后的原对象。
它保留原管理意图名称和版本；不认领同名新对象，也不覆盖基准。默认库 Owner、
普通 CRUD/分支的持续身份绑定和 Backend 名称迁移仍属 C02 后续业务。

Admin 在身份观察中选择“仅名称变化”的原对象，确认恢复原名称。角色另须明确
确认把密码重设为平台原有受管理凭据；当前外部密码没有被观察。数据库恢复不
接受该密码重设确认，不迁移 Owner。API 为
`POST /api/v1/projects/{project}/branches/{branch}/catalog-identities/{binding}/restore`，
需要原基准意图的规范引号 `If-Match`、`Idempotency-Key`、Admin 与 Cookie CSRF
或受限 Admin API key。请求不携带或返回密码/verifier。

准入重查原项目/分支/Writer、原意图版本、tenant/timeline、原 OID、管理标记、
全部已观察角色/数据库选项和原 Owner。只允许名称一个字段变化；原 OID 消失、
被替换、原名称被占用、权限/标记/Owner/选项变化均拒绝。当前外部名称也须在
支持的非系统标识符范围内。系统身份不通过此入口更名或认领。

角色原不可变凭据 Secret UID 必须与基准相同。每个保留 Compute（包括冷 Reader）
的配置 UID、全局 Proxy 路由 UID 绑定到原操作；它们必须已表达原名称、Owner
和凭据。恢复不自动改写漂移的配置或路由，也不覆盖其他对象字段。

持久分支目录操作按原 OID 执行事务性 ALTER，并检查原 OID 后置条件和完整
原生目录结果。角色在明确确认后重设为原 verifier。操作不执行 DROP/CREATE、
复制数据、强断会话或 REASSIGN OWNED；原数据库 OID、ACL、Owner、表/Schema
所有权和数据保留。整个过程继续使用原服务身份，不修改客户默认连接选择。

分支操作排他、当前父资源检查、原配置/路由、Worker 租约与原意图在提交结果时
重查。migration 050 记录原操作的结果及成功审计；审计失败会回滚结果。未知
原生提交可由同一原操作再观察原 OID 后续做。结果一旦提交，后续原操作恢复
只补齐原步骤/响应，不再应用旧密码或对新的外部 DDL 做第二次修复。

Console 未知准入响应保留内存中的原内容/键/版本；得到操作 ID 后继续观察或
重试同一个失败操作。它不把凭据、待提交内容存入浏览器持久存储。原请求重放
优先返回原操作，即使 Writer 后来退休或原意图变化。

目录中的 `credentials:not_observed` 仍保留；一次恢复不等于之后永远没有外部
密码变更。真实冷 Writer/原 Reader 的原密码、OID 和数据需要独立现场回执。
外部 DDL 并发栅栏、HA/DR、长期可靠性与性能专项按用户要求后置。
