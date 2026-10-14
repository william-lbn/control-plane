# 2026-10-14：本地构建交付与 Magic Link 验收

## 完成范围

Magic Link 已通过 Linux 真正的前端验收：UI 分支策略/有效期/注册开关、真实 SMTP 登录、HttpOnly cookie 与已验证身份、一次性重放拒绝、恶意回调拒绝、同账号再次登录、关闭新注册后未知账号拒绝和已有账号成功。测试结束后 Auth 停用、Compute 缩 0，数据/账号/邮件/凭据/证据保留。

实际浏览器 Job `publication-ui-20261014153828`，8 组检查，0 自动重试。保留原启用操作 `op_d57b0289bb0f7a1e367f0b54`；后续停用/修改策略/最终停用为 `op_2b61471925753333948ebec2`、`op_5e1fabe5791837bc58a4eff5`、`op_f55564c3aca3efbdf5c55134`。

Linux 定向 Go/OpenAPI：28 pass / 0 fail / 0 skip。真实 Auth PostgreSQL/SMTP：11 pass / 0 fail / 0 skip，另外独立 SQL TLS 检查 1 pass。前端 19 个测试、类型检查与构建、Helm 校验通过。新增应用 Auth OpenAPI 0.24.0：控制面文档与应用协议独立，文档读取不唤醒数据库。

## 本地镜像与可追溯来源

按新的开发要求，本批未提交/推送 Magic Link，未运行新的远程 CI。先 Linux 构建、Kubernetes、UI 验收；下一项对象目录/S3 兼容完成后再统一回归与提交。

| 组件 | 本地摘要 | 源码快照 SHA-256 |
|---|---|---|
| API / Worker | `de0500e9784ca9ed99952a21cd80afbd56d90ea5b53046e601326ca82ce9cee4` | `3aa8efc5382dbe8f74b6f6cefc57f5e9f5416987c675c2a319a2baf093b417e4` |
| Auth | `13ea24681adc39603c7bd4f49cb70b8516a475b78533b9e4e40887a6c55108f3` | 同上 |
| Web | `f1fc248faacb13e0515e027d7c98ed6528b165eca83424d23b07b5efcbd0203e` | `eb832c50d531144016010f9bdbdd2b71129e005f198eb4b38540d8f93945263a` |

源码父 Git 提交 `5449a11` 只用于追溯，不是未提交运行代码的真实版本。各镜像快照、文件摘要、OCI 包摘要和导入回执单独保留。其他未改组件继续保留已验收公开镜像。Neon/Postgres/autoscaling 基础镜像与上游源码未修改。

## 发现与精确修复

1. Linux 访问默认 Go 模块站失败：本地构建显式选择 HTTPS Go mirror，仍由 `go.sum` 验证依赖；公开 Dockerfile 默认镜像站保持标准。
2. containerd 的 OCI 导入保留 tag 的 index，但未自动生成期望的摘要别名：校验 tag 实际 target 后生成精确 digest alias，无需重新构建产品。
3. 首次部署将可被 Helm 清理的历史 release Secret 算作运营 Secret：保留失败记录，按前部署 Linux 私有状态逐个验证十个实际运营 Secret 的 UID/字节内容；确认只轮换 Helm 历史版本 50。后续审计排除 Helm 内部历史 Secret，记录运营身份清单。
4. 原 UI 插入产生重复配置和重复体验面板：真实浏览器检测并修复。两次失败分别发生在任何 mutation 前、已成功启用但尚未注册账号时。续测保留原邮件地址和原成功操作，未创建替代项目、账号或 Compute 来绕开问题。

最终控制面 Helm revision 72，9 个 PVC 与 768 个既有非 Helm 历史 Secret 的身份保留，其他六个已存在 release 的 manifest 完全未变。运行布局仍是预览配置；不将本地功能验收写成 HA、完整权限、TLS 或全官网生产准入。

## 复测与下一项

按 [本地 Linux 流程](LOCAL-LINUX-DEVELOPMENT.md)及 [Magic Link 合同](AUTH-MAGIC-LINK.md)复测。维护者私有证据位于 `control-plane/production/evidence/20261014-auth-mail/magic-*`；原始失败/续测、私有邮件凭据和快照不进入开源仓库。

下一项：S3 V1/V2 `Delimiter` 与目录分页、Console 目录导航、真实 SDK 与 UTF-8 验收。实现代码已加入，未通过现场验收前不计为完成。
