# 2026-10-09：分支对象存储与全切片回归

## 1. 交付结论与版本

本次增加**可以实际使用的产品 Object Storage REST v1**：从 React 创建项目、
安装分支目录、管理存储桶、上传/下载文件，到原生子分支继承、独立修改、缩零冷醒、
保留删除与恢复。不是把数据库 pages/WAL 的 MinIO 服务改名充当文件产品。

当前仍是预览产品。Functions 执行服务、AI Gateway 真实推理尚未实现；完整外部
S3 协议、物理 GC、HA/DR、外部跨实例栅栏和全链路可信 TLS 未获得独立准入。
功能实现、当前版本现场通过和生产认证是三个不同结论。

| 输入 | 本次锁定值 |
| --- | --- |
| 实际运行控制源码 | `716244956d949120b319007affc1d31e5674540f` |
| 公开 Linux CI | [37950048773](https://github.com/william-lbn/control-plane/actions/runs/37950048773)，12 个 Job 成功 |
| 镜像 | 七个控制镜像；同一源码，GHCR/Docker Hub 同 OCI manifest；部署只用 digest |
| 原生控制 Chart / API | `0.8.0` / OpenAPI `0.10.0`，58 paths / 86 operations |
| 统一 Helm | `0.1.6`；选择 `locks/control-plane-7162449.json`，八阶段安装、十个独立 Chart |
| metadata migration | `017_object_storage.sql`；用户分支目录是独立受限 SQL schema |
| Neon / Autoscaling / PG | 原 fork 的 2026-09-30 固定发行，详见统一 Helm SOURCE-PROVENANCE |
| 环境 | 三台 Linux/RKE2 实验节点、共同宿主 NVMe；非独立故障域 |

本报告后续文档提交不自动改变运行镜像。复测先检查实际 digest/source label，
不能把文档 HEAD 当作已部署应用源码。先前报告按其历史版本保留，本报告是当前
Object Storage 增量入口。最终公开源码升级 `20261010/unified-125901` 八阶段及完整 manifest/PVC/Secret
审计成功；七镜像的 Linux 匿名校验与三节点 21 次 CRI digest 拉取均通过。
此前 `d4549a8` 的现场通过记录保留原版本，不自动标成 `7162449` 的新回执。

## 2. 运行代码、模型和接口

详细架构、流程/时序图、数据模型、完整接口、限制和逐条手工步骤见
[OBJECT-STORAGE.md](OBJECT-STORAGE.md)。执行合同是 bundled OpenAPI；Swagger 在
`/api/docs`。本项目 `/api/v1` 不冒充托管 Neon `/api/v2`。

* Go API 管理授权、条件请求、目录事务、完整性验证和短期下载；Go Worker 使用
  原有领导租约与 durable Operation 安装/关闭服务。API、Worker、Web 是独立进程。
* metadata 只存实例意图/状态/代次/Endpoint；用户分支 `neon_storage` 存 buckets、
  objects、项目所有权 marker 和安装代次。目录随 Neon timeline 原生继承。
* blob 随机命名、不可变、按项目隔离。先持久化 bytes 再提交目录；覆盖/逻辑删除
  保留旧 bytes。子分支改目录不会改父目录，也不删除其他时间线引用的 bytes。
* 上传/覆盖/删除需要 create-only 或当前 SHA-256 ETag；缺条件 428、过期 412。
  一对真实并发覆盖必须只有一次成功。下载校验长度与 SHA-256 后再返回内容。
* private 链接绑定用户/分支/对象 blob/代次/到期；重新启用、撤权或对象改变关闭旧
  访问。public_read 只开放匿名 GET/HEAD，不开放列表或写入。
* 沿用项目→分支→实例的事务锁顺序；内部 SQL 使用同一 metadata 事务的共享
  休眠 gate，避免等待第二个 pool 连接。这不等于所有外部连接已经被跨实例 fence。
* React 文件页包含分支切换、服务 Operation、桶访问模式、上传、ETag 覆盖/删除、
  目录前缀与分页、下载。状态轮询只读 metadata；加载目录/文件会冷醒 Writer。

范围是 `neon-object-rest-v1`，明确 `s3_compatible=false`。Admin 配置服务，Editor
写入和创建下载链接，Viewer 可用 GET/HEAD 读取。当前 UI 下载使用 presign，需要
Editor；完整 Viewer 文件 UI 和独立应用 storage credentials 仍需后续实现。
NGINX 超限 413 的错误格式仍不同于应用 JSON envelope；UI 预检 8 MiB，接口调用方
必须先判断状态码，不能假定所有边缘错误为 JSON。

## 3. Helm、凭据与真实依赖

产品 bucket `neon-product-blobs` 使用独立随机受限账号；不是 MinIO root，也不是
数据库 page bucket 的凭据。Secret 由运营方在受保护目录生成，公共 Git/values/
镜像/日志都没有明文密钥。API/Worker 只挂载只读 config；root 只用于有界初始化 hook。

实际 IAM 验证要求产品 bucket 列举成功，数据库 bucket 返回明确 AccessDenied。
探测网络失败不能登记为拒绝访问通过。策略只允许产品 bucket 的位置/列举与
GetObject/PutObject，不授予删除 blob 或管理权限。

八阶段 Linux 部署保存完整 prior values、metadata 备份、PVC/Secret 身份和回滚镜像，
通过源合同、预检、安装、就绪与完整 manifest 审计。保护 live SQL CA 身份
`lab.neon.local`，不拿新建实验室默认名称替换现有证书。旧 proxy-api/resizer 只在
确认无消费者后退役；两个 PostgreSQL、两个不同身份 Pageserver、三个 Safekeeper
和每节点 Agent 分别有职责，不能根据相同镜像误删。

## 4. 当前发行验收

### 4.1 Linux 开发与供应链

| Gate | 结果与边界 |
| --- | --- |
| Go / 独立 PG / PostgREST | 367 通过，0 fail / 0 skip；gofmt、vet、race；不使用现场 metadata 做 CI |
| Better Auth | 公开 CI 的独立 PG 与原生 SQL TLS 正反例成功；现场 UI 另验 |
| Web | 13 Node 检查、strict TypeScript、Vite、format 成功 |
| Unified Helm | 15 Node 检查、16 个负配置拒绝、10 lint/template/package 成功 |
| 公开 registry | 七镜像匿名 OCI manifest/config、source/license、hash 一致 |
| 实际节点拉取 | 3 节点 × 7 镜像，21 次 CRI digest 核验成功 |
| 实际 S3 IAM | 产品账号成功读取目录、数据库 bucket 明确拒绝 |
| 实际部署 | 八阶段成功，完整 values、PVC/Secret 与 manifest 审计成功 |

### 4.2 从 Linux Chromium UI 开始的串行回归

每个 suite 使用新 attempt，retries=0、workers=1；API 负例是 UI 流程的补充。
每组末尾实际核对 managed VM/运行 runner 为零。密码、token、cookie 和 private
fixture 没有进入公开报告/截图；原始失败也不覆盖。
当前 `7162449` 八个切片共 126 项 UI/真实协议检查通过。基础产品的资源清理原
失败仍保留，最终同一 Job/Pod UID 续观成功；此汇总是功能验收，非稳定性/SLO 准入。
Job 名与原始时间用 UTC；本轮续验的用户日期为 2026-10-10（北京时间）。

| Suite | 当前发行结果 | 核心验证 |
| --- | --- | --- |
| Object Storage | `7162449` 19 通过；`publication-ui-20261010122440` | bytes/SHA、ACL/Range/HEAD/304、CAS/并发、父子隔离、冷醒、关闭/恢复/再启用 |
| native-product | `7162449` UI 14 通过；`publication-ui-20261010122723`；清理 Gate 原失败保留，同 Job/Pod UID 后续只读续观为零并通过 | UI 创建/SQL/目录/冷醒/监控，Worker 故障接续，操作 GET 故障不重复 POST |
| backend-credentials | `7162449` 8 通过；`publication-ui-20261010124541` | 分支/模型范围、一次明文、重放、轮换、撤销；不等于真实 AI 推理 |
| data-api | `7162449` 20 通过；`publication-ui-20261010121250` | 真实 PostgREST/RLS、双主体隔离、手动/自动零与请求冷醒 |
| restore | `7162449` 13 通过；`publication-ui-20261009154408` | 原失败项目复用、目录异步修复、时间/LSN、当前目录不投射、幂等和保留边界 |
| lifecycle | `7162449` 23 通过；`publication-ui-20261010120808` | 两 Reader 只读/WAL/独立零与冷醒、服务关闭、保留删除/恢复及原 Operation 失败重试 |
| console-invitations | `7162449` 6 通过；最终公开源码升级后 `publication-ui-20261010130324` | 受邀注册、跨组织负例、Viewer、撤销；不启动 Compute |
| managed-auth | `7162449` 23 通过；`publication-ui-20261010121730` | 实际账号/会话/JWT/RLS、原生继承、分支隔离、自动零/登录冷醒 |

Worker 受控中断期间原创建 Operation `op_289ddbf779d0de02aa1ae53b` queued；
恢复后 Worker epoch 77→78，原 Operation 成功。该场景验证独立进程接续，不是
多副本 HA、节点故障或跨实例外部栅栏认证。

2026-10-10（北京时间）继续串行回归。生命周期恢复的终态故障是明确的 operator
注入；UI 在原 Operation 内恢复，不把注入故障称为自然节点故障。测试结束正常
停止全部运行实例，再按通过回执确名保留删除，记录 held tombstone 与恢复期限；
所有数据库/WAL/blob、原身份和证据保留，物理 GC 仍未启用。

### 4.3 原始失败和恢复保留

1. MinIO hook 初版 128 MiB 容器限制下 OOMKilled/137；节点各有约 9–10 GiB 可用，
   不能归为宿主内存耗尽。hook/探测改为 request 128 MiB、limit 512 MiB、
   `GOMEMLIMIT=128MiB`，不扩大基础设施并发。
2. 失败升级留下无 owner 注解的终态 hook，严格 inspect 拒绝。修正两种 hook 的
   明确 Helm 所有权；归档后按原 UID/resourceVersion 退休终态 Job，不削弱通用
   adoption 规则，不删除任何 bucket/Secret/PVC。
3. 第一组对象 UI 已通过前 14 项，但 `nosniff` 响应头重复。修正边缘重复 singleton
   头，由 NGINX 提供唯一值，保留应用文件的 sandbox CSP；没有放宽断言。
4. 原 Job `publication-ui-20261009131515` 与项目 `prj_755cce96dbc9bcc6` 原失败保留。
   `publication-ui-20261009132654` 用原项目/父子分支完成 4 项恢复，不新建替代资源。
   后续 `6d558b0` 的 19 项和当时 `d4549a8` 的 19 项各有独立回执。
5. Data API 的 `publication-ui-20261009141217` 在 15 项通过后，冷醒返回真实
   PGRST000/503。只读采集也失去 Kubernetes 连接；恢复观察原 Pod UID 得到原
   浏览器失败，未重新提交任何业务动作。三个 dedicated etcd 日志尾部样本在
   14:12–14:22 UTC 中分别有 43/57/49 条慢 fdatasync，最大值
   12.136/21.915/12.140 秒；这是尾部样本最大值和条数，不是全窗口 p99/总量。
   中途节点 PSI some avg10 约 40%，仍约 9 GiB 可用内存；RKE2 自行失联/重启，
   操作者未重启或放松持久化。首次 journal-only 采集不含 dedicated etcd 日志，
   不以它的零条数证明没有 fsync 故障。
6. 压力归零后，原项目 `prj_7d819cdb6604a60d` 由
   `publication-ui-20261009142815` 完成 5 项显式 UI 恢复：原 SQL/RLS 行保留、
   原临时 issuer 私钥未保存故明确轮换、手动/自动零后真实请求冷醒、停止服务
   与 held 保留删除。原失败回执未覆盖，后续完整新 attempt 单独登记。
7. 第二组完整 Data API `publication-ui-20261009143421` 同样在冷醒失败，
   此次对应日志样本再有约 10.824/12.969/10.827 秒 fdatasync，Adapter routes/wake
   读取错误与 Worker 重启在同一时间窗。稍后的 PSI=0 不能反证先前没有停顿。
   `publication-ui-20261009144445` 对其原项目 `prj_16e815687f0144a4` 又完成
   同样 5 项恢复，并释放运行资源/配额。没有修改业务断言或 Neon 源码。

按用户方向，本次暂不调优宿主硬件、不并发进行压力测试；保留硬件限制和失败，
继续串行功能验证。两组恢复成功仍不把两次完整 Data API 失败改写为成功。
2026-10-10 后续完整新 attempt `publication-ui-20261010121250` 通过全部 20 项，
包含自动空闲缩零后的真实 Data API 冷醒；原两次失败和对应恢复回执仍分别保留。

### 独立目录 UI 缺陷

`publication-ui-20261009144903` 的历史恢复在创建后置数据库之前失败：角色
Operation 已 succeeded，但等待目录刷新时输入的数据库名称被无条件清空。
浏览器记录显示 owner 已选中、名称变空、按钮 disabled；`Catalog.mutate` 在所有
动作完成后同时清空角色与数据库表单，存在明确异步竞争。这不是存储恢复失败，
不能归为硬件问题。

修复只清理实际提交的表单及仍相等的旧字段，保留其他表单和后续编辑；敏感密码
也只清理对应的已提交动作。新增 live 测试显式延迟只读刷新，验证名称仍保留。
原项目正常 suspend，数据/原失败保留；恢复模式在同一项目新建唯一探测表及新
历史目标点，旧表和历史回执不改写。新发行 `7162449` 已通过全部公开 CI、匿名
registry、21 次节点拉取及八阶段升级/审计。`publication-ui-20261009154408` 在同一
项目通过 13 项恢复，包括显式延迟目录刷新时数据库名称保留；时间点/LSN 两种历史
数据恢复、目录隔离、恢复 Endpoint 冷醒、重放与窗口负例均通过，最终 VM/runner 为零。

## 5. 资源、复测与交接

基础组件保持资源预留，suite 串行，不并发编译、拉取、压测与真实 SQL。停止 Compute
不释放组织 50 项目逻辑配额；只对确名、具有通过回执的自有测试 fixture 正常解除
保护/CAS 保留删除，记录 recover_until 与 held tombstone，保留所有数据和 evidence。
最终清理 `storage-quality-cleanup-attempt2` 归档并按 UID/resourceVersion 前置条件
退休 31 个自有终态测试 Job，115 个已不存在；未删除 SQL/WAL/blob/PVC/Secret。
Endpoint desired=active 表示可接受后续冷醒；observed=suspended 加 VM/runner 缺失
才是 Compute 为零。不要修改 metadata 把两种状态强行改成相同。

2026-10-10 基础产品 UI Job `publication-ui-20261010122723` 的 14 项通过，原创建
Operation `op_79353f154422382dec880dce` 在 Worker epoch 88→89 后接续成功。
最终清理观察仍见旧 Runner `cp-a74568638df31b53-hrhjs`，原 UID
`c768ed64-d101-4636-b92a-d3099ee4d112`，已处于 Terminating；因此总采集回执为失败。
同 Job/Pod UID 的只读续观也未在其 120 秒内收敛，不覆盖原失败。node2 API 曾拒绝
连接，12:27 UTC 后 dedicated etcd 尾部样本 53 次慢 fdatasync，最大 12.503 秒，
仍约 9 GiB 可用内存。后续 CRI 按原 Pod UID 证明运行容器已经不存在，VM 也为零；
Kubernetes Pod 记录回收仍须单独通过，不把 CRI 零视为全部 Kubernetes 清理成功。
操作者没有强制删除 Pod、重启 RKE2 或改写清理时限；硬件/节点稳定性暂后置。
`storage-native2-original-observation-attempt2` 对同一原 Job/Pod UID 的后续只读观察
成功，原 14 项 UI 结果不重跑，全部 VM/Runner 记录已正常收敛为零，三节点/控制面
重新 Ready。原清理失败与第一次续观失败仍保留；不据此批准资源回收延迟 SLO。

受信 Linux 的自动步骤见 [TESTING.md](TESTING.md)，手工对象流程及协议例外见
[OBJECT-STORAGE.md](OBJECT-STORAGE.md)，完整安装/回滚/容量诊断见统一 Helm runbook。
自动化操作者需要自己已有的 Linux/Kubernetes 与管理员安全输入；公共 clone 不包含
这些秘密，不能承诺无凭据访问原环境。API contract、锁和代码足以让其他维护者在
授权环境复现；未知结果先观察原 Operation，不重新创建或自动重放 SQL。

### 5.1 公开发行与部署复现

[统一 Helm v0.1.6](https://github.com/william-lbn/neon-helm/releases/tag/v0.1.6)
锁定 Chart 源码 `ae2e9013172ccb6e50dd30f547d37f1106a7f3d4`；
[main CI 38053540900](https://github.com/william-lbn/neon-helm/actions/runs/38053540900)
和 [tag/release CI 38053672551](https://github.com/william-lbn/neon-helm/actions/runs/38053672551)
均成功。十个 Chart、index、SHA256SUMS 和版本锁已公开。Linux 不携带 GitHub 或
registry 凭据的标准 `helm repo add/update/pull` 下载十个包并通过全部 SHA256。

首次匿名 Git 精确源码审计发现 Web 配置校验和 annotation 有差异：私有 Windows
源码包的 CRLF 与 Git 中 LF 字节不同；解析后的 ConfigMap 数据完全相等，原差异
及字段 hash 保留。没有忽略严格审计：从已通过公开 CI 的匿名精确 Git 源码进行
八阶段正式升级 `20261010/unified-125901` 后，独立重新 clone 同 SHA 的完整
manifest/hooks/镜像/PVC/Secret/零实例审计全部成功。升级后 Linux Chromium 又通过
六项邀请注册/权限检查，不启动 Compute。操作标准是从 Linux Git/正式 tgz 部署，
不拿 Windows 脏工作树压缩包代替公开代码。后续仅文档提交不移动已发布标签，
也不改变实际应用源码 `7162449` 的七镜像锁。

## 6. 后续实现与独立准入

1. 对象存储：统一 storage scopes/应用钥匙、S3/SigV4 服务端、multipart/CORS、
   事件 outbox、独立受限 gateway、全局/物理配额、引用安全 GC 与独立恢复演练。
2. Functions：不可变 Node.js 24 bundle、每执行租户 microVM 隔离、依赖/版本回滚、
   Secret 引用、HTTP/SSE/WS、空闲生命周期、cron/文件事件的 durable retry、日志 UI。
   没有实际隔离执行就不能把 SQL 工作台或通用 Job 当作 Functions 已实现。
3. AI Gateway：供应商配置的安全页面与 secret adapter、模型路由、真实流式推理、
   限额/用量/撤销。尚无模型凭据，不能调用 Codex 自身账号充当用户推理上游。
4. 独立硬门槛：外部连接账本/跨实例 fencing、小数 CPU/完整内存归还、全栈恢复、
   HA/DR、全链路可信 TLS、完整租户攻击矩阵、长期 SLO/告警和共同磁盘停顿诊断。

当前成功回归没有消除共享 NVMe 的既有 fsync 故障记录，也没有提供独立物理故障域。
按 [PRODUCT-COMPLETION-PLAN.md](PRODUCT-COMPLETION-PLAN.md) 和
[PRODUCTION-GATES.md](PRODUCTION-GATES.md) 逐项开发与验收，不虚标官网全功能完成。
