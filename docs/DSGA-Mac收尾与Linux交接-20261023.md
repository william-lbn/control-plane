# DSGA 控制面 Mac 收尾与 Linux 交接（2026-10-23）

本批完成自有 main 的 `09850b56` 和 `c8767714` 两次完整同步，分别在独立同步
分支验证，再快进自有 DSGA 功能分支；不合并 main。沿用原 DSGA 创建身份、
冻结预算、凭据签发与六路由代理，保留两份原始 058 及高位前向 join。新主线
059 没有编号冲突，真实新库、旧分支来源及双次升级均由最终全量测试覆盖。

## 本批修复及兼容性

1. Mac 默认大小写不敏感，Workbench 的无扩展名组件导入先解析到
   `planComparison.ts` / `sqlAssistant.ts` 工具文件。显式使用组件 `.tsx` 后
   TypeScript/Vite 构建通过；文件名、工具 API 和生产业务逻辑不变，附中文说明。
2. MFA 集成测试原来提前生成上一时间窗验证码，真实密码校验跨窗时会正确过期。
   测试改用当前窗注册及下一窗未消费码登录；固定时间单测继续验证前/后窗、过期
   与重放。生产窗口、密码校验、原子消费与登录限流均未放宽，没有重试认证 POST。
   修改原因和边界已在测试中用中文解释。

原始失败保留在 `build/control-main098-web-build.log` 和
`build/control-plane-mainc876-auth-mac-final/`。最终成功结果来自下面的新运行，
没有删除失败断言或用一次重试替换最终全量检查。

## Mac 实际结果

| 范围 | 实际结果 | 边界 |
|---|---|---|
| 完整 API race | 1369 pass 事件：380 顶层、989 子测试；0 fail/skip | 11 测试包；9 无测试包单列 |
| 数据库与 RLS | 一次性 SCRAM PG17 准确 `0edff73d`，实际 PostgREST 16.4 | 本批 API 未冒称使用之后的 PG 维护提交 |
| API 源码绑定 | 462 份 Go/SQL/mod/sum 文件前后摘要相同 | JSON 向量保留上游原字节 |
| Auth 服务 | 13 项全部通过，0 fail/skip/cancel | 独占本地 auth_ci、真实 Better Auth 1.7.7/PG/SMTP 夹具 |
| Web | Node 24.19：43 项单测通过；TypeScript/Vite 完整构建通过 | 没有运行浏览器业务 E2E |
| 静态/构建 | 全 API vet；Linux/amd64 CGO0 全部 cmd 编译通过 | 交叉编译不等于 Linux 已运行 |

两次一次性数据库均已关闭。机器摘要与原生日志 SHA256 见
[验收摘要](acceptance/dsga-mac-close-20261023.json)。上游附带的 Linux/集群验收报告
属于其原始提交，本次没有据此声称已经在本工作区执行部署。

## 复验入口

只使用独立本地 `control_ci` 数据库及独占 schema，配置：
`NEON_V2_TEST_DATABASE_URL`、`NEON_V2_TEST_SCHEMA=v2_migration_<本次唯一标记>`、
`NEON_V2_TEST_AUTH_SCHEMA=v2_auth_ci_<本次唯一标记>`、
`NEON_V2_TEST_IDLE_SCHEMA=v2_migration_idle_ci_<本次唯一标记>`、
`NEON_DATA_API_TEST_POSTGREST=<准确16.4可执行文件>`、
`NEON_DSGA_NATIVE_PLAN_CONTRACT=<本次新契约输出文件>`。然后在 api 执行：

```sh
go test -p=2 -race -count=1 -timeout=300s ./...
go vet -p=2 ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -p=2 ./cmd/...
```

本机使用 Go 1.26.5、Xcode SDK 14.4；未改仓库声明的 Go/toolchain 版本。在
services/auth 用 Node 24.19、锁定依赖和独立本地 `auth_ci` 用户/数据库执行
`npm test`、`npm run typecheck`；必须显式设置 `AUTH_TEST_DATABASE_URL`，测试
拒绝缺失环境和不合规则的数据库。Web 执行 `npm test`、`npm run build`。

本次完整进程/清理步骤保存在工作区 `build/run-control-mainc876-auth-mac-fixed.py`
及 `build/run-control-mainc876-auth-service-mac-final.py`；原始输出在对应 report 目录。

## 整体仍缺的代码与 Linux 待验须分别管理

尚须编码/贯通：完整安全 Renderer、租户与私有管理角色/凭据及 SET ROLE 隔离、
公开创建入口与真实 worker/VM 模板；完整预算/quiesce、Monitor/Scheduler/最终
Runner fence、跨 owner/boot 关闭、多 PG 大版本及完整 Neon 存储链路。它们不能
写成“只剩 Linux 验证”，完整 V3 尚未完成，保持默认关闭。

Linux 后验包括真实 CRD/admission/RBAC/TokenReview、投影令牌与 Lease/UID 对账，
NeonVM/KVM/virtio-mem/HugeTLB、Host/Guest 物理回收、故障/迁移与长稳性能。现在
可对已交付模块分层验收；完整功能 E2E 须先补齐上述代码并复验。主仓的 V3 和
《DSGA-Mac收尾与Linux交接-20261023》统一定义整体门槛。本批不构建镜像、不准备
或执行 K8s 部署、不发布生产。
