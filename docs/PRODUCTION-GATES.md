# 生产门槛与官网对齐

状态截至 2026-10-15；运行 API 0.32 / metadata migration 030 / Chart 0.9.16 / Helm revision 83。新增应用 TOTP 八项实际 UI 通过，见 [精确交付](DELIVERY-2026-10-15-AUTH-TOTP.md)；API/Auth 源 967e1b3、Web 源 720cea9。之前各功能沿用各自回执，没有重跑旧全量。公开统一 Helm 发行仍为 0.1.30；父 Git d1230bb 不代表运行源码。
前一版本 `be04cb4` 的真实 Linux/React 闭环见 [Auth/S3 交付](DELIVERY-2026-10-14-AUTH-S3.md)：
788 Go/真实 PostgreSQL 检查、八个同源公开镜像、三节点 24 次串行 digest 导入；
应用密码/会话十组 UI，以及实际 S3 SDK 签名/字节/权限/轮换/撤销/正常缩零均通过。
[Functions HTTP/SSE 与自动零/一次冷醒](DELIVERY-2026-10-14-FUNCTIONS-HTTP.md)
在 0.1.28 有自己的十组 UI 回执。当前仍是预览产品；这些切片不关闭下列生产门槛。
历史失败、版本和原 fixture 保留，未重复不相关的旧全量 UI 套件。
本地 OTP/Magic Link/S3 目录三个新 UI 闭环已通过；本批综合 Go/PG 回归 793 pass / 0 fail / 0 skip。
需运营方输入见 [OPERATOR-INPUTS.md](OPERATOR-INPUTS.md)，不阻塞其他本地工程。

本地邮件链接验证/密码找回已通过十组 UI、真实 SMTP、790 Go/PG 与最终八发布审计，见 [精确回执](DELIVERY-2026-10-14-AUTH-MAIL.md)。本地 OTP 和应用 Auth OpenAPI 已通过八组真实 UI，见 [OTP 回执](DELIVERY-2026-10-14-AUTH-OTP.md)。Magic Link 已通过八组真实 UI，见 [本地交付](DELIVERY-2026-10-14-LOCAL-MAGIC-LINK.md)；应用 TOTP 本地八项 UI 已通过；公网邮件、Console MFA、OAuth 等仍按各自门槛推进。

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
| 多租户生产安全 | 受邀注册/已有账号接受、一次凭据、撤销/过期、组织锁、事务审计已有代码；OIDC/SSO/MFA、公网邮件投递与完整攻击矩阵仍缺 | 新版本 Linux UI 与实际 PG 验收、跨组织/项目负例、撤销竞争、外部安全评审 |
| 跨实例缩零栅栏 | DB 租约无法 fence 所有外部动作；缺短连接 ledger | 多 API/Proxy 并发、过期 worker、新连接/删除竞争负例与无误删 |
| HA | 独立 API/Worker 的领导租约已有代码；两者仍单副本；Metadata/数据面单故障域实验部署 | 独立故障域、主从切换/脑裂矩阵、指标和业务连续性 |
| DR | 元数据/Secrets/对象/WAL 恢复未形成全栈演练 | 固定镜像与密钥、隔离 restore、真实 PG 数据校验、实测 RPO/RTO |
| PITR | 已实现按时间戳/LSN 恢复到新分支、原生保留窗口/租约与 UI；原地恢复、Time Travel Assist、完整 Backend 一致性恢复未实现 | 新分支路径按版本做真实 Linux UI 验收；另验原地切换、GC 竞争、服务一致性、长期保留与恢复 |
| 小数 CPU | 已支持的 bounds 只有整核 | Guest cgroup quota + SQL + 调度竞争/计量实测 |
| 内存缩回 | 未通过完整 RAM 扩缩闭环 | Guest 状态、压力/回收、OOM 防护、SQL 持续和冷醒完整循环 |
| 基础设施稳定性 | 同物理盘 VM 存在共同 I/O 竞争 | 分离故障域/IO、持续负载、etcd 延迟与错误预算 |
| TLS / 网络 | Auth→Proxy 已实现显式 CA/证书名称验证及真实 TLS 正反例；浏览器、控制库、Storage 等仍有实验传输例外 | 正确域名或 IP SAN、可信证书、verify-full、管理网络隔离及 NetworkPolicy；本地 CA 路径不依赖公网域名 |
| 目录完整性 | 自定义 API-owned 数据库 Owner 变更已通过实际 UI/SQL、Writer/Reader 冷醒、幂等与正常回收；rename、默认/系统 Owner、外部 DDL 回收、默认身份迁移未完成 | 按版本 UI/实际 PG 验证；扩展动作与完整身份迁移另设门槛 |
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

Managed Auth 已有 Go Driver、Better Auth 1.7.7 运行时、React UI、注册/会话/JWT/分支隔离与 PostgreSQL CI，详细见 [Auth 合同](MANAGED-AUTH.md)。真实 Neon UI 结果按版本交付报告记录；应用密码/会话安全已完成本地验收；公网 SMTP/OAuth、Console MFA、分支 factor 迁移、完整恢复/删除一致性、生产隔离与 HA 仍须独立实现/验收。

产品 Object Storage 已实现分支目录、独立受限对象凭据、Go REST Driver、React UI、条件写入、签名下载和分支继承，详见 [Object Storage 合同](OBJECT-STORAGE.md)。REST 是 `neon-object-rest-v1`；f0b63f5 Linux 现场 UI 19 项已通过，实际版本与其余回归见 [当前交付](DELIVERY-2026-10-12-SQL-IDENTITY.md)。基础 S3/SigV4/signed chunks/统一存储凭据已在 0.1.29 实际验收，见 [S3 合同](OBJECT-STORAGE-S3-CORE.md)。基础 COPY、条件批量删除、预签名 PUT、CORS 和用户 metadata 已通过本地 UI；完整 multipart/COPY 条件/metadata REPLACE/大文件、物理 GC、分布式网关隔离及 HA/DR 仍未完成。

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

独立 Go AI 网关、限定列权限元数据库身份、AI UI 和持久用量已完成代码/Helm/实际 UI 闭环。769 个公开 Linux Go/PG 测试与九组真实 UI 检查通过，见 [交付](DELIVERY-2026-10-13-AI-GATEWAY.md)。只验收文本 Chat Completions；动态供应商管理、完整协议/计费、HA 和全链路 TLS 仍未完成。个人保存查询已在 0.1.27 通过实际 UI 六组检查；完整交付见 [当前报告](DELIVERY-2026-10-13-SAVED-QUERIES.md)。查询历史、共享查询仍未实现。
