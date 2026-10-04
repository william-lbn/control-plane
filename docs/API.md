# 实际 API 合同

OpenAPI 3 合同的权威源是 [openapi-v1.json](../contracts/openapi-v1.json)。
版本 0.3.2，35 个路径、48 个操作，全部拥有稳定且唯一的 operationId。
运行 Swagger 位于 `/api/docs`，JSON 位于 `/api/openapi.json`。
本合同是自托管 `/api/v1`，不声称兼容 Neon SaaS `/api/v2`。

## 1. 请求边界

Session：POST /auth/login，浏览器保存 Session/CSRF Cookie。Cookie 写请求
携带 X-CSRF-Token；Bearer API Key 使用独立的 scope/role/expiry/revoke 校验。
公开健康检查和登录没有会话前提，业务接口进行服务端组织/项目授权。

资源创建需要 Idempotency-Key；创建返回 202 + resource + operation。
资源并未因为 202 自动就绪；查询 Operation 等待 succeeded，failed 时保存
request_id/operation_id/error_code，再由显式 retry 接口恢复可重试操作。
版本、参数、密码规则和每个错误响应均以 JSON 合同及实际 handler 为准。

## 2. 操作索引

| Method | Path | operationId | Success responses |
| --- | --- | --- | --- |
| GET | `/healthz` | `getHealth` | 200 |
| GET | `/readyz` | `getReadiness` | 200 |
| POST | `/auth/login` | `login` | 200 |
| POST | `/auth/logout` | `logout` | 200 |
| GET | `/api/v1/session` | `getSession` | 200 |
| GET | `/api/v1/capabilities` | `getCapabilities` | 200 |
| GET | `/api/v1/organizations/{org}/projects` | `listProjects` | 200 |
| POST | `/api/v1/organizations/{org}/projects` | `createProject` | 202 |
| GET | `/api/v1/projects/{project}` | `getProject` | 200 |
| GET | `/api/v1/projects/{project}/branches` | `listBranches` | 200 |
| POST | `/api/v1/projects/{project}/branches` | `createBranch` | 202 |
| GET | `/api/v1/projects/{project}/branches/{branch}` | `getBranch` | 200 |
| GET | `/api/v1/projects/{project}/branches/{branch}/services` | `listBranchServices` | 200 |
| GET | `/api/v1/projects/{project}/endpoints` | `listEndpoints` | 200 |
| POST | `/api/v1/projects/{project}/endpoints` | `createEndpoint` | 202 |
| GET | `/api/v1/projects/{project}/endpoints/{endpoint}` | `getEndpoint` | 200 |
| PATCH | `/api/v1/projects/{project}/endpoints/{endpoint}` | `updateEndpoint` | 202 |
| GET | `/api/v1/projects/{project}/endpoints/{endpoint}/connection-info` | `getEndpointConnectionInfo` | 200 |
| GET | `/api/v1/projects/{project}/endpoints/{endpoint}/metrics` | `getEndpointMetrics` | 200 |
| POST | `/api/v1/projects/{project}/endpoints/{endpoint}/query` | `executeEndpointQuery` | 200 |
| GET | `/api/v1/projects/{project}/operations` | `listOperations` | 200 |
| GET | `/api/v1/projects/{project}/operations/{operation}` | `getOperation` | 200 |
| POST | `/api/v1/projects/{project}/operations/{operation}/retry` | `retryOperation` | 202 |
| PATCH | `/api/v1/projects/{project}/endpoints/{endpoint}/lifecycle` | `configureEndpointLifecycle` | 200 |
| POST | `/api/v1/projects/{project}/endpoints/{endpoint}/suspend` | `suspendEndpoint` | 202 |
| GET | `/api/v1/organizations` | `listOrganizations` | 200 |
| POST | `/api/v1/organizations` | `createOrganization` | 201 |
| GET | `/api/v1/organizations/{org}` | `getOrganization` | 200 |
| GET | `/api/v1/organizations/{org}/members` | `listOrganizationMembers` | 200 |
| POST | `/api/v1/organizations/{org}/members` | `provisionLocalMember` | 201 |
| PATCH | `/api/v1/organizations/{org}/members/{member}` | `updateOrganizationMember` | 200 |
| DELETE | `/api/v1/organizations/{org}/members/{member}` | `removeOrganizationMember` | 200 |
| GET | `/api/v1/projects/{project}/permissions` | `listProjectPermissions` | 200 |
| PUT | `/api/v1/projects/{project}/permissions/{member}` | `setProjectPermission` | 200 |
| DELETE | `/api/v1/projects/{project}/permissions/{member}` | `revokeProjectPermission` | 200 |
| GET | `/api/v1/api-keys` | `listPersonalAPIKeys` | 200 |
| POST | `/api/v1/api-keys` | `createPersonalAPIKey` | 201 |
| DELETE | `/api/v1/api-keys/{key}` | `revokePersonalAPIKey` | 200 |
| GET | `/api/v1/organizations/{org}/api-keys` | `listOrganizationAPIKeys` | 200 |
| POST | `/api/v1/organizations/{org}/api-keys` | `createOrganizationAPIKey` | 201 |
| DELETE | `/api/v1/organizations/{org}/api-keys/{key}` | `revokeOrganizationAPIKey` | 200 |
| GET | `/api/v1/projects/{project}/branches/{branch}/roles` | `GetBranchRoles` | 200 |
| POST | `/api/v1/projects/{project}/branches/{branch}/roles` | `PostBranchRoles` | 202 |
| DELETE | `/api/v1/projects/{project}/branches/{branch}/roles/{role}` | `DeleteBranchRole` | 202 |
| PATCH | `/api/v1/projects/{project}/branches/{branch}/roles/{role}` | `PatchBranchRole` | 202 |
| GET | `/api/v1/projects/{project}/branches/{branch}/databases` | `GetBranchDatabases` | 200 |
| POST | `/api/v1/projects/{project}/branches/{branch}/databases` | `PostBranchDatabases` | 202 |
| DELETE | `/api/v1/projects/{project}/branches/{branch}/databases/{database}` | `DeleteBranchDatabase` | 202 |

## 3. 实现映射与演进

路由在 api/internal/control/handlers.go；OpenAPI Go AST 门槛双向比较注册
与文档操作，并检查引用、Session Cookie 和 operationId。
请求/响应模型在 components/schemas，权限与 bearer 语义在 securitySchemes。
组织/授权：organizations.go、authorization.go；资源创建：create_handlers.go；
目录：catalog.go；生命周期：lifecycle.go；SQL/监控：proxy.go、monitor.go。

新增操作应先实现权限、参数、持久意图、调谐和负例，再更新 OpenAPI 与 UI。
不能用返回成功的空 handler 宣布未实现服务可用。破坏性合同变更需要新版本与迁移计划。
