# 剩余需求：源码核对与实施账本

## 1. 核对基线

审计基线为控制面 `b4ad4f4`，此前已交付 `63456a7`，最初增量 API/Worker、Web 0.48.0；并核对 Autoscaling `1720924` 和统一 Helm `648488a`，以及本地
website 文档快照 `c0d49cbb6979b2ce79ea502d62dbc40780a923b0`（提交时间
2026-09-24T19:22:11+01:00）。这是本地可追溯的官方文档基线，不声称它包含该提交后
所有在线变更。公开 Git 源码已推送，不等于所有最新本地镜像已公开发行。
本表的“已有”是可定位的实现；现场通过以各版本 delivery 为准。
功能组没有固定的“全部功能数量”，本账本按可独立实施、可验收的需求划分。

## 2. 需求列表

核对路径：官方仓库 `content/docs/compute/functions/`（尤其 websockets、triggers、logs）、
`content/docs/storage/`（S3 矩阵、objects 与 5 GiB 限制）、`content/docs/auth/` 和
`content/docs/ai-gateway/`。本仓库对应 `api/internal/control/functions_public.go`、
`object_storage.go`、`sql_history*.go`、`services/functions/runtime/server.mjs`、
`services/auth/`、`api/internal/control/ai*.go`、授权/生命周期/调谐代码与 Chart。
基线中的 Functions 网关曾明确返回 426；当前 F01 基础已单独交付，Object Storage 已实现有界 multipart，响应仍明确列出 large_objects/physical_gc 等 missing；
没有把文档目标推断为现有功能。历史通过记录以对应 delivery 的源码与镜像为界。

| ID | 需求 | 基线状态 / 缺口 | 下一验收闭环 |
|---|---|---|---|
| F01 | Functions WebSocket | 标准握手、子协议、文本/二进制、双向关闭、活动阻止休眠、原代码冷醒和实际 UI 基础已验收 | [合同](FUNCTION-WEBSOCKETS.md)及[交付](DELIVERY-2026-10-18-FUNCTION-WEBSOCKETS.md)；商业 SDK/Hono 适配、完整计量和独立 edge 为扩展边界 |
| F02 | Functions cron 触发器 | UTC 五字段 Cron、持久计划/回执、租约/去重、有界重试、UI 与真实 PostgreSQL 正反例已实现；原 Function/计划从零冷醒到真实 SQL、伪造拒绝和正常退休的本地基础验收通过 | [合同](FUNCTION-SCHEDULES.md)及[精确交付](DELIVERY-2026-10-17-FUNCTION-SCHEDULES.md)；分支继承、完整 HA/外部 fencing 分别归 F05/H01/X01，不自动关闭 |
| F03 | 对象事件触发 Functions | 原生事务 outbox、签名分支绑定、桶/前缀过滤、自动重试、不可变失败回执和显式重投递、SQL 去重与实际 UI 基础通过 | [合同](FUNCTION-OBJECT-EVENTS.md)及[交付](DELIVERY-2026-10-18-FUNCTION-OBJECT-EVENTS.md)；继承、完整计费/事件保留物理清理仍单列 |
| F04 | Functions stdout/stderr | 明确授权、已知值替换、四级输出、scope/实例/部署/boot 归属、密文分页/三天保留、同次停止尾日志及实际 UI 基础通过 | [合同](FUNCTION-OUTPUT.md)及[交付](DELIVERY-2026-10-18-FUNCTION-OUTPUT.md)；任意秘密识别、未发布失败启动和外部日志平台不由本片代替 |
| F05 | Functions 分支复制及恢复 | 同项目显式复制、不可变来源/环境重封装、新 SQL 身份、无 Writer 延迟冻结继承、默认禁用触发器、历史 SQL 当前代码确认、实际隔离及原失败操作恢复基础已验收 | [合同](FUNCTION-BRANCHES.md)及[交付](DELIVERY-2026-10-19-FUNCTION-BRANCHES.md)；跨服务一致时间点归 L03，物理去重/跨项目共享另列 |
| F06 | Functions 完整计量与独立 edge | 未完成；当前 public listener 同 API 进程 | 最小权限进程、实际资源/调用计量、长流与部署切换 |
| S01 | S3 multipart | 有界基础已实现并验收：七类 S3 操作、身份隔离、原子发布/回执、UI 续传、中止、SDK 字节、目录分组分页、已撤销身份管理员中止/审计恢复；仍限 8 MiB | [当前交付](DELIVERY-2026-10-16-CURRENT.md)与[运维合同](MULTIPART-ADMINISTRATION.md)；完整 checksum/大对象、GC/自动回收和写入权转让仍有独立扩展门槛 |
| S02 | 大对象与完整 checksum | 64 MiB 有界流式、Range/基础 checksum、UI 已验收；官网快照 5 GiB 仍未满足 | 5 GiB 和完整扩展 checksum 等继续，见 [交付](DELIVERY-2026-10-22-STORAGE-LARGE-OBJECTS.md) |
| S03 | 引用安全物理 GC | 未开发；删除只移除分支清单 | 分支/PITR/保留窗口引用证明、隔离清扫、失败恢复；不得删除历史业务字节 |
| S04 | S3 兼容扩展 | 固定官网矩阵 41 项操作及预签名/POST/trailers、公开读阻断、MinIO/Boto3 基线已通过；版本/生命周期/Object Lock 仅存储 | [矩阵与回执](DELIVERY-2026-10-23-S3-CAPABILITIES.md)；5 GiB/完整 checksum、安全 GC、独立入口分别归 S02/S03/S05；原账本的 STS 不在固定支持矩阵中，不提供 |
| S05 | 独立 Storage gateway / 可信计量 | 当前 S3 同 API 进程；last_used_at 不等于计费 | 最小权限独立部署、配额/物理容量、租户并发与用量 |
| A01 | Console MFA | 基础自助已完成：标准 TOTP、恢复码、密码/重放、启用/关闭的其他会话撤销及 UI/PG 已验收 | [当前交付](DELIVERY-2026-10-16-CURRENT.md)；组织强制/WebAuthn/正式恢复流程仍是扩展门槛 |
| A02 | Console OIDC/SSO | 固定本地协议、显式绑定、组织策略、本地 MFA 与 UI 基础闭环 | [合同](CONSOLE-SSO.md)与[验收](DELIVERY-2026-10-24-CONSOLE-SSO.md)；跨 authority 需代码扩展，商业 IdP 接入未认证，SAML/SCIM/WebAuthn 未提供 |
| A03 | 应用 Auth OAuth/Admin/组织/手机号/事件 | 固定本地 OAuth、手机、七类签名事件、Admin 和组织基线已验收；原邮件/密码/OTP/Magic/TOTP 回执复用 | [合同](AUTH-INTEGRATIONS.md)与[交付](DELIVERY-2026-10-24-AUTH-INTEGRATIONS.md)；商业接入、A05 生命周期与 T01 可信 TLS 单列，原无状态 JWT 五分钟边界保留 |
| A04 | Auth 分支第二因素 | 显式物理迁移、独立重封装、会话/绑定清除、恢复码隔离和验证器恢复基础已验收 | [交付](DELIVERY-2026-10-22-AUTH-FACTOR-MIGRATION.md)；生命周期恢复与在线根密钥轮换保留独立门槛 |
| A05 | Auth 保留删除/恢复/隔离 | 叶分支删除、项目保留恢复、显式新代次、原会话/JWKS 失效及账号/密码/SQL 保留基线已验收 | [矩阵](AUTH-LIFECYCLE.md)与[交付](DELIVERY-2026-10-25-AUTH-LIFECYCLE.md)；单分支恢复 API 未提供，物理 GC、KMS 根轮换、完整 Backend 时间点恢复另列 |
| C01 | 数据库/角色 rename | 自定义受管理非系统对象的异步 API/UI、原 OID/SCRAM/数据、所有现存配置/Proxy CAS、未知结果恢复和 Writer/原 Reader 冷醒基础通过 | [合同](CATALOG-RENAME.md)与[交付](DELIVERY-2026-10-17-CATALOG-RENAME.md)；默认身份、Backend 依赖迁移和未知外部 DDL 归 C02 |
| C02 | 默认/系统身份与外部 DDL | 默认连接/Owner、目录原结果、冷目录保留及普通 CRUD 持续 OID/凭据代际基础通过；整组未完成 | [0.70.0 交付](DELIVERY-2026-10-21-CATALOG-CURRENT-OBSERVATION.md)补齐有原证明的旧身份衔接、继承凭据背书及 Console；当前身份展示和首次启动继承沿用原验收；默认/系统漂移和 Backend 名称依赖继续 |
| L01 | 失败创建清理、异常运行时回收与 TTL | 客户 Node 退出后的认证 idle 回收，以及独立 Compute、项目/分支唯一原失败创建的身份绑定清理、原失败/timeline 保留和重放已实现并验收；分支 TTL 设置/更新/取消、持久重放、自动退休及原数据/父分支保留基础已验收；节点分区后置 | [失败父资源交付](DELIVERY-2026-10-18-FAILED-PARENT-CLEANUP.md)、[失败 Compute 交付](DELIVERY-2026-10-18-FAILED-ENDPOINT-CLEANUP.md)、[异常 idle 交付](DELIVERY-2026-10-18-FUNCTION-UNHEALTHY-IDLE.md)；[TTL 交付](DELIVERY-2026-10-18-BRANCH-EXPIRATION.md)；后续节点分区/外部 fencing 归 X01，按用户要求后置 |
| L02 | 原分支 reset / 原地 PITR | 未开发；恢复到新分支已有 | 备份分支、连接切换、GC/连接竞争、失败回退与 UI |
| L03 | Time Travel Assist / Backend 一致恢复 | 未开发 | 查询时间点辅助、Auth/对象/函数分别描述恢复语义并校验 |
| Q01 | 个人 SQL 历史保留/删除 | 采集开关、0/7/30/90 天保留、到期清理和单条手工删除的隔离/重试/审计/UI/PG 基础闭环已完成 | [当前交付](DELIVERY-2026-10-16-CURRENT.md)；未知结果仍保护，人工定性/合规流程另验 |
| Q02 | 项目共享 SQL 历史 | 基础已实现并验收：逐条确认、密文引用、撤回、当前作者/读者授权、删除/到期失效、分页与多账号 UI | [共享合同](SHARED-SQL-HISTORY.md)与[当前交付](DELIVERY-2026-10-16-CURRENT.md)；不含外部公开链接/已复制内容召回 |
| Q03 | SQL AI、计划比较/建议 | 计划比较、受控 Schema/SQL AI 草稿、真实本地模型与人工执行、计划建议通路已验收；AI 修错及个人加密名称/显式 AI 命名也已验收；默认自动命名/长期授权和建议准确性仍有扩展门槛 | [比较合同](SQL-PLAN-COMPARISON.md)、[助手合同](SQL-ASSISTANT.md)；模型建议准确性独立，[修错合同](SQL-AI-REPAIR.md)、[命名合同](SQL-HISTORY-NAMES.md)；默认自动授权/全自动命名另验 |
| I01 | AI 动态供应商配置 UI | 明确 operator、密文/固定 UID 来源、原幂等操作、受限模型发现、显式别名发布、失败更新保留旧版、有效凭据更新、停用及实际 Console/本地模型基础已验收 | [合同](AI-PROVIDERS.md)及[交付](DELIVERY-2026-10-20-AI-PROVIDERS.md)；商业供应商正式输入、其他协议/预算和生产 TLS 独立继续 |
| I02 | AI 原生协议/预算/持久状态 | 原生协议、预算/取消/对账、供应商发现、组织额度/BYOK、分支文件和持久响应/会话基线已验收 | [持久状态交付](DELIVERY-2026-10-25-AI-PROVIDER-STATE.md)；外部账单/支付仍与 O03 继续，不冒充商业供应商验收 |
| R01 | 严格 Guest 小数 CPU | QEMU 实际 250/500m 已验收；四倍预算保留 | Guest 隔离、公平性/计量；公开修复 runner 及统一锁待发行 |
| R02 | 完整物理内存归还 | VM status 缩回已有；物理 RAM/OOM 未通过 | Guest/宿主联合采样、压力与 SQL/coldboot；硬件压力后置单列 |
| X01 | 跨实例缩零栅栏 | 缺完整短连接 ledger / 外部 epoch fence | 多 API/Proxy、旧 Worker、分区、新连接/删除竞争；不能用 DB 租约代替 |
| H01 | 软件 HA | API/Worker 已分进程但单副本；元数据/数据面单域 | 多副本、领导切换、旧实例拒绝、业务连续性；物理独立另验 |
| H02 | 全栈 DR | 未完成演练 | 元数据/Secrets/对象/WAL 独立恢复、真实 SQL、实测 RPO/RTO |
| T01 | 全链路可信 TLS | Auth→Proxy 有正反例；Functions 原实例管理叶自动续期已完成 TLS/PG/真实 Console/SQL 基础闭环；浏览器/元数据/Storage 等仍有例外 | [续期合同](FUNCTION-MANAGER-IDENTITY.md)及[交付](DELIVERY-2026-10-19-FUNCTION-MANAGER-IDENTITY.md)；根 CA 轮换、全链路本地 CA/IP SAN/verify-full 与网络策略继续，公网输入后置 |
| O01 | 长期监控/告警/审计/预算 | UI 瞬时/基础指标已有；长期运营不全 | TSDB 保留、SLO/告警闭环、容量/错误预算和审计导出 |
| O02 | 供应链消费策略与统一发行 | digest/source 锁已有；最新本地镜像尚未统一公开 | 同源发布、签名/SBOM/provenance 验证、版本锁/回滚和独立安装 |
| O03 | 官网运营集成 | 自托管服务计划/配额与 CLI/MCP 固定基线已验收；计费/充值仍缺 | [CLI/MCP](DELIVERY-2026-10-25-CONTROL-CLIENTS.md)及[服务计划](DELIVERY-2026-10-25-SERVICE-PLANS.md)；账单接口继续，外部支付/正式运营输入另列 |

## 3. 实施和记录原则

优先缺代码且不依赖人工输入的闭环。本批已完成 A01 基础闭环及 Q01 的个人策略/到期清理/单条删除闭环；Q02 基础共享与 Q03 的计划比较、受控 SQL AI、修错和显式命名均已有验收；F02 UTC 定时触发器的本地基础闭环现已通过；C01 重命名基础也已验收；L01 客户 Node 异常后的认证 idle 自动退休切片已通过，继续失败创建清理、分支 TTL，再继续 F/S/生命周期
和分布式基础设施工作。独立失败 Compute、项目/分支创建清理切片现已通过；TTL 继续实施。清单不是完成证明，不以“已有页面”替代实际服务。
同集群串行验证，只重跑新增功能和具体受影响风险。Linux 构建与运行；保留原始
fixture、失败、日志、密码保护输入和所有业务字节。测试完成只退休确认为自有的
临时 Job/VM/Runner，记录 UID；Compute 回到 0。代码、API、模型、UI、部署和复测
文档同步，只有实际通过才关闭对应条目。

新增实现与验收追加到交付记录并更新本表，生产门槛保持独立。不把公网域名、商业
模型或物理故障域不足作为本地软件工程未完成的理由。

## 4. 当前读法

截至 2026-10-23，35 个需求组中有 14 组具备基础验收回执；21 组未完整闭环，其中 13 个业务组、运维/TLS/发行三组和五组后置专项。I02 协议/预算和原生供应商发现的增量已经验收，完整多模态、credit/账单/BYOK 等仍有开发缺口。按用户要求复用原回执，继续新增业务功能，性能/可靠性/HA/DR 后置。准确分组见[当前清单](FUNCTIONAL-STATUS-2026-10-21.md)，不以页面、部分切片或需求行数宣称全部完成。


2026-10-17 补充：F02 基础已验收，连同 A01/Q01/Q02/Q03/S01 基础，C01 基础也已验收，共七组具有对应基础回执；其余 28 组仍有开发或独立验收缺口。已验收基本组的扩展边界继续按实际合同保留，不用计数宣称官网全部能力完成。

2026-10-18：L01 分支 TTL 基础验收通过，迁移 042 / 0.52.0。下一业务组 F01。用户要求先完成全部业务功能，性能、可靠性、HA/DR、分区和外部 fencing 专项后置，必要构建、权限隔离和新增功能验收继续。

2026-10-18：F01 WebSocket 基础已通过，同一原函数在三个实际 UI Job 中累计完成七个唯一检查点及只读原冷醒证明。当前 0.53.0 / migration 042 / Chart 0.9.45 / Helm 118；八组具有基础回执，剩余 27 组仍有开发或独立验收缺口。继续 F03；性能、可靠性、HA/DR 与外部栅栏专项后置，功能必要校验继续。

2026-10-18：F03 对象上传触发基础已验收，0.54.0 / migration 043 / Chart 0.9.46 / Helm 121。九组有基础回执，剩余 26 组仍有代码或独立验收缺口，继续 F04 客户输出日志；后置性能和可靠性专项。


2026-10-18：F04 客户输出本地基础通过，0.55.0 / migration 044 / Chart 0.9.47 / Helm 124。十一组有基础回执，其余 24 组仍有代码或独立验收缺口，下一 F05 分支复制和恢复；按用户要求后置性能/可靠性专项。

2026-10-19：F05 分支复制与恢复基础通过，0.56.0 / migration 045 / Chart 0.9.48 / Helm 127。十二组有基础回执，其余 23 组仍有实现或独立验收缺口，下一处理 T01 中原实例管理证书续期功能；性能和可靠性专项仍后置。

2026-10-19：原实例短期管理证书自动续期功能通过，0.57.0 / migration 046 / Chart 0.9.49 / Helm 130；默认提前 2700 秒，原身份/数据保留并正常退休。T01 其他全链路 TLS 门槛仍未关闭，基础完成组数维持十二组，其余 23 组仍有实现或独立验收缺口；继续优先业务开发，性能/可靠性专项后置。

2026-10-20：I01 动态供应商基础闭环通过，0.58.0 / migration 047 / Chart 0.9.50 / Helm 133；五项真实 Console 检查、三次实际缓存模型推理、原失败 metadata 操作重试及最终数据保全通过。十三组有基础回执，其余 22 组仍有业务或独立门槛；继续 C02 默认身份与外部 DDL 业务，性能/可靠性专项后置。

2026-10-20：C02 客户连接默认身份基础闭环通过，0.59.1 / migration 048 / Chart 0.9.52 / Helm 135；九项真实检查及原数据保全通过。整组计数仍为 13 基础、22 继续；下一步持久 OID 与外部 DDL，性能/可靠性专项后置。

2026-10-20：C02 原目录身份与外部 DDL 观察基础通过，0.60.0 / migration 049 / Chart 0.9.53 / Helm 136；五个新 Go 顶层测试和十项真实检查通过。仍为 13 基础、22 继续；下一步受控原 OID 修复，性能/可靠性专项后置。

2026-10-20：C02 原目录身份受控恢复基础通过，0.61.0 / migration 050 / Chart 0.9.54 / Helm 137；四项新 Go 测试、八项真实检查及原 Writer/Reader 冷醒通过。仍为 13 基础、22 继续；性能/可靠性专项后置，继续持续身份绑定、默认 Owner 与 Backend 依赖迁移。

2026-10-21：C02 原默认数据库 Owner 与已知继承角色替换保留处理通过，0.63.0 / migration 052 / Chart 0.9.57 / Helm 140；原夹具累计八项真实检查、八项新 Go 顶层测试通过，原失败分支同一操作恢复，最终 VM 0。仍为 13 基础、22 继续；继续持续 OID/凭据代际及 Backend 名称依赖迁移，性能/可靠性专项后置。

2026-10-21：C02 普通目录原结果恢复通过，0.64.0 / migration 053 / Chart 0.9.58 / Helm 141；一个新增 Go 顶层测试的六类元数据动作子用例与五项真实检查通过。最终 VM 0，原数据保留；仍为 13 基础、22 继续，性能/可靠性专项后置。

2026-10-21：C02 rename 原结果恢复通过，API/Worker 0.65.0、Web 复用 0.64.0 / migration 054 / Chart 0.9.59 / Helm 142；角色/数据库两个元数据子用例及五项真实检查通过，原名字变化后的 OID/数据/最新密码保留，最终 VM 0。仍为 13 基础、22 继续；继续当前原生身份代际和 Backend 名称依赖迁移，性能/可靠性专项后置。

2026-10-21：C02 初始化后冷目录保留与仅目标角色原生管理通过，API/Worker/管理网关 0.66.3、Web 复用 0.64.0 / migration 055 / 控制面 Chart 0.9.63、Helm 146 / 管理网关 Chart 0.1.1、Helm 56。五个新增 Go 顶层测试、一个受影响既有拓扑测试及原夹具累计五项真实检查通过；132 份初始化发布一致，原失败操作同 ID 恢复，最终 VM 0。仍为 13 基础、22 继续；继续当前 OID/凭据代际与 Backend 名称依赖，性能/可靠性专项后置。

2026-10-21：C02 普通目录持续原生身份基础通过，API/Worker 0.67.0 / migration 056 / Chart 0.9.64 / Helm 147；四个新增 Go 功能测试、两个受影响原结果测试及累计五项现场检查通过。18 条身份代际、9 份 CREATE OID、15 份普通及 2 份 rename 原结果一致，8 个替换拒绝场景通过，最终 VM 0。仍为 13 基础、22 继续；继续分支继承、旧身份链衔接、系统/Backend 依赖，性能/可靠性专项后置。

2026-10-21：C02 首次启动保留继承目录基础通过，API/Worker 0.68.0 / migration 057 / Chart 0.9.65 / Helm 148；8 个新增及 9 个受影响 Go 顶层测试、累计六项现场检查通过。5 份来源、4 份原观察/发布及 136 份初始化回执一致，原数据和既有身份保留，最终 VM 0。仍为 13 基础、22 继续；继续当前身份展示、旧身份链及系统/Backend 依赖，性能/可靠性专项后置。

2026-10-21：C02 当前目录身份展示通过，API/Worker 0.69.0 / migration 057 保持 / Chart 0.9.66 / Helm 149；两项新增及五项受影响 Go 顶层测试、五项现场检查和 16 张目录表前后摘要一致。原夹具/身份/数据保留，最终 VM 0。仍为 13 基础、22 继续；继续旧身份链、系统/Backend 依赖，性能/可靠性专项后置。

2026-10-21：[旧目录身份衔接](DELIVERY-2026-10-21-CATALOG-CURRENT-OBSERVATION.md)通过继承原凭据背书、原 OID 到当前代际的显式观察、控制台、改名/改密、原回执重放与冷目录数据保留。API/Worker 0.70.0 / migration 058 / Chart 0.9.67 / Helm 150；仍为 13 基础、22 继续，C02 默认/系统漂移及 Backend 名称依赖继续，性能/可靠性后置。

2026-10-22：[2026-10-22 有界大对象、Range 与 checksum](DELIVERY-2026-10-22-STORAGE-LARGE-OBJECTS.md)通过新增有界大对象功能；需求组数量保持 13 基础、22 继续，其中五组专项后置。

2026-10-22：A04 基础闭环通过，14 组有基础回执、21 组继续；后者含 13 个业务组、运维/TLS/发行三组及五组后置专项。性能/可靠性专项继续后置。

2026-10-22 AI 增量已通过，完整 I02 仍开放；需求组计数以[当前清单](FUNCTIONAL-STATUS-2026-10-21.md)为准：14 组基础通过、13 个业务组未完整闭环、另三组及五组后置专项。

2026-10-23：[2026-10-23 原生供应商发现与发布](DELIVERY-2026-10-23-AI-NATIVE-PROVIDERS.md)：Anthropic/Gemini 原生目录、分页、不可变类型、匹配协议发布、Console 调用及跨类型凭据隔离通过。5 组新增 Go/PG、1 组受影响检查、累计 8 个实机检查点通过；原协议/预算与真实 Ollama 回执复用。API/Worker/AI 0.74.0、Web 重建，schema 061 / Chart 0.9.71 / Core Helm 158；Auth 和 Gateway 保留。17 张目录表、9 PVC、916 个原 Secret 身份保留，最终三节点、三个 Safekeeper Ready，VM 0。完整 I02 及其余业务组继续，性能/可靠性专项后置。

2026-10-23 组织额度增量：[2026-10-23 组织 AI 额度](DELIVERY-2026-10-23-AI-ORGANIZATION-CREDITS.md)：手工授信/更正、原键恢复、双账本原子准入/结算、未知用量保留及凭证对账通过。9 组新增 Go/PG、3 组受影响检查、8 个实际页面检查通过，原功能回执复用。API/Worker/AI 0.75.0、Web 重建，schema 062 / Chart 0.9.72 / Core Helm 160；Auth 和 Gateway 保留。17 张目录表、9 PVC、916 个原 Secret 身份保留，最终三节点、三个 Safekeeper Ready，VM 0。合成授信未接入真实支付；BYOK、完整多模态及其余业务继续，性能/可靠性专项后置。

2026-10-23 BYOK 增量：[2026-10-23 组织自有 AI 供应商](DELIVERY-2026-10-23-AI-ORGANIZATION-BYOK.md)：组织密钥/模型隔离、发现发布、原凭据轮换和费用归属通过，BYOK 不扣平台额度并继续受分支预算约束。5 组新增 Go/PG、3 组受影响检查、累计 7 个实际页面检查通过；已完成步骤从原检查点续验。API/Worker/AI 0.76.0、Web 重建，schema 063 / Chart 0.9.73 / Core Helm 162。17 张目录表、9 PVC、916 个原 Secret 身份保留，最终三节点和三个 Safekeeper Ready，VM 0。完整多模态/持久引用、外部账单/支付及其他业务继续，性能/可靠性专项后置。

2026-10-23 S3 矩阵收尾：[2026-10-23 S3 兼容矩阵与双 SDK](DELIVERY-2026-10-23-S3-CAPABILITIES.md)：固定需求矩阵 41 项操作逐项关联既有/新增回执；27 个授权/加密写入负例、18 个未支持子资源及 Boto3 互通通过，元数据响应键大小写已修复。3 组新增 Go、3 组受影响检查和 15 个累计实机检查点通过，原上传未重做。API/Worker 0.80.0，Web/AI/Auth/Gateway 复用，schema 064 / 原生格式 4 / Chart 0.9.77 / Core Helm 169。17 张目录表、6 类旧存储清单、9 PVC、917 个原 Secret 和其他六套 Helm 配置保留，最终三节点、三个 Safekeeper Ready，VM 0。S04 固定矩阵基线闭环，当前 15 组基础有回执，12 个业务和 3 个其他组继续；5 GiB/完整 checksum、安全 GC、独立网关及性能/可靠性专项分别保留。

2026-10-24 SSO：[Console OIDC 与组织策略](DELIVERY-2026-10-24-CONSOLE-SSO.md)完成固定本地基线；4 组新增 Go、4 组受影响检查、2 组 Helm 和 10 个累计浏览器检查点通过。原键恢复、显式绑定、强制策略、本地 MFA、恢复码、会话失效及 UI 已验收。Core Helm 171 / schema 065；API/Worker 0.81.0，Web 修复镜像，其他组件复用。17 张目录表、9 PVC、917 个原 Secret 保留，三节点、三个 Safekeeper Ready，VM 0。当前 16 组基础有回执，11 个业务和 3 个其他组继续；跨 authority IdP、商业接入和性能/可靠性认证单列。

2026-10-24 应用 Admin：[管理员功能与实际页面](DELIVERY-2026-10-24-AUTH-ADMIN.md)通过 2 组新增/7 组受影响 Go、1 组新增/5 组受影响维护库 PostgreSQL 检查，以及 11 个累计实机检查点。显式授权、角色、封禁、会话、改密/删除及一小时身份代入闭环；原已过步骤及改密未重做。Core Helm 172 / schema 065 / Auth 协议 0.26.0；原目录和凭据身份保留，三节点、三个 Safekeeper Ready，VM 0。A03 的 OAuth、组织、手机号、事件继续；全项目仍有 11 个业务和 3 个其他组未整体闭环。

2026-10-24 应用组织：[组织、成员与邀请](DELIVERY-2026-10-24-AUTH-ORGANIZATIONS.md)完成 22 个接口和控制台操作；2 组新增/7 组受影响 Go、3 组新增维护库 PostgreSQL、1 组受影响 Admin 及 9 个累计实机检查点通过。配额和最后管理者原子保护、验证收件人、当前 JWT 及复制分支邀请隔离通过；复用原创建 Operation 完成续验。Core Helm 174 / schema 065 / 应用协议 0.27.0，原目录与凭据身份保留，最终三节点、三个 Safekeeper Ready，VM 0。A03 的 OAuth、手机号和事件继续，整体仍有 11 个业务和 3 个其他组未闭环。

2026-10-24 应用 Auth 高级功能：[OAuth、手机和签名事件](DELIVERY-2026-10-24-AUTH-INTEGRATIONS.md)完成固定本地 A03 基线。25 个唯一维护库综合组、2 组新增/6 组受影响 Go、2 组协议和十个累计浏览器检查点通过；已过检查从原进度续验。Core Helm 178 / schema 065 / 应用协议 0.28.0；原目录、PVC 和 Secret 身份保留，三节点、三个 Safekeeper Ready，VM 0。当前 17 组基础有回执，10 个业务和 3 个其他组继续；商业 OAuth/短信接入、可信 TLS 与后置专项未冒充已过。

2026-10-25 Auth 生命周期：[保留删除、恢复与身份一致性](DELIVERY-2026-10-25-AUTH-LIFECYCLE.md)通过两组新增 Go 和八个累计实机检查点；前四项与原 Cookie 保留后只续验余项。恢复后 Auth 保持关闭，显式新代次拒绝旧会话/JWKS 并保留原账号、密码和 SQL 数据。复用 0.84 镜像，Core Helm 178 / schema 065 不变；原目录、945 个续验前持久 Secret UID 和 9 PVC 保留，三节点、三个 Safekeeper Ready，VM 0。A05 保留生命周期基线完成；当前 18 组基础、9 个业务和 3 个其他组继续。

2026-10-25 AI 持久状态：[供应商响应与会话](DELIVERY-2026-10-25-AI-PROVIDER-STATE.md)通过 15 组新增 Go/PG、4 组受影响检查和 6 个累计实机检查点；复用前三项和原已完成 SSE，未重复推理。原生 JSON/SSE、响应续接、会话/条目、不可变加密引用及未知结果核对通过。API/Worker/AI 0.85.0，Web 重建，schema 066 / Chart 0.9.82 / Core Helm 180；原 17 张目录表、946 个持久 Secret UID、9 PVC 和另外六套发行配置保留，最终三节点/三个 Safekeeper Ready，VM 0。临时配置已撤下、合成服务归档后缩零。I02 外部账单/支付及其他业务继续；性能、可靠性后置。

2026-10-25 服务计划：[自托管服务计划与组织配额](DELIVERY-2026-10-25-SERVICE-PLANS.md)通过五组新增 Go/PG、三组受影响 OpenAPI、三组 Helm 和九个累计实机检查点；复用前五项，仅续验余项。不可变修订、原回执、真实准入/超额拒绝、降级原子性、禁用和恢复原配额通过。API/Worker 0.86.0，Web 重建，schema 067 / Chart 0.9.83 / Core Helm 182；原目录、36 个旧组织配额、947 个持久 Secret UID、9 PVC 和另外六套发行配置保留，最终三节点/三个 Safekeeper Ready，VM 0。临时平台审批已撤回。CLI/MCP、账单接口及其余业务继续；大对象/高容量验证按用户要求后置并单列实现缺口。

2026-10-25 CLI/MCP：[CLI 与官方 MCP SDK 双协议基线](DELIVERY-2026-10-25-CONTROL-CLIENTS.md)通过 15 组新增 Linux 功能检查和七个累计真实接口检查点；两组受影响路径复验，原 Go/Web/Helm 功能不重测。创建/读取/版本更新、原键回执、冲突/跨项目拒绝及撤销后即时拒绝通过；未执行 SQL 或启动 Compute，原保存查询摘要保持一致。离线运行包复用缓存，两个终态 QA Job 已归档退休。服务端仍为 API 0.86.0 / schema 067 / Core Helm 182；O03 账单接口与外部支付继续，容量/性能验证按既定边界后置。
