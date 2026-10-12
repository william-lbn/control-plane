# 2026-10-12：SQL 身份、Worker 恢复与统一 Helm 0.1.12

## 1. 运行版本和结论

本历史 UI 验收的 Linux 控制源码为 `f0b63f5eed0fc412960b8b7dc7e8cda34abf916f`，
统一 Helm 0.1.12 为 `57a08dd62e6db9649605acb9364821d373e9466f`，不可变标签
`v0.1.12`。API/Worker 分开部署，metadata 21/21。Neon、autoscaling、PG16
自己的 fork 版本未修改。Functions 执行仍禁用；这不是生产准入或官网功能全部完成。

本次运行代码补强 Functions SQL 的权限/认证会话检查，并让独立 Worker 在首次
领导租约获取失败时保持 unready、有界重试。已有租约丢失仍排空并退出；没有放宽
外部 fencing 或宣称 HA。下一阶段源代码的 Migration 022/SQL Secret/非 superuser
委托尚不属于此镜像，需独立验证和发行。

## 2. 已完成供应链与部署

| 门槛 | 独立结果 |
| --- | --- |
| Linux Go/真实专用 PG/race/vet/build | `dataapi-quality-20261011162836`，610 pass / 0 fail / 0 skip；归档 `3016184130d87f4f5393e8a274d4a19230da0b954417680f1ba603473b442e64` |
| Linux Web | `restore-web-1791736303`，17 项，类型/构建、3 Chart、8 RBAC |
| 公开 control CI | `38156200281`，13 作业成功；8 实际下载 artifact 校验；Go 回执 610/0/0 |
| 匿名 OCI | 七镜像 manifest/config/platform/source/license 独立核验，全为 f0b63f5 |
| 三节点 CRI | 七镜像 × 三节点，21 次 pull/inspect/digest 核验 |
| Linux Helm | `helm-quality-1791737628`，15 工具/10 Chart/16 负例/10 包 |
| 公开 Helm CI | main `38157567226`、tag `38157598694` 成功 |
| 匿名 Helm 消费 | 十个 0.1.12 包、index 和 SHA256SUMS 校验通过，无发布凭据 |
| Linux 升级 | 八阶段完整 apply/verify、manifest/Secret/PVC 审计；原 values/持久身份保留，metadata 21/21 |

来源：[Control CI](https://github.com/william-lbn/control-plane/actions/runs/38156200281)、
[Helm 发行](https://github.com/william-lbn/neon-helm/releases/tag/v0.1.12)。
精确镜像见 Helm `locks/control-plane-f0b63f5.json`，旧锁未覆盖。

## 3. 当前 UI 回归：逐套记录

全部在 Linux 使用此提交的不可变 Git TS 源码，串行执行，不把上一版 129 个检查
计入此版。十组当前矩阵完成：134 pass，其中九组正常 129、原 Data API 恢复 5。
不是十组全部首次通过。汇总回执 `sql-identity-ui-matrix-202610111902/result.json`；
实际测试结束后 Compute VM/runner 为零。全部失败 partial 不加到通过数。

| 场景 | 当前结果 |
| --- | --- |
| Functions metadata | 8 pass；只读/禁用执行边界 |
| Native product | 13 pass；四次只读轮询故障、创建 mutation 一次；原项目 retained-delete |
| Managed Auth | 23 pass；实际注册/会话/RLS/分支隔离/缩零冷醒；retained-delete |
| Data API | 原 suite FAIL 14；原项目恢复 5 pass；失败 partial 不计入通过矩阵 |
| Object Storage | 19 pass；实际对象、条件写入、签名下载、分支继承/隔离、生命周期 |
| Timestamp/LSN restore | 11 pass；实际 timestamp/LSN 新分支 SQL/权限/继承负例 |
| Endpoint deletion | 19 pass；Reader 删除、Writer 替换、其它 Reader、原资源和恢复 |
| 多 Reader/项目生命周期 | 22 pass；真实两个 Reader、只读/零/WAL、依赖保护、原项目 retained-delete/recover |
| Backend credentials | 8 pass；原成功 Job 只读收集，未重放测试 mutation；没有真实推理 |
| Console invitations | 6 pass；实际邀请/注册/组织隔离/权限正反例 |

Data API 原 Job `publication-ui-20261011170839` 在首次冷读取返回 HTTP 503。
原 project/branch/writer 均是 `aed92631751b9d8e` 后缀，原 SQL/RLS/密码/操作保留。
失败期间 Adapter 路由读取出现网络错误，原 VM 随后 Running。第一次恢复
`publication-ui-20261011172026` 手动冷醒通过，但自动冷醒时控制面请求失败；收集器
SSH 超时通过只读原 Job/Pod UID 核验找回真实 Failed，未重放 mutation。
第二次恢复 `publication-ui-20261011172835` 5 pass，包含原行/RLS、显式测试 issuer
旋转、手动零/自动零首次冷读取、停用/held-delete 释放配额；实际 VM/runner 为零。
原失败和恢复失败独立保留。这是恢复后功能验收，不是故障期连续可用证明，也未
仅凭网络日志归因为硬件。没有修改/重启 RKE2、etcd 或 fork 源码来绕过失败。

Backend credentials 原 Job `publication-ui-20261011174437` 已 Succeeded，但本地
证据目录创建碰到 Windows WinError 206。归档工具补齐 extended-path 的目录创建
和遍历支持；只读核验原 Job/Pod UID，收集原 8 pass，未新建业务凭据或重跑浏览器。
后续 invitations Job `publication-ui-20261011174627` 6 pass。测试本身均在 Linux。

十二个归属明确的终态测试 Job 在完整 JSON/日志归档、原 UID/resourceVersion
核验后正常回收。回执 `sql-identity-terminal-retirement-202610111903/result.json`。
没有删除 active Job、SQL、Secret、PVC、WAL、对象或历史失败记录。

## 4. 下一阶段与未完成门槛

正在实现和验证 PG16 的明确 CREATEROLE creator 委托、immutable SQL Secret 与
Migration 022 的真实 UID/MAC 回执。详见 [SQL 合同](FUNCTIONS-SQL.md)。只有完整
leased Driver、真实 Neon/guest SQL、TLS、原 VM 退休和部署/调用 UI 都通过，才可
启用 Functions 产品执行。代码包、内部库测试、模拟资源或 CRD dry-run 不替代它。

新源代码的 Linux Job `dataapi-quality-20261011180043` 已通过完整 Go/race/vet/build、
live-tag 编译和专用 PG16：666 pass / 0 fail / 0 skip。Pod 原 UID
`fa64b530-90f0-4751-81b1-d16cbb5559d1`，源码归档 SHA-256
`b7b69e8bd0cdb494b17f53609dfa878e7170690e4a28785c9d18002395d615b8`；
九个变更 Go/SQL 文件与该 Linux 实际测试源逐字节核验。
首轮记录保留两个测试夹具失败：organizations 并无 created_by 列；PG16 不允许
向自己的 grantor 反向授予 ADMIN。按真实 migration/catalogue 修正夹具和断言，
没有修改业务 schema 或降低运行权限检查。第二轮字段失败也保留；666 项是
独立重跑全部通过的结果，不把失败轮次部分结果加到通过数。

同一源码真实 K8s/MinIO Job `functions-foundation-20261011180347` 为 4/0/0，
Pod UID `8817cee7-f426-4d97-86e2-b1d76bfb6675`。实际 immutable SQL Secret
`fn-9122af8605f0b424-sql-1` 的 UID 为 `5c4b9b4e-e515-4966-8aec-b2b2cdd0efe6`。
验证已提交创建回复丢失、独立 client 重启找回、原 UID 重读、替换 UID/跨分支/
不同密码拒绝。原 Secret 保留。Service 实际创建并核验，VM 仅 admission dry-run，
没有持久 VM，不计入函数执行或 UI 部署验收。

SQL Secret/委托代码已以 DCO commit `77e12ef82e57c22b588310f62be9bf52e40683c4`
提交，[公开 CI 38162444768](https://github.com/william-lbn/control-plane/actions/runs/38162444768)
十三个作业成功。统一 Helm 的验收文档已提交 `ee61106`，对应
[文档 CI 38162137975](https://github.com/william-lbn/neon-helm/actions/runs/38162137975) 成功；
不可变 v0.1.12 标签未覆盖。新源码镜像的现场部署与 UI 结果不能沿用运行版 f0b63f5。

随后完成 `ConnectRuntime` 的可信 SQL 连接与认证 downgrade 拒绝合同。最终专用 Linux
Job `dataapi-quality-20261011182049` 为 **704 pass / 0 fail / 0 skip**，Pod UID
`cb35b57c-5e67-46dd-a92b-1ade2c0b93df`，源码归档
`fa32a6073c32d755c51dbeda70376a255176e79afd1ca04d920d79c726b7066a`。
真实 PG16 的正确 SCRAM/TLS 登录、普通表/sequence DML、默认 30s/5s GUC、错误
密码/CA/hostname/branch/cancellation 都通过；认证降级和环境输入负例独立运行。

首次 TLS 测试失败 `dataapi-quality-20261011181349` 保留：专用 PG 的回环默认 trust
使错误密码未被验证，pgx 在 TLS mode 分支之前会读取 ambient 证书路径。修复的是
专用 CI 的 SCRAM initdb 参数和连接配置/认证检查，未修改业务 HBA 或放宽断言。
此前仅以回环登录声称实际 SCRAM 的证明不足；704 项补上正确/错误密码及强制 SASL
握手的独立证据。TLS routing fixture 仍不是 Neon Proxy/guest 产品验收。

AI Gateway 真实供应商凭据仍未配置。跨实例栅栏、HA/DR、全链路可信 TLS、完整
内存缩回、小数 CPU、完整物理删除/GC 等仍需独立开发/验收；硬件影响记录与业务
能力分开。完整状态见 [PRODUCTION-GATES](PRODUCTION-GATES.md)。

每次 Linux 原始回执保留源 commit、artifact digest、原 Job/Pod/VM UID、SQL/Secret
身份和全部失败链。只正常回收归属明确且已归档的终态测试资源，不删除业务数据、
WAL、PVC、Secret、对象或历史记录。
