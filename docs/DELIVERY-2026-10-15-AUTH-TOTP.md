# 2026-10-15：应用 TOTP 本地真实 UI 交付

## 结论

新增应用 TOTP 功能通过 Linux/真实 PostgreSQL/实际 React UI 八项检查。不是控制台管理员 MFA，也不关闭全部官网或生产准入门槛。合同见 [AUTH-TOTP](AUTH-TOTP.md)。

## 精确版本

- Control API 0.32.0，82 paths / 121 operations；Application Auth 0.25.0。
- 独立 Chart 0.9.16，Helm revision 83；metadata migration 030。
- Linux 本地构建、三节点 digest 导入；没有触发远端发布或 CI。父 Git d1230bb 不代表运行源码。

| 组件 | 源码快照 SHA256 | 镜像 digest |
| --- | --- | --- |
| api | `967e1b3037eb5c23b7bb2e2f42593f13ad45dc4b24bfe95cfd4a3888ec821aa4` | `sha256:dfc0cdee0bd24e3cb3b6365836a0a52e4ae52706869cc7e9997594f916658c65` |
| web | `720cea9a4ef2320b5d2877cf23bb3bab350c09938986003ebf9f3177a3496d91` | `sha256:eabc4bd00d116239d16ad25aa8ae8a7823865ed75e7bb3088b2bf7ffc00fe768` |
| auth | `967e1b3037eb5c23b7bb2e2f42593f13ad45dc4b24bfe95cfd4a3888ec821aa4` | `sha256:da7b1f62578db97fdb107b8c93ff888049ebe7cfe2858bbbb4088e0cc020a7c5` |

## 验收与恢复记录

- Go 新增边界测试两项、真实临时 PG Driver 迁移/权限/降级/克隆保护一项通过。
- 固定 Better Auth 1.7.7 使用本仓库实际基础及增量 SQL 资产，通过一项完整真实 PG 绑定/挑战/恢复码/停用流程；0 fail / 0 skip。
- TypeScript、Vite、Helm 渲染、33 个固定 UI 套件的可执行准入检查通过。未重跑旧 799 项 Go、旧前端单测或已验收 UI。
- 首次构建在编译前因重启后的临时 BuildKit 未运行失败；保留回执后恢复 CPU 1 核、内存 2 GiB、磁盘读 20 MB/s、写 8 MB/s 限额服务，使用同一源码归档完成构建。
- 首次 SQL 资产 Provider 测试在建表前启动库实例，导致 schema 初始化缓存失败；修正测试顺序，未放宽运行时校验。
- UI 原始 Job `publication-ui-20261015114439` 已通过前四项，但 Auth 重启后认证面板相同 React key 导致重复渲染；修正各面板独立 key，只重建 Web 镜像。
- 精确续验 Job `publication-ui-20261015115003` 复用原账户、原 TOTP URI/恢复码和三个既有 Operation，从重启检查点继续，未重做前四项或新建账户。

最终八项检查：

- `ui_native_opt_in_and_branch_protocol`
- `real_ui_pending_enrollment_confirmed_by_rfc6238_totp_and_masked_material`
- `password_only_has_no_session_or_jwt_challenge_cookie_relay_and_totp_login`
- `one_time_recovery_code_establishes_original_identity`
- `service_disable_reenable_preserves_encryption_identity_and_used_recovery_code_rejected`
- `ui_retrieves_existing_binding_and_rotates_recovery_codes`
- `ui_user_disables_mfa_and_clears_visible_binding_material`
- `owned_auth_runtime_stopped_compute_zero_identity_and_evidence_retained`

最终四个 Operation 全部保留：`op_60283e45cd7d4b49fcc60232`、`op_d2d428a6c62b31f388c24109`、`op_7d2c48f2fb6b64dbd2ff864f`、`op_b99c0643d614e2bc000f727f`。

## 数据与资源

两次 Helm 升级保留全部 9 个 PVC、既有非 Helm Secret UID 和其他 6 个 release；升级前元数据备份保存在 Linux 私有交付目录。最终备份 SHA256：`9f50492f350e17baf0ccc0563577c83971931016bf177b6179077a25773b8e50`。

Auth 停用、测试用户停用自己的 MFA；托管 compute Pod/VM 均为空。用户资料、SQL、Secret、失败/成功测试记录和私密复测输入均保留，未上传公开仓库。临时输出不作为已部署基础组件删除依据。

## 复测入口与限制

使用 `web/e2e/auth-totp.spec.ts`，受信任 Linux fixture 与密码文件通过环境变量提供；单 worker、零重试。操作前确认 fixture 是授权测试项目、Auth 当前停用；遇到已执行失败只从原检查点调查/续验，不盲目新建资源。私密 URI、恢复码、密码不得写入控制台、公开日志或 Git。

本地证据目录为 `control-plane/production/evidence/20261014-auth-mail/auth-totp-*`，其中 `auth-totp-ui-local2/result.json` 是八项合并结论，`auth-totp-ui-local1` 保留首次失败。

不支持可信设备记忆、跨分支 factor 重新加密和根密钥在线轮换。Email OTP/Magic Link 不是密码登录第二因子的替代强制策略；Console MFA、OAuth/SSO、HA/DR、完整 TLS 等仍未放行。
