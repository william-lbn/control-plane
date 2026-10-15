# 2026-10-15：项目共享查询本地交付

## 结论与版本

显式项目内共享查询已完成 Go/真实 PostgreSQL、多用户权限与实际 React UI 验收。仍是预览产品；合同、手工步骤和范围见 [SHARED-QUERIES](SHARED-QUERIES.md)。不代表执行历史、公开共享链接或完整官网 SQL Editor 已实现。

- API 0.33.0：83 paths / 122 operations；metadata migration 031。
- Chart 0.9.17；Helm revision 84；Application Auth 0.25.1（补正待验证 factor Cookie 的 Swagger 安全模型）。
- API 与 Web 源快照：`5fb08c5145695b56fdcfad9eb98f36749e53a9bccdced8a609c37d633ae16803`；父 Git e98a104，不冒充运行源码。
- API：`neon.local/control-api@sha256:5ee040473994d874280899a005740c5d9e986813de6451a50a575572a181773f`。
- Web：`neon.local/control-web@sha256:3e895032a7bcb3600deb74a699fba94e98a0ecb8afd8da807a0bf820f0120462`。
- Auth 继续使用已验收 TOTP 镜像，不重新构建或运行旧 Auth 套件。

## 新功能检查

真实临时 PostgreSQL `TestSavedQueryExplicitSharingIsolationAndRevoke` 一项完整检查通过：多作者/多组织、默认私有、共享元数据、真正 owner AAD 解密、非作者不可编辑/删除/共享、Viewer 和跨组织拒绝、幂等重放/输入冲突/版本冲突、显式 boolean 与前置条件、撤销及不改变原 SQL 密文。没有重跑旧保存查询集成测试。

新增 `TestApplicationFactorChallengeCookieContract` 一项通过；nil DB/Kubernetes 的文档路由验证所选分支 pending-factor Cookie 与会话 Cookie 的协议边界。不是重跑 MFA UI。

Linux TypeScript/Vite/Helm 检查与 34 个固定套件准入通过。实际 UI Job `publication-ui-20261015120130` 的六项检查：

- `ui_new_draft_private_and_absent_from_project_shared_view`
- `ui_author_explicit_consent_shares_with_project_sql_editors`
- `ui_project_shared_view_loads_exact_sql_clears_password_and_does_not_execute`
- `stale_sharing_generation_denied_without_overwriting_concurrent_state`
- `ui_revoke_removes_shared_listing_but_keeps_original_private_draft`
- `owned_draft_retired_with_tombstone_and_zero_sql_or_compute_activity`

保留 tombstone 查询 ID：`sq_b8b4a0a6dfec734fc6dfd7ab`。UI 执行 SQL 次数为 0；完成后的托管 compute Pod 与 VM 均为空。真实跨用户授权验证来自 PG/HTTP 路由集成测试；UI 验证是作者在实际控制台的完整共享/撤销流程，不冒充两个浏览器用户已验收。

## 部署与环境记录

Linux 本地串行构建 API/Web，三个节点精确 digest 导入；没有上传远端镜像或触发 CI。首次升级在 Helm 执行前的备份 exec 步骤失败，保留 `query-sharing-local-deploy1` 原记录与部分备份；不把它当成成功备份。经 API server 102 使用新的私有部署目录完成备份及升级，未重放已执行的 Helm 变更。没有证据将首次失败归因于资源耗尽。

成功备份 SHA256：`e5917683b01894e1af6f9d39ff6a17c1ce42bf2be97895f69425d5f9d1dedb63`。保留全部 9 个 PVC、升级前 775 个非 Helm Secret UID，其他 6 个 release 不变。

本批已归档并按 UID/ResourceVersion 前置条件退役 16 个终态测试 Job，以及三个固定 UID 的旧 TOTP 库检查 Job（含原失败检查）。完整 Job/Pod 清单和日志保留；未删除 PVC、SQL 数据、用户账户或测试记录。当前本次 UI Job 保留待下一批统一退役。

## 证据与未关闭门槛

维护者私有证据位于 `control-plane/production/evidence/20261014-auth-mail/query-sharing-*`、`auth-factor-contract-new1`、`owned-terminal-jobs-retirement-attempt5` 和 `owned-inspection-jobs-retirement2`。公开源码不包含 Python 私有编排脚本或凭据文件。

共享列表最多返回最新 50 条；团队分页、实时协作和项目执行历史尚未实现。撤销不能追回先前授权读取的副本。完整生产安全、HA/DR、跨实例缩零栅栏、可信 TLS 等门槛仍见 [PRODUCTION-GATES](PRODUCTION-GATES.md)。
