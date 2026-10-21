# 动态 AI 供应商与模型目录

I01 本地基础业务已验收并启用，见[精确交付](DELIVERY-2026-10-20-AI-PROVIDERS.md)。当前 API/Worker/Web/独立 AI Gateway 0.58.0 / migration 047 / Chart 0.9.50 / Helm 133。平台操作员、加密凭据、原操作状态机、连接测试、模型选择/发布、失败更新保留旧版、有效凭据更新和停用均通过；其他协议、预算和生产 TLS 单列。

## 配置与权限

平台供应商由明确的 operator user ID 白名单管理，不使用组织 Owner/Admin 身份推导平台权限；应用 API Key 不允许管理供应商。后台在外部 I/O 与提交前重新检查原 actor 和操作归属。组织 BYOK 是独立扩展，不会自动获得平台配置或密钥权限。

供应商类型先支持现有 OpenAI-compatible 文本 Chat Completions。发现 `/v1/models` 只代表列出了模型 ID，不自动确认工具、多模态、价格或推理兼容。管理员明确选择公开 alias、上游 ID 和有界输出上限后发布，付费推理测试要求单独操作。其他协议由 I02 实现。

## 凭据

API 使用公开密钥封装 API Key，Worker/独立 Gateway 使用保留版本的私有 keyring。使用已有锁定的 `golang.org/x/crypto/nacl/box` anonymous sealed boxes；提供商 ID、不可变修订 ID、规范目标地址摘要和 recipient key version 均包含在受保护载荷中。数据库保存密文，不保存可读 API Key；读取接口只返回凭据是否设置及修订状态，不回显部分原值。

API 录入与 operator 预置 Secret 引用是两条明确路径；引用只接受 operator 配置中的固定来源 ID，不接受浏览器提供文件路径或任意 Secret 名称。更换 endpoint 不隐式把旧供应商密钥转交到新地址。轮换生成新修订，验证与发布前保留原活动版本；失败不替换原配置。

生产密钥录入必须通过可信 HTTPS，不能相信客户端自报的 X-Forwarded-Proto。现有 HTTP 预览只有独立、默认关闭的 operator 实验开关才允许人工测试密钥；真实验收完成后恢复默认。封装保护数据库行与备份，不替代 Kubernetes Secret 静态加密、受限 RBAC 或防御已被控制的集群管理员；当前 API 的 namespace Secret 权限也不能被宣称为私钥的硬隔离证明。

封装接口和行为依据 [Go NaCl box 官方文档](https://pkg.go.dev/golang.org/x/crypto/nacl/box)，使用仓库已锁定 v0.56.0，不因文档最新版本而升级依赖。

## 网络与目录

连接地址固定为规范 `/v1` 根，无 userinfo、query、fragment、路径编码或重定向。私网只允许 operator 已审批的精确 origin/端口，不能把审批一个 IP 当成访问该节点所有服务的授权；云 metadata/link-local 等特殊目标始终拒绝。公开 HTTPS 地址解析和实际 Dial 均验证固定 IP 集，DNS 重绑定不能转向内网。内部连接不走环境代理。动态公网 HTTPS 可配置唯一受信 LAN CONNECT 代理；CONNECT 发送已批准 IP，原 TLS hostname 校验保留，代理不能自行解析目标域名。

创建与编辑保留原幂等请求/修订，测试不会直接发布。发现结果、选中模型、原凭据版本和测试身份组成不可变发布快照；发布与禁用推进目录代次。请求按实际目录代次准入和记账，途中变更不会把一个未知推理结果自动重放到其他上游。旧静态 operator routes 保留显式选择兼容，不自动切换或回退。

模型同步和控制台功能不得依赖外部付费凭据：本地验收复用已接受的 Ollama/私有 relay 与固定模型缓存，已完成新增供应商、模型发现、发布、三次实际非流式文本请求、失败更新保留旧版和禁用；既有静态 Chat/SSE 沿用之前回执。外部商业账户最终连通、价格、预算及其他协议仍按对应独立输入和需求验收。

## 原操作与发布接口

`GET /api/v1/platform/ai-providers/capabilities` 返回实际平台权限和已批准凭据来源 ID；其余平台接口要求显式 operator Console 会话。列表/详情、新建、`/{provider}/revisions` 保存并测试、`/publish`、`/disable`、删除定义，以及原 `ai-provider-operations/{id}` 查询/重试均见 [OpenAPI 操作索引](API.md)。所有写入要求 CSRF、8–128 字符原幂等键；更改已有定义还要求确切 version。发布必须引用当前 target 的 tested 修订，1–16 个明确别名，整个目录最多 100 个别名；定义最多 16 个。

原 actor 仍有效且当前修订未被替代时，瞬时 metadata 失败可显式重试原操作，最多五次。Worker 只重复有界模型 GET；不会因 metadata 测试发起付费推理。创建新修订不会替换活动 publication。发布/停用使用独占目录准入锁；Gateway 用共享锁核对代次并登记请求，不需要 catalog UPDATE 权限。锁在准入提交后释放，已准入的推理不阻塞停用。

keygen 为 `api/cmd/ai-provider-keygen`：在 Linux 用 `--output /absolute/new/protected-directory --version provider-v1` 生成独占目录，公钥与私钥分文件，私钥 0600；不会把私钥输出到终端或覆盖旧尝试。Chart 的 `aiProviderManagement` 默认关闭，仅引用运营方预先批准的 immutable public-policy/public-key 和 private-key Secret。API 只读 public 投影；Worker/Gateway 读 private 投影。公开 origin 未通过 DNS/IP/TLS 校验时，不执行请求。

当前 HTTP 实验 Console 的新密钥录入默认关闭；可选择运营方批准且绑定精确目标的既有凭据来源。`lab-ollama` 对应已经恢复的内网缓存模型服务，受控来源 UID 不接受替换。新建供应商后模型发现不会自动发布。已发布模型代表经过审批的路由配置，不代表模型质量、所有协议能力或长期健康保证。
