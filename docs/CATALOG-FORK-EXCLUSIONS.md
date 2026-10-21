# 已知继承角色身份替换的保留处理

live fork 继承的 ready 受管理角色，如果有原 049 OID/凭据 UID 观察或上游
不可变继承排除记录，先在固定源 Writer、原 tenant/timeline 下读取原生目录。
同名新 OID 不能通过复制管理注释成为原角色。没有原记录的对象不虚构历史；
正常源 OID 不被排除，缺失、系统角色和关键依赖冲突拒绝完成分支。

若原角色仍被数据库 Owner、原始 Endpoint、客户连接默认值、默认数据库 Owner
或保留 Backend 配置引用，不能按本流程忽略依赖。源分支必须 ready，项目与
组织允许操作，无 Owner 变更保留状态；原创建操作租约、父/子 tenant/timeline、
原源基准和子凭据意图在元数据事务重查。最多处理 500 个已知角色。

052 的 inspection/exclusion 记录不可变，绑定原 create_branch 操作、源基准、
期望/实见 OID 和原凭据引用/UID。不能覆盖原 049 历史，不能在重试时换绑新的
同名替换对象。上游排除传给后代仍要核对相同实见 OID；历史恢复不复制今天的
目录管理意图。原创建 SQL probe 已成功的操作不会被回溯改变管理范围。

子原生 OID/scope 在配置过滤前后只读确认。原 immutable 凭据 Secret UID、
保留配置 UID、全局路由 UID 与 verifier 摘要固定；旧失败操作的配置还须属于
原 create_neonvm 时间窗口。所有重试都复查原身份，拒绝凭据漂移或替换 UID。
仅从子配置删除该角色匹配的旧 managed role 定义，从该 Endpoint 路由删除
匹配的旧角色凭据；保留其他配置、路由、数据库、选项和并发无关变更。
冲突重读后有界 CAS，写入响应未知时由原操作重查确认，不换新请求。

不调用 SQL DROP/ALTER、GRANT、密码修改、COMMENT 或全量 compute configure。
已存在依赖、该角色的 native delta、异常配置字段或凭据内容拒绝过滤。
原 role 元数据行保留，不能标 deleted 导致冷启动 DROP；冷配置投影跳过它。
过滤结果与成功审计原子提交，原回执复用，ready 状态不退回。

目录显示 `management_excluded:true`、`managed:false`、
`state:external_identity`。普通删除、轮换、改名、新建库 Owner、默认 Owner
选择和新身份基准初始化拒绝，返回 `catalog_inheritance_identity_excluded`。
Console 标注“保留的外部 SQL 角色”和“平台管理已隔离”。物理角色、源注释、
数据和原凭据 Secret 均保留。

这是已知、非关键依赖角色的继承替换处理。普通源分支 CRUD 的持久 OID、后续
合法凭据代际、数据库替换和完整冷启动保护另行开发；不能据此声称所有目录
DDL、未知对象或跨实例栅栏已经解决。真实范围见
[Owner 与原失败分支交付](DELIVERY-2026-10-21-DEFAULT-OWNER.md)。
