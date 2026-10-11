# 2026-10-11：Functions 资源恢复、发布基础层与 Helm 0.1.11

这是 0.1.11 的历史验收。当前 0.1.12 的精确版本和独立回归见
[SQL 身份交付](DELIVERY-2026-10-12-SQL-IDENTITY.md)。下文结果不计入新版本通过。

## 1. 结论和精确运行版本

控制面运行源码为 `932f5adbd9db5babce1ad05aa280cc34e452e193`；统一 Helm
0.1.11 源码为 `b2f8fcd952bd9c685858cbfb57e1142605b9b13c`，不可变标签为
`v0.1.11`。API 与 Worker 分开部署，metadata 仍为 21/21 migrations。
Neon/autoscaling/PostgreSQL fork 源码和镜像没有在此增量修改。

Functions Service/VM 恢复与原子 metadata 发布已有内部 Go 代码，但尚未注册
Worker 执行 action、公共部署/调用接口或执行 UI。`functions.enabled=false`。
OpenAPI 仍是 0.11.0 / 61 paths / 90 operations。这个运行版不是官网全部功能
或生产准入；完整门槛见 [PRODUCTION-GATES](PRODUCTION-GATES.md)。

## 2. 代码与供应链验收

| 门槛 | 本次独立结果 |
| --- | --- |
| Linux Go / 真实专用 PG / race / vet / 构建 | Job `dataapi-quality-20261011124348`，593 pass / 0 fail / 0 skip；源码归档 `2e92b5036188bb65e48eba7364d03f123a1251074c23796dac006af5e6c494e4` |
| 实际 MinIO / Kubernetes foundation | Job `functions-foundation-20261011124840`，3 pass；Service 原 UID 恢复、immutable bootstrap、实际 CRD dry-run；没有持久执行 VM |
| Linux Web | Job `restore-web-1791721442`，17 项测试、类型/构建、3 Chart 和 8 RBAC 测试 |
| 公开 control CI | Run `38141439187`，13 作业全部成功，8 个下载 artifact 的摘要核验 |
| 七镜像匿名 OCI | manifest/config/platform/license/source label 独立核验；源码均为 `932f5ad` |
| 三节点 CRI | 七镜像 × 三节点，21 次串行 pull / digest / source 核验 |
| Linux Helm | Job `helm-quality-1791725104`，15 工具测试、10 Chart、16 非法配置拒绝、10 包 |
| 公开 Helm CI | main `38143225335`、tag `38143264928` 成功 |
| 匿名 Helm 消费者 | 0.1.11 十个包、index 和 SHA256SUMS 下载/摘要验证通过 |
| 已有集群升级 | 八阶段 inspect/preflight/apply/verify，加完整 manifest/Secret/PVC 审计通过；原 values 私有保存 |

公开来源：[控制面 CI](https://github.com/william-lbn/control-plane/actions/runs/38141439187)、
[Helm 发行](https://github.com/william-lbn/neon-helm/releases/tag/v0.1.11)。
镜像细节位于 Helm `locks/control-plane-932f5ad.json`。历史锁保留供回滚核对，
不覆盖原失败。源码归档与 Linux 格式化后编译文件之间的格式差异已单独核验，
不能把本地工作树或后续 SQL 增量作为该镜像的源码。

## 3. 新镜像的现场 UI 回归

只运行 Linux Playwright，一组一组测试。实际采用已提交 `932f5ad` 的 TS 源码，
正常 suite 没有从当前工作树抽取未提交代码。明确恢复的 TS override 单独保存摘要
和原失败链。十组实际串行回归已结束，结果如下。

| 场景 | 通过检查 | 范围 |
| --- | --- | --- |
| Functions metadata | 8 | 只读定义/历史、隔离、执行入口关闭 |
| Native | 13 | 项目/分支/Writer、Proxy SQL、冷醒；四种 GET 观察故障、一个 project POST |
| Managed Auth | 23 | 注册、JWT/RLS、分支隔离、缩零冷醒、停用 |
| Data API 原项目恢复 | 5 | 保留行/RLS，显式 test issuer 轮换，手动与自动缩零后的首读冷醒，正常保留删除 |
| Object Storage 原项目续测 | 15 | 原对象及密码、原失败 suspend 恢复、实际冷醒、分支继承/隔离、CAS、链接代次、保留删除/恢复 |
| 历史恢复 | 11 | 正常 UI timestamp/LSN 历史点恢复和目录隔离 |
| Endpoint 删除 | 19 | 正常 UI 两个 Reader、Writer 删除/替换、原 SQL/密码与 WAL、保留删除/恢复 |
| Lifecycle 原项目续测 | 21 | 原两个 Reader/Operation、拒写、独立零/冷醒/WAL、原项目保留删除/恢复、真实 Data API/RLS、撤销凭据不复活 |
| 应用凭据 | 8 | 原已授权测试项目、Key scope、轮换、撤销与数据入口 |
| Console 邀请注册 | 6 | 正常 UI 邀请接受、注册登录、权限隔离、拒绝非法流程 |

合计 **129**：七组正常场景 **88**，三个明确原项目恢复/续测 **41**。
总回执 `functions-publication-serial-ui-202610111631`；每个独立原 Job/Pod UID、
实际运行源码、正常回归/恢复范围、恢复 TS SHA256 和退出后 VM/Runner 观察均保留。
没有把历史 0.1.10 的数字或本次失败中的部分检查加进合计。

Data API 原 Job `publication-ui-20261011133615` 在 16 项后失败；自动缩零后的
首个 Data API GET 返回 HTTP 503/PGRST000，Proxy 报控制面请求失败。同期实际
Kubernetes API read 发生连接拒绝；这不足以单独证明硬件根因。原 VM 后来实际
Running，原自动 suspend Operation 成功，Worker 后续重新启动的事实也已保存。
该原失败未改为通过。

恢复 Job `publication-ui-20261011135113` 继续原 `prj_c25ced4e89982a72`，
保留 `br_c25ced4e89982a72` / `ep_c25ced4e89982a72` 和原密码数据。原测试
issuer 私钥未保存，续测显式通过 UI 轮换测试 issuer，不创建替代项目；5 项通过。
新 API 稳定时的首读恢复不证明 API server 故障期间的业务连续性。
不把原失败中 16 项加进完整通过计数，也不把恢复称为正常 suite 首次通过。

Storage 原 Job `publication-ui-20261011135620` 在 7 项后等待缩零冷醒目录失败；
失败清理的原 suspend `op_0e999980fe4fb93a203150d1` 为 retryable failed。
续测 Job `publication-ui-20261011152607` 使用完全相同的已提交 Storage suite，
从原 `prj_221b08f71bfd7bad` 重试原 Operation，验证原对象字节，再完成分支和
生命周期续测。不替换项目/密码/对象，不把 7 项失败过程计入完整通过结果。

Endpoint Job `publication-ui-20261011153125` 本身已经通过 UI 完成最终保留删除。
随后私有清理工具要求项目仍 ready 而失败，尚未发出第二个删除请求；修复为对
原项目 tombstone 和 succeeded delete Operation 的只读观察后确认通过。
该工具失败与 19 项 UI 正常通过分开保留，不重建测试项目。

### 3.1 资源与 Worker 原日志

实际 Runner Pod 缺少常规 CPU/memory requests；但自有 autoscale-scheduler 在
`autoscaling/pkg/plugin/state/pod.go` 的 `podStateForVMRunner` 中，根据 VM
resource 注解及 requested/approved scaling 核算预留，初次采用 guest `.use`。
不能据 Pod requests 为空断言无调度核算，也不能盲目加固定 limits 截断热扩容。
后续仍需独立验证宿主 QEMU 开销、kubelet eviction、其它 scheduler 和真实预算。

本次原 Worker previous log 为 14:03:36 UTC 的 leadership acquisition unavailable，
容器 Error/exit 1；Adapter 先前是 Unknown/exit 255，均不能据此归为 OOMKilled。
本轮拟补独立 Worker 的启动租约等待：取得租约前不 ready、不执行调谐，失去已取得
领导租约后仍停止。它不等同 HA 或外部 fencing，也不解决共享 NVMe 的物理瓶颈。

后续 Lifecycle 原 Job `publication-ui-20261011153910` 在 3 项后叶 Endpoint 创建
modal 超时。三 API read 同期连接拒绝，原创建 Operation
`op_3de06d5953fbdcffeab2a746` 为 retryable reconcile_failed；不是未提交请求。
只读三节点证据记录约 8.5–9 GiB MemAvailable、无当前 memory pressure、etcd
apply 约 7–14 秒和 API readiness 失败。apply 延迟不自动等于 fsync 延迟，也不足以
单独确定 Windows/宿主/虚拟磁盘根因。API 恢复后从原叶分支/Endpoint/Operation
进行明确 UI 续测；该失败与续测须独立记录，不能计为正常首次通过。

原项目首次续测 Job `publication-ui-20261011155809` 已完成叶/父保留删除和第一
Reader 后，在第二 Reader 的 create Operation 上失败（依赖 HTTP 500）；保留
原 `op_f94c3c241638d77b1b4ecd9b` 和 `ep_387b4fa15c066b43`，没有新建替代 Reader。
接续 Job `publication-ui-20261011161351` 在第一 Reader 首读 504 后失败。
最终 Job `publication-ui-20261011161946` 通过 **21** 项：明确复用原项目/两个
Reader、原 SQL/密码/held branches，第二 Reader 从 UI 重试原 Operation，完成
两个只读负例、独立 1→0→1/WAL、项目恢复与真实 Data API/RLS/撤销凭据测试。
最后实际 managed VM 和 Running/Pending Runner 都为零。首次失败 3 项、续测失败
6 项/2 项不加进通过合计；该 21 项是恢复阶段范围，不能称为正常首次 suite 通过。
显式恢复 TS override 的摘要和原失败链单独保存；后续公共测试已加入两个恢复阶段。
原项目通过正常 retained-delete Operation 释放逻辑 quota，SQL/密码/对象/日志不删除。

历史 `dda5d34` / Helm 0.1.10 的 135 项基线见
[上一个发行](DELIVERY-2026-10-11-FUNCTIONS-BACKING.md#33-历史-0110-发行与逐套回归)，
不能作为当前镜像的验收。所有原记录、数据、密码和 SQL 文件保留在私有维护者环境。
公共 Git 仅提交脱敏合同和结果，不包含私有 Python transport。

## 4. 本轮下一阶段与未准入项

后续 SQL 增量补强列级授权、字面 catalog 前缀、同 schema 非普通表、转授、额外
表/sequence 权限和执行 trigger 检查，提供实际运行连接核验；同时补独立 Worker
启动数据库暂不可用时的等待。该增量已经独立通过 Linux Job
`dataapi-quality-20261011162836`：**610 pass / 0 fail / 0 skip**，包含实际 PG 权限、
SCRAM、RLS、运行会话核验、单连接 pool 超时后 Worker 恢复，以及 race/vet/build。
输入归档 SHA256 `3016184130d87f4f5393e8a274d4a19230da0b954417680f1ba603473b442e64`。
Linux Web Job `restore-web-1791736303` 通过 17 项、TS 类型/构建、3 Chart、8 RBAC。
运行代码、Linux 格式化后源文件和提交 provenance 单独核验。
该增量尚未部署到上述 `932f5ad` 镜像；不能把 610 项或恢复 TS 源码当成该镜像的
实际运行代码，也不能把 129 项旧运行镜像 UI 回归称为此增量的部署验收。

后续 Functions 仍需 immutable SQL 身份/分支 manifest、精确 Proxy database 路由、
Worker CA/config、artifact TLS 服务、真实 guest SQL、部署 action、排空与退休、
invocation ledger、公共 API/UI ZIP/config-only/调用/回滚、分支复制和零/冷醒。
AI 真实供应商凭据尚未配置；HA/DR、跨实例栅栏、小数 CPU、完整 RAM 缩回、
全链路可信 TLS、物理 GC 和其他生产门槛继续按独立验收推进。
