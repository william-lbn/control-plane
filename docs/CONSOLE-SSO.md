# Console OIDC 与组织 SSO

控制台支持 OIDC 授权码流程、S256 PKCE、浏览器绑定的一次性 state/nonce、现有账号显式绑定、组织策略和本地 MFA。身份提供商证明外部主体；组织成员和项目授权仍由控制面管理。邮箱、显示名和 IdP role 声明不会创建账号、合并账号或授予权限。

实现依据 [OIDC Code Flow](https://openid.net/specs/openid-connect-core-1_0.html#CodeFlowAuth)，使用固定的 [CoreOS go-oidc v3.21.0](https://github.com/coreos/go-oidc/tree/v3.21.0) 和 OAuth2 客户端；JWT 允许 RS256/ES256。SAML、SCIM、WebAuthn、动态客户端注册、隐式流程和所有商业 IdP 的认证不在本合同内。

## 运维配置

Chart 默认 `consoleSSO.enabled: false`。启用时必须保留版本化 Backend 加密密钥，并指定 `consoleSSO.existingSecret`；Secret 的 `config.json` 只读挂载到 API，Worker/Web/浏览器不会收到 IdP client secret。运行时也可通过 `NEON_CONSOLE_OIDC_CONFIG_FILE` 指定文件。

配置结构如下，所有示例占位符必须由运维替换。明文配置应保存在仓库外，通过已有 Secret 管理流程提供。

```json
{
  "version": 1,
  "public_origin": "https://console.example.com",
  "lab_http": false,
  "egress_proxy_url": "",
  "providers": [{
    "id": "company-idp",
    "name": "Company identity",
    "organization_id": "<existing-organization-id>",
    "issuer": "https://identity.example.com/realms/company",
    "authorization_url": "https://identity.example.com/realms/company/protocol/openid-connect/auth",
    "token_url": "https://identity.example.com/realms/company/protocol/openid-connect/token",
    "jwks_url": "https://identity.example.com/realms/company/protocol/openid-connect/certs",
    "client_id": "<registered-confidential-client-id>",
    "client_secret": "<operator-provisioned-client-secret>",
    "auth_method": "client_secret_basic",
    "allowed_ips": ["<approved-exact-address>"],
    "lab_http": false
  }]
}
```

`public_origin` 是固定控制台 origin，不能包含路径/查询/片段；回调固定为该 origin 的 `/auth/sso/callback`，登录后固定回到组织页面。HTTPS 要求 `NEON_COOKIE_SECURE=true`；实验室 HTTP 必须同时在全局和该 provider 明确开启，且只能访问显式批准的私有地址。

当前手工配置要求 issuer、授权、token、JWKS 使用同一 scheme/authority；不从未经批准的发现文档导入新地址。跨 authority 的 IdP 需要后续的逐端点授权扩展，不能仅填地址绕过限制。每次连接检查完整 DNS 答案并拨号到已批准的字面 IP，拒绝重定向、地址漂移、超时和超过 1 MiB 的上游响应。`auth_method` 仅接受 `client_secret_basic` 或 `client_secret_post`。

`egress_proxy_url` 只用于公网 IdP 的 CONNECT 转发，TLS 仍验证原 hostname/系统证书；内网 IdP 总是直接连接，不采用环境中的通用代理变量。更换出口不改变组织权限。

## 开通、绑定与强制

1. 运维将 provider 限定到已存在的组织。组织管理员在控制台选择 provider，输入当前控制台密码，先保存 `optional` 策略。
2. 现有组织成员使用自己的本地账号登录，输入当前密码后点击绑定，完成 IdP 登录。外部主体绑定到该账号及确切 provider 配置，不能根据同邮箱接管另一个账号。
3. 管理员通过组织 SSO 重新登录，确认当前会话已通过 SSO，再将策略改为 `required`。尚未完成此步骤的本地会话不能直接启用强制策略。
4. `required` 约束该组织的控制台资源访问。其他组织沿用各自策略；机器 API key 继续使用原有范围、角色、过期和当前成员校验，不冒充浏览器 SSO 会话。

策略 PUT 必须有当前 quoted `If-Match`、稳定 `Idempotency-Key` 和当前密码。原键成功回执优先于旧版本检查；未知响应时页面冻结编辑，继续确认原请求，禁止另造请求覆盖未知结果。每次变更重新检查组织管理员与账号会话；原键对应其他请求返回 409，版本过期返回 412。

策略更新仅保留已经证明身份的发起 SSO 会话，其余旧策略代际的 SSO 会话立即失效。停用策略会使相关 SSO 会话失效；本地登录仍能用于未要求 SSO 的组织。组织要求 SSO 后，修改/停用策略也需要当前组织的有效 SSO 会话及新密码证明。运维应在更改 issuer/client/端点/地址配置前切换到允许本地登录的维护状态，避免失去管理入口。

client secret 轮换不改变外部身份绑定；issuer/client/端点、认证方式或地址配置变更需要显式策略更新与重新绑定。成员移除、组织停用、账号停用、绑定撤销、策略/提供商代际变化都会使关联 SSO 会话失效。撤销自己的绑定同时撤销该绑定的会话和未完成的 MFA 挑战。

## 会话与本地 MFA

开始请求必须来自配置的 Console origin。state 与浏览器回调 cookie 绑定，10 分钟过期；数据库只保存 state/nonce/cookie 摘要和目的隔离加密后的 PKCE verifier。state 在 token 交换前原子消费；超时、拒绝或未知交换结果必须重新开始授权，不能自动重放原 code。

token 交换后检查签名、确切 issuer、client audience/azp、过期、签发时间和 nonce。不会用 access token 或邮箱声明代替 ID Token。正式会话仍使用 HttpOnly/Strict cookie 和独立 CSRF；跨站回调仅使用独立、短期、HttpOnly/Lax cookie。

账号启用本地 MFA 时，OIDC 成功只创建五分钟、最多五次的挑战，尚不能访问组织。用户必须再完成本地 TOTP 或单次恢复码；原有重放、恢复码消费和会话撤销语义继续生效。最终会话取 ID Token 有效期和八小时的较小值。发起请求按连接 IP 有界限流，反向代理部署需另行配置入口流量策略，不能据此声称已完成生产容量或可用性认证。

身份绑定、策略、登录和撤销与审计在同一事务提交。metadata 不可用返回 503；不能把存储故障误报为账号不存在或无效证明。回调响应无缓存，NGINX 访问日志不含 query，回调错误日志不记录原授权码 URI。

## 功能验收与发行边界

本地验收使用独立组织、独立账号、跨主机的受控 IdP、固定客户端与真实浏览器/PostgreSQL。IdP 是测试夹具，不代表外部供应商认证。成功回执、首次失败原因、原夹具续验和修复以对应 delivery 为准；性能、压力、HA/DR 和完整恢复认证按用户要求后置。

开启组织强制策略后，不允许回退到不理解此策略的旧 API。发行/运维必须保留支持 schema 065 和组织 SSO 授权的 API 版本，或先用当前管理员会话显式解除强制策略。历史备份、IdP client secret、验证器和恢复码不会进入公开仓库。浏览器失败诊断须在导出前脱敏，不能只依赖截图遮罩。
