# 2026-10-16 当前交付：共享执行历史与计划比较

## 1. 结论

本地运行 **API 0.41.1 / migration 036 / Chart 0.9.31 / Helm revision 98**，
**97 paths / 139 operations**。本阶段新增项目共享执行历史和执行计划比较；前一阶段
MFA、个人历史策略/删除记录见[0.40 交付](DELIVERY-2026-10-16-HISTORY-DELETION.md)。

| 功能 | 已完成并本地验收 | 独立边界 |
|---|---|---|
| 项目共享执行历史 Q02 | 默认私有、作者逐条确认、原密文引用、版本/幂等/审计、当前作者/读者授权、撤回/删除/到期失效、元数据分页、React UI | 不含密码/结果；不能收回已复制内容；当前 Console session only；不代表合规销毁 |
| 计划比较 Q03 切片 | 真实 Explain/Analyze 的内存基线、成本/行数/实际指标差值、树路径观察、连接范围变化/重载清除、React UI | 不自动 SQL/Analyze，不调用模型、不声称单次耗时变化代表性能提升；AI SQL/建议仍缺 |
| 跨组织深链接修复 | 与 Project 合同一致读取 org_id，使用前端 Project 类型；实际切换到项目组织后再操作 | 不更改服务端项目授权模型 |

**整个产品、官网全部功能和生产准入仍未完成。** 逐项工程与独立验收门槛继续见
[剩余需求账本](REMAINING-REQUIREMENTS-2026-10-16.md)和[生产门槛](PRODUCTION-GATES.md)。
本次通过只关闭对应基础切片，不能理解为其余需求已经有完整代码。

## 2. 数据模型、API、UI 与部署

详细模型、API 错误/并发语义、流程图和手工复测步骤见
[共享历史](SHARED-SQL-HISTORY.md)、[计划比较](SQL-PLAN-COMPARISON.md)及生成的
[API 文档](API.md)。迁移 036 增加共享时间、版本和 metadata-only 幂等表，无默认共享、
无 SQL 明文副本、无历史删除。本人删除/到期删除原记录时，关联共享引用同时失效。

Linux 本地 BuildKit 串行构建、三节点 checksum/digest 核对与 containerd 导入。
父 Git 为 `72f4b29`，父提交不冒充运行源码。组织字段修复只更新 Web。随后 0.41.1 补齐闭合响应模型；三个 Go 执行文件与此前 UI 验收版本逐字节 SHA-256 相同，只更新合同工件，Web 镜像保持。

| 组件 | 不可变源快照 SHA-256 | 实际本地 digest |
|---|---|---|
| API / Worker | `7efece48b3e3985721a3afc54f044596fb1e5a6a052cf2b575c8e585c9136a0c` | `sha256:6cf6978e8dd52f697fc59e8a543e3388456aa1b3b6551662035bf09de1d91a67` |
| Web | `3588ba9c886554c780bcd8e019315d41a076bcec5c14b6d3077dcffaeea8ead3` | `sha256:80d9432ede5c51639d2fd555bf4f5ec6580ff5920e29d5f977e9bc3244a01605` |

在测试续跑和文档更新后，逐文件核对当前 Go/契约与 API 快照、当前 Web 运行源码与
Web 快照一致；后续变化限测试/文档。三次 Helm 升级均先备份元数据/values/manifest，
保留 9 个 PVC、779 个既有非 Helm Secret UID 及其他六个 neon release manifest。
最新备份 SHA-256：`0a2a846cdaca4b28390bd47837bf1729ad1f0ea5419f39e4ba2b74bed6aaf642`。

当前是本地精确 image overrides 部署；公开镜像发行与统一 Helm 旧锁升级仍属 O02。
公开统一 Helm 0.1.31 源码候选不能冒充已默认消费本地 0.41.0 镜像。

## 3. 真实验证

### Go / 元数据 PG

新 `TestSQLHistorySharingConsentIsolationRevocationAndLifecycle` 在独立 control_ci
PostgreSQL 与 race 下通过；补充同 key 并发请求只产生一个 generation/一条审计后
再次通过。覆盖 owner/Editor/Viewer/外组织、API Key、默认私有、显式确认、版本冲突、
旧重放不重启共享、作者停用/降权、读者降权、密钥缺失、审计失败回滚、分页隔离、
原记录删除和到期撤回。vet 与三项 OpenAPI 合同检查通过。最终另在真实 PG 测试中对照实际 HTTP 字段与 closed/required schema，修复共享 allOf 叠加 closed 基类及个人新增字段遗漏；0.41.1 已部署。当前 Go 执行文件与 UI 验收版本完全一致，因此不重复运行整套 UI；不以 DB 租约冒充跨实例
Compute 栅栏或 HA 验收。

### Linux 前端与实际 UI

TypeScript/Prettier/Vite 通过，最终测试定位调整另做格式/类型检查，不重跑旧业务全集。
四项新计划比较模型测试通过；47 个 UI suite 声明/准入/文件存在检查通过。

- 共享历史 Job **publication-ui-20261015235003**，**8 条**检查通过：UI 临时单项目授权、一次真实
  SELECT 的默认私有、作者确认、另一 Editor 实际加载、撤回/旧重放、权限撤回/恢复、
  保留原私有历史、移除临时成员和 Compute 0。仅 **1 次**无写入 SQL。
- 计划比较 Job **publication-ui-20261015235217**，**5 条**检查通过：真实 Explain 内存基线、第二计划
  差值/结构、真实 Analyze 指标门槛及差值、清除/连接变化、重载无持久基线。仅
  **4 次**用户主动只读 Explain/Analyze，随后 Compute 0。

本阶段合计 13 条新 UI 检查，不冒充一次官网全量回归。其他功能保留各版本已验收
记录；本批不反复执行 MFA 或旧业务套件。

## 4. 原失败、恢复与资料

首次 UI 在任何成员/SQL 修改前发现 Project `org_id` 与 Web `organization_id` 不一致；
按正式合同修正并复用 Project 类型，只重建 Web。第二次在已确认临时单项目授权后，
遇到 select 的 exact label 定位超时；原 SQL 请求数仍为 0。改用真实 combobox 角色，
从原账号/原 checkpoint 接续，不创建新账号，不重复已有授权，不隐藏两次失败。
测试 privateDir 类型收窄和维护者 formatter 路径错误也保留于私有日志。

最终专用读者恢复原组织权限、临时项目/组织授权移除、密码/MFA 未改；新共享历史
`qh_d869ac730e7def2f8bf0bb02` 最终保留为私有，所有原历史、SQL 回执、失败、源码
快照和 OCI 保留。原始材料位于维护者私有
`control-plane/production/evidence/20261016-history-sharing`，不随公共源上传。
共享 suite 的原始 DOM/trace 不上传 GitHub artifact；公开源不包含密码或 Python 运维脚本。

终态 Job/Pod 先留描述、容器日志，再按 UID/resourceVersion 退休；一次性 PG Pod 已在
各自回执中退休。最终只读资源回执记录节点、控制面期望副本、错误 Pod 与 Compute 0。
节点瞬时健康不代表物理故障域独立、HA/DR 或长期 SLO 已达标。

## 5. 下一阶段

继续 S3 multipart 的实现与实际 SDK/UI 验证，再推进大对象/安全 GC、Functions 和
其余 Auth/生命周期/分布式工程门槛。不能把阶段提交当成整个任务完成。
