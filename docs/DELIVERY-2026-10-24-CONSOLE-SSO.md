# 2026-10-24 Console OIDC 与组织 SSO

0.81 完成 A02 固定本地 OIDC 基线：现有账号显式绑定、组织 optional/required/disabled 策略、跨站授权码回调及本地 MFA。接口、限制和运维步骤见 [Console SSO 合同](CONSOLE-SSO.md)，逐项结果见 [脱敏验收](acceptance/2026-10-24-console-sso-functional.json)。目前 16 组有基础验收，11 个业务组和 3 个其他组继续；整个产品尚未完成。

4 组新增 Go/PostgreSQL、4 组受影响身份/授权检查、2 组 Helm、TypeScript/Vite、88 套测试入口编译通过。实际浏览器累计 10 个检查点覆盖：提交响应丢失后的原键确认、独立 IdP origin 显式绑定、Strict 会话、强制策略拒绝本地组织访问、OIDC 后本地 TOTP、单次恢复码和重放拒绝、策略代际失效、撤销绑定退出及停用专用组织策略。续验保留原检查点，已通过步骤未重复。

初次 HTTP 环境发现请求键使用仅安全上下文提供的 API，已改用项目现有兼容实现，并重建 Web。随后测试 IdP 的 CSP 阻止 POST 后跨站跳转，已仅向 form-action 加入确切 Console origin；这是测试夹具修复，不改变产品镜像。最终 UI 测试文件与构建归档相同；实际 IdP 修复文件及 SHA 单独记录。

一次失败诊断文本包含新建专用测试账号的控制台密码。该密码已轮换、该账号旧会话已撤销、已导出的文本已脱敏；后续日志/文本在 Linux 端先检查和脱敏，再导出。没有现有管理员或业务凭据受影响。最终运行检查 15 个私有值，未导出凭据。验收后该新账号停用、绑定撤销、策略 disabled，会话和挑战归零；5 个终态验证 Job 与临时 IdP Job/Service/Secret 按 UID/resourceVersion 归档后清理。API 单独挂载的实验客户端配置保留，关联组织和账号均已禁用。

API/Worker 0.81.0、Web 修复镜像部署至 Core Helm 171；schema 065 / 原生存储格式 4 / Chart 0.9.78。API 使用先前已验证镜像，Web 单独重建并内网分发三节点。851 个非文档输入完成对照，其中 497 个 API 输入不变；仅上述测试 IdP 文件有声明的后续变更。058–065 迁移摘要固定。17 张目录表前后摘要相同；本批未发送原生 SQL/对象请求，也未创建 Compute，复用 6 类存储清单原回执。9 PVC、917 个原 Secret 与其他 6 套 Helm 配置保留。三节点、三个 Safekeeper、七个关键组件 Ready，最终 VM/Compute/Functions 实例均为 0。

当前 provider 需要手工配置固定端点与地址白名单，issuer/授权/token/JWKS 必须同 scheme/authority；跨 authority 适配仍需代码扩展，不能当作仅缺外部凭据。此交付不声称商业 IdP、SAML、SCIM 或 WebAuthn 认证。公网 IdP 使用显式出口，内网直连。启用 required 后禁止退回不理解 SSO 的旧 API。性能、压力、可靠性、HA/DR 与完整备份恢复认证按用户要求后置；成功 dump/header/SHA 不等同于恢复测试通过。镜像为本地实验发行，公开仓库提交不等于注册表统一发行完成。
