# 2026-10-10/11：Compute 生命周期交付与全切片回归

## 1. 交付结论与版本

本增量完成独立 Compute 删除、保留数据和 Writer 重建。React UI 原生管理一个
Writer 和多个 Reader；删除某个 Endpoint 不删除 Timeline，也不改变其他 Compute。
旧 Selector 永久关闭，新 Compute 获得新 Selector。OpenAPI、迁移、Driver、UI、
Linux 回归、失败恢复和 Helm RBAC 已对齐。完整合同与图见 [ENDPOINT-DELETION](ENDPOINT-DELETION.md)。

**当前仍是预览产品，未取得完整生产准入。** 下述是精确版本的功能验收。
Functions 执行、真实模型推理、HA/DR、外部跨实例栅栏、全链路可信 TLS、完整内存
归还、小数 CPU、物理 GC/TTL 等仍有实现或独立验收缺口，见 [生产门槛](PRODUCTION-GATES.md)。

| 输入 | 本次锁定值 |
| --- | --- |
| 运行控制源码 | `0ba173262d53d9bd634b52347ce417ae83033a85` |
| 公开 CI | [38058862506](https://github.com/william-lbn/control-plane/actions/runs/38058862506)，12 个 Job 成功 |
| 七镜像 | GHCR/Docker Hub 同源码 manifest；实际部署用 `locks/control-plane-0ba1732.json` digest |
| OpenAPI / migration | `0.10.1`，58 paths / 87 operations；前向 `018_endpoint_deletion.sql`，以实际 migration 文件名为准 |
| 统一 Helm | 十个 Chart `0.1.7`；实际 RBAC 源码 `28183a5a552bccadd9f386721d89453bf03cbc0a` |
| Helm CI | [38060646356](https://github.com/william-lbn/neon-helm/actions/runs/38060646356)，10 Chart / 15 Node / 16 负配置成功 |
| 控制仓库兼容 Chart | `0.8.1`；同步 Worker observer 和 combined profile，只作为控制仓库入口 |
| 数据面 | own fork 的 Neon `1f30cd0`、Autoscaling `c0052f5`、PG16 `a42351f`；2026-09-30 固定镜像，不改 fork 运行源码 |
| 现场 | 三台 RKE2/Linux，UI `http://192.168.146.100:30788/`；共享宿主磁盘，非独立故障域 |

本轮原始 Job 时刻为 UTC（2026-10-10）；完成验收时用户日期为北京时间
2026-10-11。后续测试/文档提交不自动改变实际运行镜像。不要拿新文档 HEAD 的
CI 镜像冒充已回归的 `0ba1732`。历史报告及镜像锁按版本保留。

## 2. 安全调谐和现场修复

删除必须带准确 Selector、带引号的 If-Match、稳定 Idempotency-Key、有效项目
授权和 Session CSRF。事务锁顺序为项目→分支→Endpoint。原 key/version/body
在删除完成后重放仍返回同一 Operation；恢复只重试原 Operation。

Worker 四阶段：关闭目标 Proxy 路由、按 UID/resourceVersion 正常退休自有 VM、
观察其 Runner 全部消失、在有效租约下写 held tombstone。Succeeded Runner
仍存在时也不能登记回收完成。数据/角色/WAL/文件/凭据/模板均保留。

开启或失败待恢复的 Data API/Auth/Object Storage 阻止删除其 Writer；须先显式
停用服务。新 Writer 验证原分支密码，复用保留的 cloud_admin/control_probe verifier，
保护已有 Reader 和监控；其管理签名身份与旧 Endpoint 独立。项目恢复排除之前
单独删除的 Endpoint。物理清理和所有外部连接的分布式 fence 尚未开放。

原现场失败暴露 Worker `list pods` 403。统一 Chart 已加入命名空间只读 observer
Role/Binding，Worker get/list，分离 API 仅 get。没有 Pod 删除/写入/exec 权限。
Kubernetes 无法按 label 约束 RBAC list，Driver 对每个返回对象另验项目/Endpoint
所有权。现场四项正负 can-i 和两种渲染 profile 均验证；控制仓库兼容 Chart 与
YAML 解析式 CI 同步修复，防止后续重装漏权限。

SQL 工作台提交成功或失败后清空已提交密码；pending 期间另一份新输入保留。
多语句预处理被明确拒绝，测试准备改为分别提交单条 SQL，没有改变数据库功能。

## 3. 本版本验收证据

### 3.1 开发、供应链和部署

| Gate | 结果与边界 |
| --- | --- |
| Go + 隔离真实 PostgreSQL | 383 pass / 0 fail / 0 skip；format、vet、race、迁移、权限、幂等、PostgREST RLS |
| React / TypeScript | 13 Node 检查，strict typecheck、format、Vite build；真浏览器另列 |
| Better Auth / SQL TLS | 独立 PG 与原生 TLS 正负检查；不替代全链路可信 TLS |
| 公开 registry | 7 个匿名 OCI manifest/config/source/license 核验成功 |
| 节点拉取 | 三节点 × 七镜像，21 次实际 CRI digest 核验成功 |
| 统一部署 | `20261010/unified-144500` 八阶段成功；完整 private overlays、metadata 备份、PVC/Secret/CA 身份和 manifest 审计保留 |
| Worker observer | list neon=yes、delete pods=no；API list pods=no；Worker list default=no |

兼容 Chart 的八项解析式 RBAC 正负检查是本轮交付修复的门槛；公开 Linux CI
必须再次验证本次提交。现场镜像没有因此自动升级。

### 3.2 从 Linux Chromium UI 开始

九套共 **146 项**功能检查通过；workers=1、retries=0、suite 串行。
每组末尾核对 managed VM 和所有同前缀 Runner 为零。真正执行 SQL、Auth、文件、
PostgREST、权限负例和控制操作；没有把测试上游当作 AI 推理。

| Suite | Checks | 原始 UTC Job | 核心范围 |
| --- | ---: | --- | --- |
| endpoint-deletion | 19 | `publication-ui-20261010145208` | 独立 Reader/Writer 删除、lost 202 重放、依赖阻止、原密码重建、空分支和项目恢复 |
| restore | 13 | `publication-ui-20261010193607` | timestamp/LSN、历史 SQL/数据库/角色隔离、原失败项目复用、请求观察故障与冷醒 |
| lifecycle | 23 | `publication-ui-20261010194145` | 双 Reader WAL/只读/独立零与唤醒、服务关闭、保留删除/恢复、原 Operation 重试 |
| data-api | 21 | `publication-ui-20261010194605` | 真实 PostgREST/RLS、主体隔离、缩零和请求唤醒、启用服务阻止 Writer 删除 |
| managed-auth | 23 | `publication-ui-20261010195003` | 实际注册/会话/JWT/JWKS、分支账户继承与会话隔离、RLS、自动零和登录唤醒 |
| object-storage | 19 | `publication-ui-20261010195432` | 真实 bytes/SHA、ACL/HEAD/Range/304、ETag 并发、父子隔离、保留恢复和冷醒 |
| native-product | 14 | `publication-ui-20261010200951` | 项目/纯数据分支/Writer、真实 Proxy SQL、分支隔离、监控、Worker 接续与 GET 故障 |
| backend-credentials | 8 | `publication-ui-20261010201117` | 一次凭据、分支/模型范围、轮换、撤销、失权；不等于真实推理 |
| console-invitations | 6 | `publication-ui-20261010201138` | 邀请注册、跨组织与 Viewer 负例、撤销；无 Compute |

已经通过的自有测试项目在保留删除后释放逻辑配额；未增加 maxProjects。
数据、Timeline、WAL、blob、Secrets、操作与所有失败/成功记录保留。
原生 Worker 故障注入验证新 epoch 接续原队列，不等于多副本 HA。

### 3.3 原失败 Operation 的独立恢复

另有 **23 项**恢复检查，不能混入九套完整回归或覆盖原失败。

| 原失败场景 | Checks | 原 Operation | 恢复 UTC Job |
| --- | ---: | --- | --- |
| endpoint-deletion | 6 | `op_d705da98ae7d7d988d0493fc` | `publication-ui-20261010144941` |
| historical-restore | 5 | `op_d54628ebe36ea30b368917c4` | `publication-ui-20261010150932` |
| catalog-database | 6 | `op_b4eec787ad9883365f1aab55` | `publication-ui-20261010193502` |
| native-writer | 6 | `op_7ef4861403e1f20965e17489` | `publication-ui-20261010200832` |

1. Endpoint DELETE 的已受理 202 被受控丢弃；原 Worker 403 失败记录保留。
   RBAC 修复后 UI 重试原 ID，正常退休剩余 Runner，Writer/Reader 原 SQL 不变。
2. 历史分支创建时 node3/Worker 中断，原 Operation 失败；重试原 Timeline/固定
   timestamp→LSN 解析点成功。SQL 观测 LSN 可早于最终解析点，不要求错误的相等。
3. CREATE DATABASE 曾已成功但 COMMENT 标记尚未完成，连接中断后不能静默
   收编同名无标记数据库。只读验证原目录意图、owner、DB/role OID、role branch
   marker 和空用户目录后，运营者用条件 DO 再查相同不变量，仅补原数据库 COMMENT。
   随后 UI 重试原 Operation 成功。这是明确运营修复，不声称自动 unknown-DDL
   收编；安全 guard 保留。详细步骤见 [Operation 恢复](OPERATION-RECOVERY.md)。
4. 原生子分支 Writer 测试期间三个 API 入口超时/拒绝，原 Job 最终失败；只读
   采样三节点 I/O full avg10 约 37–57%，内存压力接近零，RKE2 自动重启记录
   保留。恢复后同一 create_endpoint Operation 从 attempts 2→3 成功，实际父子
   写入隔离与正常缩零通过。随后使用新 attempt 完成整套 native 回归。

SQL 准备缺陷、项目配额不足、临时 registry TLS 握手失败、采证器超时、节点停顿
均保留原 attempt。硬件原因允许暂时忽略以继续功能验证，但不能据此通过稳定性
SLO。密码框遮挡截图；ARIA/网络/fixture 仍可能含敏感输入，原始证据只保留在
受保护工作区，不上传公开 Git。

## 4. 手动部署和复测

1. 匿名克隆 neon-helm，选择完整发行与 `control-plane-0ba1732` 锁；按
   UPGRADE.md 的 inspect/preflight/apply/verify/audit 顺序执行。完整备份现有
   private values、metadata、原镜像与 Secret/PVC UID；采用原 CA 证书名称。
2. 等 API/Worker/Web 分离进程及基础服务就绪。查看 Swagger 的 87 操作和当前
   capability 门槛。首选一个专用 1 CPU/1 GiB Compute 项目，不并行启动测试。
3. 按 ENDPOINT-DELETION.md 第 4 节完成两 Reader、真实 WAL、独立删除、
   服务依赖、Writer 原密码重建、空分支、项目恢复全过程。
4. 逐套执行公开 TESTING.md 的 Linux Playwright，保存新 attempt、Job/Pod UID、
   Request/Operation ID 和当前 image/source digest。失败先看原 ID/步骤，再按
   OPERATION-RECOVERY.md 恢复；不知道写入结果时不要重新创建另一份资源。
5. 每组停用自有服务/Compute，等待 VM/Runner 正常归零。只按已通过回执和准确
   名称保留删除自有项目，释放运行和逻辑配额。归档终态 Jobs 后按 UID/RV 删除。
   不删除数据库、对象、WAL、PVC、Secrets、失败证据或用户项目。

## 5. 下一阶段与独立门槛

下一项为实际 Functions：不可变 Node.js 24 bundle/version/env、独立 NeonVM、
受限 SQL 身份、网络/进程隔离、HTTP/流式执行、日志、分支继承、缩零与唤醒、
回滚和 React UI。官方 Functions overview 在线核验到 2026-10-02，deploy 文档
核验到 2026-10-09；不把未开源的托管实现误认为已有代码。

Foundation/Driver/现场功能/生产准入分别记录。Cron/object triggers、WebSocket、
外部模型/SMTP/OAuth、完整 S3/multipart/GC、HA/DR、外部 fencing、可信 TLS、
小数 CPU 和完整内存归还继续逐项实现。没有外部调用凭据不阻止本地 Functions
HTTP/SQL 闭环；实际模型推理仍须运营方配置真实上游。
