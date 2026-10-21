# 恢复原目录身份基础交付 · 2026-10-20

API/Worker/Web 0.61.0，AI Gateway 沿用 0.58.0，migration 050，Chart 0.9.54，
Helm 137。[受控恢复合同](CATALOG-IDENTITY-RESTORE.md)的本地功能闭环通过。
C02 的普通 CRUD/分支持续身份绑定、默认 Owner 和 Backend 依赖迁移仍须继续；
不能将本次原名称恢复算作整组完成。性能/可靠性、HA/DR、外部 fencing 专项后置。

## 实现与验证

仅对原生 tenant/timeline、原 OID、管理标记、选项和 Owner 均匹配、仅名称变化
的受管理非系统对象恢复原名称。同名新对象、目标占用和其他观察冲突拒绝。
角色单独明确确认重设为原平台受管理凭据；不会根据未观察的外部密码默默
覆盖。原不可变凭据 UID、每个保留配置 UID、原路由 UID 和期望内容均检查，
不自动改写漂移配置/路由。原数据库 OID、ACL、Owner、表所有权及数据保留。

操作保持原管理名称/版本和不可变基准。当前权限、父资源和 Writer、原意图、
活动操作及租约检查贯穿准入和提交。原结果与成功审计同事务，失败回滚；
结果已提交后只补原步骤/回执，避免旧操作对新的外部 DDL 再应用旧密码。
Console 未知响应用原内容/版本/键重试，已知操作继续观察或重试同一 ID。

四项新增 Go 顶层测试通过，覆盖确认/冲突、一次性 PG 原生改名/密码/数据、
元数据原子准入/结果/审计回滚/原结果恢复，以及真实 HTTP 客户端下的所有
保留配置/路由 UID、跨项目、Reader 凭据和内容漂移拒绝；检查过程没有写入。
补充父资源进入删除状态的检查后，仅续跑受影响的元数据测试，通过。Linux
编译/vet、API 合同、格式/TypeScript/Vite、179 个 API 操作索引和 69 个受保护
入口通过；旧业务未整体重复验证。普通 PG 模拟 GUC 不代替真实 Neon scope。

继续原测试分支 `br_7d4c985f85c13039`、原 Writer `ep_7d4c985f85c13039` 与原 Reader
`ep_702009efb2684c08`，没有重建业务夹具。八项真实检查通过：

1. `original_inventory_scope_and_replacement_rejected`
2. `explicit_credential_ack_required`
3. `same_role_original_request_recovers_known_credential`
4. `database_restoration_preserves_data_acl_and_owners`
5. `collision_and_provenance_conflicts_refuse_adoption`
6. `retained_cold_reader_uses_original_oid_credential_data`
7. `cold_writer_retains_original_oid_credential_data`
8. `original_customer_data_retained_and_computes_zero`


原角色 OID `212992`、数据库 OID `213004`
保留。测试明确改变专用角色的外部密码，再经确认恢复原凭据；原密码实际登录
成功。数据库外部 Owner 冲突先拒绝，清除该专用冲突后由 UI 恢复原名称；名称
碰撞/标记冲突也先拒绝。原生数据、数据库 ACL 和 Owner、表 OID/Owner 与
初始记录一致。原 Writer 与原 Reader 各自从零冷醒验证相同数据和原凭据。

三个成功恢复操作、三个原结果、各三条 accepted/succeeded 审计一致，三条
原基准及角色/库管理版本 2 保留。原客户数据 `retained-c02-default-data` 和
平台连接默认值版本 5 保留。同名替换负例继续保留原基准，未认领为已修复。

## 来源与保全

源码 SHA256 `f3ecef647dbfaca66a7ae09322a81f79978e50a696dc52b8853f4e9ff342203d`，归档 SHA256
`30a80b87b0d63b1d210f820d6ebcf5995ca9dcf29a55840636f01096e149239e`；父 Git HEAD `928eec95c034af8b8bd89fc1a88761d4e67ece07`
不是本次运行代码提交。API/Web 同一不可变快照，358 个运行输入逐一一致。
没有 Git/镜像推送或模型下载；复用缓存，一次构建后内网分发并在三节点校验。

| 组件 | 运行镜像 |
| --- | --- |
| API / Worker | `neon.local/control-api@sha256:163c74cc5fa70ae3770ec2fc149b68137763a2a26dd0a25b7a2ad6be0a8571a4` |
| Web | `neon.local/control-web@sha256:4642dcffc87ffa4a33fc598077283f00cc947a12b0d28785ccafdf625195e16a` |
| AI Gateway（复用） | `neon.local/control-ai@sha256:d626f14a4eb426279a36a938636023cd021efa945558ddfa77b6e740c8926fbc` |

049 保持原摘要。050 冻结 SHA256 `0413ff6666cf2c2c20b212fc8a53c34556b1547fbecaf26473e6974cbc6da189`；升级前
Linux 元数据备份 SHA256 `1f0aea9eef63b676af5625d81427f874b872a8e24b0c815678308ca47d2a8392`。
24 对象语义检查仅允许三个相关镜像与 Chart 标签变化。保留 9 个 PVC、
875 个既有 Secret UID、其他六个
Helm 发布，以及 AI 策略/挂载和 Functions 公共配置。

最终三节点、三个 Safekeeper、关键存储组件就绪，VM 0，原 Function 定义 1、
非退休实例 0。两个终态测试 Job 按原 UID/resourceVersion 归档退休，数据和
Secret 保留。这是功能时点验证，不能替代长期可靠性认证。

私有证据 `production/evidence/20261020-catalog-restore` 中保留
`accepted-functional-slice.json`、`ui-a/result.json`、`deploy-a/result.json`、
`images-a/merged-result.json`、源码匹配、最终只读核对、Go/前端检查及精确 Job
退休回执。账本仍为 13 组基础回执、其余 22 组继续。
