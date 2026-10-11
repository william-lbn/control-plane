# Functions 存储基础与缩零恢复增量

## 1. 结论

本阶段交付实际 Go 代码：不可变代码包、不可变环境 Secret、持久 UID/MAC/密钥版本回执，
以及现场发现的“VM 已消失时原缩零操作无法幂等完成”的修复。
完整 Functions 部署/调用 Driver 仍未注册，`capabilities.functions.enabled=false`。
不能用内部库通过代替产品已经可用。公开接口仍是 OpenAPI 0.11.0 的 90 个实际操作。

已有版本的 Linux UI 结果与本阶段新增开发测试分别记录；本页不是官网全部功能或生产准入声明。
旧失败、凭据文件、数据、对象、WAL 和私密日志完整保留，不进入公共仓库。

## 2. 代码与实际验证

| 层次 | 本阶段结果 | 证明范围 |
| --- | --- | --- |
| 默认 Go/真实 PG/race/vet/构建 | Linux Job `dataapi-quality-20261011071213`，540 pass / 0 fail / 0 skip | Migration 020、持久身份和 lease、HTTP ownership/recovery 负例及现有合同回归 |
| 实际 MinIO 与 Kubernetes | Linux Job `functions-foundation-20261011061448`，2 pass / 0 fail / 0 skip | 条件创建实际返回 412；原代码字节保留；实际 Secret POST 成功后丢回复，再 GET 恢复原 UID |
| 前端与控制面 Chart | Linux Job `restore-web-1791703704` 通过 | 最终恢复测试源码的格式、13 个测试、类型/构建、3 个 Chart |
| 既有默认 guest | 32 项真实 NeonVM 检查，独立源码/镜像 | 见 [guest 验收](ACCEPTANCE-2026-10-11-FUNCTIONS-GUEST.md)，没有冒充真实 SQL/完整 Driver |

540 项源码归档 SHA-256：
`28be7bccd60da12b74deff2f02c9609977dedb936d61530712c74b8a0cc9ece6`。
实际外部存储测试使用先前通过 526 项的源码归档
`0ecf0f223e9b2293ea4f4a7c60b055cf28e4c03c6e1eca654f1ba4541ce18b22`；
该测试不涉及新 metadata 回执方法。回执方法只在专用可丢弃 PostgreSQL 中验收。
上述验证全部发生在 Linux；Windows 仅传递源文件和采证，没有 Windows 业务测试。

### 2.1 缩零恢复修复

现场 UI 重试原 failed suspend 时，原 VM 和 Runner 已被正常回收；旧代码在 VM GET 404
直接失败。新代码先核验原 Proxy route/template 所属，再观察精确 workload 的 Runner
正常消失，最后复核 VM 仍然 404，才允许幂等完成。所有观察均为 GET；不唤醒计算、
不 patch/delete 新实例，不跳过未知/失败读取。即使终态 Runner 仍存在也不能成功。

真实 kube HTTP 客户端负例覆盖后继实例、读取错误、外来 Pod、正常 GC 和终态 Pod 残留。
本修复尚不提供外部 epoch fence；最后观察之后的迟到连接仍须由独立跨实例门槛解决。
新镜像已在现场通过原 Object Storage 项目的 UI 恢复验证，见第 3 节；不能引用单测代替。

### 2.2 持久 Functions 身份

Migration 020 不改历史 migration，不为旧部署伪造 UID。私有回执绑定完整 FK scope、
已核验代码路径及 metadata 摘要、实际 Secret UID、内容 MAC 和密钥版本。
创建结果未知时读取原记录；变更旧 UID/MAC/key version 被拒绝。租约检查使用实际
wall clock，锁内没有网络 I/O。公开 API projections 不选择私有回执；JSON 也不序列化。
接口、数据模型、流程和手动复测见 [存储合同](FUNCTIONS-BACKING.md)。

## 3. 运行版本回归与保留失败

统一 Helm 0.1.8 / 控制面源码 `f256408c97bed15f5bf91b243e750d214cfe933a` 的七镜像
已逐一匿名核验，并在三节点共完成 21 次固定 digest 拉取。
其当次真实 Linux UI 已完成 Functions 只读元数据 8 项、原生计算链路 13 项、Managed Auth
23 项、Data API 20 项，合计 **64 项**。原生链路的 13 项包含轮询两次 503，未注入 Worker
故障；Data API 20 项没有该故障注入。不同版本/故障矩阵的数字不得拼接。

该版 Object Storage 原测试完成 7 项后在计算冷醒超时；另一个恢复尝试完成 3 项后
在原子分支创建失败。第三次 UI 恢复又暴露上述 VM 已不存在的幂等缺陷，结果仍是失败。
不能宣称存储完整回归或该版本全部功能通过。临时关闭的自有测试 selector 已按原 Secret
UID/resourceVersion 恢复准确的原 entry；没有改变其他项目入口或删除数据。
后续结果使用独立 attempt，沿用原项目、原子分支、原请求与 Operation。

历史版本九套 UI 146 项/独立恢复检查是历史证据，不自动放行本次新镜像。

### 3.1 后续同日发布和原失败恢复

控制面源码 `fbff1b09f26f0840255b92b58428de1e9167689f` 的公开 Linux CI
`38123437193` 全部 13 作业成功，七镜像匿名核验及三节点 21 次 CRI 拉取通过。
统一 Helm **0.1.9** 源码 `939ec2074d4e7cab85279c463f3140603a4f818e` 的分支 CI
`38123903894`、标签 CI `38123968623` 均成功。Linux 本地 Chart 验证包含 15 Node
测试、10 Chart、16 拒绝场景、10 包；匿名消费者下载 10 包并核验 index/SHA。
按八阶段更新已有 releases 后，metadata 实际为 20/20 migrations；未修改 data plane fork。

原 Object Storage UI 恢复 Job `publication-ui-20261011081950` 使用精确提交的正常
Storage suite，通过 **16 项**，没有恢复测试源码 override。沿用原项目
`prj_66f9c109a5d5935c`、原子分支 `br_90ef64a644eed870`，在 UI 重试原 failed suspend
和原 failed create_branch；验证实际数据、对象、克隆隔离和生命周期。测试结束实际
Compute VM/运行 Pod 为 0，逻辑删除释放测试配额，数据、对象、密码及旧失败保留。
这是恢复场景，不能标为本版本新建项目/分支的完整 Storage 验收。

同一 `fbff1b09` 的串行 UI：Functions 只读 **8 项通过**，Native **13 项通过**
（两次受控 503 GET）。Managed Auth 原 Job `publication-ui-20261011082552` 在第 8 项后
失败：后台原 child create_branch Operation 已 succeeded，但 UI 创建窗口停留，显示
`Invalid Operation response`。原 malformed response 的字节没有采到；不能断定网络根因。
源码有 JSON 解析失败吞为 `{}` 的缺陷，下一增量改为可识别的 502，使已受理 Operation
的 GET 观察有界恢复，同时继续拒绝外来 Operation ID，不重放客户 mutation。
原 Auth 项目 `prj_ae29a8fffebad70e` 和子分支 `br_411c065750acbd77`、数据、账号、密码保留，
两个 Writer 均已通过正常 API 缩零。恢复测试沿用原身份，不再创建替代项目。

该串行批次在 Auth 失败处停止；本版本后续 Data API/PITR/Endpoint/生命周期/凭据/邀请
尚未完成该批次验证。8 个已完成 Auth 检查不是一次通过的完整 Auth 验收。

### 3.2 后续已发布的引导身份与 UI 响应修复

Migration 021 与 [不可变引导合同](FUNCTIONS-BOOTSTRAP.md) 已实现并在 Linux 通过
553 Go/PG/race/vet/构建检查、3 个实际 MinIO/Kubernetes 检查。Linux Web Job
`restore-web-1791709106` 完成格式、17 个前端测试、类型/构建和 3 个 Chart（另含 8 个
RBAC 测试）。畸形成功响应的恢复测试只重读原 accepted Operation；有效但外来 ID 仍拒绝。
这些结果使用独立 fixture，仍未接通产品部署/调用 Driver。UI 响应修复和原 Auth 恢复
suite 的运行验收单独记录，未用前端测试冒充真实浏览器恢复。

### 3.3 历史 0.1.10 发行与逐套回归

本历史发行源码 **`dda5d34dc9bed7117950f31de0bc438b47dc2c02`**，公开 Linux CI
**`38126879254`** 的 13 个作业全部成功。七镜像逐一匿名核验，三节点共 21 次
CRI 固定 digest 拉取通过。统一 Helm **0.1.10** 提交
`be44ca850cdd12b7207b9e78c93b7b02a98ae818`；分支 CI `38131636400`、标签 CI
`38131682981` 成功。10 Chart 的匿名消费者 index、包 SHA 和下载验证通过。
八阶段更新已有 releases，metadata 为 **21/21 migrations**。没有修改 data plane fork。

实际串行 Linux UI 回归如下。运行均为上述源码；恢复续测的 TypeScript 增量采用
明确记录 SHA-256 的 override，之后提交公开源代码。所有原失败、密码和数据保留。

| 场景 | 实际通过检查 | 验证范围 |
| --- | --- | --- |
| Functions metadata | 8 | 真实只读 UI/API/PG、执行禁用；没有函数执行 |
| Native | 13 | UI 项目/分支/Writer/Proxy SQL/缩零冷醒；四次 GET 观察故障、一次 project POST |
| Managed Auth | 23 | 本版新建项目/注册/JWT/RLS、父子隔离、零/冷醒、禁用 |
| Data API | 20 | 本版新建项目、真实分支 RLS、零/冷醒、禁用；没有 Worker 故障注入 |
| Object Storage | 19 | 本版正常完整 suite：分支文件、隔离、条件写入、冷醒及保留生命周期 |
| 历史恢复续测 | 10 | 原 timestamp Operation/固定点恢复后继续；写入隔离、UI 新 LSN fork、冷醒/重放/监控 |
| Endpoint 删除续测 | 14 | 原 Reader 删除已成功；依赖保护、替换 Writer/原密码数据、保留 Reader WAL、独立退休和项目恢复 |
| 生命周期续测 | 14 | 原依赖/两 Reader 状态；SQL 未知结果核验、项目恢复、真实 Data API、撤销凭据不复活 |
| 应用凭据 | 8 | 精确自有既有项目、data-only 分支、祖先/兄弟/模型范围、一次性重放、轮换/撤销 |
| Console 邀请 | 6 | 原 Job 实际注册、已有账号、隔离/撤销；只读补采，没有重新注册 |

合计 **135 个实际通过检查**：七套正常场景 **97** 个，三个明确恢复续测场景 **38** 个。
这不是“十套全新首次通过”，也没有把旧失败中的部分成功累计进去。各场景终态
实际 managed Compute VM/运行 Pod 为零。已通过的本版新建测试项目正常保留删除
释放逻辑配额；恢复项目也按原名称和通过证据保留删除。物理 GC 保持 false。

原 Auth 项目 `prj_ae29a8fffebad70e` 的独立恢复 **19 项**使用已提交的测试修复
`af063ac`，其与运行源码间只包含 Auth 测试与 TESTING 文档；没有运行代码差异。
`68e4327`/`af063ac` 的公开 CI `38132297918`/`38132506964` 均成功。
该 19 项单独保留，不加入上述 135 项。

本轮原失败与纠正：

- timestamp 创建原 Operation `op_b3a4a83e428170d24cabb00e` 在 wait_neonvm_running
  重试性失败，根因未证明。实际原 VM 随后 Running；UI 重试相同 Operation 并固定
  原 LSN，通过独立 5 项恢复，再用原项目完成 10 项续测。原 3 项后失败仍保留。
- Endpoint 原 Job `publication-ui-20261011115104` 在 7 项后失败。409 依赖保护正确，
  测试仍匹配旧英语提示；改为机器 code + 实际 UI message。原项目续测，首个续测
  的 endpoint_type 字段读取错误也保留；纠正后 14 项通过，未替换原项目。
- 生命周期原 Job `publication-ui-20261011121353` 在 10 项后 INSERT 返回 503。
  不假定未执行，不盲目重放 DML。原项目 UI SELECT 核验精确行后继续，14 项通过；
  不将 503 归因硬件或抹去。硬件影响与软件缺陷分别记录。
- 凭据首个尝试误选普通项目；`ci-` 自有 fixture 前置保护拒绝，未创建分支/凭据。
  改用有 23 项历史通过证据、当前 ready 的 `prj_3a5251a12cefad42`，正常 suite 8 项通过。
- 邀请原 Job `publication-ui-20261011124004` 已成功完成 6 项；Windows 采证路径
  过长导致复制失败。按原 Job/Pod UID 只读补采并查证零 Compute，原采证失败不覆盖。

135 项串行总回执为维护者私有 `functions-bootstrap-final-serial-matrix-202610111315`。
原测试密码、Token/SQL 请求、Kubernetes Secrets 和私密截图不进入公开仓库。
新的资源恢复/原子发布代码、质量检查及未接通范围见 [下一实现](FUNCTIONS-PUBLICATION.md)。

## 4. 下一阶段与独立门槛

1. 将资源恢复/原子发布内部增量经过实际 CRD 与 PG 质量检查后，提交公开源代码，
   校验 Linux CI、七个自有镜像与下一统一 Helm 固定版本锁。当前已发布运行版见 3.3。
2. Functions API/Worker 与本阶段存储/持久引用接通；真实 scope/UID、私有 CA/bootstrap、
   受限 SQL、guest health、排空、正常退休、版本发布、UI 上传/调用/回滚和零/冷醒。
3. 同一新镜像逐套完成存储、历史恢复、目录/凭据、成员邀请及生命周期回归；每套结束
   回收自有 Compute，保留数据/记录，通过后才逻辑退休测试项目释放配额。
4. 继续独立实现完整 S3/multipart/GC、Functions triggers/WS/日志、原地 PITR、CPU/内存、
   外部 fencing、HA/DR、全链路可信 TLS 和长期运营。AI 真实推理仍需运营方的上游凭据。

当前发行保持预览状态。硬件性能问题与软件功能缺陷分别记录；实际失败不能忽略后计为通过。
