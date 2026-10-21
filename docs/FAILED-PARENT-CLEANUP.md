# 项目与分支创建失败后的保留清理

此切片已完成 Linux Go/真实 PostgreSQL、race/vet/OpenAPI、TypeScript/Vite、真实失败分支及原失败项目续验。API/Worker、Web 0.51.0、migration 041、Chart 0.9.43、Helm 115 已部署。精确来源、保留的工具故障与恢复链见[交付记录](DELIVERY-2026-10-18-FAILED-PARENT-CLEANUP.md)；上一版[独立失败 Compute 清理](DELIVERY-2026-10-18-FAILED-ENDPOINT-CLEANUP.md)保留独立回执。

通过既有生命周期 DELETE、名称确认、If-Match 和幂等键清理唯一原失败 `create_project` / `create_branch`。项目清理要求管理员；就绪项目下的失败叶分支要求编辑权限。原 Operation 必须已失败、完成、无租约，原 tenant/timeline/分支/可选 Writer 与实际元数据一致。创建默认自动保护且从未修改的 version-one 失败项目及原根分支被包含在明确确认中；另行修改的保护、子分支、其他 Compute、活跃操作或 Backend 依赖均阻止清理。

新持久 action 为 `cleanup_failed_project` / `cleanup_failed_branch`，旧 Worker 明确拒绝。父锁与 migration 041 的准入触发器序列化原创建重试和清理。清理开始后原创建不能再入队。五步为关闭已有入口或证明原入口未发布、绑定并退休原 VM、观察正常 Runner GC、退休原 Service、提交 held tombstone。沿用 migration 040 的不可变 VM/Service witness，未知删除结果后不收编同名替代资源。

保留 tenant、timeline、WAL、对象、凭据/config Secret、原失败 Operation/日志。创建可能在外部返回未知结果，不能把“保留这些身份”写成“已经完成数据库恢复”；失败创建清理不提供就绪项目的七天恢复窗口。没有物理删除或强制 Runner 删除。

真实 PostgreSQL 准入/竞态/负例、HTTP 未知结果与替代身份保护，以及 Linux UI 对真实项目和分支创建失败的清理、重放、原失败与原生 timeline/祖先保留均已通过。无 Compute 失败分支通过真实 PG/HTTP 验证，现场 native 故障两例均带原 Writer；不把后者写成无 Compute 现场故障演练。分支 TTL、物理 GC、外部 fencing 和 HA/DR 仍单列。
