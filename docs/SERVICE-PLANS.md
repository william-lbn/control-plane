# 自托管服务计划与组织资源权益

计划包含不可变的项目数和 Endpoint 数上限。它管理本地资源权益；没有支付服务商、收款、订阅续费、税费或 AI 额度发放行为。外部账单与支付仍须单独实现和验收。

普通组织 Admin 可以在组织页面查看当前权益、实际使用量与分配历史。只有独立平台审批策略列出的 Console 用户可以创建计划、发布修订、启用或禁用计划，以及给许可范围内的组织分配权益。组织 Admin、API Key 和 AI 供应商运营权限本身都不授予平台计划管理权。

## 配置与发布

默认不启用平台管理。Helm 配置示例中的用户和组织 ID 必须由部署方审批并替换为真实 ID：

```yaml
platformManagement:
  enabled: true
  operatorIds: [approved_operator]
  allowedOrganizations: [approved_organization]
```

启用时必须提供 1–16 个不同的有效用户 ID；组织名单最多 128 个，不填组织名单表示允许已批准运营人员管理所有组织。Chart 为策略生成内容寻址的不可变 ConfigMap，只挂载到 API，使用 `NEON_PLATFORM_POLICY_FILE` 读取。改变名单产生新的 ConfigMap 和 API 滚动更新；旧策略由 Helm 保留，便于审计。策略文件采用 version 1 严格 JSON；禁止未知字段、重复 ID 和超过 64 KiB 的文件。

Migration 067 只创建计划、修订、绑定、幂等回执和不可变历史，不迁移任何组织的原配额。未分配计划的组织沿用原有规则。首次分配会保存原配额，以后显式清除分配时恢复该值。

## 修改规则

每次写入需要当前资源版本和 8–128 字符的 `Idempotency-Key`。服务端保存不可变回执；相同用户、键和请求返回原结果，改变请求则冲突。重放时仍检查当前平台审批权限。页面遇到连接中断或 5xx 时保留原请求，提供“读取原计划操作回执”，避免重新生成键。刷新或关闭页面后，调用方应保留原键和请求体，通过 API 核对原回执。

发布新修订不改变已有组织绑定。组织只有在明确指定一个修订 ID 后才切换配额。禁用计划会将其从公开目录移除并拒绝新分配，已分配的修订及资源保留。重新启用后可以继续发布；历史修订不会被修改或删除。

分配使用与项目、Endpoint 创建相同的组织配额行锁，统计未删除的项目和 Endpoint，包括已经占用名额但尚未完成创建的资源。低于当前使用量的分配返回 `409 service_plan_below_current_usage`，不产生部分配额或历史。版本冲突必须读取当前状态后重新决定，不能自动覆盖。数据库提交约束同时检查绑定与实际配额一致；修订、回执和历史禁止修改或删除。

清除分配必须提交 `revision_id: null`；省略字段会被拒绝。它恢复首次分配前的配额，同样不能低于当前使用量，不会删除项目、Endpoint 或持久数据。资源创建继续走现有 `organization_quota_exceeded` 拦截路径。

## API

- `GET /api/v1/service-plans`：当前启用计划目录；单个计划详情为 `/{servicePlan}`。
- `GET /api/v1/organizations/{org}/service-plan`：组织 Admin 读取权益、使用量和最近 100 条历史。
- `GET/POST /api/v1/platform/service-plans`：批准的平台运营人员读取目录或创建计划。
- `GET /api/v1/platform/service-plans/{servicePlan}`、`POST .../revisions`、`PUT .../state`：详情、发布不可变修订、启停。
- `GET/PUT /api/v1/platform/organizations/{planOrg}/service-plan`：在审批组织范围内读取或显式分配/清除。

完整请求合同见 [API 索引](API.md) 和 [OpenAPI](../contracts/openapi-v1.json)。平台接口接受 Console Session 和 CSRF，不接受 API Key 写入。受限 AI Gateway 数据库角色没有计划写权限。

## 功能验证范围

维护的 `service_plans_test.go` 检查独立审批、组织权限、不可变修订/历史、原回执、固定旧权益、禁用、显式恢复、降级原子性、现有配额准入和并发版本冲突。`platform-plans.test.mjs` 检查默认关闭、API 独占策略投影及审批配置拒绝。

`service-plans.spec.ts` 使用新建的专用组织，验证真实页面和实际资源创建，记录原请求与 Operation 后串行观察，失败保留检查点。通过项及实际镜像、迁移和集群状态以对应交付报告为准。性能、可靠性、HA、灾备和真实收款不属于这批功能验收。
