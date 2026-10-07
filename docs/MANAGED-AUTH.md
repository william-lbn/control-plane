# 分支 Managed Auth v1

## 1. 范围与状态

使用 **Better Auth 1.7.7、node-postgres 8.23.1、Node 24** 的独立 TypeScript 运行时；控制 API、Worker、权限、操作调谐和生命周期仍由 Go/PostgreSQL 管理。依赖版本、完整性和许可证锁定，运行时不使用 Console 账号数据库。

对齐 [Neon 分支身份语义](https://neon.com/docs/auth/overview)、[Better Auth 会话](https://better-auth.com/docs/concepts/session-management)和 [JWT/JWKS](https://better-auth.com/docs/plugins/jwt)。不宣称与全部 Neon SaaS SDK 或商业特性兼容。

已实现：邮箱/密码注册登录、HttpOnly 签名 cookie、数据库会话、检查/列举/撤销会话、退出、修改密码、基础用户更新、五分钟 Ed25519 JWT、公开 JWKS、管理员读取最近 100 个用户、启停与重新启用、分支身份复制、保留删除恢复和 Data API 配置联动。

Linux 开发验收包括 337 个 Go 测试（零跳过）、13 个前端测试、真实 PostgreSQL 的 Auth 库集成和 Helm 渲染/拒绝测试。**真实 Neon UI 验收以独立交付报告为准；代码与临时 PostgreSQL 测试不等于已部署验收。**

尚未验收或实现：SMTP、邮箱所有权验证/重置投递、OAuth、MFA/SSO、定制 Auth 域名、完整用户分页/管理员生命周期、KMS 密钥轮换、动态 Data API JWKS 刷新、分布式入口限流认证、全链路可信 TLS、HA/DR、外部 SQL 缩零栅栏。注册成功不代表邮箱已验证。

## 2. 架构与数据边界

```mermaid
flowchart LR
  UI[React Console 管理员] --> API[Go API: RBAC / CSRF / CAS / 幂等]
  API --> Meta[(控制元数据与 Operations)]
  Meta --> Worker[独立 Go Worker]
  Worker --> Kube[分支 Secret / Deployment / Service]
  App[应用客户端] --> Relay[Go 分支入口]
  Relay --> Auth[Better Auth TypeScript 运行时]
  Auth --> Proxy[Neon Proxy / writer selector]
  Proxy --> SQL[(用户分支 neon_auth schema)]
  Auth --> JWT[分支 JWT / 公开 JWKS]
  JWT --> DataAPI[受限 Data API / PostgREST]
  DataAPI --> RLS[PostgreSQL FORCE RLS]
```

控制库的 `managed_auth_instances` 保存项目、分支、writer、递增 generation、状态、公开配置和不可变 `secret_ref`。密码、密码哈希、签名私钥、应用 cookie、会话和 JWT 不进入控制元数据、Operation、审计或公开测试记录。

身份数据位于用户数据库的 `neon_auth.user/session/account/verification/jwks/rateLimit`；`control_installations` 是仅供 provisioner 使用的幂等标记。SQL 来自锁定库的迁移生成器，保存在 `api/internal/control/assets/auth-schema.sql`。schema 所有权注释不匹配时拒绝接管。

`control_auth_<branch suffix>` 仅有身份表 CRUD、schema USAGE 和数据库 CONNECT；没有 schema/table CREATE、SUPERUSER、BYPASSRLS、角色/数据库管理、复制、继承成员权限或安装标记访问。运行时最多一个连接，一秒池空闲回收，专用角色五秒服务端空闲断开；健康检查不访问 SQL。

## 3. 管理 API

准确合同为 `contracts/openapi-v1.json` 0.9.0。四个路由均需项目管理员；Console 写请求需 CSRF。组织范围 API key 沿用既有租户规则。

| 路由 | 输入/返回 | 语义 |
|---|---|---|
| `GET /api/v1/projects/{project}/branches/{branch}/auth` | 实例、数字 ETag、issuer/audience、可选运行状态 | 仅观察，不读用户 SQL、不唤醒 Compute |
| `POST .../auth` | `{database, allowed_origins: []}`；`If-Match`、`Idempotency-Key` → 202 Operation | 仅从 disabled 启用；需要 ready writer；域名/镜像由部署锁控制 |
| `DELETE .../auth` | 相同版本/幂等头 → 202 Operation | 关闭入口、移除 Proxy 身份、停止运行时，保留数据与 Secret |
| `POST .../auth/users` | 空请求 → 最近 100 个安全用户记录 | 明确 SQL 操作，可冷唤醒；不返回账户密码、会话、token、私钥 |

主要错误：缺版本 428、版本变更 412、操作/幂等冲突 409、配置非法 422、实例/权限不可见 404、前置条件或运行时不可用 503。观察失败不能重新发一个创建请求；修复后重试原 Operation。

应用入口为 **`/auth/v1/{branch}`**。只允许 GET/POST/OPTIONS 和 `sign-up/email`、`sign-in/email`、`sign-out`、`get-session`、`token`、`jwks`、`list-sessions`、`revoke-session`、`revoke-sessions`、`revoke-other-sessions`、`change-password`、`update-user`。请求体/会话合同遵循锁定的 Better Auth 版本；没有任意 URL 转发或任意插件接口。它与 Console 的 `/api/v1/session` 独立。

Go 入口剔除 Console cookie、Authorization/CSRF 和调用方伪造的转发头；只转发此分支的应用 session cookie。仅固定且处于 active 的 live 分支运行时可达。返回 cookie 限于此分支名称、路径、HttpOnly 和无跨域 Domain。

## 4. 分支复制、密钥与生命周期

```mermaid
sequenceDiagram
  participant U as Console
  participant W as Go Worker
  participant N as Neon Timeline
  participant M as 控制元数据
  participant A as 子分支 Auth
  U->>M: 创建分支/writer Operation
  W->>N: 创建独立 Timeline
  W->>M: writer ready + 原子排队 Auth companion Operation
  W->>A: 新分支 Secret / issuer / cookie 域
  W->>N: 保留 users/accounts；仅清除子分支 sessions/JWKS
  W->>A: 受限 SQL 身份、Proxy route、Deployment
  W->>M: 原租约下提交 Auth active
```

Neon Timeline 复制用户/账户记录。父 Auth 为 active 时，子分支的首个 writer 就绪事务原子排队独立 Auth Operation；无 Compute 的分支等待 writer；Reader 不承担 Auth writer。PostgreSQL 分支就绪与 Auth 服务就绪独立呈现。

每个分支具有独立 URL/issuer、分支 ID audience、cookie 名称/路径及随机 Secret。首次安装或重新启用仅清理本分支的 sessions/JWKS，保留用户与密码账户。同 generation 重试不清除已签发密钥；新 generation 旋转身份域。父 cookie 不成为子会话，父 JWT 不授权子 Data API，子身份修改不影响父数据库。

JWT 有效期五分钟。登出撤销数据库会话并阻止继续发 JWT，**已有 JWT 在静态 JWKS 的 Data API 中可一直使用到到期**；不要声称立即撤销 bearer token。禁用/重新配置 Auth 前须禁用依赖它的 Data API；重新启用后，通过 UI 刷新公钥再启用 Data API。更换 Auth 数据库需要独立迁移，不允许简单 re-enable 搬迁身份。

项目/分支删除先关闭入口，再按所有权和 resourceVersion 退役运行时；保留 SQL、Timeline、WAL、对象、Secret 与证据。保留恢复使 PostgreSQL 可再次使用，但 Auth 保持 disabled，显式重启时使用新 generation。物理 GC 仍为 held。

## 5. Linux/Helm 与人工部署

canonical chart 参数：`managedAuth.enabled/labHTTP/runtimeImage/publicOrigin`。v1 要求显式实验室 HTTP 例外；不是生产 TLS 认证。镜像必须取发行版已验证 digest，不可使用 latest。publicOrigin 必须为精确 scheme/host/port，无路径、尾斜杠、通配符或凭据。

```yaml
managedAuth:
  enabled: true
  labHTTP: true
  runtimeImage: docker.io/williamluckyli/control-auth@sha256:<发行版验证的 digest>
  publicOrigin: http://192.168.146.100:30788
```

API/Worker 接收一致参数。Auth Pod 无 ServiceAccount token、特权和 hostNetwork，根文件系统只读，只挂载自己分支的配置文件；请求 50m CPU/64Mi 内存，限制 500m/256Mi。数据库触发器在多个 API 之间限制最多八个非 disabled 实例；有限环境中及时停用无用服务。

1. UI 创建项目，在 SQL 工作台由数据库所有者委托选定数据库权限：

   ```sql
   GRANT CREATE, CONNECT ON DATABASE postgres TO control_probe WITH GRANT OPTION;
   ```

2. 打开 **Auth**，选择分支/数据库/可信来源，启用并观察原 Operation。
3. 在应用体验面板注册、登录、检查会话、签发 JWT 和退出。密码执行后清空，仅临时存在页面/HTTPS 请求；不写控制库或测试报告。
4. 按 [Data API Driver](DATA-API-NATIVE-DRIVER.md) 准备业务 schema、ENABLE/FORCE RLS 和授权。不自动猜测业务策略。
5. Data API 点击 **使用当前分支 Auth**，加载 issuer/audience/公开 JWKS；应用从 `/token` 获取 JWT 调用 Data API。不要把数据库 owner 用作 RLS runtime 角色。

## 6. Linux 复测与发布

```sh
cd services/auth
npm ci --ignore-scripts --no-audit --fund=false
npm audit --audit-level=high
npm run typecheck
AUTH_TEST_DATABASE_URL='postgres://disposable-user:disposable-password@127.0.0.1:5432/disposable-auth' npm test
```

上述命令只能指向专用临时 PostgreSQL。测试创建/删除随机隔离 schema，不能使用用户库或控制库。Go 测试只在专用 CI 数据库服务器创建/移除其随机 Auth database/role；CI 不允许悄悄跳过集成测试。

公开 `web/e2e/managed-auth.spec.ts` 从 React Console 开始，真实创建 Neon 项目，启用 Auth、注册用户、验证 Data API/RLS、创建独立子 Timeline、验证继承与隔离、自动缩零/冷唤醒、重新启用保留用户。最后仅停用自有服务和 Compute，保留数据与重放凭据。Linux 受保护测试输入见 [TESTING.md](TESTING.md)。交付报告单独记录每次失败和最终通过的版本/证据；本地产品验收不等于全官网或生产准入。
