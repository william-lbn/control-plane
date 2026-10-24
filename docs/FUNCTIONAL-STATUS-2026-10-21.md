# 当前功能验收与剩余清单（更新至 2026-10-25）

**还未到全功能收尾。** 35 个审计需求组中，18 组有基础验收回执；剔除五组后置专项后，仍有 12 组没有整体闭环，其中 9 组业务、运维/TLS/发行各一组。这是需求组数量，不是独立按钮或测试用例数，不能据此计算完成百分比。部分组仍缺代码，不能表述为只剩测试。

用户最新优先级：先完成所有业务功能；性能、压力、可靠性、分区、HA/DR、外部栅栏与长期环境认证转到其他环境。已有功能在来源/配置/依赖未变时复用原回执，只验证新增及受影响路径。权限、身份、数据保留和幂等是功能验收的一部分，继续执行。

## 已有基础验收的 18 组

| ID | 已通过基础功能 | 对应记录 |
|---|---|---|
| F01 | Functions WebSocket | [验收](DELIVERY-2026-10-18-FUNCTION-WEBSOCKETS.md) |
| F02 | Functions 定时触发 | [验收](DELIVERY-2026-10-17-FUNCTION-SCHEDULES.md) |
| F03 | 对象事件触发 Functions | [验收](DELIVERY-2026-10-18-FUNCTION-OBJECT-EVENTS.md) |
| F04 | Functions 客户日志 | [验收](DELIVERY-2026-10-18-FUNCTION-OUTPUT.md) |
| F05 | Functions 分支复制/恢复 | [验收](DELIVERY-2026-10-19-FUNCTION-BRANCHES.md) |
| S01 | 有界 S3 multipart | [验收](DELIVERY-2026-10-16-CURRENT.md) |
| A01 | Console 自助 MFA | [验收](DELIVERY-2026-10-16-CURRENT.md) |
| C01 | 角色/数据库 rename | [验收](DELIVERY-2026-10-17-CATALOG-RENAME.md) |
| L01 | 异常回收、失败创建清理、分支 TTL | [验收](DELIVERY-2026-10-18-BRANCH-EXPIRATION.md) |
| Q01 | 个人 SQL 历史策略/删除 | [验收](DELIVERY-2026-10-16-CURRENT.md) |
| Q02 | 项目共享 SQL 历史 | [验收](DELIVERY-2026-10-16-CURRENT.md) |
| Q03 | SQL AI、计划、修错/命名 | [验收](DELIVERY-2026-10-16-CURRENT.md) |
| I01 | 动态 AI 供应商配置 | [验收](DELIVERY-2026-10-20-AI-PROVIDERS.md) |
| A04 | Auth 分支 TOTP 迁移、会话清除、恢复码隔离及验证器恢复 | [验收](DELIVERY-2026-10-22-AUTH-FACTOR-MIGRATION.md) |
| A05 | Auth 保留删除/恢复、会话与密钥代次一致性 | [验收](DELIVERY-2026-10-25-AUTH-LIFECYCLE.md) |
| A03 | 应用 OAuth、手机、签名事件及原 Admin/组织等固定基线 | [验收](DELIVERY-2026-10-24-AUTH-INTEGRATIONS.md) |
| A02 | 固定本地 OIDC、账号绑定、组织策略与本地 MFA | [验收与范围](DELIVERY-2026-10-24-CONSOLE-SSO.md) |
| S04 | 固定 S3 操作矩阵、POST/trailers、公开读阻断及双 SDK | [矩阵验收](DELIVERY-2026-10-23-S3-CAPABILITIES.md) |

“基础通过”不代表官网全部扩展能力或生产认证完成。项目/分支/Compute、SQL、Auth、存储等更早的基础路径另见对应历史交付。本表不重复累加同一夹具续测，亦不把 88 套可选测试入口当作已通过功能数量。

## 当前尚未整体完成的 9 个业务组

| ID | 业务组 | 实际缺口 |
|---|---|---|
| C02 | 目录/系统身份 | 默认/系统漂移、Backend 名称依赖；普通身份、继承、当前展示及有原证明的旧身份衔接已通过 |
| S02 | 大对象/checksum | 64 MiB 流式、Range/基础 checksum 与 UI 已通过；5 GiB、完整扩展 checksum 等仍缺 |
| S03 | 引用安全 GC | 分支/历史引用保护下的清理功能；只使用新建专用夹具验证，不删除历史业务字节 |
| S05 | Storage gateway/用量 | 独立入口、授权/配额与可信用量；吞吐和容量压测后置 |
| L02 | 原分支 reset/PITR | 备份、原地切换、失败回退和 UI |
| L03 | Time Travel/Backend 一致恢复 | 查询时间点辅助与 Auth/对象/函数恢复语义 |
| F06 | Functions 用量/独立 edge | 调用/资源计量、独立入口和版本切换；性能/HA 后置 |
| I02 | AI 多协议/预算 | 原生协议/流式、累计预算/取消/对账及 Anthropic/Gemini 发现/发布已验收；组织手工额度已验收；组织 BYOK、有界多模态及分支自有文件已验收；供应商侧持久会话/响应引用、外部账单/支付等仍缺 |
| O03 | 运营集成 | 自托管计划/配额、计费接口与 CLI/MCP；外部支付输入单列 |

这 9 组既含未实现，也含已有部分实现但缺完整功能验收；依赖链按完整业务组连续处理。为优先推进日常业务，S02 有界大对象、A04 和 I02 协议/预算增量已验收；C02 默认/系统漂移与 Backend 名称依赖继续保留，随后推进 S3、Auth、生命周期恢复和计量/预算；不为已通过的原路径反复新建夹具。

## 另外三组与后置专项

| 分类 | ID | 处理 |
|---|---|---|
| 运维功能 | O01 | 告警、审计导出、保留与预算功能继续；长期 SLO/容量试运行后置 |
| TLS/安全 | T01 | 协议/证书功能缺口单列；已有 Auth→Proxy、Function 管理证书续期回执复用，完整环境认证不冒充已过 |
| 统一发行 | O02 | 当前提交代码和验收文档；签名/SBOM/provenance 消费、独立安装及所有组件统一发行另有门槛 |
| 后置专项 | R01/R02/X01/H01/H02 | 资源公平性/内存压力、跨实例栅栏、HA、全栈 DR 在其他环境做，不阻塞本地业务功能闭环 |

## 本轮已完成的目录功能

0.68：新分支首次启动保留客户 OID、密码与数据，继承角色 rename/改密/Owner、Reader 冷醒、无 Compute 分支原凭据代际及替换 Writer，累计六项真实检查通过。

0.69：持续身份与首次观察历史分开展示，当前 OID/Owner/代际/来源及排除可读，已知身份重复首次记录被拒绝；两项新 Go、五项受影响 Go 及五项累计真实检查通过。16 张目录表前后行数/摘要一致，三节点和三个 Safekeeper Ready，最终 VM 0。

0.70：有原证明的旧身份显式衔接、继承旧凭据背书及控制台入口通过；改名/改密、原回执重放、客户数据和 Writer/Reader 冷目录保留通过。四项新增及四项受影响 Go、五项实机功能检查通过，目标分支之外 16 张目录表范围摘要一致。

单对象 64 MiB、分片 8 MiB、分支可见对象 256 MiB；私有文件缓冲、浏览器分块哈希、17 MiB 真实 UI/SDK 上传和分片合并、Range/HEAD/SHA-256/CRC32C 拒绝路径通过。21 项 Go、3 项页面哈希检查通过，原目录摘要保留。API/Worker 0.71.0，Web 重建，schema 058 不变 / Chart 0.9.68 / Core Helm 152；Gateway 0.66.3 / Helm 56、AI 0.58.0 保留，最终 VM 0。S02 的 5 GiB 和完整扩展 checksum 仍有缺口，性能/可靠性专项后置。 见 [2026-10-22 有界大对象、Range 与 checksum](DELIVERY-2026-10-22-STORAGE-LARGE-OBJECTS.md)。

公开仓库只包含源码、测试、合同和脱敏验收摘要。密码、Kubernetes Secrets、原始私有快照/日志、虚拟机备份留在保护目录。当前部署仍由不可变源码/镜像摘要识别，提交代码不等于所有本地镜像已经公开发行。

2026-10-22：新增大对象验收详见 [DELIVERY-2026-10-22-STORAGE-LARGE-OBJECTS.md](DELIVERY-2026-10-22-STORAGE-LARGE-OBJECTS.md)，不是整个 S02 完成证明。

2026-10-22 Auth 增量：显式物理分支 TOTP 迁移、独立密钥重封装、复制会话/JWKS/链接清除、复制 SQL 登录禁用、未完成绑定清除、父子恢复码隔离及验证器恢复通过。4 项新增 Go、3 项受影响 Go、固定 Better Auth 的真实 PostgreSQL 互通、累计 11 项实机检查通过。API/Worker 0.72.0，Web 重建，schema 059 / Chart 0.9.69 / Core Helm 153；Auth 运行镜像、Gateway 0.66.3 / Helm 56、AI 0.58.0 保留。最终三节点、三个 Safekeeper Ready，VM 0。A04 基础闭环完成，其余 13 个业务组继续；性能/可靠性专项后置。 见 [2026-10-22 Auth 分支双重认证迁移](DELIVERY-2026-10-22-AUTH-FACTOR-MIGRATION.md)。

2026-10-22 AI 增量：[2026-10-22 AI 原生协议与累计预算](DELIVERY-2026-10-22-AI-NATIVE-BUDGETS.md)：Responses、Embeddings、Messages、Gemini、原生 SSE、Chat 工具/内联图片、不可变价格、预算预留/结算、取消与证据对账通过。15 项新增 Go/PG、4 项受影响 Go、4 项前端检查及 9 组实机检查通过；原有真实 Ollama 回执复用。本次多协议实机上游明确为合成协议服务，未冒充商业模型验收。API/Worker/AI 0.73.0、Web 重建，schema 060 / Chart 0.9.70 / Core Helm 155。最终三节点、三个 Safekeeper Ready，VM 0；17 张目录表和原数据身份保留。完整 I02 仍开放，其余业务缺口继续，性能/可靠性专项后置。

2026-10-23 原生供应商增量：[2026-10-23 原生供应商发现与发布](DELIVERY-2026-10-23-AI-NATIVE-PROVIDERS.md)：Anthropic/Gemini 原生目录、分页、不可变类型、匹配协议发布、Console 调用及跨类型凭据隔离通过。5 组新增 Go/PG、1 组受影响检查、累计 8 个实机检查点通过；原协议/预算与真实 Ollama 回执复用。API/Worker/AI 0.74.0、Web 重建，schema 061 / Chart 0.9.71 / Core Helm 158；Auth 和 Gateway 保留。17 张目录表、9 PVC、916 个原 Secret 身份保留，最终三节点、三个 Safekeeper Ready，VM 0。完整 I02 及其余业务组继续，性能/可靠性专项后置。

2026-10-23 组织额度增量：[2026-10-23 组织 AI 额度](DELIVERY-2026-10-23-AI-ORGANIZATION-CREDITS.md)：手工授信/更正、原键恢复、双账本原子准入/结算、未知用量保留及凭证对账通过。9 组新增 Go/PG、3 组受影响检查、8 个实际页面检查通过，原功能回执复用。API/Worker/AI 0.75.0、Web 重建，schema 062 / Chart 0.9.72 / Core Helm 160；Auth 和 Gateway 保留。17 张目录表、9 PVC、916 个原 Secret 身份保留，最终三节点、三个 Safekeeper Ready，VM 0。合成授信未接入真实支付；BYOK、完整多模态及其余业务继续，性能/可靠性专项后置。

2026-10-23 BYOK 增量：[2026-10-23 组织自有 AI 供应商](DELIVERY-2026-10-23-AI-ORGANIZATION-BYOK.md)：组织密钥/模型隔离、发现发布、原凭据轮换和费用归属通过，BYOK 不扣平台额度并继续受分支预算约束。5 组新增 Go/PG、3 组受影响检查、累计 7 个实际页面检查通过；已完成步骤从原检查点续验。API/Worker/AI 0.76.0、Web 重建，schema 063 / Chart 0.9.73 / Core Helm 162。17 张目录表、9 PVC、916 个原 Secret 身份保留，最终三节点和三个 Safekeeper Ready，VM 0。完整多模态/持久引用、外部账单/支付及其他业务继续，性能/可靠性专项后置。

2026-10-23 文件输入增量：[2026-10-23 分支 AI 文件与多模态输入](DELIVERY-2026-10-23-AI-INPUT-FILES.md)：加密文件、原键恢复、确切分支隔离、四种原生协议引用、删除/过期和计费引用历史通过。9 组新增 PostgreSQL、4 组受影响检查、3 组新增媒体和 8 个实际页面检查通过；四次合成调用，旧功能回执复用。API/Worker/AI 0.77.0、Web 重建，schema 064 / Chart 0.9.74 / Core Helm 164。17 张目录表、9 PVC、917 个此前 Secret 身份保留，最终三节点和三个 Safekeeper Ready，VM 0。供应商侧持久会话/响应引用、外部账单/支付及其他业务继续，性能/可靠性专项后置。

2026-10-23 S3 增量：[2026-10-23 S3 设置、POST 与校验尾部](DELIVERY-2026-10-23-S3-SETTINGS.md)：桶/对象标签、响应头、独立 COPY 指令、multipart 元数据、匿名读与公开阻断、签名 POST 和校验尾部通过。16 组新增 Go、2 组受影响检查、19 个累计实机检查点通过；已通过步骤从原检查点续验。API/Worker 0.78.0、Web 重建，schema 064 / 原生存储格式 3 / Chart 0.9.75 / Core Helm 166，AI/Auth/Gateway 保留。17 张目录表、6 类旧存储清单摘要、9 PVC 和 917 个原 Secret 身份保留，最终三节点、三个 Safekeeper Ready，VM 0。版本、生命周期、Object Lock 配置仅保存，不执行对应策略；ObjectParts 和完整 SDK 矩阵继续，性能/可靠性后置。

2026-10-23 对象属性增量：[2026-10-23 S3 完整属性与分片快照](DELIVERY-2026-10-23-S3-ATTRIBUTES.md)：完整类型化 SDK 属性、非连续分片分页/校验值、COPY 字节及覆盖后清除旧分片布局通过，UTC 日期边界已修正。3 组新增 Go、3 组受影响检查和 10 个实机检查点通过，旧标签/POST/尾部回执复用。API/Worker 0.79.0、原 Web 复用，schema 064 / 原生格式 4 / Chart 0.9.76 / Core Helm 167；17 张目录表、6 类旧存储清单、9 PVC、917 个原 Secret 和其他六套 Helm 配置保留。最终三节点、三个 Safekeeper Ready，VM 0。历史对象分片布局未推断或回填；完整兼容矩阵及其他业务继续，性能/可靠性后置。

2026-10-23 S3 矩阵收尾：[2026-10-23 S3 兼容矩阵与双 SDK](DELIVERY-2026-10-23-S3-CAPABILITIES.md)：固定需求矩阵 41 项操作逐项关联既有/新增回执；27 个授权/加密写入负例、18 个未支持子资源及 Boto3 互通通过，元数据响应键大小写已修复。3 组新增 Go、3 组受影响检查和 15 个累计实机检查点通过，原上传未重做。API/Worker 0.80.0，Web/AI/Auth/Gateway 复用，schema 064 / 原生格式 4 / Chart 0.9.77 / Core Helm 169。17 张目录表、6 类旧存储清单、9 PVC、917 个原 Secret 和其他六套 Helm 配置保留，最终三节点、三个 Safekeeper Ready，VM 0。S04 固定矩阵基线闭环，当前 15 组基础有回执，12 个业务和 3 个其他组继续；5 GiB/完整 checksum、安全 GC、独立网关及性能/可靠性专项分别保留。

2026-10-24 SSO：[Console OIDC 与组织策略](DELIVERY-2026-10-24-CONSOLE-SSO.md)完成固定本地基线；4 组新增 Go、4 组受影响检查、2 组 Helm 和 10 个累计浏览器检查点通过。原键恢复、显式绑定、强制策略、本地 MFA、恢复码、会话失效及 UI 已验收。Core Helm 171 / schema 065；API/Worker 0.81.0，Web 修复镜像，其他组件复用。17 张目录表、9 PVC、917 个原 Secret 保留，三节点、三个 Safekeeper Ready，VM 0。当前 16 组基础有回执，11 个业务和 3 个其他组继续；跨 authority IdP、商业接入和性能/可靠性认证单列。

2026-10-24 应用 Admin：[管理员功能与实际页面](DELIVERY-2026-10-24-AUTH-ADMIN.md)通过 2 组新增/7 组受影响 Go、1 组新增/5 组受影响维护库 PostgreSQL 检查，以及 11 个累计实机检查点。显式授权、角色、封禁、会话、改密/删除及一小时身份代入闭环；原已过步骤及改密未重做。Core Helm 172 / schema 065 / Auth 协议 0.26.0；原目录和凭据身份保留，三节点、三个 Safekeeper Ready，VM 0。A03 的 OAuth、组织、手机号、事件继续；全项目仍有 11 个业务和 3 个其他组未整体闭环。

2026-10-24 应用组织：[组织、成员与邀请](DELIVERY-2026-10-24-AUTH-ORGANIZATIONS.md)完成 22 个接口和控制台操作；2 组新增/7 组受影响 Go、3 组新增维护库 PostgreSQL、1 组受影响 Admin 及 9 个累计实机检查点通过。配额和最后管理者原子保护、验证收件人、当前 JWT 及复制分支邀请隔离通过；复用原创建 Operation 完成续验。Core Helm 174 / schema 065 / 应用协议 0.27.0，原目录与凭据身份保留，最终三节点、三个 Safekeeper Ready，VM 0。A03 的 OAuth、手机号和事件继续，整体仍有 11 个业务和 3 个其他组未闭环。

2026-10-24 应用 Auth 高级功能：[OAuth、手机和签名事件](DELIVERY-2026-10-24-AUTH-INTEGRATIONS.md)完成固定本地 A03 基线。25 个唯一维护库综合组、2 组新增/6 组受影响 Go、2 组协议和十个累计浏览器检查点通过；已过检查从原进度续验。Core Helm 178 / schema 065 / 应用协议 0.28.0；原目录、PVC 和 Secret 身份保留，三节点、三个 Safekeeper Ready，VM 0。当前 17 组基础有回执，10 个业务和 3 个其他组继续；商业 OAuth/短信接入、可信 TLS 与后置专项未冒充已过。

2026-10-25 Auth 生命周期：[保留删除、恢复与身份一致性](DELIVERY-2026-10-25-AUTH-LIFECYCLE.md)通过两组新增 Go 和八个累计实机检查点；前四项与原 Cookie 保留后只续验余项。恢复后 Auth 保持关闭，显式新代次拒绝旧会话/JWKS 并保留原账号、密码和 SQL 数据。复用 0.84 镜像，Core Helm 178 / schema 065 不变；原目录、945 个续验前持久 Secret UID 和 9 PVC 保留，三节点、三个 Safekeeper Ready，VM 0。A05 保留生命周期基线完成；当前 18 组基础、9 个业务和 3 个其他组继续。
