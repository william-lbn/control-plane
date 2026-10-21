# 原目录身份与外部 DDL 观察基础交付 · 2026-10-20

API/Worker/Web 0.60.0，AI Gateway 沿用 0.58.0，migration 049，Chart 0.9.53，
Helm 136。[身份观察合同](CATALOG-IDENTITIES.md)的本地功能闭环通过。C02 仍有
受控修复、普通 CRUD/分支的持续身份绑定、默认 Owner 和 Backend 依赖迁移，
不能将本次观察路径视为整组完成。性能、可靠性、HA/DR、外部 fencing 专项后置。

## 实现与验证

完整有界只读目录核对真实 tenant/timeline，按原 OID 区分改名、同名替换、缺失、
名称占用、角色选项、管理标记、超级用户、数据库选项、Owner OID 及 Owner 改名。
NOLOGIN 角色和禁止连接/模板数据库也纳入观察；不查询用户表、库大小或客户
密码/verifier。所有结果明确标记 `credentials:not_observed`。

Admin 显式确认仅从当前时点记录旧对象原身份。规范原版本、幂等键、原不可变
凭据 UID、当前权限、父资源与活动操作重查，和审计/原响应在一个事务内提交。
同名或同 OID 基准不能覆盖；Writer 退休后原回执仍能重放。平台后续意图版本
变化另报 `intent_changed`，不会自动改写历史或误称外部 DDL。读取不执行修复，
旧 CRUD 暂未增加新的外部身份栅栏。

新增五个 Go 顶层测试通过：稳定 OID/冲突分类、完整有界/私有标记、一次性 PG
真实 DDL、首次可信观察条件，以及持久事务/权限/版本/审计回滚/HTTP 重放。
普通 PG 的自定义 GUC 不代替真实 Neon；现场另验原生 scope 和真实 DDL。
Linux 编译/vet、API 合同、格式、TypeScript、Vite、178 个 API 操作索引及
68 个受保护测试入口通过，没有重跑无关的旧业务验证。首次编译发现测试文件
多余导入，修正后通过；原失败回执保留，没有遗漏业务提交或重建夹具。

保留测试分支 `br_7d4c985f85c13039` 中新建专用角色和库，完成十项真实检查：

1. `owned_native_objects_ready`
2. `original_identity_receipt_replayed_in_console`
3. `database_and_replacement_baselines_retained`
4. `same_oid_rename_and_owner_role_rename_detected`
5. `nologin_and_nonconnectable_database_observed`
6. `external_owner_change_preserved_native_data`
7. `same_name_collision_does_not_replace_original_identity`
8. `drop_recreate_with_copied_marker_detected_by_oid`
9. `page_reload_retains_original_baselines_and_customer_data`
10. `original_writer_returned_to_zero`


角色 `c02_identity_1792496820961` 原 OID `212992`、测试数据库
`c02_identity_1792496820961_db` 原 OID `213004` 与测试数据保留。
独立替换角色的原 OID `213000` 已刻意外部删除并
同名新建，复制管理标记仍被识别为替换；其原基准保留，用于下一步负例，未冒充
已修复。名称冲突测试仅删除该测试自己新建的无依赖碰撞角色，再恢复原角色名。
客户原角色/库和 `retained-c02-default-data` 保留；客户连接默认值仍为平台版本 5。
三条不可变基准、三条原请求回执和三条成功审计一致。页面重载保留原记录。

## 来源与保全

源码 SHA256 `1e228cd7fab15de53c42d21e90c79a3837c4870a93172e30ae3df8c85fd55b89`；归档 SHA256
`acd992dd6931d5e7d995f54237311bd83f79bd94dd90014988f7b953fe9f3a0d`；父 Git HEAD `928eec95c034af8b8bd89fc1a88761d4e67ece07`
不是这次运行源码提交。API/Web 来自同一不可变快照，355 个运行输入逐一匹配。
本次未提交/推送 Git 或镜像，没有模型下载；复用缓存，镜像构建一次后内网分发
并在三节点导入校验。

| 组件 | 运行镜像 |
| --- | --- |
| API / Worker | `neon.local/control-api@sha256:33ffd8bc7525767ead6f775db579f3f7f21cf5c59d9e8da1ea309cf78f0e6ea7` |
| Web | `neon.local/control-web@sha256:aee83e58041d6960329540c04dea4eccaf44ca4e15076553facd1600c21d8682` |
| AI Gateway（复用） | `neon.local/control-ai@sha256:d626f14a4eb426279a36a938636023cd021efa945558ddfa77b6e740c8926fbc` |

已应用 048 保持原摘要，049 冻结 SHA256 `40e909c9c7a2c3fa1c834bc9c7f4d976c964a9c2f12b7af6f4f8618c1f39e57d`。
升级前 Linux 元数据备份 SHA256 `77bed823c0982d72ac07710aec77912ba7f51d00ab5b26b8564ac6e60e8e5c8f`。
24 对象语义检查仅允许 API/Worker/Web 镜像与 Chart 标签变化；保留 9 个 PVC、
872 个既有 Secret UID，以及其他 6 个
Helm 发布。AI 供应商策略/挂载、Functions 公共配置和原 Function 定义保留。

最终三节点、三个 Safekeeper 和关键存储组件就绪，VM 0，原 Function 定义 1、
非退休实例 0。两个终态测试 Job 按原 UID/resourceVersion 归档退休，不删除
业务数据或 Secret。这是功能时点验证，不代表长期可靠性认证。

私有证据：`production/evidence/20261020-catalog-inventory` 中的
`accepted-functional-slice.json`、`ui-a/result.json`、`deploy-a/result.json`、
`images-a/merged-result.json`、`candidate-source-match-a/result.json`、
`final-readonly-proof-a/result.json`、Go/质量检查及精确 Job 退休回执。

账本仍为 13 组基础回执、其余 22 组继续。下一步受控原 OID 修复，之后完成
持续身份绑定、默认库 Owner 与 Backend 依赖迁移。
