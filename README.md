# Neon 自托管控制面

Go + PostgreSQL 控制 API、React + TypeScript 控制台、Go Proxy/Storage 适配器、Compute 管理网关及 Helm Chart。
许可证：Apache-2.0。项目由自托管维护者开发，与 Neon 托管服务独立。

**发行状态：预览版本。已经验证的实验环境功能不等于生产认证，也不等于 Neon 官网全部 Backend 服务。**

功能进度与当前优先级见 [已验收/剩余清单](docs/FUNCTIONAL-STATUS-2026-10-21.md)：18 组基础已有回执；五组专项后置，12 组尚未整体闭环。后续优先业务功能，复用来源未变的既有验收。

最新状态：2026-10-25 Auth 生命周期：[保留删除、恢复与身份一致性](docs/DELIVERY-2026-10-25-AUTH-LIFECYCLE.md)通过两组新增 Go 和八个累计实机检查点；前四项与原 Cookie 保留后只续验余项。恢复后 Auth 保持关闭，显式新代次拒绝旧会话/JWKS 并保留原账号、密码和 SQL 数据。复用 0.84 镜像，Core Helm 178 / schema 065 不变；原目录、945 个续验前持久 Secret UID 和 9 PVC 保留，三节点、三个 Safekeeper Ready，VM 0。A05 保留生命周期基线完成；当前 18 组基础、9 个业务和 3 个其他组继续。

此前增量：[应用 TOTP 双重认证](docs/DELIVERY-2026-10-15-AUTH-TOTP.md)八项 UI 与[项目共享查询](docs/DELIVERY-2026-10-15-SHARED-QUERIES.md)六项 UI 均通过；该批基线为 API 0.33 / Chart 0.9.17 / migration 031。

此前本地已验收：[预签名上传](docs/DELIVERY-2026-10-15-PRESIGNED-UPLOAD.md)、[CORS 与用户元数据](docs/DELIVERY-2026-10-15-STORAGE-BROWSER.md)、[Functions 请求日志](docs/DELIVERY-2026-10-15-FUNCTIONS-JOURNAL.md)。[元数据条件编辑与 S3 REPLACE](docs/DELIVERY-2026-10-15-METADATA-EDIT.md)也已通过 8 项新的独立 UI 验收。当前状态以 [生产门槛](docs/PRODUCTION-GATES.md) 和各版本回执为准；复用既有功能记录，不重复旧 UI 套件。

[本地 Auth 邮箱验证/密码恢复](docs/DELIVERY-2026-10-14-AUTH-MAIL.md)已通过十组实际 Linux UI、真实 SMTP 和 790 Go/PG 检查；邮箱 OTP 和独立应用 Auth OpenAPI 已通过[八组真实 Linux UI](docs/DELIVERY-2026-10-14-AUTH-OTP.md)；Magic Link 也已通过[八组真实 Linux UI](docs/DELIVERY-2026-10-14-LOCAL-MAGIC-LINK.md)。[环境修复回执](docs/DELIVERY-2026-10-14-ENVIRONMENT-RECOVERY.md)说明 CNI 文件故障、精确修复和 63 次主机检查。
当前为 [Linux 本地镜像/逐项验收批次](docs/DELIVERY-2026-10-15-LOCAL-BATCH.md)，镜像按组件源码快照留证，历次父提交在各交付报告单独记录；后续采用[Linux 本地构建与验收](docs/LOCAL-LINUX-DEVELOPMENT.md)，整批回归后统一提交与远程发布。本地源码快照和各组件镜像来源独立留证。
[Auth/S3 历史切片](docs/DELIVERY-2026-10-14-AUTH-S3.md)在 `be04cb4` 记录 788 项公开 Go/真实 PG 检查、
应用密码/会话安全十组实际 UI，以及原对象/凭据保留的实际 S3 SDK 验收。
S3 的 test-only `875e48b` 只修正选择器和原 checkpoint 续测，不改变运行时代码。
[Functions HTTP/SSE 与自动零/冷醒](docs/DELIVERY-2026-10-14-FUNCTIONS-HTTP.md)
在 0.1.28 通过十组实际 UI。历史版本保留自己的回执，不能当作所有当前代码的全量回归。
[历史原生 Functions 验收](https://github.com/william-lbn/neon-helm/blob/v0.1.19/docs/ACCEPTANCE-2026-10-12-FUNCTIONS.md)：`bf0dd9e` / 统一 Helm 0.1.19 已通过真实 Linux UI→microVM→Proxy→Neon SQL、停止/重部署和 NOLOGIN/保留数据验收。原失败与续测均独立保留，历史切片不能冒充全部官网功能回归。

[Functions 原生执行](docs/FUNCTIONS-NATIVE-PREVIEW.md)默认禁用，实验部署由不可变 operator 配置明确启用；其实际验收范围见上述报告。[运行监控](docs/FUNCTIONS-RUNTIME-OBSERVATION.md)已在 `be557e0` / Helm 0.1.20 通过真实 Linux UI；[精确报告](https://github.com/william-lbn/neon-helm/blob/v0.1.20/docs/ACCEPTANCE-2026-10-12-FUNCTIONS-MONITORING.md)。[显式唤醒保留版本](docs/FUNCTIONS-WAKE.md)已在 `0198f7c` / Helm 0.1.21 通过实际 UI、保留身份、幂等与正常回收；[精确报告](https://github.com/william-lbn/neon-helm/blob/v0.1.21/docs/ACCEPTANCE-2026-10-12-FUNCTIONS-WAKE.md)。[数据库 Owner 变更](docs/DATABASE-OWNER.md)已在 `e2dc587` / Helm 0.1.22 通过原 fixture 的实际 UI/SQL、Writer/Reader 冷醒和正常回收；[精确报告](https://github.com/william-lbn/neon-helm/blob/v0.1.22/docs/ACCEPTANCE-2026-10-12-DATABASE-OWNER.md)。后续版本不能沿用旧版本通过结果。
[历史 Compute 生命周期交付](docs/DELIVERY-2026-10-10-COMPUTE-LIFECYCLE.md)记录先前七镜像、统一 Helm 0.1.7、九套真实 Linux UI 共 146 项与 23 项原失败恢复检查；[先前 Object Storage](docs/DELIVERY-2026-10-09-OBJECT-STORAGE.md)、[Managed Auth](docs/DELIVERY-2026-10-08-MANAGED-AUTH.md)及历史报告保留各版本证据。
独立 Chart 提供 API、Worker、Web 进程及可选 Data API、应用凭据、Console 邀请注册、历史分支恢复、保留删除与独立 Compute 退休；Functions 包括元数据/历史、不可变存储、原生执行 Driver 和条件式 Worker Service 回收权限。Worker 使用 PostgreSQL 领导租约和只读 Runner observer。
当前只允许一个 API 和一个 Worker，跨实例外部 fencing 与 HA 仍须独立验收。
旧合并版本升级前必须执行 [停止/排空流程](docs/WORKER-SPLIT.md)。
运行代码与实际合同见 [OpenAPI](contracts/openapi-v1.json)；本项目使用 `/api/v1`，未声称兼容托管 Neon `/api/v2`。

[Object Storage REST v1](docs/OBJECT-STORAGE.md) 已增加真实分支文件服务，该基础路径自 OpenAPI 0.21.0 引入。现场验收按版本记录，现有基础 S3 独立入口、signed chunks 与统一凭据；完整兼容仍有独立门槛。[SQL 脚本](docs/SQL-SCRIPTS.md)新增原生解析、逐条结果、事务结果和输出预算，已在 0.1.23 通过首次实际 UI 六组检查；原单条请求保持兼容。[Explain/Analyze 与 CSV/JSON 导出](docs/SQL-TOOLS.md)已在 0.1.24 通过首次实际 UI 五组检查；[Functions 历史部署恢复](docs/FUNCTIONS-ROLLBACK.md)已在 0.1.25 通过原 fixture 实际 UI 九组检查；[SDK 空闲断开处理](docs/FUNCTIONS-SDK-RELIABILITY.md)已验证进程存活、明确重连和保留写入。需人工提供的外部依赖统一见 [人工输入清单](docs/OPERATOR-INPUTS.md)，其余工程任务继续推进。

## 1. 功能范围

| 功能 | 本仓库代码 | 当前边界 |
| --- | --- | --- |
| 组织、成员、项目授权、API Key | 已实现，真实 PostgreSQL 集成测试 | 加法式项目授权；Console 自助 MFA 已验收；完整生产安全审计、SSO/组织强制 MFA 未完成 |
| Console 邀请与受邀注册 | 账号绑定、一次性凭据、幂等/过期/撤销、事务审计、Go/React/PG 测试 | 手动安全交付邀请；Console 自助 TOTP/恢复码与会话撤销已验收；邀请邮件自动投递、OIDC/组织强制 MFA 仍缺，独立于 Managed Auth |
| 项目、当前时间点分支、Writer、多个 Reader | 已实现，异步 Operation + SQL 就绪探针 | 单集群、单 Region、PG16；不支持所有项目/分支删除与恢复动作 |
| PostgreSQL 连接与 SQL 工作台 | 通过独立 Neon Proxy 和 Endpoint selector | 应用不能直接连接控制 API；生产入口必须配置可信 TLS |
| 数据库、角色与密码轮换 | 分支目录、Compute 原生配置、Proxy spec 调谐；自定义库 Owner 变更和受管理库/角色 rename 已通过实际 UI、原 OID/密码/数据及 Writer/Reader 冷醒 | 默认/系统身份和未知外部 DDL 恢复未完成；见 [Owner 合同](docs/DATABASE-OWNER.md)和[重命名回执](docs/DELIVERY-2026-10-17-CATALOG-RENAME.md) |
| CPU / 内存边界、自动休眠、连接唤醒 | 已实现实验环境控制路径 | 整数 CPU 和实际 QEMU 250→500→250m、SQL/冷醒已验收；严格 Guest 隔离、完整物理内存归还、跨实例外部栅栏仍未通过 |
| 监控、Operation 步骤、错误与重试 | 已实现 | 长期指标持久化、SLO 告警、完整审计导出未完成 |
| 分支 Managed Auth | Go/TS/React、真实 PG 与 Neon UI 注册/会话/JWT/克隆隔离/RLS/自动缩零通过 | 密码/会话、邮件、OTP/Magic Link、TOTP、Admin/组织与固定本地 OAuth/手机/签名事件已分别验收；保留删除/恢复和身份代次已验收；商业接入、KMS 根轮换及可信 TLS/HA 继续；见对应交付 |
| 时间点恢复到新分支 | 原生时间戳/LSN、存储保留租约、异步调谐、历史目录隔离、React UI | 默认禁用；仅受管理分支；原地恢复、Time Travel Assist、完整 Backend 一致性恢复未实现 |
| 项目/分支保留删除与独立 Compute 删除 | 保护/依赖图、关闭 Proxy 和服务、正常 Runner 回收、held tombstone、项目恢复、原密码重建与 React UI | 分支 TTL 和唯一原失败创建保留清理已验收；物理 GC 与跨实例栅栏后置；详见 [删除手册](docs/RETAINED-DELETION.md)和本版交付 |
| HA / DR | 部分租约与恢复工具 | 未通过独立故障域 HA/DR；历史新分支恢复不等于完整 DR |
| 产品 Object Storage | Go REST Driver、独立受限 bucket、分支目录、REST/S3、SigV4/signed chunks、统一密钥、条件写/预签名、COPY/bulk/CORS 与有界 multipart 已分别实际验收 | 当前单对象 64 MiB；标签/POST/尾部校验已通过；5 GiB/完整 checksum、安全物理 GC、完整兼容矩阵、HA/TLS 仍有缺口 |
| Functions | Go Driver、隔离 Node guest、ZIP/环境、真实 SQL、停止/重部署/删除、监控、版本冷醒、UTC 定时触发和客户异常 idle 回收已分别通过实际 UI；默认关闭 | 公开 HTTP/SSE/WebSocket、自动休眠/冷醒已验收；对象事件、客户 stdout/stderr、完整计量和 Backend 分支复制仍缺 |
| AI Gateway 推理 | 独立 Go 网关、模型目录/Playground、文本 Chat/SSE、分支应用授权与用量，本地 Ollama 已真实 UI 验收 | 动态供应商管理、其他协议/预算、商业上游、HA/可信 TLS 尚有缺口 |
| Data API | 原生 Go Driver、持久 Operation、React UI、分支 JWT Gateway 和固定 PostgREST 镜像 | 默认禁用；须显式 labHTTP，真实 Neon UI 验收独立于 PG CI；RPC/views 等不在首版范围 |
| 分支应用凭据 | 一次性 Token、范围/到期、轮换/撤销、哈希存储、当前权限检查和 UI | ai_gateway:invoke、storage:read/write；加密 signing material、同 ID 轮换和即时撤销；不替代供应商/租户生产准入 |

**外部依赖**：Neon Storage Controller、Pageserver、Safekeeper、Proxy、持久对象存储、
NeonVM、Autoscaling Agent/Scheduler，以及与本版合同匹配的 Proxy/Storage adapter。
Proxy/Storage adapter 的 Go 实现、镜像入口和故障测试现已包含在本仓库；统一部署 Chart 位于 neon-helm 仓库。
Go 实现的部署验收状态以当次交付报告为准；仍限单实例，不能据此放行跨实例缩零或 HA。
Python SSH、采证和历史验证工具属于维护者私有工具，不进入本产品源码或业务镜像。
控制面、管理网关与 Data API Chart 不替代完整 Neon/Autoscaling 基础设施。

## 2. 目录与合同

```text
api/                  Go module：API/Worker、Proxy/Storage Adapter、Compute/Data API Gateway、DR 工具、SQL migrations
web/                  React/TypeScript：控制台、Node 回归测试、Playwright 真实 UI 测试
contracts/            版本化 OpenAPI；路由与引用由 Go 测试校验
charts/               控制面、Compute 管理网关与 Data API 认证入口 Helm Chart
containers/           固定 builder / runtime digest
tools/                Linux CI、Helm 安装/验收、供应链输入输出
docs/                 架构、对象模型、部署、测试、来源及生产门槛
.github/              Linux 质量 CI、镜像发布、受保护的 live UI workflow
```

- [独立 Compute 删除、重建与保留数据](docs/ENDPOINT-DELETION.md)
- [架构与模型](docs/ARCHITECTURE.md)
- [实际 API 操作索引与自动同步](docs/API.md)
- [原 Operation 恢复与未知 DDL 的受控修复](docs/OPERATION-RECOVERY.md)
- [Linux 部署与回滚](docs/DEPLOYMENT.md)
- [测试与交付标准](docs/TESTING.md)
- [Fork 来源与镜像对应关系](docs/SOURCE-PROVENANCE.md)
- [生产功能门槛](docs/PRODUCTION-GATES.md)
- [API/Worker 拆分与升级](docs/WORKER-SPLIT.md)
- [Go Proxy/Storage 适配器：合同、安全边界与升级](docs/GO-PROXY-ADAPTER.md)
- [Go 运行链路、Helm 0.1.3 和五套 Linux UI 验收](https://github.com/william-lbn/neon-helm/blob/main/docs/ACCEPTANCE-2026-10-07-GO-ADAPTER.md)
- [统一 Helm 0.1.4、保留恢复和六套正式镜像 UI 验收](https://github.com/william-lbn/neon-helm/blob/v0.1.4/docs/ACCEPTANCE-2026-10-08-LIFECYCLE.md)
- [分支 Managed Auth：Go Driver、TypeScript 运行时、React UI、分支隔离与复测](docs/MANAGED-AUTH.md)
- [七镜像、Auth 23 项 UI、环境诊断和本版回归](docs/DELIVERY-2026-10-08-MANAGED-AUTH.md)
- [完整 Backend 产品实施合同](docs/FULL-PRODUCT-IMPLEMENTATION.md)
- [Functions 执行基础层、内部协议和下一步验收](docs/FUNCTIONS-IMPLEMENTATION.md)
- [Functions 不可变数据模型、并发准入和删除依赖](docs/FUNCTIONS-MODEL.md)
- [Functions SQL 最小权限、owner 准入与真实 PostgreSQL 验证](docs/FUNCTIONS-SQL.md)
- [Functions 候选实例、租约和 Worker 重启恢复](docs/FUNCTIONS-DRIVER.md)
- [Functions 不可变代码包、环境 Secret 与丢失回复恢复](docs/FUNCTIONS-BACKING.md)
- [Functions 不可变引导 Secret、持久 UID 与重启恢复](docs/FUNCTIONS-BOOTSTRAP.md)
- [AI Gateway 官网核验、平台配置与动态模型设计](docs/AI-GATEWAY-PLATFORM-DESIGN.md)
- [原生 Data API 驱动与部署](docs/DATA-API-NATIVE-DRIVER.md)
- [分支应用凭据：模型、API、部署、UI 和验收](docs/BACKEND-CREDENTIALS.md)
- [控制台成员邀请、受邀注册与复测](docs/CONSOLE-INVITATIONS.md)
- [异步 Operation 观察恢复与 Linux 故障注入验收](docs/OPERATION-OBSERVATION.md)
- [历史分支恢复：API、LSN 租约、目录隔离与 UI 复测](docs/HISTORICAL-BRANCH-RESTORE.md)
- [完整产品逐项实施与交付计划](docs/PRODUCT-COMPLETION-PLAN.md)
- [保留删除、恢复、双 Reader 与 Data API 生命周期候选验收](docs/ACCEPTANCE-2026-10-07-LIFECYCLE.md)
- [2026-10-04 产品切片验收、修复与手动复测](docs/ACCEPTANCE-2026-10-04.md)
- [贡献规范](CONTRIBUTING.md) / [安全政策](SECURITY.md)

## 3. Linux 开发与 CI

固定工具链：Go 1.27.1、Node 24.19.0；Web 与 Auth 锁文件提交到仓库。
测试使用专用可丢弃 PostgreSQL，不能把生产数据库 DSN 注入 CI。

```bash
export NEON_V2_TEST_DATABASE_URL='postgres://ci:ci-disposable@127.0.0.1:5432/control_ci?sslmode=disable'
bash tools/install-postgrest.sh /tmp/neon-postgrest
export NEON_DATA_API_TEST_POSTGREST=/tmp/neon-postgrest/postgrest
make test
make web
bash tools/install-helm.sh /tmp/neon-helm
PATH="/tmp/neon-helm:$PATH" make helm
```

CI 在 Linux 执行 gofmt、vet、race、真实 PG 集成、OpenAPI 路由/引用、
前端格式/回归/类型/构建以及 Helm 拒绝门槛。集成测试跳过会导致 CI 失败。
详细的真实 UI 流程见 [测试手册](docs/TESTING.md)。

## 4. 镜像与部署

可信分支通过质量门槛后发布 `control-api`、`control-web`、`control-gateway`、`control-dataapi`、`control-postgrest`、`control-adapter`、`control-auth`。
原生 Data API 的数据库权限、RLS、服务开关及现场测试见
[Driver 手册](docs/DATA-API-NATIVE-DRIVER.md)。默认禁用，不能用 Ready Pod 代替实际数据访问验收。
tag 使用 `sha-<full-commit>-r<run-id>-a<attempt>`，重跑产生新 tag；部署固定 registry manifest digest，不能使用 `latest`。
默认 GHCR owner 为仓库所有者；Docker Hub 需要在本仓库设置
`DOCKERHUB_USERNAME` 与 `DOCKERHUB_TOKEN`。初次 GHCR package 公开性须独立检查。

```bash
helm upgrade --install neon-control charts/neon-control-plane -n neon \
  -f /secure/operator/control-values.yaml --wait --timeout 10m
```

此命令的前提、Secrets、完整参数和安全回滚步骤见 [部署手册](docs/DEPLOYMENT.md)。
没有外部基础组件、可用 adapter、可信证书和实际镜像 digest 时不能直接部署运行。

## 5. 演进方向

按照 Neon 官网对象语义扩展：Organization → Project → Branch → Backend services，
Postgres 下包含一个 Writer、多个 Readers、数据库、角色和 Data API。
具体实现受能力门槛约束，所有新增服务必须提供实际驱动、API、UI、负例和 Linux 现场测试。
独立 HA/fencing、容量、TLS、备份恢复门槛完成后再发布生产就绪声明。


## 2026-10-09 Object Storage increment

[Branch Object Storage REST v1](docs/OBJECT-STORAGE.md) adds Go manifest/immutable-blob access and React bucket/file management while preserving native timeline inheritance and branch isolation. The 2026-10-09 seven-image deployment and 19 real Linux UI checks are recorded in its [versioned delivery](docs/DELIVERY-2026-10-09-OBJECT-STORAGE.md). Bounded S3 core/multipart and administrator abort have newer independent acceptance receipts; large objects and physical GC remain incomplete. Native Functions deployment/invocation and manual lifecycle passed the exact 0.1.19 UI slice above; runtime monitoring and explicit retained wake have their own accepted version receipts. AI inference, HA/DR, external cross-instance fencing and whole-chain TLS remain unqualified. Historical receipts cannot replace the explicit active runtime image lock.
