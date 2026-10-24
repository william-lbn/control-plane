# 实际 API 合同

OpenAPI 3 合同的权威源是 [openapi-v1.json](../contracts/openapi-v1.json)。
版本 0.84.0，169 个路径、234 个操作，全部拥有稳定且唯一的 operationId。
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
| PUT | `/storage/v1/{storageBranch}/{bucket}` | `uploadWithStorageCapability` | 200 |
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
| POST | `/api/v1/projects/{project}/branches/{branch}/storage/buckets/{bucket}/copy` | `copyStorageObject` | 201 |
| POST | `/api/v1/projects/{project}/branches/{branch}/storage/buckets/{bucket}/delete-batch` | `deleteStorageObjectBatch` | 200 |
| POST | `/api/v1/projects/{project}/branches/{branch}/storage/buckets/{bucket}/presign-upload` | `presignStorageUpload` | 200 |
| GET | `/api/v1/projects/{project}/branches/{branch}/storage/buckets/{bucket}/cors` | `getStorageBucketCORS` | 200 |
| PUT | `/api/v1/projects/{project}/branches/{branch}/storage/buckets/{bucket}/cors` | `putStorageBucketCORS` | 200 |
| DELETE | `/api/v1/projects/{project}/branches/{branch}/storage/buckets/{bucket}/cors` | `deleteStorageBucketCORS` | 200 |
| GET | `/api/v1/projects/{project}/branches/{branch}/functions/{slug}/request-logs` | `listFunctionRequestLogs` | 200 |
| PATCH | `/api/v1/projects/{project}/branches/{branch}/storage/buckets/{bucket}/objects/metadata` | `updateStorageObjectMetadata` | 200 |
| POST | `/api/v1/projects/{project}/saved-queries/{query}/sharing` | `setSavedQuerySharing` | 200 |
| GET | `/api/v1/projects/{project}/query-history` | `listPersonalSQLHistory` | 200 |
| GET | `/api/v1/projects/{project}/query-history/{history}` | `getPersonalSQLHistory` | 200 |
| DELETE | `/api/v1/projects/{project}/query-history/{history}` | `deletePersonalSQLHistory` | 204 |
| GET | `/api/v1/account/sessions` | `listOwnConsoleSessions` | 200 |
| POST | `/api/v1/account/sessions/{session}/revoke` | `revokeOwnConsoleSession` | 200 |
| POST | `/api/v1/account/sessions/revoke-others` | `revokeOtherConsoleSessions` | 200 |
| GET | `/api/v1/account/mfa` | `getOwnConsoleMFA` | 200 |
| POST | `/api/v1/account/mfa/enroll` | `enrollOwnConsoleMFA` | 200 |
| POST | `/api/v1/account/mfa/confirm` | `confirmOwnConsoleMFA` | 200 |
| POST | `/api/v1/account/mfa/disable` | `disableOwnConsoleMFA` | 200 |
| GET | `/api/v1/account/query-history-policy` | `getOwnSQLHistoryPolicy` | 200 |
| PATCH | `/api/v1/account/query-history-policy` | `updateOwnSQLHistoryPolicy` | 200 |
| POST | `/api/v1/account/query-history-cleanup` | `cleanupOwnExpiredSQLHistory` | 200 |
| GET | `/api/v1/projects/{project}/query-history/{history}/sharing` | `getPersonalSQLHistorySharing` | 200 |
| PUT | `/api/v1/projects/{project}/query-history/{history}/sharing` | `putPersonalSQLHistorySharing` | 200 |
| GET | `/api/v1/projects/{project}/shared-query-history` | `listProjectSharedSQLHistory` | 200 |
| GET | `/api/v1/projects/{project}/shared-query-history/{history}` | `getProjectSharedSQLHistory` | 200 |
| GET | `/api/v1/projects/{project}/branches/{branch}/storage/buckets/{bucket}/multipart-uploads` | `listMultipartUploads` | 200 |
| POST | `/api/v1/projects/{project}/branches/{branch}/storage/buckets/{bucket}/multipart-uploads` | `createMultipartUpload` | 201 |
| GET | `/api/v1/projects/{project}/branches/{branch}/storage/buckets/{bucket}/multipart-uploads/{upload}` | `readMultipartUpload` | 200 |
| DELETE | `/api/v1/projects/{project}/branches/{branch}/storage/buckets/{bucket}/multipart-uploads/{upload}` | `abortMultipartUpload` | 204 |
| PUT | `/api/v1/projects/{project}/branches/{branch}/storage/buckets/{bucket}/multipart-uploads/{upload}/parts/{part}` | `uploadMultipartPart` | 200 |
| POST | `/api/v1/projects/{project}/branches/{branch}/storage/buckets/{bucket}/multipart-uploads/{upload}/complete` | `completeMultipartUpload` | 200 |
| GET | `/api/v1/projects/{project}/branches/{branch}/storage/buckets/{bucket}/multipart-administration` | `listMultipartAdministration` | 200 |
| POST | `/api/v1/projects/{project}/branches/{branch}/storage/buckets/{bucket}/multipart-administration/{upload}/abort` | `abortMultipartAsAdministrator` | 200 |
| GET | `/api/v1/projects/{project}/query-history/{history}/name` | `getPersonalSQLHistoryName` | 200 |
| PUT | `/api/v1/projects/{project}/query-history/{history}/name` | `putPersonalSQLHistoryName` | 200 |
| GET | `/api/v1/projects/{project}/branches/{branch}/functions-triggers` | `listFunctionTriggers` | 200 |
| POST | `/api/v1/projects/{project}/branches/{branch}/functions-triggers` | `createFunctionTrigger` | 201 |
| GET | `/api/v1/projects/{project}/branches/{branch}/functions-triggers/{trigger}` | `getFunctionTrigger` | 200 |
| PATCH | `/api/v1/projects/{project}/branches/{branch}/functions-triggers/{trigger}` | `updateFunctionTrigger` | 200 |
| DELETE | `/api/v1/projects/{project}/branches/{branch}/functions-triggers/{trigger}` | `deleteFunctionTrigger` | 204 |
| GET | `/api/v1/projects/{project}/branches/{branch}/functions-triggers/{trigger}/invocations` | `listFunctionTriggerInvocations` | 200 |
| POST | `/api/v1/projects/{project}/branches/{branch}/roles/{role}/rename` | `renameBranchRole` | 202 |
| POST | `/api/v1/projects/{project}/branches/{branch}/databases/{database}/rename` | `renameBranchDatabase` | 202 |
| PATCH | `/api/v1/projects/{project}/branches/{branch}/expiration` | `setBranchExpiration` | 202 |
| POST | `/api/v1/projects/{project}/branches/{branch}/functions-triggers/{trigger}/invocations/{invocation}/retry` | `retryFunctionObjectInvocation` | 202 |
| GET | `/api/v1/projects/{project}/branches/{branch}/functions/{slug}/output-logs` | `listFunctionCustomerOutput` | 200 |
| GET | `/api/v1/projects/{project}/branches/{branch}/functions/{slug}/output-policy` | `getFunctionOutputPolicy` | 200 |
| PATCH | `/api/v1/projects/{project}/branches/{branch}/functions/{slug}/output-policy` | `setFunctionOutputPolicy` | 200 |
| POST | `/api/v1/projects/{project}/branches/{branch}/functions-copy` | `copyFunctionBranch` | 202 |
| GET | `/api/v1/projects/{project}/branches/{branch}/function-copy-plans` | `listFunctionBranchCopyPlans` | 200 |
| GET | `/api/v1/platform/ai-providers/capabilities` | `getAIProviderCapabilities` | 200 |
| GET | `/api/v1/platform/ai-providers` | `listAIProviders` | 200 |
| POST | `/api/v1/platform/ai-providers` | `createAIProvider` | 202 |
| GET | `/api/v1/platform/ai-providers/{provider}` | `getAIProvider` | 200 |
| DELETE | `/api/v1/platform/ai-providers/{provider}` | `deleteAIProvider` | 200 |
| POST | `/api/v1/platform/ai-providers/{provider}/revisions` | `testAIProviderRevision` | 202 |
| POST | `/api/v1/platform/ai-providers/{provider}/publish` | `publishAIProvider` | 200 |
| POST | `/api/v1/platform/ai-providers/{provider}/disable` | `disableAIProvider` | 200 |
| GET | `/api/v1/platform/ai-provider-operations/{providerOperation}` | `getAIProviderOperation` | 200 |
| POST | `/api/v1/platform/ai-provider-operations/{providerOperation}/retry` | `retryAIProviderOperation` | 202 |
| GET | `/api/v1/projects/{project}/branches/{branch}/connection-defaults` | `getBranchConnectionDefaults` | 200 |
| PATCH | `/api/v1/projects/{project}/branches/{branch}/connection-defaults` | `updateBranchConnectionDefaults` | 200 |
| GET | `/api/v1/projects/{project}/branches/{branch}/catalog-identities` | `getCatalogIdentityInventory` | 200 |
| POST | `/api/v1/projects/{project}/branches/{branch}/catalog-identities` | `initializeCatalogIdentity` | 200 |
| POST | `/api/v1/projects/{project}/branches/{branch}/catalog-identities/{binding}/restore` | `restoreOriginalCatalogIdentity` | 202 |
| GET | `/api/v1/projects/{project}/branches/{branch}/default-database-owner` | `getDefaultDatabaseOwner` | 200 |
| PATCH | `/api/v1/projects/{project}/branches/{branch}/default-database-owner` | `updateDefaultDatabaseOwner` | 202 |
| POST | `/api/v1/projects/{project}/branches/{branch}/catalog-identities/{binding}/observe-current` | `observeCurrentCatalogIdentity` | 200 |
| POST | `/api/v1/projects/{project}/branches/{branch}/ai-gateway/request/{protocol}` | `invokeAINativeProtocol` | 200 |
| POST | `/ai/v1/{aiBranch}/v1/responses` | `createAINativeResponses` | 200 |
| POST | `/ai/v1/{aiBranch}/openai/v1/responses` | `createAINativeResponsesAlias` | 200 |
| POST | `/ai/v1/{aiBranch}/v1/embeddings` | `createAINativeEmbeddings` | 200 |
| POST | `/ai/v1/{aiBranch}/openai/v1/embeddings` | `createAINativeEmbeddingsAlias` | 200 |
| POST | `/ai/v1/{aiBranch}/v1/messages` | `createAINativeMessages` | 200 |
| POST | `/ai/v1/{aiBranch}/anthropic/v1/messages` | `createAINativeMessagesAlias` | 200 |
| POST | `/ai/v1/{aiBranch}/gemini/v1beta/models/{aiModelAction}` | `createAINativeGemini` | 200 |
| GET | `/api/v1/projects/{project}/branches/{branch}/ai-gateway/budget` | `getAIBudget` | 200 |
| PUT | `/api/v1/projects/{project}/branches/{branch}/ai-gateway/budget` | `updateAIBudget` | 200 |
| POST | `/api/v1/projects/{project}/branches/{branch}/ai-gateway/requests/{aiRequest}/reconcile` | `reconcileAIBudget` | 200 |
| GET | `/api/v1/organizations/{org}/ai-credits` | `getOrganizationCredits` | 200 |
| GET | `/api/v1/organizations/{org}/ai-credits/ledger` | `listOrganizationCreditLedger` | 200 |
| GET | `/api/v1/organizations/{org}/ai-credits/requests` | `listOrganizationPendingCredits` | 200 |
| GET | `/api/v1/platform/ai-credits/organizations/{creditOrg}` | `getPlatformOrganizationCredits` | 200 |
| PUT | `/api/v1/platform/ai-credits/organizations/{creditOrg}` | `updateOrganizationCreditPolicy` | 200 |
| GET | `/api/v1/platform/ai-credits/organizations/{creditOrg}/ledger` | `listPlatformCreditLedger` | 200 |
| GET | `/api/v1/platform/ai-credits/organizations/{creditOrg}/requests` | `listPlatformPendingCredits` | 200 |
| POST | `/api/v1/platform/ai-credits/organizations/{creditOrg}/entries` | `createOrganizationCreditEntry` | 200 |
| POST | `/api/v1/platform/ai-credits/requests/{creditRequest}/reconcile` | `reconcileOrganizationCreditRequest` | 200 |
| GET | `/api/v1/organizations/{org}/ai-providers/capabilities` | `organizationGetAIProviderCapabilities` | 200 |
| GET | `/api/v1/organizations/{org}/ai-providers` | `organizationListAIProviders` | 200 |
| POST | `/api/v1/organizations/{org}/ai-providers` | `organizationCreateAIProvider` | 202 |
| GET | `/api/v1/organizations/{org}/ai-providers/{provider}` | `organizationGetAIProvider` | 200 |
| DELETE | `/api/v1/organizations/{org}/ai-providers/{provider}` | `organizationDeleteAIProvider` | 200 |
| POST | `/api/v1/organizations/{org}/ai-providers/{provider}/revisions` | `organizationTestAIProviderRevision` | 202 |
| POST | `/api/v1/organizations/{org}/ai-providers/{provider}/publish` | `organizationPublishAIProvider` | 200 |
| POST | `/api/v1/organizations/{org}/ai-providers/{provider}/disable` | `organizationDisableAIProvider` | 200 |
| GET | `/api/v1/organizations/{org}/ai-provider-operations/{providerOperation}` | `organizationGetAIProviderOperation` | 200 |
| POST | `/api/v1/organizations/{org}/ai-provider-operations/{providerOperation}/retry` | `organizationRetryAIProviderOperation` | 202 |
| GET | `/api/v1/projects/{project}/branches/{branch}/ai-gateway/files` | `listAIInputFiles` | 200 |
| POST | `/api/v1/projects/{project}/branches/{branch}/ai-gateway/files` | `createAIInputFile` | 201 |
| GET | `/api/v1/projects/{project}/branches/{branch}/ai-gateway/files/{inputFile}` | `getAIInputFile` | 200 |
| DELETE | `/api/v1/projects/{project}/branches/{branch}/ai-gateway/files/{inputFile}` | `deleteAIInputFile` | 200 |
| GET | `/api/v1/projects/{project}/branches/{branch}/ai-gateway/files/{inputFile}/content` | `contentAIInputFile` | 200 |
| GET | `/ai/v1/{aiBranch}/v1/files` | `gatewayListAIInputFiles` | 200 |
| POST | `/ai/v1/{aiBranch}/v1/files` | `gatewayCreateAIInputFile` | 201 |
| GET | `/ai/v1/{aiBranch}/v1/files/{inputFile}` | `gatewayGetAIInputFile` | 200 |
| DELETE | `/ai/v1/{aiBranch}/v1/files/{inputFile}` | `gatewayDeleteAIInputFile` | 200 |
| GET | `/ai/v1/{aiBranch}/v1/files/{inputFile}/content` | `gatewayContentAIInputFile` | 200 |
| GET | `/api/v1/projects/{project}/branches/{branch}/storage/buckets/{bucket}/settings` | `getStorageBucketSettings` | 200 |
| PUT | `/api/v1/projects/{project}/branches/{branch}/storage/buckets/{bucket}/settings` | `putStorageBucketSettings` | 200 |
| GET | `/api/v1/projects/{project}/branches/{branch}/storage/buckets/{bucket}/objects/settings` | `getStorageObjectSettings` | 200 |
| PUT | `/api/v1/projects/{project}/branches/{branch}/storage/buckets/{bucket}/objects/settings` | `putStorageObjectSettings` | 200 |
| GET | `/auth/sso/provider` | `getPublicConsoleSSOProvider` | 200 |
| POST | `/auth/sso/start` | `startConsoleSSO` | 200 |
| GET | `/auth/sso/callback` | `acceptConsoleSSOCallback` |  |
| POST | `/auth/sso/mfa` | `verifyConsoleSSOMFA` | 200 |
| GET | `/api/v1/organizations/{org}/sso` | `getOrganizationConsoleSSO` | 200 |
| PUT | `/api/v1/organizations/{org}/sso` | `setOrganizationConsoleSSO` | 200 |
| POST | `/api/v1/organizations/{org}/sso/link` | `linkOwnConsoleSSO` | 200 |
| DELETE | `/api/v1/organizations/{org}/sso/bindings/{binding}` | `revokeOwnConsoleSSO` | 200 |

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
