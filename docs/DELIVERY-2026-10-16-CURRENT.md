# 2026-10-16：Console MFA 与个人 SQL 历史策略

## 1. 交付结论

| 能力 | 已实现与实际验证 | 保留的边界 |
|---|---|---|
| Console MFA | TOTP、加密待确认、一次性恢复码、密码确认、防重放、其他会话撤销、旧版 session INSERT 保护、React UI、OpenAPI | 组织强制、WebAuthn/SSO、正式运营恢复和完整攻击矩阵未完成 |
| 个人 SQL 历史策略 | 采集开关、0/7/30/90 天期限、明确清理确认、版本/幂等、owner 隔离、有界清理、审计事务和 UI | 单条手动删除、共享历史和未知结果人工处置未完成 |

这是两个经过本地验收的功能切片。官网全部功能及生产准入仍未完成；准确待办见
[35 项源码核对账本](REMAINING-REQUIREMENTS-2026-10-16.md)和[生产门槛](PRODUCTION-GATES.md)。
产品后端为 Go、前端 React/TypeScript、元数据库 PostgreSQL；Python 只作维护者私有
SSH/证据传输，不进入公开源、产品服务或新功能运行路径。未修改 Neon Rust 源码。

## 2. 最终运行版本及来源

API **0.39.0**，94 paths / 134 operations，migration **035**，独立 Chart **0.9.27**，
控制面 Helm revision **94**。保留同一元数据库、keyring 和既有业务配置。

| 组件 | 本地源快照 SHA-256 | 实际 image digest |
|---|---|---|
| API / Worker | `062513a9bac8cd9b52bdac97f44e711df449cd662fc867f9b4b07351a8c526c7` | `sha256:e58c82edf9de5aab5c98a1c0e629ce120dca418bc44c76c47a76d245bf04209b` |
| Web | `e341c431340eae5112c8261f4b699ad533c4e347f44199692789b5c7ad4b2717` | `sha256:12c2f5739b6ad035d1e399393c1b0fc4b2b7f0fed869d6da712913f4ac7185d2` |

均为 Linux 本地 BuildKit 构建、逐节点 checksum/digest 核对与 containerd 导入；没有
通过 Windows 构建/CI。各组件保留自己的快照，不把父 Git 提交冒充运行源码。
最后一次 Web 修复只替换 Web image，API/Worker 保持其已验收 Go digest。
API 镜像补齐本项目和引入依赖的 license/NOTICE 文件。

三次部署都先备份元数据、原 values/manifest 和资源身份，再执行标准 Helm 升级。
最终升级前备份摘要 `a4e95473d7508e69c1b23551a5e435b770051fec3dccfc1c8d133eea23884ac1`。
9 个 PVC、779 个既有非 Helm Secret UID、其他六个 neon release manifest 保持。
公开统一 Helm 0.1.31 源码候选仍锁旧 0.23.0 公共控制镜像；新本地 0.39.0 镜像的
公开发布、匿名验证及统一版本锁更新属于独立发行步骤。

## 3. 测试与修复

### Go / PostgreSQL

两个 MFA race 测试（含真实 PG）和一个个人历史策略 race/PG 测试通过；vet 通过。
OpenAPI 三项路径、引用/会话合同与 operationId 检查通过。没有重跑旧业务全量套件。
TypeScript、Prettier、Vite 构建通过，Linux 文档生成确认 94/134；44 个受保护 UI
suite 的声明、准入和文件存在性校验通过。

历史策略的真实 PG 负例覆盖：默认不删、停止采集、显式期限确认、幂等冲突/版本冲突、
其他账号、进行中/结果未知/缺完成回执保护、审计失败回滚，以及并发批次不重删。
DELETE 是元数据库记录删除，WAL/备份/死元组遵循其自身保留策略，不声称安全擦盘。

### 实际 UI

MFA 原账号累计 **12 项**检查见[独立回执](DELIVERY-2026-10-16-CONSOLE-MFA.md)。
历史策略最终 Job **publication-ui-20261015192603** 又通过 **11 项**：八项个人策略/
版本/幂等/重载/空目录清理/恢复默认/运营方隔离，以及三项原账号正常再次绑定、
实际恢复码显示、启用使此前有效的其他浏览器登录后续 401，最后正常关闭因素并恢复
密码登录。后三项关闭了原 MFA 流程在首次 textarea 定位失败前未观察到的浏览器断言。
累计 **23 项**是各自检查账本之和，不冒充一次完整官网全量测试。

两类原失败都保留：测试的嵌套 textarea/select label 定位和续测页面导航已修正；
实际产品的 HTTP 浏览器 `crypto.randomUUID()` 不可用导致保存没有发出，改复用现有
Web Crypto CSPRNG `newRequestKey()`，并将生成失败纳入 UI 错误处理。只重建 Web，
沿用原账号继续验证，没有创建新账号掩盖失败。明文因素/恢复码只保留于私有输入；
涉及因素 DOM 的两个 suite 不上传公开 GitHub artifact。

## 4. 数据、资源与证据

既有历史、数据、业务目录和原始证据未删除；实际 UI 的清理只针对没有 SQL 的新测试
账号，返回 deleted_count=0。合成数据物理删除和失败回滚只发生在独立 control_ci。
测试账号最后为采集开启、保留全部、MFA 关闭；运营方 admin 未变更因素或策略。

归档 Job/Pod 描述及所有容器日志后，用 UID/resourceVersion 条件退休 **11 个**终态
运行资源（10 Job / 1 失败 Go Pod）；另两个成功 Go 测试 Pod 在各自回执中已按 UID
退休。中断过的浏览器 Pod 已由 deadline 清除时，核对其预先归档的精确 Job UID 后
才退休 Job。源码快照、OCI、私有 checkpoint、业务数据/PVC/Secrets 与失败记录保留。

最终只读检查：3 节点 Ready、无压力条件；neon/neonvm-system 的 **87 个** Deployment/
StatefulSet 达到各自期望副本数；错误 Pod 0，托管 Compute VM 0，Node100 可用约
369 GiB。该瞬时检查不代表 HA、物理故障域或长期 SLO 验收。

维护者私有 evidence：`control-plane/production/evidence/20261016-mfa`、
`20261016-history-policy`、`20261016-delivery`；全部保留，不随源码推送。

## 5. 开发者手工复测

详细接口/模型与步骤见 [MFA 合同](CONSOLE-MFA.md)、[历史策略合同](SQL-HISTORY-POLICY.md)
及 [Linux 开发流程](LOCAL-LINUX-DEVELOPMENT.md)。先在 Linux 按锁定 builder 构建并
部署本批 API/Worker/Web 和迁移，保留既有 keyring。Go 测试只允许一次性 control_ci：

```bash
cd api
# 由操作者在受保护环境配置 NEON_V2_TEST_DATABASE_URL；禁止指向业务元数据库。
go test -race ./internal/control -run '^TestConsoleMFA|^TestSQLHistoryPolicy' -count=1
go test ./internal/control -run '^TestOpenAPI' -count=1
```

浏览器依赖及真实 Chromium 使用锁定 Linux 镜像；安装依赖按项目现有 Linux 流程。
每次新建受保护 evidence/private 目录，不覆盖旧 attempt。密码由文件输入：

```bash
export NEON_E2E_BASE_URL=http://192.168.146.100:30788
export NEON_E2E_ADMIN_PASSWORD_FILE=/secure/operator/admin-password
export NEON_E2E_PRIVATE_DIR=/secure/attempt-new/private
export NEON_E2E_ARTIFACTS=/secure/attempt-new/evidence
install -d -m 0700 "$NEON_E2E_PRIVATE_DIR" "$NEON_E2E_ARTIFACTS"
npm --prefix web run test:e2e -- console-mfa.spec.ts
# 复用上一步最终关闭 MFA 的同一账号，不接触既有 SQL 历史。
export NEON_E2E_CONSOLE_ACCOUNT_FIXTURE="$NEON_E2E_PRIVATE_DIR/console-mfa-account.private.json"
npm --prefix web run test:e2e -- history-policy.spec.ts
```

保持一 worker、零自动重试。失败先看原状态/checkpoint，不能重新建账号/分支绕过。
这些自助测试不会为 admin 配置 MFA，不创建 Compute；同一账号原始输入继续保留。
