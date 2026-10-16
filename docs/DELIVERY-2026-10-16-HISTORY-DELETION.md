# 2026-10-16 当前交付：MFA、历史策略与单条删除

## 1. 结论

本地运行 API **0.40.0**、migration **035**、Chart **0.9.28**、控制面 Helm revision **95**。
本次实现并按阶段验收 Console MFA、个人 SQL 历史采集/保留策略和单条手工删除。
后端 Go、前端 React/TypeScript、元数据库 PostgreSQL；未修改 Neon Rust 源码。
Python 只在维护者私有 SSH/证据传输中使用，不进入公共仓库或产品运行路径。

| 能力 | 实现 / 验收 | 边界 |
|---|---|---|
| Console MFA | TOTP、一次性恢复码、加密绑定、密码确认、防重放、会话撤销、旧版 INSERT 保护和真实 UI/PG | 组织强制、WebAuthn/SSO、完整恢复/攻击矩阵仍缺 |
| 个人历史策略 | 采集开关、0/7/30/90 天保留、显式确认、版本/幂等、owner 隔离、到期清理与事务审计 | 默认保留全部；不清理进行中/未知结果；共享历史与人工定性仍缺 |
| 单条历史删除 | 本人终态、UI 二次确认、Cookie/CSRF、并发重试、审计原子性，Compute 0 下真实 UI 验收 | 元数据 DELETE 非安全擦盘；不撤销 SQL、不删除备份/测试证据 |

**官网全部功能和生产准入仍未完成**。未开发与部分实现/待验收分列于
[35 项需求账本](REMAINING-REQUIREMENTS-2026-10-16.md)和[生产门槛](PRODUCTION-GATES.md)。
本账本不是官网全部 SaaS 子功能的穷尽计数。详细合同见 [MFA](CONSOLE-MFA.md)、
[历史策略](SQL-HISTORY-POLICY.md)、[单条删除](SQL-HISTORY-DELETION.md)。

## 2. 当前部署与镜像来源

当前合同 **94 paths / 135 operations**。API/Worker 与 Web 均来自不可变 Linux 本地快照
`a715af6db9c2bd58c327798e2b9dafe562254c991d8366dc2f28480b53f240f5`，父 Git 为 `19558ec`；父提交不冒充运行源码。

| 组件 | 实际本地 digest |
|---|---|
| API / Worker | `sha256:3eeedafc1dec74bc1fa9c38de1893b6f10d6ada89a0ebcbe662af6979970be65` |
| Web | `sha256:72815df15d0d3e3033315d585ebcd2cdd6da032a7add7c759ff75362f94ddbf0` |

Linux BuildKit 串行构建，逐节点 checksum/digest 核对并导入 containerd。其余组件沿用各自
已接受镜像。升级前元数据库备份 SHA-256 为
`76795b4ff50ce29aef7183a33f7f58bda0f95ec8828351a5220d6019f3bce136`；9 个 PVC、779 个既有非 Helm Secret UID 和
其他六个 neon release manifest 保持。没有在 Windows 构建或执行 CI。

公开统一 Helm 0.1.31 源码候选仍锁 0.23.0 公共控制镜像；当前 0.40.0 是带本地精确
image overrides 的独立 Chart 部署。完整公开镜像发行、匿名验证和统一版本锁更新
仍是 O02，不声称默认下载旧锁即可复现最新功能。

## 3. 实际测试与失败记录

- 前一阶段：两个 MFA Go race 测试、一个个人历史策略 Go race/PG 测试；MFA 累计
  12 条与历史策略/补充因素验证 11 条 UI 账本通过。准确旧快照、原账号续测和 HTTP
  UUID 修复见[0.39 阶段回执](DELIVERY-2026-10-16-MFA-HISTORY-POLICY.md)。
- 新增删除：`TestSQLHistoryDeleteIsolationProtectionReplayAndAtomicAudit` 真实一次性
  control_ci PostgreSQL/race 通过，覆盖项目/owner/API Key/CSRF、显式确认、未结束/
  未知结果/缺回执保护、审计失败回滚、并发和重复 DELETE。vet 与三项 OpenAPI 合同
  检查通过。首轮审计字段不符合已有模型，事务回滚后修正；失败证据保留。
- TypeScript、Prettier、Vite 通过。对修改过的 API response decoder 单独执行五项测试，
  含新 204 空响应与旧的损坏 JSON/权限/请求身份保护。新增 UI suite 分页变量补全显式
  类型后通过；原类型失败保留。45 个受保护 suite 的声明/准入/文件存在检查通过。
- 实际 Linux UI Job **publication-ui-20261015195624** 五项检查通过：记录原历史集合；通过页面一次无写入
  SELECT 创建新记录；缩零后取消；确认只删除新记录且保留编辑器；重复 DELETE、
  重载及 Compute 0。新记录原始详情先保存证据，原 **10** 条历史全部保留。
  本次仅 **1** 次 SQL 请求，没有重跑旧功能全量套件。

三个阶段 UI 账本为 12 + 11 + 5，不冒充一次官网全量回归。新版本只重新验收新增路径
和修改过的共享 decoder；旧功能按其已有版本回执保留，不能声称全产品已准入。

## 4. 数据、资源与复测

本批三个终态 UI/quality Job 均先归档描述/容器日志，再按 UID/resourceVersion 退休；
一次性 PG 测试 Pod 也在各自回执中保留 UID/日志后退休。失败、源码、OCI、密码保护
输入、原测试文件和所有原业务历史保持。SQL 新记录只通过被验收的产品功能删除，
其原始内容仍在证据中。MFA/策略测试账号最终恢复 MFA 关闭、采集开启、保留全部。

最终检查 3 节点 Ready、无压力条件，87 个 Deployment/StatefulSet 达到各自期望副本，
错误 Pod 0、托管 Compute VM 0。瞬时健康不等于 HA、物理独立或长期 SLO。
私有 evidence 为 `control-plane/production/evidence/20261016-history-delete` 与前一阶段
各根目录；不进入 Git。各阶段只清理已确认归属的运行资源，不删除原始测试证据。

完整手工步骤和 API/模型见三个功能合同及 [Linux 开发流程](LOCAL-LINUX-DEVELOPMENT.md)。
只允许一次性 control_ci 做合成破坏性 Go 测试；真实浏览器使用受保护 owner fixture。
失败核对同一原请求/历史 ID，不重新建 fixture 或盲目重发 SQL。
