# 动态 AI 供应商基础交付 · 2026-10-20

I01 本地基础业务闭环通过。API/Worker/Web/独立 AI Gateway 0.58.0，migration 047，Chart 0.9.50，Helm 133；其他业务缺口继续处理，性能、压力、HA/DR 与长期可靠性专项后置。此回执不代表整个项目或 Neon 官网功能全部完成。

## 实现与边界

明确的平台 operator ID 白名单管理供应商，组织管理员与应用 API Key 不获得此权限。控制台支持新增/修改、批准凭据来源或新密钥、模型发现、选中别名发布、凭据更新、停用与删除定义。默认无动态供应商；空目录不伪造可调用模型。

原请求键绑定 actor、方法、路径与 HMAC 请求摘要。草稿、测试操作、不可变模型发现、发布记录分别持久保存；更新失败不覆盖活动版本。发布只接受当前目标的已测试修订，别名跨静态/动态目录去重。显式重试保留原操作和已封装凭据；更换地址不能隐式重用旧密钥。停用阻止后续准入，已准入的推理可以完成，不自动重放或切换上游。

密钥使用锁定的 NaCl anonymous sealed box，绑定供应商/修订/规范 endpoint 摘要及 key version。API 只挂载公钥；私钥投影只在 Worker/Gateway。现有 API 的 namespace Secret RBAC 权限仍需单独约束，不能把挂载差异宣称为抵御已攻陷 API/集群管理员的硬隔离。详情、能力接口和操作回执不返回密钥、密文或部分原值。生产录入要求实际 TLS 与 Secure cookie，不信任自报 X-Forwarded-Proto；验收专用内网 HTTP 录入开关已恢复 false。

新动态供应商的公网 HTTPS 使用显式批准的 LAN CONNECT 代理，连接目标为已校验 IP，外层 TLS 继续验证原 hostname；内网接口直连。metadata/link-local/特殊网络、混合 DNS、越过批准端口、重定向、无效 TLS 及过大模型元数据均拒绝。模型发现仅 GET /v1/models，不执行推理；只验收 OpenAI-compatible 文本 Chat Completions，其他协议/预算归 I02。

## 定向验证

首次 Linux 基础检查 13 个顶层测试通过，覆盖封装/作用域/keyring 保留与移除、网络策略、元数据、relay、keygen、设置和 OpenAPI。显式公网代理/内网直连与实际 CONNECT/TLS 专项通过；最终一次性 PostgreSQL 5 个顶层测试通过，其中 4 个为新供应商业务/权限测试，1 个为受影响的原 Gateway 检查。受限角色能读取活动视图、登记请求，但不能 UPDATE catalog 或读取未发布密钥。

实际 Console 在保留项目 `prj_6f3c51bd15b772e3`、分支 `br_460df22bd7a05881` 完成五项：

1. 丢弃第一次 202 响应后，以原键得到原供应商/修订/操作；恢复内网通道后，原 metadata 操作显式重试成功。
2. 从五个已有缓存模型中选择 qwen2.5:0.5b，发布公开别名并真实调用。
3. 错误密钥产生不可重试的 authentication_rejected，原活动修订仍可真实调用。
4. 新密文凭据修订测试并发布后，真实调用与用量登记通过。
5. 停用后别名退出目录；删除定义保留三修订、三原操作、两发布历史；旧 local-small/local-sql 静态入口保留。

供应商 `aip_9dd929b64c1b562d9ec3843f`，原操作 `apo_62c3789203fb2247ad1ed8f2` 共两次尝试。有效修订 `apv_c39766ee37d57edc684a7ca1`，失败修订 `apv_938a654910c06b9d26ae284f`。三次实际推理为 `air_34bcd4e4ef2cfff4f9c15741`、`air_90529064a9c4fa04f253e0ff`、`air_3e5a2f0f089f8fb419a8ed9e`，目录代次 4/4/5，均 completed 且有实际 Token 用量。最终 catalog revision 3、动态活动供应商 0、定义 version 7/deleted；八条原幂等回执保留。密钥/密文仅在 Linux 内存内核对，不导出实际密钥。

## 来源与数据保全

构建源码 SHA256 `7baf703768cc016c039d2e09a3452d830c734caaab70f09499d53903cd2c2279`，归档 SHA256 `f1187b47f34e9607fbf3412a9a22a2cec5b0337a9bd93e4666175d07761080b6`。父 Git HEAD 仍为 `928eec95c034af8b8bd89fc1a88761d4e67ece07`，不能当成本次运行源码提交；当前是本地不可变源码快照，未推送镜像或统一提交。

| 组件 | 本地镜像 |
| --- | --- |
| API / Worker | `neon.local/control-api@sha256:01d39285643dcfb77c062d8a1909bdfa38cc75159f6eab87317af28e5dbc0110` |
| Web | `neon.local/control-web@sha256:f140f682c0077cad0406db95e7e92ed4be7ce93eba31db0ea7dd553c437e8cf1` |
| AI Gateway | `neon.local/control-ai@sha256:d626f14a4eb426279a36a938636023cd021efa945558ddfa77b6e740c8926fbc` |

migration 047 冻结 SHA256 `4468f33b4f75664ee42f5498096cd852b7b13cf55dd4bf7576f7c03b0941a528`，没有修改已应用的 045/046。Linux 构建一次，复用现有 builder/模块/编译缓存，OCI 通过内网分发并在三节点校验。无模型下载；Functions Guest 沿用 0.57 已验收镜像，326 个实际生产输入与运行构建快照一致，23 个 Guest 生产输入与前版一致。

Helm 131 先升级镜像且新功能关闭；132 统一启用 API/Worker/Gateway；133 恢复稳定 policy。每次核对 24 个对象，9 个 PVC、865 个已有 Secret UID 和其他 6 个发布保持一致。原 Functions common config/密钥保留；Gateway 无 service-account token，新增 catalog 权限只有两个 SELECT。最终三节点、三个 Safekeeper 就绪，VM 0，原 Function 定义仍保留。六个本轮终态 Job 按 UID/resourceVersion 归档退休，不删除业务数据或 Secret。

保留失败与续测：首次 AI Dockerfile 仍走旧模块下载路径，超时后修为共享缓存再成功构建；ui-a 只因含图标的导航名称选择器失败，未创建供应商；ui-c 证实原 Windows Ollama SSH 隧道断开导致 metadata 502，恢复同一内网路径并重试原操作，未重建供应商。恢复稳定 policy 的本地脚本在实际升级前变量冲突，复用原备份和精确方案完成 Helm 133。最后的测试选择器/原操作续测为 test-only override，quality-c 再做格式/TypeScript 检查，运行代码未变化。

证据位于私有 `production/evidence/20261019-ai-providers`：`accepted-functional-slice.json`、`runtime-source-match.json`、`ui-d/result.json`、`go-20261019131343/result.json`、`quality-b/result.json`、`quality-c/result.json`、`images-b/merged-result.json`、`deploy-restore-b/result.json`、`final-readonly-proof-a/result.json` 及两份精确 Job 退休回执。

## 继续范围

官网功能账本现有 13 组基础回执，其余 22 组仍有实现/验收缺口。商业账户、价格与供应商正式能力没有用本地缓存模型代替验收；I02、完整计费、全链路 TLS、统一远程发行等仍按账本推进。Windows Ollama 通道当前手动恢复，未新增未经授权的登录自启动或长期可靠性承诺。
