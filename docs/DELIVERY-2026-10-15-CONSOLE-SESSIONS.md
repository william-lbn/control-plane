# 2026-10-15：控制台账号会话管理本地交付

## 结论与版本

Go/PostgreSQL API、forward-only migration、React UI 与真实浏览器隔离验收已完成。模型、接口、事务流程和复测步骤见 [CONSOLE-SESSIONS](CONSOLE-SESSIONS.md)。这是自己的 Console 账号安全功能，不冒充应用 Better Auth 或 Neon 托管后台内部实现，也不关闭 Console MFA/SSO 门槛。

- API 0.37.0，88 paths / 127 operations；migration 033；Chart 0.9.23；Helm revision 89→90。
- 最终 API/Worker 源 b72793b807ff1193ec4ff6a45b9195372956580d35cc3f57a3827b3ebf0cd697，镜像 neon.local/control-api@sha256:b548609ba3b0d61177ebaee9a7cb8a4f2bb4a11ae828c00efbf7daaa1480f2de。
- Web 源 96a84e9289cf0684cb87876a3950e44b456c2d872ef7b5a33bc7c6870137a12c，镜像 neon.local/control-web@sha256:ea9313c08db24953716aa41ec559e0b5eb3356e9db2d0f1e9e55a10309d5276f。
- Auth 保持已验收的 967e1b3 源；其余组件各自保留精确来源。API 和 Web 的不同快照明确记录，不用 Git 父提交冒充运行源。

## 新增验证

两项新 Go/实际 PostgreSQL 检查通过：账号隔离、过期过滤、随机 public ID、CSRF、密码确认、单个/其他会话撤销、原 Cookie 后续拒绝、幂等时间/审计与限流；API Key 身份拒绝。部署前评审发现旧 API 不认识 revoked_at，补充同时缩短 expiry，并仅定向再验一项实际 PG 测试，证明旧读取条件也不能复活已撤销 token。第一个候选 API 未部署。没有为了该修复重建未变化的 Web。

TypeScript/Vite/Helm 与 41 个固定 suite 准入通过，未重跑旧全量 Go/应用 Auth/SQL/Storage UI。公开仓库没有 Python 文件；私有编排器仅负责 SSH、镜像/回执传输。

真实 Linux UI Job **publication-ui-20261015150038**，5 项检查通过：

- isolated_viewer_account_has_three_sessions_without_credential_hash_or_cross_account_data
- ui_wrong_current_password_rejected_target_session_remains_valid_and_password_clears
- ui_revokes_selected_own_session_and_next_request_is_unauthenticated
- ui_revokes_other_sessions_retains_current_and_does_not_revoke_operator_account
- owned_test_current_session_logged_out_account_audit_and_evidence_retained

独立 Viewer 的组织 org_f73f991f3a4f5e739a898330、邀请 inv_8890021a235d03b22a4aaaba 与三个 public session ID 保留在原回执。没有撤销运营方账号的任何登录。一个指定登录和另一个登录的原 Cookie 后续访问确实返回 401；当前测试登录保留到最后再正常注销。密码确认失败未改变目标会话；UI 清除密码。账号、审计与私有复测凭据保留。

## 部署与资源

Linux 本地串行构建及三节点精确 digest 导入后部署；无远程 CI/镜像推送。升级前 metadata 备份 SHA256 e482c4431e5797d44f98e2eb1c3d03fbf40bb37807b930997c45eeb5e9115cbc；9 个 PVC、779 个既有非 Helm Secret UID 和其他 6 个 release 保持。原登录有效期在迁移中没有重置，实际管理员当前登录在新版本仍有效。

UI 切片不创建 Compute、不发送 SQL，结束托管 Compute Pod/VM 均为空。之前已归档并精确 UID 退休 40 个本批终态测试 Job，数据/Secrets/证据未删。本切片新临时 Job 按同样规则归档后退休。

最初一次合并命令在候选导入未完成时被成功状态前置检查拒绝，未执行部署，原错误输出保留；真正升级使用最终修正版及独立回执。

## 未关闭门槛

本能力撤销后续 Console 请求，不取消已经受理的 SQL/Operation/长连接，不撤销 API Key、数据库密码或 Managed Auth session。尚无完整 Console MFA/SSO、设备身份/风险检测和外部安全评审；跨实例数据面栅栏、HA/DR、全链路 TLS、multipart/大对象、Functions WS/触发器等仍单独推进，不能宣称官网全部能力完成或生产准入。

维护者证据：control-plane/production/evidence/20261014-auth-mail/console-sessions-*。API 合同、迁移和 UI 测试均已进入公开源码目录，私有证据不上传 Git。
