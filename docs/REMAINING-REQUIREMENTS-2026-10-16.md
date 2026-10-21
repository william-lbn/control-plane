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
| S02 | 大对象与完整 checksum | 当前单对象 8 MiB；官网快照描述 5 GiB | 有界内存流式 I/O、Range/checksum、断线/配额和大文件 UI |
| S03 | 引用安全物理 GC | 未开发；删除只移除分支清单 | 分支/PITR/保留窗口引用证明、隔离清扫、失败恢复；不得删除历史业务字节 |
| S04 | S3 兼容扩展 | anonymous public read、trailers、STS 等未完成 | 按官网支持矩阵分别实现；每项签名/负例/真实 SDK 验证 |
| S05 | 独立 Storage gateway / 可信计量 | 当前 S3 同 API 进程；last_used_at 不等于计费 | 最小权限独立部署、配额/物理容量、租户并发与用量 |
| A01 | Console MFA | 基础自助已完成：标准 TOTP、恢复码、密码/重放、启用/关闭的其他会话撤销及 UI/PG 已验收 | [当前交付](DELIVERY-2026-10-16-CURRENT.md)；组织强制/WebAuthn/正式恢复流程仍是扩展门槛 |
| A02 | Console OIDC/SSO | 未开发；外部 IdP 可后置，本地协议可先做 | state/nonce/PKCE、绑定、组织策略、本地 IdP 与 UI |
| A03 | 应用 Auth OAuth/Admin/组织/手机号/事件 | 邮件密码/OTP/Magic/TOTP 已有；其余非完整官网能力 | 标准插件、配置隔离、用户管理、webhooks、真实本地服务和 UI |
| A04 | Auth 分支 factor 重绑定 | 未完成；克隆账户不能等同复制可信第二因素 | secret 重封装/重绑、会话清除、恢复与用户可见状态 |
| A05 | Auth 完整删除/恢复/隔离 | 独立门槛 | 原账号/会话/JWKS/密钥与分支生命周期一致性矩阵 |
| C01 | 数据库/角色 rename | 自定义受管理非系统对象的异步 API/UI、原 OID/SCRAM/数据、所有现存配置/Proxy CAS、未知结果恢复和 Writer/原 Reader 冷醒基础通过 | [合同](CATALOG-RENAME.md)与[交付](DELIVERY-2026-10-17-CATALOG-RENAME.md)；默认身份、Backend 依赖迁移和未知外部 DDL 归 C02 |
| C02 | 默认/系统身份与外部 DDL | 默认连接/Owner、目录原结果、冷目录保留及普通 CRUD 持续 OID/凭据代际基础通过；整组未完成 | [0.70.0 交付](DELIVERY-2026-10-21-CATALOG-CURRENT-OBSERVATION.md)补齐有原证明的旧身份衔接、继承凭据背书及 Console；当前身份展示和首次启动继承沿用原验收；默认/系统漂移和 Backend 名称依赖继续 |
| L01 | 失败创建清理、异常运行时回收与 TTL | 客户 Node 退出后的认证 idle 回收，以及独立 Compute、项目/分支唯一原失败创建的身份绑定清理、原失败/timeline 保留和重放已实现并验收；分支 TTL 设置/更新/取消、持久重放、自动退休及原数据/父分支保留基础已验收；节点分区后置 | [失败父资源交付](DELIVERY-2026-10-18-FAILED-PARENT-CLEANUP.md)、[失败 Compute 交付](DELIVERY-2026-10-18-FAILED-ENDPOINT-CLEANUP.md)、[异常 idle 交付](DELIVERY-2026-10-18-FUNCTION-UNHEALTHY-IDLE.md)；[TTL 交付](DELIVERY-2026-10-18-BRANCH-EXPIRATION.md)；后续节点分区/外部 fencing 归 X01，按用户要求后置 |
| L02 | 原分支 reset / 原地 PITR | 未开发；恢复到新分支已有 | 备份分支、连接切换、GC/连接竞争、失败回退与 UI |
| L03 | Time Travel Assist / Backend 一致恢复 | 未开发 | 查询时间点辅助、Auth/对象/函数分别描述恢复语义并校验 |
| Q01 | 个人 SQL 历史保留/删除 | 采集开关、0/7/30/90 天保留、到期清理和单条手工删除的隔离/重试/审计/UI/PG 基础闭环已完成 | [当前交付](DELIVERY-2026-10-16-CURRENT.md)；未知结果仍保护，人工定性/合规流程另验 |
| Q02 | 项目共享 SQL 历史 | 基础已实现并验收：逐条确认、密文引用、撤回、当前作者/读者授权、删除/到期失效、分页与多账号 UI | [共享合同](SHARED-SQL-HISTORY.md)与[当前交付](DELIVERY-2026-10-16-CURRENT.md)；不含外部公开链接/已复制内容召回 |
| Q03 | SQL AI、计划比较/建议 | 计划比较、受控 Schema/SQL AI 草稿、真实本地模型与人工执行、计划建议通路已验收；AI 修错及个人加密名称/显式 AI 命名也已验收；默认自动命名/长期授权和建议准确性仍有扩展门槛 | [比较合同](SQL-PLAN-COMPARISON.md)、[助手合同](SQL-ASSISTANT.md)；模型建议准确性独立，[修错合同](SQL-AI-REPAIR.md)、[命名合同](SQL-HISTORY-NAMES.md)；默认自动授权/全自动命名另验 |
| I01 | AI 动态供应商配置 UI | 明确 operator、密文/固定 UID 来源、原幂等操作、受限模型发现、显式别名发布、失败更新保留旧版、有效凭据更新、停用及实际 Console/本地模型基础已验收 | [合同](AI-PROVIDERS.md)及[交付](DELIVERY-2026-10-20-AI-PROVIDERS.md)；商业供应商正式输入、其他协议/预算和生产 TLS 独立继续 |
| I02 | AI 更多协议/预算 | 仅文本 Chat Completions；Responses/Messages/Gemini 等不全 | 按协议实现真实流式、用量/预留/预算、取消结算；商业凭据可后置 |
| R01 | 严格 Guest 小数 CPU | QEMU 实际 250/500m 已验收；四倍预算保留 | Guest 隔离、公平性/计量；公开修复 runner 及统一锁待发行 |
| R02 | 完整物理内存归还 | VM status 缩回已有；物理 RAM/OOM 未通过 | Guest/宿主联合采样、压力与 SQL/coldboot；硬件压力后置单列 |
| X01 | 跨实例缩零栅栏 | 缺完整短连接 ledger / 外部 epoch fence | 多 API/Proxy、旧 Worker、分区、新连接/删除竞争；不能用 DB 租约代替 |
| H01 | 软件 HA | API/Worker 已分进程但单副本；元数据/数据面单域 | 多副本、领导切换、旧实例拒绝、业务连续性；物理独立另验 |
| H02 | 全栈 DR | 未完成演练 | 元数据/Secrets/对象/WAL 独立恢复、真实 SQL、实测 RPO/RTO |
| T01 | 全链路可信 TLS | Auth→Proxy 有正反例；Functions 原实例管理叶自动续期已完成 TLS/PG/真实 Console/SQL 基础闭环；浏览器/元数据/Storage 等仍有例外 | [续期合同](FUNCTION-MANAGER-IDENTITY.md)及[交付](DELIVERY-2026-10-19-FUNCTION-MANAGER-IDENTITY.md)；根 CA 轮换、全链路本地 CA/IP SAN/verify-full 与网络策略继续，公网输入后置 |
| O01 | 长期监控/告警/审计/预算 | UI 瞬时/基础指标已有；长期运营不全 | TSDB 保留、SLO/告警闭环、容量/错误预算和审计导出 |
| O02 | 供应链消费策略与统一发行 | digest/source 锁已有；最新本地镜像尚未统一公开 | 同源发布、签名/SBOM/provenance 验证、版本锁/回滚和独立安装 |
| O03 | 官网运营集成 | 计费/充值、服务计划、CLI/MCP 全覆盖非当前完整实现 | 先固定自托管 API/配额合同；外部支付/正式运营输入另列 |

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

截至 2026-10-21，35 个需求组中有 13 组具备本地基础验收回执：F01–F05、S01、A01、C01、L01、Q01–Q03、I01；另 22 组未整体闭环。按用户最新优先级，R01/R02/X01/H01/H02 五组性能、资源压力、外部栅栏、HA/DR 专项后置；当前仍有 17 组未闭环，其中 14 组业务功能，以及运维 O01、TLS T01、发行 O02 各一组。既有未测试项，也有未实现项，不能称为即将全部结束。准确分组、通过记录与功能优先顺序见 [当前功能验收与剩余清单](FUNCTIONAL-STATUS-2026-10-21.md)。已验收组的扩展、商业 SDK 全覆盖及后置专项不因此自动关闭；R01 的小数 CPU 切片虽已通过，完整隔离/计量和发行仍有缺口，继续列在待完成组内。不能把已有页面、部分切片或需求行数表述成全部功能完成。


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
