# 保留删除、恢复与多个 Reader 的候选验收 — 2026-10-07

## 1. 结论与范围

Go API/独立 Worker、PostgreSQL 迁移 014、React Console 已实现并在真实 Linux
集群完成项目/叶分支保护、保留删除、七天项目恢复、两个 Reader 独立休眠/冷醒、
Data API 删除协调与应用凭据永久撤销。本报告记录**候选版本**，公开发布镜像
和统一 Helm 的最终锁定、CI 与回归由对应版本发布报告记录；不能沿用旧镜像的通过结果。

本版本仍未通过全部官网功能或生产准入。物理 GC、独立 Endpoint 删除、TTL、
失败创建删除、分布式连接栅栏、HA/DR、可信 TLS、小数 CPU、完整内存缩回，
以及 Managed Auth、Functions、产品 Object Storage、AI 推理仍有开发和验收缺口。
准确合同见 [保留删除](RETAINED-DELETION.md)；后续任务见
[实施计划](PRODUCT-COMPLETION-PLAN.md)和[生产门槛](PRODUCTION-GATES.md)。

## 2. 代码与合同

| 项目 | 本次增量 |
| --- | --- |
| 运行语言 | Go API/Worker/Adapter；React/TypeScript Console |
| API | OpenAPI 0.8.0，50 paths / 69 operations；新增六个 lifecycle/protection/delete/recover 操作 |
| 元数据 | migration 014；删除 generation、恢复窗口、不可变 ID scope、tombstone、恢复 Operation 标记 |
| 并发 | parent lifecycle 数据库锁和触发器、同一 Idempotency-Key 同一 Operation、CAS 路由、VM UID/resourceVersion 删除 |
| UI | 保护与删除、依赖图、准确名称确认、操作历史/重试、已删除项目列表、七天恢复 |
| 保留 | 原生 Timeline/WAL/对象、Secret、审计和成功/失败测试文件均保留；GC held |

没有修改本次使用的 Neon/autoscaling/postgres fork 源码或基础镜像版本。
私有 Python SSH/证据采集工具在维护者工作区，未作为产品后端，也不发布到此源码仓库。

## 3. Linux 候选验收证据

| Gate | 结果 | 证据身份 |
| --- | --- | --- |
| Go/真实 PG | PASS：325 pass，0 fail，0 skip；格式、vet、race、授权、迁移、幂等/旧 Worker 拒绝和错误 peer | dataapi-quality-20261007142922 |
| Web | PASS：13 tests、零 skipped、格式、严格 TS、Vite 构建；三个 canonical Chart lint/package | restore-web 系列保留 attempt |
| 部署 | PASS：迁移 14；API/Worker/Web rollout 完成，保留完整旧 values/Secrets/PVC | deploy-145010 |
| 第一组 UI | PASS：18 checks；两个真实 Reader、叶分支删除、项目删除/恢复，最终 VM/Runner 0 | publication-ui-20261007145052；prj_1af9fbc112f27da2 |
| 扩展 UI | PASS：22 checks；加入实际 Data API/PostgREST/RLS 和 Backend 凭据生命周期；最终 VM/Runner 0 | publication-ui-20261007145916；Pod UID 4352070c-5732-4312-b1e8-0bf18c4af6f9；prj_ea532f86c92a8971 |

候选 API digest：`93d4132e92fbc8d354906eb5d2b93d7672f12f44f3bcb71dc245d54eb731b549`。
候选 Web digest：`789f7d800fe4cd8439a6f43f23d2d177234b449c2e15c4062e4330a701156ee0`。
这两个 `neon.local` 候选用于提交前测试，不是匿名公开拉取的最终发布镜像。

真实浏览器断言包括：

1. 专用项目和父/叶分支从 UI 原生创建；根分支、子依赖及保护阻止误删。
2. 活跃叶 Compute 通过四步删除回收；原请求重放得到同一 Operation，Endpoint 404。
3. 两个 Reader 各有独立 Endpoint；`pg_is_in_recovery=true`、只读事务、写入返回
   422；各自 1→0→1 并读取主节点后续 WAL 数据。
4. 活跃 Writer/Reader 随项目删除回收；七天 tombstone、正常入口关闭、UI 回收列表恢复。
5. 原 Writer/Reader ID、Selector 和数据库凭据恢复；全部先保持 0，再分别冷醒读取原数据。
6. 已单独删除的 parent/leaf 不复活；历史 Operation/tombstone 保留。
7. 真实 RLS 表与 Data API 查询成功；服务运行时再删除项目，公开 relay 返回 404。
8. 再恢复项目，Data API 仍 disabled、旧 Backend Token 401且 UI 显示已撤销；
   显式重新启用服务，原 RLS 内容可读，最终正常停用服务、所有 Compute 为 0。

多个 Reader 的**手动**休眠和冷醒通过，不等于多 Reader 自动 idle、长事务、
跨实例竞争或完整热资源伸缩认证。没有真实模型上游参与，本报告不认证 AI 推理。

## 4. 失败、恢复和基础设施证据

首次候选升级因迁移等待超时失败。数据库证据显示一个先前遗留的 `pg_dump`
闲置事务已持有 AccessShareLock 两小时以上，阻挡 migration 014 的表锁；三节点
当时 Ready，I/O PSI avg10=0。保存锁等待证据、核对新完整备份后，按 PID 和
backend_start 精确结束该只读导出，再执行 reviewed upgrade，迁移成功。
新备份 38,247,343 bytes，SHA256
`1e46377ef6f0664cad5decca87a8748a7ed23af217efbd8f14da2fb91390160d`，
完整 footer 已确认；`restoreTested:false`，不能宣称 DR 验证通过。

扩展 UI 的观察程序曾因三个 API Server 读取超时退出，原失败记录保留。
当时节点 .100 的 I/O PSI some avg10=63.72、full=58.04，仍有约 9.1 GiB
可用内存。没有并发镜像构建；BuildKit 已停止。只读进程采样包含 etcd、QEMU、
MinIO 和 Chromium，未采集宿主机完整存储链路，不能据此断言唯一硬件根因。
恢复后继续观察**原 Job 和原 Pod UID**，不重发创建/删除；浏览器最终成功。
两个 Endpoint 创建 Operation 的 attempts=2，均沿原资源身份完成，而非新建替代资源。

所有中断、采样、原失败观察与成功恢复记录按独立 attempt 保留；没有把原失败
report 改成成功。统一 Helm 工具增加独立远端导出时限、唯一 PGAPPNAME 和
闲置事务期限，其发布验收另记。

## 5. 手工复测与交付规则

按 [RETAINED-DELETION.md](RETAINED-DELETION.md)第 5 节和
[TESTING.md](TESTING.md)的 Linux 参数执行 `lifecycle.spec.ts`。
每次使用新的 attempt，保存 Operation/Endpoint IDs、镜像 digest 和版本。
失败先读原 Operation/日志，明确 retryable 后重试原操作，不能盲目重新 POST。
测试结束只退休已归属且完成的运行资源；数据库、对象、WAL、凭据与证据不清理。
