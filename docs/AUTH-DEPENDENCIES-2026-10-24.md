# 2026-10-24 Auth 依赖修复

组织管理提交 `1f86fa3275706483f31d8d976ccedfb1510dea65` 的 GitHub Auth 检查停在 `npm audit`，尚未执行该 Job 的业务测试。上游于 2026-10-23 更新的 [Zod 公告 GHSA-mrhx-c7xc-m3w5](https://github.com/advisories/GHSA-mrhx-c7xc-m3w5)影响 `<4.6.6`；[Nodemailer 公告](https://github.com/nodemailer/nodemailer/security/advisories/GHSA-8c8m-p3m8-cjg8)及同批两个地址解析公告的修复版本为 10.1.4。

本次仅把 Nodemailer 从 10.1.0 固定到 10.1.4，并用 override 将传递依赖 Zod 从 4.6.5 固定到 4.6.6。Better Auth 1.7.7、pg 8.23.1 及其余锁定包保持。没有降低审计门槛或执行 `audit fix --force`。

Linux 使用原依赖缓存，只下载两个变更包。审计报告零漏洞，TypeScript 和独立真实 PostgreSQL/SMTP 的 17 个 Auth 功能组全部通过，零跳过；覆盖账号/会话、TLS、密码/邮件、OTP/Magic Link、TOTP、Admin、组织权限与原子准入、邀请邮件失败回滚。由于校验器和邮件依赖发生变化，本次重跑这些受影响用例；原 9 个组织实机检查点保留，不重复新建分支或账号。

组织管理的实机回执仍对应其原不可变镜像，见[交付](DELIVERY-2026-10-24-AUTH-ORGANIZATIONS.md)。本补丁的运行镜像分发、部署和 GitHub 检查另行记录；不能把源码依赖修复等同于旧镜像已经更新。全项目的剩余功能统计不变，性能和可靠性专项继续后置。
