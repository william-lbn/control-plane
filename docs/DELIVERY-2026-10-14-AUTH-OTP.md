# 2026-10-14：Auth 邮箱验证码验收

## 已完成

运行源码 `5449a11a8809767aca0ea934ef94cf7d7e7db10d`，Control OpenAPI 0.23.0；新增独立应用 Auth OpenAPI。统一 Helm 0.1.31 在本地验收中，尚未打公开 release tag。8 个统一部署步骤、完整旧配置/PVC/Secret 保留审计通过。

真实 Linux Chromium UI Job `publication-ui-20261014145221`：8 组检查通过，0 自动重试。真实 SMTP 验证码创建并验证新账号，拒绝错误码与重放；已有密码账号验证邮箱，密码恢复撤销捕获的旧服务器会话/JWT 路由，旧密码拒绝、新密码可登录。原生插件独立 PostgreSQL 测试此前还验证了 5 分钟期限、3 次错误上限、摘要存储和既有未验证账号的所有权恢复。

测试操作 `op_ba0011a66f00a3eeb4261a7b`、`op_19fc84c906b422526dadb1ba`。复用已有数据库 `owner_transfer_v1` 与分支 `br_6f3c51bd15b772e3`。完成后测试 Auth 已停用、Compute 真实缩为 0；用户、邮件、SQL、凭据和 evidence 保留。

GitHub CI `38581455583` 成功，8 个公开 OCI 镜像已匿名确认。后台 3 节点全组件预拉取在第三节点 `gateway` 超时，**不宣称 24 次全部完成**；本次实际运行所需镜像已到位，部署和 UI 均通过。下一批切换 Linux 本地构建/校验/导入，无需等待远程发布。

## 证据与复测

私有 evidence：`control-plane/production/evidence/20261014-auth-mail/otp-deploy-local-attempt1` 和 `otp-ui-local-attempt1`，包含资源审计、协议结果、真实浏览器截图。口令与邮件凭据留在 Linux 私有目录，不进入开源仓库。

按 [OTP 手册](AUTH-EMAIL-OTP.md)和 [Linux 本地开发流程](LOCAL-LINUX-DEVELOPMENT.md)复测。浏览器依赖镜像与产品运行镜像分别记录，TypeScript 取验收源码的不可变快照。

## 下一批

Magic Link 免密码登录：按 [设计与验收步骤](AUTH-MAGIC-LINK.md)推进。官网全部能力和生产准入仍未达成；单项真实 UI 验收不替代多实例栅栏、HA/DR 或全链路 TLS 的独立门槛。
