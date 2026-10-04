# 贡献规范

## 开发原则

1. 一个改动描述一个问题，先记录实际日志、固定镜像/源码和复现步骤。
2. 明确区分本控制面、Neon 数据面、Autoscaling、PostgreSQL 的代码归属。
3. 数据面 Bug 修复只在对应 fork 的分支提交；不能把验证脚本当作数据面修复。
4. 不改变公开 API 语义而不更新 OpenAPI、模型和兼容说明。
5. 新增迁移文件采用递增序号；已应用的 migrations 不得修改或重新编号。
6. 密码、密钥、真实 DSN、身份 fixture、测试记录和备份不能进入 Git。
7. 源码、构建成功、实验环境验收与生产放行是不同的证据。

## Linux 本地验收

使用 README 的固定工具链及专用 PostgreSQL，执行 `make test web helm`。
格式化：`make fmt`、`cd web && npm run format`。
前端 TypeScript 开启 strict；后端必须通过 vet/race 和真实 PG 集成测试。
提交新 route 时同时更新合同；Go AST 测试拒绝不匹配。

真实 live UI 验收只在受信任 Linux 环境执行，使用个人授权的测试账号。
不得让来自 fork 的未审核 PR 获取集群/注册表/真实账户 Secrets。

## 提交与评审

- 从明确的基线创建描述性分支。
- 提交说明包含动机、代码影响、测试命令/环境/结果和尚未通过的门槛。
- 使用 `git commit -s` 提供 Developer Certificate of Origin sign-off：
  贡献者确认有权按项目许可证提交这份改动。
- PR 必须通过 Linux CI；破坏性数据库升级需要恢复演练和维护窗口。
- 禁止 force-push 共享发行分支、移动已发行 tag、覆盖历史验收记录。

依赖版本及 Actions 更新由 Dependabot 提议并正常评审。
