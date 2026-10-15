# 2026-10-15：个人 SQL 执行历史本地交付

## 结论

可选加密个人执行历史已完成 Go/真实 PostgreSQL、Helm 安全前置与实际 React UI 验收。它是个人历史，不是官网项目内所有成员共享的执行历史；完整范围、模型、API、状态机和手工步骤见 [SQL-QUERY-HISTORY](SQL-QUERY-HISTORY.md)。

- API 0.34.0：85 paths / 124 operations；migration 032。
- Chart 0.9.18；Helm revision 85；Application Auth 继续 0.25.1。
- API/Web 运行源快照：`640e384b0b54acf61bc665bcad946d1284eb0db8b4d6216104dac05755c4180b`，父 Git 70a8c94。
- API 镜像：`neon.local/control-api@sha256:10d30044f4cf9f8276140ecf1fe9fced6267b9a46527cc74002211030173cacb`。
- Web 镜像：`neon.local/control-web@sha256:65c6cb63cd7972506b118c37a87088d31bf226dbc53e34cc7cc7aa0c60bd48cd`。
- Auth 与其他组件沿用各自已经验收的镜像和来源，不虚称所有镜像本次重建。

## 定向检查

- 新 Go/临时 PG 两项测试通过：真实路由多用户隔离、密文/AAD、旧密钥读取、keyset 分页、完成只写一次、超期 admitted 不伪造结果、关闭记录兼容旧执行路径、Unicode 与失败阶段分类。
- 首次新测试因误用已有错误类型字段名在 vet 阶段失败，修正为 cause 后通过；原失败回执保留，不是运行时 SQL 错误。
- 新增 Linux Helm 测试一项通过：默认关闭、缺密钥环拒绝启用、有保留密钥配置可渲染；未运行旧 Helm 单测。
- TypeScript/Vite/Chart 与 35 个固定 UI 套件准入通过。未重跑旧 799 Go、旧前端单测或旧 SQL/草稿/共享 UI。

真实 Linux UI Job `publication-ui-20261015122050`，实际提交三个新 SQL 请求（不是复测原 SQL parser）：

- `ui_real_single_query_durably_recorded_completed_with_metadata_only_list`
- `ui_real_script_sql_failure_is_recorded_without_false_success_or_replay`
- `ui_utf8_history_truncation_is_visible_loads_only_recorded_text_and_clears_password`
- `ui_history_survives_reload_and_loads_at_compute_zero_without_sql_submission`

原历史 ID 全部保留：`qh_3a81a17ba351fd68a305bb02`、`qh_f96bc70e51b06a670c331e58`、`qh_6755bb259a817e02f8227323`。

分别记录单条成功、脚本 SQLSTATE 42601 失败、Unicode 超长查询成功与显式截断。缩零后刷新及加载原历史，没有再发送 SQL；最终托管 compute Pod/VM 均为空，三个历史记录和原数据保留。

## 部署与安全边界

仅 Linux 本地串行构建 API/Web，并在三节点导入精确 digest；未推送远端或触发 CI。实验环境明确设置 sqlHistory.enabled=true；开源 Chart 默认关闭，必须先配置版本化加密密钥环再启用。未改变默认项目数据库访问策略。

升级前备份 SHA256：`809fe3ec4444fd479008dbdfd4b8adca2bb3ae898854f4b3e4f38f52cb15ad80`；9 个 PVC、775 个既有非 Helm Secret UID 和其他 6 个 release 保持。UI 脚本不输出密码、SQL 正文、密钥、结果行或连接 URI 到公开报告。截图使用无敏感内容的固定测试 SQL。

构建完成后另增加一个纯 Helm 测试文件并在 Linux 单独运行；它不是运行二进制变更，不冒充构建源快照已包含后续文档/测试文件。

维护者证据：`control-plane/production/evidence/20261014-auth-mail/sql-history-*`。个人历史、项目共享查询与手工草稿各自分离；生产保留策略、物理清除、项目级共享历史、AI 命名、全链路 TLS、HA/DR、跨实例栅栏等仍按独立门槛推进。
