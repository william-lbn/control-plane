# 应用 OAuth、手机和签名事件

本功能扩展分支应用 Auth，与 Console SSO、Console 组织权限分开。协议见 [应用 Auth 合同](../contracts/application-auth-v1.json)。部署和实机验收结果另记交付文档；本说明本身不是部署证明。

## 运维配置与分支选择

在 Auth 所在命名空间创建不可变 Kubernetes Secret，`config.json` 使用下列结构。示例中的客户端、地址、项目 ID 和 IP 必须替换；`PUBLIC_IP_PIN` 不是可直接使用的 IP。DNS 的所有返回地址都必须在批准清单内。

```json
{
  "version": 1,
  "webhooks": {
    "delivery": {
      "project_ids": ["prj_0123456789abcdef"],
      "branch_ids": ["br_0123456789abcdef"],
      "destination": {
        "url": "https://hooks.example.com/auth",
        "addresses": ["PUBLIC_IP_PIN"]
      }
    }
  },
  "oauth": {
    "google": {
      "project_ids": ["prj_0123456789abcdef"],
      "branch_ids": ["br_0123456789abcdef"],
      "provider": {
        "id": "google",
        "kind": "oidc",
        "clientID": "REPLACE_CLIENT_ID",
        "clientSecret": "REPLACE_CLIENT_SECRET",
        "authorizationURL": "https://accounts.google.com/o/oauth2/v2/auth",
        "issuer": "https://accounts.google.com",
        "token": {
          "url": "https://oauth2.googleapis.com/token",
          "addresses": ["PUBLIC_IP_PIN"]
        },
        "jwks": {
          "url": "https://www.googleapis.com/oauth2/v3/certs",
          "addresses": ["PUBLIC_IP_PIN"]
        },
        "scopes": ["openid", "email", "profile"]
      }
    }
  }
}
```

Helm 设置 `managedAuth.integrationsSecret` 指向该 Secret。控制台只显示当前项目、分支可选的别名和 OAuth 回调地址，不返回客户端密钥、目标 IP、代理或签名种子。项目列表必填；省略 `branch_ids` 允许该项目的分支选择，填写时只允许列出的确切分支。

分支配置通过现有 Auth 的 generation、If-Match 和 Idempotency-Key 发布，例如：

```json
{
  "database": "postgres",
  "allowed_origins": ["https://app.example.com"],
  "oauth_providers": ["google"],
  "webhook_id": "delivery",
  "webhook_events": ["send.otp", "send.magic_link", "user.before_create", "user.created"],
  "webhook_timeout_seconds": 5,
  "phone_number_enabled": true,
  "phone_number_expires_in": 300
}
```

密钥与网络设置只从运维配置选取。API 在受理操作时固定 Secret UID；尚未创建分支配置时替换运维 Secret，会使原操作拒绝使用新身份。已创建的不可变分支配置可按原意图重试。轮换采用新 Secret、新一代分支配置。分支运行时仅取得获准且已选择的条目，不挂载完整运维配置。

外网 HTTPS 可显式配置 `egressProxyURL`，仅支持固定 IP 的 HTTP CONNECT 代理，例如 `http://PROXY_IP:PORT/`。连接仍固定到批准的目标 IP，验证目标主机名和证书；可提供独立公开 CA 的 `caPEM`。私网连接直接访问，不使用该代理，也不读取环境代理。实验室私网/HTTP 例外需要目标 `allowLabHTTP: true` 及集群实验室配置共同启用。

## OAuth 与账号绑定

`POST sign-in/social` 使用标准 Better Auth 接口；提供商登记的回调必须为当前分支的 `{AuthBaseURL}/callback/{provider}`。应用最终落地的 callbackURL、newUserCallbackURL、errorCallbackURL 需要属于精确可信来源。每个分支的回调单独登记，仓库不包含商业平台的共享客户端。

授权码流程强制 PKCE，维护库负责浏览器 state 和一次性使用；分支层额外绑定提供商。OIDC 通过批准的 JWKS 校验签名、issuer、audience、有效期、nonce 和已验证邮箱。`github` 适配使用用户资料与主邮箱接口，仅接受已验证的主邮箱；`vercel` 适配读取批准的用户资料并要求已验证邮箱。自定义 OIDC 同样使用 `kind: oidc`。

同邮箱的已有账号不会自动绑定提供商。先登录该应用账号，再使用 `link-social`，且提供商邮箱必须一致。`list-accounts`、`unlink-account` 只操作当前应用身份；最后一种账号登录凭据不能解绑。第三方访问、刷新、ID 令牌不持久保存，保留的是 provider/subject/user 绑定。本协议不提供第三方 API 访问令牌管理。已有封禁继续限制第三方登录。

已有绑定在再次登录时，若提供商签名证明相同邮箱已验证，维护库会更新该应用邮箱的验证状态；首次回调即可建立会话。未绑定账号不会仅因邮箱相同被标记或自动合并。

## 手机登录

先通过已有应用会话发送短信并调用 `phone-number/verify`、`updatePhoneNumber: true` 完成手机绑定。之后可通过 `phone-number/send-otp` 和 `phone-number/verify` 登录。号码必须是带国家区号的 E.164 格式；未绑定号码不能直接注册账号。

短信由阻塞事件 `send.otp` 交付，`delivery_preference` 为 `sms`，需自行接入运营商。验证码六位、有效期 60–600 秒，默认 300 秒，最多三次错误尝试；原生表只提交分支专用 HMAC 摘要。并发验证仅能消费一次。发送失败回滚新挑战，保留此前仍有效的挑战。公开注册和修改资料接口不能直接写入已验证手机号。

## 七种签名事件

| 事件 | 提交关系 |
|---|---|
| `send.otp` | 阻塞；邮件或短信代码，接收端需要成功受理 |
| `send.magic_link` | 阻塞；登录、邮箱验证或密码找回链接 |
| `user.before_create` | 阻塞；返回 `{"allowed":true}` 或明确拒绝，无法确认时拒绝创建 |
| `user.created` | 非阻塞，用户写入的同一事务入队 |
| `phone_number.verified` | 非阻塞，仅手机验证路径入队 |
| `organization.invitation.created` | 非阻塞，邀请创建事务入队 |
| `organization.invitation.accepted` | 非阻塞，成员实际插入并提交后入队 |

接收端应按 event_id 幂等处理。每批最多三次 HTTP 尝试，总上限 15 秒；单次默认 5 秒，可配置 1–10 秒。成功和明确 4xx 不自动重试，408/429 可重试；不跟随重定向，响应限 10 KiB。注册校验要求明确的 boolean allowed，异常或无效响应不会放行。

事件公钥为 `GET .well-known/jwks.json`，无需 SQL 准入。`X-Neon-Signature` 使用 Ed25519 分离 JWS，公钥 kid 来自分支稳定公钥；用户字段采用固定白名单。验证签名时使用收到的原始 body 字节：

```text
protected = base64url(JWS protected header)
message = base64url(X-Neon-Timestamp + "." + base64url(rawBody))
signingInput = protected + "." + message
X-Neon-Signature = protected + ".." + base64url(Ed25519(signingInput))
```

同时校验签名 kid、事件 ID、类型、项目/分支/端点以及时间戳窗口。一次 HTTP 重试批次保持原始 body、event_id、时间戳和签名一致；接收端应保存已处理 ID，防止重复业务副作用。

非阻塞队列最多保存 1000 条未完成事件；达到上限时增加 dropped_events，业务事务继续。实际业务活动触发有界派发，并合并派发期间到达的活动；不会为轮询而唤醒空闲 Compute。`webhooks/events` 仅向当前应用管理员提供最近 100 条元数据，隐藏事件 payload、验证码和链接；`webhooks/drain` 可显式派发最多三条待处理事件。失败状态和丢弃计数可见；该机制不构成外部接收端长期可用性或无限重放承诺。

新一代配置清除待用手机挑战，丢弃旧代待投递事件，避免将旧 payload 发往新地址。物理复制保留应用用户、已验证手机和 OAuth 绑定；新分支使用独立手机号摘要密钥与签名密钥，复制的挑战和待处理事件失效。更完整的 Auth 删除/恢复一致性、全链路 TLS 和性能/可靠性专项分别验收。
