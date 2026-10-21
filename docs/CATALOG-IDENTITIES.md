# 原目录身份与外部 DDL 观察

这是 C02 的身份观察路径。客户默认连接选择见[独立合同](CONNECTION-DEFAULTS.md)。
读取观察结果不执行 SQL 修复；受控原名称恢复见[独立合同](CATALOG-IDENTITY-RESTORE.md)及[基础交付](DELIVERY-2026-10-20-CATALOG-RESTORE.md)。默认库 Owner、平台 CRUD 的持续身份绑定和
Backend 依赖迁移仍须继续实现和独立验证，整个 C02 尚未关闭。

数据库与角色页可显式读取身份变化。Editor/Admin 经原服务探测身份和 Proxy
读取 Writer；可能唤醒 Compute。查询在只读 Repeatable Read 事务中核对真实
Neon tenant/timeline，包含全部角色和数据库，包括 NOLOGIN、禁止连接及模板库。
每类最多 1024 项、每个管理注释最多 4096 字节；超限整体拒绝，不交付部分目录。
不查询用户表、库大小、客户角色密码或 SCRAM verifier。原服务探测凭据仍用于
认证连接。管理注释只留内部摘要，不把任意原始注释带到公开 API。

旧的管理记录只有名称，不能证明观察前是否已被删除重建。Admin 可选择尚未
记录的受管理非系统对象，明确确认“从当前时点开始记录”，提交当前原生 OID、
规范引号 `If-Match` 管理意图版本和 `Idempotency-Key`。服务核对分支管理标记、
角色规范选项或数据库 Owner/选项；角色只记录原不可变凭据 Secret UID，不解码
客户 verifier。系统身份和默认系统库不能用这个入口认领。

`GET /api/v1/projects/{project}/branches/{branch}/catalog-identities` 返回完整原生
对象清单和历史基准差异。`POST` 同路径需要 Admin、Cookie CSRF 或受限 Admin
API key，以及 `acknowledge_initial_observation:true`。记录、原响应回执、审计在
一个元数据事务内提交，同时重查当前权限、父资源、意图版本与活动操作。
不同请求不能覆盖同一原名称或 OID 的基准；原请求先重放原响应，Writer 已退休
或意图后来变化也不创建新基准。Console 未知响应保留内存中的原内容、版本和键，
显式重试，不存入浏览器持久存储。

原生比较以 OID 为主：同一 OID 换名显示 `renamed`；原 OID 消失而原名称被新
OID 占用显示 `replaced`；原 OID 尚在而原名被占用显示 `name_collision`。
缺失、管理标记、角色选项、超级用户权限、数据库选项、Owner OID 和原 Owner
角色名称变化分别显示。复制原管理标记不能把新 OID 变成原对象。

每条结果始终带 `credentials:not_observed`。`catalog_matches` 只表示这些目录
字段一致，不能表述为密码同步或全部状态一致。普通平台操作若改变了管理意图
版本，显示 `intent_changed` 并附原生比较 `catalog_status`；不会把正常平台操作
自动标成外部 DDL，也不会覆盖原基准。分支 fork 不伪造源分支之前的身份历史。
这些基准当前用于观察，不为旧 CRUD 额外提供外部并发栅栏。

迁移 049 只增加元数据表。普通 PostgreSQL 集成测试使用显式自定义 GUC 测试
scope 拒绝，真实 Neon 的 scope、Console、外部 DDL 及原数据保留须另有现场回执。
性能、可靠性、HA/DR 和外部 fencing 专项按用户要求后置。

现场功能回执见[2026-10-21 交付](DELIVERY-2026-10-21-DEFAULT-OWNER.md)；已知继承角色替换的保留处理见[独立合同](CATALOG-FORK-EXCLUSIONS.md)。

初始化后的冷启动目录保留及仅目标角色原生请求见[合同](COMPUTE-CATALOG-BOOTSTRAP.md)和[0.66.3 交付](DELIVERY-2026-10-21-COMPUTE-CATALOG-BOOTSTRAP.md)；当前原生身份代际仍继续。

2026-10-21：普通目录持续身份基础已在 0.67.0 / migration 056 验收，见[交付记录](DELIVERY-2026-10-21-CATALOG-OBJECT-BINDINGS.md)；分支继承、旧链衔接和系统/Backend 范围继续，不关闭 C02。
