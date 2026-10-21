# 2026-10-18：项目与分支原失败创建的保留清理

唯一原 `create_project` / `create_branch` 失败后，可以通过生命周期界面、原名称确认和版本/幂等键清理原资源。清理关闭旧入口、绑定原 VM/Service 身份并退休计算，保留原失败、tenant/timeline、WAL、对象和凭据。就绪资源的既有删除/恢复合同保留；失败创建清理不提供就绪项目的七天恢复窗口。详见[合同](FAILED-PARENT-CLEANUP.md)。

本切片的 Go/真实 PostgreSQL、HTTP 故障和真实 Linux UI 验收已完成。L01 的分支 TTL、节点分区仍须继续完成；其余 28 组的代码或独立验收缺口保持开放。

## 精确来源与部署

- API/Worker、Web **0.51.0**；migration **041**；Chart **0.9.43**（`0.51.0-local-failed-parent-preview`）；Helm **114 → 115**。已应用的 040 保持不变，041 为前向迁移。
- 父提交 `928eec95c034af8b8bd89fc1a88761d4e67ece07`，未提交源通过独立快照留证。
- 不可变源码 fingerprint `3e47161e606b78762c9c42d91d364b902a0c994ae96f36d114c83d09fb3c8484`；归档 SHA-256 `c7c425483813ef7530d35219ffd329fa39c6e1c4b7c92b79660054427ebfa028`。
- API/Worker：`neon.local/control-api@sha256:2268a571a117396c82a3c474ae4fab1c4add14a319e89e3e5d2e74d227efacb8`。
- Web：`neon.local/control-web@sha256:22558251af4f8d304447581c68a4c5912cce80d747b36b9f8d65824925c0265c`。
- 其余八个组件复用 0.50.0 记录的各自 digest/provenance。Linux 构建后内网导入三台，没有向公共 registry 推送；构建/导入的 etcd 守卫八次采样通过，峰值约 11.24 ms。
- 升级前控制库完整逻辑备份 SHA-256 `507439b7dd3571cec3edcc0db2df73d7d453041eba090a6eb61165bc5736eb1b`。

24 对象语义部署约束只允许 API/Worker/Web 镜像和 Chart 标签变化；9 个 neon PVC、809 个既有 Secret 身份保留，其他六个 release 不变。验收后独立审计：全集群 106 个原控制器 UID/副本及预期模板、10 个 PVC 身份/绑定保留，76 个活动 Pod 全部 Ready，受管 VM 为零，实际 OpenAPI 0.51.0、Helm 115。

`go-runtime-source-match.json` 核对 321 个 API/合同文件与最终通过的 Go 源一致。`runtime-source-match.json` 核对 413 个运行输入及实际 UI 源一致。项目续验只修正受信任 operator 中一处原项目身份检查，严格对比仅 `scope.project_id` 改为目标 `targetProject`，没有重建 API/Web：operator SHA-256 `1f02831c83383fc9ebb81cd6fa9395b1a2c413ecf3ebe998117389b057615131`，原快照 operator `e576bcb90292511091797882030ab06d4794228b0249362654849bf75acc13a4`。

## 合同与验证

管理员可清理原失败项目；编辑者可清理就绪项目下的失败叶分支。准入持有父资源锁，核对唯一终态、无租约的原创建以及原 tenant/timeline/分支/可选 Writer。项目与根分支创建时默认自动开启保护；只把未经修改的 version-one 默认保护包含在明确失败项目确认中。其他保护、额外分支/Compute、活跃操作和 Backend 依赖拒绝清理。

新 action `cleanup_failed_project` / `cleanup_failed_branch` 对旧 Worker 明确失败，避免旧代码误执行新语义。五个持久步骤关闭原入口、绑定并退休原 VM、观察 Runner GC、退休原 Service、提交 held tombstone。沿用不可变 VM/Service witness；未知 DELETE 结果后的显式重试不能收编同名替代资源。原创建重试与清理在相同父锁下串行，准入清理后旧创建不能再排队。

维护者私有证据根为 `control-plane/production/evidence/20261018-failed-parent-cleanup/`，密码、私有 checkpoint、OCI、原日志及逻辑备份不进入公开 Git。

| 回执 | 结果与范围 |
| --- | --- |
| `go-20261018002848` | 12 个受影响顶层 Go/真实 PG 测试、race/vet/OpenAPI 通过，零跳过；包括失败项目、带 Writer 失败分支、无 Compute 失败分支，授权/版本/归属/保护/依赖、原重放、原失败保留和未知 Service 删除/替代拒绝 |
| `go-20261018002848/tests.log` | 项目和分支各自的真实 PG held-parent-lock 竞争均恰好一个准入成功，另一个由 23514 生命周期检查拒绝，无死锁/传输错误；既有独立 Compute 竞争及受影响删除回归通过 |
| `quality-a` | Linux 格式、TypeScript、Vite、59 套 live 入口准入及 109 paths / 157 operations API 索引通过；没有执行 59 套现场 UI |
| `images-a` / `deploy-a` | 同一精确快照构建、三台导入、备份/部署约束通过 |
| `native-timeline-api-shape.json` | 只读确认实际 PageServer API 的原生 timeline 身份/祖先字段，未用模拟响应代替现场读取 |
| `ui-branch-a` | 首次七项实际 UI 检查及独立 native operator 通过，原失败子分支完成清理，父分支/Writer 保留 |
| `ui-project-a` | 原项目/Writer 创建已受理并真实失败；operator 回收临时策略时错误比较父 fixture 项目 ID，停止并保留现场，原 UI 等待正确回执后超时。没有改写原失败或创建替代项目 |
| `ui-project-a-owned-guard-recovery` | 私有恢复工具读错 route Secret 名，在删除前停止；原失败证据保留 |
| `ui-project-a-owned-guard-recovery-b` | 原策略/binding UID、标签/完整预期 spec、原 HTTP 403、Service、两个 Secret、Worker 和原生 timeline 均核对后，按 UID/RV 回收策略；三 API dry-run 从拒绝恢复成功，没有伪造原 UI marker |
| `ui-project-b` | 从同一原失败项目续验，复用经独立审阅的原故障证据；没有新建项目或策略。完成原清理、丢失 202 重放、旧创建拒绝及保留校验，native operator 成功退出 |
| `owned-terminal-jobs-retirement` | 四个终态自有 Job 完整归档后按 UID/RV 退休；没有删除业务数据或 Secret |
| `platform-after-acceptance-b` | 全集群身份、Ready、VM 零及实际版本审计通过；首次审计工具仍指向失败的 `ui-project-a`，在集群读取前被前置检查拒绝，改为已完成原项目续验回执 |

开发阶段的失败 Go 回执保留：默认自动保护的合同遗漏被修正；随后测试 fixture 的 tenant/timeline 唯一性和变量重声明被修正。`go-20261018002433` 先完成三种失败父资源路径，最后补入项目/分支并发竞争和最终 OpenAPI 后由 `go-20261018002848` 通过。所有测试只使用独立临时 PostgreSQL，未改现场业务状态。

## 原生验收身份与结果

失败分支沿用项目 `prj_6f3c51bd15b772e3`，新分支 `br_fed95001d2d5a26c`、Writer `ep_fed95001d2d5a26c`，原创建 `op_928b83f4e5d6e1c081824bfe`，清理 `op_c6165b3bf750b785582104b1`。Job 为 `publication-ui-20261018003419`。只拒绝该原 Writer VM CREATE；三台 API 的基线/拒绝/恢复 dry-run、Worker 结构化 dependency HTTP 403、VM 缺席及原 Service 均证明实际故障来源。清理前后 PageServer 返回同一 timeline 和祖先身份。

失败项目始终为 `prj_05f6883980377e87`，根分支 `br_05f6883980377e87`、Writer `ep_05f6883980377e87`、原创建 `op_4e59ee0e7a303b6f0d0205ab`。初始 Job `publication-ui-20261018003501` 保留两项原受理检查和真实故障；受信任策略恢复回执补齐原 native 故障/恢复证明，续验 Job `publication-ui-20261018003908` 新执行清理及其后四项检查，清理为 `op_b4726fc54828dc410fb1ad66`。最终七个检查点来自这条完整原资源链，不能计作七项全部重新执行，更不能与分支回执相加称为十四项全新检查。

两个清理都丢弃一次真实 202，以原 key/version 返回同一 Operation；完成后旧创建 retry 返回 409，原失败摘要不变。原 Service UID witness、VM 缺席 witness、原 Secret UID/内容摘要及同一 PageServer timeline/祖先均保留。临时策略退出、原 Worker Ready、VM/Runner 零。项目 held tombstone 的 `recover_until` 为 null，未把未完成创建伪装为可恢复就绪项目。

随后继续分支 TTL。物理 GC、外部 fencing、节点分区、HA/DR、全链路 TLS、长期负载及完整官网其他业务组仍按各自门槛推进，共享 SSD 的既有介质错误没有被宣布修复。
