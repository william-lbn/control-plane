# 完整产品实施与逐项验收计划

## 1. 本轮约束

当前 Linux 本地批次：[2026-10-22 Auth 分支双重认证迁移](DELIVERY-2026-10-22-AUTH-FACTOR-MIGRATION.md)：显式物理分支 TOTP 迁移、独立密钥重封装、复制会话/JWKS/链接清除、复制 SQL 登录禁用、未完成绑定清除、父子恢复码隔离及验证器恢复通过。4 项新增 Go、3 项受影响 Go、固定 Better Auth 的真实 PostgreSQL 互通、累计 11 项实机检查通过。API/Worker 0.72.0，Web 重建，schema 059 / Chart 0.9.69 / Core Helm 153；Auth 运行镜像、Gateway 0.66.3 / Helm 56、AI 0.58.0 保留。最终三节点、三个 Safekeeper Ready，VM 0。A04 基础闭环完成，其余 13 个业务组继续；性能/可靠性专项后置。

后续按 [当前功能清单](FUNCTIONAL-STATUS-2026-10-21.md) 推进剩余业务：本轮优先完成有界 S3 大对象；C02 剩余身份语义保留，继续处理完整大对象/安全 GC、Auth 高级/生命周期、原地恢复和计量/预算。Functions 对象触发器和客户日志基础已有独立通过记录。性能、资源压力、可靠性、外部栅栏、HA/DR 和长期环境认证后置；复用来源未变的原验收，只验证新增及具体受影响路径。
2026-10-18 用户调整优先级：先补齐全部业务功能，加快逐项交付；性能压测、可靠性、HA/DR、节点分区及跨实例栅栏等专项验证后置。本轮只执行必要的构建、功能正反例、权限/数据隔离及新增 UI 验收，复用已通过路径。不得据此宣称后置门槛已经通过。
下方带日期段落是历史增量，不覆盖本节当前状态。

继续执行完整官网 Backend 目标。需要外部账号、模型调用凭据、SMTP 或 OAuth
应用的真实接入放在后面；这些依赖不阻止我们实现本地服务、API、数据模型、UI、
隔离测试和部署代码。当前三节点有限资源下每次串行验证一个闭环，基础组件保持
资源预留；只退休已完成测试的运行资源，数据和证据保留。

实施合同由 FULL-PRODUCT-IMPLEMENTATION.md 和实际 OpenAPI 共同约束。
下面是顺序及退出条件，不代表所有项目已经实现。

2026-10-13 执行补充：优先现有环境可以完成的新功能闭环，不反复全量验证历史套件。
确需运营方输入的依赖统一见 [OPERATOR-INPUTS.md](OPERATOR-INPUTS.md)，后续集中处理，
不要求逐项选择，不阻塞不依赖这些输入的实现/部署/验收。

历史 0.1.4 基线的运行语言收敛与保留生命周期已完成其版本闭环：原生 Go Proxy/Storage adapter、统一 Helm 0.1.4
已部署，327 Go 测试和六套真实 Linux UI（83 检查）通过；现场没有运行中的 Python
Pod。私有 Python 测试工具不是产品后端。源码/镜像锁和失败修复记录见
[本轮验收](https://github.com/william-lbn/neon-helm/blob/v0.1.4/docs/ACCEPTANCE-2026-10-08-LIFECYCLE.md)。
此迁移不等于第 4 项的连接账本、跨实例栅栏和 HA 已经完成。
本次增加第 2 项的项目/叶分支保护、保留删除、七天项目恢复和两个 Reader 的独立
休眠/冷醒测试。准确合同见 [保留删除](RETAINED-DELETION.md)；是否通过现场测试以
对应版本交付报告为准。第 2 项的物理 GC、TTL、失败创建清理，
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
dependency blocking and exact Operation replay. Current source 0ba1732 passes
the 19-check endpoint slice, nine full suites (146 checks) and 23 separate
original-operation recovery checks; see the current Compute lifecycle delivery.
Physical GC/TTL and external fencing remain independent gates.

The next local service implementation is item 8: actual isolated Functions,
following `content/docs/compute/functions/overview.md` (snapshot update
2026-09-22) and `reference/runtime-limits.md` (2026-09-16). A Node.js process in
a shared Kubernetes Pod is insufficient to claim the documented microVM-per-
isolate boundary. First implement immutable bundle/version/intent and a NeonVM
isolated runner with bounded HTTP invocation; then branching, zero/wake, secrets,
streaming and triggers. Each increment must have real execution/tenant negatives,
standard APIs, UI, Helm and retained recovery evidence before capability enablement.

The Functions source foundation now contains immutable ZIP validation and sealed
installation, manager HMAC/boot-generation scope, bounded HTTP/SSE forwarding,
shutdown recovery, guest UID/cgroup/egress primitives and actual Node Fetch/SSE/
waitUntil execution. The internal contract and outstanding F1–F4 exits are in
[FUNCTIONS-IMPLEMENTATION.md](FUNCTIONS-IMPLEMENTATION.md). It remains disabled
until the real microVM, native Driver, Console and zero/wake acceptance exist.

## 2026-10-13：真实本地 AI Gateway

`73ae9bb` / Helm 0.1.26 已完成独立 Go 网关、React 目录/Playground、分支应用授权、文本 JSON/SSE 与 PostgreSQL 用量，真实 Windows Ollama 小模型由 Linux 浏览器端到端调用，九组检查通过且没有创建 Compute。外部模型账号不再阻塞本地推理闭环。动态供应商 UI、其他协议/计费仍继续开发；接着实现个人保存查询，不重复历史全量 UI 套件。

## 2026-10-13：个人保存查询已完成现场验收

`8c1b777` / Helm 0.1.27 已完成个人 SQL 草稿保存、重载、加载、更新、删除、
版本化 AES-GCM 加密、元数据幂等重放、Owner/租户隔离和审计。首次实际 Linux UI
六组检查通过，AI Gateway 在同一版本九组回归通过；没有执行 SQL 或创建 Compute。
772 个公开 Linux Go/PG 测试零失败、零跳过。准确接口和复测见
[SAVED-QUERIES.md](SAVED-QUERIES.md)及[当前交付](DELIVERY-2026-10-13-SAVED-QUERIES.md)。
历史源/失败记录保留，不把完成的切片重复记为待开发。下一批优先 Functions
公开独立入口/自动休眠冷醒、对象 S3 兼容和 Auth 本地高级流程；后续继续按功能闭环
实现及串行验收，域名/商业模型/外部 IdP 输入仍后置。所有生产门槛继续以当前清单为准。

## 2026-10-14：本阶段退出与下一阶段

Functions HTTP/SSE、自动零/一次冷醒、Auth 密码/会话、S3 基础 SDK 已分别通过实际 UI；
源/镜像/Helm 锁、失败恢复、原对象和剩余门槛见当前交付。本地先完成 Auth 邮件验证、
找回、Email OTP/Magic Link 和管理流程；公网与外部送信后置。MFA 在官方 roadmap
仍为 coming soon，本项目既定 MFA 也须独立实现，不能用当前会话安全代替。

有界 multipart 已通过原用例 UI 续传和七类实际 SDK 操作，见[本次交付](DELIVERY-2026-10-16-CURRENT.md)与[合同](OBJECT-STORAGE-MULTIPART.md)。64 MiB 有界大对象已通过新增验收；5 GiB、完整 checksum、GC、分片写入权接管及独立 Storage gateway 等门槛仍保留。

0.43.0 已验收原失效凭据的管理员逻辑中止/审计恢复和 multipart 分组分页，详见[合同](MULTIPART-ADMINISTRATION.md)。后台自动续跑、写入权转让、GC 等仍按各自门槛；随后继续 Q03 SQL AI/计划建议。
