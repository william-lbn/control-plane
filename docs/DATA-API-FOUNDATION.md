# Data API 认证入口：已实现合同与后续 Driver 门槛

## 1. 范围与状态

`api/cmd/control-data-gateway` 是独立 Go 数据服务；`internal/dataapi` 实现分支 JWT 校验与
PostgREST 转发。它不使用 Console Cookie、控制面 API Key、Kubernetes 凭据或数据库管理员密码。
`charts/data-api-gateway` 部署这个入口，`Dockerfile.dataapi` 构建独立无 shell 的非 root 镜像。

**这尚不是 Console 原生 Data API 产品。** 控制面 `services.data_api.enabled` 继续 false。
尚需完成 service intent/Operation、最小权限数据库角色 Driver、每分支 PostgREST 部署、
clone/轮换/删除/恢复、UI、NetworkPolicy、真实 Neon Proxy 冷醒和浏览器端到端验收。
Pod 配置就绪不能代替 PostgreSQL/RLS/产品就绪。

官网依据为本地 website `c0d49cbb…` 的 `docs/data-api/access-control.md`、
`custom-authentication-providers.md`。标准参考：
[PostgREST 认证](https://docs.postgrest.org/en/stable/references/auth.html)、
[配置](https://docs.postgrest.org/en/stable/references/configuration.html)、
[固定发行 16.4](https://github.com/PostgREST/postgrest/releases/tag/v16.4)、
[golang-jwt 5.3.1](https://github.com/golang-jwt/jwt/releases/tag/v5.3.1)。

## 2. 请求链路与隔离

```mermaid
sequenceDiagram
  participant A as Application
  participant G as Go Data Gateway
  participant P as Branch PostgREST
  participant DB as Branch PostgreSQL
  A->>G: /data/v1/{branch}/notes + provider JWT
  G->>G: configured public key / alg / iss / exp / aud / sub / role
  G->>G: branch binding + bounded Ed25519 delegation
  G->>P: configured fixed upstream + trusted delegation JWT
  P->>DB: dedicated NOINHERIT login; SET LOCAL ROLE
  DB->>DB: trusted-hop context guard + GRANT + RLS
  DB-->>A: subject-filtered rows through P and G
```

Provider JWT 必須由静态公共 JWKS 验证；不信任 Token 中的 `jku/jwk/x5u/x5c/crit`。
首版接受 RS256（RSA 2048–4096）或 EdDSA/Ed25519，拒绝 none、HMAC、私钥 JWKS 和未知 key。
必须包含正确 issuer、有效 exp、非空 sub，以及唯一的分支 audience；iat/nbf 如有则检查。
每个路由的 audience 不得重复；JWT 中出现 branch_id 时必须与路由相同。
JWT role 必须在该路由白名单；未指定时使用 default_role。系统高权限角色被拒绝。

内部 delegation 使用独立 Ed25519 密钥，最长 30 秒且不超过 Provider exp。
保留已验证的 sub/应用 claims，重新固定 iss/aud/branch_id/时间字段，并附 provider_issuer。
PostgREST 只配置 delegation 公钥，不能把 Provider 原始 JWT 当作可信内部上下文。
这是当前实现的严格子集：动态 JWKS 自动刷新、EC 算法、Managed Auth 与完整 Provider 配置界面仍待实现。

## 3. 配置模型

环境变量：

| 名称 | 输入与约束 |
| --- | --- |
| NEON_DATA_API_CONFIG_FILE | 单个 JSON 对象，最大 1 MiB；未知字段/尾随对象拒绝 |
| NEON_DATA_API_SIGNING_SEED_FILE | 单独 Secret 文件；标准 base64 编码的 32 字节随机 Ed25519 seed |
| NEON_DATA_API_UPSTREAM_CA_FILE | 可选 PEM 公共 CA；与系统信任合并，始终核验 HTTPS hostname |
| NEON_DATA_API_ALLOW_PLAINTEXT_UPSTREAM | 只有显式 true 才允许配置中的实验室 HTTP upstream |
| NEON_DATA_API_BIND / HEALTH_BIND | 默认 :9080 / :9081；health 端口不暴露公网 |

配置 v1 字段：

| 字段 | 含义 |
| --- | --- |
| version | 必须 1 |
| delegation_issuer | 固定可信内部 issuer，例如 urn:neon:selfhost:data-api |
| allow_plaintext_upstream | 默认 false；仅实验 profile true，与环境变量双重允许 |
| routes | 1–256 个 branch 路由 |
| routes[].branch_id | 当前控制面的 br_ 加 16 位十六进制 ID，不允许重复 |
| upstream | 固定 HTTPS origin，无用户名/密码、path、query、fragment；HTTP 需实验 profile |
| issuer / audience | Provider issuer 与唯一分支 audience；强制核验 |
| default_role / allowed_roles | 非系统角色，默认角色必须在白名单；最多 32 项 |
| allowed_origins | 最多 32 个精确 HTTPS origin；没有通配符/Cookie 授权 |
| jwks | 公共 JWK Set；最多 32 个唯一 kid；use=sig，key_ops 如有只能 verify |

配置存入 config Secret 的 `config.json`；签名文件存入另一个 Secret 的 `delegation-seed`。
Chart 不创建、生成、记录真实 Secret，不将它们拼进命令行。
配置为不可变启动输入；更新后由部署系统执行受控 rollout。`-print-delegation-jwks`
只输出公共 JWK，不输出 seed，供 PostgREST 配置。

## 4. HTTP 合同

入口：`/data/v1/{branch_id}/{PostgREST path}`，下游移除分支前缀，查询参数保留。
GET/HEAD/POST/PATCH/DELETE 支持；配置来源的 CORS OPTIONS 不访问数据库。
动态表、RPC 和查询语法由固定版本 PostgREST 提供；本入口不伪造兼容 REST 查询引擎。
当前限制：request body ≤1 MiB，query ≤16 KiB，Bearer ≤16 KiB，整体转发 150 秒；
全局并发最多 64，upstream 每 host 8 条 HTTP 连接。百分号编码的 path、重复分隔符和 dot path 拒绝。
这是明确的兼容性限制，后续应以一致解码规则安全扩展。

认证失败 401 + WWW-Authenticate: Bearer；未知 route 404；Origin 不允许 403；
方法不允许 405；超限 body 413；入口并发容量不足 429；上游失败 503。
错误 JSON 只含稳定 code，不反射 Token、Subject、DSN、SQL 或底层错误。
Console Cookie、代理认证与身份伪造头不转发；上游 Set-Cookie/CORS 不被继承。
响应禁止缓存；应用身份 Cookie 不参与授权。

`/healthz` 和 `/readyz` 在单独 health listener：后者只表示配置载入成功，
不会访问 PostgREST、也不会为了探测唤醒 Compute。服务 Driver 必须另做真实 readiness。

## 5. PostgreSQL / PostgREST 部署不变量

不得直接使用 Console 创建的管理员角色或 `control_probe` 给 Data API 服务连接。
已部署 Neon `compute_tools/src/spec_apply.rs` 对普通 spec 新建角色授予 CREATEROLE/CREATEDB/BYPASSRLS
及 neon_superuser 成员资格。应用连接身份必须由独立 Driver 创建和实际检查：

1. authenticator：LOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS。
2. 请求角色：NOLOGIN，不能 owner/BYPASSRLS，不能继承 neon_superuser 或平台角色。
3. authenticator 只可 SET 到明确请求角色；GRANT 与 RLS 分别授权，不给默认匿名表权限。
4. db-config=false、明确 db-schemas allowlist、独立 guard schema、无 db-anon-role。
5. pre-request guard 校验内部 iss、单分支 aud、branch_id 和 sub；函数 SECURITY INVOKER，
   受保护 search_path；不可由任意客户改写。auth.user_id() 的官网兼容函数仍需 Driver 安装。
6. PostgREST 只能从入口访问；用 NetworkPolicy 封锁绕过 Gateway 的直接请求。
7. SQL 连接经 Neon Proxy 使用服务凭据并核验 TLS；短连接/Pool idle 必须与缩零账本协调。

Chart 默认需要 HTTPS Ingress/ClusterIP，只有 laboratory.allowHTTP 可显式降级。
NetworkPolicy、PostgREST、SQL credential 与数据库 schema 尚不由该 Chart 生成：
当前 Chart 是认证入口的部署包，不能独立声称完整 Data API 安装。

## 6. Linux 测试与剩余验收

```bash
bash tools/install-postgrest.sh /secure/ci-tools
export NEON_DATA_API_TEST_POSTGREST=/secure/ci-tools/postgrest
# 使用隔离 PostgreSQL 的 control_ci 或 dataapi_ci，绝不能用 neon_control_v2。
export NEON_V2_TEST_DATABASE_URL='<disposable CI DSN>'
bash tools/ci-go.sh
```

安装器按 services.lock.json 的官方 SHA256 核验 16.4 Linux 静态包。
Go race 测试覆盖 JWT/配置/转发负例；真实 PostgREST/PG 测试检查两个用户的 RLS 可见性、
自有 insert、伪造 owner 拒绝、跨用户 update 无效、匿名无授权、Console Cookie 不授权。
全量 CI 缺 PostgREST 或真实 disposable PG 时失败，不用跳过冒充通过。

仍需验收：真实 Neon branch 创建后从 UI 启用、数据库/角色最小权限、Endpoint 冷醒、
两个分支和两个应用用户的负例、凭据/JWKS 轮换、应用请求与缩零竞争、恢复/删除、
网络/TLS 与扩容。完成前 Data API capability 保持 disabled。
