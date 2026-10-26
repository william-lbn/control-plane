# 独立 Storage 网关与服务端用量合同（S05）

本合同定义功能实现和待验收边界。新增源码和测试不代表部署/验收已经通过；验收由协调器统一在环境容量稳定后执行。吞吐、容量压力、可靠性、HA、灾备与大对象专项继续后置。

## 入口与实际数据路径

`api/cmd/storage-gateway` 独立启动 `RunStorageGateway`，直接执行现有原生 S3 handlers、分支 SQL 清单、checksum、multipart 和专用产品 blob 访问，并非转发到控制 API。数据端口 8793 仅服务 S3 与 `/storage/v1/{branch}/{bucket}` 签名下载/上传；8794 仅为私有健康端口。Console 登录、工作负载创建、控制器、元数据迁移和 Functions 执行不在该进程注册。

Console 管理 REST 保留原权限与处理器，通过 `withStorageGatewayRoutes` 与网关写同一计量账本。现有 native Storage 格式 5/6/7、bucket/object 清单、原 immutable blob key 和小对象边界保持；独立进程只接受已升级到格式 7 的服务，升级由控制面/Worker 执行。

S3 的 SigV4、预签名、POST policy、Backend credential generation/撤销/过期、组织与项目授权，以及匿名读 PublicAccessBlock 均沿用实际原生检查。请求中的 Host 必须匹配既有 canonical S3 origin。原生 public REST 上传仍校验 capability 的内容摘要、大小、service generation 及当前用户授权；下载保留原公开读/签名检查。

Storage→Functions 的对象事件仍在对象发布的同一分支 SQL 事务写入原 outbox。网关仅装载与原 Functions 配置一致的 MAC 和版本，不装载 Guest CA、artifact 私钥或管理凭据。若存在已启用的匹配 trigger 而未配置 MAC，发布失败回滚，不能静默跳过事件。Worker 继续处理 dirty marker/export 和实际函数调用。

## 权限与部署

新增元数据迁移 `072_storage_gateway.sql`；不得改写 058–069 或 branch native 格式版本。独立网关绝不运行 `migrate`，启动要求 072 已提交。

使用新建、无父角色成员关系的独立 metadata LOGIN。`tools/storage-gateway/metadata-grants.sql` 仅授予授权状态读取、用量账本写入、GC prospective allocation journal、Storage event dirty marker，以及原生签名上传 audit append。不给生命周期业务表 UPDATE、operations INSERT、sessions SELECT 或 DDL。三个固定 search_path 的 SECURITY DEFINER 函数只执行真实 lifecycle 身份的 `FOR SHARE`，避免因 row lock 而授予业务表 UPDATE；独立进程启动拒绝超级用户/建库/建角色/BYPASSRLS 及上述明显越权身份。

独立 chart `charts/storage-gateway` 使用固定 digest 镜像、非 root、只读 root filesystem、私有 emptyDir、有限资源和 Recreate 单副本。ServiceAccount 仅可 GET 显式 `endpointCredentialSecrets` 名称，不得 list/watch/create/update/delete Secrets，也不能管理 Pods/VM/Deployments。新增 endpoint 必须先加入此 operator allowlist。公共 Service 不暴露健康端口。默认要求 HTTPS Ingress 与私有 ClusterIP；HTTP/未验证代理 TLS 只能在显式 laboratory 配置中使用。

现有原生 SQL 访问仍使用 endpoint Secret 的 `control_probe` 密码。此 Secret 还可能包含其他端点凭据，probe 自身权限也大于纯 Storage 表权限；本轮没有将分支身份改造成独立 Storage-only SQL LOGIN，不能宣称已经消除该既有权限边界。Backend keyring 与原生 capability key 也是当前共享信任材料。后续最小化这两类分支/签名身份需独立迁移与切换验收。

主要挂载：

| 输入 | Secret key / 环境变量 | 要求 |
| --- | --- | --- |
| metadata | `database-url` / `NEON_STORAGE_DATABASE_URL_FILE` | 独立 restricted LOGIN；生产 DSN 使用验证 TLS |
| 产品 blob | `config.json` / `NEON_OBJECT_STORAGE_CONFIG_FILE` | 仅专用 product bucket 的 List/Get/Put，无 DELETE |
| Backend keyring | `keys.json` / `NEON_BACKEND_CREDENTIAL_KEYS_FILE` | 与原 S3 credential envelope 的版本一致 |
| 原生 capability | `signing-key` / `NEON_STORAGE_SIGNING_KEY_FILE` | 原 API idempotency key 的相同 base64 字节，保留既有签名 |
| event MAC | `mac-key` / `NEON_STORAGE_EVENT_MAC_KEY_FILE` | 与原 Functions MAC 一致；同时配置 `NEON_STORAGE_EVENT_KEY_VERSION` |
| SQL proxy CA | `ca.crt` / `NEON_PG_CA_FILE` | 默认 `NEON_PG_TLS_MODE=verify-full`，代理 DNS 与证书匹配 |

不向网关挂载 `NEON_OBJECT_STORAGE_GC_CONFIG_FILE`；启动显式拒绝它。普通 blob 客户端仅接入 071 的 prospective journal，物理清扫仍由独立 GC 权限处理。

## 可信用量与准入语义

计量来源为服务端实际消费的 HTTP body 字节、实际交给 ResponseWriter 的 body 字节、HTTP 状态、持续时间与分支/project metadata 身份。HEAD 响应 body 计零。它不使用 `Content-Length`、调用方 usage/request-ID header、`last_used_at` 或客户自报数字；不记录 token、object key、query、body 内容或客户端 Cookie。`X-Neon-Storage-Usage-Id` 返回服务端生成的 receipt ID。

每次即将打开 Storage SQL 的请求，先从已 ready/active metadata 解析 canonical project/branch，在独立事务中持久化 `admitted` receipt。持久化失败则拒绝 SQL/blob 请求。租户按 project，默认跨进程并发 4，允许 operator 设置 1–8。原进程全局并发 8、私有 payload buffer 和对象/分支逻辑配额继续生效。

`storage_gateway_limits` 支持 operator 的 UTC 日请求数和 transfer body bytes 预算，0 表示不限。所有 API/网关进程通过同一个 project advisory lock 与 day row 进行准入，不能各自计算本地余额。每次预留 `2*object_bytes + 8 MiB` 的保守 transfer 预算；准入时要求已观测+未结预留+本次预留不超过日预算。它可能在剩余额度仍大于一个小文件时拒绝请求，此保守行为是明确合同的一部分。计数包括已进入 tenant SQL 检查后的拒绝响应/错误请求；尚未取得 canonical tenant 的无效签名等请求没有伪造租户归属。

完成后仅将实际观测数与 HTTP status 原子写入 receipt，并同时释放预留。重复完成不重复累计。客户端读/写错误或取消、panic 记 `interrupted`，不能等同成功。若进程崩溃、提交结果未知或完成写入失败，receipt 保持 `admitted`，过期后只读 API 显示 `unknown`；其观测数仍 NULL、预留仍保留到该 UTC 日结束。有限 lease 只释放并发占位，不能制造 0 用量，也不能虚报已完成。这里的“HTTP completed”不证明客户端最终落盘，更不等于 object mutation 成功，需结合 HTTP status。

该账本是可信内部业务观测，`billing_enabled=false`。不包含 TLS/TCP/网络重传字节、blob 后端读写量、磁盘物理占用、历史共享字节/clone 去重归属、CPU、请求最终交付证明或发票；`physical_capacity_measured=false`。既有 branch logical quota 也不能当作物理容量。因此 S05 的生产物理容量归属与计费完整性仍未闭环。

只读 Console API 通过原 `s.auth`/project authorization：

- `GET /api/v1/projects/{project}/branches/{branch}/storage/usage`：最近 50 receipt 与当前 UTC 日该 branch 汇总；unknown 不当作 0。
- `GET /api/v1/projects/{project}/storage-gateway`：project 默认/配置限额与当前日 admission/observed/reserved 预算。限额只由 operator 维护，不暴露租户自行提高额度的接口。

## 接入与验收顺序

控制 API 的 main handler 使用 `s.middleware(s.withStorageGatewayRoutes(s.routes()))`；若并行功能有其他 wrapper，可保持组合。S3 handler 自带计量。core chart 显式接管配置如下（示例独立 release 为 `dedicated`，namespace 为 `neon`）：

```yaml
objectStorage:
  enabled: true
  s3:
    enabled: true
    apiListenerEnabled: false
    gatewayService: dedicated-storage.neon.svc.cluster.local
    origin: http://192.168.146.100:30893 # 保留原值；HTTP 仅为显式实验室示例
    labHTTP: true
    serviceType: NodePort
    nodePort: 30893
```

`apiListenerEnabled=false` 同时关闭 API listener、移除 API 的 s3 container port，以及不再渲染 core 的旧 `-s3` Service。因此即使保留旧 `serviceType: NodePort/nodePort` 参数，也不会与独立 chart 争用同一个 NodePort。core 仍向 API 注入原 `NEON_S3_PUBLIC_ORIGIN` 和 region，原 UI S3 endpoint 与签名语义保留。显式独立模式必须同时提供 `gatewayService`；两个 listener 并开、单独关 listener 却没有 REST data route、scheme/port/path 注入的 gatewayService 都拒绝渲染。

Console 原生签名 API 返回的是相对 `/storage/v1/{branch}/{bucket}?key=...&token=...` URL。前端临时上传 `fetch(value.url)`、下载 `<a href=value.url>` 均从 Console 同源访问。Web Nginx 现在有单独 `location ^~ /storage/v1/`：独立模式直接 `proxy_pass http://<gatewayService>:8793`，不会再落入通用 API 正则 location；未接管时仍使用原 API upstream。proxy_pass 不附带 URI、不 rewrite，完整 escaped URI/query 与 capability token 原样保留；该 public path 去掉 Cookie、Authorization、CSRF，并隐藏 Set-Cookie，避免下载链接自动携带的 Console 身份流入独立进程。错误日志不输出含 capability 的原始 URI。请求不缓存重放，body/响应 buffering 关闭，payload 与有限 timeout 跟随 operator Storage 预算。

`/api/v1/.../storage/...` 的签名签发、目录管理与 Console 授权继续走 API；实际签名上传/下载 body 则经 Web 直接到独立网关。独立 chart 的 `restHost` 是额外直接 Ingress 入口的可选方案，不能代替上述 Console NodePort/Web Nginx 配置。S3 public origin 由独立 chart 的 Service/Ingress 接管。

固定 NodePort 必须分阶段转移，不能同时安装两个声明相同端口的 Service：先以 ClusterIP 安装独立 release 并完成受限身份/小对象检查；再更新 core 上述接管配置，使旧 Service 释放 NodePort 且 Web 签名路径改指独立 ClusterIP；最后将独立 Service 改为原 NodePort。该有限维护窗口不宣称 HA/无损切换。回退时先把独立 Service 改回 ClusterIP 释放端口，再将 core `apiListenerEnabled=true,gatewayService=''` 恢复旧 Service 和 Web upstream；业务目录、blob、PVC、Secret 和账本全部保留。

控制面先应用 071/072，再新建并授权独立 metadata LOGIN、准备明确 Secret allowlist 与原 trust 材料，渲染独立 chart，执行小对象读写/签名拒绝/撤销拒绝/clone 隔离与现有事件 outbox 的新增路径功能验收，再切换入口。回退入口到原 API 保留同一目录与账本，不删除 blob、VM、PVC、Secret 或历史业务数据。

协调器已验证通过 Pure 对应组、LedgerPG1、vet 与独立 binary 编译；后续 chart 接入变更未修改 Go，不重复这些已过组。现有功能测试源码：`TestStorageUsage*`、`TestStorageGatewayDoesNotExposeConsoleOrHealthOnDataPort`、`TestStorageGatewayEventsConfiguration`、`TestStorageGatewayLedgerCrossProcessQuotaAndUnknown`。集成测试只允许 disposable `control_ci` 的专用随机 schema，覆盖跨 pool tenant cap、日请求/transfer quota、重复完成、不可修改 receipt、外租户拒绝与 crash unknown 保留；不写 blob、不做物理删除。另需真实独立进程使用受限 metadata role、RBAC allowlist、原生小对象/事件和入口切换的功能证据。不能由 gofmt/测试源码存在宣称整体验收完成。

新增仅针对接管路径的静态渲染组 `tools/helm-tests/storage-gateway.test.mjs`：旧部署兼容、旧 NodePort 释放、origin 保留、签名 REST 直达、payload/timeout 预算、非法配置拒绝，以及独立 chart 的 NodePort/RBAC。此组需协调器统一运行；它不能代替真实部署接管与现有用户小对象路径的功能证据。

2026-10-27 主代理已完成本文新增 Go/PG、独立 binary 编译、vet 及五组 Helm 检查；具体复用与失败续验记录见[增量回执](DELIVERY-2026-10-27-FUNCTIONAL-SLICES.md)。本记录不声明尚未执行的实机接管通过。
