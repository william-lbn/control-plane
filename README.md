# Neon 自托管控制面

Go + PostgreSQL 控制 API、React + TypeScript 控制台、Go Proxy/Storage 适配器、Compute 管理网关及 Helm Chart。
许可证：Apache-2.0。项目由自托管维护者开发，与 Neon 托管服务独立。

**发行状态：预览版本。已经验证的实验环境功能不等于生产认证，也不等于 Neon 官网全部 Backend 服务。**
[2026-10-07 交付及验收边界](docs/DELIVERY-2026-10-07.md)记录邀请注册、权限撤销和资源清理。
Chart 0.5.0 提供独立 API、Worker、Web 进程及可选 Data API、应用凭据、Console 邀请注册与历史分支恢复；Worker 使用 PostgreSQL 领导租约。
当前只允许一个 API 和一个 Worker，跨实例外部 fencing 与 HA 仍须独立验收。
旧合并版本升级前必须执行 [停止/排空流程](docs/WORKER-SPLIT.md)。
运行代码与实际合同见 [OpenAPI](contracts/openapi-v1.json)；本项目使用 `/api/v1`，未声称兼容托管 Neon `/api/v2`。

## 1. 功能范围

| 功能 | 本仓库代码 | 当前边界 |
| --- | --- | --- |
| 组织、成员、项目授权、API Key | 已实现，真实 PostgreSQL 集成测试 | 加法式项目授权；完整生产安全审计、SSO/MFA 未完成 |
| Console 邀请与受邀注册 | 账号绑定、一次性凭据、幂等/过期/撤销、事务审计、Go/React/PG 测试 | 手动安全交付邀请；邮件验证/OIDC/MFA 未完成，独立于 Managed Auth |
| 项目、当前时间点分支、Writer、多个 Reader | 已实现，异步 Operation + SQL 就绪探针 | 单集群、单 Region、PG16；不支持所有项目/分支删除与恢复动作 |
| PostgreSQL 连接与 SQL 工作台 | 通过独立 Neon Proxy 和 Endpoint selector | 应用不能直接连接控制 API；生产入口必须配置可信 TLS |
| 数据库、角色与密码轮换 | 分支目录、Compute 原生配置、Proxy spec 调谐 | rename、owner 变更、未知外部 DDL 恢复未完成 |
| CPU / 内存边界、自动休眠、连接唤醒 | 已实现实验环境控制路径 | 整数 CPU 1→2→1 已验证；小数 CPU、完整内存缩回、跨实例栅栏未验收 |
| 监控、Operation 步骤、错误与重试 | 已实现 | 长期指标持久化、SLO 告警、完整审计导出未完成 |
| 时间点恢复到新分支 | 原生时间戳/LSN、存储保留租约、异步调谐、历史目录隔离、React UI | 默认禁用；仅受管理分支；原地恢复、Time Travel Assist、完整 Backend 一致性恢复未实现 |
| HA / DR | 部分租约与恢复工具 | 未通过独立故障域 HA/DR；历史新分支恢复不等于完整 DR |
| Auth、Functions、Object Storage、AI Gateway | 能力与服务模型预留，明确 disabled | 对应 Backend 服务尚未实现，不能作为可用功能宣传 |
| Data API | 原生 Go Driver、持久 Operation、React UI、分支 JWT Gateway 和固定 PostgREST 镜像 | 默认禁用；须显式 labHTTP，真实 Neon UI 验收独立于 PG CI；RPC/views 等不在首版范围 |
| 分支应用凭据 | 一次性 Token、范围/到期、轮换/撤销、哈希存储、当前权限检查和 UI | 当前仅 ai_gateway:invoke；凭据不等于 AI 推理服务可用 |

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

- [架构与模型](docs/ARCHITECTURE.md)
- [63 个实际 API 操作](docs/API.md)
- [Linux 部署与回滚](docs/DEPLOYMENT.md)
- [测试与交付标准](docs/TESTING.md)
- [Fork 来源与镜像对应关系](docs/SOURCE-PROVENANCE.md)
- [生产功能门槛](docs/PRODUCTION-GATES.md)
- [API/Worker 拆分与升级](docs/WORKER-SPLIT.md)
- [Go Proxy/Storage 适配器：合同、安全边界与升级](docs/GO-PROXY-ADAPTER.md)
- [Go 运行链路、Helm 0.1.3 和五套 Linux UI 验收](https://github.com/william-lbn/neon-helm/blob/main/docs/ACCEPTANCE-2026-10-07-GO-ADAPTER.md)
- [完整 Backend 产品实施合同](docs/FULL-PRODUCT-IMPLEMENTATION.md)
- [AI Gateway 官网核验、平台配置与动态模型设计](docs/AI-GATEWAY-PLATFORM-DESIGN.md)
- [原生 Data API 驱动与部署](docs/DATA-API-NATIVE-DRIVER.md)
- [分支应用凭据：模型、API、部署、UI 和验收](docs/BACKEND-CREDENTIALS.md)
- [控制台成员邀请、受邀注册与复测](docs/CONSOLE-INVITATIONS.md)
- [异步 Operation 观察恢复与 Linux 故障注入验收](docs/OPERATION-OBSERVATION.md)
- [历史分支恢复：API、LSN 租约、目录隔离与 UI 复测](docs/HISTORICAL-BRANCH-RESTORE.md)
- [完整产品逐项实施与交付计划](docs/PRODUCT-COMPLETION-PLAN.md)
- [2026-10-04 产品切片验收、修复与手动复测](docs/ACCEPTANCE-2026-10-04.md)
- [贡献规范](CONTRIBUTING.md) / [安全政策](SECURITY.md)

## 3. Linux 开发与 CI

固定工具链：Go 1.27.1、Node 24.19.0；Web 锁文件提交到仓库。
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

可信分支通过质量门槛后发布 `control-api`、`control-web`、`control-gateway`、`control-dataapi`、`control-postgrest`、`control-adapter`。
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
