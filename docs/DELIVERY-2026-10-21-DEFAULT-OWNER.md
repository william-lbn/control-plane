# 原默认数据库 Owner 与原失败分支恢复 · 2026-10-21

API/Worker/Web 0.63.0，AI Gateway 沿用 0.58.0，migration 052，Chart 0.9.57，
Helm 140。[默认 Owner 合同](DEFAULT-DATABASE-OWNER.md)与
[已知继承身份替换处理](CATALOG-FORK-EXCLUSIONS.md)的本地功能闭环通过。
普通 CRUD 的持续 OID/凭据代际、完整默认/系统身份漂移及 Backend 名称依赖迁移
仍属于 C02 余项。性能、可靠性、HA/DR、外部 fencing 专项后置。

## 行为与原结果

Admin 通过 Console 显式读取原默认数据库 OID/Owner，确认迁移并提交当前平台
密码、原版本和幂等键。操作绑定原 tenant/timeline、数据库/角色 OID、原凭据
UID、所有保留配置 UID 和原路由 UID；密码仅作认证、请求摘要和用途隔离 AEAD
封存。原生事务只改变数据库 Owner；保留数据库、Schema/表 OID、表所有权、
数据、任意数据库注释和原选项。PG 会按 Owner 迁移规则改写数据库 ACL 中旧
Owner 的 grantee/grantor 引用并合并权限，未声称 ACL 文本逐字不变。

先更新 Reader 冷配置，再更新 Writer 冷配置，不执行全量 configure。SQL 已
达到严格目标状态时可以只读确认；结果、绑定版本、成功审计与封存凭据清除在
同一元数据事务提交。旧结果恢复不重复 SQL，旧 HTTP 请求重放不会撤销之后
的授权 Owner。当前 Owner 角色的删除/改名和原默认库的普通修改被阻止。

0.62.0 曾因 Worker 缺少已有 Backend 密钥只读挂载，在原 SQL 迁移前失败。
0.62.1 补齐与 API 相同的保留 Secret 挂载，没有替换密钥或创建替代请求；
原操作 `op_dfc898d8fe0cff271301219e` 恢复成功。该故障及原封存输入留证。

原 live fork `br_8db8bf35fb79dffd` 的原操作
`op_8b0a0c35548a52221c6f4641` 随后因继承了已知同名替换角色失败。0.63.0
依据原 049 基准 OID 213000 与实际 OID 213013，保存不可变的继承排除回执；
不认领该角色、不更改其原生注释、不授予探针权限，也不删除原生角色或数据。
仅按原 UID/CAS 移除子分支该角色的旧配置/Proxy 凭据意图，原凭据 Secret、
源分支基准、其他路由和原 timeline 均保留。原失败操作已用同一 ID 恢复成功。

## 验证

累计八项新增 Go 顶层测试覆盖 Owner 密文用途/范围、PG ACL 变换、真实 PG
迁移/数据/选项/低权限目标确认、原子元数据/封存/回执/审计恢复，以及真实
HTTP 下的配置 CAS/丢响应/换 UID 拒绝；继承替换识别、禁止原生 DDL 的精确
配置过滤、原租约/不可变历史/审计回滚也通过。变化后只续跑受影响测试。
Go 编译/vet、格式/TypeScript/Vite、181 个 API 操作索引和 70 个受保护测试
入口检查通过。普通 PG 的模拟 GUC 测试不代替真实 Neon scope。

继续同一原 C02 分支、原 Writer/Reader 与受保护 Linux 输入，累计检查点：

1. `original_default_identity_and_input_guards`
2. `durable_ui_original_request_and_native_owner`
3. `default_owner_dependencies_and_independent_connection_defaults`
4. `cold_reader_preserves_original_oid_data_and_owners`
5. `cold_writer_preserves_original_oid_data_and_owners`
6. `live_and_historical_fork_owner_intent`
7. `platform_restore_and_old_receipt_do_not_revert_new_owner`
8. `original_customer_data_retained_and_all_computes_zero`


原 `postgres` OID 5 从 `cloud_admin` OID 10 迁移到受管理角色 OID 212992，
又恢复原 Owner。父分支最终 platform/version 3；两个原结果、各两条
accepted/succeeded 审计一致，两份密码封存均清除。live fork 保留迁移后的
Owner OID，使用不同 timeline；historical fork 保留迁移前原 Owner，未复制
当前 Owner 绑定。复制的外部角色仍为 OID 213013，保留源分支注释，五种管理
修改与新基准初始化均拒绝，UI 显示保留的外部角色并禁用删除。

原客户连接默认值 platform/version 5、`retained-c02-default-data` 与 Owner
测试数据保留，根业务分支目录计数及原 Function 定义不变。最终三节点、三个
Safekeeper、关键存储组件和控制面就绪，VM 0，Function 非退休实例 0。
八个终态 UI/质量 Job 已按原 UID/resourceVersion 归档退休；测试 Pod 也有各自
精确退休回执。没有删除业务数据或 Secret。

## 来源与部署保全

源码 SHA256 `9788162381c8a06d80dc9e685318072bab14334cc7ee652bf5d78a6a3604d317`，归档 SHA256
`53eb88a45bd06ea9e0ae1323bdef1c116a83905173854ec8cac70ec1b41dd7ea`；Git 父 HEAD `928eec95c034af8b8bd89fc1a88761d4e67ece07`
不是本次运行源码提交。API/Web 来自同一不可变快照，372 个
运行输入逐一一致。缓存构建一次，内网分发到三节点并校验，无镜像/Git 推送，
无模型下载。

| 组件 | 运行镜像 |
| --- | --- |
| API / Worker | `neon.local/control-api@sha256:0607e44234ba88f257495f8f917607b5a6ac37ab29477eaa6c6c2ec1ca598f24` |
| Web | `neon.local/control-web@sha256:c7501e2107b4a7b005ed69ebd6d93498252887fd7217dddcb5d6f839d753dba0` |
| AI Gateway（复用） | `neon.local/control-ai@sha256:d626f14a4eb426279a36a938636023cd021efa945558ddfa77b6e740c8926fbc` |

049/050 保持原摘要。051 SHA256 `da3f58293c41f3f7fd34ec06601a02253540eca8f604c7456f1169dc712ef45b`；
052 SHA256 `49bd101e82bb4ef5f65eed587c5a9b32d531afa82e2b8620cfcfe11bc81080fa`，均先在一次性元数据库验证并冻结。
本次升级前 Linux 元数据备份 SHA256 `e11ba5d78064ecc88fc1ba6b69c4289b7f442c026790b671f4f9b9b5b08032c7`。
24 对象语义核对仅允许相关镜像和 Chart 标签变化；保留 9 个 PVC、
879 个既有 Secret UID、其他六个 Helm
发布、AI 配置及 Functions 公共配置。之前 0.62.1 的 Worker 密钥挂载有独立
限定变更核对和回执，本次没有新增挂载或 RBAC。

私有证据位于 `production/evidence/20261021-catalog-fork-exclusions`：
`accepted-functional-slice.json` 汇总跨 0.62/0.63 原夹具的真实检查、Go 回执及
运行来源；另有 `ui-e/result.json`、`final-readonly-proof-a/result.json`、
`images-a/merged-result.json`、部署、源码匹配、质量和精确 Job 退休回执。
前期失败保留在 `20261020-default-owner`。密码文件留在 Linux 保护目录，未
下载进公开或本地验收结果。账本仍为 13 组基础回执、22 组继续，C02 未关闭。
