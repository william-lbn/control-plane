# 2026-10-15：本批最终交付状态

## 已完成并实际验证

| 功能切片 | 新实际 UI/SDK 检查 | 精确回执 |
| --- | --- | --- |
| 小数逻辑 CPU、实际 QEMU quota、Reader SQL/冷醒、原生新建准入 | 9 | [资源交付](DELIVERY-2026-10-15-FRACTIONAL-CPU.md) |
| S3 COPY 源条件、优先级、失败无目标及签名负例 | 16 | [S3 交付](DELIVERY-2026-10-15-COPY-CONDITIONS.md) |
| XLSX 下载与基础 Explain/Analyze 计划树 | 8 | [SQL 交付](DELIVERY-2026-10-15-SQL-EXPORT-PLAN.md) |
| Console 自己的会话列表/撤销、密码/隔离、后续 401 | 5 | [会话交付](DELIVERY-2026-10-15-CONSOLE-SESSIONS.md) |
| 会话错误→成功提示与测试登录正常收尾 | 2 | 本节下文 |

共 **40 项**，包含各自原 fixture 的连续验收，不是同一次全量套件。此前 TOTP/共享查询/个人历史等沿用独立记录；没有重新运行旧全量 Go/UI，未使用硬件压力测试。公开 API/Worker 是 Go，Web 为 React/TypeScript，产品元数据库 PostgreSQL；私有 Python 仅做部署/证据传输，没有进入公共源码。

## 最终运行版本与源码

- API 0.37.0，88 paths / 127 operations；migration 033；Chart 0.9.24；Control Plane Helm revision 91，NeonVM revision 53。
- API/Worker 保持会话撤销回滚安全版源 b72793b807ff1193ec4ff6a45b9195372956580d35cc3f57a3827b3ebf0cd697。
- Web 最终源 af85f1ea13008335bc7a896fba0d1b4bdd673ab1d98c03e6496644d580eae32c，镜像 neon.local/control-web@sha256:486a77d1fe8b8e2b5ab49f2ea68b07d1aa5c879f1bba7716904d6e838098a8d9。
- Auth 保持 967e1b3；其他组件沿用各自已验证 source/digest。autoscaling fork 的 runner 修复本地提交 1720924，未修改 Neon Rust。

本地构建、逐节点 digest 校验/导入、只升级受影响组件。尚未远程推送或启动本批远程 CI，未把临时 neon.local 地址写入公开 profiles。公开统一 Helm 发行仍为 0.1.30；仓库 0.1.31 为未完成发行批次。各源码仓库有本地提交，正式公开镜像/锁和发行必须在后续发布门槛完成后同步。

## 最终 UI 提示修正

截图发现早先错密码的全局 toast 在成功后仍保留，改为面板内的错误/成功状态与有间距的输入布局。Linux UI Job **publication-ui-20261015151031** 复用原测试账号，仅创建两个必要的新登录，不重新邀请注册或重跑原五项流程。错密码的原登录仍有效，随后正确确认得到成功提示、错误消失、密码清空，另一登录的后续访问 401，当前登录最后正常注销。两项检查通过，无 Compute/SQL。

Web-only Helm 升级备份 SHA256 c6261703ea6490d9603c8f8b5ef99369a113e182399f165414c2550add571ae9；9 个 PVC、779 个既有非 Helm Secret UID 和其他六个 neon release 清单保持。修正后的截图保留，原错误显示截图也保留。

## 资源与资料保留

归档原 Job/Pod 描述和所有容器日志后，用 UID/resourceVersion 前置条件退休 **46 个**本批终态临时 Job（40+4+2）。原测试记录、数据、PVC、Secrets、源码快照、OCI 和密码保护复测输入未删除。最后托管 Compute Pod/VM 均为空。

只读健康检查：3 个节点 Ready 且无压力条件；neon/neonvm-system 的 87 个 Deployment/StatefulSet 均达到各自期望副本数（包括按计划为 0 的测试服务），这两个命名空间没有 Failed/CrashLoop/ImagePull 错误 Pod。Node100 容器目录可用约 369.4 GiB。该瞬时结果不是长期 SLO/HA 认证。

早期 Functions 基础层文档补充了明确的当前状态入口，避免把历史“尚未集成”当作当前事实；历史过程和失败记录继续保留。最新 API、合同与 [生产门槛](PRODUCTION-GATES.md)为当前开发依据。

## 尚未完成

Functions WS/触发器/完整日志计量、S3 multipart/大对象/物理 GC、Console MFA/SSO/完整 Auth 迁移、严格 Guest CPU 计量隔离/完整物理内存归还、跨实例数据面栅栏、HA/DR/全链路可信 TLS 等仍有工程或验收缺口。不得把上述 40 项等同于官网全部功能或生产准入，具体功能边界见 [生产门槛](PRODUCTION-GATES.md)和[完整实施计划](PRODUCT-COMPLETION-PLAN.md)。
