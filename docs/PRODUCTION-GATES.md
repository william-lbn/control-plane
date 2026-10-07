# 生产门槛与官网对齐

状态截至 2026-10-07；这是一份缺口清单，未通过项不能被 Ready Pod 或 CI build 替代。

## 1. 已实现路径

原生项目/分支/Endpoint、一个 Writer/多个 Reader、Proxy SQL、
数据库/角色目录及轮换、组织权限与 Key scope、持久 Operation、
实验环境休眠/唤醒、整数 CPU 边界与 UI 监控均有运行代码。
历史实验环境 Linux PG/UI 证据保留在维护者工作区，未把私密记录上传公共 Git。

## 2. 独立放行门槛

| 门槛 | 当前缺口 | 完成判据 |
| --- | --- | --- |
| Proxy/Storage adapter | 原生 Go 实现及故障矩阵已有代码；仍单实例，通知重配置和分布式连接账本未完成；新镜像现场验收独立记录 | 固定源码/镜像、真实 Proxy/UI 冷醒与通知、全新 Helm 安装；完整 placement/Safekeeper 重配置另验 |
| 多租户生产安全 | 受邀注册/已有账号接受、一次凭据、撤销/过期、组织锁、事务审计已有代码；OIDC/SSO/MFA、邮件验证与完整攻击矩阵仍缺 | 新版本 Linux UI 与实际 PG 验收、跨组织/项目负例、撤销竞争、外部安全评审 |
| 跨实例缩零栅栏 | DB 租约无法 fence 所有外部动作；缺短连接 ledger | 多 API/Proxy 并发、过期 worker、新连接/删除竞争负例与无误删 |
| HA | 独立 API/Worker 的领导租约已有代码；两者仍单副本；Metadata/数据面单故障域实验部署 | 独立故障域、主从切换/脑裂矩阵、指标和业务连续性 |
| DR | 元数据/Secrets/对象/WAL 恢复未形成全栈演练 | 固定镜像与密钥、隔离 restore、真实 PG 数据校验、实测 RPO/RTO |
| PITR | 已实现按时间戳/LSN 恢复到新分支、原生保留窗口/租约与 UI；原地恢复、Time Travel Assist、完整 Backend 一致性恢复未实现 | 新分支路径按版本做真实 Linux UI 验收；另验原地切换、GC 竞争、服务一致性、长期保留与恢复 |
| 小数 CPU | 已支持的 bounds 只有整核 | Guest cgroup quota + SQL + 调度竞争/计量实测 |
| 内存缩回 | 未通过完整 RAM 扩缩闭环 | Guest 状态、压力/回收、OOM 防护、SQL 持续和冷醒完整循环 |
| 基础设施稳定性 | 同物理盘 VM 存在共同 I/O 竞争 | 分离故障域/IO、持续负载、etcd 延迟与错误预算 |
| TLS / 网络 | 实验入口有 HTTP/lab-insecure 例外 | 域名、可信证书、verify-full、管理网络隔离及 NetworkPolicy |
| 目录完整性 | rename/owner、外部 DDL 回收、默认身份迁移未完成 | API/UI 实际 PG 验证，密码轮换全部 Endpoint 冷醒/分支一致 |
| 生命周期完整性 | project/branch delete、reset/restore 等缺口 | 所有权/保护/GC/计费/幂等/恢复与 UI 负例 |
| 监控与运营 | 长期指标、告警、审计、容量/计量不足 | SLO、持久 TSDB、告警测试、预算与容量恢复 |
| 发布供应链 | 镜像公开性和 registry 凭据需独立确认 | 固定 SHA/digest、SBOM/provenance、匿名拉取、回滚镜像保留 |

首个 e4fd1f3 发行三个镜像已由 Linux CI 发布，Linux 匿名 manifest/config 验证通过。
后续源码每次修改仍须取得本次提交的独立回执；不能沿用旧版本通过结果。

## 3. 官网 Backend 服务

Auth、Functions、Object Storage、AI Gateway 尚未实现服务。Data API 已有原生 Driver、
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
