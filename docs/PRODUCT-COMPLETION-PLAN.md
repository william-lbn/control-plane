# 完整产品实施与逐项验收计划

## 1. 本轮约束

继续执行完整官网 Backend 目标。需要外部账号、模型调用凭据、SMTP 或 OAuth
应用的真实接入放在后面；这些依赖不阻止我们实现本地服务、API、数据模型、UI、
隔离测试和部署代码。当前三节点有限资源下每次串行验证一个闭环，基础组件保持
资源预留；只退休已完成测试的运行资源，数据和证据保留。

实施合同由 FULL-PRODUCT-IMPLEMENTATION.md 和实际 OpenAPI 共同约束。
下面是顺序及退出条件，不代表所有项目已经实现。

历史 0.1.4 基线的运行语言收敛与保留生命周期已完成其版本闭环：原生 Go Proxy/Storage adapter、统一 Helm 0.1.4
已部署，327 Go 测试和六套真实 Linux UI（83 检查）通过；现场没有运行中的 Python
Pod。私有 Python 测试工具不是产品后端。源码/镜像锁和失败修复记录见
[本轮验收](https://github.com/william-lbn/neon-helm/blob/v0.1.4/docs/ACCEPTANCE-2026-10-08-LIFECYCLE.md)。
此迁移不等于第 4 项的连接账本、跨实例栅栏和 HA 已经完成。
本次增加第 2 项的项目/叶分支保护、保留删除、七天项目恢复和两个 Reader 的独立
休眠/冷醒测试。准确合同见 [保留删除](RETAINED-DELETION.md)；是否通过现场测试以
对应版本交付报告为准。第 2 项的物理 GC、TTL、失败创建清理和独立 Endpoint 删除，
以及第 3/4 项的资源和分布式门槛，仍须独立实现与验收。

## 2. 不依赖外部凭据的交付序列

| 顺序 | 闭环 | 必须交付和验证 |
| --- | --- | --- |
| 1 | 历史点恢复到新分支 | 原生 timestamp/LSN、真实保留边界、LSN lease、历史目录隔离、API/UI、幂等恢复、SQL 与冷醒 |
| 2 | 完整删除生命周期 | 项目/分支依赖图、保护、入口关闭、服务停用、Compute 回收、tombstone、保留与恢复窗口、安全 GC、竞争与重试 |
| 3 | 精细资源与多 Reader | 小数 CPU 的真实 Runner 配额、分配/使用区分、完整内存归还、压力/OOM/coldboot、多个 Reader 独立扩缩与写入拒绝 |
| 4 | Go Adapter 与跨实例栅栏 | 替换兼容原型、Proxy 连接账本、generation/epoch 外部检查、多实例并发、旧 Worker 拒绝、短连接与删除竞争 |
| 5 | 本地可信传输 | 内部 CA/IP SAN、浏览器/API/SQL/管理入口/元数据/服务校验、证书轮换和失败负例；无公网域名也能完成 |
| 6 | Managed Auth 基础闭环 | 受限 Better Auth runtime、分支账户/会话/JWT/JWKS、注册登录登出、父子隔离、Data API RLS 与 UI；Console 登录独立 |
| 7 | 产品 Object Storage | 不可变 blob、随分支清单、S3/SigV4/预签名、ACL/CORS、配额、multipart、共享引用与 GC、上传下载 UI |
| 8 | Functions | 不可变 Node bundle、隔离资源/网络、HTTP/SSE/WS、版本回滚、env Secret refs、cron/对象触发去重与重试、部署和日志 UI |
| 9 | 本地恢复、监控与 HA 演练 | 元数据/对象/WAL/Secrets 全栈恢复、实测 RPO/RTO、持续指标/告警、节点与进程故障矩阵；独立物理故障域仍另列门槛 |
| 10 | 原分支恢复与 Backend 一致性 | 备份分支、连接切换、Time Travel Assist、历史窗口管理、Auth/对象清单/函数版本协调、失败恢复 |

每个闭环提交可编译运行代码、迁移、严格 OpenAPI、React UI、Helm/schema/锁定镜像、
Linux 正反例与恢复测试、当前交付报告；通过后才更新对应 capability。

## 3. 后置的外部集成

* AI Gateway：先完成供应商配置/Secret adapter、模型目录、协议/流式、
  分支授权、预算和用量代码；协议测试明确标为测试上游。真实模型推理需要
  平台运营方在受保护页面接入服务，不能以测试回复替代。
* Auth：SMTP 送信、邮件验证/密码重置投递、OAuth、OIDC/SSO 与外部 IdP
  需要真实配置。基础密码登录与分支隔离先行实现，认证协议复用维护中的组件。
* 独立物理故障域、异地恢复和真实长期负载由部署输入和硬件条件决定，
  软件自动化与有限实验演练先行，认证结论分别记录。

## 4. 状态与证据规则

1. `设计`、`实现`、`集成验证`、`现场 UI 验收`、`生产准入`分开记录。
2. 失败记录不可覆盖；重试使用新的 attempt。通过历史版本不自动放行新镜像。
3. 当前代码即使 Pod Ready 也不等于完整生产认证；独立门槛见 PRODUCTION-GATES.md。
4. 安全删除只作用于确认完成、归属于当前测试的 VM/Runner/Job；归档后按 UID
   与版本删除，不能删除 PVC、WAL、对象、业务目录、Secrets 或本项目历史证据。
5. 本轮首项的准确合同和复测步骤见 HISTORICAL-BRANCH-RESTORE.md；最终结论
   以该项实际 Linux UI、发布镜像和 Helm 验证回执为准。


## 2026-10-08 增量：Managed Auth

已进入 Managed Auth 基础闭环实现，具体合同见 [MANAGED-AUTH.md](MANAGED-AUTH.md)。
Better Auth 1.7.7 与 pg 8.23.1 由 Linux 查询注册表后锁定；新增 metadata migration 016、四个管理 API、分支公共 Auth 入口、独立 TS runtime、Go leased Driver、自动子分支 companion Operation、Auth/删除恢复与 Data API 联动、React 体验页面和第七个发布镜像。
本次 SQL TLS 与标准 JWKS 兼容修复后的 Linux 开发门槛为 354 个 Go/真实 PostgreSQL 测试（race、vet、零跳过）、5 个 Auth 测试及 1 个原生 TLS 测试、13 个 Web 测试；真实 Neon UI 需单独留证。此前候选版的 UI 注册分别暴露 CA 信任和 pg 覆盖主机名问题，失败证据保留，不能用传输层检查冒充产品通过。Auth UI 测试失败时会有界停用自有服务/Compute，记录原 Operation/幂等键并保留数据库和凭据。原有跨实例外部栅栏、全链路可信 TLS、HA/DR、Functions、产品 Object Storage 和 AI 推理门槛继续保持未通过；本增量不将它们变成已完成状态。

`2dcd7d2` 的基础 Auth 已通过真实 Neon React UI 23 项检查，包含账号复制与会话/JWT 隔离、Data API RLS、自动缩零和登录冷唤醒。最新证据和失败修复边界见 [Auth 交付](DELIVERY-2026-10-08-MANAGED-AUTH.md)。此项已可复测；顺序 7 的产品对象存储和顺序 8 的 Functions 仍需从实际服务实现开始。

历史 0.1.5 / `2dcd7d2` 的七套真实 Linux UI 共 106 检查通过，另有原 Reader
失败操作的五项 UI 恢复检查；每套 managed VM/Runner 归零。统一 Helm 与复测
证据以最新 Auth 交付报告为准。共享宿主磁盘停顿根因未定位，仍阻止生产准入。


## 2026-10-09 increment: branch file management

See [OBJECT-STORAGE.md](OBJECT-STORAGE.md) for the actual REST v1 contract, data model, concurrency, diagram, deployment and manual/automated UI acceptance. This implements the first real product file path with independent backing credentials; the public S3 protocol is still false. Independent next gates: unified storage scopes/SigV4, multipart and CORS, file-trigger outbox, reference-safe physical GC, standalone restricted gateway, physical quotas and restore drills. The source changes do not implement Functions or inference and do not relax HA/TLS/fence gates.

`d4549a8` / unified Helm 0.1.6 has been deployed with seven matching images and
passes the 19-check real Linux Object Storage UI suite. The original failed file
fixture has a separate four-check recovery receipt. This closes the first REST
slice of item 7, not its full S3/GC contract. The exact current regression matrix,
retained failures and environment boundary are in
[the current delivery](DELIVERY-2026-10-09-OBJECT-STORAGE.md).

The independent Compute deletion increment in
[ENDPOINT-DELETION.md](ENDPOINT-DELETION.md) first closes a remaining part of
item 2: owned VM/Runner retirement, branch data retention, replacement credentials,
dependency blocking and exact Operation replay. Physical GC/TTL and external
fencing remain independent gates; live acceptance is reported separately.

The next local service implementation is item 8: actual isolated Functions,
following `content/docs/compute/functions/overview.md` (snapshot update
2026-09-22) and `reference/runtime-limits.md` (2026-09-16). A Node.js process in
a shared Kubernetes Pod is insufficient to claim the documented microVM-per-
isolate boundary. First implement immutable bundle/version/intent and a NeonVM
isolated runner with bounded HTTP invocation; then branching, zero/wake, secrets,
streaming and triggers. Each increment must have real execution/tenant negatives,
standard APIs, UI, Helm and retained recovery evidence before capability enablement.
