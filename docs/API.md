# 实际 API 合同

OpenAPI 3 合同的权威源是 [openapi-v1.json](../contracts/openapi-v1.json)。
版本 0.23.0，76 个路径、112 个操作，全部拥有稳定且唯一的 operationId。
运行 Swagger 位于 `/api/docs`，JSON 位于 `/api/openapi.json`。
本合同是自托管 `/api/v1`，不声称兼容 Neon SaaS `/api/v2`。

## 1. 请求边界

Session：POST /auth/login，浏览器保存 Session/CSRF Cookie。Cookie 写请求
携带 X-CSRF-Token；Bearer API Key 使用独立的 scope/role/expiry/revoke 校验。
公开健康检查和登录没有会话前提，业务接口进行服务端组织/项目授权。
受邀注册由有效的账号绑定邀请准入；Console 邀请管理要求管理员 Cookie Session，
已有账号接受要求受邀者本人 Session。详细合同见 CONSOLE-INVITATIONS.md。

资源创建需要 Idempotency-Key；创建返回 202 + resource + operation。
资源并未因为 202 自动就绪；查询 Operation 等待 succeeded，failed 时保存
request_id/operation_id/error_code，再由显式 retry 接口恢复可重试操作。
版本、参数、密码规则和每个错误响应均以 JSON 合同及实际 handler 为准。

## 2. 操作索引

以下索引由 `tools/generate-api-docs.mjs` 从实际 OpenAPI 生成，Linux CI 检查同步。

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
| DELETE | `/api/v1/projects/{project}` | `DeleteProject` | 202 |
| GET | `/api/v1/projects/{project}/branches` | `listBranches` | 200 |
| POST | `/api/v1/projects/{project}/branches` | `createBranch` | 202 |
| GET | `/api/v1/projects/{project}/branches/{branch}` | `getBranch` | 200 |
| DELETE | `/api/v1/projects/{project}/branches/{branch}` | `DeleteBranch` | 202 |
| GET | `/api/v1/projects/{project}/branches/{branch}/services` | `listBranchServices` | 200 |
| GET | `/api/v1/projects/{project}/endpoints` | `listEndpoints` | 200 |
| POST | `/api/v1/projects/{project}/endpoints` | `createEndpoint` | 202 |
| GET | `/api/v1/projects/{project}/endpoints/{endpoint}` | `getEndpoint` | 200 |
| PATCH | `/api/v1/projects/{project}/endpoints/{endpoint}` | `updateEndpoint` | 202 |
| DELETE | `/api/v1/projects/{project}/endpoints/{endpoint}` | `DeleteEndpoint` | 202 |
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
| PATCH | `/api/v1/projects/{project}/branches/{branch}/roles/{role}` | `PatchBranchRole` | 202 |
| DELETE | `/api/v1/projects/{project}/branches/{branch}/roles/{role}` | `DeleteBranchRole` | 202 |
| GET | `/api/v1/projects/{project}/branches/{branch}/databases` | `GetBranchDatabases` | 200 |
| POST | `/api/v1/projects/{project}/branches/{branch}/databases` | `PostBranchDatabases` | 202 |
| PATCH | `/api/v1/projects/{project}/branches/{branch}/databases/{database}` | `updateDatabaseOwner` | 202 |
| DELETE | `/api/v1/projects/{project}/branches/{branch}/databases/{database}` | `DeleteBranchDatabase` | 202 |
| GET | `/api/v1/projects/{project}/branches/{branch}/data-api` | `getDataAPI` | 200 |
| POST | `/api/v1/projects/{project}/branches/{branch}/data-api` | `enableDataAPI` | 202 |
| DELETE | `/api/v1/projects/{project}/branches/{branch}/data-api` | `disableDataAPI` | 202 |
| GET | `/api/v1/projects/{project}/branches/{branch}/credentials` | `listBackendCredentials` | 200 |
| POST | `/api/v1/projects/{project}/branches/{branch}/credentials` | `createBackendCredential` | 200, 201 |
| POST | `/api/v1/projects/{project}/branches/{branch}/credentials/check` | `checkBackendCredential` | 200 |
| POST | `/api/v1/projects/{project}/branches/{branch}/credentials/{credential}/rotate` | `rotateBackendCredential` | 200 |
| DELETE | `/api/v1/projects/{project}/branches/{branch}/credentials/{credential}` | `revokeBackendCredential` | 200 |
| POST | `/api/v1/projects/{project}/branches/{branch}/data-api/request` | `testDataAPIConsoleRequest` | 200 |
| GET | `/api/v1/organizations/{org}/invitations` | `listConsoleInvitations` | 200 |
| POST | `/api/v1/organizations/{org}/invitations` | `createConsoleInvitation` | 200, 201 |
| DELETE | `/api/v1/organizations/{org}/invitations/{invitation}` | `revokeConsoleInvitation` | 200 |
| POST | `/api/v1/invitations/accept` | `acceptConsoleInvitation` | 201 |
| POST | `/auth/signup` | `registerInvitedConsoleAccount` | 201 |
| GET | `/api/v1/projects/{project}/branches/{branch}/restore-window` | `readBranchRestoreWindow` | 200 |
| GET | `/api/v1/projects/{project}/lifecycle` | `GetProjectLifecycle` | 200 |
| PATCH | `/api/v1/projects/{project}/protection` | `SetProjectProtection` | 200 |
| PATCH | `/api/v1/projects/{project}/branches/{branch}/protection` | `SetBranchProtection` | 200 |
| POST | `/api/v1/projects/{project}/recover` | `RecoverProject` | 202 |
| GET | `/api/v1/projects/{project}/branches/{branch}/auth` | `getManagedAuth` | 200 |
| POST | `/api/v1/projects/{project}/branches/{branch}/auth` | `enableManagedAuth` | 202 |
| DELETE | `/api/v1/projects/{project}/branches/{branch}/auth` | `disableManagedAuth` | 202 |
| POST | `/api/v1/projects/{project}/branches/{branch}/auth/users` | `inspectManagedAuthUsers` | 200 |
| GET | `/api/v1/projects/{project}/branches/{branch}/storage` | `getObjectStorage` | 200 |
| POST | `/api/v1/projects/{project}/branches/{branch}/storage` | `enableObjectStorage` | 202 |
| DELETE | `/api/v1/projects/{project}/branches/{branch}/storage` | `disableObjectStorage` | 202 |
| GET | `/api/v1/projects/{project}/branches/{branch}/storage/buckets` | `listStorageBuckets` | 200 |
| POST | `/api/v1/projects/{project}/branches/{branch}/storage/buckets` | `createStorageBucket` | 201 |
| DELETE | `/api/v1/projects/{project}/branches/{branch}/storage/buckets/{bucket}` | `deleteStorageBucket` | 200 |
| GET | `/api/v1/projects/{project}/branches/{branch}/storage/buckets/{bucket}/objects` | `getStorageObject` | 200, 206 |
| HEAD | `/api/v1/projects/{project}/branches/{branch}/storage/buckets/{bucket}/objects` | `headStorageObject` | 200, 206 |
| PUT | `/api/v1/projects/{project}/branches/{branch}/storage/buckets/{bucket}/objects` | `putStorageObject` | 200 |
| DELETE | `/api/v1/projects/{project}/branches/{branch}/storage/buckets/{bucket}/objects` | `deleteStorageObject` | 200 |
| POST | `/api/v1/projects/{project}/branches/{branch}/storage/buckets/{bucket}/presign` | `presignStorageDownload` | 200 |
| GET | `/storage/v1/{storageBranch}/{bucket}` | `getPublicStorageObject` | 200 |
| HEAD | `/storage/v1/{storageBranch}/{bucket}` | `headPublicStorageObject` | 200 |
| GET | `/api/v1/projects/{project}/branches/{branch}/functions` | `listFunctions` | 200 |
| POST | `/api/v1/projects/{project}/branches/{branch}/functions` | `createFunction` | 202 |
| GET | `/api/v1/projects/{project}/branches/{branch}/functions/{slug}` | `getFunction` | 200 |
| DELETE | `/api/v1/projects/{project}/branches/{branch}/functions/{slug}` | `deleteFunction` | 202 |
| GET | `/api/v1/projects/{project}/branches/{branch}/functions/{slug}/deployments` | `listFunctionDeployments` | 200 |
| POST | `/api/v1/projects/{project}/branches/{branch}/functions/{slug}/deployments` | `deployFunction` | 202 |
| POST | `/api/v1/projects/{project}/branches/{branch}/functions/{slug}/suspend` | `suspendFunction` | 202 |
| POST | `/api/v1/projects/{project}/branches/{branch}/functions/{slug}/console-invoke` | `invokeFunctionConsole` | 200 |
| GET | `/api/v1/projects/{project}/branches/{branch}/functions/{slug}/runtime` | `observeFunctionRuntime` | 200 |
| POST | `/api/v1/projects/{project}/branches/{branch}/functions/{slug}/resume` | `resumeFunction` | 202 |
| POST | `/api/v1/projects/{project}/endpoints/{endpoint}/query-script` | `executeSQLScript` | 200 |
| POST | `/api/v1/projects/{project}/branches/{branch}/functions/{slug}/rollback` | `rollbackFunction` | 202 |
| GET | `/api/v1/projects/{project}/branches/{branch}/ai-gateway` | `getAIGateway` | 200 |
| GET | `/api/v1/projects/{project}/branches/{branch}/ai-gateway/usage` | `getAIGatewayUsage` | 200 |
| POST | `/api/v1/projects/{project}/branches/{branch}/ai-gateway/request` | `invokeAIPlayground` | 200 |
| GET | `/ai/v1/{aiBranch}/v1/models` | `listAIModels` | 200 |
| POST | `/ai/v1/{aiBranch}/v1/chat/completions` | `createAIChatCompletion` | 200 |
| GET | `/api/v1/projects/{project}/saved-queries` | `listSavedQueries` | 200 |
| POST | `/api/v1/projects/{project}/saved-queries` | `createSavedQuery` | 200, 201 |
| GET | `/api/v1/projects/{project}/saved-queries/{query}` | `getSavedQuery` | 200 |
| PATCH | `/api/v1/projects/{project}/saved-queries/{query}` | `updateSavedQuery` | 200 |
| DELETE | `/api/v1/projects/{project}/saved-queries/{query}` | `deleteSavedQuery` | 200 |
| PATCH | `/api/v1/projects/{project}/branches/{branch}/functions/{slug}/http-policy` | `setFunctionHTTPPolicy` | 200 |
| GET | `/api/application-auth-openapi.json` | `getApplicationAuthOpenAPI` | 200 |

## 3. 实现映射与演进

路由在 api/internal/control/handlers.go；OpenAPI Go AST 门槛双向比较注册
与文档操作，并检查引用、Session Cookie 和 operationId。
请求/响应模型在 components/schemas，权限与 bearer 语义在 securitySchemes。
组织/授权：organizations.go、authorization.go；资源创建：create_handlers.go；
目录：catalog.go；Compute 生命周期：lifecycle.go；保留删除/恢复：deletion.go；
SQL/监控：proxy.go、monitor.go；历史恢复：restore.go。

`createBranch` 可提供 `parent_timestamp` 或 `parent_lsn`，二者互斥。它创建新分支，
不修改原分支；解析出的固定 `parent_lsn`、`restore_source`、`parent_timestamp` 与
Operation 一并持久化。时间点、保留窗口、租约、错误与历史目录规则见
[恢复合同](HISTORICAL-BRANCH-RESTORE.md)。新增读接口不会唤醒 Compute。

新增操作应先实现权限、参数、持久意图、调谐和负例，再更新 OpenAPI 与 UI。
不能用返回成功的空 handler 宣布未实现服务可用。破坏性合同变更需要新版本与迁移计划。

## 5. 分支 Object Storage REST v1

参见 [REST 存储合同](OBJECT-STORAGE.md)与 [S3 基础协议合同](OBJECT-STORAGE-S3-CORE.md)。元数据状态读取不唤醒 Compute；目录/文件读取会按需唤醒。REST 公开下载使用短期签名或明确的 public_read ACL。服务配置需要管理员、If-Match 与 Idempotency-Key；REST 文件修改需要内容 ETag 条件。独立 S3 入口只在 operator 显式启用后开放已列明的基础能力；不声称完整 S3 兼容。

| Method | Path | operationId |
| --- | --- | --- |
| GET | `/api/v1/projects/{project}/branches/{branch}/storage` | `getObjectStorage` |
| POST | `/api/v1/projects/{project}/branches/{branch}/storage` | `enableObjectStorage` |
| DELETE | `/api/v1/projects/{project}/branches/{branch}/storage` | `disableObjectStorage` |
| GET | `/api/v1/projects/{project}/branches/{branch}/storage/buckets` | `listStorageBuckets` |
| POST | `/api/v1/projects/{project}/branches/{branch}/storage/buckets` | `createStorageBucket` |
| DELETE | `/api/v1/projects/{project}/branches/{branch}/storage/buckets/{bucket}` | `deleteStorageBucket` |
| GET | `/api/v1/projects/{project}/branches/{branch}/storage/buckets/{bucket}/objects` | `getStorageObject` |
| HEAD | `/api/v1/projects/{project}/branches/{branch}/storage/buckets/{bucket}/objects` | `headStorageObject` |
| PUT | `/api/v1/projects/{project}/branches/{branch}/storage/buckets/{bucket}/objects` | `putStorageObject` |
| DELETE | `/api/v1/projects/{project}/branches/{branch}/storage/buckets/{bucket}/objects` | `deleteStorageObject` |
| POST | `/api/v1/projects/{project}/branches/{branch}/storage/buckets/{bucket}/presign` | `presignStorageDownload` |
| GET | `/storage/v1/{storageBranch}/{bucket}` | `getPublicStorageObject` |
| HEAD | `/storage/v1/{storageBranch}/{bucket}` | `headPublicStorageObject` |
