# 生产门槛与官网对齐

当前增量：[2026-10-22 有界大对象、Range 与 checksum](DELIVERY-2026-10-22-STORAGE-LARGE-OBJECTS.md)：单对象 64 MiB、分片 8 MiB、分支可见对象 256 MiB；私有文件缓冲、浏览器分块哈希、17 MiB 真实 UI/SDK 上传和分片合并、Range/HEAD/SHA-256/CRC32C 拒绝路径通过。21 项 Go、3 项页面哈希检查通过，原目录摘要保留。API/Worker 0.71.0，Web 重建，schema 058 不变 / Chart 0.9.68 / Core Helm 152；Gateway 0.66.3 / Helm 56、AI 0.58.0 保留，最终 VM 0。S02 的 5 GiB 和完整扩展 checksum 仍有缺口，性能/可靠性专项后置。

状态截至 2026-10-17；API/Worker、Web 0.48.0 / migration 039 / Chart 0.9.40 / Helm revision 112。Console MFA、个人 SQL 历史采集/保留策略和单条删除已有 Linux Go/PG 与真实 UI 回执，见[当前交付](DELIVERY-2026-10-16-CURRENT.md)。[剩余需求账本](REMAINING-REQUIREMENTS-2026-10-16.md)区分未开发、部分实现和独立验收门槛。运行镜像按本地快照留证；公开统一 Helm 0.1.31 源码候选仍锁 0.23.0 公共控制镜像，不能冒充本地 0.48.0 发行。
前一版本 `be04cb4` 的真实 Linux/React 闭环见 [Auth/S3 交付](DELIVERY-2026-10-14-AUTH-S3.md)：
788 Go/真实 PostgreSQL 检查、八个同源公开镜像、三节点 24 次串行 digest 导入；
应用密码/会话十组 UI，以及实际 S3 SDK 签名/字节/权限/轮换/撤销/正常缩零均通过。
[Functions HTTP/SSE 与自动零/一次冷醒](DELIVERY-2026-10-14-FUNCTIONS-HTTP.md)
在 0.1.28 有自己的十组 UI 回执。当前仍是预览产品；这些切片不关闭下列生产门槛。
历史失败、版本和原 fixture 保留，未重复不相关的旧全量 UI 套件。
本地 OTP/Magic Link/S3 目录三个新 UI 闭环已通过；本批综合 Go/PG 回归 793 pass / 0 fail / 0 skip。
需运营方输入见 [OPERATOR-INPUTS.md](OPERATOR-INPUTS.md)，不阻塞其他本地工程。

本地邮件链接验证/密码找回已通过十组 UI、真实 SMTP、790 Go/PG 与最终八发布审计，见 [精确回执](DELIVERY-2026-10-14-AUTH-MAIL.md)。本地 OTP 和应用 Auth OpenAPI 已通过八组真实 UI，见 [OTP 回执](DELIVERY-2026-10-14-AUTH-OTP.md)。Magic Link 已通过八组真实 UI，见 [本地交付](DELIVERY-2026-10-14-LOCAL-MAGIC-LINK.md)；应用 TOTP 本地八项 UI 已通过；公网邮件、组织强制 MFA、OAuth 等仍按各自门槛推进；Console MFA 自助见当前交付。

## 1. 已实现路径

Functions 历史恢复在 0.1.25 通过原 fixture 的九组实际 UI 检查；SDK 空闲会话
断开后进程存活、明确新连接与保留写入已验证，见 [恢复合同](FUNCTIONS-ROLLBACK.md)
和 [SDK 可靠性](FUNCTIONS-SDK-RELIABILITY.md)。该历史回执不单独关闭公开访问和自动休眠；这两项已有 0.1.28 独立验收，仍不关闭
分布式栅栏、HA/DR 或完整日志/计量门槛。

原生项目/分支/Endpoint、一个 Writer/多个 Reader、Proxy SQL、
数据库/角色目录及轮换、组织权限与 Key scope、持久 Operation、
实验环境休眠/唤醒、整数 CPU 边界与 UI 监控均有运行代码。
历史实验环境 Linux PG/UI 证据保留在维护者工作区，未把私密记录上传公共 Git。

## 2. 独立放行门槛

| 门槛 | 当前缺口 | 完成判据 |
| --- | --- | --- |
| Proxy/Storage adapter | 原生 Go/Helm 迁移和真实 Proxy/UI 冷醒/通知已验收；仍单实例，通知重配置和分布式连接账本未完成 | 全新隔离环境安装、完整 placement/Safekeeper 重配置、跨实例长短连接与删除竞争另验 |
| 多租户生产安全 | 受邀注册/已有账号接受、一次凭据、撤销/过期、组织锁、事务审计及个人 Console 会话管理已有代码/真实 UI；Console MFA 自助已验收；OIDC/SSO、组织强制 MFA、公网邮件投递与完整攻击矩阵仍缺 | 新版本 Linux UI 与实际 PG 验收、跨组织/项目负例、撤销竞争、外部安全评审 |
| 跨实例缩零栅栏 | DB 租约无法 fence 所有外部动作；缺短连接 ledger | 多 API/Proxy 并发、过期 worker、新连接/删除竞争负例与无误删 |
| HA | 独立 API/Worker 的领导租约已有代码；两者仍单副本；Metadata/数据面单故障域实验部署 | 独立故障域、主从切换/脑裂矩阵、指标和业务连续性 |
| DR | 元数据/Secrets/对象/WAL 恢复未形成全栈演练 | 固定镜像与密钥、隔离 restore、真实 PG 数据校验、实测 RPO/RTO |
| PITR | 已实现按时间戳/LSN 恢复到新分支、原生保留窗口/租约与 UI；原地恢复、Time Travel Assist、完整 Backend 一致性恢复未实现 | 新分支路径按版本做真实 Linux UI 验收；另验原地切换、GC 竞争、服务一致性、长期保留与恢复 |
| 小数 CPU | 新建小数 Endpoint、实际 runner 配额 250→500→250m、SQL、冷醒及最终缩零已通过；保留四倍 QEMU 预算 | 严格 Guest 隔离、争用公平性及计量另验；正式公开镜像和统一 Helm 发行待完成 |
| 内存缩回 | 本批 VM status 1→2→1Gi 与 SQL 已通过，物理 RAM 归还和压力/OOM 仍未通过 | Guest 状态、压力/回收、OOM 防护、SQL 持续和冷醒完整循环 |
| 基础设施稳定性 | 同物理盘 VM 存在共同 I/O 竞争 | 分离故障域/IO、持续负载、etcd 延迟与错误预算 |
| TLS / 网络 | Auth→Proxy 已实现显式 CA/证书名称验证及真实 TLS 正反例；浏览器、控制库、Storage 等仍有实验传输例外 | 正确域名或 IP SAN、可信证书、verify-full、管理网络隔离及 NetworkPolicy；本地 CA 路径不依赖公网域名 |
| 目录完整性 | 自定义 API-owned 数据库 Owner 变更已通过实际 UI/SQL、Writer/Reader 冷醒、幂等与正常回收；自定义 rename 基础已通过；默认/系统 Owner、外部 DDL 回收、默认身份迁移未完成 | 按版本 UI/实际 PG 验证；扩展动作与完整身份迁移另设门槛 |
| 生命周期完整性 | 保护/依赖、保留删除、独立 Endpoint 退休/原密码重建、held tombstone、七天项目恢复已通过当前 UI；物理 GC、失败创建资源删除、TTL 和原分支 reset 仍缺 | 当前九套 UI 与原失败恢复分别留证；物理 GC、未知外部 DDL 自动恢复与分布式栅栏另设 Gate |
| 监控与运营 | 长期指标、告警、审计、容量/计量不足 | SLO、持久 TSDB、告警测试、预算与容量恢复 |
| 发布供应链 | 当前八镜像已有同源码公开 CI、匿名 OCI/source 验证及三节点 digest 拉取；长期签名/证明消费策略仍需治理 | 固定 SHA/digest、SBOM/provenance、匿名拉取、回滚镜像保留及独立消费验证 |

首个 e4fd1f3 发行三个镜像已由 Linux CI 发布，Linux 匿名 manifest/config 验证通过。
后续源码每次修改仍须取得本次提交的独立回执；不能沿用旧版本通过结果。

## 3. 官网 Backend 服务

Functions 已开始实际 Go/Node 基础层实现，合同和内部接口见
[FUNCTIONS-IMPLEMENTATION](FUNCTIONS-IMPLEMENTATION.md)。默认 guest 已有独立真实 VM
验收；代码包/环境存储和持久身份回执见 [FUNCTIONS-BACKING](FUNCTIONS-BACKING.md)。
原生 Driver/UI 已在 `bf0dd9e` / Helm 0.1.19 通过实际 Linux UI→microVM→Proxy→SQL 与手动停止/重部署/删除，运行监控在 `be557e0` / Helm 0.1.20 通过独立实际 UI；显式保留版本唤醒在 `0198f7c` / Helm 0.1.21 通过自己的现场回执，合同见 [FUNCTIONS-WAKE](FUNCTIONS-WAKE.md)。公开 HTTP/SSE、自动零/冷醒已在 0.1.28 独立验收；分支复制、WS、触发器和全部 Functions 产品能力仍未完成。

Managed Auth 已有 Go Driver、Better Auth 1.7.7 运行时、React UI、注册/会话/JWT/分支隔离与 PostgreSQL CI，详细见 [Auth 合同](MANAGED-AUTH.md)。真实 Neon UI 结果按版本交付报告记录；应用密码/会话安全已完成本地验收；公网 SMTP/OAuth、Console 组织强制 MFA、分支 factor 迁移、完整恢复/删除一致性、生产隔离与 HA 仍须独立实现/验收。

产品 Object Storage 已实现分支目录、独立受限对象凭据、Go REST Driver、React UI、条件写入、签名下载和分支继承，详见 [Object Storage 合同](OBJECT-STORAGE.md)。REST 是 `neon-object-rest-v1`；f0b63f5 Linux 现场 UI 19 项已通过，实际版本与其余回归见 [当前交付](DELIVERY-2026-10-12-SQL-IDENTITY.md)。基础 S3/SigV4/signed chunks/统一存储凭据已在 0.1.29 实际验收，见 [S3 合同](OBJECT-STORAGE-S3-CORE.md)。基础 COPY、条件批量删除、预签名 PUT、CORS 和用户 metadata 已通过本地 UI；8 MiB 有界 multipart/运维已在 0.42/0.43 验收；大文件、进阶校验、物理 GC、分布式网关隔离及 HA/DR 仍未完成。

Functions 已有 guest、不可变存储、持久候选/引导/SQL 身份及新增原生执行
Driver/API/UI，默认禁用，实际发布验收见 [执行合同](FUNCTIONS-NATIVE-PREVIEW.md)；AI Gateway 文本 Chat/SSE、目录、Playground、应用授权和用量已通过本地真实 Ollama UI；商业上游/其他协议/预算和生产门槛仍缺。Data API 已有原生 Driver、
异步生命周期、最小权限角色、PostgREST、UI 和 RLS 集成测试；默认禁用、仅实验传输开关。
现场 Neon UI 验收与生产 TLS/HA 是独立门槛，具体结果以该版本交付证据为准。
应用凭据管理及分支/模型授权检查已有代码和真实 PostgreSQL 验收；不代表 AI 推理可用。
控制台登录不能冒充 Managed Auth；Neon 持久层对象存储不能冒充产品 Object Storage；
SQL 工作台不能冒充 Data API。现有 fork Proxy 的 REST 构建依赖包含 stub，
不能作为完整官网数据服务的验收证据。

官网快照 `c0d49cbb…` 描述以 Branch 组织 Backend。
后续需要分别实现真实服务、分支 URL/密钥、隔离、配额/计量、版本、备份/恢复和 UI。
控制台受邀注册的实现和复测合同见 [CONSOLE-INVITATIONS.md](CONSOLE-INVITATIONS.md)。
恢复操作必须说明：恢复 PG/随 PG 存储的身份数据，不自动回滚独立对象/Functions。

## 4. 发布原则

当前版本可以作为自托管开发与实验验收基线，必须保留上述限制。
达到生产门槛后再提升 release 状态；每项变更以代码 + API/模型 + UI +
Linux 真实场景正反例 + 恢复证据共同验收。

## 2026-10-13：本地真实推理已解除外部凭据阻塞

独立 Go AI 网关、限定列权限元数据库身份、AI UI 和持久用量已完成代码/Helm/实际 UI 闭环。769 个公开 Linux Go/PG 测试与九组真实 UI 检查通过，见 [交付](DELIVERY-2026-10-13-AI-GATEWAY.md)。只验收文本 Chat Completions；动态供应商 I01 已在 0.58.0 完成本地基础验收，见[精确交付](DELIVERY-2026-10-20-AI-PROVIDERS.md)；完整协议/计费、HA 和全链路 TLS 仍未完成。个人保存查询已在 0.1.27 通过实际 UI 六组检查；完整交付见 [当前报告](DELIVERY-2026-10-13-SAVED-QUERIES.md)。显式项目共享查询已通过新 Linux UI/真实 PG 多用户隔离验收；个人加密执行历史已通过新真实 SQL/UI 验收。个人采集/保留策略与到期清理已在 0.39.0 验收；单条手工删除已在 0.40.0 验收；项目共享历史已在 0.41.0 完成基础多账号验收，显式个人 AI 命名已在 0.46.0 验收，默认自动命名仍需开发。

## 2026-10-15：SQL 工作台新增闭环

XLSX 与 Explain/Analyze 基础树形计划各四项真实 Linux UI 通过，版本和源码见 [交付](DELIVERY-2026-10-15-SQL-EXPORT-PLAN.md)。个人保留/到期记录删除已有独立本地验收；单条手工删除已在 0.40.0 验收；项目共享历史及基础计划比较已在 0.41.0 验收；受控 AI SQL 草稿/计划建议通路已在 Web 0.44.0 本地验收；AI 修错基础路径已在 Web 0.45.0 验收；个人加密名称/显式历史 AI 命名已在 0.46.0 验收；默认自动命名和建议准确性仍有门槛；个人加密历史和已保存共享草稿不能代替这些能力。

有界 multipart 已通过原用例 UI 续传和七类实际 SDK 操作，见[本次交付](DELIVERY-2026-10-16-CURRENT.md)与[合同](OBJECT-STORAGE-MULTIPART.md)。8 MiB 上限、大对象、GC、分片写入权接管及独立 Storage gateway 等门槛仍保留。

0.43.0 已验收原失效凭据的管理员逻辑中止/审计恢复和 multipart 分组分页，详见[合同](MULTIPART-ADMINISTRATION.md)。后台自动续跑、写入权转让、GC 等仍按各自门槛；Web 0.44.0 的受控 SQL AI 草稿与计划建议通路已验收；0.45/0.46 的修错和显式命名也已验收；继续 F02 及其余账本项。


## 2026-10-17：定时调用本地基础闭环

UTC Cron 的代码/Go/PG、原计划从零冷醒到真实 SQL、伪造拒绝、UI 历史与正常退休已通过，[精确交付](DELIVERY-2026-10-17-FUNCTION-SCHEDULES.md)保留所有原失败与纠正。F02 的分支继承、完整 HA 不随之关闭；异常 Node 的自动有界回收另列生命周期缺口。下一阶段继续数据库/角色 rename 与其余工程。


## 2026-10-17：目录重命名基础闭环

[精确交付](DELIVERY-2026-10-17-CATALOG-RENAME.md)记录五个新 Go/PG 顶层测试、UID/CAS 及未知提交、三个 OpenAPI 合同测试和十项真实 UI。Writer/原 Reader 冷醒保留原 OID/密码/数据。C02、完整生命周期、外部 fencing 和 HA 等门槛继续保留。下一项处理异常 Functions 自动有界退休。
