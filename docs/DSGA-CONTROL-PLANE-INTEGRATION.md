# DSGA 控制面主线集成与生产验收

本文件只包含产品架构、源码接口、验证与生产准入；不保存任何主机 IP、个人目录、集群运行日志、原始 Journal 或凭据。运维审计材料应存放于受限的独立 Artifact 存储，而不是产品仓库。

## 分支和发布规则

- 主线以 control-plane 的 main 为唯一业务基线。DSGA 独立集成分支完整保留经过审查的核心功能代码，合并前必须经过 CI、真实 K8s 和安全审查。
- Neon、Autoscaling、PostgreSQL PG17 与 PG18 仍各自维护经验证的 DSGA 开发分支；镜像发布必须固定完整 Commit、PG ABI、协议版本和 OCI Digest。
- Git 历史可快进不等于完整功能已经通过生产回归。任何测试失败、权限未知结果、重试不确定、数据丢失或对象身份漂移，都须 Fail Closed。
- 不在源码中包含旧机器日志、环境专用 ConfigMap/PV、Token、密钥或完整线上 Journal。

## 分层责任与接口

| 层 | 负责原始事实 | 必须拒绝的越权 |
|---|---|---|
| Control API 与 Worker | 组织/项目/分支/Endpoint、持久幂等意图、原始身份、事务和操作生命周期 | API 的 202 不能证明 SQL 已就绪；不能代替 VM/Host 授权 Shrink |
| Storage Controller、Pageserver、Safekeeper | Tenant、Timeline、WAL Quorum、LSN 和分支可恢复状态 | Pod Ready 不等于数据已复制或 WAL 已耐久 |
| Adapter 与 Proxy | 精确 VM/Service/Route UID、管理 TLS/JWT、连接与路由接管 | 不应将只读能力提升为创建凭据或越 namespace 访问 |
| Runner/Host | 原始 Resource Budget、VM 规格、硬限额、Lease、执行与真实回收 | 不能伪造 Guest、PG 或 Host ACK |
| Autoscaling Scheduler 与 Agent | 资源目标、策略、执行计划、控制闭环 | 不能把目标值当成物理内存归还 |
| PG17/18 DSGA 内核 | Dynamic Region/Granule/Buffer Pool 增减、同步和持久化、SQL 事实 | 无权独立确认全机 Shrink，必须保持 fail-closed |

每次跨层操作遵循原 Request Key、Owner Epoch、Resource Identity、预留 Budget、原始执行回执、状态只读观察、最终结算与幂等恢复。出现网络未知结果、原 Owner 变化、Quota FSYNC 证据缺失或 Journal 容量不足，不得自动复发副作用。

## 数据库迁移的兼容性

DSGA 创建身份迁移曾与主线迁移编号碰撞，当前采用独立的前向 Join/Migration Registry。必须保留已发布 Migration 原文和数据库版本历史，不允许删除旧记录、重编号已运行迁移或在生产库中手改。验收覆盖空库、旧主线升级、DSGA 分支升级、失败中断后续跑与迁移并发。

## CI 和端到端阻断标准

- Go：固定工具链、gofmt、vet、Race、完整测试分片、零失败零跳过，并要求每分片具备独立真实 PostgreSQL/RLS/PostgREST 环境和充足共享内存。
- API、CLI/MCP、Console：OpenAPI 机器合同、文档、CLI 操作目录保持一致；Autosc​aling 查询接口只读；Secret 不得随重定向或取消请求泄露；TypeScript、单测、生产构建和真实 Playwright 浏览器分别验收。
- Helm/K8s：严格 schema、不可变镜像、最小 RBAC、CEL Admission 正负例、独立测试命名空间和真实故障重建。默认不允许创建 VM，缺可信 Catalog/Lease/Host Budget 不得放行。
- 存储：三 Safekeeper 必须跨独立持久化故障域；记录真实 SQL 提交、WAL Quorum、LSN、故障注入后的数据一致性和 RPO/RTO。
- 伸缩：新 PG17 和 PG18 实例分别完成 SQL、分支、Buffer/Guest/VM/Host Grow 和 Shrink、预算结清、资源回收、断电/断网恢复与性能 SLO。
- 发布：Control Plane、Neon、Autoscaling、PostgreSQL 和 Helm 的源码 Commit、完整 OCI digest、签名、发行 BOM 与跨仓 CI 必须全部可复核；只通过单仓单测不得发布。

当前完整产品尚未通过生产 E2E。任何 P40/P42 受保护的创建/凭据操作只有经过授权才能执行；不允许为绕过阻塞而清 Journal、改写原 quota、伪造 ACK 或放宽 Guard。
