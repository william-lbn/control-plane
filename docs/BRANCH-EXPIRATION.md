# 分支到期自动退休

已在 0.52.0 / migration 042 完成基础功能与原生 UI 验收，见[精确交付](DELIVERY-2026-10-18-BRANCH-EXPIRATION.md)。本轮先完成功能，不执行性能、HA/DR、节点分区或跨实例栅栏专项验证。

依据本地官网文档快照 `content/docs/guides/branch-expiration.md` 的 RFC3339、未来 30 天、非默认/非保护/无子分支限制，实现显式设置、更新、取消和自动退休。保留本项目既有的数据保全合同：到期关闭入口、退休原 Compute/Service、提交 held tombstone，保留 timeline、WAL、对象、Secrets 和审计；不执行官网所描述的物理永久删除。到期由后台控制器处理，不声称实时外部连接栅栏。

设置使用编辑权限、名称确认、当前 If-Match 和持久幂等键；成功策略与完成 Operation 同一事务提交。只允许就绪受管理叶分支和已就绪原生 Compute。Data API、Functions 及未知服务需要先停用/退休；Auth 和 Object Storage 沿既有父资源保留删除 Driver 停用，业务字节保留。设置到期后不能添加子分支、开启保护或新增 Compute/Data API/Functions；可以取消未来策略后再进行这些操作。到期后策略不能撤回，仍可读取状态及原操作。

控制器按数据库时间发现到期策略，重新检查依赖，记录明确的等待原因；原图及 VM/Service UID 或缺席进入持久 `expire_branch` Operation。关闭入口后按原 UID/version 回收，观察正常 Runner 消失及 Service 退休，再提交 tombstone。未知结果保留同一 Operation，用户显式重试，不重新生成资源身份。被活跃操作阻塞的分支不得饿死其他到期候选。

基础功能验收已覆盖设置/更新/取消、时间/权限/保护/父子限制、持久重放、自动退休、原数据和父分支保留，以及真实 Linux UI。性能压测、HA/DR、分区与外部 fencing 不在本轮执行。
