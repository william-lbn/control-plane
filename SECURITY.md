# 安全政策

本项目为预览版本，尚无生产安全认证。
请使用 GitHub 的 “Report a vulnerability” 私密渠道报告敏感问题。
如该渠道尚未启用，请先创建不含利用细节或秘密值的联络 Issue 请求私密沟通方式。

不得在 Issue、PR、日志、浏览器 trace 或截图中发布真实账户密码、API Key、DSN、
Kubernetes Secrets、私钥和数据库数据。普通 Bug 报告应包含脱敏 request_id、
Operation ID、镜像 digest、源码提交、复现步骤与预期行为。

当前核心边界：

- 服务端组织/项目授权和有界 Key scope；API Key 仅保存哈希。
- Session cookie 与写请求 CSRF；浏览器 UI 不是授权边界。
- 创建请求用 HMAC 做幂等内容指纹；该密钥须在恢复时保持一致。
- 分支应用 Token 只首次返回，metadata 保存 versioned pepper HMAC；密钥与数据库分开备份。
- Data API 使用独立 NOSUPERUSER/NOBYPASSRLS 身份、强制 RLS 和应用 JWT；Console Explorer 的会话不进入分支服务。
- Compute 管理使用 Endpoint 身份、短期凭据和 TLS 校验。
- API 当前只允许一副本；没有通过跨实例缩零/外部 fencing 的验收。
- API 在命名空间范围内有 Secrets 与工作负载管理能力，须隔离管理网络。
- 对外 SQL 入口由 Proxy 承担，不能公开 Storage、Safekeeper 或 Compute 管理端口。
- 默认 Charts 非 root、只读根文件系统、drop ALL；镜像固定 digest。
- 禁止生产采用 HTTP Console、lab-insecure 或关闭数据库证书校验。

完整风险、缺失功能与发行门槛见 docs/PRODUCTION-GATES.md。
