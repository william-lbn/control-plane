# 2026-10-16：Console MFA 本地交付

## 已实现与部署

Go API/Worker、PostgreSQL 迁移 034、React 登录及个人设置已加入标准 TOTP、一次性
恢复码、跨实例消费、其他会话撤销、旧版 session INSERT 保护和 OpenAPI。
不依赖分支应用 Auth、SMTP 或公网域名，操作合同见 [CONSOLE-MFA](CONSOLE-MFA.md)。

本地镜像源码快照 `8ce799a06362b7829f92c6fea0797ff805b7cbf767d31b5d99993b69c7cec1f9`。
API digest `sha256:0b48f7f34ed0a1f799ca056f2919e9378bf671df740e03e606b92743e785e760`，
Web digest `sha256:85d6cc427f52165f5d7f5be26537230b00c045f4cb0b9dd6440cacaa7c7b3d58`。
OpenAPI 0.38.0、Chart 0.9.25；本地构建、三节点逐项校验/导入和窄范围 Helm 升级。
升级前 revision 91。元数据备份 SHA-256
`25531f97c71cecb963fd25a5f7e654daca93debb82a8233a99281ac566554e55`；
9 个 PVC、779 个非 Helm Secret 的 UID 和其他六个 neon release manifest 保持。
这些本地镜像尚不是公开 registry 发行。

## 验收及原失败保留

两个 Linux race 测试通过，其中一个使用独立真实 PostgreSQL：密文、过期、密码/因素
负例、跨账号、旧会话、旧 API INSERT 拒绝、计数重放及恢复码并发消费。vet、前端类型
和构建通过；没有重跑旧全量功能套件。

UI 从新邀请的 viewer 注册开始，完成密码负例、绑定和实际启用。十个恢复码已显示，
首次测试却在 textarea 精确 label 定位失败。原 DOM 快照与原响应的十个码已在私有
工具中逐项比对，未重新创建账号/因素。后续还修复了测试脚本重复折叠 details、续测后
未进入组织页面的导航问题。已确认的测试等待用自有 Job UID/有界 deadline 停止。
原失败和 checkpoint 均保留，没有改产品绕过失败或自动重试。

最终 Linux UI Job `publication-ui-20261015185626` 沿用原账号/因素，累计 **12 项**检查
通过：密码独自登录拒绝、错误证明、真实 TOTP 登录及重放拒绝、恢复码登录及重放
拒绝、刷新不回显、关闭 MFA 后其他有效登录后续 401、恢复密码登录。续测真实消费
原有未使用恢复码，按实际剩余数量验证。原启用时旧浏览器会话后续请求断言因首次
textarea 定位失败未执行；该不变量由真实 PG 测试覆盖，最终关闭时两个事先有效的
浏览器会话后续 401 已实际验证。

admin 的因素未改变；没有 SQL、Compute/VM/Runner 创建。私有 evidence 位于
`control-plane/production/evidence/20261016-mfa`。同一账号、所有失败、checkpoint 和
可复测输入继续保留；含因素的 DOM/诊断不上传公开 GitHub artifact。公共 Git 不包含
凭据、原始私有证据或 Python 传输脚本。

## 独立后续门槛

后续 0.39.0 的 [当前交付](DELIVERY-2026-10-16-CURRENT.md)在同一完成账号的正常再次
绑定中，以真实 UI 补验了恢复码文本框和“启用后此前有效的其他浏览器登录后续 401”，
并正常关闭该因素；原流程没有覆盖的浏览器断言至此另有三项现场回执。

组织强制 MFA、WebAuthn/SSO、完整攻击矩阵、正式恢复运营流程、跨实例外部栅栏、
HA/DR 与可信 TLS 仍须分别交付。单项闭环不代表官网全部功能或生产准入。
