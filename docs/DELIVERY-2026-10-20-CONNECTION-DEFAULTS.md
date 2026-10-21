# 客户连接默认身份基础交付 · 2026-10-20

C02 的连接默认身份路径通过本地功能验收。API/Worker/Web 0.59.1，独立 AI
Gateway 沿用 0.58.0，migration 048，Chart 0.9.52，Helm 135。C02 的默认数据库
Owner、持久外部 DDL 对账/修复及 Backend 名称依赖迁移尚未完成；性能、压力、
HA/DR 和长期可靠性专项按用户要求后置。

## 实现与验证

[合同](CONNECTION-DEFAULTS.md)覆盖 Admin 选择客户默认角色/数据库、保留平台身份、
原请求回执、版本冲突、当前权限及目录作用域重查、审计回滚、默认依赖保护、
Writer/Reader 和当前时点分支继承。目录记住当前标签页所选分支；不保存凭据或
待提交请求。新 Endpoint 的原库可以是 postgres，原始角色/库名由初始化配置
投影，不能写死为 neondb。选择原始 postgres 仅影响连接默认值，系统库的删除、
改名、认领保护保留。历史原回执维持原表示，新投影补充原平台名称。

连接串、SQL 单条/脚本省略目标、编辑器使用同一默认身份；仍须输入所选角色的
现有密码。原平台 cloud_admin/control_probe、初始化 Secret、已有会话和 Backend
身份保持各自职责。Owner、ACL、表/Schema 所有权和业务数据不会随选择迁移。

Linux Go/一次性 PostgreSQL 验证三个新顶层测试及受影响的 C01 准入；最后对
原始 postgres、权限/标记/真实 OID、原子提交、当前授权、审计回滚、原回执及
Writer 退休回放再次通过。普通 PostgreSQL 的模拟 GUC 不替代真实 Neon；实际
tenant/timeline 在下述真实路径中另验。格式、TypeScript、Vite、176 个 API 操作
合同及 67 个受保护 Linux 测试入口通过；没有重跑无关的旧业务验证。

保留项目 `prj_6f3c51bd15b772e3` 下，测试分支 `br_7d4c985f85c13039`、Writer
`ep_7d4c985f85c13039`、Reader `ep_702009efb2684c08`、子分支 `br_5341f9e982844f67`
完成九项真实检查：

1. `owned_native_catalog_ready`
2. `lost_ack_replayed_original_customer_defaults`
3. `connection_info_sql_and_script_use_customer_identity`
4. `default_dependencies_reject_delete_and_rename`
5. `retained_reader_uses_same_oid_password_and_defaults`
6. `live_fork_inherits_customer_defaults_and_native_oids`
7. `managed_role_can_keep_original_platform_database`
8. `platform_restore_keeps_native_data_and_bootstrap_identity`
9. `original_parent_catalog_wake_returned_to_zero`


原角色 OID `196608`、数据库 OID
`196612`、密码和数据保留。先前平台恢复请求 version 3
按原记录保留；新增“保留原库、只换客户角色”到 version 4，最后平台恢复到
version 5。子分支保留 managed/version 2；共四条原回执和四条成功审计。
原业务分支的目录、原始默认身份和已有 Function 保留。

## 来源与保全

不可变源码 SHA256 `e9da3524d4bbfcc1e244e20c814dc7c0b29d54ba3246e05e8aab23a6db93a654`，归档 SHA256
`f522f5c962478326bc2858ce9631a255dfc25dc9376b2ec75a4c1c924dea6fd6`；父 Git HEAD `928eec95c034af8b8bd89fc1a88761d4e67ece07`
不是本次运行源码提交。本次未提交/推送代码或推送镜像。

| 组件 | 运行镜像 |
| --- | --- |
| API / Worker | `neon.local/control-api@sha256:8b6e8503d3e0f7e828f54eeb59994efe961e24cbe7db240ba8ad078aa14251e9` |
| Web | `neon.local/control-web@sha256:de543692818cad7c16e2f0407eac00a5ddcd42d98905d9f2368a44ff056f554a` |
| AI Gateway（复用） | `neon.local/control-ai@sha256:d626f14a4eb426279a36a938636023cd021efa945558ddfa77b6e740c8926fbc` |

API/Web 来自同一 images-d 快照，生产输入与该快照逐一一致；未修改已应用的
045/046/047/048。048 冻结 SHA256 `28b9e4b8f052079c7cb7fbdb72fc1032fdcd01f7d94fda9d4a1ded8cd3472b39`。
最终升级前 Linux 元数据备份 SHA256 `374fb8120e836707185ac23c992295dabc68d2d974c4884482772dbe85cf7664`。
两次窄升级保留 9 个 PVC、全部既有 Secret UID（最后一次
872 个）及其他 6 个 Helm 发布；原 865 个
业务 Secret 也在第一次升级前后核对保留。24 对象语义检查只允许相关镜像和
Chart 标签变化。AI 策略及私钥挂载、Functions 公共配置均保留。

仅构建改变的镜像，复用缓存并内网分发到三节点校验，无模型下载。最终三节点、
三个 Safekeeper 和关键存储组件就绪，VM 0，原 Function 定义仍为 1；全部本轮
终态 Job 按 UID/resourceVersion 归档退休，业务数据与 Secret 不删除。

失败与续测原样保存：首轮 Go 夹具角色值、Endpoint 定位和 Reader type 字段
修正后，从同一输入续测；首次浏览器网络变化发生在任何业务提交前。控制面
etcd 超时/API Server 重启中断了原 D 容器，按原成员和数据目录恢复观测，没有
回滚或重建集群；子分支原创建操作 `op_80365c0604f5668c2a7d7bf2` 第六次尝试
成功，沿用原分支、时间线和凭据。E 的平台恢复实际成功，旧测试错误地期望
neondb；原 version 3 回执保留。0.59.1 补齐原库投影与角色单独迁移，再继续原
检查点。构建服务达到原 12 小时运行上限后，按原 CPU/内存/IO 限额重启并复用
同一归档；缓存不删除，未扩大限额。控制面曾中断的事实不构成长时间可靠性认证。

证据：私有 `production/evidence/20261020-catalog-identities` 中的
`accepted-functional-slice.json`、`candidate-source-match-b/result.json`、
`images-d/merged-result.json`、`ui-f/result.json`、`deploy-b/result.json`、
`final-readonly-proof-a/result.json`、中断观测/缩零回执和精确 Job 退休记录。

整组账本计数仍为 13 组基础回执、其余 22 组；C02 继续持久 OID、外部 DDL
识别/受控恢复和默认 Owner 迁移，不能将本次局部路径算作整组完成。
