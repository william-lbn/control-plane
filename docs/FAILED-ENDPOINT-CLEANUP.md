# 独立 Compute 创建失败后的保留退休

## 合同

就绪的受管理项目/分支下，独立 `create_endpoint` 失败的 Compute 可由原删除界面正常退休。使用原 Selector、If-Match、幂等键和当前编辑权限；服务端绑定唯一、完成且无租约的原失败创建 Operation，并验证完整项目/分支/Endpoint 归属。没有这项证明时不允许把任意 `error` 资源当作失败创建清理。

删除使用独立 `cleanup_failed_endpoint` 持久 Operation，旧 Worker 不识别时明确拒绝。先关闭已有原 Proxy route；尚未发布 route 时只接受该原失败创建绑定下的缺席，不放宽正常 Compute 删除的归属检查。任何外部删除前，先将自有 VM/Service UID 或确定缺席保存在不可变表 `failed_endpoint_resource_bindings`；未知响应后的重试不能收编同名替代资源。按原 UID/resourceVersion 退休 VM，观察 Runner 实际消失，再退休已绑定的原 Service，最后提交 endpoint tombstone，释放运行配额。保留分支、timeline、数据库、角色、凭据及配置 Secret、原失败 Operation 和日志。

五个持久步骤是关闭入口、退休原 VM、观察 Runner 消失、退休原 Service、提交 tombstone。正常 active Compute 的既有删除合同保留自己的四步流程，本切片不追溯修改已完成的历史删除。

原创建重试与删除准入在相同父资源锁下序列化。进入删除后，即使旧创建 Operation 原本 retryable，也不能重新入队。运行中的创建、已成功创建、错误归属、未知 route/VM/Runner、活跃 Backend 依赖均拒绝或保持失败待检查；不强制删除陌生或替代资源。

## 实施与验证状态

本切片已通过 Linux Go/真实 PostgreSQL、race/vet/OpenAPI、HTTP 故障、TypeScript/Vite 和七项实际 UI 检查；API/Worker、Web 0.50.0、migration 040、Chart 0.9.42、Helm 114 已部署。精确来源、原失败工具恢复和边界见[交付记录](DELIVERY-2026-10-18-FAILED-ENDPOINT-CLEANUP.md)。覆盖授权、原失败身份、版本/幂等、旧重试拒绝及事务负例，route 缺席/归属、VM/Runner UID 保护和未知删除结果下的替代 Service 拒绝。

现场故障由受信任 Linux 工具 `tools/failed-endpoint-create-fault.mjs` 执行。浏览器没有 Kubernetes 凭据。工具只在队列、VM 和计划均空闲时暂停原 UID Worker，等待 UI 受理一个新 Reader；随后使用 [Kubernetes ValidatingAdmissionPolicy](https://kubernetes.io/docs/reference/access-authn-authz/validating-admission-policy/) 只拒绝该 namespace/name/project/endpoint 的 VM CREATE。三台 API 的原模板 dry-run 先成功，再均返回本次策略拒绝，才恢复 Worker。必须观察原 Operation 的结构化 HTTP 403、已创建原 Service 和 VM 缺席，随后按 UID/RV 退休临时策略，并证明三端点 dry-run 再次成功。没有伪造元数据状态、替换镜像或停用全局存储。

UI 丢弃一次真实清理 202，再以原 key/version 重取同一 Operation；完成后原创建重试必须被拒绝，原失败回执保留。受信任工具独立检查 VM 缺席/原 Service UID 证据、Service 消失及原 credential/config Secret UID/内容摘要保留。失败时保存原 Endpoint、两个 Operation 和私有 checkpoint；续测使用经审阅的原 Operator Service/Secret 摘要，不创建替代成功。工具的 finally/中止标记恢复原 Worker 并清理本次策略。

上一版 [0.49.0 异常 Functions idle 回收](DELIVERY-2026-10-18-FUNCTION-UNHEALTHY-IDLE.md)保留独立回执。本切片不关闭失败项目/分支创建清理、分支 TTL、物理 GC 或跨实例外部 fencing；后续继续逐项完成。
