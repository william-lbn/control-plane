# CLI 与 MCP 0.1.0

CLI 与 stdio MCP 共用固定来源的 HTTP 客户端，读取随代码发布的 [OpenAPI 合同](../contracts/openapi-v1.json)。206 个路径、293 个操作可检索和描述；这表示合同覆盖，不表示每个操作均通过本次实机验证。实际权限始终由当前服务端角色、项目范围及凭据状态决定。

## 安装与配置

使用 Node.js 24.5 或更高的 24.x 版本。在仓库的 `clients` 目录执行 `npm ci --ignore-scripts --no-audit --no-fund`。依赖版本和完整性由 lockfile 固定。MCP 使用官方 TypeScript SDK server 2.4.0，测试使用同版官方 client；普通测试入口为 `npm test`，不会创建实机资源。已验证 Linux Node 24.20；下面的 PowerShell 启动方式不构成 Windows 运行时验收。

把配置保存到仓库外的受保护 JSON 文件，只允许使用者读取，不要提交 Git。以下仅为占位示例，应替换为自己的入口和最小权限凭据：

```json
{
  "version": 1,
  "origin": "https://control.example.invalid",
  "token": "REPLACE_WITH_SCOPED_API_KEY",
  "allow_mutations": false,
  "allowed_operation_ids": ["getProject", "listSavedQueries", "getSavedQuery"]
}
```

只允许一个不含路径、查询、用户信息的固定 HTTP(S) origin。HTTPS 校验证书；私有 CA 可通过 `ca_file` 指定，不能关闭证书验证。内网测试入口使用 HTTP 时必须显式设置 `allow_http: true`，且主机必须符合客户端的 LAN/loopback 判断。私有 IP、localhost、`.svc` 和 `.cluster.local` 强制直连；外网遵循 HTTP_PROXY/HTTPS_PROXY/NO_PROXY。[Node.js HTTP Agent](https://nodejs.org/api/http.html) 提供所使用的 `proxyEnv` 支持。自定义内网域名应使用 HTTPS，并加入 NO_PROXY。

凭据二选一：`token`，或完整的 `session_cookie` 加 `csrf_token`。Console Cookie 应包含 `neon_v2_session` 和 `neon_v2_csrf`，后者须与 `csrf_token` 一致；客户端同时发送固定 Origin 和 CSRF 请求头。会话仍有自己的到期和权限条件，不能用它绕过独立平台审批。

## CLI

在仓库根目录运行：

```powershell
$env:NEON_CONTROL_PROFILE = 'C:\private\control-profile.json'
node clients/cli.mjs search --query SavedQuery
node clients/cli.mjs describe getSavedQuery
node clients/cli.mjs call getProject --path '{"project":"REPLACE_PROJECT_ID"}'
```

`search` 与 `describe` 不访问网络，也不需要凭据。调用前先描述操作，按合同提供 `--path`、`--query`、`--headers`、`--body` JSON。写操作必须同时在配置中设置 `allow_mutations: true` 并显式传 `--write`；allowlist 仍然生效。Idempotency-Key、If-Match 等只接受合同声明的请求头，身份、Cookie、Origin、Host 等托管头不能覆盖。

响应为 JSON，包含 HTTP status、request_id、ETag、Retry-After、幂等重放标记及 body。退出码 0 表示正常返回，HTTP 4xx/5xx 为 2，配置或传输错误为 1。响应中凭据字段和已配置凭据会脱敏；需要保留一次性凭据或原始内容时，使用 CLI 的私有输出文件，妥善保管。

`--body-file` 配合合同允许的 `--content-type` 上传字节；`--output-file` 流式保存原始成功响应，不能覆盖已有目标。上传和原始输出均限制 64 MiB；JSON 请求与普通聚合响应限制 1 MiB。输出先写新建的 `.partial-…` 文件，在成功后发布到新目标，Linux 权限 0600；失败的部分文件保留供检查。文件输出是原始内容，不应用 JSON 脱敏。

没有自动重试。遇到超时、断连或取消，应先观察原操作，再根据该操作合同决定是否用原键取回原回执；不得生成新键创建替代资源。重定向被拒绝，身份始终绑定固定 origin。当前未实现自动分页下载、SSE/WS 交互或 5 GiB 文件传输。

## MCP

将 MCP 客户端的 stdio command 配置为 Node 24.x 的绝对路径，args 为 `clients/mcp.mjs` 的绝对路径；环境只传 `NEON_CONTROL_PROFILE` 配置路径。不要把 Token 放入 MCP 参数、提示词或命令行。stdout 只承载协议。

默认提供 `api_search`、`api_describe`、`api_read`；只有配置明确允许写入时才注册 `api_mutate`。工具参数不接受 origin、凭据或文件路径。先检索、描述，再按 operation_id 调用；写工具可能删除资源或使用已配置的模型额度，实际审批仍由调用方和服务端执行。

官方 SDK 的 [serveStdio](https://ts.sdk.modelcontextprotocol.io/v2/serving/stdio) 同时处理 2025 初始化与 2026 无状态发现协议；本实现通过同版官方 client 分别验证两种模式和取消传播。协议迁移见 [SDK 2026-07-28 支持说明](https://ts.sdk.modelcontextprotocol.io/v2/migration/support-2026-07-28)。没有自行维护 JSON-RPC 方言，也未声称第三方宿主全部兼容。

本次真实接口只使用专用项目范围的 editor Key，创建、更新和保留删除一个专用保存查询，并验证冲突、跨项目拒绝和撤销后拒绝。没有执行保存的 SQL、启动 Compute 或调用商业服务。完整结果见 [验收记录](DELIVERY-2026-10-25-CONTROL-CLIENTS.md)。大对象后置边界见 [环境验证边界](ENVIRONMENT-DEFERRED-VALIDATION.md)；CLI/MCP 基线完成后，O03 的账单接口及外部支付衔接仍待完成。
